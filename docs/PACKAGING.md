# Packaging & releases

Lurk ships as native packages plus plain binaries — for **both** the `lurk`
client and the `lurkd` bouncer daemon ([`LURKD.md`](LURKD.md)). Everything is
produced by a single script from a pure-Go (CGO-disabled) cross-compile, so no C
toolchain or Docker is needed.

On Linux the two ship as **separate packages** (`lurk` and `lurkd`), so a headless
server can install just the daemon without pulling in the terminal-UI client. On
macOS (`.pkg`), Windows (`.zip`, `.msi`, `.msix`), and as raw binaries, both are
included together.

## Build all artifacts

```sh
scripts/package.sh 1.0.0      # version is embedded via -ldflags main.version
```

Outputs land in `dist/` (git-ignored):

| Artifact | Platform | Built with |
|---|---|---|
| `lurk_<v>_{amd64,arm64}.deb`, `lurkd_<v>_{amd64,arm64}.deb` | Debian/Ubuntu | [nfpm](https://nfpm.goreleaser.com) |
| `lurk-<v>-1.{x86_64,aarch64}.rpm`, `lurkd-<v>-1.{x86_64,aarch64}.rpm` | Fedora/RHEL/openSUSE | nfpm |
| `lurk_<v>_macos.pkg` (contains `lurk` + `lurkd`) | macOS (universal x86_64+arm64) | `pkgbuild` + `lipo` |
| `lurk_<v>_windows_amd64.zip` (contains both `.exe`s) | Windows | `zip` |
| `lurk_<v>_<os>_<arch>`, `lurkd_<v>_<os>_<arch>` raw binaries + `darwin_universal` | all | `go build` |
| `SHA256SUMS` | — | `shasum` |

The Linux packages install to `/usr/bin/lurk` and `/usr/bin/lurkd` (separate
packages); the `.pkg` installs both to `/usr/local/bin/`.

### Tool prerequisites
- `go` (1.26+) — always.
- `nfpm` — for `.deb`/`.rpm` (`brew install nfpm` / `go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest`).
- `pkgbuild` + `lipo` — for `.pkg` (preinstalled on macOS).
- `zip` — for the Windows archive.

Missing tools are skipped with a notice; the script never hard-fails on them.

## Installing

```sh
# Debian/Ubuntu — client and/or daemon (separate packages)
sudo dpkg -i lurk_1.1.0_amd64.deb            # the client
sudo dpkg -i lurkd_1.1.0_amd64.deb           # the bouncer daemon
# Fedora/RHEL
sudo rpm -i lurk-1.1.0-1.x86_64.rpm lurkd-1.1.0-1.x86_64.rpm
# macOS — one .pkg installs both lurk and lurkd
sudo installer -pkg lurk_1.1.0_macos.pkg -target /
# Windows: unzip and put lurk.exe / lurkd.exe on PATH (or use the .msi)
```

`lurk -version` / `lurkd -version` print the embedded version.

## Windows `.msi` and `.msix`

- **`.msi`** is built with [msitools](https://wiki.gnome.org/msitools)' `wixl`
  (cross-platform — runs on macOS/Linux, no Windows needed), from
  `packaging/windows/lurk.wxs` via `scripts/build-msi.sh`. `scripts/package.sh`
  builds it automatically when `wixl` is present (`brew install msitools` /
  `apt-get install wixl`). It installs both `lurk.exe` and `lurkd.exe` to
  `Program Files\lurk` and appends that directory to the system `PATH`.
- **`.msix`** is built only in CI on a Windows runner (`makeappx`, from the
  Windows SDK) using `packaging/windows/AppxManifest.xml`, as a full-trust Win32
  package declaring both `lurk` and `lurkd` applications. See
  `.github/workflows/release.yml`.

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
