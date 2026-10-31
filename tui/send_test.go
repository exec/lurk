package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
)

func TestFailedSendKeepsInput(t *testing.T) {
	for _, line := range []string{
		"hello channel", "/msg bob hello bob", "/me waves", "//literal slash",
		"/query bob hello bob", "/notice bob hello bob", "/ctcp bob PING 123",
		"/join #new", "/join #new secret", "/part", "/part #go leaving now",
		"/close", "/close #go",
		"/nick newnick", "/topic", "/topic a new topic", "/whois bob",
		"/list", "/list >50", "/away", "/away back after lunch", "/names",
		"/kick bob reason", "/op bob alice", "/deop bob", "/voice bob",
		"/devoice bob", "/ban *!*@example.net", "/unban *!*@example.net",
		"/invite bob", "/invite bob #other", "/mode", "/mode #go +m",
		"/raw PING test", "/motd", "/whowas bob",
	} {
		t.Run(line, func(t *testing.T) {
			m := newModel(client.New(client.Config{Nick: "me"}), nil)
			_, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			before := m.activeBuffer()
			m.input.SetValue(line)
			tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = tm.(model)
			if m.activeBuffer() != before || len(m.buffers) != 2 {
				t.Fatal("failed send changed the open buffers or active conversation")
			}
			if m.chanListOpen {
				t.Fatal("failed LIST opened a loading modal")
			}
			if got := m.input.Value(); got != line {
				t.Errorf("input = %q, want original %q", got, line)
			}
			if len(m.history) != 1 || m.history[0] != line {
				t.Errorf("input history = %q, want submitted line", m.history)
			}
			var rows []string
			for _, b := range m.buffers {
				rows = append(rows, b.lines...)
			}
			if len(rows) != 1 || !strings.Contains(stripANSI(rows[0]), "not connected") {
				t.Errorf("scrollback = %q, want only a send error", rows)
			}
		})
	}
}

func TestSendAfterConnectionClosed(t *testing.T) {
	cli, _ := connectedClient(t, "me")
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"retry me", "/join #new", "/part", "/part #go bye", "/close",
		"/nick newnick", "/topic new topic", "/list", "/away lunch",
		"/names", "/whois bob", "/kick bob", "/op bob", "/invite bob",
		"/raw PING test", "/mode", "/motd", "/whowas bob",
	} {
		t.Run(line, func(t *testing.T) {
			m := newModel(cli, nil)
			b, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			m.input.SetValue(line)
			tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = tm.(model)
			if m.input.Value() != line || m.activeBuffer() != b || len(m.buffers) != 2 {
				t.Error("closed connection discarded input or changed buffers")
			}
			rows := m.activeBuffer().lines
			if len(rows) != 1 || !strings.Contains(stripANSI(rows[0]), "failed") {
				t.Errorf("scrollback = %q, want only a send error", rows)
			}
		})
	}
}

func TestProtocolCommandsSendSuccessfully(t *testing.T) {
	for _, tc := range []struct {
		line, wire, info string
		list             bool
	}{
		{line: "/nick newnick", wire: "NICK newnick"},
		{line: "/topic", wire: "TOPIC #go"},
		{line: "/topic a new topic", wire: "TOPIC #go :a new topic"},
		{line: "/whois bob", wire: "WHOIS bob"},
		{line: "/list", wire: "LIST", list: true},
		{line: "/list >50", wire: "LIST >50", list: true},
		{line: "/away", wire: "AWAY", info: "marked back"},
		{line: "/away back after lunch", wire: "AWAY :back after lunch", info: "marked away: back after lunch"},
		{line: "/names", wire: "NAMES #go"},
		{line: "/kick bob reason", wire: "KICK #go bob reason"},
		{line: "/op bob alice", wire: "MODE #go +oo bob alice"},
		{line: "/deop bob", wire: "MODE #go -o bob"},
		{line: "/voice bob", wire: "MODE #go +v bob"},
		{line: "/devoice bob", wire: "MODE #go -v bob"},
		{line: "/ban *!*@example.net", wire: "MODE #go +b *!*@example.net"},
		{line: "/unban *!*@example.net", wire: "MODE #go -b *!*@example.net"},
		{line: "/invite bob", wire: "INVITE bob #go"},
		{line: "/invite bob #other", wire: "INVITE bob #other"},
		{line: "/mode", wire: "MODE #go"},
		{line: "/mode #go +m", wire: "MODE #go +m"},
		{line: "/raw PING test", wire: "PING test"},
		{line: "/motd", wire: "MOTD"},
		{line: "/whowas bob", wire: "WHOWAS bob"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			m, srv := historyModel(t)
			b, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			m.input.SetValue(tc.line)
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(model)
			historyWire(t, srv, tc.wire)
			if m.input.Value() != "" || m.activeBuffer() != b {
				t.Fatal("successful command left submitted input or changed the conversation")
			}
			if m.chanListOpen != tc.list || m.chanListLoading != tc.list {
				t.Fatal("successful LIST did not open its loading modal, or another command opened it")
			}
			if tc.info == "" {
				if len(b.lines) != 0 {
					t.Fatalf("successful command added an unexpected line: %q", b.lines)
				}
			} else if len(b.lines) != 1 || !strings.Contains(stripANSI(b.lines[0]), tc.info) {
				t.Fatalf("successful command lost its confirmation: %q", b.lines)
			}
		})
	}
}

func TestChannelCommandsSendAndChangeBuffers(t *testing.T) {
	for _, tc := range []struct {
		line, wire string
		join       bool
	}{
		{"/join #new", "JOIN #new", true},
		{"/join #new secret", "JOIN #new secret", true},
		{"/join #new :secret", "JOIN #new ::secret", true},
		{"/part", "PART #go", false},
		{"/part leaving now", "PART #go :leaving now", false},
		{"/part #go leaving now", "PART #go :leaving now", false},
		{"/close", "PART #go", false},
	} {
		t.Run(tc.line, func(t *testing.T) {
			m, srv := historyModel(t)
			_, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			m.input.SetValue(tc.line)
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(model)
			historyWire(t, srv, tc.wire)
			if m.input.Value() != "" {
				t.Fatal("successful command left the submitted input")
			}
			if tc.join {
				if m.activeBuffer().Title != "#new" {
					t.Fatal("successful join did not focus the requested channel")
				}
			} else if m.buffer("#go") != nil {
				t.Fatal("successful leave did not close the channel buffer")
			}
		})
	}
}

func TestFailedBackgroundLeavePreservesDraft(t *testing.T) {
	for _, line := range []string{"/part #other", "/part #other leaving", "/close #other"} {
		t.Run(line, func(t *testing.T) {
			m, srv := historyModel(t)
			healthy := m.activeNet()
			offline := m.addNetwork("offline", client.New(client.Config{Nick: "me"}))
			b, i := m.ensureBufferIn(offline, "#other", BufferChannel)
			m.switchTo(i)
			m.input.SetValue("unsent channel draft")
			m.input.SetCursor(5)
			m.switchTo(0)
			m.input.SetValue(line)
			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(model)
			if m.netBuffer(offline, "#other") != b || m.input.Value() != line || m.activeNet() != healthy {
				t.Fatal("failed background leave changed buffers, focus, or input")
			}
			if b.draft != "unsent channel draft" || b.draftCursor != 5 {
				t.Fatal("failed background leave discarded the channel's draft")
			}
			// The next message must be this barrier, never PART on the healthy net.
			if err := healthy.cli.SendRaw("PING barrier"); err != nil {
				t.Fatal(err)
			}
			historyWire(t, srv, "PING barrier")
		})
	}
}

func TestClosePMAndUnknownChannelStayLocal(t *testing.T) {
	m, srv := historyModel(t)
	pm, i := m.ensureBuffer("alice", BufferPM)
	pm.historyLoaded = true
	m.switchTo(i)
	act, _ := runLine(m, "/close")
	m = applyAction(m, act)
	if m.buffer("alice") != nil {
		t.Fatal("/close did not close the PM")
	}
	act, _ = runLine(m, "/close #unknown")
	if act.kind != actionInfo || !strings.Contains(act.text, "no such buffer") {
		t.Fatal("unknown /close target did not report a missing buffer")
	}
	if err := m.cli.SendRaw("PING barrier"); err != nil {
		t.Fatal(err)
	}
	historyWire(t, srv, "PING barrier")
}

func TestSuccessfulSendClearsInputAndEchoes(t *testing.T) {
	cli, _ := connectedClient(t, "me")
	t.Cleanup(func() { _ = cli.Close() })
	for _, tc := range []struct {
		line, target, echo string
	}{
		{"hello channel", "#go", "hello channel"},
		{"/msg bob hello bob", "bob", "hello bob"},
		{"/me waves", "#go", "waves"},
		{"//literal slash", "#go", "/literal slash"},
	} {
		t.Run(tc.line, func(t *testing.T) {
			m := newModel(cli, nil)
			_, i := m.ensureBuffer("#go", BufferChannel)
			m.switchTo(i)
			m.input.SetValue(tc.line)
			tm, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = tm.(model)
			if got := m.input.Value(); got != "" {
				t.Errorf("successful send left input %q", got)
			}
			b := m.buffer(tc.target)
			if b == nil || len(b.lines) != 1 || !strings.Contains(stripANSI(b.lines[0]), tc.echo) {
				t.Error("successful send did not echo into the recipient's buffer")
			}
		})
	}
}
