// burst.go implements the attach state burst (Phase 7 [B#6][B#14]):
// synthetic channel JOIN, RPL_TOPIC/RPL_TOPICWHOTIME, RPL_NAMREPLY/RPL_ENDOFNAMES
// sent to a newly-bound client to bring it up to date with the upstream's
// current channel state.
//
// # Design
//
// When a session binds (registerBoundSession, called from sendWelcome), lurkd
// emits a synthetic state burst to THAT session only. The burst:
//
//  1. For each channel the upstream client is in (cc.Channels()):
//     a. JOIN from the client's own nick (so the client's local state is seeded).
//     b. RPL_TOPIC (332) + RPL_TOPICWHOTIME (333) if a topic is set.
//     c. RPL_NAMREPLY (353) line(s) built from cc.Members(), respecting
//     multi-prefix (if the session enabled multi-prefix) and the symbol
//     ordering from the upstream's PrefixSymbols.
//     d. RPL_ENDOFNAMES (366).
//
// NO stored PRIVMSG/NOTICE history is pushed here — the client pulls history
// itself via CHATHISTORY (its self-JOIN triggers that in lurk) [B#6].
//
// All nick/topic/source values pass through client.SanitizeForRelay before
// transmission.
//
// If the upstream client is not yet connected (mgr is nil or Client(netid) is
// not found), the burst is silently skipped (empty burst — no channel joins).
// This is graceful degradation: the lurk client will issue CHATHISTORY once
// it connects to the upstream directly; a missed burst is recoverable.
//
// # Away-on-detach
//
// Register/unregister transitions also manage the upstream AWAY state:
//   - 0 → 1 bound sessions: send AWAY clear (Back) to signal the bouncer is
//     attended.
//   - 1 → 0 bound sessions: send AWAY ":detached" to signal the bouncer is
//     unattended.
//
// The count is computed under boundMu; Away/Back is called OUTSIDE the lock
// to avoid lock-held-during-I/O.
package server

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// detachedAwayMessage is the AWAY reason sent to the upstream when the last
// bound client detaches. It intentionally starts with ":" (a colon) so any
// IRC client that echoes it as a status knows this is from the bouncer.
const detachedAwayMessage = ":detached"

// maxBurstChannels is the maximum number of channels included in a single
// state burst (the synthetic JOIN+topic+NAMES sequence sent to an attaching
// client). Without this cap, a hostile upstream that has forced lurkd to
// join thousands of channels would cause sendStateBurst to emit O(n) messages
// to every attaching client — an unbounded amplification.
//
// This is a secondary defence at the burst serialisation layer. The primary
// defence is the client-state channel cap (task #3), which limits how many
// channels the upstream client.Client can be in. If that cap is in place, the
// upstream slice will never exceed it; this guard protects against that cap
// being absent or bypassed, and makes the invariant explicit here.
//
// 500 matches DefaultMaxTargetsPerNet in the backlog store, keeping the three
// related caps consistent.
const maxBurstChannels = 500

// sendStateBurst emits the synthetic channel-state burst to sess.
// It is called from registerBoundSession after the session has been added to
// the bound-session registry, so the session is fully registered.
//
// sendStateBurst reads the upstream client state via Manager.Client(sess.netid).
// If no manager or no upstream is found the burst is empty (no-op).
func (s *Server) sendStateBurst(sess *session) {
	if s.mgr == nil {
		return
	}
	cc, ok := s.mgr.Client(sess.netid)
	if !ok {
		return
	}

	// Get the upstream's current nick (what the lurk client will use as its
	// own nick in the burst JOIN lines).
	upstreamNick := client.SanitizeForRelay(cc.Nick())
	if upstreamNick == "" {
		upstreamNick = sess.nick // fall back to the session nick
	}

	// Determine whether the session enabled multi-prefix. If so, emit all
	// prefix symbols; if not, emit only the highest-privilege symbol.
	multiPrefix := sess.capEnabled["multi-prefix"]

	channels := cc.Channels()
	if len(channels) > maxBurstChannels {
		log.Printf("server: state burst netid=%d: upstream has %d channels; capping burst at %d",
			sess.netid, len(channels), maxBurstChannels)
		channels = channels[:maxBurstChannels]
	}
	for _, ch := range channels {
		ch = client.SanitizeForRelay(ch)
		if ch == "" {
			continue
		}
		if err := s.sendChannelBurst(sess, cc, upstreamNick, ch, multiPrefix); err != nil {
			// The session likely disconnected mid-burst; stop and let run() clean up.
			log.Printf("server: state burst netid=%d channel=%s: %v", sess.netid, ch, err)
			return
		}
	}
}

// sendChannelBurst sends the JOIN+topic+names burst for one channel to sess.
func (s *Server) sendChannelBurst(sess *session, cc *client.Client, upstreamNick, ch string, multiPrefix bool) error {
	// 1. Synthetic JOIN: :<nick>!*@* JOIN #channel
	// The host/user components are synthetic; the important part is the nick so
	// the client seeds its local member state.
	joinSource := upstreamNick + "!*@*"
	if err := sess.send(&irc.Message{
		Source:  joinSource,
		Command: irc.JOIN,
		Params:  []string{ch},
	}); err != nil {
		return err
	}

	// 2. Topic (332 + 333) if set.
	topicText, topicSetBy, topicAt := cc.Topic(ch)
	topicText = client.SanitizeForRelay(topicText)
	if topicText != "" {
		// 332 RPL_TOPIC
		if err := sess.send(&irc.Message{
			Source:  serverName,
			Command: irc.RPL_TOPIC,
			Params:  []string{upstreamNick, ch, topicText},
		}); err != nil {
			return err
		}
		// 333 RPL_TOPICWHOTIME — only send if we have metadata.
		setByStr := client.SanitizeForRelay(topicSetBy)
		if setByStr == "" {
			setByStr = serverName // use a safe placeholder
		}
		var atStr string
		if !topicAt.IsZero() {
			atStr = fmt.Sprintf("%d", topicAt.Unix())
		} else {
			atStr = fmt.Sprintf("%d", time.Now().Unix())
		}
		if err := sess.send(&irc.Message{
			Source:  serverName,
			Command: irc.RPL_TOPICWHOTIME,
			Params:  []string{upstreamNick, ch, setByStr, atStr},
		}); err != nil {
			return err
		}
	}

	// 3. NAMES: build RPL_NAMREPLY (353) lines.
	// The prefix symbols string from the upstream's PrefixSymbols is used to
	// map mode letters to symbols. We build a space-separated list of
	// [symbols]nick entries.
	members := cc.Members(ch)
	prefixSymbols := cc.PrefixSymbols() // e.g. "~&@%+"

	// Build the names list in chunks so no single 353 line exceeds the IRC
	// wire budget. The max line length for a NAMES reply is bounded by the IRC
	// 512-byte limit; we conservatively allow up to 400 bytes of names per line.
	const namesChunkSize = 400

	var buf strings.Builder
	for i, m := range members {
		nick := client.SanitizeForRelay(m.Nick)
		if nick == "" {
			continue
		}

		// Determine the prefix symbols to prepend.
		var prefix string
		if multiPrefix {
			// Emit all symbols in PrefixSymbols order (highest privilege first).
			for _, sym := range prefixSymbols {
				if strings.ContainsRune(m.Prefixes, sym) {
					prefix += string(sym)
				}
			}
		} else {
			// Only the highest-privilege symbol.
			for _, sym := range prefixSymbols {
				if strings.ContainsRune(m.Prefixes, sym) {
					prefix = string(sym)
					break
				}
			}
		}

		entry := prefix + nick

		// Flush the buffer if adding this entry would exceed the chunk size.
		if buf.Len() > 0 && buf.Len()+1+len(entry) > namesChunkSize {
			if err := sess.send(&irc.Message{
				Source:  serverName,
				Command: irc.RPL_NAMREPLY,
				Params:  []string{upstreamNick, "=", ch, buf.String()},
			}); err != nil {
				return err
			}
			buf.Reset()
		}

		if buf.Len() > 0 {
			buf.WriteByte(' ')
		}
		buf.WriteString(entry)

		// Flush on last member.
		if i == len(members)-1 && buf.Len() > 0 {
			if err := sess.send(&irc.Message{
				Source:  serverName,
				Command: irc.RPL_NAMREPLY,
				Params:  []string{upstreamNick, "=", ch, buf.String()},
			}); err != nil {
				return err
			}
			buf.Reset()
		}
	}

	// 4. RPL_ENDOFNAMES (366).
	return sess.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_ENDOFNAMES,
		Params:  []string{upstreamNick, ch, "End of /NAMES list."},
	})
}
