package tui

import (
	"strings"
	"testing"
)

// TestNoDividerForLiveMessageInActiveBuffer is the regression for #7: a message
// arriving in the buffer you are watching (after returning to it with nothing
// pending) must not draw a "── new messages ──" divider above itself.
func TestNoDividerForLiveMessageInActiveBuffer(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)

	_, ia := m.ensureBuffer("#a", BufferChannel)
	_, ib := m.ensureBuffer("#b", BufferChannel)
	m.switchTo(ia)
	m = layout(m)

	// Two messages read while viewing #a.
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :one"))
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :two"))

	// Leave to #b (sets #a.readMarker = 2) and come straight back: nothing new.
	m.switchTo(ib)
	m.switchTo(ia)
	m = layout(m)

	// A live message now arrives while we are watching #a.
	m = appendLine(m, m.buffer("#a"), evt(t, ":x!x@h PRIVMSG #a :three"))

	out := stripANSI(m.buffer("#a").wrapped())
	if strings.Contains(out, "new messages") {
		t.Errorf("a live message in the active buffer must not raise a divider:\n%s", out)
	}
	// The marker must track the live line count, not lag behind it.
	if got, want := m.buffer("#a").readMarker, len(m.buffer("#a").lines); got != want {
		t.Errorf("readMarker = %d, want %d (pinned to end for the active buffer)", got, want)
	}
}

// TestSearchSurvivesTrim is the regression for #8: stored search indices must
// stay valid after a scrollback trim drops lines off the front, so Ctrl-R lands
// on the right line rather than out of range.
func TestSearchSurvivesTrim(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	b := m.activeBuffer()

	// Pre-seed the scrollback right up to the cap with a unique marker near the
	// front, so a small handful of appends forces a front trim that drops it.
	b.lines = make([]string, 0, scrollbackLimit)
	b.lines = append(b.lines, "UNIQUEMARK here")
	for n := 1; n < scrollbackLimit; n++ {
		b.lines = append(b.lines, "filler")
	}
	b.refresh()

	if note := runSearch(&m, "uniquemark"); note != "" {
		t.Fatalf("runSearch note = %q, want empty (jumped to the match)", note)
	}
	if len(m.searchMatches) != 1 || m.searchMatches[0] != 0 {
		t.Fatalf("matches = %v, want [0]", m.searchMatches)
	}

	// A handful of appends past the cap trims the front, dropping the UNIQUEMARK
	// line — the search match should be discarded, not left dangling.
	for n := 0; n < 5; n++ {
		m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :flood"))
	}

	for _, mi := range m.searchMatches {
		if mi < 0 || mi >= len(m.activeBuffer().lines) {
			t.Fatalf("stale match index %d after trim (len %d)", mi, len(m.activeBuffer().lines))
		}
	}
	if len(m.searchMatches) != 0 {
		t.Errorf("dropped match should be discarded, got %v", m.searchMatches)
	}
	if m.searchPos < 0 {
		t.Errorf("searchPos %d should be clamped to >= 0", m.searchPos)
	}
	// cycleSearch must not panic / go out of range on an emptied match set.
	m.cycleSearch()
}

// TestSearchIndexShiftsWithTrim verifies a match that survives the trim is
// shifted to its new position so Ctrl-R still lands on the correct line.
func TestSearchIndexShiftsWithTrim(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	_, i := m.ensureBuffer("#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	b := m.activeBuffer()

	// Pre-seed up to the cap with a unique marker a few lines from the front, so a
	// small trim drops earlier lines but keeps the marker — its index must shift.
	b.lines = make([]string, 0, scrollbackLimit)
	for n := 0; n < scrollbackLimit; n++ {
		if n == 10 {
			b.lines = append(b.lines, "KEEPMARK x")
		} else {
			b.lines = append(b.lines, "filler")
		}
	}
	b.refresh()
	runSearch(&m, "keepmark")
	if len(m.searchMatches) != 1 || m.searchMatches[0] != 10 {
		t.Fatalf("matches = %v, want [10]", m.searchMatches)
	}

	// Each append past the cap trims one line off the front; five appends drop the
	// five frontmost lines, shifting the KEEPMARK match from index 10 down to 5 but
	// keeping it.
	for n := 0; n < 5; n++ {
		m = appendLine(m, m.activeBuffer(), evt(t, ":a!a@h PRIVMSG #go :flood"))
	}

	if len(m.searchMatches) != 1 {
		t.Fatalf("match should survive a small trim, got %d", len(m.searchMatches))
	}
	idx := m.searchMatches[0]
	if !strings.Contains(stripANSI(m.activeBuffer().lines[idx]), "KEEPMARK") {
		t.Errorf("shifted match index %d points at %q, want the KEEPMARK line",
			idx, stripANSI(m.activeBuffer().lines[idx]))
	}
}

// TestCrossNetworkPartTargetsOwningServer is the regression for #9: /part #dup
// for a channel that lives on a non-active network must PART that network's
// server and close that network's buffer, not the focused network's.
func TestCrossNetworkPartTargetsOwningServer(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	// #dup is open only on netB; focus stays on net0.
	m.ensureBufferIn(netB, "#dup", BufferChannel)
	m.switchTo(0) // focus net0's server buffer

	if m.netBuffer(netB, "#dup") == nil {
		t.Fatal("setup: #dup should exist on netB")
	}

	act, _ := runLine(m, "/part #dup")
	if act.kind != actionClose || act.target != "#dup" {
		t.Fatalf("/part action = %+v, want actionClose #dup", act)
	}
	m = applyAction(m, act)

	if m.netBuffer(netB, "#dup") != nil {
		t.Errorf("netB's #dup buffer should be closed after cross-network /part")
	}
	// net0 never had a #dup; nothing about net0 should change.
	if m.netBuffer(net0, "#dup") != nil {
		t.Errorf("net0 should not have gained a #dup buffer")
	}
}

// TestCrossNetworkPartUnknownChannel verifies /part of a named channel open on
// no network surfaces a notice rather than PARTing the active server.
func TestCrossNetworkPartUnknownChannel(t *testing.T) {
	m, _, _ := twoNetModel(t)
	act, _ := runLine(m, "/part #nowhere")
	if act.kind != actionInfo || !strings.Contains(act.text, "no such channel") {
		t.Fatalf("/part #nowhere = %+v, want an info notice", act)
	}
}

// TestCrossNetworkCloseTargetsOwningServer is the regression for #9 via /close:
// closing a channel on a non-active network must close that network's buffer.
func TestCrossNetworkCloseTargetsOwningServer(t *testing.T) {
	m, _, netB := twoNetModel(t)
	m.ensureBufferIn(netB, "#dup", BufferChannel)
	m.switchTo(0)

	act, _ := runLine(m, "/close #dup")
	if act.kind != actionClose || act.target != "#dup" {
		t.Fatalf("/close action = %+v, want actionClose #dup", act)
	}
	m = applyAction(m, act)
	if m.netBuffer(netB, "#dup") != nil {
		t.Errorf("netB's #dup buffer should be closed after cross-network /close")
	}
}

// TestRemoveNetworkClearsRehomedActivity is the regression for #11: the survivor
// the focus re-homes onto must have its unread/highlight cleared.
func TestRemoveNetworkClearsRehomedActivity(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	// Give net0's server buffer some unread/highlight, then make it the survivor.
	sb0 := m.serverBuffer(net0)
	sb0.Unread = 3
	sb0.Highlight = true

	// Focus a netB buffer so removing netB re-homes onto net0's buffer.
	m.ensureBufferIn(netB, "#x", BufferChannel)
	m.switchTo(m.bufferIndexIn(netB, "#x"))

	m = m.removeNetwork(netB)

	rehomed := m.buffers[m.active]
	if rehomed.Unread != 0 || rehomed.Highlight {
		t.Errorf("re-homed buffer keeps stale activity: unread=%d highlight=%v",
			rehomed.Unread, rehomed.Highlight)
	}
}

// TestStaleEventDroppedForRemovedNetwork is the regression for #16: an event for
// a network that was already removed must be dropped, not routed into net0's
// server buffer (or re-create a buffer back-pointing at the dead network).
func TestStaleEventDroppedForRemovedNetwork(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	before := len(m.serverBuffer(net0).lines)

	m = m.removeNetwork(netB) // netB is gone

	// A late event tagged with the removed network.
	m = routeEventOn(m, netB, evt(t, ":a!a@h PRIVMSG #ghost :leak"))

	if got := len(m.serverBuffer(net0).lines); got != before {
		t.Errorf("stale event leaked into net0's server buffer: lines %d -> %d", before, got)
	}
	if m.netBuffer(netB, "#ghost") != nil {
		t.Errorf("stale event re-created a buffer for the removed network")
	}
}

// TestServerBufferNilForUnknownNet verifies serverBuffer/targetBuffer no longer
// fall back to buffers[0] for a network that is not connected.
func TestServerBufferNilForUnknownNet(t *testing.T) {
	m, _, netB := twoNetModel(t)
	m = m.removeNetwork(netB)
	if b := m.serverBuffer(netB); b != nil {
		t.Errorf("serverBuffer(removed) = %v, want nil", b)
	}
}

// TestStatusBarSanitizesNetworkAndTyping is the regression for #10: the status
// bar must not emit raw ESC from a server-controlled network label or a
// typing-indicator nick.
func TestStatusBarSanitizesNetworkAndTyping(t *testing.T) {
	m, _, _ := twoNetModel(t)
	m.networks[0].name = "ev\x1b[31mil"
	m.cli = m.networks[0].cli

	// A typing nick laden with ESC, in the active channel.
	_, i := m.ensureBufferIn(m.networks[0], "#go", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	m = m.noteTyping(asciiLower("#go"), "ev\x1bil")

	// The renderer's own SGR uses ESC, so we cannot assert "no ESC at all";
	// instead assert the injected payload sequences are gone.
	out := renderStatus(m)
	if strings.Contains(out, "ev\x1b[31mil") {
		t.Errorf("status bar leaked the raw network ESC payload:\n%q", out)
	}
	if strings.Contains(out, "ev\x1bil") {
		t.Errorf("status bar leaked the raw typing-nick ESC payload:\n%q", out)
	}
}

// TestSidebarSanitizesNetworkHeader is the regression for #10's sidebar header:
// the network-group label must be sanitized before rendering.
func TestSidebarSanitizesNetworkHeader(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	netB.name = "ev\x1bil"
	m.ensureBufferIn(net0, "#a", BufferChannel)
	m.ensureBufferIn(netB, "#b", BufferChannel)

	out := renderSidebar(m, sidebarWidth, 20)
	if strings.Contains(out, "ev\x1bil") {
		t.Errorf("sidebar header leaked the raw network ESC payload:\n%q", out)
	}
}

// TestNickMenuSanitizesTitle is the regression for #15: the per-user context
// menu title must sanitize the subject nick.
func TestNickMenuSanitizesTitle(t *testing.T) {
	m := newTestModel()
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)
	m.menuOpen = true
	m.menuNick = "ev\x1bil"
	out := renderNickMenu(m, nicklistWidth, 10)
	if strings.Contains(out, "ev\x1bil") {
		t.Errorf("nick menu title leaked the raw ESC payload:\n%q", out)
	}
}
