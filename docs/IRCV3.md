# IRCv3 support

This matrix describes the current implementation. Capability negotiation,
library support, and a feature exposed in the TUI are separate things.

Classic IRC text formatting needs no IRCv3 capability. The TUI safely renders
bold, italic, underline, strikethrough, reverse, indexed colors, and hex colors
by default; see [`disable_formatting`](CONFIG.md#disable_formatting--boolean)
to opt out. Monospace is already the terminal's font. Incoming ANSI escape
sequences are never accepted as formatting.

## Negotiation

The client starts with `CAP LS 302`, collects multiline advertisements, and
requests the intersection of the advertised capabilities and its configured
request list. A capability becomes enabled after `CAP ACK`; a rejected request
does not enable it. `CAP NEW`, `CAP DEL`, and `CAP LIST` update negotiation state.

[`client.DefaultCaps`](../client/config.go) supplies the default request list.
Library callers can override it with `client.Config.Caps`: `nil` uses the
defaults, while an explicit empty slice requests none. Configuring SASL or a
`BouncerNetID` adds the corresponding capability even with a custom list.
The stock command-line client uses the defaults; it has no capability-list flag.

Implementation: [`cap/`](../cap), [`client/config.go`](../client/config.go),
and [`client/run.go`](../client/run.go). Negotiation tests:
[`cap/cap_test.go`](../cap/cap_test.go) and [`cap/ack_test.go`](../cap/ack_test.go).

## Client library and TUI

“Default” means requested when advertised, not guaranteed to be available on
every connection. The behavior below refers to direct IRC connections; see the
next section for lurkd's client-facing connection.

| Capability / request-list entry | Requested | Implemented behavior and UI limits |
|---|---|---|
| `cap-notify` | Default | Tracks newly available and removed capabilities; requests newly advertised capabilities from the configured request list. |
| `server-time` | Default | `Event.Time()` reads `@time`, falling back to receive time. The TUI uses it for message timestamps and history ordering. |
| `echo-message` | Default | The TUI uses server echoes instead of local echoes when enabled. Without it, successful sends are echoed locally. |
| `message-tags` | Default | Parses and preserves tags in events. The TUI sends and displays typing notifications using the `+typing` tag on `TAGMSG`. |
| `account-notify` | Default | Updates members' accounts from `ACCOUNT`; the nicklist badges logged-in members. |
| `extended-join` | Default | Captures the joining member's account alongside user/host. The realname parameter is not stored in member state. |
| `multi-prefix` | Default | Tracks all membership prefixes in `NAMES`; the nicklist and status menu use server-advertised prefix modes. |
| `away-notify` | Default | Updates members' away flags; the nicklist dims away members. Away-message text is not stored on members. |
| `chghost` | Default | Updates member user/host in every shared channel. Host data is available to the TUI's ban-mask builder. |
| `account-tag` | Default | Preserves the `account` tag on events. It does not update member accounts from message tags. |
| `setname` | Default | Delivers `SETNAME` events, but has no tracked member-realname field. The TUI suppresses these metadata notices. |
| `userhost-in-names` | Default | Extracts user/host from `nick!user@host` entries in `NAMES`. |
| `invite-notify` | Default | Delivers `INVITE` events. The TUI falls back to generic server-buffer text; it has no dedicated invitation UI. |
| `batch` | Default | Resolves a message's batch reference to `Event.BatchType()`. The TUI handles history batches and hides batch-control lines; this is not multiline-message reassembly. |
| `draft/chathistory` | Default | Requests recent history after self-JOIN. The library has `LATEST`, `BEFORE`, `AFTER`, and `TARGETS` helpers; the TUI pages older messages and recovers reconnect gaps and missed PMs. See [history limits](#history-limits). |
| `monitor` | Default request-list entry | Library methods send `MONITOR` commands and track online/offline replies. The TUI has no watch-list UI. See the MONITOR note below. |
| `standard-replies` | Default | Exposes `FAIL`, `WARN`, and `NOTE` handlers. The TUI formats command, code, context, and description. |
| `sasl` | When configured | Supports `PLAIN`, `EXTERNAL`, and `SCRAM-SHA-256`. See [configuration](CONFIG.md). |
| `soju.im/bouncer-networks` | When `BouncerNetID > 0`, or explicitly requested | Binds before `CAP END`. Library helpers cover network listing, addition, modification, and deletion. The TUI connects to saved netids using one connection per network; it has no runtime bouncer-management screen. |

MONITOR commands and `MONITOR=N` in `ISUPPORT` are handled separately from CAP.
Although `monitor` appears in the default request list, the command methods do
not check `CapEnabled("monitor")`. `MonitorLimit()` reads the ISUPPORT token;
zero means no usable limit was advertised, not a zero-sized watch list.

`+typing` is a client tag carried by `message-tags`, not a separate capability
requested by Lurk. Typing sends are throttled; remote indicators expire and are
scoped to the owning network and conversation.

`labeled-response`, `draft/event-playback`, and
`soju.im/bouncer-networks-notify` are not in the stock client's default request
list. Raw tags and events remain available to library callers; the stock TUI
does not correlate labeled requests, replay membership history, or maintain a
bouncer-network notification list.

Implementation: [`client/actions.go`](../client/actions.go),
[`client/event.go`](../client/event.go), [`client/run.go`](../client/run.go),
[`client/state.go`](../client/state.go), [`client/bouncer.go`](../client/bouncer.go),
[`tui/events.go`](../tui/events.go), [`tui/style.go`](../tui/style.go), and
[`tui/history.go`](../tui/history.go).

Representative hermetic tests:

- Member metadata and NAMES: [`client/member_meta_test.go`](../client/member_meta_test.go)
  and [`client/names_userhost_test.go`](../client/names_userhost_test.go).
- Tags, batch resolution, typing, and automatic history:
  [`client/phase3_test.go`](../client/phase3_test.go) and
  [`tui/phase3_test.go`](../tui/phase3_test.go).
- Paging, deduplication, and reconnect/PM recovery:
  [`tui/history_test.go`](../tui/history_test.go).
- MONITOR: [`client/monitor_test.go`](../client/monitor_test.go).
- Standard replies: [`client/standard_replies_test.go`](../client/standard_replies_test.go).
- Bouncer binding: [`client/bouncer_bind_test.go`](../client/bouncer_bind_test.go).

## lurkd: upstream and client-facing support

These are two separate negotiations. lurkd's upstream client uses
[`client.DefaultCaps` plus `echo-message` and `labeled-response`](../server/upstream.go).
Upstream authentication uses the client library's SASL mechanisms. Attached
clients authenticate to lurkd independently, using **PLAIN over TLS**.

The following table covers every capability in lurkd's
[`advertisedCaps`](../server/server.go). “Advertised” describes its CAP surface;
the implementation column records the scope of the actual behavior.

| Advertised capability | Implementation and limits |
|---|---|
| `sasl` | Client-facing authentication supports PLAIN only, with a TLS gate. It does not expose the upstream's credentials or SASL mechanism. |
| `soju.im/bouncer-networks`, `soju.im/bouncer-networks-notify` | Network binding and management, batched listing, and network-change notifications to subscribed control sessions. |
| `draft/chathistory` | Serves `LATEST`, `BEFORE`, `AFTER`, `AROUND`, `BETWEEN`, and `TARGETS` from the local backlog. A request's limit is capped at 100; without a store, history batches are empty. |
| `draft/event-playback` | Has a replay-filter gate, but the current store persists only `PRIVMSG` and `NOTICE`. Membership events are not retained for replay. |
| `server-time`, `message-tags` | Live fan-out adds `time` and preserves upstream tags except upstream labels. Stored messages receive a consistent server-assigned `msgid` for live delivery and replay. |
| `batch` | Emits CHATHISTORY and network-list batches; live upstream batches can also pass through fan-out. |
| `labeled-response` | Matches labels on outgoing `PRIVMSG`/`NOTICE` to upstream echoes and returns them only to the sending session. This is limited echo correlation, not general labeled command/reply handling. |
| `cap-notify` | Advertises a fixed capability set and handles CAP negotiation/listing. It does not dynamically change that advertised set. |
| `echo-message` | Relies on the upstream's echoes for self-send storage and fan-out. There is no synthesized echo when the upstream does not support it. |
| `multi-prefix` | Attach-time NAMES includes all prefixes when requested, otherwise the highest prefix. |
| `away-notify`, `account-notify`, `extended-join`, `chghost`, `setname` | Live upstream metadata events are relayed when received. The attach-time snapshot is JOIN/topic/NAMES; it does not restore all member metadata to an attaching client. |

Live fan-out largely preserves upstream message shapes rather than adapting
every metadata event to each attached client's negotiated capabilities.
`account-tag`, `userhost-in-names`, `invite-notify`, `monitor`, and
`standard-replies` are **not advertised by lurkd**. It can still relay upstream
tags/events and emits local `FAIL` replies; that does not add them to CAP LS.

Implementation: [`server/server.go`](../server/server.go),
[`server/bouncer.go`](../server/bouncer.go), [`server/relay.go`](../server/relay.go),
[`server/fanout.go`](../server/fanout.go), [`server/burst.go`](../server/burst.go),
and [`server/chathistory.go`](../server/chathistory.go).
Tests: [`server/registration_test.go`](../server/registration_test.go),
[`server/sasl_test.go`](../server/sasl_test.go),
[`server/fanout_test.go`](../server/fanout_test.go),
[`server/phase7_test.go`](../server/phase7_test.go), and
[`server/chathistory_test.go`](../server/chathistory_test.go).

## History limits

The TUI retains up to 2,000 lines per buffer. History requests respect the
server's `CHATHISTORY` limit and `MSGREFTYPES`, use bounded queues, and time out.
Stable message IDs let the TUI remove live/replay overlap; timestamp references
are used when needed. Replayed history does not ring the mention bell.

Availability depends on the server or bouncer's retained backlog. A transport
reconnect and recovered chat history do not imply that membership events or
all member metadata have been reconstructed.

For configuration and operating limits, see [CONFIG.md](CONFIG.md),
[LURKD.md](LURKD.md), and [ARCHITECTURE-TUI.md](ARCHITECTURE-TUI.md).
