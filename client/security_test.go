package client

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

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
