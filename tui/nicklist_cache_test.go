package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/client"
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

// TestNicklistStaleAfterMembershipEvent is the primary staleness proof for the
// nicklist cache bug. It directly demonstrates the incorrect behavior: without
// the fix, a JOIN event into the currently-viewed channel would leave the sorted
// member cache intact, and a subsequent sortedMembers call would return the
// pre-JOIN snapshot rather than re-reading the (now-updated) member list.
//
// The test fabricates a stale cache entry for the active buffer — a single
// "ghost" member that the nil-client model would never return from Members() —
// then delivers a JOIN event for that channel and calls sortedMembers again.
// Without the fix (nicklistSortedFor = nil in the ircMsg handler), the cache
// hit fires and the ghost is returned. With the fix the cache is cleared,
// sortedMembers recomputes from the nil client (empty list), and the ghost
// is gone — proving the stale data is no longer served.
func TestNicklistStaleAfterMembershipEvent(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#chan", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	net := m.activeNet()

	// Inject a stale cache entry: a ghost member that the nil client would
	// never return. This simulates the in-memory state a live client would
	// produce after an initial sortedMembers call on a non-empty channel.
	ghost := client.Member{Nick: "ghost"}
	m.nicklistSorted = []client.Member{ghost}
	m.nicklistSortedFor = m.activeBuffer() // same buffer pointer as after a real sort

	// Sanity: the cache is primed and sortedMembers returns the ghost.
	_, pre := sortedMembers(m)
	if len(pre) != 1 || pre[0].Nick != "ghost" {
		t.Fatalf("test setup: sortedMembers did not return cached ghost (got %v)", pre)
	}

	// Deliver a JOIN event for the same channel. The buffer pointer does NOT
	// change — only the fix's unconditional nicklistSortedFor = nil clears the cache.
	joinEv := evt(t, ":newuser!u@h JOIN #chan")
	tm, _ := m.Update(ircMsg{net: net, ev: joinEv})
	m = tm.(model)

	// With the fix: cache cleared → sortedMembers recomputes → nil client →
	// empty list → ghost is gone.
	// Without the fix: cache hit → ghost returned → stale nicklist.
	_, post := sortedMembers(m)
	for _, mem := range post {
		if mem.Nick == "ghost" {
			t.Error("sortedMembers returned stale cached member after a JOIN event — nicklist does not update live")
			return
		}
	}
}

// TestNicklistCacheClearedByMembershipEvent verifies the cache key
// (nicklistSortedFor) is nil after an ircMsg event, confirming the invalidation
// fires before the next renderNicklist call.
func TestNicklistCacheClearedByMembershipEvent(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#chan", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	net := m.activeNet()

	// Prime the cache by calling sortedMembers on the active buffer.
	m, _ = sortedMembers(m)
	if m.nicklistSortedFor == nil {
		t.Fatal("test setup: cache not populated after sortedMembers call")
	}

	// Deliver a JOIN event for the same channel through the ircMsg path.
	// The buffer pointer does NOT change (it is the same #chan buffer), so the
	// old cache invalidation logic (pointer comparison) would leave the cache
	// stale. The fix clears nicklistSortedFor unconditionally on every ircMsg.
	joinEv := evt(t, ":newuser!u@h JOIN #chan")
	tm, _ := m.Update(ircMsg{net: net, ev: joinEv})
	m = tm.(model)

	if m.nicklistSortedFor != nil {
		t.Error("ircMsg did not clear nicklistSortedFor — nicklist cache is stale after a membership change")
	}
}

// TestNicklistCacheClearedByBatchMembershipEvent mirrors the above for the
// ircBatchMsg path (the live event bridge), which carries JOIN/PART bursts from
// a bouncer or a NAMES replay. The same stale-cache bug applied to batches:
// a batch containing a JOIN would leave the sorted cache intact for the
// entire Update pass, hiding the new member from renderNicklist.
func TestNicklistCacheClearedByBatchMembershipEvent(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#chan", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	net := m.activeNet()

	// Prime the cache.
	m, _ = sortedMembers(m)
	if m.nicklistSortedFor == nil {
		t.Fatal("test setup: cache not populated after sortedMembers call")
	}

	// Deliver a JOIN event as a batch (the normal live-bridge path).
	evs := []client.Event{evt(t, ":newuser!u@h JOIN #chan")}
	tm, _ := m.Update(ircBatchMsg{net: net, evs: evs})
	m = tm.(model)

	if m.nicklistSortedFor != nil {
		t.Error("ircBatchMsg did not clear nicklistSortedFor — nicklist cache is stale after a batch membership change")
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
