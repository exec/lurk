package backlog

import (
	"fmt"
	"testing"
	"time"
)

func TestLatestAfterSelectsNewestMatches(t *testing.T) {
	for _, ringSize := range []int{32, 2} {
		t.Run(fmt.Sprintf("ring_size_%d", ringSize), func(t *testing.T) {
			s := newTestStore(t, WithRingSize(ringSize))
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			var ids []string
			for i, second := range []int{0, 1, 1, 2, 3, 4} {
				id, stored := s.Ingest(1, makeEventWithTime("PRIVMSG", "bob!u@h",
					[]string{"#c", fmt.Sprintf("message-%d", i)}, base.Add(time.Duration(second)*time.Second)))
				if !stored {
					t.Fatal("fixture message was not stored")
				}
				ids = append(ids, id)
			}
			for _, tc := range []struct {
				name  string
				ref   Ref
				limit int
				want  []int
			}{
				{"old_msgid", Ref{IsMsgID: true, MsgID: ids[0]}, 2, []int{4, 5}},
				{"recent_msgid", Ref{IsMsgID: true, MsgID: ids[4]}, 2, []int{5}},
				{"last_msgid", Ref{IsMsgID: true, MsgID: ids[5]}, 2, nil},
				{"missing_msgid", Ref{IsMsgID: true, MsgID: "missing"}, 2, nil},
				{"time_before_first", Ref{IsTime: true, Time: base.Add(-time.Second)}, 2, []int{4, 5}},
				{"time_gap", Ref{IsTime: true, Time: base.Add(500 * time.Millisecond)}, 2, []int{4, 5}},
				{"time_ties", Ref{IsTime: true, Time: base.Add(time.Second)}, 10, []int{3, 4, 5}},
				{"time_beyond_last", Ref{IsTime: true, Time: base.Add(5 * time.Second)}, 2, nil},
				{"large_limit", Ref{IsMsgID: true, MsgID: ids[0]}, 10, []int{1, 2, 3, 4, 5}},
				{"zero_limit", Ref{IsMsgID: true, MsgID: ids[0]}, 0, nil},
				{"negative_limit", Ref{IsMsgID: true, MsgID: ids[0]}, -1, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := s.LatestAfter(1, "#c", tc.ref, tc.limit)
					if len(got) != len(tc.want) {
						t.Fatalf("got %d messages, want %v", len(got), tc.want)
					}
					for i, index := range tc.want {
						if got[i].MsgID != ids[index] {
							t.Errorf("message %d = %q, want message-%d", i, got[i].Params[1], index)
						}
					}
				})
			}
		})
	}
}
