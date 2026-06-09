package irc

import (
	"errors"
	"fmt"
	"strings"
)

// Serialize errors. These guard against producing a line that would be
// truncated, split, or misparsed by a peer.
var (
	// ErrNoCommand is returned when Command is empty.
	ErrNoCommand = errors.New("irc: cannot serialize message with empty command")
	// ErrIllegalByte is returned when a non-tag field contains a byte that is
	// illegal on the wire: NUL, CR, or LF.
	ErrIllegalByte = errors.New("irc: field contains CR, LF, or NUL")
)

// Serialize renders the Message into a single wire line terminated by CRLF. It
// is the inverse of Parse: a Message obtained from Parse round-trips back to an
// equivalent message (tag order aside, which is not significant).
//
// The final parameter is emitted as a trailing (':'-prefixed) param when it is
// empty, contains a space, or begins with ':'. Tag values are escaped per the
// message-tags spec. CR, LF, and NUL are rejected in every non-tag field
// because they would terminate or split the line; in tag values CR and LF are
// escaped instead and NUL (which has no escape) is rejected. A space in the
// source, or a tag key containing a byte that would end or split the tag
// segment (space, ';', '=', CR, LF, NUL), is rejected too: each would shift
// the frame so the line reparses as a different message.
func (m *Message) Serialize() (string, error) {
	if m.Command == "" {
		return "", ErrNoCommand
	}

	var b strings.Builder

	if len(m.Tags) > 0 {
		seg, err := serializeTags(m.Tags)
		if err != nil {
			return "", err
		}
		b.WriteByte('@')
		b.WriteString(seg)
		b.WriteByte(' ')
	}

	if m.Source != "" {
		if err := checkField("source", m.Source); err != nil {
			return "", err
		}
		// A space would end the source token early and promote the remainder
		// to the command position — a frame shift, like the command checks
		// below — so reject it rather than emit a self-corrupting line.
		if strings.ContainsRune(m.Source, ' ') {
			return "", fmt.Errorf("irc: source %q contains a space", m.Source)
		}
		b.WriteByte(':')
		b.WriteString(m.Source)
		b.WriteByte(' ')
	}

	if err := checkField("command", m.Command); err != nil {
		return "", err
	}
	if strings.ContainsRune(m.Command, ' ') {
		return "", fmt.Errorf("irc: command %q contains a space", m.Command)
	}
	// The command must begin with a letter or digit (per the grammar: a name or a
	// numeric). A leading ':' or '@' would be reparsed as a source or tag segment
	// — i.e. the serialized line would not frame back to this message — so reject
	// it rather than emit a self-corrupting line.
	if !isCommandStart(m.Command[0]) {
		return "", fmt.Errorf("irc: command %q does not begin with a letter or digit", m.Command)
	}
	b.WriteString(m.Command)

	for i, p := range m.Params {
		if err := checkField("param", p); err != nil {
			return "", err
		}
		last := i == len(m.Params)-1
		trailing := last && (p == "" || strings.ContainsRune(p, ' ') || strings.HasPrefix(p, ":"))
		if !last && strings.ContainsRune(p, ' ') {
			return "", fmt.Errorf("irc: non-final param %q contains a space", p)
		}
		if !last && p == "" {
			return "", fmt.Errorf("irc: non-final param at index %d is empty", i)
		}
		// A non-final param beginning with ':' would be reparsed as the trailing
		// param (swallowing the rest of the line), so it cannot be serialized
		// unambiguously in a non-final position.
		if !last && strings.HasPrefix(p, ":") {
			return "", fmt.Errorf("irc: non-final param %q begins with ':'", p)
		}
		b.WriteByte(' ')
		if trailing {
			b.WriteByte(':')
		}
		b.WriteString(p)
	}

	b.WriteString("\r\n")
	return b.String(), nil
}

// isCommandStart reports whether b is a valid first byte of a command token: an
// ASCII letter (a command name) or digit (a numeric reply).
func isCommandStart(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// checkField rejects bytes that cannot appear in a non-tag wire field: NUL, CR,
// and LF would terminate or split the line.
func checkField(name, s string) error {
	if i := strings.IndexAny(s, "\x00\r\n"); i >= 0 {
		return fmt.Errorf("irc: %s: %w", name, ErrIllegalByte)
	}
	return nil
}
