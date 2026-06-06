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
// burst (001–005 with RPL_ISUPPORT including a stub BOUNCER_NETID). TLS
// enforcement and SASL authentication land in Phase 2.
package server

import (
	"fmt"
	"log"
	"net"
	"strings"

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

// maxCapRequests caps how many distinct capability names a client may include
// across all CAP REQ lines during registration. A conformant client sends at
// most the set it received in CAP LS; this bound guards against a hostile client
// flooding the registration loop with arbitrarily many names.
const maxCapRequests = 256

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
}

// New builds a Server from cfg. cfg must not be nil.
func New(cfg *Config) *Server {
	return &Server{cfg: cfg}
}

// NewListener starts a plain TCP listener on addr (e.g. ":6697") and returns
// it. The caller typically passes the result to Serve. TLS wrapping is expected
// to be layered on in Phase 2; for Phase 1 the accept loop works over plain
// TCP.
func NewListener(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen %s: %w", addr, err)
	}
	return ln, nil
}

// Serve accepts connections from ln until it returns an error. Each accepted
// connection is handed to serveConn in a new goroutine. Serve blocks until ln
// is closed. It is intended for cmd/lurkd's main loop; tests drive serveConn
// directly without a real socket.
func (s *Server) Serve(ln net.Listener) error {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return fmt.Errorf("server: accept: %w", err)
		}
		go func() {
			if err := s.serveConn(nc); err != nil {
				log.Printf("server: conn %s: %v", nc.RemoteAddr(), err)
			}
		}()
	}
}

// serveConn wraps nc in a framed conn.Conn, runs the registration handshake
// (CAP negotiation + NICK/USER + welcome burst), and then enters the
// post-registration dispatch loop. It returns when the connection closes or
// an unrecoverable error occurs.
//
// serveConn is the primary testability seam: tests hand it the server end of a
// net.Pipe pair (via dialPipe) instead of a real socket, so the full protocol
// surface is exercised without network I/O.
func (s *Server) serveConn(nc net.Conn) error {
	c := conn.NewConn(nc, conn.Options{})
	defer c.Close()

	sess := &session{
		srv:  s,
		conn: c,
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

// session holds all per-connection state for one attached client. It is created
// by serveConn and lives on a single goroutine; no locking is needed.
type session struct {
	srv  *Server
	conn *conn.Conn

	// Registration state.
	nick     string
	user     string
	real     string
	welcomed bool // true once the 001 welcome burst has been sent (exactly once)

	// CAP negotiation state.
	capPhase    capPhase
	capEnabled  map[string]bool // caps ACKed in this session
	capReqCount int             // total distinct cap names seen in REQ lines
}

// run is the session goroutine's main loop. It processes messages until the
// connection closes.
func (s *session) run() error {
	s.capEnabled = make(map[string]bool)

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
	switch strings.ToUpper(msg.Command) {
	case irc.CAP:
		return s.handleCAP(msg)
	case irc.NICK:
		return s.handleNICK(msg)
	case irc.USER:
		return s.handleUSER(msg)
	case irc.PING:
		return s.handlePING(msg)
	case irc.QUIT:
		// Graceful client quit: close the connection and let run() return.
		_ = s.conn.Close()
		return nil
	default:
		// Unknown commands during registration: send ERR_NOTREGISTERED if not
		// yet done, otherwise ignore. We never panic on unknown input.
		if !s.registered() {
			return s.sendNumeric(irc.ERR_NOTREGISTERED, s.clientNick(), "You have not registered")
		}
		// Post-registration unknown commands: silently ignore for Phase 1.
		// (Phase 6+ adds BOUNCER verb dispatch, CHATHISTORY, etc.)
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
func (s *session) handleCAPEND() error {
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
func (s *session) maybeWelcome() error {
	if s.welcomed {
		return nil // already done
	}
	// If no CAP was used at all (capPhasePreLS) we treat the client as having
	// implicitly completed CAP negotiation once both NICK and USER are known.
	if s.capPhase == capPhasePreLS && s.nick != "" && s.user != "" {
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
func (s *session) sendWelcome() error {
	s.welcomed = true
	nick := s.nick

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

	// 005 RPL_ISUPPORT — first token line.
	// BOUNCER_NETID=0 is the stub value; Phase 6 fills in the real netid.
	isupportTokens := []string{
		"CASEMAPPING=ascii",
		"CHANTYPES=#",
		"PREFIX=(qaohv)~&@%+",
		"NETWORK=lurkd",
		"BOUNCER_NETID=0",
		"are supported by this server",
	}
	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.RPL_ISUPPORT,
		Params:  append([]string{nick}, isupportTokens...),
	}); err != nil {
		return err
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
