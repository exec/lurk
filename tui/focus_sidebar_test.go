package tui

import (
	"fmt"
	"strings"
	"testing"
)

// TestSidebarFollowsActive verifies the buffer sidebar windows around the active
// buffer, so a selection past the fold stays visible (the same clipping bug the
// nicklist had).
func TestSidebarFollowsActive(t *testing.T) {
	m := newTestModel()
	for i := 0; i < 30; i++ {
		m.ensureBuffer(fmt.Sprintf("#c%02d", i), BufferChannel)
	}
	// Focus a buffer well past a short sidebar's height.
	m.switchTo(m.bufferIndex("#c27"))

	out := stripANSI(renderSidebar(m, sidebarWidth, 6))
	if !strings.Contains(out, "#c27") {
		t.Errorf("active buffer #c27 clipped from sidebar:\n%s", out)
	}
	if strings.Contains(out, "#c00") {
		t.Errorf("sidebar did not scroll: early buffer #c00 still shown:\n%s", out)
	}
}

// TestRefocusInputFocusesEditor verifies refocusInput restores editor focus from
// a blurred state (the hand-back from the nicklist/menu).
func TestRefocusInputFocusesEditor(t *testing.T) {
	m := newTestModel()
	m.focus = focusNicks
	m.input.Blur()

	m = m.refocusInput()
	if m.focus != focusInput {
		t.Errorf("focus = %v, want focusInput", m.focus)
	}
	if !m.input.Focused() {
		t.Error("refocusInput did not re-focus the editor")
	}
}

// TestEnterNickFocusNoMembersIsNoop verifies that focusing the nicklist with
// nothing to select leaves the editor focused (so the caret does not vanish on a
// PM/server buffer or an empty channel).
func TestEnterNickFocusNoMembersIsNoop(t *testing.T) {
	m := newTestModel() // server buffer, no members
	m = m.enterNickFocus()
	if m.focus != focusInput {
		t.Errorf("focus = %v, want focusInput (no members to select)", m.focus)
	}
	if !m.input.Focused() {
		t.Error("editor should stay focused when there is nothing to select")
	}
}

// TestCursorSuppressedWhenNotEditorFocused verifies the rendered view omits the
// hardware cursor whenever the editor is not the focused pane.
func TestCursorSuppressedWhenNotEditorFocused(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)

	m.focus = focusNicks
	if m.View().Cursor != nil {
		t.Error("nicklist-focused view should not place the editor cursor")
	}
	m.focus = focusInput
	m.menuOpen = true
	if m.View().Cursor != nil {
		t.Error("menu-open view should not place the editor cursor")
	}
}
