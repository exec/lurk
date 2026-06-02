// Package irc implements the IRCv3 client protocol message layer: parsing and
// serializing the wire format described in the "Modern IRC" document and the
// IRCv3 message-tags specification.
//
// This file defines the Message type, the central data structure that flows
// through the entire client. It is the stable contract that every other
// package (conn, cap, sasl, isupport, client) builds upon. Parsing and
// serialization logic live in sibling files (parse.go, serialize.go, tags.go).
package irc

// Message is a single parsed IRC protocol message.
//
// Wire format (IRCv3):
//
//	['@' <tags> SPACE] [':' <source> SPACE] <command> {SPACE <param>} crlf
//
// The final parameter may contain spaces and is, on the wire, prefixed with a
// colon (the "trailing" parameter). Parsing collapses that distinction: Params
// holds every parameter in order, and serialization decides which one needs the
// colon. Code reading a Message should never need to know whether a parameter
// was sent as trailing.
type Message struct {
	// Tags holds IRCv3 message tags (the '@'-prefixed segment). Keys are stored
	// without the leading '@'; client-only tags retain their '+' prefix in the
	// key. Values are stored already-unescaped. A tag present with no value (a
	// bare key on the wire) has the empty string as its value; use Tags.Has to
	// distinguish "present and empty" from "absent".
	Tags Tags

	// Source is the message prefix (the ':'-prefixed segment) with the leading
	// ':' removed. It is either a server name or a "nick!user@host" mask. Empty
	// if the message had no source. Use Nick/User/Host helpers to decompose.
	Source string

	// Command is the IRC command (e.g. "PRIVMSG", "CAP") or a three-digit
	// numeric reply as a string (e.g. "001"). Commands are conventionally
	// upper-cased; numerics are preserved verbatim.
	Command string

	// Params holds the command parameters in order, fully decoded.
	Params []string
}

// Param returns the parameter at index i, or "" if i is out of range. This is
// the safe accessor handlers should use instead of indexing Params directly.
func (m *Message) Param(i int) string {
	if i < 0 || i >= len(m.Params) {
		return ""
	}
	return m.Params[i]
}

// Nick returns the nickname portion of the Source (the text before '!'), or the
// whole Source if it contains no '!'/'@' (e.g. a server name).
func (m *Message) Nick() string {
	s := m.Source
	for i := 0; i < len(s); i++ {
		if s[i] == '!' || s[i] == '@' {
			return s[:i]
		}
	}
	return s
}

// User returns the user/ident portion of the Source (between '!' and '@'), or ""
// if absent.
func (m *Message) User() string {
	s := m.Source
	bang := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '!' {
			bang = i
		} else if s[i] == '@' {
			if bang >= 0 {
				return s[bang+1 : i]
			}
			return ""
		}
	}
	return ""
}

// Host returns the host portion of the Source (after '@'), or "" if absent.
func (m *Message) Host() string {
	s := m.Source
	for i := 0; i < len(s); i++ {
		if s[i] == '@' {
			return s[i+1:]
		}
	}
	return ""
}

// Tags is an ordered-insensitive map of IRCv3 message tags. Keys are stored
// without the leading '@'. Client-only tag keys keep their '+' prefix.
type Tags map[string]string

// Has reports whether key is present (even with an empty value).
func (t Tags) Has(key string) bool {
	_, ok := t[key]
	return ok
}

// Get returns the value for key (empty string if absent or value-less).
func (t Tags) Get(key string) string {
	return t[key]
}
