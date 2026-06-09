package conn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/exec/lurk/irc"
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

// TryWriteMessage serializes m and attempts a non-blocking enqueue. It returns
// (true, nil) on success, (false, nil) when the outbound queue is full (the
// message is dropped — the caller decides how to handle the drop), and
// (false, err) if the message cannot be serialized or the Conn is already
// closed. Unlike WriteMessage/Send, TryWriteMessage never blocks waiting for
// queue space; it is safe to call from a goroutine that must not stall.
func (c *Conn) TryWriteMessage(m *irc.Message) (bool, error) {
	line, err := m.Serialize()
	if err != nil {
		return false, fmt.Errorf("conn: serialize: %w", err)
	}
	return c.TrySend(line)
}

// TrySend attempts a non-blocking enqueue of an already-serialized line.
// It returns (true, nil) when the line was accepted, (false, nil) when the
// outbound queue is full, and (false, err) if the Conn is already closed or
// the line contains forbidden bytes (CR/LF/NUL). The caller is responsible
// for deciding what to do when the queue is full (log, drop, close the peer).
func (c *Conn) TrySend(line string) (bool, error) {
	line = strings.TrimRight(line, "\r\n")
	if strings.ContainsAny(line, "\r\n\x00") {
		return false, fmt.Errorf("conn: refusing to send line containing CR/LF/NUL: %q", line)
	}

	select {
	case <-c.done:
		return false, c.closedErr()
	default:
	}
	select {
	case c.out <- line:
		return true, nil
	case <-c.done:
		return false, c.closedErr()
	default:
		// Queue is full; return false without blocking.
		return false, nil
	}
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
//
// Burst coalescing: after the first line is dequeued, any additional lines
// already sitting in the outbound channel are written to the buffered writer
// without flushing (up to writeQueueDepth lines per batch). A single Flush at
// the end of each batch coalesces what would otherwise be N consecutive kernel
// write syscalls into one — the dominant cost on a loopback or fast bouncer
// feed replaying a large CHATHISTORY batch.
func (c *Conn) writeLoop() {
	defer c.wg.Done()
	defer c.writerDone.Done()

	for {
		select {
		case line := <-c.out:
			if err := c.writeBurst(line); err != nil {
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

// writeBurst writes first plus any lines already queued in c.out (up to
// writeQueueDepth total) into the buffered writer, then flushes once. The
// Limiter (if set) is consulted for each line before it is written, and the
// WriteTimeout deadline (if set) is refreshed per line, after the Limiter
// wait — so time spent paced by the Limiter never counts against the socket
// deadline, and the deadline set for the batch's last line covers the final
// Flush.
func (c *Conn) writeBurst(first string) error {
	if err := c.writeToBuffer(first); err != nil {
		return err
	}

	// Non-blocking drain: pull any additional lines that are already waiting
	// in the channel and append them to the same bufio.Writer buffer. Stop
	// when the channel is empty or the batch cap is reached, so we never
	// spin here indefinitely under a sustained producer.
	for i := 1; i < writeQueueDepth; i++ {
		select {
		case line := <-c.out:
			if err := c.writeToBuffer(line); err != nil {
				return err
			}
		default:
			// Nothing more queued; stop coalescing.
			goto flush
		}
	}

flush:
	if err := c.bw.Flush(); err != nil {
		return fmt.Errorf("conn: flush: %w", err)
	}
	return nil
}

// writeToBuffer consults the Limiter (if set), refreshes the write deadline
// (if configured), and writes line + CRLF into the buffered writer. It does
// NOT flush explicitly; the caller flushes once the batch is complete. The
// deadline is set after the Limiter wait and before the write so that (a)
// Limiter pacing never erodes it, and (b) an implicit flush — when the
// bufio.Writer fills mid-batch and WriteString touches the socket — is still
// covered by a fresh deadline.
func (c *Conn) writeToBuffer(line string) error {
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
	return nil
}

// drain writes every line still buffered in the outbound queue, stopping when
// the queue is empty, on a write error, or if the Conn context is cancelled
// (the CloseGrace window expired). It runs only on the graceful-close path.
// Each line is written and flushed individually so partial progress is visible
// to the peer even if the context fires mid-drain.
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
// write timeout, and flushes the buffered writer. It is used by the
// graceful-close drain path where each line is flushed individually so a
// partial drain makes forward progress even if the grace window is tight.
func (c *Conn) writeLine(line string) error {
	if err := c.writeToBuffer(line); err != nil {
		return err
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
