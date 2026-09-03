package lake

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// GoldTable is the name of the materialised gold position table.
const GoldTable = "gold_positions"

// GoldSchema is the curated position row: the bitemporal holding flattened to
// one (validAt, knownAt) pair and joined to the reference data the risk engine
// needs. It is materialised rather than computed on every read so that the
// figure on a dashboard has a row behind it that can be pointed at.
var GoldSchema = Schema{
	SchemaID: 0,
	Fields: []Field{
		{ID: 1, Name: "position_id", Type: TypeString, Required: true},
		{ID: 2, Name: "counterparty", Type: TypeString, Required: true},
		{ID: 3, Name: "counterparty_name", Type: TypeString},
		{ID: 4, Name: "symbol", Type: TypeString, Required: true},
		{ID: 5, Name: "asset_class", Type: TypeString},
		{ID: 6, Name: "ledger", Type: TypeString, Required: true},
		{ID: 7, Name: "quantity", Type: TypeLong, Required: true},
		{ID: 8, Name: "pledged", Type: TypeBoolean, Required: true},
		{ID: 9, Name: "price_units", Type: TypeLong, Doc: "mark in 1e-6 currency units"},
		{ID: 10, Name: "price_ccy", Type: TypeString},
		{ID: 11, Name: "price_as_of", Type: TypeTimestamp},
		{ID: 12, Name: "valid_at", Type: TypeTimestamp, Required: true},
		{ID: 13, Name: "known_at", Type: TypeTimestamp, Required: true},
		{ID: 14, Name: "corrected", Type: TypeBoolean, Required: true},
		{ID: 15, Name: "source", Type: TypeString},
	},
}

// GoldSpec partitions the curated table by ledger and by the hour of the
// transaction time it was built for. Hour rather than day because a desk asks
// for several cuts a day and each is a separate answer.
var GoldSpec = PartitionSpec{
	SpecID: 0,
	Fields: []PartitionField{
		{SourceID: 6, FieldID: 1000, Name: "ledger", Transform: TransformIdentity},
		{SourceID: 13, FieldID: 1001, Name: "known_hour", Transform: TransformHour},
	},
}

// Reference is the slowly-changing data the gold layer joins positions to. It
// arrives from the book of record rather than from a ledger, which is why it is
// a parameter rather than a table the pipeline populates.
type Reference struct {
	// Source names where the reference data came from, for lineage.
	Source         string
	Counterparties []domain.Counterparty
	Instruments    []domain.Instrument
	Prices         []domain.Price
	Obligations    []domain.Obligation
	Haircuts       domain.HaircutSchedule
	Limits         []domain.Limit
}

// ReferenceFromSeed lifts the seeded reference data into the gold layer.
func ReferenceFromSeed(s *domain.Seed) Reference {
	return Reference{
		Source:         "seed:domain.NewSeed",
		Counterparties: s.Counterparties,
		Instruments:    s.Instruments,
		Prices:         s.Prices,
		Obligations:    s.Obligations,
		Haircuts:       s.Haircuts,
		Limits:         s.Limits,
	}
}

// GoldStore builds curated snapshots.
//
// A snapshot is the contract with the risk engine: positions as they were at a
// valid time and as they were understood at a transaction time, joined to
// counterparties, instruments, marks, obligations, haircuts and limits, carrying
// the lineage of every table and snapshot id it was built from and the verdict
// of the data-quality controls. A snapshot that failed a critical control is
// still returned, marked, so a caller can refuse to act on it rather than never
// seeing it.
type GoldStore struct {
	opts       Options
	tbl        *Table
	bronze     *Bronze
	silver     *Silver
	ref        Reference
	thresholds Thresholds
}

// NewGold opens or creates the curated table under root.
func NewGold(ctx context.Context, root string, bronze *Bronze, silver *Silver, ref Reference, opts Options) (*GoldStore, error) {
	opts = opts.resolve()
	tbl, err := LoadOrCreateTable(ctx, root, GoldTable, GoldSchema, GoldSpec, opts.tableOptions())
	if err != nil {
		return nil, err
	}
	return &GoldStore{opts: opts, tbl: tbl, bronze: bronze, silver: silver, ref: ref, thresholds: DefaultThresholds}, nil
}

// Table exposes the underlying table.
func (g *GoldStore) Table() *Table { return g.tbl }

// Reference returns the reference data the store joins against.
func (g *GoldStore) Reference() Reference { return g.ref }

// SetThresholds replaces the data-quality thresholds.
func (g *GoldStore) SetThresholds(th Thresholds) { g.thresholds = th.resolve() }

// Snapshot builds the curated view for one (validAt, knownAt) pair and
// materialises its positions as a new snapshot of the gold table.
func (g *GoldStore) Snapshot(ctx context.Context, validAt, knownAt time.Time) (domain.Snapshot, error) {
	ctx, sp := g.opts.start(ctx, "lake.gold.snapshot", obs.KindInternal)
	if sp != nil {
		defer sp.End()
	}
	validAt, knownAt = validAt.UTC(), knownAt.UTC()

	all := g.silver.AsOf(validAt, knownAt)
	positions := make([]domain.Position, 0, len(all))
	for _, p := range all {
		// A slot that has netted to nothing is not a holding. It stays in
		// silver, where its history is still readable, but it is not collateral
		// and does not belong in a view the risk engine sums.
		if p.Quantity != 0 {
			positions = append(positions, p)
		}
	}
	events := g.bronze.EventsKnownAt(knownAt)
	prices := g.pricesAsOf(validAt)

	quality := RunQualityChecks(QualityInputs{
		ValidAt:        validAt,
		KnownAt:        knownAt,
		Events:         events,
		Landed:         g.bronze.Events(),
		Positions:      all,
		Counterparties: g.ref.Counterparties,
		Prices:         valuesOf(prices),
	}, g.thresholds, g.opts.Clock.Now())

	rows := make([]Row, 0, len(positions))
	for _, p := range positions {
		rows = append(rows, g.goldRow(p, prices, validAt, knownAt))
	}
	goldSnapID := int64(0)
	if len(rows) > 0 {
		snap, err := g.tbl.Append(ctx, rows)
		if err != nil {
			if sp != nil {
				sp.RecordError(err)
			}
			return domain.Snapshot{}, err
		}
		goldSnapID = snap.ID
	}

	out := domain.Snapshot{
		ValidAt:        validAt,
		KnownAt:        knownAt,
		Counterparties: g.ref.Counterparties,
		Instruments:    g.ref.Instruments,
		Prices:         prices,
		Positions:      positions,
		Obligations:    g.obligationsAsOf(validAt),
		Haircuts:       g.ref.Haircuts,
		Limits:         g.limitsAsOf(validAt),
		Quality:        quality,
		Lineage: []string{
			lineage(BronzeTable, g.bronze.Table().CurrentSnapshotID()),
			lineage(SilverTable, g.silver.Table().CurrentSnapshotID()),
			lineage(GoldTable, goldSnapID),
			"reference:" + g.ref.Source,
		},
	}
	g.opts.gauge("tcrp_lake_gold_positions", "Positions in the most recent curated snapshot.", nil).
		Set(float64(len(positions)))
	if sp != nil {
		sp.SetAttrs(map[string]any{
			"lake.gold.positions": len(positions),
			"lake.gold.quality":   string(quality.Status),
			"lake.snapshot.id":    goldSnapID,
		})
	}
	return out, nil
}

func lineage(table string, snapshot int64) string {
	return fmt.Sprintf("lake.%s@snapshot=%d", table, snapshot)
}

func (g *GoldStore) goldRow(p domain.Position, prices map[string]domain.Price, validAt, knownAt time.Time) Row {
	row := Row{
		"position_id":  p.ID,
		"counterparty": p.Counterparty,
		"symbol":       p.Symbol,
		"ledger":       string(p.Ledger),
		"quantity":     p.Quantity,
		"pledged":      p.Pledged,
		"valid_at":     validAt,
		"known_at":     knownAt,
		"corrected":    p.Corrected,
		"source":       p.Source,
	}
	for _, cp := range g.ref.Counterparties {
		if cp.ID == p.Counterparty {
			row["counterparty_name"] = cp.Name
			break
		}
	}
	for _, in := range g.ref.Instruments {
		if in.Symbol == p.Symbol {
			row["asset_class"] = string(in.Class)
			break
		}
	}
	if px, ok := prices[p.Symbol]; ok {
		row["price_units"] = px.Px.Units
		row["price_ccy"] = string(px.Px.Ccy)
		row["price_as_of"] = px.AsOf.UTC()
	}
	return row
}

func (g *GoldStore) pricesAsOf(validAt time.Time) map[string]domain.Price {
	out := map[string]domain.Price{}
	for _, p := range g.ref.Prices {
		if p.AsOf.After(validAt) {
			continue
		}
		if cur, ok := out[p.Symbol]; ok && !p.AsOf.After(cur.AsOf) {
			continue
		}
		out[p.Symbol] = p
	}
	return out
}

func (g *GoldStore) obligationsAsOf(validAt time.Time) []domain.Obligation {
	out := make([]domain.Obligation, 0, len(g.ref.Obligations))
	for _, o := range g.ref.Obligations {
		if !o.AsOf.After(validAt) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (g *GoldStore) limitsAsOf(validAt time.Time) []domain.Limit {
	out := make([]domain.Limit, 0, len(g.ref.Limits))
	for _, l := range g.ref.Limits {
		if !l.Effective.After(validAt) {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func valuesOf(m map[string]domain.Price) []domain.Price {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]domain.Price, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}
