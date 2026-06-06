package client

import (
	"time"

	"github.com/exec/lurk/irc"
)

// Event carries a single inbound IRC message to handlers, along with a back
// reference to the Client so handlers can react (send messages, inspect state).
// It is the argument to every handler, both raw (On) and semantic (Handle*),
// and the element type of the Events stream.
type Event struct {
	// Client is the client that received the message.
	Client *Client

	// Message is the parsed inbound message that triggered the event.
	Message *irc.Message

	// recvTime is when the client received the message (monotonic wall clock at
	// dispatch). Time() prefers the server-time @time tag and falls back to this.
	recvTime time.Time

	// Dropped, when > 0, marks a synthetic overflow event: the Events stream
	// consumer fell behind and Dropped earlier events were discarded to avoid
	// blocking the client read loop. Such an event carries no Message (it is nil);
	// consumers should check Dropped before dereferencing Message. See Events.
	Dropped int

	// batchType is the type of the open BATCH this event belongs to (resolved
	// from its @batch tag at dispatch time), or "" if the event is not part of a
	// batch. Read via BatchType(); captured at dispatch because the batch may be
	// closed (and forgotten) before a consumer inspects the event.
	batchType string

	// affectedChannels lists the channels the event's subject was in at the time
	// the event was processed, captured before state tracking mutates membership.
	// It is set only for QUIT and NICK — messages that carry no channel of their
	// own yet need to be shown in every channel the user shared with us (by then
	// the member has already been removed/renamed). Read via Channels().
	affectedChannels []string
}

// Convenience accessors mirroring irc.Message so handlers can read common
// fields without reaching through Message every time. All are nil-safe on a
// synthetic overflow event (Dropped > 0, Message == nil): they return zero
// values.

// Command returns the message command (e.g. "PRIVMSG" or a numeric like "001").
func (e *Event) Command() string {
	if e.Message == nil {
		return ""
	}
	return e.Message.Command
}

// Nick returns the nickname of the message source.
func (e *Event) Nick() string {
	if e.Message == nil {
		return ""
	}
	return e.Message.Nick()
}

// Source returns the raw source prefix of the message.
func (e *Event) Source() string {
	if e.Message == nil {
		return ""
	}
	return e.Message.Source
}

// Param returns the i-th parameter, or "" if out of range.
func (e *Event) Param(i int) string {
	if e.Message == nil {
		return ""
	}
	return e.Message.Param(i)
}

// Text returns the trailing/last parameter, the conventional message body for
// PRIVMSG, NOTICE, PART, QUIT, etc. Empty if there are no parameters.
func (e *Event) Text() string {
	if e.Message == nil {
		return ""
	}
	n := len(e.Message.Params)
	if n == 0 {
		return ""
	}
	return e.Message.Params[n-1]
}

// Target returns the conventional target of the message: the first parameter
// for PRIVMSG/NOTICE/JOIN/PART/TOPIC/MODE/etc. (a channel or a nick). For a
// PRIVMSG/NOTICE addressed to the client itself, Target is the client's own
// nick — the TUI uses Nick() in that case to key a per-correspondent PM buffer.
// Returns "" when there are no parameters.
func (e *Event) Target() string {
	if e.Message == nil {
		return ""
	}
	return e.Message.Param(0)
}

// Time returns the time associated with the event. If the message carries an
// IRCv3 server-time tag (@time=...) and it parses as RFC3339(Nano), that is
// returned (the moment the server recorded the event); otherwise the time the
// client received the message is used. The result is always non-zero for a
// real event.
func (e *Event) Time() time.Time {
	if e.Message != nil {
		if ts, ok := e.Message.Tags["time"]; ok && ts != "" {
			// IRCv3 server-time is RFC3339 with millisecond precision and a 'Z'
			// zone, e.g. 2011-10-19T16:40:51.620Z. Accept RFC3339Nano too.
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
				return t
			}
		}
	}
	return e.recvTime
}

// BatchType returns the type of the BATCH this event is part of (e.g.
// "chathistory"), resolved from the event's @batch tag against the batches open
// when it was dispatched. It returns "" when the event is not part of any batch.
func (e *Event) BatchType() string {
	return e.batchType
}

// Channels returns the channels the event's subject shared with us when the
// event was processed. It is populated only for QUIT and NICK — events that name
// no channel of their own but should be reflected in every channel the user was
// in (the member is already gone from state by the time a consumer sees the
// event). It returns nil for every other command.
func (e *Event) Channels() []string {
	return e.affectedChannels
}

// WithChannels returns a copy of e annotated with the given affected channels
// (the ones Channels reports). The client sets these internally for QUIT/NICK;
// this builder lets tests and tools construct the same fan-out events directly.
func (e Event) WithChannels(channels []string) Event {
	e.affectedChannels = channels
	return e
}

// Handler is the signature for both raw and semantic event handlers. It is
// deliberately simple: a single *Event argument and no return.
type Handler func(*Event)

// dispatcher holds the registered handlers and fans out events. It is owned by
// the Client and accessed only from the client's run goroutine, so it needs no
// locking for dispatch; registration happens before Run (or is the caller's
// responsibility to serialize).
type dispatcher struct {
	// byCommand maps an upper-cased command/numeric to its handlers.
	byCommand map[string][]Handler
	// any holds handlers invoked for every message regardless of command.
	any []Handler
}

func newDispatcher() *dispatcher {
	return &dispatcher{byCommand: make(map[string][]Handler)}
}

// on registers h for the given command. The command is matched case-sensitively
// against the (upper-cased) Message.Command; callers should pass the canonical
// upper-case form or a numeric string.
func (d *dispatcher) on(command string, h Handler) {
	d.byCommand[command] = append(d.byCommand[command], h)
}

// onAny registers h to run for every dispatched message.
func (d *dispatcher) onAny(h Handler) {
	d.any = append(d.any, h)
}

// dispatch invokes the handlers registered for ev's command, then the
// catch-all handlers.
func (d *dispatcher) dispatch(ev *Event) {
	for _, h := range d.byCommand[ev.Message.Command] {
		h(ev)
	}
	for _, h := range d.any {
		h(ev)
	}
}

// Semantic event command keys. Semantic events are dispatched under these
// synthetic keys (which cannot collide with real IRC commands because real
// commands are upper-case ASCII or numerics). The typed Handle* helpers
// register under these keys; the run loop raises them after updating state.
const (
	evtConnected = "@connected"

	// evtReconnecting / evtReconnected are raised by the auto-reconnect supervisor
	// (reconnect.go) around a transparent re-dial after an unexpected disconnect,
	// so a UI can surface the gap. Their Message carries a human-readable note as
	// the trailing parameter.
	evtReconnecting = "@reconnecting"
	evtReconnected  = "@reconnected"
)

// Exported synthetic-event command keys, for stream consumers that route on
// Event.Command() (e.g. the TUI surfacing reconnect status).
const (
	EventReconnecting = evtReconnecting
	EventReconnected  = evtReconnected
)

// HandleReconnecting registers a handler fired when the connection dropped and
// the client is about to (or retrying to) re-dial.
func (c *Client) HandleReconnecting(h Handler) { c.disp.on(evtReconnecting, h) }

// HandleReconnected registers a handler fired once a re-dial has re-registered
// successfully and the prior channels have been re-joined.
func (c *Client) HandleReconnected(h Handler) { c.disp.on(evtReconnected, h) }

// On registers a handler for a raw IRC command or numeric. The command should
// be the canonical upper-case form (use the constants in package irc, e.g.
// irc.PRIVMSG, or a numeric like irc.RPL_WELCOME). Multiple handlers may be
// registered for the same command and run in registration order.
func (c *Client) On(command string, h Handler) {
	c.disp.on(command, h)
}

// OnAny registers a handler invoked for every inbound message, after any
// command-specific handlers.
func (c *Client) OnAny(h Handler) {
	c.disp.onAny(h)
}

// HandleConnected registers a handler fired once, when registration completes
// (RPL_WELCOME received). The event's Message is the 001 line.
func (c *Client) HandleConnected(h Handler) {
	c.disp.on(evtConnected, h)
}

// HandleMessage registers a handler for PRIVMSG.
func (c *Client) HandleMessage(h Handler) { c.disp.on(irc.PRIVMSG, h) }

// HandleJoin registers a handler for JOIN.
func (c *Client) HandleJoin(h Handler) { c.disp.on(irc.JOIN, h) }

// HandlePart registers a handler for PART.
func (c *Client) HandlePart(h Handler) { c.disp.on(irc.PART, h) }

// HandleQuit registers a handler for QUIT.
func (c *Client) HandleQuit(h Handler) { c.disp.on(irc.QUIT, h) }

// HandleNick registers a handler for NICK changes.
func (c *Client) HandleNick(h Handler) { c.disp.on(irc.NICK, h) }

// HandleMonitorOnline registers a handler fired when watched nicks come online
// (RPL_MONONLINE 730). The event's trailing parameter (Text()) is a
// comma-separated list of nick!user@host entries that are now online. State is
// updated before the handler is called, so MonitoredOnline() reflects the change.
func (c *Client) HandleMonitorOnline(h Handler) { c.disp.on(irc.RPL_MONONLINE, h) }

// HandleMonitorOffline registers a handler fired when watched nicks go offline
// (RPL_MONOFFLINE 731). The event's trailing parameter (Text()) is a
// comma-separated list of nick!user@host entries that are now offline. State is
// updated before the handler is called, so MonitoredOnline() reflects the change.
func (c *Client) HandleMonitorOffline(h Handler) { c.disp.on(irc.RPL_MONOFFLINE, h) }

// HandleStandardReply registers h to be called for every FAIL, WARN, and NOTE
// message the server sends (the standard-replies IRCv3 extension). The wire
// format is:
//
//	FAIL/WARN/NOTE <command> <code> [<context>...] :<description>
//
// Param(0) is the command the reply relates to (e.g. "JOIN"), Param(1) is the
// machine-readable error code (e.g. "CHANNEL_BANNED"), subsequent params are
// optional context tokens, and Text() is the human-readable description. A
// single handler h is registered for all three verbs; callers that need to
// distinguish severity can inspect Command() ("FAIL", "WARN", or "NOTE").
func (c *Client) HandleStandardReply(h Handler) {
	c.disp.on(irc.FAIL, h)
	c.disp.on(irc.WARN, h)
	c.disp.on(irc.NOTE, h)
}
