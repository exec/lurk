package sasl

import (
	"encoding/base64"
	"fmt"

	"github.com/exec/lurk/irc"
)

// SASL result numerics (sasl-3.1 / 3.2), aliased from package irc's numerics so
// the switch in Receive reads clearly and stays in lock-step with the parser.
const (
	numLoggedIn    = irc.RPL_LOGGEDIN    // 900
	numLoggedOut   = irc.RPL_LOGGEDOUT   // 901
	numNickLocked  = irc.ERR_NICKLOCKED  // 902
	numSASLSuccess = irc.RPL_SASLSUCCESS // 903
	numSASLFail    = irc.ERR_SASLFAIL    // 904
	numSASLTooLong = irc.ERR_SASLTOOLONG // 905
	numSASLAborted = irc.ERR_SASLABORTED // 906
	numSASLAlready = irc.ERR_SASLALREADY // 907
	numSASLMechs   = irc.RPL_SASLMECHS   // 908
)

// cmdAuthenticate is the AUTHENTICATE command verb.
const cmdAuthenticate = irc.AUTHENTICATE

// abortLine is the line a client sends to abort an in-progress SASL exchange.
const abortLine = cmdAuthenticate + " *"

// Conversation drives a single SASL authentication exchange for one Mechanism.
//
// It follows the IRCv3 SASL flow: the client sends "AUTHENTICATE <mech>" and
// then waits for the server to reply "AUTHENTICATE +" (the first, empty
// challenge) before sending its initial response. Begin therefore emits only
// the mechanism-selection line; the mechanism's initial response (if any) is
// sent in reply to that first server AUTHENTICATE inside Receive.
//
// Usage from the client run-loop:
//
//	conv := sasl.NewConversation(sasl.Plain("", "jilles", "sesame"))
//	for _, line := range conv.Begin() {
//	    conn.Send(line)
//	}
//	// then, for every inbound message during SASL:
//	lines, done, err := conv.Receive(msg)
//	for _, line := range lines {
//	    conn.Send(line)
//	}
//	if err != nil { /* SASL failed; decide whether to abort registration */ }
//	if done { /* SASL succeeded; proceed to CAP END */ }
//
// A Conversation is single-use and is not safe for concurrent use.
type Conversation struct {
	mech Mechanism
	done bool // exchange has terminated (success or failure)

	// initial holds the mechanism's initial response, sent in reply to the first
	// server AUTHENTICATE challenge (not eagerly in Begin). hasInitial reports
	// whether the mechanism supplied one; initialSent guards against sending it
	// more than once.
	initial     []byte
	hasInitial  bool
	initialSent bool
}

// NewConversation returns a Conversation that will authenticate using m.
func NewConversation(m Mechanism) *Conversation {
	c := &Conversation{mech: m}
	c.initial, c.hasInitial = m.Start()
	return c
}

// Session is an alias for Conversation, provided so callers that prefer the
// "session" vocabulary can use NewSession/Start/Continue. The two names refer to
// the same single-use SASL driver.
type Session = Conversation

// NewSession is an alias for NewConversation.
func NewSession(m Mechanism) *Session { return NewConversation(m) }

// Start is an alias for Begin: it returns the opening line of the exchange
// ("AUTHENTICATE <mech>").
func (c *Conversation) Start() []string { return c.Begin() }

// Continue is an alias for Receive: feed it every inbound AUTHENTICATE / 9xx
// message during the SASL window; it returns the lines to send, whether SASL
// completed successfully (done), and a non-nil error on rejection or failure.
func (c *Conversation) Continue(msg *irc.Message) (lines []string, done bool, err error) {
	return c.Receive(msg)
}

// Begin returns the single line that opens the exchange, "AUTHENTICATE <mech>".
// Per the IRCv3 SASL flow the client must then wait for the server's first
// "AUTHENTICATE +" before sending its response; the mechanism's initial response
// is emitted by Receive in reply to that first challenge, not here. The caller
// sends this line and then feeds every subsequent inbound message to Receive.
func (c *Conversation) Begin() []string {
	return []string{cmdAuthenticate + " " + c.mech.Name()}
}

// Receive processes one inbound message during a SASL exchange. It returns the
// lines to send in response, whether the exchange has completed successfully
// (done), and a non-nil error if the exchange failed or was rejected by the
// server.
//
// Messages other than AUTHENTICATE and the 900-908 numerics are ignored
// (returning no lines, not done, no error) so the caller can pass through
// unrelated traffic without filtering.
//
// On the first server AUTHENTICATE challenge (the empty "+" prompt), Receive
// emits the mechanism's initial response if it has one; on subsequent challenges
// it decodes the payload and calls the mechanism's Next. The response is chunked
// into AUTHENTICATE lines. If the mechanism returns an error, Receive emits an
// abort line ("AUTHENTICATE *") and returns that error.
//
// Result numerics are interpreted per spec:
//
//   - 900 (RPL_LOGGEDIN) records the account but is not itself terminal; 903
//     (RPL_SASLSUCCESS) completes the exchange successfully.
//   - 904/905/906/907 (fail/too-long/aborted/already) and 902 (nick locked)
//     terminate with an error.
//   - 908 (RPL_SASLMECHS) reports the server's mechanism list after a failure
//     and is surfaced as an error carrying that list.
//
// Once the exchange has terminated, further calls return an error.
func (c *Conversation) Receive(msg *irc.Message) (lines []string, done bool, err error) {
	if c.done {
		return nil, true, fmt.Errorf("sasl: Receive called after the exchange completed")
	}

	switch msg.Command {
	case cmdAuthenticate:
		return c.handleChallenge(msg)

	case numSASLSuccess:
		c.done = true
		return nil, true, nil

	case numLoggedIn:
		// Informational: the account name is in the params. Authentication is
		// only confirmed by 903, so do not finish here.
		return nil, false, nil

	case numSASLMechs:
		c.done = true
		return nil, false, fmt.Errorf("sasl: authentication failed; server mechanisms: %s", msg.Param(1))

	case numSASLFail, numSASLTooLong, numSASLAborted, numSASLAlready, numNickLocked, numLoggedOut:
		c.done = true
		return nil, false, fmt.Errorf("sasl: authentication rejected (%s): %s", msg.Command, lastParam(msg))

	default:
		// Unrelated message during the SASL window; pass through.
		return nil, false, nil
	}
}

// handleChallenge answers a server "AUTHENTICATE" line. The first such line is
// the empty "+" prompt: if the mechanism supplied an initial response, it is
// sent in reply (without invoking Next). Otherwise, and for any later challenge,
// the payload is decoded and passed to the mechanism's Next.
func (c *Conversation) handleChallenge(msg *irc.Message) (lines []string, done bool, err error) {
	if c.hasInitial && !c.initialSent {
		c.initialSent = true
		return chunkResponse(c.initial), false, nil
	}

	challenge, err := decodeChallenge(msg.Param(0))
	if err != nil {
		c.done = true
		return []string{abortLine}, false, fmt.Errorf("sasl: decoding server challenge: %w", err)
	}
	resp, err := c.mech.Next(challenge)
	if err != nil {
		c.done = true
		return []string{abortLine}, false, err
	}
	return chunkResponse(resp), false, nil
}

// decodeChallenge decodes the payload of a server AUTHENTICATE line. The single
// payload token "+" denotes an empty challenge. The mechanisms in this package
// (PLAIN, EXTERNAL) only ever receive the single empty "+" challenge, so each
// AUTHENTICATE line is decoded independently rather than reassembled across
// continuation lines.
//
// A single chunk longer than the 400-byte SASL line limit is rejected, matching
// ircv3's sasl-3.1 framing and Ergo's server-side SASLBuffer (which returns
// ErrSASLTooLong for an over-long chunk); this guards against a malformed or
// hostile server payload.
func decodeChallenge(payload string) ([]byte, error) {
	if payload == "" || payload == "+" {
		return nil, nil
	}
	if len(payload) > maxChunk {
		return nil, fmt.Errorf("sasl: server challenge chunk exceeds %d-byte limit", maxChunk)
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("sasl: invalid base64 in challenge: %w", err)
	}
	return data, nil
}

// lastParam returns the final parameter of msg (the human-readable description
// on a result numeric), or "" if there are none.
func lastParam(msg *irc.Message) string {
	if n := len(msg.Params); n > 0 {
		return msg.Params[n-1]
	}
	return ""
}
