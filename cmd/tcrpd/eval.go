package main

import (
	"context"
	"fmt"
	"os"

	"github.com/udaykishore-resu/tcrp/internal/eval"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/platform"
)

// runEval executes the benchmark suite against the real runtime.
//
// Each scenario gets its own platform, so the suite measures the runtime rather
// than the order the scenarios happened to run in. The exit code is meaningful:
// a suite with a policy violation in it fails, because a policy violation is
// not a score, it is a defect.
func runEval(ctx context.Context, args []string) error {
	fs := flagsFor("eval")
	suitePath := fs.String("suite", "", "suite JSON to run instead of the built-in one")
	out := fs.String("out", "", "write the report as JSON to this path")
	markdown := fs.String("markdown", "", "write a Markdown summary to this path")
	concurrency := fs.Int("concurrency", 0, "scenarios in flight at once (0 = NumCPU)")
	category := fs.String("category", "", "run only scenarios in this category")
	strict := fs.Bool("strict", true, "exit non-zero when any scenario fails")
	if err := fs.Parse(args); err != nil {
		return err
	}

	suite, err := eval.BuiltinSuite()
	if err != nil {
		return err
	}
	if *suitePath != "" {
		f, err := os.Open(*suitePath)
		if err != nil {
			return err
		}
		suite, err = eval.LoadSuite(f)
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	if *category != "" {
		filtered := suite
		filtered.Scenarios = nil
		for _, sc := range suite.Scenarios {
			if string(sc.Category) == *category {
				filtered.Scenarios = append(filtered.Scenarios, sc)
			}
		}
		if len(filtered.Scenarios) == 0 {
			return fmt.Errorf("no scenarios in category %q", *category)
		}
		suite = filtered
	}

	runner := &eval.Runner{
		Harness:     platform.NewEvalHarness(platform.Options{Environment: "evaluation"}),
		Clock:       obs.RealClock{},
		Concurrency: *concurrency,
	}
	fmt.Fprintf(os.Stderr, "running %d scenarios from %s %s\n", len(suite.Scenarios), suite.Name, suite.Version)

	rep, err := runner.Run(ctx, suite)
	if err != nil {
		return err
	}
	fmt.Print(rep.TextSummary())

	if *out != "" {
		b, err := rep.JSON()
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "report written to %s\n", *out)
	}
	if *markdown != "" {
		if err := os.WriteFile(*markdown, []byte(rep.MarkdownSummary()), 0o644); err != nil {
			return err
		}
	}
	if *strict && rep.Failed > 0 {
		return fmt.Errorf("%d of %d scenarios failed", rep.Failed, len(rep.Scenarios))
	}
	return nil
}
