package tui

import (
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
