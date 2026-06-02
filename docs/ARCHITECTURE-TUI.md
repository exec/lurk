# Lurk — Cycle 2 Architecture & Contracts (TUI)

Cycle 2 builds a full-screen terminal IRC client on **Bubble Tea v2** on top of
the Cycle-1 `lurk/client` library. Deep lib study: `docs/TUI-RESEARCH.md`.
Reference source: `reference/{bubbletea,bubbles,lipgloss,senpai}/`.

## Package layout & ownership

```
lurk/
├── client/         (existing) extended with an event STREAM + TUI-support accessors   [owner: client]
├── tui/            Bubble Tea TUI package — the new product
│   ├── app.go      root tea.Model: Init/Update/View, program wiring, event bridge      [owner: tui-core]
│   ├── model.go    app state: buffers map, active buffer, sizes, focus                  [owner: tui-core]
│   ├── events.go   client.Event -> tea.Msg bridge (waitForIRC Cmd)                      [owner: tui-core]
│   ├── buffer.go   a Buffer (channel/PM/server): scrollback lines, members, unread      [owner: tui-view]
│   ├── view.go     layout: sidebar + viewport + nicklist + statusbar + input            [owner: tui-view]
│   ├── style.go    Lip Gloss theme, nick colorization, line formatting                  [owner: tui-view]
│   ├── input.go    textinput editor: history, tab-completion                            [owner: tui-input]
│   ├── command.go  slash-command parser (/join /msg /nick /me /part /quit /raw ...)      [owner: tui-input]
│   └── keys.go     keybindings (bubbles/key) + help footer                              [owner: tui-input]
└── cmd/lurk/       becomes the TUI entrypoint (line-mode client kept behind -plain)     [owner: tui-core]
```

Dependency rule unchanged: **only `tui/` and `cmd/lurk` may import the charm
libs.** `irc`, `conn`, `cap`, `sasl`, `isupport`, `client` stay stdlib-only.

## THE central contract: `client.Events()` (the new linchpin)

The TUI is driven by a stream of events from the client. `client` owns this and
must land it FIRST (like `irc.Message` in Cycle 1). Proposed shape — finalize and
announce to tui-core:

```go
// Events returns a buffered, read-only stream of every protocol event the
// client dispatches (same Events the On/Handle callbacks see). Multiple calls
// MAY return the same shared channel or a fan-out subscription — decide and
// document. MUST NOT block the client read loop if the consumer is slow:
// buffer (e.g. 256) and, on overflow, drop-oldest with a dropped-counter or a
// synthetic "lost N events" marker rather than deadlocking.
func (c *Client) Events() <-chan Event

// Event already exists (Cycle 1). Confirm it carries what the TUI needs:
//   - Command, Nick/User/Host, Params, Text(), Target
//   - Tags (for server-time): Time() time.Time  // @time tag if present, else recv time
//   - a stable kind/semantic for easy switch in the bridge
```

Additional `client` additions the TUI needs (coordinate exact names with tui-*):
- `Client.CapEnabled(name string) bool` — e.g. to know if `echo-message`/`server-time` are on.
- `Client.SendRaw(line string) error` — for `/raw` and anything not yet wrapped.
- `Client.Topic(channel string) (text, setBy string, at time.Time)` — track 332/333/TOPIC.
- Server-time: surface the `@time` tag as `Event.Time()`; negotiate `server-time` cap by default.
- Confirm existing: `Nick()`, `Network()`, `Channels()`, `Members(ch)` (with prefixes), `Join/Part/Privmsg/Notice/SetNick/Quit`.
- Buffers/PMs: the TUI tracks buffers itself, but needs PRIVMSG-to-self routed so it can open a PM buffer (the Event already has Target/Nick — fine).

The in-flight interactive-CLI task already adds `CapEnabled`/`SendRaw`; reuse them.

## tui-core ↔ tui-view ↔ tui-input seams

`tui/` is ONE package (shared `model` struct), so the three owners edit sibling
files and MUST agree on the `model` field set up front. Proposed `model` fields
(tui-core owns the struct; view/input add methods + read fields):

```go
type model struct {
    cli      *client.Client
    sub      <-chan client.Event     // from cli.Events()
    buffers  []*Buffer               // ordered; index 0 = server/status buffer
    active   int                     // index into buffers
    width    int
    height   int
    input    textinput.Model         // tui-input owns behavior
    keys     keymap                  // tui-input
    help     help.Model              // tui-input
    ready    bool                    // got first WindowSizeMsg
    // styles live in style.go (tui-view); buffer rendering in view.go (tui-view)
}
```

- **tui-core** owns: the `model` struct definition, `Init/Update/View` dispatch,
  the event bridge (`waitForIRC`), window-size plumbing, buffer create/switch/close
  logic, program lifecycle (alt-screen, quit on Ctrl-C and on client disconnect),
  and the teatest smoke test. It calls into view/input render + handlers.
- **tui-view** owns: `Buffer` type + scrollback storage, the Lip Gloss layout
  (`func (m model) View() tea.View` body via a `render(m)` helper), styles/theme,
  per-nick color hashing, line formatting (timestamps from `Event.Time()`,
  join/part dimming, highlight on own-nick), statusbar, nicklist, sidebar with
  activity markers.
- **tui-input** owns: the `textinput` config + key handling for the editor,
  input history (up/down), tab-completion (nicks→channels→commands), the
  keymap + help footer, and `command.go` (parse a line → a client action or a
  tui control action). Non-slash line → `cli.Privmsg(activeBuffer, line)`;
  leading `//` → literal message.

Agree the boundary calls explicitly (e.g. `m = view.appendLine(m, buf, line)`,
`action, m := input.handle(m, keymsg)`), message each other before finalizing,
and keep `model` mutations funneled through `Update`.

## Build order & coordination
1. **client** lands `Events()` + the small accessors and ANNOUNCES the exact
   signatures to tui-core (this gates everyone, like the parser did in Cycle 1).
2. **tui-core** scaffolds the model + Update/View skeleton + event bridge against
   the announced `Events()`; defines the `model` struct and shares it.
3. **tui-view** and **tui-input** build their files against the shared `model`,
   coordinating field/seam additions via messages.
4. Integrate; run against live Ergo (10.0.0.116). `go build ./...`, `go vet`,
   `gofmt`, `go test ./...` green (TUI: Update unit tests + teatest golden smoke).

## Conventions
- First external deps enter here: add `charm.land/{bubbletea,bubbles,lipgloss}/v2`
  to go.mod (pinned: bubbletea v2.0.7, lipgloss v2.0.3). `go mod tidy`.
- Keep charm imports out of every non-tui package.
- gofmt/vet/test green; godoc on exported ids; no panics in the run loop;
  recover-and-show on a render panic rather than killing the terminal.
- Credit Bubble Tea / Lip Gloss / Bubbles and senpai in CREDITS.md.
