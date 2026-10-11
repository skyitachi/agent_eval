package metric

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// A small, dependency-free expression language for custom metrics.
//
// Two grammars share one lexer:
//
//	Arithmetic (the `expr:` form) -- evaluated against a map of already-computed
//	  metric values, so a derived metric can reference the built-ins:
//
//	    cost_per_resolved_usd / max(resolve_rate, 0.01)
//
//	Filter (the `where:` form) -- evaluated against a single sample, producing a
//	  boolean:
//
//	    difficulty == 'hard' and passed
//	    category in ['bugfix', 'feature'] and turns > 20
//	    tags contains 'security'
//
// Deliberately absent: assignment, user-defined functions, and any form of I/O.
// A metric definition comes from a dataset file, so the language must not be able
// to do anything but arithmetic and comparison.

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNumber
	tokString
	tokIdent
	tokOp
	tokLParen
	tokRParen
	tokComma
	tokLBracket
	tokRBracket
)

type token struct {
	kind tokenKind
	text string
	num  float64
	pos  int
}

type lexer struct {
	src string
	pos int
}

func (l *lexer) next() (token, error) {
	for l.pos < len(l.src) && unicode.IsSpace(rune(l.src[l.pos])) {
		l.pos++
	}
	start := l.pos
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: start}, nil
	}
	ch := l.src[l.pos]

	switch {
	case ch == '(':
		l.pos++
		return token{kind: tokLParen, text: "(", pos: start}, nil
	case ch == ')':
		l.pos++
		return token{kind: tokRParen, text: ")", pos: start}, nil
	case ch == '[':
		l.pos++
		return token{kind: tokLBracket, text: "[", pos: start}, nil
	case ch == ']':
		l.pos++
		return token{kind: tokRBracket, text: "]", pos: start}, nil
	case ch == ',':
		l.pos++
		return token{kind: tokComma, text: ",", pos: start}, nil
	case ch == '\'' || ch == '"':
		quote := ch
		l.pos++
		var sb strings.Builder
		for l.pos < len(l.src) {
			c := l.src[l.pos]
			if c == '\\' && l.pos+1 < len(l.src) {
				l.pos++
				sb.WriteByte(l.src[l.pos])
				l.pos++
				continue
			}
			if c == quote {
				l.pos++
				return token{kind: tokString, text: sb.String(), pos: start}, nil
			}
			sb.WriteByte(c)
			l.pos++
		}
		return token{}, fmt.Errorf("unterminated string literal at offset %d", start)
	}

	// Numbers, including exponents and a leading dot.
	if unicode.IsDigit(rune(ch)) || (ch == '.' && l.pos+1 < len(l.src) && unicode.IsDigit(rune(l.src[l.pos+1]))) {
		for l.pos < len(l.src) {
			c := l.src[l.pos]
			if unicode.IsDigit(rune(c)) || c == '.' || c == 'e' || c == 'E' ||
				((c == '+' || c == '-') && l.pos > start && (l.src[l.pos-1] == 'e' || l.src[l.pos-1] == 'E')) {
				l.pos++
				continue
			}
			break
		}
		text := l.src[start:l.pos]
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return token{}, fmt.Errorf("invalid number %q at offset %d", text, start)
		}
		return token{kind: tokNumber, text: text, num: f, pos: start}, nil
	}

	// Identifiers and keywords.
	if unicode.IsLetter(rune(ch)) || ch == '_' {
		for l.pos < len(l.src) {
			c := rune(l.src[l.pos])
			if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '_' || c == '.' {
				l.pos++
				continue
			}
			break
		}
		return token{kind: tokIdent, text: l.src[start:l.pos], pos: start}, nil
	}

	// Operators, longest match first so that >= is not read as > then =.
	for _, op := range []string{"==", "!=", ">=", "<=", "&&", "||", "~=", "=", ">", "<", "+", "-", "*", "/", "%", "^"} {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.pos += len(op)
			return token{kind: tokOp, text: op, pos: start}, nil
		}
	}
	return token{}, fmt.Errorf("unexpected character %q at offset %d", string(ch), start)
}

func lexAll(src string) ([]token, error) {
	l := &lexer{src: src}
	var out []token
	for {
		t, err := l.next()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
		if t.kind == tokEOF {
			return out, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Arithmetic and comparison expressions
// ---------------------------------------------------------------------------
//
// Grammar, lowest precedence first:
//
//	expr    := logicOr
//	logicOr := logicAnd (('or' | '||') logicAnd)*
//	logicAnd:= comparison (('and' | '&&') comparison)*
//	comparison := additive (('==' | '!=' | '>' | '>=' | '<' | '<=') additive)?
//	additive:= multiplicative (('+' | '-') multiplicative)*
//	multiplicative := unary (('*' | '/' | '%') unary)*
//	unary   := ('-' | '+') unary | power
//	power   := primary ('^' unary)?
//	primary := number | ident | func '(' args ')' | '(' expr ')'
//
// Comparisons and boolean operators yield 1 or 0, which is what makes
// `if(resolve_rate > 0.5, ...)` and `pass_rate * (cost_usd < 1)` expressible
// without a separate conditional grammar.

// Resolver looks up a named value. ok=false makes the reference an error, which
// is how a typo in a metric name is caught instead of silently becoming zero.
type Resolver func(name string) (float64, bool)

// EvalExpr evaluates an expression against named values.
func EvalExpr(source string, vars map[string]float64) (float64, error) {
	return EvalExprWith(source, func(name string) (float64, bool) {
		v, ok := vars[name]
		if !ok {
			v, ok = vars[strings.ToLower(name)]
		}
		return v, ok
	})
}

// EvalExprWith evaluates an expression using a custom resolver.
func EvalExprWith(source string, resolve Resolver) (float64, error) {
	toks, err := lexAll(source)
	if err != nil {
		return 0, err
	}
	p := &exprParser{toks: toks, resolve: resolve}
	v, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	if p.peek().kind != tokEOF {
		return 0, fmt.Errorf("unexpected %q at offset %d in expression", p.peek().text, p.peek().pos)
	}
	return v, nil
}

// CheckExprSyntax validates an expression without needing metric values.
//
// It uses a resolver that accepts any identifier, so only genuine syntax errors
// (unbalanced parentheses, a trailing operator, an unknown function) are
// reported. This runs at configuration time, which is where a broken metric
// definition should surface -- not halfway through a paid evaluation run.
func CheckExprSyntax(source string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("expression is empty")
	}
	_, err := EvalExprWith(source, func(string) (float64, bool) { return 0, true })
	return err
}

type exprParser struct {
	toks    []token
	pos     int
	resolve Resolver
}

func (p *exprParser) peek() token { return p.toks[p.pos] }

func (p *exprParser) advance() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *exprParser) isKeyword(kw string) bool {
	t := p.peek()
	if t.kind == tokOp {
		switch kw {
		case "and":
			return t.text == "&&"
		case "or":
			return t.text == "||"
		}
		return false
	}
	return t.kind == tokIdent && strings.EqualFold(t.text, kw)
}

func (p *exprParser) consumeKeyword(kw string) bool {
	if p.isKeyword(kw) {
		p.advance()
		return true
	}
	return false
}

func (p *exprParser) parseExpr() (float64, error) { return p.parseOr() }

func (p *exprParser) parseOr() (float64, error) {
	left, err := p.parseAnd()
	if err != nil {
		return 0, err
	}
	for p.consumeKeyword("or") || p.peekOp("||") {
		right, err := p.parseAnd()
		if err != nil {
			return 0, err
		}
		left = boolToFloat(left != 0 || right != 0)
	}
	return left, nil
}

func (p *exprParser) parseAnd() (float64, error) {
	left, err := p.parseComparison()
	if err != nil {
		return 0, err
	}
	for p.consumeKeyword("and") || p.peekOp("&&") {
		right, err := p.parseComparison()
		if err != nil {
			return 0, err
		}
		left = boolToFloat(left != 0 && right != 0)
	}
	return left, nil
}

func (p *exprParser) peekOp(op string) bool {
	t := p.peek()
	return t.kind == tokOp && t.text == op
}

func (p *exprParser) parseComparison() (float64, error) {
	left, err := p.parseAdditive()
	if err != nil {
		return 0, err
	}
	t := p.peek()
	if t.kind != tokOp {
		return left, nil
	}
	var cmp func(a, b float64) bool
	switch t.text {
	case "==", "=":
		cmp = func(a, b float64) bool { return a == b }
	case "!=":
		cmp = func(a, b float64) bool { return a != b }
	case ">":
		cmp = func(a, b float64) bool { return a > b }
	case ">=":
		cmp = func(a, b float64) bool { return a >= b }
	case "<":
		cmp = func(a, b float64) bool { return a < b }
	case "<=":
		cmp = func(a, b float64) bool { return a <= b }
	default:
		return left, nil
	}
	p.advance()
	right, err := p.parseAdditive()
	if err != nil {
		return 0, err
	}
	return boolToFloat(cmp(left, right)), nil
}

func (p *exprParser) parseAdditive() (float64, error) {
	left, err := p.parseMultiplicative()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind != tokOp || (t.text != "+" && t.text != "-") {
			return left, nil
		}
		p.advance()
		right, err := p.parseMultiplicative()
		if err != nil {
			return 0, err
		}
		if t.text == "+" {
			left += right
		} else {
			left -= right
		}
	}
}

func (p *exprParser) parseMultiplicative() (float64, error) {
	left, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind != tokOp {
			return left, nil
		}
		switch t.text {
		case "*", "/", "%":
		default:
			return left, nil
		}
		p.advance()
		right, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		switch t.text {
		case "*":
			left *= right
		case "/":
			// A zero denominator yields 0 rather than an error so that a ratio
			// over an empty run still renders a report instead of aborting it.
			// The denominator is itself a reported metric, so the zero is not
			// hiding anything.
			if right == 0 {
				left = 0
			} else {
				left /= right
			}
		case "%":
			if right == 0 {
				left = 0
			} else {
				left = math.Mod(left, right)
			}
		}
	}
}

func (p *exprParser) parseUnary() (float64, error) {
	t := p.peek()
	if t.kind == tokOp && (t.text == "-" || t.text == "+") {
		p.advance()
		v, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if t.text == "-" {
			return -v, nil
		}
		return v, nil
	}
	return p.parsePower()
}

func (p *exprParser) parsePower() (float64, error) {
	base, err := p.parsePrimary()
	if err != nil {
		return 0, err
	}
	if t := p.peek(); t.kind == tokOp && t.text == "^" {
		p.advance()
		exp, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

func (p *exprParser) parsePrimary() (float64, error) {
	t := p.advance()
	switch t.kind {
	case tokNumber:
		return t.num, nil
	case tokIdent:
		lower := strings.ToLower(t.text)
		switch lower {
		case "true":
			return 1, nil
		case "false":
			return 0, nil
		case "inf", "infinity":
			return math.Inf(1), nil
		}
		if p.peek().kind == tokLParen {
			return p.parseCall(lower)
		}
		if p.resolve == nil {
			return 0, fmt.Errorf("no resolver for %q", t.text)
		}
		v, ok := p.resolve(t.text)
		if !ok {
			return 0, fmt.Errorf("unknown metric %q in expression", t.text)
		}
		return v, nil
	case tokLParen:
		v, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek().kind != tokRParen {
			return 0, fmt.Errorf("missing ')' in expression at offset %d", p.peek().pos)
		}
		p.advance()
		return v, nil
	default:
		return 0, fmt.Errorf("unexpected %q at offset %d in expression", t.text, t.pos)
	}
}

func (p *exprParser) parseCall(name string) (float64, error) {
	p.advance() // consume '('
	var args []float64
	if p.peek().kind != tokRParen {
		for {
			v, err := p.parseExpr()
			if err != nil {
				return 0, err
			}
			args = append(args, v)
			if p.peek().kind == tokComma {
				p.advance()
				continue
			}
			break
		}
	}
	if p.peek().kind != tokRParen {
		return 0, fmt.Errorf("missing ')' after %s( at offset %d", name, p.peek().pos)
	}
	p.advance()
	return applyExprFunc(name, args)
}

func applyExprFunc(name string, args []float64) (float64, error) {
	need := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("%s() takes %d argument(s), got %d", name, n, len(args))
		}
		return nil
	}
	switch name {
	case "min":
		if len(args) == 0 {
			return 0, fmt.Errorf("min() needs at least one argument")
		}
		out := args[0]
		for _, v := range args[1:] {
			out = math.Min(out, v)
		}
		return out, nil
	case "max":
		if len(args) == 0 {
			return 0, fmt.Errorf("max() needs at least one argument")
		}
		out := args[0]
		for _, v := range args[1:] {
			out = math.Max(out, v)
		}
		return out, nil
	case "abs":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Abs(args[0]), nil
	case "round":
		if len(args) == 1 {
			return math.Round(args[0]), nil
		}
		if len(args) == 2 {
			f := math.Pow(10, args[1])
			return math.Round(args[0]*f) / f, nil
		}
		return 0, fmt.Errorf("round() takes 1 or 2 arguments, got %d", len(args))
	case "floor":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Floor(args[0]), nil
	case "ceil":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Ceil(args[0]), nil
	case "sqrt":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Sqrt(args[0]), nil
	case "log":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Log(args[0]), nil
	case "exp":
		if err := need(1); err != nil {
			return 0, err
		}
		return math.Exp(args[0]), nil
	case "clamp":
		if err := need(3); err != nil {
			return 0, err
		}
		return math.Max(args[1], math.Min(args[2], args[0])), nil
	case "if":
		if err := need(3); err != nil {
			return 0, err
		}
		if args[0] != 0 {
			return args[1], nil
		}
		return args[2], nil
	default:
		return 0, fmt.Errorf("unknown function %q", name)
	}
}

// ---------------------------------------------------------------------------
// Sample filters
// ---------------------------------------------------------------------------

// CheckFilterSyntax validates a filter without needing a sample.
//
// Unknown field names are tolerated (they can only be resolved against real
// data), but grammar errors and unknown functions are reported, so a typo in a
// `where:` clause is caught at configuration time rather than silently matching
// nothing during a run.
func CheckFilterSyntax(source string) error {
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("filter is empty")
	}
	toks, err := lexAll(source)
	if err != nil {
		return err
	}
	p := &filterParser{toks: toks}
	if _, err := p.parseOr(); err != nil {
		return err
	}
	if p.peek().kind != tokEOF {
		return fmt.Errorf("unexpected %q at offset %d in filter", p.peek().text, p.peek().pos)
	}
	return nil
}

// EvalFilter evaluates a boolean filter expression against one sample.
func EvalFilter(source string, s *Sample) (bool, error) {
	toks, err := lexAll(source)
	if err != nil {
		return false, err
	}
	p := &filterParser{toks: toks, sample: s}
	v, err := p.parseOr()
	if err != nil {
		return false, err
	}
	if p.peek().kind != tokEOF {
		return false, fmt.Errorf("unexpected %q at offset %d in filter", p.peek().text, p.peek().pos)
	}
	return v, nil
}

type filterParser struct {
	toks []token
	pos  int
	// sample is the row being tested. A nil sample means syntax-only mode: the
	// grammar is validated but nothing is evaluated, and any operand resolution
	// that would need real values is skipped. Read-only parsing and evaluation
	// share one parser, so every sample access must tolerate nil.
	sample *Sample
}

func (p *filterParser) peek() token { return p.toks[p.pos] }

func (p *filterParser) advance() token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *filterParser) isKeyword(kw string) bool {
	t := p.peek()
	return t.kind == tokIdent && strings.EqualFold(t.text, kw)
}

func (p *filterParser) parseOr() (bool, error) {
	left, err := p.parseAnd()
	if err != nil {
		return false, err
	}
	for {
		if p.isKeyword("or") || (p.peek().kind == tokOp && p.peek().text == "||") {
			p.advance()
			right, err := p.parseAnd()
			if err != nil {
				return false, err
			}
			left = left || right
			continue
		}
		return left, nil
	}
}

func (p *filterParser) parseAnd() (bool, error) {
	left, err := p.parseNot()
	if err != nil {
		return false, err
	}
	for {
		if p.isKeyword("and") || (p.peek().kind == tokOp && p.peek().text == "&&") {
			p.advance()
			right, err := p.parseNot()
			if err != nil {
				return false, err
			}
			left = left && right
			continue
		}
		return left, nil
	}
}

func (p *filterParser) parseNot() (bool, error) {
	if p.isKeyword("not") {
		p.advance()
		v, err := p.parseNot()
		if err != nil {
			return false, err
		}
		return !v, nil
	}
	return p.parseComparison()
}

func (p *filterParser) parseComparison() (bool, error) {
	// A parenthesised sub-expression.
	if p.peek().kind == tokLParen {
		p.advance()
		v, err := p.parseOr()
		if err != nil {
			return false, err
		}
		if p.peek().kind != tokRParen {
			return false, fmt.Errorf("missing ')' in filter at offset %d", p.peek().pos)
		}
		p.advance()
		return v, nil
	}

	left, isNum, err := p.parseValue()
	if err != nil {
		return false, err
	}

	// A bare boolean field, e.g. `where: passed`.
	if p.endsComparison() {
		if isNum {
			return left.num != 0, nil
		}
		if left.field == "" {
			return false, fmt.Errorf("a literal is not a boolean condition in filter")
		}
		return false, fmt.Errorf("field %q is not a boolean in filter", left.field)
	}

	// Negated membership: `x not in [...]`.
	if p.isIdentKeyword("not") && p.peekAt(1).kind == tokIdent &&
		strings.EqualFold(p.peekAt(1).text, "in") {
		p.advance()
		p.advance()
		right, err := p.parseValueOrList()
		if err != nil {
			return false, err
		}
		ok, err := compareValues(left, isNum, "in", right)
		if err != nil {
			return false, err
		}
		return !ok, nil
	}

	// Infix predicates: `tags contains 'x'`, `category in [...]`,
	// `task_id startswith 'fix'`, `task_id endswith 'y'`.
	if kw, ok := p.infixPredicate(); ok {
		p.advance()
		right, err := p.parseValueOrList()
		if err != nil {
			return false, err
		}
		if p.sample == nil {
			// Syntax-only mode: the shape is valid and there is no row to test.
			// Evaluating here would dereference a nil Sample, which is exactly
			// the panic this guard prevents.
			return false, nil
		}
		return applyInfixPredicate(p.sample, left, kw, right)
	}

	t := p.advance()
	if t.kind != tokOp {
		return false, fmt.Errorf("expected a comparison operator at offset %d, got %q", t.pos, t.text)
	}
	right, err := p.parseValueOrList()
	if err != nil {
		return false, err
	}
	return compareValues(left, isNum, t.text, right)
}

// endsComparison reports whether the expression ends here, i.e. the next token
// cannot continue a comparison.
func (p *filterParser) endsComparison() bool {
	t := p.peek()
	switch t.kind {
	case tokEOF, tokRParen:
		return true
	case tokOp:
		return t.text == "&&" || t.text == "||"
	case tokIdent:
		return strings.EqualFold(t.text, "and") || strings.EqualFold(t.text, "or")
	}
	return false
}

// infixPredicate reports whether the next token is an infix predicate keyword.
func (p *filterParser) infixPredicate() (string, bool) {
	t := p.peek()
	if t.kind != tokIdent {
		return "", false
	}
	lower := strings.ToLower(t.text)
	switch lower {
	case "in", "contains", "startswith", "endswith":
		return lower, true
	}
	return "", false
}

func (p *filterParser) isIdentKeyword(kw string) bool {
	t := p.peek()
	return t.kind == tokIdent && strings.EqualFold(t.text, kw)
}

func (p *filterParser) peekAt(n int) token {
	i := p.pos + n
	if i >= len(p.toks) {
		return token{kind: tokEOF}
	}
	return p.toks[i]
}

// applyInfixPredicate evaluates `field <op> value` written in infix form.
func applyInfixPredicate(s *Sample, left filterValue, kw string, right filterValue) (bool, error) {
	needleOf := func() string {
		if right.isStr {
			return right.str
		}
		return strconv.FormatFloat(right.num, 'f', -1, 64)
	}

	switch kw {
	case "in":
		if !right.isList {
			return false, fmt.Errorf("'in' requires a bracketed list on the right-hand side")
		}
		got := left.str
		if !left.isStr {
			got = strconv.FormatFloat(left.num, 'f', -1, 64)
		}
		for _, item := range right.list {
			if item == got {
				return true, nil
			}
		}
		return false, nil
	case "contains":
		if left.field == "tags" {
			return s.Contains("tags", needleOf()), nil
		}
		haystack := left.str
		if !left.isStr {
			haystack = strconv.FormatFloat(left.num, 'f', -1, 64)
		}
		return strings.Contains(haystack, needleOf()), nil
	case "startswith":
		return strings.HasPrefix(left.str, needleOf()), nil
	case "endswith":
		return strings.HasSuffix(left.str, needleOf()), nil
	}
	return false, fmt.Errorf("unknown predicate %q", kw)
}

// filterValue is a resolved operand.
type filterValue struct {
	num    float64
	str    string
	field  string // non-empty when the operand came from a field reference
	isStr  bool
	list   []string
	isList bool
}

func (p *filterParser) parseValue() (filterValue, bool, error) {
	t := p.advance()
	switch t.kind {
	case tokNumber:
		return filterValue{num: t.num}, true, nil
	case tokString:
		return filterValue{str: t.text, isStr: true}, false, nil
	case tokIdent:
		// Function-style predicates.
		if p.peek().kind == tokLParen {
			return p.parsePredicateCall(strings.ToLower(t.text))
		}
		lower := strings.ToLower(t.text)
		switch lower {
		case "true":
			return filterValue{num: 1}, true, nil
		case "false":
			return filterValue{num: 0}, true, nil
		}
		if p.sample == nil {
			// Syntax-check mode: accept any field name, assume numeric.
			return filterValue{num: 0, field: t.text}, true, nil
		}
		num := p.sample.Numeric()
		if v, ok := num[t.text]; ok {
			return filterValue{num: v, field: t.text}, true, nil
		}
		if v, ok := num[lower]; ok {
			return filterValue{num: v, field: lower}, true, nil
		}
		// Fall back to a string field.
		sv := p.sample.Strings()
		if v, ok := sv[t.text]; ok {
			return filterValue{str: v, field: t.text, isStr: true}, false, nil
		}
		if v, ok := sv[lower]; ok {
			return filterValue{str: v, field: lower, isStr: true}, false, nil
		}
		switch lower {
		case "tags", "category", "task_id", "difficulty", "stop_reason", "run_id":
			return filterValue{field: lower, isStr: true}, false, nil
		}
		return filterValue{}, false, fmt.Errorf("unknown field %q in filter", t.text)
	case tokLParen:
		v, isNum, err := p.parseValue()
		if err != nil {
			return filterValue{}, false, err
		}
		if p.peek().kind != tokRParen {
			return filterValue{}, false, fmt.Errorf("missing ')' in filter")
		}
		p.advance()
		return v, isNum, nil
	default:
		return filterValue{}, false, fmt.Errorf("unexpected %q at offset %d in filter", t.text, t.pos)
	}
}

// parseValueOrList handles the right-hand side, which may be a single value or a
// bracketed list for `in` comparisons.
func (p *filterParser) parseValueOrList() (filterValue, error) {
	if p.peek().kind == tokLBracket {
		p.advance()
		var items []string
		for p.peek().kind != tokRBracket {
			v, _, err := p.parseValue()
			if err != nil {
				return filterValue{}, err
			}
			if v.isStr {
				items = append(items, v.str)
			} else {
				items = append(items, strconv.FormatFloat(v.num, 'f', -1, 64))
			}
			if p.peek().kind == tokComma {
				p.advance()
				continue
			}
			break
		}
		if p.peek().kind != tokRBracket {
			return filterValue{}, fmt.Errorf("missing ']' in filter")
		}
		p.advance()
		return filterValue{list: items, isList: true, isStr: true}, nil
	}
	v, _, err := p.parseValue()
	return v, err
}

// parsePredicateCall implements contains(field, value), startswith, endswith,
// and is_null(field).
func (p *filterParser) parsePredicateCall(name string) (filterValue, bool, error) {
	p.advance() // '('
	args := make([]filterValue, 0, 2)
	for p.peek().kind != tokRParen {
		v, _, err := p.parseValue()
		if err != nil {
			return filterValue{}, false, err
		}
		args = append(args, v)
		if p.peek().kind == tokComma {
			p.advance()
			continue
		}
		break
	}
	if p.peek().kind != tokRParen {
		return filterValue{}, false, fmt.Errorf("missing ')' after %s(", name)
	}
	p.advance()

	switch name {
	case "is_null":
		if len(args) != 1 {
			return filterValue{}, false, fmt.Errorf("is_null() takes 1 argument")
		}
		return filterValue{num: boolToFloat(args[0].field != "" && args[0].str == "")}, true, nil
	case "contains", "startswith", "endswith":
		if len(args) != 2 {
			return filterValue{}, false, fmt.Errorf("%s() takes 2 arguments", name)
		}
		needle := args[1].str
		if !args[1].isStr {
			needle = strconv.FormatFloat(args[1].num, 'f', -1, 64)
		}
		var ok bool
		switch {
		case name == "contains" && args[0].field == "tags":
			ok = p.sample != nil && p.sample.Contains("tags", needle)
		case name == "contains":
			ok = strings.Contains(args[0].str, needle)
		case name == "startswith":
			ok = strings.HasPrefix(args[0].str, needle)
		case name == "endswith":
			ok = strings.HasSuffix(args[0].str, needle)
		}
		return filterValue{num: boolToFloat(ok)}, true, nil
	default:
		return filterValue{}, false, fmt.Errorf("unknown filter function %q", name)
	}
}

func compareValues(left filterValue, leftNum bool, op string, right filterValue) (bool, error) {
	switch op {
	case "in":
		if !right.isList {
			return false, fmt.Errorf("'in' requires a bracketed list on the right-hand side")
		}
		if !left.isStr {
			left.str = strconv.FormatFloat(left.num, 'f', -1, 64)
		}
		for _, item := range right.list {
			if item == left.str {
				return true, nil
			}
		}
		return false, nil
	case "~=":
		if !left.isStr {
			left.str = strconv.FormatFloat(left.num, 'f', -1, 64)
		}
		re, err := compileFilterRegex(right.str)
		if err != nil {
			return false, err
		}
		return re.MatchString(left.str), nil
	}

	// Numeric comparison when both sides look numeric.
	if leftNum && !right.isStr {
		switch op {
		case "==", "=":
			return left.num == right.num, nil
		case "!=":
			return left.num != right.num, nil
		case ">":
			return left.num > right.num, nil
		case ">=":
			return left.num >= right.num, nil
		case "<":
			return left.num < right.num, nil
		case "<=":
			return left.num <= right.num, nil
		}
		return false, fmt.Errorf("operator %q cannot be applied to numbers", op)
	}

	// String comparison.
	ls := left.str
	if !left.isStr {
		ls = strconv.FormatFloat(left.num, 'f', -1, 64)
	}
	rs := right.str
	if !right.isStr {
		rs = strconv.FormatFloat(right.num, 'f', -1, 64)
	}
	switch op {
	case "==", "=":
		return ls == rs, nil
	case "!=":
		return ls != rs, nil
	case ">", ">=", "<", "<=":
		// Lexicographic ordering is rarely what a dataset author means, so it is
		// rejected rather than silently applied.
		return false, fmt.Errorf("ordering comparison %q is not supported for text fields", op)
	}
	return false, fmt.Errorf("unknown operator %q", op)
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
