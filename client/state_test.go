package client

import (
	"testing"

	"lurk/irc"
)

// feed merges 005-style tokens into a fresh state and returns it, for tests that
// need a particular PREFIX/CASEMAPPING in place.
func newTestState(tokens ...string) *state {
	s := newState()
	if len(tokens) > 0 {
		s.mergeISupport(tokens)
	}
	return s
}

func TestStateNamReplyPrefixes(t *testing.T) {
	s := newTestState("PREFIX=(ov)@+", "CHANTYPES=#")
	s.self = "me"
	s.applyNamReply("#chan", "@op +voice plain @+double")

	cs := s.channel("#chan")
	if cs == nil {
		t.Fatal("#chan not tracked")
	}
	cases := map[string]string{
		"op":     "@",
		"voice":  "+",
		"plain":  "",
		"double": "@+",
	}
	for nick, want := range cases {
		m, ok := cs.members[s.foldKey(nick)]
		if !ok {
			t.Errorf("member %q missing", nick)
			continue
		}
		if m.Prefixes != want {
			t.Errorf("member %q prefixes = %q, want %q", nick, m.Prefixes, want)
		}
	}
}

// TestStateMultiPrefix5Level mirrors Ergo's PREFIX=(qaohv)~&@%+ (five
// membership levels). Cross-checked against reference/ergo/irc/modes/modes.go,
// where ChannelUserModes is ordered founder>admin>op>halfop>voice and
// ChannelModePrefixes maps them to ~&@%+ — so a member holding several modes is
// advertised with prefixes in that descending order under multi-prefix.
func TestStateMultiPrefix5Level(t *testing.T) {
	s := newTestState("PREFIX=(qaohv)~&@%+", "CHANTYPES=#", "CASEMAPPING=ascii")
	s.self = "me"
	// A 353 line as Ergo emits it under multi-prefix: each member is its full
	// prefix run followed by the nick, highest privilege first.
	s.applyNamReply("#c", "~&@%+founder &@admin @op %halfop +voice plain")

	want := map[string]string{
		"founder": "~&@%+",
		"admin":   "&@",
		"op":      "@",
		"halfop":  "%",
		"voice":   "+",
		"plain":   "",
	}
	cs := s.channel("#c")
	if cs == nil {
		t.Fatal("#c not tracked")
	}
	for nick, wantPfx := range want {
		m, ok := cs.members[s.foldKey(nick)]
		if !ok {
			t.Errorf("member %q missing", nick)
			continue
		}
		if m.Prefixes != wantPfx {
			t.Errorf("member %q prefixes = %q, want %q", nick, m.Prefixes, wantPfx)
		}
	}
}

// TestStateModeFounderPrefix exercises the auto-grant MODE a client receives
// when it creates/joins an unregistered channel on Ergo: after the JOIN echo the
// server sends "MODE #chan +o <nick>" (or +q for a registered founder). See
// reference/ergo/irc/channel.go (the join handler sends MODE chname modestr nick
// right after the JOIN). The 'o'->'@' and 'q'->'~' mapping comes from PREFIX.
func TestStateModeFounderPrefix(t *testing.T) {
	s := newTestState("PREFIX=(qaohv)~&@%+", "CHANTYPES=#", "CASEMAPPING=ascii")
	s.self = "me"
	// Client joins; tracking adds self with no prefix.
	s.addChannel("#new")
	s.channel("#new").addMember(s.foldKey, "me", "")

	// Server grants founder.
	s.applyModeChange("#new", "+q", []string{"me"})
	if got := s.channel("#new").members[s.foldKey("me")].Prefixes; got != "~" {
		t.Errorf("after +q, prefixes = %q, want ~", got)
	}
	// Add operator too: ordering must be founder-then-op (~@).
	s.applyModeChange("#new", "+o", []string{"me"})
	if got := s.channel("#new").members[s.foldKey("me")].Prefixes; got != "~@" {
		t.Errorf("after +q+o, prefixes = %q, want ~@", got)
	}
}

// TestStateAsciiCasemapping verifies that under CASEMAPPING=ascii (what the live
// Ergo advertises) only A-Z fold, and the rfc1459 bracket characters do NOT
// fold — i.e. "nick[]" and "nick{}" are distinct nicks.
func TestStateAsciiCasemapping(t *testing.T) {
	s := newTestState("CASEMAPPING=ascii", "CHANTYPES=#")
	if s.foldKey("Foo") != "foo" {
		t.Errorf("ascii fold of Foo = %q, want foo", s.foldKey("Foo"))
	}
	// Under ascii, '[' must NOT fold to '{'.
	if s.foldKey("a[b]") == s.foldKey("a{b}") {
		t.Error("ascii casemapping must not fold []\\^ to {}|~")
	}
}

func TestStateCaseFoldingKeysChannelsAndNicks(t *testing.T) {
	// rfc1459: '[' folds to '{'. A nick "Foo[bar]" and "foo{bar}" are the same.
	s := newTestState("CASEMAPPING=rfc1459", "CHANTYPES=#")
	s.self = "me"
	s.addChannel("#Chan")
	if s.channel("#chan") == nil {
		t.Error("channel lookup not case-folded")
	}
	cs := s.channel("#CHAN")
	if cs == nil {
		t.Fatal("channel lookup #CHAN failed")
	}
	cs.addMember(s.foldKey, "Nick[X]", "@")
	if _, ok := cs.members[s.foldKey("nick{x}")]; !ok {
		t.Error("rfc1459 nick folding not applied to member key")
	}
}

func TestStateModePrefixChanges(t *testing.T) {
	s := newTestState("PREFIX=(ov)@+", "CHANTYPES=#")
	s.self = "me"
	s.applyNamReply("#c", "alice")

	// +o alice -> "@"
	s.applyModeChange("#c", "+o", []string{"alice"})
	if got := s.channel("#c").members[s.foldKey("alice")].Prefixes; got != "@" {
		t.Errorf("after +o, prefixes = %q, want @", got)
	}
	// +v alice -> "@+" (ordered by PREFIX symbol ranking)
	s.applyModeChange("#c", "+v", []string{"alice"})
	if got := s.channel("#c").members[s.foldKey("alice")].Prefixes; got != "@+" {
		t.Errorf("after +v, prefixes = %q, want @+", got)
	}
	// -o alice -> "+"
	s.applyModeChange("#c", "-o", []string{"alice"})
	if got := s.channel("#c").members[s.foldKey("alice")].Prefixes; got != "+" {
		t.Errorf("after -o, prefixes = %q, want +", got)
	}
}

func TestStateRenameAndRemoveEverywhere(t *testing.T) {
	s := newTestState("PREFIX=(ov)@+", "CHANTYPES=#")
	s.self = "me"
	s.applyNamReply("#a", "@bob")
	s.applyNamReply("#b", "+bob")

	s.renameEverywhere("bob", "rob")
	for _, ch := range []string{"#a", "#b"} {
		cs := s.channel(ch)
		if _, gone := cs.members[s.foldKey("bob")]; gone {
			t.Errorf("%s still has old nick bob", ch)
		}
		if _, here := cs.members[s.foldKey("rob")]; !here {
			t.Errorf("%s missing renamed nick rob", ch)
		}
	}

	s.removeEverywhere("rob")
	for _, ch := range []string{"#a", "#b"} {
		if _, here := s.channel(ch).members[s.foldKey("rob")]; here {
			t.Errorf("%s still has rob after removeEverywhere", ch)
		}
	}
}

func TestStateSelfNickFollowsOwnNickChange(t *testing.T) {
	s := newTestState("CHANTYPES=#")
	s.self = "me"
	s.renameEverywhere("me", "newme")
	if s.self != "newme" {
		t.Errorf("self = %q, want newme after own NICK change", s.self)
	}
}

func TestStateTopicTracking(t *testing.T) {
	s := newTestState("CHANTYPES=#", "CASEMAPPING=ascii")
	// RPL_TOPIC (332): <client> <channel> :<topic>
	s.setTopicText("#chan", "welcome to #chan")
	// RPL_TOPICWHOTIME (333): setter + unix time.
	at := parseUnixSeconds("1700000000")
	s.setTopicMeta("#chan", "alice", at)

	cs := s.channel("#chan")
	if cs == nil {
		t.Fatal("#chan not tracked after topic")
	}
	if cs.topic != "welcome to #chan" {
		t.Errorf("topic = %q", cs.topic)
	}
	if cs.topicSetBy != "alice" || !cs.topicAt.Equal(at) {
		t.Errorf("topic meta = %q/%v, want alice/%v", cs.topicSetBy, cs.topicAt, at)
	}

	// A live TOPIC change replaces the text (case-insensitive channel key).
	s.setTopicText("#CHAN", "new topic")
	if s.channel("#chan").topic != "new topic" {
		t.Errorf("topic after change = %q, want 'new topic'", s.channel("#chan").topic)
	}
}

func TestParseUnixSeconds(t *testing.T) {
	if got := parseUnixSeconds("1700000000"); got.Unix() != 1700000000 {
		t.Errorf("parseUnixSeconds = %v", got)
	}
	if got := parseUnixSeconds("bogus"); !got.IsZero() {
		t.Errorf("parseUnixSeconds(bogus) = %v, want zero", got)
	}
}

func TestCapEnabledBeforeConnect(t *testing.T) {
	// Before Connect, the negotiator is nil; CapEnabled must not panic and must
	// report false for any cap (the CLI calls this to decide local echo).
	c := New(Config{Nick: "bob"})
	if c.CapEnabled("echo-message") {
		t.Error("CapEnabled should be false before connecting")
	}
}

func TestDispatchByCommandAndAny(t *testing.T) {
	d := newDispatcher()
	var got []string
	d.on(irc.PRIVMSG, func(*Event) { got = append(got, "privmsg") })
	d.on(irc.PRIVMSG, func(*Event) { got = append(got, "privmsg2") })
	d.onAny(func(ev *Event) { got = append(got, "any:"+ev.Message.Command) })

	d.dispatch(&Event{Message: &irc.Message{Command: irc.PRIVMSG}})
	d.dispatch(&Event{Message: &irc.Message{Command: irc.JOIN}})

	want := []string{"privmsg", "privmsg2", "any:PRIVMSG", "any:JOIN"}
	if len(got) != len(want) {
		t.Fatalf("dispatch order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatch[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{Nick: "bob"}.withDefaults()
	if cfg.User != "bob" || cfg.Realname != "bob" {
		t.Errorf("User/Realname defaults = %q/%q, want bob/bob", cfg.User, cfg.Realname)
	}
	if len(cfg.Caps) == 0 {
		t.Error("Caps default should be DefaultCaps, got empty")
	}

	// SASL configured -> sasl cap auto-appended.
	cfg2 := Config{Nick: "bob", SASL: SASLConfig{Mechanism: "PLAIN", Username: "bob", Password: "x"}}.withDefaults()
	if !containsFold(cfg2.Caps, "sasl") {
		t.Error("sasl cap should be auto-added when SASL is configured")
	}

	// Explicit empty caps stay empty (no defaults), but sasl still added if used.
	cfg3 := Config{Nick: "bob", Caps: []string{}}.withDefaults()
	if len(cfg3.Caps) != 0 {
		t.Errorf("explicit empty Caps should stay empty, got %v", cfg3.Caps)
	}
}
