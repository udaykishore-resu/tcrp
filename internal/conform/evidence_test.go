package conform_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/conform"
)

// bundleKey returns a deterministic ed25519 key pair, so a fixture's signature
// is the same on every run and a golden digest means something.
func bundleKey(t *testing.T, seedWord string) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	copy(seed, seedWord)
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

// signedBundle runs the collateral process and seals the result.
func signedBundle(t *testing.T, ev conform.Evidence) (conform.Bundle, ed25519.PublicKey) {
	t.Helper()
	p := mustParse(t, "collateral-margin-call.bpmn")
	rep := conform.NewEngine(conform.Default(), testClock()).Run(context.Background(), p, ev)
	b, err := conform.NewBundle(testClock(), rep, ev)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	priv, pub := bundleKey(t, "conformance-signing-key")
	signed, err := b.Sign(priv)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed, pub
}

func TestBundleSignsAndVerifies(t *testing.T) {
	b, pub := signedBundle(t, agentEvidence(t))
	if err := conform.VerifyBundle(b, pub); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if b.ID == "" || len(b.ID) != 64 {
		t.Fatalf("bundle id %q is not a sha256", b.ID)
	}
	if !b.GatePassed {
		t.Fatal("clean evidence produced a bundle that did not pass the gate")
	}
	if b.ProcessDigest == "" || b.AuditHead == "" || b.PolicyBundleDigest == "" {
		t.Fatalf("bundle is missing an anchor: %+v", b)
	}
	if !strings.Contains(b.Summary(), "GATE PASSED") {
		t.Fatalf("summary: %s", b.Summary())
	}
}

func TestBundleManifestNamesEveryEvidenceArtefact(t *testing.T) {
	b, _ := signedBundle(t, settlementEvidence(t))
	got := map[string]conform.Artefact{}
	var names []string
	for _, a := range b.Manifest() {
		got[a.Name] = a
		names = append(names, a.Name)
	}
	for _, want := range []string{
		"audit.records", "otel.spans", "bridge.supply", "bridge.mints",
		"lake.quality", "lake.lineage", "ledger.evm.head", "ledger.canton.head",
	} {
		a, ok := got[want]
		if !ok {
			t.Fatalf("manifest is missing %q; it lists %v", want, names)
		}
		if a.Digest == "" {
			t.Fatalf("artefact %q carries no digest", want)
		}
	}
	// The manifest is sorted, because a bundle whose id depended on map order
	// would not be content-addressed at all.
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("manifest is not sorted: %v", names)
		}
	}
}

// TestBundleIsDeterministic is the property the whole package rests on: same
// evidence in, byte-identical digest out.
func TestBundleIsDeterministic(t *testing.T) {
	var first conform.Bundle
	for i := 0; i < 10; i++ {
		b, _ := signedBundle(t, agentEvidence(t))
		if i == 0 {
			first = b
			continue
		}
		if b.ID != first.ID {
			t.Fatalf("run %d produced bundle %s, first run produced %s", i, b.ID, first.ID)
		}
		if b.Signature != first.Signature {
			t.Fatalf("run %d produced a different signature", i)
		}
	}
}

// TestDifferentEvidenceProducesADifferentBundle is the other half of the same
// property: the id has to move when the evidence does.
func TestDifferentEvidenceProducesADifferentBundle(t *testing.T) {
	clean, _ := signedBundle(t, agentEvidence(t))

	ev := agentEvidence(t)
	ev.Records[3].Actor = "somebody.else"
	dirty, _ := signedBundle(t, ev)

	if clean.ID == dirty.ID {
		t.Fatal("altering the audit log did not change the bundle id")
	}
	if dirty.GatePassed {
		t.Fatal("a bundle over a broken chain claims the gate passed")
	}
}

func TestVerifyDetectsAnEditedVerdict(t *testing.T) {
	b, pub := signedBundle(t, agentEvidence(t))
	// The most tempting edit there is: turn the failing line green.
	b.Verdicts[0].Status = conform.StatusPass
	b.Verdicts[0].Observed = "everything is fine"
	err := conform.VerifyBundle(b, pub)
	if !errors.Is(err, conform.ErrBundleTampered) {
		t.Fatalf("error %v, want ErrBundleTampered", err)
	}
}

func TestVerifyDetectsAnEditedTally(t *testing.T) {
	b, pub := signedBundle(t, agentEvidence(t))
	b.Failed = 0
	b.Passed = 99
	if err := conform.VerifyBundle(b, pub); !errors.Is(err, conform.ErrBundleTampered) {
		t.Fatalf("error %v, want ErrBundleTampered", err)
	}
}

func TestVerifyDetectsAnEditedAuditHead(t *testing.T) {
	b, pub := signedBundle(t, agentEvidence(t))
	b.AuditHead = strings.Repeat("0", 64)
	if err := conform.VerifyBundle(b, pub); !errors.Is(err, conform.ErrBundleTampered) {
		t.Fatalf("error %v, want ErrBundleTampered", err)
	}
}

func TestVerifyRejectsTheWrongKey(t *testing.T) {
	b, _ := signedBundle(t, agentEvidence(t))
	_, other := bundleKey(t, "somebody-elses-key")
	if err := conform.VerifyBundle(b, other); !errors.Is(err, conform.ErrBundleSignature) {
		t.Fatalf("error %v, want ErrBundleSignature", err)
	}
}

func TestVerifyRefusesToVouchWithoutAKey(t *testing.T) {
	b, _ := signedBundle(t, agentEvidence(t))
	if err := conform.VerifyBundle(b, nil); !errors.Is(err, conform.ErrNoVerificationKey) {
		t.Fatalf("error %v, want ErrNoVerificationKey", err)
	}
}

func TestVerifyRejectsAnUnsignedBundle(t *testing.T) {
	ev := agentEvidence(t)
	p := mustParse(t, "collateral-margin-call.bpmn")
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(), p, ev)
	b, err := conform.NewBundle(testClock(), rep, ev)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	_, pub := bundleKey(t, "conformance-signing-key")
	if err := conform.VerifyBundle(b, pub); !errors.Is(err, conform.ErrBundleUnsigned) {
		t.Fatalf("error %v, want ErrBundleUnsigned", err)
	}
}

// TestResigningWithADifferentKeyIsADifferentBundle: the public key is inside
// the signed payload, so a bundle cannot be quietly re-attributed.
func TestResigningWithADifferentKeyIsADifferentBundle(t *testing.T) {
	ev := agentEvidence(t)
	p := mustParse(t, "collateral-margin-call.bpmn")
	rep := conform.NewEngine(nil, testClock()).Run(context.Background(), p, ev)
	b, err := conform.NewBundle(testClock(), rep, ev)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	privA, pubA := bundleKey(t, "key-a")
	privB, pubB := bundleKey(t, "key-b")
	a, err := b.Sign(privA)
	if err != nil {
		t.Fatalf("sign a: %v", err)
	}
	c, err := b.Sign(privB)
	if err != nil {
		t.Fatalf("sign b: %v", err)
	}
	if a.ID == c.ID {
		t.Fatal("two keys produced the same bundle id")
	}
	if err := conform.VerifyBundle(a, pubA); err != nil {
		t.Fatalf("verify a: %v", err)
	}
	if err := conform.VerifyBundle(c, pubB); err != nil {
		t.Fatalf("verify c: %v", err)
	}
	if err := conform.VerifyBundle(a, pubB); err == nil {
		t.Fatal("bundle a verified against key b")
	}
}

// TestBundleRoundTripsThroughJSON is the offline case: a file, a key, and this
// package, with none of the platform running.
func TestBundleRoundTripsThroughJSON(t *testing.T) {
	b, pub := signedBundle(t, settlementEvidence(t))
	var buf bytes.Buffer
	if err := conform.WriteBundle(&buf, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := conform.ReadBundle(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := conform.VerifyBundle(got, pub); err != nil {
		t.Fatalf("verify after round trip: %v", err)
	}
	if got.ID != b.ID || len(got.Verdicts) != len(b.Verdicts) {
		t.Fatalf("round trip changed the bundle: %s vs %s", got.ID, b.ID)
	}
}

// TestATamperedFileDoesNotSurviveTheRoundTrip edits the JSON on the way past,
// which is what somebody with access to the artefact store would actually do.
func TestATamperedFileDoesNotSurviveTheRoundTrip(t *testing.T) {
	ev := agentEvidence(t)
	ev.Records[3].Actor = "somebody.else"
	b, pub := signedBundle(t, ev)

	var buf bytes.Buffer
	if err := conform.WriteBundle(&buf, b); err != nil {
		t.Fatalf("write: %v", err)
	}
	edited := strings.Replace(buf.String(), `"status": "fail"`, `"status": "pass"`, 1)
	if edited == buf.String() {
		t.Fatal("fixture produced no failing verdict to edit")
	}
	got, err := conform.ReadBundle(strings.NewReader(edited))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := conform.VerifyBundle(got, pub); !errors.Is(err, conform.ErrBundleTampered) {
		t.Fatalf("error %v, want ErrBundleTampered", err)
	}
}
