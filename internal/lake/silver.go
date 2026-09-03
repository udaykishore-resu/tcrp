package lake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// SilverTable is the table name the bitemporal position store uses.
const SilverTable = "silver_positions"

// DefaultLateArrival is the observation lag beyond which an event that lands
// inside an interval the store has already published a belief about is treated
// as a late arrival and its rows marked as corrections. Five minutes is well
// clear of an indexer's ordinary few seconds and well inside a risk cut.
const DefaultLateArrival = 5 * time.Minute

// SilverSchema is the bitemporal position row. Four timestamps rather than one
// is the whole design: valid time says when the holding was true, transaction
// time says when the platform believed it, and a query supplies both.
var SilverSchema = Schema{
	SchemaID: 0,
	Fields: []Field{
		{ID: 1, Name: "position_id", Type: TypeString, Required: true},
		{ID: 2, Name: "counterparty", Type: TypeString, Required: true},
		{ID: 3, Name: "symbol", Type: TypeString, Required: true},
		{ID: 4, Name: "ledger", Type: TypeString, Required: true},
		{ID: 5, Name: "quantity", Type: TypeLong, Required: true},
		{ID: 6, Name: "pledged", Type: TypeBoolean, Required: true},
		{ID: 7, Name: "valid_from", Type: TypeTimestamp, Required: true, Doc: "when the holding became true"},
		{ID: 8, Name: "valid_to", Type: TypeTimestamp, Doc: "absent while the holding is still true"},
		{ID: 9, Name: "tx_from", Type: TypeTimestamp, Required: true, Doc: "when the platform learned it"},
		{ID: 10, Name: "tx_to", Type: TypeTimestamp, Doc: "absent while this is the current belief"},
		{ID: 11, Name: "source", Type: TypeString},
		{ID: 12, Name: "block_height", Type: TypeLong},
		{ID: 13, Name: "tx_hash", Type: TypeString},
		{ID: 14, Name: "corrected", Type: TypeBoolean, Required: true, Doc: "row supersedes a belief already published"},
	},
}

// SilverSpec partitions positions by ledger and by the day the holding became
// true, which is the shape of every as-of query the risk engine asks.
var SilverSpec = PartitionSpec{
	SpecID: 0,
	Fields: []PartitionField{
		{SourceID: 4, FieldID: 1000, Name: "ledger", Transform: TransformIdentity},
		{SourceID: 7, FieldID: 1001, Name: "valid_day", Transform: TransformDay},
	},
}

// Silver is the bitemporal position store.
//
// A row is a version of one economic slot — counterparty, symbol, ledger — over
// a half-open valid interval and a half-open transaction interval. Applying an
// event never edits history: it closes the affected rows on the transaction
// axis at the event's ObservedAt and writes replacements starting there. That
// is what makes "what did we believe at 16:00" and "what was actually true at
// 16:00" two arguments to the same query rather than two systems.
//
// An event that lands inside an interval the store has already published a
// belief about splits it: the part before the event keeps the old quantity, the
// part from the event onwards takes the new one. When the event also arrived
// materially late, or arrived out of order in valid time, the replacement rows
// are marked Corrected — a retroactive correction. The superseded rows stay
// exactly where they were, closed on the transaction axis, so a query with the
// earlier knownAt still returns the earlier belief. Nothing is ever deleted.
type Silver struct {
	opts      Options
	tbl       *Table
	lateAfter time.Duration

	mu      sync.Mutex
	rows    []domain.Position
	applied map[string]bool
	nextID  int
}

// Applied reports what one call to Apply did.
type Applied struct {
	// Events is the number of events applied, excluding ones already seen.
	Events int
	// Duplicates is the number of events dropped as already applied.
	Duplicates int
	// Rows is the number of version rows written.
	Rows int
	// Corrections is the number of rows that superseded a published belief.
	Corrections int
	// SnapshotID is the snapshot the new row set was committed as, or zero
	// when nothing changed.
	SnapshotID int64
}

// NewSilver opens or creates the position store under root and rebuilds its
// row set from the table.
func NewSilver(ctx context.Context, root string, opts Options) (*Silver, error) {
	opts = opts.resolve()
	tbl, err := LoadOrCreateTable(ctx, root, SilverTable, SilverSchema, SilverSpec, opts.tableOptions())
	if err != nil {
		return nil, err
	}
	s := &Silver{opts: opts, tbl: tbl, lateAfter: DefaultLateArrival, applied: map[string]bool{}}
	if tbl.CurrentSnapshotID() != 0 {
		res, err := tbl.Scan(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range res.Rows {
			p, err := positionFromRow(row)
			if err != nil {
				return nil, err
			}
			s.rows = append(s.rows, p)
			if n := idOrdinal(p.ID); n > s.nextID {
				s.nextID = n
			}
		}
		s.sortRows()
	}
	return s, nil
}

// Table exposes the underlying table so lineage can name its snapshot.
func (s *Silver) Table() *Table { return s.tbl }

// SetLateArrivalThreshold sets the observation lag beyond which an event is
// treated as a late arrival.
func (s *Silver) SetLateArrivalThreshold(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lateAfter = d
}

// Apply folds events into the store in transaction order — the order the
// platform learned them, not the order they happened — and commits the
// resulting row set as one snapshot. Applying an event twice is a no-op.
func (s *Silver) Apply(ctx context.Context, events []domain.LedgerEvent) (Applied, error) {
	ctx, sp := s.opts.start(ctx, "lake.silver.apply", obs.KindInternal)
	if sp != nil {
		defer sp.End()
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := append([]domain.LedgerEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].ObservedAt.Equal(ordered[j].ObservedAt) {
			return ordered[i].ObservedAt.Before(ordered[j].ObservedAt)
		}
		return ordered[i].Seq < ordered[j].Seq
	})

	var out Applied
	before := len(s.rows)
	for _, ev := range ordered {
		if s.applied[ev.ID()] {
			out.Duplicates++
			continue
		}
		s.applied[ev.ID()] = true
		out.Events++
		for _, eff := range eventEffects(ev) {
			if s.applyEffect(eff, ev) {
				out.Corrections++
			}
		}
	}
	out.Rows = len(s.rows) - before
	if out.Events == 0 {
		if sp != nil {
			sp.SetAttr("lake.events.duplicate", out.Duplicates)
		}
		return out, nil
	}
	s.sortRows()

	rows := make([]Row, 0, len(s.rows))
	for _, p := range s.rows {
		rows = append(rows, rowFromPosition(p))
	}
	snap, err := s.tbl.Overwrite(ctx, rows)
	if err != nil {
		if sp != nil {
			sp.RecordError(err)
		}
		return Applied{}, err
	}
	out.SnapshotID = snap.ID
	s.opts.counter("tcrp_lake_silver_rows_total", "Bitemporal position versions written.", nil).Add(float64(out.Rows))
	s.opts.counter("tcrp_lake_silver_corrections_total", "Position versions that superseded a published belief.", nil).Add(float64(out.Corrections))
	if sp != nil {
		sp.SetAttrs(map[string]any{
			"lake.events.applied":   out.Events,
			"lake.rows.written":     out.Rows,
			"lake.rows.corrected":   out.Corrections,
			"lake.snapshot.id":      snap.ID,
			"lake.rows.total":       len(s.rows),
			"lake.events.duplicate": out.Duplicates,
		})
	}
	return out, nil
}

// AsOf returns the positions that were true at validAt as the platform
// understood them at knownAt. Move validAt to restate the past; move knownAt to
// see what the desk actually had in front of it.
func (s *Silver) AsOf(validAt, knownAt time.Time) []domain.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	best := map[string]domain.Position{}
	for _, p := range s.rows {
		if !p.ValidAt(validAt, knownAt) {
			continue
		}
		cur, ok := best[p.Key()]
		if !ok || p.TxFrom.After(cur.TxFrom) || (p.TxFrom.Equal(cur.TxFrom) && p.ValidFrom.After(cur.ValidFrom)) {
			best[p.Key()] = p
		}
	}
	keys := make([]string, 0, len(best))
	for k := range best {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]domain.Position, 0, len(keys))
	for _, k := range keys {
		out = append(out, best[k])
	}
	domain.SortPositions(out)
	return out
}

// History returns every version of one slot, oldest belief first. The key is
// the form domain.Position.Key produces: counterparty|symbol|ledger.
func (s *Silver) History(key string) []domain.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []domain.Position
	for _, p := range s.rows {
		if p.Key() == key {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].TxFrom.Equal(out[j].TxFrom) {
			return out[i].TxFrom.Before(out[j].TxFrom)
		}
		return out[i].ValidFrom.Before(out[j].ValidFrom)
	})
	return out
}

// Rows returns every version row in the store.
func (s *Silver) Rows() []domain.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Position(nil), s.rows...)
}

// Keys returns every economic slot the store holds, sorted.
func (s *Silver) Keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, p := range s.rows {
		if !seen[p.Key()] {
			seen[p.Key()] = true
			out = append(out, p.Key())
		}
	}
	sort.Strings(out)
	return out
}

// PositionKey builds the key History takes from its three components.
func PositionKey(counterparty, symbol string, ledger domain.Ledger) string {
	return strings.Join([]string{counterparty, symbol, string(ledger)}, "|")
}

// effect is one slot's change from one event.
type effect struct {
	Counterparty string
	Symbol       string
	Ledger       domain.Ledger
	Delta        int64
	// Pledge is 1 to pledge the holding, -1 to release it, 0 to leave it.
	Pledge int8
}

// eventEffects maps a ledger event onto the slots it moves.
//
// Lock is a reduction rather than a transfer to an escrow holder: the escrow is
// not a counterparty and inventing one would break referential integrity for
// the sake of a prettier balance sheet. The supply identity in quality.go
// accounts for it explicitly instead.
func eventEffects(e domain.LedgerEvent) []effect {
	mk := func(cp string, delta int64, pledge int8) effect {
		return effect{Counterparty: cp, Symbol: e.Symbol, Ledger: e.Ledger, Delta: delta, Pledge: pledge}
	}
	switch e.Kind {
	case domain.EventMint:
		if e.To == "" {
			return nil
		}
		return []effect{mk(e.To, e.Quantity, 0)}
	case domain.EventBurn:
		if e.From == "" {
			return nil
		}
		return []effect{mk(e.From, -e.Quantity, 0)}
	case domain.EventTransfer:
		var out []effect
		if e.From != "" {
			out = append(out, mk(e.From, -e.Quantity, 0))
		}
		if e.To != "" {
			out = append(out, mk(e.To, e.Quantity, 0))
		}
		return out
	case domain.EventLock:
		if e.From == "" {
			return nil
		}
		return []effect{mk(e.From, -e.Quantity, 0)}
	case domain.EventUnlock:
		holder := e.To
		if holder == "" {
			holder = e.From
		}
		if holder == "" {
			return nil
		}
		return []effect{mk(holder, e.Quantity, 0)}
	case domain.EventPledge:
		if e.From == "" {
			return nil
		}
		return []effect{mk(e.From, 0, 1)}
	case domain.EventRelease:
		if e.From == "" {
			return nil
		}
		return []effect{mk(e.From, 0, -1)}
	default:
		// Freeze and anything else recorded later changes no quantity. It is
		// still landed in bronze; silver simply has nothing to version.
		return nil
	}
}

// applyEffect versions one slot. The caller holds s.mu. It returns true when
// the write superseded a belief the store had already published.
func (s *Silver) applyEffect(eff effect, ev domain.LedgerEvent) bool {
	key := PositionKey(eff.Counterparty, eff.Symbol, eff.Ledger)
	validFrom := ev.OccurredAt.UTC()
	txAt := ev.ObservedAt.UTC()

	var open []int
	for i := range s.rows {
		if s.rows[i].TxTo.IsZero() && s.rows[i].Key() == key {
			open = append(open, i)
		}
	}
	sort.SliceStable(open, func(a, b int) bool {
		return s.rows[open[a]].ValidFrom.Before(s.rows[open[b]].ValidFrom)
	})

	var (
		affected   []int
		carry      int64
		carrySeen  bool
		carryFrom  time.Time
		outOfOrder bool
		splits     bool
		firstAfter time.Time
	)
	for _, i := range open {
		r := s.rows[i]
		if !r.ValidTo.IsZero() && !r.ValidTo.After(validFrom) {
			if !carrySeen || r.ValidFrom.After(carryFrom) {
				carry, carrySeen, carryFrom = r.Quantity, true, r.ValidFrom
			}
			continue
		}
		affected = append(affected, i)
		switch {
		case r.ValidFrom.After(validFrom):
			outOfOrder = true
			if firstAfter.IsZero() || r.ValidFrom.Before(firstAfter) {
				firstAfter = r.ValidFrom
			}
		case r.ValidFrom.Before(validFrom):
			if r.TxFrom.Before(txAt) {
				splits = true
			}
		}
	}
	// A retroactive correction is either an event that arrived out of order in
	// valid time, or one that arrived materially late and landed inside an
	// interval the store had already committed to.
	corrected := outOfOrder || (splits && txAt.Sub(validFrom) > s.lateAfter)

	var emitted []domain.Position
	hasAtValidFrom := false
	for _, i := range affected {
		r := s.rows[i]
		s.rows[i].TxTo = txAt
		if r.ValidFrom.Before(validFrom) {
			head := r
			head.ValidTo, head.TxFrom, head.TxTo = validFrom, txAt, time.Time{}
			emitted = append(emitted, s.version(head, ev, false))

			tail := r
			tail.ValidFrom, tail.TxFrom, tail.TxTo = validFrom, txAt, time.Time{}
			tail.Quantity = r.Quantity + eff.Delta
			applyPledge(&tail, eff.Pledge)
			emitted = append(emitted, s.version(tail, ev, corrected))
			hasAtValidFrom = true
			continue
		}
		nr := r
		nr.TxFrom, nr.TxTo = txAt, time.Time{}
		nr.Quantity = r.Quantity + eff.Delta
		applyPledge(&nr, eff.Pledge)
		emitted = append(emitted, s.version(nr, ev, corrected))
		if r.ValidFrom.Equal(validFrom) {
			hasAtValidFrom = true
		}
	}
	if !hasAtValidFrom {
		base := int64(0)
		if carrySeen {
			base = carry
		}
		fresh := domain.Position{
			Counterparty: eff.Counterparty,
			Symbol:       eff.Symbol,
			Ledger:       eff.Ledger,
			Quantity:     base + eff.Delta,
			Pledged:      true,
			ValidFrom:    validFrom,
			ValidTo:      firstAfter,
			TxFrom:       txAt,
		}
		applyPledge(&fresh, eff.Pledge)
		emitted = append(emitted, s.version(fresh, ev, corrected))
	}
	s.rows = append(s.rows, emitted...)
	return corrected
}

func applyPledge(p *domain.Position, pledge int8) {
	switch pledge {
	case 1:
		p.Pledged = true
	case -1:
		p.Pledged = false
	}
}

func (s *Silver) version(p domain.Position, ev domain.LedgerEvent, corrected bool) domain.Position {
	s.nextID++
	p.ID = fmt.Sprintf("SPOS-%06d", s.nextID)
	p.Source = sourceForLedger(ev.Ledger)
	p.BlockHeight = ev.Block
	p.TxHash = ev.TxHash
	p.Corrected = corrected
	return p
}

func (s *Silver) sortRows() {
	sort.SliceStable(s.rows, func(i, j int) bool {
		return idOrdinal(s.rows[i].ID) < idOrdinal(s.rows[j].ID)
	})
}

func sourceForLedger(l domain.Ledger) string {
	switch l {
	case domain.LedgerEVM:
		return "indexer/evm"
	case domain.LedgerCanton:
		return "indexer/canton"
	default:
		return "book-of-record"
	}
}

func idOrdinal(id string) int {
	_, digits, ok := strings.Cut(id, "-")
	if !ok {
		return 0
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func rowFromPosition(p domain.Position) Row {
	r := Row{
		"position_id":  p.ID,
		"counterparty": p.Counterparty,
		"symbol":       p.Symbol,
		"ledger":       string(p.Ledger),
		"quantity":     p.Quantity,
		"pledged":      p.Pledged,
		"valid_from":   p.ValidFrom.UTC(),
		"tx_from":      p.TxFrom.UTC(),
		"source":       p.Source,
		"block_height": int64(p.BlockHeight),
		"tx_hash":      p.TxHash,
		"corrected":    p.Corrected,
	}
	// An open interval is an absent column rather than a sentinel far-future
	// timestamp, so a manifest's upper bound on valid_to means what it says.
	if !p.ValidTo.IsZero() {
		r["valid_to"] = p.ValidTo.UTC()
	}
	if !p.TxTo.IsZero() {
		r["tx_to"] = p.TxTo.UTC()
	}
	return r
}

func positionFromRow(r Row) (domain.Position, error) {
	var p domain.Position
	qty, err := rowInt(r, "quantity")
	if err != nil {
		return p, err
	}
	block, err := rowInt(r, "block_height")
	if err != nil {
		return p, err
	}
	validFrom, err := rowTime(r, "valid_from")
	if err != nil {
		return p, err
	}
	validTo, err := rowTime(r, "valid_to")
	if err != nil {
		return p, err
	}
	txFrom, err := rowTime(r, "tx_from")
	if err != nil {
		return p, err
	}
	txTo, err := rowTime(r, "tx_to")
	if err != nil {
		return p, err
	}
	pledged, _ := r["pledged"].(bool)
	corrected, _ := r["corrected"].(bool)
	p = domain.Position{
		ID:           rowString(r, "position_id"),
		Counterparty: rowString(r, "counterparty"),
		Symbol:       rowString(r, "symbol"),
		Ledger:       domain.Ledger(rowString(r, "ledger")),
		Quantity:     qty,
		Pledged:      pledged,
		ValidFrom:    validFrom,
		ValidTo:      validTo,
		TxFrom:       txFrom,
		TxTo:         txTo,
		Source:       rowString(r, "source"),
		BlockHeight:  uint64(block),
		TxHash:       rowString(r, "tx_hash"),
		Corrected:    corrected,
	}
	return p, nil
}
