package client

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/exec/lurk/irc"
)

func TestMessagesFitRelayedWireLimit(t *testing.T) {
	for _, kind := range []string{"PRIVMSG", "NOTICE", "ACTION"} {
		t.Run(kind, func(t *testing.T) {
			c, rec := newRecordClient()
			c.st.self = "sender"
			feedTrack(t, c, ":sender!~ident@cloak.example.test JOIN #c")
			for _, mask := range []string{
				"sender!~ident@cloak.example.test",
				"sender!" + strings.Repeat("u", 20) + "@" + strings.Repeat("h", 120),
			} {
				if strings.Contains(mask, strings.Repeat("h", 120)) {
					feedTrack(t, c, ":sender!~ident@cloak.example.test CHGHOST "+strings.Repeat("u", 20)+" "+strings.Repeat("h", 120))
				}
				rec.sent = nil
				text := strings.Repeat("界🙂word ", 130)
				var err error
				switch kind {
				case "PRIVMSG":
					err = c.Privmsg("#c", text)
				case "NOTICE":
					err = c.Notice("#c", text)
				case "ACTION":
					err = c.Action("#c", text)
				}
				if err != nil {
					t.Fatal(err)
				}
				var chunks []string
				for _, line := range rec.sent {
					m, err := irc.Parse(line)
					if err != nil {
						t.Fatal(err)
					}
					body := m.Param(1)
					if kind == "ACTION" {
						var ok bool
						body, ok = CTCPAction(body)
						if !ok || !strings.HasSuffix(m.Param(1), "\x01") {
							t.Fatal("ACTION framing lost")
						}
					}
					if !utf8.ValidString(body) {
						t.Fatal("split inside UTF-8 rune")
					}
					chunks = append(chunks, body)
					m.Source = mask
					relayed, err := m.Serialize()
					if err != nil {
						t.Fatal(err)
					}
					if len(relayed) > 512 {
						t.Fatalf("relayed %s has %d bytes, want <=512", kind, len(relayed))
					}
				}
				if !equalWords(strings.Fields(strings.Join(chunks, " ")), strings.Fields(text)) {
					t.Fatal("message content lost")
				}
			}
		})
	}
}

func TestRelayBudgetTracksSelfIdentity(t *testing.T) {
	c := New(Config{Nick: "me", User: "configured"})
	c.st.self = "me"
	c.tr = newRecordTransport()
	unknown := c.messageBudget(irc.PRIVMSG, "#c")
	feedHandle(t, c, ":me!~u@short.host JOIN #c")
	known := c.messageBudget(irc.PRIVMSG, "#c")
	if known <= unknown {
		t.Fatal("known mask should release unused fallback space")
	}
	feedHandle(t, c, ":srv BATCH +h chathistory #c")
	feedHandle(t, c, "@batch=h :me!old@old.host PRIVMSG #c :old self message")
	feedHandle(t, c, ":srv BATCH -h")
	if got := c.messageBudget(irc.PRIVMSG, "#c"); got != known {
		t.Fatal("history replay changed our current relay identity")
	}
	feedHandle(t, c, ":other!x@elsewhere CHGHOST changed longer.host")
	if got := c.messageBudget(irc.PRIVMSG, "#c"); got != known {
		t.Fatal("another member changed our budget")
	}
	feedHandle(t, c, ":me!~u@short.host NICK longer-nick")
	if got := c.messageBudget(irc.PRIVMSG, "#c"); got != known-len("longer-nick")+len("me") {
		t.Fatal("nickname change did not update relay budget")
	}
	c.resetRegistration()
	if c.st.selfUser != "" || c.st.selfHost != "" {
		t.Fatal("reconnect retained stale relay identity")
	}
	if got := c.messageBudget(irc.PRIVMSG, "#c"); got >= known {
		t.Fatal("reconnect did not restore fallback allowance")
	}
}

func TestRelayedMessageLengthsOverPipe(t *testing.T) {
	for _, kind := range []string{"PRIVMSG", "NOTICE", "ACTION"} {
		t.Run(kind, func(t *testing.T) {
			c, srv, cleanup := dialNoSASL(t, []string{"server-time"})
			defer cleanup()
			const mask = "me!~ident@cloak.example.test"
			srv.send(":"+mask+" JOIN #c", "PING :identity-tracked")
			srv.expect("PONG identity-tracked")
			text := strings.Repeat("界🙂word ", 130)
			var err error
			switch kind {
			case "PRIVMSG":
				err = c.Privmsg("#c", text)
			case "NOTICE":
				err = c.Notice("#c", text)
			case "ACTION":
				err = c.Action("#c", text)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Privmsg("#c", "split-done"); err != nil {
				t.Fatal(err)
			}
			var chunks []string
			for {
				line := srv.readLine()
				m, err := irc.Parse(line)
				if err != nil {
					t.Fatal(err)
				}
				body := m.Param(1)
				if body == "split-done" {
					break
				}
				if n := len(":" + mask + " " + line + "\r\n"); n > 512 {
					t.Fatalf("relayed %s line has %d bytes", kind, n)
				}
				if kind == "ACTION" {
					var ok bool
					body, ok = CTCPAction(body)
					if !ok || !strings.HasSuffix(m.Param(1), "\x01") {
						t.Fatal("ACTION framing lost")
					}
				}
				if !utf8.ValidString(body) {
					t.Fatal("invalid UTF-8 chunk")
				}
				chunks = append(chunks, body)
			}
			if !equalWords(strings.Fields(strings.Join(chunks, " ")), strings.Fields(text)) {
				t.Fatal("wire chunks lost content")
			}
		})
	}
}
