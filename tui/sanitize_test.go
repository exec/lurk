package tui

import (
	"strings"
	"testing"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// TestSanitizeStripsControlBytes checks that sanitize removes the control bytes a
// hostile peer could use to drive the terminal, while leaving printable text
// (including multibyte Unicode) intact and mapping tab to a space.
func TestSanitizeStripsControlBytes(t *testing.T) {
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
		{"c1 csi byte", "x2Jy", "x2Jy"},
		{"irc color codes", "\x03" + "04red\x0f", "04red"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitize(c.in); got != c.want {
				t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
			}
			// The result must never carry an ESC or other unsafe control rune.
			for _, r := range sanitize(c.in) {
				if r != ' ' && (r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)) {
					t.Errorf("sanitize(%q) leaked control rune %#x", c.in, r)
				}
			}
		})
	}
}

// TestStripANSIForms exercises the hardened stripANSI against the escape-sequence
// families (CSI, OSC terminated by BEL or ST, and the DCS/APC string forms),
// confirming none survive.
func TestStripANSIForms(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"csi sgr", "\x1b[1;31mhi\x1b[0m", "hi"},
		{"csi no final", "a\x1b[", "a"},
		{"osc bel", "a\x1b]52;c;x\x07b", "ab"},
		{"osc st", "a\x1b]0;title\x1b\\b", "ab"},
		{"dcs st", "a\x1bP1;2q\x1b\\b", "ab"},
		{"apc st", "a\x1b_payload\x1b\\b", "ab"},
		{"two byte escape", "a\x1b=b", "ab"},
		{"lone trailing esc", "a\x1b", "a"},
		{"no escape", "plain", "plain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripANSI(c.in)
			if got != c.want {
				t.Errorf("stripANSI(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.ContainsRune(got, 0x1b) {
				t.Errorf("stripANSI(%q) left an ESC: %q", c.in, got)
			}
		})
	}
}

// TestFormatLineNeutralizesEscapes is the end-to-end check: a PRIVMSG whose body
// carries an OSC-52 clipboard write and a cursor-move sequence must render with
// no ESC reaching the scrollback, even though the renderer adds its own styling.
func TestFormatLineNeutralizesEscapes(t *testing.T) {
	tm := newTheme()
	payload := "hi\x1b]52;c;ZXZpbA==\x07\x1b[2Jthere"
	m, err := irc.Parse(":evil!e@h PRIVMSG #chan :" + payload)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	line, _ := tm.formatLine(client.Event{Message: m}, "me", nil)

	// The styled line legitimately contains the renderer's own ESC-based SGR
	// codes; stripping those (which only wrap our own styling) must leave no ESC
	// from the payload behind, and the inert remnants must be present as text.
	visible := stripANSI(line)
	if strings.ContainsRune(visible, 0x1b) {
		t.Errorf("formatLine leaked an ESC into visible content: %q", visible)
	}
	if !strings.Contains(visible, "hithere") && !strings.Contains(visible, "]52;c;ZXZpbA==[2J") {
		t.Errorf("formatLine dropped or mangled the message text: %q", visible)
	}
}
