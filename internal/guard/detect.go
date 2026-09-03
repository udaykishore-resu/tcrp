// Package guard is the inline guardrail service.
//
// It inspects three things on every turn: the prompt the user sent, the context
// retrieved on the agent's behalf, and the output the model produced. It runs
// on the request path with a declared latency budget, and when the budget is
// exhausted its behaviour is a policy choice per risk tier rather than a
// hard-coded default: a read-only turn fails open and is flagged, a
// money-movement turn fails closed and is blocked.
//
// The detectors here are deterministic and cheap. A classifier callout is a
// pluggable extra stage, not the foundation, because a guardrail that cannot
// run without a network round trip is a guardrail that is off during an
// incident.
package guard

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Category classifies a finding.
type Category string

// Finding categories.
const (
	CatPII        Category = "pii"
	CatSecret     Category = "secret"
	CatInjection  Category = "prompt_injection"
	CatJailbreak  Category = "jailbreak"
	CatExfil      Category = "exfiltration"
	CatUngrounded Category = "ungrounded_number"
	CatOffDomain  Category = "off_domain"
	CatToxicity   Category = "toxicity"
)

// Severity orders findings.
type Severity int

// Severities.
const (
	SevInfo Severity = iota
	SevLow
	SevMedium
	SevHigh
	SevCritical
)

// String renders the severity.
func (s Severity) String() string {
	switch s {
	case SevCritical:
		return "critical"
	case SevHigh:
		return "high"
	case SevMedium:
		return "medium"
	case SevLow:
		return "low"
	default:
		return "info"
	}
}

// Finding is one detection.
type Finding struct {
	Category Category `json:"category"`
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Detail   string   `json:"detail,omitempty"`
	Start    int      `json:"start"`
	End      int      `json:"end"`
	Excerpt  string   `json:"excerpt,omitempty"`
}

// Detector inspects text and returns findings. Detectors must be pure and
// allocation-light: they run on every turn.
type Detector interface {
	Name() string
	Detect(text string) []Finding
}

// ---------- PII ----------

type pattern struct {
	rule     string
	cat      Category
	sev      Severity
	re       *regexp.Regexp
	validate func(string) bool
}

var piiPatterns = []pattern{
	{rule: "us_ssn", cat: CatPII, sev: SevHigh, re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)},
	{rule: "email", cat: CatPII, sev: SevMedium, re: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
	{rule: "phone_e164", cat: CatPII, sev: SevLow, re: regexp.MustCompile(`\+\d{1,3}[\s\-]?\(?\d{2,4}\)?[\s\-]?\d{3,4}[\s\-]?\d{3,4}\b`)},
	{rule: "iban", cat: CatPII, sev: SevHigh, re: regexp.MustCompile(`\b[A-Z]{2}\d{2}[A-Z0-9]{11,30}\b`), validate: validIBANShape},
	{rule: "payment_card", cat: CatPII, sev: SevCritical, re: regexp.MustCompile(`\b(?:\d[ \-]?){13,19}\b`), validate: luhn},
	{rule: "internal_account", cat: CatPII, sev: SevMedium, re: regexp.MustCompile(`\bACCT-[A-Z0-9]{6,12}\b`)},
	{rule: "evm_address", cat: CatPII, sev: SevLow, re: regexp.MustCompile(`\b0x[a-fA-F0-9]{40}\b`)},
}

// PIIDetector finds personal and account identifiers.
type PIIDetector struct{}

// Name identifies the detector.
func (PIIDetector) Name() string { return "pii" }

// Detect returns PII findings.
func (PIIDetector) Detect(text string) []Finding {
	return runPatterns(text, piiPatterns)
}

func runPatterns(text string, pats []pattern) []Finding {
	var out []Finding
	for _, p := range pats {
		for _, loc := range p.re.FindAllStringIndex(text, -1) {
			m := text[loc[0]:loc[1]]
			if p.validate != nil && !p.validate(m) {
				continue
			}
			out = append(out, Finding{
				Category: p.cat, Rule: p.rule, Severity: p.sev,
				Start: loc[0], End: loc[1], Excerpt: mask(m),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// luhn validates a candidate payment card number, which is what keeps a
// nine-digit position quantity from being reported as a credit card.
func luhn(s string) bool {
	digits := make([]int, 0, 19)
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits = append(digits, int(r-'0'))
		case r == ' ' || r == '-':
		default:
			return false
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum, alt := 0, false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}
	return sum%10 == 0
}

func validIBANShape(s string) bool {
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	letters := 0
	for _, r := range s[4:] {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	// A pure-alphabetic tail is a ticker or an identifier, not an IBAN.
	return letters < len(s)-4
}

func mask(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return s[:2] + strings.Repeat("*", len(s)-4) + s[len(s)-2:]
}

// ---------- secrets ----------

var secretPatterns = []pattern{
	{rule: "aws_access_key", cat: CatSecret, sev: SevCritical, re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{rule: "private_key_block", cat: CatSecret, sev: SevCritical, re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----`)},
	{rule: "bearer_token", cat: CatSecret, sev: SevHigh, re: regexp.MustCompile(`(?i)\b(?:bearer|api[_\-]?key|secret)\s*[:=]\s*[A-Za-z0-9._\-]{16,}`)},
	{rule: "jwt", cat: CatSecret, sev: SevHigh, re: regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\b`)},
	{rule: "evm_private_key", cat: CatSecret, sev: SevCritical, re: regexp.MustCompile(`\b0x[a-fA-F0-9]{64}\b`)},
}

// SecretDetector finds credentials.
type SecretDetector struct{}

// Name identifies the detector.
func (SecretDetector) Name() string { return "secret" }

// Detect returns secret findings.
func (SecretDetector) Detect(text string) []Finding { return runPatterns(text, secretPatterns) }

// ---------- prompt injection ----------

// injectionSignals are weighted heuristics. No single signal blocks a turn;
// the score is the sum, and the threshold is a per-tier policy input. This is
// deliberate: an analyst legitimately writes "ignore the stale price" and a
// guardrail that blocks on one phrase is a guardrail operators route around.
var injectionSignals = []struct {
	rule   string
	weight int
	re     *regexp.Regexp
	cat    Category
}{
	{"instruction_override", 40, regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget|override)\b[^.]{0,40}\b(?:previous|prior|above|earlier|all)\b[^.]{0,20}\b(?:instruction|prompt|rule|direction|context)s?\b`), CatInjection},
	{"role_switch", 35, regexp.MustCompile(`(?i)\b(?:you are now|from now on you are|act as|pretend to be|roleplay as)\b`), CatInjection},
	{"system_prompt_probe", 30, regexp.MustCompile(`(?i)\b(?:reveal|show|print|repeat|display)\b[^.]{0,30}\b(?:system prompt|instructions|your rules|initial prompt)\b`), CatExfil},
	{"policy_bypass", 45, regexp.MustCompile(`(?i)\b(?:bypass|skip|disable|turn off|circumvent)\b[^.]{0,30}\b(?:approval|policy|guardrail|check|control|four.eyes|review)s?\b`), CatInjection},
	{"authority_claim", 30, regexp.MustCompile(`(?i)\b(?:i am|this is)\b[^.]{0,20}\b(?:the )?(?:cfo|ceo|administrator|admin|auditor|regulator|head of risk)\b`), CatInjection},
	{"urgency_pressure", 15, regexp.MustCompile(`(?i)\b(?:urgent|immediately|right now|before (?:the )?(?:market )?close|no time)\b[^.]{0,40}\b(?:transfer|wire|send|move|release)\b`), CatInjection},
	{"tool_smuggling", 45, regexp.MustCompile(`(?i)\b(?:call|invoke|execute|run)\b[^.]{0,20}\b(?:execute_transfer|issue_margin_call|redeem_shares|tool)\b`), CatInjection},
	{"encoded_payload", 25, regexp.MustCompile(`\b[A-Za-z0-9+/]{80,}={0,2}\b`), CatInjection},
	{"data_exfil", 40, regexp.MustCompile(`(?i)\b(?:send|post|upload|email|forward)\b[^.]{0,30}\b(?:to|at)\b[^.]{0,20}(?:https?://|@)`), CatExfil},
	{"jailbreak_persona", 35, regexp.MustCompile(`(?i)\b(?:dan mode|developer mode|jailbreak|no restrictions|unfiltered)\b`), CatJailbreak},
	{"hidden_directive", 35, regexp.MustCompile(`(?i)<!--[^>]{0,200}(?:ignore|instruct|system|transfer)[^>]{0,200}-->`), CatInjection},
	{"invisible_unicode", 30, regexp.MustCompile(`[\x{200B}\x{200C}\x{200D}\x{2060}\x{FEFF}]{3,}`), CatInjection},
}

// InjectionDetector scores prompt-injection and jailbreak attempts.
type InjectionDetector struct {
	// Threshold is the score at or above which a finding is reported as
	// critical. Below it, findings are reported at their signal weight.
	Threshold int
}

// Name identifies the detector.
func (InjectionDetector) Name() string { return "injection" }

// Detect returns injection findings and an aggregate score encoded in the
// detail of a summary finding.
func (d InjectionDetector) Detect(text string) []Finding {
	th := d.Threshold
	if th == 0 {
		th = 50
	}
	var out []Finding
	score := 0
	for _, s := range injectionSignals {
		loc := s.re.FindStringIndex(text)
		if loc == nil {
			continue
		}
		score += s.weight
		sev := SevMedium
		switch {
		case s.weight >= 40:
			sev = SevHigh
		case s.weight <= 20:
			sev = SevLow
		}
		out = append(out, Finding{
			Category: s.cat, Rule: s.rule, Severity: sev,
			Start: loc[0], End: loc[1], Excerpt: excerpt(text, loc[0], loc[1]),
			Detail: fmt.Sprintf("weight %d", s.weight),
		})
	}
	if score > 0 {
		sev := SevLow
		switch {
		case score >= th+30:
			sev = SevCritical
		case score >= th:
			sev = SevHigh
		case score >= th/2:
			sev = SevMedium
		}
		out = append(out, Finding{
			Category: CatInjection, Rule: "injection_score", Severity: sev,
			Detail: fmt.Sprintf("score %d threshold %d", score, th),
		})
	}
	return out
}

func excerpt(text string, a, b int) string {
	if a < 0 {
		a = 0
	}
	if b > len(text) {
		b = len(text)
	}
	s := strings.TrimSpace(text[a:b])
	if len(s) > 120 {
		s = s[:117] + "..."
	}
	return s
}

// ---------- grounding ----------

var numberRe = regexp.MustCompile(`-?\d[\d,]*\.?\d*`)

// GroundingDetector reports numbers in model output that do not appear in the
// record the answer was supposed to be derived from.
//
// This is the single highest-value output check in a risk setting. A model that
// invents an exposure figure is more dangerous than one that refuses, and no
// amount of prompt engineering makes invention impossible. The engines produce
// the numbers; the model may only narrate them.
type GroundingDetector struct {
	// Grounded holds every number the model is permitted to state, as it was
	// rendered by the engines.
	Grounded []string
	// AllowSmall permits small integers (dates, counts, list indices) that are
	// not plausibly financial quantities.
	AllowSmall bool
}

// Name identifies the detector.
func (GroundingDetector) Name() string { return "grounding" }

// Detect returns ungrounded-number findings.
func (g GroundingDetector) Detect(text string) []Finding {
	if len(g.Grounded) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(g.Grounded)*2)
	for _, v := range g.Grounded {
		for _, n := range numberRe.FindAllString(v, -1) {
			allowed[normalizeNum(n)] = true
		}
		allowed[normalizeNum(v)] = true
	}
	var out []Finding
	for _, loc := range numberRe.FindAllStringIndex(text, -1) {
		raw := text[loc[0]:loc[1]]
		n := normalizeNum(raw)
		if n == "" || allowed[n] {
			continue
		}
		if g.AllowSmall && len(strings.TrimLeft(n, "-")) <= 4 && !strings.Contains(n, ".") {
			continue
		}
		out = append(out, Finding{
			Category: CatUngrounded, Rule: "ungrounded_number", Severity: SevHigh,
			Start: loc[0], End: loc[1], Excerpt: raw,
			Detail: "number does not appear in the grounded record",
		})
	}
	return out
}

func normalizeNum(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return ""
	}
	// Trim trailing zeros in the fractional part so "1500.00" matches "1500".
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = strings.TrimRight(s, "0")
		s = strings.TrimSuffix(s, ".")
	}
	return s
}

// ---------- domain scope ----------

// DomainDetector flags turns that are outside the collateral-operations
// domain. An assistant wired to money-movement tools has no business answering
// general questions, and off-domain traffic is the first sign of misuse.
type DomainDetector struct {
	Keywords []string
	MinHits  int
}

// DefaultDomainKeywords describes the collateral-operations vocabulary.
var DefaultDomainKeywords = []string{
	"collateral", "margin", "exposure", "haircut", "counterparty", "position",
	"netting", "concentration", "stress", "transfer", "redeem", "redemption",
	"fund", "token", "ledger", "settlement", "nav", "limit", "breach", "custody",
	"pledge", "eligibility", "valuation", "portfolio", "issuer", "share", "shares",
}

// Name identifies the detector.
func (DomainDetector) Name() string { return "domain" }

// Detect returns an off-domain finding when too few domain terms appear.
func (d DomainDetector) Detect(text string) []Finding {
	kw := d.Keywords
	if kw == nil {
		kw = DefaultDomainKeywords
	}
	min := d.MinHits
	if min == 0 {
		min = 1
	}
	l := strings.ToLower(text)
	hits := 0
	for _, k := range kw {
		if strings.Contains(l, k) {
			hits++
		}
	}
	if hits >= min {
		return nil
	}
	if len(strings.Fields(text)) < 4 {
		return nil // greetings and acknowledgements are not off-domain traffic
	}
	return []Finding{{
		Category: CatOffDomain, Rule: "off_domain", Severity: SevLow,
		Detail: fmt.Sprintf("%d domain terms, want %d", hits, min),
	}}
}

// Redact replaces every finding's span with a typed placeholder and returns the
// redacted text plus the placeholder map, which is retained as evidence rather
// than discarded.
func Redact(text string, findings []Finding) (string, map[string]string) {
	spans := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if f.End > f.Start && (f.Category == CatPII || f.Category == CatSecret) {
			spans = append(spans, f)
		}
	}
	if len(spans) == 0 {
		return text, nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	var sb strings.Builder
	placeholders := map[string]string{}
	last, n := 0, 0
	for _, f := range spans {
		if f.Start < last {
			continue // overlapping match already redacted
		}
		n++
		ph := fmt.Sprintf("[%s_%d]", strings.ToUpper(f.Rule), n)
		sb.WriteString(text[last:f.Start])
		sb.WriteString(ph)
		placeholders[ph] = mask(text[f.Start:f.End])
		last = f.End
	}
	sb.WriteString(text[last:])
	return sb.String(), placeholders
}

// MaxSeverity returns the highest severity among findings.
func MaxSeverity(fs []Finding) Severity {
	m := SevInfo
	for _, f := range fs {
		if f.Severity > m {
			m = f.Severity
		}
	}
	return m
}
