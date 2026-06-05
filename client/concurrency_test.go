package client

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lurk/conn"
)

// TestReconnectRaceAgainstReaders drives several reconnect cycles while caller
// goroutines concurrently invoke the public read surface that loads the session
// pointers: Err → c.tr and CapEnabled → c.neg + the enabled map. The reconnect
// supervisor reassigns c.tr and c.neg in startSession on each re-dial, so
// without the trMu guard the -race detector reports an unsynchronized read/write
// of c.tr (Err's load racing startSession's store) or of c.neg (CapEnabled's
// load racing the store), and without the Negotiator's internal mutex it reports
// a "concurrent map read and map write" on the enabled set (CapEnabled's
// IsEnabled racing the run-loop's Receive). With both fixes this is clean.
//
// The reader surface performs no network I/O, so the writers cannot wedge on an
// undrained net.Pipe — the test stays fast and cannot deadlock at teardown.
//
// Run with: go test -race -run TestReconnectRace -count=5 ./client/
func TestReconnectRaceAgainstReaders(t *testing.T) {
	const nick = "racebot"

	var mu sync.Mutex
	var attempts int
	const maxSessions = 3
	testDone := make(chan struct{})
	var down atomic.Bool // set at teardown to silence post-shutdown scripted failures

	c := New(Config{
		Nick: nick, User: "u", Realname: "r", Server: "x:1",
		// Offer caps the negotiator will REQ/ACK so the enabled map is actively
		// written (the run-loop's Receive) on each session while CapEnabled reads it.
		Caps:          []string{"message-tags", "server-time", "batch"},
		AutoReconnect: true,
	})
	c.dial = func(ctx context.Context) (transport, error) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()

		clientSide, serverSide := net.Pipe()
		srv := newMockServer(t, serverSide)
		srv.down = &down
		go func() {
			// Registration that actually negotiates caps (writer side of Fix 2).
			srv.expect("CAP LS 302")
			srv.expect("NICK " + nick)
			srv.expectPrefix("USER ")
			srv.send("CAP * LS :message-tags server-time batch")
			srv.expectPrefix("CAP REQ ")
			srv.send("CAP * ACK :message-tags server-time batch")
			srv.expect("CAP END")
			srv.send(":server 001 " + nick + " :Welcome")

			if n < maxSessions {
				// Drop the link so the supervisor re-dials, reassigning c.tr/c.neg
				// while the readers below are mid-flight.
				srv.close()
				return
			}
			<-testDone // hold the final session open until teardown
			srv.close()
		}()
		return conn.NewConn(clientSide, conn.Options{}), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	// Spin reader goroutines that hammer the pointer-loading surface while the
	// supervisor reconnects underneath them.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.CapEnabled("message-tags")
				_ = c.CapEnabled("server-time")
				_ = c.Err()
			}
		}()
	}

	// Let the reconnect cycles run to completion (until the final session sticks).
	waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts >= maxSessions
	}, 8*time.Second)

	// Give the final session a moment to register, then stop everything.
	waitFor(func() bool { return c.Nick() == nick }, 2*time.Second)

	down.Store(true) // entering teardown
	close(stop)
	wg.Wait()
	close(testDone)
	_ = c.Close()
}

// TestCapEnabledDuringReconnectSwap focuses Fix 1's c.neg pointer: CapEnabled
// reads c.neg while startSession replaces it on each re-dial. It forces several
// session swaps and reads CapEnabled from a parallel goroutine. CapEnabled does
// no network I/O (it only loads the pointer and reads the enabled map under the
// Negotiator's lock), so no draining is needed. Without trMu the read of the
// c.neg pointer races the store in startSession; the -race detector flags it.
func TestCapEnabledDuringReconnectSwap(t *testing.T) {
	const nick = "swapbot"
	var mu sync.Mutex
	var attempts int
	const maxSessions = 3
	testDone := make(chan struct{})
	var down atomic.Bool // set at teardown to silence post-shutdown scripted failures

	c := New(Config{
		Nick: nick, User: "u", Realname: "r", Server: "x:1",
		Caps: []string{"sasl"}, AutoReconnect: true,
	})
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
			if n < maxSessions {
				srv.close()
				return
			}
			<-testDone
			srv.close()
		}()
		return conn.NewConn(clientSide, conn.Options{}), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = c.CapEnabled("sasl")
			}
		}
	}()

	waitFor(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return attempts >= maxSessions
	}, 8*time.Second)
	waitFor(func() bool { return c.Nick() == nick }, 2*time.Second)

	down.Store(true) // entering teardown
	close(stop)
	wg.Wait()
	close(testDone)
	_ = c.Close()
}
