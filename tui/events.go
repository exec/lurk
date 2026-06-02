package tui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"lurk/client"
	"lurk/irc"
)

// This file bridges the client's event stream into the Bubble Tea update loop.
// The design follows the idiomatic "channel + re-subscribing Cmd" subscription
// from reference/bubbletea/examples/realtime/main.go: the model holds the
// channel (not the *Program), so it stays pure and testable, and exactly one
// waitForIRC Cmd is in flight at a time. After handling each ircMsg, Update
// MUST re-issue waitForIRC or the subscription dies (see docs/TUI-RESEARCH.md
// §2).

// ircMsg wraps a single inbound client.Event for delivery into Update. It is a
// distinct type (not a bare client.Event) so the Update type switch can match
// it unambiguously alongside Bubble Tea's own message types.
type ircMsg struct {
	ev client.Event
}

// ircClosedMsg is delivered once when the event stream closes, which happens
// when the client's connection ends. Update treats it as a disconnect and
// quits the program (the line client's behavior, surfaced in the TUI).
type ircClosedMsg struct{}

// waitForIRC returns a Cmd that blocks on the next event from sub and delivers
// it as an ircMsg. Bubble Tea runs Cmds on their own goroutines, so the block
// is safe and does not stall the update loop. When sub is closed (connection
// ended) it delivers a single ircClosedMsg.
//
// A nil channel blocks forever, which is the correct no-op for tests that build
// a model without a live client: no events arrive and the Cmd never returns.
func waitForIRC(sub <-chan client.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-sub
		if !ok {
			return ircClosedMsg{}
		}
		return ircMsg{ev: ev}
	}
}

// routeEvent applies one inbound IRC event to model state. tui-core owns the
// routing decision (which buffer an event belongs to, whether it opens a new
// PM/channel window, what counts as activity); the view layer owns the actual
// line storage and formatting, reached through appendLine / appendInfo.
//
// Routing rules:
//   - Channel-targeted messages (PRIVMSG/NOTICE to a #channel) go to that
//     channel's buffer, opening it if missing.
//   - A PRIVMSG/NOTICE addressed to us (target == our nick) is a PM; it routes
//     to a buffer named after the sender, opened on demand.
//   - JOIN/PART/QUIT/NICK/TOPIC/numerics route to their relevant channel
//     buffer when one is identifiable, else to the server buffer.
//   - Anything we can't place lands in the server buffer (index 0).
func routeEvent(m model, ev client.Event) model {
	// A synthetic overflow marker (the client dropped events under backpressure)
	// carries a nil Message and a non-zero Dropped count. Surface the gap in the
	// server buffer rather than dereferencing the nil Message below.
	if ev.Dropped > 0 || ev.Message == nil {
		return appendInfo(m, m.buffers[0],
			pluralEvents(ev.Dropped))
	}
	switch ev.Command() {
	case irc.PRIVMSG, irc.NOTICE:
		return routeText(m, ev)
	case irc.JOIN, irc.PART, irc.QUIT, irc.NICK, irc.MODE, irc.TOPIC, irc.KICK:
		return routeMembership(m, ev)
	case irc.ACCOUNT, irc.AWAY, irc.CHGHOST, irc.SETNAME:
		// Pure metadata notifications: the client has already folded these into
		// member state (account/away/host), and the nicklist reflects them live.
		// Rendering a line per toggle would be noise, so they are intentionally
		// not written to any buffer.
		return m
	case irc.RPL_AWAY, irc.RPL_WHOISUSER, irc.RPL_WHOISSERVER,
		irc.RPL_WHOISOPERATOR, irc.RPL_WHOISIDLE, irc.RPL_ENDOFWHOIS,
		irc.RPL_WHOISCHANNELS, irc.RPL_WHOISACCOUNT, irc.RPL_WHOISACTUALLY,
		irc.RPL_WHOISSECURE:
		// WHOIS replies render in the buffer the user is looking at (the menu /
		// command was triggered there), not the distant server buffer.
		return appendLine(m, m.activeBuffer(), ev)
	default:
		// Registration burst, MOTD, numerics, errors: server buffer.
		return appendLine(m, m.buffers[0], ev)
	}
}

// routeText places a PRIVMSG/NOTICE in the right buffer (channel or PM),
// opening a PM buffer on demand, and marks unread/highlight activity on
// non-active buffers.
func routeText(m model, ev client.Event) model {
	target := ev.Param(0)
	self := ""
	if m.cli != nil {
		self = m.cli.Nick()
	}

	var b *Buffer
	var idx int
	if isChannel(target) {
		b, idx = m.ensureBuffer(target, BufferChannel)
	} else if equalFold(target, self) {
		// A message to us: the conversation buffer is the sender.
		b, idx = m.ensureBuffer(ev.Nick(), BufferPM)
	} else {
		// A message we sent (echo-message) or otherwise targeted elsewhere.
		b, idx = m.ensureBuffer(target, BufferPM)
	}

	m = appendLine(m, b, ev)
	m = markActivity(m, idx, mentionsSelf(ev.Text(), self))
	return m
}

// routeMembership routes JOIN/PART/QUIT/NICK/TOPIC/etc. to a channel buffer
// when one is identifiable, else the server buffer. State (membership) is
// already tracked by the client; here we only render the notice.
func routeMembership(m model, ev client.Event) model {
	target := ev.Param(0)
	if isChannel(target) {
		if b := m.buffer(target); b != nil {
			return appendLine(m, b, ev)
		}
	}
	return appendLine(m, m.buffers[0], ev)
}

// markActivity bumps the unread counter (and highlight flag) of the buffer at
// idx when it is not the active buffer. The active buffer is considered read.
func markActivity(m model, idx int, highlight bool) model {
	if idx == m.active || idx < 0 || idx >= len(m.buffers) {
		return m
	}
	b := m.buffers[idx]
	b.Unread++
	if highlight {
		b.Highlight = true
	}
	return m
}

// pluralEvents formats the dropped-events overflow notice.
func pluralEvents(n int) string {
	if n == 1 {
		return "lost 1 event (UI fell behind)"
	}
	return fmt.Sprintf("lost %d events (UI fell behind)", n)
}

// mentionsSelf reports whether text contains the nick as a highlight mention.
// The check is a case-insensitive substring for now; the view layer may apply a
// stricter word-boundary rule when it formats the line.
func mentionsSelf(text, nick string) bool {
	if nick == "" || text == "" {
		return false
	}
	return containsFold(text, nick)
}
