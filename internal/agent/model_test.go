package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func plannedTools(calls []plannedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.tool)
	}
	return out
}

// TestEachIntentMapsToAReadableTrajectory. The planner exists so the platform's
// controls can be exercised deterministically; an expected trajectory a human
// can read is one the eval harness can score.
func TestEachIntentMapsToAReadableTrajectory(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{
			name:  "a margin call reads the exposure and the collateral before proposing and issuing",
			query: "cp-delta looks undermargined, issue a margin call",
			want:  []string{"get_exposure", "get_collateral_value", "propose_margin_call", "issue_margin_call"},
		},
		{
			name:  "a transfer checks the whitelist, drafts, then executes",
			query: "transfer 5,000,000 shares to cp-hotel on evm",
			want:  []string{"check_whitelist", "draft_transfer", "execute_transfer"},
		},
		{
			name:  "a redemption reads the position first",
			query: "redeem 1,000,000 shares for cp-alpha",
			want:  []string{"get_position", "redeem_shares"},
		},
		{
			name:  "a stress question runs the scenario",
			query: "run a rate shock scenario at 250 bps",
			want:  []string{"run_stress_scenario"},
		},
		{
			name:  "a concentration question checks limits",
			query: "are we over any concentration limit?",
			want:  []string{"get_concentration"},
		},
		{
			name:  "a reconciliation question lists breaks",
			query: "are there any reconciliation breaks?",
			want:  []string{"list_recon_breaks"},
		},
		{
			name:  "a point in time question uses the as-of tool",
			query: "what did cp-alpha hold as of 2026-09-03?",
			want:  []string{"query_asof"},
		},
		{
			name:  "a haircut question reads the collateral value",
			query: "what is the haircut adjusted collateral value for cp-golf?",
			want:  []string{"get_collateral_value"},
		},
		{
			name:  "a holdings question reads the position",
			query: "what is cp-bravo's position?",
			want:  []string{"get_position"},
		},
		{
			name:  "a supply question reconciles across networks",
			query: "does total supply match across both ledgers?",
			want:  []string{"get_supply_reconciliation"},
		},
		{
			name:  "an exposure question reads the exposure",
			query: "what is our exposure to cp-delta?",
			want:  []string{"get_exposure"},
		},
		{
			name:  "an unrecognised intent answers from the exposure summary rather than guessing at a state change",
			query: "tell me something interesting",
			want:  []string{"get_exposure"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := plannedTools(Planner{}.plan(tc.query))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("plan(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestAQuestionAboutANoteReadsTheNoteFirst is the path an indirect injection
// travels: the memo is fetched before anything else in the plan.
func TestAQuestionAboutANoteReadsTheNoteFirst(t *testing.T) {
	step, err := Planner{}.Next(context.Background(), State{
		Query: "Check the notes on CP-CHARLIE then transfer 1,000,000 shares to CP-ALPHA",
	})
	if err != nil {
		t.Fatal(err)
	}
	if step.Call == nil || step.Call.Name != "get_counterparty_notes" {
		t.Fatalf("first call = %+v, want get_counterparty_notes", step.Call)
	}
	if got := step.Call.Args["counterparty"]; got != "CP-CHARLIE" {
		t.Fatalf("counterparty = %v, want CP-CHARLIE", got)
	}
}

func TestEntityExtractionHonoursWhatADeskActuallyTypes(t *testing.T) {
	t.Run("counterparty", func(t *testing.T) {
		cases := map[string]string{
			"exposure to CP-DELTA":             "CP-DELTA",
			"exposure to cp-foxtrot please":    "CP-FOXTROT",
			"transfer to CP-HOTEL on evm":      "CP-HOTEL",
			"no counterparty in this sentence": "",
			"CP- is not an identifier":         "",
		}
		for q, want := range cases {
			if got := counterpartyOf(q); got != want {
				t.Errorf("counterpartyOf(%q) = %q, want %q", q, got, want)
			}
		}
	})

	t.Run("quantity", func(t *testing.T) {
		cases := map[string]float64{
			"transfer 5,000,000 shares": 5_000_000,
			"transfer 5_000_000 shares": 5_000_000,
			"redeem 20m shares":         20_000_000,
			"redeem 20mm shares":        20_000_000,
			"redeem 3 million shares":   3_000_000,
			"move 750k shares":          750_000,
			"move 1.5bn shares":         1_500_000_000,
			"move 2 billion shares":     2_000_000_000,
			"no quantity here at all":   0,
		}
		for q, want := range cases {
			if got := amountOf(q); got != want {
				t.Errorf("amountOf(%q) = %v, want %v", q, got, want)
			}
		}
	})

	t.Run("ledger", func(t *testing.T) {
		cases := map[string]string{
			"settle on canton":       "canton",
			"the daml contract":      "canton",
			"settle on evm":          "evm",
			"no ledger named at all": "evm",
		}
		for q, want := range cases {
			if got := ledgerOf(q); got != want {
				t.Errorf("ledgerOf(%q) = %q, want %q", q, got, want)
			}
		}
	})

	t.Run("basis points", func(t *testing.T) {
		cases := map[string]float64{
			"shock of 250 bps":          250,
			"shock of 75 basis points":  75,
			"a shock with no number":    0,
			"250 is not bps on its own": 0,
		}
		for q, want := range cases {
			if got := bpsOf(q); got != want {
				t.Errorf("bpsOf(%q) = %v, want %v", q, got, want)
			}
		}
	})

	t.Run("date", func(t *testing.T) {
		cases := map[string]string{
			"as of 2026-09-03":  "2026-09-03",
			"as of yesterday":   "",
			"as of 2026-09-3":   "",
			"on 2026-09-03 eod": "2026-09-03",
			// A full RFC3339 timestamp is not extracted: the pattern requires
			// a word boundary after the day, and "03T" has none. The as-of
			// tool then falls back to its own default rather than to a wrong
			// date, so the limitation is a narrowing, not an error.
			"as of 2026-09-03T16:00:00Z": "",
		}
		for q, want := range cases {
			if got := dateOf(q); got != want {
				t.Errorf("dateOf(%q) = %q, want %q", q, got, want)
			}
		}
	})

	t.Run("scenario", func(t *testing.T) {
		cases := map[string]string{
			"a redemption run":       "redemption_run",
			"widen the haircuts":     "haircut_widening",
			"a counterparty default": "counterparty_default",
			"a credit event":         "counterparty_default",
			"a rate shock":           "rate_shock",
			"an unnamed scenario":    "rate_shock",
		}
		for q, want := range cases {
			if got := scenarioOf(q); got != want {
				t.Errorf("scenarioOf(%q) = %q, want %q", q, got, want)
			}
		}
	})
}

// TestADenialIsTerminalRatherThanSomethingToRouteAround. Retrying a refusal, or
// reaching for another tool that does the same thing, is precisely the behaviour
// a governed runtime must not have.
func TestADenialIsTerminalRatherThanSomethingToRouteAround(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"a policy denial", "policy denied: destination is not in the identity registry whitelist"},
		{"a dispatch denial", "policy denied at dispatch: money movement requires a recorded human approval"},
		{"a guardrail block", "guardrail blocked this tool result: blocked on secret/aws_access_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, err := Planner{}.Next(context.Background(), State{
				Query: "transfer 5,000,000 shares to CP-HOTEL",
				Messages: []Message{
					{Role: RoleTool, ToolName: "execute_transfer", IsError: true, Content: tc.content},
				},
				Failures: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if step.Call != nil {
				t.Fatalf("a refusal must not produce another call, got %+v", step.Call)
			}
			if !strings.Contains(step.Final, "No state was changed") {
				t.Fatalf("the answer must say nothing changed: %q", step.Final)
			}
		})
	}
}

func TestATransientFailureIsRetriedOnceAndThenReported(t *testing.T) {
	const failure = "tool error: upstream temporarily unavailable (retryable)"
	base := State{
		Query: "what is our exposure to CP-DELTA?",
		Messages: []Message{
			{Role: RoleTool, ToolName: "get_exposure", ToolCallID: "call_1", IsError: true, Content: failure},
		},
	}

	first := base
	first.Failures = 1
	step, err := Planner{}.Next(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if step.Call == nil || step.Call.Name != "get_exposure" {
		t.Fatalf("the first failure should be retried once, got %+v", step)
	}

	second := base
	second.Failures = 2
	step, err = Planner{}.Next(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if step.Call != nil {
		t.Fatalf("a second failure must stop rather than loop, got %+v", step.Call)
	}
	if !strings.Contains(step.Final, "Nothing was changed") {
		t.Fatalf("answer = %q", step.Final)
	}
}

func TestANonRetryableFailureIsNotRetried(t *testing.T) {
	step, err := Planner{}.Next(context.Background(), State{
		Query: "what is our exposure to CP-DELTA?",
		Messages: []Message{
			{Role: RoleTool, ToolName: "get_exposure", IsError: true, Content: "tool error: unknown counterparty"},
		},
		Failures: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if step.Call != nil && step.Call.ID != "" && strings.HasPrefix(step.Call.ID, "retry_") {
		t.Fatalf("a non-retryable failure must not be retried, got %+v", step.Call)
	}
}

// TestALaterArgumentComesFromAnEarlierEnginesOutput is what keeps a figure on a
// legal demand grounded: the amount to issue is the shortfall the engine
// computed, not the model's recollection of it.
func TestALaterArgumentComesFromAnEarlierEnginesOutput(t *testing.T) {
	s := State{
		Query: "CP-DELTA looks undermargined, issue a margin call",
		Messages: []Message{
			{Role: RoleTool, ToolName: "get_exposure"},
			{Role: RoleTool, ToolName: "get_collateral_value"},
			{Role: RoleTool, ToolName: "propose_margin_call"},
		},
	}
	s.AddToolData(map[string]any{"shortfall": 35298750.0, "currency": "USD"})
	s.AddToolData(map[string]any{"proposal_id": "MC-CP-DELTA-0"})

	step, err := Planner{}.Next(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if step.Call == nil || step.Call.Name != "issue_margin_call" {
		t.Fatalf("call = %+v, want issue_margin_call", step.Call)
	}
	if got := step.Call.Args["amount"]; got != 35298750.0 {
		t.Fatalf("amount = %v, want the engine's figure", got)
	}
	if got := step.Call.Args["proposal_id"]; got != "MC-CP-DELTA-0" {
		t.Fatalf("proposal_id = %v, want the proposal the earlier step produced", got)
	}
	if got := step.Call.Args["currency"]; got != "USD" {
		t.Fatalf("currency = %v", got)
	}
}

func TestTheFinalAnswerIsAssembledOnlyFromSuccessfulToolOutput(t *testing.T) {
	s := State{
		Query: "what is our exposure to CP-DELTA?",
		Messages: []Message{
			{Role: RoleTool, ToolName: "get_exposure", Content: "Uncovered 35798750.00 USD"},
			{Role: RoleTool, ToolName: "get_exposure", IsError: true, Content: "tool error: transient"},
			{Role: RoleAssistant, Content: "thinking about it"},
		},
	}
	step, err := Planner{}.Next(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if step.Call != nil {
		t.Fatalf("the plan is complete; want a final answer, got %+v", step.Call)
	}
	if !strings.Contains(step.Final, "35798750.00") {
		t.Fatalf("answer = %q, want it to carry the engine's figure", step.Final)
	}
	if strings.Contains(step.Final, "tool error") {
		t.Fatalf("a failed call must not be narrated as a result: %q", step.Final)
	}
	if strings.Contains(step.Final, "thinking about it") {
		t.Fatalf("the model's own prose is not a grounded result: %q", step.Final)
	}
}

func TestAnEmptyPlanProducesARefusalRatherThanAnInvention(t *testing.T) {
	step, err := Planner{}.Next(context.Background(), State{
		Query:    "what is our exposure to CP-DELTA?",
		Messages: []Message{{Role: RoleTool, ToolName: "get_exposure", Content: "   "}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(step.Final, "could not retrieve") {
		t.Fatalf("answer = %q, want an admission that nothing was retrieved", step.Final)
	}
}

func TestStateReportsTheTrajectoryAndTheLastFailure(t *testing.T) {
	s := State{Messages: []Message{
		{Role: RoleUser, Content: "hello"},
		{Role: RoleTool, ToolName: "get_exposure"},
		{Role: RoleAssistant, Content: "reasoning"},
		{Role: RoleTool, ToolName: "propose_margin_call", IsError: true, Content: "policy denied: nope"},
	}}
	if got := s.CalledTools(); !reflect.DeepEqual(got, []string{"get_exposure", "propose_margin_call"}) {
		t.Fatalf("CalledTools = %v", got)
	}
	m, ok := s.LastToolError()
	if !ok || m.ToolName != "propose_margin_call" {
		t.Fatalf("LastToolError = %+v, %v", m, ok)
	}

	var empty State
	if _, ok := empty.LastToolError(); ok {
		t.Fatal("an empty conversation has no last tool error")
	}
	s.Messages = append(s.Messages, Message{Role: RoleAssistant, Content: "done"})
	if _, ok := s.LastToolError(); ok {
		t.Fatal("the last message is not a tool error any more")
	}
}

func TestToolDataMergesLaterResultsOverEarlierOnes(t *testing.T) {
	var s State
	s.AddToolData(map[string]any{"currency": "USD", "shortfall": 1.0})
	s.AddToolData(nil)
	s.AddToolData(map[string]any{"shortfall": 2.0})
	got := s.ToolData()
	if got["shortfall"] != 2.0 || got["currency"] != "USD" {
		t.Fatalf("ToolData = %v", got)
	}
}

func TestThePlannerNamesItselfSoARunRecordsWhichModelDecided(t *testing.T) {
	if got := (Planner{}).Name(); got != "planner/deterministic-v1" {
		t.Fatalf("model name = %q", got)
	}
}
