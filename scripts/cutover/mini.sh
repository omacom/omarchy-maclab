# What the Cutover's scripts that run on the mini share. Sourced by the
# scripts beside it. The mini's own library comes from the repository this
# folder is in or, for a copy of this folder on the mini, from the build
# Deploy switched to.
# shellcheck shell=bash disable=SC2034

cutover="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mini_lib="$cutover/../mini/lib.sh"
[[ -f $mini_lib ]] || mini_lib="${LAB_SYSROOT:-}/usr/local/lab/current/scripts/mini/lib.sh"
[[ -f $mini_lib ]] || { echo "$(basename "$0"): no Lab release is Deployed on this machine ($mini_lib is not there): this runs on the mini, after a Deploy" >&2; exit 1; }
# shellcheck source=scripts/mini/lib.sh
. "$mini_lib"

LABD="$LAB_CURRENT/bin/labd"
LABD_PLIST="$LAUNCHD_DIR/$LABD_LABEL.plist"
LITESTREAM_PLIST="$LAUNCHD_DIR/$LITESTREAM_LABEL.plist"
MACHINE_ID_KEY="$LAB_HOME/secrets/machine-id-key"

deployed() {
  [[ -x $LABD && -f $LABD_PLIST ]] || fail "no Lab release is Deployed here ($LABD): Deploy one first (scripts/mini/deploy)"
}

# lab_stage: preview, "live <time>", open (a labd started without
# --preview) or unknown. The lab is in Preview while labd's LaunchDaemon
# file starts it with --preview and its data directory holds no opening:
# once the lab has opened (scripts/cutover/open) the file still says
# --preview and labd does not obey it. A labd that cannot say counts as
# unknown, never as Preview.
lab_stage() {
  local is
  grep -q '<string>--preview</string>' "$LABD_PLIST" 2>/dev/null || { echo open; return 0; }
  is="$(as_lab "$LABD" stage --data "$LAB_DATA" 2>/dev/null)" || is=""
  case "$is" in
    "not opened") echo preview ;;
    "live "*) echo "$is" ;;
    *) echo unknown ;;
  esac
}

# in_preview: the lab is closed still. A dry run by an account that cannot
# read the lab's data goes by the LaunchDaemon file alone: it changes
# nothing.
in_preview() {
  local is
  is="$(lab_stage)"
  [[ $is == preview ]] || { [[ $is == unknown ]] && (( ${DRY:-0} )) && [[ "$(id -u)" != 0 && -z $SYSROOT ]]; }
}

# hold_lab: one change to the lab at a time. The opening, and the import
# from its last look at the stage to its pass's mark, hold this lock: an
# opening never lands inside the import's emptying, nor two imports in each
# other. It is a link to the holder's process id in root's /var/run; a
# holder that died leaves a link to no process, which the next run takes.
# Two runs can find the same dead holder: only one at a time takes a left
# lock away (the directory CUTOVER_RECLAIM is made by one alone), and only
# while the lock still names that dead holder, so the lock the other has
# taken meanwhile is never removed.
CUTOVER_LOCK="$SYSROOT/var/run/org.omarchy-mac.lab.cutover"
CUTOVER_RECLAIM="$CUTOVER_LOCK.reclaim"
held_lab=0
held_reclaim=0
hold_lab() {
  local pid now try
  [[ -d ${CUTOVER_LOCK%/*} ]] || mkdir -p "${CUTOVER_LOCK%/*}"
  for try in 1 2 3 4 5 6; do
    if ln -s "$$" "$CUTOVER_LOCK" 2>/dev/null; then
      held_lab=1
      trap let_go_of_lab EXIT
      return 0
    fi
    pid="$(readlink "$CUTOVER_LOCK" 2>/dev/null)" || pid=""
    if [[ $pid =~ ^[0-9]+$ ]] && ps -p "$pid" >/dev/null 2>&1; then
      fail "another of the Cutover's scripts changes the lab now (process $pid: the opening or the import): let it end, then run this again. Nothing was changed"
    fi
    [[ -n $pid ]] || continue
    trap let_go_of_lab EXIT
    if mkdir "$CUTOVER_RECLAIM" 2>/dev/null; then
      held_reclaim=1
      now="$(readlink "$CUTOVER_LOCK" 2>/dev/null)" || now=""
      [[ $now != "$pid" ]] || rm -f "$CUTOVER_LOCK"
      rmdir "$CUTOVER_RECLAIM"
      held_reclaim=0
    else
      sleep 0.5
    fi
  done
  [[ ! -d $CUTOVER_RECLAIM ]] || fail "could not take the lock $CUTOVER_LOCK: $CUTOVER_RECLAIM is there, left by a run that stopped while it took a left lock away. If none of the Cutover's scripts runs, remove it (rmdir) and run this again. Nothing was changed"
  fail "could not take the lock $CUTOVER_LOCK: look at what is there. Nothing was changed"
}
let_go_of_lab() {
  if (( held_reclaim )); then
    rmdir "$CUTOVER_RECLAIM" 2>/dev/null || true
    held_reclaim=0
  fi
  if (( held_lab )) && [[ "$(readlink "$CUTOVER_LOCK" 2>/dev/null)" == "$$" ]]; then
    rm -f "$CUTOVER_LOCK"
  fi
  held_lab=0
}

labd_answers() {
  local n
  for (( n = ${LAB_VERIFY_TRIES:-30}; n > 0; n-- )); do
    if loaded "$LABD_LABEL" && [[ "$(curl -fsS --max-time 5 -A lab-cutover "http://$LABD_LISTEN/healthz" 2>/dev/null)" == ok ]]; then
      return 0
    fi
    sleep 1
  done
  return 1
}

# stop_lab: labd and Litestream let go of the data directory, once the jobs
# that run have ended. had_litestream remembers whether Litestream ran, so
# that start_lab starts it again. lab_stopping is 1 from the moment after
# the drain on, when a daemon may be stopped: a way back that starts labd
# again is armed by then.
had_litestream=0
lab_stopping=0
stop_lab() {
  if loaded "$LITESTREAM_LABEL"; then had_litestream=1; fi
  # As a Deploy does: no job is cut by the stop.
  if declare -F drain_jobs >/dev/null; then
    drain_jobs 30 || fail "$NOT_DRAINED. Nothing was changed"
  fi
  lab_stopping=1
  stopped "$LABD_LABEL" || fail "labd did not stop (20 s): nothing was changed"
  stopped "$LITESTREAM_LABEL" || fail "Litestream did not stop (20 s): labd is stopped, and nothing else was changed. Start labd again with: launchctl bootstrap system $LABD_PLIST"
}

start_lab() {
  launchctl enable "system/$LABD_LABEL" || true
  launchctl bootstrap system "$LABD_PLIST" || say "launchd did not take $LABD_PLIST"
  if (( had_litestream )); then
    launchctl bootstrap system "$LITESTREAM_PLIST" || say "launchd did not take $LITESTREAM_PLIST: Litestream is not running"
  fi
  labd_answers
}
