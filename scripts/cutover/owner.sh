# What the day's scripts that run on the owner's Mac share (day, back,
# confirmed): the names, what is asked of the Lab address and the old name
# from outside, the mini over SSH, and the day's record of the steps done.
# Sourced by the scripts beside it.
# shellcheck shell=bash disable=SC2034,SC2088

set -uo pipefail
ME="$(basename "$0")"
CUTOVER="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$CUTOVER/../.." && pwd)"

LAB_HOST="lab.omarchy-mac.org"
ADDRESS="https://$LAB_HOST"
OLD_NAME="omarchy-m-testing.org"
OLD="https://$OLD_NAME"
RELEASES="https://github.com/omacom/omarchy-maclab/releases"
NEW_REPO="omacom/omarchy-maclab"
OLD_REPO="maralcbr/omarchy-m-testing"
# The existing OAuth app, which the lab takes over on the day. A client id
# is public: it is in every sign-in link, and in omarchy-m-test.
CLIENT_ID="Ov23liyct9clXDWu2d42"
VERSION="0.2.0"
M_TEAM=14
MINI=""
# Where the Cutover's scripts for the mini are there (OWNER-TODO, "49").
MINI_SCRIPTS="~/lab-cutover"
ACCEPTANCE="$REPO/docs/acceptance"
STATE="${XDG_STATE_HOME:-$HOME/.local/state}/omarchy-mac-lab/the-day"
TOKEN_FILE=""
RUNNER="$CUTOVER/front-door/run"
SMOKE="$REPO/release/smoke.sh"

say() { echo "$ME: $*"; }

# set_mode MODE: the script's mode, from --MODE. A second, different one is
# refused: which was meant is not guessed.
mode_given=""
set_mode() {
  if [[ -n $mode_given && $mode_given != "$1" ]]; then
    echo "$ME: --$mode_given and --$1 are two modes: give one. Nothing was done." >&2
    exit 2
  fi
  mode_given="$1"
  mode="$1"
}
fail() { echo "$ME: $*" >&2; exit 1; }
# stop: the script goes no further by itself: the next move is the owner's.
stop() { echo "$ME: $*"; exit 3; }

# The tests stand servers on this machine in for the Lab address, the old
# name and GitHub's releases, and a program in for the Front door's runner.
seam() {
  local name="$1" value="${!1:-}"
  [[ -n $value ]] || return 1
  [[ $value =~ ^http://127\.0\.0\.1:[0-9]+(/.*)?$ ]] || fail "$name is the tests' and takes a loopback address only"
  printf '%s' "$value"
}
ADDRESS="$(seam LAB_DAY_ADDRESS)" || ADDRESS="https://$LAB_HOST"
OLD="$(seam LAB_DAY_OLD)" || OLD="https://$OLD_NAME"
RELEASES="$(seam LAB_DAY_RELEASES)" || RELEASES="https://github.com/$NEW_REPO/releases"
if [[ -n ${LAB_DAY_RUNNER:-}${LAB_DAY_SMOKE:-} ]]; then
  [[ -n ${LAB_DAY_ADDRESS:-} ]] || fail "LAB_DAY_RUNNER and LAB_DAY_SMOKE are the tests', beside LAB_DAY_ADDRESS"
  RUNNER="${LAB_DAY_RUNNER:-$RUNNER}"
  SMOKE="${LAB_DAY_SMOKE:-$SMOKE}"
fi

today() { TZ=Australia/Brisbane date +%Y-%m-%d; }
stamp() { TZ=Australia/Brisbane date '+%Y-%m-%d %H:%M'; }

# What is asked from outside. Nothing an answer holds is printed but its
# status and a header: never a page.
ask() { curl -sS --max-time 30 -A omarchy-mac-lab-day "$@" 2>/dev/null; }
status_of() { ask -o /dev/null -w '%{http_code}' "$1" || echo 000; }
header_of() { ask -o /dev/null -D - "$1" | tr -d '\r' | awk -v n="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]'):" 'tolower($1) == n { sub(/^[^:]*: */, ""); print; exit }'; }

# The same for an address that holds a secret, as a deletion link does: it
# reaches curl on its standard input, never as an argument.
quiet() { printf 'url = "%s"\n' "$1" | curl -sS --max-time 30 -A omarchy-mac-lab-day -K - -o /dev/null "${@:2}" 2>/dev/null; }
quiet_status() { quiet "$1" -w '%{http_code}' || echo 000; }
quiet_location() { quiet "$1" -w '%{redirect_url}'; }

# lab_from_outside: preview (the Lab address answers and tells crawlers not
# to index it), open (it answers and does not), or dark.
lab_from_outside() {
  [[ "$(status_of "$ADDRESS/healthz")" == 200 ]] || { echo dark; return 0; }
  if [[ "$(header_of "$ADDRESS/healthz" X-Robots-Tag)" == noindex ]]; then echo preview; else echo open; fi
}

# old_site_answers: the old name still serves the old site, itself.
old_site_answers() { [[ "$(status_of "$OLD/")" == 200 ]]; }

# sign_in_app: the client id the lab's Sign in link sends a browser to GitHub with.
sign_in_app() { header_of "$ADDRESS/sign-in" Location | sed -n 's/.*[?&]client_id=\([^&]*\).*/\1/p'; }

# The day's record: a line for each step done, "STEP<TAB>TIME<TAB>NOTE". A
# line with less, or with no time, is no step done.
done_at() { [[ -f $STATE ]] && awk -F '\t' -v s="$1" 'NF == 3 && $1 == s && $2 ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9] [0-9][0-9]:[0-9][0-9]$/ { print $2; found = 1; exit } END { exit !found }' "$STATE"; }
note_of() { [[ -f $STATE ]] && awk -F '\t' -v s="$1" '$1 == s { print $3; exit }' "$STATE"; }
is_done() { done_at "$1" >/dev/null 2>&1; }
# record STEP [NOTE [TIME]]: TIME is now unless it is given.
record() {
  mkdir -p "$(dirname "$STATE")"
  is_done "$1" || printf '%s\t%s\t%s\n' "$1" "${3:-$(stamp)}" "${2:-}" >> "$STATE"
}

# brisbane TIME: a time in UTC as labd says it (2026-11-20T00:01:02Z), as
# the day's record writes times.
brisbane() {
  [[ $1 =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || return 1
  TZ=Australia/Brisbane date -j -f '%Y-%m-%dT%H:%M:%S%z' "${1%Z}+0000" '+%Y-%m-%d %H:%M' 2>/dev/null ||
    TZ=Australia/Brisbane date -d "$1" '+%Y-%m-%d %H:%M' 2>/dev/null
}

# newest_record NAME: the newest dated record DATE-NAME.md, or nothing.
newest_record() {
  local f last=""
  for f in "$ACCEPTANCE"/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]-"$1".md; do
    [[ -f $f ]] && last="$f"
  done
  printf '%s' "$last"
}

# on_mini SCRIPT ARGS...: one of the Cutover's scripts on the mini, with
# sudo, at a terminal: sudo asks the owner for the password, and so does
# whatever the script itself asks. Nothing typed passes through here.
mini_line() { echo "ssh -t ${MINI:-USER@HOST} 'sudo $MINI_SCRIPTS/$*'"; }
on_mini() {
  [[ -n $MINI ]] || fail "this step runs on the mini: give --mini USER@HOST"
  say "on the mini: $(mini_line "$@")"
  ssh -t "$MINI" "sudo $MINI_SCRIPTS/$*"
}

# mini_stage: where the mini itself says the lab is (open stage): preview,
# "live <time>", open or unknown; nothing when it could not be asked. sudo
# asks for the password there: all the mini's terminal shows reaches this
# one as it comes, its prompt too, and the answer is the line that is a
# stage and nothing else.
mini_stage() {
  [[ -n $MINI ]] || return 1
  echo "$ME: on the mini: $(mini_line "open stage")" >&2
  ssh -t "$MINI" "sudo $MINI_SCRIPTS/open stage" | tee /dev/stderr | tr -d '\r' | grep -E '^(preview|open|unknown|live [0-9TZ:-]+)$' | tail -n 1
}

token_option() { [[ -z $TOKEN_FILE ]] || printf -- ' --token-file %s' "$TOKEN_FILE"; }
# cloud SCRIPT ARGS...: one of scripts/cloud, which asks for the Cloudflare
# token at its own hidden prompt unless --token-file was given.
cloud() {
  local script="$1"
  shift
  say "starting: scripts/cloud/$script $*$(token_option)"
  if [[ -n $TOKEN_FILE ]]; then
    "$REPO/scripts/cloud/$script" "$@" --token-file "$TOKEN_FILE"
  else
    "$REPO/scripts/cloud/$script" "$@"
  fi
}
