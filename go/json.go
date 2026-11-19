// The forge's private JSON engine: it parses the bodies clients send
// and renders the replies it returns, with no encoding/json anywhere
// in the binary. The standard decoder is built for every shape JSON can
// take; this server only ever meets a narrow slice of it, and skipping
// the general machinery keeps the static binary well inside the
// entry's 8 MiB per-file allowance.
//
// One value model throughout: nil, bool, float64 for every number,
// string, []any and map[string]any. When a reply's key order matters
// the ordered object type below carries it; plain maps render with
// sorted keys, which is stable across runs and good enough for the
// surfaces that build plain maps (the GraphQL and MCP layers assemble
// ordered objects where order is visible).

package main

import (
	"strconv"
	"strings"
)

// OrderedJSON is a JSON object that remembers the order its keys were
// first set, and renders them in that order.
type OrderedJSON struct {
	Order  []string
	Values map[string]any
}

func NewOrderedJSON() *OrderedJSON {
	return &OrderedJSON{Values: map[string]any{}}
}

// Put stores val under key, appending the key to the render order the
// first time it appears. It returns the object so calls can chain.
func (o *OrderedJSON) Put(key string, val any) *OrderedJSON {
	if _, seen := o.Values[key]; !seen {
		o.Order = append(o.Order, key)
	}
	o.Values[key] = val
	return o
}

// Lookup fetches the value stored under key.
func (o *OrderedJSON) Lookup(key string) (any, bool) {
	val, seen := o.Values[key]
	return val, seen
}

// ---------------------------------------------------------------------------
// Rendering values to text
// ---------------------------------------------------------------------------

const hexAlphabet = "0123456789abcdef"

// quoteJSONString renders s as a JSON string literal. Most strings the
// forge emits (names, titles, ids) need no escaping at all, and the
// scan for that case runs first so they cost one pass and a concat.
func quoteJSONString(s string) string {
	plain := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' {
			plain = false
			break
		}
	}
	if plain {
		return `"` + s + `"`
	}
	var out strings.Builder
	out.Grow(len(s) + 2)
	out.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if c >= 0x20 {
				out.WriteByte(c)
			} else {
				out.WriteString(`\u00`)
				out.WriteByte(hexAlphabet[c>>4])
				out.WriteByte(hexAlphabet[c&0xf])
			}
		}
	}
	out.WriteByte('"')
	return out.String()
}

// renderValueInto appends the compact JSON form of v to out.
func renderValueInto(out *strings.Builder, v any) {
	switch node := v.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if node {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		out.WriteString(quoteJSONString(node))
	case int:
		out.WriteString(strconv.Itoa(node))
	case int64:
		out.WriteString(strconv.FormatInt(node, 10))
	case float64:
		out.WriteString(strconv.FormatFloat(node, 'g', -1, 64))
	case []any:
		out.WriteByte('[')
		for idx, item := range node {
			if idx > 0 {
				out.WriteByte(',')
			}
			renderValueInto(out, item)
		}
		out.WriteByte(']')
	case map[string]any:
		names := make([]string, 0, len(node))
		for name := range node {
			names = append(names, name)
		}
		sortLexical(names)
		out.WriteByte('{')
		for idx, name := range names {
			if idx > 0 {
				out.WriteByte(',')
			}
			out.WriteString(quoteJSONString(name))
			out.WriteByte(':')
			renderValueInto(out, node[name])
		}
		out.WriteByte('}')
	case *OrderedJSON:
		out.WriteByte('{')
		for idx, name := range node.Order {
			if idx > 0 {
				out.WriteByte(',')
			}
			out.WriteString(quoteJSONString(name))
			out.WriteByte(':')
			renderValueInto(out, node.Values[name])
		}
		out.WriteByte('}')
	default:
		out.WriteString("null")
	}
}

// EncodeJSON renders a value as one compact JSON document.
func EncodeJSON(v any) string {
	var out strings.Builder
	renderValueInto(&out, v)
	return out.String()
}

// sortLexical orders strings ascending, in place (insertion sort: the
// slices involved are a single object's keys, always short).
func sortLexical(items []string) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j] < items[j-1]; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// ---------------------------------------------------------------------------
// Reading values from text
// ---------------------------------------------------------------------------

// jsonScanner walks one input document with a byte cursor.
type jsonScanner struct {
	src []byte
	at  int
}

func (sc *jsonScanner) skipBlanks() {
	for sc.at < len(sc.src) {
		switch sc.src[sc.at] {
		case ' ', '\t', '\n', '\r':
			sc.at++
		default:
			return
		}
	}
}

// readValue parses whichever value starts at the cursor.
func (sc *jsonScanner) readValue() (any, bool) {
	sc.skipBlanks()
	if sc.at >= len(sc.src) {
		return nil, false
	}
	switch c := sc.src[sc.at]; c {
	case '{':
		return sc.readObject()
	case '[':
		return sc.readArray()
	case '"':
		return sc.readString()
	case 't':
		return sc.readKeyword("true", true)
	case 'f':
		return sc.readKeyword("false", false)
	case 'n':
		return sc.readKeyword("null", nil)
	default:
		return sc.readNumber()
	}
}

// readKeyword consumes one of the three literal words of JSON.
func (sc *jsonScanner) readKeyword(word string, val any) (any, bool) {
	if sc.at+len(word) <= len(sc.src) && string(sc.src[sc.at:sc.at+len(word)]) == word {
		sc.at += len(word)
		return val, true
	}
	return nil, false
}

func (sc *jsonScanner) readObject() (any, bool) {
	sc.at++ // the opening brace
	obj := map[string]any{}
	sc.skipBlanks()
	if sc.at < len(sc.src) && sc.src[sc.at] == '}' {
		sc.at++
		return obj, true
	}
	for {
		sc.skipBlanks()
		key, ok := sc.readString()
		if !ok {
			return nil, false
		}
		sc.skipBlanks()
		if sc.at >= len(sc.src) || sc.src[sc.at] != ':' {
			return nil, false
		}
		sc.at++
		val, ok := sc.readValue()
		if !ok {
			return nil, false
		}
		obj[key] = val
		sc.skipBlanks()
		if sc.at >= len(sc.src) {
			return nil, false
		}
		if sc.src[sc.at] == '}' {
			sc.at++
			return obj, true
		}
		if sc.src[sc.at] != ',' {
			return nil, false
		}
		sc.at++
	}
}

func (sc *jsonScanner) readArray() (any, bool) {
	sc.at++ // the opening bracket
	arr := []any{}
	sc.skipBlanks()
	if sc.at < len(sc.src) && sc.src[sc.at] == ']' {
		sc.at++
		return arr, true
	}
	for {
		val, ok := sc.readValue()
		if !ok {
			return nil, false
		}
		arr = append(arr, val)
		sc.skipBlanks()
		if sc.at >= len(sc.src) {
			return nil, false
		}
		if sc.src[sc.at] == ']' {
			sc.at++
			return arr, true
		}
		if sc.src[sc.at] != ',' {
			return nil, false
		}
		sc.at++
	}
}

// readString parses a string literal, resolving escapes. A high
// surrogate directly followed by an escaped low surrogate combines
// into the single rune the pair encodes.
func (sc *jsonScanner) readString() (string, bool) {
	if sc.at >= len(sc.src) || sc.src[sc.at] != '"' {
		return "", false
	}
	sc.at++
	var out strings.Builder
	for sc.at < len(sc.src) {
		c := sc.src[sc.at]
		if c == '"' {
			sc.at++
			return out.String(), true
		}
		if c != '\\' {
			out.WriteByte(c)
			sc.at++
			continue
		}
		if sc.at+1 >= len(sc.src) {
			return "", false
		}
		switch esc := sc.src[sc.at+1]; esc {
		case 'n':
			out.WriteByte('\n')
			sc.at += 2
		case 't':
			out.WriteByte('\t')
			sc.at += 2
		case 'r':
			out.WriteByte('\r')
			sc.at += 2
		case 'b':
			out.WriteByte('\b')
			sc.at += 2
		case 'f':
			out.WriteByte('\f')
			sc.at += 2
		case '/':
			out.WriteByte('/')
			sc.at += 2
		case '\\':
			out.WriteByte('\\')
			sc.at += 2
		case '"':
			out.WriteByte('"')
			sc.at += 2
		case 'u':
			r, width, ok := sc.readUnicodeEscape()
			if !ok {
				return "", false
			}
			out.WriteRune(r)
			sc.at += width
		default:
			return "", false
		}
	}
	return "", false
}

// readUnicodeEscape decodes the \uXXXX escape at the cursor (which
// points at the backslash), joining a surrogate pair when one follows.
// It reports the rune and the total bytes the escape spans.
func (sc *jsonScanner) readUnicodeEscape() (rune, int, bool) {
	if sc.at+6 > len(sc.src) {
		return 0, 0, false
	}
	n, err := strconv.ParseUint(string(sc.src[sc.at+2:sc.at+6]), 16, 32)
	if err != nil {
		return 0, 0, false
	}
	// A high surrogate earns its meaning only paired with the low
	// surrogate written right after it.
	if n >= 0xD800 && n <= 0xDBFF && sc.at+12 <= len(sc.src) &&
		sc.src[sc.at+6] == '\\' && sc.src[sc.at+7] == 'u' {
		if m, err2 := strconv.ParseUint(string(sc.src[sc.at+8:sc.at+12]), 16, 32); err2 == nil && m >= 0xDC00 && m <= 0xDFFF {
			joined := 0x10000 + (rune(n-0xD800) << 10) + rune(m-0xDC00)
			return joined, 12, true
		}
	}
	return rune(n), 6, true
}

func (sc *jsonScanner) readNumber() (any, bool) {
	from := sc.at
	if sc.at < len(sc.src) && sc.src[sc.at] == '-' {
		sc.at++
	}
	for sc.at < len(sc.src) {
		c := sc.src[sc.at]
		if (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-' {
			sc.at++
		} else {
			break
		}
	}
	if sc.at == from {
		return nil, false
	}
	if f, err := strconv.ParseFloat(string(sc.src[from:sc.at]), 64); err == nil {
		return f, true
	}
	return nil, false
}

// DecodeJSON parses a complete JSON document, rejecting trailing
// content and any syntax error along the way.
func DecodeJSON(data []byte) (any, bool) {
	sc := &jsonScanner{src: data}
	v, ok := sc.readValue()
	if !ok {
		return nil, false
	}
	sc.skipBlanks()
	if sc.at != len(sc.src) {
		return nil, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// Pulling typed members out of a parsed object
// ---------------------------------------------------------------------------

func asObjectMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func stringMember(m map[string]any, name string) (string, bool) {
	raw, seen := m[name]
	if !seen {
		return "", false
	}
	s, ok := raw.(string)
	return s, ok
}

func boolMember(m map[string]any, name string) (bool, bool) {
	raw, seen := m[name]
	if !seen {
		return false, false
	}
	b, ok := raw.(bool)
	return b, ok
}

func numberMember(m map[string]any, name string) (float64, bool) {
	raw, seen := m[name]
	if !seen {
		return 0, false
	}
	f, ok := raw.(float64)
	return f, ok
}
