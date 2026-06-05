package tui

import (
	"testing"
	"time"
)

// TestTypingPruneAndDeadline checks the data side of the expiry tick: which
// entries survive a prune at a given moment, and when the next one is due.
func TestTypingPruneAndDeadline(t *testing.T) {
	base := time.Now()
	m := newTestModel()
	m.typing = map[string][]typingEntry{
		"#a": {
			{nick: "alice", expiry: base.Add(2 * time.Second)},
			{nick: "bob", expiry: base.Add(8 * time.Second)},
		},
		"#b": {
			{nick: "carol", expiry: base.Add(-1 * time.Second)}, // already expired
		},
	}

	// The earliest deadline is carol's, already in the past.
	if d, any := m.nextTypingDeadline(base); !any || d != -1*time.Second {
		t.Fatalf("nextTypingDeadline = (%v, %v), want (-1s, true)", d, any)
	}

	// Pruning at base removes carol; #b empties and is deleted, #a keeps both.
	m = m.pruneTyping(base)
	if _, ok := m.typing["#b"]; ok {
		t.Errorf("expired #b entry was not pruned")
	}
	if len(m.typing["#a"]) != 2 {
		t.Errorf("live #a entries were dropped: %+v", m.typing["#a"])
	}

	// Now the earliest deadline is alice at +2s.
	if d, any := m.nextTypingDeadline(base); !any || d != 2*time.Second {
		t.Fatalf("after prune nextTypingDeadline = (%v, %v), want (2s, true)", d, any)
	}

	// Pruning at +3s expires alice; only bob remains.
	m = m.pruneTyping(base.Add(3 * time.Second))
	if got := m.typing["#a"]; len(got) != 1 || got[0].nick != "bob" {
		t.Errorf("after +3s prune, #a entries = %+v, want just bob", got)
	}

	// At +9s everyone is gone: no deadline, so the tick stops.
	m = m.pruneTyping(base.Add(9 * time.Second))
	if d, any := m.nextTypingDeadline(base.Add(9 * time.Second)); any {
		t.Errorf("nextTypingDeadline after all expired = (%v, true), want false", d)
	}
}

// TestEnsureTypingTickSchedulesOnce checks the tick is scheduled while typers
// exist, never stacked, and not scheduled when nobody is typing.
func TestEnsureTypingTickSchedulesOnce(t *testing.T) {
	base := time.Now()
	m := newTestModel()
	m.typing = map[string][]typingEntry{
		"#a": {{nick: "alice", expiry: base.Add(6 * time.Second)}},
	}

	m, cmd := m.ensureTypingTick(base)
	if cmd == nil || !m.typingTicking {
		t.Fatalf("ensureTypingTick should schedule a tick and set the in-flight flag")
	}

	// A second call while one is pending must not stack a duplicate.
	if _, cmd2 := m.ensureTypingTick(base); cmd2 != nil {
		t.Errorf("ensureTypingTick stacked a duplicate tick")
	}

	// With no typers and the flag cleared, nothing is scheduled.
	m.typing = nil
	m.typingTicking = false
	if _, cmd3 := m.ensureTypingTick(base); cmd3 != nil {
		t.Errorf("ensureTypingTick scheduled a tick with no typers")
	}
}
