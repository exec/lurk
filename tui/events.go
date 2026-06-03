package tui

import (
	"fmt"
	"strings"

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
	case irc.BATCH:
		// Batch open/close markers are control messages with no body to show; the
		// batch's effect is already applied to the inner messages via
		// Event.BatchType(). Rendering them would dump "BATCH +ref …" noise into
		// the server buffer on every chathistory fetch.
		return m
	case irc.TAGMSG:
		// Client-tag-only messages (e.g. +typing): no body to render. Typing state
		// is tracked separately and surfaced in the status bar.
		return routeTagmsg(m, ev)
	case irc.FAIL, irc.WARN, irc.NOTE:
		// Standard replies render where the user is looking (the command that
		// triggered them was issued from the active buffer).
		b := m.activeBuffer()
		b.addLine(defaultTheme.formatStandardReply(ev))
		b.refresh()
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
	statusMsg := ""
	if m.cli != nil {
		self = m.cli.Nick()
		statusMsg = m.cli.StatusMsg()
	}
	// A STATUSMSG target ("@#chan", "+#chan") addresses a subset of a channel's
	// members; route it to the channel's own buffer, not a phantom "@#chan" window.
	target = stripStatusPrefix(target, statusMsg)

	// Resolve which window this message belongs to: the channel for channel
	// traffic; the sender for a message addressed to us (a PM, whose buffer is the
	// peer); otherwise the target (our own echo, or traffic aimed elsewhere).
	name, kind := target, BufferChannel
	if !isChannel(target) {
		kind = BufferPM
		if equalFold(target, self) {
			name = ev.Nick()
		}
	}

	// Never let inbound traffic open an unbounded number of windows: a hostile
	// server can address messages from endless distinct channels/nicks. An
	// already-open buffer is always reused; a new one is created only while under
	// the maxAutoBuffers ceiling, past which the message lands in the server
	// buffer rather than growing the list without bound.
	b := m.buffer(name)
	if b == nil {
		if len(m.buffers) < maxAutoBuffers {
			b, _ = m.ensureBuffer(name, kind)
		} else {
			b = m.buffers[0]
		}
	}

	// A message from a user ends any "typing…" indication they had in this buffer.
	m = m.clearTyping(typingBufferKey(target, ev.Nick(), self), ev.Nick())

	// appendLine is the single place that records unread/highlight for non-active
	// buffers (and skips replayed chathistory backlog), so there is no separate
	// activity bump here — a prior duplicate caused background messages to count
	// toward unread twice.
	m = appendLine(m, b, ev)
	return m
}

// stripStatusPrefix removes a leading run of STATUSMSG prefix characters from a
// channel target ("@#chan" -> "#chan") when the remainder is itself a channel,
// so a status-message PRIVMSG/NOTICE routes to the channel's buffer. statusMsg is
// the server's STATUSMSG set (e.g. "@+"); an empty set disables stripping. A
// target whose stripped form is not a channel (e.g. a modeless "+chan", where the
// leading '+' is the channel type, not a status prefix) is returned unchanged.
func stripStatusPrefix(target, statusMsg string) string {
	if statusMsg == "" || target == "" {
		return target
	}
	stripped := strings.TrimLeft(target, statusMsg)
	if stripped != target && isChannel(stripped) {
		return stripped
	}
	return target
}

// routeTagmsg applies a TAGMSG carrying the +typing client tag to the model's
// typing state. "active" (or "paused") marks the sender typing in the relevant
// buffer; "done" clears it. TAGMSGs without a +typing tag are ignored (there is
// nothing to display).
func routeTagmsg(m model, ev client.Event) model {
	state := ev.Message.Tags.Get("+typing")
	if state == "" {
		return m
	}
	self := ""
	if m.cli != nil {
		self = m.cli.Nick()
	}
	// Never show our own typing: with echo-message the server reflects our
	// +typing TAGMSGs back to us, and we don't indicate to ourselves that we're
	// typing.
	if equalFold(ev.Nick(), self) {
		return m
	}
	key := typingBufferKey(ev.Param(0), ev.Nick(), self)
	if state == "active" {
		return m.noteTyping(key, ev.Nick())
	}
	// "paused" and "done" both stop showing the indicator.
	return m.clearTyping(key, ev.Nick())
}

// typingBufferKey returns the ASCII-folded key of the buffer a typing/message
// event belongs to: the channel target for channel traffic, or the sender's
// nick for a message addressed to us (a PM, whose buffer is the peer).
func typingBufferKey(target, sender, self string) string {
	if isChannel(target) || !equalFold(target, self) {
		return asciiLower(target)
	}
	return asciiLower(sender)
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

// pluralEvents formats the dropped-events overflow notice.
func pluralEvents(n int) string {
	if n == 1 {
		return "lost 1 event (UI fell behind)"
	}
	return fmt.Sprintf("lost %d events (UI fell behind)", n)
}
