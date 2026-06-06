# lurkd — Design & Implementation Plan (FINAL)

**Status:** adopted by the council. This is the authoritative v1 plan for **lurkd**,
the single-user IRC bouncer (BNC) that the lurk client connects into. Every
adopted ballot item (#1–#14, #17, plus the #15 model and the #16 forward-compat
amendment) is folded in below. Open questions from the draft are now resolved
decisions; where a ballot settled one, it is cited inline as **[B#n]**.

lurkd holds persistent IRCv3 connections to upstream networks and presents a
plain IRCv3 *server* face to lurk (and to any third-party IRC client). lurk
attaches over one TLS connection, authenticates to the **bouncer**, selects an
upstream network, and gets a live + backlogged view.

---

## 1. Overview & the hook decision

### 1.1 lurkd is two halves glued by a message store

- **Upstream-client side** (lurkd → networks): persistent IRCv3 client
  connections. lurk's existing `client` / `conn` / `cap` / `sasl` / `isupport`
  stack already does this almost verbatim (one `client.Client` per network, with
  auto-reconnect and state tracking).
- **Client-facing listener side** (lurk → lurkd): lurkd speaks IRC *back* to lurk
  as if it were a server. lurk does **not** have this today: `cap.Negotiator` and
  `sasl` are written client-side only and emit *client* lines, so lurkd needs a
  new **server-side** CAP/SASL responder. This is the single biggest build item.

### 1.2 The hook: a plain IRCv3 connection with `soju.im/bouncer-networks`

All three reference bouncers converge on a **plain IRCv3 connection** as the
client hook — no custom binary protocol — because it maximizes client
compatibility and lets the bouncer reuse one IRC parser for both directions:

- **soju** (the modern Go reference): network selected either by the
  `soju.im/bouncer-networks` capability (`BOUNCER BIND <netid>` at registration +
  `LISTNETWORKS/ADDNETWORK/CHANGENETWORK/DELNETWORK` for runtime management) or by
  the universal `user/network@client` SASL/PASS username convention.
- **ZNC** (the incumbent): network selected entirely via the auth username
  (`username@clientid/network`, optionally `…:password` stuffed in `PASS`).
- **pounce** (deliberately minimal): one process per network, selected by TLS
  SNI; a single-producer/multi-consumer ring buffer with a per-client cursor.

**Decision.** lurkd implements **`soju.im/bouncer-networks`** as the primary hook
(the only mechanism with both a published spec *and* real client implementations
— gamja, goguma — and the only one giving *runtime* network management over one
connection, which directly serves lurk's multi-network TUI), and **also** accepts
the universal **`user/network@client`** username fallback so any third-party IRC
client still works. We **reject** pounce's SNI-per-instance model: it pushes
selection into deployment, gives no runtime management, and forces
one-process-per-network — contradicting lurk's multi-network-in-one-process
design.

Backlog is delivered via **CHATHISTORY**, not ZNC-style raw replay: lurk is
already a CHATHISTORY *consumer* (`client.ChatHistoryLatest`,
`client/actions.go:264`), so lurkd just becomes the *producer*. Bound clients
**pull** history themselves; lurkd does **not** push an unsolicited replay
**[B#6]**.

**Authentication separation (security-critical).** lurk authenticates to the
*bouncer* with SASL PLAIN over TLS; this is **never** round-tripped to the
upstream. Upstream network credentials live server-side in lurkd's config and are
set once. Conflating the two is a known bouncer security bug; we follow soju's
separation.

---

## 2. Package layout & reuse map

All new lurkd packages are **standard-library only** — lurkd is a server and
**must not** import the charm libraries (`charm.land/{bubbletea,bubbles,lipgloss}/v2`).
Per CLAUDE.md's dependency rule, only `tui/` and `cmd/lurk` may import charm; the
sole client-side change here (§7) lands in `client`/`config`/`tui`, keeping
`client` stdlib-only.

### 2.1 New stdlib-only packages (peers of `client/`)

1. **`server/`** — client-facing IRC server core. `net.Listener` + TLS accept
   loop, each conn wrapped via `conn.NewConn`. Contains the **server-side
   registration/CAP responder** (answers `CAP LS`/`REQ` with the caps lurkd
   offers; emits `ACK`/`NAK`; handles `CAP END`), the **server-side SASL
   responder** (consumes `AUTHENTICATE`, decodes PLAIN, authenticates the bouncer
   user), and the registration handshake (`NICK`/`USER`, welcome `001`–`005`,
   `RPL_ISUPPORT` with `BOUNCER_NETID`). The session/multiplexer glue (§4) lives
   here too (no separate `session` package; fewer packages, no charm risk).
2. **`bouncer/`** — `soju.im/bouncer-networks` server side: the `BOUNCER` verb
   dispatcher (`BIND`/`LISTNETWORKS`/`ADDNETWORK`/`CHANGENETWORK`/`DELNETWORK`),
   **integer** netid allocation/lifetime **[B#7]**, the `soju.im/bouncer-networks`
   BATCH responder, `soju.im/bouncer-networks-notify` broadcast, `BOUNCER_NETID`
   emission. Pure `irc.Message` construction.
3. **`backlog/`** — per-network, per-target store fed by each upstream
   `client.Client` via **`OnAny`** (not `Events()`) **[B#4]**; per-client cursors
   keyed by `@client`; a CHATHISTORY server emitting `draft/chathistory` BATCHes
   with `server-time` + `msgid`. The authoritative store is **structured JSONL**
   **[B#9]**, separate from the human `chatlog`.

### 2.2 Reuse map — upstream-client side (reused wholesale)

| Package | Seam | Use |
|---|---|---|
| `irc` | `irc.Parse`, `Message.Serialize`, `Message{Tags,Source,Command,Params}` | Direction-agnostic; no changes. |
| `conn` | `conn.Dial(ctx, "tcp", addr, Options)` (`client/client.go:325`) | Upstream TLS transport unchanged, incl. the 512/8191 length budget. |
| `cap` | `cap.NewNegotiator(wanted)` (`client/client.go:378`) | Client-side CAP negotiation to upstream, unchanged. |
| `sasl` | `sasl.Plain` / `sasl.External` | lurkd authenticates *itself* to each upstream, unchanged. |
| `isupport` | token parser + CASEMAPPING fold | Track per-network `CASEMAPPING`/`PREFIX`/`CHANTYPES` to key buffers. |
| `client` | `client.New` → `Connect`; `client.OnAny` (`client/event.go:230`); read-only accessors (`Channels`/`Members`/`Topic`/…, §2.4); `HandleReconnecting`/`HandleReconnected` (`client/event.go:214,218`) | **One `client.Client` per configured network**, exactly as the TUI runs today: auto-reconnect, state tracking, CTCP replies. |
| `config` | `config.Network/Identity/SASL` JSON-at-0600-XDG | Template for lurkd's per-network persisted config (§3). |

### 2.3 Reuse map — client-facing listener side (partial reuse + new code)

| Package | Reuse on listener side |
|---|---|
| `irc` | Verbatim — serialize what lurkd emits to lurk; parse what lurk sends. |
| `conn` | `conn.NewConn(raw, Options)` wraps a `net.Listener.Accept()`ed conn. The transport is **symmetric** — same framed read/write, send queue, length budget. lurkd's listener transport is *already written*. |
| `isupport` | lurkd **generates** its own `005` to lurk, incl. the `BOUNCER_NETID` token. |
| `sasl` | The PLAIN authcid/authzid/passwd **layout** is the reference for *decoding* what lurk sends — but lurkd must *respond* to `AUTHENTICATE` as the server (new code in `server/`). |
| `cap` | **Blueprint only.** `cap.Negotiator` emits *client* lines; lurkd needs a new server-side responder. The `State` machine + `irc.CAP_*` constants are the model. |
| `client` | `client.SanitizeTerminal` is reused on the **ingestion** path (§6). |
| `chatlog` | `chatlog.Log` keeps writing the human log; gains `chatlog.Tail` (§3.3, **[B#5]**) for lossy tier-2 rehydration only. |

### 2.4 Pre-attach accessor audit **[B#14]**

Before the attach phase (Phase 7), a small **additive** step audits
`client.Client` for the read-only accessors needed to synthesize the attach
JOIN/topic/names/away burst from current upstream state. Present today:
`Channels()` (`client/client.go:155`), `Members(channel)` (`:164`, returns
`Member` with prefixes), `Topic(channel)` (`:199`), `PrefixModes`/`PrefixSymbols`
(`:229`,`:237`). **To audit/add:** an away-state accessor (per-member or
self-away) if not already exposed, and a channel-modes snapshot if the burst
needs it. Any addition is additive client API and is enumerated in §7's
additive-changes accounting — so Phase 7 cannot silently expand the public
contract mid-build.

---

## 3. `cmd/lurkd` + config + on-disk format

### 3.1 The binary

`cmd/lurkd` is a **separate binary** from `cmd/lurk`, stdlib + the new server
packages only; it **must not** import charm.

| | `cmd/lurk` | `cmd/lurkd` |
|---|---|---|
| Role | interactive client (TUI / `-plain`) | headless daemon |
| Charm deps | yes (`tui`) | **no** |
| Lifecycle | foreground session | long-running; `SIGINT`/`SIGTERM` → flush store + cursors, close upstreams |
| Listener | none | TLS `net.Listener` on a configured bind addr |
| Flags | `-server`, `-plain`, `-version` | `-config <path>`, `-listen <addr>`, `-version`; no TTY |

### 3.2 Config on disk

Reuse `config`'s pattern exactly: JSON at `0600` under XDG
(`$XDG_CONFIG_HOME/lurkd/config.json`). Shape (additive over `config.Network`):

```jsonc
{
  "listen": { "addr": ":6697", "tls_cert": "...", "tls_key": "..." },
  "bouncer_auth": {
    "user": "dylan",
    // PBKDF2-HMAC-SHA256, self-describing: pbkdf2-sha256:$iter:$saltB64:$dkB64
    "password_hash": "pbkdf2-sha256:600000:<b64 salt>:<b64 dk>"
  },
  "networks": [
    {
      "netid": 1,                       // lurkd-allocated INTEGER [B#7]
      "name": "Libera.Chat",
      "addr": "irc.libera.chat:6697", "tls": true,
      "identity": { "nick": "...", "user": "...", "realname": "..." },
      "sasl": { "mechanism": "PLAIN", "authcid": "...", "password": "..." }, // upstream creds, server-side only
      "channels": ["#lurk"]
    }
  ]
}
```

- **netid is a lurkd-allocated integer [B#7]**, matching the
  `soju.im/bouncer-networks` spec (gamja/goguma assume server allocation).
  Config-defined networks are assigned ids at config load; `BOUNCER ADDNETWORK`
  allocates the next integer and returns it. Single id namespace.
- **Password hash is PBKDF2-HMAC-SHA256 [B#1]** (see §6.1).
- Upstream SASL passwords stored at `0600`, server-side only, never sent to lurk.
- Listener is **TLS-only** (no plaintext bind), enforced as a code invariant
  (§6.2). TLS material is a path pair (self-signed first-run generation is a
  Phase 9 nicety).

### 3.3 `chatlog.Tail` read seam **[B#5]**

Add an additive exported read method to `chatlog`:

```go
func (l *Logger) Tail(scope, target string, n int) ([]string, error)
```

returning the last `n` non-empty lines of the target's log via a reverse/tail
scan. This keeps `safeName` file-naming and path conventions in one place. **Note
its scope is narrow:** because the authoritative CHATHISTORY store is structured
JSONL **[B#9]**, `Tail` on the plain-text human log serves **only** the lossy
tier-2 rehydration *fallback*, not the authoritative cursor store.

---

## 4. Session / multiplexing model

- **Persistent upstream sessions.** lurkd starts one `client.Client` per
  configured network at boot (or lazily on first `ADDNETWORK`), auto-reconnect
  on. These live independently of any attached client — this is what makes it a
  bouncer.
- **Ingestion via `OnAny`, not `Events()` [B#4].** Each upstream session
  subscribes to events through `client.OnAny(handler)` (`client/event.go:230`),
  **not** by reading `Events()`. `Events()` is documented as a single shared
  channel intended for one consumer (`client/events.go`); the TUI process does
  not run here, but the rule is load-bearing in lurkd too — a second consumer
  would race or silently drop. `OnAny` runs synchronously from the client's
  single run goroutine, in order, after state tracking.
- **Multiple attached clients per network.** Each network session has a fan-out
  set of attached listener connections. An upstream line is: (a)
  `SanitizeTerminal`'d at ingestion (§6.3), (b) tagged with `server-time` +
  `msgid`, (c) appended to the JSONL store once, (d) relayed live to every
  attached client bound to that network.
- **Attach = state-only burst, no pushed replay [B#6].** On `BOUNCER BIND <netid>`
  (or username fallback), lurkd replays only the channel **state** burst — synthetic
  `JOIN` + names (`multi-prefix`) + topic + away — synthesized from the upstream
  `client.Client`'s read-only accessors (§2.4). lurkd does **not** push missed
  messages. The bound lurk client pulls history itself via CHATHISTORY, reusing
  its existing self-JOIN path (`client/run.go:121`, `afterTrack` → `ChatHistoryLatest`).
  This eliminates the double-delivery collision (burst + pull) and needs zero new
  client code. A missed-while-detached *replay* is offered **only** for non-lurk
  clients that never send CHATHISTORY, gated strictly on the client having issued
  no CHATHISTORY; the default lurk path is pull-only.
- **Detach.** A listener disconnect (or unbind) marks the client detached and
  **persists its cursor** (§5.3). The upstream session keeps running; reconnect
  resumes from the cursor.
- **Self-send fan-out rule [B#11].** For a `PRIVMSG` originating from attached
  client *A*: (1) send it upstream; (2) on upstream echo (or local echo), deliver
  it back to *A* with *A*'s `@label` echoed **iff** *A* used `labeled-response`;
  (3) relay to all **other** attached clients of that network **without** *A*'s
  label; (4) store **exactly once**. Stating this prevents the fan-out from either
  dropping the self-echo or double-showing it.
- **Reconnect-while-attached [B#12].** The session glue hooks
  `HandleReconnecting`/`HandleReconnected` (`client/event.go:214,218`), not just
  `OnAny`. When the upstream drops while a client is bound, lurkd stops relaying
  (or buffers into the store) during the gap; on `HandleReconnected` (after the
  client re-runs its JOIN burst), lurkd re-injects the synthetic JOIN/names/topic
  burst to attached clients so their channel-member state is not corrupted.
- **Away handling.** lurkd sets itself `AWAY` upstream when no client is attached
  to a network and clears it on attach (standard bouncer courtesy); upstream
  `away-notify` state is tracked by `client` and relayed.

---

## 5. Persistence & backlog

### 5.1 Structured JSONL CHATHISTORY store **[B#9]**

The authoritative backlog is a **structured per-entry store, separate from
`chatlog`**: one JSON object per line — `{time, msgid, raw irc.Message}` — under
lurkd's data dir, per-network/per-target. Rationale: `chatlog`'s date-prefixed
plain text **loses `server-time` and `msgid` on disk**, making it unusable for
restart-safe CHATHISTORY cursor queries and Phase 5 golden transcripts. `chatlog`
continues unchanged as the human-readable log; the backlog store is a distinct
file tree. Stdlib-only (`encoding/json`). An in-memory per-target ring (pounce's
single-producer/multi-consumer model, drop-oldest at a fixed bound) fronts the
JSONL for live fan-out and is rehydrated from the JSONL tail on boot (with
`chatlog.Tail` as a lossy fallback only).

### 5.2 Server-assigned opaque msgid **[B#8]**

Each stored line persists a **server-assigned opaque `msgid`** (the soju model),
written to disk alongside the line. Rejected: a monotonic counter (not
restart-stable unless persisted) and a content hash (collides on duplicate
identical lines). msgid stability across restart is **required** for CHATHISTORY
`BEFORE`/`AFTER` selectors and cursors, so it is decided now (not Phase 5):
the Phase 4 store schema and the rehydration format both depend on it.

### 5.3 Per-client cursors + write-back policy **[B#10]**

A read position per `(client, network, target)`, keyed by the `@client` name
(unbound clients with no `@client` share a single default cursor). Persisted as
small JSON. **Write-back policy:** flush on **clean detach** AND on a
**configurable periodic interval (default 30s)** — avoiding per-message `fsync`
overhead while bounding loss. The Phase 7 crash-recovery test (§8) asserts replay
resumes from within the last flush window, not from the log start.

### 5.4 CHATHISTORY production

`LATEST/BEFORE/AFTER/AROUND/BETWEEN/TARGETS` served from the JSONL store with
casemap-folded target keys (via `isupport` casemapping). Every replayed line
carries its original `server-time` + `msgid` in a `draft/chathistory` BATCH.
Without `draft/event-playback` negotiated, the batch contains **only**
`PRIVMSG`/`NOTICE` (spec requirement). A `limit` ceiling bounds response size.

### 5.5 Resource bounds

Fixed per-target ring entry cap (drop-oldest); caps on attached clients per
network, total networks, line length (already enforced by the `conn` budget), and
the CHATHISTORY `limit` ceiling. JSONL files are size/rotation-bounded.

---

## 6. Security

### 6.1 PBKDF2 bouncer-password hashing **[B#1]**

Store the lurkd bouncer password with **PBKDF2-HMAC-SHA256** via stdlib
`crypto/pbkdf2.Key` (in the Go stdlib since 1.24; this module targets Go 1.26)
over a per-credential salt from `crypto/rand`. Parameters are serialized
self-describingly into the `0600` JSON:
`pbkdf2-sha256:$iter:$saltB64:$dkB64`. **Concrete parameters:** 16-byte salt,
32-byte derived key, **600000 iterations** (OWASP guidance for PBKDF2-HMAC-SHA256).
Explicitly **ruled out:** plain salted SHA-256 (not a password KDF) and
`golang.org/x/crypto/{bcrypt,scrypt}` (would be the first non-stdlib dependency in
the server packages, violating the stdlib-only constraint). A `server/` unit test
round-trips a password through hash/verify using only stdlib imports.

### 6.2 Server-side TLS gate on AUTHENTICATE PLAIN **[B#2]**

TLS is a **code invariant**, not just config intent. If an accepted connection is
not a `*tls.Conn` (the accept loop did not wrap it via `tls.Server`), the
`server/` registration handler rejects any `AUTHENTICATE PLAIN` with
`ERR_SASLFAIL` (**904**) and a message like `SASL PLAIN requires TLS` **before
decoding the base64 payload**, so credential bytes are never parsed over
plaintext. This mirrors the client-side guard at `client/client.go:301-307`. A
hermetic test connects without TLS, attempts `AUTHENTICATE PLAIN`, and asserts
904 with no credential bytes decoded.

### 6.3 Hardened authcid fallback parser **[B#3]**

The `user/network@client` fallback parser (splitting the SASL authcid on `/` and
`@`) gets **table-driven** `server/` unit tests covering attacker-controlled
boundaries, modeled on `chatlog`'s `safeName` defense
(`chatlog/chatlog.go:86-110`): (a) a NUL byte anywhere → auth failure, not a
parse error leaking internal state; (b) no `/` → no-network (control context),
not a panic; (c) more than one `/` or `@` → error or documented last-wins
determinism, tested; (d) network name matching no configured netid → 904, not
phantom network creation; (e) network name exceeding a sane length bound (e.g.
128 bytes) → 904, not unbounded allocation. These exist **before** Phase 6, since
the fallback parser is exercised even when bouncer-networks is negotiated.

### 6.4 SanitizeTerminal touchpoints

`client.SanitizeTerminal` is applied **at ingestion into the backlog store**,
before any line is persisted or relayed — not only on the live path. Hostile
upstream escapes must never be persisted and replayed later. Concretely: the
upstream `OnAny` → store hop sanitizes; CHATHISTORY replay reads
already-sanitized text; bouncer-generated control text is lurkd's own and safe by
construction.

### 6.5 Auth separation

Bouncer-level auth (lurk → lurkd) is distinct from network-level auth (lurkd →
upstream). Upstream creds are stored server-side and **never** forwarded to lurk.

---

## 7. Client-facing capability set & lurk/TUI integration

### 7.1 Capabilities lurkd advertises (listener `CAP LS`)

- `sasl` (PLAIN; EXTERNAL via client cert is a later add)
- `soju.im/bouncer-networks` + `soju.im/bouncer-networks-notify`
- `draft/chathistory` + `draft/event-playback`
- `server-time`, `batch`, `message-tags`, `labeled-response`
- `cap-notify`, `echo-message`
- pass-through state caps lurkd already tracks via `client`: `away-notify`,
  `account-notify`, `extended-join`, `multi-prefix`, `chghost`, `setname`

Draft/vendor specs (`soju.im/bouncer-networks*`, `draft/*`) are pinned to the soju
doc as the de-facto contract (gamja/goguma interop is the real test); each verb
family is confined to its own package so a spec bump is localized; the username
fallback keeps lurkd usable even if a client ignores `bouncer-networks`.

### 7.2 The v1 lurk-side model [B#15] — one `client.Client` per bound netid

v1 lurk opens **one `client.Client` per bound netid** to lurkd. Selection uses a
single **additive** registration hook: a pre-CAP-END callback/Config field that
emits `BOUNCER BIND <netid>` **after SASL success and before CAP END**.

**Exact seam in the existing flow.** The cap-driven registration sequence is
`CAP LS 302` → (optional SASL) → `CAP END` → `001` → `005`:

- CAP negotiation is driven by `handleCAP` → `advanceCAP`
  (`client/run.go:162,176`).
- When SASL is required, `advanceCAP` calls `startSASL` (`:178`); on SASL
  success the run loop calls `finishSASL` (`client/run.go:539`), which emits the
  `CAP END` line(s) via `c.neg.SASLComplete()`.
- The BIND hook fires **inside this window**: after the SASL success numeric
  (`RPL_SASLSUCCESS`/903) is processed and **before** `finishSASL` sends
  `CAP END`. Concretely, an additive `Config.BeforeCapEnd func(*Client) error`
  (or an explicit `Config.BouncerBindNetID int`) is invoked at the top of
  `finishSASL` — and, for the no-SASL path, at the equivalent pre-`CAP END` point
  reached from `advanceCAP` — so the `BOUNCER BIND <netid>` line is written before
  CAP END. This converts the vague "BIND is additive" claim into a buildable,
  contract-preserving change: BIND-before-CAP-END would otherwise mutate the
  cap-driven sequence, so it is wired at exactly the one place CAP END is emitted.

The lurk-side `BOUNCER` consumer is a small additive method set on `client.Client`
(`BouncerBind(netid)`, `BouncerListNetworks()`, `BouncerAddNetwork(...)`, etc.)
that emits the `BOUNCER` verb and parses the `soju.im/bouncer-networks` BATCH,
mirroring the existing `ChatHistoryLatest` consumer pattern
(`client/actions.go:264`). Stdlib, additive.

### 7.3 `config.BounceConfig` [B#17]

Before Phase 8, add a typed `config.BounceConfig` struct as a field on
`config.Network` (which today has no extension point for bouncer metadata).
Required fields: the bouncer's `Addr` (may differ from the network display
address), a stable integer `NetID` (the `BOUNCER BIND` target), and a `ClientID`
(the `@client` suffix for per-client cursor keying). A zero `BounceConfig` means
direct connect; existing configs round-trip unchanged (additive). Deciding it now
lets the launcher form reference it without a later reshuffle.

### 7.4 Additive client-side changes (full accounting)

- `config.BounceConfig` field on `config.Network` (§7.3) **[B#17]**.
- `Config.BeforeCapEnd`/`Config.BouncerBindNetID` registration hook (§7.2)
  **[B#15]**.
- `client.Bouncer*` consumer methods (§7.2).
- Any read-only accessors added by the §2.4 audit **[B#14]**.
- `chatlog.Tail` (§3.3) **[B#5]** — used by lurkd, additive to `chatlog`.
- TUI: when attached to a bouncer, the network list is populated from
  `BOUNCER LISTNETWORKS` and `ADDNETWORK`/`DELNETWORK` map to TUI actions;
  `-plain` works unchanged via the username fallback.

### 7.5 Forward-compat amendment to the N-connection model **[B#16 deferred]**

Per the orchestrator's binding ruling, the v1 N-connection model **must** be
designed so the later collapse to a single demuxed connection (#16) is an
**additive, `tui/`-only** change — no rip-and-replace of sidebar/buffer routing:

- **(a)** Keep the **netid ↔ network mapping explicit in `tui/`** so the
  evolution is mechanical. Today the TUI holds one `client.Client` per network;
  v1 over a bouncer holds one `client.Client` per *bound netid*. Buffer/sidebar
  routing keys on the (netid, network) pair, not on the connection object, so
  swapping N connections for one demuxed connection is a routing-layer change
  confined to `tui/`.
- **(b)** lurkd **must** expose a **control/master connection** — a session that
  finishes registration **without** a `BOUNCER BIND` — and keep
  `BOUNCER LISTNETWORKS` reachable from the lurk TUI in v1, so network management
  does **not** depend on the future demux work.
- **(c)** #16 is recorded as the intended end-state in §10.

---

## 8. Phased build order

Each phase is independently **buildable + vettable + gofmt-clean +
race-test-green**, and hermetically testable with `net.Pipe`. The template is
`client/integration_test.go` (scripted `mockServer` + `ConnectConn`), used two
ways: a synthetic *client* drives lurkd's listener (lurkd on the accepting end),
and the existing `mockServer` stands in for an upstream network. The full path is
a **two-mock sandwich**: mock client ↔ lurkd ↔ mock upstream.

**Phase 0 — Harness + skeleton.** `net.Pipe` harness; `cmd/lurkd` skeleton (flag
parse, config load, no listener). *Test:* config round-trips at `0600`; binary
builds; **import-guard test** asserts the server packages + `cmd/lurkd` import no
charm path.

**Phase 1 — Listener transport + registration + server-side CAP responder.**
`server/` accept loop over `conn.NewConn`; `CAP LS/REQ/ACK/NAK/END`; `NICK`/`USER`;
welcome `001`–`005` with a stub `BOUNCER_NETID`. *Test (golden transcripts):*
`CAP LS 302` → offered caps; `CAP REQ` of supported/unsupported → `ACK`/`NAK`;
full registration reaches `001`+`005`; assert exact serialized lines.

**Phase 2 — Server-side SASL PLAIN + bouncer auth + PBKDF2 + TLS gate.** Consume
`AUTHENTICATE`, decode PLAIN, verify against the PBKDF2 hash **[B#1]**. *Tests:*
good PLAIN → `900/903` then registration completes; wrong password → `904`;
**non-TLS conn → `AUTHENTICATE PLAIN` rejected with 904 before any base64 decode
[B#2]**; PBKDF2 hash/verify round-trip (stdlib-only) **[B#1]**;
**table-driven authcid fallback parser** covering NUL/no-slash/multi-token/unknown-
netid/over-length **[B#3]**.

**Phase 3a — Upstream session manager (feed-only).** Start one `client.Client`
per network (reusing the whole upstream stack), subscribe via `OnAny` **[B#4]**,
feed events into an **in-memory sink — no listener involved [B#13]**. *Tests:*
with a scripted upstream `mockServer`, lurkd registers upstream, joins autojoin
channels, and an upstream `PRIVMSG` reaches the sink. **Reconnect-while-attached:
[B#12]** the upstream drops + reconnects; assert the session hooks
`HandleReconnecting`/`HandleReconnected`, pauses/buffers during the gap, and
re-injects the JOIN/names/topic burst on reconnection (uses existing
`dialPair`+`mockServer` infra).

> **Phase ordering fix [B#13]:** the bind+listener end-to-end delivery test
> (upstream `PRIVMSG` reaching an *attached listener* over the pipe) needs
> `BOUNCER BIND`, which does not exist until Phase 6 — so it moves there. Phase 3a
> proves the feed with no listener; each phase's stated test compiles and passes
> at that phase.

**Phase 4 — Backlog store + SanitizeTerminal at ingestion.** Structured **JSONL**
store **[B#9]** with per-line **server-assigned msgid [B#8]** + `server-time`;
in-memory ring front; rehydrate-on-boot from JSONL (with `chatlog.Tail` **[B#5]**
as lossy fallback); sanitize at ingestion. *Tests:* an upstream line with an
embedded escape (`\x1b[…`) is stored sanitized (stored bytes contain no raw
escape); ring drop-oldest at the bound; restart rehydrates from JSONL with msgids
intact.

**Phase 5 — CHATHISTORY server.** `LATEST/BEFORE/AFTER/AROUND/BETWEEN/TARGETS` in
`draft/chathistory` BATCHes; the `draft/event-playback` gate. *Tests (golden
transcripts per subcommand):* correct BATCH open/close, `server-time` + `msgid`
on every line, casemapped target match; **without** `event-playback`, only
`PRIVMSG`/`NOTICE` appear; `limit` ceiling enforced.

**Phase 6 — bouncer-networks + bind/listener delivery.** `bouncer/`:
`BOUNCER BIND` attaches a registered connection to an **integer** netid **[B#7]**;
`LISTNETWORKS/ADDNETWORK/CHANGENETWORK/DELNETWORK`; `bouncer-networks-notify`;
real `BOUNCER_NETID` in `005`. *Tests:* `LISTNETWORKS` returns the configured
networks in a correct BATCH; **`BIND <netid>` then a live upstream `PRIVMSG` is
delivered to the bound listener over the pipe (the e2e moved from Phase 3
[B#13]);** `ADDNETWORK` allocates the next integer id, persists config, and
broadcasts `-notify` to other control clients; an **unbound client stays on the
control/master context** (amendment (b)); the **self-send fan-out rule [B#11]** is
asserted (echo back to A with its label, relay to others without it, stored once).

**Phase 7 — Attach state burst + cursors + away.** On bind, the synthetic
JOIN/topic/names/away **state burst only — no pushed replay [B#6]**, synthesized
from the audited read-only accessors **[B#14]**; the bound lurk pulls history via
its existing self-JOIN CHATHISTORY path; cursor write-back on detach + 30s
periodic **[B#10]**; lurkd AWAY when no client attached. *Tests:* attach → exactly
the state burst (no message replay); two clients with distinct `@client` ids get
independent cursors; **crash-recovery [B#10]:** attach, receive N messages,
abrupt close (not clean detach), restart, reattach → replay resumes from within
the last flush window, not the log start; no-attach sets upstream AWAY (observed
on the upstream mock).

**Phase 8 — lurk client integration (additive).** `config.BounceConfig`
**[B#17]**; the pre-CAP-END `BOUNCER BIND` hook at the cited seam **[B#15]**;
`client.Bouncer*` consumer methods; minimal TUI "connect via bouncer" path with
the explicit netid↔network mapping (amendment (a)). *Tests:* the new consumer
methods serialize the right `BOUNCER` lines (unit, mirroring the existing
CHATHISTORY serialize test); a full `net.Pipe` round-trip of lurk ↔ lurkd binding
one network, incl. BIND firing after SASL, before CAP END.

**Phase 9 — `cmd/lurkd` polish.** Graceful shutdown (flush store + cursors, close
upstreams), signal handling, optional self-signed cert generation, `-version`,
docs/packaging hooks. *Tests:* shutdown flushes cleanly (race-tested);
import-guard test still green.

### Test strategy summary

- **Everything hermetic** via `net.Pipe`; the live test stays gated on
  `LURK_TEST_SERVER` and skips by default.
- **Golden-transcript tests** for every protocol surface (CAP, SASL,
  CHATHISTORY, bouncer-networks) — assert exact serialized `irc.Message` lines,
  because batch-boundary / `server-time` / msgid errors corrupt timelines
  silently.
- **Import-guard test** enforces the stdlib-only / no-charm rule for the server
  packages + `cmd/lurkd` in CI.
- **Definition of done** (per phase and overall): `go build ./...`,
  `go vet ./...`, `gofmt` clean, `go test -race ./...` green.

---

## 9. Non-goals (v1)

- **Multi-user.** Single-user only; soju's HTTP/PAM/OAuth/multi-tenant surface is
  out of scope.
- **SQL/indexed message store + FTS.** v1 is the stdlib JSONL store (no sql
  driver). Additive later.
- **Web UI / admin HTTP.** None.
- **ZNC module compatibility** beyond the username convention.
- **Non-PLAIN bouncer auth** beyond a later optional SASL EXTERNAL.
- **Federation / server-to-server.** lurkd is not an ircd.
- **The single-connection demux** (deferred — §10).

---

## 10. Deferred / post-v1 evolution

**#16 — single shared connection demultiplexed into per-netid synthetic
networks.** This is the intended end-state. The Researcher confirms it is exactly
what **gamja/goguma do over soju**: one connection to the bouncer, with the
client demultiplexing per-netid traffic into synthetic networks via the
`soju.im/bouncer-networks` metadata and `BOUNCER_NETID`. It was **rejected for
v1** by orchestrator veto (it failed the council 2–4 and conflicts with the
adopted #15 N-connection model).

*Migration sketch.* Because of the forward-compat amendment (§7.5), the collapse
is a `tui/`-only change: v1 already keys all sidebar/buffer routing on the
explicit (netid, network) pair rather than on the connection object, and lurkd
already exposes the control/master connection plus `BOUNCER LISTNETWORKS` in v1.
The evolution replaces the N `client.Client` connections with a single
`client.Client` to lurkd that stays on the control context, learns the network
set from `LISTNETWORKS`/`-notify`, and routes inbound lines to the right
synthetic network by `BOUNCER_NETID`/batch metadata — feeding the **same**
routing layer. No protocol or store changes in lurkd are required; network
management already works over the v1 control connection.

Other deferred items: SASL EXTERNAL (client-cert) bouncer auth; an indexed/SQL
store with FTS; first-run self-signed cert auto-generation refinements; ZNC fs
store compatibility.

---

## Council provenance

Produced by a 6-member design council: a **Writer** and a **Researcher** on Opus,
and **4 members on Sonnet**. All 6 members **approved with changes**. **16 ballot
items were adopted** and are folded into this plan (cited inline as **[B#n]**).
The v1 connection model is ballot **#15** (one `client.Client` per bound netid via
a pre-CAP-END `BOUNCER BIND` hook), adopted 4–2. Ballot **#16** (single demuxed
connection) was **resolved by orchestrator veto** — rejected for v1 — with the
binding forward-compatibility amendment recorded in §7.5 and §10.
