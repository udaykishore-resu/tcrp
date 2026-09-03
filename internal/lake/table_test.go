package lake_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/lake"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

var origin = time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)

var tradeSchema = lake.Schema{
	SchemaID: 0,
	Fields: []lake.Field{
		{ID: 1, Name: "trade_id", Type: lake.TypeString, Required: true},
		{ID: 2, Name: "venue", Type: lake.TypeString, Required: true},
		{ID: 3, Name: "quantity", Type: lake.TypeLong, Required: true},
		{ID: 4, Name: "settled", Type: lake.TypeBoolean, Required: true},
		{ID: 5, Name: "traded_at", Type: lake.TypeTimestamp, Required: true},
	},
}

var tradeSpec = lake.PartitionSpec{
	SpecID: 0,
	Fields: []lake.PartitionField{
		{SourceID: 2, FieldID: 1000, Name: "venue", Transform: lake.TransformIdentity},
	},
}

func newTable(t *testing.T, dir string, clock obs.Clock) *lake.Table {
	t.Helper()
	tbl, err := lake.CreateTable(context.Background(), dir, "trades", tradeSchema, tradeSpec,
		lake.TableOptions{Clock: clock})
	if err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	return tbl
}

func TestAppendAndScanRoundTrip(t *testing.T) {
	ctx := context.Background()
	tbl := newTable(t, t.TempDir(), obs.NewManualClock(origin))

	// A quantity beyond float64's exact integer range: a JSON round trip that
	// widens it to a float would come back wrong.
	const big = int64(9_007_199_254_740_993)
	rows := []lake.Row{
		{"trade_id": "T1", "venue": "evm", "quantity": big, "settled": true, "traded_at": origin},
		{"trade_id": "T2", "venue": "canton", "quantity": int64(-42), "settled": false, "traded_at": origin.Add(time.Hour)},
	}
	if _, err := tbl.Append(ctx, rows); err != nil {
		t.Fatalf("Append: %v", err)
	}
	res, err := tbl.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("scanned %d rows, want 2", len(res.Rows))
	}
	byID := map[string]lake.Row{}
	for _, r := range res.Rows {
		byID[r["trade_id"].(string)] = r
	}
	if got := byID["T1"]["quantity"]; got != big {
		t.Fatalf("quantity came back %v (%T), want %d", got, got, big)
	}
	if got := byID["T2"]["settled"]; got != false {
		t.Fatalf("settled came back %v", got)
	}
	ts, ok := byID["T2"]["traded_at"].(time.Time)
	if !ok || !ts.Equal(origin.Add(time.Hour)) {
		t.Fatalf("traded_at came back %v", byID["T2"]["traded_at"])
	}
}

func TestScanSkipsFilesUsingManifestBounds(t *testing.T) {
	ctx := context.Background()
	tbl := newTable(t, t.TempDir(), obs.NewManualClock(origin))

	// Three venues, three appends: nine data files, three per venue.
	for i := 0; i < 3; i++ {
		var rows []lake.Row
		for _, venue := range []string{"evm", "canton", "golden"} {
			rows = append(rows, lake.Row{
				"trade_id": venue + string(rune('a'+i)), "venue": venue,
				"quantity": int64(100 * (i + 1)), "settled": true,
				"traded_at": origin.Add(time.Duration(i) * time.Hour),
			})
		}
		if _, err := tbl.Append(ctx, rows); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	entries, err := tbl.Entries(tbl.CurrentSnapshotID())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 9 {
		t.Fatalf("table holds %d data files, want 9", len(entries))
	}

	res, err := tbl.Scan(ctx, lake.Eq("venue", "canton"))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.FilesScanned != 3 || res.FilesSkipped != 6 {
		t.Fatalf("partition predicate scanned %d and skipped %d, want 3 and 6",
			res.FilesScanned, res.FilesSkipped)
	}
	if len(res.Rows) != 3 {
		t.Fatalf("returned %d rows, want 3", len(res.Rows))
	}

	// A predicate on a non-partition column has to fall back to the manifest
	// bounds, which are still enough to skip two thirds of the files.
	res, err = tbl.Scan(ctx, lake.Gte("quantity", int64(300)))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.FilesScanned != 3 || res.FilesSkipped != 6 {
		t.Fatalf("bounds predicate scanned %d and skipped %d, want 3 and 6",
			res.FilesScanned, res.FilesSkipped)
	}
	for _, r := range res.Rows {
		if r["quantity"].(int64) < 300 {
			t.Fatalf("row survived the predicate with quantity %v", r["quantity"])
		}
	}

	// A value outside every file's bounds skips everything.
	res, err = tbl.Scan(ctx, lake.Eq("quantity", int64(999)))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.FilesScanned != 0 || res.FilesSkipped != 9 {
		t.Fatalf("unmatchable predicate scanned %d files", res.FilesScanned)
	}
}

func TestTimeTravelAndSnapshotAsOf(t *testing.T) {
	ctx := context.Background()
	clock := obs.NewManualClock(origin)
	tbl := newTable(t, t.TempDir(), clock)

	var ids []int64
	for i := 0; i < 3; i++ {
		snap, err := tbl.Append(ctx, []lake.Row{{
			"trade_id": "T" + string(rune('1'+i)), "venue": "evm",
			"quantity": int64(i + 1), "settled": true, "traded_at": origin,
		}})
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		ids = append(ids, snap.ID)
		clock.Advance(time.Hour)
	}
	if got := len(tbl.Snapshots()); got != 3 {
		t.Fatalf("%d snapshots, want 3", got)
	}
	for i, id := range ids {
		res, err := tbl.TimeTravel(ctx, id)
		if err != nil {
			t.Fatalf("TimeTravel %d: %v", id, err)
		}
		if len(res.Rows) != i+1 {
			t.Fatalf("snapshot %d holds %d rows, want %d", id, len(res.Rows), i+1)
		}
	}
	snap, ok := tbl.SnapshotAsOf(origin.Add(90 * time.Minute))
	if !ok || snap.ID != ids[1] {
		t.Fatalf("SnapshotAsOf returned %v/%v, want %d", snap.ID, ok, ids[1])
	}
	if _, ok := tbl.SnapshotAsOf(origin.Add(-time.Hour)); ok {
		t.Fatal("SnapshotAsOf found a snapshot before the table existed")
	}
}

func TestExpireSnapshotsDropsUnreferencedFiles(t *testing.T) {
	ctx := context.Background()
	clock := obs.NewManualClock(origin)
	dir := t.TempDir()
	tbl := newTable(t, dir, clock)

	first, err := tbl.Append(ctx, []lake.Row{{
		"trade_id": "T1", "venue": "evm", "quantity": int64(1), "settled": true, "traded_at": origin,
	}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	firstEntries, err := tbl.Entries(first.ID)
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	clock.Advance(2 * time.Hour)
	// An overwrite makes the first snapshot's data file unreferenced by the
	// current one, but it stays readable until the snapshot is expired.
	if _, err := tbl.Overwrite(ctx, []lake.Row{{
		"trade_id": "T2", "venue": "evm", "quantity": int64(2), "settled": true, "traded_at": origin,
	}}); err != nil {
		t.Fatalf("Overwrite: %v", err)
	}
	if res, err := tbl.TimeTravel(ctx, first.ID); err != nil || len(res.Rows) != 1 {
		t.Fatalf("time travel to the overwritten snapshot: %v, %d rows", err, len(res.Rows))
	}

	n, err := tbl.ExpireSnapshots(origin.Add(time.Hour))
	if err != nil {
		t.Fatalf("ExpireSnapshots: %v", err)
	}
	if n != 1 {
		t.Fatalf("expired %d snapshots, want 1", n)
	}
	if len(tbl.Snapshots()) != 1 {
		t.Fatalf("%d snapshots retained, want 1", len(tbl.Snapshots()))
	}
	if _, err := tbl.TimeTravel(ctx, first.ID); err == nil {
		t.Fatal("time travel to an expired snapshot still works")
	}
	if _, err := os.Stat(filepath.Join(dir, "trades", firstEntries[0].Path)); !os.IsNotExist(err) {
		t.Fatalf("expired data file %s is still on disk", firstEntries[0].Path)
	}
	res, err := tbl.Scan(ctx)
	if err != nil || len(res.Rows) != 1 || res.Rows[0]["trade_id"] != "T2" {
		t.Fatalf("current snapshot damaged by expiry: %v, %v", err, res.Rows)
	}
}

func TestCompactCollapsesSmallFiles(t *testing.T) {
	ctx := context.Background()
	tbl := newTable(t, t.TempDir(), obs.NewManualClock(origin))
	for i := 0; i < 6; i++ {
		if _, err := tbl.Append(ctx, []lake.Row{{
			"trade_id": "T" + string(rune('1'+i)), "venue": "evm",
			"quantity": int64(i), "settled": true, "traded_at": origin,
		}}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	before, err := tbl.Entries(tbl.CurrentSnapshotID())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(before) != 6 {
		t.Fatalf("%d files before compaction, want 6", len(before))
	}
	snap, err := tbl.Compact(ctx, 1<<20)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if snap.ID == 0 {
		t.Fatal("Compact rewrote nothing")
	}
	after, err := tbl.Entries(tbl.CurrentSnapshotID())
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("%d files after compaction, want 1", len(after))
	}
	res, err := tbl.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Rows) != 6 {
		t.Fatalf("compaction changed the row count to %d", len(res.Rows))
	}
	if after[0].RecordCount != 6 {
		t.Fatalf("compacted manifest claims %d records", after[0].RecordCount)
	}
}

func TestWritesAreByteIdentical(t *testing.T) {
	ctx := context.Background()
	rows := []lake.Row{
		{"trade_id": "T1", "venue": "evm", "quantity": int64(7), "settled": true, "traded_at": origin},
		{"trade_id": "T2", "venue": "evm", "quantity": int64(9), "settled": false, "traded_at": origin.Add(time.Minute)},
	}
	read := func() []byte {
		dir := t.TempDir()
		tbl, err := lake.CreateTable(ctx, dir, "trades", tradeSchema, tradeSpec,
			lake.TableOptions{Clock: obs.NewManualClock(origin)})
		if err != nil {
			t.Fatalf("CreateTable: %v", err)
		}
		if _, err := tbl.Append(ctx, rows); err != nil {
			t.Fatalf("Append: %v", err)
		}
		entries, err := tbl.Entries(tbl.CurrentSnapshotID())
		if err != nil {
			t.Fatalf("Entries: %v", err)
		}
		body, err := os.ReadFile(filepath.Join(dir, "trades", entries[0].Path))
		if err != nil {
			t.Fatalf("read data file: %v", err)
		}
		return body
	}
	a, b := read(), read()
	if string(a) != string(b) {
		t.Fatal("two identical writes produced different bytes")
	}
}

func TestOpenTableRecoversMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clock := obs.NewManualClock(origin)
	tbl := newTable(t, dir, clock)
	if _, err := tbl.Append(ctx, []lake.Row{{
		"trade_id": "T1", "venue": "evm", "quantity": int64(1), "settled": true, "traded_at": origin,
	}}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	reopened, err := lake.OpenTable(ctx, dir, "trades", lake.TableOptions{Clock: clock})
	if err != nil {
		t.Fatalf("OpenTable: %v", err)
	}
	if reopened.CurrentSnapshotID() != tbl.CurrentSnapshotID() {
		t.Fatalf("reopened at snapshot %d, want %d", reopened.CurrentSnapshotID(), tbl.CurrentSnapshotID())
	}
	res, err := reopened.Scan(ctx)
	if err != nil || len(res.Rows) != 1 {
		t.Fatalf("reopened scan: %v, %d rows", err, len(res.Rows))
	}
	if _, err := lake.CreateTable(ctx, dir, "trades", tradeSchema, tradeSpec, lake.TableOptions{Clock: clock}); err == nil {
		t.Fatal("CreateTable overwrote an existing table")
	}
}

func TestScanOnUnknownFieldIsAnError(t *testing.T) {
	ctx := context.Background()
	tbl := newTable(t, t.TempDir(), obs.NewManualClock(origin))
	if _, err := tbl.Append(ctx, []lake.Row{{
		"trade_id": "T1", "venue": "evm", "quantity": int64(1), "settled": true, "traded_at": origin,
	}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := tbl.Scan(ctx, lake.Eq("nope", "x")); err == nil {
		t.Fatal("a filter on an unknown column was accepted")
	}
}
