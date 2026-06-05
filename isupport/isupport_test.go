package isupport

import (
	"testing"

	"github.com/exec/lurk/irc"
)

// Real Libera.Chat 005 token lines, split as the server actually sends them.
// Captured from an `005` burst (nick + trailer stripped, i.e. exactly what
// TokensFromMessage yields).
var liberaBatches = [][]string{
	{"CALLERID", "CASEMAPPING=rfc1459", "DEAF=D", "KICKLEN=490", "MODES=4", "MONITOR=100", "PREFIX=(ov)@+", "CHANTYPES=#", "CHANLIMIT=#:250"},
	{"CHANNELLEN=50", "CHANMODES=eIbq,k,flj,CFLMPQScgimnprstuz", "EXCEPTS", "EXTBAN=$,ajrxz", "ELIST=CMNTU", "INVEX", "KNOCK", "NETWORK=Libera.Chat", "NICKLEN=16"},
	{"SAFELIST", "STATUSMSG=@+", "TARGMAX=ACCEPT:,KICK:1,LIST:1,NAMES:1,NOTICE:4,PRIVMSG:4,WHOIS:1", "TOPICLEN=390", "UTF8ONLY", "WHOX", "AWAYLEN=200"},
}

func parseLibera() ISupport {
	feat := Parse(liberaBatches[0])
	for _, batch := range liberaBatches[1:] {
		feat = feat.Merge(batch)
	}
	return feat
}

func TestParseLiberaAccessors(t *testing.T) {
	feat := parseLibera()

	if got := feat.Network(); got != "Libera.Chat" {
		t.Errorf("Network() = %q, want %q", got, "Libera.Chat")
	}
	if got := feat.ChanTypes(); got != "#" {
		t.Errorf("ChanTypes() = %q, want %q", got, "#")
	}
	if got := feat.StatusMsg(); got != "@+" {
		t.Errorf("StatusMsg() = %q, want %q", got, "@+")
	}
	if got := feat.PrefixModes(); got != "ov" {
		t.Errorf("PrefixModes() = %q, want %q", got, "ov")
	}
	if got := feat.PrefixSymbols(); got != "@+" {
		t.Errorf("PrefixSymbols() = %q, want %q", got, "@+")
	}

	cm := feat.ChanModes()
	if cm.A != "eIbq" || cm.B != "k" || cm.C != "flj" || cm.D != "CFLMPQScgimnprstuz" {
		t.Errorf("ChanModes() = %+v, want A=eIbq B=k C=flj D=...", cm)
	}

	for _, tc := range []struct {
		name string
		get  func() (int, bool)
		want int
	}{
		{"NickLen", feat.NickLen, 16},
		{"ChannelLen", feat.ChannelLen, 50},
		{"TopicLen", feat.TopicLen, 390},
	} {
		if n, ok := tc.get(); !ok || n != tc.want {
			t.Errorf("%s() = (%d, %v), want (%d, true)", tc.name, n, ok, tc.want)
		}
	}

	if cm := feat.CaseMapping(); cm != CaseRFC1459 {
		t.Errorf("CaseMapping() = %v, want CaseRFC1459", cm)
	}
}

func TestPrefixMapping(t *testing.T) {
	feat := Parse([]string{"PREFIX=(qaohv)~&@%+"})
	if got := feat.PrefixModes(); got != "qaohv" {
		t.Fatalf("PrefixModes() = %q", got)
	}
	if got := feat.PrefixSymbols(); got != "~&@%+" {
		t.Fatalf("PrefixSymbols() = %q", got)
	}
	if sym, ok := feat.PrefixSymbolForMode('o'); !ok || sym != '@' {
		t.Errorf("PrefixSymbolForMode('o') = (%q, %v)", sym, ok)
	}
	if mode, ok := feat.PrefixModeForSymbol('~'); !ok || mode != 'q' {
		t.Errorf("PrefixModeForSymbol('~') = (%q, %v)", mode, ok)
	}
	if _, ok := feat.PrefixModeForSymbol('!'); ok {
		t.Errorf("PrefixModeForSymbol('!') should be absent")
	}
}

func TestPrefixDefaults(t *testing.T) {
	var feat ISupport // zero value
	if feat.PrefixModes() != "ov" || feat.PrefixSymbols() != "@+" {
		t.Errorf("zero-value prefix = (%q,%q), want (ov,@+)", feat.PrefixModes(), feat.PrefixSymbols())
	}
	// Malformed PREFIX falls back to defaults.
	bad := Parse([]string{"PREFIX=ov@+"}) // missing parens
	if bad.PrefixModes() != "ov" || bad.PrefixSymbols() != "@+" {
		t.Errorf("malformed prefix = (%q,%q), want defaults", bad.PrefixModes(), bad.PrefixSymbols())
	}
	// Mismatched lengths get truncated to the aligned pair.
	mis := Parse([]string{"PREFIX=(ovh)@+"})
	if mis.PrefixModes() != "ov" || mis.PrefixSymbols() != "@+" {
		t.Errorf("mismatched prefix = (%q,%q), want (ov,@+)", mis.PrefixModes(), mis.PrefixSymbols())
	}
}

func TestChanTypesDefault(t *testing.T) {
	var feat ISupport
	if got := feat.ChanTypes(); got != "#&" {
		t.Errorf("default ChanTypes() = %q, want #&", got)
	}
}

func TestBareTokenAndGet(t *testing.T) {
	feat := Parse([]string{"WHOX", "UTF8ONLY", "NETWORK=Foo"})
	if v, ok := feat.Get("WHOX"); !ok || v != "" {
		t.Errorf("Get(WHOX) = (%q, %v), want (\"\", true)", v, ok)
	}
	// Case-insensitive key lookup.
	if v, ok := feat.Get("network"); !ok || v != "Foo" {
		t.Errorf("Get(network) = (%q, %v), want (Foo, true)", v, ok)
	}
	if _, ok := feat.Get("MISSING"); ok {
		t.Errorf("Get(MISSING) should be absent")
	}
}

func TestNegation(t *testing.T) {
	feat := Parse([]string{"EXCEPTS", "INVEX", "KNOCK"})
	feat = feat.Merge([]string{"-INVEX", "-KNOCK=ignored"})
	if _, ok := feat.Get("INVEX"); ok {
		t.Errorf("INVEX should have been negated")
	}
	if _, ok := feat.Get("KNOCK"); ok {
		t.Errorf("KNOCK should have been negated (-KEY=value form)")
	}
	if _, ok := feat.Get("EXCEPTS"); !ok {
		t.Errorf("EXCEPTS should survive negation of others")
	}
}

func TestMergeOverride(t *testing.T) {
	feat := Parse([]string{"NICKLEN=9"})
	feat = feat.Merge([]string{"NICKLEN=30"})
	if n, ok := feat.NickLen(); !ok || n != 30 {
		t.Errorf("NickLen after override = (%d,%v), want 30", n, ok)
	}
}

func TestMergeDoesNotMutateReceiver(t *testing.T) {
	base := Parse([]string{"NICKLEN=9"})
	_ = base.Merge([]string{"NICKLEN=30", "-NICKLEN"})
	// base must be unchanged by the Merge above.
	if n, ok := base.NickLen(); !ok || n != 9 {
		t.Errorf("base mutated by Merge: NickLen = (%d,%v), want 9", n, ok)
	}
}

func TestTargMax(t *testing.T) {
	feat := Parse([]string{"TARGMAX=ACCEPT:,KICK:1,NOTICE:4,PRIVMSG:4"})
	if n, ok := feat.TargMax("PRIVMSG"); !ok || n != 4 {
		t.Errorf("TargMax(PRIVMSG) = (%d,%v), want (4,true)", n, ok)
	}
	if n, ok := feat.TargMax("kick"); !ok || n != 1 {
		t.Errorf("TargMax(kick) = (%d,%v), want (1,true)", n, ok)
	}
	// ACCEPT has no numeric limit (unlimited) -> ok=false.
	if _, ok := feat.TargMax("ACCEPT"); ok {
		t.Errorf("TargMax(ACCEPT) should report no limit")
	}
	if _, ok := feat.TargMax("WHO"); ok {
		t.Errorf("TargMax(WHO) absent should report ok=false")
	}
	// No TARGMAX token at all.
	var empty ISupport
	if _, ok := empty.TargMax("PRIVMSG"); ok {
		t.Errorf("TargMax with no token should be ok=false")
	}
}

func TestIntTokenMalformed(t *testing.T) {
	feat := Parse([]string{"NICKLEN=abc", "TOPICLEN=-5"})
	if _, ok := feat.NickLen(); ok {
		t.Errorf("NickLen(abc) should be ok=false")
	}
	if _, ok := feat.TopicLen(); ok {
		t.Errorf("TopicLen(-5) should be ok=false")
	}
}

func TestDecodeValueEscapes(t *testing.T) {
	// NETWORK with an escaped space: "Cool\x20Net" -> "Cool Net".
	feat := Parse([]string{"NETWORK=Cool\\x20Net"})
	if got := feat.Network(); got != "Cool Net" {
		t.Errorf("escaped NETWORK = %q, want %q", got, "Cool Net")
	}
	// Backslash itself via \x5C.
	feat2 := Parse([]string{"FOO=a\\x5Cb"})
	if v, _ := feat2.Get("FOO"); v != "a\\b" {
		t.Errorf("escaped FOO = %q, want %q", v, "a\\b")
	}
	// Malformed escape left verbatim.
	feat3 := Parse([]string{"FOO=a\\xZZb"})
	if v, _ := feat3.Get("FOO"); v != "a\\xZZb" {
		t.Errorf("malformed escape = %q, want verbatim", v)
	}
}

func TestEmptyAndNilTokens(t *testing.T) {
	feat := Parse(nil)
	if _, ok := feat.Get("ANY"); ok {
		t.Errorf("Parse(nil) should be empty")
	}
	// Empty string tokens are skipped.
	feat = Parse([]string{"", "NETWORK=X", ""})
	if feat.Network() != "X" {
		t.Errorf("empty tokens should be skipped")
	}
}

func TestTokensFromMessage(t *testing.T) {
	m := &irc.Message{
		Command: "005",
		Params:  []string{"mynick", "PREFIX=(ov)@+", "CHANTYPES=#", "are supported by this server"},
	}
	toks := TokensFromMessage(m)
	want := []string{"PREFIX=(ov)@+", "CHANTYPES=#"}
	if len(toks) != len(want) {
		t.Fatalf("TokensFromMessage = %v, want %v", toks, want)
	}
	for i := range want {
		if toks[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q", i, toks[i], want[i])
		}
	}

	// Round-trip: feed the extracted tokens straight into Parse.
	feat := Parse(toks)
	if feat.PrefixModes() != "ov" || feat.ChanTypes() != "#" {
		t.Errorf("round-trip parse failed: %q %q", feat.PrefixModes(), feat.ChanTypes())
	}

	// Degenerate messages yield no tokens.
	if TokensFromMessage(nil) != nil {
		t.Errorf("TokensFromMessage(nil) should be nil")
	}
	short := &irc.Message{Command: "005", Params: []string{"nick", "trailer"}}
	if TokensFromMessage(short) != nil {
		t.Errorf("TokensFromMessage(no tokens) should be nil")
	}
}

// ergoBatches are the RPL_ISUPPORT tokens our live Ergo server actually emits,
// transcribed from reference/ergo/irc/config.go generateISupport (PREFIX,
// CHANMODES, STATUSMSG, TARGMAX, CASEMAPPING in particular). This exercises the
// quirks of a real modern ircd: a 5-level PREFIX, a TARGMAX entry with an empty
// (unlimited) value (KICK:), an EXTBAN value that begins with a comma, a
// slash-bearing vendor key (draft/CHATHISTORY), and CASEMAPPING=ascii.
var ergoBatches = [][]string{
	{"AWAYLEN=390", "BOT=B", "CASEMAPPING=ascii", "CHANLIMIT=#:100", "CHANMODES=Ibe,k,fl,CEMRUimnstu", "CHANNELLEN=64", "CHANTYPES=#", "ELIST=U"},
	{"EXCEPTS", "EXTBAN=,m", "FORWARD=f", "INVEX", "KICKLEN=390", "MAXLIST=beI:60", "MAXTARGETS=4", "MODES", "MONITOR=100", "draft/CHATHISTORY=1000"},
	{"MSGREFTYPES=msgid,timestamp", "NETWORK=ErgoTest", "NICKLEN=32", "PREFIX=(qaohv)~&@%+", "SAFELIST", "STATUSMSG=~&@%+", "TOPICLEN=390", "WHOX"},
	{"TARGMAX=NAMES:1,LIST:1,KICK:,WHOIS:1,USERHOST:10,PRIVMSG:4,TAGMSG:4,NOTICE:4,MONITOR:100", "UTF8ONLY", "UTF8MAPPING=rfc8265"},
}

func parseErgo() ISupport {
	var feat ISupport
	for _, batch := range ergoBatches {
		feat = feat.Merge(batch)
	}
	return feat
}

func TestErgoRealISupport(t *testing.T) {
	feat := parseErgo()

	// 5-level PREFIX must parse aligned, highest privilege first.
	if got := feat.PrefixModes(); got != "qaohv" {
		t.Errorf("PrefixModes() = %q, want qaohv", got)
	}
	if got := feat.PrefixSymbols(); got != "~&@%+" {
		t.Errorf("PrefixSymbols() = %q, want ~&@%%+", got)
	}
	if mode, ok := feat.PrefixModeForSymbol('%'); !ok || mode != 'h' {
		t.Errorf("PrefixModeForSymbol('%%') = (%q,%v), want (h,true)", mode, ok)
	}

	// CHANMODES splits into ergo's four real groups.
	cm := feat.ChanModes()
	if cm.A != "Ibe" || cm.B != "k" || cm.C != "fl" || cm.D != "CEMRUimnstu" {
		t.Errorf("ChanModes() = %+v, want A=Ibe B=k C=fl D=CEMRUimnstu", cm)
	}

	// CASEMAPPING=ascii: brackets are NOT folded (the key difference from rfc1459).
	if feat.CaseMapping() != CaseASCII {
		t.Fatalf("CaseMapping() = %v, want ascii", feat.CaseMapping())
	}
	if got := feat.CaseMapping().Fold("[Nick]"); got != "[nick]" {
		t.Errorf("ascii Fold([Nick]) = %q, want [nick]", got)
	}

	// TARGMAX: KICK has an empty value -> no limit (ok=false); PRIVMSG:4 -> 4.
	if _, ok := feat.TargMax("KICK"); ok {
		t.Errorf("TargMax(KICK) with empty value should report no limit")
	}
	if n, ok := feat.TargMax("PRIVMSG"); !ok || n != 4 {
		t.Errorf("TargMax(PRIVMSG) = (%d,%v), want (4,true)", n, ok)
	}

	// EXTBAN value begins with a comma; it must survive verbatim.
	if v, ok := feat.Get("EXTBAN"); !ok || v != ",m" {
		t.Errorf("Get(EXTBAN) = (%q,%v), want (\",m\",true)", v, ok)
	}

	// Vendor key with a slash is a normal key.
	if v, ok := feat.Get("draft/CHATHISTORY"); !ok || v != "1000" {
		t.Errorf("Get(draft/CHATHISTORY) = (%q,%v), want (1000,true)", v, ok)
	}

	// Bare flag tokens.
	for _, k := range []string{"MODES", "EXCEPTS", "INVEX", "WHOX", "SAFELIST", "UTF8ONLY"} {
		if v, ok := feat.Get(k); !ok || v != "" {
			t.Errorf("bare %s = (%q,%v), want (\"\",true)", k, v, ok)
		}
	}

	if got := feat.StatusMsg(); got != "~&@%+" {
		t.Errorf("StatusMsg() = %q, want ~&@%%+", got)
	}
	if got := feat.Network(); got != "ErgoTest" {
		t.Errorf("Network() = %q, want ErgoTest", got)
	}
	if n, ok := feat.NickLen(); !ok || n != 32 {
		t.Errorf("NickLen() = (%d,%v), want 32", n, ok)
	}
}
