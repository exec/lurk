package client

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveServer runs the full registration + SASL + JOIN + PRIVMSG round-trip
// against a real IRCv3 server. It is opt-in: it skips cleanly unless the
// LURK_TEST_SERVER environment variable names a reachable server, so the default
// `go test ./...` stays hermetic and CI without LAN access still passes.
//
// Configuration (environment):
//
//	LURK_TEST_SERVER     host:port of the server (required to run; e.g. 10.0.0.116:6667)
//	LURK_TEST_TLS        "1"/"true" to connect with TLS (self-signed → verification is skipped)
//	LURK_TEST_NICK       nickname to use (default "lurktest")
//	LURK_TEST_SASL_USER  SASL authcid; when set (with PASS) SASL PLAIN is attempted
//	LURK_TEST_SASL_PASS  SASL password
//	LURK_TEST_CHANNEL    channel to join (default "#lurk")
//
// The test relies on the echo-message capability to observe its own PRIVMSG
// echoed back; Ergo advertises it. If the server does not enable echo-message
// the PRIVMSG-echo assertion is skipped (the rest still runs).
func TestLiveServer(t *testing.T) {
	server := os.Getenv("LURK_TEST_SERVER")
	if server == "" {
		t.Skip("LURK_TEST_SERVER not set; skipping live server smoke test")
	}

	nick := envOr("LURK_TEST_NICK", "lurktest")
	channel := envOr("LURK_TEST_CHANNEL", "#lurk")
	saslUser := os.Getenv("LURK_TEST_SASL_USER")
	saslPass := os.Getenv("LURK_TEST_SASL_PASS")

	cfg := Config{
		Nick:               nick,
		User:               "lurk",
		Realname:           "lurk live test",
		Server:             server,
		TLS:                envBoolTest("LURK_TEST_TLS"),
		InsecureSkipVerify: true, // self-signed cert on the test server
		// A focused, widely-supported cap set; echo-message lets us read our own
		// PRIVMSG back to confirm a full client->server->client round-trip.
		Caps: []string{"server-time", "message-tags", "multi-prefix", "echo-message", "extended-join"},
	}
	if saslUser != "" && saslPass != "" {
		cfg.SASL = SASLConfig{Mechanism: "PLAIN", Username: saslUser, Password: saslPass}
		// The lab server may be plaintext (LURK_TEST_TLS unset); the operator
		// running the live test is knowingly opting into insecure auth there.
		cfg.AllowInsecureAuth = true
	}

	c := New(cfg)

	// Collect inbound PRIVMSGs so we can spot our own echoed line.
	echoed := make(chan string, 16)
	c.HandleMessage(func(ev *Event) {
		select {
		case echoed <- ev.Text():
		default:
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect to %s: %v", server, err)
	}
	defer c.Close()

	t.Logf("registered as %q on network %q", c.Nick(), c.Network())

	// 005 should have populated the network name; ErgoTest is what the doc'd
	// server advertises, but don't hard-fail on a different deployment — just
	// require that *some* network was parsed.
	if !waitFor(func() bool { return c.Network() != "" }, 3*time.Second) {
		t.Errorf("no NETWORK token parsed from 005")
	} else {
		t.Logf("network = %q", c.Network())
	}

	// If SASL was configured, registration succeeding means it worked (a SASL
	// failure aborts Connect with an error, caught above).
	if cfg.SASL.enabled() {
		t.Logf("SASL PLAIN authentication succeeded as %q", saslUser)
	}

	// Join the channel and wait until the server confirms our membership (the
	// server echoes our JOIN; state tracking records it).
	if err := c.Join(channel); err != nil {
		t.Fatalf("Join %s: %v", channel, err)
	}
	if !waitFor(func() bool { return inChannel(c, channel) }, 10*time.Second) {
		t.Fatalf("did not see JOIN confirmation for %s; channels=%v", channel, c.Channels())
	}
	t.Logf("joined %s; members=%d", channel, len(c.Members(channel)))

	// Send a unique PRIVMSG and, if echo-message is active, read it back.
	marker := "lurk-live-test " + time.Now().Format("150405.000")
	if err := c.Privmsg(channel, marker); err != nil {
		t.Fatalf("Privmsg: %v", err)
	}
	if sawText(echoed, marker, 5*time.Second) {
		t.Logf("observed PRIVMSG echo: %q", marker)
	} else {
		// echo-message may not be enabled on this deployment; the send itself
		// succeeding is still a meaningful smoke signal.
		t.Logf("PRIVMSG echo not observed within timeout (echo-message may be disabled); send path still exercised")
	}

	// Clean shutdown via QUIT.
	if err := c.Quit("lurk live test done"); err != nil {
		t.Logf("Quit: %v", err)
	}
}

// envOr returns the environment value for key, or def if unset/empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envBoolTest reports whether the env var key is set to a truthy value.
func envBoolTest(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// inChannel reports whether the client currently tracks membership in channel.
func inChannel(c *Client, channel string) bool {
	for _, ch := range c.Channels() {
		if strings.EqualFold(ch, channel) {
			return true
		}
	}
	return false
}

// sawText drains ch until it sees want or the timeout elapses.
func sawText(ch <-chan string, want string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case got := <-ch:
			if strings.Contains(got, want) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
