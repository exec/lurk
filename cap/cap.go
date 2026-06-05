// Package cap implements IRCv3 capability negotiation as a pure state machine.
//
// The Negotiator consumes parsed CAP messages (as *irc.Message) together with
// the set of capabilities the client wants, and emits the protocol lines the
// client should send. It performs no I/O of its own: the caller is responsible
// for reading messages off the wire, feeding them to Receive, and writing the
// returned lines back out. This keeps the negotiation logic deterministic and
// trivially testable.
//
// Typical lifecycle, driven by the client run-loop:
//
//	n := cap.NewNegotiator(wanted)
//	send(n.Start())                       // "CAP LS 302"
//	for msg := range capMessages {
//	    lines, err := n.Receive(msg)
//	    for _, l := range lines { send(l) }
//	    if n.NeedSASL() {                 // sasl was ACKed and is wanted
//	        runSASL(n.SASLMechs())        // owned by the sasl package
//	        for _, l := range n.SASLComplete() { send(l) }
//	    }
//	    if n.State() == cap.StateDone { break }
//	}
//
// CAP NEW / CAP DEL are handled after negotiation completes: Receive keeps
// updating Available()/Enabled() and may emit a fresh CAP REQ for newly offered
// wanted caps. It never emits CAP END for post-registration changes.
package cap

import (
	"sort"
	"strings"
	"sync"

	"github.com/exec/lurk/irc"
)

// lineBudget is the classic IRC message length limit including the trailing
// CRLF. CAP REQ payloads are split so that each emitted line stays within it.
const lineBudget = 512

// reqPrefix is the fixed head of a "CAP REQ :<caps>" line; crlfLen accounts for
// the trailing CRLF when measuring against the line budget.
var reqPrefix = irc.CAP + " " + irc.CAP_REQ + " :"

const crlfLen = 2

// capEndLine is the line that finishes negotiation and resumes registration.
var capEndLine = irc.CAP + " " + irc.CAP_END

// capLSLine opens negotiation: CAP LS at version 302 (enables multiline LS,
// cap values, and implicit cap-notify).
var capLSLine = irc.CAP + " " + irc.CAP_LS + " 302"

// maxAvailableCaps caps how many distinct advertised capabilities are retained.
// Real servers advertise a few dozen; the bound guards against a hostile server
// growing the available set without limit via a flood of distinct CAP NEW names
// after registration (when no handshake timeout is in play).
const maxAvailableCaps = 1024

// State is the high-level phase of capability negotiation.
type State int

const (
	// StateInit is the state before Start has been called.
	StateInit State = iota
	// StateListing means CAP LS has been sent and we are collecting the
	// (possibly multiline) advertisement.
	StateListing
	// StateRequesting means one or more CAP REQ have been sent and we are
	// waiting for the matching ACK/NAK responses.
	StateRequesting
	// StateWaitingSASL means sasl was successfully requested and the client
	// should run the SASL exchange before negotiation can finish. The client
	// signals completion via SASLComplete.
	StateWaitingSASL
	// StateDone means CAP END has been emitted (or negotiation finished with
	// nothing to request). The connection registration can proceed. The
	// Negotiator continues to process CAP NEW/DEL in this state.
	StateDone
)

// String renders a State for logging and test output.
func (s State) String() string {
	switch s {
	case StateInit:
		return "Init"
	case StateListing:
		return "Listing"
	case StateRequesting:
		return "Requesting"
	case StateWaitingSASL:
		return "WaitingSASL"
	case StateDone:
		return "Done"
	default:
		return "Unknown"
	}
}

// Negotiator drives one connection's capability negotiation. The run-loop
// owns mutation (it feeds CAP messages to Receive from a single goroutine),
// but the read-only queries (IsEnabled, Enabled, Available, NeedSASL, State,
// SASLMechs) may be called concurrently from other goroutines — e.g. the TUI
// calling IsEnabled on every TAGMSG. The mu mutex guards every access to the
// maps and negotiation fields so those concurrent reads cannot race the
// run-loop's writes. Public entry points lock mu; the internal helpers they
// call assume it is already held.
type Negotiator struct {
	// mu guards all fields below for the duration of any public method call. It
	// is non-reentrant: internal helpers must use the unlocked variants (e.g.
	// needSASL) rather than calling a public method that re-locks.
	mu sync.Mutex

	// wanted is the set of caps the client would like to enable, in the order
	// the caller supplied (used for stable, deterministic REQ ordering).
	wanted    []string
	wantedSet map[string]struct{}

	// available maps every advertised cap name to its value ("" if value-less).
	available map[string]string

	// enabled is the set of caps the server has ACKed and not since removed.
	enabled map[string]struct{}

	// requested counts caps we have sent in a REQ and are still awaiting a
	// verdict for. Negotiation during initial registration is complete once
	// this reaches zero (and SASL, if any, is resolved).
	pendingReqs int

	// collectingLS is true while a multiline CAP LS (the '*' continuation) is
	// still in progress; collectingLIST is the same for CAP LIST replies.
	collectingLS   bool
	collectingLIST bool

	state State

	// saslWanted records whether the caller asked for the sasl cap; saslAcked
	// records whether the server granted it. Together they decide NeedSASL.
	saslWanted bool
	saslAcked  bool
	saslDone   bool

	// registered flips true once initial negotiation finished (CAP END sent or
	// nothing to do). After this, ACK/NAK from CAP NEW-driven REQs no longer
	// gate CAP END.
	registered bool
}

// NewNegotiator returns a Negotiator that will try to enable the given wanted
// capabilities. Duplicate or empty entries are ignored. The wanted slice is
// copied; the caller may reuse it.
func NewNegotiator(wanted []string) *Negotiator {
	n := &Negotiator{
		wantedSet: make(map[string]struct{}, len(wanted)),
		available: make(map[string]string),
		enabled:   make(map[string]struct{}),
		state:     StateInit,
	}
	for _, w := range wanted {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		if _, dup := n.wantedSet[w]; dup {
			continue
		}
		n.wantedSet[w] = struct{}{}
		n.wanted = append(n.wanted, w)
		if w == "sasl" {
			n.saslWanted = true
		}
	}
	return n
}

// Start returns the opening line the client should send to begin negotiation
// ("CAP LS 302", requesting CAP version 302 for multiline LS and cap values)
// and moves the Negotiator into the Listing state. It is idempotent only in the
// sense that calling it again simply re-returns the line; call it once.
func (n *Negotiator) Start() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.state = StateListing
	n.collectingLS = true
	return capLSLine
}

// State reports the current negotiation phase.
func (n *Negotiator) State() State {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.state
}

// NeedSASL reports whether the client should run the SASL exchange now: the
// sasl cap was wanted, the server ACKed it, and SASL has not yet been completed.
// When true, the Negotiator is in StateWaitingSASL and will not emit CAP END
// until SASLComplete is called.
func (n *Negotiator) NeedSASL() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.needSASL()
}

// needSASL is the unlocked body of NeedSASL, for internal callers that already
// hold n.mu.
func (n *Negotiator) needSASL() bool {
	return n.saslWanted && n.saslAcked && !n.saslDone
}

// Available returns a copy of the advertised capabilities mapped to their
// values (the empty string for value-less caps). The returned map is owned by
// the caller.
func (n *Negotiator) Available() map[string]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[string]string, len(n.available))
	for k, v := range n.available {
		out[k] = v
	}
	return out
}

// Enabled returns the sorted list of capabilities currently enabled (ACKed and
// not subsequently disabled or removed via CAP DEL).
func (n *Negotiator) Enabled() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.enabled))
	for k := range n.enabled {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsEnabled reports whether a specific capability is currently enabled.
func (n *Negotiator) IsEnabled(capName string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.enabled[capName]
	return ok
}

// SASLMechs returns the SASL mechanisms advertised via the "sasl" cap value
// (e.g. sasl=PLAIN,EXTERNAL → ["PLAIN","EXTERNAL"]), in advertised order. It
// returns nil when sasl is not advertised or was advertised without a value
// (sasl-3.1 style), in which case the client falls back to its configured
// mechanism.
func (n *Negotiator) SASLMechs() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	v, ok := n.available["sasl"]
	if !ok || v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
