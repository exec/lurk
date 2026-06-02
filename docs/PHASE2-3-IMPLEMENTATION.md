# Cycle 3 — Phases 2 & 3 implementation guide (self-contained)

This is the actionable task list. Everything you need is here — the IRCv3
message formats are inlined so you do not need the (absent) `reference/` clones.
Work top to bottom; commit after each step with `go test -race ./...` green.
**You cannot reach the live server (private LAN IP) — verify with hermetic tests
only** (mock server in `client/integration_test.go`, unit tests in `*_test.go`).

The Phase-2 capabilities are already negotiated (see `client.DefaultCaps`:
`account-notify`, `extended-join`, `away-notify`, `chghost`, `server-time`,
`message-tags`, `userhost-in-names`, `multi-prefix`, `account-tag`, `setname`).
So Phase 2 is message *handling*, not cap setup. Phase 3 adds `batch` and
`draft/chathistory` to DefaultCaps.

---

## PHASE 2 — member metadata: away / account / host

### Step 2.1 — Extend `client.Member`
In `client/state.go`, add fields to `Member`:
```go
type Member struct {
    Nick     string
    Prefixes string
    User     string // ident, when known (from userhost-in-names / extended-join / chghost)
    Host     string // hostname, when known
    Account  string // services account name; "" if not logged in
    Away     bool   // currently away (from away-notify)
}
```
Keep zero values meaningful (unknown = ""/false). `addMember` should preserve
existing metadata when a nick is re-seen (e.g. a later NAMES shouldn't wipe an
Account learned from extended-join). Update `renameMember` to carry fields over.

### Step 2.2 — Capture user/host/account on the way in
- **NAMES (userhost-in-names):** entries are `@nick!user@host`. Cycle-1 fix
  truncates at `!` to get the nick (`applyNamReply`). Now ALSO parse the
  `!user@host` portion and store `User`/`Host` on the member.
- **extended-join:** the JOIN we receive is
  `:nick!user@host JOIN <channel> <account> :<realname>`. `<account>` is `*` when
  not logged in. Where you track JOIN, set the joining member's `User`/`Host`
  (from the source mask) and `Account` (Param(1), treating `*`/`""` as "none").
  Today JOIN handling only records the nick — extend it.

### Step 2.3 — Handle the notify messages (state + dispatch)
These arrive as their own commands; route them in `client/run.go` `handle` (add
cases) into new `state` methods, then they continue to `dispatch` as events.

- **account-notify:** `:nick!user@host ACCOUNT <account>` — `*` means logged out.
  Update that nick's `Account` in EVERY channel it's in. Add `irc.ACCOUNT="ACCOUNT"`.
- **away-notify:** `:nick!user@host AWAY :<message>` = now away (set `Away=true`);
  `:nick!user@host AWAY` (no param) = back (`Away=false`). Add `irc.AWAY="AWAY"`.
- **chghost:** `:nick!user@host CHGHOST <newuser> <newhost>` — update that nick's
  `User`/`Host` in every channel. Add `irc.CHGHOST="CHGHOST"`.

Add a `state` helper like `updateMemberEverywhere(foldedNick, func(*Member))` that
walks all channels and applies a mutation, since these are not channel-scoped.

### Step 2.4 — Surface it in the TUI
In `tui/view.go` `renderNicklist` (and `tui/style.go`):
- **Away users:** render dimmed (e.g. a faint style) and/or append a marker. Pull
  `Away` from `client.Member` (now available via `cli.Members(channel)`).
- **Account:** optionally show a small badge (e.g. a trailing `·` or distinct
  color) for logged-in users. Keep it subtle; don't break the width math
  (measure with `lipgloss.Width`, still `truncate` to column width).
- WHOIS already renders account/away via numerics (Cycle-3 Phase 1) — no change.
- Optionally render concise notices ("alice is now away (lunch)", "bob logged in
  as bob") in the relevant channel buffer via the existing event routing
  (`tui/events.go`) — keep them low-noise (you may gate behind the active buffer).

### Phase 2 acceptance (hermetic tests)
Add to `client/*_test.go` using the `newState()` + apply* pattern (see
`client/names_userhost_test.go`) and/or the mock server:
- NAMES with `@alice!a@h` populates `User="a" Host="h"`.
- extended-join `:bob!b@h JOIN #c acct :Bob` → member bob has `Account="acct"`,
  `User="b"`, `Host="h"`; account `*` → `Account==""`.
- `:bob!b@h ACCOUNT *` clears account; `:bob ACCOUNT name` sets it across channels.
- `:bob AWAY :brb` sets `Away`; `:bob AWAY` clears it.
- `:bob!b@h CHGHOST b2 h2` updates User/Host everywhere bob is.

---

## PHASE 3 — batch, chathistory, typing, standard-replies

### Step 3.1 — `batch` handling
Add `draft/chathistory` and `batch` to `client.DefaultCaps`. The parser already
preserves the `@batch=<ref>` tag and the `BATCH` command.

Wire format:
```
:srv BATCH +<ref> <type> [params...]      ; batch opens
@batch=<ref> :nick!u@h PRIVMSG #c :hello   ; message belongs to <ref>
:srv BATCH -<ref>                          ; batch closes
```
Minimum viable handling in `client`:
- Track open batches by ref (a map ref -> {type, params}). On `BATCH +ref`,
  record it; on `BATCH -ref`, drop it. Don't error on unknown refs.
- Tag each dispatched event with its batch info so the TUI can group/label it.
  Simplest: add `Event.BatchType() string` (reads the open batch the event's
  `@batch` tag points to, "" if none). Store enough on the client to resolve it
  at dispatch time.
- You do NOT need to buffer-and-delay in Phase 3; pass events through in order,
  annotated. (Netsplit collapsing is a later nicety.)

### Step 3.2 — `chathistory` backlog on join
Goal: a freshly joined channel shows recent history instead of being empty.
- Negotiate `draft/chathistory` (done in 3.1). Gate the feature on
  `cli.CapEnabled("draft/chathistory")`.
- After a successful JOIN to a channel (and only if the cap is enabled), send:
  ```
  CHATHISTORY LATEST <target> * <limit>     e.g. CHATHISTORY LATEST #chan * 50
  ```
  Add `irc.CHATHISTORY="CHATHISTORY"` and a `Client.ChatHistoryLatest(target string, limit int) error`.
- The server replies with a `BATCH +ref chathistory <target>` … `BATCH -ref`
  containing PRIVMSG/NOTICE lines carrying `@time` tags (use `Event.Time()` for
  the timestamp). These flow through `Events()` like any message.
- TUI: render history lines into the target buffer. Because they arrive after the
  user may have typed, prepend/insert by timestamp is ideal but optional;
  acceptable Phase-3 behavior is to append them (they arrive right after JOIN,
  before live traffic) and mark the batch (e.g. a faint "── history ──" divider
  using `Event.BatchType()=="chathistory"`). Don't double-count unread.
- Decide where the JOIN→request happens: the client can auto-issue it on its own
  JOIN echo, OR the TUI can call `cli.ChatHistoryLatest` when it opens a channel
  buffer. **Prefer client-side auto-request** (so `-plain` benefits too), gated on
  the cap, issued when the client sees its OWN join confirmed.

### Step 3.3 — typing notifications (`+typing` client tag)
Wire format (client-only tag on a TAGMSG, no text body):
```
@+typing=active :nick!u@h TAGMSG #chan      ; also values: paused, done
```
- **Receive:** in `client`, handle `TAGMSG`; expose the `+typing` value. Either a
  dedicated event or let the TUI read `ev.Tags.Get("+typing")` on a `TAGMSG`
  event (the bridge already delivers it). TUI: show "X is typing…" in the status
  bar for the active buffer; expire it after ~6s or on a `done`/a message from X.
- **Send:** the user's own typing. Needs a way to send a message WITH tags —
  `client.write` doesn't support tags today. Add:
  ```go
  // SendTagged sends a command with client-only tags (keys keep their '+').
  func (c *Client) SendTagged(tags map[string]string, command string, params ...string) error
  ```
  Build an `irc.Message{Tags: tags, Command: command, Params: params}`, serialize
  it (`Message.Serialize()` handles tags + escaping), and enqueue via the same
  path as `Send`. Then `Client.Typing(target, state string)` sends
  `SendTagged(map[string]string{"+typing": state}, irc.TAGMSG, target)`.
  Gate on `cli.CapEnabled("message-tags")`. In the TUI, debounce: send `active`
  when the user types into a channel (at most every few seconds), `done` on send,
  `paused` after a short idle. Keep it simple; don't spam.

### Step 3.4 — standard-replies (FAIL / WARN / NOTE)
Wire format:
```
FAIL <COMMAND> <code> [context...] :<human-readable description>
WARN <COMMAND> <code> [context...] :<description>
NOTE <COMMAND> <code> [context...] :<description>
```
- Add `irc.FAIL/WARN/NOTE`. Route in the TUI (`tui/events.go`) to the active or
  server buffer, formatted readably, e.g. `! FAIL JOIN ACCOUNT_REQUIRED: You must
  register…`, with WARN/NOTE styled less severely. No client state needed.

### Phase 3 acceptance (hermetic tests)
- batch: feed `BATCH +x chathistory #c` / `@batch=x … PRIVMSG` / `BATCH -x`;
  assert `Event.BatchType()=="chathistory"` for the inner message and "" outside.
- chathistory: assert `ChatHistoryLatest` writes `CHATHISTORY LATEST #c * 50`;
  assert auto-request fires on own-join only when the cap is enabled (use the
  mock server to advertise/omit `draft/chathistory`).
- typing: `SendTagged`/`Typing` serializes `@+typing=active TAGMSG #c`; receiving
  a `+typing` TAGMSG surfaces the state.
- standard-replies: a `FAIL`/`WARN`/`NOTE` line renders the expected formatted
  string (test the formatter directly).

---

## Protocol quick-reference (inlined)

| Feature | You receive | You send |
|---|---|---|
| extended-join | `:n!u@h JOIN #c <account> :<real>` (`*`=none) | normal JOIN |
| account-notify | `:n!u@h ACCOUNT <account>` (`*`=logout) | — |
| away-notify | `:n!u@h AWAY :<msg>` / `:n!u@h AWAY` | `AWAY :<msg>` / `AWAY` |
| chghost | `:n!u@h CHGHOST <user> <host>` | — |
| batch | `BATCH +ref type [p]` … `@batch=ref …` … `BATCH -ref` | — |
| chathistory | a `chathistory` batch of past messages w/ `@time` | `CHATHISTORY LATEST <t> * <n>` |
| server-time | `@time=2026-06-02T...Z` on any message | — (use `Event.Time()`) |
| typing | `@+typing=active TAGMSG <t>` | `@+typing=<state> TAGMSG <t>` |
| standard-replies | `FAIL/WARN/NOTE <CMD> <code> [ctx] :<desc>` | — |
| msgid (future) | `@msgid=...` | — |

Numerics already added in Phase 1: `irc.RPL_AWAY` (301) and the WHOIS burst
(311/312/313/317/318/319/330/338/671).

## Suggested commit sequence
1. `client.Member` fields + NAMES/extended-join capture (+ tests)
2. account-notify / away-notify / chghost handling (+ tests)
3. TUI away-dim / account badge (+ render tests)
4. batch tracking + `Event.BatchType()` (+ tests)
5. chathistory request on join + TUI backlog render (+ tests)
6. `SendTagged`/`Typing` + receive `+typing` + TUI indicator (+ tests)
7. standard-replies formatting (+ tests)
8. update README/docs; ensure `go test -race ./...` green; push to `main`.

When all phases are green and pushed, the headline UX is: join a channel and see
recent history, away/registered status in the nicklist, "typing…" indicators,
and clean FAIL/WARN/NOTE messages — all on top of the Phase-1 user context menu.
