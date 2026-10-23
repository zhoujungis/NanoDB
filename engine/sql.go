package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// SQLQuery 是一条只读 SQL 的解析结果。
type SQLQuery struct {
	Collection string
	Query      Query
	Count      bool
	// HasLimit 区分 "没有 LIMIT" 与 "LIMIT 0"。
	HasLimit bool
}

// SQLStore 是 SQL 只读查询需要的最小接口，*DB 与 *Tx 都满足。
type SQLStore interface {
	Find(coll string, q Query) ([]map[string]any, error)
	Count(coll string, filter map[string]any) (int, error)
}

// RunSQL 解析并执行一条只读 SQL，返回结果文档；
// COUNT(*) 返回单条 {"count": n}。
func RunSQL(store SQLStore, statement string) ([]map[string]any, error) {
	q, err := ParseSQL(statement)
	if err != nil {
		return nil, err
	}
	return ExecuteSQL(store, q)
}

// ExecuteSQL 执行已经解析好的只读 SQL。
func ExecuteSQL(store SQLStore, q SQLQuery) ([]map[string]any, error) {
	if q.Count {
		n, err := store.Count(q.Collection, q.Query.Filter)
		if err != nil {
			return nil, err
		}
		return []map[string]any{{"count": n}}, nil
	}
	if q.HasLimit && q.Query.Limit == 0 {
		return []map[string]any{}, nil
	}
	return store.Find(q.Collection, q.Query)
}

// QuerySQL 在数据库上执行只读 SQL。
func (db *DB) QuerySQL(statement string) ([]map[string]any, error) {
	return RunSQL(db, statement)
}

const (
	sqlEOF = iota
	sqlIdent
	sqlNumber
	sqlString
	sqlSymbol
)

type sqlToken struct {
	kind   int
	text   string
	num    float64
	quoted bool
}

type sqlLexer struct {
	src  []rune
	pos  int
	toks []sqlToken
}

func lexSQL(input string) ([]sqlToken, error) {
	l := &sqlLexer{src: []rune(input)}
	for {
		l.skipSpace()
		if l.pos >= len(l.src) {
			break
		}
		r := l.src[l.pos]
		switch {
		case isIdentStart(r):
			l.lexIdent()
		case r >= '0' && r <= '9':
			l.lexNumber()
		case r == '\'':
			if err := l.lexString(); err != nil {
				return nil, err
			}
		case r == '"':
			if err := l.lexQuotedIdent(); err != nil {
				return nil, err
			}
		default:
			l.lexSymbol()
		}
	}
	l.toks = append(l.toks, sqlToken{kind: sqlEOF})
	return l.toks, nil
}

func (l *sqlLexer) skipSpace() {
	for l.pos < len(l.src) {
		r := l.src[l.pos]
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			l.pos++
			continue
		}
		// -- 行注释
		if r == '-' && l.pos+1 < len(l.src) && l.src[l.pos+1] == '-' {
			for l.pos < len(l.src) && l.src[l.pos] != '\n' {
				l.pos++
			}
			continue
		}
		break
	}
}

func isIdentStart(r rune) bool {
	return r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
}

func isIdentPart(r rune) bool {
	return isIdentStart(r) || (r >= '0' && r <= '9')
}

func (l *sqlLexer) lexIdent() {
	start := l.pos
	for l.pos < len(l.src) && isIdentPart(l.src[l.pos]) {
		l.pos++
	}
	l.toks = append(l.toks, sqlToken{kind: sqlIdent, text: string(l.src[start:l.pos])})
}

func (l *sqlLexer) lexNumber() {
	start := l.pos
	for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
		l.pos++
	}
	if l.pos < len(l.src) && l.src[l.pos] == '.' {
		l.pos++
		for l.pos < len(l.src) && l.src[l.pos] >= '0' && l.src[l.pos] <= '9' {
			l.pos++
		}
	}
	raw := string(l.src[start:l.pos])
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		// 无法解析时保留原文，由语法层报错。
		l.toks = append(l.toks, sqlToken{kind: sqlIdent, text: raw})
		return
	}
	l.toks = append(l.toks, sqlToken{kind: sqlNumber, text: raw, num: f})
}

func (l *sqlLexer) lexString() error {
	l.pos++ // 跳过起始单引号
	var b strings.Builder
	for l.pos < len(l.src) {
		r := l.src[l.pos]
		if r == '\'' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '\'' {
				b.WriteRune('\'')
				l.pos += 2
				continue
			}
			l.pos++
			l.toks = append(l.toks, sqlToken{kind: sqlString, text: b.String()})
			return nil
		}
		b.WriteRune(r)
		l.pos++
	}
	return fmt.Errorf("%w: sql: unterminated string literal", ErrInvalid)
}

func (l *sqlLexer) lexQuotedIdent() error {
	l.pos++ // 跳过起始双引号
	var b strings.Builder
	for l.pos < len(l.src) {
		r := l.src[l.pos]
		if r == '"' {
			if l.pos+1 < len(l.src) && l.src[l.pos+1] == '"' {
				b.WriteRune('"')
				l.pos += 2
				continue
			}
			l.pos++
			l.toks = append(l.toks, sqlToken{kind: sqlIdent, text: b.String(), quoted: true})
			return nil
		}
		b.WriteRune(r)
		l.pos++
	}
	return fmt.Errorf("%w: sql: unterminated quoted identifier", ErrInvalid)
}

func (l *sqlLexer) lexSymbol() {
	two := ""
	if l.pos+1 < len(l.src) {
		two = string(l.src[l.pos : l.pos+2])
	}
	switch two {
	case ">=", "<=", "<>", "!=":
		l.toks = append(l.toks, sqlToken{kind: sqlSymbol, text: two})
		l.pos += 2
		return
	}
	l.toks = append(l.toks, sqlToken{kind: sqlSymbol, text: string(l.src[l.pos])})
	l.pos++
}

type sqlParser struct {
	toks []sqlToken
	pos  int
}

func (p *sqlParser) cur() sqlToken { return p.toks[p.pos] }

func (p *sqlParser) next() sqlToken {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *sqlParser) isKeyword(kw string) bool {
	t := p.cur()
	return t.kind == sqlIdent && !t.quoted && strings.EqualFold(t.text, kw)
}

func (p *sqlParser) acceptKeyword(kw string) bool {
	if p.isKeyword(kw) {
		p.next()
		return true
	}
	return false
}

func (p *sqlParser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return p.syntaxf("expected %s", kw)
	}
	return nil
}

func (p *sqlParser) isSymbol(s string) bool {
	t := p.cur()
	return t.kind == sqlSymbol && t.text == s
}

func (p *sqlParser) acceptSymbol(s string) bool {
	if p.isSymbol(s) {
		p.next()
		return true
	}
	return false
}

func (p *sqlParser) expectSymbol(s string) error {
	if !p.acceptSymbol(s) {
		return p.syntaxf("expected %q", s)
	}
	return nil
}

func (p *sqlParser) syntaxf(format string, args ...any) error {
	tok := p.cur()
	near := tok.text
	if tok.kind == sqlEOF {
		near = "end of statement"
	}
	return fmt.Errorf("%w: sql: %s near %q", ErrInvalid, fmt.Sprintf(format, args...), near)
}

// ParseSQL 解析只读 SELECT 语句。
func ParseSQL(statement string) (SQLQuery, error) {
	toks, err := lexSQL(statement)
	if err != nil {
		return SQLQuery{}, err
	}
	p := &sqlParser{toks: toks}
	q, err := p.parseSelect()
	if err != nil {
		return SQLQuery{}, err
	}
	if p.acceptSymbol(";") {
		// 允许结尾分号
	}
	if p.cur().kind != sqlEOF {
		return SQLQuery{}, p.syntaxf("unexpected token")
	}
	return q, nil
}

func (p *sqlParser) parseSelect() (SQLQuery, error) {
	var out SQLQuery
	if err := p.expectKeyword("SELECT"); err != nil {
		return out, err
	}
	if p.acceptSymbol("*") {
		// 全字段
	} else {
		for {
			if p.isKeyword("COUNT") {
				p.next()
				if err := p.expectSymbol("("); err != nil {
					return out, err
				}
				if err := p.expectSymbol("*"); err != nil {
					return out, err
				}
				if err := p.expectSymbol(")"); err != nil {
					return out, err
				}
				if out.Count || len(out.Query.Fields) > 0 {
					return out, p.syntaxf("COUNT(*) cannot be combined with other columns")
				}
				out.Count = true
			} else {
				if out.Count {
					return out, p.syntaxf("COUNT(*) cannot be combined with other columns")
				}
				field, err := p.parseFieldName()
				if err != nil {
					return out, err
				}
				if p.isKeyword("AS") {
					return out, p.syntaxf("column aliases are not supported")
				}
				out.Query.Fields = append(out.Query.Fields, field)
			}
			if !p.acceptSymbol(",") {
				break
			}
		}
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return out, err
	}
	coll, err := p.parseFieldName()
	if err != nil {
		return out, err
	}
	out.Collection = coll

	if p.acceptKeyword("WHERE") {
		filter, err := p.parseOrExpr()
		if err != nil {
			return out, err
		}
		out.Query.Filter = filter
	}
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return out, err
		}
		sort, err := p.parseOrderBy()
		if err != nil {
			return out, err
		}
		out.Query.Sort = sort
	}
	for {
		switch {
		case p.acceptKeyword("LIMIT"):
			n, err := p.parseNonNegativeInt()
			if err != nil {
				return out, err
			}
			out.Query.Limit = n
			out.HasLimit = true
		case p.acceptKeyword("OFFSET"):
			n, err := p.parseNonNegativeInt()
			if err != nil {
				return out, err
			}
			out.Query.Skip = n
		default:
			return out, nil
		}
	}
}

func (p *sqlParser) parseFieldName() (string, error) {
	tok := p.cur()
	if tok.kind != sqlIdent {
		return "", p.syntaxf("expected identifier")
	}
	name := tok.text
	p.next()
	for p.isSymbol(".") {
		p.next()
		seg := p.cur()
		if seg.kind != sqlIdent {
			return "", p.syntaxf("expected identifier after %q", ".")
		}
		name += "." + seg.text
		p.next()
	}
	return name, nil
}

func (p *sqlParser) parseOrderBy() ([]SortField, error) {
	var out []SortField
	for {
		field, err := p.parseFieldName()
		if err != nil {
			return nil, err
		}
		sf := SortField{Field: field}
		switch {
		case p.acceptKeyword("DESC"):
			sf.Desc = true
		case p.acceptKeyword("ASC"):
		}
		out = append(out, sf)
		if !p.acceptSymbol(",") {
			return out, nil
		}
	}
}

func (p *sqlParser) parseNonNegativeInt() (int, error) {
	tok := p.cur()
	if tok.kind != sqlNumber || tok.num < 0 || tok.num != float64(int(tok.num)) {
		return 0, p.syntaxf("expected non-negative integer")
	}
	p.next()
	return int(tok.num), nil
}

func (p *sqlParser) parseOrExpr() (map[string]any, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		left = combineSQLFilter("$or", left, right)
	}
	return left, nil
}

func (p *sqlParser) parseAndExpr() (map[string]any, error) {
	left, err := p.parseNotExpr()
	if err != nil {
		return nil, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parseNotExpr()
		if err != nil {
			return nil, err
		}
		left = combineSQLFilter("$and", left, right)
	}
	return left, nil
}

func combineSQLFilter(op string, left, right map[string]any) map[string]any {
	out := make(map[string]any)
	if existing, ok := left[op].([]any); ok && len(left) == 1 {
		out[op] = append(existing, right)
		return out
	}
	return map[string]any{op: []any{left, right}}
}

func (p *sqlParser) parseNotExpr() (map[string]any, error) {
	if p.acceptKeyword("NOT") {
		sub, err := p.parseNotExpr()
		if err != nil {
			return nil, err
		}
		return map[string]any{"$not": sub}, nil
	}
	if p.acceptSymbol("(") {
		sub, err := p.parseOrExpr()
		if err != nil {
			return nil, err
		}
		if err := p.expectSymbol(")"); err != nil {
			return nil, err
		}
		return sub, nil
	}
	return p.parseComparison()
}

func (p *sqlParser) parseComparison() (map[string]any, error) {
	field, err := p.parseFieldName()
	if err != nil {
		return nil, err
	}
	if p.acceptKeyword("IS") {
		negate := p.acceptKeyword("NOT")
		if err := p.expectKeyword("NULL"); err != nil {
			return nil, err
		}
		if negate {
			return fieldFilter(field, map[string]any{"$exists": true}), nil
		}
		return fieldFilter(field, map[string]any{"$eq": nil}), nil
	}
	negate := p.acceptKeyword("NOT")
	if p.acceptKeyword("IN") {
		items, err := p.parseValueList()
		if err != nil {
			return nil, err
		}
		if negate {
			return fieldFilter(field, map[string]any{"$nin": items}), nil
		}
		return fieldFilter(field, map[string]any{"$in": items}), nil
	}
	if p.acceptKeyword("LIKE") {
		tok := p.cur()
		if tok.kind != sqlString {
			return nil, p.syntaxf("LIKE requires a string literal")
		}
		p.next()
		pattern := likeToRegex(tok.text)
		if negate {
			return fieldFilter(field, map[string]any{"$not": map[string]any{"$regex": pattern}}), nil
		}
		return fieldFilter(field, map[string]any{"$regex": pattern}), nil
	}
	if p.acceptKeyword("BETWEEN") {
		lo, err := p.parseLiteral()
		if err != nil {
			return nil, err
		}
		if err := p.expectKeyword("AND"); err != nil {
			return nil, err
		}
		hi, err := p.parseLiteral()
		if err != nil {
			return nil, err
		}
		sub := map[string]any{"$and": []any{
			fieldFilter(field, map[string]any{"$gte": lo}),
			fieldFilter(field, map[string]any{"$lte": hi}),
		}}
		if negate {
			return map[string]any{"$not": sub}, nil
		}
		return sub, nil
	}
	if negate {
		return nil, p.syntaxf("expected IN, LIKE or BETWEEN after NOT")
	}
	tok := p.cur()
	if tok.kind != sqlSymbol {
		return nil, p.syntaxf("expected comparison operator")
	}
	var op string
	switch tok.text {
	case "=":
		op = "$eq"
	case "!=", "<>":
		op = "$ne"
	case ">":
		op = "$gt"
	case ">=":
		op = "$gte"
	case "<":
		op = "$lt"
	case "<=":
		op = "$lte"
	default:
		return nil, p.syntaxf("unsupported operator %q", tok.text)
	}
	p.next()
	val, err := p.parseLiteral()
	if err != nil {
		return nil, err
	}
	return fieldFilter(field, map[string]any{op: val}), nil
}

func fieldFilter(field string, cond map[string]any) map[string]any {
	return map[string]any{field: cond}
}

func (p *sqlParser) parseValueList() ([]any, error) {
	if err := p.expectSymbol("("); err != nil {
		return nil, err
	}
	out := []any{}
	for {
		v, err := p.parseLiteral()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		if !p.acceptSymbol(",") {
			break
		}
	}
	if err := p.expectSymbol(")"); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *sqlParser) parseLiteral() (any, error) {
	tok := p.cur()
	switch tok.kind {
	case sqlNumber:
		p.next()
		return tok.num, nil
	case sqlString:
		p.next()
		return tok.text, nil
	case sqlIdent:
		if !tok.quoted {
			switch strings.ToUpper(tok.text) {
			case "NULL":
				p.next()
				return nil, nil
			case "TRUE":
				p.next()
				return true, nil
			case "FALSE":
				p.next()
				return false, nil
			}
		}
	case sqlSymbol:
		if tok.text == "-" {
			p.next()
			num := p.cur()
			if num.kind != sqlNumber {
				return nil, p.syntaxf("expected number after %q", "-")
			}
			p.next()
			return -num.num, nil
		}
	}
	return nil, p.syntaxf("expected literal value")
}

// likeToRegex 把 SQL LIKE 模式转换成锚定的 Go 正则。
func likeToRegex(pattern string) string {
	var b strings.Builder
	b.WriteString("(?s)^")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String()
}
