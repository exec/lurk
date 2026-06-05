package cap

import "testing"

// TestACKRejectsUnrequestedCap verifies that a server ACK naming a cap the client
// never requested does NOT enable it: only caps in the wanted set are honored.
// The requested-and-ACKed caps still enable, the pending-request accounting stays
// correct, and registration completes (CAP END is emitted).
func TestACKRejectsUnrequestedCap(t *testing.T) {
	n := NewNegotiator([]string{"server-time", "multi-prefix"})

	if got := n.Start(); got != "CAP LS 302" {
		t.Fatalf("Start() = %q, want CAP LS 302", got)
	}

	// Server advertises the wanted caps (plus an extra it offers but we don't want).
	lines, err := feed(n, capMsg("*", "LS", "server-time multi-prefix away-notify"))
	if err != nil {
		t.Fatalf("LS: %v", err)
	}
	// One CAP REQ for the intersection (server-time multi-prefix).
	if len(lines) != 1 {
		t.Fatalf("after LS: emitted %v, want a single CAP REQ", lines)
	}

	// The server ACKs the two requested caps AND sneaks in a third the client
	// never asked for ("away-notify"), as a misbehaving/hostile server might.
	lines, err = feed(n, capMsg("*", "ACK", "server-time multi-prefix away-notify"))
	if err != nil {
		t.Fatalf("ACK: %v", err)
	}

	// Requested caps are enabled.
	for _, want := range []string{"server-time", "multi-prefix"} {
		if !n.IsEnabled(want) {
			t.Errorf("requested cap %q not enabled after ACK", want)
		}
	}
	// The un-requested cap must NOT be enabled despite being in the ACK.
	if n.IsEnabled("away-notify") {
		t.Errorf("un-requested cap away-notify was enabled by ACK (should be ignored)")
	}

	// Registration still completes: the verdict accounting counted every ACKed
	// cap, so CAP END was emitted and the Negotiator reached StateDone.
	if got := lines; len(got) != 1 || got[0] != "CAP END" {
		t.Errorf("after ACK: emitted %v, want [CAP END]", got)
	}
	if n.State() != StateDone {
		t.Errorf("State after ACK = %v, want Done", n.State())
	}
}

// TestACKUnrequestedSaslNotMarked guards the SASL path specifically: an ACK that
// names "sasl" the client never requested must not mark sasl as ACKed (which
// would wrongly drive NeedSASL / a SASL exchange).
func TestACKUnrequestedSaslNotMarked(t *testing.T) {
	n := NewNegotiator([]string{"multi-prefix"}) // sasl NOT wanted
	n.Start()
	if _, err := feed(n, capMsg("*", "LS", "multi-prefix sasl")); err != nil {
		t.Fatalf("LS: %v", err)
	}
	if _, err := feed(n, capMsg("*", "ACK", "multi-prefix sasl")); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	if n.IsEnabled("sasl") {
		t.Errorf("un-requested sasl enabled by ACK")
	}
	if n.NeedSASL() {
		t.Errorf("NeedSASL true after un-requested sasl ACK; sasl was never wanted")
	}
}
