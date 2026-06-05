package tui

import (
	"strings"

	"charm.land/bubbles/v2/viewport"

	"github.com/exec/lurk/client"
)

// buffer.go defines the Buffer type: one window per channel, private message,
// or the server/status view. A Buffer owns its scrollback (the formatted lines
// shown in the message pane), an embedded bubbles/viewport for scrolling that
// scrollback, and the activity counters the sidebar renders. Member lists are
// read live from the client at render time (the client already tracks them), so
// a Buffer does not duplicate that state.
//
// The decomposition — per-buffer scrollback, unread/highlight counters, a
// stick-to-bottom scroll model — follows senpai's buffer concept
// (senpai/ui/buffers.go), reimplemented on Bubble Tea's viewport.

// BufferKind distinguishes the three window types so the view can label and
// route them differently (a channel has a nicklist; a PM and the server buffer
// do not).
type BufferKind int

const (
	// BufferServer is the always-present status window (index 0). It shows
	// connection notices, MOTD, and any message not tied to a channel/PM.
	BufferServer BufferKind = iota
	// BufferChannel is a joined channel (Title starts with a channel prefix).
	BufferChannel
	// BufferPM is a private-message conversation with a single nick.
	BufferPM
)

// scrollbackLimit caps the number of stored lines per buffer so a long-running
// session does not grow unbounded. Older lines are dropped from the front once
// the limit is exceeded; the viewport only ever shows a window of them anyway.
const scrollbackLimit = 2000

// Buffer is a single window: a channel, a PM, or the server/status view. The
// exported fields (Title, Kind, Unread, Highlight) are read by tui-core's
// sidebar/switch logic; the unexported fields are the view layer's rendering
// state and are only touched from Update (via appendLine/layout/render).
type Buffer struct {
	// net is the network this buffer belongs to (its client/event stream). It is
	// nil only in low-level tests that construct a Buffer directly; the helpers
	// that read it tolerate nil.
	net *network

	// Title is the buffer's identity: a channel name ("#go"), a PM peer nick,
	// or the network/status label for the server buffer. Matching is
	// case-insensitive (see model.bufferIndex).
	Title string

	// Kind is the window type (server/channel/PM).
	Kind BufferKind

	// Unread counts messages received while the buffer was not focused; the
	// sidebar shows it as an activity marker and switchTo clears it.
	Unread int

	// Highlight is set when an unread message mentioned the user's nick. It
	// drives the sidebar's distinct highlight marker and is cleared on focus.
	Highlight bool

	// lines is the formatted scrollback: each entry is a fully styled,
	// ready-to-render row (ANSI included), produced by theme.formatLine. Stored
	// pre-format because formatting needs the event and the user's nick at
	// receive time, not at render time.
	lines []string

	// vp is the scrollback viewport. It is sized in layout() and re-filled from
	// lines whenever content changes or the pane resizes.
	vp viewport.Model

	// vpReady reports whether vp has been sized at least once (after the first
	// WindowSizeMsg); before that there is nothing to render into.
	vpReady bool

	// contentWidth is the width lines were last wrapped to. When the pane
	// resizes, content is re-wrapped and pushed into the viewport.
	contentWidth int

	// gotHistory is set once a chathistory backlog line has been rendered into
	// this buffer, so the "── history ──" divider is drawn only once.
	gotHistory bool

	// readMarker is the index into lines where unread content begins: it is set
	// to the line count when the user switches away from the buffer, so on return
	// a "new messages" divider can be drawn before everything that arrived since.
	// It is adjusted when old lines are trimmed (addLine).
	readMarker int
}

// newBuffer creates a channel or PM buffer for name. The server buffer is built
// separately via newServerBuffer. The viewport is left zero-valued until
// layout() sizes it on the next frame.
func newBuffer(name string, kind BufferKind) *Buffer {
	return &Buffer{
		Title: name,
		Kind:  kind,
	}
}

// bufferScope returns the log scope (network label) for a buffer, so logs are
// filed per network: <dir>/<network>/<target>.log.
func bufferScope(b *Buffer) string {
	return b.net.label()
}

// newServerBuffer creates the always-present status buffer (buffers index 0).
// title is the network name (or a placeholder before it is known).
func newServerBuffer(title string) *Buffer {
	return &Buffer{
		Title: title,
		Kind:  BufferServer,
	}
}

// isChannel reports whether name looks like a channel (starts with one of the
// common channel-type prefixes). It is a lightweight heuristic used when
// opening a buffer for an incoming PRIVMSG target without consulting CHANTYPES;
// the Ergo test server uses '#' and '&'.
func isChannel(name string) bool {
	if name == "" {
		return false
	}
	switch name[0] {
	case '#', '&', '+', '!':
		return true
	}
	return false
}

// addLine appends a pre-formatted row to the scrollback, enforcing the
// scrollback cap. It does not touch the viewport; layout/refresh push lines into
// the viewport so sizing stays centralized.
// markHistory draws the one-time "── history ──" divider that separates
// replayed chathistory backlog from live traffic. Subsequent calls are no-ops,
// so a multi-line history batch yields a single divider.
func (b *Buffer) markHistory(t theme) {
	if b.gotHistory {
		return
	}
	b.addLine(t.dim.Render("── history ──"))
	b.gotHistory = true
}

// addLine returns the number of lines dropped off the front by the scrollback
// trim (0 when nothing was dropped), so callers holding absolute indices into
// lines — the model's scrollback-search matches — can shift them to stay valid.
func (b *Buffer) addLine(s string) int {
	b.lines = append(b.lines, s)
	if len(b.lines) > scrollbackLimit {
		// Drop the oldest lines. Re-slice onto a fresh backing array
		// periodically would be tidier, but the simple re-slice keeps amortized
		// cost low and the excess is bounded by one line per append.
		drop := len(b.lines) - scrollbackLimit
		b.lines = b.lines[drop:]
		// Keep the read marker pointing at the same logical line.
		if b.readMarker -= drop; b.readMarker < 0 {
			b.readMarker = 0
		}
		return drop
	}
	return 0
}

// refresh re-renders the scrollback into the viewport. It wraps each stored line
// to the current viewport width (the viewport itself does not wrap styled
// content reliably, so we pre-wrap each stored row) and sticks to
// the bottom unless the user has scrolled up. atBottom is sampled before
// SetContent because SetContent can change the offset.
func (b *Buffer) refresh() {
	if !b.vpReady {
		return
	}
	stick := b.vp.AtBottom()
	b.vp.SetContent(b.wrapped())
	if stick {
		b.vp.GotoBottom()
	}
}

// wrapped joins the scrollback into the viewport's content, inserting a "new
// messages" divider at the read marker when there is unread content below it. An
// empty buffer renders as the empty string so the pane is blank rather than
// showing a stray newline. (The viewport soft-wraps each joined row to the width
// set in layout(); joining here preserves explicit row boundaries.)
func (b *Buffer) wrapped() string {
	if len(b.lines) == 0 {
		return ""
	}
	if b.readMarker > 0 && b.readMarker < len(b.lines) {
		rows := make([]string, 0, len(b.lines)+1)
		rows = append(rows, b.lines[:b.readMarker]...)
		rows = append(rows, markerLine(b.contentWidth))
		rows = append(rows, b.lines[b.readMarker:]...)
		return strings.Join(rows, "\n")
	}
	return strings.Join(b.lines, "\n")
}

// markerLine renders the "new messages" divider sized to the content width.
func markerLine(w int) string {
	const label = " new messages "
	if w < len(label)+2 {
		return defaultTheme.dim.Render(strings.TrimSpace(label))
	}
	dashes := w - len(label)
	left := dashes / 2
	right := dashes - left
	return defaultTheme.dim.Render(strings.Repeat("─", left) + label + strings.Repeat("─", right))
}

// memberList returns the channel's members for the nicklist, read live from the
// client. It returns nil for non-channel buffers and when the client is nil
// (tests) or not in the channel. The slice is the client's own snapshot copy.
func (b *Buffer) memberList(cli *client.Client) []client.Member {
	if b.Kind != BufferChannel || cli == nil {
		return nil
	}
	return cli.Members(b.Title)
}
