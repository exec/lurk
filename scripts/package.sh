#!/usr/bin/env bash
# Build lurk release artifacts into ./dist for the given version.
#
#   scripts/package.sh [version]   # default 0.1.0
#
# Produces, where the required tool is available:
#   - cross-compiled binaries (linux/darwin/windows, amd64/arm64)
#   - .deb and .rpm           (linux amd64 + arm64)   via nfpm
#   - .pkg                    (macOS universal)       via pkgbuild + lipo
#   - .zip                    (windows amd64)         via zip
#   - .msi                    (windows amd64)         via wixl, if msitools is installed
#   - SHA256SUMS over every artifact
#
# Pure-Go build (CGO disabled) so cross-compilation needs no C toolchain.
set -euo pipefail

VERSION="${1:-0.1.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
DIST="$ROOT/dist"
rm -rf "$DIST"
mkdir -p "$DIST"
LDFLAGS="-s -w -X main.version=${VERSION}"

# build OS ARCH [EXT] -> prints the output path (status goes to stderr so $() is clean).
build() {
	local os="$1" arch="$2" ext="${3:-}"
	local out="$DIST/lurk_${VERSION}_${os}_${arch}${ext}"
	echo "  build ${os}/${arch}" >&2
	GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
		go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/lurk
	echo "$out"
}

echo "==> Cross-compiling binaries (v${VERSION})"
LIN_AMD64=$(build linux amd64)
LIN_ARM64=$(build linux arm64)
DAR_AMD64=$(build darwin amd64)
DAR_ARM64=$(build darwin arm64)
WIN_AMD64=$(build windows amd64 .exe)

if command -v nfpm >/dev/null 2>&1; then
	echo "==> .deb / .rpm (nfpm)"
	# Render a concrete config per arch (sed instead of relying on nfpm's env
	# expansion, which doesn't reach the contents[].src glob in all versions).
	for arch in amd64 arm64; do
		case "$arch" in
		amd64) bin="$LIN_AMD64" ;;
		arm64) bin="$LIN_ARM64" ;;
		esac
		cfg="$(mktemp)"
		sed -e "s|\${PKG_ARCH}|$arch|g" \
			-e "s|\${PKG_VERSION}|$VERSION|g" \
			-e "s|\${PKG_BIN}|$bin|g" \
			nfpm.yaml >"$cfg"
		nfpm package -f "$cfg" -p deb -t "$DIST/"
		nfpm package -f "$cfg" -p rpm -t "$DIST/"
		rm -f "$cfg"
	done
else
	echo "==> skip .deb/.rpm (nfpm not installed)" >&2
fi

if command -v lipo >/dev/null 2>&1 && command -v pkgbuild >/dev/null 2>&1; then
	echo "==> macOS .pkg (universal)"
	UNI="$DIST/lurk_${VERSION}_darwin_universal"
	lipo -create -output "$UNI" "$DAR_AMD64" "$DAR_ARM64"
	rootfs="$(mktemp -d)"
	mkdir -p "$rootfs/usr/local/bin"
	cp "$UNI" "$rootfs/usr/local/bin/lurk"
	chmod 0755 "$rootfs/usr/local/bin/lurk"
	pkgbuild --root "$rootfs" \
		--identifier com.github.exec.lurk \
		--version "$VERSION" \
		--install-location / \
		"$DIST/lurk_${VERSION}_macos.pkg" >/dev/null
	rm -rf "$rootfs"
else
	echo "==> skip .pkg (lipo/pkgbuild unavailable — not on macOS?)" >&2
fi

echo "==> Windows .zip"
wstage="$(mktemp -d)"
cp "$WIN_AMD64" "$wstage/lurk.exe"
cp README.md LICENSE "$wstage/"
(cd "$wstage" && zip -q "$DIST/lurk_${VERSION}_windows_amd64.zip" lurk.exe README.md LICENSE)
rm -rf "$wstage"

# Windows .msi is intentionally not built here: a proper, code-signed MSI is best
# produced on a Windows CI runner with WiX (see docs/PACKAGING.md). The .zip above
# is the supported Windows artifact (works with Scoop/winget/manual install).
echo "==> Windows .msi: see docs/PACKAGING.md (built in CI with WiX, not locally)" >&2

echo "==> SHA256SUMS"
(cd "$DIST" && shasum -a 256 lurk_* >SHA256SUMS)

echo "==> Done. Artifacts in dist/:"
ls -1 "$DIST"
