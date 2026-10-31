package client

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/exec/lurk/irc"
)

// Wait blocks until the connection has ended (the run loop exited), returning
// the transport's terminal error. It is the convenient way to keep a program
// alive after Connect while handlers process events.
func (c *Client) Wait() error {
	<-c.done
	return c.Err()
}

// Done returns a channel closed when the connection ends. Useful for selecting
// against other work.
func (c *Client) Done() <-chan struct{} { return c.done }

// Join sends a JOIN for one or more channels.
func (c *Client) Join(channels ...string) error {
	if len(channels) == 0 {
		return nil
	}
	return c.write(irc.JOIN, strings.Join(channels, ","))
}

// Part sends a PART for one or more channels with no reason. To include a part
// reason, use PartReason.
func (c *Client) Part(channels ...string) error {
	if len(channels) == 0 {
		return nil
	}
	return c.write(irc.PART, strings.Join(channels, ","))
}

// PartReason sends a PART for the given channels with a reason.
func (c *Client) PartReason(reason string, channels ...string) error {
	if len(channels) == 0 {
		return nil
	}
	return c.write(irc.PART, strings.Join(channels, ","), reason)
}

// Names sends a NAMES query for the given channels (or, with no arguments, a
// bare NAMES). The replies arrive as RPL_NAMREPLY (353) and RPL_ENDOFNAMES
// (366), which also refresh member tracking.
func (c *Client) Names(channels ...string) error {
	if len(channels) == 0 {
		return c.write(irc.NAMES)
	}
	return c.write(irc.NAMES, strings.Join(channels, ","))
}

// List sends a LIST query for the channel directory. With no arguments it
// requests the full list; given channels (or a server-specific filter like
// ">50") it narrows the query. Replies arrive as RPL_LIST (322) per channel,
// bracketed by the optional RPL_LISTSTART (321) and RPL_LISTEND (323).
func (c *Client) List(args ...string) error {
	if len(args) == 0 {
		return c.write(irc.LIST)
	}
	return c.write(irc.LIST, strings.Join(args, ","))
}

// messageBudget reserves the server-added sender prefix as well as the command
// and target, so the relayed line fits IRC's 512-byte limit.
func (c *Client) messageBudget(command, target string) int {
	const wireLimit = 512
	const crlf = 2
	c.mu.Lock()
	nickLen, userLen, hostLen := len(c.st.self), len(c.st.selfUser), len(c.st.selfHost)
	c.mu.Unlock()
	if nickLen == 0 {
		nickLen = len(c.cfg.Nick)
	}
	if userLen == 0 {
		userLen = max(10, len(c.cfg.User)+1) // Allow an ident '~' before learning our mask.
	}
	if hostLen == 0 {
		hostLen = 253 // Allow a full DNS hostname before our first self-prefixed event.
	}
	// ":nick!user@host <command> <target> :<text>\r\n".
	overhead := nickLen + userLen + hostLen + len(":!@ ") + len(command) + 1 + len(target) + len(" :") + crlf
	budget := wireLimit - overhead
	if budget < 1 {
		budget = 1 // pathological (huge target): still emit minimal chunks
	}
	return budget
}

// Privmsg sends text to target (a channel or nick) as a PRIVMSG, splitting a
// message longer than a single wire line into multiple PRIVMSGs (on word
// boundaries where possible) so it is delivered in full rather than truncated.
// The split budget accounts for the target and our server-confirmed sender
// prefix, keeping relayed lines within the 512-byte wire limit.
func (c *Client) Privmsg(target, text string) error {
	for _, chunk := range splitMessage(text, c.messageBudget(irc.PRIVMSG, target)) {
		if err := c.write(irc.PRIVMSG, target, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Notice sends a NOTICE to target, split the same way as Privmsg.
func (c *Client) Notice(target, text string) error {
	for _, chunk := range splitMessage(text, c.messageBudget(irc.NOTICE, target)) {
		if err := c.write(irc.NOTICE, target, chunk); err != nil {
			return err
		}
	}
	return nil
}

// actionPrefix and actionSuffix are the CTCP ACTION framing that wraps the "/me"
// text inside a PRIVMSG. They are sized into the split budget so each emitted
// chunk — framing included — stays within the wire limit.
const (
	actionPrefix = "\x01ACTION "
	actionSuffix = "\x01"
)

// Action sends a CTCP ACTION to target (a channel or nick) — the "/me" form,
// conventionally rendered as "* nick <text>". It wraps text in the CTCP framing
// (\x01ACTION …\x01) and sends it as a PRIVMSG. A long action is split into
// multiple PRIVMSGs, each re-wrapped in its own \x01ACTION …\x01 framing, so no
// chunk overflows 512 bytes (which would otherwise truncate the line and drop
// the closing \x01). The split budget is the target-aware PRIVMSG budget minus
// the framing overhead, so the framed wire line still fits.
func (c *Client) Action(target, text string) error {
	budget := c.messageBudget(irc.PRIVMSG, target) - len(actionPrefix) - len(actionSuffix)
	if budget < 1 {
		budget = 1 // pathological (huge target): still emit minimal chunks
	}
	for _, chunk := range splitMessage(text, budget) {
		if err := c.write(irc.PRIVMSG, target, actionPrefix+chunk+actionSuffix); err != nil {
			return err
		}
	}
	return nil
}

// splitMessage breaks s into pieces each at most max bytes, preferring to cut at
// a space and never splitting a UTF-8 rune. A message already within the limit
// is returned as a single piece (including the empty string).
func splitMessage(s string, max int) []string {
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		cut := max
		if i := strings.LastIndexByte(s[:max], ' '); i > 0 {
			cut = i // break at the last space within the budget
		} else {
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut-- // back up to a rune boundary on a word with no spaces
			}
			if cut == 0 {
				cut = max // pathological (single huge rune run): hard cut
			}
		}
		out = append(out, strings.TrimRight(s[:cut], " "))
		s = strings.TrimLeft(s[cut:], " ")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// SetTopic changes channel's topic. An empty topic clears the channel topic (the
// server interprets the trailing empty parameter, "TOPIC <channel> :", as a
// removal). To read the current topic without changing it, use RequestTopic.
func (c *Client) SetTopic(channel, topic string) error {
	return c.write(irc.TOPIC, channel, topic)
}

// RequestTopic asks the server for channel's current topic by sending a bare
// "TOPIC <channel>". The reply arrives as RPL_TOPIC (332) or RPL_NOTOPIC (331)
// and refreshes the tracked topic (see Topic).
func (c *Client) RequestTopic(channel string) error {
	return c.write(irc.TOPIC, channel)
}

// Away marks the client as away with the given reason: servers then send an
// automatic RPL_AWAY (301) to anyone who messages the client, and (under
// away-notify) broadcast an AWAY to shared channels. An empty reason clears the
// away status instead, marking the client back — this mirrors the IRC AWAY
// command, whose bare, parameterless form means "no longer away". Use Back for a
// self-documenting way to clear it.
func (c *Client) Away(reason string) error {
	if reason == "" {
		return c.write(irc.AWAY)
	}
	return c.write(irc.AWAY, reason)
}

// Back clears the client's away status (equivalent to Away("")).
func (c *Client) Back() error {
	return c.write(irc.AWAY)
}

// SetNick requests a nickname change.
func (c *Client) SetNick(nick string) error {
	return c.write(irc.NICK, nick)
}

// Quit sends a QUIT with an optional reason and lets the server close the
// connection. It also stops the reconnect supervisor (a deliberate quit must not
// trigger a re-dial); the caller may still call Close to tear down locally.
//
// The QUIT is written BEFORE the supervisor is stopped: closing c.stop wakes the
// supervisor, which tears the transport down, so stopping first would race (and
// usually lose) the QUIT write. conn's graceful Close drains the outbound queue,
// so the already-enqueued QUIT still flushes to the server before teardown.
func (c *Client) Quit(reason string) error {
	var err error
	if reason == "" {
		err = c.write(irc.QUIT)
	} else {
		err = c.write(irc.QUIT, reason)
	}
	c.stopOnce.Do(func() { close(c.stop) })
	return err
}

// Whois sends a WHOIS query for nick. The reply arrives as the numeric burst
// RPL_WHOISUSER (311) … RPL_ENDOFWHOIS (318), which is delivered through the
// normal event stream / handlers for the caller to display.
func (c *Client) Whois(nick string) error {
	return c.write(irc.WHOIS, nick)
}

// Kick removes nick from channel with an optional reason (requires channel
// operator privileges on the server).
func (c *Client) Kick(channel, nick, reason string) error {
	if reason == "" {
		return c.write(irc.KICK, channel, nick)
	}
	return c.write(irc.KICK, channel, nick, reason)
}

// Invite invites nick to channel.
func (c *Client) Invite(nick, channel string) error {
	return c.write(irc.INVITE, nick, channel)
}

// ChannelMode applies a MODE change to channel. modes is the mode string (e.g.
// "+o" or "-v") and args are its per-mode arguments (e.g. the target nick for a
// prefix mode). For example, ChannelMode("#chan", "+o", "alice").
func (c *Client) ChannelMode(channel, modes string, args ...string) error {
	params := make([]string, 0, 2+len(args))
	params = append(params, channel, modes)
	params = append(params, args...)
	return c.write(irc.MODE, params...)
}

// ChatHistoryLatest requests the most recent history for target via the
// draft/chathistory extension: "CHATHISTORY LATEST <target> * <limit>". The '*'
// selector means "from the latest message backwards"; limit caps the number of
// messages returned. The server answers with a "chathistory" BATCH of past
// PRIVMSG/NOTICE lines (each carrying an @time tag). A non-positive limit is
// treated as 1, the protocol minimum. Limits are capped by the server's
// CHATHISTORY ISUPPORT token when advertised.
func (c *Client) ChatHistoryLatest(target string, limit int) error {
	return c.write(irc.CHATHISTORY, "LATEST", target, "*", strconv.Itoa(c.historyLimit(limit)))
}

// ChatHistoryBefore requests a page older than ref (msgid=… or timestamp=…).
// Replies arrive as a chathistory BATCH on Events.
func (c *Client) ChatHistoryBefore(target, ref string, limit int) error {
	return c.write(irc.CHATHISTORY, "BEFORE", target, ref, strconv.Itoa(c.historyLimit(limit)))
}

// ChatHistoryAfter requests a page newer than ref (msgid=… or timestamp=…).
// Replies arrive as a chathistory BATCH on Events.
func (c *Client) ChatHistoryAfter(target, ref string, limit int) error {
	return c.write(irc.CHATHISTORY, "AFTER", target, ref, strconv.Itoa(c.historyLimit(limit)))
}

// ChatHistoryTargets discovers conversations whose latest message falls between
// from and to. Use from > to for newest-first pagination. Replies arrive in a
// draft/chathistory-targets BATCH on Events.
func (c *Client) ChatHistoryTargets(from, to time.Time, limit int) error {
	return c.write(irc.CHATHISTORY, "TARGETS", "timestamp="+from.UTC().Format("2006-01-02T15:04:05.000Z"),
		"timestamp="+to.UTC().Format("2006-01-02T15:04:05.000Z"), strconv.Itoa(c.historyLimit(limit)))
}

func (c *Client) historyLimit(limit int) int {
	if limit < 1 {
		limit = 1
	}
	if value, ok := c.ISupport("CHATHISTORY"); ok {
		if max, err := strconv.Atoi(value); err == nil && max > 0 && limit > max {
			limit = max
		}
	}
	return limit
}

// SendTagged sends a command with client-only message tags. Tag keys keep their
// leading '+' (e.g. "+typing"); values are escaped by Message.Serialize. It is
// the tagged counterpart of the internal write path and underpins typing
// notifications and other client-tag features.
func (c *Client) SendTagged(tags map[string]string, command string, params ...string) error {
	return c.WriteMessage(&irc.Message{Tags: irc.Tags(tags), Command: command, Params: params})
}

// Typing sends a typing notification for target (a channel or nick) using the
// "+typing" client tag on a TAGMSG. state is one of "active", "paused", or
// "done". It is a no-op (returning nil) unless the message-tags capability is
// enabled, since the tag would otherwise be stripped by the server.
func (c *Client) Typing(target, state string) error {
	if !c.CapEnabled("message-tags") {
		return nil
	}
	return c.SendTagged(map[string]string{"+typing": state}, irc.TAGMSG, target)
}

// SelfPrefixes returns the membership prefix symbols (e.g. "@", "~@", "") the
// client itself holds in channel, or "" if not a member. Useful for deciding
// whether operator-only actions (kick, mode) are available. The self nick is
// matched through the server's case mapping (the same fold used for member keys),
// not Unicode case-folding, so it is correct under any CASEMAPPING.
func (c *Client) SelfPrefixes(channel string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs := c.st.channel(channel)
	if cs == nil {
		return ""
	}
	if m, ok := cs.members[c.st.foldKey(c.st.self)]; ok {
		return m.Prefixes
	}
	return ""
}

// MonitorLimit returns the server-advertised MONITOR limit (from ISUPPORT
// MONITOR=N), or 0 if the server did not advertise one. A return of 0 should be
// treated as "no limit stated", not as "zero allowed".
func (c *Client) MonitorLimit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.st.feat.Get("MONITOR")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Monitor asks the server to watch nicks for online/offline changes (MONITOR +).
// Notifications arrive as RPL_MONONLINE (730) and RPL_MONOFFLINE (731) events;
// use HandleMonitorOnline / HandleMonitorOffline to subscribe. The nicks are also
// recorded in local state so MonitoredOnline and MonitorList reflect them. It is
// a no-op when monitor was not negotiated (the server ignores it regardless).
func (c *Client) Monitor(nicks ...string) error {
	if len(nicks) == 0 {
		return nil
	}
	c.mu.Lock()
	for _, nick := range nicks {
		c.st.addMonitor(nick)
	}
	c.mu.Unlock()
	return c.write(irc.MONITOR, "+", strings.Join(nicks, ","))
}

// Unmonitor removes nicks from the server watch list (MONITOR -) and from local
// state.
func (c *Client) Unmonitor(nicks ...string) error {
	if len(nicks) == 0 {
		return nil
	}
	c.mu.Lock()
	for _, nick := range nicks {
		c.st.removeMonitor(nick)
	}
	c.mu.Unlock()
	return c.write(irc.MONITOR, "-", strings.Join(nicks, ","))
}

// UnmonitorAll clears the entire server watch list (MONITOR C) and resets the
// local monitored and online sets.
func (c *Client) UnmonitorAll() error {
	c.mu.Lock()
	c.st.clearMonitor()
	c.mu.Unlock()
	return c.write(irc.MONITOR, "C")
}

// MonitorList requests the server to send back the current watch list (MONITOR L).
// The server replies with RPL_MONLIST (732) lines and a RPL_ENDOFMONLIST (733).
func (c *Client) MonitorList() error {
	return c.write(irc.MONITOR, "L")
}

// MonitoredNicks returns a sorted snapshot of the display-case nicks currently
// being monitored (those added via Monitor that have not since been removed).
func (c *Client) MonitoredNicks() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.monitoredNicks()
}

// MonitoredOnline reports whether nick was last reported online by the server
// (via RPL_MONONLINE 730). It returns false for nicks not in the watch list, or
// for nicks that are watched but whose current status is offline or unknown.
func (c *Client) MonitoredOnline(nick string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.isMonitoredOnline(nick)
}
