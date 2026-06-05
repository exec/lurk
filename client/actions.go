package client

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"lurk/irc"
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

// messageBudget returns the maximum text-parameter byte length for one command
// line to target so the full wire line "<command> <target> :<text>\r\n" stays
// within IRC's 512-byte limit. It is target-aware because a long STATUSMSG +
// large-CHANNELLEN target eats into the budget: a fixed cap that ignored the
// target could serialize past 512 and be truncated (or rejected with 417).
//
// We count only OUR sent line; the server's own "<nick>!<user>@<host>" source
// prefix it adds when relaying is not part of what we transmit, so it is not
// counted here.
func messageBudget(command, target string) int {
	const wireLimit = 512
	const crlf = 2
	// "<command> <target> :" + CRLF: command, one space, target, " :", CRLF.
	overhead := len(command) + 1 + len(target) + len(" :") + crlf
	budget := wireLimit - overhead
	if budget < 1 {
		budget = 1 // pathological (huge target): still emit minimal chunks
	}
	return budget
}

// Privmsg sends text to target (a channel or nick) as a PRIVMSG, splitting a
// message longer than a single wire line into multiple PRIVMSGs (on word
// boundaries where possible) so it is delivered in full rather than truncated.
// The split budget is target-aware (see messageBudget) so even a long target
// keeps every line within the 512-byte wire limit.
func (c *Client) Privmsg(target, text string) error {
	for _, chunk := range splitMessage(text, messageBudget(irc.PRIVMSG, target)) {
		if err := c.write(irc.PRIVMSG, target, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Notice sends a NOTICE to target, split the same way as Privmsg.
func (c *Client) Notice(target, text string) error {
	for _, chunk := range splitMessage(text, messageBudget(irc.NOTICE, target)) {
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
	budget := messageBudget(irc.PRIVMSG, target) - len(actionPrefix) - len(actionSuffix)
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
// treated as 1, the protocol minimum.
func (c *Client) ChatHistoryLatest(target string, limit int) error {
	if limit < 1 {
		limit = 1
	}
	return c.write(irc.CHATHISTORY, "LATEST", target, "*", strconv.Itoa(limit))
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
