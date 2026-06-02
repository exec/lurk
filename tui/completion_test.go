package tui

import (
	"strings"
	"testing"
)

func TestWordStart(t *testing.T) {
	tests := []struct {
		text   string
		cursor int
		want   int
	}{
		{"hello", 5, 0},
		{"hi bob", 6, 3}, // cursor at end of "bob"
		{"hi bob", 3, 3}, // cursor just after the space
		{"", 0, 0},
		{"/join #go", 9, 6}, // word after the space
		{"/join", 5, 0},     // single word incl. slash
	}
	for _, tc := range tests {
		got := wordStart([]rune(tc.text), tc.cursor)
		if got != tc.want {
			t.Errorf("wordStart(%q, %d) = %d, want %d", tc.text, tc.cursor, got, tc.want)
		}
	}
}

func TestHasPrefixFold(t *testing.T) {
	tests := []struct {
		s, prefix string
		want      bool
	}{
		{"Alice", "al", true},
		{"alice", "AL", false}, // caller passes already-lowercased prefix; "AL" never matches
		{"Bob", "", true},      // empty prefix matches anything
		{"Bo", "bob", false},   // shorter than prefix
		{"#golang", "#go", true},
	}
	for _, tc := range tests {
		got := hasPrefixFold(tc.s, tc.prefix)
		if got != tc.want {
			t.Errorf("hasPrefixFold(%q, %q) = %v, want %v", tc.s, tc.prefix, got, tc.want)
		}
	}
}

func TestNextCandidateCycles(t *testing.T) {
	cands := []string{"alice", "bob", "carol"}
	// From an empty word, the first sorting candidate is chosen.
	if got := nextCandidate(cands, ""); got != "alice" {
		t.Errorf("nextCandidate(empty) = %q, want alice", got)
	}
	// From a full candidate, advance to the next.
	if got := nextCandidate(cands, "alice"); got != "bob" {
		t.Errorf("nextCandidate(alice) = %q, want bob", got)
	}
	if got := nextCandidate(cands, "bob"); got != "carol" {
		t.Errorf("nextCandidate(bob) = %q, want carol", got)
	}
	// Past the last, wrap around to the first.
	if got := nextCandidate(cands, "carol"); got != "alice" {
		t.Errorf("nextCandidate(carol) = %q, want alice (wrap)", got)
	}
	// A partial prefix advances to the first candidate sorting strictly after it.
	if got := nextCandidate(cands, "a"); got != "alice" {
		t.Errorf("nextCandidate(a) = %q, want alice", got)
	}
}

func TestCommandCandidates(t *testing.T) {
	// All command candidates carry a leading slash and lower-case name, sorted.
	all := commandCandidates("")
	if len(all) != len(commands) {
		t.Fatalf("commandCandidates(\"\") returned %d, want %d (all commands)", len(all), len(commands))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1] >= all[i] {
			t.Errorf("command candidates not sorted: %q >= %q", all[i-1], all[i])
		}
	}
	for _, c := range all {
		if !strings.HasPrefix(c, "/") {
			t.Errorf("command candidate %q missing leading slash", c)
		}
	}

	// Prefix filtering: "j" should yield /join and nothing else.
	j := commandCandidates("j")
	if len(j) != 1 || j[0] != "/join" {
		t.Errorf("commandCandidates(\"j\") = %v, want [/join]", j)
	}
}

func TestCompletionCandidatesCommandAtLineStart(t *testing.T) {
	m := newTestModel()
	// A slash-led first word offers commands.
	got := completionCandidates(m, "/j", true)
	if len(got) != 1 || got[0] != "/join" {
		t.Errorf("completionCandidates(/j, firstWord) = %v, want [/join]", got)
	}
}

func TestCompleteSubstitutesCommand(t *testing.T) {
	m := newTestModel()
	m.input.SetValue("/jo")
	m.input.CursorEnd()
	m.input = complete(m)
	// "/jo" has the single candidate "/join"; completing appends a trailing space.
	if got := m.input.Value(); got != "/join " {
		t.Errorf("after complete, input = %q, want %q", got, "/join ")
	}
}

func TestHistoryNavigation(t *testing.T) {
	m := newTestModel()
	m = pushHistory(m, "first")
	m = pushHistory(m, "second")
	// pushHistory leaves the cursor just past the end.
	if m.histIndex != len(m.history) {
		t.Fatalf("after push, histIndex = %d, want %d", m.histIndex, len(m.history))
	}

	// Up recalls the most recent line.
	m = historyPrev(m)
	if got := m.input.Value(); got != "second" {
		t.Errorf("first Up = %q, want second", got)
	}
	// Up again recalls the older line.
	m = historyPrev(m)
	if got := m.input.Value(); got != "first" {
		t.Errorf("second Up = %q, want first", got)
	}
	// At the oldest, Up stays put.
	m = historyPrev(m)
	if got := m.input.Value(); got != "first" {
		t.Errorf("Up at oldest = %q, want first", got)
	}
	// Down walks forward.
	m = historyNext(m)
	if got := m.input.Value(); got != "second" {
		t.Errorf("Down = %q, want second", got)
	}
	// Down past the newest clears to an empty fresh line.
	m = historyNext(m)
	if got := m.input.Value(); got != "" {
		t.Errorf("Down past newest = %q, want empty", got)
	}
}

func TestPushHistorySkipsDuplicate(t *testing.T) {
	m := newTestModel()
	m = pushHistory(m, "same")
	m = pushHistory(m, "same")
	if len(m.history) != 1 {
		t.Errorf("consecutive duplicate not skipped: history = %v", m.history)
	}
	m = pushHistory(m, "other")
	m = pushHistory(m, "same")
	if len(m.history) != 3 {
		t.Errorf("non-consecutive duplicate wrongly skipped: history = %v", m.history)
	}
}
