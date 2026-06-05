package client

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// recordTransport is a minimal transport that records every line the client
// writes, for asserting serialization without a live server. Messages() returns
// a never-delivering channel so the run loop (if started) simply idles.
type recordTransport struct {
	sent []string
	msgs chan *irc.Message
}

func newRecordTransport() *recordTransport {
	return &recordTransport{msgs: make(chan *irc.Message)}
}

func (r *recordTransport) Messages() <-chan *irc.Message { return r.msgs }
func (r *recordTransport) WriteMessage(m *irc.Message) error {
	line, err := m.Serialize()
	if err != nil {
		return err
	}
	r.sent = append(r.sent, strings.TrimRight(line, "\r\n"))
	return nil
}
func (r *recordTransport) Send(line string) error {
	r.sent = append(r.sent, strings.TrimRight(line, "\r\n"))
	return nil
}
func (r *recordTransport) Close() error { return nil }
func (r *recordTransport) Err() error   { return nil }

// newEventClient builds a dispatching client (no transport) and a slice that
// collects every dispatched event, for testing the track/dispatch path.
func newEventClient(tokens ...string) (*Client, *[]Event) {
	c := &Client{st: newState(), disp: newDispatcher()}
	if len(tokens) > 0 {
		c.st.mergeISupport(tokens)
	}
	var got []Event
	c.OnAny(func(ev *Event) { got = append(got, *ev) })
	return c, &got
}

func feedHandle(t *testing.T, c *Client, line string) {
	t.Helper()
	m, err := irc.Parse(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	c.handle(m)
}

func TestBatchTypeResolution(t *testing.T) {
	c, got := newEventClient("CHANTYPES=#")
	feedHandle(t, c, ":srv BATCH +xyz chathistory #c")
	feedHandle(t, c, "@batch=xyz :bob!b@h PRIVMSG #c :from history")
	feedHandle(t, c, ":srv BATCH -xyz")
	feedHandle(t, c, ":bob!b@h PRIVMSG #c :live message")

	var inner, live *Event
	for i := range *got {
		ev := &(*got)[i]
		if ev.Command() != irc.PRIVMSG {
			continue
		}
		switch ev.Text() {
		case "from history":
			inner = ev
		case "live message":
			live = ev
		}
	}
	if inner == nil || live == nil {
		t.Fatalf("missing PRIVMSG events: inner=%v live=%v", inner, live)
	}
	if inner.BatchType() != "chathistory" {
		t.Errorf("batched message BatchType = %q, want chathistory", inner.BatchType())
	}
	if live.BatchType() != "" {
		t.Errorf("live message BatchType = %q, want empty", live.BatchType())
	}
}

func TestChatHistoryLatestSerialize(t *testing.T) {
	c := &Client{st: newState()}
	rec := newRecordTransport()
	c.tr = rec
	if err := c.ChatHistoryLatest("#c", 50); err != nil {
		t.Fatalf("ChatHistoryLatest: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "CHATHISTORY LATEST #c * 50" {
		t.Errorf("sent = %q, want [CHATHISTORY LATEST #c * 50]", rec.sent)
	}
	// A non-positive limit is clamped to the protocol minimum of 1.
	rec.sent = nil
	_ = c.ChatHistoryLatest("#c", 0)
	if rec.sent[0] != "CHATHISTORY LATEST #c * 1" {
		t.Errorf("clamped sent = %q, want limit 1", rec.sent[0])
	}
}

func TestSendTaggedSerialize(t *testing.T) {
	c := &Client{st: newState()}
	rec := newRecordTransport()
	c.tr = rec
	if err := c.SendTagged(map[string]string{"+typing": "active"}, irc.TAGMSG, "#c"); err != nil {
		t.Fatalf("SendTagged: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "@+typing=active TAGMSG #c" {
		t.Errorf("sent = %q, want [@+typing=active TAGMSG #c]", rec.sent)
	}
}

func TestTypingNoopWithoutMessageTags(t *testing.T) {
	c := &Client{st: newState()} // neg is nil => CapEnabled is false
	rec := newRecordTransport()
	c.tr = rec
	if err := c.Typing("#c", "active"); err != nil {
		t.Fatalf("Typing: %v", err)
	}
	if len(rec.sent) != 0 {
		t.Errorf("Typing sent %q with message-tags disabled, want nothing", rec.sent)
	}
}

func TestReceiveTypingTag(t *testing.T) {
	c, got := newEventClient("CHANTYPES=#")
	feedHandle(t, c, "@+typing=active :bob!b@h TAGMSG #c")
	if len(*got) != 1 {
		t.Fatalf("got %d events, want 1", len(*got))
	}
	ev := (*got)[0]
	if ev.Command() != irc.TAGMSG {
		t.Fatalf("command = %q, want TAGMSG", ev.Command())
	}
	if v := ev.Message.Tags.Get("+typing"); v != "active" {
		t.Errorf("+typing tag = %q, want active", v)
	}
}

func TestChatHistoryNotRequestedWithoutCap(t *testing.T) {
	c := &Client{st: newState(), disp: newDispatcher()} // neg nil => draft/chathistory not enabled
	c.st.mergeISupport([]string{"CHANTYPES=#"})
	c.st.self = "me"
	rec := newRecordTransport()
	c.tr = rec
	feedHandle(t, c, ":me!u@h JOIN #c")
	for _, line := range rec.sent {
		if strings.HasPrefix(line, "CHATHISTORY") {
			t.Errorf("unexpected history request without cap: %q", line)
		}
	}
}

// TestChatHistoryAutoRequestOnJoin drives a no-SASL handshake that negotiates
// draft/chathistory, then verifies a self-JOIN triggers an automatic
// CHATHISTORY LATEST request.
func TestChatHistoryAutoRequestOnJoin(t *testing.T) {
	c, srv, cleanup := dialNoSASL(t, []string{"batch", "draft/chathistory", "server-time"})
	defer cleanup()

	if err := c.Join("#hist"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	srv.expect("JOIN #hist")
	srv.send(":me!u@h JOIN #hist")
	srv.expect("CHATHISTORY LATEST #hist * 50")
}

// TestTypingSendsWithCap negotiates message-tags and verifies Typing emits a
// +typing TAGMSG.
func TestTypingSendsWithCap(t *testing.T) {
	c, srv, cleanup := dialNoSASL(t, []string{"message-tags"})
	defer cleanup()

	if err := c.Typing("#c", "active"); err != nil {
		t.Fatalf("Typing: %v", err)
	}
	srv.expect("@+typing=active TAGMSG #c")
}

// dialNoSASL runs a minimal no-SASL registration that advertises and ACKs the
// given caps, returning the connected client, the mock server, and a cleanup
// func. The client's nick is "me".
func dialNoSASL(t *testing.T, caps []string) (*Client, *mockServer, func()) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{Nick: "me", User: "u", Realname: "Me", Caps: caps})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		srv.expect("CAP LS 302")
		srv.expect("NICK me")
		srv.expect("USER u 0 * Me")
		srv.send("CAP * LS :" + strings.Join(caps, " "))
		req := srv.expectPrefix("CAP REQ :")
		reqd := strings.TrimPrefix(req, "CAP REQ :")
		srv.send("CAP * ACK :" + reqd)
		srv.expect("CAP END")
		srv.send(
			":server 001 me :Welcome to the Net me",
			":server 005 me NETWORK=Net CHANTYPES=# :are supported by this server",
			":server 376 me :End of MOTD.",
		)
	}()

	cleanup := func() {
		// Wait for the server script to finish writing its welcome burst before
		// closing the client. ConnectConn returns as soon as 001 is processed, but
		// the script goroutine may still be mid-write on the 005/376 lines over the
		// synchronous net.Pipe; closing the client end first would make those writes
		// fail with "closed pipe" and spuriously fail the test during teardown.
		// scriptDone is always eventually closed (the goroutine defers it, even on a
		// failed expect), so this cannot deadlock.
		<-scriptDone
		c.Close()
		srv.close()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.ConnectConn(ctx, tr); err != nil {
		cleanup()
		t.Fatalf("ConnectConn: %v", err)
	}
	return c, srv, cleanup
}
