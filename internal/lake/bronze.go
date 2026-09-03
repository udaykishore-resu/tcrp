package lake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// BronzeTable is the table name the raw landing zone uses.
const BronzeTable = "bronze_ledger_events"

// BronzeSchema is the landing schema. It is the ledger event as observed, with
// nothing derived and nothing dropped, because the point of a bronze layer is
// that a bug in a downstream transform is repairable by replaying it rather
// than by re-reading a chain.
var BronzeSchema = Schema{
	SchemaID: 0,
	Fields: []Field{
		{ID: 1, Name: "event_id", Type: TypeString, Required: true, Doc: "ledger:tx_hash:log_index, the event's natural key"},
		{ID: 2, Name: "seq", Type: TypeLong, Required: true},
		{ID: 3, Name: "ledger", Type: TypeString, Required: true},
		{ID: 4, Name: "kind", Type: TypeString, Required: true},
		{ID: 5, Name: "symbol", Type: TypeString, Required: true},
		{ID: 6, Name: "from_party", Type: TypeString},
		{ID: 7, Name: "to_party", Type: TypeString},
		{ID: 8, Name: "quantity", Type: TypeLong, Required: true},
		{ID: 9, Name: "block", Type: TypeLong, Required: true},
		{ID: 10, Name: "tx_hash", Type: TypeString, Required: true},
		{ID: 11, Name: "log_index", Type: TypeLong, Required: true},
		{ID: 12, Name: "occurred_at", Type: TypeTimestamp, Required: true, Doc: "block time"},
		{ID: 13, Name: "observed_at", Type: TypeTimestamp, Required: true, Doc: "when the platform saw it"},
		{ID: 14, Name: "finalised", Type: TypeBoolean, Required: true},
		{ID: 15, Name: "memo", Type: TypeString},
	},
}

// BronzeSpec partitions the landing zone by ledger and by the day the event
// occurred. Ledger is identity because every reconciliation query names one;
// the day transform keeps the partition count linear in time rather than in
// events.
var BronzeSpec = PartitionSpec{
	SpecID: 0,
	Fields: []PartitionField{
		{SourceID: 3, FieldID: 1000, Name: "ledger", Transform: TransformIdentity},
		{SourceID: 12, FieldID: 1001, Name: "occurred_day", Transform: TransformDay},
	},
}

// Bronze is the raw landing zone: append-only, deduplicated on the event's
// natural key. Ingest is therefore idempotent — the same event delivered twice,
// by a retried produce or by a consumer replaying an uncommitted batch, lands
// once — which is what lets the stream layer choose at-least-once delivery and
// still produce an exactly-once table.
type Bronze struct {
	opts Options
	tbl  *Table

	mu     sync.Mutex
	seen   map[string]bool
	events []domain.LedgerEvent
}

// Ingested reports what one Append did.
type Ingested struct {
	// Accepted is the number of events new to the table.
	Accepted int
	// Duplicates is the number suppressed by the natural key.
	Duplicates int
	// SnapshotID is the snapshot the accepted events landed in, or zero when
	// every event was a duplicate and nothing was committed.
	SnapshotID int64
}

// NewBronze opens or creates the landing table under root and rebuilds its
// dedupe set by reading what is already there.
func NewBronze(ctx context.Context, root string, opts Options) (*Bronze, error) {
	opts = opts.resolve()
	tbl, err := LoadOrCreateTable(ctx, root, BronzeTable, BronzeSchema, BronzeSpec, opts.tableOptions())
	if err != nil {
		return nil, err
	}
	b := &Bronze{opts: opts, tbl: tbl, seen: map[string]bool{}}
	if tbl.CurrentSnapshotID() != 0 {
		res, err := tbl.Scan(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range res.Rows {
			ev, err := eventFromRow(row)
			if err != nil {
				return nil, err
			}
			if b.seen[ev.ID()] {
				continue
			}
			b.seen[ev.ID()] = true
			b.events = append(b.events, ev)
		}
		domain.SortEvents(b.events)
	}
	return b, nil
}

// Table exposes the underlying table so lineage can name its snapshot.
func (b *Bronze) Table() *Table { return b.tbl }

// Append lands events. Events already in the table are counted and dropped;
// nothing is committed when every event is a duplicate, so a replayed batch
// does not produce an empty snapshot on every cycle.
func (b *Bronze) Append(ctx context.Context, events []domain.LedgerEvent) (Ingested, error) {
	ctx, sp := b.opts.start(ctx, "lake.bronze.append", obs.KindInternal)
	if sp != nil {
		defer sp.End()
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	var out Ingested
	fresh := make([]domain.LedgerEvent, 0, len(events))
	rows := make([]Row, 0, len(events))
	staged := map[string]bool{}
	for _, ev := range events {
		id := ev.ID()
		if b.seen[id] || staged[id] {
			out.Duplicates++
			continue
		}
		staged[id] = true
		fresh = append(fresh, ev)
		rows = append(rows, rowFromEvent(ev))
	}
	out.Accepted = len(fresh)
	b.opts.counter("tcrp_lake_bronze_events_total", "Ledger events landed in bronze.", nil).Add(float64(out.Accepted))
	b.opts.counter("tcrp_lake_bronze_duplicates_total", "Ledger events suppressed by the natural key.", nil).Add(float64(out.Duplicates))
	if len(rows) == 0 {
		if sp != nil {
			sp.SetAttrs(map[string]any{"lake.rows.accepted": 0, "lake.rows.duplicate": out.Duplicates})
		}
		return out, nil
	}
	snap, err := b.tbl.Append(ctx, rows)
	if err != nil {
		if sp != nil {
			sp.RecordError(err)
		}
		return Ingested{}, err
	}
	for _, ev := range fresh {
		b.seen[ev.ID()] = true
	}
	b.events = append(b.events, fresh...)
	domain.SortEvents(b.events)
	out.SnapshotID = snap.ID
	if sp != nil {
		sp.SetAttrs(map[string]any{
			"lake.rows.accepted":  out.Accepted,
			"lake.rows.duplicate": out.Duplicates,
			"lake.snapshot.id":    snap.ID,
		})
	}
	return out, nil
}

// Events returns every landed event, ordered by occurrence.
func (b *Bronze) Events() []domain.LedgerEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.LedgerEvent(nil), b.events...)
}

// EventsKnownAt returns the events the platform had observed by knownAt. It is
// the transaction-time filter every bitemporal read starts from.
func (b *Bronze) EventsKnownAt(knownAt time.Time) []domain.LedgerEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]domain.LedgerEvent, 0, len(b.events))
	for _, ev := range b.events {
		if !ev.ObservedAt.After(knownAt) {
			out = append(out, ev)
		}
	}
	return out
}

// Count returns the number of distinct events landed.
func (b *Bronze) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

// ScanLedger reads one ledger's events back through the table, which exercises
// partition pruning and reports how many files the predicate skipped.
func (b *Bronze) ScanLedger(ctx context.Context, ledger domain.Ledger) (ScanResult, error) {
	return b.tbl.Scan(ctx, Eq("ledger", string(ledger)))
}

func rowFromEvent(e domain.LedgerEvent) Row {
	return Row{
		"event_id":    e.ID(),
		"seq":         int64(e.Seq),
		"ledger":      string(e.Ledger),
		"kind":        string(e.Kind),
		"symbol":      e.Symbol,
		"from_party":  e.From,
		"to_party":    e.To,
		"quantity":    e.Quantity,
		"block":       int64(e.Block),
		"tx_hash":     e.TxHash,
		"log_index":   int64(e.LogIndex),
		"occurred_at": e.OccurredAt.UTC(),
		"observed_at": e.ObservedAt.UTC(),
		"finalised":   e.Finalised,
		"memo":        e.Memo,
	}
}

func eventFromRow(r Row) (domain.LedgerEvent, error) {
	var e domain.LedgerEvent
	seq, err := rowInt(r, "seq")
	if err != nil {
		return e, err
	}
	qty, err := rowInt(r, "quantity")
	if err != nil {
		return e, err
	}
	block, err := rowInt(r, "block")
	if err != nil {
		return e, err
	}
	logIndex, err := rowInt(r, "log_index")
	if err != nil {
		return e, err
	}
	occurred, err := rowTime(r, "occurred_at")
	if err != nil {
		return e, err
	}
	observed, err := rowTime(r, "observed_at")
	if err != nil {
		return e, err
	}
	finalised, _ := r["finalised"].(bool)
	e = domain.LedgerEvent{
		Seq:        uint64(seq),
		Ledger:     domain.Ledger(rowString(r, "ledger")),
		Kind:       domain.EventKind(rowString(r, "kind")),
		Symbol:     rowString(r, "symbol"),
		From:       rowString(r, "from_party"),
		To:         rowString(r, "to_party"),
		Quantity:   qty,
		Block:      uint64(block),
		TxHash:     rowString(r, "tx_hash"),
		LogIndex:   int(logIndex),
		OccurredAt: occurred,
		ObservedAt: observed,
		Finalised:  finalised,
		Memo:       rowString(r, "memo"),
	}
	return e, nil
}

func rowString(r Row, k string) string {
	s, _ := r[k].(string)
	return s
}

func rowInt(r Row, k string) (int64, error) {
	v, ok := r[k]
	if !ok {
		return 0, nil
	}
	n, err := asInt64(v)
	if err != nil {
		return 0, fmt.Errorf("lake: column %s: %w", k, err)
	}
	return n, nil
}

func rowTime(r Row, k string) (time.Time, error) {
	v, ok := r[k]
	if !ok {
		return time.Time{}, nil
	}
	ts, ok := v.(time.Time)
	if !ok {
		return time.Time{}, fmt.Errorf("lake: column %s is not a timestamp", k)
	}
	return ts.UTC(), nil
}
