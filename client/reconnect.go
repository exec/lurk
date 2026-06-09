package client

import (
	"context"
	"errors"
	"time"

	"github.com/exec/lurk/irc"
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

// reconnectRegisterTimeout bounds the registration phase (CAP/SASL/001) of a
// reconnect attempt. A peer that accepts the dial but then never completes the
// handshake — and never closes the socket — would otherwise wedge the
// supervisor forever: the client sets no read deadline and sends no keepalive
// PINGs, so nothing else unblocks startSession. With the bound, the attempt
// fails, the loop backs off, and reconnection keeps being retried. It is a
// variable only so tests can shorten it.
var reconnectRegisterTimeout = 60 * time.Second

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
			// The dead transport's goroutines have unwound, but a peer-initiated
			// drop (EOF / read error) only cancels the conn — it never closes the
			// underlying socket; Conn.Close alone does that. Release it before
			// re-dialing so a long-lived client on a flaky link doesn't leak a file
			// descriptor (stuck in CLOSE_WAIT) on every reconnect. Close is
			// idempotent, so the later teardown stays safe.
			if tr := c.transport(); tr != nil {
				_ = tr.Close()
			}
		case <-c.stop:
			// Asked to stop while connected: tear the connection down and wait for
			// its run goroutine to exit before finalizing (so nothing publishes onto
			// a closed Events channel).
			if tr := c.transport(); tr != nil {
				_ = tr.Close()
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
		c.remonitorNicks()
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

		// Bound the registration phase like the dial: the initial Connect honors
		// the caller's context here, but on reconnect there is no caller context,
		// and an unbounded wait on a stalled handshake would end reconnection
		// attempts for good.
		regCtx, cancelReg := context.WithTimeout(context.Background(), reconnectRegisterTimeout)
		sessionDone := make(chan struct{})
		err = c.startSession(tr, sessionDone, regCtx)
		cancelReg()
		if err != nil {
			_ = tr.Close()
			<-sessionDone // let the run goroutine exit before another attempt
			if errors.Is(err, errStopped) {
				return nil
			}
			backoff = nextBackoff(backoff)
			c.emitSynthetic(evtReconnecting, "reconnect failed — retrying…")
			continue
		}
		// Reset backoff so the next drop (if any) starts fresh from the initial
		// delay rather than the accumulated value. Without this, a flapping
		// upstream (connect → 001 → drop, repeat) plateaus at reconnectMaxBackoff
		// after a few cycles, making each subsequent reconnect wait the full max.
		backoff = reconnectInitialBackoff
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

// finalizeWhenDone waits for a single (unsupervised) session's run goroutine to
// exit, then finalizes the client — closing both the public done channel and the
// Events stream. It is used when reconnect is off and when a connect fails to
// register: there is no supervisor, so this is what releases an Events() consumer
// blocked in a range loop once the connection is gone.
func (c *Client) finalizeWhenDone(sessionDone chan struct{}) {
	<-sessionDone
	c.finalize()
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

// remonitorNicks re-sends MONITOR + for every nick that was being watched before
// the drop. It also clears the online set (all statuses are unknown until the
// server re-reports them via 730/731). Called after a successful reconnect.
func (c *Client) remonitorNicks() {
	nicks := c.MonitoredNicks()
	if len(nicks) == 0 {
		return
	}
	// Reset online state: we don't know who is online on the new session until
	// the server sends fresh 730/731 notifications.
	c.mu.Lock()
	c.st.monitorOnline = make(map[string]bool)
	c.mu.Unlock()
	_ = c.Monitor(nicks...)
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
