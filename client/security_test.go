package client

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// mustParse parses a wire line into a Message, failing the test on error.
func mustParse(t *testing.T, line string) *irc.Message {
	t.Helper()
	m, err := irc.Parse(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	return m
}

// TestConnectRefusesCleartextCredentials checks that Connect refuses to dial when
// it would send SASL PLAIN or a PASS password over a plaintext (non-TLS) link,
// and that AllowInsecureAuth / EXTERNAL are not blocked by the guard. The guard
// fires before any dial, so these cases need no server.
func TestConnectRefusesCleartextCredentials(t *testing.T) {
	t.Run("plain over plaintext", func(t *testing.T) {
		c := New(Config{
			Nick: "me", Server: "irc.example.org:6667",
			SASL: SASLConfig{Mechanism: "PLAIN", Username: "me", Password: "secret"},
		})
		err := c.Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), "plaintext") {
			t.Fatalf("Connect err = %v, want a plaintext refusal", err)
		}
	})

	t.Run("pass over plaintext", func(t *testing.T) {
		c := New(Config{Nick: "me", Server: "irc.example.org:6667", Pass: "hunter2"})
		err := c.Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), "plaintext") {
			t.Fatalf("Connect err = %v, want a plaintext refusal", err)
		}
	})

	t.Run("external over plaintext is not blocked by the guard", func(t *testing.T) {
		// EXTERNAL carries no secret, so the credential guard must not trigger;
		// the call then fails at dial time instead (with a non-plaintext error).
		// A bounded context keeps the test from hanging if the dial unexpectedly
		// connects.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		c := New(Config{
			Nick: "me", Server: "127.0.0.1:1", // port 1: dial is refused fast
			SASL: SASLConfig{Mechanism: "EXTERNAL"},
		})
		err := c.Connect(ctx)
		if err != nil && strings.Contains(err.Error(), "plaintext") {
			t.Fatalf("EXTERNAL wrongly blocked by credential guard: %v", err)
		}
	})
}

// TestNickInUseFloodFailsRegistration verifies that a hostile server flooding
// ERR_NICKNAMEINUSE (433) during registration causes the client to fail
// registration rather than appending underscores to the nick indefinitely.
// After nickRetryMax collisions ConnectConn must return a non-nil error, and
// the final attempted nick must not have grown past a reasonable bound.
func TestNickInUseFloodFailsRegistration(t *testing.T) {
	const startNick = "bot"
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	tr := conn.NewConn(clientSide, conn.Options{})

	c := New(Config{Nick: startNick, User: "u", Realname: "r", Caps: []string{}})

	// down is set once the test enters teardown (or the flood loop ends) so that
	// the server-side goroutine does not fail the test when failRegistration closes
	// the client connection mid-read.
	var down atomic.Bool
	srv.down = &down

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		srv.expect("CAP LS 302")
		srv.expect("NICK " + startNick)
		srv.expect("USER u 0 * r")
		srv.send("CAP * LS :")
		srv.expectPrefix("CAP END")

		// Flood more than nickRetryMax consecutive 433s; never send 001.
		// Mark the server as shutting down before the flood so that when
		// failRegistration closes the client connection mid-read, the pipe error
		// in readLine is treated as a silent exit rather than a test failure.
		down.Store(true)
		for i := 0; i < nickRetryMax+5; i++ {
			line := srv.readLine() // exits silently on pipe close (srv.down is set)
			nick := strings.TrimPrefix(line, "NICK ")
			srv.send(":server 433 * " + nick + " :Nickname is already in use")
		}
	}()

	defer func() {
		down.Store(true)
		c.Close()
		srv.close()
		<-scriptDone
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := c.ConnectConn(ctx, tr)
	if err == nil {
		t.Fatal("ConnectConn succeeded after a 433 flood, want an error")
	}

	// The final self-nick must not have grown without bound. With nickRetryMax=10
	// and a 3-char starting nick, the worst-case appended length is startNick +
	// nickRetryMax underscores. Anything significantly beyond that indicates the
	// cap did not fire.
	finalNick := c.Nick()
	maxAllowed := len(startNick) + nickRetryMax + 5 // a little headroom
	if len(finalNick) > maxAllowed {
		t.Errorf("nick grew to %d chars (%q) — cap did not fire (max allowed %d)", len(finalNick), finalNick, maxAllowed)
	}
}

// TestTrackTopicIgnoresUnknownChannel verifies that TOPIC, RPL_TOPIC (332), and
// RPL_TOPICWHOTIME (333) messages for channels the client has not joined are
// silently ignored and do not fabricate phantom channel state. A hostile server
// sending a flood of topic messages for arbitrary channel names must not be able
// to grow the channel map without bound below the TUI-layer buffer cap.
func TestTrackTopicIgnoresUnknownChannel(t *testing.T) {
	c := New(Config{Nick: "me"})
	c.mu.Lock()
	c.st.self = "me"
	c.mu.Unlock()

	const phantomCount = 1000

	// Flood TOPIC, RPL_TOPIC (332), and RPL_TOPICWHOTIME (333) for channels the
	// client has never joined. None should create channel state.
	for i := 0; i < phantomCount; i++ {
		ch := "#phantom" + strconv.Itoa(i)
		c.track(mustParse(t, ":server!s@s TOPIC "+ch+" :hostile topic"))
		c.track(mustParse(t, ":server 332 me "+ch+" :hostile topic"))
		c.track(mustParse(t, ":server 333 me "+ch+" setter 1700000000"))
	}

	if got := c.Channels(); len(got) != 0 {
		t.Fatalf("topic messages for unjoined channels created %d phantom channel(s): %v", len(got), got)
	}

	// After the client joins a channel, TOPIC and 332/333 for that channel must
	// still be applied correctly.
	c.track(mustParse(t, ":me!u@h JOIN #real"))
	c.track(mustParse(t, ":server!s@s TOPIC #real :welcome"))
	c.track(mustParse(t, ":server 332 me #real :welcome"))
	c.track(mustParse(t, ":server 333 me #real alice 1700000000"))

	text, setBy, at := c.Topic("#real")
	if text != "welcome" {
		t.Errorf("topic text = %q, want welcome", text)
	}
	if setBy != "alice" {
		t.Errorf("topic setBy = %q, want alice", setBy)
	}
	if at.IsZero() {
		t.Errorf("topic at is zero, want non-zero")
	}
}

// TestTrackJoinIgnoresForeignChannel verifies that a JOIN from another user for a
// channel the client is not in does not fabricate channel state (which a hostile
// server could otherwise use to grow the channel map without bound), while a
// foreign JOIN to a channel we are in is still tracked as a member.
func TestTrackJoinIgnoresForeignChannel(t *testing.T) {
	c := New(Config{Nick: "me"})

	// Foreign JOIN for an unknown channel: ignored, no phantom state.
	c.track(mustParse(t, ":other!u@h JOIN #nope"))
	if got := c.Channels(); len(got) != 0 {
		t.Fatalf("foreign JOIN created phantom channels: %v", got)
	}

	// Our own JOIN creates the channel; a later foreign JOIN adds a member.
	c.track(mustParse(t, ":me!u@h JOIN #real"))
	c.track(mustParse(t, ":other!u@h JOIN #real"))
	if got := c.Channels(); len(got) != 1 || got[0] != "#real" {
		t.Fatalf("channels = %v, want [#real]", got)
	}
	var foundOther bool
	for _, m := range c.Members("#real") {
		if m.Nick == "other" {
			foundOther = true
		}
	}
	if !foundOther {
		t.Errorf("foreign joiner not tracked as a member of a joined channel")
	}
}
