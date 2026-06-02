# Ergo reference map

A trimmed read-only copy of [Ergo](https://github.com/ergochat/ergo) lives at
`reference/ergo/` (git-ignored; see `CREDITS.md`). Use it to check correct
protocol behaviour against a production IRCv3 implementation. **Do not copy code
verbatim** — study the approach, write our own. Credit non-obvious borrowed
design decisions in a source comment.

## Where to look, by Lurk package

### `irc` (parser) — the gold standard
- `reference/ergo/vendor/github.com/ergochat/irc-go/ircmsg/message.go` — message
  parse/serialize, line length handling, trailing-param logic, source parsing.
- `.../ircmsg/tags.go` — tag escape/unescape table, the exact edge cases
  (lone trailing `\`, unknown escapes), tag length budgets.
- `.../ircmsg/userhost.go` — nick!user@host splitting.

### `cap` (capneg)
- `reference/ergo/irc/caps/constants.go` — the full enumerated capability list.
- `reference/ergo/irc/caps/defs.go` — capability names/values, draft handling.
- `reference/ergo/irc/caps/set.go` — the cap Set, multiline LS generation,
  302 value encoding.

### `sasl`
- `reference/ergo/irc/handlers.go` — the `AUTHENTICATE` handler (search
  `authenticateHandler`): mechanism dispatch, 400-byte chunk handling, the
  `+`/`*` sentinels, numeric replies (900/903/904/…).
- `reference/ergo/irc/accounts.go` — SASL PLAIN/EXTERNAL/SCRAM credential logic.

### `isupport` (state)
- `reference/ergo/irc/isupport/list.go` — how ISUPPORT tokens are built/encoded,
  value escaping (`\xHH`), the `-TOKEN` negation semantics.
- `reference/ergo/irc/getters.go` + `reference/ergo/irc/server.go` — which tokens
  ergo emits (matches our live server's 005).
- `reference/ergo/irc/strings.go` — `Casefold` / `CasefoldChannel`. NOTE: Ergo
  uses PRECIS/UTF-8 casefolding, which is richer than the `ascii`/`rfc1459`
  CASEMAPPING our client must honour — but the live server advertises
  `CASEMAPPING=ascii`, so for Cycle 1 our ascii/rfc1459 fold is correct. Read
  this for context on why casemapping matters, not to copy PRECIS.

### `client`
- `reference/ergo/irc/client.go` — client lifecycle, registration gating on
  `CAP END`, PING/PONG, nick handling.
- `reference/ergo/irc/client_lookup_set.go` — casefolded client/nick tracking.
- `reference/ergo/irc/channel.go` + `irc/channelmanager.go` — channel membership,
  member prefixes / multi-prefix, NAMES (353/366) generation.
- `reference/ergo/irc/modes.go` — MODE parsing against CHANMODES A/B/C/D groups
  and PREFIX modes.
- `reference/ergo/irc/handlers.go` + `irc/commands.go` — server-side handling of
  JOIN/PART/QUIT/NICK/PRIVMSG, which tells us exactly what a client receives.
