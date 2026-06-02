package tui

import (
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// keymap is the declarative set of keybindings the TUI recognizes. It feeds the
// bubbles/help footer (via ShortHelp/FullHelp) and is consulted by the core
// dispatch (app.go) and the editor handler (input.go) through key_matches.
//
// Bindings are grouped roughly by concern: global navigation (buffer switching,
// scrolling), editor affordances (completion, history), and program control
// (quit, toggling the help view). senpai inspired the binding choices
// (Ctrl-N/Ctrl-P to cycle buffers, PgUp/PgDn to scroll) but the declarative
// key.Binding wiring is the bubbles idiom.
type keymap struct {
	// Quit ends the program (also reachable via /quit).
	Quit key.Binding

	// NextBuffer / PrevBuffer cycle the focused window.
	NextBuffer key.Binding
	PrevBuffer key.Binding

	// FocusNicks moves keyboard focus to the nicklist so a user can be selected
	// and acted on via the context menu (Esc returns focus to the editor).
	FocusNicks key.Binding

	// ScrollUp / ScrollDown move the scrollback by a line; PageUp / PageDown
	// move it by a screenful. The view layer owns the viewport these drive.
	ScrollUp   key.Binding
	ScrollDown key.Binding
	PageUp     key.Binding
	PageDown   key.Binding

	// Complete triggers tab-completion cycling in the editor.
	Complete key.Binding

	// HistPrev / HistNext recall earlier / later input-history lines.
	HistPrev key.Binding
	HistNext key.Binding

	// Help toggles the help footer between the short and full views.
	Help key.Binding
}

// defaultKeymap returns the standard bindings. The help text (second arg to
// WithHelp) is what the footer renders, so it is kept terse.
func defaultKeymap() keymap {
	return keymap{
		Quit: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "quit"),
		),
		NextBuffer: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("ctrl+n", "next buf"),
		),
		PrevBuffer: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "prev buf"),
		),
		FocusNicks: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "users"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("ctrl+up"),
			key.WithHelp("ctrl+↑", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("ctrl+down"),
			key.WithHelp("ctrl+↓", "scroll down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdn", "page down"),
		),
		Complete: key.NewBinding(
			key.WithKeys("tab"),
			key.WithHelp("tab", "complete"),
		),
		HistPrev: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("↑", "history"),
		),
		HistNext: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("↓", "history"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
	}
}

// ShortHelp implements help.KeyMap: the one-line footer hints. It surfaces the
// handful of bindings a user most needs at a glance.
func (k keymap) ShortHelp() []key.Binding {
	return []key.Binding{
		k.NextBuffer,
		k.PrevBuffer,
		k.FocusNicks,
		k.Complete,
		k.Help,
		k.Quit,
	}
}

// FullHelp implements help.KeyMap: the expanded multi-column view shown when the
// help model is toggled open. Bindings are grouped by column.
func (k keymap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.NextBuffer, k.PrevBuffer, k.FocusNicks},
		{k.ScrollUp, k.ScrollDown, k.PageUp, k.PageDown},
		{k.Complete, k.HistPrev, k.HistNext},
		{k.Help, k.Quit},
	}
}

// key_matches reports whether the key press matches binding b. It wraps
// bubbles/key.Matches, which is generic over fmt.Stringer keys; tea.KeyPressMsg
// satisfies that via its String() method ("ctrl+n", "tab", "up", …). The name is
// the one app.go (tui-core) calls; keeping it stable avoids a rename there.
func key_matches(b key.Binding, msg tea.KeyPressMsg) bool {
	return key.Matches(msg, b)
}
