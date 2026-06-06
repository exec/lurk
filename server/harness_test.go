package server

import (
	"net"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// dialPipe returns a connected client/server pair of framed conns over net.Pipe —
// the in-process transport lurkd's hermetic tests use instead of real sockets.
// Both ends are torn down on test cleanup. Later phases drive lurkd's listener by
// handing it the server end of such a pair while a synthetic client scripts the
// other; the two-mock sandwich (mock client ↔ lurkd ↔ mock upstream) is built
// from two of these.
func dialPipe(t *testing.T) (client, srv *conn.Conn) {
	t.Helper()
	c, s := net.Pipe()
	client = conn.NewConn(c, conn.Options{})
	srv = conn.NewConn(s, conn.Options{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = srv.Close()
	})
	return client, srv
}

// TestPipeHarnessRoundTrip is the Phase 0 infrastructure smoke test: it proves the
// net.Pipe + conn.NewConn framing the daemon reuses actually round-trips a parsed
// message in process, so later phases can rely on dialPipe.
func TestPipeHarnessRoundTrip(t *testing.T) {
	client, srv := dialPipe(t)
	go func() {
		_ = client.WriteMessage(&irc.Message{Command: "PING", Params: []string{"tok123"}})
	}()
	select {
	case m := <-srv.Messages():
		if m.Command != "PING" || m.Param(0) != "tok123" {
			t.Fatalf("got %q %v over the pipe, want PING tok123", m.Command, m.Params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message arrived over the pipe harness")
	}
}
