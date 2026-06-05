package client

import (
	"testing"

	"github.com/exec/lurk/cap"
	"github.com/exec/lurk/irc"
)

// nopTransport is a do-nothing transport for driving the client's inbound
// handling without any network I/O: outbound sends are discarded and the inbound
// channel is never used (the fuzz target calls handle directly).
type nopTransport struct{}

func (nopTransport) Messages() <-chan *irc.Message   { return nil }
func (nopTransport) WriteMessage(*irc.Message) error { return nil }
func (nopTransport) Send(string) error               { return nil }
func (nopTransport) Close() error                    { return nil }
func (nopTransport) Err() error                      { return nil }

// FuzzClientHandle throws arbitrary wire lines at the full inbound-handling path
// — PING, CAP/SASL negotiation, registration numerics, and every state-tracking
// mutator (JOIN/PART/QUIT/NICK/KICK/MODE/BATCH/NAMES/topic/metadata). A
// conformant client must never panic on server input, however malformed; this is
// the standing form of the manual "remote-triggerable panic" review. Explore
// with `go test -run=x -fuzz=FuzzClientHandle ./client`.
func FuzzClientHandle(f *testing.F) {
	seeds := []string{
		"PING :tok",
		":srv 001 me :welcome",
		":srv 005 me PREFIX=(ov)@+ CHANTYPES=# CHANMODES=beI,k,l,imnpst :are supported",
		":me!u@h JOIN #c",
		":bob!b@h JOIN #c acct :Bob",
		":bob!b@h PART #c :bye",
		":bob!b@h QUIT :gone",
		":bob!b@h NICK rob",
		":srv 353 me = #c :@op +voice plain",
		":srv 332 me #c :the topic",
		":srv 333 me #c setter 1700000000",
		":op!o@h MODE #c +bo mask!*@* alice",
		":op!o@h MODE #c +o",
		"BATCH +ref chathistory #c",
		"BATCH -ref",
		":bob!b@h ACCOUNT acct",
		":bob!b@h AWAY :brb",
		":bob!b@h CHGHOST u2 h2",
		"CAP * LS :sasl multi-prefix",
		"CAP * ACK :sasl",
		"CAP * NEW :batch",
		"AUTHENTICATE +",
		":srv 904 me :auth failed",
		":kicker!u@h KICK #c victim :reason",
		"@time=2020-01-01T00:00:00.000Z :s PRIVMSG #c :hi",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		m, err := irc.Parse(line)
		if err != nil {
			return
		}
		c := New(Config{Nick: "me"})
		c.tr = nopTransport{}
		c.neg = cap.NewNegotiator(c.cfg.Caps)
		// Feed twice to exercise repeated/stateful transitions (a second JOIN, a
		// close of an already-open batch, a verdict after completion, …).
		c.handle(m)
		c.handle(m)
	})
}

// FuzzSanitizeTerminal asserts the shared terminal sanitizer never emits an
// unsafe control rune and is idempotent, for any input.
func FuzzSanitizeTerminal(f *testing.F) {
	for _, s := range []string{"", "plain", "a\x1b[2Jb", "x\x1b]52;c;AA==\x07y", "héllo 🌍", "a\tb", "\x00\x07\x7f\x9b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := SanitizeTerminal(s)
		for _, r := range out {
			if r == ' ' {
				continue
			}
			if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				t.Fatalf("SanitizeTerminal(%q) leaked control rune %#x in %q", s, r, out)
			}
		}
		if again := SanitizeTerminal(out); again != out {
			t.Fatalf("SanitizeTerminal not idempotent: %q -> %q -> %q", s, out, again)
		}
	})
}
