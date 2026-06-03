package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"lurk/cap"
	"lurk/conn"
	"lurk/irc"
	"lurk/sasl"
)

// transport is the subset of *conn.Conn the Client depends on. Keeping it as an
// interface lets tests substitute a scripted transport, and documents exactly
// what the client needs from the conn package.
type transport interface {
	// Messages delivers parsed inbound messages until the connection ends.
	Messages() <-chan *irc.Message
	// WriteMessage serializes and enqueues a message.
	WriteMessage(*irc.Message) error
	// Send enqueues a pre-serialized line (no CRLF).
	Send(line string) error
	// Close tears down the connection.
	Close() error
	// Err reports the terminal error after the connection ends.
	Err() error
}

// Client is a high-level IRCv3 client. It wires the transport, capability
// negotiation, SASL, server-feature parsing, and connection-state tracking
// behind an event-driven API.
//
// Lifecycle: build with New, then Connect (which dials and runs registration),
// then Run (which drives the event loop until the connection ends). Handlers
// registered with On / Handle* are invoked from the Run goroutine.
type Client struct {
	cfg  Config
	disp *dispatcher

	// tr is the transport, set by Connect or ConnectConn.
	tr transport

	// neg drives capability negotiation during registration.
	neg *cap.Negotiator

	// conv drives the SASL exchange while one is in progress (nil otherwise). It
	// is owned by the run goroutine.
	conv *sasl.Conversation

	// done is closed when the run goroutine exits (the connection ended).
	done chan struct{}

	// mu guards the fields below, which are read by snapshot accessors that may
	// be called from handler goroutines or other goroutines.
	mu sync.Mutex
	st *state

	// registered is closed once RPL_WELCOME has been processed.
	registered chan struct{}
	regOnce    sync.Once
	regErr     error // a fatal registration error (e.g. SASL failure), if any

	// Event stream (Events). evCh is created lazily on the first Events() call so
	// non-streaming users pay nothing. evMu guards creation and the publish path;
	// evDropped counts events discarded while the consumer was behind, surfaced
	// as a synthetic overflow event on the next successful publish.
	evMu      sync.Mutex
	evCh      chan Event
	evDropped int
}

// New returns a Client configured by cfg. It does not perform any I/O; call
// Connect to dial and register.
func New(cfg Config) *Client {
	c := &Client{
		cfg:        cfg.withDefaults(),
		disp:       newDispatcher(),
		st:         newState(),
		registered: make(chan struct{}),
		done:       make(chan struct{}),
	}
	// Seed the self identity from the configured nick so Nick() is meaningful
	// before registration (Connect re-seeds it, and a 433 fallback / NICK change
	// updates it later).
	c.st.self = c.cfg.Nick
	return c
}

// Nick returns the client's current nickname (which may differ from the
// configured nick after a 433 fallback or a NICK change).
func (c *Client) Nick() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.self
}

// Channels returns the names of the channels the client is currently in.
func (c *Client) Channels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.channelNames()
}

// Members returns a snapshot of the members of channel, or nil if the client is
// not in that channel. The returned slice and its Members are copies safe for
// the caller to retain.
func (c *Client) Members(channel string) []Member {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs := c.st.channel(channel)
	if cs == nil {
		return nil
	}
	out := make([]Member, 0, len(cs.members))
	for _, m := range cs.members {
		out = append(out, *m)
	}
	return out
}

// Topic returns the tracked topic of channel: the topic text, the nick/mask
// that last set it, and when it was set. It reports the values gathered from
// RPL_TOPIC (332), RPL_TOPICWHOTIME (333), and live TOPIC commands. For a
// channel with no known topic (or one the client is not tracking) it returns
// ("", "", zero time). setBy and at may be empty/zero even when text is set, if
// the server did not supply the who/when metadata.
func (c *Client) Topic(channel string) (text, setBy string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cs := c.st.channel(channel)
	if cs == nil {
		return "", "", time.Time{}
	}
	return cs.topic, cs.topicSetBy, cs.topicAt
}

// Network returns the server's advertised network name, or "" if not yet known.
func (c *Client) Network() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.feat.Network()
}

// CapEnabled reports whether the named IRCv3 capability was successfully
// negotiated (server-ACKed and not since removed). It returns false before
// connecting or for caps the server did not grant. Cap names are case-sensitive.
func (c *Client) CapEnabled(name string) bool {
	if c.neg == nil {
		return false
	}
	return c.neg.IsEnabled(name)
}

// Send enqueues a raw, pre-serialized protocol line (without CRLF).
func (c *Client) Send(line string) error {
	if c.tr == nil {
		return errors.New("client: not connected")
	}
	return c.tr.Send(line)
}

// SendRaw enqueues a raw, pre-serialized protocol line exactly as given (no
// CRLF). It is an alias for Send with a name that reads clearly at call sites
// that are deliberately sending hand-written wire lines (e.g. a /raw command in
// a REPL). The transport rejects lines containing interior CR/LF/NUL.
func (c *Client) SendRaw(line string) error {
	return c.Send(line)
}

// WriteMessage enqueues a message for sending.
func (c *Client) WriteMessage(m *irc.Message) error {
	if c.tr == nil {
		return errors.New("client: not connected")
	}
	return c.tr.WriteMessage(m)
}

// write is the internal helper the registration/run code uses to emit a
// command with parameters.
func (c *Client) write(command string, params ...string) error {
	return c.tr.WriteMessage(&irc.Message{Command: command, Params: params})
}

// Connect dials the configured server, attaches the transport, and runs the
// registration handshake. It blocks until registration completes (RPL_WELCOME)
// or fails. On success the caller should call Run to process subsequent
// messages. The context governs the dial and the registration phase.
func (c *Client) Connect(ctx context.Context) error {
	if c.cfg.Server == "" {
		return errors.New("client: Config.Server is required")
	}
	// Refuse to send credentials in the clear: SASL PLAIN is base64-encoded, not
	// encrypted, and PASS is sent verbatim, so either over a non-TLS link exposes
	// the secret to the network. AllowInsecureAuth is the deliberate opt-out.
	if !c.cfg.TLS && !c.cfg.AllowInsecureAuth {
		if c.cfg.SASL.Mechanism == "PLAIN" {
			return errors.New("client: refusing to send SASL PLAIN credentials over a plaintext connection; enable Config.TLS or set Config.AllowInsecureAuth")
		}
		if c.cfg.Pass != "" {
			return errors.New("client: refusing to send a PASS password over a plaintext connection; enable Config.TLS or set Config.AllowInsecureAuth")
		}
	}
	co, err := conn.Dial(ctx, "tcp", c.cfg.Server, conn.Options{
		TLS:                c.cfg.TLS,
		InsecureSkipVerify: c.cfg.InsecureSkipVerify,
	})
	if err != nil {
		return fmt.Errorf("client: connect: %w", err)
	}
	return c.attachAndRegister(ctx, co)
}

// ConnectConn attaches an already-established transport (for tests, or callers
// performing their own dialing) and runs the registration handshake. The Client
// takes ownership of tr and closes it on shutdown.
func (c *Client) ConnectConn(ctx context.Context, tr transport) error {
	return c.attachAndRegister(ctx, tr)
}

// attachAndRegister stores the transport, starts the run loop in the
// background, drives the opening handshake, and waits for registration.
func (c *Client) attachAndRegister(ctx context.Context, tr transport) error {
	c.tr = tr
	c.neg = cap.NewNegotiator(c.cfg.Caps)

	// Seed the self nick before any I/O so state tracking has an identity.
	c.mu.Lock()
	c.st.self = c.cfg.Nick
	c.mu.Unlock()

	// Start the run loop, which processes inbound messages (including the
	// registration burst) and signals completion via the registered channel.
	go c.run()

	// Kick off the opening sequence: CAP LS 302, (PASS), NICK, USER.
	if err := c.sendOpening(); err != nil {
		c.Close()
		return fmt.Errorf("client: opening: %w", err)
	}

	// Wait for registration to complete, the context to cancel, or the
	// connection to end.
	select {
	case <-c.registered:
		return c.regErr
	case <-ctx.Done():
		c.Close()
		return fmt.Errorf("client: registration: %w", ctx.Err())
	}
}

// sendOpening emits the capability-negotiation start and the NICK/USER (and
// optional PASS) registration lines.
func (c *Client) sendOpening() error {
	// CAP LS 302 must precede NICK/USER so the server holds 001 until CAP END.
	if err := c.tr.Send(c.neg.Start()); err != nil {
		return err
	}
	if c.cfg.Pass != "" {
		if err := c.write(irc.PASS, c.cfg.Pass); err != nil {
			return err
		}
	}
	if err := c.write(irc.NICK, c.cfg.Nick); err != nil {
		return err
	}
	// USER <user> 0 * :<realname>
	if err := c.write(irc.USER, c.cfg.User, "0", "*", c.cfg.Realname); err != nil {
		return err
	}
	return nil
}

// signalRegistered closes the registered channel exactly once, recording err as
// the registration outcome. A non-nil err means registration failed fatally.
func (c *Client) signalRegistered(err error) {
	c.regOnce.Do(func() {
		c.regErr = err
		close(c.registered)
	})
}

// Close tears down the connection. It is safe to call multiple times.
func (c *Client) Close() error {
	if c.tr == nil {
		return nil
	}
	// Unblock any pending registration wait with a closed-connection error.
	c.signalRegistered(errors.New("client: closed before registration completed"))
	return c.tr.Close()
}

// Err returns the transport's terminal error after the connection ends.
func (c *Client) Err() error {
	if c.tr == nil {
		return nil
	}
	return c.tr.Err()
}
