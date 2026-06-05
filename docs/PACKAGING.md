# Packaging & releases

Lurk ships as native packages plus plain binaries. Everything is produced by a
single script from a pure-Go (CGO-disabled) cross-compile, so no C toolchain or
Docker is needed.

## Build all artifacts

```sh
scripts/package.sh 1.0.0      # version is embedded via -ldflags main.version
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
sudo dpkg -i lurk_1.0.0_amd64.deb
# Fedora/RHEL
sudo rpm -i lurk-1.0.0-1.x86_64.rpm
# macOS
sudo installer -pkg lurk_1.0.0_macos.pkg -target /
# Windows: unzip and put lurk.exe on PATH
```

`lurk -version` prints the embedded version.

## Windows `.msi` and `.msix`

- **`.msi`** is built with [msitools](https://wiki.gnome.org/msitools)' `wixl`
  (cross-platform — runs on macOS/Linux, no Windows needed), from
  `packaging/windows/lurk.wxs` via `scripts/build-msi.sh`. `scripts/package.sh`
  builds it automatically when `wixl` is present (`brew install msitools` /
  `apt-get install wixl`). It installs `lurk.exe` to `Program Files\lurk` and
  appends that directory to the system `PATH`.
- **`.msix`** is built only in CI on a Windows runner (`makeappx`, from the
  Windows SDK) using `packaging/windows/AppxManifest.xml`, as a full-trust Win32
  package. See `.github/workflows/release.yml`.

Both are produced **unsigned**. Windows will not install an unsigned MSIX, and an
unsigned MSI trips SmartScreen — sign them with a code-signing certificate before
distribution (e.g. add a `signtool` step in CI).

## CI (GitHub Actions)

`.github/workflows/release.yml` builds the whole matrix on the appropriate
runners and publishes to the GitHub release:

- **Linux runner** — binaries, `.deb`, `.rpm`, Windows `.zip`, Windows `.msi`.
- **macOS runner** — universal binary + `.pkg`.
- **Windows runner** — `.msix` (best-effort; never blocks the release).

Trigger it by pushing a `v*` tag, or manually from the Actions tab / CLI:

```sh
gh workflow run release.yml -f version=1.0.0     # manual, attaches to v1.0.0
# or
git tag v0.2.0 && git push origin v0.2.0         # tag-driven
```

The release job (re)uploads every artifact with `--clobber` and regenerates a
combined `SHA256SUMS`.

## Cutting a release locally

```sh
scripts/package.sh 1.0.0
gh release create v1.0.0 dist/* --title "lurk 1.0.0" --notes "…"
```
