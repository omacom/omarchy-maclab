# Omarchy Mac Lab

Omarchy Mac Lab tests Omarchy on real Apple Silicon Macs and publishes what it finds. This repository, `omarchy-maclab`, holds all of it.

The code began as [maclab](https://github.com/chriskearney/maclab) by Chris Kearney and as omarchy-m-testing; both histories are kept here.

| Path | Holds |
|---|---|
| `cmd/`, `internal/` | the lab's Go programs: `labd`, `lab`, `lab-agent`, `lab-builder`, `oobd` and the rest |
| `builder/`, `specs/`, `tools/` | the Builder's images, the TLA+ specs, the boot-test tools |
| `cli/` | the `omarchy-m-test` command ([docs/omarchy-m-test.md](docs/omarchy-m-test.md)) |
| `schema/`, `catalogue/` | the report schema with its golden reports, and the feature catalogue |
| `installer/`, `release/` | the install line's script and the release build |
| `deploy/`, `scripts/` | what is installed on the lab's server, and what is run |
| `docs/` | decisions (`adr/`), the spec (`spec/`), acceptance records (`acceptance/`), guides, history |
| `site/` | the Rails site the Go port is checked against; it goes when the port passes |
| `GLOSSARY.md` | the words the lab uses |

`scripts/ci-local` runs what the tests workflow runs.

## The lab

A test lab for Omarchy Macs. Hand it a kernel, and it boots that kernel exactly
once on a real Apple Silicon Mac. It then runs tests (including Wayland GUI
tests), collects logs, and puts the Mac back on its known-good kernel. When a
kernel crashes or hangs the machine, the lab gets the Mac back itself. When it
can't, it tells a person exactly what to press.

```
lab run m1air --kernel kernel-7.2.0-rc1-lab1.tar.zst --gui-test gui-smoke
```

AI agents get the same operations over MCP (`lab mcp`).

## Pieces

| Binary | Runs on | Job |
|---|---|---|
| `labd` | any Linux box, or a Mac | Controller: devices, leases, job queue, recovery ladder, notifications, artifact and log store |
| `lab-agent` | each test Mac (Omarchy/Asahi, aarch64) | Heartbeat. Stages kernels beside the known-good one, arms the one-shot boot, runs tests, uploads logs |
| `oobd` | the mini, as a root LaunchDaemon | The Out-of-band controller: a Hard reset for a Lab-owned Mac cabled to one of the mini's three rear USB-C ports, through the pinned `macvdmtool` |
| `lab` | your machine | CLI, and `lab mcp` for agents |

## How a job runs

1. **Stage.** The agent downloads the artifact, installs its modules under
   `/usr/lib/modules/<release>` (refusing any release it didn't install
   itself), and writes the boot files for the Mac's bootloader. The
   known-good kernel is never touched.
   - **GRUB:** an initramfs and an entry in `/boot/grub/maclab.cfg`.
   - **Limine** (Omarchy on Apple Silicon: U-Boot, then Limine, then a UKI on
     the ESP): a unified kernel image in `<esp>/maclab/<job>/uki.efi`, listed in
     a marked block at the end of `limine.conf`. Limine reads only FAT, so the
     ESP needs room for one UKI rounded up to its allocation unit, plus a
     margin of `ceil(UKI size / 10)` and 1 MiB for metadata. Preflight uses
     the largest UKI in `<esp>/EFI/Linux` (or a labelled 64 MiB guess if none
     exists); the writer and post-copy gate use the generated UKI's actual size.

   A failed staging is tried again, up to 3 attempts, 30 s and then 2 min
   apart, with the failed attempt's leftovers removed first. Only an attempt
   that is over is tried again: one the agent reported failed, or one the Mac
   rebooted under. One that outlasts labd's 20-minute wait, or whose Mac goes
   quiet, may still be staging, so the job fails instead. The same error
   twice in a row looks permanent and ends it early.
2. **Boot once.** The agent arms a one-shot and reboots. The bootloader
   **clears it before booting the entry**, so any reset afterwards, for any
   reason, lands on the known-good kernel.
   - **GRUB:** `maclab_next=<entry>` in `maclab.env` on the ESP. GRUB can't
     write `grubenv` on btrfs, so a plain `grub-reboot` would never clear;
     that's why the flag lives on the FAT ESP.
   - **Limine:** the Boot Loader Interface variable `LoaderEntryOneShot`, which
     Limine deletes as it reads it. Apple Silicon's U-Boot keeps EFI variables
     in RAM at runtime, so the agent writes U-Boot's store (`VarToFile`) to the
     file U-Boot loads them from (`ubootefi.var` on the ESP).
3. **Watch.** labd waits for a check-in from the new boot. The boot is tagged
   `maclab.job=<id>` on the kernel command line, so labd can tell "booted the
   test kernel" from "fell back". labd reads no console: what it knows of a
   lost boot is what the Mac kept of it (see "When things go wrong").
4. **Test.** `boot-health` always runs. It is followed by any builtin tests
   (`gui-smoke`, `gui-terminal`, `suspend-devices`) or your own scripts. GUI
   tests run inside the GUI user's Hyprland session. Anything written to
   `$MACLAB_OUT` comes back with the job. `suspend-devices` writes `devices` to
   `/sys/power/pm_test` and runs `systemctl suspend`: the kernel freezes tasks
   and runs the devices' prepare and suspend callbacks (not their late and
   noirq phases, nor the sleep itself), waits 5 s and resumes them by itself.
   The test passes if that cycle's kernel log shows the wait (which comes only
   once those callbacks succeeded) and no device failing to suspend or resume,
   and the default route comes back. On a kernel without `pm_test`'s `devices`
   mode (`CONFIG_PM_DEBUG`) it is skipped, which is not a pass: the job ends
   `tests_failed`. `pm_test` goes back to the mode it
   had, but to `none` only once nothing is left that could suspend the Mac; if
   a suspend that didn't finish in time never settles, it stays at `devices`
   until the next boot, so a later suspend is a test one too. Omarchy locks the
   desktop before every suspend, and only the user's password unlocks it, so
   the test stops the GUI user's `omarchy-sleep-lock.service` for the cycle and
   starts it again after. All of it, putting things back too, ends within the
   test's timeout (5 minutes by default). The Mac goes quiet for 10-15 s
   (NetworkManager takes Wi-Fi down for the suspend), so just before it
   suspends, the test tells labd how long the Mac may be off the network (3.5
   minutes: the waits for the cycle, for it to settle and for the route, or
   what is left of the timeout), and labd waits that out on that boot before
   counting the Mac as lost. Once the network is back, the test tells labd
   so, and labd stops waiting: a hang in the next test shows after the usual
   60 s of silence. A real suspend can't be
   tested unattended: Apple Silicon's RTC has no wake alarm, so only a key
   press wakes the Mac (a reset ends the boot). labd warns when a test named
   `suspend-*` needs someone there.
   A `--script` test can bring files with it: `--payload NAME=FILE` (repeatable,
   at most 16; `NAME=SHA256` reuses an upload) puts each file in `$MACLAB_IN`
   under NAME, executable, before the script runs. A module, a firmware blob or
   a binary needs no download inside the script this way, and the Audit log
   names each file with its hash. `--gui-script` takes none.
5. **Collect.** The journal and the kernel log are saved.
   Warnings and errors that aren't in the Mac's baseline are listed as
   `new_error_lines`.
6. **Restore.** The agent reboots to the known-good kernel, checks that the
   one-shot really cleared, and removes the staged kernel.

A test boot's command line starts from the known-good kernel's and adds the Lab
boot parameters (`panic=10`, `watchdog.open_timeout=N` and the Lab boot marker
`maclab.lab_boot=1`) and `maclab.job=<id>`. It keeps `quiet`, `splash` and `loglevel=`, so the Mac boots
the way its user's boots do; dmesg and the journal record every level anyway. With
`lab run --verbose-boot` it drops them instead and adds `loglevel=7`.
`lab run --cmdline-base default` starts from the distro's stock
command line (`KERNEL_CMDLINE[default]` in `/etc/default/limine`) instead, to show a
kernel needs no bring-up parameters. `--cmdline-base '<cmdline>'` takes a literal
one. `--cmdline-strip 'asahi.* dcpext_*'` drops matching parameters, and `--cmdline`
appends. The job records the staged and the booted command line. Test boots also
keep `systemd-analyze` time, blame and critical-chain.

Outcomes: `pass`, `tests_failed`, `booted_unhealthy`, `panicked`, `hung`,
`crashed_or_hung`, `boot_failed`, `lost_contact`, `stage_failed`, `infra_error`, `canceled`.
`crashed_or_hung` means the Mac rebooted during a test with no kernel error
captured (`panicked` needs one); its summary gives the seconds from the last
check-in to the next boot's first, and back within about 90s reads as a crash
(`panic=10` reboots in seconds), later as a watchdog reset after a hang, which is
an inference, not a diagnosis.

### Reserving a Mac

`lab lease <device>` takes a Lease on a Mac, as does the Lease button on Macs:
until it runs out or `lab release <device>`, only your jobs run on it. Others'
new jobs are refused, a Maintainer's included, naming the holder and the end
time. Jobs they queued before you took it wait aside, saying so, while yours
queued after them go first, and run once the Lease ends. A job already running
when you take it finishes first.

A Lease is a person's, a Maintainer's or an M team member's: the lab keeps the
account id, and a rename changes nothing. Inside it is the holder label
(`MACLAB_HOLDER`, else `user@host`), so one person's several agents keep apart:
naming the same holder doesn't let another person renew it, release it, reset
the Mac or run jobs under it. It lasts 4 hours; a Maintainer may ask for up to
24 (`--ttl 24h`), an M team member may not. Taking it again renews it, at any
time: it then ends 4 hours from that moment. Only a Mac that has passed its
baseline and has not left is leased; a Volunteer's Mac only inside its
Availability window, at either Access level, and there the Lease ends with the
window at the latest.

An M team member holds one Mac of each Configuration (the board with its CPU
and GPU core counts): a second Lease is refused, naming the one held. A
Maintainer is warned and goes on. The lab works the Configuration out: from a
table for a board sold with one, otherwise from the kernel log of the Mac's
latest job on its Known-good kernel. Macs of one board whose counts it cannot
read count as the same Configuration until a Maintainer sets them, on Macs.

A Lease fences jobs, `lab exec`, screenshots, the live journal and reset. An
M team member needs their own Lease on the Mac for each, cancels only their
own jobs, and runs no baseline and no build. A Maintainer acts without a Lease
on a free Mac, with one exception: nobody runs `lab exec` without their own
Lease, a Maintainer included. Under another person's Lease a Maintainer is
refused jobs, exec and a reset, may still look and cancel a job, and may end
the Lease. The M team
reads every job, result and collected file on the fleet; a Mac's Audit log is
for Maintainers and its Volunteer.

How a Lease ends decides what happens to its holder's work:

| How it ends | Running job | Holder's queued jobs and sessions |
| --- | --- | --- |
| Released by the holder | finishes | canceled and cut |
| Runs out with the holder's job running | the Lease is extended until that job ends, no further | canceled and cut at the original end |
| Runs out with nothing running | | canceled and cut |
| Ended by a Maintainer (`lab release <device> --force --why "…"`, or End Lease on People), the holder leaving the M team or being Blocked, "End all access" | canceled as `lab cancel` does; the Mac returns to its Known-good kernel | canceled and cut |
| Window end, Pause, lowered Access level on a Volunteer's Mac | stopped by `lab-agent` | stay queued |

Every `lab exec`, and every refused attempt, is in the Exec record: the person
and the handle they had then, the Mac, when, the command's text, its exit code
and how long it ran, and never its output. A line is deleted when it is 12
months old. `GET /api/devices/<device>/execs` shows it to Maintainers and the
Mac's Volunteer, and to anyone else the commands they ran themselves. The text
of a command is kept and is visible on the Mac while it runs: never put a
secret in one.

A job that ran under a Lease is never queued again by labd itself. Jobs others
queued before the Lease run when it ends. Cutting a `lab exec` ends its answer;
a command already started on the Mac runs to its own end.

Every Lease taken, renewed, extended, released, expired and ended is in the
Mac's Lease record (`GET /api/devices/<device>/leases`, for whoever sees the
Mac) with the handle the person had then, and on the person's row on People.
The Lab group gets a line for a Lease taken, released, expired or ended, none
for a renewal or an extension. `lab status` shows when each Lease ends, and
`lab status <device>` how the last one on a free Mac ended: who ended it and
why.

## Lab boots come back by themselves

Every Lab boot gets the Mac back to its known-good kernel without help, on
every Mac. The one-shot is spent by the time anything below fires, so each
reboot lands on the Mac's own entry.

- `panic=10` reboots a panic, even one before the sysctl applies.
- `watchdog.open_timeout=N` (labd `--watchdog-open-timeout`; by default half the job's boot timeout, at least 120s)
  resets an early hang: U-Boot leaves the hardware watchdog running into the
  kernel, `apple_wdt` takes it over, and the kernel stops feeding it unless
  systemd opens it within N seconds (worst case N + 30s). A test kernel must
  be built with `apple_wdt`, or the watchdog resets it about 60s into every
  boot; the job flags one without it.
- Before every Lab boot, lab-agent writes a deadline record to
  `/var/lib/maclab/lab-boot/`: **no contact** (twice the job's boot timeout, at most
  9 minutes so it comes before lab-agent's own watchdog, unless lab-agent
  reaches labd first) and a **hard ceiling** (the job's end).
  `maclab-lab-boot-deadlines.service`, an early unit with no network
  dependency, reboots a Lab boot when either passes, and on a Volunteer's Mac
  as soon as its Availability window closes. labd waits for that
  before its recovery ladder, so a Lab-owned Mac's Hard resets are a later rung.
- After any recovery, lab-agent uploads the lost boot's journal (by boot ID)
  and any pstore/ramoops records, as `prevboot/` in the job.
- A boot that came back only through its deadline fails as `lost_contact`:
  the kernel or Release broke networking.

[docs/lab-boot-hardware-check.md](docs/lab-boot-hardware-check.md) is the
check that the early unit and the watchdog work on a real Mac.

## When things go wrong

labd notices a problem from any of these signals:

- The heartbeat stops.
- The boot ID doesn't change after a reboot.
- The Mac comes back on the known-good kernel.
- The Mac reports a panic, and doesn't reboot within 60s.

It then climbs a ladder:

1. Soft reboot through the agent, if the agent still answers.
2. Hard reset through the Out-of-band controller, up to twice: only a
   Lab-owned Mac the lab has a Sighting of, while the controller is healthy
   (see [Out-of-band control on the mini](#out-of-band-control-on-the-mini)).
   With no Sighting, no cable or an unhealthy controller the rung is skipped:
   the Mac's own watchdog has had its time by then, and the next rung says
   why no Hard reset was sent. A Volunteer's Mac never gets one.
3. **Ask a human.** The Mac goes to `needs_hands`, and one urgent
   notification goes out (ntfy and/or a command such as `notify-send`):

   > **m1air needs a power cycle**
   > 1. Find m1air (MacBook Air M1, left desk). Open the lid if it's closed.
   > 2. Hold the power button (the Touch ID key, top right of the keyboard) for 10 seconds, until the screen stays black.
   > 3. Wait 5 seconds, then press it once to turn it back on.
   > 4. That's it. The lab notices when it checks in again and picks up from there.

On a Volunteer's Mac the ladder differs: nothing reaches the Volunteer until
the Lab boot's Hard ceiling plus one boot timeout have passed with no check-in
(by then its own deadlines have had every chance to bring it back), and the
notice goes only to the Mac's own ntfy topic, never the lab-wide topic or
`--notify-cmd`, which are the Maintainers'. `sudo lab-agent notify --topic
https://ntfy.sh/my-topic` switches the Mac to the Volunteer's own topic; it
reaches labd at the next check-in, and `status` shows the `subscribe to:` URL.

When the Mac checks in again, the alert clears, leftover staged kernels are
removed, and queued jobs continue. `lab status` shows the same instruction
while it's outstanding.

A job that couldn't stage its kernel ends `stage_failed`, with the reason and
`lab retry <job>` in its summary. `lab retry` (or Retry on a finished job's page)
runs any finished job again as a new job: the same Mac, kernel, tests and cmdline,
and the new job records which one it retries (`retry_of`). A source job boots the
kernel its first run built. Once that has been collected, the retry builds again
from the exact commit and the config the first build recorded, not from wherever
its branch or pull request points now or the Mac's config as it is now; a job
that recorded no commit, or whose config has been collected too, can't be
retried (a build from another config would be another kernel), and the refusal
says to run it afresh with `lab run`.
`lab retry` holds the new job as you, and the web UI's Retry as `web`.

Each Mac gets a recovery tier: 0 = panic reboot + one-shot fallback and the
Lab boot deadlines,
1 = + hardware watchdog (`apple_wdt`, armed through systemd),
2 = + Hard reset: a Lab-owned Mac the lab has a Sighting of, while the Out-of-band controller is healthy.

What a crashed boot leaves behind is incomplete. These Macs have no pstore
backend: there is no ramoops region in the device tree, and the kernel's EFI
pstore backend is off by default (`CONFIG_EFI_VARS_PSTORE_DEFAULT_DISABLE`); it
wouldn't help anyway, since U-Boot keeps EFI variables in RAM at runtime. So the
panic message itself reaches only a serial console, which this lab does not
read. Writes made just before a
panic are lost too. On an M1 MacBook Air (two runs, each ending in a sysrq
panic), files whose `fsync` had returned 1 s or 0 s before the panic were gone
after the reset, although `panic=10` held the Mac up for 10 s after it. Files
fsync'd 2 s or more before survived on btrfs, and 5 s or more before on ext4
(where 2 s and 1 s weren't tried). A file written without `fsync` 35 s before
the panic came back empty. The cause wasn't investigated. So the previous boot's
journal that labd collects can end well before the crash: journald syncs to disk
every 5 minutes by default, and at once only after a critical message. labd's
job events, which name each test as labd starts it, show where a boot crashed.

On a tier 1 Mac a test boot recovers from hangs without the Out-of-band controller, once Linux runs.
U-Boot leaves `apple_wdt` running into Linux (it feeds it itself while it or Limine waits,
so a Mac sitting at a U-Boot prompt or the Limine menu is not reset). A test boot's
command line adds `systemd.watchdog_sec=0 watchdog.open_timeout=N`: systemd, in the
initramfs too, leaves the watchdog alone, and the kernel pings it only for N seconds. N is
half the job's boot timeout, and at least 120 (what an older labd, which doesn't send its
timeout, gets too): 120 with labd's default 4-minute `--boot-timeout`, 300 with
`lab run --boot-timeout 600`. The other half is left for the reset and the fallback boot, so labd
sees the Mac back on its known-good kernel before it gives up and resets it mid-fallback,
with a margin that grows with the timeout. A per-job `--boot-timeout` under 4 minutes still gets
N = 120, which leaves the reset and the fallback boot less than half of it: they may not be done
before labd stops waiting, and labd then resets the Mac mid-fallback. Any boot that hasn't opened the
watchdog by then is reset within 30 s, a healthy one too, so lab-agent opens it on every
test boot (about 5 s after the kernel starts, on an M1 Air). If it can't yet, it tries
again every second, while it checks in, until N seconds after the kernel started, and then
logs that the Mac will be reset: a boot that hung, or is alive but stuck before lab-agent
runs (say waiting for its root device), is reset. An installed-mode boot doesn't hand the
watchdog over (no `systemd.watchdog_sec=0`): systemd holds and feeds it as on any boot,
and lab-agent just notes that. On a test boot lab-agent then holds the watchdog (so the
Mac still shows tier 1) and feeds it only while:

- PID 1 answers. lab-agent asks it for its version every 10 s, and 30 s without an answer
  stops the feeding, test or not. This stands in for systemd's own watchdog, which is off
  on a test boot: a wedged PID 1 is reset within 90 s of its last answer, where
  `RuntimeWatchdogSec=30s` takes 30 s, and a PID 1 too busy to answer for 30 s counts as
  wedged, which lab-agent logs.
- A test runs within its timeout plus 20 s (2 minutes more for a GUI test, as waiting for
  the desktop session can outlast a short timeout), counted, like the timeout, from when
  its script has downloaded (a builtin test downloads nothing, even one that names a script
  too). Nothing else extends that: a test that hangs where its timeout can't reach it ends
  in a reset although lab-agent keeps checking in, while labd still waits for it. A test
  already running when lab-agent opens the watchdog late (on a retry) gets the same budget,
  counted from when it started, so one already past it is reset at once.
- Outside tests, the last check-in that reached labd, or the last command labd sent
  starting, was less than 10 minutes ago. A test boot stays up for as long as labd is in
  touch (a long log upload, or a Mac left on a lab entry whose one-shot didn't clear), and
  is reset 10 minutes after it last heard from labd. Nothing lab-agent does by itself
  counts: a test or command ending, or an upload retrying while labd is out of reach,
  leaves a test boot that lost labd during a test with what is left of those 10 minutes.

A test may set `require_watchdog: true` to refuse execution until lab-agent has
opened the watchdog on that boot. The check records a successful open; it does
not verify later writes or hardware resets. Deploy labd with this support before
updating lab-agent, and use the option only after both are updated. Older labd
versions can discard the field without enforcing it.

Either way the one-shot is spent and the Mac comes back on its known-good kernel, except
on a Mac whose one-shot didn't clear: that one comes back on the lab entry, so while labd
is down it resets every 11 minutes or so. The watchdog stays running when lab-agent exits,
so a shutdown that stalls is reset 60 s after that, and so is a test boot whose lab-agent
is stopped (`systemctl stop lab-agent`).

## Setup

Omarchy Mac Lab answers at `https://lab.omarchy-mac.org`, the Lab address.
The site is for everyone; `lab` is for the M team and Maintainers.

```sh
curl -fsSL https://lab.omarchy-mac.org/install/lab | bash   # the latest Lab release, into ~/.local/bin/lab, no root
lab login https://lab.omarchy-mac.org    # asks for a Lab token; it is never an argument
lab status
```

The install line takes the latest Lab release's `lab` for the machine (macOS
on Apple Silicon, Linux on arm64 or x86-64), checks it against the pinned
release key, and running it again updates `lab`. While the lab is in Preview
it serves the Release candidate the mini runs, as `/install` does for the
test tool. It never replaces a `lab` that is not Omarchy Mac Lab's: the
install stops, and a person who also works in maclab's own lab keeps that
build under a name they choose. `lab version` prints its release and the lab's. The lab
refuses a `lab` older than its minimum release in one line, with the install
line, and `lab status` and `lab login` add a line when the lab runs a newer
release.

`lab` keeps one login, in its own file `maclab/omarchy-mac-lab.json` under
your config directory, and never reads or writes `maclab/client.json`. A
command always goes to the saved login. `MACLAB_URL` and `MACLAB_TOKEN` given
together reach another address for one call; `MACLAB_URL` alone, naming
another address, is refused, so a saved token only goes to the address it was
saved for. While the lab cannot be reached `lab` says so in one line, and
that no reset is possible until it returns; it never prints an error page.

A person signs in with GitHub and makes their own Lab tokens on their own
page, up to 10: each is shown once (labd keeps only its hash), lasts 30 days,
90 days or 1 year unused, or never expires, and does what its person may do
at that moment. Jobs and Promotions record the person whose token asked.
There is no admin token and no Maintainer token: Maintainer is a mark on
People. `labd break-glass`, run on the server as the account labd runs as,
mints a Lab token for a named person when nothing else gets in.

Some things are done on the site only, and `lab` has no command for them:
People, Requests to lend, Invites, Block, "End all access", revoking another
person's token, a Configuration override, Pause, Limit, making and renewing a
Lab token, and Hide, Show and Delete of a report.

`labd import` carries the old site's reports into the lab, once, on the
server (`scripts/cutover/import-reports` runs it whole). `labd import dump
--to FILE` reads the old site's Postgres into one file: it asks for the
Postgres address at a prompt and does not show it, and takes it from no
argument, variable or file. `labd import load --from FILE` fills an empty
`reports.db` from that file alone, and `labd import check` runs the three
checks the copy is accepted by.

`labd old-lab` is for the data directory of the old lab's labd, copied to
the server (`scripts/cutover/old-lab` stops that labd, copies the directory
without its token files, checks it file for file and puts it in place).
`labd old-lab counts --data DIR` says how many Mac records, jobs and builds
it holds. `labd old-lab upgrade --data DIR --account-id N HANDLE` makes it
this lab's where it lies, once, before labd first opens it: every record,
job and build keeps its id, its Mac records are Retired, what was done under
its admin token becomes the owner's, and nothing the old lab handed out is
honoured. `--dry-run` says what it would do and changes nothing.
`scripts/cutover/rehearsal-import` puts the machine-id key in place from a
prompt and runs the import and its three checks on an emptied `reports.db`,
while the lab is in Preview.

The lab opens once (the Cutover's step 25). `scripts/cutover/open`, on the
server, asks labd on its ops socket: from that answer reports and Mac sign-in
are accepted, Preview's banner and `noindex` are gone, the home page says for
30 days that `omarchy-m-testing.org` is now here, and the install lines serve
the latest Lab release. labd keeps the opening in `labd.db`: an open lab stays
open whatever `--preview` says, and nothing closes it again. `labd stage
--data DIR` says whether a data directory's lab has opened. The day around
that step is `scripts/cutover/day`, on the owner's Mac: with no option it
says every step in order and asks nothing; `--run` checks what must be true
before each step, does the script's part and stops at each step that is the
owner's with the words to type or click. `scripts/cutover/back` is the way
back before the opening, `scripts/cutover/confirmed` the checklist after it,
`scripts/cutover/ready` the list of which gate tests have a green record
under `docs/acceptance/`.

To run a lab of your own while developing:

```sh
make                                 # binaries in bin/
labd --sign-in <folder>              # <folder>/admins names the Admins by GitHub account id
labd break-glass --account-id <id> <your-handle>   # prints a Lab token for you, once
lab login http://127.0.0.1:7770      # asks for that token
lab enroll m1air --label "MacBook Air M1, left desk"   # prints the Join command for a Lab-owned Mac
```

labd listens on its own machine only: a loopback port (`127.0.0.1:7770`
unless `--listen` names another) and, on the mini, the Unix socket the tunnel
dials (`--socket`, with `--public-url` naming the Lab address). It serves no
TLS and listens on no other network address;
[docs/https.md](docs/https.md) says how the lab is reached. Remote links are slow, so
the timeouts that assume a LAN are flags: `--boot-timeout`, `--offline-after`,
`--alive-window`, `--quiet-grace` and `--stage-timeout` (defaults are sized for
remote Macs). Artifact downloads resume with HTTP range requests after a dropped
transfer. lab-agent's own request timeouts are `http_timeout_sec` and
`transfer_timeout_sec` in its config file.

An enrollment token activates one Mac, once. The first enrollment with it uses
it up for that Mac's activation key, which `setup` keeps until it has finished:
a `setup` that stopped half way can run again on the same Mac with the same
token until the Mac first checks in, while labd keeps running. Once lab-agent
has started and checked in (which uses the token up), finish with `sudo
lab-agent setup` and no token. Another Mac holding a copy of the token gets
nothing. Once the token has expired (1 hour), make a new one with `lab
enroll <name>`, after `lab remove <name>` if the Mac is listed already. `lab
enroll <name> --revoke` withdraws a token.

On the Mac, the Join command is one short line:

```sh
 curl -fsSL https://lab.omarchy-mac.org/join | LAB_JOIN=enr_... bash
```

`/join` is a script that holds no secret, made by labd for its current
release. It downloads lab-agent with the token, checks its sha256 and its
signature against the release key written into the script, and runs
`setup`, handing it the token on stdin; setup asks its questions on the
terminal. By hand:

```sh
printf '%s\n' enr_... | sudo ./lab-agent setup --server https://lab.omarchy-mac.org --token-stdin --gui-user <you> --autologin
```

With a token, `setup` is what the Join command runs on the Mac, in this
order:

1. **Pre-checks.** They read the Mac, change nothing and reach no lab. One
   that fails stops setup, and the lab is told nothing: the Mac must be an
   Apple Silicon Mac on Linux running Omarchy; the Join command must be run
   as yourself (it asks for sudo itself); the Mac must not be in this lab
   already, nor Lab-owned in another; it must not be running a lab kernel;
   its disk must not wait for a passphrase at every start (reinstall without
   encryption, or `scripts/lab-only-autounlock` on an install that is for the
   lab only: whoever holds the Mac can then read that disk); the bootloader
   must be able to restart it unattended, the tools the lab needs must be
   there, and test kernels need 300 MB. Two only warn: a system that cannot
   be snapshotted gets no Installed runs, and a laptop on battery starts
   jobs only on AC power.
2. **Another lab.** A Mac is in one lab at a time. Setup names the other
   lab and asks once; on `y` the Mac leaves it properly, after the terms and
   before it enrolls: that lab is told, then its boot entries, units and
   device secret go. The Mac arrives with a new device id and secret, and
   the other lab's Audit log is set aside (`audit.log.before-<time>`), not
   copied here.
3. **The terms.** Setup shows the lab's terms and goes on only when you type
   `yes`. Without a terminal it stops, unless `--accept-terms <sha256>` names
   the exact terms you read (`<Lab address>/terms`). The lab records the
   acceptance with the person, the Mac, the time and that sha256, and takes
   no enrollment that accepted other terms. Until the `yes`, the lab has
   been sent nothing about the Mac: setup only read the terms from it.
4. **Enrolling.** The lab tests the Join command once more and says here
   what would refuse the Mac (Blocked, cap reached, name taken). A
   Volunteer's Mac arrives at Qualification with publishing off, whatever it
   had before; an Availability window it had is kept.
5. **The window.** Setup ends by asking once for the weekly Availability
   window (`mon-fri 19:00-07:00`); Enter decides later. A Mac with no window
   takes nothing from the lab.

A system counts as Omarchy when pacman's records of installed packages
(`/var/lib/pacman/local`) hold `omarchy` or `omarchy-mac` (a converged
image), `omarchy-dev` (the omarchy-mx-mac fork) or `omarchy-mac-keyring` (a
legacy omarchy-mac install), or when Omarchy's own `omarchy-version` is in
`/usr/share/omarchy/bin` or in `~/.local/share/omarchy/bin` of whoever ran the
Join command (a legacy install from a checkout). It is `omarchy-m-test`'s
rule, read from files, since the pre-checks run nothing
(`TestHowASystemIsRecognisedAsOmarchy`).

The Join command holds the token, so keep it private. The token stays in the
environment of your own shell (`LAB_JOIN`, readable by you and root only,
taken out before anything starts) and out of URLs, labd's log and the
arguments of the curl and sudo it starts. The line starts with a space, so a
shell that skips such lines leaves it out of its history. A download of the
script cut short runs nothing.

`setup` makes these changes. Each one is idempotent and backed up:

- GRUB: appends the one-shot hook to `/boot/grub/custom.cfg` (backup: `custom.cfg.pre-maclab`) and creates `maclab.env` on the ESP.
- Limine: creates `<esp>/maclab/`, adds the (empty) maclab block to `limine.conf`, and keeps a copy of U-Boot's `ubootefi.var` as `ubootefi.var.pre-maclab`.
- Records the running kernel as known-good.
- Sets `kernel.panic = 10`.
- Arms the hardware watchdog (`RuntimeWatchdogSec=30s`).
- Keeps the journal on disk, if it isn't already, with
  `/etc/systemd/journald.conf.d/90-maclab.conf` (`Storage=persistent`), and
  moves this boot's journal there, so labd can read a failed lab boot's journal.
  `--volatile-journal` leaves journald alone.
- Installs and enables `maclab-lab-boot-deadlines.service`, which reboots a Lab boot that misses its deadline.
- With `--autologin`, logs the GUI user into the desktop on boot; with `--autologin` or `--stay-awake`, turns their idle lock and screensaver off (Lab-owned Macs only).
- Installs and starts `lab-agent.service`.

`setup` refuses, before changing anything, a Mac that can't reboot unattended:
one whose disk needs a passphrase at boot, or, under Limine, one with Limine
config enrollment on. An encrypted volume is fine when a key file unlocks it. For the
volume under `/` (or an encrypted `/boot`), that must be a key file the initramfs can read
before `/` is unlocked: one its `/etc/crypttab.initramfs` line names, or `rd.luks.key=` or
`cryptkey=` on the cmdline, and not on that volume itself (as in
`rd.luks.key=<file>:<device>` or `cryptkey=<device>:...` naming it, or `root=`). Keys in
`/etc/crypttab` and `/etc/cryptsetup-keys.d/<name>.key` live on `/` and the initramfs
reads neither, so they count only for other volumes; the `encrypt` hook (`cryptdevice=`)
reads no crypttab at all, only `cryptkey=`. What is refused is a volume that would wait
for a passphrase and that boot can't do without: the one under `/` or `/boot`, one the
cmdline unlocks (`rd.luks.uuid=`, `rd.luks.name=`, `luks.uuid=`, `luks.name=`,
`cryptdevice=`), or a crypttab entry without `nofail`. Under GRUB, a `/boot` on dm-crypt is
refused whatever unlocks it later: GRUB asks for its passphrase itself. Limine then
checks `limine.conf` against a checksum built into its EFI binary and stops
booting once the lab edits `limine.conf`. Enrollment is on when
`EFI/BOOT/BOOTAA64.EFI` or `EFI/limine/limine_aa64.efi` on the ESP holds a
checksum, or when `ENABLE_ENROLL_LIMINE_CONFIG=yes` in `/etc/default/limine` has
every Limine update enroll one; set it to `no` there, then run
`sudo limine enroll-config --reset <binary>` for each enrolled binary.
`lab-agent preflight` reports the same as problems, which hold jobs back, and
warns about a journal that isn't kept across reboots: that loses the logs of a
lab boot that fails, but doesn't hold jobs back. For an Omarchy Mac whose disk is
encrypted, see [docs/encrypted-macs.md](docs/encrypted-macs.md).

Then run the baseline:

```sh
lab baseline m1air    # two reboots: proves the one-shot selects AND clears
lab crashtest m1air   # optional: deliberate panic, checks it recovers by itself
```

No other job runs on a Mac until its baseline passes.

A distro kernel update (`pacman -Syu`) removes the known-good kernel's package, and
preflight then holds every job back with "known-good kernel ... is no longer
installed"; `lab status` says so with the fix. Reboot the Mac onto the new kernel,
then run `lab baseline <mac>` again: it records the running kernel as the Mac's
known-good, and labd's once it passes. Until it passes, the Mac has no known-good
kernel on labd and takes no other job. The baseline passes only if a plain reboot
comes back on that kernel, so it must be the bootloader's default entry.

lab-agent itself comes from the lab server only, signed, and never from GitHub.
What each install checks:

- **The Join command** checks the agent's signature against the release key
  written into the script labd serves at `/join` for the current release, and setup stops at its pre-checks when the lab-agent
  installed on the Mac is newer than the one it brings.
- **`sudo lab-agent update`** and **`lab agent-update`** check the new agent's
  signature against the release key pinned in the lab-agent already running,
  and refuse an agent older than the one installed; a refusal says "Nothing was
  changed."
- **A labd with no signed lab-agent** to hand out makes no Join command:
  `lab enroll` prints the enrollment token alone and no line that installs an
  agent copied by hand.

```sh
sudo lab-agent update      # at the Mac: take the lab's lab-agent
lab agent-update m1air     # a Maintainer, for a Lab-owned Mac, while it is idle: no job running or waiting, no Lease
lab-agent version          # its own number, the Lab release it came from, the release key it trusts
```

`lab agent-update` has the Mac's running agent do what `update` does, so the
check is the Mac's own in both. It needs lab-agent 0.10.0 or later on the Mac,
the first that updates itself; a Mac with an older one Joins again. `update`
is refused on a Lab boot.

Nothing pushes an agent to a Volunteer's Mac: `lab agent-update` is refused
there. Its Volunteer gets one Notice, "An update is waiting for this Mac. Open
My Macs.", with no command in it, sees the line on the Mac's own page, and runs
`sudo lab-agent update` at the Mac. The agent's check-ins carry its number and
its Lab release, and `lab status` and Macs show an agent older than the lab's
with its version and who updates it. There is no fleet-wide minimum: the lab
refuses only a job that needs a newer agent, naming the version.

To take a Mac out of the lab, `sudo lab-agent uninstall` undoes every change `setup` made. First it
tells labd the Mac is leaving: labd cancels the Mac's running and queued jobs, saying why, lists it as
`left`, and from then on neither resets it nor asks for hands, as it otherwise would once the agent
stops mid-job. If labd can't be reached, uninstall goes on, and says so and what a Maintainer runs
instead (`lab remove <device>`). Then it stops the agent. If
that fails, or uninstall is interrupted (Ctrl-C) before it, nothing else is changed, and while the
agent runs on, uninstall tells labd the Mac stays after all: labd puts it back in the pool (its
canceled jobs stay canceled). But `systemctl disable --now` can fail, or die of a Ctrl-C, while
systemd goes on to stop the agent, so uninstall first asks systemd whether `lab-agent.service` is
still active or enabled. If it is neither, there is no agent to keep the Mac in the lab: uninstall
leaves it `left`, says so, and finishes when run again. If labd doesn't hear that the Mac stays,
uninstall says so: `lab reset <device>` puts a Lab-owned Mac that left back in the lab and sends it
a Hard reset, as for any Mac the lab has a Sighting of (a Mac that wedged
mid-uninstall, say). Only labd's own recovery leaves a Mac that left alone. Don't `lab reset` a Mac
whose uninstall finished: it brings back a Mac with no agent, which then goes through recovery,
Hard resets and all, and ends in a call for hands. Use `lab remove` for it instead. Once the
agent is stopped, uninstall disables and removes it and the lab's other units
(`maclab-lab-boot-deadlines`, `-session` and `-lock`, as `leave` does), then its config (with the device secret), clears a lab one-shot, removes the
maclab block from `limine.conf` (or restores `custom.cfg` from its backup), the staged kernels and
the modules the lab installed, and the panic, watchdog, serial-getty, autologin and stay-awake settings, and
maclab's journald drop-in, if a newer setup wrote one. U-Boot's `ubootefi.var.pre-maclab` is kept,
not restored: it would drop any EFI variable set since. It refuses on a lab kernel. While the maclab
block is in `limine.conf`, it also refuses while Limine config enrollment is on, and says how to turn
it off: a Limine binary on the ESP with the file's checksum enrolled would not boot once the block
came out, and with `ENABLE_ENROLL_LIMINE_CONFIG=yes` any Limine update could enroll one mid-uninstall.
Without `--force`, it refuses while a job is in flight or the bootloader can't be found; with it, a
bootloader it can't find is left alone, and so is `/var/lib/maclab`. `--dry-run` prints each action,
or the refusal, without taking it. labd still lists the Mac afterwards, as `left`, holding its name:
`lab remove <device>` ends its enrollment and Retires its record at once, so the name is free to
enroll again. Its jobs, the lab's copy of its Audit log and its record stay as history. `lab forget`
is retired: `lab remove` leaves nothing for it to do.

### A Volunteer's Mac

A Mac is Lab-owned or a Volunteer's for as long as it stays enrolled; the
Join command decides which, and changing it means enrolling again. A
Volunteer's Mac is lent by its own Volunteer, signed in, from "Lend a Mac" on
the site (see [Volunteer a Mac](#volunteer-a-mac)). `lab enroll` makes Join
commands for Lab-owned Macs only, and nobody enrolls a Mac in another
person's name.

The Mac is its Volunteer's: they see it on My Macs with its jobs, its screens
and its records, and see nobody else's Mac. labd gives the Mac its own
unguessable ntfy topic when it enrolls (on the server `--ntfy` posts to), and
`setup` prints it with a QR code to subscribe from a phone. On a Volunteer's
Mac `setup` refuses `--autologin` and `--stay-awake`. Setup and every
check-in report whether `@` can be snapshotted (`/` is the btrfs subvolume
`@` and snapper has a root config for it); a Mac whose `@` can't is shown as
not eligible for Installed runs. On the Mac, `sudo lab-agent status` shows
what it is enrolled as, its topic, its Availability window and its
eligibility.

Nothing from the lab starts on a Volunteer's Mac outside its Availability
window, the baseline included, and a Mac with no window is closed. Only the
Volunteer sets it, on the Mac, in the Mac's local time:

```sh
sudo lab-agent window add mon-fri 19:00-07:00   # a window crossing midnight belongs to the day it starts
sudo lab-agent window remove mon-fri 19:00-07:00
sudo lab-agent window show
sudo lab-agent pause                     # end the current window now
sudo lab-agent pause --until 2026-11-03  # closed until that date begins
sudo lab-agent open --until 22:00        # open now until 22:00, whatever the schedule says
```

These take effect at once and work with the lab out of reach; check-ins
carry them to labd, which has no API to change them. lab-agent refuses any lab
command outside the window, and the job records the refusal. labd starts a job
on a Volunteer's Mac only inside its window, on AC power, and when the job's
expected length fits in what is left of the window; until then the job stays
`queued`. `--omt-every` refreshes wait for a window the same way. A Lab boot's
hard ceiling is the window's end if that comes before the job's end.

At the window's end, or on `pause`, lab-agent stops the job itself, on its own
clock and with the lab out of reach too: staging stops where it is, and a
Lab boot (one that hasn't reached labd yet included) reboots to the Known-good
kernel within a second or two. The job ends `incomplete`, not failed, and labd
queues it again as a new job for a later window. A lease on a Volunteer's Mac
ends with the window at the latest, and `pause` ends it. While a job holds the
Mac, lab-agent blocks suspend (`systemd-inhibit --what=sleep --mode=block`)
and releases it after; the idle lock and screensaver are never touched.
Nothing wakes a sleeping Mac: a window it sleeps through is missed.

About 5 minutes before a job's first reboot (labd's `--reboot-warning`, 0 for
none), a desktop notice in the Volunteer's session names the job and the
reboot's time, with a Pause button that does what `pause` does; the job waits
those minutes, and one warning is shown per job. With no desktop session to
show it, the job goes ahead within the window. The Audit log records, per job,
whether the warning was shown. Lab-owned Macs get no warning and no wait.

A Volunteer can also ask to be told about jobs on the Mac's own ntfy topic:
`sudo lab-agent notify --jobs on` (off by default; `off` turns it off again,
and `status` shows it). As a job starts, the notice names the job and who
asked, says the job takes at most about so many minutes, and asks the
Volunteer to leave the Mac alone until the done notice. When the job ends, a
second notice says how it ended and that the Mac is free. A Mac that still
needs hands gets that notice once it is back, and one whose job labd lost by
restarting gets it at once. Nothing else about jobs goes to the Mac's topic,
and Lab-owned Macs get none of this.

The Volunteer also chooses what Maintainers may do on the Mac, its Access
level, and can read everything done to it, its Audit log:

```sh
sudo lab-agent access qualification   # the default: no lab exec, --script or --gui-script
sudo lab-agent access full            # everything a Lab-owned Mac allows
sudo lab-agent log                    # every Maintainer action on this Mac (works offline)
sudo lab-agent jobs                   # the lab's jobs on this Mac
sudo lab-agent publish on|off         # omarchy-m-test publishing; on lists the deletion links
```

At Qualification labd rejects `lab exec`, `--script` and `--gui-script` at
submit (`403`, naming the Access level), and lab-agent refuses them at run
whatever labd sends: Qualification still takes baselines, crash tests,
built-in tests, omarchy-m-test and lab-kernel runs. Like the window commands,
`access` takes effect on the Mac at once, with labd down too. Lowering it ends
a lease at once, and a job running a Maintainer's script ends `incomplete`:
its Lab boot reboots to the Known-good kernel within a second or two. A job it
still allows keeps running. At either level the Mac's screen and live journal
are shown only while a job or lease holds it inside its window; a job's own
screenshots are still taken.

Every command labd sends carries the Maintainer it acts for (the one who
asked, or whose job it is) and its job. Before acting, lab-agent appends who,
what (with the command or script text), the job and the time to the Mac's
append-only Audit log, `/var/lib/maclab/audit.log`, and records each refusal
too. Leaving the lab leaves the log in place: it is the Volunteer's record.
Reports carry new entries to labd, whose copy Maintainers read with `lab audit
<mac>` (`GET /api/devices/{n}/audit`). `status` shows the Access level and the
latest entries.

An Installed run (`lab run <mac> --installed <manifest>`) is Qualification
work, and it runs on Lab-owned Macs only. Until Rollback exists on real Macs
labd refuses an Installed run on a Volunteer's Mac, and with it `lab
installed freeze-entry` and `lab installed promote`, through `lab`, the site
and a direct request; labd sends no Install operation to any Mac. Nothing on
the Mac refuses one: only the server does. The identity checks before, booted
and after are lab-agent's own read-only collector, not a script. A Mac at
Qualification still refuses any script, even one named `identity-*`, and
`lab exec`.

### Releases, verdicts and Promotion

A Release is listed from the moment its manifest is frozen (`lab installed
freeze-manifest`): its name, who froze it and when, its packages and its
Macs, fixed at that moment. `lab installed list` shows every Release with its
verdict; `lab installed status --release R` adds its Macs, their Installed
runs, the verdict history and the frozen files.

An M team member or a Maintainer freezes a manifest and a Mac's installer
entry and runs Installed runs, each under their own Lease on that Mac.
`lab installed freeze-entry <mac> --release R` is not an exec: labd has the
Mac run the read-only identity check and freezes the entry it finds, with who
froze it and when. It is refused without the caller's own Lease.

A Release carries one of four states: no verdict, qualified, not qualified,
revoked.

```sh
lab installed verdict --release 12.0 qualified --note "passed on j316s and j413"
lab installed verdict --release 12.0 not-qualified --note "display stays dark on j314"
lab installed verdict --release 12.0 --revoke --note "recorded against the wrong Release"
```

The first verdict is any M team member's; every later one (a change, a
revoke, a new one after a revoke) is a Maintainer's. A note is required every
time. "qualified" with no passed Installed run of the Release is refused;
everything else is a warning in the sentence `lab` shows and asks about once
(`--yes` answers it). Every verdict stays in the history with who, when and
the note. Nothing changes by itself: an Installed run that fails later adds a
warning to the Release.

Promotion (`lab installed promote <mac> --release R --job <id>`, Maintainers)
does not wait for the verdict: it needs that Mac's own passed Installed run
of the Release, and its sentence says so when the Release is not qualified or
its Qualification was revoked. Revoking undoes nothing: a Mac already
promoted stays promoted, marked "promoted from a revoked Release".

The same is on the Releases page (`/releases`, the M team and Maintainers):
the list, one Release opened in place, Record verdict for an M team member
while there is none, and for a Maintainer Change, Revoke and Promote on each
Mac with a passed Installed run. Each asks twice with the lab's sentence. The
page starts nothing: freezing and Installed runs stay in `lab`. A visitor
sees one line, on Candidates, beside the candidate set a Release matches:
"Qualified by the lab", "Not qualified" or "Qualification revoked", with the
date. A Release matches a set when a Verified run on the set lists every
package of the Release at its version, or when the set is known by the
Release's `omarchy` package (`internal/reports/candidate_sets.yml`).

### Leaving

A Volunteer can leave at any time, at the Mac or with Leave on My Macs (it
asks twice); a Maintainer can end an enrollment, with `lab remove` or with
"End enrollment" on People; and a Block ends every enrollment of the person.
Either way nothing of the lab is left on the Mac.

```sh
sudo lab-agent leave          # asks first; --yes answers for you (scripts)
lab remove ada-mbp            # a Maintainer ends the enrollment in labd
```

In the lab the Mac's record is Retired at once: it is out of the fleet, its
name is free and it counts toward nobody's cap. Its screenshots, serial log
and baselines are deleted. Its jobs with their results, labd's copy of its
Audit log and the Mac's record stay, for Maintainers (`GET /api/retired`). A
Mac the lab could not reach when it was ended learns of it when it next
reaches the lab, and removes the lab then. The leaving is one line of the
Mac's record, of the People record and for the Lab group, which names the Mac
only.

`leave` is immediate and works with labd down. It stops any job (the job ends
`incomplete`), reboots a Lab boot to the Known-good kernel, removes the lab's
boot entries (its block in `limine.conf`, or its hook in GRUB's `custom.cfg`,
leaving every other line as it was, and the staged kernels), its systemd
units, the Lab-boot autologin drop-in, sysctl and watchdog files and the device
secret, and tells labd. If labd can't be reached, lab-agent keeps trying
and finishes leaving once it has told labd. Then it prints what stays, which is
the Volunteer's: the lab-agent package, the Audit log
(`/var/lib/maclab/audit.log`, which `log` still reads), the Availability
window and the Access level; and the one command that removes the last
three, `sudo lab-agent uninstall`. Only a Volunteer's Mac has `leave`; a Lab-owned Mac's enrollment is
ended by the Maintainers.

`lab remove` (`POST /api/devices/{name}/end-enrollment`) ends any Mac, Lab-owned or a
Volunteer's, and Retires its record at once: the record is kept as history and leaves the
fleet, the name is free, its device secret is refused from that moment, whether or not the
Mac checks in again, and a job on it ends `incomplete` and is not retried. The Volunteer
is told on the Mac's own ntfy topic and, at its next check-in, on the desktop,
which Maintainer ended it. At that check-in lab-agent does what `leave` does
and appends the Maintainer to the Audit log. To enroll the Mac again it Joins
again: a Volunteer's with a new Join command from the site, a Lab-owned one
with `lab enroll`.

This is not `lab-agent uninstall` (see [Setup](#setup) above), which stays as
it was: the full teardown of a Mac set up for the lab. labd lists the Mac as
`left` until `lab remove` Retires its record, and `lab reset` (or an
uninstall that stops early) can bring it back. `leave` is the Volunteer's
one-step exit: labd Retires the Mac and refuses its secret at once (`POST
/api/agent/left`), and there is nothing to undo.

### Arriving, and what the site can take away

A Mac that Joined is at Qualification with publishing off, whatever it had in
another lab, and with no Availability window it takes nothing. The lab queues
its first baseline itself; it runs in the Mac's first open window. My Macs
reads "no window yet", then "first check pending (restarts twice)", then
"ready"; a first baseline that fails is shown and posted, and a Maintainer
looks.

Only the Mac gives Open now, raising to Full and turning publishing on:
`sudo lab-agent …` at the Mac, and labd has no route for them. The weekly
schedule is set at the Mac or, by the Mac's own Volunteer, on My Macs (`POST
/my-macs/{name}/window`, or `POST /api/devices/{name}/window` with their Lab
token): labd keeps the change and hands it to the Mac in its next check-in
answer, the Mac takes it unless its own changed later, and the card shows it
pending until a check-in carries it. Otherwise the site only takes away, for
the Mac's own Volunteer, and undoes only what it took:

- **Pause**, until a time or until Resume. The Mac is closed to the lab
  whatever its own window says, an Open now included.
- **Limit to Qualification**, until "Remove limit". The lab treats the Mac as
  at Qualification whatever Access level it has.
- **Leave**, above.

Pause and Limit stop a running job as a closing window does (it ends
`incomplete`) and end a Lease. `lab-agent` is told nothing of them: labd
applies both under what the Mac's check-ins carry. Each event (Joined, first
baseline, a new weekly window, Pause, Open now, Resume, Access level, Limit,
publishing, leaving) is a line of the Mac's record and for the Lab group. A
window opening or closing on schedule is no event.

Publishing omarchy-m-test reports is off on a Volunteer's Mac until its
Volunteer runs `publish on`, whatever `--omt-publish` or a job's `--publish`
asks; `lab publish` is refused too. Turning it off again leaves the reports
already published up. Each `publish on` lists the deletion link of every
report published so far and not deleted since.

### My Macs

`/my-macs` is an External volunteer's page: one card per Mac they lend, and
nobody else's Mac anywhere on it. An M team member or a Maintainer who lends
a Mac has the same cards under "Mine" on Macs (`/macs?show=mine`), where
`/my-macs` leads them. Above the cards: an unused Join command with its
expiry and Cancel (its line is shown only once, when it is made), and "Lend
another Mac, n of 5".

A card has the Mac's name, Configuration and state in plain words, with the
steps, the reason and the last log line when it Needs hands; its Availability
window, Access level, publishing and whether it is Eligible; its ntfy topic
and whether job Notices are on; who holds a Lease, their kind and until when;
its newest jobs with who asked and how each ended; the newest lines of the
Mac's record, the Audit log and the Exec record, each leading to the whole on
the Mac's own page; and the reports the lab published from it, each with
Delete, which asks twice and deletes for good. The job then says "deleted by
the Mac's Volunteer" and when, and the lab does not publish it again. Pause,
Limit and Leave are the card's buttons. What only the Mac can give is shown
as the command typed there, `sudo lab-agent update` among them. When the
Mac's `lab-agent` is older than the lab's, its card says so beside the
number, with that line.

A Mac whose enrollment ended is no card: the page keeps its name, when it
left and who ended it. The link a Notice carries is `/my-macs/<name>`, which
opens that Mac's card for its Volunteer once signed in, and says nothing to
anyone else of whether such a Mac is there.

### The mini's set-up and its check

`sudo scripts/mini/setup` sets the whole machine up in one run, in the order
of the spec's "Changes to make on the mini": the `lab` account and its
folders; the Homebrew formulae of `deploy/homebrew-formulae`, installed as
the account Homebrew belongs to and copied into `/usr/local/lab/tools`, which
only root can write and every daemon runs from; `container` at its pin; each
LaunchDaemon under its account; `tailscaled` in place of the Tailscale
Standalone app, under the lab's own LaunchDaemon file and joined tagged
`tag:lab`; Wi-Fi off; SSH with keys only after Unlock; no update that
restarts by itself; `autorestart 1` and `sleep 0`. `--dry-run` says what
would change. A run changes only what is not yet right, so a run that
stopped is finished by the next one, and what it cannot do yet (no Deployed
build, a key not brought) it names and passes over.

`sudo scripts/mini/check` reads the result and changes nothing: every
daemon's account, program and loaded files, the two copies of the tools,
the formulae's versions, the pins, the tailnet, SSH, Screen Sharing, Wi-Fi,
Software Update, power, and that no process has a secret in its arguments.
It exits 1 when anything is not as the set-up leaves it.

### Deploy and the Maintenance run

`sudo scripts/mini/deploy vX.Y.Z[-rc.N]` Deploys one exact version, never
"latest": it checks the bundle against the pinned release key and unpacks it
beside the running build; says so in "Agents" and drains jobs (labd starts no
further job, and the Deploy waits up to `--wait` minutes for those that run);
stops labd and copies both databases to `/var/lab/before-deploy/<version>`;
switches and restarts labd, the Out-of-band controller and the Builder, never
the mini; verifies labd on the mini and through the Front door, and asks labd
whether the Builder asks it for work and the controller answers; reports in
"Agents". When the check fails it puts the previous build back by itself. A
release that cannot be left with its databases as they are carries
`deploy/no-way-back` (its notes say "No way back."): going back from it also
restores the copy, and what arrived since is lost. A Deploy calls no
Homebrew and touches no tool.

`sudo /usr/local/lab/current/scripts/mini/maintenance` is the Maintenance
run, started by the owner from the house's network or at the mini's screen.
It says so in "Agents", drains jobs, upgrades the tools on the lab's Homebrew
list and nothing else (as the account `/opt/homebrew` belongs to), runs the
set-up (the pins, a new root-owned copy of the tools with the previous one
kept, each daemon started from the new copy), installs the macOS updates
Software Update lists (a major macOS only with `--major`), and restarts with
`fdesetup authrestart`, which asks for the account's name and password at
the terminal. After the restart the second half runs by itself:
`scripts/mini/check services`, then `scripts/mini/check tool NAME` for each
Homebrew tool (the tunnel connected, the node on the tailnet, Litestream
running with no new error, a `restic` snapshot listed, one line through
`signal-cli` to "Agents"). A tool that fails after its upgrade is switched
back to the previous copy's version, checked again, and held there until
Homebrew has another version. Then a Deploy when a newer Lab release exists
(or the one named with `--deploy`), `scripts/mini/ports` with no reset, and
the report in "Agents"; the full report, each version before and after, is
under `/usr/local/lab/maintenance/`. `--dry-run` says what the first half
would do.

### Out-of-band control on the mini

One Out-of-band controller, `oobd`, runs on the mini as a root LaunchDaemon
(`deploy/com.maclab.oobd.plist`) and sends a Hard reset through one of the
mini's three rear USB-C ports: RID 0 is the middle one, 1 the left, 2 the
right, facing the back. Any Lab-owned Mac goes on any port, with a
Thunderbolt-capable cable, and nothing else goes on those ports.

- **The tool** is the lab's fork `omacom/macvdmtool`, whose `--rid` is
  required and strictly parsed. `deploy/pins` names its commit and the
  checksum of its source; the release workflow's macOS job builds it
  (`release/reset-tool.sh`) and it reaches the mini in the server bundle.
- **Every request names its port**: `GET /v1/ports/<rid>/status` and
  `POST /v1/ports/<rid>/reset`. A RID that is not 0, 1 or 2 is refused by
  the controller before the tool, which refuses it too. One tool run happens
  at a time, so two resets asked together run one after the other.
- **Status** says, for a port: connected or not, the name the Mac shows over
  Thunderbolt, the partner's product id and the plug count. It runs no tool;
  `?probe=1` adds the tool's own answer (`nop`), still with no reset.
- **A reset may carry the port's state as it was last seen** (product id and
  plug count). The controller looks once more, under its lock, and refuses
  with 409 when the port no longer shows it.
- **Loopback only**, on `127.0.0.1:7780`. The token is in two files and on
  no command line: `/usr/local/lab/secrets/oobd.token`, which only root
  reads, for the controller, and `/var/lab/secrets/oobd.token`, which only
  the `lab` account reads, for labd (`--oobd-token-file`).

**Which Mac is on which port** is not written in any Mac's record: labd
learns it, and keeps what it learnt in its data.

- **Sighting.** Every 15 s labd asks the controller what its three ports
  show. While a Mac's Linux is up with a Thunderbolt link the port shows the
  Mac's name; when that name (the Mac's name in the lab, or the hostname its
  check-ins report) belongs to exactly one Lab-owned Mac's record, labd
  remembers the Mac, the port, the partner's product id, the plug count and
  the Mac's boot id. A port that is no longer connected, or shows another
  product id, ends the Sighting. A Mac's own restart moves its port's plug
  count, as a pull does: a higher count keeps the Sighting while the Mac
  comes back on a new boot (which takes the new count) or stays silent, as
  a Mac hung after a restart does, and ends it once the Mac is heard again
  on the boot it had, a cable pulled while it stayed up. Any conflict ends the Sightings involved and
  makes none: one name on two ports, a sighted Mac's name on another port,
  another name on a sighted Mac's port, one name in two records. labd
  keeps its Sightings in its data: after labd restarts, as at a Deploy,
  each one holds while its port is still connected with the same product
  id (the log says `sighting kept across the restart: MAC on PORT`), and
  the rules above go on from the port as it is then. The same holds after
  the mini restarts, which starts its plug counts again.
- **Hard reset.** `lab reset <mac>`, or Hard reset on the Live tab of a
  Lab-owned Mac's page, which asks twice: a Maintainer, or the holder of a
  Lease on the Mac. labd looks at the port, and the controller compares it
  once more under its lock; a port that changed ends the Sighting and the
  reset is refused with the reason. While the plug count has moved and the
  Mac is still heard on the boot it had, no reset goes out. If the Mac does not return the lab sends
  one more, then asks for hands. Refused for a Mac the lab cannot place, and
  always for a Volunteer's Mac.
- **Proving reset.** `lab reset <mac> --prove left|middle|right`, Maintainers
  only, for a Mac that shows no name (hung when cabled, a USB 2 cable, a
  kernel with no Thunderbolt): the lab resets that port and places the Mac
  there only if it returns on a new boot and every other Lab-owned Mac keeps
  its boot id.
- **What a Mac shows.** `lab status`, `lab oob <mac>`, Macs and a Mac's page
  say sighted, not sighted (a connected port with no Sighting could be its
  cable) or no cable, and why no Hard reset reaches it.
- **The controller unhealthy.** A reset is refused with the reason, each Mac
  is shown as having no Hard reset, jobs still run, and "Agents" gets one
  line once that has lasted 2 minutes, and one when it answers again.
- **After labd starts**, it gives each Lab-owned Mac that was up when it
  stopped its own watchdog's time and a boot, runs the ladder on whichever
  did not return, and writes the result for "Agents".

`sudo scripts/mini/setup` makes the token, installs the LaunchDaemon file and
starts the controller once a build is Deployed. `sudo scripts/mini/ports`
checks all three ports with no reset: what the mini sees, the tool's answer,
the controller's, and that a bad RID is refused by both.

**The eight tests of this wiring** (Ready item 2) are
`scripts/cutover/out-of-band/oob01-tool` to `oob08-refusals`, with `run`
around them. With no option a test says its steps and what each expects and
sends nothing. `--local` runs it against a labd on loopback and a made-up
mini (`tools/fake-mini`: the controller's own code over made-up ports, with
simulated Macs on its cables), which proves the script and the lab's rules,
not the hardware. `--lab --mini USER@HOST` runs it through the Lab address
on the real ports: it resets real Macs, stops wherever a cable or a hand is
needed, and writes `docs/acceptance/YYYY-MM-DD-out-of-band-NN.md` and its
line in `docs/acceptance/out-of-band.results`. `run --results` is green when
all eight say pass. `scripts/mini/ports --seen` is what the tests read on
the mini: one line a port, no root, no tool.

**Hardening's fourteen tests** (Ready item 8, spec section 12) are
`scripts/cutover/hardening/h01-console` to `h14-volunteers-mac`, with `run`
around them, in the same three ways. `--local` starts a labd on loopback
with a stand-in for GitHub that signs in an Admin, two M team members, a
Volunteer and a person with no role, and simulated Macs, one of them lent
through the Join command. `--lab` runs through the Lab address under the
saved login; where a test acts as the owner's browser, the session cookie
is typed at a prompt that shows nothing. Each writes
`docs/acceptance/YYYY-MM-DD-hardening-NN.md` and its line in
`docs/acceptance/hardening.results`; `run --results` is green when all
fourteen say pass. Test 14 runs once, while the M2 Max is the owner's own
Volunteer's Mac (step 14a). `sudo scripts/mini/secrets-seen` says whether a
secret of the mini shows in a process or a service definition: counts and
file names only.

### The Signal sender on the mini

labd tells two Signal groups what happens, a line for each event: the Lab
group ("Omarchy Mac Lab") hears of Leases, of every event of lending a Mac
and, at 08:00 Brisbane, the daily summary; the owner's group "Agents" hears
of each change of who someone is, each new Lab token, each cut a Maintainer
makes and the lab's alerts. Every line is recorded in `labd.db` first. A
line goes at most once, and a send that fails holds nothing up. The lab only
sends: it reads nothing from a group and keeps no phone number.

- The sender is `signal-cli`, a LaunchDaemon of the `lab` account
  (`deploy/org.omarchy-mac.lab.signal-cli.plist`), run from the root-owned
  copy of the Homebrew tools. labd reaches it over a socket only `lab` can
  open (`--signal-socket`). Its number is the lab's own, linked to nobody's
  phone; its keys are files of the `lab` account.
- labd reads each group's id from a file in the folder `--signal` names:
  `signal-lab-group` and `signal-agents-group`. A group with no file gets
  nothing: its lines are recorded and not posted, then or later.

`sudo scripts/mini/setup` installs the LaunchDaemon file and starts the
sender. `sudo scripts/mini/signal` does the rest at a prompt: `register` the
number, list the `groups` it was added to, give labd a `group`'s id, and
`test` with one line to each group.

### What labd watches, and `/status`

Once a minute labd looks at the lab's own parts. "Agents" gets one line when
a part goes wrong and one when it is well again, never the same thing twice,
and it holds across a restart of labd:

- the lab is back after the mini restarted, with how long it was Dark;
- labd, or a daemon launchd keeps alive, started 3 times in 10 minutes;
- the tunnel disconnected for over 5 minutes;
- Litestream not running, or logging errors, for 2 minutes;
- the last good backup older than 36 hours;
- a failed Restore drill, and the next one that passes;
- the disk under 15 % free;
- the Builder silent, or the Out-of-band controller not answering, for 2 minutes;
- the Alias no longer leading to the Lab address, once it has;
- `container`'s server found stopped and started again, each Deploy and each
  Maintenance run, which the mini's scripts say themselves.

On Monday at 09:00 Brisbane a weekly digest goes there too: what waits for a
Maintenance run (macOS, and the lab's Homebrew tools that have a newer
version), and the dates of the last good backup and Restore drill.

`/status` shows the server, the tunnel, the Builder, the Out-of-band
controller, the last good backup, the last Restore drill and the last
restart, as labd last saw them. Anyone may open it, so it carries states and
times and no reason, address or path; it is never cached.

- labd sees the disk, the Builder, the controller and the marker files
  (`--ops-marks /var/lab/log`: `backup.ok`, `restore-drill.ok`) by itself.
  What only the mini can say comes from two scripts labd runs as the `lab`
  account (`--ops-scripts`): `scripts/mini/parts` every minute (the boot
  time, the tunnel as `cloudflared` itself answers on its loopback port,
  Litestream, how often launchd started each daemon) and
  `scripts/mini/pending-updates` for the digest. Neither changes anything,
  needs root or runs Homebrew.
- A script of the mini tells "Agents" a line with
  `scripts/mini/notify agents TEXT`, or `notify trouble KEY TEXT` and
  `notify recovered KEY TEXT` for something that lasts. The line goes to labd
  on a second socket only `lab` can open (`--ops-socket
  /var/lab/run/ops.sock`), as the request's body: no token, and nothing in
  an argument. That socket takes a line and nothing else; the tunnel's has no
  such route. The line is also written to `/var/lab/log/notify.log`.

## Live data: stream, screen, logs

Everything a page needs is on the API. The site uses it now, and a TUI can use it later.

A Mac's page (`/macs/<name>`, the Mac's name on Macs) has three tabs. Overview: its facts, its Lease
with Lease, Renew and Release, its jobs, and the way to "Boot a kernel". Live: its screen and its
logs. Records: the Lease record, the Mac's record, the Audit log (Maintainers and the Mac's
Volunteer) and the Exec record. An M team member has Live only under their own Lease, and is
otherwise told who holds the Mac and until when; a Volunteer's Live of their own Mac is view only.
The page has no serial console.

- `GET /api/stream`: server-sent events for `device`, `job`, `kevent` and `screen` (`?device=` limits it to one Mac).
  A Mac's `kevent` and `screen` events reach an M team member only under their own Lease.
- `POST /api/devices/{n}/screenshot`, `GET .../screen`, `.../screens`: captures come from the display
  controller via `ffmpeg kmsgrab`. That works from the moment the display driver loads (boot splash,
  text console, emergency shell, login screen, desktop), with `grim` as a fallback. Jobs capture
  `screen/after-boot.jpg` and `screen/after-tests.jpg`.
- `GET /api/devices/{n}/logs?source=journal|kernel&lines=N`: live from the Mac.

The serial console is not part of Omarchy Mac Lab: a Hard reset is the only out-of-band function.
`lab shell`, `lab console`, `lab serial` and `lab recovery-mode` say so, and labd refuses the console
routes, the serial log, the serial-assisted recovery mode and a `lab exec` on a Mac that is off the
network with the same line.

## Build from a GitHub URL

```sh
lab run m1air --kernel https://github.com/iconidentify/aurora-linux/tree/custom/sep --gui-test gui-smoke
lab build https://github.com/AsahiLinux/linux/pull/123 --device m1air
lab build https://github.com/AsahiLinux/linux/pull/123 --config ./my.config   # one build for every Mac
```

Agents do the same with `lab_run(source=...)`, `lab_build` and `lab_build_log`, and use `lab_exec`
instead of ssh. labd resolves repo, branch (`/tree/...`), commit and PR URLs (or `git-url#ref`) to
a commit with `git ls-remote`. It keys the build on that commit, the target Mac's running
`/proc/config.gz` and the kbuild recipe, and reuses an identical earlier build.

`lab-builder` runs on the mini as a root LaunchDaemon. Root is needed because macOS Local
Network privacy blocks user processes. It runs `builder/kbuild` in an Apple `container` micro-VM
(`builder/Containerfile`: Debian, gcc, clang, LLD, Rust 1.93.1, bindgen), started in the user's
session. A config with `CONFIG_CC_IS_CLANG=y` (linux-aurora's: clang 18 + LLD) is built with
`LLVM=1`, any other with gcc; the log's `toolchain:` line says which. Debian's clang is 19, not
18. After `olddefconfig`, a `config:` warning lists every `=y`/`=m` option the build dropped and
any `CC_IS_*`, `LD_IS_*` or `AS_IS_*` change. All
state is one size-capped volume (`container volume create -s 40G maclab-cache`): one shared
depth-1 git store, one reused worktree, at most two build dirs, and an 8 GB ccache. The host scratch
folder (`--io`) is emptied after every build. On the mini `lab-builder` runs as root and the VMs
under another account, so the scratch folder is root's (`/usr/local/lab/io`, made by the set-up) and
`lab-builder` refuses one that another account can write or that is a link: a build's input is in a
folder the VM reads as `/in`, and the one folder the VM writes, `/io`, is read back with no link
followed. An image built before this reads its input from `/io`: rebuild it. labd's artifact store is
garbage-collected to a budget (`--artifact-budget-gb`, default 20).

Future `kbuild-2` builds also publish `module-build.tar.zst` as a named output.
`lab build-get <build-id> -o ./build` fetches it through the existing artifact store.
It contains the kernel's external-module header installation, `Module.symvers`,
`vmlinux`, the generated config, and the original input config. `IDENTITY.json`
records the actual source commit, compiled compiler/linker string, release, and
file hashes, including generated headers. This archive targets external C modules
on the builder architecture; it does not bundle a toolchain.
The builder refuses missing completed-kernel inputs or changed source files.
The kernel source must provide `scripts/package/install-extmod-build`; a missing
installer fails the build rather than publishing incomplete headers.
Deploy the matching builder image and controller together; older builder outputs
cannot satisfy the new recipe. Existing builds retain their original identity.
No new archive proves compatibility with an older build, or proves a module loads.

A builder host without Apple `container` can build with Docker instead (Colima, Docker Desktop):
`lab-builder --runtime docker [--context colima]`, with the same `kbuild` image and `/cache`
volume layout. labd sees no difference: progress, logs and reuse of earlier builds work the same.
Colima shares only paths under `$HOME` into its VM and a bind mount of anything else is silently
empty, so a build's input (`/in`) is copied into its container and its outputs (`/io`) back out with
`docker cp`, never mounted.

```sh
docker build -f builder/Containerfile -t maclab-kbuild:1 builder/   # -f: the file is not named Dockerfile
docker volume create maclab-cache
lab-builder --runtime docker --context colima --cpus 12 --memory 24G --pkg-image '' ...
```

`--pkg-image ''` takes kernel builds only: the package builder (`builder/pkg`) has not been tried
under Docker.

Docker volumes have no size cap of their own: the cache is bounded only by what kbuild keeps on it.

Measured on a base M5 (10 cores, VM with 16 GB) with aurora's config: first build 9 minutes;
rebuild of a warm tree about 40 seconds. Progress (stage, object count, ETA) and the log stream
live on `/api/stream` (`build`, `buildlog`); `lab build-log <build> -f` follows the log by asking
`/api/builds/{id}/log?offset=N&wait=50` again from where it stopped, each call at most 50 s.

### The Builder

The lab has one Builder: `lab-builder` on the mini, as root, for kernels and packages, one build at a time.

- **Its limits.** A build gets 10 of the mini's 18 cores and 16 of its 64 GB. `--cpus` takes 10, 8 or 6 and nothing else (the
  steps after a failed gate test); the memory is no setting. The kernel cache volume is 40 GB, the package volume 120 GB, the
  artifact store 20 GB.
- **Its token.** A Maintainer adds the builder and gets a token of its own, once; labd makes and accepts no shared builder
  token. On the mini the token sits in `/usr/local/lab/secrets/builder.token`, which only root reads, and lab-builder takes it
  from `--token-file` alone: there is no `--token`, and no variable holds one.
- **What the token does.** The builder takes queued builds of its kinds (`kernel`, `package`). For the build it runs it reads
  that build's inputs, reports progress, uploads outputs and finishes it, naming only what it uploaded for that build. An
  artifact is served to it only as the input of the build it is running. It reaches no other route.
- **Sources** are public. labd takes `https://github.com/<owner>/<repo>` (a branch, commit or pull request of it), and another
  host only when labd is started with `--source-host <host>`. An address that carries credentials is refused. A refused source
  is never contacted. The Builder holds no GitHub token.
- **Who builds.** Maintainers. An M team member's `lab build` and `lab package` are refused in one line until M team builds
  open. An M team member uploads an artifact only for a job on a Mac they lease (`lab run <mac> --kernel FILE`, or
  `lab upload FILE --for <mac>`). Every build records who asked.
- **`container`.** With nobody logged in nothing starts its server, so a LaunchDaemon does, at boot and every minute after
  (`scripts/mini/container watch`), in the session of the account that owns `container`'s state, and tells "Agents" a line on
  each change. lab-builder never starts it; its own LaunchDaemon waits until the server answers.
- **A labd restart fails a running build**: the builder's claim on it is kept in labd's memory. Ask for the build again.

```sh
lab builder add mini --kinds kernel,package -o mini.token   # as a Maintainer; the token goes only into this new file (0600)
sudo scripts/mini/setup --container-user <account> --builder-token mini.token   # on the mini
lab builder list                                            # last seen, current build
lab builder remove mini                                     # its token stops working; what it was building fails
```

**A builder's kernels boot as root on lab Macs.** What the artifact store's 20 GB budget clears is in
[docs/builder.md](docs/builder.md).

## omarchy-m-test: hardware checks in every job

Unattended reports retain the IDs of skipped on-screen video checks and checks
that passed in their reference but were not run. This missing coverage survives
known-good reference updates. Such a run may pass its executed tests, but it
cannot qualify as a fully compared release-gate pass until those checks run.

On a Mac with a desktop user, every job also runs
[omarchy-m-test](docs/omarchy-m-test.md) in the desktop
session: about 70 checks of the boot chain, drivers, GPU, video decode, display,
audio, Wi-Fi, power and CPU, and short benchmarks. Before each run the agent installs
the latest tool from the release the lab's own install line (`<Lab address>/install`)
names: it reads that line for where the release is and never runs it, and installs
the release only once its signature verifies against the release key lab-agent
pins, the check `lab-agent update` makes. It runs with
`--unattended`: it answers the disclaimer and gets no more input, every check meant
for a person is skipped, and the speaker tone, brightness steps, camera window and
disruptive sections (lid, Wi-Fi reload) don't happen. The run takes 1 to 3 minutes.

- **On-screen video checks:** on 0.1.15, Lab-owned Macs skip `video.h264-on-screen`
  and `video.hevc-on-screen` in an unattended run. Once
  maralcbr/omarchy-m-testing#61 ships they run again, and skip under a lock or a
  full-screen overlay. A Volunteer's Mac leaves them out either way (`--skip video`).
- **What else it uses:** the built-in microphone for 3 seconds and 30 camera frames,
  measured and not kept. Where passwordless sudo works, it sets the battery charge
  limit and puts it back.
- **Older tools:** a tool older than 0.1.15 (a failed upgrade) has no `--unattended`.
  The job logs that and runs without it, so the tone, brightness steps and camera
  window happen on that run.

- **Known-good runs** (`lab baseline`, `lab run <mac>` with no kernel) become the
  Mac's reference report.
- **Lab kernels** are compared with that reference. A check that passes on the
  known-good kernel and fails on the test kernel is a regression and fails the job.
  `boot.files` and `boot.kernel-package` are listed apart: they fail for any
  kernel that isn't an installed package. A kernel that lacks a feature on
  purpose names what it's expected to fail: `--omt-allow system.failed-units`,
  or `--omt-allow hardware.drivers:<compatible>` for one device-tree node, which
  still fails the job if any other node stops binding. When `hardware.drivers`
  fails on both kernels, the unclaimed nodes are compared compatible by
  compatible, counting nodes: more nodes of a compatible left unclaimed on the
  test kernel than on the known-good one (none then, or 3 against 1) regress as
  `hardware.drivers:<compatible>`, and fewer count as fixed. A reference
  recorded before the lab counted nodes compares compatibles only.
- **Skipped checks:** a skip on either side compares as nothing. Against a reference
  from an attended run, an unattended run names what it skipped that the reference
  passed in "compared with". Missing coverage persists across known-good
  reference updates, and prevents a fully compared release-gate pass. An Installed run is stricter: a frozen pass that is now a skip is a
  regression. On 0.1.15 that means taking preinstall references frozen from an
  attended run again. Once the on-screen checks run again it means a locked or
  covered screen fails the run, so run Lab-owned Macs with `--autologin` or
  `--stay-awake`, and run it again if the screen was locked.
- **Publishing:** the lab publishes a run's signed report on its own public
  pages, through the checks every upload meets (schema, signature, check ids,
  evidence), handed over inside labd. The one signature is the Mac's Machine
  key. Only a Known-good run on a packaged kernel qualifies, because a
  report's build is named from the installed packages, and those don't
  describe a test kernel; an Installed run is never published.
  - **By itself** the lab publishes a run that became the Mac's Reference:
    each baseline, and the scheduled refresh once a week.
  - **By hand**: `lab run <mac> --publish`, `lab publish <job>`, or the job
    page's Publish button, for a Maintainer on any job and an M team member
    on their own.
  - A Volunteer's Mac is published only while its Volunteer has publishing on
    (see above), read at the moment of publishing. The deletion link of each
    of its reports is kept for `lab-agent publish on` to list.
  - The same report twice answers "Already uploaded" with the report's link.
  - `lab job`, `lab jobs` and the job's page say what became of the report:
    not published, published, hidden or deleted. After a delete the job says
    who deleted it and when, and the lab does not publish that job again.
  - Reports shows Maintainers how each report arrived: by the Mac's own tool,
    or by the lab with the job and who asked. A lab run's public page says
    "run by the lab" and names nobody.
  - With Preview on, the lab publishes nothing.

```sh
labd                                 # publishes baselines and a weekly refresh by itself
labd --omt-publish=false             # publish by hand only
labd --omt-every 0                   # no scheduled refresh (it reboots the Mac)
labd --omt=false                     # don't add omarchy-m-test to jobs
```

## Release packages from a PKGBUILD

```sh
lab package ~/recipes/linux-aurora                        # the recipe exactly as written
lab package ~/recipes/linux-aurora --pkgrel 11.14 \
    --commit https://github.com/iconidentify/aurora-linux/commit/<sha> -o ./rel
lab run m1air --kernel <build-id>                         # boot the kernel package once
lab build-get <build-id> -o ./rel                         # packages + the PKGBUILD as built, sha256-checked,
                                                          # and the uploaded recipe (patches, data files) in ./rel/recipe/
```

A kernel build (above) makes a lab test artifact. A package build makes the release: it runs a
PKGBUILD recipe with `makepkg`, the way the release machine (m1) does, and returns every package it
makes (`linux-aurora`, `linux-aurora-headers`, ...) plus the PKGBUILD as built. The CLI uploads only
the recipe's own files: the PKGBUILD, its local sources, and install/changelog files. makepkg
downloads the rest. `--commit` repoints `_commit` (the PKGBUILD must download from that repository),
`--pkgrel` sets pkgrel, and either one refreshes the checksums with `updpkgsums`. Agents use
`lab_package`, then `lab_build_log`, then `lab_run(build_id=...)`.

The builder is `builder/pkg`. It is Arch Linux ARM, the release machine's `/etc/makepkg.conf`
verbatim, and `maclab.conf` with the one difference: packages are `.pkg.tar.zst`. ccache stays
off, as on the release machine: its base-dir path rewriting changes `__FILE__` strings inside
modules. When the image is built, gcc, glibc, binutils, rustup, bindgen and pahole come out at the
same versions as on an up-to-date Arch Linux ARM machine. Rebuild the image to follow the distribution:

The image starts from Arch Linux ARM's root file system, `ArchLinuxARM-aarch64-latest.tar.gz`, which
no script of the lab downloads: Arch Linux ARM publishes it over plain http (`os.archlinuxarm.org/os/`),
so it counts only once its signature is checked. Fetch the archive and its `.sig` on any machine with
gpg and check them against Arch Linux ARM's build key before the archive goes anywhere near a build:

```sh
gpg --recv-keys 68B3537F39A313B3E574D06777193F152BDBE6A6   # Arch Linux ARM Build System
gpg --verify ArchLinuxARM-aarch64-latest.tar.gz.sig ArchLinuxARM-aarch64-latest.tar.gz   # want: Good signature
```

On the mini the set-up takes it (`scripts/mini/setup --builder-rootfs FILE`): it copies the archive into
a build context only root writes (`/usr/local/lab/pkg-image`), beside the Deployed build's `builder/pkg`,
says its SHA-256, and builds the image from there. With no archive the Builder's step stops before
`container`'s server is started and says what to bring.

On the mini each image is tagged by what it is made from: `maclab-kbuild:<tag>` with the first 12 hex
of a SHA-256 over the paths and contents of `builder/` (not `builder/pkg`), `maclab-pkgbuild:<tag>`
over `builder/pkg` and the root file system's archive. The set-up builds a tag it does not find,
writes both into the Builder's LaunchDaemon file (`--image`, `--pkg-image`) and starts it again,
and removes the images it made before, keeping the last as the way back. A Deploy whose `builder/`
changed says to run the set-up; until then the Builder runs the images it had, and
`scripts/mini/check` and `build-watch` say so. Elsewhere:

```sh
# in a directory holding builder/pkg/* and the checked rootfs tarball
container build -t maclab-pkgbuild:1 .
container volume create -s 120G maclab-pkgcache   # once; a kernel tree needs ~35 GB while building
```

Checked against a linux-aurora build of the same recipe on an Arch Linux ARM machine: the file
lists and package metadata are identical. Contents are byte-identical except where the build
directory's path leaks in: ELF build-ids, the source paths Rust drivers embed for panics
(`/cache/work/b/...` here), and the build timestamp in `vmlinuz`. A cold build takes 8 to 12
minutes on a base M5.

Recipes take their parallelism from `OMARCHY_BUILD_JOBS` (`make -j"${OMARCHY_BUILD_JOBS:-10}"`). lab-builder gives package builds
`--pkg-jobs` (default: its `--cpus`), and `builder/pkg/pkgbuild` passes it through to makepkg past sudo. An image built before this
ignores it: rebuild the image, under a new tag if another lab-builder still uses the old one.

The 30+ GB work tree is deleted after every build. The volume keeps downloaded sources (the newest
six) and the Rust toolchains. `lab-builder` takes package builds only when it has
`--pkg-image` (the default, `maclab-pkgbuild:1`; on the mini, the set-up's tag). An older builder only ever gets kernel builds.

## Release gate

`lab gate` answers whether a kernel has passed on real hardware for every supported chip family,
from the jobs already run. It covers the kernel only: lab boots use each Mac's installed m1n1,
U-Boot and device trees (see "Kernel artifacts"), so it says nothing about a boot-chain change.

```sh
lab gate <build-id | kernel sha256 prefix>                    # all six families must pass
lab gate b1015-120000-ab12 --families m1,m2 --evidence gate.json
lab gate b1015-120000-ab12 --volunteers                       # count volunteers' Macs too
lab gate b1015-120000-ab12 --exclude j1015-093012-ab12        # set a lab-caused failure aside, this once
lab exclude j1015-093012-ab12 --reason "the access point went down mid-test"   # ... in every report
lab exclude j1015-093012-ab12 --clear                         # count it again
```

Families come from the SoC compatible in each Mac's device tree (or, from an agent older than that
fact, its device-tree model): `m1` (t8103), `m1-pro` (M1 Pro/Max/Ultra: t6000-t6002), `m2` (t8112),
`m2-pro` (M2 Pro/Max/Ultra: t6020-t6022), `m3` (t8122) and `m3-pro` (M3 Pro/Max: t6030, t6031,
t6034). M3 Ultra and M4 and later are not covered: those Macs are listed as not counted.

Only plain runs count: ones that boot the Mac's known-good cmdline (no `--cmdline`,
`--cmdline-strip` or `--cmdline-base` other than `known-good`; `--verbose-boot` is fine), run at
least the tests every job on that Mac runs (`boot-health`, and `omarchy-m-test` where labd runs it,
each known by its builtin whatever the job named it), and have no `--omt-allow`. Where labd runs
`omarchy-m-test`, a pass counts only if its report was compared with the Mac's known-good run of
it: with nothing to compare with, nothing could regress, so such a pass is listed as not counted
("no OMT reference"; `lab baseline` makes the reference). A run with a changed cmdline boots
something a release doesn't, so it counts neither way. A run that tested less counts when it
failed, since it failed even so, but not when it passed. Canceled, `infra_error` and `build_failed`
jobs say nothing about the kernel and are skipped, and so are crash tests, which crash it on
purpose. So is a `tests_failed` job whose every failed test never started, because bash couldn't
open the test's script, and whose kernel logged nothing fatal: the last thing the test printed is
bash's own error for its `test-<name>.sh`, exiting 126 for `Permission denied` (as for
`omarchy-m-test` when the desktop user can't read the job's directory) or 127 for `No such file or
directory`. A test that ran and exited 126 or 127 itself, say for a binary it couldn't execute
(`Exec format error`) or a command it couldn't find, failed like any other, and other trouble
running a test (a script that didn't download, no desktop session) looks like the kernel's in the
job's record, so it still counts as a failure. A job canceled after its kernel had already failed
(a panic, a reboot or hang during a test, a test that failed by itself) keeps that failure as its
outcome, with the cancel in its summary; tests the cancel cut short are the cancel's. Every other
outcome but `pass` is a failure.

Each job records its Mac as it was when the job was submitted: its chip family, whether it was a
volunteer's, and whether labd ran `omarchy-m-test` in its jobs. The gate judges the job by that,
so what the Mac became since (or another Mac enrolled under its name after `lab remove`) doesn't
change the verdict; a job from before labd recorded it is judged by the Mac as it is now. A Mac
the lab no longer has (`lab remove`) doesn't count: its runs are listed as not counted, "device gone",
under their family, or under the report when the job didn't record its family.

Each Mac fails when it failed on the kernel and never passed, passes when it passed and never
failed, and is **flaky** when it did both, in either order: a `lab retry` that passes doesn't hide
the failure before it, and the report lists both jobs, a retry naming the job it retries
(`retry_of`). `stage_failed` means the kernel never booted (the Mac couldn't stage it, e.g. its
boot partition was full): it counts only for a Mac with no pass or failure on the kernel, so a run
that couldn't stage never hides another result. A family's result is decided in this order:

1. **fail** when any of its Macs failed;
2. **flaky** when any of its Macs both passed and failed;
3. **pass** when at least one passed;
4. **couldn't stage** (`stage_failed`) when its only results are Macs that couldn't stage the
   kernel: the gate stays shut, and the report gives the job's reason to fix before running it again;
5. **untested** when none of its Macs has a result, including when it has no Mac in the lab.

Volunteers' Macs (see [Volunteer a Mac](#volunteer-a-mac)) run agents the lab never updates
from its side, so they count only with `--volunteers`; otherwise the report lists them
and their runs apart. A staging problem on one Mac doesn't hide a pass or a failure on another.
The gate is open only when every required family passed. `lab gate` exits 3 when it is shut.

A failure the lab caused can read as the kernel's: an access point that goes down during a test
makes the Mac look lost, `crashed_or_hung`, and that failure would keep its family flaky for good.
`--exclude j1,j2` sets those jobs aside for that one report. They are listed as excluded, in
`excluded` and, marked `excluded`, in their family's `not_counted`, and the summary names them, so
the decision is on the record; nothing is kept, so a `lab gate` without it counts them again. Only
finished jobs of the kernel can be excluded.

`lab exclude <job> --reason "..."` keeps a job aside instead: labd marks the job (`gate_exclusion`:
who set it aside, why and when), and every `lab gate` after that, release scripts' included, sets it
aside as `--exclude` would, without being asked. The report lists each such job of the kernel in
`exclusions`, with who, why and when, and the summary names them; a run it sets aside that would
have counted is in its family's `not_counted`, marked `excluded`, with the mark as `exclusion`.
`lab job` shows the mark too. It is a Maintainer's and takes a reason, and only a finished job; the
person is the Maintainer whose token asked. `lab exclude <job> --clear` takes it off, and
the job counts again. It is for a failure the lab caused, never for one of the kernel's that would
keep a release back. Jobs from before labd kept the mark have none, and count as before. The API is
`POST /api/jobs/{id}/exclude` with `{"reason": "...", "by": "..."}`, and `DELETE` to clear; agents
use `lab_exclude`.

Results are strings: a family's is `pass`, `fail`, `flaky`, `stage_failed` or `untested`, and a
run's `pass`, `fail`, `flaky` or `stage_failed`. A script that looks for `"fail"` must treat
`"flaky"` as not passing too. The report's own `result` is only ever `pass` (the gate is open) or
`fail`.

`--evidence` writes the report as one JSON evidence record:
the kernel (sha256, build, release), `result` (`pass` or `fail`), and per family its result and
runs (device, job, outcome, per-test results, the deciding run first; `reason` for a run that
couldn't stage, or a flaky Mac's other result; `retry_of` for a retry), and `not_counted`, the runs
that decide nothing, each with its reason (`volunteer` marks a volunteer's Mac, `excluded` a job
`--exclude` or `lab exclude` set aside, and `exclusion` the latter's mark). Then `volunteers` names
the volunteers' Macs, `volunteers_counted` says whether they counted, `excluded` lists the jobs
`--exclude` set aside, `exclusions` those `lab exclude` did (job, device, outcome, by, reason,
time), `not_counted` holds the runs of Macs the lab no longer has whose jobs didn't record their family, and the
labd host. The API is
`GET /api/gate/{kernel}?families=m1,m2&volunteers=1&exclude=j1,j2`; agents use `lab_gate`.

## Kernel artifacts

A tarball in the Arch package layout: exactly one
`usr/lib/modules/<release>/` holding the modules and `vmlinuz` (or `Image`).
A `linux-*.pkg.tar.zst` works as-is. From an `O=` build tree:

```sh
lab pack --build ~/linux/build    # the O= dir -> kernel-<release>.tar.zst
```

The release string must be unique (set `LOCALVERSION`). The agent refuses to
install over a kernel it didn't install, including the running one.

The device tree comes from m1n1 on the ESP, not from the test kernel, and that includes a
release package booted with `lab run`. A kernel that needs new DT bindings needs a matching
m1n1 stage 2. A lab boot doesn't exercise device-tree changes. [Two installs per Mac](docs/ab-installs.md)
proposes how it could.

## For AI agents

`lab mcp` is an MCP server on standard input and output: give your agent the
command `lab mcp`. It acts as the person whose login `lab` holds, and the lab
records what it does under their name.

Its tools follow the commands one for one, each named after its command:
`lab_status`, `lab_enroll`, `lab_remove`, `lab_device`, `lab_baseline`,
`lab_crashtest`, `lab_run`, `lab_jobs`, `lab_job`, `lab_wait`, `lab_cancel`,
`lab_logs`, `lab_retry`, `lab_publish`, `lab_gate`, `lab_exclude`,
`lab_lease`, `lab_release`, `lab_exec`, `lab_reset`, `lab_oob`, `lab_audit`,
`lab_upload`, `lab_build`, `lab_package`, `lab_builds`, `lab_build_log`,
`lab_build_get`, `lab_installed_list`, `lab_installed_status`,
`lab_installed_freeze_manifest`, `lab_installed_freeze_entry`,
`lab_installed_verdict`, `lab_installed_promote`, `lab_agent_update`,
`lab_builder_add`, `lab_builder_remove`, `lab_builder_list` and
`lab_version`. Four commands have no tool: `login` (the Lab token is typed by
its person and never handed to an agent), `mcp` itself, and `pack` and
`boot-test`, which work on files of your own machine and ask the lab nothing.

- A tool never answers with a token. `lab_enroll` and `lab_builder_add` write
  the Join command and the builder's token into a new file you name, and say
  only where it is.
- `lab_installed_verdict` and `lab_installed_promote` first answer with what
  the lab is about to do, warnings included, and do it only when called again
  with `confirm`: the verdict is a person's.
- `lab_exec` needs your own Lease (`lab_lease`), and its text is kept in the
  Exec record for 12 months: never put a secret in a command.
- A Mac in `needs_hands` is waiting for a person. Tell the user; don't retry.
- Take `lab_lease` before a series of jobs that must not interleave with
  others' (benchmarks). A Mac someone else leases is theirs until the Lease
  ends: wait, or use another Mac.

A lab is worth describing to its agents once: which Macs you may use and what
they must not touch. Write that in a short Markdown file and hand it to them
with a prompt.

## Verified on hardware

On an M3 Max host (J516cAP, macOS 27.0.1) driving an M1 MacBook Air (J313) through its DFU port:

- `macvdmtool reboot` resets the target from an M3 host: back 37 s later. (It was already verified
  from an **M5 Pro** host, J704AP, on macOS 26.5.) With the watchdog stopped at runtime (systemd's
  `RuntimeWatchdogSec` set to 0) and `kernel.panic=0`, a crashed Air stayed down for 2 minutes until
  `macvdmtool reboot` brought it back in 36 s.
- Apple Silicon's U-Boot arms `apple_wdt` and leaves it running into Linux: booting with
  `watchdog.handle_boot_enabled=0` logs `watchdog0 running and kernel based pre-userspace handler
  disabled` within the first second. With `watchdog.open_timeout=N` (lab boots don't set it today),
  a boot that hangs or gets stuck before something opens `/dev/watchdog` is reset: a test boot stuck
  in the initramfs waiting for its root device came back on its known-good kernel by itself.

From the sources, not measured: m1n1 disables the watchdog, so m1n1 itself runs unwatched, and
U-Boot feeds it while U-Boot or Limine waits, so a boot sitting at a prompt or a menu stays up. Once
Linux runs, the kernel feeds it until something opens `/dev/watchdog` (forever without
`open_timeout`), and a healthy boot must open it in time too. `lab-agent setup`'s
`RuntimeWatchdogSec=30s` has systemd open it once the real root is up (the initramfs doesn't get
that setting), and systemd then keeps it fed for as long as it runs, even through a boot stuck in
emergency mode.

## Not yet verified on hardware

- `lab installed freeze-entry` on a real Mac: the identity check sent outside
  a job, and its observation coming back. labd sends no Install operation, so
  lab-agent's install path stays unproven on hardware.
- Whether `apple_wdt` can be armed before the kernel starts (m1n1/U-Boot).
  If it can, tier 1 would also cover early-boot hangs.

## Development

`make test` runs the unit tests and eight end-to-end scenarios against a
simulated Mac (`internal/agent/sim.go`):

- A good kernel
- Panic at boot
- Hang, recovered by a Hard reset
- Silent hang, Hard reset after its boot timeout
- No Sighting or no controller, human power cycle, then queued jobs resume
- Crash during a test
- Deliberate crash test
- A failing test

`lab-agent sim --server .. --token ..` enrolls a simulated Mac against a real labd.

## Volunteer a Mac

Lending starts on the site. `./volunteer` is retired, and nothing looks for
the lab: every line a person is told to run names the Lab address and nothing
else.

1. Sign in with GitHub and open "Lend a Mac". The page opens with the Test
   machine rule, then what the lab may and may not do, then the terms.
2. Leave to lend is needed once per person. The M team and Maintainers have
   it; a person a Maintainer gave an Invite has it from their first sign-in;
   anyone else sends a Request to lend from the page and a Maintainer answers
   it on People. The page shows waiting, approved or declined; the lab sends
   nothing.
3. Name the Mac on the page and copy the Join command: one line, good for one
   Mac, once, for 1 hour. A person holds one unused Join command; a new one
   cancels the old. Run the line on the Mac, as yourself.

A person lends up to 5 Macs, an unused Join command counting as one and Macs
that left not counting. A Maintainer raises one person's cap on People;
Maintainers have none. A Mac's name is lowercase letters, digits and dashes,
up to 31, unique in the lab, and is never changed.

Nobody enrolls a Mac in another person's name. `lab enroll <name>` makes the
same kind of Join command for a Lab-owned Mac only.

An unused Join command stops with the access of whoever made it: "End all
access" and a Block cancel every one the person made, and the loss of the
Maintainer role those for Lab-owned Macs.

## Where the lab answers

Everything crosses one address, `https://lab.omarchy-mac.org`, the Lab
address: the public results, the site, `lab`, the Macs' agents, uploads and
downloads. It reaches the mini through one tunnel. No port is open at home,
there is no second way in, and nothing needs Tailscale.
[docs/https.md](docs/https.md) is the guide: what the path allows, how long
calls and big files cross it, and what a client checks.

- **Who you are** is your GitHub sign-in on the site and a Lab token in `lab`;
  a Mac has its device secret, the Builder its own token. Where a request
  comes from is never who it is from.
- **One name.** Through the tunnel labd answers only as the Lab address.
  `maclab.omarchy.org` is an Alias: a page opened there lands on the same page
  at the Lab address, anything under `/api/` is refused with a line naming the
  Lab address, and the lab never prints the Alias. Any other name gets 421.
- **Other sites' pages** cannot act for you: labd refuses (403) any request
  but GET, HEAD and OPTIONS whose `Origin` names another host than the one it
  was sent to, or that comes marked cross-site with no matching `Origin`, and
  every form carries a token of your session.
- **While Dark** (the tunnel, Cloudflare or the home link gone) the lab is out
  of reach for everyone, Maintainers included. `lab` and `omarchy-m-test` say
  "Omarchy Mac Lab cannot be reached right now." and never print an error
  page; a test still runs in full and its report waits (`omarchy-m-test
  --upload <path>` sends it later); Macs keep retrying. No reset is possible
  until the lab returns.
- **`/status`** says how the lab is, to anyone.

## License

MIT; see [LICENSE](LICENSE). The interface bundles third-party files under their own
licenses: JetBrains Mono (SIL OFL 1.1) and the Omarchy logo (MIT). See
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). Omarchy Mac Lab isn't an official
Asahi Linux project, and it isn't affiliated with Apple.

## Artifact package boot-test workflow

`lab boot-test` accepts an exact reviewed device profile/catalog and kernel, Mesa and boot-setup artifacts, compiles the finite workflow, then invokes the real local runner with plan-bound authorization. Recovery mode is explicit and is `network-only` (the lab refuses `serial-assisted`: it has no serial console); pending device adapters are refused. See [the command, source-review contract and limits](docs/artifact-boot-test.md). `make boot-test-dist BOOT_TEST_OUT=/owned/new-prefix` builds a relocatable local command/resource prefix without installing services or contacting a Mac. Device owner/compiler closures and per-run artifact/authorization data remain explicit reviewed inputs; this does not claim every historical prototype is packaged or portable.
