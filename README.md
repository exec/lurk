<p align="center">
  <img src="assets/lurk.png" alt="lurk" width="480">
</p>

## Status

- **Protocol library** (`irc`, `conn`, `cap`, `sasl`, `isupport`, `client`) —
  message parsing & serialization with full message-tag support, TCP/TLS
  transport, capability negotiation, SASL (PLAIN/EXTERNAL), `RPL_ISUPPORT` +
  casemapping, and a high-level client with registration, event dispatch, and
  channel/user state tracking. Standard library only.
- **Terminal UI** (`tui`, `cmd/lurk`) — a [Bubble Tea](https://github.com/charmbracelet/bubbletea)
  full-screen client: buffer sidebar, scrollback, nicklist, status bar, input
  editor with history and tab-completion, slash commands, and an interactive
  nicklist with a per-user context menu (message, whois, op/voice/kick).
- **IRCv3 features** — capability negotiation plus live handling of
  `away-notify`, `account-notify`/`extended-join`, and `chghost` (away users are
  dimmed and logged-in users badged in the nicklist); `batch`; `draft/chathistory`
  (recent backlog is fetched automatically on join and shown under a history
  divider); `+typing` notifications ("X is typing…" in the status bar, debounced
  outbound); and `standard-replies` (`FAIL`/`WARN`/`NOTE`) rendered readably.

## Install

```sh
go install lurk/cmd/lurk@latest   # or: go build ./cmd/lurk
```

## Run

```sh
# Plaintext
lurk -server irc.example.net:6667 -nick yournick -channel '#chan'

# TLS + SASL
lurk -server irc.example.net:6697 -tls -nick yournick \
     -sasl PLAIN -sasl-user account -sasl-pass secret -channel '#chan'

# Line-mode (no full-screen UI), useful for scripting
lurk -plain -server irc.example.net:6667 -nick yournick -channel '#chan'
```

All flags have `LURK_*` environment-variable equivalents.

### Keys (TUI)

| Key | Action |
|-----|--------|
| type + `Enter` | send to the active buffer |
| `Tab` | complete nick / channel / command |
| `↑` / `↓` | input history |
| `Ctrl-N` / `Ctrl-P` | next / previous buffer |
| `Ctrl-U` | focus the Users list; `↑/↓` select, `Enter` opens the menu, `Esc` back |
| `PgUp` / `PgDn` — **`Fn-↑` / `Fn-↓` on a Mac** | scroll a page (works in every terminal) |
| `Shift-↑` / `Shift-↓` | scroll a few lines (also `Alt-↑/↓`, or `Ctrl-↑/↓` off macOS) |
| `Ctrl-C` | quit |

**Scrolling on macOS:** use `Fn-↑` / `Fn-↓` (these are `PgUp`/`PgDn`) — they reach
the app in every terminal, including Apple's Terminal.app. `Shift-↑/↓` line
scrolling needs a terminal with Kitty-keyboard support (iTerm2, Ghostty, Kitty,
WezTerm, Alacritty); Terminal.app can't send `Shift-↑/↓`, and macOS reserves
`Ctrl-↑/↓` for Mission Control.

Slash commands: `/join /part /msg /query /nick /me /away /whois /topic /names
/close /raw /help /quit` — type `/help` in-app for the keys and command list, or
`/help <command>` for usage.

## Packages

```
irc/       wire protocol: Message, parse/serialize, tags, numerics, commands
conn/      TCP/TLS transport, framed read/write, send queue
cap/       capability negotiation state machine
sasl/      SASL mechanisms (PLAIN, EXTERNAL)
isupport/  RPL_ISUPPORT token parsing + casemapping
client/    high-level client: registration, events, state tracking
tui/       Bubble Tea terminal UI
cmd/lurk/  the client binary (TUI by default, -plain for line mode)
```

## Development

```sh
go build ./...
go test -race ./...
```

An optional live test runs against a real server when `LURK_TEST_SERVER` is set
(see `docs/`). Without it the suite is fully hermetic.

## Credits

Built with the help of, and tested against, [Ergo](https://github.com/ergochat/ergo);
the TUI uses [Bubble Tea](https://github.com/charmbracelet/bubbletea),
[Bubbles](https://github.com/charmbracelet/bubbles), and
[Lip Gloss](https://github.com/charmbracelet/lipgloss), with
[senpai](https://git.sr.ht/~delthas/senpai) as a UX reference. See
[`CREDITS.md`](CREDITS.md).

## License

MIT
