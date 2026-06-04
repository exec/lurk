package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// fillBuffer sizes the UI, opens and focuses a channel buffer, and floods it
// with enough lines to overflow the viewport so scrolling has somewhere to go.
func fillBuffer(t *testing.T) model {
	t.Helper()
	m := newTestModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	for n := 0; n < 200; n++ {
		m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :a message line"))
	}
	if !m.activeBuffer().vp.AtBottom() {
		t.Fatal("expected viewport stuck to bottom after fill")
	}
	return m
}

// TestScrollKeysMoveViewport verifies the scrollback keys actually move the
// viewport, via the real Update path, for every modifier+arrow binding
// (Shift/Ctrl/Alt) plus PgUp/PgDn. The code path is modifier-agnostic; whether a
// given terminal delivers the modifier is a separate, terminal-specific matter
// (see the macOS note in keys.go) — PgUp/PgDn always reach the app.
func TestScrollKeysMoveViewport(t *testing.T) {
	press := func(m model, msg tea.KeyPressMsg) (model, int) {
		tm, _ := m.Update(msg)
		m = tm.(model)
		return m, m.activeBuffer().vp.YOffset()
	}

	m := fillBuffer(t)
	bottom := m.activeBuffer().vp.YOffset()

	// Shift+Up scrolls up — the offset decreases.
	m, up := press(m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: tea.ModShift})
	if up >= bottom {
		t.Fatalf("shift+up did not scroll up: YOffset %d -> %d", bottom, up)
	}

	// Page Up scrolls up further (the reliable cross-terminal binding).
	m, pg := press(m, tea.KeyPressMsg{Code: tea.KeyPgUp})
	if pg >= up {
		t.Fatalf("pgup did not scroll up: YOffset %d -> %d", up, pg)
	}

	// Shift+Down moves back toward the bottom.
	m, dn := press(m, tea.KeyPressMsg{Code: tea.KeyDown, Mod: tea.ModShift})
	if dn <= pg {
		t.Fatalf("shift+down did not scroll down: YOffset %d -> %d", pg, dn)
	}

	// Ctrl+Up and Alt+Up aliases scroll up too.
	for _, mod := range []tea.KeyMod{tea.ModCtrl, tea.ModAlt} {
		cur := m.activeBuffer().vp.YOffset()
		var got int
		m, got = press(m, tea.KeyPressMsg{Code: tea.KeyUp, Mod: mod})
		if got >= cur {
			t.Fatalf("alias mod %v did not scroll up: YOffset %d -> %d", mod, cur, got)
		}
	}
}

// TestHelpCommand checks /help lists the keybinding digest and command names,
// and that /help <command> shows that command's usage.
func TestHelpCommand(t *testing.T) {
	m := newTestModel()

	act, _ := runLine(m, "/help")
	if act.kind != actionInfo {
		t.Fatalf("/help kind = %v, want actionInfo", act.kind)
	}
	for _, want := range []string{"keys:", "shift+↑", "scroll up", "/join", "/help"} {
		if !strings.Contains(act.text, want) {
			t.Errorf("/help output missing %q:\n%s", want, act.text)
		}
	}

	act, _ = runLine(m, "/help join")
	if !strings.Contains(act.text, "/join <channel> [key] — join a channel") {
		t.Errorf("/help join = %q", act.text)
	}

	act, _ = runLine(m, "/help nope")
	if !strings.Contains(act.text, "no such command") {
		t.Errorf("/help nope = %q", act.text)
	}
}

// TestQuestionMarkIsLiteral guards that "?" is not swallowed as a help shortcut
// but typed into the editor like any other character.
func TestQuestionMarkIsLiteral(t *testing.T) {
	m := newTestModel()
	tm, _ := m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	m = tm.(model)
	if got := m.input.Value(); got != "?" {
		t.Errorf("input after '?' = %q, want %q", got, "?")
	}
}
