# Omarchy Mac Lab

The lab that tests Omarchy on real Apple Silicon Macs and publishes what it finds: one platform that replaces maclab and omarchy-m-testing.

## Language

### People

**Omarchy M team**:
The people in the Signal group "Omarchy M+ - M4, M5 and M6".
_Avoid_: Core team, staff, insiders

**Lab group**:
The Signal group "Omarchy Mac Lab", where the lab tells members of the Omarchy M team who want it about Leases, about Requests to lend, about Macs joining, leaving or changing what they allow and, once a day, the state of every Mac.
_Avoid_: MacLab chat, Agents (the owner's group for the lab's operations alerts)

**Maintainer**:
A person who administers the lab: its Macs, its jobs, its published results and its people. Every Maintainer belongs to the Omarchy M team.
_Avoid_: Operator, owner

**Admin**:
A Maintainer who may also add and remove Maintainers.
_Avoid_: Owner, superuser, root

**Volunteer**:
A person who lends their own Omarchy Mac, a Test machine, to the lab from wherever it lives; a Maintainer who lends theirs is its Volunteer like anyone else. Every Volunteer is either an M team volunteer or an External volunteer.
_Avoid_: Contributor, donor, tester

**M team volunteer**:
A Volunteer who belongs to the Omarchy M team.
_Avoid_: Internal volunteer, team tester

**External volunteer**:
A Volunteer who does not belong to the Omarchy M team.
_Avoid_: Community volunteer, outsider, guest

**Request to lend**:
A person outside the Omarchy M team asking the Maintainers to let them lend Macs. It is answered once, for the person, not for each Mac.
_Avoid_: Application, sign-up, join request (Join is what a Mac does)

**Invite**:
A Maintainer's approval to lend, given to a named person before they ask.
_Avoid_: Linked login, invitation link

### Results

**Verified run**:
A published test report from a Mac that was signed in as a member of the Omarchy M team when it was uploaded.
_Avoid_: Tester run, trusted run, official run

**Community run**:
A published test report that is not a Verified run.
_Avoid_: Unverified run, anonymous run

**Published**:
Said of a test report shown on the public site, whether a person's own upload or a lab run's.
_Avoid_: Uploaded, posted, live

**Hidden**:
Said of a Published report a Maintainer has taken off the public site; it can be shown again. A deleted report is gone for good.
_Avoid_: Withdrawn, unpublished

**Qualification**:
The fleet-wide verdict on one Release, qualified or not qualified, reached by running the Release as installed on Macs and recorded by a named member of the Omarchy M team. Once recorded, only a Maintainer changes or revokes it, and a revoked verdict stays on the Release's history.
_Avoid_: Certification, sign-off

**Release**:
A kernel package together with what the Omarchy installer puts on a Mac, versioned as one unit.
_Avoid_: Build, version, candidate set, Lab release (the lab's own software)

**Candidate set**:
The name an Omarchy image carries for the packages it was built from; published reports are grouped by it. It is not a Release, though a Release's packages may match a set's.
_Avoid_: Release, candidate

**Known-good run**:
A lab run of the tests on a Mac's Known-good kernel, with no test kernel and no Release installed for it. It is the only kind of lab run that can be published.
_Avoid_: Plain run, baseline run

**Reference**:
A Mac's latest accepted Known-good run, which later runs on that Mac are compared with.
_Avoid_: Baseline (the first run that sets a Mac's Reference and Known-good kernel)

**Installed run**:
A run of a Release as the Omarchy installer installs it on a Mac. It is never published.
_Avoid_: Install test, release run

**Promotion**:
A Maintainer making an accepted Installed run a Mac's new Reference and Known-good kernel. A Volunteer's Mac is never promoted.
_Avoid_: Ready for promotion (said of a candidate set fit to ship), release (as a verb)

### Machines

**Lease**:
A Maintainer or a member of the Omarchy M team holding one Mac for themselves until it runs out or they release it: nobody else runs anything on it. A person may hold several at once.
_Avoid_: Reservation, lock, booking

**Access level**:
What Maintainers and Lease holders may do on a Volunteer's Mac, chosen by its Volunteer: Qualification (kernels, the lab's own tests and Installed runs; never a root shell or their own scripts) or Full (everything a Lab-owned Mac allows).
_Avoid_: Permissions, trust level

**Exec record**:
The lab's own line for one `lab exec`: who ran what on which Mac and when, or that it was refused. The lab keeps it, apart from the Audit log a Volunteer's Mac keeps for its Volunteer.
_Avoid_: Audit log, transcript, session log

**Configuration**:
A Mac's board together with its CPU and GPU core counts. Memory and storage size are not part of it.
_Avoid_: Model, spec, variant

**Known-good kernel**:
The kernel a Mac returns to after every boot the lab asked for. On a Volunteer's Mac it is the kernel their own system boots by default.
_Avoid_: Default kernel, stable kernel

**Eligible**:
Said of a Mac whose own system can be captured before an Installed run, so that a Rollback can return it exactly as it was.
_Avoid_: Supported, ready

**Rollback**:
Returning a Volunteer's Mac to its system exactly as it was just before an Installed run.
_Avoid_: Restore, revert

**Join**:
A Mac enrolling with the lab, at the Mac, by its Volunteer or a Maintainer. A Mac that was in another lab Joins like any other and brings no history with it.
_Avoid_: Move, migrate, re-enrol, transfer

**Join command**:
The one line a person gets from the lab and runs on a Mac to Join it. It is theirs alone and good for one Mac, once.
_Avoid_: One-liner, volunteer script, invite

**Retired**:
A Mac's record kept for its history after its enrollment ended. A Retired Mac is not part of the fleet.
_Avoid_: Archived, forgotten, deleted

**Lab-owned Mac**:
A Mac the Maintainers keep for the lab, with no Volunteer.
_Avoid_: Lab Mac, test Mac, house Mac

**Test machine**:
A Mac with nothing on it, in Omarchy or in macOS, that its Volunteer cannot afford to lose or to have read. It is the only kind of Mac a Volunteer lends.
_Avoid_: Spare Mac, test Mac, daily driver (what it is not)

**Hard reset**:
Forcing a Mac to restart from outside it, whatever its own system is doing.
_Avoid_: Power cycle, VDM reset, reboot (a restart the Mac's own system carries out)

**Out-of-band controller**:
The part of the lab that Hard resets Lab-owned Macs over the cables that join them to the lab server.
_Avoid_: oobd, VDM host, controller Mac

**Sighting**:
The lab recognising which Lab-owned Mac is on which of the server's cables, from the name the Mac shows there or a Proving reset. It lasts until the cable is unplugged or changed, across restarts of the lab and of the server.
_Avoid_: Binding, port mapping, detection

**Proving reset**:
A Hard reset a Maintainer aims at one cable to show which Mac is on it, for a Mac the lab has no Sighting of. It counts only if that Mac returns and no other Mac restarts.
_Avoid_: Test reset, qualifying reset

**Needs hands**:
The state of a Mac that a person has to restart at the machine, because nothing the lab can do brings it back.
_Avoid_: Stuck, bricked

### Operations

**Unlock**:
The owner entering the lab server's password after an unplanned restart, so that its disk opens and the lab starts. Until then the lab is Dark.
_Avoid_: Login, boot, wake

**Dark**:
The lab cannot be reached from the internet, whatever the cause: power, the home connection, the tunnel or the server.
_Avoid_: Down, offline, outage

**Dead-man switch**:
A check kept outside the house that expects to hear from the lab every minute and raises an alert when it does not.
_Avoid_: Heartbeat, uptime monitor, ping

**Front door**:
The one way into the lab from the internet. Everything a visitor, a person's `lab` command or a Mac sends to the lab crosses it.
_Avoid_: Edge, proxy, gateway, ingress

**Keepalive**:
A byte the lab sends on an answer it is holding open, so that the Front door does not cut it.
_Avoid_: Heartbeat, ping

**Maintenance run**:
A planned restart of the lab server, started by the owner, that applies updates; the lab announces it beforehand and comes back without an Unlock.
_Avoid_: Patch window, upgrade, downtime

**Lab release**:
One numbered, signed set of the lab's own software: its two commands and its server. It is never called a Release, which is Omarchy's.
_Avoid_: Release, build, version

**Release candidate**:
A Lab release made to be tested, which nobody is offered when they install or update.
_Avoid_: Candidate set (the name an Omarchy image carries), beta, pre-release

**Deploy**:
Putting one Lab release on the lab server, started by the owner. The lab's services restart; the server does not.
_Avoid_: Maintenance run (which restarts the server and may include a Deploy), update, rollout

**Restore drill**:
A scheduled rehearsal that restores the lab's backups somewhere harmless and checks them against the live lab.
_Avoid_: Backup test, verification

**Builder**:
The part of the lab that turns source into a kernel or a package for a Mac to run. It makes one at a time.
_Avoid_: Build server, worker, build farm

**Notice**:
A short message the lab sends to a Volunteer's phone about their Mac: the Mac's name and what is asked or what happened, never a person and never a detail of the Mac. It never asks for a password or for a command to be run.
_Avoid_: Alert (what the lab raises to its owner in "Agents"), notification, push

### Addresses

**Lab address**:
The one address of the lab that Macs, commands and sign-in use, and the only one the lab itself ever names.
_Avoid_: Canonical host, public URL, primary domain

**Alias**:
A name that only forwards people to the Lab address. Nothing is enrolled, signed in or stored against it.
_Avoid_: Second hostname, vanity domain, mirror

### Signing in

**Person**:
Someone the lab knows by their GitHub account, whatever its handle is today.
_Avoid_: User, account, member

**Lab token**:
A secret a person makes for themselves so that the `lab` command and their agents act as them, with whatever their roles allow at that moment. It expires when left unused for as long as the person chose, and they can renew it.
_Avoid_: Maintainer token, admin token, API key

**Machine key**:
The key a Mac's test tool signs its reports with. A Mac that is reinstalled has a new one.
_Avoid_: Device key, machine id (the name the lab derives from the key)

**Mac sign-in**:
A Mac's Machine key tied to one person, so that the reports it signs are uploaded as theirs. It is separate from the Mac's enrollment in the lab, and the person need not be the Mac's Volunteer.
_Avoid_: Tester binding, tester sign-in, linking

**Blocked**:
A person a Maintainer has shut out of the lab: they cannot sign in, lend a Mac or upload as themselves. Unblocking gives back sign-in and nothing else.
_Avoid_: Banned, suspended, removed

**Forgotten**:
Said of a person the lab has removed at their own request: their record, Lab tokens and Mac sign-ins are gone, their Macs have left and their handle is off the records the lab keeps. A Blocked person who is Forgotten stays Blocked.
_Avoid_: Deleted, erased, Retired (said of a Mac)

### Cutover

**Cutover**:
The move from the old report site and the old lab server to the lab, from Preview to Confirmed.
_Avoid_: Migration, launch, go-live

**Preview**:
The lab answering at its own name before it has opened: it shows a rehearsal copy of the old site's reports and takes no uploads and no Mac sign-ins.
_Avoid_: Staging, beta, soft launch

**Ready**:
Every test that gates Live has passed, so the owner may pick the day.
_Avoid_: Green, go

**Freeze**:
Stopping the old site so that nothing new reaches it while its reports are carried to the lab.
_Avoid_: Maintenance window, read-only mode

**Live**:
The lab open to everyone, holding the old site's reports, with the old site's name leading to it.
_Avoid_: Launched, switched, in production

**Confirmed**:
The owner's dated statement, after Live, that the lab has passed its checks and the old site is no longer needed.
_Avoid_: Signed off, accepted (said of a Mac), done

### Origins

**maclab**:
Chris Kearney's lab software, which the Omarchy Mac Lab's code began as and no longer follows.
_Avoid_: Upstream, the original, the old lab
