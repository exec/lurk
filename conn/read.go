package conn

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"lurk/irc"
)

// ReadMessage blocks until the next protocol message is available, returning it
// parsed. It draws from the same internal reader goroutine that feeds Messages,
// so a given Conn should use either ReadMessage or Messages consistently rather
// than both.
//
// On a terminal condition (EOF, a read error, an over-long line, or Close)
// ReadMessage returns the error reported by Err; subsequent calls keep
// returning it. A non-nil message with a nil error is the normal case.
func (c *Conn) ReadMessage() (*irc.Message, error) {
	m, ok := <-c.msgs
	if !ok {
		return nil, c.Err()
	}
	return m, nil
}

// Messages returns the channel of parsed inbound messages. The channel is
// closed when the reader terminates (EOF, error, or Close); after it closes,
// call Err for the cause. Consumers using the channel should not also call
// ReadMessage on the same Conn.
func (c *Conn) Messages() <-chan *irc.Message {
	return c.msgs
}

// readLoop frames the byte stream into lines, enforces the length budgets,
// parses each line, and delivers messages until a terminal error or Close.
func (c *Conn) readLoop() {
	defer c.wg.Done()
	defer close(c.msgs)

	for {
		line, err := c.readLine()
		if err != nil {
			c.fail(err)
			return
		}
		// An empty line (blank or bare CRLF) is not a protocol message; the
		// spec says to silently ignore it rather than parse.
		if line == "" {
			continue
		}

		msg, perr := irc.Parse(line)
		if perr != nil {
			// A malformed line is non-fatal: the connection is still usable, so
			// skip the bad line rather than tearing down. (A future option could
			// surface these; for Cycle 1 we drop them and keep going.)
			continue
		}

		select {
		case c.msgs <- msg:
		case <-c.ctx.Done():
			c.fail(c.ctx.Err())
			return
		}
	}
}

// readLine reads a single LF-terminated line, tolerating a bare LF and
// stripping a stray trailing CR (so it accepts \r\n, bare \n, and a final line
// without a terminator at EOF). The returned line excludes the terminator and
// is validated against the tag and message budgets, returning a wrapped
// ErrLineTooLong when either is exceeded.
//
// The read is bounded: ReadSlice draws from a fixed-size buffer (MaxLineBytes),
// so a peer that withholds the newline cannot force unbounded buffering — once
// the buffer fills, ReadSlice returns bufio.ErrBufferFull and we surface
// ErrLineTooLong rather than growing memory. This bounded-readQ design follows
// Ergo's ircreader (reference/ergo/.../ircreader/ircreader.go), adapted to
// stdlib bufio.
func (c *Conn) readLine() (string, error) {
	if c.opts.ReadTimeout > 0 {
		// Reset the idle deadline before each read. bufio may satisfy the read
		// from already-buffered data without touching the socket, in which case
		// the deadline is simply unused until the next socket read.
		if err := c.raw.SetReadDeadline(time.Now().Add(c.opts.ReadTimeout)); err != nil {
			return "", fmt.Errorf("conn: set read deadline: %w", err)
		}
	}

	slice, err := c.br.ReadSlice('\n')
	switch {
	case err == nil:
		return c.trimAndCheck(slice, false)
	case errors.Is(err, bufio.ErrBufferFull):
		// The line reached the buffer cap without a newline: it cannot be a
		// conformant line, so reject it as over-long. We do not attempt to
		// resynchronize to the next newline; an over-long line is a framing
		// fault and the connection is torn down (a conformant server never
		// sends a client a line this long). This is the deliberate counterpart
		// to Ergo's server-side ErrReadQ, which likewise terminates the peer.
		return "", fmt.Errorf("%w: line reached %d-byte cap without terminator", ErrLineTooLong, MaxLineBytes)
	case errors.Is(err, io.EOF) && len(slice) > 0:
		// A final line without a terminator at EOF is still worth delivering.
		return c.trimAndCheck(slice, true)
	default:
		return "", err
	}
}

// trimAndCheck removes the LF/CRLF terminator, strips a stray trailing CR, and
// validates the line against the tag and message budgets. partial indicates the
// line arrived without a terminator (trailing data at EOF). The input is the
// raw slice from ReadSlice, which is only valid until the next read; the string
// conversion here copies it, so the result is safe to retain.
func (c *Conn) trimAndCheck(raw []byte, partial bool) (string, error) {
	line := string(raw)
	if !partial {
		// Drop the trailing '\n'.
		line = strings.TrimSuffix(line, "\n")
	}
	// Strip a single stray trailing CR (the CR of a CRLF) so a \r\n terminator
	// is fully removed; a bare \n leaves no CR to strip.
	line = strings.TrimSuffix(line, "\r")

	if err := checkBudget(line); err != nil {
		return "", err
	}
	return line, nil
}

// checkBudget enforces the IRCv3 tag-data (MaxTagBytes) and message-body
// (MaxMessageBytes) budgets on a single line whose terminator has already been
// removed. When a tag segment is present (leading '@'), the data between '@' and
// the separating space is measured against MaxTagBytes and the remainder against
// MaxMessageBytes; otherwise the whole line is the message body.
func checkBudget(line string) error {
	tagSeg := ""
	body := line
	if strings.HasPrefix(line, "@") {
		if sp := strings.IndexByte(line, ' '); sp >= 0 {
			tagSeg = line[1:sp] // exclude '@' and the separating space
			body = line[sp+1:]
		} else {
			// A line that is all tags and no command is malformed, but bound it
			// by the tag budget so we don't pass an oversized blob downstream.
			tagSeg = line[1:]
			body = ""
		}
	}

	if len(tagSeg) > MaxTagBytes {
		return fmt.Errorf("%w: tag segment %d > %d bytes", ErrLineTooLong, len(tagSeg), MaxTagBytes)
	}
	if len(body) > MaxMessageBytes {
		return fmt.Errorf("%w: message body %d > %d bytes", ErrLineTooLong, len(body), MaxMessageBytes)
	}
	return nil
}

// fail records err as the terminal error (the first one wins) and cancels the
// Conn so the writer goroutine also unwinds. A clean EOF is normalized to
// io.EOF; an explicit Close is normalized to ErrClosed by Close itself.
func (c *Conn) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	c.cancel()
}

// Err returns the terminal error that stopped the Conn, or nil if it is still
// running. After the Messages channel closes (or ReadMessage returns an error),
// Err reports the cause: io.EOF for a clean server hangup, ErrClosed for a local
// Close, or a wrapped read/budget/context error otherwise.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// IsTerminal reports whether err is a condition that has torn down the Conn (as
// opposed to a transient/parse issue, which Conn handles internally). It is a
// small convenience for run loops. In practice every non-nil error surfaced by
// Err is terminal once the Messages channel has closed; this names the common
// causes explicitly and additionally recognizes network errors (such as a
// ReadTimeout deadline) for callers that branch on the cause.
func IsTerminal(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, ErrClosed) ||
		errors.Is(err, ErrLineTooLong) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne)
}
