package backlog

import (
	"fmt"
	"testing"
	"time"
)

func TestBetweenDirectionsAndBoundaries(t *testing.T) {
	for _, ringSize := range []int{32, 2} {
		t.Run(fmt.Sprintf("ring_size_%d", ringSize), func(t *testing.T) {
			s := newTestStore(t, WithRingSize(ringSize))
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			var ids []string
			for i, second := range []int{0, 1, 1, 3, 2, 4} {
				id, stored := s.Ingest(1, makeEventWithTime("PRIVMSG", "bob!u@h",
					[]string{"#c", fmt.Sprintf("message-%d", i)}, base.Add(time.Duration(second)*time.Second)))
				if !stored {
					t.Fatal("fixture message was not stored")
				}
				ids = append(ids, id)
			}
			msg := func(i int) Ref { return Ref{IsMsgID: true, MsgID: ids[i]} }
			ts := func(ms int) Ref { return Ref{IsTime: true, Time: base.Add(time.Duration(ms) * time.Millisecond)} }
			missing := Ref{IsMsgID: true, MsgID: "missing"}
			for _, tc := range []struct {
				name     string
				from, to Ref
				limit    int
				want     []int
			}{
				{"msgid_forward_limit", msg(0), msg(5), 2, []int{1, 2}},
				{"msgid_reverse_limit", msg(5), msg(0), 2, []int{3, 4}},
				{"msgid_tied_times", msg(1), msg(4), 10, []int{2, 3}},
				{"msgid_adjacent", msg(1), msg(2), 10, nil},
				{"msgid_equal", msg(2), msg(2), 10, nil},
				{"time_forward_limit", ts(-1000), ts(5000), 2, []int{0, 1}},
				{"time_reverse_limit", ts(5000), ts(-1000), 2, []int{4, 5}},
				{"time_gaps_and_skew", ts(500), ts(2500), 10, []int{1, 2, 4}},
				{"time_reverse_gaps_and_skew", ts(2500), ts(500), 10, []int{1, 2, 4}},
				{"time_ties", ts(1000), ts(4000), 10, []int{3, 4}},
				{"time_equal", ts(1000), ts(1000), 10, nil},
				{"mixed_forward", msg(0), ts(3500), 10, []int{1, 2, 3, 4}},
				{"mixed_reverse_limit", ts(3500), msg(0), 2, []int{3, 4}},
				{"mixed_skew", ts(500), msg(4), 10, []int{1, 2, 3}},
				{"missing_from", missing, msg(4), 10, nil},
				{"missing_to", msg(0), missing, 10, nil},
				{"star_from", Ref{IsStar: true}, msg(4), 10, nil},
				{"star_to", msg(0), Ref{IsStar: true}, 10, nil},
				{"zero_limit", msg(0), msg(5), 0, nil},
				{"negative_limit", msg(0), msg(5), -1, nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := s.Between(1, "#c", tc.from, tc.to, tc.limit)
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
