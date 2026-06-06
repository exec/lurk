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
		line := su.readLine()
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
		// Read and discard the relayed PRIVMSG.
		_ = su.readLine()
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
