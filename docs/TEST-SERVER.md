# Live test server — Ergo IRCv3

A real [Ergo](https://github.com/ergochat/ergo) v2.18.0 instance is running on the
homelab for integration/smoke testing. It is reachable from the dev LAN.

| | |
|---|---|
| Host | `10.0.0.116` (Proxmox LXC 116 `ergo`, on `eqr`) |
| Plaintext | `10.0.0.116:6667` |
| TLS | `10.0.0.116:6697` (self-signed cert — use `InsecureSkipVerify` / opt-out verify for tests) |
| Network name | `ErgoTest` |
| Server name | `ergo.test` |
| Casemapping | `ascii` |

## SASL test account

| field | value |
|---|---|
| account / nick | `lurktest` |
| password | `testpass123` |

Mechanisms advertised: `PLAIN`, `EXTERNAL`, `SCRAM-SHA-256`. Use **PLAIN** for the
SASL integration test (authzid empty, authcid `lurktest`, passwd `testpass123`).

## Capabilities advertised (CAP LS 302)

```
account-notify account-tag away-notify batch cap-notify chghost
draft/account-registration draft/channel-rename draft/chathistory
draft/event-playback draft/extended-isupport draft/languages
draft/metadata-2 draft/multiline draft/no-implicit-names draft/persistence
draft/pre-away draft/read-marker draft/relaymsg echo-message extended-join
extended-monitor invite-notify labeled-response message-tags multi-prefix
sasl=PLAIN,EXTERNAL,SCRAM-SHA-256 server-time setname standard-replies
userhost-in-names znc.in/playback znc.in/self-message
```

## Sample ISUPPORT (005)

```
AWAYLEN=390 BOT=B CASEMAPPING=ascii CHANLIMIT=#:100
CHANMODES=Ibe,k,fl,CEMRUimnstu CHANNELLEN=64 CHANTYPES=# CHATHISTORY=1000
ELIST=U EXCEPTS EXTBAN=,m FORWARD=f INVEX KICKLEN=390 MAXLIST=beI:100
MAXTARGETS=4 MODES MONITOR=100 MSGREFTYPES=msgid,timestamp NETWORK=ErgoTest
NICKLEN=32 PREFIX=(qaohv)~&@%+ SAFELIST SAFERATE STATUSMSG=~&@%+
TARGMAX=NAMES:1,LIST:1,KICK:,WHOIS:1,USERHOST:10,PRIVMSG:4,TAGMSG:4 ...
```

Note the 5-level `PREFIX=(qaohv)~&@%+` and four-group `CHANMODES` — good stress
for the isupport parser.

## Notes
- A plaintext listener on `:6667` (all interfaces) was enabled specifically so
  tests can connect without TLS friction. Prefer TLS (`:6697`) where practical.
- The server is managed by systemd (`systemctl status ergo`) on the container and
  starts on boot. Manage via `ssh eqr` then `sudo pct exec 116 -- <cmd>`.
- Live tests SHOULD be opt-in (e.g. guarded by an env var like `LURK_TEST_SERVER`
  / `go test -tags live`) so `go test ./...` stays hermetic by default and CI
  without LAN access still passes. Deterministic unit/integration tests should use
  the in-process mock server.
