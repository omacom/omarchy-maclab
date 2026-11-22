#!/usr/bin/env bash
# Build a Lab release and check every file of it: release/check.sh
#
# Builds as a Release candidate of a number the tree doesn't have, signs with a key
# made here and thrown away, and fails unless:
#   - the test tool's tarball holds the launcher, every module of the package
#     (the number stamped into it), the bundled feature catalogue, Asahi's
#     reference kernel config, every vendored omarchy-mac check script and the
#     video test card script byte for byte (executable where the source is),
#     and the unpacked CLI finds its catalogue and scripts from where it was
#     unpacked;
#   - lab is there for macOS arm64, Linux arm64 and Linux x86-64 and says the tag;
#   - the server bundle holds labd, oobd and lab-builder for the mini, the
#     reset tool it was given, byte for byte, the lab-agent handed to Macs
#     with its own signature and the release key pinned inside it, and the
#     builder/, deploy/ and scripts/mini/ files, the two texts of /terms and
#     /privacy as labd carries them, and none of the commands that are not
#     shipped; a reset tool that is no macOS arm64 program is refused before
#     anything is built;
#   - every file has a signature the release key verifies, a changed file or a
#     file signed by another key is refused, and a tag that is not a Lab
#     release's is refused before anything is built;
#   - each benchmark program's archive holds the program, BUILD.json and
#     licence it was given (stand-ins here: the programs are built on the
#     release workflow's runners) and the tag; a program built for the
#     other system, a script, a missing program or BUILD.json, or a symlink
#     is refused before anything is built; and a release that leaves some
#     out verifies only under LAB_RELEASE_WITHOUT, which names nothing else.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
gnu_tools
t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

number="7.8.9-rc.7"
tag="v$number"

ssh-keygen -q -t ed25519 -N '' -C check -f "$t/key"
printf '%s namespaces="%s" %s\n' "$RELEASE_SIGNER" "$RELEASE_NAMESPACE" "$(cut -d' ' -f1,2 "$t/key.pub")" > "$t/allowed_signers"
key="$(release_key "$t/allowed_signers")"

for bad in v7.8 7.8.9 v7.8.9-rc v7.8.9-beta.1 "v7.8.9-rc.1 " latest; do
  if LAB_RELEASE="$bad" "$repo/release/build.sh" "$t/refused" >/dev/null 2>&1; then fail "built the tag $bad"; fi
done
[[ ! -e $t/refused/lab-linux-arm64 ]] || fail "a refused tag still built something"

# The reset tool is built on macOS alone; what stands in for it here has a
# macOS arm64 program's first bytes and nothing else.
printf '\xcf\xfa\xed\xfe\x0c\x00\x00\x01 not the reset tool\n' > "$t/macvdmtool"
printf '#!/bin/sh\n' > "$t/script"
if LAB_RELEASE="$tag" RESET_TOOL="$t/script" "$repo/release/build.sh" "$t/refused" >/dev/null 2>&1; then fail "took a script for the reset tool"; fi
[[ ! -e $t/refused/lab-linux-arm64 ]] || fail "a refused reset tool still built something"

# The benchmark programs are built on the release workflow's runners
# (release/bench.sh); what stands in for each here has its system's first
# bytes, a BUILD.json and a licence.
standin() { # standin DIR SYSTEM PROGRAM
  local d="$1/$2-arm64/$3"
  mkdir -p "$d/bin"
  if [[ $2 == darwin ]]; then
    printf '\xcf\xfa\xed\xfe\x0c\x00\x00\x01 stands in for %s\n' "$3" > "$d/bin/$3"
  else
    printf '\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\xb7\x00 stands in for %s\n' "$3" > "$d/bin/$3"
  fi
  chmod 0755 "$d/bin/$3"
  printf '{"program":"%s","system":"%s-arm64"}\n' "$3" "$2" > "$d/BUILD.json"
  printf 'the licence of %s\n' "$3" > "$d/LICENSE"
}
for p in "${RELEASE_BENCH_DARWIN[@]}"; do standin "$t/bench" darwin "$p"; done
for p in "${RELEASE_BENCH_LINUX[@]}"; do standin "$t/bench" linux "$p"; done
printf 'stands in for MLX kernels\n' > "$t/bench/darwin-arm64/mlx-bench/bin/mlx.metallib"
(( ${#RELEASE_BENCH[@]} == ${#RELEASE_BENCH_DARWIN[@]} + ${#RELEASE_BENCH_LINUX[@]} )) || fail "the benchmark archives are not one per program and system"

refused_bench() { # refused_bench WHY: BENCH_PROGRAMS=$t/bad is refused before anything is built
  if LAB_RELEASE="$tag" RESET_TOOL="$t/macvdmtool" BENCH_PROGRAMS="$t/bad" "$repo/release/build.sh" "$t/refused" >/dev/null 2>&1; then fail "built $1"; fi
  [[ ! -e $t/refused/lab-linux-arm64 ]] || fail "$1 was refused but still built something"
}
rm -rf "$t/bad" && cp -R "$t/bench" "$t/bad"
cp "$t/bench/darwin-arm64/stream/bin/stream" "$t/bad/linux-arm64/stream/bin/stream"
refused_bench "with a macOS program for Omarchy's STREAM"
rm -rf "$t/bad" && cp -R "$t/bench" "$t/bad"
cp "$t/script" "$t/bad/darwin-arm64/fio/bin/fio"
refused_bench "with a script for fio"
rm -rf "$t/bad" && cp -R "$t/bench" "$t/bad"
rm -rf "$t/bad/darwin-arm64/ane-bench"
refused_bench "without ane-bench"
rm -rf "$t/bad" && cp -R "$t/bench" "$t/bad"
rm "$t/bad/linux-arm64/coremark/BUILD.json"
refused_bench "a program with no BUILD.json"
rm -rf "$t/bad" && cp -R "$t/bench" "$t/bad"
ln -s /etc/passwd "$t/bad/darwin-arm64/smc-power/bin/passwd"
refused_bench "a program beside a symlink"
if LAB_RELEASE_WITHOUT="lab-linux-amd64" LAB_RELEASE="$tag" "$repo/release/build.sh" "$t/refused" >/dev/null 2>&1; then fail "left out lab-linux-amd64, which is not a benchmark program's archive"; fi
[[ ! -e $t/refused/lab-linux-arm64 ]] || fail "a refused LAB_RELEASE_WITHOUT still built something"

LAB_RELEASE="$tag" RELEASE_ALLOWED_SIGNERS="$t/allowed_signers" RESET_TOOL="$t/macvdmtool" BENCH_PROGRAMS="$t/bench" "$repo/release/build.sh" "$t/dist" >/dev/null
for f in "${RELEASE_FILES[@]}"; do
  [[ -s $t/dist/$f ]] || fail "the build left no $f"
done
[[ "$(<"$t/dist/VERSION")" == "$number" ]] || fail "VERSION says $(<"$t/dist/VERSION"), not $number"

# The test tool's tarball.
mkdir "$t/x"
tar -xzf "$t/dist/omarchy-m-test.tar.gz" -C "$t/x"
root="$t/x/omarchy-m-test"
pkg="$root/omarchy_m_test"

[[ -x $root/bin/omarchy-m-test ]] || fail "no executable bin/omarchy-m-test"
[[ "$(<"$root/VERSION")" == "$number" ]] || fail "the tarball's VERSION says $(<"$root/VERSION"), not $number"
cmp -s "$repo/catalogue/catalogue.json" "$pkg/catalogue.json" || fail "catalogue.json missing or different"
for f in source.json config; do
  cmp -s "$repo/catalogue/asahi-kernel/$f" "$pkg/asahi-kernel/$f" || fail "asahi-kernel/$f missing or different"
done
for f in "$repo"/cli/omarchy_m_test/*.py; do
  if [[ $(basename "$f") == __init__.py ]]; then
    sed -E "s/^TOOL_VERSION = \".*\"\$/TOOL_VERSION = \"$number\"/" "$f" | cmp -s - "$pkg/__init__.py" || fail "__init__.py is not the source with $number stamped in"
    continue
  fi
  cmp -s "$f" "$pkg/$(basename "$f")" || fail "module $(basename "$f") missing or different"
done
"$root/bin/omarchy-m-test" --version | grep -qxF "omarchy-m-test $number" || fail "the unpacked CLI doesn't say $number"
for f in "$repo"/catalogue/macos-reference/*.json; do
  cmp -s "$f" "$pkg/macos-reference/$(basename "$f")" || fail "macos-reference/$(basename "$f") missing or different"
done
vendored=0
while read -r f; do
  cmp -s "$repo/cli/omarchy_m_test/$f" "$pkg/$f" || fail "$f missing or different"
  if [[ -x $repo/cli/omarchy_m_test/$f && ! -x $pkg/$f ]]; then fail "$f lost its executable bit"; fi
  vendored=$((vendored + 1))
done < <(cd "$repo/cli/omarchy_m_test" && find vendor testcard gpu_native gl_spec -type f ! -name '*.pyc' ! -path '*/__pycache__/*' ! -path '*/bin/*' | sort)
(( vendored > 0 )) || fail "no vendored files found in the source tree"

# The unpacked CLI resolves its own data, not the repository's.
python3 - "$root" <<'PY' || fail "the unpacked CLI can't find its bundled files"
import os, sys
sys.path.insert(0, sys.argv[1])
from omarchy_m_test import bundled
assert bundled.catalogue_text()
assert all(bundled.asahi_kernel_config())
assert bundled.ASAHI_KERNEL_DIRS[0].startswith(sys.argv[1]) and os.path.isdir(bundled.ASAHI_KERNEL_DIRS[0])
for name in bundled.SCRIPTS:
    path = bundled.script_path(name)
    assert path.startswith(os.path.realpath(sys.argv[1])) or path.startswith(sys.argv[1]), path
assert os.path.isfile(os.path.join(os.path.dirname(bundled.script_path("video-card")), "testcard.lua"))
assert bundled.gpu_document_schema()
PY

# platform FILE: the system and processor a binary was built for.
platform() {
  python3 - "$1" <<'PY'
import struct, sys
b = open(sys.argv[1], "rb").read(32)
if b[:4] == b"\xcf\xfa\xed\xfe":
    print("darwin", {0x0100000c: "arm64", 0x01000007: "amd64"}.get(struct.unpack_from("<I", b, 4)[0], "?"))
elif b[:6] == b"\x7fELF\x02\x01":
    print("linux", {183: "arm64", 62: "amd64"}.get(struct.unpack_from("<H", b, 18)[0], "?"))
else:
    print("? ?")
PY
}
# runs FILE: whether this machine can run a binary built for FILE's platform.
runs() {
  local m
  m="$(uname -m)"
  case "$m" in aarch64) m=arm64 ;; x86_64) m=amd64 ;; esac
  [[ "$(platform "$1")" == "$(uname -s | tr '[:upper:]' '[:lower:]') $m" ]]
}
stamped() { grep -qaF -- "$2" "$1"; }

# lab, for each of its three machines.
ran=0
for p in darwin-arm64 linux-arm64 linux-amd64; do
  f="$t/dist/lab-$p"
  [[ "$(platform "$f")" == "${p/-/ }" ]] || fail "lab-$p was built for $(platform "$f")"
  stamped "$f" "$tag" || fail "lab-$p doesn't carry $tag"
  if runs "$f"; then
    [[ -x $f ]] || fail "lab-$p is not executable"
    # Not through head: lab says a second line, and a pipe closed before it
    # is written ends lab on SIGPIPE and this check with it, without a word.
    said="$(HOME="$t/nobody" XDG_CONFIG_HOME="$t/nobody/.config" MACLAB_URL= MACLAB_TOKEN= "$f" version)" || fail "lab-$p version failed"
    said="${said%%$'\n'*}"
    [[ $said == "lab $tag" ]] || fail "lab-$p version says $said, not lab $tag"
    ran=$((ran + 1))
  fi
done
(( ran == 1 )) || fail "no lab ran here to say its version"

bundled=0
bundle_check() { # bundle_check DIR: the unpacked bundle under DIR
  local s="$1/lab-server" f p
  [[ "$(<"$s/VERSION")" == "$tag" ]] || fail "the bundle's VERSION says $(<"$s/VERSION"), not $tag"
  for f in labd oobd lab-builder; do
    [[ -x $s/bin/$f ]] || fail "the bundle has no executable bin/$f"
    [[ "$(platform "$s/bin/$f")" == "darwin arm64" ]] || fail "the bundle's $f was built for $(platform "$s/bin/$f")"
  done
  [[ -x $s/bin/macvdmtool ]] || fail "the bundle has no executable bin/macvdmtool"
  cmp -s "$t/macvdmtool" "$s/bin/macvdmtool" || fail "the bundle's reset tool is not the one it was given"
  [[ -x $s/bin/lab-agent-linux-arm64 ]] || fail "the bundle has no executable bin/lab-agent-linux-arm64"
  [[ "$(platform "$s/bin/lab-agent-linux-arm64")" == "linux arm64" ]] || fail "the bundle's lab-agent was built for $(platform "$s/bin/lab-agent-linux-arm64")"
  stamped "$s/bin/labd" "$tag" || fail "labd doesn't carry $tag"
  stamped "$s/bin/labd" "$key" || fail "labd doesn't carry the release key its install line pins"
  stamped "$s/bin/lab-agent-linux-arm64" "$tag" || fail "lab-agent doesn't carry the Lab release it came from"
  stamped "$s/bin/lab-agent-linux-arm64" "$key" || fail "lab-agent doesn't carry the release key"
  if stamped "$s/bin/lab-agent-linux-arm64" "$(release_key "$repo/release/allowed_signers")"; then fail "lab-agent carries a second release key"; fi
  if runs "$s/bin/labd"; then
    [[ "$("$s/bin/labd" --version)" == "$tag" ]] || fail "labd --version says $("$s/bin/labd" --version), not $tag"
  fi
  if runs "$s/bin/lab-agent-linux-arm64"; then
    "$s/bin/lab-agent-linux-arm64" version | grep -qxF "Lab release $tag" || fail "lab-agent version doesn't name $tag"
  fi
  for f in LICENSE THIRD_PARTY_NOTICES.md; do
    cmp -s "$repo/$f" "$s/$f" || fail "the bundle's $f is missing or different"
  done
  for f in terms.txt privacy.txt; do
    cmp -s "$repo/internal/web/texts/$f" "$s/texts/$f" || fail "the bundle's texts/$f is missing or different"
    python3 -c 'import sys; sys.exit(open(sys.argv[2], "rb").read() not in open(sys.argv[1], "rb").read())' "$s/bin/labd" "$s/texts/$f" || fail "labd does not carry the shipped texts/$f"
  done
  bundled=0
  while read -r f; do
    cmp -s "$repo/$f" "$s/$f" || fail "the bundle's $f is missing or different"
    if [[ -x $repo/$f && ! -x $s/$f ]]; then fail "the bundle's $f lost its executable bit"; fi
    bundled=$((bundled + 1))
  done < <(cd "$repo" && bundled_sources)
  (( bundled > 0 )) || fail "no builder/ or deploy/ files found in the source tree"
  [[ ! -e $s/deploy/mini.conf ]] || fail "the bundle holds deploy/mini.conf, the owner's own file"
  [[ "$( (cd "$s" && find builder deploy scripts -type f 2>/dev/null) | wc -l | tr -d ' ')" == "$bundled" ]] || fail "the bundle holds files of builder/, deploy/ or scripts/ the tree doesn't"
  for p in lab-loop lab-esp lab-workflow lab-recovery-default; do
    [[ -z "$(find "$s" -name "$p*")" ]] || fail "the bundle ships $p"
  done
  [[ -z "$(find "$s" -name '__pycache__' -o -name '*.pyc' -o -name '.DS_Store')" ]] || fail "the bundle holds build leftovers"
}

# The server bundle as built: everything but the agent's signature.
mkdir "$t/unsigned"
tar -xzf "$t/dist/$RELEASE_BUNDLE" -C "$t/unsigned"
bundle_check "$t/unsigned"
[[ ! -e $t/unsigned/$RELEASE_AGENT.sig ]] || fail "an unsigned build carries a signature"
if "$repo/release/verify.sh" "$t/dist" "$t/allowed_signers" "$tag" >/dev/null 2>&1; then fail "an unsigned build verified"; fi

# The macOS runner: the program, its install script and LaunchDaemon.
mkdir "$t/runner"
tar -xzf "$t/dist/$RELEASE_MACOS_RUNNER" -C "$t/runner"
r="$t/runner/lab-macos-runner"
[[ "$(platform "$r/bin/lab-macos-runner")" == "darwin arm64" ]] || fail "the macOS runner was built for $(platform "$r/bin/lab-macos-runner")"
[[ "$(<"$r/VERSION")" == "$tag" ]] || fail "the macOS runner's VERSION says $(<"$r/VERSION"), not $tag"
cmp -s "$repo/release/macos-runner/install.sh" "$r/install.sh" && [[ -x $r/install.sh ]] || fail "the macOS runner's install.sh is missing, different or not executable"
cmp -s "$repo/release/macos-runner/org.omarchy.maclab.macos-runner.plist" "$r/org.omarchy.maclab.macos-runner.plist" || fail "the macOS runner's LaunchDaemon plist is missing or different"

# The Mac lab controller: oobd and the reset tool the bundle carries, and its install.
mkdir "$t/ctl"
tar -xzf "$t/dist/$RELEASE_CONTROLLER" -C "$t/ctl"
for f in bin/oobd bin/macvdmtool install; do
  [[ -x $t/ctl/lab-controller/$f ]] || fail "the controller has no executable $f"
done
cmp -s "$t/ctl/lab-controller/bin/oobd" "$t/unsigned/lab-server/bin/oobd" || fail "the controller's oobd is not the bundle's"
cmp -s "$t/ctl/lab-controller/bin/macvdmtool" "$t/macvdmtool" || fail "the controller's reset tool is not the one given"
cmp -s "$t/ctl/lab-controller/install" "$repo/scripts/controller/install" || fail "the controller's install is not scripts/controller/install"
[[ "$(<"$t/ctl/lab-controller/VERSION")" == "$tag" ]] || fail "the controller's VERSION is not $tag"

# The benchmark programs: each archive holds its program, BUILD.json and
# licence as given, byte for byte, and the tag; nothing else.
for a in "${RELEASE_BENCH[@]}"; do
  [[ $a =~ ^bench-(.+)-(darwin|linux)-arm64\.tar\.gz$ ]] || fail "$a is not bench-PROGRAM-SYSTEM-arm64.tar.gz"
  p="${BASH_REMATCH[1]}" sys="${BASH_REMATCH[2]}"
  rm -rf "$t/b" && mkdir "$t/b"
  tar -xzf "$t/dist/$a" -C "$t/b"
  [[ "$(ls -A "$t/b")" == "bench-$p" ]] || fail "$a does not unpack to bench-$p/ alone"
  b="$t/b/bench-$p" given="$t/bench/$sys-arm64/$p"
  [[ -x $b/bin/$p ]] || fail "$a has no executable bin/$p"
  [[ "$(platform "$b/bin/$p")" == "$sys arm64" ]] || fail "$a's $p was built for $(platform "$b/bin/$p")"
  [[ "$(<"$b/VERSION")" == "$tag" ]] || fail "$a's VERSION says $(<"$b/VERSION"), not $tag"
  while read -r f; do
    cmp -s "$given/$f" "$b/$f" || fail "$a's $f is not the one given"
  done < <(cd "$given" && find . -type f | sort)
  [[ "$(cd "$b" && find . -type f ! -path ./VERSION | sort)" == "$(cd "$given" && find . -type f | sort)" ]] || fail "$a holds files it was not given"
done
[[ -f $t/b/bench-$p/bin/$p ]] || fail "no benchmark archive was checked"
tar -tzf "$t/dist/bench-mlx-bench-darwin-arm64.tar.gz" | grep -qx bench-mlx-bench/bin/mlx.metallib || fail "mlx-bench has no mlx.metallib beside it"

# The fixed lines of the release notes.
while read -r line; do
  [[ $line =~ ^(No\ way\ back\.|Needs\ lab(-agent)?\ [0-9]+\.[0-9]+\.[0-9]+\ or\ later\.|Pins\ changed:\ needs\ a\ Maintenance\ run\.)$ ]] || fail "release/notes: not one of the fixed lines: $line"
done < <(cat "$repo"/release/notes/*.txt 2>/dev/null)
cmp -s "$t/unsigned/lab-server/NOTES" <(cat "$repo/release/notes/7.8.9.txt" 2>/dev/null) || fail "the bundle's NOTES are not release/notes/7.8.9.txt"

# Signed: the same bytes when signed again, and every file verifies.
cp -R "$t/dist" "$t/again"
"$repo/release/sign.sh" "$t/dist" "$t/key" >/dev/null
"$repo/release/sign.sh" "$t/again" "$t/key" >/dev/null
for f in "${RELEASE_FILES[@]}"; do
  cmp -s "$t/dist/$f" "$t/again/$f" || fail "$f differs between two signings of one build"
  [[ -s $t/dist/$f.sig ]] || fail "$f has no signature"
done
[[ "$(ls "$t/dist" | wc -l | tr -d ' ')" == "$(( ${#RELEASE_FILES[@]} * 2 ))" ]] || fail "a signed release holds something else: $(ls "$t/dist" | tr '\n' ' ')"
"$repo/release/verify.sh" "$t/dist" "$t/allowed_signers" "$tag" >/dev/null || fail "a signed release doesn't verify"
mkdir "$t/signed"
tar -xzf "$t/dist/$RELEASE_BUNDLE" -C "$t/signed"
bundle_check "$t/signed"
verified "$t/allowed_signers" "$t/signed/$RELEASE_AGENT" || fail "lab-agent's own signature doesn't verify"

# Refused: the real key's pin, another tag, a changed file, a missing signature,
# a stray file, another key, and an agent that is not the one signed.
refused() { # refused WHY DIR [ALLOWED_SIGNERS [TAG]]
  if "$repo/release/verify.sh" "$2" "${3:-$t/allowed_signers}" "${4:-$tag}" >/dev/null 2>&1; then fail "verified: $1"; fi
}
refused "against the pinned release key" "$t/dist" "$repo/release/allowed_signers"
refused "as another tag" "$t/dist" "$t/allowed_signers" v7.8.9-rc.8
for f in "${RELEASE_FILES[@]}"; do
  rm -rf "$t/bad" && cp -R "$t/dist" "$t/bad"
  printf 'x' >> "$t/bad/$f"
  refused "a changed $f" "$t/bad"
  rm -rf "$t/bad" && cp -R "$t/dist" "$t/bad"
  rm "$t/bad/$f.sig"
  refused "$f with no signature" "$t/bad"
done
rm -rf "$t/bad" && cp -R "$t/dist" "$t/bad"
touch "$t/bad/lab-windows-amd64"
refused "a file that is not a Lab release's" "$t/bad"

ssh-keygen -q -t ed25519 -N '' -C other -f "$t/other"
rm -rf "$t/bad" && cp -R "$t/dist" "$t/bad"
for f in "${RELEASE_FILES[@]}"; do sign "$t/other" "$t/bad/$f"; done
refused "a release signed by another key" "$t/bad"

# The bundle signed by the release key around an agent signed by another.
rm -rf "$t/bad" && cp -R "$t/dist" "$t/bad" && mkdir "$t/stage"
tar -xzf "$t/bad/$RELEASE_BUNDLE" -C "$t/stage"
sign "$t/other" "$t/stage/$RELEASE_AGENT"
pack "$t/stage" lab-server 0 > "$t/bad/$RELEASE_BUNDLE"
sign "$t/key" "$t/bad/$RELEASE_BUNDLE"
refused "an agent signed by another key" "$t/bad"

# A release made on a machine that cannot build every benchmark program
# (release/local.sh) leaves those archives out under LAB_RELEASE_WITHOUT;
# without it, the same release is refused.
partial="bench-mlx-bench-darwin-arm64.tar.gz bench-clpeak-linux-arm64.tar.gz"
LAB_RELEASE_WITHOUT="$partial" LAB_RELEASE="$tag" RELEASE_ALLOWED_SIGNERS="$t/allowed_signers" RESET_TOOL="$t/macvdmtool" BENCH_PROGRAMS="$t/bench" "$repo/release/build.sh" "$t/partial" >/dev/null 2>&1
for a in $partial; do
  [[ ! -e $t/partial/$a ]] || fail "LAB_RELEASE_WITHOUT left $a in"
done
[[ -s $t/partial/bench-clpeak-darwin-arm64.tar.gz && -s $t/partial/bench-stream-linux-arm64.tar.gz ]] || fail "LAB_RELEASE_WITHOUT left out more than it named"
LAB_RELEASE_WITHOUT="$partial" "$repo/release/sign.sh" "$t/partial" "$t/key" >/dev/null
LAB_RELEASE_WITHOUT="$partial" "$repo/release/verify.sh" "$t/partial" "$t/allowed_signers" "$tag" >/dev/null || fail "a release that leaves out $partial doesn't verify under LAB_RELEASE_WITHOUT"
refused "a release without $partial, as a whole one" "$t/partial"

echo "release ok: $tag; the test tool ($(ls "$pkg"/*.py | wc -l | tr -d ' ') modules, catalogue, Asahi kernel config and $vendored vendored, test card and GPU source/data files), lab for three machines, the server bundle ($bundled builder, deploy and mini files), ${#RELEASE_BENCH[@]} benchmark programs; ${#RELEASE_FILES[@]} files signed, verified and refused when changed"
