package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestAltDigitIndex(t *testing.T) {
	cases := []struct {
		key     tea.KeyPressMsg
		wantIdx int
		wantOK  bool
	}{
		{tea.KeyPressMsg{Code: '1', Text: "1", Mod: tea.ModAlt}, 0, true},
		{tea.KeyPressMsg{Code: '9', Text: "9", Mod: tea.ModAlt}, 8, true},
		{tea.KeyPressMsg{Code: '0', Text: "0", Mod: tea.ModAlt}, 0, false}, // Alt+0 unmapped
		{tea.KeyPressMsg{Code: 'a', Text: "a", Mod: tea.ModAlt}, 0, false},
		{tea.KeyPressMsg{Code: '1', Text: "1"}, 0, false}, // bare 1, no Alt
	}
	for _, c := range cases {
		idx, ok := altDigitIndex(c.key)
		if ok != c.wantOK || (ok && idx != c.wantIdx) {
			t.Errorf("altDigitIndex(%q) = (%d,%v), want (%d,%v)", c.key.String(), idx, ok, c.wantIdx, c.wantOK)
		}
	}
}

func TestAltDigitSwitchesBuffer(t *testing.T) {
	m := newTestModel()
	m.ensureBuffer("#a", BufferChannel) // index 1
	m.ensureBuffer("#b", BufferChannel) // index 2

	tm, _ := m.Update(tea.KeyPressMsg{Code: '3', Text: "3", Mod: tea.ModAlt})
	m = tm.(model)
	if m.active != 2 {
		t.Errorf("Alt+3 switched to buffer %d, want 2 (#b)", m.active)
	}

	// Alt+9 with no 9th buffer is a no-op (stays put).
	tm, _ = m.Update(tea.KeyPressMsg{Code: '9', Text: "9", Mod: tea.ModAlt})
	m = tm.(model)
	if m.active != 2 {
		t.Errorf("Alt+9 with no 9th buffer changed active to %d, want 2", m.active)
	}
}

func TestJumpToActive(t *testing.T) {
	m := newTestModel()
	m.ensureBuffer("#a", BufferChannel) // 1
	b, _ := m.ensureBuffer("#b", BufferChannel)
	b.Unread = 3                        // index 2 has activity
	m.ensureBuffer("#c", BufferChannel) // 3

	m.active = 0
	m.jumpToActive()
	if m.active != 2 {
		t.Errorf("jumpToActive went to %d, want 2 (the unread buffer)", m.active)
	}

	// With nothing active, it stays put.
	m.buffers[2].Unread = 0
	m.active = 1
	m.jumpToActive()
	if m.active != 1 {
		t.Errorf("jumpToActive with no activity moved to %d, want 1 (no-op)", m.active)
	}
}

// TestSwitchBackShowsLinesReceivedWhileAway is the regression for the stale
// viewport on buffer switch: a message that arrives while a buffer is
// unfocused only bumps Unread, and switching back between two SAME-sized
// buffers used to re-show the old SetContent snapshot — the unread counter
// said new messages, the pane didn't show them until a resize or the next
// active-line append. appendLine now marks the buffer dirty and layout()
// refreshes a dirty buffer when it becomes active.
func TestSwitchBackShowsLinesReceivedWhileAway(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	_, ia := m.ensureBuffer("#a", BufferChannel)
	_, ib := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(ia)
	m = layout(m)
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :first"))

	// Switch to #b (same kind, hence the same pane size), then a message lands
	// in the now-unfocused #a.
	m.switchTo(ib)
	m = layout(m)
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :while-away"))
	if !m.buffer("#a").dirty {
		t.Fatal("append to an unfocused buffer did not mark it dirty")
	}

	// Switch back: layout must refresh the dirty buffer even though its pane
	// size is unchanged.
	m.switchTo(ia)
	m = layout(m)
	a := m.buffer("#a")
	if a.dirty {
		t.Error("layout left the newly active buffer dirty (no refresh)")
	}
	if got := stripANSI(a.vp.View()); !strings.Contains(got, "while-away") {
		t.Errorf("switch-back viewport is stale; missing the message received while away:\n%s", got)
	}
}
