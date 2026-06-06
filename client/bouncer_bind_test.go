package client

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
)

// bindClient wires a client.Client to a scripted mock over net.Pipe and returns
// both, plus a channel closed when the script goroutine finishes.
func bindClient(t *testing.T, cfg Config, script func(srv *mockServer)) (*Client, chan struct{}) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(cfg)
	done := make(chan struct{})
	go func() {
		defer close(done)
		script(srv)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.ConnectConn(ctx, tr); err != nil {
		t.Fatalf("ConnectConn: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, done
}

// TestBouncerBindAfterSASLBeforeCapEnd is the [B#15] bind-point test: with
// Config.BouncerNetID set and the soju.im/bouncer-networks capability negotiated,
// the client sends "BOUNCER BIND <netid>" after SASL succeeds and immediately
// before CAP END.
func TestBouncerBindAfterSASLBeforeCapEnd(t *testing.T) {
	cfg := Config{
		Nick: "lurkbot", User: "lurk", Realname: "Lurk Bot",
		SASL:         SASLConfig{Mechanism: "PLAIN", Username: "lurkbot", Password: "hunter2"},
		BouncerNetID: 7,
		Caps:         []string{}, // withDefaults adds sasl + soju.im/bouncer-networks
	}
	c, done := bindClient(t, cfg, func(srv *mockServer) {
		srv.expect("CAP LS 302")
		srv.expect("NICK lurkbot")
		srv.expect("USER lurk 0 * :Lurk Bot")
		srv.send("CAP * LS :sasl=PLAIN soju.im/bouncer-networks")
		req := srv.expectPrefix("CAP REQ :")
		reqd := strings.TrimPrefix(req, "CAP REQ :")
		for _, want := range []string{"sasl", "soju.im/bouncer-networks"} {
			if !strings.Contains(reqd, want) {
				srv.fail("CAP REQ %q missing %q", reqd, want)
			}
		}
		srv.send("CAP * ACK :" + reqd)
		srv.expect("AUTHENTICATE PLAIN")
		srv.send("AUTHENTICATE +")
		srv.expectPrefix("AUTHENTICATE ")
		srv.send(
			":server 900 lurkbot lurkbot!lurk@host lurkbot :logged in",
			":server 903 lurkbot :SASL ok",
		)
		// The assertion: BIND is sent BEFORE CAP END. If the client sent CAP END
		// first (or nothing), these exact-match expects fail.
		srv.expect("BOUNCER BIND 7")
		srv.expect("CAP END")
		srv.send(":server 001 lurkbot :Welcome")
	})
	<-done
	if c.Nick() != "lurkbot" {
		t.Errorf("nick = %q, want lurkbot", c.Nick())
	}
}

// TestBouncerBindNoSASL verifies the bind also fires before CAP END on the
// no-SASL registration path (the handleCAP send path, not finishSASL).
func TestBouncerBindNoSASL(t *testing.T) {
	cfg := Config{
		Nick: "lurkbot", User: "lurk", Realname: "Lurk Bot",
		BouncerNetID: 3,
		Caps:         []string{}, // withDefaults adds soju.im/bouncer-networks
	}
	_, done := bindClient(t, cfg, func(srv *mockServer) {
		srv.expect("CAP LS 302")
		srv.expect("NICK lurkbot")
		srv.expect("USER lurk 0 * :Lurk Bot")
		srv.send("CAP * LS :soju.im/bouncer-networks")
		req := srv.expectPrefix("CAP REQ :")
		reqd := strings.TrimPrefix(req, "CAP REQ :")
		if !strings.Contains(reqd, "soju.im/bouncer-networks") {
			srv.fail("CAP REQ %q missing soju.im/bouncer-networks", reqd)
		}
		srv.send("CAP * ACK :" + reqd)
		srv.expect("BOUNCER BIND 3")
		srv.expect("CAP END")
		srv.send(":server 001 lurkbot :Welcome")
	})
	<-done
}

// TestBouncerBindSuppressedWithoutCap verifies the bind is NOT sent when the
// server does not offer soju.im/bouncer-networks, even though BouncerNetID is
// set — so a bounce-configured profile pointed at a plain server sends nothing
// spurious. The script expects CAP END directly after the ACK; a stray BOUNCER
// BIND would make the exact-match expect fail.
func TestBouncerBindSuppressedWithoutCap(t *testing.T) {
	cfg := Config{
		Nick: "lurkbot", User: "lurk", Realname: "Lurk Bot",
		BouncerNetID: 9,
		Caps:         []string{"server-time"}, // no bouncer-networks offered below
	}
	_, done := bindClient(t, cfg, func(srv *mockServer) {
		srv.expect("CAP LS 302")
		srv.expect("NICK lurkbot")
		srv.expect("USER lurk 0 * :Lurk Bot")
		// Server offers server-time only (NOT soju.im/bouncer-networks).
		srv.send("CAP * LS :server-time")
		req := srv.expectPrefix("CAP REQ :")
		srv.send("CAP * ACK :" + strings.TrimPrefix(req, "CAP REQ :"))
		// No BIND must precede CAP END.
		srv.expect("CAP END")
		srv.send(":server 001 lurkbot :Welcome")
	})
	<-done
}
