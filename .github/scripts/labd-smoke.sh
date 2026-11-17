#!/usr/bin/env bash
# A smoke test of the plain labd binary on an empty data directory, in
# Preview, as it runs on the mini: at its Lab address through the tunnel's
# Unix socket, where curl plays cloudflared, and at its loopback port, where
# curl is a caller on the same machine. The Lab address may carry a prefix,
# such as https://example.org/lab. Nothing leaves this machine.
#
#   .github/scripts/labd-smoke.sh <directory holding labd and lab-agent-linux-arm64> [Lab address]
set -euo pipefail

BIN=${1:?usage: labd-smoke.sh <bin directory> [public URL]}
PUB=${2:-https://lab.omarchy-mac.org}
HOST=${PUB#https://}
HOST=${HOST%%/*}
PREFIX=${PUB#https://"$HOST"}
[ -x "$BIN/labd" ] || { echo "no labd in $BIN" >&2; exit 1; }
[ -f "$BIN/lab-agent-linux-arm64" ] || { echo "no lab-agent-linux-arm64 in $BIN" >&2; exit 1; }

WORK=$(mktemp -d)
LOG=${RUNNER_TEMP:-$WORK}/labd-smoke.log
PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
ALIAS=alias.example
SOCK=$WORK/labd.sock
ROOT=http://$HOST                   # labd as cloudflared asks for it, on the socket
B=$ROOT$PREFIX
LOOP=http://127.0.0.1:$PORT$PREFIX  # labd as a caller on this machine asks for it
FWD=(--unix-socket "$SOCK" -H 'X-Forwarded-Proto: https')
PID=

fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
	[ -z "$PID" ] || kill "$PID" 2>/dev/null || true
	[ -z "$PID" ] || wait "$PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# Account 1 is the Admin the configuration names; nobody signs in here.
mkdir "$WORK/sign-in"
echo 1 >"$WORK/sign-in/admins"
"$BIN/labd" --listen="127.0.0.1:$PORT" --socket="$SOCK" --data="$WORK/data" --public-url="$PUB" \
	--alias="$ALIAS" --preview --sign-in="$WORK/sign-in" >"$LOG" 2>&1 &
PID=$!

c() { curl -sS --max-time 20 "$@"; }
code() { c -o /dev/null -w '%{http_code}' "$@"; }
head_of() { c -o /dev/null -D - "$@" | tr -d '\r'; }
sha256() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }
field() { python3 -c 'import json, sys; sys.stdout.write(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
mask() { [ -z "${GITHUB_ACTIONS:-}" ] || echo "::add-mask::$1"; }

for _ in $(seq 60); do
	[ "$(code "${FWD[@]}" "$B/healthz" 2>/dev/null || true)" = 200 ] && break
	kill -0 "$PID" 2>/dev/null || { cat "$LOG" >&2; fail "labd stopped while starting"; }
	sleep 1
done
[ "$(c "${FWD[@]}" "$B/healthz")" = ok ] || fail "healthz"

# No admin token on disk: the server's own command mints a Lab token.
[ ! -e "$WORK/data/admin.token" ] || fail "labd made an admin.token"
TOK=$("$BIN/labd" break-glass --data="$WORK/data" --account-id=1 ci 2>/dev/null) || fail "labd break-glass"
mask "$TOK"
AUTH=(-H "Authorization: Bearer $TOK")

# labd listens on this machine only: a socket for its own account, and loopback.
[ "$(python3 -c 'import os, stat, sys; print(oct(stat.S_IMODE(os.stat(sys.argv[1]).st_mode)))' "$SOCK")" = 0o600 ] || fail "the socket's mode"
"$BIN/labd" --listen="0.0.0.0:$PORT" --data="$WORK/other" >/dev/null 2>&1 && fail "labd listened beyond this machine"
[ "$(c "$LOOP/healthz")" = ok ] || fail "healthz at the loopback port"

# The interface, under the prefix: its pages, their files and headers.
page=$(c -D - "${FWD[@]}" "$B/")
grep -qF "<a href=\"$PREFIX/terms\">Terms</a>" <<<"$page" || fail "the footer's link to the terms"
grep -qF 'omarchy-m-testing.org' <<<"$page" || fail "Preview's banner"
grep -qi '^strict-transport-security: max-age=' <<<"$page" || fail "HSTS"
grep -qi "^content-security-policy: .*frame-ancestors 'none'" <<<"$page" || fail "frame-ancestors"
grep -qi '^x-content-type-options: nosniff' <<<"$page" || fail "nosniff"
grep -qi '^set-cookie:' <<<"$page" && fail "a signed-out page set a cookie"
css=$(grep -oE "$PREFIX/assets/app-[0-9a-f]{16}\.css" <<<"$page" | head -n 1)
[ -n "$css" ] || fail "the page's stylesheet has no fingerprint"
sheet=$(head_of "${FWD[@]}" -H 'Accept-Encoding: br' "$ROOT$css")
grep -qi '^cache-control: public, max-age=31536000, immutable$' <<<"$sheet" || fail "the stylesheet is not kept a year"
grep -qi '^content-encoding: br$' <<<"$sheet" || fail "the stylesheet is not sent as Brotli"
[ "$(c "${FWD[@]}" "$B/terms" | grep -c 'OMARCHY MAC LAB: LENDING A MAC')" = 1 ] || fail "/terms"
[ "$(c "${FWD[@]}" "$B/privacy" | grep -c 'privacy@omarchy-mac.org')" = 1 ] || fail "/privacy"
[ "$(code "${FWD[@]}" "$B/api/themes")" = 404 ] || fail "the old interface's themes"
grep -qi '^x-robots-tag: noindex' <<<"$page" || fail "noindex in Preview"
head_of "${FWD[@]}" "$B/healthz" | grep -qi '^cache-control: no-store$' || fail "no-store by default"

# The API: tokens only, under the prefix only.
[ "$(code "${FWD[@]}" "$B/api/devices")" = 401 ] || fail "the API without a token"
[ "$(c "${FWD[@]}" "${AUTH[@]}" "$B/api/devices")" = "[]" ] || fail "the API with the token"
if [ -n "$PREFIX" ]; then
	[ "$(code "${FWD[@]}" "${AUTH[@]}" "$ROOT/api/devices")" = 404 ] || fail "the API outside the prefix"
	[ "$(code "${FWD[@]}" "${AUTH[@]}" "${ROOT}${PREFIX}x/api/devices")" = 404 ] || fail "a sibling path"
	[ "$(code "${FWD[@]}" "${AUTH[@]}" "$B//api/devices")" = 404 ] || fail "a path the router would clean"
fi

# Exactly two names through the tunnel, and this machine's own at the loopback port.
[ "$(code "${FWD[@]}" "${AUTH[@]}" "http://evil.example$PREFIX/api/devices")" = 421 ] || fail "a third name"
[ "$(code --unix-socket "$SOCK" -H 'X-Forwarded-Proto: http' "${AUTH[@]}" "$B/api/devices")" = 421 ] || fail "plain http forwarded"
[ "$(code -H "Host: $HOST" "${AUTH[@]}" "$LOOP/api/devices")" = 421 ] || fail "the Lab address at the loopback port"
[ "$(c "${AUTH[@]}" "$LOOP/api/devices")" = "[]" ] || fail "the API at the loopback port"

# The Alias: pages are forwarded for good, the API is refused in one line.
fwd=$(head_of "${FWD[@]}" "http://$ALIAS$PREFIX/install?x=1")
grep -q '^HTTP/1.1 308' <<<"$fwd" || fail "the Alias's forward"
grep -qixF "location: $PUB/install?x=1" <<<"$fwd" || fail "the Alias's forward keeps path and query"
grep -qi '^set-cookie' <<<"$fwd" && fail "the Alias set a cookie"
[ "$(code "${FWD[@]}" "${AUTH[@]}" "http://$ALIAS$PREFIX/api/devices")" = 421 ] || fail "the API on the Alias"
[ "$(c "${FWD[@]}" "${AUTH[@]}" "http://$ALIAS$PREFIX/api/devices")" = "Omarchy Mac Lab is at $PUB: use that address" ] || fail "the Alias's line"

# Sign-in is off until the OAuth app's files are there, and a labd behind the
# tunnel never takes a stand-in for GitHub.
[ "$(code "${FWD[@]}" "$B/sign-in")" = 404 ] || fail "sign-in without an OAuth app"
head_of "${FWD[@]}" "$B/sign-in" | grep -qi '^set-cookie' && fail "a signed-out answer set a cookie"
"$BIN/labd" --listen="127.0.0.1:$PORT" --socket="$WORK/other.sock" --public-url="$PUB" --data="$WORK/other" \
	--sign-in="$WORK" --github-url=http://127.0.0.1:1 >/dev/null 2>&1 && fail "labd took a stand-in for GitHub behind the tunnel"

# In Preview the lab takes no reports, and says which site is the live one.
up=$(c -w '\n%{http_code}' -X POST "${FWD[@]}" -H 'Content-Type: application/json' -d '{}' "$B/api/v1/reports")
[ "${up##*$'\n'}" = 403 ] || fail "an upload in Preview was not refused"
grep -qF 'omarchy-m-testing.org is the live site' <<<"$up" || fail "the refusal of an upload in Preview"
[ "$(code "${FWD[@]}" "$B/reports/1111111111111111aaaaaaaa")" = 404 ] || fail "a report that is not there"

# Early data can be replayed.
[ "$(code "${FWD[@]}" "${AUTH[@]}" -H 'Early-Data: 1' "$B/api/devices")" = 425 ] || fail "early data under the API"

# A page from another origin can't change anything.
[ "$(code -X POST "${FWD[@]}" "${AUTH[@]}" -H 'Origin: https://evil.example' -d '{}' "$B/api/jobs")" = 403 ] || fail "a cross-origin POST"

# An invite: the join line, the terms and lab-agent, with the token in headers only.
inv=$(c -X POST "${FWD[@]}" "${AUTH[@]}" -H "Origin: https://$HOST" -d '{"name":"ci-mac"}' "$B/api/enroll-tokens")
ENR=$(field token <<<"$inv")
mask "$ENR"
cmd=$(field join_command <<<"$inv")
[ "$cmd" = " curl -fsSL $PUB/join | LAB_JOIN=$ENR bash" ] || fail "the join line"
join=$(c "${FWD[@]}" "$B/join")
grep -qF "u='$PUB'" <<<"$join" || fail "the join script's URL"
grep -qF -- '-H @-' <<<"$join" || fail "the join script's header on stdin"
grep -qF -- '--token-stdin' <<<"$join" || fail "the join script's token on stdin"
want=$(sha256 <"$BIN/lab-agent-linux-arm64")
grep -qF "echo '$want  '" <<<"$join" || fail "the join script's lab-agent sha256"
head_of "${FWD[@]}" "$B/join" | grep -qix 'cache-control: no-store' || fail "the join script may be kept"
got=$(printf 'Authorization: Bearer %s\n' "$ENR" | c "${FWD[@]}" -H @- "$B/api/join/lab-agent" | sha256)
[ "$got" = "$want" ] || fail "the downloaded lab-agent"
terms=$(printf 'Authorization: Bearer %s\n' "$ENR" | c "${FWD[@]}" -H @- "$B/api/join/terms")
[ "$(field text <<<"$terms" | sha256)" = "$(field sha256 <<<"$terms")" ] || fail "the terms' sha256"
[ "$(code "${FWD[@]}" "$B/api/join/terms?token=$ENR")" = 401 ] || fail "a token in the URL"

# The caller log: the edge's address is believed on the socket alone, and no
# token or query reaches it.
c "${FWD[@]}" -H 'CF-Connecting-IP: 203.0.113.7' -H 'CF-Ray: 8f1e2d3c4b5a6978-BNE' "$B/healthz" >/dev/null
c -H 'CF-Connecting-IP: 198.51.100.66' -H 'CF-Ray: forged' "$LOOP/healthz" >/dev/null
CALLERS=$(cat "$WORK"/data/callers/*.log)
grep -qF '"addr":"203.0.113.7","ray":"8f1e2d3c4b5a6978-BNE"' <<<"$CALLERS" || fail "the caller's address through the tunnel"
grep -qF '198.51.100.66' <<<"$CALLERS" && fail "a forged address at the loopback port was believed"
grep -qF '"who":"person:ci"' <<<"$CALLERS" || fail "the caller log names no person"
grep -qF "$TOK" <<<"$CALLERS" && fail "the caller log holds the Lab token"
grep -qF "$ENR" <<<"$CALLERS" && fail "the caller log holds the enrollment token"

# labd's log holds neither token.
grep -qF "$TOK" "$LOG" && fail "labd logged the Lab token"
grep -qF "$ENR" "$LOG" && fail "labd logged the enrollment token"
kill -0 "$PID" 2>/dev/null || fail "labd stopped"
echo "labd smoke at $PUB: PASS"
