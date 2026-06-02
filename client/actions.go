package client

import (
	"strings"

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

// Privmsg sends a PRIVMSG to target (a channel or nick).
func (c *Client) Privmsg(target, text string) error {
	return c.write(irc.PRIVMSG, target, text)
}

// Notice sends a NOTICE to target.
func (c *Client) Notice(target, text string) error {
	return c.write(irc.NOTICE, target, text)
}

// SetNick requests a nickname change.
func (c *Client) SetNick(nick string) error {
	return c.write(irc.NICK, nick)
}

// Quit sends a QUIT with an optional reason and lets the server close the
// connection. The caller may also call Close to tear down locally.
func (c *Client) Quit(reason string) error {
	if reason == "" {
		return c.write(irc.QUIT)
	}
	return c.write(irc.QUIT, reason)
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

// SelfPrefixes returns the membership prefix symbols (e.g. "@", "~@", "") the
// client itself holds in channel, or "" if not a member. Useful for deciding
// whether operator-only actions (kick, mode) are available.
func (c *Client) SelfPrefixes(channel string) string {
	self := c.Nick()
	for _, m := range c.Members(channel) {
		if strings.EqualFold(m.Nick, self) {
			return m.Prefixes
		}
	}
	return ""
}
