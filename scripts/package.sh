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

# build CMD OS ARCH [EXT] -> prints the output path (status goes to stderr so $() is clean).
build() {
	local cmd="$1" os="$2" arch="$3" ext="${4:-}"
	local out="$DIST/${cmd}_${VERSION}_${os}_${arch}${ext}"
	echo "  build ${cmd} ${os}/${arch}" >&2
	GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
		go build -trimpath -ldflags "$LDFLAGS" -o "$out" "./cmd/${cmd}"
	echo "$out"
}

echo "==> Cross-compiling binaries (v${VERSION})"
LIN_AMD64=$(build lurk linux amd64)
build lurkd linux amd64 >/dev/null
LIN_ARM64=$(build lurk linux arm64)
build lurkd linux arm64 >/dev/null
DAR_AMD64=$(build lurk darwin amd64)
build lurkd darwin amd64 >/dev/null
DAR_ARM64=$(build lurk darwin arm64)
build lurkd darwin arm64 >/dev/null
WIN_AMD64=$(build lurk windows amd64 .exe)
build lurkd windows amd64 .exe >/dev/null

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
	UNI_D="$DIST/lurkd_${VERSION}_darwin_universal"
	lipo -create -output "$UNI_D" \
		"$DIST/lurkd_${VERSION}_darwin_amd64" "$DIST/lurkd_${VERSION}_darwin_arm64"
	rootfs="$(mktemp -d)"
	mkdir -p "$rootfs/usr/local/bin"
	cp "$UNI" "$rootfs/usr/local/bin/lurk"
	cp "$UNI_D" "$rootfs/usr/local/bin/lurkd"
	chmod 0755 "$rootfs/usr/local/bin/lurk" "$rootfs/usr/local/bin/lurkd"
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
cp "$DIST/lurkd_${VERSION}_windows_amd64.exe" "$wstage/lurkd.exe"
cp README.md LICENSE "$wstage/"
(cd "$wstage" && zip -q "$DIST/lurk_${VERSION}_windows_amd64.zip" lurk.exe lurkd.exe README.md LICENSE)
rm -rf "$wstage"

if command -v wixl >/dev/null 2>&1; then
	echo "==> Windows .msi (wixl)"
	"$ROOT/scripts/build-msi.sh" "$VERSION" "$WIN_AMD64" "$DIST" >/dev/null
else
	echo "==> skip .msi (wixl not installed — brew install msitools / apt-get install wixl)" >&2
fi

# Windows .msix is built only in CI on a Windows runner (makeappx, from the
# Windows SDK). See .github/workflows/release.yml and docs/PACKAGING.md.

echo "==> SHA256SUMS"
(cd "$DIST" && shasum -a 256 lurk_* lurkd_* 2>/dev/null | sort >SHA256SUMS)

echo "==> Done. Artifacts in dist/:"
ls -1 "$DIST"
