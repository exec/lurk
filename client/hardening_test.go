package client

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// TestLowercaseCommandsHandled verifies that commands from a non-conformant
// (or hostile) server arriving in lowercase still drive state tracking and
// user dispatch: IRC commands are case-insensitive on the wire, and the
// client normalizes them before handling.
func TestLowercaseCommandsHandled(t *testing.T) {
	const nick = "lcbot"
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	c := New(Config{Nick: nick, User: "u", Realname: "r", Caps: []string{}})

	privmsgs := make(chan string, 1)
	c.On(irc.PRIVMSG, func(ev *Event) { privmsgs <- ev.Text() })

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		miniRegister(t, srv, nick)
		srv.expect("JOIN #lurk")
		// Confirm our join, then deliver a lowercase join and privmsg.
		srv.send(":" + nick + "!u@h JOIN #lurk")
		srv.send(":bob!b@h join #lurk")
		srv.send(":bob!b@h privmsg #lurk :hi there")
	}()
	defer func() {
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ConnectConn(ctx, conn.NewConn(clientSide, conn.Options{})); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}
	if err := c.Join("#lurk"); err != nil {
		t.Fatalf("Join: %v", err)
	}

	// The lowercase join must be tracked as membership (self membership is
	// populated by NAMES, which this script omits — bob is the probe).
	if !waitFor(func() bool {
		_, ok := membersByNick(c.Members("#lurk"))["bob"]
		return ok
	}, 2*time.Second) {
		t.Errorf("lowercase join not tracked: members = %+v", c.Members("#lurk"))
	}
	// The lowercase privmsg must reach the PRIVMSG handler.
	select {
	case text := <-privmsgs:
		if text != "hi there" {
			t.Errorf("PRIVMSG text = %q, want %q", text, "hi there")
		}
	case <-time.After(2 * time.Second):
		t.Error("lowercase privmsg never dispatched to the PRIVMSG handler")
	}
}

// TestReconnectRegistrationTimeout verifies that a reconnect attempt whose
// peer accepts the dial but never completes registration (and never closes
// the socket) is abandoned after reconnectRegisterTimeout, so the supervisor
// keeps retrying instead of wedging forever.
func TestReconnectRegistrationTimeout(t *testing.T) {
	const nick = "rtbot"

	saved := reconnectRegisterTimeout
	reconnectRegisterTimeout = 250 * time.Millisecond
	defer func() { reconnectRegisterTimeout = saved }()

	var mu sync.Mutex
	var attempts int
	var down atomic.Bool
	testDone := make(chan struct{})
	defer close(testDone)

	c := New(Config{Nick: nick, User: "u", Realname: "r", Server: "x:1", Caps: []string{}, AutoReconnect: true})
	c.dial = func(ctx context.Context) (transport, error) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		clientSide, serverSide := net.Pipe()
		switch n {
		case 2:
			// The stalled reconnect: swallow everything the client sends and
			// never respond. Only the registration timeout can end this attempt.
			go func() { _, _ = io.Copy(io.Discard, serverSide) }()
		default:
			srv := newMockServer(t, serverSide)
			srv.down = &down
			go func() {
				miniRegister(t, srv, nick)
				if n == 1 {
					srv.close() // unexpected drop: trigger the supervisor
					return
				}
				<-testDone
			}()
		}
		return conn.NewConn(clientSide, conn.Options{}), nil
	}

	reconnected := make(chan struct{}, 4)
	c.HandleReconnected(func(*Event) { reconnected <- struct{}{} })
	defer c.Close()
	defer down.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The drop after attempt 1 starts the supervisor; attempt 2 stalls and must
	// time out; attempt 3 registers and fires @reconnected.
	select {
	case <-reconnected:
	case <-time.After(15 * time.Second):
		mu.Lock()
		got := attempts
		mu.Unlock()
		t.Fatalf("no @reconnected after a stalled registration (dial attempts = %d) — supervisor wedged?", got)
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got < 3 {
		t.Errorf("dial attempts = %d, want >= 3 (stalled attempt abandoned, then retried)", got)
	}
}

// TestNoConcurrentHandlerInvocations verifies the dispatch contract: handler
// invocations never overlap, even when the reconnect supervisor's synthetic
// events are emitted from its own goroutine while the run goroutine is
// dispatching inbound messages.
func TestNoConcurrentHandlerInvocations(t *testing.T) {
	const nick = "serbot"
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	c := New(Config{Nick: nick, User: "u", Realname: "r", Caps: []string{}})

	var inFlight, overlaps atomic.Int32
	seen := make(chan struct{}, 256)
	c.OnAny(func(*Event) {
		if inFlight.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(50 * time.Microsecond) // widen the window
		inFlight.Add(-1)
		seen <- struct{}{}
	})

	const nInbound, nSynthetic = 50, 50
	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		miniRegister(t, srv, nick)
		for i := 0; i < nInbound; i++ {
			srv.send(":bob!b@h PRIVMSG " + nick + " :ping")
		}
	}()
	defer func() {
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ConnectConn(ctx, conn.NewConn(clientSide, conn.Options{})); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}

	// Emit synthetic events from this goroutine while the run goroutine
	// dispatches the inbound flood — the same shape as the supervisor emitting
	// @reconnected while the new session is already live.
	for i := 0; i < nSynthetic; i++ {
		c.emitSynthetic(evtReconnecting, "x")
	}

	// Wait for every event to have been dispatched: registration numerics from
	// miniRegister also pass through OnAny, so just drain until quiescent.
	deadline := time.After(5 * time.Second)
	total := 0
	for total < nInbound+nSynthetic {
		select {
		case <-seen:
			total++
		case <-deadline:
			t.Fatalf("only %d/%d events dispatched before timeout", total, nInbound+nSynthetic)
		}
	}

	if n := overlaps.Load(); n != 0 {
		t.Errorf("observed %d overlapping handler invocations, want 0", n)
	}
}
