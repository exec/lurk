// Package server — authcid.go: hardened authcid fallback parser [B#3].
//
// The universal SASL username convention for IRC bouncers is:
//
//	user/network@client
//
// where "user" is the bouncer username, "network" is the upstream network name,
// and "client" is an opaque client identifier (used for per-client cursors).
// Any or all of the optional components may be absent:
//
//   - bare "user" — no network, no client (control context, no BOUNCER BIND)
//   - "user/network" — no client suffix
//   - "user/network@client" — full form
//
// This surface is hostile-input: the authcid arrives from an unauthenticated
// client over a network connection. The parser must not panic, must not allocate
// unboundedly, and must reject any authcid containing NUL bytes (which are the
// SASL field separator and must not appear inside field values).
package server

import (
	"errors"
	"strings"
)

// maxAuthcidLen is the maximum byte length of an authcid string this parser
// accepts. An authcid longer than this is rejected (errAuthcidTooLong) before
// any splitting. 512 bytes is generous for any real username/network/client
// combination and well below the 512-byte IRC line budget.
const maxAuthcidLen = 512

// Sentinel errors returned by ParseAuthcid. Callers match on these with
// errors.Is so exact wording may change.
var (
	// errAuthcidNUL is returned when the authcid contains a NUL byte (the SASL
	// field separator). This is never a valid identifier component.
	errAuthcidNUL = errors.New("server: authcid contains NUL byte")

	// errAuthcidTooLong is returned when the authcid exceeds maxAuthcidLen bytes.
	errAuthcidTooLong = errors.New("server: authcid exceeds maximum length")

	// errAuthcidEmpty is returned when the authcid is empty after stripping, or
	// when the user component (the mandatory first field) is empty.
	errAuthcidEmpty = errors.New("server: authcid user component is empty")
)

// ParsedAuthcid holds the components extracted from a SASL authcid by
// ParseAuthcid. Unset optional components are empty strings.
type ParsedAuthcid struct {
	// User is the bouncer username — everything before the first '/'.
	// It is always non-empty for a valid result.
	User string

	// Network is the upstream network selector — between '/' and '@'.
	// Empty when no '/' was present in the authcid.
	Network string

	// Client is the per-client cursor identifier — everything after '@'.
	// Empty when no '@' was present.
	Client string

	// HasNetwork reports whether the authcid included a '/' (network selector).
	// A bare trailing '/' (e.g. "user/") sets HasNetwork with an empty Network;
	// resolving an empty/unknown network to a netid is a Phase 6 concern, so it
	// is not rejected here.
	HasNetwork bool

	// HasClient reports whether the authcid included the '@client' component.
	HasClient bool
}

// ParseAuthcid parses the SASL authcid string and extracts the (user, network,
// client) triple from the universal user/network@client convention. It is
// hardened against hostile input per §6.3 of docs/LURKD-DESIGN.md:
//
//   - Empty authcid → errAuthcidEmpty
//   - Length > maxAuthcidLen → errAuthcidTooLong
//   - Any NUL byte → errAuthcidNUL (NUL is the SASL field separator)
//   - Empty user component (leading '/' or empty string) → errAuthcidEmpty
//   - Multiple '/' → only the first is the separator; the rest join the network
//     component (deterministic, documented behavior, not a panic)
//   - Multiple '@' → only the LAST '@' is the client separator; everything
//     before it is the user(/network) part (matches soju convention; documented)
//
// ParseAuthcid never panics. Unknown network names are not rejected here —
// network-to-netid resolution is a Phase 6 concern.
func ParseAuthcid(authcid string) (ParsedAuthcid, error) {
	if authcid == "" {
		return ParsedAuthcid{}, errAuthcidEmpty
	}
	if len(authcid) > maxAuthcidLen {
		return ParsedAuthcid{}, errAuthcidTooLong
	}
	if strings.ContainsRune(authcid, 0) {
		return ParsedAuthcid{}, errAuthcidNUL
	}

	var p ParsedAuthcid

	// Split on '@': the LAST '@' separates client. Everything before it is
	// the user[/network] part.
	atIdx := strings.LastIndex(authcid, "@")
	userNet := authcid
	if atIdx >= 0 {
		p.HasClient = true
		p.Client = authcid[atIdx+1:]
		userNet = authcid[:atIdx]
	}

	// Split on '/': the FIRST '/' separates user from network. Any additional
	// '/' characters are treated as part of the network name.
	slashIdx := strings.Index(userNet, "/")
	if slashIdx >= 0 {
		p.HasNetwork = true
		p.User = userNet[:slashIdx]
		p.Network = userNet[slashIdx+1:]
	} else {
		p.User = userNet
	}

	if p.User == "" {
		return ParsedAuthcid{}, errAuthcidEmpty
	}

	return p, nil
}
