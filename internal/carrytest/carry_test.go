// Package carrytest runs the Cutover's scripts for the mini
// (scripts/cutover/old-lab and rehearsal-import) against a made-up mini and
// a made-up old host: a folder stands in for /, launchctl and ssh are played
// by scripts that write down what they were asked, and labd is the real one,
// built for the test.
package carrytest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/omacom/omarchy-maclab/internal/oldlabtest"
	"github.com/omacom/omarchy-maclab/internal/server"
	"github.com/omacom/omarchy-maclab/internal/testhome"
)

// No test here reads or writes the home of whoever runs the tests.
func TestMain(m *testing.M) {
	exit := testhome.Set()
	// The labd these tests build runs on the wall clock sped up this much.
	if os.Getenv("MACLAB_TEST_CLOCK_SCALE") == "" {
		os.Setenv("MACLAB_TEST_CLOCK_SCALE", "10")
	}
	code := m.Run()
	if labdDir != "" {
		os.RemoveAll(labdDir)
	}
	exit(code)
}

const (
	labdLabel       = "org.omarchy-mac.lab.labd"
	litestreamLabel = "org.omarchy-mac.lab.litestream"
	counted         = "records 2 jobs 253 builds 90"
)

func repo(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

var (
	labdOnce sync.Once
	labdDir  string
	labdErr  string
)

// labd is the real labd, built once.
func labd(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("needs go, to build labd")
	}
	labdOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cutover-labd")
		if err != nil {
			labdErr = err.Error()
			return
		}
		labdDir = dir
		cmd := exec.Command("go", "build", "-tags", "labtest", "-o", filepath.Join(dir, "labd"), "./cmd/labd")
		cmd.Dir = repo(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			labdErr = err.Error() + "\n" + string(out)
		}
	})
	if labdErr != "" {
		t.Fatalf("building labd: %s", labdErr)
	}
	return filepath.Join(labdDir, "labd")
}

func write(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// everyFile is every file under dir by its path there, with the SHA-256 of
// what it holds.
func everyFile(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		sum := sha256.Sum256(b)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func less(m map[string]string, names ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for _, n := range names {
		delete(out, n)
	}
	return out
}

func changed(before, after map[string]string) []string {
	var out []string
	for name, sum := range after {
		if before[name] != sum {
			out = append(out, name)
		}
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			out = append(out, name+" (gone)")
		}
	}
	sort.Strings(out)
	return out
}

// launchctl keeps one file for each loaded daemon. Where a test names the
// real labd (STUB_LABD), starting labd runs it on the data directory for a
// moment, so that it makes the stores a labd makes when it starts.
const launchctlStub = `#!/bin/bash
echo "launchctl $*" >> "$STUB_LOG"
label="$(basename "${!#}" .plist)"
state="$STUB_STATE/loaded.$label"
case "$1" in
  print) [[ -f $state ]] || exit 113 ;;
  bootout) rm -f "$state" ;;
  bootstrap)
    [[ -f $3 ]] || exit 5
    : > "$state"
    if [[ $label == org.omarchy-mac.lab.labd && -n ${STUB_LABD:-} ]]; then
      "$STUB_LABD" --listen 127.0.0.1:0 --data "$STUB_DATA" > "$STUB_STATE/labd.out" 2>&1 &
      for n in $(seq 100); do
        [[ ! -f $STUB_DATA/reports.db ]] || break
        sleep 0.1
      done
      sleep 0.3
      kill $! 2>/dev/null
      wait $! 2>/dev/null
    fi ;;
  kickstart) [[ -f $state ]] || exit 113 ;;
esac
exit 0
`

const asLabStub = `#!/bin/bash
echo "as-lab $*" >> "$STUB_LOG"
exec "$@"
`

// ssh runs the command on the made-up old host: in its home, with its tools.
const sshStub = `#!/bin/bash
while [[ $1 == -o ]]; do shift 2; done
echo "ssh $1: $2" >> "$STUB_LOG"
[[ ! -f $STUB_STATE/ssh.fails ]] || exit 255
cd "$GPU_HOME" && HOME="$GPU_HOME" PATH="$GPU_BIN:$PATH" exec /bin/sh -c "$2"
`

// systemctl --user, of the old host: its labd is active and enabled until
// it is disabled.
const systemctlStub = `#!/bin/bash
echo "systemctl $*" >> "$STUB_LOG"
[[ $1 == --user && ${!#} == labd ]] || exit 64
case "$2" in
  is-active) if [[ -f $GPU_STATE/active ]]; then echo active; else echo inactive; exit 3; fi ;;
  is-enabled) if [[ -f $GPU_STATE/enabled ]]; then echo enabled; else echo disabled; exit 1; fi ;;
  disable) [[ $3 == --now ]] || exit 64; rm -f "$GPU_STATE/active" "$GPU_STATE/enabled" ;;
  *) exit 64 ;;
esac
`

const pgrepStub = `#!/bin/bash
[[ -f $GPU_STATE/stray ]]
`

const sha256sumStub = `#!/bin/bash
if [[ -f $GPU_STATE/sums.lie ]]; then
  shasum -a 256 "$@" | sed '1s/^[0-9a-f]\{4\}/0000/'
else
  exec shasum -a 256 "$@"
fi
`

// tar of the old host. A test may have something write into the old
// directory while it is read.
const tarStub = `#!/bin/bash
[[ ! -f $GPU_STATE/writes ]] || echo "a line labd wrote meanwhile" >> "$2/serial/m1pro.log"
# A tar that leaves nothing out.
[[ ! -f $GPU_STATE/takes.all ]] || exec /usr/bin/tar -C "$2" -cf - .
/usr/bin/tar "$@"
status=$?
[[ ! -f $GPU_STATE/writes.after ]] || echo "a line labd wrote after it was read" >> "$2/serial/m1pro.log"
exit $status
`

// mini is the made-up mini, with the made-up old host beside it.
type mini struct {
	t     *testing.T
	base  string
	root  string // stands in for /
	state string
	log   string
	env   []string
	// gpu is the old host's home, old its labd's data directory there, and
	// lab what the fixture knows of it.
	gpu, old string
	lab      *oldlabtest.Lab
	stage    string
	// healthy is whether labd answers once launchd has it.
	healthy atomic.Bool
}

func newMini(t *testing.T, shape oldlabtest.Shape) *mini {
	t.Helper()
	for _, tool := range []string{"bash", "curl", "tar", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
	base, err := os.MkdirTemp("/tmp", "labcut")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	m := &mini{t: t, base: base, root: filepath.Join(base, "r"), state: filepath.Join(base, "state"), log: filepath.Join(base, "stubs.log"),
		gpu: filepath.Join(base, "gpu"), stage: filepath.Join(base, "home", "old-lab")}
	m.healthy.Store(true)
	bin, gpuBin, gpuState := filepath.Join(base, "stubs"), filepath.Join(base, "gpu-bin"), filepath.Join(base, "gpu-state")
	for _, d := range []string{m.state, gpuState, filepath.Join(base, "home"), m.root + "/var/lab/run", m.root + "/var/lab/log"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, text := range map[string]string{"launchctl": launchctlStub, "as-lab": asLabStub, "ssh": sshStub} {
		write(t, filepath.Join(bin, name), text, 0o755)
	}
	for name, text := range map[string]string{"systemctl": systemctlStub, "pgrep": pgrepStub, "sha256sum": sha256sumStub, "tar": tarStub} {
		write(t, filepath.Join(gpuBin, name), text, 0o755)
	}
	write(t, gpuState+"/active", "", 0o644)
	write(t, gpuState+"/enabled", "", 0o644)

	m.old = filepath.Join(m.gpu, ".local", "share", "maclab")
	if err := os.MkdirAll(filepath.Dir(m.old), 0o755); err != nil {
		t.Fatal(err)
	}
	m.lab = oldlabtest.Make(t, m.old, shape)

	// The mini: a Deployed labd in Preview, its Admin, and data of its own.
	build := m.path("usr/local/lab/builds/v0.2.0-rc.9")
	if err := os.MkdirAll(build+"/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(labd(t), build+"/bin/labd"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("builds/v0.2.0-rc.9", m.path("usr/local/lab/current")); err != nil {
		t.Fatal(err)
	}
	// Its labd has no ops socket and its file names none, so the scripts
	// stop it with no drain: a test that wants the socket listens on it
	// itself, and the drain is internal/minitest's to prove.
	plist := read(t, filepath.Join(repo(t), "deploy", labdLabel+".plist"))
	if noOps := strings.Replace(plist, "    <string>--ops-socket</string><string>/var/lab/run/ops.sock</string>\n", "", 1); noOps != plist {
		plist = noOps
	} else {
		t.Fatal("labd's LaunchDaemon file names its ops socket otherwise now")
	}
	write(t, m.path("Library/LaunchDaemons/"+labdLabel+".plist"), plist, 0o644)
	write(t, m.path("Library/LaunchDaemons/"+litestreamLabel+".plist"), read(t, filepath.Join(repo(t), "deploy", litestreamLabel+".plist")), 0o644)
	write(t, m.path("var/lab/secrets/admins"), "7\n", 0o600)
	for _, d := range []string{"var/lab", "var/lab/secrets"} {
		if err := os.Chmod(m.path(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := server.New(ctx, server.Config{DataDir: m.path("var/lab/data")}, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	cancel()
	write(t, m.path("var/lab/data/callers/2026-11-19.log"), "a caller of Preview\n", 0o600)
	write(t, m.state+"/loaded."+labdLabel, "", 0o644)
	write(t, m.state+"/loaded."+litestreamLabel, "", 0o644)

	loop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && m.healthy.Load() {
			io.WriteString(w, "ok")
			return
		}
		http.Error(w, "no", http.StatusServiceUnavailable)
	}))
	t.Cleanup(loop.Close)
	m.env = append(os.Environ(),
		"LAB_SYSROOT="+m.root,
		"PATH="+bin+":"+os.Getenv("PATH"),
		"HOME="+filepath.Join(base, "home"),
		"STUB_LOG="+m.log,
		"STUB_STATE="+m.state,
		"GPU_HOME="+m.gpu,
		"GPU_BIN="+gpuBin,
		"GPU_STATE="+gpuState,
		"LAB_VERIFY_TRIES=1",
		"LAB_AS_LAB="+bin+"/as-lab",
		"LAB_LOOPBACK="+strings.TrimPrefix(loop.URL, "http://"),
		"MACLAB_OLD_LAB=owner@omarchy-gpu",
	)
	return m
}

func (m *mini) path(p string) string { return filepath.Join(m.root, p) }

// run starts one of the Cutover's scripts, typed at its standard input, and
// returns what it printed and its exit status.
func (m *mini) run(typed, script string, args ...string) (string, int) {
	m.t.Helper()
	cmd := exec.Command(filepath.Join(repo(m.t), "scripts", "cutover", script), args...)
	cmd.Env = m.env
	cmd.Dir = m.base
	cmd.Stdin = strings.NewReader(typed)
	out, err := cmd.CombinedOutput()
	said := strings.ReplaceAll(string(out), m.base, "")
	if err == nil {
		m.t.Logf("$ %s %s\n%s", script, strings.Join(args, " "), said)
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		m.t.Logf("$ %s %s\n%sexit %d", script, strings.Join(args, " "), said, ee.ExitCode())
		return string(out), ee.ExitCode()
	}
	m.t.Fatalf("%s: %v\n%s", script, err, out)
	return "", 0
}

func (m *mini) ok(script string, args ...string) string {
	m.t.Helper()
	out, code := m.run("", script, args...)
	if code != 0 {
		m.t.Fatalf("%s %s: exit %d\n%s", script, strings.Join(args, " "), code, out)
	}
	return out
}

func (m *mini) refused(script string, args ...string) string {
	m.t.Helper()
	out, code := m.run("", script, args...)
	if code == 0 {
		m.t.Fatalf("%s %s was not refused\n%s", script, strings.Join(args, " "), out)
	}
	return out
}

func (m *mini) calls() string { return read(m.t, m.log) }

func (m *mini) loaded(label string) bool { return exists(filepath.Join(m.state, "loaded."+label)) }

// ids is every id of one of labd.db's tables in dir, in order.
func ids(t *testing.T, dir, table string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "labd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id FROM ` + table + ` ORDER BY id`)
	if err != nil {
		t.Fatalf("%s in %s: %v", table, dir, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func count(t *testing.T, file, query string) int {
	t.Helper()
	db, err := sql.Open("sqlite", file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n := -1
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// The whole of step 10: the old labd stopped and disabled, its directory
// copied without its token files and checked, then made the lab's.
func TestTheOldLabsDataGoesFromItsHostToTheMini(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	source := everyFile(t, m.old)
	records, jobs, builds := ids(t, m.old, "devices"), ids(t, m.old, "jobs"), ids(t, m.old, "builds")
	mine := everyFile(t, m.path("var/lab"))

	// Nothing is copied from a labd that still runs.
	if out := m.refused("old-lab", "copy", "--stage", m.stage); !strings.Contains(out, "still runs") || exists(m.stage) {
		t.Fatalf("a copy from a running labd: %s", out)
	}
	if out := m.ok("old-lab", "stop", "--dry-run"); !strings.Contains(out, "would stop and disable") || strings.Contains(m.calls(), "disable --now") {
		t.Fatalf("the dry run of stop: %s\n%s", out, m.calls())
	}
	out := m.ok("old-lab", "stop")
	if !strings.Contains(m.calls(), "ssh owner@omarchy-gpu: systemctl --user disable --now 'labd'") || !strings.Contains(out, "inactive disabled") {
		t.Fatalf("stop: %s\n%s", out, m.calls())
	}
	if out := m.ok("old-lab", "stop"); !strings.Contains(out, "already") {
		t.Errorf("a second stop: %s", out)
	}

	if out := m.ok("old-lab", "copy", "--dry-run", "--stage", m.stage); !strings.Contains(out, "would copy") || exists(m.stage) {
		t.Fatalf("the dry run of copy: %s", out)
	}
	out = m.ok("old-lab", "copy", "--stage", m.stage)
	if !strings.Contains(out, "the copy holds: "+counted) || !strings.Contains(out, "is as it was") {
		t.Errorf("copy said: %s", out)
	}
	if got := everyFile(t, m.old); !reflect.DeepEqual(got, source) {
		t.Errorf("the copy changed the old lab's directory: %v", changed(source, got))
	}
	if got, want := everyFile(t, m.stage+"/data"), less(source, "admin.token", "builder.token"); !reflect.DeepEqual(got, want) {
		t.Errorf("the copy is not the old directory less its token files: %v", changed(want, got))
	}
	if !strings.Contains(m.calls(), "--exclude='./admin.token' --exclude='./builder.token' -cf - .") {
		t.Errorf("the old host was asked for its token files:\n%s", m.calls())
	}
	if st, err := os.Stat(m.stage); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("the copy's folder is not its owner's alone: %v", err)
	}
	if out := m.ok("old-lab", "copy", "--stage", m.stage); !strings.Contains(out, "there and whole") {
		t.Errorf("a second copy: %s", out)
	}
	staged := everyFile(t, m.stage)

	// The upgrade, first as a dry run.
	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "owner"); !strings.Contains(out, "--account-id") {
		t.Errorf("an upgrade for nobody: %s", out)
	}
	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "8", "owner"); !strings.Contains(out, "Admins") {
		t.Errorf("an upgrade for an account that is no Admin: %s", out)
	}
	out = m.ok("old-lab", "upgrade", "--dry-run", "--stage", m.stage, "--account-id", "7", "owner")
	if !strings.Contains(out, "A dry run") || !strings.Contains(out, "Retired: m1pro, m1pro2") || !strings.Contains(out, "would stop labd") {
		t.Errorf("the dry run of upgrade: %s", out)
	}
	if got := everyFile(t, m.path("var/lab")); !reflect.DeepEqual(got, mine) {
		t.Errorf("a dry run and two refusals changed the lab's files: %v", changed(mine, got))
	}
	if got := everyFile(t, m.stage); !reflect.DeepEqual(got, staged) {
		t.Errorf("a dry run changed the copy: %v", changed(staged, got))
	}
	if strings.Contains(m.calls(), "launchctl bootout") {
		t.Errorf("a dry run stopped a daemon:\n%s", m.calls())
	}

	os.Remove(m.log)
	out = m.ok("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner")
	data := m.path("var/lab/data")
	if !strings.Contains(out, "the old lab's data is the lab's: "+counted) {
		t.Errorf("upgrade said: %s", out)
	}
	if got := ids(t, data, "retired_macs"); !reflect.DeepEqual(got, records) {
		t.Errorf("Retired records %v, want %v", got, records)
	}
	if got := ids(t, data, "devices"); len(got) != 0 {
		t.Errorf("records still in the fleet: %v", got)
	}
	if got := ids(t, data, "jobs"); !reflect.DeepEqual(got, jobs) {
		t.Errorf("%d jobs, want the old lab's %d under their ids", len(got), len(jobs))
	}
	if got := ids(t, data, "builds"); !reflect.DeepEqual(got, builds) {
		t.Errorf("%d builds, want the old lab's %d under their ids", len(got), len(builds))
	}
	if n := count(t, data+"/labd.db", `SELECT count(*) FROM people WHERE account_id = 7 AND handle = 'owner'`); n != 1 {
		t.Error("the owner is not a person of the lab")
	}
	now := everyFile(t, data)
	for name, sum := range oldlabtest.Files(t, m.old) {
		if now[name] != sum {
			t.Errorf("%s of the old lab is not in the lab's directory as it was", name)
		}
	}
	for _, f := range []string{"admin.token", "builder.token"} {
		if exists(filepath.Join(data, f)) {
			t.Errorf("%s reached the mini", f)
		}
	}
	// The lab's own reports and caller log moved over; the rest is kept.
	kept := m.path("var/lab/data.before-old-lab")
	if now["reports.db"] != mine["data/reports.db"] || now["callers/2026-11-19.log"] != mine["data/callers/2026-11-19.log"] {
		t.Error("the lab's reports.db and caller log did not move into the new directory")
	}
	if got := everyFile(t, kept); got["labd.db"] != mine["data/labd.db"] || got["reports.db"] != "" {
		t.Errorf("what the lab's directory held is not kept in %s", kept)
	}
	if exists(m.path("var/lab/data.old-lab")) {
		t.Error("the working copy was left behind")
	}
	calls := m.calls()
	stop, start := strings.Index(calls, "launchctl bootout system/"+labdLabel), strings.Index(calls, "launchctl bootstrap system "+m.path("Library/LaunchDaemons/"+labdLabel+".plist"))
	move := strings.Index(calls, "as-lab mv "+data+" "+kept)
	if stop < 0 || move < stop || start < move {
		t.Errorf("labd was not stopped before the switch and started after it:\n%s", calls)
	}
	if !m.loaded(labdLabel) || !m.loaded(litestreamLabel) {
		t.Error("labd and Litestream are not running again")
	}
	if got := everyFile(t, m.stage); !reflect.DeepEqual(got, staged) {
		t.Errorf("the upgrade changed the copy it was given: %v", changed(staged, got))
	}
	if got := everyFile(t, m.old); !reflect.DeepEqual(got, source) {
		t.Errorf("the old lab's directory changed: %v", changed(source, got))
	}
	for _, secret := range []string{m.lab.AdminToken, m.lab.SharedBuilder, m.lab.EnrollToken, m.lab.Records[0].Secret} {
		if strings.Contains(calls+out, secret) {
			t.Error("a secret of the old lab was printed or passed as an argument")
		}
	}

	// It goes in once.
	before := everyFile(t, m.path("var/lab"))
	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner"); !strings.Contains(out, "once") {
		t.Errorf("a second upgrade: %s", out)
	}
	if got := everyFile(t, m.path("var/lab")); !reflect.DeepEqual(got, before) {
		t.Errorf("a second upgrade changed %v", changed(before, got))
	}
}

// The copy is refused when it is not the old directory file for file, and
// when the old directory changed while it was read.
func TestACopyThatIsNotTheOldDirectoryIsRefused(t *testing.T) {
	m := newMini(t, oldlabtest.AtFork)
	m.ok("old-lab", "stop")
	gpuState := filepath.Join(m.base, "gpu-state")

	write(t, gpuState+"/stray", "", 0o644)
	if out := m.refused("old-lab", "copy", "--stage", m.stage); !strings.Contains(out, "still runs") {
		t.Errorf("a copy beside a labd started by hand: %s", out)
	}
	os.Remove(gpuState + "/stray")

	write(t, gpuState+"/sums.lie", "", 0o644)
	if out := m.refused("old-lab", "copy", "--stage", m.stage); !strings.Contains(out, "file for file") || exists(m.stage+"/data") {
		t.Errorf("a copy that differs from the old host's list: %s", out)
	}
	os.Remove(gpuState + "/sums.lie")

	write(t, gpuState+"/writes", "", 0o644)
	if out := m.refused("old-lab", "copy", "--stage", m.stage); !(strings.Contains(out, "changed while it was copied") || strings.Contains(out, "file for file")) || exists(m.stage+"/data") {
		t.Errorf("a copy of a directory something still writes in: %s", out)
	}
	os.Remove(gpuState + "/writes")

	// The copy is whole, and the old directory is not what it was: it is
	// listed again after the copy.
	write(t, gpuState+"/writes.after", "", 0o644)
	if out := m.refused("old-lab", "copy", "--stage", m.stage); !strings.Contains(out, "changed while it was copied") || exists(m.stage+"/data") {
		t.Errorf("a copy of a directory that changed after it was read: %s", out)
	}
	os.Remove(gpuState + "/writes.after")

	write(t, m.state+"/ssh.fails", "", 0o644)
	if out := m.refused("old-lab", "copy", "--stage", m.stage); !strings.Contains(out, "was not reached over SSH") {
		t.Errorf("a copy with no SSH to the old host: %s", out)
	}
	os.Remove(m.state + "/ssh.fails")

	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner"); !strings.Contains(out, "old-lab copy first") {
		t.Errorf("an upgrade with no whole copy: %s", out)
	}
	for _, bad := range [][]string{{"copy", "--from", "omarchy-gpu; rm -rf ."}, {"copy", "--data", "x'; rm -rf ."}, {"stop", "--unit", "labd; true"}, {"forget"}, {}} {
		before := m.calls()
		m.refused("old-lab", bad...)
		if m.calls() != before {
			t.Errorf("old-lab %v reached the old host:\n%s", bad, m.calls())
		}
	}
	// The old lab's host is named by its owner: the repository names none.
	named := m.env
	m.env = append(m.env, "MACLAB_OLD_LAB=")
	for _, step := range []string{"stop", "copy"} {
		before := m.calls()
		if out := m.refused("old-lab", step); !strings.Contains(out, "name the old lab's host") || m.calls() != before {
			t.Errorf("old-lab %s with no host named: %s", step, out)
		}
	}
	m.env = named
	// The token files do not stay on the mini even if the old host's tar
	// sends them.
	write(t, gpuState+"/takes.all", "", 0o644)
	m.ok("old-lab", "copy", "--stage", m.stage)
	if !exists(m.stage+"/data/labd.db") || exists(m.stage+"/data/admin.token") || exists(m.stage+"/data/builder.token") {
		t.Error("no copy after the refusals, or one with a token file in it")
	}
}

// The upgrade is refused for a lab that is open, and for one that holds
// Macs, jobs or builds of its own; neither loses anything.
func TestTheOldLabsDataGoesOnlyIntoALabInPreviewWithNothingOfItsOwn(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	m.ok("old-lab", "stop")
	m.ok("old-lab", "copy", "--stage", m.stage)
	plist := m.path("Library/LaunchDaemons/" + labdLabel + ".plist")
	preview := read(t, plist)

	write(t, plist, strings.Replace(preview, "    <string>--preview</string>\n", "", 1), 0o644)
	before := everyFile(t, m.path("var/lab"))
	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner"); !strings.Contains(out, "the lab is open") {
		t.Errorf("an upgrade of an open lab: %s", out)
	}
	write(t, plist, preview, 0o644)

	db, err := sql.Open("sqlite", m.path("var/lab/data/labd.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO devices(id, name, secret_hash, data) VALUES ('d-1', 'm1-pro', 'x', '{"id":"d-1","name":"m1-pro"}')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before = everyFile(t, m.path("var/lab"))
	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner"); !strings.Contains(out, "records 1 jobs 0 builds 0") {
		t.Errorf("an upgrade over a lab with a Mac of its own: %s", out)
	}
	if got := everyFile(t, m.path("var/lab")); !reflect.DeepEqual(less(got, "data/labd.db-shm", "data/labd.db-wal"), less(before, "data/labd.db-shm", "data/labd.db-wal")) {
		t.Errorf("a refused upgrade changed %v", changed(before, got))
	}
	if strings.Contains(m.calls(), "launchctl bootout") {
		t.Errorf("a refused upgrade stopped a daemon:\n%s", m.calls())
	}
}

// When labd does not answer on the old lab's data, the directory the lab
// had is put back with its reports, and the upgraded copy is kept aside.
func TestTheLabsDirectoryIsPutBackWhenLabdDoesNotAnswer(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	m.ok("old-lab", "stop")
	m.ok("old-lab", "copy", "--stage", m.stage)
	mine := everyFile(t, m.path("var/lab/data"))

	m.healthy.Store(false)
	out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner")
	if !strings.Contains(out, "the old lab's data is not in") {
		t.Errorf("said: %s", out)
	}
	if got := everyFile(t, m.path("var/lab/data")); !reflect.DeepEqual(got, mine) {
		t.Errorf("the lab's directory is not back as it was: %v", changed(mine, got))
	}
	if exists(m.path("var/lab/data.before-old-lab")) {
		t.Error("data.before-old-lab is left, which would refuse the next run")
	}
	if got := ids(t, m.path("var/lab/data.old-lab"), "retired_macs"); len(got) != 2 {
		t.Errorf("the upgraded copy is not kept aside: %v", got)
	}
	if !m.loaded(labdLabel) {
		t.Error("labd was not started again")
	}

	// With labd answering, the next run goes through.
	m.healthy.Store(true)
	if out := m.ok("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner"); !strings.Contains(out, "the old lab's data is the lab's: "+counted) {
		t.Errorf("the run after it: %s", out)
	}
}

// A switch that stops after the lab's reports moved into the upgraded
// directory and before that became the lab's puts each file back from where
// it is: the lab's directory is whole again, reports and caller log with it.
func TestASwitchThatStopsPartWayPutsEachFileBackFromWhereItIs(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	m.ok("old-lab", "stop")
	m.ok("old-lab", "copy", "--stage", m.stage)
	mine := everyFile(t, m.path("var/lab/data"))
	asLab := filepath.Join(m.base, "stubs", "as-lab")
	plain := read(t, asLab)
	write(t, asLab, "#!/bin/bash\necho \"as-lab $*\" >> \"$STUB_LOG\"\n[[ \"$1 ${2##*/} ${3##*/}\" != \"mv data.old-lab data\" ]] || exit 1\nexec \"$@\"\n", 0o755)

	out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner")

	if got := everyFile(t, m.path("var/lab/data")); !reflect.DeepEqual(got, mine) {
		t.Errorf("the lab's directory is not back as it was: %v\n%s", changed(mine, got), out)
	}
	if exists(m.path("var/lab/data.before-old-lab")) || exists(m.path("var/lab/data.old-lab/reports.db")) {
		t.Error("data.before-old-lab is left, or the lab's reports stayed in the upgraded copy")
	}
	if !m.loaded(labdLabel) {
		t.Error("labd was not started again")
	}

	// A data.old-lab that holds a reports.db holds the lab's own reports:
	// it is never removed to make room.
	write(t, asLab, plain, 0o755)
	write(t, m.path("var/lab/data.old-lab/reports.db"), "the lab's reports\n", 0o600)
	if out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner"); !strings.Contains(out, "reports.db") {
		t.Errorf("a data.old-lab with reports in it: %s", out)
	}
	if got := read(t, m.path("var/lab/data.old-lab/reports.db")); got != "the lab's reports\n" {
		t.Errorf("the reports in data.old-lab: %q", got)
	}
}

// A way back whose own move fails leaves labd stopped, and both folders.
func TestAWayBackThatCannotMoveTheReportsLeavesLabdStopped(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	m.ok("old-lab", "stop")
	m.ok("old-lab", "copy", "--stage", m.stage)
	reports := read(t, m.path("var/lab/data/reports.db"))
	asLab := filepath.Join(m.base, "stubs", "as-lab")
	write(t, asLab, "#!/bin/bash\necho \"as-lab $*\" >> \"$STUB_LOG\"\n"+
		"[[ \"$1 ${2##*/} ${3##*/}\" != \"mv data.old-lab data\" ]] || exit 1\n"+
		"[[ \"$1 $2\" != \"mv \"*/data.old-lab/reports.db ]] || exit 1\n"+
		"exec \"$@\"\n", 0o755)

	out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner")

	if m.loaded(labdLabel) || m.loaded(litestreamLabel) {
		t.Errorf("started on a directory with no reports: labd %v, Litestream %v\n%s", m.loaded(labdLabel), m.loaded(litestreamLabel), out)
	}
	if !exists(m.path("var/lab/data.before-old-lab/labd.db")) || read(t, m.path("var/lab/data.old-lab/reports.db")) != reports {
		t.Errorf("a folder of the way back is gone\n%s", out)
	}
	if !strings.Contains(out, "labd is left stopped") || !strings.Contains(out, "reports.db") {
		t.Errorf("no word of the failed move:\n%s", out)
	}
}

// A way back that cannot stop labd moves nothing: a labd still running is
// never left on a directory pulled from under it. Both folders stay, and it
// says which did not stop.
func TestAWayBackWhoseStopFailsMovesNothing(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	m.ok("old-lab", "stop")
	m.ok("old-lab", "copy", "--stage", m.stage)
	stubs := filepath.Join(m.base, "stubs")
	// labd stops for the switch, and not once it runs on the old lab's data.
	write(t, stubs+"/launchctl", strings.Replace(launchctlStub, `bootout) rm -f "$state" ;;`, `bootout) [[ $label == `+labdLabel+` && -f $STUB_STATE/sticks ]] || rm -f "$state" ;;`, 1), 0o755)
	write(t, stubs+"/sleep", "#!/bin/sh\nexit 0\n", 0o755)
	asLab := filepath.Join(stubs, "as-lab")
	write(t, asLab, "#!/bin/bash\necho \"as-lab $*\" >> \"$STUB_LOG\"\n[[ \"$1 ${2##*/} ${3##*/}\" != \"mv data.old-lab data\" ]] || touch \"$STUB_STATE/sticks\"\nexec \"$@\"\n", 0o755)
	m.healthy.Store(false)

	out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner")

	if !exists(m.path("var/lab/data.before-old-lab/labd.db")) || !exists(m.path("var/lab/data/reports.db")) || exists(m.path("var/lab/data.old-lab")) {
		t.Errorf("the way back moved something after labd did not stop\n%s", out)
	}
	if got := ids(t, m.path("var/lab/data"), "retired_macs"); len(got) != 2 {
		t.Errorf("the lab's directory is no longer the upgraded one\n%s", out)
	}
	if !strings.Contains(out, "could not stop labd") || !strings.Contains(out, "moved nothing") {
		t.Errorf("no word of the stop that failed:\n%s", out)
	}
}

// Litestream that does not stop leaves labd stopped part-way: the way back
// is armed before the stop, and labd runs again on the directory it had.
func TestAStopThatFailsPartWayStartsLabdAgain(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	m.ok("old-lab", "stop")
	m.ok("old-lab", "copy", "--stage", m.stage)
	mine := everyFile(t, m.path("var/lab/data"))
	stubs := filepath.Join(m.base, "stubs")
	write(t, stubs+"/launchctl", strings.Replace(launchctlStub, `bootout) rm -f "$state" ;;`, `bootout) [[ $label == `+litestreamLabel+` ]] || rm -f "$state" ;;`, 1), 0o755)
	write(t, stubs+"/sleep", "#!/bin/sh\nexit 0\n", 0o755)

	out := m.refused("old-lab", "upgrade", "--stage", m.stage, "--account-id", "7", "owner")

	if !strings.Contains(out, "Litestream did not stop") || !m.loaded(labdLabel) {
		t.Errorf("labd was left stopped (loaded %v):\n%s", m.loaded(labdLabel), out)
	}
	if got := everyFile(t, m.path("var/lab/data")); !reflect.DeepEqual(less(got, "labd.db-shm", "labd.db-wal"), less(mine, "labd.db-shm", "labd.db-wal")) {
		t.Errorf("the lab's directory changed: %v", changed(mine, got))
	}
}

// A copy of scripts/cutover alone, as the owner puts it on the mini, takes
// the mini's own library from the Deployed build.
func TestACopyOfTheScriptsUsesTheDeployedBuildsLibrary(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	alone := filepath.Join(m.base, "home", "lab-cutover")
	for _, name := range []string{"old-lab", "rehearsal-import", "mini.sh"} {
		write(t, filepath.Join(alone, name), read(t, filepath.Join(repo(t), "scripts", "cutover", name)), 0o755)
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command(filepath.Join(alone, "old-lab"), args...)
		cmd.Env = m.env
		cmd.Dir = m.base
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := run("stop", "--dry-run"); err == nil || !strings.Contains(out, "no Lab release is Deployed") {
		t.Errorf("with no library anywhere: %v, %s", err, out)
	}
	write(t, m.path("usr/local/lab/current/scripts/mini/lib.sh"), read(t, filepath.Join(repo(t), "scripts", "mini", "lib.sh")), 0o644)
	if out, err := run("stop", "--dry-run"); err != nil || !strings.Contains(out, "would stop and disable") {
		t.Errorf("with the Deployed build's library: %v, %s", err, out)
	}
}
