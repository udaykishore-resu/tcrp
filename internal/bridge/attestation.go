// Package bridge moves a tokenised fund share between the platform's two
// settlement networks and proves that doing so did not create or destroy any.
//
// The mechanism is lock-and-mint. Units are locked into a vault on the source
// ledger, an independent set of signers attests that the lock happened and is
// final, and a quorum of those attestations authorises a mint of the same
// quantity on the target. Redemption is the reverse: burn on the target,
// attest, unlock on the source. Nothing is transferred; the two ledgers each
// hold their own units and the vault balance is the link between them.
//
// The interesting part of a bridge is not the happy path, which is a for loop.
// It is what happens when the source chain changes its mind. This relayer is
// built around four failure modes, each of which has a test:
//
//   - A lock is orphaned by a reorganisation before it was final. Nothing was
//     minted, the pending attestation is abandoned, and an alert says so.
//   - A lock is orphaned by a reorganisation after it was final. Units have
//     already been minted on the target against collateral that no longer
//     exists. There is no safe automatic repair: the relayer raises a
//     finality-violation alert and does not mint again, because the one thing
//     worse than a stuck bridge is a silent double mint.
//   - The same lock is observed twice, because a reorganisation re-included
//     the transaction. Replay protection is keyed on the bridge nonce carried
//     by the lock itself, not on the transaction hash or the block, so
//     re-observation mints nothing.
//   - A signer is unavailable, or the target submission fails. A quorum of M
//     of N still authorises; a failed submission retries with backoff and is
//     idempotent by nonce.
//
// Attestations are signed with ed25519 over a canonical digest of the
// attestation's fields, so a mutated field, an unknown signer, a signature
// counted twice and a quorum short by one are all rejected — and all four are
// tested rather than asserted.
//
// What is deliberately not here: the signers are in-process, holding keys in
// memory, and a real deployment runs them as separate processes in separate
// failure domains with keys in separate HSMs. That is an operational
// difference, not a protocol one; the quorum arithmetic and the verification
// are the same either way. There is also no fee model, no rate limiting and no
// governance over the signer set: changing the set means constructing a new
// SignerSet.
package bridge

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
)

// Errors returned by attestation verification. They are sentinels because
// "this signature is not one of ours" and "this quorum is one short" are two
// different operational conditions: the first is an attack or a
// misconfiguration, the second is an outage.
var (
	// ErrUnknownSigner is returned when a submission carries a signature from
	// a key that is not in the signer set. It is refused outright rather than
	// ignored, because a submission containing a stranger's signature is
	// evidence of something the operator has to look at.
	ErrUnknownSigner = errors.New("bridge: signature from a signer outside the set")
	// ErrDuplicateSigner is returned when one signer appears twice in a
	// submission. This is the cheapest possible attack on an M-of-N scheme and
	// it is refused rather than deduplicated.
	ErrDuplicateSigner = errors.New("bridge: signer appears more than once")
	// ErrBadSignature is returned when a signature does not verify against the
	// attestation's digest, which is what a mutated field produces.
	ErrBadSignature = errors.New("bridge: signature does not verify")
	// ErrQuorumNotMet is returned when fewer than the required number of
	// distinct signers signed.
	ErrQuorumNotMet = errors.New("bridge: quorum not met")
	// ErrBadSignerSet is returned when a signer set is constructed with a
	// quorum that cannot be reached, or with no signers.
	ErrBadSignerSet = errors.New("bridge: invalid signer set")
	// ErrNotCanonical is returned when a value cannot be encoded
	// deterministically — a float, most often, which has no single textual
	// form and must never appear in something that is hashed and signed.
	ErrNotCanonical = errors.New("bridge: value has no canonical encoding")
)

// Attestation is a signer's statement that a quantity of a symbol was locked
// (or burned) on one ledger, at a named transaction in a named block, and may
// therefore be minted (or unlocked) on the other.
//
// Nonce is the replay key and it is the only field that has to be unique. It
// comes from the lock itself, carried in the event so that it survives a chain
// reorganisation re-including the transaction: keying replay protection on a
// transaction hash or a block height would fail exactly when it matters.
type Attestation struct {
	Nonce        uint64        `json:"nonce"`
	SourceLedger domain.Ledger `json:"source_ledger"`
	TargetLedger domain.Ledger `json:"target_ledger"`
	Symbol       string        `json:"symbol"`
	// Holder is the party the minted units belong to, expressed as the
	// platform's counterparty identifier rather than either ledger's address
	// format, because the two ledgers do not share one.
	Holder       string    `json:"holder"`
	Quantity     int64     `json:"quantity"`
	SourceTxHash string    `json:"source_tx_hash"`
	SourceBlock  uint64    `json:"source_block"`
	ObservedAt   time.Time `json:"observed_at"`
}

// Validate reports whether the attestation is structurally usable. It is not a
// judgement about whether the lock happened — that is what the signers are
// for — only that the statement is complete.
func (a Attestation) Validate() error {
	switch {
	case a.SourceLedger == "" || a.TargetLedger == "":
		return errors.New("bridge: attestation names no ledger")
	case a.SourceLedger == a.TargetLedger:
		return errors.New("bridge: attestation source and target are the same ledger")
	case a.Symbol == "":
		return errors.New("bridge: attestation names no symbol")
	case a.Holder == "":
		return errors.New("bridge: attestation names no holder")
	case a.Quantity <= 0:
		return errors.New("bridge: attestation quantity must be positive")
	case a.SourceTxHash == "":
		return errors.New("bridge: attestation names no source transaction")
	}
	return nil
}

// fields returns the attestation as the map that is canonically encoded and
// hashed. Every field is included: an attestation that signed only some of its
// contents would let an attacker vary the rest.
func (a Attestation) fields() map[string]any {
	return map[string]any{
		"nonce":          a.Nonce,
		"source_ledger":  string(a.SourceLedger),
		"target_ledger":  string(a.TargetLedger),
		"symbol":         a.Symbol,
		"holder":         a.Holder,
		"quantity":       a.Quantity,
		"source_tx_hash": a.SourceTxHash,
		"source_block":   a.SourceBlock,
		"observed_at":    a.ObservedAt.UTC(),
	}
}

// Digest is the sha256 of the attestation's canonical encoding. It is what
// signers sign and what verification checks, so two structurally identical
// attestations produce one digest regardless of how either side built them.
func (a Attestation) Digest() [32]byte {
	b, err := CanonicalJSON(a.fields())
	if err != nil {
		// fields() contains only types the encoder handles, so this is
		// unreachable; returning a zero digest would be worse than failing
		// loudly, because a zero digest verifies against nothing.
		panic("bridge: attestation is not canonically encodable: " + err.Error())
	}
	return sha256.Sum256(b)
}

// DigestHex renders the digest for logs and audit records.
func (a Attestation) DigestHex() string {
	d := a.Digest()
	return hex.EncodeToString(d[:])
}

// Signature is one signer's endorsement of an attestation.
type Signature struct {
	// Signer is the signer's identifier, which is how the set looks up the
	// public key. It is not itself trusted: a signature claiming to be from
	// one signer but produced by another key fails verification.
	Signer string `json:"signer"`
	// Sig is the hex-encoded ed25519 signature over the attestation digest.
	Sig string `json:"sig"`
}

// Signer holds one attester's private key.
type Signer struct {
	id   string
	priv ed25519.PrivateKey
	// available models an attester being unreachable. A bridge whose quorum is
	// only ever exercised at full strength has not been tested. It is atomic
	// because a test may take a signer offline while the relayer is running.
	available atomic.Bool
}

// NewSigner builds a signer from an ed25519 private key.
func NewSigner(id string, priv ed25519.PrivateKey) *Signer {
	s := &Signer{id: id, priv: priv}
	s.available.Store(true)
	return s
}

// ID returns the signer's identifier.
func (s *Signer) ID() string { return s.id }

// Public returns the signer's verification key.
func (s *Signer) Public() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// SetAvailable marks the signer reachable or not. An unavailable signer
// returns no signature, which is how a test drives the M-of-N path.
func (s *Signer) SetAvailable(v bool) { s.available.Store(v) }

// Available reports whether the signer will sign.
func (s *Signer) Available() bool { return s.available.Load() }

// Sign endorses an attestation. It returns false when the signer is
// unavailable, which the relayer treats as one fewer signature rather than as
// an error: an attester being down is an expected condition, not a fault.
func (s *Signer) Sign(a Attestation) (Signature, bool) {
	if !s.available.Load() {
		return Signature{}, false
	}
	d := a.Digest()
	return Signature{Signer: s.id, Sig: hex.EncodeToString(ed25519.Sign(s.priv, d[:]))}, true
}

// SignerSet is the set of attesters and the quorum a mint requires.
type SignerSet struct {
	quorum int
	keys   map[string]ed25519.PublicKey
	ids    []string
}

// NewSignerSet builds an M-of-N signer set. A quorum larger than the set, or
// below one, is an error rather than a clamp: silently reducing a threshold is
// how a bridge ends up with a quorum of one.
func NewSignerSet(quorum int, keys map[string]ed25519.PublicKey) (*SignerSet, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: no signers", ErrBadSignerSet)
	}
	if quorum < 1 || quorum > len(keys) {
		return nil, fmt.Errorf("%w: quorum %d of %d signers", ErrBadSignerSet, quorum, len(keys))
	}
	ids := make([]string, 0, len(keys))
	copied := make(map[string]ed25519.PublicKey, len(keys))
	for id, k := range keys {
		ids = append(ids, id)
		copied[id] = k
	}
	sort.Strings(ids)
	return &SignerSet{quorum: quorum, keys: copied, ids: ids}, nil
}

// SignerSetFrom builds a set from signers, which is the usual case.
func SignerSetFrom(quorum int, signers ...*Signer) (*SignerSet, error) {
	keys := make(map[string]ed25519.PublicKey, len(signers))
	for _, s := range signers {
		keys[s.ID()] = s.Public()
	}
	return NewSignerSet(quorum, keys)
}

// Quorum is the number of distinct valid signatures a mint requires.
func (s *SignerSet) Quorum() int { return s.quorum }

// Size is the number of signers in the set.
func (s *SignerSet) Size() int { return len(s.keys) }

// Signers returns the signer identifiers, sorted.
func (s *SignerSet) Signers() []string { return append([]string(nil), s.ids...) }

// Verify checks a submission against the set and returns the number of
// distinct valid signatures.
//
// It refuses, in this order: a signature from outside the set, the same signer
// twice, a signature that does not verify against the digest, and finally a
// count short of the quorum. The order matters because the first three are
// evidence of tampering and the fourth is evidence of an outage, and an
// operator paged at three in the morning should be told which.
func (s *SignerSet) Verify(a Attestation, sigs []Signature) (int, error) {
	if err := a.Validate(); err != nil {
		return 0, err
	}
	d := a.Digest()
	seen := make(map[string]bool, len(sigs))
	for _, sig := range sigs {
		pub, ok := s.keys[sig.Signer]
		if !ok {
			return 0, fmt.Errorf("%w: %s", ErrUnknownSigner, sig.Signer)
		}
		if seen[sig.Signer] {
			return 0, fmt.Errorf("%w: %s", ErrDuplicateSigner, sig.Signer)
		}
		raw, err := hex.DecodeString(sig.Sig)
		if err != nil || !ed25519.Verify(pub, d[:], raw) {
			return 0, fmt.Errorf("%w: %s over %s", ErrBadSignature, sig.Signer, a.DigestHex()[:12])
		}
		seen[sig.Signer] = true
	}
	if len(seen) < s.quorum {
		return len(seen), fmt.Errorf("%w: %d of %d required", ErrQuorumNotMet, len(seen), s.quorum)
	}
	return len(seen), nil
}

// Collect gathers signatures from the available signers, in signer-id order so
// that the same set of available signers always produces the same submission.
func Collect(a Attestation, signers []*Signer) []Signature {
	ordered := append([]*Signer(nil), signers...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID() < ordered[j].ID() })
	out := make([]Signature, 0, len(ordered))
	for _, s := range ordered {
		if sig, ok := s.Sign(a); ok {
			out = append(out, sig)
		}
	}
	return out
}

// CanonicalJSON encodes v deterministically: object keys sorted, no
// insignificant whitespace, integers written as integers and times as RFC 3339
// in UTC.
//
// It is not encoding/json with a sort bolted on. A round trip through
// encoding/json turns every number into a float64, which silently loses
// precision above 2^53 — and share quantities are int64 precisely because they
// must not be approximated. Anything this encoder cannot represent exactly,
// float64 above all, is an error rather than a best effort.
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeCanonical(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	case int:
		buf.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(t, 10))
	case time.Time:
		buf.WriteByte('"')
		buf.WriteString(t.UTC().Format(time.RFC3339Nano))
		buf.WriteByte('"')
	case []string:
		buf.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, s); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := encodeCanonical(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("%w: %T", ErrNotCanonical, v)
	}
	return nil
}
