#!/usr/bin/env bash
# Build every file of a Lab release, unsigned: release/build.sh OUTDIR
#
#   OUTDIR/omarchy-m-test.tar.gz   the test tool; unpacks to omarchy-m-test/
#   OUTDIR/VERSION                 its number on its own, which the tool's
#                                  newer-release check reads
#   OUTDIR/lab-darwin-arm64, lab-linux-arm64, lab-linux-amd64
#   OUTDIR/lab-server-darwin-arm64.tar.gz
#                                  the mini's bundle; unpacks to lab-server/
#   OUTDIR/lab-macos-runner-darwin-arm64.tar.gz
#                                  the macOS runner for a lab Mac's macOS:
#                                  unpacks to lab-macos-runner/ (bin/,
#                                  install.sh, the LaunchDaemon's plist)
#   OUTDIR/lab-controller-darwin-arm64.tar.gz
#                                  a Mac lab controller; unpacks to
#                                  lab-controller/ (bin/oobd, bin/macvdmtool,
#                                  install, VERSION)
#   OUTDIR/bench-PROGRAM-darwin-arm64.tar.gz, bench-PROGRAM-linux-arm64.tar.gz
#                                  a benchmark program for macOS or for
#                                  Omarchy; unpacks to bench-PROGRAM/ (bin/,
#                                  BUILD.json, the licence, VERSION)
#   OUTDIR/.stage/                 the bundle unpacked, for release/sign.sh
#
# LAB_RELEASE is the tag being built, vX.Y.Z or vX.Y.Z-rc.N. It is the one
# number of a Lab release: stamped into omarchy-m-test (over TOOL_VERSION in
# cli/omarchy_m_test/__init__.py), lab and labd, and into lab-agent beside the
# agent's own. With no LAB_RELEASE the Go commands say "dev" and the test tool
# keeps the tree's number.
#
# RELEASE_ALLOWED_SIGNERS is the allowed_signers file whose key lab-agent
# carries (default: release/allowed_signers, the release key).
#
# RESET_TOOL is the reset tool as release/reset-tool.sh built it on macOS;
# it goes into the bundle as bin/macvdmtool. Without it the bundle has none
# and its Out-of-band controller refuses every reset: the release workflow
# always gives one.
#
# BENCH_PROGRAMS is the folder of benchmark programs release/bench.sh built:
# darwin-arm64/PROGRAM/ from the macOS runner and linux-arm64/PROGRAM/ from
# the arm64 Linux one. Every program of the release must be there, built for
# its system, with its BUILD.json and licence, or nothing is built. Without
# it the build has no benchmark archives, and release/sign.sh refuses it
# unless LAB_RELEASE_WITHOUT leaves them out (release/lib.sh).
#
# The tarball unpacks with bin/, the omarchy_m_test package (the feature
# catalogue bundled as omarchy_m_test/catalogue.json, Asahi's reference kernel
# config as omarchy_m_test/asahi-kernel/, omarchy-mac's check scripts under
# omarchy_m_test/vendor/, the on-screen video check under
# omarchy_m_test/testcard/) and VERSION. The bundle holds bin/ (labd, oobd and
# lab-builder for the mini, the reset tool, the lab-agent handed to Macs), builder/, deploy/,
# scripts/mini/, texts/ (the Volunteer's terms and the privacy statement, the
# files labd serves at /terms and /privacy), VERSION (the tag) and NOTES (the
# fixed lines of this number's release notes, from release/notes/X.Y.Z.txt). release/check.sh
# checks a built one. Signing happens separately (release/sign.sh), so this
# runs anywhere with Go, Python and GNU tar.
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
gnu_tools
die() { echo "release/build.sh: $*" >&2; exit 1; }

version="$(python3 -c 'import sys; sys.path.insert(0, sys.argv[1]); import omarchy_m_test as t; print(t.TOOL_VERSION)' "$repo/cli")"
tag="${LAB_RELEASE:-}"
if [[ -n $tag ]]; then
  [[ $tag =~ $RELEASE_TAG ]] || die "$tag is not a Lab release's tag (vX.Y.Z or vX.Y.Z-rc.N)"
  version="${tag#v}"
fi
tool="${RESET_TOOL:-}"
if [[ -n $tool ]]; then
  macho_arm64 "$tool" || die "RESET_TOOL ($tool) is not a program for macOS on arm64"
fi
bench="${BENCH_PROGRAMS:-}"
if [[ -n $bench ]]; then
  [[ -d $bench ]] || die "BENCH_PROGRAMS ($bench) is not a folder"
  bench="$(cd "$bench" && pwd)"
  for a in "${RELEASE_FILES[@]}"; do
    [[ $a =~ ^bench-(.+)-(darwin|linux)-arm64\.tar\.gz$ ]] || continue
    p="${BASH_REMATCH[1]}" sys="${BASH_REMATCH[2]}"
    d="$bench/$sys-arm64/$p"
    [[ -x $d/bin/$p && ! -L $d/bin/$p ]] || die "BENCH_PROGRAMS has no $sys-arm64/$p/bin/$p"
    if [[ $sys == darwin ]]; then
      macho_arm64 "$d/bin/$p" || die "BENCH_PROGRAMS: $sys-arm64/$p/bin/$p is not a program for macOS on arm64"
    else
      elf_arm64 "$d/bin/$p" || die "BENCH_PROGRAMS: $sys-arm64/$p/bin/$p is not a program for Linux on arm64"
    fi
    [[ -s $d/BUILD.json ]] || die "BENCH_PROGRAMS: $sys-arm64/$p has no BUILD.json"
    [[ -n "$(find "$d" -maxdepth 1 -type f \( -name 'LICENSE*' -o -name 'COPYING*' \))" ]] || die "BENCH_PROGRAMS: $sys-arm64/$p has no licence"
    [[ -z "$(find "$d" ! -type f ! -type d)" ]] || die "BENCH_PROGRAMS: $sys-arm64/$p holds something that is not a file or a folder"
  done
fi
signers="${RELEASE_ALLOWED_SIGNERS:-$repo/release/allowed_signers}"
key="$(release_key "$signers")"
[[ $key =~ ^[a-z0-9@.-]+\ [A-Za-z0-9+/=]+$ ]] || die "no key in $signers"

out="$(mkdir -p "$1" && cd "$1" && pwd)"
stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT
root="$stage/omarchy-m-test"
mkdir -p "$root/bin" "$root/omarchy_m_test"
install -m 0755 "$repo/cli/bin/omarchy-m-test" "$root/bin/omarchy-m-test"
install -m 0644 "$repo"/cli/omarchy_m_test/*.py "$root/omarchy_m_test/"
if [[ -n $tag ]]; then
  sed -E "s/^TOOL_VERSION = \".*\"\$/TOOL_VERSION = \"$version\"/" "$repo/cli/omarchy_m_test/__init__.py" > "$root/omarchy_m_test/__init__.py"
  grep -qxF "TOOL_VERSION = \"$version\"" "$root/omarchy_m_test/__init__.py" || die "couldn't stamp $version into the test tool"
fi
install -m 0644 "$repo/catalogue/catalogue.json" "$root/omarchy_m_test/catalogue.json"
mkdir -p "$root/omarchy_m_test/asahi-kernel"
install -m 0644 "$repo/catalogue/asahi-kernel/source.json" "$repo/catalogue/asahi-kernel/config" "$root/omarchy_m_test/asahi-kernel/"
mkdir -p "$root/omarchy_m_test/macos-reference"
install -m 0644 "$repo"/catalogue/macos-reference/*.json "$root/omarchy_m_test/macos-reference/"
# Vendored check scripts and the video test card script (omarchy_m_test/bundled.py runs them), modes kept.
(cd "$repo/cli/omarchy_m_test" && find vendor testcard gpu_native gl_spec -type f ! -name '*.pyc' ! -path '*/__pycache__/*' ! -path '*/bin/*' | sort | while read -r f; do
  install -D -m "$([[ -x $f ]] && echo 0755 || echo 0644)" "$f" "$root/omarchy_m_test/$f"
done)
# Native helper inclusion is explicit; source-only archives report it unsupported.
# Release CI builds one AArch64 helper, not a compiler/install step on users' Macs.
if [[ -n ${OMARCHY_GPU_BINARY_DIR:-} ]]; then
  python3 - "$OMARCHY_GPU_BINARY_DIR" "$repo/cli/omarchy_m_test/gpu_native" <<'PY'
import hashlib,json,pathlib,struct,sys
base=pathlib.Path(sys.argv[1]);p=base/'gpu-benchmark-vulkan';b=p.read_bytes();j=json.loads((base/'gpu-benchmark-build.json').read_text())
assert b[:6]==b'\x7fELF\x02\x01' and struct.unpack_from('<H',b,18)[0]==183,'release helper must be AArch64'
assert hashlib.sha256(b).hexdigest()==j['executable_sha256'] and j['suite']==1
src=pathlib.Path(sys.argv[2]);names=['benchmark.c','compute.comp','fullscreen.vert','raster.frag','texture.frag']
sources={name:hashlib.sha256((src/name).read_bytes()).hexdigest() for name in names}
assert sources==j['sources'],'helper was built from different packaged sources'
assert hashlib.sha256(json.dumps(sources,sort_keys=True,separators=(',',':')).encode()).hexdigest()==j['source_sha256']
PY
  install -Dm755 "$OMARCHY_GPU_BINARY_DIR/gpu-benchmark-vulkan" "$root/omarchy_m_test/gpu_native/bin/gpu-benchmark-vulkan"
  install -Dm644 "$OMARCHY_GPU_BINARY_DIR/gpu-benchmark-build.json" "$root/omarchy_m_test/gpu_native/bin/gpu-benchmark-build.json"
fi
printf '%s\n' "$version" > "$root/VERSION"

# Reproducible: fixed order, owners and times.
epoch="${SOURCE_DATE_EPOCH:-$(git -C "$repo" log -1 --format=%ct 2>/dev/null || echo 0)}"
pack "$stage" omarchy-m-test "$epoch" > "$out/omarchy-m-test.tar.gz"
printf '%s\n' "$version" > "$out/VERSION"

# lab for its three machines, and the mini's bundle.
rm -rf "$out/.stage"
server="$out/.stage/lab-server"
mkdir -p "$server"
make -s -C "$repo" release-binaries RELEASE_OUT="$out" LAB_RELEASE="${tag:-dev}" RELEASE_KEY="$key"
if [[ -n $tool ]]; then
  install -m 0755 "$tool" "$server/bin/macvdmtool"
else
  echo "release/build.sh: no RESET_TOOL: this bundle has no reset tool" >&2
fi
printf '%s\n' "${tag:-dev}" > "$server/VERSION"
cat "$repo/release/notes/${version%%-rc.*}.txt" 2>/dev/null > "$server/NOTES" || true
install -m 0644 "$repo/LICENSE" "$repo/THIRD_PARTY_NOTICES.md" "$server/"
# The Volunteer's terms and the privacy statement, as labd carries and serves them.
install -D -m 0644 "$repo/internal/web/texts/terms.txt" "$server/texts/terms.txt"
install -D -m 0644 "$repo/internal/web/texts/privacy.txt" "$server/texts/privacy.txt"
(cd "$repo" && bundled_sources | while read -r f; do
  install -D -m "$([[ -x $f ]] && echo 0755 || echo 0644)" "$f" "$server/$f"
done)
printf '%s\n' "$epoch" > "$out/.stage/epoch"
pack "$out/.stage" lab-server "$epoch" > "$out/$RELEASE_BUNDLE"
runner="$out/.stage/lab-macos-runner"
install -m 0755 "$repo/release/macos-runner/install.sh" "$runner/install.sh"
install -m 0644 "$repo/release/macos-runner/org.omarchy.maclab.macos-runner.plist" "$runner/"
install -m 0644 "$repo/LICENSE" "$runner/"
printf '%s\n' "${tag:-dev}" > "$runner/VERSION"
pack "$out/.stage" lab-macos-runner "$epoch" > "$out/$RELEASE_MACOS_RUNNER"

# A Mac lab controller: the same oobd and reset tool, and its install.
ctl="$stage/lab-controller"
install -D -m 0755 "$server/bin/oobd" "$ctl/bin/oobd"
if [[ -n $tool ]]; then install -m 0755 "$tool" "$ctl/bin/macvdmtool"; fi
install -m 0755 "$repo/scripts/controller/install" "$ctl/install"
install -m 0644 "$repo/LICENSE" "$repo/THIRD_PARTY_NOTICES.md" "$ctl/"
printf '%s\n' "${tag:-dev}" > "$ctl/VERSION"
pack "$stage" lab-controller "$epoch" > "$out/$RELEASE_CONTROLLER"

# The benchmark programs, each in its own archive with the tag beside it.
if [[ -n $bench ]]; then
  for a in "${RELEASE_FILES[@]}"; do
    [[ $a =~ ^bench-(.+)-(darwin|linux)-arm64\.tar\.gz$ ]] || continue
    p="${BASH_REMATCH[1]}" sys="${BASH_REMATCH[2]}"
    b="$stage/bench-$sys/bench-$p"
    mkdir -p "$b"
    cp -R "$bench/$sys-arm64/$p/." "$b/"
    find "$b" -type d -exec chmod 0755 {} +
    find "$b" -type f -exec chmod 0644 {} +
    chmod 0755 "$b/bin/$p"
    printf '%s\n' "${tag:-dev}" > "$b/VERSION"
    pack "$stage/bench-$sys" "bench-$p" "$epoch" > "$out/$a"
  done
elif [[ " ${RELEASE_FILES[*]} " == *" bench-"* ]]; then
  echo "release/build.sh: no BENCH_PROGRAMS: this build has no benchmark programs" >&2
fi

echo "built ${tag:-omarchy-m-test $version and a dev Lab release}, unsigned: $out"
