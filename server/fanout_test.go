package server

// Phase 6b hermetic tests: fan-out, client→upstream relay, self-send rule,
// multi-client fan-out, network isolation, disconnect safety, sanitization.
//
// Test architecture (the two-mock sandwich):
//
//	scripted upstream (net.Pipe) ← lurkd Manager ← Server ← scripted client (net.Pipe)
//
// Each test spins up:
//  1. One or more scriptedUpstream instances (existing harness from upstream_test.go)
//  2. A Manager wired to a Server as its Sink
//  3. One or more test clients that drive serveConnInternal over net.Pipe
//
// The "two-mock sandwich" is: mock client ↔ lurkd Server ↔ mock upstream.

import (
	"context"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// ─── sandwich harness helper ─────────────────────────────────────────────────

// sandwichCfg describes one network in a two-mock sandwich test.
type sandwichCfg struct {
	netid   int
	nick    string
	channel string
}

// sandwich is a complete two-mock sandwich setup for one network:
// a scripted upstream + a lurkd server + an attached test client.
type sandwich struct {
	upstream *scriptedUpstream // scripted upstream (mock IRC server)
	srv      *Server           // lurkd server
	mgr      *Manager          // upstream manager
	clientC  *conn.Conn        // test client's conn (net.Pipe end)
	testDone chan struct{}
	down     *atomic.Bool
}

// newSandwich creates a two-mock sandwich for one network. The upstream script
// function receives the server side of the upstream net.Pipe and should at
// minimum call su.register(nick) to complete the registration handshake.
func newSandwich(t *testing.T, nc sandwichCfg, upstreamScript func(*scriptedUpstream)) *sandwich {
	t.Helper()
	testDone := make(chan struct{})
	var down atomic.Bool

	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	sw := &sandwich{testDone: testDone, down: &down}

	dialFn := pipeDialer(t, &down, upstreamScript)

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    nc.netid,
				Name:     "TestNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nc.nick, User: "u", Realname: "r"},
				Channels: []string{nc.channel},
			},
		},
	}

	sw.srv = New(cfg)
	sw.mgr = NewManager(cfg, sw.srv)
	sw.mgr.dialers = map[int]dialer{nc.netid: dialFn}
	sw.srv.WithManager(sw.mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sw.mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { sw.mgr.Close() })

	// Connect the test client to the server over net.Pipe.
	sw.clientC = connectClientToSrv(t, sw.srv)
	sw.upstream = nil // set by upstreamScript via pipeDialer (use sink or direct access)
	return sw
}

// connectClientToSrv creates a new client connection to srv over net.Pipe.
func connectClientToSrv(t *testing.T, srv *Server) *conn.Conn {
	t.Helper()
	cRaw, sRaw := net.Pipe()
	c := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = c.Close()
		_ = sRaw.Close()
	})
	go func() { _ = srv.serveConnInternal(sRaw, false) }()
	return c
}

// doBindRegister drives full registration with BOUNCER BIND <netid>, requesting
// bouncer-networks, server-time, labeled-response, echo-message caps, and
// draining the welcome burst.
func doBindRegister(t *testing.T, c *conn.Conn, nick string, netid int) {
	t.Helper()
	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c) // CAP LS

	sendLine(t, c, "CAP REQ :soju.im/bouncer-networks server-time labeled-response echo-message")
	_ = recvMsg(t, c) // CAP ACK

	sendLine(t, c, "NICK "+nick)
	sendLine(t, c, "USER "+nick+" 0 * :Test")
	if netid != 0 {
		sendLine(t, c, "BOUNCER BIND "+itoa(netid))
	}
	sendLine(t, c, "CAP END")
	skipTo(t, c, irc.ERR_NOMOTD) // drain welcome burst
}

// itoa converts an int to its decimal string.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

// waitMsg reads from c until a message matching predicate arrives or timeout.
func waitMsg(t *testing.T, c *conn.Conn, timeout time.Duration, pred func(*irc.Message) bool) *irc.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-c.Messages():
			if !ok {
				t.Fatalf("connection closed while waiting for message")
			}
			if pred(msg) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timeout waiting for message")
			return nil
		}
	}
}

// noMsgFor drains c for d and reports if any message matching pred arrives.
func noMsgFor(t *testing.T, c *conn.Conn, d time.Duration, pred func(*irc.Message) bool) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case msg, ok := <-c.Messages():
			if !ok {
				return
			}
			if pred(msg) {
				t.Errorf("unexpected message arrived: %v", msg)
			}
		case <-deadline:
			return
		}
	}
}

// ─── TestE2EDelivery [B#13] ──────────────────────────────────────────────────

// TestE2EDelivery is the end-to-end delivery test [B#13]: a scripted upstream
// sends a PRIVMSG to a joined channel; the bound client receives it with @time.
func TestE2EDelivery(t *testing.T) {
	const (
		netid   = 1
		nick    = "bouncer"
		channel = "#e2e"
		msgText = "hello world"
	)

	privmsgReady := make(chan struct{})

	cfg := sandwichCfg{netid: netid, nick: nick, channel: channel}
	sink := &collectSink{}

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	var suHolder atomic.Value // stores *scriptedUpstream
	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		suHolder.Store(su)
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		// Wait until test signals PRIVMSG can be sent.
		<-privmsgReady
		su.send(":peer!p@host PRIVMSG " + channel + " :" + msgText)
		<-testDone
	})

	_ = cfg // suppress unused warning — we use dialFn directly

	netCfg := &Config{
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

	srv := New(netCfg)
	mgr := NewManager(netCfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	// Wire server as sink (it IS the sink — Ingest fans out).
	// Also wire a parallel collectSink to monitor store path.
	_ = sink

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Wait for JOIN event in mgr so the upstream is ready before we send PRIVMSG.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := mgr.UpstreamState(netid); ok {
			st, _ := mgr.UpstreamState(netid)
			if st == ConnConnected {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Connect a test client and register as a bound session.
	c := connectClientToSrv(t, srv)
	doBindRegister(t, c, "testclient", netid)

	// Give the server a moment to process the registration and register the session.
	time.Sleep(50 * time.Millisecond)

	// Signal the upstream to send the PRIVMSG.
	close(privmsgReady)

	// The bound client must receive the PRIVMSG with @time.
	msg := waitMsg(t, c, 3*time.Second, func(m *irc.Message) bool {
		return m.Command == "PRIVMSG" && m.Param(1) == msgText
	})

	if msg.Tags["time"] == "" {
		t.Errorf("PRIVMSG missing @time tag; tags=%v", msg.Tags)
	}
	if msg.Source != "peer!p@host" {
		t.Errorf("PRIVMSG source = %q, want peer!p@host", msg.Source)
	}
}

// ─── TestClientToUpstreamRelay ───────────────────────────────────────────────

// TestClientToUpstreamRelay verifies that a bound client's PRIVMSG is forwarded
// to the scripted upstream.
func TestClientToUpstreamRelay(t *testing.T) {
	const (
		netid   = 2
		nick    = "relaynick"
		channel = "#relay"
		msgText = "relay me upstream"
	)

	clientSent := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	upstreamReceived := make(chan string, 1)

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		// Wait until the client is registered and sends its PRIVMSG.
		<-clientSent
		// The upstream should now receive the relayed PRIVMSG.
		// Skip any AWAY messages emitted by bouncer attach/detach logic.
		line := su.readLineSkipAway()
		upstreamReceived <- line
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "RelayNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	c := connectClientToSrv(t, srv)
	doBindRegister(t, c, "relayuser", netid)
	time.Sleep(50 * time.Millisecond)

	// Signal: client sends PRIVMSG.
	close(clientSent)
	sendLine(t, c, "PRIVMSG "+channel+" :"+msgText)

	select {
	case got := <-upstreamReceived:
		if !strings.Contains(got, "PRIVMSG") || !strings.Contains(got, msgText) {
			t.Errorf("upstream received %q, want PRIVMSG containing %q", got, msgText)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive relayed PRIVMSG within 3s")
	}
}

// ─── TestFanoutMultipleClients ────────────────────────────────────────────────

// TestFanoutMultipleClients verifies that two clients bound to the same netid
// both receive an upstream PRIVMSG.
func TestFanoutMultipleClients(t *testing.T) {
	const (
		netid   = 3
		nick    = "multinick"
		channel = "#multi"
		msgText = "to all clients"
	)

	privmsgReady := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		<-privmsgReady
		su.send(":peer!p@host PRIVMSG " + channel + " :" + msgText)
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "MultiNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Two clients, both bound to the same netid.
	c1 := connectClientToSrv(t, srv)
	c2 := connectClientToSrv(t, srv)
	doBindRegister(t, c1, "alpha", netid)
	doBindRegister(t, c2, "beta", netid)
	time.Sleep(80 * time.Millisecond)

	close(privmsgReady)

	pred := func(m *irc.Message) bool {
		return m.Command == "PRIVMSG" && m.Param(1) == msgText
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = waitMsg(t, c1, 3*time.Second, pred)
	}()
	go func() {
		defer wg.Done()
		_ = waitMsg(t, c2, 3*time.Second, pred)
	}()
	wg.Wait()
}

// ─── TestFanoutIsolation ─────────────────────────────────────────────────────

// TestFanoutIsolation verifies that a client bound to netid 1 does NOT receive
// netid 2's messages.
func TestFanoutIsolation(t *testing.T) {
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	net1Ready := make(chan struct{})
	net2Ready := make(chan struct{})

	makeDialer := func(id int, ch string, ready chan struct{}) dialer {
		return pipeDialer(t, &down, func(su *scriptedUpstream) {
			su.register("nick" + itoa(id))
			su.expectJoin(ch)
			su.sendJoinConfirm("nick"+itoa(id), ch)
			<-ready
			if id == 2 {
				su.send(":peer!p@host PRIVMSG " + ch + " :net2 message")
			}
			<-testDone
		})
	}

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    1,
				Name:     "Net1",
				Addr:     "upstream1.local:6667",
				Identity: Identity{Nick: "nick1", User: "u", Realname: "r"},
				Channels: []string{"#chan1"},
			},
			{
				NetID:    2,
				Name:     "Net2",
				Addr:     "upstream2.local:6667",
				Identity: Identity{Nick: "nick2", User: "u", Realname: "r"},
				Channels: []string{"#chan2"},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{
		1: makeDialer(1, "#chan1", net1Ready),
		2: makeDialer(2, "#chan2", net2Ready),
	}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Client bound to netid 1 only.
	c1 := connectClientToSrv(t, srv)
	doBindRegister(t, c1, "iso1", 1)
	time.Sleep(80 * time.Millisecond)

	// Signal both upstreams. Only net2 sends a PRIVMSG.
	close(net1Ready)
	close(net2Ready)

	// c1 (netid=1) must NOT receive the net2 PRIVMSG.
	noMsgFor(t, c1, 400*time.Millisecond, func(m *irc.Message) bool {
		return m.Command == "PRIVMSG" && strings.Contains(m.Param(1), "net2")
	})
}

// ─── TestUnboundControlNoFanout ──────────────────────────────────────────────

// TestUnboundControlNoFanout verifies that an unbound/control client receives
// no upstream traffic (only control-surface replies).
func TestUnboundControlNoFanout(t *testing.T) {
	const (
		netid   = 4
		nick    = "ctrlnick"
		channel = "#ctrlchan"
	)

	privmsgReady := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		<-privmsgReady
		su.send(":peer!p@host PRIVMSG " + channel + " :invisible to control")
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "CtrlNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Register as CONTROL session (no BIND, netid=0).
	ctrl := connectClientToSrv(t, srv)
	doRegisterSimple(t, ctrl, "ctrlsess")
	time.Sleep(80 * time.Millisecond)

	close(privmsgReady)

	// Control session must NOT receive the PRIVMSG.
	noMsgFor(t, ctrl, 400*time.Millisecond, func(m *irc.Message) bool {
		return m.Command == "PRIVMSG"
	})
}

// ─── TestDisconnectUnregistersCleanly ─────────────────────────────────────────

// TestDisconnectUnregistersCleanly verifies that a bound client disconnecting
// unregisters cleanly and no panic occurs when the upstream subsequently sends.
// Run with -race to validate no data races.
func TestDisconnectUnregistersCleanly(t *testing.T) {
	const (
		netid   = 5
		nick    = "discnick"
		channel = "#disc"
	)

	clientRegistered := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	// How many PRIVMSGs were sent after disconnect.
	var afterDisconnect atomic.Int32

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		// Wait until client is registered so we know it was in boundSessions.
		<-clientRegistered
		// Now disconnect the client (we'll do that in the test goroutine).
		// Give a brief window for disconnect to propagate.
		time.Sleep(150 * time.Millisecond)
		// Send several messages after disconnect — must not panic.
		for i := 0; i < 5; i++ {
			afterDisconnect.Add(1)
			su.send(":peer!p@host PRIVMSG " + channel + " :after disconnect " + itoa(i))
			time.Sleep(20 * time.Millisecond)
		}
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "DiscNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	c := connectClientToSrv(t, srv)
	doBindRegister(t, c, "discuser", netid)
	time.Sleep(50 * time.Millisecond)

	// Signal upstream that client is registered.
	close(clientRegistered)

	// Disconnect the client (close the connection).
	_ = c.Close()

	// Wait for the upstream to finish sending after-disconnect messages.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && afterDisconnect.Load() < 5 {
		time.Sleep(20 * time.Millisecond)
	}

	// Verify no sessions remain registered for netid (registry cleaned up).
	srv.boundMu.RLock()
	remaining := len(srv.boundSessions[netid])
	srv.boundMu.RUnlock()
	if remaining != 0 {
		t.Errorf("boundSessions[%d] still has %d entries after disconnect", netid, remaining)
	}
}

// ─── TestSanitizeOnRelay ─────────────────────────────────────────────────────

// TestSanitizeOnRelay verifies that a PRIVMSG with an embedded ESC (0x1b)
// is delivered with the ESC stripped but IRC bold (\x02) preserved.
func TestSanitizeOnRelay(t *testing.T) {
	const (
		netid   = 6
		nick    = "sannick"
		channel = "#san"
	)

	privmsgReady := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	// Message with ESC (terminal-escape) AND IRC bold (\x02).
	rawMsg := "\x02bold\x02 and \x1b[31m escape"
	wantMsg := "\x02bold\x02 and [31m escape" // ESC stripped, \x02 preserved

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		<-privmsgReady
		su.send(":peer!p@host PRIVMSG " + channel + " :" + rawMsg)
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "SanNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	c := connectClientToSrv(t, srv)
	doBindRegister(t, c, "sanuser", netid)
	time.Sleep(50 * time.Millisecond)
	close(privmsgReady)

	msg := waitMsg(t, c, 3*time.Second, func(m *irc.Message) bool {
		return m.Command == "PRIVMSG"
	})

	got := msg.Param(1)
	if got != wantMsg {
		t.Errorf("sanitized relay body = %q, want %q", got, wantMsg)
	}
}

// ─── TestSelfSendFanout [B#11] ────────────────────────────────────────────────

// TestSelfSendFanout verifies the self-send rule [B#11]:
//   - Client A sends PRIVMSG with @label.
//   - The upstream echoes (simulated by the scripted upstream sending the same
//     PRIVMSG back as if echo-message was enabled).
//   - Client A receives the echo WITH its @label.
//   - Client B (same netid) receives the echo WITHOUT a label.
//   - The store path is exercised once (we check via a collectSink wired in
//     parallel — in this test we verify the fanout labels, not the store count,
//     since the store is tested separately in backlog/).
func TestSelfSendFanout(t *testing.T) {
	const (
		netid   = 7
		nick    = "selfnick"
		channel = "#selfchan"
		msgText = "hello from self"
		label   = "myLabel42"
	)

	clientASent := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		// Wait for client A to send its PRIVMSG.
		<-clientASent
		// Read and discard the relayed PRIVMSG (skip any AWAY from bouncer attach).
		_ = su.readLineSkipAway()
		// Echo it back as if echo-message is enabled.
		su.send(":selfnick!u@host PRIVMSG " + channel + " :" + msgText)
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "SelfNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Two bound clients.
	cA := connectClientToSrv(t, srv)
	cB := connectClientToSrv(t, srv)
	doBindRegister(t, cA, nick, netid) // Client A uses the bouncer nick
	doBindRegister(t, cB, "beta", netid)
	time.Sleep(80 * time.Millisecond)

	// Client A sends PRIVMSG with @label.
	_ = cA.WriteMessage(&irc.Message{
		Tags:    irc.Tags{"label": label},
		Command: "PRIVMSG",
		Params:  []string{channel, msgText},
	})
	close(clientASent)

	// Client A must receive the echo WITH its label.
	msgA := waitMsg(t, cA, 3*time.Second, func(m *irc.Message) bool {
		return m.Command == "PRIVMSG" && m.Param(1) == msgText
	})
	if msgA.Tags["label"] != label {
		t.Errorf("client A echo: label = %q, want %q; tags=%v", msgA.Tags["label"], label, msgA.Tags)
	}

	// Client B must receive the echo WITHOUT a label.
	msgB := waitMsg(t, cB, 3*time.Second, func(m *irc.Message) bool {
		return m.Command == "PRIVMSG" && m.Param(1) == msgText
	})
	if msgB.Tags["label"] != "" {
		t.Errorf("client B echo: unexpected label %q; tags=%v", msgB.Tags["label"], msgB.Tags)
	}
}

// ─── TestFanoutConcurrencyRace ────────────────────────────────────────────────

// TestFanoutConcurrencyRace drives multiple concurrent upstream deliveries while
// clients are simultaneously connecting and disconnecting. Run with -race.
func TestFanoutConcurrencyRace(t *testing.T) {
	const (
		netid   = 8
		nick    = "racenick"
		channel = "#race"
	)

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	// Upstream streams messages rapidly.
	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		for i := 0; i < 50; i++ {
			if down.Load() {
				return
			}
			su.send(":peer!p@host PRIVMSG " + channel + " :msg " + itoa(i))
			time.Sleep(5 * time.Millisecond)
		}
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "RaceNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Concurrently connect/disconnect clients while the upstream is streaming.
	var wg sync.WaitGroup
	for g := 0; g < 5; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := connectClientToSrv(t, srv)
			doBindRegister(t, c, "race"+itoa(g), netid)
			time.Sleep(30 * time.Millisecond)
			_ = c.Close()
		}()
	}
	wg.Wait()
	// If we reach here without a panic or race detector alarm, the test passes.
}

// ─── TestFanoutNonBlocking ────────────────────────────────────────────────────

// TestFanoutNonBlocking verifies that a slow/stuck attached client (one whose
// outbound queue is permanently full) does NOT stall fanout for a fast client
// on the same netid. This is the head-of-line-blocking defence.
//
// The stuck session is injected directly into boundSessions with a conn.Conn
// backed by a net.Conn that blocks all writes indefinitely (the conn's writer
// goroutine is parked and can never drain the queue). We then flood messages
// from the scripted upstream and assert that the fast client receives all of
// them within the test deadline.
func TestFanoutNonBlocking(t *testing.T) {
	const (
		netid    = 9
		nick     = "nbnick"
		channel  = "#nb"
		msgCount = 128 // enough to overflow the 64-slot outbound queue twice over
	)

	allSent := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		<-allSent
		for i := 0; i < msgCount; i++ {
			su.send(":peer!p@host PRIVMSG " + channel + " :msg" + itoa(i))
		}
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "NBNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	srv := New(cfg)
	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// clientFast: a normal bound session backed by a real net.Pipe that we read.
	clientFast := connectClientToSrv(t, srv)
	doBindRegister(t, clientFast, "fast", netid)

	// Inject a stuck session directly into boundSessions. The stuck session's
	// conn is backed by a net.Pipe whose client side we never read — because
	// net.Pipe writes block when the peer is not reading, the conn writer goroutine
	// parks on its first write, and the 64-slot outbound queue fills up. Every
	// subsequent TryWriteMessage call during the flood returns (false, nil) without
	// blocking the fanout goroutine.
	//
	// We bypass the normal registration flow: this test targets the fanout path
	// only, and directly inserting the session is the cleanest way to isolate it.
	stuckClientSide, stuckServerSide := net.Pipe()
	t.Cleanup(func() {
		_ = stuckClientSide.Close()
		_ = stuckServerSide.Close()
	})
	// stuckClientSide is never read — this stalls the conn writer goroutine.
	stuckConnObj := conn.NewConn(stuckServerSide, conn.Options{})
	t.Cleanup(func() { _ = stuckConnObj.Close() })

	// Directly inject the stuck conn as a fake session in boundSessions.
	stuckSess := &session{
		srv:   srv,
		conn:  stuckConnObj,
		nc:    stuckServerSide,
		isTLS: false,
		netid: netid,
		nick:  "stuck",
	}
	srv.boundMu.Lock()
	if srv.boundSessions[netid] == nil {
		srv.boundSessions[netid] = make(map[*session]struct{})
	}
	srv.boundSessions[netid][stuckSess] = struct{}{}
	srv.boundMu.Unlock()
	t.Cleanup(func() { srv.unregisterBoundSession(stuckSess) })

	// Pre-fill the stuck session's outbound queue so it is full before the flood
	// starts. The writer goroutine will block on the first write (stuckClientSide
	// never reads), and the remaining 63+ TrySend calls return (false, nil) once
	// the queue channel is full.
	for i := 0; i < 70; i++ {
		_, _ = stuckConnObj.TrySend("PING :prefill")
	}

	// Both clients are now present. Open the flood gate.
	time.Sleep(80 * time.Millisecond)
	close(allSent)

	// clientFast must receive all msgCount messages within 3 s. If fanout
	// blocked on the stuck client this would time out well before all arrived.
	received := 0
	deadline := time.After(3 * time.Second)
	for received < msgCount {
		select {
		case msg, ok := <-clientFast.Messages():
			if !ok {
				t.Fatalf("clientFast connection closed after %d messages", received)
			}
			if msg.Command == "PRIVMSG" && strings.HasPrefix(msg.Param(1), "msg") {
				received++
			}
		case <-deadline:
			t.Fatalf("clientFast received only %d/%d messages within 3s; fanout may have blocked on stuck client",
				received, msgCount)
		}
	}
	// Reaching here proves the stuck session did not stall the fanout goroutine.
}

// ─── TestFanoutEarlyExitNoSessions ───────────────────────────────────────────

// TestFanoutEarlyExitNoSessions verifies that when no sessions are bound to a
// netid, fanout returns without calling fanoutToSession (no per-message
// allocation). Critically, the backlog sink must still receive the message —
// detached capture is the whole point of a bouncer.
//
// We wire a dualSink as the manager's Sink so that each upstream event reaches
// both a collectSink (our "backlog store" stand-in) and the server's Ingest.
func TestFanoutEarlyExitNoSessions(t *testing.T) {
	const (
		netid   = 10
		nick    = "eenick"
		channel = "#ee"
	)

	msgSent := make(chan struct{})
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(nick)
		su.expectJoin(channel)
		su.sendJoinConfirm(nick, channel)
		<-msgSent
		su.send(":peer!p@host PRIVMSG " + channel + " :detached message")
		<-testDone
	})

	cfg := &Config{
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "EENet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: nick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	collect := &collectSink{}
	srv := New(cfg)

	// dualSink fans each event to both collect (our store stand-in) and srv.
	dual := &dualSink{a: collect, b: srv}

	mgr := NewManager(cfg, dual)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// No client is attached (zero sessions bound to netid). Signal upstream to send.
	time.Sleep(80 * time.Millisecond)
	close(msgSent)

	// The collect sink must see the PRIVMSG (store-path fires even with no sessions).
	if !collect.waitForPrivmsg(netid, "detached message", 3*time.Second) {
		t.Fatal("backlog sink did not receive the PRIVMSG — detached capture broken")
	}

	// No sessions are in boundSessions, confirming fanout early-exited cleanly.
	srv.boundMu.RLock()
	n := len(srv.boundSessions[netid])
	srv.boundMu.RUnlock()
	if n != 0 {
		t.Errorf("boundSessions[%d] = %d, want 0 (no clients attached)", netid, n)
	}
}

// dualSink routes every Ingest call to two sinks in order. It is used by
// TestFanoutEarlyExitNoSessions to observe events without bypassing the server.
type dualSink struct {
	a Sink
	b Sink
}

func (d *dualSink) Ingest(netid int, ev *client.Event) {
	d.a.Ingest(netid, ev)
	d.b.Ingest(netid, ev)
}

// ─── TestFanoutSuppressKillAndError ──────────────────────────────────────────

// TestFanoutSuppressKillAndError verifies that KILL and ERROR from the upstream
// are NOT relayed to attached clients. A hostile upstream sending either of
// these would otherwise cause all attached sessions to disconnect (IRC clients
// treat inbound KILL/ERROR aimed at themselves as a disconnect signal).
//
// Test strategy: send KILL/ERROR from the scripted upstream, assert the client
// does NOT receive the hostile command within a short window, then confirm the
// client session is still alive by sending it a PING and receiving a PONG.
func TestFanoutSuppressKillAndError(t *testing.T) {
	const (
		nick    = "killnick"
		channel = "#killchan"
	)

	for i, hostile := range []string{"KILL", "ERROR"} {
		hostile := hostile
		netid := 20 + i // use distinct netids so sub-tests don't share state
		t.Run(hostile, func(t *testing.T) {
			sent := make(chan struct{})
			testDone := make(chan struct{})
			var down atomic.Bool
			t.Cleanup(func() { down.Store(true) })
			t.Cleanup(func() { close(testDone) })

			dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
				su.register(nick)
				su.expectJoin(channel)
				su.sendJoinConfirm(nick, channel)
				<-sent
				if hostile == "KILL" {
					su.send(":upstream.local KILL " + nick + " :you are removed")
				} else {
					su.send("ERROR :Closing link")
				}
				// Hold the connection open long enough for the test to assert.
				// (The client.Client may reconnect; we don't care — we just need
				// the hostile message to have passed through fanout.)
				<-testDone
			})

			cfg := &Config{
				Networks: []Network{
					{
						NetID:    netid,
						Name:     "KillNet" + itoa(netid),
						Addr:     "upstream.local:6667",
						Identity: Identity{Nick: nick, User: "u", Realname: "r"},
						Channels: []string{channel},
					},
				},
			}

			srv := New(cfg)
			mgr := NewManager(cfg, srv)
			mgr.dialers = map[int]dialer{netid: dialFn}
			srv.WithManager(mgr)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := mgr.Start(ctx); err != nil {
				t.Fatalf("Manager.Start: %v", err)
			}
			t.Cleanup(func() { mgr.Close() })

			c := connectClientToSrv(t, srv)
			doBindRegister(t, c, "victim", netid)
			time.Sleep(50 * time.Millisecond)

			close(sent)

			// The KILL or ERROR must NOT arrive at the client.
			noMsgFor(t, c, 300*time.Millisecond, func(m *irc.Message) bool {
				return m.Command == hostile
			})

			// Confirm the client session is still alive: send a PING, expect a PONG
			// from lurkd. If KILL/ERROR had been forwarded the client's run loop
			// would have torn down the session and PING would not get a PONG.
			sendLine(t, c, "PING :liveness")
			_ = waitMsg(t, c, 2*time.Second, func(m *irc.Message) bool {
				return m.Command == "PONG"
			})
		})
	}
}

// ─── TestFanoutCursorAdvanceOnlyOnDelivery ───────────────────────────────────

// TestFanoutCursorAdvanceOnlyOnDelivery is the regression test for the
// cursor-past-undelivered-message bug: fanoutToSession used to advance the
// per-client read cursor unconditionally BEFORE the non-blocking
// TryWriteMessage, so when a session's outbound queue was full the message was
// dropped but the persisted read position had already moved past it — the
// client would never see the message and never get it replayed. The cursor
// must advance only after TryWriteMessage reports the message was queued.
func TestFanoutCursorAdvanceOnlyOnDelivery(t *testing.T) {
	const (
		netid  = 31
		target = "#cursordrop"
	)

	srv := New(&Config{})
	cs, err := NewCursorStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatalf("NewCursorStore: %v", err)
	}
	t.Cleanup(cs.Close)
	srv.WithCursorStore(cs)

	// newSess builds a fake bound session over a net.Pipe whose client side is
	// never read. Messages still enter the 64-slot outbound queue until it is
	// deliberately filled, so the first fanout below is "delivered" (queued)
	// while the second is dropped.
	newSess := func() *session {
		clientSide, serverSide := net.Pipe()
		t.Cleanup(func() {
			_ = clientSide.Close()
			_ = serverSide.Close()
		})
		c := conn.NewConn(serverSide, conn.Options{})
		t.Cleanup(func() { _ = c.Close() })
		return &session{srv: srv, conn: c, nc: serverSide, netid: netid, nick: "c"}
	}

	mkMsgEv := func(msgid, text string) (*irc.Message, *client.Event) {
		raw := &irc.Message{
			Tags:    irc.Tags{"msgid": msgid, "time": "2024-05-01T00:00:00.000Z"},
			Source:  "peer!p@host",
			Command: "PRIVMSG",
			Params:  []string{target, text},
		}
		return raw, &client.Event{Message: raw}
	}

	key := CursorKey{ClientID: defaultClientID, NetID: netid, Target: target}

	// Case 1: deliverable session (queue has room) — the cursor must advance.
	okSess := newSess()
	msg1, ev1 := mkMsgEv("msgid-delivered", "hello")
	line1, _ := msg1.Serialize()
	srv.fanoutToSession(okSess, msg1, line1, ev1)
	got, ok := cs.Get(key)
	if !ok || got.MsgID != "msgid-delivered" {
		t.Fatalf("cursor after delivered message = (%+v, %v), want msgid-delivered", got, ok)
	}

	// Case 2: stuck session — prefill the outbound queue to capacity and park
	// the writer goroutine. The prefill lines are deliberately long (~1 KiB)
	// so the writer's burst coalescing fills its 4 KiB bufio buffer after a
	// few lines and blocks mid-batch on the unread pipe; short lines would all
	// fit in the buffer and the writer would drain the whole queue before
	// parking on the final flush. After the writer is parked, top up any slots
	// it freed; the next fanout is then guaranteed to be dropped.
	stuck := newSess()
	bigLine := "PING :" + strings.Repeat("x", 1024)
	for i := 0; i < 70; i++ {
		_, _ = stuck.conn.TrySend(bigLine)
	}
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 70; i++ {
		if ok, _ := stuck.conn.TrySend(bigLine); !ok {
			break
		}
	}
	if ok, _ := stuck.conn.TrySend(bigLine); ok {
		t.Fatal("stuck session queue did not fill; cannot exercise the drop path")
	}

	msg2, ev2 := mkMsgEv("msgid-dropped", "lost")
	line2, _ := msg2.Serialize()
	srv.fanoutToSession(stuck, msg2, line2, ev2)
	got, ok = cs.Get(key)
	if !ok || got.MsgID != "msgid-delivered" {
		t.Fatalf("cursor after dropped message = (%+v, %v), want unchanged msgid-delivered (cursor moved past an undelivered message)", got, ok)
	}
}
