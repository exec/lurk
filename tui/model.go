// Package tui implements Lurk's full-screen terminal IRC client on Bubble Tea
// v2 (charm.land/bubbletea/v2). It is the one place in the module that may
// import the charm libraries; every other package (irc, conn, cap, sasl,
// isupport, client) stays dependency-free.
//
// The package is a single Bubble Tea Model split across sibling files by
// concern:
//
//   - model.go  the shared model struct + buffer create/switch/close (tui-core)
//   - app.go    Init/Update/View dispatch + program lifecycle           (tui-core)
//   - events.go client.Event -> tea.Msg bridge (the waitForIRC Cmd)      (tui-core)
//   - buffer.go the Buffer type + scrollback storage                     (tui-view)
//   - view.go   Lip Gloss layout: sidebar/viewport/nicklist/status/input (tui-view)
//   - style.go  theme, nick colorization, line formatting                (tui-view)
//   - input.go  textinput editor: history, tab-completion                (tui-input)
//   - command.go slash-command parser                                    (tui-input)
//   - keys.go   keybindings + help footer                                (tui-input)
//
// The model is a plain value, so Update is unit-testable by feeding synthetic
// messages and asserting on the resulting state and View(). See app_test.go.
package tui

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/chatlog"
	"github.com/exec/lurk/client"
)

// model is the root Bubble Tea model and the single piece of shared state the
// three tui owners (core/view/input) collaborate on. tui-core owns the struct
// definition and the Update/View dispatch; tui-view and tui-input add methods
// and read (never directly mutate outside Update) these fields.
//
// All model mutation is funneled through Update: handlers return a new model
// value rather than mutating through a pointer, matching the Elm-architecture
// value semantics Bubble Tea expects.
type model struct {
	// networks are the connected servers. Each owns a client + event stream; its
	// buffers live in the flat buffers slice below, grouped contiguously and
	// tagged with a back-pointer (Buffer.net). There is always at least one.
	networks []*network

	// cli is the ACTIVE network's client — a convenience kept in sync by switchTo
	// so command/render code that acts on the focused buffer can use it directly.
	// It may be nil in unit tests that exercise pure rendering/buffer logic.
	cli *client.Client

	// buffers is the ordered list of open windows. Index 0 is always the
	// server/status buffer (created in newModel); channels and PMs follow in
	// open order. tui-view owns the Buffer type (buffer.go).
	buffers []*Buffer

	// active is the index into buffers of the focused window.
	active int

	// width and height are the latest terminal dimensions from WindowSizeMsg
	// and the single source of truth for all layout math (view.go).
	width  int
	height int

	// input is the message editor. tui-input owns its configuration and key
	// handling (input.go).
	input textinput.Model

	// keys is the declarative keymap; help renders the footer hint bar. Both
	// are owned by tui-input (keys.go).
	keys keymap
	help help.Model

	// history is the input line history for up/down recall (tui-input).
	history   []string
	histIndex int

	// ready becomes true after the first WindowSizeMsg, gating any rendering
	// that depends on real dimensions.
	ready bool

	// quitting is set when a quit has been initiated so View can render a
	// final frame if desired.
	quitting bool

	// focus selects which pane receives navigation keys. The default is the
	// input editor; Ctrl-U moves focus to the nicklist so a user can be picked
	// with the keyboard and acted on via the context menu (nickmenu.go).
	focus focusArea

	// nickSel is the selected row in the active channel's sorted member list
	// while the nicklist is focused or the context menu is open.
	nickSel int

	// menuOpen is true while a per-user context menu is shown; menuNick is the
	// subject nick and menuSel the highlighted menu entry. menuStatus is true
	// while the "Status…" sub-menu (grant/revoke modes + Kick/Ban) is showing
	// instead of the top-level menu.
	menuOpen   bool
	menuNick   string
	menuSel    int
	menuStatus bool

	// typing tracks remote typing notifications (the +typing client tag),
	// keyed by the ASCII-folded buffer the typer is composing in (a channel name
	// or, for a PM, the peer's nick). Entries expire after typingTTL or when the
	// typer sends a message / a "done" notification. The status bar shows the
	// active buffer's typers.
	typing map[string][]typingEntry

	// lastTypingSent is when we last emitted our own "active" +typing tag, used
	// to throttle outbound typing notifications (see maybeSendTyping).
	lastTypingSent time.Time

	// typingTicking is true while a typing-expiry tick is scheduled. The tick
	// reschedules itself while anyone is typing and stops once they are all done,
	// so stale "X is typing…" indications clear on time even with no other
	// traffic and the editor blurred (where the cursor blink isn't repainting).
	typingTicking bool

	// bell is a one-shot flag set when an inbound message highlighted a buffer
	// the user is not currently viewing; Update rings the terminal bell and
	// clears it. It lives on the model so the pure routing path (appendLine) can
	// request a bell without reaching for I/O.
	bell bool

	// batchMode is set by the ircBatchMsg handler for the duration of its event
	// loop so that appendLine/appendInfo skip the per-line refresh() of the
	// active viewport (which would re-join and SetContent the entire scrollback
	// for every event). The handler calls refreshIfDirty() once after the loop.
	batchMode bool

	// membersCache is a one-per-Update-pass memoization of sortedMembers for
	// the active channel. sortedMembers snapshots the client's member list and
	// sorts it (O(N log N)); on a 10k-member channel that cost adds up when
	// several call sites within one Update+View cycle each call it independently.
	// The cache is populated on the first sortedMembers call in a pass and reused
	// by all subsequent calls (renderNicklist, handleNickFocusKey, enterNickFocus,
	// memberPrefixes). It is invalidated whenever the active buffer changes
	// (membersCacheFor tracks which buffer the cached slice belongs to) so a
	// buffer switch never surfaces stale nicks.
	membersCache    []client.Member
	membersCacheFor *Buffer // the buffer whose members are cached

	// highlights are extra mention words (besides the nick), and ignored are the
	// nicks whose messages are suppressed. Both are keyed by ASCII-folded form and
	// are reference-type maps, so the slash-command handlers (which receive the
	// model by value) can mutate them in place. Seeded from the client config.
	highlights map[string]bool
	ignored    map[string]bool

	// logger, when non-nil, appends each buffer's lines to a per-target log file.
	// A nil *chatlog.Logger is a safe no-op, so the logging calls need no guard.
	logger *chatlog.Logger

	// connect dials and registers an additional saved network by name (provided by
	// cmd/lurk, which owns the config→client mapping). nil disables /connect.
	connect ConnectFunc

	// Scrollback search state for the active buffer: the lower-cased term, the
	// matching line indices (ascending), and the position within them. searchTerm
	// is "" when no search is active. Ctrl-R cycles to the next (older) match.
	searchTerm    string
	searchMatches []int
	searchPos     int

	// chanList is the channel-directory modal shown by /list: a filterable,
	// scrollable list drawn as a centered overlay (channellist.go). chanListOpen
	// gates the overlay and its key capture; chanListLoading is true between the
	// LIST request and RPL_LISTEND, while chanListAccum gathers the RPL_LIST rows
	// before they populate the list on completion.
	chanList        list.Model
	chanListOpen    bool
	chanListLoading bool
	chanListAccum   []list.Item
}

// typingEntry is one remote user composing in a buffer, with the moment its
// "typing…" indication should expire absent a refresh.
type typingEntry struct {
	nick   string
	expiry time.Time
}

// typingTTL is how long a "X is typing…" indication lingers without a refresh.
const typingTTL = 6 * time.Second

// newModel builds the initial model for a connected client. cli may be nil for
// tests; sub is typically cli.Events(). It seeds the always-present server
// buffer (index 0) and delegates editor/keymap setup to the tui-input helpers.
func newModel(cli *client.Client, sub <-chan client.Event) model {
	net := &network{name: serverBufferTitle(cli), cli: cli, sub: sub}
	sb := newServerBuffer(serverBufferTitle(cli))
	sb.net = net
	m := model{
		networks:   []*network{net},
		cli:        cli,
		buffers:    []*Buffer{sb},
		active:     0,
		keys:       defaultKeymap(),
		help:       help.New(),
		input:      newInput(),
		typing:     make(map[string][]typingEntry),
		highlights: make(map[string]bool),
		ignored:    make(map[string]bool),
	}
	// Seed extra highlight words from the client config.
	if cli != nil {
		for _, w := range cli.Highlights() {
			if w = strings.TrimSpace(w); w != "" {
				m.highlights[asciiLower(w)] = true
			}
		}
	}
	return m
}

// isIgnored reports whether messages from nick are suppressed.
func (m *model) isIgnored(nick string) bool {
	return m.ignored[asciiLower(nick)]
}

// noteTyping records that nick is composing in the buffer keyed by key,
// (re)setting its expiry to typingTTL from now. Returns the mutated model so it
// composes in the value-semantics Update flow.
func (m model) noteTyping(key, nick string) model {
	if m.typing == nil {
		m.typing = make(map[string][]typingEntry)
	}
	entries := m.typing[key]
	now := time.Now()
	for i := range entries {
		if equalFold(entries[i].nick, nick) {
			entries[i].expiry = now.Add(typingTTL)
			m.typing[key] = entries
			return m
		}
	}
	m.typing[key] = append(entries, typingEntry{nick: nick, expiry: now.Add(typingTTL)})
	return m
}

// clearTyping drops nick's typing indication from the buffer keyed by key (on a
// "done"/"paused" notice or once they send a message).
func (m model) clearTyping(key, nick string) model {
	entries := m.typing[key]
	out := entries[:0]
	for _, e := range entries {
		if !equalFold(e.nick, nick) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		delete(m.typing, key)
	} else {
		m.typing[key] = out
	}
	return m
}

// typingNicks returns the still-current typers in the buffer keyed by key,
// dropping any whose indication has expired. Expired entries are not pruned from
// the map here (render is read-only); they are overwritten on the next notice.
func (m model) typingNicks(key string) []string {
	now := time.Now()
	var nicks []string
	for _, e := range m.typing[key] {
		if e.expiry.After(now) {
			nicks = append(nicks, e.nick)
		}
	}
	return nicks
}

// pruneTyping drops every expired typing entry across all buffers. The expiry
// tick calls it so a stale indication clears even when nothing else forces a
// repaint.
func (m model) pruneTyping(now time.Time) model {
	for key, entries := range m.typing {
		out := entries[:0]
		for _, e := range entries {
			if e.expiry.After(now) {
				out = append(out, e)
			}
		}
		if len(out) == 0 {
			delete(m.typing, key)
		} else {
			m.typing[key] = out
		}
	}
	return m
}

// nextTypingDeadline returns the duration until the earliest typing indication
// expires (negative if one is already due) and whether any indication exists. It
// drives the self-rescheduling expiry tick.
func (m model) nextTypingDeadline(now time.Time) (time.Duration, bool) {
	var earliest time.Time
	found := false
	for _, entries := range m.typing {
		for _, e := range entries {
			if !found || e.expiry.Before(earliest) {
				earliest, found = e.expiry, true
			}
		}
	}
	if !found {
		return 0, false
	}
	return earliest.Sub(now), true
}

// serverBufferTitle picks a label for the status buffer: the network name if
// known, else a generic placeholder. It tolerates a nil client.
func serverBufferTitle(cli *client.Client) string {
	if cli != nil {
		if net := cli.Network(); net != "" {
			return net
		}
	}
	return "(server)"
}

// activeBuffer returns the currently focused buffer. buffers always holds at
// least the server buffer, so this never returns nil for a model built with
// newModel.
func (m *model) activeBuffer() *Buffer {
	if m.active < 0 || m.active >= len(m.buffers) {
		return m.buffers[0]
	}
	return m.buffers[m.active]
}

// bufferIndex returns the index of the buffer whose title case-insensitively
// matches name, or -1 if none is open. The server buffer (index 0) is matched
// too. Channel and nick comparison uses simple ASCII folding, which is correct
// for the Ergo test server (CASEMAPPING=ascii); a future change can route this
// through the client's casemapping if needed.
func (m *model) bufferIndex(name string) int {
	return m.bufferIndexIn(m.activeNet(), name)
}

// bufferIndexIn returns the index of the buffer named name within network net,
// or -1. Scoping by network lets two networks host a channel of the same name.
func (m *model) bufferIndexIn(net *network, name string) int {
	for i, b := range m.buffers {
		if b.net == net && equalFold(b.Title, name) {
			return i
		}
	}
	return -1
}

// buffer returns the open buffer for name on the active network, or nil.
func (m *model) buffer(name string) *Buffer {
	if i := m.bufferIndex(name); i >= 0 {
		return m.buffers[i]
	}
	return nil
}

// netBuffer returns net's open buffer for name, or nil.
func (m *model) netBuffer(net *network, name string) *Buffer {
	if i := m.bufferIndexIn(net, name); i >= 0 {
		return m.buffers[i]
	}
	return nil
}

// findBuffer locates an open buffer named name across all networks, preferring
// the active network so a name open on several networks resolves to the one the
// user is looking at. It returns the buffer (whose .net is its owning network)
// and its index, or (nil, -1) if no buffer by that name is open anywhere. It is
// used by named-target commands (/part #x, /close #x) so the protocol side
// effect goes to the channel's OWN server, not whichever network is focused.
func (m *model) findBuffer(name string) (*Buffer, int) {
	if i := m.bufferIndexIn(m.activeNet(), name); i >= 0 {
		return m.buffers[i], i
	}
	for i, b := range m.buffers {
		if equalFold(b.Title, name) {
			return b, i
		}
	}
	return nil, -1
}

// activeNet returns the network owning the focused buffer.
func (m *model) activeNet() *network {
	return m.activeBuffer().net
}

// hasNetwork reports whether net is currently a connected network. It guards the
// event router against routing for a network that removeNetwork already dropped
// (a stale ircMsg that was queued before the removal).
func (m *model) hasNetwork(net *network) bool {
	if net == nil {
		return false
	}
	for _, n := range m.networks {
		if n == net {
			return true
		}
	}
	return false
}

// serverBuffer returns net's server/status buffer (its first buffer), or nil
// when net is not (or no longer) a connected network. Returning nil rather than
// falling back to buffers[0] keeps a late event for a removed network from
// leaking into net0's server buffer (or re-creating a buffer back-pointing at the
// dead network); callers drop the event when this is nil.
func (m *model) serverBuffer(net *network) *Buffer {
	if net == nil {
		return nil
	}
	for _, b := range m.buffers {
		if b.net == net && b.Kind == BufferServer {
			return b
		}
	}
	return nil
}

// targetBuffer returns where a net-scoped reply (WHOIS, LIST, reconnect notice)
// should render: the focused buffer when it belongs to net, else net's server
// buffer — so a reply never lands in an unrelated network's window. It is nil
// when net is not a connected network (see serverBuffer); callers drop the event.
func (m *model) targetBuffer(net *network) *Buffer {
	if ab := m.activeBuffer(); ab.net == net {
		return ab
	}
	return m.serverBuffer(net)
}

// maxAutoBuffers caps how many windows inbound traffic may auto-open. A hostile
// or compromised server can address messages from endless distinct channels and
// nicks; without a ceiling each new target would spawn a buffer (and its
// viewport) and grow the list without bound. Past the cap, traffic for an
// unopened target is shown in the server buffer instead. It is generous enough
// that no realistic session of joined channels + PMs approaches it. User-driven
// opens (/join, /query, the nicklist menu) are not subject to it.
const maxAutoBuffers = 512

// ensureBuffer returns the buffer for name, creating and appending a new one if
// it does not already exist. kind distinguishes channels from PMs for display.
// It returns the buffer and its index. The newly created buffer is NOT focused;
// callers that want to switch to it should call switchTo with the returned
// index.
func (m *model) ensureBuffer(name string, kind BufferKind) (*Buffer, int) {
	return m.ensureBufferIn(m.activeNet(), name, kind)
}

// ensureBufferIn returns net's buffer for name, creating it if absent. A new
// buffer is inserted immediately after net's last buffer so each network's
// buffers stay contiguous in the flat list (and thus grouped in the sidebar).
// The new buffer is NOT focused.
func (m *model) ensureBufferIn(net *network, name string, kind BufferKind) (*Buffer, int) {
	if i := m.bufferIndexIn(net, name); i >= 0 {
		return m.buffers[i], i
	}
	b := newBuffer(name, kind)
	b.net = net

	at := len(m.buffers)
	for i := range m.buffers {
		if m.buffers[i].net == net {
			at = i + 1
		}
	}
	m.buffers = append(m.buffers[:at], append([]*Buffer{b}, m.buffers[at:]...)...)
	if at <= m.active {
		m.active++ // keep focus on the same buffer after the insert
	}
	return b, at
}

// switchTo focuses the buffer at index i if it is in range, clearing that
// buffer's unread/highlight markers. Out-of-range indices are ignored so key
// handlers can pass computed targets without bounds-checking.
func (m *model) switchTo(i int) {
	if i < 0 || i >= len(m.buffers) {
		return
	}
	if i != m.active && m.active >= 0 && m.active < len(m.buffers) {
		// Mark everything currently in the buffer we're leaving as read, so a
		// "new messages" divider appears before anything that arrives while away.
		old := m.buffers[m.active]
		old.readMarker = len(old.lines)
	}
	m.active = i
	b := m.buffers[i]
	b.Unread = 0
	b.Highlight = false
	// Keep the active-client convenience pointing at the focused network.
	if b.net != nil {
		m.cli = b.net.cli
	}
	// A scrollback search is scoped to the buffer it ran in.
	m.clearSearch()
}

// nextBuffer focuses the next buffer in order, wrapping around. With a single
// buffer it is a no-op.
func (m *model) nextBuffer() { m.switchTo((m.active + 1) % len(m.buffers)) }

// prevBuffer focuses the previous buffer in order, wrapping around.
func (m *model) prevBuffer() {
	m.switchTo((m.active - 1 + len(m.buffers)) % len(m.buffers))
}

// jumpToActive focuses the next buffer (after the current one, wrapping) that
// has unread activity or a highlight. It is a no-op when nothing is active.
func (m *model) jumpToActive() {
	n := len(m.buffers)
	for off := 1; off <= n; off++ {
		i := (m.active + off) % n
		if m.buffers[i].Unread > 0 || m.buffers[i].Highlight {
			m.switchTo(i)
			return
		}
	}
}

// closeBuffer closes the buffer at index i and focuses a neighbour. The server
// buffer (index 0) cannot be closed and the call is ignored for it, so a
// /close on the status window is harmless. Closing the active buffer moves
// focus to the previous buffer (or the server buffer).
//
// closeBuffer does NOT send a PART; the command layer (tui-input) is
// responsible for any protocol side effects before calling this.
func (m *model) closeBuffer(i int) {
	if i <= 0 || i >= len(m.buffers) {
		return // index 0 (server) is permanent; out-of-range ignored
	}
	m.buffers = append(m.buffers[:i], m.buffers[i+1:]...)
	switch {
	case m.active > i:
		m.active--
	case m.active == i:
		if m.active >= len(m.buffers) {
			m.active = len(m.buffers) - 1
		}
	}
	if b := m.activeBuffer(); b.net != nil {
		m.cli = b.net.cli
	}
}

// addNetwork registers a new connected network and its server buffer (appended
// at the end), returning the network so the caller can start its event pump.
func (m *model) addNetwork(name string, cli *client.Client) *network {
	net := &network{name: name, cli: cli, sub: cli.Events()}
	sb := newServerBuffer(name)
	sb.net = net
	m.networks = append(m.networks, net)
	m.buffers = append(m.buffers, sb)
	return net
}

// removeNetwork drops net and all of its buffers (its connection ended for
// good), re-homing focus onto a surviving buffer. The caller quits when no
// networks remain.
func (m model) removeNetwork(net *network) model {
	kept := make([]*Buffer, 0, len(m.buffers))
	for _, b := range m.buffers {
		if b.net != net {
			kept = append(kept, b)
		}
	}
	m.buffers = kept

	nets := make([]*network, 0, len(m.networks))
	for _, n := range m.networks {
		if n != net {
			nets = append(nets, n)
		}
	}
	m.networks = nets

	if len(m.buffers) == 0 {
		m.active = 0
		return m
	}
	if m.active >= len(m.buffers) {
		m.active = len(m.buffers) - 1
	}
	if b := m.buffers[m.active]; b.net != nil {
		m.cli = b.net.cli
	}
	// The re-homed buffer is now the focused one, so clear its activity markers —
	// otherwise the survivor we're looking at keeps stale unread/highlight pips.
	// (We clear directly rather than routing through switchTo: the old network and
	// its buffers are already gone, so switchTo's "mark old buffer read" branch has
	// nothing valid to act on.)
	nb := m.buffers[m.active]
	nb.Unread = 0
	nb.Highlight = false
	return m
}

// equalFold reports ASCII-case-insensitive equality of a and b. It avoids
// pulling strings.EqualFold's Unicode handling, which is wrong for IRC where
// casemapping is server-defined (ascii here); a dedicated comparison keeps the
// intent explicit at call sites.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// asciiLower returns s lowercased over ASCII A-Z, the canonical map key for
// case-insensitive buffer/nick lookups under the server's ascii casemapping.
func asciiLower(s string) string {
	b := []byte(s)
	for i := range b {
		if 'A' <= b[i] && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// Compile-time assertion that model satisfies the Bubble Tea v2 Model
// interface (Init/Update/View live in app.go).
var _ tea.Model = model{}
