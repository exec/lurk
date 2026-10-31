package client

import (
	"testing"
	"time"
)

func TestHistoryQueries(t *testing.T) {
	c, rec := newRecordClient()
	c.st.mergeISupport([]string{"CHATHISTORY=2"})
	from := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	to := from.Add(-time.Hour)
	for _, tc := range []struct {
		name, want string
		send       func() error
	}{
		{"latest", "CHATHISTORY LATEST #c * 2", func() error { return c.ChatHistoryLatest("#c", 50) }},
		{"before", "CHATHISTORY BEFORE #c msgid=old 2", func() error { return c.ChatHistoryBefore("#c", "msgid=old", 50) }},
		{"after", "CHATHISTORY AFTER bob timestamp=2026-10-28T11:00:00Z 1", func() error { return c.ChatHistoryAfter("bob", "timestamp=2026-10-28T11:00:00Z", 0) }},
		{"targets", "CHATHISTORY TARGETS timestamp=2026-10-28T12:00:00.000Z timestamp=2026-10-28T11:00:00.000Z 2", func() error { return c.ChatHistoryTargets(from, to, 50) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec.sent = nil
			if err := tc.send(); err != nil {
				t.Fatal(err)
			}
			if len(rec.sent) != 1 || rec.sent[0] != tc.want {
				t.Fatalf("sent = %q, want %q", rec.sent, tc.want)
			}
		})
	}
	c.st.mergeISupport([]string{"CHATHISTORY=0"})
	rec.sent = nil
	_ = c.ChatHistoryLatest("#c", 50)
	if rec.sent[0] != "CHATHISTORY LATEST #c * 50" {
		t.Fatal("CHATHISTORY=0 should not clamp the limit")
	}
}
