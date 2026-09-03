package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The platform's claim is that nothing on a decision path reads the wall clock
// directly: every engine takes an obs.Clock, which is what makes a trajectory
// replayable, a figure reproducible and a test deterministic. A claim like that
// is worth exactly as much as its enforcement, so this file enforces it over
// the source rather than restating it in a comment.
//
// clockExceptions names the packages allowed to read the wall clock, and why.
// It is a list of import-path suffixes relative to the repository root.
var clockExceptions = map[string]string{
	// obs defines the Clock abstraction. RealClock.Now is the one wall-clock
	// read the platform is built on; everything else takes a Clock from it.
	"internal/obs": "defines obs.RealClock, the single wall-clock read the rest of the platform is given",
}

// wallClockCalls are the standard-library functions that read the machine's
// clock. time.Since is included because it is time.Now by another name.
var wallClockCalls = map[string]bool{"Now": true, "Since": true}

type violation struct {
	pos  string
	call string
}

// TestNoEngineReadsTheWallClockDirectly walks the non-test source under
// internal/ and fails on any call to time.Now or time.Since outside the
// packages listed above.
func TestNoEngineReadsTheWallClockDirectly(t *testing.T) {
	root, err := filepath.Abs("../internal")
	if err != nil {
		t.Fatal(err)
	}

	var (
		found  []violation
		files  int
		fset   = token.NewFileSet()
		walked = map[string]bool{}
	)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(filepath.Dir(root), path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		walked[pkg] = true
		if _, exempt := clockExceptions[pkg]; exempt {
			return nil
		}
		files++

		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", rel, err)
		}
		// Resolve the local name the "time" package is bound to in this file,
		// so an alias or a dot-import cannot smuggle a wall-clock read past the
		// check. A file that does not import time cannot call into it.
		local, imported := timeImportName(file)
		if !imported {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !wallClockCalls[sel.Sel.Name] {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != local {
				return true
			}
			p := fset.Position(call.Pos())
			found = append(found, violation{
				pos:  fmt.Sprintf("%s:%d:%d", rel, p.Line, p.Column),
				call: local + "." + sel.Sel.Name + "()",
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if files == 0 {
		t.Fatal("the walk found no source to check; the architecture test is checking nothing")
	}
	// An exception that no longer names a real package is an exception nobody
	// is maintaining.
	for pkg, why := range clockExceptions {
		if !walked[pkg] {
			t.Errorf("clockExceptions names %q (%s), which does not exist", pkg, why)
		}
	}

	if len(found) > 0 {
		sort.Slice(found, func(i, j int) bool { return found[i].pos < found[j].pos })
		var b strings.Builder
		b.WriteString("the wall clock is read directly outside the packages allowed to:\n")
		for _, v := range found {
			fmt.Fprintf(&b, "  %s  %s\n", v.pos, v.call)
		}
		b.WriteString("\nevery engine takes an obs.Clock; add the dependency rather than the call.\n")
		b.WriteString("if a read is genuinely unavoidable, add its package to clockExceptions with a reason.\n")
		t.Fatal(b.String())
	}
	t.Logf("checked %d non-test files under internal/ across %d packages", files, len(walked))
}

// timeImportName returns the identifier the standard "time" package is bound to
// in this file. A dot-import is reported as a violation in its own right,
// because it makes the check — and the reader — unable to see where a call came
// from.
func timeImportName(file *ast.File) (string, bool) {
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"time"` {
			continue
		}
		if imp.Name == nil {
			return "time", true
		}
		switch imp.Name.Name {
		case "_":
			return "", false
		case ".":
			return ".", true // no identifier can match, so a dot-import is caught below
		default:
			return imp.Name.Name, true
		}
	}
	return "", false
}

// TestTheTimePackageIsNeverDotImportedUnderInternal keeps the check above
// honest: a dot-import would hide `Now()` behind a bare identifier.
func TestTheTimePackageIsNeverDotImportedUnderInternal(t *testing.T) {
	root, err := filepath.Abs("../internal")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		file, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range file.Imports {
			if imp.Name != nil && imp.Name.Name == "." {
				rel, _ := filepath.Rel(filepath.Dir(root), path)
				t.Errorf("%s dot-imports %s", filepath.ToSlash(rel), imp.Path.Value)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestTheGuardrailDoesNotDependOnTheAgentRuntime. The guardrail is deployable on
// its own — as a sidecar or a separate service — which is only true while it
// carries no import of the runtime it protects. That is why guard spells its
// tiers as strings rather than importing agent.Tier.
func TestTheGuardrailDoesNotDependOnTheAgentRuntime(t *testing.T) {
	forbidden := map[string][]string{
		"internal/guard":  {"internal/agent", "internal/tools", "internal/platform", "internal/policy"},
		"internal/policy": {"internal/agent", "internal/tools", "internal/platform", "internal/guard"},
		"internal/obs":    {"internal/agent", "internal/tools", "internal/platform", "internal/guard", "internal/policy"},
		"internal/audit":  {"internal/agent", "internal/tools", "internal/platform", "internal/guard"},
	}
	const module = "github.com/udaykishore-resu/tcrp/"

	for pkg, banned := range forbidden {
		t.Run(pkg, func(t *testing.T) {
			dir, err := filepath.Abs(filepath.Join("..", pkg))
			if err != nil {
				t.Fatal(err)
			}
			fset := token.NewFileSet()
			entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			for _, path := range entries {
				if strings.HasSuffix(path, "_test.go") {
					continue
				}
				checked++
				file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
				if err != nil {
					t.Fatal(err)
				}
				for _, imp := range file.Imports {
					got := strings.Trim(imp.Path.Value, `"`)
					for _, bad := range banned {
						if got == module+bad {
							t.Errorf("%s imports %s; the dependency must run the other way",
								filepath.Base(path), bad)
						}
					}
				}
			}
			if checked == 0 {
				t.Fatalf("no source found under %s", pkg)
			}
		})
	}
}
