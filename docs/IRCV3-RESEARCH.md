# IRCv3 Protocol Reference (research distilled for Cycle 1)

Sources: ircv3.net specs, modern.ircdocs.horse. This is the working reference;
follow the linked specs for edge cases.

## 1. Wire format

```
['@' <tags> SPACE] [':' <source> SPACE] <command> {SPACE <param>} <crlf>
```

- Line ends with `\r\n`. Be tolerant on read: accept bare `\n`, strip stray `\r`.
- **Length budgets**: classic message body ≤ 512 bytes *including* `\r\n`. With
  message-tags, the tag segment adds up to 8191 bytes (client may receive),
  4094 bytes of which is the cap for client-added / server-added tag data.
  Over-long → server replies `417 ERR_INPUTTOOLONG`.
- **Source/prefix**: server name, or `nick!user@host` (user/host optional).
- **Params**: space-separated. The LAST param may contain spaces iff it is sent
  as the *trailing* param, marked on the wire by a leading `:`. A param must be
  sent as trailing if it is empty, contains a space, or begins with `:`.
- Encoding: prefer UTF-8; never assume it — treat as bytes, don't choke on
  invalid UTF-8. `UTF8ONLY` ISUPPORT token signals UTF-8-only servers.

## 2. Message tags

```
<tags> ::= <tag> {';' <tag>}
<tag>  ::= <key> ['=' <escaped_value>]
<key>  ::= ['+'] [<vendor> '/'] <name>     ; '+' = client-only tag
```

- Vendor prefix is a DNS name then `/` (e.g. `znc.in/server-time`).
- A key with no `=` is valued as empty string but is "present".
- **Escaping** (value only):

  | raw char | escape |
  |----------|--------|
  | `;`      | `\:`   |
  | space    | `\s`   |
  | `\`      | `\\`   |
  | CR       | `\r`   |
  | LF       | `\n`   |

  Unescaping: `\x` for any other `x` → `x` (drop the backslash). A trailing lone
  `\` produces nothing. Round-trip must be lossless for the table above.
- Example: `@aaa=bbb;ccc;example.com/ddd=eee :nick!u@h PRIVMSG #ch :Hello`
- `TAGMSG` carries tags with no text body (e.g. typing, react).

## 3. Connection registration

Recommended client opening sequence:

```
CAP LS 302
PASS <password>            ; only if server password configured
NICK <nick>
USER <user> 0 * :<realname>
... capability negotiation (and SASL) ...
CAP END
```

Server then sends, in order:
`001 RPL_WELCOME`, `002 RPL_YOURHOST`, `003 RPL_CREATED`, `004 RPL_MYINFO`,
one or more `005 RPL_ISUPPORT`, LUSERS replies, then MOTD (`375/372/376`) or
`422 ERR_NOMOTD`. Registration is "done" at `001`; capture `005` for features.

Always respond to `PING <token>` with `PONG <token>`.

## 4. Capability negotiation (CAP)

- `CAP LS 302` → server lists caps, possibly multiline:
  ```
  CAP * LS * :cap1 cap2 cap3        (the '*' before ':' = more lines coming)
  CAP * LS :cap4 sasl=PLAIN,EXTERNAL
  ```
  `302` enables: cap values (`key=val`), multiline LS, implicit `cap-notify`.
- `CAP REQ :cap1 cap2` → `CAP * ACK :cap1 cap2` or `CAP * NAK :...`.
  REQ is **atomic**: all-or-nothing. NAK ⇒ server changed nothing.
  Disable with leading `-`: `CAP REQ :-away-notify`.
- `CAP LIST` → currently-enabled caps (multiline w/ `*`).
- `CAP NEW :...` / `CAP DEL :...` → post-registration add/remove (needs
  cap-notify, implicit under 302). Client may REQ newly-offered caps.
- `CAP END` finishes negotiation and resumes registration. Ignored if already
  registered. Until `CAP END`, server must not send `001`.
- Caps are opaque, case-sensitive. `draft/` prefix = experimental.

Cycle-1 caps worth requesting if offered: `sasl`, `cap-notify`, `server-time`,
`message-tags`, `account-notify`, `extended-join`, `multi-prefix`,
`away-notify`, `chghost`, `echo-message`, `batch`, `labeled-response`,
`account-tag`, `setname`, `userhost-in-names`. (Cycle 1 must *negotiate* these
cleanly; deep handling of batch/labeled-response/history is later cycles.)

## 5. SASL (sasl-3.1 / 3.2)

- Requires `sasl` cap. Under 3.2, `sasl=PLAIN,EXTERNAL` advertises mechs.
- Flow:
  ```
  C: AUTHENTICATE PLAIN
  S: AUTHENTICATE +
  C: AUTHENTICATE <base64(authzid \0 authcid \0 passwd)>
  S: 900 <nick> <nick>!u@h <account> :You are now logged in as ...
  S: 903 <nick> :SASL authentication successful
  ```
- **PLAIN** payload: `authzid \x00 authcid \x00 passwd`, then base64. authzid is
  usually empty.
- **EXTERNAL**: `AUTHENTICATE EXTERNAL` then `AUTHENTICATE +` (empty response),
  identity comes from the TLS client certificate.
- **Chunking**: base64 payload split into 400-byte `AUTHENTICATE` lines. If the
  payload length is an exact multiple of 400 (including 0), send a final
  `AUTHENTICATE +` to signal the end. Empty response is `AUTHENTICATE +`.
- Abort with `AUTHENTICATE *`.
- Numerics: `900 RPL_LOGGEDIN`, `901 RPL_LOGGEDOUT`, `902 ERR_NICKLOCKED`,
  `903 RPL_SASLSUCCESS`, `904 ERR_SASLFAIL`, `905 ERR_SASLTOOLONG`,
  `906 ERR_SASLABORTED`, `907 ERR_SASLALREADY`, `908 RPL_SASLMECHS`.
- If SASL is requested, do it after `CAP ACK :sasl` and before `CAP END`.

## 6. RPL_ISUPPORT (005)

Format: `005 <nick> <TOKEN[=value]>... :are supported by this server`. Tokens
accumulate across multiple 005 lines. `-TOKEN` negates a previously-set token.

Key tokens for Cycle 1:
- `PREFIX=(ov)@+` — membership mode letters `(ov)` ↔ display symbols `@+`,
  highest privilege first. Drives nick-prefix parsing in NAMES/who.
- `CHANTYPES=#&` — valid channel-name first characters.
- `CHANMODES=A,B,C,D` — four comma groups: A=list modes (always param), B=always
  param, C=param only when set, D=never param. Needed to parse MODE correctly.
- `MODES=n` — max mode changes per MODE command.
- `NICKLEN`, `CHANNELLEN`, `TOPICLEN`, `KICKLEN`, `AWAYLEN` — length limits.
- `NETWORK=Name` — network display name.
- `CASEMAPPING=ascii|rfc1459` — see below.
- `STATUSMSG=@+` — allowed prefixes for status-targeted PRIVMSG (`@#chan`).
- `TARGMAX=PRIVMSG:4,...` — per-command max targets.
- `CHANLIMIT`, `EXCEPTS`, `INVEX`, `ELIST`, `UTF8ONLY`.

### CASEMAPPING (critical)
Nicks and channels are case-insensitive; the mapping says how to fold:
- `ascii`: A–Z ↔ a–z only.
- `rfc1459`: ascii PLUS `{}|~` are the lowercase of `[]\^` (i.e. fold
  `[→{, ]→}, \→|, ^→~`). (`rfc1459-strict` omits `~`/`^`.)
All nick/channel equality + map keys MUST go through the active fold function.

## 7. Numerics quick list (Cycle 1)

`001` welcome · `002` yourhost · `003` created · `004` myinfo · `005` isupport ·
`353` namreply · `366` endofnames · `375/372/376` motd · `422` nomotd ·
`433` nick in use · `432` erroneous nick · `451` not registered ·
`461` need more params · `462` already registered · `417` input too long ·
SASL `900`–`908`.

## 8. Notes for later cycles (don't build now, but don't paint into a corner)
`batch` (group messages via `@batch=ref`, `BATCH +ref type` / `BATCH -ref`,
nesting), `labeled-response` (`@label=` correlate replies), `chathistory`,
`message-ids` (`@msgid=`), `server-time` (`@time=`), `echo-message`, `multiline`,
`standard-replies` (`FAIL/WARN/NOTE <cmd> <code> :desc`), `monitor`, `metadata`,
`STS`, `WHOX`. Keep tag handling generic so these "just work" at the parse layer.
