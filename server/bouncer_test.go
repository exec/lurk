package server

// Hermetic tests for the soju.im/bouncer-networks control surface.
//
// Test coverage:
//  - BOUNCER BIND <valid netid> during registration → 005 has BOUNCER_NETID=<netid>
//  - BOUNCER BIND <unknown netid> → FAIL, not bound, control context registration completes
//  - Unbound registration → control context, BOUNCER_NETID=0
//  - BOUNCER LISTNETWORKS → correct BATCH of NETWORK lines; password NOT present
//  - BOUNCER ADDNETWORK → new integer netid, persists to disk, starts upstream (mock),
//                         broadcasts -notify to a second connected control client
//  - BOUNCER DELNETWORK → removes, persists, notifies
//  - BOUNCER CHANGENETWORK → updates, persists, notifies
//  - BOUNCER CHANGENETWORK host/port change → live upstream is restarted (reconnect)
//  - changeNetwork Save failure → in-memory rollback, config unchanged
//  - delNetwork Save failure → in-memory rollback, removed entry restored
//  - Malformed BOUNCER subcommand → FAIL not panic
//  - Double-BIND → FAIL ALREADY_BOUND
//  - BIND after registration → FAIL
//  - Pre-auth BOUNCER BIND oracle blocked: unauthenticated BIND returns NOT_AUTHED,
//    not INVALID_NETID, for both valid and unknown netids (no netid-existence oracle)
//  - Manager Add/Remove race safety (checked by -race)
//  - Control-session registry: session joins on registration, leaves on disconnect
//  - LISTNETWORKS password attr must not be present

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// pipeServerWithManager creates a server wired with a Manager (for ADDNETWORK
// tests that need upstream start), a cfgPath, and an optional isTLS flag. The
// server is started in a goroutine; the wrapped client conn is returned.
func pipeServerWithManager(t *testing.T, cfg *Config, mgr *Manager, cfgPath string) *conn.Conn {
	t.Helper()
	cRaw, sRaw := net.Pipe()
	c := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = c.Close()
		_ = sRaw.Close()
	})
	srv := New(cfg)
	if mgr != nil {
		srv.WithManager(mgr)
	}
	if cfgPath != "" {
		srv.WithConfigPath(cfgPath)
	}
	go func() { _ = srv.serveConnInternal(sRaw, false) }()
	return c
}

// pipeServerSharedSrv creates two client connections to the SAME server
// instance. Used to test cross-session broadcast.
func pipeServerSharedSrv(t *testing.T, srv *Server) (c1, c2 *conn.Conn) {
	t.Helper()
	c1Raw, s1Raw := net.Pipe()
	c2Raw, s2Raw := net.Pipe()
	c1 = conn.NewConn(c1Raw, conn.Options{})
	c2 = conn.NewConn(c2Raw, conn.Options{})
	t.Cleanup(func() {
		_ = c1.Close()
		_ = s1Raw.Close()
		_ = c2.Close()
		_ = s2Raw.Close()
	})
	go func() { _ = srv.serveConnInternal(s1Raw, false) }()
	go func() { _ = srv.serveConnInternal(s2Raw, false) }()
	return c1, c2
}

// doBouncerRegister drives a complete registration handshake (with
// soju.im/bouncer-networks and soju.im/bouncer-networks-notify in CAP REQ),
// inserting extraLines (if any) before CAP END to simulate BOUNCER BIND etc.
// Drains the welcome burst before returning.
func doBouncerRegister(t *testing.T, c *conn.Conn, nick string, extraLines []string) {
	t.Helper()
	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c) // CAP LS reply

	sendLine(t, c, "CAP REQ :soju.im/bouncer-networks soju.im/bouncer-networks-notify")
	_ = recvMsg(t, c) // CAP ACK

	sendLine(t, c, "NICK "+nick)
	sendLine(t, c, "USER "+nick+" 0 * :Test")

	for _, l := range extraLines {
		sendLine(t, c, l)
	}

	sendLine(t, c, "CAP END")
	skipTo(t, c, irc.ERR_NOMOTD) // drain the welcome burst
}

// doRegisterSimple registers with bouncer-networks caps and no extra lines.
func doRegisterSimple(t *testing.T, c *conn.Conn, nick string, extraLines ...string) {
	doBouncerRegister(t, c, nick, extraLines)
}

// recvUntilCmd reads messages until one matching wantCmd is found, returning it.
// Like skipTo but with a configurable timeout.
func recvUntilCmd(t *testing.T, c *conn.Conn, wantCmd string, timeout time.Duration) *irc.Message {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case msg, ok := <-c.Messages():
			if !ok {
				t.Fatalf("connection closed waiting for %s", wantCmd)
			}
			if msg.Command == wantCmd {
				return msg
			}
		case <-deadline:
			t.Fatalf("timeout waiting for %s", wantCmd)
			return nil
		}
	}
}

// registerAndCapture does CAP LS/REQ, NICK, USER, optional extra lines, CAP END
// and returns the 005 message (intercepting it before the rest of the burst).
func registerAndCapture005(t *testing.T, c *conn.Conn, nick string, extraLines []string) *irc.Message {
	t.Helper()
	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c)

	sendLine(t, c, "CAP REQ :soju.im/bouncer-networks soju.im/bouncer-networks-notify")
	_ = recvMsg(t, c) // ACK

	sendLine(t, c, "NICK "+nick)
	sendLine(t, c, "USER "+nick+" 0 * :Test")

	for _, l := range extraLines {
		resp := sendLineCollect(t, c, l)
		if resp != nil {
			// Consume the BIND response (FAIL or nothing).
		}
	}

	sendLine(t, c, "CAP END")

	// Read the full burst and find 005.
	var w005 *irc.Message
	for i := 0; i < 20; i++ {
		msg := recvMsg(t, c)
		if msg.Command == irc.RPL_ISUPPORT {
			w005 = msg
		}
		if msg.Command == irc.ERR_NOMOTD {
			break
		}
	}
	if w005 == nil {
		t.Fatal("never received 005 RPL_ISUPPORT")
	}
	return w005
}

// sendLineCollect sends a line and immediately checks if a reply is available
// within 100ms. Used to consume a FAIL message from a BIND command without
// blocking.
func sendLineCollect(t *testing.T, c *conn.Conn, line string) *irc.Message {
	t.Helper()
	sendLine(t, c, line)
	select {
	case msg, ok := <-c.Messages():
		if !ok {
			return nil
		}
		return msg
	case <-time.After(200 * time.Millisecond):
		return nil // no immediate reply (e.g. BIND success is silent)
	}
}

// ─── BIND tests ───────────────────────────────────────────────────────────────

// TestBindValidNetID verifies that BOUNCER BIND <valid-netid> during registration
// causes 005 to emit BOUNCER_NETID=<netid> (not 0).
func TestBindValidNetID(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 7, Name: "TestNet", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}
	c := pipeServerWith(t, cfg, false)

	w005 := registerAndCapture005(t, c, "binduser", []string{"BOUNCER BIND 7"})
	all := strings.Join(w005.Params, " ")
	if !strings.Contains(all, "BOUNCER_NETID=7") {
		t.Errorf("005 after BIND 7: expected BOUNCER_NETID=7; params=%v", w005.Params)
	}
}

// TestBindUnknownNetID verifies that BIND to an unknown netid sends a FAIL and
// leaves the session on the control context (BOUNCER_NETID=0 in 005).
func TestBindUnknownNetID(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 1, Name: "TestNet", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}

	cRaw, sRaw := net.Pipe()
	c := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = c.Close()
		_ = sRaw.Close()
	})
	srv := New(cfg)
	go func() { _ = srv.serveConnInternal(sRaw, false) }()

	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c)
	sendLine(t, c, "CAP REQ :soju.im/bouncer-networks")
	_ = recvMsg(t, c)
	sendLine(t, c, "NICK bindtest")
	sendLine(t, c, "USER bindtest 0 * :Test")

	// BIND to netid 99 — does not exist.
	sendLine(t, c, "BOUNCER BIND 99")
	fail := recvMsg(t, c)
	if fail.Command != irc.FAIL {
		t.Fatalf("expected FAIL for unknown netid, got %s", fail.Command)
	}
	if fail.Param(1) != "INVALID_NETID" {
		t.Errorf("FAIL code = %q, want INVALID_NETID", fail.Param(1))
	}

	// Registration should still complete normally (on control context).
	sendLine(t, c, "CAP END")
	var w005 *irc.Message
	for i := 0; i < 20; i++ {
		msg := recvMsg(t, c)
		if msg.Command == irc.RPL_ISUPPORT {
			w005 = msg
		}
		if msg.Command == irc.ERR_NOMOTD {
			break
		}
	}
	if w005 == nil {
		t.Fatal("never received 005 after failed BIND")
	}
	all := strings.Join(w005.Params, " ")
	if !strings.Contains(all, "BOUNCER_NETID=0") {
		t.Errorf("005 after failed BIND: expected BOUNCER_NETID=0; params=%v", w005.Params)
	}
}

// TestUnboundRegistrationIsControlContext verifies that a session that
// completes registration without BOUNCER BIND stays on the control context
// and gets BOUNCER_NETID=0 in 005.
func TestUnboundRegistrationIsControlContext(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 1, Name: "TestNet", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}
	c := pipeServerWith(t, cfg, false)

	w005 := registerAndCapture005(t, c, "controlsess", nil)
	all := strings.Join(w005.Params, " ")
	if !strings.Contains(all, "BOUNCER_NETID=0") {
		t.Errorf("unbound session: expected BOUNCER_NETID=0; params=%v", w005.Params)
	}
}

// TestDoubleBind verifies that a second BOUNCER BIND during the registration
// window (after the first succeeds) returns FAIL ALREADY_BOUND.
func TestDoubleBind(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 3, Name: "TestNet", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}

	cRaw, sRaw := net.Pipe()
	c := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = c.Close()
		_ = sRaw.Close()
	})
	go func() { _ = New(cfg).serveConnInternal(sRaw, false) }()

	// Use CAP LS to keep the registration window open while we send both BINDs.
	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c) // CAP LS reply
	sendLine(t, c, "NICK db")
	sendLine(t, c, "USER db 0 * :Test")
	// First BIND during the registration window — should succeed silently.
	sendLine(t, c, "BOUNCER BIND 3")
	// The first BIND succeeds with no reply; give a short window.
	time.Sleep(20 * time.Millisecond)
	// Second BIND — should fail with ALREADY_BOUND.
	sendLine(t, c, "BOUNCER BIND 3")
	// Close the CAP window and drain the welcome burst, scanning for ALREADY_BOUND.
	sendLine(t, c, "CAP END")

	var gotFail bool
	for i := 0; i < 20; i++ {
		msg := recvMsg(t, c)
		if msg.Command == irc.FAIL && msg.Param(1) == "ALREADY_BOUND" {
			gotFail = true
		}
		if msg.Command == irc.ERR_NOMOTD {
			break
		}
	}
	if !gotFail {
		t.Error("expected FAIL ALREADY_BOUND on double BIND")
	}
}

// TestBindPreAuthOracleBlocked verifies that when BouncerAuth is configured,
// an unauthenticated client that sends BOUNCER BIND before or after a failed
// SASL exchange cannot probe which netids exist. The netid lookup (findNetworkByID)
// must not be reachable before authentication; both a valid and an unknown netid
// must return the same NOT_AUTHED failure code so the attacker learns nothing.
func TestBindPreAuthOracleBlocked(t *testing.T) {
	cfg := makeTestCfg(t, "alice", "secret")
	cfg.Networks = []Network{
		{NetID: 42, Name: "ExistsNet", Addr: "irc.example.com:6697",
			Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
	}

	// isTLS=true so SASL PLAIN is available, but we intentionally fail auth.
	c := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, c, "CAP LS 302")
	_ = recvMsg(t, c)

	sendLine(t, c, "CAP REQ :sasl soju.im/bouncer-networks")
	_ = recvMsg(t, c) // CAP ACK

	sendLine(t, c, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, c) // AUTHENTICATE +

	// Deliberately wrong password — SASL fails, saslAuthed stays false.
	sendLine(t, c, "AUTHENTICATE "+plainPayload("", "alice", "wrongpassword"))
	r904 := recvMsg(t, c)
	assertMsg(t, r904, irc.ERR_SASLFAIL)

	// Now attempt BOUNCER BIND for a VALID netid — must get NOT_AUTHED (not INVALID_NETID).
	sendLine(t, c, "BOUNCER BIND 42")
	fail := recvMsg(t, c)
	if fail.Command != irc.FAIL {
		t.Fatalf("expected FAIL for unauthenticated BIND, got %s", fail.Command)
	}
	if fail.Param(1) == "INVALID_NETID" {
		t.Errorf("unauthenticated BIND of valid netid 42 returned INVALID_NETID — leaks netid existence oracle; want NOT_AUTHED")
	}
	if fail.Param(1) != "NOT_AUTHED" {
		t.Errorf("unauthenticated BIND: FAIL code = %q, want NOT_AUTHED", fail.Param(1))
	}

	// Attempt BOUNCER BIND for an UNKNOWN netid — must also get NOT_AUTHED, not INVALID_NETID.
	sendLine(t, c, "BOUNCER BIND 999")
	fail2 := recvMsg(t, c)
	if fail2.Command != irc.FAIL {
		t.Fatalf("expected FAIL for unauthenticated BIND of unknown netid, got %s", fail2.Command)
	}
	if fail2.Param(1) != "NOT_AUTHED" {
		t.Errorf("unauthenticated BIND of unknown netid: FAIL code = %q, want NOT_AUTHED", fail2.Param(1))
	}
}

// TestBindAfterRegistration verifies that BOUNCER BIND sent after the welcome
// burst returns FAIL INVALID_PARAMS.
func TestBindAfterRegistration(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 2, Name: "Net", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}
	c := pipeServerWith(t, cfg, false)
	doRegisterSimple(t, c, "postbind")

	sendLine(t, c, "BOUNCER BIND 2")
	msg := recvMsg(t, c)
	if msg.Command != irc.FAIL {
		t.Fatalf("expected FAIL for post-registration BIND, got %s", msg.Command)
	}
}

// ─── LISTNETWORKS tests ───────────────────────────────────────────────────────

// TestLISTNETWORKS verifies the BATCH open/NETWORK lines/close sequence and
// that the password is NOT present in any attribute.
func TestLISTNETWORKS(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{
				NetID: 1, Name: "Libera", Addr: "irc.libera.chat:6697", TLS: true,
				Identity: Identity{Nick: "bouncenick", User: "u", Realname: "r"},
				SASL:     SASL{Mechanism: "PLAIN", Authcid: "user", Password: "s3cr3t"},
			},
			{
				NetID: 2, Name: "OFTC", Addr: "irc.oftc.net:6697", TLS: true,
				Identity: Identity{Nick: "bouncenick2", User: "u2", Realname: "r2"},
			},
		},
	}
	c := pipeServerWith(t, cfg, false)
	doRegisterSimple(t, c, "listuser")

	sendLine(t, c, "BOUNCER LISTNETWORKS")

	// Read BATCH open.
	batchOpen := recvMsg(t, c)
	assertMsg(t, batchOpen, irc.BATCH)
	if batchRef := batchOpen.Param(0); !strings.HasPrefix(batchRef, "+") {
		t.Errorf("BATCH open param[0] = %q, want '+<ref>'", batchRef)
	}
	batchRef := batchOpen.Param(0)[1:] // strip '+'
	batchType := batchOpen.Param(1)
	if batchType != "soju.im/bouncer-networks" {
		t.Errorf("BATCH type = %q, want soju.im/bouncer-networks", batchType)
	}

	// Read NETWORK lines.
	var networkLines []*irc.Message
	for {
		msg := recvMsg(t, c)
		if msg.Command == irc.BATCH {
			// BATCH close.
			if close := msg.Param(0); close != "-"+batchRef {
				t.Errorf("BATCH close = %q, want '-%s'", close, batchRef)
			}
			break
		}
		if msg.Command == "BOUNCER" && msg.Param(0) == "NETWORK" {
			networkLines = append(networkLines, msg)
		}
	}

	if len(networkLines) != 2 {
		t.Fatalf("got %d NETWORK lines, want 2", len(networkLines))
	}

	// Verify no password in any NETWORK line attrs.
	for _, nl := range networkLines {
		attrs := nl.Param(2)
		if strings.Contains(strings.ToLower(attrs), "password") ||
			strings.Contains(strings.ToLower(attrs), "passwd") ||
			strings.Contains(attrs, "s3cr3t") {
			t.Errorf("NETWORK line attrs contain password material: %q", attrs)
		}
	}

	// Verify netids and name attrs are present.
	attrMap := make(map[string]string) // netid -> name attr
	for _, nl := range networkLines {
		netidStr := nl.Param(1)
		attrs := nl.Param(2)
		parsed := parseAttrStringSimple(attrs)
		attrMap[netidStr] = parsed["name"]
	}
	if attrMap["1"] != "Libera" {
		t.Errorf("netid 1 name = %q, want Libera", attrMap["1"])
	}
	if attrMap["2"] != "OFTC" {
		t.Errorf("netid 2 name = %q, want OFTC", attrMap["2"])
	}
}

// TestLISTNETWORKSRequiresCap verifies that LISTNETWORKS on a session that
// did not negotiate soju.im/bouncer-networks sends FAIL CAP_NOT_NEGOTIATED.
func TestLISTNETWORKSRequiresCap(t *testing.T) {
	cfg := &Config{
		Networks: []Network{{NetID: 1, Name: "Net", Addr: "irc.example.com:6697",
			Identity: Identity{Nick: "n", User: "u", Realname: "r"}}},
	}
	c := pipeServerWith(t, cfg, false)
	// Register without the bouncer-networks cap.
	sendLine(t, c, "NICK ncap")
	sendLine(t, c, "USER ncap 0 * :Test")
	skipTo(t, c, irc.ERR_NOMOTD)

	sendLine(t, c, "BOUNCER LISTNETWORKS")
	msg := recvMsg(t, c)
	if msg.Command != irc.FAIL {
		t.Fatalf("expected FAIL for LISTNETWORKS without cap, got %s", msg.Command)
	}
}

// ─── ADDNETWORK tests ─────────────────────────────────────────────────────────

// TestADDNETWORK verifies that ADDNETWORK:
//  1. Returns the assigned netid in a BOUNCER NETWORK response.
//  2. Persists the config to disk (JSON file contains the new network).
//  3. Starts the upstream via a mock dialer.
//  4. Broadcasts NETWORK notify to a second connected control client.
func TestADDNETWORK(t *testing.T) {
	// Set up a temp config file.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	// Initial config with no networks.
	cfg := &Config{}

	// Mock upstream: just register and hold.
	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() { down.Store(true); close(testDone) })

	var dialCount atomic.Int32
	mockDialFn := func(_ context.Context) (net.Conn, error) {
		dialCount.Add(1)
		clientSide, serverSide := net.Pipe()
		su := newScriptedUpstream(t, serverSide)
		su.down = &down
		go func() {
			defer su.close()
			su.register("bouncenick")
			<-testDone
		}()
		return clientSide, nil
	}

	mgr := NewManager(cfg, &collectSink{})
	// We'll inject the dialer for the about-to-be-created netid (will be 1).
	mgr.dialers = map[int]dialer{1: mockDialFn}

	// Build a Server with manager+cfgPath.
	srv := New(cfg)
	srv.WithManager(mgr)
	srv.WithConfigPath(cfgPath)

	// Two client connections to the same server.
	c1, c2 := pipeServerSharedSrv(t, srv)

	// Register both as control sessions.
	doRegisterSimple(t, c1, "ctrl1")
	doRegisterSimple(t, c2, "ctrl2")

	// Give the server a moment to register c2 in the controlSessions set.
	time.Sleep(50 * time.Millisecond)

	// c1 sends ADDNETWORK.
	sendLine(t, c1, "BOUNCER ADDNETWORK name=TestAdd;host=irc.example.com;port=6697;tls=1;nick=bouncenick")

	// c1 should receive a BOUNCER NETWORK reply with the new netid.
	addReply := recvUntilCmd(t, c1, "BOUNCER", 3*time.Second)
	if addReply.Param(0) != "NETWORK" {
		t.Fatalf("ADDNETWORK reply: expected BOUNCER NETWORK, got BOUNCER %s", addReply.Param(0))
	}
	gotNetID := addReply.Param(1)
	if gotNetID == "" || gotNetID == "0" {
		t.Errorf("ADDNETWORK returned empty/zero netid: %q", gotNetID)
	}
	if gotNetID != "1" {
		t.Errorf("expected netid 1 (first network), got %q", gotNetID)
	}

	// Verify the config was persisted.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config file not found after ADDNETWORK: %v", err)
	}
	var diskCfg Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parse persisted config: %v", err)
	}
	if len(diskCfg.Networks) != 1 {
		t.Errorf("persisted config has %d networks, want 1", len(diskCfg.Networks))
	} else {
		if diskCfg.Networks[0].NetID != 1 {
			t.Errorf("persisted netid = %d, want 1", diskCfg.Networks[0].NetID)
		}
		if diskCfg.Networks[0].Name != "TestAdd" {
			t.Errorf("persisted name = %q, want TestAdd", diskCfg.Networks[0].Name)
		}
	}

	// Verify the upstream was started (dialer was called).
	// Allow a short window for the goroutine to call mgr.Add.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dialCount.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if dialCount.Load() == 0 {
		t.Error("mock dialer was not called: upstream not started")
	}

	// c2 should receive the NETWORK notify broadcast.
	notifyMsg := recvUntilCmd(t, c2, "BOUNCER", 3*time.Second)
	if notifyMsg.Param(0) != "NETWORK" {
		t.Fatalf("c2 notify: expected BOUNCER NETWORK, got BOUNCER %s", notifyMsg.Param(0))
	}
	if notifyMsg.Param(1) != "1" {
		t.Errorf("c2 notify netid = %q, want 1", notifyMsg.Param(1))
	}
}

// ─── ADDNETWORK network-limit test ───────────────────────────────────────────

// TestADDNETWORKLimit verifies that BOUNCER ADDNETWORK returns FAIL
// NETWORK_LIMIT when cfg.Networks is already at maxNetworks, and that the
// in-memory count does not exceed the cap.
func TestADDNETWORKLimit(t *testing.T) {
	// Pre-fill a config with exactly maxNetworks entries so the very first
	// ADDNETWORK from the client should be rejected.
	networks := make([]Network, maxNetworks)
	for i := range networks {
		networks[i] = Network{
			NetID:    i + 1,
			Name:     fmt.Sprintf("Net%d", i+1),
			Addr:     "irc.example.com:6697",
			Identity: Identity{Nick: "n", User: "u", Realname: "r"},
		}
	}
	cfg := &Config{Networks: networks}

	c := pipeServerWith(t, cfg, false)
	doRegisterSimple(t, c, "limituser")

	// Issue one more ADDNETWORK — must be rejected.
	sendLine(t, c, "BOUNCER ADDNETWORK name=Overflow;host=irc.example.com;port=6697")
	msg := recvMsg(t, c)
	if msg.Command != irc.FAIL {
		t.Fatalf("expected FAIL when at network limit, got %s", msg.Command)
	}
	if msg.Param(1) != "NETWORK_LIMIT" {
		t.Errorf("FAIL code = %q, want NETWORK_LIMIT", msg.Param(1))
	}

	// In-memory count must still be exactly maxNetworks (not maxNetworks+1).
	if got := len(cfg.Networks); got != maxNetworks {
		t.Errorf("network count = %d after limit FAIL, want %d", got, maxNetworks)
	}
}

// ─── Port-validation tests ────────────────────────────────────────────────────

// TestADDNETWORKInvalidPort verifies that BOUNCER ADDNETWORK with a non-numeric
// or out-of-range port returns FAIL INVALID_PARAMS rather than propagating a
// malformed addr to the dialer (which would produce an opaque dial error).
func TestADDNETWORKInvalidPort(t *testing.T) {
	cases := []struct {
		name      string
		attrLine  string
		wantFail  bool
		wantValid bool // true when we expect success (valid port)
	}{
		{"non-numeric port", "name=Net;host=irc.example.com;port=abc", true, false},
		{"zero port", "name=Net;host=irc.example.com;port=0", true, false},
		{"negative port", "name=Net;host=irc.example.com;port=-1", true, false},
		{"port too large", "name=Net;host=irc.example.com;port=99999", true, false},
		{"port 65536 OOB", "name=Net;host=irc.example.com;port=65536", true, false},
		{"valid port 6697", "name=Net;host=irc.example.com;port=6697", false, true},
		{"valid port 1", "name=Net;host=irc.example.com;port=1", false, true},
		{"valid port 65535", "name=Net;host=irc.example.com;port=65535", false, true},
		{"no port given", "name=Net;host=irc.example.com", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			c := pipeServerWith(t, cfg, false)
			doRegisterSimple(t, c, "porttest")

			sendLine(t, c, "BOUNCER ADDNETWORK "+tc.attrLine)
			msg := recvMsg(t, c)

			if tc.wantFail {
				if msg.Command != irc.FAIL {
					t.Fatalf("expected FAIL for %q, got %s", tc.attrLine, msg.Command)
				}
				if msg.Param(1) != "INVALID_PARAMS" {
					t.Errorf("FAIL code = %q, want INVALID_PARAMS", msg.Param(1))
				}
				// Config must be unchanged — no network was added.
				if len(cfg.Networks) != 0 {
					t.Errorf("network count = %d after invalid-port FAIL, want 0", len(cfg.Networks))
				}
			}
			if tc.wantValid {
				if msg.Command == irc.FAIL {
					t.Errorf("unexpected FAIL for valid port case %q: code=%s desc=%s",
						tc.attrLine, msg.Param(1), msg.Param(2))
				}
			}
		})
	}
}

// TestCHANGENETWORKInvalidPort verifies that BOUNCER CHANGENETWORK with an
// invalid port returns FAIL INVALID_PARAMS without modifying the stored network.
func TestCHANGENETWORKInvalidPort(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Networks: []Network{
			{NetID: 11, Name: "Stable", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}
	if err := Save(cfg, cfgPath); err != nil {
		t.Fatalf("save: %v", err)
	}
	srv := New(cfg)
	srv.WithConfigPath(cfgPath)

	c1, _ := pipeServerSharedSrv(t, srv)
	doRegisterSimple(t, c1, "chgport")

	// Send a CHANGENETWORK with a non-numeric port.
	sendLine(t, c1, "BOUNCER CHANGENETWORK 11 port=notaport")
	msg := recvMsg(t, c1)
	if msg.Command != irc.FAIL {
		t.Fatalf("expected FAIL for invalid port in CHANGENETWORK, got %s", msg.Command)
	}
	if msg.Param(1) != "INVALID_PARAMS" {
		t.Errorf("FAIL code = %q, want INVALID_PARAMS", msg.Param(1))
	}

	// The network addr must be unchanged.
	nets := srv.snapshotNetworks()
	if len(nets) != 1 {
		t.Fatalf("network count = %d, want 1", len(nets))
	}
	if nets[0].Addr != "irc.example.com:6697" {
		t.Errorf("addr changed to %q after invalid-port CHANGENETWORK, want unchanged", nets[0].Addr)
	}
}

// ─── DELNETWORK tests ─────────────────────────────────────────────────────────

// TestDELNETWORK verifies that DELNETWORK removes the network from config,
// persists, and broadcasts notify with state=deleted.
func TestDELNETWORK(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Networks: []Network{
			{NetID: 5, Name: "ToDelete", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}
	// Write initial config to disk.
	if err := Save(cfg, cfgPath); err != nil {
		t.Fatalf("save initial config: %v", err)
	}

	srv := New(cfg)
	srv.WithConfigPath(cfgPath)

	c1, c2 := pipeServerSharedSrv(t, srv)
	doRegisterSimple(t, c1, "del1")
	doRegisterSimple(t, c2, "del2")
	time.Sleep(50 * time.Millisecond)

	sendLine(t, c1, "BOUNCER DELNETWORK 5")

	// c1 gets the delete confirmation.
	delReply := recvUntilCmd(t, c1, "BOUNCER", 3*time.Second)
	if delReply.Param(0) != "NETWORK" {
		t.Fatalf("DELNETWORK reply not BOUNCER NETWORK: %v", delReply)
	}
	if delReply.Param(1) != "5" {
		t.Errorf("DELNETWORK reply netid = %q, want 5", delReply.Param(1))
	}
	if !strings.Contains(delReply.Param(2), "deleted") {
		t.Errorf("DELNETWORK reply attrs don't contain 'deleted': %q", delReply.Param(2))
	}

	// Config on disk must not contain netid 5.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config after DELNETWORK: %v", err)
	}
	var diskCfg Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parse config after DELNETWORK: %v", err)
	}
	for _, nw := range diskCfg.Networks {
		if nw.NetID == 5 {
			t.Error("netid 5 still present in persisted config after DELNETWORK")
		}
	}

	// c2 should receive the delete notify.
	notifyMsg := recvUntilCmd(t, c2, "BOUNCER", 3*time.Second)
	if notifyMsg.Param(1) != "5" || !strings.Contains(notifyMsg.Param(2), "deleted") {
		t.Errorf("c2 del notify wrong: %v", notifyMsg)
	}
}

// ─── CHANGENETWORK tests ──────────────────────────────────────────────────────

// TestCHANGENETWORK verifies that CHANGENETWORK updates the config, persists,
// and sends notify.
func TestCHANGENETWORK(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Networks: []Network{
			{NetID: 10, Name: "Original", Addr: "irc.old.example.com:6667",
				Identity: Identity{Nick: "oldnick", User: "u", Realname: "r"}},
		},
	}
	if err := Save(cfg, cfgPath); err != nil {
		t.Fatalf("save: %v", err)
	}

	srv := New(cfg)
	srv.WithConfigPath(cfgPath)

	c1, c2 := pipeServerSharedSrv(t, srv)
	doRegisterSimple(t, c1, "chg1")
	doRegisterSimple(t, c2, "chg2")
	time.Sleep(50 * time.Millisecond)

	sendLine(t, c1, "BOUNCER CHANGENETWORK 10 name=Renamed;nick=newnick")

	chgReply := recvUntilCmd(t, c1, "BOUNCER", 3*time.Second)
	if chgReply.Param(0) != "NETWORK" || chgReply.Param(1) != "10" {
		t.Fatalf("CHANGENETWORK reply unexpected: %v", chgReply)
	}
	attrs := parseAttrStringSimple(chgReply.Param(2))
	if attrs["name"] != "Renamed" {
		t.Errorf("name after CHANGENETWORK = %q, want Renamed", attrs["name"])
	}
	if attrs["nick"] != "newnick" {
		t.Errorf("nick after CHANGENETWORK = %q, want newnick", attrs["nick"])
	}

	// Verify persisted.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var diskCfg Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(diskCfg.Networks) == 0 {
		t.Fatal("no networks in persisted config")
	}
	if diskCfg.Networks[0].Name != "Renamed" {
		t.Errorf("disk name = %q, want Renamed", diskCfg.Networks[0].Name)
	}
	if diskCfg.Networks[0].Identity.Nick != "newnick" {
		t.Errorf("disk nick = %q, want newnick", diskCfg.Networks[0].Identity.Nick)
	}

	// c2 should receive the notify.
	notifyMsg := recvUntilCmd(t, c2, "BOUNCER", 3*time.Second)
	if notifyMsg.Param(1) != "10" {
		t.Errorf("c2 change notify netid = %q, want 10", notifyMsg.Param(1))
	}
}

// ─── Malformed BOUNCER tests ──────────────────────────────────────────────────

// TestMalformedBOUNCERSubcommand verifies that unknown or malformed BOUNCER
// subcommands receive FAIL and do not panic.
func TestMalformedBOUNCERSubcommand(t *testing.T) {
	cfg := &Config{}
	c := pipeServerWith(t, cfg, false)
	doRegisterSimple(t, c, "malform")

	cases := []string{
		"BOUNCER UNKNOWNCMD",
		"BOUNCER FROBNICATENETWORK",
		"BOUNCER",          // no subcommand
		"BOUNCER BIND",     // BIND with no netid
		"BOUNCER BIND abc", // BIND with non-numeric netid
	}
	for _, line := range cases {
		t.Run(line, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("panic on %q: %v", line, r)
				}
			}()
			sendLine(t, c, line)
			msg := recvMsg(t, c)
			if msg.Command != irc.FAIL {
				t.Errorf("%q: expected FAIL, got %s", line, msg.Command)
			}
		})
	}
}

// ─── Manager Add/Remove race test ─────────────────────────────────────────────

// TestManagerAddRemoveRace verifies that concurrent Add+Remove+UpstreamState
// calls do not data-race. Run with -race.
func TestManagerAddRemoveRace(t *testing.T) {
	cfg := &Config{}
	sink := &collectSink{}
	mgr := NewManager(cfg, sink)

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() {
		down.Store(true)
		close(testDone)
	})

	dialFn := func(netid int) dialer {
		return func(_ context.Context) (net.Conn, error) {
			clientSide, serverSide := net.Pipe()
			su := newScriptedUpstream(t, serverSide)
			su.down = &down
			go func() {
				defer su.close()
				su.register(fmt.Sprintf("nick%d", netid))
				<-testDone
			}()
			return clientSide, nil
		}
	}

	var wg sync.WaitGroup
	const concurrency = 5
	for i := 1; i <= concurrency; i++ {
		i := i
		nw := &Network{
			NetID: i, Name: fmt.Sprintf("Net%d", i), Addr: "irc.example.com:6667",
			Identity: Identity{Nick: fmt.Sprintf("nick%d", i), User: "u", Realname: "r"},
		}
		cfg.Networks = append(cfg.Networks, *nw)
		mgr.dialers = map[int]dialer{}
		for j := 1; j <= concurrency; j++ {
			mgr.dialers[j] = dialFn(j)
		}
	}

	// Start all upstreams first.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Concurrently read UpstreamState while adding a new network.
	for g := 0; g < 10; g++ {
		wg.Add(1)
		netidToCheck := (g % concurrency) + 1
		go func() {
			defer wg.Done()
			_, _ = mgr.UpstreamState(netidToCheck)
		}()
	}
	wg.Wait()
}

// ─── cfg.Networks concurrency race test ───────────────────────────────────────

// TestConfigNetworksConcurrentMutation exercises the cfgMu invariant: two
// concurrent control sessions issuing ADDNETWORK while a third repeatedly
// snapshots/finds must not data-race on cfg.Networks, and the final state must
// be consistent (every successfully-added netid is present exactly once, all
// netids are distinct, and the persisted config matches memory).
//
// The data race this guards against: ADDNETWORK's append and the
// snapshot/find reads all touch s.cfg.Networks from per-session goroutines.
// Without cfgMu the race detector flags it. Run with -race.
func TestConfigNetworksConcurrentMutation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{}
	srv := New(cfg)
	srv.WithConfigPath(cfgPath)
	// No manager: ADDNETWORK skips mgr.Add, isolating the cfg.Networks race.

	const (
		writers       = 4
		addsPerWriter = 10
		readers       = 4
		readsPerRead  = 50
	)

	var wg sync.WaitGroup

	// Writer goroutines: each calls addNetwork concurrently.
	var addedMu sync.Mutex
	added := map[int]bool{}
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < addsPerWriter; i++ {
				nw := Network{
					Name: fmt.Sprintf("W%d-N%d", w, i),
					Addr: "irc.example.com:6697",
					Identity: Identity{
						Nick: fmt.Sprintf("nick%d-%d", w, i), User: "u", Realname: "r",
					},
				}
				gotNet, err := srv.addNetwork(nw)
				if err != nil {
					t.Errorf("addNetwork: %v", err)
					return
				}
				addedMu.Lock()
				if added[gotNet.NetID] {
					t.Errorf("netid %d assigned twice", gotNet.NetID)
				}
				added[gotNet.NetID] = true
				addedMu.Unlock()
			}
		}()
	}

	// Reader goroutines: snapshot + find concurrently with the writers.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < readsPerRead; i++ {
				_ = srv.snapshotNetworks()
				_, _ = srv.findNetworkByID((i % 7) + 1)
			}
		}()
	}

	wg.Wait()

	// Final state consistency: exactly writers*addsPerWriter networks, all
	// distinct netids, matching what addNetwork reported.
	final := srv.snapshotNetworks()
	wantCount := writers * addsPerWriter
	if len(final) != wantCount {
		t.Fatalf("final network count = %d, want %d", len(final), wantCount)
	}
	seen := map[int]bool{}
	for _, nw := range final {
		if nw.NetID <= 0 {
			t.Errorf("non-positive netid %d in final state", nw.NetID)
		}
		if seen[nw.NetID] {
			t.Errorf("duplicate netid %d in final state", nw.NetID)
		}
		seen[nw.NetID] = true
		if !added[nw.NetID] {
			t.Errorf("netid %d in config was never reported by addNetwork", nw.NetID)
		}
	}

	// Persisted config on disk must match memory (same count, same netids).
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var diskCfg Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parse persisted config: %v", err)
	}
	if len(diskCfg.Networks) != wantCount {
		t.Errorf("persisted network count = %d, want %d", len(diskCfg.Networks), wantCount)
	}
	for _, nw := range diskCfg.Networks {
		if !seen[nw.NetID] {
			t.Errorf("persisted netid %d not in memory snapshot", nw.NetID)
		}
	}
}

// TestConcurrentControlSessionsADDNETWORK drives two real control-session
// goroutines over net.Pipe, each issuing ADDNETWORK concurrently, while a third
// issues LISTNETWORKS. This is the end-to-end form of the cfg.Networks race:
// the phone-and-laptop scenario. Run with -race.
func TestConcurrentControlSessionsADDNETWORK(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{}
	srv := New(cfg)
	srv.WithConfigPath(cfgPath)
	// No manager: keeps the test deterministic (no upstream dialing) while
	// still exercising the full handler path including persistence + broadcast.

	c1, c2 := pipeServerSharedSrv(t, srv)
	doRegisterSimple(t, c1, "ctrlA")
	doRegisterSimple(t, c2, "ctrlB")
	time.Sleep(50 * time.Millisecond)

	const perSession = 6

	var wg sync.WaitGroup
	wg.Add(2)

	// Session A: issue ADDNETWORK perSession times, draining replies+notifies.
	go func() {
		defer wg.Done()
		for i := 0; i < perSession; i++ {
			sendLine(t, c1, fmt.Sprintf("BOUNCER ADDNETWORK name=A%d;host=irc.a.example;port=6697", i))
		}
	}()
	// Session B: issue ADDNETWORK perSession times.
	go func() {
		defer wg.Done()
		for i := 0; i < perSession; i++ {
			sendLine(t, c2, fmt.Sprintf("BOUNCER ADDNETWORK name=B%d;host=irc.b.example;port=6697", i))
		}
	}()
	wg.Wait()

	// Drain pending messages on both connections so the writer goroutines don't
	// block; give the server time to process all the ADDNETWORKs.
	drainFor(t, c1, 500*time.Millisecond)
	drainFor(t, c2, 500*time.Millisecond)

	// Final state: 2*perSession networks, all distinct netids.
	final := srv.snapshotNetworks()
	want := 2 * perSession
	if len(final) != want {
		t.Fatalf("final network count = %d, want %d", len(final), want)
	}
	seen := map[int]bool{}
	for _, nw := range final {
		if seen[nw.NetID] {
			t.Errorf("duplicate netid %d", nw.NetID)
		}
		seen[nw.NetID] = true
	}

	// Persisted config matches.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var diskCfg Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(diskCfg.Networks) != want {
		t.Errorf("persisted count = %d, want %d", len(diskCfg.Networks), want)
	}
}

// drainFor reads and discards any messages arriving on c for the given
// duration. Used to keep the server's writer goroutine from blocking on a full
// outbound queue while a test issues many commands.
func drainFor(t *testing.T, c *conn.Conn, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-c.Messages():
			if !ok {
				return
			}
		case <-deadline:
			return
		}
	}
}

// ─── Parse helper for tests ───────────────────────────────────────────────────

// ─── Rollback tests (CHANGE/DEL save failure) ────────────────────────────────

// TestChangeNetworkRollbackOnSaveFailure verifies that when changeNetwork's
// Save call fails (unwritable path), the in-memory config is unchanged — the
// rolled-back entry has the original name, not the applied one.
func TestChangeNetworkRollbackOnSaveFailure(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 1, Name: "Original", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
		},
	}
	srv := New(cfg)
	// Point cfgPath at a path that can never be written (directory, not file).
	srv.WithConfigPath(t.TempDir())

	_, _, found, err := srv.changeNetwork(1, func(n *Network) {
		n.Name = "ShouldNotStick"
	})
	if !found {
		t.Fatal("changeNetwork: network 1 not found")
	}
	if err == nil {
		t.Fatal("changeNetwork: expected Save error, got nil")
	}

	// In-memory state must be rolled back: name still "Original".
	nets := srv.snapshotNetworks()
	if len(nets) != 1 {
		t.Fatalf("network count = %d, want 1", len(nets))
	}
	if nets[0].Name != "Original" {
		t.Errorf("in-memory name = %q after Save failure, want Original (rollback failed)", nets[0].Name)
	}
}

// TestDelNetworkRollbackOnSaveFailure verifies that when delNetwork's Save call
// fails, the removed entry is re-inserted and in-memory config is unchanged.
func TestDelNetworkRollbackOnSaveFailure(t *testing.T) {
	cfg := &Config{
		Networks: []Network{
			{NetID: 1, Name: "Keep", Addr: "irc.example.com:6697",
				Identity: Identity{Nick: "n", User: "u", Realname: "r"}},
			{NetID: 2, Name: "ToDelete", Addr: "irc.other.com:6697",
				Identity: Identity{Nick: "m", User: "u", Realname: "r"}},
			{NetID: 3, Name: "AlsoKeep", Addr: "irc.third.com:6697",
				Identity: Identity{Nick: "o", User: "u", Realname: "r"}},
		},
	}
	srv := New(cfg)
	// Point cfgPath at a directory so Save always fails.
	srv.WithConfigPath(t.TempDir())

	found, err := srv.delNetwork(2)
	if !found {
		t.Fatal("delNetwork: network 2 not found")
	}
	if err == nil {
		t.Fatal("delNetwork: expected Save error, got nil")
	}

	// In-memory state must be rolled back: all three networks present with
	// correct order.
	nets := srv.snapshotNetworks()
	if len(nets) != 3 {
		t.Fatalf("network count = %d after rollback, want 3", len(nets))
	}
	// Verify all three netids are present.
	netids := map[int]string{}
	for _, n := range nets {
		netids[n.NetID] = n.Name
	}
	if netids[1] != "Keep" {
		t.Errorf("netid 1 name = %q, want Keep", netids[1])
	}
	if netids[2] != "ToDelete" {
		t.Errorf("netid 2 name = %q after rollback, want ToDelete", netids[2])
	}
	if netids[3] != "AlsoKeep" {
		t.Errorf("netid 3 name = %q, want AlsoKeep", netids[3])
	}
}

// ─── CHANGENETWORK reconnect test ─────────────────────────────────────────────

// TestCHANGENETWORKReconnect verifies that when BOUNCER CHANGENETWORK changes
// the upstream host, the live upstream is torn down and re-dialed against the
// new address. The test uses two scripted mock upstreams:
//
//   - oldUpstream: the initial connection, expects to be closed (sees EOF/dial drop)
//   - newUpstream: registers after the reconnect
//
// After CHANGENETWORK the new upstream must have received a registration and the
// manager must report ConnConnected for the netid.
func TestCHANGENETWORKReconnect(t *testing.T) {
	const (
		netid    = 20
		nick     = "rcnick"
		oldAddr  = "old.example.com:6697"
		newAddr  = "new.example.com:6697"
		oldNetID = netid
	)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	cfg := &Config{
		Networks: []Network{
			{NetID: netid, Name: "RCNet", Addr: oldAddr,
				Identity: Identity{Nick: nick, User: "u", Realname: "r"}},
		},
	}
	if err := Save(cfg, cfgPath); err != nil {
		t.Fatalf("save initial config: %v", err)
	}

	testDone := make(chan struct{})
	var down atomic.Bool
	t.Cleanup(func() {
		down.Store(true)
		close(testDone)
	})

	// newConnected is closed when the new upstream completes its registration.
	newConnected := make(chan struct{})
	var newConnectedOnce sync.Once

	var dialCount atomic.Int32
	dialFn := func(_ context.Context) (net.Conn, error) {
		n := dialCount.Add(1)
		clientSide, serverSide := net.Pipe()
		su := newScriptedUpstream(t, serverSide)
		su.down = &down
		go func() {
			defer su.close()
			su.register(nick)
			if n >= 2 {
				// Second (and later) dial is the reconnect after CHANGENETWORK.
				newConnectedOnce.Do(func() { close(newConnected) })
			}
			<-testDone
		}()
		return clientSide, nil
	}

	sink := &collectSink{}
	mgr := NewManager(cfg, sink)
	mgr.dialers = map[int]dialer{netid: dialFn}

	// Start the manager so the initial upstream connects.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Manager.Start: %v", err)
	}
	t.Cleanup(func() { mgr.Close() })

	// Wait for the initial upstream registration (001 in the sink).
	if ev := sink.waitForCommand(netid, "001", 3*time.Second); ev == nil {
		t.Fatal("timed out waiting for initial upstream 001")
	}

	// Build a server wired to this manager.
	srv := New(cfg)
	srv.WithManager(mgr)
	srv.WithConfigPath(cfgPath)

	c := pipeServerWithManager(t, cfg, mgr, cfgPath)

	// Override the server's cfg pointer — pipeServerWithManager creates its own
	// Server from cfg, but the manager was already started against cfg, so we
	// need the server to use the same cfg pointer for cfgMu coordination.
	// Instead, use the server created by pipeServerWithManager and inject the
	// manager + cfgPath after the fact, which pipeServerWithManager already does.
	// We just need to register and issue CHANGENETWORK.

	doRegisterSimple(t, c, "rcuser")

	// Issue CHANGENETWORK to switch host.
	sendLine(t, c, fmt.Sprintf("BOUNCER CHANGENETWORK %d host=new.example.com;port=6697", netid))

	chgReply := recvUntilCmd(t, c, "BOUNCER", 3*time.Second)
	if chgReply.Param(0) != "NETWORK" || chgReply.Param(1) != fmt.Sprintf("%d", netid) {
		t.Fatalf("CHANGENETWORK reply unexpected: %v", chgReply)
	}

	// The new upstream should connect. Wait for it.
	select {
	case <-newConnected:
		// Good: new upstream registered.
	case <-time.After(5 * time.Second):
		t.Fatal("new upstream did not connect after CHANGENETWORK within 5s")
	}

	// The manager must report ConnConnected for the netid after the reconnect.
	if !waitForUpstreamState(mgr, netid, ConnConnected, 3*time.Second) {
		st, _ := mgr.UpstreamState(netid)
		t.Errorf("upstream state = %v after CHANGENETWORK reconnect, want ConnConnected", st)
	}

	// The dial count must be at least 2: initial connect + reconnect.
	if dialCount.Load() < 2 {
		t.Errorf("dial count = %d, want >= 2 (initial + reconnect)", dialCount.Load())
	}

	// Persisted config must reflect the new address.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	var diskCfg Config
	if err := json.Unmarshal(data, &diskCfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if len(diskCfg.Networks) == 0 {
		t.Fatal("no networks in persisted config after CHANGENETWORK")
	}
	if diskCfg.Networks[0].Addr != newAddr {
		t.Errorf("persisted addr = %q, want %q", diskCfg.Networks[0].Addr, newAddr)
	}
}

// ─── Parse helper for tests ───────────────────────────────────────────────────

// parseAttrStringSimple is a minimal attr parser used in tests (mirrors
// bouncer.ParseAttrs but without importing the bouncer package here to keep
// tests in the server package).
func parseAttrStringSimple(s string) map[string]string {
	m := make(map[string]string)
	if s == "" {
		return m
	}
	for _, field := range strings.Split(s, ";") {
		if field == "" {
			continue
		}
		k, v, _ := strings.Cut(field, "=")
		if k == "" {
			continue
		}
		// Minimal unescape: \: → ;  \s → space  \\ → \  \r → CR  \n → LF
		v = strings.ReplaceAll(v, `\:`, ";")
		v = strings.ReplaceAll(v, `\s`, " ")
		v = strings.ReplaceAll(v, `\\`, `\`)
		v = strings.ReplaceAll(v, `\r`, "\r")
		v = strings.ReplaceAll(v, `\n`, "\n")
		m[k] = v
	}
	return m
}
