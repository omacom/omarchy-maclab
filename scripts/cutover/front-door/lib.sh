# What the Front door's tests share (spec section 16, Ready item 1). Sourced
# by the scripts beside it; needs bash, curl and the lab command.
#
# Every test runs three ways:
#
#   (no option), --dry-run   says what it would do; sends nothing, needs no login
#   --local                  against a labd it starts on this machine's loopback,
#                            with a simulated Mac, a login made for the run and
#                            small files; nothing leaves this machine
#   --lab                    through the Lab address, under the saved login of
#                            whoever runs it; its dated output goes to
#                            docs/acceptance/
#
# A test never holds a token: lab reads the saved login by itself. Nothing
# lab or curl answers is printed raw: an error page becomes its status and
# ray id.
# shellcheck shell=bash disable=SC2034

FD_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$FD_HERE/../../.." && pwd)"
ME="$(basename "$0")"

ADDRESS="${LAB_ADDRESS:-https://lab.omarchy-mac.org}"
LAB_CMD="${LAB_COMMAND:-lab}"
MODE=dry
MAC=m1-pro
SIZE=""
RECORDS=""
MINI=""
JOB=""
BIN="$REPO/bin"
DEPLOY="$REPO/deploy"
REST=()

PART_BYTES=$((32 << 20))
GIB=$((1 << 30))
# The file a job leaves on the Mac for the download half of a transfer test:
# as large as one job file may be under labd's 90 MB limit on a body.
JOB_SCRIPT="$FD_HERE/transfer-file.sh"
JOB_FILE="test-transfer-file/transfer.bin"
JOB_FILE_BYTES=$((80 << 20))
MINI_SCRIPTS=/usr/local/lab/current/scripts/mini
MINI_DATA=/var/lab/data
TUNNEL_METRICS=http://127.0.0.1:20241/metrics

WORK=""
LABD_PID=""
SIM_PID=""
RECORD=""

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

# fd_options ARGS...: the options every test takes. What it does not know
# stays in REST for the test itself.
fd_options() {
  while (( $# )); do
    case "$1" in
      --dry-run) one_mode "$1" dry ;;
      --local) one_mode "$1" local ;;
      --lab) one_mode "$1" lab ;;
      --mac) (( $# >= 2 )) || fail "--mac needs a Mac's name"; MAC="$2"; shift ;;
      --size) (( $# >= 2 )) || fail "--size needs a number of bytes"; SIZE="$2"; shift ;;
      --records) (( $# >= 2 )) || fail "--records needs a folder"; RECORDS="$2"; shift ;;
      --mini) (( $# >= 2 )) || fail "--mini needs USER@HOST"; MINI="$2"; shift ;;
      --job) (( $# >= 2 )) || fail "--job needs a job's id"; JOB="$2"; shift ;;
      --bin) (( $# >= 2 )) || fail "--bin needs a folder"; BIN="$2"; shift ;;
      --deploy) (( $# >= 2 )) || fail "--deploy needs a folder"; DEPLOY="$2"; shift ;;
      -h|--help) usage ;;
      *) REST+=("$1") ;;
    esac
    shift
  done
  [[ $MAC =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || fail "--mac $MAC is not a Mac's name"
  [[ -z $SIZE || $SIZE =~ ^[1-9][0-9]*$ ]] || fail "--size $SIZE is not a number of bytes"
  [[ -z $JOB || $JOB =~ ^j[0-9a-z-]{1,40}$ ]] || fail "--job $JOB is not a job's id"
  [[ -z $MINI || $MINI =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+$ ]] || fail "--mini $MINI is not USER@HOST"
  # A login for another lab in the environment would send the run there.
  unset MACLAB_URL MACLAB_TOKEN
  export MACLAB_HOLDER="front-door-tests"
}

# no_rest: for a test that takes no option of its own.
no_rest() {
  (( ${#REST[@]} == 0 )) || fail "an option it does not take: ${REST[0]}. Nothing was done."
}

# now: seconds since 1970 with a fraction, where the machine can say one.
now() {
  local t
  if [[ -n ${EPOCHREALTIME:-} ]]; then
    echo "${EPOCHREALTIME/,/.}"
    return
  fi
  t="$(date +%s.%N 2>/dev/null)"
  if [[ $t =~ ^[0-9]+\.[0-9]+$ ]]; then echo "$t"; else date +%s; fi
}

# calc EXPRESSION: arithmetic on decimals, three places.
calc() { awk "BEGIN { printf \"%.3f\", $1 }"; }
# is EXPRESSION: whether a comparison of decimals holds.
is() { awk "BEGIN { exit !($1) }"; }
# mbit BYTES SECONDS
mbit() { awk -v b="$1" -v s="$2" 'BEGIN { if (s <= 0) s = 0.001; printf "%.1f", b * 8 / s / 1000000 }'; }

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# random_file PATH BYTES
random_file() { head -c "$2" /dev/urandom > "$1"; }

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
    head -n 20 "$1"
  fi
}

# Processes a test stops (SIGSTOP) while it runs. A stopped process takes no
# signal but KILL, so the cleanup kills these first: its wait would wait for
# them for good.
HALTED_PIDS=()

cleanup() {
  local pid
  for pid in ${HALTED_PIDS[@]+"${HALTED_PIDS[@]}"}; do
    kill -9 "$pid" 2>/dev/null || true
  done
  [[ -z $SIM_PID ]] || kill "$SIM_PID" 2>/dev/null || true
  [[ -z $LABD_PID ]] || kill "$LABD_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  [[ -z $WORK ]] || rm -rf "$WORK"
}

# workspace: a folder of the run's own, gone when the run ends.
workspace() {
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/front-door.XXXXXX")"
  trap cleanup EXIT
  trap 'exit 130' INT TERM
}

# local_lab: a labd on loopback with an empty data folder, and a login for
# it made here and kept in the run's own folder. HOME moves there, so the
# login of whoever runs this is neither read nor written.
local_lab() {
  local tool n port
  for tool in labd lab lab-agent; do
    [[ -x $BIN/$tool ]] || fail "no $tool in $BIN: run make all, or name the folder with --bin"
  done
  LAB_CMD="$BIN/lab"
  export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XDG_STATE_HOME="$WORK/home/.local/state"
  export XDG_DATA_HOME="$WORK/home/.local/share" XDG_CACHE_HOME="$WORK/home/.cache" TMPDIR="$WORK/tmp"
  LOCAL_ROOT="$WORK/root"
  LOCAL_DATA="$LOCAL_ROOT/var/lab/data"
  mkdir -p "$HOME" "$TMPDIR" "$WORK/sign-in" "$LOCAL_ROOT/var/lab"
  echo 1 > "$WORK/sign-in/admins"
  # The log is made before labd starts: a background job opens its redirect
  # only once it runs, and the first read below can come before.
  : > "$WORK/labd.log"
  "$BIN/labd" --listen=127.0.0.1:0 --data="$LOCAL_DATA" --sign-in="$WORK/sign-in" --omt=false >> "$WORK/labd.log" 2>&1 &
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
  ADDRESS="http://$LOCAL_LISTEN"
  "$BIN/labd" break-glass --data="$LOCAL_DATA" --account-id=1 front-door 2>/dev/null | "$LAB_CMD" login "$ADDRESS" > /dev/null \
    || fail "no login at the labd started here"
}

# local_mac: a simulated Mac under the name the test asks for, with its
# baseline passed.
local_mac() {
  local token n
  token="$("$LAB_CMD" enroll "$MAC" --model desktop 2>/dev/null | grep -oE 'enr_[0-9a-f]+' | head -n 1)"
  [[ -n $token ]] || fail "the labd started here made no Join command"
  "$BIN/lab-agent" sim --server "$ADDRESS" --token "$token" > "$WORK/sim.log" 2>&1 &
  SIM_PID=$!
  for n in $(seq 100); do
    "$LAB_CMD" status "$MAC" 2>/dev/null | grep -q "$MAC" && break
    sleep 0.1
  done
  "$LAB_CMD" baseline "$MAC" > "$WORK/baseline.log" 2>&1 || fail "the simulated Mac's baseline: $(plain "$WORK/baseline.log")"
}

# logged_in: the saved login is for the address the test runs through. lab
# itself says so: given the address alone, it refuses to send a saved Lab
# token anywhere else.
logged_in() {
  command -v "$LAB_CMD" >/dev/null 2>&1 || fail "no lab command: install it with curl -fsSL $ADDRESS/install/lab | bash, then lab login $ADDRESS"
  MACLAB_URL="$ADDRESS" "$LAB_CMD" status > "$WORK/status.out" 2> "$WORK/status.err" \
    || fail "lab is not logged in to Omarchy Mac Lab at $ADDRESS: $(plain "$WORK/status.err")"
}

# transfer_job: a job on the Mac that leaves the download's file among its
# files, in JOB. On a simulated Mac no script runs, so the run plays the
# Mac's part and writes the file where the job's files are kept.
transfer_job() {
  local out="$WORK/job.out"
  "$LAB_CMD" run "$MAC" --script "$JOB_SCRIPT" > "$out" 2> "$WORK/job.err" \
    || fail "the job that leaves the file on $MAC did not pass: $(plain "$WORK/job.err") $(tail -n 3 "$out")"
  JOB="$(sed -n 's/^job \(j[0-9a-z-]*\) queued.*/\1/p' "$out" | head -n 1)"
  [[ -n $JOB ]] || fail "lab run did not name its job"
  if [[ $MODE == local ]]; then
    mkdir -p "$LOCAL_DATA/jobs/$JOB/test-transfer-file"
    MACLAB_OUT="$LOCAL_DATA/jobs/$JOB/test-transfer-file" TRANSFER_BYTES="$JOB_FILE_BYTES" bash "$JOB_SCRIPT"
  fi
}

# upload_timed FILE [1|4]: sends FILE with lab upload, with labd's number of
# parts at once or the one given, and checks the SHA-256 lab prints against
# the file's. Leaves UP_SECONDS and UP_MBIT.
upload_timed() {
  local file="$1" at="${2:-}" want got t0 t1 bytes
  want="$(sha256_of "$file")"
  bytes="$(wc -c < "$file" | tr -d ' ')"
  t0="$(now)"
  got="$(MACLAB_PARTS_AT_ONCE="$at" "$LAB_CMD" upload "$file" 2> "$WORK/upload.err")" \
    || fail "the upload failed: $(plain "$WORK/upload.err")"
  t1="$(now)"
  [[ $got == "$want" ]] || fail "labd stored another SHA-256 than the file's"
  UP_SECONDS="$(calc "$t1 - $t0")"
  UP_MBIT="$(mbit "$bytes" "$UP_SECONDS")"
}

# download_timed BYTES: fetches the job's file with lab logs, again and
# again until BYTES have come. Leaves DOWN_BYTES, DOWN_SECONDS and DOWN_MBIT.
download_timed() {
  local want="$1" got t0 t1
  DOWN_BYTES=0
  t0="$(now)"
  while (( DOWN_BYTES < want )); do
    got="$("$LAB_CMD" logs "$JOB" "$JOB_FILE" 2> "$WORK/download.err" | wc -c | tr -d ' ')"
    [[ ! -s $WORK/download.err ]] || fail "the download failed: $(plain "$WORK/download.err")"
    (( got > 0 )) || fail "the download brought nothing: job $JOB has no $JOB_FILE"
    DOWN_BYTES=$((DOWN_BYTES + got))
  done
  t1="$(now)"
  DOWN_SECONDS="$(calc "$t1 - $t0")"
  DOWN_MBIT="$(mbit "$DOWN_BYTES" "$DOWN_SECONDS")"
}

# The mini's side. Under --lab it is reached over SSH as the owner, who
# types the password sudo asks for; under --local this machine stands in.

# mini_transport quic|http2|deployed: restart the tunnel under a transport.
mini_transport() {
  if [[ $MODE == local ]]; then
    say "local: no tunnel here; the transfers under \"$1\" go straight to labd"
    return 0
  fi
  ssh -t "$MINI" sudo "$MINI_SCRIPTS/transport" "$1" || fail "the mini did not put the tunnel under $1"
}

# mini_cpu: the CPU seconds cloudflared has used so far. Under --local,
# labd's, as a stand-in.
mini_cpu() {
  local t
  if [[ $MODE == local ]]; then
    ps -o time= -p "$LABD_PID" | awk -F: '{ s = 0; for (i = 1; i <= NF; i++) s = s * 60 + $i; printf "%.2f\n", s }'
    return 0
  fi
  # shellcheck disable=SC2029
  t="$(ssh "$MINI" curl -fsS --max-time 10 "$TUNNEL_METRICS" 2>/dev/null | awk '$1 == "process_cpu_seconds_total" { print $2 }')"
  [[ $t =~ ^[0-9.eE+]+$ ]] || fail "cloudflared's metrics on the mini gave no CPU time"
  calc "$t"
}

# mini_page_times: labd's own times for its pages, as numbers.
mini_page_times() {
  if [[ $MODE == local ]]; then
    LAB_SYSROOT="$LOCAL_ROOT" LAB_LOOPBACK="$LOCAL_LISTEN" "$REPO/scripts/mini/page-times"
    return
  fi
  # sudo's prompt comes down the same pipe as the numbers, and may share a
  # line with the first: it is shown, and only the numbers are kept.
  ssh -t "$MINI" sudo -u lab "$MINI_SCRIPTS/page-times" | tee /dev/stderr | tr -d '\r' | grep -oE '(probe|day)_[a-z0-9_]+=[0-9]+$'
}

# front_door_answers: wait until the lab answers again, after the tunnel
# was restarted.
front_door_answers() {
  local n
  for n in $(seq 60); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$ADDRESS/healthz" 2>/dev/null)" == 200 ]] && return 0
    sleep 2
  done
  fail "the lab did not answer at $ADDRESS within two minutes"
}

# both_ways BYTES: BYTES up with lab upload and as many down, with the CPU
# the tunnel used for each. Leaves UP_* and DOWN_* and UP_CORES, DOWN_CORES.
both_ways() {
  local bytes="$1" c0 c1 c2 file="$WORK/up.bin"
  random_file "$file" "$bytes"
  c0="$(mini_cpu)"
  upload_timed "$file"
  c1="$(mini_cpu)"
  rm -f "$file"
  download_timed "$bytes"
  c2="$(mini_cpu)"
  UP_CORES="$(calc "($c1 - $c0) / $UP_SECONDS")"
  DOWN_CORES="$(calc "($c2 - $c1) / $DOWN_SECONDS")"
}

# The record: dated output, under docs/acceptance/ for a run through the
# Lab address. A local run shows its record and keeps none.

record_start() {
  local number="$1" title="$2" dir="$RECORDS"
  if [[ -z $dir ]]; then
    if [[ $MODE == lab ]]; then dir="$REPO/docs/acceptance"; else dir="$WORK/records"; fi
  fi
  mkdir -p "$dir"
  RECORD="$dir/$(TZ=Australia/Brisbane date +%Y-%m-%d)-front-door-$number.md"
  {
    echo "# Front door test ${number#0}: $title"
    echo
    if [[ $MODE == lab ]]; then
      echo "Run $(TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M') (Brisbane) by \`scripts/cutover/front-door/$ME --lab\`, through $ADDRESS."
    else
      echo "Run $(TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M') (Brisbane) by \`scripts/cutover/front-door/$ME --local\`, against a labd on this machine's loopback: no tunnel, no edge, small files. It proves the script, not the Front door."
    fi
    echo
  } > "$RECORD"
}

# rec LINE: one line of the record, shown as it is written.
rec() {
  echo "- $*" >> "$RECORD"
  say "$*"
}

# record_done WORD: pass, fail or stopped.
record_done() {
  { echo; echo "Result: $1"; } >> "$RECORD"
  if [[ $MODE == lab || -n $RECORDS ]]; then
    say "$1. The record is $RECORD"
  else
    say "$1"
  fi
}
