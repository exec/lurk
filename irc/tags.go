package irc

import "strings"

// Tag escaping is defined by the IRCv3 message-tags specification. Only tag
// values are escaped on the wire; keys are not. The mapping is:
//
//	raw  ->  escaped
//	;        \:
//	space    \s
//	\        \\
//	CR       \r
//	LF       \n
//
// When unescaping, a backslash followed by any other character drops the
// backslash and keeps the character verbatim, and a lone trailing backslash
// produces nothing. Round-tripping a value through escapeTagValue and
// unescapeTagValue is lossless for the five characters above.

// unescapeTagValue decodes an on-the-wire tag value into its raw form.
func unescapeTagValue(s string) string {
	// Fast path: nothing to unescape.
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		// A lone trailing backslash produces nothing.
		if i == len(s)-1 {
			break
		}
		i++
		switch s[i] {
		case ':':
			b.WriteByte(';')
		case 's':
			b.WriteByte(' ')
		case '\\':
			b.WriteByte('\\')
		case 'r':
			b.WriteByte('\r')
		case 'n':
			b.WriteByte('\n')
		default:
			// Unknown escape: drop the backslash, keep the character.
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// escapeTagValue encodes a raw tag value for the wire.
func escapeTagValue(s string) string {
	if !strings.ContainsAny(s, ";  \\\r\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ';':
			b.WriteString(`\:`)
		case ' ':
			b.WriteString(`\s`)
		case '\\':
			b.WriteString(`\\`)
		case '\r':
			b.WriteString(`\r`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// parseTags parses the tag segment (without the leading '@') into a Tags map.
// Keys are stored as-is, retaining any '+' client-only prefix and vendor/
// prefix; values are unescaped. A tag with no '=' has the empty string value
// but is still present (see Tags.Has).
func parseTags(segment string) Tags {
	tags := make(Tags)
	for _, raw := range strings.Split(segment, ";") {
		if raw == "" {
			// Tolerate empty entries (e.g. a trailing or doubled ';').
			continue
		}
		key, val, hasEq := strings.Cut(raw, "=")
		if key == "" {
			continue
		}
		if hasEq {
			tags[key] = unescapeTagValue(val)
		} else {
			tags[key] = ""
		}
	}
	return tags
}

// serializeTags renders Tags into the on-the-wire segment (without the leading
// '@'). Keys are emitted as stored; values are escaped. A value-less tag (empty
// string) is emitted as a bare key with no '='. Iteration order follows Go map
// ordering, which is unspecified — tag order is not semantically significant.
func serializeTags(t Tags) string {
	var b strings.Builder
	first := true
	for k, v := range t {
		if !first {
			b.WriteByte(';')
		}
		first = false
		b.WriteString(k)
		if v != "" {
			b.WriteByte('=')
			b.WriteString(escapeTagValue(v))
		}
	}
	return b.String()
}
