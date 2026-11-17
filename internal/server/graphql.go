// Package server — GraphQL layer (graphql.go).
//
// Ported from graphql.ts (723 lines; map §6): hand-written tokenizer +
// recursive-descent parser + switch-based executor for the fixed set of
// documents the suite sends, plus the introspection tables. No GraphQL
// library (there is none in stdlib, and the Node parser is hand-rolled);
// its quirks and error messages are replicated, including:
//   - tokenize() silently skips any character it does not recognise;
//   - numbers are [0-9-]-started, [0-9.]-continued, no exponents;
//   - named fragment spreads are consumed as unconditional inline fragments;
//   - only the first operation in a document is parsed;
//   - addComment-on-PR allocates a comment id and returns a commentEdge
//     but stores the comment nowhere.
//
// JSON field order: Node's JSON.stringify preserves JS object insertion
// order. encoding/json on map[string]any would sort keys alphabetically,
// so every result object built here is an *orderedObj (insertion-ordered,
// custom Marshaler). Values that come from the store (RepoView, IssueView,
// PullView) are structs whose field order already matches store.ts.
//
// Wiring: server.go's router currently stubs /api/graphql. The parent
// wires it with either HandleGraphQL (full route handler, Handler
// signature) or ExecuteGraphQL (per-document executor) — see below.
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"forge/internal/store"
)

// ---------------------------------------------------------------------------
// Insertion-ordered JSON object + JS-flavoured coercion helpers
// (shared by graphql.go and mcp.go)
// ---------------------------------------------------------------------------

// orderedObj is a JSON object that preserves insertion order when
// marshalled (Node's JSON.stringify order). Set on an existing key
// replaces the value in place (JS assignment keeps the first position).
type orderedObj struct {
	keys []string
	vals map[string]any
}

func newOrderedObj() *orderedObj {
	return &orderedObj{vals: map[string]any{}}
}

func (o *orderedObj) set(k string, v any) *orderedObj {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
	return o
}

func (o *orderedObj) get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// MarshalJSON writes {"k":v,...} in insertion order, without HTML
// escaping (<, >, & stay literal, as in JSON.stringify). NaN/±Inf
// floats (JS Number() of a malformed numeric token) serialise as null,
// as JSON.stringify does.
func (o *orderedObj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := marshalJSONValue(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalJSONValue(o.vals[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalJSONValue serialises v with encoding/json but without HTML
// escaping, matching JSON.stringify. NaN/±Inf become null.
func marshalJSONValue(v any) ([]byte, error) {
	if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
		return []byte("null"), nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// jsString mirrors JS String(v) for the argument types the executor and
// MCP tools coerce (string/bool/number/null/object/array). Inputs the
// suite sends are strings; the rest only matters for fidelity.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return jsNumberToString(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return t.String()
		}
		return jsNumberToString(f)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any, *orderedObj:
		return "[object Object]"
	default:
		return fmt.Sprint(v)
	}
}

// jsNumberToString mirrors JS Number→String for ordinary values:
// integers in decimal, small/large magnitudes in exponent form.
func jsNumberToString(f float64) string {
	if math.IsNaN(f) {
		return "NaN"
	}
	if math.IsInf(f, 1) {
		return "Infinity"
	}
	if math.IsInf(f, -1) {
		return "-Infinity"
	}
	if f == 0 {
		return "0"
	}
	af := math.Abs(f)
	if af >= 1e21 || af < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Go gives "1e+21"; JS gives "1e+21". Go gives "1e-07"; JS "1e-7".
		if i := strings.IndexByte(s, 'e'); i >= 0 {
			mant, exp := s[:i], s[i+1:]
			exp = strings.TrimPrefix(exp, "+")
			exp = strings.TrimPrefix(exp, "0")
			if exp == "" || exp == "-" {
				exp += "0"
			}
			return mant + "e" + exp
		}
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsNumber mirrors JS Number(v): (value, isNaN). The suite sends JSON
// numbers for numeric args; the string/bool/object branches exist for
// fidelity with mcp.ts's Number(args.pull_number) / String(...) calls.
func jsNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case nil:
		return 0, false
	case bool:
		if t {
			return 1, false
		}
		return 0, false
	case float64:
		return t, math.IsNaN(t)
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return math.NaN(), true
		}
		return f, math.IsNaN(f)
	case int:
		return float64(t), false
	case int64:
		return float64(t), false
	case string:
		return jsNumberFromString(t)
	case []any:
		if len(t) == 0 {
			return 0, false
		}
		if len(t) == 1 {
			return jsNumber(t[0])
		}
		return math.NaN(), true
	default:
		return math.NaN(), true
	}
}

func jsNumberFromString(s string) (float64, bool) {
	t := strings.Trim(s, " \t\n\v\f\r               　\ufeff")
	if t == "" {
		return 0, false
	}
	if t == "Infinity" || t == "+Infinity" {
		return math.Inf(1), false
	}
	if t == "-Infinity" {
		return math.Inf(-1), false
	}
	// Hex / binary / octal literals.
	if len(t) > 2 && t[0] == '0' && (t[1] == 'x' || t[1] == 'X') {
		if n, err := strconv.ParseInt(t[2:], 16, 64); err == nil {
			return float64(n), false
		}
		return math.NaN(), true
	}
	if len(t) > 2 && t[0] == '0' && (t[1] == 'b' || t[1] == 'B') {
		if n, err := strconv.ParseInt(t[2:], 2, 64); err == nil {
			return float64(n), false
		}
		return math.NaN(), true
	}
	if len(t) > 2 && t[0] == '0' && (t[1] == 'o' || t[1] == 'O') {
		if n, err := strconv.ParseInt(t[2:], 8, 64); err == nil {
			return float64(n), false
		}
		return math.NaN(), true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return math.NaN(), true
	}
	return f, math.IsNaN(f)
}

// jsParseInt mirrors JS parseInt(s, 10): leading whitespace, optional
// sign, then decimal digits; no digits → NaN (ok=false).
func jsParseInt(s string) (float64, bool) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r' || s[i] == '\v' || s[i] == '\f') {
		i++
	}
	neg := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	start := i
	var n float64
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		n = n*10 + float64(s[i]-'0')
		i++
	}
	if i == start {
		return math.NaN(), false
	}
	if neg {
		n = -n
	}
	return n, true
}

// numericValue reports whether v is a JS number (typeof === 'number'):
// float64 from the tokenizer, json.Number / int from a decoded JSON body.
func numericValue(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// strOrEmpty mirrors JS `String(v ?? ”)`: null/undefined → ”, else String(v).
func strOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return jsString(v)
}

// jsSliceEnd mirrors JS arr.slice(0, n) length: n>=0 → min(n, len),
// n<0 → max(0, len+n).
func jsSliceLen(length, n int) int {
	if n >= 0 {
		if n > length {
			return length
		}
		return n
	}
	if length+n < 0 {
		return 0
	}
	return length + n
}

// ---------------------------------------------------------------------------
// Tokenizer (graphql.ts:21-112)
// ---------------------------------------------------------------------------

type gqlTokenKind string

const (
	tokName   gqlTokenKind = "name"
	tokPunct  gqlTokenKind = "punct"
	tokString gqlTokenKind = "string"
	tokNumber gqlTokenKind = "number"
	tokVar    gqlTokenKind = "var"
	tokEOF    gqlTokenKind = "eof"
)

type gqlToken struct {
	kind  gqlTokenKind
	value string
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func tokenize(src string) []gqlToken {
	var tokens []gqlToken
	i := 0
	for i < len(src) {
		ch := src[i]
		if isSpaceByte(ch) || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '$' {
			j := i + 1
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokVar, value: src[i+1 : j]})
			i = j
			continue
		}
		if ch == '"' {
			j := i + 1
			var out strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					if esc == 'u' && j+6 <= len(src) && isHex4(src[j+2:j+6]) {
						cp, _ := strconv.ParseInt(src[j+2:j+6], 16, 32)
						r := rune(cp)
						// Combine a surrogate pair into one rune, as JS
						// holds the pair and JSON.stringify emits the
						// combined character.
						if cp >= 0xD800 && cp <= 0xDBFF && j+12 <= len(src) &&
							src[j+6] == '\\' && src[j+7] == 'u' && isHex4(src[j+8:j+12]) {
							lo, _ := strconv.ParseInt(src[j+8:j+12], 16, 32)
							if lo >= 0xDC00 && lo <= 0xDFFF {
								r = 0x10000 + (rune(cp-0xD800) << 10) + rune(lo-0xDC00)
								out.WriteRune(r)
								j += 12
								continue
							}
						}
						out.WriteRune(r)
						j += 6
						continue
					}
					switch esc {
					case '"':
						out.WriteByte('"')
					case '\\':
						out.WriteByte('\\')
					case '/':
						out.WriteByte('/')
					case 'b':
						out.WriteByte('\b')
					case 'f':
						out.WriteByte('\f')
					case 'n':
						out.WriteByte('\n')
					case 'r':
						out.WriteByte('\r')
					case 't':
						out.WriteByte('\t')
					default:
						out.WriteByte(esc)
					}
					j += 2
					continue
				}
				out.WriteByte(src[j])
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokString, value: out.String()})
			i = j + 1
			continue
		}
		if (ch >= '0' && ch <= '9') || ch == '-' {
			j := i
			for j < len(src) && ((src[j] >= '0' && src[j] <= '9') || src[j] == '.') {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokNumber, value: src[i:j]})
			i = j
			continue
		}
		if isNameStart(ch) {
			j := i
			for j < len(src) && isNameChar(src[j]) {
				j++
			}
			tokens = append(tokens, gqlToken{kind: tokName, value: src[i:j]})
			i = j
			continue
		}
		if strings.IndexByte("{}()[]:!@=|&.", ch) >= 0 {
			if ch == '.' && strings.HasPrefix(src[i:], "...") {
				tokens = append(tokens, gqlToken{kind: tokPunct, value: "..."})
				i += 3
				continue
			}
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(ch)})
			i++
			continue
		}
		// Any other character is silently skipped (graphql.ts).
		i++
	}
	tokens = append(tokens, gqlToken{kind: tokEOF, value: ""})
	return tokens
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Parser (graphql.ts:114-262)
// ---------------------------------------------------------------------------

// gqlEnum is a bare-name value (GraphQL enum literal): parseValue returns
// it for any name that is not true/false/null.
type gqlEnum string

type gqlSelection struct {
	name         string
	alias        *string
	args         map[string]any
	children     []*gqlSelection // nil = no selection set
	fragmentType *string         // only for name == "__fragment"; nil = null
}

func gqlKey(sel *gqlSelection) string {
	if sel.alias != nil {
		return *sel.alias
	}
	return sel.name
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
	vars   map[string]any
}

func newGqlParser(src string, variables map[string]any) *gqlParser {
	return &gqlParser{tokens: tokenize(src), vars: variables}
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken {
	t := p.tokens[p.pos]
	p.pos++
	return t
}

func (p *gqlParser) expectPunct(v string) error {
	t := p.next()
	if t.kind != tokPunct || t.value != v {
		return fmt.Errorf("expected %s, got %s", v, t.value)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("expected name, got %s", t.value)
	}
	return t.value, nil
}

func (p *gqlParser) parseValue() (any, error) {
	t := p.next()
	switch t.kind {
	case tokString:
		return t.value, nil
	case tokNumber:
		f, err := strconv.ParseFloat(t.value, 64)
		if err != nil {
			return math.NaN(), nil
		}
		return f, nil
	case tokVar:
		if p.vars != nil {
			if v, ok := p.vars[t.value]; ok && v != nil {
				return v, nil
			}
		}
		return nil, nil
	case tokName:
		switch t.value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		}
		return gqlEnum(t.value), nil
	case tokPunct:
		if t.value == "[" {
			var arr []any
			for !(p.peek().kind == tokPunct && p.peek().value == "]") {
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			if arr == nil {
				arr = []any{}
			}
			return arr, nil
		}
		if t.value == "{" {
			obj := map[string]any{}
			for !(p.peek().kind == tokPunct && p.peek().value == "}") {
				key, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				v, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				obj[key] = v
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return obj, nil
		}
	}
	return nil, fmt.Errorf("unexpected token %s", t.value)
}

func (p *gqlParser) parseArgs() (map[string]any, error) {
	args := map[string]any{}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for !(p.peek().kind == tokPunct && p.peek().value == ")") {
		key, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[key] = v
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for !(p.peek().kind == tokPunct && p.peek().value == "}") {
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	t := p.peek()
	if t.kind == tokPunct && t.value == "..." {
		p.next()
		var fragmentType *string
		nt := p.peek()
		if nt.kind == tokName && nt.value == "on" {
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			fragmentType = &name
		} else if nt.kind == tokName {
			// Named fragment spread — the suite does not use these;
			// consumed as an unconditional inline fragment.
			p.next()
		}
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{name: "__fragment", children: children, fragmentType: fragmentType}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	var alias *string
	name := first
	t = p.peek()
	if t.kind == tokPunct && t.value == ":" {
		p.next()
		alias = &first
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args := map[string]any{}
	t = p.peek()
	if t.kind == tokPunct && t.value == "(" {
		args, err = p.parseArgs()
		if err != nil {
			return nil, err
		}
	}
	var children []*gqlSelection
	t = p.peek()
	if t.kind == tokPunct && t.value == "{" {
		children, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &gqlSelection{name: name, alias: alias, args: args, children: children}, nil
}

// parseOperation returns ("query"|"mutation", selections). Only the first
// operation in the document is parsed; a second is ignored.
func (p *gqlParser) parseOperation() (string, []*gqlSelection, error) {
	operation := "query"
	t := p.peek()
	if t.kind == tokName && (t.value == "query" || t.value == "mutation") {
		operation = t.value
		p.next()
		t = p.peek()
		if t.kind == tokName {
			p.next() // operation name
		}
		// Variable definitions: skipped by paren-depth counting,
		// contents never validated.
		t = p.peek()
		if t.kind == tokPunct && t.value == "(" {
			depth := 0
			for {
				tok := p.next()
				if tok.kind == tokPunct && tok.value == "(" {
					depth++
				}
				if tok.kind == tokPunct && tok.value == ")" {
					depth--
					if depth == 0 {
						break
					}
				}
				if tok.kind == tokEOF {
					break
				}
			}
		}
	}
	sels, err := p.parseSelectionSet()
	if err != nil {
		return "", nil, err
	}
	return operation, sels, nil
}

// ---------------------------------------------------------------------------
// Introspection tables (graphql.ts:265-291)
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":            {"id", "number", "title", "state", "body", "author", "createdAt", "updatedAt", "repository"},
	"PullRequest":      {"id", "number", "title", "state", "merged", "mergeable", "headRefName", "baseRefName", "headRef", "baseRef", "files", "author", "createdAt", "updatedAt", "repository"},
	"Repository":       {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "createdAt", "updatedAt"},
	"SearchType":       {},
	"IssueState":       {},
	"PullRequestState": {},
}

var gqlTypeEnumValues = map[string][]string{
	"SearchType":       {"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER"},
	"IssueState":       {"OPEN", "CLOSED"},
	"PullRequestState": {"OPEN", "CLOSED", "MERGED"},
	"IssueOrderField":  {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"OrderDirection":   {"ASC", "DESC"},
	"MergeMethod":      {"MERGE", "SQUASH", "REBASE"},
	"MergeStateStatus": {"BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "MERGEABLE", "UNKNOWN", "UNSTABLE"},
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

// gqlError is a field-level throw: caught per top-level field by
// executeQuery/executeMutation, which append it to errors and set that
// field's data to null. Any other panic propagates (and becomes a 500
// via handleRequestSafe), as in app.ts's handler try/catch.
type gqlError struct{ msg string }

func gqlThrow(format string, args ...any) {
	panic(gqlError{msg: fmt.Sprintf(format, args...)})
}

// enumValue mirrors graphql.ts enumValue(): an __enum literal or a plain
// string; anything else → ("", false).
func enumValue(v any) (string, bool) {
	switch t := v.(type) {
	case gqlEnum:
		return string(t), true
	case string:
		return t, true
	}
	return "", false
}

var cursorRe = regexp.MustCompile(`^cursor:(\d+)$`)

func decodeCursor(cursor any) *int {
	s, ok := cursor.(string)
	if !ok {
		return nil
	}
	// Node's Buffer.from(x, 'base64') is lenient; the suite only ever
	// sends well-formed cursors produced by encodeCursor.
	text, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		text, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return nil
		}
	}
	m := cursorRe.FindStringSubmatch(string(text))
	if m == nil {
		return nil
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	n := int(f)
	return &n
}

func encodeCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

func gqlErrObj(msg string) *orderedObj {
	return newOrderedObj().set("message", msg)
}

// argInt returns the integer value of a numeric arg, or (0, false) when
// the arg is absent or not a number. Floats truncate toward zero, as JS
// slice()/Map-key behaviour requires per call site.
func argInt(args map[string]any, key string) (int, bool) {
	f, ok := numericValue(args[key])
	if !ok {
		return 0, false
	}
	return int(f), true
}

// argIntExact is argInt but only when the value is integral (for Map
// lookups: issue/pull numbers — 2.5 never matches an int key).
func argIntExact(args map[string]any, key string) (int, bool) {
	f, ok := numericValue(args[key])
	if !ok || math.Trunc(f) != f {
		return 0, false
	}
	return int(f), true
}

func nodeChildren(sel *gqlSelection, field string) []*gqlSelection {
	for _, c := range sel.children {
		if c.name == field {
			return c.children
		}
	}
	return nil
}

func repositoryNode(s *store.Store, repo *store.Repo, sels []*gqlSelection) *orderedObj {
	out := newOrderedObj()
	for _, sel := range sels {
		if sel.name == "__fragment" {
			if sel.fragmentType != nil && *sel.fragmentType != "Repository" {
				continue
			}
			merged := repositoryNode(s, repo, sel.children)
			for _, k := range merged.keys {
				out.set(k, merged.vals[k])
			}
			continue
		}
		key := gqlKey(sel)
		switch sel.name {
		case "id":
			out.set(key, store.GlobalID("Repository", repo.ID))
		case "databaseId":
			out.set(key, repo.ID)
		case "name":
			out.set(key, repo.Name)
		case "nameWithOwner":
			out.set(key, repo.Owner+"/"+repo.Name)
		case "owner":
			out.set(key, newOrderedObj().set("login", repo.Owner))
		case "defaultBranchRef":
			if ref, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok && ref != "" {
				out.set(key, newOrderedObj().set("name", repo.DefaultBranch))
			} else {
				out.set(key, nil)
			}
		case "viewerPermission":
			out.set(key, "ADMIN")
		case "hasIssuesEnabled":
			out.set(key, true)
		case "issue":
			n, ok := argIntExact(sel.args, "number")
			var issue *store.Issue
			if ok {
				issue = repo.Issues[n]
			}
			if issue != nil {
				out.set(key, issueNode(s, repo, issue, sel.children))
			} else {
				out.set(key, nil)
			}
		case "issues":
			out.set(key, issueConnection(s, repo, sel))
		case "pullRequests":
			out.set(key, pullConnection(s, repo, sel))
		case "pullRequest":
			n, ok := argIntExact(sel.args, "number")
			var pull *store.Pull
			if ok {
				pull = repo.Pulls[n]
			}
			if pull != nil {
				out.set(key, pullNode(s, repo, pull, sel.children))
			} else {
				out.set(key, nil)
			}
		case "createdAt", "updatedAt":
			out.set(key, store.FixedISO)
		default:
			gqlThrow("unknown field Repository.%s", sel.name)
		}
	}
	return out
}

func issueConnection(s *store.Store, repo *store.Repo, sel *gqlSelection) *orderedObj {
	first := 100
	if f, ok := argInt(sel.args, "first"); ok {
		first = f
	}
	after := decodeCursor(sel.args["after"])
	states := map[string]bool{}
	if arr, ok := sel.args["states"].([]any); ok {
		for _, e := range arr {
			if v, ok := enumValue(e); ok {
				states[v] = true
			}
		}
	}
	direction := "DESC"
	if ob, ok := sel.args["orderBy"].(map[string]any); ok {
		if v, ok := enumValue(ob["direction"]); ok {
			direction = v
		}
	}
	issues := make([]*store.Issue, 0, len(repo.Issues))
	for _, iss := range repo.Issues {
		issues = append(issues, iss)
	}
	if len(states) > 0 {
		filtered := issues[:0]
		for _, iss := range issues {
			if states[strings.ToUpper(iss.State)] {
				filtered = append(filtered, iss)
			}
		}
		issues = filtered
	}
	sort.Slice(issues, func(i, j int) bool {
		if direction == "ASC" {
			return issues[i].Number < issues[j].Number
		}
		return issues[i].Number > issues[j].Number
	})
	if after != nil {
		filtered := issues[:0]
		for _, iss := range issues {
			if direction == "ASC" {
				if iss.Number > *after {
					filtered = append(filtered, iss)
				}
			} else if iss.Number < *after {
				filtered = append(filtered, iss)
			}
		}
		issues = filtered
	}
	pageLen := jsSliceLen(len(issues), first)
	page := issues[:pageLen]
	hasNext := len(issues) > first
	nodes := make([]any, 0, len(page))
	for _, iss := range page {
		nodes = append(nodes, issueNode(s, repo, iss, nodeChildren(sel, "nodes")))
	}
	out := newOrderedObj()
	for _, child := range sel.children {
		key := gqlKey(child)
		switch child.name {
		case "nodes":
			out.set(key, nodes)
		case "edges":
			nodeSels := nodeChildren(child, "node")
			edges := make([]any, 0, len(page))
			for _, iss := range page {
				edges = append(edges, newOrderedObj().
					set("node", issueNode(s, repo, iss, nodeSels)).
					set("cursor", encodeCursor(iss.Number)))
			}
			out.set(key, edges)
		case "pageInfo":
			var startCursor, endCursor any
			if len(page) > 0 {
				startCursor = encodeCursor(page[0].Number)
				endCursor = encodeCursor(page[len(page)-1].Number)
			}
			out.set(key, newOrderedObj().
				set("hasNextPage", hasNext).
				set("hasPreviousPage", after != nil).
				set("startCursor", startCursor).
				set("endCursor", endCursor))
		case "totalCount":
			out.set(key, len(issues))
		}
	}
	return out
}

func pullConnection(s *store.Store, repo *store.Repo, sel *gqlSelection) *orderedObj {
	first := 100
	if f, ok := argInt(sel.args, "first"); ok {
		first = f
	}
	after := decodeCursor(sel.args["after"])
	states := map[string]bool{}
	if arr, ok := sel.args["states"].([]any); ok {
		for _, e := range arr {
			if v, ok := enumValue(e); ok {
				states[v] = true
			}
		}
	}
	pulls := make([]*store.Pull, 0, len(repo.Pulls))
	for _, p := range repo.Pulls {
		pulls = append(pulls, p)
	}
	if len(states) > 0 {
		filtered := pulls[:0]
		for _, p := range pulls {
			st := p.State
			if p.Merged {
				st = "MERGED"
			} else {
				st = strings.ToUpper(p.State)
			}
			if states[st] || states[strings.ToUpper(p.State)] {
				filtered = append(filtered, p)
			}
		}
		pulls = filtered
	} else {
		filtered := pulls[:0]
		for _, p := range pulls {
			if p.State == "open" {
				filtered = append(filtered, p)
			}
		}
		pulls = filtered
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	if after != nil {
		filtered := pulls[:0]
		for _, p := range pulls {
			if p.Number < *after {
				filtered = append(filtered, p)
			}
		}
		pulls = filtered
	}
	pageLen := jsSliceLen(len(pulls), first)
	page := pulls[:pageLen]
	hasNext := len(pulls) > first
	nodes := make([]any, 0, len(page))
	for _, p := range page {
		nodes = append(nodes, pullNode(s, repo, p, nodeChildren(sel, "nodes")))
	}
	out := newOrderedObj()
	for _, child := range sel.children {
		key := gqlKey(child)
		switch child.name {
		case "nodes":
			out.set(key, nodes)
		case "edges":
			nodeSels := nodeChildren(child, "node")
			edges := make([]any, 0, len(page))
			for _, p := range page {
				edges = append(edges, newOrderedObj().
					set("node", pullNode(s, repo, p, nodeSels)).
					set("cursor", encodeCursor(p.Number)))
			}
			out.set(key, edges)
		case "pageInfo":
			var startCursor, endCursor any
			if len(page) > 0 {
				startCursor = encodeCursor(page[0].Number)
				endCursor = encodeCursor(page[len(page)-1].Number)
			}
			out.set(key, newOrderedObj().
				set("hasNextPage", hasNext).
				set("hasPreviousPage", after != nil).
				set("startCursor", startCursor).
				set("endCursor", endCursor))
		case "totalCount":
			out.set(key, len(pulls))
		}
	}
	return out
}

func issueNode(s *store.Store, repo *store.Repo, issue *store.Issue, sels []*gqlSelection) *orderedObj {
	out := newOrderedObj()
	for _, sel := range sels {
		if sel.name == "__fragment" {
			if sel.fragmentType != nil && *sel.fragmentType != "Issue" {
				continue
			}
			merged := issueNode(s, repo, issue, sel.children)
			for _, k := range merged.keys {
				out.set(k, merged.vals[k])
			}
			continue
		}
		key := gqlKey(sel)
		switch sel.name {
		case "id":
			out.set(key, store.GlobalID("Issue", fmt.Sprintf("%d:%d", repo.ID, issue.Number)))
		case "number":
			out.set(key, issue.Number)
		case "title":
			out.set(key, issue.Title)
		case "body":
			out.set(key, issue.Body)
		case "state":
			out.set(key, strings.ToUpper(issue.State))
		case "author":
			out.set(key, newOrderedObj().set("login", issue.User))
		case "createdAt":
			out.set(key, issue.CreatedAt)
		case "updatedAt":
			out.set(key, issue.UpdatedAt)
		case "repository":
			out.set(key, repositoryNode(s, repo, sel.children))
		default:
			gqlThrow("unknown field Issue.%s", sel.name)
		}
	}
	return out
}

func pullNode(s *store.Store, repo *store.Repo, pull *store.Pull, sels []*gqlSelection) *orderedObj {
	out := newOrderedObj()
	for _, sel := range sels {
		if sel.name == "__fragment" {
			if sel.fragmentType != nil && *sel.fragmentType != "PullRequest" {
				continue
			}
			merged := pullNode(s, repo, pull, sel.children)
			for _, k := range merged.keys {
				out.set(k, merged.vals[k])
			}
			continue
		}
		key := gqlKey(sel)
		switch sel.name {
		case "id":
			out.set(key, store.GlobalID("PullRequest", fmt.Sprintf("%d:%d", repo.ID, pull.Number)))
		case "number":
			out.set(key, pull.Number)
		case "title":
			out.set(key, pull.Title)
		case "state":
			if pull.Merged {
				out.set(key, "MERGED")
			} else {
				out.set(key, strings.ToUpper(pull.State))
			}
		case "merged":
			out.set(key, pull.Merged)
		case "mergeable":
			out.set(key, s.PullMergeable(repo, pull))
		case "headRefName":
			out.set(key, pull.Head.Ref)
		case "baseRefName":
			out.set(key, pull.Base.Ref)
		case "headRef":
			out.set(key, newOrderedObj().set("name", pull.Head.Ref))
		case "baseRef":
			out.set(key, newOrderedObj().set("name", pull.Base.Ref))
		case "author":
			out.set(key, newOrderedObj().set("login", pull.User))
		case "createdAt":
			out.set(key, pull.CreatedAt)
		case "updatedAt":
			out.set(key, pull.CreatedAt)
		case "repository":
			out.set(key, repositoryNode(s, repo, sel.children))
		case "files":
			diff := s.DiffPull(repo, pull)
			first := 100
			if f, ok := argInt(sel.args, "first"); ok {
				first = f
			}
			paths := make([]string, 0, len(diff))
			for p := range diff {
				paths = append(paths, p)
			}
			sort.Strings(paths)
			paths = paths[:jsSliceLen(len(paths), first)]
			nodes := make([]any, 0, len(paths))
			for _, p := range paths {
				nodes = append(nodes, newOrderedObj().set("path", p))
			}
			out2 := newOrderedObj()
			for _, child := range sel.children {
				k := gqlKey(child)
				switch child.name {
				case "nodes":
					out2.set(k, nodes)
				case "edges":
					edges := make([]any, 0, len(paths))
					for _, p := range paths {
						edges = append(edges, newOrderedObj().set("node", newOrderedObj().set("path", p)))
					}
					out2.set(k, edges)
				case "totalCount":
					out2.set(k, len(diff))
				}
			}
			out.set(key, out2)
		default:
			gqlThrow("unknown field PullRequest.%s", sel.name)
		}
	}
	return out
}

func typeNode(typeName string, sels []*gqlSelection) *orderedObj {
	out := newOrderedObj()
	for _, sel := range sels {
		key := gqlKey(sel)
		switch sel.name {
		case "name":
			out.set(key, typeName)
		case "fields":
			fields := gqlTypeFields[typeName]
			nodes := make([]any, 0, len(fields))
			for _, f := range fields {
				nodes = append(nodes, newOrderedObj().set("name", f))
			}
			// Requested sub-selections are ignored (graphql.ts builds
			// an out2 and discards it); includeDeprecated args ignored.
			out.set(key, nodes)
		case "enumValues":
			values := gqlTypeEnumValues[typeName]
			nodes := make([]any, 0, len(values))
			for _, v := range values {
				nodes = append(nodes, newOrderedObj().set("name", v))
			}
			out.set(key, nodes)
		default:
			out.set(key, nil)
		}
	}
	return out
}

// findRepoByID is the linear scan graphql.ts uses for node()/mutation
// id lookups ([...store.repos.values()].find(r => r.id === repoId)).
func findRepoByID(s *store.Store, repoID float64) *store.Repo {
	if math.Trunc(repoID) != repoID {
		return nil
	}
	for _, repo := range s.Repos {
		if repo.ID == int(repoID) {
			return repo
		}
	}
	return nil
}

// splitGlobalID splits a decoded global id's "<repoId>:<num>" payload
// the way graphql.ts's `decoded.id.split(':')` does; the second element
// is absent (not "") when there is no colon.
func splitGlobalID(id string) (repoPart string, numPart string, hasNum bool) {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[:i], id[i+1:], true
	}
	return id, "", false
}

// executeTop runs one top-level field list with per-field try/catch:
// on a gqlError the message is appended to errors and that field's
// data is null; siblings still resolve.
func executeTop(s *store.Store, sels []*gqlSelection, isMutation bool) (*orderedObj, []*orderedObj) {
	data := newOrderedObj()
	var errs []*orderedObj
	for _, sel := range sels {
		key := gqlKey(sel)
		func() {
			defer func() {
				if r := recover(); r != nil {
					if ge, ok := r.(gqlError); ok {
						errs = append(errs, gqlErrObj(ge.msg))
						data.set(key, nil)
						return
					}
					panic(r)
				}
			}()
			if isMutation {
				data.set(key, executeMutationField(s, sel))
			} else {
				data.set(key, executeQueryField(s, sel))
			}
		}()
	}
	return data, errs
}

func executeQueryField(s *store.Store, sel *gqlSelection) any {
	switch sel.name {
	case "viewer":
		out := newOrderedObj()
		for _, child := range sel.children {
			k := gqlKey(child)
			switch child.name {
			case "login":
				out.set(k, store.Viewer.Login)
			case "id":
				out.set(k, store.GlobalID("User", store.Viewer.ID))
			case "name":
				out.set(k, store.Viewer.Name)
			default:
				gqlThrow("Cannot query field %q on type %q.", child.name, "User")
			}
		}
		return out
	case "repository":
		owner, _ := sel.args["owner"].(string)
		name, _ := sel.args["name"].(string)
		var repo *store.Repo
		if owner != "" && name != "" {
			repo = s.GetRepo(owner, name)
		}
		if repo == nil {
			return nil
		}
		return repositoryNode(s, repo, sel.children)
	case "node":
		id, _ := sel.args["id"].(string)
		var node any
		if id != "" {
			if decoded := store.DecodeGlobalID(id); decoded != nil && decoded.Kind == "Repository" {
				if repoID, ok := jsParseInt(decoded.ID); ok {
					if repo := findRepoByID(s, repoID); repo != nil {
						node = repositoryNode(s, repo, sel.children)
					}
				}
			}
		}
		return node
	case "__type":
		name, _ := sel.args["name"].(string)
		if name == "" {
			return nil
		}
		return typeNode(name, sel.children)
	case "__schema":
		return newOrderedObj().
			set("queryType", newOrderedObj().set("name", "Query")).
			set("mutationType", newOrderedObj().set("name", "Mutation"))
	default:
		gqlThrow("Cannot query field %q on type %q.", sel.name, "Query")
		return nil
	}
}

// mutationInput is sel.args.input as an object, default {} (graphql.ts:
// `const input = (sel.args.input ?? {}) as Record<string, unknown>`).
func mutationInput(sel *gqlSelection) map[string]any {
	if m, ok := sel.args["input"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func executeMutationField(s *store.Store, sel *gqlSelection) any {
	input := mutationInput(sel)
	switch sel.name {
	case "createIssue":
		decoded := store.DecodeGlobalID(strOrEmpty(input["repositoryId"]))
		var repo *store.Repo
		if decoded != nil {
			if repoID, ok := jsParseInt(decoded.ID); ok {
				repo = findRepoByID(s, repoID)
			}
		}
		if repo == nil {
			gqlThrow("Repository not found")
		}
		issue, err := s.CreateIssue(repo, strOrEmpty(input["title"]), strOrEmpty(input["body"]), []string{})
		if err != nil {
			gqlThrow("%s", err.Error())
		}
		out := newOrderedObj()
		for _, child := range sel.children {
			k := gqlKey(child)
			switch child.name {
			case "issue":
				out.set(k, issueNode(s, repo, issue, child.children))
			case "clientMutationId":
				out.set(k, input["clientMutationId"])
			}
		}
		return out
	case "closeIssue":
		decoded := store.DecodeGlobalID(strOrEmpty(input["issueId"]))
		if decoded == nil {
			gqlThrow("Issue not found")
		}
		repoPart, numPart, hasNum := splitGlobalID(decoded.ID)
		repoID, okID := jsParseInt(repoPart)
		num, okNum := jsParseInt(numPart)
		var repo *store.Repo
		var issue *store.Issue
		if okID && hasNum && okNum {
			repo = findRepoByID(s, repoID)
			if repo != nil && math.Trunc(num) == num {
				issue = repo.Issues[int(num)]
			}
		}
		if issue == nil {
			gqlThrow("Issue not found")
		}
		issue.State = "closed"
		issue.UpdatedAt = store.FixedISO
		store.InvalidateIssueJSON(issue)
		out := newOrderedObj()
		for _, child := range sel.children {
			k := gqlKey(child)
			switch child.name {
			case "issue":
				out.set(k, issueNode(s, repo, issue, child.children))
			case "clientMutationId":
				out.set(k, input["clientMutationId"])
			}
		}
		return out
	case "createPullRequest":
		decoded := store.DecodeGlobalID(strOrEmpty(input["repositoryId"]))
		var repo *store.Repo
		if decoded != nil {
			if repoID, ok := jsParseInt(decoded.ID); ok {
				repo = findRepoByID(s, repoID)
			}
		}
		if repo == nil {
			gqlThrow("Repository not found")
		}
		pull, err := s.CreatePull(repo, strOrEmpty(input["title"]), strOrEmpty(input["body"]),
			strOrEmpty(input["headRefName"]), strOrEmpty(input["baseRefName"]))
		if err != nil {
			gqlThrow("%s", err.Error())
		}
		out := newOrderedObj()
		for _, child := range sel.children {
			k := gqlKey(child)
			switch child.name {
			case "pullRequest":
				out.set(k, pullNode(s, repo, pull, child.children))
			case "clientMutationId":
				out.set(k, input["clientMutationId"])
			}
		}
		return out
	case "mergePullRequest":
		decoded := store.DecodeGlobalID(strOrEmpty(input["pullRequestId"]))
		if decoded == nil {
			gqlThrow("Pull request not found")
		}
		repoPart, numPart, hasNum := splitGlobalID(decoded.ID)
		repoID, okID := jsParseInt(repoPart)
		num, okNum := jsParseInt(numPart)
		var repo *store.Repo
		var pull *store.Pull
		if okID && hasNum && okNum {
			repo = findRepoByID(s, repoID)
			if repo != nil && math.Trunc(num) == num {
				pull = repo.Pulls[int(num)]
			}
		}
		if pull == nil || repo == nil {
			gqlThrow("Pull request not found")
		}
		if _, err := s.MergePull(repo, pull); err != nil {
			gqlThrow("%s", err.Error())
		}
		out := newOrderedObj()
		for _, child := range sel.children {
			k := gqlKey(child)
			switch child.name {
			case "pullRequest":
				out.set(k, pullNode(s, repo, pull, child.children))
			case "clientMutationId":
				out.set(k, input["clientMutationId"])
			}
		}
		return out
	case "addComment":
		decoded := store.DecodeGlobalID(strOrEmpty(input["subjectId"]))
		if decoded == nil {
			gqlThrow("Subject not found")
		}
		repoPart, numPart, hasNum := splitGlobalID(decoded.ID)
		repoID, okID := jsParseInt(repoPart)
		num, okNum := jsParseInt(numPart)
		var repo *store.Repo
		var issue *store.Issue
		var pull *store.Pull
		if okID && hasNum && okNum && math.Trunc(num) == num {
			repo = findRepoByID(s, repoID)
			if repo != nil {
				issue = repo.Issues[int(num)]
				pull = repo.Pulls[int(num)]
			}
		}
		if repo == nil || (issue == nil && pull == nil) {
			gqlThrow("Subject not found")
		}
		comment := &store.IssueComment{
			ID:        repo.NextCommentID,
			Body:      strOrEmpty(input["body"]),
			User:      store.Viewer.Login,
			CreatedAt: store.FixedISO,
		}
		repo.NextCommentID++
		// Quirk (graphql.ts): the comment is appended ONLY if an issue
		// was found; for a pull-request subject nothing is stored — but
		// the response still returns a commentEdge with the fresh id.
		if issue != nil {
			issue.Comments = append(issue.Comments, comment)
			store.InvalidateIssueJSON(issue)
		}
		commentID := store.GlobalID("IssueComment", fmt.Sprintf("%d:%d", repo.ID, comment.ID))
		out := newOrderedObj()
		for _, child := range sel.children {
			k := gqlKey(child)
			switch child.name {
			case "commentEdge":
				nodeSels := nodeChildren(child, "node")
				node := newOrderedObj()
				for _, n := range nodeSels {
					nk := gqlKey(n)
					switch n.name {
					case "id":
						node.set(nk, commentID)
					case "body":
						node.set(nk, comment.Body)
					}
				}
				out.set(k, newOrderedObj().set("node", node).set("cursor", encodeCursor(comment.ID)))
			case "clientMutationId":
				out.set(k, input["clientMutationId"])
			}
		}
		return out
	default:
		gqlThrow("Cannot query field %q on type %q.", sel.name, "Mutation")
		return nil
	}
}

// ExecuteGraphQL executes one GraphQL document and returns the result
// object (graphql.ts:696-709): {data} when no errors, else
// {data, errors}; a parse failure is {data: null, errors: [{message}]}.
func ExecuteGraphQL(s *store.Store, query string, variables map[string]any) *orderedObj {
	parser := newGqlParser(query, variables)
	operation, sels, err := parser.parseOperation()
	if err != nil {
		return newOrderedObj().
			set("data", nil).
			set("errors", []any{gqlErrObj(err.Error())})
	}
	data, errs := executeTop(s, sels, operation == "mutation")
	if len(errs) == 0 {
		return newOrderedObj().set("data", data)
	}
	errAny := make([]any, len(errs))
	for i, e := range errs {
		errAny[i] = e
	}
	return newOrderedObj().set("data", data).set("errors", errAny)
}

// HandleGraphQL is the full /api/graphql route handler (app.ts:494-509):
// non-POST → 405; unparsable JSON body → 400 "Invalid JSON body"; an
// array body runs each document in order, else one document; always
// HTTP 200 on success. query defaults to ” when absent/non-string and
// variables to {} when absent/non-object.
func HandleGraphQL(s *store.Store, req *RawRequest) []byte {
	if req.Method != "POST" {
		return BuildErrorResponse(405, "Method Not Allowed")
	}
	parsed, err := decodeJSONBody(req.Body)
	if err != nil {
		return BuildErrorResponse(400, "Invalid JSON body")
	}
	run := func(doc any) *orderedObj {
		m, _ := doc.(map[string]any)
		var query string
		var vars map[string]any
		if m != nil {
			if q, ok := m["query"].(string); ok {
				query = q
			}
			if v, ok := m["variables"].(map[string]any); ok {
				vars = v
			}
		}
		return ExecuteGraphQL(s, query, vars)
	}
	var result any
	if arr, ok := parsed.([]any); ok {
		out := make([]*orderedObj, 0, len(arr))
		for _, d := range arr {
			out = append(out, run(d))
		}
		result = out
	} else {
		result = run(parsed)
	}
	return BuildResponse(200,
		[]Header{{Key: "Content-Type", Value: "application/json"}},
		store.MarshalJSONValue(result))
}

// decodeJSONBody parses a request body exactly like JSON.parse: one JSON
// value, no trailing content (trailing garbage is a parse error).
func decodeJSONBody(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("unexpected trailing JSON value")
		}
		return nil, err
	}
	return v, nil
}
