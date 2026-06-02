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
// because they would terminate or split the line; in tag values they are
// escaped instead.
func (m *Message) Serialize() (string, error) {
	if m.Command == "" {
		return "", ErrNoCommand
	}

	var b strings.Builder

	if len(m.Tags) > 0 {
		b.WriteByte('@')
		b.WriteString(serializeTags(m.Tags))
		b.WriteByte(' ')
	}

	if m.Source != "" {
		if err := checkField("source", m.Source); err != nil {
			return "", err
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
		b.WriteByte(' ')
		if trailing {
			b.WriteByte(':')
		}
		b.WriteString(p)
	}

	b.WriteString("\r\n")
	return b.String(), nil
}

// checkField rejects bytes that cannot appear in a non-tag wire field: NUL, CR,
// and LF would terminate or split the line.
func checkField(name, s string) error {
	if i := strings.IndexAny(s, "\x00\r\n"); i >= 0 {
		return fmt.Errorf("irc: %s: %w", name, ErrIllegalByte)
	}
	return nil
}
