#!/usr/bin/env bash
# Run Lurk's opt-in live tests against a real Ergo upstream, with lurkd in the
# middle. This script provisions everything the gated tests need and tears it
# all down again, so the whole live suite runs from one command:
#
#   scripts/live-test.sh
#
# What it does:
#   1. Builds (or reuses) an Ergo IRCd binary.
#   2. Generates a throwaway Ergo config + self-signed certs + datastore and
#      starts Ergo on a loopback plaintext port.
#   3. Builds lurkd, points it at that Ergo as one upstream network, and starts
#      it on a loopback TLS port (self-signed cert auto-generated).
#   4. Runs the LURK_TEST_SERVER / LURKD_TEST_ADDR-gated tests in ./client:
#        - TestLiveServer        (single client round-trip)
#        - TestLiveTwoClients    (two clients exchange messages)
#        - TestLiveLurkdFanout   (two clients bound via lurkd receive a fan-out)
#   5. Stops both daemons and removes the temp workdir (always, via trap).
#
# Everything binds to 127.0.0.1 on high ports, so it does not collide with a
# real local ircd and needs no network access beyond fetching Ergo once.
#
# Environment overrides:
#   ERGO_BIN    path to a prebuilt ergo binary (skips the build)
#   ERGO_REF    git ref to build Ergo from           (default: master)
#   ERGO_PORT   Ergo plaintext loopback port         (default: 16667)
#   LURKD_PORT  lurkd TLS loopback port              (default: 16698)
#   KEEP        set to 1 to keep the workdir + daemons running for inspection
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ERGO_REF="${ERGO_REF:-master}"
ERGO_PORT="${ERGO_PORT:-16667}"
LURKD_PORT="${LURKD_PORT:-16698}"
WORK="$(mktemp -d "${TMPDIR:-/tmp}/lurk-live.XXXXXX")"
ERGO_PID=""
LURKD_PID=""

log() { printf '\033[1;34m[live-test]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[live-test] error:\033[0m %s\n' "$*" >&2; exit 1; }

cleanup() {
	local code=$?
	if [[ "${KEEP:-}" == "1" ]]; then
		log "KEEP=1 — leaving Ergo (pid ${ERGO_PID:-?}) and lurkd (pid ${LURKD_PID:-?}) running; workdir $WORK"
		return
	fi
	[[ -n "$LURKD_PID" ]] && kill "$LURKD_PID" 2>/dev/null || true
	[[ -n "$ERGO_PID" ]] && kill "$ERGO_PID" 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "$WORK"
	[[ $code -eq 0 ]] && log "done — cleaned up" || log "exited with code $code — cleaned up"
}
trap cleanup EXIT

# wait_port HOST PORT TIMEOUT_SECS — poll a TCP port until it accepts.
wait_port() {
	local host="$1" port="$2" timeout="$3" i=0
	while (( i < timeout * 10 )); do
		if (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null; then exec 3>&- 3<&-; return 0; fi
		sleep 0.1; (( i++ )) || true
	done
	return 1
}

# ─── 1. Ergo binary ──────────────────────────────────────────────────────────
ERGO_SRC="$WORK/ergo-src"
if [[ -n "${ERGO_BIN:-}" ]]; then
	[[ -x "$ERGO_BIN" ]] || die "ERGO_BIN=$ERGO_BIN is not executable"
	# We still need Ergo's languages/ dir + default.yaml from source for config.
	log "using ERGO_BIN=$ERGO_BIN; cloning Ergo source ($ERGO_REF) for config templates"
	git clone --depth 1 --branch "$ERGO_REF" https://github.com/ergochat/ergo.git "$ERGO_SRC" >/dev/null 2>&1 \
		|| git clone --depth 1 https://github.com/ergochat/ergo.git "$ERGO_SRC" >/dev/null 2>&1 \
		|| die "could not clone Ergo source"
else
	log "building Ergo from $ERGO_REF (override with ERGO_BIN)"
	git clone --depth 1 --branch "$ERGO_REF" https://github.com/ergochat/ergo.git "$ERGO_SRC" >/dev/null 2>&1 \
		|| git clone --depth 1 https://github.com/ergochat/ergo.git "$ERGO_SRC" >/dev/null 2>&1 \
		|| die "could not clone Ergo (need network access on first run)"
	( cd "$ERGO_SRC" && GOTOOLCHAIN=auto go build -o ergo . ) || die "ergo build failed"
	ERGO_BIN="$ERGO_SRC/ergo"
fi

# ─── 2. Ergo config + certs + datastore ──────────────────────────────────────
ERGO_RUN="$WORK/ergo-run"
mkdir -p "$ERGO_RUN"
cp "$ERGO_SRC/default.yaml" "$ERGO_RUN/config.yaml"
# Loopback IPv4 only (the sandbox has no IPv6; also avoids binding public ifaces):
#   - drop the [::1] plaintext listener
#   - point the plaintext listener at our port
#   - bind the TLS listener to loopback on our port + 30
#   - resolve the languages dir to an absolute path so CWD does not matter
ERGO_TLS_PORT=$(( ERGO_PORT + 30 ))
sed -i.bak \
	-e '/"\[::1\]:6667":/d' \
	-e "s|\"127.0.0.1:6667\":|\"127.0.0.1:${ERGO_PORT}\":|" \
	-e "s|\":6697\":|\"127.0.0.1:${ERGO_TLS_PORT}\":|" \
	-e "s|^\( *path: \)languages\$|\1${ERGO_SRC}/languages|" \
	"$ERGO_RUN/config.yaml"

( cd "$ERGO_RUN" && "$ERGO_BIN" mkcerts --conf config.yaml >/dev/null 2>&1 ) || die "ergo mkcerts failed"
( cd "$ERGO_RUN" && "$ERGO_BIN" initdb  --conf config.yaml >/dev/null 2>&1 ) || die "ergo initdb failed"

log "starting Ergo on 127.0.0.1:${ERGO_PORT}"
( cd "$ERGO_RUN" && exec "$ERGO_BIN" run --conf config.yaml ) >"$ERGO_RUN/ergo.log" 2>&1 &
ERGO_PID=$!
wait_port 127.0.0.1 "$ERGO_PORT" 15 || { tail -20 "$ERGO_RUN/ergo.log"; die "Ergo did not open ${ERGO_PORT}"; }

# ─── 3. lurkd pointed at Ergo ────────────────────────────────────────────────
log "building lurkd"
go build -o "$WORK/lurkd" ./cmd/lurkd || die "lurkd build failed"

LURKD_RUN="$WORK/lurkd-run"
mkdir -p "$LURKD_RUN/backlog"
cat >"$LURKD_RUN/config.json" <<JSON
{
  "listen": { "addr": "127.0.0.1:${LURKD_PORT}" },
  "networks": [
    {
      "netid": 1,
      "name": "ErgoLocal",
      "addr": "127.0.0.1:${ERGO_PORT}",
      "tls": false,
      "identity": { "nick": "lurkbot", "user": "lurkbot", "realname": "Lurk Bot" },
      "channels": ["#fan"]
    }
  ]
}
JSON

log "starting lurkd on 127.0.0.1:${LURKD_PORT} (upstream → Ergo)"
LURKD_BACKLOG_DIR="$LURKD_RUN/backlog" LURKD_CONFIG="$LURKD_RUN/config.json" \
	"$WORK/lurkd" -config "$LURKD_RUN/config.json" >"$LURKD_RUN/lurkd.log" 2>&1 &
LURKD_PID=$!
wait_port 127.0.0.1 "$LURKD_PORT" 15 || { tail -20 "$LURKD_RUN/lurkd.log"; die "lurkd did not open ${LURKD_PORT}"; }
# Give lurkd's upstream a moment to register + auto-join #fan on Ergo.
sleep 2

# ─── 4. Run the gated live tests ─────────────────────────────────────────────
log "running live tests"
set +e
LURK_TEST_SERVER="127.0.0.1:${ERGO_PORT}" \
LURKD_TEST_ADDR="127.0.0.1:${LURKD_PORT}" \
LURKD_TEST_NETID=1 \
LURKD_TEST_CHANNEL='#fan' \
LURK_TEST_CHANNEL='#lurk' \
	go test ./client/ -run 'TestLive' -v -count=1
RESULT=$?
set -e

if [[ $RESULT -ne 0 ]]; then
	log "tests FAILED — Ergo log tail:";  tail -20 "$ERGO_RUN/ergo.log"  || true
	log "lurkd log tail:";                tail -20 "$LURKD_RUN/lurkd.log" || true
fi
exit $RESULT
