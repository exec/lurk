# TODO

Running list of follow-ups and hardening ideas. Newest at the top of each
section.

## Security / robustness

- [x] **Malicious-server fuzz / integration harness.** Done — native Go fuzz
      targets covering the four hostile-input sinks, runnable as seed-corpus
      tests under plain `go test` and explorable with `-fuzz`:
      - `irc.FuzzParse` — `Parse` never panics; any accepted line round-trips
        through `Serialize`+`Parse`. **Found a real round-trip bug:** `Parse(": :")`
        yielded `Command ":"`, which serialized to `":"` and reparsed as a bare
        source. Fixed in `serialize.go` (a command must begin with a letter/digit;
        a non-final param must not begin with ':'), regression seed persisted in
        `irc/testdata/fuzz/FuzzParse/`.
      - `client.FuzzClientHandle` — arbitrary wire lines through the full
        `handle`/`track` path (PING, CAP/SASL, numerics, all state mutators) over
        a no-op transport; asserts no panic. 5.3M execs clean.
      - `client.FuzzSanitizeTerminal` — output never carries an unsafe control
        rune and is idempotent. 2.3M execs clean.
      - `tui.FuzzUpdate` — arbitrary inbound events through `Update`+`View`
        (route → format/sanitize → width math). No panic.

      Run an extended pass with e.g.
      `go test ./client/ -run=x -fuzz=FuzzClientHandle -fuzztime=2m`.

## Notes

- v0.2.0 fixed terminal-escape injection in the TUI; v0.2.1 hardened the
  server-trust boundary (cleartext-credential refusal, BATCH/CAP/channel-map
  bounds); v0.2.2 extended the escape sanitizer to the `-plain` line client via
  the shared `client.SanitizeTerminal`.
- v0.2.3: fuzz harness + Serialize round-trip guard; TUI auto-buffer cap;
  STATUSMSG-target routing, CASEMAPPING-change re-keying, casemapping-correct
  SelfPrefixes, and the multibyte-safe completion prefix match.
