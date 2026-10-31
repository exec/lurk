package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/exec/lurk/irc"
)

func TestCHBetweenDirectionsAndLimits(t *testing.T) {
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	store, entries := makeTestStore(t, 1, "#c", []string{"m0", "m1", "m2", "m3", "m4", "m5"}, base)
	c := pipeServerCHBound(t, store, 1)
	doRegisterWithCaps(t, c, "draft/chathistory", "batch", "server-time", "message-tags")
	msg := func(i int) string { return "msgid=" + entries[i].MsgID }
	ts := func(i int) string { return "timestamp=" + entries[i].Time.Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		name, from, to string
		want           []string
	}{
		{"msgid_forward", msg(0), msg(5), []string{"m1", "m2"}},
		{"msgid_reverse", msg(5), msg(0), []string{"m3", "m4"}},
		{"timestamp_forward", ts(0), ts(5), []string{"m1", "m2"}},
		{"timestamp_reverse", ts(5), ts(0), []string{"m3", "m4"}},
		{"mixed_forward", msg(0), ts(5), []string{"m1", "m2"}},
		{"mixed_reverse", ts(5), msg(0), []string{"m3", "m4"}},
		{"equal_timestamps", ts(2), ts(2), nil},
		{"missing_endpoint", msg(0), "msgid=missing", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sendLine(t, c, fmt.Sprintf("CHATHISTORY BETWEEN #c %s %s 2", tc.from, tc.to))
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
	for _, refs := range [][2]string{{"*", msg(5)}, {msg(0), "*"}} {
		sendLine(t, c, fmt.Sprintf("CHATHISTORY BETWEEN #c %s %s 2", refs[0], refs[1]))
		reply := recvMsg(t, c)
		assertMsg(t, reply, irc.FAIL, "CHATHISTORY", "INVALID_PARAMS")
	}
}
