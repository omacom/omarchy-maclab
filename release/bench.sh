#!/usr/bin/env bash
# Build the benchmark programs of a Lab release: release/bench.sh SYSTEM OUTDIR [PROGRAM...]
#
# SYSTEM is darwin, on the release workflow's macOS runner (CoreMark,
# STREAM, clpeak, fio and the lab's own ane-bench, smc-power and
# mlx-bench), or linux, on its arm64 Linux runner (CoreMark, STREAM and
# clpeak, for Omarchy). With PROGRAMs, only those.
#
# Each upstream source is fetched from its line in release/bench-pins and
# refused unless its checksum is the pinned one; the program is built from
# exactly what was checked, with clang -O2. The lab's own three are built
# from tools/bench-macos/. Leaves OUTDIR/PROGRAM/: bin/PROGRAM (mlx-bench
# with its mlx.metallib), BUILD.json (the sources and their checksums, the
# compiler, Xcode and SDK, the flags, the program's own sha256) and the
# source's licence. release/build.sh packs each into
# bench-PROGRAM-SYSTEM-arm64.tar.gz (BENCH_PROGRAMS).
#
# CC and CXX name the compilers (default clang and clang++); on macOS
# DEVELOPER_DIR picks the Xcode, and BENCH_XCODE, when set, is the
# `xcodebuild -version` first line it must be ("Xcode 16.4"). BENCH_PINS
# names another pins file, which may name sources on this machine
# (file:///...): the tests'.
#
# Settled here, for both systems alike:
#   CoreMark  built for up to 64 threads (-DMULTITHREAD=64, pthreads); the
#             first argument M<n> picks n at run time, M1 for one core.
#   STREAM    arrays of 2^26 doubles, 512 MiB each: four times 128 MiB,
#             above the largest cache of any Apple Silicon chip (96 MiB).
#             stream.c takes its size at compile time only. No OpenMP:
#             Apple's clang has none, so one thread on both systems.
#   clpeak    the OpenCL backend alone, against the system's OpenCL.
#   fio       --disable-optimizations (no -O3, no -march=native), then -O2;
#             --disable-shm, as macOS allows too little System V shared memory
#             for 32 workers: they are threads (--thread).
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=release/lib.sh
. "$repo/release/lib.sh"
die() { echo "release/bench.sh: $*" >&2; exit 1; }

STREAM_ARRAY_SIZE=67108864
COREMARK_THREADS=64
MACOS_MIN=13.0
MLX_MACOS_MIN=14.0

[[ $# -ge 2 ]] || die "usage: release/bench.sh darwin|linux OUTDIR [PROGRAM...]"
system="$1"
shift
case "$system" in
  darwin) all=("${RELEASE_BENCH_DARWIN[@]}") ;;
  linux) all=("${RELEASE_BENCH_LINUX[@]}") ;;
  *) die "$system is not darwin or linux" ;;
esac
outarg="$1"
shift
programs=("$@")
(( ${#programs[@]} )) || programs=("${all[@]}")
for p in "${programs[@]}"; do
  [[ " ${all[*]} " == *" $p "* ]] || die "$p is not a $system benchmark program (${all[*]})"
done

host="$(uname -s)-$(uname -m)"
case "$system-$host" in
  darwin-Darwin-arm64 | linux-Linux-aarch64 | linux-Linux-arm64) ;;
  *) die "$system programs are built on $system arm64, not on $host" ;;
esac

CC="${CC:-clang}"
CXX="${CXX:-clang++}"
command -v "$CC" >/dev/null || die "no C compiler $CC"
if [[ $system == darwin && -n ${BENCH_XCODE:-} ]]; then
  got="$(xcodebuild -version | head -1)"
  [[ $got == "$BENCH_XCODE" ]] || die "this is $got, not the pinned $BENCH_XCODE: nothing was built"
fi

pins="${BENCH_PINS:-$repo/release/bench-pins}"
[[ -f $pins ]] || die "no pins file $pins"
out="$(mkdir -p "$outarg" && cd "$outarg" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 2)"

json() { # json STRING: STRING as a JSON string
  local s="${1//\\/\\\\}"
  s="${s//\"/\\\"}"
  printf '"%s"' "${s//$'\t'/ }"
}

# pin NAME: the pinned version, sha256 and url of NAME, checked, into
# PIN_VERSION, PIN_SUM and PIN_URL.
pin() {
  local rest lines
  lines="$(sed -e 's/#.*//' "$pins" | awk -v n="$1" '$1 == n')"
  [[ -n $lines ]] || die "$pins has no line for $1"
  [[ $lines != *$'\n'* ]] || die "$pins has more than one line for $1"
  read -r _ PIN_VERSION PIN_SUM PIN_URL rest <<< "$lines"
  [[ -z ${rest:-} && -n ${PIN_URL:-} ]] || die "$pins: the line of $1 is not: name version sha256 url"
  [[ $PIN_SUM =~ ^[0-9a-f]{64}$ ]] || die "$pins: $1's checksum is not a sha256"
  case "$PIN_URL" in
    https://*) ;;
    file:///*) [[ -n ${BENCH_PINS:-} ]] || die "$pins: $PIN_URL is not https" ;;
    *) die "$pins: $PIN_URL is not https" ;;
  esac
}

# fetch NAME: the pinned source of NAME, checked, unpacked into `src NAME`.
# A git source is the commit's `git archive`.
SOURCES=()
src() { echo "$work/src/$1"; }
fetch() {
  local got dir git="$work/git/$1" dl="$work/$1.download"
  dir="$(src "$1")"
  pin "$1"
  rm -rf "$dir" "$git" && mkdir -p "$dir"
  if [[ $PIN_VERSION =~ ^[0-9a-f]{40}$ ]]; then
    git clone --quiet --no-checkout --filter=blob:none "$PIN_URL" "$git" 2>/dev/null || git clone --quiet --no-checkout "$PIN_URL" "$git" || die "could not fetch $PIN_URL"
    git -C "$git" cat-file -e "$PIN_VERSION^{commit}" 2>/dev/null || die "$PIN_URL has no commit $PIN_VERSION: nothing was built"
    git -C "$git" -c tar.umask=0002 -c core.autocrlf=false archive --format=tar "$PIN_VERSION" > "$dl"
    got="$(sha256 < "$dl")"
    [[ $got == "$PIN_SUM" ]] || die "$1 at $PIN_VERSION has the checksum $got, not the pinned $PIN_SUM: nothing was built"
    tar -xf "$dl" -C "$dir"
  else
    curl -fsSL --retry 3 -o "$dl" "$PIN_URL" || die "could not fetch $PIN_URL"
    got="$(sha256 < "$dl")"
    [[ $got == "$PIN_SUM" ]] || die "$PIN_URL has the checksum $got, not the pinned $PIN_SUM: nothing was built"
    case "$PIN_URL" in
      *.zip) (cd "$dir" && unzip -q "$dl") ;;
      *.tar.xz) tar -xJf "$dl" -C "$dir" ;;
      *) cp "$dl" "$dir/$(basename "$PIN_URL")" ;;
    esac
  fi
  SOURCES+=("{\"name\":$(json "$1"),\"version\":$(json "$PIN_VERSION"),\"sha256\":\"$PIN_SUM\",\"url\":$(json "$PIN_URL")}")
}

# own NAME: the lab's own source of NAME; its checksum is the sha256 of
# `shasum -a 256` over its files, as for a model.
own() {
  local dir="$repo/tools/bench-macos/$1" sum
  [[ -d $dir ]] || die "no source for $1 in tools/bench-macos"
  sum="$(cd "$dir" && find . -type f | LC_ALL=C sort | while read -r f; do printf '%s  %s\n' "$(sha256 < "$f")" "$f"; done | sha256)"
  SOURCES+=("{\"name\":$(json "$1"),\"path\":$(json "tools/bench-macos/$1"),\"sha256\":\"$sum\"}")
}

first_line() { "$@" 2>&1 | head -1; }

# finish NAME FLAGS LICENCE...: the program built into $dest/bin, its
# licences beside it, and BUILD.json.
finish() {
  local p="$1" flags="$2" bin="$dest/bin/$1" f toolchain sources
  shift 2
  [[ -x $bin ]] || die "$p was not built"
  case "$system" in
    darwin) macho_arm64 "$bin" || die "what was built for $p is not a program for macOS on arm64" ;;
    linux) elf_arm64 "$bin" || die "what was built for $p is not a program for Linux on arm64" ;;
  esac
  for f in "$@"; do install -m 0644 "$f" "$dest/"; done
  toolchain="\"cc\":$(json "$(first_line "$CC" --version)")"
  if [[ $system == darwin ]]; then
    toolchain+=",\"xcode\":$(json "$(xcodebuild -version | tr '\n' ' ' | sed 's/ *$//')")"
    toolchain+=",\"sdk\":$(json "macosx $(xcrun --sdk macosx --show-sdk-version)")"
    toolchain+=",\"builder\":$(json "macOS $(sw_vers -productVersion) ($(sw_vers -buildVersion))")"
    [[ $p != ane-bench ]] || toolchain+=",\"swiftc\":$(json "$(first_line xcrun swiftc --version)")"
  else
    # shellcheck disable=SC1091
    toolchain+=",\"builder\":$(json "$(. /etc/os-release && echo "$PRETTY_NAME")")"
  fi
  [[ $p != clpeak && $p != mlx-bench ]] || toolchain+=",\"cmake\":$(json "$(first_line cmake --version)")"
  sources="$(IFS=,; echo "${SOURCES[*]}")"
  printf '{"program":%s,"system":"%s-arm64","sources":[%s],%s,"flags":%s,"sha256":"%s"}\n' \
    "$(json "$p")" "$system" "$sources" "$toolchain" "$(json "$flags")" "$(sha256 < "$bin")" > "$dest/BUILD.json"
  echo "built $p for $system arm64: $out/$p, sha256 $(sha256 < "$bin")" >&2
}

build_coremark() {
  local s port=linux xc="-DMULTITHREAD=$COREMARK_THREADS -DUSE_PTHREAD -pthread"
  fetch coremark
  s="$(src coremark)"
  if [[ $system == darwin ]]; then
    port=macos
    xc+=" -mmacosx-version-min=$MACOS_MIN"
  fi
  make -C "$s" PORT_DIR="$port" CC="$CC" PORT_CFLAGS=-O2 XCFLAGS="$xc" compile >&2
  install -m 0755 "$s/coremark.exe" "$dest/bin/coremark"
  finish coremark "-O2 $xc" "$s/LICENSE.md"
}

build_stream() {
  local flags="-O2 -DSTREAM_ARRAY_SIZE=$STREAM_ARRAY_SIZE"
  fetch stream
  fetch stream-licence
  [[ $system == linux ]] || flags+=" -mmacosx-version-min=$MACOS_MIN"
  # shellcheck disable=SC2086
  "$CC" $flags "$(src stream)/stream.c" -o "$dest/bin/stream" >&2
  finish stream "$flags" "$(src stream-licence)/LICENSE.txt"
}

build_clpeak() {
  local s headers clhpp b="$work/build/clpeak" args=() o flags="-O2 -DNDEBUG"
  fetch clpeak
  fetch opencl-headers
  fetch opencl-clhpp
  s="$(src clpeak)"
  headers="$(src opencl-headers)"
  clhpp="$(src opencl-clhpp)"
  # Apple's OpenCL headers lack this one Khronos error code, which clpeak
  # compares against; -1001 is its value in Khronos's cl_ext.h.
  [[ $system == linux ]] || flags+=" -DCL_PLATFORM_NOT_FOUND_KHR=-1001"
  args=(-DCMAKE_BUILD_TYPE=Release -DCMAKE_C_COMPILER="$CC" -DCMAKE_CXX_COMPILER="$CXX"
    "-DCMAKE_C_FLAGS_RELEASE=$flags" "-DCMAKE_CXX_FLAGS_RELEASE=$flags"
    -DHPP_FOUND="$clhpp/include")
  for o in VULKAN CUDA ROCM METAL ONEAPI CPU ONNX COREML LITERT GUI; do args+=("-DCLPEAK_ENABLE_$o=OFF"); done
  if [[ $system == darwin ]]; then
    args+=(-DCMAKE_OSX_DEPLOYMENT_TARGET="$MACOS_MIN")
  else
    args+=(-DOpenCL_INCLUDE_DIR="$headers" -DOpenCL_LIBRARY="$(linux_opencl)")
  fi
  cmake -S "$s" -B "$b" "${args[@]}" >&2
  cmake --build "$b" --parallel "$jobs" --target clpeak >&2
  install -m 0755 "$(find "$b" -type f -name clpeak -perm -u+x | head -1)" "$dest/bin/clpeak"
  finish clpeak "$flags; the OpenCL backend alone" "$s/LICENSE"
}

linux_opencl() {
  local f
  for f in /usr/lib/aarch64-linux-gnu/libOpenCL.so /usr/lib/libOpenCL.so /usr/lib64/libOpenCL.so; do
    [[ -e $f ]] && { echo "$f"; return; }
  done
  die "no libOpenCL.so to link clpeak against (Ubuntu: ocl-icd-opencl-dev)"
}

build_fio() {
  local s cflags="-O2 -mmacosx-version-min=$MACOS_MIN"
  fetch fio
  s="$(src fio)"
  (cd "$s" && ./configure --cc="$CC" --disable-optimizations --disable-shm --extra-cflags="$cflags" >&2 && make -j "$jobs" fio >&2)
  install -m 0755 "$s/fio" "$dest/bin/fio"
  finish fio "--disable-optimizations --disable-shm --extra-cflags=$cflags" "$s/COPYING" "$s/MORAL-LICENSE"
}

build_ane_bench() {
  local flags="-O -parse-as-library -target arm64-apple-macos$MACOS_MIN"
  own ane-bench
  # shellcheck disable=SC2086
  xcrun swiftc $flags "$repo/tools/bench-macos/ane-bench/main.swift" -o "$dest/bin/ane-bench" >&2
  finish ane-bench "swiftc $flags" "$repo/LICENSE"
}

build_smc_power() {
  local flags="-O2 -mmacosx-version-min=$MACOS_MIN"
  own smc-power
  # shellcheck disable=SC2086
  "$CC" $flags "$repo/tools/bench-macos/smc-power/smc-power.c" -framework IOKit -framework CoreFoundation -o "$dest/bin/smc-power" >&2
  finish smc-power "$flags" "$repo/LICENSE"
}

build_mlx_bench() {
  local s mlx metal json fmt b="$work/build/mlx" prefix="$work/mlx" flags="-O2 -std=c++20 -mmacosx-version-min=$MLX_MACOS_MIN"
  metal_toolchain || die "no Metal toolchain in $(xcode-select -p): mlx-bench needs it for MLX's kernels (Xcode 26: xcodebuild -downloadComponent MetalToolchain)"
  own mlx-bench
  for s in mlx metal-cpp nlohmann-json fmt; do fetch "$s"; done
  mlx="$(src mlx)"
  metal="$(src metal-cpp)"
  json="$(src nlohmann-json)"
  fmt="$(src fmt)"
  cmake -S "$mlx" -B "$b" -DCMAKE_BUILD_TYPE=Release -DCMAKE_C_COMPILER="$CC" -DCMAKE_CXX_COMPILER="$CXX" \
    -DCMAKE_OSX_DEPLOYMENT_TARGET="$MLX_MACOS_MIN" -DCMAKE_INSTALL_PREFIX="$prefix" -DBUILD_SHARED_LIBS=OFF \
    -DMLX_BUILD_TESTS=OFF -DMLX_BUILD_EXAMPLES=OFF -DMLX_BUILD_BENCHMARKS=OFF -DMLX_BUILD_PYTHON_BINDINGS=OFF \
    -DMLX_BUILD_GGUF=OFF -DMLX_BUILD_SAFETENSORS=OFF -DMLX_BUILD_METAL=ON -DMLX_USE_CCACHE=OFF \
    -DFETCHCONTENT_FULLY_DISCONNECTED=ON -DFETCHCONTENT_SOURCE_DIR_METAL_CPP="$(only_dir "$metal")" \
    -DFETCHCONTENT_SOURCE_DIR_JSON="$(only_dir "$json")" -DFETCHCONTENT_SOURCE_DIR_FMT="$fmt" >&2
  cmake --build "$b" --parallel "$jobs" >&2
  cmake --install "$b" >&2
  # shellcheck disable=SC2086
  "$CXX" $flags -I"$prefix/include" -I"$prefix/include/metal_cpp" "$repo/tools/bench-macos/mlx-bench/mlx-bench.cpp" \
    "$prefix/lib/libmlx.a" -framework Metal -framework Foundation -framework QuartzCore -framework Accelerate \
    -o "$dest/bin/mlx-bench" >&2
  install -m 0644 "$(find "$prefix" -name mlx.metallib | head -1)" "$dest/bin/mlx.metallib"
  finish mlx-bench "$flags; MLX's own Release build" "$repo/LICENSE" "$mlx/LICENSE"
}

metal_toolchain() { xcrun -sdk macosx metal --version >/dev/null 2>&1; }

# only_dir DIR: the one folder an unpacked archive holds.
only_dir() {
  local d
  d="$(find "$1" -mindepth 1 -maxdepth 1 -type d ! -name __MACOSX)"
  [[ -n $d && $d != *$'\n'* ]] || die "$1 does not hold one folder"
  echo "$d"
}

for p in "${programs[@]}"; do
  SOURCES=()
  dest="$work/out/$p"
  rm -rf "$dest" && mkdir -p "$dest/bin"
  "build_${p//-/_}"
  rm -rf "${out:?}/$p"
  cp -R "$dest" "$out/$p"
done
