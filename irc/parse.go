package irc

import (
	"errors"
	"strings"
)

// maxParamCount is the maximum number of parameters Parse will collect. The
// IRC spec defines a conventional limit of 15 parameters per message (RFC
// 1459 §2.3), and no widely-deployed command uses more than a handful.
// Without an explicit cap, a caller that passes an unbounded string directly
// to Parse (bypassing the conn-layer budget) could receive a Params slice
// with thousands of entries. Excess parameters are silently dropped, matching
// the behaviour of irc-go and Ergo's own parser.
const maxParamCount = 32

// Wire length budgets from the Modern IRC and message-tags specifications.
// These are exported so transport/framing code can enforce a shared, correct
// limit instead of hardcoding magic numbers; the parser itself stays tolerant
// and does not reject on length (the server is the authority that answers
// ERR_INPUTTOOLONG). Values match the canonical irc-go constants.
const (
	// MaxLenTags is the size limit for the tag segment, including the leading
	// '@' and the trailing space.
	MaxLenTags = 8191

	// MaxLenTagData is MaxLenTags minus the '@' and the space.
	MaxLenTagData = MaxLenTags - 2

	// MaxLenClientTagData is the cap on client-added tag data ("Clients MUST
	// NOT send messages with tag data exceeding 4094 bytes").
	MaxLenClientTagData = 4094

	// MaxLenServerTagData is the cap on server-added tag data ("Servers MUST
	// NOT add tag data exceeding 4094 bytes").
	MaxLenServerTagData = 4094

	// MaxLenMessage is the classic message-body budget, including the trailing
	// CRLF. Over-length lines draw ERR_INPUTTOOLONG (417) from the server.
	MaxLenMessage = 512
)

// ErrEmptyMessage is returned by Parse when the line has no command. A bare
// line, or one consisting only of tags and/or a source, has nothing to act on.
var ErrEmptyMessage = errors.New("irc: message has no command")

// ErrBadLineChar is returned by Parse when the line, after the trailing
// terminator is removed, still contains a NUL, CR, or LF. Such a byte cannot
// legitimately appear inside a single message: it would split or terminate the
// line, so a message carrying one mid-body is malformed (or a smuggling
// attempt) and is rejected rather than parsed into something misleading. This
// mirrors the canonical irc-go parser's ErrorLineContainsBadChar.
var ErrBadLineChar = errors.New("irc: line contains NUL, CR, or LF")

// Parse decodes a single IRC protocol line into a Message. The line should not
// include the trailing CRLF; if it does, the CR and LF are stripped. Parse is
// tolerant: tags and source are optional, surplus spaces between fields are
// skipped, and a trailing param (':'-prefixed) is captured verbatim. It returns
// ErrEmptyMessage if no command is present, and ErrBadLineChar if the body
// contains an embedded NUL/CR/LF after the terminator is removed.
//
// The wire grammar parsed is:
//
//	['@' <tags> SPACE] [':' <source> SPACE] <command> {SPACE <param>} [SPACE ':' <trailing>]
func Parse(line string) (*Message, error) {
	// Remove a single trailing terminator the caller may not have stripped. We
	// peel at most one '\n' and then one '\r' (the canonical "\r\n" order)
	// rather than greedily trimming, so that any *other* CR/LF is left in place
	// to be caught by the bad-char check below instead of being silently eaten.
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")

	// After the terminator is gone, no NUL/CR/LF may remain: those bytes can
	// only have been smuggled into the body, which would corrupt framing.
	if strings.IndexAny(line, "\x00\r\n") >= 0 {
		return nil, ErrBadLineChar
	}

	m := &Message{}

	// Tags: an '@'-prefixed segment terminated by the first space.
	if strings.HasPrefix(line, "@") {
		seg, rest, ok := cutSpace(line[1:])
		if !ok {
			// Tags with no following command: nothing actionable.
			return nil, ErrEmptyMessage
		}
		m.Tags = parseTags(seg)
		line = rest
	}

	// Skip any extra spaces between fields.
	line = strings.TrimLeft(line, " ")

	// Source: a ':'-prefixed segment terminated by the first space.
	if strings.HasPrefix(line, ":") {
		seg, rest, ok := cutSpace(line[1:])
		if !ok {
			// Source with no following command.
			return nil, ErrEmptyMessage
		}
		m.Source = seg
		line = rest
	}

	line = strings.TrimLeft(line, " ")

	// Command: the next space-delimited token. It is required.
	cmd, rest, _ := cutSpace(line)
	if cmd == "" {
		return nil, ErrEmptyMessage
	}
	m.Command = cmd
	line = rest

	// Params: space-delimited tokens until a ':'-prefixed trailing param, which
	// consumes the remainder of the line (and may contain spaces). At most
	// maxParamCount parameters are stored; any further tokens are silently
	// dropped (they cannot affect command dispatch for any standard message).
	//
	// The slice is preallocated to cover the overwhelming majority of messages
	// (a PRIVMSG has 2, most numerics a handful) in a single allocation rather
	// than growing from nil on every inbound line. It is left as a local until
	// the loop ends so that a paramless message keeps a nil Params (matching the
	// zero value) rather than an empty non-nil slice.
	params := make([]string, 0, 4)
	for len(params) < maxParamCount {
		line = strings.TrimLeft(line, " ")
		if line == "" {
			break
		}
		if line[0] == ':' {
			params = append(params, line[1:])
			break
		}
		tok, rest, _ := cutSpace(line)
		params = append(params, tok)
		line = rest
	}
	if len(params) > 0 {
		m.Params = params
	}

	return m, nil
}

// cutSpace splits s at the first space, returning the part before, the part
// after, and whether a space was found. When no space is present, after is ""
// and found is false.
func cutSpace(s string) (before, after string, found bool) {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}
