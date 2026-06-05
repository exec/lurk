// Package client provides the high-level Lurk IRC client: it wires the
// transport (conn), capability negotiation (cap), SASL authentication (sasl),
// server-feature parsing (isupport), and connection state tracking together
// behind a small event-driven API.
//
// A Client is created with New and a Config, connected with Connect (which runs
// the full IRCv3 registration handshake: CAP LS, NICK/USER, capability
// negotiation, optional SASL, CAP END, and waiting for RPL_WELCOME), and then
// driven by ranging its event loop via Run. Handlers are registered with On for
// raw commands and with the typed Handle* helpers for semantic events.
package client

import "lurk/sasl"

// SASLConfig configures SASL authentication during registration. It is used
// only when Mechanism is non-empty (and the server advertises the sasl
// capability). The zero value disables SASL.
type SASLConfig struct {
	// Mechanism selects the SASL mechanism: "PLAIN" or "EXTERNAL". Empty
	// disables SASL.
	Mechanism string

	// Username is the authentication identity (authcid) for PLAIN.
	Username string

	// Password is the secret for PLAIN.
	Password string

	// Authzid is the optional authorization identity. It is almost always left
	// empty, in which case the server uses the authentication identity.
	Authzid string
}

// enabled reports whether SASL should be attempted given this config.
func (s SASLConfig) enabled() bool {
	return s.Mechanism != ""
}

// mechanism builds the sasl.Mechanism described by the config, or nil if SASL
// is disabled or the mechanism name is unknown.
func (s SASLConfig) mechanism() sasl.Mechanism {
	switch s.Mechanism {
	case "PLAIN":
		return sasl.Plain(s.Authzid, s.Username, s.Password)
	case "EXTERNAL":
		return sasl.External(s.Authzid)
	default:
		return nil
	}
}

// Config describes how a Client connects and registers. Nick is the only truly
// required field; sensible defaults fill in the rest.
type Config struct {
	// Nick is the desired nickname. Required.
	Nick string

	// User is the username (ident) sent in the USER command. Defaults to Nick
	// when empty.
	User string

	// Realname is the "real name" / gecos field of the USER command. Defaults to
	// Nick when empty.
	Realname string

	// Pass, when non-empty, is sent as a PASS command before NICK/USER (a server
	// connection password, distinct from SASL).
	Pass string

	// Server is the "host:port" address to dial. Required by Connect (but a
	// caller using ConnectConn supplies its own transport).
	Server string

	// TLS enables a TLS handshake when dialing.
	TLS bool

	// InsecureSkipVerify disables TLS certificate verification. Use only
	// knowingly (testing, self-signed certs).
	InsecureSkipVerify bool

	// AllowInsecureAuth permits sending credentials (a SASL PLAIN password or a
	// PASS server password) over a plaintext, non-TLS connection. It defaults to
	// false: Connect refuses such a configuration rather than leaking the secret
	// in the clear, since SASL PLAIN is base64-encoded, not encrypted. Set this
	// only when the plaintext link is otherwise trusted (a localhost bouncer, a
	// test). It governs the Connect (dialing) path only; a caller supplying its
	// own transport via ConnectConn is responsible for that transport's security.
	AllowInsecureAuth bool

	// SASL configures SASL authentication. The zero value disables it.
	SASL SASLConfig

	// Caps is the list of capabilities to request if the server offers them. If
	// nil, DefaultCaps is used. An explicit empty slice requests none.
	Caps []string

	// FallbackNick, when set, is appended-to / used to derive an alternate nick
	// if the desired Nick is in use (ERR_NICKNAMEINUSE). When empty, the client
	// appends an underscore to the current nick on each collision.
	FallbackNick string

	// Version is the client identification string returned to a CTCP VERSION
	// query. Defaults to "lurk" when empty.
	Version string

	// Highlights are extra words (besides the nick) that, when they appear in a
	// message, a front-end should treat as a highlight/mention. The client only
	// carries them so the UI can read them back via Highlights().
	Highlights []string

	// AutoReconnect, when true, makes a connection established via Connect
	// transparently re-dial and re-register (with backoff) after an unexpected
	// disconnect, re-joining the channels it was in. A user-initiated Quit/Close
	// stops it. It has no effect on a ConnectConn (injected-transport) session,
	// which has no address to re-dial.
	AutoReconnect bool
}

// DefaultCaps is the set of capabilities Lurk requests when Config.Caps is nil.
// sasl is always added when SASL is configured.
var DefaultCaps = []string{
	"cap-notify",
	"server-time",
	"message-tags",
	"account-notify",
	"extended-join",
	"multi-prefix",
	"away-notify",
	"chghost",
	"account-tag",
	"setname",
	"userhost-in-names",
	"invite-notify",
	"batch",
	"draft/chathistory",
}

// withDefaults returns a copy of the config with empty fields filled in.
func (c Config) withDefaults() Config {
	if c.User == "" {
		c.User = c.Nick
	}
	if c.Realname == "" {
		c.Realname = c.Nick
	}
	if c.Version == "" {
		c.Version = "lurk"
	}
	if c.Caps == nil {
		c.Caps = append([]string(nil), DefaultCaps...)
	}
	// Ensure sasl is requested when SASL is configured, so negotiation enables
	// the capability the auth flow depends on.
	if c.SASL.enabled() && !containsFold(c.Caps, "sasl") {
		c.Caps = append(append([]string(nil), c.Caps...), "sasl")
	}
	return c
}

// containsFold reports whether s is present in list (caps are case-sensitive,
// so this is a plain equality scan).
func containsFold(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
