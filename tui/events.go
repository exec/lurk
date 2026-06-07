package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// typingTickMsg fires when a remote typing indication may have expired, forcing a
// repaint (and a prune) so "X is typing…" clears on time even with no other
// traffic — e.g. while the editor is blurred onto the nicklist, where the cursor
// blink isn't repainting.
type typingTickMsg struct{}

// typingExpiryTick delivers a typingTickMsg after d (clamped to ≥ 0).
func typingExpiryTick(d time.Duration) tea.Cmd {
	if d < 0 {
		d = 0
	}
	return tea.Tick(d, func(time.Time) tea.Msg { return typingTickMsg{} })
}

// ensureTypingTick schedules an expiry tick when remote typers exist and none is
// already pending. It self-reschedules from the typingTickMsg handler, so the
// tick runs only while someone is typing and stops once everyone is done.
// Returns the model (with the in-flight flag set) and the command to batch, or a
// nil command when nothing needs scheduling.
func (m model) ensureTypingTick(now time.Time) (model, tea.Cmd) {
	if m.typingTicking {
		return m, nil
	}
	d, any := m.nextTypingDeadline(now)
	if !any {
		return m, nil
	}
	m.typingTicking = true
	return m, typingExpiryTick(d)
}

// bellCmd rings the terminal bell by writing a BEL to stderr. stderr is not the
// stream Bubble Tea's renderer writes to, so the control byte cannot corrupt the
// on-screen frame; if stderr is not a terminal the byte is harmlessly discarded.
func bellCmd() tea.Cmd {
	return func() tea.Msg {
		fmt.Fprint(os.Stderr, "\a")
		return nil
	}
}

// This file bridges the client's event stream into the Bubble Tea update loop.
// The design follows the idiomatic "channel + re-subscribing Cmd" subscription
// (a pattern from Bubble Tea's realtime example): the model holds the channel,
// not the *Program, so it stays pure and testable, and exactly one waitForIRC
// Cmd is in flight at a time. After handling each ircBatchMsg, Update MUST
// re-issue waitForIRC or the subscription dies — see docs/ARCHITECTURE-TUI.md,
// "The event bridge".

// ircMsg wraps a single inbound client.Event with the network it arrived on, for
// delivery into Update. It is a distinct type (not a bare client.Event) so the
// Update type switch can match it unambiguously alongside Bubble Tea's own types.
// It remains the unit of synthetic event injection in tests; the live bridge
// delivers ircBatchMsg.
type ircMsg struct {
	net *network
	ev  client.Event
}

// maxEventBatch bounds how many events a single waitForIRC pass drains. A burst
// (a large /list reply, a CHATHISTORY replay, a busy channel) is delivered to
// Update in batches of up to this many events — one render per batch instead of
// one render per event — so the consumer keeps pace with the server and the
// (lossy, drop-oldest) Events buffer does not overflow. The bound caps the work
// a single Update cycle does so a huge flood can't stall the UI in one pass.
const maxEventBatch = 1024

// ircBatchMsg carries a run of events drained from a network's stream in one
// pass. Batch delivery is the throughput fix for large servers: applying a burst
// with a single render keeps the consumer ahead of the producer so events are
// not dropped. closed is set when the stream ended mid-drain (the events that
// were drained first are still applied, then the network is dropped).
type ircBatchMsg struct {
	net    *network
	evs    []client.Event
	closed bool
}

// ircClosedMsg is delivered once a network's event stream closes (its connection
// ended for good). Update drops that network; when none remain, it quits.
type ircClosedMsg struct {
	net *network
}

// waitForIRC returns a Cmd that blocks on the next event from net's stream and
// delivers it as an ircMsg tagged with net. Bubble Tea runs Cmds on their own
// goroutines, so the block is safe and does not stall the update loop. When the
// stream is closed it delivers a single ircClosedMsg.
//
// A nil channel blocks forever, the correct no-op for tests that build a model
// without a live client: no events arrive and the Cmd never returns.
func waitForIRC(net *network) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-net.sub
		if !ok {
			return ircClosedMsg{net: net}
		}
		// Block for the first event, then opportunistically drain everything
		// else already buffered (without blocking) into one batch. This is what
		// lets the UI keep up with a flood: instead of one render per event, the
		// whole burst is applied in a single Update + render.
		evs := make([]client.Event, 1, maxEventBatch)
		evs[0] = ev
		for len(evs) < maxEventBatch {
			select {
			case ev, ok := <-net.sub:
				if !ok {
					return ircBatchMsg{net: net, evs: evs, closed: true}
				}
				evs = append(evs, ev)
			default:
				return ircBatchMsg{net: net, evs: evs}
			}
		}
		return ircBatchMsg{net: net, evs: evs}
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
	return routeEventOn(m, m.activeNet(), ev)
}

// routeEventOn applies an event that arrived on a specific network, routing into
// that network's buffers and resolving self/nick state through its client. A nil
// net falls back to the active network (the path tests using routeEvent take).
func routeEventOn(m model, net *network, ev client.Event) model {
	if net == nil {
		net = m.activeNet()
	}
	// Drop events for a network that is no longer connected (e.g. an ircMsg that
	// was already queued when removeNetwork dropped its network). Routing it would
	// either land in net0's server buffer (a cross-network leak) or re-create a
	// buffer back-pointing at the dead network.
	if !m.hasNetwork(net) {
		return m
	}
	// A synthetic overflow marker (the client dropped events under backpressure)
	// carries a nil Message and a non-zero Dropped count. Surface the gap in the
	// network's server buffer rather than dereferencing the nil Message below.
	if ev.Dropped > 0 || ev.Message == nil {
		return appendInfo(m, m.serverBuffer(net), pluralEvents(ev.Dropped))
	}
	switch ev.Command() {
	case client.EventReconnecting, client.EventReconnected:
		// Connection-status notices from the reconnect supervisor: show them where
		// the user is looking (when on this network), else its server buffer.
		return appendInfo(m, m.targetBuffer(net), ev.Text())
	case irc.PRIVMSG, irc.NOTICE:
		return routeText(m, net, ev)
	case irc.JOIN, irc.PART, irc.QUIT, irc.NICK, irc.MODE, irc.TOPIC, irc.KICK:
		return routeMembership(m, net, ev)
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
		return routeTagmsg(m, net, ev)
	case irc.FAIL, irc.WARN, irc.NOTE:
		// Standard replies render where the user is looking (when on this network),
		// else the network's server buffer.
		b := m.targetBuffer(net)
		b.addLine(defaultTheme.formatStandardReply(ev))
		b.refresh()
		return m
	case irc.RPL_AWAY, irc.RPL_WHOISUSER, irc.RPL_WHOISSERVER,
		irc.RPL_WHOISOPERATOR, irc.RPL_WHOISIDLE, irc.RPL_ENDOFWHOIS,
		irc.RPL_WHOISCHANNELS, irc.RPL_WHOISACCOUNT, irc.RPL_WHOISACTUALLY,
		irc.RPL_WHOISSECURE:
		// WHOIS replies render in the buffer the user is looking at (the menu /
		// command was triggered there), not the distant server buffer.
		return appendLine(m, m.targetBuffer(net), ev)
	case irc.RPL_LISTSTART, irc.RPL_LIST, irc.RPL_LISTEND:
		// LIST replies feed the channel-directory modal (channellist.go); when the
		// modal is not driving the request they fall back to the active buffer.
		return routeListReply(m, net, ev)
	default:
		// Registration burst, MOTD, numerics, errors: the network's server buffer.
		return appendLine(m, m.serverBuffer(net), ev)
	}
}

// routeText places a PRIVMSG/NOTICE in the right buffer (channel or PM),
// opening a PM buffer on demand, and marks unread/highlight activity on
// non-active buffers.
func routeText(m model, net *network, ev client.Event) model {
	// Drop messages from an ignored sender entirely (their own echo aside).
	if m.isIgnored(ev.Nick()) {
		return m
	}
	target := ev.Param(0)
	self := ""
	statusMsg := ""
	if net != nil && net.cli != nil {
		self = net.cli.Nick()
		statusMsg = net.cli.StatusMsg()
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
	b := m.netBuffer(net, name)
	if b == nil {
		if len(m.buffers) < maxAutoBuffers {
			b, _ = m.ensureBufferIn(net, name, kind)
		} else {
			b = m.serverBuffer(net)
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
func routeTagmsg(m model, net *network, ev client.Event) model {
	state := ev.Message.Tags.Get("+typing")
	if state == "" {
		return m
	}
	self := net.nick()
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
func routeMembership(m model, net *network, ev client.Event) model {
	// QUIT and NICK name no channel of their own. The client attaches the
	// channels the subject was in (captured before the membership change) so the
	// notice lands in each of those channel buffers — not the distant server
	// buffer, where quits used to wrongly appear.
	if chans := ev.Channels(); len(chans) > 0 {
		shown := false
		for _, ch := range chans {
			if b := m.netBuffer(net, ch); b != nil {
				m = appendLine(m, b, ev)
				shown = true
			}
		}
		if shown {
			return m
		}
		// No buffer open for any shared channel: fall back to the server buffer.
		return appendLine(m, m.serverBuffer(net), ev)
	}

	target := ev.Param(0)
	if isChannel(target) {
		if b := m.netBuffer(net, target); b != nil {
			return appendLine(m, b, ev)
		}
	}
	return appendLine(m, m.serverBuffer(net), ev)
}

// pluralEvents formats the dropped-events overflow notice.
func pluralEvents(n int) string {
	if n == 1 {
		return "lost 1 event (UI fell behind)"
	}
	return fmt.Sprintf("lost %d events (UI fell behind)", n)
}
