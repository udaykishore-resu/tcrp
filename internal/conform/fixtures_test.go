package conform_test

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// epoch is the fixed instant every fixture is built at. Nothing in these tests
// reads a wall clock, which is what lets the determinism test assert on a
// bundle digest rather than on a shape.
var epoch = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

func testClock() *obs.ManualClock { return obs.NewManualClock(epoch) }

// chain builds a real hash-chained audit log from hand-written records, so the
// fixtures carry genuine hashes and AGENT-5 has something to verify.
type chain struct {
	t     *testing.T
	log   *audit.Log
	clock *obs.ManualClock
}

func newChain(t *testing.T) *chain {
	t.Helper()
	c := testClock()
	return &chain{t: t, log: audit.New(c), clock: c}
}

// add appends a record and advances the clock a second, so sequence order and
// time order agree.
func (c *chain) add(r audit.Record) audit.Record {
	c.t.Helper()
	out, err := c.log.Append(r)
	if err != nil {
		c.t.Fatalf("append: %v", err)
	}
	c.clock.Advance(time.Second)
	return out
}

func (c *chain) records() []audit.Record { return c.log.Records() }

// runStarted, policyDecision and friends keep the fixtures readable: a test
// that spells out fourteen struct literals hides which field is the one under
// test.

func runStarted(run, actor string) audit.Record {
	return audit.Record{
		Actor: actor, Tenant: "acme", Action: audit.ActionRunStarted,
		Subject: "raise a margin call", Outcome: audit.OutcomeOK, RunID: run,
		TraceID: "0000000000000000000000000000ab01", SpanID: "00000000000000a1",
	}
}

func runCompleted(run, actor string) audit.Record {
	return audit.Record{
		Actor: actor, Tenant: "acme", Action: audit.ActionRunCompleted,
		Subject: "raise a margin call", Outcome: audit.OutcomeOK, RunID: run,
		TraceID: "0000000000000000000000000000ab01", SpanID: "00000000000000a1",
	}
}

func guardVerdict(run, surface string) audit.Record {
	return audit.Record{
		Actor: "agent.collateral", Tenant: "acme", Action: audit.ActionGuardVerdict,
		Subject: surface, Outcome: audit.OutcomeOK, RunID: run,
		Details: map[string]any{"verdict": "allow"},
	}
}

func policyDecision(run, tool, outcome string, extra map[string]any) audit.Record {
	d := map[string]any{"rule": "allow-margin-call", "tier": "money_movement"}
	for k, v := range extra {
		d[k] = v
	}
	return audit.Record{
		Actor: "agent.collateral", OnBehalf: "trader.rossi", Tenant: "acme",
		Action: audit.ActionPolicyDecision, Subject: tool, Outcome: outcome, RunID: run,
		Details: d,
	}
}

func approvalGranted(run, approver, approvalID string) audit.Record {
	return audit.Record{
		Actor: approver, Tenant: "acme", Action: audit.ActionApprovalGranted,
		Subject: "issue_margin_call", Outcome: audit.OutcomeOK, RunID: run,
		Details: map[string]any{"approval_id": approvalID, "grants": 1, "required": 1},
	}
}

func moneyInvoked(run, tool, approvalID string) audit.Record {
	d := map[string]any{"tier": "money_movement", "effect": "margin call issued"}
	if approvalID != "" {
		d["approval_id"] = approvalID
	}
	return audit.Record{
		Actor: "agent.collateral", OnBehalf: "trader.rossi", Tenant: "acme",
		Action: audit.ActionMarginCallIssued, Subject: tool, Outcome: audit.OutcomeOK,
		RunID: run, Details: d,
	}
}

func readInvoked(run, tool string) audit.Record {
	return audit.Record{
		Actor: "agent.collateral", OnBehalf: "trader.rossi", Tenant: "acme",
		Action: audit.ActionToolInvoked, Subject: tool, Outcome: audit.OutcomeOK, RunID: run,
		Details: map[string]any{"tier": "read_only"},
	}
}

// span builds an exported span with the identity attributes filled in. Passing
// an empty subject is how a test drops one.
func span(traceID, spanID, name, subject, tenant, run string) obs.SpanData {
	attrs := map[string]any{}
	if subject != "" {
		attrs[obs.AttrPrincipalSubject] = subject
	}
	if tenant != "" {
		attrs[obs.AttrPrincipalTenant] = tenant
	}
	if run != "" {
		attrs[obs.AttrRunID] = run
	}
	return obs.SpanData{
		TraceID: traceID, SpanID: spanID, Name: name, Kind: obs.KindInternal,
		Start: epoch, End: epoch.Add(time.Millisecond), Status: obs.StatusOK,
		Attributes: attrs,
	}
}

// fakeLedger is a ledger.Ledger built from literal figures.
//
// It exists so a settlement invariant can be tested against a supply position
// that has been stated rather than simulated. The write side is unimplemented
// because conformance never writes: an invariant that could change a ledger
// would be measuring the platform with itself.
type fakeLedger struct {
	name     domain.Ledger
	supply   map[string]int64
	locked   map[string]int64
	balances map[string]int64 // "holder|symbol"
	events   []domain.LedgerEvent
	head     ledger.Block
}

func (f *fakeLedger) Name() domain.Ledger { return f.name }

func (f *fakeLedger) BalanceOf(holder, symbol string) int64 { return f.balances[holder+"|"+symbol] }

func (f *fakeLedger) TotalSupply(symbol string) int64 { return f.supply[symbol] }

func (f *fakeLedger) Locked(symbol string) int64 { return f.locked[symbol] }

func (f *fakeLedger) Head() ledger.Block { return f.head }

func (f *fakeLedger) Events(from uint64) []domain.LedgerEvent {
	out := make([]domain.LedgerEvent, 0, len(f.events))
	for _, e := range f.events {
		if e.Seq >= from {
			out = append(out, e)
		}
	}
	return out
}

func (f *fakeLedger) Subscribe() <-chan domain.LedgerEvent {
	c := make(chan domain.LedgerEvent)
	close(c)
	return c
}

func (f *fakeLedger) Mint(context.Context, string, string, string, int64, string) (ledger.Receipt, error) {
	panic("conformance must not write to a ledger")
}

func (f *fakeLedger) Burn(context.Context, string, string, string, int64, string) (ledger.Receipt, error) {
	panic("conformance must not write to a ledger")
}

func (f *fakeLedger) Transfer(context.Context, string, string, string, string, int64, string) (ledger.Receipt, error) {
	panic("conformance must not write to a ledger")
}

func (f *fakeLedger) Lock(context.Context, string, string, string, int64, string) (ledger.Receipt, error) {
	panic("conformance must not write to a ledger")
}

func (f *fakeLedger) Unlock(context.Context, string, string, string, int64, string) (ledger.Receipt, error) {
	panic("conformance must not write to a ledger")
}

// newFakeLedger builds a ledger with a plausible head block.
func newFakeLedger(name domain.Ledger) *fakeLedger {
	return &fakeLedger{
		name:     name,
		supply:   map[string]int64{},
		locked:   map[string]int64{},
		balances: map[string]int64{},
		head: ledger.Block{
			Height: 100, Hash: "0x" + string(name) + "-head", ParentHash: "0x" + string(name) + "-parent",
			Time: epoch, Finalised: true, TxCount: 1,
		},
	}
}

// event appends a ledger event.
func (f *fakeLedger) event(kind domain.EventKind, symbol string, qty int64, memo string) *fakeLedger {
	seq := uint64(len(f.events) + 1)
	f.events = append(f.events, domain.LedgerEvent{
		Seq: seq, Ledger: f.name, Kind: kind, Symbol: symbol, Quantity: qty,
		Block: 90 + seq, TxHash: "0xtx" + string(f.name) + string(rune('0'+seq%10)),
		OccurredAt: epoch, ObservedAt: epoch, Finalised: true, Memo: memo,
	})
	return f
}
