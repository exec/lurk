package tui

import (
	"testing"

	"lurk/client"
	"lurk/irc"
)

// evt builds a client.Event from a raw line for routing tests.
func evt(t *testing.T, line string) client.Event {
	t.Helper()
	m, err := irc.Parse(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	return client.Event{Message: m}
}

func TestMetadataNotificationsAreNotRendered(t *testing.T) {
	for _, line := range []string{
		":bob!b@h ACCOUNT bobby",
		":bob!b@h AWAY :brb",
		":bob!b@h CHGHOST b2 h2",
		":bob!b@h SETNAME :New Real Name",
	} {
		m := newTestModel()
		before := len(m.buffers[0].lines)
		m = routeEvent(m, evt(t, line))
		if got := len(m.buffers[0].lines); got != before {
			t.Errorf("%q wrote %d server-buffer lines, want none", line, got-before)
		}
	}
}
