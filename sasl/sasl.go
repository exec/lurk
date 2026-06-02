// Package sasl implements the client side of SASL authentication over IRC, as
// described by the IRCv3 sasl-3.1 and sasl-3.2 specifications.
//
// SASL runs inside capability negotiation: once the server has acknowledged the
// "sasl" capability (CAP ACK :sasl) and before CAP END, the client selects a
// mechanism, exchanges base64-encoded AUTHENTICATE payloads with the server,
// and interprets the 900-908 result numerics. This package provides:
//
//   - The Mechanism interface plus the PLAIN and EXTERNAL mechanisms.
//   - A Conversation driver that turns the abstract mechanism exchange into the
//     concrete AUTHENTICATE lines to send (including ≤400-byte chunking) and
//     consumes the server's *irc.Message replies, reporting success or failure.
//
// The package performs no I/O: a Conversation emits the lines to send as
// strings and is fed the server's messages by the caller (the client run-loop).
package sasl

import "fmt"

// Mechanism is a SASL authentication mechanism. A Mechanism is a value
// configured by the caller (via Plain or External) and driven by a
// Conversation.
//
// The exchange is: Start may provide an initial client response; thereafter the
// server may send challenges, each answered by Next. For the mechanisms in this
// package the exchange completes after the initial response, but the interface
// supports multi-step (challenge/response) mechanisms.
type Mechanism interface {
	// Name returns the mechanism name as it appears on the wire after
	// "AUTHENTICATE ", e.g. "PLAIN" or "EXTERNAL". It must be a valid mechanism
	// token (uppercase ASCII, no spaces).
	Name() string

	// Start returns the initial client response, if the mechanism sends one
	// before any server challenge. hasInitial reports whether response is
	// meaningful: when false, the client waits for the first server challenge
	// before calling Next. A response of nil (or empty) with hasInitial true is
	// an explicit empty initial response, encoded on the wire as "AUTHENTICATE +".
	Start() (response []byte, hasInitial bool)

	// Next answers a server challenge (the decoded payload of a server
	// "AUTHENTICATE" line; an empty challenge corresponds to "AUTHENTICATE +").
	// It returns the client's response, or an error to abort the exchange.
	Next(challenge []byte) (response []byte, err error)
}

// plainMechanism implements SASL PLAIN (RFC 4616): the response is
// authzid \0 authcid \0 passwd, sent as the initial response.
type plainMechanism struct {
	authzid string
	authcid string
	passwd  string
}

// Plain returns a SASL PLAIN mechanism. authzid is the authorization identity
// and is usually empty (the server then authorizes as authcid). authcid is the
// account name to log in as and passwd is its password.
func Plain(authzid, authcid, passwd string) Mechanism {
	return &plainMechanism{authzid: authzid, authcid: authcid, passwd: passwd}
}

func (p *plainMechanism) Name() string { return "PLAIN" }

func (p *plainMechanism) Start() ([]byte, bool) {
	// authzid \0 authcid \0 passwd, raw bytes (base64 encoding is the
	// Conversation/chunker's job, not the mechanism's).
	buf := make([]byte, 0, len(p.authzid)+len(p.authcid)+len(p.passwd)+2)
	buf = append(buf, p.authzid...)
	buf = append(buf, 0)
	buf = append(buf, p.authcid...)
	buf = append(buf, 0)
	buf = append(buf, p.passwd...)
	return buf, true
}

func (p *plainMechanism) Next(challenge []byte) ([]byte, error) {
	// PLAIN completes with the initial response; any further server challenge is
	// unexpected.
	return nil, fmt.Errorf("sasl: PLAIN received an unexpected server challenge")
}

// externalMechanism implements SASL EXTERNAL: the identity is taken from the
// already-established TLS client certificate, so the response is empty (an
// optional authzid may request authorization as a different identity).
type externalMechanism struct {
	authzid string
}

// External returns a SASL EXTERNAL mechanism. The identity is derived from the
// TLS client certificate presented on the connection. authzid is usually empty;
// if set it requests authorization as that identity. The initial response is the
// authzid (empty for the common case), sent on the wire as "AUTHENTICATE +" when
// empty.
func External(authzid string) Mechanism {
	return &externalMechanism{authzid: authzid}
}

func (e *externalMechanism) Name() string { return "EXTERNAL" }

func (e *externalMechanism) Start() ([]byte, bool) {
	return []byte(e.authzid), true
}

func (e *externalMechanism) Next(challenge []byte) ([]byte, error) {
	return nil, fmt.Errorf("sasl: EXTERNAL received an unexpected server challenge")
}

// SelectMechanism reports whether the configured mechanism is usable given the
// list of mechanisms the server advertised (e.g. via the sasl-3.2
// "sasl=PLAIN,EXTERNAL" capability value, as parsed by package cap).
//
// When available is empty the server advertised SASL without a mechanism list
// (sasl-3.1 behavior); any mechanism is permitted and m is returned unchanged.
// When available is non-empty, m is returned only if its Name appears in the
// list (case-sensitively, per the spec); otherwise an error is returned naming
// the advertised mechanisms.
func SelectMechanism(available []string, m Mechanism) (Mechanism, error) {
	if len(available) == 0 {
		return m, nil
	}
	name := m.Name()
	for _, a := range available {
		if a == name {
			return m, nil
		}
	}
	return nil, fmt.Errorf("sasl: mechanism %q not offered by server (advertised: %v)", name, available)
}
