package tui

import (
	"fmt"
	"testing"

	"github.com/exec/lurk/client"
)

// TestChannelListBatchDeliversAllRows is the regression test for the /list
// event-drop bug — "lost N events (UI fell behind)" plus an incomplete channel
// list on a large server. A big LIST reply delivered as one ircBatchMsg through
// Update must populate every row. The old one-event-per-render bridge overran
// the lossy, drop-oldest Events buffer and silently dropped rows; batch-draining
// applies the whole burst in a single Update so the consumer stays ahead.
func TestChannelListBatchDeliversAllRows(t *testing.T) {
	m := sizedModel(t)
	m = openChannelList(m)
	net := m.activeNet()

	const n = 3000 // far beyond eventBufferSize: the old bridge would drop rows
	evs := make([]client.Event, 0, n+1)
	for i := 0; i < n; i++ {
		evs = append(evs, evt(t, fmt.Sprintf(":srv 322 me #chan%d %d :topic %d", i, i%500, i)))
	}
	evs = append(evs, evt(t, ":srv 323 me :End of /LIST"))

	tm, _ := m.Update(ircBatchMsg{net: net, evs: evs})
	m = tm.(model)

	if m.chanListLoading {
		t.Fatal("still loading after a batch that included RPL_LISTEND")
	}
	if got := len(m.chanList.Items()); got != n {
		t.Fatalf("channel list has %d items, want %d — events were dropped", got, n)
	}
}

// TestWaitForIRCBatchesBufferedEvents verifies the bridge drains everything
// already buffered into a single ircBatchMsg in one pass, rather than returning
// one event per Cmd round-trip (the throughput fix that keeps the UI ahead of a
// flood).
func TestWaitForIRCBatchesBufferedEvents(t *testing.T) {
	sub := make(chan client.Event, 32)
	net := &network{name: "t", sub: sub}

	const n = 8
	for i := 0; i < n; i++ {
		sub <- evt(t, fmt.Sprintf(":srv 322 me #c%d 1 :x", i))
	}

	msg := waitForIRC(net)()
	b, ok := msg.(ircBatchMsg)
	if !ok {
		t.Fatalf("waitForIRC returned %T, want ircBatchMsg", msg)
	}
	if len(b.evs) != n {
		t.Errorf("drained %d events into the batch, want %d", len(b.evs), n)
	}
}

// TestWaitForIRCBatchCapBound verifies a single drain never exceeds maxEventBatch
// even when far more events are already buffered, so one Update cycle can't be
// stalled by an unbounded flood.
func TestWaitForIRCBatchCapBound(t *testing.T) {
	sub := make(chan client.Event, maxEventBatch*2)
	net := &network{name: "t", sub: sub}
	for i := 0; i < maxEventBatch+50; i++ {
		sub <- evt(t, ":srv 322 me #c 1 :x")
	}
	b := waitForIRC(net)().(ircBatchMsg)
	if len(b.evs) != maxEventBatch {
		t.Errorf("batch drained %d events, want it capped at %d", len(b.evs), maxEventBatch)
	}
}
