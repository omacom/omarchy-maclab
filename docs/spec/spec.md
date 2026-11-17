# Omarchy Mac Lab: locked architecture and migration spec

Locked 2026-11-14 (Brisbane). Written from the 32 resolved tickets of the map beside this file (`map.md`, `issues/`). Every statement is a ticket's Answer or a dated note on it, given in its final state, except the decisions S1 to S15 (end of section 19), which the owner made on 2026-11-14 to settle what the tickets left open. The ticket named in brackets after a decision holds the reasoning. Terms are those of `GLOSSARY.md` at the repository root; "M team member" means a member of the Omarchy M team.

Building the merged codebase and running the Cutover are a separate effort.

## Contents

1. Open conflicts
2. Purpose and scope
3. People and access
4. Macs: joining, Leases and Qualification
5. The server: processes, data, sign-in, screens
6. The Front door and the Alias
7. Out-of-band control
8. The Builder and builds
9. Publishing, Releases and verdicts
10. CLIs
11. Repository and release pipeline
12. Hardening
13. Operations
14. Terms and privacy
15. Fleet move
16. Cutover
17. Not verified, and tests owed
18. Out of scope
19. Where a later ticket changed an earlier one
20. Where each ticket landed

## 1. Open conflicts

None. Eight points were found while this spec was written and reviewed; the owner settled all eight on 2026-11-14 (Brisbane). Seven more came from drafting the build plan (`build-plan.md`) and were settled the same day. Together they are decisions S1 to S15: listed at the end of section 19, stated in place in the sections they belong to, and carried as a dated note on each ticket they touch.

## 2. Purpose and scope

- Omarchy Mac Lab tests Omarchy on real Apple Silicon Macs and publishes what it finds. It is one platform that replaces maclab and omarchy-m-testing: one repository, one server binary, one origin. (map, "Settled while charting")
- **Stack.** A Go monolith; SQLite in WAL mode; server-rendered templates with small vanilla JS and SSE; a native binary under launchd on macOS with `cloudflared` beside it. No Docker, Kubernetes or Node build chain. (map)
- **What is rewritten.** Only the site (Rails to Go), with unified data, sign-in and interface. maclab's agent, recovery ladder and ESP code carry over. (map)
- **Hard fork.** MIT, history kept, depending on nothing from Chris Kearney, who is unreachable. No remote, schedule or review follows his repository; a single commit from it, or from the forks of Naeem Malik, Scott Jones or Joshua Warren, can be copied in by hand. ("Fork mechanics and upstream policy")
- **Chris's deployment** at `maclab.chonkbase.net` keeps running and is outside this effort. Nothing is read from or written to it on anyone's behalf. The lab starts from the `omarchy-gpu` labd's data and the old site's reports. (map; "Moving the fleet")
- **Where it runs.** The Mac mini at home (`Mac17,16`, M5 Pro 18 cores, 64 GB, 1 TB, macOS 27.2.1): dedicated, headless, always on, on macOS. It takes over `oobd` and `lab-builder` from the M4 Pro, which keeps no role and is never a test target. (map; "Mac mini bring-up"; "Out-of-band wiring")
- **Where it answers.** `lab.omarchy-mac.org`, the Lab address, for public results and lab control. No inbound port at home; the home address is never published. (map; "The front door")
- **Names.** The product is Omarchy Mac Lab. `omarchy-maclab` is only the repository. "maclab" alone means Chris's project. Every message a command or a Notice shows says "Omarchy Mac Lab". ("Fork mechanics and upstream policy"; "Operations leftovers")
- **Reliability.** Best effort, no uptime promise: a status page served by the lab, the house battery in place of a UPS, backups in the house with the databases also off-site, auto-restart, and alerts to the owner's Signal group "Agents". ("Operations bar")
- **Automation.** Everything is code and scripts in the repository. The owner does every step that needs a password, an account, a purchase, a cable or a message; any purchase is confirmed with item and total first. (map; "Cutover and shutting the old site down")
- **Size of the port.** About 3,500 lines of Ruby and 1,063 of ERB over 3 tables: 19 public routes, 9 API and export endpoints, 11 admin routes. Its acceptance corpus, reused unchanged: the report schema, catalogue v18, the macOS reference, `candidate_sets.yml`, 5 golden, 1 benchmark and 2 signed reports, 3 signed sign-in documents, 11 production reports with expected builds, the installer tests and `release/smoke.sh`; 224 Rails test cases are translated. ("Site parity inventory")

## 3. People and access

### Kinds of people

A Person is one GitHub account, kept by its numeric account id; the handle is a label that follows renames. Maintainer is a role; a Volunteer is whoever lends their own Mac, so one person can be both. ("People and access"; "Data and identity")

| | Signed in, no role | External volunteer | M team member | Maintainer | Admin |
|---|---|---|---|---|---|
| Lends own Macs | sends a Request to lend | up to 5, the person approved once | up to 5 | no cap | no cap |
| Sees | public site | own Macs only | whole fleet | whole fleet | whole fleet |
| Leases a Mac and uses it | no | no | yes | yes | yes |
| Runs are Verified runs | no | no | yes | yes | yes |
| Runs Qualification and records its first verdict | no | no | yes | yes | yes |
| Baselines, Promotion, changing or revoking a verdict, enrolling and removing Macs, builders, Hide and Show, People | no | no | no | yes | yes |
| Adds and removes Maintainers | no | no | no | no | yes |

("People and access", as changed by "Leases for the M team"; "Volunteering a Mac")

- **Admin.** A Maintainer who may also add and remove Maintainers. Admins are named in the server's configuration by account id, on no screen. The owner is the only one at the start. ("People and access"; "Data and identity")
- **Maintainer.** Every Maintainer belongs to the Omarchy M team. A Maintainer's own Mac is a Volunteer's Mac like anyone's. The old site's admin powers (hide, show, delete reports; sign a Mac out; open-issue links) are Maintainer actions; there is no separate site admin. ("People and access")
- **M team member.** Rights attach to the mark, not to lending: a member with no Mac enrolled sees and leases the same. ("People and access")
- **External volunteer.** Sees only their own Macs, with their jobs, logs and screens. ("People and access")
- **The team record.** A Maintainer marks each person as M team on People. The mark is tied to the account id; marking a handle looks it up on GitHub at once and pins the id, so nothing waits for a first sign-in. The Signal group stays the definition; nothing syncs with it and the lab stores no phone number or Signal id. ("People and access"; "Data and identity")
- **Tester is retired** as a role. `runs=tester` stays as an alias. ("People and access")
- **Leaving the M team.** Earlier runs stay Verified runs, later ones are Community runs, Leases end at once. The person's Macs stay enrolled with no new approval: they become an External volunteer and keep leave to lend, under the cap of 5 (above it they keep what they have and add none). Lending no Mac, they return to "no role". ("People and access"; "The remaining screens")
- **What others see of a person.** The public sees nothing: pages and exports show Verified or Community only. Handles on reports and Mac sign-ins are for Maintainers. M team members see handles on the fleet (whose Mac, who holds which Lease), not who uploaded which report. ("Data and identity")

### Access to a Mac, in three rules

1. A Maintainer may do everything on any Mac.
2. An M team member may do the same on a Mac they lease, except what stays with Maintainers: baselines, Promotion, revoking a verdict, ending someone else's Lease, and managing Macs, people and builders.
3. On a Volunteer's Mac the Access level limits what runs, for everyone, and `lab-agent` enforces it.

One exception to rule 1: nobody, a Maintainer included, runs `lab exec` without their own Lease. ("Leases for the M team"; "Hardening inside the lab")

### Blocked, cuts and Forgotten

- **Blocked.** A Maintainer shuts an account out, with an optional reason; only an Admin blocks a Maintainer; an Admin cannot be Blocked, nobody blocks themselves, and only a person the lab knows can be Blocked. It ends sign-in, browser sessions, Lab tokens, Leases, Mac sign-ins, enrollments, the M team mark, the Maintainer role and leave to lend. Reports stay unless Hidden. The person meets one sentence, on the site, in `lab` and in `omarchy-m-test --sign-in`: "This GitHub account is blocked from Omarchy Mac Lab." Their Macs' reports still upload, as Community runs. ("Data and identity"; "The remaining screens")
- **Unblock.** One click by any Maintainer. It gives back sign-in and nothing else. While Blocked, the M team mark, Maintainer and Invite are refused for the person. ("The remaining screens")
- **Three cuts for a Lab token suspected stolen**, smallest first: revoke one token (its person or any Maintainer; it stops on the next request and its open streams end); "End all access" (every browser session, Lab token and Lease of the person ends, the Leases as when a Maintainer ends them; the person stays and signs in again; Mac sign-ins untouched; used by the person, or by a Maintainer for them, an Admin for a Maintainer); Blocked. "Sign out everywhere" is browsers only. For the whole lab the owner stops the tunnel. Afterwards every Mac the Exec record shows the thief touched is treated as read: a Lab-owned Mac is reinstalled and Joins again; a Volunteer is told. ("Hardening inside the lab")
- **Forgotten.** A person asks on their own page, or by mail to `privacy@omarchy-mac.org`; a Maintainer (an Admin for a Maintainer) carries it out with one button within 30 days; it cannot be undone. What happens to each thing is in section 14. ("Terms and privacy")

### What the owner is told

"Agents" gets one line for each change of who someone is (M team mark, Maintainer, Blocked or unblocked, a Request to lend answered, an Invite), each new Lab token (who, its name, its expiry) and each cut a Maintainer makes (another person's token revoked, "End all access"). These lines carry the handle; the line for a forgetting carries none, only who carried it out and when. ("Hardening inside the lab"; "Terms and privacy")

## 4. Macs: joining, Leases and Qualification

### Kinds of Mac

- A **Lab-owned Mac** has no Volunteer; Maintainers keep it. At the start: the M1 Pro (`m1-pro`, unencrypted) and the M2 Max (`m2-max`, LUKS with `lab-only-autounlock`). ("Moving the fleet"; "Out-of-band wiring")
- A **Volunteer's Mac** belongs to exactly one person, by account id, and is a Test machine: a Mac with nothing on it, in Omarchy or in macOS, that its Volunteer cannot afford to lose or to have read. Software cannot check that; the typed `yes` covers it. A Mac its owner uses daily is not welcome. ("Data and identity"; "Terms and privacy")
- A Mac is in one lab at a time. ("Moving the fleet")

### Volunteering: the flow

It starts on the site. `./volunteer` is retired; nothing looks for the lab, and every line a person is told to run names the Lab address and nothing else. ("Volunteering a Mac"; "Serving maclab.omarchy.org")

1. **Sign in** with GitHub. "Lend a Mac" opens with the Test machine rule, then a table of what the lab may and may not do, and the terms text.
2. **Leave to lend**, needed once per person: an M team member or a Maintainer has it; a person with an Invite has it; anyone else sends a Request to lend and waits for a Maintainer.
3. **Join command.** The person names the Mac on the page and gets one line to copy.
4. **On the Mac.** Pre-checks, then the terms and a typed `yes`, then leaving any other lab, then enrolling and setup. Join asks nothing about the Availability window: the Mac arrives closed, and setup's last line points to My Macs to set it.
5. **Arrival.** The Mac is at Qualification with publishing off. The lab queues its first baseline itself; it runs in the first open window.
6. **Afterwards.** My Macs shows the Mac, its records and its jobs. Leaving is one click or one command.

**Request to lend.** The form takes the Mac's model from the site's list, an optional note and an optional contact (free text, Maintainers only). One per person; it waits until answered or withdrawn. The lab sends the person nothing: "Lend a Mac" shows waiting (date, Withdraw), approved (the page becomes the join form) or declined (date, the Maintainer's optional one-line reason, "Ask again", which works at once). Approval is of the person: it stands when their last Mac leaves; Blocked takes it back. ("Volunteering a Mac")

**Invite.** A Maintainer names a GitHub handle on People; the lab pins the account id at once; that person has leave to lend from their first sign-in. Nobody enrolls a Mac in another person's name. Lab-owned Macs keep the Maintainer's own enroll path (`lab enroll`), with the same command shape and pre-checks. ("Volunteering a Mac"; "CLI changes")

**The Join command and its token.**

- One short line, `curl -fsSL https://lab.omarchy-mac.org/join | LAB_JOIN=enr_… bash`: the enrollment token travels in the environment of the person's own shell, readable only by them and root, never in an argument or a URL; then only in a header and on stdin; the agent downloaded with the token; then `lab-agent setup`, which asks its questions on the terminal. The agent the Mac installs carries its own signature by the release key, checked against the key pinned in the script labd serves at `/join` for the current release; that script holds no secret, is served to anyone and is never cached, and a download of it cut short runs nothing. ("Volunteering a Mac"; "Hardening inside the lab")
- The token is made by the signed-in person on the page; no Lab token and no device flow in the terminal. It lasts 1 hour; one unused per person; a new one cancels the old.
- One Mac, once: the first Mac to use it binds it and may run the command again if setup was interrupted; any other Mac is refused.
- The command tests the token before anything changes, fails once without retrying and names the case: expired or used; too many refused attempts from this network (wait 15 minutes); challenged at the edge. Blocked, cap reached and name taken are refused at enrollment with the reason.

("Volunteering a Mac")

**The cap.** Every enrolled Mac a person is Volunteer of counts toward 5; an unused Join command counts as one; Macs that left do not. A Maintainer can raise one person's cap on People. Maintainers have none. ("Volunteering a Mac")

**Naming.** Typed on the page before the command is made. The suggestion is the Mac's model alone, numbered on a clash. Lowercase letters, digits and dashes, up to 31, unique in the lab. The page says who sees it: Maintainers, the M team, the Lab group and the notice service; never the public site. No rename; a Mac that leaves frees its name. ("Volunteering a Mac", as changed by "Terms and privacy")

**Pre-checks**, all before the terms, before the token is used and before anything changes:

| Check | On failure |
|---|---|
| An Apple Silicon Mac on Linux, and the system is Omarchy | Stop |
| Run as yourself, with sudo | Stop |
| Already in this lab | Stop: names the Mac, points to My Macs |
| In another lab | Names it and asks once; on yes the Mac leaves it properly after the terms are accepted, before enrolling. Lab-owned there: stop, ask that lab's Maintainer |
| Running a lab kernel | Stop |
| The disk waits for a passphrase | Stop, naming two ways out: reinstall without encryption, or `lab-only-autounlock` for a lab-only install (whoever holds the Mac can then read that disk) |
| The bootloader cannot restart unattended, tools missing, under 300 MB | Stop, naming each |
| The system cannot be snapshotted | Warn: no Installed runs on this Mac |
| A laptop on battery | Warn: jobs start only on AC power |

A failed pre-check or a declined terms prompt is never reported to the lab. ("Volunteering a Mac"; "Moving the fleet")

**What the person is told**, on "Lend a Mac" and as the terms in the terminal:

| | The lab may |
|---|---|
| **Qualification** (every Mac starts here) | Restart the Mac into test kernels, the M team's own included, and back; run its own tests; collect hardware facts, logs and test output; see the screen and the live journal while a job or a Lease holds the Mac. Who: Maintainers, and M team members holding a Lease |
| **Full** (the Volunteer chooses it, on the Mac) | Also a root shell, their own scripts, staged Mesa |
| **Never** | A Hard reset. Anything outside the Availability window or on battery. Publishing its reports unless the Volunteer turns that on. Showing the Volunteer's handle to the public |

Installing a Release and rolling back is not offered on a Volunteer's Mac until Rollback exists. ("Volunteering a Mac", as changed by "Publishing, Releases and verdicts" and "Terms and privacy")

**Arriving.** A Mac arrives closed: with no Availability window it takes nothing. Joining sets Qualification and publishing off, whatever the Mac had in another lab; a window it had is kept. The first baseline is queued by the lab itself, no Maintainer step; a failed one is shown and posted, and a Maintainer looks. My Macs shows "no window yet", "first check pending (restarts twice)", "ready". ("Volunteering a Mac")

**Controls: the Mac sets the ceiling, save the weekly schedule.** Only the Mac gives Open now, raising to Full and turning publishing on: `sudo lab-agent …` at the Mac; My Macs shows each command. The weekly schedule is set either at the Mac (`sudo lab-agent window …`) or on My Macs by that Mac's own Volunteer, signed in or with their Lab token (add a window, always included, or remove one); nobody else sets it, Maintainers and the M team included. labd keeps a change made on the site and hands it to the Mac at its next check-in; the Mac enforces its window itself, labd down or not, and My Macs shows the change pending until the Mac holds it. The last change wins, and each is in the Mac's record and posted to the Lab group. The trade-off: a stolen session or Lab token of that Volunteer can widen their own Mac's window, which before took the Mac itself. Otherwise the site only takes away, and undoes only what it took: Pause (until a time or Resume), Limit to Qualification (until "Remove limit"), Leave. Pause and Limit stop a running job as a closing window does and end a Lease. Two controls on the site give: the weekly schedule and accepting changed terms. Lab-owned Macs have no window. ("Volunteering a Mac"; "Terms and privacy")

**The weekly schedule** is days and hours in the Mac's local time, `mon-fri 19:00-07:00`; `00:00-24:00` is a whole day, and `always` (every day, every hour) leaves no gap at midnight. ("Volunteering a Mac")

**Leaving.** By the Volunteer from My Macs (asks twice) or `sudo lab-agent leave`; by a Maintainer; by the Volunteer being Blocked or Forgotten. On the Mac: any job stops, the Mac returns to its own system, and the lab's boot entries, staged kernels, units, settings files and device secret are removed; the `lab-agent` program, the Audit log and the Volunteer's own window and Access level stay, with the one command that removes them named. In the lab: the record is Retired at once, its name is free and it stops counting toward the cap; screenshots, serial log and baselines are deleted; jobs, results, the Audit log copy and the Mac's record stay for Maintainers. A Mac not reaching the lab when ended from the site finishes leaving when it next does. ("Volunteering a Mac")

**Records and the Lab group.** Every event of this flow is recorded and posted to the Lab group, one line each, as it happens; a failed send never blocks the event. Requests to lend, Invites, a raised cap and Join commands go in the People record; a Mac's Join, first baseline, window changes, Pause, Open now, Resume, Access level and publishing changes go in the Mac's record; a Mac leaving goes in both. A line about a Mac names the Mac only. A line about a request, an Invite or a Join command names the Maintainer who acted, never the person asking. Refused enrollment tokens are logged, not posted. A window opening or closing on schedule is not an event. No limit on lines: a person who floods the group is Blocked. The Mac's record is read by its Volunteer on My Macs and by Maintainers and the M team on the Mac's page; the People record is for Maintainers. ("Volunteering a Mac")

### A Volunteer's Mac and the lab's own software

- `lab-agent` always comes from the lab server and is never pushed to a Volunteer's Mac: the Volunteer runs `sudo lab-agent update` at the Mac, shown on My Macs only. Until then labd's "needs agent X or later" refusals apply. No fleet-wide minimum agent. ("Repository layout and release pipeline"; "CLI changes"; "Terms and privacy")
- A Volunteer's Mac never gets a Hard reset: it recovers by network means and its own watchdog, then its Volunteer is asked. ("Out-of-band wiring")
- Before each job the lab installs the latest `omarchy-m-test` on the Mac from the lab's install line, checking its signature (in Preview that line serves the Release candidate: decision S4). ("Cutover and shutting the old site down"; "Terms and privacy")

### The Lease

`lab-agent` (the running agent) does not change for Leases; everything is labd, `lab` and the interface. ("Leases for the M team"; "CLI changes")

- **Whose.** A person's: the account id on the server, the handle in the field the agent already has, the free-text holder label kept inside it.
- **Which Macs.** Any Lab-owned Mac and any Volunteer's Mac inside its Availability window, at either Access level; only a Mac that has passed its baseline and has not left.
- **How long.** 4 hours by default for everyone. A Maintainer may ask for up to 24 hours; the M team may not. On a Volunteer's Mac it is cut to the window's end.
- **Renewal.** At any time; the end becomes 4 hours from now. No count limit and no idle rule.
- **One per Configuration** for the M team, counted per person; a second is refused, naming the one held. Maintainers are warned and may go on.
- **The Configuration** is worked out by labd: the board from check-in; for a board with one Configuration, from a table in the repository; otherwise both core counts from the kernel log of the Mac's latest job on its Known-good kernel. A Maintainer can set or correct it. Macs of one board whose counts are unknown count as the same Configuration until a Maintainer sets it.
- **What it fences.** Jobs, `lab exec`, screenshots, the live journal and reset. An M team member needs their own Lease for each and cancels only their own jobs. A Maintainer acts without a Lease on a free Mac, except `lab exec`; under another person's Lease a Maintainer is refused jobs and exec, and may still cancel a job or end the Lease.
- **Maintainers wait.** A leased Mac refuses a Maintainer's jobs, naming the holder and the end time, and labd's scheduled refresh skips it. A Maintainer who needs the Mac ends the Lease, which is recorded.
- **Hard reset.** A Lease holder may Hard reset the Lab-owned Mac they hold. ("Out-of-band wiring")

("Leases for the M team", as changed by "Hardening inside the lab" and "CLI changes")

| How a Lease ends | Running job | Holder's queued jobs and exec |
|---|---|---|
| Released by the holder | finishes | none left to run; sessions cut |
| Runs out with a job running | the Lease is extended until that job ends, no further; the record says "extended" | canceled and cut at the original end |
| Runs out, nothing running | | canceled and cut |
| Ended by a Maintainer | canceled as `lab cancel` does; the Mac returns to its Known-good kernel | canceled and cut |
| Holder leaves the M team, is Blocked, is Forgotten, or "End all access" is used | same as ended by a Maintainer | canceled and cut |
| Window ends, Pause, Limit, or the Volunteer lowers the Access level | stopped by `lab-agent` as today | as today |

A job that ran under a Lease is never retried by labd itself. Jobs others queued before the Lease run again when it ends. ("Leases for the M team"; "Hardening inside the lab"; "Terms and privacy")

**Reading.** The M team reads every job, result and collected file on the fleet. Live views (screen, journal) only under their own Lease. The Audit log is for Maintainers and the Mac's Volunteer. ("Leases for the M team")

**Records.** labd keeps, for every Mac, each Lease taken, renewed, extended, released, expired and ended, with who and why, shown beside the Audit log and on the person's record. The Volunteer sees on My Macs who holds the Lease (handle, kind) and until when. ("Leases for the M team")

**The Lab group** (Signal group "Omarchy Mac Lab"): one line for each Lease taken (handle, Mac, until when), released or expired, or ended by a Maintainer or by the holder leaving the team or being Blocked (who and why). Renewals and extensions are not posted. A Volunteer's Mac is named by its Mac name only. Once a day at 08:00 Brisbane: one summary of each Mac with its Configuration and state (free, leased by whom until when, running a job, outside its window, Paused, not seen), the count of Requests to lend waiting, and a link to Macs. Members: M team members who want it, added by the owner by hand, plus the lab's sender (the owner's own number when the sender is linked to it); no External volunteers. The lab only sends, reads nothing from the group and stores no phone number. A failed send never blocks a Lease. ("Leases for the M team"; "Volunteering a Mac")

### Qualification of a Release

- An M team member may freeze a Release's manifest and a Mac's entry and run Installed runs, each under their own Lease on that Mac, one per Configuration. ("Leases for the M team")
- Until Rollback exists on real Macs the lab refuses an Installed run on a Volunteer's Mac, through `lab`, the site and a direct request. Qualification runs on Lab-owned Macs only, and no Install operation runs anywhere. ("Publishing, Releases and verdicts"; "Hardening inside the lab")
- Baselines and Promotion stay with Maintainers. ("Leases for the M team")
- The verdict and the Releases page are in section 9.

## 5. The server: processes, data, sign-in, screens

### The mini

- Reached as your account over SSH on the LAN (Ethernet, its address reserved on the router) and on the tailnet. `sudo` asks for a password. Wi-Fi is off. ("Mac mini bring-up"; "Operations bar")
- **FileVault stays on.** After an unplanned restart the lab is Dark until the owner Unlocks it. Planned restarts use `fdesetup authrestart` and need no Unlock. ("Operations bar")
- **No login session; everything is a LaunchDaemon.** ("Operations bar")

| Runs as | Processes |
|---|---|
| the unprivileged `lab` account | labd, Litestream, the backup job, the Dead-man switch's ping, `cloudflared` |
| root | the Out-of-band controller (`oobd`), the Builder (`lab-builder`), `tailscaled`, and one LaunchDaemon that starts `container` before the Builder polls |

("Operations bar"; "`container` without a login session")

- The lab's Signal sender is `signal-cli` on the mini; its secrets, like every secret the port brings, are files of the `lab` account. It is either a linked device of the owner's own number (`scripts/mini/signal link`, the owner's choice) or a number of its own (`scripts/mini/signal register`). Linked, the mini holds keys that read the owner's Signal, and every line comes from the owner's own number. `link` shows the `sgnl://` link only as a QR code, drawn by the lab's own binary (`labd qr`, from the Deployed build, the link on its input); the link is never on the terminal as text, an argument, a file or a log. ("Operations bar"; "Hardening inside the lab")
- **`container`.** A root LaunchDaemon runs `launchctl asuser <uid> sudo -n -u <user> container system start --enable-kernel-install` at boot, which puts the API server in `user/<uid>` with nobody logged in. `lab-builder` never starts the service. Health checks and restarts go through the same wrapper or over SSH. The same LaunchDaemon checks the server every minute and starts it again through that wrapper; "Agents" gets a line on each change (decision S11). Proved with your account; another account needs the same check once. ("`container` without a login session")
- **Tailscale.** The open-source `tailscaled` system daemon replaces the Standalone app; the mini rejoins as a new node tagged `tag:lab`. SSH stays OpenSSH with keys after Unlock; Screen Sharing stays on, from the LAN and the tailnet. ("Operations bar")
- **Tools on the mini.** Any tool with a Homebrew formula comes from Homebrew (7.0.13 at `/opt/homebrew`): `cloudflared`, `tailscaled`, Litestream, restic, and `signal-cli` with its Java. Their versions follow Homebrew and are checked in the Maintenance run; this replaces "No Homebrew" and the pin by version and checksum for those tools (decision S10). No daemon runs from `/opt/homebrew`, which the admin account can write: set-up and each Maintenance run copy the Homebrew tools the lab uses into a root-owned folder and every daemon runs from there; the previous copy is kept, so a bad upgrade has a way back (decision S14). A tool Homebrew ships as a wrapper script on a Java tree, as `signal-cli` once was, is copied with its whole Java beside it and the wrapper's paths pointed at the two copies; anything else in a copy that names `/opt/homebrew` is refused. `container` (Apple's package) stays pinned by version and checksum in the repository and installed by script; the lab's own reset tool comes in the server bundle. The owner can be at the mini's own screen for the first set-up if required, so the Command Line Tools prompt, the reason "Operations bar" gave for the ban, is no obstacle then. In normal running no script of the lab's on the mini may need `python3`, `git` or `cc`. The mini compiles none of the lab's own software. ("Operations bar"; "Repository layout and release pipeline")
- **Kept names.** Everything an enrolled Mac or a stored record carries keeps its `maclab` name: the `MACLAB_*` variables (38 at the fork; `MACLAB_BUILDER_TOKEN` goes, section 12), `/var/lib/maclab`, `/etc/maclab`, `~/.config/maclab`, `~/.local/share/maclab`, the systemd units, `com.maclab.oobd`, the kernel command-line keys, the 17 schema ids, the ESP names. No binary is renamed. ("Fork mechanics and upstream policy")

### labd's listeners

- A Unix socket only the `lab` account can open, used by `cloudflared`: the only place forwarded headers and `CF-Connecting-IP` are believed.
- A loopback port for callers on the mini (the Builder, the Out-of-band controller's client side, break-glass) that believes none.
- labd and `oobd` listen on the mini only, never the LAN or the tailnet.

("The front door"; "Out-of-band wiring")

### Stores

- Two SQLite files in one process, WAL, `fullfsync`, a read pool and numbered migrations; no atomic commit across the two. In the port: the old site's row lock becomes a write transaction or upsert, timestamps are integer microseconds, ids are `AUTOINCREMENT`; its four JSON queries run unchanged. ("SQLite consolidation and replication")
- `labd.db`: people, roles and marks, Blocked, leave to lend, browser sessions, Lab tokens, and everything maclab keeps there.
- `reports.db`: reports and Mac sign-ins, each with account id and handle. It renders the public site without `labd.db` and holds no credential.

("Data and identity")

### Credentials

None can be turned into another. ("Data and identity")

| Credential | Held by | Made by | What it does | Ends |
|---|---|---|---|---|
| Browser session | a person's browser | GitHub web sign-in | acts as the person on the site | 30 days unused, sign-out, "Sign out everywhere", "End all access", Blocked, Forgotten |
| Lab token | `lab`, `lab mcp`, a person's agents | the person, on their own page | acts as the person, with their roles at that moment | left unused past its period, revoked by the person or a Maintainer, "End all access", Blocked, Forgotten |
| Mac sign-in | a Mac's Machine key | `omarchy-m-test --sign-in` (GitHub device flow) | uploads that Mac's reports as the person; decides Verified | sign-out on the Mac, the person on their own page, a Maintainer, Blocked, Forgotten |
| Enrollment | a Mac's `lab-agent` (device secret) | a one-time enrollment token | the Mac's own routes | Leaving |
| A named builder's token | the mini's Builder, in a file only root reads | a Maintainer | the Builder's routes, for its running build only | removal of the builder |
| The Out-of-band controller's token | the controller, in a root-only file; labd, in a file only the `lab` account reads (decision S1) | a Maintainer | Hard resets | as in maclab |

("Data and identity", as changed by "The remaining screens", "Hardening inside the lab" and "Terms and privacy")

- **Browser session.** A random id in an HttpOnly, Secure, SameSite=Lax cookie; only its hash is stored. Roles are read on every request, so a change takes effect at once and cuts open streams. The lab keeps no address and no browser name for a session.
- **Lab token.** Replaces maclab's Maintainer tokens; any signed-in person may make one; it does only what the person may do. Up to 10 per person, each with a name, made, expires and last used (time and address). Shown once, stored as a hash. Its period is chosen at creation: 30 days, 90 days (default) or 1 year unused, or never; every use pushes the date out. An expired token stays listed and its person can Renew it: the same secret works again. No device flow for `lab`.
- **Break-glass.** A command run on the server itself mints a Lab token for a named person. No standing admin token on disk; `admin.token` is retired. The owner alone reaches it: SSH to the mini over the tailnet, then `lab` against the loopback listener.
- **Mac sign-in and enrollment stay separate.** The person signed in on a Mac need not be its Volunteer. When the lab publishes a job's report it records that Mac's machine id on its lab record.
- **Names in records.** Jobs, Leases and each Mac's Audit log record the handle at the time, with the account id on the server. A rename does not rewrite the past.

("Data and identity"; "The remaining screens"; "The front door"; "Hardening inside the lab")

### GitHub

- One OAuth app, the existing one (client id unchanged), renamed, its callback at the Lab address. Web flow for browsers; device flow only for Mac sign-in. Scope empty: identity only, no email.
- A second app for development, the import rehearsal and Preview.

("Data and identity"; "Cutover and shutting the old site down")

### Verified runs

- One rule: a run is a Verified run when its Mac's Machine key is signed in as an M team member at upload, whoever uploads it. The verdict, account id and handle are stamped on the report then and never recomputed.
- Lab-owned Macs are signed in once by a Maintainer; if that Maintainer leaves the team, another signs the Mac in again.
- A Mac signed in by someone outside the M team stays signed in; its runs are Community runs.
- Wire names stay (`/api/v1/tester_bindings`, `/api/v1/tester_requests`, the `tester` key, `tester.json`, `runs=tester`).

("Data and identity"; "CLI changes")

### What is carried from the old world

- **From Railway, all three tables whole.** `reports` (hidden ones included) with numeric `id`, `public_id`, `deletion_token_digest`, `schema_version`, `body` as text, timestamps, `hidden_at`, `machine_id`, `tester_login`. `tester_bindings` become Mac sign-ins with timestamps to the microsecond. `testers` (32 handles) become people with no mark; handles that never signed in are looked up on GitHub at import.
- **Imported reports keep the verification the old site shows on the day**, stamped once.
- **The machine-id key is reproduced**: PBKDF2-HMAC-SHA256 over the old site's `SECRET_KEY_BASE`, salt `report machine id from key`, 1,000 iterations, 64 bytes. The server keeps those 64 bytes as a permanent secret and never sees `SECRET_KEY_BASE`.
- **From `omarchy-gpu`, the whole data directory** with ids unchanged (2 Lab-owned records, 253 jobs, 90 builds, 3.9 GB), upgraded in place; everything done under its admin token is attributed to the owner. Its two Mac records are Retired at import.
- **Old links.** `/reports/<public_id>` and `/reports/<public_id>/deletion?token=<token>` work with only the host changed.
- **The import** is a re-runnable script that fills an empty `reports.db`. It is accepted only when: one known machine id is recomputed from one of the owner's Macs' public key; row counts of the three tables match the dump, hidden reports included; a deletion link issued by the old site deletes its report on the lab.
- **The dump** is taken by the import command itself, which ships in the server bundle: it reads Railway's Postgres, its address typed by the owner at a prompt, and writes the three tables to a file on the mini; the import then reads that file. No `psql`, `pg_dump` or `python3` on the mini (decision S9). Railway's certificate is checked (`verify-ca`) against a CA file the owner gives at a second prompt; beyond loopback nothing is read without it. The rehearsal import first shows the certificate chain Railway's Postgres presents, with each SHA-256 fingerprint, and keeps its last certificate as that file, so the owner looks at it before the day.

("Data and identity"; "Moving the fleet")

### The interface

**One header, today's site look.** Tokyo Night, JetBrains Mono, green brand, terminal cards, outline badges for kind and state. Dark only; no theme picker; `/api/themes*` is gone. Lab pages are ordinary scrolling pages. ("Interface shell and People screen"; "The remaining screens")

- **Row 1, the same for everyone:** brand; Matrix, Models, Features, Kernel gaps, Aurora, Benchmarks, Candidates, Reports, Data; Install. Signed out, a Sign in link at its end.
- **Row 2, only when signed in:** a "lab" tag, the lab pages the role allows, then the fleet status (M team and up) and the person's handle (the link to their own page), kind and Sign out.
- A page a role may not open answers as not found; signed out, it leads to sign-in.
- People carries a count of pending requests in the navigation.
- Phone width: each row scrolls sideways; tables turn into stacked label-and-value rows.
- The footer of every page links `/terms` and `/privacy`. No cookie or consent notice.

| Viewing as | Row 2 |
|---|---|
| Signed out | none |
| Signed in, no role | Lend a Mac |
| External volunteer | My Macs |
| M team member | Macs, Jobs, Releases |
| Maintainer | Macs, Jobs, Builds, Releases, People |
| Admin | as Maintainer; People adds Make Maintainer and Remove Maintainer |

("Interface shell and People screen"; "Publishing, Releases and verdicts"; "The remaining screens"; "Terms and privacy")

**Actions that take something away** ask twice in place: the first click turns the button into a sentence stating the effect, the second does it. Every action writes a record line with the actor and the time. ("Interface shell and People screen")

**People** (Maintainers). Top to bottom: requests (Requests to lend, and requests to be Forgotten), each with its actions; the table of people with filter chips by kind (Blocked among them), one row per person: handle, kind, their Macs, Leases held, Macs signed in for Verified runs, last seen; a sign-in by someone not on the M team shows as "not counted", a handle marked before its first sign-in as "not signed in yet"; a field to mark a handle as M team or to Invite it before it signs in; the record of who did what. A row opens in place: Role; Their Macs (End enrollments), the cap control; Leases held (End Lease); Signed in for Verified runs (Sign out); Lab tokens (name, last used, Revoke); "End all access"; Block or Unblock; Forget; that person's record lines. ("Interface shell and People screen"; "Volunteering a Mac"; "The remaining screens"; "Hardening inside the lab"; "Terms and privacy")

**A person's own page.** You (handle, kind, what it allows); Lab tokens ("n of 10", Make, Revoke, Renew); Browsers (a count, "Sign out everywhere", "End all access"); Macs signed in as you (Sign out on each); "Ask to be forgotten". ("The remaining screens"; "Hardening inside the lab"; "Terms and privacy")

**Macs.** The fleet with who holds each Lease; Lease, Renew, Release on Macs at either Access level; filters All, Mine, Leased by me; each Mac's Configuration with a Maintainer's control to set it; each Lab-owned Mac as sighted, not sighted or no cable; an agent older than the lab's, with its version. A row that takes no Lease says why. Enroll a Mac (Maintainers, Lab-owned Macs) is reached from here. ("Interface shell and People screen"; "Leases for the M team"; "Out-of-band wiring"; "CLI changes")

**A Mac's page**: Overview (facts, its Lease and Lease buttons, its jobs, "Boot a kernel"); Live (Screen, Logs, and for a Lab-owned Mac Hard reset, asking twice); Records (the Lease record, the Mac's record, the Audit log for Maintainers and the Mac's Volunteer, and the Exec record for Maintainers, the person who ran the command and the Mac's Volunteer). An M team member sees Live only under their own Lease. A Volunteer on their own Mac's page sees Overview, Live (view only) and Records. There is no serial console panel and no console record. ("The remaining screens", as changed by "Hardening inside the lab"; "Out-of-band wiring")

**My Macs.** Above the cards: an unused Join command with its expiry and Cancel, "Lend another Mac, n of 5", and a banner with Accept when the terms have changed. One card per Mac: name, Configuration and state in plain words (for a Mac that Needs hands: the steps, the reason and the last log line; who ended an enrollment; who asked for a job and how it ended); Availability window, Access level, publishing, Eligible or not; its ntfy topic, whether job notices are on, and a refused topic; its jobs, the Audit log, the Mac's record and the Exec record; its lab-published reports, each with Delete; who holds a Lease and until when; Pause, Limit and Leave as buttons; the commands for what only the Mac can give, `sudo lab-agent update` among them. The link in a Notice opens that Mac here, and the page shows the Mac's state without one. An M team member or a Maintainer sees the same cards under "Mine" on Macs. ("Volunteering a Mac"; "Publishing, Releases and verdicts"; "Hardening inside the lab"; "Terms and privacy")

**Other pages.** "Boot a kernel" is a page reached from Jobs and from a Mac's page. A job shows its report's state (not published, published, hidden, deleted) and, for an Installed run, its Release. Builds shows who asked. Reports carries Hide, Show and Delete for Maintainers, hidden reports, who uploaded each and how each arrived. `/status` shows the server, the tunnel, the Builder, the Out-of-band controller, backup age, the last Restore drill and the last restart. ("The remaining screens"; "Publishing, Releases and verdicts"; "Interface shell and People screen"; "Operations bar")

**Signed-out pages** carry nothing per visitor: no cookie, no token, no flash line. Sign in is a plain link to a `no-store` address that sets a short-lived state cookie; the single-use OAuth state is its protection; afterwards the person returns only to a path on the Lab address. Who is signed in is rendered by the server, with no script: a browser that sends the session cookie never gets the cached copy. There is no browser upload form; reports come only from `omarchy-m-test`. The old site's browser gate (406 to older browsers) is not carried over. ("The remaining screens"; "Hardening inside the lab")

**The port's fixed points.** Upload is an 11-step pipeline with fixed order and status codes; signatures are SSHSIG ed25519 over canonical JSON; the client address is `CF-Connecting-IP`; the content security policy is host- or hash-based. ("Site parity inventory"; "The front door")

## 6. The Front door and the Alias

### One path

- The public site, browsers, `lab`, agents, uploads and downloads all cross `lab.omarchy-mac.org` through one Cloudflare tunnel on the Free plan. No second path for Macs or large files, no Pro, no Argo, no Cloudflare Access. The lab's own sign-in is the only gate. ("The front door"; "Tunnel spike"; "Paths around the proxy")
- The limits it works inside: the edge cuts a response after 125 s without a body byte (headers do not count) and refuses a request body over 100 MB. ("Cloudflare Tunnel limits"; "Tunnel spike")
- Direction: agents, `lab`, browsers and the Builder dial labd; labd dials the Out-of-band controller, which never crosses the tunnel. ("Lab traffic inventory")

### Held responses

- No silent response through the tunnel. A held response sends a Keepalive at once and then every 30 s; the event stream keeps its 15 s.
- Through the tunnel `lab exec` and `freeze-entry` always answer as a stream (`application/x-ndjson`, the result as the last line, errors in the body). The silent form is kept only for callers on the mini.
- Job waits are capped by the server at 50 s a call. A followed build log is polled from an offset.
- A cut `lab exec` is not repeated: `lab` never re-sends and says "connection lost; the command may still be running". Long work goes in a job.

("The front door")

### Uploads and downloads

- Uploads go in numbered parts of 32 MiB; anything smaller is one request. Resume asks the server which parts it holds; each part is retried up to 8 times; a SHA-256 closes the upload; unfinished uploads are deleted after 24 h. How many parts go at once (1 or 4) is set by the measurement from outside the house: 4 only if at least 20% faster.
- labd refuses any single body over 90 MB, and a client never sends a body without `Content-Length`. An agent's job file over 90 MB is refused.
- Downloads come from the mini, through the tunnel, resumable, never cached at the edge. No object storage. They move to R2 when the outside-the-house test measures under 50 Mbit/s, downloads fill the home uplink, or Cloudflare sends a large-file notice.

("The front door")

### What labd does itself

- The caller's address is `CF-Connecting-IP`, believed only on the tunnel's socket.
- Every request is logged with address, person or Mac, and ray id. The caller log is kept 30 days, then deleted. The secret in a report's deletion link never appears in it.
- Refused credentials are logged and counted: past 10 in 10 minutes from one address, further bad ones get 429 for 15 minutes and one alert goes to "Agents". A valid credential always passes. Refused enrollment tokens count with the rest.
- The old site's limits carry over on the real address: uploads 30 an hour per address and 10 per Machine key, Mac sign-in 10, status 60.
- labd answers 425 to any early-data request under `/api/*`.

("The front door"; "Terms and privacy"; "Hardening inside the lab"; "Volunteering a Mac")

### Edge settings

All set, and checked on every run, by the zone script, which fails if one drifts. ("The front door")

- **Off**: Bot Fight Mode, AI-bot blocking, AI Labyrinth, Rocket Loader, email obfuscation, Always Online, automatic HTTPS rewrites, Cloudflare Fonts, the analytics script injection.
- **On**: Always Use HTTPS, HTTP/3, TLS 1.3, 0-RTT, Early Hints, Smart Tiered Cache, Speed Brain.
- **Browser Cache TTL**: Respect Existing Headers, so a browser keeps to the server's `max-age`; the default four hours would override it.
- **One skip rule on `/api/*` and `/join`**: no Browser Integrity Check, no managed rules.
- **One rate rule**, a flood cap: 300 requests in 10 s per address, whole site.
- Clients name themselves in `User-Agent` and report an edge challenge as a plain error.
- HTTP/3 is switched off for the hostname if Chrome is seen to stall large downloads over it.

### Cache

- The server states a policy on every response; the default is `no-store`.
- Fingerprinted assets: public, one year, shipped inside the binary already compressed at the highest Brotli level.
- `/install`: public, 5 minutes.
- Public pages and exports, `/terms` and `/privacy` among them, for visitors with no session: cached at the edge, fresh for 60 s, then served stale while the edge refreshes, for up to a day, and stale on error for a day (`stale-if-error`). Browsers always revalidate. Nothing is purged in normal running; the one purge is at the lab's opening (section 16, step 25).
- The guard: the cache rule matches only public paths on requests without the session cookie or a Lab token; anything rendered for a session says `private, no-store`.
- `/api/*`: one rule bypasses cache. Artifacts send `Cache-Control: private` themselves.
- A request with a Lab token (`Authorization`) or the session cookie: one rule, the last, bypasses cache on every path.
- Never a "cache everything" rule or a forced edge lifetime.
- Left to the build: the exact header pair; the edge lifetime goes in the CDN-only header.

("The front door"; "Operations leftovers"; "Terms and privacy"; "Tunnel spike")

### The tunnel

- One tunnel, used for nothing else. Its routes are a config file in the repository listing two names, the Lab address and the Alias; everything else is 404. The credential is on the mini, readable by `lab` only.
- `cloudflared` speaks HTTP/1.1 to the Unix socket. labd's idle timeout (2 min) stays above `cloudflared`'s pool timeout (90 s).
- The tunnel's transport is chosen by measurement: 1 GB up and down under QUIC and under HTTP/2, with `cloudflared`'s CPU; the faster is set, and within 10% QUIC stays. If QUIC wins, the mini's UDP buffer limit is raised in its set-up script.

("The front door"; "Serving maclab.omarchy.org")

### Performance targets

Recorded in the acceptance run, measured without a script in the page. ("The front door")

1. Signed-out public page: edge HIT, first byte under 100 ms at the visitor's Cloudflare site.
2. Any page rendered by the server: under 100 ms at the 95th percentile.
3. Transfers through the tunnel: at least 80% of the slower end's link speed.
4. `cloudflared` under one core at full rate.

### The accounts

- Whoever holds the Cloudflare account can point the hostname elsewhere. The Cloudflare and Porkbun accounts use a security key or an authenticator; DNSSEC is on; a "tunnel created or deleted" alert goes to the owner beside the tunnel-health alert. ("The front door")
- `omarchy-mac.org` is registered at Porkbun (order 12534484, expires 2027-11-13, auto-renew on; the account needs credit or a card before then) and is an active Cloudflare zone on Free (`68381e2decfb093d789b6547503729cc`, nameservers `garret.ns.cloudflare.com` and `holly.ns.cloudflare.com`). ("Register the domain and stand up the zone")
- At step 31 of the Cutover the Porkbun `full` key is swapped for one without `purchase`, limited to this domain, and the dead `CLOUDFLARE_KEY` line is dropped (decision S6). The scripts and `lab.env` move into the repository. The scripts read Porkbun's two keys from a file only the owner reads (`--porkbun-keys FILE`, by default `~/.config/omarchy-mac-lab/porkbun.keys`), as they read the Cloudflare token, and refuse one others can read; never from the environment or the Keychain. ("Register the domain and stand up the zone")
- The standing Cloudflare API token leaves the M4 Pro's Keychain at step 31 and stays in Bitwarden for emergencies; routine changes use a short-lived token (decision S2). The token that stays has no expiry: an accepted exception to "Register the domain and stand up the zone", which asked for one (decision S8). ("Operations bar"; "The front door")

### While Dark

- No second way in. When Cloudflare, the tunnel or the home link is gone, the lab is Dark for everyone, Maintainers included. Macs keep retrying and report when it returns. ("The front door")
- Visitors get Cloudflare's stock error page, or a stale copy of a signed-out public page for up to a day. No Worker, no hosted status page. ("Operations leftovers")
- Testing a Mac does not need the lab: a test runs in full on any Mac, Joined or not; only the upload waits. ("Operations leftovers")

### The Alias

- The lab has one Lab address for good. `maclab.omarchy.org` is an Alias: a person who opens it lands on the same page at the Lab address. The lab neither moves nor answers on both names, and never prints the Alias.
- **Route.** A Cloudflare for SaaS custom hostname on `omarchy-mac.org` with the tunnel as fallback origin; the other side adds one DNS-only CNAME. US$0; enabling it needs payment details on the Cloudflare account, which the owner enters.
- **labd, on the Alias.** A page request, `/install` included, gets a 308 to the same path and query at the Lab address. Anything under `/api/*` is refused with a plain line naming the Lab address. No cookie is set, no credential read, no sign-in started. The host check accepts exactly two names. Every link labd builds names the Lab address.
- **The edge.** The flood cap covers both names. The skip rule and the cache rules stay written for the Lab address. Forwards are never cached. No redirect rule at the edge.
- **The zone script** creates the fallback origin and the custom hostname and fails if either drifts. The lab alerts "Agents" once when the Alias stops being active.
- **The ask.** The owner asks Emir, who configures DNS on `omarchy.org`, after Confirmed, with the rehearsal green; the draft is in the ticket. On yes the custom hostname is created that day. Silent: one reminder after 14 days, then dropped. Declined: dropped; a redirect in their own zone is accepted. A record removed later is not chased.
- Serving the lab in place under that name is closed, reopened only at the request of `omarchy.org`'s side.

("Serving maclab.omarchy.org"; "Mapping to maclab.omarchy.org")

## 7. Out-of-band control

- **Ports.** The mini's three rear USB-C ports are three reset lines for Lab-owned Macs (left RID 1, middle RID 0, right RID 2, facing the back). Any Lab-owned Mac goes on any port; no port is written in a Mac's record. A port with a display or power on it instead, connected on CC alone with no product id, is no Mac: the controller and the port check report it so and leave it alone. One port is addressed at a time and a Mac on another port is not disturbed; a Hard reset brings a Mac back in about 36 s. ("Out-of-band wiring"; "macvdmtool port selection on the mini")
- **Tool.** The lab's own fork, `omacom/macvdmtool`, based on the variant whose `--rid` is required and strictly parsed (`GravityLinux/macvdmtool` `d57c485`). Pinned by commit and checksum, built off the mini, installed by script.
- **A bad RID is refused twice**: by the tool, and by the controller, which accepts only the RIDs in its own list for this host (0, 1, 2).
- **One Out-of-band controller**: one root LaunchDaemon for all three ports, loopback only, the port named in every request, one lock so a single tool run happens at a time. Two resets asked together run one after the other.
- **Sighting.** While a Mac's Linux is up with a Thunderbolt link, the mini reads the Mac's name on that port; when it belongs to exactly one Lab-owned Mac's record, the lab remembers the Mac, the port, the partner's product id, the plug count and the Mac's boot id. A placeholder name a Mac's link shows before its hostname is set, early in boot or hung there (`unknown_xd_device`, the kernel's `(none)`, systemd's `localhost` or `archlinux`), is no Mac's name: it neither places, moves nor ends a Sighting, and a Proving reset takes it as a port that shows none. A Hard reset goes out only if the port is still connected with the same product id, and with the same plug count or one the Mac's own restart moved. A Mac's port counts plugs at every restart (the M2 Max's went from 21 to 25 across a job's two reboots), so a higher count keeps the Sighting while the Mac comes back on a new boot, which takes the new count, or is not heard again, as when it hangs after a restart; it ends the Sighting once the Mac is heard again on the boot it had, a cable pulled while it stayed up. While the Mac is still heard on that boot no Hard reset goes out. Anything else drops the Sighting and the reset is refused with the reason. Accepted: two Macs of one model swapped while one of them is silent are not told apart. Any conflict drops the Sightings involved. labd keeps its Sightings in its data: after labd restarts, at a Deploy or with the mini, each one holds while its port is still connected with the same product id, and the rules above go on from the port as it is then (the mini's own restart starts its plug counts again); the log says which were kept and which not. Accepted: cables swapped while labd is down between two Macs of one model are not told apart. The controller reports what it sees; labd holds the Sightings; the controller compares port state once more, under its lock, at the moment of the reset.
- **Proving reset.** A Mac that shows no name is placed by a Maintainer naming the port; it counts only if that Mac returns and every other Mac keeps its boot id.
- **Who.** A Hard reset: a Maintainer, a Lease holder on the Mac they hold, and the recovery ladder. A Proving reset: Maintainers only.
- **With no Sighting** the ladder skips the Hard-reset rung: the Mac's own watchdog, then Needs hands, with the reason shown.
- **Encrypted Macs: no remote unlock.** A Lab-owned Mac must start unattended: unencrypted, or LUKS with `lab-only-autounlock`. The owner enables it on the M2 Max, typing the passphrase, before it Joins. Accepted: whoever holds the M2 Max can read its Omarchy disk.
- **A Mac the mini cannot reach** (beyond three ports, or no cable) gets no Hard reset: its watchdog, then Needs hands, which means the owner at the machine. No second controller host.
- **While Dark** nothing can Hard reset a Mac. After the Unlock the lab runs recovery on whatever did not return and posts the result to "Agents" with the back-after-restart message. With the controller unhealthy: a reset is refused with the reason, the Mac is shown as having no Hard reset, jobs still run, and "Agents" is told.
- **Serial console: out.** Hard reset is the only out-of-band function. `lab shell` over serial, the serial boot-stall check and serial-assisted exec are off. The server refuses the three console routes, the browser terminal and exec's serial path with one plain line. If a console returns, it returns with the Lease check and one record per session: who, which Mac, opened, closed, and what the Mac printed; raw keystrokes are never kept.
- **Cables.** Every rear port gets a Thunderbolt-capable cable.
- Every Maintenance run ends with a status check on each connected port, with no reset.

("Out-of-band wiring"; "Hardening inside the lab"; "CLI changes")

## 8. The Builder and builds

- **One Builder**: one `lab-builder` on the mini, for kernels and packages, one build at a time. The Proxmox worker has no part in the lab. ("Operations leftovers")
- **Limits on a build**: 10 of the mini's 18 cores and 16 of its 64 GB. If the Builder's gate test fails, cores go 10, then 8, then 6. No priorities and no pausing of builds. ("Operations leftovers")
- **Sizes**: the kernel cache volume 40 GB, the package volume 120 GB, the artifact store 20 GB. ("Operations leftovers")
- **Its token.** No shared builder token is made or accepted. The mini's Builder holds one named builder's token in a file only root reads. `lab builder add|remove|list` stays. ("Hardening inside the lab"; "CLI changes")
- **What an artifact fetch must prove**: that the builder holds a running build whose input is that artifact. Uploads and results are tied to that build likewise. ("Hardening inside the lab")
- **Sources.** Public only; no GitHub token on the Builder. labd accepts only `https://github.com/<owner>/<repo>`; another host is added in the server's configuration. An address carrying credentials is refused. For a refused source labd contacts nothing. ("Hardening inside the lab")
- **A build and the mini.** Every build reaches the internet and nothing else: not the mini's services, the home network or the tailnet. That this holds is proved by the first of the two tests below, which gates M team builds, not Ready. ("Hardening inside the lab")
- **Who builds.** Maintainers. An M team member's build is refused until two tests pass: from inside a build the internet answers and the mini's services, a home-network address and a tailnet address do not; an M team member's build cannot read another build's source and leaves nothing a later build reads. When they open: an M team member may ask for a build only for a job on a Mac they lease, one queued or running per member, Maintainers' builds first, on a cache no other build reads or inherits, deleted when the build ends (it may start from a copy of the Maintainers' cache). ("Hardening inside the lab"; "Leases for the M team")
- An M team member may upload artifacts only for a job on a Mac they lease. Every build records who asked. ("Leases for the M team")
- Maintainers' builds share the kept cache. Nothing built on the lab's Builder is shipped to anyone; if that ever changes, the shared cache needs the isolation above first. ("Hardening inside the lab")
- Kernels, packages and the Builder's VM images are never built on GitHub. `builder/Containerfile` and `builder/pkg/Containerfile` are the Builder's VMs. ("Repository layout and release pipeline")

## 9. Publishing, Releases and verdicts

`lab-agent` does not change for any of this. ("Publishing, Releases and verdicts")

### Publishing a lab run

- **One way in.** A lab run's report passes the same checks as any upload (schema, signature, check ids, evidence), handed over inside the application. That path is reachable only from inside labd by a lab job's own run; an upload from outside claiming to be the lab's is counted and treated as anyone's.
- **One signature**: the Mac's Machine key. The lab adds none.
- **Limits.** The lab's own publishing is not counted against the per-address limit; 10 an hour per Mac stays.
- **What can be published.** Only a Known-good run on a packaged kernel. Installed runs and test-kernel runs never are.
- **By itself** the lab publishes only a Known-good run that became the Mac's Reference: each baseline, and a refresh once a week.
- **By hand**: any other Known-good run, by a Maintainer or by an M team member for their own job (`lab publish`, `run --publish`).
- **Verified or Community** is decided by the Mac's sign-in at upload. A lab run on an External volunteer's Mac is a Community run, unless an M team member signed that Mac in, when it is a Verified run like any other (decision S7).
- **The same report twice is refused.** The lab keeps a digest of each report it stores and answers a repeat with "Already uploaded" and the report's link, never its deletion link, for the test tool and for the lab's own publishing. The digest of a deleted report stays.
- **How it arrived** is recorded on each report: by the Mac's own tool, or by the lab, with which job and who asked. Maintainers see it on Reports. The public report page gains one line, "run by the lab", beside "unattended". No aggregate treats it differently.

("Publishing, Releases and verdicts", as changed by "CLI changes"; "Hardening inside the lab")

### The Volunteer's switch, hiding and deleting

- The switch gates one thing: the lab publishing that Mac's reports, checked at the moment of publishing. Off, the test still runs and Maintainers and the M team still read the result. Turning it off leaves Published reports up. The Volunteer's own uploads are untouched by it. `lab-agent publish on` still lists the deletion links.
- A report is Published, Hidden or deleted. Hide and Show: Maintainers. Delete for good: a Maintainer, the Mac's Volunteer (from My Macs), or anyone holding the deletion link; an M team member asks a Maintainer.
- The deletion link opens a confirmation and deletes only when that is sent; the page is `no-store` and sends no referrer.
- The job then says who deleted its report and when; the lab never publishes that job again by itself. Public pages say nothing.
- A Hidden or deleted report, or a changed verdict line, may stay visible from the edge for up to a day.

("Publishing, Releases and verdicts"; "Hardening inside the lab"; "Operations leftovers")

### Releases and the verdict

- A Release is listed from the moment its manifest is frozen: name, who froze it, when, its packages, its Macs. Its Macs are fixed at that moment.
- **Four states**: no verdict, qualified, not qualified, revoked.
- The first verdict is any M team member's, who need not have run the Installed runs. Everything after is a Maintainer's: change, revoke, record again after a revoke.
- A note is required every time.
- **One refusal**: "qualified" with no passed Installed run of that Release. Everything else is a warning in the sentence the person confirms. No quorum.
- History is kept: every verdict stays with who, when and the note. Nothing changes by itself; an Installed run that fails later adds a warning.
- `freeze-entry` is not an exec: it reads the Mac through the lab's read-only identity check under the caller's own Lease; labd checks role, Lease and Availability window; who froze the entry, and when, is recorded. It is refused on a Volunteer's Mac for as long as Installed runs are.

("Publishing, Releases and verdicts"; "Leases for the M team"; "Hardening inside the lab")

### Promotion

- It does not wait for the verdict: it needs that Mac's own passed Installed run of that Release. When the Release is not qualified or revoked the confirm sentence says so.
- A Volunteer's Mac is never promoted: the server refuses it through every way in.
- Revoking undoes nothing: Macs already promoted stay promoted, marked "promoted from a revoked Release".

("Publishing, Releases and verdicts"; "Hardening inside the lab")

### What a visitor sees

- No public Releases page; Promotion is never public.
- A Release and a Candidate set stay two records, linked when the Release's packages match the set's: every package a Verified run of the set names, at its version, and no other. One package alone, the `omarchy` package the lab knows a set by included, links nothing.
- The verdict shows on Candidates, beside the Candidate set the Release matches: "Qualified by the lab, date", "Not qualified, date" or "Qualification revoked, date". No handle, note or Mac. A Release matching no set shows nowhere public.
- The Candidates page's own line is "Passing: Verified runs on N models, nothing failing", "Failing: N failures, M regressions" or "Waiting".

("Publishing, Releases and verdicts")

### The Releases page

- Open to M team members and Maintainers.
- The list: one row per Release, newest first: name; who froze it and when; the state badge with its date; coverage; how many Macs are promoted from it; the matching Candidate set, linked.
- One Release opened in place: what it is (kernel and packages with versions, and who approved its manifest, by handle); its Macs (Mac, Configuration, latest Installed run linked to its job, number of runs, promoted or not) under "n of m Configurations have a passed run"; the verdict history.
- M team member: Record verdict, only while there is none. Maintainer: also Change, Revoke, and Promote on each Mac row with a passed run. All ask twice.
- The page starts nothing: freezing a Release and starting an Installed run stay in `lab`. The kernel gate is not shown.

("Publishing, Releases and verdicts")

## 10. CLIs

One table each; the full rows with their wording are in "CLI changes".

### Two labs

- Our `lab` serves Omarchy Mac Lab only. A person in both labs keeps maclab's own build for Chris's, under a name they choose. Our installer never replaces a `lab` that is not ours.
- A `lab` that does not name itself is refused on every call: "This `lab` was not built for Omarchy Mac Lab", with the install line.
- The lab keeps one minimum `lab` release, raised only when a Lab release's notes say so; an older `lab` is refused in one line with the install line. A `lab` newer than the lab is never refused.
- No minimum `omarchy-m-test` version: a report is refused only for an old report schema or checks the lab does not know. No fleet-wide minimum `lab-agent`.

("CLI changes")

### `omarchy-m-test`

It keeps its name, Python, inside the repository. ("CLI changes"; map)

| | What |
|---|---|
| added | `--upload REPORT`: sends a report a run saved earlier, as it is; Verified or Community is decided by the Mac's sign-in at upload |
| changed | default host, the releases it reads and the install line name the Lab address (release 0.2.0); `--site` stays |
| changed | `--sign-in`, `--status`, `--sign-out` and the once-per-Mac offer speak of Verified runs and Community runs; "Tester" leaves every line a person reads; a Blocked person gets the Blocked sentence |
| changed | upload: while Dark, "Omarchy Mac Lab cannot be reached right now. Your report is saved at `<path>`." and "Upload it later: omarchy-m-test --upload `<path>`"; never prints an error page; "Already uploaded"; names itself in `User-Agent`; refused in Preview with a line pointing at the old site; refused at the Alias naming the Lab address |
| changed | update notice reads this repository's latest Lab release, never a Release candidate |
| kept | the name, `--site`, the wire names, `tester.json`, the state folder |

A first sign-in makes the lab know the person; it gives no browser session, no Lab token and no leave to lend. ("CLI changes")

### `lab`

- **Login.** One login, in its own file `maclab/omarchy-mac-lab.json` under the person's config directory; it never reads or writes `maclab/client.json`. `lab login <url>` prompts for the Lab token, which is never an argument. Every Maintainer token stops at Cutover, so everyone logs in once.
- **Which lab.** Always the saved login. `MACLAB_URL` and `MACLAB_TOKEN` together override it for one call; `MACLAB_URL` alone, naming another address, is refused.

| Command | | What |
|---|---|---|
| `version` | added | its release and the lab's |
| `installed list` | added | every Release with its verdict |
| `installed verdict --release R qualified\|not-qualified --note "…"` | added | records the verdict; run again by a Maintainer, it changes |
| `installed verdict --release R --revoke --note "…"` | added | revokes it, Maintainers only |
| `reset <mac> --prove left\|middle\|right` | added | a Proving reset, Maintainers only |
| `agent-update <mac>` | added | updates a Lab-owned Mac's agent while it is idle, Maintainers only |
| every command | changed | names itself in `User-Agent`; meets the minimum release; says "Omarchy Mac Lab"; while Dark says it cannot be reached and that no reset is possible until it returns; never prints an error page |
| `login` | changed | as above; the Blocked sentence; an expired token gets a line naming the person's own page; at the Alias it fails plainly and stores nothing |
| `exec` | changed | answered as a stream; never sent again after a cut; no serial path; needs the caller's own Lease; kept in the Exec record, its text for 12 months, and the help says never to put a secret in one |
| `installed freeze-entry` | changed | not an exec; needs the caller's own Lease; refused outside the Availability window and on a Volunteer's Mac while Installed runs are |
| `wait`, `build-log -f`, `upload` | changed | calls of at most 50 s; polling from an offset; numbered 32 MiB parts |
| `lease`, `release`, `cancel` | changed | 4 hours by default; refusals name the Configuration held, an unknown Configuration, or the holder and end time |
| `status`, `oob` | changed | a Lease's end and why it ended; sighted, not sighted or no cable; an agent older than the lab's; `status` and `login` add one line when the lab runs a newer release |
| `job`, `jobs` | changed | a report's state in one of four words |
| `publish`, `run --publish` | changed | open to an M team member for their own job; only a Known-good run |
| `installed status --release R` | changed | also the Release's Macs, runs and verdict history |
| `installed promote`, `run --installed` | changed | both refused on a Volunteer's Mac |
| `build`, `package` | changed | a source off the list or carrying credentials is refused; an M team member's build is refused until M team builds open |
| `enroll` | changed | Lab-owned Macs only; its token lasts 1 hour; `--volunteer`, `--contact`, `--login` go |
| `remove` | changed | ends any Mac and Retires its record at once |
| `device` | changed | `--label` and `--model` only |
| `reset` | changed | refused for a Mac the lab cannot place |
| `mcp` | changed | tools follow the commands one for one |
| `shell`, `console`, `serial`, `recovery-mode` | retired | "The serial console is not part of Omarchy Mac Lab." |
| `forget` | retired | |
| `maintainer add\|remove\|list` | retired | Maintainer is a mark on People |

Unchanged: `baseline`, `crashtest`, `run`, `logs`, `retry`, `gate`, `exclude`, `builds`, `build-get`, `pack`, `audit`, `installed freeze-manifest`, `builder add|remove|list`, `boot-test`. On the site only: People, Requests to lend, Invites, Block, "End all access", revoking another person's token, a Configuration override, Pause, Limit, making and renewing a Lab token, Hide, Show and Delete of a report. ("CLI changes"; "Hardening inside the lab")

### `lab-agent`

| Command | | What |
|---|---|---|
| `update` | added | `sudo lab-agent update`, at the Mac, takes the agent from the lab server; a bad signature, or an agent older than the one installed, is refused |
| `setup` | changed | pre-checks first; finds and leaves another lab; arrives at Qualification with publishing off; asks nothing of the window and points to My Macs for it; shows the terms (`--accept-terms <sha256>` keeps its meaning); the Tailscale flags go |
| `move` | changed | only for a change of the Lab address |
| `leave` | changed | its message loses the Tailscale line |
| `status` | changed | says when changed terms are waiting on My Macs |
| `version`, each check-in | changed | its own number, which the lab compares, and the Lab release it came from |
| the built-in test | changed | installs the test tool from the lab's install line |
| `./volunteer` | retired | |
| `setup --token` | kept | a named risk |

Unchanged: the running agent, and `window`, `pause`, `open`, `notify`, `access`, `publish on|off`, `log`, `jobs`, `uninstall`, `preflight`, `known-good`, `sim`. ("CLI changes"; "Terms and privacy")

## 11. Repository and release pipeline

### The repository

- `omacom/omarchy-maclab`: a fresh public repository with maclab's history (`chriskearney/maclab` `main` as it stands on the day of import) and omarchy-m-testing's 255 commits, authors kept; not a GitHub fork. Order: maclab's history, the module path rename (`github.com/iconidentify/maclab` to `github.com/omacom/omarchy-maclab`, one commit), then omarchy-m-testing's history. Created as the first step of the build. ("Fork mechanics and upstream policy")
- Accepted: the owner is a member of `omacom`, not one of its owners; they can administer, transfer or delete the repository. ("Fork mechanics and upstream policy")
- **Licence.** MIT. One `LICENSE` keeping `Copyright (c) 2026 Chris Kearney` and adding `Copyright (c) 2026 Marcelo Alcantara and contributors`; `THIRD_PARTY_NOTICES.md` extended; one README line saying the code began as maclab. ("Fork mechanics and upstream policy")
- **Who develops.** The owner is admin. Write access only for Maintainers; everyone else by pull request from a fork. `main` is protected: a pull request, green CI and the Codex review. The repository's Issues is the only tracker. ("Fork mechanics and upstream policy")
- **The owner's copy** is a nightly mirror of the repository and of every Lab release's files on the backup guest on `pve2`, alerting when it fails. ("Repository layout and release pipeline")

| Path | Holds |
|---|---|
| `cmd/`, `internal/`, `builder/`, `specs/`, `tools/` | as maclab has them |
| `cli/`, `schema/`, `catalogue/`, `installer/`, `release/` | as omarchy-m-testing has them |
| `internal/web/` (`templates/`, `static/`) | the whole interface, compiled into labd |
| `internal/reports/` | the ported site's rules: upload, signatures, aggregation, `reports.db` |
| `deploy/` | what is installed on the mini: LaunchDaemon files, the tunnel's route file, the pin for `container` (the Homebrew tools carry none: decision S10) |
| `scripts/` | `mini/` (set-up, Deploy, Maintenance run, backup, Restore drill), `cloud/`, `cutover/`, `boot-test/`, `workflow/` |
| `docs/` | `adr/`, `acceptance/`, `spec/`, the guides, `history/` |
| `site/` | the Rails reference, deleted when the port passes its acceptance corpus |
| `GLOSSARY.md` | replaces maclab's `CONTEXT.md` |

- Docker goes: `Dockerfile`, `.dockerignore`, `image.yml`, `publish.yml`, `deploy/k8s/` and `deploy/labd.service` are deleted; the smoke checks are rewritten against the plain `labd` binary.
- Three workflows: tests, release, the scheduled catalogue-drift check.

("Repository layout and release pipeline")

### A Lab release

- **One tag, one GitHub Release**, every file with a `.sig`: `omarchy-m-test.tar.gz` and `VERSION`; `lab` for macOS arm64, Linux arm64 and Linux x86-64; one server bundle for the mini (`labd` with its import command, `oobd`, `lab-builder`, the `--rid` reset tool, the `lab-agent` handed to Macs, and the `builder/`, `deploy/` and `scripts/mini/` files). `lab-agent` is also signed by itself and carries the pinned release key. The two texts of section 14 ship with it.
- **One number**: the tag, stamped into `omarchy-m-test`, `lab` and `labd`. `lab-agent` keeps its own number and also reports the Lab release it came from.
- **The trigger** is a tag the owner pushes on `main`; only the owner creates `v*` tags. `vX.Y.Z-rc.N` is a Release candidate, never "latest". `vX.Y.Z` builds, then waits for the owner's approval, which signs and publishes it as latest.
- **Release notes**: the generated list plus fixed lines when they apply: "No way back.", "Needs lab-agent X or later.", "Needs lab X or later.", "Pins changed: needs a Maintenance run."
- **Runners.** All GitHub-hosted: one arm64 Linux runner builds the CLI tarball and cross-compiles every Go binary; one macOS runner builds only the `--rid` reset tool. Tests add the Go tests on macOS. No self-hosted runner, ever.
- **One secret on GitHub**: `RELEASE_SIGNING_KEY`, in an environment `release` with the owner as required reviewer and `v*` tags only. The key is the old site's, moved, not rotated; a copy is in Bitwarden. No lab secret goes to GitHub.
- **Replacing the key**: a new key; a pull request changing the pins; the mini's pin by hand over SSH; a Lab release; a Deploy. It has to reach the Macs' agents as well. No standby key.

("Repository layout and release pipeline"; "Hardening inside the lab"; "CLI changes"; "Terms and privacy"; "Cutover and shutting the old site down")

### Reaching people, Macs and the mini

- `lab`: `curl -fsSL https://lab.omarchy-mac.org/install/lab | bash`: the latest Lab release for the machine, checked against the pinned release key, into `~/.local/bin/lab`, no root; running it again updates.
- While the lab is in Preview both install lines serve the Release candidate the mini runs; from the lab's opening, the latest Lab release (decision S4).
- `/install` (the test tool's install line) is served by the lab.
- `lab-agent`: from the lab server only. A Lab-owned Mac's is updated by a Maintainer through the lab while idle. Every install of an agent checks its signature and refuses an older one.
- **Deploy.** One command the owner runs on the mini over SSH, naming an exact version (a Release candidate allowed, never "latest"): download the bundle and `.sig` with `curl` and check it against the release key pinned on the mini; unpack beside the running build; announce in "Agents" and drain jobs; snapshot both databases; switch and restart the lab's services, not the mini; verify labd locally and through the Front door, the Builder and the Out-of-band controller; report in "Agents". The first Deploy, in Preview before the name answers, is verified on labd locally only (decision S3).
- **The way back** is a Deploy of the previous version, automatic when verification fails. The mini keeps the last three builds. A Lab release only adds to a database; one that cannot says "No way back.", and going back from it restores the snapshot and loses what arrived since.
- `container` and macOS change only in a Maintenance run, which installs what the deployed Lab release pins; the Homebrew tools are checked and brought up to date in the same run (decision S10). A Deploy touches none of them.

("Repository layout and release pipeline"; "Hardening inside the lab")

## 12. Hardening

No second gate on exec or on People, and no Cloudflare Access. What the lab adds instead: ("Hardening inside the lab"; "The front door")

- **`lab exec`: Lease first, for everyone**, so every root shell is announced in the Lab group by handle and Mac.
- **The Exec record**: every `lab exec` on every Mac, Volunteers' Macs at Full included: the person and their handle then, the Mac, when, the command's text, its exit code and how long it ran; a refused attempt too; no output. Kept 12 months, then deleted. Shown to Maintainers, the person who ran it and the Mac's Volunteer.
- **The console is off** (section 7). **`freeze-entry` is not an exec** (section 9). **The Builder** is as in section 8.
- **Secrets: one rule.** No secret on a command line or in a service definition. Each is read from a file only its own account can read, or from a prompt or standard input. `--token`, `MACLAB_BUILDER_TOKEN` and `OOBD_TOKEN` go; the notice topic moves into labd's configuration; the LaunchDaemon files in `deploy/` name files that hold tokens.
- **Notices.** labd checks a Volunteer's own topic when the Mac reports it and each time it sends: `https` only, a public address, judged on the address it actually connects to, no redirect followed. A refused topic is shown on My Macs and the Notice is dropped.
- **The browser.** Every write from a browser, signed in or not, must come from the lab's own pages (`Origin`, or `Sec-Fetch-Site` when there is none); signed-in forms carry the CSRF token as well. After sign-in a person returns only to a path on the Lab address.
- **The lab's own software.** A new `lab-agent` is trusted by signature, checked on the Mac against the key pinned in the agent already running.
- **The Lab-owned Macs' network.** They sit on a network of their own that reaches the internet and nothing in the house, if the owner's router can do it.
- **Each Lab token shows its last use**, when and from which address.

### Accepted risks, by name

From "Hardening inside the lab":

1. A Lab token is all it takes; no second factor on exec or People.
2. A token in use never expires, and a renewed one keeps its secret.
3. `MACLAB_TOKEN` sits in a person's environment.
4. `lab-agent setup --token` puts an enrollment secret on a command line when the Join command is not used.
5. An exec command's text is visible on the Mac while it runs and kept 12 months.
6. Exec output is not kept.
7. A job with a script or an own kernel is root too, with a job record and no Exec record.
8. Maintainers' builds share one cache.
9. A build runs untrusted code in a VM on the mini; escape from the VM is not assessed.
10. A labd restart fails a running build.
11. `lab exec` on a Lab-owned Mac is root on a machine in the owner's home network, if the separate network cannot be done.
12. Whoever holds a report's deletion link can delete that report.
13. A Maintainer is trusted fully; a stolen Lab token of an Admin can add Maintainers, seen only in "Agents".
14. Stopping the whole lab means stopping the tunnel, which only the owner can do.
15. After a theft, a Volunteer's Mac at Full that was touched cannot be cleaned by the lab.
16. Whoever holds the owner's GitHub account or an `omacom` owner's can sign a Lab release, `lab-agent` included.
17. Nothing on a Mac refuses an Installed run; only the server does.

From other tickets: the Free plan's large-file clause has no stated threshold; signed-out visitors can see a page up to a minute old; when Cloudflare is Dark, so is the lab ("The front door"). A standing Cloudflare API token with no expiry sits in Bitwarden (decision S8). The Homebrew tools' versions follow Homebrew, so the gate tests do not bind tool versions and a rebuilt mini gets newer ones (decision S14). Root on one Lab-owned Mac renaming it to another's hostname can draw the other's resets while the other is not sighted ("Out-of-band wiring"). One person can fill the Lab group with request lines until Blocked ("Volunteering a Mac"). The Alias exists at the will of `omarchy.org`'s side and is Dark whenever the lab is ("Serving maclab.omarchy.org"). The accepted costs of "Terms and privacy" are in section 14.

## 13. Operations

The bar: the lab comes back from anything planned by itself, and from anything unplanned as soon as the owner Unlocks it; nothing on the mini is readable by whoever carries it away. ("Operations bar")

### Unlock and restarts

- **Unlock: the owner only, by hand.** At the keyboard, or by SSH from the home LAN with the account password. From outside the house: over the tailnet to `omarchy-gpu`, then from it to the mini; `omarchy-gpu` stays on and on the tailnet for that until another home host takes it over. The password is stored on no machine. If the owner cannot be reached, the lab stays Dark.
- The host key before Unlock is the usual one. Only an Unlock over SSH leaves the mini with no login session.
- **Power.** No UPS: the house battery (Sigenergy) stands in, and the lab does not read it. A hard power loss restarts the mini (`autorestart`), which waits for Unlock. `sleep 0`.
- **Updates.** Nothing that restarts installs by itself: automatic macOS and Security Response installs are off; only XProtect and configuration data stay automatic.
- **Maintenance run.** Started by the owner, monthly; security fixes within a week of release; a major macOS only after `container`, the reset tool and `tailscaled` are confirmed on it. It announces in "Agents", drains jobs, updates (the Homebrew tools among them, copied afresh into their root-owned folder with the previous copy kept: decisions S10 and S14), restarts with `fdesetup authrestart`, verifies every service, includes a Deploy when a newer Lab release exists, ends with the port status check, and reports. A weekly digest lists what is pending.

("Operations bar"; "`container` without a login session"; "Operations leftovers"; "Repository layout and release pipeline"; "Out-of-band wiring")

### Backups

- **What is backed up.** Litestream replicates both databases continuously, about a second behind. Nightly, restic (encrypted, deduplicated) takes the data directory, carrying a `VACUUM INTO` copy of each database and excluding the live database files.
- **In the house.** A small guest on `pve2` receives restic through `rest-server` in append-only mode (pruning runs on the guest) and Litestream over SFTP. The Proxmox Backup Server on `pve2` backs that guest up.
- **Off-site, the databases only.** The nightly `VACUUM INTO` snapshots of `labd.db` and `reports.db`, restic-encrypted, also go to a bucket in Cloudflare R2's free tier.
- **Retention.** restic keeps 7 daily, 4 weekly, 6 monthly; Litestream reaches back 30 days. No copy of the lab's data is kept longer than 6 months, the Proxmox Backup Server's copies of the guest and the R2 snapshots included.
- **Keys.** The mini holds its own restic key, which can be revoked alone. The owner's restic key, the mini's account password, the guest's keys and the R2, Cloudflare and Porkbun credentials, the standing Cloudflare API token among them (decision S2), live in Bitwarden, plus one printed sheet kept in the house. The machine-id key sits with the server's secrets and in the off-machine copy.
- **Restore drill.** Monthly, scripted, on the mini: both databases and one artifact restored to a scratch directory, with integrity and freshness checks, compared with live, alert on failure. Quarterly: a full restore on another machine, once with Litestream and once from the restic snapshots alone. Order in a real loss: files, then databases, then the server, then Litestream.
- A restore from a backup older than a forgetting brings that person back; no list of forgettings is kept.

("Operations bar"; "SQLite consolidation and replication"; "Terms and privacy"; "Data and identity")

### Noticing and telling

- **Dead-man switch.** Every minute the mini fetches its own public URL through Cloudflare and, only on success, pings a check at healthchecks.io. Its alert goes to the owner directly (Signal to the owner's number, and email), not to "Agents". The nightly backup and the monthly drill each ping a check of their own.
- **Second signal**: Cloudflare's tunnel-health alert by email.
- **Alerts to "Agents"**, operations events only, from the lab's Signal sender (`signal-cli` on the mini): a linked device of the owner's own number, or a number of its own that is a member of the group. Linked, lines come from the owner's own number, so his own phone takes them as sent by him, with no notification, unless they are sent with notify_self (signal-cli's `notifySelf`). Sent on change and on recovery, never repeated: back after a restart (with how long it was Dark); a service restarting repeatedly; the tunnel disconnected for over 5 minutes; Litestream failing; a backup older than 36 hours; a failed Restore drill; disk under 15 %; the Builder or the Out-of-band controller unhealthy; `container`'s server found stopped and started again (decision S11); the refused-credential limiter; the Alias no longer active; a Lab-owned Mac starting to Need hands, and its end (a Volunteer's Mac is its Volunteer's Notice); the people lines of section 3; each Deploy and Maintenance run. A weekly digest (pending updates, backup and drill dates) also proves the Signal path works.
- **Status page** at `/status`, served by the lab.

("Operations bar"; "The front door"; "Serving maclab.omarchy.org"; "Hardening inside the lab"; "Repository layout and release pipeline")

### Notices

- The notice server is ntfy.sh, on its free tier. The lab stores no phone number or email.
- A Notice carries the Mac's name, fixed wording and a link to My Macs; its title is "Omarchy Mac Lab". Never a person's handle, the Mac's label, a log line, a kernel or a source. It never asks for a password or for a command to be run.
- Five fixed wordings: a restart is needed, with the physical steps; the Mac's enrollment was ended; a job is starting or is done, with its id and longest duration; "The lab's terms changed. This Mac is waiting for you: open My Macs."; "An update is waiting for this Mac. Open My Macs."
- The Mac's own ntfy topic is shown as a QR code at Join; a Volunteer may set a topic of their own (`lab-agent notify --topic`), which must be a public `https` address. No Notice about Leases.
- What ntfy.sh holds about a Mac: for 12 hours, under the Mac's topic, its name and that one of the fixed things happened. Its logs hold topic names and the addresses of sender and subscribers. The text is readable by ntfy.sh and passes through Google's and Apple's push services. A topic's name is its only secret: whoever has it can read those 12 hours and can publish to it.
- ntfy.sh's 250 messages a day: no cap, queue or retry in the lab. A Notice ntfy.sh refuses is logged and dropped; My Macs shows the Mac's state regardless.

("Operations leftovers"; "Terms and privacy"; "Volunteering a Mac"; "Leases for the M team")

### Changes to make on the mini

1. Create the `lab` account; write the LaunchDaemons of section 5.
2. Remove the Tailscale Standalone app; install `tailscaled`; rejoin tagged `tag:lab`; remove the old node.
3. Turn Wi-Fi off; reserve the mini's address on the router.
4. `sshd`: `PasswordAuthentication no` and `KbdInteractiveAuthentication no` after Unlock.
5. Software Update: `AutomaticallyInstallMacOSUpdates` and `CriticalUpdateInstall` off; `ConfigDataInstall` on.
6. Power: keep `autorestart 1` and `sleep 0`.
7. Install the Homebrew tools, copy them into the root-owned folder the daemons run from, and install the pinned `container`, by script; set `fullfsync`.
8. On `pve2`: the backup guest (`rest-server --append-only`, an SFTP account for Litestream), a PBS job for it, a prune job, the nightly repository mirror.

To sign up for or buy, by the owner: a number for the lab's Signal sender, unless it is linked to the owner's own number; a healthchecks.io account; one R2 bucket with a bucket-scoped token; a Bitwarden account; `tag:lab` and an auth key on Tailscale; payment details on Cloudflare for the Alias. No UPS.

("Operations bar"; "Repository layout and release pipeline"; "Serving maclab.omarchy.org")

## 14. Terms and privacy

All from "Terms and privacy" unless another ticket is named.

### Decisions

1. **Test machines only.** A Volunteer lends only a Test machine. It is point 1 of the terms and the first thing on "Lend a Mac".
2. **Posture.** Plain promises: what is kept, for how long, who to write to, and being Forgotten within 30 days. No law is named, no legal basis claimed, no EU or UK representative appointed.
3. **Who is named.** The lab as a volunteer project run by its Maintainers, its server in Brisbane, Australia, kept by `@maralcbr`. No street address. One contact address, `privacy@omarchy-mac.org`, forwarded to the owner's mailbox by Cloudflare's mail forwarding.
4. **Where the texts live.** Two public pages, `/terms` and `/privacy`, linked from the footer of every page, each showing its date. The terms are also shown on "Lend a Mac" and in the terminal at Join. Both are text files in the repository and ship with a Lab release; the server serves them and hands the terms to the Join command. The terms' version is the sha256 of the shipped text plus its date; earlier versions are the repository's history. A change of terms reaches the lab by a Deploy. ("Repository layout and release pipeline")
5. **No cookie or consent notice.** A signed-out page sets no cookie and runs no analytics.
6. **Acceptance.** The typed `yes` at Join is the one acceptance, recorded with the person, the Mac, the time and the sha256 of the text, kept as long as the Mac's record. ("Volunteering a Mac")
7. **A change of terms asks every enrolled Volunteer again.** From the moment new terms take effect, each Mac whose Volunteer has not accepted them takes nothing from the lab, as when Paused: no job, no Lease. No deadline, never an automatic Leave. The Volunteer accepts on My Macs, signed in, once for all their Macs; that is recorded as the first acceptance was. They are told on My Macs and by one Notice. `lab` says so in one line when a job or a Lease is refused for that reason.
8. **A Notice never asks for a password or a command.**
9. **The suggested name for a Mac is its model alone.**
10. **How long.**

    | What | Kept |
    |---|---|
    | The caller log (each request's address) | 30 days, then deleted |
    | The Exec record | 12 months |
    | Backups | 6 months |
    | A Lab token's last-use address | until the next use or until the token is revoked |
    | Everything else: the person's record, the People record, Lease, job and build records, the Mac's record, collected files, verdict history, reports | as long as the lab runs, or until the person is Forgotten or the report deleted |

    No separate period for the files a job collects from a Volunteer's Mac. No automatic expiry of people who stop signing in.
11. **The M team and Maintainers accept no terms.** The privacy statement says what is kept about them, and that a forgetting keeps the approver's handle in a Release frozen before 2026-11-16.
12. **No further machinery.** No disappearing-message timer on either Signal group, no list of past forgettings, no second kind of terms change.

### Forgetting a person

The person asks on their own page or by mail; the request shows on People; a Maintainer carries it out with one button within 30 days; an Admin for a Maintainer. For a request by mail the Maintainer satisfies themselves that the writer holds the GitHub account; no fixed proof is set. A Blocked person asks by mail and is told the account stays Blocked. If the same GitHub account signs in again later it is a new person with no history. ("Terms and privacy"; "Data and identity")

| Thing | What happens |
|---|---|
| The person's record: account id, handle, marks, leave to lend, contact, request notes, acceptances | Erased |
| Browser sessions and Lab tokens | Erased; they stop at once |
| Leases | Ended, as when a Maintainer ends them |
| Mac sign-ins | Erased; those Macs are signed out |
| The Macs they lend | They Leave; each record is Retired and loses its Volunteer |
| The handle and account id stamped on their reports | Erased |
| The reports themselves, with Verified or Community and the machine id | Kept, Published |
| Report digests | Kept |
| Exec record lines they ran | Kept with the handle until each is 12 months old, then deleted |
| The Audit log on a Volunteer's own Mac | Kept |
| The lab's copy of an Audit log, Lease record lines, "who asked" on jobs and builds | Kept, the handle removed |
| People record lines, about them or by them | Kept, the handle removed |
| Verdict history entries; who froze a Release or an entry; who promoted | Kept with the time and the note, the handle removed |
| Who approved a Release's manifest | From 2026-11-16 a manifest names each approver by account id and the pages show the handle: the lab stops showing them, and the frozen manifest keeps its bytes. The approver's handle in a Release frozen before 2026-11-16 is kept: the manifest is pinned, byte for byte |
| A lab-published report's "who asked" | The handle removed |
| A run's kernel error log that holds the handle | Kept as it was: a Promotion compares it byte for byte, so it is evidence |
| The Blocked state | Kept: the account id, "Blocked", the date and who blocked. The handle and the typed reason are erased |
| Lines already posted to the Lab group or "Agents" | Kept on the members' phones |
| Backups | Kept until they age out, 6 months at most |

A Retired Mac's name is left as it was typed. The lines the People record and "Agents" get for a forgetting carry no handle.

### The Volunteer's terms

Accepted by the owner word for word.

```
OMARCHY MAC LAB: LENDING A MAC
Terms of 2026-11-14

Omarchy Mac Lab is a volunteer project. Nobody is paid and nothing is promised.

1. Lend only a test machine: a Mac with nothing on it, in Omarchy or in
   macOS, that you cannot afford to lose or to have read. It is yours to
   lend and you are 18 or over.
2. What the lab does. Inside the window you set, and only on AC power, it
   restarts this Mac into test kernels and back, runs its tests, and
   collects hardware facts, logs, test output and screenshots, without
   asking each time. A test kernel has full control of the machine and can
   crash it or lose what is on it. There is no warranty.
3. Who does it. The lab's Maintainers, and members of the Omarchy M team
   holding a Lease on this Mac. At Qualification, where every Mac starts:
   test kernels, their own included, and the lab's tests. At Full, which
   only you can choose, at the Mac: also commands and scripts as root. The
   lab does not install an Omarchy release on this Mac; if that changes you
   are asked again.
4. What is kept and seen. Every root command run here at Full is kept by
   the lab for 12 months, with its text and who ran it; you can read it on
   My Macs. The Mac's name, never yours, is posted to the M team's Signal
   group when it joins, is leased, is paused, changes its window or Access
   level, or leaves, and in a daily summary. Its reports are published only
   if you turn that on.
5. What the lab never does. Force this Mac to restart from outside: a hung
   Mac waits for its own watchdog, then for you. Act outside your window.
   Update its own software here: you run the update at this Mac, and it
   installs only if signed by the lab. Ask for a password or a command in a
   Notice.
6. If someone's access to the lab is stolen and this Mac may have been
   touched, you are told. The lab cannot clean your Mac: reinstall it.
7. You are in charge. Set the window, pause, choose the Access level, read
   every record, and leave at any time: Leave on My Macs, or
   `sudo lab-agent leave`.
8. If these terms change, this Mac takes nothing from the lab until you
   accept the new ones on My Macs.

What the lab keeps: https://lab.omarchy-mac.org/privacy
Your yes is recorded with your GitHub account, this Mac, the time and the
sha256 of this text.
```

### The privacy statement

Accepted by the owner word for word.

```
PRIVACY
Statement of 2026-11-14

Omarchy Mac Lab is a volunteer project run by its Maintainers. Its server
is in Brisbane, Australia, kept by @maralcbr. Write to
privacy@omarchy-mac.org.

Visiting. No cookie, no analytics, no tracking. The lab logs the internet
address of each request for 30 days, to keep the lab safe. Pages reach you
through Cloudflare, which sees your address, keeps its own logs for a
limited time, and holds copies of public pages for up to a day, also while
the lab cannot be reached.

Signing in. Sign-in is by GitHub. From GitHub the lab keeps your account
number and handle and nothing else: no email, no name, no picture. A cookie
keeps your browser signed in until you sign out or 30 days pass unused. For
a Lab token you make, the lab keeps when and from which address it was last
used. Maintainers see your handle; so does the Omarchy M team if you lend
or lease a Mac. The public never does.

Reports. A report holds hardware facts and test results. The test tool
strips serial numbers, addresses, host and user names and network names,
shows you the report, and uploads only that. Published reports are public
and free to copy (CC0). If the Mac is signed in with GitHub, your handle is
stamped on its reports, for Maintainers only. A report run by the lab says
so and names nobody. Each upload comes with a deletion link that removes
the report for good; copies others already took are out of our reach, and
Cloudflare's copy can show for up to a day. The lab keeps a fingerprint of
every report file, deleted ones included, so the same file cannot be
posted twice.

Lending a Mac. Lend only a test machine with nothing personal on it. The
lab keeps the Mac's name, hardware facts, settings and jobs, and what its
tests collect: logs, test output, screenshots. Maintainers and the M team
see them. The Mac's name and what happens to it are posted to the M team's
Signal group. Notices to your phone go through ntfy.sh, which holds the
Mac's name and what happened for 12 hours, and through Google's or Apple's
push services. When a Mac leaves, its screenshots are deleted; its history
stays, for Maintainers.

Using a Mac. For the M team: Leases, jobs, builds and verdicts are recorded
with your handle, and Leases are announced in the Signal group. Every root
command is kept 12 months with its text, and the Mac's Volunteer can read
it.

How long. Request log: 30 days. Root commands: 12 months. Backups: 6
months. Everything else: as long as the lab runs, or until you ask to be
forgotten.

Being forgotten. Ask on your own page, or write to us. Within 30 days a
Maintainer removes your record, Lab tokens and sign-ins, your Macs leave,
and your handle comes off your reports and every record. Four things stay:
root commands you ran, until their 12 months are up; lines already sent to
Signal, on its members' phones; the log a Mac keeps for its own Volunteer;
backups, for 6 months. If your account was blocked, the lab keeps its
number so that it stays blocked.

Others who hold something. Cloudflare (delivery, a backup copy of the
databases, mail to the address above). GitHub (sign-in; remove the lab
under Settings, Applications). ntfy.sh. Signal.

Changes. This page carries its date; earlier versions are in the lab's
public repository.
```

### Accepted costs

- If the GDPR or the UK GDPR reaches the lab, a representative in the EU or the UK is formally owed, and the lab has none.
- The owner's GitHub handle and city are public on `/privacy`.
- A Volunteer who lends a Mac that is not a Test machine has broken the terms; nothing stops them.
- A change of terms, however small, quiets every Volunteer's Mac until its Volunteer accepts.
- A stolen browser session can accept changed terms for a Volunteer.
- A restore from a backup older than a forgetting brings that person back.
- A person's handle stays on Signal members' phones, and for up to 12 months on Exec record lines, after they are Forgotten.
- Journals, logs and screenshots collected from a Volunteer's Mac are not scrubbed as reports are, and are kept as long as the lab runs.

## 15. Fleet move

All from "Moving the fleet" unless another ticket is named.

**No Mac is moved. Every Mac Joins afresh**, at the Mac. Nothing is pushed to a Mac, and no proxy or DNS trick stands at an old address. `lab-agent move` stays for one use: a later change of the Lab address.

| Macs | Where they are today | How they reach the lab |
|---|---|---|
| The owner's M1 Pro | Chris's lab, as `m1pro` | The owner, a Maintainer there, ends it with `lab remove m1pro`; it Joins over SSH as `m1-pro`, Lab-owned |
| The owner's M2 Max | no lab | The owner enables `lab-only-autounlock`. It first Joins as the owner's own Volunteer's Mac by the Join command, for the Volunteer's checks in Preview, and Leaves (decision S12); then it Joins over SSH as `m2-max`, Lab-owned ("Out-of-band wiring") |
| 5 Macs of `scottjones`, `RyanTheTide` and `malik-na` | Chris's lab | Each member is asked directly and chooses. A Mac that is the one they use daily is not asked for ("Terms and privacy") |
| 3 Macs of 3 people outside the M team | Chris's lab (1 enrolled, 2 already left) | Asked personally by the owner or by the member who first invited them; they Join as External volunteers after a Maintainer's approval |
| Any other Mac | no lab | section 4 |
| The records `m1pro` and `m1pro2` on `omarchy-gpu` | match no living Mac | Carried as history and Retired at import: out of the fleet list, the Lease on `m1pro` ended, out-of-band entries cleared |

- **Joining from another lab.** The Join command finds the other enrollment, names that lab, asks once, and on yes leaves it properly, then enrolls. A Mac that is Lab-owned in the other lab cannot leave by itself. New device id and secret; no history follows the Mac.
- **Accepted**, for a Lab-owned Mac in Preview: checked in, baseline passed, one job run, sighted on its port or placed by a Proving reset, then one Hard reset. Its Machine key sign-in is made at the old site before the Freeze, carried by the import, and proved after Live by one Verified run (the Confirmed checklist). ("Cutover and shutting the old site down"; "Out-of-band wiring")
- **Order of telling people**: one heads-up line in the M team's Signal group the day `m1pro` leaves Chris's lab; nothing more until the M1 Pro is accepted; after Confirmed, the three members asked directly, then the M team post, then the public notice on "Lend a Mac" and the people outside the team. Draft wording is in the ticket; each ask and post also says that only a Test machine is lent, and the M team post promises no builds until M team builds open. ("Terms and privacy"; "Hardening inside the lab")
- **The way back.** A Mac: ended here (`lab remove`, or `sudo lab-agent leave`), then enrolled again where it was. The data: the `omarchy-gpu` data directory stays untouched for 30 days after Confirmed, then is deleted. The old host stays on and on the tailnet for Unlock, with no labd.

## 16. Cutover

All from "Cutover and shutting the old site down" unless another ticket is named. Step numbers are that ticket's; lettered steps are placed here from the dated notes of the ticket named.

In one line: the lab runs closed at its own name until every test is green; then one day carries the old site across, opens the lab, turns the old name into a redirect and deletes the old site. Opening the lab is the point of no return. Every wait in the order is a check passing, not a date.

| Stage | What it is |
|---|---|
| **Preview** | The lab answers at the Lab address, closed. The old site is untouched. Macs Join and every gate test runs here |
| **Ready** | Every gate test is green. The owner picks a day to be at home, able to Unlock |
| **Live** | The lab is open, holds the old site's reports, and the old name leads to it |
| **Confirmed** | The owner's dated statement that the lab passed its checks. It can be the same day as Live |

**Who.** The owner does every step that needs a password, an account, a purchase, a cable or a message. Everything else is a script the owner starts; the day itself is one script that stops at each owner step and checks what must be true first.

**What Preview serves.** The real lab with the rehearsal copy of the old site's reports; public pages carry a banner naming `omarchy-m-testing.org` as the live site, and `noindex`; uploads and Mac sign-in are refused with a line pointing at the old site; the lab publishes no report; browser sign-in uses the development OAuth app; nobody is invited, with one exception: the M team member who runs the Front door's outside transfer test, whom the owner marks in Preview (decision S15).

### Before Preview (in the build)

| # | Step | Who |
|---|---|---|
| 1 | The old repository's open work is settled: [maralcbr/omarchy-m-testing#61](https://github.com/maralcbr/omarchy-m-testing/pull/61) rebased and merged; [maralcbr/omarchy-m-testing#51](https://github.com/maralcbr/omarchy-m-testing/pull/51) merged if it passes review, else closed with a request to re-open on the new repository; [maralcbr/omarchy-m-testing#1](https://github.com/maralcbr/omarchy-m-testing/issues/1) closed; [maralcbr/omarchy-m-testing#63](https://github.com/maralcbr/omarchy-m-testing/issues/63) filed again by hand, then closed with a link | owner |
| 2 | The old repository's history is pushed into `omacom/omarchy-maclab`. Nothing merges in the old repository afterwards | owner |
| 3 | The name Macs and CLIs are given: the Lab address. Nothing to apply ("Serving maclab.omarchy.org") | - |
| 4 | The Release candidate of 0.2.0 is built | scripts |
| 5 | The existing signing key is put in the new repository's `release` environment and in Bitwarden | owner |
| 6 | The owner's sign-ups and purchases of section 13, the development OAuth app, and the owner's GitHub account id in the lab's configuration | owner |

The Release candidate carries: streamed `lab exec`, numbered-part upload, its own `User-Agent`, no re-send after a cut ("The front door"); the 4-hour Lease default, the new refusals, the table of single-Configuration boards ("Leases for the M team"); `lab login`'s prompt, the Lease rule on a Maintainer's `lab exec`, a `lab-agent` signed by itself ("Hardening inside the lab"); `--upload REPORT`, the new sign-in wording and "Omarchy Mac Lab" in every message ("CLI changes"); lab-agent's built-in test and labd's default publishing address naming the lab, not the old site.

### Preview

| # | Step | Who |
|---|---|---|
| 7 | The mini's changes of section 13, the LaunchDaemon that starts `container` among them ("`container` without a login session") | owner starts the scripts, types the passwords |
| 8 | DNSSEC on for `omarchy-mac.org`; the Cloudflare and Porkbun accounts on a security key or an authenticator; the tunnel-health and "tunnel created or deleted" alerts set | owner |
| 8a | The first Deploy: the owner runs it on the mini over SSH, naming the Release candidate; the signature is checked; it is verified on labd locally, not through the Front door, which does not answer yet. This is the check that a binary cross-compiled on Linux runs under launchd there (decision S3; "Repository layout and release pipeline") | owner |
| 9 | The tunnel and the hostname come up with the final edge settings. The name first answers here, serving Preview | scripts |
| 10 | The `omarchy-gpu` labd is stopped and disabled; its data directory is copied to the mini and upgraded in place; `m1pro` and `m1pro2` are Retired; `admin.token` is retired. The host stays on for Unlock | owner, then scripts |
| 11 | Rehearsal import: the owner uploads one throwaway report to the old site, looks at the certificate chain Railway's Postgres presents, has the import command read Railway's Postgres, its address and the CA file typed at prompts, and write the three tables to a file on the mini (decision S9), derives the machine-id key inside Railway into the mini's secrets and the off-machine copy, and gives one Mac's Machine public key. The import runs; its three acceptance checks pass | owner, then scripts |
| 12 | The Signal group "Omarchy Mac Lab" is created with the lab's sender in it (the owner himself when the sender is linked to his number); labd is given its group id. Until then events are recorded and not posted | owner |
| 13 | The pinned `--rid` tool and the one Out-of-band controller are installed. A Thunderbolt-capable cable on each rear port in use; the M2 Max's cable swapped once with the M1 Pro's to tell cable from kernel | scripts; owner for cables |
| 13a | If the owner's router allows it, the Lab-owned Macs go on a network of their own before they Join ("Hardening inside the lab") | owner |
| 14 | The heads-up line goes to the M team's Signal group; the owner ends `m1pro` in Chris's lab with `lab remove`; the M1 Pro Joins as `m1-pro` by the Maintainer's enroll path and is accepted | owner |
| 14a | The M2 Max as a Volunteer's Mac (decision S12). The owner enables `lab-only-autounlock` on it, typing its passphrase, since the Join command stops on a disk that waits for one; signs in and Joins it as their own Volunteer's Mac by the Join command; the Volunteer's side is checked on a real Mac (the Join command end to end through the Front door; Hardening's test 14); it Leaves | owner |
| 14b | For the Front door's test 5 (decisions S13 and S15): the owner asks one M team member and marks them on People; they sign in with their own GitHub account and take a Lease on `m1-pro`; the script the owner sends uploads and downloads through the real path, under their own login. It comes after step 14, since it needs `m1-pro` Joined | owner; the member |
| 15 | The M2 Max, its auto-unlock enabled in step 14a, Joins as `m2-max` by the Maintainer's enroll path and is accepted | owner |
| 16 | Each Lab-owned Mac shows a Configuration after its baseline, set by a Maintainer where the kernel log gives no GPU count | owner |
| 17 | Each Lab-owned Mac's Machine key is signed in at the old site, if it is not already | owner |
| 17a | The owner enters payment details on Cloudflare; the Alias rehearsal runs on a throwaway subdomain of `omarchy-m-testing.org`, while that name is still on Porkbun's nameservers. Not a gate for Ready or the day; it gates the ask ("Serving maclab.omarchy.org") | owner, then scripts |
| 17b | Recorded, not gates: what a signed-out visitor gets for a cached public page with `cloudflared` stopped, then labd alone stopped ("Operations leftovers"); that a signed-out page load sets no cookie, Cloudflare's included ("Terms and privacy") | scripts |

The Alias rehearsal: the certificate is issued with nothing but the CNAME in place; a page request gets the forward, path and query kept; an API request is refused with the plain line and `lab` pointed at the name stores nothing; no answer carries a cookie; a third name still gets 404 at the tunnel; the Front door's cache test is still green on the Lab address; the record is removed, the hostname is seen to lapse and the alert reaches "Agents"; the throwaway hostname and record are deleted. ("Serving maclab.omarchy.org")

### Ready

All green, with dated output committed under `docs/acceptance/`:

1. **The Front door's fourteen tests**, through the Lab address with the final edge settings, run by a script in the repository. The run sets the tunnel's transport and how many upload parts go at once. Numbers are the ticket's; 3 and 14 fell with the serial console. ("The front door"; "CLI changes")
   - 1: one-hour silent `lab exec` on a real Mac, Keepalive every 30 s, from labd itself.
   - 2: `freeze-entry` on a real Mac.
   - 4: 1 GB upload in parts, cut mid-part, resumes, checksum matches.
   - 5: large upload and download from a wired host outside the house; parts 1 and 4 at a time. The host is an M team member's machine: the owner asks, the member runs a script the owner sends; a draft of the ask is owed and nobody sends it for him (decision S13). It runs under the member's own login, with a Lease on `m1-pro`, after step 14 (step 14b; decision S15).
   - 6: ten clients at once (check-ins, three streams, a download): no cut, no throttle.
   - 7: Go CLI, Python CLI, agent and `curl … /install` pass the edge unchallenged; an exec body full of shell text is not blocked.
   - 8: a forged `CF-Connecting-IP` from outside does not change the logged address.
   - 9: `cloudflared` to the Unix socket, streams included; tunnel restarted mid-stream: clean error, agents back by themselves.
   - 10: origin closed: nothing answers on the LAN or tailnet; the router has no forwards, UPnP off.
   - 11: a cut job wait leaves its job untouched.
   - 12: the eleventh bad credential gets 429; a valid one from the same address passes.
   - 13: cache: `/api/*` and signed-in HTML never HIT; a fingerprinted asset and a signed-out public page do, and the page is served stale while refreshed.
   - 15: QUIC against HTTP/2 on the tunnel: 1 GB each way, CPU recorded.
   - 16: the four performance targets.
2. **Out-of-band wiring's eight tests.** ("Out-of-band wiring")
   - 1: the pinned `--rid` tool on all three ports: status, three resets, power-on from shutdown, a second cabled Mac undisturbed.
   - 2: a bad RID refused by the tool (missing, not a number, negative, no such port) and by the controller.
   - 3: two resets asked at the same moment for two Macs run one after the other, each Mac reset once.
   - 4: Sighting: each Mac sighted on each port after a move; two cables swapped and the resets follow the Macs; how the plug count and connection id move across a reset, a shutdown and a cable pull; the product id readable on a hung and a powered-off Mac.
   - 5: a truly hung M1 Pro (crashed, watchdog off) brought back from the mini.
   - 6: the M2 Max with auto-unlock: three Hard resets, back with no passphrase, timed.
   - 7: the controller as a root LaunchDaemon: after labd restarts alone, after an Unlock and after a planned restart, every Sighting is kept.
   - 8: refusals: a Mac with no Sighting, and the controller stopped (status page and "Agents").
3. **Operations**: one Restore drill, the off-site database copy and the PBS restore included; the Dead-man switch seen to fire and clear; an Unlock from outside the house, through `omarchy-gpu`; the planned-restart rehearsal; the grid-switch test. ("Operations bar")
4. **The rehearsal import** and its three acceptance checks (step 11), read signed in.
5. **One Lease** taken, extended past its end by a running job, and ended by a Maintainer, the first and the last seen in the Lab group, the extension in the Lease record (decision S5). ("Leases for the M team")
6. **Both Lab-owned Macs accepted**, each showing a Configuration.
7. **The Release candidate built**; step 1 done.
8. **Hardening's tests**, fourteen. ("Hardening inside the lab")
   - 1: the console routes and serial exec are refused.
   - 2: exec without the caller's own Lease is refused, a Maintainer included; with one it is recorded, as is a refused attempt.
   - 3: `freeze-entry`, on a Lab-owned Mac: refused without the caller's Lease, and a freeze writes no Exec record.
   - 4: the shared builder token is refused; a named builder cannot fetch an artifact that is not its running build's input.
   - 5: a build source off the list, or carrying credentials, is refused and labd contacts nothing.
   - 6: no secret shows in the process list or in a service definition on the mini.
   - 7: revoking a token and "End all access" each stop the next request and end an open stream; the second also ends the Leases.
   - 8: a people change, a new Lab token and a cut each appear in "Agents".
   - 9: a Notice topic at a private address, or behind a redirect, is refused.
   - 10: a cross-site write is refused; a return address outside the lab lands on the home page; `/api/themes*` is gone; the deletion link asks before deleting.
   - 11: an Exec record is seen only by its three kinds of people, and a record past 12 months is deleted.
   - 12: a `lab-agent` with a bad signature, and an older one, are each refused.
   - 13: an upload from the internet that claims to be the lab's own is counted and treated as anyone's.
   - 14: an Installed run and a Promotion on a Volunteer's Mac are refused through `lab`, the site and a direct request. Run on the M2 Max while it is the owner's Volunteer's Mac, step 14a (decision S12).
9. **The Builder's gate test**, through `lab-builder` against labd on the mini: one cold linux-aurora kernel build, one warm rebuild, one linux-aurora package build; the page target (under 100 ms at the 95th percentile) measured again during the cold kernel build and the package build with a 1 GB transfer in flight. Pass: the builds succeed, the target holds, each volume stays inside its size. The times, peak memory and volume use are recorded with the run. ("Operations leftovers")

Ready waits for both Macs. **Accepted** in Preview means: checked in, baseline passed, one job run, sighted on its port or placed by a Proving reset, then one Hard reset; nothing depends on the serial console, and the Machine key sign-in is proved after Live. M team builds have two tests of their own (section 8), which do not gate Ready.

### The day

| # | Step | Who | Must be true first |
|---|---|---|---|
| 18 | The Front door's fourteen tests are run again | scripts | Ready |
| 19 | `omarchy-m-testing.org`'s DNS moves to Cloudflare: the zone is added to the same account with the same records, still pointing at Railway, and the nameservers are changed at Porkbun. DNSSEC is turned on once it is active | owner | step 18 green. The Alias rehearsal needs this name still on Porkbun's nameservers, so it has run before this step |
| 20 | **Freeze.** Railway is disconnected from the old repository and the `site` service is stopped. `Postgres` stays up | owner | Cloudflare shows the old zone active and its certificate issued |
| 21 | Final dump: the import command reads Railway's Postgres, its address and the rehearsal's CA file typed at prompts, and writes the three tables to a file on the mini (decision S9) | owner | step 20 |
| 22 | The import fills an emptied `reports.db`. Its three acceptance checks pass and the row counts match the dump. Reports are spot-checked signed in | scripts, owner looks | step 21 |
| 23 | The OAuth app is renamed and its callback moved to the Lab address; its client secret goes into the mini's secrets; the lab switches from the development app to it | owner | step 22 |
| 24 | The owner signs in and marks the 14 M team members on People | owner | step 23 |
| 24a | `/terms` and `/privacy` are live with the texts of section 14 and the footer links to them; `privacy@omarchy-mac.org` exists and one test message has arrived ("Terms and privacy") | owner, scripts | before step 25 |
| 25 | **The lab opens. Point of no return.** In one step: uploads and Mac sign-in accepted; the Preview banner and `noindex` off; the edge cache purged once; a "omarchy-m-testing.org is now here" banner on the home page; the old name's 308 rule on, apex and `www`; 0.2.0 published in the new repository (the owner's approval of the tag); from this step the install lines serve the latest Lab release (decision S4). The lab is Live | scripts, owner starts | step 24; the owner says go |
| 26 | The owner attaches 0.2.0's three files as the old repository's last release | owner | step 25 |
| 27 | One line in the M team's Signal group: "omarchy-m-testing.org moved to lab.omarchy-mac.org today. Old links still work. Update omarchy-m-test when it asks." | owner | step 25 |
| 28 | The Confirmed checklist. The owner writes the dated line | owner | step 25; no copy of the lab's data kept longer than 6 months ("Terms and privacy") |
| 29 | The Railway project is deleted. The dump on the mini is deleted | owner | Confirmed |
| 30 | The old repository gets the MIT `LICENSE` and a README line and description pointing at the new repository and site; its `RELEASE_SIGNING_KEY` secret is deleted; it is archived | owner | Confirmed, step 26 |
| 31 | On the M4 Pro, the owner removes the `oobd` and `lab-builder` LaunchDaemons and deletes the standing Cloudflare token from the Keychain; the copy in Bitwarden stays, for emergencies (decision S2). In the same sitting the Porkbun `full` key is swapped for one without `purchase`, limited to this domain, and the dead `CLOUDFLARE_KEY` line is dropped (decision S6). The Cloudflare token kept in Bitwarden gets no expiry (decision S8) | owner only | Confirmed |
| 32 | The three members with Macs in Chris's lab are asked directly; then the M team post; then the public notice on "Lend a Mac", and the people outside the team are asked | owner | Confirmed; the Lab group and the Configurations in place |

The lab opens on the Release candidate that passed Ready. 0.2.0 is built from the same commit and the mini takes it at its next Deploy; no Deploy happens inside step 25. ("Repository layout and release pipeline")

**The Confirmed checklist.**

1. The smoke script is green against the new host: install, upload a golden report, see it, delete it by its link.
2. The old install line installs 0.2.0; an old report link and an old deletion link land on the right pages through the old name.
3. Sign-in with the production OAuth app works; People shows the 14 marked.
4. Each Lab-owned Mac shows as signed in, and the lab has published one Verified run from each.
5. One backup run by hand and restored; the daily summary sent once by hand to the Lab group; the Dead-man switch green since the lab opened.

### Afterwards

| # | Step | Who | When |
|---|---|---|---|
| 33 | The `omarchy-gpu` data directory is deleted. The host stays on for Unlock | owner | 30 days after Confirmed |
| 34 | The home-page banner comes off | scripts | 30 days after Live |
| 35 | The `omarchy-m-testing.org` registration renews by itself and the redirect stays | nobody | every year |
| 35a | The ask to Emir for the Alias, from the draft ("Serving maclab.omarchy.org") | owner | after Confirmed, rehearsal green |
| 35b | The two tests that open M team builds ("Hardening inside the lab") | scripts | when wanted |
| 35c | The mini takes 0.2.0 by a Deploy ("Repository layout and release pipeline") | owner | its next Deploy |

### The old world's end

- **The Freeze.** The old site is stopped; there is no second import. During it the old name answers with Railway's error page; an old CLI keeps its report on the Mac.
- **The old name** is a redirect and nothing else: one 308 rule on its own zone, path and query kept, apex and `www`. No Worker; the lab never answers at that name. Its registration is held indefinitely (auto-renew on, DNSSEC on, the same account security; it next expires 2027-09-26) and lapses only by a dated decision. Accepted: for up to 24 hours after the nameservers change, some resolvers still reach the stopped Railway service.
- **An older installed CLI after Live** is told a newer version exists and prints the old install line, which the redirect carries to the lab's `/install`. Its upload, sign-in, sign-out and status fail with "the site answered HTTP 308" until it is updated. A Mac signed in before the Freeze stays signed in.
- **Chris's deployment** keeps working untouched: its agents follow the redirect to the lab's installer, and its labd re-sends a redirected upload to the lab.
- **Announcements** are the owner's alone and go nowhere but: the M team's Signal group (steps 14, 27, 32), the lab's home page for 30 days, the old repository's README and description, direct messages and the "Lend a Mac" page (step 32).

### The way back

| Where | The way back |
|---|---|
| Preview and Ready | Nothing public has changed. Stop the tunnel. A Mac that Joined is ended with `lab remove` and enrolled again where it was. The `omarchy-gpu` data directory is untouched |
| The day, steps 19 to 24 | Start the Railway `site` service again and move the callback back. About 10 minutes |
| **Step 25** | The point of no return. From here problems are fixed forward |
| Live, before Confirmed | Railway's data is still there as a last resort. No way back is planned |
| After step 29 | Railway keeps a deleted project for 48 hours. After that the lab and its backups hold the only copy |

## 17. Not verified, and tests owed

Everything a ticket marked "Not verified", "not tested" or left as a test, with where it is checked. "Ready n" is item n of the Ready list; "FD", "OOB" and "H" are the Front door's, Out-of-band wiring's and Hardening's tests.

| What | Ticket | Where it is checked |
|---|---|---|
| A Keepalive patch inside labd itself; a real Mac as target; an hour-long `lab exec` | Tunnel spike | Ready 1, FD 1 |
| `freeze-entry` through the tunnel | Tunnel spike | Ready 1, FD 2 |
| Whether labd cancels a job when its waiter is cut | Tunnel spike | Ready 1, FD 11 |
| Large transfers from outside the house; parts 1 or 4 at a time | Tunnel spike; The front door | Ready 1, FD 5, from an M team member's machine under their own login (decisions S13 and S15; step 14b); the draft of the ask is owed |
| More than one client at a time | Tunnel spike | Ready 1, FD 6 |
| The Join command end to end through the Front door | Volunteering a Mac | Ready 1, FD 7; on a real Mac at step 14a (decision S12) |
| An open console blocking a Hard reset | The front door | dropped with the serial console |
| The exact cache header pair | The front door | Ready 1, FD 13 |
| QUIC or HTTP/2 for the tunnel | The front door | Ready 1, FD 15 |
| HTTP/3 on its own; a LAN baseline | The front door | not gating |
| Chrome stalling large downloads over HTTP/3 | The front door | watched after Live |
| The Free plan's large-file threshold | The front door | accepted risk |
| What the edge serves for a cached page once the tunnel is gone | The front door; Operations leftovers | Preview, step 17b, recorded |
| That no cookie reaches a signed-out visitor | Terms and privacy | Preview, step 17b, recorded |
| The `--rid` tool: read, not built or run | Out-of-band wiring | Ready 2, OOB 1 |
| Two resets at the same moment | macvdmtool port selection on the mini | Ready 2, OOB 3 |
| That a cable pulled moves the plug count (a restart moves it too, seen 2026-11-16: the lab tells the two apart by the Mac's boot id) | Out-of-band wiring | Ready 2, OOB 4; if a pull does not move it, the controller watches the ports continuously |
| Recovery of a truly hung Mac | Mac mini bring-up; macvdmtool port selection on the mini | Ready 2, OOB 5 |
| The M2 Max's timed return after a Hard reset | macvdmtool port selection on the mini | Ready 2, OOB 6 |
| `oobd` itself on the mini, on any port | Mac mini bring-up | Ready 2, OOB 1 and 7 |
| Why the M2 Max shows no Thunderbolt link (it had one on 2026-11-16 until 12:40; at every boot since, its link training times out) | Out-of-band wiring | Preview, step 13 |
| Three Macs cabled at once | Out-of-band wiring | when a third Lab-owned Mac arrives |
| A hang inside m1n1 or U-Boot; the mini's front ports | Out-of-band wiring | accepted as unknown |
| Why the M2 Max came up on a lab kernel after its reset | macvdmtool port selection on the mini | not investigated |
| The grid main switch: the mini and network gear stay up; the Sigenergy backup is whole-home | Operations bar | Ready 3; if it fails, the UPS question reopens |
| Unlock from outside the house | Operations bar; `container` without a login session | Ready 3 |
| The Dead-man switch fires and clears | Operations bar | Ready 3 |
| First Restore drill, restic against R2 and the PBS restore of the guest included | Operations bar | Ready 3 |
| Litestream on the mini under launchd beside labd; `fullfsync` in the darwin build | SQLite consolidation and replication | Preview, step 7; Ready 3 |
| The `synchronous` level the SQLite driver uses by default; its `_txlock` option for `BEGIN IMMEDIATE`; Full Disk Access for LaunchDaemons reading the data directory; Litestream while the home uplink is gone | SQLite consolidation and replication | the build; not placed |
| Where the PBS on `pve2` keeps its datastore | Operations bar | not placed |
| How long the PBS keeps the backup guest (6 months promised) | Terms and privacy | before Confirmed, step 28 |
| Whether R2 asks for a payment method | Operations bar | step 6 |
| Whether `softwareupdate --restart` honours `authrestart` | `container` without a login session | first Maintenance run; it can restart with `fdesetup authrestart` itself |
| `container` under an account that has never logged in | `container` without a login session | Preview, step 7, one restart |
| Nothing restarts `container`'s server if it dies | `container` without a login session | settled: the LaunchDaemon checks it every minute (decision S11) |
| That the Homebrew answer covers `tailscaled`: the owner had first picked "Linux runner builds it"; the wider Homebrew answer is the orchestrating session's reading, told to him | decision S10 | stands unless the owner says otherwise |
| That daemons run as root or as `lab` from `/opt/homebrew`, a prefix the admin account can write | decision S10 | settled: they run from a root-owned copy (decision S14) |
| Which Litestream version Homebrew ships, and whether its formula is in core or a tap | decision S14 | Preview, step 7 |
| That "no script on the mini may need `git` or `cc`" still holds for the lab's own scripts, though Homebrew itself needs the command line tools | decision S10 | the build |
| That each tool named has a Homebrew formula with a bottle for this Mac; no formula is installed yet | decision S10 | Preview, step 7 |
| That the mini reaches Railway's Postgres at the address the owner types | decision S9 | Preview, step 11 |
| `container system status` asked from inside a login session about a server in `user/501` | `container` without a login session | not tested; the server is started at boot, before anyone can log in |
| The spike's VM networking failure | Tunnel spike; `container` without a login session | unexplained; what to read if it recurs is in the ticket |
| A linux-aurora kernel and package build on the mini, `lab-builder` end to end | `container` without a login session; Operations leftovers | Ready 9 |
| Whether `container` can cut a build off from the home network and the tailnet | Hardening inside the lab | M team builds' test 1 |
| How much slower an M team build is on a cache of its own; whether the volumes hold the copy | Hardening inside the lab | M team builds' test 2 |
| Whether escape from a build VM is practical | Hardening inside the lab | accepted risk 9 |
| Whether the owner's router can isolate the Lab-owned Macs | Hardening inside the lab | Preview, step 13a; else accepted risk 11 |
| An identity check sent to a Mac outside a job | Hardening inside the lab | Ready 8, H 3 |
| `freeze-entry` at Qualification | Hardening inside the lab | after Installed runs open on Volunteers' Macs |
| A binary cross-compiled on Linux under launchd on the mini | Repository layout and release pipeline | Preview, step 8a, the first Deploy; else the mini's binaries move to the macOS runner |
| The release files' sizes; `omacom`'s Actions settings | Repository layout and release pipeline | when the `release` environment is made, step 5 |
| That every installed Python raises on a 308 answer to a POST | Cutover and shutting the old site down | not checked; either result is harmless |
| How long Cloudflare takes to activate the old zone | Cutover and shutting the old site down | the day, step 20 waits for it |
| Whether the old site's Railway project has backups | Cutover and shutting the old site down | does not change the order |
| A name in another Cloudflare account | Serving maclab.omarchy.org | first seen with the real record, step 35a |
| Whether anything is charged when payment details are entered on Cloudflare | Serving maclab.omarchy.org | Preview, step 17a |
| That `sudo lab-agent uninstall` runs cleanly after `leave` | Volunteering a Mac | the build |
| How the Join command tells that a system is Omarchy | Volunteering a Mac | the build |
| That a check-in's copy of the window carries Pause and Open now | Volunteering a Mac | the build |
| Whether a maclab-built `lab` sends only Go's default `User-Agent` | CLI changes | the build |
| What Chris's server answers to our `lab` | CLI changes | never contacted |
| The `MACLAB_URL` rule against an address differing by a trailing slash or case | CLI changes | the build |
| What `lab` prints today on an edge 530 | Operations leftovers | the build |
| The M4 Pro builder's current limits | Operations leftovers | not readable |
| The GPU core-count line on M3 and later | Leases for the M team | out of scope; a Maintainer sets the Configuration |
| The limits of Cloudflare's mail forwarding on Free | Terms and privacy | step 24a, one test message |
| How long Cloudflare and ntfy.sh keep their own logs; the iPhone Notice path; Signal's 24 hours | Terms and privacy | not checkable; the statement is worded for it |
| Whether collected files are ever cleared by the artifact store's 20 GB budget | Terms and privacy | the build |
| Two wording points in the privacy statement ("shows you the report" for a lab run; the machine id of reports that predate Machine keys) | Terms and privacy | for the owner to weigh at the build |
| The narrow Porkbun key | Register the domain and stand up the zone | step 31, when the `full` key is swapped |
| The Releases page, and an expired token with Renew, were never prototyped | Publishing, Releases and verdicts; The remaining screens | the build |

## 18. Out of scope

From the map.

- Chris's deployment and its data: he is unreachable, so nothing can be exported or moved.
- A promised uptime, a second site or cloud failover.
- Rewriting the lab core (agent, recovery ladder, ESP handling).
- Porting `omarchy-m-test` to Go.
- Checking the GPU core-count line in the kernel log of an M3 or later Mac: needs such a Mac in the lab. Until one Joins, a Maintainer sets its Configuration, as "Leases for the M team" decided.
- Reading the house battery (Sigenergy), for an "on battery" line or a shutdown before it runs flat: the owner ruled it out in "Operations leftovers".
- Building the merged codebase and running the cutover: the map ends at the spec.

Also ruled out by tickets, after the lab opens and outside the map: Rollback on real Macs, then Installed runs on Volunteers' Macs ("Publishing, Releases and verdicts"); bringing the serial console back ("Out-of-band wiring").

## 19. Where a later ticket changed an earlier one

The later ticket won in every row. The earlier Answers are unedited: the change is carried by a dated note on the earlier ticket, or stated by the later Answer itself where it names what it replaces.

| What changed | Earlier | Later |
|---|---|---|
| The M team may lease a Mac at Qualification, run Qualification and record its verdict | People and access | Leases for the M team |
| An External volunteer's "first Mac approved" is the person approved once | People and access | Volunteering a Mac |
| No upstream is followed (was: fixes merged while cheap) | map, settled while charting | Fork mechanics and upstream policy |
| No UPS; off-site backups for the databases only; backups to the guest on `pve2` and R2, not B2 | map; SQLite consolidation and replication | Operations bar |
| Keepalive every 30 s (was at least every 60 s); numbered 32 MiB parts (was 30 MB) | Tunnel spike | The front door |
| The tunnel's route file lists two names (was one) | The front door | Serving maclab.omarchy.org |
| Fourteen Front door tests gate (was sixteen) | The front door; Cutover and shutting the old site down | CLI changes |
| The caller log is kept 30 days | The front door | Terms and privacy |
| The old host's API need not keep answering: a plain 308, an old CLI's upload fails | Site parity inventory | Cutover and shutting the old site down |
| A Lab token expires by disuse and can be renewed (was a date from creation) | Data and identity | The remaining screens |
| A person may end their own Mac sign-in | Data and identity | The remaining screens |
| Blocked also ends the M team mark, the Maintainer role and leave to lend | Data and identity | The remaining screens |
| `lab login` prompts for the token (was unchanged) | Data and identity | Hardening inside the lab |
| A third cut, "End all access"; a token's last use keeps the address | Data and identity; The remaining screens | Hardening inside the lab |
| Builder and out-of-band tokens are files, with no shared builder token (was as in maclab) | Data and identity | Hardening inside the lab |
| A person can be Forgotten | Data and identity | Terms and privacy |
| "Qualification" is no reason for no Lease; Releases opens to the M team | Interface shell and People screen | Leases for the M team; Publishing, Releases and verdicts |
| Open now and Full on My Macs are commands, not buttons | Interface shell and People screen | Volunteering a Mac |
| Updates "only when the owner starts them": the Deploy is a lighter act beside the Maintenance run | Operations bar | Repository layout and release pipeline |
| No copy of the lab's data is kept longer than 6 months | Operations bar | Terms and privacy |
| The conditional Screen Sharing login before a build drops out | Operations bar; Cutover and shutting the old site down | `container` without a login session |
| The M2 Max Joins after auto-unlock and is placed like any Mac (was: Joins without out-of-band) | Moving the fleet | Out-of-band wiring |
| Both Lab-owned Macs Join in Preview; the Machine key sign-in is proved after Live | Moving the fleet | Cutover and shutting the old site down |
| Only a Test machine is asked for; the posts say so | Moving the fleet; Volunteering a Mac | Terms and privacy |
| The Ready list gains "Hardening's tests" and the Builder's gate test; M team builds do not open with the lab | Cutover and shutting the old site down | Hardening inside the lab; Operations leftovers |
| A Maintainer needs their own Lease for `lab exec`; the Exec record covers every Mac | Leases for the M team | Hardening inside the lab |
| The console's fence and record apply only if a console returns; "lab-agent is unchanged" means the running agent | Leases for the M team | CLI changes |
| Installed runs on Lab-owned Macs only; no Install operation runs; the roll-back promise waits | Leases for the M team; Volunteering a Mac | Publishing, Releases and verdicts |
| The suggested name is the model alone (was `<handle>-<model>`) | Volunteering a Mac | Terms and privacy |
| One control on the site gives: accepting changed terms | Volunteering a Mac | Terms and privacy |
| A Notice carries fixed wording only (was maclab's texts), then five wordings (was three) | Volunteering a Mac; Operations leftovers | Operations leftovers; Terms and privacy |
| The owner's copy of the repository is a nightly mirror on the backup guest (was the mini's clone) | Fork mechanics and upstream policy | Repository layout and release pipeline |
| `lab-agent` is trusted by its own signature (was a sha256 from the lab server) | Repository layout and release pipeline | Hardening inside the lab |
| The Notice about a new `lab-agent` gives no line to run | Repository layout and release pipeline | Terms and privacy |
| `lab`'s installer never replaces a `lab` that is not ours; "Needs lab X or later." joins the fixed note lines | Repository layout and release pipeline | CLI changes |
| The same report posted twice is refused (was two reports) | Publishing, Releases and verdicts | CLI changes |
| `freeze-entry` is not an exec and follows the Installed run | Publishing, Releases and verdicts; The front door | Hardening inside the lab |
| A Mac's page has no serial console panel and no console record; the Exec record is for every Mac, shown to three kinds of people | The remaining screens | Hardening inside the lab |
| One named builder, the mini's own (was no named builder) | Operations leftovers | Hardening inside the lab |
| A forgetting's line in "Agents" carries no handle | Hardening inside the lab | Terms and privacy |

### Settled by the owner after the review

In each, two Answers disagreed with nothing to settle it, or a decision the spec or the build plan needs had never been made. Decided 2026-11-14 (Brisbane). Each ticket named carries a dated note: `From the spec's open conflicts (2026-11-14)` for S1 to S8, `From the build plan's questions (2026-11-14)` for S9 to S15. No Answer is edited.

| | What was unsettled | Decided | Tickets noted |
|---|---|---|---|
| S1 | "Hardening inside the lab" (point 17) keeps the Out-of-band controller's token in "a root-only file on both sides"; "Operations bar" (decision 3) runs labd under the unprivileged `lab` account, which cannot read one | Root-only on the controller's side; on labd's side a file only the `lab` account reads | Hardening inside the lab |
| S2 | "Operations bar" (decision 8) moves the standing Cloudflare API token into Bitwarden; "The front door" and "Cutover and shutting the old site down" (step 31) delete it | It is deleted from the M4 Pro's Keychain only, at step 31; a standing token stays in Bitwarden for emergencies. This changes "The front door", which deleted it outright; routine changes still use a short-lived token | Operations bar; The front door; Cutover and shutting the old site down |
| S3 | A Deploy verifies through the Front door ("Repository layout and release pipeline"), which first answers at step 9 ("Cutover and shutting the old site down"); no step put labd on the mini | A step before step 9 (8a): the owner runs the first Deploy over SSH with the Release candidate, its signature checked, verified on labd locally. It is the check that a binary cross-compiled on Linux runs under launchd. Every later Deploy verifies through the Front door | Cutover and shutting the old site down; Repository layout and release pipeline |
| S4 | Jobs install the latest `omarchy-m-test` before each run, but a Release candidate is never "latest" and 0.2.0 is published only at step 25; nothing said what the install lines serve in Preview | While the lab is in Preview, `/install` and `/install/lab` serve the Release candidate the mini runs; from step 25 the latest Lab release | Cutover and shutting the old site down; Repository layout and release pipeline |
| S5 | Ready item 5 wanted a Lease extension "seen in the Lab group"; "Leases for the M team" does not post extensions | Taken and ended are seen in the Lab group; the extension is seen in the Lease record. Extensions stay unposted | Cutover and shutting the old site down; Leases for the M team |
| S6 | "Register the domain and stand up the zone" owes a narrower Porkbun key and the dead `CLOUDFLARE_KEY` line dropped, "once setup is over", with no step | Done at step 31, in the same sitting as S2. The old name's nameserver change at step 19 is the last use of the wide key | Register the domain and stand up the zone; Cutover and shutting the old site down |
| S7 | "Data and identity" has one rule for a Verified run (the Mac's Machine key signed in as an M team member at upload, whoever its Volunteer is); "Publishing, Releases and verdicts" says without condition that a lab run on an External volunteer's Mac is a Community run | The one rule governs: such a run is a Verified run when an M team member signed that Mac in, a Community run otherwise | Publishing, Releases and verdicts; Data and identity |
| S8 | "Register the domain and stand up the zone" also owes "give the Cloudflare token an expiry"; with S2 a standing token is kept, and no ticket set a length | No expiry, as an accepted exception to that ticket: an emergency token must work when it is needed | Register the domain and stand up the zone |
| S9 | "Data and identity" and "Cutover and shutting the old site down" have the owner take a dump of Railway's three tables "straight onto the mini", with no tool named and none on the mini | The import command in the server bundle reads Railway's Postgres itself, its address typed by the owner at a prompt, and writes the three tables to a file on the mini; the import then reads that file. No `psql`, `pg_dump` or `python3` on the mini | Data and identity; Cutover and shutting the old site down; Repository layout and release pipeline; Hardening inside the lab |
| S10 | "Operations bar" (decision 10) has no Homebrew on the mini and every tool pinned by version and checksum; the owner has since installed Homebrew there | Any tool with a Homebrew formula comes from Homebrew: `cloudflared`, `tailscaled`, Litestream, restic, `signal-cli` with its Java. Versions follow Homebrew and are checked in the Maintenance run. `container` and the lab's reset tool stay as they were. The owner can be at the mini's screen for the first set-up if required; the lab's own scripts still need no `python3`, `git` or `cc` in normal running | Operations bar; Repository layout and release pipeline; Cutover and shutting the old site down; Hardening inside the lab |
| S11 | "`container` without a login session" left "nothing restarts the server if it dies" as a choice for the build | The root LaunchDaemon that starts it at boot also checks it every minute and starts it again through the same wrapper; "Agents" gets a line on each change | `container` without a login session; Operations bar; Operations leftovers |
| S12 | Hardening's test 14 and the real-Mac side of Joining need a Volunteer's Mac in Preview, when nobody is invited | The M2 Max first Joins as the owner's own Volunteer's Mac by the Join command, the Volunteer's checks run, it Leaves, then it Joins as `m2-max`, Lab-owned: step 14a, before step 15 | Cutover and shutting the old site down; Hardening inside the lab; Moving the fleet; Volunteering a Mac |
| S13 | The Front door's test 5 needs "a wired host outside the house"; no ticket says whose | An M team member's machine; the member runs a script the owner sends. The owner asks; a draft message is owed, and nobody sends for him | The front door; Cutover and shutting the old site down |
| S14 | With S10 daemons would run as root or as `lab` from `/opt/homebrew`, a prefix the admin account can write | Set-up and each Maintenance run copy the Homebrew tools the lab uses (`tailscaled`, `cloudflared`, Litestream, `restic`, `signal-cli` with its Java) into a root-owned folder; every daemon runs from there, never from `/opt/homebrew`. The previous copy is kept, so a bad upgrade has a way back. Accepted: versions follow Homebrew, so gates do not bind tool versions and a rebuilt mini gets newer ones | Operations bar; Repository layout and release pipeline; Cutover and shutting the old site down; Hardening inside the lab |
| S15 | S13 named whose machine runs the Front door's test 5, not whose login, while "nobody is invited" in Preview | The member's own: in Preview the owner marks that M team member; they sign in with their GitHub account, take a Lease on `m1-pro`, and the script uploads and downloads through the real path. The test comes after step 14. The member is the one exception to "nobody is invited" | The front door; Cutover and shutting the old site down; Leases for the M team |

## 20. Where each ticket landed

| Ticket | Sections |
|---|---|
| Cloudflare Tunnel limits | 6 |
| Lab traffic inventory | 6 |
| Site parity inventory | 2, 5, 19 |
| SQLite consolidation and replication | 5, 13, 17 |
| The Mac mini as a headless server | 5, 13 (its findings as decided by "Operations bar") |
| Domain and Cloudflare automation | 2, 6 (as carried out by "Register the domain and stand up the zone") |
| Mapping to maclab.omarchy.org | 6 |
| Paths around the proxy | 6 |
| People and access | 3, 4 |
| Fork mechanics and upstream policy | 2, 5, 11 |
| Mac mini bring-up | 2, 5, 17 |
| Register the domain and stand up the zone | 6, 17 |
| Tunnel spike | 6, 17 |
| The front door | 5, 6, 12, 16, 17 |
| Data and identity | 3, 5, 14 |
| Interface shell and People screen | 5 |
| Operations bar | 5, 13, 16, 17 |
| Moving the fleet | 4, 15, 16 |
| Cutover and shutting the old site down | 16, 17 |
| Serving maclab.omarchy.org | 6, 16 |
| Leases for the M team | 3, 4, 8, 9 |
| Volunteering a Mac | 4, 13 |
| macvdmtool port selection on the mini | 7, 17 |
| Out-of-band wiring | 7, 15, 16 |
| `container` without a login session | 5, 13, 17 |
| Repository layout and release pipeline | 11, 13, 16 |
| CLI changes | 10, 9, 16 |
| Hardening inside the lab | 3, 8, 12, 16 |
| Publishing, Releases and verdicts | 9, 4 |
| The remaining screens | 3, 5 |
| Terms and privacy | 14, 3, 13 |
| Operations leftovers | 8, 13, 6, 16 |
