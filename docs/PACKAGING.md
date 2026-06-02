# Packaging & releases

Lurk ships as native packages plus plain binaries. Everything is produced by a
single script from a pure-Go (CGO-disabled) cross-compile, so no C toolchain or
Docker is needed.

## Build all artifacts

```sh
scripts/package.sh 0.1.0      # version is embedded via -ldflags main.version
```

Outputs land in `dist/` (git-ignored):

| Artifact | Platform | Built with |
|---|---|---|
| `lurk_<v>_amd64.deb`, `lurk_<v>_arm64.deb` | Debian/Ubuntu | [nfpm](https://nfpm.goreleaser.com) |
| `lurk-<v>-1.x86_64.rpm`, `lurk-<v>-1.aarch64.rpm` | Fedora/RHEL/openSUSE | nfpm |
| `lurk_<v>_macos.pkg` | macOS (universal x86_64+arm64) | `pkgbuild` + `lipo` |
| `lurk_<v>_windows_amd64.zip` | Windows | `zip` |
| `lurk_<v>_<os>_<arch>` raw binaries + `darwin_universal` | all | `go build` |
| `SHA256SUMS` | — | `shasum` |

The `.deb`/`.rpm` install the binary to `/usr/bin/lurk`; the `.pkg` installs to
`/usr/local/bin/lurk`.

### Tool prerequisites
- `go` (1.26+) — always.
- `nfpm` — for `.deb`/`.rpm` (`brew install nfpm` / `go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest`).
- `pkgbuild` + `lipo` — for `.pkg` (preinstalled on macOS).
- `zip` — for the Windows archive.

Missing tools are skipped with a notice; the script never hard-fails on them.

## Installing

```sh
# Debian/Ubuntu
sudo dpkg -i lurk_0.1.0_amd64.deb
# Fedora/RHEL
sudo rpm -i lurk-0.1.0-1.x86_64.rpm
# macOS
sudo installer -pkg lurk_0.1.0_macos.pkg -target /
# Windows: unzip and put lurk.exe on PATH
```

`lurk -version` prints the embedded version.

## Windows `.msi` (deferred)

A `.zip` is the supported Windows artifact today. A proper installer is left to
CI rather than built on macOS, because:

- A useful `.msi`/`.msix` should be **code-signed** (an unsigned installer trips
  SmartScreen and the MSIX trust model outright), which needs a Windows signing
  cert.
- The clean, reproducible path is a Windows GitHub Actions runner using
  [WiX](https://wixtoolset.org/) (or GoReleaser's `msi` feature). Cross-building
  an MSI from macOS via `msitools`/`wixl` is possible but unsigned and fiddly.

When we add release CI, the MSI step belongs there. Until then, Windows users
take the `.zip` (also consumable by Scoop/winget manifests).

## Cutting a release

```sh
scripts/package.sh 0.1.0
git tag v0.1.0 && git push origin v0.1.0
gh release create v0.1.0 dist/* --title "lurk 0.1.0" --notes "…"
```
