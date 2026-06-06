package client

import (
	"strings"
	"testing"
)

// TestSanitizeTerminal checks that the shared sanitizer strips the control bytes
// a hostile peer could use to drive the terminal (ESC-introduced ANSI/OSC, BEL,
// backspace, C1 introducers) while preserving printable text, including
// multibyte Unicode, and mapping tab to a space.
func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"unicode kept", "héllo 🌍 こんにちは", "héllo 🌍 こんにちは"},
		{"esc csi cursor move", "before\x1b[2Jafter", "before[2Jafter"},
		{"osc52 clipboard", "x\x1b]52;c;ZXZpbA==\x07y", "x]52;c;ZXZpbA==y"},
		{"osc window title", "\x1b]0;pwned\x07", "]0;pwned"},
		{"bel", "ding\x07ding", "dingding"},
		{"backspace", "ab\x08c", "abc"},
		{"tab to space", "a\tb", "a b"},
		{"irc color codes", "\x03" + "04red\x0f", "04red"},
		// Bidirectional formatting controls (Trojan Source, CVE-2021-42574) are
		// invisible but reorder displayed text; they must be dropped.
		{"rlo override", "user‮gniteehc", "usergniteehc"},
		{"isolate spoof", "⁦alice⁩ ⁨", "alice "},
		{"lrm/rlm/alm marks", "a‎b‏c؜d", "abcd"},
		{"natural rtl kept", "مرحبا 🌍", "مرحبا 🌍"},
		// Unicode line/paragraph separators collapse to a space so a single message
		// cannot be split across visual lines.
		{"line separator", "a b", "a b"},
		{"paragraph separator", "a b", "a b"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizeTerminal(c.in)
			if got != c.want {
				t.Errorf("SanitizeTerminal(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.ContainsRune(got, 0x1b) {
				t.Errorf("SanitizeTerminal(%q) leaked an ESC: %q", c.in, got)
			}
		})
	}
}

// TestSanitizeForRelay checks the relay/storage sanitizer: it strips
// terminal-hijacking escapes (ESC, C1, DEL, bidi) but PRESERVES the inline IRC
// formatting controls and the tab, unlike SanitizeTerminal.
func TestSanitizeForRelay(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "hello world", "hello world"},
		{"keeps bold and reset", "\x02bold\x0f", "\x02bold\x0f"},
		{"keeps colour", "\x033,4coloured\x03", "\x033,4coloured\x03"},
		{"keeps italic/underline/strike", "\x1ditalic\x1f under \x1estrike", "\x1ditalic\x1f under \x1estrike"},
		{"keeps monospace and reverse", "\x11mono\x11 \x16rev\x16", "\x11mono\x11 \x16rev\x16"},
		{"keeps hex colour", "\x04FF0000red\x04", "\x04FF0000red\x04"},
		{"keeps tab", "a\tb", "a\tb"},
		{"strips ESC, leaves the rest literal", "\x1b[31mred\x1b[0m", "[31mred[0m"},
		{"strips BEL and backspace", "a\x07\x08b", "ab"},
		{"strips DEL", "a\x7fb", "ab"},
		{"strips C1 CSI (valid codepoint)", "a\u009bb", "ab"},
		{"strips Trojan-Source bidi", "admin‮nimda", "adminnimda"},
		{"drops NUL-class but never present on wire", "a\x00b", "ab"},
		{"keeps emoji and rtl text", "héllo 😀 שלום", "héllo 😀 שלום"},
		{"mixed: keep formatting, drop escape", "\x02bold\x1b]0;title\x07\x02", "\x02bold]0;title\x02"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizeForRelay(c.in); got != c.want {
				t.Errorf("SanitizeForRelay(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestSanitizeForRelayVsTerminal documents the key difference: the formatting
// bytes survive a relay-sanitize but not a terminal-sanitize, while both strip
// ESC.
func TestSanitizeForRelayVsTerminal(t *testing.T) {
	in := "\x02bold\x03 \x1b[31m"
	relay := SanitizeForRelay(in)
	term := SanitizeTerminal(in)
	if !strings.ContainsRune(relay, 0x02) || !strings.ContainsRune(relay, 0x03) {
		t.Errorf("relay sanitize should keep IRC formatting: %q", relay)
	}
	if strings.ContainsRune(term, 0x02) || strings.ContainsRune(term, 0x03) {
		t.Errorf("terminal sanitize should drop IRC formatting: %q", term)
	}
	if strings.ContainsRune(relay, 0x1b) || strings.ContainsRune(term, 0x1b) {
		t.Errorf("both must strip ESC; relay=%q term=%q", relay, term)
	}
}
