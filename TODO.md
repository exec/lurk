# Roadmap / ideas

Forward-looking notes only. Shipped work lives in [`CHANGELOG.md`](CHANGELOG.md).

## Toward 1.0
- [ ] Add a screenshot/GIF of the TUI to the README.
- [ ] `CONTRIBUTING.md` (style, hermetic-test expectations, commit conventions).
- [ ] A config-file / flags reference doc (the JSON schema in `config/` and the
      `LURK_*` environment variables).
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
