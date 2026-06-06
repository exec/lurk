package client

import (
	"strings"
	"sync"
	"time"

	"github.com/exec/lurk/irc"
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

	// Rate-limit the auto-reply: a peer spraying private CTCP queries must not be
	// able to turn us into a 1:1 NOTICE amplifier (which would get us throttled or
	// killed by our own server). Drop the reply when the bucket is empty.
	if !c.ctcpRL.allow(time.Now()) {
		return
	}
	_ = c.Notice(sender, "\x01"+reply+"\x01")
}

// CTCP auto-reply rate limits: a burst of ctcpBurst replies is allowed, then
// replies refill at one per ctcpRefill, shared across all peers.
const (
	ctcpBurst  = 5
	ctcpRefill = 2 * time.Second
)

// ctcpLimiter is a token bucket bounding the rate of automatic CTCP replies. The
// zero value is ready to use and starts full on first use. It is safe for
// concurrent use, though in practice only the run goroutine calls allow.
type ctcpLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

// allow reports whether a reply may be sent at time now, consuming one token if
// so. now is a parameter (rather than a time.Now call) so tests can drive the
// bucket deterministically.
func (l *ctcpLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		l.tokens = ctcpBurst // first use: start with a full burst
	} else {
		l.tokens += now.Sub(l.last).Seconds() / ctcpRefill.Seconds()
		if l.tokens > ctcpBurst {
			l.tokens = ctcpBurst
		}
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
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
