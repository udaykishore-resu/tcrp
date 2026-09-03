package bridge_test

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/domain"
)

// signerFor builds a deterministic signer. Keys come from a fixed seed rather
// than crypto/rand so that a failing test can be reproduced exactly.
func signerFor(t *testing.T, id string, seed byte) *bridge.Signer {
	t.Helper()
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return bridge.NewSigner(id, ed25519.NewKeyFromSeed(s))
}

func sampleAttestation() bridge.Attestation {
	return bridge.Attestation{
		Nonce: 7, SourceLedger: domain.LedgerEVM, TargetLedger: domain.LedgerCanton,
		Symbol: domain.FundSymbol, Holder: "CP-BRAVO", Quantity: 5_000_000,
		SourceTxHash: "0xabc", SourceBlock: 21_400_042,
		ObservedAt: domain.SeedTime,
	}
}

func TestQuorumAccepts(t *testing.T) {
	a, b, c := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2), signerFor(t, "attester-c", 3)
	set, err := bridge.SignerSetFrom(2, a, b, c)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	att := sampleAttestation()
	sigs := bridge.Collect(att, []*bridge.Signer{a, b, c})
	if len(sigs) != 3 {
		t.Fatalf("collected %d signatures", len(sigs))
	}
	n, err := set.Verify(att, sigs)
	if err != nil || n != 3 {
		t.Fatalf("verify = %d, %v", n, err)
	}
	// Exactly the quorum is enough.
	if n, err := set.Verify(att, sigs[:2]); err != nil || n != 2 {
		t.Fatalf("verify at quorum = %d, %v", n, err)
	}
}

func TestQuorumShortByOneIsRejected(t *testing.T) {
	a, b, c := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2), signerFor(t, "attester-c", 3)
	set, err := bridge.SignerSetFrom(3, a, b, c)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	att := sampleAttestation()
	sigs := bridge.Collect(att, []*bridge.Signer{a, b})
	n, err := set.Verify(att, sigs)
	if !errors.Is(err, bridge.ErrQuorumNotMet) {
		t.Fatalf("a quorum one short verified: n=%d err=%v", n, err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want the two valid signatures reported", n)
	}
}

func TestSignerOutsideTheSetIsRejected(t *testing.T) {
	a, b := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2)
	stranger := signerFor(t, "attester-x", 9)
	set, err := bridge.SignerSetFrom(2, a, b)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	att := sampleAttestation()
	sigs := bridge.Collect(att, []*bridge.Signer{a, stranger})
	if _, err := set.Verify(att, sigs); !errors.Is(err, bridge.ErrUnknownSigner) {
		t.Fatalf("a signature from outside the set was accepted: %v", err)
	}
}

func TestSignerImpersonationIsRejected(t *testing.T) {
	// A signature that claims to be attester-b but was produced by another key
	// must fail: the identifier is a lookup key, not a credential.
	a, b := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2)
	set, err := bridge.SignerSetFrom(2, a, b)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	att := sampleAttestation()
	impostor := signerFor(t, "attester-b", 9)
	sigA, _ := a.Sign(att)
	sigFake, _ := impostor.Sign(att)
	if _, err := set.Verify(att, []bridge.Signature{sigA, sigFake}); !errors.Is(err, bridge.ErrBadSignature) {
		t.Fatalf("an impersonated signature was accepted: %v", err)
	}
}

func TestDuplicateSignerIsNotCountedTwice(t *testing.T) {
	a, b, c := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2), signerFor(t, "attester-c", 3)
	set, err := bridge.SignerSetFrom(2, a, b, c)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	att := sampleAttestation()
	sigA, _ := a.Sign(att)
	if _, err := set.Verify(att, []bridge.Signature{sigA, sigA}); !errors.Is(err, bridge.ErrDuplicateSigner) {
		t.Fatalf("one signer submitted twice reached a quorum of two: %v", err)
	}
}

func TestMutatedFieldBreaksVerification(t *testing.T) {
	a, b := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2)
	set, err := bridge.SignerSetFrom(2, a, b)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	att := sampleAttestation()
	sigs := bridge.Collect(att, []*bridge.Signer{a, b})

	mutations := map[string]func(bridge.Attestation) bridge.Attestation{
		"quantity": func(x bridge.Attestation) bridge.Attestation { x.Quantity++; return x },
		"holder":   func(x bridge.Attestation) bridge.Attestation { x.Holder = "CP-HOTEL"; return x },
		"nonce":    func(x bridge.Attestation) bridge.Attestation { x.Nonce++; return x },
		"symbol":   func(x bridge.Attestation) bridge.Attestation { x.Symbol = "USDC"; return x },
		"tx_hash":  func(x bridge.Attestation) bridge.Attestation { x.SourceTxHash = "0xdef"; return x },
		"block":    func(x bridge.Attestation) bridge.Attestation { x.SourceBlock++; return x },
		"observed": func(x bridge.Attestation) bridge.Attestation {
			x.ObservedAt = x.ObservedAt.Add(time.Second)
			return x
		},
		"target": func(x bridge.Attestation) bridge.Attestation {
			x.TargetLedger = domain.LedgerGolden
			return x
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := set.Verify(mutate(att), sigs); !errors.Is(err, bridge.ErrBadSignature) {
				t.Fatalf("a mutated %s verified: %v", name, err)
			}
		})
	}
}

func TestSignerSetConstruction(t *testing.T) {
	a := signerFor(t, "attester-a", 1)
	if _, err := bridge.NewSignerSet(1, nil); !errors.Is(err, bridge.ErrBadSignerSet) {
		t.Fatalf("an empty signer set was accepted: %v", err)
	}
	if _, err := bridge.SignerSetFrom(2, a); !errors.Is(err, bridge.ErrBadSignerSet) {
		t.Fatalf("a quorum larger than the set was accepted: %v", err)
	}
	if _, err := bridge.SignerSetFrom(0, a); !errors.Is(err, bridge.ErrBadSignerSet) {
		t.Fatalf("a quorum of zero was accepted: %v", err)
	}
}

func TestUnavailableSignerContributesNothing(t *testing.T) {
	a, b, c := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2), signerFor(t, "attester-c", 3)
	c.SetAvailable(false)
	sigs := bridge.Collect(sampleAttestation(), []*bridge.Signer{a, b, c})
	if len(sigs) != 2 {
		t.Fatalf("collected %d signatures from two available signers", len(sigs))
	}
	set, err := bridge.SignerSetFrom(2, a, b, c)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	if _, err := set.Verify(sampleAttestation(), sigs); err != nil {
		t.Fatalf("two of three did not reach a quorum of two: %v", err)
	}
}

func TestCollectIsOrderIndependent(t *testing.T) {
	a, b, c := signerFor(t, "attester-a", 1), signerFor(t, "attester-b", 2), signerFor(t, "attester-c", 3)
	att := sampleAttestation()
	x := bridge.Collect(att, []*bridge.Signer{c, a, b})
	y := bridge.Collect(att, []*bridge.Signer{b, c, a})
	if len(x) != len(y) {
		t.Fatalf("lengths differ")
	}
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("signature %d differs: %+v vs %+v", i, x[i], y[i])
		}
	}
}

func TestDigestIsStableAndFieldSensitive(t *testing.T) {
	att := sampleAttestation()
	if att.DigestHex() != att.DigestHex() {
		t.Fatal("digest is not stable across calls")
	}
	// Reconstructing the same attestation from its parts must give the same
	// digest, or two signers built from the same event would disagree.
	same := bridge.Attestation{
		Nonce: att.Nonce, SourceLedger: att.SourceLedger, TargetLedger: att.TargetLedger,
		Symbol: att.Symbol, Holder: att.Holder, Quantity: att.Quantity,
		SourceTxHash: att.SourceTxHash, SourceBlock: att.SourceBlock,
		ObservedAt: att.ObservedAt.In(time.FixedZone("elsewhere", 3600)),
	}
	if same.DigestHex() != att.DigestHex() {
		t.Fatal("the same attestation in a different time zone digests differently")
	}
	other := att
	other.Quantity++
	if other.DigestHex() == att.DigestHex() {
		t.Fatal("a changed quantity did not change the digest")
	}
}

func TestCanonicalJSONSortsKeysAndKeepsIntegersExact(t *testing.T) {
	b, err := bridge.CanonicalJSON(map[string]any{
		"z": int64(1), "a": "x", "m": map[string]any{"b": true, "a": uint64(1 << 62)},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := `{"a":"x","m":{"a":4611686018427387904,"b":true},"z":1}`
	if string(b) != want {
		t.Fatalf("canonical = %s\nwant       %s", b, want)
	}
	// A float has no single textual form, so it is refused rather than
	// rendered one of several ways.
	if _, err := bridge.CanonicalJSON(map[string]any{"x": 1.5}); !errors.Is(err, bridge.ErrNotCanonical) {
		t.Fatalf("a float was canonically encoded: %v", err)
	}
}

func TestAttestationValidation(t *testing.T) {
	cases := map[string]func(bridge.Attestation) bridge.Attestation{
		"no source ledger": func(a bridge.Attestation) bridge.Attestation { a.SourceLedger = ""; return a },
		"same ledger":      func(a bridge.Attestation) bridge.Attestation { a.TargetLedger = a.SourceLedger; return a },
		"no symbol":        func(a bridge.Attestation) bridge.Attestation { a.Symbol = ""; return a },
		"no holder":        func(a bridge.Attestation) bridge.Attestation { a.Holder = ""; return a },
		"zero quantity":    func(a bridge.Attestation) bridge.Attestation { a.Quantity = 0; return a },
		"negative":         func(a bridge.Attestation) bridge.Attestation { a.Quantity = -1; return a },
		"no tx":            func(a bridge.Attestation) bridge.Attestation { a.SourceTxHash = ""; return a },
	}
	for name, mutate := range cases {
		if err := mutate(sampleAttestation()).Validate(); err == nil {
			t.Fatalf("%s: validated", name)
		} else if !strings.HasPrefix(err.Error(), "bridge: ") {
			t.Fatalf("%s: error is not package-prefixed: %v", name, err)
		}
	}
	if err := sampleAttestation().Validate(); err != nil {
		t.Fatalf("a well-formed attestation failed validation: %v", err)
	}
}
