package tui

import (
	"strings"
	"testing"
)

// These tests exercise the slash-command handlers added for /away and /whois,
// plus the /topic guard, using a model with no live client (m.cli == nil). The
// handlers guard their client calls on a nil client, so they still return the
// status action a real run would, which is what we assert on.

func TestRunLineAway(t *testing.T) {
	m := newTestModel()

	act, _ := runLine(m, "/away brb lunch")
	if act.kind != actionInfo {
		t.Fatalf("/away kind = %v, want actionInfo", act.kind)
	}
	if !strings.Contains(act.text, "brb lunch") {
		t.Errorf("/away text = %q, want it to mention the reason", act.text)
	}

	act, _ = runLine(m, "/away")
	if act.kind != actionInfo {
		t.Fatalf("/away (clear) kind = %v, want actionInfo", act.kind)
	}
	if !strings.Contains(strings.ToLower(act.text), "back") {
		t.Errorf("/away (clear) text = %q, want it to mention being back", act.text)
	}
}

func TestRunLineList(t *testing.T) {
	m := newTestModel()

	// /list opens the channel-directory modal: it returns actionListOpen (the
	// LIST request is sent and the RPL_LIST replies populate the modal). It must
	// be accepted both bare and with a filter argument.
	for _, line := range []string{"/list", "/list >50"} {
		act, _ := runLine(m, line)
		if act.kind != actionListOpen {
			t.Errorf("%q kind = %v, want actionListOpen", line, act.kind)
		}
	}

	// It is listed in /help.
	if !strings.Contains(commandNames(), "/list") {
		t.Errorf("/list missing from command list: %s", commandNames())
	}
}

func TestRunLineWhois(t *testing.T) {
	m := newTestModel()

	// An explicit nick is a clean send with no status line.
	act, _ := runLine(m, "/whois bob")
	if act.kind != actionNone {
		t.Fatalf("/whois bob kind = %v, want actionNone", act.kind)
	}

	// No argument from the server buffer (not a PM) is a usage error rather than
	// whoising an empty nick.
	act, _ = runLine(m, "/whois")
	if act.kind != actionInfo {
		t.Fatalf("/whois (no arg) kind = %v, want actionInfo", act.kind)
	}
}

func TestRunLineWhoisDefaultsToPM(t *testing.T) {
	m := newTestModel()
	// Open and focus a PM buffer; a bare /whois there targets the correspondent.
	_, idx := m.ensureBuffer("bob", BufferPM)
	m.switchTo(idx)

	act, _ := runLine(m, "/whois")
	if act.kind != actionNone {
		t.Fatalf("/whois in PM kind = %v, want actionNone (defaults to correspondent)", act.kind)
	}
}

func TestRunLineTopicNotAChannel(t *testing.T) {
	m := newTestModel() // active buffer is the server buffer
	act, _ := runLine(m, "/topic new topic")
	if act.kind != actionInfo {
		t.Fatalf("/topic on non-channel kind = %v, want actionInfo", act.kind)
	}
}
