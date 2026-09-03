package lake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by the reconciliation workflow.
var (
	// ErrNoSuchBreak is returned when a workflow transition names a break the
	// register does not hold.
	ErrNoSuchBreak = errors.New("lake: no such break")
	// ErrBreakClosed is returned when a resolved or suppressed break is
	// transitioned again. Reopening is a deliberate act, not a retry.
	ErrBreakClosed = errors.New("lake: break is already closed")
)

// Instruction is one settlement the book of record holds. It is the golden
// source's side of a reconciliation: the institution's own record of what it
// believes settled, which is not the same thing as what a chain says settled.
type Instruction struct {
	EventID    string           `json:"event_id"`
	Ledger     domain.Ledger    `json:"ledger"`
	Kind       domain.EventKind `json:"kind"`
	Symbol     string           `json:"symbol"`
	From       string           `json:"from,omitempty"`
	To         string           `json:"to,omitempty"`
	Quantity   int64            `json:"quantity"`
	OccurredAt time.Time        `json:"occurred_at"`
	RecordedAt time.Time        `json:"recorded_at"`
	Reference  string           `json:"reference,omitempty"`
}

func (i Instruction) event() domain.LedgerEvent {
	return domain.LedgerEvent{
		Ledger: i.Ledger, Kind: i.Kind, Symbol: i.Symbol,
		From: i.From, To: i.To, Quantity: i.Quantity,
		OccurredAt: i.OccurredAt, ObservedAt: i.RecordedAt,
	}
}

// Book is the golden source: the institution's off-chain record of what
// settled. In production it is an extract from another system; here it is
// replayed from the same event stream with a stated set of divergences, which
// is what gives the controls something to find.
type Book struct {
	Source       string        `json:"source"`
	Instructions []Instruction `json:"instructions"`
}

// Divergence states one way the book differs from the chain.
type Divergence struct {
	// EventID names the chain event this is about.
	EventID string
	// Missing true means the book never received the instruction at all.
	Missing bool
	// Quantity, when non-zero, is the size the book recorded instead.
	Quantity int64
	// Note explains the divergence for the break's audit trail.
	Note string
}

// BookFromEvents replays a chain event stream into a book of record, applying
// the stated divergences. The reconciler knows nothing about them: it sees two
// sources that both claim to be true and reports where they are not.
func BookFromEvents(events []domain.LedgerEvent, divergences []Divergence) Book {
	byID := make(map[string]Divergence, len(divergences))
	for _, d := range divergences {
		byID[d.EventID] = d
	}
	ordered := append([]domain.LedgerEvent(nil), events...)
	domain.SortEvents(ordered)

	b := Book{Source: "book-of-record"}
	for _, e := range ordered {
		d, adjusted := byID[e.ID()]
		if adjusted && d.Missing {
			continue
		}
		inst := Instruction{
			EventID: e.ID(), Ledger: e.Ledger, Kind: e.Kind, Symbol: e.Symbol,
			From: e.From, To: e.To, Quantity: e.Quantity,
			OccurredAt: e.OccurredAt, RecordedAt: e.ObservedAt, Reference: e.Memo,
		}
		if adjusted && d.Quantity != 0 {
			inst.Quantity = d.Quantity
			if d.Note != "" {
				inst.Reference = d.Note
			}
		}
		b.Instructions = append(b.Instructions, inst)
	}
	return b
}

// Index returns the book's instructions keyed by the chain event id.
func (b Book) Index() map[string]Instruction {
	out := make(map[string]Instruction, len(b.Instructions))
	for _, i := range b.Instructions {
		out[i.EventID] = i
	}
	return out
}

// PositionsAt folds the book's instructions into holdings that were true at
// validAt as the book had recorded them by knownAt.
func (b Book) PositionsAt(validAt, knownAt time.Time) []domain.Position {
	type acc struct {
		qty   int64
		from  time.Time
		tx    time.Time
		block uint64
	}
	totals := map[string]*acc{}
	meta := map[string]effect{}
	for _, i := range b.Instructions {
		if !validAt.IsZero() && i.OccurredAt.After(validAt) {
			continue
		}
		if !knownAt.IsZero() && i.RecordedAt.After(knownAt) {
			continue
		}
		for _, eff := range eventEffects(i.event()) {
			key := PositionKey(eff.Counterparty, eff.Symbol, eff.Ledger)
			a, ok := totals[key]
			if !ok {
				a = &acc{from: i.OccurredAt, tx: i.RecordedAt}
				totals[key] = a
				meta[key] = eff
			}
			a.qty += eff.Delta
			if i.OccurredAt.Before(a.from) {
				a.from = i.OccurredAt
			}
			if i.RecordedAt.After(a.tx) {
				a.tx = i.RecordedAt
			}
		}
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]domain.Position, 0, len(keys))
	for _, k := range keys {
		eff, a := meta[k], totals[k]
		out = append(out, domain.Position{
			ID:           "BOOK-" + shortHash(k),
			Counterparty: eff.Counterparty,
			Symbol:       eff.Symbol,
			Ledger:       eff.Ledger,
			Quantity:     a.qty,
			Pledged:      true,
			ValidFrom:    a.from,
			TxFrom:       a.tx,
			Source:       "book-of-record",
		})
	}
	domain.SortPositions(out)
	return out
}

// SeedDivergences returns the divergences the seeded dataset plants, located by
// the memos domain.seedEvents writes on them.
//
// Two of the seed's three anomalies live here because they are properties of
// the book, not of the chain: a settlement instruction the book never received,
// and a partial fill the book sized differently. The third — an event observed
// long after it occurred — is a property of the chain observation itself and
// needs no divergence; the reconciler finds it from the observation lag.
func SeedDivergences(s *domain.Seed) []Divergence {
	var out []Divergence
	for _, e := range s.Events {
		switch {
		case strings.HasPrefix(e.Memo, "settlement instruction"):
			out = append(out, Divergence{EventID: e.ID(), Missing: true, Note: e.Memo})
		case strings.HasPrefix(e.Memo, "partial fill"):
			out = append(out, Divergence{EventID: e.ID(), Quantity: 1_000_000, Note: e.Memo})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EventID < out[j].EventID })
	return out
}

// ReconPolicy sets what counts as a break worth a human's attention and how bad
// each one is. Every threshold is explicit because "we suppressed it" has to be
// a decision on the record rather than a filter somebody added to a dashboard.
type ReconPolicy struct {
	// MaterialityQty is the absolute quantity below which a difference is
	// recorded but suppressed rather than opened.
	MaterialityQty int64
	// HighQty and CriticalQty escalate a difference by size.
	HighQty     int64
	CriticalQty int64
	// StaleAfter is the observation lag beyond which an event is a break in its
	// own right, whatever the quantities agree on.
	StaleAfter time.Duration
}

// DefaultReconPolicy is the platform's default: a thousand units is noise, a
// hundred thousand is escalated, a million is critical, and fifteen minutes of
// observation lag is a break.
var DefaultReconPolicy = ReconPolicy{
	MaterialityQty: 1_000,
	HighQty:        100_000,
	CriticalQty:    1_000_000,
	StaleAfter:     15 * time.Minute,
}

func (p ReconPolicy) resolve() ReconPolicy {
	if p.MaterialityQty <= 0 {
		p.MaterialityQty = DefaultReconPolicy.MaterialityQty
	}
	if p.HighQty <= 0 {
		p.HighQty = DefaultReconPolicy.HighQty
	}
	if p.CriticalQty <= 0 {
		p.CriticalQty = DefaultReconPolicy.CriticalQty
	}
	if p.StaleAfter <= 0 {
		p.StaleAfter = DefaultReconPolicy.StaleAfter
	}
	return p
}

// ReconInput is one reconciliation run's two sides plus the registry that says
// which holders are known.
type ReconInput struct {
	ValidAt        time.Time
	KnownAt        time.Time
	Events         []domain.LedgerEvent
	Ledger         []domain.Position
	Book           Book
	Counterparties []domain.Counterparty
}

// ReconResult is what one run found.
type ReconResult struct {
	ValidAt time.Time `json:"valid_at"`
	KnownAt time.Time `json:"known_at"`
	// Compared is the number of chain events reconciled against the book.
	Compared int `json:"compared"`
	// Matched is the number that agreed on every axis.
	Matched int `json:"matched"`
	// Breaks are the differences found, worst first.
	Breaks []domain.Break `json:"breaks"`
	// Suppressed is the number of differences below the materiality threshold.
	Suppressed int `json:"suppressed"`
}

// Open returns the breaks that are neither resolved nor suppressed.
func (r ReconResult) Open() []domain.Break {
	var out []domain.Break
	for _, b := range r.Breaks {
		if b.Status == domain.BreakOpen || b.Status == domain.BreakAcknowledged {
			out = append(out, b)
		}
	}
	return out
}

// Reconciler compares the ledger view against the book of record and drives the
// investigation workflow for what it finds.
//
// It reconciles at two levels. First instruction by instruction, which is what
// a settlement operations team actually does: a chain event with no matching
// book instruction, a size the two sides disagree on, an observation that
// arrived too late to have been in the cut. Then position by position, but only
// for slots no instruction-level break already explains — because a position
// difference that no instruction accounts for is a different and worse finding
// than the same difference with a cause attached to it.
type Reconciler struct {
	opts     Options
	policy   ReconPolicy
	register *Register
}

// NewReconciler builds a reconciler. A zero ReconPolicy takes the defaults.
func NewReconciler(opts Options, policy ReconPolicy) *Reconciler {
	opts = opts.resolve()
	return &Reconciler{opts: opts, policy: policy.resolve(), register: NewRegister(opts.Clock)}
}

// Register exposes the break register so a caller can drive the workflow.
func (r *Reconciler) Register() *Register { return r.register }

// Policy returns the thresholds in force.
func (r *Reconciler) Policy() ReconPolicy { return r.policy }

// Reconcile runs the controls and opens every break it finds in the register.
// A break already in the register keeps whatever a human did to it, so a run
// every minute does not undo an acknowledgement made between two of them.
func (r *Reconciler) Reconcile(ctx context.Context, in ReconInput) (ReconResult, error) {
	if err := ctx.Err(); err != nil {
		return ReconResult{}, err
	}
	_, sp := r.opts.start(ctx, "lake.recon", obs.KindInternal)
	if sp != nil {
		defer sp.End()
	}
	now := r.opts.Clock.Now()
	res := ReconResult{ValidAt: in.ValidAt, KnownAt: in.KnownAt}

	events := visibleEvents(in.Events, in.ValidAt, in.KnownAt)
	bookIdx := in.Book.Index()
	known := make(map[string]bool, len(in.Counterparties))
	for _, cp := range in.Counterparties {
		known[cp.ID] = true
	}

	var found []domain.Break
	explained := map[string]bool{}
	seenOnChain := make(map[string]bool, len(events))

	for _, ev := range events {
		seenOnChain[ev.ID()] = true
		res.Compared++
		inst, inBook := bookIdx[ev.ID()]
		agreed := true
		switch {
		case !inBook:
			agreed = false
			found = append(found, r.breakFor(domain.Break{
				Kind:         domain.BreakMissingInBook,
				Counterparty: primaryParty(ev),
				Symbol:       ev.Symbol,
				Ledger:       ev.Ledger,
				LedgerQty:    ev.Quantity,
				BookQty:      0,
				Delta:        ev.Quantity,
				AsOf:         in.ValidAt,
			}, ev.ID(), now, fmt.Sprintf("chain event %s has no instruction in the book", ev.ID())))
			markExplained(explained, ev)
		case inst.Quantity != ev.Quantity:
			agreed = false
			found = append(found, r.breakFor(domain.Break{
				Kind:         domain.BreakQuantityDiff,
				Counterparty: primaryParty(ev),
				Symbol:       ev.Symbol,
				Ledger:       ev.Ledger,
				LedgerQty:    ev.Quantity,
				BookQty:      inst.Quantity,
				Delta:        ev.Quantity - inst.Quantity,
				AsOf:         in.ValidAt,
			}, ev.ID(), now, fmt.Sprintf("chain %d against book %d on %s", ev.Quantity, inst.Quantity, ev.ID())))
			markExplained(explained, ev)
		}
		if lag := ev.ObservedAt.Sub(ev.OccurredAt); lag > r.policy.StaleAfter {
			agreed = false
			b := r.breakFor(domain.Break{
				Kind:         domain.BreakStale,
				Counterparty: primaryParty(ev),
				Symbol:       ev.Symbol,
				Ledger:       ev.Ledger,
				LedgerQty:    ev.Quantity,
				BookQty:      ev.Quantity,
				AsOf:         in.ValidAt,
			}, ev.ID(), now, fmt.Sprintf("%s occurred at %s and was observed at %s, a lag of %s",
				ev.ID(), ev.OccurredAt.UTC().Format(time.RFC3339), ev.ObservedAt.UTC().Format(time.RFC3339), lag))
			b.Severity = staleSeverity(lag, r.policy.StaleAfter)
			b.Status = domain.BreakOpen
			found = append(found, b)
		}
		if agreed {
			res.Matched++
		}
	}

	for _, inst := range in.Book.Instructions {
		if !in.ValidAt.IsZero() && inst.OccurredAt.After(in.ValidAt) {
			continue
		}
		if !in.KnownAt.IsZero() && inst.RecordedAt.After(in.KnownAt) {
			continue
		}
		if seenOnChain[inst.EventID] {
			continue
		}
		found = append(found, r.breakFor(domain.Break{
			Kind:         domain.BreakMissingOnChain,
			Counterparty: primaryInstructionParty(inst),
			Symbol:       inst.Symbol,
			Ledger:       inst.Ledger,
			BookQty:      inst.Quantity,
			Delta:        -inst.Quantity,
			AsOf:         in.ValidAt,
		}, inst.EventID, now, fmt.Sprintf("book instruction %s never appeared on chain", inst.EventID)))
		explained[PositionKey(inst.From, inst.Symbol, inst.Ledger)] = true
		explained[PositionKey(inst.To, inst.Symbol, inst.Ledger)] = true
	}

	for _, party := range unknownParties(events, in.Book.Instructions, known) {
		found = append(found, r.breakFor(domain.Break{
			Kind:         domain.BreakUnknownHolder,
			Counterparty: party,
			AsOf:         in.ValidAt,
		}, "holder:"+party, now, fmt.Sprintf("%s moves tokens but is not in the counterparty registry", party)))
	}

	// Position sweep. Anything the instruction pass already explained is left
	// alone; what remains is a difference with no cause, which is the finding
	// worth waking somebody for.
	bookPos := in.Book.PositionsAt(in.ValidAt, in.KnownAt)
	for _, diff := range positionDiffs(in.Ledger, bookPos) {
		if explained[diff.key] {
			continue
		}
		kind := domain.BreakQuantityDiff
		switch {
		case diff.bookQty == 0:
			kind = domain.BreakMissingInBook
		case diff.ledgerQty == 0:
			kind = domain.BreakMissingOnChain
		}
		found = append(found, r.breakFor(domain.Break{
			Kind:         kind,
			Counterparty: diff.counterparty,
			Symbol:       diff.symbol,
			Ledger:       diff.ledger,
			LedgerQty:    diff.ledgerQty,
			BookQty:      diff.bookQty,
			Delta:        diff.ledgerQty - diff.bookQty,
			AsOf:         in.ValidAt,
		}, "position:"+diff.key, now, "position difference with no instruction to explain it"))
	}

	for _, b := range found {
		if b.Status == domain.BreakSuppressed {
			res.Suppressed++
		}
		r.opts.counter("tcrp_lake_recon_breaks_total", "Reconciliation breaks detected.",
			map[string]string{"kind": string(b.Kind)}).Inc()
	}
	res.Breaks = r.register.OpenAll(found)
	domain.SortBreaks(res.Breaks)
	r.opts.gauge("tcrp_lake_recon_breaks_open", "Reconciliation breaks awaiting investigation.", nil).
		Set(float64(len(r.register.Unresolved())))
	if sp != nil {
		sp.SetAttrs(map[string]any{
			"lake.recon.compared":   res.Compared,
			"lake.recon.matched":    res.Matched,
			"lake.recon.breaks":     len(res.Breaks),
			"lake.recon.suppressed": res.Suppressed,
		})
	}
	return res, nil
}

func (r *Reconciler) breakFor(b domain.Break, ref string, now time.Time, note string) domain.Break {
	b.ID = breakID(b.Kind, b.Ledger, b.Symbol, b.Counterparty, ref)
	b.DetectedAt = now
	b.Status = domain.BreakOpen
	if note != "" {
		b.Notes = []string{note}
	}
	switch {
	case b.Kind == domain.BreakUnknownHolder:
		b.Severity = "critical"
	case !b.Material(r.policy.MaterialityQty):
		b.Severity = "low"
		b.Status = domain.BreakSuppressed
		b.Resolution = fmt.Sprintf("below the materiality threshold of %d units", r.policy.MaterialityQty)
	case abs64(b.Delta) >= r.policy.CriticalQty:
		b.Severity = "critical"
	case abs64(b.Delta) >= r.policy.HighQty:
		b.Severity = "high"
	default:
		b.Severity = "medium"
	}
	return b
}

func staleSeverity(lag, threshold time.Duration) string {
	if lag > 4*threshold {
		return "high"
	}
	return "medium"
}

func primaryParty(e domain.LedgerEvent) string {
	if e.To != "" {
		return e.To
	}
	return e.From
}

func primaryInstructionParty(i Instruction) string {
	if i.To != "" {
		return i.To
	}
	return i.From
}

func markExplained(explained map[string]bool, e domain.LedgerEvent) {
	for _, eff := range eventEffects(e) {
		explained[PositionKey(eff.Counterparty, eff.Symbol, eff.Ledger)] = true
	}
}

func unknownParties(events []domain.LedgerEvent, instructions []Instruction, known map[string]bool) []string {
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !known[p] {
			seen[p] = true
		}
	}
	for _, e := range events {
		add(e.From)
		add(e.To)
	}
	for _, i := range instructions {
		add(i.From)
		add(i.To)
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

type positionDiff struct {
	key          string
	counterparty string
	symbol       string
	ledger       domain.Ledger
	ledgerQty    int64
	bookQty      int64
}

func positionDiffs(ledger, book []domain.Position) []positionDiff {
	type side struct {
		p  domain.Position
		ok bool
	}
	all := map[string]*[2]side{}
	put := func(p domain.Position, idx int) {
		s, ok := all[p.Key()]
		if !ok {
			s = &[2]side{}
			all[p.Key()] = s
		}
		s[idx] = side{p: p, ok: true}
	}
	for _, p := range ledger {
		put(p, 0)
	}
	for _, p := range book {
		put(p, 1)
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []positionDiff
	for _, k := range keys {
		s := all[k]
		var lq, bq int64
		if s[0].ok {
			lq = s[0].p.Quantity
		}
		if s[1].ok {
			bq = s[1].p.Quantity
		}
		if lq == bq {
			continue
		}
		ref := s[0].p
		if !s[0].ok {
			ref = s[1].p
		}
		out = append(out, positionDiff{
			key: k, counterparty: ref.Counterparty, symbol: ref.Symbol, ledger: ref.Ledger,
			ledgerQty: lq, bookQty: bq,
		})
	}
	return out
}

func breakID(kind domain.BreakKind, ledger domain.Ledger, symbol, cp, ref string) string {
	return "BRK-" + shortHash(strings.Join([]string{string(kind), string(ledger), symbol, cp, ref}, "|"))
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:10]
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Register is the break investigation workflow: what was found, who picked it
// up, what they decided and how long it sat there.
//
// It is separate from the reconciler because detection and investigation move
// at different speeds. A control runs every cycle and finds the same break each
// time; a human touches it once. Re-detecting a break the desk has already
// acknowledged must not silently reopen it, or the acknowledgement means
// nothing.
type Register struct {
	clock obs.Clock
	mu    sync.Mutex
	items map[string]domain.Break
	order []string
}

// NewRegister builds an empty register on a clock.
func NewRegister(clock obs.Clock) *Register {
	if clock == nil {
		clock = obs.RealClock{}
	}
	return &Register{clock: clock, items: map[string]domain.Break{}}
}

// Open records a break. A break already in the register keeps its status,
// assignee, notes and resolution; only the observed quantities and the
// detection time are refreshed, so a control that fires every minute does not
// erase what a human did between two runs.
func (r *Register) Open(b domain.Break) domain.Break {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.openLocked(b)
}

// OpenAll records several breaks and returns them as the register now holds
// them.
func (r *Register) OpenAll(bs []domain.Break) []domain.Break {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.Break, 0, len(bs))
	for _, b := range bs {
		out = append(out, r.openLocked(b))
	}
	return out
}

func (r *Register) openLocked(b domain.Break) domain.Break {
	existing, ok := r.items[b.ID]
	if !ok {
		b.AgeMinutes = 0
		r.items[b.ID] = b
		r.order = append(r.order, b.ID)
		return b
	}
	existing.LedgerQty, existing.BookQty, existing.Delta = b.LedgerQty, b.BookQty, b.Delta
	existing.Severity = b.Severity
	existing.AsOf = b.AsOf
	existing.AgeMinutes = r.clock.Now().Sub(existing.DetectedAt).Minutes()
	r.items[b.ID] = existing
	return existing
}

// Acknowledge assigns a break to somebody and records why.
func (r *Register) Acknowledge(id, who, note string) (domain.Break, error) {
	return r.transition(id, func(b *domain.Break) error {
		if b.Status == domain.BreakResolved || b.Status == domain.BreakSuppressed {
			return fmt.Errorf("%w: %s is %s", ErrBreakClosed, id, b.Status)
		}
		b.Status = domain.BreakAcknowledged
		b.Assignee = who
		b.Notes = append(b.Notes, fmt.Sprintf("%s acknowledged: %s", who, note))
		return nil
	})
}

// Resolve closes a break with a stated resolution.
func (r *Register) Resolve(id, who, resolution string) (domain.Break, error) {
	return r.transition(id, func(b *domain.Break) error {
		if b.Status == domain.BreakResolved {
			return fmt.Errorf("%w: %s", ErrBreakClosed, id)
		}
		b.Status = domain.BreakResolved
		b.Assignee = who
		b.Resolution = resolution
		b.ResolvedAt = r.clock.Now()
		b.Notes = append(b.Notes, fmt.Sprintf("%s resolved: %s", who, resolution))
		return nil
	})
}

// Suppress closes a break as not worth chasing. It takes a reason for the same
// reason the materiality threshold is explicit: suppression is a decision.
func (r *Register) Suppress(id, who, reason string) (domain.Break, error) {
	return r.transition(id, func(b *domain.Break) error {
		if b.Status == domain.BreakResolved {
			return fmt.Errorf("%w: %s", ErrBreakClosed, id)
		}
		b.Status = domain.BreakSuppressed
		b.Assignee = who
		b.Resolution = reason
		b.Notes = append(b.Notes, fmt.Sprintf("%s suppressed: %s", who, reason))
		return nil
	})
}

func (r *Register) transition(id string, fn func(*domain.Break) error) (domain.Break, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.items[id]
	if !ok {
		return domain.Break{}, fmt.Errorf("%w: %s", ErrNoSuchBreak, id)
	}
	if err := fn(&b); err != nil {
		return domain.Break{}, err
	}
	b.AgeMinutes = r.clock.Now().Sub(b.DetectedAt).Minutes()
	r.items[id] = b
	return b, nil
}

// Get returns one break with its age recomputed.
func (r *Register) Get(id string) (domain.Break, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.items[id]
	if !ok {
		return domain.Break{}, false
	}
	b.AgeMinutes = r.clock.Now().Sub(b.DetectedAt).Minutes()
	return b, true
}

// Breaks returns every break the register holds, worst first, with ages
// recomputed against the clock.
func (r *Register) Breaks() []domain.Break {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked(func(domain.Break) bool { return true })
}

// Unresolved returns the breaks still awaiting a decision.
func (r *Register) Unresolved() []domain.Break {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked(func(b domain.Break) bool {
		return b.Status == domain.BreakOpen || b.Status == domain.BreakAcknowledged
	})
}

func (r *Register) snapshotLocked(keep func(domain.Break) bool) []domain.Break {
	now := r.clock.Now()
	out := make([]domain.Break, 0, len(r.order))
	for _, id := range r.order {
		b := r.items[id]
		if !keep(b) {
			continue
		}
		b.AgeMinutes = now.Sub(b.DetectedAt).Minutes()
		out = append(out, b)
	}
	domain.SortBreaks(out)
	return out
}

// AgeBucket is one band of the break ageing report.
type AgeBucket struct {
	Label string        `json:"label"`
	Upto  time.Duration `json:"upto"`
	Count int           `json:"count"`
}

// Ageing buckets the unresolved breaks by how long they have been sitting. A
// break that ages is worse than a break that is merely large: the first says
// nobody is looking.
func (r *Register) Ageing() []AgeBucket {
	buckets := []AgeBucket{
		{Label: "under 15m", Upto: 15 * time.Minute},
		{Label: "15m to 1h", Upto: time.Hour},
		{Label: "1h to 4h", Upto: 4 * time.Hour},
		{Label: "over 4h", Upto: 0},
	}
	for _, b := range r.Unresolved() {
		age := time.Duration(b.AgeMinutes * float64(time.Minute))
		placed := false
		for i := range buckets {
			if buckets[i].Upto > 0 && age < buckets[i].Upto {
				buckets[i].Count++
				placed = true
				break
			}
		}
		if !placed {
			buckets[len(buckets)-1].Count++
		}
	}
	return buckets
}
