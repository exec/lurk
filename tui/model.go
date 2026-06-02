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
	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"lurk/client"
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
	// cli is the connected IRC client driving the session. It may be nil in
	// unit tests that exercise pure rendering/buffer logic.
	cli *client.Client

	// sub is the event stream consumed by the waitForIRC bridge (events.go). It
	// is client.Events(); a nil channel simply means no events ever arrive,
	// which is fine for tests.
	sub <-chan client.Event

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
	// subject nick and menuSel the highlighted menu entry.
	menuOpen bool
	menuNick string
	menuSel  int
}

// newModel builds the initial model for a connected client. cli may be nil for
// tests; sub is typically cli.Events(). It seeds the always-present server
// buffer (index 0) and delegates editor/keymap setup to the tui-input helpers.
func newModel(cli *client.Client, sub <-chan client.Event) model {
	m := model{
		cli:     cli,
		sub:     sub,
		buffers: []*Buffer{newServerBuffer(serverBufferTitle(cli))},
		active:  0,
		keys:    defaultKeymap(),
		help:    help.New(),
		input:   newInput(),
	}
	return m
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
	for i, b := range m.buffers {
		if equalFold(b.Title, name) {
			return i
		}
	}
	return -1
}

// buffer returns the open buffer for name, or nil if none. It is a convenience
// over bufferIndex for read sites that don't need the index.
func (m *model) buffer(name string) *Buffer {
	if i := m.bufferIndex(name); i >= 0 {
		return m.buffers[i]
	}
	return nil
}

// ensureBuffer returns the buffer for name, creating and appending a new one if
// it does not already exist. kind distinguishes channels from PMs for display.
// It returns the buffer and its index. The newly created buffer is NOT focused;
// callers that want to switch to it should call switchTo with the returned
// index.
func (m *model) ensureBuffer(name string, kind BufferKind) (*Buffer, int) {
	if i := m.bufferIndex(name); i >= 0 {
		return m.buffers[i], i
	}
	b := newBuffer(name, kind)
	m.buffers = append(m.buffers, b)
	return b, len(m.buffers) - 1
}

// switchTo focuses the buffer at index i if it is in range, clearing that
// buffer's unread/highlight markers. Out-of-range indices are ignored so key
// handlers can pass computed targets without bounds-checking.
func (m *model) switchTo(i int) {
	if i < 0 || i >= len(m.buffers) {
		return
	}
	m.active = i
	b := m.buffers[i]
	b.Unread = 0
	b.Highlight = false
}

// nextBuffer focuses the next buffer in order, wrapping around. With a single
// buffer it is a no-op.
func (m *model) nextBuffer() { m.switchTo((m.active + 1) % len(m.buffers)) }

// prevBuffer focuses the previous buffer in order, wrapping around.
func (m *model) prevBuffer() {
	m.switchTo((m.active - 1 + len(m.buffers)) % len(m.buffers))
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

// containsFold reports whether sub occurs in s under ASCII case folding. It is
// used for highlight detection; like equalFold it deliberately avoids Unicode
// folding since IRC casemapping is server-defined.
func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if equalFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// Compile-time assertion that model satisfies the Bubble Tea v2 Model
// interface (Init/Update/View live in app.go).
var _ tea.Model = model{}
