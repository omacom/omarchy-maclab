package minitest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// ops plays labd's ops socket: it writes each request into the log the
// stand-ins write, so that a test reads one order of events, keeps the lines
// for "Agents", and answers the drain and the parts as the test says.
type ops struct {
	mu sync.Mutex
	// drain is what POST /drain answers, one after the other; the last one
	// stays. None is a labd with nothing running.
	drain []string
	parts string
	lines []string
	// old is a labd from before the drain: it has the lines only.
	old bool
	// drainStatus, when set, is the status POST /drain answers with. Parts
	// of "500" are a GET /parts that answers that status.
	drainStatus int
	// onLift, when set, runs before DELETE /drain is answered.
	onLift func()
	// busy lines, those that start so, are answered 503 that many times
	// first, as by a labd that has just started again.
	busy      string
	busyTimes int
}

func (o *ops) told() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string{}, o.lines...)
}

func (o *ops) set(f func(*ops)) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f(o)
}

func (m *mini) ops() *ops {
	m.t.Helper()
	o := &ops{parts: "builder none\ncontroller none\n"}
	for _, d := range []string{"var/lab/run", "var/lab/log", "var/lab/data"} {
		if err := os.MkdirAll(m.path(d), 0o700); err != nil {
			m.t.Fatal(err)
		}
	}
	ln, err := net.Listen("unix", m.path("var/lab/run/ops.sock"))
	if err != nil {
		m.t.Fatal(err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		o.mu.Lock()
		defer o.mu.Unlock()
		f, err := os.OpenFile(m.log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "ops %s %s %s\n", r.Method, r.URL.Path, body)
			f.Close()
		}
		switch r.Method + " " + r.URL.Path {
		case "POST /agents/line":
			if o.busy != "" && o.busyTimes > 0 && strings.HasPrefix(string(body), o.busy) {
				o.busyTimes--
				http.Error(w, "not yet", http.StatusServiceUnavailable)
				return
			}
			o.lines = append(o.lines, string(body))
			io.WriteString(w, "recorded\n")
		case "POST /drain":
			if o.old {
				http.NotFound(w, r)
				return
			}
			if o.drainStatus != 0 {
				http.Error(w, "drain broke", o.drainStatus)
				return
			}
			answer := "0 0"
			if len(o.drain) > 0 {
				answer = o.drain[0]
				if len(o.drain) > 1 {
					o.drain = o.drain[1:]
				}
			}
			cutShort(w, answer+"\n")
		case "DELETE /drain":
			if o.onLift != nil {
				o.onLift()
			}
			io.WriteString(w, "lifted\n")
		case "GET /parts":
			if o.old {
				http.NotFound(w, r)
				return
			}
			if o.parts == "500" {
				http.Error(w, "parts broke", http.StatusInternalServerError)
				return
			}
			cutShort(w, o.parts)
		default:
			http.NotFound(w, r)
		}
	})}}
	srv.Start()
	m.t.Cleanup(srv.Close)
	m.env = append(m.env, "LAB_DRAIN_STEP=0", "LAB_PARTS_TRIES=2")
	return o
}

// cutShort writes an answer; one that starts "cut:" is the rest of it, cut
// off before the length it said it had.
func cutShort(w http.ResponseWriter, answer string) {
	if rest, ok := strings.CutPrefix(answer, "cut:"); ok {
		w.Header().Set("Content-Length", fmt.Sprint(len(rest)+100))
		io.WriteString(w, rest)
		return
	}
	io.WriteString(w, answer)
}

// load marks a daemon as one the set-up has started.
func (m *mini) load(label string) {
	m.t.Helper()
	write(m.t, filepath.Join(filepath.Dir(m.root), "state", "loaded."+label), "", 0o644)
}

func (m *mini) sql(db, statement string) string {
	m.t.Helper()
	out, err := exec.Command("sqlite3", m.path("var/lab/data/"+db), statement).CombinedOutput()
	if err != nil {
		m.t.Fatalf("sqlite3 %s %q: %v\n%s", db, statement, err, out)
	}
	return strings.TrimSpace(string(out))
}

// databases gives the mini both databases, each with one row.
func (m *mini) databases() {
	m.t.Helper()
	need(m.t, "sqlite3")
	if err := os.MkdirAll(m.path("var/lab/data"), 0o700); err != nil {
		m.t.Fatal(err)
	}
	for _, db := range []string{"labd.db", "reports.db"} {
		m.sql(db, "PRAGMA journal_mode=WAL; CREATE TABLE seen(what TEXT); INSERT INTO seen VALUES('before');")
	}
}

// onSwitch runs a line of shell at the moment a Deploy installs tag's
// LaunchDaemon file: labd of the old build is stopped, the databases are
// copied, and tag's labd is about to start.
func (m *mini) onSwitch(tag, shell string) {
	m.t.Helper()
	write(m.t, m.bin+"/cp", `#!/bin/bash
if [[ "$*" == *"/`+tag+`/deploy/`+labdLabel+`.plist "* ]]; then
  `+shell+`
fi
exec /bin/cp "$@"
`, 0o755)
}

// order fails unless each of want is in the log, after the one before it.
func order(t *testing.T, log string, want ...string) {
	t.Helper()
	at := 0
	for _, w := range want {
		i := strings.Index(log[at:], w)
		if i < 0 {
			t.Fatalf("no %q after %q in:\n%s", w, log[:at], log)
		}
		at += i + len(w)
	}
}

// noWayBack adds deploy/no-way-back to a bundle already written, as a
// release that changes a database beyond adding to it carries it.
func noWayBack(t *testing.T, k key, dir, tag string, healthy bool) {
	t.Helper()
	work := t.TempDir()
	if out, err := exec.Command("tar", "-xzf", filepath.Join(dir, bundleName), "-C", work).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	write(t, work+"/lab-server/deploy/no-way-back", "This release rewrites a table.\n", 0o644)
	if out, err := exec.Command("tar", "-czf", filepath.Join(dir, bundleName), "-C", work, "lab-server").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	k.sign(t, filepath.Join(dir, bundleName))
}

func TestDeployGoesThroughItsStepsInOrder(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.databases()
	d := m.frontDoor()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	m.ok("deploy", "--from", r.from(first), first)
	// The set-up has since started the tunnel, the controller and the Builder,
	// and the tools are in their copy.
	write(t, m.path("Library/LaunchDaemons/"+cloudflaredLabel+".plist"), "<plist/>\n", 0o644)
	m.load(cloudflaredLabel)
	m.load(oobdLabel)
	m.load(builderLabel)
	write(t, m.path("usr/local/lab/tools/20261101T000000Z.abcdef/cloudflared/bin/cloudflared"), "cloudflared 2026.10.1\n", 0o755)
	if err := os.Symlink("20261101T000000Z.abcdef", m.path("usr/local/lab/tools/current")); err != nil {
		t.Fatal(err)
	}
	m.sql("labd.db", "INSERT INTO seen VALUES('a job that ended')")
	o.set(func(o *ops) {
		o.drain = []string{"1 1", "1 0", "0 0"}
		o.lines = nil
		o.parts = "builder ok\ncontroller ok\n"
	})
	os.Remove(m.log)
	asked := d.count()

	out := m.ok("deploy", "--from", r.from(tag), tag)

	labd := m.path("Library/LaunchDaemons/" + labdLabel + ".plist")
	order(t, m.calls(),
		"ops POST /agents/line Deploy of "+tag+" on the mini: started, from "+first,
		"ops POST /drain", "ops POST /drain", "ops POST /drain",
		"launchctl bootout system/"+labdLabel,
		"as-lab sqlite3 "+m.path("var/lab/data/labd.db"),
		"as-lab sqlite3 "+m.path("var/lab/data/reports.db"),
		"launchctl bootstrap system "+labd,
		"launchctl kickstart -k system/"+oobdLabel,
		"launchctl kickstart -k system/"+builderLabel,
		"ops GET /parts",
		"ops POST /agents/line Deployed "+tag+" on the mini",
	)
	if m.current() != tag || !m.loaded() {
		t.Fatalf("the mini runs %q, labd loaded %v\n%s", m.current(), m.loaded(), out)
	}
	if d.count() == asked {
		t.Errorf("the Front door was not asked:\n%s", out)
	}
	// The copy is of the databases as they were with labd stopped.
	for _, db := range []string{"labd.db", "reports.db"} {
		copied := m.path("var/lab/before-deploy/" + tag + "/" + db)
		got, err := exec.Command("sqlite3", copied, "SELECT group_concat(what, ', ') FROM seen").CombinedOutput()
		want := "before"
		if db == "labd.db" {
			want = "before, a job that ended"
		}
		if err != nil || strings.TrimSpace(string(got)) != want {
			t.Errorf("the copy of %s holds %q (%v), want %q", db, got, err, want)
		}
	}
	if got := o.told(); len(got) != 2 || !strings.Contains(got[1], "the Builder and the Out-of-band controller") || !strings.Contains(got[1], first+" is kept as the way back") {
		t.Errorf("\"Agents\" was told %q", got)
	}
	// A Deploy restarts the lab's services: not the mini, and no tool.
	calls := m.calls()
	for _, never := range []string{"brew", "reboot", "shutdown", "fdesetup", "softwareupdate", "installer", "launchctl reboot", "tools/", "DELETE /drain",
		"system/" + cloudflaredLabel + "\n", "kickstart -k system/" + signalLabel, "kickstart -k system/" + tailscaledLabel, "kickstart -k system/" + litestreamLabel} {
		if strings.Contains(calls, never) {
			t.Errorf("a Deploy's calls hold %q:\n%s", never, calls)
		}
	}
	if got := read(t, m.path("usr/local/lab/tools/current/cloudflared/bin/cloudflared")); got != "cloudflared 2026.10.1\n" {
		t.Errorf("the tools' copy changed: %q", got)
	}
	if entries, _ := os.ReadDir(m.path("usr/local/lab/tools")); len(entries) != 2 {
		t.Errorf("the tools folder holds %d entries after a Deploy", len(entries))
	}
}

func TestDeployStopsWhenJobsStillRunAfterItsWait(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	m.ok("deploy", "--from", r.from(first), first)
	o.set(func(o *ops) { o.drain = []string{"2 1"}; o.lines = nil })
	os.Remove(m.log)

	out := m.refused("deploy", "--wait", "0", "--from", r.from(tag), tag)

	if m.current() != first || !m.loaded() || !reflect.DeepEqual(m.builds(), []string{first}) {
		t.Fatalf("the mini runs %q (loaded %v), builds %v\n%s", m.current(), m.loaded(), m.builds(), out)
	}
	if strings.Contains(m.calls(), "launchctl boot") {
		t.Errorf("a service was touched:\n%s", m.calls())
	}
	order(t, m.calls(), "ops POST /drain", "ops DELETE /drain")
	if !strings.Contains(out, "2 jobs and 1 builds still ran after 0 minutes") {
		t.Errorf("no word of what still ran:\n%s", out)
	}
	if got := o.told(); len(got) != 2 || !strings.Contains(got[1], "did not go on") || !strings.Contains(got[1], "2 jobs and 1 builds") {
		t.Errorf("\"Agents\" was told %q", got)
	}
	if exists(m.path("usr/local/lab/.deploying")) {
		t.Error("the Deploy's lock was left behind")
	}
}

func TestDeployOverALabdThatCannotDrainGoesOn(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	// The first Deploy finds no labd to drain.
	if out := m.ok("deploy", "--from", r.from(first), first); !strings.Contains(out, "labd does not run: there is no job to drain") {
		t.Errorf("a first Deploy:\n%s", out)
	}
	o.set(func(o *ops) { o.old = true })

	out := m.ok("deploy", "--from", r.from(tag), tag)

	if m.current() != tag || !strings.Contains(out, "does not drain jobs") {
		t.Errorf("the mini runs %q\n%s", m.current(), out)
	}
}

// Only a labd from before the drain, which answers 404, lets Deploy go on
// without one. Any other failure of /drain stops it before labd is stopped,
// with the drain lifted.
func TestDeployStopsWhenTheDrainFails(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		answer string
	}{
		{"a 500", http.StatusInternalServerError, ""},
		{"an answer it does not understand", 0, "draining, sort of"},
		{"counts with more after them", 0, "0 0\nand more"},
		{"counts cut short", 0, "cut:0 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, k, r := deployed(t)
			o := m.ops()
			first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
			bundle(t, k, filepath.Join(r.dir, first), first, true)
			bundle(t, k, filepath.Join(r.dir, tag), tag, true)
			m.ok("deploy", "--from", r.from(first), first)
			o.set(func(o *ops) {
				o.lines = nil
				o.drainStatus = c.status
				if c.answer != "" {
					o.drain = []string{c.answer}
				}
			})
			os.Remove(m.log)

			out := m.refused("deploy", "--from", r.from(tag), tag)

			if m.current() != first || !m.loaded() || !reflect.DeepEqual(m.builds(), []string{first}) {
				t.Fatalf("the mini runs %q (loaded %v), builds %v\n%s", m.current(), m.loaded(), m.builds(), out)
			}
			if strings.Contains(m.calls(), "launchctl boot") {
				t.Errorf("a service was touched:\n%s", m.calls())
			}
			if !strings.Contains(out, "drain") || !strings.Contains(out, "Nothing was switched") {
				t.Errorf("no word of the drain:\n%s", out)
			}
			if got := o.told(); len(got) != 2 || !strings.Contains(got[1], "did not go on") {
				t.Errorf("\"Agents\" was told %q", got)
			}
		})
	}
}

// withOpsSocket rewrites a bundle already written so that its labd's
// LaunchDaemon file starts it with an ops socket, as every build since
// Deploy in full does.
func withOpsSocket(t *testing.T, k key, dir, tag string) {
	t.Helper()
	work := t.TempDir()
	if out, err := exec.Command("tar", "-xzf", filepath.Join(dir, bundleName), "-C", work).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	write(t, work+"/lab-server/deploy/"+labdLabel+".plist", "<plist><!-- "+tag+" --><string>--ops-socket</string><string>/var/lab/run/ops.sock</string></plist>\n", 0o664)
	if out, err := exec.Command("tar", "-czf", filepath.Join(dir, bundleName), "-C", work, "lab-server").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	k.sign(t, filepath.Join(dir, bundleName))
}

// A labd started with an ops socket that has none is not taken for one too
// old to have it: its parts fail Deploy's check.
func TestALabdThatShouldHaveAnOpsSocketAndHasNoneFailsDeploy(t *testing.T) {
	t.Run("the check after the switch", func(t *testing.T) {
		m, k, r := deployed(t)
		m.env = append(m.env, "LAB_PARTS_TRIES=2")
		first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
		bundle(t, k, filepath.Join(r.dir, first), first, true)
		bundle(t, k, filepath.Join(r.dir, tag), tag, true)
		withOpsSocket(t, k, filepath.Join(r.dir, tag), tag)
		m.ok("deploy", "--from", r.from(first), first)

		out := m.refused("deploy", "--from", r.from(tag), tag)

		if m.current() != first || !m.loaded() {
			t.Fatalf("the mini runs %q (loaded %v)\n%s", m.current(), m.loaded(), out)
		}
		if !strings.Contains(out, "no ops socket") || !strings.Contains(out, first+" is back and answers") {
			t.Errorf("no word of the ops socket:\n%s", out)
		}
	})
	t.Run("the drain before labd stops", func(t *testing.T) {
		m, k, r := deployed(t)
		o := m.ops()
		first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
		bundle(t, k, filepath.Join(r.dir, first), first, true)
		withOpsSocket(t, k, filepath.Join(r.dir, first), first)
		bundle(t, k, filepath.Join(r.dir, tag), tag, true)
		m.ok("deploy", "--from", r.from(first), first)
		o.set(func(o *ops) { o.lines = nil })
		if err := os.Remove(m.path("var/lab/run/ops.sock")); err != nil {
			t.Fatal(err)
		}
		os.Remove(m.log)

		out := m.refused("deploy", "--from", r.from(tag), tag)

		if m.current() != first || !m.loaded() || !reflect.DeepEqual(m.builds(), []string{first}) {
			t.Fatalf("the mini runs %q (loaded %v), builds %v\n%s", m.current(), m.loaded(), m.builds(), out)
		}
		// Only the stand-in's own lines: the as-lab lines carry what "Agents" is told.
		if regexp.MustCompile(`(?m)^launchctl boot`).MatchString(m.calls()) {
			t.Errorf("a service was touched:\n%s", m.calls())
		}
		if !strings.Contains(out, "no ops socket") || !strings.Contains(out, "Nothing was switched") || !strings.Contains(out, "launchctl bootout system/"+labdLabel) {
			t.Errorf("no word of the ops socket, or of the way past it:\n%s", out)
		}
	})
}

func TestDeployGoesBackWhenTheBuilderOrTheControllerIsNotWell(t *testing.T) {
	for _, c := range []struct{ parts, word string }{
		{"builder silent\ncontroller ok\n", "the Builder has not asked labd for work"},
		{"builder ok\ncontroller bad\n", "the Out-of-band controller does not answer labd"},
		{"500", "labd did not say how the Builder and the Out-of-band controller are"},
		{"all is well\n", "labd did not say how the Builder and the Out-of-band controller are"},
		{"builder ok\n", "labd did not say how the Builder and the Out-of-band controller are"},
		{"builder ok\ncontroller ok\nbuilder silent\n", "labd did not say how the Builder and the Out-of-band controller are"},
		{"cut:builder ok\ncontroller ok\n", "labd did not say how the Builder and the Out-of-band controller are"},
	} {
		t.Run(c.word, func(t *testing.T) {
			m, k, r := deployed(t)
			o := m.ops()
			first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
			bundle(t, k, filepath.Join(r.dir, first), first, true)
			bundle(t, k, filepath.Join(r.dir, tag), tag, true)
			m.ok("deploy", "--from", r.from(first), first)
			// The part is unwell under the new build only.
			m.onSwitch(tag, `touch "$STUB_STATE/unwell"`)
			m.onSwitchBack(first, `rm -f "$STUB_STATE/unwell"`)
			o.set(func(o *ops) { o.lines = nil })
			stop := m.partsFollow(o, c.parts)
			defer stop()

			out := m.refused("deploy", "--from", r.from(tag), tag)

			if m.current() != first || !m.loaded() {
				t.Fatalf("the mini runs %q (loaded %v)\n%s", m.current(), m.loaded(), out)
			}
			if !strings.Contains(out, c.word) || !strings.Contains(out, first+" is back and answers") {
				t.Errorf("no word of why:\n%s", out)
			}
			if got := o.told(); len(got) != 2 || !strings.Contains(got[1], "failed its check: "+first+" is back and answers") {
				t.Errorf("\"Agents\" was told %q", got)
			}
		})
	}
}

// onSwitchBack is onSwitch for the build put back, kept beside it.
func (m *mini) onSwitchBack(tag, shell string) {
	m.t.Helper()
	was := read(m.t, m.bin+"/cp")
	i := strings.Index(was, "exec /bin/cp")
	write(m.t, m.bin+"/cp", was[:i]+`if [[ "$*" == *"/`+tag+`/deploy/`+labdLabel+`.plist "* ]]; then
  `+shell+`
fi
`+was[i:], 0o755)
}

// partsFollow has the made-up labd answer unwell while the file a switch
// leaves is there, and well otherwise.
func (m *mini) partsFollow(o *ops, unwell string) (stop func()) {
	m.t.Helper()
	mark := filepath.Join(filepath.Dir(m.root), "state", "unwell")
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			parts := "builder ok\ncontroller ok\n"
			if exists(mark) {
				parts = unwell
			}
			o.set(func(o *ops) { o.parts = parts })
		}
	}()
	return func() { close(done); wg.Wait() }
}

func TestGoingBackFromANoWayBackReleaseRestoresTheDatabases(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.databases()
	first, bad := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)
	noWayBack(t, k, filepath.Join(r.dir, bad), bad, false)
	m.ok("deploy", "--from", r.from(first), first)
	m.load(litestreamLabel)
	write(t, m.path("Library/LaunchDaemons/"+litestreamLabel+".plist"), "<plist/>\n", 0o644)
	write(t, m.path("var/lab/data/.labd.db-litestream/generation"), "abc\n", 0o600)
	// What the new release does to the databases once it has them.
	m.onSwitch(bad, `sqlite3 "$LAB_SYSROOT/var/lab/data/labd.db" "INSERT INTO seen VALUES('after')"; sqlite3 "$LAB_SYSROOT/var/lab/data/reports.db" "DROP TABLE seen"`)
	o.set(func(o *ops) { o.lines = nil })
	os.Remove(m.log)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != first || !m.loaded() {
		t.Fatalf("the mini runs %q (loaded %v)\n%s", m.current(), m.loaded(), out)
	}
	for _, db := range []string{"labd.db", "reports.db"} {
		if got := m.sql(db, "SELECT group_concat(what, ', ') FROM seen"); got != "before" {
			t.Errorf("%s holds %q after the way back, want what it held before the Deploy\n%s", db, got, out)
		}
	}
	if exists(m.path("var/lab/data/.labd.db-litestream")) {
		t.Error("Litestream's own files of the database that was replaced were kept")
	}
	order(t, m.calls(),
		"launchctl bootout system/"+litestreamLabel,
		"launchctl bootstrap system "+m.path("Library/LaunchDaemons/"+litestreamLabel+".plist"),
		"launchctl bootstrap system "+m.path("Library/LaunchDaemons/"+labdLabel+".plist"),
	)
	if !strings.Contains(out, `"No way back."`) {
		t.Errorf("no word of why the databases went back:\n%s", out)
	}
	if got := o.told(); len(got) != 2 || !strings.Contains(got[1], "what arrived since is lost") {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// A release that only adds to a database leaves it alone on the way back:
// the previous build reads it as it is.
func TestGoingBackFromAnOrdinaryReleaseKeepsWhatArrived(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	first, bad := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)
	m.ok("deploy", "--from", r.from(first), first)
	m.onSwitch(bad, `sqlite3 "$LAB_SYSROOT/var/lab/data/labd.db" "INSERT INTO seen VALUES('after')"`)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if got := m.sql("labd.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before, after" {
		t.Errorf("labd.db holds %q\n%s", got, out)
	}
	if strings.Contains(out, "No way back") {
		t.Errorf("the databases went back:\n%s", out)
	}
}

// The way back from a release that says "No way back." is also a Deploy of
// an older version by hand: that, too, puts the databases back as they were
// before the marked release, and what arrived since is lost.
func TestDeployingAnOlderVersionOverANoWayBackReleaseRestoresTheDatabases(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.databases()
	old, marked := "v0.2.0", "v0.3.0"
	bundle(t, k, filepath.Join(r.dir, old), old, true)
	bundle(t, k, filepath.Join(r.dir, marked), marked, true)
	noWayBack(t, k, filepath.Join(r.dir, marked), marked, true)
	m.ok("deploy", "--from", r.from(old), old)
	m.ok("deploy", "--from", r.from(marked), marked)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")
	m.sql("reports.db", "DROP TABLE seen; CREATE TABLE rewritten(x)")
	o.set(func(o *ops) { o.lines = nil })

	out := m.ok("deploy", "--from", r.from(old), old)

	if m.current() != old || !m.loaded() {
		t.Fatalf("the mini runs %q (loaded %v)\n%s", m.current(), m.loaded(), out)
	}
	for _, db := range []string{"labd.db", "reports.db"} {
		if got := m.sql(db, "SELECT group_concat(what, ', ') FROM seen"); got != "before" {
			t.Errorf("%s holds %q, want what it held before %s\n%s", db, got, marked, out)
		}
	}
	order(t, m.calls(), "launchctl bootout system/"+labdLabel, "launchctl bootstrap system "+m.path("Library/LaunchDaemons/"+labdLabel+".plist"))
	if !strings.Contains(out, marked+` says "No way back."`) {
		t.Errorf("no word of why the databases went back:\n%s", out)
	}
	if got := strings.Join(o.told(), "\n"); !strings.Contains(got, "as they were before "+marked) || !strings.Contains(got, "what arrived since is lost") {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// An older version Deployed over a marked release that fails its check puts
// the marked release back on the databases it had, not on the older ones.
func TestAnOlderVersionThatFailsOverANoWayBackReleaseLeavesItItsDatabases(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	old, marked, bad := "v0.2.0", "v0.3.0", "v0.2.1"
	bundle(t, k, filepath.Join(r.dir, old), old, true)
	bundle(t, k, filepath.Join(r.dir, marked), marked, true)
	noWayBack(t, k, filepath.Join(r.dir, marked), marked, true)
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)
	m.ok("deploy", "--from", r.from(old), old)
	m.ok("deploy", "--from", r.from(marked), marked)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != marked || !m.loaded() {
		t.Fatalf("the mini runs %q (loaded %v)\n%s", m.current(), m.loaded(), out)
	}
	if got := m.sql("labd.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before, after" {
		t.Errorf("labd.db holds %q, want what %s had\n%s", got, marked, out)
	}
}

// Going forward from a marked release is no way back: nothing is restored.
func TestDeployingANewerVersionOverANoWayBackReleaseKeepsTheDatabases(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	old, marked, newer := "v0.2.0", "v0.3.0", "v0.4.0"
	for _, tag := range []string{old, marked, newer} {
		bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	}
	noWayBack(t, k, filepath.Join(r.dir, marked), marked, true)
	m.ok("deploy", "--from", r.from(old), old)
	m.ok("deploy", "--from", r.from(marked), marked)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")

	out := m.ok("deploy", "--from", r.from(newer), newer)

	if got := m.sql("labd.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before, after" {
		t.Errorf("labd.db holds %q\n%s", got, out)
	}
	if strings.Contains(out, "No way back") {
		t.Errorf("the databases went back:\n%s", out)
	}
}

// An rc of the marked version is older than it.
func TestAReleaseCandidateIsOlderThanItsRelease(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	rc, marked := "v0.3.0-rc.2", "v0.3.0"
	bundle(t, k, filepath.Join(r.dir, rc), rc, true)
	bundle(t, k, filepath.Join(r.dir, marked), marked, true)
	noWayBack(t, k, filepath.Join(r.dir, marked), marked, true)
	m.ok("deploy", "--from", r.from(rc), rc)
	m.ok("deploy", "--from", r.from(marked), marked)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")

	out := m.ok("deploy", "--from", r.from(rc), rc)

	if got := m.sql("labd.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before" {
		t.Errorf("labd.db holds %q\n%s", got, out)
	}
}

func TestADeployDryRunOverANoWayBackReleaseNamesTheRestore(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.databases()
	old, marked := "v0.2.0", "v0.3.0"
	bundle(t, k, filepath.Join(r.dir, old), old, true)
	bundle(t, k, filepath.Join(r.dir, marked), marked, true)
	noWayBack(t, k, filepath.Join(r.dir, marked), marked, true)
	m.ok("deploy", "--from", r.from(old), old)
	m.ok("deploy", "--from", r.from(marked), marked)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")
	o.set(func(o *ops) { o.lines = nil })

	out := m.ok("deploy", "--dry-run", "--from", r.from(old), old)

	if !strings.Contains(out, "would put the databases back as they were before "+marked) {
		t.Errorf("the dry run doesn't name the restore:\n%s", out)
	}
	if got := m.sql("labd.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before, after" || m.current() != marked || len(o.told()) != 0 {
		t.Errorf("a dry run changed something: labd.db %q, running %q, told %q", got, m.current(), o.told())
	}
}

func TestADeployThatCannotCopyTheDatabasesPutsThePreviousBuildBack(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	m.ok("deploy", "--from", r.from(first), first)
	write(t, m.path("var/lab/data/reports.db"), "not a database\n", 0o600)

	out := m.refused("deploy", "--from", r.from(tag), tag)

	if m.current() != first || !m.loaded() || !reflect.DeepEqual(m.builds(), []string{first}) {
		t.Fatalf("the mini runs %q (loaded %v), builds %v\n%s", m.current(), m.loaded(), m.builds(), out)
	}
	if !strings.Contains(out, "reports.db could not be copied") || !strings.Contains(out, first+" is back and answers") {
		t.Errorf("no word of what happened:\n%s", out)
	}
}

func TestTheCopiesOfTheDatabasesGoWithTheirBuilds(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	tags := []string{"v0.2.0-rc.1", "v0.2.0-rc.2", "v0.2.0-rc.3", "v0.2.0"}
	for _, tag := range tags {
		bundle(t, k, filepath.Join(r.dir, tag), tag, true)
		m.ok("deploy", "--from", r.from(tag), tag)
	}
	entries, _ := os.ReadDir(m.path("var/lab/before-deploy"))
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if want := []string{"v0.2.0", "v0.2.0-rc.2", "v0.2.0-rc.3"}; !reflect.DeepEqual(got, want) {
		t.Errorf("copies kept: %v, want %v", got, want)
	}
	if got := mode(t, m.path("var/lab/before-deploy")); got != 0o700 {
		t.Errorf("the folder of the copies is %v", got)
	}
}

func TestDeploysDryRunNamesEveryStep(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	noWayBack(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.ok("deploy", "--dry-run", "--from", r.from(tag), tag)

	for _, want := range []string{"would tell \"Agents\" and drain jobs", "would stop labd and copy labd.db reports.db", "would switch", "never the mini, and no tool", "the Builder asks it for work", `says "No way back."`} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run doesn't say %q:\n%s", want, out)
		}
	}
	if len(o.told()) != 0 || strings.Contains(m.calls(), "ops ") || exists(m.path("var/lab/before-deploy")) {
		t.Errorf("a dry run told \"Agents\", drained or copied:\n%s", m.calls())
	}
}

// What a Deploy says reaches "Agents" through a real labd's ops socket.
func TestDeployTellsAgentsThroughLabd(t *testing.T) {
	m, k, r := deployed(t)
	a, _ := m.withLabd()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	m.ok("deploy", "--from", r.from(first), first)
	a.posted(t, 2)

	out := m.ok("deploy", "--from", r.from(tag), tag)

	got := a.posted(t, 4)
	if got[2] != "Deploy of "+tag+" on the mini: started, from "+first+". Jobs are drained, then the lab's services restart; the mini does not." {
		t.Errorf("the line of the start: %q\n%s", got[2], out)
	}
	if !strings.HasPrefix(got[3], "Deployed "+tag+" on the mini: labd answers on its loopback port and its socket") {
		t.Errorf("the report: %q", got[3])
	}
	if !strings.Contains(out, "jobs are drained") {
		t.Errorf("a real labd was not asked to drain:\n%s", out)
	}
}

// With no labd to take them, a Deploy's lines are in notify.log, written
// by the lab account, as Deploy says they are.
func TestADeploysLinesThatCannotGoAreInNotifyLog(t *testing.T) {
	m, k, r := deployed(t)
	if err := os.MkdirAll(m.path("var/lab/log"), 0o700); err != nil {
		t.Fatal(err)
	}
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.ok("deploy", "--from", r.from(tag), tag)

	log := m.notified()
	for _, want := range []string{" agents Deploy of " + tag + " on the mini: started.", " agents Deployed " + tag + " on the mini: labd answers"} {
		if !strings.Contains(log, want) {
			t.Errorf("notify.log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(out, `that line did not reach "Agents": it is in `+m.path("var/lab/log/notify.log")) {
		t.Errorf("Deploy did not say where the lines are:\n%s", out)
	}
}

// The labd just started may not take a line at once: the report waits a
// moment for it.
func TestTheDeployedLineWaitsForTheNewLabd(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	m.ok("deploy", "--from", r.from(first), first)
	o.set(func(o *ops) { o.lines, o.busy, o.busyTimes = nil, "Deployed ", 2 })
	m.env = append(m.env, "LAB_TELL_TRIES=5")

	out := m.ok("deploy", "--from", r.from(tag), tag)

	got := o.told()
	if len(got) != 2 || !strings.HasPrefix(got[0], "Deploy of "+tag+" on the mini: started") || !strings.HasPrefix(got[1], "Deployed "+tag) {
		t.Errorf("\"Agents\" was told %q\n%s", got, out)
	}
	if strings.Contains(out, "did not reach") {
		t.Errorf("a line was said not to reach \"Agents\":\n%s", out)
	}
}

func TestDeployRefusesALinkPlantedWhereTheCopiesGo(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	first, tag := "v0.2.0-rc.1", "v0.2.0-rc.2"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)
	m.ok("deploy", "--from", r.from(first), first)
	v := m.victim()
	link := m.path("var/lab/before-deploy")
	plant(t, link, v.dir)
	os.Remove(m.log)

	out := m.refused("deploy", "--from", r.from(tag), tag)

	saysLink(t, out, link)
	v.intact("a Deploy with a link where the databases' copies go")
	if m.current() != first || !m.loaded() || strings.Contains(m.calls(), "launchctl boot") {
		t.Errorf("the mini runs %q (loaded %v) after the refusal:\n%s", m.current(), m.loaded(), m.calls())
	}
}

// controllerFollows has the made-up labd, which holds the controller's
// token, say the controller answers while launchd has it, and not otherwise.
func (m *mini) controllerFollows(o *ops) (stop func()) {
	m.t.Helper()
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			parts := "builder none\ncontroller bad\n"
			if m.oobdLoaded() {
				parts = "builder none\ncontroller ok\n"
			}
			o.set(func(o *ops) { o.parts = parts })
		}
	}()
	return func() { close(done); wg.Wait() }
}

// installedController is the mini as the set-up leaves it before the first
// Deploy: the controller's token and LaunchDaemon file in place, the
// controller not started, since no build was there to run it from.
func (m *mini) installedController() {
	m.t.Helper()
	write(m.t, m.path(rootToken), "root's token\n", 0o600)
	write(m.t, m.path(labToken), "root's token\n", 0o600)
	write(m.t, m.path(oobdPlist), "<plist><!-- oobd --></plist>\n", 0o644)
}

func TestAFirstDeployStartsTheControllerTheSetUpInstalled(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.installedController()
	stop := m.controllerFollows(o)
	defer stop()
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.ok("deploy", "--from", r.from(tag), tag)

	if m.current() != tag || !m.loaded() || !m.oobdLoaded() {
		t.Fatalf("the mini runs %q, labd loaded %v, controller loaded %v\n%s", m.current(), m.loaded(), m.oobdLoaded(), out)
	}
	order(t, m.calls(),
		"launchctl bootstrap system "+m.path("Library/LaunchDaemons/"+labdLabel+".plist"),
		"launchctl bootstrap system "+m.path(oobdPlist),
		"ops GET /parts",
	)
	if !strings.Contains(out, "started "+oobdLabel) || !strings.Contains(out, "Deployed "+tag) {
		t.Errorf("no word of the controller's start or the Deploy:\n%s", out)
	}
}

func TestAFirstDeployWhoseControllerCannotStartChangesNothingElse(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.installedController()
	m.bootstrapFails(oobdLabel, "99")
	stop := m.controllerFollows(o)
	defer stop()
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.refused("deploy", "--from", r.from(tag), tag)

	for _, want := range []string{
		"launchd did not start " + oobdLabel,
		"the Out-of-band controller does not answer labd",
		tag + " was not Deployed: there was no build before it, so labd is stopped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q:\n%s", want, out)
		}
	}
	if m.current() != "" || len(m.builds()) != 0 || m.loaded() || m.oobdLoaded() {
		t.Errorf("left behind: current %q, builds %v, labd loaded %v, controller loaded %v\n%s", m.current(), m.builds(), m.loaded(), m.oobdLoaded(), out)
	}
	for p, want := range map[string]string{rootToken: "root's token\n", labToken: "root's token\n", oobdPlist: "<plist><!-- oobd --></plist>\n"} {
		if got := read(t, m.path(p)); got != want {
			t.Errorf("%s is %q, was %q", p, got, want)
		}
	}
}

func TestAFirstDeployThatFailsStopsTheControllerItStarted(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.installedController()
	bad := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != "" || m.loaded() || m.oobdLoaded() {
		t.Errorf("left behind: current %q, labd loaded %v, controller loaded %v\n%s", m.current(), m.loaded(), m.oobdLoaded(), out)
	}
	if !exists(m.path(oobdPlist)) {
		t.Error("the controller's LaunchDaemon file was removed")
	}
}

// A controller that does not stop runs from the build: the build stays, and
// the way back says it is incomplete.
func TestAFirstDeployWhoseControllerDoesNotStopKeepsItsBuild(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.installedController()
	write(t, filepath.Join(filepath.Dir(m.root), "state", "bootout.sticks."+oobdLabel), "", 0o644)
	write(t, m.bin+"/sleep", "#!/bin/sh\n", 0o755)
	bad := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if !m.oobdLoaded() || m.loaded() {
		t.Fatalf("controller loaded %v, labd loaded %v\n%s", m.oobdLoaded(), m.loaded(), out)
	}
	if m.current() != bad || !reflect.DeepEqual(m.builds(), []string{bad}) || !exists(m.path("usr/local/lab/current/bin/labd")) {
		t.Errorf("the controller's build is gone: current %q, builds %v\n%s", m.current(), m.builds(), out)
	}
	if !strings.Contains(out, oobdLabel+" did not stop") || !strings.Contains(out, "the way back is incomplete") {
		t.Errorf("no word of the controller left running:\n%s", out)
	}
}

// marked is a mini that ran old, then the marked release, with Litestream
// running beside labd; labd.db holds a row the marked release wrote.
func marked(t *testing.T) (m *mini, o *ops, r *release, old, mark string) {
	t.Helper()
	var k key
	m, k, r = deployed(t)
	o = m.ops()
	m.databases()
	old, mark = "v0.2.0", "v0.3.0"
	bundle(t, k, filepath.Join(r.dir, old), old, true)
	bundle(t, k, filepath.Join(r.dir, mark), mark, true)
	noWayBack(t, k, filepath.Join(r.dir, mark), mark, true)
	m.ok("deploy", "--from", r.from(old), old)
	m.ok("deploy", "--from", r.from(mark), mark)
	m.load(litestreamLabel)
	write(t, m.path("Library/LaunchDaemons/"+litestreamLabel+".plist"), "<plist/>\n", 0o644)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")
	o.set(func(o *ops) { o.lines = nil })
	os.Remove(m.log)
	return m, o, r, old, mark
}

// theMarkedReleasesDatabases fails unless the marked release runs on what it
// had: labd.db with the row it wrote, reports.db as before.
func theMarkedReleasesDatabases(t *testing.T, m *mini, mark, out string) {
	t.Helper()
	if m.current() != mark || !m.loaded() {
		t.Fatalf("the mini runs %q (loaded %v)\n%s", m.current(), m.loaded(), out)
	}
	if got := m.sql("labd.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before, after" {
		t.Errorf("labd.db holds %q, want what %s had\n%s", got, mark, out)
	}
	if got := m.sql("reports.db", "SELECT group_concat(what, ', ') FROM seen"); got != "before" {
		t.Errorf("reports.db holds %q\n%s", got, out)
	}
	for _, f := range []string{"labd.db.restored", "reports.db.restored"} {
		if exists(m.path("var/lab/data/" + f)) {
			t.Errorf("%s is left beside the databases", f)
		}
	}
}

// With no copy of the databases from before the marked release, an older
// Deploy cannot go back to them: it is refused while labd still runs.
func TestAnOlderDeployOverAMarkedReleaseWithNoCopyIsRefusedWithLabdRunning(t *testing.T) {
	m, o, r, old, mark := marked(t)
	if err := os.RemoveAll(m.path("var/lab/before-deploy/" + mark)); err != nil {
		t.Fatal(err)
	}

	out := m.refused("deploy", "--from", r.from(old), old)

	theMarkedReleasesDatabases(t, m, mark, out)
	if strings.Contains(m.calls(), "launchctl bootout") || strings.Contains(m.calls(), "ops POST /drain") {
		t.Errorf("labd was drained or stopped:\n%s", m.calls())
	}
	if !strings.Contains(out, mark+` says "No way back."`) || !strings.Contains(out, "no copy") {
		t.Errorf("no word of why:\n%s", out)
	}
	if len(o.told()) != 0 {
		t.Errorf("\"Agents\" was told %q", o.told())
	}
}

// A Litestream that does not stop is never pulled from under: the older
// Deploy changes no database, and the marked release runs again.
func TestAStuckLitestreamChangesNoDatabase(t *testing.T) {
	m, _, r, old, mark := marked(t)
	write(t, filepath.Join(filepath.Dir(m.root), "state", "bootout.sticks."+litestreamLabel), "", 0o644)
	write(t, m.bin+"/sleep", "#!/bin/sh\n", 0o755)

	out := m.refused("deploy", "--from", r.from(old), old)

	theMarkedReleasesDatabases(t, m, mark, out)
	if !strings.Contains(out, "Litestream did not stop") || !strings.Contains(out, mark+" is back and answers") {
		t.Errorf("no word of Litestream:\n%s", out)
	}
}

// A disk that fills while the copies are put beside the databases leaves
// them as they were: the marked release is back on its own, and Litestream,
// stopped for the restore, runs again.
func TestAFailingSecondCopyLeavesTheMarkedReleaseUpWithLitestreamStarted(t *testing.T) {
	m, o, r, old, mark := marked(t)
	write(t, m.bin+"/cp", `#!/bin/bash
[[ "$*" != *reports.db.restored* ]] || { echo "cp: No space left on device" >&2; exit 1; }
exec /bin/cp "$@"
`, 0o755)

	out := m.refused("deploy", "--from", r.from(old), old)

	theMarkedReleasesDatabases(t, m, mark, out)
	if !m.daemonLoaded(litestreamLabel) {
		t.Errorf("Litestream was left stopped:\n%s", m.calls())
	}
	if got := strings.Join(o.told(), "\n"); !strings.Contains(got, mark+" is back and answers") || strings.Contains(got, "as they were before "+mark) {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// The way back from an older Deploy that put the databases back needs no
// room: it renames, so a full disk does not leave the lab Dark.
func TestTheWayBackFromAnOlderDeployNeedsNoRoom(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.databases()
	old, mark, bad := "v0.2.0", "v0.3.0", "v0.2.1"
	bundle(t, k, filepath.Join(r.dir, old), old, true)
	bundle(t, k, filepath.Join(r.dir, mark), mark, true)
	noWayBack(t, k, filepath.Join(r.dir, mark), mark, true)
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)
	m.ok("deploy", "--from", r.from(old), old)
	m.ok("deploy", "--from", r.from(mark), mark)
	m.sql("labd.db", "INSERT INTO seen VALUES('after')")
	// The disk is full from the moment the older build starts.
	write(t, m.bin+"/cp", `#!/bin/bash
if [[ "$*" == *"/`+bad+`/deploy/`+labdLabel+`.plist "* ]]; then touch "$STUB_STATE/full"; fi
if [[ -f $STUB_STATE/full && "$*" == *"/var/lab/data/"* ]]; then echo "cp: No space left on device" >&2; exit 1; fi
exec /bin/cp "$@"
`, 0o755)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	theMarkedReleasesDatabases(t, m, mark, out)
	if !strings.Contains(out, mark+" is back and answers") {
		t.Errorf("no word of the way back:\n%s", out)
	}
}

// launchd loads the controller's file at boot: a first Deploy that fails
// stops the controller it found loaded, as one it started, and leaves no
// build behind.
func TestAFirstDeployThatFailsStopsAControllerLoadedAtBoot(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	m.installedController()
	m.load(oobdLabel)
	bad := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != "" || len(m.builds()) != 0 || m.loaded() || m.oobdLoaded() {
		t.Errorf("left behind: current %q, builds %v, labd loaded %v, controller loaded %v\n%s", m.current(), m.builds(), m.loaded(), m.oobdLoaded(), out)
	}
	if !exists(m.path(oobdPlist)) {
		t.Error("the controller's LaunchDaemon file was removed")
	}
}

// The first Deploy on a mini restarted since its set-up: the controller is
// loaded already, and runs from the build once it is there.
func TestAFirstDeployWithTheControllerLoadedAtBootGoesThrough(t *testing.T) {
	m, k, r := deployed(t)
	o := m.ops()
	m.installedController()
	m.load(oobdLabel)
	stop := m.controllerFollows(o)
	defer stop()
	tag := "v0.2.0-rc.1"
	bundle(t, k, filepath.Join(r.dir, tag), tag, true)

	out := m.ok("deploy", "--from", r.from(tag), tag)

	if m.current() != tag || !m.loaded() || !m.oobdLoaded() {
		t.Fatalf("the mini runs %q, labd loaded %v, controller loaded %v\n%s", m.current(), m.loaded(), m.oobdLoaded(), out)
	}
	if !strings.Contains(out, "restarted "+oobdLabel) || !strings.Contains(out, "Deployed "+tag) {
		t.Errorf("no word of the controller's restart or the Deploy:\n%s", out)
	}
}

// A Litestream that launchd does not start again once the databases went
// back fails the Deploy, though the older release runs: "Agents" is told
// that nothing is replicated.
func TestALitestreamThatDoesNotStartAgainFailsTheDeploy(t *testing.T) {
	m, o, r, old, _ := marked(t)
	m.bootstrapFails(litestreamLabel, "99")

	out := m.refused("deploy", "--from", r.from(old), old)

	if m.current() != old || !m.loaded() || m.daemonLoaded(litestreamLabel) {
		t.Fatalf("the mini runs %q (loaded %v), Litestream loaded %v\n%s", m.current(), m.loaded(), m.daemonLoaded(litestreamLabel), out)
	}
	if !strings.Contains(out, "Litestream did not start again") {
		t.Errorf("no word of Litestream:\n%s", out)
	}
	if got := strings.Join(o.told(), "\n"); !strings.Contains(got, "Litestream did not start again") || !strings.Contains(got, "not replicated") {
		t.Errorf("\"Agents\" was told %q", got)
	}
}

// A rename that fails part-way through putting the databases back leaves no
// copy beside them: the way back gives the marked release its own, and the
// copy left would go into the house backup.
func TestAFailingSecondRenameLeavesNoCopyBesideTheDatabases(t *testing.T) {
	m, _, r, old, mark := marked(t)
	write(t, m.bin+"/mv", `#!/bin/bash
[[ "$*" != *reports.db.restored* ]] || { echo "mv: rename failed" >&2; exit 1; }
exec /bin/mv "$@"
`, 0o755)

	out := m.refused("deploy", "--from", r.from(old), old)

	theMarkedReleasesDatabases(t, m, mark, out)
	if !strings.Contains(out, "reports.db could not be put in place") || !strings.Contains(out, mark+" is back and answers") {
		t.Errorf("no word of the rename or the way back:\n%s", out)
	}
	order(t, m.calls(), "ops POST /agents/line Deploy of "+old+" on the mini failed its check", "as-lab rm -f "+m.path("var/lab/data/reports.db.restored"))
}

// Another Deploy that ends while this one downloads changes what runs: the
// build this Deploy goes back to is the one that runs once it holds the lock.
func TestTheWayBackIsWhatRunsOnceTheDeployHoldsTheLock(t *testing.T) {
	m, k, r := deployed(t)
	m.ops()
	first, other, bad := "v0.2.0-rc.1", "v0.2.0-rc.2", "v0.2.0-rc.3"
	bundle(t, k, filepath.Join(r.dir, first), first, true)
	bundle(t, k, filepath.Join(r.dir, other), other, true)
	bundle(t, k, filepath.Join(r.dir, bad), bad, false)
	m.ok("deploy", "--from", r.from(first), first)
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	write(t, m.bin+"/curl", `#!/bin/bash
if [[ "$*" == *"/`+bad+`/"* && ! -f $STUB_STATE/raced ]]; then
  touch "$STUB_STATE/raced"
  `+filepath.Join(m.tree, "scripts", "mini", "deploy")+` --from `+r.from(other)+` `+other+` >/dev/null 2>&1 || exit 9
fi
exec `+curl+` "$@"
`, 0o755)

	out := m.refused("deploy", "--from", r.from(bad), bad)

	if m.current() != other || !m.loaded() {
		t.Fatalf("the mini runs %q (loaded %v), want %s, which ran when the Deploy took the lock\n%s", m.current(), m.loaded(), other, out)
	}
	if !strings.Contains(out, other+" is back and answers") || !strings.Contains(out, "started, from "+other) {
		t.Errorf("no word of %s:\n%s", other, out)
	}
}
