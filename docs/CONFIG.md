# lurk — configuration reference

This document covers the `lurk` client's on-disk config file (the network
launcher store) and all environment variables and command-line flags. For the
`lurkd` bouncer daemon see [`LURKD.md`](LURKD.md).

---

## Config file

`lurk` stores saved networks in a JSON file written with owner-only permissions
(`0600`). Passwords are kept in cleartext, matching the convention of irssi and
WeeChat.

### Location

1. `$LURK_CONFIG` — set this env var to use a custom path.
2. `$XDG_CONFIG_HOME/lurk/config.json` — if `XDG_CONFIG_HOME` is set.
3. `~/.config/lurk/config.json` — the default.

The directory is created (`0700`) on first save if it does not exist.

### Top-level structure

```json
{
  "defaults": { ... },
  "networks": [ ... ]
}
```

#### `defaults` — `Identity`

Default identity fields used to pre-fill new network forms and as a fallback
when a network leaves those fields blank.

| Field | Type | Description |
|---|---|---|
| `nick` | string | Default nickname |
| `user` | string | Default username / ident |
| `realname` | string | Default realname (GECOS) |

#### `networks` — array of `Network`

Each entry is a saved server. `name` and `addr` are required.

| Field | Type | Default | Description |
|---|---|---|---|
| `name` | string | — | Display name; used as the unique key (case-insensitive). **Required.** |
| `addr` | string | — | Server address as `host:port`. **Required.** |
| `tls` | bool | `false` | Connect with TLS. |
| `insecure` | bool | `false` | Skip TLS certificate verification. |
| `allow_insecure_auth` | bool | `false` | Allow sending PASS/SASL credentials over a plaintext connection. Has no effect when `tls` is true. |
| `nick` | string | `defaults.nick` | Nickname for this network. Falls back to `defaults.nick`, then `"lurk"`. |
| `user` | string | `defaults.user` | Username / ident. Falls back to `defaults.user`. |
| `realname` | string | `defaults.realname` | Realname (GECOS). Falls back to `defaults.realname`, then `"lurk IRC client"`. |
| `pass` | string | — | Server PASS (sent before registration). |
| `sasl` | SASL | — | SASL authentication (see below). An empty/absent object disables SASL. |
| `channels` | []string | — | Channels to auto-join on connect (e.g. `["#lurk", "#go-nuts"]`). |
| `highlights` | []string | — | Extra words that trigger a mention highlight in the TUI. |
| `bounce` | BounceConfig | — | Route through a `lurkd` bouncer instead of connecting to `addr` directly (see below). The zero value disables bouncing. |

#### `sasl` — `SASL`

| Field | Type | Description |
|---|---|---|
| `mechanism` | string | SASL mechanism: `"PLAIN"` or `"EXTERNAL"`. An empty string disables SASL. |
| `username` | string | SASL username (used with `PLAIN`). |
| `password` | string | SASL password (used with `PLAIN`). |

#### `bounce` — `BounceConfig`

When `netid > 0` and `addr` is set, `lurk` dials the bouncer at `addr`
instead of the network's own address, authenticates with the network's
SASL/identity, and sends `BOUNCER BIND <netid>` to select the upstream network.

| Field | Type | Description |
|---|---|---|
| `addr` | string | Bouncer host:port (e.g. `"localhost:6697"`). |
| `netid` | int | Bouncer-side network ID (`BOUNCER BIND` target). Must be > 0 to enable bouncing. |
| `client_id` | string | Per-client cursor name (`@client`). When set and a SASL mechanism is configured, the SASL username is sent as `<user>@<client_id>` so multiple devices keep independent backlog positions. |

### Example

```json
{
  "defaults": {
    "nick": "alice",
    "user": "alice",
    "realname": "Alice"
  },
  "networks": [
    {
      "name": "Libera.Chat",
      "addr": "irc.libera.chat:6697",
      "tls": true,
      "sasl": { "mechanism": "PLAIN", "username": "alice", "password": "s3cret" },
      "channels": ["#lurk", "#go-nuts"],
      "highlights": ["alice", "alicepls"]
    },
    {
      "name": "Local",
      "addr": "127.0.0.1:6667"
    },
    {
      "name": "Libera via bouncer",
      "addr": "irc.libera.chat:6697",
      "tls": true,
      "nick": "alice",
      "sasl": { "mechanism": "PLAIN", "username": "alice", "password": "bouncer-pass" },
      "bounce": { "addr": "localhost:6697", "netid": 1, "client_id": "laptop" }
    }
  ]
}
```

---

## Chat log directory

When chat logging is enabled (`-log` / `LURK_LOG`), logs land under:

1. `$LURK_LOG_DIR` — custom directory.
2. `$XDG_DATA_HOME/lurk/logs` — if `XDG_DATA_HOME` is set.
3. `~/.local/share/lurk/logs` — the default.

Files are written as `<log-dir>/<network>/<target>.log`, one line per message
prefixed with the date (`2006-01-02`). Directories are created as needed
(`0700`); files are `0600`.

---

## Environment variables

Boolean env vars accept `1`, `true`, `yes`, `on` (true) and `0`, `false`,
`no`, `off` (false), case-insensitively. An unrecognized value keeps the
documented default.

| Variable | Type | Default | Description |
|---|---|---|---|
| `LURK_CONFIG` | path | — | Override the config-file path. |
| `LURK_LOG_DIR` | path | (XDG) | Override the chat-log directory. |
| `LURK_SERVER` | string | — | Server `host:port` to connect to directly (bypasses the launcher). |
| `LURK_NICK` | string | `lurk` | Nickname. |
| `LURK_USER` | string | — | Username / ident. Defaults to the nick when blank. |
| `LURK_REALNAME` | string | `lurk IRC client` | Realname (GECOS). |
| `LURK_PASS` | string | — | Server PASS. |
| `LURK_TLS` | bool | `false` | Connect with TLS. |
| `LURK_INSECURE` | bool | `false` | Skip TLS certificate verification. |
| `LURK_INSECURE_AUTH` | bool | `false` | Allow PASS/SASL over a plaintext connection. |
| `LURK_CHANNEL` | string | — | Channel to join immediately after connecting. |
| `LURK_PLAIN` | bool | `false` | Use the plain line-mode client instead of the TUI. |
| `LURK_LOG` | bool | `false` | Enable per-channel chat logs. |
| `LURK_SASL` | string | — | SASL mechanism: `PLAIN` or `EXTERNAL`. |
| `LURK_SASL_USER` | string | — | SASL username. |
| `LURK_SASL_PASS` | string | — | SASL password. |

---

## Command-line flags

Every flag has a corresponding environment variable (listed in the table
above). Flags take precedence over environment variables.

```
lurk [flags] [network-name]
```

When given a saved network name as the first positional argument, `lurk`
connects to it directly (skipping the interactive launcher). Without a
positional argument and without `-server`, the TUI launcher opens.

| Flag | Env | Description |
|---|---|---|
| `-server host:port` | `LURK_SERVER` | Connect directly to this server, bypassing the launcher. |
| `-nick <nick>` | `LURK_NICK` | Nickname. |
| `-user <user>` | `LURK_USER` | Username / ident. |
| `-realname <name>` | `LURK_REALNAME` | Realname (GECOS). |
| `-pass <pass>` | `LURK_PASS` | Server PASS. |
| `-tls` | `LURK_TLS` | Connect with TLS. |
| `-insecure` | `LURK_INSECURE` | Skip TLS certificate verification. |
| `-insecure-auth` | `LURK_INSECURE_AUTH` | Allow credentials over plaintext. |
| `-channel <#chan>` | `LURK_CHANNEL` | Channel to join after connecting. |
| `-plain` | `LURK_PLAIN` | Use the plain line-mode client instead of the TUI. |
| `-log` | `LURK_LOG` | Enable per-channel chat logs. |
| `-config <path>` | `LURK_CONFIG` | Config-file path for the network launcher. |
| `-sasl <MECH>` | `LURK_SASL` | SASL mechanism (`PLAIN` or `EXTERNAL`). |
| `-sasl-user <user>` | `LURK_SASL_USER` | SASL username. |
| `-sasl-pass <pass>` | `LURK_SASL_PASS` | SASL password. |
| `-version` | — | Print the version string and exit. |

### Plain-mode slash commands

When running with `-plain`, lines beginning with `/` are commands:

| Command | Description |
|---|---|
| `/join <#chan>` | Join a channel and set it as the current target. |
| `/part [<#chan>]` | Part a channel (defaults to the current channel). |
| `/msg <target> <text>` | Send a private message to a target. |
| `/me <text>` | Send a CTCP ACTION to the current channel. |
| `/away [<message>]` | Set or clear your away status. |
| `/nick <newnick>` | Change your nickname. |
| `/names [<#chan>]` | List channel members. |
| `/list [<pattern>]` | List available channels. |
| `/raw <line>` | Send a raw IRC line. |
| `/quit [<message>]` | Send QUIT and exit. |

A line starting with `//` sends a literal message beginning with a single `/`.
