// Package jsonc edits JSON-with-comments documents in place.
//
// Tools such as OpenCode keep their configuration as JSONC: JSON plus
// `//` and `/* */` comments and trailing commas. Round-tripping a file
// like that through encoding/json would throw the comments away, and
// people use them to explain why a setting is there. So instead of
// re-serialising, Set finds the byte span of the value it is changing
// (or the object it must add a member to) and splices new text in,
// leaving every other byte of the document as it was.
//
// Standardize turns a document into plain JSON for reading with
// encoding/json; offsets are preserved, since comments and trailing
// commas become spaces.
package jsonc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Standardize returns src as plain JSON: comments and trailing commas
// are replaced by spaces, strings are left alone.
func Standardize(src []byte) []byte {
	out := append([]byte(nil), src...)
	for i := 0; i < len(out); i++ {
		switch c := out[i]; {
		case c == '"':
			i = stringEnd(out, i) - 1
		case c == '/' && i+1 < len(out) && (out[i+1] == '/' || out[i+1] == '*'):
			end := commentEnd(out, i)
			for j := i; j < end; j++ {
				if out[j] != '\n' && out[j] != '\r' {
					out[j] = ' '
				}
			}
			i = end - 1
		case c == ',':
			j := skipSpace(out, i+1)
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				out[i] = ' '
			}
		}
	}
	return out
}

// Unmarshal decodes a JSONC document into v.
func Unmarshal(src []byte, v any) error {
	return json.Unmarshal(Standardize(src), v)
}

// Set sets the value at path, an object-key path from the root, to the
// JSON encoding of value. Objects missing along the path are created; a
// non-object found along the path is replaced. Everything else in the
// document, comments included, is kept byte for byte.
//
// An empty (or comment-only) document is treated as an empty object.
func Set(src []byte, path []string, value any) ([]byte, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("jsonc: empty path")
	}
	if _, err := json.Marshal(value); err != nil {
		return nil, err
	}

	p := &parser{src: src}
	p.pos = skipSpace(src, 0)
	if p.pos == len(src) {
		// Nothing but whitespace and comments: start an object after them.
		doc := append([]byte(nil), src...)
		if len(doc) > 0 && doc[len(doc)-1] != '\n' {
			doc = append(doc, '\n')
		}
		doc = append(doc, "{}\n"...)
		return Set(doc, path, value)
	}
	root, err := p.value()
	if err != nil {
		return nil, err
	}
	if p.pos = skipSpace(src, p.pos); p.pos != len(src) {
		return nil, p.errorf("unexpected data after the top-level value")
	}
	if root.kind != kindObject {
		return nil, fmt.Errorf("jsonc: top-level value is not an object")
	}

	unit := indentUnit(src, root)
	leaf := func(indent string) []byte { return render(value, indent, unit) }
	node := root
	for i, key := range path {
		m := node.member(key)
		last := i == len(path)-1
		switch {
		case m == nil:
			indent := memberIndent(src, node, unit)
			text := memberText(path[i:], leaf, indent, unit)
			return insertMember(src, node, text, indent), nil
		case last:
			return splice(src, m.value.start, m.value.end, leaf(lineIndent(src, m.keyStart))), nil
		case m.value.kind != kindObject:
			indent := lineIndent(src, m.keyStart)
			return splice(src, m.value.start, m.value.end, nested(path[i+1:], leaf, indent, unit)), nil
		default:
			node = m.value
		}
	}
	panic("unreachable")
}

type kind int

const (
	kindScalar kind = iota
	kindObject
	kindArray
)

type node struct {
	kind       kind
	start, end int // value span; for objects and arrays, the brackets inclusive
	members    []member
}

type member struct {
	key      string
	keyStart int
	value    *node
	// commaEnd is just past a comma following the value, or 0 when the
	// value isn't followed by one (the last member, without a trailing
	// comma).
	commaEnd int
}

// member returns the last member named key: JSON parsers, OpenCode's
// included, let a later duplicate win.
func (n *node) member(key string) *member {
	for i := len(n.members) - 1; i >= 0; i-- {
		if n.members[i].key == key {
			return &n.members[i]
		}
	}
	return nil
}

type parser struct {
	src []byte
	pos int
}

func (p *parser) errorf(format string, args ...any) error {
	line := 1 + bytes.Count(p.src[:min(p.pos, len(p.src))], []byte("\n"))
	return fmt.Errorf("jsonc: line %d: %s", line, fmt.Sprintf(format, args...))
}

func (p *parser) value() (*node, error) {
	p.pos = skipSpace(p.src, p.pos)
	if p.pos >= len(p.src) {
		return nil, p.errorf("unexpected end of document")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		start := p.pos
		end := stringEnd(p.src, p.pos)
		if end > len(p.src) || p.src[end-1] != '"' || end == start+1 {
			return nil, p.errorf("unterminated string")
		}
		p.pos = end
		return &node{kind: kindScalar, start: start, end: end}, nil
	default:
		start := p.pos
		for p.pos < len(p.src) && strings.IndexByte("-+.0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", p.src[p.pos]) >= 0 {
			p.pos++
		}
		if p.pos == start {
			return nil, p.errorf("unexpected %q", c)
		}
		lit := p.src[start:p.pos]
		var v any
		if err := json.Unmarshal(lit, &v); err != nil {
			return nil, p.errorf("invalid value %q", lit)
		}
		return &node{kind: kindScalar, start: start, end: p.pos}, nil
	}
}

func (p *parser) object() (*node, error) {
	n := &node{kind: kindObject, start: p.pos}
	p.pos++
	for {
		p.pos = skipSpace(p.src, p.pos)
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated object")
		}
		if p.src[p.pos] == '}' {
			p.pos++
			n.end = p.pos
			return n, nil
		}
		if len(n.members) > 0 && n.members[len(n.members)-1].commaEnd == 0 {
			return nil, p.errorf("expected ',' or '}'")
		}
		if p.src[p.pos] != '"' {
			return nil, p.errorf("expected a string key")
		}
		keyStart := p.pos
		keyEnd := stringEnd(p.src, p.pos)
		var key string
		if err := json.Unmarshal(p.src[keyStart:keyEnd], &key); err != nil {
			return nil, p.errorf("invalid key")
		}
		p.pos = skipSpace(p.src, keyEnd)
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, p.errorf("expected ':' after key %q", key)
		}
		p.pos++
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		m := member{key: key, keyStart: keyStart, value: v}
		p.pos = skipSpace(p.src, p.pos)
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			m.commaEnd = p.pos
		}
		n.members = append(n.members, m)
	}
}

func (p *parser) array() (*node, error) {
	n := &node{kind: kindArray, start: p.pos}
	p.pos++
	needComma := false
	for {
		p.pos = skipSpace(p.src, p.pos)
		if p.pos >= len(p.src) {
			return nil, p.errorf("unterminated array")
		}
		if p.src[p.pos] == ']' {
			p.pos++
			n.end = p.pos
			return n, nil
		}
		if needComma {
			return nil, p.errorf("expected ',' or ']'")
		}
		if _, err := p.value(); err != nil {
			return nil, err
		}
		p.pos = skipSpace(p.src, p.pos)
		needComma = true
		if p.pos < len(p.src) && p.src[p.pos] == ',' {
			p.pos++
			needComma = false
		}
	}
}

// stringEnd returns the index just past the string starting at i (which
// holds the opening quote), or len(src)+1 if it never closes.
func stringEnd(src []byte, i int) int {
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		case '\n':
			return len(src) + 1
		}
	}
	return len(src) + 1
}

// commentEnd returns the index just past the comment starting at i. A
// line comment ends before its newline.
func commentEnd(src []byte, i int) int {
	if src[i+1] == '/' {
		if nl := bytes.IndexByte(src[i:], '\n'); nl >= 0 {
			return i + nl
		}
		return len(src)
	}
	if end := bytes.Index(src[i+2:], []byte("*/")); end >= 0 {
		return i + 2 + end + 2
	}
	return len(src)
}

// skipSpace returns the first index at or after i that is neither
// whitespace nor inside a comment.
func skipSpace(src []byte, i int) int {
	for i < len(src) {
		switch c := src[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '/' && i+1 < len(src) && (src[i+1] == '/' || src[i+1] == '*'):
			i = commentEnd(src, i)
		default:
			return i
		}
	}
	return i
}

// lineIndent is the leading whitespace of the line containing i.
func lineIndent(src []byte, i int) string {
	start := bytes.LastIndexByte(src[:i], '\n') + 1
	end := start
	for end < len(src) && (src[end] == ' ' || src[end] == '\t') {
		end++
	}
	return string(src[start:end])
}

// startsLine reports whether only whitespace precedes i on its line.
func startsLine(src []byte, i int) bool {
	start := bytes.LastIndexByte(src[:i], '\n') + 1
	return strings.TrimLeft(string(src[start:i]), " \t") == ""
}

// indentUnit guesses the document's indentation step from the root
// object's first member, defaulting to two spaces.
func indentUnit(src []byte, root *node) string {
	for _, m := range root.members {
		if startsLine(src, m.keyStart) {
			if in := lineIndent(src, m.keyStart); in != "" {
				return in
			}
		}
	}
	return "  "
}

// memberIndent is the indentation a new member of n should get: that of
// its existing members, or one step in from the object's own line.
func memberIndent(src []byte, n *node, unit string) string {
	for _, m := range n.members {
		if startsLine(src, m.keyStart) {
			return lineIndent(src, m.keyStart)
		}
	}
	return lineIndent(src, n.start) + unit
}

// memberText renders `"k": <nested rest>` for a member at indent.
func memberText(path []string, leaf func(string) []byte, indent, unit string) []byte {
	key, _ := json.Marshal(path[0])
	out := append(key, ": "...)
	return append(out, nested(path[1:], leaf, indent, unit)...)
}

// nested renders leaf wrapped in one object per remaining key, laid out
// as if it were the value of a member indented by indent.
func nested(path []string, leaf func(string) []byte, indent, unit string) []byte {
	if len(path) == 0 {
		return leaf(indent)
	}
	inner := indent + unit
	out := []byte("{\n" + inner)
	out = append(out, memberText(path, leaf, inner, unit)...)
	return append(out, "\n"+indent+"}"...)
}

// insertMember adds a member to object n. It goes after the last member
// — after that member's comma, and after a comment on the same line,
// so a comment stays with the line it annotates — or inside empty
// braces.
func insertMember(src []byte, n *node, text []byte, indent string) []byte {
	if len(n.members) == 0 {
		ins := append([]byte("\n"+indent), text...)
		ins = append(ins, "\n"+lineIndent(src, n.start)...)
		inside := src[n.start+1 : n.end-1]
		if len(bytes.TrimSpace(inside)) == 0 {
			// `{}`, `{ }` or braces on separate lines: replace the inside.
			return splice(src, n.start+1, n.end-1, ins)
		}
		// Only comments inside: keep them, after the new member.
		at := n.start + 1
		if eol := restOfLineIsComment(src, at); eol >= 0 {
			at = eol
		}
		return splice(src, at, at, ins[:len(ins)-len(lineIndent(src, n.start))-1])
	}

	last := n.members[len(n.members)-1]
	if !startsLine(src, last.keyStart) {
		// A one-line object such as {"a": 1}: stay on the line.
		at, prefix := last.value.end, ", "
		if last.commaEnd != 0 {
			at, prefix = last.commaEnd, " "
		}
		return splice(src, at, at, append([]byte(prefix), text...))
	}
	out, at := src, last.commaEnd
	if at == 0 {
		out = splice(src, last.value.end, last.value.end, []byte(","))
		at = last.value.end + 1
	}
	if eol := restOfLineIsComment(out, at); eol >= 0 {
		at = eol
	}
	return splice(out, at, at, append([]byte("\n"+indent), text...))
}

// restOfLineIsComment returns the end of the line starting at i when
// what's left of it is whitespace and comments only, or -1.
func restOfLineIsComment(src []byte, i int) int {
	for i < len(src) {
		switch c := src[i]; {
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '\n':
			return i
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			return commentEnd(src, i)
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := commentEnd(src, i)
			if bytes.IndexByte(src[i:end], '\n') >= 0 {
				return -1
			}
			i = end
		default:
			return -1
		}
	}
	return i
}

// render encodes v as the value of a member indented by indent: scalars
// inline, objects and arrays laid out one member per line. HTML isn't
// escaped, since nothing here ends up in a web page and a URL with a
// query string should stay readable.
func render(v any, indent, unit string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent(indent, unit)
	_ = enc.Encode(v) // already checked by Set
	return bytes.TrimRight(b.Bytes(), "\n")
}

func splice(src []byte, start, end int, text []byte) []byte {
	out := make([]byte, 0, len(src)-(end-start)+len(text))
	out = append(out, src[:start]...)
	out = append(out, text...)
	return append(out, src[end:]...)
}
