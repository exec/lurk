#!/usr/bin/env bash
# Build the lurk Windows .msi from built lurk.exe + lurkd.exe using msitools'
# wixl (cross-platform — runs on macOS/Linux, no Windows needed). Used by
# scripts/package.sh and by CI. The installer ships both binaries.
#
#   scripts/build-msi.sh <version> <lurk-exe-path> <lurkd-exe-path> [out-dir]
#
# Prints the resulting .msi path. Requires `wixl` (brew install msitools /
# apt-get install wixl).
set -euo pipefail
VERSION="${1:?version required}"
EXE="${2:?path to lurk.exe required}"
LURKD_EXE="${3:?path to lurkd.exe required}"
OUT="${4:-dist}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out="$OUT/lurk_${VERSION}_windows_amd64.msi"
wixl -D Version="$VERSION" -D ExeSource="$EXE" -D LurkdExeSource="$LURKD_EXE" --arch x64 \
	-o "$out" "$ROOT/packaging/windows/lurk.wxs"
echo "$out"
