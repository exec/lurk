package client

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
)

// TestHandleStandardReplyFAIL verifies that a FAIL sent by the server after
// registration fires the HandleStandardReply handler and that the event's
// parameters are correct.
func TestHandleStandardReplyFAIL(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{
		Nick: "me",
		User: "me",
		Caps: []string{}, // no caps; standard-replies doesn't need a negotiated cap
	})

	type reply struct {
		command string
		param0  string // the command the reply relates to
		param1  string // the machine-readable code
		text    string // human-readable description
	}
	got := make(chan reply, 4)

	c.HandleStandardReply(func(ev *Event) {
		got <- reply{
			command: ev.Command(),
			param0:  ev.Param(0),
			param1:  ev.Param(1),
			text:    ev.Text(),
		}
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		// Minimal registration: no caps requested.
		srv.expect("CAP LS 302")
		srv.expect("NICK me")
		srv.expect("USER me 0 * me")
		srv.send("CAP * LS :") // no caps advertised
		srv.expect("CAP END")  // client sends CAP END with empty intersection
		srv.send(":server 001 me :Welcome to TestNet me")
		srv.send(":server 376 me :End of /MOTD command.")

		// Server sends a FAIL after registration.
		srv.send(":server FAIL JOIN CHANNEL_BANNED #lurk :You are banned from that channel")
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

	select {
	case r := <-got:
		if r.command != "FAIL" {
			t.Errorf("Command() = %q, want FAIL", r.command)
		}
		if r.param0 != "JOIN" {
			t.Errorf("Param(0) = %q, want JOIN", r.param0)
		}
		if r.param1 != "CHANNEL_BANNED" {
			t.Errorf("Param(1) = %q, want CHANNEL_BANNED", r.param1)
		}
		if r.text != "You are banned from that channel" {
			t.Errorf("Text() = %q, want the description", r.text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleStandardReply handler did not fire for FAIL")
	}
}

// TestHandleStandardReplyAllVerbs verifies that WARN and NOTE also trigger the
// same handler registered via HandleStandardReply, and that Command() reports
// the right verb for each.
func TestHandleStandardReplyAllVerbs(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{
		Nick: "me",
		User: "me",
		Caps: []string{},
	})

	received := make(chan string, 8)
	c.HandleStandardReply(func(ev *Event) { received <- ev.Command() })

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		srv.expect("CAP LS 302")
		srv.expect("NICK me")
		srv.expect("USER me 0 * me")
		srv.send("CAP * LS :")
		srv.expect("CAP END")
		srv.send(":server 001 me :Welcome to TestNet me")
		srv.send(":server 376 me :End of /MOTD command.")

		srv.send(":server WARN REHASH REHASH_IN_PROGRESS :Rehash already in progress")
		srv.send(":server NOTE * OPER_MESSAGE :Server is operating normally")
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

	want := []string{"WARN", "NOTE"}
	for _, wantCmd := range want {
		select {
		case got := <-received:
			if got != wantCmd {
				t.Errorf("got command %q, want %q", got, wantCmd)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("HandleStandardReply handler did not fire for %s", wantCmd)
		}
	}
}

// TestStandardRepliesInDefaultCaps verifies that "standard-replies" is included
// in DefaultCaps so a zero-config Client requests the capability automatically.
func TestStandardRepliesInDefaultCaps(t *testing.T) {
	found := false
	for _, cap := range DefaultCaps {
		if cap == "standard-replies" {
			found = true
			break
		}
	}
	if !found {
		t.Error("\"standard-replies\" not found in DefaultCaps")
	}
}
