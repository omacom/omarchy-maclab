# Performance: tickets

The build of section 22 of `spec.md` ("Performance"), one ticket per slice. A ticket is unblocked when every ticket it is blocked by is done. Each ticket ends with its tests passing and `scripts/ci-local` green for what it touches. Status: `todo`, `doing`, `done` or `human` (needs the owner or real hardware).

| # | Ticket | Blocked by | Status |
|---|---|---|---|
| P1 | Benchmark set format and bench-1 | | done |
| P2 | The release workflow builds the benchmark programs | | done |
| P3 | macOS runner set-up and enrollment | | done |
| P4 | Omarchy runs a Benchmark set | P1 | done |
| P5 | macOS runs a Benchmark set | P1, P3 | done |
| P6 | Performance results in labd | P1 | done |
| P7 | Performance run job and scheduling | P6 | done |
| P8 | The macOS side in labd | P5, P6 | doing |
| P9 | Publishing and Counted | P6 | done |
| P10 | The figures | P9 | doing |
| P11 | Compare on Benchmark sets | P4, P8 | todo |
| P12 | Performance pages | P10 | todo |
| P13 | Time bench-1 on the slowest Lab-owned Mac | P2, P4, P5, P14 | human |
| P14 | lab-agent runs a Performance run's metrics | P4, P7 | doing |

## P1. Benchmark set format and bench-1

Section 22, "Benchmark sets" and "What runs".

- `internal/agent/tests/bench/sets/bench-1.json` and a Go package that loads and checks a set: metric id (the same on both systems), version, settings, unit, direction, category, tolerance (a percentage), Quick or Full, and per system the files by address, sha256 and size. bench-1 holds the table in "What runs"; files the lab builds itself (P2) are listed with a placeholder digest that P13 or the first Lab release fills, and a set with a placeholder cannot be the current set.
- The current set is the newest set without placeholders. Frozen: a test fails when a committed set's content changes.
- `lab bench mirror` mirrors every set's files; older sets' files are mirrored again by hash on demand.
- `benchmarks.json` and `macos-bench.json` go; nothing reads them.
- Acceptance: loader tests (a bad digest, a duplicate metric id, a metric missing on one system, a Quick list without one row per category are refused); `internal/repotest` checks every set in `sets/`.

## P2. The release workflow builds the benchmark programs

Section 11, "Runners", and section 22, "Built by the lab".

- `.github/workflows/release.yml`: the macOS runner builds CoreMark, STREAM, clpeak and fio for macOS arm64, and `ane-bench`, `smc-power` and `mlx-bench`; the arm64 Linux runner builds CoreMark, STREAM and clpeak for Omarchy. Each from pinned source (by commit or sha256), with clang `-O2`, a pinned toolchain and SDK; STREAM's array size is a run-time argument or built at four times the largest Apple Silicon cache.
- Source for the lab's own three programs in the repository (for instance `tools/bench-macos/`): `ane-bench` loads a pinned MobileNet v1 fp16 Core ML model with compute units CPU and Neural Engine, proves it ran on the Neural Engine, and prints inferences/s; `smc-power` reads the SMC key `PSTR` through IOKit's AppleSMC once a second and prints watts; `mlx-bench` times a synchronised fp16 matmul on the GPU and prints TFLOPS.
- The archives are signed release files; `release/local.sh`, `release/check.sh` and `internal/repotest`'s release test know them.
- Acceptance: `release/local.sh` makes them locally where it can (Go-side packaging tested with stand-in binaries); the release test lists them.

## P3. macOS runner set-up and enrollment

Section 22, "Setting up".

- `install.sh` checks the runner's signature before installing anything.
- `lab-macos-runner enroll` keeps the secret per enrollment; labd revokes it when the Mac Leaves or is Retired.
- `sudo lab-macos-runner setup` asks on `/dev/tty`: turn FileVault off (`fdesetup disable`), log this user in automatically (`sysadminctl -autologin set`); it changes nothing without a yes. A Mac starts Attended; the runner reports Unattended only after a boot reaches that user's desktop with nobody typing (a console user logged in with no login window shown since boot).
- labd keeps each Mac's Attended or Unattended state; `lab macos enroll` (Maintainers) and My Macs' "Set up macOS" (the Volunteer) give the one-time line.
- Acceptance: tests for the setup prompts with a fake tty and fake `fdesetup`/`sysadminctl`, the Unattended check, revocation on Leave and Retire.

## P4. Omarchy runs a Benchmark set

Section 22, "What runs" and "Measuring". `internal/agent/tests/bench/`.

- The `bench` builtins run a set's Quick or Full list by metric id: CoreMark, 7-Zip, MotionMark, clpeak (Rusticl), fio as specified, STREAM, Speedometer and JetStream in Chrome for Testing, H.264 and HEVC decode (V4L2 request), the Neural Engine through libane, llama.cpp (Vulkan and CPU), MLX when the Stack has a GPU build, Power from `PSTR` (idle watts; energy for fixed work), boot time.
- Proof the work ran where it says: GPU-drawn pages at 60 Hz, clpeak on the physical GPU, every llama.cpp layer offloaded, hardware decode per codec; else "fell back".
- Output only the allowlisted fields and fixed reason codes; the tools' own text goes to a private log. glmark2, vkmark, vkpeak, the kernel compile and suspend leave.
- Conditions as specified, Storage's 20% free and cleanup on cancel.
- Acceptance: unit tests per benchmark against recorded tool output; the reason codes; no free text in `bench.json`.

## P5. macOS runs a Benchmark set

Section 22, "The macOS side". `internal/macosrunner/`.

- The runner runs a set's list on macOS with the same metric ids and output shape: CoreMark, 7-Zip, MotionMark, clpeak (Apple's OpenCL), fio, STREAM, Speedometer and JetStream in Chrome for Testing (fresh user-owned profile), VideoToolbox decode, `ane-bench`, llama.cpp (Metal and CPU), `mlx-bench`, `smc-power`.
- It measures only on a boot labd started for a queued macOS run; a person's own boot reports `sw_vers`'s version and build and nothing else, even after a one-shot that never started.
- Acceptance: tests with fake tools; the interrupted one-shot then personal boot case.

## P6. Performance results in labd

Section 22, "Performance run" ("Never repeated", "Kept").

- A labd.db migration: `perf_runs` (Mac, enrollment, set, Release or macOS build, list, attempt, state, booted versions, boot time, conditions) and `perf_results` (run, metric, value, spread, unit, api, capacity, reason code). One current value per Mac, set, metric and Release (or macOS build); every attempt kept; an upload taken once per attempt and metric.
- The upload endpoints for lab-agent and the macOS runner, refusing fields outside the allowlist.
- Acceptance: store tests for supersede-on-success, dedupe, attempts kept.

## P7. Performance run job and scheduling

Section 22, "Performance run".

- `spec.performance`: the Release's Package set resolved by its frozen manifest's digest, staged as a Candidate boot; refused with a reason when a file is missing or the board unsupported; the running kernel checked before measuring; booted versions recorded by exact package name.
- Scheduling: claim an idle Mac (no Lease, no job) inside its Availability window, at Full access, on AC, not Paused, not leaving; reconcile what is missing on Release frozen, Mac enrolled or opted in, set changed, verdict recorded; Quick while no verdict, the rest of Full once qualified; newest Release first; the Sighting rule for m1n1 and U-Boot.
- One retry of a failed benchmark at the next slot; `lab perf rerun <mac> <release>` under the caller's Lease; `sudo lab-agent performance on|off` and its state on My Macs.
- Acceptance: scheduler tests with the existing fakes; Quick then Full measures nothing twice; a failed rerun keeps the old values.

## P8. The macOS side in labd

Section 22, "The macOS side".

- A macOS result per Mac, set and macOS build; the runner's build report at enrollment and each macOS boot; a macOS run queued when one is missing, after the Omarchy side.
- `boot-macos-once`: labd told the run started only after `asahi-bless` succeeds; a failed start cancelled; the Mac ready after Known-good checks in and cleanup.
- Unattended: 60-minute deadline. Attended: notify the Volunteer or the Lab group, wait in Omarchy until a person says they are at the Mac (Mac page, My Macs), then boot macOS; 24-hour expiry drops the macOS side.
- Acceptance: tests for the deadline, the Attended confirmation and expiry, the failed start.

## P9. Publishing and Counted

Section 22, "Publishing and Counted".

- labd copies published values (allowlisted fields only) to `reports.db`; deletions carried there; a Volunteer's Mac only while its publishing is on, checked at each copy.
- Counted: a Maintainer's mark on a Volunteer's Mac; automatic for an M team volunteer's Macs; both ways cover past and future values.
- Releases: kernel version and verdict state only; a Release found not qualified or revoked leaves the data.
- Acceptance: the "Tests owed" for publishing, Counted and Releases.

## P10. The figures

Section 22, "The figures". `internal/reports/`.

- A Mac's ratio (Power as specified), a Configuration's median over its headline Macs on the newest measured macOS build, older builds apart, Storage per SSD capacity, coverage, category and Overall geometric means with the excluded reasons, a blank Overall when a category is empty, like-for-like trends, tolerance colours, biggest gaps.
- `GET /api/v1/performance.json?set=&release=&volunteers=` (CC0).
- Acceptance: aggregation tests with fixtures, including a lost slow row drawing no trend.

## P11. Compare on Benchmark sets

Section 21, "Compare job".

- Compare's tiers run the current set's Quick and Full lists, the set kept in the job; matching by metric id; verdicts by the set's tolerance (`compare-noise.json` goes); `--macos` uses a kept macOS result for the set and build.
- Acceptance: the Compare tests updated.

## P12. Performance pages

Section 22, "Pages", and the design canvas "Performance pages".

- `/performance` and `/performance/<board>/<cpu>c-<gpu>g`, server-rendered in the Console look; row 1's Benchmarks becomes Performance and `/benchmarks` redirects; `/models/<board>` links its Configurations; My Macs shows "Set up macOS" and performance runs on or off.
- Collapsible sections without a Node build chain (`<details>` or the site's small JS).
- Acceptance: page tests in `internal/server`; checked in a browser at desktop and phone width.

## P13. Time bench-1 on the slowest Lab-owned Mac

Section 22, "Two lists". The owner's part: on real hardware, time the Quick and Full lists on both systems, record the timings and the built files' digests in bench-1's pull request, and refuse a list over 45 minutes.

## P14. lab-agent runs a Performance run's metrics

Found when P7 merged: labd sends a Performance run as `TestSpec{Name: "perf-bench", Builtin: "bench", Bench: &api.BenchRun{Set, List, Metrics, Run}}` (`internal/api/performance.go`), and nothing on the Mac acts on `Bench` yet.

- lab-agent runs the `bench` builtin on exactly `Bench.Metrics` of `Bench.Set` (passing them to `bench.py`), fetches the set's pinned Omarchy files the job grants, and posts the entries, conditions and state to `POST /api/agent/performance/{Run}/results`: `succeeded`, or `failed` when a result is `error` or `software_renderer`.
- The builtin without `Bench` keeps working for Compare until P11.
- Acceptance: an agent test with the sim Mac that a perf-bench job runs only the named metrics and its upload is taken by labd; the Python runner's metric selection tested.
