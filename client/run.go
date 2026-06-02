package client

import (
	"strings"
	"time"

	"lurk/irc"
	"lurk/isupport"
	"lurk/sasl"
)

// Topic-related command/numerics. The irc package does not yet export these as
// named constants; define them locally as unexported strings so the client
// builds independently, and switch to irc.TOPIC / irc.RPL_TOPIC /
// irc.RPL_TOPICWHOTIME once the parser package adds them (requested). They match
// the parsed Message.Command.
const (
	cmdTopic        = "TOPIC" // TOPIC <channel> :<topic>
	rplTopic        = "332"   // RPL_TOPIC: <client> <channel> :<topic>
	rplTopicWhoTime = "333"   // RPL_TOPICWHOTIME: <client> <channel> <setBy> <setAt unix>
)

// run is the client's single message-processing goroutine. It drains inbound
// messages from the transport, applies protocol handling (PING, capability
// negotiation, SASL, registration numerics, state tracking), and dispatches
// each message to user handlers. It returns when the Messages channel closes.
func (c *Client) run() {
	defer close(c.done)
	for m := range c.tr.Messages() {
		c.handle(m)
	}
	// The connection ended. If registration was still pending, release the
	// waiter with the transport's terminal error.
	c.signalRegistered(c.connError())
}

// connError returns a registration-phase error describing why the connection
// ended, preferring the transport's recorded error.
func (c *Client) connError() error {
	if err := c.tr.Err(); err != nil {
		return err
	}
	return nil
}

// handle processes one inbound message: protocol-level handling first (which
// may emit responses and advance registration), then state tracking, then user
// dispatch.
func (c *Client) handle(m *irc.Message) {
	// Always answer PING immediately, even mid-registration.
	if strings.EqualFold(m.Command, irc.PING) {
		_ = c.write(irc.PONG, m.Params...)
		c.dispatch(m)
		return
	}

	switch m.Command {
	case irc.CAP:
		c.handleCAP(m)
	case irc.AUTHENTICATE,
		irc.RPL_LOGGEDIN, irc.RPL_LOGGEDOUT, irc.ERR_NICKLOCKED,
		irc.RPL_SASLSUCCESS, irc.ERR_SASLFAIL, irc.ERR_SASLTOOLONG,
		irc.ERR_SASLABORTED, irc.ERR_SASLALREADY, irc.RPL_SASLMECHS:
		c.handleSASL(m)
	case irc.RPL_WELCOME:
		c.handleWelcome(m)
	case irc.RPL_ISUPPORT:
		c.handleISupport(m)
	case irc.ERR_NICKNAMEINUSE, irc.ERR_ERRONEUSNICKNAME:
		c.handleNickInUse(m)
	}

	// State tracking for membership-affecting messages.
	c.track(m)

	// Protocol reactions that depend on freshly-tracked state (e.g. requesting
	// channel history once our own JOIN is confirmed).
	c.afterTrack(m)

	// User dispatch last, so handlers see fully updated state.
	c.dispatch(m)
}

// chatHistoryLimit is the number of recent messages requested per channel when
// auto-fetching backlog on join.
const chatHistoryLimit = 50

// afterTrack runs client-initiated protocol reactions that should happen after
// state tracking but before user dispatch. Currently it auto-requests channel
// history when the client confirms its own JOIN and draft/chathistory is
// enabled, so a freshly joined channel is not empty (and the line client
// benefits too, not just the TUI).
func (c *Client) afterTrack(m *irc.Message) {
	if m.Command != irc.JOIN {
		return
	}
	c.mu.Lock()
	self := c.st.foldKey(m.Nick()) == c.st.foldKey(c.st.self)
	c.mu.Unlock()
	if !self || !c.CapEnabled("draft/chathistory") {
		return
	}
	for _, ch := range joinTargets(m) {
		if ch != "" {
			_ = c.ChatHistoryLatest(ch, chatHistoryLimit)
		}
	}
}

// dispatch builds an Event for an inbound message and emits it to both the
// callback dispatcher and the Events stream. If the message carries an @batch
// tag, the event is annotated with the open batch's type so consumers can group
// or label it (the batch may be closed before they inspect the event).
func (c *Client) dispatch(m *irc.Message) {
	ev := &Event{Client: c, Message: m, recvTime: time.Now()}
	if ref := m.Tags.Get("batch"); ref != "" {
		c.mu.Lock()
		ev.batchType = c.st.batchTypeFor(ref)
		c.mu.Unlock()
	}
	c.emit(ev)
}

// emit stamps the receive time (if unset), runs the registered callback
// handlers, and publishes the event to the Events stream (if anyone is
// listening). It is the single funnel for every dispatched event so the stream
// and the On/Handle callbacks observe exactly the same events.
func (c *Client) emit(ev *Event) {
	if ev.recvTime.IsZero() {
		ev.recvTime = time.Now()
	}
	c.disp.dispatch(ev)
	c.publish(ev)
}

// handleCAP feeds a CAP message to the negotiator, sends whatever lines it
// returns, and advances the registration phase (kicking off SASL or finishing
// with CAP END as appropriate).
func (c *Client) handleCAP(m *irc.Message) {
	lines, err := c.neg.Receive(m)
	if err != nil {
		c.failRegistration(err)
		return
	}
	for _, l := range lines {
		_ = c.tr.Send(l)
	}
	c.advanceCAP()
}

// advanceCAP checks whether SASL should now run, or whether negotiation is done
// without SASL. It is called after every CAP message and after SASL completes.
func (c *Client) advanceCAP() {
	if c.neg.NeedSASL() && c.conv == nil {
		c.startSASL()
	}
}

// handleISupport folds a 005 line into the feature set and refreshes the case
// mapping used for state-tracking keys.
func (c *Client) handleISupport(m *irc.Message) {
	tokens := isupport.TokensFromMessage(m)
	if len(tokens) == 0 {
		return
	}
	c.mu.Lock()
	c.st.mergeISupport(tokens)
	c.mu.Unlock()
}

// handleWelcome records the confirmed self nick from RPL_WELCOME and signals
// that registration is complete.
func (c *Client) handleWelcome(m *irc.Message) {
	if nick := m.Param(0); nick != "" {
		c.mu.Lock()
		c.st.self = nick
		c.mu.Unlock()
	}
	c.signalRegistered(nil)
	// Raise the semantic Connected event.
	c.disp.dispatch(&Event{Client: c, Message: withCommand(m, evtConnected)})
}

// withCommand returns a shallow copy of m with its Command replaced, used to
// raise synthetic semantic events without mutating the original message.
func withCommand(m *irc.Message, command string) *irc.Message {
	cp := *m
	cp.Command = command
	return &cp
}

// handleNickInUse responds to ERR_NICKNAMEINUSE / ERR_ERRONEUSNICKNAME during
// registration by trying a fallback nick. After registration it is left to user
// handlers (the client does not auto-rename a live session).
func (c *Client) handleNickInUse(m *irc.Message) {
	select {
	case <-c.registered:
		// Already registered: don't auto-change a live nick.
		return
	default:
	}
	next := c.nextNick()
	c.mu.Lock()
	c.st.self = next
	c.mu.Unlock()
	_ = c.write(irc.NICK, next)
}

// nextNick derives the next nickname to try after a collision: the configured
// FallbackNick on the first collision (if set and not already current),
// otherwise the current nick with an underscore appended.
func (c *Client) nextNick() string {
	c.mu.Lock()
	cur := c.st.self
	c.mu.Unlock()
	if c.cfg.FallbackNick != "" && cur != c.cfg.FallbackNick {
		return c.cfg.FallbackNick
	}
	return cur + "_"
}

// failRegistration records a fatal registration error and tears down the
// connection. It is used for unrecoverable protocol errors during the handshake.
func (c *Client) failRegistration(err error) {
	c.signalRegistered(err)
	_ = c.tr.Close()
}

// track updates connection state from membership-affecting messages. It runs
// under the client mutex so snapshot accessors see consistent state.
func (c *Client) track(m *irc.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch m.Command {
	case irc.JOIN:
		c.trackJoin(m)
	case irc.PART:
		c.trackPart(m)
	case irc.QUIT:
		c.st.removeEverywhere(m.Nick())
	case irc.KICK:
		c.trackKick(m)
	case irc.NICK:
		c.trackNick(m)
	case irc.ACCOUNT:
		c.trackAccount(m)
	case irc.AWAY:
		c.trackAway(m)
	case irc.CHGHOST:
		c.trackChghost(m)
	case irc.BATCH:
		c.trackBatch(m)
	case irc.RPL_NAMREPLY:
		c.trackNamReply(m)
	case irc.MODE:
		c.trackMode(m)
	case cmdTopic:
		c.trackTopicCommand(m)
	case rplTopic:
		c.trackTopicReply(m)
	case rplTopicWhoTime:
		c.trackTopicWhoTime(m)
	}
}

// trackTopicCommand records a live TOPIC change: ":nick TOPIC <channel>
// :<text>". The setter and time come from the message source and receive time
// (server-time tag if present is applied by the caller's Event, but for state
// we use the wall clock here).
func (c *Client) trackTopicCommand(m *irc.Message) {
	channel := m.Param(0)
	if channel == "" {
		return
	}
	c.st.setTopicText(channel, m.Param(1))
	c.st.setTopicMeta(channel, m.Nick(), time.Now())
}

// trackTopicReply records RPL_TOPIC (332): "<client> <channel> :<topic>".
func (c *Client) trackTopicReply(m *irc.Message) {
	channel := m.Param(1)
	if channel == "" {
		return
	}
	c.st.setTopicText(channel, m.Param(2))
}

// trackTopicWhoTime records RPL_TOPICWHOTIME (333): "<client> <channel> <setBy>
// <setAt unix-seconds>".
func (c *Client) trackTopicWhoTime(m *irc.Message) {
	channel := m.Param(1)
	if channel == "" {
		return
	}
	c.st.setTopicMeta(channel, m.Param(2), parseUnixSeconds(m.Param(3)))
}

// trackJoin records a JOIN. When the joiner is the client itself, the channel
// is added to the joined set; otherwise the joiner is added as a member. The
// joiner's ident/host come from the source mask, and under extended-join the
// account is the first parameter ("*" meaning not logged in).
func (c *Client) trackJoin(m *irc.Message) {
	joiner := m.Nick()
	user, host := m.User(), m.Host()
	self := c.st.foldKey(joiner) == c.st.foldKey(c.st.self)
	// extended-join: ":nick!user@host JOIN <channel> <account> :<realname>".
	// Param(0) is the channel; Param(1), when present, is the account.
	account := normalizeAccount(m.Param(1))
	for _, ch := range joinTargets(m) {
		if ch == "" {
			continue
		}
		cs := c.st.addChannel(ch)
		if !self {
			cs.addMember(c.st.foldKey, joiner, "", user, host)
			if account != "" {
				if mem := cs.members[c.st.foldKey(joiner)]; mem != nil {
					mem.Account = account
				}
			}
		}
	}
}

// trackPart records a PART. When the client itself parts, the channel is
// forgotten; otherwise the parter is removed from the channel's members.
func (c *Client) trackPart(m *irc.Message) {
	parter := m.Nick()
	self := c.st.foldKey(parter) == c.st.foldKey(c.st.self)
	for _, ch := range strings.Split(m.Param(0), ",") {
		if ch == "" {
			continue
		}
		if self {
			c.st.removeChannel(ch)
			continue
		}
		if cs := c.st.channel(ch); cs != nil {
			cs.removeMember(c.st.foldKey, parter)
		}
	}
}

// trackKick records a KICK (":kicker!u@h KICK <channel> <user>[,<user>...]
// [:reason]"). When the client itself is the target the channel is forgotten;
// otherwise each kicked user is dropped from the channel's members. Multiple
// targets in one channel (a comma list) are all handled.
func (c *Client) trackKick(m *irc.Message) {
	channel := m.Param(0)
	cs := c.st.channel(channel)
	for _, nick := range strings.Split(m.Param(1), ",") {
		if nick == "" {
			continue
		}
		if c.st.foldKey(nick) == c.st.foldKey(c.st.self) {
			c.st.removeChannel(channel)
			return
		}
		if cs != nil {
			cs.removeMember(c.st.foldKey, nick)
		}
	}
}

// trackNick records a NICK change across every channel the user shares.
func (c *Client) trackNick(m *irc.Message) {
	c.st.renameEverywhere(m.Nick(), m.Param(0))
}

// trackAccount records an account-notify ":nick!user@host ACCOUNT <account>"
// message, updating the nick's Account in every channel it shares. An account of
// "*" (or empty) means the user logged out.
func (c *Client) trackAccount(m *irc.Message) {
	account := normalizeAccount(m.Param(0))
	c.st.updateMemberEverywhere(c.st.foldKey(m.Nick()), func(mem *Member) {
		mem.Account = account
	})
}

// trackAway records an away-notify message: ":nick!user@host AWAY :<message>"
// marks the nick away, while ":nick!user@host AWAY" with no parameter marks them
// back. The away text itself is not stored on the member (whois carries it).
func (c *Client) trackAway(m *irc.Message) {
	away := len(m.Params) > 0
	c.st.updateMemberEverywhere(c.st.foldKey(m.Nick()), func(mem *Member) {
		mem.Away = away
	})
}

// trackChghost records a chghost ":nick!user@host CHGHOST <newuser> <newhost>"
// message, updating the nick's User/Host in every channel it shares.
func (c *Client) trackChghost(m *irc.Message) {
	newUser, newHost := m.Param(0), m.Param(1)
	c.st.updateMemberEverywhere(c.st.foldKey(m.Nick()), func(mem *Member) {
		if newUser != "" {
			mem.User = newUser
		}
		if newHost != "" {
			mem.Host = newHost
		}
	})
}

// trackBatch records an opening or closing BATCH command. The first parameter
// is the reference tag prefixed with '+' (open) or '-' (close); on open the
// second parameter is the batch type and the rest are batch parameters.
func (c *Client) trackBatch(m *irc.Message) {
	ref := m.Param(0)
	if ref == "" {
		return
	}
	switch ref[0] {
	case '+':
		c.st.openBatch(ref[1:], m.Param(1), m.Params[min(2, len(m.Params)):])
	case '-':
		c.st.closeBatch(ref[1:])
	}
}

// trackNamReply records the members from a RPL_NAMREPLY (353). The params are
// [nick, symbol, channel, :names] where symbol is the channel visibility
// ("=", "*", "@"); the channel is the third param and the names the trailing.
func (c *Client) trackNamReply(m *irc.Message) {
	channel := m.Param(2)
	names := m.Param(3)
	if channel == "" {
		return
	}
	c.st.applyNamReply(channel, names)
}

// trackMode applies a channel MODE change to membership prefixes. The params
// are [target, modes, args...]; only channel targets affect membership.
func (c *Client) trackMode(m *irc.Message) {
	target := m.Param(0)
	if !c.st.isChannel(target) || len(m.Params) < 2 {
		return
	}
	c.st.applyModeChange(target, m.Param(1), m.Params[2:])
}

// --- SASL drive ---

// startSASL opens a SASL exchange for the configured mechanism (selecting it
// against the server's advertised mechanism list) and sends the opening
// AUTHENTICATE lines.
func (c *Client) startSASL() {
	mech := c.cfg.SASL.mechanism()
	if mech == nil {
		// SASL cap was ACKed but no usable mechanism is configured: skip SASL and
		// proceed to CAP END.
		c.finishSASL()
		return
	}
	mech, err := sasl.SelectMechanism(c.neg.SASLMechs(), mech)
	if err != nil {
		c.failRegistration(err)
		return
	}
	c.conv = sasl.NewConversation(mech)
	for _, l := range c.conv.Begin() {
		_ = c.tr.Send(l)
	}
}

// handleSASL feeds an AUTHENTICATE or 9xx message to the active conversation,
// sends any response lines, and on completion (success or failure) releases the
// capability negotiator to send CAP END.
func (c *Client) handleSASL(m *irc.Message) {
	if c.conv == nil {
		return
	}
	lines, done, err := c.conv.Receive(m)
	for _, l := range lines {
		_ = c.tr.Send(l)
	}
	if err != nil {
		// SASL failed. Cycle-1 policy: abort registration so the caller learns
		// authentication did not succeed rather than connecting unauthenticated.
		c.failRegistration(err)
		return
	}
	if done {
		c.finishSASL()
	}
}

// finishSASL tells the negotiator SASL is complete and sends the resulting
// CAP END line, allowing the server to proceed to RPL_WELCOME.
func (c *Client) finishSASL() {
	c.conv = nil
	for _, l := range c.neg.SASLComplete() {
		_ = c.tr.Send(l)
	}
}
