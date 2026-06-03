package tui

import (
	"fmt"
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

// TestStripStatusPrefix covers routing of STATUSMSG-prefixed channel targets to
// the channel's own buffer, while leaving plain channels, nicks, and modeless
// '+'-type channels (when '+' is not a status prefix) untouched.
func TestStripStatusPrefix(t *testing.T) {
	cases := []struct {
		target, statusMsg, want string
	}{
		{"@#chan", "@+", "#chan"},
		{"+#chan", "@+", "#chan"},
		{"@@#chan", "@+", "#chan"},
		{"#chan", "@+", "#chan"}, // no prefix to strip
		{"bob", "@+", "bob"},     // a nick is unaffected
		{"@#chan", "", "@#chan"}, // server has no STATUSMSG: never strip
		{"+chan", "@+", "+chan"}, // modeless '+'-type channel: stripped form not a channel, keep
		{"@#chan", "@", "#chan"}, // only '@' is a status prefix here
	}
	for _, c := range cases {
		if got := stripStatusPrefix(c.target, c.statusMsg); got != c.want {
			t.Errorf("stripStatusPrefix(%q, %q) = %q, want %q", c.target, c.statusMsg, got, c.want)
		}
	}
}

// TestRouteTextBufferCap verifies that inbound traffic addressed to an unbounded
// number of distinct targets cannot grow the window list without bound: once the
// cap is reached, overflow messages route to the server buffer instead of opening
// new windows.
func TestRouteTextBufferCap(t *testing.T) {
	m := newTestModel()
	const overflow = 50
	for i := 0; i < maxAutoBuffers+overflow; i++ {
		m = routeEvent(m, evt(t, fmt.Sprintf(":srv!s@h PRIVMSG #chan%d :hi", i)))
	}
	if len(m.buffers) > maxAutoBuffers {
		t.Errorf("buffer list grew past cap: %d > %d", len(m.buffers), maxAutoBuffers)
	}
	if len(m.buffers) != maxAutoBuffers {
		t.Errorf("buffer count = %d, want exactly the cap %d", len(m.buffers), maxAutoBuffers)
	}
	// Overflow messages must still be surfaced (in the server buffer), not dropped.
	if len(m.buffers[0].lines) == 0 {
		t.Errorf("overflow messages were dropped instead of routed to the server buffer")
	}
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
