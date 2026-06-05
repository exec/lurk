// Package isupport parses RPL_ISUPPORT (numeric 005) feature tokens and exposes
// them through typed accessors, including the CASEMAPPING-driven case folding
// that every nick and channel comparison in the client must go through.
//
// A server advertises its capabilities across one or more 005 lines. Each line
// carries a list of tokens of the form KEY, KEY=value, or -KEY (a negation that
// removes a previously-advertised KEY). Tokens accumulate: later lines extend or
// override earlier ones. Build an ISupport from the first batch with Parse, then
// fold in subsequent batches with Merge:
//
//	feat := isupport.Parse(tokens1)
//	feat = feat.Merge(tokens2)
//
// ISupport is a small immutable-by-convention value wrapping a map; Parse and
// Merge return fresh copies and never mutate their receiver, so a previously
// captured ISupport stays valid.
package isupport

import (
	"strconv"
	"strings"

	"github.com/exec/lurk/irc"
)

// ISupport holds the accumulated RPL_ISUPPORT tokens advertised by a server.
//
// The zero value is usable and behaves as an empty feature set (every typed
// accessor returns its documented default). Values are stored already-unescaped
// per the ISUPPORT value-escaping rules (see decodeValue).
type ISupport struct {
	// tokens maps an upper-cased KEY to its decoded value. A bare token (KEY
	// with no '=') is stored with the empty string as its value; use the
	// (value, ok) form of Get to distinguish "present and empty" from "absent".
	tokens map[string]string
}

// TokensFromMessage extracts the ISUPPORT tokens from a parsed 005 message.
//
// A 005 reply has params [nick, TOKEN, TOKEN, ..., :are supported by this
// server]: the first param is the recipient's nick and the last is a
// human-readable trailer. This drops both and returns the token slice suitable
// for Parse or Merge. It returns nil if the message does not carry any tokens.
func TokensFromMessage(m *irc.Message) []string {
	// Need at least nick + one token + trailer to have a real token.
	if m == nil || len(m.Params) < 3 {
		return nil
	}
	return m.Params[1 : len(m.Params)-1]
}

// Parse builds an ISupport from a single batch of 005 tokens. Each token is
// KEY, KEY=value, or -KEY; see Merge for how each form is applied. Parse is
// equivalent to calling Merge on an empty ISupport.
func Parse(tokens []string) ISupport {
	return ISupport{}.Merge(tokens)
}

// Merge folds a later batch of 005 tokens onto the receiver and returns the
// result. The receiver is not modified.
//
//   - KEY=value sets KEY to the decoded value.
//   - KEY (bare) sets KEY to the empty string (advertised as a flag).
//   - -KEY removes KEY (a negation); -KEY=value is also treated as a removal.
//
// Keys are upper-cased so lookups are case-insensitive.
func (s ISupport) Merge(tokens []string) ISupport {
	out := make(map[string]string, len(s.tokens)+len(tokens))
	for k, v := range s.tokens {
		out[k] = v
	}
	for _, tok := range tokens {
		if tok == "" {
			continue
		}
		if strings.HasPrefix(tok, "-") {
			key := tok[1:]
			if eq := strings.IndexByte(key, '='); eq >= 0 {
				key = key[:eq]
			}
			delete(out, strings.ToUpper(key))
			continue
		}
		key, val, hasVal := strings.Cut(tok, "=")
		key = strings.ToUpper(key)
		if hasVal {
			out[key] = decodeValue(val)
		} else {
			out[key] = ""
		}
	}
	return ISupport{tokens: out}
}

// decodeValue applies ISUPPORT value escaping: a literal backslash-x followed by
// two hex digits ("\xHH") denotes the byte with that hex value. This lets values
// carry characters such as space ("\x20") or backslash ("\x5C") that would
// otherwise be impossible to express on the wire. Malformed escapes are left
// verbatim.
func decodeValue(v string) string {
	if !strings.Contains(v, "\\x") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	for i := 0; i < len(v); {
		// A valid escape is "\xHH": backslash, x, and two hex digits.
		if v[i] == '\\' && i+3 < len(v) && (v[i+1] == 'x' || v[i+1] == 'X') {
			hi, ok1 := hexNibble(v[i+2])
			lo, ok2 := hexNibble(v[i+3])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 4
				continue
			}
		}
		b.WriteByte(v[i])
		i++
	}
	return b.String()
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Get returns the raw value advertised for key (case-insensitive) and whether
// the key is present. A bare flag token reports ("", true).
func (s ISupport) Get(key string) (string, bool) {
	if s.tokens == nil {
		return "", false
	}
	v, ok := s.tokens[strings.ToUpper(key)]
	return v, ok
}

// Network returns the network display name (NETWORK token), or "" if unset.
func (s ISupport) Network() string {
	v, _ := s.Get("NETWORK")
	return v
}

// ChanTypes returns the valid channel-name prefix characters (CHANTYPES token).
// It defaults to "#&" when the server does not advertise the token, matching
// historical IRC behaviour.
func (s ISupport) ChanTypes() string {
	if v, ok := s.Get("CHANTYPES"); ok {
		return v
	}
	return "#&"
}

// StatusMsg returns the prefix characters that may target a channel's
// privileged members in a status-message PRIVMSG (STATUSMSG token, e.g. "@+"),
// or "" if the server does not support status messages.
func (s ISupport) StatusMsg() string {
	v, _ := s.Get("STATUSMSG")
	return v
}

// PrefixModes returns the membership mode letters from PREFIX=(modes)symbols,
// highest privilege first (e.g. "ov" for PREFIX=(ov)@+). PrefixModes()[i]
// corresponds to PrefixSymbols()[i].
//
// It defaults to "ov" when PREFIX is unadvertised or malformed.
func (s ISupport) PrefixModes() string {
	modes, _ := s.prefix()
	return modes
}

// PrefixSymbols returns the display symbols from PREFIX=(modes)symbols, highest
// privilege first (e.g. "@+" for PREFIX=(ov)@+). It defaults to "@+".
func (s ISupport) PrefixSymbols() string {
	_, symbols := s.prefix()
	return symbols
}

// prefix splits the PREFIX token "(modes)symbols" into its two aligned halves.
// On absence or malformed input it returns the conventional defaults so callers
// always get a usable, length-matched pair.
func (s ISupport) prefix() (modes, symbols string) {
	v, ok := s.Get("PREFIX")
	if !ok || len(v) < 2 || v[0] != '(' {
		return "ov", "@+"
	}
	close := strings.IndexByte(v, ')')
	if close < 0 {
		return "ov", "@+"
	}
	modes = v[1:close]
	symbols = v[close+1:]
	// The two halves must align one-to-one; if they don't, fall back to the
	// shorter length so PrefixModes()[i]/PrefixSymbols()[i] stay valid.
	if len(modes) != len(symbols) {
		n := min(len(modes), len(symbols))
		modes, symbols = modes[:n], symbols[:n]
	}
	return modes, symbols
}

// PrefixSymbolForMode returns the display symbol for a membership mode letter
// (e.g. 'o' -> '@'), and whether the mode is a recognised prefix mode.
func (s ISupport) PrefixSymbolForMode(mode byte) (byte, bool) {
	modes, symbols := s.prefix()
	if i := strings.IndexByte(modes, mode); i >= 0 {
		return symbols[i], true
	}
	return 0, false
}

// PrefixModeForSymbol returns the membership mode letter for a display symbol
// (e.g. '@' -> 'o'), and whether the symbol is a recognised prefix symbol. This
// is the inverse used when parsing prefixed nicks from a NAMES reply.
func (s ISupport) PrefixModeForSymbol(symbol byte) (byte, bool) {
	modes, symbols := s.prefix()
	if i := strings.IndexByte(symbols, symbol); i >= 0 {
		return modes[i], true
	}
	return 0, false
}

// ChanModes holds the four CHANMODES groups, which determine whether a channel
// mode takes a parameter:
//
//   - A: list modes; always take a parameter (e.g. ban list +b).
//   - B: always take a parameter (e.g. +k key).
//   - C: take a parameter only when set (e.g. +l limit).
//   - D: never take a parameter (e.g. +n).
//
// Each field is the concatenated mode letters for that group.
type ChanModes struct {
	A, B, C, D string
}

// ChanModes returns the parsed CHANMODES groups. Groups beyond the fourth (some
// servers advertise extensions) are ignored. Missing groups are empty.
func (s ISupport) ChanModes() ChanModes {
	v, ok := s.Get("CHANMODES")
	if !ok {
		return ChanModes{}
	}
	parts := strings.Split(v, ",")
	var cm ChanModes
	dst := []*string{&cm.A, &cm.B, &cm.C, &cm.D}
	for i := 0; i < len(parts) && i < len(dst); i++ {
		*dst[i] = parts[i]
	}
	return cm
}

// intToken parses a token whose value is a non-negative integer, returning the
// value and whether it was present and well-formed.
func (s ISupport) intToken(key string) (int, bool) {
	v, ok := s.Get(key)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// NickLen returns the maximum nickname length (NICKLEN), and whether it was
// advertised.
func (s ISupport) NickLen() (int, bool) { return s.intToken("NICKLEN") }

// ChannelLen returns the maximum channel-name length (CHANNELLEN), and whether
// it was advertised.
func (s ISupport) ChannelLen() (int, bool) { return s.intToken("CHANNELLEN") }

// TopicLen returns the maximum topic length (TOPICLEN), and whether it was
// advertised.
func (s ISupport) TopicLen() (int, bool) { return s.intToken("TOPICLEN") }

// TargMax returns the maximum number of targets allowed for the named command
// (from the TARGMAX token, e.g. "PRIVMSG:4,NOTICE:4"), and whether a limit is
// defined for it. A command listed with an empty value (no numeric limit, i.e.
// unlimited) reports ok=false, matching the "no limit" semantics.
//
// The empty-value case is not hypothetical: Ergo emits entries such as "KICK:"
// to mean "no per-command target limit" (see reference/ergo/irc/config.go's
// generateISupport), so callers must treat ok=false as "unlimited", distinct
// from "command absent" which also reports ok=false.
//
// The command name is matched case-insensitively.
func (s ISupport) TargMax(command string) (int, bool) {
	v, ok := s.Get("TARGMAX")
	if !ok {
		return 0, false
	}
	command = strings.ToUpper(command)
	for _, entry := range strings.Split(v, ",") {
		name, limit, hasLimit := strings.Cut(entry, ":")
		if strings.ToUpper(name) != command {
			continue
		}
		if !hasLimit || limit == "" {
			return 0, false
		}
		n, err := strconv.Atoi(limit)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	return 0, false
}
