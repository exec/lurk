# Contributing to Lurk

Lurk's client library, TUI, and bouncer are feature-complete. Contributions focus
on bug fixes, hardening, small useful features, and documentation. Keep each
change focused and preserve existing public contracts where possible.

## Getting started

Use the Go version required by [`go.mod`](go.mod), and run commands from the
checkout root. Go fetches the dependencies recorded in `go.mod` and `go.sum`.
Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) before changing code; for TUI
work, also read [`docs/ARCHITECTURE-TUI.md`](docs/ARCHITECTURE-TUI.md).

To run the client during development:

```sh
go run ./cmd/lurk
```

Use `-config` with a throwaway config path for isolated manual testing. See
[`docs/CONFIG.md`](docs/CONFIG.md) for flags, identities, credentials, and logs,
and [`docs/LURKD.md`](docs/LURKD.md) for running the bouncer.

## Code conventions

- Match the surrounding Go style: godoc for exported identifiers, table-driven
  tests where useful, errors wrapped with `%w`, and no panics in library paths.
- Keep `irc`, `conn`, `cap`, `sasl`, `isupport`, `client`, `config`, `chatlog`,
  `server`, `bouncer`, `backlog`, and `cmd/lurkd` standard-library only. Only
  `tui/` and `cmd/lurk` may import the Charm libraries. The
  [dependency guard](server/depguard_test.go) checks transitive imports too.
- Keep TUI state changes in `Update`; reassign updated Bubble Tea components
  and carry their returned commands forward. Resolve buffers and actions through
  their owning network.
- Sanitize server-controlled text before displaying it, using the existing
  `client.SanitizeTerminal` path. Use `client.SanitizeForRelay` for bouncer relay
  text, where IRC formatting is preserved.
- Credit non-obvious borrowed designs in a source comment and
  [`CREDITS.md`](CREDITS.md); keep reference implementations out of the repository.

## Hermetic tests

Every new behavior needs a hermetic test. Ordinary tests must run without a live
IRC server or personal credentials.

For protocol work, reuse the scripted `net.Pipe` server and `ConnectConn` pattern
in [`client/integration_test.go`](client/integration_test.go). Bound waits with
contexts or deadlines, close both ends during cleanup, and wait for test-owned
goroutines to finish. Assert the wire exchange as well as the resulting state.

For TUI work, drive `Update` with synthetic keys, resize messages, and IRC events,
then inspect state and `View`. Examples include
[`tui/chrome_test.go`](tui/chrome_test.go),
[`tui/multinetwork_test.go`](tui/multinetwork_test.go), and
[`tui/launcher_test.go`](tui/launcher_test.go).

Run focused tests while developing, then the full checks below:

```sh
go test ./client -run '^TestFullHandshake$' -count=1
go test ./tui -run '^TestHelpFooterRendered$' -count=1
```

For parser or hostile-input changes, existing fuzz targets provide extra
coverage. For example:

```sh
go test ./client -run '^$' -fuzz '^FuzzSanitizeTerminal$' -fuzztime=30s
```

## Required checks

Before submitting a change, all of these must pass. `gofmt -l` must print nothing.
Do not disable a failing test or add a skip to make the suite pass.

```sh
go build ./...
go vet ./...
gofmt -l irc conn cap sasl isupport client tui cmd config chatlog server bouncer backlog
go test -race ./...
```

The [CI workflow](.github/workflows/ci.yml) also scans for reachable known
vulnerabilities with:

```sh
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

The test suite needs no running IRC service. Dependency/toolchain downloads and
the vulnerability scan can still require network access.

## Optional live smoke test

```sh
bash scripts/live-test.sh
```

The [harness](scripts/live-test.sh) fetches/builds Ergo, starts throwaway Ergo and
lurkd instances on loopback, runs the gated client/TUI tests, and cleans up on
exit. It requires Bash, Git, Go, and the usual Unix shell tools. It may fetch a
newer toolchain to build Ergo. `ERGO_BIN`, `ERGO_REF`, and port overrides are
documented in the script; `KEEP=1` intentionally leaves the daemons and workdir
running for inspection.

Live tests skip by default when their environment variables are unset. Keep
that gating intact: a live smoke test supplements the hermetic tests and is
never required to establish correctness. Its separate
[workflow](.github/workflows/live.yml) runs on main, nightly, and on demand.

## Commits and pull requests

Use a short commit subject describing the change; existing commits often prefix
the affected component, such as `client:` or `test(server):`. Follow the
[working agreement](CLAUDE.md)'s `Co-Authored-By` trailer requirement. For
Codex-assisted commits, the existing convention is:

```text
Co-Authored-By: OpenAI Codex <noreply@openai.com>
```

In the pull request, describe the problem, the resulting behavior, the checks
you ran, and any remaining limitations. Add regression tests for fixes and
update affected documentation. Record user-visible changes under `Unreleased`
in [`CHANGELOG.md`](CHANGELOG.md); keep future ideas in [`TODO.md`](TODO.md).
For capability changes, update the [support matrix](docs/IRCV3.md).

Packaging and release instructions live in
[`docs/PACKAGING.md`](docs/PACKAGING.md).
