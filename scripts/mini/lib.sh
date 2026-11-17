# What the mini's scripts share: where the lab lives on the mini and how a
# Lab release is checked there. Sourced by the scripts beside it. Needs only
# what macOS has.
# shellcheck shell=bash disable=SC2034

RELEASE_SIGNER="release@omarchy-m-testing.org"
RELEASE_NAMESPACE="omarchy-m-test-release"
RELEASE_BUNDLE="lab-server-darwin-arm64.tar.gz"
RELEASE_TAG='^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'
RELEASE_URL="https://github.com/omacom/omarchy-maclab/releases/download"

LAB_HOST="lab.omarchy-mac.org"
LABD_LABEL="org.omarchy-mac.lab.labd"
LABD_LISTEN="127.0.0.1:7770"
OOBD_LABEL="com.maclab.oobd"
OOBD_LISTEN="127.0.0.1:7780"
RESET_TOOL_NAME="macvdmtool"
CLOUDFLARED_LABEL="org.omarchy-mac.lab.cloudflared"
FRONT_DOOR="https://$LAB_HOST"
SIGNAL_LABEL="org.omarchy-mac.lab.signal-cli"
LITESTREAM_LABEL="org.omarchy-mac.lab.litestream"
BACKUP_LABEL="org.omarchy-mac.lab.backup"
DRILL_LABEL="org.omarchy-mac.lab.restore-drill"
DEAD_MAN_LABEL="org.omarchy-mac.lab.dead-man"
# tailscaled, the open-source daemon, in place of the Standalone app; the
# tag the mini joins the tailnet under, so that its key does not expire.
TAILSCALED_LABEL="org.omarchy-mac.lab.tailscaled"
TAILNET_TAG="tag:lab"
# Where the checks kept outside the house are pinged: healthchecks.io.
CHECKS_AT="https://hc-ping.com"
# cloudflared's own answer to whether it holds a connection to Cloudflare,
# on the port its LaunchDaemon file gives it (--metrics).
TUNNEL_READY="http://127.0.0.1:20241/ready"
# Where the newest version of a Homebrew formula is read, by address and
# with no Homebrew: the list Homebrew publishes, and a tap's own file.
FORMULA_LIST="https://formulae.brew.sh/api/formula"
TAP_FILES="https://raw.githubusercontent.com"
# The Builder, and the daemon that starts container's server for it and
# keeps it up. The volumes are the sizes of the spec.
CONTAINER_LABEL="org.omarchy-mac.lab.container"
BUILDER_LABEL="org.omarchy-mac.lab.builder"
BUILDER_VOLUMES="maclab-cache:40G maclab-pkgcache:120G"
# The two images: the name, lab-builder's option for it and the folder of
# the build it is made from. Each is tagged by what it is made from
# (builder_image), so a build whose builder/ is other has other images.
BUILDER_IMAGES="maclab-kbuild:--image:builder maclab-pkgbuild:--pkg-image:builder/pkg"
# The backup guest on pve2, by the name the set-up writes into /etc/hosts.
BACKUP_GUEST="lab-backup"
BACKUP_HOUSE="rest:https://$BACKUP_GUEST:8000/mini/"
BACKUP_HOST="lab-mini"

ROOT_OWNER=root
ROOT_GROUP=wheel
LAB_OWNER=lab
LAB_GROUP=lab
AS_LAB=(sudo -n -u "$LAB_OWNER" --)

# The tests stand a folder in for / (LAB_SYSROOT) and the account running
# them in for root and lab. sudo drops the variable, and without it the
# scripts run only what macOS itself put on the machine.
SYSROOT="${LAB_SYSROOT:-}"
if [[ -n $SYSROOT ]]; then
  ROOT_OWNER="$(id -un)" ROOT_GROUP="$(id -gn)" LAB_OWNER="$(id -un)" LAB_GROUP="$(id -gn)"
  AS_LAB=("${LAB_AS_LAB:-env}")
  LABD_LISTEN="${LAB_LOOPBACK:-$LABD_LISTEN}"
  OOBD_LISTEN="${LAB_OOBD_LOOPBACK:-$OOBD_LISTEN}"
  FRONT_DOOR="${LAB_FRONT_DOOR:-$FRONT_DOOR}"
  BACKUP_HOUSE="${LAB_BACKUP_HOUSE:-$BACKUP_HOUSE}"
  CHECKS_AT="${LAB_CHECKS_AT:-$CHECKS_AT}"
  TUNNEL_READY="${LAB_TUNNEL_READY:-$TUNNEL_READY}"
  FORMULA_LIST="${LAB_FORMULA_LIST:-$FORMULA_LIST}"
  TAP_FILES="${LAB_TAP_FILES:-$TAP_FILES}"
else
  PATH=/usr/bin:/bin:/usr/sbin:/sbin
  export PATH
fi

# Root's, and only root writes there: the builds, the copy of the Homebrew
# tools the daemons run from, the release key, and the secrets of what runs
# as root, which only root reads.
LAB_ROOT="$SYSROOT/usr/local/lab"
LAB_BUILDS="$LAB_ROOT/builds"
LAB_CURRENT="$LAB_ROOT/current"
LAB_TOOLS="$LAB_ROOT/tools"
LAB_SIGNERS="$LAB_ROOT/allowed_signers"
LAB_SECRETS="$LAB_ROOT/secrets"
OOBD_TOKEN="$LAB_SECRETS/oobd.token"
RESET_TOOL="$LAB_CURRENT/bin/$RESET_TOOL_NAME"
# The lab account's, and only it reads there: data, secrets, the socket, logs.
LAB_HOME="$SYSROOT/var/lab"
LABD_SOCKET="$LAB_HOME/run/labd.sock"
# labd's second socket, where a script of the mini leaves a line for
# "Agents" (scripts/mini/notify). It is not the tunnel's.
OPS_SOCKET="$LAB_HOME/run/ops.sock"
LABD_LOG="$LAB_HOME/log/labd.log"
LABD_OOBD_TOKEN="$LAB_HOME/secrets/oobd.token"
# The tunnel's credential, as cloudflared reads it with --token-file.
TUNNEL_TOKEN="$LAB_HOME/secrets/cloudflared.token"
LAUNCHD_DIR="$SYSROOT/Library/LaunchDaemons"
# Before a Deploy switches, both databases are copied here, a folder for
# each version Deployed: the way back from a release that says "No way
# back.". It is outside data/, so no backup carries it.
BEFORE_DEPLOY="$LAB_HOME/before-deploy"
# The Maintenance run's own folder, root's: its reports, and what it carries
# over the restart of the mini. Its second half is started by a LaunchDaemon
# that it installs before the restart and removes when it is done.
MAINTENANCE="$LAB_ROOT/maintenance"
MAINTENANCE_LABEL="org.omarchy-mac.lab.maintenance"
# Backups. The live databases stay out of restic: it carries the copies the
# nightly run makes in snapshots/. What the guest and R2 are reached with is
# in one folder only the lab account reads.
LAB_DATA="$LAB_HOME/data"
LAB_SNAPSHOTS="$LAB_DATA/snapshots"
LAB_DATABASES="labd.db reports.db"
BACKUP_KEYS="$LAB_HOME/secrets/backup"
BACKUP_KEYS_HOUSE="restic-password rest-password guest.crt litestream-sftp-key host-key"
BACKUP_KEYS_OFFSITE="r2-repository r2-access-key-id r2-secret-access-key"
BACKUP_OK="$LAB_HOME/log/backup.ok"
DRILL_OK="$LAB_HOME/log/restore-drill.ok"
# The checks kept outside the house: one the Dead-man switch pings each
# minute, one the nightly backup pings, one the Restore drill does. A check's
# address is its only secret, since whoever has it can ping in the lab's
# place: each is one line in a file only the lab account reads.
CHECKS="$LAB_HOME/secrets/checks"
CHECK_NAMES="dead-man backup restore-drill"
CHECK_URL="^${CHECKS_AT//./\\.}/[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}\$"
LITESTREAM_CONFIG="$LAB_ROOT/litestream.yml"
LITESTREAM="$LAB_TOOLS/current/litestream/bin/litestream"
RESTIC="$LAB_TOOLS/current/restic/bin/restic"
HOSTS_FILE="$SYSROOT/etc/hosts"
OOBD_PLIST="$LAUNCHD_DIR/$OOBD_LABEL.plist"
# Apple's container, as its package installs it; the account whose session
# its server runs in, as the set-up was told; the builder's own token, which
# only root reads; and what the watch last found.
CONTAINER="$SYSROOT/usr/local/bin/container"
CONTAINER_USER_FILE="$LAB_ROOT/container-user"
CONTAINER_STATE="$LAB_ROOT/container.state"
CONTAINER_LOG="$SYSROOT/var/log/lab-container.log"
BUILDER_TOKEN="$LAB_SECRETS/builder.token"
# The Builder's scratch folder: root's, and in no home. lab-builder, which
# runs as root, makes each build's folder there, and hands the account
# container runs under one folder inside it and nothing else.
BUILDER_IO="$LAB_ROOT/io"
# The package image is built from Arch Linux ARM's root file system. The
# set-up downloads none: the owner brings the archive, and it is kept in the
# image's build context, root's, beside a copy of the Deployed build's
# builder/pkg.
BUILDER_ROOTFS="ArchLinuxARM-aarch64-latest.tar.gz"
BUILDER_PKG_CONTEXT="$LAB_ROOT/pkg-image"
# The Builder's images the set-up made, one NAME:TAG a line, the last made
# last: it removes all but the two last of each name.
BUILDER_MADE="$LAB_ROOT/builder-images"
# The lab's Signal sender: signal-cli from the copy of the Homebrew tools,
# its account's keys and the two groups' ids as files of the lab account.
SIGNAL_PLIST="$LAUNCHD_DIR/$SIGNAL_LABEL.plist"
SIGNAL_CLI="$LAB_TOOLS/current/signal-cli/bin/signal-cli"
SIGNAL_DATA="$LAB_HOME/secrets/signal-cli"
SIGNAL_SOCKET="$LAB_HOME/run/signal-cli.sock"
BREW_PREFIX="$SYSROOT/opt/homebrew"
# tailscaled and its command, from the copy of the Homebrew tools. The
# Standalone app it replaces, which must be gone before it starts.
TAILSCALE="$LAB_TOOLS/current/tailscale/bin/tailscale"
TAILSCALED="$LAB_TOOLS/current/tailscale/bin/tailscaled"
TAILSCALED_SOCKET="$SYSROOT/var/run/tailscaled.socket"
TAILSCALE_APP="$SYSROOT/Applications/Tailscale.app"
# After Unlock, SSH takes keys only: the first file sshd reads.
SSHD_KEYS_ONLY="$SYSROOT/etc/ssh/sshd_config.d/000-lab.conf"
UPDATE_PREFS="$SYSROOT/Library/Preferences/com.apple.SoftwareUpdate"
# Every daemon of the lab and the account it runs as (the spec's table).
LAB_DAEMONS="$LABD_LABEL:lab $LITESTREAM_LABEL:lab $BACKUP_LABEL:lab $DRILL_LABEL:lab $DEAD_MAN_LABEL:lab $CLOUDFLARED_LABEL:lab $SIGNAL_LABEL:lab $OOBD_LABEL:root $CONTAINER_LABEL:root $BUILDER_LABEL:root $TAILSCALED_LABEL:root"

umask 022
ME="$(basename "$0")"
DRY=0
CHANGES=0

say() { echo "$ME: $*"; }
fail() { echo "$ME: $*" >&2; exit 1; }

# change WHAT COMMAND...: make one change, or in a dry run say what it would be.
change() {
  local what="$1"
  shift
  CHANGES=$((CHANGES + 1))
  if (( DRY )); then
    say "would $what"
  else
    say "$what"
    "$@"
  fi
}

need_root() {
  [[ -n $SYSROOT || "$(id -u)" == 0 ]] || fail "run it with sudo"
}

# sys TOOL ARGS...: a tool of macOS that reads or changes the machine itself
# (its network, its power, its updates, its sshd), which no folder can stand
# in for. A test names the folder its own stand-ins are in, and nothing else
# is run: a test never reaches the machine it runs on.
sys() {
  local tool="$1"
  shift
  if [[ -n $SYSROOT ]]; then
    [[ -n ${LAB_SYSTEM_TOOLS:-} && -x $LAB_SYSTEM_TOOLS/$tool ]] || fail "a test ran $tool with no stand-in for it"
    "$LAB_SYSTEM_TOOLS/$tool" "$@"
  else
    "$tool" "$@"
  fi
}

# loads PROGRAM: what a Mach-O file has the loader fetch: the libraries it
# names and the folders it has them looked for in. Its own name as a library
# is not among them. otool comes with the Command Line Tools, which Homebrew
# cannot be installed without.
loads() {
  local commands
  commands="$(otool -l "$1" 2>/dev/null)" && [[ $commands == *"Load command 0"* && $commands != *"Unknown load command"* ]] || return 1
  awk '$1 == "cmd" { cmd = $2 }
    ($1 == "name" || $1 == "path") && cmd != "LC_ID_DYLIB" { sub(/^ *(name|path) /, ""); sub(/ \(offset [0-9]+\)$/, ""); print }' <<<"$commands"
}

# leans COPY: why a copy of the Homebrew tools still leads back to Homebrew,
# or nothing when it does not. A link that leaves the copy does. So does a
# file that names Homebrew's folder, such as a script that runs a program
# there. A program is judged by what it loads: its own name as a library and
# the places it was built from, which it also carries, take it nowhere. The
# set-up refuses a copy that leans; the check looks at the copies in place.
leans() {
  local new="$1" real link to at="" file asked
  real="$(cd "$new" && pwd -P)"
  while IFS= read -r -d '' link; do
    to="$(readlink "$link")"
    if [[ -d $link ]]; then
      at="$(cd "$link" 2>/dev/null && pwd -P)" || at=""
    else
      at="$(cd "$(dirname "$link")" && cd "$(dirname "$to")" 2>/dev/null && pwd -P)" || at=""
    fi
    if [[ $at != "$real" && $at != "$real"/* ]]; then
      echo "${link#"$new"/} is a link that leaves the copy (to $to)"
      return 0
    fi
  done < <(find "$new" -type l -print0)
  while IFS= read -r -d '' file; do
    case "$(od -An -tx1 -N4 "$file" | tr -d ' \n')" in
      cffaedfe | cafebabe)
        if ! asked="$(loads "$file")"; then
          echo "${file#"$new"/} names Homebrew's folder, and otool could not say what it loads"
          return 0
        fi
        asked="$(grep -F -m 1 /opt/homebrew <<<"$asked" || true)"
        if [[ -n $asked ]]; then
          echo "${file#"$new"/} loads from Homebrew's folder ($asked)"
          return 0
        fi
        ;;
      *)
        echo "${file#"$new"/} names Homebrew's folder ($(grep -aoE '/opt/homebrew[A-Za-z0-9._/@+-]*' "$file" | head -n 1))"
        return 0 ;;
    esac
  done < <(grep -rlF --null /opt/homebrew "$new" 2>/dev/null || true)
}

# repoint DIR COPY: a script below DIR that runs a file of another copy of
# the tools by its path ($LAB_TOOLS/<copy>/...) runs that file of COPY
# instead. A wrapper the set-up pointed at its copy (rewrite_kegs) moves
# with a tool into another copy, as a held tool does at the next copy and a
# switch back does into the copy in use, and the copy it named is pruned.
repoint() {
  local dir="$1" to="$2" file d text changed from rep mode
  while IFS= read -r -d '' file; do
    [[ -f $file && ! -L $file && "$(head -c 2 "$file")" == '#!' ]] || continue
    text="$(cat "$file"; printf x)"
    text="${text%x}"
    changed="$text"
    for d in "$LAB_TOOLS"/*; do
      [[ -d $d && ! -L $d ]] || continue
      from="$LAB_TOOLS/$(basename "$d")/" rep="$to/"
      [[ $from != "$rep" ]] || continue
      # Unquoted: bash 3.2 would keep the quotes of a quoted replacement.
      changed="${changed//"$from"/$rep}"
    done
    [[ $changed != "$text" ]] || continue
    if [[ -x $file ]]; then mode=0555; else mode=0444; fi
    { chmod u+w "$file" && printf '%s' "$changed" > "$file" && chmod "$mode" "$file"; } || return 1
  done < <(grep -rlF --null "$LAB_TOOLS/" "$dir" 2>/dev/null || true)
}

# formulae: the Homebrew formulae of deploy/homebrew-formulae, one a line.
# The script that sources this names the folder in $deploy.
# shellcheck disable=SC2154
formulae() { sed -e 's/#.*//' "$deploy/homebrew-formulae" | awk 'NF { print $1 }'; }

# wifi_device: the Wi-Fi port's device, or nothing on a Mac with none.
# (awk reads to the end: a tool cut off in the middle of its answer would
# fail the pipe.)
wifi_device() { sys networksetup -listallhardwareports 2>/dev/null | awk '/^Hardware Port: Wi-Fi/ { getline; if (!seen) print $2; seen = 1 }' || true; }

# wired_address: the address of the first wired port that has one.
wired_address() {
  local port dev addr
  while IFS='|' read -r port dev; do
    case "$port" in Wi-Fi | *Bridge* | "") continue ;; esac
    addr="$(sys ipconfig getifaddr "$dev" 2>/dev/null || true)"
    if [[ -n $addr ]]; then
      echo "$addr"
      return 0
    fi
  done < <(sys networksetup -listallhardwareports 2>/dev/null | awk '/^Hardware Port: / { sub(/^Hardware Port: /, ""); port = $0 } /^Device: / { print port "|" $2 }')
}

# sshd_says: whether sshd takes a password after Unlock, as sshd itself
# reads its configuration; SSHD_WANT is keys only. Keywords are any case
# (macOS 27's sshd -T prints PasswordAuthentication), and are said in
# lowercase. Empty only when sshd -T printed nothing, as without root.
SSHD_WANT="kbdinteractiveauthentication no passwordauthentication no"
sshd_says() {
  sys sshd -T 2>/dev/null | awk '
    { k = tolower($1) }
    k == "passwordauthentication" || k == "kbdinteractiveauthentication" { print k, $2; n++ }
    END { if (NR && !n) print "neither passwordauthentication nor kbdinteractiveauthentication" }' | sort | tr '\n' ' ' | sed 's/ $//' || true
}

# sshd_match: the first line of a Match block, in sshd_config or a file of
# sshd_config.d, that lets SSH take a password again, as "FILE: line", or
# nothing. sshd -T prints only what holds outside every Match block. A block
# ends with its file.
sshd_match() {
  local f files=()
  for f in "$SYSROOT/etc/ssh/sshd_config" "$SYSROOT/etc/ssh/sshd_config.d"/*; do
    [[ ! -f $f ]] || files+=("$f")
  done
  (( ${#files[@]} )) || return 0
  awk '
    FNR == 1 { inside = 0 }
    { line = $0; sub(/^[ \t]+/, "", line); split(line, w, /[ \t]*=[ \t]*|[ \t]+/); k = tolower(w[1]) }
    k == "" || k ~ /^#/ { next }
    k == "match" { inside = 1; next }
    inside && (k == "passwordauthentication" || k == "kbdinteractiveauthentication" || k == "challengeresponseauthentication") && tolower(w[2]) != "no" {
      print FILENAME ": " line; exit
    }' "${files[@]}" 2>/dev/null || true
}

# power_value KEY: one of pmset's settings in use.
power_value() { sys pmset -g 2>/dev/null | awk -v key="$1" '$1 == key && !seen { print $2; seen = 1 }' || true; }

# loaded LABEL: whether launchd has that daemon.
loaded() { launchctl print "system/$1" >/dev/null 2>&1; }

# stopped LABEL: boot the daemon out and wait until launchd has let it go.
stopped() {
  local n
  loaded "$1" || return 0
  launchctl bootout "system/$1" 2>/dev/null || true
  for n in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
    loaded "$1" || return 0
    sleep 1
  done
  return 1
}

# owner_of PATH: the account PATH belongs to.
# boot_in LABEL PLIST: launchd takes the daemon from PLIST, asked up to three
# times; 1 when it is still not loaded.
boot_in() {
  local n
  launchctl enable "system/$1" 2>/dev/null || true
  for n in 1 2 3; do
    launchctl bootstrap system "$2" 2>/dev/null || true
    ! loaded "$1" || return 0
    (( n == 3 )) || sleep 1
  done
  return 1
}

owner_of() {
  # shellcheck disable=SC2012
  ls -ld "$1" | awk '{ print $3 }'
}

# looks PATH: its kind and mode, owner and group, as ls shows them.
looks() {
  # shellcheck disable=SC2012
  ls -ld "$1" 2>/dev/null | awk '{ print substr($1, 1, 10), $3 ":" $4 }'
}

# The lab account owns /var/lab, so whoever has its rights can put a link
# there, a hard link too, or swap one in while a script runs. Root therefore
# writes, chowns and chmods nothing below /var/lab with its own hand: that
# account does it, which reaches only what it could already write, and a
# symbolic link found there is refused.

# as_lab COMMAND...: run a program as the lab account.
as_lab() { (cd / && "${AS_LAB[@]}" "$@"); }

planted() {
  fail "$1 is a symbolic link (to $(readlink "$1")), and nothing the lab keeps there is one: whoever has the $LAB_OWNER account's rights put it there. Nothing was written through it. Look at what else that account did, remove the link and run this again"
}

# no_link PATH...: refuse when PATH, or a folder between it and /var/lab, is
# a symbolic link.
no_link() {
  local p
  for p in "$@"; do
    while :; do
      [[ ! -L $p ]] || planted "$p"
      [[ $p == "$LAB_HOME"/* ]] || break
      p="$(dirname "$p")"
    done
  done
}

# no_planted_link: the same for /var/lab, its folders and its secrets, to
# refuse before anything is changed.
no_planted_link() {
  local link
  link="$(find "$LAB_HOME" "$LAB_HOME/secrets" -maxdepth 1 -type l 2>/dev/null | head -n 1 || true)"
  [[ -z $link ]] || planted "$link"
}

# lab_folder PATH MODE: a folder below /var/lab, the lab account's.
lab_folder() {
  no_link "$1"
  [[ -d $1 ]] || as_lab mkdir "$1" || fail "$LAB_OWNER could not make the folder $1"
  { as_lab chown -h "$LAB_OWNER:$LAB_GROUP" "$1" && as_lab chmod "$2" "$1"; } || fail "$1 is not $LAB_OWNER's to set: look at what it is, remove it and run this again"
}

# lab_file PATH [MODE]: standard input becomes the file PATH below /var/lab,
# written by the lab account: under PATH.new, which is made only if nothing
# has that name, then moved into place.
lab_file() {
  no_link "$1" "$1.new"
  [[ ! -d $1 ]] || fail "$1 is a folder"
  # shellcheck disable=SC2016
  as_lab /bin/sh -c 'umask 077; set -C; rm -f "$1.new" && cat > "$1.new" && chmod "$2" "$1.new" && mv -f "$1.new" "$1"' lab-file "$1" "${2:-600}" ||
    fail "$LAB_OWNER could not write $1"
}

# fetch URL FILE: https, or a file on this machine.
fetch() {
  case "$1" in
    https://* | file:///*) ;;
    http://127.0.0.1:*) [[ -n $SYSROOT ]] || fail "$1 is not https" ;;
    *) fail "$1 is neither https:// nor file:///" ;;
  esac
  curl -fsSL --retry 2 --max-time 900 -o "$2" "$1"
}

# verified FILE: whether FILE.sig is the pinned release key's signature of FILE.
verified() {
  ssh-keygen -Y verify -f "$LAB_SIGNERS" -I "$RELEASE_SIGNER" -n "$RELEASE_NAMESPACE" -s "$1.sig" < "$1" >/dev/null 2>&1
}

stop_sender() { stopped "$SIGNAL_LABEL" || fail "$SIGNAL_LABEL did not stop (20 s)"; }

# sender_answers: signal-cli's daemon takes a connection on its socket.
sender_answers() { nc -U -w 1 "$SIGNAL_SOCKET" </dev/null >/dev/null 2>&1; }

# start_sender: the daemon started again, and waited for until its socket
# answers, for LAB_SENDER_WAIT seconds (30): what is sent right after, the
# test line or labd's own lines, finds it there. One that does not answer is
# said, and not failed.
start_sender() {
  local waited=0 wait="${LAB_SENDER_WAIT:-30}"
  stop_sender
  launchctl enable "system/$SIGNAL_LABEL" || true
  launchctl bootstrap system "$SIGNAL_PLIST" || fail "launchd did not take $SIGNAL_PLIST"
  loaded "$SIGNAL_LABEL" || fail "$SIGNAL_LABEL is not loaded after its start: see $LAB_HOME/log/signal-cli.log"
  until sender_answers; do
    if (( waited >= wait )); then
      say "$SIGNAL_LABEL started, but its socket $SIGNAL_SOCKET does not answer after $wait s: labd's lines and scripts/mini/signal test fail until it does. See $LAB_HOME/log/signal-cli.log"
      return 0
    fi
    sleep 1
    waited=$((waited + 1))
  done
}

# notify TEXT: one operations line for "Agents". A line that cannot be sent
# is still printed, so it is in the log of whatever ran.
notify() {
  local hook
  hook="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/notify"
  say "$1"
  [[ ! -x $hook ]] || "$hook" agents "$1" || say "that line was not sent"
}

# trouble KEY TEXT: the same for a trouble that lasts. "Agents" is told once,
# however often this is said, until recovered KEY says it is over.
trouble() {
  local hook
  hook="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/notify"
  say "$2"
  [[ ! -x $hook ]] || "$hook" trouble "$1" "$2" || say "that line was not sent"
}

# recovered KEY TEXT: the trouble named KEY is over. "Agents" is told only
# if it was told of the trouble, so this prints nothing.
recovered() {
  local hook
  hook="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/notify"
  [[ ! -x $hook ]] || "$hook" recovered "$1" "$2" >/dev/null 2>&1 || true
}

# What follows is for the two scripts that restart labd: Deploy and the
# Maintenance run. Both run as root and reach labd as the lab account.

# tell TEXT: one line for "Agents", from a script that runs as root. Deploy
# runs from a checkout, often in an admin's home that the lab account cannot
# read, so the lab account runs no script of it: it writes the line in
# notify.log, as notify does, and hands it to labd on the ops socket with
# curl. A labd that is loaded and does not take it yet, as one just started
# again, is asked again each second for LAB_TELL_TRIES seconds (10).
tell() {
  local log="$LAB_HOME/log/notify.log" line n kept=0
  say "$1"
  line="$(tr '\n' ' ' <<<"$1")"
  line="${line% }"
  # shellcheck disable=SC2016
  if (no_link "$log") && as_lab /bin/sh -c 'printf "%s agents %s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1" >> "$2"' tell "$line" "$log" 2>/dev/null; then
    kept=1
  fi
  for (( n = ${LAB_TELL_TRIES:-10}; n > 0; n-- )); do
    if [[ -S $OPS_SOCKET ]] && printf '%s' "$line" | as_lab curl -fsS --max-time 10 --unix-socket "$OPS_SOCKET" -H 'Content-Type: text/plain' --data-binary @- http://labd/agents/line >/dev/null 2>&1; then
      return 0
    fi
    if (( n == 1 )) || ! loaded "$LABD_LABEL"; then break; fi
    sleep 1
  done
  if (( kept )); then
    say "that line did not reach \"Agents\": it is in $log"
  else
    say "that line did not reach \"Agents\", and could not be written to $log either"
  fi
}

# ops METHOD PATH: labd's answer on its ops socket.
ops() { as_lab curl -fsS --max-time 20 --unix-socket "$OPS_SOCKET" -X "$1" "http://labd$2" 2>/dev/null; }

# ops_status METHOD PATH: labd's answer in OPS_ANSWER, with no newline at
# its end, and its HTTP status in OPS_STATUS: 000 when the socket did not
# answer at all or the answer did not arrive whole (curl failed).
OPS_ANSWER=""
OPS_STATUS=""
ops_status() {
  local out code=0
  out="$(as_lab curl -sS --max-time 20 --unix-socket "$OPS_SOCKET" -X "$1" -w '\n%{http_code}' "http://labd$2" 2>/dev/null)" || code=$?
  OPS_STATUS="${out##*$'\n'}"
  OPS_ANSWER="${out%$'\n'*}"
  OPS_ANSWER="${OPS_ANSWER%$'\n'}"
  [[ $out == *$'\n'* ]] || OPS_ANSWER=""
  [[ $OPS_STATUS =~ ^[0-9]{3}$ ]] || OPS_STATUS=000
  (( code == 0 )) || OPS_STATUS=000
}

DRAINING=0

# ops_socket_missing: labd's LaunchDaemon file starts it with an ops socket,
# and there is none. Only a labd whose file names none is too old to have it.
# Deploy's check of the parts asks it.
ops_socket_missing() {
  [[ ! -S $OPS_SOCKET ]] && grep -qF -- '--ops-socket' "$LAUNCHD_DIR/$LABD_LABEL.plist" 2>/dev/null
}

# drain_jobs MINUTES: labd starts no further job, and this waits until the
# jobs and builds that run have ended. A job that waits stays queued and
# starts after the restart. Only a labd too old to drain (404, or a
# LaunchDaemon file that names no ops socket) is gone on from with no drain.
# Returns 1, with the drain lifted, when some still run after MINUTES, when
# labd runs with no ops socket though its file names one, or when labd's
# drain fails or answers what is not understood; NOT_DRAINED then says
# which, and STILL_RUNNING what still ran.
STILL_RUNNING=""
NOT_DRAINED=""
drain_jobs() {
  local limit=$(( $1 * 60 )) step="${LAB_DRAIN_STEP:-5}" waited=0 jobs builds
  if loaded "$LABD_LABEL" && ops_socket_missing; then
    NOT_DRAINED="labd runs with no ops socket at $OPS_SOCKET, though its LaunchDaemon file names one, so running jobs could not be kept whole. If labd is down for good, stop it (sudo launchctl bootout system/$LABD_LABEL) and run this again"
    return 1
  fi
  if [[ ! -S $OPS_SOCKET ]] || ! loaded "$LABD_LABEL"; then
    say "labd does not run: there is no job to drain"
    return 0
  fi
  # Set before the drain is asked for, so that a stop while it is asked lifts it.
  DRAINING=1
  ops_status POST /drain
  if [[ $OPS_STATUS == 404 ]]; then
    DRAINING=0
    say "the labd that runs does not drain jobs (it is older than this script): a job that runs now ends with the restart"
    return 0
  fi
  while :; do
    if [[ $OPS_STATUS != 200 ]]; then
      NOT_DRAINED="labd's drain of jobs failed (status $OPS_STATUS on its ops socket), so running jobs could not be kept whole"
      lift_drain
      return 1
    fi
    # The whole answer is the two counts, and nothing else.
    if [[ $OPS_ANSWER =~ ^([0-9]+)\ ([0-9]+)$ ]]; then
      jobs="${BASH_REMATCH[1]}" builds="${BASH_REMATCH[2]}"
    else
      jobs="" builds=""
    fi
    if [[ ! ${jobs:-} =~ ^[0-9]+$ || ! ${builds:-} =~ ^[0-9]+$ ]]; then
      NOT_DRAINED="labd's answer to the drain of jobs was not understood, so running jobs could not be kept whole"
      lift_drain
      return 1
    fi
    (( jobs + builds > 0 )) || break
    if (( waited >= limit )); then
      STILL_RUNNING="$jobs jobs and $builds builds"
      NOT_DRAINED="$STILL_RUNNING still ran after $1 minutes: cancel them (lab cancel) or try again later"
      lift_drain
      return 1
    fi
    (( waited % 60 )) || say "waiting for $jobs jobs and $builds builds to end"
    sleep "$step"
    waited=$((waited + step))
    ops_status POST /drain
  done
  say "jobs are drained: none runs, and labd starts none until it is back"
}

# lift_drain: labd starts jobs again. A labd that restarted is not draining.
lift_drain() {
  (( DRAINING )) || return 0
  DRAINING=0
  ops DELETE /drain >/dev/null || true
}

# builder_image NAME DIR BUILD [ROOTFS]: NAME:TAG, the image of the Builder
# made from BUILD's DIR. TAG is the first 12 hex of a sha256 over each
# file's path and content in DIR, but the package image's folder inside
# builder/; the package image's adds the root file system it is made from
# (ROOTFS, or the one in its build context). Nothing when DIR, or that root
# file system, is not there.
builder_image() {
  local name="$1" dir="$2" build="$3" rootfs="${4:-$BUILDER_PKG_CONTEXT/$BUILDER_ROOTFS}" sums
  [[ -d $build/$dir ]] || return 0
  if [[ $dir == builder/pkg ]]; then
    [[ -s $rootfs ]] || return 0
  else
    rootfs=""
  fi
  sums="$(
    cd "$build/$dir" || exit 1
    find . -type f ! -path './pkg/*' | LC_ALL=C sort | while IFS= read -r file; do
      echo "$(shasum -a 256 < "$file" | cut -c1-64) $file"
    done
    [[ -z $rootfs ]] || echo "$(shasum -a 256 < "$rootfs" | cut -c1-64) $BUILDER_ROOTFS"
  )" || return 0
  echo "$name:$(shasum -a 256 <<<"$sums" | cut -c1-12)"
}

# builder_runs OPTION: what the Builder's installed LaunchDaemon file gives
# lab-builder for OPTION (--image, --pkg-image, --cpus); nothing without it.
builder_runs() {
  awk -v o="<string>$1</string><string>" 'index($0, o) { s = substr($0, index($0, o) + length(o)); sub(/<.*/, "", s); print s; exit }' "$LAUNCHD_DIR/$BUILDER_LABEL.plist" 2>/dev/null || true
}

# builder_behind BUILD: one line for each image the Builder does not run
# that BUILD names, when the Builder is installed.
builder_behind() {
  local pair name option dir want runs
  [[ -f $LAUNCHD_DIR/$BUILDER_LABEL.plist ]] || return 0
  for pair in $BUILDER_IMAGES; do
    IFS=: read -r name option dir <<<"$pair"
    want="$(builder_image "$name" "$dir" "$1")"
    runs="$(builder_runs "$option")"
    [[ -z $want || $runs == "$want" ]] || echo "the Builder runs ${runs:-the image lab-builder names when it is given none}, not $want, the image of $dir in the Deployed build"
  done
}

# parts_bad: what labd says is wrong with the Builder or the Out-of-band
# controller, or nothing. A part the mini does not have yet is not wrong,
# and a labd too old to say (a LaunchDaemon file with no ops socket, or 404)
# is not asked. No ops socket where the file names one, or an answer that
# fails or does not read as the two parts, is wrong.
parts_bad() {
  local builder="" controller=""
  if ops_socket_missing; then
    echo "labd has no ops socket at $OPS_SOCKET, though its LaunchDaemon file names one"
    return 0
  fi
  [[ -S $OPS_SOCKET ]] || return 0
  ops_status GET /parts
  [[ $OPS_STATUS != 404 ]] || return 0
  # The whole answer is one line for each part, and nothing else.
  if [[ "$(awk 'END { print NR }' <<<"$OPS_ANSWER")" == 2 ]]; then
    builder="$(awk '$1 == "builder" && NF == 2 { print $2 }' <<<"$OPS_ANSWER")"
    controller="$(awk '$1 == "controller" && NF == 2 { print $2 }' <<<"$OPS_ANSWER")"
  fi
  if [[ $OPS_STATUS != 200 || ! $builder =~ ^(ok|silent|none)$ || ! $controller =~ ^(ok|bad|none)$ ]]; then
    echo "labd did not say how the Builder and the Out-of-band controller are (status $OPS_STATUS on its ops socket)"
    return 0
  fi
  [[ $builder != silent ]] || echo "the Builder has not asked labd for work"
  [[ $controller != bad ]] || echo "the Out-of-band controller does not answer labd"
}

# ping_check NAME: tell that check the lab did what the check waits for. The
# address reaches curl on its standard input, never as an argument, and is
# never printed. Says why and returns 1 when nothing was sent.
ping_check() {
  local url
  if [[ ! -s $CHECKS/$1 ]]; then
    say "no check to ping for $1 yet: the set-up puts its address at $CHECKS/$1 (--checks FOLDER)"
    return 1
  fi
  url="$(<"$CHECKS/$1")"
  if [[ ! $url =~ $CHECK_URL ]]; then
    say "$CHECKS/$1 is not a check's address ($CHECKS_AT/ and the check's id, on one line): nothing was sent"
    return 1
  fi
  if ! printf 'url = "%s"\n' "$url" | curl -fsS --max-time 10 --retry 2 -o /dev/null -K - 2>/dev/null; then
    say "the check $1 was not reached"
    return 1
  fi
}
