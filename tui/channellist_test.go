package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"charm.land/bubbles/v2/list"
)

// sizedModel returns a test model with real dimensions so layout/render and the
// modal sizing have something to work with.
func sizedModel(t *testing.T) model {
	t.Helper()
	m := newTestModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return tm.(model)
}

// feedList drives a full /list cycle into m: open the modal, stream some
// RPL_LIST rows, then RPL_LISTEND. It returns the resulting model.
func feedList(t *testing.T, m model, rows ...string) model {
	t.Helper()
	m = openChannelList(m)
	for _, r := range rows {
		m = routeEvent(m, evt(t, r))
	}
	m = routeEvent(m, evt(t, ":srv 323 me :End of /LIST"))
	return m
}

// TestChannelListOpensAndLoads verifies the modal opens in a loading state and
// finalizes into a sorted, populated list on RPL_LISTEND.
func TestChannelListOpensAndLoads(t *testing.T) {
	m := sizedModel(t)

	m = openChannelList(m)
	if !m.chanListOpen || !m.chanListLoading {
		t.Fatalf("after open: open=%v loading=%v, want both true", m.chanListOpen, m.chanListLoading)
	}

	m = routeEvent(m, evt(t, ":srv 321 me Channel :Users Name"))
	m = routeEvent(m, evt(t, ":srv 322 me #go 120 :Gophers"))
	m = routeEvent(m, evt(t, ":srv 322 me #lurk 5 :Lurk dev"))
	if len(m.chanListAccum) != 2 {
		t.Fatalf("accumulated %d rows, want 2", len(m.chanListAccum))
	}
	if m.buffers[0].lines != nil && len(m.buffers[0].lines) != 0 {
		t.Errorf("LIST replies leaked into the server buffer: %v", m.buffers[0].lines)
	}

	m = routeEvent(m, evt(t, ":srv 323 me :End of /LIST"))
	if m.chanListLoading {
		t.Error("still loading after RPL_LISTEND")
	}
	items := m.chanList.Items()
	if len(items) != 2 {
		t.Fatalf("list has %d items, want 2", len(items))
	}
	// Sorted most-populated first.
	if got := items[0].(channelItem).name; got != "#go" {
		t.Errorf("first item = %q, want #go (most users)", got)
	}
}

// TestChannelListEnterJoins verifies Enter on a row joins that channel, opens its
// buffer, and closes the modal.
func TestChannelListEnterJoins(t *testing.T) {
	m := feedList(t, sizedModel(t),
		":srv 322 me #go 120 :Gophers",
		":srv 322 me #lurk 5 :Lurk dev",
	)
	if !m.chanListOpen {
		t.Fatal("modal should be open after load")
	}

	tm, _ := m.handleChannelListKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = tm.(model)

	if m.chanListOpen {
		t.Error("Enter did not close the modal")
	}
	// The selected (first, most-populated) channel buffer should now exist.
	if m.buffer("#go") == nil {
		t.Errorf("Enter did not open the #go buffer; buffers=%v", bufferTitles(m))
	}
}

// TestChannelListEscCloses verifies Esc dismisses the modal when no filter is
// active.
func TestChannelListEscCloses(t *testing.T) {
	m := feedList(t, sizedModel(t), ":srv 322 me #go 120 :Gophers")
	tm, _ := m.handleChannelListKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = tm.(model)
	if m.chanListOpen {
		t.Error("Esc did not close the modal")
	}
}

// TestChannelListFilter verifies the "/" filter narrows the visible items.
func TestChannelListFilter(t *testing.T) {
	m := feedList(t, sizedModel(t),
		":srv 322 me #go 120 :Gophers",
		":srv 322 me #python 90 :Pythonistas",
		":srv 322 me #lurk 5 :Lurk dev",
	)
	// Start filtering and type "py".
	m = pressList(t, m, tea.KeyPressMsg{Code: '/', Text: "/"})
	if m.chanList.FilterState() != list.Filtering {
		t.Fatalf("'/' did not start filtering (state=%v)", m.chanList.FilterState())
	}
	m = pressList(t, m, tea.KeyPressMsg{Code: 'p', Text: "p"})
	m = pressList(t, m, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if got := m.chanList.FilterValue(); got != "py" {
		t.Errorf("filter value = %q, want py", got)
	}
	// Esc clears the filter rather than closing the modal.
	m = pressList(t, m, tea.KeyPressMsg{Code: tea.KeyEsc})
	if !m.chanListOpen {
		t.Error("Esc with an active filter should clear it, not close the modal")
	}
}

// TestChannelListOverlayRenders confirms the modal frame composites over the
// chat view (the rounded border shows up in the rendered output).
func TestChannelListOverlayRenders(t *testing.T) {
	m := feedList(t, sizedModel(t), ":srv 322 me #go 120 :Gophers")
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "Channels") {
		t.Errorf("rendered overlay missing the modal title:\n%s", out)
	}
}

// TestChannelListScopedToRequestingNetwork verifies the /list modal only
// accepts LIST replies from the network it was opened for: a second network's
// (or a hostile server's unsolicited) RPL_LIST rows must not populate a
// directory whose Enter joins via the requesting network's client, and its
// RPL_LISTEND must not finalize the modal early.
func TestChannelListScopedToRequestingNetwork(t *testing.T) {
	m, net0, netB := twoNetModel(t)

	m = openChannelList(m) // active buffer is net0's server buffer
	if m.chanListNet != net0 {
		t.Fatalf("chanListNet = %v, want net0", m.chanListNet)
	}

	// netB volunteers rows + an end marker while net0's request is in flight.
	netBServer := m.serverBuffer(netB)
	before := len(netBServer.lines)
	m = routeEventOn(m, netB, evt(t, ":srv 322 me #evil 666 :join me"))
	m = routeEventOn(m, netB, evt(t, ":srv 323 me :End of /LIST"))
	if len(m.chanListAccum) != 0 {
		t.Errorf("foreign RPL_LIST rows accumulated into the modal: %v", m.chanListAccum)
	}
	if !m.chanListLoading {
		t.Error("foreign RPL_LISTEND finalized the modal")
	}
	// The foreign replies fall back to their own network's buffer, not the void.
	if len(netBServer.lines) <= before {
		t.Error("netB's LIST replies were dropped instead of rendered in its buffer")
	}

	// net0's own replies still drive the modal to completion.
	m = routeEventOn(m, net0, evt(t, ":srv 322 me #go 120 :Gophers"))
	m = routeEventOn(m, net0, evt(t, ":srv 323 me :End of /LIST"))
	if m.chanListLoading {
		t.Error("still loading after the requesting network's RPL_LISTEND")
	}
	items := m.chanList.Items()
	if len(items) != 1 || items[0].(channelItem).name != "#go" {
		t.Errorf("modal items = %v, want just #go", items)
	}
}

// pressList feeds one key to the open modal via handleChannelListKey.
func pressList(t *testing.T, m model, msg tea.KeyPressMsg) model {
	t.Helper()
	tm, _ := m.handleChannelListKey(msg)
	return tm.(model)
}

// bufferTitles lists open buffer titles for diagnostics.
func bufferTitles(m model) []string {
	var out []string
	for _, b := range m.buffers {
		out = append(out, b.Title)
	}
	return out
}
