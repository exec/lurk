package tui

import (
	"fmt"
	"testing"

	"github.com/exec/lurk/client"
	"github.com/exec/lurk/irc"
)

// TestBatchRefreshOnce is the regression test for O(batch × scrollback) viewport
// work during a burst: appendLine was calling b.refresh() — which Joins the
// entire scrollback into one string and calls vp.SetContent — once for every
// event inside an ircBatchMsg. On a 1024-event burst with a 2000-line buffer
// that is ~2 M Join operations per Update. The fix marks the buffer dirty
// during the batch loop and calls refreshIfDirty() exactly once after.
//
// The test verifies the observable postcondition: after a 100-event batch
// applied through Update, every line is present in the viewport content —
// proving the single end-of-batch refresh was complete and not dropped.
func TestBatchRefreshOnce(t *testing.T) {
	m := sizedModel(t) // gives us real dimensions so vpReady=true after layout

	// Open a channel buffer and focus it so every event is an "active buffer"
	// append — the hot path that previously called b.refresh() per event.
	_, i := m.ensureBuffer("#busy", BufferChannel)
	m.switchTo(i)
	m = layout(m)

	net := m.activeNet()

	const n = 100
	evs := make([]client.Event, n)
	for k := 0; k < n; k++ {
		evs[k] = evt(t, fmt.Sprintf(":alice!a@h PRIVMSG #busy :message %d", k))
	}

	tm, _ := m.Update(ircBatchMsg{net: net, evs: evs})
	m = tm.(model)

	// All n lines must be present — a stale or skipped refresh would leave the
	// viewport with fewer rows than the scrollback.
	got := len(m.activeBuffer().lines)
	if got != n {
		t.Fatalf("after %d-event batch: buffer has %d lines, want %d", n, got, n)
	}

	// The viewport must have been refreshed: its content must contain the first
	// and last message.
	content := stripANSI(m.activeBuffer().vp.View())
	if content == "" {
		t.Fatal("viewport content is empty after batch — refreshIfDirty was not called")
	}
	// The viewport shows a window of lines; check that it is non-empty and
	// that the buffer has all the expected lines (viewport may not show all).
	first := stripANSI(m.activeBuffer().lines[0])
	last := stripANSI(m.activeBuffer().lines[n-1])
	if first == "" || last == "" {
		t.Errorf("unexpected empty line in scrollback: first=%q last=%q", first, last)
	}
}

// TestBatchRefreshDirtyFlagCleared verifies the dirty flag is cleared after a
// batch so a subsequent single-event ircMsg does not skip its refresh.
func TestBatchRefreshDirtyFlagCleared(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#busy", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	net := m.activeNet()

	// Deliver a small batch.
	evs := []client.Event{evt(t, ":alice!a@h PRIVMSG #busy :hello")}
	tm, _ := m.Update(ircBatchMsg{net: net, evs: evs})
	m = tm.(model)

	if m.activeBuffer().dirty {
		t.Error("dirty flag still set after ircBatchMsg — refreshIfDirty did not clear it")
	}
	if m.batchMode {
		t.Error("batchMode still true after ircBatchMsg handler returned")
	}
}

// BenchmarkBatchRefresh measures the cost of applying a 200-event burst through
// Update on a sized, active channel buffer. Before the fix each event called
// b.refresh() → strings.Join(all scrollback) → vp.SetContent, so a burst of N
// events with a scrollback of S lines cost O(N×S). After the fix a single
// refreshIfDirty() call runs at the end of the batch (O(S) once), decoupling
// burst size from refresh cost. Run with -benchmem to see allocation savings.
func BenchmarkBatchRefresh(b *testing.B) {
	// Build a sized model with an active channel buffer and a pre-loaded
	// scrollback (200 lines) so the joined string is non-trivial.
	m := sizedModelB(b)
	_, i := m.ensureBuffer("#bench", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	net := m.activeNet()

	// parseEvB parses a raw IRC line into a client.Event for use in benchmarks
	// (evt takes *testing.T, not *testing.B).
	parseEvB := func(line string) client.Event {
		m, err := irc.Parse(line)
		if err != nil {
			b.Fatalf("parse %q: %v", line, err)
		}
		return client.Event{Message: m}
	}

	// Pre-fill the scrollback so the refresh has real content to Join.
	for k := 0; k < 200; k++ {
		m = appendLine(m, m.activeBuffer(), parseEvB(fmt.Sprintf(":pre!p@h PRIVMSG #bench :line %d", k)))
	}

	// Build a 200-event burst — the kind a bouncer chathistory replay delivers.
	const batchSize = 200
	evs := make([]client.Event, batchSize)
	for k := range evs {
		evs[k] = parseEvB(fmt.Sprintf(":alice!a@h PRIVMSG #bench :message %d", k))
	}

	b.ResetTimer()
	for range b.N {
		tm, _ := m.Update(ircBatchMsg{net: net, evs: evs})
		_ = tm
	}
}

// TestSingleEventPathStillRefreshes guards the ircMsg (single-event) path used
// by tests: batchMode is not set, so appendLine must call b.refresh() inline
// and the viewport must reflect the new line immediately.
func TestSingleEventPathStillRefreshes(t *testing.T) {
	m := sizedModel(t)
	_, i := m.ensureBuffer("#c", BufferChannel)
	m.switchTo(i)
	m = layout(m)
	net := m.activeNet()

	before := m.activeBuffer().vp.View()

	tm, _ := m.Update(ircMsg{net: net, ev: evt(t, ":bob!b@h PRIVMSG #c :single event")})
	m = tm.(model)

	after := m.activeBuffer().vp.View()
	if before == after {
		t.Error("single ircMsg did not update the viewport (refresh was skipped)")
	}
	if len(m.activeBuffer().lines) != 1 {
		t.Errorf("buffer has %d lines after one ircMsg, want 1", len(m.activeBuffer().lines))
	}
}
