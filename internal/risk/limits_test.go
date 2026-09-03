package risk

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

func TestBreachRegisterDeduplicates(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	log := audit.New(clock)
	reg := NewBreachRegister(clock, log, NewMetrics(obs.NewRegistry()))

	e, snap := newTestEngine(t)
	ctx := context.Background()
	breaches, err := e.CheckLimits(ctx, snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	if len(breaches) != 2 {
		t.Fatalf("fixture drifted: %d breaches", len(breaches))
	}

	fresh, err := reg.Record(ctx, breaches)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(fresh) != 2 {
		t.Fatalf("first cycle raised %d, want 2", len(fresh))
	}
	if got := log.Len(); got != 2 {
		t.Fatalf("audit records = %d, want 2", got)
	}

	// Five more recomputes of the same still-broken limits raise nothing new
	// and write nothing further to the chain.
	for i := 0; i < 5; i++ {
		fresh, err = reg.Record(ctx, breaches)
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if len(fresh) != 0 {
			t.Fatalf("cycle %d re-raised %d breaches", i+2, len(fresh))
		}
	}
	if got := log.Len(); got != 2 {
		t.Errorf("audit records = %d after six cycles, want 2", got)
	}
	if got := reg.Len(); got != 2 {
		t.Errorf("open breaches = %d, want 2", got)
	}
	if err := log.Verify(); err != nil {
		t.Errorf("audit chain: %v", err)
	}
}

func TestBreachRegisterAuditRecord(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	log := audit.New(clock)
	reg := NewBreachRegister(clock, log, nil, WithActor("risk-engine"), WithTenant("acme"))

	e, snap := newTestEngine(t)
	breaches, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	if _, err := reg.Record(context.Background(), breaches); err != nil {
		t.Fatalf("Record: %v", err)
	}

	recs := log.Find(func(r audit.Record) bool { return r.Action == audit.ActionLimitBreached })
	if len(recs) != 2 {
		t.Fatalf("limit-breached records = %d, want 2", len(recs))
	}
	for _, r := range recs {
		if r.Tenant != "acme" || r.Actor != "risk-engine" {
			t.Errorf("record actor/tenant = %s/%s", r.Actor, r.Tenant)
		}
		if r.Outcome != audit.OutcomeOK {
			t.Errorf("outcome = %s", r.Outcome)
		}
		if r.Details["limit_id"] == nil || r.Details["severity"] == nil {
			t.Errorf("details missing limit_id/severity: %v", r.Details)
		}
		if !r.Time.Equal(domain.SeedTime) {
			t.Errorf("record time = %s, want the engine clock", r.Time)
		}
	}
}

func TestBreachRegisterNilAuditLog(t *testing.T) {
	reg := NewBreachRegister(obs.NewManualClock(domain.SeedTime), nil, nil)
	e, snap := newTestEngine(t)
	breaches, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	fresh, err := reg.Record(context.Background(), breaches)
	if err != nil {
		t.Fatalf("Record with a nil audit log: %v", err)
	}
	if len(fresh) != len(breaches) {
		t.Errorf("raised %d, want %d", len(fresh), len(breaches))
	}
}

func TestBreachRegisterWorkflow(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	metrics := NewMetrics(obs.NewRegistry())
	reg := NewBreachRegister(clock, audit.New(clock), metrics)

	e, snap := newTestEngine(t)
	ctx := context.Background()
	breaches, err := e.CheckLimits(ctx, snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}
	if _, err := reg.Record(ctx, breaches); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := metrics.OpenBreaches.Value(); got != 2 {
		t.Errorf("open gauge = %v, want 2", got)
	}

	const id = "BRC-LIM-CP-EXP-CP-DELTA"

	b, err := reg.Acknowledge(id, "ana@risk", "chasing the collateral desk")
	if err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if b.Status != domain.BreachAcknowledged {
		t.Errorf("status = %s, want acknowledged", b.Status)
	}
	if len(b.Notes) != 1 {
		t.Errorf("notes = %v, want one", b.Notes)
	}
	if got := metrics.OpenBreaches.Value(); got != 2 {
		t.Errorf("acknowledging closed a breach: gauge = %v", got)
	}

	// Acceptance keeps the breach open, because accepting a risk is not fixing it.
	if _, err := reg.Accept(id, "head-of-risk", "within appetite until settlement"); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if got, ok := reg.Get(id); !ok || got.Status != domain.BreachAccepted {
		t.Errorf("after Accept: %+v ok=%v", got, ok)
	}
	if got := metrics.OpenBreaches.Value(); got != 2 {
		t.Errorf("accepting closed a breach: gauge = %v", got)
	}

	clock.Advance(time.Hour)
	if _, err := reg.Remediate(id, "ops", "collateral received"); err != nil {
		t.Fatalf("Remediate: %v", err)
	}
	if _, ok := reg.Get(id); ok {
		t.Error("a remediated breach is still tracked")
	}
	if got := metrics.OpenBreaches.Value(); got != 1 {
		t.Errorf("open gauge = %v, want 1", got)
	}

	// A remediated condition that comes back is news again.
	fresh, err := reg.Record(ctx, breaches)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(fresh) != 1 || fresh[0].ID != id {
		t.Fatalf("re-detection = %+v, want just %s", fresh, id)
	}

	hist := reg.History()
	want := []struct {
		id string
		to domain.BreachStatus
	}{
		{"BRC-LIM-CP-EXP-CP-DELTA", domain.BreachOpen},
		{"BRC-LIM-ISSUER-CONC-SELF", domain.BreachOpen},
		{id, domain.BreachAcknowledged},
		{id, domain.BreachAccepted},
		{id, domain.BreachRemediated},
		{id, domain.BreachOpen},
	}
	if len(hist) != len(want) {
		t.Fatalf("history = %d events, want %d: %+v", len(hist), len(want), hist)
	}
	for i, w := range want {
		if hist[i].Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d", i, hist[i].Seq)
		}
		if hist[i].BreachID != w.id || hist[i].To != w.to {
			t.Errorf("event %d = %s->%s, want %s->%s", i, hist[i].BreachID, hist[i].To, w.id, w.to)
		}
	}
}

func TestBreachRegisterUnknownID(t *testing.T) {
	reg := NewBreachRegister(obs.NewManualClock(domain.SeedTime), nil, nil)
	for _, fn := range []func() (domain.Breach, error){
		func() (domain.Breach, error) { return reg.Acknowledge("nope", "who", "") },
		func() (domain.Breach, error) { return reg.Remediate("nope", "who", "") },
		func() (domain.Breach, error) { return reg.Accept("nope", "who", "") },
	} {
		if _, err := fn(); !errors.Is(err, ErrNoSuchBreach) {
			t.Errorf("err = %v, want ErrNoSuchBreach", err)
		}
	}
}

// TestBreachRegisterEscalation checks that a worsening breach updates in place
// and leaves a trail, without raising a second breach or a second audit record.
func TestBreachRegisterEscalation(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	log := audit.New(clock)
	reg := NewBreachRegister(clock, log, nil)
	ctx := context.Background()

	warn := domain.Breach{
		ID: "BRC-X", LimitID: "LIM-X", Kind: domain.LimitCounterparty, Scope: "CP-DELTA",
		Observed: money.FromInt(money.USD, 8), Limit: money.FromInt(money.USD, 10),
		Severity: domain.BreachWarn, AsOf: domain.SeedTime,
	}
	if fresh, err := reg.Record(ctx, []domain.Breach{warn}); err != nil || len(fresh) != 1 {
		t.Fatalf("first record: %v, %d fresh", err, len(fresh))
	}

	worse := warn
	worse.Observed = money.FromInt(money.USD, 20)
	worse.Severity = domain.BreachCritical
	fresh, err := reg.Record(ctx, []domain.Breach{worse})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(fresh) != 0 {
		t.Errorf("escalation raised %d new breaches, want 0", len(fresh))
	}
	if log.Len() != 1 {
		t.Errorf("audit records = %d, want 1", log.Len())
	}
	got, ok := reg.Get("BRC-X")
	if !ok {
		t.Fatal("breach vanished")
	}
	if got.Severity != domain.BreachCritical {
		t.Errorf("severity = %s, want critical", got.Severity)
	}
	if !got.Observed.Equal(money.FromInt(money.USD, 20)) {
		t.Errorf("observed = %s, want 20", got.Observed)
	}
	hist := reg.History()
	if len(hist) != 2 || hist[1].Note != "severity escalated" {
		t.Errorf("history = %+v, want a detection then an escalation", hist)
	}
}

// TestBreachRegisterConcurrentRecord exercises the register the way the
// recomputer does: several cycles landing at once. Correctness here is that
// exactly one detection wins, which -race and the audit length together prove.
func TestBreachRegisterConcurrentRecord(t *testing.T) {
	clock := obs.NewManualClock(domain.SeedTime)
	log := audit.New(clock)
	reg := NewBreachRegister(clock, log, nil)

	e, snap := newTestEngine(t)
	breaches, err := e.CheckLimits(context.Background(), snap)
	if err != nil {
		t.Fatalf("CheckLimits: %v", err)
	}

	var wg sync.WaitGroup
	total := make([]int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fresh, err := reg.Record(context.Background(), breaches)
			if err != nil {
				t.Errorf("Record: %v", err)
			}
			total[i] = len(fresh)
		}(i)
	}
	wg.Wait()

	raised := 0
	for _, n := range total {
		raised += n
	}
	if raised != len(breaches) {
		t.Errorf("raised %d across 8 concurrent cycles, want %d", raised, len(breaches))
	}
	if log.Len() != len(breaches) {
		t.Errorf("audit records = %d, want %d", log.Len(), len(breaches))
	}
	if err := log.Verify(); err != nil {
		t.Errorf("audit chain: %v", err)
	}
}
