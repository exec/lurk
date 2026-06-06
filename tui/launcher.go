package tui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/exec/lurk/config"
)

// launcher.go is the pre-connection network picker shown when `lurk` is run with
// no -server: a HexChat-style list of saved networks with connect / add / edit /
// delete, plus an add/edit form. It is a self-contained Bubble Tea program (run
// by Launch) separate from the chat model — mirroring the plain-vs-TUI split in
// cmd/lurk — and reuses defaultTheme for styling. Networks persist to the JSON
// config (package config) as they are added/edited/deleted.

// Launch runs the launcher over the given config store and returns the network
// the user chose to connect to, or nil if they quit without choosing. Adds,
// edits, and deletes are persisted to path as they happen.
func Launch(ctx context.Context, store *config.File, path string) (*config.Network, error) {
	m := launcherModel{store: store, path: path, mode: modeList}
	if len(store.Networks) == 0 {
		m.sel = 0 // the "add network" row
	}
	out, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if err != nil {
		return nil, err
	}
	return out.(launcherModel).chosen, nil
}

type launcherMode int

const (
	modeList    launcherMode = iota // the network list
	modeForm                        // add/edit a network
	modeConfirm                     // confirm a delete
)

// launcherModel is the launcher's Bubble Tea model. store is a pointer so adds/
// edits/deletes mutate the shared document that Save persists.
type launcherModel struct {
	store *config.File
	path  string

	mode launcherMode
	sel  int // selected row in modeList; len(Networks) is the "add network" row

	form     *netForm // active add/edit form (modeForm)
	formOrig string   // the network's prior name when editing ("" when adding)

	chosen *config.Network // set when the user connects; returned by Launch
	status string          // a transient status/error line (e.g. a save failure)

	width, height int
}

func (m launcherModel) Init() tea.Cmd { return nil }

func (m launcherModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyPressMsg:
		switch m.mode {
		case modeForm:
			return m.updateForm(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		default:
			return m.updateList(msg)
		}
	default:
		// Forward other messages (notably the cursor blink) to the focused form
		// field so its caret keeps blinking while editing.
		if m.mode == modeForm && m.form != nil {
			if cur := &m.form.fields[m.form.idx]; cur.kind == kindText {
				var cmd tea.Cmd
				cur.input, cmd = cur.input.Update(msg)
				return m, cmd
			}
		}
		return m, nil
	}
}

// updateList handles keys on the network list.
func (m launcherModel) updateList(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	addRow := len(m.store.Networks) // index of the "add network" row
	switch msg.String() {
	case "ctrl+c", "q", "esc":
		return m, tea.Quit // chosen stays nil
	case "up", "k":
		if m.sel > 0 {
			m.sel--
		}
	case "down", "j":
		if m.sel < addRow {
			m.sel++
		}
	case "home":
		m.sel = 0
	case "end":
		m.sel = addRow
	case "a":
		return m.openForm(-1)
	case "enter":
		if m.sel >= addRow {
			return m.openForm(-1) // the "add network" row
		}
		net := m.store.Networks[m.sel]
		m.chosen = &net
		return m, tea.Quit
	case "e":
		if m.sel < addRow {
			return m.openForm(m.sel)
		}
	case "d":
		if m.sel < addRow {
			m.mode = modeConfirm
		}
	}
	return m, nil
}

// openForm switches to the add (idx < 0) or edit (idx into Networks) form.
func (m launcherModel) openForm(idx int) (tea.Model, tea.Cmd) {
	if idx < 0 {
		m.form = newNetForm(config.Network{}, m.store.Defaults)
		m.formOrig = ""
	} else {
		n := m.store.Networks[idx]
		m.form = newNetForm(n, m.store.Defaults)
		m.formOrig = n.Name
	}
	m.mode = modeForm
	m.status = ""
	return m, textinput.Blink
}

// updateForm handles keys while adding/editing a network.
func (m launcherModel) updateForm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.form = nil
		return m, nil
	case "enter", "ctrl+s":
		return m.submitForm()
	case "tab", "down":
		m.form.focus(1)
		return m, textinput.Blink
	case "shift+tab", "up":
		m.form.focus(-1)
		return m, textinput.Blink
	}

	cur := &m.form.fields[m.form.idx]
	switch cur.kind {
	case kindBool:
		switch msg.String() {
		case " ", "x", "left", "right":
			cur.on = !cur.on
		}
		return m, nil
	case kindMech:
		switch msg.String() {
		case " ", "right":
			cur.mech = (cur.mech + 1) % len(saslMechs)
		case "left":
			cur.mech = (cur.mech + len(saslMechs) - 1) % len(saslMechs)
		}
		return m, nil
	case kindSep:
		// Separators are non-interactive; focus() always skips them, so this
		// branch is unreachable in normal use. No-op.
		return m, nil
	default: // kindText
		var cmd tea.Cmd
		cur.input, cmd = cur.input.Update(msg)
		return m, cmd
	}
}

// submitForm validates and saves the form, returning to the list on success.
func (m launcherModel) submitForm() (tea.Model, tea.Cmd) {
	n, err := m.form.toNetwork()
	if err != nil {
		m.form.err = err.Error()
		return m, nil
	}
	m.store.Upsert(n, m.formOrig)
	if err := config.Save(m.store, m.path); err != nil {
		m.status = "save failed: " + err.Error()
	} else {
		m.status = "saved " + n.Name
	}
	m.mode = modeList
	m.form = nil
	if i := indexOfNetwork(m.store.Networks, n.Name); i >= 0 {
		m.sel = i
	}
	return m, nil
}

// indexOfNetwork returns the index of the network named name (case-insensitive),
// or -1.
func indexOfNetwork(nets []config.Network, name string) int {
	for i := range nets {
		if strings.EqualFold(nets[i].Name, name) {
			return i
		}
	}
	return -1
}

// updateConfirm handles the delete confirmation prompt.
func (m launcherModel) updateConfirm(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		if m.sel < len(m.store.Networks) {
			name := m.store.Networks[m.sel].Name
			m.store.Remove(name)
			if err := config.Save(m.store, m.path); err != nil {
				m.status = "save failed: " + err.Error()
			} else {
				m.status = "deleted " + name
			}
		}
		if m.sel > len(m.store.Networks) {
			m.sel = len(m.store.Networks)
		}
		m.mode = modeList
	case "n", "N", "esc":
		m.mode = modeList
	}
	return m, nil
}

func (m launcherModel) View() tea.View {
	v := tea.NewView(renderLauncher(m))
	v.AltScreen = true
	return v
}
