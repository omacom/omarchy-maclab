package minitest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The mini's side of the Builder's gate test, scripts/mini/build-watch, on
// a made-up mini whose process list, pf and memory are what the test says.
// The set-up and the watch of container are the real scripts.

const watchVMStat = `#!/bin/bash
cat <<'END'
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               100000.
Pages active:                            1000000.
Pages inactive:                           500000.
Pages wired down:                         300000.
Pages occupied by compressor:              10720.
END
`

const watchPfctl = `#!/bin/bash
echo "pfctl $*" >> "$STUB_LOG"
case "$*" in
  "-s info") echo "Status: Enabled for 0 days 00:10:20           Debug: Urgent" ;;
  "-a com.apple.internet-sharing/shared_v4 -s nat") echo "nat on en0 inet from 192.168.64.0/24 to any -> (en0:0) extfilter ei" ;;
  *) exit 1 ;;
esac
`

const watchPageTimes = `#!/bin/bash
echo "page-times $*" >> "$STUB_LOG"
printf 'probe_pages=13\nprobe_requests=260\nprobe_p95_ms=12\nprobe_max_ms=40\nday_requests=9000\nday_p95_ms=20\n'
`

const vmProcess = "/System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"

// building is the process list while the Builder's VM for build id is up,
// its processes holding the given kB.
func building(id, cores string, kb ...int) string {
	text := "  4096 /sbin/launchd\n  9000 /usr/local/bin/container run --rm --name maclab-" + id + " --cpus " + cores + " --memory 16G -v maclab-cache:/cache -e URL=https://github.com/o/r -v /usr/local/lab/io/b/in:/in -v /usr/local/lab/io/b/io:/io maclab-kbuild:1\n"
	for _, n := range kb {
		text += fmt.Sprintf("%d %s\n", n, vmProcess)
	}
	return text
}

const idle = "  4096 /sbin/launchd\n 20000 /usr/local/lab/current/bin/lab-builder --server http://127.0.0.1:7770 --cpus 10\n"

// watchedMini is a made-up mini with the Builder running at 10 cores.
func watchedMini(t *testing.T) *mini {
	t.Helper()
	need(t, "openssl", "jq")
	m := builderMini(t)
	k := newKey(t)
	m.ok("setup", "--release-key", k.signers)
	m.ok("setup", "--container-user", account(t))
	m.builderBuild("v0.2.0")
	m.ok("setup", "--builder-rootfs", rootfs(t), "--builder-token", tokenFile(t, madeUpToken+"\n", 0o600))
	if !exists(filepath.Join(filepath.Dir(m.root), "state", "loaded."+builderLabel)) {
		t.Fatal("the set-up did not start the Builder")
	}
	write(t, filepath.Join(m.tools, "vm_stat"), watchVMStat, 0o755)
	write(t, filepath.Join(m.tools, "pfctl"), watchPfctl, 0o755)
	write(t, filepath.Join(m.tree, "scripts", "mini", "page-times"), watchPageTimes, 0o755)
	m.set("ps", idle)
	m.env = append(m.env, "LAB_WATCH_TICK=0.2")
	os.Remove(m.log)
	write(t, m.log, "", 0o644)
	return m
}

func (m *mini) builderCores() string {
	f := regexp.MustCompile(`<string>--cpus</string><string>([0-9]+)</string>`).FindStringSubmatch(read(m.t, m.path("Library/LaunchDaemons/"+builderLabel+".plist")))
	if f == nil {
		m.t.Fatal("the Builder's installed file names no cores")
	}
	return f[1]
}

// until waits for what a running script is to do next.
func until(t *testing.T, what string, done func() bool) {
	t.Helper()
	for i := 0; i < 600; i++ {
		if done() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waited 30 s for %s", what)
}

func TestTheBuildWatchRefusesBeforeItChangesAnything(t *testing.T) {
	m := watchedMini(t)
	for _, bad := range [][]string{{}, {"--cpus", "7"}, {"--cpus", "10", "--builds", "0"}, {"--cpus", "10", "--every", "soon"}, {"--cpus", "10", "--wait", "0"}, {"--cpus", "10", "--force"}, {"--cold"}} {
		m.refused("build-watch", bad...)
	}
	// A build under way: the Builder is not the test's.
	m.set("ps", building("b1115-000009-ffff", "10", 5000))
	if out := m.refused("build-watch", "--cpus", "8", "--cold"); !strings.Contains(out, "a build is running on the Builder") {
		t.Errorf("with a build running it said:\n%s", out)
	}
	m.set("ps", idle)
	// container's server stopped.
	m.kill()
	if out := m.refused("build-watch", "--cpus", "8", "--cold"); !strings.Contains(out, "container's server does not answer") {
		t.Errorf("with container's server stopped it said:\n%s", out)
	}
	calls := m.calls()
	if m.builderCores() != "10" || strings.Contains(calls, "volume delete") || strings.Contains(calls, "bootstrap") || strings.Contains(calls, "system start") {
		t.Errorf("a refused watch changed something:\n%s", calls)
	}

	// A mini where the Builder was never set up.
	bare := builderMini(t)
	if out := bare.refused("build-watch", "--cpus", "10"); !strings.Contains(out, "the Builder is not set up on this machine") {
		t.Errorf("on a mini with no Builder it said:\n%s", out)
	}
}

// One run of the watch, as the gate test starts it for a run at 8 cores:
// the cores through the set-up, a cold kernel cache, then two builds
// watched, the second right behind the first.
func TestTheBuildWatchGivesTheCoresEmptiesTheCacheAndWatchesEachBuild(t *testing.T) {
	m := watchedMini(t)
	user := account(t)
	// container names the image of the one volume; the other's is found
	// where container keeps a volume's image, under the account's home.
	image := m.state("maclab-cache.img")
	write(t, image, strings.Repeat("kernel cache ", 200000), 0o644)
	home := ""
	if entry, err := exec.Command("id", "-P", user).Output(); err == nil && strings.Count(string(entry), ":") >= 9 {
		home = strings.Split(string(entry), ":")[8]
	}
	kept := m.path(home + "/Library/Application Support/com.apple.container/volumes/maclab-pkgcache/volume.img")
	write(t, kept, strings.Repeat("kernel cache ", 200000), 0o644)
	want, err := exec.Command("du", "-k", image).Output()
	if err != nil {
		t.Fatal(err)
	}
	used := strings.Fields(string(want))[0]
	if err := os.MkdirAll(m.path("var/lab/data/artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(m.tree, "scripts", "mini", "build-watch"), "--cpus", "8", "--cold", "--builds", "2", "--every", "0", "--wait", "60")
	cmd.Env = m.env
	cmd.Dir = filepath.Dir(m.tree)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	said := func(text string) func() bool {
		return func() bool {
			select {
			case err := <-done:
				done <- err
				return true
			default:
				return strings.Contains(m.calls(), text)
			}
		}
	}
	// It is ready once the kernel cache is made again; then the builds come.
	until(t, "the cold cache", said("volume create -s 40G maclab-cache"))
	time.Sleep(500 * time.Millisecond)
	m.set("ps", building("b1115-000001-abcd", "8", 5000000, 1000000))
	until(t, "the first page times", said("page-times"))
	m.set("ps", building("b1115-000001-abcd", "8", 7000000, 1500000))
	time.Sleep(600 * time.Millisecond)
	// One look that finds no VM ends no build: the most memory comes after it.
	m.set("ps.once", idle)
	until(t, "the look that finds no VM", func() bool { return !exists(filepath.Join(filepath.Dir(m.root), "state", "machine", "ps.once")) })
	m.set("ps", building("b1115-000001-abcd", "8", 9000000, 500000))
	time.Sleep(1000 * time.Millisecond)
	m.set("ps", building("b1115-000001-abcd", "8", 2000000))
	time.Sleep(600 * time.Millisecond)
	m.set("ps", building("b1115-000002-abcd", "8", 3000000))
	time.Sleep(1200 * time.Millisecond)
	m.set("ps", idle)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("build-watch: %v\n%s", err, out.String())
		}
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("build-watch did not end:\n%s", out.String())
	}
	text := out.String()

	// The cores: through the set-up, which starts the Builder alone again.
	calls := m.calls()
	if m.builderCores() != "8" || strings.Count(calls, "launchctl bootstrap") != 1 || !strings.Contains(calls, "bootstrap system "+m.path("Library/LaunchDaemons/"+builderLabel+".plist")) {
		t.Errorf("the Builder was not given 8 cores by the set-up, alone:\n%s", calls)
	}
	// The cold cache: the kernel's volume alone, as the account container runs under.
	wrapper := "sudo -n -u " + user + " " + m.path("usr/local/bin/container")
	del, made := strings.Index(calls, wrapper+" volume delete maclab-cache\n"), strings.LastIndex(calls, wrapper+" volume create -s 40G maclab-cache\n")
	if del < 0 || made < del || strings.Contains(calls, "delete maclab-pkgcache") {
		t.Errorf("the kernel cache was not deleted and made again, or more was:\n%s", calls)
	}
	if strings.Contains(calls, " run ") || strings.Contains(calls, "system start") {
		t.Errorf("the watch started something:\n%s", calls)
	}
	if !strings.Contains(calls, "as-lab "+filepath.Join(m.tree, "scripts", "mini", "page-times")) {
		t.Errorf("labd's page times were not read as the lab account:\n%s", calls)
	}

	var gate []string
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "@gate ") && !strings.HasPrefix(line, "@gate probe ") && !strings.HasPrefix(line, "@gate pf ") {
			gate = append(gate, regexp.MustCompile(`seconds=[0-9]+|probes=[0-9]+|used_kb=[0-9]+ budget`).ReplaceAllStringFunc(line, func(s string) string {
				return regexp.MustCompile(`[0-9]+`).ReplaceAllString(s, "N")
			}))
		}
	}
	wantGate := []string{
		"@gate ready cpus=8",
		"@gate build 1 begin id=b1115-000001-abcd cpus=8 memory=16G",
		"@gate build 1 end id=b1115-000001-abcd seconds=N vm_peak_kb=9500000 machine_peak_kb=20971520 probes=N",
		"@gate build 2 begin id=b1115-000002-abcd cpus=8 memory=16G",
		"@gate build 2 end id=b1115-000002-abcd seconds=N vm_peak_kb=3000000 machine_peak_kb=20971520 probes=N",
		"@gate volume maclab-cache used_kb=" + used + " size_kb=41943040",
		"@gate volume maclab-pkgcache used_kb=" + used + " size_kb=125829120",
		"@gate store artifacts used_kb=N budget_kb=20971520",
		"@gate done",
	}
	if strings.Join(gate, "\n") != strings.Join(wantGate, "\n") {
		t.Errorf("the watch said:\n%s\nwant:\n%s\n\nall of it:\n%s", strings.Join(gate, "\n"), strings.Join(wantGate, "\n"), text)
	}
	for _, want := range []string{
		"@gate pf $ pfctl -s info\n@gate pf Status: Enabled for 0 days 00:10:20           Debug: Urgent\n@gate pf $ pfctl -a com.apple.internet-sharing/shared_v4 -s nat\n@gate pf nat on en0 inet from 192.168.64.0/24 to any -> (en0:0) extfilter ei\n",
		"@gate probe 1 begin\n@gate probe 1 end probe_pages=13 probe_requests=260 probe_p95_ms=12 probe_max_ms=40\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the watch did not say:\n%s\nin:\n%s", want, text)
		}
	}
	if n := strings.Count(text, "@gate pf $ pfctl -s info"); n != 2 {
		t.Errorf("pf was read %d times, want once for each build", n)
	}
	if strings.Contains(text, "day_") || strings.Contains(text, madeUpToken) {
		t.Errorf("the watch said more than the gate test's lines:\n%s", text)
	}
	// Every reading that began ended before anything else was said.
	began := len(regexp.MustCompile(`(?m)^@gate probe [0-9]+ begin$`).FindAllString(text, -1))
	if whole := len(regexp.MustCompile(`@gate probe [0-9]+ begin\n@gate probe [0-9]+ end probe_pages=`).FindAllString(text, -1)); began < 2 || whole != began {
		t.Errorf("%d readings began and %d ended right after:\n%s", began, whole, text)
	}

	// At the cores it has, nothing of the set-up runs; with no build in
	// sight for --wait seconds it ends, saying so.
	os.Remove(m.log)
	late, code := m.run("build-watch", "--cpus", "8", "--builds", "1", "--wait", "1")
	if code == 0 || !strings.Contains(late, "@gate ready cpus=8") || !strings.Contains(late, "@gate failed no VM of the Builder was seen in 1 s") {
		t.Errorf("with no build coming: exit %d\n%s", code, late)
	}
	if calls := m.calls(); strings.Contains(calls, "bootstrap") || strings.Contains(calls, "volume delete") || strings.Contains(calls, "volume create") {
		t.Errorf("without --cold and at its cores the watch changed something:\n%s", calls)
	}

	// A set-up that leaves the Builder its cores: no build is waited for.
	real := read(t, filepath.Join(m.tree, "scripts", "mini", "setup"))
	write(t, filepath.Join(m.tree, "scripts", "mini", "setup"), "#!/bin/bash\necho 'setup: nothing to change'\n", 0o755)
	if out := m.refused("build-watch", "--cpus", "6", "--builds", "1", "--wait", "1"); !strings.Contains(out, "the Builder does not run with 6 cores a build after the set-up") || strings.Contains(out, "@gate ready") {
		t.Errorf("with a set-up that changed nothing it said:\n%s", out)
	}
	write(t, filepath.Join(m.tree, "scripts", "mini", "setup"), real, 0o755)

	// What lab-builder builds with is looked for too: its scratch folder
	// and its two images.
	kernel := m.builderRuns("--image")
	os.Remove(m.state("image." + kernel))
	if out := m.refused("build-watch", "--cpus", "8", "--builds", "1", "--wait", "1"); !strings.Contains(out, "the Builder's image "+kernel+" is not there") || strings.Contains(out, "@gate ready") {
		t.Errorf("with an image missing it said:\n%s", out)
	}
	m.touch("image." + kernel)
	// A Deploy whose builder/ is other, and no set-up since: the Builder
	// runs an image of the build before.
	plist := m.path("Library/LaunchDaemons/" + builderLabel + ".plist")
	installed := read(t, plist)
	write(t, plist, strings.Replace(installed, kernel, "maclab-kbuild:0123456789ab", 1), 0o644)
	if out := m.refused("build-watch", "--cpus", "8", "--builds", "1", "--wait", "1"); !strings.Contains(out, "the Builder runs maclab-kbuild:0123456789ab, not "+kernel) || strings.Contains(out, "@gate ready") {
		t.Errorf("with the Builder on an image of another build it said:\n%s", out)
	}
	write(t, plist, installed, 0o644)
	if err := os.Rename(m.path("usr/local/lab/io"), m.path("usr/local/lab/io.away")); err != nil {
		t.Fatal(err)
	}
	if out := m.refused("build-watch", "--cpus", "8", "--builds", "1", "--wait", "1"); !strings.Contains(out, "the Builder's scratch folder") || strings.Contains(out, "@gate ready") {
		t.Errorf("with no scratch folder it said:\n%s", out)
	}
	if err := os.Rename(m.path("usr/local/lab/io.away"), m.path("usr/local/lab/io")); err != nil {
		t.Fatal(err)
	}

	// What the test reads at its end is read before the first build: a
	// volume with no image to measure, and page times nobody gives, end the
	// watch before it is ready.
	os.Remove(kept)
	if out := m.refused("build-watch", "--cpus", "8", "--builds", "1", "--wait", "1"); !strings.Contains(out, "how full the volume maclab-pkgcache is cannot be read") || strings.Contains(out, "@gate ready") {
		t.Errorf("with a volume it cannot measure it said:\n%s", out)
	}
	write(t, kept, "packages", 0o644)
	write(t, filepath.Join(m.tree, "scripts", "mini", "page-times"), "#!/bin/bash\necho 'page-times: the caller log is not readable' >&2\nexit 1\n", 0o755)
	if out := m.refused("build-watch", "--cpus", "8", "--builds", "1", "--wait", "1"); !strings.Contains(out, "labd's page times cannot be read") || strings.Contains(out, "@gate ready") {
		t.Errorf("with no page times it said:\n%s", out)
	}
	write(t, filepath.Join(m.tree, "scripts", "mini", "page-times"), watchPageTimes, 0o755)

	// A cache that cannot be made again is said, and no build is waited for.
	m.touch("full")
	if out := m.refused("build-watch", "--cpus", "8", "--cold", "--builds", "1", "--wait", "1"); !strings.Contains(out, "did not make the volume maclab-cache again (40G)") || strings.Contains(out, "@gate ready") {
		t.Errorf("with no room for the cache it said:\n%s", out)
	}
}

// deploy/builder-cpus is where the gate test writes the cores down. The
// set-up gives the Builder the fewer of that and what it runs with: the
// cores go down by themselves and back up only by the option.
func TestSetupGivesTheBuilderTheFewerCoresOfDeployAndItsOwn(t *testing.T) {
	m := watchedMini(t)
	file := m.tree + "/deploy/builder-cpus"
	settled := func(n string) { write(t, file, "# The cores a build gets.\n"+n+"\n", 0o644) }
	if m.builderCores() != "10" {
		t.Fatalf("with no file the Builder got %s cores", m.builderCores())
	}

	settled("8")
	if out := m.ok("setup", "--dry-run"); m.builderCores() != "10" || !strings.Contains(out, "would install") {
		t.Fatalf("the dry run:\n%s", out)
	}
	os.Remove(m.log)
	m.ok("setup")
	if m.builderCores() != "8" || strings.Count(m.calls(), "launchctl bootstrap") != 1 {
		t.Fatalf("deploy/ says 8 and the Builder has %s:\n%s", m.builderCores(), m.calls())
	}
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("a second run:\n%s", out)
	}

	// deploy/ still says 10 while the gate test has stepped the Builder down.
	settled("10")
	if out := m.ok("setup"); m.builderCores() != "8" || !strings.Contains(out, "nothing to change") {
		t.Errorf("the set-up gave the Builder more cores by itself (%s):\n%s", m.builderCores(), out)
	}
	m.ok("setup", "--builder-cpus", "10")
	if m.builderCores() != "10" {
		t.Errorf("--builder-cpus 10 left %s", m.builderCores())
	}
	settled("6")
	m.ok("setup")
	if m.builderCores() != "6" {
		t.Errorf("deploy/ says 6 and the Builder has %s", m.builderCores())
	}
	m.ok("setup", "--builder-cpus", "8")
	if m.builderCores() != "8" {
		t.Errorf("--builder-cpus 8 with 6 in deploy/ left %s", m.builderCores())
	}

	for _, bad := range []string{"7", "12", "ten", "0"} {
		settled(bad)
		if out := m.refused("setup"); !strings.Contains(out, "deploy/builder-cpus says "+bad) || m.builderCores() != "8" {
			t.Errorf("deploy/builder-cpus saying %s: Builder at %s\n%s", bad, m.builderCores(), out)
		}
	}

	// The file as the repository has it is one the set-up takes.
	write(t, file, read(t, filepath.Join(repo(t), "deploy", "builder-cpus")), 0o644)
	m.ok("setup")
}

// A mini set up for the first time takes the cores deploy/ says.
func TestTheFirstSetupTakesTheCoresDeploySays(t *testing.T) {
	need(t, "openssl")
	m := builderMini(t)
	k := newKey(t)
	write(t, m.tree+"/deploy/builder-cpus", "8\n", 0o644)
	m.ok("setup", "--release-key", k.signers)
	m.ok("setup", "--container-user", account(t))
	if m.builderCores() != "8" {
		t.Errorf("the first set-up gave the Builder %s cores, and deploy/ says 8", m.builderCores())
	}
}
