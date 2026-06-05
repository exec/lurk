package tui

import (
	"strings"
	"testing"

	"github.com/exec/lurk/client"
)

// escNick is a malicious nick/channel fragment: a benign-looking prefix followed
// by an OSC sequence (ESC ] 0 ; ... BEL) that would set the terminal title or,
// via OSC 52, write the clipboard if it ever reached the terminal raw.
const escNick = "a\x1b]0;pwned\x07b"

// TestNickRowSanitizesEscape proves a member nick carrying ESC cannot inject a
// terminal escape through the nicklist. The 0x1b byte must be gone while the
// visible characters survive. Covers selected, unselected, and prefixed rows
// since each takes a different formatting path in nickRow.
func TestNickRowSanitizesEscape(t *testing.T) {
	tm := newTheme()
	cases := []struct {
		name     string
		mem      client.Member
		selected bool
	}{
		{"unselected", client.Member{Nick: escNick}, false},
		{"selected", client.Member{Nick: escNick}, true},
		{"prefixed", client.Member{Nick: escNick, Prefixes: "@"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tm.nickRow(tc.mem, 40, tc.selected, false)
			if strings.ContainsRune(out, 0x1b) {
				// 0x1b also delimits the lipgloss styling, so strip ANSI first
				// to be sure the survivor is real styling, not the payload.
				if strings.Contains(stripANSI(out), "\x1b") {
					t.Fatalf("nickRow output still contains ESC after stripANSI: %q", out)
				}
			}
			vis := stripANSI(out)
			if !strings.Contains(vis, "a") || !strings.Contains(vis, "pwned") {
				t.Fatalf("visible characters lost: %q", vis)
			}
		})
	}
}

// TestSidebarSanitizesTitleEscape proves a buffer Title (channel name / PM peer
// nick) carrying ESC cannot inject a terminal escape through the sidebar.
func TestSidebarSanitizesTitleEscape(t *testing.T) {
	m := newTestModel()
	chanName := "#" + escNick
	m.ensureBuffer(chanName, BufferChannel)

	out := renderSidebar(m, sidebarWidth, 20)
	if strings.Contains(stripANSI(out), "\x1b") {
		t.Fatalf("sidebar still contains ESC after stripANSI: %q", out)
	}
	vis := stripANSI(out)
	if !strings.Contains(vis, "pwned") {
		t.Fatalf("sanitized channel name lost its visible text:\n%s", vis)
	}
}

// TestViewSanitizesEscapeFromEvents drives the model from a real PRIVMSG, opening
// a PM buffer whose Title is the ESC-bearing peer taken straight from the wire.
// Rendering the sidebar must emit no raw ESC payload while keeping the visible
// name — proving the escape can't reach the terminal via the event path.
func TestViewSanitizesEscapeFromEvents(t *testing.T) {
	m := newTestModel()
	// A PM addressed to the ESC-bearing nick (with a nil client, self is empty so
	// this routes to a PM buffer titled with the raw target).
	ev := evt(t, ":srv!s@h PRIVMSG "+escNick+" :hi there")
	m = routeEvent(m, ev)

	out := stripANSI(renderSidebar(m, sidebarWidth, 20))
	if strings.Contains(out, "\x1b") {
		t.Fatalf("sidebar from event still contains ESC: %q", out)
	}
	if !strings.Contains(out, "pwned") {
		t.Fatalf("PM peer title lost its visible text:\n%s", out)
	}
}
