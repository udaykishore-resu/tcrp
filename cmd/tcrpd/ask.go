package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/platform"
)

// ask runs one governed turn from the terminal.
//
// It is the same runtime the server uses, with the same controls in the same
// order. The only thing the CLI adds is that it can grant the approval for you
// when you ask it to — and it still cannot grant it as the requester, because
// the four-eyes rule lives in the approval service and not in the caller.
func ask(ctx context.Context, args []string) error {
	fs := flagsFor("ask")
	as := fs.String("as", "agent", "principal: "+strings.Join(platform.PrincipalNames(), ", "))
	approveAs := fs.String("approve-as", "", "grant any approval this run opens, as this principal")
	env := fs.String("env", "development", "environment name; a policy input")
	jsonOut := fs.Bool("json", false, "print the run as JSON instead of prose")
	dir := fs.String("dir", "", "state directory (default: a temporary directory)")
	model := fs.String("model", "planner", "model: planner or anthropic")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New(`ask: give me a question, e.g. tcrpd ask "what is our exposure to CP-DELTA?"`)
	}
	who, ok := platform.PrincipalByName(*as)
	if !ok {
		return fmt.Errorf("unknown principal %q; expected one of %s", *as, strings.Join(platform.PrincipalNames(), ", "))
	}

	p, err := platform.New(ctx, platform.Options{
		Dir: *dir, Environment: *env, Model: *model, Deterministic: *dir == "",
	})
	if err != nil {
		return err
	}
	defer p.Close()

	run, err := p.Runtime.Start(ctx, agent.Request{Query: query, Principal: who})
	if err != nil {
		return err
	}

	if *approveAs != "" {
		approver, ok := platform.PrincipalByName(*approveAs)
		if !ok {
			return fmt.Errorf("unknown approver %q", *approveAs)
		}
		for range 4 {
			cur, err := p.Store.LoadRun(ctx, run.ID)
			if err != nil || cur.Status != agent.StatusAwaiting || cur.Pending == nil {
				break
			}
			if _, err := p.Approvals.Grant(ctx, cur.Pending.ApprovalID, approver, "granted via tcrpd ask"); err != nil {
				fmt.Fprintf(os.Stderr, "approval refused: %v\n", err)
				break
			}
		}
		run, _ = p.Store.LoadRun(ctx, run.ID)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(run)
	}

	fmt.Printf("run    %s\nas     %s\nstatus %s\n\n", run.ID, who, run.Status)
	for _, s := range run.Steps {
		switch {
		case s.Tool == "":
			fmt.Printf("  %-2d %-24s guard=%s %s\n", s.N, s.Outcome, s.GuardVerdict, s.GuardReason)
		case s.Outcome == agent.OutcomeDenied:
			fmt.Printf("  %-2d %-24s %-16s REFUSED %s: %s\n", s.N, s.Tool, s.Tier, s.PolicyRule, s.PolicyReason)
		case s.Outcome == agent.OutcomeAwaiting:
			fmt.Printf("  %-2d %-24s %-16s AWAITING APPROVAL %s\n", s.N, s.Tool, s.Tier, s.ApprovalID)
		default:
			fmt.Printf("  %-2d %-24s %-16s %s (%.2fms)\n", s.N, s.Tool, s.Tier, s.Outcome, s.DurationMS)
		}
	}
	if run.Status == agent.StatusAwaiting && run.Pending != nil {
		fmt.Printf("\nThis run is paused. Approve it with:\n  tcrpd ask --approve-as risk-manager ...\nor through the dashboard at /v1/approvals/%s/grant\n", run.Pending.ApprovalID)
	}
	if run.Answer != "" {
		fmt.Printf("\n%s\n", run.Answer)
	}
	return nil
}
