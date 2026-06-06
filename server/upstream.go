// upstream.go implements the Phase 3a upstream session manager for lurkd.
//
// For each configured Network the manager builds a client.Client (reusing the
// entire upstream stack — conn, cap, sasl, isupport) and connects it with
// AutoReconnect=true. Each client's OnAny handler forwards every event to a
// Sink. HandleReconnecting/HandleReconnected hooks track per-upstream connection
// state and expose it through UpstreamState.
//
// # Concurrency model
//
// The manager struct itself is only mutated by Start (before any goroutine sees
// it) and Close, which is safe to call once from any goroutine.
//
// Each upstream's OnAny handler runs on that client's own run goroutine; because
// multiple upstreams can call Sink.Ingest concurrently, the Sink must be
// concurrency-safe. The per-upstream connState is also written from the handler
// goroutine and read by external callers, so it is guarded by connState.mu.
//
// No client methods are called from inside an OnAny or reconnect handler —
// handlers merely store state and call Sink.Ingest — avoiding a deadlock that
// would arise if a handler blocked on the same goroutine that dispatches events.
//
// # Resilient startup (Phase 9)
//
// Start is non-fatal: a per-network initial-connect failure is logged and the
// network is kept in the managed set (marked ConnDisconnected). A background
// goroutine retries the initial connect with capped exponential backoff. The
// daemon comes up and serves the networks that ARE reachable even if others are
// down. These retry goroutines are tracked in Manager.retryWg and stopped via
// Manager.stopCh when Close is called.
package server

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/exec/lurk/client"
)

// Sink is the interface that receives every event from every upstream client.
// It is designed so that Phase 4's backlog store implements it directly: the
// netid identifies which upstream the event came from, allowing per-network
// per-target routing.
//
// Ingest must be concurrency-safe: multiple upstream clients call it
// concurrently from their respective run goroutines.
type Sink interface {
	Ingest(netid int, ev *client.Event)
}

// ConnStatus is the connection status of a single upstream session.
type ConnStatus int

const (
	// ConnConnected means the upstream is currently registered and connected.
	ConnConnected ConnStatus = iota
	// ConnReconnecting means the upstream dropped and is retrying.
	ConnReconnecting
	// ConnDisconnected means the upstream has never successfully connected (initial
	// connect failed) and is pending a background retry. Distinguished from
	// ConnReconnecting (which means it was connected at least once) so callers can
	// tell the two apart, though both resolve to ConnConnected when the session
	// eventually registers.
	ConnDisconnected
)

// initialRetryBase is the starting back-off delay for initial-connect retries.
// Each retry doubles, capped at initialRetryMax. These values are the defaults;
// the test-injection point is Manager.retryBase / Manager.retryMax.
const initialRetryBase = 2 * time.Second
const initialRetryMax = 120 * time.Second

// connState tracks one upstream's connection status. It is guarded by its own
// mutex because the reconnect handlers run on the client's goroutine while
// callers may read the state concurrently.
type connState struct {
	mu     sync.Mutex
	status ConnStatus
}

func (s *connState) set(st ConnStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
}

// get returns the current connection status.
func (s *connState) get() ConnStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// upstreamEntry holds one upstream client and its associated state.
type upstreamEntry struct {
	netid  int
	client *client.Client
	state  *connState
}

// dialer is the function signature for injecting an in-process connection in
// tests, matching client.Config.Dialer. It is unexported because it exists only
// for hermetic testing; production upstreams use the client's default dialer.
type dialer func(ctx context.Context) (net.Conn, error)

// Manager is the upstream session manager. It owns one client.Client per
// configured Network and feeds all events into a Sink.
//
// Lifecycle: build with NewManager, then call Start (which dials all upstreams),
// then close with Close when the daemon shuts down. Start and Close must each be
// called at most once. The zero value is not usable; always use NewManager.
//
// Concurrency: Start writes upstreams once before any goroutine reads it.
// After Start returns, Add and Remove may be called concurrently from any
// goroutine (e.g. from a session handler). All access to the upstreams slice
// is guarded by mu (a sync.RWMutex): readers use RLock, writers use Lock.
//
// Resilient startup: if a network's initial connect fails, Start logs it and
// moves on rather than aborting. A background goroutine retries with capped
// exponential backoff. All such goroutines are joined by Close.
type Manager struct {
	cfg  *Config
	sink Sink

	// mu guards the upstreams slice for concurrent Add/Remove and UpstreamState.
	mu        sync.RWMutex
	upstreams []*upstreamEntry

	// dialers, when non-nil, maps a network NetID to a custom dialer function.
	// This is the test-injection seam: tests populate it before calling Start so
	// each upstream uses an in-process net.Pipe pair instead of a real TCP
	// connection. In production it is nil and the client's default TCP/TLS dialer
	// is used. It is unexported — a test-only knob, not part of the public API.
	dialers map[int]dialer

	// closeOnce ensures Close only tears down clients once even if called
	// concurrently or multiple times.
	closeOnce sync.Once
	// stopCh is closed by Close to signal all background retry goroutines to stop.
	stopCh chan struct{}
	// retryWg tracks outstanding initial-connect retry goroutines so Close can
	// wait for them before returning.
	retryWg sync.WaitGroup

	// retryBase and retryMax are the back-off parameters for initial-connect
	// retries. Zero values mean use the production defaults (initialRetryBase,
	// initialRetryMax). Set by tests for fast retry.
	retryBase time.Duration
	retryMax  time.Duration
}

// NewManager builds a Manager for the given config and sink. It does not
// connect to any upstream; call Start for that.
func NewManager(cfg *Config, sink Sink) *Manager {
	return &Manager{
		cfg:    cfg,
		sink:   sink,
		stopCh: make(chan struct{}),
	}
}

// Start dials every configured network. It returns nil once it has processed
// all networks — successfully-connected ones are serving immediately; those
// that fail their initial connect are kept in the managed set (status
// ConnDisconnected) and retried in the background with capped exponential
// backoff. Start never fails due to a single network being unreachable; it only
// returns a non-nil error for configuration-level problems (none today, reserved
// for future validation).
//
// The context governs only the initial dial+registration phase of each network;
// it does not govern the lifetime of running sessions (use Close for that).
// Background retry goroutines are not bound to ctx; they stop when Close is
// called (via Manager.stopCh). This allows a short-timeout startup context
// while still giving background goroutines time to succeed.
//
// After Start returns, each successfully-connected upstream's auto-reconnect
// supervisor is running. Handlers fire from each client's own goroutine, so
// Sink.Ingest is called concurrently across upstreams.
func (m *Manager) Start(ctx context.Context) error {
	for i := range m.cfg.Networks {
		nw := &m.cfg.Networks[i]
		cc := m.buildClient(nw)
		st := &connState{status: ConnDisconnected}

		// Register reconnect-lifecycle hooks before Connect so they are not
		// missed if a reconnect fires very quickly after Connect returns.
		cc.HandleReconnecting(func(_ *client.Event) {
			st.set(ConnReconnecting)
		})
		cc.HandleReconnected(func(_ *client.Event) {
			st.set(ConnConnected)
		})

		// Register the OnAny ingestion handler. It runs on the client's
		// goroutine (after state tracking) so we must not call any client
		// method that could block on the run goroutine — we only forward to
		// the sink.
		netid := nw.NetID
		sink := m.sink
		cc.OnAny(func(ev *client.Event) {
			sink.Ingest(netid, ev)
		})

		entry := &upstreamEntry{netid: nw.NetID, client: cc, state: st}
		m.mu.Lock()
		m.upstreams = append(m.upstreams, entry)
		m.mu.Unlock()

		if err := cc.Connect(ctx); err != nil {
			// Non-fatal: log and schedule a background retry instead of aborting.
			log.Printf("upstream: initial connect network %d (%s) failed: %v — retrying in background", nw.NetID, nw.Name, err)
			m.scheduleRetry(nw, entry)
			continue
		}

		// Successful initial connect: mark connected and seed autojoin state.
		st.set(ConnConnected)
		// Join the autojoin channels. On reconnect the client's rejoinChannels
		// re-sends JOIN for channels already in its tracked state; an explicit
		// Join here seeds that state on the first connection.
		if len(nw.Channels) > 0 {
			_ = cc.Join(nw.Channels...)
		}
	}
	return nil
}

// scheduleRetry starts a background goroutine that retries the initial connect
// for the given network entry until it succeeds or Close is called. The retry
// uses capped exponential back-off (retryBase → retryMax). The goroutine is
// tracked in retryWg so Close can join it.
func (m *Manager) scheduleRetry(nw *Network, entry *upstreamEntry) {
	base := m.retryBase
	if base <= 0 {
		base = initialRetryBase
	}
	max := m.retryMax
	if max <= 0 {
		max = initialRetryMax
	}

	m.retryWg.Add(1)
	go func() {
		defer m.retryWg.Done()
		delay := base
		for {
			// Sleep with stop-channel interrupt.
			select {
			case <-m.stopCh:
				// Daemon is shutting down — stop retrying before we connect.
				// The client may have been started by a previous successful
				// Connect; either way, Close will clean it up.
				return
			case <-time.After(delay):
			}

			// Double delay for next iteration, capped at max.
			delay *= 2
			if delay > max {
				delay = max
			}

			// Check stop again before attempting (avoids a dial after Close).
			select {
			case <-m.stopCh:
				return
			default:
			}

			log.Printf("upstream: retrying initial connect for network %d (%s)", nw.NetID, nw.Name)

			// Use a background context: the startup context may already be done.
			// The retry is intentionally unbounded in time.
			if err := entry.client.Connect(context.Background()); err != nil {
				log.Printf("upstream: retry connect network %d (%s): %v", nw.NetID, nw.Name, err)
				continue
			}

			// Successfully connected.
			entry.state.set(ConnConnected)
			log.Printf("upstream: network %d (%s) connected after retry", nw.NetID, nw.Name)

			if len(nw.Channels) > 0 {
				_ = entry.client.Join(nw.Channels...)
			}
			return
		}
	}()
}

// Close shuts down every upstream client and stops all background retry
// goroutines. It is safe to call multiple times and from any goroutine.
// It signals each client's reconnect supervisor to stop (client.Close is
// idempotent and non-blocking by design) and waits for all retry goroutines
// to exit before returning.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		// Signal background retry goroutines to stop first, then close clients.
		close(m.stopCh)

		m.mu.RLock()
		entries := make([]*upstreamEntry, len(m.upstreams))
		copy(entries, m.upstreams)
		m.mu.RUnlock()
		for _, e := range entries {
			_ = e.client.Close()
		}

		// Wait for all initial-connect retry goroutines to finish.
		m.retryWg.Wait()
	})
}

// UpstreamState returns the current ConnStatus for the network with the given
// netid, and false if no such network is managed.
func (m *Manager) UpstreamState(netid int) (ConnStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.upstreams {
		if e.netid == netid {
			return e.state.get(), true
		}
	}
	return 0, false
}

// Client returns the upstream client.Client for the given netid, and false if
// no such upstream is managed. The returned pointer is safe to use from any
// goroutine (client.Client is goroutine-safe for its write methods).
func (m *Manager) Client(netid int) (*client.Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.upstreams {
		if e.netid == netid {
			return e.client, true
		}
	}
	return nil, false
}

// Add builds, connects, and registers a new upstream for nw. It appends the
// entry to the managed set and returns when the upstream has completed
// registration. ctx governs the connect+registration phase only.
//
// Add is safe to call concurrently with UpstreamState, Close, and other Add
// or Remove calls; the upstreams slice is guarded by m.mu.
//
// The caller must have added nw to Config.Networks before calling Add so that
// buildClient can find any injected Dialer for nw.NetID.
func (m *Manager) Add(ctx context.Context, nw *Network) error {
	cc := m.buildClient(nw)
	st := &connState{status: ConnConnected}

	cc.HandleReconnecting(func(_ *client.Event) { st.set(ConnReconnecting) })
	cc.HandleReconnected(func(_ *client.Event) { st.set(ConnConnected) })

	netid := nw.NetID
	sink := m.sink
	cc.OnAny(func(ev *client.Event) { sink.Ingest(netid, ev) })

	entry := &upstreamEntry{netid: nw.NetID, client: cc, state: st}
	m.mu.Lock()
	m.upstreams = append(m.upstreams, entry)
	m.mu.Unlock()

	if err := cc.Connect(ctx); err != nil {
		// Remove the entry we just added so the slice stays consistent.
		m.mu.Lock()
		for i, e := range m.upstreams {
			if e == entry {
				m.upstreams = append(m.upstreams[:i], m.upstreams[i+1:]...)
				break
			}
		}
		m.mu.Unlock()
		return fmt.Errorf("upstream: Add: connect network %d (%s): %w", nw.NetID, nw.Name, err)
	}

	if len(nw.Channels) > 0 {
		_ = cc.Join(nw.Channels...)
	}
	return nil
}

// Remove stops and removes the upstream with the given netid. If no such
// upstream is managed, Remove is a no-op. It is safe to call concurrently.
func (m *Manager) Remove(netid int) {
	m.mu.Lock()
	var found *upstreamEntry
	for i, e := range m.upstreams {
		if e.netid == netid {
			found = e
			m.upstreams = append(m.upstreams[:i], m.upstreams[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	if found != nil {
		_ = found.client.Close()
	}
}

// upstreamCaps is the set of capabilities the upstream client requests from the
// IRC server. echo-message is required for the self-send fan-out rule [B#11]:
// lurkd must see its own sent PRIVMSGs echoed so they can be stored once and
// fanned out to all bound clients. Without echo-message, self-sends are not
// delivered to other bound clients (graceful degradation — the message is
// still forwarded to the upstream, but lurkd never sees it back).
var upstreamCaps = append(
	append([]string(nil), client.DefaultCaps...),
	"echo-message",
	"labeled-response",
)

// buildClient constructs a client.Client from a Network config entry, wiring in
// any injected Dialer from m.Dialers.
func (m *Manager) buildClient(n *Network) *client.Client {
	cfg := client.Config{
		Nick:          n.Identity.Nick,
		User:          n.Identity.User,
		Realname:      n.Identity.Realname,
		Server:        n.Addr,
		TLS:           n.TLS,
		AutoReconnect: true,
		Caps:          upstreamCaps,
	}

	// Map upstream SASL credentials.
	if n.SASL.Mechanism != "" {
		cfg.SASL = client.SASLConfig{
			Mechanism: n.SASL.Mechanism,
			Username:  n.SASL.Authcid,
			Password:  n.SASL.Password,
		}
	}

	// Wire the test-injection dialer when present for this network.
	if m.dialers != nil {
		if d, ok := m.dialers[n.NetID]; ok {
			cfg.Dialer = d
			// AllowInsecureAuth is required when the test Dialer returns a plain
			// net.Pipe (no TLS) but SASL PLAIN would otherwise be refused.
			cfg.AllowInsecureAuth = true
		}
	}

	return client.New(cfg)
}
