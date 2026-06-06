package tui

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/conn"
)

// connectedClient wires a real client through net.Pipe, runs a minimal
// no-caps IRC registration handshake on the server side in a goroutine, and
// returns the connected client plus a channel that is closed when the server
// side sees a QUIT line. The goroutine drains lines until QUIT (ignoring all
// other client output), then closes quitSeen.
//
// The server side is minimal: it acknowledges CAP LS with an empty cap list
// and sends the two lines (001 + 376) required for the client to consider
// itself registered. After that it reads lines until it sees "QUIT …" and
// signals quitSeen.
func connectedClient(t *testing.T, nick string) (*client.Client, <-chan struct{}) {
	t.Helper()
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() { clientSide.Close(); serverSide.Close() })

	quitSeen := make(chan struct{})

	go func() {
		defer serverSide.Close()
		br := bufio.NewReader(serverSide)
		deadline := func() {
			_ = serverSide.SetDeadline(time.Now().Add(3 * time.Second))
		}
		writeLine := func(s string) {
			deadline()
			_, _ = serverSide.Write([]byte(s + "\r\n"))
		}
		readLine := func() string {
			deadline()
			line, _ := br.ReadString('\n')
			return strings.TrimRight(line, "\r\n")
		}

		// Minimal no-caps handshake:
		//   client → CAP LS 302
		//   client → NICK <nick>
		//   client → USER <nick> 0 * :<nick>
		//   server ← CAP * LS :
		//   client → CAP END
		//   server ← :srv 001 <nick> :welcome
		//   server ← :srv 376 <nick> :End of MOTD
		readLine() // CAP LS 302
		readLine() // NICK <nick>
		readLine() // USER ...
		writeLine("CAP * LS :")
		readLine() // CAP END
		writeLine(":srv 001 " + nick + " :welcome")
		writeLine(":srv 376 " + nick + " :End of MOTD")

		// Drain until QUIT.
		for {
			line := readLine()
			if line == "" {
				return
			}
			if strings.HasPrefix(line, "QUIT") {
				close(quitSeen)
				return
			}
		}
	}()

	tr := conn.NewConn(clientSide, conn.Options{})
	c := client.New(client.Config{
		Nick:     nick,
		User:     nick,
		Realname: nick,
		Caps:     []string{}, // empty → no cap negotiation beyond CAP LS/END
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	if err := c.ConnectConn(ctx, tr); err != nil {
		t.Fatalf("ConnectConn %s: %v", nick, err)
	}
	return c, quitSeen
}

// twoNetConnectedModel builds a model with two fully-registered clients
// (connected through net.Pipe) and returns the model along with a quit-seen
// channel for each network. The first network is active.
func twoNetConnectedModel(t *testing.T) (model, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	cli0, quit0 := connectedClient(t, "me0")
	cli1, quit1 := connectedClient(t, "me1")

	m := newModel(cli0, nil)
	m.addNetwork("oftc", cli1)
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)
	return m, quit0, quit1
}

// waitClosed reports whether ch is closed within 2 s.
func waitClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// TestQuitCommandAllNetworks verifies that /quit sends the IRC QUIT message
// to every connected network, not just the active one. With two networks the
// QUIT should reach both pipe-connected mock servers.
func TestQuitCommandAllNetworks(t *testing.T) {
	m, quit0, quit1 := twoNetConnectedModel(t)

	act, cmd := runLine(m, "/quit bye")
	if act.kind != actionNone {
		t.Errorf("/quit action kind = %v, want actionNone", act.kind)
	}
	if cmd == nil {
		t.Fatal("/quit returned nil cmd, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("/quit cmd did not produce tea.QuitMsg")
	}

	if !waitClosed(quit0) {
		t.Error("network 0 (active) did not receive QUIT")
	}
	if !waitClosed(quit1) {
		t.Error("network 1 (inactive) did not receive QUIT")
	}
}

// TestCtrlCAllNetworks verifies that Ctrl-C sends the IRC QUIT message to
// every connected network via the handleKey path in app.go.
func TestCtrlCAllNetworks(t *testing.T) {
	m, quit0, quit1 := twoNetConnectedModel(t)

	tm, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl, Text: ""})
	m = tm.(model)
	if !m.quitting {
		t.Error("model.quitting not set after Ctrl-C")
	}
	if cmd == nil {
		t.Fatal("Ctrl-C returned nil cmd, want tea.Quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("Ctrl-C cmd did not produce tea.QuitMsg")
	}

	if !waitClosed(quit0) {
		t.Error("network 0 (active) did not receive QUIT via Ctrl-C")
	}
	if !waitClosed(quit1) {
		t.Error("network 1 (inactive) did not receive QUIT via Ctrl-C")
	}
}
