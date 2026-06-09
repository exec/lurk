package cap

import (
	"strings"

	"github.com/exec/lurk/irc"
)

// Receive feeds one parsed message into the Negotiator and returns the lines
// the client should send in response (CAP REQ payloads, or CAP END). Messages
// whose Command is not CAP are ignored and return no lines and no error, so the
// caller may route every inbound message through Receive without pre-filtering.
//
// The returned slice is nil when there is nothing to send. Receive never blocks
// and never performs I/O.
func (n *Negotiator) Receive(m *irc.Message) ([]string, error) {
	if m == nil || !strings.EqualFold(m.Command, irc.CAP) {
		return nil, nil
	}

	// Lock for the whole dispatch: the helpers below mutate the available/enabled
	// maps and negotiation fields that concurrent readers (IsEnabled etc.) touch.
	n.mu.Lock()
	defer n.mu.Unlock()

	// CAP layout: <client> <SUBCMD> [*] [:caps]. Param(0) is the target nick or
	// "*"; Param(1) is the subcommand. The cap list is the final parameter; a
	// bare "*" parameter before it signals multiline continuation (LS/LIST).
	sub := strings.ToUpper(m.Param(1))
	switch sub {
	case irc.CAP_LS:
		return n.onLS(m)
	case irc.CAP_LIST:
		n.onList(m)
		return nil, nil
	case irc.CAP_ACK:
		return n.onACK(m)
	case irc.CAP_NAK:
		return n.onNAK(m)
	case irc.CAP_NEW:
		return n.onNew(m)
	case irc.CAP_DEL:
		n.onDel(m)
		return nil, nil
	default:
		return nil, nil
	}
}

// capsAndMore extracts the trailing capability list and whether a multiline
// continuation marker ("*") precedes it. For "CAP * LS * :a b c" the params
// after the subcommand are ["*", "a b c"]; for the final line they are just
// ["a b c"].
//
// The "*" here is the continuation marker, which is NOT the same token as the
// "*" target nick that an unregistered client sees in Param(0). We only ever
// inspect the parameters AFTER the subcommand (Params[2:]), so a "*" target
// never masquerades as "more lines coming". (Cross-checked against Ergo's
// irc/handlers.go sendCapLines, which emits "CAP <nick> LS * :<caps>" for
// continuation vs "CAP <nick> LS :<caps>" for the final line, with <nick>="*"
// pre-registration — the exact collision this guards against.)
func capsAndMore(m *irc.Message) (caps string, more bool) {
	// Everything after Param(1) is either [*, caps] or [caps].
	rest := m.Params
	if len(rest) <= 2 {
		// Param(0)=target, Param(1)=sub, no cap list at all.
		return "", false
	}
	tail := rest[2:]
	if len(tail) >= 2 && tail[0] == "*" {
		return tail[len(tail)-1], true
	}
	return tail[len(tail)-1], false
}

// parseCapList splits a space-separated cap list into (name, value, present)
// tuples and applies fn to each. Each token is "name" or "name=value"; a
// leading '-' (used in CAP DEL or REQ echoes) is left on the name for the
// caller to interpret.
func parseCapList(list string, fn func(name, value string, hasValue bool)) {
	for _, tok := range strings.Fields(list) {
		name, value, hasEq := strings.Cut(tok, "=")
		if name == "" {
			continue
		}
		fn(name, value, hasEq)
	}
}

// addAvailable records name→value in the advertised set, enforcing
// maxAvailableCaps. It returns false (recording nothing) when a previously unseen
// name would push the set past the ceiling, so a hostile server cannot grow it
// without bound by streaming endless distinct names — whether via multiline CAP
// LS continuations during registration, a long CAP LIST reply, or a CAP NEW flood
// afterwards. A name already present is always updated (and reported true).
func (n *Negotiator) addAvailable(name, value string) bool {
	if _, known := n.available[name]; !known && len(n.available) >= maxAvailableCaps {
		return false
	}
	n.available[name] = value
	return true
}

// enable adds name to the enabled set under the same maxAvailableCaps ceiling, so
// a flood of (possibly unsolicited) ACK or LIST entries from a hostile server
// cannot grow it without bound. A cap already enabled stays enabled.
func (n *Negotiator) enable(name string) {
	if _, on := n.enabled[name]; !on && len(n.enabled) >= maxAvailableCaps {
		return
	}
	n.enabled[name] = struct{}{}
}

// onLS records a CAP LS advertisement line, accumulating across multiline '*'
// continuations. When the final line arrives it commits the advertisement and,
// during initial negotiation, computes the CAP REQ payloads to send.
func (n *Negotiator) onLS(m *irc.Message) ([]string, error) {
	caps, more := capsAndMore(m)
	parseCapList(caps, func(name, value string, _ bool) {
		n.addAvailable(name, value)
	})
	if more {
		// More CAP LS lines are coming; keep collecting.
		n.collectingLS = true
		return nil, nil
	}
	n.collectingLS = false

	// Advertisement complete. During initial registration, request the
	// intersection of available and wanted. CAP NEW (not LS) handles the
	// post-registration path.
	return n.beginRequests(), nil
}

// beginRequests computes the REQ lines for available∩wanted and updates state.
// It returns nil (and may emit CAP END) when there is nothing to request.
func (n *Negotiator) beginRequests() []string {
	toReq := n.intersection()
	if len(toReq) == 0 {
		// Nothing to negotiate. Finish immediately.
		return n.finish()
	}
	lines := n.buildReqLines(toReq)
	n.pendingReqs += len(lines)
	n.state = StateRequesting
	return lines
}

// intersection returns the wanted caps that are advertised and not already
// enabled, preserving the caller's wanted order for deterministic output.
func (n *Negotiator) intersection() []string {
	var out []string
	for _, w := range n.wanted {
		if _, avail := n.available[w]; !avail {
			continue
		}
		if _, on := n.enabled[w]; on {
			continue
		}
		out = append(out, w)
	}
	return out
}

// buildReqLines packs caps into one or more "CAP REQ :<caps>" lines, each
// within the 512-byte line budget (including the trailing CRLF). A single cap
// that cannot fit on its own is still emitted on its own line (the server will
// NAK it rather than us silently dropping it).
func (n *Negotiator) buildReqLines(caps []string) []string {
	var lines []string
	// Room for caps on one line = budget - len("CAP REQ :") - len("\r\n").
	budget := lineBudget - len(reqPrefix) - crlfLen

	var cur []string
	curLen := 0
	flush := func() {
		if len(cur) == 0 {
			return
		}
		lines = append(lines, reqPrefix+strings.Join(cur, " "))
		cur = cur[:0]
		curLen = 0
	}
	for _, c := range caps {
		// Added length: the cap plus a separating space if not first.
		add := len(c)
		if len(cur) > 0 {
			add++ // space
		}
		if len(cur) > 0 && curLen+add > budget {
			flush()
			add = len(c) // first on the new line, no leading space
		}
		cur = append(cur, c)
		curLen += add
	}
	flush()
	return lines
}

// onACK applies a server ACK. Per the spec REQ is atomic, so every cap named in
// the ACK is enabled (or disabled, for "-name" entries) together. The ACK
// resolves one outstanding REQ line and advances toward SASL or CAP END.
//
// We only enable caps the client actually wanted (and therefore REQ'd): a
// conformant server only ACKs what we asked for, but a misbehaving or hostile
// one could ACK arbitrary caps, which would otherwise flip on behaviour gated by
// IsEnabled (e.g. Typing checking message-tags) without the client ever asking.
func (n *Negotiator) onACK(m *irc.Message) ([]string, error) {
	caps, _ := capsAndMore(m)
	parseCapList(caps, func(name, _ string, _ bool) {
		if rest, neg := strings.CutPrefix(name, "-"); neg {
			delete(n.enabled, rest)
			return
		}
		if _, wanted := n.wantedSet[name]; !wanted {
			// Not a cap we requested: do not enable it.
			return
		}
		n.enable(name)
		if name == "sasl" {
			n.saslAcked = true
		}
	})
	return n.afterVerdict(), nil
}

// onNAK applies a server NAK. The REQ changed nothing, so no caps are enabled;
// the NAK simply resolves one outstanding REQ line. If sasl was among the
// rejected caps, it stays un-ACKed and NeedSASL remains false.
//
// The NAK's cap list is deliberately not consulted: the spec only obliges the
// server to echo the first 100 characters of the rejected REQ, so the list may
// be truncated and cannot be used for accounting.
func (n *Negotiator) onNAK(m *irc.Message) ([]string, error) {
	return n.afterVerdict(), nil
}

// afterVerdict resolves one outstanding REQ line (each ACK/NAK answers exactly
// one REQ) and, when all initial requests are resolved, either hands off to
// SASL or finishes negotiation. Verdicts that arrive after registration (from
// CAP NEW-driven REQs) do not re-trigger CAP END; an unsolicited verdict with
// nothing outstanding is ignored.
func (n *Negotiator) afterVerdict() []string {
	if n.pendingReqs == 0 {
		// Unsolicited verdict with no REQ outstanding (a misbehaving or hostile
		// server): there is nothing to resolve, and it must not advance
		// negotiation (e.g. trigger a premature CAP END mid-LS or mid-SASL).
		return nil
	}
	n.pendingReqs--
	if n.registered || n.pendingReqs > 0 {
		return nil
	}
	// All initial REQs resolved.
	if n.needSASL() {
		n.state = StateWaitingSASL
		return nil
	}
	return n.finish()
}

// SASLComplete is called by the client once the SASL exchange has finished
// (whether it succeeded or failed — failure handling is the client's policy).
// It releases the negotiation hold and returns the CAP END line to send. It is
// a no-op returning nil if SASL was not pending.
func (n *Negotiator) SASLComplete() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.saslDone {
		return nil
	}
	n.saslDone = true
	if n.state != StateWaitingSASL {
		return nil
	}
	return n.finish()
}

// finish marks initial negotiation complete, transitions to StateDone, and
// returns the CAP END line. It is only meaningful during initial registration;
// post-registration CAP NEW/DEL never call it.
func (n *Negotiator) finish() []string {
	n.state = StateDone
	n.registered = true
	return []string{capEndLine}
}

// onList records a CAP LIST reply (currently-enabled caps). It rebuilds the
// enabled set from the (possibly multiline) reply so the client can reconcile
// its view with the server's. The first line of a fresh LIST reply replaces the
// prior set; continuation lines add to it.
func (n *Negotiator) onList(m *irc.Message) {
	caps, more := capsAndMore(m)
	if !n.collectingLIST {
		// Start of a new LIST reply: reset the enabled set.
		n.enabled = make(map[string]struct{})
	}
	parseCapList(caps, func(name, _ string, _ bool) {
		n.enable(name)
		if name == "sasl" {
			n.saslAcked = true
		}
	})
	n.collectingLIST = more
}

// onNew records caps newly offered by the server post-registration (CAP NEW).
// It adds them to Available and, for any that are wanted and not yet enabled,
// emits a CAP REQ. These REQs are tracked but do not affect CAP END (which has
// already been sent).
func (n *Negotiator) onNew(m *irc.Message) ([]string, error) {
	caps, _ := capsAndMore(m)
	var toReq []string
	parseCapList(caps, func(name, value string, _ bool) {
		if !n.addAvailable(name, value) {
			// Advertised set is at its bound; ignore further novel names so a
			// CAP NEW flood cannot grow it without limit post-registration.
			return
		}
		if _, want := n.wantedSet[name]; !want {
			return
		}
		if _, on := n.enabled[name]; on {
			return
		}
		toReq = append(toReq, name)
	})
	if len(toReq) == 0 {
		return nil, nil
	}
	lines := n.buildReqLines(toReq)
	n.pendingReqs += len(lines)
	return lines, nil
}

// onDel removes caps the server has withdrawn post-registration (CAP DEL),
// dropping them from both Available and Enabled. Per the spec the named caps are
// disabled without a client REQ.
func (n *Negotiator) onDel(m *irc.Message) {
	caps, _ := capsAndMore(m)
	parseCapList(caps, func(name, _ string, _ bool) {
		delete(n.available, name)
		delete(n.enabled, name)
		if name == "sasl" {
			n.saslAcked = false
		}
	})
}
