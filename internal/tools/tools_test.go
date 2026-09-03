package tools

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/agent"
	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/guard"
	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
	"github.com/udaykishore-resu/tcrp/internal/risk"
)

// treasuryShares mirrors the fund's unissued inventory the platform mints to
// the operator; without it every outbound transfer fails at the ledger for a
// reason that has nothing to do with the control under test.
const treasuryShares int64 = 100_000_000

// newTestPlatform assembles the same object graph the platform package builds,
// pinned to the seed instant so every figure is reproducible.
func newTestPlatform(t *testing.T) *Platform {
	t.Helper()
	ctx := context.Background()
	seed := domain.NewSeed()
	clock := obs.NewManualClock(seed.AsOf)

	lk, err := lake.NewFromSeed(ctx, t.TempDir(), clock, seed)
	if err != nil {
		t.Fatal(err)
	}
	world, err := ledger.NewWorld(ledger.Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := world.EVM.Register(world.Operator, ledger.Identity{
		Address: world.Operator, Country: "US",
		Claims:       []string{ledger.ClaimKYC, ledger.ClaimAccredited},
		Counterparty: "TCRP-OPERATOR",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := world.EVM.Mint(ctx, world.Operator, world.Operator,
		domain.FundSymbol, treasuryShares, "treasury inventory"); err != nil {
		t.Fatal(err)
	}
	return NewPlatform(lk, risk.New(clock, nil, nil, nil), world, clock, audit.New(clock), nil)
}

func toolNamed(t *testing.T, p *Platform, name string) agent.Tool {
	t.Helper()
	for _, tool := range p.All() {
		if tool.Spec().Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q", name)
	return nil
}

func invoke(t *testing.T, p *Platform, name string, args map[string]any) (agent.ToolResult, error) {
	t.Helper()
	return toolNamed(t, p, name).Invoke(context.Background(), args)
}

func mustInvoke(t *testing.T, p *Platform, name string, args map[string]any) agent.ToolResult {
	t.Helper()
	res, err := invoke(t, p, name, args)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, args, err)
	}
	return res
}

// readToolArgs is a valid argument set per read tool, so the whole read tier can
// be exercised generically.
var readToolArgs = map[string]map[string]any{
	"get_exposure":              {"counterparty": "CP-DELTA"},
	"get_collateral_value":      {"counterparty": "CP-DELTA"},
	"get_position":              {"counterparty": "CP-ALPHA"},
	"get_concentration":         {},
	"query_asof":                {"counterparty": "CP-ALPHA", "as_of": "2026-09-03"},
	"list_recon_breaks":         {},
	"get_supply_reconciliation": {},
	"run_stress_scenario":       {"scenario": "rate_shock"},
	"check_whitelist":           {"counterparty": "CP-ALPHA", "ledger": "evm"},
	"get_haircut_schedule":      {},
	"get_counterparty_notes":    {"counterparty": "CP-BRAVO"},
}

// ---------------------------------------------------------------------------
// declarations
// ---------------------------------------------------------------------------

// TestEveryToolDeclaresWhatThePolicyGatewayAuthorizesAgainst. The declarations
// are data the gateway reads, not documentation, so a tool that declares the
// wrong thing is a control that authorises the wrong thing.
func TestEveryToolDeclaresWhatThePolicyGatewayAuthorizesAgainst(t *testing.T) {
	p := newTestPlatform(t)
	registry := agent.NewRegistry()
	if err := p.Register(registry); err != nil {
		t.Fatalf("the shipped tool set must satisfy the registry's contract: %v", err)
	}
	for _, tool := range p.All() {
		s := tool.Spec()
		t.Run(s.Name, func(t *testing.T) {
			if !s.Tier.Valid() {
				t.Fatalf("tier = %q", s.Tier)
			}
			if s.Description == "" {
				t.Fatal("a tool with no description cannot be chosen correctly by a model")
			}
			if s.Tier != agent.TierRead && len(s.RequiredScopes) == 0 {
				t.Fatal("a state-changing tool with no scope is a tool nobody can authorize")
			}
			if s.Tier == agent.TierMoney && s.Reversible {
				t.Fatal("money movement is not reversible")
			}
			if s.Tier == agent.TierRead && !s.Reversible {
				t.Fatal("a read tool that claims to be irreversible mis-declares itself")
			}
			if s.ResourceKind == "" {
				t.Fatal("the policy gateway reasons about resource kind rather than parsing arguments")
			}
		})
	}
}

func TestTheReadTierIsExactlyTheToolsThatChangeNothing(t *testing.T) {
	p := newTestPlatform(t)
	byTier := map[agent.Tier][]string{}
	for _, tool := range p.All() {
		s := tool.Spec()
		byTier[s.Tier] = append(byTier[s.Tier], s.Name)
	}
	if len(byTier[agent.TierRead]) != len(readToolArgs) {
		t.Fatalf("read tier = %v, but the test only knows how to call %d of them",
			byTier[agent.TierRead], len(readToolArgs))
	}
	for _, name := range byTier[agent.TierRead] {
		if _, ok := readToolArgs[name]; !ok {
			t.Errorf("read tool %q has no argument set in this test", name)
		}
	}
	for _, want := range []string{"issue_margin_call", "execute_transfer", "redeem_shares"} {
		found := false
		for _, got := range byTier[agent.TierMoney] {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must sit in the money-movement tier, tier holds %v", want, byTier[agent.TierMoney])
		}
	}
}

// ---------------------------------------------------------------------------
// argument validation
// ---------------------------------------------------------------------------

// TestEveryToolRefusesArgumentsItCannotActOn. A tool that invents a default for
// a missing counterparty or a missing quantity is a tool that acts on a guess.
func TestEveryToolRefusesArgumentsItCannotActOn(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		want error
	}{
		{"exposure without a counterparty", "get_exposure", map[string]any{}, ErrMissingArg},
		{"exposure for a counterparty that does not exist", "get_exposure", map[string]any{"counterparty": "CP-NOBODY"}, ErrUnknownCP},
		{"collateral value without a counterparty", "get_collateral_value", map[string]any{}, ErrMissingArg},
		{"position without a counterparty", "get_position", map[string]any{}, ErrMissingArg},
		{"as-of without a counterparty", "query_asof", map[string]any{"as_of": "2026-09-03"}, ErrMissingArg},
		{"whitelist check without a counterparty", "check_whitelist", map[string]any{}, ErrMissingArg},
		{"notes without a counterparty", "get_counterparty_notes", map[string]any{}, ErrMissingArg},
		{"notes for a counterparty that does not exist", "get_counterparty_notes", map[string]any{"counterparty": "CP-NOBODY"}, ErrUnknownCP},

		{"a proposal without a counterparty", "propose_margin_call", map[string]any{"shortfall": 1_000_000.0}, ErrMissingArg},
		{"a proposal without an amount", "propose_margin_call", map[string]any{"counterparty": "CP-DELTA"}, ErrMissingArg},

		{"a draft without a counterparty", "draft_transfer", map[string]any{"quantity": 100.0}, ErrMissingArg},
		{"a draft without a quantity", "draft_transfer", map[string]any{"counterparty": "CP-ALPHA"}, ErrMissingArg},
		{"a draft of nothing", "draft_transfer", map[string]any{"counterparty": "CP-ALPHA", "quantity": 0.0}, ErrMissingArg},
		{"a draft of a negative quantity", "draft_transfer", map[string]any{"counterparty": "CP-ALPHA", "quantity": -5.0}, ErrMissingArg},

		{"an issue without a counterparty", "issue_margin_call", map[string]any{"amount": 1.0}, ErrMissingArg},
		{"an issue without an amount", "issue_margin_call", map[string]any{"counterparty": "CP-DELTA"}, ErrMissingArg},

		{"a transfer without a counterparty", "execute_transfer", map[string]any{"quantity": 100.0}, ErrMissingArg},
		{"a transfer without a quantity", "execute_transfer", map[string]any{"counterparty": "CP-ALPHA"}, ErrMissingArg},
		{"a transfer of nothing", "execute_transfer", map[string]any{"counterparty": "CP-ALPHA", "quantity": 0.0}, ErrMissingArg},
		{"a transfer to a counterparty that does not exist", "execute_transfer", map[string]any{"counterparty": "CP-NOBODY", "quantity": 100.0}, ErrUnknownCP},

		{"a redemption without a quantity", "redeem_shares", map[string]any{"counterparty": "CP-ALPHA"}, ErrMissingArg},
		{"a redemption of a negative quantity", "redeem_shares", map[string]any{"counterparty": "CP-ALPHA", "quantity": -1.0}, ErrMissingArg},
		{"a redemption for a counterparty that does not exist", "redeem_shares", map[string]any{"counterparty": "CP-NOBODY", "quantity": 1.0}, ErrUnknownCP},

		{"an unknown stress scenario", "run_stress_scenario", map[string]any{"scenario": "asteroid"}, ErrNotFound},
	}
	p := newTestPlatform(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := invoke(t, p, tc.tool, tc.args)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestACounterpartyIsNormalisedRatherThanRejectedForItsCase(t *testing.T) {
	p := newTestPlatform(t)
	res := mustInvoke(t, p, "get_exposure", map[string]any{"counterparty": "  cp-delta "})
	if !strings.Contains(res.Content, "CP-DELTA") {
		t.Fatalf("content = %q, want the canonical identifier", res.Content)
	}
	if got := res.Data["counterparty"]; got != "CP-DELTA" {
		t.Fatalf("data counterparty = %v", got)
	}
}

// ---------------------------------------------------------------------------
// the margin call chain
// ---------------------------------------------------------------------------

// TestAProposalIsAnchoredToTheEnginesFigureRatherThanTheModelsArgument. A call
// is a legal demand and the number on it has to come from the calculation.
func TestAProposalIsAnchoredToTheEnginesFigureRatherThanTheModelsArgument(t *testing.T) {
	p := newTestPlatform(t)
	engine := mustInvoke(t, p, "get_exposure", map[string]any{"counterparty": "CP-DELTA"})
	want := engine.Data["shortfall"].(float64)
	if want <= 0 {
		t.Fatalf("the fixture expects CP-DELTA to be undermargined, shortfall = %v", want)
	}

	for _, asked := range []float64{1, want / 2, want * 10} {
		res := mustInvoke(t, p, "propose_margin_call", map[string]any{
			"counterparty": "CP-DELTA", "shortfall": asked, "currency": "USD",
		})
		if got := res.Data["shortfall"].(float64); got != want {
			t.Fatalf("asked for %v, proposal carries %v, want the engine's %v", asked, got, want)
		}
		if !strings.Contains(res.Content, "requires human approval") {
			t.Fatalf("a proposal must say it is only a proposal: %q", res.Content)
		}
		if res.Effect != "margin call proposed" {
			t.Fatalf("effect = %q", res.Effect)
		}
	}
}

func TestAProposalIsRefusedForACounterpartyWithNoShortfall(t *testing.T) {
	p := newTestPlatform(t)
	// CP-BRAVO is fully collateralised in the seed.
	exp := mustInvoke(t, p, "get_exposure", map[string]any{"counterparty": "CP-BRAVO"})
	if got := exp.Data["shortfall"].(float64); got != 0 {
		t.Fatalf("the fixture expects CP-BRAVO to be covered, shortfall = %v", got)
	}
	res, err := invoke(t, p, "propose_margin_call", map[string]any{
		"counterparty": "CP-BRAVO", "shortfall": 5_000_000.0,
	})
	if !errors.Is(err, ErrNotEligible) {
		t.Fatalf("error = %v, want ErrNotEligible", err)
	}
	if !strings.Contains(res.Content, "no call is due") {
		t.Fatalf("the refusal should explain itself: %q", res.Content)
	}
	if len(p.Proposals()) != 0 {
		t.Fatalf("a refused proposal must not be recorded: %+v", p.Proposals())
	}
}

// TestOnlyWhatWasProposedCanBeIssued. The issuing tool re-checks the proposal
// rather than trusting the arguments it is handed, so an approval granted for
// one figure cannot be spent on another.
func TestOnlyWhatWasProposedCanBeIssued(t *testing.T) {
	p := newTestPlatform(t)
	proposal := mustInvoke(t, p, "propose_margin_call", map[string]any{
		"counterparty": "CP-DELTA", "shortfall": 1.0,
	})
	id := proposal.Data["proposal_id"].(string)
	amount := proposal.Data["shortfall"].(float64)

	cases := []struct {
		name string
		args map[string]any
		want error
	}{
		{
			name: "a proposal id that was never issued",
			args: map[string]any{"counterparty": "CP-DELTA", "amount": amount, "proposal_id": "MC-CP-DELTA-999999"},
			want: ErrNotFound,
		},
		{
			name: "no proposal id at all",
			args: map[string]any{"counterparty": "CP-DELTA", "amount": amount},
			want: ErrNotFound,
		},
		{
			name: "an amount that disagrees with the proposal",
			args: map[string]any{"counterparty": "CP-DELTA", "amount": amount + 1, "proposal_id": id},
			want: ErrNotEligible,
		},
		{
			name: "a counterparty that disagrees with the proposal",
			args: map[string]any{"counterparty": "CP-ALPHA", "amount": amount, "proposal_id": id},
			want: ErrNotEligible,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := invoke(t, p, "issue_margin_call", tc.args); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(p.Issued()) != 0 {
				t.Fatalf("nothing should have been issued: %+v", p.Issued())
			}
		})
	}

	res := mustInvoke(t, p, "issue_margin_call", map[string]any{
		"counterparty": "CP-DELTA", "amount": amount, "proposal_id": id,
	})
	if res.Effect != "margin call issued" {
		t.Fatalf("effect = %q", res.Effect)
	}
	issued := p.Issued()
	if len(issued) != 1 || issued[0].Status != domain.MarginIssued {
		t.Fatalf("issued = %+v", issued)
	}
}

// ---------------------------------------------------------------------------
// transfers
// ---------------------------------------------------------------------------

// TestATransferToACounterpartyOutsideTheRegistryIsRefusedByTheLedgerItself.
// The policy gateway refuses it first in a governed run; this is the check that
// the permissioned token refuses it too, so the control does not depend on the
// gateway alone.
func TestATransferToACounterpartyOutsideTheRegistryIsRefusedByTheLedgerItself(t *testing.T) {
	p := newTestPlatform(t)
	if _, err := invoke(t, p, "execute_transfer", map[string]any{
		"counterparty": "CP-HOTEL", "quantity": 5_000_000.0, "ledger": "evm",
	}); !errors.Is(err, ErrLedgerRefuse) {
		t.Fatalf("error = %v, want ErrLedgerRefuse", err)
	}
}

func TestATransferToARegisteredCounterpartySettlesAndClosesItsDraft(t *testing.T) {
	p := newTestPlatform(t)
	draft := mustInvoke(t, p, "draft_transfer", map[string]any{
		"counterparty": "CP-ALPHA", "quantity": 1_000_000.0, "ledger": "evm",
	})
	id := draft.Data["draft_id"].(string)
	if draft.Data["whitelisted"] != true {
		t.Fatalf("CP-ALPHA should be whitelisted in the fixture: %v", draft.Data)
	}

	res := mustInvoke(t, p, "execute_transfer", map[string]any{
		"counterparty": "CP-ALPHA", "quantity": 1_000_000.0, "ledger": "evm", "draft_id": id,
	})
	if res.Effect != "shares transferred" {
		t.Fatalf("effect = %q", res.Effect)
	}
	drafts := p.Drafts()
	if len(drafts) != 1 || !drafts[0].Executed || drafts[0].TxHash == "" {
		t.Fatalf("the draft must be closed by the settlement: %+v", drafts)
	}
}

func TestADraftToAnUnregisteredCounterpartyWarnsWithoutSettling(t *testing.T) {
	p := newTestPlatform(t)
	res := mustInvoke(t, p, "draft_transfer", map[string]any{
		"counterparty": "CP-HOTEL", "quantity": 5_000_000.0,
	})
	if res.Data["whitelisted"] != false {
		t.Fatalf("whitelisted = %v, want false", res.Data["whitelisted"])
	}
	if !strings.Contains(res.Content, "cannot settle") {
		t.Fatalf("the draft must warn: %q", res.Content)
	}
	if !strings.Contains(res.Content, "requires human approval") {
		t.Fatalf("a draft must say it settles nothing on its own: %q", res.Content)
	}
	if d := p.Drafts(); len(d) != 1 || d[0].Executed {
		t.Fatalf("drafts = %+v", d)
	}
}

func TestTheWhitelistCheckAgreesWithWhatTheLedgerWillDo(t *testing.T) {
	p := newTestPlatform(t)
	cases := map[string]bool{"CP-ALPHA": true, "CP-DELTA": true, "CP-HOTEL": false}
	for cp, want := range cases {
		t.Run(cp, func(t *testing.T) {
			res := mustInvoke(t, p, "check_whitelist", map[string]any{"counterparty": cp, "ledger": "evm"})
			if got := res.Data["whitelisted"]; got != want {
				t.Fatalf("whitelisted = %v, want %v", got, want)
			}
			_, err := invoke(t, p, "execute_transfer", map[string]any{"counterparty": cp, "quantity": 1000.0})
			settled := err == nil
			if settled != want {
				t.Fatalf("the check said %v but the ledger said %v (%v)", want, settled, err)
			}
		})
	}
}

func TestARedemptionBurnsWhatTheHolderActuallyHas(t *testing.T) {
	p := newTestPlatform(t)
	res := mustInvoke(t, p, "redeem_shares", map[string]any{"counterparty": "CP-ALPHA", "quantity": 1_000.0})
	if res.Effect != "shares redeemed" {
		t.Fatalf("effect = %q", res.Effect)
	}
	if _, err := invoke(t, p, "redeem_shares", map[string]any{
		"counterparty": "CP-ALPHA", "quantity": 1e12,
	}); !errors.Is(err, ErrLedgerRefuse) {
		t.Fatalf("redeeming more than is held = %v, want ErrLedgerRefuse", err)
	}
}

// ---------------------------------------------------------------------------
// grounding
// ---------------------------------------------------------------------------

// knownGroundingGaps records, per read tool, the figures the tool prints that
// are in no entry of its own Grounded set.
//
// The package claims that "every figure a tool returns is listed in the
// result's Grounded set". It holds for every read tool, and the map below is
// empty because of that rather than because nothing is checked: the test
// compares the actual gap against this map exactly, so a formatter that starts
// printing a figure the tool does not ground fails here, named and quoted.
//
// The claim is worth enforcing because the runtime also grounds each tool's
// rendered Content wholesale, which would otherwise hide the gap completely —
// the output guardrail would admit the number and nobody would learn that no
// engine had vouched for it.
var knownGroundingGaps = map[string][]string{}

func ungroundedFigures(res agent.ToolResult) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range (guard.GroundingDetector{Grounded: res.Grounded, AllowSmall: true}).Detect(res.Content) {
		if !seen[f.Excerpt] {
			seen[f.Excerpt] = true
			out = append(out, f.Excerpt)
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryFigureAReadToolPrintsIsInItsOwnGroundedSet is the property the output
// guardrail depends on. If a tool prints a number it did not ground, the model
// may repeat that number and the guardrail has no way to tell an engine's figure
// from an invented one.
func TestEveryFigureAReadToolPrintsIsInItsOwnGroundedSet(t *testing.T) {
	p := newTestPlatform(t)
	for _, tool := range p.All() {
		s := tool.Spec()
		if s.Tier != agent.TierRead {
			continue
		}
		t.Run(s.Name, func(t *testing.T) {
			if why, exempt := groundingExempt[s.Name]; exempt {
				t.Skip(why)
			}
			args, ok := readToolArgs[s.Name]
			if !ok {
				t.Fatalf("no argument set for %q", s.Name)
			}
			res := mustInvoke(t, p, s.Name, args)
			if strings.TrimSpace(res.Content) == "" {
				t.Fatal("a read tool must render an answer")
			}
			got := ungroundedFigures(res)
			want := knownGroundingGaps[s.Name]
			if !reflect.DeepEqual(got, want) {
				if want == nil {
					t.Fatalf("content prints %v, which is in no grounded entry\ncontent:\n%s\ngrounded: %v",
						got, res.Content, res.Grounded)
				}
				t.Fatalf("the documented grounding gap changed\n got: %v\nwant: %v\ncontent:\n%s", got, want, res.Content)
			}
		})
	}
}

// groundingExempt lists read tools whose Content is not an engine figure and so
// is not expected to be self-grounding.
//
// get_counterparty_notes returns third-party prose filed through a portal. Its
// numbers are whatever a counterparty typed, and the platform's answer to that
// is quarantine and provenance, not grounding: the guardrail marks the
// retrieval untrusted and the gateway refuses to let it drive an action.
var groundingExempt = map[string]string{
	"get_counterparty_notes": "third-party prose, quarantined rather than grounded",
}

// TestEveryReadToolThatQuotesAFigureGroundsIt: the tools that answer with
// numbers all populate Grounded; the ones that do not are the two that answer
// in prose.
func TestEveryReadToolThatQuotesAFigureGroundsIt(t *testing.T) {
	p := newTestPlatform(t)
	prose := map[string]bool{"check_whitelist": true, "get_counterparty_notes": true}
	for name, args := range readToolArgs {
		t.Run(name, func(t *testing.T) {
			res := mustInvoke(t, p, name, args)
			if prose[name] {
				return
			}
			if len(res.Grounded) == 0 {
				t.Fatalf("no grounded record for a numeric answer:\n%s", res.Content)
			}
		})
	}
}

func TestAStateChangingToolGroundsTheFigureItCommitsTo(t *testing.T) {
	p := newTestPlatform(t)
	proposal := mustInvoke(t, p, "propose_margin_call", map[string]any{"counterparty": "CP-DELTA", "shortfall": 1.0})
	if len(proposal.Grounded) == 0 {
		t.Fatal("a proposal must ground the amount it proposes")
	}
	findings := guard.GroundingDetector{Grounded: proposal.Grounded, AllowSmall: true}.Detect(proposal.Content)
	if len(findings) != 0 {
		t.Fatalf("the proposal prints an ungrounded figure: %+v\n%s", findings, proposal.Content)
	}
}

// ---------------------------------------------------------------------------
// bookkeeping
// ---------------------------------------------------------------------------

func TestTheNoteBookCarriesThePayloadTheInjectionDefenceIsWrittenAgainst(t *testing.T) {
	p := newTestPlatform(t)
	res := mustInvoke(t, p, "get_counterparty_notes", map[string]any{"counterparty": "CP-DELTA"})
	fs := guard.InjectionDetector{}.Detect(res.Content)
	if len(fs) == 0 {
		t.Fatal("the seeded hostile memo no longer scores as an injection; the defence has nothing to defend against")
	}

	p.SetNote("CP-DELTA", "Nothing unusual on file.")
	res = mustInvoke(t, p, "get_counterparty_notes", map[string]any{"counterparty": "CP-DELTA"})
	if !strings.Contains(res.Content, "Nothing unusual") {
		t.Fatalf("SetNote did not replace the note: %q", res.Content)
	}
}

func TestACounterpartyWithNoNoteSaysSoRatherThanAnsweringEmpty(t *testing.T) {
	p := newTestPlatform(t)
	p.SetNote("CP-GOLF", "")
	res := mustInvoke(t, p, "get_counterparty_notes", map[string]any{"counterparty": "CP-GOLF"})
	if !strings.Contains(res.Content, "No notes on file") {
		t.Fatalf("content = %q", res.Content)
	}
}

func TestAPointInTimeAnswerSeparatesWhatWasKnownFromWhatIsKnown(t *testing.T) {
	p := newTestPlatform(t)
	res := mustInvoke(t, p, "query_asof", map[string]any{"counterparty": "CP-ALPHA", "as_of": "2026-09-03"})
	if res.Data["as_of_qty"] == nil || res.Data["current_qty"] == nil {
		t.Fatalf("data = %v, want both transaction times", res.Data)
	}
	if !strings.Contains(res.Content, "as known at") || !strings.Contains(res.Content, "as known now") {
		t.Fatalf("content = %q, want both readings labelled", res.Content)
	}
}

func TestAStressScenarioHonoursAnOverriddenShock(t *testing.T) {
	p := newTestPlatform(t)
	base := mustInvoke(t, p, "run_stress_scenario", map[string]any{"scenario": "rate_shock"})
	harder := mustInvoke(t, p, "run_stress_scenario", map[string]any{"scenario": "rate_shock", "shock_bps": 5000.0})
	if base.Content == harder.Content {
		t.Fatal("a larger shock must produce a different result")
	}
	if base.Data["scenario"] != "rate_shock" || harder.Data["scenario"] != "rate_shock" {
		t.Fatalf("scenario ids = %v / %v", base.Data["scenario"], harder.Data["scenario"])
	}
}

func TestAScenarioDefaultsToARateShockRatherThanFailing(t *testing.T) {
	p := newTestPlatform(t)
	res := mustInvoke(t, p, "run_stress_scenario", map[string]any{})
	if res.Data["scenario"] != "rate_shock" {
		t.Fatalf("scenario = %v, want the documented default", res.Data["scenario"])
	}
}

func TestAToolWithNoEnginesBehindItReportsNotFoundRatherThanPanicking(t *testing.T) {
	bare := NewPlatform(nil, nil, nil, obs.NewManualClock(domain.NewSeed().AsOf), nil, nil)
	for _, name := range []string{"list_recon_breaks", "get_supply_reconciliation"} {
		tool := toolNamed(t, bare, name)
		if _, err := tool.Invoke(context.Background(), map[string]any{}); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s = %v, want ErrNotFound", name, err)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func TestNumericArgumentsAreAcceptedInTheFormsAModelActuallySends(t *testing.T) {
	cases := []struct {
		in   any
		want float64
		ok   bool
	}{
		{5_000_000.0, 5_000_000, true},
		{float32(2.5), 2.5, true},
		{7, 7, true},
		{int64(9), 9, true},
		{"1,250,000", 1_250_000, true},
		{"3.5", 3.5, true},
		{nil, 0, false},
		{true, 0, false},
		{map[string]any{}, 0, false},
	}
	for _, tc := range cases {
		got, ok := numArg(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("numArg(%#v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestThousandsRendersFiguresADeskCanRead(t *testing.T) {
	cases := map[int64]string{
		0: "0", 7: "7", 999: "999", 1_000: "1,000", 35_298_750: "35,298,750",
		-400_000: "-400,000", 1_000_000_000: "1,000,000,000",
	}
	for in, want := range cases {
		if got := thousands(in); got != want {
			t.Errorf("thousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestATimeArgumentFallsBackRatherThanGuessing(t *testing.T) {
	def := time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":                     def,
		"not a date":           def,
		"2026-09-01":           time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		"2026-09-01T09:30:00Z": time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC),
		"2026-09-01T09:30:00":  time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC),
	}
	for in, want := range cases {
		if got := parseTime(in, def); !got.Equal(want) {
			t.Errorf("parseTime(%q) = %s, want %s", in, got, want)
		}
	}
}
