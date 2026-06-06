# CLAUDE.md — working agreement for Lurk

**Lurk** is an IRCv3 client in Go (module `github.com/exec/lurk`, Go 1.26). The
client library and TUI are feature-complete; work now is maintenance: bug fixes,
hardening, small features, and docs. Read `docs/ARCHITECTURE.md` for the layout
before changing anything.

## Hard constraints (these change how you verify)

- **You cannot reach a live IRC server from here.** The live test
  (`client/live_test.go`) is gated on `LURK_TEST_SERVER` and skips by default —
  leave it that way. **Verify everything with hermetic tests** (in-process
  `net.Pipe` mock server + unit tests). The template is
  `client/integration_test.go` (scripted `mockServer` + `ConnectConn`); the TUI
  tests drive `Update`/`View` with synthetic events. Every new behavior gets a
  hermetic test.
- **Dependency rule:** only `tui/` and `cmd/lurk` may import the charm libraries
  (`charm.land/{bubbletea,bubbles,lipgloss}/v2`). The protocol packages (`irc`,
  `conn`, `cap`, `sasl`, `isupport`, `client`) and the `config`/`chatlog`
  helpers stay **standard-library only**.
- Upstream references, if you want them (do not vendor/commit): Bubble Tea v2
  <https://github.com/charmbracelet/bubbletea> (import path `charm.land/bubbletea/v2`);
  Ergo <https://github.com/ergochat/ergo>; the IRCv3 specs <https://ircv3.net>.

## Commands

```sh
go build ./...
go vet ./...
gofmt -l irc conn cap sasl isupport client tui cmd config chatlog server bouncer backlog   # must print nothing
go test -race ./...                                                  # must be all green
```

Definition of done for any change: build + vet clean, `gofmt` clean, and
`go test -race ./...` green. Never commit with failing or skipped-because-broken
tests. End commit messages with the project's `Co-Authored-By` trailer.

## Architecture (where things live)

```
irc/       wire protocol: Message, parse/serialize, tags, numerics, commands   (stdlib)
conn/      TCP/TLS transport, framed read/write, send queue                    (stdlib)
cap/       capability negotiation state machine                                (stdlib)
sasl/      SASL mechanisms (PLAIN, EXTERNAL)                                    (stdlib)
isupport/  RPL_ISUPPORT parsing + casemapping                                  (stdlib)
client/    high-level client: registration, Events() stream, state tracking,
           auto-reconnect, CTCP replies, terminal-escape sanitization          (stdlib)
config/    on-disk network/identity config (JSON, 0600), XDG paths             (stdlib)
chatlog/   per-target plain-text chat logs                                     (stdlib)
server/    lurkd client-facing IRC server: TLS listener, CAP/SASL server-side,
           session mux, upstream manager, cursor store, self-signed cert gen   (stdlib)
bouncer/   soju.im/bouncer-networks server side: BOUNCER verb dispatcher,
           netid allocation, BATCH responder, bouncer-networks-notify          (stdlib)
backlog/   structured JSONL CHATHISTORY store: per-(netid,target) ring +
           JSONL files, server-assigned msgids, rehydration on restart         (stdlib)
tui/       Bubble Tea v2 terminal UI (multi-network)        (the charm-importing package)
cmd/lurk/  binary: TUI by default, -plain for the line client, launcher w/o -server
cmd/lurkd/ bouncer daemon: headless, stdlib-only; SIGINT/SIGTERM graceful shutdown
```

Key contracts:
- `irc.Message{Tags, Source, Command, Params}` flows everywhere; `Tags`
  preserves IRCv3 message tags (`@time`, `@batch`, `@+typing` are parsed).
- `client.Events() <-chan Event` is the buffered stream the TUI consumes
  (`Event.Time()/Nick()/User()/Host()/Param(i)/Text()/Command()/Tags`). It never
  blocks the read loop (drop-oldest on overflow).
- All server-controlled text reaches the screen through `client.SanitizeTerminal`.
- The TUI is a single Bubble Tea `model` (`tui/model.go`); `Update` is the only
  place state mutates. Components are value types — reassign and thread the
  returned `cmd`. It holds multiple networks at once (one `client.Client` each).

## Conventions

- Match the surrounding style: thorough godoc on exported identifiers,
  table-driven tests, errors wrapped with `%w`, no panics in library paths.
- v2 gotchas: `Update` returns `(tea.Model, tea.Cmd)`; `View()` returns
  `tea.View` (wrap with `tea.NewView`, set `.Cursor`); keys are
  `tea.KeyPressMsg` matched via `msg.String()` ("enter", "esc", "ctrl+u", "up"),
  with `msg.Mod`/`msg.Code` for modified keys.
- Prefer additive changes to public contracts.
- Credit non-obvious borrowed design in a comment (see `CREDITS.md`).

## Docs map

- `docs/ARCHITECTURE.md` — packages, dependency direction, the core contracts.
- `docs/ARCHITECTURE-TUI.md` — the Bubble Tea model, file map, event bridge.
- `docs/LURKD.md` — the lurkd bouncer operator guide (install, config reference,
  TLS, auth, connecting clients, runtime network management, operations).
- `docs/LURKD-DESIGN.md` — the lurkd bouncer design (all 16 ballot items,
  package layout, security model, phased build order).
- `docs/PACKAGING.md` — building native packages and the release workflow.
- `CHANGELOG.md` — released history; `TODO.md` — forward-looking roadmap.
