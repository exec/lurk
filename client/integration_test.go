package client

import (
	"bufio"
	"context"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lurk/conn"
)

// mockServer is a scripted IRC server backed by one end of a net.Pipe. It reads
// client lines, lets the test assert on them, and writes server lines back. It
// is intentionally simple: the test drives the conversation step by step.
type mockServer struct {
	t    *testing.T
	c    net.Conn
	br   *bufio.Reader
	once sync.Once
}

func newMockServer(t *testing.T, serverSide net.Conn) *mockServer {
	return &mockServer{t: t, c: serverSide, br: bufio.NewReader(serverSide)}
}

// fail marks the test failed and aborts the current goroutine. Because the
// script runs in its own goroutine, it uses Errorf (goroutine-safe) plus a
// connection close (so the client side unblocks) rather than Fatalf (which is
// only legal on the test goroutine), then Goexits to stop the script.
func (s *mockServer) fail(format string, args ...any) {
	s.t.Helper()
	s.t.Errorf(format, args...)
	s.close()
	runtime.Goexit()
}

// readLine reads one CRLF-terminated client line (without the terminator),
// failing the test on timeout.
func (s *mockServer) readLine() string {
	s.t.Helper()
	_ = s.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := s.br.ReadString('\n')
	if err != nil {
		s.fail("mock server read: %v (got %q)", err, line)
	}
	return strings.TrimRight(line, "\r\n")
}

// expect reads the next client line and asserts it equals want.
func (s *mockServer) expect(want string) {
	s.t.Helper()
	got := s.readLine()
	if got != want {
		s.fail("client sent %q, want %q", got, want)
	}
}

// expectPrefix reads the next client line and asserts it starts with prefix,
// returning the full line for further inspection.
func (s *mockServer) expectPrefix(prefix string) string {
	s.t.Helper()
	got := s.readLine()
	if !strings.HasPrefix(got, prefix) {
		s.fail("client sent %q, want prefix %q", got, prefix)
	}
	return got
}

// send writes one or more server lines to the client, appending CRLF.
func (s *mockServer) send(lines ...string) {
	s.t.Helper()
	_ = s.c.SetWriteDeadline(time.Now().Add(2 * time.Second))
	for _, l := range lines {
		if _, err := s.c.Write([]byte(l + "\r\n")); err != nil {
			s.fail("mock server write %q: %v", l, err)
		}
	}
}

func (s *mockServer) close() { s.once.Do(func() { _ = s.c.Close() }) }

// TestFullHandshake drives the complete CAP LS / SASL PLAIN / CAP END / 001 /
// 005 registration handshake against the scripted mock server, then exercises a
// JOIN + NAMES round-trip to verify state tracking.
func TestFullHandshake(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{
		Nick:     "lurkbot",
		User:     "lurk",
		Realname: "Lurk Bot",
		SASL: SASLConfig{
			Mechanism: "PLAIN",
			Username:  "lurkbot",
			Password:  "hunter2",
		},
		// Request a small, deterministic cap set.
		Caps: []string{"sasl", "server-time", "multi-prefix"},
	})

	// Capture the semantic Connected event.
	connected := make(chan string, 1)
	c.HandleConnected(func(ev *Event) { connected <- ev.Text() })

	// Run the scripted server in a goroutine; the client's Connect blocks until
	// registration completes.
	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		runHandshakeScript(t, srv)
	}()
	// Always tear down and wait for the script goroutine so a failing assertion
	// can never log after the test returns.
	defer func() {
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.ConnectConn(ctx, tr); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}

	select {
	case welcome := <-connected:
		if !strings.Contains(welcome, "Welcome") {
			t.Errorf("Connected event text = %q, want it to contain \"Welcome\"", welcome)
		}
	case <-time.After(time.Second):
		t.Fatal("Connected event not fired")
	}

	// Post-registration assertions on parsed server features and self nick.
	if got := c.Nick(); got != "lurkbot" {
		t.Errorf("Nick() = %q, want lurkbot", got)
	}
	// 005 lines are processed after 001, so poll for the accumulated network.
	if !waitFor(func() bool { return c.Network() == "TestNet" }, time.Second) {
		t.Errorf("Network() = %q, want TestNet", c.Network())
	}

	// Drive a JOIN + NAMES exchange and assert membership tracking.
	if err := c.Join("#lurk"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	runJoinScript(t, srv)

	// Wait for the membership state to settle (the run loop processes 353/366
	// asynchronously). Poll briefly.
	if !waitFor(func() bool { return len(c.Members("#lurk")) == 3 }, time.Second) {
		t.Fatalf("expected 3 members in #lurk, got %d: %+v", len(c.Members("#lurk")), c.Members("#lurk"))
	}

	members := membersByNick(c.Members("#lurk"))
	if m, ok := members["lurkbot"]; !ok {
		t.Error("self not tracked as a member of #lurk")
	} else if m.Prefixes != "@" {
		t.Errorf("lurkbot prefixes = %q, want @", m.Prefixes)
	}
	if m, ok := members["alice"]; !ok || m.Prefixes != "+" {
		t.Errorf("alice = %+v, want prefix +", m)
	}
	if _, ok := members["bob"]; !ok {
		t.Error("bob not tracked")
	}

	// A NICK change should follow the member across the channel.
	srv.send(":alice!a@host NICK :alice2")
	if !waitFor(func() bool {
		ms := membersByNick(c.Members("#lurk"))
		_, gone := ms["alice"]
		_, here := ms["alice2"]
		return !gone && here
	}, time.Second) {
		t.Errorf("NICK change not tracked: %+v", c.Members("#lurk"))
	}

	// A PART by another member should drop them.
	srv.send(":bob!b@host PART #lurk :bye")
	if !waitFor(func() bool {
		_, ok := membersByNick(c.Members("#lurk"))["bob"]
		return !ok
	}, time.Second) {
		t.Errorf("PART not tracked: %+v", c.Members("#lurk"))
	}

	// Server PING must be answered with PONG carrying the same token. The token
	// has no spaces, so it serializes as a plain (non-trailing) parameter.
	srv.send("PING :keepalive")
	srv.expect("PONG keepalive")
}

// runHandshakeScript plays the server side of the registration handshake.
func runHandshakeScript(t *testing.T, srv *mockServer) {
	t.Helper()

	// The client opens with CAP LS 302, then PASS(none)/NICK/USER. Order: the
	// client sends CAP LS first, then NICK, then USER (no PASS configured).
	srv.expect("CAP LS 302")
	srv.expect("NICK lurkbot")
	srv.expect("USER lurk 0 * :Lurk Bot")

	// Advertise caps including sasl with a mechanism list.
	srv.send("CAP * LS :sasl=PLAIN,EXTERNAL server-time multi-prefix away-notify")

	// Client requests the intersection (sasl server-time multi-prefix), in the
	// wanted order it was configured with.
	req := srv.expectPrefix("CAP REQ :")
	reqd := strings.TrimPrefix(req, "CAP REQ :")
	for _, want := range []string{"sasl", "server-time", "multi-prefix"} {
		if !strings.Contains(reqd, want) {
			t.Errorf("CAP REQ %q missing %q", reqd, want)
		}
	}
	srv.send("CAP * ACK :" + reqd)

	// SASL PLAIN: client sends AUTHENTICATE PLAIN, server prompts with '+',
	// client sends the base64 payload.
	srv.expect("AUTHENTICATE PLAIN")
	srv.send("AUTHENTICATE +")
	payload := srv.expectPrefix("AUTHENTICATE ")
	// base64("\0lurkbot\0hunter2")
	if got := strings.TrimPrefix(payload, "AUTHENTICATE "); got != "AGx1cmtib3QAaHVudGVyMg==" {
		t.Errorf("SASL payload = %q, want base64 of \\0lurkbot\\0hunter2", got)
	}
	srv.send(
		":server 900 lurkbot lurkbot!lurk@host lurkbot :You are now logged in as lurkbot",
		":server 903 lurkbot :SASL authentication successful",
	)

	// After SASL succeeds the client sends CAP END.
	srv.expect("CAP END")

	// Registration burst: 001 welcome and 005 isupport (two lines to exercise
	// accumulation), then end of MOTD.
	srv.send(
		":server 001 lurkbot :Welcome to the TestNet IRC Network lurkbot",
		":server 005 lurkbot NETWORK=TestNet PREFIX=(ov)@+ CHANTYPES=# :are supported by this server",
		":server 005 lurkbot CASEMAPPING=rfc1459 NICKLEN=30 :are supported by this server",
		":server 376 lurkbot :End of /MOTD command.",
	)
}

// runJoinScript plays the server side of a JOIN + NAMES exchange for #lurk.
func runJoinScript(t *testing.T, srv *mockServer) {
	t.Helper()
	srv.expect("JOIN #lurk")
	srv.send(
		":lurkbot!lurk@host JOIN #lurk",
		":server 353 lurkbot = #lurk :@lurkbot +alice bob",
		":server 366 lurkbot #lurk :End of /NAMES list.",
	)
}

// waitFor polls cond until it returns true or the timeout elapses.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// membersByNick indexes a member snapshot by nick for assertions.
func membersByNick(ms []Member) map[string]Member {
	out := make(map[string]Member, len(ms))
	for _, m := range ms {
		out[m.Nick] = m
	}
	return out
}

// TestRegistrationNoSASL drives a plain registration (no SASL configured) where
// the server offers no caps the client wants, so negotiation finishes with an
// immediate CAP END. It also exercises the 433 nick-in-use fallback.
func TestRegistrationNoSASL(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{
		Nick:         "taken",
		User:         "u",
		Realname:     "Real Name",
		FallbackNick: "backup",
		// Want a cap the server won't offer, so the intersection is empty.
		Caps: []string{"some-unoffered-cap"},
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)

		srv.expect("CAP LS 302")
		srv.expect("NICK taken")
		srv.expect("USER u 0 * :Real Name")

		// Server rejects the desired nick; client must try the fallback.
		srv.send(":server 433 * taken :Nickname is already in use")
		srv.expect("NICK backup")

		// No wanted caps are offered, so the negotiator finishes immediately.
		srv.send("CAP * LS :away-notify chghost")
		srv.expect("CAP END")

		srv.send(
			":server 001 backup :Welcome to the Net backup",
			":server 005 backup NETWORK=Net :are supported by this server",
			":server 376 backup :End of MOTD.",
		)
	}()
	defer func() {
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.ConnectConn(ctx, tr); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}
	if got := c.Nick(); got != "backup" {
		t.Errorf("Nick() = %q, want backup after 433 fallback", got)
	}
	// 005 arrives after 001, so the network is populated shortly after Connect
	// returns; poll for it.
	if !waitFor(func() bool { return c.Network() == "Net" }, time.Second) {
		t.Errorf("Network() = %q, want Net", c.Network())
	}
}

// TestSASLExternal exercises the EXTERNAL mechanism, whose initial response is
// empty and is therefore sent on the wire as "AUTHENTICATE +".
func TestSASLExternal(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{
		Nick:     "certuser",
		User:     "cert",
		Realname: "Cert User",
		SASL:     SASLConfig{Mechanism: "EXTERNAL"},
		Caps:     []string{"sasl"},
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)

		srv.expect("CAP LS 302")
		srv.expect("NICK certuser")
		srv.expect("USER cert 0 * :Cert User")
		srv.send("CAP * LS :sasl=EXTERNAL")
		srv.expect("CAP REQ :sasl")
		srv.send("CAP * ACK :sasl")

		srv.expect("AUTHENTICATE EXTERNAL")
		srv.send("AUTHENTICATE +")
		// Empty initial response -> "AUTHENTICATE +".
		srv.expect("AUTHENTICATE +")
		srv.send(":server 903 certuser :SASL authentication successful")

		srv.expect("CAP END")
		srv.send(":server 001 certuser :Welcome certuser")
	}()
	defer func() {
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ConnectConn(ctx, tr); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}
	if got := c.Nick(); got != "certuser" {
		t.Errorf("Nick() = %q, want certuser", got)
	}
}

// TestSASLFailureAbortsRegistration verifies that a SASL failure numeric (904)
// surfaces as a registration error rather than connecting unauthenticated.
func TestSASLFailureAbortsRegistration(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{
		Nick: "bot",
		SASL: SASLConfig{Mechanism: "PLAIN", Username: "bot", Password: "wrong"},
		Caps: []string{"sasl"},
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		srv.expect("CAP LS 302")
		srv.expect("NICK bot")
		// Realname "bot" has no space, so it serializes as a plain final param.
		srv.expect("USER bot 0 * bot")
		srv.send("CAP * LS :sasl=PLAIN")
		srv.expect("CAP REQ :sasl")
		srv.send("CAP * ACK :sasl")
		srv.expect("AUTHENTICATE PLAIN")
		srv.send("AUTHENTICATE +")
		srv.expectPrefix("AUTHENTICATE ")
		srv.send(":server 904 bot :SASL authentication failed")
	}()
	defer func() {
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.ConnectConn(ctx, tr)
	if err == nil {
		t.Fatal("expected registration to fail on SASL 904, got nil error")
	}
	if !strings.Contains(err.Error(), "904") && !strings.Contains(strings.ToLower(err.Error()), "sasl") {
		t.Errorf("error = %v, want it to mention the SASL failure", err)
	}
}
