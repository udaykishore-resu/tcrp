package eval

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/identity"
)

func TestBuiltinSuiteLoads(t *testing.T) {
	s, err := BuiltinSuite()
	if err != nil {
		t.Fatalf("BuiltinSuite: %v", err)
	}
	if len(s.Scenarios) < 24 {
		t.Fatalf("built-in suite has %d scenarios, want at least 24", len(s.Scenarios))
	}
	counts := s.CountByCategory()
	for _, c := range Categories() {
		if counts[c] == 0 {
			t.Errorf("category %s has no scenarios", c)
		}
	}
	if _, ok := s.Scenario(DenyApprovalScenarioID); !ok {
		t.Fatalf("built-in suite has no scenario %q for the designated denial", DenyApprovalScenarioID)
	}
	t.Logf("%d scenarios across %d categories", len(s.Scenarios), len(counts))
	for _, c := range Categories() {
		t.Logf("  %-20s %d", c, counts[c])
	}
}

// TestBuiltinSuiteCoverage asserts the properties that make the suite worth
// running: safety categories must actually forbid something, pausing scenarios
// must expect a money-tier tool, and refusing scenarios must name what they
// forbid rather than only hoping for a polite answer.
func TestBuiltinSuiteCoverage(t *testing.T) {
	s := MustBuiltinSuite()
	moneyTools := map[string]bool{
		"execute_transfer": true, "issue_margin_call": true, "redeem_shares": true,
	}
	for _, sc := range s.Scenarios {
		switch sc.Category {
		case CatDirectInjection, CatIndirectInjection, CatApprovalBypass, CatOutOfScope, CatWhitelist:
			if len(sc.ForbiddenTools) == 0 {
				t.Errorf("scenario %s is category %s but forbids nothing", sc.ID, sc.Category)
			}
		case CatErrorRecovery:
			if sc.FailureBudget() == 0 {
				t.Errorf("scenario %s is an error-recovery case with no injected failures", sc.ID)
			}
		}
		if sc.MustPause {
			found := false
			for _, tool := range sc.ExpectedTools {
				if moneyTools[tool] {
					found = true
				}
			}
			if !found && sc.ID != DenyApprovalScenarioID {
				t.Errorf("scenario %s must pause but expects no money-movement tool", sc.ID)
			}
		}
		if sc.MustRefuse && len(sc.ForbiddenTools) == 0 && len(sc.ExpectAnswerOmits) == 0 {
			t.Errorf("scenario %s must refuse but names nothing it must not do", sc.ID)
		}
	}
}

func TestSuiteJSONRoundTrip(t *testing.T) {
	original := MustBuiltinSuite()

	var buf bytes.Buffer
	if err := original.Save(&buf); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := LoadSuite(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("LoadSuite: %v", err)
	}
	if !reflect.DeepEqual(original, reloaded) {
		t.Fatal("suite did not survive a JSON round trip")
	}

	// A second round trip must be byte-identical to the first, which is the
	// property that lets CI diff a suite file.
	var second bytes.Buffer
	if err := reloaded.Save(&second); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), second.Bytes()) {
		t.Fatal("serialising the suite twice produced different bytes")
	}

	path := filepath.Join(t.TempDir(), "suite.json")
	if err := original.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	fromFile, err := LoadSuiteFile(path)
	if err != nil {
		t.Fatalf("LoadSuiteFile: %v", err)
	}
	if !reflect.DeepEqual(original, fromFile) {
		t.Fatal("suite did not survive a file round trip")
	}
}

func TestSuiteValidation(t *testing.T) {
	good := Scenario{
		ID: "ok", Name: "ok", Query: "q", Category: CatRead,
		Principal: identity.Principal{Subject: "u", Tenant: "t"},
	}
	mutate := func(fn func(*Scenario)) Scenario {
		sc := good
		fn(&sc)
		return sc
	}

	tests := []struct {
		name  string
		suite Suite
		want  string
	}{
		{"no name", Suite{Version: "1", Scenarios: []Scenario{good}}, "no name"},
		{"no version", Suite{Name: "s", Scenarios: []Scenario{good}}, "no version"},
		{"no scenarios", Suite{Name: "s", Version: "1"}, "no scenarios"},
		{"duplicate ids", Suite{Name: "s", Version: "1", Scenarios: []Scenario{good, good}}, "duplicate scenario id"},
		{"no id", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) { sc.ID = "" })}}, "no id"},
		{"no query", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) { sc.Query = " " })}}, "no query"},
		{"unknown category", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) { sc.Category = "made_up" })}}, "unknown category"},
		{"no principal", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) { sc.Principal = identity.Principal{} })}}, "no principal subject"},
		{"negative weight", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) { sc.Weight = -1 })}}, "negative weight"},
		{"expects and forbids the same tool", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) {
			sc.ExpectedTools, sc.ForbiddenTools = []string{"t"}, []string{"t"}
		})}}, "both expects and forbids"},
		{"non-positive injected failure", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) {
			sc.InjectedFailures = map[string]int{"t": 0}
		})}}, "injects 0 failures"},
		{"refuse and pause", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) {
			sc.MustRefuse, sc.MustPause = true, true
		})}}, "cannot both refuse and pause"},
		{"designated denial does not pause", Suite{Name: "s", Version: "1", Scenarios: []Scenario{mutate(func(sc *Scenario) {
			sc.ID = DenyApprovalScenarioID
		})}}, "does not pause"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.suite.Validate()
			if err == nil {
				t.Fatalf("Validate accepted an invalid suite, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate: %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadSuiteRejectsUnknownFields(t *testing.T) {
	// An unknown field is almost always a renamed or misspelled expectation,
	// and silently ignoring it turns a scenario into one that asserts nothing.
	in := `{"name":"s","version":"1","scenarios":[{"id":"a","name":"a","query":"q",
	  "category":"read","principal":{"sub":"u"},"expcted_tools":["x"]}]}`
	if _, err := LoadSuite(strings.NewReader(in)); err == nil {
		t.Fatal("LoadSuite accepted an unknown field")
	}
}

func TestSuiteFilterAndSorted(t *testing.T) {
	s := MustBuiltinSuite()
	reads := s.Filter(func(sc Scenario) bool { return sc.Category == CatRead })
	if reads.Name != s.Name || reads.Version != s.Version {
		t.Fatal("Filter dropped the suite identity")
	}
	if len(reads.Scenarios) != s.CountByCategory()[CatRead] {
		t.Fatalf("Filter returned %d read scenarios, want %d", len(reads.Scenarios), s.CountByCategory()[CatRead])
	}
	sorted := s.Sorted()
	for i := 1; i < len(sorted.Scenarios); i++ {
		if sorted.Scenarios[i-1].ID > sorted.Scenarios[i].ID {
			t.Fatalf("Sorted is not ordered at index %d", i)
		}
	}
	if len(s.Scenarios) > 1 && &s.Scenarios[0] == &sorted.Scenarios[0] {
		t.Fatal("Sorted must not alias the original slice")
	}
}

func TestEffectiveWeight(t *testing.T) {
	if got := (Scenario{}).EffectiveWeight(); got != 1 {
		t.Fatalf("EffectiveWeight = %v, want 1 for an unset weight", got)
	}
	if got := (Scenario{Weight: 2.5}).EffectiveWeight(); got != 2.5 {
		t.Fatalf("EffectiveWeight = %v, want 2.5", got)
	}
}
