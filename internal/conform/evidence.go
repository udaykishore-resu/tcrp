package conform

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/obs"
)

// Errors returned by bundle verification. They are sentinels because "this
// bundle was edited" and "this bundle was signed by a key I do not trust" are
// two different conversations, and an auditor should not have to read a string
// to tell them apart.
var (
	// ErrBundleTampered is returned when the bundle's content no longer
	// matches its id.
	ErrBundleTampered = errors.New("conform: bundle content does not match its id")
	// ErrBundleUnsigned is returned when a bundle carries no signature.
	ErrBundleUnsigned = errors.New("conform: bundle is not signed")
	// ErrBundleSignature is returned when the signature does not verify.
	ErrBundleSignature = errors.New("conform: bundle signature does not verify")
	// ErrNoVerificationKey is returned when Verify is called with no key. A
	// third party has to say which key they trust; falling back to the key the
	// bundle names would verify every bundle against itself.
	ErrNoVerificationKey = errors.New("conform: no verification key supplied")
)

// Bundle is the signed, content-addressed record of one conformance run.
//
// It is the artefact the platform hands to somebody who was not there: the
// verdicts, the digest of the model that declared them, the digests of the
// evidence they were rendered against, the policy bundle in force, and the
// audit chain head at the moment of the run. VerifyBundle needs none of the
// platform to check it — a file, a public key, and this package.
//
// The id is the sha256 of the canonical encoding of everything except the id
// and the signature. That makes the bundle content-addressed: two runs over the
// same evidence produce the same id, and any edit produces a different one.
type Bundle struct {
	// ID is the sha256 of the canonical payload: the bundle's content address.
	ID string `json:"id"`
	// CreatedAt is the clock reading at the moment the bundle was packaged.
	CreatedAt time.Time `json:"created_at"`
	// Process and ProcessName identify the model the verdicts belong to.
	Process     string `json:"process,omitempty"`
	ProcessName string `json:"process_name,omitempty"`
	// ProcessDigest is the sha256 of the BPMN document. The verdicts are a
	// statement about this exact model text and no other.
	ProcessDigest string `json:"process_digest,omitempty"`
	// Verdicts are the results, ordered by invariant id.
	Verdicts []Verdict `json:"verdicts"`
	// Artefacts is the evidence manifest: every input, named and digested.
	Artefacts []Artefact `json:"artefacts,omitempty"`
	// PolicyBundleDigest is the digest of the policy bundle in force.
	PolicyBundleDigest string `json:"policy_bundle_digest,omitempty"`
	// AuditHead is the audit chain head hash at the time of the run.
	AuditHead string `json:"audit_head,omitempty"`
	// Passed, Failed and Inconclusive restate the tally so a reader does not
	// have to count. They are inside the signed payload, so they cannot be
	// rewritten to flatter the run.
	Passed       int `json:"passed"`
	Failed       int `json:"failed"`
	Inconclusive int `json:"inconclusive"`
	// GatePassed records the gate's own verdict at the time of signing.
	GatePassed bool `json:"gate_passed"`
	// PublicKey identifies the signing key. It is a convenience for finding
	// the right key, never a substitute for being handed one.
	PublicKey string `json:"public_key,omitempty"`
	// Signature is the hex ed25519 signature over the canonical payload.
	Signature string `json:"signature,omitempty"`
}

// canonicalBundle is the signed payload: every field except the id and the
// signature, in a fixed order, with no maps. Go's encoder writes struct fields
// in declaration order and sorts map keys, and there are no maps here, so the
// encoding is byte-stable for the same values.
type canonicalBundle struct {
	CreatedAt          time.Time  `json:"created_at"`
	Process            string     `json:"process"`
	ProcessName        string     `json:"process_name"`
	ProcessDigest      string     `json:"process_digest"`
	Verdicts           []Verdict  `json:"verdicts"`
	Artefacts          []Artefact `json:"artefacts"`
	PolicyBundleDigest string     `json:"policy_bundle_digest"`
	AuditHead          string     `json:"audit_head"`
	Passed             int        `json:"passed"`
	Failed             int        `json:"failed"`
	Inconclusive       int        `json:"inconclusive"`
	GatePassed         bool       `json:"gate_passed"`
	PublicKey          string     `json:"public_key"`
}

// canonical returns the bytes the id is taken over and the signature covers.
func (b Bundle) canonical() ([]byte, error) {
	return json.Marshal(canonicalBundle{
		CreatedAt:          b.CreatedAt.UTC(),
		Process:            b.Process,
		ProcessName:        b.ProcessName,
		ProcessDigest:      b.ProcessDigest,
		Verdicts:           b.Verdicts,
		Artefacts:          b.Artefacts,
		PolicyBundleDigest: b.PolicyBundleDigest,
		AuditHead:          b.AuditHead,
		Passed:             b.Passed,
		Failed:             b.Failed,
		Inconclusive:       b.Inconclusive,
		GatePassed:         b.GatePassed,
		PublicKey:          b.PublicKey,
	})
}

// Manifest lists every evidence artefact with its digest, sorted by name.
func (b Bundle) Manifest() []Artefact { return b.Artefacts }

// NewBundle packages a report and the evidence it was rendered against.
//
// The bundle is unsigned and its id is already set, so a caller that has no
// signing key still gets a content address. Sign turns it into something a
// third party can rely on.
func NewBundle(clock obs.Clock, rep Report, ev Evidence) (Bundle, error) {
	if clock == nil {
		clock = obs.RealClock{}
	}
	b := Bundle{
		CreatedAt:          clock.Now().UTC(),
		Process:            rep.Process,
		ProcessName:        rep.ProcessName,
		ProcessDigest:      rep.ProcessDigest,
		Verdicts:           rep.Verdicts,
		Artefacts:          ev.Manifest(),
		PolicyBundleDigest: ev.PolicyBundleDigest,
		AuditHead:          ev.AuditHead(),
		Passed:             rep.Passed,
		Failed:             rep.Failed,
		Inconclusive:       rep.Inconclusive,
		GatePassed:         rep.Gate() == nil,
	}
	if err := b.reseal(); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// reseal recomputes the content address from the current payload.
func (b *Bundle) reseal() error {
	raw, err := b.canonical()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	b.ID = hex.EncodeToString(sum[:])
	return nil
}

// Sign seals the bundle with an ed25519 key and returns the signed copy.
//
// The public key is recorded in the payload before the id is computed, so a
// bundle re-signed by a different key is a different bundle rather than the
// same bundle with a new signature.
func (b Bundle) Sign(priv ed25519.PrivateKey) (Bundle, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return Bundle{}, errors.New("conform: signing key is not an ed25519 private key")
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return Bundle{}, errors.New("conform: signing key has no ed25519 public key")
	}
	b.PublicKey = hex.EncodeToString(pub)
	b.Signature = ""
	if err := b.reseal(); err != nil {
		return Bundle{}, err
	}
	raw, err := b.canonical()
	if err != nil {
		return Bundle{}, err
	}
	b.Signature = hex.EncodeToString(ed25519.Sign(priv, raw))
	return b, nil
}

// VerifyBundle checks a bundle offline: that its content still hashes to its
// id, and that pub signed exactly that content.
//
// The two checks are separate on purpose. A mutated verdict breaks the id, and
// the error says the content was altered. A bundle whose id was recomputed
// after the mutation still fails the signature, and the error says so. An
// auditor reading either message knows which one happened.
func VerifyBundle(b Bundle, pub ed25519.PublicKey) error {
	raw, err := b.canonical()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	if want := hex.EncodeToString(sum[:]); want != b.ID {
		return fmt.Errorf("%w: id is %s, content hashes to %s",
			ErrBundleTampered, shortHash(b.ID), shortHash(want))
	}
	if b.Signature == "" {
		return ErrBundleUnsigned
	}
	if len(pub) != ed25519.PublicKeySize {
		return ErrNoVerificationKey
	}
	sig, err := hex.DecodeString(b.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrBundleSignature)
	}
	if !ed25519.Verify(pub, raw, sig) {
		return fmt.Errorf("%w: not signed by the supplied key", ErrBundleSignature)
	}
	return nil
}

// WriteBundle writes a bundle as indented JSON, which is the form checked into
// a build artefact store and read by a human when something has gone wrong.
func WriteBundle(w io.Writer, b Bundle) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(b)
}

// ReadBundle reads a bundle written by WriteBundle.
func ReadBundle(r io.Reader) (Bundle, error) {
	var b Bundle
	if err := json.NewDecoder(r).Decode(&b); err != nil {
		return Bundle{}, err
	}
	return b, nil
}

// Summary renders the bundle in one line, for a build log.
func (b Bundle) Summary() string {
	gate := "GATE FAILED"
	if b.GatePassed {
		gate = "GATE PASSED"
	}
	return fmt.Sprintf("bundle %s process=%s %d pass %d fail %d inconclusive %s",
		shortHash(b.ID), b.Process, b.Passed, b.Failed, b.Inconclusive, gate)
}
