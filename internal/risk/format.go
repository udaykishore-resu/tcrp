package risk

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/udaykishore-resu/tcrp/internal/domain"
	"github.com/udaykishore-resu/tcrp/internal/money"
)

// The renderers below produce the exact text a risk manager reads in the CLI
// and the agent hands back as a tool result. They are terse on purpose: a
// margin call is read at speed, and every line that is not a number or a name
// is a line between the reader and the decision.
//
// Money always goes through Amount.Display, which rounds half-even to two
// places for presentation only. Nothing formatted here is ever parsed back.

// FormatCollateral renders a counterparty's pledge, line by line, with the
// haircut applied to each and the totals underneath.
func FormatCollateral(cv domain.CollateralValue) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Collateral  %s  as of %s  schedule %s\n",
		cv.Counterparty, stamp(cv.AsOf), orNone(cv.HaircutVersion))
	if !cv.PriceAsOf.IsZero() {
		fmt.Fprintf(&sb, "  marks as of %s\n", stamp(cv.PriceAsOf))
	}

	if len(cv.Lines) == 0 {
		sb.WriteString("  no pledged positions\n")
	} else {
		t := &table{
			header: []string{"SYMBOL", "CLASS", "LEDGER", "QUANTITY", "PRICE", "MARKET", "HAIRCUT", "ADVANCE", "COLLATERAL"},
			right:  []bool{false, false, false, true, true, true, true, true, true},
		}
		for _, l := range cv.Lines {
			collateral := l.Collateral.Display()
			if !l.Eligible {
				collateral = "ineligible"
			}
			t.rows = append(t.rows, []string{
				l.Symbol, string(l.Class), string(l.Ledger),
				group(l.Quantity), l.Price.Display(), l.Market.Display(),
				pct(l.Haircut), pct(l.AdvanceRate), collateral,
			})
		}
		t.render(&sb, "  ")
	}

	f := &fields{}
	f.addAmount("Market value", cv.MarketValue, "")
	f.addAmount("Collateral value", cv.CollateralValue, "")
	if !cv.IneligibleValue.IsZero() {
		f.addAmount("Ineligible", cv.IneligibleValue, "")
	}
	f.render(&sb, "  ")
	return sb.String()
}

// FormatExposure renders one counterparty's exposure, ending with the callable
// number — the margin call, or its absence — because that is the line the
// reader came for.
func FormatExposure(x domain.Exposure) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Exposure  %s  as of %s\n", x.Counterparty, stamp(x.AsOf))

	f := &fields{}
	f.addAmount("Gross exposure", x.GrossExposure, "")
	f.addAmount("Net exposure", x.NetExposure, "")
	f.addAmount("Collateral", x.Collateral, "")
	if x.Covered {
		f.addAmount("Excess", x.Excess, "")
	} else {
		f.addAmount("Uncovered", x.Uncovered, "")
	}
	f.addAmount("Threshold", x.Threshold, "")
	f.addAmount("Minimum transfer", x.MinimumTransfer, "")
	if x.Shortfall.IsZero() {
		f.addText("Margin call", "none")
	} else {
		f.addAmount("MARGIN CALL", x.Shortfall, "")
	}
	f.render(&sb, "  ")

	if len(x.NettingSets) > 0 {
		sb.WriteString("  netting sets\n")
		t := &table{
			header: []string{"SET", "OBLIGATIONS", "GROSS", "NET", "STATUS"},
			right:  []bool{false, true, true, true, false},
		}
		for _, ns := range x.NettingSets {
			status := "not enforceable"
			if ns.Enforced {
				status = "enforceable"
			}
			t.rows = append(t.rows, []string{
				ns.ID, fmt.Sprint(ns.Count), ns.Gross.Display(), ns.Net.Display(), status,
			})
		}
		t.render(&sb, "    ")
	}
	return sb.String()
}

// FormatStress renders a scenario summary: the aggregate move, the worst
// counterparty, and the per-counterparty detail worst first.
func FormatStress(s domain.StressSummary) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Stress  %s  as of %s  %d counterparties  %.1fms\n",
		s.Scenario, stamp(s.AsOf), s.Counterparties, s.DurationMS)

	change := s.TotalStressed
	if d, err := s.TotalStressed.Sub(s.TotalBase); err == nil {
		change = d
	}
	f := &fields{}
	f.addAmount("Base collateral", s.TotalBase, "")
	f.addAmount("Stressed collateral", s.TotalStressed, "")
	f.addAmount("Change", change, "  ("+relative(change, s.TotalBase)+")")
	f.addAmount("New shortfall", s.TotalShortfall, "")
	f.addText("Worst counterparty", orNone(s.WorstCP))
	f.render(&sb, "  ")

	rows := append([]domain.StressResult(nil), s.Results...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].NewShortfall.Units != rows[j].NewShortfall.Units {
			return rows[i].NewShortfall.Units > rows[j].NewShortfall.Units
		}
		if rows[i].Delta.Units != rows[j].Delta.Units {
			return rows[i].Delta.Units < rows[j].Delta.Units
		}
		return rows[i].Counterparty < rows[j].Counterparty
	})

	t := &table{
		header: []string{"COUNTERPARTY", "BASE COLLATERAL", "STRESSED", "DELTA", "BASE UNCOV", "STRESSED UNCOV", "NEW SHORTFALL", "LIMIT"},
		right:  []bool{false, true, true, true, true, true, true, false},
	}
	for _, r := range rows {
		limit := ""
		if r.BreachesLimit {
			limit = "BREACHED"
		}
		t.rows = append(t.rows, []string{
			r.Counterparty, r.BaseCollateral.Display(), r.StressedValue.Display(), r.Delta.Display(),
			r.BaseUncovered.Display(), r.StressedUncov.Display(), r.NewShortfall.Display(), limit,
		})
	}
	t.render(&sb, "  ")
	return sb.String()
}

// FormatBreaches renders a breach list worst first. An empty list renders as a
// single line saying so, rather than as nothing, because a blank pane and a
// clean book are not the same message.
func FormatBreaches(bs []domain.Breach) string {
	var sb strings.Builder
	if len(bs) == 0 {
		return "Breaches  none: every limit observed\n"
	}
	sorted := append([]domain.Breach(nil), bs...)
	sortBreaches(sorted)

	counts := map[domain.BreachSeverity]int{}
	for _, b := range sorted {
		counts[b.Severity]++
	}
	fmt.Fprintf(&sb, "Breaches  %d  (critical %d, breach %d, warning %d)\n",
		len(sorted), counts[domain.BreachCritical], counts[domain.BreachHard], counts[domain.BreachWarn])

	t := &table{
		header: []string{"SEVERITY", "STATUS", "ID", "LIMIT", "SCOPE", "OBSERVED", "AGAINST", "UTILISATION"},
		right:  []bool{false, false, false, false, false, true, true, true},
	}
	for _, b := range sorted {
		observed, against := b.Observed.Display(), b.Limit.Display()
		if b.ObservedPc.Den != 0 {
			observed, against = pct(b.ObservedPc), pct(b.LimitPc)
		}
		status := string(b.Status)
		if status == "" {
			status = string(domain.BreachOpen)
		}
		t.rows = append(t.rows, []string{
			string(b.Severity), status, b.ID, b.LimitID, b.Scope, observed, against, utilisation(b),
		})
	}
	t.render(&sb, "  ")
	return sb.String()
}

// ---------------------------------------------------------------------------
// Rendering primitives
// ---------------------------------------------------------------------------

// table lays a grid out with per-column alignment. text/tabwriter aligns every
// column the same way; a collateral table needs its names left and its numbers
// right, so the widths are computed here instead.
type table struct {
	header []string
	rows   [][]string
	right  []bool
}

func (t *table) render(sb *strings.Builder, indent string) {
	if len(t.rows) == 0 {
		return
	}
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = len(h)
	}
	for _, r := range t.rows {
		for i, c := range r {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	write := func(cells []string) {
		var line strings.Builder
		line.WriteString(indent)
		for i, c := range cells {
			if i > 0 {
				line.WriteString("  ")
			}
			if i < len(t.right) && t.right[i] {
				line.WriteString(padLeft(c, widths[i]))
			} else {
				line.WriteString(padRight(c, widths[i]))
			}
		}
		// Trailing padding on the last column is invisible on screen and
		// noisy in a diff, so it is trimmed rather than emitted.
		sb.WriteString(strings.TrimRight(line.String(), " "))
		sb.WriteString("\n")
	}
	write(t.header)
	for _, r := range t.rows {
		write(r)
	}
}

// fields is a label/value block. Amounts are right-aligned on their own column
// so the decimal points line up; anything else is left-aligned, because a name
// pushed to the right edge under a column of money reads as a number.
type fields struct {
	labels  []string
	values  []string
	suffix  []string
	numeric []bool
}

func (f *fields) addAmount(label string, a money.Amount, suffix string) {
	f.push(label, a.Display(), suffix, true)
}

func (f *fields) addText(label, value string) { f.push(label, value, "", false) }

func (f *fields) push(label, value, suffix string, numeric bool) {
	f.labels = append(f.labels, label)
	f.values = append(f.values, value)
	f.suffix = append(f.suffix, suffix)
	f.numeric = append(f.numeric, numeric)
}

func (f *fields) render(sb *strings.Builder, indent string) {
	lw, vw := 0, 0
	for i := range f.labels {
		if len(f.labels[i]) > lw {
			lw = len(f.labels[i])
		}
		if f.numeric[i] && len(f.values[i]) > vw {
			vw = len(f.values[i])
		}
	}
	for i := range f.labels {
		value := f.values[i]
		if f.numeric[i] {
			value = padLeft(value, vw)
		}
		line := indent + padRight(f.labels[i], lw) + "  " + value + f.suffix[i]
		sb.WriteString(strings.TrimRight(line, " "))
		sb.WriteString("\n")
	}
}

func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// pct renders a ratio as a percentage. It is display only: the ratio itself is
// exact and is never replaced by this number.
func pct(r money.Ratio) string {
	if r.Den == 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f%%", r.Float()*100)
}

// relative renders change as a percentage of base, for the one-glance read of
// how far a stress moved the book.
func relative(change, base money.Amount) string {
	if base.Units == 0 {
		return "-"
	}
	return fmt.Sprintf("%+.2f%%", float64(change.Units)/float64(base.Units)*100)
}

// utilisation renders observed ÷ limit for a breach, whichever of the amount or
// the ratio form the limit uses.
func utilisation(b domain.Breach) string {
	switch {
	case b.ObservedPc.Den != 0 && b.LimitPc.Den != 0 && b.LimitPc.Num != 0:
		return fmt.Sprintf("%.1f%%", b.ObservedPc.Float()/b.LimitPc.Float()*100)
	case b.Limit.Units != 0:
		return fmt.Sprintf("%.1f%%", float64(b.Observed.Units)/float64(b.Limit.Units)*100)
	default:
		return "-"
	}
}

// group renders a quantity with thousands separators, which is the difference
// between reading 40000000 and reading 40,000,000 at a glance.
func group(v int64) string {
	s := fmt.Sprint(v)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
