package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
)

// connect.go adds runtime multi-network support: /connect <name> dials an
// additional saved network and folds it into the unified buffer list. cmd/lurk
// supplies the ConnectFunc (it owns the config→client mapping and the dial); the
// TUI runs it on a background Cmd and, on success, registers the new network.

// ConnectFunc dials and registers a saved network by name, returning its
// connected client (with autojoins already requested). It blocks, so the TUI
// only ever calls it from a background Cmd.
type ConnectFunc func(name string) (*client.Client, error)

// connectedMsg is delivered when a /connect dial succeeded.
type connectedMsg struct {
	name string
	cli  *client.Client
}

// connectErrMsg is delivered when a /connect dial failed.
type connectErrMsg struct {
	name string
	err  error
}

// connectCmd returns a Cmd that dials name in the background via the model's
// ConnectFunc and reports the outcome.
func connectCmd(fn ConnectFunc, name string) tea.Cmd {
	return func() tea.Msg {
		cli, err := fn(name)
		if err != nil {
			return connectErrMsg{name: name, err: err}
		}
		return connectedMsg{name: name, cli: cli}
	}
}

// addConnected registers a freshly connected network, focuses its server buffer,
// and returns the model plus the Cmd that starts pumping its events.
func (m model) addConnected(name string, cli *client.Client) (model, tea.Cmd) {
	net := m.addNetwork(name, cli)
	// Focus the new network's server buffer.
	if i := m.bufferIndexIn(net, name); i >= 0 {
		m.switchTo(i)
	} else {
		m.switchTo(len(m.buffers) - 1)
	}
	m = layout(m)
	return m, tea.Batch(waitForIRC(net), func() tea.Msg { return historyStartMsg{net: net} })
}

// networkClosed reports the terminal disconnect before its buffers disappear.
func (m model) networkClosed(net *network) (model, tea.Cmd) {
	if !m.hasNetwork(net) {
		return m, nil
	}
	note := "disconnected from " + sanitize(net.label())
	if !net.disconnecting {
		reason := net.disconnectReason
		if reason == "" && net.cli != nil {
			if err := net.cli.Err(); err != nil {
				reason = sanitize(err.Error())
			}
		}
		if reason != "" {
			note += ": " + reason
		}
	}
	m = m.removeNetwork(net)
	if len(m.networks) == 0 {
		m.quitting = true
		if !net.disconnecting {
			m.exitReason = note
		}
		return m, tea.Quit
	}
	// The new focus may never have been displayed; size and fill its viewport.
	m = layout(m)
	return appendInfo(m, m.activeBuffer(), note), nil
}
