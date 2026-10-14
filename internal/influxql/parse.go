// Package influxql implements the InfluxQL-subset query language on top of
// the query engine: a tokenizer, a recursive-descent parser, an executor and
// CSV/JSON result rendering.
//
//	SELECT <select_list> FROM <measurement>
//	    [WHERE <cond> (AND <cond>)*]
//	    [GROUP BY <group_item>, ...]
//	    [LIMIT <n>]
//
//	SHOW MEASUREMENTS
//
// The select list is "*", bare field names, or aggregates
// count|sum|mean|min|max(field); a query containing any aggregate runs in
// aggregation mode. WHERE accepts time comparisons (time > / >= / < / <=
// against an epoch integer, an RFC-3339 date string, or now() plus/minus
// durations such as 1h) and tag equality / inequality. GROUP BY accepts tag
// names and time(<duration>) buckets.
package influxql

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Error reports a syntax or semantic error in a query.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// --------------------------------------------------------------------------- //
// Tokenizer
// --------------------------------------------------------------------------- //

type tokenKind int

const (
	tokStr tokenKind = iota
	tokNum
	tokKw
	tokIdent
	tokPunct
)

type token struct {
	kind tokenKind
	val  string
}

var puncts = []string{"!=", ">=", "<=", "(", ")", ",", "=", ">", "<", "+", "-", "*"}

var keywords = map[string]bool{
	"select": true, "from": true, "where": true, "group": true, "by": true,
	"limit": true, "show": true, "measurements": true, "and": true,
	"time": true, "now": true,
}

func tokenize(text string) ([]token, error) {
	var toks []token
	i, n := 0, len(text)
	for i < n {
		r, size := utf8.DecodeRuneInString(text[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		if r == '\'' || r == '"' {
			quote := r
			i += size
			var buf strings.Builder
			for i < n {
				r2, s2 := utf8.DecodeRuneInString(text[i:])
				if r2 == quote {
					break
				}
				if r2 == '\\' && i+s2 < n {
					rr, ss := utf8.DecodeRuneInString(text[i+s2:])
					buf.WriteRune(rr)
					i += s2 + ss
					continue
				}
				buf.WriteRune(r2)
				i += s2
			}
			if i >= n {
				return nil, &Error{Msg: "unterminated string"}
			}
			toks = append(toks, token{tokStr, buf.String()})
			i += size // closing quote
			continue
		}
		if r >= '0' && r <= '9' {
			j := i
			for j < n && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			toks = append(toks, token{tokNum, text[i:j]})
			i = j
			continue
		}
		if unicode.IsLetter(r) || r == '_' {
			j := i + size
			for j < n {
				r2, s2 := utf8.DecodeRuneInString(text[j:])
				if !unicode.IsLetter(r2) && !unicode.IsDigit(r2) && r2 != '_' && r2 != '-' {
					break
				}
				j += s2
			}
			word := text[i:j]
			low := strings.ToLower(word)
			if keywords[low] {
				toks = append(toks, token{tokKw, low})
			} else {
				toks = append(toks, token{tokIdent, word})
			}
			i = j
			continue
		}
		matched := false
		for _, p := range puncts {
			if strings.HasPrefix(text[i:], p) {
				toks = append(toks, token{tokPunct, p})
				i += len(p)
				matched = true
				break
			}
		}
		if !matched {
			return nil, errf("unexpected character %q", r)
		}
	}
	return toks, nil
}

// --------------------------------------------------------------------------- //
// AST
// --------------------------------------------------------------------------- //

// SelectItem is one entry of a SELECT list.
type SelectItem struct {
	Kind  string // "star", "field" or "agg"
	Func  string // aggregate function (agg only)
	Field string // field name (field and agg)
}

// SelectQuery is a parsed SELECT statement.
type SelectQuery struct {
	Measurement string
	Items       []SelectItem
	Start, End  *int64            // inclusive ns bounds (nil = unbounded)
	TagEq       map[string]string // tag equality filters (intersected)
	TagNeq      map[string]string // tag inequality filters
	GroupTags   []string          // GROUP BY tag names
	TimeBucket  int64             // GROUP BY time() bucket in ns; 0 = none
	Limit       int               // 0 = none
}

// ShowQuery is a parsed SHOW MEASUREMENTS statement.
type ShowQuery struct{}

// Query is either *SelectQuery or *ShowQuery.
type Query interface{ isQuery() }

func (*SelectQuery) isQuery() {}
func (*ShowQuery) isQuery()   {}

// --------------------------------------------------------------------------- //
// Parser
// --------------------------------------------------------------------------- //

var aggFuncs = map[string]bool{"count": true, "sum": true, "mean": true, "min": true, "max": true}

// Durations tokenize as <num><unit-ident> (e.g. 1h); units in nanoseconds.
var durNs = map[string]int64{
	"ns": 1, "us": 1000, "ms": 1000 * 1000, "s": 1000 * 1000 * 1000,
	"m": 60 * 1000 * 1000 * 1000, "h": 3600 * 1000 * 1000 * 1000,
	"d": 86400 * 1000 * 1000 * 1000, "w": 7 * 86400 * 1000 * 1000 * 1000,
}

type parser struct {
	toks  []token
	pos   int
	nowNs int64
}

// ParseQuery parses one InfluxQL-subset statement. nowNs == 0 uses the
// current time (injectable for tests).
func ParseQuery(sql string, nowNs int64) (Query, error) {
	toks, err := tokenize(strings.TrimSpace(sql))
	if err != nil {
		return nil, err
	}
	if len(toks) == 0 {
		return nil, &Error{Msg: "empty query"}
	}
	if nowNs == 0 {
		nowNs = time.Now().UnixNano()
	}
	return (&parser{toks: toks, nowNs: nowNs}).parse()
}

func (p *parser) peek() (token, bool) {
	if p.pos < len(p.toks) {
		return p.toks[p.pos], true
	}
	return token{}, false
}

func (p *parser) next() (token, error) {
	t, ok := p.peek()
	if !ok {
		return t, &Error{Msg: "unexpected end of query"}
	}
	p.pos++
	return t, nil
}

func (p *parser) expectPunct(v string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokPunct || t.val != v {
		return errf("expected %q after %q", v, t.val)
	}
	return nil
}

func (p *parser) expectKw(kw string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tokKw || t.val != kw {
		return errf("expected keyword %s", kw)
	}
	return nil
}

func (p *parser) eof() error {
	if _, ok := p.peek(); ok {
		return errf("trailing tokens %v", p.toks[p.pos:])
	}
	return nil
}

func (p *parser) parse() (Query, error) {
	if t, _ := p.peek(); t.kind == tokKw && t.val == "show" {
		p.next()
		if err := p.expectKw("measurements"); err != nil {
			return nil, err
		}
		if err := p.eof(); err != nil {
			return nil, err
		}
		return &ShowQuery{}, nil
	}
	if err := p.expectKw("select"); err != nil {
		return nil, err
	}
	q := &SelectQuery{TagEq: map[string]string{}, TagNeq: map[string]string{}}
	// Select list.
	if t, _ := p.peek(); t.kind == tokPunct && t.val == "*" {
		p.next()
		q.Items = append(q.Items, SelectItem{Kind: "star"})
	} else {
		for {
			t, err := p.next()
			if err != nil {
				return nil, err
			}
			if t.kind != tokIdent {
				return nil, errf("expected field or aggregate, got %q", t.val)
			}
			nxt, _ := p.peek()
			if nxt.kind == tokPunct && nxt.val == "(" {
				// Aggregate: func(field) -- count() may omit the field.
				if !aggFuncs[t.val] {
					return nil, errf("unknown function %q", t.val)
				}
				p.next() // (
				fname := ""
				if inner, _ := p.peek(); !(inner.kind == tokPunct && inner.val == ")") {
					ft, err := p.next()
					if err != nil {
						return nil, err
					}
					if ft.kind != tokIdent {
						return nil, errf("expected field name in %s()", t.val)
					}
					fname = ft.val
				}
				if err := p.expectPunct(")"); err != nil {
					return nil, err
				}
				q.Items = append(q.Items, SelectItem{Kind: "agg", Func: t.val, Field: fname})
			} else {
				q.Items = append(q.Items, SelectItem{Kind: "field", Field: t.val})
			}
			if nxt, _ := p.peek(); nxt.kind == tokPunct && nxt.val == "," {
				p.next()
				continue
			}
			break
		}
	}
	// FROM measurement.
	if err := p.expectKw("from"); err != nil {
		return nil, err
	}
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	if t.kind != tokIdent {
		return nil, errf("expected measurement name, got %q", t.val)
	}
	q.Measurement = t.val
	// Optional WHERE / GROUP BY / LIMIT (WHERE first per InfluxQL).
	if t, _ := p.peek(); t.kind == tokKw && t.val == "where" {
		p.next()
		for {
			if err := p.parseCond(q); err != nil {
				return nil, err
			}
			if t, _ := p.peek(); t.kind == tokKw && t.val == "and" {
				p.next()
				continue
			}
			break
		}
	}
	if t, _ := p.peek(); t.kind == tokKw && t.val == "group" {
		p.next()
		if err := p.expectKw("by"); err != nil {
			return nil, err
		}
		for {
			t, err := p.next()
			if err != nil {
				return nil, err
			}
			switch {
			case t.kind == tokIdent:
				q.GroupTags = append(q.GroupTags, t.val)
			case t.kind == tokKw && t.val == "time":
				if err := p.expectPunct("("); err != nil {
					return nil, err
				}
				bucket, err := p.readDuration()
				if err != nil {
					return nil, err
				}
				q.TimeBucket = bucket
				if err := p.expectPunct(")"); err != nil {
					return nil, err
				}
			default:
				return nil, errf("bad GROUP BY item %q", t.val)
			}
			if t, _ := p.peek(); t.kind == tokPunct && t.val == "," {
				p.next()
				continue
			}
			break
		}
	}
	if t, _ := p.peek(); t.kind == tokKw && t.val == "limit" {
		p.next()
		t, err := p.next()
		if err != nil {
			return nil, err
		}
		if t.kind != tokNum {
			return nil, &Error{Msg: "expected integer after LIMIT"}
		}
		n, cerr := strconv.Atoi(t.val)
		if cerr != nil {
			return nil, &Error{Msg: "expected integer after LIMIT"}
		}
		q.Limit = n
	}
	if err := p.eof(); err != nil {
		return nil, err
	}
	return q, nil
}

// parseCond parses one WHERE condition into q.
func (p *parser) parseCond(q *SelectQuery) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind == tokKw && t.val == "time" {
		op, err := p.next()
		if err != nil {
			return err
		}
		if op.kind != tokPunct || (op.val != ">" && op.val != ">=" && op.val != "<" && op.val != "<=") {
			return errf("bad time comparison %q", op.val)
		}
		ts, err := p.parseTimeExpr()
		if err != nil {
			return err
		}
		// Strict inequalities over the integer-ns domain: shift by one tick.
		switch op.val {
		case ">":
			lo := ts + 1
			if q.Start == nil || lo > *q.Start {
				q.Start = &lo
			}
		case ">=":
			lo := ts
			if q.Start == nil || lo > *q.Start {
				q.Start = &lo
			}
		case "<":
			hi := ts - 1
			if q.End == nil || hi < *q.End {
				q.End = &hi
			}
		default: // "<="
			hi := ts
			if q.End == nil || hi < *q.End {
				q.End = &hi
			}
		}
		return nil
	}
	if t.kind == tokIdent {
		op, err := p.next()
		if err != nil {
			return err
		}
		if op.kind != tokPunct || (op.val != "=" && op.val != "!=") {
			return &Error{Msg: "expected = or != after tag name"}
		}
		s, err := p.next()
		if err != nil {
			return err
		}
		if s.kind != tokStr {
			return &Error{Msg: "tag value must be a string literal"}
		}
		if op.val == "=" {
			q.TagEq[t.val] = s.val
		} else {
			q.TagNeq[t.val] = s.val
		}
		return nil
	}
	return errf("expected time or tag condition, got %q", t.val)
}

// parseTimeExpr parses an epoch integer, an RFC-3339 date string, or
// "now()" optionally followed by +/- duration adjustments.
func (p *parser) parseTimeExpr() (int64, error) {
	t, err := p.next()
	if err != nil {
		return 0, err
	}
	if t.kind == tokNum {
		return strconv.ParseInt(t.val, 10, 64)
	}
	if t.kind == tokStr {
		return parseDateNs(t.val)
	}
	if t.kind == tokKw && t.val == "now" {
		if err := p.expectPunct("("); err != nil {
			return 0, err
		}
		if err := p.expectPunct(")"); err != nil {
			return 0, err
		}
		base := p.nowNs
		for {
			nxt, ok := p.peek()
			if !ok || nxt.kind != tokPunct || (nxt.val != "+" && nxt.val != "-") {
				break
			}
			p.next()
			d, err := p.readDuration()
			if err != nil {
				return 0, err
			}
			if nxt.val == "+" {
				base += d
			} else {
				base -= d
			}
		}
		return base, nil
	}
	return 0, errf("expected time expression, got %q", t.val)
}

// readDuration consumes "<num> <unit>" (e.g. 1h) and returns nanoseconds.
// A bare number is nanoseconds (e.g. time(100)).
func (p *parser) readDuration() (int64, error) {
	t, err := p.next()
	if err != nil {
		return 0, err
	}
	if t.kind != tokNum {
		return 0, &Error{Msg: "expected duration like 1h"}
	}
	unit := ""
	if nxt, ok := p.peek(); ok && nxt.kind == tokIdent && len(nxt.val) <= 2 {
		p.next()
		unit = nxt.val
	}
	if unit == "" {
		unit = "ns"
	}
	mult, ok := durNs[unit]
	if !ok {
		return 0, errf("bad duration unit %q", unit)
	}
	num, err := strconv.ParseInt(t.val, 10, 64)
	if err != nil {
		return 0, &Error{Msg: "expected duration like 1h"}
	}
	return num * mult, nil
}

// dateLayouts covers the RFC-3339 shapes Python's datetime.fromisoformat
// accepts (naive strings are interpreted as UTC).
var dateLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

func parseDateNs(s string) (int64, error) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()*1e9 + int64(t.Nanosecond()), nil
		}
	}
	return 0, errf("bad date string %q", s)
}
