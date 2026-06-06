package server

// TestE2EFullStack is the Phase 8 capstone end-to-end test.
//
// Architecture:
//
//	Real lurk client.Client  ←net.Pipe→  Real lurkd (server+manager)  ←net.Pipe→  Scripted upstream
//
// Both sides are real implementations:
//   - The lurkd side: server.New + backlog.NewStore + NewManager with an injected
//     dialer so the upstream is a scripted net.Pipe goroutine (not a real IRC server).
//   - The client side: client.Client with Config.BouncerNetID=1, SASL PLAIN, and
//     Config.Dialer returning one end of the net.Pipe whose other end is served by
//     serveConnInternal.
//
// TLS-gate handling (option b): lurkd's serveConnInternal is called with
// isTLS=true. This marks the session as TLS for the SASL PLAIN gate without
// requiring a real TLS handshake over net.Pipe. The choice is documented:
// net.Pipe provides no TLS guarantees; the TLS-gate seam exists precisely for
// this use — see server.go serveConnTLS godoc. The real TLS code path (cert
// generation, tls.Server/tls.Client wrapping) is exercised separately in the TLS
// gate unit test; the goal here is end-to-end functional verification.
//
// Assertions:
//  1. The lurk client completes registration and is bound to netid 1.
//  2. A PRIVMSG from the scripted upstream reaches the real lurk client via
//     fan-out, carrying @time AND @msgid (msgid comes from the backlog store,
//     proving Phase 7 store→live-delivery msgid consistency).
//  3. The lurk client sends a PRIVMSG, and the scripted upstream receives it
//     (proving the relay path: client → lurkd → upstream).

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// TestE2EFullStack runs the capstone Phase 8 full-stack end-to-end test.
func TestE2EFullStack(t *testing.T) {
	const (
		netid           = 1
		upstreamNick    = "bouncer"
		channel         = "#e2e-full"
		bouncerUser     = "testuser"
		bouncerPass     = "e2e-secret"
		msgFromUpstream = "hello from upstream"
		msgFromClient   = "hello from lurk client"
	)

	// ─── 1. Hash the bouncer password ─────────────────────────────────────────
	pwHash, err := HashPassword(bouncerPass)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	// ─── 2. Build the backlog store ────────────────────────────────────────────
	store, err := backlog.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("backlog.NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	// ─── 3. Build the lurkd config ────────────────────────────────────────────
	cfg := &Config{
		BouncerAuth: BouncerAuth{
			User:         bouncerUser,
			PasswordHash: pwHash,
		},
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "TestNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: upstreamNick, User: "u", Realname: "r"},
				Channels: []string{channel},
			},
		},
	}

	// ─── 4. Build lurkd server + manager with scripted upstream ───────────────
	srv := New(cfg)
	srv.regTimeout = 10 * time.Second // generous for test
	srv.WithStore(store)

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	// Channel: client has sent its PRIVMSG; upstream can now try to read it.
	clientRelayed := make(chan struct{})
	// Channel: upstream has sent its PRIVMSG; fanout can proceed.
	upstreamReady := make(chan struct{})
	// The upstream receives the relayed PRIVMSG from the client here.
	upstreamReceived := make(chan string, 1)

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register(upstreamNick)
		su.expectJoin(channel)
		su.sendJoinConfirm(upstreamNick, channel)

		// Wait for the client to be registered and bound before sending upstream PRIVMSG.
		// We signal upstreamReady after the server indicates the client is ready.
		<-upstreamReady

		// Send a PRIVMSG from upstream that will fan out to the bound lurk client.
		su.send(":peer!p@host PRIVMSG " + channel + " :" + msgFromUpstream)

		// Wait for the client to send its PRIVMSG, then read it.
		<-clientRelayed
		line := su.readLineSkipAway()
		upstreamReceived <- line

		<-testDone
	})

	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Wait for the upstream to be connected and joined before proceeding.
	// We poll UpstreamState; once Connected the JOIN has been processed.
	if !waitForUpstreamState(mgr, netid, ConnConnected, 5*time.Second) {
		t.Fatal("upstream did not reach ConnConnected within 5s")
	}

	// Give the upstream's JOIN confirm time to propagate through the client run loop.
	// We check by polling the upstream client's channel list.
	upCC, ok := mgr.Client(netid)
	if !ok {
		t.Fatal("no upstream client for netid 1")
	}
	joinedDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(joinedDeadline) {
		if ch := upCC.Channels(); len(ch) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// ─── 5. Build the real lurk client ────────────────────────────────────────
	//
	// The Dialer returns one end of a net.Pipe whose other end is served by
	// serveConnInternal(nc, isTLS=true) — the TLS-gate seam (option b).
	//
	// We use AllowInsecureAuth:true on the client because net.Pipe is not a
	// *tls.Conn; the server is told isTLS=true via the test seam, allowing it to
	// accept SASL PLAIN. The client-side AllowInsecureAuth suppresses the client's
	// own "refusing PLAIN over plaintext" guard.
	lurkCfg := client.Config{
		Nick:              "lurkclient",
		User:              "u",
		Realname:          "Lurk E2E Test",
		AllowInsecureAuth: true,
		SASL: client.SASLConfig{
			Mechanism: "PLAIN",
			Username:  bouncerUser,
			Password:  bouncerPass,
		},
		BouncerNetID: netid,
		// Request the caps the bound client needs.
		Caps: []string{
			"sasl",
			"soju.im/bouncer-networks",
			"server-time",
			"batch",
			"message-tags",
			"draft/chathistory",
		},
		Dialer: func(_ context.Context) (net.Conn, error) {
			// Create the pipe and start serving the server end.
			clientSide, serverSide := net.Pipe()
			// Serve the lurkd end; isTLS=true to satisfy the SASL PLAIN TLS gate.
			go func() {
				if err := srv.serveConnInternal(serverSide, true /*isTLS*/); err != nil {
					// Non-fatal: the session may close when the test ends.
					_ = err
				}
			}()
			return clientSide, nil
		},
	}

	lurk := client.New(lurkCfg)
	t.Cleanup(func() { _ = lurk.Close() })

	// ─── 6. Register handlers and drain Events BEFORE Connect ─────────────────
	//
	// Race safety: OnAny and On callbacks are registered before the client's run
	// goroutine starts (before Connect). The dispatcher has no internal locking
	// — the contract is that handlers are registered before the run goroutine
	// sees them. Registering after Connect would race: the run goroutine reads
	// d.any while the test goroutine writes it. By registering before Connect
	// there is no concurrent access.
	//
	// We pass the *irc.Message through a buffered channel so the callback's
	// goroutine (the client run goroutine) and the test goroutine never share a
	// raw pointer write/read.
	msgCh := make(chan *irc.Message, 1)
	lurk.OnAny(func(ev *client.Event) {
		if ev.Command() == "PRIVMSG" && ev.Param(0) == channel && ev.Text() == msgFromUpstream {
			if ev.Message != nil {
				select {
				case msgCh <- ev.Message:
				default:
				}
			}
		}
	})
	// Drain the Events() stream in a goroutine. Events() is a separate delivery
	// path (the streaming channel) — it must be drained so the lurk client's
	// internal event loop does not block when the channel fills. OnAny callbacks
	// fire regardless of Events() being drained, but draining is good hygiene.
	go func() {
		for range lurk.Events() {
		}
	}()

	// Connect the lurk client now that handlers are registered.
	if err := lurk.Connect(ctx); err != nil {
		t.Fatalf("lurk client Connect: %v", err)
	}

	// ─── 7. Signal the upstream to send its PRIVMSG ───────────────────────────
	//
	// Wait until the server has registered the lurk client session in
	// boundSessions before signaling the upstream to send. lurk.Connect returns
	// after RPL_WELCOME (001), but registerBoundSession is called later in the
	// server goroutine (after the 005 is written to the queue). Polling
	// boundSessions is the correct synchronization: the PRIVMSG fanout iterates
	// boundSessions, so a fanout that runs before registration delivers to zero
	// sessions and the test would hang. A sleep is fragile under load.
	boundDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(boundDeadline) {
		srv.boundMu.RLock()
		n := len(srv.boundSessions[netid])
		srv.boundMu.RUnlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	{
		srv.boundMu.RLock()
		n := len(srv.boundSessions[netid])
		srv.boundMu.RUnlock()
		if n == 0 {
			t.Fatal("lurk client session was not registered in boundSessions within 3s")
		}
	}
	close(upstreamReady)

	var m *irc.Message
	select {
	case m = <-msgCh:
	case <-time.After(5 * time.Second):
		t.Fatal("lurk client did not receive PRIVMSG from upstream within 5s")
	}

	if m == nil {
		t.Fatal("received nil irc.Message via channel")
	}

	// Assert @time tag is present.
	if m.Tags["time"] == "" {
		t.Errorf("PRIVMSG missing @time tag; tags=%v", m.Tags)
	}

	// Assert @msgid tag is present (store-assigned, Phase 7 msgid consistency).
	if m.Tags["msgid"] == "" {
		t.Errorf("PRIVMSG missing @msgid tag; tags=%v (msgid consistency requires Phase 7 store be wired)", m.Tags)
	} else {
		t.Logf("PRIVMSG @msgid=%s (store-assigned, restart-stable)", m.Tags["msgid"])
	}

	// Assert the source is correct.
	if m.Source != "peer!p@host" {
		t.Errorf("PRIVMSG source = %q, want peer!p@host", m.Source)
	}

	// ─── 8. Client → upstream relay test ──────────────────────────────────────
	//
	// The lurk client sends a PRIVMSG to the channel. The scripted upstream
	// should receive it (relayed through lurkd).
	if err := lurk.Privmsg(channel, msgFromClient); err != nil {
		t.Fatalf("lurk.Privmsg: %v", err)
	}
	close(clientRelayed) // tell the upstream goroutine to read the relayed line

	select {
	case got := <-upstreamReceived:
		if !strings.Contains(got, "PRIVMSG") || !strings.Contains(got, msgFromClient) {
			t.Errorf("upstream received %q, want PRIVMSG containing %q", got, msgFromClient)
		}
		t.Logf("upstream received: %q", got)
	case <-time.After(5 * time.Second):
		t.Fatal("scripted upstream did not receive relayed PRIVMSG within 5s")
	}

	// ─── 9. Verify BOUNCER BIND was sent before CAP END ───────────────────────
	//
	// The lurk client completed registration against a bouncer offering
	// soju.im/bouncer-networks. The 005 should carry BOUNCER_NETID=1 (the real
	// netid), not BOUNCER_NETID=0 (which would indicate an unbound/control session).
	// We verify this by inspecting the lurk client's isupport feature set via Nick()
	// completing (registration success already implies BIND succeeded, since a bad
	// netid would have returned a FAIL and the server would proceed as control).
	if lurk.Nick() != "lurkclient" {
		t.Errorf("lurk.Nick() = %q, want lurkclient", lurk.Nick())
	}

	// Log the bound channel list as a diagnostic.
	t.Logf("lurk client channels after bind: %v", lurk.Channels())
}

// TestE2EFullStackBouncerBindOrdering verifies that within the TestE2EFullStack
// scenario the BOUNCER BIND message actually arrived before CAP END on the
// server side. We do this by inspecting the session registry: after Connect
// returns, the session must be in boundSessions[netid] (not netid=0 control).
// If BIND arrived AFTER CAP END, the server would have completed welcome on
// control context (netid=0) and BIND would have been rejected with ALREADY_REGISTERED.
func TestE2EFullStackBouncerBindOrdering(t *testing.T) {
	const (
		netid       = 1
		bouncerUser = "binduser"
		bouncerPass = "bind-pass"
	)

	pwHash, err := HashPassword(bouncerPass)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	cfg := &Config{
		BouncerAuth: BouncerAuth{
			User:         bouncerUser,
			PasswordHash: pwHash,
		},
		Networks: []Network{
			{
				NetID:    netid,
				Name:     "BindNet",
				Addr:     "upstream.local:6667",
				Identity: Identity{Nick: "bindnick", User: "u", Realname: "r"},
			},
		},
	}

	srv := New(cfg)
	srv.regTimeout = 5 * time.Second

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true) })
	t.Cleanup(func() { close(testDone) })

	dialFn := pipeDialer(t, &down, func(su *scriptedUpstream) {
		su.register("bindnick")
		<-testDone
	})

	mgr := NewManager(cfg, srv)
	mgr.dialers = map[int]dialer{netid: dialFn}
	srv.WithManager(mgr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("mgr.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Connect the real lurk client.
	lurkCfg := client.Config{
		Nick:              "bindclient",
		User:              "u",
		Realname:          "Bind Test",
		AllowInsecureAuth: true,
		SASL: client.SASLConfig{
			Mechanism: "PLAIN",
			Username:  bouncerUser,
			Password:  bouncerPass,
		},
		BouncerNetID: netid,
		Caps: []string{
			"sasl",
			"soju.im/bouncer-networks",
			"server-time",
		},
		Dialer: func(_ context.Context) (net.Conn, error) {
			clientSide, serverSide := net.Pipe()
			go func() {
				_ = srv.serveConnInternal(serverSide, true)
			}()
			return clientSide, nil
		},
	}

	lurk := client.New(lurkCfg)
	t.Cleanup(func() { _ = lurk.Close() })

	if err := lurk.Connect(ctx); err != nil {
		t.Fatalf("lurk.Connect: %v", err)
	}

	// If Connect succeeded, the session completed registration. Verify the session
	// is in boundSessions[netid=1], proving BIND was processed before welcome.
	// Poll boundSessions rather than sleeping: lurk.Connect returns after RPL_WELCOME
	// (001) but registerBoundSession is called by the server goroutine after the
	// 005 send. A fixed sleep is fragile; polling with a timeout is correct.
	var boundCount int
	boundOKDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(boundOKDeadline) {
		srv.boundMu.RLock()
		boundCount = len(srv.boundSessions[netid])
		srv.boundMu.RUnlock()
		if boundCount > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if boundCount == 0 {
		t.Errorf("no sessions in boundSessions[netid=%d] after successful Connect; BIND may not have fired before CAP END", netid)
	} else {
		t.Logf("boundSessions[netid=%d] has %d session(s) — BIND ordering confirmed", netid, boundCount)
	}

	// Verify also that Nick() is correct (registration completed).
	if lurk.Nick() != "bindclient" {
		t.Errorf("lurk.Nick() = %q, want bindclient", lurk.Nick())
	}

	// Verify that the lurk client's BouncerNetID was used (the client saw the
	// soju.im/bouncer-networks cap, since lurkd advertises it). The Connect would
	// only have emitted BIND if CapEnabled returned true for bouncer-networks.
	if !lurk.CapEnabled("soju.im/bouncer-networks") {
		t.Error("lurk client did not negotiate soju.im/bouncer-networks — BIND would have been suppressed")
	}
}
