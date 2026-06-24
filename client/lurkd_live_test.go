package client

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLiveLurkdFanout exercises lurkd's upstream→client fan-out end to end with
// the bouncer in the middle: two clients bind to the same network on a running
// lurkd, a third client connected directly to the upstream Ergo sends a message
// to the shared channel, and the test asserts BOTH bound clients receive it.
// This is the live counterpart to the hermetic server/fanout_test.go and the
// path the "serialize once, TrySend to each session" change optimises.
//
// It is opt-in and skips unless both the upstream and the bouncer are named:
//
//	LURK_TEST_SERVER   host:port of the upstream Ergo (the sender connects here)
//	LURKD_TEST_ADDR    host:port of the running lurkd TLS listener (bound clients)
//	LURKD_TEST_NETID   bouncer netid to BIND to (default 1)
//	LURKD_TEST_CHANNEL channel the upstream auto-joined (default "#fan")
func TestLiveLurkdFanout(t *testing.T) {
	ergo := os.Getenv("LURK_TEST_SERVER")
	lurkd := os.Getenv("LURKD_TEST_ADDR")
	if ergo == "" || lurkd == "" {
		t.Skip("LURK_TEST_SERVER and LURKD_TEST_ADDR not both set; skipping lurkd fan-out live test")
	}
	netid := 1
	if v := os.Getenv("LURKD_TEST_NETID"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			netid = n
		}
	}
	channel := envOr("LURKD_TEST_CHANNEL", "#fan")

	// boundClient connects to lurkd and BINDs to netid, returning the client and
	// a buffered funnel of the PRIVMSGs it receives.
	boundClient := func(nick string) (*Client, <-chan *Event) {
		cfg := Config{
			Nick:               nick,
			User:               nick,
			Realname:           "lurkd fan-out test",
			Server:             lurkd,
			TLS:                true,
			InsecureSkipVerify: true, // lurkd's self-signed cert
			BouncerNetID:       netid,
			Caps:               []string{"server-time", "message-tags", "batch", "soju.im/bouncer-networks"},
		}
		c := New(cfg)
		got := make(chan *Event, 64)
		c.HandleMessage(func(ev *Event) {
			select {
			case got <- ev:
			default:
			}
		})
		return c, got
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two clients bound to the same network through lurkd.
	deviceA, aMsgs := boundClient("deviceA")
	if err := deviceA.Connect(ctx); err != nil {
		t.Fatalf("deviceA bind-connect to lurkd: %v", err)
	}
	defer deviceA.Close()
	deviceB, bMsgs := boundClient("deviceB")
	if err := deviceB.Connect(ctx); err != nil {
		t.Fatalf("deviceB bind-connect to lurkd: %v", err)
	}
	defer deviceB.Close()
	t.Logf("both devices bound to netid %d via lurkd %s", netid, lurkd)

	// The bind state-burst should hand each bound client the upstream's channel
	// membership; wait for both to track it before sending.
	if !waitFor(func() bool { return inChannel(deviceA, channel) && inChannel(deviceB, channel) }, 10*time.Second) {
		t.Logf("warning: bound clients did not both report %s membership (A=%v B=%v); proceeding anyway",
			channel, deviceA.Channels(), deviceB.Channels())
	}

	// A third client connected DIRECTLY to the upstream Ergo is the sender, so
	// the message arrives at lurkd's upstream as a normal inbound PRIVMSG and is
	// fanned out — unambiguously exercising the broadcast path, not an echo.
	sender := New(Config{
		Nick:               "sender",
		User:               "sender",
		Realname:           "fan-out sender",
		Server:             ergo,
		InsecureSkipVerify: true,
		Caps:               []string{"server-time", "message-tags"},
	})
	if err := sender.Connect(ctx); err != nil {
		t.Fatalf("sender connect to ergo: %v", err)
	}
	defer sender.Close()
	if err := sender.Join(channel); err != nil {
		t.Fatalf("sender Join %s: %v", channel, err)
	}
	if !waitFor(func() bool { return inChannel(sender, channel) }, 10*time.Second) {
		t.Fatalf("sender did not join %s", channel)
	}

	marker := "lurkd-fanout " + time.Now().Format("150405.000")
	if err := sender.Privmsg(channel, marker); err != nil {
		t.Fatalf("sender Privmsg: %v", err)
	}

	// Both bound clients must receive the fanned-out message.
	if !sawPrivmsg(aMsgs, channel, marker, 8*time.Second) {
		t.Fatalf("deviceA did not receive fanned-out message %q", marker)
	}
	t.Logf("deviceA received fanned-out message: %q", marker)
	if !sawPrivmsg(bMsgs, channel, marker, 8*time.Second) {
		t.Fatalf("deviceB did not receive fanned-out message %q", marker)
	}
	t.Logf("deviceB received fanned-out message: %q", marker)

	_ = sender.Quit("done")
}
