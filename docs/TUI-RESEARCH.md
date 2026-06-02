# Bubble Tea (v2) — deep study for Lurk's TUI (Cycle 2)

The Cycle-2 TUI is built on **Bubble Tea v2** (the Elm Architecture for Go),
with **Bubbles** (components) and **Lip Gloss** (layout/styling). This is the
working reference distilled from the cloned source at `reference/{bubbletea,
bubbles,lipgloss}/` and the IRCv3 TUI client `reference/senpai/`. Read those for
exact signatures; this doc is the mental model + the load-bearing decisions.

> ⚠️ **Import paths moved to `charm.land`.** v2 modules are
> `charm.land/bubbletea/v2`, `charm.land/bubbles/v2`, `charm.land/lipgloss/v2`
> — NOT `github.com/charmbracelet/...`. Pin: bubbletea v2.0.7, lipgloss v2.0.3,
> bubbles (matching). `go get charm.land/bubbletea/v2@v2.0.7` resolves via the
> vanity path. These are our FIRST external dependencies — keep them confined to
> the `tui/` package; the protocol/client packages stay stdlib-only.

## 1. The Elm Architecture (v2 specifics)

A program is a `Model`:

```go
type Model interface {
    Init() tea.Cmd                       // initial side-effects
    Update(tea.Msg) (tea.Model, tea.Cmd) // handle one event, return new state + next effect
    View() tea.View                      // render (v2: returns tea.View, NOT string)
}
```

Key v2 differences from v1 the agents WILL trip on:
- **`View() tea.View`**, not `string`. Wrap content with `tea.NewView(s)`. A
  `tea.View` carries an optional `Cursor` (set `v.Cursor = ...` to place the
  hardware cursor — see the chat example offsetting the textarea cursor by the
  viewport height).
- **Keys are `tea.KeyPressMsg`**, not `tea.KeyMsg`. Use `msg.String()` ("enter",
  "ctrl+c", "tab", "up", …) for matching. There is also `tea.KeyReleaseMsg`
  under the keyboard-enhancements protocol; we only need press.
- **`tea.WindowSizeMsg`** (`.Width`, `.Height`) arrives on start and on resize —
  the single source of truth for layout dimensions. Recompute all component
  sizes here.
- Components are **value types**: `m.viewport, cmd = m.viewport.Update(msg)` —
  reassign, then return the `cmd`. Forgetting to thread the returned model/cmd is
  the #1 bug.

Program lifecycle:

```go
p := tea.NewProgram(initialModel(), tea.WithAltScreen(), tea.WithMouseCellMotion())
if _, err := p.Run(); err != nil { ... }
```

`tea.Quit` is the command that exits. `tea.Batch(cmds...)` runs several;
`tea.Sequence(...)` runs them in order.

## 2. THE integration crux: feeding IRC events into the loop

Our `lurk/client` runs its own read goroutine and dispatches events. The TUI must
receive those *inside* `Update`. Two supported mechanisms (we use BOTH, for
different things):

### (a) Channel + re-subscribing Cmd — the idiomatic subscription (PRIMARY)
From `reference/bubbletea/examples/realtime/main.go`. Keep the model pure and
testable — it holds a channel, not a `*Program`:

```go
// client side: expose a stream. Add to lurk/client:
//   func (c *Client) Events() <-chan Event   // buffered fan-out of all events
func waitForIRC(sub <-chan client.Event) tea.Cmd {
    return func() tea.Msg { return ircMsg(<-sub) } // blocks; BT runs it async
}
// In Init(): return waitForIRC(m.sub)
// In Update(), case ircMsg: mutate buffers, THEN return m, waitForIRC(m.sub)
```
The re-issue after each event is mandatory — that's what keeps the subscription
alive. One in-flight `waitForIRC` Cmd at a time; no races.

### (b) `Program.Send(msg)` — goroutine-safe push (SECONDARY)
`func (p *Program) Send(msg Msg)` (tea.go:1183) is safe to call from any
goroutine. Useful for one-off injections (e.g. a SIGWINCH-like nudge, or wiring
a callback that can't be modeled as a channel). Downside: the model needs the
`*Program`, which hurts testability. Prefer (a); reserve (b) for glue.

**Decision:** add `Client.Events() <-chan Event` to the library (buffered, drop-
oldest or block-with-backpressure — coordinate the policy). The TUI consumes it
via pattern (a). Do NOT block the client's read loop on a slow UI: the channel
must be buffered and the producer must not deadlock if the UI stalls.

## 3. Components we'll use (Bubbles)

`reference/bubbles/` has: `viewport`, `textarea`, `textinput`, `list`,
`key`, `help`, `spinner`, `table`, `paginator`, `cursor`, …

- **`viewport`** — the scrollback pane. `viewport.New(viewport.WithWidth(w),
  WithHeight(h))`, `SetContent(string)`, `GotoBottom()`, `ScrollUp/Down`,
  `AtBottom()`. Pre-wrap content to the viewport width with
  `lipgloss.NewStyle().Width(w).Render(text)` BEFORE `SetContent` (the chat
  example does this). Track "stick to bottom unless the user scrolled up".
- **`textinput`** — single-line input box (our message editor). `Value()`,
  `SetValue`, `Reset`, `Focus`, `Blur`, `CharLimit`, `Prompt`. For multiline use
  `textarea`; for an IRC input single-line `textinput` is right.
- **`list`** — the buffer/channel sidebar and/or nick list. `list-fancy` and
  `list-simple` examples show item interfaces, filtering, delegates. For a dense
  IRC sidebar a custom Lip Gloss render may be lighter than full `list` — agents
  decide; `list` is fine to start.
- **`key`** — declarative keybindings (`key.NewBinding(key.WithKeys("ctrl+n"),
  WithHelp(...))`) feeding **`help`** for a footer hint bar.

## 4. Layout & styling (Lip Gloss)

- `lipgloss.NewStyle()` with `.Width/.Height/.Padding/.Margin/.Border/.Foreground
  /.Background/.Bold`. Styles are immutable/chainable.
- Compose panes: `lipgloss.JoinHorizontal(lipgloss.Top, sidebar, main, nicks)`
  and `lipgloss.JoinVertical(lipgloss.Left, header, body, input)`. `lipgloss.
  Place(w,h,...)` to position within a box. `lipgloss.Width/Height(s)` to measure.
- The classic IRC layout to target:
  ```
  ┌──────────┬─────────────────────────────┬────────┐
  │ buffers  │  message scrollback         │ nicks  │  <- JoinHorizontal
  │ (#chan)  │  (viewport)                 │ (list) │
  │ *active  │                             │        │
  ├──────────┴─────────────────────────────┴────────┤
  │ statusbar: net/nick/lag/mode                     │
  ├──────────────────────────────────────────────────┤
  │ > input textinput                                │
  └──────────────────────────────────────────────────┘
  ```
  All inner widths/heights derive from the latest `WindowSizeMsg`. Reserve rows
  for statusbar(1)+input(1) and columns for sidebar(~16-20)+nicks(~16); the
  viewport gets the remainder.
- **Color downsampling** is automatic (truecolor → 256 → 16 → mono by terminal).
  Use `lipgloss.Color("…")`; don't hardcode ANSI.

## 5. Concepts to model (from senpai, an IRCv3 TUI — `reference/senpai/`)

senpai is tcell-based (not Bubble Tea) but is an excellent UX reference. Study
its decomposition, implement in Bubble Tea:
- **Buffers** (`senpai/ui/buffers.go`): one window per channel/PM/server, each
  with its own scrollback, unread/highlight counters, member list. The sidebar
  lists buffers with activity markers. Ctrl-N/Ctrl-P or Alt-<n> to switch.
- **Editor** (`senpai/ui/editor.go`): input line with history, cursor movement,
  and **tab-completion** (nicks, then channels, then commands).
- **Commands** (`senpai/commands.go`): `/join /part /msg /query /nick /me /topic
  /names /quit /close /raw …` parsed from the input; non-slash lines → PRIVMSG to
  the active buffer. A leading `//` escapes to a literal message.
- **Colorization** (`senpai/ui/colors.go`): stable per-nick color hashing; render
  own nick distinctly; dim joins/parts; highlight lines mentioning your nick.
- **Activity & highlights**: unread counts per buffer; bell/notify on highlight.
- `senpai/irc/` shows the protocol surface a TUI needs (events, tokens, rpl, typing) —
  cross-check against what our `lurk/client` already exposes; request additions
  from the `client` agent rather than reaching into protocol internals.

## 6. Testing a Bubble Tea app

- Models are plain values: unit-test `Update` by feeding synthetic msgs
  (`ircMsg{...}`, `tea.KeyPressMsg{...}`, `tea.WindowSizeMsg{Width,Height}`) and
  asserting on resulting state/`View()`. No terminal needed.
- For end-to-end, charm ships **teatest** (`charm.land/x/exp/teatest` —
  check the exact path in the cloned deps) to drive a program and assert on
  output/golden frames. Use it for a smoke test: feed a scripted IRC event
  stream, assert the buffer renders.
- The TUI is hard to assert pixel-exactly; prioritize `Update` unit tests +
  a golden-frame smoke test + manual runs against the live Ergo
  (10.0.0.116, see docs/TEST-SERVER.md).

## 7. Gotchas checklist
- Return the cmd from every component `Update`; reassign the component value.
- Re-issue `waitForIRC` after each `ircMsg` or the stream dies.
- Pre-wrap viewport content to width; call `GotoBottom()` only when stuck-to-bottom.
- Recompute sizes on EVERY `WindowSizeMsg`; never hardcode 80x24.
- `View()` returns `tea.View`; set `.Cursor` for the input caret.
- Keep charm deps inside `tui/`; protocol packages stay dependency-free.
- Don't block the client read loop on the UI channel — buffer it.
- Alt-screen (`tea.WithAltScreen()`) for a full-screen app; restore on quit (BT
  handles it, but ensure `tea.Quit` is reached on Ctrl-C and on client disconnect).
