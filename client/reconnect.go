package client

import (
	"context"
	"errors"
	"time"

	"lurk/irc"
)

// reconnect.go implements transparent auto-reconnect for a Connect-established
// session (Config.AutoReconnect). A supervisor goroutine watches the current
// connection; when it ends unexpectedly it re-dials with capped exponential
// backoff, re-registers, re-joins the channels the client was in, and resumes —
// keeping the same Events() stream alive across the gap so the UI and its buffers
// survive. A user-initiated Close/Quit (which closes c.stop) ends the supervisor
// for good.

// errStopped is the sentinel returned when a session attempt is abandoned because
// the client was asked to stop.
var errStopped = errors.New("client: stopped")

// reconnect backoff bounds.
const (
	reconnectInitialBackoff = 1 * time.Second
	reconnectMaxBackoff     = 30 * time.Second
	reconnectDialTimeout    = 30 * time.Second
)

// supervise owns a reconnectable client's lifecycle. It blocks until the current
// session ends, then either shuts down (on Close/Quit) or re-dials and keeps
// going. sessionDone is the channel the active connection's run goroutine closes
// when it exits.
func (c *Client) supervise(sessionDone chan struct{}) {
	for {
		select {
		case <-sessionDone:
			// The connection ended on its own. If we were asked to stop, finalize;
			// otherwise re-dial. The run goroutine has exited, so no publish can race
			// the finalize below.
			if c.stopped() {
				c.finalize()
				return
			}
		case <-c.stop:
			// Asked to stop while connected: tear the connection down and wait for
			// its run goroutine to exit before finalizing (so nothing publishes onto
			// a closed Events channel).
			if c.tr != nil {
				_ = c.tr.Close()
			}
			<-sessionDone
			c.finalize()
			return
		}

		// Unexpected drop: announce it and re-dial with backoff.
		c.emitSynthetic(evtReconnecting, "connection lost — reconnecting…")
		next := c.reconnectLoop()
		if next == nil {
			c.finalize() // stopped while retrying
			return
		}
		c.rejoinChannels()
		c.emitSynthetic(evtReconnected, "reconnected")
		sessionDone = next
	}
}

// reconnectLoop re-dials and re-registers with capped exponential backoff,
// returning the new session's done channel, or nil if the client was stopped
// before a connection could be re-established.
func (c *Client) reconnectLoop() chan struct{} {
	backoff := reconnectInitialBackoff
	for {
		select {
		case <-c.stop:
			return nil
		case <-time.After(backoff):
		}
		// time.After and c.stop can both be ready at once and select picks at
		// random, so re-check: a stop that arrived during the backoff must not
		// trigger one last wasteful dial (which would spin up, then immediately
		// tear down, a fresh session).
		if c.stopped() {
			return nil
		}

		dialCtx, cancel := context.WithTimeout(context.Background(), reconnectDialTimeout)
		tr, err := c.dial(dialCtx)
		cancel()
		if err != nil {
			backoff = nextBackoff(backoff)
			c.emitSynthetic(evtReconnecting, "reconnect failed — retrying…")
			continue
		}

		sessionDone := make(chan struct{})
		if err := c.startSession(tr, sessionDone, context.Background()); err != nil {
			_ = tr.Close()
			<-sessionDone // let the run goroutine exit before another attempt
			if errors.Is(err, errStopped) {
				return nil
			}
			backoff = nextBackoff(backoff)
			c.emitSynthetic(evtReconnecting, "reconnect failed — retrying…")
			continue
		}
		return sessionDone
	}
}

// nextBackoff doubles backoff up to the cap.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMaxBackoff {
		return reconnectMaxBackoff
	}
	return d
}

// bridgeDone mirrors a single (unsupervised, but private-channel) session's end
// onto the public done channel. It is used when a supervised connect fails to
// register: there is no supervisor, so this closes done once the run goroutine
// exits.
func (c *Client) bridgeDone(sessionDone chan struct{}) {
	<-sessionDone
	c.closeDone()
}

// finalize shuts the client down for good: it closes the public done channel and
// the Events stream so a ranging consumer (the TUI) learns the session is over.
// Callers must ensure the active run goroutine has exited first, so no publish
// races the Events close.
func (c *Client) finalize() {
	c.closeDone()
	c.closeEvents()
}

// closeDone closes the public done channel exactly once.
func (c *Client) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// closeEvents closes the Events stream channel exactly once, if one was ever
// created. publish checks for this via the same mutex so it never sends on a
// closed channel.
func (c *Client) closeEvents() {
	c.evMu.Lock()
	defer c.evMu.Unlock()
	if c.evCh != nil && !c.evClosed {
		c.evClosed = true
		close(c.evCh)
	}
}

// stopped reports whether Close/Quit has asked the client to stop.
func (c *Client) stopped() bool {
	select {
	case <-c.stop:
		return true
	default:
		return false
	}
}

// rejoinChannels re-sends JOIN for every channel the client was in before the
// drop, restoring membership after a reconnect.
func (c *Client) rejoinChannels() {
	for _, ch := range c.Channels() {
		_ = c.Join(ch)
	}
}

// emitSynthetic dispatches a client-originated semantic event (reconnecting /
// reconnected) carrying note as its trailing parameter, so it flows through the
// same handler and Events paths as any inbound message.
func (c *Client) emitSynthetic(command, note string) {
	msg := &irc.Message{Command: command}
	if note != "" {
		msg.Params = []string{note}
	}
	c.emit(&Event{Client: c, Message: msg, recvTime: time.Now()})
}
