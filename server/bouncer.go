// bouncer.go implements the soju.im/bouncer-networks BOUNCER verb dispatcher
// for lurkd's client-facing server (Phase 6a).
//
// # Control surface
//
// BOUNCER BIND <netid>                  — during registration only; sets session.netid
// BOUNCER LISTNETWORKS                  — post-registration, unbound session
// BOUNCER ADDNETWORK <attrs>            — post-registration, unbound session
// BOUNCER CHANGENETWORK <netid> <attrs> — post-registration, unbound session
// BOUNCER DELNETWORK <netid>            — post-registration, unbound session
//
// All management verbs (LIST/ADD/CHANGE/DEL) are gated on the session being:
//  1. Registered (welcome burst sent).
//  2. Unbound (netid == 0 / control context).
//  3. Having enabled the soju.im/bouncer-networks cap.
//
// Security invariant: no upstream SASL password is ever emitted in NETWORK
// lines. The bouncer.NetworkInfo type has no password field; the server.Network →
// bouncer.NetworkInfo conversion (networkToInfo) explicitly omits SASL.Password.
package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"

	"github.com/exec/lurk/bouncer"
	"github.com/exec/lurk/irc"
)

// handleBOUNCER is the top-level dispatcher for the BOUNCER verb. It may be
// called before or after registration — BIND is legal before registration,
// all management commands require registration.
func (s *session) handleBOUNCER(msg *irc.Message) error {
	cmd, err := bouncer.ParseCmd(msg.Params)
	if err != nil {
		return s.sendFail("BOUNCER", "INVALID_PARAMS", err.Error())
	}

	switch cmd.Sub {
	case bouncer.SubBind:
		return s.handleBouncerBIND(cmd)
	case bouncer.SubListNetworks:
		return s.handleBouncerLISTNETWORKS()
	case bouncer.SubAddNetwork:
		return s.handleBouncerADDNETWORK(cmd)
	case bouncer.SubChangeNetwork:
		return s.handleBouncerCHANGENETWORK(cmd)
	case bouncer.SubDelNetwork:
		return s.handleBouncerDELNETWORK(cmd)
	default:
		return s.sendFail("BOUNCER", "UNKNOWN_COMMAND",
			fmt.Sprintf("unknown BOUNCER subcommand: %s", cmd.Sub))
	}
}

// ─── BIND ────────────────────────────────────────────────────────────────────

// handleBouncerBIND handles "BOUNCER BIND <netid>".
// Must arrive during the registration window (before the welcome burst is
// sent). On success session.netid is set and the 005 BOUNCER_NETID token will
// carry the real netid. On failure a FAIL is sent and session.netid stays 0
// so the client can still complete registration on the control context.
func (s *session) handleBouncerBIND(cmd bouncer.Cmd) error {
	// Double-BIND: reject regardless of registration state.
	if s.netid != 0 {
		return s.sendFail("BOUNCER", "ALREADY_BOUND",
			fmt.Sprintf("session is already bound to network %d", s.netid))
	}
	// Post-registration BIND: spec says BIND must precede the welcome burst.
	if s.registered() {
		return s.sendFail("BOUNCER", "INVALID_PARAMS",
			"BOUNCER BIND must be sent before registration completes")
	}

	// Auth gate: when bouncer authentication is configured, BIND must not run
	// the netid lookup before the client has authenticated. Without this check
	// an unauthenticated client could probe which netids exist (a pre-auth
	// information-disclosure oracle) by observing whether the response is
	// INVALID_NETID vs. NOT_AUTHED. The findNetworkByID call below must not
	// be reachable by an unauthenticated session.
	if s.srv.cfg.BouncerAuth.User != "" && !s.saslAuthed {
		return s.sendFail("BOUNCER", "NOT_AUTHED",
			"authentication required before BOUNCER BIND")
	}

	// Validate the netid exists in the current config (under cfgMu).
	if _, ok := s.srv.findNetworkByID(cmd.NetID); !ok {
		return s.sendFail("BOUNCER", "INVALID_NETID",
			fmt.Sprintf("unknown network id %d", cmd.NetID))
	}

	s.netid = cmd.NetID
	return nil
}

// ─── Management gate ─────────────────────────────────────────────────────────

// gateManagementOp verifies the three pre-conditions for management verbs and
// sends a FAIL + returns a non-nil error if any check fails. The caller should
// propagate the error unchanged so dispatch does not double-send.
func (s *session) gateManagementOp(sub string) error {
	if !s.registered() {
		return s.sendFail("BOUNCER", "NOT_REGISTERED", "you have not registered")
	}
	if s.netid != 0 {
		return s.sendFail("BOUNCER", "BOUND_SESSION",
			"management commands are only available on the control context (unbound session)")
	}
	if !s.capEnabled["soju.im/bouncer-networks"] {
		return s.sendFail("BOUNCER", "CAP_NOT_NEGOTIATED",
			fmt.Sprintf("BOUNCER %s requires the soju.im/bouncer-networks capability", sub))
	}
	return nil
}

// ─── LISTNETWORKS ─────────────────────────────────────────────────────────────

// handleBouncerLISTNETWORKS handles "BOUNCER LISTNETWORKS".
// Responds with a soju.im/bouncer-networks BATCH of BOUNCER NETWORK lines.
// SASL passwords are NEVER included in the attribute output.
func (s *session) handleBouncerLISTNETWORKS() error {
	if err := s.gateManagementOp("LISTNETWORKS"); err != nil {
		return err
	}

	batchRef := newBatchRef()

	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: irc.BATCH,
		Params:  []string{"+" + batchRef, "soju.im/bouncer-networks"},
	}); err != nil {
		return err
	}

	networks := s.srv.snapshotNetworks()
	for i := range networks {
		nw := &networks[i]
		ni := networkToInfo(nw, s.srv.mgr)
		attrs := bouncer.EncodeAttrs(bouncer.NetworkInfoToAttrs(ni))
		if err := s.send(&irc.Message{
			Tags:    irc.Tags{"batch": batchRef},
			Source:  serverName,
			Command: "BOUNCER",
			Params:  []string{"NETWORK", fmt.Sprintf("%d", ni.NetID), attrs},
		}); err != nil {
			log.Printf("server: LISTNETWORKS: send NETWORK %d: %v", ni.NetID, err)
			break
		}
	}

	return s.send(&irc.Message{
		Source:  serverName,
		Command: irc.BATCH,
		Params:  []string{"-" + batchRef},
	})
}

// ─── ADDNETWORK ──────────────────────────────────────────────────────────────

// handleBouncerADDNETWORK handles "BOUNCER ADDNETWORK <attrs>".
// Allocates the next integer netid, adds the network to Config, persists,
// starts the upstream, replies to the requesting client, and broadcasts notify
// to other control sessions.
func (s *session) handleBouncerADDNETWORK(cmd bouncer.Cmd) error {
	if err := s.gateManagementOp("ADDNETWORK"); err != nil {
		return err
	}

	// Validate the port attr before touching config or disk.
	if err := validateAttrsAddr(cmd.Attrs); err != nil {
		return s.sendFail("BOUNCER", "INVALID_PARAMS",
			fmt.Sprintf("ADDNETWORK: %v", err))
	}

	// Allocate the netid, append, and persist under cfgMu. addNetwork returns a
	// COPY of the appended network, safe to use after the lock is released.
	added, err := s.srv.addNetwork(attrsToNetwork(cmd.Attrs))
	if err != nil {
		if err == errNetworkLimitReached {
			return s.sendFail("BOUNCER", "NETWORK_LIMIT",
				fmt.Sprintf("ADDNETWORK: %v", err))
		}
		return s.sendFail("BOUNCER", "INTERNAL_ERROR",
			fmt.Sprintf("ADDNETWORK: persist failed: %v", err))
	}

	// Start the upstream OUTSIDE the lock — mgr.Add → client.Connect blocks on
	// dialing/registration, which must not stall other management ops. Pass a
	// pointer to the local copy (buildClient reads its fields synchronously).
	if s.srv.mgr != nil {
		if err := s.srv.mgr.Add(context.Background(), &added); err != nil {
			// Non-fatal: config is persisted; the upstream will reconnect.
			log.Printf("server: ADDNETWORK: start upstream %d: %v", added.NetID, err)
		}
	}

	ni := networkToInfo(&added, s.srv.mgr)
	attrs := bouncer.EncodeAttrs(bouncer.NetworkInfoToAttrs(ni))
	netidStr := fmt.Sprintf("%d", added.NetID)

	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: "BOUNCER",
		Params:  []string{"NETWORK", netidStr, attrs},
	}); err != nil {
		return err
	}

	s.srv.broadcastNetworkNotify(s, &irc.Message{
		Source:  serverName,
		Command: "BOUNCER",
		Params:  []string{"NETWORK", netidStr, attrs},
	})
	return nil
}

// ─── CHANGENETWORK ───────────────────────────────────────────────────────────

// handleBouncerCHANGENETWORK handles "BOUNCER CHANGENETWORK <netid> <attrs>".
// Updates the named network's config fields, persists, and broadcasts notify.
// If Addr or TLS changed, the live upstream is restarted so the new address
// takes effect without a daemon restart.
func (s *session) handleBouncerCHANGENETWORK(cmd bouncer.Cmd) error {
	if err := s.gateManagementOp("CHANGENETWORK"); err != nil {
		return err
	}

	// Validate the port attr before touching config or disk.
	if err := validateAttrsAddr(cmd.Attrs); err != nil {
		return s.sendFail("BOUNCER", "INVALID_PARAMS",
			fmt.Sprintf("CHANGENETWORK: %v", err))
	}

	// Find, apply, and persist under cfgMu. changeNetwork returns both the
	// pre-apply snapshot and the post-apply copy, both safe to use after the
	// lock is released.
	old, updated, ok, err := s.srv.changeNetwork(cmd.NetID, func(n *Network) {
		applyAttrsToNetwork(cmd.Attrs, n)
	})
	if !ok {
		return s.sendFail("BOUNCER", "INVALID_NETID",
			fmt.Sprintf("unknown network id %d", cmd.NetID))
	}
	if err != nil {
		return s.sendFail("BOUNCER", "INTERNAL_ERROR",
			fmt.Sprintf("CHANGENETWORK: persist failed: %v", err))
	}

	// Reconnect the upstream when the connection address or TLS flag changed.
	// Remove tears down the live client; Add re-dials against the new address.
	// Both calls are made OUTSIDE cfgMu (already released above). A connect
	// failure is non-fatal: the config is already persisted and the upstream
	// will retry via auto-reconnect.
	//
	// Identity (nick/user/realname) and SASL changes do not trigger an immediate
	// re-dial; they take effect on the next connection (auto-reconnect or restart).
	if s.srv.mgr != nil && (updated.Addr != old.Addr || updated.TLS != old.TLS) {
		s.srv.mgr.Remove(cmd.NetID)
		if err := s.srv.mgr.Add(context.Background(), &updated); err != nil {
			log.Printf("server: CHANGENETWORK: reconnect network %d: %v", cmd.NetID, err)
		}
	}

	ni := networkToInfo(&updated, s.srv.mgr)
	attrs := bouncer.EncodeAttrs(bouncer.NetworkInfoToAttrs(ni))
	netidStr := fmt.Sprintf("%d", cmd.NetID)

	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: "BOUNCER",
		Params:  []string{"NETWORK", netidStr, attrs},
	}); err != nil {
		return err
	}

	s.srv.broadcastNetworkNotify(s, &irc.Message{
		Source:  serverName,
		Command: "BOUNCER",
		Params:  []string{"NETWORK", netidStr, attrs},
	})
	return nil
}

// ─── DELNETWORK ──────────────────────────────────────────────────────────────

// handleBouncerDELNETWORK handles "BOUNCER DELNETWORK <netid>".
// Stops the upstream, removes the network from Config, persists, and broadcasts
// notify. The notify carries attrs with state=deleted.
func (s *session) handleBouncerDELNETWORK(cmd bouncer.Cmd) error {
	if err := s.gateManagementOp("DELNETWORK"); err != nil {
		return err
	}

	// Remove from config and persist under cfgMu.
	ok, err := s.srv.delNetwork(cmd.NetID)
	if !ok {
		return s.sendFail("BOUNCER", "INVALID_NETID",
			fmt.Sprintf("unknown network id %d", cmd.NetID))
	}
	if err != nil {
		return s.sendFail("BOUNCER", "INTERNAL_ERROR",
			fmt.Sprintf("DELNETWORK: persist failed: %v", err))
	}

	// Stop the upstream OUTSIDE the lock (Remove → client.Close).
	if s.srv.mgr != nil {
		s.srv.mgr.Remove(cmd.NetID)
	}

	delAttrs := bouncer.EncodeAttrs([][2]string{{"state", "deleted"}})
	netidStr := fmt.Sprintf("%d", cmd.NetID)

	if err := s.send(&irc.Message{
		Source:  serverName,
		Command: "BOUNCER",
		Params:  []string{"NETWORK", netidStr, delAttrs},
	}); err != nil {
		return err
	}

	s.srv.broadcastNetworkNotify(s, &irc.Message{
		Source:  serverName,
		Command: "BOUNCER",
		Params:  []string{"NETWORK", netidStr, delAttrs},
	})
	return nil
}

// ─── Locked config-access helpers ─────────────────────────────────────────────
//
// These are the ONLY functions that read or mutate s.cfg.Networks. Each holds
// s.cfgMu for the duration of the slice access and returns COPIES, never
// pointers into the live slice — so callers can use the result after the lock
// is released without racing a concurrent mutation. No blocking I/O (mgr.Add/
// Remove, client.Connect, sending replies) runs inside the lock; Save is the
// only I/O, and it is fast and necessary to keep disk and memory consistent
// atomically with the mutation.

// snapshotNetworks returns a copy of the current network list under cfgMu.
// Used by LISTNETWORKS and any other pure read of the configured networks.
func (s *Server) snapshotNetworks() []Network {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	out := make([]Network, len(s.cfg.Networks))
	copy(out, s.cfg.Networks)
	return out
}

// findNetworkByID returns a copy of the network with the given netid and true,
// or a zero Network and false if no such network exists. Used by BIND
// validation.
func (s *Server) findNetworkByID(netid int) (Network, bool) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	for i := range s.cfg.Networks {
		if s.cfg.Networks[i].NetID == netid {
			return s.cfg.Networks[i], true
		}
	}
	return Network{}, false
}

// errNetworkLimitReached is returned by addNetwork when cfg.Networks is already
// at maxNetworks. It is a sentinel so the handler can send a specific FAIL code
// (NETWORK_LIMIT) rather than the generic INTERNAL_ERROR.
var errNetworkLimitReached = fmt.Errorf("network limit reached (%d)", maxNetworks)

// addNetwork allocates the next netid for nw, appends it to cfg.Networks, and
// persists. On a Save failure the append is rolled back so disk and memory stay
// consistent. It returns a COPY of the appended network (with its assigned
// netid). The caller must start the upstream (mgr.Add) OUTSIDE the lock using
// the returned copy.
//
// Returns errNetworkLimitReached (without modifying state) when the current
// count is already at maxNetworks.
func (s *Server) addNetwork(nw Network) (Network, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	// Enforce the per-daemon network ceiling before any allocation or I/O.
	if len(s.cfg.Networks) >= maxNetworks {
		return Network{}, errNetworkLimitReached
	}

	nw.NetID = s.cfg.NextNetID()
	s.cfg.Networks = append(s.cfg.Networks, nw)

	if s.cfgPath != "" {
		if err := Save(s.cfg, s.cfgPath); err != nil {
			// Roll back the append so memory matches the unchanged disk.
			s.cfg.Networks = s.cfg.Networks[:len(s.cfg.Networks)-1]
			return Network{}, err
		}
	}

	// Return a copy of the appended network (independent of the live slice).
	return s.cfg.Networks[len(s.cfg.Networks)-1], nil
}

// changeNetwork finds the network with the given netid, applies apply to it in
// place, persists, and returns both the pre-apply snapshot (old) and the
// post-apply copy (updated), whether the network was found, and any Save error.
// On a Save failure the in-memory mutation is rolled back to old so that disk
// and memory stay consistent; the caller receives the error and surfaces it to
// the client. The returned old and updated values are safe to use after the
// lock is released.
func (s *Server) changeNetwork(netid int, apply func(*Network)) (old, updated Network, found bool, err error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	idx := -1
	for i := range s.cfg.Networks {
		if s.cfg.Networks[i].NetID == netid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Network{}, Network{}, false, nil
	}

	// Snapshot the original before mutation so we can roll back on Save failure
	// and return it for reconnect comparison.
	old = s.cfg.Networks[idx]

	apply(&s.cfg.Networks[idx])

	if s.cfgPath != "" {
		if saveErr := Save(s.cfg, s.cfgPath); saveErr != nil {
			// Roll back: restore the pre-apply entry so memory matches disk.
			s.cfg.Networks[idx] = old
			return old, Network{}, true, saveErr
		}
	}

	return old, s.cfg.Networks[idx], true, nil
}

// delNetwork removes the network with the given netid and persists. It returns
// whether the network was found and any Save error. On a Save failure the
// removal is rolled back so that disk and memory stay consistent. The caller
// stops the upstream (mgr.Remove) OUTSIDE the lock.
func (s *Server) delNetwork(netid int) (bool, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	idx := -1
	for i := range s.cfg.Networks {
		if s.cfg.Networks[i].NetID == netid {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}

	// Snapshot the entry before removal so we can restore it on Save failure.
	removed := s.cfg.Networks[idx]
	s.cfg.Networks = append(s.cfg.Networks[:idx], s.cfg.Networks[idx+1:]...)

	if s.cfgPath != "" {
		if saveErr := Save(s.cfg, s.cfgPath); saveErr != nil {
			// Roll back: re-insert the removed entry at its original index so
			// memory matches the unchanged disk.
			tail := make([]Network, len(s.cfg.Networks)-idx)
			copy(tail, s.cfg.Networks[idx:])
			s.cfg.Networks = append(s.cfg.Networks[:idx], removed)
			s.cfg.Networks = append(s.cfg.Networks, tail...)
			return true, saveErr
		}
	}

	return true, nil
}

// ─── Conversion helpers ───────────────────────────────────────────────────────

// networkToInfo converts a server.Network to a bouncer.NetworkInfo, looking up
// the current connection state from mgr. Passwords are NEVER included — only
// non-secret attributes are carried. mgr may be nil (state will be
// "disconnected").
func networkToInfo(nw *Network, mgr *Manager) bouncer.NetworkInfo {
	ni := bouncer.NetworkInfo{
		NetID:    nw.NetID,
		Name:     nw.Name,
		Nick:     nw.Identity.Nick,
		Username: nw.Identity.User,
		Realname: nw.Identity.Realname,
		TLS:      nw.TLS,
		State:    "disconnected",
	}

	// Split Addr into host:port. Handles IPv6 addrs too via strings.Cut.
	if host, port, ok := strings.Cut(nw.Addr, ":"); ok {
		ni.Host = host
		ni.Port = port
	} else {
		ni.Host = nw.Addr
	}

	if mgr != nil {
		if st, found := mgr.UpstreamState(nw.NetID); found {
			switch st {
			case ConnConnected:
				ni.State = "connected"
			case ConnReconnecting:
				ni.State = "connecting"
			}
		}
	}
	return ni
}

// attrsToNetwork builds a server.Network from a parsed bouncer attribute map.
// Merges host+port into Addr. SASL password is NOT accepted (upstream
// credentials are server-side only). Caller must set NetID after allocation.
func attrsToNetwork(attrs map[string]string) Network {
	nw := Network{
		Name: attrs["name"],
		TLS:  attrs["tls"] == "1" || attrs["tls"] == "true",
		Identity: Identity{
			Nick:     attrs["nick"],
			User:     attrs["username"],
			Realname: attrs["realname"],
		},
	}
	host := attrs["host"]
	port := attrs["port"]
	switch {
	case host != "" && port != "":
		nw.Addr = host + ":" + port
	case host != "":
		nw.Addr = host
	}
	return nw
}

// validateAttrsAddr validates the host and port attributes from a parsed bouncer
// attribute map and returns an error when either would produce a malformed
// Network.Addr. Two checks are applied:
//
//  1. Port (if present and non-empty) must be a decimal integer in [1, 65535].
//     A non-numeric or out-of-range value would be silently concatenated into
//     Network.Addr and then cause a runtime dial error instead of a clean FAIL.
//
//  2. When both host and port are present, the assembled "host:port" string is
//     validated with net.SplitHostPort. This rejects unbracketed IPv6 literals
//     (e.g. host=::1 port=6697 assembles "::1:6697" which net.Dial cannot
//     parse), while correctly accepting bracketed IPv6 (host=[::1]) and normal
//     hostnames. A host-only addr (no port attr) is not assembled with a colon
//     and requires no SplitHostPort check.
func validateAttrsAddr(attrs map[string]string) error {
	p := attrs["port"]
	h := attrs["host"]

	// Port numeric check (applies whenever port is supplied).
	if p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("invalid port %q: must be an integer in [1, 65535]", p)
		}
	}

	// Host+port assembly check: only needed when both are present, because
	// attrsToNetwork/applyAttrsToNetwork only concatenates "host:port" in that
	// case. A host-only addr (no port) is stored as-is and does not need a
	// SplitHostPort round-trip.
	if h != "" && p != "" {
		if _, _, err := net.SplitHostPort(h + ":" + p); err != nil {
			return fmt.Errorf("invalid host %q with port %q: %v", h, p, err)
		}
	}

	return nil
}

// applyAttrsToNetwork applies only the attrs present in the map to an existing
// Network, leaving other fields unchanged. SASL password is NOT accepted.
func applyAttrsToNetwork(attrs map[string]string, nw *Network) {
	if v, ok := attrs["name"]; ok {
		nw.Name = v
	}
	if v, ok := attrs["nick"]; ok {
		nw.Identity.Nick = v
	}
	if v, ok := attrs["username"]; ok {
		nw.Identity.User = v
	}
	if v, ok := attrs["realname"]; ok {
		nw.Identity.Realname = v
	}
	if v, ok := attrs["tls"]; ok {
		nw.TLS = v == "1" || v == "true"
	}

	// Only update Addr if at least one of host/port is in the attrs.
	_, hasHost := attrs["host"]
	_, hasPort := attrs["port"]
	if hasHost || hasPort {
		// Parse existing Addr to get current host/port.
		curHost, curPort := nw.Addr, ""
		if i := strings.LastIndex(nw.Addr, ":"); i >= 0 {
			curHost = nw.Addr[:i]
			curPort = nw.Addr[i+1:]
		}
		if hasHost {
			curHost = attrs["host"]
		}
		if hasPort {
			curPort = attrs["port"]
		}
		if curHost != "" && curPort != "" {
			nw.Addr = curHost + ":" + curPort
		} else if curHost != "" {
			nw.Addr = curHost
		}
	}
}
