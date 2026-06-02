package client

import "testing"

// TestKickRemovesMember verifies KICK is tracked: a kicked third party is
// dropped from the channel's members (the channel stays joined), and being
// kicked oneself forgets the channel entirely.
func TestKickRemovesMember(t *testing.T) {
	c := newTrackClient("CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #c * :Me")
	feedTrack(t, c, ":bob!b@h JOIN #c * :Bob")

	// Someone else is kicked: removed from members, channel still tracked.
	feedTrack(t, c, ":op!o@h KICK #c bob :spam")
	cs := c.st.channel("#c")
	if cs == nil {
		t.Fatal("#c should remain tracked after kicking another user")
	}
	if _, ok := cs.members[c.st.foldKey("bob")]; ok {
		t.Error("bob should be removed from #c after KICK")
	}

	// We are kicked: the channel is forgotten.
	feedTrack(t, c, ":op!o@h KICK #c me :bye")
	if c.st.channel("#c") != nil {
		t.Error("#c should be forgotten after we are kicked")
	}
}

// TestKickCommaSeparatedTargets verifies multiple users kicked in one KICK
// (comma-separated) are all removed.
func TestKickCommaSeparatedTargets(t *testing.T) {
	c := newTrackClient("CHANTYPES=#")
	c.st.self = "me"
	feedTrack(t, c, ":me!m@h JOIN #c * :Me")
	feedTrack(t, c, ":bob!b@h JOIN #c * :Bob")
	feedTrack(t, c, ":carol!c@h JOIN #c * :Carol")

	feedTrack(t, c, ":op!o@h KICK #c bob,carol :cleanup")
	cs := c.st.channel("#c")
	for _, n := range []string{"bob", "carol"} {
		if _, ok := cs.members[c.st.foldKey(n)]; ok {
			t.Errorf("%s should be removed after comma-separated KICK", n)
		}
	}
}
