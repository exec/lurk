package tui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/exec/lurk/client"
)

// nickmenu.go adds interactive selection of users in the nicklist and a
// per-user context menu (Message, Whois, op actions, …) — the "reach over to
// a user and act on them" feature.
//
// Two pieces of model state drive it (declared in model.go): `focus` selects
// whether navigation keys drive the editor or the nicklist, and the menu* fields
// hold the open context menu. All transitions happen in Update via the handlers
// here, keeping the Elm value-semantics intact.

// focusArea selects which pane consumes navigation keys.
type focusArea int

const (
	focusInput focusArea = iota // the message editor (default)
	focusNicks                  // the nicklist (arrow keys select a user)
)

// menuActionID identifies a context-menu action. The label list is built per
// context (channel vs PM, whether we hold ops) by menuEntries.
type menuActionID int

const (
	miMessage menuActionID = iota
	miWhois
	miInsertNick
	miStatus  // open the grant/revoke status sub-menu
	miSetMode // grant or revoke one prefix mode (entry.mode is e.g. "+o")
	miKick
	miBan
)

// menuEntry is one row in the context menu: a label, the action it performs, and
// (for miSetMode) the signed mode change to apply.
type menuEntry struct {
	label string
	act   menuActionID
	mode  string // for miSetMode: e.g. "+o" / "-v"
}

// sortedMembers returns the active channel's members in the exact order the
// nicklist renders them (ops/voiced first by prefix rank, then case-insensitive
// by nick). renderNicklist and the selection/menu logic share this so the
// selected index always matches the row on screen.
//
// The result is cached on the returned model keyed by the active buffer pointer:
// subsequent calls within the same Update+View cycle (which operates on the same
// model value) return the cached slice without re-sorting. This eliminates the
// O(N log N) cost for a 10k-member channel on every keystroke in the nicklist —
// both the key handler (handleNickFocusKey / enterNickFocus) and the renderer
// (renderNicklist) sort the same list; with the cache the second call is free.
func sortedMembers(m model) (model, []client.Member) {
	active := m.activeBuffer()
	if m.nicklistSortedFor == active {
		// Cache hit: same buffer, return the already-sorted slice (may be nil
		// for a non-channel or empty channel — that is the correct result).
		return m, m.nicklistSorted
	}
	members := active.memberList(m.cli)
	sort.Slice(members, func(i, j int) bool {
		pi, pj := prefixRank(members[i].Prefixes), prefixRank(members[j].Prefixes)
		if pi != pj {
			return pi < pj
		}
		return strings.ToLower(members[i].Nick) < strings.ToLower(members[j].Nick)
	})
	m.nicklistSorted = members
	m.nicklistSortedFor = active
	return m, members
}

// memberPrefixes returns the membership prefix symbols held by nick in the
// active channel, or "" if not found. It takes the already-sorted slice so the
// caller (statusEntries) can pass the result of its own sortedMembers call and
// avoid a redundant sort — the old signature called sortedMembers internally
// and discarded the returned model, so the cache write was thrown away.
func memberPrefixes(members []client.Member, nick string) string {
	for _, mem := range members {
		if equalFold(mem.Nick, nick) {
			return mem.Prefixes
		}
	}
	return ""
}

// enterNickFocus moves keyboard focus to the nicklist, clamping the selection
// into range. It is a no-op when there is nothing to select (a PM/server buffer
// or an empty channel), so the editor keeps focus.
func (m model) enterNickFocus() model {
	var members []client.Member
	m, members = sortedMembers(m)
	if len(members) == 0 {
		return m
	}
	m.focus = focusNicks
	// Blur the editor so its caret stops blinking while the nicklist has focus.
	m.input.Blur()
	if m.nickSel < 0 || m.nickSel >= len(members) {
		m.nickSel = 0
	}
	return m
}

// refocusInput returns keyboard focus to the message editor and re-focuses the
// textinput so its caret reappears. It is the single place the nicklist/menu
// hand control back to the editor.
func (m model) refocusInput() model {
	m.focus = focusInput
	m.input.Focus()
	return m
}

// handleNickFocusKey processes a key while the nicklist is focused: arrows/jk
// move the selection, Enter opens the context menu for the selected user, Esc
// (or Ctrl-U again) returns focus to the editor.
func (m model) handleNickFocusKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	var members []client.Member
	m, members = sortedMembers(m)
	if len(members) == 0 {
		m = m.refocusInput()
		return m, nil
	}
	if m.nickSel >= len(members) {
		m.nickSel = len(members) - 1
	}
	switch msg.String() {
	case "esc", "ctrl+u":
		m = m.refocusInput()
	case "up", "k":
		if m.nickSel > 0 {
			m.nickSel--
		}
	case "down", "j":
		if m.nickSel < len(members)-1 {
			m.nickSel++
		}
	case "home":
		m.nickSel = 0
	case "end":
		m.nickSel = len(members) - 1
	case "enter", "right":
		m.menuOpen = true
		m.menuStatus = false
		m.menuNick = members[m.nickSel].Nick
		m.menuSel = 0
	}
	return m, nil
}

// handleMenuKey processes a key while the context menu is open: arrows/jk move
// the highlighted entry, Enter runs it, Esc closes the menu (back to the editor).
func (m model) handleMenuKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	entries := currentMenuEntries(m)
	if m.menuSel >= len(entries) {
		m.menuSel = len(entries) - 1
	}
	switch msg.String() {
	case "esc", "left":
		// In the sub-menu, back out to the top-level menu; otherwise close it.
		if m.menuStatus {
			m.menuStatus = false
			m.menuSel = 0
		} else {
			m.menuOpen = false
			m = m.refocusInput()
		}
	case "up", "k":
		if m.menuSel > 0 {
			m.menuSel--
		}
	case "down", "j":
		if m.menuSel < len(entries)-1 {
			m.menuSel++
		}
	case "enter", "right":
		if m.menuSel >= 0 && m.menuSel < len(entries) {
			// "Status…" opens the sub-menu rather than acting.
			if e := entries[m.menuSel]; e.act == miStatus {
				m.menuStatus = true
				m.menuSel = 0
				return m, nil
			}
			return m.applyMenu(entries[m.menuSel])
		}
		m.menuOpen = false
		m = m.refocusInput()
	}
	return m, nil
}

// menuEntries builds the context-menu rows for the current subject (m.menuNick)
// and context. Message/Whois/Insert-nick are always available; channel operator
// actions (Op/Deop, Voice/Devoice, Kick) appear only in a channel where we hold
// operator privileges, and Op-vs-Deop / Voice-vs-Devoice flip to match the
// target's current status.
func menuEntries(m model) []menuEntry {
	entries := []menuEntry{
		{label: "Message", act: miMessage},
		{label: "Whois", act: miWhois},
		{label: "Insert nick", act: miInsertNick},
	}
	b := m.activeBuffer()
	if b.Kind != BufferChannel || m.cli == nil {
		return entries
	}
	// The Status… sub-menu (grant/revoke modes, kick, ban) is offered when we hold
	// any operator-class prefix and can actually act — founder/admin/op/half-op.
	// Half-ops (%) can kick and set lower modes, so they get it too; plain voice
	// (+) cannot manage others and is excluded.
	if strings.ContainsAny(m.cli.SelfPrefixes(b.Title), "~&@%") {
		entries = append(entries, menuEntry{label: "Status…", act: miStatus})
	}
	return entries
}

// currentMenuEntries returns the rows for whichever menu level is showing.
func currentMenuEntries(m model) []menuEntry {
	if m.menuStatus {
		return statusEntries(m)
	}
	return menuEntries(m)
}

// statusEntries builds the "Status…" sub-menu: a grant/revoke toggle for each
// membership mode the server advertises (limited to those at or below our own
// level, since you cannot set a status above your own), then Kick and Ban.
func statusEntries(m model) []menuEntry {
	var entries []menuEntry
	b := m.activeBuffer()
	if m.cli != nil && b.Kind == BufferChannel {
		modes := m.cli.PrefixModes()
		symbols := m.cli.PrefixSymbols()
		// Call sortedMembers once and pass the slice to memberPrefixes so the
		// cache write propagates — the old memberPrefixes(m, ...) called
		// sortedMembers internally and discarded the returned model, throwing
		// away the cache write on every menu render.
		_, members := sortedMembers(m)
		target := memberPrefixes(members, m.menuNick)
		selfHigh := highestPrefixIndex(m.cli.SelfPrefixes(b.Title), symbols)
		for i := 0; i < min(len(modes), len(symbols)); i++ {
			if i < selfHigh {
				continue // above our own level — can't manage it
			}
			letter := string(modes[i])
			if strings.IndexByte(target, symbols[i]) >= 0 {
				entries = append(entries, menuEntry{label: "Revoke " + modeName(modes[i]), act: miSetMode, mode: "-" + letter})
			} else {
				entries = append(entries, menuEntry{label: "Grant " + modeName(modes[i]), act: miSetMode, mode: "+" + letter})
			}
		}
	}
	entries = append(entries, menuEntry{label: "Kick", act: miKick})
	entries = append(entries, menuEntry{label: "Ban", act: miBan})
	return entries
}

// modeName maps a membership-mode letter to a friendly label for the menu.
func modeName(letter byte) string {
	switch letter {
	case 'q':
		return "Founder"
	case 'a':
		return "Admin"
	case 'o':
		return "Op"
	case 'h':
		return "Half-op"
	case 'v':
		return "Voice"
	default:
		return "+" + string(letter)
	}
}

// highestPrefixIndex returns the index into symbols of the highest-privilege
// symbol present in have (lower index = higher privilege), or len(symbols) when
// none are held — used to hide modes above our own level from the sub-menu.
func highestPrefixIndex(have, symbols string) int {
	for i := 0; i < len(symbols); i++ {
		if strings.IndexByte(have, symbols[i]) >= 0 {
			return i
		}
	}
	return len(symbols)
}

// banMask returns the +b mask for nick: a host ban (*!*@host) when the member's
// host is known, otherwise a nick ban (nick!*@*).
func banMask(m model, nick string) string {
	for _, mem := range m.activeBuffer().memberList(m.cli) {
		if equalFold(mem.Nick, nick) && mem.Host != "" {
			return "*!*@" + mem.Host
		}
	}
	return nick + "!*@*"
}

// applyMenu performs the selected action against m.menuNick, closes the menu,
// and returns focus to the editor. Protocol side effects go through the client;
// buffer/editor changes mutate the model directly (Update owns this transition).
func (m model) applyMenu(e menuEntry) (tea.Model, tea.Cmd) {
	nick := m.menuNick
	ch := m.activeBuffer().Title
	m.menuOpen = false
	m.menuStatus = false
	m = m.refocusInput()

	switch e.act {
	case miMessage:
		// Open/focus a PM buffer for the nick. No client needed for the buffer
		// bookkeeping; messages get sent once the user types.
		_, i := m.ensureBuffer(nick, BufferPM)
		m.switchTo(i)
		m = layout(m)
	case miInsertNick:
		if cur := m.input.Value(); cur == "" {
			m.input.SetValue(nick + ": ")
		} else {
			m.input.SetValue(cur + nick + " ")
		}
		m.input.CursorEnd()
	case miWhois:
		if m.cli != nil {
			_ = m.cli.Whois(nick)
		}
	case miSetMode:
		if m.cli != nil {
			_ = m.cli.ChannelMode(ch, e.mode, nick)
		}
	case miKick:
		if m.cli != nil {
			_ = m.cli.Kick(ch, nick, "")
		}
	case miBan:
		if m.cli != nil {
			_ = m.cli.ChannelMode(ch, "+b", banMask(m, nick))
		}
	}
	return m, nil
}

// renderNickMenu draws the open context menu inside the nicklist column. Keeping
// it within that column (rather than as a free-floating overlay) avoids
// compositing over the frame while putting the menu right where the user is
// looking. The subject nick is the title; the selected entry is highlighted.
func renderNickMenu(m model, w, h int) string {
	entries := currentMenuEntries(m)
	// menuNick is a raw member nick from the server; sanitize before it reaches the
	// renderer so an ESC-laden nick cannot inject control sequences into the title.
	rows := []string{defaultTheme.nicklistTtl.Render(truncate(sanitize(m.menuNick), w))}
	sel := lipgloss.NewStyle().Reverse(true)
	for i, e := range entries {
		// In the status sub-menu, rule the mode toggles off from Kick/Ban.
		if m.menuStatus && e.act == miKick && i > 0 {
			rows = append(rows, defaultTheme.dim.Render(strings.Repeat("─", w)))
		}
		if i == m.menuSel {
			rows = append(rows, sel.Render(truncate("▸ "+e.label, w)))
		} else {
			rows = append(rows, truncate("  "+e.label, w))
		}
	}
	hint := "↑↓ ⏎ esc"
	if m.menuStatus {
		hint = "↑↓ ⏎ ←back"
	}
	rows = append(rows, "", truncate(hint, w))
	body := strings.Join(rows, "\n")
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(body)
}
