package client

import (
	"errors"
	"fmt"
	"strings"
)

// Bouncer consumer methods — send-side BOUNCER verbs.
//
// These methods let a lurk client in "control context" (connected to a lurkd
// bouncer without a BOUNCER BIND in effect) issue network-management commands.
// The BOUNCER NETWORK responses arrive as normal events via OnAny / Events();
// the caller observes them by registering a handler for the "BOUNCER" command.
// Example:
//
//	c.On("BOUNCER", func(ev *Event) {
//	    sub := ev.Param(0) // "NETWORK"
//	    netid := ev.Param(1)
//	    attrs := ev.Param(2)
//	    // parse attrs with the bouncer package if needed
//	})
//
// For LISTNETWORKS the response arrives inside a "soju.im/bouncer-networks"
// BATCH; the batch open/close are also delivered as normal events.
//
// These methods are stdlib-only. Attribute escaping uses the same rule as
// IRCv3 message-tag values and the soju.im/bouncer-networks spec:
//
//	\ → \\   ; → \:   space → \s   CR → \r   LF → \n
//
// NOTE on attribute keys: keys are emitted verbatim (not escaped). The
// soju.im/bouncer-networks spec uses a fixed set of well-known key names
// (name, host, port, tls, nick, username, realname, state) that contain no
// special characters. Callers MUST NOT pass keys containing ';' or '=' as
// those characters act as field separators in the wire format and would corrupt
// the attribute string — the server-side parser uses ';' to split fields and
// '=' to split key/value within each field.

// BouncerListNetworks sends "BOUNCER LISTNETWORKS". The server responds with a
// "soju.im/bouncer-networks" BATCH of BOUNCER NETWORK lines (one per configured
// network), followed by a BATCH close. Responses arrive via the normal event
// stream.
func (c *Client) BouncerListNetworks() error {
	return c.write("BOUNCER", "LISTNETWORKS")
}

// BouncerAddNetwork sends "BOUNCER ADDNETWORK <attrs>" to request the bouncer
// to add a new network. attrs must be non-empty and contain at least one valid
// attribute (the soju.im/bouncer-networks server rejects a missing or empty
// attribute list). Standard keys include "name", "host", "port", "tls"
// (value "1" or "0"), "nick", "username", "realname". Values are escaped per
// the IRCv3 tag-value / bouncer-networks encoding before transmission. On
// success the server replies with a "BOUNCER NETWORK <netid> <attrs>" line.
//
// Returns an error immediately (without sending) if attrs is nil or empty,
// since that would produce a server-side FAIL (the protocol requires at least
// one attribute to be meaningful).
func (c *Client) BouncerAddNetwork(attrs map[string]string) error {
	if len(attrs) == 0 {
		return errors.New("client: BouncerAddNetwork: attrs must not be empty (server requires at least one attribute)")
	}
	return c.write("BOUNCER", "ADDNETWORK", encodeBouncerAttrs(attrs))
}

// BouncerChangeNetwork sends "BOUNCER CHANGENETWORK <netid> <attrs>" to modify
// the named network's attributes in place. Only attributes present in attrs are
// changed; absent keys are left unchanged on the server. Values are escaped per
// the bouncer-networks encoding. The server replies with a BOUNCER NETWORK line
// reflecting the updated state.
func (c *Client) BouncerChangeNetwork(netid int, attrs map[string]string) error {
	return c.write("BOUNCER", "CHANGENETWORK", fmt.Sprintf("%d", netid), encodeBouncerAttrs(attrs))
}

// BouncerDelNetwork sends "BOUNCER DELNETWORK <netid>" to remove the named
// network from the bouncer. The server confirms with a BOUNCER NETWORK line
// carrying state=deleted and broadcasts a bouncer-networks-notify to all other
// control sessions.
func (c *Client) BouncerDelNetwork(netid int) error {
	return c.write("BOUNCER", "DELNETWORK", fmt.Sprintf("%d", netid))
}

// encodeBouncerAttrs encodes a key→value map into the semicolon-delimited
// attribute string required by the soju.im/bouncer-networks protocol. The
// encoding matches IRCv3 message-tag value escaping:
//
//	\ → \\   ; → \:   space → \s   CR → \r   LF → \n
//
// Keys are emitted verbatim; they must not contain ';' or '=' (the wire field
// separators). Values are escaped so any byte is safe in a value. If attrs is
// nil or empty, "" is returned.
func encodeBouncerAttrs(attrs map[string]string) string {
	if len(attrs) == 0 {
		return ""
	}
	var sb strings.Builder
	first := true
	for k, v := range attrs {
		if k == "" {
			continue
		}
		if !first {
			sb.WriteByte(';')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(escapeAttrValue(v))
		first = false
	}
	return sb.String()
}

// bouncerAttrEscaper maps the special chars that require escaping in bouncer
// attribute values, matching the IRCv3 message-tag escaping and the
// soju.im/bouncer-networks spec. The replacer applies them in order: backslash
// first to avoid double-escaping.
var bouncerAttrEscaper = strings.NewReplacer(
	`\`, `\\`,
	`;`, `\:`,
	" ", `\s`,
	"\r", `\r`,
	"\n", `\n`,
)

// escapeAttrValue escapes a single attribute value per the soju.im/bouncer-
// networks / IRCv3 tag-value rules. The escaping is intentionally minimal and
// reversible: only the five characters that carry structural meaning (backslash,
// semicolon, space, CR, LF) are escaped; all other bytes pass through unchanged.
func escapeAttrValue(v string) string {
	return bouncerAttrEscaper.Replace(v)
}
