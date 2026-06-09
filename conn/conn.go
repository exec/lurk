// Package conn implements the IRC transport layer: dialing a server over plain
// TCP or TLS, framing the byte stream into protocol lines, and reading/writing
// irc.Message values.
//
// A Conn owns two goroutines once dialed: a reader that frames incoming lines,
// enforces the IRCv3 length budgets, parses each line via irc.Parse, and feeds
// the result to consumers; and a writer that drains a buffered outbound queue.
// Consumers may read either by calling the blocking ReadMessage or by ranging
// over the Messages channel — both draw from the same reader goroutine, so pick
// one style per Conn. Writes go through the non-blocking WriteMessage/Send,
// which enqueue onto the outbound buffer.
//
// Conn depends only on the irc package and the standard library.
package conn

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/exec/lurk/irc"
)

// IRCv3 line-length budgets, in bytes, applied per received line (excluding the
// trailing CRLF). These are derived from the canonical constants the irc parser
// exports (irc.MaxLenTags / irc.MaxLenMessage), restated here in the form the
// framer applies them: against the tag-data segment (between '@' and the space)
// and the non-tag body separately.
const (
	// MaxMessageBytes is the budget for the non-tag portion of a line (command,
	// source, params) excluding CRLF. irc.MaxLenMessage (512) counts the 2-byte
	// CRLF, so the on-the-wire content budget is 2 less.
	MaxMessageBytes = irc.MaxLenMessage - 2

	// MaxTagBytes is the budget for the tag segment of a line measured between
	// the leading '@' and the separating space (i.e. the tag *data*). This is
	// irc.MaxLenTagData: the spec's 8191-byte tag limit (irc.MaxLenTags) counts
	// the '@' and the space, leaving 8189 bytes of data.
	MaxTagBytes = irc.MaxLenTagData

	// MaxLineBytes is the hard upper bound on a single received line, terminator
	// excluded. It is the tag budget plus the message budget plus slack: a
	// conformant line never approaches it, but it caps how much the reader will
	// buffer for one unterminated line so a peer cannot force unbounded memory
	// growth by withholding the newline. The reader rejects a line that reaches
	// this bound with ErrLineTooLong before the per-field budgets are even
	// checked. (Bound modeled on Ergo's ircreader readQ cap of 8192+1024 bytes;
	// see the credit in read.go.)
	MaxLineBytes = irc.MaxLenTags + irc.MaxLenMessage + 512

	// readBufferLines is the depth of the inbound Messages channel buffer.
	readBufferLines = 64

	// writeQueueDepth is the depth of the outbound send queue.
	writeQueueDepth = 64
)

// Sentinel errors surfaced by Conn. Callers may test these with errors.Is.
var (
	// ErrClosed is returned by Send/WriteMessage after the Conn is closed, and
	// is the terminal error reported by Err when Close was called explicitly.
	ErrClosed = errors.New("conn: closed")

	// ErrLineTooLong is returned (wrapped) when a received line exceeds the tag
	// or message budget. It corresponds to ERR_INPUTTOOLONG (417) on the wire.
	ErrLineTooLong = errors.New("conn: line exceeds length budget")
)

// Options configures Dial. The zero value dials plain TCP with no timeouts.
type Options struct {
	// TLS, when true, wraps the connection in a TLS client handshake using
	// TLSConfig (or a default config if TLSConfig is nil). Certificate
	// verification is on by default; opt out with InsecureSkipVerify.
	TLS bool

	// TLSConfig is the configuration for the TLS handshake. If nil and TLS is
	// true, a config is synthesized with ServerName set to the dialed host.
	// If non-nil it is cloned before use; ServerName is filled in from the
	// dial address when empty.
	TLSConfig *tls.Config

	// InsecureSkipVerify disables TLS certificate verification. It is a
	// convenience that maps onto TLSConfig.InsecureSkipVerify; use only for
	// testing or when connecting to servers with self-signed certs knowingly.
	InsecureSkipVerify bool

	// Dialer is the net.Dialer used for the underlying TCP connection. The zero
	// value is used if left empty; the Dial context still governs cancellation.
	Dialer net.Dialer

	// WriteTimeout, if non-zero, bounds each flush of the outbound queue. A
	// write that exceeds it fails the Conn.
	WriteTimeout time.Duration

	// ReadTimeout, if non-zero, is the maximum idle time between received bytes.
	// The reader sets a read deadline this far in the future before each read
	// and the deadline resets on every successful read, so it bounds inactivity,
	// not total connection lifetime. It exists so a silently-dead server (one
	// that neither sends data nor closes the socket) surfaces an error instead
	// of blocking the reader forever. A client that sends periodic PINGs should
	// set this comfortably larger than its PING interval. Zero disables it.
	ReadTimeout time.Duration

	// Limiter, if non-nil, is consulted by the writer goroutine before each
	// outbound line; it is the seam for flood/rate limiting. The default (nil)
	// applies no limiting.
	Limiter Limiter
}

// Limiter is the seam for outbound flood/rate limiting. The writer goroutine
// calls Wait before sending each line; Wait should block until the line may be
// sent or return a non-nil error to fail the Conn. A nil Limiter means no
// limiting. Implementations must be safe for use from the single writer
// goroutine (no concurrent calls).
type Limiter interface {
	Wait(ctx context.Context, line string) error
}

// Conn is a framed IRC connection over an underlying net.Conn.
type Conn struct {
	raw net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	opts Options

	// ctx/cancel govern the lifetime of the read and write goroutines.
	ctx    context.Context
	cancel context.CancelFunc

	msgs chan *irc.Message // reader -> consumer
	out  chan string       // producer -> writer (serialized lines, no CRLF)
	done chan struct{}     // closed by Close to start graceful shutdown

	closeOnce  sync.Once
	wg         sync.WaitGroup // both goroutines, awaited by Close at the end
	writerDone sync.WaitGroup // writer only, so Close can await the drain

	mu  sync.Mutex
	err error // first terminal error, surfaced by Err
}

// Dial connects to addr (host:port) on the given network ("tcp", "tcp4",
// "tcp6") and returns a ready Conn. The context governs the dial and the TLS
// handshake; once Dial returns, the context no longer affects the Conn (use
// Close to tear it down). TLS is used when opts.TLS is set.
func Dial(ctx context.Context, network, addr string, opts Options) (*Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	raw, err := opts.Dialer.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("conn: dial %s %s: %w", network, addr, err)
	}

	if opts.TLS {
		raw, err = tlsHandshake(ctx, raw, addr, opts)
		if err != nil {
			return nil, err
		}
	}

	return newConn(raw, opts), nil
}

// tlsHandshake wraps raw in a TLS client and completes the handshake under ctx.
func tlsHandshake(ctx context.Context, raw net.Conn, addr string, opts Options) (net.Conn, error) {
	cfg := opts.TLSConfig.Clone()
	if cfg == nil {
		cfg = &tls.Config{}
	}
	if cfg.ServerName == "" {
		if host, _, splitErr := net.SplitHostPort(addr); splitErr == nil {
			cfg.ServerName = host
		}
	}
	// Pin a TLS 1.2 floor independent of the Go toolchain's default, so the
	// minimum can never silently regress on an older build. Callers may raise
	// it further via opts.TLSConfig.MinVersion.
	if cfg.MinVersion == 0 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if opts.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}

	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("conn: tls handshake %s: %w", addr, err)
	}
	return tc, nil
}

// NewConn wraps an already-established net.Conn (e.g. one half of net.Pipe or a
// connection accepted from a test listener) in a framed Conn. It is the seam
// used by tests and by callers that perform their own dialing/TLS. The Conn
// takes ownership of raw and closes it on Close.
func NewConn(raw net.Conn, opts Options) *Conn {
	return newConn(raw, opts)
}

// newConn builds a Conn around raw and starts its reader and writer goroutines.
func newConn(raw net.Conn, opts Options) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		raw: raw,
		// Size the read buffer to the hard line cap (plus CRLF) so a line that
		// never terminates is bounded: ReadSlice reports ErrBufferFull once the
		// buffer fills, which the reader maps to ErrLineTooLong instead of
		// growing memory without limit.
		br:     bufio.NewReaderSize(raw, MaxLineBytes+2),
		bw:     bufio.NewWriter(raw),
		opts:   opts,
		ctx:    ctx,
		cancel: cancel,
		msgs:   make(chan *irc.Message, readBufferLines),
		out:    make(chan string, writeQueueDepth),
		done:   make(chan struct{}),
	}

	c.wg.Add(2)
	c.writerDone.Add(1)
	go c.readLoop()
	go c.writeLoop()
	return c
}

// CloseGrace bounds how long Close waits for the writer goroutine to drain and
// flush already-enqueued outbound lines before the underlying connection is
// torn down. It is a package default; Close uses it unconditionally in Cycle 1.
const CloseGrace = 2 * time.Second

// Close shuts the Conn down gracefully. It closes the outbound queue so the
// writer drains and flushes any already-enqueued lines (bounded by CloseGrace),
// then cancels the goroutines, closes the underlying connection, and waits for
// both goroutines to exit. It is safe to call multiple times and from multiple
// goroutines; only the first call has an effect. After Close, Err reports
// ErrClosed unless an earlier read/write error was already recorded. Close
// returns the error from closing the underlying connection, if any.
func (c *Conn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		// Record ErrClosed first so Err reports the local-close cause rather
		// than a context.Canceled that the cancel below sets for the reader.
		c.mu.Lock()
		if c.err == nil {
			c.err = ErrClosed
		}
		c.mu.Unlock()

		// Signal graceful shutdown: the writer stops accepting new lines and
		// drains the queue; in-flight Send calls observe c.done and return.
		close(c.done)

		// Let the writer drain the queue, but don't wait forever: if the socket
		// is wedged, the grace timer wins and we force the teardown below.
		drained := make(chan struct{})
		go func() {
			c.writerDone.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(CloseGrace):
		}

		// Cancel the reader (and any still-running writer), close the socket,
		// and wait for both goroutines to fully exit.
		c.cancel()
		closeErr = c.raw.Close()
		c.wg.Wait()
	})
	return closeErr
}

// LocalAddr returns the local network address of the underlying connection.
func (c *Conn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// RemoteAddr returns the remote network address of the underlying connection.
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }
