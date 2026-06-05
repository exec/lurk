# Architecture — Terminal UI

The terminal UI (`tui/`) is a [Bubble Tea v2](https://github.com/charmbracelet/bubbletea)
program layered on the `client` library. It is, with `cmd/lurk`, the only package
allowed to import the charm libraries (`charm.land/{bubbletea,bubbles,lipgloss}/v2`).

## The Elm model

The whole UI is one Bubble Tea `model` (`tui/model.go`). State mutates in exactly
one place — `Update(msg) (tea.Model, tea.Cmd)`. The bubbles components
(`viewport`, `textinput`, `list`, …) are value types: they are reassigned and the
`tea.Cmd` they return is threaded back out. `View()` returns a `tea.View` built by
the renderer in `view.go`.

A few Bubble Tea v2 specifics the code relies on: keys arrive as
`tea.KeyPressMsg`, matched via `msg.String()` (`"enter"`, `"esc"`, `"ctrl+u"`,
…) with `msg.Mod`/`msg.Code` for modified keys (`String()` drops the modifier for
printable keys); the background color is probed with `tea.RequestBackgroundColor`;
the window size arrives as `tea.WindowSizeMsg`.

## Multiple networks

Lurk holds several networks at once. Each `network` (`tui/network.go`) owns its
own `client.Client` and `Events()` subscription. The model keeps a single flat
list of buffers — channels, PMs, and one server/status buffer per network —
grouped by network in the sidebar; one client is the "active" target for input.
Inbound events are network-scoped: the per-network event pump tags each event
with its origin so routing only touches that network's buffers
(`tui/events.go`). `/connect <name>` dials another saved network at runtime and
folds it into the unified sidebar; `/disconnect` drops the current one.

## File map

| File | Responsibility |
|---|---|
| `app.go` | root `tea.Model`: `Init`/`Update`/`View` dispatch, program wiring, per-network event bridge, key dispatch |
| `model.go` | the `model` struct and buffer/network bookkeeping (create/switch/close, active-index math) |
| `events.go` | `client.Event` → `tea.Msg` bridge (`waitForIRC`) and per-network event routing |
| `buffer.go` | a `Buffer` (channel/PM/server): scrollback, unread/highlight counters, the read marker |
| `view.go` | layout: sidebar + viewport + nicklist + topic/status bars + input |
| `style.go` | Lip Gloss theme, per-nick colorization, line/numeric formatting, render-layer sanitization |
| `input.go` | the `textinput` editor: history, tab-completion |
| `command.go` | the slash-command parser and handlers (`/join`, `/msg`, `/mode`, `/kick`, …) |
| `keys.go` | keybindings and the on-screen help footer |
| `network.go`, `connect.go` | the `network` type and the runtime `/connect` flow |
| `launcher.go`, `launcher_form.go` | the pre-connect network launcher (a separate Bubble Tea program) |
| `channellist.go` | the `/list` channel-directory modal (bubbles/list + a Lip Gloss compositor overlay) |
| `search.go` | scrollback search (`/search`, Ctrl-R to cycle matches) |
| `nickmenu.go` | the per-user context menu (message / whois / op / voice / kick) |

## The event bridge

`client.Events()` is the linchpin: a buffered channel of every protocol event.
For each network the model runs a `waitForIRC` command that receives one event
and re-issues itself, turning the stream into a sequence of `tea.Msg`s that
`Update` routes. Because the client never blocks its read loop (drop-oldest on
overflow) and Bubble Tea serializes `Update`, the UI and the network I/O stay
fully decoupled.

## Theme & safety

The theme follows the terminal: a dark (Catppuccin Mocha) or light (Latte)
palette is chosen from the detected background, and `NO_COLOR` selects a
monochrome, attribute-only theme. Every server-controlled string — messages,
nicks, topics, channel and network names — is passed through the shared
`client.SanitizeTerminal` at the render layer, so a malicious server cannot
inject terminal escape sequences through any display path (message body,
nicklist, sidebar, status bar, or menus).

## Conventions

- Keep charm imports out of every non-`tui` package.
- All `model` mutation funnels through `Update`; recover-and-show on a render
  panic rather than killing the terminal.
- Tests drive `Update`/`View` (and the render helpers) with synthetic events and
  assert on the rendered output — hermetic, no real server.
