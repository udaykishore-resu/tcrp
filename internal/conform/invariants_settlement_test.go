package conform_test

import (
	"crypto/ed25519"
	"strings"
	"testing"

	"github.com/udaykishore-resu/tcrp/internal/bridge"
	"github.com/udaykishore-resu/tcrp/internal/conform"
	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/ledger"
)

const sym = "TCRP-MMF"

// balancedLedgers is the position a healthy bridge leaves behind: a thousand
// units issued on the source, four hundred of them locked in the vault, and
// exactly four hundred minted against that lock on the target.
func balancedLedgers() (*fakeLedger, *fakeLedger) {
	src := newFakeLedger(domain.LedgerEVM)
	src.supply[sym] = 1000
	src.locked[sym] = 400
	src.event(domain.EventMint, sym, 1000, "primary issuance")
	src.event(domain.EventLock, sym, 400, ledger.BridgeMemo(ledger.BridgeOpLock, 1))

	tgt := newFakeLedger(domain.LedgerCanton)
	tgt.supply[sym] = 400
	tgt.event(domain.EventMint, sym, 400, ledger.BridgeMemo(ledger.BridgeOpMint, 1))
	return src, tgt
}

func balancedEvidence() conform.Evidence {
	src, tgt := balancedLedgers()
	return conform.Evidence{
		Source:  conform.LedgerView{Name: src.name, Ledger: src},
		Target:  conform.LedgerView{Name: tgt.name, Ledger: tgt},
		Symbols: []string{sym},
		AsOf:    epoch,
	}
}

// ---------- SETTLE-1 ----------

func TestSettle1PassesOnABalancedBridge(t *testing.T) {
	v := wantStatus(t, evaluate(t, "SETTLE-1", balancedEvidence()), conform.StatusPass)
	if v.Severity != conform.SeverityCritical {
		t.Fatalf("SETTLE-1 severity %s, want critical", v.Severity)
	}
}

func TestSettle1FailsWhenMoreWasMintedThanLocked(t *testing.T) {
	src, tgt := balancedLedgers()
	// A hundred units on the target that nothing on the source is backing.
	tgt.supply[sym] = 500
	tgt.events = nil
	tgt.event(domain.EventMint, sym, 500, ledger.BridgeMemo(ledger.BridgeOpMint, 1))
	ev := conform.Evidence{
		Source:  conform.LedgerView{Name: src.name, Ledger: src},
		Target:  conform.LedgerView{Name: tgt.name, Ledger: tgt},
		Symbols: []string{sym}, AsOf: epoch,
	}
	v := wantStatus(t, evaluate(t, "SETTLE-1", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "BRK-BRIDGE-LOCK-"+sym) {
		t.Fatalf("detail does not name the break: %s", v.Detail)
	}
	wantRefs(t, v, 1)
}

func TestSettle1IsInconclusiveWithoutLedgers(t *testing.T) {
	wantStatus(t, evaluate(t, "SETTLE-1", conform.Evidence{}), conform.StatusInconclusive)
}

// ---------- SETTLE-2 ----------

func registryEvidence(evmHolders, cantonHolders []string, verified map[string]bool) conform.Evidence {
	src, tgt := balancedLedgers()
	reg := func(h string) bool { return verified[h] }
	return conform.Evidence{
		Source: conform.LedgerView{
			Name: src.name, Ledger: src,
			Holders:    func(string) []string { return evmHolders },
			Registered: reg,
		},
		Target: conform.LedgerView{
			Name: tgt.name, Ledger: tgt,
			Holders:    func(string) []string { return cantonHolders },
			Registered: reg,
		},
		Symbols: []string{sym}, AsOf: epoch,
	}
}

func TestSettle2PassesWhenEveryHolderIsRegistered(t *testing.T) {
	ev := registryEvidence(
		[]string{"0xalice", ledger.VaultAddress},
		[]string{"Bob::1220", ledger.VaultParty},
		map[string]bool{"0xalice": true, "Bob::1220": true},
	)
	v := wantStatus(t, evaluate(t, "SETTLE-2", ev), conform.StatusPass)
	// The vault holds units and is outside the registry by construction, so it
	// must not have been counted.
	if !strings.Contains(v.Observed, "2 holdings") {
		t.Fatalf("observed: %s", v.Observed)
	}
}

func TestSettle2FailsOnAHolderOutsideTheRegistry(t *testing.T) {
	ev := registryEvidence(
		[]string{"0xalice", "0xstranger"},
		[]string{"Bob::1220"},
		map[string]bool{"0xalice": true, "Bob::1220": true},
	)
	v := wantStatus(t, evaluate(t, "SETTLE-2", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "0xstranger") {
		t.Fatalf("detail does not name the holder: %s", v.Detail)
	}
	wantRefs(t, v, 1)
}

// TestSettle2IsInconclusiveOnHalfTheEvidence: one clean network is a
// conclusion about one network.
func TestSettle2IsInconclusiveOnHalfTheEvidence(t *testing.T) {
	src, tgt := balancedLedgers()
	ev := conform.Evidence{
		Source: conform.LedgerView{
			Name: src.name, Ledger: src,
			Holders:    func(string) []string { return []string{"0xalice"} },
			Registered: func(string) bool { return true },
		},
		Target:  conform.LedgerView{Name: tgt.name, Ledger: tgt},
		Symbols: []string{sym}, AsOf: epoch,
	}
	v := wantStatus(t, evaluate(t, "SETTLE-2", ev), conform.StatusInconclusive)
	if !strings.Contains(v.Detail, string(domain.LedgerCanton)) {
		t.Fatalf("detail does not name the network with no evidence: %s", v.Detail)
	}
}

// ---------- SETTLE-3 ----------

// signerSet builds a real 2-of-3 attester set and returns it with its signers.
func signerSet(t *testing.T) (*bridge.SignerSet, []*bridge.Signer) {
	t.Helper()
	var signers []*bridge.Signer
	for _, id := range []string{"attester-a", "attester-b", "attester-c"} {
		// Deterministic keys: the seed is the signer id padded out, so the
		// fixture produces the same attestation digests on every run.
		seed := make([]byte, ed25519.SeedSize)
		copy(seed, id)
		signers = append(signers, bridge.NewSigner(id, ed25519.NewKeyFromSeed(seed)))
	}
	set, err := bridge.SignerSetFrom(2, signers...)
	if err != nil {
		t.Fatalf("signer set: %v", err)
	}
	return set, signers
}

func attestedEvidence(t *testing.T, qty int64, signWith int) conform.Evidence {
	t.Helper()
	set, signers := signerSet(t)
	ev := balancedEvidence()
	att := bridge.Attestation{
		Nonce: 1, SourceLedger: domain.LedgerEVM, TargetLedger: domain.LedgerCanton,
		Symbol: sym, Holder: "CP-001", Quantity: qty,
		SourceTxHash: "0xlock", SourceBlock: 91, ObservedAt: epoch,
	}
	ev.Signers = set
	ev.Mints = []conform.AttestedMint{{
		Attestation: att,
		Signatures:  bridge.Collect(att, signers[:signWith]),
	}}
	return ev
}

func TestSettle3PassesWhenTheMintIsAttestedAndLocked(t *testing.T) {
	v := wantStatus(t, evaluate(t, "SETTLE-3", attestedEvidence(t, 400, 2)), conform.StatusPass)
	wantRefs(t, v, 3)
}

func TestSettle3FailsWhenTheQuorumIsShort(t *testing.T) {
	v := wantStatus(t, evaluate(t, "SETTLE-3", attestedEvidence(t, 400, 1)), conform.StatusFail)
	if !strings.Contains(v.Detail, "quorum") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestSettle3FailsWhenTheAttestationDisagreesWithTheMint(t *testing.T) {
	v := wantStatus(t, evaluate(t, "SETTLE-3", attestedEvidence(t, 300, 2)), conform.StatusFail)
	if !strings.Contains(v.Detail, "attestation says 300") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestSettle3FailsWhenAMintHasNoAttestationAtAll(t *testing.T) {
	ev := attestedEvidence(t, 400, 2)
	ev.Mints = nil
	v := wantStatus(t, evaluate(t, "SETTLE-3", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "no attestation") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestSettle3FailsWhenNoLockBacksTheMint(t *testing.T) {
	ev := attestedEvidence(t, 400, 2)
	src := ev.Source.Ledger.(*fakeLedger)
	src.events = nil
	src.event(domain.EventMint, sym, 1000, "primary issuance")
	v := wantStatus(t, evaluate(t, "SETTLE-3", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "no matching lock") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestSettle3IsInconclusiveWithoutASignerSet(t *testing.T) {
	ev := attestedEvidence(t, 400, 2)
	ev.Signers = nil
	wantStatus(t, evaluate(t, "SETTLE-3", ev), conform.StatusInconclusive)
}

// ---------- SETTLE-4 ----------

func TestSettle4PassesWhenEveryNonceIsUsedOnce(t *testing.T) {
	wantStatus(t, evaluate(t, "SETTLE-4", balancedEvidence()), conform.StatusPass)
}

func TestSettle4FailsWhenANonceWasMintedTwice(t *testing.T) {
	ev := balancedEvidence()
	tgt := ev.Target.Ledger.(*fakeLedger)
	// A reorganisation re-included the lock and the relayer minted again.
	tgt.event(domain.EventMint, sym, 400, ledger.BridgeMemo(ledger.BridgeOpMint, 1))
	tgt.supply[sym] = 800
	v := wantStatus(t, evaluate(t, "SETTLE-4", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "nonce 1 minted 2 times") {
		t.Fatalf("detail: %s", v.Detail)
	}
	wantRefs(t, v, 2)
}

func TestSettle4IsInconclusiveWithoutALedger(t *testing.T) {
	wantStatus(t, evaluate(t, "SETTLE-4", conform.Evidence{}), conform.StatusInconclusive)
}

// ---------- SETTLE-5 ----------

func TestSettle5PassesWhenTheIssuanceIdentityHolds(t *testing.T) {
	wantStatus(t, evaluate(t, "SETTLE-5", balancedEvidence()), conform.StatusPass)
}

func TestSettle5FailsWhenUnitsExistThatNothingIssued(t *testing.T) {
	src, tgt := balancedLedgers()
	// A hundred extra units on the source with no issuance event behind them.
	// The locks still balance, so SETTLE-1's lock check would not see this.
	src.supply[sym] = 1100
	ev := conform.Evidence{
		Source:  conform.LedgerView{Name: src.name, Ledger: src},
		Target:  conform.LedgerView{Name: tgt.name, Ledger: tgt},
		Symbols: []string{sym}, AsOf: epoch,
	}
	v := wantStatus(t, evaluate(t, "SETTLE-5", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "delta 100") {
		t.Fatalf("detail: %s", v.Detail)
	}
	// The lock half of the same reconciliation is still clean, which is the
	// reason the issuance identity has its own line.
	lock := evaluate(t, "SETTLE-1", ev)
	if lock.Status != conform.StatusFail {
		t.Fatalf("SETTLE-1 should also fail on a supply break, got %s", lock.Status)
	}
	if !strings.Contains(lock.Detail, "BRK-BRIDGE-ISSUE-") {
		t.Fatalf("SETTLE-1 detail should name the issuance break: %s", lock.Detail)
	}
}

// ---------- DATA-1 ----------

func TestData1PassesWhenEveryGoldRowTraces(t *testing.T) {
	q := domain.QualityReport{Status: domain.QualityPass, RanAt: epoch}
	ev := conform.Evidence{
		Lineage: func() (int, int) { return 128, 128 },
		Quality: &q,
	}
	v := wantStatus(t, evaluate(t, "DATA-1", ev), conform.StatusPass)
	wantRefs(t, v, 2)
}

func TestData1FailsWhenAGoldRowHasNoBronzeAncestor(t *testing.T) {
	ev := conform.Evidence{Lineage: func() (int, int) { return 128, 120 }}
	v := wantStatus(t, evaluate(t, "DATA-1", ev), conform.StatusFail)
	if !strings.Contains(v.Detail, "8 gold rows have no bronze ancestor") {
		t.Fatalf("detail: %s", v.Detail)
	}
}

func TestData1IsInconclusiveWithoutACounter(t *testing.T) {
	wantStatus(t, evaluate(t, "DATA-1", conform.Evidence{}), conform.StatusInconclusive)
}
