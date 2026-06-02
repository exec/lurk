# CLAUDE.md — working agreement for Lurk

You are continuing work on **Lurk**, an IRCv3 client in Go (module `lurk`).
Read this first, then `docs/PHASE2-3-IMPLEMENTATION.md` for the current task.

## What to build now

Implement **Cycle 3, Phases 2 and 3** — fully specified, step by step with
acceptance criteria, in **`docs/PHASE2-3-IMPLEMENTATION.md`**. Work through it in
order, committing after each phase/step with green tests. Push to `main` when
done (the repo owner expects Phases 2 & 3 on the remote when they return).

## Hard constraints (read these — they change how you verify)

- **You cannot reach the test IRC server.** `docs/TEST-SERVER.md` mentions a
  homelab Ergo at `10.0.0.116` — that is a **private LAN address, unreachable
  from the cloud.** Do NOT try to connect to it. The live test
  (`client/live_test.go`) is env-gated and will skip; leave it that way.
  **Verify everything with hermetic tests** (in-process mock server + unit
  tests). The template is `client/integration_test.go` (`net.Pipe` + a scripted
  `mockServer` + `ConnectConn`). Every new behavior gets a hermetic test.
- **`reference/` is git-ignored and not in this repo.** The deep-dive docs
  reference a local Ergo/Charm/senpai clone that you do NOT have. Everything you
  need is inlined in `docs/PHASE2-3-IMPLEMENTATION.md`. If you want the upstream
  source, you may clone it yourself but DO NOT commit it (it stays ignored):
  - Bubble Tea v2 docs/source: https://github.com/charmbracelet/bubbletea (v2 import path is `charm.land/bubbletea/v2`)
  - Ergo (protocol reference): https://github.com/ergochat/ergo
- **Dependency rule:** only `tui/` and `cmd/lurk` may import the charm libraries
  (`charm.land/{bubbletea,bubbles,lipgloss}/v2`). The protocol packages (`irc`,
  `conn`, `cap`, `sasl`, `isupport`, `client`) stay **standard-library only**.

## Commands

```sh
go build ./...
go vet ./...
gofmt -l irc conn cap sasl isupport client tui cmd   # must print nothing
go test -race ./...                                   # must be all green
```

Definition of done for any change: build + vet clean, `gofmt` clean, and
`go test -race ./...` green. Never commit with failing or skipped-because-broken
tests.

## Architecture (where things live)

```
irc/       wire protocol: Message, parse/serialize, tags, numerics, commands  (stdlib)
conn/      TCP/TLS transport, framed read/write                               (stdlib)
cap/       capability negotiation state machine                               (stdlib)
sasl/      SASL mechanisms                                                     (stdlib)
isupport/  RPL_ISUPPORT parsing + casemapping                                 (stdlib)
client/    high-level client: registration, Events() stream, state tracking   (stdlib)
tui/       Bubble Tea v2 terminal UI (the only charm-importing package)
cmd/lurk/  binary: TUI by default, -plain for the line client
```

Key contracts:
- `irc.Message{Tags, Source, Command, Params}` — flows everywhere; `Tags` already
  preserves IRCv3 message tags (so `@time`, `@batch`, `@+typing` are parsed).
- `client.Events() <-chan Event` — the buffered event stream the TUI consumes.
  `Event.Time()`, `Event.Nick()/User()/Host()`, `Event.Param(i)`, `Event.Text()`,
  `Event.Command()`, `Event.Tags` (via Message).
- The TUI is a single Bubble Tea `model` (`tui/model.go`); `Update` is the only
  place state mutates. Components are value types — reassign and thread the
  returned `cmd`.

## Conventions

- Match the surrounding style: thorough godoc on exported identifiers,
  table-driven tests, errors wrapped with `%w`, no panics in library paths.
- v2 gotchas: `Update` returns `(tea.Model, tea.Cmd)`; `View()` returns
  `tea.View` (wrap with `tea.NewView`, set `.Cursor`); keys are
  `tea.KeyPressMsg` matched via `msg.String()` ("enter", "esc", "ctrl+u", "up").
- Don't change Cycle-1/2 public contracts without need; prefer additive changes.
- Credit non-obvious borrowed design in a comment (see `CREDITS.md`).

## Docs map

- `docs/PHASE2-3-IMPLEMENTATION.md` — **your task list** (self-contained).
- `docs/PLAN-CYCLE3.md` — the higher-level cycle plan.
- `docs/ARCHITECTURE.md`, `docs/ARCHITECTURE-TUI.md` — package contracts.
- `docs/IRCV3-RESEARCH.md`, `docs/TUI-RESEARCH.md` — protocol/TUI references
  (note: their `reference/...` path citations are not present in this checkout).
