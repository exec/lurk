package client

// eventBufferSize is the capacity of the Events stream channel. It is large
// enough to absorb a big server burst — a registration flood, a busy channel, or
// a large /list / WHO / CHATHISTORY reply — while a UI consumer renders a frame,
// without letting an unbounded backlog accumulate. The TUI bridge drains this in
// batches (see tui/events.go), so the buffer only needs to cover what arrives
// during a single render; this headroom keeps a fast loopback bouncer feed from
// overrunning it between batches.
const eventBufferSize = 1024

// Events returns a buffered, read-only stream of every protocol event the
// client dispatches — the same Events that On/OnAny/Handle* callbacks observe,
// in the same order, after state tracking has been applied.
//
// Fan-out model: a SINGLE shared channel. The first call creates it; every
// subsequent call returns that same channel. It is therefore intended for ONE
// consumer (e.g. the TUI's event bridge); multiple goroutines ranging the same
// channel would compete for events. Callers wanting independent subscriptions
// should fan out themselves from this single stream.
//
// Backpressure: the stream MUST NOT block the client's read loop. Delivery is
// non-blocking with a drop-OLDEST policy — if the buffer (eventBufferSize) is
// full because the consumer fell behind, the oldest buffered event is discarded
// to make room for the newest, and a counter is incremented. The next event
// delivered after any drops is preceded by a synthetic overflow event with its
// Dropped field set to the number of events lost (and a nil Message); consumers
// should check Event.Dropped before using Message. This bounds memory and keeps
// the connection responsive even if the UI stalls.
//
// The channel is never closed by Events; use Done/Wait to learn when the
// connection ends. Events delivered are snapshots — the Client's state
// accessors (Nick, Channels, Members, Topic, ...) reflect the state as of the
// moment the consumer reads them, which may be slightly ahead of a buffered
// event.
func (c *Client) Events() <-chan Event {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	if c.evCh == nil {
		c.evCh = make(chan Event, eventBufferSize)
	}
	return c.evCh
}

// publish delivers ev to the Events stream if a consumer has subscribed. It is
// called from the client's single run goroutine for every emitted event and
// never blocks: on a full buffer it drops the oldest event(s) to make room,
// tracking the count so the loss is reported via a synthetic overflow event.
func (c *Client) publish(ev *Event) {
	c.evMu.Lock()
	ch := c.evCh
	if ch == nil || c.evClosed {
		c.evMu.Unlock()
		return
	}

	// If earlier deliveries dropped events, surface a synthetic overflow marker
	// first so the consumer learns about the gap, then continue with ev. Snapshot
	// the count we are reporting and subtract exactly that on success: trySend may
	// itself drop events (bumping evDropped) while making room for the marker, and
	// those new drops must remain counted rather than be reset away.
	if c.evDropped > 0 {
		reporting := c.evDropped
		marker := Event{Client: c, Dropped: reporting, recvTime: ev.recvTime}
		if c.trySend(ch, marker) {
			c.evDropped -= reporting
		}
		// If even the marker couldn't be sent, keep accumulating into evDropped
		// below; the marker will be retried on a later publish.
	}

	if !c.trySend(ch, *ev) {
		// Buffer still full after drop-oldest attempts: count this event as lost.
		c.evDropped++
	}
	c.evMu.Unlock()
}

// trySend attempts a non-blocking send of ev onto ch. If ch is full it drops
// the single oldest buffered event (a non-blocking receive) and retries once,
// incrementing evDropped for the discarded event. It returns whether ev was
// ultimately enqueued. evMu is held by the caller.
func (c *Client) trySend(ch chan Event, ev Event) bool {
	select {
	case ch <- ev:
		return true
	default:
	}
	// Full: drop the oldest to make room (drop-oldest policy).
	select {
	case dropped := <-ch:
		// Account for the discarded event. A discarded synthetic marker just
		// rolls its count forward.
		if dropped.Dropped > 0 {
			c.evDropped += dropped.Dropped
		} else {
			c.evDropped++
		}
	default:
		// Someone drained it concurrently (shouldn't happen for a single
		// consumer, but harmless); fall through to retry.
	}
	select {
	case ch <- ev:
		return true
	default:
		return false
	}
}
