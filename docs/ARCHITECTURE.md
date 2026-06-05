# Architecture

Lurk is an IRCv3 client written in Go (module `github.com/exec/lurk`, Go 1.26).
It is split into a standard-library-only protocol stack and a Bubble Tea terminal
UI layered on top. This document covers the packages, how they depend on each
other, and the few contracts that hold the whole thing together. For the UI
internals see [`ARCHITECTURE-TUI.md`](ARCHITECTURE-TUI.md).

## Packages

```
irc/        Wire protocol: Message, parse/serialize, message tags, numerics, command names.
conn/       Transport: TCP/TLS dial, framed line read/write, bounded read buffer, outbound send queue.
cap/        IRCv3 capability negotiation state machine (CAP LS/REQ/ACK/NAK/NEW/DEL, SASL gating).
sasl/       SASL mechanisms (PLAIN, EXTERNAL) and AUTHENTICATE base64 chunking.
isupport/   RPL_ISUPPORT (005) token parsing and CASEMAPPING folding.
client/     High-level client: registration, the Events() stream, channel/user state tracking,
            auto-reconnect, CTCP auto-replies, terminal-escape sanitization.
config/     On-disk network/identity configuration (JSON, 0600) with XDG path resolution.
chatlog/    Append-only per-target plain-text chat logs.
tui/        Bubble Tea v2 terminal UI.
cmd/lurk/   The binary: TUI by default, -plain for the line client, network launcher when no -server.
```

## Dependency direction

There are no import cycles; dependencies point "downward":

- `irc` depends on nothing but the standard library.
- `conn`, `cap`, `sasl`, `isupport` depend only on `irc` (plus stdlib).
- `client` depends on the protocol packages above.
- `config` and `chatlog` are standalone (stdlib only) and import neither `client`
  nor any UI library.
- `tui` depends on `client`, `config`, `chatlog`, and `irc`.
- `cmd/lurk` wires `client`, `config`, and `tui` together.

**Dependency rule (enforced by review):** only `tui/` and `cmd/lurk` may import the
charm libraries (`charm.land/{bubbletea,bubbles,lipgloss}/v2`). Every protocol
package stays standard-library only, so `client` is usable as a library without
pulling in a terminal UI.

## The central type: `irc.Message`

`irc.Message{Tags, Source, Command, Params}` is the one type that crosses every
boundary. Transport frames a line and `irc.Parse` produces a `*Message`;
`Message.Serialize` turns one back into a wire line — choosing the trailing (`:`)
parameter correctly and rejecting CR/LF/NUL in non-tag fields. Message tags are
parsed and unescaped per the IRCv3 spec, so `@time`, `@batch`, and `@+typing`
are available everywhere downstream. The 512-byte message budget and the
separate 8191-byte tag budget are enforced at the transport.

## The client and its event stream

`client.Client` wires conn + cap + sasl + isupport together. It drives
registration (CAP LS 302 → optional SASL → CAP END → 001 → 005), answers PING,
and tracks channels, members (with prefixes/metadata), and topics keyed through
the server's CASEMAPPING. Everything surfaces as events:

- `client.Events() <-chan Event` is the buffered stream the front-ends consume.
  It never blocks the read loop: a slow consumer gets drop-oldest with a
  synthetic overflow marker. `Event` exposes `Time()`, `Nick()/User()/Host()`,
  `Param(i)`, `Text()`, `Command()`, and the underlying `Tags`. Registered
  handlers (`On`/`Handle*`) see the same events.
- **Auto-reconnect** (`Config.AutoReconnect`): a supervisor goroutine re-dials
  with capped backoff after an unexpected drop, re-registers, and re-joins
  channels — all over the same `Events()` stream, so a UI survives the gap. A
  user Quit/Close stops the supervisor. See `client/reconnect.go`.
- **Terminal-escape safety:** all server-controlled text the front-ends display
  passes through `client.SanitizeTerminal`, which strips C0/C1/DEL/bidi control
  runes so a hostile server cannot inject terminal escape sequences.

## The front-ends

`cmd/lurk` is the binary. With no `-server` it runs the **network launcher**
(`tui/launcher*.go`) — a list of saved `config.Network`s — then hands the chosen
network to the chat UI. The full-screen TUI is the default; `-plain` runs a
line-mode client against the same `client` API. The TUI can hold **multiple
networks at once** (see [`ARCHITECTURE-TUI.md`](ARCHITECTURE-TUI.md)).

## Conventions

- Protocol packages are standard-library only; errors are wrapped with `%w`;
  no panics in library paths; exported identifiers carry godoc.
- Tests are hermetic — an in-process `net.Pipe` mock server plus unit tests.
  The one live test is environment-gated (`LURK_TEST_SERVER`) and skips by
  default. There are also Go fuzz targets for the hostile-input sinks.
- Definition of done: `go build ./...`, `go vet ./...`, `gofmt` clean, and
  `go test -race ./...` green.
