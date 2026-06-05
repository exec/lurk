package client

import (
	"testing"

	"github.com/exec/lurk/irc"
)

// findEvent returns the first dispatched event with the given command, or nil.
func findEvent(got []Event, command string) *Event {
	for i := range got {
		if got[i].Command() == command {
			return &got[i]
		}
	}
	return nil
}

// TestQuitEventCarriesChannels verifies a QUIT event is annotated with the
// channels the quitter shared with us, captured before state tracking removes
// the member — so the TUI can show the quit in those channel buffers rather than
// the server buffer.
func TestQuitEventCarriesChannels(t *testing.T) {
	c, got := newEventClient("CHANTYPES=#")
	c.st.self = "me" // so our own JOINs create the channels
	// We and bob share #a and #b; bob is alone-with-us in neither extra place.
	feedHandle(t, c, ":me!u@h JOIN #a")
	feedHandle(t, c, ":me!u@h JOIN #b")
	feedHandle(t, c, ":me!u@h JOIN #c")
	feedHandle(t, c, ":bob!b@h JOIN #a")
	feedHandle(t, c, ":bob!b@h JOIN #b")
	// bob is NOT in #c.
	feedHandle(t, c, ":bob!b@h QUIT :bye")

	ev := findEvent(*got, irc.QUIT)
	if ev == nil {
		t.Fatal("no QUIT event dispatched")
	}
	chans := ev.Channels()
	want := map[string]bool{"#a": true, "#b": true}
	if len(chans) != 2 {
		t.Fatalf("QUIT Channels() = %v, want #a and #b", chans)
	}
	for _, ch := range chans {
		if !want[ch] {
			t.Errorf("QUIT Channels() contained unexpected %q (bob was not there): %v", ch, chans)
		}
	}

	// And state tracking still removed bob from the channel.
	for _, mem := range c.Members("#a") {
		if mem.Nick == "bob" {
			t.Errorf("#a still lists bob after his QUIT: %v", c.Members("#a"))
		}
	}
}

// TestNickEventCarriesChannels verifies the same capture for a NICK change: the
// event names only the new nick, but Channels() lists every channel the renamer
// was in so the notice fans out correctly.
func TestNickEventCarriesChannels(t *testing.T) {
	c, got := newEventClient("CHANTYPES=#")
	c.st.self = "me" // so our own JOINs create the channels
	feedHandle(t, c, ":me!u@h JOIN #a")
	feedHandle(t, c, ":me!u@h JOIN #b")
	feedHandle(t, c, ":bob!b@h JOIN #a")
	feedHandle(t, c, ":bob!b@h NICK bobby")

	ev := findEvent(*got, irc.NICK)
	if ev == nil {
		t.Fatal("no NICK event dispatched")
	}
	if chans := ev.Channels(); len(chans) != 1 || chans[0] != "#a" {
		t.Errorf("NICK Channels() = %v, want [#a]", chans)
	}
}

// TestNonMembershipEventHasNoChannels guards that ordinary commands are not
// annotated (Channels() is nil), so routing for them is unchanged.
func TestNonMembershipEventHasNoChannels(t *testing.T) {
	c, got := newEventClient("CHANTYPES=#")
	c.st.self = "me" // so our own JOINs create the channels
	feedHandle(t, c, ":me!u@h JOIN #a")
	feedHandle(t, c, ":bob!b@h PRIVMSG #a :hi")
	ev := findEvent(*got, irc.PRIVMSG)
	if ev == nil {
		t.Fatal("no PRIVMSG event")
	}
	if chans := ev.Channels(); chans != nil {
		t.Errorf("PRIVMSG Channels() = %v, want nil", chans)
	}
}
