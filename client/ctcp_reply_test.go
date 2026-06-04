package client

import (
	"strings"
	"testing"

	"lurk/irc"
)

// ctcpClient builds a registered-enough client wired to a record transport so a
// CTCP reply can be captured, with self nick "me".
func ctcpClient(t *testing.T) (*Client, *recordTransport) {
	t.Helper()
	c := &Client{st: newState(), disp: newDispatcher(), cfg: Config{Version: "lurk-test"}}
	c.st.self = "me"
	rec := newRecordTransport()
	c.tr = rec
	return c, rec
}

func feedPrivmsg(t *testing.T, c *Client, line string) {
	t.Helper()
	m, err := irc.Parse(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	c.maybeAnswerCTCP(m)
}

func TestCTCPReplies(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{"version", ":bob!b@h PRIVMSG me :\x01VERSION\x01", "NOTICE bob :\x01VERSION lurk-test\x01"},
		{"ping", ":bob!b@h PRIVMSG me :\x01PING 12345\x01", "NOTICE bob :\x01PING 12345\x01"},
		{"clientinfo", ":bob!b@h PRIVMSG me :\x01CLIENTINFO\x01", "NOTICE bob :\x01CLIENTINFO " + ctcpSupported + "\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := ctcpClient(t)
			feedPrivmsg(t, c, tc.line)
			if len(rec.sent) != 1 || rec.sent[0] != tc.want {
				t.Errorf("CTCP reply = %q, want %q", rec.sent, tc.want)
			}
		})
	}
}

func TestCTCPTimeReplies(t *testing.T) {
	c, rec := ctcpClient(t)
	feedPrivmsg(t, c, ":bob!b@h PRIVMSG me :\x01TIME\x01")
	if len(rec.sent) != 1 || !strings.HasPrefix(rec.sent[0], "NOTICE bob :\x01TIME ") {
		t.Errorf("TIME reply = %q", rec.sent)
	}
}

func TestCTCPIgnoredCases(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"action is not a query", ":bob!b@h PRIVMSG me :\x01ACTION waves\x01"},
		{"plain message", ":bob!b@h PRIVMSG me :hello there"},
		{"channel ctcp not answered", ":bob!b@h PRIVMSG #chan :\x01VERSION\x01"},
		{"unknown ctcp", ":bob!b@h PRIVMSG me :\x01FINGER\x01"},
		{"own echo", ":me!m@h PRIVMSG me :\x01VERSION\x01"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := ctcpClient(t)
			feedPrivmsg(t, c, tc.line)
			if len(rec.sent) != 0 {
				t.Errorf("expected no reply, got %q", rec.sent)
			}
		})
	}
}

// TestCTCPPingArgSanitized guards that a crafted PING token cannot inject extra
// framing or control bytes into the echoed NOTICE.
func TestCTCPPingArgSanitized(t *testing.T) {
	c, rec := ctcpClient(t)
	feedPrivmsg(t, c, ":bob!b@h PRIVMSG me :\x01PING evil\x01nested\x01")
	if len(rec.sent) != 1 {
		t.Fatalf("want 1 reply, got %q", rec.sent)
	}
	// The inner \x01 should be stripped from the echoed token, leaving a single
	// well-framed NOTICE.
	if strings.Count(rec.sent[0], "\x01") != 2 {
		t.Errorf("reply has stray CTCP delimiters: %q", rec.sent[0])
	}
}
