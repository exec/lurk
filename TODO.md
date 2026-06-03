# TODO

Running list of follow-ups and hardening ideas. Newest at the top of each
section.

## Security / robustness

- [ ] **Malicious-server fuzz / integration harness.** Drive the in-process mock
      server (the `net.Pipe` + scripted `mockServer` template in
      `client/integration_test.go`) with randomized, oversized, and adversarial
      messages — truncated sources, huge param counts, embedded control bytes,
      never-closed BATCH/CAP floods, out-of-order numerics — and assert the
      client never panics, never deadlocks, and never grows memory without
      bound. A Go fuzz target over `irc.Parse` + the `client.handle`/`track`
      path would cover the protocol layer; a second target over `tui.Update`
      with synthetic `ircMsg` events would cover the render path. Goal: turn the
      manual "look for a remote-triggerable panic" review into a standing check.

## Notes

- v0.2.0 fixed terminal-escape injection in the TUI; v0.2.1 hardened the
  server-trust boundary (cleartext-credential refusal, BATCH/CAP/channel-map
  bounds). The sanitizer now lives in `client.SanitizeTerminal` and is shared by
  both the TUI and the `-plain` line client.
