# Changelog

All notable changes to Lurk are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/); the project aims to follow
semantic versioning.

## [Unreleased]

## [1.2.4] - 2026-06-07

### Fixed
- **`/list` on a large network no longer drops channels.** A big `LIST` reply
  flooded the event stream faster than the TUI could drain it, overrunning the
  drop-oldest buffer — producing "lost N events (UI fell behind)" lines and an
  incomplete channel list. The TUI now batch-drains the event stream (one render
  per burst), so a large `/list` (or any flood) is applied completely.
- The **nicklist updates live again** on JOIN/PART/NICK in the channel you are
  viewing — a sort cache could otherwise serve a stale member list until you
  switched buffers.

### Changed
- **Large-server efficiency pass** — a sweep of hot-path optimizations, each
  benchmark-backed, so the client and daemon stay responsive on big servers:
  - **TUI:** refresh the active viewport once per event-batch instead of once per
    event (was O(batch × scrollback) per update); compute the nicklist sort once
    per update instead of 3–5× per keystroke in a large channel.
  - **Client:** in a large NAMES burst, fold each nick once (was 3×) and split the
    line without a per-line allocation; QUIT/NICK pass a pre-folded key through
    the channel update (was O(channels) redundant folds per message).
  - **conn:** coalesce a queued write burst into a single flush (one syscall per
    burst instead of per message).
  - **lurkd:** `CHATHISTORY TARGETS` reads each target's newest time from the
    in-memory ring instead of scanning every JSONL file (a reattach was a disk-I/O
    storm); the backlog ring is now circular — O(1) eviction instead of an
    O(ring-size) memmove per stored message.

## [1.2.3] - 2026-06-07

### Security
- **Hardened lurkd against a hostile or flapping upstream IRC server** — the
  daemon's connection *out* to the real networks, defense-in-depth on top of the
  client-layer bounds each upstream already inherits:
  - **Manager resilience:** bound the upstream registration retry with a timeout
    (a stalling upstream could otherwise wedge `Manager.Close()` forever — a
    clean-shutdown DoS); reset the reconnect backoff on a successful reconnect
    (a flapping upstream no longer plateaus at the 30 s maximum); and drain a
    removed upstream before returning, so a late event can't land in the backlog
    under a reused netid.
  - **Fanout amplification:** the per-session relay send is now non-blocking
    (drop + log on a full queue), so one slow or stuck client can no longer stall
    the upstream goroutine and freeze fan-out to every attached session; skip the
    live fan-out entirely when no client is attached; and suppress `KILL`/`ERROR`
    from the upstream→client relay — a hostile upstream `KILL`ing lurkd's nick was
    relayed verbatim and disconnected every attached client.
  - **Ingest trust:** clamp a hostile upstream's forged `@time` to a sane window
    at ingestion (it could otherwise skew CHATHISTORY ordering and the
    `BEFORE`/`AFTER`/`AROUND`/`BETWEEN`/`TARGETS` windows), and honor the
    per-target cap during backlog rehydration too.
  - **State burst:** sanitize the ISUPPORT-derived PREFIX symbols before they
    reach attached clients in the synthetic NAMES attach-burst.

## [1.2.2] - 2026-06-06

### Security
- **Hardened the lurkd↔client trust boundary** — defense-in-depth for the
  bouncer's two trust edges. (The primary recommendation remains to restrict
  network access to the bouncer; see the new "Network access" section in
  [`docs/LURKD.md`](docs/LURKD.md).)
  - **Against a hostile client attacking the bouncer:** gate `BOUNCER BIND` on
    authentication (closes a pre-auth netid-existence oracle); cap concurrent
    sessions against goroutine/FD exhaustion; cap configured networks at
    `BOUNCER ADDNETWORK` (256) against config/disk/FD exhaustion and an O(N²)
    netid scan; bound BOUNCER attr parsing (≤32 keys, ≤512-byte values); validate
    `ADDNETWORK`/`CHANGENETWORK` `host:port` (numeric port, well-formed address)
    before persisting or dialing; and reject malformed `CHATHISTORY` requests —
    `TARGETS` non-timestamp refs, and `BETWEEN` reversed/equal bounds that forced
    a full backlog scan for an empty result.
  - **Against a hostile/compromised bouncer attacking the client:** add
    `client.ParseBouncerAttrs`, which strips terminal-escape/bidi controls from
    bouncer-supplied network names and identities before they reach a consumer;
    the attr key/value caps above also bound a hostile bouncer's `LISTNETWORKS`
    reply.
- **`docs/LURKD.md`:** a new **Network access** section recommending loopback
  binding / firewall restriction as the primary hardening, with the code-level
  bounds framed as defense-in-depth, not a substitute.

## [1.2.1] - 2026-06-06

### Security
- **Hardened the client and the lurkd daemon against a malicious server/upstream** —
  a sweep of hostile-input bounds across every layer, all hermetically tested:
  - **Wire:** cap per-message IRCv3 tags (64) and parsed params (32) so a hostile
    line can't explode the tag map or the params slice.
  - **Client state:** bound tracked channels (4096) and members-per-channel
    (65536); cap `ERR_NICKNAMEINUSE` retries during registration (no unbounded
    nick growth under a 433 flood); guard `TOPIC`/`RPL_TOPIC`/`RPL_TOPICWHOTIME`
    against creating phantom channel state for channels never joined.
  - **Negotiation:** bound the ISUPPORT token map (512) against `005` floods and
    clamp ISUPPORT integers (`NICKLEN`/`CHANNELLEN`/`TOPICLEN`/`TARGMAX`) to sane
    ceilings; reassemble multi-chunk SASL server challenges (8 KiB cap) so a long
    server nonce can no longer break SASL or exhaust the client.
  - **TUI:** sanitize and length-cap the server-controlled buffer title in the
    empty-buffer hint — the one render site that bypassed terminal-escape
    sanitization.
  - **lurkd:** cap the per-attach state burst (500 channels) against a hostile
    upstream's channel flood; right-size the backlog JSONL scanner buffer to
    16 KiB (was 1 MiB) and add a write-side line-length cap with a matching
    read/ring invariant.

## [1.2.0] - 2026-06-06

### Added
- **SASL SCRAM-SHA-256** (`SCRAM-SHA-256`, RFC 5802/7677) — a challenge-response
  mechanism that proves knowledge of the password without sending it, selectable
  via a network's SASL `mechanism`. Standard-library crypto only, with
  server-signature verification that rejects a rogue authenticator.
- **MONITOR presence tracking** — request the `monitor` capability and watch a
  set of nicks for online/offline transitions (`Monitor`/`Unmonitor`/
  `UnmonitorAll`/`MonitorList` actions, `MonitoredOnline` state, and
  `HandleMonitorOnline`/`HandleMonitorOffline` handlers), re-established
  automatically after an auto-reconnect.
- **`standard-replies`** — request the capability and register one handler for
  server `FAIL`/`WARN`/`NOTE` messages via `HandleStandardReply`.
- **`irc.Message.Clone` + nil-safe `Tags.Set`** — deep-copy a message
  (independent `Tags` map and `Params` slice) so a cached message no longer
  aliases the receive buffer.
- **Launcher bouncer fields** — the network add/edit form now has Bounce
  address / NetID / Client-ID fields, so a lurkd connection can be configured
  from the UI instead of hand-editing `config.json`.
- **lurkd backlog rotation** — `backlog.WithMaxFileSize` bounds each per-target
  JSONL file, rotating `<target>.jsonl` to `.jsonl.1` and rehydrating the ring
  from both segments on restart. Off by default (unbounded), backward-compatible.
- **`config.Validate`** — an additive validator that reports malformed network
  entries (empty name/addr, unknown SASL mechanism, bad `host:port`, duplicate
  names, a misconfigured bounce block). It is not wired into `Load`, so existing
  configs keep loading unchanged.
- **`docs/CONFIG.md`** — a full client configuration reference: the JSON schema
  for networks/identity/SASL/bounce, the `LURK_*` environment variables, and the
  CLI flags.

### Fixed
- `/quit` (and `Ctrl-C`) now sends `QUIT` to **every** connected network, not
  just the active one, so the other servers see a clean quit instead of a silent
  TCP drop.
- lurkd `CHANGENETWORK` now reconnects the upstream when its address or TLS flag
  changes (previously it kept dialing the old host until a restart), and
  `CHANGENETWORK`/`DELNETWORK` roll back their in-memory change if persisting the
  config fails, so disk and memory no longer diverge.

## [1.1.1] - 2026-06-06

### Changed
- **lurkd is now in every native package.** It already shipped as a standalone
  binary and in the macOS `.pkg` / Windows `.zip`; it is now also a separate
  `lurkd` `.deb`/`.rpm` (so a headless server can install just the daemon) and is
  bundled into the Windows `.msi` and `.msix`. `SHA256SUMS` now covers the
  hyphenated `.rpm` names too.

## [1.1.0] - 2026-06-06

### Added
- **lurkd** — a new single-user IRC bouncer daemon (`cmd/lurkd`). It holds
  persistent IRCv3 connections to upstream networks and serves them to attached
  lurk (or any IRCv3) clients over a local TLS listener, so clients can detach
  and reattach without missing messages.
  - **Persistent upstreams:** one `client.Client` per configured network, with
    auto-reconnect. Networks that fail their initial connect are retried in the
    background with capped exponential backoff — the daemon comes up and serves
    reachable networks immediately (resilient startup).
  - **CHATHISTORY backlog:** structured JSONL store (`backlog/`) with per-line
    server-assigned msgids and `server-time`, survives restarts with full cursor
    fidelity. In-memory ring fronts the store for live fan-out.
  - **`soju.im/bouncer-networks`:** the BOUNCER verb (`BIND`/`LISTNETWORKS`/
    `ADDNETWORK`/`CHANGENETWORK`/`DELNETWORK`) and the `user/network@client`
    authcid fallback for third-party clients.
  - **SASL PLAIN + PBKDF2 auth:** bouncer password hashed with
    PBKDF2-HMAC-SHA256 (600 000 iterations, 16-byte salt). `lurkd -hashpw`
    bootstraps the hash for the config.
  - **TLS out of the box:** generates a self-signed ECDSA P-256 certificate on
    first run when no cert/key is configured; paths are persisted to the config
    for reuse. Explicit cert/key config is supported and unchanged.
  - **Per-client cursors:** read positions per `(clientID, netid, target)`,
    flushed on clean detach and every 30 s. Survive restarts within the last
    flush window.
  - **Graceful shutdown:** `SIGINT`/`SIGTERM` closes the listener, drains
    in-flight sessions, closes all upstream connections, flushes the backlog
    store and cursor store. A second signal forces immediate exit.
  - **Packaging:** `lurkd` binary shipped alongside `lurk` in all release
    artifacts (cross-compiled for linux/darwin/windows, amd64/arm64, included
    in the Windows `.zip`). No systemd unit — just the binary.
  - Config at `~/.config/lurkd/config.json` (XDG-aware, `0600`).
    See [`docs/LURKD-DESIGN.md`](docs/LURKD-DESIGN.md) for the full design.

## [1.0.2] - 2026-06-06

### Added
- `-insecure-auth` flag (env `LURK_INSECURE_AUTH`) to allow sending credentials
  over a plaintext connection, matching the per-network setting the launcher
  already exposed.

### Fixed
- **Reconnect file-descriptor leak:** a peer-initiated drop only cancelled the
  connection without closing the local socket, so every auto-reconnect leaked a
  descriptor (stuck in `CLOSE_WAIT`). The supervisor now closes the dead
  transport before re-dialing.
- The Events stream is now closed when a non-reconnecting session ends (an
  `AutoReconnect=false` or `ConnectConn` client), so a consumer ranging over
  `Events()` is released instead of blocking forever — matching the supervised
  path.
- The per-user **Status…** menu is now offered to half-ops (`%`), not just
  founder/admin/op, so a half-op can use the kick and lower-mode actions the
  server would honor.
- Automatic CTCP replies (VERSION/PING/TIME/CLIENTINFO) are rate-limited with a
  shared token bucket, so a peer flooding private CTCP queries can no longer
  induce a 1:1 outbound NOTICE flood (a reflection vector).
- A chat-log buffer named `.` or `..` can no longer redirect its log file outside
  the log directory; such names fall back to `server`.
- `LURK_*` boolean env vars now keep their documented default on an unrecognized
  value instead of silently becoming false.

## [1.0.1] - 2026-06-05

### Added
- The per-user nicklist menu gains a **Status…** sub-menu that grants/revokes any
  membership mode the server advertises via PREFIX — op, half-op, voice, and
  founder/admin where supported — alongside Kick and Ban. Modes above your own
  level are hidden.

### Changed
- Mention highlights (`/highlight`) now render over the active-buffer blue accent
  instead of red.

### Fixed
- Remote "X is typing…" indications now clear on time. A self-rescheduling expiry
  tick prunes and repaints when the typing TTL elapses, so an indication no longer
  lingers past its timeout on an idle channel with the editor blurred (where the
  cursor blink wasn't forcing a redraw).

## [1.0.0] - 2026-06-05

First stable release: the IRCv3 client, the multi-network terminal UI, and the
native packaging are feature-complete and hardened.

### Security
- Sanitize every server-controlled identifier before display — member nicks in
  the nicklist, channel/PM titles in the sidebar, the typing indicator, the
  NETWORK label, and the per-user menu title — closing the remaining
  terminal-escape injection paths in the TUI.

### Fixed
- **Concurrency:** guard the session transport and capability negotiator against
  the reconnect supervisor (a dedicated mutex for the `tr`/`neg` pointers, an
  internal lock in the cap negotiator) and synchronize the registration signal,
  so a `Close`/`Quit` landing mid-reconnect can't race the supervisor. Confirmed
  clean under `go test -race`.
- `Quit` now sends the QUIT line before stopping the reconnect supervisor, so it
  isn't raced away by teardown; the supervisor also skips a stray dial when a
  stop arrives during backoff.
- PRIVMSG / NOTICE / CTCP ACTION are split with a target-aware budget, so a long
  target can no longer push a line past the 512-byte wire limit.
- TUI: the "new messages" divider no longer appears above live messages;
  scrollback-search indices survive a scrollback trim; cross-network `/part` and
  `/close` act on the owning network's server; re-homed buffers clear stale
  activity markers.
- Protocol: reconcile channel membership on `RPL_ENDOFNAMES` (no ghost members
  after a re-issued `NAMES`); only enable capabilities actually requested on
  `CAP ACK`.

### Changed
- Module path is now `github.com/exec/lurk` so `go install` resolves.
- Documentation consolidated: stale planning/research notes removed; the
  architecture docs refreshed; this changelog added.

## [0.4.0] - 2026-06-05

### Added
- **Multiple networks at once:** a unified sidebar grouped by network;
  `/connect <name>` dials another saved network at runtime, `/disconnect` drops
  one. Event routing, the nicklist, and commands are all network-scoped.
- **Auto-reconnect** with capped backoff and channel re-join after an unexpected
  drop.
- **Theming:** dark (Catppuccin Mocha) / light (Latte) chosen from the terminal
  background; `NO_COLOR` selects a monochrome theme.
- CTCP auto-replies (VERSION/PING/TIME/CLIENTINFO) and a highlight bell.
- Scrollback search (`/search`, Ctrl-R), a read marker, custom highlight words,
  `/ignore`, and per-network chat logging (`-log`).
- Buffer navigation (`Alt+1`–`9`, `Alt+A`); long messages split across lines.
- Op/channel command surface: `/mode /kick /ban /op /deop /voice /devoice
  /invite /notice /ctcp /whowas /motd /clear`, plus empty-state hints and unread
  counts.

## [0.3.0] - 2026-06-04

### Added
- Network launcher: run `lurk` with no `-server` to pick / add / edit / delete
  saved networks (each bundling its own identity and SASL), persisted to a 0600
  JSON config.
- `/list` channel directory in a filterable modal overlay.
- On-screen key-hint footer and a channel topic bar; compact WHOIS rendering.

### Fixed
- Nicklist scroll follows the selection (no longer clips off-screen).
- QUIT/NICK now show in the channels you share with the user, not the server
  buffer.
- `/help` preserves intentional newlines; macOS scroll-key guidance corrected.

## [0.2.5] - 2026-06-03
### Fixed
- WHOIS replies render their data, not just the trailing label.
- Added regression coverage against known IRC-client crash/CVE classes.

## [0.2.4] - 2026-06-03
### Added
- AWAY / CTCP ACTION / TOPIC helpers and `/away`, `/whois` commands.
### Security
- Strip Unicode bidi controls (Trojan Source) from displayed text.
- Bound the advertised/enabled capability sets on CAP LS/LIST/ACK.

## [0.2.3] - 2026-06-03
### Security
- Malicious-server fuzz harness; fixed a `Serialize` round-trip misframing.
- Cap auto-opened TUI buffers to bound hostile-server window growth.
### Fixed
- STATUSMSG-target routing and CASEMAPPING-change re-keying.

## [0.2.2] - 2026-06-03
### Security
- Extend terminal-escape sanitization to the `-plain` line client via the shared
  `client.SanitizeTerminal`.

## [0.2.1] - 2026-06-03
### Security
- Refuse to send cleartext credentials to an untrusted server; bound
  BATCH/CAP/channel-map growth against a hostile server.

## [0.2.0] - 2026-06-03
### Security
- Terminal-escape sanitization in the TUI.
### Added
- GitHub Actions release workflow; Windows `.msi` (wixl) and `.msix` (makeappx).
- TUI keybinding and scrollback improvements.

## [0.1.0] - 2026-06-02

Initial release.

### Added
- IRCv3 client library — `irc`, `conn`, `cap`, `sasl`, `isupport`, `client` —
  with full message-tag support, TCP/TLS transport, capability negotiation, SASL
  (PLAIN/EXTERNAL), `RPL_ISUPPORT` + casemapping, and a high-level client with
  registration, an event stream, and channel/user state tracking. Standard
  library only.
- IRCv3 features: `away-notify`, `account-notify`/`extended-join`, `chghost`,
  `batch`, `draft/chathistory`, `+typing`, and `standard-replies`.
- Bubble Tea terminal UI and a `-plain` line client.
- Native packaging: `.deb`, `.rpm`, `.pkg`, and Windows archives, with a
  `-version` flag.

[Unreleased]: https://github.com/exec/lurk/compare/v1.2.4...HEAD
[1.2.4]: https://github.com/exec/lurk/compare/v1.2.3...v1.2.4
[1.2.3]: https://github.com/exec/lurk/compare/v1.2.2...v1.2.3
[1.2.2]: https://github.com/exec/lurk/compare/v1.2.1...v1.2.2
[1.2.1]: https://github.com/exec/lurk/compare/v1.2.0...v1.2.1
[1.2.0]: https://github.com/exec/lurk/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/exec/lurk/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/exec/lurk/compare/v1.0.2...v1.1.0
[1.0.2]: https://github.com/exec/lurk/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/exec/lurk/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/exec/lurk/compare/v0.4.0...v1.0.0
[0.4.0]: https://github.com/exec/lurk/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/exec/lurk/compare/v0.2.5...v0.3.0
[0.2.5]: https://github.com/exec/lurk/compare/v0.2.4...v0.2.5
[0.2.4]: https://github.com/exec/lurk/compare/v0.2.3...v0.2.4
[0.2.3]: https://github.com/exec/lurk/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/exec/lurk/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/exec/lurk/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/exec/lurk/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/exec/lurk/releases/tag/v0.1.0
