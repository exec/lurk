# Lurk — Architecture & Cycle 1 Contracts

Lurk is an IRCv3 client library (and demo CLI) written in Go. This document is
the **shared contract** for the team. Every agent owns one package and must
honor the interfaces below so the pieces compose without rework. When in doubt
about a boundary, **message the owning agent** rather than guessing.

Module path: `lurk` (Go 1.26).

## Package layout & ownership

```
lurk/
├── irc/        Wire protocol: Message, parsing, serialization, tags, numerics.   [owner: parser]
├── conn/       Transport: TCP/TLS dial, framed line read/write, send queue.       [owner: transport]
├── cap/        Capability negotiation logic (CAP LS/REQ/ACK/NAK/NEW/DEL).         [owner: capneg]
├── sasl/       SASL mechanisms (PLAIN, EXTERNAL) + AUTHENTICATE chunking.         [owner: sasl]
├── isupport/   RPL_ISUPPORT token parsing + CASEMAPPING folding.                  [owner: state]
├── client/     High-level Client: orchestration, registration, events, tracking. [owner: client]
└── cmd/lurk/   Demo CLI that drives the client library.                           [owner: client]
```

Dependency direction (no cycles): `irc` depends on nothing. `conn`, `cap`,
`sasl`, `isupport` depend only on `irc` (and stdlib). `client` depends on all of
them. Nobody imports `client`.

## The central contract: `irc.Message`

Defined in `irc/message.go` (already written, do not change its shape without
team agreement). It is the ONLY type that crosses every boundary:

- Transport reads bytes → hands a raw line (`[]byte`/`string`) to the parser.
- `irc.Parse(line) (*Message, error)` → `*Message`.
- `irc.Message.Serialize() ([]byte, error)` (or `String()`) → bytes for transport.
- `cap`, `sasl`, `isupport`, `client` all consume/produce `*irc.Message`.

## Per-package interface contracts (Cycle 1)

### `irc` (parser)
- `func Parse(line string) (*Message, error)` — tolerant of missing tags/source.
- `func (m *Message) Serialize() (string, error)` — round-trips Parse; chooses
  the trailing (`:`) parameter correctly (last param if it is empty, contains a
  space, or starts with `:`). Enforce no CR/LF/NUL in non-tag fields.
- Tag escaping per spec: `\:`→`;`, `\s`→space, `\\`→`\`, `\r`→CR, `\n`→LF;
  lone trailing `\` drops; unknown `\x`→`x`. Round-trip safe.
- `numerics.go`: named constants for numerics used in Cycle 1 (RPL_WELCOME=`"001"`
  … RPL_ISUPPORT=`"005"`, RPL_NAMREPLY=`"353"`, ERR_NICKNAMEINUSE=`"433"`,
  SASL 900–908, ERR_INPUTTOOLONG=`"417"`, etc.).
- `commands.go`: string constants for CAP, AUTHENTICATE, NICK, USER, PASS, PING,
  PONG, PRIVMSG, NOTICE, JOIN, PART, QUIT, MODE, NAMES, BATCH, TAGMSG.

### `conn` (transport)
- `type Conn` with a constructor like
  `func Dial(ctx, network, addr string, opts Options) (*Conn, error)` where
  `Options{ TLS bool; TLSConfig *tls.Config; ... }`.
- Read side: expose a channel or `ReadMessage() (*irc.Message, error)` that
  frames on `\r\n` (also tolerate bare `\n`), enforces the 8191-byte tag budget
  + 512-byte message budget, and parses via `irc.Parse`. Decide channel-vs-call
  with the client owner — **coordinate**, the client run-loop consumes this.
- Write side: `WriteMessage(*irc.Message) error` (serializes) and/or
  `Send(line string)`. Include a simple outbound send queue so the client can
  hand off without blocking. Flood/rate limiting can be a stub in Cycle 1 but
  leave the seam.
- TLS support via stdlib `crypto/tls`. Verify certs by default; allow opt-out.

### `cap` (capability negotiation)
- A pure-ish state machine that does NOT do I/O. It takes parsed CAP messages
  and the set of caps the client *wants*, and emits the lines to send.
- `type Negotiator` with methods to: record `CAP LS` (handle multiline `*`
  continuation and `key=value` values), compute the `CAP REQ` payload(s) from
  the intersection of available∩wanted, process `ACK`/`NAK`, and report when
  negotiation is done so the client can send `CAP END` (or proceed to SASL).
- Track enabled caps; handle `CAP NEW`/`CAP DEL` post-registration.
- Expose `Available()`, `Enabled()`, and the SASL mechanism list (from
  `sasl=...` value) so the `sasl` flow can consume it.

### `sasl` (authentication)
- `type Mechanism interface { Name() string; Start() ([]byte, bool); Next(challenge []byte) ([]byte, error) }`
  (final shape is the sasl owner's call — coordinate with capneg & client).
- `PLAIN` (authzid\0authcid\0passwd, base64) and `EXTERNAL` (cert-based, usually
  empty `+`).
- Helper to chunk a base64 payload into ≤400-byte `AUTHENTICATE` lines, emitting
  a trailing `AUTHENTICATE +` when the payload length is a multiple of 400 (and
  for empty payloads send `AUTHENTICATE +`).
- Consumes numerics 900/903 (success), 904/905/906/907/908 (fail/abort/mechs).

### `isupport` (server features + casemapping)
- `func Parse(tokens []string) ISupport` accumulating across multiple 005 lines;
  handle `KEY=value`, bare `KEY`, and `-KEY` (negation).
- Typed accessors: `PrefixModes()`/`PrefixSymbols()` (from `PREFIX=(ov)@+`),
  `ChanTypes()`, `ChanModes()` (4 categories A,B,C,D), `NickLen()`,
  `ChannelLen()`, `Network()`, `StatusMsg()`, `CaseMapping()`.
- `type CaseMapping` with `Fold(string) string` implementing `ascii` and
  `rfc1459` (`[]\^` ↔ `{}|~`). The client uses Fold for ALL nick/channel
  comparisons (map keys for channel/user tracking).

### `client` (orchestration + public API)
- `type Client` wiring conn + cap + sasl + isupport + state + event dispatch.
- `type Config{ Nick, User, Realname, Pass, Server string; TLS bool; SASL ...; Caps []string }`.
- Registration state machine: on connect send `CAP LS 302`, `NICK`, `USER`
  (+`PASS` if set); run cap negotiation; if `sasl` enabled+configured run SASL;
  send `CAP END`; wait for `001`; parse `005` into isupport; ready.
- Always auto-respond to `PING` with `PONG`.
- Event dispatch: `func (c *Client) On(command string, h Handler)` plus a small
  set of semantic events (Connected, Message, Join, Part, Quit, Nick). Handler
  signature TBD by client owner — keep it simple (`func(*Event)`).
- State tracking: channels the client is in, members per channel with prefixes,
  self nick (updated on NICK/433). Keyed via `isupport.CaseMapping.Fold`.
- `cmd/lurk/main.go`: connect to a server from flags/env, request common caps,
  optionally SASL, join a channel, print messages — a living smoke test.

## Conventions
- Standard library only for Cycle 1 (no external deps). `crypto/tls`,
  `encoding/base64`, `bufio`, `context`, `net`.
- Every package ships `_test.go` with table-driven tests. The parser and
  isupport packages especially need thorough spec-derived test vectors.
- `gofmt` clean; `go vet ./...` clean; `go test ./...` green before "done".
- Errors wrapped with `%w`; no panics in library code paths.
- Keep godoc on exported identifiers.

## How we coordinate
- Parser (`irc`) is the keystone — it should land first and be announced to all.
- If you need to change a cross-package contract, message the affected owners
  AND the lead before editing. Prefer additive changes.
- The client owner integrates last; transport/cap/sasl/isupport should message
  the client owner to confirm the exact call shapes (channel vs method) they
  expose, so integration is mechanical.
