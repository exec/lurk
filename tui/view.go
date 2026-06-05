package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"lurk/client"
)

// view.go owns the on-screen composition: the classic IRC layout of a buffer
// sidebar, the message scrollback (a bubbles/viewport), an optional channel
// nicklist, a status bar, and the input pane — assembled with Lip Gloss
// Join{Horizontal,Vertical}. All sizes derive from the model's width/height,
// recomputed in layout() on every resize and buffer switch.
//
// Layout (target from docs/TUI-RESEARCH.md §4):
//
//	┌──────────┬─────────────────────────────┬────────┐
//	│ buffers  │  message scrollback         │ nicks  │
//	│ (sidebar)│  (viewport)                 │ (chan) │
//	├──────────┴─────────────────────────────┴────────┤
//	│ statusbar: net/nick/buf                          │
//	├──────────────────────────────────────────────────┤
//	│ > input                                          │
//	└──────────────────────────────────────────────────┘

// Pane sizing constants. These are the reserved columns/rows the layout carves
// out before handing the remainder to the message viewport.
const (
	sidebarWidth  = 18 // buffer-list column
	nicklistWidth = 16 // member-list column (channels only)
	topicHeight   = 1  // channel topic bar rows (channels only)
	statusHeight  = 1  // status bar rows
	helpHeight    = 1  // key-hint footer rows
	inputHeight   = 1  // input pane rows (coordinated with tui-input)

	// minBodyWidth is the floor for the message pane; below this we drop the
	// nicklist and then the sidebar so the message text never collapses.
	minBodyWidth = 20
)

// verticalLayout returns the row heights of the stacked regions for the current
// model: the topic bar (shown for channels only), and the message body that
// fills what remains after the topic bar, status bar, help footer, and input.
// body is floored at 1 so the message pane never vanishes on a tiny terminal.
// The full stack is: [topic] · body · status · help · input, summing to height.
func verticalLayout(m model) (topic, body int) {
	if m.activeBuffer().Kind == BufferChannel {
		topic = topicHeight
	}
	body = m.height - topic - statusHeight - helpHeight - inputHeight
	if body < 1 {
		body = 1
	}
	return topic, body
}

// layout recomputes every pane size from the model's current dimensions and the
// active buffer, then re-fills the active buffer's viewport. It is called from
// tui-core on the first WindowSizeMsg, on every resize, and after a buffer
// switch (so the newly focused buffer's viewport is sized and stuck to bottom).
//
// It returns the model by value to fit the Elm update style; the buffers it
// mutates are pointers, so the viewport state persists across calls.
func layout(m model) model {
	if m.width <= 0 || m.height <= 0 {
		return m
	}

	bodyW, _ := paneWidths(m.width, m.activeBuffer().Kind == BufferChannel)
	_, bodyH := verticalLayout(m)

	// Size the editor to the full terminal width so long input lines scroll
	// horizontally within the bottom row rather than wrapping.
	m.input.SetWidth(m.width)

	// Keep the channel-list modal sized to the (possibly resized) terminal.
	if m.chanListOpen {
		w, h := channelListSize(m)
		m.chanList.SetSize(w, h)
	}

	// Size the active buffer's viewport. Inactive buffers are sized lazily when
	// they become active (their lines are retained), which keeps resize O(1)
	// rather than O(buffers).
	b := m.activeBuffer()
	if !b.vpReady {
		b.vp = viewport.New(viewport.WithWidth(bodyW), viewport.WithHeight(bodyH))
		b.vp.SoftWrap = true
		b.vpReady = true
		b.contentWidth = bodyW
		b.refresh()
		b.vp.GotoBottom()
	} else if b.vp.Width() != bodyW || b.vp.Height() != bodyH {
		stick := b.vp.AtBottom()
		b.vp.SetWidth(bodyW)
		b.vp.SetHeight(bodyH)
		b.contentWidth = bodyW
		b.refresh()
		if stick {
			b.vp.GotoBottom()
		}
	}
	return m
}

// paneWidths splits the terminal width into (body, columns), reserving the
// sidebar and (for channels) the nicklist. On narrow terminals it sheds the
// nicklist first, then the sidebar, so the message body keeps at least
// minBodyWidth columns. The returned showCols flags tell render which columns
// to draw.
func paneWidths(total int, channel bool) (body int, showCols struct{ sidebar, nicklist bool }) {
	showCols.sidebar = true
	showCols.nicklist = channel

	cols := sidebarWidth + 1 // sidebar + its separator
	if showCols.nicklist {
		cols += nicklistWidth + 1
	}
	body = total - cols

	if body < minBodyWidth && showCols.nicklist {
		// Drop the nicklist.
		showCols.nicklist = false
		cols = sidebarWidth + 1
		body = total - cols
	}
	if body < minBodyWidth && showCols.sidebar {
		// Drop the sidebar too.
		showCols.sidebar = false
		body = total
	}
	if body < 1 {
		body = 1
	}
	return body, showCols
}

// render composes the full frame as a tea.View. It reads (never mutates) the
// model; the only state changes are the cursor placement Bubble Tea needs. The
// active buffer's viewport is assumed sized by a prior layout() call.
func render(m model) tea.View {
	bodyKind := m.activeBuffer().Kind
	bodyW, show := paneWidths(m.width, bodyKind == BufferChannel)
	topicH, bodyH := verticalLayout(m)

	body := renderBody(m, bodyW, bodyH)

	// Assemble the top region: [sidebar | body | nicklist].
	cols := []string{}
	if show.sidebar {
		cols = append(cols, renderSidebar(m, sidebarWidth, bodyH), verticalRule(bodyH))
	}
	cols = append(cols, body)
	if show.nicklist {
		cols = append(cols, verticalRule(bodyH), renderNicklist(m, nicklistWidth, bodyH))
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, cols...)

	// Stack the regions: an optional topic bar above the panes, then the status
	// bar, the key-hint footer, and the input editor at the bottom.
	rows := make([]string, 0, 5)
	if topicH > 0 {
		rows = append(rows, renderTopic(m, m.width))
	}
	rows = append(rows, top, renderStatus(m), renderHelp(m), renderInput(m))
	frame := lipgloss.JoinVertical(lipgloss.Left, rows...)

	// The channel-list modal (/list) draws as a centered overlay on top of the
	// whole frame; while it is open it owns the screen, so we skip the editor
	// cursor placement below.
	if m.chanListOpen {
		v := tea.NewView(overlayChannelList(m, frame))
		return v
	}

	v := tea.NewView(frame)
	// Place the hardware cursor at the input caret only when the editor actually
	// has focus; while the nicklist or its menu is driving keys, the caret would
	// otherwise keep blinking in the input box and muddle where focus is.
	if m.focus == focusInput && !m.menuOpen {
		if cur := inputCursor(m); cur != nil {
			v.Cursor = cur
		}
	}
	return v
}

// renderBody returns the active buffer's scrollback viewport, padded to the
// pane height so it always fills its region (an empty buffer would otherwise be
// shorter than bodyH and misalign the JoinHorizontal).
func renderBody(m model, w, h int) string {
	b := m.activeBuffer()
	// An empty buffer shows a centered hint instead of a blank void, so a new user
	// has a next step (and a fresh channel/PM reads as "nothing yet" not "broken").
	if len(b.lines) == 0 {
		hint := defaultTheme.dim.Width(min(w-2, 56)).Align(lipgloss.Center).Render(emptyHint(b))
		return lipgloss.Place(w, h, lipgloss.Center, lipgloss.Center, hint)
	}
	var content string
	if b.vpReady {
		content = b.vp.View()
	}
	// Pad/truncate to exactly h rows and w cols so the columns line up.
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(content)
}

// emptyHint returns the placeholder shown in an empty buffer, tailored to its
// kind so the suggested next step fits the context.
func emptyHint(b *Buffer) string {
	switch b.Kind {
	case BufferServer:
		return "Type /help for commands · /list to browse channels · /join #channel"
	case BufferPM:
		return "No messages yet — say hello to " + b.Title
	default:
		return "No messages yet in " + b.Title
	}
}

// renderSidebar renders the buffer list with per-buffer activity markers: a
// highlight ("!") buffer in the highlight style, an unread ("•") buffer bold,
// the active buffer reverse-highlighted. The layout mirrors senpai's vertical
// buffer list (reference/senpai/ui/buffers.go DrawVerticalBufferList).
func renderSidebar(m model, w, h int) string {
	t := defaultTheme
	multi := len(m.networks) > 1

	var rows []string
	activeRow := 0
	var lastNet *network
	for i, b := range m.buffers {
		// With more than one network, group buffers under a bold network header.
		if multi && b.net != lastNet {
			rows = append(rows, t.nicklistTtl.Width(w).Render(truncate(b.net.label(), w)))
			lastNet = b.net
		}

		marker := " "
		st := t.sidebarItem
		switch {
		case b.Highlight:
			marker = markHigh
			st = t.sidebarHigh
		case b.Unread > 0:
			marker = markUnread
			st = t.sidebarUnread
		}
		label := b.Title
		if b.Kind == BufferServer {
			label = "(server)" // the header already names the network
		}
		// Show the unread count on an inactive buffer with pending activity, so a
		// busy session can be triaged at a glance.
		if i != m.active && b.Unread > 0 {
			label = fmt.Sprintf("%s (%d)", label, b.Unread)
		}
		indent := ""
		if multi {
			indent = " " // nest buffers under their network header
		}
		line := fmt.Sprintf("%s%s %s", indent, marker, label)
		if i == m.active {
			st = t.sidebarActive
			activeRow = len(rows)
		}
		rows = append(rows, st.Width(w).Render(truncate(line, w)))
	}

	// Window the display rows around the active buffer's row so it never scrolls
	// off-screen when there are more rows than fit (the nicklist's clipping fix).
	start := nicklistStart(activeRow, len(rows), h, true)
	rows = rows[start:min(start+h, len(rows))]
	body := strings.Join(rows, "\n")
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(body)
}

// renderNicklist renders the channel member list, sorted with ops/voiced first
// (by prefix) then alphabetically, each colored by its hashed nick color and
// prefixed with its highest membership symbol. Reads members live from the
// client.
func renderNicklist(m model, w, h int) string {
	// When a context menu is open it takes over the column (nickmenu.go).
	if m.menuOpen {
		return renderNickMenu(m, w, h)
	}

	t := defaultTheme
	members := sortedMembers(m) // shared order so selection indices line up

	self := ""
	if m.cli != nil {
		self = m.cli.Nick()
	}

	focused := m.focus == focusNicks

	// The title takes the first row; the rest is a scroll window over the member
	// list. When focused, the window follows the selection so it never scrolls off
	// screen (the bug where a selection below the fold vanished). avail clamps to
	// at least 1 so a 1-row column still renders the selected nick.
	avail := h - 1
	if avail < 1 {
		avail = 1
	}
	start := nicklistStart(m.nickSel, len(members), avail, focused)
	end := min(start+avail, len(members))

	title := t.nicklistTtl.Render(nicklistTitle(len(members), start, end))
	rows := []string{title}
	for i := start; i < end; i++ {
		selected := focused && i == m.nickSel
		isSelf := equalFold(members[i].Nick, self)
		rows = append(rows, t.nickRow(members[i], w, selected, isSelf))
	}
	body := strings.Join(rows, "\n")
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(body)
}

// nicklistStart returns the index of the first member to render so that a window
// of `rows` rows keeps the selection visible. When the list fits, or the nicklist
// is not focused, it renders from the top; otherwise it centers the selection and
// clamps to the ends so the first/last page stays full.
func nicklistStart(sel, n, rows int, focused bool) int {
	if !focused || n <= rows || rows <= 0 {
		return 0
	}
	start := sel - rows/2
	if start < 0 {
		start = 0
	}
	if start > n-rows {
		start = n - rows
	}
	return start
}

// nicklistTitle renders the "Users (N)" header, appending a "↑M ↓K" hint of how
// many members are scrolled off above/below the current window.
func nicklistTitle(n, start, end int) string {
	title := fmt.Sprintf("Users (%d)", n)
	above, below := start, n-end
	switch {
	case above > 0 && below > 0:
		return fmt.Sprintf("%s ↑%d↓%d", title, above, below)
	case above > 0:
		return fmt.Sprintf("%s ↑%d", title, above)
	case below > 0:
		return fmt.Sprintf("%s ↓%d", title, below)
	}
	return title
}

// nickRow formats a single nicklist row for mem within column width w. Away
// members render faint, logged-in members get a subtle trailing "·" badge, and
// the selected row (when the nicklist is focused) is shown as a reverse-video
// bar. The result never exceeds w display cells: the nick is truncated first,
// reserving a column for the badge so the styled output fits.
func (t theme) nickRow(mem client.Member, w int, selected, isSelf bool) string {
	sym := ""
	if mem.Prefixes != "" {
		sym = string(mem.Prefixes[0])
	}
	badge := ""
	if mem.Account != "" {
		badge = "·"
	}
	if selected {
		// Reverse-video bar with an unstyled label so it stays legible.
		return lipgloss.NewStyle().Reverse(true).Render(truncate(sym+mem.Nick+badge, w))
	}
	nickText := truncate(sym+mem.Nick, w-lipgloss.Width(badge))
	var styled string
	if mem.Away {
		styled = t.nicklistAway.Render(nickText)
	} else {
		styled = lipgloss.NewStyle().Foreground(t.nickColor(mem.Nick, isSelf)).Render(nickText)
	}
	if badge != "" {
		styled += t.nicklistAcct.Render(badge)
	}
	return styled
}

// renderTopic renders the channel topic bar shown across the top for channel
// buffers: the tracked topic (read live from the client), or a placeholder when
// none is set. It reuses the status-bar style so the top and bottom bars frame
// the conversation. The topic is server-controlled, so it is sanitized and
// truncated to the bar width.
func renderTopic(m model, w int) string {
	topic := ""
	if m.cli != nil {
		topic, _, _ = m.cli.Topic(m.activeBuffer().Title)
	}
	topic = sanitize(topic)
	if topic == "" {
		topic = "(no topic set)"
	}
	content := truncate(" Topic: "+topic+" ", w)
	return defaultTheme.statusBar.Width(w).Render(content)
}

// renderHelp renders the one-line key-hint footer under the status bar, so the
// core navigation keys are discoverable on screen rather than only via /help. It
// always leads with the "/help" gateway (bubbles/help skips that binding because
// it has no key), then the curated ShortHelp set sized to whatever width remains
// so the keys truncate before the gateway does.
func renderHelp(m model) string {
	t := defaultTheme
	gateway := t.statusKey.Render("/help") + t.dim.Render(" commands  •  ")
	m.help.SetWidth(max(0, m.width-lipgloss.Width(gateway)))
	footer := gateway + m.help.View(m.keys)
	return lipgloss.NewStyle().
		Width(m.width).
		Height(helpHeight).
		MaxHeight(helpHeight).
		Render(footer)
}

// renderStatus renders the bottom status bar with network/nick/active-buffer
// and a scroll indicator when the viewport is scrolled up.
func renderStatus(m model) string {
	network, nick := "", ""
	if m.cli != nil {
		network = m.cli.Network()
		nick = m.cli.Nick()
	}
	b := m.activeBuffer()
	scrollNote := ""
	if b.vpReady && !b.vp.AtBottom() {
		below := b.vp.TotalLineCount() - b.vp.YOffset() - b.vp.Height()
		if below < 1 {
			scrollNote = "↓ more"
		} else {
			scrollNote = fmt.Sprintf("↓ %d more (End)", below)
		}
	}
	typing := typingNote(m.typingNicks(asciiLower(b.Title)))
	return defaultTheme.statusLine(network, nick, b.Title, m.searchNote(), scrollNote, typing, m.width)
}

// typingNote renders the "X is typing…" status segment for the given typers, or
// "" when nobody is typing. Two names are joined with "and"; more collapse to
// "N people".
func typingNote(nicks []string) string {
	switch len(nicks) {
	case 0:
		return ""
	case 1:
		return nicks[0] + " is typing…"
	case 2:
		return nicks[0] + " and " + nicks[1] + " are typing…"
	default:
		return fmt.Sprintf("%d people are typing…", len(nicks))
	}
}

// verticalRule draws a 1-cell-wide vertical separator h rows tall, used between
// the panes in the top region.
func verticalRule(h int) string {
	rule := defaultTheme.verticalRule.Render("│")
	rows := make([]string, h)
	for i := range rows {
		rows[i] = rule
	}
	return strings.Join(rows, "\n")
}

// truncate shortens s to at most w display columns, appending an ellipsis when
// it overflows. It measures with lipgloss.Width so ANSI styling does not count
// toward the width.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	// Trim rune-by-rune until it fits, leaving room for the ellipsis. This is
	// O(n) in the rune count, fine for sidebar/nick rows.
	runes := []rune(stripANSI(s))
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > w {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// prefixRank maps a member's highest prefix symbol to a sort rank (lower =
// higher privilege). Unknown/empty prefixes sort last. The ordering matches the
// conventional ~&@%+ hierarchy; servers advertise their own via PREFIX, but for
// the nicklist sort this fixed order is a reasonable default.
func prefixRank(prefixes string) int {
	if prefixes == "" {
		return len(prefixOrder)
	}
	for i, p := range prefixOrder {
		if rune(prefixes[0]) == p {
			return i
		}
	}
	return len(prefixOrder)
}

// prefixOrder is the conventional membership-prefix privilege order, highest
// first: owner, admin, op, halfop, voice.
var prefixOrder = []rune{'~', '&', '@', '%', '+'}

// appendLine formats a client event into a scrollback row for buffer b, updates
// b's unread/highlight counters when b is not focused, and refreshes the
// viewport if b is active. It is the seam tui-core's routeEvent calls for every
// inbound event; the view layer owns all formatting (see the message to
// tui-core agreeing the raw-Event signature).
func appendLine(m model, b *Buffer, ev client.Event) model {
	// Self is the nick of the buffer's OWN network, so highlight/own-message
	// detection is correct even for an event on a background network.
	self := b.net.nick()
	// Mark the start of a chathistory backlog with a one-time divider so the user
	// can tell replayed history from live traffic.
	if ev.BatchType() == "chathistory" {
		b.markHistory(defaultTheme)
	}
	row, highlight := defaultTheme.formatLine(ev, self, m.highlights)
	b.addLine(row)
	m.logger.Log(bufferScope(b), b.Title, stripANSI(row))

	active := b == m.activeBuffer()
	switch {
	case active:
		b.refresh()
	case ev.BatchType() == "chathistory":
		// Replayed backlog into a background buffer is rendered but is not "new
		// activity": it must not inflate unread or raise a highlight (your own
		// nick may appear in your history).
	default:
		b.Unread++
		if highlight {
			b.Highlight = true
			// Ring the bell for a mention in a buffer the user isn't watching.
			m.bell = true
		}
	}
	return m
}

// appendInfo appends a local informational line (command output, error, status)
// to buffer b. Unlike appendLine it is not tied to a protocol event and never
// raises unread/highlight.
func appendInfo(m model, b *Buffer, text string) model {
	row := defaultTheme.formatInfo(text)
	b.addLine(row)
	m.logger.Log(bufferScope(b), b.Title, stripANSI(row))
	if b == m.activeBuffer() {
		b.refresh()
	} else {
		b.Unread++
	}
	return m
}

// echoSelf appends a locally-echoed copy of an outbound PRIVMSG to the target
// buffer, used when the server has not negotiated echo-message so the user sees
// their own message immediately. It ensures the target buffer exists, formats
// the line as a self message, and switches focus is left to the caller.
func echoSelf(m model, target, text string) model {
	kind := BufferPM
	if isChannel(target) {
		kind = BufferChannel
	}
	b, _ := m.ensureBuffer(target, kind)
	row := defaultTheme.formatSelfMessage(currentNick(m), text)
	b.addLine(row)
	m.logger.Log(bufferScope(b), b.Title, stripANSI(row))
	if b == m.activeBuffer() {
		b.refresh()
	}
	return m
}

// currentNick returns the client's nick, or a placeholder when there is no
// client (tests). Centralized so the self-echo formatting has a single source.
func currentNick(m model) string {
	if m.cli != nil {
		return m.cli.Nick()
	}
	return "me"
}
