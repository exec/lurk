// Package server implements lurkd's client-facing IRC server: the TLS listener,
// the server-side registration / CAP / SASL responder, and the session
// multiplexer that bridges attached lurk clients to persistent upstream
// connections.
//
// It reuses the protocol packages (irc, conn, isupport) for both directions and
// is standard-library only — lurkd is a headless daemon and must never import
// the charm UI libraries that only tui/ and cmd/lurk are permitted to use.
// The TestNoCharmDependency guard in depguard_test.go enforces this.
//
// Phase 1 delivers: the Server type + serveConn seam, server-side CAP
// negotiation (LS/REQ/ACK/NAK/END), NICK/USER registration, and the welcome
// burst (001–005 with RPL_ISUPPORT including a stub BOUNCER_NETID).
//
// Phase 2 adds: server-side SASL PLAIN with PBKDF2 password verification,
// TLS-gate enforcement (AUTHENTICATE PLAIN rejected on non-TLS connections
// before any base64 decode), the authcid fallback parser, and a registration
// timeout that drops idle unauthenticated connections.
package server

import (
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/exec/lurk/backlog"
	"github.com/exec/lurk/conn"
	"github.com/exec/lurk/irc"
)

// serverName is the name lurkd reports as the originating server in the welcome
// burst and message sources. In a real deployment this would be the listener
// hostname; for Phase 1 a fixed constant is fine.
const serverName = "lurkd.local"

// serverVersion is the server version string included in RPL_YOURHOST (002) and
// RPL_MYINFO (004).
const serverVersion = "lurkd-1"

// maxSessions is the default cap on simultaneous accepted connections (both
// pre-registration and fully-registered). lurkd is a single-user daemon; a
// phone + laptop + a control client is a realistic maximum. The cap prevents a
// hostile authenticated-but-idle client from exhausting goroutines and file
// descriptors: each accepted connection costs ≥2 goroutines + 1 FD.
//
// Connections beyond the cap are closed immediately at the accept loop — before
// any goroutine is spawned — so the cap is enforced even for half-open
// (slow-loris) attempts.
//
// Tests override this via Server.maxSessions (a zero value means "use the
// default").
const defaultMaxSessions = 64

// maxCapRequests caps how many distinct capability names a client may include
// across all CAP REQ lines during registration. A conformant client sends at
// most the set it received in CAP LS; this bound guards against a hostile client
// flooding the registration loop with arbitrarily many names.
const maxCapRequests = 256

// maxNetworks is the maximum number of upstream networks lurkd will manage.
// It bounds BOUNCER ADDNETWORK against a hostile (but authenticated) client
// issuing unbounded ADDs, which would otherwise grow cfg.Networks without
// limit (unbounded JSON config on disk), start one upstream goroutine + FD per
// network, and make NextNetID's linear scan increasingly expensive (O(N²) over
// N successive ADDs). A cap of 256 is far above any realistic single-user
// configuration and keeps the worst-case config file well under ~200 KiB.
// Note: configs loaded from disk are not subject to this cap — it applies only
// at BOUNCER ADDNETWORK time, so an operator can pre-configure more networks
// via config.json without hitting the per-session limit.
const maxNetworks = 256

// registrationTimeout is the maximum time a client has to complete the
// registration handshake (CAP + NICK/USER + optional SASL + CAP END + welcome
// burst) before the connection is dropped. This bounds the goroutine lifetime
// of idle or slow-connecting hostile clients. The deadline is set on the raw
// net.Conn before registration and cleared once the welcome burst is sent.
const registrationTimeout = 30 * time.Second

// clientWriteTimeout bounds each flush of a session's outbound queue. Without
// it a registered session has no write deadline at all: a synchronous send
// (CHATHISTORY replay, state burst) to a client that stops reading parks the
// session goroutine indefinitely once the outbound queue and the TCP send
// window fill. With it, a stuck flush fails after this interval and the
// write-failure watchdog (see newSessionConn) tears the conn down so the
// session goroutine exits. 30s is generous for any live client while still
// bounding the goroutine lifetime against a stalled or hostile peer.
const clientWriteTimeout = 30 * time.Second

// maxSASLPayloadB64 is the maximum byte length of a single AUTHENTICATE payload
// line (base64 encoded). The IRCv3 SASL specification uses 400 bytes as the
// chunking threshold for multi-chunk payloads. A PLAIN payload for realistic
// credentials is far smaller. We cap at 400 (the spec threshold) to bound
// hostile input before any decode attempt while being more generous than
// necessary for any real username/password combination.
//
// Note: the IRC message body budget is 510 bytes (512 - CRLF), and
// "AUTHENTICATE " is 13 bytes, so payloads larger than 497 bytes cannot
// arrive over a standards-compliant connection anyway. Our cap at 400 is
// deliberately below that so it is the first check to fire.
const maxSASLPayloadB64 = 400

// advertisedCaps is the ordered list of capabilities lurkd advertises in
// CAP LS 302. It matches §7.1 of docs/LURKD-DESIGN.md: the realistic v1 set
// that lurk and gamja/goguma expect to see. SASL is advertised so clients
// initiate the exchange; the actual SASL handler lands in Phase 2.
//
// Caps with values use the "name=value" form (CAP LS 302 supports this);
// caps without values are just the cap name.
var advertisedCaps = []string{
	"sasl",
	"soju.im/bouncer-networks",
	"soju.im/bouncer-networks-notify",
	"draft/chathistory",
	"draft/event-playback",
	"server-time",
	"batch",
	"message-tags",
	"labeled-response",
	"cap-notify",
	"echo-message",
	"away-notify",
	"account-notify",
	"extended-join",
	"multi-prefix",
	"chghost",
	"setname",
}

// advertisedCapsSet is the same set as a map for O(1) lookup in CAP REQ
// validation.
var advertisedCapsSet = func() map[string]bool {
	m := make(map[string]bool, len(advertisedCaps))
	for _, c := range advertisedCaps {
		// Strip any "=value" suffix for the lookup key.
		name, _, _ := strings.Cut(c, "=")
		m[name] = true
	}
	return m
}()

// capLSPayload is the space-joined cap list sent as the CAP LS reply body.
// Pre-built once at startup.
var capLSPayload = strings.Join(advertisedCaps, " ")

// Server is a lurkd listener. It holds its configuration and accepts connections
// from clients. Each accepted connection is served by a session goroutine
// spawned from Serve.
//
// Server is safe for concurrent use from multiple goroutines once constructed.
type Server struct {
	cfg *Config

	// regTimeout bounds the registration handshake (see registrationTimeout).
	// New defaults it to registrationTimeout; tests set a short value to exercise
	// the idle-client drop without waiting the full production timeout.
	regTimeout time.Duration

	// writeTimeout bounds each flush of a session's outbound queue (see
	// clientWriteTimeout). New defaults it to clientWriteTimeout; tests set a
	// short value to exercise the stuck-client drop without waiting the full
	// production timeout.
	writeTimeout time.Duration

	// maxSessions is the concurrent-connection cap. Zero means use
	// defaultMaxSessions. Tests set a small value to exercise the rejection
	// path without opening many real connections.
	maxSessions int

	// activeSessions is the current count of accepted, not-yet-closed
	// connections. It is incremented atomically at accept and decremented
	// when serveConn returns. The accept loop checks it before spawning.
	activeSessions atomic.Int64

	// store is the durable backlog store used to serve CHATHISTORY queries.
	// Set via WithStore. If nil, CHATHISTORY returns an empty batch.
	store *backlog.Store

	// mgr is the upstream session manager. Set via WithManager before Serve.
	// Used by BOUNCER ADDNETWORK/DELNETWORK to start/stop upstreams at runtime.
	mgr *Manager

	// cfgPath is the on-disk config path used by ADDNETWORK/CHANGENETWORK/DELNETWORK
	// to persist mutations. Set via WithConfigPath.
	cfgPath string

	// cfgMu guards ALL access to cfg.Networks. The bouncer is multi-client: a
	// phone and a laptop may both sit on the control context, so two concurrent
	// control sessions can race on cfg.Networks (read in BIND validation and
	// LISTNETWORKS; mutated by ADD/CHANGE/DELNETWORK). Every read or mutation of
	// cfg.Networks must hold cfgMu. The locked helpers (snapshotNetworks,
	// findNetworkByID, addNetwork, changeNetwork, delNetwork) in bouncer.go are
	// the only places that touch cfg.Networks; blocking I/O (mgr.Add/Remove,
	// sending replies, broadcasting notify) is always done outside the lock.
	cfgMu sync.Mutex

	// controlMu guards the controlSessions set.
	controlMu sync.RWMutex
	// controlSessions is the set of currently-connected, registered, unbound
	// (netid==0) sessions that have enabled soju.im/bouncer-networks-notify.
	// Used to broadcast bouncer-networks-notify on ADD/CHANGE/DEL.
	controlSessions map[*session]struct{}

	// boundMu guards boundSessions. It is a separate mutex from controlMu so
	// fanout (high-frequency, upstream goroutine) does not contend with the
	// low-frequency control-session registry mutations.
	boundMu sync.RWMutex
	// boundSessions maps netid → set of sessions currently bound to that netid.
	// A session is added on registration (end of sendWelcome, netid != 0) and
	// removed on disconnect (defer in run). Guarded by boundMu; never hold
	// boundMu across conn I/O.
	boundSessions map[int]map[*session]struct{}

	// cursors is the optional per-client cursor store. When set, fanout advances
	// the cursor for the delivering session as each PRIVMSG/NOTICE is delivered.
	// Flushed on detach and periodically. Set via WithCursorStore.
	cursors *CursorStore
}

// WithStore configures the Server to use the given backlog store for
// CHATHISTORY queries. Must be called before Serve/serveConn.
func (s *Server) WithStore(store *backlog.Store) {
	s.store = store
}

// WithManager configures the Server to use mgr for runtime upstream
// management (BOUNCER ADDNETWORK / DELNETWORK). Must be called before
// Serve/serveConn.
func (s *Server) WithManager(mgr *Manager) {
	s.mgr = mgr
}

// WithConfigPath sets the on-disk path that BOUNCER ADD/CHANGE/DELNETWORK
// persist mutations to. Must be called before Serve/serveConn.
func (s *Server) WithConfigPath(path string) {
	s.cfgPath = path
}

// WithCursorStore configures the Server to use the given per-client cursor
// store. When set, fanout advances cursors as PRIVMSG/NOTICE messages are
// delivered to bound sessions, and the cursor store is flushed on clean detach.
// Must be called before Serve/serveConn.
func (s *Server) WithCursorStore(cs *CursorStore) {
	s.cursors = cs
}

// New builds a Server from cfg. cfg must not be nil.
func New(cfg *Config) *Server {
	s := &Server{
		cfg:             cfg,
		regTimeout:      registrationTimeout,
		writeTimeout:    clientWriteTimeout,
		controlSessions: make(map[*session]struct{}),
	}
	s.initBoundSessions()
	return s
}

// registerControlSession adds sess to the control-session registry. Called
// after registration completes for an unbound session that enabled
// soju.im/bouncer-networks-notify.
func (s *Server) registerControlSession(sess *session) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	s.controlSessions[sess] = struct{}{}
}

// unregisterControlSession removes sess from the control-session registry.
// Safe to call even if sess was never registered (e.g. bound sessions).
func (s *Server) unregisterControlSession(sess *session) {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	delete(s.controlSessions, sess)
}

// broadcastNetworkNotify sends a bouncer-networks-notify NETWORK message to all
// control sessions (other than the originator, if any). Only sessions that
// enabled soju.im/bouncer-networks-notify are in controlSessions (the gate in
// sendWelcome checks the cap before calling registerControlSession), so no
// per-session cap check is needed here — and deliberately avoided to prevent
// a data race on capEnabled across goroutines. origin may be nil.
func (s *Server) broadcastNetworkNotify(origin *session, msg *irc.Message) {
	s.controlMu.RLock()
	sessions := make([]*session, 0, len(s.controlSessions))
	for sess := range s.controlSessions {
		if sess != origin {
			sessions = append(sessions, sess)
		}
	}
	s.controlMu.RUnlock()

	// Serialize once and deliver the same line to every control session with a
	// non-blocking enqueue. A slow or stuck control client must not be able to
	// stall the goroutine of the session that issued the network change (the
	// live fanout path uses the same drop-on-full policy for the same reason);
	// blocking sess.send here would let one wedged notify recipient wedge an
	// unrelated client's ADDNETWORK/CHANGENETWORK/DELNETWORK handler.
	line, err := msg.Serialize()
	if err != nil {
		log.Printf("server: broadcast notify serialize: %v", err)
		return
	}
	for _, sess := range sessions {
		if ok, err := sess.conn.TrySend(line); err != nil {
			// The session may have disconnected between snapshot and send.
			// Log only; do not tear down the broadcasting session.
			log.Printf("server: broadcast notify to session: %v", err)
		} else if !ok {
			log.Printf("server: broadcast notify drop: control session queue full")
		}
	}
}

// NewListener starts a TCP listener on addr. If the config's Listen block
// has a TLS certificate+key pair, the listener is wrapped with TLS and all
// accepted connections are TLS — plaintext is never served from a TLS-configured
// listener. If no cert/key is configured, a plain TCP listener is returned and
// a dev warning is printed; AUTHENTICATE PLAIN will still be refused on such
// connections by the TLS gate in serveConn.
func NewListener(addr string, cfg *Config) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen %s: %w", addr, err)
	}

	if cfg.Listen.TLSCert != "" && cfg.Listen.TLSKey != "" {
		tlsCfg, err := loadTLSConfig(cfg.Listen.TLSCert, cfg.Listen.TLSKey)
		if err != nil {
			_ = ln.Close()
			return nil, err
		}
		return tls.NewListener(ln, tlsCfg), nil
	}

	// No TLS material configured. Allow plain-TCP for development (so Phase 1
	// tests still pass) but log a clear warning. The AUTHENTICATE PLAIN TLS gate
	// is still enforced per-session, so no credentials can be extracted over plain
	// connections even in dev mode.
	log.Printf("server: WARNING: no TLS cert/key configured; listening on plain TCP — AUTHENTICATE PLAIN will be refused on all connections")
	return ln, nil
}

// loadTLSConfig loads a TLS configuration from a PEM certificate and key file.
func loadTLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("server: load TLS key pair: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// sessionLimit returns the effective concurrent-connection cap, honouring the
// test override in s.maxSessions.
func (s *Server) sessionLimit() int64 {
	if s.maxSessions > 0 {
		return int64(s.maxSessions)
	}
	return defaultMaxSessions
}

// Serve accepts connections from ln until it returns an error. Each accepted
// connection is handed to serveConn in a new goroutine. Serve blocks until ln
// is closed. It is intended for cmd/lurkd's main loop; tests drive serveConn
// directly without a real socket.
//
// Connections beyond sessionLimit are closed immediately at the accept loop —
// before any goroutine is spawned — guarding against goroutine and FD
// exhaustion by a hostile client that opens many connections.
func (s *Server) Serve(ln net.Listener) error {
	// backoff grows on consecutive transient Accept errors (e.g. EMFILE under
	// FD pressure) and resets on a successful accept, so a temporary condition
	// no longer tears down the whole listener.
	const maxAcceptBackoff = time.Second
	var backoff time.Duration
	for {
		nc, err := ln.Accept()
		if err != nil {
			// A closed listener is the normal shutdown signal — stop cleanly.
			if errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("server: accept: %w", err)
			}
			// Treat any other error as transient: log, back off, and retry
			// rather than killing the accept loop.
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff < maxAcceptBackoff {
				backoff *= 2
			}
			log.Printf("server: accept error (retrying in %v): %v", backoff, err)
			time.Sleep(backoff)
			continue
		}
		backoff = 0

		// Enforce the concurrent-session cap before spawning a goroutine.
		// We increment first and check: if the new count exceeds the limit,
		// decrement and close the connection without allocating anything else.
		if s.activeSessions.Add(1) > s.sessionLimit() {
			s.activeSessions.Add(-1)
			log.Printf("server: session limit (%d) reached; closing %s", s.sessionLimit(), nc.RemoteAddr())
			_ = nc.Close()
			continue
		}

		go func() {
			defer s.activeSessions.Add(-1)
			if err := s.serveConn(nc); err != nil {
				log.Printf("server: conn %s: %v", nc.RemoteAddr(), err)
			}
		}()
	}
}

// serveConn wraps nc in a framed conn.Conn, runs the registration handshake
// (CAP negotiation + NICK/USER + optional SASL + welcome burst), and then
// enters the post-registration dispatch loop. It returns when the connection
// closes or an unrecoverable error occurs.
//
// serveConn is the primary testability seam: tests hand it the server end of a
// net.Pipe pair (via dialPipe) instead of a real socket, so the full protocol
// surface is exercised without network I/O. For TLS-path tests the isTLS flag
// is set via serveConnTLS (which takes the flag explicitly) so tests can use
// the non-TLS net.Pipe transport while still exercising the TLS-gated code
// paths, without needing a real TLS handshake.
func (s *Server) serveConn(nc net.Conn) error {
	// Detect TLS at the transport layer. This is the code invariant: if the
	// accept loop wrapped the listener with tls.NewListener, nc is a *tls.Conn.
	_, isTLS := nc.(*tls.Conn)
	return s.serveConnInternal(nc, isTLS)
}

// serveConnInternalNetid is the Phase 5 test seam: identical to
// serveConnInternal but also sets session.netid before the run loop. This
// allows CHATHISTORY tests to bind a session to a specific netid without
// going through Phase 6's BOUNCER BIND flow.
//
// This method is intentionally unexported and used only from within the server
// package (by chathistory_test.go). Phase 6 will have BOUNCER BIND set
// session.netid from within the run loop; this seam is only for Phase 5 tests.
func (s *Server) serveConnInternalNetid(nc net.Conn, isTLS bool, netid int) error {
	regTimeout := s.regTimeout
	if regTimeout <= 0 {
		regTimeout = registrationTimeout
	}
	if err := nc.SetDeadline(time.Now().Add(regTimeout)); err != nil {
		log.Printf("server: set registration deadline: %v", err)
	}

	c, stopWatchdog := s.newSessionConn(nc)
	defer c.Close()
	defer stopWatchdog()

	sess := &session{
		srv:   s,
		conn:  c,
		nc:    nc,
		isTLS: isTLS,
		netid: netid,
	}
	return sess.run()
}

// sessionWriteTimeout returns the per-flush write deadline for client-session
// conns: s.writeTimeout when set, clientWriteTimeout otherwise. It exists so
// a zero-valued Server (constructed without New, as some tests do) still gets
// the production write deadline rather than none.
func (s *Server) sessionWriteTimeout() time.Duration {
	if s.writeTimeout > 0 {
		return s.writeTimeout
	}
	return clientWriteTimeout
}

// newSessionConn wraps nc in a framed conn.Conn configured with the session
// write deadline, plus a watchdog that turns a write failure into a full conn
// teardown. Registered sessions otherwise have no write deadline at all: a
// synchronous send (CHATHISTORY replay, state burst) to a client that stops
// reading would park the session goroutine forever once the outbound queue
// and the transport fill. The conn-level WriteTimeout bounds each flush of
// the outbound queue; when a flush misses the deadline (or fails for any
// other reason) the watchdog closes the conn, which closes the transport and
// unblocks the session goroutine wherever it is parked — in ReadMessage or in
// a send blocked on the full queue — so the session exits instead of leaking.
//
// The returned stop function must be deferred by the caller so the watchdog
// goroutine is released when the session ends normally.
func (s *Server) newSessionConn(nc net.Conn) (*conn.Conn, func()) {
	fc := &writeFailConn{Conn: nc, failed: make(chan struct{})}
	c := conn.NewConn(fc, conn.Options{WriteTimeout: s.sessionWriteTimeout()})
	stop := make(chan struct{})
	go func() {
		select {
		case <-fc.failed:
			// The client has stopped reading (deadline miss) or the transport
			// broke. Close is safe to call from here: the conn's writer
			// goroutine has already returned from its failed write, so Close
			// does not wait on a goroutine that is blocked behind us.
			_ = c.Close()
		case <-stop:
		}
	}()
	return c, func() { close(stop) }
}

// writeFailConn wraps a net.Conn and signals the first Write error by closing
// the failed channel. All Writes on a session conn happen on the conn.Conn
// writer goroutine; pairing this signal with the watchdog in newSessionConn
// propagates a write-deadline failure into a full session teardown (the conn
// package records the failure internally but does not close the transport,
// so without the watchdog the session's reader would stay parked).
type writeFailConn struct {
	net.Conn
	once   sync.Once
	failed chan struct{}
}

// Write implements net.Conn. The first failing write closes the failed
// channel; the error is returned unchanged.
func (w *writeFailConn) Write(p []byte) (n int, err error) {
	n, err = w.Conn.Write(p)
	if err != nil {
		w.once.Do(func() { close(w.failed) })
	}
	return n, err
}

// serveConnInternal is the internal implementation shared by serveConn and
// the test seam. isTLS is passed explicitly so tests can set it independently
// of the transport type.
func (s *Server) serveConnInternal(nc net.Conn, isTLS bool) error {
	// Set a registration deadline. If the client does not complete the full
	// handshake within registrationTimeout, the read deadline fires and the
	// connection is dropped. The deadline is cleared once the welcome burst is
	// sent (in sendWelcome), so long-lived registered sessions are unaffected.
	regTimeout := s.regTimeout
	if regTimeout <= 0 {
		regTimeout = registrationTimeout
	}
	if err := nc.SetDeadline(time.Now().Add(regTimeout)); err != nil {
		// Best-effort; proceed even if the deadline cannot be set (e.g. net.Pipe
		// does not support deadlines in all test environments).
		log.Printf("server: set registration deadline: %v", err)
	}

	c, stopWatchdog := s.newSessionConn(nc)
	defer c.Close()
	defer stopWatchdog()

	sess := &session{
		srv:   s,
		conn:  c,
		nc:    nc,
		isTLS: isTLS,
	}
	return sess.run()
}

// capPhase represents where a session is in the CAP negotiation lifecycle.
type capPhase int

const (
	// capPhasePreLS: no CAP LS has been seen yet. The client may send NICK/USER
	// without doing CAP at all (legacy clients).
	capPhasePreLS capPhase = iota
	// capPhaseListing: CAP LS has been seen. The session is waiting for CAP REQ
	// lines and/or CAP END.
	capPhaseListing
	// capPhaseDone: CAP END has been processed (or no CAP was used). Registration
	// may complete once NICK and USER are also known.
	capPhaseDone
)

// saslState is the SASL sub-state within a CAP negotiation session.
type saslState int

const (
	// saslStateIdle: no AUTHENTICATE has been sent yet.
	saslStateIdle saslState = iota
	// saslStateAwaitPayload: "AUTHENTICATE PLAIN" accepted; waiting for the
	// base64 payload line.
	saslStateAwaitPayload
	// saslStateDone: SASL exchange completed (success or failure).
	saslStateDone
)

// session holds all per-connection state for one attached client. It is created
// by serveConn and lives on a single goroutine; no locking is needed.
type session struct {
	srv  *Server
	conn *conn.Conn
	nc   net.Conn // underlying net.Conn, used to clear the registration deadline

	// isTLS is true when the underlying transport is a *tls.Conn. The TLS gate
	// in handleAUTHENTICATE uses this flag to refuse PLAIN authentication on
	// non-TLS connections before any base64 decode.
	isTLS bool

	// Registration state.
	nick     string
	user     string
	real     string
	welcomed bool // true once the 001 welcome burst has been sent (exactly once)

	// CAP negotiation state.
	capPhase    capPhase
	capEnabled  map[string]bool // caps ACKed in this session
	capReqCount int             // total distinct cap names seen in REQ lines

	// SASL sub-state (nested within the CAP window).
	saslState  saslState
	saslAuthed bool           // true if SASL authentication succeeded
	saslParsed *ParsedAuthcid // parsed authcid from the PLAIN payload (set on success)

	// netid identifies which upstream network this session is bound to.
	// 0 means the session is on the control context (unbound). Phase 6's
	// BOUNCER BIND sets this field; tests set it directly to exercise
	// CHATHISTORY without going through the full bind flow.
	netid int

	// labelMu guards pendingLabels. It is held briefly by both the session's
	// own goroutine (when pushing a new pending label on relay) and by the
	// fanout goroutine (when matching and consuming the head label on echo).
	// Never hold labelMu across conn I/O.
	labelMu sync.Mutex
	// pendingLabels is a bounded FIFO of @label-tagged sends awaiting their
	// upstream echo. When the upstream echoes a matching message, fanout pops
	// the label and attaches it to that session's copy of the fanned-out
	// message (the self-send rule [B#11]). Other sessions see no label.
	// The slice is at most pendingLabelFIFOSize entries; the oldest is evicted
	// when full (the client sent so many unlabeled-echoed messages that the
	// FIFO wrapped — graceful degradation: no label on the excess echo).
	pendingLabels []pendingLabel
}

// run is the session goroutine's main loop. It processes messages until the
// connection closes.
func (s *session) run() error {
	s.capEnabled = make(map[string]bool)

	// Deregister from both registries on exit. The defers are placed before
	// any registration call so they fire even if registration never completes —
	// both unregister helpers are no-ops for sessions that were never registered.
	defer s.srv.unregisterControlSession(s)
	defer s.srv.unregisterBoundSession(s)

	for {
		msg, err := s.conn.ReadMessage()
		if err != nil {
			// Normal EOF / close is not an error worth logging.
			return nil
		}
		if err := s.dispatch(msg); err != nil {
			return err
		}
	}
}

// dispatch routes a single incoming message to the appropriate handler.
// An error from dispatch tears down the session.
func (s *session) dispatch(msg *irc.Message) error {
	cmd := strings.ToUpper(msg.Command)
	switch cmd {
	case irc.CAP:
		return s.handleCAP(msg)
	case irc.AUTHENTICATE:
		return s.handleAUTHENTICATE(msg)
	case irc.NICK:
		return s.handleNICK(msg)
	case irc.USER:
		return s.handleUSER(msg)
	case irc.PING:
		// Always handled locally: respond with a PONG (even for bound sessions,
		// per the bouncer design — PING keepalives are between client and lurkd,
		// not forwarded to the upstream).
		return s.handlePING(msg)
	case irc.QUIT:
		// QUIT from a bound client closes the client's connection to lurkd, not
		// the upstream. The upstream stays connected (bouncer semantics).
		_ = s.conn.Close()
		return nil
	case "BOUNCER":
		return s.handleBOUNCER(msg)
	case "CHATHISTORY":
		if !s.registered() {
			return s.sendNumeric(irc.ERR_NOTREGISTERED, s.clientNick(), "You have not registered")
		}
		return s.handleCHATHISTORY(msg)
	default:
		if !s.registered() {
			return s.sendNumeric(irc.ERR_NOTREGISTERED, s.clientNick(), "You have not registered")
		}
		// For a bound session: relay to the upstream. For an unbound/control
		// session: silently ignore (no upstream to forward to).
		if s.netid != 0 {
			return s.relayToUpstream(msg)
		}
		return nil
	}
}

// registered reports whether the client has completed the registration
// handshake (CAP phase done, NICK and USER known, and welcome burst sent).
func (s *session) registered() bool {
	return s.welcomed
}

// ─── CAP handlers ────────────────────────────────────────────────────────────

// handleCAP dispatches a CAP message to the appropriate sub-handler.
func (s *session) handleCAP(msg *irc.Message) error {
	sub := strings.ToUpper(msg.Param(0))
	switch sub {
	case irc.CAP_LS:
		return s.handleCAPLS(msg)
	case irc.CAP_REQ:
		return s.handleCAPREQ(msg)
	case irc.CAP_END:
		return s.handleCAPEND()
	case irc.CAP_LIST:
		return s.handleCAPLIST()
	default:
		// Unknown CAP subcommand: 410 ERR_INVALIDCAPCMD per the spec.
		return s.send(&irc.Message{
			Source:  serverName,
			Command: "410",
			Params:  []string{s.clientNick(), sub, "Invalid CAP command"},
		})
	}
}

// handleCAPLS responds to "CAP LS [302]". lurkd always uses the 302 multiline
// format (single-line here since our cap list fits; a future expansion would
// chunk with the * continuation marker). This transitions to capPhaseListing.
func (s *session) handleCAPLS(msg *irc.Message) error {
	// Regardless of what version the client sent, respond with the full list.
	// For Phase 1 the payload fits in one line; a * continuation is not needed.
	s.capPhase = capPhaseListing
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.CAP,
		Params:  []string{s.clientNick(), irc.CAP_LS, capLSPayload},
	})
}

// handleCAPREQ processes "CAP REQ :<cap> [<cap> ...]". Per the IRCv3 CAP spec
// (section 3.2), a REQ is all-or-nothing: if every named cap is supported, send
// ACK with the same list; if any single cap is not supported, send NAK with the
// same list and enable nothing. Requesting before CAP LS is accepted but
// logged; it cannot cause a panic.
func (s *session) handleCAPREQ(msg *irc.Message) error {
	// The cap list is in the trailing parameter. Per spec the list is
	// space-separated, cap names may carry a leading '-' (disable request).
	// irc.Parse already strips the ':' from trailing params, so msg.Param(1)
	// is the raw space-separated cap name list.
	payload := msg.Param(1)
	if payload == "" {
		// Empty REQ: NAK with empty payload (do not enable anything).
		return s.send(&irc.Message{
			Source:  serverName,
			Command: irc.CAP,
			Params:  []string{s.clientNick(), irc.CAP_NAK, ""},
		})
	}

	names := strings.Fields(payload)

	// Bound the total cap names seen so far to guard against flooding.
	s.capReqCount += len(names)
	if s.capReqCount > maxCapRequests {
		return s.send(&irc.Message{
			Source:  serverName,
			Command: irc.CAP,
			Params:  []string{s.clientNick(), irc.CAP_NAK, payload},
		})
	}

	// Validate all-or-nothing: every name must be supported.
	for _, raw := range names {
		name := raw
		if strings.HasPrefix(name, "-") {
			name = name[1:] // disable-cap prefix
		}
		if !advertisedCapsSet[name] {
			// At least one unsupported cap: NAK the whole request.
			return s.send(&irc.Message{
				Source:  serverName,
				Command: irc.CAP,
				Params:  []string{s.clientNick(), irc.CAP_NAK, payload},
			})
		}
	}

	// All caps are supported: apply them and ACK.
	for _, raw := range names {
		if strings.HasPrefix(raw, "-") {
			delete(s.capEnabled, raw[1:])
		} else {
			s.capEnabled[raw] = true
		}
	}
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.CAP,
		Params:  []string{s.clientNick(), irc.CAP_ACK, payload},
	})
}

// handleCAPEND processes "CAP END", which closes the negotiation window. After
// this point the client may not send new CAP REQ lines (well, it can, but any
// such line will be processed and responded to normally — the spec does not
// require us to reject them). If NICK and USER are already known, the welcome
// burst is emitted immediately.
//
// If the server has bouncer authentication configured and the client has not
// successfully authenticated via SASL, the connection is closed. This enforces
// that lurkd requires authentication when BouncerAuth is configured; an
// unauthenticated CAP END is not a valid registration path in that case.
func (s *session) handleCAPEND() error {
	// If bouncer auth is configured and the client skipped SASL (or failed it),
	// reject the registration — close the connection without a welcome burst.
	if s.srv.cfg.BouncerAuth.User != "" && !s.saslAuthed {
		_ = s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "Authentication required")
		_ = s.conn.Close()
		return nil
	}
	s.capPhase = capPhaseDone
	return s.maybeWelcome()
}

// handleCAPLIST responds to "CAP LIST" by returning the currently-enabled caps
// for this session. This is advisory and may be called at any time.
func (s *session) handleCAPLIST() error {
	var caps []string
	for c := range s.capEnabled {
		caps = append(caps, c)
	}
	// Send the enabled list (may be empty).
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.CAP,
		Params:  []string{s.clientNick(), irc.CAP_LIST, strings.Join(caps, " ")},
	})
}

// ─── SASL / AUTHENTICATE handlers ────────────────────────────────────────────

// handleAUTHENTICATE processes AUTHENTICATE messages. It implements the
// server side of the IRCv3 SASL exchange:
//
//  1. Client: AUTHENTICATE PLAIN
//  2. Server: AUTHENTICATE +   (challenge: empty, meaning "send now")
//  3. Client: AUTHENTICATE <base64(authzid\0authcid\0passwd)>
//  4. Server: 900 + 903 on success, or 904 on failure
//
// Aborting: AUTHENTICATE * at any point → 906 ERR_SASLABORTED.
// Unknown mechanism → 908 RPL_SASLMECHS.
// TLS gate: if isTLS is false, AUTHENTICATE PLAIN is refused with 904
// BEFORE any base64 decode (§6.2).
// Payload size gate: a base64 payload longer than maxSASLPayloadB64 is
// refused with 904 BEFORE any decode (IRCv3 400-byte chunking rule).
func (s *session) handleAUTHENTICATE(msg *irc.Message) error {
	param := msg.Param(0)

	// Client abort: "AUTHENTICATE *" at any sub-state. Per the IRCv3 SASL spec,
	// the server SHOULD reply with 906 ERR_SASLABORTED.
	if param == "*" {
		s.saslState = saslStateDone
		return s.sendNumeric(irc.ERR_SASLABORTED, s.clientNick(), "SASL authentication aborted")
	}

	switch s.saslState {
	case saslStateIdle:
		return s.handleAuthenticateMech(param)
	case saslStateAwaitPayload:
		return s.handleAuthenticatePayload(param)
	case saslStateDone:
		// Re-authentication after a completed exchange is rejected.
		return s.sendNumeric(irc.ERR_SASLALREADY, s.clientNick(), "You have already authenticated using SASL")
	default:
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL error")
	}
}

// handleAuthenticateMech processes the mechanism selection line
// "AUTHENTICATE <MECHANISM>". Only PLAIN is supported; any other mechanism
// name is rejected with 908 listing PLAIN.
func (s *session) handleAuthenticateMech(mech string) error {
	mech = strings.ToUpper(mech)

	if mech != "PLAIN" {
		// Unknown or unsupported mechanism: list what we support.
		return s.send(&irc.Message{
			Source:  serverName,
			Command: irc.RPL_SASLMECHS,
			Params:  []string{s.clientNick(), "PLAIN", "are available SASL mechanisms"},
		})
	}

	// TLS gate [B#2]: refuse PLAIN on a non-TLS connection BEFORE any decode.
	// This is checked after the mechanism is known but before any challenge or
	// payload handling, so a non-TLS client never gets a useful oracle — even
	// if they somehow supply a valid credential, the 904 fires unconditionally.
	if !s.isTLS {
		s.saslState = saslStateDone
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL PLAIN requires TLS")
	}

	// PLAIN selected on a TLS connection: send the empty challenge "AUTHENTICATE +"
	// to tell the client to send the payload immediately.
	s.saslState = saslStateAwaitPayload
	return s.send(&irc.Message{
		Command: irc.AUTHENTICATE,
		Params:  []string{"+"},
	})
}

// handleAuthenticatePayload processes the base64 PLAIN payload
// "AUTHENTICATE <base64>". It decodes, verifies the credential, and emits
// 900+903 or 904.
//
// The TLS gate was already enforced in handleAuthenticateMech; by the time we
// reach here isTLS is always true. The explicit re-check below is a defence-in-
// depth assertion that must not be reachable on non-TLS sessions.
func (s *session) handleAuthenticatePayload(payload string) error {
	s.saslState = saslStateDone // consume the sub-state regardless of outcome

	// Defence-in-depth: the TLS gate must have fired before we ever reach here.
	// This check should be unreachable on a non-TLS session, but an adversarial
	// reviewer correctly notes that defence-in-depth demands it.
	if !s.isTLS {
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL PLAIN requires TLS")
	}

	// Payload size gate: cap before any allocation or decode. The IRCv3 SASL
	// spec uses 400 bytes as a chunking threshold; we use a slightly more
	// generous cap to handle edge cases while still bounding hostile input.
	if len(payload) > maxSASLPayloadB64 {
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL payload too long")
	}

	// Decode base64. Do NOT include the decoded bytes in any error message or log.
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL PLAIN: invalid base64")
	}

	// Parse the PLAIN format: authzid\0authcid\0passwd.
	// Split on NUL bytes — exactly three fields.
	parts := splitNUL(raw)
	if len(parts) != 3 {
		// Malformed payload: do not log any decoded bytes.
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL PLAIN: malformed payload")
	}
	// parts[0] = authzid (may be empty), parts[1] = authcid, parts[2] = passwd
	authzid := string(parts[0])
	authcid := string(parts[1])
	passwd := string(parts[2])

	// Validate authcid (the user component of bouncer auth) — hostname input
	// surface is hostile. ParseAuthcid enforces length, NUL, and emptiness.
	parsed, err := ParseAuthcid(authcid)
	if err != nil {
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL PLAIN: invalid authcid")
	}

	// Authorize: the user component of the authcid (or the authzid, if set and
	// matching) must match Config.BouncerAuth.User.
	wantUser := s.srv.cfg.BouncerAuth.User
	if wantUser == "" {
		// No bouncer auth configured: reject all authentication attempts.
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL PLAIN: no bouncer auth configured")
	}

	// Verify the password via PBKDF2 FIRST — before checking the username.
	// This is a deliberate constant-time defence: verifying unconditionally
	// ensures the response time does not leak whether the username is valid.
	// (A username check that short-circuits before the KDF would be a timing
	// oracle distinguishing valid vs. invalid usernames.)
	// Do NOT log passwd or the raw payload.
	//
	// The gated variant serializes the expensive KDF across all sessions so an
	// unauthenticated peer cannot burn one core per connection by churning
	// AUTHENTICATE attempts (see kdfGate in auth.go).
	ok, err := verifyPasswordGated(s.srv.cfg.BouncerAuth.PasswordHash, passwd)
	if err != nil {
		// Malformed stored hash or unsupported algorithm. Log the structural error
		// (no password bytes) so the admin can diagnose misconfiguration.
		log.Printf("server: SASL verify: %v", err)
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL authentication failed")
	}

	// authzid, when non-empty, must match the bouncer username. (If authzid is
	// empty the client is authorizing as authcid, which is the common case.)
	// These comparisons use a bitwise-AND logic so both password and username
	// checks always run (no short-circuit after password failure).
	userMatch := (parsed.User == wantUser) && (authzid == "" || authzid == wantUser)

	if !ok || !userMatch {
		return s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "SASL authentication failed")
	}

	// Success. Record auth state on the session.
	s.saslAuthed = true
	s.saslParsed = &parsed

	// 900 RPL_LOGGEDIN — <nick>!<user>@<host> <account> :You are now logged in as <user>
	nick := s.clientNick()
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_LOGGEDIN,
		Params:  []string{nick, nick + "!*@*", wantUser, "You are now logged in as " + wantUser},
	}); err != nil {
		return err
	}

	// 903 RPL_SASLSUCCESS
	return s.sendNumeric(irc.RPL_SASLSUCCESS, nick, "SASL authentication successful")
}

// splitNUL splits a byte slice on NUL bytes, returning the parts (not including
// the NUL separators). It is used to parse the SASL PLAIN payload
// (authzid\0authcid\0passwd).
func splitNUL(b []byte) [][]byte {
	var parts [][]byte
	start := 0
	for i, c := range b {
		if c == 0 {
			parts = append(parts, b[start:i])
			start = i + 1
		}
	}
	parts = append(parts, b[start:])
	return parts
}

// ─── NICK / USER handlers ────────────────────────────────────────────────────

// handleNICK processes a NICK command. A collision check against other sessions
// is a Phase 6 concern; for Phase 1 the first nick is always accepted. After
// CAP END + NICK + USER, the welcome burst fires.
func (s *session) handleNICK(msg *irc.Message) error {
	newNick := msg.Param(0)
	if newNick == "" {
		return s.sendNumeric(irc.ERR_NONICKNAMEGIVEN, s.clientNick(), "No nickname given")
	}
	// Reject any nick that contains illegal bytes. A hostile client controls
	// this value; we must not panic and must not produce a broken message.
	if strings.ContainsAny(newNick, " \r\n\x00") {
		return s.sendNumeric(irc.ERR_ERRONEUSNICKNAME, s.clientNick(),
			"Erroneous nickname")
	}
	s.nick = newNick
	return s.maybeWelcome()
}

// handleUSER processes a USER command (USER <user> 0 * :<realname>).
// Repeated USER after registration is rejected per the spec.
func (s *session) handleUSER(msg *irc.Message) error {
	if s.registered() {
		return s.sendNumeric(irc.ERR_ALREADYREGISTERED, s.nick,
			"You may not reregister")
	}
	u := msg.Param(0)
	if u == "" {
		return s.sendNumeric(irc.ERR_NEEDMOREPARAMS, s.clientNick(),
			"Not enough parameters")
	}
	// Reject control bytes in user/realname.
	if strings.ContainsAny(u, " \r\n\x00") {
		return s.sendNumeric(irc.ERR_NEEDMOREPARAMS, s.clientNick(),
			"Invalid parameters")
	}
	s.user = u
	s.real = msg.Param(3) // trailing param; Param returns "" if absent
	return s.maybeWelcome()
}

// ─── PING handler ────────────────────────────────────────────────────────────

// handlePING responds to a client PING with a PONG, preserving the token.
func (s *session) handlePING(msg *irc.Message) error {
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.PONG,
		Params:  []string{serverName, msg.Param(0)},
	})
}

// ─── Welcome burst ───────────────────────────────────────────────────────────

// maybeWelcome emits the registration welcome burst if and only if all three
// conditions are met: CAP END has been processed (or no CAP was used), and both
// NICK and USER have been seen. It is idempotent — once the welcome burst is
// sent s.welcomed is set and subsequent calls are no-ops, so it is safe to call
// from every NICK, USER, and CAP END handler without risk of a double burst.
//
// When bouncer authentication is configured, the legacy no-CAP path (NICK+USER
// without CAP LS) does NOT satisfy the auth requirement. Such a client will
// complete the welcome burst only if no auth is configured, preserving backward
// compatibility with legacy IRC clients connecting to a dev/unconfigured bouncer.
// When auth is configured, the legacy path is also rejected (auth is mandatory).
func (s *session) maybeWelcome() error {
	if s.welcomed {
		return nil // already done
	}
	// If no CAP was used at all (capPhasePreLS) we treat the client as having
	// implicitly completed CAP negotiation once both NICK and USER are known.
	if s.capPhase == capPhasePreLS && s.nick != "" && s.user != "" {
		// If bouncer auth is configured, a client that skipped CAP entirely has
		// not authenticated. Reject via the same path as handleCAPEND.
		if s.srv.cfg.BouncerAuth.User != "" && !s.saslAuthed {
			_ = s.sendNumeric(irc.ERR_SASLFAIL, s.clientNick(), "Authentication required")
			_ = s.conn.Close()
			return nil
		}
		s.capPhase = capPhaseDone
	}
	if s.capPhase != capPhaseDone || s.nick == "" || s.user == "" {
		return nil
	}
	return s.sendWelcome()
}

// sendWelcome emits the IRC registration welcome burst: 001, 002, 003, 004, 005.
// It sets s.welcomed = true before sending so that any re-entrant call (which
// should not occur, but is guarded against) is a no-op.
//
// It also clears the registration timeout deadline set by serveConnInternal so
// that the now-registered long-lived session is not subject to it.
func (s *session) sendWelcome() error {
	s.welcomed = true
	nick := s.nick

	// Clear the registration deadline. A zero time disables the deadline.
	if err := s.nc.SetDeadline(time.Time{}); err != nil {
		// Best-effort: log but continue. A failed clear means the session may
		// eventually be killed by the old deadline if it fires before any I/O —
		// which is safe but suboptimal.
		log.Printf("server: clear registration deadline: %v", err)
	}

	// 001 RPL_WELCOME
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_WELCOME,
		Params:  []string{nick, fmt.Sprintf("Welcome to the lurkd bouncer, %s", nick)},
	}); err != nil {
		return err
	}

	// 002 RPL_YOURHOST
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_YOURHOST,
		Params:  []string{nick, fmt.Sprintf("Your host is %s, running version %s", serverName, serverVersion)},
	}); err != nil {
		return err
	}

	// 003 RPL_CREATED
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_CREATED,
		Params:  []string{nick, "This server was created just now"},
	}); err != nil {
		return err
	}

	// 004 RPL_MYINFO
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_MYINFO,
		Params:  []string{nick, serverName, serverVersion, "o", "o", ""},
	}); err != nil {
		return err
	}

	// 005 RPL_ISUPPORT — emit the real BOUNCER_NETID.
	// Per the soju.im/bouncer-networks spec and §7.5 amendment (b):
	//   - A bound session (netid > 0) emits BOUNCER_NETID=<netid>.
	//   - An unbound/control session emits BOUNCER_NETID= (empty value) to
	//     signal to the client that it is on the control/master context.
	bounceNetIDToken := fmt.Sprintf("BOUNCER_NETID=%d", s.netid)
	isupportTokens := []string{
		"CASEMAPPING=ascii",
		"CHANTYPES=#",
		"PREFIX=(qaohv)~&@%+",
		"NETWORK=lurkd",
		bounceNetIDToken,
		"are supported by this server",
	}
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_ISUPPORT,
		Params:  append([]string{nick}, isupportTokens...),
	}); err != nil {
		return err
	}

	// Register in the appropriate session registry after welcome so the client
	// is fully registered before any fan-out or notify reaches it.
	if s.netid != 0 {
		// Bound session: register for live upstream fan-out.
		s.srv.registerBoundSession(s)
	} else if s.capEnabled["soju.im/bouncer-networks-notify"] {
		// Unbound/control session with notify cap: register for network broadcasts.
		s.srv.registerControlSession(s)
	}

	// ERR_NOMOTD — lurkd has no MOTD; this is the standard way to signal that.
	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.ERR_NOMOTD,
		Params:  []string{nick, "No MOTD configured"},
	})
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// clientNick returns the client's current nick, or "*" if none is known yet.
// This is the correct first parameter for numerics sent before registration
// completes.
func (s *session) clientNick() string {
	if s.nick != "" {
		return s.nick
	}
	return "*"
}

// send writes msg to the connection. If serialization fails (malformed message
// — should not happen with server-controlled messages), the error is returned
// and the session is torn down.
func (s *session) send(msg *irc.Message) error {
	if err := s.conn.WriteMessage(msg); err != nil {
		return fmt.Errorf("server: send %s: %w", msg.Command, err)
	}
	return nil
}

// sendNumeric is a convenience wrapper for sending a numeric reply with a
// single trailing text parameter. target is the first param (recipient nick or
// "*"), text is the human-readable message.
func (s *session) sendNumeric(numeric, target, text string) error {
	return s.send(&irc.Message{
		Source:  serverName,
		Command: numeric,
		Params:  []string{target, text},
	})
}
