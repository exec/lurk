package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// search.go implements scrollback search: /search <term> finds the matching
// lines in the active buffer and scrolls to the most recent one; Ctrl-R cycles to
// the next (older) match. The match position is shown in the status bar. The
// search is per active buffer and cleared by /search with no argument (or by a
// buffer switch via clearSearch).

// runSearch finds term in the active buffer and jumps to the most recent match.
// An empty term clears any active search. It returns a status string for the
// info line.
func runSearch(m *model, term string) string {
	term = strings.TrimSpace(term)
	if term == "" {
		m.clearSearch()
		return "search cleared"
	}
	b := m.activeBuffer()
	needle := strings.ToLower(term)

	matches := matches(b.lines, needle)
	m.searchTerm = needle
	m.searchMatches = matches
	if len(matches) == 0 {
		return "no matches for " + term
	}
	m.searchPos = len(matches) - 1 // most recent
	scrollToLine(b, matches[m.searchPos])
	return ""
}

// matches returns the indices of lines whose visible text contains needle
// (already lower-cased), in ascending order.
func matches(lines []string, needle string) []int {
	var out []int
	for i, line := range lines {
		if strings.Contains(strings.ToLower(stripANSI(line)), needle) {
			out = append(out, i)
		}
	}
	return out
}

// cycleSearch moves to the next (older) match, wrapping, and scrolls to it. It is
// a no-op when no search is active.
func (m *model) cycleSearch() {
	if len(m.searchMatches) == 0 {
		return
	}
	n := len(m.searchMatches)
	m.searchPos = (m.searchPos - 1 + n) % n
	scrollToLine(m.activeBuffer(), m.searchMatches[m.searchPos])
}

// shiftSearchMatches keeps the stored match indices valid after a scrollback
// trim dropped `drop` lines off the front of the active buffer. Matches are
// absolute indices into b.lines, so each shifts down by drop; any that fall off
// the front are discarded and searchPos is clamped to what remains. It is a
// no-op when no search is active or nothing was dropped. (Search is only ever
// scoped to the active buffer, so the active buffer is the one whose trim
// matters.)
func (m *model) shiftSearchMatches(drop int) {
	if drop <= 0 || len(m.searchMatches) == 0 {
		return
	}
	out := m.searchMatches[:0]
	for _, idx := range m.searchMatches {
		if idx -= drop; idx >= 0 {
			out = append(out, idx)
		}
	}
	m.searchMatches = out
	if m.searchPos >= len(out) {
		m.searchPos = len(out) - 1
	}
	if m.searchPos < 0 {
		m.searchPos = 0
	}
}

// clearSearch drops any active search state.
func (m *model) clearSearch() {
	m.searchTerm = ""
	m.searchMatches = nil
	m.searchPos = 0
}

// searchNote returns the status-bar indicator for an active search, or "".
func (m *model) searchNote() string {
	if m.searchTerm == "" {
		return ""
	}
	if len(m.searchMatches) == 0 {
		return "search: " + m.searchTerm + " (0)"
	}
	// 1-based position, oldest..newest as shown.
	return fmt.Sprintf("search: %s (%d/%d)", m.searchTerm, m.searchPos+1, len(m.searchMatches))
}

// scrollToLine scrolls the buffer's viewport so the logical line at idx sits near
// the top, leaving a couple of rows of context above it. It accounts for soft
// wrapping and the read-marker divider so the offset lands on the right line.
func scrollToLine(b *Buffer, idx int) {
	if !b.vpReady {
		return
	}
	off := b.wrappedOffsetOf(idx) - 2
	if off < 0 {
		off = 0
	}
	b.vp.SetYOffset(off)
}

// wrappedOffsetOf returns the viewport row offset at which logical line idx
// begins, summing the soft-wrapped heights of the preceding lines plus the
// read-marker divider row when it falls above idx.
func (b *Buffer) wrappedOffsetOf(idx int) int {
	w := b.contentWidth
	if w <= 0 {
		return idx
	}
	markerAt := -1
	if b.readMarker > 0 && b.readMarker < len(b.lines) {
		markerAt = b.readMarker
	}
	off := 0
	wrap := lipgloss.NewStyle().Width(w)
	for i := 0; i < idx && i < len(b.lines); i++ {
		if i == markerAt {
			off++ // the inserted "new messages" divider is one row
		}
		off += lipgloss.Height(wrap.Render(b.lines[i]))
	}
	return off
}
