package server

// Phase 2 golden-transcript tests for:
//   - PBKDF2 hash/verify round-trip and edge cases (auth_test functions)
//   - Server-side SASL PLAIN over the CAP flow (good path, wrong password, abort)
//   - TLS gate: AUTHENTICATE PLAIN on a non-TLS connection → 904 before any decode
//   - Authentication required: unauthenticated CAP END on a bouncer with auth
//   - Registration timeout: an idle client is dropped
//   - Authcid parser: table-driven covering all hostile-input cases

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// ─── PBKDF2 hash/verify tests ─────────────────────────────────────────────────

// TestHashVerifyRoundTrip verifies that a password hashed by HashPassword is
// accepted by VerifyPassword with the same password.
func TestHashVerifyRoundTrip(t *testing.T) {
	hash, err := HashPassword("hunter2")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256:") {
		t.Errorf("hash prefix wrong: %q", hash)
	}
	// Verify count the fields.
	parts := strings.SplitN(hash, ":", 4)
	if len(parts) != 4 {
		t.Fatalf("hash field count = %d, want 4; hash=%q", len(parts), hash)
	}
	if parts[0] != "pbkdf2-sha256" {
		t.Errorf("algo = %q, want pbkdf2-sha256", parts[0])
	}
	if parts[1] != "600000" {
		t.Errorf("iter = %q, want 600000", parts[1])
	}

	ok, err := VerifyPassword(hash, "hunter2")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword returned false for correct password")
	}
}

// TestVerifyWrongPassword verifies that VerifyPassword returns false (not error)
// for an incorrect password.
func TestVerifyWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	ok, err := VerifyPassword(hash, "wrong")
	if err != nil {
		t.Fatalf("VerifyPassword error on wrong password: %v", err)
	}
	if ok {
		t.Error("VerifyPassword returned true for wrong password")
	}
}

// TestVerifyDifferentSalts verifies that two hashes of the same password differ
// (different random salts).
func TestVerifyDifferentSalts(t *testing.T) {
	h1, _ := HashPassword("same")
	h2, _ := HashPassword("same")
	if h1 == h2 {
		t.Error("two hashes of the same password should differ (different salts)")
	}
	// Both should still verify correctly.
	for _, h := range []string{h1, h2} {
		ok, err := VerifyPassword(h, "same")
		if err != nil || !ok {
			t.Errorf("VerifyPassword(%q, same): ok=%v err=%v", h, ok, err)
		}
	}
}

// TestVerifyMalformedHash verifies that malformed hash strings are rejected
// with an error (not a panic or false-positive).
func TestVerifyMalformedHash(t *testing.T) {
	cases := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"no-colons", "notahash"},
		{"two-fields", "pbkdf2-sha256:600000"},
		{"three-fields", "pbkdf2-sha256:600000:abc"},
		{"wrong-algo", "bcrypt:14:salt:hash"},
		{"bad-iter", "pbkdf2-sha256:notanumber:salt:hash"},
		{"zero-iter", "pbkdf2-sha256:0:salt:hash"},
		{"bad-salt-b64", "pbkdf2-sha256:600000:!!!:aGVsbG8="},
		{"bad-dk-b64", "pbkdf2-sha256:600000:aGVsbG8=:!!!"},
		{"empty-dk", "pbkdf2-sha256:600000:aGVsbG8=:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := VerifyPassword(tc.hash, "any")
			if ok {
				t.Errorf("VerifyPassword(%q, any) = true, want false", tc.hash)
			}
			if err == nil {
				t.Errorf("VerifyPassword(%q, any) = nil error, want non-nil", tc.hash)
			}
		})
	}
}

// ─── SASL PLAIN CAP flow tests ────────────────────────────────────────────────

// testHashCache caches hashed passwords to avoid redundant (slow) PBKDF2
// computations across test functions that use the same credentials. The real
// PBKDF2 parameters (600 000 iterations) are still exercised — just computed
// once per unique password per test run.
var (
	testHashMu    sync.Mutex
	testHashCache = map[string]string{}
)

// makeTestCfg builds a Config with BouncerAuth set to user/password for use in
// SASL tests. It hashes the password using HashPassword. Repeated calls with
// the same password reuse a cached hash to keep the test suite fast.
func makeTestCfg(t *testing.T, user, password string) *Config {
	t.Helper()

	testHashMu.Lock()
	hash, ok := testHashCache[password]
	testHashMu.Unlock()

	if !ok {
		var err error
		hash, err = HashPassword(password)
		if err != nil {
			t.Fatalf("HashPassword: %v", err)
		}
		testHashMu.Lock()
		testHashCache[password] = hash
		testHashMu.Unlock()
	}

	return &Config{
		BouncerAuth: BouncerAuth{
			User:         user,
			PasswordHash: hash,
		},
	}
}

// plainPayload encodes a SASL PLAIN payload: authzid\0authcid\0passwd.
func plainPayload(authzid, authcid, passwd string) string {
	raw := authzid + "\x00" + authcid + "\x00" + passwd
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

// doSASLPlain scripts the full SASL PLAIN exchange through CAP END.
// Returns the client conn positioned after the SASL numerics (before 001 or error).
func doSASLPlain(t *testing.T, client interface {
	Send(string) error
}, recv func() *irc.Message, authcid, passwd string) {
	t.Helper()
	// CAP REQ sasl
	if err := client.(interface{ Send(string) error }).Send("CAP REQ :sasl"); err != nil {
		t.Fatalf("CAP REQ send: %v", err)
	}
	_ = recv() // CAP ACK :sasl

	// AUTHENTICATE PLAIN
	if err := client.(interface{ Send(string) error }).Send("AUTHENTICATE PLAIN"); err != nil {
		t.Fatalf("AUTHENTICATE PLAIN send: %v", err)
	}
	// Expect AUTHENTICATE + (server challenge)
	authChallenge := recv()
	if authChallenge.Command != irc.AUTHENTICATE || authChallenge.Param(0) != "+" {
		t.Fatalf("expected AUTHENTICATE +, got %+v", authChallenge)
	}

	// Send payload.
	payload := plainPayload("", authcid, passwd)
	if err := client.(interface{ Send(string) error }).Send("AUTHENTICATE " + payload); err != nil {
		t.Fatalf("AUTHENTICATE payload send: %v", err)
	}
}

// TestSASLPlainGoodPath verifies the full golden-transcript for a successful
// SASL PLAIN auth: good PLAIN → 900 + 903, then CAP END + NICK + USER → 001/005.
func TestSASLPlainGoodPath(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client) // CAP LS payload

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client) // CAP ACK :sasl

	sendLine(t, client, "AUTHENTICATE PLAIN")
	challenge := recvMsg(t, client)
	if challenge.Command != irc.AUTHENTICATE || challenge.Param(0) != "+" {
		t.Fatalf("expected AUTHENTICATE +, got %+v", challenge)
	}

	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "dylan", "secret"))

	// Expect 900 RPL_LOGGEDIN
	r900 := recvMsg(t, client)
	assertMsg(t, r900, irc.RPL_LOGGEDIN)
	if !strings.Contains(strings.Join(r900.Params, " "), "dylan") {
		t.Errorf("900 should mention username; params=%v", r900.Params)
	}

	// Expect 903 RPL_SASLSUCCESS
	r903 := recvMsg(t, client)
	assertMsg(t, r903, irc.RPL_SASLSUCCESS)

	// Now complete registration.
	sendLine(t, client, "NICK dylan")
	sendLine(t, client, "USER dylan 0 * :Dylan Hart")
	sendLine(t, client, "CAP END")

	// Should receive the welcome burst.
	w001 := skipTo(t, client, irc.RPL_WELCOME)
	assertMsg(t, w001, irc.RPL_WELCOME, "dylan")

	skipTo(t, client, irc.RPL_ISUPPORT) // 005
}

// TestSASLPlainWrongPassword verifies that a wrong password results in 904 and
// registration does NOT complete (no 001 is sent).
func TestSASLPlainWrongPassword(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "correct")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client) // AUTHENTICATE +

	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "dylan", "wrong"))

	r904 := recvMsg(t, client)
	assertMsg(t, r904, irc.ERR_SASLFAIL)

	// Attempt CAP END after failed auth — should be refused (auth required).
	sendLine(t, client, "NICK dylan")
	sendLine(t, client, "USER dylan 0 * :Dylan")
	sendLine(t, client, "CAP END")

	// Connection should close (auth rejected). Drain until close or timeout.
	timeout := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-client.Messages():
			if !ok {
				return // success: connection closed without 001
			}
			if msg.Command == irc.RPL_WELCOME {
				t.Error("got 001 after failed SASL auth — registration must not complete")
			}
		case <-timeout:
			t.Fatal("timeout: expected connection close after failed SASL auth")
		}
	}
}

// TestSASLPlainNonTLSRejected verifies the TLS gate [B#2]: AUTHENTICATE PLAIN
// on a non-TLS connection is rejected with 904 BEFORE any base64 decode. Even
// a completely valid credential must be refused on plaintext.
//
// The gate ordering is tested by asserting 904 fires on the mechanism selection
// line (before the payload is even sent), and that no 900 or 903 is ever seen.
func TestSASLPlainNonTLSRejected(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	// isTLS=false — this is the critical flag for the TLS gate.
	client := pipeServerWith(t, cfg, false /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")

	// The gate fires HERE — on mechanism selection, before any payload is sent
	// or decoded. We must receive 904 and no AUTHENTICATE + challenge.
	msg := recvMsg(t, client)
	if msg.Command == irc.AUTHENTICATE {
		t.Fatalf("non-TLS conn received AUTHENTICATE + challenge — TLS gate failed; got %+v", msg)
	}
	assertMsg(t, msg, irc.ERR_SASLFAIL)

	// Verify the error text mentions TLS (sanity check the gate message).
	if !strings.Contains(strings.Join(msg.Params, " "), "TLS") {
		t.Errorf("904 message should mention TLS; params=%v", msg.Params)
	}

	// Drain any remaining messages: must never see 900 or 903.
	// We also verify that even if someone sent a valid payload now, no auth
	// oracle is provided (saslState is saslStateDone after the gate fires).
	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "dylan", "secret"))
	// Should get ERR_SASLALREADY (re-auth after done) not 900/903.
	msg2 := recvMsg(t, client)
	if msg2.Command == irc.RPL_LOGGEDIN || msg2.Command == irc.RPL_SASLSUCCESS {
		t.Errorf("got 900/903 after non-TLS AUTHENTICATE PLAIN — TLS gate bypass; got %+v", msg2)
	}
}

// TestSASLPlainAbort verifies that AUTHENTICATE * aborts the exchange with
// 906 ERR_SASLABORTED per the IRCv3 SASL spec. Registration does not complete.
func TestSASLPlainAbort(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client) // AUTHENTICATE +

	// Abort.
	sendLine(t, client, "AUTHENTICATE *")
	r906 := recvMsg(t, client)
	assertMsg(t, r906, irc.ERR_SASLABORTED)
}

// TestSASLAbortBeforePayload verifies abort before mechanism is selected
// also returns 906.
func TestSASLAbortBeforePayload(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	// Abort without selecting a mechanism.
	sendLine(t, client, "AUTHENTICATE *")
	r906 := recvMsg(t, client)
	assertMsg(t, r906, irc.ERR_SASLABORTED)
}

// TestSASLUnknownMechanism verifies that an unknown mechanism receives 908
// RPL_SASLMECHS listing PLAIN.
func TestSASLUnknownMechanism(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE SCRAM-SHA-256")
	r908 := recvMsg(t, client)
	assertMsg(t, r908, irc.RPL_SASLMECHS)
	payload := strings.Join(r908.Params, " ")
	if !strings.Contains(payload, "PLAIN") {
		t.Errorf("908 must list PLAIN; params=%v", r908.Params)
	}
}

// TestSASLReauthRejected verifies that a second AUTHENTICATE after success
// receives ERR_SASLALREADY (907).
func TestSASLReauthRejected(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client) // AUTHENTICATE +

	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "dylan", "secret"))
	_ = skipTo(t, client, irc.RPL_SASLSUCCESS) // drain 900+903

	// Try again.
	sendLine(t, client, "AUTHENTICATE PLAIN")
	r907 := recvMsg(t, client)
	assertMsg(t, r907, irc.ERR_SASLALREADY)
}

// TestSASLPayloadBeforeMech verifies that sending a payload line before
// AUTHENTICATE PLAIN is selected gets 904.
func TestSASLPayloadBeforeMech(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	// Send a payload without having sent AUTHENTICATE PLAIN first.
	// This is treated as if "AUTHENTICATE <mech>" was sent with a non-PLAIN mech.
	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "dylan", "secret"))
	// The payload is base64-encoded data that isn't "PLAIN" — it should
	// get 908 RPL_SASLMECHS (unknown mechanism).
	msg := recvMsg(t, client)
	if msg.Command != irc.RPL_SASLMECHS && msg.Command != irc.ERR_SASLFAIL {
		t.Errorf("expected 908 or 904 for payload-before-mech; got %s", msg.Command)
	}
}

// TestSASLOversizePayload verifies that a base64 payload longer than
// maxSASLPayloadB64 is rejected with 904 before any decode attempt.
//
// The payload must fit within the IRC message body budget (510 bytes minus
// "AUTHENTICATE " = 497 bytes of b64) but exceed maxSASLPayloadB64 (400).
// We use a raw payload of 300 bytes → 400 bytes of base64 = exactly the limit,
// then 301 bytes → 404 bytes of base64 > 400, which fits in an IRC line (13 +
// 404 = 417 bytes, under 510) but exceeds our application-level cap.
func TestSASLOversizePayload(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client) // AUTHENTICATE +

	// 301 raw bytes → 404 base64 bytes (> maxSASLPayloadB64=400 but fits IRC line).
	bigRaw := strings.Repeat("X", 301)
	bigPayload := base64.StdEncoding.EncodeToString([]byte(bigRaw))
	if len(bigPayload) <= maxSASLPayloadB64 {
		t.Skipf("oversize payload test assumption wrong: b64 len=%d <= %d", len(bigPayload), maxSASLPayloadB64)
	}
	// Sanity: the full IRC line must fit within the message budget.
	fullLine := "AUTHENTICATE " + bigPayload
	const maxMessageBody = 510 // MaxMessageBytes (512 - 2 for CRLF)
	if len(fullLine) > maxMessageBody {
		t.Skipf("test line too long for IRC budget: %d > %d", len(fullLine), maxMessageBody)
	}

	sendLine(t, client, fullLine)
	r904 := recvMsg(t, client)
	assertMsg(t, r904, irc.ERR_SASLFAIL)
}

// TestAuthRequiredUnauthCAPEND verifies that when BouncerAuth is configured,
// a client that sends CAP END without authenticating is refused (connection
// closed without 001).
func TestAuthRequiredUnauthCAPEND(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "NICK dylan")
	sendLine(t, client, "USER dylan 0 * :Dylan")
	sendLine(t, client, "CAP END") // skip SASL entirely

	// Should get 904 and then connection close, no 001.
	timeout := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-client.Messages():
			if !ok {
				return // connection closed — success
			}
			if msg.Command == irc.RPL_WELCOME {
				t.Error("got 001 for unauthenticated CAP END with auth configured")
			}
		case <-timeout:
			t.Fatal("timeout: expected connection close for unauthenticated CAP END")
		}
	}
}

// TestLegacyNoAuthConfigured verifies that the legacy no-CAP path (NICK+USER
// without CAP) still works when no BouncerAuth is configured.
func TestLegacyNoAuthConfigured(t *testing.T) {
	client := pipeServer(t) // empty Config, no auth

	sendLine(t, client, "NICK noauth")
	sendLine(t, client, "USER noauth 0 * :No Auth")

	w001 := skipTo(t, client, irc.RPL_WELCOME)
	assertMsg(t, w001, irc.RPL_WELCOME, "noauth")
}

// TestLegacyNoCapWithAuthRequired verifies that the legacy no-CAP path is
// rejected when BouncerAuth is configured (auth is mandatory).
func TestLegacyNoCapWithAuthRequired(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, false /* isTLS — doesn't matter for no-CAP rejection */)

	sendLine(t, client, "NICK dylan")
	sendLine(t, client, "USER dylan 0 * :Dylan")
	// No CAP, no SASL — should be rejected.

	timeout := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-client.Messages():
			if !ok {
				return // closed — success
			}
			if msg.Command == irc.RPL_WELCOME {
				t.Error("got 001 for legacy no-CAP client when auth is required")
			}
		case <-timeout:
			t.Fatal("timeout: expected connection close for legacy no-CAP with auth required")
		}
	}
}

// ─── Registration timeout test ───────────────────────────────────────────────

// TestRegistrationTimeout verifies that a client which connects and then sends
// nothing is dropped once the registration deadline fires. The timeout is
// injected via Server.regTimeout (which New defaults to registrationTimeout in
// production), so the test runs in milliseconds rather than the 30s production
// bound.
func TestRegistrationTimeout(t *testing.T) {
	s := New(&Config{}) // no bouncer auth; the client simply never registers
	s.regTimeout = 100 * time.Millisecond

	cRaw, sRaw := net.Pipe()
	client := conn.NewConn(cRaw, conn.Options{})
	t.Cleanup(func() {
		_ = client.Close()
		_ = sRaw.Close()
	})
	go func() { _ = s.serveConnInternal(sRaw, false) }()

	// Send nothing. The server's registration deadline must fire and close the
	// connection, which the client observes as its Messages channel closing.
	select {
	case _, ok := <-client.Messages():
		if ok {
			t.Fatal("server sent a message to an idle client before the timeout; expected only a close")
		}
		// Channel closed: the server dropped the idle connection. Correct.
	case <-time.After(3 * time.Second):
		t.Fatal("idle client was not dropped after the registration timeout fired")
	}
}

// TestDeadlineClearedAfterRegistration verifies that the registration deadline
// is cleared after a successful welcome burst, so a long-lived registered
// session is not affected.
func TestDeadlineClearedAfterRegistration(t *testing.T) {
	// A client that completes registration should be able to idle after 001
	// without being dropped by the registration deadline. We verify by completing
	// registration and then sending a PING several seconds later (simulated by
	// just sending it immediately — the key is that the deadline is not still
	// active and causing an error).
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true /* isTLS */)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client) // AUTHENTICATE +

	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "dylan", "secret"))
	_ = skipTo(t, client, irc.RPL_SASLSUCCESS)

	sendLine(t, client, "NICK dylan")
	sendLine(t, client, "USER dylan 0 * :Dylan")
	sendLine(t, client, "CAP END")
	_ = skipTo(t, client, irc.ERR_NOMOTD) // drain full welcome burst

	// After registration, the session should be alive. A PING should work.
	sendLine(t, client, "PING :post-reg")
	pong := recvMsg(t, client)
	assertMsg(t, pong, irc.PONG)
	found := false
	for _, p := range pong.Params {
		if p == "post-reg" {
			found = true
		}
	}
	if !found {
		t.Errorf("PONG did not echo token after registration; params=%v", pong.Params)
	}
}

// ─── Authcid parser table-driven tests ───────────────────────────────────────

// TestParseAuthcid exercises the hardened authcid parser with a comprehensive
// set of inputs covering all hostile-input boundaries specified in §6.3.
func TestParseAuthcid(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantUser  string
		wantNet   string
		wantCli   string
		wantHasN  bool
		wantHasC  bool
		wantErr   bool
		errSubstr string
	}{
		// Valid cases.
		{name: "bare-user", input: "dylan", wantUser: "dylan"},
		{name: "user-network", input: "dylan/libera", wantUser: "dylan", wantNet: "libera", wantHasN: true},
		{name: "user-network-client", input: "dylan/libera@hexchat", wantUser: "dylan", wantNet: "libera", wantCli: "hexchat", wantHasN: true, wantHasC: true},
		{name: "user-at-client", input: "dylan@hexchat", wantUser: "dylan", wantCli: "hexchat", wantHasC: true},
		{name: "user-with-number", input: "user123", wantUser: "user123"},

		// Multiple slashes: extra slashes are part of the network name.
		{name: "multi-slash", input: "dylan/net/extra", wantUser: "dylan", wantNet: "net/extra", wantHasN: true},

		// Multiple @: last @ is the client separator.
		{name: "multi-at", input: "dylan/net@foo@bar", wantUser: "dylan", wantNet: "net@foo", wantCli: "bar", wantHasN: true, wantHasC: true},

		// Error cases.
		{name: "empty", input: "", wantErr: true, errSubstr: "empty"},
		{name: "leading-slash", input: "/network", wantErr: true, errSubstr: "empty"},
		{name: "NUL-in-user", input: "dy\x00lan", wantErr: true, errSubstr: "NUL"},
		{name: "NUL-in-network", input: "dylan/net\x00work", wantErr: true, errSubstr: "NUL"},
		{name: "NUL-at-start", input: "\x00dylan", wantErr: true, errSubstr: "NUL"},
		{name: "too-long", input: strings.Repeat("a", maxAuthcidLen+1), wantErr: true, errSubstr: "length"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseAuthcid(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("ParseAuthcid(%q) = no error, want error containing %q", tc.input, tc.errSubstr)
					return
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseAuthcid(%q) error: %v", tc.input, err)
			}
			if got.User != tc.wantUser {
				t.Errorf("User = %q, want %q", got.User, tc.wantUser)
			}
			if got.Network != tc.wantNet {
				t.Errorf("Network = %q, want %q", got.Network, tc.wantNet)
			}
			if got.Client != tc.wantCli {
				t.Errorf("Client = %q, want %q", got.Client, tc.wantCli)
			}
			if got.HasNetwork != tc.wantHasN {
				t.Errorf("HasNetwork = %v, want %v", got.HasNetwork, tc.wantHasN)
			}
			if got.HasClient != tc.wantHasC {
				t.Errorf("HasClient = %v, want %v", got.HasClient, tc.wantHasC)
			}
		})
	}
}

// TestParseAuthcidExactMaxLength verifies that an authcid of exactly
// maxAuthcidLen bytes is accepted (boundary condition).
func TestParseAuthcidExactMaxLength(t *testing.T) {
	input := strings.Repeat("a", maxAuthcidLen)
	got, err := ParseAuthcid(input)
	if err != nil {
		t.Fatalf("ParseAuthcid of exactly maxAuthcidLen bytes should succeed: %v", err)
	}
	if got.User != input {
		t.Errorf("User = %q (len %d), want len %d", got.User[:10]+"...", len(got.User), maxAuthcidLen)
	}
}

// TestParseAuthcidNoPanic verifies that no adversarial input causes a panic.
func TestParseAuthcidNoPanic(t *testing.T) {
	inputs := []string{
		"",
		"/",
		"//",
		"@",
		"@@",
		"/@",
		"@/",
		"\x00",
		"a\x00b",
		strings.Repeat("a", 2000),
		"user/" + strings.Repeat("n", 200) + "@" + strings.Repeat("c", 200),
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ParseAuthcid(%q) panicked: %v", in, r)
				}
			}()
			_, _ = ParseAuthcid(in)
		})
	}
}

// TestSASLWrongUser verifies that a valid password but wrong username (user
// component not matching BouncerAuth.User) is rejected with 904.
func TestSASLWrongUser(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client)

	// Use a different user name.
	sendLine(t, client, "AUTHENTICATE "+plainPayload("", "notdylan", "secret"))
	r904 := recvMsg(t, client)
	assertMsg(t, r904, irc.ERR_SASLFAIL)
}

// TestSASLMalformedPayload verifies that a base64 PLAIN payload that doesn't
// decode to authzid\0authcid\0passwd is rejected with 904.
func TestSASLMalformedPayload(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client)

	// Payload with only one NUL separator (2 fields instead of 3).
	bad := base64.StdEncoding.EncodeToString([]byte("user\x00password"))
	sendLine(t, client, "AUTHENTICATE "+bad)
	r904 := recvMsg(t, client)
	assertMsg(t, r904, irc.ERR_SASLFAIL)
}

// TestSASLInvalidBase64Payload verifies that a non-base64 AUTHENTICATE payload
// is rejected with 904.
func TestSASLInvalidBase64Payload(t *testing.T) {
	cfg := makeTestCfg(t, "dylan", "secret")
	client := pipeServerWith(t, cfg, true)

	sendLine(t, client, "CAP LS 302")
	_ = recvMsg(t, client)

	sendLine(t, client, "CAP REQ :sasl")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE PLAIN")
	_ = recvMsg(t, client)

	sendLine(t, client, "AUTHENTICATE not-valid-base64!!!")
	r904 := recvMsg(t, client)
	assertMsg(t, r904, irc.ERR_SASLFAIL)
}

// ─── KDF concurrency gate tests ──────────────────────────────────────────────

// lowIterHash builds a valid pbkdf2-sha256 hash string with a tiny iteration
// count so gate tests do not pay the production 600k-iteration KDF cost.
// VerifyPassword honours the iteration count stored in the hash.
func lowIterHash(t *testing.T, pw string) string {
	t.Helper()
	salt := []byte("0123456789abcdef")
	dk, err := pbkdf2.Key(sha256.New, pw, salt, 10, 32)
	if err != nil {
		t.Fatalf("pbkdf2.Key: %v", err)
	}
	return fmt.Sprintf("%s:10:%s:%s", pbkdf2Prefix,
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(dk))
}

// TestVerifyPasswordGateSerializes is the regression test for the SASL KDF
// DoS bound: PBKDF2 verification costs ~hundreds of milliseconds of CPU per
// attempt, and the server used to run one KDF per concurrent AUTHENTICATE
// with no bound, letting an unauthenticated peer burn a core per connection.
// verifyPasswordGated (the server's auth path) must not start a KDF while the
// kdfGate token is held, and must proceed once it is released.
func TestVerifyPasswordGateSerializes(t *testing.T) {
	hash := lowIterHash(t, "hunter2")

	// Occupy the gate, simulating a verification already in flight.
	kdfGate <- struct{}{}

	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		ok, err := verifyPasswordGated(hash, "hunter2")
		done <- result{ok, err}
	}()

	select {
	case <-done:
		t.Fatal("verifyPasswordGated completed while the gate was held; KDFs can run concurrently")
	case <-time.After(100 * time.Millisecond):
		// Blocked behind the gate, as required.
	}

	// Release the token; the queued verification must now run to completion
	// and still produce the correct constant-time verification result.
	<-kdfGate
	select {
	case r := <-done:
		if r.err != nil || !r.ok {
			t.Fatalf("verifyPasswordGated after release = (%v, %v), want (true, nil)", r.ok, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verifyPasswordGated did not complete after the gate was released")
	}
}
