package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// newServerBuffer-backed model with no client: activeBuffer is the server
// buffer (not a channel), so menuEntries should offer only the base actions and
// no operator items (which require a channel + ops).
func baseMenuModel() model {
	return model{
		buffers:  []*Buffer{newServerBuffer("test")},
		menuOpen: true,
		menuNick: "alice",
		input:    newInput(),
	}
}

func TestMenuEntriesBaseActions(t *testing.T) {
	m := baseMenuModel()
	got := menuEntries(m)
	if len(got) != 3 {
		t.Fatalf("want 3 base entries (no channel/ops), got %d: %+v", len(got), got)
	}
	want := []menuActionID{miMessage, miWhois, miInsertNick}
	for i, e := range got {
		if e.act != want[i] {
			t.Errorf("entry %d = %v, want %v", i, e.act, want[i])
		}
	}
}

func TestMenuKeyNavigationAndClose(t *testing.T) {
	m := baseMenuModel()

	// Down then up returns to the top; bounds are respected.
	nm, _ := m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyDown})
	m = nm.(model)
	if m.menuSel != 1 {
		t.Fatalf("after down, menuSel=%d want 1", m.menuSel)
	}
	nm, _ = m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyUp})
	m = nm.(model)
	if m.menuSel != 0 {
		t.Fatalf("after up, menuSel=%d want 0", m.menuSel)
	}

	// Esc closes the menu and returns focus to the editor.
	nm, _ = m.handleMenuKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = nm.(model)
	if m.menuOpen || m.focus != focusInput {
		t.Fatalf("after esc: menuOpen=%v focus=%v, want closed+input", m.menuOpen, m.focus)
	}
}

func TestApplyMenuInsertNick(t *testing.T) {
	m := baseMenuModel()
	nm, _ := m.applyMenu(menuEntry{"Insert nick", miInsertNick})
	m = nm.(model)
	if got := m.input.Value(); got != "alice: " {
		t.Errorf("insert nick into empty editor = %q, want %q", got, "alice: ")
	}
	if m.menuOpen || m.focus != focusInput {
		t.Errorf("applyMenu should close the menu and refocus input")
	}
}
