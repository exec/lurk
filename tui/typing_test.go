package tui

import (
	"strings"
	"testing"
	"time"
)

// TestTypingPruneAndDeadline checks the data side of the expiry tick: which
// entries survive a prune at a given moment, and when the next one is due.
func TestTypingPruneAndDeadline(t *testing.T) {
	base := time.Now()
	m := newTestModel()
	keyA := typingKey{net: m.activeNet(), target: "#a"}
	keyB := typingKey{net: m.activeNet(), target: "#b"}
	m.typing = map[typingKey][]typingEntry{
		keyA: {
			{nick: "alice", expiry: base.Add(2 * time.Second)},
			{nick: "bob", expiry: base.Add(8 * time.Second)},
		},
		keyB: {
			{nick: "carol", expiry: base.Add(-1 * time.Second)}, // already expired
		},
	}

	// The earliest deadline is carol's, already in the past.
	if d, any := m.nextTypingDeadline(base); !any || d != -1*time.Second {
		t.Fatalf("nextTypingDeadline = (%v, %v), want (-1s, true)", d, any)
	}

	// Pruning at base removes carol; #b empties and is deleted, #a keeps both.
	m = m.pruneTyping(base)
	if _, ok := m.typing[keyB]; ok {
		t.Errorf("expired #b entry was not pruned")
	}
	if len(m.typing[keyA]) != 2 {
		t.Errorf("live #a entries were dropped: %+v", m.typing[keyA])
	}

	// Now the earliest deadline is alice at +2s.
	if d, any := m.nextTypingDeadline(base); !any || d != 2*time.Second {
		t.Fatalf("after prune nextTypingDeadline = (%v, %v), want (2s, true)", d, any)
	}

	// Pruning at +3s expires alice; only bob remains.
	m = m.pruneTyping(base.Add(3 * time.Second))
	if got := m.typing[keyA]; len(got) != 1 || got[0].nick != "bob" {
		t.Errorf("after +3s prune, #a entries = %+v, want just bob", got)
	}

	// At +9s everyone is gone: no deadline, so the tick stops.
	m = m.pruneTyping(base.Add(9 * time.Second))
	if d, any := m.nextTypingDeadline(base.Add(9 * time.Second)); any {
		t.Errorf("nextTypingDeadline after all expired = (%v, true), want false", d)
	}
}

// TestTypingScopedPerNetwork verifies typing indications are keyed by network
// as well as target: with #chan open on two networks, a +typing TAGMSG on one
// must not show while viewing (or be cleared by traffic in) the other
// network's same-named channel.
func TestTypingScopedPerNetwork(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	m.ensureBufferIn(net0, "#chan", BufferChannel)
	_, iB := m.ensureBufferIn(netB, "#chan", BufferChannel)
	m.switchTo(iB)
	m = layout(m)

	// alice types in net0's #chan while the user views netB's #chan.
	m = routeEventOn(m, net0, evt(t, "@+typing=active :alice!a@h TAGMSG #chan"))
	if got := m.typingNicks(typingKey{net: net0, target: "#chan"}); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("net0 typers = %v, want [alice]", got)
	}
	if got := m.typingNicks(typingKey{net: netB, target: "#chan"}); len(got) != 0 {
		t.Errorf("net0's typer leaked into netB's #chan: %v", got)
	}
	if out := stripANSI(renderStatus(m)); strings.Contains(out, "typing") {
		t.Errorf("status bar shows a foreign network's typer while viewing netB's #chan:\n%q", out)
	}

	// A message in netB's #chan must not clear net0's indication.
	m = routeEventOn(m, netB, evt(t, ":alice!a@h PRIVMSG #chan :hi"))
	if got := m.typingNicks(typingKey{net: net0, target: "#chan"}); len(got) != 1 {
		t.Errorf("netB traffic cleared net0's typing indication: %v", got)
	}
}

// TestEnsureTypingTickSchedulesOnce checks the tick is scheduled while typers
// exist, never stacked, and not scheduled when nobody is typing.
func TestEnsureTypingTickSchedulesOnce(t *testing.T) {
	base := time.Now()
	m := newTestModel()
	m.typing = map[typingKey][]typingEntry{
		{net: m.activeNet(), target: "#a"}: {{nick: "alice", expiry: base.Add(6 * time.Second)}},
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
