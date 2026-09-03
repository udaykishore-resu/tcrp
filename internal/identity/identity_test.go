package identity

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	now = time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

	operator = Principal{
		Subject: "j.okafor", Kind: KindHuman, Tenant: "collateral-ops", Desk: "collateral",
		Roles:  []string{RoleCollateralOps},
		Scopes: []string{ScopeCollateralRead, ScopeExposureRead, ScopeMarginPropose},
	}
	agent = Principal{
		Subject: "svc/collateral-agent", Kind: KindAgent, Tenant: "collateral-ops",
		OnBehalf: "j.okafor",
		Roles:    []string{RoleCollateralOps},
		Scopes:   []string{ScopeTransferExecute},
	}
)

// TestMintVerifyRoundTrip proves a minted assertion carries the whole principal
// back, including the human an agent is acting for, and that the signer stamps
// issue and expiry times from the clock it is handed.
func TestMintVerifyRoundTrip(t *testing.T) {
	s := NewSigner("test-secret", 15*time.Minute)

	for _, p := range []Principal{operator, agent} {
		tok, err := s.Mint(p, now)
		if err != nil {
			t.Fatalf("Mint(%s): %v", p.Subject, err)
		}
		got, err := s.Verify(tok, now)
		if err != nil {
			t.Fatalf("Verify(%s): %v", p.Subject, err)
		}
		if got.Subject != p.Subject || got.Kind != p.Kind || got.Tenant != p.Tenant ||
			got.Desk != p.Desk || got.OnBehalf != p.OnBehalf {
			t.Fatalf("round trip lost identity: %+v vs %+v", got, p)
		}
		if got.ScopeString() != p.ScopeString() || strings.Join(got.Roles, ",") != strings.Join(p.Roles, ",") {
			t.Fatalf("round trip lost scopes or roles: %+v", got)
		}
		if !got.Issued.Equal(now) {
			t.Fatalf("Issued = %v, want the supplied clock reading", got.Issued)
		}
		if want := now.Add(15 * time.Minute); !got.Expires.Equal(want) {
			t.Fatalf("Expires = %v, want %v", got.Expires, want)
		}
	}

	// An explicit expiry is honoured rather than overwritten by the TTL.
	pinned := operator
	pinned.Expires = now.Add(90 * time.Second)
	tok, err := s.Mint(pinned, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Verify(tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Expires.Equal(pinned.Expires) {
		t.Fatalf("an explicit expiry was replaced: %v", got.Expires)
	}

	// A non-positive TTL falls back to the documented default.
	if def := NewSigner("k", 0); def.ttl != 15*time.Minute {
		t.Fatalf("default ttl = %v", def.ttl)
	}
}

// TestVerifyRejectsBadTokens covers every way an assertion can fail to be one.
// The distinction between a mangled token, a forged signature and a stale one
// matters because the three mean different things to an operator.
func TestVerifyRejectsBadTokens(t *testing.T) {
	s := NewSigner("test-secret", 15*time.Minute)
	good, err := s.Mint(operator, now)
	if err != nil {
		t.Fatal(err)
	}
	body, sig, _ := strings.Cut(good, ".")

	// A token whose payload was edited and re-encoded, keeping the old signature.
	edited := operator
	edited.Scopes = append(append([]string{}, edited.Scopes...), ScopeTransferExecute)
	edited.Issued, edited.Expires = now, now.Add(time.Hour)
	raw, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	tamperedBody := base64.RawURLEncoding.EncodeToString(raw) + "." + sig

	cases := []struct {
		name  string
		token string
		at    time.Time
		want  error
	}{
		{"empty", "", now, ErrBadToken},
		{"no separator", body + sig, now, ErrBadToken},
		{"signature flipped", body + "." + flip(sig), now, ErrBadSignature},
		{"signature truncated", body + "." + sig[:len(sig)-4], now, ErrBadSignature},
		{"payload edited to add a scope", tamperedBody, now, ErrBadSignature},
		{"payload not base64", "!!!." + sig, now, ErrBadSignature},
		{"expired by one second", good, now.Add(15*time.Minute + time.Second), ErrExpired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.Verify(c.token, c.at)
			if !errors.Is(err, c.want) {
				t.Fatalf("Verify = %+v, %v; want %v", got, err, c.want)
			}
			if got.Subject != "" {
				t.Fatalf("a rejected token still returned a principal: %+v", got)
			}
		})
	}

	// The token is valid right up to the expiry instant.
	if _, err := s.Verify(good, now.Add(15*time.Minute)); err != nil {
		t.Fatalf("a token must be valid at its expiry instant, got %v", err)
	}
}

// TestWrongKeyIsRejected proves the signature binds the assertion to the issuing
// key: a second signer with a different secret cannot mint an acceptable token,
// and cannot verify one it did not mint.
func TestWrongKeyIsRejected(t *testing.T) {
	mine := NewSigner("the-real-secret", time.Hour)
	theirs := NewSigner("a-different-secret", time.Hour)

	forged, err := theirs.Mint(agent, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mine.Verify(forged, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a token from another key verified: %v", err)
	}
	good, err := mine.Mint(agent, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := theirs.Verify(good, now); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("verification with the wrong key succeeded: %v", err)
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

// TestScopeAndRoleHelpers proves the membership tests the policy gateway and the
// approval service are written against, including that Anonymous holds nothing.
func TestScopeAndRoleHelpers(t *testing.T) {
	cases := []struct {
		name        string
		p           Principal
		scope       string
		wantScope   bool
		role        string
		wantHasRole bool
	}{
		{"operator holds a scope it was granted", operator, ScopeMarginPropose, true, RoleCollateralOps, true},
		{"operator does not hold execution", operator, ScopeTransferExecute, false, RoleTreasurer, false},
		{"agent holds execution", agent, ScopeTransferExecute, true, RoleCollateralOps, true},
		{"anonymous holds nothing", Anonymous, ScopeCollateralRead, false, RoleAuditor, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.p.HasScope(c.scope); got != c.wantScope {
				t.Fatalf("HasScope(%s) = %v, want %v", c.scope, got, c.wantScope)
			}
			if got := c.p.HasRole(c.role); got != c.wantHasRole {
				t.Fatalf("HasRole(%s) = %v, want %v", c.role, got, c.wantHasRole)
			}
		})
	}

	// ScopeString is sorted, so a span attribute does not change between runs.
	p := Principal{Scopes: []string{ScopeTransferExecute, ScopeCollateralRead, ScopeApprove}}
	want := "approvals:grant,collateral:read,transfer:execute"
	if got := p.ScopeString(); got != want {
		t.Fatalf("ScopeString = %q, want %q", got, want)
	}
	// It sorts a copy: the principal's own order is untouched.
	if p.Scopes[0] != ScopeTransferExecute {
		t.Fatalf("ScopeString reordered the principal's scopes: %v", p.Scopes)
	}
}

// TestValidAndString covers the validity predicate the signer and the runtime
// both use, and the rendering that lands in logs.
func TestValidAndString(t *testing.T) {
	if err := (Principal{}).Valid(now); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("an empty principal must be ErrNoPrincipal, got %v", err)
	}
	// No expiry means no expiry, not an immediately stale token.
	if err := operator.Valid(now.Add(100 * 365 * 24 * time.Hour)); err != nil {
		t.Fatalf("a principal with no expiry must stay valid, got %v", err)
	}
	expiring := operator
	expiring.Expires = now
	if err := expiring.Valid(now); err != nil {
		t.Fatalf("valid at the expiry instant, got %v", err)
	}
	if err := expiring.Valid(now.Add(time.Nanosecond)); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired a nanosecond later, got %v", err)
	}

	if got := operator.String(); got != "human/j.okafor@collateral-ops" {
		t.Fatalf("String = %q", got)
	}
	if got := agent.String(); got != "agent/svc/collateral-agent@collateral-ops obo:j.okafor" {
		t.Fatalf("agent String = %q; it must name the human it acts for", got)
	}
}

// TestContextPropagation proves the acting principal travels on the context and
// that the two readers differ where it matters: FromContext degrades to
// Anonymous for convenience, MustFromContext refuses, so a decision path cannot
// silently proceed with no identity.
func TestContextPropagation(t *testing.T) {
	ctx := context.Background()

	if got := FromContext(ctx); got.Subject != Anonymous.Subject {
		t.Fatalf("FromContext on a bare context = %+v, want Anonymous", got)
	}
	if _, err := MustFromContext(ctx); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("MustFromContext on a bare context = %v, want ErrNoPrincipal", err)
	}

	ctx = WithPrincipal(ctx, operator)
	if got := FromContext(ctx); got.Subject != operator.Subject {
		t.Fatalf("FromContext = %+v", got)
	}
	got, err := MustFromContext(ctx)
	if err != nil || got.Subject != operator.Subject {
		t.Fatalf("MustFromContext = %+v, %v", got, err)
	}

	// An explicitly anonymous principal is still refused by MustFromContext:
	// asserting nothing is not the same as asserting somebody.
	if _, err := MustFromContext(WithPrincipal(context.Background(), Anonymous)); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("an explicit Anonymous must be refused, got %v", err)
	}
	if _, err := MustFromContext(WithPrincipal(context.Background(), Principal{Tenant: "t"})); !errors.Is(err, ErrNoPrincipal) {
		t.Fatalf("a principal with no subject must be refused, got %v", err)
	}

	// The innermost principal wins, so a nested call cannot inherit the outer one.
	inner := WithPrincipal(ctx, agent)
	if got := FromContext(inner); got.Subject != agent.Subject {
		t.Fatalf("nested WithPrincipal = %+v", got)
	}
	if got := FromContext(ctx); got.Subject != operator.Subject {
		t.Fatalf("the outer context was mutated: %+v", got)
	}
}

// TestMintedTokenIsTransportSafe proves the wire form survives an HTTP header:
// two dot-separated base64url segments with no padding and no reserved
// characters.
func TestMintedTokenIsTransportSafe(t *testing.T) {
	tok, err := NewSigner("k", time.Hour).Mint(agent, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 2 {
		t.Fatalf("token has %d segments", len(parts))
	}
	for i, p := range parts {
		if strings.ContainsAny(p, "=+/ \t\n") {
			t.Fatalf("segment %d is not base64url without padding: %q", i, p)
		}
		if _, err := base64.RawURLEncoding.DecodeString(p); err != nil {
			t.Fatalf("segment %d does not decode: %v", i, err)
		}
	}
	if HeaderPrincipal == "" || HeaderTraceParent == "" {
		t.Fatal("the wire header names must be declared")
	}
}
