// relay.go implements the client→upstream relay for bound sessions (Phase 6b).
//
// When a session is bound to a netid and has completed registration, commands
// that originate from the attached client are forwarded to the upstream
// client.Client via Manager.Client(netid).WriteMessage(msg).
//
// # Forwarding rules
//
// Forwarded (transparent relay):
//
//	PRIVMSG, NOTICE, JOIN, PART, MODE, TOPIC, NAMES, WHO, WHOIS, KICK,
//	INVITE, AWAY, NICK, TAGMSG, LIST, WHO, MOTD, and any other verb not
//	explicitly intercepted.
//
// NOT forwarded (handled locally or control-only):
//
//	CAP, AUTHENTICATE, BOUNCER — registration and control verbs handled by
//	the server state machine.
//	PING / PONG — handled locally by handlePING; a PING from the client gets
//	a PONG from lurkd, keeping the keepalive loop working without involving
//	the upstream.
//	QUIT — handled locally (closes the client's connection, not the upstream).
//	CHATHISTORY — served from the local backlog store.
//
// All other verbs arriving on a bound session are forwarded transparently.
// This makes lurkd a transparent proxy for IRC commands it does not understand,
// which is the correct bouncer behaviour (e.g. WHOX, DCC, custom extensions).
//
// # Labeled-response [B#11]
//
// When the client sends a PRIVMSG (or NOTICE) with a @label tag, the label is
// recorded in the per-session pending-label FIFO before forwarding the message
// to the upstream (with the @label stripped — we do not forward the label tag
// to the upstream because labeled-response is negotiated separately per
// connection). When the upstream echoes the message back via echo-message, the
// fanout path checks the FIFO and re-attaches the label to that session's copy.
// Other sessions receive the echo without any label.
//
// If the upstream does not support echo-message, the message is forwarded but
// the echo is never seen; the pending-label entry eventually ages out silently
// (the FIFO is bounded by pendingLabelFIFOSize, oldest-eviction on overflow).
//
// # Concurrency
//
// relayToUpstream is called on the session's own goroutine (the run loop). The
// pendingLabels FIFO is also accessed by the fanout goroutine, so labelMu is
// held during both the push (here) and the pop (fanout.go). No blocking I/O is
// performed while holding labelMu — the lock is released before WriteMessage.
package server

import (
	"strings"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// relayToUpstream forwards msg from the bound client to the upstream.
// It is only called for registered, bound (netid != 0) sessions on commands
// that are not intercepted by the local handlers (dispatch calls this from its
// default branch for registered bound sessions).
//
// CAP, AUTHENTICATE, BOUNCER, PING, QUIT, CHATHISTORY are never passed here
// (handled in dispatch before reaching the default branch).
func (s *session) relayToUpstream(msg *irc.Message) error {
	if s.srv.mgr == nil {
		// No manager configured (test or degraded mode): silently drop.
		return nil
	}
	cc, ok := s.srv.mgr.Client(s.netid)
	if !ok {
		// The upstream for this netid is not (yet) running or has been removed.
		// Silently drop — do not error, as this would disconnect the client.
		return nil
	}

	// For PRIVMSG/NOTICE with a @label, record the label in the pending FIFO
	// before forwarding (so fanout can match the echo back to this session).
	// We forward WITHOUT the label tag because labeled-response is negotiated
	// per-connection and the upstream has its own labeled-response negotiation.
	cmd := strings.ToUpper(msg.Command)
	outMsg := msg
	if (cmd == "PRIVMSG" || cmd == "NOTICE") && msg.Tags["label"] != "" {
		label := msg.Tags["label"]
		target := msg.Param(0)
		text := ""
		if len(msg.Params) > 0 {
			text = client.SanitizeForRelay(msg.Params[len(msg.Params)-1])
		}

		// Push to FIFO (guarded by labelMu). Evict oldest if full.
		s.labelMu.Lock()
		if len(s.pendingLabels) >= pendingLabelFIFOSize {
			// Drop oldest: eviction, not an error.
			s.pendingLabels = s.pendingLabels[1:]
		}
		s.pendingLabels = append(s.pendingLabels, pendingLabel{
			label:  label,
			target: target,
			text:   text,
		})
		s.labelMu.Unlock()

		// Strip the @label tag before forwarding to the upstream.
		newTags := make(irc.Tags, len(msg.Tags))
		for k, v := range msg.Tags {
			if k != "label" {
				newTags[k] = v
			}
		}
		outMsg = &irc.Message{
			Tags:    newTags,
			Source:  msg.Source,
			Command: msg.Command,
			Params:  msg.Params,
		}
	}

	// Forward to upstream. Errors from WriteMessage (e.g. upstream disconnected)
	// are non-fatal for the bound session — the upstream reconnect supervisor will
	// re-establish the connection. We log but do not disconnect the client.
	if err := cc.WriteMessage(outMsg); err != nil {
		// Log the relay error. Do not return it — returning an error from dispatch
		// would tear down the session, which is too aggressive for a transient
		// upstream write failure.
		_ = s.sendFail("*", "UPSTREAM_ERROR", "upstream write failed: "+err.Error())
	}
	return nil
}
