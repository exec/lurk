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
