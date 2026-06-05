package tui

import (
	"strings"
	"testing"
)

func TestReadMarker(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)

	_, ia := m.ensureBuffer("#a", BufferChannel)
	_, ib := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(ia)
	m = layout(m)

	// Two messages read while viewing #a.
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :one"))
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :two"))

	// Leave to #b: #a's marker should pin at the current line count.
	m.switchTo(ib)
	if got := m.buffer("#a").readMarker; got != 2 {
		t.Fatalf("readMarker after leaving = %d, want 2", got)
	}

	// New traffic arrives in #a while we're away.
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :three"))
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :four"))

	out := stripANSI(m.buffer("#a").wrapped())
	if !strings.Contains(out, "new messages") {
		t.Errorf("returning to #a should show a new-messages divider:\n%s", out)
	}
	// The divider sits between the read and unread lines.
	if i, j := strings.Index(out, "two"), strings.Index(out, "new messages"); i < 0 || j < i {
		t.Errorf("divider misplaced relative to read content:\n%s", out)
	}
	if i, j := strings.Index(out, "new messages"), strings.Index(out, "three"); i < 0 || j < i {
		t.Errorf("divider should precede the unread lines:\n%s", out)
	}
}

// TestReadMarkerNoneWhenCaughtUp verifies no divider when there's nothing new.
func TestReadMarkerNoneWhenCaughtUp(t *testing.T) {
	m := newTestModel()
	_, ia := m.ensureBuffer("#a", BufferChannel)
	_, ib := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(ia)
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :one"))
	m.switchTo(ib) // marker = 1, len = 1
	m.switchTo(ia) // back, still nothing new
	if strings.Contains(stripANSI(m.buffer("#a").wrapped()), "new messages") {
		t.Error("no divider expected when caught up")
	}
}

// TestReadMarkerSurvivesTrim verifies the marker shifts with scrollback trimming.
func TestReadMarkerSurvivesTrim(t *testing.T) {
	b := newBuffer("#x", BufferChannel)
	b.readMarker = 5
	// Fill past the cap so the front is trimmed.
	for i := 0; i < scrollbackLimit+10; i++ {
		b.addLine("line")
	}
	if b.readMarker < 0 || b.readMarker > len(b.lines) {
		t.Errorf("readMarker out of range after trim: %d (len %d)", b.readMarker, len(b.lines))
	}
}
