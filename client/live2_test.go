package client

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveTwoClients connects two independent clients to a real IRCv3 server,
// joins both to the same channel, and verifies a message sent by one is
// delivered to the other (and that the sender sees its own echo). Like
// TestLiveServer it is opt-in via LURK_TEST_SERVER, so the default `go test`
// stays hermetic.
//
//	LURK_TEST_SERVER   host:port (required; e.g. 127.0.0.1:6667)
//	LURK_TEST_TLS      "1"/"true" to use TLS (self-signed → verification skipped)
//	LURK_TEST_CHANNEL  channel to join (default "#lurk2")
func TestLiveTwoClients(t *testing.T) {
	server := os.Getenv("LURK_TEST_SERVER")
	if server == "" {
		t.Skip("LURK_TEST_SERVER not set; skipping two-client live test")
	}
	channel := envOr("LURK_TEST_CHANNEL", "#lurk2")
	tls := envBoolTest("LURK_TEST_TLS")

	newClient := func(nick string) (*Client, <-chan *Event) {
		cfg := Config{
			Nick:               nick,
			User:               "lurk",
			Realname:           "lurk two-client live test",
			Server:             server,
			TLS:                tls,
			InsecureSkipVerify: true,
			// echo-message lets the sender observe its own line; server-time and
			// message-tags exercise the tag parse/serialize hot path.
			Caps: []string{"server-time", "message-tags", "multi-prefix", "echo-message", "extended-join"},
		}
		c := New(cfg)
		// A per-client PRIVMSG funnel. Buffered so the read loop never blocks.
		got := make(chan *Event, 64)
		c.HandleMessage(func(ev *Event) {
			select {
			case got <- ev:
			default:
			}
		})
		return c, got
	}

	alice, aliceMsgs := newClient("alice")
	bob, bobMsgs := newClient("bob")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := alice.Connect(ctx); err != nil {
		t.Fatalf("alice Connect: %v", err)
	}
	defer alice.Close()
	if err := bob.Connect(ctx); err != nil {
		t.Fatalf("bob Connect: %v", err)
	}
	defer bob.Close()
	t.Logf("connected: alice=%q bob=%q on %q", alice.Nick(), bob.Nick(), alice.Network())

	// Join both to the channel and wait until each tracks its own membership.
	for _, c := range []*Client{alice, bob} {
		if err := c.Join(channel); err != nil {
			t.Fatalf("%s Join: %v", c.Nick(), err)
		}
	}
	if !waitFor(func() bool { return inChannel(alice, channel) && inChannel(bob, channel) }, 10*time.Second) {
		t.Fatalf("clients did not both join %s", channel)
	}

	// Wait until alice sees bob as a co-member, so her PRIVMSG can't race ahead
	// of bob's JOIN being processed by the server's channel membership.
	if !waitFor(func() bool { return memberPresent(alice, channel, "bob") }, 10*time.Second) {
		t.Logf("warning: alice does not yet see bob in %s members=%v", channel, memberNicks(alice, channel))
	}

	// Alice sends; bob should receive it, and alice should see her own echo.
	marker := "two-client-live " + time.Now().Format("150405.000")
	if err := alice.Privmsg(channel, marker); err != nil {
		t.Fatalf("alice Privmsg: %v", err)
	}

	if !sawPrivmsg(bobMsgs, channel, marker, 5*time.Second) {
		t.Fatalf("bob did not receive alice's message %q", marker)
	}
	t.Logf("bob received alice's message: %q", marker)

	if sawPrivmsg(aliceMsgs, channel, marker, 5*time.Second) {
		t.Logf("alice observed her own echo")
	} else {
		t.Logf("alice echo not observed (echo-message may be disabled)")
	}

	// Reply in the other direction to confirm full-duplex delivery.
	reply := "two-client-reply " + time.Now().Format("150405.000")
	if err := bob.Privmsg(channel, reply); err != nil {
		t.Fatalf("bob Privmsg: %v", err)
	}
	if !sawPrivmsg(aliceMsgs, channel, reply, 5*time.Second) {
		t.Fatalf("alice did not receive bob's reply %q", reply)
	}
	t.Logf("alice received bob's reply: %q", reply)

	_ = alice.Quit("done")
	_ = bob.Quit("done")
}

// memberNicks returns the nicks alice tracks in channel (for diagnostics).
func memberNicks(c *Client, channel string) []string {
	var out []string
	for _, m := range c.Members(channel) {
		out = append(out, m.Nick)
	}
	return out
}

// memberPresent reports whether c tracks a member named nick in channel.
func memberPresent(c *Client, channel, nick string) bool {
	for _, m := range c.Members(channel) {
		if m.Nick == nick {
			return true
		}
	}
	return false
}

// sawPrivmsg drains ch until it sees a PRIVMSG to target whose text contains
// want, or the timeout elapses.
func sawPrivmsg(ch <-chan *Event, target, want string, timeout time.Duration) bool {
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if ev.Command() == "PRIVMSG" && ev.Param(0) == target &&
				strings.Contains(ev.Text(), want) {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
