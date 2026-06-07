# lurkd — the Lurk IRC bouncer

**lurkd** is a single-user IRC bouncer (BNC) daemon. It holds persistent IRCv3
connections to your upstream networks and serves them to attached clients over a
local TLS listener, so you can detach and reattach without missing messages: the
daemon stays connected while your client is closed, and CHATHISTORY pulls the
backlog automatically when you return.

It is part of the Lurk repository (`cmd/lurkd`) but a separate binary from the
`lurk` client, and it shares the same standard-library protocol stack (`irc`,
`conn`, `cap`, `sasl`, `isupport`, `client`). lurkd is headless — it imports no
terminal-UI libraries.

This is the operator guide. For the internal design and the package layout, see
[`LURKD-DESIGN.md`](LURKD-DESIGN.md); for how the whole repository fits together,
see [`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Install

```sh
go install github.com/exec/lurk/cmd/lurkd@latest   # or: go build ./cmd/lurkd
```

Or install a native package from a release:

```sh
# Debian/Ubuntu — lurkd ships as its own package (no client/TUI pulled in)
sudo dpkg -i lurkd_<version>_amd64.deb
# Fedora/RHEL
sudo rpm -i lurkd-<version>-1.x86_64.rpm
# macOS .pkg and the Windows .zip/.msi install both lurk and lurkd
```

On Linux `lurkd` is a **separate package** from `lurk`, so a headless server gets
just the daemon. See [`PACKAGING.md`](PACKAGING.md) for the full matrix.

---

## Quick start

1. **Write a config** at `~/.config/lurkd/config.json` (the directory is
   XDG-aware; the file is created `0600`):

   ```json
   {
     "listen": { "addr": "127.0.0.1:6697" },
     "bouncer_auth": {
       "user": "alice",
       "password_hash": ""
     },
     "networks": [
       {
         "name": "Libera.Chat",
         "addr": "irc.libera.chat:6697",
         "tls": true,
         "identity": { "nick": "alice", "user": "alice", "realname": "Alice" },
         "sasl": { "mechanism": "PLAIN", "authcid": "alice", "password": "upstream-secret" },
         "channels": ["#lurk"]
       }
     ]
   }
   ```

2. **Set the bouncer password.** lurkd never stores the password itself, only a
   PBKDF2 hash. Generate one and paste it into `bouncer_auth.password_hash`:

   ```sh
   lurkd -hashpw
   # reads a password from stdin, prints:  pbkdf2-sha256:600000:<salt>:<dk>
   ```

3. **Run it:**

   ```sh
   lurkd                          # reads ~/.config/lurkd/config.json
   lurkd -config /path/to.json    # explicit path (also $LURKD_CONFIG)
   lurkd -listen :6697            # override the listen address
   lurkd -version
   ```

   On first run, if no TLS material is configured, lurkd **generates a self-signed
   certificate** (see [TLS](#tls)) and starts serving. It dials the configured
   networks; any that are unreachable are retried in the background while the rest
   come up immediately.

4. **Point a client at it** — see [Connecting clients](#connecting-clients).

---

## Configuration reference

The config is a single JSON document. Resolution order for its path:
`$LURKD_CONFIG` → `$XDG_CONFIG_HOME/lurkd/config.json` →
`~/.config/lurkd/config.json`. It is always written `0600` (it holds upstream
credentials in cleartext, the same model as irssi/WeeChat).

### `listen`

| Field | Type | Notes |
|-------|------|-------|
| `addr` | string | Listen address, e.g. `":6697"` or `"127.0.0.1:6697"`. |
| `tls_cert` | string | Path to a PEM certificate. Optional — auto-generated on first run if omitted (see [TLS](#tls)). |
| `tls_key` | string | Path to the matching PEM private key. |

### `bouncer_auth`

The credential a client presents to attach to **the bouncer** — distinct from any
upstream-network credential.

| Field | Type | Notes |
|-------|------|-------|
| `user` | string | The bouncer username clients authenticate as. |
| `password_hash` | string | `pbkdf2-sha256:<iter>:<saltB64>:<dkB64>`, produced by `lurkd -hashpw`. Never the cleartext password. |

### `networks[]`

Each entry is one upstream network lurkd maintains a persistent connection to.

| Field | Type | Notes |
|-------|------|-------|
| `netid` | int | Stable integer id (the `BOUNCER BIND` target). **Optional** — lurkd assigns one at load if omitted, and `BOUNCER ADDNETWORK` allocates the next free id. |
| `name` | string | Display name (e.g. `"Libera.Chat"`); also the `user/network` selector. |
| `addr` | string | Upstream `host:port`. |
| `tls` | bool | Use TLS to the upstream. |
| `identity` | object | `{ "nick", "user", "realname" }` lurkd registers with upstream. |
| `sasl` | object | `{ "mechanism", "authcid", "password" }` — **upstream** SASL credentials, stored server-side only, never sent to attached clients. |
| `channels` | []string | Autojoin channels (rejoined automatically after reconnect). |

---

## TLS

The listener is TLS-only by design — credentials are never accepted over a
plaintext link (the `AUTHENTICATE PLAIN` handler refuses a non-TLS connection
before decoding anything).

- **First-run self-signed cert.** When `listen.tls_cert`/`tls_key` are unset,
  lurkd generates an ECDSA P-256 self-signed certificate and key in the config
  directory on first run, writes the key `0600`, records both paths in the config
  (so subsequent runs reuse them), and serves TLS. No manual setup is required.
  Because it is self-signed, clients must skip certificate verification (the lurk
  `insecure_skip_verify` option, below) or trust the generated cert.
- **Explicit cert.** Set `tls_cert`/`tls_key` to your own PEM pair (e.g. a real
  certificate from a CA) and lurkd uses it unchanged.

---

## Authentication

lurkd keeps **two credential layers** strictly separate — a known bouncer-security
practice:

- **Bouncer auth** (`bouncer_auth`): who may *attach to lurkd*. Verified with
  SASL PLAIN over TLS against the PBKDF2 hash. This password is never round-tripped
  to any upstream.
- **Upstream auth** (`networks[].sasl`): how lurkd authenticates to each *upstream
  network*. Set once in the config, used by lurkd's own connection, never exposed
  to attached clients (it is also omitted from `BOUNCER LISTNETWORKS` replies).

Bootstrap the bouncer password hash with `lurkd -hashpw` (it reads a line from
stdin and prints the `pbkdf2-sha256:…` string for `password_hash`).

---

## Connecting clients

A client attaches to lurkd as if it were a normal IRC server: it connects to the
listener, authenticates with SASL PLAIN as the bouncer user, and selects which
upstream network to bind to.

### lurk

In `~/.config/lurk/config.json`, add a saved network with a `bounce` block:

```json
{
  "name": "Libera via lurkd",
  "addr": "127.0.0.1:6697",
  "tls": true,
  "insecure_skip_verify": true,
  "identity": { "nick": "alice", "user": "alice", "realname": "Alice" },
  "sasl": { "mechanism": "PLAIN", "authcid": "alice", "password": "bouncer-password" },
  "bounce": { "addr": "127.0.0.1:6697", "netid": 1, "client_id": "laptop" }
}
```

- `bounce.addr` — the bouncer's `host:port` (lurk dials this instead of the
  network's own address).
- `bounce.netid` — the bouncer-side network id; lurk sends `BOUNCER BIND <netid>`
  after SASL and before `CAP END`.
- `bounce.client_id` — an optional per-client cursor name. It is folded into the
  SASL username as `<user>@<client_id>` (here `alice@laptop`), so a phone and a
  laptop keep **independent** backlog positions.
- `insecure_skip_verify` is needed for the first-run self-signed certificate.

### Third-party clients

Any IRCv3 client works via the universal `user/network@client` username
convention (the `soju.im/bouncer-networks` fallback). Set the SASL username (or
the server password, for clients without a separate username field) to:

```
alice/Libera.Chat@laptop
```

where `alice` is the bouncer user, `Libera.Chat` is the network `name`, and
`laptop` is the per-client cursor id. Clients that do speak
`soju.im/bouncer-networks` may instead negotiate the capability and
`BOUNCER BIND <netid>` directly.

---

## Managing networks at runtime

A client that finishes registration **without** binding stays on the bouncer's
**control context** and can manage networks with the `soju.im/bouncer-networks`
verbs (requires the capability negotiated):

```
BOUNCER LISTNETWORKS                       list configured networks (no passwords)
BOUNCER ADDNETWORK name=…;host=…;port=…;…  add one; lurkd allocates the next netid,
                                           connects it, and persists the config
BOUNCER CHANGENETWORK <netid> name=…;…     edit a network
BOUNCER DELNETWORK <netid>                 stop and remove a network
```

Changes are persisted to the config (`0600`) and broadcast to other connected
control clients via `soju.im/bouncer-networks-notify`. The lurk client exposes
these as `client.BouncerListNetworks()` / `BouncerAddNetwork()` /
`BouncerChangeNetwork()` / `BouncerDelNetwork()`.

---

## How it works

A short tour; the full design is in [`LURKD-DESIGN.md`](LURKD-DESIGN.md).

- **Upstream manager** (`server/upstream.go`) — one `client.Client` per network,
  reusing the whole protocol stack with auto-reconnect. Every event is forwarded
  to the server, which both stores it and fans it out live.
- **Backlog store** (`backlog/`) — a structured JSONL store per `(network, target)`
  with a server-assigned opaque msgid and `server-time` on every line, fronted by
  an in-memory ring. The same msgid is stamped on the live copy, so a message you
  saw live resolves in a later CHATHISTORY query.
- **CHATHISTORY** (`server/chathistory.go`) — serves
  `LATEST/BEFORE/AFTER/AROUND/BETWEEN/TARGETS` in `draft/chathistory` batches.
  Attaching clients receive only a synthetic JOIN/topic/names **state burst** —
  no message replay is pushed; the client pulls history itself.
- **Cursors** (`server/cursor.go`) — per `(client, network, target)` read
  positions, flushed on detach and every 30 s, surviving restarts.
- **Away** — lurkd marks an upstream `AWAY :detached` when no client is attached
  and clears it when one reattaches.
- **Sanitization** — all relayed and stored text passes through
  `client.SanitizeForRelay`, which strips terminal-hijacking escapes (ESC, C1,
  DEL, Trojan-Source bidi) while preserving IRC formatting (bold/colour/italic).

---

## Operations

- **Resilient startup.** A network that fails its initial connect does not stop
  the daemon: lurkd logs it, serves the reachable networks immediately, and retries
  the rest in the background with capped backoff.
- **Graceful shutdown.** `SIGINT`/`SIGTERM` closes the listener, drains in-flight
  sessions, closes all upstream connections, and flushes the backlog and cursor
  stores. A second signal forces an immediate exit.
- **On-disk locations** (XDG-aware):
  - config — `~/.config/lurkd/config.json`
  - self-signed cert/key — alongside the config
  - backlog (JSONL) — `~/.local/share/lurkd/backlog/<netid>/<target>.jsonl`
  - cursors — `~/.local/share/lurkd/cursors/cursors.json`

  All credential- and message-bearing files are written `0600`.

---

## Network access

> **Restrict network access first.** The code-level input bounds (registration
> timeout, SASL authentication, TLS gate) are defence-in-depth for when network
> restrictions are absent. The single most effective hardening step is to limit
> who can reach the listener at the network layer.

lurkd is a **single-user** daemon. If you run it on the same machine as your
IRC client, bind to loopback — no remote access is needed and the attack surface
shrinks to zero for anyone off the machine:

```json
"listen": { "addr": "127.0.0.1:6697" }
```

If you run lurkd on a remote host and connect to it from a laptop or phone:

- **Prefer a firewall rule** that allows only your own IP(s) to reach the listen
  port (`ufw allow from 203.0.113.42 to any port 6697`, or equivalent). A VPN or
  SSH tunnel achieves the same isolation without a public port.
- Avoid binding to `0.0.0.0` / `::` (all interfaces) on an internet-facing host
  without such a restriction: even with SASL authentication and a strong password,
  an open port increases the exposure window for future vulnerabilities.
- If you need a public-facing bouncer accessible from multiple clients, consider
  running lurkd behind a reverse proxy or within a private network segment.

The listener is **TLS-only**: credentials are never decoded over plaintext. SASL
authentication is required when `bouncer_auth` is configured. These guarantees
hold regardless of network topology, but they are not a substitute for limiting
which hosts can attempt a connection in the first place.

---

## Limitations (v1)

- **Single-user.** One `bouncer_auth` credential. (Multi-user, web auth, and PAM
  are explicit non-goals — run one daemon per user.)
- **One connection per network on the lurk side.** lurk opens one connection per
  bound network; the single-connection multiplex (gamja/goguma-style) is the
  intended future evolution and the v1 design keeps it an additive, UI-only change.
- `CHANGENETWORK`/`DELNETWORK` do not roll back the in-memory change if the config
  save fails (a restart resyncs from disk); `ADDNETWORK` does.
- No SCRAM yet (PLAIN over TLS for bouncer auth; PLAIN/EXTERNAL upstream).

---

## See also

- [`LURKD-DESIGN.md`](LURKD-DESIGN.md) — the full design: package layout, security
  model, the phased build order, and the design-council provenance.
- [`ARCHITECTURE.md`](ARCHITECTURE.md) — the whole repository's packages and
  contracts.
- [`PACKAGING.md`](PACKAGING.md) — building and releasing the binaries.
