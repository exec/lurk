# Lurk — Cycle 3 Plan: interactive nicklist + IRCv3 features

Two themes: (A) make the nicklist interactive with a per-user context menu
(message / whois / etc.), and (B) land the IRCv3 features that make that menu —
and the UI generally — genuinely useful. Builds on Cycle 2's `tui/` + `client/`.

## A. Interactive nicklist & context menu  (the headline ask)

Today the nicklist is render-only (no selection state); mouse cell-motion is
already enabled but unhandled.

1. **Focus model** (tui-core): add `focus` ∈ {input, nicklist, buffers} to
   `model`. Default = input. A key cycles focus to the nicklist (proposal:
   `Ctrl-U` "users", `Esc` returns to input — Tab stays completion). When the
   nicklist is focused, `↑/↓`/`j`/`k` move a `nickSel` index, `Enter` opens the
   context menu, `Esc` exits.
2. **Mouse** (tui-core): handle `tea.MouseClickMsg`/`MouseReleaseMsg`; map the
   click's Y within the nicklist column to a member → select + open menu. (Also
   wire click-to-switch on the buffer sidebar while we're here.)
3. **Context-menu overlay** (tui-view): a small modal list rendered ABOVE the
   frame via Lip Gloss v2 layering (`reference/lipgloss` canvas/layer; see the
   bubbletea `composable-views`/`canvas` examples). Actions:
   - **Message** → open/focus a query (PM) buffer for the nick.
   - **Whois** → `cli.Whois(nick)`, show the result (popup or status buffer).
   - **Ignore / Unignore** → client-side mute (TUI-side set; filters that nick).
   - **Invite to…**, **Copy nick**.
   - When we hold ops in the channel: **Op/Deop**, **Voice/Devoice**, **Kick**.
   Menu navigated `↑/↓/Enter/Esc`; closing returns focus to input.
4. **Action wiring** (tui-input/core): each menu item maps to a client call or a
   model mutation, reusing the existing `action` dispatch in `app.go`.

## B. Client-library additions needed for A  (owner: client)

- **WHOIS**: `cli.Whois(nick) error` + parse numerics **311** (userinfo), **312**
  (server), **313** (oper), **317** (idle/signon), **318** (end), **319**
  (channels), **330** (logged in as <account>), **301** (away), **338**, **671**
  (secure). Emit a structured `WhoisInfo` (assembled across the burst, closed by
  318) as an event the TUI can show. Add the numeric constants to `irc`.
- **Channel ops**: `Kick(channel, nick, reason)`, `ChannelMode(channel, change,
  args…)` (for Op/Voice), `Invite(nick, channel)`. (`SetNick`/`Join`/`Part`/
  `Privmsg` already exist.)
- **Self-privilege check**: expose the client's own prefixes in a channel so the
  menu can show op-only actions (`cli.Members(ch)` already carries prefixes —
  add a `cli.SelfPrefixes(ch) string` convenience).

## C. IRCv3 features  (prioritized; all supported by our Ergo test server)

Priority order — top items are cheap and directly improve the nicklist/whois UX:

1. **away-notify** — track `AWAY` (cap already negotiable); dim away users in the
   nicklist, show away text in whois. (client + tui-view)
2. **account-notify + extended-join** — track `ACCOUNT` and the extended JOIN
   account param; show a "logged-in/registered" badge in nicklist/whois.
3. **chghost** — update stored user@host on `CHGHOST` (matters once we keep
   user/host per member — capture it from userhost-in-names/extended-join).
4. **chathistory** (`draft/chathistory`) — on join, request recent backlog
   (`CHATHISTORY LATEST <target> * <n>`) and render it, so a freshly-joined
   buffer isn't empty. Depends on (5). Big UX win.
5. **batch** — handle `BATCH +/-ref` and the `batch` tag: buffer and group
   batched messages (chathistory playback, and collapse netsplit/netjoin into a
   single line). (irc already preserves the tag; add grouping in client + tui)
6. **typing notifications** (`message-tags` + `TAGMSG` + `+typing` client tag) —
   show "X is typing…" in the statusbar; emit our own `+typing` tags as the user
   edits the input (debounced).
7. **standard-replies** (`FAIL`/`WARN`/`NOTE <cmd> <code> :desc`) — render these
   cleanly in the status/active buffer instead of as raw lines.
8. **message-ids** (`msgid` tag) — surface `Event.MsgID()`; prerequisite for
   reply/react/redaction.
9. **reactions & replies** (`+draft/react`, `+draft/reply` client tags) — render
   reactions under messages; optional this cycle.
10. **monitor** — `MONITOR +` a watch list / PM targets; notify on online/offline
    (numerics 730/731/732…). 
11. **WHOX** — extended `WHO` to populate the nicklist with account/away/realname
    in one query on join (more efficient than per-nick whois).
12. **read-marker** (`draft/read-marker`) — sync read position across clients.

## Suggested sequencing & team

- **Phase 1 (the ask):** client WHOIS + ops (B) → tui-core focus/mouse + tui-view
  context-menu overlay (A). Ships the headline feature.
- **Phase 2 (UX polish):** away-notify, account-notify/extended-join, chghost
  (C1–C3) — visible in the now-interactive nicklist/whois.
- **Phase 3 (depth):** batch + chathistory (C4–C5), typing (C6), standard-replies
  (C7).
- **Phase 4 (optional):** msgid/react/reply, monitor, WHOX, read-marker.

Team mapping (reuse the existing agents): `client` owns B + the protocol side of
C; `tui-core` owns focus/mouse/overlay plumbing; `tui-view` owns menu rendering +
nicklist away/account styling; `tui-input` owns menu key handling + the typing
emitter. Same coordination model as Cycles 1–2.

## Notes
- Capture per-member `user@host` now (from userhost-in-names/extended-join) so
  chghost/whois/away have somewhere to live — extend `client.Member` with
  `User`, `Host`, `Account`, `Away` fields.
- Keep charm deps confined to `tui/`; protocol stays stdlib-only.
- Test against live Ergo (10.0.0.116); it supports every cap above.
