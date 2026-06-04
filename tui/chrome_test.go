package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// renderAt sizes the model and returns its rendered frame (ANSI stripped).
func renderAt(t *testing.T, m model, w, h int) (model, string) {
	t.Helper()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = tm.(model)
	return m, stripANSI(m.View().Content)
}

// TestHelpFooterRendered verifies the key-hint footer is drawn on screen (the
// bindings were previously only reachable via /help).
func TestHelpFooterRendered(t *testing.T) {
	_, out := renderAt(t, newTestModel(), 120, 24)
	for _, want := range []string{"/help", "next buf", "users"} {
		if !strings.Contains(out, want) {
			t.Errorf("help footer missing %q in rendered frame:\n%s", want, out)
		}
	}
}

// TestHelpFooterLeadsWithHelpWhenNarrow verifies the discovery gateway survives
// truncation on a narrow terminal (Help leads the ShortHelp set).
func TestHelpFooterLeadsWithHelpWhenNarrow(t *testing.T) {
	_, out := renderAt(t, newTestModel(), 40, 24)
	if !strings.Contains(out, "/help") {
		t.Errorf("narrow footer dropped /help:\n%s", out)
	}
}

// TestTopicBarShownForChannels verifies a channel buffer shows the topic bar
// (here the placeholder, since the test client is nil), while the server buffer
// does not.
func TestTopicBarShownForChannels(t *testing.T) {
	m := newTestModel()
	m, serverOut := renderAt(t, m, 100, 24)
	if strings.Contains(serverOut, "Topic:") {
		t.Errorf("server buffer should have no topic bar:\n%s", serverOut)
	}

	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "Topic:") {
		t.Errorf("channel buffer missing topic bar:\n%s", out)
	}
	if !strings.Contains(out, "(no topic set)") {
		t.Errorf("channel with no topic should show the placeholder:\n%s", out)
	}
}

// TestVerticalLayoutSumsToHeight guards the row math: the stacked regions must
// exactly fill the terminal height (topic + body + status + help + input).
func TestVerticalLayoutSumsToHeight(t *testing.T) {
	cases := []struct {
		kind BufferKind
		h    int
	}{
		{BufferServer, 24},
		{BufferChannel, 24},
		{BufferChannel, 6},
		{BufferServer, 3},
	}
	for _, c := range cases {
		m := newTestModel()
		m.buffers[0].Kind = c.kind
		m.width, m.height = 80, c.h
		topic, body := verticalLayout(m)
		got := topic + body + statusHeight + helpHeight + inputHeight
		// body is floored at 1, so on a very short terminal the sum may exceed
		// height; otherwise it must match exactly.
		if c.h >= topic+statusHeight+helpHeight+inputHeight+1 && got != c.h {
			t.Errorf("kind=%v h=%d: regions sum to %d, want %d", c.kind, c.h, got, c.h)
		}
	}
}
