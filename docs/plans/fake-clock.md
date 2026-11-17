# Fake clock for the tests

The Go suite spends most of its time asleep. This plan makes labd's, lab-agent's and the runbooks' waits run fast in tests, without changing what the Lab does. Nothing here is built yet; each ticket below is its own pull request.

## Where the time goes (measured 2026-11-17, owner's Mac, `go test -json -count=1`)

| Package | Wall | Sum of tests (serial) | Tests |
|---|---|---|---|
| `internal/server` (sharded) | 187 s | 1911 s | 1027 |
| `internal/minitest` | 538 s | 797 s | 236 |
| `internal/cutovertest/outofband` | 374 s | | |
| `internal/cutovertest` | 278 s | | |
| `internal/carrytest` | 94 s | | |
| `internal/cloudtest` | 91 s | | |
| `internal/cutovertest/hardening` | 48 s | | |
| `internal/agent` | 46 s | | |

CI's runners have fewer cores, so the sharding overlaps less: `internal/server` is 17 min on macOS and 12 min on Linux there.

Slowest tests and what they wait for:

- 66 s × 4, `server_test.go` (`TestNetworkBackOnTheSameBootIsLostContact` and kin): the sim suspends for 8 s past `quietGrace`, then labd waits out `TestGrace`, `BootTimeout` and the reset ladder on the wall clock.
- 15–29 s × ~20: reboot warning (`reboot_warning_test.go`), hard-reset ladder, leases, drains, leaving, sighting. Each waits a few heartbeats (`Interval` 200 ms), `RebootDelay` 300 ms, `BootTimeout` 4 s, warning and grace windows.
- 141 s `TestOOB04PassesWhenARestartMovesThePlugCount`, 46 s `TestOOB06…`: `oob08-refusals` `sleep 16` / `sleep 240` and the out-of-band lib's polling.
- 52–76 s `TestFD01…`, `TestFD09…`, `TestFD06…`: keepalive and tunnel-restart waits in `tools/front-door-probe` and the `fd*` scripts.
- 36–55 s builder-gate tests: `GATE_*` waits, already env-set but not to the minimum.
- 15–25 s minitest: `sleep 1`/`sleep 2` retry loops in `scripts/mini/lib.sh`, `check`, `deploy`, `setup`, `transport`, `hold`.

## What reads time today

A seam already exists: `agent.Clock` (`internal/agent/clock.go`) with `Now`, `After`, `NewTicker`, `Sleep`, a `WallClock` and a `FakeClock` that moves only on `Advance`. labd reads it through `Server.clock()`, `s.now()`, `s.since()` and `s.withTimeout()` (24 call sites); `Config.Clock` and `agent.Config.Clock` take it. A handful of tests (`held_test.go`, `watch_test.go`, `reboot_warning_test.go`, `availability_window_test.go`, …) use `FakeClock`. The shared test lab (`newLabWith`, `server_test.go:144`) defaults to `WallClock` and shrinks package variables (`aliveWindow`, `panicGrace`, `quietGrace`, `quickReboot`, `stageBackoff`) instead.

Non-test code that bypasses the seam:

- **labd (`internal/server`)**: `runner.go:1035` stage backoff `time.After`; `recovery_mode.go:128` `time.After(5s)`; `agent_software.go:228` `time.AfterFunc(hold)`; `leaving.go:268–290` retiring deadline loop with `time.Sleep(20ms)`; `gc.go:22` hourly ticker; `sighting.go:375` ticker; `context.WithTimeout` in `roles.go`, `signin.go`, `signal.go`, `notify.go`, `builds.go`, `sources.go`, `lines.go`, `recover.go`, `drain.go:125`, `watch.go`, `watch_mini.go`, `sighting.go:402`, `server.go:1599` (ResetWait); `time.Now()` in `exports.go`, `releasegate.go`, `artifacts.go`, `device.go:681` (command ids), `signin.go`/`reports.go`/`listen.go`/`server.go:1093`/`builders.go` (body read deadlines).
- **lab-agent (`internal/agent`)**: `sim.go` (`RebootDelay`, `HangFor`, `SuspendFor` sleeps, timers at 842 and 1034); `lockscreen.go`, `rebootwarning.go:107` (`noticeUpAfter`), `limine.go`, `linux.go` (boot/network waits), `procgroup*.go`, `agent.go:320` watchdog loop, `tests.go`/`mesa.go` timings, `uninstall.go`, `leave.go`, `access.go`, `install.go`, `stage.go`, `uki_reserve.go` timeouts.
- **oobd (`internal/oobd`)**: `ports.go` 30 s and `oobd.go:148` 45 s timeouts. Fast already (5 s package).
- **Others**: `internal/loop/run.go`, `internal/workflow`, `cmd/lab-builder`, `internal/esprotation/live_linux.go`, `cmd/labd/import.go`, `oldlab.go`, `breakglass.go`. Not in the slow path.
- **SQLite**: no `CURRENT_TIMESTAMP`/`now()` in labd's SQL; every stored time comes from Go. Keep it so (ticket 1 adds a check).
- **Runbooks (bash)**: fixed `sleep N` in `scripts/mini/{lib.sh,check,deploy,setup,transport,hold,container,maintenance,backup,restore-drill,rehearsal-alias}`, `scripts/cutover/{mini.sh,dark-recordings,alias-rehearsal,builder-gate}`, `out-of-band/{oob08-refusals,lib.sh}`, `front-door/{fd11-cut-job-wait,fd13-cache}`. Some already read env (`LAB_WAIT_STEP`, `LAB_LITESTREAM_WAIT`, `LAB_SENDER_WAIT`, `LAB_*_TRIES`, `GATE_*`).

## Design

**One seam, the one that exists.** Keep `agent.Clock`. Add `AfterFunc` (for `agent_software.go`) and a package helper `WithTimeout(clock, ctx, d)` (move `Server.withTimeout` there) so the agent can use it too. No new interface, no library.

**A fast clock as the tests' default.** `FakeClock` needs each test to call `Advance` at the right moment, which is right for tests about a deadline and wrong for the hundreds of tests that only need waits to be short. Add `agent.ScaledClock(k)`: real time sped up `k` times. `Now()` = start + elapsed × k; `After(d)`, `Sleep(d)`, tickers wait `d/k` real time. It never deadlocks, keeps ordering, and needs no test changes beyond the lab's default. `newLabWith` uses `ScaledClock(50)` (tuned per ticket; start at 20) and the real knob values come back (`BootTimeout` minutes, not 4 s), so tests run production timings, only faster. Tests that pin an exact moment keep `FakeClock`.

**How it reaches each program.**
- labd: `Config.Clock` (exists). Tests set it; `cmd/labd` never does.
- lab-agent and the sim: `agent.Config.Clock` (exists); `Sim` gets a `Clock` field and its sleeps use it.
- oobd: not needed; its waits are short and real.
- Script suites start real `labd`/`lab-agent sim` binaries. Add a test-only `MACLAB_TEST_CLOCK_SCALE` read in `cmd/labd` and `cmd/lab-agent` only when built with `-tags labtest` (the suites already build their own binaries), so a released binary cannot run on a scaled clock.
- Runbooks: every fixed `sleep` and retry count in a script that a suite runs becomes `${LAB_<NAME>_WAIT:-<today's value>}`, the pattern `LAB_WAIT_STEP` already uses. Defaults stay today's values; suites set them to 0 or 0.05. `scripts/mini/` keeps to what macOS has (bash's `sleep` takes fractions there).

**What stays real.** Network and HTTP deadlines in `net/http` servers and clients (body read deadlines, `Server.ReadTimeout`): the kernel enforces them, and scaling them would make loopback flaky. Process timeouts on real tools (`procgroup`, `mesa`, `tests.go`): they bound real child processes. SQLite busy timeouts. TLS certificate times in `testpki` and `oldsitetest`. `front-door-probe`'s stream timings that test the edge, not labd.

**Risks.**
- *Scaled clock and real I/O*: at k=50 a 200 ms heartbeat is 4 ms; a slow CI runner can miss beats and count a Mac unresponsive. Mitigation: pick k per knob class so no scaled wait falls under ~20 ms; run the package `-count=5 -race` on Linux CI before each ticket merges.
- *Mixed clocks*: a deadline taken from the scaled `Now()` and checked against `time.Now()` (or the reverse) is wrong by k. Every bypass in the inventory moves to the clock in the same ticket that scales its package; `internal/repotest` gets a check that `internal/server` and `internal/agent` non-test files call no `time.Now/Sleep/After/NewTicker/AfterFunc` and no `context.WithTimeout` except where listed.
- *Stored timestamps*: labd and the sim must stamp from the same clock, or leases look expired at once. Both take the lab's one clock already; the scaled clock shares its start between them. A Volunteer's real Mac never sees a scaled labd.
- *FakeClock races*: `Advance` before a goroutine has called `After` skips the wait. Tests that use it must wait for `Next()` to report the waiter first (helper `waitWaiter`), as `reboot_warning_test.go` does.
- *Behaviour change*: production keeps `WallClock`; `cmd/` never sets a clock without the build tag. Each ticket's diff to non-test code is mechanical (`time.X` → `s.clock().X`).

## Status (2026-11-18)

| Ticket | PR | State |
|---|---|---|
| 1 labd waits on its clock | #15 | open, CI green |
| 2 sim waits on a clock | #16 (on #15) | open, CI green |
| 3 test lab on a scaled clock | #17 (on #16) | open |
| 4 drop the server shards | | dropped: the package's serial time is still 1248 s, so the shards still pay |
| 5 cutover runbook waits | | dropped: `cutovertest`'s time is the front door's wire timings, which must stay real, and the builder gate's waits, already set from the environment; the out-of-band waits follow labd, which ticket 7 speeds up |
| 6 mini scripts' waits | #19 | open |
| 7 scaled labd in the script suites | #18 (on #17) | open |
| 8 agent package | | dropped: its slow tests time real child processes (process-group kill, SIGTERM grace, hung sync), which stay on the wall clock; ~20 s at most |
| 9 tests' own waits | | not started |

## Tickets

Ordered by time saved per effort. Each is one pull request, mergeable alone, with `scripts/ci-local` green.

1. **Route labd's remaining waits through the clock** (small). `runner.go`, `recovery_mode.go`, `agent_software.go`, `leaving.go`, `sighting.go`, `gc.go`, `drain.go`, `recover.go`, `server.go:1599`; add `Clock.AfterFunc`, `agent.WithTimeout`; repotest check for `internal/server` and for SQL `now()`. Saves ~0 alone; unblocks 2.
2. **Sim sleeps on the clock** (small). `internal/agent/sim.go`, `rebootwarning.go`, `lockscreen.go`, `limine.go`, `agent.go` watchdog loop; repotest check for `internal/agent`. Saves ~0 alone; unblocks 3.
3. **Shared test lab on a scaled clock** (medium). `agent/clock.go` (`ScaledClock`), `server/server_test.go` `newLabWith` default (k=10, `LAB_TEST_CLOCK_SCALE`); tests that time labd against wall-clock sleeps keep the wall clock (`wallClock(t)`, 24 tests). Knob values unchanged. **Measured**: `internal/server` wall 187 s → 117 s on the owner's Mac, serial 1911 s → 1248 s. Less than first estimated: most of what is left is the tests' own `time.Sleep`s and wall deadlines, and `FakeClock` tests pumped in wall time (the reboot-warning ones, 15–23 s each).
4. **Drop the server shards** (small). With 3 in, `main_test.go`'s copy-of-itself sharding costs more than it saves; remove it, keep `t.Parallel` where safe. Saves the shard start-up (~10–20 s) and a lot of code.
5. **Runbook waits from the environment** (medium). `scripts/cutover/out-of-band/{oob08-refusals,lib.sh}`, `front-door/{fd11-cut-job-wait,fd13-cache}`, `builder-gate`, `dark-recordings`, `alias-rehearsal`, `mini.sh`; suites in `internal/cutovertest/...` set them low. `outofband` 374 s → ~60 s, `cutovertest` 278 s → ~80 s.
6. **Mini scripts' waits from the environment** (medium). `scripts/mini/{lib.sh,check,deploy,setup,transport,hold,container,maintenance,backup,restore-drill,rehearsal-alias}`; `internal/minitest` sets them. 538 s → ~150 s.
7. **Scaled clock for the binaries the suites start** (medium). `-tags labtest` read of `MACLAB_TEST_CLOCK_SCALE` in `cmd/labd`, `cmd/lab-agent`; `internal/carrytest`, `cloudtest`, `cutovertest/hardening` set it. 94 + 91 + 48 s → ~60 s together.
9. **Tests' own waits on the lab clock** (medium, added after ticket 3). Replace test-side `time.Sleep`/wall deadlines in `internal/server` tests with the lab's clock, move the 24 `wallClock(t)` tests back, and speed `pumpClock`. Expected `internal/server` serial 1248 s → ~400 s.

8. **Agent package off the wall clock** (small). Remaining `internal/agent` waits that tests hit (`linux.go` boot/network loops, `leave.go`, `uninstall.go`); `internal/agent` 46 s → ~15 s.

## Expected result

Go test packages run in parallel, so the suite's wall time is its slowest package.

| | Now (this Mac) | After all tickets (estimate) |
|---|---|---|
| `internal/server` | 187 s (CI 12–17 min) | 117 s after ticket 3 (measured); ~60 s after ticket 9 |
| `internal/minitest` | 538 s | ~150 s |
| `internal/cutovertest/outofband` | 374 s | ~60 s |
| Whole suite, wall | ~9 min here, ~17 min on CI | ~3 min here, ~4–5 min on CI |

Ticket 3 is measured; the rest are estimates. They assume the scaled clock at k≈10–20 and runbook waits at 0–0.05 s.
