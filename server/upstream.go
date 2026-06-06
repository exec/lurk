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
package server

import (
	"context"
	"fmt"
	"net"
	"sync"

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
)

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
type Manager struct {
	cfg       *Config
	sink      Sink
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
}

// NewManager builds a Manager for the given config and sink. It does not
// connect to any upstream; call Start for that.
func NewManager(cfg *Config, sink Sink) *Manager {
	return &Manager{
		cfg:  cfg,
		sink: sink,
	}
}

// Start dials every configured network and returns once all upstream clients
// have completed registration (or failed). The context governs the dial and
// registration phase of each network; it does not govern the lifetime of the
// running sessions (use Close for that).
//
// A connect error on one network causes Start to clean up already-started
// upstreams and return the error. The caller does not need to call Close on a
// failed Start.
//
// After Start returns without error, each upstream's auto-reconnect supervisor
// is running in the background. Handlers fire from the client's own goroutine,
// so Sink.Ingest is called concurrently across upstreams.
func (m *Manager) Start(ctx context.Context) error {
	for i := range m.cfg.Networks {
		nw := &m.cfg.Networks[i]
		cc := m.buildClient(nw)

		st := &connState{status: ConnConnected}

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
		m.upstreams = append(m.upstreams, entry)

		if err := cc.Connect(ctx); err != nil {
			// Clean up all upstreams (including the one that failed) and return.
			m.Close()
			return fmt.Errorf("upstream: connect network %d (%s): %w", nw.NetID, nw.Name, err)
		}

		// Join the autojoin channels. On reconnect the client's rejoinChannels
		// re-sends JOIN for channels already in its tracked state; an explicit
		// Join here seeds that state on the first connection.
		if len(nw.Channels) > 0 {
			_ = cc.Join(nw.Channels...)
		}
	}
	return nil
}

// Close shuts down every upstream client. It is safe to call multiple times
// and from any goroutine. It signals each client's reconnect supervisor to
// stop (client.Close is idempotent and non-blocking by design).
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		for _, e := range m.upstreams {
			_ = e.client.Close()
		}
	})
}

// UpstreamState returns the current ConnStatus for the network with the given
// netid, and false if no such network is managed.
func (m *Manager) UpstreamState(netid int) (ConnStatus, bool) {
	for _, e := range m.upstreams {
		if e.netid == netid {
			return e.state.get(), true
		}
	}
	return 0, false
}

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
