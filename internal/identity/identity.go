// Package identity carries the acting principal end to end.
//
// The agent never becomes the system. Every tool call, policy decision, audit
// record and span is attributed to the human or service principal that started
// the run, plus the agent workload identity that is executing it. Both travel
// together; a decision made with only one of them is a bug, and the conformance
// invariant AGENT-4 fails the build when a span is missing either.
package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Errors returned by this package.
var (
	ErrNoPrincipal  = errors.New("identity: no principal in context")
	ErrBadToken     = errors.New("identity: malformed token")
	ErrBadSignature = errors.New("identity: signature mismatch")
	ErrExpired      = errors.New("identity: token expired")
)

// HeaderPrincipal is the wire header carrying a signed principal assertion.
const HeaderPrincipal = "X-TCRP-Principal"

// HeaderTraceParent is the W3C trace header.
const HeaderTraceParent = "traceparent"

// Kind distinguishes a human from a workload.
type Kind string

// Principal kinds.
const (
	KindHuman   Kind = "human"
	KindService Kind = "service"
	KindAgent   Kind = "agent"
)

// Principal is the acting identity.
type Principal struct {
	Subject  string    `json:"sub"`
	Kind     Kind      `json:"kind"`
	Tenant   string    `json:"tenant"`
	Roles    []string  `json:"roles,omitempty"`
	Scopes   []string  `json:"scopes,omitempty"`
	Desk     string    `json:"desk,omitempty"`
	OnBehalf string    `json:"obo,omitempty"` // agent runs carry the human they act for
	Issued   time.Time `json:"iat"`
	Expires  time.Time `json:"exp"`
}

// Anonymous is the principal used when nothing has been asserted. It holds no
// scopes, so every policy that requires one denies it.
var Anonymous = Principal{Subject: "anonymous", Kind: KindService, Tenant: "-"}

// HasScope reports whether the principal holds scope s.
func (p Principal) HasScope(s string) bool {
	for _, v := range p.Scopes {
		if v == s {
			return true
		}
	}
	return false
}

// HasRole reports whether the principal holds role r.
func (p Principal) HasRole(r string) bool {
	for _, v := range p.Roles {
		if v == r {
			return true
		}
	}
	return false
}

// Valid reports whether the principal is usable at time now.
func (p Principal) Valid(now time.Time) error {
	if p.Subject == "" {
		return ErrNoPrincipal
	}
	if !p.Expires.IsZero() && now.After(p.Expires) {
		return ErrExpired
	}
	return nil
}

// String renders the principal for logs and spans.
func (p Principal) String() string {
	s := fmt.Sprintf("%s/%s@%s", p.Kind, p.Subject, p.Tenant)
	if p.OnBehalf != "" {
		s += " obo:" + p.OnBehalf
	}
	return s
}

// ScopeString renders scopes in a stable order for span attributes.
func (p Principal) ScopeString() string {
	s := append([]string(nil), p.Scopes...)
	sort.Strings(s)
	return strings.Join(s, ",")
}

type ctxKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal in ctx, or Anonymous.
func FromContext(ctx context.Context) Principal {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok {
		return Anonymous
	}
	return p
}

// MustFromContext returns the principal or an error when none was asserted.
func MustFromContext(ctx context.Context) (Principal, error) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	if !ok || p.Subject == "" || p.Subject == Anonymous.Subject {
		return Principal{}, ErrNoPrincipal
	}
	return p, nil
}

// Signer mints and verifies principal assertions. In production this is an
// asymmetric verifier against the enterprise IdP's JWKS; the HMAC form keeps the
// platform runnable from a clean clone with no external dependency, and the
// interface is the seam where that swap happens.
type Signer struct {
	key []byte
	ttl time.Duration
}

// NewSigner builds a signer from a shared secret.
func NewSigner(secret string, ttl time.Duration) *Signer {
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return &Signer{key: []byte(secret), ttl: ttl}
}

// Mint produces a signed assertion for p.
func (s *Signer) Mint(p Principal, now time.Time) (string, error) {
	p.Issued = now.UTC()
	if p.Expires.IsZero() {
		p.Expires = now.UTC().Add(s.ttl)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(b64))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return b64 + "." + sig, nil
}

// Verify checks the assertion and returns the principal.
func (s *Signer) Verify(token string, now time.Time) (Principal, error) {
	b64, sig, ok := strings.Cut(token, ".")
	if !ok {
		return Principal{}, ErrBadToken
	}
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(b64))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return Principal{}, ErrBadSignature
	}
	body, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil {
		return Principal{}, ErrBadToken
	}
	var p Principal
	if err := json.Unmarshal(body, &p); err != nil {
		return Principal{}, ErrBadToken
	}
	if err := p.Valid(now); err != nil {
		return Principal{}, err
	}
	return p, nil
}

// Well-known scopes. Scopes are declared per tool in the tool registry and are
// the unit the policy gateway authorizes against.
const (
	ScopeCollateralRead   = "collateral:read"
	ScopeExposureRead     = "exposure:read"
	ScopeMarginPropose    = "margin:propose"
	ScopeTransferDraft    = "transfer:draft"
	ScopeTransferExecute  = "transfer:execute"
	ScopeMarginIssue      = "margin:issue"
	ScopeRedeem           = "shares:redeem"
	ScopeLimitsAdminister = "limits:administer"
	ScopeApprove          = "approvals:grant"
)

// Well-known roles.
const (
	RoleRiskAnalyst   = "risk-analyst"
	RoleRiskManager   = "risk-manager"
	RoleCollateralOps = "collateral-ops"
	RoleTreasurer     = "treasurer"
	RoleAuditor       = "auditor"
)
