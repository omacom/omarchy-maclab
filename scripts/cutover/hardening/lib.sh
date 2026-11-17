# What Hardening's fourteen tests share (spec section 16, Ready item 8;
# section 12). Sourced by the scripts beside it; needs bash, curl and the lab
# command. It keeps to the bash macOS has (3.2).
#
# Every test runs three ways:
#
#   (no option), --dry-run   says each step and what it expects; sends
#                            nothing, needs no login
#   --local                  against a labd it starts on this machine's
#                            loopback, with a stand-in for GitHub
#                            (tools/fake-github) that signs in the people a
#                            test needs, and simulated Macs (lab-agent sim).
#                            It proves the script and the lab's rules, not
#                            the mini
#   --lab                    through the Lab address, under the saved login
#                            of whoever runs it (the owner, a Maintainer).
#                            Its dated output goes to docs/acceptance/
#
# Where the owner's hand or eyes are needed the test stops, says what to do
# and waits for Enter or a yes. With nobody there to answer it ends as
# "stopped" (exit 3), which is no failure of the script. A test whose
# behaviour another slice has not landed yet ends as "waits" (exit 4) and
# names the slice.
#
# Each run writes its record, YYYY-MM-DD-hardening-NN.md, ending in
# "Result: pass|fail|stopped|waits", and its line in hardening.results:
# "NN RESULT DATE RECORD". The Ready list reads that file: `run --results`
# is green when all fourteen say pass.
#
# A test never prints a token or a session. What the probe or curl answers
# is printed as one plain line, an error page as its status alone.
# shellcheck shell=bash disable=SC2034

HD_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HD_HERE/../../.." && pwd)"
ME="$(basename "$0")"

ADDRESS="${LAB_ADDRESS:-https://lab.omarchy-mac.org}"
LAB_CMD="${LAB_COMMAND:-lab}"
MODE=dry
MAC=m1-pro
MINI=""
RECORDS=""
BIN="$REPO/bin"
REST=()

MINI_SCRIPTS=/usr/local/lab/current/scripts/mini
RESULTS_NAME=hardening.results

WORK=""
PIDS=()
LABD_PID=""
RECORD=""
NUMBER=""
PROBE=""
LOCAL_DATA=""
LOCAL_FLAGS=()
LABD_ENV=()

# The people of a local run: account ids at the stand-in for GitHub.
OWNER_ID=1001  # the owner, an Admin
MEMBER_ID=1002 # an M team member
OTHER_ID=1003  # another M team member
LENDER_ID=1004 # a Volunteer
NOBODY_ID=1005 # signed in, with no role

say() { echo "$ME: $*"; }
fail() { echo "$ME: $*" >&2; exit 1; }

# hd_options ARGS...: the options every test takes. What it does not know
# stays in REST for the test itself.
hd_options() {
  while (( $# )); do
    case "$1" in
      --dry-run|--local|--lab)
        local want="${1#--}"
        [[ $want != dry-run ]] || want=dry
        [[ -z ${MODE_SET:-} || $MODE_SET == "$want" ]] || fail "$1 and --${MODE_SET/#dry/dry-run} are two modes: give one. Nothing was done."
        MODE="$want" MODE_SET="$want" ;;
      --mac) (( $# >= 2 )) || fail "--mac needs a Mac's name"; MAC="$2"; shift ;;
      --mini) (( $# >= 2 )) || fail "--mini needs USER@HOST"; MINI="$2"; shift ;;
      --records) (( $# >= 2 )) || fail "--records needs a folder"; RECORDS="$2"; shift ;;
      --bin) (( $# >= 2 )) || fail "--bin needs a folder"; BIN="$2"; shift ;;
      -h|--help) usage ;;
      *) REST+=("$1") ;;
    esac
    shift
  done
  [[ $MAC =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || fail "$MAC is not a Mac's name"
  [[ -z $MINI || $MINI =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+$ ]] || fail "--mini $MINI is not USER@HOST"
  # A login for another lab in the environment would send the run there.
  unset MACLAB_URL MACLAB_TOKEN
  export MACLAB_HOLDER="hardening-tests"
}

# no_rest: for a test that takes no option of its own.
no_rest() {
  (( ${#REST[@]} == 0 )) || fail "an option it does not take: ${REST[0]}. Nothing was done."
}

playing() { [[ $MODE == local ]]; }
today() { TZ=Australia/Brisbane date +%Y-%m-%d; }
nonce() { head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

# plain FILE: what was said, fit to print: an error page is never shown.
plain() {
  [[ -s $1 ]] || return 0
  if grep -qiE '<!doctype|<html|<body' "$1"; then
    echo "a page, not shown"
  else
    head -n 4 "$1" | tr '\n' ' ' | tr -cd '[:print:]' | cut -c1-300 | sed 's/ *$//'
  fi
}

# What a run changes on the lab it undoes itself; until it has, each change
# is armed here before it is made: a command, run when the run ends first,
# however it ends (a "no", a failure, Ctrl-C), the last armed first.
UNDO=()
RECORD_DONE=""

# arm NAME COMMAND...: how to undo the change about to be made.
arm() {
  local name="$1"
  shift
  UNDO+=("$name $(printf '%q ' "$@")")
}

# disarm NAME: the run undid that change itself, or it is gone.
disarm() {
  local kept=() u
  for u in ${UNDO[@]+"${UNDO[@]}"}; do
    [[ ${u%% *} == "$1" ]] || kept+=("$u")
  done
  UNDO=(${kept[@]+"${kept[@]}"})
}

# undo_all: what is still armed, undone; each says in the record how it went.
undo_all() {
  local i
  for (( i = ${#UNDO[@]} - 1; i >= 0; i-- )); do
    eval "${UNDO[i]#* }" || true
  done
  UNDO=()
}

cleanup() {
  local pid
  if [[ -n $RECORD && -z $RECORD_DONE ]]; then
    RECORD_DONE=1
    undo_all
    { echo; echo "Result: stopped"; } >> "$RECORD"
  fi
  for pid in ${PIDS[@]+"${PIDS[@]}"} $LABD_PID; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  [[ -z $WORK || -n ${HD_KEEP:-} ]] || rm -rf "$WORK"
}

# workspace: a folder of the run's own, gone when the run ends. Secrets a
# run holds (a token made for the test, a session typed at a prompt) live
# here alone, 0600, and go with it.
workspace() {
  WORK="$(mktemp -d "${TMPDIR:-/tmp}/hardening.XXXXXX")"
  chmod 700 "$WORK"
  trap cleanup EXIT
  trap 'exit 130' INT TERM
}

# the probe: the one in --bin, or built here from this checkout.
need_probe() {
  if [[ -x $BIN/hardening-probe ]]; then
    PROBE="$BIN/hardening-probe"
    return 0
  fi
  command -v go >/dev/null 2>&1 || fail "no hardening-probe in $BIN and no go to build it with: run this from a checkout on a machine with Go"
  PROBE="$WORK/hardening-probe"
  (cd "$REPO" && go build -o "$PROBE" ./tools/hardening-probe) || fail "the probe did not build"
}

# begin NN TITLE [LABD-FLAGS...]: the start of a run that is no dry run.
begin() {
  local number="$1" title="$2"
  shift 2
  workspace
  need_probe
  if playing; then
    LOCAL_FLAGS=("$@")
    ! declare -F before_lab > /dev/null || before_lab
    local_lab
  else
    logged_in
  fi
  export LAB_ADDRESS="$ADDRESS"
  record_start "$number" "$title"
}

# The record.

record_start() {
  local title="$2" dir="$RECORDS"
  NUMBER="$1"
  if [[ -z $dir ]]; then
    if [[ $MODE == lab ]]; then dir="$REPO/docs/acceptance"; else dir="$WORK/records"; fi
  fi
  mkdir -p "$dir"
  RECORD="$dir/$(today)-hardening-$NUMBER.md"
  {
    echo "# Hardening's test ${NUMBER#0}: $title"
    echo
    if [[ $MODE == lab ]]; then
      echo "Run $(TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M') (Brisbane) by \`scripts/cutover/hardening/$ME --lab\`, through $ADDRESS, under the saved login of whoever ran it."
    else
      echo "Run $(TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M') (Brisbane) by \`scripts/cutover/hardening/$ME --local\`, against a labd on this machine's loopback with a stand-in for GitHub and simulated Macs. It proves the script and the lab's rules, not the mini."
    fi
    echo
  } > "$RECORD"
  # Until the run ends, its line says stopped: a run cut half way leaves no
  # old pass behind it.
  results_line stopped
}

# results_line WORD: the test's line in the results file, in place of the
# one it had.
results_line() {
  local results kept
  results="$(dirname "$RECORD")/$RESULTS_NAME"
  kept="$(grep -v "^$NUMBER " "$results" 2>/dev/null || true)"
  { [[ -z $kept ]] || echo "$kept"; echo "$NUMBER $1 $(today) $(basename "$RECORD")"; } | sort > "$results.new"
  mv "$results.new" "$results"
}

# rec LINE: one line of the record, shown as it is written.
rec() {
  echo "- $*" >> "$RECORD"
  say "$*"
}

# record_done WORD: pass, fail, stopped or waits, in the record and in the
# results file.
record_done() {
  undo_all
  RECORD_DONE=1
  { echo; echo "Result: $1"; } >> "$RECORD"
  results_line "$1"
  if [[ $MODE == lab || -n $RECORDS ]]; then
    say "$1. The record is $RECORD"
  else
    say "$1"
  fi
}

passed() { record_done pass; exit 0; }
give_up() { rec "FAILED: $1"; record_done fail; exit 1; }
# stopped WHY: the run ends for the owner; exit 3 is no failure of the script.
stopped() { rec "STOPPED: $1"; record_done stopped; exit 3; }
# waits_for SLICE WHY: what the test checks is another slice's, not landed.
waits_for() { rec "WAITS for slice $1: $2"; record_done waits; exit 4; }

# hands TEXT: a step only the owner can do. Through the Lab address the run
# waits for Enter; locally the test plays it right after.
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

# confirm QUESTION: what only the owner can see. "yes" goes on.
confirm() {
  local line
  echo
  printf '%s: %s Type yes or no: ' "$ME" "$1"
  IFS= read -r line || stopped "nobody was there to answer: $1"
  [[ $line == yes ]]
}

# hidden FILE PROMPT: a secret typed at a prompt that shows nothing, kept in
# FILE (0600) inside the run's own folder.
hidden() {
  local line
  echo
  printf '%s: %s (it is not shown): ' "$ME" "$2"
  IFS= read -rs line || stopped "nobody was there to type it: $2"
  echo
  [[ -n $line ]] || stopped "nothing was typed for: $2"
  ( umask 077; printf '%s\n' "$line" > "$1" )
}

# The lab, asked through lab and the probe.

# logged_in: the saved login is for the address the test runs through.
logged_in() {
  command -v "$LAB_CMD" >/dev/null 2>&1 || fail "no lab command: install it with curl -fsSL $ADDRESS/install/lab | bash, then lab login $ADDRESS"
  MACLAB_URL="$ADDRESS" "$LAB_CMD" status > "$WORK/status.out" 2> "$WORK/status.err" \
    || fail "lab is not logged in to Omarchy Mac Lab at $ADDRESS: $(plain "$WORK/status.err")"
}

# ask NAME ARGS...: the probe's ask; its facts in WORK/NAME.out. ARGS are
# the probe's own: [--token-file F | --session F] ... METHOD PATH.
ask() {
  local name="$1"
  shift
  "$PROBE" ask --out "$WORK/$name.body" "$@" > "$WORK/$name.out" 2> "$WORK/$name.err" || {
    echo "status=0" > "$WORK/$name.out"
    echo "said=$(plain "$WORK/$name.err")" >> "$WORK/$name.out"
  }
}
# form NAME ARGS...: the probe's form, facts in WORK/NAME.out.
form() {
  local name="$1"
  shift
  "$PROBE" form --out "$WORK/$name.body" "$@" > "$WORK/$name.out" 2> "$WORK/$name.err" || {
    echo "status=0" > "$WORK/$name.out"
    echo "said=$(plain "$WORK/$name.err")" >> "$WORK/$name.out"
  }
}
# fact NAME KEY: one value the probe said.
fact() { sed -n "s/^$2=//p" "$WORK/$1.out" | head -n 1; }
status_of() { fact "$1" status; }
said_of() { local s; s="$(fact "$1" said_page)"; [[ -n $s ]] || s="$(fact "$1" said)"; echo "$s"; }
# answered NAME STATUS TEXT: the answer had that status and said TEXT.
answered() { [[ "$(status_of "$1")" == "$2" ]] && { [[ -z ${3:-} ]] || grep -qF -- "$3" "$WORK/$1.body" 2>/dev/null || said_of "$1" | grep -qF -- "$3"; }; }
# rows NAME: the probe's row= lines.
rows() { sed -n 's/^row=//p' "$WORK/$1.out"; }

# wait_until SECONDS COMMAND...: true once the command is.
wait_until() {
  local end=$(( $(date +%s) + $1 ))
  shift
  while ! "$@"; do
    (( $(date +%s) < end )) || return 1
    sleep "$(if playing; then echo 0.3; else echo 3; fi)"
  done
}

# owner_session: a file with the owner's browser session. Locally it is the
# owner's of this run; through the Lab address the owner types the value of
# the session cookie of a browser signed in at the lab (__Host-lab_session,
# from the browser's developer tools), once, at a prompt that shows nothing.
OWNER_SESSION=""
owner_session() {
  if [[ -n $OWNER_SESSION ]]; then return 0; fi
  OWNER_SESSION="$WORK/owner.session"
  if playing; then
    sign_in owner "$OWNER_ID"
    return 0
  fi
  hidden "$OWNER_SESSION" "the value of the __Host-lab_session cookie of your browser, signed in at $ADDRESS"
  { "$PROBE" page --session "$OWNER_SESSION" /me > "$WORK/me.out" 2>&1 && grep -qx 'status=200' "$WORK/me.out"; } \
    || stopped "that session does not open your page at $ADDRESS/me"
}

# The local lab: a labd on loopback, the stand-in for GitHub, the owner's
# login made here, simulated Macs. HOME moves to the run's folder, so the
# login of whoever runs this is neither read nor written.

# clean [NAME=VALUE...] COMMAND... &: a daemon of the local lab starts as
# launchd starts the mini's, with an environment of its own: nothing of the
# environment of whoever runs the test reaches it. Only ever in the
# background: it replaces the subshell, so that $! is the daemon itself and
# the run's end stops it.
clean() { exec env -i PATH="$PATH" HOME="$HOME" TMPDIR="$TMPDIR" "$@"; }

local_lab() {
  local tool n port gh
  for tool in labd lab lab-agent fake-github; do
    [[ -x $BIN/$tool ]] || fail "no $tool in $BIN: run make all, or name the folder with --bin"
  done
  LAB_CMD="$BIN/lab"
  export HOME="$WORK/home" XDG_CONFIG_HOME="$WORK/home/.config" XDG_STATE_HOME="$WORK/home/.local/state"
  export XDG_DATA_HOME="$WORK/home/.local/share" XDG_CACHE_HOME="$WORK/home/.cache" TMPDIR="$WORK/tmp"
  LOCAL_DATA="$WORK/data"
  mkdir -p "$HOME" "$TMPDIR" "$WORK/sign-in"
  chmod 700 "$WORK/sign-in"
  clean "$BIN/fake-github" --listen 127.0.0.1:0 --files "$WORK/sign-in" --user owner:$OWNER_ID --user member:$MEMBER_ID \
    --user other:$OTHER_ID --user lender:$LENDER_ID --user nobody:$NOBODY_ID > /dev/null 2> "$WORK/github.log" &
  PIDS+=($!)
  gh=""
  for n in $(seq 100); do
    gh="$(sed -n 's/.* on \(http:\/\/127\.0\.0\.1:[0-9]*\) with.*/\1/p' "$WORK/github.log" | head -n 1)"
    [[ -z $gh ]] || break
    sleep 0.1
  done
  [[ -n $gh && -s $WORK/sign-in/github-client-secret ]] || fail "the stand-in for GitHub did not start: $(plain "$WORK/github.log")"
  GITHUB="$gh"
  echo "$OWNER_ID" > "$WORK/sign-in/admins"
  ( umask 077; head -c 64 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$WORK/machine-id-key" )
  start_labd 0
  "$BIN/labd" break-glass --data="$LOCAL_DATA" --account-id=$OWNER_ID owner 2>/dev/null | "$LAB_CMD" login "$ADDRESS" > /dev/null \
    || fail "no login at the labd started here"
}

# start_labd PORT: labd on loopback, on PORT (0: one picked here). The port
# is picked below the range the system hands out for outgoing connections:
# a simulated Mac's reconnect could otherwise take it while labd restarts.
start_labd() {
  local n port="" try want="$1"
  for try in 1 2 3 4 5 6 7 8; do
    [[ $1 != 0 ]] || want=$(( 20000 + RANDOM % 9000 ))
    : > "$WORK/labd.log"
    clean ${LABD_ENV[@]+"${LABD_ENV[@]}"} "${LABD_BIN:-$BIN/labd}" --listen="127.0.0.1:$want" --data="$LOCAL_DATA" --sign-in="$WORK/sign-in" --github-url="$GITHUB" \
      --machine-id-key="$WORK/machine-id-key" --oobd=http://127.0.0.1:1 --omt=false \
      ${LOCAL_FLAGS[@]+"${LOCAL_FLAGS[@]}"} >> "$WORK/labd.log" 2>&1 &
    LABD_PID=$!
    for n in $(seq 300); do
      port="$(sed -n 's/.*labd listening on http:\/\/127\.0\.0\.1:\([0-9]*\).*/\1/p' "$WORK/labd.log" | head -n 1)"
      [[ -z $port ]] || break
      kill -0 "$LABD_PID" 2>/dev/null || break
      sleep 0.1
    done
    [[ -z $port ]] || break
    grep -q 'address already in use' "$WORK/labd.log" || fail "labd stopped while starting: $(tail -n 3 "$WORK/labd.log")"
    sleep 1
  done
  [[ -n $port ]] || fail "labd did not say where it listens: $(tail -n 3 "$WORK/labd.log")"
  LOCAL_PORT="$port"
  ADDRESS="http://127.0.0.1:$port"
  export LAB_ADDRESS="$ADDRESS"
}

# restart_labd: labd stopped and started again on the same data and port.
restart_labd() {
  kill "$LABD_PID" 2>/dev/null || true
  wait "$LABD_PID" 2>/dev/null || true
  start_labd "$LOCAL_PORT"
}

# local_mac NAME: a simulated Lab-owned Mac with its baseline passed.
local_mac() {
  local token n
  token="$( { "$LAB_CMD" enroll "$1" --model desktop 2>/dev/null || true; } | { grep -oE 'enr_[0-9a-f]+' || true; } | head -n 1)"
  [[ -n $token ]] || fail "the labd started here made no Join command for $1"
  sim_mac "$1" "$token"
  "$LAB_CMD" baseline "$1" > "$WORK/baseline.log" 2>&1 || fail "the simulated $1's baseline: $(plain "$WORK/baseline.log")"
}

# sim_mac NAME TOKEN: lab-agent sim joins with a Join command's token.
sim_mac() {
  local n
  clean "${SIM_AGENT:-$BIN/lab-agent}" sim --server "$ADDRESS" --token "$2" --reboot-delay 100ms > "$WORK/sim-$1.log" 2>&1 &
  PIDS+=($!)
  echo $! > "$WORK/sim-$1.pid"
  for n in $(seq 300); do
    "$LAB_CMD" status "$1" 2>/dev/null | grep -q "^$1 " && return 0
    sleep 0.1
  done
  fail "the simulated $1 did not join: $(plain "$WORK/sim-$1.log")"
}

# sim_dir NAME: the folder of a simulated Mac (its agent.json names it).
# The Mac shows in the lab a moment before it has written that file.
sim_dir() {
  local f n
  for n in $(seq 100); do
    for f in "$TMPDIR"/lab-agent-sim-*/agent.json; do
      [[ -f $f ]] && grep -q "\"name\": *\"$1\"" "$f" && { dirname "$f"; return 0; }
    done
    sleep 0.1
  done
  return 1
}

# sign_in NAME ACCOUNT: a browser of that account, signed in; its session
# in WORK/NAME.session.
sign_in() {
  "$PROBE" sign-in --session "$WORK/$1.session" --as "$2" > "$WORK/signin-$1.out" 2>&1 \
    || fail "account $2 was not signed in: $(plain "$WORK/signin-$1.out")"
}

# people: the four people of a local run beside the owner, signed in; the
# member and the other marked M team by the owner.
people() {
  owner_session
  sign_in member "$MEMBER_ID"
  sign_in other "$OTHER_ID"
  sign_in lender "$LENDER_ID"
  sign_in nobody "$NOBODY_ID"
  local who
  for who in member other; do
    form "mark-$who" --session "$OWNER_SESSION" --page /people /people/mark "handle=$who" as=m-team
    [[ "$(status_of "mark-$who")" == 303 ]] || fail "the owner could not mark $who as M team: $(status_of "mark-$who") $(said_of "mark-$who")"
  done
}

# token_for NAME TOKEN-NAME: the person makes a Lab token on their own page;
# it goes into WORK/NAME.token and is never printed.
token_for() {
  form "token-$1" --session "$WORK/$1.session" /me/tokens "name=$2" period=30
  [[ "$(status_of "token-$1")" == 200 ]] || fail "$1 could not make a Lab token: $(status_of "token-$1") $(said_of "token-$1")"
  ( umask 077; grep -oE 'lab_[0-9a-f]{40}' "$WORK/token-$1.body" | head -n 1 > "$WORK/$1.token" )
  rm -f "$WORK/token-$1.body"
  [[ -s $WORK/$1.token ]] || fail "the page that made $1's Lab token did not show it"
}

# token_id ACCOUNT TOKEN-NAME: the id of a person's Lab token, read from
# their row on People, opened (the owner's session).
token_id() {
  "$PROBE" page --session "$OWNER_SESSION" --out "$WORK/people.html" --find "/people/$1/tokens/([0-9]+)/revoke\"[^<]*<input[^>]*>[^<]*<button[^>]*data-sentence=\"[^\"]*$2" "/people?open=$1" > "$WORK/tid.out" 2>&1
  sed -n 's/^found=//p' "$WORK/tid.out" | head -n 1
}

# volunteer_mac NAME: the lender asks to lend, the owner approves, the
# lender makes a Join command on Lend a Mac and a simulated Mac Joins with
# it: a Volunteer's Mac.
volunteer_mac() {
  local token
  [[ -e $BIN/lab-agent-linux-arm64 ]] || fail "no lab-agent-linux-arm64 in $BIN: labd makes no Join command for a Volunteer's Mac without it; run make all"
  form ask-lend --session "$WORK/lender.session" --page /lend /lend/request model=j314s
  [[ "$(status_of ask-lend)" == 303 ]] || fail "the lender's Request to lend: $(status_of ask-lend) $(said_of ask-lend)"
  form approve --session "$OWNER_SESSION" --page /people "/people/$LENDER_ID/request/approve"
  [[ "$(status_of approve)" == 303 ]] || fail "the owner's approval: $(status_of approve) $(said_of approve)"
  form join --session "$WORK/lender.session" --page /lend /lend/join-command model=j314s "name=$1"
  token="$( { grep -oE 'enr_[0-9a-f]{32}' "$WORK/join.body" || true; } | head -n 1)"
  rm -f "$WORK/join.body"
  [[ -n $token ]] || fail "Lend a Mac showed the lender no Join command: $(status_of join) $(said_of join)"
  sim_mac "$1" "$token"
}

# undo_own_token TAG: the owner's own Lab token named TAG, revoked on their
# page, if it is there. Only a page that opened says it is not.
undo_own_token() {
  local id
  "$PROBE" page --session "$OWNER_SESSION" --find "/me/tokens/([0-9]+)/revoke\"[^<]*<input[^>]*>[^<]*<button[^>]*data-sentence=\"[^\"]*$1" /me > "$WORK/undo-token.out" 2>&1 || true
  if ! grep -qx 'status=200' "$WORK/undo-token.out"; then
    id="$(fact undo-token status)"
    rec "NOT UNDONE: the Lab token $1 may still be live: $ADDRESS/me did not open (${id:-no answer}): revoke it there"
    return 0
  fi
  id="$(sed -n 's/^found=//p' "$WORK/undo-token.out" | head -n 1)"
  if [[ -z $id ]]; then
    rec "undone: no Lab token $1 is left on $ADDRESS/me"
    return 0
  fi
  form undo-token --session "$OWNER_SESSION" "/me/tokens/$id/revoke" sure=yes
  if [[ "$(status_of undo-token)" == 303 ]]; then
    rec "undone: the Lab token $1 was revoked"
  else
    rec "NOT UNDONE: the Lab token $1 is still live: revoke it on $ADDRESS/me ($(status_of undo-token))"
  fi
}

# agents_has TEXT: the local lab's own record of its lines for "Agents".
sql_text() { local q="'"; printf "'%s'" "${1//$q/$q$q}"; }
agents_has() { [[ -n "$(sqlite3 "file:$LOCAL_DATA/labd.db?mode=ro" "SELECT 1 FROM lines WHERE channel = 'agents' AND instr(text, $(sql_text "$1")) > 0 LIMIT 1" 2>/dev/null)" ]]; }
agents_line() { sqlite3 "file:$LOCAL_DATA/labd.db?mode=ro" "SELECT text FROM lines WHERE channel = 'agents' AND instr(text, $(sql_text "$1")) > 0 ORDER BY id DESC LIMIT 1" 2>/dev/null | head -n 1; }

# told_agents TEXT WHAT: "Agents" got a line with TEXT. Locally the lab's
# own record says; through the Lab address the owner, who reads Signal.
told_agents() {
  if playing; then
    wait_until 20 agents_has "$1" || give_up "\"Agents\" got no line for $2 (one with \"$1\")"
    rec "\"Agents\" got a line for $2: $(agents_line "$1")"
  else
    confirm "Did \"Agents\" (Signal) get a line for $2, one with \"$1\"?" || give_up "\"Agents\" got no line for $2"
    rec "\"Agents\" got a line for $2 (the owner read it in Signal)"
  fi
}

# file_sha FILE: its SHA-256.
file_sha() { if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | cut -d' ' -f1; else sha256sum "$1" | cut -d' ' -f1; fi; }
# sha TEXT: its SHA-256.
sha() { if command -v shasum >/dev/null 2>&1; then printf '%s' "$1" | shasum -a 256 | cut -d' ' -f1; else printf '%s' "$1" | sha256sum | cut -d' ' -f1; fi; }
# made_up_release RELEASE MAC: a made-up manifest that names one Mac,
# frozen at the local lab as a Maintainer freezes one.
made_up_release() {
  local release="$1" mac="$2" dir="$WORK/release-$1" a b c d e payload dtbs
  mkdir -p "$dir"
  a="$(sha vmlinuz)" b="$(sha dtb)" c="$(sha mtree)" d="$(sha m1n1)" e="$(sha uboot)"
  payload="$a  vmlinuz"$'\n'
  printf '%s' "$payload" > "$dir/payload.sha256"
  dtbs="$(sha "sim.dtb $b"$'\n')"
  cat > "$dir/manifest.json" <<JSON
{"schema":"maclab.installed-manifest/2","release":"$release",
 "artifacts":[{"name":"linux-hardening","file":"linux-hardening.pkg.tar.zst","sha256":"$a","version":"0-1","mtree_sha256":"$c"}],
 "kernel":{"release":"0.0.0-hardening","vmlinuz_sha256":"$a","payload_manifest":"payload/linux-hardening.files.sha256","payload_manifest_sha256":"$(sha "$payload")","payload_count":1,"regenerated_on_mac":[]},
 "m1n1":{"bin_sha256":"$d","version":"v0-hardening"},
 "dtbs":[{"name":"sim.dtb","sha256":"$b"}],"dtbs_sha256":"$dtbs",
 "macs":{"$mac":{"board":"apple,sim","board_dtb":"sim.dtb","uboot_gz_sha256":"$e","boot_bin_sha256":"$e"}}}
JSON
  "$LAB_CMD" installed freeze-manifest --release "$release" "$dir/manifest.json" "$dir/payload.sha256" > "$WORK/manifest.out" 2>&1 \
    || fail "the labd started here did not freeze a made-up manifest: $(plain "$WORK/manifest.out")"
}
