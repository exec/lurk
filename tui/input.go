package tui

import (
	"sort"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// renderInput renders the editor pane: the textinput's view padded to the full
// terminal width and the reserved input height, so the bottom row aligns under
// the panes above. This is the seam view.go uses to place the editor in the
// frame (the layout() pass has already sized the editor to m.width).
func renderInput(m model) string {
	return lipgloss.NewStyle().
		Width(m.width).
		Height(inputHeight).
		MaxHeight(inputHeight).
		Render(m.input.View())
}

// inputCursor returns the hardware cursor position for the editor caret, offset
// into the full frame. The input pane is the bottom row, sitting below the
// optional topic bar, the message region, the status bar, and the help footer
// (see verticalLayout). The textinput's own Cursor already accounts for the
// prompt and the horizontal scroll offset, so only the vertical offset is added
// here; the pane starts at column 0 so X needs no adjustment.
func inputCursor(m model) *tea.Cursor {
	c := m.input.Cursor()
	if c == nil {
		return nil
	}
	topicH, bodyH := verticalLayout(m)
	c.Y += topicH + bodyH + statusHeight + helpHeight
	return c
}

// input.go owns the message editor: the textinput configuration, key handling
// on the input line (submit, history recall, tab-completion), and the action
// value the core applies. The editor itself is the bubbles textinput component
// (single-line, the right fit for IRC); history and completion are layered on
// top, modeled on senpai's editor (reference/senpai/ui/editor.go and
// completions.go) but reimplemented against textinput's runes.

// actionKind enumerates the control actions the input layer asks the core to
// perform. The core's applyAction (app.go) is the single place these take
// effect, so input.go stays focused on parsing and editing.
type actionKind int

const (
	// actionNone means "no control action" — the model was already mutated in
	// place (e.g. history navigation) or the command handled its own side effect.
	actionNone actionKind = iota
	// actionSend sends a PRIVMSG. An empty target means the active buffer. The
	// core echoes locally when echo-message is not negotiated.
	actionSend
	// actionSwitch focuses an existing buffer by target name, or by index when
	// target is empty.
	actionSwitch
	// actionOpen ensures a buffer for target (of bufferKind) exists and focuses
	// it.
	actionOpen
	// actionClose closes the buffer named by target, or the active buffer when
	// target is empty. The server buffer is never closed.
	actionClose
	// actionInfo appends a local informational line (command usage, errors) to
	// the active buffer.
	actionInfo
	// actionListOpen opens the channel-directory modal (/list). The LIST request
	// has already been sent; the core opens the modal in its loading state and
	// the RPL_LIST replies populate it (channellist.go).
	actionListOpen
	// actionSearch runs a scrollback search with the text in the action; the core
	// applies it (mutating the model's search state, which a command handler
	// cannot persist).
	actionSearch
)

// action is the control value handleInput returns for the core to apply. Its
// shape is the contract agreed with tui-core; not every field is meaningful for
// every kind (see actionKind for which fields each uses).
type action struct {
	kind       actionKind
	target     string
	text       string
	index      int
	bufferKind BufferKind
}

// newInput builds the configured message editor. The prompt and char limit
// match a typical IRC client; the editor starts focused because the TUI has a
// single input and no other focusable widget competes for keys.
func newInput() textinput.Model {
	ti := textinput.New()
	ti.Prompt = "> "
	// Allow composing/pasting a long message; the client splits anything over a
	// single wire line into multiple PRIVMSGs on send (client.Privmsg). The cap
	// just bounds pathological input.
	ti.CharLimit = 4000
	ti.Focus()
	return ti
}

// handleInput processes one key press while the editor is focused. It returns
// the (possibly mutated) model, a control action for the core to apply, and any
// Cmd. The global keys (quit, buffer switching) are handled earlier in app.go;
// here we own submit, history, completion, and the editor's own editing keys.
//
// Order matters: completion and history are intercepted before the key reaches
// the textinput, because Tab/Up/Down would otherwise be swallowed (or ignored)
// by the component.
func handleInput(m model, msg tea.KeyPressMsg) (model, action, tea.Cmd) {
	switch {
	case msg.String() == "enter":
		return submit(m)

	case key_matches(m.keys.Complete, msg):
		m.input = complete(m)
		return m, action{kind: actionNone}, nil

	case key_matches(m.keys.HistPrev, msg):
		m = historyPrev(m)
		return m, action{kind: actionNone}, nil

	case key_matches(m.keys.HistNext, msg):
		m = historyNext(m)
		return m, action{kind: actionNone}, nil
	}

	// Any other key edits the line; forward to the textinput and thread its Cmd.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, action{kind: actionNone}, cmd
}

// submit handles Enter: it reads the current line, records it in history, clears
// the editor, and parses the line into an action (+ Cmd) via command.go. A blank
// line is a no-op that still resets any in-progress history navigation.
func submit(m model) (model, action, tea.Cmd) {
	line := m.input.Value()
	if strings.TrimSpace(line) == "" {
		m.input.Reset()
		m.histIndex = len(m.history)
		return m, action{kind: actionNone}, nil
	}

	m = pushHistory(m, line)
	m.input.Reset()

	act, cmd := runLine(m, line)
	return m, act, cmd
}

// pushHistory appends line to the input history (skipping an immediate
// duplicate of the last entry, as a shell does) and resets the navigation
// cursor to just past the end so the next Up recalls the line just sent.
func pushHistory(m model, line string) model {
	if n := len(m.history); n == 0 || m.history[n-1] != line {
		m.history = append(m.history, line)
	}
	m.histIndex = len(m.history)
	return m
}

// historyPrev recalls an earlier history entry into the editor. At the oldest
// entry it stays put. When already typing a fresh line it is lost — matching the
// simple recall behavior of most line editors (senpai keeps it likewise minimal).
func historyPrev(m model) model {
	if len(m.history) == 0 {
		return m
	}
	if m.histIndex > 0 {
		m.histIndex--
	}
	m.input.SetValue(m.history[m.histIndex])
	m.input.CursorEnd()
	return m
}

// historyNext moves toward more recent history; stepping past the newest entry
// clears the editor to an empty fresh line (the conventional "back to the
// prompt" behavior).
func historyNext(m model) model {
	if len(m.history) == 0 {
		return m
	}
	if m.histIndex < len(m.history) {
		m.histIndex++
	}
	if m.histIndex >= len(m.history) {
		m.input.SetValue("")
		return m
	}
	m.input.SetValue(m.history[m.histIndex])
	m.input.CursorEnd()
	return m
}

// complete performs tab-completion on the word under the cursor and returns the
// updated editor. It cycles deterministically: the candidate set is computed
// from the current word, sorted, and the next match substituted. The priority,
// following senpai, is nicks (in a channel) first, then channels, then command
// names — but the priority only decides ordering within the candidate list; the
// word's position (first word vs later, leading '/') narrows what is offered.
//
// Cycling is stateless: rather than tracking an index across keystrokes, each
// Tab replaces the current word with the candidate that sorts immediately after
// it, wrapping around. Pressing Tab on a word that is already a full candidate
// advances to the next, so repeated Tab walks the list.
func complete(m model) textinput.Model {
	ti := m.input
	value := ti.Value()
	runes := []rune(value)
	cursor := ti.Position()
	if cursor > len(runes) {
		cursor = len(runes)
	}

	start := wordStart(runes, cursor)
	word := string(runes[start:cursor])

	candidates := completionCandidates(m, word, start == 0)
	if len(candidates) == 0 {
		return ti
	}

	next := nextCandidate(candidates, word)

	// A nick completed at the very start of the line gets the conventional
	// "nick: " address form; elsewhere a trailing space lets the user keep typing.
	suffix := " "
	if start == 0 && !strings.HasPrefix(next, "/") && !isChannel(next) {
		suffix = ": "
	}
	replacement := next + suffix

	newRunes := make([]rune, 0, len(runes)-(cursor-start)+len([]rune(replacement)))
	newRunes = append(newRunes, runes[:start]...)
	newRunes = append(newRunes, []rune(replacement)...)
	newRunes = append(newRunes, runes[cursor:]...)

	ti.SetValue(string(newRunes))
	ti.SetCursor(start + len([]rune(replacement)))
	return ti
}

// wordStart returns the index of the start of the word ending at cursor: the
// position just after the previous space, or 0.
func wordStart(runes []rune, cursor int) int {
	start := cursor
	for start > 0 && runes[start-1] != ' ' {
		start--
	}
	return start
}

// completionCandidates builds the sorted candidate list for word. When the word
// is the first on the line and begins with '/', it offers command names. A
// first word beginning with '#' offers joined channels. Otherwise it offers the
// active channel's members (nicks), then channels, then commands — the senpai
// priority — filtered by the word as a case-insensitive prefix.
//
// firstWord indicates the word is at the start of the line, which is what makes
// command and "nick:" completion appropriate.
func completionCandidates(m model, word string, firstWord bool) []string {
	// Command completion: a slash-led first word.
	if firstWord && strings.HasPrefix(word, "/") {
		return commandCandidates(word[1:])
	}

	var out []string
	lower := strings.ToLower(word)

	// Nicks of the active channel come first.
	if m.cli != nil {
		if ch := m.activeBuffer().Title; isChannel(ch) {
			for _, mem := range m.cli.Members(ch) {
				if hasPrefixFold(mem.Nick, lower) {
					out = append(out, mem.Nick)
				}
			}
		}
		// Then channels we are in.
		for _, ch := range m.cli.Channels() {
			if hasPrefixFold(ch, lower) {
				out = append(out, ch)
			}
		}
	}
	// Then command names (so "/" mid-completion still resolves), only if the
	// word looks command-like at line start.
	if firstWord && word == "" {
		out = append(out, commandCandidates("")...)
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

// commandCandidates returns the "/"-prefixed command names matching the given
// (slash-stripped) prefix, sorted, lower-cased for display.
func commandCandidates(prefix string) []string {
	up := strings.ToUpper(prefix)
	var out []string
	for name := range commands {
		if strings.HasPrefix(name, up) {
			out = append(out, "/"+strings.ToLower(name))
		}
	}
	sort.Strings(out)
	return out
}

// nextCandidate returns the candidate to substitute for word: the one that
// sorts strictly after the current word (case-insensitively), wrapping to the
// first when word already is (or sorts at/after) the last. This gives stateless
// Tab cycling — each press advances one step through the sorted list.
func nextCandidate(candidates []string, word string) string {
	lw := strings.ToLower(word)
	for _, c := range candidates {
		if strings.ToLower(c) > lw {
			return c
		}
	}
	return candidates[0]
}

// hasPrefixFold reports whether s starts with the lower-cased prefix, comparing
// case-insensitively. prefix is expected already lower-cased by the caller.
// Lowercasing the whole of s (rather than byte-slicing it to the prefix length)
// keeps the comparison correct for multibyte nicks, where a byte slice could
// split a rune or mismatch the lower-cased length.
func hasPrefixFold(s, lowerPrefix string) bool {
	return strings.HasPrefix(strings.ToLower(s), lowerPrefix)
}
