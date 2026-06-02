package tui

import (
	"strings"
	"testing"
	"time"

	"lurk/client"
	"lurk/irc"
)

func TestFormatStandardReply(t *testing.T) {
	tm := newTheme()
	cases := []struct {
		line string
		want string
	}{
		{
			":srv FAIL JOIN ACCOUNT_REQUIRED #chan :You must register to join this channel",
			"FAIL JOIN ACCOUNT_REQUIRED #chan: You must register to join this channel",
		},
		{
			":srv WARN * BAD_IDEA :This is risky",
			"WARN * BAD_IDEA: This is risky",
		},
		{
			":srv NOTE NICK PROGRESS :working on it",
			"NOTE NICK PROGRESS: working on it",
		},
	}
	for _, c := range cases {
		m, err := irc.Parse(c.line)
		if err != nil {
			t.Fatalf("parse %q: %v", c.line, err)
		}
		got := stripANSI(tm.formatStandardReply(client.Event{Message: m}))
		if !strings.Contains(got, c.want) {
			t.Errorf("formatStandardReply(%q) = %q, want it to contain %q", c.line, got, c.want)
		}
	}
}

func TestStandardReplyRoutedToActiveBuffer(t *testing.T) {
	m := newTestModel()
	before := len(m.buffers[0].lines)
	m = routeEvent(m, evt(t, ":srv FAIL JOIN ACCOUNT_REQUIRED :nope"))
	if got := len(m.buffers[0].lines); got != before+1 {
		t.Errorf("FAIL wrote %d lines, want 1", got-before)
	}
	last := m.buffers[0].lines[len(m.buffers[0].lines)-1]
	if !strings.Contains(stripANSI(last), "FAIL JOIN ACCOUNT_REQUIRED") {
		t.Errorf("rendered line = %q", stripANSI(last))
	}
}

func TestTypingNoteAndExpiry(t *testing.T) {
	m := newTestModel()
	m = m.noteTyping("#chan", "alice")
	if got := m.typingNicks("#chan"); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("typingNicks = %v, want [alice]", got)
	}
	// Expire it by backdating the entry.
	m.typing["#chan"][0].expiry = time.Now().Add(-time.Second)
	if got := m.typingNicks("#chan"); len(got) != 0 {
		t.Errorf("expired typingNicks = %v, want empty", got)
	}
}

func TestRouteTagmsgTracksTyping(t *testing.T) {
	m := newTestModel()
	m = routeEvent(m, evt(t, "@+typing=active :alice!a@h TAGMSG #chan"))
	if got := m.typingNicks("#chan"); len(got) != 1 {
		t.Fatalf("after active: typingNicks = %v, want [alice]", got)
	}
	// "done" clears it.
	m = routeEvent(m, evt(t, "@+typing=done :alice!a@h TAGMSG #chan"))
	if got := m.typingNicks("#chan"); len(got) != 0 {
		t.Errorf("after done: typingNicks = %v, want empty", got)
	}
}

func TestTypingClearedByMessage(t *testing.T) {
	m := newTestModel()
	m = routeEvent(m, evt(t, "@+typing=active :alice!a@h TAGMSG #chan"))
	if len(m.typingNicks("#chan")) != 1 {
		t.Fatal("expected alice typing")
	}
	// A message from alice ends her typing indication.
	m = routeEvent(m, evt(t, ":alice!a@h PRIVMSG #chan :hello"))
	if got := m.typingNicks("#chan"); len(got) != 0 {
		t.Errorf("typing not cleared by message: %v", got)
	}
}

func TestTypingNote(t *testing.T) {
	cases := []struct {
		nicks []string
		want  string
	}{
		{nil, ""},
		{[]string{"alice"}, "alice is typing…"},
		{[]string{"alice", "bob"}, "alice and bob are typing…"},
		{[]string{"a", "b", "c"}, "3 people are typing…"},
	}
	for _, c := range cases {
		if got := typingNote(c.nicks); got != c.want {
			t.Errorf("typingNote(%v) = %q, want %q", c.nicks, got, c.want)
		}
	}
}

func TestChatHistoryDivider(t *testing.T) {
	tm := newTheme()
	b := newBuffer("#chan", BufferChannel)

	// The first history line draws the divider; later ones do not repeat it.
	b.markHistory(tm)
	b.addLine("old message")
	b.markHistory(tm)
	b.addLine("older still")

	joined := strings.Join(linesText(b.lines), "\n")
	if got := strings.Count(joined, "── history ──"); got != 1 {
		t.Errorf("divider count = %d, want exactly 1:\n%s", got, joined)
	}
}

// TestLiveMessageMarksUnread guards the scoping of the chathistory unread skip
// in routeText: a normal (non-batched) message to a non-active channel must
// still bump the unread counter. (The complementary "history does NOT mark
// unread" case can't be constructed here — Event.batchType is unexported to the
// tui package — but it is the same BatchType() guard, and BatchType plumbing is
// covered end-to-end by client.TestBatchTypeResolution.)
func TestLiveMessageMarksUnread(t *testing.T) {
	m := newTestModel() // active = server buffer (index 0)
	m = routeEvent(m, evt(t, ":bob!b@h PRIVMSG #chan :live message"))

	b := m.buffer("#chan")
	if b == nil {
		t.Fatal("#chan buffer was not opened")
	}
	if b.Unread != 1 {
		t.Errorf("unread = %d after a live background message, want 1", b.Unread)
	}
}

func linesText(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = stripANSI(l)
	}
	return out
}
