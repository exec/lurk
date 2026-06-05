package client

import (
	"testing"
	"time"

	"github.com/exec/lurk/irc"
)

// drainEvent reads one event from the stream with a timeout, failing on stall.
func drainEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

func TestEventsStreamDelivers(t *testing.T) {
	c := New(Config{Nick: "me"})
	ch := c.Events()

	// emit a couple of events as the run loop would.
	c.emit(&Event{Client: c, Message: &irc.Message{Command: irc.PRIVMSG, Params: []string{"#x", "hi"}}})
	c.emit(&Event{Client: c, Message: &irc.Message{Command: irc.JOIN, Params: []string{"#x"}}})

	ev1 := drainEvent(t, ch)
	if ev1.Command() != irc.PRIVMSG || ev1.Text() != "hi" || ev1.Target() != "#x" {
		t.Errorf("event 1 = %+v, want PRIVMSG #x hi", ev1.Message)
	}
	ev2 := drainEvent(t, ch)
	if ev2.Command() != irc.JOIN {
		t.Errorf("event 2 command = %q, want JOIN", ev2.Command())
	}
}

func TestEventsSharedChannel(t *testing.T) {
	c := New(Config{Nick: "me"})
	if c.Events() != c.Events() {
		t.Error("Events() should return the same shared channel on repeat calls")
	}
}

func TestEventsNoConsumerNeverBlocks(t *testing.T) {
	// With no Events() subscriber, publish must be a no-op and never block.
	c := New(Config{Nick: "me"})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			c.emit(&Event{Client: c, Message: &irc.Message{Command: irc.PRIVMSG, Params: []string{"#x", "spam"}}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked with no subscriber")
	}
}

func TestEventsOverflowDropOldest(t *testing.T) {
	c := New(Config{Nick: "me"})
	ch := c.Events()

	// Overfill the buffer well past capacity without reading: it must not block,
	// and must drop-oldest, eventually surfacing a synthetic overflow marker.
	total := eventBufferSize * 3
	for i := 0; i < total; i++ {
		c.emit(&Event{Client: c, Message: &irc.Message{Command: irc.PRIVMSG, Params: []string{"#x", "m"}}})
	}

	// Drain everything currently buffered: count real events, count drops
	// reported via synthetic markers, and verify the buffer stayed bounded.
	reportedDropped := 0
	realDelivered := 0
	markers := 0
	buffered := 0
	for {
		select {
		case ev := <-ch:
			buffered++
			if ev.Dropped > 0 {
				markers++
				reportedDropped += ev.Dropped
				if ev.Message != nil {
					t.Error("overflow marker should carry a nil Message")
				}
			} else {
				realDelivered++
			}
		default:
			goto done
		}
	}
done:
	// The channel never holds more than its capacity.
	if buffered > eventBufferSize {
		t.Errorf("drained %d events, buffer cap is %d — drop-oldest not bounding", buffered, eventBufferSize)
	}
	if markers == 0 {
		t.Error("expected at least one synthetic overflow marker reporting dropped events")
	}
	// Exact accounting: every produced event is either delivered as a real event,
	// reported in a marker's Dropped count, or still pending in evDropped (events
	// dropped after the last marker was enqueued).
	c.evMu.Lock()
	pending := c.evDropped
	c.evMu.Unlock()
	if realDelivered+reportedDropped+pending != total {
		t.Errorf("accounting mismatch: real=%d + reported=%d + pending=%d = %d, want %d",
			realDelivered, reportedDropped, pending, realDelivered+reportedDropped+pending, total)
	}
}

func TestEventTimeServerTimeTag(t *testing.T) {
	// With a server-time @time tag, Time() returns the parsed tag time.
	msg := &irc.Message{
		Command: irc.PRIVMSG,
		Params:  []string{"#x", "hi"},
		Tags:    irc.Tags{"time": "2011-10-19T16:40:51.620Z"},
	}
	ev := &Event{Message: msg, recvTime: time.Now()}
	want, _ := time.Parse(time.RFC3339Nano, "2011-10-19T16:40:51.620Z")
	if !ev.Time().Equal(want) {
		t.Errorf("Time() = %v, want %v (from @time tag)", ev.Time(), want)
	}
}

func TestEventTimeFallbackToRecv(t *testing.T) {
	recv := time.Now()
	// No tag → recvTime.
	ev := &Event{Message: &irc.Message{Command: irc.PRIVMSG}, recvTime: recv}
	if !ev.Time().Equal(recv) {
		t.Errorf("Time() = %v, want recvTime %v", ev.Time(), recv)
	}
	// Malformed tag → recvTime.
	ev2 := &Event{Message: &irc.Message{Command: irc.PRIVMSG, Tags: irc.Tags{"time": "not-a-time"}}, recvTime: recv}
	if !ev2.Time().Equal(recv) {
		t.Errorf("Time() with bad tag = %v, want recvTime %v", ev2.Time(), recv)
	}
}

func TestEventNilSafeAccessors(t *testing.T) {
	// A synthetic overflow event has a nil Message; accessors must not panic.
	ev := &Event{Dropped: 5}
	if ev.Command() != "" || ev.Nick() != "" || ev.Text() != "" || ev.Target() != "" || ev.Param(0) != "" {
		t.Error("accessors on a nil-Message event should return zero values")
	}
}
