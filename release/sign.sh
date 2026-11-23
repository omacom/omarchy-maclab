#!/usr/bin/env bash
# Sign a Lab release built by release/build.sh: release/sign.sh DIR KEYFILE
#
# lab-agent is signed by itself first, inside the mini's bundle, and the macOS
# runner and its LaunchDaemon inside theirs, which are then packed again
# around those signatures. Then every file gets its .sig. DIR is
# left holding the Lab release and nothing else. Needs ssh-keygen and GNU tar;
# compiles nothing, so the release key never shares a job with a build.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
gnu_tools
die() { echo "release/sign.sh: $*" >&2; exit 1; }

[[ $# == 2 ]] || die "usage: release/sign.sh DIR KEYFILE"
dir="$(cd "$1" && pwd)"
key="$2"
[[ -f $key ]] || die "no key file $key"
[[ -f $dir/.stage/$RELEASE_AGENT && -f $dir/.stage/epoch && -d $dir/.stage/lab-macos-runner ]] || die "$dir is not a build of release/build.sh, or is signed already"
for f in "${RELEASE_FILES[@]}"; do
  [[ -s $dir/$f ]] || die "$dir has no $f"
done

sign "$key" "$dir/.stage/$RELEASE_AGENT"
chmod 0644 "$dir/.stage/$RELEASE_AGENT.sig"
pack "$dir/.stage" lab-server "$(<"$dir/.stage/epoch")" > "$dir/$RELEASE_BUNDLE"
for f in "${RELEASE_RUNNER_SIGNED[@]}"; do
  sign "$key" "$dir/.stage/$f"
  chmod 0644 "$dir/.stage/$f.sig"
done
pack "$dir/.stage" lab-macos-runner "$(<"$dir/.stage/epoch")" > "$dir/$RELEASE_MACOS_RUNNER"
rm -rf "$dir/.stage"
for f in "${RELEASE_FILES[@]}"; do
  sign "$key" "$dir/$f"
done
echo "signed ${#RELEASE_FILES[@]} files, lab-agent and the macOS runner: $dir"
