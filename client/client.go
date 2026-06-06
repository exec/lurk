package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/exec/lurk/cap"
	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
	"github.com/exec/lurk/sasl"
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

	// trMu guards the session pointers tr and neg, which the reconnect
	// supervisor replaces (in startSession) while caller goroutines read them
	// (Send/WriteMessage/write/Close/Err/CapEnabled). It is held only around the
	// pointer load/store — never across the blocking network I/O those methods
	// perform — so a reconnect swap cannot tear an interface read mid-flight.
	trMu sync.Mutex

	// tr is the transport, set by Connect or ConnectConn (guarded by trMu).
	tr transport

	// neg drives capability negotiation during registration (guarded by trMu).
	neg *cap.Negotiator

	// conv drives the SASL exchange while one is in progress (nil otherwise). It
	// is owned by the run goroutine.
	conv *sasl.Conversation

	// ctcpRL rate-limits automatic CTCP replies so a peer flooding private CTCP
	// queries cannot induce a 1:1 outbound NOTICE flood (a reflection vector).
	ctcpRL ctcpLimiter

	// done is closed once the client has stopped for good (the final connection
	// ended and, when auto-reconnect is on, no further attempt will be made).
	done     chan struct{}
	doneOnce sync.Once

	// stop is closed by Close/Quit to ask the reconnect supervisor to stop
	// retrying and shut the client down. stopOnce guards the single close.
	stop     chan struct{}
	stopOnce sync.Once

	// dial establishes a fresh transport for (re)connection. Connect sets it to
	// the real net dialer; tests inject a scripted one. reconnectable records
	// whether this client may re-dial at all (false for ConnectConn, which has no
	// address).
	dial          func(ctx context.Context) (transport, error)
	reconnectable bool

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
	evClosed  bool // set when the stream is closed on final shutdown
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
		stop:       make(chan struct{}),
	}
	// Seed the self identity from the configured nick so Nick() is meaningful
	// before registration (Connect re-seeds it, and a 433 fallback / NICK change
	// updates it later).
	c.st.self = c.cfg.Nick
	return c
}

// transport returns the current session transport under trMu. The caller must
// release the lock (which this accessor already does) before performing any
// blocking I/O on the returned value, so reconnect can swap the pointer without
// waiting on network writes.
func (c *Client) transport() transport {
	c.trMu.Lock()
	defer c.trMu.Unlock()
	return c.tr
}

// negotiator returns the current session capability negotiator under trMu.
func (c *Client) negotiator() *cap.Negotiator {
	c.trMu.Lock()
	defer c.trMu.Unlock()
	return c.neg
}

// setSession atomically installs the transport and negotiator for a new session
// under trMu. It is called by startSession for the initial connect and for each
// reconnect attempt.
func (c *Client) setSession(tr transport, neg *cap.Negotiator) {
	c.trMu.Lock()
	defer c.trMu.Unlock()
	c.tr = tr
	c.neg = neg
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

// Highlights returns a copy of the configured extra highlight words.
func (c *Client) Highlights() []string {
	return append([]string(nil), c.cfg.Highlights...)
}

// CommonChannels returns the channels the client currently shares with nick —
// channels we are in where nick is a member — sorted. It is most useful read
// just before a QUIT/NICK removes or renames the member everywhere, to fan the
// notice out to the affected channel buffers.
func (c *Client) CommonChannels(nick string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.channelsWith(nick)
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

// StatusMsg returns the server's STATUSMSG prefix characters (e.g. "@+"): the
// membership symbols that may prefix a channel target to address only the
// members holding that status (per ISUPPORT). It returns "" when the server does
// not support status messages.
func (c *Client) StatusMsg() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.feat.StatusMsg()
}

// PrefixModes returns the channel-membership mode letters the server advertises
// via PREFIX, highest privilege first (e.g. "qaohv"); it defaults to "ov" when
// PREFIX is unadvertised. PrefixModes()[i] corresponds to PrefixSymbols()[i].
func (c *Client) PrefixModes() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.feat.PrefixModes()
}

// PrefixSymbols returns the membership prefix symbols aligned with PrefixModes
// (e.g. "~&@%+"); it defaults to "@+" when PREFIX is unadvertised.
func (c *Client) PrefixSymbols() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st.feat.PrefixSymbols()
}

// CapEnabled reports whether the named IRCv3 capability was successfully
// negotiated (server-ACKed and not since removed). It returns false before
// connecting or for caps the server did not grant. Cap names are case-sensitive.
func (c *Client) CapEnabled(name string) bool {
	neg := c.negotiator()
	if neg == nil {
		return false
	}
	return neg.IsEnabled(name)
}

// Send enqueues a raw, pre-serialized protocol line (without CRLF).
func (c *Client) Send(line string) error {
	tr := c.transport()
	if tr == nil {
		return errors.New("client: not connected")
	}
	return tr.Send(line)
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
	tr := c.transport()
	if tr == nil {
		return errors.New("client: not connected")
	}
	return tr.WriteMessage(m)
}

// write is the internal helper the registration/run code uses to emit a
// command with parameters.
func (c *Client) write(command string, params ...string) error {
	tr := c.transport()
	if tr == nil {
		return errors.New("client: not connected")
	}
	return tr.WriteMessage(&irc.Message{Command: command, Params: params})
}

// Connect dials the configured server, attaches the transport, and runs the
// registration handshake. It blocks until registration completes (RPL_WELCOME)
// or fails. On success the caller should call Run to process subsequent
// messages. The context governs the dial and the registration phase.
func (c *Client) Connect(ctx context.Context) error {
	if c.cfg.Server == "" && c.cfg.Dialer == nil {
		return errors.New("client: Config.Server or Config.Dialer is required")
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
	// The real net dialer, reused by the reconnect supervisor for re-dials. A
	// pre-set dialer (tests inject one) is kept so reconnect is hermetically
	// testable without real sockets.
	if c.dial == nil {
		c.dial = c.netDial
	}
	c.reconnectable = true
	co, err := c.dial(ctx)
	if err != nil {
		return err
	}
	return c.attachAndRegister(ctx, co)
}

// netDial dials and TLS-wraps the configured server, returning it as a transport.
// When Config.Dialer is set it is used to obtain the raw connection instead (the
// embedder's seam for custom transports and hermetic reconnect tests); the result
// is wrapped in the framing layer exactly as a normally-dialed connection.
func (c *Client) netDial(ctx context.Context) (transport, error) {
	if c.cfg.Dialer != nil {
		raw, err := c.cfg.Dialer(ctx)
		if err != nil {
			return nil, fmt.Errorf("client: dial: %w", err)
		}
		return conn.NewConn(raw, conn.Options{}), nil
	}
	co, err := conn.Dial(ctx, "tcp", c.cfg.Server, conn.Options{
		TLS:                c.cfg.TLS,
		InsecureSkipVerify: c.cfg.InsecureSkipVerify,
	})
	if err != nil {
		return nil, fmt.Errorf("client: connect: %w", err)
	}
	return co, nil
}

// ConnectConn attaches an already-established transport (for tests, or callers
// performing their own dialing) and runs the registration handshake. The Client
// takes ownership of tr and closes it on shutdown. Such a session is not
// reconnectable (there is no address to re-dial).
func (c *Client) ConnectConn(ctx context.Context, tr transport) error {
	return c.attachAndRegister(ctx, tr)
}

// attachAndRegister stores the transport, starts the run loop in the background,
// drives the opening handshake, and waits for registration. On success it starts
// the reconnect supervisor (when enabled) or, otherwise, a bridge that mirrors
// the single session's end onto the public done channel.
func (c *Client) attachAndRegister(ctx context.Context, tr transport) error {
	supervised := c.reconnectable && c.cfg.AutoReconnect

	// Every session runs on a private done channel: the supervisor owns it when
	// reconnect is on, otherwise a finalizer mirrors the single session's end onto
	// the public done channel AND the Events stream. Closing Events matters in both
	// modes — a consumer ranging over Events() must learn the session is over, not
	// just one watching Done().
	sessionDone := make(chan struct{})

	if err := c.startSession(tr, sessionDone, ctx); err != nil {
		// startSession already launched the run goroutine on sessionDone; whichever
		// mode we are in, finalize once it exits so done and Events both close.
		go c.finalizeWhenDone(sessionDone)
		return err
	}

	if supervised {
		go c.supervise(sessionDone)
	} else {
		go c.finalizeWhenDone(sessionDone)
	}
	return nil
}

// startSession attaches tr, starts its run goroutine on sessionDone, sends the
// opening handshake, and waits for registration to complete (or the context to
// cancel, or the client to be stopped). It is used both for the initial connect
// and for each reconnect attempt.
func (c *Client) startSession(tr transport, sessionDone chan struct{}, ctx context.Context) error {
	c.resetRegistration()
	c.setSession(tr, cap.NewNegotiator(c.cfg.Caps))

	// Seed the self nick before any I/O so state tracking has an identity.
	c.mu.Lock()
	c.st.self = c.cfg.Nick
	c.mu.Unlock()

	go c.run(sessionDone)

	if err := c.sendOpening(); err != nil {
		tr.Close()
		return fmt.Errorf("client: opening: %w", err)
	}

	select {
	case <-c.registered:
		return c.regErr
	case <-ctx.Done():
		tr.Close()
		return fmt.Errorf("client: registration: %w", ctx.Err())
	case <-c.stop:
		tr.Close()
		return errStopped
	}
}

// resetRegistration re-arms the one-shot registration state for a fresh session
// (the initial connect, or a reconnect). It reassigns registered/regOnce/regErr
// under c.mu; signalRegistered takes the same lock, so an external Close/Quit
// landing mid-reconnect cannot race this reassignment.
func (c *Client) resetRegistration() {
	c.mu.Lock()
	c.registered = make(chan struct{})
	c.regOnce = sync.Once{}
	c.regErr = nil
	c.conv = nil
	c.mu.Unlock()
}

// sendOpening emits the capability-negotiation start and the NICK/USER (and
// optional PASS) registration lines.
func (c *Client) sendOpening() error {
	tr, neg := c.transport(), c.negotiator()
	if tr == nil || neg == nil {
		return errors.New("client: not connected")
	}
	// CAP LS 302 must precede NICK/USER so the server holds 001 until CAP END.
	if err := tr.Send(neg.Start()); err != nil {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	c.regOnce.Do(func() {
		c.regErr = err
		close(c.registered)
	})
}

// Close tears down the connection and stops any reconnect supervisor. It is safe
// to call multiple times.
func (c *Client) Close() error {
	c.stopOnce.Do(func() { close(c.stop) })
	tr := c.transport()
	if tr == nil {
		return nil
	}
	// Unblock any pending registration wait with a closed-connection error.
	c.signalRegistered(errors.New("client: closed before registration completed"))
	return tr.Close()
}

// Err returns the transport's terminal error after the connection ends.
func (c *Client) Err() error {
	tr := c.transport()
	if tr == nil {
		return nil
	}
	return tr.Err()
}
