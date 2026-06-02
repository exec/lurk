package tui

import (
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"lurk/client"
)

// nickmenu.go adds interactive selection of users in the nicklist and a
// per-user context menu (Message, Whois, op actions, …). It is the Phase-1
// "reach over to a user and act on them" feature (docs/PLAN-CYCLE3.md §A).
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
	miOp
	miDeop
	miVoice
	miDevoice
	miKick
)

// menuEntry is one row in the context menu: a label and the action it performs.
type menuEntry struct {
	label string
	act   menuActionID
}

// sortedMembers returns the active channel's members in the exact order the
// nicklist renders them (ops/voiced first by prefix rank, then case-insensitive
// by nick). renderNicklist and the selection/menu logic share this so the
// selected index always matches the row on screen.
func sortedMembers(m model) []client.Member {
	members := m.activeBuffer().memberList(m.cli)
	sort.Slice(members, func(i, j int) bool {
		pi, pj := prefixRank(members[i].Prefixes), prefixRank(members[j].Prefixes)
		if pi != pj {
			return pi < pj
		}
		return strings.ToLower(members[i].Nick) < strings.ToLower(members[j].Nick)
	})
	return members
}

// memberPrefixes returns the membership prefix symbols held by nick in the
// active channel, or "" if not found.
func memberPrefixes(m model, nick string) string {
	for _, mem := range sortedMembers(m) {
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
	if len(sortedMembers(m)) == 0 {
		return m
	}
	m.focus = focusNicks
	if m.nickSel < 0 || m.nickSel >= len(sortedMembers(m)) {
		m.nickSel = 0
	}
	return m
}

// handleNickFocusKey processes a key while the nicklist is focused: arrows/jk
// move the selection, Enter opens the context menu for the selected user, Esc
// (or Ctrl-U again) returns focus to the editor.
func (m model) handleNickFocusKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	members := sortedMembers(m)
	if len(members) == 0 {
		m.focus = focusInput
		return m, nil
	}
	if m.nickSel >= len(members) {
		m.nickSel = len(members) - 1
	}
	switch msg.String() {
	case "esc", "ctrl+u":
		m.focus = focusInput
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
		m.menuNick = members[m.nickSel].Nick
		m.menuSel = 0
	}
	return m, nil
}

// handleMenuKey processes a key while the context menu is open: arrows/jk move
// the highlighted entry, Enter runs it, Esc closes the menu (back to the editor).
func (m model) handleMenuKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	entries := menuEntries(m)
	switch msg.String() {
	case "esc", "left":
		m.menuOpen = false
		m.focus = focusInput
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
			return m.applyMenu(entries[m.menuSel])
		}
		m.menuOpen = false
		m.focus = focusInput
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
		{"Message", miMessage},
		{"Whois", miWhois},
		{"Insert nick", miInsertNick},
	}
	b := m.activeBuffer()
	if b.Kind != BufferChannel || m.cli == nil {
		return entries
	}
	// Operator-only actions, shown only when we can actually perform them.
	if strings.ContainsAny(m.cli.SelfPrefixes(b.Title), "~&@") {
		target := memberPrefixes(m, m.menuNick)
		if strings.Contains(target, "@") {
			entries = append(entries, menuEntry{"Deop", miDeop})
		} else {
			entries = append(entries, menuEntry{"Op", miOp})
		}
		if strings.Contains(target, "+") {
			entries = append(entries, menuEntry{"Devoice", miDevoice})
		} else {
			entries = append(entries, menuEntry{"Voice", miVoice})
		}
		entries = append(entries, menuEntry{"Kick", miKick})
	}
	return entries
}

// applyMenu performs the selected action against m.menuNick, closes the menu,
// and returns focus to the editor. Protocol side effects go through the client;
// buffer/editor changes mutate the model directly (Update owns this transition).
func (m model) applyMenu(e menuEntry) (tea.Model, tea.Cmd) {
	nick := m.menuNick
	ch := m.activeBuffer().Title
	m.menuOpen = false
	m.focus = focusInput

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
	case miOp:
		if m.cli != nil {
			_ = m.cli.ChannelMode(ch, "+o", nick)
		}
	case miDeop:
		if m.cli != nil {
			_ = m.cli.ChannelMode(ch, "-o", nick)
		}
	case miVoice:
		if m.cli != nil {
			_ = m.cli.ChannelMode(ch, "+v", nick)
		}
	case miDevoice:
		if m.cli != nil {
			_ = m.cli.ChannelMode(ch, "-v", nick)
		}
	case miKick:
		if m.cli != nil {
			_ = m.cli.Kick(ch, nick, "")
		}
	}
	return m, nil
}

// handleMouse maps a left click in the nicklist column to a member selection and
// opens that member's context menu. Clicks elsewhere are ignored (a future pass
// can add sidebar click-to-switch). The geometry mirrors view.go: the nicklist
// is the rightmost nicklistWidth columns, its first row is the "Users (n)" title,
// and members follow one per row.
func (m model) handleMouse(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	b := m.activeBuffer()
	_, show := paneWidths(m.width, b.Kind == BufferChannel)
	if !show.nicklist || msg.X < m.width-nicklistWidth {
		return m, nil
	}
	members := sortedMembers(m)
	row := msg.Y - 1 // row 0 is the "Users (n)" header
	if row < 0 || row >= len(members) {
		return m, nil
	}
	m.focus = focusNicks
	m.nickSel = row
	m.menuOpen = true
	m.menuNick = members[row].Nick
	m.menuSel = 0
	return m, nil
}

// renderNickMenu draws the open context menu inside the nicklist column. Keeping
// it within that column (rather than as a free-floating overlay) avoids
// compositing over the frame while putting the menu right where the user is
// looking. The subject nick is the title; the selected entry is highlighted.
func renderNickMenu(m model, w, h int) string {
	entries := menuEntries(m)
	rows := []string{defaultTheme.nicklistTtl.Render(truncate(m.menuNick, w))}
	sel := lipgloss.NewStyle().Reverse(true)
	for i, e := range entries {
		if i == m.menuSel {
			rows = append(rows, sel.Render(truncate("▸ "+e.label, w)))
		} else {
			rows = append(rows, truncate("  "+e.label, w))
		}
	}
	rows = append(rows, "", truncate("↑↓ ⏎ esc", w))
	body := strings.Join(rows, "\n")
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(body)
}
