package server

import (
	"fmt"
	"testing"
	"time"
)

func TestCHLatestReferenceSelectsNewestWindow(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	store, entries := makeTestStore(t, 1, "#c", []string{"m0", "m1", "m2", "m3", "m4", "m5"}, base)
	c := pipeServerCHBound(t, store, 1)
	doRegisterWithCaps(t, c, "draft/chathistory", "batch", "server-time", "message-tags")
	for _, tc := range []struct {
		name, sub, ref string
		limit          int
		want           []string
	}{
		{"msgid_window", "LATEST", "msgid=" + entries[0].MsgID, 2, []string{"m4", "m5"}},
		{"msgid_tail", "LATEST", "msgid=" + entries[4].MsgID, 2, []string{"m5"}},
		{"msgid_at_end", "LATEST", "msgid=" + entries[5].MsgID, 2, nil},
		{"unknown_msgid", "LATEST", "msgid=unknown", 2, nil},
		{"timestamp_window", "LATEST", "timestamp=" + base.Format(time.RFC3339Nano), 2, []string{"m4", "m5"}},
		{"timestamp_gap", "LATEST", "timestamp=" + base.Add(500*time.Millisecond).Format(time.RFC3339Nano), 2, []string{"m4", "m5"}},
		{"timestamp_at_end", "LATEST", "timestamp=" + base.Add(5*time.Second).Format(time.RFC3339Nano), 2, nil},
		{"large_limit", "LATEST", "msgid=" + entries[3].MsgID, 10, []string{"m4", "m5"}},
		{"star", "LATEST", "*", 2, []string{"m4", "m5"}},
		{"after_still_selects_oldest", "AFTER", "msgid=" + entries[0].MsgID, 2, []string{"m1", "m2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sendLine(t, c, fmt.Sprintf("CHATHISTORY %s #c %s %d", tc.sub, tc.ref, tc.limit))
			batch := recvBatch(t, c)
			if batch.btype != "chathistory" || batch.target != "#c" || len(batch.entries) != len(tc.want) {
				t.Fatalf("unexpected history window: %+v, want %v", batch, tc.want)
			}
			for i, want := range tc.want {
				assertBatchEntry(t, batch.entries[i], batch.ref)
				if got := batch.entries[i].Param(1); got != want {
					t.Errorf("message %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}
