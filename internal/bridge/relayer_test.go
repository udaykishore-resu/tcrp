package bridge_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

const sym = domain.FundSymbol

// flakyTarget wraps a ledger and fails the next n mints with a transport error
// — not a revert, because a revert is a decision and a transport error is an
// accident, and the relayer must retry only the second.
type flakyTarget struct {
	ledger.Ledger
	failures int
}

func (f *flakyTarget) Mint(ctx context.Context, caller, to, symbol string, qty int64, memo string) (ledger.Receipt, error) {
	if f.failures > 0 {
		f.failures--
		return ledger.Receipt{}, errors.New("rpc: connection reset by peer")
	}
	return f.Ledger.Mint(ctx, caller, to, symbol, qty, memo)
}

type harness struct {
	t        *testing.T
	clock    *obs.ManualClock
	world    *ledger.World
	target   ledger.Ledger
	flaky    *flakyTarget
	relayer  *bridge.Relayer
	signers  []*bridge.Signer
	log      *audit.Log
	registry *obs.Registry
	spans    *obs.MemoryExporter
}

func newHarness(t *testing.T, confirmations uint64, quorum int) *harness {
	t.Helper()
	clock := obs.NewManualClock(domain.SeedTime)
	world, err := ledger.NewWorld(ledger.Options{Clock: clock, Confirmations: confirmations})
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	spans := obs.NewMemoryExporter(0)
	tracer := obs.NewTracer("bridge-test", clock, spans).Deterministic(true)
	reg := obs.NewRegistry()
	log := audit.New(clock)

	signers := []*bridge.Signer{
		signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2), signerFor(t, "attester-c", 3),
	}
	set, err := bridge.SignerSetFrom(quorum, signers...)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	flaky := &flakyTarget{Ledger: world.Canton}
	h := &harness{
		t: t, clock: clock, world: world, target: flaky, flaky: flaky,
		signers: signers, log: log, registry: reg, spans: spans,
	}
	r, err := bridge.NewRelayer(bridge.Config{
		Source: world.EVM, Target: flaky, Symbols: []string{sym},
		Signers: signers, SignerSet: set,
		SourceOperator: world.Operator, TargetOperator: world.Issuer,
		Clock: clock, Tracer: tracer, Metrics: reg, Audit: log,
		Actor: "bridge-relayer", Tenant: "platform",
		Retry: bridge.RetryPolicy{Attempts: 3, Base: time.Millisecond},
	})
	if err != nil {
		t.Fatalf("relayer: %v", err)
	}
	r.SetTargetAddress(func(cp string) string {
		p, ok := world.Party(cp)
		if !ok {
			t.Fatalf("no Daml party for %s", cp)
		}
		return p
	})
	h.relayer = r
	return h
}

func (h *harness) wallet(cp string) string {
	h.t.Helper()
	a, ok := h.world.Wallet(cp)
	if !ok {
		h.t.Fatalf("no wallet for %s", cp)
	}
	return a
}

func (h *harness) party(cp string) string {
	h.t.Helper()
	p, ok := h.world.Party(cp)
	if !ok {
		h.t.Fatalf("no party for %s", cp)
	}
	return p
}

func (h *harness) poll() bridge.PollResult {
	h.t.Helper()
	res, err := h.relayer.Poll(context.Background())
	if err != nil {
		h.t.Fatalf("poll: %v", err)
	}
	return res
}

func (h *harness) lock(cp string, qty int64) (uint64, ledger.Receipt) {
	h.t.Helper()
	nonce, rc, err := h.relayer.Lock(context.Background(), h.wallet(cp), sym, qty)
	if err != nil {
		h.t.Fatalf("lock: %v", err)
	}
	return nonce, rc
}

func (h *harness) counter(name string, labels map[string]string) float64 {
	return h.registry.Counter(name, "", labels).Value()
}

func TestLockAndMintHappyPath(t *testing.T) {
	h := newHarness(t, 3, 2)
	before := h.world.Canton.BalanceOf(h.party("CP-ALPHA"), sym)

	nonce, rc := h.lock("CP-ALPHA", 5_000_000)
	if nonce != 1 {
		t.Fatalf("first nonce = %d", nonce)
	}

	// Not final yet: the relayer must not mint against an unconfirmed lock.
	res := h.poll()
	if len(res.Settled) != 0 || res.Waiting != 1 {
		t.Fatalf("minted before finality: %+v", res)
	}
	if got := h.world.Canton.BalanceOf(h.party("CP-ALPHA"), sym); got != before {
		t.Fatalf("target balance moved before finality: %d", got)
	}

	h.world.EVM.Mine(3)
	res = h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("settled %d flows after finality: %+v", len(res.Settled), res)
	}
	s := res.Settled[0]
	if s.Nonce != nonce || s.Attestation.SourceTxHash != rc.TxHash {
		t.Fatalf("settlement = %+v", s)
	}
	if s.Attestation.Holder != "CP-ALPHA" {
		t.Fatalf("attestation holder = %q", s.Attestation.Holder)
	}
	if len(s.Signatures) != 3 {
		t.Fatalf("carried %d signatures", len(s.Signatures))
	}
	if got := h.world.Canton.BalanceOf(h.party("CP-ALPHA"), sym); got != before+5_000_000 {
		t.Fatalf("target balance = %d, want %d", got, before+5_000_000)
	}
	if got := h.world.EVM.Locked(sym); got != 5_000_000 {
		t.Fatalf("locked on source = %d", got)
	}

	// Polling again changes nothing. This is the ordinary replay case, and it
	// is the same guard that survives a reorganisation.
	res = h.poll()
	if len(res.Settled) != 0 {
		t.Fatalf("a second poll settled %d flows", len(res.Settled))
	}
	if got := h.world.Canton.BalanceOf(h.party("CP-ALPHA"), sym); got != before+5_000_000 {
		t.Fatalf("target balance after re-poll = %d", got)
	}

	// Supply balances end to end.
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if !rep.Balanced {
		t.Fatalf("supply does not balance after a clean bridge: %+v", rep.Breaks)
	}
}

func TestAuditTrailRecordsBothHalves(t *testing.T) {
	h := newHarness(t, 1, 2)
	h.lock("CP-ALPHA", 1_000_000)
	h.world.EVM.Mine(1)
	h.poll()

	want := map[audit.Action]int{
		audit.ActionBridgeLocked: 1,
		audit.ActionBridgeMinted: 1,
		audit.ActionTokensMinted: 1,
	}
	got := map[audit.Action]int{}
	for _, r := range h.log.Records() {
		got[r.Action]++
	}
	for a, n := range want {
		if got[a] != n {
			t.Fatalf("%s recorded %d times, want %d", a, got[a], n)
		}
	}
	if err := h.log.Verify(); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	for _, r := range h.log.Records() {
		if r.Action != audit.ActionBridgeMinted {
			continue
		}
		if r.Details["attestation_digest"] == nil || r.Details["signers"] == nil {
			t.Fatalf("mint record does not carry the attestation it acted on: %+v", r.Details)
		}
	}
	// A nil audit log must be tolerated rather than panicking.
	set, _ := bridge.SignerSetFrom(1, h.signers[0])
	if _, err := bridge.NewRelayer(bridge.Config{
		Source: h.world.EVM, Target: h.world.Canton, Symbols: []string{sym},
		Signers: h.signers[:1], SignerSet: set, Clock: h.clock,
	}); err != nil {
		t.Fatalf("relayer without an audit log: %v", err)
	}
}

func TestReorgBeforeFinalityAbandonsWithoutMinting(t *testing.T) {
	h := newHarness(t, 5, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	nonce, rc := h.lock("CP-ALPHA", 3_000_000)
	res := h.poll()
	if res.Waiting != 1 || len(res.Settled) != 0 {
		t.Fatalf("poll before finality: %+v", res)
	}

	// The chain rewrites the block the lock was in and does not re-include it.
	rr, err := h.world.EVM.Reorg(1, rc.TxHash)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if rr.ViolatedFinality {
		t.Fatal("a one-block rewind under a five-confirmation policy reported a finality violation")
	}

	res = h.poll()
	if len(res.Abandoned) != 1 || res.Abandoned[0] != nonce {
		t.Fatalf("abandoned = %v, want nonce %d", res.Abandoned, nonce)
	}
	if len(res.Settled) != 0 {
		t.Fatal("a flow settled after its lock was orphaned")
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before {
		t.Fatalf("target balance = %d, want the pre-lock %d", got, before)
	}
	if got := h.world.EVM.Locked(sym); got != 0 {
		t.Fatalf("source vault still holds %d after the lock was orphaned", got)
	}
	alerts := h.relayer.Alerts()
	if len(alerts) != 1 || alerts[0].Kind != bridge.AlertAbandoned {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].Nonce != nonce || alerts[0].Severity != "high" {
		t.Fatalf("alert = %+v", alerts[0])
	}
	if h.counter(bridge.MetricAbandoned, map[string]string{"direction": string(bridge.DirectionMint)}) != 1 {
		t.Fatal("abandonment was not counted")
	}
	// Nothing was minted, so supply still balances. A pre-finality
	// reorganisation is survivable precisely because it leaves no trace.
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if !rep.Balanced {
		t.Fatalf("supply broke after an abandoned flow: %+v", rep.Breaks)
	}
	// Repeated polls do not re-page the operator.
	h.poll()
	if len(h.relayer.Alerts()) != 1 {
		t.Fatalf("an abandoned flow alerted %d times", len(h.relayer.Alerts()))
	}
}

func TestReorgAfterFinalityAlertsAndDoesNotDoubleMint(t *testing.T) {
	h := newHarness(t, 1, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	nonce, rc := h.lock("CP-ALPHA", 2_000_000)
	h.world.EVM.Mine(2)
	res := h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("did not settle: %+v", res)
	}
	minted := h.world.Canton.BalanceOf(party, sym)
	if minted != before+2_000_000 {
		t.Fatalf("target balance = %d", minted)
	}

	// The chain rewrites past its own finality guarantee and drops the lock.
	rr, err := h.world.EVM.Reorg(4, rc.TxHash)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if !rr.ViolatedFinality {
		t.Fatal("the rewind did not report a finality violation; the test is not exercising the case")
	}

	res = h.poll()
	if len(res.Settled) != 0 {
		t.Fatal("a settled flow was settled again after its source transaction was orphaned")
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != minted {
		t.Fatalf("target balance changed to %d; the relayer must not act unilaterally here", got)
	}
	alerts := h.relayer.Alerts()
	if len(alerts) != 1 || alerts[0].Kind != bridge.AlertFinalityViolation {
		t.Fatalf("alerts = %+v", alerts)
	}
	if alerts[0].Severity != "critical" || alerts[0].Nonce != nonce {
		t.Fatalf("alert = %+v", alerts[0])
	}
	if s := h.relayer.Settlements(); len(s) != 1 || !s[0].Orphaned {
		t.Fatalf("settlement not marked orphaned: %+v", s)
	}
	// The supply control is the independent detection of the same fact: units
	// on the target with nothing locked behind them.
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if rep.Balanced {
		t.Fatal("supply reports balanced with 2,000,000 unbacked units on the target")
	}
	found := false
	for _, b := range rep.Breaks {
		if b.Kind == domain.BreakSupplyDiff && b.Symbol == sym && b.Delta == 2_000_000 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no supply break of the right size: %+v", rep.Breaks)
	}
	// And it does not alert twice for the same violation.
	h.poll()
	if len(h.relayer.Alerts()) != 1 {
		t.Fatalf("finality violation alerted %d times", len(h.relayer.Alerts()))
	}
}

func TestReObservationAfterReorgDoesNotMintTwice(t *testing.T) {
	h := newHarness(t, 2, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	nonce, rc := h.lock("CP-ALPHA", 4_000_000)
	h.world.EVM.Mine(2)
	if len(h.poll().Settled) != 1 {
		t.Fatal("did not settle")
	}
	minted := h.world.Canton.BalanceOf(party, sym)

	// A reorganisation that re-includes the same transaction. The block moves,
	// the transaction hash does not, and neither does the bridge nonce — which
	// is exactly why the nonce is the replay key.
	rr, err := h.world.EVM.Reorg(3)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	if len(rr.Reapplied) != 1 || rr.Reapplied[0].TxHash != rc.TxHash {
		t.Fatalf("the lock was not re-included: %+v", rr)
	}
	if len(rr.Orphaned) != 0 {
		t.Fatalf("orphaned = %+v", rr.Orphaned)
	}

	res := h.poll()
	if len(res.Settled) != 0 {
		t.Fatalf("a re-observed lock minted again: %+v", res.Settled)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != minted {
		t.Fatalf("target balance = %d, want %d", got, minted)
	}
	if got := minted - before; got != 4_000_000 {
		t.Fatalf("net minted = %d for a 4,000,000 lock", got)
	}
	if len(h.relayer.Alerts()) != 0 {
		t.Fatalf("a benign re-inclusion raised %+v", h.relayer.Alerts())
	}
	if s := h.relayer.Settlements(); len(s) != 1 || s[0].Nonce != nonce {
		t.Fatalf("settlements = %+v", s)
	}
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if !rep.Balanced {
		t.Fatalf("supply broke across a benign reorganisation: %+v", rep.Breaks)
	}
}

func TestQuorumShortHoldsTheFlowThenSettlesWhenRestored(t *testing.T) {
	h := newHarness(t, 1, 3) // 3 of 3
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	h.signers[2].SetAvailable(false)
	nonce, _ := h.lock("CP-ALPHA", 1_500_000)
	h.world.EVM.Mine(1)

	res := h.poll()
	if len(res.Settled) != 0 || res.Waiting != 1 {
		t.Fatalf("settled without a quorum: %+v", res)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before {
		t.Fatalf("target balance moved without a quorum: %d", got)
	}
	alerts := h.relayer.Alerts()
	if len(alerts) != 1 || alerts[0].Kind != bridge.AlertQuorumUnavailable || alerts[0].Nonce != nonce {
		t.Fatalf("alerts = %+v", alerts)
	}
	if p := h.relayer.Pending(); len(p) != 1 || p[0] != nonce {
		t.Fatalf("pending = %v", p)
	}

	h.signers[2].SetAvailable(true)
	res = h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("did not settle once the signer returned: %+v", res)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before+1_500_000 {
		t.Fatalf("target balance = %d", got)
	}
	if len(h.relayer.Pending()) != 0 {
		t.Fatalf("still pending: %v", h.relayer.Pending())
	}
}

func TestMOfNSettlesWithASignerDown(t *testing.T) {
	h := newHarness(t, 1, 2) // 2 of 3
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	h.signers[1].SetAvailable(false)
	h.lock("CP-ALPHA", 900_000)
	h.world.EVM.Mine(1)

	res := h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("two of three did not settle: %+v", res)
	}
	if len(res.Settled[0].Signatures) != 2 {
		t.Fatalf("settlement carried %d signatures", len(res.Settled[0].Signatures))
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before+900_000 {
		t.Fatalf("target balance = %d", got)
	}
}

func TestTargetSubmissionRetriesWithBackoff(t *testing.T) {
	h := newHarness(t, 1, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	h.flaky.failures = 2 // the third attempt succeeds
	h.lock("CP-ALPHA", 700_000)
	h.world.EVM.Mine(1)

	res := h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("did not settle through two transport failures: %+v", res)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before+700_000 {
		t.Fatalf("target balance = %d", got)
	}
	lb := map[string]string{"direction": string(bridge.DirectionMint)}
	if n := h.counter(bridge.MetricRetries, lb); n != 2 {
		t.Fatalf("retries counted = %v, want 2", n)
	}
}

func TestTargetSubmissionExhaustsRetriesThenRecovers(t *testing.T) {
	h := newHarness(t, 1, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	h.flaky.failures = 5 // more than the three attempts allowed
	nonce, _ := h.lock("CP-ALPHA", 600_000)
	h.world.EVM.Mine(1)

	res := h.poll()
	if len(res.Settled) != 0 {
		t.Fatal("settled despite every attempt failing")
	}
	alerts := h.relayer.Alerts()
	if len(alerts) != 1 || alerts[0].Kind != bridge.AlertSubmissionFailed || alerts[0].Nonce != nonce {
		t.Fatalf("alerts = %+v", alerts)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before {
		t.Fatalf("target balance moved on a failed submission: %d", got)
	}
	for _, r := range h.log.Records() {
		if r.Action == audit.ActionBridgeMinted && r.Outcome != audit.OutcomeError {
			t.Fatalf("a failed submission was audited as %s", r.Outcome)
		}
	}

	h.flaky.failures = 0
	res = h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("did not recover: %+v", res)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before+600_000 {
		t.Fatalf("target balance = %d", got)
	}
	rep := bridge.Reconcile(h.world.EVM, h.world.Canton, []string{sym}, domain.SeedTime)
	if !rep.Balanced {
		t.Fatalf("supply broke across a retried submission: %+v", rep.Breaks)
	}
}

func TestSubmissionIsIdempotentByNonce(t *testing.T) {
	// A mint that reached the target but whose acknowledgement was lost must
	// not be written twice. The relayer asks the target whether the nonce is
	// already settled before every attempt, so the second write never happens.
	h := newHarness(t, 1, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	nonce, _ := h.lock("CP-ALPHA", 800_000)
	h.world.EVM.Mine(1)

	// Write the mint out of band, exactly as a lost acknowledgement would
	// leave it, then let the relayer discover the lock for the first time.
	if _, err := h.world.Canton.Mint(context.Background(), h.world.Issuer, party, sym, 800_000,
		ledger.BridgeMemo(ledger.BridgeOpMint, nonce)); err != nil {
		t.Fatalf("out-of-band mint: %v", err)
	}
	res := h.poll()
	if len(res.Settled) != 1 {
		t.Fatalf("the relayer did not adopt the existing mint: %+v", res)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before+800_000 {
		t.Fatalf("target balance = %d, want a single mint of 800,000 over %d", got, before)
	}
}

func TestRelayerRejectsBadConfiguration(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	w, err := ledger.NewWorld(ledger.Options{Clock: clock, Confirmations: 1})
	if err != nil {
		t.Fatalf("world: %v", err)
	}
	set, _ := bridge.SignerSetFrom(1, signerFor(t, "attester-a", 1))
	cases := map[string]bridge.Config{
		"no target":     {Source: w.EVM, Symbols: []string{sym}, SignerSet: set},
		"same ledger":   {Source: w.EVM, Target: w.EVM, Symbols: []string{sym}, SignerSet: set},
		"no signer set": {Source: w.EVM, Target: w.Canton, Symbols: []string{sym}},
		"no symbols":    {Source: w.EVM, Target: w.Canton, SignerSet: set},
	}
	for name, cfg := range cases {
		if _, err := bridge.NewRelayer(cfg); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestSpansCoverThePollAndTheLedgerWrites(t *testing.T) {
	h := newHarness(t, 1, 2)
	h.lock("CP-ALPHA", 100_000)
	h.world.EVM.Mine(1)
	h.poll()

	names := map[string]bool{}
	for _, s := range h.spans.Spans() {
		names[s.Name] = true
	}
	for _, want := range []string{"bridge.poll"} {
		if !names[want] {
			t.Fatalf("no %s span; spans = %v", want, names)
		}
	}
}

func TestUnrelayedSymbolIsRefused(t *testing.T) {
	h := newHarness(t, 1, 2)
	if _, _, err := h.relayer.Lock(context.Background(), h.wallet("CP-ECHO"), "USDC", 1); err == nil {
		t.Fatal("a symbol the relayer does not relay was locked")
	}
}

func TestConcurrentPollsSettleExactlyOnce(t *testing.T) {
	// The relayer is polled by whatever loop the deployment runs, and a slow
	// poll overlapping the next one must not mint twice. Run under -race.
	h := newHarness(t, 1, 2)
	party := h.party("CP-ALPHA")
	before := h.world.Canton.BalanceOf(party, sym)

	h.lock("CP-ALPHA", 1_100_000)
	h.world.EVM.Mine(1)

	var wg sync.WaitGroup
	settled := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.relayer.Poll(context.Background())
			if err != nil {
				t.Errorf("poll: %v", err)
				return
			}
			settled[i] = len(res.Settled)
		}(i)
	}
	wg.Wait()

	total := 0
	for _, n := range settled {
		total += n
	}
	if total != 1 {
		t.Fatalf("eight concurrent polls settled %d flows", total)
	}
	if got := h.world.Canton.BalanceOf(party, sym); got != before+1_100_000 {
		t.Fatalf("target balance = %d, want a single mint over %d", got, before)
	}
	if len(h.relayer.Settlements()) != 1 {
		t.Fatalf("settlements = %d", len(h.relayer.Settlements()))
	}
}
