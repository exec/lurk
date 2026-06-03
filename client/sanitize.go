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
// separation. It also drops the Unicode bidirectional formatting controls (the
// "Trojan Source" set, CVE-2021-42574), which are invisible but reorder how
// surrounding text is displayed: a hostile nick, message, topic, or reason could
// otherwise make the rendered text read differently from its logical content
// (masquerade as another user, or hide part of a URL). Printable Unicode (emoji,
// non-Latin scripts including natural right-to-left text) passes through
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

// unsafeControl reports whether r is a character that must not reach the
// terminal verbatim: a C0 control or DEL, a C1 control, or a bidirectional
// formatting control. Tab is included (SanitizeTerminal maps it to a space
// before this is consulted as a drop predicate).
func unsafeControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || bidiControl(r)
}

// bidiControl reports whether r is a Unicode bidirectional formatting control:
// the explicit embeddings/overrides (LRE/RLE/LRO/RLO and their PDF terminator),
// the isolates (LRI/RLI/FSI/PDI), and the directional marks (LRM/RLM/ALM). These
// characters are invisible but change the displayed order of the text around
// them, the mechanism behind "Trojan Source" visual-spoofing attacks. Natural
// right-to-left scripts (Arabic, Hebrew) lay out from their own strong
// directional characters and do not need these explicit controls, so dropping
// them defeats the spoof while leaving legitimate text rendering correctly.
func bidiControl(r rune) bool {
	switch r {
	case 0x061C, // ARABIC LETTER MARK
		0x200E, 0x200F, // LEFT-TO-RIGHT / RIGHT-TO-LEFT MARK
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // LRE, RLE, PDF, LRO, RLO
		0x2066, 0x2067, 0x2068, 0x2069: // LRI, RLI, FSI, PDI
		return true
	}
	return false
}
