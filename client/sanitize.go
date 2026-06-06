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
		if whitespaceSeparator(r) {
			return ' ' // collapse tab / line / paragraph separators to a space
		}
		if unsafeControl(r) {
			return -1 // drop
		}
		return r
	}, s)
}

// ircFormatting reports whether r is one of the inline IRC formatting control
// bytes that clients interpret as styling (not as a terminal escape): bold,
// colour, hex-colour, monospace, reverse, italic, strikethrough, underline, and
// the reset/plain byte. They live in the C0 range but are legitimate IRC content,
// so SanitizeForRelay preserves them where SanitizeTerminal (terminal-bound) drops
// them.
func ircFormatting(r rune) bool {
	switch r {
	case 0x02, // bold (STX)
		0x03, // colour (ETX)
		0x04, // hex colour (EOT)
		0x0f, // reset / plain (SI)
		0x11, // monospace (DC1)
		0x16, // reverse (SYN)
		0x1d, // italic (GS)
		0x1e, // strikethrough (RS)
		0x1f: // underline (US)
		return true
	}
	return false
}

// SanitizeForRelay neutralizes terminal-hijacking control sequences in
// peer-supplied text while PRESERVING the inline IRC formatting controls
// (bold/colour/italic/underline/strikethrough/monospace/reverse/reset). Use it
// when text will be stored or relayed onward to another IRC client — a bouncer's
// backlog and its live relay — because that client renders IRC formatting itself
// and is responsible for its own terminal safety. For text written directly to
// this process's own terminal, use SanitizeTerminal instead, which additionally
// strips the formatting bytes.
//
// It drops ESC (0x1b) and every other C0 control except the IRC formatting set
// and the horizontal tab, drops DEL (0x7f) and the C1 range (0x80–0x9f) — so no
// ANSI/OSC/CSI/DCS sequence can survive to attack a downstream terminal — and
// drops the Trojan-Source bidirectional formatting controls (CVE-2021-42574),
// which spoof rendered order in any client, IRC-aware or not. NUL/CR/LF never
// reach here (the wire parser rejects them) but would be dropped regardless.
// Printable Unicode and the IRC formatting bytes pass through untouched, so a
// round-trip preserves the message's styling exactly.
func SanitizeForRelay(s string) string {
	if strings.IndexFunc(s, unsafeForRelay) < 0 {
		return s // fast path: nothing to strip
	}
	return strings.Map(func(r rune) rune {
		if unsafeForRelay(r) {
			return -1 // drop
		}
		return r
	}, s)
}

// unsafeForRelay reports whether r must be stripped before text is relayed to or
// stored for another IRC client: a C0 control or DEL that is not an IRC
// formatting byte and not a tab, a C1 control, or a bidirectional formatting
// control. Unlike unsafeControl it keeps the IRC formatting bytes and the tab.
func unsafeForRelay(r rune) bool {
	if r == '\t' || ircFormatting(r) {
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || bidiControl(r)
}

// whitespaceSeparator reports whether r is a whitespace separator that should
// collapse to a single space rather than be dropped: the horizontal tab, and the
// Unicode line (U+2028) and paragraph (U+2029) separators. Mapping them to a
// space preserves word boundaries while preventing a single logical message from
// being split across visual lines (a layout-spoofing vector in renderers that
// honor U+2028/U+2029).
func whitespaceSeparator(r rune) bool {
	return r == '\t' || r == 0x2028 || r == 0x2029
}

// unsafeControl reports whether r is a character that must not reach the
// terminal verbatim: a C0 control or DEL, a C1 control, a bidirectional
// formatting control, or a line/paragraph separator. The whitespace separators
// (tab, U+2028, U+2029) are reported here so the fast path detects them;
// SanitizeTerminal maps them to a space before this is consulted as a drop
// predicate.
func unsafeControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || bidiControl(r) || whitespaceSeparator(r)
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
