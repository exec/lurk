package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

func TestPersistentNetworkReconnectStatus(t *testing.T) {
	m, a, b := twoNetModel(t)
	_, i := m.ensureBufferIn(b, "#go", BufferChannel)
	retrying := client.Event{Message: &irc.Message{Command: client.EventReconnecting, Params: []string{"connection lost"}}}
	reconnected := client.Event{Message: &irc.Message{Command: client.EventReconnected, Params: []string{"reconnected"}}}
	updated, _ := m.Update(ircMsg{net: b, ev: retrying})
	m = updated.(model)
	if strings.Contains(stripANSI(renderStatus(m)), "reconnecting") || a.history.offline {
		t.Fatal("background reconnect changed the active network's status")
	}
	if got := stripANSI(renderSidebar(m, 18, 20)); !strings.Contains(got, "[retry] oftc") {
		t.Fatalf("sidebar did not mark background network: %q", got)
	}
	m.switchTo(i)
	m.input.SetValue("unfinished message")
	cmdClear(m, nil, "") // scrolling/clearing the notice must not hide the state
	updated, _ = m.Update(ircBatchMsg{net: b, evs: []client.Event{
		retrying, evt(t, ":s 001 me :welcome"),
	}})
	m = updated.(model)
	for _, width := range []int{20, 80} {
		m.width = width
		status := renderStatus(m)
		if !strings.Contains(stripANSI(status), "reconnecting") || lipgloss.Width(status) != width || lipgloss.Height(status) != 1 {
			t.Fatalf("reconnect status at width %d = %q", width, status)
		}
	}
	if !strings.Contains(stripANSI(m.View().Content), "reconnecting") || m.input.Value() != "unfinished message" {
		t.Fatal("reconnect status lost the draft or was missing from the frame")
	}
	b.disconnectReason = "old connection's ERROR"
	updated, _ = m.Update(ircMsg{net: b, ev: reconnected})
	m = updated.(model)
	if strings.Contains(stripANSI(renderStatus(m)), "reconnecting") || strings.Contains(stripANSI(renderSidebar(m, 18, 20)), "[retry]") || b.disconnectReason != "" {
		t.Fatal("reconnect indicators or old failure reason survived recovery")
	}
}

func TestSingleNetworkStatusWithoutNetworkToken(t *testing.T) {
	m := newModel(client.New(client.Config{Nick: "me"}), nil)
	m.activeNet().savedName = "Work"
	m, _ = renderAt(t, m, 80, 24)
	status := stripANSI(renderStatus(m))
	if strings.Contains(status, "connecting") || !strings.Contains(status, "net:Work") {
		t.Fatalf("registered client without NETWORK token has misleading status: %q", status)
	}
	updated, _ := m.Update(ircMsg{net: m.activeNet(), ev: client.Event{Message: &irc.Message{Command: client.EventReconnecting}}})
	m = updated.(model)
	if !strings.Contains(stripANSI(renderSidebar(m, 18, 20)), "(reconnecting)") {
		t.Fatal("single-network sidebar lost the reconnect indicator")
	}
}

func TestTerminalDisconnectReportsReason(t *testing.T) {
	for _, batched := range []bool{false, true} {
		m, a, b := twoNetModel(t)
		failure := evt(t, ":s ERROR :server restarting\x1b[31m")
		var msg tea.Msg = ircClosedMsg{net: b}
		if batched {
			msg = ircBatchMsg{net: b, evs: []client.Event{failure}, closed: true}
		} else {
			updated, _ := m.Update(ircMsg{net: b, ev: failure})
			m = updated.(model)
		}
		updated, _ := m.Update(msg)
		m = updated.(model)
		if m.quitting || len(m.networks) != 1 || m.networks[0] != a {
			t.Fatal("terminal disconnect dropped the healthy network")
		}
		out := bufText(m.activeBuffer())
		if !strings.Contains(out, "disconnected from oftc: server restarting") || strings.Contains(out, "\x1b") {
			t.Fatalf("disconnect reason missing or unsafe (batched=%v): %q", batched, out)
		}
		// Late duplicate closure messages must not remove the surviving network.
		updated, _ = m.Update(ircClosedMsg{net: b})
		m = updated.(model)
		if m.quitting || len(m.networks) != 1 {
			t.Fatal("duplicate closure affected the healthy network")
		}
	}
}

func TestLastNetworkDisconnectExitReason(t *testing.T) {
	for _, intentional := range []bool{false, true} {
		m := newModel(client.New(client.Config{Nick: "me"}), nil)
		m, _ = renderAt(t, m, 80, 24)
		net := m.activeNet()
		net.disconnectReason = "server shutting down"
		if intentional {
			runLine(m, "/disconnect bye")
			if !net.disconnecting {
				t.Fatal("/disconnect did not mark intentional shutdown")
			}
		}
		updated, cmd := m.Update(ircClosedMsg{net: net})
		m = updated.(model)
		if !m.quitting || cmd == nil {
			t.Fatal("last network disconnect did not quit")
		}
		if intentional {
			if m.exitReason != "" {
				t.Fatalf("intentional disconnect reported an error: %q", m.exitReason)
			}
		} else if !strings.Contains(m.exitReason, "server shutting down") {
			t.Fatalf("terminal disconnect lost its exit reason: %q", m.exitReason)
		}
		_ = m.View() // the final frame must tolerate having no buffers
	}
}

func TestDisconnectFallsBackToTransportError(t *testing.T) {
	cli, _ := connectedClient(t, "me") // hermetic net.Pipe server
	m := newModel(cli, nil)
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	updated, _ := m.Update(ircClosedMsg{net: m.activeNet()})
	m = updated.(model)
	if cli.Err() == nil || !strings.Contains(m.exitReason, cli.Err().Error()) {
		t.Fatalf("exit reason did not include transport failure: %q", m.exitReason)
	}
}
