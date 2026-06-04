package tui

import (
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestPageScrollHelpLabelIsOSAware verifies the page-scroll help label shows the
// keys a user actually presses on their platform: Fn+↑/↓ on macOS (where those
// are PgUp/PgDn and are the reliable scroll), and pgup/pgdn elsewhere.
func TestPageScrollHelpLabelIsOSAware(t *testing.T) {
	k := defaultKeymap()
	wantUp, wantDown := "pgup", "pgdn"
	if runtime.GOOS == "darwin" {
		wantUp, wantDown = "fn+↑", "fn+↓"
	}
	if got := k.PageUp.Help().Key; got != wantUp {
		t.Errorf("PageUp help key = %q, want %q", got, wantUp)
	}
	if got := k.PageDown.Help().Key; got != wantDown {
		t.Errorf("PageDown help key = %q, want %q", got, wantDown)
	}
	if !strings.Contains(k.summary(), wantUp) {
		t.Errorf("/help summary missing page-scroll label %q:\n%s", wantUp, k.summary())
	}
}

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

// TestFormatInfoPreservesNewlines guards the /help layout: an info line's
// intentional line breaks must survive (sanitize strips '\n' as a control byte),
// while control characters within a line are still stripped.
func TestFormatInfoPreservesNewlines(t *testing.T) {
	tm := newTheme()
	got := stripANSI(tm.formatInfo("keys: a · b · ctrl+c quit\ncommands: /x /y"))
	if !strings.Contains(got, "\n") {
		t.Errorf("formatInfo dropped the newline (rows would weld): %q", got)
	}
	if !strings.Contains(got, "commands: /x /y") {
		t.Errorf("formatInfo mangled the second line: %q", got)
	}
	// An embedded escape within a line must still be neutralized.
	if strings.ContainsRune(stripANSI(tm.formatInfo("a\x1b]52;c;x\x07b")), 0x1b) {
		t.Error("formatInfo left an ESC unsanitized")
	}
}

// TestHelpCommandIsTwoLines confirms the rendered /help text actually breaks
// between the keys digest and the command list (the bug where "quit" and
// "commands:" ran together).
func TestHelpCommandIsTwoLines(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/help")
	rendered := stripANSI(defaultTheme.formatInfo(act.text))
	keysIdx := strings.Index(rendered, "keys:")
	cmdsIdx := strings.Index(rendered, "commands:")
	if keysIdx < 0 || cmdsIdx < 0 {
		t.Fatalf("/help missing sections: %q", rendered)
	}
	if !strings.Contains(rendered[keysIdx:cmdsIdx], "\n") {
		t.Errorf("/help has no newline between keys and commands:\n%s", rendered)
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
