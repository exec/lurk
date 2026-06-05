package client

import (
	"strings"
	"testing"
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
