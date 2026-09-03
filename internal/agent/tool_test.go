package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// stubTool is a tool with a declared contract and a no-op body, used wherever a
// test cares about what a tool declares rather than what it does.
func stubTool(s ToolSpec) Tool {
	return ToolFunc{S: s, F: func(context.Context, map[string]any) (ToolResult, error) {
		return ToolResult{Content: "ok"}, nil
	}}
}

func readSpec(name string) ToolSpec {
	return ToolSpec{Name: name, Tier: TierRead, Reversible: true, RequiredScopes: []string{"collateral:read"}}
}

func stateSpec(name string) ToolSpec {
	return ToolSpec{Name: name, Tier: TierState, Reversible: true, RequiredScopes: []string{"margin:propose"}}
}

func moneySpec(name string) ToolSpec {
	return ToolSpec{Name: name, Tier: TierMoney, RequiredScopes: []string{"margin:issue"}}
}

// TestRegistrationRefusesAToolNobodyCouldAuthorizeCorrectly. Each refusal is a
// declaration that cannot be enforced later: an unknown tier has no policy
// written against it, a scopeless state change cannot be granted, and a
// reversible money movement is a claim the ledger will not honour.
func TestRegistrationRefusesAToolNobodyCouldAuthorizeCorrectly(t *testing.T) {
	cases := []struct {
		name string
		spec ToolSpec
		want string // substring of the expected error; "" means the tool is accepted
	}{
		{
			name: "a well formed read tool is accepted",
			spec: readSpec("get_exposure"),
		},
		{
			name: "a read tool needs no scopes",
			spec: ToolSpec{Name: "ping", Tier: TierRead, Reversible: true},
		},
		{
			name: "a tool with no name",
			spec: ToolSpec{Tier: TierRead},
			want: "no name",
		},
		{
			name: "a tool with no tier",
			spec: ToolSpec{Name: "get_exposure"},
			want: "declares no valid tier",
		},
		{
			name: "a tool with an invented tier",
			spec: ToolSpec{Name: "get_exposure", Tier: Tier("mostly_harmless")},
			want: "declares no valid tier",
		},
		{
			name: "a state changing tool with no declared scopes",
			spec: ToolSpec{Name: "propose_margin_call", Tier: TierState},
			want: "must declare required scopes",
		},
		{
			name: "a money movement tool with no declared scopes",
			spec: ToolSpec{Name: "execute_transfer", Tier: TierMoney},
			want: "must declare required scopes",
		},
		{
			name: "a money movement tool that claims to be reversible",
			spec: ToolSpec{Name: "execute_transfer", Tier: TierMoney, Reversible: true, RequiredScopes: []string{"transfer:execute"}},
			want: "claims money movement is reversible",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			err := r.Register(stubTool(tc.spec))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Register: %v", err)
			case tc.want == "":
				if r.Len() != 1 {
					t.Fatalf("registry holds %d tools, want 1", r.Len())
				}
			case err == nil:
				t.Fatalf("Register accepted %+v, want an error mentioning %q", tc.spec, tc.want)
			case !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestASecondToolWithTheSameNameIsRefused(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(stubTool(readSpec("get_exposure"))); err != nil {
		t.Fatal(err)
	}
	err := r.Register(stubTool(moneySpec("get_exposure")))
	if err == nil {
		t.Fatal("a duplicate name must be refused; otherwise a money tool can shadow a read one")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("error = %q", err)
	}
	got, _ := r.Get("get_exposure")
	if got.Spec().Tier != TierRead {
		t.Fatalf("the first registration must stand, got tier %s", got.Spec().Tier)
	}
}

func TestMustRegisterPanicsOnARefusedTool(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustRegister must panic rather than start an ungoverned runtime")
		}
	}()
	NewRegistry().MustRegister(stubTool(ToolSpec{Name: "execute_transfer", Tier: TierMoney}))
}

func TestSpecsForTiersHidesEverythingAboveThePrincipalsCeiling(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(
		stubTool(moneySpec("execute_transfer")),
		stubTool(readSpec("get_exposure")),
		stubTool(stateSpec("propose_margin_call")),
		stubTool(readSpec("check_whitelist")),
	)
	cases := []struct {
		max  Tier
		want []string
	}{
		{TierRead, []string{"check_whitelist", "get_exposure"}},
		{TierState, []string{"check_whitelist", "get_exposure", "propose_margin_call"}},
		{TierMoney, []string{"check_whitelist", "get_exposure", "propose_margin_call", "execute_transfer"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.max), func(t *testing.T) {
			var got []string
			for _, s := range r.SpecsForTiers(tc.max) {
				got = append(got, s.Name)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("specs = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpecOrderIsStableSoATrajectoryIsComparable: the model sees the same tool
// list every run, tier ascending then name.
func TestSpecOrderIsStableSoATrajectoryIsComparable(t *testing.T) {
	build := func() *Registry {
		r := NewRegistry()
		r.MustRegister(
			stubTool(moneySpec("redeem_shares")),
			stubTool(readSpec("query_asof")),
			stubTool(moneySpec("execute_transfer")),
			stubTool(stateSpec("draft_transfer")),
			stubTool(readSpec("get_exposure")),
		)
		return r
	}
	want := []string{"get_exposure", "query_asof", "draft_transfer", "execute_transfer", "redeem_shares"}
	for i := range 5 {
		if got := build().Names(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: names = %v, want %v", i, got, want)
		}
	}
}

func TestAnUnregisteredToolIsNotFound(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(stubTool(readSpec("get_exposure")))
	if _, ok := r.Get("wire_the_money"); ok {
		t.Fatal("Get returned a tool that was never registered")
	}
}

func TestTierRankOrdersByBlastRadius(t *testing.T) {
	if !(TierRead.Rank() < TierState.Rank() && TierState.Rank() < TierMoney.Rank()) {
		t.Fatal("tiers must order read < state < money")
	}
	if Tier("").Valid() || Tier("nonsense").Valid() {
		t.Fatal("only the declared tiers are valid")
	}
	for _, tr := range []Tier{TierRead, TierState, TierMoney} {
		if !tr.Valid() {
			t.Fatalf("%s should be valid", tr)
		}
	}
}

func TestJSONSchemaRendersOnlyDeclaredParamsAndRequiredNames(t *testing.T) {
	spec := ToolSpec{
		Name: "execute_transfer", Tier: TierMoney, RequiredScopes: []string{"transfer:execute"},
		Params: []Param{
			{Name: "counterparty", Type: "string", Description: "Destination.", Required: true},
			{Name: "quantity", Type: "number", Required: true},
			{Name: "ledger", Type: "string", Enum: []string{"evm", "canton"}},
		},
	}
	schema := spec.JSONSchema()
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) != 3 {
		t.Fatalf("properties = %v", schema["properties"])
	}
	if got := props["ledger"].(map[string]any)["enum"]; !reflect.DeepEqual(got, []string{"evm", "canton"}) {
		t.Fatalf("enum = %v", got)
	}
	if _, has := props["counterparty"].(map[string]any)["description"]; !has {
		t.Fatal("a described parameter must carry its description into the schema")
	}
	if got := schema["required"]; !reflect.DeepEqual(got, []string{"counterparty", "quantity"}) {
		t.Fatalf("required = %v", got)
	}
}

func TestASchemaWithNoRequiredParamsOmitsTheKey(t *testing.T) {
	schema := ToolSpec{Name: "list_recon_breaks", Tier: TierRead}.JSONSchema()
	if _, has := schema["required"]; has {
		t.Fatal("an empty required list must be omitted rather than rendered as null")
	}
}

func TestToolFuncCarriesItsErrorThrough(t *testing.T) {
	sentinel := errors.New("upstream refused")
	tool := ToolFunc{
		S: readSpec("get_exposure"),
		F: func(context.Context, map[string]any) (ToolResult, error) {
			return ToolResult{Retryable: true}, sentinel
		},
	}
	res, err := tool.Invoke(context.Background(), nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the sentinel", err)
	}
	if !res.Retryable {
		t.Fatal("the retryable flag must survive the error path; the runtime reads it")
	}
}
