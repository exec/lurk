package sasl

import (
	"encoding/base64"
	"fmt"
	"strings"

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

// maxChallengeBytes is the maximum total decoded size of a reassembled server
// challenge. It caps the number of 400-byte base64 chunks a hostile server
// may send before the terminal chunk arrives, preventing a chunk-count
// exhaustion attack that would spin the run-loop accumulating data forever.
// 8 KiB is far above any real SCRAM server-first-message (which tops out at
// a few hundred bytes) while still being generous to future mechanisms.
const maxChallengeBytes = 8 * 1024

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

	// challBuf accumulates the base64-encoded chunks of a multi-line server
	// challenge. IRCv3 SASL uses the same framing rule as client→server: a
	// chunk of exactly maxChunk (400) bytes signals "more follows"; a chunk
	// shorter than maxChunk, or the bare "+" token, signals "end of challenge".
	// challBuf holds the concatenated base64 strings across chunks; once the
	// terminal chunk arrives the buffer is decoded and passed to mech.Next.
	challBuf strings.Builder
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

// handleChallenge processes one server "AUTHENTICATE" line, accumulating
// multi-chunk challenges before dispatching to the mechanism.
//
// IRCv3 SASL uses the same framing rule in both directions: a payload of
// exactly maxChunk (400) base64 bytes signals "more chunks follow"; a payload
// shorter than maxChunk, or the bare "+" token, signals "end of challenge".
// Chunks are concatenated into challBuf; once the terminal chunk arrives the
// buffer is base64-decoded and the raw bytes are passed to mech.Next.
//
// Special case: the very first AUTHENTICATE line is the server's empty "+"
// prompt that starts the exchange. If the mechanism supplied an initial
// response (hasInitial), we send it immediately without touching challBuf —
// no reassembly is needed because an empty "+" is always a standalone token,
// not part of a multi-chunk sequence.
func (c *Conversation) handleChallenge(msg *irc.Message) (lines []string, done bool, err error) {
	payload := msg.Param(0)

	// First challenge: the server's empty "+" prompt.
	if c.hasInitial && !c.initialSent {
		// The initial prompt is always a standalone "+" (never a multi-chunk
		// sequence), so dispatch immediately and reset the buffer just in case.
		c.initialSent = true
		c.challBuf.Reset()
		return chunkResponse(c.initial), false, nil
	}

	// Validate and accumulate this chunk.
	if payload != "+" && payload != "" {
		if len(payload) > maxChunk {
			c.done = true
			c.challBuf.Reset()
			return []string{abortLine}, false, fmt.Errorf("sasl: server challenge chunk exceeds %d-byte limit", maxChunk)
		}
		// Guard against chunk-count exhaustion: reject early if we already know
		// the reassembled base64 would decode past maxChallengeBytes. A base64
		// byte encodes 6 bits, so 4 base64 chars → 3 raw bytes; the ceiling in
		// base64 chars is maxChallengeBytes * 4/3, conservatively rounded up.
		const maxChallengeB64 = (maxChallengeBytes*4 + 2) / 3
		if c.challBuf.Len()+len(payload) > maxChallengeB64 {
			c.done = true
			c.challBuf.Reset()
			return []string{abortLine}, false, fmt.Errorf("sasl: server challenge exceeds %d-byte limit", maxChallengeBytes)
		}
		c.challBuf.WriteString(payload)
	}

	// A chunk shorter than maxChunk (or "+" / empty) terminates the challenge.
	if len(payload) == maxChunk {
		// Exactly maxChunk bytes: more chunks are coming; wait for the next line.
		return nil, false, nil
	}

	// Terminal chunk received — decode the assembled base64 and dispatch.
	assembled := c.challBuf.String()
	c.challBuf.Reset()

	var challenge []byte
	if assembled != "" {
		var decErr error
		challenge, decErr = base64.StdEncoding.DecodeString(assembled)
		if decErr != nil {
			c.done = true
			return []string{abortLine}, false, fmt.Errorf("sasl: decoding server challenge: %w", decErr)
		}
	}

	resp, err := c.mech.Next(challenge)
	if err != nil {
		c.done = true
		return []string{abortLine}, false, err
	}
	return chunkResponse(resp), false, nil
}

// lastParam returns the final parameter of msg (the human-readable description
// on a result numeric), or "" if there are none. This is intentionally placed
// after handleChallenge so the file reads top-to-bottom in flow order.
func lastParam(msg *irc.Message) string {
	if n := len(msg.Params); n > 0 {
		return msg.Params[n-1]
	}
	return ""
}
