# Roadmap / ideas

Forward-looking notes only. Shipped work lives in [`CHANGELOG.md`](CHANGELOG.md).

## lurkd follow-ups
The bouncer ([`docs/LURKD.md`](docs/LURKD.md)) is feature-complete. Remaining:
- [ ] TUI launcher fields for adding/editing a `bounce` block, so attaching lurk
      to a bouncer doesn't need hand-editing `config.json` (functional today).
- [ ] Single shared connection multiplexed per-netid on the lurk side (ballot
      #16 / gamja-goguma style) — the intended evolution of the v1 one-connection-
      per-network model; the design keeps it an additive, `tui/`-only change.
- [ ] `CHANGENETWORK` reconnect when host/port/tls change (currently config-only).
- [ ] In-memory rollback on a failed `CHANGENETWORK`/`DELNETWORK` save (today a
      restart resyncs from disk; `ADDNETWORK` already rolls back).

## Docs / polish
- [ ] Add a screenshot/GIF of the TUI to the README.
- [ ] `CONTRIBUTING.md` (style, hermetic-test expectations, commit conventions).
- [ ] A config-file / flags reference doc for the **client** (the `config/` JSON
      schema and the `LURK_*` environment variables). The lurkd config reference
      already lives in [`docs/LURKD.md`](docs/LURKD.md).
- [ ] An IRCv3 capability support matrix (what's negotiated and handled).

## Nice to have
- [ ] Sign the Windows `.msi`/`.msix` (currently shipped unsigned — see
      `docs/PACKAGING.md`).
- [ ] Optional encrypted credential storage (today the config is 0600 cleartext,
      matching irssi/WeeChat).
- [ ] SASL SCRAM mechanisms (PLAIN/EXTERNAL are implemented today).

## Testing
- Native Go fuzz targets cover the hostile-input sinks (`irc.FuzzParse`,
  `client.FuzzClientHandle`, `client.FuzzSanitizeTerminal`, `tui.FuzzUpdate`);
  run an extended pass with e.g.
  `go test ./client/ -run=x -fuzz=FuzzClientHandle -fuzztime=2m`.
