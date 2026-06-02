package conn

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"lurk/irc"
)

// pipeConn wraps one end of net.Pipe in a Conn and returns both the Conn and
// the other (server) end for the test to drive directly.
func pipeConn(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	cliRaw, srvRaw := net.Pipe()
	c := NewConn(cliRaw, Options{})
	t.Cleanup(func() {
		c.Close()
		srvRaw.Close()
	})
	return c, srvRaw
}

// writeAll writes s to w in full. It is the test's "server" side feeding bytes
// to the Conn under test, and is safe to call from a goroutine: a write error
// (e.g. the Conn closed first) is not fatal to the test, which asserts on the
// read/parse side, so the error is intentionally dropped.
func writeAll(_ *testing.T, w net.Conn, s string) {
	_, _ = io.WriteString(w, s)
}

func TestReadMessageFramesCRLF(t *testing.T) {
	c, srv := pipeConn(t)

	go writeAll(t, srv, "PING :token\r\nPRIVMSG #ch :hello world\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "PING" || m.Param(0) != "token" {
		t.Fatalf("got %+v, want PING token", m)
	}

	m, err = c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage 2: %v", err)
	}
	if m.Command != "PRIVMSG" || m.Param(0) != "#ch" || m.Param(1) != "hello world" {
		t.Fatalf("got %+v, want PRIVMSG #ch :hello world", m)
	}
}

func TestReadToleratesBareLFAndStrayCR(t *testing.T) {
	c, srv := pipeConn(t)

	// Bare LF terminator, and a line with a stray CR that should be stripped.
	go writeAll(t, srv, "NICK alice\nUSER a 0 * :A\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "NICK" || m.Param(0) != "alice" {
		t.Fatalf("got %+v, want NICK alice", m)
	}
	m, err = c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage 2: %v", err)
	}
	if m.Command != "USER" || m.Param(3) != "A" {
		t.Fatalf("got %+v, want USER ... :A", m)
	}
}

func TestReadSkipsBlankLines(t *testing.T) {
	c, srv := pipeConn(t)
	go writeAll(t, srv, "\r\n\r\nPING :x\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "PING" {
		t.Fatalf("got %+v, want PING after blank lines", m)
	}
}

func TestReadSkipsUnparseableLine(t *testing.T) {
	c, srv := pipeConn(t)
	// A leading-space line is malformed; the reader should skip it and deliver
	// the following valid line rather than failing the Conn.
	go writeAll(t, srv, "   \r\nPING :ok\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "PING" || m.Param(0) != "ok" {
		t.Fatalf("got %+v, want PING ok", m)
	}
}

func TestReadEOFSurfacesError(t *testing.T) {
	c, srv := pipeConn(t)
	go func() {
		writeAll(t, srv, "PING :x\r\n")
		srv.Close()
	}()

	if _, err := c.ReadMessage(); err != nil {
		t.Fatalf("first ReadMessage: %v", err)
	}
	_, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected error after server close, got nil")
	}
	if !errors.Is(c.Err(), io.EOF) {
		t.Fatalf("Err = %v, want io.EOF", c.Err())
	}
	// Channel should be closed too.
	if _, ok := <-c.Messages(); ok {
		t.Fatal("Messages channel should be closed after EOF")
	}
}

func TestReadFinalLineWithoutNewline(t *testing.T) {
	c, srv := pipeConn(t)
	go func() {
		writeAll(t, srv, "QUIT :bye") // no trailing CRLF
		srv.Close()
	}()

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "QUIT" || m.Param(0) != "bye" {
		t.Fatalf("got %+v, want QUIT bye", m)
	}
}

func TestReadOverlongMessageFails(t *testing.T) {
	c, srv := pipeConn(t)
	long := "PRIVMSG #x :" + strings.Repeat("a", MaxMessageBytes+50) + "\r\n"
	go writeAll(t, srv, long)

	_, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected ErrLineTooLong, got nil")
	}
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestReadOverlongTagsFails(t *testing.T) {
	c, srv := pipeConn(t)
	tags := "@" + strings.Repeat("a", MaxTagBytes+1) + " PING :x\r\n"
	go writeAll(t, srv, tags)

	_, err := c.ReadMessage()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestReadLargeTagsWithinBudgetOK(t *testing.T) {
	c, srv := pipeConn(t)
	// A big-but-legal tag segment plus a short body must parse fine, proving the
	// budget is applied to tags and body separately, not to the whole line.
	tagVal := strings.Repeat("b", MaxTagBytes-2) // "a=" + value <= 8191
	go writeAll(t, srv, "@a="+tagVal+" PING :x\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "PING" || m.Tags.Get("a") != tagVal {
		t.Fatalf("got cmd %q tag len %d, want PING with full tag", m.Command, len(m.Tags.Get("a")))
	}
}

func TestReadSkipsLineWithEmbeddedNUL(t *testing.T) {
	// A line carrying an embedded NUL (mid-body, not the terminator) is rejected
	// by irc.Parse (ErrBadLineChar). The reader must treat that as a non-fatal
	// skip and keep delivering subsequent valid lines, not tear down.
	c, srv := pipeConn(t)
	go writeAll(t, srv, "PRIVMSG #x :bad\x00here\r\nPING :ok\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v (embedded-NUL line should be skipped, not fatal)", err)
	}
	if m.Command != "PING" || m.Param(0) != "ok" {
		t.Fatalf("got %+v, want PING ok after skipping bad line", m)
	}
}

func TestReadUnterminatedFloodRejected(t *testing.T) {
	// A peer that sends more than the hard line cap WITHOUT a terminating
	// newline must be rejected with ErrLineTooLong rather than making the reader
	// buffer unbounded data. This is the bounded-readQ guard.
	c, srv := pipeConn(t)
	flood := strings.Repeat("a", MaxLineBytes+4096) // no '\n' anywhere
	go writeAll(t, srv, flood)

	_, err := c.ReadMessage()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong for unterminated flood", err)
	}
}

func TestReadMaximalConformantLineOK(t *testing.T) {
	// A line at the maximum legal size (8191-byte tag segment + a 510-byte body)
	// must still parse: it is under the hard MaxLineBytes cap, so ReadSlice finds
	// the newline and the per-field budget check passes.
	c, srv := pipeConn(t)
	tagVal := strings.Repeat("b", MaxTagBytes-2) // "a=" + value == 8191
	body := "PRIVMSG #c :" + strings.Repeat("x", MaxMessageBytes-len("PRIVMSG #c :"))
	go writeAll(t, srv, "@a="+tagVal+" "+body+"\r\n")

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != "PRIVMSG" || m.Tags.Get("a") != tagVal {
		t.Fatalf("got cmd %q, want PRIVMSG with full tag", m.Command)
	}
}

func TestReadTimeoutFiresOnIdle(t *testing.T) {
	cliRaw, srvRaw := net.Pipe()
	c := NewConn(cliRaw, Options{ReadTimeout: 75 * time.Millisecond})
	t.Cleanup(func() { c.Close(); srvRaw.Close() })

	// The server side never sends anything; the idle read deadline must fire and
	// surface a (timeout) error rather than blocking the reader forever.
	_, err := c.ReadMessage()
	if err == nil {
		t.Fatal("expected a read-timeout error, got nil")
	}
	var ne net.Error
	if !errors.As(c.Err(), &ne) || !ne.Timeout() {
		t.Fatalf("Err = %v, want a net timeout error", c.Err())
	}
}

func TestIsTerminalClassifies(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"eof", io.EOF, true},
		{"closed", ErrClosed, true},
		{"tooLong", ErrLineTooLong, true},
		{"canceled", context.Canceled, true},
		{"netTimeout", &net.OpError{Op: "read", Err: timeoutErr{}}, true},
		{"plain", errors.New("nope"), false},
	}
	for _, tc := range cases {
		if got := IsTerminal(tc.err); got != tc.want {
			t.Errorf("IsTerminal(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// timeoutErr is a minimal net.Error that reports a timeout, for IsTerminal's
// network-error branch.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestReadTimeoutResetsOnActivity(t *testing.T) {
	cliRaw, srvRaw := net.Pipe()
	c := NewConn(cliRaw, Options{ReadTimeout: 150 * time.Millisecond})
	t.Cleanup(func() { c.Close(); srvRaw.Close() })

	// Send two lines spaced under the timeout apart: because the deadline resets
	// on each successful read, neither read should time out.
	go func() {
		writeAll(t, srvRaw, "PING :1\r\n")
		time.Sleep(90 * time.Millisecond)
		writeAll(t, srvRaw, "PING :2\r\n")
	}()

	for i := 0; i < 2; i++ {
		m, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage %d: %v (deadline should have reset)", i, err)
		}
		if m.Command != "PING" {
			t.Fatalf("got %+v, want PING", m)
		}
	}
}

func TestWriteMessageSerializes(t *testing.T) {
	c, srv := pipeConn(t)
	rd := bufio.NewReader(srv)

	if err := c.WriteMessage(&irc.Message{Command: "JOIN", Params: []string{"#go"}}); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if line != "JOIN #go\r\n" {
		t.Fatalf("got %q, want %q", line, "JOIN #go\r\n")
	}
}

func TestSendStripsAndAppendsCRLF(t *testing.T) {
	c, srv := pipeConn(t)
	rd := bufio.NewReader(srv)

	// Caller-supplied trailing CRLF must not be doubled.
	if err := c.Send("PONG :tok\r\n"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	line, err := rd.ReadString('\n')
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	if line != "PONG :tok\r\n" {
		t.Fatalf("got %q, want single CRLF", line)
	}
}

func TestSendRejectsInteriorNewline(t *testing.T) {
	c, _ := pipeConn(t)
	err := c.Send("PRIVMSG #a :hi\r\nQUIT")
	if err == nil {
		t.Fatal("expected rejection of interior CRLF, got nil")
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	c, _ := pipeConn(t)
	if err := c.Close(); err != nil {
		// net.Pipe close returns nil; tolerate either way.
		t.Logf("close err: %v", err)
	}
	err := c.Send("PING :x")
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
}

func TestCloseFlushesEnqueuedLines(t *testing.T) {
	cliRaw, srvRaw := net.Pipe()
	c := NewConn(cliRaw, Options{})
	rd := bufio.NewReader(srvRaw)

	// Read the line the server side expects, concurrently, so the writer's
	// synchronous net.Pipe write can complete during the Close grace window.
	got := make(chan string, 1)
	go func() {
		line, _ := rd.ReadString('\n')
		got <- line
	}()

	if err := c.Send("QUIT :leaving"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Logf("close err: %v", err)
	}
	srvRaw.Close()

	select {
	case line := <-got:
		if line != "QUIT :leaving\r\n" {
			t.Fatalf("got %q, want QUIT :leaving", line)
		}
	case <-time.After(time.Second):
		t.Fatal("enqueued line was not flushed on Close")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, _ := pipeConn(t)
	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Logf("close %d err: %v", i, err)
		}
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Fatalf("Err = %v, want ErrClosed", c.Err())
	}
}

func TestDialContextCancel(t *testing.T) {
	// Dial to a blackhole address with an already-cancelled context; Dial must
	// return promptly with an error rather than hang.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Dial(ctx, "tcp", "10.255.255.1:6667", Options{})
	if err == nil {
		t.Fatal("expected dial error with cancelled context")
	}
}

func TestDialAndExchangeOverListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverGot := make(chan string, 1)
	go func() {
		sc, err := ln.Accept()
		if err != nil {
			return
		}
		defer sc.Close()
		rd := bufio.NewReader(sc)
		line, _ := rd.ReadString('\n')
		serverGot <- strings.TrimRight(line, "\r\n")
		io.WriteString(sc, ":server 001 alice :Welcome\r\n")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, "tcp", ln.Addr().String(), Options{})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.Send("NICK alice"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case got := <-serverGot:
		if got != "NICK alice" {
			t.Fatalf("server got %q, want NICK alice", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive NICK")
	}

	m, err := c.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if m.Command != irc.RPL_WELCOME || m.Param(1) != "Welcome" {
		t.Fatalf("got %+v, want 001 ... :Welcome", m)
	}
}

func TestLimiterSeamInvoked(t *testing.T) {
	cliRaw, srvRaw := net.Pipe()
	lim := &countLimiter{}
	c := NewConn(cliRaw, Options{Limiter: lim})
	t.Cleanup(func() { c.Close(); srvRaw.Close() })

	rd := bufio.NewReader(srvRaw)
	go func() { rd.ReadString('\n') }()

	if err := c.Send("PING :x"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Give the writer a moment to consume the queued line.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if lim.calls() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Limiter.Wait was never called")
}

// countLimiter is a trivial Limiter that counts Wait calls, exercising the
// rate-limit seam without imposing any actual delay.
type countLimiter struct {
	mu sync.Mutex
	n  int
}

func (l *countLimiter) Wait(_ context.Context, _ string) error {
	l.mu.Lock()
	l.n++
	l.mu.Unlock()
	return nil
}

func (l *countLimiter) calls() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}
