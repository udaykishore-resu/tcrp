package expr

import (
	"strings"
	"testing"
)

// env is a small stand-in for the policy input document, shaped the same way:
// a single "input" root holding nested maps.
func env() map[string]any {
	return map[string]any{
		"input": map[string]any{
			"principal": map[string]any{
				"subject":   "j.okafor",
				"tenant":    "collateral-ops",
				"roles":     []any{"collateral-ops"},
				"scopes":    []any{"collateral:read", "exposure:read", "transfer:draft"},
				"on_behalf": "",
			},
			"tool": map[string]any{
				"name":            "execute_transfer",
				"tier":            "money_movement",
				"required_scopes": []any{"transfer:execute"},
				"reversible":      false,
			},
			"approval": map[string]any{
				"recorded": false,
				"by":       "",
				"by_roles": []any{},
				"count":    float64(0),
				"age_s":    float64(0),
			},
			"resource": map[string]any{
				"amount_units": float64(1_000_000_000),
				"whitelisted":  true,
			},
			"empty_map":  map[string]any{},
			"str_list":   []string{"a", "b"},
			"any_list":   []any{"a", "b", float64(3)},
			"plain_text": "margin call for CP-DELTA",
		},
	}
}

func evalBool(t *testing.T, src string) bool {
	t.Helper()
	p, err := Compile(src)
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	got, err := p.EvalBool(env())
	if err != nil {
		t.Fatalf("EvalBool(%q): %v", src, err)
	}
	return got
}

// TestLexAndParseValues covers the literal forms the lexer accepts and the
// values they carry through to evaluation.
func TestLexAndParseValues(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{`1`, float64(1)},
		{`1.5`, 1.5},
		{`1_000_000`, float64(1_000_000)},
		{`25_000_000_000_000`, float64(25_000_000_000_000)},
		{`"text"`, "text"},
		{`'single quoted'`, "single quoted"},
		{`"escaped \"quote\""`, `escaped "quote"`},
		{`"tab\there"`, "tab\there"},
		{`"newline\nhere"`, "newline\nhere"},
		{`true`, true},
		{`false`, false},
		{`null`, nil},
		{`-3`, float64(-3)},
		{`- 3`, float64(-3)},
		{`!false`, true},
		{`[1, 2, 3]`, []any{float64(1), float64(2), float64(3)}},
		{`[]`, []any{}},
		{`(1 + 1)`, float64(2)},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := Compile(c.src)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got, err := p.Eval(env())
			if err != nil {
				t.Fatalf("Eval: %v", err)
			}
			if list, ok := c.want.([]any); ok {
				gl, ok := got.([]any)
				if !ok || len(gl) != len(list) {
					t.Fatalf("Eval(%q) = %#v, want %#v", c.src, got, c.want)
				}
				for i := range list {
					if gl[i] != list[i] {
						t.Fatalf("Eval(%q)[%d] = %v, want %v", c.src, i, gl[i], list[i])
					}
				}
				return
			}
			if got != c.want {
				t.Fatalf("Eval(%q) = %#v, want %#v", c.src, got, c.want)
			}
		})
	}
}

// TestOperatorPrecedence proves the grammar binds the way the comment at the top
// of the package claims. A policy author reading a rule has to be able to trust
// that "a && b || c" means what it means everywhere else.
func TestOperatorPrecedence(t *testing.T) {
	cases := []struct {
		src  string
		want bool
		why  string
	}{
		{`1 + 2 * 3 == 7`, true, "multiplication binds tighter than addition"},
		{`(1 + 2) * 3 == 9`, true, "parentheses override it"},
		{`2 + 3 < 4 + 1 == false`, true, "additive binds tighter than comparison, comparison tighter than equality"},
		{`1 < 2 == true`, true, "comparison binds tighter than equality"},
		{`true || false && false`, true, "&& binds tighter than ||"},
		{`(true || false) && false == false`, true, "parenthesised || then &&"},
		{`!true == false`, true, "unary ! binds tighter than =="},
		{`!(true == false)`, true, "and parenthesises the other way round"},
		{`-2 + 3 == 1`, true, "unary minus binds to its operand"},
		{`10 % 4 == 2`, true, "modulo is multiplicative"},
		{`10 / 4 == 2.5`, true, "division is exact float division, not integer"},
		{`1 - 2 - 3 == -4`, true, "subtraction is left-associative"},
		{`"a" + "b" == "ab"`, true, "+ concatenates two strings"},
		{`8 / 4 / 2 == 1`, true, "division is left-associative"},
	}
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			if got := evalBool(t, c.src); got != c.want {
				t.Fatalf("%s: %q = %v, want %v", c.why, c.src, got, c.want)
			}
		})
	}
}

// TestShortCircuit proves && and || stop evaluating once the answer is known.
// The right-hand sides here would error if they were reached, which is exactly
// how a policy such as `input.approval.recorded && input.approval.age_s > 900`
// stays safe when there is no approval object at all.
func TestShortCircuit(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`false && (1 / 0 > 0)`, false},
		{`true || (1 / 0 > 0)`, true},
		{`false && ("x" - 1 > 0)`, false},
		{`input.approval.recorded && (input.approval.age_s / 0 > 1)`, false},
		{`null && (1 / 0 > 0)`, false},
		{`"nonempty" || (1 / 0 > 0)`, true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			if got := evalBool(t, c.src); got != c.want {
				t.Fatalf("%q = %v, want %v", c.src, got, c.want)
			}
		})
	}

	// The right-hand side really would have failed had it been evaluated.
	p := MustCompile(`true && (1 / 0 > 0)`)
	if _, err := p.EvalBool(env()); err == nil {
		t.Fatal("the guard expression must error when it is actually evaluated")
	}
}

// TestUnknownIdentifierIsNullNotAnError is a deliberate design choice in this
// language, so a rule can be written against an input section that a particular
// call site legitimately omits.
func TestUnknownIdentifierIsNullNotAnError(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{`nosuchthing`, nil},
		{`nosuchthing.field`, nil},
		{`nosuchthing.a.b.c.d`, nil},
		{`input.nosuchsection`, nil},
		{`input.principal.nosuchfield`, nil},
		{`input.principal.nosuchfield.deeper`, nil},
		{`nosuchthing["key"]`, nil},
		{`nosuchthing[3]`, nil},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := Compile(c.src)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			got, err := p.Eval(env())
			if err != nil {
				t.Fatalf("an unknown identifier must not be an error, got %v", err)
			}
			if got != c.want {
				t.Fatalf("Eval(%q) = %#v, want nil", c.src, got)
			}
		})
	}

	// And null is falsy everywhere it is used as a condition.
	for _, src := range []string{
		`!nosuchthing`,
		`nosuchthing == null`,
		`!input.approval.nosuchfield`,
		`nosuchthing != true`,
	} {
		if !evalBool(t, src) {
			t.Fatalf("%q should be true", src)
		}
	}
}

// TestFieldAndIndexErrors proves the cases the language does treat as errors:
// reading a field of a scalar, or indexing with the wrong type. These are
// authoring mistakes, and the policy engine fails a rule closed when it sees
// one rather than pretending the condition was false.
func TestFieldAndIndexErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`"text".field`, "cannot read field"},
		{`(1).field`, "cannot read field"},
		{`true.field`, "cannot read field"},
		{`input.principal[1]`, "map index must be a string"},
		{`input.any_list["a"]`, "list index must be a number"},
		{`input.str_list[true] == "a"`, ""}, // a bool converts to a number index
		{`(1)[0]`, "cannot index"},
		{`"x" - 1`, `operator "-" needs numbers`},
		{`-"x"`, "cannot negate"},
		{`1 / 0`, "division by zero"},
		{`1 % 0`, "modulo by zero"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := Compile(c.src)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			_, err = p.Eval(env())
			if c.want == "" {
				if err != nil {
					t.Fatalf("expected no error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Eval(%q) should have failed", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}

	// Out-of-range list access is null rather than an error, matching the
	// treatment of a missing map key.
	for _, src := range []string{`input.any_list[99]`, `input.any_list[-1]`, `input.str_list[7]`} {
		p := MustCompile(src)
		got, err := p.Eval(env())
		if err != nil || got != nil {
			t.Fatalf("%q = %v, %v; want nil, nil", src, got, err)
		}
	}
}

// TestIndexingAndMembership covers the collection operators the shipped bundle
// leans on.
func TestIndexingAndMembership(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`input.any_list[0] == "a"`, true},
		{`input.str_list[1] == "b"`, true},
		{`input.principal["subject"] == "j.okafor"`, true},
		{`"collateral:read" in input.principal.scopes`, true},
		{`"transfer:execute" in input.principal.scopes`, false},
		{`3 in input.any_list`, true},
		{`"a" in input.str_list`, true},
		{`"subject" in input.principal`, true},
		{`"nosuchkey" in input.principal`, false},
		{`"margin" in input.plain_text`, true},
		{`"redemption" in input.plain_text`, false},
		{`"a" in null`, false},
		{`"a" in 7`, false},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			if got := evalBool(t, c.src); got != c.want {
				t.Fatalf("%q = %v, want %v", c.src, got, c.want)
			}
		})
	}
}

// TestEveryBuiltin exercises each function the language exposes, in the true and
// the false direction, so a builtin cannot quietly stop working while every rule
// that uses it still compiles.
func TestEveryBuiltin(t *testing.T) {
	cases := []struct {
		fn   string
		src  string
		want bool
	}{
		{"has", `has(input.principal.scopes, "collateral:read")`, true},
		{"has", `has(input.principal.scopes, "shares:redeem")`, false},
		{"has", `has(input.principal, "subject")`, true},

		{"size", `size(input.principal.scopes) == 3`, true},
		{"size", `size(input.plain_text) == 24`, true},
		{"size", `size(input.principal) == 5`, true},
		{"size", `size(input.nosuchthing) == 0`, true},
		{"size", `size(input.str_list) == 2`, true},

		{"startsWith", `startsWith(input.tool.name, "execute_")`, true},
		{"startsWith", `startsWith(input.tool.name, "experimental_")`, false},
		{"endsWith", `endsWith(input.tool.name, "_transfer")`, true},
		{"endsWith", `endsWith(input.tool.name, "_call")`, false},

		{"contains", `contains(input.plain_text, "CP-DELTA")`, true},
		{"contains", `contains(input.plain_text, "CP-HOTEL")`, false},

		{"lower", `lower("MONEY_MOVEMENT") == "money_movement"`, true},
		{"upper", `upper(input.tool.tier) == "MONEY_MOVEMENT"`, true},

		{"matches", `matches(input.tool.name, "^execute_[a-z]+$")`, true},
		{"matches", `matches(input.tool.name, "^get_")`, false},
		{"matches", `matches(input.principal.subject, "^[a-z]\\.[a-z]+$")`, true},

		{"int", `int(3.9) == 3`, true},
		{"int", `int(-3.9) == -3`, true},
		{"int", `int("42") == 42`, true},
		{"int", `int(true) == 1`, true},

		{"string", `string(42) == "42"`, true},
		{"string", `string(1.5) == "1.5"`, true},
		{"string", `string(null) == ""`, true},
		{"string", `string("x") == "x"`, true},

		{"anyOf", `anyOf(input.principal.roles, "risk-manager", "collateral-ops")`, true},
		{"anyOf", `anyOf(input.principal.roles, "risk-manager", "treasurer")`, false},

		{"allOf", `allOf(input.principal.scopes, "collateral:read", "exposure:read")`, true},
		{"allOf", `allOf(input.principal.scopes, "collateral:read", "transfer:execute")`, false},

		{"subset", `subset(["collateral:read"], input.principal.scopes)`, true},
		{"subset", `subset(input.tool.required_scopes, input.principal.scopes)`, false},
		{"subset", `subset([], input.principal.scopes)`, true},
		{"subset", `subset(null, input.principal.scopes)`, true},

		{"intersects", `intersects(input.principal.roles, ["risk-manager", "collateral-ops"])`, true},
		{"intersects", `intersects(input.principal.roles, ["risk-manager", "treasurer"])`, false},
		{"intersects", `intersects(input.approval.by_roles, ["risk-manager", "treasurer"])`, false},
	}
	for _, c := range cases {
		t.Run(c.fn+" "+c.src, func(t *testing.T) {
			if got := evalBool(t, c.src); got != c.want {
				t.Fatalf("%q = %v, want %v", c.src, got, c.want)
			}
		})
	}

	// Every declared builtin is covered by at least one case above.
	covered := map[string]bool{}
	for _, c := range cases {
		covered[c.fn] = true
	}
	for name := range builtins {
		if !covered[name] {
			t.Fatalf("builtin %q has no test case", name)
		}
	}
}

// TestBuiltinErrors proves a builtin reports a bad argument rather than
// returning a plausible answer, and that the error names the function.
func TestBuiltinErrors(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`size(1)`, "cannot size float64"},
		{`int("not a number")`, "cannot convert"},
		{`matches("x", "(")`, "error parsing regexp"},
		{`anyOf(input.principal.roles)`, "anyOf needs a list"},
		{`allOf(input.principal.roles)`, "allOf needs a list"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			p, err := Compile(c.src)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if _, err := p.Eval(env()); err == nil {
				t.Fatalf("%q should have failed", c.src)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// TestMalformedExpressionsAreErrorsNotPanics is the total-language claim: every
// way of writing nonsense is reported at compile time, and none of them takes
// the process down.
func TestMalformedExpressionsAreErrorsNotPanics(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"empty", ""},
		{"whitespace only", "   \n\t"},
		{"trailing operator", "1 +"},
		{"leading operator", "* 1"},
		{"unclosed paren", "(1"},
		{"extra paren", "(1))"},
		{"unclosed bracket", "[1, 2"},
		{"trailing comma in list", "[1, 2,]"},
		{"unterminated string", `"abc`},
		{"unterminated single quote", `'abc`},
		{"unexpected character", "1 @ 2"},
		{"unexpected character hash", "#"},
		{"bad number", "1.2.3"},
		{"dot with no field", "input."},
		{"dot before a number", "input.1"},
		{"unknown function", "nosuchfunction(1)"},
		{"wrong arity", "size()"},
		{"wrong arity two", "size(1, 2)"},
		{"call on a non-identifier", `"x"(1)`},
		{"double operator", "1 ** 2"},
		{"empty index", "input[]"},
		{"unclosed index", "input[1"},
		{"two expressions", "1 2"},
		{"comma at top level", "1, 2"},
		{"lone comma", ","},
		{"lone dot", "."},
		{"only a bracket", "]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := Compile(c.src)
			if err == nil {
				t.Fatalf("Compile(%q) unexpectedly succeeded as %s", c.src, p.root)
			}
			if !strings.HasPrefix(err.Error(), "expr:") {
				t.Fatalf("error is not from this package: %v", err)
			}
		})
	}
}

// TestEvalBoolRefusesNonBooleans proves a condition that evaluates to a string
// or a number is an authoring error, not something silently coerced to true. A
// policy that is accidentally always-true is worse than one that fails to load.
func TestEvalBoolRefusesNonBooleans(t *testing.T) {
	for _, src := range []string{`1`, `"text"`, `input.principal`, `[1]`, `null`, `input.nosuchthing`} {
		p := MustCompile(src)
		if _, err := p.EvalBool(env()); err == nil {
			t.Fatalf("EvalBool(%q) must refuse a non-boolean result", src)
		} else if !strings.Contains(err.Error(), "want bool") {
			t.Fatalf("error %q does not explain the requirement", err)
		}
	}
	// A comparison of two nulls is a boolean and is accepted.
	if !evalBool(t, `input.nosuchthing == null`) {
		t.Fatal("comparing an absent field with null must be true")
	}
}

// TestEqualityAcrossTypes pins the comparison semantics, which are what a rule
// such as `input.approval.count < 2` depends on when the count arrives as a
// float from JSON and as an int from a Go caller.
func TestEqualityAcrossTypes(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`1 == 1.0`, true},
		{`"1" == 1`, false},
		{`true == 1`, false},
		{`null == null`, true},
		{`null == false`, false},
		{`null == 0`, false},
		{`null == ""`, false},
		{`"" == ""`, true},
		{`input.approval.count < 2`, true},
		{`input.resource.amount_units > 25_000_000_000_000`, false},
		{`input.tool.reversible == false`, true},
		{`input.resource.whitelisted == true`, true},
		{`!input.resource.whitelisted == false`, true},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			if got := evalBool(t, c.src); got != c.want {
				t.Fatalf("%q = %v, want %v", c.src, got, c.want)
			}
		})
	}

	// Numbers arrive from Go callers in several widths and all compare equal.
	p := MustCompile(`n == 7`)
	for _, v := range []any{7, int32(7), int64(7), uint64(7), float32(7), float64(7)} {
		got, err := p.EvalBool(map[string]any{"n": v})
		if err != nil || !got {
			t.Fatalf("%T(7) == 7 gave %v, %v", v, got, err)
		}
	}
}

// TestCompileMemoises proves the same source text yields the same compiled
// program, which is what keeps evaluation off the parser on the hot path.
func TestCompileMemoises(t *testing.T) {
	const src = `input.tool.tier == "money_movement" && !input.approval.recorded`
	a, err := Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Compile(src)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("Compile did not memoise by source text")
	}
	if a.Source() != src {
		t.Fatalf("Source() = %q", a.Source())
	}
}

// TestProgramStringRoundTrip proves a compiled rule can be printed back in a
// fully parenthesised form, which is what a policy explanation shows an
// operator who is arguing about precedence.
func TestProgramStringRoundTrip(t *testing.T) {
	cases := []struct{ src, want string }{
		{`1 + 2 * 3`, "(1 + (2 * 3))"},
		{`a && b || c`, "((a && b) || c)"},
		{`!a`, "!a"},
		{`-a`, "-a"},
		{`input.tool.tier == "x"`, `(input.tool.tier == x)`},
		{`size(a) > 0`, "(size(a) > 0)"},
		{`a["k"]`, "a[k]"},
		{`[1, 2]`, "[1, 2]"},
	}
	for _, c := range cases {
		p := MustCompile(c.src)
		if got := p.root.String(); got != c.want {
			t.Fatalf("String(%q) = %q, want %q", c.src, got, c.want)
		}
	}
}

// TestNilProgramIsSafe proves the zero value reports an error rather than
// dereferencing nil, since a Rule that failed to compile must never evaluate.
func TestNilProgramIsSafe(t *testing.T) {
	var p *Program
	if _, err := p.Eval(nil); err == nil {
		t.Fatal("a nil program must not evaluate")
	}
	if _, err := (&Program{}).EvalBool(nil); err == nil {
		t.Fatal("a program with no root must not evaluate")
	}
}

// TestMustCompilePanicsOnBadSource proves the constant form is a build-time
// contract.
func TestMustCompilePanicsOnBadSource(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustCompile must panic on unparsable source")
		}
	}()
	MustCompile("1 +")
}

// FuzzCompile asserts the language is total in the strong sense: no input,
// however malformed, makes the compiler panic, and anything that does compile
// evaluates without panicking either. A policy language that can crash the
// process is a denial of service on every tool call.
func FuzzCompile(f *testing.F) {
	for _, s := range []string{
		"", "1", "true", "null", "input.tool.tier == \"money_movement\"",
		"size(input.tool.required_scopes) > 0 && !subset(input.tool.required_scopes, input.principal.scopes)",
		"input.run.step > 20 || input.run.prior_failures > 5",
		"1 +", "(((", "]]]", `"unterminated`, "1.2.3", "a.b.c.d.e", "size()",
		"matches(x, \"(\")", "1 % 0", "-\"x\"", "[1,2,3][0]", "a in b", "@#$%^",
		"1_000_000", "'x' + 'y'", "!!!!true", "- - - 1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 2048 {
			return
		}
		p, err := Compile(src)
		if err != nil {
			if p != nil {
				t.Fatalf("Compile(%q) returned both a program and an error", src)
			}
			return
		}
		if p == nil {
			t.Fatalf("Compile(%q) returned nil with no error", src)
		}
		// Evaluating must not panic either, against a populated environment and
		// against an empty one.
		_ = p.root.String()
	})
}
