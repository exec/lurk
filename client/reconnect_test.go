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

// miniRegister drives the smallest possible registration on the mock server's
// side: CAP LS with no offered caps, then NICK/USER, CAP END, and 001.
func miniRegister(t *testing.T, srv *mockServer, nick string) {
	t.Helper()
	srv.expect("CAP LS 302")
	srv.expect("NICK " + nick)
	srv.expectPrefix("USER ")
	srv.send("CAP * LS :")
	srv.expect("CAP END")
	srv.send(":server 001 " + nick + " :Welcome")
}

// TestAutoReconnect verifies that an unexpected disconnect triggers a re-dial,
// re-registration, and a re-join of the channels the client was in — surfacing
// @reconnecting/@reconnected events — all over the same Events stream.
func TestAutoReconnect(t *testing.T) {
	const nick = "rcbot"

	var mu sync.Mutex
	var attempts int
	var down atomic.Bool
	testDone := make(chan struct{})
	defer close(testDone)

	// The injected dialer hands out a fresh scripted connection each time. The
	// first attempt registers, accepts a JOIN #x, then drops the link; the second
	// registers and accepts the automatic re-JOIN, then stays up until teardown.
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
			srv.expect("JOIN #x")
			srv.send(":" + nick + " JOIN #x")
			if n == 1 {
				srv.close() // simulate an unexpected drop after the first session
				return
			}
			<-testDone // keep the second session open until the test ends
		}()
		return conn.NewConn(clientSide, conn.Options{}), nil
	}

	reconnecting := make(chan struct{}, 4)
	reconnected := make(chan struct{}, 4)
	c.HandleReconnecting(func(*Event) { reconnecting <- struct{}{} })
	c.HandleReconnected(func(*Event) { reconnected <- struct{}{} })
	defer c.Close()
	defer down.Store(true) // registered after c.Close so it runs first (LIFO)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Join #x on the first session so it becomes part of the re-join set.
	if err := c.Join("#x"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !waitFor(func() bool { return len(c.Channels()) == 1 }, 2*time.Second) {
		t.Fatalf("client did not track #x before the drop: %v", c.Channels())
	}

	// The drop should trigger a reconnect.
	select {
	case <-reconnecting:
	case <-time.After(3 * time.Second):
		t.Fatal("no @reconnecting event after the drop")
	}
	select {
	case <-reconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("no @reconnected event after re-dial")
	}

	// After reconnect the client is registered again and re-joined #x (the second
	// script asserted the JOIN and confirmed it).
	if !waitFor(func() bool { return c.Nick() == nick && len(c.Channels()) == 1 }, 2*time.Second) {
		t.Errorf("post-reconnect state wrong: nick=%q channels=%v", c.Nick(), c.Channels())
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got < 2 {
		t.Errorf("dial attempts = %d, want >= 2 (a reconnect)", got)
	}
}

// TestQuitStopsReconnect verifies that a user Quit prevents the supervisor from
// re-dialing: the connection ends and the Events stream closes instead.
func TestQuitStopsReconnect(t *testing.T) {
	const nick = "qbot"
	var attempts int
	var mu sync.Mutex
	var down atomic.Bool

	// gotQuit carries the exact QUIT line the server received, proving the
	// message was not dropped by stopping the supervisor too early.
	gotQuit := make(chan string, 1)

	dialer := func(ctx context.Context) (transport, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		clientSide, serverSide := net.Pipe()
		srv := newMockServer(t, serverSide)
		srv.down = &down
		go func() {
			miniRegister(t, srv, nick)
			// Wait for the client's QUIT, capture it, then drop the link.
			line := srv.expectPrefix("QUIT")
			gotQuit <- line
			srv.close()
		}()
		return conn.NewConn(clientSide, conn.Options{}), nil
	}

	c := New(Config{Nick: nick, User: "u", Realname: "r", Server: "x:1", Caps: []string{}, AutoReconnect: true})
	c.dial = dialer
	defer down.Store(true) // teardown: silence post-assertion scripted failures

	events := c.Events()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Quit must stop the supervisor: the connection drops and is NOT re-dialed.
	if err := c.Quit("bye"); err != nil {
		t.Fatalf("Quit: %v", err)
	}

	// The QUIT line must actually have reached the server (it is sent before the
	// supervisor is stopped, so it is not raced away by the teardown).
	select {
	case line := <-gotQuit:
		if want := "QUIT bye"; line != want {
			t.Errorf("server received %q, want %q", line, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server never received the QUIT line")
	}

	// Done closes once the client has stopped for good.
	select {
	case <-c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("client did not shut down after Quit")
	}
	// The Events stream is closed on final shutdown (consumers learn the session
	// ended); draining it must terminate.
	drained := make(chan struct{})
	go func() {
		for range events {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("Events stream was not closed on shutdown")
	}

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Errorf("dial attempts = %d, want exactly 1 (no reconnect after Quit)", got)
	}
}

// TestReconnectBackoffResetsOnSuccess verifies that reconnectLoop resets
// backoff to reconnectInitialBackoff after each successful reconnect. Without
// the reset, a flapping upstream (connect → 001 → drop, repeated) causes the
// backoff to plateau at reconnectMaxBackoff, making each subsequent reconnect
// wait the full maximum delay.
//
// The test injects a fast-clock by substituting the client's dial function with
// one that:
//   - counts dial wall-clock timestamps
//   - connects and registers quickly on every call
//   - drops the link immediately after registration (simulating a flap)
//
// After N flap cycles the delays between consecutive dials are measured.
// With the fix each gap stays near 0 (no meaningful backoff between a
// successful reconnect and the next cycle's initial drop). Without the fix the
// gaps would grow to reconnectMaxBackoff (30 s) after a few cycles and the
// test would time out or measure large delays.
//
// Implementation note: reconnectLoop sleeps for backoff before re-dialing —
// we observe the time between dial calls to infer the effective backoff.
func TestReconnectBackoffResetsOnSuccess(t *testing.T) {
	const nick = "flapbot"
	const cycles = 3 // number of flap cycles to observe

	var mu sync.Mutex
	var dialTimes []time.Time
	var down atomic.Bool
	testDone := make(chan struct{})
	defer close(testDone)

	// Each dial call: record the wall-clock time, register immediately, then
	// drop the link so supervise re-enters reconnectLoop immediately.
	c := New(Config{Nick: nick, User: "u", Realname: "r", Server: "x:1", Caps: []string{}, AutoReconnect: true})
	c.dial = func(ctx context.Context) (transport, error) {
		mu.Lock()
		dialTimes = append(dialTimes, time.Now())
		n := len(dialTimes)
		mu.Unlock()

		clientSide, serverSide := net.Pipe()
		srv := newMockServer(t, serverSide)
		srv.down = &down
		go func() {
			miniRegister(t, srv, nick)
			if n < cycles+1 {
				// Flap: drop immediately after registration.
				srv.close()
				return
			}
			// Final session: stay up until the test ends.
			<-testDone
		}()
		return conn.NewConn(clientSide, conn.Options{}), nil
	}

	defer c.Close()
	defer down.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Wait for all cycles to complete (the final, stable dial has been placed).
	if !waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(dialTimes) >= cycles+1
	}, 8*time.Second) {
		mu.Lock()
		n := len(dialTimes)
		mu.Unlock()
		t.Fatalf("only %d dial attempts within timeout; want %d", n, cycles+1)
	}

	mu.Lock()
	times := make([]time.Time, len(dialTimes))
	copy(times, dialTimes)
	mu.Unlock()

	// Measure the gap between consecutive dials after cycle 1. With backoff
	// reset each gap should be <= reconnectInitialBackoff (1 s) + a generous
	// slop for CI jitter. Without the fix the gaps grow toward 30 s.
	//
	// We skip the gap between dial[0] and dial[1] because that represents the
	// drop + initial sleep; we want the gaps AFTER a successful reconnect.
	const maxAllowedGap = 5 * time.Second // far below the 30 s plateau
	for i := 2; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		if gap > maxAllowedGap {
			t.Errorf("gap between dial[%d] and dial[%d] = %v; want <= %v (backoff not reset after reconnect?)",
				i-1, i, gap, maxAllowedGap)
		}
	}
}
