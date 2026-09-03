package lake

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by the table layer.
var (
	// ErrNoSnapshot is returned by a read against a table that has never been
	// written to.
	ErrNoSnapshot = errors.New("lake: table has no snapshot")
	// ErrUnknownSnapshot is returned when time travel names a snapshot that
	// does not exist or has been expired.
	ErrUnknownSnapshot = errors.New("lake: unknown snapshot")
	// ErrUnknownField is returned when a schema does not carry a named field.
	ErrUnknownField = errors.New("lake: unknown field")
	// ErrTableExists is returned when creating a table over an existing one.
	ErrTableExists = errors.New("lake: table already exists")
)

// FieldType is the physical type of a column. The set is small on purpose:
// these four types cover every column the platform lands, and each has an
// unambiguous ordering, which is what makes the per-column bounds in a manifest
// usable for skipping files.
type FieldType string

// Field types.
const (
	TypeString    FieldType = "string"
	TypeLong      FieldType = "long"
	TypeBoolean   FieldType = "boolean"
	TypeTimestamp FieldType = "timestamptz"
)

// Field is one column. IDs are assigned once and never reused, which is what
// lets a column be renamed without rewriting data.
type Field struct {
	ID       int       `json:"id"`
	Name     string    `json:"name"`
	Type     FieldType `json:"type"`
	Required bool      `json:"required"`
	Doc      string    `json:"doc,omitempty"`
}

// Schema is an ordered set of fields with an identifier.
type Schema struct {
	SchemaID int     `json:"schema-id"`
	Fields   []Field `json:"fields"`
}

// Field returns the named field.
func (s Schema) Field(name string) (Field, bool) {
	for _, f := range s.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return Field{}, false
}

// FieldByID returns the field with the given id.
func (s Schema) FieldByID(id int) (Field, bool) {
	for _, f := range s.Fields {
		if f.ID == id {
			return f, true
		}
	}
	return Field{}, false
}

// Transform maps a source column to a partition value.
type Transform string

// Transforms. Identity partitions on the value itself; day and hour truncate a
// timestamp, which is what keeps the partition count bounded as the table grows.
const (
	TransformIdentity Transform = "identity"
	TransformDay      Transform = "day"
	TransformHour     Transform = "hour"
)

// PartitionField is one component of a partition spec.
type PartitionField struct {
	SourceID  int       `json:"source-id"`
	FieldID   int       `json:"field-id"`
	Name      string    `json:"name"`
	Transform Transform `json:"transform"`
}

// PartitionSpec describes how a table's files are laid out. An unpartitioned
// table is a spec with no fields, which is legal and means every file is a
// candidate for every scan.
type PartitionSpec struct {
	SpecID int              `json:"spec-id"`
	Fields []PartitionField `json:"fields"`
}

// Snapshot is a committed state of the table. Every write produces one, and a
// reader that names a snapshot id sees exactly the files that snapshot names,
// no matter what has been written since.
type Snapshot struct {
	ID           int64             `json:"snapshot-id"`
	ParentID     int64             `json:"parent-snapshot-id,omitempty"`
	TimestampMS  int64             `json:"timestamp-ms"`
	Operation    string            `json:"operation"`
	ManifestList string            `json:"manifest-list"`
	Summary      map[string]string `json:"summary,omitempty"`
}

// Time returns the snapshot's commit time.
func (s Snapshot) Time() time.Time { return time.UnixMilli(s.TimestampMS).UTC() }

// Operations recorded on a snapshot, matching Iceberg's vocabulary.
const (
	OpAppend    = "append"
	OpOverwrite = "overwrite"
	OpReplace   = "replace"
)

// EntryStatus is a manifest entry's state in the snapshot that wrote it.
type EntryStatus string

// Entry statuses.
const (
	StatusAdded    EntryStatus = "added"
	StatusExisting EntryStatus = "existing"
	StatusDeleted  EntryStatus = "deleted"
)

// ManifestEntry describes one data file. The row count and the per-column
// bounds are the point: a scan reads the manifest, compares its predicate
// against the bounds, and never opens a file that cannot contain a match.
type ManifestEntry struct {
	Path            string            `json:"data-file-path"`
	Partition       map[string]string `json:"partition,omitempty"`
	RecordCount     int64             `json:"record-count"`
	FileSizeBytes   int64             `json:"file-size-in-bytes"`
	LowerBounds     map[string]string `json:"lower-bounds,omitempty"`
	UpperBounds     map[string]string `json:"upper-bounds,omitempty"`
	AddedSnapshotID int64             `json:"added-snapshot-id"`
	Status          EntryStatus       `json:"status"`
}

// ManifestFile is one entry in a snapshot's manifest list.
type ManifestFile struct {
	Path            string `json:"manifest-path"`
	AddedSnapshotID int64  `json:"added-snapshot-id"`
	AddedFilesCount int    `json:"added-files-count"`
	AddedRowsCount  int64  `json:"added-rows-count"`
}

// Metadata is the table's root metadata document, written whole on every
// commit. Iceberg swings a pointer at a new metadata file rather than editing
// one in place; this does the same, and the previous documents are what time
// travel reads.
type Metadata struct {
	FormatVersion     int               `json:"format-version"`
	TableName         string            `json:"table-name"`
	Location          string            `json:"location"`
	LastUpdatedMS     int64             `json:"last-updated-ms"`
	LastColumnID      int               `json:"last-column-id"`
	Schemas           []Schema          `json:"schemas"`
	CurrentSchemaID   int               `json:"current-schema-id"`
	PartitionSpecs    []PartitionSpec   `json:"partition-specs"`
	DefaultSpecID     int               `json:"default-spec-id"`
	CurrentSnapshotID int64             `json:"current-snapshot-id"`
	LastSnapshotID    int64             `json:"last-snapshot-id"`
	Snapshots         []Snapshot        `json:"snapshots"`
	Properties        map[string]string `json:"properties,omitempty"`
}

// Row is one record. Values are Go types matching the field types: string,
// int64, bool and time.Time. A file format is responsible for round-tripping
// them without widening an int64 into a float, which is the failure mode that
// makes JSON a poor default for financial data.
type Row map[string]any

// FileFormat writes and reads a table's data files.
//
// The interface exists so that the format is a wiring decision. The
// implementation shipped here is gzipped NDJSON, which is honest about being a
// reference format: it is self-describing, diffable and needs no dependency,
// and it is not columnar, has no page-level statistics and no dictionary
// encoding. Parquet is the production choice, and this interface is the seam
// where it goes.
type FileFormat interface {
	// Name identifies the format in table properties.
	Name() string
	// Extension is the data file suffix, including the leading dot.
	Extension() string
	// Write encodes rows in schema order.
	Write(w io.Writer, schema Schema, rows []Row) error
	// Read decodes rows, coercing each value to the schema's Go type.
	Read(r io.Reader, schema Schema) ([]Row, error)
}

// NDJSONGzip is the reference file format: one JSON object per line, gzipped.
type NDJSONGzip struct{}

// Name returns the format name recorded in table properties.
func (NDJSONGzip) Name() string { return "ndjson-gzip" }

// Extension returns the data file suffix.
func (NDJSONGzip) Extension() string { return ".ndjson.gz" }

// Write encodes each row as a JSON object on its own line. Field order follows
// the schema and timestamps are RFC 3339 in UTC, so the bytes are identical for
// identical input.
func (NDJSONGzip) Write(w io.Writer, schema Schema, rows []Row) error {
	zw := gzip.NewWriter(w)
	bw := bufio.NewWriter(zw)
	for _, row := range rows {
		obj := make(map[string]json.RawMessage, len(schema.Fields))
		var keys []string
		for _, f := range schema.Fields {
			v, ok := row[f.Name]
			if !ok || v == nil {
				continue
			}
			raw, err := encodeValue(f.Type, v)
			if err != nil {
				return fmt.Errorf("lake: encode %s: %w", f.Name, err)
			}
			obj[f.Name] = raw
			keys = append(keys, f.Name)
		}
		bw.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				bw.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("lake: encode key %s: %w", k, err)
			}
			bw.Write(kb)
			bw.WriteByte(':')
			bw.Write(obj[k])
		}
		bw.WriteString("}\n")
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("lake: flush data file: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("lake: close data file: %w", err)
	}
	return nil
}

// Read decodes rows and coerces every value to the schema's type. Numbers are
// decoded through json.Number so that a large int64 does not lose its low bits
// to a float64.
func (NDJSONGzip) Read(r io.Reader, schema Schema) ([]Row, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("lake: open data file: %w", err)
	}
	defer func() { _ = zr.Close() }()
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []Row
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		dec.UseNumber()
		raw := map[string]any{}
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("lake: decode row: %w", err)
		}
		row := make(Row, len(raw))
		for _, f := range schema.Fields {
			v, ok := raw[f.Name]
			if !ok {
				continue
			}
			cv, err := coerceValue(f.Type, v)
			if err != nil {
				return nil, fmt.Errorf("lake: field %s: %w", f.Name, err)
			}
			row[f.Name] = cv
		}
		out = append(out, row)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("lake: read data file: %w", err)
	}
	return out, nil
}

// Op is a scan predicate operator.
type Op string

// Predicate operators.
const (
	OpEq  Op = "="
	OpNe  Op = "!="
	OpLt  Op = "<"
	OpLte Op = "<="
	OpGt  Op = ">"
	OpGte Op = ">="
	OpIn  Op = "in"
)

// Filter is one scan predicate. Filters combine with AND, which is all the
// pushdown logic below can reason about safely; an OR would need a predicate
// tree and would skip nothing without one.
type Filter struct {
	Field  string
	Op     Op
	Value  any
	Values []any
}

// Eq builds an equality predicate.
func Eq(field string, v any) Filter { return Filter{Field: field, Op: OpEq, Value: v} }

// Ne builds an inequality predicate. It never skips a file, because a file can
// hold matching and non-matching rows at once.
func Ne(field string, v any) Filter { return Filter{Field: field, Op: OpNe, Value: v} }

// Lt builds a strictly-less-than predicate.
func Lt(field string, v any) Filter { return Filter{Field: field, Op: OpLt, Value: v} }

// Lte builds a less-than-or-equal predicate.
func Lte(field string, v any) Filter { return Filter{Field: field, Op: OpLte, Value: v} }

// Gt builds a strictly-greater-than predicate.
func Gt(field string, v any) Filter { return Filter{Field: field, Op: OpGt, Value: v} }

// Gte builds a greater-than-or-equal predicate.
func Gte(field string, v any) Filter { return Filter{Field: field, Op: OpGte, Value: v} }

// In builds a set-membership predicate.
func In(field string, vs ...any) Filter { return Filter{Field: field, Op: OpIn, Values: vs} }

// ScanResult is the outcome of a read. FilesScanned and FilesSkipped are
// reported rather than logged because "the predicate skipped 6 of 8 files" is
// the number that tells you whether the partitioning is earning its keep.
type ScanResult struct {
	Rows         []Row
	SnapshotID   int64
	FilesScanned int
	FilesSkipped int
	RowsScanned  int64
}

// Table is an Iceberg-shaped table on the local filesystem.
type Table struct {
	mu     sync.Mutex
	dir    string
	name   string
	clock  obs.Clock
	format FileFormat
	meta   Metadata
}

// TableOptions configure a table. The zero value is a table on the real clock
// writing gzipped NDJSON.
type TableOptions struct {
	Clock  obs.Clock
	Format FileFormat
	// Properties are written into the table metadata unchanged.
	Properties map[string]string
}

func (o TableOptions) resolve() TableOptions {
	if o.Clock == nil {
		o.Clock = obs.RealClock{}
	}
	if o.Format == nil {
		o.Format = NDJSONGzip{}
	}
	return o
}

// CreateTable creates a table under root/name. It is an error if metadata is
// already there; use OpenTable, or LoadOrCreateTable when either is acceptable.
func CreateTable(ctx context.Context, root, name string, schema Schema, spec PartitionSpec, opts TableOptions) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts = opts.resolve()
	dir := filepath.Join(root, name)
	if _, err := os.Stat(filepath.Join(dir, "metadata", "v1.metadata.json")); err == nil {
		return nil, fmt.Errorf("%w: %s", ErrTableExists, name)
	}
	for _, sub := range []string{"metadata", "data"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("lake: create table dir: %w", err)
		}
	}
	last := 0
	for _, f := range schema.Fields {
		if f.ID > last {
			last = f.ID
		}
	}
	props := map[string]string{"write.format.default": opts.Format.Name()}
	for k, v := range opts.Properties {
		props[k] = v
	}
	t := &Table{
		dir:    dir,
		name:   name,
		clock:  opts.Clock,
		format: opts.Format,
		meta: Metadata{
			FormatVersion:   2,
			TableName:       name,
			Location:        dir,
			LastUpdatedMS:   opts.Clock.Now().UnixMilli(),
			LastColumnID:    last,
			Schemas:         []Schema{schema},
			CurrentSchemaID: schema.SchemaID,
			PartitionSpecs:  []PartitionSpec{spec},
			DefaultSpecID:   spec.SpecID,
			Properties:      props,
		},
	}
	if err := t.writeMetadata(); err != nil {
		return nil, err
	}
	return t, nil
}

// OpenTable opens an existing table.
func OpenTable(ctx context.Context, root, name string, opts TableOptions) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts = opts.resolve()
	dir := filepath.Join(root, name)
	body, err := os.ReadFile(filepath.Join(dir, "metadata", "v1.metadata.json"))
	if err != nil {
		return nil, fmt.Errorf("lake: open table %s: %w", name, err)
	}
	t := &Table{dir: dir, name: name, clock: opts.Clock, format: opts.Format}
	if err := json.Unmarshal(body, &t.meta); err != nil {
		return nil, fmt.Errorf("lake: decode table metadata: %w", err)
	}
	t.meta.Location = dir
	return t, nil
}

// LoadOrCreateTable opens the table if it exists and creates it otherwise.
func LoadOrCreateTable(ctx context.Context, root, name string, schema Schema, spec PartitionSpec, opts TableOptions) (*Table, error) {
	t, err := OpenTable(ctx, root, name, opts)
	if err == nil {
		return t, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return CreateTable(ctx, root, name, schema, spec, opts)
}

// Name returns the table name.
func (t *Table) Name() string { return t.name }

// Location returns the table's directory.
func (t *Table) Location() string { return t.dir }

// Schema returns the current schema.
func (t *Table) Schema() Schema {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentSchema()
}

// Spec returns the default partition spec.
func (t *Table) Spec() PartitionSpec {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.currentSpec()
}

// Snapshots returns every retained snapshot, oldest first.
func (t *Table) Snapshots() []Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]Snapshot(nil), t.meta.Snapshots...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Current returns the current snapshot.
func (t *Table) Current() (Snapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotByID(t.meta.CurrentSnapshotID)
}

// CurrentSnapshotID returns the id of the current snapshot, or zero.
func (t *Table) CurrentSnapshotID() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.meta.CurrentSnapshotID
}

// SnapshotAsOf returns the newest snapshot committed at or before ts. This is
// time travel by wall clock rather than by id, and it is the form a
// reconciliation asks for: "what did the table hold at the risk cut".
func (t *Table) SnapshotAsOf(ts time.Time) (Snapshot, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := ts.UnixMilli()
	var best Snapshot
	found := false
	for _, s := range t.meta.Snapshots {
		if s.TimestampMS <= ms && (!found || s.TimestampMS > best.TimestampMS || (s.TimestampMS == best.TimestampMS && s.ID > best.ID)) {
			best, found = s, true
		}
	}
	return best, found
}

// Append writes rows as new data files and commits a snapshot. Rows are grouped
// by partition value, one file per partition per commit.
func (t *Table) Append(ctx context.Context, rows []Row) (Snapshot, error) {
	return t.commit(ctx, rows, OpAppend, false)
}

// Overwrite replaces the table's contents with rows in a single snapshot. The
// previous data files stay on disk and stay reachable by time travel until
// ExpireSnapshots removes them, which is what makes an overwrite reversible.
func (t *Table) Overwrite(ctx context.Context, rows []Row) (Snapshot, error) {
	return t.commit(ctx, rows, OpOverwrite, true)
}

func (t *Table) commit(ctx context.Context, rows []Row, op string, replaceAll bool) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	schema := t.currentSchema()
	spec := t.currentSpec()
	snapID := t.meta.LastSnapshotID + 1

	groups, order := groupByPartition(rows, schema, spec)
	entries := make([]ManifestEntry, 0, len(order))
	var addedRows int64
	for i, key := range order {
		part := groups[key]
		rel := filepath.Join("data", partitionPath(spec, part.values), fmt.Sprintf("%08d-%04d%s", snapID, i, t.format.Extension()))
		abs := filepath.Join(t.dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return Snapshot{}, fmt.Errorf("lake: create data dir: %w", err)
		}
		f, err := os.Create(abs)
		if err != nil {
			return Snapshot{}, fmt.Errorf("lake: create data file: %w", err)
		}
		if err := t.format.Write(f, schema, part.rows); err != nil {
			_ = f.Close()
			return Snapshot{}, err
		}
		if err := f.Close(); err != nil {
			return Snapshot{}, fmt.Errorf("lake: close data file: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return Snapshot{}, fmt.Errorf("lake: stat data file: %w", err)
		}
		lower, upper := bounds(schema, part.rows)
		entries = append(entries, ManifestEntry{
			Path:            rel,
			Partition:       part.values,
			RecordCount:     int64(len(part.rows)),
			FileSizeBytes:   info.Size(),
			LowerBounds:     lower,
			UpperBounds:     upper,
			AddedSnapshotID: snapID,
			Status:          StatusAdded,
		})
		addedRows += int64(len(part.rows))
	}

	var list []ManifestFile
	if !replaceAll {
		prev, err := t.manifestListLocked(t.meta.CurrentSnapshotID)
		if err != nil && !errors.Is(err, ErrNoSnapshot) {
			return Snapshot{}, err
		}
		list = append(list, prev...)
	}
	if len(entries) > 0 {
		manifestRel := filepath.Join("metadata", fmt.Sprintf("manifest-%08d.json", snapID))
		if err := writeJSON(filepath.Join(t.dir, manifestRel), entries); err != nil {
			return Snapshot{}, err
		}
		list = append(list, ManifestFile{
			Path:            manifestRel,
			AddedSnapshotID: snapID,
			AddedFilesCount: len(entries),
			AddedRowsCount:  addedRows,
		})
	}
	listRel := filepath.Join("metadata", fmt.Sprintf("snap-%08d-manifest-list.json", snapID))
	if err := writeJSON(filepath.Join(t.dir, listRel), list); err != nil {
		return Snapshot{}, err
	}

	now := t.clock.Now()
	total := addedRows
	if !replaceAll {
		for _, mf := range list {
			if mf.AddedSnapshotID != snapID {
				total += mf.AddedRowsCount
			}
		}
	}
	snap := Snapshot{
		ID:           snapID,
		ParentID:     t.meta.CurrentSnapshotID,
		TimestampMS:  now.UnixMilli(),
		Operation:    op,
		ManifestList: listRel,
		Summary: map[string]string{
			"added-data-files": strconv.Itoa(len(entries)),
			"added-records":    strconv.FormatInt(addedRows, 10),
			"total-records":    strconv.FormatInt(total, 10),
			"total-data-files": strconv.Itoa(countFiles(list)),
		},
	}
	t.meta.Snapshots = append(t.meta.Snapshots, snap)
	t.meta.CurrentSnapshotID = snapID
	t.meta.LastSnapshotID = snapID
	t.meta.LastUpdatedMS = now.UnixMilli()
	if err := t.writeMetadata(); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// Scan reads the current snapshot, skipping any data file whose manifest bounds
// prove it cannot satisfy the filters.
func (t *Table) Scan(ctx context.Context, filters ...Filter) (ScanResult, error) {
	t.mu.Lock()
	id := t.meta.CurrentSnapshotID
	t.mu.Unlock()
	return t.ScanSnapshot(ctx, id, filters...)
}

// TimeTravel reads the table exactly as the named snapshot left it.
func (t *Table) TimeTravel(ctx context.Context, snapshotID int64, filters ...Filter) (ScanResult, error) {
	return t.ScanSnapshot(ctx, snapshotID, filters...)
}

// ScanSnapshot reads a named snapshot with predicate pushdown.
func (t *Table) ScanSnapshot(ctx context.Context, snapshotID int64, filters ...Filter) (ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return ScanResult{}, err
	}
	t.mu.Lock()
	if snapshotID == 0 {
		t.mu.Unlock()
		return ScanResult{}, ErrNoSnapshot
	}
	if _, ok := t.snapshotByID(snapshotID); !ok {
		t.mu.Unlock()
		return ScanResult{}, fmt.Errorf("%w: %d", ErrUnknownSnapshot, snapshotID)
	}
	schema := t.currentSchema()
	spec := t.currentSpec()
	entries, err := t.entriesLocked(snapshotID)
	format := t.format
	dir := t.dir
	t.mu.Unlock()
	if err != nil {
		return ScanResult{}, err
	}
	for _, f := range filters {
		if _, ok := schema.Field(f.Field); !ok {
			return ScanResult{}, fmt.Errorf("%w: %s", ErrUnknownField, f.Field)
		}
	}

	res := ScanResult{SnapshotID: snapshotID}
	for _, e := range entries {
		if skipFile(schema, spec, e, filters) {
			res.FilesSkipped++
			continue
		}
		res.FilesScanned++
		f, err := os.Open(filepath.Join(dir, e.Path))
		if err != nil {
			return ScanResult{}, fmt.Errorf("lake: open %s: %w", e.Path, err)
		}
		rows, err := format.Read(f, schema)
		_ = f.Close()
		if err != nil {
			return ScanResult{}, err
		}
		res.RowsScanned += int64(len(rows))
		for _, row := range rows {
			if matchesRow(schema, row, filters) {
				res.Rows = append(res.Rows, row)
			}
		}
	}
	return res, nil
}

// Entries returns the manifest entries live in a snapshot, which is what a
// planner reads before it opens anything.
func (t *Table) Entries(snapshotID int64) ([]ManifestEntry, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entriesLocked(snapshotID)
}

// ExpireSnapshots drops every snapshot committed before the given time, keeping
// the current one whatever its age, and deletes the manifests and data files no
// retained snapshot still references. It returns the number of snapshots
// expired. This is the operation that makes storage bounded, and it is also the
// operation that ends time travel, so the cutoff is a retention decision rather
// than a cleanup detail.
func (t *Table) ExpireSnapshots(before time.Time) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ms := before.UnixMilli()

	var keep, drop []Snapshot
	for _, s := range t.meta.Snapshots {
		if s.TimestampMS < ms && s.ID != t.meta.CurrentSnapshotID {
			drop = append(drop, s)
			continue
		}
		keep = append(keep, s)
	}
	if len(drop) == 0 {
		return 0, nil
	}

	liveManifests := map[string]bool{}
	liveData := map[string]bool{}
	for _, s := range keep {
		list, err := t.manifestListLocked(s.ID)
		if err != nil {
			return 0, err
		}
		for _, mf := range list {
			liveManifests[mf.Path] = true
			entries, err := t.readManifest(mf.Path)
			if err != nil {
				return 0, err
			}
			for _, e := range entries {
				liveData[e.Path] = true
			}
		}
	}

	sort.Slice(drop, func(i, j int) bool { return drop[i].ID < drop[j].ID })
	for _, s := range drop {
		list, err := t.manifestListLocked(s.ID)
		if err != nil {
			return 0, err
		}
		for _, mf := range list {
			entries, err := t.readManifest(mf.Path)
			if err != nil {
				return 0, err
			}
			for _, e := range entries {
				if !liveData[e.Path] {
					_ = os.Remove(filepath.Join(t.dir, e.Path))
				}
			}
			if !liveManifests[mf.Path] {
				_ = os.Remove(filepath.Join(t.dir, mf.Path))
			}
		}
		_ = os.Remove(filepath.Join(t.dir, s.ManifestList))
	}

	t.meta.Snapshots = keep
	t.meta.LastUpdatedMS = t.clock.Now().UnixMilli()
	if err := t.writeMetadata(); err != nil {
		return 0, err
	}
	return len(drop), nil
}

// Compact rewrites the small files of each partition into one file per
// partition and commits a replace snapshot. Files already at or above
// targetBytes are left alone. The row content is unchanged; what changes is the
// number of files a scan has to open, which is the cost that grows fastest in a
// streaming table.
func (t *Table) Compact(ctx context.Context, targetBytes int64) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	t.mu.Lock()
	current := t.meta.CurrentSnapshotID
	if current == 0 {
		t.mu.Unlock()
		return Snapshot{}, ErrNoSnapshot
	}
	entries, err := t.entriesLocked(current)
	schema := t.currentSchema()
	format := t.format
	dir := t.dir
	t.mu.Unlock()
	if err != nil {
		return Snapshot{}, err
	}

	// Only partitions with more than one small file are worth rewriting.
	byPart := map[string][]ManifestEntry{}
	var order []string
	for _, e := range entries {
		k := partitionKey(e.Partition)
		if _, seen := byPart[k]; !seen {
			order = append(order, k)
		}
		byPart[k] = append(byPart[k], e)
	}
	sort.Strings(order)

	var rows []Row
	rewritten := 0
	for _, k := range order {
		group := byPart[k]
		small := 0
		for _, e := range group {
			if e.FileSizeBytes < targetBytes {
				small++
			}
		}
		if small < 2 {
			// Nothing to gain: read the files back so the replace snapshot
			// still describes the whole table.
			for _, e := range group {
				got, err := readEntry(dir, format, schema, e)
				if err != nil {
					return Snapshot{}, err
				}
				rows = append(rows, got...)
			}
			continue
		}
		for _, e := range group {
			got, err := readEntry(dir, format, schema, e)
			if err != nil {
				return Snapshot{}, err
			}
			rows = append(rows, got...)
			rewritten++
		}
	}
	if rewritten == 0 {
		return Snapshot{}, nil
	}
	return t.commit(ctx, rows, OpReplace, true)
}

func readEntry(dir string, format FileFormat, schema Schema, e ManifestEntry) ([]Row, error) {
	f, err := os.Open(filepath.Join(dir, e.Path))
	if err != nil {
		return nil, fmt.Errorf("lake: open %s: %w", e.Path, err)
	}
	defer func() { _ = f.Close() }()
	return format.Read(f, schema)
}

func (t *Table) currentSchema() Schema {
	for _, s := range t.meta.Schemas {
		if s.SchemaID == t.meta.CurrentSchemaID {
			return s
		}
	}
	if len(t.meta.Schemas) > 0 {
		return t.meta.Schemas[0]
	}
	return Schema{}
}

func (t *Table) currentSpec() PartitionSpec {
	for _, s := range t.meta.PartitionSpecs {
		if s.SpecID == t.meta.DefaultSpecID {
			return s
		}
	}
	if len(t.meta.PartitionSpecs) > 0 {
		return t.meta.PartitionSpecs[0]
	}
	return PartitionSpec{}
}

func (t *Table) snapshotByID(id int64) (Snapshot, bool) {
	for _, s := range t.meta.Snapshots {
		if s.ID == id {
			return s, true
		}
	}
	return Snapshot{}, false
}

func (t *Table) manifestListLocked(id int64) ([]ManifestFile, error) {
	if id == 0 {
		return nil, ErrNoSnapshot
	}
	s, ok := t.snapshotByID(id)
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrUnknownSnapshot, id)
	}
	var list []ManifestFile
	if err := readJSON(filepath.Join(t.dir, s.ManifestList), &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (t *Table) readManifest(rel string) ([]ManifestEntry, error) {
	var entries []ManifestEntry
	if err := readJSON(filepath.Join(t.dir, rel), &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func (t *Table) entriesLocked(id int64) ([]ManifestEntry, error) {
	list, err := t.manifestListLocked(id)
	if err != nil {
		return nil, err
	}
	var out []ManifestEntry
	for _, mf := range list {
		entries, err := t.readManifest(mf.Path)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.Status != StatusDeleted {
				out = append(out, e)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (t *Table) writeMetadata() error {
	return writeJSON(filepath.Join(t.dir, "metadata", "v1.metadata.json"), t.meta)
}

type partitionGroup struct {
	values map[string]string
	rows   []Row
}

func groupByPartition(rows []Row, schema Schema, spec PartitionSpec) (map[string]*partitionGroup, []string) {
	groups := map[string]*partitionGroup{}
	var order []string
	for _, row := range rows {
		values := partitionValues(schema, spec, row)
		k := partitionKey(values)
		g, ok := groups[k]
		if !ok {
			g = &partitionGroup{values: values}
			groups[k] = g
			order = append(order, k)
		}
		g.rows = append(g.rows, row)
	}
	sort.Strings(order)
	return groups, order
}

func partitionValues(schema Schema, spec PartitionSpec, row Row) map[string]string {
	if len(spec.Fields) == 0 {
		return nil
	}
	out := make(map[string]string, len(spec.Fields))
	for _, pf := range spec.Fields {
		src, ok := schema.FieldByID(pf.SourceID)
		if !ok {
			out[pf.Name] = "null"
			continue
		}
		out[pf.Name] = applyTransform(pf.Transform, src.Type, row[src.Name])
	}
	return out
}

func applyTransform(tr Transform, ft FieldType, v any) string {
	if v == nil {
		return "null"
	}
	switch tr {
	case TransformDay:
		if ts, ok := v.(time.Time); ok {
			return ts.UTC().Format("2006-01-02")
		}
	case TransformHour:
		if ts, ok := v.(time.Time); ok {
			return ts.UTC().Format("2006-01-02-15")
		}
	}
	return boundOf(ft, v)
}

func partitionKey(values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('/')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(values[k])
	}
	return sb.String()
}

func partitionPath(spec PartitionSpec, values map[string]string) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(spec.Fields))
	for _, pf := range spec.Fields {
		parts = append(parts, pf.Name+"="+sanitizeSegment(values[pf.Name]))
	}
	return filepath.Join(parts...)
}

func sanitizeSegment(s string) string {
	if s == "" {
		return "null"
	}
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func countFiles(list []ManifestFile) int {
	n := 0
	for _, mf := range list {
		n += mf.AddedFilesCount
	}
	return n
}

func writeJSON(path string, v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("lake: encode %s: %w", filepath.Base(path), err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("lake: create dir for %s: %w", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o644); err != nil {
		return fmt.Errorf("lake: write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func readJSON(path string, v any) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("lake: read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("lake: decode %s: %w", filepath.Base(path), err)
	}
	return nil
}
