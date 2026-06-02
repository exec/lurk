package irc

import "testing"

func TestUnescapeTagValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{`a\:b`, "a;b"},
		{`a\sb`, "a b"},
		{`a\\b`, `a\b`},
		{`a\rb`, "a\rb"},
		{`a\nb`, "a\nb"},
		{`\s\s`, "  "},
		// Unknown escape: drop the backslash, keep the char.
		{`\x`, "x"},
		{`a\qb`, "aqb"},
		// Lone trailing backslash produces nothing.
		{`abc\`, "abc"},
		{`\`, ""},
		// Combined.
		{`a\sb\:c\\d`, `a b;c\d`},
		// Escaped backslash consumes the next char as data: `\\s` is `\`
		// followed by a literal 's', NOT an escaped space. This is the case
		// that trips up left-to-right implementations that don't consume two
		// bytes per backslash.
		{`\\s`, `\s`},
		{`\\:`, `\:`},
		{`\\\\`, `\\`},
		// CRLF escapes adjacent.
		{`\r\n`, "\r\n"},
		// Backslash before a multibyte/UTF-8 char: drop backslash, keep char.
		{`\é`, "é"},
	}
	for _, tt := range tests {
		if got := unescapeTagValue(tt.in); got != tt.want {
			t.Errorf("unescapeTagValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestEscapeTagValue(t *testing.T) {
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"a;b", `a\:b`},
		{"a b", `a\sb`},
		{`a\b`, `a\\b`},
		{"a\rb", `a\rb`},
		{"a\nb", `a\nb`},
		{`a b;c\d`, `a\sb\:c\\d`},
	}
	for _, tt := range tests {
		if got := escapeTagValue(tt.in); got != tt.want {
			t.Errorf("escapeTagValue(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestTagEscapeRoundTrip checks losslessness for the five spec characters.
func TestTagEscapeRoundTrip(t *testing.T) {
	values := []string{
		"", "plain", ";", " ", `\`, "\r", "\n",
		"all five ; \\ \r \n together",
		"semicolons;;;and spaces   ",
	}
	for _, v := range values {
		if got := unescapeTagValue(escapeTagValue(v)); got != v {
			t.Errorf("round trip %q -> %q", v, got)
		}
	}
}

func TestParseTags(t *testing.T) {
	got := parseTags(`aaa=bbb;ccc;example.com/ddd=eee;+typing=active`)
	want := Tags{
		"aaa":             "bbb",
		"ccc":             "",
		"example.com/ddd": "eee",
		"+typing":         "active",
	}
	if len(got) != len(want) {
		t.Fatalf("parseTags len = %d, want %d (%v)", len(got), len(want), got)
	}
	for k, v := range want {
		if gv, ok := got[k]; !ok || gv != v {
			t.Errorf("tag %q = %q (present=%v), want %q", k, gv, ok, v)
		}
	}
	if !got.Has("ccc") {
		t.Errorf("Has(ccc) = false, want true (value-less tag is present)")
	}
}
