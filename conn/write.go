package conn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"lurk/irc"
)

// WriteMessage serializes m and enqueues it for sending. It does not block on
// the network: the line is handed to the writer goroutine via the outbound
// queue, which serializes the actual I/O. It returns an error only if the
// message cannot be serialized or the Conn is already closed/failed. While the
// queue is full, Send blocks until space frees up or the Conn terminates.
func (c *Conn) WriteMessage(m *irc.Message) error {
	line, err := m.Serialize()
	if err != nil {
		return fmt.Errorf("conn: serialize: %w", err)
	}
	return c.Send(line)
}

// Send enqueues a single already-serialized protocol line for the writer
// goroutine. Any trailing CRLF the caller includes is stripped (the writer
// appends its own); an interior CR/LF/NUL is rejected, since it could split the
// line into multiple commands on the wire. Send returns the terminal error
// (ErrClosed or the recorded failure) once the Conn is shutting down. When the
// outbound queue is full, Send blocks until space is available or the Conn
// terminates.
func (c *Conn) Send(line string) error {
	line = strings.TrimRight(line, "\r\n")
	if strings.ContainsAny(line, "\r\n\x00") {
		return fmt.Errorf("conn: refusing to send line containing CR/LF/NUL: %q", line)
	}

	// c.out is never closed (Close signals shutdown via c.done instead), so the
	// only way to lose a line is to lose the race with shutdown, which we detect
	// by selecting on c.done. This keeps Send panic-free under concurrent Close.
	select {
	case <-c.done:
		return c.closedErr()
	default:
	}
	select {
	case c.out <- line:
		return nil
	case <-c.done:
		return c.closedErr()
	}
}

// writeLoop drains the outbound queue, applies the optional Limiter, frames each
// line with CRLF, and flushes. On a normal Close it keeps running until c.done
// fires, then drains whatever remains in the queue and exits. On a hard failure
// (read error or forced teardown) the Conn context is cancelled and it stops
// without draining, since the socket is going away.
func (c *Conn) writeLoop() {
	defer c.wg.Done()
	defer c.writerDone.Done()

	for {
		select {
		case line := <-c.out:
			if err := c.writeLine(line); err != nil {
				c.fail(err)
				return
			}
		case <-c.done:
			c.drain()
			return
		case <-c.ctx.Done():
			// Hard cancellation: the socket is going away, abandon the queue.
			return
		}
	}
}

// drain writes every line still buffered in the outbound queue, stopping when
// the queue is empty, on a write error, or if the Conn context is cancelled
// (the CloseGrace window expired). It runs only on the graceful-close path.
func (c *Conn) drain() {
	for {
		select {
		case line := <-c.out:
			if err := c.writeLine(line); err != nil {
				c.fail(err)
				return
			}
		case <-c.ctx.Done():
			return
		default:
			return
		}
	}
}

// writeLine consults the Limiter, writes the line plus CRLF under the optional
// write timeout, and flushes the buffered writer.
func (c *Conn) writeLine(line string) error {
	if c.opts.Limiter != nil {
		if err := c.opts.Limiter.Wait(c.ctx, line); err != nil {
			return fmt.Errorf("conn: rate limit: %w", err)
		}
	}

	if c.opts.WriteTimeout > 0 {
		if err := c.raw.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout)); err != nil {
			return fmt.Errorf("conn: set write deadline: %w", err)
		}
	}

	if _, err := c.bw.WriteString(line); err != nil {
		return fmt.Errorf("conn: write: %w", err)
	}
	if _, err := c.bw.WriteString("\r\n"); err != nil {
		return fmt.Errorf("conn: write: %w", err)
	}
	if err := c.bw.Flush(); err != nil {
		return fmt.Errorf("conn: flush: %w", err)
	}
	return nil
}

// closedErr reports the terminal error for a Send that lost the race with
// shutdown, preferring the recorded cause but falling back to ErrClosed.
func (c *Conn) closedErr() error {
	if err := c.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return ErrClosed
}
