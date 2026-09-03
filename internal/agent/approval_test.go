package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/audit"
	"github.com/udaykishore-resu/tcrp/internal/identity"
	"github.com/udaykishore-resu/tcrp/internal/obs"
)

var testSeedInstant = time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)

// The cast: an operator the agent acts for, two people who may approve, and two
// who may not. Four eyes is only a control if the parties are distinct.
var (
	testOperator = identity.Principal{
		Subject: "j.okafor", Kind: identity.KindHuman, Tenant: "collateral-ops",
		Roles:  []string{identity.RoleCollateralOps},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeMarginPropose},
	}
	testAgent = identity.Principal{
		Subject: "svc/collateral-agent", Kind: identity.KindAgent, Tenant: "collateral-ops",
		OnBehalf: "j.okafor",
		Roles:    []string{identity.RoleCollateralOps},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeExposureRead,
			identity.ScopeMarginPropose, identity.ScopeTransferDraft,
			identity.ScopeMarginIssue, identity.ScopeTransferExecute, identity.ScopeRedeem},
	}
	testRiskManager = identity.Principal{
		Subject: "s.aldridge", Kind: identity.KindHuman, Tenant: "collateral-ops",
		Roles:  []string{identity.RoleRiskManager},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeApprove},
	}
	testTreasurer = identity.Principal{
		Subject: "m.duarte", Kind: identity.KindHuman, Tenant: "collateral-ops",
		Roles:  []string{identity.RoleTreasurer},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeApprove},
	}
	// testAuditor holds the approval scope but no authorising role.
	testAuditor = identity.Principal{
		Subject: "k.novak", Kind: identity.KindHuman, Tenant: "collateral-ops",
		Roles:  []string{identity.RoleAuditor},
		Scopes: []string{identity.ScopeCollateralRead, identity.ScopeApprove},
	}
	// testAnalystManager holds an authorising role but not the approval scope.
	testAnalystManager = identity.Principal{
		Subject: "p.oyelaran", Kind: identity.KindHuman, Tenant: "collateral-ops",
		Roles:  []string{identity.RoleRiskManager},
		Scopes: []string{identity.ScopeCollateralRead},
	}
)

type approvalFixture struct {
	svc   *ApprovalService
	store *MemoryStore
	clock *obs.ManualClock
	log   *audit.Log
}

func newApprovalFixture(t *testing.T, window time.Duration) approvalFixture {
	t.Helper()
	clock := obs.NewManualClock(testSeedInstant)
	store, err := NewMemoryStore("")
	if err != nil {
		t.Fatal(err)
	}
	log := audit.New(clock)
	return approvalFixture{
		svc:   NewApprovalService(store, clock, log, nil, window),
		store: store, clock: clock, log: log,
	}
}

func (f approvalFixture) open(t *testing.T, required int) *Approval {
	t.Helper()
	a, err := f.svc.Request(context.Background(), &Approval{
		ID: "apr_test", RunID: "run_test", Step: 4, Tool: "issue_margin_call", Tier: TierMoney,
		Summary:   "issue_margin_call(counterparty=CP-DELTA, amount=35298750)",
		Args:      map[string]any{"counterparty": "CP-DELTA", "amount": 35298750.0},
		Requester: testAgent.Subject, OnBehalfOf: testAgent.OnBehalf,
		RequiredRoles: []string{identity.RoleRiskManager, identity.RoleTreasurer},
		RequiredCount: required,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestAGrantIsRefusedUnlessTheApproverIsSomebodyElseWithTheMandate collects the
// four refusals that make the gate a control rather than a formality.
func TestAGrantIsRefusedUnlessTheApproverIsSomebodyElseWithTheMandate(t *testing.T) {
	cases := []struct {
		name     string
		approver identity.Principal
		want     error
	}{
		{"the agent that requested the action cannot approve it", testAgent, ErrSelfApproval},
		{"the human the agent acts for cannot approve it either", testOperator, ErrSelfApproval},
		{"an approver without the approvals:grant scope is refused", testAnalystManager, ErrApproverScope},
		{"an approver without an authorising role is refused", testAuditor, ErrApproverRole},
		{"a risk manager with both is accepted", testRiskManager, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newApprovalFixture(t, 15*time.Minute)
			a := f.open(t, 1)

			got, err := f.svc.Grant(context.Background(), a.ID, tc.approver, "checked against the exposure report")
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("Grant error = %v, want %v", err, tc.want)
				}
				after, _ := f.svc.Get(context.Background(), a.ID)
				if after.Status != ApprovalPending || len(after.Grants) != 0 {
					t.Fatalf("a refused grant must leave the gate pending and empty: %+v", after)
				}
				return
			}
			if err != nil {
				t.Fatalf("Grant: %v", err)
			}
			if got.Status != ApprovalGranted || len(got.Grants) != 1 {
				t.Fatalf("approval = %s with %d grants", got.Status, len(got.Grants))
			}
		})
	}
}

func TestTheSameApproverCannotSatisfyBothHalvesOfADualApproval(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 2)

	first, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "first")
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != ApprovalPending {
		t.Fatalf("one of two grants must leave the gate pending, got %s", first.Status)
	}

	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "again"); !errors.Is(err, ErrDuplicateGrant) {
		t.Fatalf("second grant by the same approver = %v, want ErrDuplicateGrant", err)
	}

	second, err := f.svc.Grant(context.Background(), a.ID, testTreasurer, "second")
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != ApprovalGranted {
		t.Fatalf("two distinct approvers must satisfy the gate, got %s", second.Status)
	}
	if got := second.Approvers(); len(got) != 2 || got[0] != testRiskManager.Subject || got[1] != testTreasurer.Subject {
		t.Fatalf("approvers = %v", got)
	}
	if roles := second.ApproverRoles(); len(roles) != 2 {
		t.Fatalf("approver roles = %v, want both mandates recorded", roles)
	}
}

func TestAnExpiredWindowCannotBeRevived(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 1)

	f.clock.Advance(15*time.Minute + time.Second)

	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "late"); !errors.Is(err, ErrApprovalExpired) {
		t.Fatalf("Grant after the window = %v, want ErrApprovalExpired", err)
	}
	after, _ := f.svc.Get(context.Background(), a.ID)
	if after.Status != ApprovalExpired {
		t.Fatalf("status = %s, want expired", after.Status)
	}
	if len(after.Grants) != 0 {
		t.Fatalf("an expired gate must record no grant: %+v", after.Grants)
	}
	if _, err := f.svc.Grant(context.Background(), a.ID, testTreasurer, "later still"); !errors.Is(err, ErrApprovalClosed) {
		t.Fatalf("a second attempt = %v, want ErrApprovalClosed", err)
	}
}

func TestAGrantJustInsideTheWindowStillCounts(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 1)
	f.clock.Advance(15 * time.Minute)

	got, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "on the bell")
	if err != nil {
		t.Fatalf("Grant at the boundary: %v", err)
	}
	if got.Status != ApprovalGranted {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestExpireDueClosesOnlyTheGatesWhoseWindowHasPassed(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	soon := f.open(t, 1)

	later, err := f.svc.Request(context.Background(), &Approval{
		ID: "apr_later", RunID: "run_test", Tool: "execute_transfer", Tier: TierMoney,
		Requester: testAgent.Subject, RequiredCount: 1,
		RequiredRoles: []string{identity.RoleRiskManager},
		ExpiresAt:     f.clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	f.clock.Advance(20 * time.Minute)
	expired, err := f.svc.ExpireDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].ID != soon.ID {
		t.Fatalf("expired = %v, want only %s", expired, soon.ID)
	}
	still, _ := f.svc.Get(context.Background(), later.ID)
	if still.Status != ApprovalPending {
		t.Fatalf("the longer window must stay open, got %s", still.Status)
	}
	pending, _ := f.svc.Pending(context.Background())
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
}

func TestADenialClosesTheGateAgainstTheAction(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 2)

	got, err := f.svc.Deny(context.Background(), a.ID, testRiskManager, "the mark is stale")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ApprovalDenied {
		t.Fatalf("status = %s, want denied", got.Status)
	}
	if len(got.Denials) != 1 || got.Denials[0].By != testRiskManager.Subject {
		t.Fatalf("denials = %+v", got.Denials)
	}
	// One refusal is enough even where two approvals would have been needed.
	if _, err := f.svc.Grant(context.Background(), a.ID, testTreasurer, "override"); !errors.Is(err, ErrApprovalClosed) {
		t.Fatalf("Grant after a denial = %v, want ErrApprovalClosed", err)
	}
}

func TestOnlyAnAuthorisedRoleMayDeny(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 1)
	if _, err := f.svc.Deny(context.Background(), a.ID, testAuditor, "no"); !errors.Is(err, ErrApproverRole) {
		t.Fatalf("Deny by an auditor = %v, want ErrApproverRole", err)
	}
}

func TestAnApproverWhoAlreadyDeniedCannotThenGrant(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 2)
	// Deny closes the gate outright, so reopen the question at the level the
	// duplicate check is written: a recorded response of either kind.
	a.Denials = append(a.Denials, Grant{By: testRiskManager.Subject, At: f.clock.Now()})
	if err := f.store.SaveApproval(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "changed my mind"); !errors.Is(err, ErrDuplicateGrant) {
		t.Fatalf("Grant after a denial by the same person = %v, want ErrDuplicateGrant", err)
	}
}

func TestARequestRecordsWhatWillBeApprovedSoTheCardCannotDrift(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 2)

	if a.ArgsHash == "" {
		t.Fatal("the arguments must be hashed at request time")
	}
	if a.ExpiresAt != testSeedInstant.Add(15*time.Minute) {
		t.Fatalf("expiry = %s, want the configured window", a.ExpiresAt)
	}
	if a.Status != ApprovalPending {
		t.Fatalf("status = %s", a.Status)
	}
	if a.Satisfied() {
		t.Fatal("a fresh dual-approval gate is not satisfied")
	}
	found := false
	for _, r := range f.log.ByRun("run_test") {
		if r.Action == audit.ActionApprovalRequired {
			found = true
			if r.Details["approval_id"] != a.ID {
				t.Fatalf("audit record names approval %v", r.Details["approval_id"])
			}
		}
	}
	if !found {
		t.Fatal("opening a gate must leave an audit record")
	}
}

func TestAMissingRequiredCountDefaultsToOneApprover(t *testing.T) {
	f := newApprovalFixture(t, 0) // a zero window also takes the shipped default
	a, err := f.svc.Request(context.Background(), &Approval{
		ID: "apr_default", RunID: "run_test", Tool: "execute_transfer", Tier: TierMoney,
		Requester: testAgent.Subject, RequiredRoles: []string{identity.RoleRiskManager},
	})
	if err != nil {
		t.Fatal(err)
	}
	if a.RequiredCount != 1 {
		t.Fatalf("required count = %d, want 1", a.RequiredCount)
	}
	if a.ExpiresAt != testSeedInstant.Add(15*time.Minute) {
		t.Fatalf("expiry = %s, want the default fifteen minute window", a.ExpiresAt)
	}
}

func TestAGateWithNoRequiredRolesAcceptsAnyoneHoldingTheApprovalScope(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a, err := f.svc.Request(context.Background(), &Approval{
		ID: "apr_open", RunID: "run_test", Tool: "propose_margin_call", Tier: TierState,
		Requester: testAgent.Subject, RequiredCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Grant(context.Background(), a.ID, testAuditor, "fine"); err != nil {
		t.Fatalf("Grant: %v", err)
	}
}

func TestWaitReturnsAsSoonAsTheGateIsDecided(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan *Approval, 1)
	errs := make(chan error, 1)
	go func() {
		got, err := f.svc.Wait(ctx, a.ID)
		if err != nil {
			errs <- err
			return
		}
		done <- got
	}()

	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "go"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.Status != ApprovalGranted {
			t.Fatalf("Wait returned %s", got.Status)
		}
	case err := <-errs:
		t.Fatalf("Wait: %v", err)
	case <-ctx.Done():
		t.Fatal("Wait did not return after the gate was decided")
	}
}

func TestWaitReturnsImmediatelyForAGateThatIsAlreadyClosed(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 1)
	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "go"); err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.Wait(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ApprovalGranted {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestAnUnknownApprovalIsNotFound(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	if _, err := f.svc.Get(context.Background(), "apr_nothing"); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("Get = %v, want ErrApprovalNotFound", err)
	}
	if _, err := f.svc.Grant(context.Background(), "apr_nothing", testRiskManager, ""); !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("Grant = %v, want ErrApprovalNotFound", err)
	}
}

func TestLastGrantAtIsTheMostRecentResponse(t *testing.T) {
	f := newApprovalFixture(t, time.Hour)
	a := f.open(t, 2)
	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "first"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(3 * time.Minute)
	got, err := f.svc.Grant(context.Background(), a.ID, testTreasurer, "second")
	if err != nil {
		t.Fatal(err)
	}
	if want := testSeedInstant.Add(3 * time.Minute); !got.LastGrantAt().Equal(want) {
		t.Fatalf("LastGrantAt = %s, want %s", got.LastGrantAt(), want)
	}
}

func TestAnApprovalSurvivesARoundTripThroughTheStore(t *testing.T) {
	f := newApprovalFixture(t, 15*time.Minute)
	a := f.open(t, 2)
	if _, err := f.svc.Grant(context.Background(), a.ID, testRiskManager, "first"); err != nil {
		t.Fatal(err)
	}
	loaded, err := f.store.LoadApproval(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A clone, not the live object: mutating what a caller was handed must not
	// change what the next reader sees.
	loaded.Grants = nil
	again, _ := f.store.LoadApproval(context.Background(), a.ID)
	if len(again.Grants) != 1 {
		t.Fatalf("the store handed out a live pointer; grants = %d", len(again.Grants))
	}
}
