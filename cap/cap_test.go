package cap

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"lurk/irc"
)

// capMsg builds a CAP *irc.Message from its parameters, mirroring what the
// parser produces. The target nick is fixed to "*" (pre-registration) or the
// caller-supplied value; tests don't care about it.
func capMsg(params ...string) *irc.Message {
	return &irc.Message{Command: "CAP", Params: params}
}

// feed runs a sequence of messages through a Negotiator and returns every line
// it emitted, flattened in order.
func feed(n *Negotiator, msgs ...*irc.Message) ([]string, error) {
	var out []string
	for _, m := range msgs {
		lines, err := n.Receive(m)
		if err != nil {
			return out, err
		}
		out = append(out, lines...)
	}
	return out, nil
}

func TestStart(t *testing.T) {
	n := NewNegotiator([]string{"sasl"})
	if got := n.Start(); got != "CAP LS 302" {
		t.Fatalf("Start() = %q, want %q", got, "CAP LS 302")
	}
	if n.State() != StateListing {
		t.Fatalf("State after Start = %v, want Listing", n.State())
	}
}

// TestFullExchange drives complete CAP exchanges end to end, asserting on the
// emitted lines and terminal state.
func TestFullExchange(t *testing.T) {
	tests := []struct {
		name      string
		wanted    []string
		msgs      []*irc.Message
		wantLines []string
		wantState State
		wantEnab  []string
		wantMechs []string
		needSASL  bool
	}{
		{
			name:   "simple ls req ack end",
			wanted: []string{"server-time", "multi-prefix"},
			msgs: []*irc.Message{
				capMsg("*", "LS", "multi-prefix server-time away-notify"),
				capMsg("*", "ACK", "server-time multi-prefix"),
			},
			// REQ preserves wanted order.
			wantLines: []string{"CAP REQ :server-time multi-prefix", "CAP END"},
			wantState: StateDone,
			wantEnab:  []string{"multi-prefix", "server-time"},
		},
		{
			name:   "nothing wanted is advertised",
			wanted: []string{"chghost"},
			msgs: []*irc.Message{
				capMsg("*", "LS", "server-time multi-prefix"),
			},
			wantLines: []string{"CAP END"},
			wantState: StateDone,
			wantEnab:  nil,
		},
		{
			name:   "multiline ls",
			wanted: []string{"sasl", "server-time", "batch"},
			msgs: []*irc.Message{
				capMsg("*", "LS", "*", "server-time multi-prefix"),
				capMsg("*", "LS", "*", "batch away-notify"),
				capMsg("*", "LS", "sasl=PLAIN,EXTERNAL chghost"),
				capMsg("*", "ACK", "sasl server-time batch"),
			},
			wantLines: []string{"CAP REQ :sasl server-time batch"},
			wantState: StateWaitingSASL,
			wantEnab:  []string{"batch", "sasl", "server-time"},
			wantMechs: []string{"PLAIN", "EXTERNAL"},
			needSASL:  true,
		},
		{
			name:   "nak then end",
			wanted: []string{"server-time", "echo-message"},
			msgs: []*irc.Message{
				capMsg("*", "LS", "server-time echo-message"),
				capMsg("*", "NAK", "server-time echo-message"),
			},
			wantLines: []string{"CAP REQ :server-time echo-message", "CAP END"},
			wantState: StateDone,
			wantEnab:  nil,
		},
		{
			name:   "partial nak partial ack across two verdicts",
			wanted: []string{"server-time", "echo-message"},
			msgs: []*irc.Message{
				// Server may split a single REQ's response oddly; here we model
				// two separate verdicts whose counts sum to the request.
				capMsg("*", "LS", "server-time echo-message"),
				capMsg("*", "ACK", "server-time"),
				capMsg("*", "NAK", "echo-message"),
			},
			wantLines: []string{"CAP REQ :server-time echo-message", "CAP END"},
			wantState: StateDone,
			wantEnab:  []string{"server-time"},
		},
		{
			name:   "sasl without value (3.1 style) still needs sasl",
			wanted: []string{"sasl"},
			msgs: []*irc.Message{
				capMsg("*", "LS", "sasl"),
				capMsg("*", "ACK", "sasl"),
			},
			wantLines: []string{"CAP REQ :sasl"},
			wantState: StateWaitingSASL,
			wantEnab:  []string{"sasl"},
			wantMechs: nil,
			needSASL:  true,
		},
		{
			name:   "sasl nak does not gate on sasl",
			wanted: []string{"sasl", "server-time"},
			msgs: []*irc.Message{
				capMsg("*", "LS", "sasl=PLAIN server-time"),
				capMsg("*", "ACK", "server-time"),
				capMsg("*", "NAK", "sasl"),
			},
			wantLines: []string{"CAP REQ :sasl server-time", "CAP END"},
			wantState: StateDone,
			wantEnab:  []string{"server-time"},
			// sasl was advertised with a value, so the parsed mechs remain
			// visible even though the cap itself was NAKed.
			wantMechs: []string{"PLAIN"},
			needSASL:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := NewNegotiator(tt.wanted)
			n.Start()
			lines, err := feed(n, tt.msgs...)
			if err != nil {
				t.Fatalf("Receive error: %v", err)
			}
			if !reflect.DeepEqual(lines, tt.wantLines) {
				t.Errorf("lines = %#v, want %#v", lines, tt.wantLines)
			}
			if n.State() != tt.wantState {
				t.Errorf("State = %v, want %v", n.State(), tt.wantState)
			}
			if got := n.Enabled(); !equalStrings(got, tt.wantEnab) {
				t.Errorf("Enabled = %#v, want %#v", got, tt.wantEnab)
			}
			if got := n.SASLMechs(); !reflect.DeepEqual(got, tt.wantMechs) {
				t.Errorf("SASLMechs = %#v, want %#v", got, tt.wantMechs)
			}
			if got := n.NeedSASL(); got != tt.needSASL {
				t.Errorf("NeedSASL = %v, want %v", got, tt.needSASL)
			}
		})
	}
}

// TestSASLComplete verifies the SASL hand-back emits CAP END exactly once.
func TestSASLComplete(t *testing.T) {
	n := NewNegotiator([]string{"sasl"})
	n.Start()
	if _, err := feed(n,
		capMsg("*", "LS", "sasl=PLAIN"),
		capMsg("*", "ACK", "sasl"),
	); err != nil {
		t.Fatal(err)
	}
	if !n.NeedSASL() {
		t.Fatal("expected NeedSASL true before SASLComplete")
	}
	lines := n.SASLComplete()
	if !reflect.DeepEqual(lines, []string{"CAP END"}) {
		t.Fatalf("SASLComplete lines = %#v, want [CAP END]", lines)
	}
	if n.State() != StateDone {
		t.Fatalf("State = %v, want Done", n.State())
	}
	if n.NeedSASL() {
		t.Error("NeedSASL still true after SASLComplete")
	}
	// Second call is a no-op.
	if got := n.SASLComplete(); got != nil {
		t.Errorf("second SASLComplete = %#v, want nil", got)
	}
}

// TestReqLineSplitting checks the 512-byte budget is honored: many caps split
// across multiple CAP REQ lines, each within budget, with no cap dropped.
func TestReqLineSplitting(t *testing.T) {
	// Build a large advertisement of distinct caps, all wanted. 60 caps of
	// ~16 bytes each (plus a separating space) is ~1KB, forcing at least two
	// CAP REQ lines under the 512-byte budget.
	var caps []string
	for i := 0; i < 60; i++ {
		caps = append(caps, "vendor/capability"+strconv.Itoa(i))
	}
	n := NewNegotiator(caps)
	n.Start()
	lines, err := feed(n, capMsg("*", "LS", strings.Join(caps, " ")))
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatalf("expected splitting into multiple REQ lines, got %d: %#v", len(lines), lines)
	}
	// Every line within budget and well-formed; collect all requested caps.
	var got []string
	for _, l := range lines {
		if len(l)+crlfLen > lineBudget {
			t.Errorf("line exceeds budget (%d): %q", len(l)+crlfLen, l)
		}
		if !strings.HasPrefix(l, reqPrefix) {
			t.Errorf("line missing %q prefix: %q", reqPrefix, l)
			continue
		}
		got = append(got, strings.Fields(strings.TrimPrefix(l, reqPrefix))...)
	}
	if !equalStrings(sortedCopy(got), sortedCopy(caps)) {
		t.Errorf("requested caps mismatch: got %d caps, want %d", len(got), len(caps))
	}
}

// TestCapNew checks post-registration CAP NEW triggers a REQ for wanted caps
// only, without emitting CAP END.
func TestCapNew(t *testing.T) {
	n := NewNegotiator([]string{"sasl", "chghost"})
	n.Start()
	// Reach Done with nothing offered.
	if _, err := feed(n, capMsg("*", "LS", "server-time")); err != nil {
		t.Fatal(err)
	}
	if n.State() != StateDone {
		t.Fatalf("setup: State = %v, want Done", n.State())
	}
	// Server later offers chghost (wanted) and away-notify (not wanted).
	lines, err := feed(n, capMsg("*", "NEW", "chghost away-notify"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lines, []string{"CAP REQ :chghost"}) {
		t.Fatalf("CAP NEW lines = %#v, want [CAP REQ :chghost]", lines)
	}
	// ACK enables it; still no CAP END.
	lines, err = feed(n, capMsg("*", "ACK", "chghost"))
	if err != nil {
		t.Fatal(err)
	}
	if lines != nil {
		t.Errorf("post-NEW ACK emitted lines %#v, want none", lines)
	}
	if !n.IsEnabled("chghost") {
		t.Error("chghost not enabled after CAP NEW/ACK")
	}
}

// TestCapDel checks CAP DEL drops caps from Available and Enabled.
func TestCapDel(t *testing.T) {
	n := NewNegotiator([]string{"away-notify"})
	n.Start()
	if _, err := feed(n,
		capMsg("*", "LS", "away-notify"),
		capMsg("*", "ACK", "away-notify"),
	); err != nil {
		t.Fatal(err)
	}
	if !n.IsEnabled("away-notify") {
		t.Fatal("setup: away-notify should be enabled")
	}
	if _, err := feed(n, capMsg("*", "DEL", "away-notify")); err != nil {
		t.Fatal(err)
	}
	if n.IsEnabled("away-notify") {
		t.Error("away-notify still enabled after CAP DEL")
	}
	if _, ok := n.Available()["away-notify"]; ok {
		t.Error("away-notify still available after CAP DEL")
	}
}

// TestDisableAck verifies a "-cap" ACK (CAP REQ :-cap echoed back) disables it.
func TestDisableAck(t *testing.T) {
	n := NewNegotiator([]string{"echo-message"})
	n.Start()
	if _, err := feed(n,
		capMsg("*", "LS", "echo-message"),
		capMsg("*", "ACK", "echo-message"),
	); err != nil {
		t.Fatal(err)
	}
	if !n.IsEnabled("echo-message") {
		t.Fatal("setup failed")
	}
	// A later ACK disabling it.
	if _, err := feed(n, capMsg("*", "ACK", "-echo-message")); err != nil {
		t.Fatal(err)
	}
	if n.IsEnabled("echo-message") {
		t.Error("echo-message still enabled after -echo-message ACK")
	}
}

// TestNonCapIgnored confirms non-CAP and nil messages are ignored.
func TestNonCapIgnored(t *testing.T) {
	n := NewNegotiator([]string{"sasl"})
	n.Start()
	for _, m := range []*irc.Message{
		nil,
		{Command: "PING", Params: []string{"token"}},
		{Command: "001", Params: []string{"nick", "welcome"}},
	} {
		lines, err := n.Receive(m)
		if err != nil || lines != nil {
			t.Errorf("Receive(%v) = (%#v, %v), want (nil, nil)", m, lines, err)
		}
	}
}

// TestCapList verifies CAP LIST rebuilds the enabled set from the server's view.
func TestCapList(t *testing.T) {
	n := NewNegotiator(nil)
	n.Start()
	feed(n, capMsg("*", "LS", "server-time multi-prefix"))
	// Server reports two caps enabled via a multiline LIST.
	if _, err := feed(n,
		capMsg("*", "LIST", "*", "server-time"),
		capMsg("*", "LIST", "multi-prefix"),
	); err != nil {
		t.Fatal(err)
	}
	if got := n.Enabled(); !equalStrings(got, []string{"multi-prefix", "server-time"}) {
		t.Errorf("Enabled after LIST = %#v", got)
	}
}

// TestStarTokenDisambiguation guards the one parsing pitfall that bites client
// CAP implementations (flagged cross-checking Ergo's irc/handlers.go
// sendCapLines): on the wire an unregistered client's CAP LS reply uses "*" as
// BOTH the target nick (Param 0) and, separately, the multiline-continuation
// marker (the param just before the trailing cap list). The two must never be
// conflated. Ergo emits "CAP <nick> LS * :<caps>" for continuation and
// "CAP <nick> LS :<caps>" for the final line; <nick> is "*" pre-registration.
func TestStarTokenDisambiguation(t *testing.T) {
	tests := []struct {
		name      string
		msgs      []*irc.Message
		wantAvail []string // sorted advertised cap names
		wantMore  bool     // whether negotiation is still listing after these msgs
	}{
		{
			name: "unregistered target star, multiline continuation star",
			msgs: []*irc.Message{
				// Param(0)="*" is the target; Param(2)="*" is "more coming".
				capMsg("*", "LS", "*", "server-time multi-prefix"),
				capMsg("*", "LS", "sasl=PLAIN"),
			},
			wantAvail: []string{"multi-prefix", "sasl", "server-time"},
		},
		{
			name: "registered nick target, continuation star",
			msgs: []*irc.Message{
				capMsg("alice", "LS", "*", "server-time"),
				capMsg("alice", "LS", "multi-prefix"),
			},
			wantAvail: []string{"multi-prefix", "server-time"},
		},
		{
			name: "single-line LS to unregistered target (no continuation)",
			msgs: []*irc.Message{
				// Only param after sub is the cap list — the lone "*" target must
				// NOT be read as a continuation marker.
				capMsg("*", "LS", "server-time sasl=EXTERNAL"),
			},
			wantAvail: []string{"sasl", "server-time"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := NewNegotiator(nil) // want nothing, so no REQ noise
			n.Start()
			if _, err := feed(n, tt.msgs...); err != nil {
				t.Fatal(err)
			}
			var got []string
			for k := range n.Available() {
				got = append(got, k)
			}
			if !equalStrings(sortedCopy(got), tt.wantAvail) {
				t.Errorf("Available names = %#v, want %#v", sortedCopy(got), tt.wantAvail)
			}
		})
	}
}

// TestValueWithInnerEquals confirms a cap value containing '=' (e.g. the STS
// "sts=duration=...,port=..." form Ergo emits) keeps everything after the FIRST
// '=' as the value, and that comma-separated mech lists parse correctly.
func TestValueWithInnerEquals(t *testing.T) {
	n := NewNegotiator(nil)
	n.Start()
	if _, err := feed(n, capMsg("*", "LS",
		"sts=duration=2419200,port=6697 sasl=PLAIN,EXTERNAL")); err != nil {
		t.Fatal(err)
	}
	av := n.Available()
	if got, want := av["sts"], "duration=2419200,port=6697"; got != want {
		t.Errorf("sts value = %q, want %q", got, want)
	}
	if got := n.SASLMechs(); !reflect.DeepEqual(got, []string{"PLAIN", "EXTERNAL"}) {
		t.Errorf("SASLMechs = %#v, want [PLAIN EXTERNAL]", got)
	}
}

// TestDisableRequestAck mirrors Ergo's behaviour of echoing the REQ verbatim in
// the ACK: a "CAP REQ :-cap" is acknowledged as "ACK :-cap", which must disable
// the cap atomically alongside any positive grants in the same ACK.
func TestMixedAckEnableDisable(t *testing.T) {
	n := NewNegotiator([]string{"away-notify", "echo-message"})
	n.Start()
	if _, err := feed(n,
		capMsg("*", "LS", "away-notify echo-message"),
		capMsg("*", "ACK", "away-notify echo-message"),
	); err != nil {
		t.Fatal(err)
	}
	// A subsequent atomic ACK that enables nothing new but disables one.
	if _, err := feed(n, capMsg("*", "ACK", "-away-notify echo-message")); err != nil {
		t.Fatal(err)
	}
	if n.IsEnabled("away-notify") {
		t.Error("away-notify should be disabled after -away-notify in ACK")
	}
	if !n.IsEnabled("echo-message") {
		t.Error("echo-message should remain enabled")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

func sortedCopy(s []string) []string {
	c := append([]string(nil), s...)
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && c[j-1] > c[j]; j-- {
			c[j-1], c[j] = c[j], c[j-1]
		}
	}
	return c
}

// TestCapNewFloodBounded verifies that a hostile stream of distinct CAP NEW names
// after registration cannot grow the advertised set without bound.
func TestCapNewFloodBounded(t *testing.T) {
	n := NewNegotiator([]string{"sasl"})
	n.registered = true
	n.state = StateDone

	// Flood well past the cap with distinct, unwanted cap names.
	for i := 0; i < maxAvailableCaps+500; i++ {
		_, _ = n.Receive(capMsg("*", "NEW", "x-flood-"+strconv.Itoa(i)))
	}
	if got := len(n.Available()); got > maxAvailableCaps {
		t.Errorf("available grew past cap: %d > %d", got, maxAvailableCaps)
	}
}
