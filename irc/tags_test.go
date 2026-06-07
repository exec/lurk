package irc

import (
	"fmt"
	"strings"
	"testing"
)

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

// TestParseTagsAllocsHint verifies that the map size-hint eliminates the
// internal grow-rehash for the common 2–4 tag case. With the hint, a 3-tag
// segment should allocate exactly 1 object (the map itself); without it,
// the runtime would need to grow the zero-size map, costing a second alloc.
func TestParseTagsAllocsHint(t *testing.T) {
	// A representative bouncer-feed segment: @time, @msgid, @account — three
	// tags, separated by two ';' characters, so hint == 3.
	seg := "time=2026-06-07T00:00:00.000Z;msgid=abcdef123456;account=alice"

	allocs := testing.AllocsPerRun(100, func() {
		_ = parseTags(seg)
	})
	// The map itself is 1 allocation. With the correct size hint the runtime
	// has no need to grow (rehash) the map during insertion, so the count must
	// stay at 1. If the hint is missing or wrong, the grow step adds a second
	// allocation and this assertion catches the regression.
	if allocs > 1 {
		t.Errorf("parseTags allocated %.0f objects for a 3-tag segment, want 1 (map only)", allocs)
	}
}

// BenchmarkParseTags measures allocation and throughput for a typical
// 3-tag bouncer-feed segment with the size hint in place.
func BenchmarkParseTags(b *testing.B) {
	seg := "time=2026-06-07T00:00:00.000Z;msgid=abcdef123456;account=alice"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = parseTags(seg)
	}
}

// TestParseTagsCountCap verifies that parseTags never stores more than
// maxTagCount entries regardless of how many are present in the segment.
// A hostile server can fill an 8189-byte tag budget with ~2729 distinct
// 3-byte keys; without a cap, every buffered message allocates a large map.
func TestParseTagsCountCap(t *testing.T) {
	// Build a segment with maxTagCount+50 unique keys ("aa", "ab", ...).
	// Each key is 2 ASCII characters; none repeat, so without the cap every
	// one would land in the map.
	total := maxTagCount + 50
	keys := make([]string, 0, total)
	chars := "abcdefghijklmnopqrstuvwxyz"
outer:
	for _, c1 := range chars {
		for _, c2 := range chars {
			keys = append(keys, fmt.Sprintf("%c%c", c1, c2))
			if len(keys) == total {
				break outer
			}
		}
	}
	segment := strings.Join(keys, ";")

	got := parseTags(segment)
	if len(got) > maxTagCount {
		t.Errorf("parseTags stored %d entries for a %d-key segment, want at most %d",
			len(got), total, maxTagCount)
	}
	// Confirm the first maxTagCount keys are all present (order is insertion-
	// order up to the cap, so the first maxTagCount unique keys must survive).
	for _, k := range keys[:maxTagCount] {
		if !got.Has(k) {
			t.Errorf("expected key %q to be present in the capped map", k)
		}
	}
}
