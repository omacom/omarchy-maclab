// Package minitest runs the mini's scripts (scripts/mini) against a made-up
// machine: a folder that stands in for /, with launchctl, dscl, installer and
// brew played by scripts that write down what they were asked.
package minitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	bundleName = "lab-server-darwin-arm64.tar.gz"
	labdLabel  = "org.omarchy-mac.lab.labd"
	oobdLabel  = "com.maclab.oobd"

	cloudflaredLabel = "org.omarchy-mac.lab.cloudflared"
	signer           = "release@omarchy-m-testing.org"
	namespace        = "omarchy-m-test-release"
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

func need(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s", tool)
		}
	}
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

// mini is the made-up machine.
type mini struct {
	t *testing.T
	// root stands in for /. It is short: a Unix socket's path has to fit.
	root string
	// tree is an unpacked bundle: the scripts under test and their deploy/ files.
	tree string
	bin  string
	// tools holds the stand-ins for what reads or changes the machine itself.
	tools string
	log   string
	env   []string
}

// launchctl keeps one file for each loaded daemon: "loaded" for labd,
// "loaded.<label>" for any other. The daemon is what the last argument names.
// A test may write beside it the state and the count of starts print shows.
// bootstrap.fails.<label> makes a bootstrap fail: a number is how many times,
// anything else a text whose LaunchDaemon file is refused. bootout.sticks.<label>
// keeps a daemon loaded through every bootout.
const launchctlStub = `#!/bin/bash
echo "launchctl $*" >> "$STUB_LOG"
label="$(basename "${!#}" .plist)"
state="$STUB_STATE/loaded"
[[ $label == org.omarchy-mac.lab.labd ]] || state="$STUB_STATE/loaded.$label"
case "$1" in
  print)
    [[ -f $state ]] || exit 113
    echo "state = $(cat "$state.state" 2>/dev/null || echo running)"
    echo "runs = $(cat "$state.runs" 2>/dev/null || echo 1)"
    [[ ! -f $STUB_STATE/pid.$label ]] || echo "	pid = $(<"$STUB_STATE/pid.$label")" ;;
  bootout) [[ -f $STUB_STATE/bootout.sticks.$label ]] || rm -f "$state" ;;
  bootstrap)
    [[ -f $3 ]] || exit 5
    f="$STUB_STATE/bootstrap.fails.$label"
    if [[ -s $f ]]; then
      n="$(<"$f")"
      if [[ $n =~ ^[0-9]+$ ]]; then
        (( n == 0 )) || { echo $((n - 1)) > "$f"; exit 5; }
      elif grep -qF -- "$n" "$3"; then
        exit 5
      fi
    fi
    cp "$3" "$state" ;;
  kickstart) [[ -f $state && ! -f $STUB_STATE/kickstart.fails ]] || exit 113 ;;
esac
`

const dsclStub = `#!/bin/bash
echo "dscl $*" >> "$STUB_LOG"
shift
verb="$1" path="$2" key="${3:-}" value="${4:-}"
file="$STUB_STATE/dscl$path"
case "$verb" in
  -read) [[ -d $file ]] || exit 56; [[ -z $key ]] || { [[ -f $file/$key ]] && echo "$key: $(<"$file/$key")"; } ;;
  -create) mkdir -p "$file"; [[ -z $key ]] || printf '%s' "$value" > "$file/$key" ;;
  -list)
    echo "root 0"
    for d in "$file"/*/; do
      [[ -f $d$key ]] && echo "$(basename "$d") $(<"$d$key")"
    done
    exit 0 ;;
esac
`

const installerStub = `#!/bin/bash
echo "installer $*" >> "$STUB_LOG"
`

// as-lab stands in for sudo -u lab: it writes down what the lab account was
// asked to run, and runs it. A program under STUB_LAB_CANNOT_READ is one the
// lab account cannot read, as in an admin's home, and does not run.
const asLabStub = `#!/bin/bash
echo "as-lab $*" >> "$STUB_LOG"
if [[ -n ${STUB_LAB_CANNOT_READ:-} && $1 == "$STUB_LAB_CANNOT_READ"/* ]]; then
  echo "bash: $1: Permission denied" >&2
  exit 126
fi
exec "$@"
`

// brew keeps its kegs in Cellar/<formula>/<version> and links opt/<formula>
// to the one in use, as Homebrew does. "install" makes version 1.0.
const brewStub = `#!/bin/bash
prefix="$(cd "$(dirname "$0")/.." && pwd)"
echo "brew $* [auto_update=${HOMEBREW_NO_AUTO_UPDATE:-} upgrade=${HOMEBREW_NO_INSTALL_UPGRADE:-} uid=$(id -u)]" >> "$STUB_LOG"
# A formula of a tap is asked for as tap/formula and kept under its own name.
case "$1" in
  list) ls "$prefix/Cellar/${3##*/}" 2>/dev/null | sed "s/^/${3##*/} /" ;;
  deps) cat "$prefix/deps/${2##*/}" 2>/dev/null || true ;;
  --prefix) echo "$prefix/opt/${2##*/}" ;;
  install)
    for f in "${2##*/}" $(cat "$prefix/deps/${2##*/}" 2>/dev/null); do
      mkdir -p "$prefix/Cellar/$f/1.0/bin" "$prefix/opt"
      printf '#!/bin/sh\necho %s 1.0\n' "$f" > "$prefix/Cellar/$f/1.0/bin/$f"
      chmod 0775 "$prefix/Cellar/$f/1.0/bin/$f" "$prefix/Cellar/$f/1.0/bin"
      ln -sfn "../Cellar/$f/1.0" "$prefix/opt/$f"
      # tailscale's keg holds the command and the daemon; the set-up asks the command.
      [[ $f != tailscale ]] || { cp "$STUB_TAILSCALE" "$prefix/Cellar/$f/1.0/bin/tailscale"; printf '#!/bin/sh\n# tailscaled 1.0\n' > "$prefix/Cellar/$f/1.0/bin/tailscaled"; chmod 0775 "$prefix/Cellar/$f/1.0/bin/tailscaled"; }
    done ;;
  update) [[ ! -f $prefix/update.fails ]] || exit 1 ;;
  # A Maintenance run's upgrade: a formula with a file newer/<formula> gets
  # the version that file names, and the program newer/<formula>.bin if any.
  upgrade)
    [[ ! -f $prefix/upgrade.fails ]] || { echo "Error: no bottle" >&2; exit 1; }
    shift
    for f in "$@"; do
      f="${f##*/}"
      [[ -f $prefix/newer/$f ]] || continue
      v="$(<"$prefix/newer/$f")" old="$(ls "$prefix/Cellar/$f" | head -n 1)"
      [[ $v != "$old" ]] || continue
      cp -Rp "$prefix/Cellar/$f/$old" "$prefix/Cellar/$f/$v"
      chmod -R u+w "$prefix/Cellar/$f"
      if [[ -f $prefix/newer/$f.bin ]]; then
        cat "$prefix/newer/$f.bin" > "$prefix/Cellar/$f/$v/bin/$f"
      elif [[ $f != tailscale ]]; then
        printf '#!/bin/sh\necho %s %s\n' "$f" "$v" > "$prefix/Cellar/$f/$v/bin/$f"
      fi
      rm -rf "$prefix/Cellar/$f/$old"
      ln -sfn "../Cellar/$f/$v" "$prefix/opt/$f"
    done ;;
  *) echo "brew $1: not expected of the set-up" >&2; exit 1 ;;
esac
`

func newMini(t *testing.T) *mini {
	t.Helper()
	need(t, "bash", "curl", "ssh-keygen", "tar")
	root, err := os.MkdirTemp("/tmp", "labmini")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	m := &mini{t: t, root: filepath.Join(root, "r"), tree: filepath.Join(root, "lab-server"), bin: filepath.Join(root, "stubs"), tools: filepath.Join(root, "machine"), log: filepath.Join(root, "stubs.log")}
	for _, d := range []string{m.root + "/Library/LaunchDaemons", filepath.Join(root, "state")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, m.bin+"/launchctl", launchctlStub, 0o755)
	write(t, m.bin+"/dscl", dsclStub, 0o755)
	write(t, m.bin+"/installer", installerStub, 0o755)
	write(t, m.bin+"/as-lab", asLabStub, 0o755)
	write(t, m.root+"/opt/homebrew/bin/brew", brewStub, 0o755)
	src := filepath.Join(repo(t), "scripts", "mini")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("the mini's scripts: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		info, _ := e.Info()
		write(t, filepath.Join(m.tree, "scripts", "mini", e.Name()), string(b), info.Mode().Perm())
	}
	write(t, m.tree+"/deploy/"+oobdLabel+".plist", read(t, filepath.Join(repo(t), "deploy", oobdLabel+".plist")), 0o644)
	write(t, m.tree+"/deploy/homebrew-formulae", "# none\n", 0o644)
	write(t, m.tree+"/deploy/"+cloudflaredLabel+".plist", read(t, filepath.Join(repo(t), "deploy", cloudflaredLabel+".plist")), 0o644)
	write(t, m.tree+"/deploy/"+signalLabel+".plist", read(t, filepath.Join(repo(t), "deploy", signalLabel+".plist")), 0o644)
	for _, f := range []string{litestreamLabel + ".plist", backupLabel + ".plist", "litestream.yml", deadManLabel + ".plist", drillLabel + ".plist", tailscaledLabel + ".plist"} {
		write(t, m.tree+"/deploy/"+f, read(t, filepath.Join(repo(t), "deploy", f)), 0o644)
	}
	write(t, m.tree+"/deploy/pins", "# none\n", 0o644)
	m.env = append(minisEnviron(),
		"LAB_SYSROOT="+m.root,
		"PATH="+m.bin+":"+os.Getenv("PATH"),
		"STUB_LOG="+m.log,
		"STUB_STATE="+filepath.Join(root, "state"),
		"LAB_VERIFY_TRIES=2",
		"LAB_TELL_TRIES=2",
		"LAB_SENDER_WAIT=0",
		"LAB_PAUSE=0.01",
		"LAB_AS_LAB="+m.bin+"/as-lab",
	)
	m.machine()
	return m
}

// minisEnviron is this process's environment less what the mini's
// programs never get: the scripts run under sudo and the daemons under
// launchd, neither of which passes a JAVA_HOME. A runner's own JAVA_HOME
// would have signal-cli's wrapper run that Java instead of its copy's.
func minisEnviron() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "JAVA_HOME=") {
			env = append(env, kv)
		}
	}
	return env
}

// run starts one of the mini's scripts and returns what it printed and its exit status.
func (m *mini) run(script string, args ...string) (string, int) {
	m.t.Helper()
	cmd := exec.Command(filepath.Join(m.tree, "scripts", "mini", script), args...)
	cmd.Env = m.env
	cmd.Dir = filepath.Dir(m.tree)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	m.t.Fatalf("%s: %v\n%s", script, err, out)
	return "", 0
}

func (m *mini) ok(script string, args ...string) string {
	m.t.Helper()
	out, code := m.run(script, args...)
	if code != 0 {
		m.t.Fatalf("%s %s: exit %d\n%s", script, strings.Join(args, " "), code, out)
	}
	return out
}

func (m *mini) refused(script string, args ...string) string {
	m.t.Helper()
	out, code := m.run(script, args...)
	if code == 0 {
		m.t.Fatalf("%s %s was not refused\n%s", script, strings.Join(args, " "), out)
	}
	return out
}

func (m *mini) calls() string { return read(m.t, m.log) }

// loaded reports whether labd's LaunchDaemon is loaded.
func (m *mini) loaded() bool { return exists(filepath.Join(filepath.Dir(m.root), "state", "loaded")) }

func (m *mini) path(p string) string { return filepath.Join(m.root, p) }

// current is the tag the mini runs, or "".
func (m *mini) current() string {
	to, err := os.Readlink(m.path("usr/local/lab/current"))
	if err != nil {
		return ""
	}
	return filepath.Base(to)
}

func (m *mini) builds() []string {
	entries, _ := os.ReadDir(m.path("usr/local/lab/builds"))
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// key is a release key made for one test.
type key struct {
	private, signers string
}

func newKey(t *testing.T) key {
	t.Helper()
	dir := t.TempDir()
	k := key{private: filepath.Join(dir, "key"), signers: filepath.Join(dir, "allowed_signers")}
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "a test's key", "-f", k.private).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub := strings.Fields(read(t, k.private+".pub"))
	write(t, k.signers, fmt.Sprintf("%s namespaces=%q %s %s\n", signer, namespace, pub[0], pub[1]), 0o644)
	return k
}

func (k key) sign(t *testing.T, file string) {
	t.Helper()
	os.Remove(file + ".sig")
	cmd := exec.Command("ssh-keygen", "-q", "-Y", "sign", "-f", k.private, "-n", namespace, file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("signing %s: %v\n%s", file, err, out)
	}
}

// bundle writes a server bundle of tag into dir and signs it. Its labd is a
// script that says its version; an unhealthy one says so in its text, which
// the made-up labd below reads.
func bundle(t *testing.T, k key, dir, tag string, healthy bool, routes ...string) {
	t.Helper()
	bundleWith(t, k, dir, tag, healthy, nil, routes...)
}

// bundleWith is bundle with more files in it, by their path in the build.
func bundleWith(t *testing.T, k key, dir, tag string, healthy bool, more map[string]string, routes ...string) {
	t.Helper()
	routes = append(routes, "the first routes")
	labd := "#!/bin/sh\n[ \"$1\" = --version ] && echo " + tag + "\n"
	if !healthy {
		labd += "# unhealthy\n"
	}
	files := []struct {
		name, text string
		mode       int64
	}{
		{"lab-server/VERSION", tag + "\n", 0o644},
		{"lab-server/bin/labd", labd, 0o755},
		{"lab-server/deploy/" + labdLabel + ".plist", "<plist><!-- " + tag + " --></plist>\n", 0o664},
		{"lab-server/deploy/cloudflared.yml", "# " + routes[0] + "\n", 0o644},
	}
	names := make([]string, 0, len(more))
	for name := range more {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		files = append(files, struct {
			name, text string
			mode       int64
		}{"lab-server/" + name, more[name], 0o644})
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		// Owned by nobody this machine knows, as a bundle built elsewhere is.
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.text)), Uid: 54321, Gid: 54321}); err != nil {
			t.Fatal(err)
		}
		io.WriteString(tw, f.text)
	}
	tw.Close()
	gz.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, bundleName)
	write(t, file, buf.String(), 0o644)
	k.sign(t, file)
}

// release serves Lab releases as GitHub does, one folder per tag, and counts
// what was asked of it.
type release struct {
	dir string
	srv *httptest.Server
	mu  sync.Mutex
	got []string
}

func newRelease(t *testing.T) *release {
	t.Helper()
	r := &release{dir: t.TempDir()}
	files := http.FileServer(http.Dir(r.dir))
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.got = append(r.got, req.URL.Path)
		r.mu.Unlock()
		files.ServeHTTP(w, req)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *release) from(tag string) string { return r.srv.URL + "/" + tag }

func (r *release) asked() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// labd plays the labd launchd would run: it answers on a loopback port and on
// the socket while the LaunchDaemon is loaded and the build it points at is healthy.
func (m *mini) labd() {
	m.t.Helper()
	state := filepath.Join(filepath.Dir(m.root), "state", "loaded")
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		labd, err := os.ReadFile(m.path("usr/local/lab/current/bin/labd"))
		if !exists(state) || err != nil || bytes.Contains(labd, []byte("unhealthy")) || r.URL.Path != "/healthz" {
			http.Error(w, "no", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	loop := httptest.NewServer(h)
	m.t.Cleanup(loop.Close)
	sock := m.path("var/lab/run/labd.sock")
	if err := os.MkdirAll(filepath.Dir(sock), 0o700); err != nil {
		m.t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		m.t.Fatal(err)
	}
	unix := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	unix.Start()
	m.t.Cleanup(unix.Close)
	m.env = append(m.env, "LAB_LOOPBACK="+strings.TrimPrefix(loop.URL, "http://"))
}

// door plays the Front door: the Lab address as the mini reaches it through
// Cloudflare. It answers as labd does, until it is made dark.
type door struct {
	mu    sync.Mutex
	asked int
	dark  bool
}

func (d *door) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

func (d *door) setDark(dark bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dark = dark
}

func (m *mini) frontDoor() *door {
	m.t.Helper()
	d := &door{}
	state := filepath.Join(filepath.Dir(m.root), "state", "loaded")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.asked++
		dark := d.dark
		d.mu.Unlock()
		labd, err := os.ReadFile(m.path("usr/local/lab/current/bin/labd"))
		if dark || !exists(state) || err != nil || bytes.Contains(labd, []byte("unhealthy")) || r.URL.Path != "/healthz" {
			http.Error(w, "no", http.StatusBadGateway)
			return
		}
		io.WriteString(w, "ok\n")
	}))
	m.t.Cleanup(srv.Close)
	m.env = append(m.env, "LAB_FRONT_DOOR="+srv.URL)
	return d
}

// tunnelLoaded reports whether cloudflared's LaunchDaemon is loaded.
func (m *mini) tunnelLoaded() bool {
	return exists(filepath.Join(filepath.Dir(m.root), "state", "loaded."+cloudflaredLabel))
}

// pinned puts k where the set-up pins the release key.
func (m *mini) pinned(k key) {
	m.t.Helper()
	write(m.t, m.path("usr/local/lab/allowed_signers"), read(m.t, k.signers), 0o644)
	if err := os.MkdirAll(m.path("usr/local/lab/builds"), 0o755); err != nil {
		m.t.Fatal(err)
	}
}
