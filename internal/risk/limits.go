package risk

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// ErrNoSuchBreach is returned when a workflow transition names a breach the
// register is not holding — either because it never existed or because it has
// been remediated and left the working set.
var ErrNoSuchBreach = errors.New("risk: no such breach")

// BreachEvent is one transition in a breach's life. The register keeps the
// sequence rather than only the current status, because "when did we know, and
// who said it was acceptable" is the question a limit breach is eventually
// asked, and a status field cannot answer it.
type BreachEvent struct {
	Seq      uint64                `json:"seq"`
	BreachID string                `json:"breach_id"`
	From     domain.BreachStatus   `json:"from,omitempty"`
	To       domain.BreachStatus   `json:"to"`
	Severity domain.BreachSeverity `json:"severity"`
	At       time.Time             `json:"at"`
	Who      string                `json:"who,omitempty"`
	Note     string                `json:"note,omitempty"`
}

// BreachRegister holds the breaches the desk is currently carrying and the
// workflow that closes them.
//
// It deduplicates: a limit that is still broken on the next recompute updates
// the breach it already raised rather than raising a second one, because a risk
// manager who is paged every thirty seconds for the same condition stops
// reading the pages. A breach is only audited on first detection, so the
// immutable record carries one entry per event rather than one per cycle.
//
// It is in-memory by design. The audit log is the durable record; this is the
// working set, and it is rebuilt by recomputing.
type BreachRegister struct {
	mu      sync.Mutex
	clock   obs.Clock
	log     *audit.Log
	metrics *Metrics
	actor   string
	tenant  string

	tracked map[string]domain.Breach
	history []BreachEvent
	seq     uint64
}

// RegisterOption configures a BreachRegister.
type RegisterOption func(*BreachRegister)

// WithActor sets the actor recorded against automatic detections.
func WithActor(actor string) RegisterOption {
	return func(r *BreachRegister) { r.actor = actor }
}

// WithTenant sets the tenant recorded on audit records.
func WithTenant(tenant string) RegisterOption {
	return func(r *BreachRegister) { r.tenant = tenant }
}

// NewBreachRegister builds a register. The audit log and the metrics are both
// optional; a nil log means detections are not written to the chain, which is
// the right behaviour for a dry run and the wrong behaviour for production, so
// the caller decides rather than the register.
func NewBreachRegister(clock obs.Clock, log *audit.Log, metrics *Metrics, opts ...RegisterOption) *BreachRegister {
	if clock == nil {
		clock = obs.RealClock{}
	}
	if metrics == nil {
		metrics = NewMetrics(nil)
	}
	r := &BreachRegister{
		clock: clock, log: log, metrics: metrics,
		actor: "risk-engine", tenant: "tcrp",
		tracked: map[string]domain.Breach{},
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Record files a recomputed set of breaches and returns only the ones that were
// not already being carried. A breach already open is updated in place — the
// observed figure and severity move with the market — but it is neither
// re-audited nor returned, because it is not news.
func (r *BreachRegister) Record(ctx context.Context, breaches []domain.Breach) ([]domain.Breach, error) {
	r.mu.Lock()
	now := r.clock.Now()
	var fresh []domain.Breach

	for _, b := range breaches {
		existing, ok := r.tracked[b.ID]
		if !ok {
			b.Status = domain.BreachOpen
			if b.DetectedAt.IsZero() {
				b.DetectedAt = now
			}
			r.tracked[b.ID] = b
			r.appendEvent(BreachEvent{
				BreachID: b.ID, To: domain.BreachOpen, Severity: b.Severity, At: b.DetectedAt,
				Who: r.actor, Note: "detected",
			})
			fresh = append(fresh, b)
			continue
		}
		// Carry the workflow state forward; only the measurement is refreshed.
		worse := breachRank[b.Severity] > breachRank[existing.Severity]
		existing.Observed = b.Observed
		existing.ObservedPc = b.ObservedPc
		existing.Limit = b.Limit
		existing.LimitPc = b.LimitPc
		existing.Severity = b.Severity
		existing.AsOf = b.AsOf
		r.tracked[b.ID] = existing
		if worse {
			r.appendEvent(BreachEvent{
				BreachID: b.ID, From: existing.Status, To: existing.Status,
				Severity: b.Severity, At: now, Who: r.actor, Note: "severity escalated",
			})
		}
	}
	open := len(r.tracked)
	r.mu.Unlock()

	r.metrics.OpenBreaches.Set(float64(open))
	r.metrics.BreachesRaised.Add(float64(len(fresh)))

	sortBreaches(fresh)
	if err := r.auditDetections(ctx, fresh); err != nil {
		return fresh, err
	}
	return fresh, nil
}

// auditDetections writes one ActionLimitBreached record per newly raised
// breach. A nil log is not an error: the caller chose not to have one.
func (r *BreachRegister) auditDetections(ctx context.Context, fresh []domain.Breach) error {
	if r.log == nil || len(fresh) == 0 {
		return nil
	}
	traceID, spanID := "", ""
	if sp := obs.SpanFromContext(ctx); sp != nil {
		sc := sp.Context()
		traceID, spanID = sc.TraceID, sc.SpanID
	}
	for _, b := range fresh {
		details := map[string]any{
			"limit_id": b.LimitID,
			"kind":     string(b.Kind),
			"scope":    b.Scope,
			"severity": string(b.Severity),
			"as_of":    b.AsOf.UTC().Format(time.RFC3339Nano),
		}
		if !b.Observed.IsZero() {
			details["observed"] = b.Observed.String()
			details["limit"] = b.Limit.String()
		}
		if b.ObservedPc.Den != 0 {
			details["observed_pct"] = b.ObservedPc.String()
			details["limit_pct"] = b.LimitPc.String()
		}
		tid := traceID
		if tid == "" {
			tid = b.TraceID
		}
		if _, err := r.log.Append(audit.Record{
			Time: b.DetectedAt, Actor: r.actor, Tenant: r.tenant,
			Action: audit.ActionLimitBreached, Subject: b.ID, Outcome: audit.OutcomeOK,
			TraceID: tid, SpanID: spanID, Details: details,
		}); err != nil {
			return fmt.Errorf("risk: auditing breach %s: %w", b.ID, err)
		}
	}
	return nil
}

// Acknowledge records that a named human has seen the breach. It does not
// change the number; it changes who is answerable for it.
func (r *BreachRegister) Acknowledge(id, who, note string) (domain.Breach, error) {
	return r.transition(id, domain.BreachAcknowledged, who, note, false)
}

// Remediate closes the breach: the underlying condition is gone. The breach
// leaves the working set, so if the limit breaks again it is raised as a new
// detection with a new audit record.
func (r *BreachRegister) Remediate(id, who, note string) (domain.Breach, error) {
	return r.transition(id, domain.BreachRemediated, who, note, true)
}

// Accept records a decision to carry the breach. An accepted breach stays open:
// acceptance is a decision to live with an exposure, not a fix, and the gauge
// should keep showing it.
func (r *BreachRegister) Accept(id, who, note string) (domain.Breach, error) {
	return r.transition(id, domain.BreachAccepted, who, note, false)
}

func (r *BreachRegister) transition(id string, to domain.BreachStatus, who, note string, closing bool) (domain.Breach, error) {
	r.mu.Lock()
	b, ok := r.tracked[id]
	if !ok {
		r.mu.Unlock()
		return domain.Breach{}, fmt.Errorf("%w: %s", ErrNoSuchBreach, id)
	}
	from := b.Status
	b.Status = to
	if note != "" {
		b.Notes = append(append([]string(nil), b.Notes...), note)
	}
	if closing {
		delete(r.tracked, id)
	} else {
		r.tracked[id] = b
	}
	r.appendEvent(BreachEvent{
		BreachID: id, From: from, To: to, Severity: b.Severity,
		At: r.clock.Now(), Who: who, Note: note,
	})
	open := len(r.tracked)
	r.mu.Unlock()

	r.metrics.OpenBreaches.Set(float64(open))
	return b, nil
}

// appendEvent records a transition. The caller holds the lock.
func (r *BreachRegister) appendEvent(ev BreachEvent) {
	r.seq++
	ev.Seq = r.seq
	r.history = append(r.history, ev)
}

// Open returns the breaches currently being carried, worst first. An accepted
// breach is included; a remediated one is not.
func (r *BreachRegister) Open() []domain.Breach {
	r.mu.Lock()
	out := make([]domain.Breach, 0, len(r.tracked))
	for _, b := range r.tracked {
		out = append(out, b)
	}
	r.mu.Unlock()
	sortBreaches(out)
	return out
}

// Get returns one tracked breach.
func (r *BreachRegister) Get(id string) (domain.Breach, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.tracked[id]
	return b, ok
}

// History returns every transition in sequence order, including those of
// breaches that have since been remediated.
func (r *BreachRegister) History() []BreachEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]BreachEvent, len(r.history))
	copy(out, r.history)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Len returns the number of breaches currently carried.
func (r *BreachRegister) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.tracked)
}
