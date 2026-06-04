package tui

import (
	"strings"
	"testing"
)

// TestQuitRoutesToSharedChannels verifies a QUIT (which names no channel) is
// shown in every channel buffer the quitter shared with us — not the server
// buffer — using the channels the client attaches to the event.
func TestQuitRoutesToSharedChannels(t *testing.T) {
	m := newTestModel()
	m.ensureBuffer("#a", BufferChannel)
	m.ensureBuffer("#b", BufferChannel)
	server := m.buffers[0]
	beforeServer := len(server.lines)

	ev := evt(t, ":bob!b@h QUIT :bye").WithChannels([]string{"#a", "#b"})
	m = routeEvent(m, ev)

	for _, name := range []string{"#a", "#b"} {
		b := m.buffer(name)
		if b == nil || len(b.lines) != 1 {
			t.Errorf("channel %s did not get the quit line (lines=%v)", name, b)
			continue
		}
		if !strings.Contains(stripANSI(b.lines[0]), "bob") {
			t.Errorf("%s quit line missing nick: %q", name, b.lines[0])
		}
	}
	if len(server.lines) != beforeServer {
		t.Errorf("quit leaked into the server buffer (+%d lines)", len(server.lines)-beforeServer)
	}
}

// TestQuitFallsBackToServerWhenNoBuffer: if the quitter's shared channels have
// no open buffer, the notice still appears (in the server buffer) rather than
// vanishing.
func TestQuitFallsBackToServerWhenNoBuffer(t *testing.T) {
	m := newTestModel()
	server := m.buffers[0]
	before := len(server.lines)

	ev := evt(t, ":bob!b@h QUIT :bye").WithChannels([]string{"#gone"})
	m = routeEvent(m, ev)

	if len(server.lines) != before+1 {
		t.Errorf("quit with no open shared buffer should land in server buffer; lines %d -> %d", before, len(server.lines))
	}
}

// TestNickRoutesToSharedChannels verifies a NICK change fans out to the user's
// channels the same way.
func TestNickRoutesToSharedChannels(t *testing.T) {
	m := newTestModel()
	m.ensureBuffer("#a", BufferChannel)
	ev := evt(t, ":bob!b@h NICK bobby").WithChannels([]string{"#a"})
	m = routeEvent(m, ev)
	b := m.buffer("#a")
	if b == nil || len(b.lines) != 1 {
		t.Fatalf("#a did not get the nick line")
	}
}

// TestNicklistStartKeepsSelectionVisible checks the scroll window math: the
// returned start always keeps sel within [start, start+rows).
func TestNicklistStartKeepsSelectionVisible(t *testing.T) {
	const n, rows = 100, 10
	for sel := 0; sel < n; sel++ {
		start := nicklistStart(sel, n, rows, true)
		if start < 0 || start > n-rows {
			t.Fatalf("sel=%d: start %d out of range [0,%d]", sel, start, n-rows)
		}
		if sel < start || sel >= start+rows {
			t.Fatalf("sel=%d not visible in window [%d,%d)", sel, start, start+rows)
		}
	}
}

// TestNicklistStartNoScrollWhenFits / unfocused renders from the top.
func TestNicklistStartNoScrollWhenFits(t *testing.T) {
	if got := nicklistStart(40, 50, 50, true); got != 0 {
		t.Errorf("list fits: start = %d, want 0", got)
	}
	if got := nicklistStart(40, 100, 10, false); got != 0 {
		t.Errorf("unfocused: start = %d, want 0", got)
	}
}

// TestNicklistTitleScrollHint shows the off-screen counts.
func TestNicklistTitleScrollHint(t *testing.T) {
	// window [10,20) of 100 -> 10 above, 80 below.
	got := nicklistTitle(100, 10, 20)
	if !strings.Contains(got, "↑10") || !strings.Contains(got, "↓80") {
		t.Errorf("title = %q, want ↑10 and ↓80", got)
	}
	// At the top, no up-arrow.
	if got := nicklistTitle(100, 0, 20); strings.Contains(got, "↑") {
		t.Errorf("title at top should not show ↑: %q", got)
	}
}
