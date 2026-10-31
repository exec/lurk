package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/client"
	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// replayUpstream feeds real client events, including resolved history batches,
// into the server. Each send waits until ingestion and fan-out have finished.
func replayUpstream(t *testing.T, srv *Server) func(string) {
	t.Helper()
	a, b := net.Pipe()
	remote := conn.NewConn(b, conn.Options{})
	c := client.New(client.Config{Nick: "me", Caps: upstreamCaps})
	t.Cleanup(func() { _ = c.Close(); _ = remote.Close() })
	processed := make(chan struct{}, 1)
	c.OnAny(func(ev *client.Event) {
		srv.Ingest(1, ev)
		if ev.Command() == irc.PRIVMSG || ev.Command() == irc.BATCH {
			processed <- struct{}{}
		}
	})
	go func() {
		for range 3 {
			<-remote.Messages()
		}
		_ = remote.WriteMessage(&irc.Message{Command: irc.CAP, Params: []string{"*", "LS", "batch draft/chathistory server-time echo-message"}})
		req := <-remote.Messages()
		if req == nil {
			return
		}
		_ = remote.WriteMessage(&irc.Message{Command: irc.CAP, Params: []string{"*", "ACK", req.Param(1)}})
		<-remote.Messages() // CAP END
		_ = remote.WriteMessage(&irc.Message{Source: "s", Command: "001", Params: []string{"me", "welcome"}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	if err := c.ConnectConn(ctx, conn.NewConn(a, conn.Options{})); err != nil {
		t.Fatal(err)
	}
	return func(raw string) {
		t.Helper()
		msg, err := irc.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := remote.WriteMessage(msg); err != nil {
			t.Fatal(err)
		}
		select {
		case <-processed:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream event was not processed")
		}
	}
}

func TestReplayDeduplicatesStorageAndFanout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ring    int
		seed    int
		rotate  bool
		restart bool
	}{
		{name: "ring", ring: 8, seed: 1},
		{name: "disk", ring: 2, seed: 3},
		{name: "rotation", ring: 2, seed: 3, rotate: true},
		{name: "restart", ring: 2, seed: 3, restart: true},
		{name: "rotation_restart", ring: 2, seed: 3, rotate: true, restart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opts := []backlog.Option{backlog.WithRingSize(tc.ring)}
			if tc.rotate {
				opts = append(opts, backlog.WithMaxFileSize(400))
			}
			store, err := backlog.NewStore(dir, opts...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			srv := New(&Config{})
			srv.WithStore(store)
			local, remote := net.Pipe()
			downstream := conn.NewConn(local, conn.Options{})
			attached := &session{netid: 1, conn: conn.NewConn(remote, conn.Options{})}
			t.Cleanup(func() { _ = downstream.Close(); _ = attached.conn.Close() })
			srv.boundSessions[1] = map[*session]struct{}{attached: {}}
			feed := replayUpstream(t, srv)
			feed("@msgid=up1 :bob!u@h PRIVMSG #c :original")
			live := skipTo(t, downstream, irc.PRIVMSG)
			originalID := live.Tags.Get("msgid")
			if originalID == "" || originalID == "up1" {
				t.Fatal("live delivery did not receive its own canonical ID")
			}
			for n := 1; n < tc.seed; n++ {
				store.Ingest(1, &client.Event{Message: &irc.Message{
					Source: "bob!u@h", Command: irc.PRIVMSG,
					Tags: irc.Tags{"msgid": fmt.Sprintf("filler%d", n)}, Params: []string{"#c", "filler"},
				}})
			}
			path := filepath.Join(dir, "1", "#c.jsonl")
			if tc.rotate {
				if _, err := os.Stat(path + ".1"); err != nil {
					t.Fatalf("fixture did not rotate: %v", err)
				}
			}
			if tc.restart {
				store.Close()
				restarted, err := backlog.NewStore(dir, opts...)
				if err != nil {
					t.Fatal(err)
				}
				store = restarted
				srv.WithStore(store)
			}
			before, _ := os.ReadFile(path)
			rotated, _ := os.ReadFile(path + ".1")
			feed(":s BATCH +h chathistory #c")
			feed("@batch=h;msgid=up1 :bob!u@h PRIVMSG #c :original")
			after, _ := os.ReadFile(path)
			afterRotation, _ := os.ReadFile(path + ".1")
			if string(before) != string(after) || string(rotated) != string(afterRotation) {
				t.Fatal("replay changed the stored files")
			}
			feed("@batch=h;msgid=up2 :bob!u@h PRIVMSG #c :missed message")
			feed(":s BATCH -h")
			reply := recvBatch(t, downstream)
			if len(reply.entries) != 1 || reply.entries[0].Param(1) != "missed message" {
				t.Fatalf("replay relayed an old duplicate or lost a new message: %+v", reply.entries)
			}
			newID := reply.entries[0].Tags.Get("msgid")
			older := store.Before(1, "#c", backlog.Ref{IsMsgID: true, MsgID: newID}, 10)
			if len(older) != tc.seed || older[0].MsgID != originalID {
				t.Fatal("replay duplicated entries or changed the original canonical ID")
			}
		})
	}
}
