package client

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"lurk/irc"
	"lurk/isupport"
)

// Member is one member of a channel as tracked by the client: the nickname and
// the membership prefixes the server has assigned (e.g. "@" for op, "+" for
// voice), highest privilege first.
type Member struct {
	// Nick is the member's current nickname (case preserved as last seen).
	Nick string

	// Prefixes are the membership prefix symbols held by the member, ordered
	// highest-privilege first per the server's PREFIX advertisement (e.g. "@+").
	Prefixes string
}

// channelState tracks one joined channel: its display name, members, and topic.
type channelState struct {
	// name is the channel name as the client knows it (case as last seen).
	name string
	// members maps folded nick -> Member.
	members map[string]*Member

	// topic is the channel topic text (empty if unset/unknown). topicSetBy is the
	// nick (or mask) that last set it, and topicAt when it was set; both are
	// populated from RPL_TOPICWHOTIME (333) and may be zero/empty if the server
	// did not provide them.
	topic      string
	topicSetBy string
	topicAt    time.Time
}

// state is the client's view of the connection: the self nick, the active case
// mapping (from isupport), and the channels the client is in. It is owned by the
// client run goroutine and accessed only from there, so it needs no locking for
// the run loop's own updates; the exported Client snapshot accessors take the
// client mutex when reading it from other goroutines.
type state struct {
	// self is the client's own current nickname.
	self string

	// feat is the accumulated server feature set; its CaseMapping drives folding.
	feat isupport.ISupport

	// fold is the active case-folding function, refreshed whenever feat changes.
	fold isupport.CaseMapping

	// channels maps folded channel name -> channelState.
	channels map[string]*channelState
}

// newState returns an empty state defaulting to the RFC1459 case mapping (the
// IRC default that applies before any 005 is seen).
func newState() *state {
	return &state{
		fold:     isupport.CaseRFC1459,
		channels: make(map[string]*channelState),
	}
}

// mergeISupport folds a batch of 005 tokens into the feature set and refreshes
// the active case mapping. Re-keying existing channel/member maps under a new
// mapping is unnecessary in practice (CASEMAPPING is fixed for a connection and
// arrives in the registration burst before any JOIN), so we simply adopt the
// latest mapping.
func (s *state) mergeISupport(tokens []string) {
	s.feat = s.feat.Merge(tokens)
	s.fold = s.feat.CaseMapping()
}

// foldKey returns the canonical map key for a nick or channel name.
func (s *state) foldKey(name string) string { return s.fold.Fold(name) }

// channel returns the tracked channel for name, or nil if not joined.
func (s *state) channel(name string) *channelState {
	return s.channels[s.foldKey(name)]
}

// addChannel records that the client has joined name, creating empty tracking.
func (s *state) addChannel(name string) *channelState {
	key := s.foldKey(name)
	ch, ok := s.channels[key]
	if !ok {
		ch = &channelState{name: name, members: make(map[string]*Member)}
		s.channels[key] = ch
	}
	return ch
}

// removeChannel forgets a channel the client has left.
func (s *state) removeChannel(name string) {
	delete(s.channels, s.foldKey(name))
}

// addMember adds or updates a member of a channel with the given prefixes.
func (cs *channelState) addMember(fold func(string) string, nick, prefixes string) {
	key := fold(nick)
	if m, ok := cs.members[key]; ok {
		m.Nick = nick
		if prefixes != "" {
			m.Prefixes = prefixes
		}
		return
	}
	cs.members[key] = &Member{Nick: nick, Prefixes: prefixes}
}

// removeMember drops a member from a channel.
func (cs *channelState) removeMember(fold func(string) string, nick string) {
	delete(cs.members, fold(nick))
}

// renameMember moves a member from oldNick to newNick across one channel,
// preserving prefixes. It is a no-op if the member is not present.
func (cs *channelState) renameMember(fold func(string) string, oldNick, newNick string) {
	oldKey := fold(oldNick)
	m, ok := cs.members[oldKey]
	if !ok {
		return
	}
	delete(cs.members, oldKey)
	m.Nick = newNick
	cs.members[fold(newNick)] = m
}

// applyNamReply parses a RPL_NAMREPLY (353) line and records its members in the
// named channel. The names parameter is a space-separated list of nicks, each
// optionally prefixed with one or more membership symbols (e.g. "@+nick" under
// multi-prefix). Prefix symbols are recognised via the server's PREFIX
// advertisement.
func (s *state) applyNamReply(channel, names string) {
	cs := s.addChannel(channel)
	symbols := s.feat.PrefixSymbols()
	for _, raw := range strings.Fields(names) {
		nick, prefixes := splitPrefixes(raw, symbols)
		// Under the userhost-in-names capability, each entry is a full
		// nick!user@host mask rather than a bare nick. A nick can contain
		// neither '!' nor '@', so truncating at the first '!' yields the nick.
		if bang := strings.IndexByte(nick, '!'); bang >= 0 {
			nick = nick[:bang]
		}
		if nick == "" {
			continue
		}
		cs.addMember(s.foldKey, nick, prefixes)
	}
}

// splitPrefixes separates leading membership prefix symbols from a nick in a
// NAMES entry. symbols is the set of valid prefix characters (e.g. "@+"); any
// run of them at the start of entry is the prefix, the rest is the nick.
func splitPrefixes(entry, symbols string) (nick, prefixes string) {
	i := 0
	for i < len(entry) && strings.IndexByte(symbols, entry[i]) >= 0 {
		i++
	}
	return entry[i:], entry[:i]
}

// applyModeChange applies a channel MODE change to membership prefixes for the
// prefix modes (those listed in PREFIX). Non-prefix modes are ignored for
// tracking purposes. modeStr is the mode-change string (e.g. "+o-v") and args
// are the per-mode arguments (nicks for prefix modes).
func (s *state) applyModeChange(channel, modeStr string, args []string) {
	cs := s.channel(channel)
	if cs == nil {
		return
	}
	adding := true
	argi := 0
	for i := 0; i < len(modeStr); i++ {
		c := modeStr[i]
		switch c {
		case '+':
			adding = true
			continue
		case '-':
			adding = false
			continue
		}
		sym, isPrefix := s.feat.PrefixSymbolForMode(c)
		if !isPrefix {
			// Non-prefix mode: it may still consume an argument, but we don't
			// track those. Best-effort: assume A/B modes and key modes take an
			// arg; without full CHANMODES bookkeeping we conservatively consume
			// one only for known prefix modes, leaving others alone.
			continue
		}
		// Prefix mode changes always take a nick argument.
		if argi >= len(args) {
			continue
		}
		nick := args[argi]
		argi++
		m := cs.members[s.foldKey(nick)]
		if m == nil {
			continue
		}
		if adding {
			m.Prefixes = addPrefix(m.Prefixes, sym, s.feat.PrefixSymbols())
		} else {
			m.Prefixes = strings.ReplaceAll(m.Prefixes, string(sym), "")
		}
	}
}

// addPrefix inserts symbol into prefixes keeping the ordering implied by the
// server's full symbol ranking (order), highest privilege first, and avoiding
// duplicates.
func addPrefix(prefixes string, symbol byte, order string) string {
	if strings.IndexByte(prefixes, symbol) >= 0 {
		return prefixes
	}
	set := prefixes + string(symbol)
	// Re-sort set by each symbol's index in order.
	bs := []byte(set)
	sort.SliceStable(bs, func(i, j int) bool {
		return strings.IndexByte(order, bs[i]) < strings.IndexByte(order, bs[j])
	})
	return string(bs)
}

// isChannel reports whether target names a channel (its first byte is one of
// the server's CHANTYPES), as opposed to a nick.
func (s *state) isChannel(target string) bool {
	if target == "" {
		return false
	}
	return strings.IndexByte(s.feat.ChanTypes(), target[0]) >= 0
}

// setTopicText records a channel's topic text (from RPL_TOPIC 332 or a TOPIC
// command). It creates tracking for the channel if needed (a TOPIC may arrive
// for a channel the client is watching). setBy/at are left to setTopicMeta.
func (s *state) setTopicText(channel, topic string) {
	cs := s.addChannel(channel)
	cs.topic = topic
}

// setTopicMeta records who set a channel's topic and when (from RPL_TOPICWHOTIME
// 333, or from a live TOPIC command's source/time).
func (s *state) setTopicMeta(channel, setBy string, at time.Time) {
	cs := s.addChannel(channel)
	cs.topicSetBy = setBy
	cs.topicAt = at
}

// parseUnixSeconds parses a Unix-epoch-seconds string (as used by 333's set-at
// field), returning the zero time on any error.
func parseUnixSeconds(s string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// channelNames returns the folded keys' display names of channels the client is
// in. Used by snapshot accessors.
func (s *state) channelNames() []string {
	out := make([]string, 0, len(s.channels))
	for _, cs := range s.channels {
		out = append(out, cs.name)
	}
	sort.Strings(out)
	return out
}

// renameEverywhere renames a member across every channel they're in (for a NICK
// change), and updates self if the change is the client's own nick.
func (s *state) renameEverywhere(oldNick, newNick string) {
	for _, cs := range s.channels {
		cs.renameMember(s.foldKey, oldNick, newNick)
	}
	if s.foldKey(oldNick) == s.foldKey(s.self) {
		s.self = newNick
	}
}

// removeEverywhere drops a member from every channel (for a QUIT).
func (s *state) removeEverywhere(nick string) {
	for _, cs := range s.channels {
		cs.removeMember(s.foldKey, nick)
	}
}

// joinTargets returns the channel name(s) from a JOIN message. JOIN can carry a
// comma-separated list, though servers echoing a single client JOIN send one.
func joinTargets(m *irc.Message) []string {
	return strings.Split(m.Param(0), ",")
}
