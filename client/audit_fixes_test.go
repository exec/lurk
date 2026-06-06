package client

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
)

// countingTransport wraps a transport and counts Close calls so a test can assert
// the reconnect supervisor releases a dead session's transport.
type countingTransport struct {
	transport
	closes *atomic.Int32
}

func (t *countingTransport) Close() error {
	t.closes.Add(1)
	return t.transport.Close()
}

// TestReconnectClosesDeadTransport verifies the supervisor closes the dropped
// transport before re-dialing. A peer-initiated drop only cancels the conn (it
// never closes the local socket), so without the explicit Close the file
// descriptor would leak in CLOSE_WAIT on every reconnect.
func TestReconnectClosesDeadTransport(t *testing.T) {
	const nick = "fdbot"
	var mu sync.Mutex
	var attempts int
	var down atomic.Bool
	var firstCloses atomic.Int32
	testDone := make(chan struct{})
	defer close(testDone)

	c := New(Config{Nick: nick, User: "u", Realname: "r", Server: "x:1", Caps: []string{}, AutoReconnect: true})
	c.dial = func(ctx context.Context) (transport, error) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		clientSide, serverSide := net.Pipe()
		srv := newMockServer(t, serverSide)
		srv.down = &down
		go func() {
			miniRegister(t, srv, nick)
			if n == 1 {
				srv.close() // unexpected drop right after the first registration
				return
			}
			<-testDone // keep the second session up until teardown
		}()
		tr := transport(conn.NewConn(clientSide, conn.Options{}))
		if n == 1 {
			tr = &countingTransport{transport: tr, closes: &firstCloses}
		}
		return tr, nil
	}

	defer c.Close()
	defer down.Store(true) // registered after c.Close so it runs first (LIFO)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// The drop must trigger a re-dial...
	if !waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts >= 2
	}, 3*time.Second) {
		t.Fatal("no reconnect after the drop")
	}
	// ...and the dead first transport must have been closed by the supervisor.
	if !waitFor(func() bool { return firstCloses.Load() >= 1 }, 2*time.Second) {
		t.Errorf("dead transport not closed on reconnect (Close count = %d)", firstCloses.Load())
	}
}

// TestUnsupervisedClosesEventsOnEnd verifies that a session with no reconnect
// supervisor (here a ConnectConn session, which is never reconnectable) closes
// the Events stream when the connection ends, so a consumer ranging over Events()
// learns the session is over rather than blocking forever.
func TestUnsupervisedClosesEventsOnEnd(t *testing.T) {
	const nick = "evbot"
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{Nick: nick, User: "u", Realname: "r", Caps: []string{}})
	events := c.Events()

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		miniRegister(t, srv, nick)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ConnectConn(ctx, tr); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}
	<-scriptDone

	// End the session from the server side. The unsupervised finalizer must close
	// the Events stream so the drain loop below terminates.
	srv.close()

	drained := make(chan struct{})
	go func() {
		for range events {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("Events stream was not closed when the unsupervised session ended")
	}
}

// TestCTCPLimiter checks the token bucket that bounds automatic CTCP replies: a
// full burst is allowed immediately, further replies are denied until time
// passes, refills happen at one per interval, and the bucket is capped at the
// burst size so a long idle period cannot grant an unbounded back-to-back run.
func TestCTCPLimiter(t *testing.T) {
	base := time.Now()
	var l ctcpLimiter

	for i := 0; i < ctcpBurst; i++ {
		if !l.allow(base) {
			t.Fatalf("reply %d within the initial burst was denied", i)
		}
	}
	if l.allow(base) {
		t.Errorf("a reply past the burst was allowed with no time elapsed")
	}

	// One refill interval grants exactly one more.
	if !l.allow(base.Add(ctcpRefill)) {
		t.Errorf("a reply was not allowed after a full refill interval")
	}
	if l.allow(base.Add(ctcpRefill)) {
		t.Errorf("two replies allowed after only one refill interval")
	}

	// After a long idle, the bucket is capped at the burst, not refilled without
	// bound: only ctcpBurst back-to-back replies are permitted.
	allowed := 0
	for i := 0; i < ctcpBurst*3; i++ {
		if l.allow(base.Add(time.Hour)) {
			allowed++
		}
	}
	if allowed != ctcpBurst {
		t.Errorf("after a long idle, allowed %d back-to-back, want the burst cap %d", allowed, ctcpBurst)
	}
}
