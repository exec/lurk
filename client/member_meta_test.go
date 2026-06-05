package client

import (
	"testing"

	"github.com/exec/lurk/irc"
)

// newTrackClient builds a Client whose state carries the given 005 tokens, for
// exercising the track* state mutators in isolation (no transport/registration).
func newTrackClient(tokens ...string) *Client {
	c := &Client{st: newState()}
	if len(tokens) > 0 {
		c.st.mergeISupport(tokens)
	}
	return c
}

// feedTrack parses a raw line and runs it through the state-tracking path.
func feedTrack(t *testing.T, c *Client, line string) {
	t.Helper()
	m, err := irc.Parse(line)
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	c.track(m)
}

// member looks up a member by nick in a channel, failing the test if absent.
func member(t *testing.T, c *Client, channel, nick string) Member {
	t.Helper()
	cs := c.st.channel(channel)
	if cs == nil {
		t.Fatalf("channel %q not tracked", channel)
	}
	m, ok := cs.members[c.st.foldKey(nick)]
	if !ok {
		t.Fatalf("member %q not in %q", nick, channel)
	}
	return *m
}

func TestNamReplyCapturesUserHost(t *testing.T) {
	s := newTestState("PREFIX=(ov)@+", "CHANTYPES=#")
	s.applyNamReply("#c", "@alice!a@h plain")

	cs := s.channel("#c")
	alice, ok := cs.members[s.foldKey("alice")]
	if !ok {
		t.Fatal("alice missing")
	}
	if alice.User != "a" || alice.Host != "h" {
		t.Errorf("alice user/host = %q/%q, want a/h", alice.User, alice.Host)
	}
	if alice.Prefixes != "@" {
		t.Errorf("alice prefixes = %q, want @", alice.Prefixes)
	}
	// A bare entry (no userhost-in-names) leaves user/host empty.
	plain := cs.members[s.foldKey("plain")]
	if plain.User != "" || plain.Host != "" {
		t.Errorf("plain user/host = %q/%q, want empty", plain.User, plain.Host)
	}
}

func TestExtendedJoinCapturesAccount(t *testing.T) {
	c := newTrackClient("CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #c") // our own join establishes the channel first

	// Logged-in joiner: account "acct".
	feedTrack(t, c, ":bob!b@host JOIN #c acct :Bob Realname")
	bob := member(t, c, "#c", "bob")
	if bob.Account != "acct" {
		t.Errorf("bob account = %q, want acct", bob.Account)
	}
	if bob.User != "b" || bob.Host != "host" {
		t.Errorf("bob user/host = %q/%q, want b/host", bob.User, bob.Host)
	}

	// Not-logged-in joiner: account "*" maps to empty.
	feedTrack(t, c, ":carol!c@host2 JOIN #c * :Carol")
	carol := member(t, c, "#c", "carol")
	if carol.Account != "" {
		t.Errorf("carol account = %q, want empty", carol.Account)
	}
}

func TestAccountNotifyUpdatesEverywhere(t *testing.T) {
	c := newTrackClient("CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #a") // establish the channels via our own joins
	feedTrack(t, c, ":me!m@h JOIN #b")
	feedTrack(t, c, ":bob!b@h JOIN #a * :Bob")
	feedTrack(t, c, ":bob!b@h JOIN #b * :Bob")

	// Logs in: account set in every shared channel.
	feedTrack(t, c, ":bob!b@h ACCOUNT bobby")
	if got := member(t, c, "#a", "bob").Account; got != "bobby" {
		t.Errorf("#a bob account = %q, want bobby", got)
	}
	if got := member(t, c, "#b", "bob").Account; got != "bobby" {
		t.Errorf("#b bob account = %q, want bobby", got)
	}

	// Logs out: "*" clears the account everywhere.
	feedTrack(t, c, ":bob!b@h ACCOUNT *")
	if got := member(t, c, "#a", "bob").Account; got != "" {
		t.Errorf("#a bob account = %q, want empty after logout", got)
	}
}

func TestAwayNotifyTogglesAway(t *testing.T) {
	c := newTrackClient("CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #c") // our own join establishes the channel first
	feedTrack(t, c, ":bob!b@h JOIN #c * :Bob")

	feedTrack(t, c, ":bob!b@h AWAY :stepping out")
	if !member(t, c, "#c", "bob").Away {
		t.Error("bob should be away after AWAY :msg")
	}

	feedTrack(t, c, ":bob!b@h AWAY")
	if member(t, c, "#c", "bob").Away {
		t.Error("bob should be back after parameterless AWAY")
	}
}

func TestChghostUpdatesUserHost(t *testing.T) {
	c := newTrackClient("CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #a") // establish the channels via our own joins
	feedTrack(t, c, ":me!m@h JOIN #b")
	feedTrack(t, c, ":bob!b@h JOIN #a * :Bob")
	feedTrack(t, c, ":bob!b@h JOIN #b * :Bob")

	feedTrack(t, c, ":bob!b@h CHGHOST b2 h2")
	for _, ch := range []string{"#a", "#b"} {
		m := member(t, c, ch, "bob")
		if m.User != "b2" || m.Host != "h2" {
			t.Errorf("%s bob user/host = %q/%q, want b2/h2", ch, m.User, m.Host)
		}
	}
}

func TestMetadataPreservedAcrossNamReply(t *testing.T) {
	c := newTrackClient("PREFIX=(ov)@+", "CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #c") // our own join establishes the channel first
	// Learn an account via extended-join.
	feedTrack(t, c, ":bob!b@h JOIN #c acct :Bob")
	// A later NAMES sweep (without account info) must not wipe the account.
	c.st.applyNamReply("#c", "@bob!b@h")
	if got := member(t, c, "#c", "bob").Account; got != "acct" {
		t.Errorf("bob account = %q after NAMES, want preserved acct", got)
	}
	if got := member(t, c, "#c", "bob").Prefixes; got != "@" {
		t.Errorf("bob prefixes = %q, want @ from NAMES", got)
	}
}
