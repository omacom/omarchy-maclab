package minitest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

const maintenanceLabel = "org.omarchy-mac.lab.maintenance"

// What plays the parts of macOS a Maintenance run touches.
const softwareupdateStub = `#!/bin/bash
echo "softwareupdate $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"; mkdir -p "$s"
case "$1" in
  --list) cat "$s/updates" 2>/dev/null || echo "No new software available." ;;
  --install) [[ ! -f $s/update-fails ]] || { echo "the update could not be installed" >&2; exit 1; }; shift; printf '%s\n' "$@" >> "$s/updates-installed" ;;
  *) exit 64 ;;
esac
`

const fdesetupStub = `#!/bin/bash
echo "fdesetup $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"
case "$1" in
  supportsauthrestart) cat "$s/authrestart" 2>/dev/null || echo true ;;
  authrestart) [[ $# == 1 && ! -f $s/authrestart-fails ]] ;;
  *) exit 64 ;;
esac
`

const swVersStub = `#!/bin/bash
cat "$STUB_STATE/machine/macos" 2>/dev/null || echo 27.2.1
`

// A restic that lists one snapshot, unless its own text says it is a bad one.
const listingRestic = `#!/bin/bash
echo "restic $*" >> "$STUB_LOG"
! grep -q '^# bad' "$0" || { echo "Fatal: unable to open repository" >&2; exit 1; }
case " $* " in
  *" snapshots "*) echo '[{"id":"0a1b2c3d","time":"2026-11-14T16:30:00Z"}]' ;;
esac
`

const twoUpdates = `Software Update Tool

Finding available software
Software Update found the following new or updated software:
* Label: macOS Tahoe 27.2.2-27C80
	Title: macOS Tahoe 27.2.2, Version: 27.2.2, Size: 3412345KiB, Recommended: YES, Action: restart,
* Label: macOS Next 28.0-28A100
	Title: macOS Next 28.0, Version: 28.0, Size: 15412345KiB, Recommended: YES, Action: restart,
`

// run is a mini set up in full and Deployed, with a labd, a Front door and
// a tunnel that answer, as a Maintenance run finds it.
type run struct {
	*whole
	ops  *ops
	door *door
	tag  string
}

func maintained(t *testing.T) *run {
	t.Helper()
	need(t, "sqlite3")
	w := wholeMini(t)
	r := &run{whole: w, tag: "v0.2.0-rc.1"}
	w.keg("restic", "0.19.1", map[string]string{"bin/restic": listingRestic})

	// The Deployed build holds the scripts and the deploy/ folder, as a
	// bundle does. Its check of the ports is slice 29's and has tests of its
	// own: here it writes down that it ran.
	build := w.path("usr/local/lab/builds/" + r.tag)
	for _, dir := range []string{"scripts/mini", "deploy"} {
		entries, err := os.ReadDir(filepath.Join(w.tree, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			info, _ := e.Info()
			write(t, filepath.Join(build, dir, e.Name()), read(t, filepath.Join(w.tree, dir, e.Name())), info.Mode().Perm())
		}
	}
	write(t, build+"/scripts/mini/ports", "#!/bin/bash\necho \"ports $*\" >> \"$STUB_LOG\"\n[[ ! -f $STUB_STATE/ports.fail ]] || { echo 'FAIL  RID 1: the tool failed on a connected port'; exit 1; }\necho 'ok    RID 0 (the middle port): nothing connected'\necho 'ports: every check passed; nothing was reset'\n", 0o755)
	write(t, build+"/scripts/mini/deploy", "#!/bin/bash\necho \"deploy $*\" >> \"$STUB_LOG\"\n[[ ! -f $STUB_STATE/deploy.fail ]]\n", 0o755)
	write(t, build+"/VERSION", r.tag+"\n", 0o644)

	for name, text := range map[string]string{"softwareupdate": softwareupdateStub, "fdesetup": fdesetupStub, "sw_vers": swVersStub} {
		write(t, filepath.Join(w.tools, name), text, 0o755)
	}
	w.labd()
	r.door = w.frontDoor()
	r.ops = w.ops()
	ready := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		if strings.Contains(read(t, w.path("usr/local/lab/tools/current/cloudflared/bin/cloudflared")), "# bad") {
			http.Error(rw, "no connection", http.StatusServiceUnavailable)
		}
	}))
	t.Cleanup(ready.Close)
	latest := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		tag := strings.TrimSpace(read(t, filepath.Join(filepath.Dir(w.root), "state", "latest")))
		if tag == "" {
			http.Error(rw, "rate limited", http.StatusForbidden)
			return
		}
		fmt.Fprintf(rw, `{"tag_name":%q,"prerelease":false}`, tag)
	}))
	t.Cleanup(latest.Close)
	w.env = append(w.env, "LAB_TUNNEL_READY="+ready.URL+"/ready", "LAB_RELEASES_LATEST="+latest.URL,
		"LAB_LITESTREAM_WAIT=0", "LAB_SERVICE_TRIES=1", "LAB_TOOL_TRIES=1", "LAB_BACK_TRIES=1")
	r.latest(r.tag)

	w.ok("setup", w.flags...)
	w.running()
	r.ops.set(func(o *ops) { o.lines = nil; o.parts = "builder ok\ncontroller ok\n" })
	os.Remove(w.log)
	return r
}

func (r *run) latest(tag string) {
	write(r.t, filepath.Join(filepath.Dir(r.root), "state", "latest"), tag+"\n", 0o644)
}

// newer has Homebrew offer formula at version; bin, if any, is its program.
func (r *run) newer(formula, version, bin string) {
	r.t.Helper()
	write(r.t, r.path("opt/homebrew/newer/"+formula), version, 0o644)
	if bin != "" {
		write(r.t, r.path("opt/homebrew/newer/"+formula+".bin"), bin, 0o644)
	}
}

// maintenance runs the Deployed build's own script, as the owner does.
func (r *run) maintenance(args ...string) (string, int) {
	r.t.Helper()
	cmd := exec.Command(r.path("usr/local/lab/current/scripts/mini/maintenance"), args...)
	cmd.Env = r.env
	cmd.Dir = filepath.Dir(r.tree)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	r.t.Fatalf("maintenance: %v\n%s", err, out)
	return "", 0
}

func (r *run) went(args ...string) string {
	r.t.Helper()
	out, code := r.maintenance(args...)
	if code != 0 {
		r.t.Fatalf("maintenance %s: exit %d\n%s\n%s", strings.Join(args, " "), code, out, r.calls())
	}
	return out
}

func (r *run) stopped(args ...string) string {
	r.t.Helper()
	out, code := r.maintenance(args...)
	if code == 0 {
		r.t.Fatalf("maintenance %s went through\n%s", strings.Join(args, " "), out)
	}
	return out
}

// report is the one report of the run.
func (r *run) report() string {
	r.t.Helper()
	files, _ := filepath.Glob(r.path("usr/local/lab/maintenance/report-*.txt"))
	if len(files) != 1 {
		r.t.Fatalf("%d reports", len(files))
	}
	return read(r.t, files[0])
}

func (r *run) version(tool string) string {
	for _, line := range strings.Split(read(r.t, r.path("usr/local/lab/tools/current/VERSIONS")), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == tool {
			return f[1]
		}
	}
	return ""
}

const formulaeAsked = "cloudflared tailscale signal-cli benbjohnson/litestream/litestream restic"

func TestTheMaintenanceRunBeforeTheRestartStepByStep(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	r.newer("restic", "0.20.0", listingRestic)
	r.set("updates", twoUpdates)
	pins := r.path("usr/local/lab/current/deploy/pins")
	write(t, pins, strings.Replace(read(t, pins), "container 1.7.1", "container 1.8.0", 1), 0o644)
	r.ops.set(func(o *ops) { o.drain = []string{"1 0", "0 0"} })
	before := r.copies()

	out := r.went()

	launchd := r.path("Library/LaunchDaemons/")
	order(t, r.calls(),
		"fdesetup supportsauthrestart",
		"ops POST /agents/line Maintenance run on the mini: started.",
		"ops POST /drain", "ops POST /drain",
		"brew update",
		"brew upgrade "+formulaeAsked+" [",
		"installer -pkg",
		"launchctl bootstrap system "+launchd+"/"+cloudflaredLabel+".plist",
		"softwareupdate --install macOS Tahoe 27.2.2-27C80\n",
		"ops POST /agents/line Maintenance run on the mini: restarting now",
		"fdesetup authrestart\n",
	)
	calls := r.calls()
	// Each daemon that runs a Homebrew tool was started from the new copy.
	for _, label := range []string{cloudflaredLabel, tailscaledLabel, litestreamLabel, signalLabel} {
		if !strings.Contains(calls, "launchctl bootstrap system "+launchd+"/"+label+".plist") {
			t.Errorf("%s was not started from the new copy:\n%s", label, calls)
		}
	}
	if n := strings.Count(calls, "brew upgrade"); n != 1 {
		t.Errorf("brew upgrade ran %d times", n)
	}
	for _, never := range []string{"--restart", "shutdown", "reboot", "brew install", "brew services", "DELETE /drain"} {
		if strings.Contains(calls, never) {
			t.Errorf("the run's calls hold %q:\n%s", never, calls)
		}
	}
	if got := r.is("updates-installed"); got != "macOS Tahoe 27.2.2-27C80" {
		t.Errorf("Software Update installed %q", got)
	}

	// A new copy, root's alone, and the one it replaced kept.
	tools := r.path("usr/local/lab/tools")
	now := r.copies()
	if len(before) != 1 || len(now) != 2 {
		t.Fatalf("copies before %v, after %v", before, now)
	}
	if to, _ := os.Readlink(tools + "/previous"); to != before[0] {
		t.Errorf("previous leads to %q, want the copy the run found, %s", to, before[0])
	}
	if r.version("cloudflared") != "2.0" || r.version("restic") != "0.20.0" || r.version("tailscale") != "1.0" {
		t.Errorf("VERSIONS of the new copy:\n%s", read(t, tools+"/current/VERSIONS"))
	}
	if got := read(t, tools+"/previous/cloudflared/bin/cloudflared"); !strings.Contains(got, "cloudflared 1.0") {
		t.Errorf("the previous copy's cloudflared: %q", got)
	}
	sealed(t, tools)
	if got := read(t, r.path("usr/local/lab/installed-pins")); !strings.HasPrefix(got, "container 1.8.0 ") {
		t.Errorf("installed-pins: %q", got)
	}

	// Each version before and after is in the report.
	report := r.report()
	for _, want := range []string{
		"macOS before: 27.2.1",
		"pinned before: container 1.7.1",
		"tools before: litestream 1.0; cloudflared 1.0; restic 0.19.1; signal-cli 1.0; tailscale 1.0",
		"tool cloudflared: 1.0 -> 2.0",
		"tool restic: 0.19.1 -> 0.20.0",
		"tool tailscale: 1.0, as before",
		"pinned container: 1.7.1 -> 1.8.0",
		"macOS Next 28.0-28A100 is a major macOS and was left alone",
		"installing macOS Tahoe 27.2.2-27C80, with no restart of its own",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	if got := read(t, r.path("usr/local/lab/maintenance/upgraded")); got != "cloudflared 1.0 2.0\nrestic 0.19.1 0.20.0\n" {
		t.Errorf("what was upgraded: %q", got)
	}

	// What runs after the restart is the Deployed build's own script, as root.
	file := launchd + "/" + maintenanceLabel + ".plist"
	values, arrays := plist(t, file)
	if got := strings.Join(arrays["ProgramArguments"], " "); got != "/usr/local/lab/current/scripts/mini/maintenance --after-restart" || values["RunAtLoad"] != "true" || values["UserName"] != "" || values["Label"] != maintenanceLabel {
		t.Errorf("the second half's LaunchDaemon: %q %v", got, values)
	}
	if strings.Contains(read(t, file), "/opt/homebrew") || strings.Contains(read(t, file), "EnvironmentVariables") {
		t.Error("the second half's LaunchDaemon names Homebrew or holds an environment")
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("plutil", "-lint", file).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v\n%s", err, out)
		}
	}
	if got := r.ops.told(); len(got) != 2 {
		t.Errorf("\"Agents\" was told %q", got)
	}
	if !strings.Contains(out, "the mini restarts now") {
		t.Errorf("no word of the restart:\n%s", out)
	}

	// A second run does not start over a first that waits for its second half.
	if out := r.stopped(); !strings.Contains(out, "--after-restart") {
		t.Errorf("a run over one that waits:\n%s", out)
	}
}

func TestTheMaintenanceRunAfterTheRestartVerifiesAndReports(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	r.set("macos", "27.2.2\n")
	os.Remove(r.log)

	out := r.went("--after-restart")

	order(t, r.calls(),
		"ops POST /agents/line Maintenance run on the mini: back after the planned restart, on macOS 27.2.2",
		"ops GET /parts",
		"restic snapshots --latest 1 --json",
		"ports \n",
		"ops POST /agents/line Maintenance run on the mini: done.",
	)
	report := r.report()
	for _, want := range []string{
		"the mini is back, macOS 27.2.2 (it was 27.2.1)",
		"ok    " + labdLabel, "ok    " + oobdLabel, "ok    " + tailscaledLabel,
		"ok    labd answers on its loopback port and on its socket",
		"ok    labd answers through the Front door",
		"ok    the Builder and the Out-of-band controller are as labd expects",
		"tool cloudflared: passes its check", "tool tailscale: passes its check", "tool litestream: passes its check",
		"tool restic: passes its check", "tool signal-cli: passes its check",
		"Lab release: " + r.tag + " runs, and the latest is " + r.tag + ": nothing to Deploy",
		"each port, with no reset:", "nothing was reset",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	got := r.ops.told()
	if len(got) != 2 || !strings.Contains(got[1], "cloudflared 2.0") || !strings.Contains(got[1], "Every service and tool passed its check and the ports were checked with no reset") {
		t.Errorf("\"Agents\" was told %q", got)
	}
	for _, never := range []string{"deploy ", "brew", "fdesetup", "softwareupdate --install", "--rid"} {
		if strings.Contains(r.calls(), never) {
			t.Errorf("the second half's calls hold %q:\n%s", never, r.calls())
		}
	}
	for _, gone := range []string{"Library/LaunchDaemons/" + maintenanceLabel + ".plist", "usr/local/lab/maintenance/run", "usr/local/lab/maintenance/upgraded", "usr/local/lab/maintenance/.second-half"} {
		if exists(r.path(gone)) {
			t.Errorf("%s was left behind\n%s", gone, out)
		}
	}
	if out := r.stopped("--after-restart"); !strings.Contains(out, "no Maintenance run waits") {
		t.Errorf("a second half with no first:\n%s", out)
	}
}

// A tool whose new version fails its own check runs the previous copy's
// version again, and the report names the tool and both versions.
func TestAToolThatFailsAfterItsUpgradeIsSwitchedBack(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	r.newer("restic", "0.20.0", listingRestic)
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	os.Remove(r.log)
	tools := r.path("usr/local/lab/tools")

	out := r.stopped("--after-restart")

	if got := read(t, tools+"/current/cloudflared/bin/cloudflared"); !strings.Contains(got, "cloudflared 1.0") {
		t.Fatalf("the copy the daemons run from still holds: %q\n%s", got, out)
	}
	if r.version("cloudflared") != "1.0" || r.version("restic") != "0.20.0" {
		t.Errorf("VERSIONS after the switch back:\n%s", read(t, tools+"/current/VERSIONS"))
	}
	if got := read(t, tools+"/current/HELD"); got != "cloudflared 2.0\n" {
		t.Errorf("HELD: %q", got)
	}
	plist := r.path("Library/LaunchDaemons/" + cloudflaredLabel + ".plist")
	order(t, r.calls(), "launchctl bootout system/"+cloudflaredLabel, "launchctl bootstrap system "+plist)
	for _, label := range []string{tailscaledLabel, litestreamLabel, signalLabel} {
		if strings.Contains(r.calls(), "launchctl bootout system/"+label) {
			t.Errorf("%s was stopped, and only the tunnel's tool failed:\n%s", label, r.calls())
		}
	}
	const said = "tool cloudflared 2.0 failed its check after the upgrade: it was switched back to 1.0 from the previous copy, and 1.0 passes"
	if report := r.report(); !strings.Contains(report, said) || !strings.Contains(report, "the tunnel holds no connection to Cloudflare") || !strings.Contains(report, "tool restic: passes its check") {
		t.Errorf("the report:\n%s", report)
	}
	if got := r.ops.told(); len(got) != 2 || !strings.Contains(got[1], said) {
		t.Errorf("\"Agents\" was told %q", got)
	}
	// The rest of the run went on: the ports were still checked.
	if !strings.Contains(r.calls(), "ports \n") {
		t.Errorf("the ports were not checked after the switch back:\n%s", r.calls())
	}
	if len(r.copies()) != 2 {
		t.Errorf("copies: %v", r.copies())
	}
	if got := read(t, tools+"/previous/cloudflared/bin/cloudflared"); !strings.Contains(got, "cloudflared 1.0") {
		t.Errorf("the previous copy changed: %q", got)
	}
	sealed(t, tools)

	// The set-up leaves the tool where the run put it while Homebrew still
	// has the version that failed, and takes the next one.
	os.Remove(r.log)
	if out := r.ok("setup"); strings.Contains(out, "copy the Homebrew tools") || r.version("cloudflared") != "1.0" {
		t.Errorf("the set-up copied the version that failed again:\n%s", out)
	}
	if out := r.ok("check"); !strings.Contains(out, "A Maintenance run switched it back from 2.0") {
		t.Errorf("the check does not say the tool is held:\n%s", out)
	}
	r.newer("cloudflared", "3.0", "")
	brew := exec.Command(r.path("opt/homebrew/bin/brew"), "upgrade", "cloudflared")
	brew.Env = r.env
	if out, err := brew.CombinedOutput(); err != nil {
		t.Fatalf("brew: %v\n%s", err, out)
	}
	if out := r.ok("setup"); r.version("cloudflared") != "3.0" || exists(tools+"/current/HELD") {
		t.Errorf("the set-up did not take the next version:\n%s", out)
	}
}

func TestAToolThatFailsWithoutAnUpgradeIsReportedAndNotSwitched(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	r.went()
	// Litestream stopped; it was not upgraded.
	write(t, filepath.Join(filepath.Dir(r.root), "state", "loaded."+litestreamLabel+".state"), "waiting\n", 0o644)
	r.ops.set(func(o *ops) { o.lines = nil })
	os.Remove(r.log)

	r.stopped("--after-restart")

	if report := r.report(); !strings.Contains(report, "tool litestream: Litestream does not run (launchd: waiting). It was not upgraded in this run, so there is no version to switch back to") {
		t.Errorf("the report:\n%s", report)
	}
	if strings.Contains(r.calls(), "launchctl bootout") || exists(r.path("usr/local/lab/tools/current/HELD")) || r.version("cloudflared") != "2.0" {
		t.Errorf("something was switched back:\n%s", r.calls())
	}
}

func TestTheMaintenanceRunStopsBeforeAnyChangeWhenJobsStillRun(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	r.ops.set(func(o *ops) { o.drain = []string{"3 0"} })

	out := r.stopped("--wait", "0")

	for _, never := range []string{"brew", "installer", "softwareupdate --install", "fdesetup authrestart", "launchctl boot"} {
		if strings.Contains(r.calls(), never) {
			t.Errorf("the run's calls hold %q:\n%s", never, r.calls())
		}
	}
	order(t, r.calls(), "ops POST /drain", "ops DELETE /drain")
	if got := r.ops.told(); len(got) != 2 || !strings.Contains(got[1], "did not go on: 3 jobs and 0 builds still ran") {
		t.Errorf("\"Agents\" was told %q\n%s", got, out)
	}
	if exists(r.path("usr/local/lab/maintenance/run")) || exists(r.path("Library/LaunchDaemons/"+maintenanceLabel+".plist")) {
		t.Error("a run that stopped left its second half waiting")
	}
}

func TestTheMaintenanceRunStopsWhenHomebrewOrTheSetUpFails(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	write(t, r.path("opt/homebrew/upgrade.fails"), "", 0o644)

	out := r.stopped()

	if !strings.Contains(out, "Homebrew could not upgrade the lab's tools") || strings.Contains(r.calls(), "fdesetup authrestart") || strings.Contains(r.calls(), "softwareupdate --install") {
		t.Errorf("after a failed brew upgrade:\n%s\n%s", out, r.calls())
	}
	if !strings.Contains(r.calls(), "ops DELETE /drain") || len(r.copies()) != 1 || exists(r.path("usr/local/lab/maintenance/run")) {
		t.Errorf("the drain was not lifted, or something changed:\n%s", r.calls())
	}
	if got := r.ops.told(); len(got) != 2 || !strings.Contains(got[1], "stopped: Homebrew could not upgrade") || !strings.Contains(got[1], "not restarted") {
		t.Errorf("\"Agents\" was told %q", got)
	}

	// A new version that still leads back into Homebrew, to a keg the copy
	// does not hold: the set-up refuses the copy, and the run stops with the
	// tools as they were.
	os.Remove(r.path("opt/homebrew/upgrade.fails"))
	r.newer("cloudflared", "2.0", "#!/bin/sh\nexec "+r.path("opt/homebrew")+"/opt/other/libexec/cloudflared \"$@\"\n")
	os.Remove(r.log)
	out = r.stopped()
	if !strings.Contains(out, "the set-up failed") || r.version("cloudflared") != "1.0" || len(r.copies()) != 1 || strings.Contains(r.calls(), "fdesetup authrestart") {
		t.Errorf("after a copy the set-up refused:\n%s", out)
	}
}

func TestTheMaintenanceRunRefusesBeforeItTellsAnyone(t *testing.T) {
	t.Parallel()
	r := maintained(t)

	// From a copy that is not the Deployed build's.
	if out := r.refused("maintenance"); !strings.Contains(out, "/usr/local/lab/current/scripts/mini/maintenance") {
		t.Errorf("run from an unpacked bundle:\n%s", out)
	}
	// On a Mac that cannot restart with no Unlock.
	r.set("authrestart", "false\n")
	if out := r.stopped(); !strings.Contains(out, "cannot restart with no Unlock") {
		t.Errorf("with no authrestart:\n%s", out)
	}
	r.unset("authrestart")
	// With a Homebrew that belongs to root: brew would run as root.
	write(t, r.bin+"/ls", "#!/bin/bash\nif [[ \"$*\" == \"-ld $LAB_SYSROOT/opt/homebrew\" ]]; then echo \"drwxr-xr-x  10 root  wheel  320 Nov 14 10:00 $2\"; exit 0; fi\nexec /bin/ls \"$@\"\n", 0o755)
	if out := r.stopped(); !strings.Contains(out, "brew never runs as root") {
		t.Errorf("with root's Homebrew:\n%s", out)
	}
	os.Remove(r.bin + "/ls")
	for _, args := range [][]string{{"--deploy", "latest"}, {"--deploy", "v0.2"}, {"--from", "https://example.org/x"}, {"--deploy", "v0.2.0", "--from", "http://example.org/x"}, {"--wait"}, {"--restart"}} {
		r.stopped(args...)
	}
	if len(r.ops.told()) != 0 || strings.Contains(r.calls(), "brew") || strings.Contains(r.calls(), "ops ") {
		t.Errorf("a refused run told \"Agents\" or changed something:\n%s", r.calls())
	}
}

func TestTheMaintenanceRunsDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	r.set("updates", twoUpdates)

	out := r.went("--dry-run")

	at := 0
	for _, want := range []string{"would tell \"Agents\" and drain jobs", "would upgrade, as ", "would run ", "would install macOS Tahoe 27.2.2-27C80", "would leave macOS Next 28.0-28A100 alone", "would restart the mini with fdesetup authrestart", "after the restart, by itself", "nothing was changed"} {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("the dry run does not say %q after %q:\n%s", want, out[:at], out)
		}
		at += i
	}
	for _, never := range []string{"brew", "ops ", "installer", "softwareupdate --install", "fdesetup", "launchctl boot"} {
		if strings.Contains(r.calls(), never) {
			t.Errorf("the dry run's calls hold %q:\n%s", never, r.calls())
		}
	}
	if exists(r.path("usr/local/lab/maintenance")) || len(r.copies()) != 1 {
		t.Error("the dry run changed the machine")
	}
}

// A labd whose parts cannot be read is not taken for one whose parts are
// well: every service is checked, and the run ends as wrong.
func TestPartsThatCannotBeReadFailTheCheckOfServices(t *testing.T) {
	t.Parallel()
	for _, parts := range []string{"500", "builder ok\n", "nothing to say\n"} {
		r := maintained(t)
		r.went()
		r.ops.set(func(o *ops) { o.lines = nil; o.parts = parts })

		out := r.stopped("--after-restart")

		if !strings.Contains(r.report(), "FAIL  labd did not say how the Builder and the Out-of-band controller are") || !strings.Contains(r.report(), "FAILED: not every service is up") {
			t.Errorf("parts %q:\n%s\n%s", parts, out, r.report())
		}
	}
}

// The second half has no dry run: asked for one, it refuses, and the run
// that waits is left as it was, for the real second half.
func TestTheSecondHalfsDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	r.latest("v0.2.0")
	waiting := []string{"usr/local/lab/maintenance/run", "usr/local/lab/maintenance/upgraded", "Library/LaunchDaemons/" + maintenanceLabel + ".plist"}

	for _, args := range [][]string{{"--dry-run", "--after-restart"}, {"--after-restart", "--dry-run"}} {
		os.Remove(r.log)
		out := r.stopped(args...)
		if !strings.Contains(out, "no dry run") || !strings.Contains(out, "nothing was changed") {
			t.Errorf("maintenance %v said:\n%s", args, out)
		}
		if calls := r.callsOrNone(); calls != "" {
			t.Errorf("maintenance %v called:\n%s", args, calls)
		}
		for _, f := range waiting {
			if !exists(r.path(f)) {
				t.Errorf("maintenance %v removed %s", args, f)
			}
		}
		if len(r.ops.told()) != 0 {
			t.Errorf("maintenance %v told \"Agents\" %q", args, r.ops.told())
		}
	}
	r.went("--after-restart")
}

func TestTheMaintenanceRunDeploysANewerLabRelease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, latest string
		args         []string
		deploy, said string
	}{
		{"the latest is newer", "v0.2.0", nil, "deploy v0.2.0\n", "Lab release: Deploying v0.2.0 over v0.2.0-rc.1"},
		{"the latest is older", "v0.1.9", nil, "", "the latest is v0.1.9: nothing to Deploy"},
		{"GitHub does not say", "", nil, "", "the latest one could not be read from GitHub; v0.2.0-rc.1 stays"},
		{"the owner named a candidate", "v0.1.9", []string{"--deploy", "v0.2.0-rc.2", "--from", "file:///tmp/a-folder"}, "deploy --from file:///tmp/a-folder v0.2.0-rc.2\n", "Lab release: v0.2.0-rc.2 is Deployed; running its set-up"},
		{"the owner named what runs", "v0.3.0", []string{"--deploy", "v0.2.0-rc.1"}, "", "Lab release: v0.2.0-rc.1 already runs"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := maintained(t)
			r.went(c.args...)
			r.latest(c.latest)
			os.Remove(r.log)

			out := r.went("--after-restart")

			calls := r.calls()
			if c.deploy == "" && strings.Contains(calls, "deploy ") || c.deploy != "" && !strings.Contains(calls, c.deploy) {
				t.Errorf("want the Deploy %q:\n%s", c.deploy, calls)
			}
			if !strings.Contains(r.report(), c.said) {
				t.Errorf("the report lacks %q:\n%s", c.said, out)
			}
			if c.deploy != "" {
				// The Deploy comes after the services and tools are verified
				// and before the ports are checked.
				order(t, calls, "restic snapshots", c.deploy, "ports \n")
			}
		})
	}
}

func TestAFailedDeployOrPortCheckEndsTheRunAsWrong(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.went("--deploy", "v0.2.0-rc.2")
	state := filepath.Join(filepath.Dir(r.root), "state")
	write(t, state+"/deploy.fail", "", 0o644)
	write(t, state+"/ports.fail", "", 0o644)
	os.Remove(state + "/loaded." + oobdLabel)
	r.ops.set(func(o *ops) { o.lines = nil })

	out := r.stopped("--after-restart")

	report := r.report()
	for _, want := range []string{
		"FAIL  " + oobdLabel + " is installed and not loaded",
		"FAILED: not every service is up",
		"FAILED: the Deploy of v0.2.0-rc.2 failed: v0.2.0-rc.1 runs",
		"FAIL  RID 1: the tool failed on a connected port",
		"FAILED: the check of the ports failed",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	if got := r.ops.told(); len(got) != 2 || !strings.Contains(got[1], "ended with something wrong") || !strings.Contains(got[1], "the Deploy of v0.2.0-rc.2 failed") {
		t.Errorf("\"Agents\" was told %q\n%s", got, out)
	}
}

func TestARestartThatDoesNotHappenLeavesTheSecondHalfWaiting(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.set("authrestart-fails", "")

	out := r.stopped()

	if !strings.Contains(out, "run sudo fdesetup authrestart yourself") {
		t.Errorf("no word of what to do:\n%s", out)
	}
	if !exists(r.path("usr/local/lab/maintenance/run")) || !exists(r.path("Library/LaunchDaemons/"+maintenanceLabel+".plist")) {
		t.Error("the second half does not wait")
	}
	r.unset("authrestart-fails")
	r.went("--after-restart")
}

// What was upgraded is written before the set-up, which selects the new
// copy before it starts the daemons: a set-up that fails after that, and a
// run that is started again, keep the version to switch back to.
func TestASetUpThatFailsKeepsWhatWasUpgraded(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	write(t, filepath.Join(filepath.Dir(r.root), "state", "bootstrap.fails."+cloudflaredLabel), "1", 0o644)

	if out := r.stopped(); !strings.Contains(out, "the set-up failed") {
		t.Fatalf("the run did not stop at the set-up:\n%s", out)
	}
	if r.version("cloudflared") != "2.0" {
		t.Fatalf("the set-up did not select the new copy before it failed:\n%s", read(t, r.path("usr/local/lab/tools/current/VERSIONS")))
	}
	upgraded := r.path("usr/local/lab/maintenance/upgraded")
	if !exists(upgraded) || read(t, upgraded) != "cloudflared 1.0 2.0\n" {
		t.Fatalf("what was upgraded, after a set-up that failed: exists %v", exists(upgraded))
	}

	// Started again, the run finds the new copy in use and keeps the record.
	r.went()
	if got := read(t, upgraded); got != "cloudflared 1.0 2.0\n" {
		t.Fatalf("what was upgraded, after the run started again: %q", got)
	}
	r.ops.set(func(o *ops) { o.lines = nil })
	out := r.stopped("--after-restart")
	if r.version("cloudflared") != "1.0" || !strings.Contains(out, "it was switched back to 1.0 from the previous copy, and 1.0 passes") {
		t.Errorf("the tool was not switched back:\n%s", out)
	}
}

// A run started again that upgrades another tool makes another copy: the
// copy that holds the version to switch back to is kept, and the switch back
// takes that version from it.
func TestARerunThatUpgradesAnotherToolStillSwitchesBackToTheRightVersion(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	write(t, filepath.Join(filepath.Dir(r.root), "state", "bootstrap.fails."+cloudflaredLabel), "1", 0o644)
	r.stopped()
	r.newer("restic", "0.20.0", listingRestic)

	r.went()

	if got := read(t, r.path("usr/local/lab/maintenance/upgraded")); got != "cloudflared 1.0 2.0\nrestic 0.19.1 0.20.0\n" {
		t.Fatalf("what was upgraded: %q", got)
	}
	r.ops.set(func(o *ops) { o.lines = nil })
	out := r.stopped("--after-restart")
	tools := r.path("usr/local/lab/tools")
	if got := read(t, tools+"/current/cloudflared/bin/cloudflared"); !strings.Contains(got, "cloudflared 1.0") || r.version("cloudflared") != "1.0" {
		t.Fatalf("switched back to %q, VERSIONS says %s\n%s", got, r.version("cloudflared"), out)
	}
	if !strings.Contains(out, "it was switched back to 1.0 from the previous copy, and 1.0 passes") {
		t.Errorf("the report:\n%s", out)
	}
	// Once the record is closed, the next set-up keeps two copies again.
	r.newer("cloudflared", "3.0", "")
	brew := exec.Command(r.path("opt/homebrew/bin/brew"), "upgrade", "cloudflared")
	brew.Env = r.env
	if out, err := brew.CombinedOutput(); err != nil {
		t.Fatalf("brew: %v\n%s", err, out)
	}
	if out := r.ok("setup"); len(r.copies()) != 2 {
		t.Errorf("copies after the record closed: %v\n%s", r.copies(), out)
	}
}

// A run started again that upgrades the same tool once more keeps the
// version the first one started from: a failed 3.0 goes back to 1.0, which
// passed, never to the 2.0 nobody checked, and the copy of 1.0 is kept.
func TestARerunThatUpgradesTheSameToolAgainSwitchesBackToTheFirstVersion(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	write(t, filepath.Join(filepath.Dir(r.root), "state", "bootstrap.fails."+cloudflaredLabel), "1", 0o644)
	r.stopped()
	r.newer("cloudflared", "3.0", "#!/bin/sh\n# bad\necho cloudflared 3.0\n")

	r.went()

	if got := read(t, r.path("usr/local/lab/maintenance/upgraded")); got != "cloudflared 1.0 3.0\n" {
		t.Fatalf("what was upgraded: %q", got)
	}
	r.ops.set(func(o *ops) { o.lines = nil })
	out := r.stopped("--after-restart")
	if got := read(t, r.path("usr/local/lab/tools/current/cloudflared/bin/cloudflared")); !strings.Contains(got, "cloudflared 1.0") || r.version("cloudflared") != "1.0" {
		t.Fatalf("switched back to %q, VERSIONS says %s\n%s", got, r.version("cloudflared"), out)
	}
	if !strings.Contains(out, "it was switched back to 1.0 from the previous copy, and 1.0 passes") {
		t.Errorf("the report:\n%s", out)
	}
}

// A switch back takes only the version its record names: a previous copy
// that holds another is not copied.
func TestASwitchBackFindsNoCopyOfTheVersionAndChangesNothing(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	write(t, r.path("usr/local/lab/maintenance/upgraded"), "cloudflared 0.9 2.0\n", 0o644)

	r.stopped("--after-restart")

	if got := read(t, r.path("usr/local/lab/tools/current/cloudflared/bin/cloudflared")); !strings.Contains(got, "cloudflared 2.0") || r.version("cloudflared") != "2.0" {
		t.Errorf("a version the record does not name was copied: %q, VERSIONS %s", got, r.version("cloudflared"))
	}
	if !r.tunnelLoaded() || !strings.Contains(r.report(), "could not be switched back to 0.9") {
		t.Errorf("tunnel loaded %v; the report:\n%s", r.tunnelLoaded(), r.report())
	}
}

// signal-cli on Java, switched back and then carried into a new copy, runs
// from the copy it is in: its wrapper names no copy that is pruned.
func TestSignalCLIOnJavaStartsAfterASwitchBackAndANewCopy(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.signalCLIOnJava()
	r.ok("setup")
	tools := r.path("usr/local/lab/tools")
	// labd's test line goes through the sender: here it runs signal-cli.
	write(t, r.path("usr/local/lab/current/bin/labd"), "#!/bin/sh\nexec \"$LAB_SYSROOT/usr/local/lab/tools/current/signal-cli/bin/signal-cli\" >/dev/null\n", 0o755)
	write(t, r.path("var/lab/secrets/signal-agents-group"), groupID+"\n", 0o600)
	r.newer("signal-cli", "0.13.25", "#!/bin/sh\nexit 1\n")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })

	out := r.stopped("--after-restart")

	if r.version("signal-cli") != "0.13.24" || !strings.Contains(out, "it was switched back to 0.13.24 from the previous copy, and 0.13.24 passes") {
		t.Fatalf("signal-cli was not switched back:\n%s", out)
	}
	r.newer("restic", "0.20.0", listingRestic)
	brew := exec.Command(r.path("opt/homebrew/bin/brew"), "upgrade", "restic")
	brew.Env = r.env
	if out, err := brew.CombinedOutput(); err != nil {
		t.Fatalf("brew: %v\n%s", err, out)
	}
	if out := r.ok("setup"); r.version("signal-cli") != "0.13.24" || r.version("restic") != "0.20.0" || len(r.copies()) != 2 {
		t.Fatalf("the next copy: %v\n%s", r.copies(), out)
	}
	current, _ := os.Readlink(tools + "/current")
	if text := read(t, tools+"/current/signal-cli/bin/signal-cli"); strings.Count(text, tools+"/"+current+"/") != 2 {
		t.Errorf("the wrapper does not run its own copy (%s):\n%s", current, text)
	}
	prefix := r.path("opt/homebrew")
	if err := os.Rename(prefix, prefix+".gone"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(prefix+".gone", prefix)
	run := exec.Command(tools + "/current/signal-cli/bin/signal-cli")
	run.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := run.CombinedOutput(); err != nil || string(out) != "openjdk 23.0.2\n" {
		t.Errorf("signal-cli from the copy: %v\n%s", err, out)
	}
}

// A first half that ends before the restart, here at a restart refused,
// lifts the drain and tells "Agents": jobs do not wait for a restart that is
// not coming.
func TestAFirstHalfThatDoesNotRestartLiftsTheDrain(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.set("authrestart-fails", "")

	r.stopped()

	order(t, r.calls(), "ops POST /drain", "fdesetup authrestart", "ops DELETE /drain")
	got := r.ops.told()
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "was not restarted, and jobs start again") {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// A run stopped while it waits for running jobs lifts the drain it set.
func TestAStopDuringTheDrainLiftsIt(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.ops.set(func(o *ops) { o.drain = []string{"1 0"} })
	r.env = append(r.env, "LAB_DRAIN_STEP=1")
	cmd := exec.Command(r.path("usr/local/lab/current/scripts/mini/maintenance"), "--wait", "5")
	cmd.Env = r.env
	cmd.Dir = filepath.Dir(r.tree)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(r.callsOrNone(), "ops POST /drain"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the run never drained:\n%s", r.callsOrNone())
		}
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err == nil {
		t.Fatal("a stopped run ended as done")
	}

	order(t, r.calls(), "ops POST /drain", "ops DELETE /drain")
	if got := r.ops.told(); len(got) == 0 || !strings.Contains(got[len(got)-1], "jobs start again") {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// A switch back whose copy fails puts the tool's folder back as it was,
// with its daemon started, and changes neither VERSIONS nor HELD.
func TestASwitchBackThatCannotCopyLeavesTheToolAsItWas(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	tools := r.path("usr/local/lab/tools")
	prevTool := tools + "/previous/cloudflared/bin/cloudflared"
	if err := os.Chmod(prevTool, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(prevTool, 0o755) })
	os.Remove(r.log)

	r.stopped("--after-restart")

	if got := read(t, tools+"/current/cloudflared/bin/cloudflared"); !strings.Contains(got, "cloudflared 2.0") {
		t.Errorf("the copy the daemons run from holds: %q", got)
	}
	if r.version("cloudflared") != "2.0" || exists(tools+"/current/HELD") || exists(tools+"/current/.cloudflared.failed") {
		t.Errorf("VERSIONS or HELD changed, or the failed folder was left:\n%s", read(t, tools+"/current/VERSIONS"))
	}
	if !r.tunnelLoaded() {
		t.Errorf("the tunnel was left stopped:\n%s", r.calls())
	}
	if !strings.Contains(r.report(), "tool cloudflared 2.0 fails its check, and it could not be switched back to 1.0") {
		t.Errorf("the report:\n%s", r.report())
	}
}

// A stop while a tool is switched back, its daemon stopped, waits until the
// switch back is done and the daemon started again: the run then ends.
func TestAStopDuringASwitchBackLeavesTheDaemonStarted(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	if err := os.Rename(r.bin+"/launchctl", r.bin+"/launchctl.stub"); err != nil {
		t.Fatal(err)
	}
	write(t, r.bin+"/launchctl", "#!/bin/bash\n[[ \"$1 $2\" != \"bootout system/"+cloudflaredLabel+"\" ]] || kill -TERM $PPID\nexec \"$(dirname \"$0\")/launchctl.stub\" \"$@\"\n", 0o755)
	os.Remove(r.log)

	out, code := r.maintenance("--after-restart")

	if code == 0 || !strings.Contains(r.calls(), "launchctl bootout system/"+cloudflaredLabel) {
		t.Fatalf("exit %d\n%s\n%s", code, out, r.calls())
	}
	if !r.tunnelLoaded() {
		t.Errorf("the tunnel was left stopped:\n%s\n%s", out, r.calls())
	}
	if r.version("cloudflared") != "1.0" || !strings.Contains(read(t, r.path("usr/local/lab/tools/current/cloudflared/bin/cloudflared")), "cloudflared 1.0") {
		t.Errorf("the switch back was cut short:\n%s", read(t, r.path("usr/local/lab/tools/current/VERSIONS")))
	}
	if strings.Contains(r.calls(), "ports ") {
		t.Errorf("the run went on after the stop:\n%s", r.calls())
	}
}

// A second stop while the first half's way out lifts the drain does not cut
// it short: jobs start again and "Agents" is told.
func TestASecondStopDuringTheWayOutStillLiftsTheDrain(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.ops.set(func(o *ops) { o.drain = []string{"1 0"} })
	r.env = append(r.env, "LAB_DRAIN_STEP=1")
	cmd := exec.Command(r.path("usr/local/lab/current/scripts/mini/maintenance"), "--wait", "5")
	cmd.Env = r.env
	cmd.Dir = filepath.Dir(r.tree)
	// The lift comes from the run, so after Start; started says so to Go too.
	started := make(chan struct{})
	r.ops.set(func(o *ops) {
		o.onLift = func() {
			<-started
			cmd.Process.Signal(syscall.SIGTERM)
			time.Sleep(300 * time.Millisecond)
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	close(started)
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(r.callsOrNone(), "ops POST /drain"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("the run never drained:\n%s", r.callsOrNone())
		}
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err == nil {
		t.Fatal("a stopped run ended as done")
	}

	order(t, r.calls(), "ops POST /drain", "ops DELETE /drain")
	if got := r.ops.told(); len(got) == 0 || !strings.Contains(got[len(got)-1], "jobs start again") {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// A switch back that cannot write HELD (a full disk) keeps the tool's
// folder as it was until then, and starts its daemon again.
func TestASwitchBackThatCannotWriteHeldLeavesTheDaemonStarted(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	r.newer("cloudflared", "2.0", "#!/bin/sh\n# bad\necho cloudflared 2.0\n")
	r.went()
	r.ops.set(func(o *ops) { o.lines = nil })
	tools := r.path("usr/local/lab/tools")
	if err := os.Mkdir(tools+"/current/HELD.new", 0o755); err != nil {
		t.Fatal(err)
	}
	os.Remove(r.log)

	r.stopped("--after-restart")

	if !r.tunnelLoaded() {
		t.Errorf("the tunnel was left stopped:\n%s", r.calls())
	}
	if got := read(t, tools+"/current/cloudflared/bin/cloudflared"); !strings.Contains(got, "cloudflared 2.0") {
		t.Errorf("the copy the daemons run from holds: %q", got)
	}
	if r.version("cloudflared") != "2.0" || exists(tools+"/current/HELD") || exists(tools+"/current/.cloudflared.failed") || exists(tools+"/current/VERSIONS.new") {
		t.Errorf("VERSIONS or HELD changed, or a file was left:\n%s", read(t, tools+"/current/VERSIONS"))
	}
	if !strings.Contains(r.report(), "tool cloudflared 2.0 fails its check, and it could not be switched back to 1.0") {
		t.Errorf("the report:\n%s", r.report())
	}
}

// Each Homebrew tool by its own check, as the Maintenance run asks it.
func TestEachToolsOwnCheck(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	state := filepath.Join(filepath.Dir(r.root), "state")
	check := func(tool string) (string, int) {
		cmd := exec.Command(r.path("usr/local/lab/current/scripts/mini/check"), "tool", tool)
		cmd.Env = r.env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return string(out), 1
		}
		return string(out), 0
	}
	for _, tool := range []string{"cloudflared", "tailscale", "litestream", "restic", "signal-cli"} {
		if out, code := check(tool); code != 0 || !strings.Contains(out, "check: ") {
			t.Errorf("%s on a mini that is well: exit %d\n%s", tool, code, out)
		}
	}
	for _, c := range []struct {
		tool string
		harm func()
		why  string
	}{
		{"cloudflared", func() {
			write(t, r.path("usr/local/lab/tools/current/cloudflared/bin/cloudflared"), "#!/bin/sh\n# bad\n", 0o755)
		}, "the tunnel holds no connection to Cloudflare"},
		{"tailscale", func() { r.set("tailnet", `{"BackendState":"NeedsLogin","Self":{"Tags":null}}`) }, "the node is not on the tailnet (NeedsLogin)"},
		{"litestream", func() {
			write(t, r.path("var/lab/log/litestream.log"), "", 0o600)
			write(t, r.bin+"/sleep", "#!/bin/bash\necho 'time=2026-11-15 level=ERROR msg=\"sync error\"' >> \"$LAB_SYSROOT/var/lab/log/litestream.log\"\n", 0o755)
		}, "Litestream's log gained 1 errors"},
		{"restic", func() {
			os.Remove(r.path("usr/local/lab/tools/current/restic/bin/restic"))
			write(t, r.path("usr/local/lab/tools/current/restic/bin/restic"), "#!/bin/bash\necho '[]'\n", 0o755)
		}, "restic lists no snapshot of the house copy"},
		{"signal-cli", func() { os.Remove(state + "/loaded." + signalLabel) }, signalLabel + " is not loaded"},
	} {
		c.harm()
		if out, code := check(c.tool); code == 0 || !strings.Contains(out, c.tool+" fails its check: "+c.why) {
			t.Errorf("%s: exit %d, want %q\n%s", c.tool, code, c.why, out)
		}
	}
	if out, code := check("openssl"); code == 0 || !strings.Contains(out, "not on the lab's Homebrew list") {
		t.Errorf("a tool that is not the lab's: exit %d\n%s", code, out)
	}
}

// One line through signal-cli goes to "Agents" and not to the Lab group.
func TestTheSignalCheckSendsOneLineToAgentsOnly(t *testing.T) {
	t.Parallel()
	r := maintained(t)
	write(t, r.path("var/lab/secrets/signal-agents-group"), agentsGroup+"\n", 0o600)
	write(t, r.path("var/lab/secrets/signal-lab-group"), "bGFiLWdyb3VwLWlkLWxhYi1ncm91cC1pZC1sYWItZ3JvdXA=\n", 0o600)
	write(t, r.path("usr/local/lab/current/bin/labd"), "#!/bin/bash\necho \"labd $* [$(ls \"$4\" | tr '\\n' ' ')]\" >> \"$STUB_LOG\"\n[[ ! -f $STUB_STATE/signal.fails ]]\n", 0o755)
	check := exec.Command(r.path("usr/local/lab/current/scripts/mini/check"), "tool", "signal-cli")
	check.Env = r.env
	if out, err := check.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	sent := regexp.MustCompile(`labd --signal-socket \S+/var/lab/run/signal-cli\.sock --signal \S+ --signal-test \[signal-agents-group \]`)
	if !sent.MatchString(r.calls()) {
		t.Errorf("labd's sender was not asked for one line to \"Agents\" alone:\n%s", r.calls())
	}
	write(t, filepath.Join(filepath.Dir(r.root), "state", "signal.fails"), "", 0o644)
	check = exec.Command(r.path("usr/local/lab/current/scripts/mini/check"), "tool", "signal-cli")
	check.Env = r.env
	if out, err := check.CombinedOutput(); err == nil || !strings.Contains(string(out), `a line through signal-cli to "Agents" was not sent`) {
		t.Errorf("a line that did not go: %v\n%s", err, out)
	}
}

// The Maintenance run's own rules, read from the script.
func TestTheMaintenanceRunsRules(t *testing.T) {
	text := read(t, filepath.Join(repo(t), "scripts", "mini", "maintenance"))
	code := regexp.MustCompile(`(?m)#.*$`).ReplaceAllString(text, "")
	for _, want := range []string{"brew never runs as root", "sys fdesetup authrestart", "brew_as_owner upgrade $list", `"$here/check" services`, `"$here/check" tool`, "scripts/mini/ports"} {
		if !strings.Contains(text, want) {
			t.Errorf("the Maintenance run lacks %q", want)
		}
	}
	for _, never := range []string{"--restart", "shutdown", "reboot", "brew_as_owner install", "brew_as_owner services", "--rid", "sudo -u root"} {
		if strings.Contains(code, never) {
			t.Errorf("the Maintenance run holds %q", never)
		}
	}
	if n := strings.Count(code, "brew_as_owner upgrade"); n != 1 {
		t.Errorf("brew upgrade is run in %d places", n)
	}
	deploy := regexp.MustCompile(`(?m)#.*$`).ReplaceAllString(read(t, filepath.Join(repo(t), "scripts", "mini", "deploy")), "")
	for _, never := range []string{"brew", "installer", "softwareupdate", "fdesetup", "LAB_TOOLS", "tools/"} {
		if strings.Contains(deploy, never) {
			t.Errorf("Deploy holds %q: it touches no tool and restarts no machine", never)
		}
	}
}
