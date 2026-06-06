// Package bouncer implements the soju.im/bouncer-networks control protocol:
// pure, stateless message construction and parsing only. It has no awareness
// of server state, configuration, or upstream connections — those all live in
// the server package which imports bouncer (not the other way around).
//
// Wire format summary (from the soju.im/bouncer-networks spec):
//
//	BOUNCER LISTNETWORKS                     → batch of BOUNCER NETWORK lines
//	BOUNCER ADDNETWORK <attrs>               → BOUNCER NETWORK <netid> <attrs>
//	BOUNCER CHANGENETWORK <netid> <attrs>    → (notify)
//	BOUNCER DELNETWORK <netid>               → (notify)
//	BOUNCER BIND <netid>                     → (during registration)
//
// Attribute encoding (same as IRCv3 message-tag values):
//
//	name=value;name2=value2
//	Special chars in values are escaped: \ → \\; ; → \:; space → \s; CR → \r; LF → \n
//
// This package is standard-library only and must not import server/ or any
// charm library. The TestNoCharmDependency guard enforces this.
package bouncer

import (
	"fmt"
	"strconv"
	"strings"
)

// NetworkInfo carries the attributes of one bouncer network.
// It maps one-to-one with the soju.im/bouncer-networks NETWORK attribute set.
// Secret fields (SASL password) are NEVER populated by server code when
// building a NetworkInfo for client consumption; this type has no password
// field by design.
type NetworkInfo struct {
	NetID    int    // lurkd-allocated integer (soju model)
	Name     string // display name
	Host     string // hostname
	Port     string // port string (e.g. "6697")
	TLS      bool   // whether TLS is used
	Nick     string // registered nick on this network
	Username string // IRC username (USER field)
	Realname string // IRC realname
	State    string // "connected" or "disconnected"
}

// ─── Attribute encoding ───────────────────────────────────────────────────────

// attrEscaper maps special byte values to their escape sequences per the
// soju.im/bouncer-networks spec (same escaping as IRCv3 message-tag values).
// The order matters: backslash must be escaped first to avoid double-escaping.
var attrEscaper = strings.NewReplacer(
	`\`, `\\`,
	`;`, `\:`,
	" ", `\s`,
	"\r", `\r`,
	"\n", `\n`,
)

// EscapeAttrValue escapes a single attribute value for use in the
// semicolon-delimited attribute list. The encoding matches IRCv3 message-tag
// value escaping: \ → \\, ; → \:, space → \s, CR → \r, LF → \n.
func EscapeAttrValue(v string) string {
	return attrEscaper.Replace(v)
}

// attrUnescaper is the inverse of attrEscaper. Unknown escape sequences are
// left unchanged per the spec's "unknown escapes are passed through" rule.
var attrUnescaper = strings.NewReplacer(
	`\\`, `\`,
	`\:`, `;`,
	`\s`, " ",
	`\r`, "\r",
	`\n`, "\n",
)

// UnescapeAttrValue reverses EscapeAttrValue, restoring the original value.
func UnescapeAttrValue(v string) string {
	return attrUnescaper.Replace(v)
}

// EncodeAttrs serialises a slice of (key, value) pairs into the
// semicolon-delimited attribute string used in BOUNCER NETWORK lines.
// Values are escaped per EscapeAttrValue. An empty pairs slice returns "".
// Each pair must have exactly two elements; a pair with an empty key is
// silently omitted.
func EncodeAttrs(pairs [][2]string) string {
	var sb strings.Builder
	first := true
	for _, kv := range pairs {
		key := kv[0]
		if key == "" {
			continue
		}
		if !first {
			sb.WriteByte(';')
		}
		sb.WriteString(key)
		sb.WriteByte('=')
		sb.WriteString(EscapeAttrValue(kv[1]))
		first = false
	}
	return sb.String()
}

// ParseAttrs parses a semicolon-delimited attribute string into a map of
// unescaped key → value pairs. It is tolerant: empty fields, duplicate keys
// (last wins), and keys without a value ("key" with no "=") are handled.
// Returns an empty (non-nil) map for an empty input.
func ParseAttrs(s string) map[string]string {
	m := make(map[string]string)
	if s == "" {
		return m
	}
	for _, field := range strings.Split(s, ";") {
		if field == "" {
			continue
		}
		k, v, _ := strings.Cut(field, "=")
		if k == "" {
			continue
		}
		m[k] = UnescapeAttrValue(v)
	}
	return m
}

// ─── NetworkInfo ↔ attrs ──────────────────────────────────────────────────────

// NetworkInfoToAttrs serialises a NetworkInfo into a (key,value) pairs slice
// suitable for EncodeAttrs. Only non-empty/non-zero fields are emitted.
// The password is NEVER included (the SASL password lives server-side only).
func NetworkInfoToAttrs(n NetworkInfo) [][2]string {
	var pairs [][2]string
	add := func(k, v string) {
		if v != "" {
			pairs = append(pairs, [2]string{k, v})
		}
	}
	add("name", n.Name)
	add("host", n.Host)
	add("port", n.Port)
	if n.TLS {
		pairs = append(pairs, [2]string{"tls", "1"})
	}
	add("nick", n.Nick)
	add("username", n.Username)
	add("realname", n.Realname)
	add("state", n.State)
	return pairs
}

// NetworkInfoFromAttrs populates a NetworkInfo from a parsed attribute map.
// It does NOT set NetID (the caller must do that from context).
func NetworkInfoFromAttrs(m map[string]string) NetworkInfo {
	var n NetworkInfo
	n.Name = m["name"]
	n.Host = m["host"]
	n.Port = m["port"]
	n.TLS = m["tls"] == "1" || m["tls"] == "true"
	n.Nick = m["nick"]
	n.Username = m["username"]
	n.Realname = m["realname"]
	n.State = m["state"]
	return n
}

// ─── BOUNCER command parsing ──────────────────────────────────────────────────

// Subcommand names as constants for robust dispatch.
const (
	SubBind          = "BIND"
	SubListNetworks  = "LISTNETWORKS"
	SubAddNetwork    = "ADDNETWORK"
	SubChangeNetwork = "CHANGENETWORK"
	SubDelNetwork    = "DELNETWORK"
)

// Cmd is a parsed BOUNCER command.
type Cmd struct {
	Sub   string            // upper-cased subcommand (BIND, LISTNETWORKS, …)
	NetID int               // parsed netid (for BIND, CHANGENETWORK, DELNETWORK)
	Attrs map[string]string // parsed attribute map (for ADDNETWORK, CHANGENETWORK)
	Raw   string            // the raw subcommand word (for error messages)
}

// ParseCmd parses a BOUNCER message. msg should be a message with
// Command=="BOUNCER". Returns a Cmd and nil on success; returns a
// descriptive error (safe to include in a FAIL message) on any parse
// failure. Never panics on hostile input.
//
// Parameter layout (irc.Message.Params):
//
//	[0] = subcommand
//	[1] = netid (for BIND/CHANGENETWORK/DELNETWORK) or attrs (for ADDNETWORK)
//	[2] = attrs (for CHANGENETWORK)
func ParseCmd(params []string) (Cmd, error) {
	if len(params) == 0 || params[0] == "" {
		return Cmd{}, fmt.Errorf("BOUNCER requires a subcommand")
	}
	sub := strings.ToUpper(params[0])

	switch sub {
	case SubBind:
		// BOUNCER BIND <netid>
		if len(params) < 2 || params[1] == "" {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER BIND requires a netid")
		}
		id, err := parseNetID(params[1])
		if err != nil {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER BIND: %w", err)
		}
		return Cmd{Sub: sub, NetID: id, Raw: params[0]}, nil

	case SubListNetworks:
		return Cmd{Sub: sub, Raw: params[0]}, nil

	case SubAddNetwork:
		// BOUNCER ADDNETWORK <attrs>
		if len(params) < 2 || params[1] == "" {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER ADDNETWORK requires attribute list")
		}
		attrs := ParseAttrs(params[1])
		return Cmd{Sub: sub, Attrs: attrs, Raw: params[0]}, nil

	case SubChangeNetwork:
		// BOUNCER CHANGENETWORK <netid> <attrs>
		if len(params) < 2 || params[1] == "" {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER CHANGENETWORK requires netid")
		}
		id, err := parseNetID(params[1])
		if err != nil {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER CHANGENETWORK: %w", err)
		}
		var attrs map[string]string
		if len(params) >= 3 {
			attrs = ParseAttrs(params[2])
		} else {
			attrs = make(map[string]string)
		}
		return Cmd{Sub: sub, NetID: id, Attrs: attrs, Raw: params[0]}, nil

	case SubDelNetwork:
		// BOUNCER DELNETWORK <netid>
		if len(params) < 2 || params[1] == "" {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER DELNETWORK requires a netid")
		}
		id, err := parseNetID(params[1])
		if err != nil {
			return Cmd{Sub: sub}, fmt.Errorf("BOUNCER DELNETWORK: %w", err)
		}
		return Cmd{Sub: sub, NetID: id, Raw: params[0]}, nil

	default:
		return Cmd{Sub: sub, Raw: params[0]},
			fmt.Errorf("unknown BOUNCER subcommand %q", sub)
	}
}

// parseNetID parses a netid string into a positive integer. Returns an error
// if the string is not a valid positive integer or overflows int.
func parseNetID(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty netid")
	}
	// Guard against leading '-' (negative numbers) before Atoi.
	if s[0] == '-' {
		return 0, fmt.Errorf("netid must be positive, got %q", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid netid %q (must be a positive integer)", s)
	}
	return n, nil
}

// ─── Message builders ─────────────────────────────────────────────────────────

// ListNetworksBatchOpen returns the BATCH + open line for a LISTNETWORKS reply.
// batchRef is the random reference token the caller generates.
func ListNetworksBatchOpen(serverName, batchRef string) *NetworkMsg {
	return &NetworkMsg{
		ServerName: serverName,
		Command:    "BATCH",
		Params:     []string{"+" + batchRef, "soju.im/bouncer-networks"},
	}
}

// ListNetworksBatchClose returns the BATCH - close line.
func ListNetworksBatchClose(serverName, batchRef string) *NetworkMsg {
	return &NetworkMsg{
		ServerName: serverName,
		Command:    "BATCH",
		Params:     []string{"-" + batchRef},
	}
}

// ListNetworksNetworkLine returns one BOUNCER NETWORK line inside a LISTNETWORKS
// batch. The batch tag is set to batchRef.
func ListNetworksNetworkLine(serverName, batchRef string, n NetworkInfo) *NetworkMsg {
	attrs := EncodeAttrs(NetworkInfoToAttrs(n))
	return &NetworkMsg{
		ServerName: serverName,
		BatchRef:   batchRef,
		Command:    "BOUNCER",
		Params:     []string{"NETWORK", strconv.Itoa(n.NetID), attrs},
	}
}

// NotifyNetworkLine returns a bouncer-networks-notify NETWORK message for
// broadcast to other control sessions. op is "add", "change", or "delete".
func NotifyNetworkLine(serverName string, n NetworkInfo) *NetworkMsg {
	attrs := EncodeAttrs(NetworkInfoToAttrs(n))
	return &NetworkMsg{
		ServerName: serverName,
		Command:    "BOUNCER",
		Params:     []string{"NETWORK", strconv.Itoa(n.NetID), attrs},
	}
}

// NetworkMsg is an intermediate message representation that the server package
// turns into an irc.Message. It holds only what bouncer/ needs to express;
// the server layer converts it to the concrete irc.Message type to avoid an
// import of the irc package from within bouncer (which would pull in irc as a
// dependency of bouncer — acceptable, but separating the concern is cleaner).
type NetworkMsg struct {
	ServerName string   // :source
	BatchRef   string   // if non-empty, added as @batch=<ref> tag
	Command    string   // BATCH or BOUNCER
	Params     []string // verbatim params
}
