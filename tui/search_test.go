package tui

import (
	"strings"
	"testing"
)

func searchModel(t *testing.T) model {
	t.Helper()
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	for _, body := range []string{"hello world", "needle one", "filler", "needle two", "tail"} {
		m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :"+body))
	}
	return m
}

func TestRunSearchFindsMatches(t *testing.T) {
	m := searchModel(t)
	note := runSearch(&m, "needle")
	if note != "" {
		t.Fatalf("runSearch note = %q, want empty (jumped to a match)", note)
	}
	if len(m.searchMatches) != 2 {
		t.Fatalf("matches = %d, want 2", len(m.searchMatches))
	}
	if m.searchTerm != "needle" {
		t.Errorf("searchTerm = %q", m.searchTerm)
	}
	// Starts at the most recent match.
	if m.searchPos != 1 {
		t.Errorf("searchPos = %d, want 1 (newest)", m.searchPos)
	}
	if note := m.searchNote(); !strings.Contains(note, "2/2") {
		t.Errorf("searchNote = %q, want it to show 2/2", note)
	}
}

func TestCycleSearchWraps(t *testing.T) {
	m := searchModel(t)
	runSearch(&m, "needle") // pos 1 (newest)
	m.cycleSearch()         // -> older
	if m.searchPos != 0 {
		t.Errorf("after cycle, pos = %d, want 0", m.searchPos)
	}
	m.cycleSearch() // wraps back to newest
	if m.searchPos != 1 {
		t.Errorf("after wrap, pos = %d, want 1", m.searchPos)
	}
}

func TestSearchNoMatchAndClear(t *testing.T) {
	m := searchModel(t)
	if note := runSearch(&m, "zzz"); !strings.Contains(note, "no matches") {
		t.Errorf("missing-term note = %q", note)
	}
	runSearch(&m, "needle")
	if note := runSearch(&m, ""); !strings.Contains(note, "cleared") {
		t.Errorf("empty-term note = %q, want cleared", note)
	}
	if m.searchTerm != "" || m.searchMatches != nil {
		t.Errorf("search not cleared: term=%q matches=%v", m.searchTerm, m.searchMatches)
	}
}

// TestSearchPersistsThroughAction verifies the /search command persists state via
// the action path (a command handler alone could not).
func TestSearchPersistsThroughAction(t *testing.T) {
	m := searchModel(t)
	act, _ := runLine(m, "/search needle")
	if act.kind != actionSearch {
		t.Fatalf("/search kind = %v, want actionSearch", act.kind)
	}
	m = applyAction(m, act)
	if m.searchTerm != "needle" || len(m.searchMatches) != 2 {
		t.Errorf("search state did not persist: term=%q matches=%d", m.searchTerm, len(m.searchMatches))
	}
}

// TestSearchClearedOnBufferSwitch verifies switching buffers drops the search.
func TestSearchClearedOnBufferSwitch(t *testing.T) {
	m := searchModel(t)
	runSearch(&m, "needle")
	m.ensureBuffer("#other", BufferChannel)
	m.switchTo(m.bufferIndex("#other"))
	if m.searchTerm != "" {
		t.Errorf("search not cleared on buffer switch: %q", m.searchTerm)
	}
}
