package tui

import (
	"strings"
	"testing"

	"github.com/exec/lurk/client"
)

func twoNetModel(t *testing.T) (model, *network, *network) {
	t.Helper()
	m := newModel(client.New(client.Config{Nick: "me"}), nil)
	net0 := m.networks[0]
	netB := m.addNetwork("oftc", client.New(client.Config{Nick: "me"}))
	m.width, m.height, m.ready = 80, 24, true
	m = layout(m)
	return m, net0, netB
}

func bufText(b *Buffer) string { return stripANSI(strings.Join(b.lines, "\n")) }

// TestMultiNetworkRoutingIsolated verifies events route into their own network's
// buffers, so same-named channels on two networks don't cross-contaminate.
func TestMultiNetworkRoutingIsolated(t *testing.T) {
	m, net0, netB := twoNetModel(t)

	m = routeEventOn(m, net0, evt(t, ":a!a@h PRIVMSG #go :from net0"))
	m = routeEventOn(m, netB, evt(t, ":b!b@h PRIVMSG #go :from netB"))

	b0 := m.netBuffer(net0, "#go")
	bB := m.netBuffer(netB, "#go")
	if b0 == nil || bB == nil {
		t.Fatal("expected a #go buffer on each network")
	}
	if b0 == bB {
		t.Fatal("the two #go buffers should be distinct objects")
	}
	if !strings.Contains(bufText(b0), "from net0") || strings.Contains(bufText(b0), "from netB") {
		t.Errorf("net0 #go content wrong / leaked:\n%s", bufText(b0))
	}
	if !strings.Contains(bufText(bB), "from netB") {
		t.Errorf("netB #go missing its message:\n%s", bufText(bB))
	}
}

// TestSidebarGroupsByNetwork verifies the sidebar shows network headers when more
// than one network is connected.
func TestSidebarGroupsByNetwork(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	m.ensureBufferIn(net0, "#go", BufferChannel)
	m.ensureBufferIn(netB, "#tor", BufferChannel)

	out := stripANSI(renderSidebar(m, sidebarWidth, 20))
	for _, want := range []string{"oftc", "#go", "#tor"} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar missing %q:\n%s", want, out)
		}
	}
}

// TestSingleNetworkNoHeader verifies a lone network renders exactly one row per
// buffer (no network header), preserving the single-network sidebar.
func TestSingleNetworkNoHeader(t *testing.T) {
	m := newTestModel()
	m.ensureBuffer("#go", BufferChannel)
	m.ensureBuffer("#rust", BufferChannel)
	out := stripANSI(renderSidebar(m, sidebarWidth, 20))
	// Count non-blank rows: 3 buffers, no header rows.
	rows := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) != "" {
			rows++
		}
	}
	if rows != 3 {
		t.Errorf("single-network sidebar has %d non-blank rows, want 3 (no headers):\n%s", rows, out)
	}
}

// TestConnectedMsgAddsNetwork verifies a successful /connect folds in a network
// and focuses its server buffer.
func TestConnectedMsgAddsNetwork(t *testing.T) {
	m, _, _ := twoNetModel(t)
	before := len(m.networks)

	tm, _ := m.Update(connectedMsg{name: "libera", cli: client.New(client.Config{Nick: "me"})})
	m = tm.(model)

	if len(m.networks) != before+1 {
		t.Fatalf("networks = %d, want %d", len(m.networks), before+1)
	}
	if m.activeBuffer().Title != "libera" {
		t.Errorf("active buffer = %q, want the new network's server buffer 'libera'", m.activeBuffer().Title)
	}
}

// TestClosedNetworkDoesNotQuitWhenOthersRemain verifies one network's stream
// closing drops it without quitting.
func TestClosedNetworkDoesNotQuitWhenOthersRemain(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	tm, _ := m.Update(ircClosedMsg{net: netB})
	m = tm.(model)
	if m.quitting {
		t.Error("should not quit while net0 remains")
	}
	if len(m.networks) != 1 || m.networks[0] != net0 {
		t.Errorf("netB not removed: %v", m.networks)
	}
}

// TestSecondaryServerBufferCannotBeClosed verifies closeBuffer refuses every
// network's server buffer, not just index 0. A second network's server buffer
// sits at index > 0; closing it would leave the network connected with no
// buffer for serverBuffer(net) to return, and the next routed standard reply
// would dereference nil.
func TestSecondaryServerBufferCannotBeClosed(t *testing.T) {
	m, _, netB := twoNetModel(t)
	i := m.bufferIndexIn(netB, "oftc")
	if i <= 0 {
		t.Fatalf("netB server buffer index = %d, want > 0", i)
	}
	before := len(m.buffers)
	m.closeBuffer(i)
	if len(m.buffers) != before {
		t.Errorf("closeBuffer removed a server buffer (buffers %d -> %d)", before, len(m.buffers))
	}
	if m.serverBuffer(netB) == nil {
		t.Error("serverBuffer(netB) = nil after attempted close; routed replies would panic")
	}
}

// TestStandardReplyNilTargetDropped is the defense-in-depth half of the same
// fix: even if a network somehow ends up with no buffers while still
// connected, a routed FAIL/WARN/NOTE must be dropped, not dereference a nil
// target buffer.
func TestStandardReplyNilTargetDropped(t *testing.T) {
	m, _, netB := twoNetModel(t)
	// Simulate the broken state directly: netB stays in m.networks (hasNetwork
	// passes) but owns no buffers (serverBuffer/targetBuffer return nil).
	kept := m.buffers[:0:0]
	for _, b := range m.buffers {
		if b.net != netB {
			kept = append(kept, b)
		}
	}
	m.buffers = kept
	m.active = 0
	// Must not panic; the reply is dropped.
	m = routeEventOn(m, netB, evt(t, ":srv FAIL JOIN ACCOUNT_REQUIRED #x :nope"))
	if got := bufText(m.buffers[0]); strings.Contains(got, "ACCOUNT_REQUIRED") {
		t.Errorf("netB's reply leaked into net0's server buffer:\n%s", got)
	}
}

// TestClosedNetworkLaysOutRehomedBuffer verifies the ircClosedMsg path runs
// layout after removeNetwork: focus can be re-homed (by index clamping) onto a
// buffer that has never been displayed (vpReady false), which rendered as a
// blank message pane until a resize or buffer switch.
func TestClosedNetworkLaysOutRehomedBuffer(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	// A net0 channel with content that has never been focused (vp not sized).
	b, _ := m.ensureBufferIn(net0, "#go", BufferChannel)
	m = appendLine(m, b, evt(t, ":a!a@h PRIVMSG #go :hello there"))
	// Focus netB's server buffer (the last buffer), then let netB die.
	m.switchTo(m.bufferIndexIn(netB, "oftc"))
	m = layout(m)

	tm, _ := m.Update(ircClosedMsg{net: netB})
	m = tm.(model)

	nb := m.activeBuffer()
	if nb.Title != "#go" {
		t.Fatalf("re-homed focus on %q, want #go", nb.Title)
	}
	if !nb.vpReady {
		t.Fatal("removeNetwork left the re-homed buffer unsized: blank pane until resize")
	}
	if got := stripANSI(nb.vp.View()); !strings.Contains(got, "hello there") {
		t.Errorf("re-homed viewport missing its content:\n%s", got)
	}
}

// TestBatchClosedLaysOutRehomedBuffer covers the same re-home for the
// ircBatchMsg{closed: true} path (stream ended mid-drain).
func TestBatchClosedLaysOutRehomedBuffer(t *testing.T) {
	m, net0, netB := twoNetModel(t)
	b, _ := m.ensureBufferIn(net0, "#go", BufferChannel)
	m = appendLine(m, b, evt(t, ":a!a@h PRIVMSG #go :hello there"))
	m.switchTo(m.bufferIndexIn(netB, "oftc"))
	m = layout(m)

	tm, _ := m.Update(ircBatchMsg{
		net:    netB,
		evs:    []client.Event{evt(t, ":srv NOTICE me :bye")},
		closed: true,
	})
	m = tm.(model)

	nb := m.activeBuffer()
	if nb.Title != "#go" {
		t.Fatalf("re-homed focus on %q, want #go", nb.Title)
	}
	if !nb.vpReady {
		t.Fatal("batch-closed removeNetwork left the re-homed buffer unsized")
	}
}

// TestConnectUnavailable verifies /connect reports gracefully without a connect
// callback.
func TestConnectUnavailable(t *testing.T) {
	m := newTestModel() // connect is nil
	act, _ := runLine(m, "/connect libera")
	if act.kind != actionInfo || !strings.Contains(act.text, "isn't available") {
		t.Errorf("/connect without callback = %+v", act)
	}
}
