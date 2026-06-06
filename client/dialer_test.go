package client

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestConfigDialerSeam verifies the public Config.Dialer override: Connect uses
// the supplied dialer to obtain the transport (so an embedder can drive a real
// reconnectable Client against an in-process server over net.Pipe), registration
// completes normally, and Server may be empty when a Dialer is set.
func TestConfigDialerSeam(t *testing.T) {
	const nick = "dialbot"
	var down atomic.Bool
	var mu sync.Mutex
	var dials int

	c := New(Config{
		Nick: nick, User: "u", Realname: "r", Caps: []string{},
		// No Server set — the Dialer supplies the connection.
		Dialer: func(ctx context.Context) (net.Conn, error) {
			mu.Lock()
			dials++
			mu.Unlock()
			clientSide, serverSide := net.Pipe()
			srv := newMockServer(t, serverSide)
			srv.down = &down
			go miniRegister(t, srv, nick)
			return clientSide, nil
		},
	})
	defer c.Close()
	defer down.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect via Config.Dialer: %v", err)
	}

	if got := c.Nick(); got != nick {
		t.Errorf("Nick() = %q, want %q", got, nick)
	}
	mu.Lock()
	got := dials
	mu.Unlock()
	if got != 1 {
		t.Errorf("Dialer invoked %d times, want exactly 1 for a single Connect", got)
	}
}

// TestConnectRequiresServerOrDialer verifies the relaxed precondition: Connect
// still errors when neither Server nor Dialer is provided.
func TestConnectRequiresServerOrDialer(t *testing.T) {
	c := New(Config{Nick: "x", Caps: []string{}})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Connect(ctx); err == nil {
		t.Fatal("Connect with neither Server nor Dialer should error")
	}
}
