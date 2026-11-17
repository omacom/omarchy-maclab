# What the Front door's stream and edge tests share beyond lib.sh (tests 1,
# 2, 6 to 13 of spec section 16, Ready item 1). Sourced after lib.sh.
#
# Under --local these tests start labd with its tunnel socket and put a
# stand-in for the edge and the tunnel in front of it (tools/front-door-probe), since
# what they test is true only there: labd streams every held response,
# believes CF-Connecting-IP and counts refused credentials on the socket
# alone. lab and the simulated Mac reach labd through the stand-in.
# shellcheck shell=bash disable=SC2034

# A name no resolver knows, and two addresses kept for documents (RFC 5737):
# the one the stand-in gives every caller, and the one a caller forges.
EDGE_HOST="lab.front-door.test"
STAND_IN_ADDRESS="198.51.100.7"
FORGED_ADDRESS="203.0.113.77"
# The edge cuts an answer that is silent for 125 s; the stand-in does too.
EDGE_SILENCE="125s"
TUNNEL_LABEL="org.omarchy-mac.lab.cloudflared"
# For the tests of these tests: a way the stand-in misbehaves (tools/front-door-probe tunnel).
FAULT="${FRONT_DOOR_FAULT:-}"

PROBE=""
TUNNEL_PID=""
SOCKET_DIR=""
LEASED=0
STATUS=000
CURL_MORE=()

edge_cleanup() {
  local pids
  pids="$(jobs -p)"
  if [[ -n $pids ]]; then
    for pid in $pids; do
      [[ $pid == "$LABD_PID" || $pid == "$SIM_PID" || $pid == "$TUNNEL_PID" ]] || kill "$pid" 2>/dev/null || true
    done
  fi
  if (( LEASED )); then
    "$LAB_CMD" release "$MAC" >/dev/null 2>&1 || echo "$ME: the Lease on $MAC was not released: run $LAB_CMD release $MAC" >&2
  fi
  [[ -z $TUNNEL_PID ]] || kill "$TUNNEL_PID" 2>/dev/null || true
  [[ -z $SOCKET_DIR ]] || rm -rf "$SOCKET_DIR"
  cleanup
}

# edge_workspace: the run's own folder, and the probe: the one in --bin, or
# built here from this checkout.
edge_workspace() {
  workspace
  trap edge_cleanup EXIT
  if [[ -x $BIN/front-door-probe ]]; then
    PROBE="$BIN/front-door-probe"
    return 0
  fi
  command -v go >/dev/null 2>&1 || fail "no front-door-probe in $BIN and no go to build it with: run this from a checkout on a machine with Go"
  PROBE="$WORK/front-door-probe"
  (cd "$REPO" && go build -o "$PROBE" ./tools/front-door-probe) || fail "the probe did not build"
}

# edge_lab [SILENCE]: a labd on this machine with its tunnel socket and an
# empty data folder, the stand-in in front of it, and a login made here and
# kept in the run's own folder. HOME moves there, so the login of whoever
# runs this is neither read nor written. ADDRESS is the stand-in's.
edge_lab() {
  local silence="${1:-$EDGE_SILENCE}" tool n port at
  for tool in labd lab lab-agent; do
    [[ -x $BIN/$tool ]] || fail "no $tool in $BIN: run make all, or name the folder with --bin"
  done
  LAB_CMD="$BIN/lab"
  export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XDG_STATE_HOME="$WORK/home/.local/state"
  export XDG_DATA_HOME="$WORK/home/.local/share" XDG_CACHE_HOME="$WORK/home/.cache" TMPDIR="$WORK/tmp"
  LOCAL_ROOT="$WORK/root"
  LOCAL_DATA="$LOCAL_ROOT/var/lab/data"
  mkdir -p "$HOME" "$TMPDIR" "$WORK/sign-in" "$LOCAL_ROOT/var/lab"
  # A socket's path is short: its folder is outside the run's own.
  SOCKET_DIR="$(mktemp -d /tmp/fd-XXXXXX)"
  LOCAL_SOCKET="$SOCKET_DIR/labd.sock"
  echo 1 > "$WORK/sign-in/admins"
  # Each log is made before its program starts: a background job opens its
  # redirect only once it runs, and the first read below can come before.
  : > "$WORK/labd.log"
  "$BIN/labd" --listen=127.0.0.1:0 --socket="$LOCAL_SOCKET" --public-url="https://$EDGE_HOST" --data="$LOCAL_DATA" \
    --sign-in="$WORK/sign-in" --omt=false >> "$WORK/labd.log" 2>&1 &
  LABD_PID=$!
  port=""
  for n in $(seq 100); do
    port="$(sed -n 's/.*labd listening on http:\/\/127\.0\.0\.1:\([0-9]*\).*/\1/p' "$WORK/labd.log" | head -n 1)"
    [[ -z $port ]] || break
    kill -0 "$LABD_PID" 2>/dev/null || fail "labd stopped while starting: $(tail -n 3 "$WORK/labd.log")"
    sleep 0.1
  done
  [[ -n $port ]] || fail "labd did not say where it listens"
  LOCAL_LISTEN="127.0.0.1:$port"
  : > "$WORK/tunnel.log"
  "$PROBE" tunnel --socket "$LOCAL_SOCKET" --host "$EDGE_HOST" --client "$STAND_IN_ADDRESS" --silence "$silence" --down 2s \
    ${FAULT:+--fault "$FAULT"} >> "$WORK/tunnel.log" 2>&1 &
  TUNNEL_PID=$!
  at=""
  for n in $(seq 100); do
    at="$(sed -n 's/^listening=//p' "$WORK/tunnel.log" | head -n 1)"
    [[ -z $at ]] || break
    kill -0 "$TUNNEL_PID" 2>/dev/null || fail "the stand-in for the tunnel stopped while starting: $(tail -n 3 "$WORK/tunnel.log")"
    sleep 0.1
  done
  [[ -n $at ]] || fail "the stand-in for the tunnel did not say where it listens"
  ADDRESS="http://$at"
  "$BIN/labd" break-glass --data="$LOCAL_DATA" --account-id=1 front-door 2>/dev/null | "$LAB_CMD" login "$ADDRESS" > /dev/null \
    || fail "no login at the labd started here"
}

# edge_mac [DELAY]: a simulated Mac under the name the test asks for, with
# its baseline passed. DELAY is how long each of its reboots takes.
edge_mac() {
  local delay="${1:-100ms}" token n
  token="$("$LAB_CMD" enroll "$MAC" --model desktop 2>/dev/null | grep -oE 'enr_[0-9a-f]+' | head -n 1)"
  [[ -n $token ]] || fail "the labd started here made no Join command"
  "$BIN/lab-agent" sim --server "$ADDRESS" --token "$token" --reboot-delay "$delay" > "$WORK/sim.log" 2>&1 &
  SIM_PID=$!
  for n in $(seq 300); do
    "$LAB_CMD" status "$MAC" 2>/dev/null | grep -q "$MAC" && break
    kill -0 "$SIM_PID" 2>/dev/null || fail "the simulated Mac stopped while joining: $(plain "$WORK/sim.log")"
    sleep 0.1
  done
  "$LAB_CMD" baseline "$MAC" > "$WORK/baseline.log" 2>&1 || fail "the simulated Mac's baseline: $(plain "$WORK/baseline.log")"
}

# own_lease: the Lease lab exec and a job need, in the name of whoever
# runs this. It is given back when the run ends, however it ends.
own_lease() {
  "$LAB_CMD" lease "$MAC" > "$WORK/lease.out" 2> "$WORK/lease.err" || fail "no Lease on $MAC: $(plain "$WORK/lease.err")"
  LEASED=1
}

# give_back: the Lease is released now and not at the run's end.
give_back() {
  "$LAB_CMD" release "$MAC" > /dev/null 2>&1 || return 0
  LEASED=0
}

# fact FILE KEY: one value of the probe's or the mini's key=value lines.
fact() { sed -n "s/^$2=//p" "$1" | head -n 1; }

# nonce: a few random letters, to tell this run's requests from any other.
nonce() { head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# ask NAME CURL-ARGS...: one request with curl. Its status is left in
# STATUS, its headers and body in the run's folder; nothing is printed.
ask() {
  local name="$1"
  shift
  STATUS="$(curl -s -o "$WORK/$name.body" -D "$WORK/$name.headers" -w '%{http_code}' --max-time 60 ${CURL_MORE[@]+"${CURL_MORE[@]}"} "$@" 2>/dev/null)" || true
  [[ $STATUS =~ ^[0-9]{3}$ ]] || STATUS=000
}

# header NAME HEADER: what the answer to ask NAME said in HEADER.
header() {
  [[ -f $WORK/$1.headers ]] || return 0
  awk -v want="$(tr '[:upper:]' '[:lower:]' <<<"$2")" '
    { line = $0; sub(/\r$/, "", line); i = index(line, ":"); if (!i) next
      if (tolower(substr(line, 1, i - 1)) == want) { v = substr(line, i + 1); sub(/^[ \t]+/, "", v); last = v } }
    END { print last }' "$WORK/$1.headers"
}

# labd_answer NAME: whether the answer to ask NAME is labd's own. Every
# answer of labd's says no-referrer and that no page may frame it; a page
# the edge makes says neither.
labd_answer() {
  [[ "$(header "$1" referrer-policy)" == no-referrer && "$(header "$1" content-security-policy)" == *"frame-ancestors 'none'"* ]]
}

# edge_page NAME: whether the answer to ask NAME is the edge's own and not
# labd's: a challenge, or an error that labd did not write.
edge_page() {
  [[ -n "$(header "$1" cf-mitigated)" ]] && return 0
  (( 10#$STATUS >= 400 )) && ! labd_answer "$1"
}

# where NAME: an answer of the edge's as it may be shown: status and ray id.
where() {
  local ray
  ray="$(header "$1" cf-ray | tr -cd '0-9A-Za-z-' | cut -c1-40)"
  echo "HTTP $STATUS, ray ${ray:-unknown}"
}

# cache_status NAME: what the edge said it did with the answer.
cache_status() { header "$1" cf-cache-status | tr '[:lower:]' '[:upper:]'; }

# mini_facts OUT COMMAND...: one or more runs of scripts/mini/edge-facts on
# the mini, in one SSH session so that sudo asks once. Each COMMAND is the
# script's arguments. Only its key=value lines are kept, in OUT. Under
# --local the script reads the labd started here.
mini_facts() {
  local out="$1" line="" part
  shift
  if [[ $MODE == local ]]; then
    : > "$out"
    for part in "$@"; do
      # shellcheck disable=SC2086
      LAB_SYSROOT="$LOCAL_ROOT" "$REPO/scripts/mini/edge-facts" $part >> "$out" || fail "edge-facts $part failed here"
    done
    return 0
  fi
  [[ -n $MINI ]] || fail "--lab needs --mini USER@HOST: the mini, as the owner reaches it over SSH with a key"
  for part in "$@"; do
    line="${line:+$line && }sudo $MINI_SCRIPTS/edge-facts $part"
  done
  say "asking the mini (sudo asks for the password there)"
  # sudo's prompt comes down the same pipe as the facts: it is shown, and
  # only the facts are kept.
  # shellcheck disable=SC2029
  ssh -t "$MINI" "$line" | tee /dev/stderr | tr -d '\r' | grep -E '^[a-z0-9_]+=[0-9A-Za-z_.,:-]*$' > "$out" \
    || fail "the mini gave no facts: is the Deployed build one that has scripts/mini/edge-facts?"
}

# restart_tunnel: the tunnel goes away and comes back, as cloudflared does
# when launchd restarts it.
restart_tunnel() {
  if [[ $MODE == local ]]; then
    kill -HUP "$TUNNEL_PID"
    return 0
  fi
  say "restarting cloudflared on the mini (sudo asks for the password there)"
  ssh -t "$MINI" sudo launchctl kickstart -k "system/$TUNNEL_LABEL" || fail "the mini did not restart the tunnel"
}

# sha256_text TEXT
sha256_text() {
  if command -v sha256sum >/dev/null 2>&1; then printf '%s' "$1" | sha256sum | cut -d' ' -f1; else printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1; fi
}

# failed LINE / stopped LINE: the record's last line and the run's end.
# Stopped (status 3) is for the owner: nothing was wrong with the lab, and
# the test could not be carried through.
failed() {
  [[ $1 == FAIL:* ]] || set -- "FAIL: $*"
  rec "$*"
  record_done fail
  exit 1
}

stopped() {
  rec "$*"
  record_done stopped
  exit 3
}
