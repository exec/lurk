// fanout.go implements the Server-as-Sink and per-network fan-out for Phase 6b.
//
// # Overview
//
// Server implements server.Sink: its Ingest method both stores events (via the
// backlog store) AND fans them out live to every session currently bound to that
// netid. This makes lurkd functional end-to-end: upstream events flow through
// OnAny → Manager → Server.Ingest → store + fanout → attached clients.
//
// # Session registry
//
// boundMu guards boundSessions, a map[int]map[*session]struct{} where the key
// is the netid the session is bound to. The control-session registry (netid==0,
// controlMu/controlSessions) is left unchanged; the two registries are separate
// because they serve different purposes (broadcast-notify vs. live fan-out) and
// the existing controlSessions logic is already well-tested.
//
// Sessions register at the end of sendWelcome (after welcome burst is sent, so
// no fanout arrives before the session is fully initialised) and deregister via
// a new unregisterBoundSession defer in run(). The defer is added alongside the
// existing unregisterControlSession defer so both always fire on exit.
//
// # Fan-out concurrency model
//
// Ingest is called from each upstream's OnAny goroutine (one per network).
// Multiple upstreams may call Ingest concurrently; the Sink interface requires
// concurrency-safety. fanout:
//
//  1. Acquires boundMu (RLock), snapshots the set of sessions bound to netid,
//     releases the lock.
//  2. Sends the serialised message to each snapshotted session OUTSIDE the lock
//     (conn.Conn.WriteMessage is goroutine-safe; the session may have been
//     deregistered between snapshot and send — a send to a closed conn errors
//     harmlessly and is logged only).
//
// This means:
//   - No lock is held during conn I/O (eliminates lock-held-during-I/O risk).
//   - A session disconnect after snapshot → harmless error, not a panic.
//   - Fanout never calls back into the Manager or any upstream goroutine
//     (eliminates the fanout→manager deadlock cycle).
//
// # Self-send / labeled-response [B#11]
//
// The upstream client negotiates echo-message so lurkd sees its own sent
// PRIVMSGs echoed back. Those echoes flow through Ingest exactly like any other
// message (stored once via the store, fanned out to all bound sessions). The
// store-once guarantee: messages are stored in Ingest via s.store.Ingest; the
// session relay path (dispatch→relayToUpstream) does NOT call store.Ingest —
// only the echo path does. This means:
//
//   - If echo-message is enabled, the upstream echoes the message, Ingest sees
//     it, stores it once, fans it out to all sessions.
//   - If echo-message is not enabled (degraded), the message is never seen in
//     Ingest, so nothing is stored and no echo is delivered (graceful degradation).
//
// The labeled-response FIFO: when a bound client sends a PRIVMSG carrying a
// @label tag, the session records (label, target, text) in a small per-session
// ring (pendingLabels). In fanout, before sending to a session, we check if
// the message matches the head of that session's pendingLabels; if so, we
// attach the original @label and dequeue. The FIFO is guarded by a small
// per-session mutex (labelMu) because fanout (upstream goroutine) reads it and
// the session goroutine writes it — they run concurrently.
//
// If no FIFO entry matches, the echo is delivered without a label (graceful
// degradation per the orchestrator's design decision).
//
// # Sanitization
//
// Live-fanned messages have their text params run through
// client.SanitizeForRelay (consistent with the storage path in backlog.Store).
// Non-relayable events (ev.Message==nil, @-prefixed commands) are skipped.
package server

import (
	"log"
	"strings"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// pendingLabel records a @label sent by a client that is awaiting its
// upstream echo.
type pendingLabel struct {
	label  string
	target string // first param of the original message (case-sensitive as sent)
	text   string // last param (message body, sanitized)
}

// pendingLabelFIFOSize is the maximum number of outstanding labeled sends per
// session. Beyond this the oldest entries are evicted (drop-oldest). In practice
// a conformant client will not pipeline more than a handful of labeled sends
// without receiving the echo.
const pendingLabelFIFOSize = 32

// boundSessionsInit is called by New to initialise the bound-session registry.
// It is a no-op beyond setting the map; placed here so New in server.go only
// needs a one-liner.
func (s *Server) initBoundSessions() {
	s.boundSessions = make(map[int]map[*session]struct{})
}

// registerBoundSession adds sess to the bound-session registry under its netid,
// then emits the synthetic channel-state burst to sess, and manages the
// upstream AWAY state (0→1 transition: send Back to clear AWAY).
//
// Called at the end of sendWelcome for sessions with netid != 0.
func (s *Server) registerBoundSession(sess *session) {
	var wasFirst bool
	s.boundMu.Lock()
	if s.boundSessions[sess.netid] == nil {
		s.boundSessions[sess.netid] = make(map[*session]struct{})
	}
	wasFirst = len(s.boundSessions[sess.netid]) == 0
	s.boundSessions[sess.netid][sess] = struct{}{}
	s.boundMu.Unlock()

	// Emit the channel state burst to this session. Called OUTSIDE the lock
	// (conn I/O must not be done under boundMu).
	s.sendStateBurst(sess)

	// On first attach (0→1): clear AWAY upstream so the bouncer appears attended.
	if wasFirst && s.mgr != nil {
		if cc, ok := s.mgr.Client(sess.netid); ok {
			_ = cc.Back() // errors are non-fatal (upstream may be reconnecting)
		}
	}
}

// unregisterBoundSession removes sess from the bound-session registry. Safe to
// call even if sess was never registered (no-op). On the last detach (1→0),
// sends AWAY upstream and flushes the cursor store for this session's clientID.
func (s *Server) unregisterBoundSession(sess *session) {
	var wasLast bool
	s.boundMu.Lock()
	if m, ok := s.boundSessions[sess.netid]; ok {
		delete(m, sess)
		if len(m) == 0 {
			delete(s.boundSessions, sess.netid)
			wasLast = true
		}
	}
	s.boundMu.Unlock()

	// On last detach (1→0): mark the upstream as away (bouncer unattended).
	// Called OUTSIDE the lock.
	if wasLast && s.mgr != nil {
		if cc, ok := s.mgr.Client(sess.netid); ok {
			_ = cc.Away(detachedAwayMessage)
		}
	}

	// Flush cursor store on clean detach so the cursor survives a crash-free
	// disconnect (the periodic flush covers the crash case up to the flush window).
	if s.cursors != nil {
		if err := s.cursors.Flush(); err != nil {
			log.Printf("server: cursor flush on detach (netid=%d): %v", sess.netid, err)
		}
	}
}

// Ingest implements server.Sink. It is called from upstream OnAny goroutines
// (concurrently, one per upstream). It stores the event via the backlog store
// (if configured) and then fans it out live to all sessions bound to netid.
//
// The store-once guarantee: only this path stores events. The relay path
// (session→upstream) never calls Ingest; the upstream echoes the sent message
// back (via echo-message) and that echo enters Ingest once.
//
// msgid consistency: when the store accepts the message, we capture its
// server-assigned msgid and stamp the live fan-out copy with that same id,
// overriding any @msgid the upstream may have sent. This ensures that a
// client can use a live-delivery msgid in a CHATHISTORY AFTER/BEFORE query
// and receive the correct result.
func (s *Server) Ingest(netid int, ev *client.Event) {
	// Store in the backlog store first (before fan-out, so the store is updated
	// before any client receives the live copy, keeping CHATHISTORY consistent).
	var storeMsgID string
	if s.store != nil {
		var stored bool
		storeMsgID, stored = s.store.Ingest(netid, ev)
		_ = stored // stored is informational; we use storeMsgID below
	}
	s.fanout(netid, ev, storeMsgID)
}

// fanout delivers ev to every session currently bound to netid. It is called
// from Ingest (upstream OnAny goroutine); no lock is held during conn I/O.
//
// storeMsgID, when non-empty, is the server-assigned msgid returned by the
// backlog store for this message. It is stamped onto the live fan-out copy so
// that live delivery and CHATHISTORY replay reference the same msgid. When
// storeMsgID is empty (message was filtered or not stored), the upstream's
// @msgid (if any) is preserved.
func (s *Server) fanout(netid int, ev *client.Event, storeMsgID string) {
	// Skip synthetic events (overflow, @-prefixed markers).
	if ev.Message == nil {
		return
	}
	cmd := ev.Command()
	if strings.HasPrefix(cmd, "@") {
		return
	}

	// Only relay real IRC messages to bound sessions (skip numerics that are
	// only meaningful to lurkd's own upstream-client registration, e.g. 001,
	// 005, etc.). Clients bound to a network expect to see chat traffic, not
	// the upstream's registration burst.
	//
	// We relay: PRIVMSG, NOTICE, JOIN, PART, QUIT, KICK, MODE, TOPIC, NICK,
	// AWAY, SETNAME, CHGHOST, ACCOUNT, INVITE, TAGMSG — essentially anything
	// that a connected IRC client would render. Numerics from the upstream that
	// are meaningful to the attached client (e.g. WHO replies, MODE queries)
	// are also relayed. What we skip: the registration numerics (001-005, 375-
	// 376, 004, 422 etc.) that are already handled by lurkd's own registration
	// burst. For v1 we relay everything that is not a registration numeric.
	if isRegistrationNumeric(cmd) {
		return
	}

	// Suppress KILL and ERROR from the upstream→client relay path. These
	// commands target lurkd's own server-to-server link, not the attached
	// clients. A hostile upstream sending KILL <lurkd-nick> or ERROR would
	// otherwise be forwarded verbatim; IRC clients treat an inbound KILL or
	// ERROR aimed at themselves as a disconnect signal, dropping every attached
	// session (DoS). The messages are already stored (or not) by the backlog
	// path above; they must never reach attached clients.
	if cmd == "KILL" || cmd == "ERROR" {
		return
	}

	// Early-exit if no sessions are currently bound to this netid. This avoids
	// O(message rate × tags+params) allocation under an upstream flood when no
	// client is attached — the common detached-bouncer case. The backlog store
	// has already been called by Ingest before fanout, so detached capture is
	// unaffected by this guard.
	s.boundMu.RLock()
	sessions := make([]*session, 0, len(s.boundSessions[netid]))
	for sess := range s.boundSessions[netid] {
		sessions = append(sessions, sess)
	}
	s.boundMu.RUnlock()

	if len(sessions) == 0 {
		return
	}

	// Build the relayed message: original message with @time tag added.
	ts := formatServerTime(ev.Time())

	// Sanitize all params before relaying (consistent with the store path).
	params := make([]string, len(ev.Message.Params))
	for i, p := range ev.Message.Params {
		params[i] = client.SanitizeForRelay(p)
	}

	// Merge existing tags with @time. Preserve upstream tags (e.g. @msgid if
	// the upstream sent one) but always override @time with our server-assigned
	// time so the client sees a consistent timeline.
	//
	// msgid consistency: if the store accepted this message, override @msgid
	// with the store-assigned id so live delivery and CHATHISTORY replay are
	// consistent. If not stored (filtered/cap-exceeded), preserve the upstream's
	// @msgid as-is (if present) for best-effort delivery.
	tags := make(irc.Tags)
	for k, v := range ev.Message.Tags {
		// Never forward the @label tag from the upstream echo — we manage it
		// ourselves via the pendingLabel FIFO.
		if k == "label" {
			continue
		}
		tags[k] = v
	}
	tags["time"] = ts
	if storeMsgID != "" {
		// Override (or set) @msgid with the store-assigned id.
		tags["msgid"] = storeMsgID
	}

	base := &irc.Message{
		Tags:    tags,
		Source:  client.SanitizeForRelay(ev.Message.Source),
		Command: cmd,
		Params:  params,
	}

	for _, sess := range sessions {
		s.fanoutToSession(sess, base, ev)
	}
}

// fanoutToSession delivers a single upstream event to one bound session,
// attaching the session's pending @label if the echo matches, and advancing
// the per-client cursor if the message is a PRIVMSG or NOTICE.
func (s *Server) fanoutToSession(sess *session, base *irc.Message, ev *client.Event) {
	msg := base

	// Check if this echo matches the head of the session's pending-label FIFO.
	// Only PRIVMSG and NOTICE carry labeled-response echoes in practice.
	if base.Command == "PRIVMSG" || base.Command == "NOTICE" {
		sess.labelMu.Lock()
		if len(sess.pendingLabels) > 0 {
			pl := sess.pendingLabels[0]
			// Match on target (Param 0) and body (last param).
			msgTarget := base.Param(0)
			msgText := ""
			if len(base.Params) > 0 {
				msgText = base.Params[len(base.Params)-1]
			}
			if strings.EqualFold(pl.target, msgTarget) && pl.text == msgText {
				// Match: attach label and dequeue.
				newTags := make(irc.Tags, len(base.Tags)+1)
				for k, v := range base.Tags {
					newTags[k] = v
				}
				newTags["label"] = pl.label
				msg = &irc.Message{
					Tags:    newTags,
					Source:  base.Source,
					Command: base.Command,
					Params:  base.Params,
				}
				sess.pendingLabels = sess.pendingLabels[1:]
			}
		}
		sess.labelMu.Unlock()

		// Advance the per-client cursor for this (clientID, netid, target).
		// Only PRIVMSG/NOTICE messages have a meaningful read position.
		if s.cursors != nil {
			msgid := msg.Tags["msgid"]
			if msgid != "" && msg.Param(0) != "" {
				key := CursorKey{
					ClientID: clientIDFromSession(sess),
					NetID:    sess.netid,
					Target:   msg.Param(0),
				}
				s.cursors.Advance(key, msgid, ev.Time())
			}
		}
	}

	// Use non-blocking TryWriteMessage so a slow or stuck attached client
	// cannot stall the upstream OnAny goroutine (and thus block fanout for
	// every other session on this netid). When the outbound queue is full the
	// message is dropped for this session only; the session is not torn down
	// (transient slowness is acceptable). If the session is persistently
	// overloaded its TCP send buffer will eventually fill, the kernel will
	// close the connection, and the session's run loop will exit naturally.
	if ok, err := sess.conn.TryWriteMessage(msg); err != nil {
		// Serialization failure or conn already closed — harmless.
		log.Printf("server: fanout to session (netid=%d): %v", sess.netid, err)
	} else if !ok {
		// Queue full: drop this message for this session and log. The drop is
		// expected under an upstream flood when a client is not reading fast
		// enough; it prevents head-of-line blocking across all other sessions.
		log.Printf("server: fanout drop (netid=%d): session queue full, message dropped", sess.netid)
	}
}

// isRegistrationNumeric reports whether cmd is a numeric that belongs to the
// upstream registration burst and should NOT be forwarded to bound clients.
// Bound clients have already received their own registration burst from lurkd.
func isRegistrationNumeric(cmd string) bool {
	switch cmd {
	case irc.RPL_WELCOME, // 001
		irc.RPL_YOURHOST,  // 002
		irc.RPL_CREATED,   // 003
		irc.RPL_MYINFO,    // 004
		irc.RPL_ISUPPORT,  // 005
		"251",             // RPL_LUSERCLIENT
		"252",             // RPL_LUSEROP
		"253",             // RPL_LUSERUNKNOWN
		"254",             // RPL_LUSERCHANNELS
		"255",             // RPL_LUSERME
		"265",             // RPL_LOCALUSERS
		"266",             // RPL_GLOBALUSERS
		irc.RPL_MOTD,      // 372
		irc.RPL_MOTDSTART, // 375
		irc.RPL_ENDOFMOTD, // 376
		irc.ERR_NOMOTD:    // 422
		return true
	}
	return false
}
