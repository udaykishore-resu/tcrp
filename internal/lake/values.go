package lake

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// encodeValue renders a value in its schema type's canonical JSON form.
func encodeValue(ft FieldType, v any) (json.RawMessage, error) {
	switch ft {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lake: %T is not a string", v)
		}
		return json.Marshal(s)
	case TypeLong:
		n, err := asInt64(v)
		if err != nil {
			return nil, err
		}
		return json.Marshal(n)
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("lake: %T is not a boolean", v)
		}
		return json.Marshal(b)
	case TypeTimestamp:
		ts, ok := v.(time.Time)
		if !ok {
			return nil, fmt.Errorf("lake: %T is not a time.Time", v)
		}
		return json.Marshal(ts.UTC().Format(time.RFC3339Nano))
	default:
		return nil, fmt.Errorf("lake: unsupported field type %q", ft)
	}
}

// coerceValue converts a decoded JSON value to the Go type the schema promises.
func coerceValue(ft FieldType, v any) (any, error) {
	switch ft {
	case TypeString:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lake: %T is not a string", v)
		}
		return s, nil
	case TypeLong:
		return asInt64(v)
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("lake: %T is not a boolean", v)
		}
		return b, nil
	case TypeTimestamp:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("lake: %T is not a timestamp string", v)
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, fmt.Errorf("lake: parse timestamp %q: %w", s, err)
		}
		return ts.UTC(), nil
	default:
		return nil, fmt.Errorf("lake: unsupported field type %q", ft)
	}
}

func asInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case int32:
		return int64(n), nil
	case uint64:
		return int64(n), nil
	case json.Number:
		return n.Int64()
	case float64:
		// Only reachable when a caller hands us a float; refuse rather than
		// silently truncating, because these columns carry share counts.
		if n != float64(int64(n)) {
			return 0, fmt.Errorf("lake: %v is not an integer", n)
		}
		return int64(n), nil
	default:
		return 0, fmt.Errorf("lake: %T is not an integer", v)
	}
}

// boundOf renders a value as the canonical string a manifest stores. The
// encodings are ordered consistently with compareBound, which is the only
// property the pushdown logic relies on.
func boundOf(ft FieldType, v any) string {
	switch ft {
	case TypeLong:
		n, err := asInt64(v)
		if err != nil {
			return ""
		}
		return strconv.FormatInt(n, 10)
	case TypeBoolean:
		b, ok := v.(bool)
		if !ok {
			return ""
		}
		return strconv.FormatBool(b)
	case TypeTimestamp:
		ts, ok := v.(time.Time)
		if !ok {
			return ""
		}
		return ts.UTC().Format(time.RFC3339Nano)
	default:
		s, ok := v.(string)
		if !ok {
			return ""
		}
		return s
	}
}

// compareBound orders two canonical bound strings for a field type.
func compareBound(ft FieldType, a, b string) int {
	switch ft {
	case TypeLong:
		x, errX := strconv.ParseInt(a, 10, 64)
		y, errY := strconv.ParseInt(b, 10, 64)
		if errX != nil || errY != nil {
			return strings.Compare(a, b)
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		default:
			return 0
		}
	case TypeTimestamp:
		x, errX := time.Parse(time.RFC3339Nano, a)
		y, errY := time.Parse(time.RFC3339Nano, b)
		if errX != nil || errY != nil {
			return strings.Compare(a, b)
		}
		switch {
		case x.Before(y):
			return -1
		case x.After(y):
			return 1
		default:
			return 0
		}
	default:
		return strings.Compare(a, b)
	}
}

// bounds computes the per-column minimum and maximum over a file's rows. A
// column absent from every row gets no bound, and a scan cannot skip on it.
func bounds(schema Schema, rows []Row) (lower, upper map[string]string) {
	lower, upper = map[string]string{}, map[string]string{}
	for _, f := range schema.Fields {
		var lo, hi string
		seen := false
		for _, row := range rows {
			v, ok := row[f.Name]
			if !ok || v == nil {
				continue
			}
			b := boundOf(f.Type, v)
			if b == "" {
				continue
			}
			if !seen {
				lo, hi, seen = b, b, true
				continue
			}
			if compareBound(f.Type, b, lo) < 0 {
				lo = b
			}
			if compareBound(f.Type, b, hi) > 0 {
				hi = b
			}
		}
		if seen {
			lower[f.Name], upper[f.Name] = lo, hi
		}
	}
	if len(lower) == 0 {
		lower = nil
	}
	if len(upper) == 0 {
		upper = nil
	}
	return lower, upper
}

// skipFile reports whether a data file can be excluded without reading it.
// It reasons from the partition value first, which is exact, and from the
// column bounds second, which are conservative: a file is skipped only when the
// bounds prove no row in it can match.
func skipFile(schema Schema, spec PartitionSpec, e ManifestEntry, filters []Filter) bool {
	for _, flt := range filters {
		f, ok := schema.Field(flt.Field)
		if !ok {
			continue
		}
		if skipByPartition(schema, spec, e, f, flt) {
			return true
		}
		if skipByBounds(f, e, flt) {
			return true
		}
	}
	return false
}

func skipByPartition(schema Schema, spec PartitionSpec, e ManifestEntry, f Field, flt Filter) bool {
	if flt.Op != OpEq && flt.Op != OpIn {
		return false
	}
	for _, pf := range spec.Fields {
		if pf.Transform != TransformIdentity || pf.SourceID != f.ID {
			continue
		}
		have, ok := e.Partition[pf.Name]
		if !ok {
			continue
		}
		switch flt.Op {
		case OpEq:
			return sanitizeSegment(boundOf(f.Type, flt.Value)) != sanitizeSegment(have)
		case OpIn:
			for _, v := range flt.Values {
				if sanitizeSegment(boundOf(f.Type, v)) == sanitizeSegment(have) {
					return false
				}
			}
			return true
		}
	}
	return false
}

func skipByBounds(f Field, e ManifestEntry, flt Filter) bool {
	lo, hasLo := e.LowerBounds[f.Name]
	hi, hasHi := e.UpperBounds[f.Name]
	if !hasLo || !hasHi {
		return false
	}
	switch flt.Op {
	case OpEq:
		v := boundOf(f.Type, flt.Value)
		return compareBound(f.Type, v, lo) < 0 || compareBound(f.Type, v, hi) > 0
	case OpIn:
		for _, raw := range flt.Values {
			v := boundOf(f.Type, raw)
			if compareBound(f.Type, v, lo) >= 0 && compareBound(f.Type, v, hi) <= 0 {
				return false
			}
		}
		return len(flt.Values) > 0
	case OpGt:
		return compareBound(f.Type, hi, boundOf(f.Type, flt.Value)) <= 0
	case OpGte:
		return compareBound(f.Type, hi, boundOf(f.Type, flt.Value)) < 0
	case OpLt:
		return compareBound(f.Type, lo, boundOf(f.Type, flt.Value)) >= 0
	case OpLte:
		return compareBound(f.Type, lo, boundOf(f.Type, flt.Value)) > 0
	default:
		// A not-equals predicate never excludes a file: the file can hold both
		// matching and non-matching rows.
		return false
	}
}

// matchesRow applies the filters to a decoded row. Filters combine with AND.
func matchesRow(schema Schema, row Row, filters []Filter) bool {
	for _, flt := range filters {
		f, ok := schema.Field(flt.Field)
		if !ok {
			return false
		}
		v, present := row[f.Name]
		if !present {
			return false
		}
		got := boundOf(f.Type, v)
		switch flt.Op {
		case OpEq:
			if compareBound(f.Type, got, boundOf(f.Type, flt.Value)) != 0 {
				return false
			}
		case OpNe:
			if compareBound(f.Type, got, boundOf(f.Type, flt.Value)) == 0 {
				return false
			}
		case OpLt:
			if compareBound(f.Type, got, boundOf(f.Type, flt.Value)) >= 0 {
				return false
			}
		case OpLte:
			if compareBound(f.Type, got, boundOf(f.Type, flt.Value)) > 0 {
				return false
			}
		case OpGt:
			if compareBound(f.Type, got, boundOf(f.Type, flt.Value)) <= 0 {
				return false
			}
		case OpGte:
			if compareBound(f.Type, got, boundOf(f.Type, flt.Value)) < 0 {
				return false
			}
		case OpIn:
			hit := false
			for _, raw := range flt.Values {
				if compareBound(f.Type, got, boundOf(f.Type, raw)) == 0 {
					hit = true
					break
				}
			}
			if !hit {
				return false
			}
		}
	}
	return true
}
