# What a Lab release is made of and how it is signed. Sourced by the scripts
# beside it; the two install scripts and Deploy verify the same way.
# shellcheck shell=bash

RELEASE_SIGNER="release@omarchy-m-testing.org"
RELEASE_NAMESPACE="omarchy-m-test-release"
RELEASE_BUNDLE="lab-server-darwin-arm64.tar.gz"
RELEASE_MACOS_RUNNER="lab-macos-runner-darwin-arm64.tar.gz"
# A Mac lab controller: oobd, the reset tool and the install script, for
# any Mac that resets Macs cabled to it. Unpacks to lab-controller/.
RELEASE_CONTROLLER="lab-controller-darwin-arm64.tar.gz"
# The benchmark programs the lab builds (release/bench.sh), each in an
# archive of its own, bench-PROGRAM-SYSTEM-arm64.tar.gz, unpacking to
# bench-PROGRAM/: for macOS, built on the macOS runner, and for Omarchy, on
# the arm64 Linux one.
RELEASE_BENCH_DARWIN=(coremark stream clpeak fio ane-bench smc-power mlx-bench)
RELEASE_BENCH_LINUX=(coremark stream clpeak fio)
release_bench() {
  local p
  for p in "${RELEASE_BENCH_DARWIN[@]}"; do printf 'bench-%s-darwin-arm64.tar.gz\n' "$p"; done
  for p in "${RELEASE_BENCH_LINUX[@]}"; do printf 'bench-%s-linux-arm64.tar.gz\n' "$p"; done
}
read -r -d '' -a RELEASE_BENCH < <(release_bench) || true
RELEASE_FILES=(omarchy-m-test.tar.gz VERSION lab-darwin-arm64 lab-linux-arm64 lab-linux-amd64 "$RELEASE_BUNDLE" "$RELEASE_MACOS_RUNNER" "$RELEASE_CONTROLLER" "${RELEASE_BENCH[@]}")

# LAB_RELEASE_WITHOUT: benchmark archives a release made on this machine
# leaves out, because this machine cannot build them (release/local.sh). No
# other file can be left out, and the release workflow leaves out none.
release_without() {
  local f keep=()
  for f in ${LAB_RELEASE_WITHOUT:-}; do
    [[ " ${RELEASE_BENCH[*]} " == *" $f "* ]] || { echo "LAB_RELEASE_WITHOUT: $f is not a benchmark program's archive" >&2; return 1; }
  done
  for f in "${RELEASE_FILES[@]}"; do
    [[ " ${LAB_RELEASE_WITHOUT:-} " == *" $f "* ]] || keep+=("$f")
  done
  RELEASE_FILES=("${keep[@]}")
}
release_without || return 1
RELEASE_AGENT="lab-server/bin/lab-agent-linux-arm64"
# What the macOS runner's install.sh checks against its own signature before
# it installs anything.
RELEASE_RUNNER_SIGNED=(lab-macos-runner/bin/lab-macos-runner lab-macos-runner/org.omarchy.maclab.macos-runner.plist)
RELEASE_TAG='^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'

# GNU install and tar: Homebrew keeps them out of PATH on macOS.
gnu_tools() {
  local d
  for d in coreutils gnu-tar; do
    if [[ -d /opt/homebrew/opt/$d/libexec/gnubin ]]; then PATH="/opt/homebrew/opt/$d/libexec/gnubin:$PATH"; fi
  done
  export PATH
  tar --version 2>/dev/null | grep -q 'GNU tar' || { echo "the release scripts need GNU tar and install (macOS: brew install gnu-tar coreutils)" >&2; return 1; }
}

# pack PARENT NAME EPOCH: PARENT/NAME as a tarball on stdout, the same bytes every time.
pack() { tar --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$3" -C "$1" -cf - "$2" | gzip -9n; }

# sha256: the checksum of standard input.
sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum; else shasum -a 256; fi | awk '{ print $1 }'
}

# macho_arm64 FILE: whether FILE is a program for macOS on arm64.
macho_arm64() { [[ "$(od -An -tx1 -N8 "$1" 2>/dev/null | tr -d ' \n')" == cffaedfe0c000001 ]]; }

# elf_arm64 FILE: whether FILE is a 64-bit program for Linux on arm64.
elf_arm64() { [[ "$(od -An -tx1 -N20 "$1" 2>/dev/null | tr -d ' \n')" =~ ^7f454c460201[0-9a-f]{24}b700$ ]]; }

# release_key FILE: the key of an allowed_signers file, as "type base64".
release_key() { awk '!/^#/ && NF { print $(NF-1), $NF; exit }' "$1"; }

# sign KEY FILE: FILE.sig, made new.
sign() {
  rm -f "$2.sig"
  ssh-keygen -q -Y sign -f "$1" -n "$RELEASE_NAMESPACE" "$2" </dev/null
}

# verified ALLOWED_SIGNERS FILE: whether FILE.sig is the release key's signature of FILE.
verified() {
  ssh-keygen -Y verify -f "$1" -I "$RELEASE_SIGNER" -n "$RELEASE_NAMESPACE" -s "$2.sig" < "$2" >/dev/null 2>&1
}

# bundled_sources: the files of builder/, deploy/ and scripts/mini/ that the
# mini's bundle carries, from the tree in the current folder, in order. Not
# deploy/mini.conf: it is the owner's own, git-ignored, and names the mini's
# address and the old lab's account.
bundled_sources() {
  local d
  for d in builder deploy scripts/mini; do
    if [[ -d $d ]]; then find "$d" -type f ! -name '*.pyc' ! -path '*/__pycache__/*' ! -name '.DS_Store' ! -path deploy/mini.conf; fi
  done | sort
}
