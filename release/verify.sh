#!/usr/bin/env bash
# Verify a signed Lab release: release/verify.sh DIR [ALLOWED_SIGNERS [TAG]]
#
# Checks each file against its .sig the way the install lines and Deploy do
# (ssh-keygen -Y verify against the pinned release key), then opens the mini's
# bundle: lab-agent's own signature, the key pinned inside lab-agent, the
# macOS runner's own signatures, and the tag when one is named. DIR must
# hold the Lab release and nothing else.
# ALLOWED_SIGNERS defaults to release/allowed_signers, the release key.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
fail() { echo "release/verify.sh: $*" >&2; exit 1; }

[[ $# -ge 1 && $# -le 3 ]] || fail "usage: release/verify.sh DIR [ALLOWED_SIGNERS [TAG]]"
dir="$(cd "$1" && pwd)"
signers="${2:-$repo/release/allowed_signers}"
tag="${3:-}"
[[ -f $signers ]] || fail "no allowed_signers file $signers"

want="$(printf '%s\n' "${RELEASE_FILES[@]}" "${RELEASE_FILES[@]/%/.sig}" | LC_ALL=C sort)"
have="$(ls -A "$dir" | LC_ALL=C sort)"
[[ $have == "$want" ]] || fail "$dir is not a Lab release's files: ${have//$'\n'/ }"
for f in "${RELEASE_FILES[@]}"; do
  verified "$signers" "$dir/$f" || fail "$f doesn't match its signature by the release key"
done

t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT
tar -xzf "$dir/$RELEASE_BUNDLE" -C "$t"
[[ -f $t/$RELEASE_AGENT.sig ]] || fail "lab-agent has no signature of its own"
verified "$signers" "$t/$RELEASE_AGENT" || fail "lab-agent doesn't match its own signature by the release key"
grep -qaF -- "$(release_key "$signers")" "$t/$RELEASE_AGENT" || fail "lab-agent doesn't carry the release key"
tar -xzf "$dir/$RELEASE_MACOS_RUNNER" -C "$t"
for f in "${RELEASE_RUNNER_SIGNED[@]}"; do
  verified "$signers" "$t/$f" || fail "the macOS runner's ${f#lab-macos-runner/} doesn't match its own signature by the release key"
done
macho_arm64 "$t/lab-macos-runner/bin/lab-macos-runner" || fail "the macOS runner is not a program for macOS on arm64"
[[ -x $t/lab-macos-runner/install.sh && -f $t/lab-macos-runner/org.omarchy.maclab.macos-runner.plist ]] || fail "the macOS runner has no install.sh or LaunchDaemon plist"
tar -xzf "$dir/$RELEASE_CONTROLLER" -C "$t"
for f in bin/oobd install VERSION; do
  [[ -s $t/lab-controller/$f ]] || fail "$RELEASE_CONTROLLER has no $f"
done
if [[ -n $tag ]]; then
  [[ "$(<"$t/lab-macos-runner/VERSION")" == "$tag" ]] || fail "the macOS runner is $(<"$t/lab-macos-runner/VERSION"), not $tag"
  [[ "$(<"$t/lab-controller/VERSION")" == "$tag" ]] || fail "the controller is $(<"$t/lab-controller/VERSION"), not $tag"
  [[ "$(<"$t/lab-server/VERSION")" == "$tag" ]] || fail "the bundle is $(<"$t/lab-server/VERSION"), not $tag"
  [[ "$(<"$dir/VERSION")" == "${tag#v}" ]] || fail "VERSION is $(<"$dir/VERSION"), not ${tag#v}"
fi
echo "verified ${tag:-$(<"$t/lab-server/VERSION")}: ${#RELEASE_FILES[@]} files and lab-agent, against $signers"
