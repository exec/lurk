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

// TestEmptyHintSanitizesTitle proves a PM or channel buffer whose Title carries
// terminal escape sequences cannot inject them through the empty-buffer hint.
// This is the render path through emptyHint → renderBody → lipgloss.Place,
// which previously passed b.Title raw to the renderer.
func TestEmptyHintSanitizesTitle(t *testing.T) {
	cases := []struct {
		name  string
		kind  BufferKind
		title string
	}{
		{"PM escape", BufferPM, escNick},
		{"channel escape", BufferChannel, "#" + escNick},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Buffer{Kind: tc.kind, Title: tc.title}
			hint := emptyHint(b)
			if strings.Contains(stripANSI(hint), "\x1b") {
				t.Fatalf("emptyHint still contains ESC after stripANSI: %q", hint)
			}
			// The inert visible remnants of the payload (e.g. "]0;pwned") must
			// survive so the hint still names the buffer.
			vis := stripANSI(hint)
			if !strings.Contains(vis, "pwned") {
				t.Fatalf("emptyHint lost visible text from title: %q", vis)
			}
		})
	}
}

// TestEmptyHintTruncatesWideTitle verifies that a server-controlled title of
// thousands of full-width grapheme clusters does not produce an unbounded
// string in the empty-buffer hint. The title width after truncation must not
// exceed emptyHintTitleMax display columns.
func TestEmptyHintTruncatesWideTitle(t *testing.T) {
	// Each '你' is 2 display columns wide; 2500 × 2 = 5000 columns total.
	wideTitle := strings.Repeat("你", 2500)
	cases := []struct {
		name string
		kind BufferKind
	}{
		{"PM", BufferPM},
		{"channel", BufferChannel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &Buffer{Kind: tc.kind, Title: wideTitle}
			hint := emptyHint(b)
			vis := stripANSI(hint)
			// The hint prefix ("No messages yet — say hello to " etc.) plus the
			// capped title must fit well below a sane maximum; we check that the
			// raw hint length is not pathologically large.
			if len(vis) > 256 {
				t.Fatalf("emptyHint produced an oversized hint (%d bytes); title was not truncated", len(vis))
			}
		})
	}
}

// TestEmptyHintRenderBodyESCFree exercises the full renderBody path with an
// empty PM buffer whose Title carries an OSC escape, proving the fix reaches
// the actual render output (not just the string returned by emptyHint).
func TestEmptyHintRenderBodyESCFree(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	// Open an empty PM buffer with a malicious title and switch to it.
	_, i := m.ensureBuffer(escNick, BufferPM)
	m.switchTo(i)
	m = layout(m)

	// renderBody is the composition layer that calls emptyHint; check its output.
	b := m.activeBuffer()
	if len(b.lines) != 0 {
		t.Skip("buffer is not empty — hint path not exercised")
	}
	bodyW, _ := paneWidths(m.width, false)
	_, bodyH := verticalLayout(m)
	out := renderBody(m, bodyW, bodyH)
	if strings.Contains(stripANSI(out), "\x1b") {
		t.Fatalf("renderBody(empty PM with ESC title) leaked ESC: %q", stripANSI(out))
	}
}
