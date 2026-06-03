package client

import "strings"

// SanitizeTerminal neutralizes terminal control sequences in attacker-controlled
// text before a front-end writes it to the terminal. IRC message bodies, nicks,
// topics, and reasons are all peer-supplied, and the wire parser only rejects
// NUL/CR/LF (which would break framing) — so ESC (0x1b) and the other C0/C1
// control bytes that introduce ANSI/OSC/DCS sequences still survive in the text
// an Event exposes. Written verbatim to a terminal, a hostile peer could rewrite
// the window title, drive the clipboard (OSC 52), move the cursor to forge UI,
// or emit query sequences whose replies are injected back into the user's input.
//
// SanitizeTerminal drops every Unicode control character — the C0 range
// (0x00–0x1F), DEL (0x7F), and the C1 range (0x80–0x9F), which some terminals
// treat as single-byte CSI/OSC introducers — so no ESC survives to begin a
// sequence; a horizontal tab is mapped to a single space to preserve word
// separation. Printable Unicode (emoji, non-Latin scripts) passes through
// untouched. CTCP framing (\x01) must be parsed off before sanitizing, since it
// too is a control byte.
//
// It is the single shared implementation used by every Lurk front-end (the
// Bubble Tea TUI and the -plain line client). Apply it to raw text *before* any
// styling/escape wrapping, never after, or it would strip the renderer's own SGR
// escapes.
func SanitizeTerminal(s string) string {
	if strings.IndexFunc(s, unsafeControl) < 0 {
		return s // fast path: nothing to strip
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if unsafeControl(r) {
			return -1 // drop
		}
		return r
	}, s)
}

// unsafeControl reports whether r is a control character that must not reach the
// terminal verbatim: a C0 control or DEL, or a C1 control. Tab is included
// (SanitizeTerminal maps it to a space before this is consulted as a drop
// predicate).
func unsafeControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}
