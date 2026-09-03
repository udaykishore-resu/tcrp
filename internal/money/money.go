// Package money provides exact fixed-point monetary arithmetic.
//
// Money is never a float64. Every amount is an integer count of 1e-6 units of
// a named currency, and every multiplication by a rate (a haircut, an FX rate,
// a stress shock) goes through an exact rational and rounds once, half-even, at
// the end. Two amounts in different currencies do not add; the type system and
// the errors make that explicit rather than silently producing a number.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Scale is the number of decimal places carried by every Amount.
const Scale = 6

// scaleFactor is 10^Scale.
const scaleFactor int64 = 1_000_000

var (
	// ErrCurrencyMismatch is returned when an operation combines two amounts
	// denominated in different currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result cannot be represented in int64
	// minor units.
	ErrOverflow = errors.New("money: overflow")
	// ErrBadFormat is returned by Parse for malformed input.
	ErrBadFormat = errors.New("money: bad format")
	// ErrDivideByZero is returned when a Ratio has a zero denominator.
	ErrDivideByZero = errors.New("money: divide by zero")
)

// Currency is an ISO-4217-style code. The zero value is invalid.
type Currency string

// Common currencies used by the platform.
const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
)

// Amount is an exact monetary quantity: units of 1e-6 of Ccy.
type Amount struct {
	Ccy   Currency
	Units int64
}

// New builds an Amount from whole units and a fractional part expressed in
// 1e-6 units. New(USD, 12, 500000) is $12.50.
func New(c Currency, whole, micros int64) Amount {
	return Amount{Ccy: c, Units: whole*scaleFactor + micros}
}

// FromUnits builds an Amount directly from minor units.
func FromUnits(c Currency, units int64) Amount { return Amount{Ccy: c, Units: units} }

// FromInt builds an Amount from a whole number of currency units.
func FromInt(c Currency, whole int64) Amount { return Amount{Ccy: c, Units: whole * scaleFactor} }

// Zero returns the zero amount in c.
func Zero(c Currency) Amount { return Amount{Ccy: c} }

// IsZero reports whether a has no value. A zero-value Amount (no currency) is
// also zero, which lets it act as the identity element in a summation.
func (a Amount) IsZero() bool { return a.Units == 0 }

// IsNegative reports whether a is below zero.
func (a Amount) IsNegative() bool { return a.Units < 0 }

// Neg returns -a.
func (a Amount) Neg() Amount { return Amount{Ccy: a.Ccy, Units: -a.Units} }

// Abs returns |a|.
func (a Amount) Abs() Amount {
	if a.Units < 0 {
		return a.Neg()
	}
	return a
}

// Add returns a+b. The currencies must match, unless one side is the untyped
// zero value, which allows Add to be folded over a slice starting from Amount{}.
func (a Amount) Add(b Amount) (Amount, error) {
	c, err := unify(a, b)
	if err != nil {
		return Amount{}, err
	}
	sum := a.Units + b.Units
	if (a.Units > 0 && b.Units > 0 && sum < 0) || (a.Units < 0 && b.Units < 0 && sum > 0) {
		return Amount{}, ErrOverflow
	}
	return Amount{Ccy: c, Units: sum}, nil
}

// MustAdd is Add for call sites that have already established currency
// agreement. It panics on error, which is correct only inside a single-currency
// aggregation that validated its inputs.
func (a Amount) MustAdd(b Amount) Amount {
	out, err := a.Add(b)
	if err != nil {
		panic(err)
	}
	return out
}

// Sub returns a-b.
func (a Amount) Sub(b Amount) (Amount, error) { return a.Add(b.Neg()) }

// MustSub is Sub for call sites that have already established currency agreement.
func (a Amount) MustSub(b Amount) Amount {
	out, err := a.Sub(b)
	if err != nil {
		panic(err)
	}
	return out
}

// Cmp compares a and b, returning -1, 0 or 1.
func (a Amount) Cmp(b Amount) (int, error) {
	if _, err := unify(a, b); err != nil {
		return 0, err
	}
	switch {
	case a.Units < b.Units:
		return -1, nil
	case a.Units > b.Units:
		return 1, nil
	default:
		return 0, nil
	}
}

// Less reports whether a < b, treating a currency mismatch as false.
func (a Amount) Less(b Amount) bool {
	c, err := a.Cmp(b)
	return err == nil && c < 0
}

// Greater reports whether a > b, treating a currency mismatch as false.
func (a Amount) Greater(b Amount) bool {
	c, err := a.Cmp(b)
	return err == nil && c > 0
}

// Equal reports whether a and b are the same currency and magnitude.
func (a Amount) Equal(b Amount) bool {
	c, err := a.Cmp(b)
	return err == nil && c == 0
}

// Mul multiplies a by an exact ratio and rounds half-even to Scale places.
func (a Amount) Mul(r Ratio) (Amount, error) {
	if r.Den == 0 {
		return Amount{}, ErrDivideByZero
	}
	n := new(big.Int).Mul(big.NewInt(a.Units), big.NewInt(r.Num))
	q, err := divRoundHalfEven(n, big.NewInt(r.Den))
	if err != nil {
		return Amount{}, err
	}
	return Amount{Ccy: a.Ccy, Units: q}, nil
}

// MustMul is Mul for ratios known to be well formed.
func (a Amount) MustMul(r Ratio) Amount {
	out, err := a.Mul(r)
	if err != nil {
		panic(err)
	}
	return out
}

// Ratio is an exact rational used for haircuts, FX rates, shocks and shares.
// It is never rounded until it is applied to an Amount.
type Ratio struct {
	Num int64
	Den int64
}

// One is the identity ratio.
var One = Ratio{Num: 1, Den: 1}

// Pct builds a ratio from a percentage expressed in basis points.
// Bps(250) is 2.50%.
func Bps(bps int64) Ratio { return Ratio{Num: bps, Den: 10_000} }

// Percent builds a ratio from whole percent.
func Percent(p int64) Ratio { return Ratio{Num: p, Den: 100} }

// Complement returns 1-r, used to turn a haircut into an advance rate.
func (r Ratio) Complement() Ratio {
	if r.Den == 0 {
		return Ratio{Num: 0, Den: 1}
	}
	return Ratio{Num: r.Den - r.Num, Den: r.Den}
}

// Mul multiplies two ratios exactly.
func (r Ratio) Mul(o Ratio) Ratio { return Ratio{Num: r.Num * o.Num, Den: r.Den * o.Den} }

// Float returns an approximate float64 for display and metrics only. It is
// never used in a calculation whose result is money.
func (r Ratio) Float() float64 {
	if r.Den == 0 {
		return 0
	}
	return float64(r.Num) / float64(r.Den)
}

func (r Ratio) String() string {
	if r.Den == 0 {
		return "invalid"
	}
	return fmt.Sprintf("%.4f", r.Float())
}

// Bag is a multi-currency accumulator. Summing mixed currencies is legal; only
// collapsing the bag to a single figure requires an FX table, which forces the
// conversion to be explicit and auditable.
type Bag struct {
	byCcy map[Currency]int64
}

// NewBag returns an empty Bag.
func NewBag() *Bag { return &Bag{byCcy: make(map[Currency]int64, 4)} }

// Add accumulates a into the bag.
func (b *Bag) Add(a Amount) {
	if a.Ccy == "" && a.Units == 0 {
		return
	}
	if b.byCcy == nil {
		b.byCcy = make(map[Currency]int64, 4)
	}
	b.byCcy[a.Ccy] += a.Units
}

// Currencies returns the currencies held, in a stable order.
func (b *Bag) Currencies() []Currency {
	out := make([]Currency, 0, len(b.byCcy))
	for c := range b.byCcy {
		out = append(out, c)
	}
	// Insertion order is not stable across map iterations, so sort by code.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// In returns the amount held in currency c.
func (b *Bag) In(c Currency) Amount { return Amount{Ccy: c, Units: b.byCcy[c]} }

// Collapse converts every holding to base using fx and returns the total.
// A missing rate is an error rather than an implicit 1:1.
func (b *Bag) Collapse(base Currency, fx FXTable) (Amount, error) {
	total := Zero(base)
	for _, c := range b.Currencies() {
		conv, err := fx.Convert(b.In(c), base)
		if err != nil {
			return Amount{}, err
		}
		total, err = total.Add(conv)
		if err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}

// FXTable converts between currencies using exact ratios.
type FXTable struct {
	Base  Currency
	Rates map[Currency]Ratio // units of Base per one unit of the keyed currency
}

// Convert converts a into want.
func (f FXTable) Convert(a Amount, want Currency) (Amount, error) {
	if a.Ccy == want {
		return a, nil
	}
	if a.Ccy == "" && a.Units == 0 {
		return Zero(want), nil
	}
	// The base currency's rate against itself is one and is not carried in the
	// table; without this the table converts inwards but not outwards.
	toBase, ok := f.Rates[a.Ccy]
	if !ok {
		if a.Ccy != f.Base {
			return Amount{}, fmt.Errorf("money: no fx rate for %s", a.Ccy)
		}
		toBase = One
	}
	inBase, err := a.Mul(toBase)
	if err != nil {
		return Amount{}, err
	}
	inBase.Ccy = f.Base
	if want == f.Base {
		return inBase, nil
	}
	fromBase, ok := f.Rates[want]
	if !ok {
		return Amount{}, fmt.Errorf("money: no fx rate for %s", want)
	}
	out, err := inBase.Mul(Ratio{Num: fromBase.Den, Den: fromBase.Num})
	if err != nil {
		return Amount{}, err
	}
	out.Ccy = want
	return out, nil
}

// String renders the amount with Scale decimal places and its currency code.
func (a Amount) String() string {
	neg := a.Units < 0
	u := a.Units
	if neg {
		u = -u
	}
	whole := u / scaleFactor
	frac := u % scaleFactor
	sign := ""
	if neg {
		sign = "-"
	}
	ccy := a.Ccy
	if ccy == "" {
		ccy = "???"
	}
	return fmt.Sprintf("%s%d.%06d %s", sign, whole, frac, ccy)
}

// Display renders the amount with two decimal places for human-facing output.
// It rounds half-even and is never fed back into a calculation.
func (a Amount) Display() string {
	q, err := divRoundHalfEven(big.NewInt(a.Units), big.NewInt(10_000))
	if err != nil {
		return a.String()
	}
	neg := q < 0
	if neg {
		q = -q
	}
	sign := ""
	if neg {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%02d %s", sign, q/100, q%100, a.Ccy)
}

// MarshalJSON encodes the amount as an object so no precision is lost in
// transport and no consumer can accidentally read it as a float.
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf(`{"ccy":%q,"units":%d,"scale":%d}`, a.Ccy, a.Units, Scale)), nil
}

// UnmarshalJSON decodes the object form produced by MarshalJSON.
func (a *Amount) UnmarshalJSON(b []byte) error {
	var raw struct {
		Ccy   string `json:"ccy"`
		Units int64  `json:"units"`
		Scale int    `json:"scale"`
	}
	if err := jsonUnmarshal(b, &raw); err != nil {
		return err
	}
	if raw.Scale != 0 && raw.Scale != Scale {
		return fmt.Errorf("money: unsupported scale %d", raw.Scale)
	}
	a.Ccy = Currency(raw.Ccy)
	a.Units = raw.Units
	return nil
}

// Parse reads a decimal string such as "1234.56" into an Amount. More than
// Scale decimal places is an error rather than a silent truncation.
func Parse(c Currency, s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Amount{}, ErrBadFormat
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" && !hasFrac {
		return Amount{}, ErrBadFormat
	}
	if len(frac) > Scale {
		return Amount{}, fmt.Errorf("%w: more than %d decimal places", ErrBadFormat, Scale)
	}
	var units int64
	for _, r := range whole {
		if r < '0' || r > '9' {
			return Amount{}, ErrBadFormat
		}
		d := int64(r - '0')
		// Each accumulation is bounds-checked. A silent int64 wrap here would
		// turn an absurd input into a plausible small figure, which is exactly
		// the failure this package exists to make impossible.
		if units > (math.MaxInt64-d)/10 {
			return Amount{}, fmt.Errorf("%w: %q does not fit in int64 minor units", ErrOverflow, s)
		}
		units = units*10 + d
	}
	if units > math.MaxInt64/scaleFactor {
		return Amount{}, fmt.Errorf("%w: %q does not fit in int64 minor units", ErrOverflow, s)
	}
	units *= scaleFactor
	if hasFrac {
		pad := frac + strings.Repeat("0", Scale-len(frac))
		var f int64
		for _, r := range pad {
			if r < '0' || r > '9' {
				return Amount{}, ErrBadFormat
			}
			f = f*10 + int64(r-'0')
		}
		if units > math.MaxInt64-f {
			return Amount{}, fmt.Errorf("%w: %q does not fit in int64 minor units", ErrOverflow, s)
		}
		units += f
	}
	if neg {
		units = -units
	}
	return Amount{Ccy: c, Units: units}, nil
}

// MustParse is Parse for constants in tests and seed data.
func MustParse(c Currency, s string) Amount {
	a, err := Parse(c, s)
	if err != nil {
		panic(err)
	}
	return a
}

// Sum folds amounts, requiring a single currency.
func Sum(amounts ...Amount) (Amount, error) {
	var total Amount
	var err error
	for _, a := range amounts {
		if total, err = total.Add(a); err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}

func unify(a, b Amount) (Currency, error) {
	switch {
	case a.Ccy == b.Ccy:
		return a.Ccy, nil
	case a.Ccy == "" && a.Units == 0:
		return b.Ccy, nil
	case b.Ccy == "" && b.Units == 0:
		return a.Ccy, nil
	default:
		return "", fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, a.Ccy, b.Ccy)
	}
}

// divRoundHalfEven divides n by d and rounds half to even, the convention used
// by financial reporting because it does not bias a long series of roundings.
func divRoundHalfEven(n, d *big.Int) (int64, error) {
	if d.Sign() == 0 {
		return 0, ErrDivideByZero
	}
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Sign() == 0 {
		if !q.IsInt64() {
			return 0, ErrOverflow
		}
		return q.Int64(), nil
	}
	// Compare |2r| with |d| to decide the tie.
	twoR := new(big.Int).Abs(new(big.Int).Lsh(r, 1))
	absD := new(big.Int).Abs(d)
	cmp := twoR.Cmp(absD)
	up := false
	switch {
	case cmp > 0:
		up = true
	case cmp == 0:
		// Exactly half: round to even.
		up = q.Bit(0) == 1
	}
	if up {
		if (n.Sign() < 0) != (d.Sign() < 0) {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, ErrOverflow
	}
	return q.Int64(), nil
}
