package money

import (
	"errors"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

// TestParseFormatRoundTrip proves that String is a faithful rendering of the
// stored units: re-parsing what String printed recovers the same Amount, so a
// figure can cross a text boundary and come back unchanged.
func TestParseFormatRoundTrip(t *testing.T) {
	cases := []struct {
		in    string
		units int64
	}{
		{"0", 0},
		{"0.000001", 1},
		{"1", 1_000_000},
		{"1.5", 1_500_000},
		{"1234.56", 1_234_560_000},
		{"-1234.56", -1_234_560_000},
		{"+42", 42_000_000},
		{"0.999999", 999_999},
		{"-0.000001", -1},
		{"  12.25  ", 12_250_000},
		{"9223372036854.775807", math.MaxInt64},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			a, err := Parse(USD, c.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.in, err)
			}
			if a.Units != c.units {
				t.Fatalf("Parse(%q).Units = %d, want %d", c.in, a.Units, c.units)
			}
			back, err := Parse(USD, decimalOf(a.String()))
			if err != nil {
				t.Fatalf("re-parsing %q: %v", a.String(), err)
			}
			if back != a {
				t.Fatalf("round trip: %v -> %q -> %v", a, a.String(), back)
			}
		})
	}
}

// decimalOf strips the currency code String appends.
func decimalOf(s string) string { return strings.Fields(s)[0] }

// TestParseRejectsMalformed proves Parse never guesses. In particular a seventh
// decimal place is refused rather than truncated: silently dropping a digit is
// how a reconciliation break is born.
func TestParseRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrBadFormat},
		{"blank", "   ", ErrBadFormat},
		{"seven decimals", "1.0000001", ErrBadFormat},
		{"seven decimals of zero", "1.0000000", ErrBadFormat},
		{"letters", "12a.34", ErrBadFormat},
		{"letters in fraction", "12.3x", ErrBadFormat},
		{"thousands separator", "1,234.56", ErrBadFormat},
		{"bare sign", "-", ErrBadFormat},
		{"two dots", "1.2.3", ErrBadFormat},
		{"currency suffix", "12.00 USD", ErrBadFormat},
		{"overflows int64", "92233720368547758070", ErrOverflow},
		{"overflows int64 negative", "-92233720368547758070", ErrOverflow},
		{"one past max", "9223372036854.775808", ErrOverflow},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Parse(USD, c.in)
			if !errors.Is(err, c.want) {
				t.Fatalf("Parse(%q) = %v, %v; want error %v", c.in, got, err, c.want)
			}
		})
	}
}

// TestAddRequiresOneCurrency proves the type refuses to add euros to dollars,
// and that the untyped zero value still acts as the identity element so a
// summation can start from Amount{}.
func TestAddRequiresOneCurrency(t *testing.T) {
	usd := FromInt(USD, 10)
	eur := FromInt(EUR, 10)

	if _, err := usd.Add(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("USD+EUR error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := Sum(usd, eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Sum(USD, EUR) error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Cmp(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("Cmp across currencies error = %v, want ErrCurrencyMismatch", err)
	}
	if usd.Less(eur) || usd.Greater(eur) || usd.Equal(eur) {
		t.Fatal("comparisons across currencies must all be false")
	}

	total, err := Sum(Amount{}, usd, FromInt(USD, 5))
	if err != nil {
		t.Fatalf("Sum from the zero value: %v", err)
	}
	if want := FromInt(USD, 15); !total.Equal(want) {
		t.Fatalf("Sum = %v, want %v", total, want)
	}
}

// TestAddDetectsOverflow proves a sum that cannot be represented is an error
// rather than a wrapped negative.
func TestAddDetectsOverflow(t *testing.T) {
	max := FromUnits(USD, math.MaxInt64)
	if _, err := max.Add(FromUnits(USD, 1)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MaxInt64+1 error = %v, want ErrOverflow", err)
	}
	min := FromUnits(USD, math.MinInt64)
	if _, err := min.Add(FromUnits(USD, -1)); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MinInt64-1 error = %v, want ErrOverflow", err)
	}
}

// TestMulRoundsHalfEvenAtTheTie proves the rounding convention at the exact
// half, in both signs. Half-even is what financial reporting uses because it
// does not bias a long series of roundings upwards.
func TestMulRoundsHalfEvenAtTheTie(t *testing.T) {
	half := Ratio{Num: 1, Den: 2}
	cases := []struct {
		units int64
		want  int64
		note  string
	}{
		{5, 2, "2.5 rounds down to the even 2"},
		{15, 8, "7.5 rounds up to the even 8"},
		{25, 12, "12.5 rounds down to the even 12"},
		{-5, -2, "-2.5 rounds towards the even -2"},
		{-15, -8, "-7.5 rounds towards the even -8"},
		{-25, -12, "-12.5 rounds towards the even -12"},
		{7, 4, "3.5 rounds up to the even 4"},
		{-7, -4, "-3.5 rounds to the even -4"},
		{3, 2, "1.5 rounds up to the even 2"},
		{-3, -2, "-1.5 rounds to the even -2"},
	}
	for _, c := range cases {
		t.Run(c.note, func(t *testing.T) {
			got, err := FromUnits(USD, c.units).Mul(half)
			if err != nil {
				t.Fatalf("Mul: %v", err)
			}
			if got.Units != c.want {
				t.Fatalf("%d * 1/2 = %d units, want %d (%s)", c.units, got.Units, c.want, c.note)
			}
		})
	}

	// Away from the tie the ordinary nearest rule still applies.
	for _, c := range []struct{ units, want int64 }{{4, 1}, {6, 2}, {-4, -1}, {-6, -2}} {
		got := mul(t, FromUnits(USD, c.units), Ratio{Num: 1, Den: 4})
		if got.Units != c.want {
			t.Fatalf("%d * 1/4 = %d, want %d", c.units, got.Units, c.want)
		}
	}
}

// TestDisplayRoundsHalfEven proves the two-decimal human rendering uses the same
// convention as the arithmetic, so a printed figure never disagrees with the
// stored one by more than the rounding it declares.
func TestDisplayRoundsHalfEven(t *testing.T) {
	cases := []struct {
		units int64
		want  string
	}{
		{1_234_560_000, "1234.56 USD"},
		{1_005_000, "1.00 USD"},   // 1.005 -> even 1.00
		{1_015_000, "1.02 USD"},   // 1.015 -> even 1.02
		{-1_005_000, "-1.00 USD"}, // symmetric in sign
		{-1_015_000, "-1.02 USD"},
		{0, "0.00 USD"},
	}
	for _, c := range cases {
		if got := FromUnits(USD, c.units).Display(); got != c.want {
			t.Fatalf("FromUnits(%d).Display() = %q, want %q", c.units, got, c.want)
		}
	}
}

// TestRatioConstructors proves the three ways a rate is written all mean what
// the desk means by them, and that Complement turns a haircut into an advance
// rate exactly rather than through a float.
func TestRatioConstructors(t *testing.T) {
	cases := []struct {
		name  string
		r     Ratio
		units int64 // result of applying r to 1_000_000 units
	}{
		{"250 bps of one unit", Bps(250), 25_000},
		{"10000 bps is the whole", Bps(10_000), 1_000_000},
		{"0 bps is nothing", Bps(0), 0},
		{"2 percent", Percent(2), 20_000},
		{"100 percent", Percent(100), 1_000_000},
		{"complement of a 250bp haircut", Bps(250).Complement(), 975_000},
		{"complement of 100 percent", Percent(100).Complement(), 0},
		{"identity", One, 1_000_000},
		{"product of two rates", Bps(5000).Mul(Percent(50)), 250_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mul(t, FromInt(USD, 1), c.r)
			if got.Units != c.units {
				t.Fatalf("%s: got %d units, want %d", c.name, got.Units, c.units)
			}
		})
	}

	// A degenerate ratio does not panic and does not silently become 1.
	if got := (Ratio{Num: 1, Den: 0}).Complement(); got != (Ratio{Num: 0, Den: 1}) {
		t.Fatalf("Complement of a zero denominator = %v, want 0/1", got)
	}
	if _, err := FromInt(USD, 1).Mul(Ratio{Num: 1, Den: 0}); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("Mul by a zero denominator must be ErrDivideByZero")
	}
	if got := (Ratio{Num: 1, Den: 0}).Float(); got != 0 {
		t.Fatalf("Float of a zero denominator = %v, want 0", got)
	}
	if got := (Ratio{Num: 1, Den: 0}).String(); got != "invalid" {
		t.Fatalf("String of a zero denominator = %q", got)
	}
}

// TestMulOnLargeAmountsDoesNotOverflow proves the intermediate product goes
// through math/big: a nine-quintillion-unit balance times a rate whose
// numerator alone would overflow int64 still lands on an exact result.
func TestMulOnLargeAmountsDoesNotOverflow(t *testing.T) {
	// 9e18 units is 9e12 currency units, and 9e18 * 2500 overflows int64 by
	// four orders of magnitude before the division brings it back.
	big9 := FromUnits(USD, 9_000_000_000_000_000_000)
	got, err := big9.Mul(Bps(2500))
	if err != nil {
		t.Fatalf("Mul on a large amount: %v", err)
	}
	if want := int64(2_250_000_000_000_000_000); got.Units != want {
		t.Fatalf("9e18 * 25%% = %d, want %d", got.Units, want)
	}

	// A result that genuinely does not fit is reported rather than wrapped.
	if _, err := FromUnits(USD, math.MaxInt64).Mul(Ratio{Num: 2, Den: 1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MaxInt64*2 error = %v, want ErrOverflow", err)
	}
	if _, err := FromUnits(USD, math.MinInt64).Mul(Ratio{Num: 3, Den: 1}); !errors.Is(err, ErrOverflow) {
		t.Fatalf("MinInt64*3 error = %v, want ErrOverflow", err)
	}
}

// TestFXConvertRoundTrip proves a conversion out and back lands within one
// minor unit, and that a currency with no rate is an error rather than an
// implicit one-to-one.
func TestFXConvertRoundTrip(t *testing.T) {
	fx := FXTable{Base: USD, Rates: map[Currency]Ratio{
		EUR: {Num: 108_432, Den: 100_000},
		GBP: {Num: 126_915, Den: 100_000},
	}}

	for _, ccy := range []Currency{EUR, GBP} {
		start := MustParse(ccy, "1234567.891234")
		inBase, err := fx.Convert(start, USD)
		if err != nil {
			t.Fatalf("%s -> USD: %v", ccy, err)
		}
		if inBase.Ccy != USD {
			t.Fatalf("converted amount carries %s, want USD", inBase.Ccy)
		}
		back, err := fx.Convert(inBase, ccy)
		if err != nil {
			t.Fatalf("USD -> %s: %v", ccy, err)
		}
		if d := back.Units - start.Units; d > 1 || d < -1 {
			t.Fatalf("%s round trip drifted by %d minor units (%v -> %v -> %v)", ccy, d, start, inBase, back)
		}
	}

	// Cross rate through the base, then back.
	eur := MustParse(EUR, "1000.00")
	gbp, err := fx.Convert(eur, GBP)
	if err != nil {
		t.Fatalf("EUR -> GBP: %v", err)
	}
	back, err := fx.Convert(gbp, EUR)
	if err != nil {
		t.Fatalf("GBP -> EUR: %v", err)
	}
	if d := back.Units - eur.Units; d > 1 || d < -1 {
		t.Fatalf("cross-rate round trip drifted by %d minor units", d)
	}

	// Same currency is a no-op, and the untyped zero adopts the target.
	if got, err := fx.Convert(eur, EUR); err != nil || got != eur {
		t.Fatalf("Convert to the same currency = %v, %v", got, err)
	}
	if got, err := fx.Convert(Amount{}, GBP); err != nil || got != Zero(GBP) {
		t.Fatalf("Convert of the zero value = %v, %v; want zero GBP", got, err)
	}
}

// TestFXMissingRateIsAnError proves an unknown currency stops the calculation
// instead of being treated as parity.
func TestFXMissingRateIsAnError(t *testing.T) {
	fx := FXTable{Base: USD, Rates: map[Currency]Ratio{EUR: {Num: 11, Den: 10}}}

	if _, err := fx.Convert(FromInt(GBP, 100), USD); err == nil {
		t.Fatal("converting from a currency with no rate must fail")
	} else if !strings.Contains(err.Error(), "GBP") {
		t.Fatalf("error should name the missing currency, got %v", err)
	}
	if _, err := fx.Convert(FromInt(EUR, 100), GBP); err == nil {
		t.Fatal("converting into a currency with no rate must fail")
	}
}

// TestBagCollapse proves a multi-currency accumulator sums freely but can only
// be reduced to a single figure through an explicit rate table, and that the
// reduction is exact and order-independent.
func TestBagCollapse(t *testing.T) {
	fx := FXTable{Base: USD, Rates: map[Currency]Ratio{
		EUR: {Num: 11, Den: 10},
		GBP: {Num: 13, Den: 10},
	}}

	b := NewBag()
	b.Add(MustParse(USD, "100.00"))
	b.Add(MustParse(EUR, "100.00"))
	b.Add(MustParse(GBP, "100.00"))
	b.Add(MustParse(EUR, "50.00"))
	b.Add(Amount{}) // the untyped zero contributes nothing and creates no bucket

	if got, want := b.Currencies(), []Currency{EUR, GBP, USD}; len(got) != len(want) {
		t.Fatalf("Currencies() = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("Currencies() = %v, want %v (sorted)", got, want)
			}
		}
	}
	if got, want := b.In(EUR), MustParse(EUR, "150.00"); !got.Equal(want) {
		t.Fatalf("In(EUR) = %v, want %v", got, want)
	}

	total, err := b.Collapse(USD, fx)
	if err != nil {
		t.Fatalf("Collapse: %v", err)
	}
	// 100 USD + 150 EUR * 1.1 + 100 GBP * 1.3 = 100 + 165 + 130 = 395
	if want := MustParse(USD, "395.00"); !total.Equal(want) {
		t.Fatalf("Collapse = %v, want %v", total, want)
	}

	// A bag holding a currency the table does not know cannot be collapsed.
	b.Add(FromInt("CHF", 1))
	if _, err := b.Collapse(USD, fx); err == nil {
		t.Fatal("Collapse with a missing rate must fail")
	}

	// The zero value of Bag is usable without NewBag.
	var zb Bag
	zb.Add(MustParse(USD, "7.50"))
	if got, want := zb.In(USD), MustParse(USD, "7.50"); !got.Equal(want) {
		t.Fatalf("zero-value Bag.In(USD) = %v, want %v", got, want)
	}
}

// TestMulByRateAndBackStaysWithinOneMinorUnit is the property this package
// exists for: applying a rate and then its reciprocal is exact enough that the
// result differs from the input by at most the last representable unit. It is
// checked over a fixed pseudo-random sample so a failure is reproducible.
func TestMulByRateAndBackStaysWithinOneMinorUnit(t *testing.T) {
	rng := rand.New(rand.NewSource(20260903))
	for i := 0; i < 20_000; i++ {
		// Amounts up to a trillion currency units, in both signs.
		units := rng.Int63n(1_000_000_000_000_000_000)
		if rng.Intn(2) == 0 {
			units = -units
		}
		a := FromUnits(USD, units)

		// Rates at or above one, so the reciprocal cannot amplify the first
		// rounding beyond the second one's own half unit.
		den := int64(1 + rng.Intn(1_000_000))
		num := den + rng.Int63n(3*den)
		r := Ratio{Num: num, Den: den}
		inv := Ratio{Num: r.Den, Den: r.Num}

		scaled, err := a.Mul(r)
		if err != nil {
			continue // an overflow is reported, not silently wrong; nothing to compare
		}
		back, err := scaled.Mul(inv)
		if err != nil {
			continue
		}
		if d := back.Units - a.Units; d > 1 || d < -1 {
			t.Fatalf("iteration %d: %d * %d/%d and back = %d, drift %d units",
				i, a.Units, r.Num, r.Den, back.Units, d)
		}
	}
}

// TestJSONRoundTrip proves an Amount crosses a JSON boundary as an object with
// its scale attached, so no consumer can read it as a float, and that a
// mismatched scale is refused.
func TestJSONRoundTrip(t *testing.T) {
	a := MustParse(EUR, "-98765.432100")
	b, err := a.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !strings.Contains(string(b), `"scale":6`) {
		t.Fatalf("encoded form must carry the scale, got %s", b)
	}
	var got Amount
	if err := got.UnmarshalJSON(b); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if got != a {
		t.Fatalf("JSON round trip: %v -> %s -> %v", a, b, got)
	}
	if err := got.UnmarshalJSON([]byte(`{"ccy":"USD","units":1,"scale":2}`)); err == nil {
		t.Fatal("a foreign scale must be rejected rather than reinterpreted")
	}
}

// TestSignHelpers covers the small predicates the risk engine leans on.
func TestSignHelpers(t *testing.T) {
	neg := FromUnits(USD, -5)
	if !neg.IsNegative() || neg.Abs().Units != 5 || neg.Neg().Units != 5 {
		t.Fatalf("sign helpers wrong for %v", neg)
	}
	if !Zero(USD).IsZero() || !(Amount{}).IsZero() {
		t.Fatal("zero amounts must report IsZero")
	}
	if (Amount{}).String() != "0.000000 ???" {
		t.Fatalf("an amount with no currency renders as %q", (Amount{}).String())
	}
	if got := New(USD, 12, 500_000); got.Units != 12_500_000 {
		t.Fatalf("New(USD, 12, 500000) = %d units", got.Units)
	}
}

// TestMustHelpersPanicOnMisuse proves the Must variants are a contract, not a
// convenience that swallows a currency mismatch.
func TestMustHelpersPanicOnMisuse(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   func()
	}{
		{"MustAdd", func() { FromInt(USD, 1).MustAdd(FromInt(EUR, 1)) }},
		{"MustSub", func() { FromInt(USD, 1).MustSub(FromInt(EUR, 1)) }},
		{"MustMul", func() { FromInt(USD, 1).MustMul(Ratio{Num: 1, Den: 0}) }},
		{"MustParse", func() { MustParse(USD, "not a number") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s must panic on misuse", c.name)
				}
			}()
			c.fn()
		})
	}
}

// TestDivRoundHalfEvenAgainstBigRat cross-checks the rounding helper against an
// independent exact computation, so the convention is proved rather than
// restated.
func TestDivRoundHalfEvenAgainstBigRat(t *testing.T) {
	for n := int64(-40); n <= 40; n++ {
		for _, d := range []int64{1, 2, 3, 4, 7, -2, -3} {
			got, err := divRoundHalfEven(big.NewInt(n), big.NewInt(d))
			if err != nil {
				t.Fatalf("divRoundHalfEven(%d, %d): %v", n, d, err)
			}
			if want := halfEvenReference(n, d); got != want {
				t.Fatalf("divRoundHalfEven(%d, %d) = %d, want %d", n, d, got, want)
			}
		}
	}
	if _, err := divRoundHalfEven(big.NewInt(1), big.NewInt(0)); !errors.Is(err, ErrDivideByZero) {
		t.Fatal("division by zero must be reported")
	}
}

// halfEvenReference computes round-half-to-even independently of the code under
// test, using exact rational arithmetic.
func halfEvenReference(n, d int64) int64 {
	q := new(big.Rat).SetFrac64(n, d)
	floor := new(big.Int).Quo(q.Num(), q.Denom())
	if q.Sign() < 0 && new(big.Int).Mul(floor, q.Denom()).Cmp(q.Num()) != 0 {
		floor.Sub(floor, big.NewInt(1))
	}
	frac := new(big.Rat).Sub(q, new(big.Rat).SetInt(floor))
	half := big.NewRat(1, 2)
	switch frac.Cmp(half) {
	case 1:
		floor.Add(floor, big.NewInt(1))
	case 0:
		if floor.Bit(0) == 1 {
			floor.Add(floor, big.NewInt(1))
		}
	}
	return floor.Int64()
}

// mul applies a ratio and fails the test on an error, so an assertion below it
// reads as the arithmetic it is checking.
func mul(t *testing.T, a Amount, r Ratio) Amount {
	t.Helper()
	out, err := a.Mul(r)
	if err != nil {
		t.Fatalf("%v * %v: %v", a, r, err)
	}
	return out
}

// FuzzParse asserts Parse is total: any input either produces an error or an
// Amount whose own rendering parses back to the identical value. It has never
// been allowed to panic, truncate a digit, or wrap an int64.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"", "0", "1", "-1", "+1", "1.5", "0.000001", "1.0000001", "9223372036854.775807",
		"92233720368547758070", "  12.25  ", "1,234.56", "..", "-", "1.2.3", "1e9", "1_000",
		"00000000000000000000001", "-0.0000005", "0.", ".5",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := Parse(USD, s)
		if err != nil {
			return
		}
		if a.Ccy != USD {
			t.Fatalf("Parse(%q) produced currency %q", s, a.Ccy)
		}
		back, err := Parse(USD, decimalOf(a.String()))
		if err != nil {
			t.Fatalf("Parse(%q) -> %q which does not re-parse: %v", s, a.String(), err)
		}
		if back != a {
			t.Fatalf("Parse(%q) = %v but its rendering %q parses to %v", s, a, a.String(), back)
		}
	})
}
