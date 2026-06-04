package client

import (
	"strings"
	"time"

	"lurk/irc"
)

// ctcp_reply.go implements automatic responses to the standard CTCP *queries*
// (VERSION, PING, TIME, CLIENTINFO) that peers send as a PRIVMSG wrapped in the
// \x01 framing. Answering these is expected client behavior — a peer that
// /ctcp-s you for VERSION otherwise gets silence. ACTION (the /me form) is a
// message, not a query, and is never answered here.

// ctcpSupported is the set of CTCP queries this client answers, advertised in a
// CLIENTINFO reply.
const ctcpSupported = "ACTION CLIENTINFO PING TIME VERSION"

// maybeAnswerCTCP inspects an inbound PRIVMSG and, if it is a CTCP query
// addressed privately to us, sends the conventional NOTICE reply. It is a no-op
// for channel-directed messages, our own echo, ACTION, and unknown CTCP types.
func (c *Client) maybeAnswerCTCP(m *irc.Message) {
	body := m.Param(1)
	if len(body) < 2 || body[0] != 0x01 {
		return // not CTCP framed
	}
	payload := strings.Trim(body, "\x01")
	verb, arg, _ := strings.Cut(payload, " ")
	verb = strings.ToUpper(verb)
	if verb == "" || verb == "ACTION" {
		return
	}

	// Only answer queries directed at us personally; a channel CTCP is left alone
	// to avoid replying to the whole channel.
	self := c.Nick()
	if !strings.EqualFold(m.Param(0), self) {
		return
	}
	sender := m.Nick()
	if sender == "" || strings.EqualFold(sender, self) {
		return // no source, or our own echo-message copy
	}

	var reply string
	switch verb {
	case "VERSION":
		reply = "VERSION " + c.cfg.Version
	case "PING":
		// Echo the token verbatim (stripped of control bytes so it cannot inject a
		// second line or further CTCP framing into the NOTICE).
		reply = "PING " + stripCTCPArg(arg)
	case "TIME":
		reply = "TIME " + time.Now().Format("Mon, 02 Jan 2006 15:04:05 -0700")
	case "CLIENTINFO":
		reply = "CLIENTINFO " + ctcpSupported
	default:
		return // unknown query: silence is correct
	}
	_ = c.Notice(sender, "\x01"+reply+"\x01")
}

// stripCTCPArg removes control bytes (including CR/LF/NUL and the \x01 CTCP
// delimiter) from a peer-supplied CTCP argument before it is echoed back, so a
// crafted token cannot inject additional protocol lines or framing.
func stripCTCPArg(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}
