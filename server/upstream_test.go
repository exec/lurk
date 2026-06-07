package server

// Phase 3a hermetic tests for the upstream session manager.
//
// The test-injection seam is Manager.dialers (unexported): each entry maps a netid to a
// function that returns one end of a net.Pipe pair. The other end is a
// "scripted upstream" — a small goroutine that speaks the server side of the
// IRC registration handshake and then drives a scripted conversation.
//
// Scripted upstream helpers live in scriptedUpstream (similar to the
// client/integration_test.go mockServer, but for the upstream side where lurkd
// is the client).

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/client"
)

// ─── scripted upstream helper ─────────────────────────────────────────────────

// scriptedUpstream drives the server side of a net.Pipe for one upstream
// connection. The lurkd Manager's Client is on the other end; this helper
// speaks the upstream IRC server.
type scriptedUpstream struct {
	t    *testing.T
	c    net.Conn
	br   *bufio.Reader
	once sync.Once

	// down, when non-nil and set, silences scripted failures once teardown
	// begins — mirrors the client package's mockServer.down pattern so a
	// reconnect dial fired just before shutdown does not fail a completed test.
	down *atomic.Bool
}

func newScriptedUpstream(t *testing.T, serverSide net.Conn) *scriptedUpstream {
	t.Helper()
	return &scriptedUpstream{t: t, c: serverSide, br: bufio.NewReader(serverSide)}
}

// fail marks the test failed and causes the goroutine to exit. Because the
// script runs in its own goroutine we use Errorf (goroutine-safe) + Goexit
// rather than Fatalf (only legal on the test goroutine).
func (s *scriptedUpstream) fail(format string, args ...any) {
	if s.down != nil && s.down.Load() {
		s.close()
		runtime.Goexit()
	}
	s.t.Helper()
	s.t.Errorf(format, args...)
	s.close()
	runtime.Goexit()
}

func (s *scriptedUpstream) readLine() string {
	s.t.Helper()
	_ = s.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := s.br.ReadString('\n')
	if err != nil {
		s.fail("scripted upstream read: %v (got %q)", err, line)
	}
	return strings.TrimRight(line, "\r\n")
}

// readLineSkipAway reads lines, silently skipping any AWAY (or "AWAY ...")
// messages that lurkd emits as part of its attach/detach away management.
// This prevents upstream test scripts from accidentally consuming the AWAY
// line when they are waiting for a relayed command.
func (s *scriptedUpstream) readLineSkipAway() string {
	s.t.Helper()
	for {
		line := s.readLine()
		if !strings.HasPrefix(line, "AWAY") {
			return line
		}
		// AWAY/BACK from bouncer internal logic — silently consumed.
	}
}

func (s *scriptedUpstream) expect(want string) {
	s.t.Helper()
	got := s.readLine()
	if got != want {
		s.fail("lurkd client sent %q, want %q", got, want)
	}
}

func (s *scriptedUpstream) expectPrefix(prefix string) string {
	s.t.Helper()
	got := s.readLine()
	if !strings.HasPrefix(got, prefix) {
		s.fail("lurkd client sent %q, want prefix %q", got, prefix)
	}
	return got
}

func (s *scriptedUpstream) send(lines ...string) {
	s.t.Helper()
	_ = s.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	for _, l := range lines {
		if _, err := s.c.Write([]byte(l + "\r\n")); err != nil {
			s.fail("scripted upstream write %q: %v", l, err)
		}
	}
}

func (s *scriptedUpstream) close() { s.once.Do(func() { _ = s.c.Close() }) }

// register drives the minimal upstream registration handshake: CAP LS (no
// offered caps), NICK/USER, CAP END, 001 welcome. nick must match the
// client.Config.Nick used in the Manager.
func (s *scriptedUpstream) register(nick string) {
	s.t.Helper()
	s.expect("CAP LS 302")
	s.expect("NICK " + nick)
	s.expectPrefix("USER ")
	s.send("CAP * LS :") // no caps offered
	s.expect("CAP END")
	s.send(":upstream.local 001 " + nick + " :Welcome to the test network")
	s.send(":upstream.local 376 " + nick + " :End of /MOTD command.")
}

// expectJoin reads the next line and asserts it is "JOIN #channel".
func (s *scriptedUpstream) expectJoin(channel string) {
	s.t.Helper()
	s.expect("JOIN " + channel)
}

// sendJoinConfirm sends the server-side confirmation of a JOIN.
func (s *scriptedUpstream) sendJoinConfirm(nick, channel string) {
	s.t.Helper()
	s.send(
		":"+nick+"!~u@host JOIN "+channel,
		":upstream.local 353 "+nick+" = "+channel+" :"+nick,
		":upstream.local 366 "+nick+" "+channel+" :End of /NAMES list.",
	)
}

// ─── collecting Sink ─────────────────────────────────────────────────────────

// collectSink collects all ingested events. It is concurrency-safe.
type collectSink struct {
	mu     sync.Mutex
	events []sinkEvent
}

type sinkEvent struct {
	netid int
	ev    *client.Event
}

func (s *collectSink) Ingest(netid int, ev *client.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, sinkEvent{netid: netid, ev: ev})
}

// waitForCommand polls until an event with the given IRC command and netid
// arrives in the sink, or the timeout expires. Returns the matching event or nil.
func (s *collectSink) waitForCommand(netid int, cmd string, timeout time.Duration) *client.Event {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, se := range s.events {
			if se.netid == netid && se.ev.Command() == cmd {
				ev := se.ev
				s.mu.Unlock()
				return ev
			}
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// waitForPrivmsg polls until a PRIVMSG with the given text is in the sink.
func (s *collectSink) waitForPrivmsg(netid int, text string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, se := range s.events {
			if se.netid == netid && se.ev.Command() == "PRIVMSG" && se.ev.Text() == text {
				s.mu.Unlock()
				return true
			}
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// ─── pipeDialer helper ───────────────────────────────────────────────────────

// pipeDialer returns a Dialer that, on each invocation, produces one end of a
// fresh net.Pipe, starts the provided script goroutine on the other end, and
// returns the client side. The script function receives the server side.
//
// The down flag is stored on the scriptedUpstream so post-teardown failures
// are suppressed.
func pipeDialer(t *testing.T, down *atomic.Bool, script func(*scriptedUpstream)) dialer {
	t.Helper()
	return func(_ context.Context) (net.Conn, error) {
		clientSide, serverSide := net.Pipe()
		su := newScriptedUpstream(t, serverSide)
		su.down = down
		go func() {
			defer su.close() // ensure the server side is released when the script exits
			script(su)
		}()
		return clientSide, nil
	}
}

// ─── TestUpstreamRegistersAndJoins ───────────────────────────────────────────

// TestUpstreamRegistersAndJoins verifies that the Manager connects upstream,
// receives the welcome, joins the autojoin channel, and that a subsequent
// PRIVMSG from the scripted upstream reaches the Sink with the correct netid.
func TestUpstreamRegistersAndJoins(t *testing.T) {
	const (
		nick    = "botnick"
		channel = "#lurk"
		netid   = 1
		msgText = "hello from upstream"
	)

	testDone := make(chan struct{})
	privmsgSent := make(chan struct{})
	var down atomic.Bool

	// Register down.Store BEFORE the mgr.Close cleanup so that it runs AFTER
	// mgr.Close in LIFO order: this ensures down is already true when the
	// connection closes, silencing any scripted-goroutine failures on teardown.
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		// Wait until the test signals we may send the PRIVMSG.
		<-privmsgSent
		su.send(":peer!p@host PRIVMSG " + channel + " :" + msgText)
		// Hold the connection open until testDone; ignore the close error from teardown.
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "TestNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netid: dialFn}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	// mgr.Close registered AFTER down+testDone cleanups so it runs FIRST (LIFO).
	t.Cleanup(func() { mgr.Close() })

	// Wait for the JOIN to appear in the sink (the client confirms the channel
	// join with a server-sent JOIN that the run loop tracks and dispatches).
	if ev := sink.waitForCommand(netid, "JOIN", 3*time.Second); ev == nil {
		t.Fatal("timed out waiting for JOIN event in Sink")
	}

	// Signal the scripted upstream to send the PRIVMSG.
	close(privmsgSent)

	if !sink.waitForPrivmsg(netid, msgText, 3*time.Second) {
		t.Errorf("PRIVMSG %q not seen in Sink within timeout", msgText)
	}
}

// ─── TestUpstreamReconnect ────────────────────────────────────────────────────

// TestUpstreamReconnect verifies [B#12]: when the upstream drops, the Manager's
// HandleReconnecting hook fires (transition to ConnReconnecting), the upstream
// re-dials and re-registers (transition to ConnConnected), and a PRIVMSG sent
// on the second connection reaches the Sink.
func TestUpstreamReconnect(t *testing.T) {
	const (
		nick    = "rcnick"
		channel = "#rc"
		netid   = 2
	)

	var mu sync.Mutex
	var dialCount int
	testDone := make(chan struct{})

	var down atomic.Bool
	// Cleanup order (LIFO): mgr.Close → down.Store → close(testDone).
	// mgr.Close runs first so the client stops reconnecting; down.Store is set
	// before testDone closes so post-shutdown readLine failures are silenced
	// (though the second-connection goroutine is at <-testDone, not I/O).
	t.Cleanup(func() { close(testDone) })
	t.Cleanup(func() { down.Store(true) })

	const privmsgAfterReconnect = "after-reconnect"

	dialFn := func(_ context.Context) (net.Conn, error) {
		mu.Lock()
		dialCount++
		n := dialCount
		mu.Unlock()

		clientSide, serverSide := net.Pipe()
		su := newScriptedUpstream(t, serverSide)
		su.down = &down
		go func() {
			defer su.close()
			su.register(nick)
			su.expectJoin(channel)
			su.sendJoinConfirm(nick, channel)
			if n == 1 {
				// First connection: drop after join to trigger reconnect.
				return
			}
			// Second connection: send a PRIVMSG then hold open until test ends.
			su.send(":peer!p@host PRIVMSG " + channel + " :" + privmsgAfterReconnect)
			<-testDone
		}()
		return clientSide, nil
	}

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "RCNet",
				Addr:     "rc.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netid: dialFn}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	// mgr.Close registered last so it runs FIRST in LIFO cleanup order.
	t.Cleanup(func() { mgr.Close() })

	// Wait for reconnecting state (upstream drops → supervisor fires
	// HandleReconnecting → connState becomes ConnReconnecting).
	if !waitForUpstreamState(mgr, netid, ConnReconnecting, 5*time.Second) {
		st, _ := mgr.UpstreamState(netid)
		t.Fatalf("upstream did not enter ConnReconnecting; state=%v", st)
	}

	// Wait for reconnected state (supervisor re-dials → HandleReconnected fires).
	if !waitForUpstreamState(mgr, netid, ConnConnected, 5*time.Second) {
		st, _ := mgr.UpstreamState(netid)
		t.Fatalf("upstream did not re-enter ConnConnected; state=%v", st)
	}

	// After reconnect a PRIVMSG from the second connection must reach the Sink.
	if !sink.waitForPrivmsg(netid, privmsgAfterReconnect, 3*time.Second) {
		t.Errorf("PRIVMSG %q not seen in Sink after reconnect", privmsgAfterReconnect)
	}

	mu.Lock()
	got := dialCount
	mu.Unlock()
	if got < 2 {
		t.Errorf("dial attempts = %d, want >= 2 (a reconnect happened)", got)
	}
}

// waitForUpstreamState polls until the Manager reports the given status for the
// netid, or the timeout expires.
func waitForUpstreamState(m *Manager, netid int, want ConnStatus, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, ok := m.UpstreamState(netid); ok && st == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, ok := m.UpstreamState(netid)
	return ok && st == want
}

// ─── TestUpstreamShutdown ─────────────────────────────────────────────────────

// TestUpstreamShutdown verifies that Close shuts all upstream clients down and
// that no goroutine leaks. The race detector covers the latter; we also confirm
// the client's Done channel closes promptly after Close.
func TestUpstreamShutdown(t *testing.T) {
	const (
		nick  = "shutnick"
		netid = 3
	)

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() {
		down.Store(true)
		close(testDone)
	})

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		<-testDone // hold the connection open until test ends
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "ShutNet",
				Addr:     "shut.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
			},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netid: dialFn}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}

	// Wait for the welcome to arrive (registration complete).
	if ev := sink.waitForCommand(netid, "001", 3*time.Second); ev == nil {
		t.Fatal("timed out waiting for 001 in Sink")
	}

	// Grab the client's Done channel before calling Close.
	var clientDone <-chan struct{}
	for _, e := range mgr.upstreams {
		if e.netid == netid {
			clientDone = e.client.Done()
		}
	}
	if clientDone == nil {
		t.Fatal("no upstream entry found for netid")
	}

	mgr.Close()

	select {
	case <-clientDone:
		// Good: the upstream client stopped.
	case <-time.After(3 * time.Second):
		t.Error("upstream client did not stop within 3s of Manager.Close")
	}
}

// ─── TestSinkConcurrencySafe ─────────────────────────────────────────────────

// TestSinkConcurrencySafe verifies that collectSink (and any real Sink) can
// safely handle concurrent Ingest calls without data races. The race detector
// checks this. The test is intentionally lightweight — the goal is to exercise
// concurrent writes, not to test manager logic.
func TestSinkConcurrencySafe(t *testing.T) {
	sink := &collectSink{}
	const goroutines = 20
	const events = 50

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < events; i++ {
				// Synthesize a minimal event (real Message not needed here).
				sink.Ingest(g, &client.Event{})
			}
		}()
	}
	wg.Wait()

	sink.mu.Lock()
	total := len(sink.events)
	sink.mu.Unlock()
	if total != goroutines*events {
		t.Errorf("collected %d events, want %d", total, goroutines*events)
	}
}

// ─── TestResilientStartup ─────────────────────────────────────────────────────

// TestResilientStartup verifies Phase 9's resilient-startup requirement:
// a 2-network config where network A's dialer succeeds and network B's dialer
// fails initially then succeeds on retry. The daemon must:
//   - serve network A immediately (Start returns, A is ConnConnected)
//   - leave network B as ConnDisconnected (not abort the whole Start)
//   - connect network B after a background retry (short backoff)
//   - stop all retry goroutines cleanly when Close is called (no leak, race-clean)
func TestResilientStartup(t *testing.T) {
	const (
		nickA  = "nicka"
		nickB  = "nickb"
		netidA = 10
		netidB = 11
	)

	var down atomic.Bool
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	t.Cleanup(func() { down.Store(true) })

	// dialA: always succeeds.
	dialA := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nickA)
		<-testDone
	})

	// dialB: fails the first call, succeeds on the second.
	var bDialCount int
	var bMu sync.Mutex
	bConnectedCh := make(chan struct{})
	dialB := func(_ context.Context) (net.Conn, error) {
		bMu.Lock()
		n := bDialCount
		bDialCount++
		bMu.Unlock()

		if n == 0 {
			// First call: fail to simulate unreachable network.
			return nil, fmt.Errorf("test: network B unreachable (dial %d)", n)
		}
		// Second call: succeed.
		clientSide, serverSide := net.Pipe()
		su := newScriptedUpstream(t, serverSide)
		su.down = &down
		go func() {
			defer su.close()
			su.register(nickB)
			close(bConnectedCh)
			<-testDone
		}()
		return clientSide, nil
	}

	cfg := &Config{
		Networks: []Network{
			{NetID: netidA, Name: "NetA", Addr: "a.local:6667", Identity: Identity{Nick: nickA, User: "u", Realname: "r"}},
			{NetID: netidB, Name: "NetB", Addr: "b.local:6667", Identity: Identity{Nick: nickB, User: "u", Realname: "r"}},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netidA: dialA, netidB: dialB}
	// Very short backoff so the test is fast.
	mgr.retryBase = 20 * time.Millisecond
	mgr.retryMax = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Start must return nil even though network B fails initially.
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v (want nil — resilient startup)", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Network A must be connected.
	if st, ok := mgr.UpstreamState(netidA); !ok || st != ConnConnected {
		t.Errorf("network A state = %v, want ConnConnected", st)
	}

	// Network B must be disconnected (initial connect failed).
	if st, ok := mgr.UpstreamState(netidB); !ok || st != ConnDisconnected {
		t.Errorf("network B state = %v, want ConnDisconnected immediately after Start", st)
	}

	// Wait for network B to connect via the background retry.
	select {
	case <-bConnectedCh:
		// Good: background retry succeeded.
	case <-time.After(3 * time.Second):
		t.Fatal("network B did not connect via background retry within 3s")
	}

	// After retry, network B must be ConnConnected.
	if !waitForUpstreamState(mgr, netidB, ConnConnected, 3*time.Second) {
		st, _ := mgr.UpstreamState(netidB)
		t.Errorf("network B state = %v after retry, want ConnConnected", st)
	}

	// Verify that network A saw events in the sink (it served immediately).
	if ev := sink.waitForCommand(netidA, "001", 3*time.Second); ev == nil {
		t.Error("network A 001 event not seen in sink")
	}

	// Close must stop all goroutines cleanly. The race detector will catch leaks.
	mgr.Close()
}

// ─── TestResilientStartupCloseBeforeRetry ─────────────────────────────────────

// TestResilientStartupCloseBeforeRetry verifies that calling Close before a
// retry goroutine attempts a reconnect stops the goroutine cleanly (no leak,
// no post-Close connect attempt). This exercises the stop-channel interrupt.
func TestResilientStartupCloseBeforeRetry(t *testing.T) {
	const (
		nickC  = "nickc"
		netidC = 12
	)
	var down atomic.Bool
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	t.Cleanup(func() { down.Store(true) })

	// dialC: always fails — retry goroutine should be stopped by Close.
	dialC := func(_ context.Context) (net.Conn, error) {
		return nil, fmt.Errorf("test: network C always unreachable")
	}

	cfg := &Config{
		Networks: []Network{
			{NetID: netidC, Name: "NetC", Addr: "c.local:6667", Identity: Identity{Nick: nickC, User: "u", Realname: "r"}},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netidC: dialC}
	// Long enough that Close fires before the first retry.
	mgr.retryBase = 500 * time.Millisecond
	mgr.retryMax = 500 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}

	// Close immediately. The retry goroutine must stop — retryWg.Wait in Close
	// ensures we don't return before the goroutine exits.
	done := make(chan struct{})
	go func() {
		mgr.Close()
		close(done)
	}()
	select {
	case <-done:
		// Good.
	case <-time.After(3 * time.Second):
		t.Error("Manager.Close did not return within 3s — retry goroutine leak?")
	}
}

// ─── TestAddDuplicateNetIDReplacesOld ─────────────────────────────────────────

// TestAddDuplicateNetIDReplacesOld verifies the TOCTOU guard in Manager.Add:
// calling Add twice for the same netid (as two concurrent CHANGENETWORK
// sessions could do) must result in exactly one upstream entry, and the
// older connection must be closed (its Done channel closes promptly).
//
// Without the guard, two entries would exist for the same netid, causing
// duplicate fan-out and an unbounded resource leak.
func TestAddDuplicateNetIDReplacesOld(t *testing.T) {
	const (
		nick  = "dupnick"
		netid = 42
	)

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() {
		down.Store(true)
		close(testDone)
	})

	cfg := &Config{
		Networks: []Network{
			{NetID: netid, Name: "DupNet", Addr: "dup.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"}},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{
		netid: pipeDialer(t, &down, func(su *scriptedUpstream) {
			su.register(nick)
			<-testDone
		}),
	}
	t.Cleanup(func() { mgr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First Add — the normal path (simulates CHANGENETWORK re-dial).
	if err := mgr.Add(ctx, &cfg.Networks[0]); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Grab the first client's Done channel before the second Add closes it.
	mgr.mu.RLock()
	var firstDone <-chan struct{}
	for _, e := range mgr.upstreams {
		if e.netid == netid {
			firstDone = e.client.Done()
			break
		}
	}
	mgr.mu.RUnlock()
	if firstDone == nil {
		t.Fatal("first upstream entry not found after first Add")
	}

	// Second Add for the same netid — must evict and close the first entry.
	if err := mgr.Add(ctx, &cfg.Networks[0]); err != nil {
		t.Fatalf("second Add: %v", err)
	}

	// The old upstream must have been closed.
	select {
	case <-firstDone:
		// Good: old client was closed.
	case <-time.After(3 * time.Second):
		t.Error("old upstream client was not closed after duplicate Add")
	}

	// Exactly one entry for this netid must remain.
	mgr.mu.RLock()
	var count int
	for _, e := range mgr.upstreams {
		if e.netid == netid {
			count++
		}
	}
	mgr.mu.RUnlock()
	if count != 1 {
		t.Errorf("upstream entry count for netid=%d = %d after duplicate Add, want 1", netid, count)
	}
}

// ─── TestUpstreamRegistrationTimeout ─────────────────────────────────────────

// TestUpstreamRegistrationTimeout verifies that scheduleRetry bounds each
// Connect attempt with Manager.registrationTimeout: a hostile upstream that
// accepts the TCP connection but never sends RPL_WELCOME must not park the
// retry goroutine forever, which would block Manager.Close via retryWg.Wait.
//
// Without the fix, Manager.Close would never return in this test (the retry
// goroutine blocks on <-c.registered with context.Background()). With the fix,
// the registration attempt times out after registrationTimeout and Close
// returns promptly.
func TestUpstreamRegistrationTimeout(t *testing.T) {
	const (
		nickD  = "nickd"
		netidD = 20
	)

	var down atomic.Bool
	testDone := make(chan struct{})
	t.Cleanup(func() { close(testDone) })
	t.Cleanup(func() { down.Store(true) })

	// dialD: accepts the connection but sends nothing — simulates a hostile
	// upstream that stalls registration indefinitely.
	dialD := pipeDialer(t, &down, func(su *scriptedUpstream) {
		// Intentionally do nothing: do not complete the registration handshake.
		// Hold the connection open until the test ends (or we are closed first).
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{NetID: netidD, Name: "StallNet", Addr: "stall.local:6667",
				Identity: Identity{Nick: nickD, User: "u", Realname: "r"}},
		},
	}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netidD: dialD}
	// Very short backoff so the retry fires quickly.
	mgr.retryBase = 20 * time.Millisecond
	mgr.retryMax = 50 * time.Millisecond
	// Short registration timeout — far shorter than the test budget — so the
	// stalling upstream is detected fast without making the test slow.
	mgr.registrationTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Start must return non-nil (initial connect fails: no registration sent).
	// The network is marked ConnDisconnected and a retry goroutine is spawned.
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}

	// Let one retry cycle fire (backoff + registration timeout).
	time.Sleep(mgr.retryBase + mgr.registrationTimeout + 50*time.Millisecond)

	// Close must return well within 1 s. Without the fix it blocks forever
	// because the retry goroutine is parked on <-c.registered.
	closeDone := make(chan struct{})
	go func() {
		mgr.Close()
		close(closeDone)
	}()

	select {
	case <-closeDone:
		// Good: Close returned promptly.
	case <-time.After(2 * time.Second):
		t.Error("Manager.Close did not return within 2 s — retry goroutine leaked (registration timeout not applied?)")
	}
}
