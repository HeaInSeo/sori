package admininit

// This file is the canonical JSON profile of the admin-tail records
// (keys/keyring, policy/state, epoch) from SORI-D01-W021B-FMT-PREP-r2 §2.3 and
// §2.3.1. The bytes are those of Python
// json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=True):
// objects with keys sorted by byte value, no whitespace, ASCII only, and the
// string escapes of §2.3.1. encoding/json is not used on either side: its
// Marshal escapes &, < and > and writes raw UTF-8, and its decoder keeps the
// last of two duplicate keys and accepts lone surrogates.
//
// The value model is the v1 admin model: object (map[string]any), array
// ([]any), string and unsigned integer (uint64, at most 2^63-1). true, false,
// null, negative numbers, fractions and exponents are refused.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// maxCanonSize bounds an admin-tail record (§2.3 reader step 1).
const maxCanonSize = 64 << 10

// maxCanonDepth bounds nesting. The deepest v1 record has depth 3.
const maxCanonDepth = 16

// maxCanonInt is the largest integer the profile allows (2^63-1).
const maxCanonInt = 1<<63 - 1

// errNotCanonical is the cause of every byte-level refusal of a record.
var errNotCanonical = errors.New("not canonical")

// encodeCanon returns the canonical bytes of v, without a trailing newline.
func encodeCanon(v any) ([]byte, error) {
	return appendCanon(nil, v, 0)
}

func appendCanon(b []byte, v any, depth int) ([]byte, error) {
	if depth > maxCanonDepth {
		return nil, fmt.Errorf("canonical encode: nesting deeper than %d", maxCanonDepth)
	}
	switch x := v.(type) {
	case map[string]any:
		return appendCanonObject(b, x, depth)
	case []any:
		b = append(b, '[')
		for i, e := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = appendCanon(b, e, depth+1); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case string:
		return appendCanonString(b, x)
	case uint64:
		if x > maxCanonInt {
			return nil, fmt.Errorf("canonical encode: integer %d above 2^63-1", x)
		}
		return strconv.AppendUint(b, x, 10), nil
	default:
		return nil, fmt.Errorf("canonical encode: unsupported type %T", v)
	}
}

func appendCanonObject(b []byte, x map[string]any, depth int) ([]byte, error) {
	keys := make([]string, 0, len(x))
	for k := range x {
		keys = append(keys, k)
	}
	sort.Strings(keys) // Go string order is byte order.
	b = append(b, '{')
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		var err error
		if b, err = appendCanonString(b, k); err != nil {
			return nil, err
		}
		b = append(b, ':')
		if b, err = appendCanon(b, x[k], depth+1); err != nil {
			return nil, err
		}
	}
	return append(b, '}'), nil
}

// appendCanonString writes s with the §2.3.1 escapes: \" \\ \b \f \n \r \t,
// \u00xx (lowercase hex) for the other controls and DEL, \uxxxx for every
// value from U+0080, as a surrogate pair from U+10000, and every other
// printable ASCII byte as itself.
func appendCanonString(b []byte, s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, errors.New("canonical encode: string is not valid UTF-8")
	}
	b = append(b, '"')
	for _, r := range s {
		switch {
		case r == '"':
			b = append(b, '\\', '"')
		case r == '\\':
			b = append(b, '\\', '\\')
		case r == '\b':
			b = append(b, '\\', 'b')
		case r == '\f':
			b = append(b, '\\', 'f')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r == '\t':
			b = append(b, '\\', 't')
		case r >= 0x20 && r < 0x7f:
			b = utf8.AppendRune(b, r) // one byte: r is printable ASCII
		case r >= 0x10000:
			hi, lo := utf16.EncodeRune(r)
			b = appendUEscape(appendUEscape(b, hi), lo)
		default:
			b = appendUEscape(b, r)
		}
	}
	return append(b, '"'), nil
}

func appendUEscape(b []byte, r rune) []byte {
	const hexdigits = "0123456789abcdef"
	return append(b, '\\', 'u',
		hexdigits[r>>12&0xf], hexdigits[r>>8&0xf], hexdigits[r>>4&0xf], hexdigits[r&0xf])
}

// decodeCanon parses data strictly and requires that it is exactly the
// canonical encoding of the parsed value, followed by one LF when withLF is
// set. It refuses non-ASCII bytes, a duplicate key at any level (an explicit
// check, not left to the byte comparison), a lone surrogate escape, any value
// outside the v1 model and anything after the value. Every refusal wraps
// errNotCanonical.
func decodeCanon(data []byte, withLF bool) (any, error) {
	if len(data) > maxCanonSize {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", errNotCanonical, len(data), maxCanonSize)
	}
	for i, c := range data {
		if c >= 0x80 {
			return nil, fmt.Errorf("%w: non-ASCII byte 0x%02x at offset %d", errNotCanonical, c, i)
		}
	}
	body := data
	if withLF {
		if len(body) == 0 || body[len(body)-1] != '\n' {
			return nil, fmt.Errorf("%w: missing trailing newline", errNotCanonical)
		}
		body = body[:len(body)-1]
	}
	p := &canonParser{data: body}
	p.skipSpace()
	v, err := p.value(0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotCanonical, err)
	}
	p.skipSpace()
	if p.pos != len(p.data) {
		return nil, fmt.Errorf("%w: trailing data at offset %d", errNotCanonical, p.pos)
	}
	again, err := encodeCanon(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotCanonical, err)
	}
	if string(again) != string(body) {
		return nil, fmt.Errorf("%w: bytes differ from the canonical encoding", errNotCanonical)
	}
	return v, nil
}

// canonParser is a strict JSON parser for the v1 value model. It accepts JSON
// whitespace so that an indented file is reported by the byte comparison as
// not canonical rather than as a syntax error; the comparison refuses it
// either way.
type canonParser struct {
	data []byte
	pos  int
}

func (p *canonParser) skipSpace() {
	for p.pos < len(p.data) {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *canonParser) errorf(format string, args ...any) error {
	return fmt.Errorf("offset %d: %s", p.pos, fmt.Sprintf(format, args...))
}

func (p *canonParser) value(depth int) (any, error) {
	if depth > maxCanonDepth {
		return nil, p.errorf("nesting deeper than %d", maxCanonDepth)
	}
	if p.pos >= len(p.data) {
		return nil, p.errorf("unexpected end")
	}
	switch c := p.data[p.pos]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case c >= '0' && c <= '9':
		return p.number()
	default:
		return nil, p.errorf("value type not allowed (%q)", c)
	}
}

func (p *canonParser) object(depth int) (any, error) {
	p.pos++ // {
	obj := map[string]any{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == '}' {
		p.pos++
		return obj, nil
	}
	for {
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != '"' {
			return nil, p.errorf("object key expected")
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, dup := obj[k]; dup {
			return nil, p.errorf("duplicate key %q", k)
		}
		p.skipSpace()
		if p.pos >= len(p.data) || p.data[p.pos] != ':' {
			return nil, p.errorf("':' expected")
		}
		p.pos++
		p.skipSpace()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		obj[k] = v
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, p.errorf("unexpected end in object")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			return obj, nil
		default:
			return nil, p.errorf("',' or '}' expected")
		}
	}
}

func (p *canonParser) array(depth int) (any, error) {
	p.pos++ // [
	arr := []any{}
	p.skipSpace()
	if p.pos < len(p.data) && p.data[p.pos] == ']' {
		p.pos++
		return arr, nil
	}
	for {
		p.skipSpace()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skipSpace()
		if p.pos >= len(p.data) {
			return nil, p.errorf("unexpected end in array")
		}
		switch p.data[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			return arr, nil
		default:
			return nil, p.errorf("',' or ']' expected")
		}
	}
}

// number accepts only an unsigned decimal integer without leading zeros in
// 0..2^63-1. A fraction or exponent is a type the v1 model does not have.
func (p *canonParser) number() (any, error) {
	start := p.pos
	for p.pos < len(p.data) && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
		p.pos++
	}
	if p.pos < len(p.data) {
		switch p.data[p.pos] {
		case '.', 'e', 'E':
			return nil, p.errorf("fraction or exponent not allowed")
		}
	}
	digits := string(p.data[start:p.pos])
	if len(digits) > 1 && digits[0] == '0' {
		return nil, p.errorf("leading zero")
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || n > maxCanonInt {
		return nil, p.errorf("integer out of range")
	}
	return n, nil
}

func (p *canonParser) str() (string, error) {
	p.pos++ // "
	var out []rune
	for {
		if p.pos >= len(p.data) {
			return "", p.errorf("unterminated string")
		}
		c := p.data[p.pos]
		switch {
		case c == '"':
			p.pos++
			return string(out), nil
		case c < 0x20:
			return "", p.errorf("raw control byte 0x%02x in string", c)
		case c != '\\':
			out = append(out, rune(c))
			p.pos++
			continue
		}
		p.pos++ // backslash
		r, err := p.escape()
		if err != nil {
			return "", err
		}
		out = append(out, r)
	}
}

// escape reads one escape after its backslash.
func (p *canonParser) escape() (rune, error) {
	if p.pos >= len(p.data) {
		return 0, p.errorf("unterminated escape")
	}
	e := p.data[p.pos]
	p.pos++
	switch e {
	case '"', '\\', '/':
		return rune(e), nil
	case 'b':
		return '\b', nil
	case 'f':
		return '\f', nil
	case 'n':
		return '\n', nil
	case 'r':
		return '\r', nil
	case 't':
		return '\t', nil
	case 'u':
		return p.unicodeEscape()
	}
	return 0, p.errorf("invalid escape \\%c", e)
}

// unicodeEscape reads the four hex digits of a \u escape and, for a high
// surrogate, the escaped low surrogate that must follow. A lone surrogate is
// refused.
func (p *canonParser) unicodeEscape() (rune, error) {
	r, err := p.hex4()
	if err != nil {
		return 0, err
	}
	switch {
	case utf16.IsSurrogate(r) && r < 0xdc00:
		if p.pos+1 >= len(p.data) || p.data[p.pos] != '\\' || p.data[p.pos+1] != 'u' {
			return 0, p.errorf("lone surrogate \\u%04x", r)
		}
		p.pos += 2
		lo, err := p.hex4()
		if err != nil {
			return 0, err
		}
		if lo < 0xdc00 || lo > 0xdfff {
			return 0, p.errorf("lone surrogate \\u%04x", r)
		}
		return utf16.DecodeRune(r, lo), nil
	case utf16.IsSurrogate(r):
		return 0, p.errorf("lone surrogate \\u%04x", r)
	}
	return r, nil
}

// hex4 reads four hex digits (either case; the byte comparison refuses
// uppercase later).
func (p *canonParser) hex4() (rune, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.errorf("short \\u escape")
	}
	var r rune
	for _, c := range p.data[p.pos : p.pos+4] {
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, p.errorf("bad \\u escape")
		}
		r = r<<4 | d
	}
	p.pos += 4
	return r, nil
}
