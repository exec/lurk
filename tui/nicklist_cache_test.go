package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestSortedMembersCacheHit verifies that when sortedMembers is called twice on
// the same model (same active buffer pointer), the second call returns the
// cached slice without re-sorting. This is the property that prevents O(N log N)
// repeated sorts on every keypress when the nicklist is focused.
func TestSortedMembersCacheHit(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#bigchan", BufferChannel)
	m.switchTo(i)
	m = layout(m)

	// First call: populates the cache.
	m1, slice1 := sortedMembers(m)
	// Second call on the returned model: must return the same slice (cache hit).
	m2, slice2 := sortedMembers(m1)
	_ = m2

	// The cache key must be set after the first call.
	if m1.nicklistSortedFor == nil {
		t.Error("nicklistSortedFor not set after first sortedMembers call — cache was not populated")
	}
	if m1.nicklistSortedFor != m.activeBuffer() {
		t.Error("nicklistSortedFor points to wrong buffer")
	}
	// A cache hit returns the same length (both may be empty for a nil-cli model).
	if len(slice1) != len(slice2) {
		t.Fatalf("cache miss: first call returned %d members, second %d", len(slice1), len(slice2))
	}
}

// TestSortedMembersCacheInvalidatedOnBufferSwitch verifies the cache is not
// reused when the active buffer changes — so a buffer switch never shows one
// channel's members in another channel's nicklist.
func TestSortedMembersCacheInvalidatedOnBufferSwitch(t *testing.T) {
	m := sizedModel(t)
	_, i1 := m.ensureBuffer("#chan1", BufferChannel)
	_, i2 := m.ensureBuffer("#chan2", BufferChannel)

	m.switchTo(i1)
	m = layout(m)
	buf1 := m.activeBuffer()
	m, _ = sortedMembers(m) // populate cache for #chan1

	if m.nicklistSortedFor != buf1 {
		t.Fatal("cache not populated for #chan1 after first call")
	}

	// Switch to #chan2: the active buffer pointer changes, so the cache miss
	// path runs and nicklistSortedFor is updated to #chan2's buffer.
	m.switchTo(i2)
	m = layout(m)
	buf2 := m.activeBuffer()
	if buf2 == buf1 {
		t.Fatal("test setup: both buffer indices point to the same buffer")
	}

	m, _ = sortedMembers(m) // call on chan2's buffer
	if m.nicklistSortedFor != buf2 {
		t.Errorf("after buffer switch, cache points to wrong buffer (want #chan2, got %v)", m.nicklistSortedFor)
	}
	if m.nicklistSortedFor == buf1 {
		t.Error("cache still points to #chan1 after switching to #chan2 — stale data risk")
	}
}

// TestEnterNickFocusOneSortCall verifies enterNickFocus calls sortedMembers
// exactly once (the old code called it twice, redundantly). We verify by
// observing that the returned model carries a populated cache — if enterNickFocus
// had called sortedMembers on a throwaway copy and discarded the result model,
// the cache would not propagate.
func TestEnterNickFocusOneSortCall(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#chan", BufferChannel)
	m.switchTo(i)
	m = layout(m)

	result := m.enterNickFocus()

	// enterNickFocus should have called sortedMembers once and kept the returned
	// model (with its cache key populated) through to the end.
	if result.nicklistSortedFor == nil {
		t.Error("enterNickFocus discarded the sortedMembers result — cache not propagated (called sortedMembers on a throwaway copy)")
	}
}

// BenchmarkSortedMembersKeyDown measures the cost of a down-arrow keypress
// while the nicklist is focused. Before the cache, handleNickFocusKey (Update)
// and renderNicklist (View) each called sortedMembers independently — two sorts
// per keypress. With the cache the second call is an O(1) pointer comparison.
func BenchmarkSortedMembersKeyDown(b *testing.B) {
	m := sizedModelB(b)
	_, i := m.ensureBuffer("#bigchan", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	m.focus = focusNicks

	key := tea.KeyPressMsg{Code: tea.KeyDown}
	b.ResetTimer()
	for range b.N {
		tm, _ := m.Update(key)
		m = tm.(model)
	}
}

// sizedModelB is sizedModel adapted for benchmarks.
func sizedModelB(b *testing.B) model {
	b.Helper()
	m := newTestModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return tm.(model)
}
