package client

import (
	"context"
	"net"
	"testing"
	"time"

	"lurk/conn"
)

// TestNamesReconcileOn366 drives two NAMES bursts for the same channel through a
// scripted mock server and asserts the second burst (366-terminated) is treated
// as authoritative: a member present in the first burst but absent from the
// second is reconciled away rather than left as a ghost.
func TestNamesReconcileOn366(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{Nick: "me", User: "u", Realname: "Real Name"})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		// Minimal registration: no caps offered, straight to 001/005.
		srv.expect("CAP LS 302")
		srv.expect("NICK me")
		srv.expect("USER u 0 * :Real Name")
		srv.send("CAP * LS :")
		srv.expect("CAP END")
		srv.send(
			":server 001 me :Welcome to TestNet me",
			":server 005 me PREFIX=(ov)@+ CHANTYPES=# CASEMAPPING=rfc1459 :are supported",
			":server 376 me :End of /MOTD.",
		)

		// JOIN and a first NAMES burst establishing {me, a, b, c}.
		srv.expect("JOIN #chan")
		srv.send(
			":me!u@h JOIN #chan",
			":server 353 me = #chan :@me a b c",
			":server 366 me #chan :End of /NAMES list.",
		)

		// A SECOND NAMES burst (e.g. a manual refresh) naming only {me, a, c}.
		srv.expect("NAMES #chan")
		srv.send(
			":server 353 me = #chan :@me a c",
			":server 366 me #chan :End of /NAMES list.",
		)
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

	if err := c.Join("#chan"); err != nil {
		t.Fatalf("Join: %v", err)
	}
	// First burst: all four members tracked, with self's prefix preserved.
	if !waitFor(func() bool { return len(c.Members("#chan")) == 4 }, time.Second) {
		t.Fatalf("after first burst: got %d members, want 4: %+v", len(c.Members("#chan")), c.Members("#chan"))
	}
	if m, ok := membersByNick(c.Members("#chan"))["me"]; !ok || m.Prefixes != "@" {
		t.Errorf("self member = %+v, want prefix @", m)
	}

	// Second burst drops b. Refresh and wait for reconciliation.
	if err := c.Names("#chan"); err != nil {
		t.Fatalf("Names: %v", err)
	}
	if !waitFor(func() bool {
		_, present := membersByNick(c.Members("#chan"))["b"]
		return !present
	}, time.Second) {
		t.Errorf("after second burst: b still present (ghost): %+v", c.Members("#chan"))
	}
	got := membersByNick(c.Members("#chan"))
	if len(got) != 3 {
		t.Errorf("after second burst: %d members, want 3: %+v", len(got), got)
	}
	for _, want := range []string{"me", "a", "c"} {
		if _, ok := got[want]; !ok {
			t.Errorf("after second burst: %q missing: %+v", want, got)
		}
	}
}

// TestNamesSingleBurstTracksAll checks that a normal single 353+366 burst still
// records every member and their prefixes (no regression from the 366
// reconciliation), and preserves metadata learned outside the burst.
func TestNamesSingleBurstTracksAll(t *testing.T) {
	s := newState()
	s.mergeISupport([]string{"PREFIX=(ov)@+", "CHANTYPES=#"})

	// An account learned earlier (e.g. via extended-join) must survive the sweep.
	cs := s.addChannel("#chan")
	cs.addMember(s.foldKey, "alice", "", "", "")
	cs.members[s.foldKey("alice")].Account = "alice_acct"

	s.applyNamReply("#chan", "@op +voice plain @+double alice")
	s.endNames("#chan")

	cs = s.channel("#chan")
	if cs == nil {
		t.Fatal("channel not tracked")
	}
	want := map[string]string{"op": "@", "voice": "+", "plain": "", "double": "@+", "alice": ""}
	if len(cs.members) != len(want) {
		t.Fatalf("got %d members, want %d: %+v", len(cs.members), len(want), cs.members)
	}
	for nick, pfx := range want {
		m, ok := cs.members[s.foldKey(nick)]
		if !ok {
			t.Errorf("missing member %q", nick)
			continue
		}
		if m.Prefixes != pfx {
			t.Errorf("%s prefixes = %q, want %q", nick, m.Prefixes, pfx)
		}
	}
	if m := cs.members[s.foldKey("alice")]; m == nil || m.Account != "alice_acct" {
		t.Errorf("alice account not preserved across NAMES sweep: %+v", m)
	}
	if cs.namesSeen != nil {
		t.Errorf("namesSeen not cleared after endNames: %v", cs.namesSeen)
	}
}
