package tui

import (
	"strings"
	"testing"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// channelModel returns a test model focused on a channel buffer named #go.
func channelModel(t *testing.T) model {
	t.Helper()
	m := newTestModel()
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	return m
}

// TestNewCommandsRegistered confirms the added op/channel commands appear in the
// registry (and thus /help and completion).
func TestNewCommandsRegistered(t *testing.T) {
	names := commandNames()
	for _, want := range []string{"/kick", "/mode", "/op", "/deop", "/voice", "/devoice", "/ban", "/invite", "/notice", "/ctcp", "/whowas", "/motd", "/clear"} {
		if !strings.Contains(names, want) {
			t.Errorf("command %q not registered: %s", want, names)
		}
	}
}

// TestChannelCommandsRequireChannel verifies op-style commands reject non-channel
// buffers with a usage hint rather than acting.
func TestChannelCommandsRequireChannel(t *testing.T) {
	m := newTestModel() // active = server buffer
	for _, line := range []string{"/kick bob", "/op bob", "/ban *!*@x"} {
		act, _ := runLine(m, line)
		if act.kind != actionInfo {
			t.Errorf("%q on server buffer kind = %v, want actionInfo", line, act.kind)
		}
	}
}

// TestChannelCommandsOnChannel verifies they parse to a clean action on a channel
// buffer (with a nil client they perform no protocol but must not error out).
func TestChannelCommandsOnChannel(t *testing.T) {
	m := channelModel(t)
	for _, line := range []string{"/op alice bob", "/kick bob rude", "/mode +m", "/voice carol"} {
		act, _ := runLine(m, line)
		if act.kind != actionNone {
			t.Errorf("%q kind = %v, want actionNone", line, act.kind)
		}
	}
}

// TestClearEmptiesBuffer verifies /clear drops the active buffer's scrollback.
func TestClearEmptiesBuffer(t *testing.T) {
	m := channelModel(t)
	m = layout(m)
	m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :one"))
	m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :two"))
	if len(m.activeBuffer().lines) == 0 {
		t.Fatal("setup: buffer should have lines")
	}
	act, _ := runLine(m, "/clear")
	if act.kind != actionNone {
		t.Fatalf("/clear kind = %v, want actionNone", act.kind)
	}
	if got := len(m.activeBuffer().lines); got != 0 {
		t.Errorf("after /clear, buffer has %d lines, want 0", got)
	}
}

// TestReconnectEventsRouteToActiveBuffer verifies the supervisor's status events
// surface where the user is looking.
func TestReconnectEventsRouteToActiveBuffer(t *testing.T) {
	m := channelModel(t)
	m = layout(m)
	mk := func(cmd, text string) client.Event {
		return client.Event{Message: &irc.Message{Command: cmd, Params: []string{text}}}
	}
	m = routeEvent(m, mk(client.EventReconnecting, "connection lost — reconnecting…"))
	m = routeEvent(m, mk(client.EventReconnected, "reconnected"))
	out := stripANSI(strings.Join(m.activeBuffer().lines, "\n"))
	if !strings.Contains(out, "reconnecting") || !strings.Contains(out, "reconnected") {
		t.Errorf("reconnect notices not shown in active buffer:\n%s", out)
	}
}

// TestHighlightRingsBell verifies a mention in a non-active buffer sets the bell
// flag (which Update turns into a bell command), while a mention in the buffer
// you are already viewing does not.
func TestHighlightRingsBell(t *testing.T) {
	// A real (unconnected) client gives appendLine a self nick to match on.
	m := newModel(client.New(client.Config{Nick: "me"}), nil)
	m.ensureBuffer("#go", BufferChannel) // unfocused; active stays the server buffer

	m.bell = false
	m = routeEvent(m, evt(t, ":a!a@h PRIVMSG #go :hey me!"))
	if !m.bell {
		t.Error("expected bell flag set for an unfocused-buffer highlight")
	}

	// A highlight in the buffer we are actively viewing should not bell.
	m.switchTo(m.bufferIndex("#go"))
	m = layout(m)
	m.bell = false
	m = routeEvent(m, evt(t, ":a!a@h PRIVMSG #go :me again"))
	if m.bell {
		t.Error("did not expect a bell for a highlight in the active buffer")
	}
}
