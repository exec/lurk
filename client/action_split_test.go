package client

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"lurk/conn"
	"lurk/irc"
)

// TestActionSplitsWithinWireLimit drives a long CTCP ACTION ("/me") to a long
// target through the mock server and asserts every emitted PRIVMSG serializes to
// a line within the 512-byte wire limit AND keeps intact CTCP framing
// (\x01ACTION …\x01) on every chunk — the regression where a long action
// overflowed 512 bytes and dropped its trailing \x01.
func TestActionSplitsWithinWireLimit(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{Nick: "me", User: "u", Realname: "Real Name"})

	// A long STATUSMSG-prefixed channel name a large-CHANNELLEN server could have,
	// plus a long action text that must be split into several PRIVMSGs.
	target := "@#" + strings.Repeat("channel", 26) // 184 bytes
	text := strings.Repeat("the quick brown fox jumps ", 60)

	lines := make(chan string, 64)
	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		srv.expect("CAP LS 302")
		srv.expect("NICK me")
		srv.expect("USER u 0 * :Real Name")
		srv.send("CAP * LS :")
		srv.expect("CAP END")
		srv.send(
			":server 001 me :Welcome to TestNet me",
			":server 005 me PREFIX=(ov)@+ CHANTYPES=# STATUSMSG=@+ :are supported",
			":server 376 me :End of /MOTD.",
		)
		// Capture each PRIVMSG the client sends for the action.
		for {
			line := srv.readLine()
			if !strings.HasPrefix(line, "PRIVMSG ") {
				continue
			}
			lines <- line
			if strings.Contains(line, "DONE") {
				return
			}
		}
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

	if err := c.Action(target, text); err != nil {
		t.Fatalf("Action: %v", err)
	}
	// A sentinel action whose presence marks the end of the captured stream.
	if err := c.Action(target, "DONE"); err != nil {
		t.Fatalf("Action sentinel: %v", err)
	}

	var got []string
	deadline := time.After(3 * time.Second)
collect:
	for {
		select {
		case line := <-lines:
			got = append(got, line)
			if strings.Contains(line, "DONE") {
				break collect
			}
		case <-deadline:
			t.Fatalf("timed out collecting action lines; got %d so far", len(got))
		}
	}

	// Drop the sentinel; assert the real action produced multiple chunks.
	body := got[:len(got)-1]
	if len(body) < 2 {
		t.Fatalf("expected the long action to split into multiple PRIVMSGs, got %d", len(body))
	}

	for i, line := range body {
		// Each captured client line is "PRIVMSG <target> :<framed>". Re-serialize
		// it through irc.Message to measure the true wire length (with CRLF) the
		// way the client transmits it.
		parsed, err := irc.Parse(line)
		if err != nil {
			t.Fatalf("chunk %d: parse %q: %v", i, line, err)
		}
		if parsed.Command != irc.PRIVMSG {
			t.Fatalf("chunk %d: command = %q, want PRIVMSG", i, parsed.Command)
		}
		wire, err := parsed.Serialize()
		if err != nil {
			t.Fatalf("chunk %d: serialize: %v", i, err)
		}
		if len(wire) > 512 {
			t.Errorf("chunk %d serializes to %d bytes, over the 512 wire limit", i, len(wire))
		}
		framed := parsed.Param(1)
		if !strings.HasPrefix(framed, "\x01ACTION ") || !strings.HasSuffix(framed, "\x01") {
			t.Errorf("chunk %d framing broken: %q (want \\x01ACTION …\\x01)", i, framed)
		}
	}

	// The action text, stripped of framing and rejoined, must contain all the
	// original words (no content lost across the split).
	var inner []string
	for _, line := range body {
		parsed, _ := irc.Parse(line)
		framed := parsed.Param(1)
		framed = strings.TrimPrefix(framed, "\x01ACTION ")
		framed = strings.TrimSuffix(framed, "\x01")
		inner = append(inner, framed)
	}
	if got := strings.Fields(strings.Join(inner, " ")); !equalWords(got, strings.Fields(text)) {
		t.Errorf("action round trip lost or split words (%d vs %d)", len(got), len(strings.Fields(text)))
	}
}
