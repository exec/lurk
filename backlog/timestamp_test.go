package backlog

import (
	"fmt"
	"testing"
	"time"
)

func TestTimestampQueryBoundaries(t *testing.T) {
	for _, ringSize := range []int{32, 2} {
		t.Run(fmt.Sprintf("ring_size_%d", ringSize), func(t *testing.T) {
			s := newTestStore(t, WithRingSize(ringSize))
			base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			// Tied times and clock skew must not change the stored message order.
			for i, second := range []int{0, 1, 1, 3, 2, 4} {
				s.Ingest(1, makeEventWithTime("PRIVMSG", "bob!u@h",
					[]string{"#c", fmt.Sprintf("message-%d", i)}, base.Add(time.Duration(second)*time.Second)))
			}
			for _, tc := range []struct {
				name   string
				after  bool
				millis int
				limit  int
				want   []int
			}{
				{name: "before_first", millis: -1000, limit: 10},
				{name: "before_exact_first", millis: 0, limit: 10},
				{name: "before_gap", millis: 500, limit: 10, want: []int{0}},
				{name: "before_tied_time", millis: 1000, limit: 10, want: []int{0}},
				{name: "before_clock_skew", millis: 2500, limit: 10, want: []int{0, 1, 2, 4}},
				{name: "before_exact_last", millis: 4000, limit: 10, want: []int{0, 1, 2, 3, 4}},
				{name: "before_beyond_last", millis: 5000, limit: 10, want: []int{0, 1, 2, 3, 4, 5}},
				{name: "before_limit", millis: 5000, limit: 2, want: []int{4, 5}},
				{name: "after_before_first", after: true, millis: -1000, limit: 10, want: []int{0, 1, 2, 3, 4, 5}},
				{name: "after_exact_first", after: true, millis: 0, limit: 10, want: []int{1, 2, 3, 4, 5}},
				{name: "after_gap", after: true, millis: 500, limit: 10, want: []int{1, 2, 3, 4, 5}},
				{name: "after_tied_time", after: true, millis: 1000, limit: 10, want: []int{3, 4, 5}},
				{name: "after_clock_skew", after: true, millis: 2500, limit: 10, want: []int{3, 5}},
				{name: "after_exact_last", after: true, millis: 4000, limit: 10},
				{name: "after_beyond_last", after: true, millis: 5000, limit: 10},
				{name: "after_limit", after: true, millis: 500, limit: 2, want: []int{1, 2}},
				{name: "zero_limit", after: true, millis: 500, limit: 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ref := Ref{IsTime: true, Time: base.Add(time.Duration(tc.millis) * time.Millisecond)}
					var got []Entry
					if tc.after {
						got = s.After(1, "#c", ref, tc.limit)
					} else {
						got = s.Before(1, "#c", ref, tc.limit)
					}
					if len(got) != len(tc.want) {
						t.Fatalf("got %d messages, want %v: %+v", len(got), tc.want, got)
					}
					for i, index := range tc.want {
						if want := fmt.Sprintf("message-%d", index); got[i].Params[1] != want {
							t.Errorf("message %d = %q, want %q", i, got[i].Params[1], want)
						}
					}
				})
			}
		})
	}
}
