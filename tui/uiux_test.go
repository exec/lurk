package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestThemeSelection covers the startup theme resolution: NO_COLOR forces the
// monochrome theme; a light COLORFGBG selects the light theme; otherwise dark.
func TestThemeSelection(t *testing.T) {
	t.Run("NO_COLOR forces mono", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		if !noColorSet() {
			t.Fatal("noColorSet should be true")
		}
		// The mono theme uses no nick colors (a single NoColor entry).
		th := monoTheme()
		if len(th.nickPalette) != 1 {
			t.Errorf("mono palette = %d entries, want 1 (NoColor)", len(th.nickPalette))
		}
	})

	t.Run("light COLORFGBG", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("COLORFGBG", "0;15")
		if !lightBackgroundEnv() {
			t.Error("COLORFGBG 0;15 should be detected as a light background")
		}
		t.Setenv("COLORFGBG", "15;0")
		if lightBackgroundEnv() {
			t.Error("COLORFGBG 15;0 (dark bg) should not be light")
		}
	})
}

// TestApplyDetectedBackgroundRespectsNoColor verifies the runtime background
// detection does not override NO_COLOR.
func TestApplyDetectedBackgroundRespectsNoColor(t *testing.T) {
	saved := defaultTheme
	defer func() { defaultTheme = saved }()

	t.Setenv("NO_COLOR", "1")
	defaultTheme = monoTheme()
	applyDetectedBackground(false) // a light terminal
	// NO_COLOR wins: the palette stays the single-NoColor mono palette.
	if len(defaultTheme.nickPalette) != 1 {
		t.Errorf("NO_COLOR overridden by background detection: palette = %d", len(defaultTheme.nickPalette))
	}
}

// TestEmptyBufferHint verifies an empty buffer shows a context-appropriate hint.
func TestEmptyBufferHint(t *testing.T) {
	server := &Buffer{Kind: BufferServer}
	if !strings.Contains(emptyHint(server), "/help") {
		t.Errorf("server hint should mention /help: %q", emptyHint(server))
	}
	pm := &Buffer{Kind: BufferPM, Title: "bob"}
	if !strings.Contains(emptyHint(pm), "bob") {
		t.Errorf("PM hint should mention the peer: %q", emptyHint(pm))
	}

	// The hint renders into the message body of an empty buffer.
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "browse channels") {
		t.Errorf("empty server buffer should render the hint:\n%s", out)
	}
}

// TestSidebarShowsUnreadCount verifies an inactive buffer with activity shows its
// unread count.
func TestSidebarShowsUnreadCount(t *testing.T) {
	m := newTestModel()
	b, _ := m.ensureBuffer("#go", BufferChannel)
	b.Unread = 7
	// Active buffer stays the server buffer, so #go is inactive.
	out := stripANSI(renderSidebar(m, sidebarWidth, 10))
	if !strings.Contains(out, "(7)") {
		t.Errorf("sidebar should show unread count (7):\n%s", out)
	}
}

// TestScrollCountInStatus verifies the status bar reports how far below the
// bottom the user has scrolled.
func TestScrollCountInStatus(t *testing.T) {
	m := newTestModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m = tm.(model)
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	for n := 0; n < 200; n++ {
		m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :a line"))
	}
	// Scroll up so we are no longer at the bottom.
	m.activeBuffer().vp.PageUp()
	status := stripANSI(renderStatus(m))
	if !strings.Contains(status, "more") {
		t.Errorf("scrolled status should report lines below:\n%s", status)
	}
}
