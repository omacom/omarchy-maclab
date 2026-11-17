#!/usr/bin/env bash
# release/smoke.sh against a labd started on this machine, open (Preview off):
# install from its /install, upload a golden report, see it, see the repeat
# refused, delete it by its link. Then lab for this machine, installed from
# its /install/lab.
#
# Builds the test tool's release and a labd as one Release candidate, both
# carrying a key made here in a temporary folder and thrown away, so the
# install line labd serves pins the key that signed the download. labd runs
# on a loopback port with an empty data directory and a machine-id key made
# here. Then the same labd in Preview: its install line names the Release
# candidate it was built as, and it takes no report. Then that labd is
# opened, as on the day (the Cutover's step 25), and the same smoke test is
# green against it. Nothing leaves this machine and the release key is never
# read.
#
# Needs go, curl, python3, ssh-keygen, GNU tar.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
gnu_tools
tag="${LAB_RELEASE:-v0.0.0-rc.1}"
[[ $tag =~ $RELEASE_TAG ]] || { echo "release/smoke-local.sh: $tag is not a Lab release's tag" >&2; exit 1; }

t="$(mktemp -d)"
pid=
fail() { echo "FAIL: $*" >&2; [[ ! -f $t/labd.log ]] || sed 's/^/  labd: /' "$t/labd.log" >&2; exit 1; }
stop() {
  [[ -z $pid ]] || kill "$pid" 2>/dev/null || true
  [[ -z $pid ]] || wait "$pid" 2>/dev/null || true
  pid=
}
trap 'stop; rm -rf "$t"' EXIT
# offline COMMAND...: COMMAND with https sent to a loopback port that
# answers nothing, and the lab on the loopback reached directly: the test
# tool it installs does not ask GitHub for its latest version.
offline() { https_proxy=http://127.0.0.1:1 HTTPS_PROXY=http://127.0.0.1:1 no_proxy=127.0.0.1,localhost NO_PROXY=127.0.0.1,localhost "$@"; }

echo "== a Release candidate $tag signed with a key made here"
ssh-keygen -q -t ed25519 -N '' -C "throwaway key for the local smoke test" -f "$t/key"
printf '%s namespaces="%s" %s\n' "$RELEASE_SIGNER" "$RELEASE_NAMESPACE" "$(cut -d' ' -f1,2 "$t/key.pub")" > "$t/allowed_signers"
key="$(release_key "$t/allowed_signers")"
LAB_RELEASE="$tag" RELEASE_ALLOWED_SIGNERS="$t/allowed_signers" "$repo/release/build.sh" "$t/release" >/dev/null 2>"$t/build.log" || { cat "$t/build.log" >&2; fail "release/build.sh"; }
sign "$t/key" "$t/release/omarchy-m-test.tar.gz"
# lab for this machine, as /install/lab takes it; none is built for macOS on x86-64.
arch="$(uname -m)"
case "$arch" in aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; esac
lab_file="lab-$(uname -s | tr '[:upper:]' '[:lower:]')-$arch"
[[ ! -f $t/release/$lab_file ]] || sign "$t/key" "$t/release/$lab_file"
rm -f "$t/key"
(cd "$repo" && CGO_ENABLED=0 go build -ldflags "-X main.version=$tag -X 'main.releaseKey=$key'" -o "$t/labd" ./cmd/labd)
[[ "$("$t/labd" --version)" == "$tag" ]] || fail "labd --version"

python3 -c 'import secrets; print(secrets.token_hex(64))' > "$t/machine-id-key"
chmod 600 "$t/machine-id-key"
start() { # start [labd flags]: labd on a new loopback port, with a new data directory
  stop
  port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
  site="http://127.0.0.1:$port"
  rm -rf "$t/data"
  "$t/labd" --listen="127.0.0.1:$port" --data="$t/data" --machine-id-key="$t/machine-id-key" "$@" > "$t/labd.log" 2>&1 &
  pid=$!
  local _
  for _ in $(seq 60); do
    [[ "$(curl -sS --max-time 5 "$site/healthz" 2>/dev/null || true)" == ok ]] && return
    kill -0 "$pid" 2>/dev/null || fail "labd stopped while starting"
    sleep 1
  done
  fail "labd did not answer"
}

echo "== labd, open"
start
curl -fsS -D "$t/install.head" -o "$t/install.sh" "$site/install"
tr -d '\r' < "$t/install.head" | grep -qix 'cache-control: public, max-age=300' || fail "/install is not public for five minutes"
tr -d '\r' < "$t/install.head" | grep -qix 'content-type: text/x-shellscript; charset=utf-8' || fail "/install is not a shell script"
grep -qF "$key'" "$t/install.sh" || fail "/install does not pin the key this labd was built with"
grep -qF "$(release_key "$repo/release/allowed_signers")" "$t/install.sh" && fail "/install pins a second key"
grep -qF 'releases/latest/download' "$t/install.sh" || fail "an open lab's /install does not take the latest Lab release"

offline env SITE="$site" OMARCHY_M_TEST_RELEASES="file://$t/release" "$repo/release/smoke.sh"

curl -fsS -D "$t/install-lab.head" -o "$t/install-lab.sh" "$site/install/lab"
tr -d '\r' < "$t/install-lab.head" | grep -qix 'cache-control: public, max-age=300' || fail "/install/lab is not public for five minutes"
grep -qF "$key'" "$t/install-lab.sh" || fail "/install/lab does not pin the key this labd was built with"
grep -qF 'releases/latest/download' "$t/install-lab.sh" || fail "an open lab's /install/lab does not take the latest Lab release"
if [[ -f $t/release/$lab_file.sig ]]; then
  mkdir "$t/home"
  for round in Installed Reinstalled; do
    env -i PATH="$PATH" HOME="$t/home" OMARCHY_LAB_RELEASES="file://$t/release" bash "$t/install-lab.sh" > "$t/install-lab.log" 2>&1 || { cat "$t/install-lab.log" >&2; fail "/install/lab"; }
    grep -qxF "$round lab $tag." "$t/install-lab.log" || { cat "$t/install-lab.log" >&2; fail "/install/lab did not say: $round lab $tag."; }
  done
  [[ "$(env -i PATH="$PATH" HOME="$t/home" "$t/home/.local/bin/lab" version | head -1)" == "lab $tag" ]] || fail "the lab installed from /install/lab is not $tag"
  printf '#!/bin/sh\necho "lab: run kernels and tests on real Macs"\nexit 2\n' > "$t/home/.local/bin/lab"
  if env -i PATH="$PATH" HOME="$t/home" OMARCHY_LAB_RELEASES="file://$t/release" bash "$t/install-lab.sh" > "$t/install-lab.log" 2>&1; then fail "/install/lab replaced a lab that is not ours"; fi
  grep -qF "is not Omarchy Mac Lab's lab" "$t/install-lab.log" || fail "/install/lab did not say why it stopped"
fi

echo "== labd, in Preview"
start --preview
curl -fsS -o "$t/install-preview.sh" "$site/install"
grep -qF "releases/download/$tag}" "$t/install-preview.sh" || fail "in Preview /install does not take the Release candidate labd was built as"
grep -qF 'releases/latest/download' "$t/install-preview.sh" && fail "in Preview /install takes the latest Lab release"
curl -fsS -o "$t/install-lab-preview.sh" "$site/install/lab"
grep -qF "releases/download/$tag}" "$t/install-lab-preview.sh" || fail "in Preview /install/lab does not take the Release candidate labd was built as"
grep -qF 'releases/latest/download' "$t/install-lab-preview.sh" && fail "in Preview /install/lab takes the latest Lab release"
code="$(curl -sS -o "$t/refused.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{}' "$site/api/v1/reports")"
[[ $code == 403 ]] && grep -qF 'is not open yet' "$t/refused.json" || fail "a lab in Preview took a report ($code)"

echo "== labd, opened from Preview: the same smoke test"
mkdir -m 700 "$t/sign-in" "$t/run"
printf 'made-up-client-id\n' > "$t/sign-in/github-client-id"
(umask 077; printf 'made-up-client-secret\n' > "$t/sign-in/github-client-secret")
ops() { curl -sS --max-time 10 --unix-socket "$t/run/ops.sock" "$@"; }
start --preview --sign-in "$t/sign-in" --ops-socket "$t/run/ops.sock"
[[ "$(ops http://labd/stage)" == preview ]] || fail "a labd started with --preview does not say it is in Preview"
curl -sS -D - -o /dev/null "$site/healthz" | tr -d '\r' | grep -qix 'x-robots-tag: noindex' || fail "in Preview labd does not say noindex"
[[ "$(ops -o /dev/null -w '%{http_code}' -X POST 'http://labd/open?m-team=1')" == 409 ]] || fail "labd opened with the wrong count of M team members"
ops -X POST 'http://labd/open?m-team=0' | grep -q '^live 20' || fail "labd did not open"
curl -sS -D - -o /dev/null "$site/healthz" | tr -d '\r' | grep -qi '^x-robots-tag:' && fail "an opened lab still says noindex"
# Each page is saved, then read: under pipefail, curl | grep -q fails when
# grep has its match and exits before curl's last write.
curl -fsS -o "$t/home-open.html" "$site/"
grep -qF 'omarchy-m-testing.org is now here' "$t/home-open.html" || fail "an opened lab's home page does not say the old site is now here"
curl -fsS -o "$t/install-open.sh" "$site/install"
grep -qF 'releases/latest/download' "$t/install-open.sh" || fail "an opened lab's /install does not take the latest Lab release"
curl -fsS -o "$t/install-lab-open.sh" "$site/install/lab"
grep -qF 'releases/latest/download' "$t/install-lab-open.sh" || fail "an opened lab's /install/lab does not take the latest Lab release"
offline env SITE="$site" OMARCHY_M_TEST_RELEASES="file://$t/release" "$repo/release/smoke.sh"

echo "local smoke ok: $tag installed from labd's /install with the key made here; uploaded, seen, refused the second time and deleted; lab installed from /install/lab; in Preview both install lines name $tag; opened from Preview, the same again"
