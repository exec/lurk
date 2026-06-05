package tui

import (
	"strings"
	"testing"

	"github.com/exec/lurk/client"
)

// TestCustomHighlightWord verifies an extra highlight word triggers a mention
// (bell + highlight) the same as the nick.
func TestCustomHighlightWord(t *testing.T) {
	m := newModel(client.New(client.Config{Nick: "me"}), nil)
	m.ensureBuffer("#go", BufferChannel) // unfocused
	// Add a highlight word via the command.
	if act, _ := runLine(m, "/highlight deploy"); act.kind != actionInfo {
		t.Fatalf("/highlight kind = %v", act.kind)
	}
	if !m.highlights["deploy"] {
		t.Fatal("/highlight did not record the word")
	}

	m.bell = false
	m = routeEvent(m, evt(t, ":a!a@h PRIVMSG #go :time to deploy now"))
	if !m.bell {
		t.Error("custom highlight word did not trigger a mention bell")
	}
	if !m.buffer("#go").Highlight {
		t.Error("custom highlight word did not raise the buffer highlight")
	}
}

// TestHighlightSeededFromConfig verifies configured highlight words are loaded.
func TestHighlightSeededFromConfig(t *testing.T) {
	m := newModel(client.New(client.Config{Nick: "me", Highlights: []string{"Release", "ops"}}), nil)
	if !m.highlights["release"] || !m.highlights["ops"] {
		t.Errorf("config highlights not seeded (folded): %v", m.highlights)
	}
}

// TestIgnoreSuppressesMessages verifies an ignored nick's messages don't appear.
func TestIgnoreSuppressesMessages(t *testing.T) {
	m := newModel(client.New(client.Config{Nick: "me"}), nil)
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)

	runLine(m, "/ignore Spammer")
	if !m.isIgnored("spammer") {
		t.Fatal("/ignore did not record the nick")
	}

	before := len(m.activeBuffer().lines)
	m = routeEvent(m, evt(t, ":Spammer!s@h PRIVMSG #go :buy now"))
	if len(m.activeBuffer().lines) != before {
		t.Errorf("ignored sender's message was shown")
	}

	// A non-ignored sender still comes through.
	m = routeEvent(m, evt(t, ":alice!a@h PRIVMSG #go :hi"))
	if len(m.activeBuffer().lines) != before+1 {
		t.Errorf("non-ignored message was dropped")
	}

	// Unignore restores delivery.
	runLine(m, "/unignore Spammer")
	if m.isIgnored("spammer") {
		t.Error("/unignore did not remove the nick")
	}
}

// TestIgnoreListing verifies the no-arg /ignore lists current ignores.
func TestIgnoreListing(t *testing.T) {
	m := newTestModel()
	act, _ := runLine(m, "/ignore")
	if !strings.Contains(act.text, "not ignoring") {
		t.Errorf("empty /ignore = %q", act.text)
	}
	runLine(m, "/ignore bob")
	act, _ = runLine(m, "/ignore")
	if !strings.Contains(act.text, "bob") {
		t.Errorf("/ignore list = %q, want it to include bob", act.text)
	}
}
