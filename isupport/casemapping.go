package isupport

import "strings"

// CaseMapping identifies how a server folds nicknames and channel names for
// case-insensitive comparison. IRC names are compared case-insensitively, but
// "case" is defined by the server's advertised CASEMAPPING token rather than by
// Unicode, so all equality checks and map keys for nicks/channels must route
// through Fold using the server's mapping.
//
// CaseMapping is a comparable value type; copying it is safe and cheap.
type CaseMapping int

const (
	// CaseRFC1459 folds ASCII A–Z to a–z and additionally treats the brace
	// characters as the lowercase of their bracket counterparts:
	// '[' '\' ']' fold to '{' '|' '}', and '^' folds to '~'. This is the
	// historical default and is assumed when no CASEMAPPING token is present.
	CaseRFC1459 CaseMapping = iota

	// CaseRFC1459Strict is like CaseRFC1459 but omits the '^' <-> '~' pairing,
	// folding only '[' '\' ']' to '{' '|' '}'.
	CaseRFC1459Strict

	// CaseASCII folds only ASCII A–Z to a–z, leaving all other bytes untouched.
	CaseASCII
)

// CaseMapping returns the server's active case mapping, derived from the
// CASEMAPPING token. Unrecognised or absent values default to CaseRFC1459, the
// historical IRC default.
func (s ISupport) CaseMapping() CaseMapping {
	v, ok := s.Get("CASEMAPPING")
	if !ok {
		return CaseRFC1459
	}
	switch strings.ToLower(v) {
	case "ascii":
		return CaseASCII
	case "rfc1459-strict":
		return CaseRFC1459Strict
	case "rfc1459":
		return CaseRFC1459
	default:
		return CaseRFC1459
	}
}

// String returns the canonical CASEMAPPING token value for the mapping.
func (c CaseMapping) String() string {
	switch c {
	case CaseASCII:
		return "ascii"
	case CaseRFC1459Strict:
		return "rfc1459-strict"
	default:
		return "rfc1459"
	}
}

// Fold returns the case-folded form of s under the mapping. Two names refer to
// the same nick or channel exactly when their folded forms are byte-for-byte
// equal, so Fold(a) == Fold(b) is the canonical equality test and Fold(name) is
// the canonical map key.
//
// Folding operates byte-wise on the ASCII range only; bytes >= 0x80 (e.g. UTF-8
// continuation bytes) pass through unchanged, which is the intended behaviour
// for all three mappings.
func (c CaseMapping) Fold(s string) string {
	// Fast path: if nothing in s needs folding, return it unchanged to avoid an
	// allocation — the common case for already-lowercase names.
	if !c.needsFold(s) {
		return s
	}
	b := []byte(s)
	for i := range b {
		b[i] = c.foldByte(b[i])
	}
	return string(b)
}

// needsFold reports whether any byte of s would change under the mapping.
func (c CaseMapping) needsFold(s string) bool {
	for i := 0; i < len(s); i++ {
		if c.foldByte(s[i]) != s[i] {
			return true
		}
	}
	return false
}

// foldByte folds a single byte according to the mapping.
func (c CaseMapping) foldByte(ch byte) byte {
	if ch >= 'A' && ch <= 'Z' {
		return ch + ('a' - 'A')
	}
	switch c {
	case CaseRFC1459:
		// '[' ']' '\\' '^'  ->  '{' '}' '|' '~'
		switch ch {
		case '[':
			return '{'
		case ']':
			return '}'
		case '\\':
			return '|'
		case '^':
			return '~'
		}
	case CaseRFC1459Strict:
		// Like rfc1459 but without the '^' -> '~' pairing.
		switch ch {
		case '[':
			return '{'
		case ']':
			return '}'
		case '\\':
			return '|'
		}
	}
	return ch
}
