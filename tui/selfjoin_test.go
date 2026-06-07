package tui

import "testing"

// TestSelfJoinOpensChannelBuffer verifies that our own JOIN opens the channel's
// sidebar buffer immediately — even for a quiet channel with no messages — so
// autojoined / comma-separated channels (JOIN #linux,#python) appear in the
// sidebar instead of being tracked invisibly (members/NAMES only) until the
// first message or history line arrives.
func TestSelfJoinOpensChannelBuffer(t *testing.T) {
	cli, _ := connectedClient(t, "me")
	m := newModel(cli, nil)
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)
	net := m.activeNet()

	if m.netBuffer(net, "#linux") != nil {
		t.Fatal("#linux buffer existed before any JOIN")
	}

	// A server echoes JOIN #linux,#python as two separate JOIN lines.
	m = routeEventOn(m, net, evt(t, ":me!u@h JOIN #linux"))
	m = routeEventOn(m, net, evt(t, ":me!u@h JOIN #python"))

	if m.netBuffer(net, "#linux") == nil {
		t.Error("self-JOIN did not open #linux (missing from the sidebar)")
	}
	if m.netBuffer(net, "#python") == nil {
		t.Error("self-JOIN did not open #python (missing from the sidebar)")
	}

	// A JOIN by someone ELSE to a channel we have NOT joined must not open a
	// buffer (a hostile server can't conjure windows via foreign joins).
	m = routeEventOn(m, net, evt(t, ":bob!u@h JOIN #notours"))
	if m.netBuffer(net, "#notours") != nil {
		t.Error("a foreign JOIN to an unjoined channel wrongly opened a buffer")
	}
}
