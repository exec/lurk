package client

import "testing"

// newRecordClient builds a client wired to a recordTransport so action methods
// can be asserted against the exact lines they put on the wire, without a live
// server. The recordTransport is defined in phase3_test.go.
func newRecordClient() (*Client, *recordTransport) {
	c := &Client{st: newState()}
	rec := newRecordTransport()
	c.tr = rec
	return c, rec
}

func TestActionSerialize(t *testing.T) {
	c, rec := newRecordClient()
	if err := c.Action("#c", "waves hello"); err != nil {
		t.Fatalf("Action: %v", err)
	}
	// CTCP ACTION is a PRIVMSG whose body is wrapped in \x01ACTION …\x01. The
	// body contains spaces, so it serializes as a trailing param.
	want := "PRIVMSG #c :\x01ACTION waves hello\x01"
	if len(rec.sent) != 1 || rec.sent[0] != want {
		t.Errorf("sent = %q, want [%q]", rec.sent, want)
	}
}

func TestTopicSerialize(t *testing.T) {
	c, rec := newRecordClient()

	// Query: a bare TOPIC <channel> with no trailing param.
	if err := c.RequestTopic("#c"); err != nil {
		t.Fatalf("RequestTopic: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "TOPIC #c" {
		t.Errorf("RequestTopic sent = %q, want [TOPIC #c]", rec.sent)
	}

	// Set: the topic travels as the trailing param so spaces survive.
	rec.sent = nil
	if err := c.SetTopic("#c", "new shiny topic"); err != nil {
		t.Fatalf("SetTopic: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "TOPIC #c :new shiny topic" {
		t.Errorf("SetTopic sent = %q, want [TOPIC #c :new shiny topic]", rec.sent)
	}

	// Clear: an empty topic must still emit the trailing colon (a removal), so it
	// is distinguishable from the bare query above.
	rec.sent = nil
	if err := c.SetTopic("#c", ""); err != nil {
		t.Fatalf("SetTopic clear: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "TOPIC #c :" {
		t.Errorf("SetTopic clear sent = %q, want [TOPIC #c :]", rec.sent)
	}
}

func TestAwaySerialize(t *testing.T) {
	c, rec := newRecordClient()

	// A reason marks away and travels as the trailing param.
	if err := c.Away("brb lunch"); err != nil {
		t.Fatalf("Away: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "AWAY :brb lunch" {
		t.Errorf("Away sent = %q, want [AWAY :brb lunch]", rec.sent)
	}

	// An empty reason clears the away status: a bare, parameterless AWAY.
	rec.sent = nil
	if err := c.Away(""); err != nil {
		t.Fatalf("Away clear: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "AWAY" {
		t.Errorf("Away clear sent = %q, want [AWAY]", rec.sent)
	}

	// Back is the self-documenting equivalent of Away("").
	rec.sent = nil
	if err := c.Back(); err != nil {
		t.Fatalf("Back: %v", err)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "AWAY" {
		t.Errorf("Back sent = %q, want [AWAY]", rec.sent)
	}
}
