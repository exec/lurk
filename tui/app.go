package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"lurk/chatlog"
	"lurk/client"
)

// Run builds the TUI model around an already-connected client and runs the
// Bubble Tea program until the user quits (Ctrl-C / /quit) or the connection
// ends. It owns the program lifecycle: alt-screen, mouse cell motion, and a
// clean teardown that sends QUIT before returning.
//
// The caller is responsible for dialing and registering the client (Connect)
// before calling Run; Run drives the client's Run loop implicitly by consuming
// its event stream.
func Run(ctx context.Context, cli *client.Client, logDir string, connect ConnectFunc) error {
	m := newModel(cli, cli.Events())
	m.connect = connect
	if logDir != "" {
		m.logger = chatlog.New(logDir)
		defer m.logger.Close()
	}
	// In Bubble Tea v2 the alternate screen and mouse mode are properties of the
	// rendered tea.View (set in View), not program options, so the program only
	// needs the cancellation context here.
	p := tea.NewProgram(m, tea.WithContext(ctx))
	_, err := p.Run()
	return err
}

// Init starts the event subscription and the editor cursor blink. The single
// in-flight waitForIRC Cmd is the heartbeat of the IRC->UI bridge; it is
// re-issued after every ircMsg in Update.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		textinput.Blink,
		// Ask the terminal for its background color so the theme can match a light
		// or dark terminal (answered via tea.BackgroundColorMsg in Update).
		tea.RequestBackgroundColor,
	}
	// One event pump per connected network.
	for _, n := range m.networks {
		cmds = append(cmds, waitForIRC(n))
	}
	return tea.Batch(cmds...)
}

// Update handles one message and returns the next model and command. tui-core
// owns the top-level dispatch: window sizing, the IRC bridge, and quit. Key
// presses are delegated to the input layer (tui-input), which returns a
// control action describing what the core should do.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.handleResize(msg)

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case ircMsg:
		// Apply the event to its network's buffers, then re-subscribe to that
		// network's stream to keep it alive. A highlight in an unfocused buffer
		// also rings the terminal bell.
		m = routeEventOn(m, msg.net, msg.ev)
		cmds := []tea.Cmd{waitForIRC(msg.net)}
		if m.bell {
			cmds = append(cmds, bellCmd())
			m.bell = false
		}
		return m, tea.Batch(cmds...)

	case connectedMsg:
		// A /connect dial succeeded: fold the new network in and start its pump.
		return m.addConnected(msg.name, msg.cli)

	case connectErrMsg:
		m = appendInfo(m, m.activeBuffer(), fmt.Sprintf("connect %s failed: %v", msg.name, msg.err))
		return m, nil

	case tea.BackgroundColorMsg:
		// The terminal answered our background-color query: match the theme to a
		// light or dark terminal (unless NO_COLOR has disabled color).
		applyDetectedBackground(msg.IsDark())
		return m, nil

	case ircClosedMsg:
		// One network's connection ended for good: drop it. Quit only when no
		// networks remain.
		m = m.removeNetwork(msg.net)
		if len(m.networks) == 0 {
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil

	default:
		// While the channel-list modal is open it owns ancillary messages (its
		// filter-input blink, status-message timers, filtering results).
		if m.chanListOpen {
			var cmd tea.Cmd
			m.chanList, cmd = m.chanList.Update(msg)
			return m, cmd
		}
		// Forward anything else (cursor blink, paste, etc.) to the editor so the
		// caret keeps blinking and bracketed paste works.
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}
}

// handleResize records the new terminal dimensions, marks the model ready, and
// recomputes component sizes via the view layer. It runs on the first
// WindowSizeMsg and every resize after.
func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height
	m.ready = true
	m = layout(m) // view.go: resize viewport, input, recompute panes
	return m, nil
}

// handleKey processes a key press. Global keys (quit, buffer switching) are
// handled by the core; everything else is passed to the input layer, which
// returns an action telling the core what to do (send a message, run a
// command, switch/close a buffer) plus any Cmd to run.
func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Quit is always available, even from the nicklist or an open menu.
	if key_matches(m.keys.Quit, msg) {
		m.quitting = true
		return m, tea.Quit
	}

	// The channel-list modal is a full overlay: while open it owns every key
	// (navigation, filtering, Enter to join, Esc to close) ahead of scrollback,
	// the nicklist, and the editor.
	if m.chanListOpen {
		return m.handleChannelListKey(msg)
	}

	// Ctrl-R cycles to the next (older) scrollback-search match when a search is
	// active. It is consumed here so the editor never sees it.
	if msg.String() == "ctrl+r" && len(m.searchMatches) > 0 {
		m.cycleSearch()
		return m, nil
	}

	// Scrollback navigation works in any focus mode: its keys (Shift/Ctrl+arrows,
	// PgUp/PgDn) don't collide with menu/nicklist/editor keys, so it is handled
	// before the focus-specific branches.
	if m.scrollActive(msg) {
		return m, nil
	}

	// The context menu and nicklist focus capture navigation keys while active
	// (nickmenu.go), so they take precedence over editor/buffer keys.
	if m.menuOpen {
		return m.handleMenuKey(msg)
	}
	if m.focus == focusNicks {
		return m.handleNickFocusKey(msg)
	}

	// Alt-based buffer navigation, matched on Mod/Code directly because String()
	// drops the modifier for a printable key (so key.Matches is unreliable here):
	// Alt+1…9 jump by position, Alt+A jumps to the next buffer with activity.
	if msg.Mod == tea.ModAlt {
		if idx, ok := altDigitIndex(msg); ok {
			if idx < len(m.buffers) {
				m.switchTo(idx)
				m = layout(m)
			}
			return m, nil
		}
		if msg.Code == 'a' {
			m.jumpToActive()
			m = layout(m)
			return m, nil
		}
	}

	switch {
	case key_matches(m.keys.FocusNicks, msg):
		m = m.enterNickFocus()
		return m, nil
	case key_matches(m.keys.NextBuffer, msg):
		m.nextBuffer()
		m = layout(m)
		return m, nil
	case key_matches(m.keys.PrevBuffer, msg):
		m.prevBuffer()
		m = layout(m)
		return m, nil
	}

	// Editor / command handling lives in tui-input. It returns a possibly
	// mutated model, an action for the core to apply, and a Cmd.
	before := m.input.Value()
	m, act, cmd := handleInput(m, msg)
	m = applyAction(m, act)
	m = m.maybeSendTyping(before, act)
	return m, cmd
}

// altDigitIndex reports whether msg is Alt+1…9 and, if so, the zero-based buffer
// index it selects (Alt+1 → 0). Alt+0 is intentionally unmapped. It reads the
// Mod/Code fields directly because KeyPressMsg.String() omits the modifier for a
// printable key.
func altDigitIndex(msg tea.KeyPressMsg) (int, bool) {
	if msg.Mod != tea.ModAlt {
		return 0, false
	}
	if msg.Code >= '1' && msg.Code <= '9' {
		return int(msg.Code - '1'), true
	}
	return 0, false
}

// scrollLines is how many lines a single Shift/Ctrl+↑/↓ press moves the
// scrollback viewport — a few lines for a readable pace while still allowing
// fine control by tapping.
const scrollLines = 3

// scrollActive applies a scrollback key to the active buffer's viewport and
// reports whether the key was a scroll key (and thus consumed). The buffers are
// pointers, so mutating the viewport in place persists across the returned
// model. A no-op until the viewport has been sized (vpReady).
func (m model) scrollActive(msg tea.KeyPressMsg) bool {
	b := m.activeBuffer()
	if !b.vpReady {
		return false
	}
	switch {
	case key_matches(m.keys.ScrollUp, msg):
		b.vp.ScrollUp(scrollLines)
	case key_matches(m.keys.ScrollDown, msg):
		b.vp.ScrollDown(scrollLines)
	case key_matches(m.keys.PageUp, msg):
		b.vp.PageUp()
	case key_matches(m.keys.PageDown, msg):
		b.vp.PageDown()
	default:
		return false
	}
	return true
}

// typingThrottle is the minimum gap between successive outbound "active" typing
// notifications, so editing a line emits at most one tag every few seconds.
const typingThrottle = 3 * time.Second

// maybeSendTyping emits our own +typing notifications as the user edits the
// input of a channel/PM buffer: a throttled "active" while the (non-command)
// line grows or changes, and a "done" when a message is sent. It is a no-op
// without a client, on the server buffer, or while composing a slash command.
func (m model) maybeSendTyping(before string, act action) model {
	if m.cli == nil || m.activeBuffer().Kind == BufferServer {
		return m
	}
	target := m.activeBuffer().Title
	if act.kind == actionSend {
		_ = m.cli.Typing(target, "done")
		m.lastTypingSent = time.Time{}
		return m
	}
	after := m.input.Value()
	if after == before || after == "" || strings.HasPrefix(after, "/") {
		return m
	}
	now := time.Now()
	if now.Sub(m.lastTypingSent) >= typingThrottle {
		_ = m.cli.Typing(target, "active")
		m.lastTypingSent = now
	}
	return m
}

// applyAction performs the control action the input layer asked for. Keeping
// the protocol side effects here (rather than in input.go) means tui-input
// stays focused on parsing/editing and the core remains the single place that
// touches buffer lifecycle and the client connection.
func applyAction(m model, act action) model {
	switch act.kind {
	case actionNone:
		// nothing to do

	case actionSend:
		// Send a PRIVMSG to the active (or named) target and locally echo it
		// when echo-message is not negotiated.
		target := act.target
		if target == "" {
			target = m.activeBuffer().Title
		}
		if m.cli != nil && target != "" {
			_ = m.cli.Privmsg(target, act.text)
			if !m.cli.CapEnabled("echo-message") {
				m = echoSelf(m, target, act.text)
			}
		}

	case actionSwitch:
		if act.target != "" {
			if i := m.bufferIndex(act.target); i >= 0 {
				m.switchTo(i)
			}
		} else {
			m.switchTo(act.index)
		}
		m = layout(m)

	case actionOpen:
		_, i := m.ensureBuffer(act.target, act.bufferKind)
		m.switchTo(i)
		m = layout(m)

	case actionClose:
		i := m.active
		if act.target != "" {
			i = m.bufferIndex(act.target)
		}
		m.closeBuffer(i)
		m = layout(m)

	case actionInfo:
		// A local informational line for the active buffer (e.g. command usage,
		// errors). Routed through the view layer's append.
		m = appendInfo(m, m.activeBuffer(), act.text)

	case actionListOpen:
		// Open the channel-directory modal in its loading state; the LIST replies
		// (already requested by the command) populate it (channellist.go).
		m = openChannelList(m)

	case actionSearch:
		// Run a scrollback search and report a status line when there is one
		// (a successful jump reports nothing; the status bar shows the position).
		if note := runSearch(&m, act.text); note != "" {
			m = appendInfo(m, m.activeBuffer(), note)
		}
	}
	return m
}

// View renders the current frame. The whole-frame composition lives in the
// view layer (render); app.go only wraps it as a tea.View, places the cursor,
// and keeps the program on the alternate screen.
func (m model) View() tea.View {
	if !m.ready {
		// Before the first WindowSizeMsg we have no dimensions; show a minimal
		// placeholder so the alt-screen is not blank-then-flicker.
		v := tea.NewView("connecting…")
		v.AltScreen = true
		return v
	}
	v := render(m)
	// Full-screen app on the alternate buffer. We deliberately do NOT enable
	// mouse tracking: any mouse mode makes the terminal forward mouse events to
	// the app and disables native click-drag text selection, which would stop
	// users from selecting/copying links, invite codes, and other text. Nicklist
	// interaction is fully keyboard-driven instead (Ctrl-U to focus the Users
	// list, then ↑/↓ and Enter for the context menu).
	v.AltScreen = true
	return v
}

// quitWithError is a small helper for the cmd/lurk entrypoint to format a
// terminal error consistently. It is here (not in cmd/lurk) so the message
// wording stays with the lifecycle code.
func quitWithError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("tui: %w", err)
}
