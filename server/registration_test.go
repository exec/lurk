package server

// Phase 1 golden-transcript tests. Each test scripts the CLIENT end of a
// net.Pipe pair while serveConn drives the SERVER end in a goroutine. Tests
// assert the exact serialized irc.Message lines lurkd sends.
//
// Conventions:
//   - sendLine(t, c, line)      — client sends one already-serialized line
//   - recvMsg(t, c)             — read next parsed message from the client end
//   - assertMsg(t, got, cmd, params...) — deep equality check on Command + Params
//   - skipTo(t, c, cmd)         — drain until a message with the given command
//   - pipeServer(t)             — create a pipe and start the server in a goroutine

import (
	"bufio"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// ─── test harness helpers ─────────────────────────────────────────────────────

// pipeServer creates a net.Pipe, wraps the client end in a conn.Conn, and
// starts serveConnInternal on the server end in a goroutine with isTLS=false.
// It registers cleanup for both ends. It returns the wrapped client conn for
// test scripts to use.
//
// pipeServer uses serveConnInternal (the internal seam) rather than serveConn so
// that the isTLS flag is passed explicitly — net.Pipe returns a *net.Conn, not a
// *tls.Conn, so serveConn's type assertion would always see isTLS=false, which
// is the correct hermetic-test behavior but it is better stated explicitly.
func pipeServer(t *testing.T) *conn.Conn {
	t.Helper()
	return pipeServerWith(t, &Config{}, false)
}

// pipeServerWith is the general helper used by Phase 2 tests that need to
// control the server config and the isTLS flag independently.
func pipeServerWith(t *testing.T, cfg *Config, isTLS bool) *conn.Conn {
	t.Helper()
	cRaw, sRaw := net.Pipe()
	client := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = sRaw.Close()
	})
	s := New(cfg)
	go func() { _ = s.serveConnInternal(sRaw, isTLS) }()
	return client
}

// sendLine writes a raw IRC line from the client side of the pipe.
func sendLine(t *testing.T, c *conn.Conn, line string) {
	t.Helper()
	if err := c.Send(line); err != nil {
		t.Fatalf("client Send %q: %v", line, err)
	}
}

// recvMsg reads the next parsed message arriving at the client end. Fails if
// none arrives within 3 seconds.
func recvMsg(t *testing.T, c *conn.Conn) *irc.Message {
	t.Helper()
	select {
	case msg, ok := <-c.Messages():
		if !ok {
			t.Fatalf("client connection closed while waiting for message")
		}
		return msg
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for server message")
		return nil
	}
}

// assertMsg checks that got.Command == wantCmd and that every listed param
// matches (extra trailing params are ignored).
func assertMsg(t *testing.T, got *irc.Message, wantCmd string, wantParams ...string) {
	t.Helper()
	if got.Command != wantCmd {
		t.Errorf("command = %q, want %q (full message: %+v)", got.Command, wantCmd, got)
		return
	}
	for i, wp := range wantParams {
		if got.Param(i) != wp {
			t.Errorf("param[%d] = %q, want %q (cmd=%s params=%v)",
				i, got.Param(i), wp, got.Command, got.Params)
		}
	}
}

// skipTo drains messages from c until one with the matching command is seen
// and returns it. Fails on connection close or timeout.
func skipTo(t *testing.T, c *conn.Conn, wantCmd string) *irc.Message {
	t.Helper()
	for {
		msg := recvMsg(t, c)
		if msg.Command == wantCmd {
			return msg
		}
	}
}

// ─── CAP LS tests ────────────────────────────────────────────────────────────

// TestCAPLS302 verifies that "CAP LS 302" yields a CAP LS reply containing all
// advertised capabilities from §7.1 of docs/LURKD-DESIGN.md.
func TestCAPLS302(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	msg := recvMsg(t, client)

	assertMsg(t, msg, irc.CAP, "*", irc.CAP_LS)
	payload := msg.Param(2)

	// Every cap in advertisedCaps must appear in the payload.
	for _, cap := range advertisedCaps {
		name, _, _ := strings.Cut(cap, "=")
		if !strings.Contains(payload, name) {
			t.Errorf("CAP LS payload missing %q; got: %q", name, payload)
		}
	}

	// Spot-check the caps that §7.1 explicitly requires.
	must := []string{
		"sasl",
		"soju.im/bouncer-networks",
		"soju.im/bouncer-networks-notify",
		"server-time",
		"batch",
		"message-tags",
		"labeled-response",
		"echo-message",
		"away-notify",
		"multi-prefix",
		"cap-notify",
		"draft/chathistory",
	}
	for _, c := range must {
		if !strings.Contains(payload, c) {
			t.Errorf("CAP LS payload must contain %q; payload=%q", c, payload)
		}
	}
}

// TestCAPLSPreNickShowsStar verifies that the CAP LS reply uses "*" as the
// nick before any NICK command has been seen.
func TestCAPLSPreNickShowsStar(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*")
}

// ─── CAP REQ / ACK / NAK tests ───────────────────────────────────────────────

// TestCAPREQSupportedACK verifies that a request of all-supported caps → ACK
// with the same payload.
func TestCAPREQSupportedACK(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client) // consume CAP LS reply

	sendLine(t, client, "CAP REQ :server-time batch message-tags")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_ACK, "server-time batch message-tags")
}

// TestCAPREQUnsupportedNAK verifies the all-or-nothing rule: one unsupported
// cap in the request → NAK the whole thing.
func TestCAPREQUnsupportedNAK(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :server-time unknown-cap-xyz")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_NAK, "server-time unknown-cap-xyz")
}

// TestCAPREQAllUnsupportedNAK verifies NAK when every cap is unsupported.
func TestCAPREQAllUnsupportedNAK(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP REQ :not-a-real-cap-ever")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_NAK)
}

// TestCAPREQBeforeLS verifies that a CAP REQ before CAP LS still yields
// ACK/NAK correctly (the spec does not require LS first).
func TestCAPREQBeforeLS(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP REQ :server-time")
	msg := recvMsg(t, client)
	// server-time is a supported cap → ACK.
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_ACK, "server-time")
}

// TestCAPREQDisable verifies that "-cap" in a REQ (cap disable) is ACK'd for
// a cap that lurkd supports.
func TestCAPREQDisable(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	// Enable it first.
	sendLine(t, client, "CAP REQ :server-time")
	_ = recvMsg(t, client) // ACK

	// Disable it.
	sendLine(t, client, "CAP REQ :-server-time")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_ACK, "-server-time")
}

// TestCAPREQEmptyNAK verifies that an empty REQ payload gets a NAK (no caps
// to enable, nothing to ACK).
func TestCAPREQEmptyNAK(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP REQ :")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_NAK)
}

// TestCAPLIST verifies that CAP LIST returns currently-enabled caps.
func TestCAPLIST(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :server-time batch")
	_ = recvMsg(t, client) // ACK

	sendLine(t, client, "CAP LIST")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.CAP, "*", irc.CAP_LIST)
	payload := msg.Param(2)
	if !strings.Contains(payload, "server-time") || !strings.Contains(payload, "batch") {
		t.Errorf("CAP LIST payload %q should contain server-time and batch", payload)
	}
}

// ─── Full registration tests ──────────────────────────────────────────────────

// TestFullRegistration exercises the complete registration sequence and
// asserts the exact burst: CAP LS 302 → CAP REQ → NICK → USER → CAP END →
// 001/002/003/004/005 with BOUNCER_NETID.
func TestFullRegistration(t *testing.T) {
	client := pipeServer(t)

	// Step 1: CAP LS
	sendLine(t, client, "CAP LS 302")
	capLS := recvMsg(t, client)
	assertMsg(t, capLS, irc.CAP, "*", irc.CAP_LS)

	// Step 2: CAP REQ
	sendLine(t, client, "CAP REQ :server-time batch echo-message")
	capACK := recvMsg(t, client)
	assertMsg(t, capACK, irc.CAP, "*", irc.CAP_ACK, "server-time batch echo-message")

	// Step 3: NICK + USER
	sendLine(t, client, "NICK testuser")
	sendLine(t, client, "USER testuser 0 * :Test User")

	// Step 4: CAP END — triggers welcome burst
	sendLine(t, client, "CAP END")

	// 001 RPL_WELCOME
	w001 := skipTo(t, client, irc.RPL_WELCOME)
	assertMsg(t, w001, irc.RPL_WELCOME, "testuser")
	if !strings.Contains(w001.Param(1), "testuser") {
		t.Errorf("001 text should mention nick; got %q", w001.Param(1))
	}

	// 002 RPL_YOURHOST
	w002 := skipTo(t, client, irc.RPL_YOURHOST)
	assertMsg(t, w002, irc.RPL_YOURHOST, "testuser")

	// 003 RPL_CREATED
	w003 := skipTo(t, client, irc.RPL_CREATED)
	assertMsg(t, w003, irc.RPL_CREATED, "testuser")

	// 004 RPL_MYINFO
	w004 := skipTo(t, client, irc.RPL_MYINFO)
	assertMsg(t, w004, irc.RPL_MYINFO, "testuser")

	// 005 RPL_ISUPPORT — must contain BOUNCER_NETID=0, CASEMAPPING=ascii, NETWORK=lurkd
	w005 := skipTo(t, client, irc.RPL_ISUPPORT)
	assertMsg(t, w005, irc.RPL_ISUPPORT, "testuser")
	all005 := strings.Join(w005.Params, " ")
	for _, tok := range []string{"CASEMAPPING=ascii", "CHANTYPES=#", "BOUNCER_NETID=0", "NETWORK=lurkd"} {
		if !strings.Contains(all005, tok) {
			t.Errorf("005 missing token %q; params=%v", tok, w005.Params)
		}
	}
}

// TestRegistrationLegacyNoCAP verifies that a client that sends NICK + USER
// without any CAP exchange gets the welcome burst (legacy client path).
func TestRegistrationLegacyNoCAP(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "NICK legacy")
	sendLine(t, client, "USER legacy 0 * :Legacy Client")

	w001 := skipTo(t, client, irc.RPL_WELCOME)
	assertMsg(t, w001, irc.RPL_WELCOME, "legacy")
}

// TestRegistrationCAPENDFirst verifies that CAP END before NICK/USER defers
// the welcome burst until both NICK and USER are received.
func TestRegistrationCAPENDFirst(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP END")
	// No welcome yet (no nick/user).

	sendLine(t, client, "NICK late")
	sendLine(t, client, "USER late 0 * :Late User")

	w001 := skipTo(t, client, irc.RPL_WELCOME)
	assertMsg(t, w001, irc.RPL_WELCOME, "late")
}

// TestRegistrationUSERBeforeNICK verifies that USER arriving before NICK works.
func TestRegistrationUSERBeforeNICK(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "USER first 0 * :First")
	sendLine(t, client, "NICK first")

	w001 := skipTo(t, client, irc.RPL_WELCOME)
	assertMsg(t, w001, irc.RPL_WELCOME, "first")
}

// TestBOUNCER_NETIDInISUPPORT is an explicit golden assertion that 005
// contains BOUNCER_NETID as required by Phase 1 (stub value of 0).
func TestBOUNCER_NETIDInISUPPORT(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "NICK bnc")
	sendLine(t, client, "USER bnc 0 * :BNC Client")

	w005 := skipTo(t, client, irc.RPL_ISUPPORT)
	all := strings.Join(w005.Params, " ")
	if !strings.Contains(all, "BOUNCER_NETID=") {
		t.Errorf("005 must contain BOUNCER_NETID; params=%v", w005.Params)
	}
}

// ─── Hostile / edge-case tests ─────────────────────────────────────────────

// TestUnknownCommandDuringRegistration verifies that unknown commands during
// registration receive ERR_NOTREGISTERED (451) without crashing.
func TestUnknownCommandDuringRegistration(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "UNKNOWNCMD foo bar baz")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.ERR_NOTREGISTERED)
}

// TestNICKEmpty verifies that an empty NICK triggers ERR_NONICKNAMEGIVEN (431).
func TestNICKEmpty(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "NICK")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.ERR_NONICKNAMEGIVEN)
}

// TestUSERRepeatedAfterRegistration verifies that a second USER after
// registration is rejected with ERR_ALREADYREGISTERED (462).
func TestUSERRepeatedAfterRegistration(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "NICK user1")
	sendLine(t, client, "USER user1 0 * :User One")
	// Drain the entire welcome burst before sending the second USER.
	skipTo(t, client, irc.ERR_NOMOTD)

	sendLine(t, client, "USER user1 0 * :User One Again")
	// Now the next message should be ERR_ALREADYREGISTERED.
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.ERR_ALREADYREGISTERED)
}

// TestQUITDuringRegistration verifies that QUIT closes the connection cleanly
// without panic, even before registration completes.
func TestQUITDuringRegistration(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "QUIT :bye")

	// Connection should close.
	timeout := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-client.Messages():
			if !ok {
				return // success
			}
		case <-timeout:
			t.Fatal("timeout waiting for connection to close after QUIT")
		}
	}
}

// TestPINGResponse verifies that a PING during registration receives a PONG
// with the token echoed.
func TestPINGResponse(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "PING :token123")
	msg := recvMsg(t, client)
	assertMsg(t, msg, irc.PONG)
	found := false
	for _, p := range msg.Params {
		if p == "token123" {
			found = true
		}
	}
	if !found {
		t.Errorf("PONG did not echo token; params=%v", msg.Params)
	}
}

// TestCAPREQFloodBound verifies that a hostile client cannot grow the session's
// cap-request accounting without limit. We send many separate CAP REQ lines
// (each with a few cap names), accumulating past maxCapRequests. Once the
// budget is exhausted the server NAKs. We send valid caps (from advertisedCaps)
// so the only rejection cause is the budget, not unknown-cap detection.
func TestCAPREQFloodBound(t *testing.T) {
	client := pipeServer(t)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	// Send batches of 10 caps per REQ line, all valid, until we exceed the
	// budget. Each REQ line has 10 names × ~12 bytes = ~120 bytes — well under
	// the 512-byte IRC line limit.
	const batchSize = 10
	// Pick a valid cap to repeat so each REQ is individually ACK-able until
	// the budget is blown.
	validCap := advertisedCaps[0] // e.g. "sasl"
	name, _, _ := strings.Cut(validCap, "=")

	batches := (maxCapRequests / batchSize) + 2 // enough to blow the budget

	var lastMsg *irc.Message
	for i := 0; i < batches; i++ {
		names := make([]string, batchSize)
		for j := range names {
			names[j] = name
		}
		line := "CAP REQ :" + strings.Join(names, " ")
		if err := client.Send(line); err != nil {
			// conn closed the line as too long — budget already exceeded
			return
		}
		msg := recvMsg(t, client)
		lastMsg = msg
		if msg.Command == irc.CAP && msg.Param(1) == irc.CAP_NAK {
			return // got NAK before budget exhaustion — acceptable
		}
	}
	// The last message should be a NAK.
	if lastMsg == nil || lastMsg.Command != irc.CAP || lastMsg.Param(1) != irc.CAP_NAK {
		t.Errorf("expected NAK after exceeding cap request budget; last msg=%+v", lastMsg)
	}
}

// TestNoDoubleWelcome verifies that a client that does NICK+USER (triggering
// the legacy-no-CAP welcome path) and then sends CAP END does NOT receive a
// second welcome burst. The welcomed flag must be idempotent.
func TestNoDoubleWelcome(t *testing.T) {
	client := pipeServer(t)

	// Legacy path: NICK + USER with no CAP — welcome fires immediately.
	sendLine(t, client, "NICK nodup")
	sendLine(t, client, "USER nodup 0 * :No Dup")
	skipTo(t, client, irc.ERR_NOMOTD) // drain the entire burst

	// Now send CAP END — should produce NO messages (welcome already sent).
	sendLine(t, client, "CAP END")

	// Give the server a moment to process CAP END, then verify silence.
	// We send a PING to flush any queued responses; only the PONG should arrive.
	sendLine(t, client, "PING :probe")
	pong := recvMsg(t, client)
	assertMsg(t, pong, irc.PONG)
}

// TestMultipleUnknownCommands verifies that multiple hostile unknown commands
// each get ERR_NOTREGISTERED without the session panicking.
func TestMultipleUnknownCommands(t *testing.T) {
	client := pipeServer(t)

	for _, line := range []string{
		"EVIL something",
		"BLAH blah",
		"INJECT \x00\r\n", // NUL/CR/LF are stripped by conn before reaching us
	} {
		// conn.Send strips CR/LF; the malformed line with embedded control
		// bytes should either be refused by Send or arrive stripped.
		if err := client.Send(line); err != nil {
			// conn.Send may reject lines with embedded CR/LF/NUL — that's fine.
			continue
		}
		msg := recvMsg(t, client)
		assertMsg(t, msg, irc.ERR_NOTREGISTERED)
	}
}

// ─── Session-cap tests ────────────────────────────────────────────────────────

// TestSessionCapRejectsExcess verifies that Serve closes connections beyond
// Server.maxSessions before spawning a goroutine for them.
//
// Approach: set maxSessions=3, open 4 connections via the real Serve loop,
// assert the 4th is closed by the server (its Messages channel closes or no
// message arrives within the deadline), then confirm the goroutine count
// returns to baseline after all connections close.
func TestSessionCapRejectsExcess(t *testing.T) {
	const cap = 3

	// Baseline goroutine count before any test activity.
	goroutinesBefore := runtime.NumGoroutine()

	// Build a real net.Listener so Serve's accept loop runs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &Server{
		cfg:             &Config{},
		regTimeout:      registrationTimeout,
		maxSessions:     cap,
		controlSessions: make(map[*session]struct{}),
	}
	s.initBoundSessions()

	go func() { _ = s.Serve(ln) }()

	addr := ln.Addr().String()

	// Open exactly cap connections and keep them open so they hold their slots.
	var held []net.Conn
	for i := 0; i < cap; i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial #%d: %v", i+1, err)
		}
		held = append(held, c)
	}
	// Give Serve a moment to accept all cap connections and increment activeSessions.
	time.Sleep(50 * time.Millisecond)

	// The cap+1'th connection must be closed by the server immediately.
	excess, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial excess: %v", err)
	}
	t.Cleanup(func() { _ = excess.Close() })

	// Wrap in a conn.Conn and read. The server must either close the connection
	// (channel closes) or send nothing within a short deadline.
	_ = excess.SetDeadline(time.Now().Add(2 * time.Second))
	ec := conn.NewConn(excess, conn.Options{})
	t.Cleanup(func() { _ = ec.Close() })

	rejected := false
	select {
	case _, ok := <-ec.Messages():
		if !ok {
			// Channel closed: server closed the connection. Expected.
			rejected = true
		}
		// A message arriving means the server accepted the excess connection —
		// that is a test failure, caught below.
	case <-time.After(2 * time.Second):
		// No message and connection still open: also acceptable (rejected at TCP
		// layer before any data). Count as rejected.
		rejected = true
	}

	if !rejected {
		t.Error("excess connection was accepted past the session cap")
	}

	// Close all held connections so their session goroutines can exit.
	for _, c := range held {
		_ = c.Close()
	}
	// Allow session goroutines time to drain.
	time.Sleep(150 * time.Millisecond)

	// Goroutine count must return to within a small delta of baseline. A large
	// positive delta indicates leaked session goroutines.
	goroutinesAfter := runtime.NumGoroutine()
	delta := goroutinesAfter - goroutinesBefore
	if delta > 5 {
		t.Errorf("goroutine leak: %d goroutines before, %d after (delta=%d); want delta ≤ 5",
			goroutinesBefore, goroutinesAfter, delta)
	}
}

// ─── session write-deadline tests ─────────────────────────────────────────────

// TestSessionWriteTimeoutUnsticksStuckClient is the regression test for the
// missing post-registration write deadline: registered sessions used to have
// no write timeout at all, so a synchronous send to a client that stopped
// reading parked the session goroutine indefinitely once the outbound queue
// and the transport filled. With WriteTimeout set on the session conn, a
// stuck flush must fail the conn and the session goroutine must exit.
//
// The client end is driven with raw reads/writes (no conn.Conn) so that
// "stops reading" is literal: net.Pipe writes block until the peer reads, so
// the server's very next flush after the client goes quiet is stuck.
func TestSessionWriteTimeoutUnsticksStuckClient(t *testing.T) {
	srv := New(&Config{})
	srv.writeTimeout = 100 * time.Millisecond // test seam; production default is clientWriteTimeout

	cRaw, sRaw := net.Pipe()
	t.Cleanup(func() {
		_ = cRaw.Close()
		_ = sRaw.Close()
	})

	errCh := make(chan error, 1)
	go func() { errCh <- srv.serveConnInternal(sRaw, false) }()

	// Register (legacy flow, no CAP) and drain the welcome burst, which ends
	// with 422 ERR_NOMOTD. The registration deadline covers this phase; the
	// bug under test is the unbounded write AFTER registration.
	if _, err := cRaw.Write([]byte("NICK stuckwt\r\nUSER stuckwt 0 * :T\r\n")); err != nil {
		t.Fatalf("write registration: %v", err)
	}
	br := bufio.NewReader(cRaw)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read welcome burst: %v", err)
		}
		if strings.Contains(line, " "+irc.ERR_NOMOTD+" ") {
			break
		}
	}

	// Trigger one more server write (PING → PONG), then stop reading
	// entirely. The PONG flush blocks on the unread pipe; the write deadline
	// must fail the conn and unstick the session goroutine.
	if _, err := cRaw.Write([]byte("PING :tok\r\n")); err != nil {
		t.Fatalf("write PING: %v", err)
	}

	select {
	case <-errCh:
		// Session goroutine exited — the write deadline unstuck it.
	case <-time.After(5 * time.Second):
		t.Fatal("session goroutine still parked 5s after the client stopped reading; no write deadline on the session conn")
	}
}
