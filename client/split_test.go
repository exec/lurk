package client

import (
	"strings"
	"testing"

	"github.com/exec/lurk/irc"
)

func TestSplitMessage(t *testing.T) {
	t.Run("short message is one piece", func(t *testing.T) {
		got := splitMessage("hello", 400)
		if len(got) != 1 || got[0] != "hello" {
			t.Errorf("got %q, want [hello]", got)
		}
	})

	t.Run("empty stays one (empty) piece", func(t *testing.T) {
		got := splitMessage("", 400)
		if len(got) != 1 || got[0] != "" {
			t.Errorf("got %q, want one empty piece", got)
		}
	})

	t.Run("splits on word boundaries within budget", func(t *testing.T) {
		words := strings.Fields(strings.Repeat("word ", 300)) // 300 words
		long := strings.Join(words, " ")
		pieces := splitMessage(long, 40)
		if len(pieces) < 2 {
			t.Fatalf("expected multiple pieces, got %d", len(pieces))
		}
		for i, p := range pieces {
			if len(p) > 40 {
				t.Errorf("piece %d is %d bytes, over budget 40: %q", i, len(p), p)
			}
		}
		// Reassembling on spaces yields the original words.
		if strings.Join(pieces, " ") != long {
			t.Errorf("round trip mismatch")
		}
	})

	t.Run("hard-splits a spaceless run without breaking runes", func(t *testing.T) {
		// 100 multi-byte runes, no spaces.
		long := strings.Repeat("é", 100) // 2 bytes each = 200 bytes
		pieces := splitMessage(long, 31)
		joined := strings.Join(pieces, "")
		if joined != long {
			t.Errorf("multibyte round trip mismatch: %d vs %d bytes", len(joined), len(long))
		}
		for _, p := range pieces {
			if len(p) > 31 {
				t.Errorf("piece over budget: %d bytes", len(p))
			}
			if !isValidUTF8Boundary(p) {
				t.Errorf("piece split mid-rune: %q", p)
			}
		}
	})
}

func isValidUTF8Boundary(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// wireLen serializes "<command> <target> :<chunk>\r\n" the way the client
// actually transmits it and returns the byte length of that real wire line.
func wireLen(t *testing.T, command, target, chunk string) int {
	t.Helper()
	line, err := (&irc.Message{Command: command, Params: []string{target, chunk}}).Serialize()
	if err != nil {
		t.Fatalf("serialize %q %q: %v", command, target, err)
	}
	return len(line) // Serialize already includes the trailing CRLF
}

func TestMessageBudget(t *testing.T) {
	// The budget plus the framing overhead must exactly fill (never exceed) the
	// 512-byte line for any command/target.
	cases := []struct{ command, target string }{
		{irc.PRIVMSG, "#chan"},
		{irc.NOTICE, "nick"},
		{irc.PRIVMSG, "@#" + strings.Repeat("c", 180)}, // STATUSMSG + long CHANNELLEN
		{irc.NOTICE, "@+#" + strings.Repeat("x", 250)},
	}
	for _, tc := range cases {
		budget := messageBudget(tc.command, tc.target)
		if budget < 1 {
			t.Errorf("%s %q: budget %d < 1", tc.command, tc.target, budget)
		}
		// A full-budget chunk must serialize to no more than 512 bytes.
		chunk := strings.Repeat("a", budget)
		if got := wireLen(t, tc.command, tc.target, chunk); got > 512 {
			t.Errorf("%s %q: full-budget line is %d bytes, over 512", tc.command, tc.target, got)
		}
	}
}

// TestPrivmsgChunksWithinWireLimit asserts that for a long target plus long
// text, EVERY chunk Privmsg/Notice would send serializes to a line within the
// 512-byte limit — the regression for the target-blind fixed cap.
func TestPrivmsgChunksWithinWireLimit(t *testing.T) {
	// A long STATUSMSG-prefixed channel name (182 bytes) of the kind a server
	// with a large CHANNELLEN could legitimately have.
	target := "@#" + strings.Repeat("channel", 26) // 2 + 182 = 184 bytes
	text := strings.Repeat("the quick brown fox ", 200)

	for _, command := range []string{irc.PRIVMSG, irc.NOTICE} {
		chunks := splitMessage(text, messageBudget(command, target))
		if len(chunks) < 2 {
			t.Fatalf("%s: expected multiple chunks for long text, got %d", command, len(chunks))
		}
		for i, chunk := range chunks {
			if got := wireLen(t, command, target, chunk); got > 512 {
				t.Errorf("%s chunk %d serializes to %d bytes, over the 512 wire limit", command, i, got)
			}
		}
		// No words are lost or split: the words across all chunks equal the words
		// of the original text (word-boundary splitting is preserved for a target
		// this size). Comparing word sequences avoids brittle trailing/leading
		// space bookkeeping that splitMessage trims per chunk.
		if got := strings.Fields(strings.Join(chunks, " ")); !equalWords(got, strings.Fields(text)) {
			t.Errorf("%s: round trip lost or split words (%d vs %d)", command, len(got), len(strings.Fields(text)))
		}
	}
}

// equalWords reports whether two word slices are identical in order and content.
func equalWords(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrivmsgShortMessageUnchanged confirms a normal short message to a normal
// target is sent as a single, unmodified chunk.
func TestPrivmsgShortMessageUnchanged(t *testing.T) {
	const msg = "hello world"
	chunks := splitMessage(msg, messageBudget(irc.PRIVMSG, "#chan"))
	if len(chunks) != 1 || chunks[0] != msg {
		t.Errorf("got %q, want single piece [%q]", chunks, msg)
	}
}
