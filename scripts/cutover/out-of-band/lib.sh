# What the Out-of-band wiring's eight tests share (spec section 16, Ready
# item 2; section 7). Sourced by the scripts beside it; needs bash, curl and
# the lab command. It keeps to the bash macOS has (3.2).
#
# Every test runs three ways:
#
#   (no option), --dry-run   says each step and what it expects; sends
#                            nothing, resets nothing, needs no login
#   --local                  against a labd it starts on this machine's
#                            loopback, with a made-up mini (tools/fake-mini):
#                            the controller's own code over made-up ports,
#                            and simulated Macs on its cables. It proves the
#                            script and the lab's rules, not the hardware
#   --lab                    through the Lab address, under the saved login
#                            of whoever runs it (a Maintainer), with the mini
#                            reached over SSH (--mini USER@HOST). It resets
#                            real Macs. Its dated output goes to
#                            docs/acceptance/
#
# Where a cable or the owner's hand is needed the test stops, says what to
# do and waits for Enter. With nobody there to answer it ends as "stopped"
# (exit 3), which is no failure of the script.
#
# Each run through the Lab address writes its record,
# docs/acceptance/YYYY-MM-DD-out-of-band-NN.md, ending in "Result: pass",
# "Result: fail" or "Result: stopped", and its line in
# docs/acceptance/out-of-band.results: "NN RESULT DATE RECORD". The Ready
# list reads that file: `run --results` is green when all eight say pass.
#
# A test never holds a token: lab reads the saved login by itself. Nothing
# lab or curl answers is printed raw: an error page becomes its status and
# ray id.
# shellcheck shell=bash disable=SC2034

OOB_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$OOB_HERE/../../.." && pwd)"
ME="$(basename "$0")"

ADDRESS="${LAB_ADDRESS:-https://lab.omarchy-mac.org}"
LAB_CMD="${LAB_COMMAND:-lab}"
MODE=dry
M1=m1-pro
M2=m2-max
M1_PORT=""
M2_PORT=""
MINI=""
RECORDS=""
WITHIN=""
BIN="$REPO/bin"
REST=()

MINI_SCRIPTS=/usr/local/lab/current/scripts/mini
MINI_TOOL=/usr/local/lab/current/bin/macvdmtool
OOBD_LABEL=com.maclab.oobd
OOBD_PLIST=/Library/LaunchDaemons/com.maclab.oobd.plist
LABD_LABEL=org.omarchy-mac.lab.labd
OOBD_LOG=/var/log/oobd.log
RESULTS_NAME=out-of-band.results

WORK=""
LABD_PID=""
FAKE_PID=""
FAKE=""
RECORD=""
NUMBER=""

say() { echo "$ME: $*"; }
fail() { echo "$ME: $*" >&2; exit 1; }

# one_mode FLAG MODE: the run's mode. A second, other mode is refused before
# anything is done: which one was meant is not guessed.
MODE_FLAG=""
one_mode() {
  [[ -z $MODE_FLAG || $MODE_FLAG == "$1" ]] || fail "$MODE_FLAG and $1 are two modes: give one. Nothing was done."
  MODE_FLAG="$1"
  MODE="$2"
}

# oob_options ARGS...: the options every test takes. What it does not know
# stays in REST for the test itself.
oob_options() {
  while (( $# )); do
    case "$1" in
      --dry-run) one_mode "$1" dry ;;
      --local) one_mode "$1" local ;;
      --lab) one_mode "$1" lab ;;
      --mini) (( $# >= 2 )) || fail "--mini needs USER@HOST"; MINI="$2"; shift ;;
      --m1) (( $# >= 2 )) || fail "--m1 needs a Mac's name"; M1="$2"; shift ;;
      --m2) (( $# >= 2 )) || fail "--m2 needs a Mac's name"; M2="$2"; shift ;;
      --m1-port) (( $# >= 2 )) || fail "--m1-port needs left, middle or right"; M1_PORT="$2"; shift ;;
      --m2-port) (( $# >= 2 )) || fail "--m2-port needs left, middle or right"; M2_PORT="$2"; shift ;;
      --within) (( $# >= 2 )) || fail "--within needs a number of seconds"; WITHIN="$2"; shift ;;
      --records) (( $# >= 2 )) || fail "--records needs a folder"; RECORDS="$2"; shift ;;
      --bin) (( $# >= 2 )) || fail "--bin needs a folder"; BIN="$2"; shift ;;
      -h|--help) usage ;;
      *) REST+=("$1") ;;
    esac
    shift
  done
  local mac port
  for mac in "$M1" "$M2"; do
    [[ $mac =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || fail "$mac is not a Mac's name"
  done
  [[ $M1 != "$M2" ]] || fail "--m1 and --m2 name the same Mac"
  for port in "$M1_PORT" "$M2_PORT"; do
    [[ -z $port || $port == left || $port == middle || $port == right ]] || fail "$port is not left, middle or right"
  done
  [[ -z $WITHIN || $WITHIN =~ ^[1-9][0-9]*$ ]] || fail "--within $WITHIN is not a number of seconds"
  [[ -z $MINI || $MINI =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+$ ]] || fail "--mini $MINI is not USER@HOST"
  if [[ -z $WITHIN ]]; then
    if [[ $MODE == local ]]; then WITHIN=60; else WITHIN=300; fi
  fi
  # A login for another lab in the environment would send the run there.
  unset MACLAB_URL MACLAB_TOKEN
  export MACLAB_HOLDER="out-of-band-tests"
}

# no_rest: for a test that takes no option of its own.
no_rest() {
  (( ${#REST[@]} == 0 )) || fail "an option it does not take: ${REST[0]}. Nothing was done."
}

playing() { [[ $MODE == local ]]; }
today() { TZ=Australia/Brisbane date +%Y-%m-%d; }
# each: how long to wait between two looks at the lab.
each() { if playing; then echo 0.3; else echo 3; fi; }

# plain FILE: what lab or curl said, fit to print: an error page is never
# shown, only its status and ray id.
plain() {
  [[ -s $1 ]] || return 0
  if grep -qiE '<!doctype|<html|<body' "$1"; then
    local status ray
    status="$(grep -oE '(Error|error code:?) ?[0-9]{3,4}|[^0-9][45][0-9]{2}:' "$1" | grep -oE '[0-9]{3,4}' | head -n 1)"
    ray="$(grep -oiE 'ray id:? *(<[^>]*>)* *[0-9a-f]{16}' "$1" | grep -oE '[0-9a-f]{16}' | head -n 1)"
    echo "an error page (status ${status:-unknown}, ray id ${ray:-unknown})"
  else
    head -n 6 "$1" | tr '\n' ' ' | sed 's/ *$//'
  fi
}

# AT_END: what puts back a change the test made on the mini, run once before
# the record's result however the run ends: a failure, a stop, Ctrl-C or a
# dropped connection. A test sets it before the change and empties it once
# it has put the change back itself.
AT_END=""
at_end() {
  local what="$AT_END"
  [[ -n $what ]] || return 0
  AT_END=""
  "$what" || true
}

cleanup() {
  [[ -z $RECORD || ! -f $RECORD ]] || at_end
  [[ -z $LABD_PID ]] || kill "$LABD_PID" 2>/dev/null || true
  [[ -z $FAKE_PID ]] || kill "$FAKE_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  [[ -z $WORK ]] || rm -rf "$WORK"
}

# workspace: a folder of the run's own, gone when the run ends.
workspace() {
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/out-of-band.XXXXXX")"
  trap cleanup EXIT
  trap 'exit 129' HUP
  trap 'exit 130' INT TERM
}

# begin NN TITLE: the start of a run that is no dry run.
begin() {
  workspace
  if playing; then
    local_lab
  else
    [[ -n $MINI ]] || fail "--lab needs --mini USER@HOST: the mini's ports are read over SSH. Nothing was done."
    logged_in
  fi
  record_start "$1" "$2"
}

# The record.

record_start() {
  local title="$2" dir="$RECORDS"
  NUMBER="$1"
  if [[ -z $dir ]]; then
    if [[ $MODE == lab ]]; then dir="$REPO/docs/acceptance"; else dir="$WORK/records"; fi
  fi
  mkdir -p "$dir"
  RECORD="$dir/$(today)-out-of-band-$NUMBER.md"
  {
    echo "# Out-of-band wiring's test ${NUMBER#0}: $title"
    echo
    if [[ $MODE == lab ]]; then
      echo "Run $(TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M') (Brisbane) by \`scripts/cutover/out-of-band/$ME --lab\`, through $ADDRESS, the mini's ports read on the mini itself."
    else
      echo "Run $(TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M') (Brisbane) by \`scripts/cutover/out-of-band/$ME --local\`, against a labd on this machine's loopback with a made-up mini and simulated Macs. It proves the script and the lab's rules, not the hardware."
    fi
    echo
  } > "$RECORD"
}

# rec LINE: one line of the record, shown as it is written.
rec() {
  echo "- $*" >> "$RECORD"
  say "$*"
}

# record_done WORD: pass, fail or stopped. The results file gets the test's
# line, in place of the one it had.
record_done() {
  local results kept
  { echo; echo "Result: $1"; } >> "$RECORD"
  results="$(dirname "$RECORD")/$RESULTS_NAME"
  kept="$(grep -v "^$NUMBER " "$results" 2>/dev/null || true)"
  { [[ -z $kept ]] || echo "$kept"; echo "$NUMBER $1 $(today) $(basename "$RECORD")"; } | sort > "$results.new"
  mv "$results.new" "$results"
  if [[ $MODE == lab || -n $RECORDS ]]; then
    say "$1. The record is $RECORD"
  else
    say "$1"
  fi
}

passed() { record_done pass; exit 0; }
give_up() { rec "FAILED: $1"; at_end; record_done fail; exit 1; }
# stopped WHY: the run ends for the owner; exit 3 is no failure of the script.
stopped() { rec "STOPPED: $1"; at_end; record_done stopped; exit 3; }

# hands TEXT: a step only a person at the machines can do. Through the Lab
# address the run waits for Enter; against the made-up mini the caller plays
# it right after (see playing).
hands() {
  if playing; then
    say "by hand (played here): $1"
    return 0
  fi
  echo
  echo "$ME: STOP, by hand: $1"
  printf '%s: press Enter when it is done: ' "$ME"
  local line
  IFS= read -r line || stopped "nobody was there for a step by hand: $1"
  echo
}

# confirm QUESTION: what only a person can see. "yes" goes on.
confirm() {
  local line
  echo
  printf '%s: %s Type yes or no: ' "$ME" "$1"
  IFS= read -r line || stopped "nobody was there to answer: $1"
  [[ $line == yes ]]
}

# The lab, asked through lab.

# logged_in: the saved login is for the address the test runs through. lab
# itself says so: given the address alone, it refuses to send a saved Lab
# token anywhere else.
logged_in() {
  command -v "$LAB_CMD" >/dev/null 2>&1 || fail "no lab command: install it with curl -fsSL $ADDRESS/install/lab | bash, then lab login $ADDRESS"
  MACLAB_URL="$ADDRESS" "$LAB_CMD" status > "$WORK/status.out" 2> "$WORK/status.err" \
    || fail "lab is not logged in to Omarchy Mac Lab at $ADDRESS: $(plain "$WORK/status.err")"
}

# boot_of MAC: the boot id the lab has for it.
boot_of() { "$LAB_CMD" status "$1" 2>/dev/null | sed -n 's/^  boot: *//p' | head -n 1; }
# state_of MAC: READY, BUSY, OFFLINE and so on.
state_of() { "$LAB_CMD" status "$1" 2>/dev/null | awk 'NR == 1 { print $2 }'; }
# cable_of MAC: "sighted on the right port", "not sighted", "no cable".
cable_of() { "$LAB_CMD" oob "$1" 2>/dev/null | sed -n "1s/^$1: //p"; }
# since_of MAC: since when the lab holds its Sighting; empty for none.
since_of() { "$LAB_CMD" oob "$1" 2>/dev/null | sed -n 's/^  since: *//p'; }
# port_of MAC: the port it is placed on; empty for none.
port_of() { cable_of "$1" | sed -n 's/^sighted on the \([a-z]*\) port.*/\1/p'; }
# shows PORT: what lab oob says the port shows, as the controller last saw it.
shows() { "$LAB_CMD" oob "$M1" 2>/dev/null | sed -n "s/^  $1: *//p" | head -n 1; }

# reset ARGS...: lab reset. Leaves RESET_CODE and what lab said in reset.out.
reset() {
  RESET_CODE=0
  "$LAB_CMD" reset "$@" > "$WORK/reset.out" 2>&1 || RESET_CODE=$?
}
said() { grep -qF -- "$1" "$WORK/reset.out"; }

# wait_until SECONDS COMMAND...: true once the command is.
wait_until() {
  local end=$(( $(date +%s) + $1 ))
  shift
  while ! "$@"; do
    (( $(date +%s) < end )) || return 1
    sleep "$(each)"
  done
}

is_back() { local b; b="$(boot_of "$1")"; [[ -n $b && $b != "$2" && "$(state_of "$1")" == READY ]]; }
is_placed() { [[ "$(port_of "$1")" == "${2:-$(port_of "$1")}" && -n "$(port_of "$1")" ]]; }
is_unplaced() { [[ -z "$(port_of "$1")" ]]; }
by_name() { [[ "$(cable_of "$1")" == "sighted on the $2 port" ]]; }
# resighted MAC PORT PLUGS: sighted by name on a port the lab has looked at
# since its plug count became PLUGS.
resighted() { by_name "$1" "$2" && [[ "$(shows "$2")" == *"plug count $3,"* ]]; }

# wait_back MAC OLD_BOOT: the Mac is back on another boot and ready. Leaves
# BACK_SECONDS, counted from T0.
wait_back() {
  wait_until "$WITHIN" is_back "$1" "$2" || give_up "$1 did not come back on a new boot within $WITHIN s of its reset; lab status says: $("$LAB_CMD" status "$1" 2>&1 | head -n 1)"
  BACK_SECONDS=$(( $(date +%s) - T0 ))
}

# hard_reset MAC WHY: one Hard reset that must go out, and the Mac's return.
hard_reset() {
  local mac="$1" boot
  boot="$(boot_of "$mac")"
  [[ -n $boot ]] || give_up "the lab has no boot id for $mac: is it up?"
  T0="$(date +%s)"
  reset "$mac"
  (( RESET_CODE == 0 )) || give_up "$2: the Hard reset of $mac was refused: $(plain "$WORK/reset.out")"
  wait_back "$mac" "$boot"
  rec "$2: $(head -n 1 "$WORK/reset.out" | sed 's/;.*//'); back on a new boot and ready $BACK_SECONDS s after the reset was asked"
}

# place MAC PORT: has the lab place the Mac, by its Sighting when it shows its
# name, by a Proving reset of PORT when it shows none.
place() {
  local mac="$1" port="$2" boot
  if wait_until "$(if playing; then echo 25; else echo 90; fi)" by_name "$mac" "$port"; then
    rec "$mac is sighted on the $port port by the name it shows"
    return 0
  fi
  [[ "$(shows "$port")" == connected*"shows no name"* ]] \
    || give_up "$mac was not sighted on the $port port, and that port does not show a Mac with no name: $(shows "$port")"
  boot="$(boot_of "$mac")"
  T0="$(date +%s)"
  reset "$mac" --prove "$port"
  (( RESET_CODE == 0 )) || give_up "$mac shows no name on the $port port, and the Proving reset of that port did not place it: $(plain "$WORK/reset.out")"
  [[ "$(boot_of "$mac")" != "$boot" ]] || give_up "the Proving reset of the $port port is said to have placed $mac, which is on the boot it was on"
  wait_until 30 is_placed "$mac" "$port" || give_up "after its Proving reset $mac is not shown on the $port port: $(cable_of "$mac")"
  rec "$mac shows no name on the $port port (cable or kernel): placed there by a Proving reset, back in $(( $(date +%s) - T0 )) s, every other Mac on the boot it was on"
}

# placed MAC: the Mac is placed now, or is placed here by a Proving reset of
# the port --m1-port or --m2-port names. Leaves PORT.
placed() {
  local mac="$1" port="$M2_PORT"
  [[ $mac == "$M2" ]] || port="$M1_PORT"
  if ! wait_until "$(if playing; then echo 25; else echo 45; fi)" is_placed "$mac"; then
    [[ -n $port ]] || give_up "the lab cannot place $mac ($(cable_of "$mac")). If it shows no name, name its port (--m1-port or --m2-port) and the test places it by a Proving reset"
    place "$mac" "$port"
  fi
  PORT="$(port_of "$mac")"
}

# The mini. Through the Lab address it is reached over SSH; where root is
# needed the owner types the password sudo asks for. Against the made-up
# mini its control port stands in.

# mini_seen FILE: what the mini sees on each port, one line a port:
# rid= port= connected= product= plugs= connection=
mini_seen() {
  if playing; then
    curl -fsS --max-time 20 "$FAKE/seen" > "$1" || fail "the made-up mini does not answer"
  else
    # shellcheck disable=SC2029
    ssh "$MINI" "$MINI_SCRIPTS/ports" --seen > "$1" 2> "$WORK/ssh.err" || give_up "the mini did not say what its ports see: $(plain "$WORK/ssh.err")"
  fi
  [[ "$(grep -c '^rid=[0-9]* port=' "$1")" == 3 ]] || give_up "the mini's ports were not read as three lines"
}
# seen_of FILE PORT KEY: one value of one port's line.
seen_of() { sed -n "s/.* port=$2 .*/&/p" "$1" | tr ' ' '\n' | sed -n "s/^$3=//p"; }

# mini_port_check FILE: the port check (scripts/mini/ports), which resets
# nothing: each port's status by the reset tool through the controller, and
# the bad RIDs.
mini_port_check() {
  if playing; then
    local rid code
    : > "$1"
    for rid in 0 1 2; do
      controller "/v1/ports/$rid/status?probe=1" > "$WORK/probe.out"
      echo "ok    RID $rid: $(sed '$d' "$WORK/probe.out" | tr -d '\n')" >> "$1"
    done
    for rid in 3 -1 x 01; do
      controller "/v1/ports/$rid/status" > "$WORK/probe.out"
      code="$(tail -n 1 "$WORK/probe.out")"
      if [[ $code == 400 ]]; then
        echo "ok    RID $rid is refused: $(sed '$d' "$WORK/probe.out" | tr -d '\n')" >> "$1"
      else
        echo "FAIL  RID $rid was not refused by the controller: $code" >> "$1"
      fi
    done
    return 0
  fi
  say "the port check on the mini needs root: sudo asks for the password of ${MINI%%@*}"
  ssh -t "$MINI" sudo "$MINI_SCRIPTS/ports" 2> "$WORK/ssh.err" | tr -d '\r' | tee "$1" | grep -E '^(ok|FAIL) ' || true
  grep -q 'nothing was reset' "$1" || give_up "the port check did not run to its end on the mini: $(plain "$WORK/ssh.err")"
}

# controller PATH: the made-up controller's answer and, on a last line, its
# status. Its token is the made-up mini's own, given on standard input.
controller() {
  printf 'header = "Authorization: Bearer %s"\n' "$(<"$WORK/mini/oobd.token")" |
    curl -sS --max-time 30 -K - -w '\n%{http_code}' "$CONTROLLER$1" 2>&1
}

# mini_controller stop|start: the Out-of-band controller, stopped as launchd
# stops it and started again. Returns 1 when the mini did not do it.
mini_controller() {
  if playing; then
    fake controller "state=$(if [[ $1 == stop ]]; then echo stopped; else echo running; fi)"
    return 0
  fi
  say "the controller on the mini is root's: sudo asks for the password of ${MINI%%@*}"
  if [[ $1 == stop ]]; then
    ssh -t "$MINI" sudo launchctl bootout "system/$OOBD_LABEL"
  else
    ssh -t "$MINI" sudo launchctl bootstrap system "$OOBD_PLIST"
  fi
}

# status_part NAME: what /status says of one of the lab's parts.
status_part() {
  curl -sS --max-time 20 "$ADDRESS/status" 2> /dev/null | tr -d '\n' |
    sed -n "s/.*<dt>$1<\/dt><dd>\(<span[^>]*>\)\{0,1\}\([^<]*\).*/\2/p"
}
# shellcheck disable=SC2053
status_says() { [[ "$(status_part "$1")" == $2 ]]; }

# The made-up mini and the labd beside it.

# fake ACTION KEY=VALUE...: the owner's hand, played.
fake() {
  local action="$1" kv args=()
  shift
  for kv in "$@"; do args+=(--data-urlencode "$kv"); done
  [[ "$(curl -sS --max-time 120 -o "$WORK/fake.out" -w '%{http_code}' -X POST "$FAKE/$action" ${args[@]+"${args[@]}"} 2>&1)" == 200 ]] \
    || fail "the made-up mini refused $action: $(plain "$WORK/fake.out")"
}

# fake_resets: how many resets the made-up reset tool has sent.
fake_resets() { curl -fsS --max-time 20 "$FAKE/resets" | grep -c '^rid=' || true; }

start_labd() {
  local n port=""
  : > "$WORK/labd.log"
  "$BIN/labd" --listen="127.0.0.1:$1" --data="$LOCAL_DATA" --sign-in="$WORK/mini/sign-in" --github-url="$GITHUB" \
    --oobd="$CONTROLLER" --oobd-token-file="$WORK/mini/oobd.token" --omt=false --boot-timeout=30s >> "$WORK/labd.log" 2>&1 &
  LABD_PID=$!
  for n in $(seq 200); do
    port="$(sed -n 's/.*labd listening on http:\/\/127\.0\.0\.1:\([0-9]*\).*/\1/p' "$WORK/labd.log" | head -n 1)"
    [[ -z $port ]] || break
    kill -0 "$LABD_PID" 2>/dev/null || fail "labd stopped while starting: $(tail -n 3 "$WORK/labd.log")"
    sleep 0.1
  done
  [[ -n $port ]] || fail "labd did not say where it listens"
  LOCAL_PORT="$port"
  ADDRESS="http://127.0.0.1:$port"
}

# local_lab: the made-up mini, a labd on loopback that asks its controller,
# a login made here and kept in the run's own folder, and the two Macs as
# they are cabled today: the M1 Pro on the right port under its name, the
# M2 Max on the middle port showing none. HOME moves to the run's folder, so
# the login of whoever runs this is neither read nor written.
local_lab() {
  local tool n flags=()
  for tool in labd lab fake-mini; do
    [[ -x $BIN/$tool ]] || fail "no $tool in $BIN: run make all, or name the folder with --bin"
  done
  LAB_CMD="$BIN/lab"
  export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XDG_STATE_HOME="$WORK/home/.local/state"
  export XDG_DATA_HOME="$WORK/home/.local/share" XDG_CACHE_HOME="$WORK/home/.cache" TMPDIR="$WORK/tmp"
  LOCAL_DATA="$WORK/data"
  mkdir -p "$HOME" "$TMPDIR" "$WORK/mini"
  chmod 700 "$WORK/mini"
  # shellcheck disable=SC2206
  [[ -z ${OOB_FAKE_MINI:-} ]] || flags=($OOB_FAKE_MINI)
  "$BIN/fake-mini" --files "$WORK/mini" ${flags[@]+"${flags[@]}"} > "$WORK/fake-mini.out" 2> "$WORK/fake-mini.log" &
  FAKE_PID=$!
  for n in $(seq 200); do
    [[ "$(grep -c '^[a-z]* http://127' "$WORK/fake-mini.out")" == 3 ]] && break
    kill -0 "$FAKE_PID" 2>/dev/null || fail "the made-up mini stopped while starting: $(tail -n 3 "$WORK/fake-mini.log")"
    sleep 0.1
  done
  CONTROLLER="$(sed -n 's/^controller //p' "$WORK/fake-mini.out")"
  GITHUB="$(sed -n 's/^github //p' "$WORK/fake-mini.out")"
  FAKE="$(sed -n 's/^control //p' "$WORK/fake-mini.out")"
  [[ -n $CONTROLLER && -n $GITHUB && -n $FAKE ]] || fail "the made-up mini did not say where it serves"
  start_labd 0
  "$BIN/labd" break-glass --data="$LOCAL_DATA" --account-id=1001 owner 2>/dev/null | "$LAB_CMD" login "$ADDRESS" > /dev/null \
    || fail "no login at the labd started here"
  local_mac "$M1" right shown
  local_mac "$M2" middle unnamed
}

# local_mac NAME PORT shown|unnamed: a simulated Lab-owned Mac with its
# baseline passed, cabled to a port of the made-up mini.
local_mac() {
  ( umask 077; "$LAB_CMD" enroll "$1" --model desktop 2>/dev/null | grep -oE 'enr_[0-9a-f]+' | head -n 1 | tr -d '\n' > "$WORK/enr" )
  [[ -s $WORK/enr ]] || fail "the labd started here made no Join command"
  fake join "lab=$ADDRESS" "name=$1" "token@$WORK/enr"
  rm -f "$WORK/enr"
  "$LAB_CMD" baseline "$1" > "$WORK/baseline.log" 2>&1 || fail "the simulated $1's baseline: $(plain "$WORK/baseline.log")"
  if [[ $3 == shown ]]; then fake plug "port=$2" "mac=$1"; else fake plug "port=$2" "mac=$1" "name="; fi
}

# play_cables M1_PORT M2_PORT: the two cables moved by hand, played: only a
# cable that changes port is pulled. The M2 Max shows no name, as today.
CABLE1=right
CABLE2=middle
play_cables() {
  local to1="$1" to2="$2"
  [[ $to1 == "$CABLE1" ]] || fake unplug "port=$CABLE1"
  [[ $to2 == "$CABLE2" ]] || fake unplug "port=$CABLE2"
  [[ $to1 == "$CABLE1" ]] || fake plug "port=$to1" "mac=$M1"
  [[ $to2 == "$CABLE2" ]] || fake plug "port=$to2" "mac=$M2" "name="
  CABLE1="$to1"
  CABLE2="$to2"
}

# restart_labd: labd stopped and started on the same data and port, as
# after a restart of the mini: it holds no Sighting.
restart_labd() {
  kill "$LABD_PID" 2>/dev/null || true
  wait "$LABD_PID" 2>/dev/null || true
  start_labd "$LOCAL_PORT"
}
