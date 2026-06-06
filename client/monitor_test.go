package client

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/exec/lurk/conn"
)

// registerMinimal drives the server side of a minimal registration (no SASL,
// no caps beyond what is requested). It sends 001 and a 005 with MONITOR=100.
func registerMinimal(t *testing.T, srv *mockServer, nick string, extraCaps ...string) {
	t.Helper()
	srv.expect("CAP LS 302")
	srv.expect("NICK " + nick)
	// Realname defaults to nick; if nick contains no space or special chars the
	// serializer omits the trailing ':' — match what Serialize actually emits.
	srv.expect("USER " + nick + " 0 * " + nick)

	caps := "monitor"
	if len(extraCaps) > 0 {
		caps += " " + strings.Join(extraCaps, " ")
	}
	srv.send("CAP * LS :" + caps)

	req := srv.readLine() // "CAP REQ :monitor"
	reqd := strings.TrimPrefix(req, "CAP REQ :")
	srv.send("CAP * ACK :" + reqd)
	srv.expect("CAP END")

	srv.send(
		":server 001 "+nick+" :Welcome to TestNet "+nick,
		":server 005 "+nick+" NETWORK=TestNet MONITOR=100 CHANTYPES=# :are supported by this server",
		":server 376 "+nick+" :End of /MOTD command.",
	)
}

// TestMonitorAddAndStatus verifies that Monitor() sends "MONITOR + nicks", and
// that subsequent 730/731 lines update MonitoredOnline correctly.
func TestMonitorAddAndStatus(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{
		Nick: "me",
		User: "me",
		Caps: []string{"monitor"},
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		registerMinimal(t, srv, "me")

		// Client calls Monitor("alice", "bob") — expect MONITOR + alice,bob (order
		// may vary; accept either).
		line := srv.readLine()
		if !strings.HasPrefix(line, "MONITOR + ") {
			t.Errorf("expected MONITOR + ..., got %q", line)
		}
		nicks := strings.TrimPrefix(line, "MONITOR + ")
		if !strings.Contains(nicks, "alice") || !strings.Contains(nicks, "bob") {
			t.Errorf("MONITOR + list %q missing alice or bob", nicks)
		}

		// Server reports alice online, bob offline.
		srv.send(
			":server 730 me :alice!a@host",
			":server 731 me :bob!b@host",
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

	// Issue Monitor after registration.
	if !waitFor(func() bool { return c.MonitorLimit() == 100 }, time.Second) {
		t.Fatalf("MonitorLimit() = %d, want 100", c.MonitorLimit())
	}
	if err := c.Monitor("alice", "bob"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}

	// Wait for the 730/731 to be processed.
	if !waitFor(func() bool { return c.MonitoredOnline("alice") }, time.Second) {
		t.Error("alice should be online after 730")
	}
	if !waitFor(func() bool { return !c.MonitoredOnline("bob") }, time.Second) {
		t.Error("bob should be offline after 731")
	}

	// MonitoredNicks should list both.
	nicks := c.MonitoredNicks()
	if len(nicks) != 2 {
		t.Errorf("MonitoredNicks() = %v, want [alice bob]", nicks)
	}
}

// TestMonitorHandlers verifies that HandleMonitorOnline/HandleMonitorOffline
// fire and that state is already updated when the handler runs.
func TestMonitorHandlers(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{
		Nick: "me",
		User: "me",
		Caps: []string{"monitor"},
	})

	onlineEvents := make(chan string, 8)
	offlineEvents := make(chan string, 8)

	c.HandleMonitorOnline(func(ev *Event) {
		// Text() is the trailing param: "nick!user@host[,...]"
		onlineEvents <- ev.Text()
	})
	c.HandleMonitorOffline(func(ev *Event) {
		offlineEvents <- ev.Text()
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		registerMinimal(t, srv, "me")

		srv.readLine() // consume MONITOR + alice

		// Alternate online/offline to confirm both handlers fire.
		srv.send(":server 730 me :alice!a@host")
		srv.send(":server 731 me :alice!a@host")
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

	if err := c.Monitor("alice"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}

	select {
	case got := <-onlineEvents:
		if !strings.Contains(got, "alice") {
			t.Errorf("online event text = %q, want alice in it", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleMonitorOnline handler did not fire")
	}

	select {
	case got := <-offlineEvents:
		if !strings.Contains(got, "alice") {
			t.Errorf("offline event text = %q, want alice in it", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HandleMonitorOffline handler did not fire")
	}
}

// TestMonitorUnmonitorAll verifies that UnmonitorAll sends MONITOR C and that
// MonitoredNicks() is empty afterwards.
func TestMonitorUnmonitorAll(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{
		Nick: "me",
		User: "me",
		Caps: []string{"monitor"},
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		registerMinimal(t, srv, "me")
		srv.readLine()          // MONITOR + alice
		srv.expect("MONITOR C") // UnmonitorAll
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

	if err := c.Monitor("alice"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}
	if !waitFor(func() bool { return len(c.MonitoredNicks()) == 1 }, time.Second) {
		t.Fatalf("MonitoredNicks should have 1 entry, got %v", c.MonitoredNicks())
	}

	if err := c.UnmonitorAll(); err != nil {
		t.Fatalf("UnmonitorAll: %v", err)
	}
	if got := c.MonitoredNicks(); len(got) != 0 {
		t.Errorf("MonitoredNicks after UnmonitorAll = %v, want empty", got)
	}
}

// TestMonitorUnmonitorSingle verifies that Unmonitor removes only the named nick
// and sends MONITOR - <nick>.
func TestMonitorUnmonitorSingle(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	srv := newMockServer(t, serverSide)
	defer srv.close()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := New(Config{
		Nick: "me",
		User: "me",
		Caps: []string{"monitor"},
	})

	scriptDone := make(chan struct{})
	go func() {
		defer close(scriptDone)
		registerMinimal(t, srv, "me")
		srv.readLine() // MONITOR + alice,bob

		line := srv.readLine() // MONITOR - alice
		if !strings.HasPrefix(line, "MONITOR - ") {
			t.Errorf("expected MONITOR - ..., got %q", line)
		}
		if !strings.Contains(line, "alice") {
			t.Errorf("MONITOR - line %q should contain alice", line)
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

	if err := c.Monitor("alice", "bob"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}
	if !waitFor(func() bool { return len(c.MonitoredNicks()) == 2 }, time.Second) {
		t.Fatalf("MonitoredNicks should have 2 entries, got %v", c.MonitoredNicks())
	}

	if err := c.Unmonitor("alice"); err != nil {
		t.Fatalf("Unmonitor: %v", err)
	}
	nicks := c.MonitoredNicks()
	if len(nicks) != 1 || nicks[0] != "bob" {
		t.Errorf("MonitoredNicks after Unmonitor(alice) = %v, want [bob]", nicks)
	}
}

// TestMonitorOnlineStateClearedOnReconnect verifies that after a reconnect the
// online set is reset to unknown (all false) and the watched nicks are
// re-registered with the server.
func TestMonitorOnlineStateClearedOnReconnect(t *testing.T) {
	// Use the Dialer-based reconnect path so we can inject two scripted sessions.
	session := make(chan net.Conn, 4)

	clientSide1, serverSide1 := net.Pipe()
	clientSide2, serverSide2 := net.Pipe()
	session <- clientSide1
	session <- clientSide2

	c := New(Config{
		Nick:          "me",
		User:          "me",
		Caps:          []string{"monitor"},
		AutoReconnect: true,
		Dialer: func(ctx context.Context) (net.Conn, error) {
			return <-session, nil
		},
	})

	// Session 1 script.
	srv1 := newMockServer(t, serverSide1)
	srv2 := newMockServer(t, serverSide2)

	s1Done := make(chan struct{})
	go func() {
		defer close(s1Done)
		registerMinimal(t, srv1, "me")
		srv1.readLine() // MONITOR + alice

		// Report alice online, then drop the connection.
		srv1.send(":server 730 me :alice!a@host")
		srv1.close()
	}()

	s2Done := make(chan struct{})
	go func() {
		defer close(s2Done)
		// Second session: re-registration, then re-MONITOR.
		registerMinimal(t, srv2, "me")
		srv2.readLine() // MONITOR + alice re-sent after reconnect
		srv2.close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	if err := c.Monitor("alice"); err != nil {
		t.Fatalf("Monitor: %v", err)
	}

	// Wait for session 1 to confirm alice online.
	if !waitFor(func() bool { return c.MonitoredOnline("alice") }, 2*time.Second) {
		t.Fatal("alice not online before disconnect")
	}

	// Wait for session 2 scripts to finish (reconnect completed).
	select {
	case <-s2Done:
	case <-time.After(5 * time.Second):
		t.Fatal("second session script timed out")
	}

	// After reconnect the online set should have been cleared: alice is back to
	// unknown (false) until the server sends fresh 730/731.
	if c.MonitoredOnline("alice") {
		t.Error("alice should be offline/unknown after reconnect (online set cleared)")
	}

	// alice is still in the monitored list.
	if nicks := c.MonitoredNicks(); len(nicks) != 1 || nicks[0] != "alice" {
		t.Errorf("MonitoredNicks after reconnect = %v, want [alice]", nicks)
	}

	c.Close()
	<-s1Done
}

// TestMonitorStateTracking is a pure unit test for the state layer — no
// transport — to verify fold-aware lookup and the multiple-nick-per-reply path.
func TestMonitorStateTracking(t *testing.T) {
	s := newTestState("CASEMAPPING=rfc1459")

	s.addMonitor("Alice")
	s.addMonitor("BOB")

	// Online event lists both; folded lookup should find them regardless of case.
	s.setMonitorOnline("alice", true) // different case than stored "Alice"
	s.setMonitorOnline("bob", true)

	if !s.isMonitoredOnline("ALICE") {
		t.Error("ALICE should be online (fold-aware)")
	}
	if !s.isMonitoredOnline("bob") {
		t.Error("bob should be online")
	}

	// Offline clears the entry.
	s.setMonitorOnline("Alice", false)
	if s.isMonitoredOnline("alice") {
		t.Error("alice should be offline after setMonitorOnline false")
	}

	// removeMonitor drops from both sets.
	s.setMonitorOnline("bob", true)
	s.removeMonitor("Bob")
	if s.isMonitoredOnline("bob") {
		t.Error("bob should be gone after removeMonitor")
	}
	if _, ok := s.monitored[s.foldKey("bob")]; ok {
		t.Error("bob should not be in monitored map after removeMonitor")
	}

	// clearMonitor wipes everything.
	s.addMonitor("carol")
	s.setMonitorOnline("carol", true)
	s.clearMonitor()
	if len(s.monitored) != 0 || len(s.monitorOnline) != 0 {
		t.Error("clearMonitor should empty both maps")
	}
}
