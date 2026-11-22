#!/usr/bin/env bash
# Make a Lab release on this machine: release/local.sh TAG
#
# Does what .github/workflows/release.yml does (build every file, sign
# lab-agent and then every file, verify as the install lines and Deploy do),
# with two differences. The key is made here in a temporary folder and
# thrown away: nothing is tagged, nothing leaves this machine and the release
# key is never read. And the benchmark programs are built only where they
# can be (release/bench.sh fetches their pinned sources): the macOS ones on
# a Mac on Apple silicon with Xcode (mlx-bench also needs the Metal
# toolchain; clpeak and mlx-bench, CMake), the Omarchy ones on arm64 Linux
# with clang, CMake and libOpenCL. Each archive this machine cannot build is
# left out, with its reason; LAB_RELEASE_WITHOUT names more to leave out.
#
#   dist/release/TAG/                  the Lab release: every file and its .sig
#   dist/release/TAG.allowed_signers   the public half of the key that signed
#                                      it; this build's lab-agent carries it
#
# Whoever installs this build pins that file in place of release/allowed_signers.
# Running it again signs with a new key. The test tool's tarball has no GPU
# helper unless OMARCHY_GPU_BINARY_DIR names one built on AArch64 Linux.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
die() { echo "release/local.sh: $*" >&2; exit 1; }

[[ $# == 1 ]] || die "usage: release/local.sh TAG   (vX.Y.Z or vX.Y.Z-rc.N)"
tag="$1"
[[ $tag =~ $RELEASE_TAG ]] || die "$tag is not a Lab release's tag (vX.Y.Z or vX.Y.Z-rc.N)"

t="$(mktemp -d)"
trap 'rm -rf "$t"' EXIT

# cannot SYSTEM PROGRAM: why this machine cannot build PROGRAM for SYSTEM;
# nothing when it can.
cannot() {
  case "$1-$(uname -s)-$(uname -m)" in
    darwin-Darwin-arm64)
      xcodebuild -version >/dev/null 2>&1 || { echo "no Xcode here"; return; }
      if [[ $2 == mlx-bench ]] && ! xcrun -sdk macosx metal --version >/dev/null 2>&1; then
        echo "no Metal toolchain here (Xcode 26: xcodebuild -downloadComponent MetalToolchain)"
        return
      fi ;;
    linux-Linux-aarch64 | linux-Linux-arm64)
      command -v "${CC:-clang}" >/dev/null || { echo "no ${CC:-clang} here"; return; }
      if [[ $2 == clpeak ]] && ! ls /usr/lib/aarch64-linux-gnu/libOpenCL.so /usr/lib/libOpenCL.so /usr/lib64/libOpenCL.so >/dev/null 2>&1; then
        echo "no libOpenCL.so here"
        return
      fi ;;
    darwin-*) echo "it is built on macOS on Apple silicon"; return ;;
    linux-*) echo "it is built on arm64 Linux"; return ;;
  esac
  if [[ $2 == clpeak || $2 == mlx-bench ]] && ! command -v cmake >/dev/null; then echo "no cmake here"; fi
}

without=()
for f in ${LAB_RELEASE_WITHOUT:-}; do without+=("$f"); done
built=0
for system in darwin linux; do
  if [[ $system == darwin ]]; then progs=("${RELEASE_BENCH_DARWIN[@]}"); else progs=("${RELEASE_BENCH_LINUX[@]}"); fi
  here=()
  for p in "${progs[@]}"; do
    a="bench-$p-$system-arm64.tar.gz"
    [[ " ${without[*]-} " != *" $a "* ]] || continue
    why="$(cannot "$system" "$p")"
    if [[ -n $why ]]; then
      echo "release/local.sh: leaving out $a: $why" >&2
      without+=("$a")
    else
      here+=("$p")
    fi
  done
  if (( ${#here[@]} )); then
    "$repo/release/bench.sh" "$system" "$t/bench/$system-arm64" "${here[@]}"
    built=1
  fi
done
export LAB_RELEASE_WITHOUT="${without[*]-}"
release_without
if (( built )); then export BENCH_PROGRAMS="$t/bench"; fi

ssh-keygen -q -t ed25519 -N '' -C "throwaway key for a local $tag" -f "$t/key"
printf '%s namespaces="%s" %s\n' "$RELEASE_SIGNER" "$RELEASE_NAMESPACE" "$(cut -d' ' -f1,2 "$t/key.pub")" > "$t/allowed_signers"

LAB_RELEASE="$tag" RELEASE_ALLOWED_SIGNERS="$t/allowed_signers" "$repo/release/build.sh" "$t/dist"
"$repo/release/sign.sh" "$t/dist" "$t/key"
rm -f "$t/key"
"$repo/release/verify.sh" "$t/dist" "$t/allowed_signers" "$tag"

out="$repo/dist/release"
mkdir -p "$out"
rm -rf "${out:?}/$tag" "${out:?}/$tag.allowed_signers"
mv "$t/dist" "$out/$tag"
install -m 0644 "$t/allowed_signers" "$out/$tag.allowed_signers"

echo
echo "A local $tag, signed with a throwaway key:"
(cd "$out/$tag" && for f in "${RELEASE_FILES[@]}"; do printf '  %10s  %s\n' "$(wc -c < "$f" | tr -d ' ')" "$f"; done)
echo "in $out/$tag, each with its .sig"
[[ -z $LAB_RELEASE_WITHOUT ]] || echo "left out: $LAB_RELEASE_WITHOUT"
echo "pin: $out/$tag.allowed_signers"
