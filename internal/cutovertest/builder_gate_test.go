package cutovertest

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The Builder's gate test (scripts/cutover/builder-gate) with everything
// outside the two scripts made up: lab asks for builds nobody compiles,
// which end with the times and results a test writes down; ssh runs the
// mini's own script, scripts/mini/build-watch, in a folder that stands in
// for the mini, where the process list, pf, the volumes and labd's page
// times are what the test says. No container, no kernel, no mini.

// lab: a build is a file the made-up process list shows as the Builder's VM
// for a while. A build with a transfer ends once an upload has finished
// after it began; outcome.<cores>.<n> names another end for the nth build
// of a run at that many cores.
const gateLabStub = `#!/bin/bash
echo "lab $*" >> "$GATE/calls"
cores() { sed -n 's/.*<string>--cpus<\/string><string>\([0-9]*\)<.*/\1/p' "$MINI_ROOT/Library/LaunchDaemons/org.omarchy-mac.lab.builder.plist"; }
case "$1" in
  status) exit 0 ;;
  builds)
    echo "BUILD              STATE    SOURCE  COMMIT  RELEASE  TIME"
    cat "$GATE"/row.* 2>/dev/null
    exit 0 ;;
  build | package)
    [[ ! -f $GATE/refuse ]] || { echo "lab: $(cat "$GATE/refuse")" >&2; exit 1; }
    n=$(( $(cat "$GATE/count") + 1 )); echo "$n" > "$GATE/count"
    step=$(( $(cat "$GATE/step") + 1 )); echo "$step" > "$GATE/step"
    id="$(printf 'b1115-%06d-abcd' "$n")"
    outcome="$(cat "$GATE/outcome.$(cores).$step" 2>/dev/null || echo done)"
    took=(0 31m12s 4m3s 11m40s)
    echo "$id  queued  src" > "$GATE/row.$id"
    # meta: who asked and what, as labd lists a build: asked_by, kind,
    # device, config_name, recipe_dir, source.input.
    if [[ $1 == package ]]; then
      meta="owner|package|||$(cd "$2" && pwd)|"
    else
      dev="" cfg="" prev=""
      for a in "$@"; do [[ $prev != --device ]] || dev="$a"; [[ $prev != --config ]] || cfg="$(basename "$a")"; prev="$a"; done
      meta="owner||$dev|$cfg||$2"
    fi
    echo "$meta" > "$GATE/meta.$id"
    # another.build: another Maintainer asks for the same build meanwhile.
    if [[ -f $GATE/another.build ]]; then
      echo "b1115-999999-other  queued  src" > "$GATE/row.b1115-999999-other"
      echo "maint|${meta#*|}" > "$GATE/meta.b1115-999999-other"
    fi
    # another.mine: the owner asks for the same build by hand meanwhile;
    # another.source: the same, of another kernel source.
    if [[ -f $GATE/another.mine ]]; then
      echo "b1115-999998-mine  queued  src" > "$GATE/row.b1115-999998-mine"
      echo "$meta" > "$GATE/meta.b1115-999998-mine"
    fi
    if [[ -f $GATE/another.source ]]; then
      echo "b1115-999997-src  queued  src" > "$GATE/row.b1115-999997-src"
      echo "${meta%|*}|https://github.com/o/linux-aurora/tree/other" > "$GATE/meta.b1115-999997-src"
    fi
    # The build goes on as labd's would, after lab has gone; a canceled
    # build ends at once.
    (
      canceled() { grep -q '  canceled  ' "$GATE/row.$id" 2>/dev/null; }
      sleep 0.3
      canceled && exit 0
      uploads="$(wc -l < "$GATE/uploads")"
      echo "$id" > "$GATE/vm"
      echo "$id  running: build 40%  src" > "$GATE/row.$id"
      case "$outcome" in
        "done")
          if [[ $step != 2 ]]; then
            for i in $(seq 300); do canceled && exit 0; [[ "$(wc -l < "$GATE/uploads")" -gt $uploads ]] && break; sleep 0.2; done
          fi
          sleep 1.2
          rm -f "$GATE/vm"
          echo "$id  done  src  0123456789ab  7.2.11-aurora  ${took[$step]}" > "$GATE/row.$id" ;;
        *)
          sleep 1.2
          rm -f "$GATE/vm"
          {
            case "$outcome" in
              stale) ;;
              fetch) echo "==> fetch https://github.com/o/linux-aurora 0123" ;;
              config) printf '==> fetch https://github.com/o/linux-aurora 0123\n==> checkout\n==> configure\n' ;;
              *) printf '==> fetch https://github.com/o/linux-aurora 0123\n==> checkout\n==> configure\n==> build (10 cpus)\n' ;;
            esac
            cat "$GATE/says.$outcome" 2>/dev/null || echo 'make: *** Error 2'
          } > "$GATE/log.$id"
          echo "$id  failed  src" > "$GATE/row.$id" ;;
      esac
    ) > /dev/null 2>&1 &
    # answer.lost: labd took the build, and its answer never reached lab.
    [[ ! -f $GATE/answer.lost ]] || { echo "lab: the lab did not answer" >&2; exit 1; }
    echo "build $id: o/linux-aurora @ 0123456789ab (queued)" ;;
  upload)
    [[ ! -f $GATE/upload.fails ]] || { echo "lab: the lab answered 502" >&2; exit 1; }
    # The whole file is hashed before the first part goes.
    sleep "${GATE_HASH_SECONDS:-0}"
    [[ ${MACLAB_UPLOAD_PROGRESS:-} != 1 ]] || echo "lab: labd holds 1 of 2 parts" >&2
    sleep "${GATE_UPLOAD_SECONDS:-4}"
    shasum -a 256 "$2" | cut -d' ' -f1
    echo up >> "$GATE/uploads" ;;
  build-log) cat "$GATE/log.$2" ;;
  *) echo "lab $1: not expected" >&2; exit 64 ;;
esac
`

// ssh: the owner reaching the mini. sudo's prompt comes first, on the line
// of whatever the mini says next.
const gateSSHStub = `#!/bin/bash
echo "ssh $*" >> "$GATE/calls"
[[ "$1 $2 $3 $4 $5 $6" == "-t -o ServerAliveInterval=30 owner@mini sudo /usr/local/lab/current/scripts/mini/build-watch" ]] || { echo "ssh: not expected: $*" >&2; exit 64; }
shift 6
echo 0 > "$GATE/step"
echo $$ > "$GATE/watch.pid"
printf 'Password:'
export LAB_SYSROOT="$MINI_ROOT" LAB_SYSTEM_TOOLS="$GATE/machine" LAB_WATCH_TICK=0.2
exec "$MINI_ROOT/usr/local/lab/current/scripts/mini/build-watch" "$@"
`

const gatePsStub = `#!/bin/bash
[[ -f $GATE/vm ]] || { echo "  4096 /sbin/launchd"; exit 0; }
cores="$(cat "$GATE/vm.cores" 2>/dev/null || sed -n 's/.*<string>--cpus<\/string><string>\([0-9]*\)<.*/\1/p' "$LAB_SYSROOT/Library/LaunchDaemons/org.omarchy-mac.lab.builder.plist")"
echo "  4096 /sbin/launchd"
echo "  9000 /usr/local/bin/container run --rm --name maclab-$(cat "$GATE/vm") --cpus $cores --memory 16G -v maclab-cache:/cache -e URL=https://github.com/o/r -v /usr/local/lab/io/b1/in:/in -v /usr/local/lab/io/b1/io:/io maclab-kbuild:1"
echo "$(cat "$GATE/vm_kb") /System/Library/Frameworks/Virtualization.framework/Versions/A/XPCServices/com.apple.Virtualization.VirtualMachine.xpc/Contents/MacOS/com.apple.Virtualization.VirtualMachine"
`

const gateVMStatStub = `#!/bin/bash
cat <<'END'
Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               100000.
Pages active:                            1000000.
Pages inactive:                           500000.
Pages wired down:                         300000.
Pages occupied by compressor:              10720.
END
`

const gatePfctlStub = `#!/bin/bash
echo "pfctl $*" >> "$GATE/mini-calls"
[[ ! -f $GATE/watch.dies ]] || kill "$(cat "$GATE/watch.pid")"
case "$*" in
  "-s info") echo "Status: Enabled for 0 days 00:10:20           Debug: Urgent" ;;
  *) echo "nat on en0 inet from 192.168.64.0/24 to any -> (en0:0) extfilter ei" ;;
esac
`

// The mini's other scripts, as build-watch calls them.
const gateContainerStub = `#!/bin/bash
echo "container $*" >> "$GATE/mini-calls"
case "$1" in
  status) exit 0 ;;
  as)
    shift 2
    case "$1 $2" in
      "volume delete" | "volume create") [[ ! -f $GATE/volume.fails ]] ;;
      "volume inspect") printf '[{"name":"%s","source":"%s"}]\n' "$3" "$GATE/$3.img" ;;
      "image inspect") [[ ! -f $GATE/no.${3%%:*} ]] ;;
      *) exit 64 ;;
    esac ;;
  *) exit 64 ;;
esac
`

const gateSetupStub = `#!/bin/bash
echo "setup $*" >> "$GATE/mini-calls"
[[ $1 == --builder-cpus ]] || exit 64
plist="$LAB_SYSROOT/Library/LaunchDaemons/org.omarchy-mac.lab.builder.plist"
sed "s|<string>--cpus</string><string>[0-9]*</string>|<string>--cpus</string><string>$2</string>|" "$plist" > "$plist.new"
mv "$plist.new" "$plist"
`

const gatePageTimesStub = `#!/bin/bash
# pages.broken: never readable. pages.break: readable once, before the
# builds, and never again. silent.<cores>: every third reading at that many
# cores gives nothing.
[[ ! -f $GATE/pages.broken ]] || { echo "page-times: the caller log is not readable" >&2; exit 1; }
if [[ -f $GATE/pages.break ]]; then
  [[ ! -f $GATE/pages.read ]] || exit 1
  touch "$GATE/pages.read"
fi
cores="$(sed -n 's/.*<string>--cpus<\/string><string>\([0-9]*\)<.*/\1/p' "$LAB_SYSROOT/Library/LaunchDaemons/org.omarchy-mac.lab.builder.plist")"
p95="$(cat "$GATE/p95.$cores" 2>/dev/null || echo 12)"
# failing.<cores>: a page fails at every reading after the first: page-times
# says no 95th percentile then.
if [[ -f $GATE/failing.$cores ]]; then
  if [[ -f $GATE/failing.seen ]]; then
    printf 'probe_pages=12\nprobe_requests=240\nprobe_failed=1\nday_requests=9000\nday_p95_ms=20\n'
    exit 0
  fi
  touch "$GATE/failing.seen"
fi
if [[ -f $GATE/silent.$cores ]]; then
  n=$(( $(cat "$GATE/readings" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$GATE/readings"
  (( n % 3 )) || exit 1
fi
printf 'probe_pages=13\nprobe_requests=260\nprobe_p95_ms=%s\nprobe_max_ms=%s\nday_requests=9000\nday_p95_ms=20\n' "$p95" "$((p95 + 31))"
`

// hardening-probe: under the saved login it cancels a build as labd does.
const gateProbeStub = `#!/bin/bash
echo "probe $*" >> "$GATE/calls"
case "$*" in
  "ask --out "*" GET /api/auth") echo '{"trusted":true,"login":"owner","kind":"Admin"}' > "$3"; echo status=200 ;;
  "ask --rows id,state,asked_by,kind,device,config_name,recipe_dir"*" GET /api/builds?limit=100")
    echo status=200
    for f in "$GATE"/row.*; do
      [ -f "$f" ] || continue
      read -r id state _ < "$f"
      IFS='|' read -r by kind dev cfg rec src < "$GATE/meta.$id"
      echo "row=$id|${state%:}|$by|$kind|$dev|$cfg|$rec|$src"
    done ;;
  "ask POST /api/builds/"*"/cancel")
    id="${3#/api/builds/}"; id="${id%/cancel}"
    echo "$id  canceled  src" > "$GATE/row.$id"
    echo status=200 ;;
  *) echo status=500 ;;
esac
`

const gateDuStub = `#!/bin/bash
case "$1" in
  -k) echo "$(cat "$GATE/used.$(basename "$2")")	$2" ;;
  *) echo "3145728	$2" ;;
esac
`

// gate is one made-up lab and mini for a run of the gate test.
type gate struct {
	t       *testing.T
	state   string
	deploy  string
	records string
	recipe  string
	bin     string
	env     []string
}

func (g *gate) set(name, text string) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.state, name), []byte(text), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *gate) calls(file string) string {
	b, _ := os.ReadFile(filepath.Join(g.state, file))
	return string(b)
}

func (g *gate) cores() string {
	m := regexp.MustCompile(`(?m)^([0-9]+)$`).FindStringSubmatch(read(g.t, filepath.Join(g.deploy, "builder-cpus")))
	if m == nil {
		g.t.Fatal("deploy/builder-cpus holds no number")
	}
	return m[1]
}

func (g *gate) record() string {
	g.t.Helper()
	files, _ := filepath.Glob(filepath.Join(g.records, "*-builder-gate.md"))
	if len(files) != 1 {
		g.t.Fatalf("want one record, found %v", files)
	}
	return read(g.t, files[0])
}

func (g *gate) results() string {
	b, _ := os.ReadFile(filepath.Join(g.records, "builder-gate.results"))
	return strings.TrimSpace(string(b))
}

// run is the owner's one command.
func (g *gate) run(args ...string) (string, int) {
	g.t.Helper()
	all := append([]string{"--lab", "--mini", "owner@mini", "--kernel", "https://github.com/o/linux-aurora/tree/aurora", "--recipe", g.recipe,
		"--size", "65536", "--deploy", g.deploy, "--records", g.records, "--bin", g.bin}, args...)
	cmd := exec.Command(cutover(g.t, "builder-gate"), all...)
	cmd.Env = append(os.Environ(), g.env...)
	cmd.Stdin = strings.NewReader("")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.CombinedOutput()
	if cmd.Process != nil {
		endGroup(g.t, cmd.Process.Pid)
	}
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	g.t.Fatalf("builder-gate: %v\n%s", err, out)
	return "", 0
}

// endGroup ends what a run left going in its process group. A build the
// made-up lab took goes on after a run that ended first, as labd's would,
// and it writes in the test's folder: left, it races the folder's removal.
func endGroup(t *testing.T, pgid int) {
	t.Helper()
	syscall.Kill(-pgid, syscall.SIGKILL)
	for end := time.Now().Add(10 * time.Second); syscall.Kill(-pgid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			t.Errorf("processes of the run in group %d outlived it", pgid)
			return
		}
	}
}

func newGate(t *testing.T) *gate {
	t.Helper()
	if testing.Short() {
		t.Skip("runs made-up builds for some seconds each")
	}
	for _, tool := range []string{"jq", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on this machine", tool)
		}
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	g := &gate{t: t, state: filepath.Join(root, "state"), deploy: filepath.Join(root, "deploy"), records: filepath.Join(root, "records"), recipe: filepath.Join(root, "recipes", "linux-aurora")}
	mini := filepath.Join(root, "mini")
	scripts := filepath.Join(mini, "usr/local/lab/current/scripts/mini")
	stubs := filepath.Join(root, "stubs")
	g.bin = stubs
	files := map[string]string{
		filepath.Join(stubs, "lab"):                                                       gateLabStub,
		filepath.Join(stubs, "ssh"):                                                       gateSSHStub,
		filepath.Join(stubs, "du"):                                                        gateDuStub,
		filepath.Join(stubs, "hardening-probe"):                                           gateProbeStub,
		filepath.Join(stubs, "launchctl"):                                                 "#!/bin/bash\nexit 0\n",
		filepath.Join(g.state, "machine/ps"):                                              gatePsStub,
		filepath.Join(g.state, "machine/vm_stat"):                                         gateVMStatStub,
		filepath.Join(g.state, "machine/pfctl"):                                           gatePfctlStub,
		filepath.Join(scripts, "container"):                                               gateContainerStub,
		filepath.Join(scripts, "setup"):                                                   gateSetupStub,
		filepath.Join(scripts, "page-times"):                                              gatePageTimesStub,
		filepath.Join(scripts, "build-watch"):                                             read(t, filepath.Join(repo(t), "scripts/mini/build-watch")),
		filepath.Join(scripts, "lib.sh"):                                                  read(t, filepath.Join(repo(t), "scripts/mini/lib.sh")),
		filepath.Join(g.deploy, "builder-cpus"):                                           read(t, filepath.Join(repo(t), "deploy/builder-cpus")),
		filepath.Join(g.recipe, "PKGBUILD"):                                               "pkgname=linux-aurora\n",
		filepath.Join(mini, "usr/local/lab/container-user"):                               me.Username + "\n",
		filepath.Join(mini, "usr/local/lab/current/builder/Containerfile"):                "FROM scratch\n",
		filepath.Join(mini, "usr/local/lab/current/builder/pkg/Containerfile"):            "FROM scratch\n",
		filepath.Join(mini, "usr/local/lab/pkg-image/ArchLinuxARM-aarch64-latest.tar.gz"): "\x1f\x8b a made-up root file system\n",
		filepath.Join(mini, "var/lab/data/artifacts/keep"):                                "",
		filepath.Join(mini, "usr/local/lab/io/keep"):                                      "",
	}
	for path, text := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(g.records, 0o755); err != nil {
		t.Fatal(err)
	}
	// The Builder runs the images the set-up named from the Deployed build.
	named := exec.Command("bash", "-c", `. "$1"; builder_image maclab-kbuild builder "$LAB_CURRENT"; builder_image maclab-pkgbuild builder/pkg "$LAB_CURRENT"`, "images", filepath.Join(scripts, "lib.sh"))
	named.Env = append(os.Environ(), "LAB_SYSROOT="+mini)
	images, err := named.Output()
	tags := strings.Fields(string(images))
	if err != nil || len(tags) != 2 {
		t.Fatalf("the Deployed build's images: %v %q", err, images)
	}
	plist := "    <string>--cpus</string><string>10</string>\n    <string>--image</string><string>" + tags[0] + "</string>\n    <string>--pkg-image</string><string>" + tags[1] + "</string>\n"
	if err := os.MkdirAll(filepath.Join(mini, "Library/LaunchDaemons"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mini, "Library/LaunchDaemons/org.omarchy-mac.lab.builder.plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"count": "0\n", "step": "0\n", "uploads": "", "calls": "", "mini-calls": "",
		"vm_kb": "9437184\n", "maclab-cache.img": "", "maclab-pkgcache.img": "",
		"used.maclab-cache.img": "20971520\n", "used.maclab-pkgcache.img": "36700160\n",
	} {
		g.set(name, text)
	}
	g.env = []string{
		"PATH=" + stubs + ":" + os.Getenv("PATH"), "GATE=" + g.state, "MINI_ROOT=" + mini,
		"LAB_ADDRESS=http://lab.test", "GATE_POLL=0.2", "GATE_PROBE_EVERY=1", "GATE_READY_WAIT=60",
	}
	return g
}

const watchCall = "ssh -t -o ServerAliveInterval=30 owner@mini sudo /usr/local/lab/current/scripts/mini/build-watch --cpus %s --cold --builds 3 --every 1\n"

func wants(t *testing.T, what, text string, all ...string) {
	t.Helper()
	for _, want := range all {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %q:\n%s", what, want, text)
		}
	}
}

// At 10 cores everything holds: one run, one password, and the record
// carries the times, the peak memory and the volumes' use.
func TestTheBuildersGateTestPassesAtTenCoresAndRecordsItsNumbers(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	out, code := g.run()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	record := g.record()
	wants(t, "the record", record,
		"builds asked of labd, built by lab-builder on the mini",
		"the kernel is built from https://github.com/o/linux-aurora/tree/aurora with m1-pro's running config; the packages from the recipe linux-aurora",
		"At 10 cores a build:",
		"the cold kernel build (build b1115-000001-abcd): done in ", "31m12s by labd's",
		"the warm rebuild (build b1115-000002-abcd): done in ", "4m3s by labd's",
		"the package build (build b1115-000003-abcd): done in ", "11m40s by labd's",
		"with 10 cores and 16G; the VM's process held 9.0 GB at most, and the whole mini had 20.0 GB in use at most",
		"labd's pages during the cold kernel build: 12 ms at the 95th percentile at the slowest of ",
		"the slowest request 43 ms; with 65536 bytes going up at ",
		"labd's pages during the warm rebuild: 12 ms", "(told, not judged)",
		"labd's pages during the package build: 12 ms",
		"the volume maclab-cache holds 20.0 GB of its 40.0 GB",
		"the volume maclab-pkgcache holds 35.0 GB of its 120.0 GB",
		"the artifact store holds 3.0 GB; its budget is 20.0 GB",
		"10 cores a build: labd's pages took 12 ms at the 95th percentile while the Builder's gate test built, under 100",
	)
	if !strings.HasSuffix(record, "\nResult: pass\n") {
		t.Errorf("the record does not end in its result:\n%s", record)
	}
	me, _ := user.Current()
	if strings.Contains(record, g.recipe) || strings.Contains(record, filepath.Dir(g.deploy)) || strings.Contains(record, "/"+me.Username+"/") || strings.Contains(record, "192.168.") {
		t.Errorf("the record names a folder of whoever ran it, or an address:\n%s", record)
	}
	if !regexp.MustCompile(`^builder-gate pass 20\d\d-\d\d-\d\d 20\d\d-\d\d-\d\d-builder-gate\.md 10$`).MatchString(g.results()) {
		t.Errorf("the line the Ready list reads: %q", g.results())
	}
	if g.cores() != "10" {
		t.Errorf("deploy/builder-cpus says %s", g.cores())
	}

	calls := g.calls("calls")
	if strings.Count(calls, "ssh ") != 1 || !strings.Contains(calls, fmt.Sprintf(watchCall, "10")) {
		t.Errorf("the mini was reached otherwise than by one watch at 10 cores:\n%s", calls)
	}
	kernel := "lab build https://github.com/o/linux-aurora/tree/aurora --device m1-pro --force --no-wait\n"
	if strings.Count(calls, kernel) != 2 || strings.Count(calls, "lab package "+g.recipe+" --force --no-wait\n") != 1 || strings.Count(calls, "lab upload ") < 2 {
		t.Errorf("not two kernel builds, one package build and a transfer during two of them:\n%s", calls)
	}
	if strings.Index(calls, "ssh ") > strings.Index(calls, "lab build ") {
		t.Errorf("a build was asked for before the mini watched:\n%s", calls)
	}
	mini := g.calls("mini-calls")
	wants(t, "the mini", mini, " volume delete maclab-cache\n", " volume create -s 40G maclab-cache\n", "pfctl -s info\n", "pfctl -a com.apple.internet-sharing/shared_v4 -s nat\n")
	if strings.Contains(mini, "setup") || strings.Count(mini, "volume delete") != 1 || strings.Contains(mini, "maclab-pkgcache\n") && strings.Contains(mini, "delete maclab-pkgcache") {
		t.Errorf("the mini changed more than the kernel cache:\n%s", mini)
	}
	if files, _ := filepath.Glob(filepath.Join(g.records, "*network*")); len(files) != 0 {
		t.Errorf("pf's state was saved with no network failure: %v", files)
	}

	if out, code := run(t, nil, "", cutover(t, "builder-gate"), "--results", "--records", g.records); code != 0 || !strings.Contains(out, "pass on ") || !strings.Contains(out, "at 10 cores a build") {
		t.Errorf("--results: exit %d\n%s", code, out)
	}
}

// The page target missed at 10 cores: the same again at 8, where it holds,
// and 8 is what deploy/ says from then on.
func TestAMissedPageTargetDropsTheBuilderToEightCores(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	g.set("p95.10", "140\n")
	g.set("p95.8", "90\n")
	out, code := g.run()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if g.cores() != "8" {
		t.Errorf("deploy/builder-cpus says %s, want 8", g.cores())
	}
	record := g.record()
	wants(t, "the record", record,
		"At 10 cores a build:", "labd's pages during the cold kernel build: 140 ms",
		"the Builder's gate test missed at 10 (140 ms). It runs again at 8 cores a build.",
		"At 8 cores a build:", "with 8 cores and 16G", "labd's pages during the package build: 90 ms",
		"8 cores a build: labd's pages took 90 ms", "Missed with more: 10 (140 ms)", "written in deploy/builder-cpus. Commit it", "Result: pass")
	if strings.Contains(record, filepath.Dir(g.deploy)) {
		t.Errorf("the record names the folder of whoever ran it:\n%s", record)
	}
	if !strings.HasSuffix(g.results(), "-builder-gate.md 8") {
		t.Errorf("the line the Ready list reads: %q", g.results())
	}
	calls := g.calls("calls")
	if strings.Count(calls, "ssh ") != 2 || !strings.Contains(calls, fmt.Sprintf(watchCall, "10")) || !strings.Contains(calls, fmt.Sprintf(watchCall, "8")) {
		t.Errorf("want one watch at 10 and one at 8:\n%s", calls)
	}
	mini := g.calls("mini-calls")
	if strings.Count(mini, "setup --builder-cpus 8\n") != 1 || strings.Count(mini, "setup") != 1 || strings.Count(mini, "volume delete maclab-cache") != 2 {
		t.Errorf("the mini was not given 8 cores once, with a cold cache for each run:\n%s", mini)
	}
	if strings.Count(calls, "lab build ") != 4 || strings.Count(calls, "lab package ") != 2 {
		t.Errorf("each run is three builds:\n%s", calls)
	}
}

// Missed at 10, at 8 and at 6: it stops for the owner and writes nothing.
func TestMissedAtSixCoresTheGateTestStopsForTheOwner(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	g.set("p95.10", "140\n")
	g.set("p95.8", "130\n")
	g.set("p95.6", "100\n")
	out, code := g.run()
	if code != 3 {
		t.Fatalf("exit %d, want 3\n%s", code, out)
	}
	if g.cores() != "10" {
		t.Errorf("a stop wrote %s in deploy/builder-cpus", g.cores())
	}
	record := g.record()
	wants(t, "the record", record, "At 10 cores a build:", "At 8 cores a build:", "At 6 cores a build:",
		"STOPPED: the Builder's gate test did not pass at 10 (140 ms), 8 (130 ms), 6 (100 ms)", "for the owner to decide", "Result: stopped")
	if !strings.HasPrefix(g.results(), "builder-gate stopped ") || !strings.HasSuffix(g.results(), " -") {
		t.Errorf("the line the Ready list reads: %q", g.results())
	}
	if n := strings.Count(g.calls("calls"), "ssh "); n != 3 {
		t.Errorf("%d watches, want 3", n)
	}
	if out, code := run(t, nil, "", cutover(t, "builder-gate"), "--results", "--records", g.records); code == 0 || !strings.Contains(out, "stopped on ") {
		t.Errorf("--results after a stop: exit %d\n%s", code, out)
	}
}

// A build that breaks while compiling is a failed run too, and the run's
// other builds are not asked for: the same again at 8 cores.
func TestABuildThatBreaksWhileCompilingDropsTheCoresToo(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		outcome, says string
		kernels       int
	}{
		"the package build":     {"outcome.10.3", "the package build (build b1115-000003-abcd): failed after ", 4},
		"the cold kernel build": {"outcome.10.1", "the cold kernel build (build b1115-000001-abcd): failed after ", 3},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGate(t)
			g.set(c.outcome, "oom\n")
			g.set("says.oom", "clang: error: unable to execute command: Killed\n")
			out, code := g.run()
			if code != 0 || g.cores() != "8" {
				t.Fatalf("exit %d, deploy/builder-cpus %s\n%s", code, g.cores(), out)
			}
			wants(t, "the record", g.record(), c.says, "in its build stage",
				"missed at 10 (failed: see the record). It runs again at 8 cores a build.", "At 8 cores a build:", "Result: pass")
			if calls := g.calls("calls"); strings.Count(calls, "lab build ") != c.kernels || strings.Count(calls, "lab package ") != c.kernels/2 {
				t.Errorf("after a failed build the run's other builds were asked for, or the next run's were not:\n%s", calls)
			}
			if files, _ := filepath.Glob(filepath.Join(g.records, "*network*")); len(files) != 0 {
				t.Errorf("a build that failed while compiling was taken for a network failure: %v", files)
			}
		})
	}
}

// labd answering no page at all now and then while a build runs misses the
// target as a slow page does.
func TestPagesThatDoNotAnswerMissTheTarget(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	g.set("silent.10", "")
	out, code := g.run()
	if code != 0 || g.cores() != "8" {
		t.Fatalf("exit %d, deploy/builder-cpus %s\n%s", code, g.cores(), out)
	}
	wants(t, "the record", g.record(), "of those readings gave no time at all: labd did not answer its pages. That misses the target",
		"missed at 10 (failed: see the record). It runs again at 8 cores a build.", "At 8 cores a build:", "Result: pass")
}

// A page that fails is no fast page: with one failing at every reading the
// target is missed, and the run goes again at 8 cores.
func TestAFailingPageFailsTheGate(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	g.set("failing.10", "")
	out, code := g.run()
	if code != 0 || g.cores() != "8" {
		t.Fatalf("exit %d, deploy/builder-cpus %s\n%s", code, g.cores(), out)
	}
	wants(t, "the record", g.record(), "labd did not answer its pages. That misses the target",
		"missed at 10 (failed: see the record). It runs again at 8 cores a build.", "At 8 cores a build:", "Result: pass")
}

// A gate that ends while its build is queued or running cancels it: a
// failure, or the owner stopping it.
func TestAGateThatEndsFirstLeavesNoBuild(t *testing.T) {
	t.Parallel()
	t.Run("a failure", func(t *testing.T) {
		t.Parallel()
		g := newGate(t)
		g.set("watch.dies", "")
		out, code := g.run()
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if !strings.Contains(g.calls("calls"), "probe ask POST /api/builds/b1115-000001-abcd/cancel\n") {
			t.Errorf("the build was left:\n%s", g.calls("calls"))
		}
		record := g.record()
		if i := strings.Index(record, "build b1115-000001-abcd was canceled"); i < 0 || i > strings.Index(record, "Result: fail") {
			t.Errorf("the record does not say the build was canceled before its result:\n%s", record)
		}
	})
	// The cancel is armed before the build is asked for: a build labd took
	// whose answer was lost is the run's too.
	t.Run("an answer lost", func(t *testing.T) {
		t.Parallel()
		g := newGate(t)
		g.set("answer.lost", "")
		out, code := g.run()
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if !strings.Contains(g.calls("calls"), "probe ask POST /api/builds/b1115-000001-abcd/cancel\n") {
			t.Errorf("the build was left:\n%s", g.calls("calls"))
		}
		record := g.record()
		if i := strings.Index(record, "build b1115-000001-abcd was canceled"); i < 0 || i > strings.Index(record, "Result: fail") {
			t.Errorf("the record does not say the build was canceled before its result:\n%s", record)
		}
	})
	// With no id read, the run cancels only a build of its own: asked under
	// its login, for what it asked.
	t.Run("an answer lost beside another Maintainer's build", func(t *testing.T) {
		t.Parallel()
		g := newGate(t)
		g.set("answer.lost", "")
		g.set("another.build", "")
		out, code := g.run()
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		calls := g.calls("calls")
		if !strings.Contains(calls, "probe ask POST /api/builds/b1115-000001-abcd/cancel\n") {
			t.Errorf("the run's build was left:\n%s", calls)
		}
		if strings.Contains(calls, "b1115-999999-other/cancel") || strings.Contains(g.record(), "b1115-999999-other") {
			t.Errorf("another Maintainer's build was canceled:\n%s\n%s", calls, g.record())
		}
	})
	// Nor one the owner started by hand: of another kernel source it is not
	// the run's, and with two that match the run cannot tell which is its
	// own, so it cancels neither and names both.
	t.Run("an answer lost beside the owner's build of another kernel source", func(t *testing.T) {
		t.Parallel()
		g := newGate(t)
		g.set("answer.lost", "")
		g.set("another.source", "")
		out, code := g.run()
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		calls := g.calls("calls")
		if !strings.Contains(calls, "probe ask POST /api/builds/b1115-000001-abcd/cancel\n") {
			t.Errorf("the run's build was left:\n%s", calls)
		}
		if strings.Contains(calls, "b1115-999997-src/cancel") {
			t.Errorf("the owner's build of another kernel source was canceled:\n%s\n%s", calls, g.record())
		}
	})
	t.Run("an answer lost beside a second build that matches", func(t *testing.T) {
		t.Parallel()
		g := newGate(t)
		g.set("answer.lost", "")
		g.set("another.mine", "")
		out, code := g.run()
		if code != 1 {
			t.Fatalf("exit %d\n%s", code, out)
		}
		if calls := g.calls("calls"); strings.Contains(calls, "/cancel") {
			t.Errorf("a build was canceled though two matched:\n%s", calls)
		}
		wants(t, "the record", g.record(), "NOT CANCELED", "b1115-000001-abcd", "b1115-999998-mine", "Result: fail")
	})
	t.Run("stopped by the owner", func(t *testing.T) {
		t.Parallel()
		g := newGate(t)
		g.env = append(g.env, "GATE_UPLOAD_SECONDS=20")
		cmd := exec.Command(cutover(t, "builder-gate"), "--lab", "--mini", "owner@mini", "--kernel", "https://github.com/o/linux-aurora/tree/aurora", "--recipe", g.recipe,
			"--size", "65536", "--deploy", g.deploy, "--records", g.records, "--bin", g.bin)
		cmd.Env = append(os.Environ(), g.env...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for end := time.Now().Add(30 * time.Second); !strings.Contains(g.calls("calls"), "lab upload "); time.Sleep(50 * time.Millisecond) {
			if time.Now().After(end) {
				cmd.Process.Kill()
				t.Fatalf("no build began:\n%s", g.calls("calls"))
			}
		}
		// Ctrl-C at the owner's terminal: the whole group gets it.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		cmd.Wait()
		endGroup(t, cmd.Process.Pid)
		if !strings.Contains(g.calls("calls"), "probe ask POST /api/builds/b1115-000001-abcd/cancel\n") || !strings.Contains(g.record(), "build b1115-000001-abcd was canceled") {
			t.Errorf("the build was left:\n%s\n%s", g.calls("calls"), g.record())
		}
	})
}

// The transfer is in flight from its first part on, not while lab hashes the
// file before it: readings taken while it hashed prove nothing.
func TestTheTransferWindowStartsAtTheFirstPart(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	g.env = append(g.env, "GATE_HASH_SECONDS=4", "GATE_UPLOAD_SECONDS=0")
	out, code := g.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
	wants(t, "the record", g.record(), "FAILED: labd's page times were not read with a transfer in flight during the cold kernel build")
}

// pkgbuild says its build stage where compiling starts: makepkg checks the
// sources first, and a checksum that does not match is no build failure,
// which fewer cores would mend. The part of pkgbuild that runs makepkg, with
// makepkg and nproc made up.
func TestPkgbuildSaysTheBuildStageWhereCompilingStarts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repo(t), "builder", "pkg", "pkgbuild"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	cut := func(from, to string) string {
		i := strings.Index(text, from)
		j := strings.Index(text[max(i, 0):], to)
		if i < 0 || j < 0 {
			t.Fatalf("pkgbuild has no %q ... %q", from, to)
		}
		return text[i : i+j+len(to)]
	}
	defs := cut("step() {", "\n") + cut("fail() {", "\n")
	if strings.Contains(text, "\nstages() {") {
		defs += cut("\nstages() {", "\n}\n")
	}
	end := strings.Index(text, `fail "makepkg failed"`)
	begin := strings.LastIndex(text[:strings.Index(text, "start=$(date +%s)")], "\nstep ")
	if end < 0 || begin < 0 {
		t.Fatal("pkgbuild has no step before makepkg, or no check of its status")
	}
	part := text[begin : end+len(`fail "makepkg failed"`)]
	for name, c := range map[string]struct{ makepkg, stage string }{
		"a checksum that does not match": {"echo '==> Making package: linux-aurora 7.2-1'\necho '==> Validating source files with sha256sums...'\necho '    linux-7.2.tar.xz ... FAILED'\necho '==> ERROR: One or more files did not pass the validity check!'\nexit 1\n", "sources"},
		"a compiler that breaks":         {"echo '==> Making package: linux-aurora 7.2-1'\necho '==> Starting build()...'\necho 'make: *** Error 2'\nexit 2\n", "build"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "makepkg"), []byte("#!/bin/bash\n"+c.makepkg), 0o755)
			os.WriteFile(filepath.Join(dir, "nproc"), []byte("#!/bin/sh\necho 10\n"), 0o755)
			script := "set -euo pipefail\nC=" + dir + " W=" + dir + "\n" + defs + "\nas_build() { \"$@\"; }\n" + part + "\n"
			cmd := exec.Command("/bin/bash", "-c", script)
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin"}
			out, _ := cmd.CombinedOutput()
			// What the gate reads: the last "==> word" line of the build's log.
			m := regexp.MustCompile(`(?m)^==> ([a-z]+)( .*)?$`).FindAllStringSubmatch(string(out), -1)
			if len(m) == 0 || m[len(m)-1][1] != c.stage {
				t.Errorf("the last stage said is %v, want %s:\n%s", m, c.stage, out)
			}
			if !strings.Contains(string(out), "error: makepkg failed") {
				t.Errorf("makepkg's failure was not said:\n%s", out)
			}
		})
	}
}

// A build that reaches nothing past the mini, as container's VMs once did:
// fewer cores mend nothing, so the run stops at once, with what the mini
// read of pf while that VM was up saved beside the record.
func TestABuildThatCannotReachTheInternetStopsTheRunAndSavesWhatToRead(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ outcome, says, stage string }{
		"the kernel's fetch":  {"outcome.10.1", "fetch", "fetch"},
		"a package's sources": {"outcome.10.3", "curl", "build"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGate(t)
			g.set(c.outcome, c.says+"\n")
			g.set("says.fetch", "fatal: unable to access 'https://github.com/o/linux-aurora/': Failed to connect to github.com port 443 after 75002 ms\n")
			g.set("says.curl", "curl: (6) Could not resolve host: cdn.kernel.org\n==> ERROR: Failure while downloading linux-7.2.tar.xz\n")
			out, code := g.run()
			if code != 3 {
				t.Fatalf("exit %d, want 3\n%s", code, out)
			}
			record := g.record()
			wants(t, "the record", record, "could not reach the internet: it failed in its "+c.stage+" stage", "Fewer cores do not mend that",
				"-builder-gate-network.txt: read it before restarting anything on the mini",
				"sudo pfctl -a com.apple.internet-sharing/shared_v4 -s nat", "Result: stopped")
			files, _ := filepath.Glob(filepath.Join(g.records, "*-builder-gate-network.txt"))
			if len(files) != 1 {
				t.Fatalf("what the mini read was not saved: %v", files)
			}
			wants(t, "the saved file", read(t, files[0]), "$ pfctl -s info", "Status: Enabled for 0 days", "$ pfctl -a com.apple.internet-sharing/shared_v4 -s nat", "nat on en0 inet from 192.168.64.0/24 to any")
			if strings.Contains(read(t, files[0]), "@gate") || regexp.MustCompile(`(?m)^[0-9]{9,}`).MatchString(read(t, files[0])) {
				t.Errorf("the saved file carries the watch's own marks:\n%s", read(t, files[0]))
			}
			if g.cores() != "10" || strings.Count(g.calls("calls"), "ssh ") != 1 || strings.Contains(g.calls("mini-calls"), "setup") {
				t.Errorf("after a network failure the run went on: cores %s\n%s", g.cores(), g.calls("calls"))
			}
		})
	}
}

// A volume that did not stay inside its size fails the run at any number
// of cores.
func TestAVolumeOutsideItsSizeFailsTheRun(t *testing.T) {
	t.Parallel()
	g := newGate(t)
	g.set("used.maclab-cache.img", "41943040\n")
	out, code := g.run("--cores", "6")
	if code != 3 || g.cores() != "10" {
		t.Fatalf("exit %d, deploy/builder-cpus %s\n%s", code, g.cores(), out)
	}
	wants(t, "the record", g.record(), "At 6 cores a build:", "the volume maclab-cache holds 40.0 GB, and its size is 40.0 GB: it did not stay inside",
		"did not pass at 6 (failed: see the record)", "Result: stopped")
	if strings.Count(g.calls("mini-calls"), "setup --builder-cpus 6\n") != 1 {
		t.Errorf("--cores 6 did not give the Builder 6 cores:\n%s", g.calls("mini-calls"))
	}
}

// What cannot be measured is no pass and no reason for fewer cores: the run
// ends as failed and decides nothing.
func TestWhatCannotBeMeasuredDecidesNothing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		break_ func(*gate)
		says   string
	}{
		"labd's page times":   {func(g *gate) { g.set("pages.break", "") }, "labd's page times could not be read while the cold kernel build ran"},
		"no page times ever":  {func(g *gate) { g.set("pages.broken", "") }, "the mini did not get ready for the builds at 10 cores"},
		"a volume's use":      {func(g *gate) { os.Remove(filepath.Join(g.state, "maclab-pkgcache.img")) }, "the mini did not get ready for the builds at 10 cores"},
		"no transfer in time": {func(g *gate) { g.env = append(g.env, "GATE_UPLOAD_SECONDS=0") }, "labd's page times were not read with a transfer in flight during the cold kernel build"},
		"the cold cache":      {func(g *gate) { g.set("volume.fails", "") }, "the mini did not get ready for the builds at 10 cores"},
		"the watch gone":      {func(g *gate) { g.set("watch.dies", "") }, "the watch on the mini ended while the cold kernel build ran"},
		"other cores":         {func(g *gate) { g.set("vm.cores", "4\n") }, "the Builder gave the cold kernel build 4 cores, not 10"},
		"an image missing":    {func(g *gate) { g.set("no.maclab-pkgbuild", "") }, "the mini did not get ready for the builds at 10 cores"},
		"a stale image": {func(g *gate) {
			g.set("outcome.10.1", "stale\n")
			g.set("says.stale", "kbuild: /in/config: No such file or directory\n")
		}, "the cold kernel build (build b1115-000001-abcd) failed after "},
		"a refused config":  {func(g *gate) { g.set("outcome.10.2", "config\n") }, "the warm rebuild (build b1115-000002-abcd) failed after "},
		"a failed transfer": {func(g *gate) { g.set("upload.fails", "") }, "the upload failed: lab: the lab answered 502"},
		"a refused build":   {func(g *gate) { g.set("refuse", "M team builds are not open yet") }, "the cold kernel build could not be asked for: lab: M team builds are not open yet"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := newGate(t)
			c.break_(g)
			out, code := g.run()
			if code != 1 {
				t.Fatalf("exit %d, want 1\n%s", code, out)
			}
			wants(t, "the record", g.record(), "FAILED: "+c.says, "Result: fail")
			if name == "a stale image" {
				wants(t, "the record", g.record(), "sudo /usr/local/lab/current/scripts/mini/setup builds the images the Deployed build names and starts the Builder with them")
			}
			if g.cores() != "10" || strings.Count(g.calls("calls"), "ssh ") != 1 || !strings.HasPrefix(g.results(), "builder-gate fail ") {
				t.Errorf("it went on, or wrote: cores %s, results %q\n%s", g.cores(), g.results(), g.calls("calls"))
			}
		})
	}
}

// Before anything is asked of the mini or of labd: what the owner must
// name, and a Builder that is not idle.
func TestTheGateTestRefusesBeforeItAsksAnything(t *testing.T) {
	g := newGate(t)
	base := []string{"--lab", "--deploy", g.deploy, "--records", g.records}
	full := append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "https://github.com/o/linux-aurora", "--recipe", g.recipe)
	empty := t.TempDir()
	for _, bad := range [][]string{
		append(append([]string{}, base...), "--kernel", "https://github.com/o/r", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "https://github.com/o/r"),
		append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "git@github.com:o/r.git", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "http://github.com/o/r", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "https://me:secret@github.com/o/r", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "https://github.com/o/r; rm -rf x", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini; id", "--kernel", "https://github.com/o/r", "--recipe", g.recipe),
		append(append([]string{}, base...), "--mini", "owner@mini", "--kernel", "https://github.com/o/r", "--recipe", empty),
		append(append([]string{}, full...), "--cores", "7"),
		append(append([]string{}, full...), "--cores", "12"),
		append(append([]string{}, full...), "--config", filepath.Join(empty, "no.config")),
		append(append([]string{}, full...), "--everything"),
		{"--local"},
	} {
		if out, code := run(t, g.env, "", cutover(t, "builder-gate"), bad...); code == 0 || code == 3 {
			t.Errorf("%v: exit %d\n%s", bad[len(base):], code, out)
		}
	}
	if calls := g.calls("calls"); calls != "" {
		t.Fatalf("a refused run called:\n%s", calls)
	}

	// Somebody's build is under way: the Builder is not the test's alone.
	g.set("row.b1114-000009-ffff", "b1114-000009-ffff  running: build 12%  o/r\n")
	out, code := run(t, g.env, "", cutover(t, "builder-gate"), full...)
	if code != 1 || !strings.Contains(out, "a build is queued or running") || strings.Contains(g.calls("calls"), "ssh") || strings.Contains(g.calls("calls"), "lab build ") {
		t.Errorf("with a build under way: exit %d\n%s\n%s", code, out, g.calls("calls"))
	}
	if files, _ := filepath.Glob(filepath.Join(g.records, "*")); len(files) != 0 {
		t.Errorf("a refused run left %v", files)
	}

	// A cores file that says something else than 10, 8 or 6.
	os.WriteFile(filepath.Join(g.deploy, "builder-cpus"), []byte("# cores\n12\n"), 0o644)
	if out, code := run(t, g.env, "", cutover(t, "builder-gate"), "--deploy", g.deploy); code == 0 || !strings.Contains(out, "12 is not a number of cores") {
		t.Errorf("deploy/builder-cpus saying 12: exit %d\n%s", code, out)
	}
}

// With no option it says its steps and runs nothing; with no record,
// --results is not green.
func TestTheGateTestsDryRunSaysTheStepsAndRunsNothing(t *testing.T) {
	added := recordsAdded(t, "*builder-gate*")
	g := newGate(t)
	out, code := run(t, g.env, "", cutover(t, "builder-gate"))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	wants(t, "the dry run", out, "build-watch --cpus 10 --cold --builds 3", "lab build URL --device m1-pro --force", "the warm rebuild", "lab package DIR --force",
		"1073741824 bytes", "under 100 ms at the 95th percentile", "again at 8 cores a build, then 6", "deploy/builder-cpus", "-builder-gate.md", "builder-gate.results", "Nothing was sent.")
	if again, _ := run(t, g.env, "", cutover(t, "builder-gate"), "--dry-run"); again != out {
		t.Error("--dry-run says something else than no option")
	}
	if g.calls("calls") != "" || g.calls("mini-calls") != "" {
		t.Errorf("a dry run ran:\n%s%s", g.calls("calls"), g.calls("mini-calls"))
	}
	if files := added(); len(files) != 0 {
		t.Errorf("a dry run left a record: %v", files)
	}
	if out, code := run(t, nil, "", cutover(t, "builder-gate"), "--results", "--records", g.records); code == 0 || !strings.Contains(out, "no run of the Builder's gate test is recorded") {
		t.Errorf("--results with no run: exit %d\n%s", code, out)
	}
}

var coresLine = regexp.MustCompile(`(?m)^([0-9]+)$`)

// The rule: the most cores, of 10, 8 and 6, that kept the pages under 100
// ms; the next number down while one is still to be run; a stop for the
// owner when 6 did not pass.
func TestTheCoresAreTheMostThatKeptThePagesUnderAHundredMilliseconds(t *testing.T) {
	for _, c := range []struct {
		ten, eight, six string
		status          int
		cores, says     string
	}{
		{"12", "-", "-", 0, "10", "10 cores a build"},
		{"99.9", "-", "-", 0, "10", "10 cores a build"},
		{"100", "-", "-", 4, "10", "It runs again at 8 cores a build"},
		{"140", "-", "-", 4, "10", "missed at 10 (140 ms)"},
		{"140", "90", "-", 0, "8", "Missed with more: 10 (140 ms)"},
		{"fail", "90", "-", 0, "8", "8 cores a build"},
		{"140", "fail", "-", 4, "10", "It runs again at 6 cores a build"},
		{"140", "130", "80", 0, "6", "6 cores a build"},
		{"140", "fail", "120", 3, "10", "did not pass at 10 (140 ms), 8 (failed: see the record), 6 (120 ms)"},
		{"fail", "fail", "fail", 3, "10", "STOP"},
		{"-", "80", "-", 0, "8", "8 cores a build"},
		{"-", "140", "-", 4, "10", "It runs again at 6 cores a build"},
		{"-", "-", "140", 3, "10", "for the owner to decide"},
		{"140", "-", "90", 4, "10", "It runs again at 8 cores a build"}, // 8 was skipped: 6 does not count yet
		{"80", "140", "150", 0, "10", "10 cores a build"},
	} {
		dir := t.TempDir()
		file := filepath.Join(dir, "builder-cpus")
		os.WriteFile(file, []byte(read(t, filepath.Join(repo(t), "deploy", "builder-cpus"))), 0o644)
		out, code := run(t, nil, "", frontDoor(t, "settle"), "cores", c.ten, c.eight, c.six, "--deploy", dir)
		if code != c.status || !strings.Contains(out, c.says) {
			t.Errorf("settle cores %s %s %s: exit %d, want %d and %q\n%s", c.ten, c.eight, c.six, code, c.status, c.says, out)
		}
		if got := setting(t, coresLine, file); got != c.cores {
			t.Errorf("settle cores %s %s %s: deploy/builder-cpus says %s, want %s", c.ten, c.eight, c.six, got, c.cores)
		}
		if c.status != 0 && !strings.Contains(out, "othing was written") {
			t.Errorf("status %d does not say that nothing was written:\n%s", c.status, out)
		}
		if strings.Count(read(t, file), "#") < 3 {
			t.Errorf("settle dropped the file's own words:\n%s", read(t, file))
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "builder-cpus")
	os.WriteFile(file, []byte("# cores\n10\n"), 0o644)
	for _, bad := range [][]string{{"cores"}, {"cores", "12"}, {"cores", "12", "-"}, {"cores", "-", "-", "-"}, {"cores", "fast", "-", "-"}, {"cores", "-5", "-", "-"}, {"cores", "1e1", "-", "-"}, {"cores", "140", "90", "-", "80"}} {
		if out, code := run(t, nil, "", frontDoor(t, "settle"), append(bad, "--deploy", dir)...); code == 0 || code == 3 || code == 4 || setting(t, coresLine, file) != "10" {
			t.Errorf("settle %v: exit %d\n%s", bad, code, out)
		}
	}
	if out, _ := run(t, nil, "", frontDoor(t, "settle"), "cores", "140", "90", "-", "--deploy", dir, "--dry-run"); setting(t, coresLine, file) != "10" || !strings.Contains(out, "would write") {
		t.Errorf("a dry run wrote, or did not say what it would:\n%s", out)
	}
	// The number in deploy/ is the one the set-up and the gate test read.
	if got := setting(t, coresLine, filepath.Join(repo(t), "deploy", "builder-cpus")); got != "10" && got != "8" && got != "6" {
		t.Errorf("deploy/builder-cpus says %s", got)
	}
}

func TestTheGateTestsScriptsAreSound(t *testing.T) {
	files := []string{cutover(t, "builder-gate"), filepath.Join(repo(t), "scripts", "mini", "build-watch")}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", filepath.Base(file))
		}
		if out, err := exec.Command("/bin/bash", "-n", file).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", filepath.Base(file), err, out)
		}
	}
	// It holds no token, sends nothing and starts no VM by itself.
	text := read(t, files[0]) + read(t, files[1])
	for _, banned := range []string{"MACLAB_TOKEN=", "lab_", "bldr_", "signal", "ntfy", "api.cloudflare.com", "container run", "system start"} {
		if strings.Contains(text, banned) {
			t.Errorf("the gate test's scripts name %q", banned)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck is not installed")
	}
	cmd := exec.Command("shellcheck", "--external-sources", files[0])
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}
