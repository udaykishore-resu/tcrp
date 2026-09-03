// Package expr is a small, total expression language used by the policy
// gateway's rule conditions.
//
// It exists because the gateway must evaluate policy in-process, in
// microseconds, on the hot path of every tool call, and because a policy that
// can loop or recurse is a policy that can hang a trading system. The language
// has no loops, no user-defined functions and no unbounded recursion: every
// expression terminates in time proportional to its own size.
//
// Grammar:
//
//	expr       := or
//	or         := and ( "||" and )*
//	and        := equality ( "&&" equality )*
//	equality   := comparison ( ( "==" | "!=" ) comparison )*
//	comparison := unary ( ( "<" | "<=" | ">" | ">=" | "in" ) unary )*
//	unary      := ( "!" | "-" ) unary | postfix
//	postfix    := primary ( "." IDENT | "[" expr "]" | "(" args ")" )*
//	primary    := NUMBER | STRING | "true" | "false" | "null" | IDENT
//	            | "(" expr ")" | "[" ( expr ( "," expr )* )? "]"
package expr

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Node is a compiled expression.
type Node interface {
	Eval(env map[string]any) (any, error)
	String() string
}

// Program is a compiled, reusable expression.
type Program struct {
	src  string
	root Node
}

// Source returns the original expression text.
func (p *Program) Source() string { return p.src }

// Eval evaluates the program against env.
func (p *Program) Eval(env map[string]any) (any, error) {
	if p == nil || p.root == nil {
		return nil, fmt.Errorf("expr: nil program")
	}
	return p.root.Eval(env)
}

// EvalBool evaluates the program and requires a boolean result. A condition
// that evaluates to a non-boolean is a policy authoring error and is reported
// as such rather than being coerced, because a silently truthy policy is worse
// than a broken one.
func (p *Program) EvalBool(env map[string]any) (bool, error) {
	v, err := p.Eval(env)
	if err != nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("expr: condition %q evaluated to %T, want bool", p.src, v)
	}
	return b, nil
}

var cache sync.Map // string -> *Program

// Compile parses src into a Program, memoising by source text.
func Compile(src string) (*Program, error) {
	if v, ok := cache.Load(src); ok {
		return v.(*Program), nil
	}
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{toks: toks, src: src}
	root, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tEOF {
		return nil, p.errorf("unexpected %s after expression", p.peek())
	}
	prog := &Program{src: src, root: root}
	cache.Store(src, prog)
	return prog, nil
}

// MustCompile is Compile for constants.
func MustCompile(src string) *Program {
	p, err := Compile(src)
	if err != nil {
		panic(err)
	}
	return p
}

// ---------- lexer ----------

type tokKind int

const (
	tEOF tokKind = iota
	tIdent
	tNumber
	tString
	tOp
)

type token struct {
	kind tokKind
	s    string
	num  float64
	pos  int
}

func (t token) String() string {
	switch t.kind {
	case tEOF:
		return "end of expression"
	case tString:
		return strconv.Quote(t.s)
	default:
		return t.s
	}
}

func lex(src string) ([]token, error) {
	var out []token
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"' || c == '\'':
			quote := c
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != quote {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					default:
						sb.WriteByte(src[j])
					}
				} else {
					sb.WriteByte(src[j])
				}
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("expr: unterminated string at %d", i)
			}
			out = append(out, token{kind: tString, s: sb.String(), pos: i})
			i = j + 1
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == '_') {
				j++
			}
			raw := strings.ReplaceAll(src[i:j], "_", "")
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("expr: bad number %q at %d", raw, i)
			}
			out = append(out, token{kind: tNumber, num: n, s: raw, pos: i})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			out = append(out, token{kind: tIdent, s: src[i:j], pos: i})
			i = j
		default:
			two := ""
			if i+1 < len(src) {
				two = src[i : i+2]
			}
			switch two {
			case "==", "!=", "<=", ">=", "&&", "||":
				out = append(out, token{kind: tOp, s: two, pos: i})
				i += 2
				continue
			}
			switch c {
			case '(', ')', '[', ']', '.', ',', '!', '<', '>', '+', '-', '*', '/', '%':
				out = append(out, token{kind: tOp, s: string(c), pos: i})
				i++
			default:
				return nil, fmt.Errorf("expr: unexpected character %q at %d", string(c), i)
			}
		}
	}
	return append(out, token{kind: tEOF, pos: len(src)}), nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// ---------- parser ----------

type parser struct {
	toks []token
	i    int
	src  string
}

// peek clamps to the trailing EOF token. A production that consumes a token in
// order to report a better error — `input.` consuming the end of input before
// complaining about the missing field name — would otherwise index past the
// slice and panic, which for this package means a malformed rule crashing the
// process that is loading the policy bundle.
func (p *parser) peek() token {
	if p.i >= len(p.toks) {
		return p.toks[len(p.toks)-1]
	}
	return p.toks[p.i]
}

func (p *parser) next() token { t := p.peek(); p.i++; return t }
func (p *parser) accept(op string) bool {
	if t := p.peek(); (t.kind == tOp || t.kind == tIdent) && t.s == op {
		p.i++
		return true
	}
	return false
}
func (p *parser) expect(op string) error {
	if p.accept(op) {
		return nil
	}
	return p.errorf("expected %q, got %s", op, p.peek())
}
func (p *parser) errorf(f string, a ...any) error {
	return fmt.Errorf("expr: %s (in %q at %d)", fmt.Sprintf(f, a...), p.src, p.peek().pos)
}

func (p *parser) parseExpr() (Node, error) { return p.parseOr() }

func (p *parser) parseOr() (Node, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept("||") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &orNode{left, right}
	}
	return left, nil
}

func (p *parser) parseAnd() (Node, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for p.accept("&&") {
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		left = &andNode{left, right}
	}
	return left, nil
}

func (p *parser) parseEquality() (Node, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.accept("=="):
			r, err := p.parseComparison()
			if err != nil {
				return nil, err
			}
			left = &binNode{"==", left, r}
		case p.accept("!="):
			r, err := p.parseComparison()
			if err != nil {
				return nil, err
			}
			left = &binNode{"!=", left, r}
		default:
			return left, nil
		}
	}
}

func (p *parser) parseComparison() (Node, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.accept("<="):
			op = "<="
		case p.accept(">="):
			op = ">="
		case p.accept("<"):
			op = "<"
		case p.accept(">"):
			op = ">"
		case p.accept("in"):
			op = "in"
		default:
			return left, nil
		}
		r, err := p.parseAdditive()
		if err != nil {
			return nil, err
		}
		left = &binNode{op, left, r}
	}
}

func (p *parser) parseAdditive() (Node, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.accept("+"):
			op = "+"
		case p.accept("-"):
			op = "-"
		default:
			return left, nil
		}
		r, err := p.parseMultiplicative()
		if err != nil {
			return nil, err
		}
		left = &binNode{op, left, r}
	}
}

func (p *parser) parseMultiplicative() (Node, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.accept("*"):
			op = "*"
		case p.accept("/"):
			op = "/"
		case p.accept("%"):
			op = "%"
		default:
			return left, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binNode{op, left, r}
	}
}

func (p *parser) parseUnary() (Node, error) {
	if p.accept("!") {
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &notNode{n}, nil
	}
	if p.accept("-") {
		n, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &negNode{n}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (Node, error) {
	n, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.accept("."):
			t := p.next()
			if t.kind != tIdent {
				return nil, p.errorf("expected field name after '.', got %s", t)
			}
			n = &fieldNode{n, t.s}
		case p.accept("["):
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			n = &indexNode{n, idx}
		case p.accept("("):
			id, ok := n.(*identNode)
			if !ok {
				return nil, p.errorf("only named functions may be called")
			}
			var args []Node
			if !p.accept(")") {
				for {
					a, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.accept(",") {
						continue
					}
					if err := p.expect(")"); err != nil {
						return nil, err
					}
					break
				}
			}
			fn, ok := builtins[id.name]
			if !ok {
				return nil, p.errorf("unknown function %q", id.name)
			}
			if fn.arity >= 0 && len(args) != fn.arity {
				return nil, p.errorf("%s takes %d argument(s), got %d", id.name, fn.arity, len(args))
			}
			n = &callNode{id.name, fn.fn, args}
		default:
			return n, nil
		}
	}
}

func (p *parser) parsePrimary() (Node, error) {
	t := p.next()
	switch t.kind {
	case tNumber:
		return &litNode{t.num}, nil
	case tString:
		return &litNode{t.s}, nil
	case tIdent:
		switch t.s {
		case "true":
			return &litNode{true}, nil
		case "false":
			return &litNode{false}, nil
		case "null":
			return &litNode{nil}, nil
		}
		return &identNode{t.s}, nil
	case tOp:
		switch t.s {
		case "(":
			n, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return n, nil
		case "[":
			var items []Node
			if !p.accept("]") {
				for {
					it, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					items = append(items, it)
					if p.accept(",") {
						continue
					}
					if err := p.expect("]"); err != nil {
						return nil, err
					}
					break
				}
			}
			return &listNode{items}, nil
		}
	}
	p.i--
	return nil, p.errorf("unexpected %s", t)
}

// ---------- nodes ----------

type litNode struct{ v any }

func (n *litNode) Eval(map[string]any) (any, error) { return n.v, nil }
func (n *litNode) String() string                   { return fmt.Sprintf("%v", n.v) }

type identNode struct{ name string }

func (n *identNode) Eval(env map[string]any) (any, error) {
	v, ok := env[n.name]
	if !ok {
		// An unknown identifier is null, not an error: policies are written
		// against inputs that may legitimately omit optional sections, and
		// `!input.approval.recorded` must work when there is no approval at all.
		return nil, nil
	}
	return v, nil
}
func (n *identNode) String() string { return n.name }

type fieldNode struct {
	obj   Node
	field string
}

func (n *fieldNode) Eval(env map[string]any) (any, error) {
	o, err := n.obj.Eval(env)
	if err != nil {
		return nil, err
	}
	switch t := o.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return t[n.field], nil
	case map[string]string:
		return t[n.field], nil
	default:
		return nil, fmt.Errorf("expr: cannot read field %q of %T", n.field, o)
	}
}
func (n *fieldNode) String() string { return n.obj.String() + "." + n.field }

type indexNode struct{ obj, idx Node }

func (n *indexNode) Eval(env map[string]any) (any, error) {
	o, err := n.obj.Eval(env)
	if err != nil {
		return nil, err
	}
	i, err := n.idx.Eval(env)
	if err != nil {
		return nil, err
	}
	switch t := o.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		s, ok := i.(string)
		if !ok {
			return nil, fmt.Errorf("expr: map index must be a string, got %T", i)
		}
		return t[s], nil
	case []any:
		f, ok := toNumber(i)
		if !ok {
			return nil, fmt.Errorf("expr: list index must be a number, got %T", i)
		}
		k := int(f)
		if k < 0 || k >= len(t) {
			return nil, nil
		}
		return t[k], nil
	case []string:
		f, ok := toNumber(i)
		if !ok {
			return nil, fmt.Errorf("expr: list index must be a number, got %T", i)
		}
		k := int(f)
		if k < 0 || k >= len(t) {
			return nil, nil
		}
		return t[k], nil
	default:
		return nil, fmt.Errorf("expr: cannot index %T", o)
	}
}
func (n *indexNode) String() string { return fmt.Sprintf("%s[%s]", n.obj, n.idx) }

type notNode struct{ n Node }

func (n *notNode) Eval(env map[string]any) (any, error) {
	v, err := n.n.Eval(env)
	if err != nil {
		return nil, err
	}
	return !truthy(v), nil
}
func (n *notNode) String() string { return "!" + n.n.String() }

type negNode struct{ n Node }

func (n *negNode) Eval(env map[string]any) (any, error) {
	v, err := n.n.Eval(env)
	if err != nil {
		return nil, err
	}
	f, ok := toNumber(v)
	if !ok {
		return nil, fmt.Errorf("expr: cannot negate %T", v)
	}
	return -f, nil
}
func (n *negNode) String() string { return "-" + n.n.String() }

type andNode struct{ l, r Node }

func (n *andNode) Eval(env map[string]any) (any, error) {
	l, err := n.l.Eval(env)
	if err != nil {
		return nil, err
	}
	if !truthy(l) {
		return false, nil // short circuit
	}
	r, err := n.r.Eval(env)
	if err != nil {
		return nil, err
	}
	return truthy(r), nil
}
func (n *andNode) String() string { return "(" + n.l.String() + " && " + n.r.String() + ")" }

type orNode struct{ l, r Node }

func (n *orNode) Eval(env map[string]any) (any, error) {
	l, err := n.l.Eval(env)
	if err != nil {
		return nil, err
	}
	if truthy(l) {
		return true, nil
	}
	r, err := n.r.Eval(env)
	if err != nil {
		return nil, err
	}
	return truthy(r), nil
}
func (n *orNode) String() string { return "(" + n.l.String() + " || " + n.r.String() + ")" }

type listNode struct{ items []Node }

func (n *listNode) Eval(env map[string]any) (any, error) {
	out := make([]any, 0, len(n.items))
	for _, it := range n.items {
		v, err := it.Eval(env)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func (n *listNode) String() string {
	parts := make([]string, len(n.items))
	for i, it := range n.items {
		parts[i] = it.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

type binNode struct {
	op   string
	l, r Node
}

func (n *binNode) Eval(env map[string]any) (any, error) {
	l, err := n.l.Eval(env)
	if err != nil {
		return nil, err
	}
	r, err := n.r.Eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case "==":
		return equal(l, r), nil
	case "!=":
		return !equal(l, r), nil
	case "in":
		return contains(r, l), nil
	}
	lf, lok := toNumber(l)
	rf, rok := toNumber(r)
	if n.op == "+" && (!lok || !rok) {
		ls, lsok := l.(string)
		rs, rsok := r.(string)
		if lsok && rsok {
			return ls + rs, nil
		}
	}
	if !lok || !rok {
		return nil, fmt.Errorf("expr: operator %q needs numbers, got %T and %T", n.op, l, r)
	}
	switch n.op {
	case "<":
		return lf < rf, nil
	case "<=":
		return lf <= rf, nil
	case ">":
		return lf > rf, nil
	case ">=":
		return lf >= rf, nil
	case "+":
		return lf + rf, nil
	case "-":
		return lf - rf, nil
	case "*":
		return lf * rf, nil
	case "/":
		if rf == 0 {
			return nil, fmt.Errorf("expr: division by zero")
		}
		return lf / rf, nil
	case "%":
		if rf == 0 {
			return nil, fmt.Errorf("expr: modulo by zero")
		}
		return float64(int64(lf) % int64(rf)), nil
	}
	return nil, fmt.Errorf("expr: unknown operator %q", n.op)
}
func (n *binNode) String() string {
	return "(" + n.l.String() + " " + n.op + " " + n.r.String() + ")"
}

type callNode struct {
	name string
	fn   func([]any) (any, error)
	args []Node
}

func (n *callNode) Eval(env map[string]any) (any, error) {
	vals := make([]any, 0, len(n.args))
	for _, a := range n.args {
		v, err := a.Eval(env)
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
	}
	v, err := n.fn(vals)
	if err != nil {
		return nil, fmt.Errorf("expr: %s: %w", n.name, err)
	}
	return v, nil
}
func (n *callNode) String() string {
	parts := make([]string, len(n.args))
	for i, a := range n.args {
		parts[i] = a.String()
	}
	return n.name + "(" + strings.Join(parts, ", ") + ")"
}

// ---------- helpers and builtins ----------

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case []any:
		return len(t) > 0
	case []string:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	default:
		return true
	}
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

func equal(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if as, ok := a.(string); ok {
		bs, ok := b.(string)
		return ok && as == bs
	}
	if ab, ok := a.(bool); ok {
		bb, ok := b.(bool)
		return ok && ab == bb
	}
	af, aok := toNumber(a)
	bf, bok := toNumber(b)
	if aok && bok {
		return af == bf
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

func contains(haystack, needle any) bool {
	switch t := haystack.(type) {
	case []any:
		for _, v := range t {
			if equal(v, needle) {
				return true
			}
		}
	case []string:
		for _, v := range t {
			if equal(v, needle) {
				return true
			}
		}
	case map[string]any:
		s, ok := needle.(string)
		if !ok {
			return false
		}
		_, found := t[s]
		return found
	case string:
		s, ok := needle.(string)
		return ok && strings.Contains(t, s)
	}
	return false
}

func toList(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	case nil:
		return nil
	default:
		return []any{v}
	}
}

type builtin struct {
	arity int // -1 for variadic
	fn    func([]any) (any, error)
}

var reCache sync.Map

var builtins = map[string]builtin{
	"has": {2, func(a []any) (any, error) { return contains(a[0], a[1]), nil }},
	"size": {1, func(a []any) (any, error) {
		switch t := a[0].(type) {
		case string:
			return float64(len(t)), nil
		case []any:
			return float64(len(t)), nil
		case []string:
			return float64(len(t)), nil
		case map[string]any:
			return float64(len(t)), nil
		case nil:
			return float64(0), nil
		default:
			return nil, fmt.Errorf("cannot size %T", a[0])
		}
	}},
	"startsWith": {2, func(a []any) (any, error) { return strings.HasPrefix(str(a[0]), str(a[1])), nil }},
	"endsWith":   {2, func(a []any) (any, error) { return strings.HasSuffix(str(a[0]), str(a[1])), nil }},
	"contains":   {2, func(a []any) (any, error) { return strings.Contains(str(a[0]), str(a[1])), nil }},
	"lower":      {1, func(a []any) (any, error) { return strings.ToLower(str(a[0])), nil }},
	"upper":      {1, func(a []any) (any, error) { return strings.ToUpper(str(a[0])), nil }},
	"matches": {2, func(a []any) (any, error) {
		pat := str(a[1])
		re, ok := reCache.Load(pat)
		if !ok {
			c, err := regexp.Compile(pat)
			if err != nil {
				return nil, err
			}
			reCache.Store(pat, c)
			re = c
		}
		return re.(*regexp.Regexp).MatchString(str(a[0])), nil
	}},
	"int": {1, func(a []any) (any, error) {
		f, ok := toNumber(a[0])
		if !ok {
			n, err := strconv.ParseFloat(str(a[0]), 64)
			if err != nil {
				return nil, fmt.Errorf("cannot convert %v to int", a[0])
			}
			f = n
		}
		return float64(int64(f)), nil
	}},
	"string": {1, func(a []any) (any, error) { return str(a[0]), nil }},
	"anyOf": {-1, func(a []any) (any, error) {
		if len(a) < 2 {
			return nil, fmt.Errorf("anyOf needs a list and at least one value")
		}
		for _, want := range a[1:] {
			if contains(a[0], want) {
				return true, nil
			}
		}
		return false, nil
	}},
	"allOf": {-1, func(a []any) (any, error) {
		if len(a) < 2 {
			return nil, fmt.Errorf("allOf needs a list and at least one value")
		}
		for _, want := range a[1:] {
			if !contains(a[0], want) {
				return false, nil
			}
		}
		return true, nil
	}},
	"subset": {2, func(a []any) (any, error) {
		for _, v := range toList(a[0]) {
			if !contains(a[1], v) {
				return false, nil
			}
		}
		return true, nil
	}},
	"intersects": {2, func(a []any) (any, error) {
		for _, v := range toList(a[1]) {
			if contains(a[0], v) {
				return true, nil
			}
		}
		return false, nil
	}},
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	if f, ok := toNumber(v); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}
