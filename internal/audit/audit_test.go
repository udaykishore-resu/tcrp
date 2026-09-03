package audit

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

var seedTime = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

func newTestLog(t *testing.T, opts ...Option) (*Log, *obs.ManualClock) {
	t.Helper()
	clock := obs.NewManualClock(seedTime)
	l := New(clock, opts...)
	t.Cleanup(func() { _ = l.Close() })
	return l, clock
}

// appendSample writes a short, representative chain and returns it.
func appendSample(t *testing.T, l *Log, clock *obs.ManualClock) []Record {
	t.Helper()
	for _, r := range []Record{
		{Actor: "svc/collateral-agent", OnBehalf: "j.okafor", Tenant: "collateral-ops",
			Action: ActionRunStarted, Subject: "exposure to CP-DELTA", Outcome: OutcomeOK, RunID: "run_1"},
		{Actor: "svc/collateral-agent", Tenant: "collateral-ops", Action: ActionPolicyDecision,
			Subject: "get_exposure", Outcome: OutcomeAllowed, RunID: "run_1",
			Details: map[string]any{"rule": "allow-read-only", "latency_us": 42}},
		{Actor: "s.aldridge", Tenant: "collateral-ops", Action: ActionApprovalGranted,
			Subject: "execute_transfer", Outcome: OutcomeOK, RunID: "run_1"},
		{Actor: "svc/collateral-agent", Tenant: "collateral-ops", Action: ActionTransferExecuted,
			Subject: "execute_transfer", Outcome: OutcomeOK, RunID: "run_1"},
	} {
		if _, err := l.Append(r); err != nil {
			t.Fatalf("Append: %v", err)
		}
		clock.Advance(time.Millisecond)
	}
	return l.Records()
}

// TestChainVerifiesOnAGoodChain proves an untouched chain verifies, that the
// sequence, times and links are filled in by the log rather than the caller,
// and that signatures are checked when the log is signing.
func TestChainVerifiesOnAGoodChain(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	l, clock := newTestLog(t, WithSigning(priv))
	recs := appendSample(t, l, clock)

	if err := l.Verify(); err != nil {
		t.Fatalf("a freshly written chain must verify: %v", err)
	}
	if len(recs) != 4 || l.Len() != 4 {
		t.Fatalf("chain holds %d records", l.Len())
	}
	if recs[0].PrevHash != GenesisHash {
		t.Fatalf("the first record must link to genesis, got %s", recs[0].PrevHash)
	}
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("record %d has seq %d", i, r.Seq)
		}
		if r.Hash == "" || r.Signature == "" {
			t.Fatalf("record %d is missing a hash or signature", r.Seq)
		}
		if i > 0 && r.PrevHash != recs[i-1].Hash {
			t.Fatalf("record %d does not link to its predecessor", r.Seq)
		}
	}
	if got := l.Head(); got != recs[len(recs)-1].Hash {
		t.Fatalf("Head = %s, want the last record's hash", got)
	}
	if recs[0].Time != seedTime {
		t.Fatalf("the clock, not the caller, stamps the time: %v", recs[0].Time)
	}
	if len(l.ByRun("run_1")) != 4 || len(l.ByRun("run_2")) != 0 {
		t.Fatalf("ByRun returned %d records", len(l.ByRun("run_1")))
	}
	if err := VerifyRecords(l.Records(), l.PublicKey()); err != nil {
		t.Fatalf("VerifyRecords on the exported copy: %v", err)
	}
	if err := VerifyRecords(nil, l.PublicKey()); err != nil {
		t.Fatalf("an empty chain is trivially valid, got %v", err)
	}
}

// TestChainDetectsTampering is the point of the package. Each case corrupts the
// chain in a different way and the check is not merely that verification fails
// but that the error names the sequence number where it broke, which is what an
// auditor needs in order to look at the record.
func TestChainDetectsTampering(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)

	cases := []struct {
		name    string
		mutate  func([]Record) []Record
		wantSeq string
	}{
		{
			name: "a field is edited",
			mutate: func(r []Record) []Record {
				r[2].Outcome = OutcomeAllowed
				return r
			},
			wantSeq: "seq 3",
		},
		{
			name: "a detail inside the payload is edited",
			mutate: func(r []Record) []Record {
				r[1].Details["rule"] = "allow-everything"
				return r
			},
			wantSeq: "seq 2",
		},
		{
			name: "the actor is swapped to hide who acted",
			mutate: func(r []Record) []Record {
				r[3].Actor = "s.aldridge"
				return r
			},
			wantSeq: "seq 4",
		},
		{
			name: "two records are reordered",
			mutate: func(r []Record) []Record {
				r[1], r[2] = r[2], r[1]
				return r
			},
			wantSeq: "record 2",
		},
		{
			name: "a record is deleted",
			mutate: func(r []Record) []Record {
				return append(r[:2:2], r[3:]...)
			},
			wantSeq: "record 3",
		},
		{
			name: "a record is inserted with a recomputed hash",
			mutate: func(r []Record) []Record {
				forged := Record{
					Seq: 5, Time: seedTime, Actor: "attacker", Tenant: "collateral-ops",
					Action: ActionTransferExecuted, Subject: "execute_transfer",
					Outcome: OutcomeOK, RunID: "run_1", PrevHash: r[3].Hash,
				}
				forged.Hash = forged.ComputeHash()
				forged.Signature = hex.EncodeToString(ed25519.Sign(priv, []byte("not this hash")))
				return append(r, forged)
			},
			wantSeq: "seq 5 signature invalid",
		},
		{
			name: "a signature is forged with the wrong key",
			mutate: func(r []Record) []Record {
				_, other, _ := ed25519.GenerateKey(rand.Reader)
				r[1].Signature = hex.EncodeToString(ed25519.Sign(other, []byte(r[1].Hash)))
				return r
			},
			wantSeq: "seq 2 signature invalid",
		},
		{
			name: "a hash is replaced with a plausible-looking one",
			mutate: func(r []Record) []Record {
				r[0].Hash = strings.Repeat("ab", 32)
				return r
			},
			wantSeq: "seq 1 content hash",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, clock := newTestLog(t, WithSigning(priv))
			recs := appendSample(t, l, clock)
			if err := VerifyRecords(recs, pub); err != nil {
				t.Fatalf("precondition: the untouched copy must verify, got %v", err)
			}
			broken := c.mutate(recs)
			err := VerifyRecords(broken, pub)
			if err == nil {
				t.Fatalf("%s went undetected", c.name)
			}
			if !errors.Is(err, ErrChainBroken) {
				t.Fatalf("error is not ErrChainBroken: %v", err)
			}
			if !strings.Contains(err.Error(), c.wantSeq) {
				t.Fatalf("error %q does not name %q", err, c.wantSeq)
			}
		})
	}
}

// TestCanonicalJSONIsStableAcrossMapOrderings proves the hashed encoding does
// not depend on Go's map iteration order: two maps with the same content, built
// in different orders and nested, encode identically and therefore hash
// identically.
func TestCanonicalJSONIsStableAcrossMapOrderings(t *testing.T) {
	a := map[string]any{
		"zulu":  1,
		"alpha": "x",
		"nested": map[string]any{
			"gamma": true,
			"beta":  []any{map[string]any{"b": 2, "a": 1}},
		},
	}
	b := map[string]any{
		"nested": map[string]any{
			"beta":  []any{map[string]any{"a": 1, "b": 2}},
			"gamma": true,
		},
		"alpha": "x",
		"zulu":  1,
	}
	want := `{"alpha":"x","nested":{"beta":[{"a":1,"b":2}],"gamma":true},"zulu":1}`
	for i := 0; i < 50; i++ {
		if got := CanonicalJSON(a); got != want {
			t.Fatalf("CanonicalJSON(a) = %s, want %s", got, want)
		}
		if CanonicalJSON(a) != CanonicalJSON(b) {
			t.Fatalf("two orderings of the same map encoded differently:\n%s\n%s",
				CanonicalJSON(a), CanonicalJSON(b))
		}
	}
	if got := CanonicalJSON(nil); got != "" {
		t.Fatalf("CanonicalJSON(nil) = %q, want empty", got)
	}

	// The record hash inherits that stability, so a re-serialised record with a
	// differently-ordered details map still verifies.
	r1 := Record{Seq: 1, Time: seedTime, Action: ActionPolicyDecision, PrevHash: GenesisHash, Details: a}
	r2 := Record{Seq: 1, Time: seedTime, Action: ActionPolicyDecision, PrevHash: GenesisHash, Details: b}
	if r1.ComputeHash() != r2.ComputeHash() {
		t.Fatal("record hashes depend on map ordering")
	}

	// A different time is a different record, so the hash is not degenerate.
	r3 := r1
	r3.Time = seedTime.Add(time.Nanosecond)
	if r1.ComputeHash() == r3.ComputeHash() {
		t.Fatal("the hash does not cover the time")
	}
}

// TestFileSinkRoundTripsAndVerifiesOffline proves the on-disk journal is the
// same chain, and that VerifyRecords accepts it with nothing but a public key —
// which is the whole claim of an exportable audit trail.
func TestFileSinkRoundTripsAndVerifiesOffline(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	clock := obs.NewManualClock(seedTime)
	l := New(clock, WithFile(path), WithSigning(priv))
	written := appendSample(t, l, clock)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fromDisk := readJSONL(t, path)
	if len(fromDisk) != len(written) {
		t.Fatalf("the file holds %d records, the log wrote %d", len(fromDisk), len(written))
	}
	for i := range written {
		if fromDisk[i].Hash != written[i].Hash || fromDisk[i].Signature != written[i].Signature {
			t.Fatalf("record %d differs on disk", i+1)
		}
	}
	if err := VerifyRecords(fromDisk, pub); err != nil {
		t.Fatalf("the exported file must verify against the public key alone: %v", err)
	}

	// Export produces the same bytes as the sink did.
	var buf bytes.Buffer
	if err := l.Export(&buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	exported := decodeJSONL(t, buf.Bytes())
	if err := VerifyRecords(exported, pub); err != nil {
		t.Fatalf("Export output must verify: %v", err)
	}

	// Editing one byte of the file is caught.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte(`"outcome":"allowed"`), []byte(`"outcome":"denied!"`), 1)
	if bytes.Equal(raw, edited) {
		t.Fatal("precondition: the file should contain an allowed outcome")
	}
	tampered := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(tampered, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRecords(readJSONL(t, tampered), pub); err == nil {
		t.Fatal("an edited export must not verify")
	}

	// A closed log refuses further appends rather than losing them silently.
	if _, err := l.Append(Record{Action: ActionRunStarted}); !errors.Is(err, ErrClosed) {
		t.Fatalf("append after close = %v, want ErrClosed", err)
	}
}

func readJSONL(t *testing.T, path string) []Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return decodeJSONL(t, b)
}

func decodeJSONL(t *testing.T, b []byte) []Record {
	t.Helper()
	var out []Record
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("line is not a record: %v (%s)", err, sc.Text())
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestConcurrentAppendsKeepTheChainIntact proves the log serialises writers: a
// hundred goroutines appending at once still produce a chain with contiguous
// sequence numbers and unbroken links. Meaningful under -race.
func TestConcurrentAppendsKeepTheChainIntact(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, _ := newTestLog(t, WithSigning(priv), WithFile(path))

	const writers, each = 25, 20
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := l.Append(Record{
					Actor: "writer", Tenant: "t", Action: ActionToolInvoked,
					Subject: "get_exposure", Outcome: OutcomeOK,
					RunID:   "run_1",
					Details: map[string]any{"writer": w, "i": i},
				}); err != nil {
					t.Errorf("Append: %v", err)
					return
				}
			}
		}(w)
	}
	// Readers run alongside, which is where an unsynchronised Records() would
	// show up under the race detector.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = l.Len()
				_ = l.Head()
				_ = l.Records()
			}
		}()
	}
	wg.Wait()

	if got := l.Len(); got != writers*each {
		t.Fatalf("chain holds %d records, want %d", got, writers*each)
	}
	if err := l.Verify(); err != nil {
		t.Fatalf("concurrent appends broke the chain: %v", err)
	}
	if err := VerifyRecords(readJSONL(t, path), l.PublicKey()); err != nil {
		t.Fatalf("the file written concurrently does not verify: %v", err)
	}
}

// TestSubscribeDeliversWithoutBlockingTheAuditedPath proves a subscriber sees
// new records and that a full subscriber channel drops rather than stalling the
// writer, because the audited path must never wait on a dashboard.
func TestSubscribeDeliversWithoutBlockingTheAuditedPath(t *testing.T) {
	l, _ := newTestLog(t)
	ch := l.Subscribe(4)

	if _, err := l.Append(Record{Action: ActionRunStarted, RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch:
		if got.Action != ActionRunStarted {
			t.Fatalf("received %v", got.Action)
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber received nothing")
	}

	// Overfill the buffer; appends still return.
	for i := 0; i < 100; i++ {
		if _, err := l.Append(Record{Action: ActionToolInvoked, RunID: "run_1"}); err != nil {
			t.Fatalf("append %d blocked or failed: %v", i, err)
		}
	}
	if l.Len() != 101 {
		t.Fatalf("a slow subscriber cost records: %d", l.Len())
	}
}

// TestVerifyWithoutAKeySkipsSignaturesButNotTheChain proves the two checks are
// independent: an auditor with no key still detects an edit.
func TestVerifyWithoutAKeySkipsSignaturesButNotTheChain(t *testing.T) {
	l, clock := newTestLog(t)
	recs := appendSample(t, l, clock)
	if l.PublicKey() != nil {
		t.Fatal("an unsigned log must expose no public key")
	}
	if err := VerifyRecords(recs, nil); err != nil {
		t.Fatalf("an unsigned chain must still verify: %v", err)
	}
	recs[1].Subject = "execute_transfer"
	if err := VerifyRecords(recs, nil); err == nil {
		t.Fatal("an edit must be caught even with no signing key")
	}
}

// TestFindAndByRunFilterWithoutCopyingTheChain covers the small query surface
// the conformance invariants read the log through.
func TestFindAndByRunFilterWithoutCopyingTheChain(t *testing.T) {
	l, clock := newTestLog(t)
	appendSample(t, l, clock)
	if _, err := l.Append(Record{Action: ActionRunStarted, RunID: "run_2", Outcome: OutcomeOK}); err != nil {
		t.Fatal(err)
	}

	transfers := l.Find(func(r Record) bool { return r.Action == ActionTransferExecuted })
	if len(transfers) != 1 || transfers[0].Subject != "execute_transfer" {
		t.Fatalf("Find returned %+v", transfers)
	}
	if got := l.Find(func(Record) bool { return false }); got == nil || len(got) != 0 {
		t.Fatal("Find with no matches must return an empty slice, not nil")
	}
	if got := l.ByRun("run_2"); len(got) != 1 {
		t.Fatalf("ByRun(run_2) returned %d records", len(got))
	}

	// Records returns a copy: mutating it cannot corrupt the log.
	copyOut := l.Records()
	copyOut[0].Actor = "attacker"
	if err := l.Verify(); err != nil {
		t.Fatalf("mutating a returned copy affected the log: %v", err)
	}
}

// TestWithWriterSink proves the sink option accepts any WriteCloser, which is
// how a deployment ships records to a WORM store instead of a local file.
func TestWithWriterSink(t *testing.T) {
	sink := &closableBuffer{}
	l, clock := newTestLog(t, WithWriter(sink))
	appendSample(t, l, clock)
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !sink.closed {
		t.Fatal("Close must close the sink")
	}
	if got := decodeJSONL(t, sink.Bytes()); len(got) != 4 {
		t.Fatalf("sink received %d records", len(got))
	}
}

type closableBuffer struct {
	bytes.Buffer
	closed bool
}

func (b *closableBuffer) Close() error { b.closed = true; return nil }
