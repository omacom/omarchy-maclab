package carrytest

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/oldlabtest"
	"github.com/omacom/omarchy-maclab/internal/server"
)

// The day's scripts for the owner's Mac (scripts/cutover/day, back,
// confirmed, ready) in a dress run that touches nothing live: the Lab
// address, the old name, Cloudflare's API and GitHub's releases are servers
// on this machine, the mini is the made-up one, reached by a stand-in for
// ssh, and labd is the real one.

const (
	existingApp = "Ov23liyct9clXDWu2d42"
	oldZone     = "abcdef0123456789abcdef0123456789"
	labZone     = "0123456789abcdef0123456789abcdef"
	redirectAt  = "/rulesets/phases/http_request_dynamic_redirect/entrypoint"
)

// daySSH stands in for ssh on the owner's Mac: it writes down what it was
// asked and runs the mini's script on the made-up mini. With DAY_SUDO_ASKS
// sudo's prompt comes back on the terminal as over ssh -t, with no end of
// line until the password is typed; nothing here types one.
const daySSH = `#!/bin/bash
echo "ssh $*" >> "$DAY_SSH_LOG"
[[ $1 == -t ]] && shift
cmd="$2"
[[ $cmd == "sudo ~/lab-cutover/"* ]] || { echo "ssh stand-in: not one of the Cutover's scripts with sudo: $cmd" >&2; exit 64; }
if [[ -n ${DAY_SUDO_ASKS:-} ]]; then
  printf 'Password for owner on the mini:'
  sleep 0.2
  printf '\r\n'
fi
cmd="${cmd#sudo }"
cmd="${cmd/#\~\/lab-cutover/$DAY_REPO/scripts/cutover}"
cmd="${cmd//\~/$DAY_MINI_HOME}"
cmd="${cmd// \/var\/lab\// $LAB_SYSROOT/var/lab/}"
PATH="${PATH#*:}"
exec /bin/bash -c "$cmd"
`

// The Front door's runner, played: it writes the day's list as the real
// one does and ends as the file beside it says.
const runnerStub = `#!/bin/bash
echo "runner $*" >> "$DAY_RUNNER_LOG"
code="$(cat "$DAY_RUNNER_CODE" 2>/dev/null || echo 0)"
if [[ $code == 0 ]]; then
  printf '# The Front door'"'"'s tests\n\nResult: pass, all fourteen\n' > "$DAY_ACCEPTANCE/$(TZ=Australia/Brisbane date +%Y-%m-%d)-front-door.md"
fi
exit "$code"
`

const smokeStub = `#!/bin/bash
echo "smoke SITE=$SITE" >> "$DAY_RUNNER_LOG"
[[ ! -f $DAY_SMOKE_FAILS ]] || { echo "FAIL: the report's page"; exit 1; }
echo "smoke ok"
`

// outside is everything the day's scripts ask that is not the mini.
type outside struct {
	t  *testing.T
	mu sync.Mutex

	asked int // requests of any kind, to anything

	// The Lab address: stage and app until labd is put behind it.
	lab      *httptest.Server
	labd     http.Handler
	stage    string // preview, open or dark, while labd is nil
	app      string
	noFooter bool

	// The old name.
	old      *httptest.Server
	siteUp   bool
	oldAsked []string

	// Cloudflare.
	cloud       *httptest.Server
	token       string
	zoneStatus  string
	certificate bool
	redirect    []map[string]any
	purges      int
	purgeFails  int
	writes      []string

	// GitHub's releases.
	releases  *httptest.Server
	published string
}

func newOutside(t *testing.T) *outside {
	t.Helper()
	o := &outside{t: t, stage: "preview", app: "dev-app-client-id", siteUp: true, token: "tok-made-up-for-the-day", zoneStatus: "pending"}
	count := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			o.mu.Lock()
			o.asked++
			o.mu.Unlock()
			h(w, r)
		}
	}
	o.lab = httptest.NewServer(count(o.serveLab))
	o.old = httptest.NewServer(count(o.serveOld))
	o.cloud = httptest.NewServer(count(o.serveCloud))
	o.releases = httptest.NewServer(count(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if r.Method != "GET" {
			t.Errorf("a script sent %s %s to GitHub", r.Method, r.URL.Path)
		}
		if r.URL.Path != "/latest/download/VERSION" || o.published == "" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, o.published+"\n")
	}))
	t.Cleanup(func() { o.lab.Close(); o.old.Close(); o.cloud.Close(); o.releases.Close() })
	return o
}

func (o *outside) serveLab(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	labd, stage, app, noFooter := o.labd, o.stage, o.app, o.noFooter
	o.mu.Unlock()
	if labd != nil {
		labd.ServeHTTP(w, r)
		return
	}
	if stage == "dark" {
		http.Error(w, "<html><body>error code: 1033</body></html>", 530)
		return
	}
	if stage == "preview" {
		w.Header().Set("X-Robots-Tag", "noindex")
	}
	switch r.URL.Path {
	case "/healthz":
		io.WriteString(w, "ok")
	case "/sign-in":
		w.Header().Set("Location", "https://github.com/login/oauth/authorize?client_id="+app+"&scope=")
		w.WriteHeader(http.StatusSeeOther)
	case "/install":
		io.WriteString(w, "#!/bin/bash\n# https://github.com/omacom/omarchy-maclab/releases/latest/download\n")
	case "/", "/terms", "/privacy":
		if !noFooter {
			io.WriteString(w, `<p><a href="/terms">Terms</a> · <a href="/privacy">Privacy</a></p>`)
		}
	default:
		if strings.HasPrefix(r.URL.Path, "/reports/") {
			io.WriteString(w, "a report's page")
			return
		}
		http.NotFound(w, r)
	}
}

// serveOld is the old name: the old site while its service runs, Railway's
// error page while it is stopped, and the redirect once the rule is on.
func (o *outside) serveOld(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.oldAsked = append(o.oldAsked, r.Method+" "+r.URL.Path)
	switch {
	case len(o.redirect) == 1:
		w.Header().Set("Location", o.lab.URL+r.URL.RequestURI())
		w.WriteHeader(http.StatusPermanentRedirect)
	case o.siteUp:
		io.WriteString(w, "the old site")
	default:
		http.Error(w, "<html><body><h1>Application not found</h1>Request ID: made-up</body></html>", http.StatusNotFound)
	}
}

func (o *outside) serveCloud(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	path := strings.TrimPrefix(r.URL.Path, "/client/v4")
	ok := func(result any) {
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
	}
	if r.Header.Get("Authorization") != "Bearer "+o.token {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 10000, "message": "Authentication error"}}})
		return
	}
	if r.Method != "GET" {
		o.writes = append(o.writes, r.Method+" "+path)
	}
	switch {
	case path == "/zones" && r.Method == "GET" && r.URL.Query().Get("name") == "omarchy-m-testing.org":
		ok([]any{map[string]any{"id": oldZone, "name": "omarchy-m-testing.org", "status": o.zoneStatus, "account": map[string]any{"id": "acct"}}})
	case path == "/zones" && r.Method == "GET" && r.URL.Query().Get("name") == "omarchy-mac.org":
		ok([]any{map[string]any{"id": labZone, "name": "omarchy-mac.org", "status": "active", "account": map[string]any{"id": "acct"}}})
	case path == "/zones/"+oldZone+"/ssl/certificate_packs" && r.Method == "GET":
		packs := []any{}
		if o.certificate {
			packs = append(packs, map[string]any{"status": "active", "hosts": []string{"omarchy-m-testing.org", "*.omarchy-m-testing.org"}})
		}
		ok(packs)
	case path == "/zones/"+oldZone+redirectAt && r.Method == "GET":
		ok(map[string]any{"rules": o.redirect})
	case path == "/zones/"+oldZone+redirectAt && r.Method == "PUT":
		var in struct {
			Rules []map[string]any `json:"rules"`
		}
		json.Unmarshal(body, &in)
		o.redirect = in.Rules
		ok(map[string]any{"rules": o.redirect})
	case path == "/zones/"+labZone+"/purge_cache" && r.Method == "POST":
		if o.purgeFails > 0 {
			o.purgeFails--
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 1000, "message": "try again"}}})
			return
		}
		o.purges++
		ok(map[string]any{"id": labZone})
	default:
		if r.Method != "GET" {
			o.t.Errorf("a script sent %s %s to Cloudflare", r.Method, path)
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 7003, "message": "No route for the URI"}}})
	}
}

func (o *outside) set(change func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	change()
}

// day is the owner's Mac on the day: the scripts' environment, the day's
// record, the dated records and what the stand-ins were asked.
type day struct {
	t          *testing.T
	o          *outside
	m          *mini
	env        []string
	state      string
	acceptance string
	sshLog     string
	runLog     string
	runnerCode string
	smokeFails string
	tokenFile  string
}

func newDay(t *testing.T, m *mini) *day {
	t.Helper()
	o := newOutside(t)
	dir := filepath.Join(m.base, "owner")
	d := &day{t: t, o: o, m: m, state: filepath.Join(dir, "state", "the-day"), acceptance: filepath.Join(dir, "acceptance"),
		sshLog: filepath.Join(dir, "ssh.log"), runLog: filepath.Join(dir, "run.log"), runnerCode: filepath.Join(dir, "runner.code"),
		smokeFails: filepath.Join(dir, "smoke.fails"), tokenFile: filepath.Join(dir, "token")}
	bin := filepath.Join(dir, "bin")
	write(t, filepath.Join(bin, "ssh"), daySSH, 0o755)
	write(t, filepath.Join(dir, "runner"), runnerStub, 0o755)
	write(t, filepath.Join(dir, "smoke"), smokeStub, 0o755)
	write(t, d.tokenFile, o.token+"\n", 0o600)
	if err := os.MkdirAll(d.acceptance, 0o755); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, e := range m.env {
		if strings.HasPrefix(e, "PATH=") {
			e = "PATH=" + bin + ":" + strings.TrimPrefix(e, "PATH=")
		}
		env = append(env, e)
	}
	d.env = append(env,
		"LAB_DAY_ADDRESS="+o.lab.URL, "LAB_DAY_OLD="+o.old.URL, "LAB_DAY_RELEASES="+o.releases.URL,
		"LAB_CLOUD_API="+o.cloud.URL+"/client/v4", "LAB_PORKBUN_API=http://127.0.0.1:1/never-asked",
		"LAB_DAY_RUNNER="+filepath.Join(dir, "runner"), "LAB_DAY_SMOKE="+filepath.Join(dir, "smoke"),
		"DAY_SSH_LOG="+d.sshLog, "DAY_RUNNER_LOG="+d.runLog, "DAY_RUNNER_CODE="+d.runnerCode, "DAY_SMOKE_FAILS="+d.smokeFails,
		"DAY_ACCEPTANCE="+d.acceptance, "DAY_REPO="+repo(t), "DAY_MINI_HOME="+filepath.Join(m.base, "home"),
		"KEYCHAIN_SERVICE=daytest-no-such-service")
	return d
}

// script runs one of the owner's scripts with typed on its standard input.
func (d *day) script(typed, name string, args ...string) (string, int) {
	d.t.Helper()
	cmd := exec.Command(filepath.Join(repo(d.t), "scripts", "cutover", name), args...)
	cmd.Env = d.env
	cmd.Dir = d.m.base
	cmd.Stdin = strings.NewReader(typed)
	// The stand-ins answer on their own goroutines: taking their lock before
	// and after the script orders the test's plain reads and writes of their
	// fields with the script's requests.
	d.o.set(func() {})
	out, err := cmd.CombinedOutput()
	d.o.set(func() {})
	said := strings.ReplaceAll(string(out), d.m.base, "")
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		d.t.Fatalf("%s: %v\n%s", name, err, out)
	}
	d.t.Logf("$ %s %s\n%sexit %d", name, strings.Join(args, " "), said, code)
	if strings.Contains(string(out), d.o.token) {
		d.t.Errorf("%s printed the Cloudflare token", name)
	}
	if regexp.MustCompile(`(?i)<html|<body`).MatchString(string(out)) {
		d.t.Errorf("%s printed a web page", name)
	}
	return string(out), code
}

// run is the day's script, going on with what the test adds.
func (d *day) run(typed string, args ...string) (string, int) {
	d.t.Helper()
	all := append([]string{"--run", "--state", d.state, "--acceptance", d.acceptance, "--mini", "owner@mini.test", "--token-file", d.tokenFile}, args...)
	return d.script(typed, "day", all...)
}

// stops asserts a run that ends with status code and says each of want.
func (d *day) stops(code int, typed string, args []string, want ...string) string {
	d.t.Helper()
	out, got := d.run(typed, args...)
	if got != code {
		d.t.Fatalf("day --run %s: exit %d, want %d\n%s", strings.Join(args, " "), got, code, out)
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			d.t.Fatalf("day --run %s did not say %q:\n%s", strings.Join(args, " "), w, out)
		}
	}
	return out
}

func (d *day) done() string {
	var steps []string
	for _, line := range strings.Split(strings.TrimSpace(read(d.t, d.state)), "\n") {
		if line != "" {
			steps = append(steps, strings.SplitN(line, "\t", 2)[0])
		}
	}
	return strings.Join(steps, " ")
}

func (d *day) record(name, text string) {
	d.t.Helper()
	write(d.t, filepath.Join(d.acceptance, name), text, 0o644)
}

// ready files the nine items' records, all green.
func (d *day) ready() {
	d.t.Helper()
	for _, name := range []string{"front-door", "restore-drill", "dead-man", "unlock-outside", "planned-restart", "grid-switch", "rehearsal-import", "lease", "macs-accepted", "release-candidate"} {
		result := "Result: pass\n"
		if name == "front-door" {
			result = "Result: pass, all fourteen\n"
		}
		d.record("2026-11-10-"+name+".md", "# "+name+"\n\n"+result)
	}
	var oob, hard string
	for i := 1; i <= 14; i++ {
		if i <= 8 {
			oob += fmt.Sprintf("%02d pass 2026-11-10 2026-11-10-out-of-band-%02d.md\n", i, i)
		}
		hard += fmt.Sprintf("%02d pass 2026-11-10 2026-11-10-hardening-%02d.md\n", i, i)
	}
	d.record("out-of-band.results", oob)
	d.record("hardening.results", hard)
	d.record("builder-gate.results", "builder-gate pass 2026-11-10 2026-11-10-builder-gate.md 6\n")
}

// labdBehind starts the real labd on the mini's data directory, in Preview
// with its ops socket, and puts it behind the Lab address.
func (d *day) labdBehind(app string) *server.Server {
	d.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d.t.Cleanup(cancel)
	s, err := server.New(ctx, server.Config{DataDir: d.m.path("var/lab/data"), Preview: true, MachineIDKey: machineIDKey(), Release: "v0.2.0-rc.9",
		GitHub: &server.GitHubApp{ClientID: app, ClientSecret: "secret", WebURL: "http://127.0.0.1:1", APIURL: "http://127.0.0.1:1"}}, log.New(io.Discard, "", 0))
	if err != nil {
		d.t.Fatal(err)
	}
	ln, err := server.ListenOps(d.m.path("var/lab/run/ops.sock"))
	if err != nil {
		d.t.Fatal(err)
	}
	ops := s.OpsServer()
	go ops.Serve(ln)
	d.t.Cleanup(func() { ops.Close() })
	d.o.set(func() { d.o.labd = s.Handler() })
	return s
}

func (d *day) mark(n int) {
	d.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(d.m.path("var/lab/data"), "labd.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		d.t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < n; i++ {
		var have int
		db.QueryRow(`SELECT count(*) FROM people`).Scan(&have)
		if _, err := db.Exec(`INSERT INTO people (account_id, handle, m_team, added) VALUES (?, ?, 1, 1)`, 7000+have, fmt.Sprintf("member%d", have)); err != nil {
			d.t.Fatal(err)
		}
	}
}

// The dry run walks every step of the day in order, says what must be true
// first and what the owner types, and asks nothing of anything.
func TestTheDaysDryRunWalksEveryStepInOrderAndAsksNothing(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	out, code := d.script("", "day", "--state", d.state, "--acceptance", d.acceptance)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	at := 0
	for _, n := range strings.Fields("18 19 20 21 22 23 24 24a 25 26 27 28 29 30 31 32") {
		i := strings.Index(out[at:], "\nStep "+n+". ")
		if i < 0 {
			t.Fatalf("the dry run does not come to step %s after the one before:\n%s", n, out)
		}
		at += i + 1
		rest := out[at:]
		if j := strings.Index(rest, "\nStep "); j > 0 {
			rest = rest[:j]
		}
		if !strings.Contains(rest, "  must be true first: ") {
			t.Errorf("step %s does not say what must be true first:\n%s", n, rest)
		}
	}
	for _, want := range []string{
		"scripts/cloud/old-name.sh zone --apply", "DOMAIN=omarchy-m-testing.org scripts/cloud/nameservers.sh --apply",
		"ssh -t USER@HOST 'sudo ~/lab-cutover/rehearsal-import dump --dump /var/lab/import/the-day-",
		"Authorization callback URL: https://lab.omarchy-mac.org/auth/github/callback",
		"ssh -t USER@HOST 'sudo ~/lab-cutover/open --m-team 14'", "scripts/cloud/purge-cache.sh --apply", "scripts/cloud/old-name.sh redirect --apply",
		"git push origin v0.2.0", "omarchy-m-testing.org moved to lab.omarchy-mac.org today. Old links still work. Update omarchy-m-test when it asks.",
		"gh repo archive maralcbr/omarchy-m-testing", "scripts/cutover/back",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q", want)
		}
	}
	if d.o.asked != 0 || exists(d.sshLog) || exists(d.runLog) || exists(d.state) {
		t.Errorf("the dry run asked %d things, or ran ssh or the runner, or wrote the day's record", d.o.asked)
	}
	for _, name := range []string{"back", "confirmed"} {
		if out, code := d.script("", name); code != 0 || !strings.Contains(out, "a dry run") {
			t.Errorf("%s with no option: exit %d\n%s", name, code, out)
		}
	}
	for _, args := range [][]string{{"--done", "19"}, {"--go"}, {"--run", "--done", "33"}, {"--now"}} {
		if out, code := d.script("", "day", append(args, "--state", d.state)...); code == 0 {
			t.Errorf("day %q: exit 0\n%s", args, out)
		}
	}
	if d.o.asked != 0 || exists(d.sshLog) || exists(d.state) {
		t.Errorf("the other dry runs and the refused options asked %d things or wrote something", d.o.asked)
	}
}

// The dress run: the whole day against a labd on this machine, with
// Cloudflare, Railway's old site and GitHub played. At each step what must
// be true first is false first, and the script stops there with the reason;
// it stops for each of the owner's steps and does none of them; stopped and
// started again it is at the same step; the opening's three acts each run
// once.
func TestTheDressRunOfTheDay(t *testing.T) {
	m, dump, public := rehearsal(t)
	d := newDay(t, m)
	o := d.o
	today := brisbaneDay(time.Now())
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	write(t, filepath.Join(m.base, "home", "machine-key.pub"), read(t, public), 0o644)

	// Step 18: Ready first, then the fourteen tests.
	d.stops(1, "", nil, "step 18 stops here: not Ready", "Ready 1")
	if exists(d.runLog) || d.done() != "" {
		t.Fatal("the Front door's tests ran before Ready, or a step was written down")
	}
	d.ready()
	write(t, d.runnerCode, "1", 0o644)
	d.stops(1, "", []string{"--front-door", `--mac m1-pro --for 2 "--release 0.3.0"`}, "step 18 stops here: the Front door's tests did not all pass")
	if got := strings.TrimSpace(read(t, d.runLog)); got != "runner --lab --mac m1-pro --for 2 --release 0.3.0" {
		t.Errorf("the runner was started as %q", got)
	}
	write(t, d.runnerCode, "3", 0o644)
	d.stops(3, "", nil, "step 18 stopped for you")
	if d.done() != "" {
		t.Fatalf("step 18 was written down though it did not pass: %s", d.done())
	}
	write(t, d.runnerCode, "0", 0o644)
	// Step 19 wants the Alias rehearsal passed, since it cannot run afterwards.
	d.stops(1, "", nil, "step 18 is done", "step 19 stops here: no passed Alias rehearsal")
	d.record("2026-11-12-alias-rehearsal.md", "# The Alias rehearsal\n\nResult: pass\n")
	out := d.stops(3, "", nil, "step 19 is yours", "DOMAIN=omarchy-m-testing.org scripts/cloud/nameservers.sh --apply", "--run --done 19")
	// The day prints its own path, which on a CI machine can hold "runner" too.
	if strings.Contains(strings.ReplaceAll(out, repo(t), ""), "runner") || strings.Count(read(t, d.runLog), "\n") != 3 {
		t.Errorf("started again, the day ran step 18 once more:\n%s", read(t, d.runLog))
	}
	// Started again it is at the same step, and no step is skipped.
	d.stops(3, "", nil, "step 19 is yours")
	d.stops(1, "", []string{"--done", "20"}, "the day is at step 19, not 20")
	d.stops(1, "", []string{"--done", "18"}, "the day is at step 19, not 18")
	if d.done() != "18" || len(o.writes) != 0 {
		t.Fatalf("at step 19 the record holds %q and Cloudflare was sent %v", d.done(), o.writes)
	}

	// Step 20, the Freeze, waits for the zone to be active with its certificate.
	d.stops(1, "", []string{"--done", "19"}, "step 19 is done", "step 20 stops here: Cloudflare does not show omarchy-m-testing.org active")
	o.set(func() { o.zoneStatus = "active" })
	d.stops(1, "", nil, "step 20 stops here")
	o.set(func() { o.certificate = true })
	d.stops(3, "", nil, "step 20 is yours", "upload one throwaway report", `Click the service "site" > Settings > Source > Disconnect`)
	d.stops(1, "", []string{"--done", "20"}, "step 20 is not done yet: omarchy-m-testing.org still serves the old site")
	// Step 21 is told only once the old site is stopped.
	o.set(func() { o.siteUp = false })
	final := "/var/lab/import/the-day-" + strings.ReplaceAll(today, "-", "") + ".dump"
	d.stops(3, "", []string{"--done", "20"}, "step 20 is done", "step 21 is yours", "ssh -t owner@mini.test 'sudo ~/lab-cutover/rehearsal-import dump --dump "+final+"'")
	if exists(d.sshLog) {
		t.Fatalf("the script itself went to the mini for the dump, which is the owner's:\n%s", read(t, d.sshLog))
	}

	// Step 22: the import, which wants the day's dump there.
	d.stops(1, oldLink+"\n", []string{"--done", "21"}, "step 21 is done", "the day's final dump comes first", "step 22 stops here: the import or one of its three checks did not pass")
	if d.done() != "18 19 20 21" {
		t.Fatalf("the record: %q", d.done())
	}
	write(t, m.root+final, read(t, dump), 0o600)
	d.stops(3, oldLink+"\n", nil, "the import and its three checks passed", "step 22 is yours", "read Reports")
	if got := read(t, d.sshLog); strings.Count(got, "\n") != 2 || !strings.Contains(got, "ssh -t owner@mini.test sudo ~/lab-cutover/rehearsal-import run --public-key ~/machine-key.pub --dumped "+final) {
		t.Errorf("ssh was asked:\n%s", got)
	}
	// Started again, the import does not run a second time.
	d.stops(3, "", nil, "step 22 is yours")
	if strings.Count(read(t, d.sshLog), "\n") != 2 {
		t.Errorf("the import ran again:\n%s", read(t, d.sshLog))
	}

	// Step 23: the lab must name the existing OAuth app before it counts as done.
	d.stops(3, "", []string{"--done", "22"}, "step 23 is yours", "Authorization callback URL: https://lab.omarchy-mac.org/auth/github/callback", "scripts/mini/sign-in")
	d.stops(1, "", []string{"--done", "23"}, "step 23 is not done yet: the lab's Sign in link names client id dev-app-client-id")
	s := d.labdBehind(existingApp)
	d.stops(3, "", []string{"--done", "23"}, "step 23 is done", "step 24 is yours", "mark each of the 14 M team members")
	d.mark(13)
	// Step 24a: the two texts and the footer.
	d.stops(3, "", []string{"--done", "24"}, "step 24a is yours", "privacy@omarchy-mac.org")
	// Step 25 waits for the owner's go.
	d.stops(3, "", []string{"--done", "24a"}, "step 25 waits for you to say go", "ssh -t owner@mini.test 'sudo ~/lab-cutover/open --m-team 14'", "scripts/cloud/purge-cache.sh --apply")
	d.stops(3, "", nil, "step 25 waits for you to say go")
	if strings.Count(read(t, d.sshLog), "\n") != 2 || o.purges != 0 || len(o.writes) != 0 || s.Preview() != true {
		t.Fatalf("before the owner said go: ssh %q, %d purges, Cloudflare writes %v", read(t, d.sshLog), o.purges, o.writes)
	}
	// labd counts 13 of the 14: step 24's check has not passed, and nothing opens.
	d.stops(1, "", []string{"--go"}, "13 people are marked M team, not 14", "step 25 stops here: the mini did not open the lab")
	if !s.Preview() || o.purges != 0 || len(o.writes) != 0 || len(o.redirect) != 0 {
		t.Fatalf("the opening went on past labd's refusal: Preview %v, %d purges, writes %v", s.Preview(), o.purges, o.writes)
	}
	d.mark(1)
	// The purge fails once: the lab is open, and the day stops for the owner at the purge.
	o.set(func() { o.purgeFails = 1 })
	d.stops(3, "", []string{"--go"}, "the lab is open. From here problems are fixed forward", "step 25 stops for you: the one purge of the edge's cache did not run", "scripts/cloud/purge-cache.sh --apply")
	if s.Preview() || d.done() != "18 19 20 21 22a 22 23 24 24a 25a" || len(o.redirect) != 0 {
		t.Fatalf("after the opening with a failed purge: Preview %v, record %q", s.Preview(), d.done())
	}
	// Said go again: the lab is not opened twice, the cache is purged once, the redirect goes on.
	d.stops(3, "", []string{"--go"}, "step 25 is yours", "git push origin v0.2.0", "Approve and deploy")
	if got := strings.Count(read(t, d.sshLog), "lab-cutover/open "); got != 2 {
		t.Errorf("the mini was asked to open %d times, want the refused one and the one that opened", got)
	}
	if o.purges != 1 || len(o.redirect) != 1 {
		t.Fatalf("%d purges and %d redirect rules", o.purges, len(o.redirect))
	}
	d.stops(3, "", []string{"--go"}, "step 25 is yours")
	d.stops(3, "", nil, "step 25 is yours")
	if o.purges != 1 || strings.Join(o.writes, "; ") != "POST /zones/"+labZone+"/purge_cache; POST /zones/"+labZone+"/purge_cache; PUT /zones/"+oldZone+redirectAt {
		t.Errorf("Cloudflare was sent: %v (%d purges)", o.writes, o.purges)
	}
	// The tag is the owner's: step 25 is done only once 0.2.0 is the latest Lab release.
	d.stops(1, "", []string{"--done", "25"}, "step 25 is not done yet: the latest Lab release of omacom/omarchy-maclab is not 0.2.0")
	o.set(func() { o.published = "0.2.0" })
	d.stops(3, "", []string{"--done", "25"}, "step 25 is done", "step 26 is yours", "gh release create v0.2.0 --repo maralcbr/omarchy-m-testing")

	// The open lab, as the opening left it.
	page := func(path string) (string, http.Header) {
		req, _ := http.NewRequest("GET", o.lab.URL+path, nil)
		req.Header.Set("User-Agent", "TestBrowser/9.75")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b), res.Header
	}
	if body, hdr := page("/"); !strings.Contains(body, "omarchy-m-testing.org is now here") || strings.Contains(body, "not open yet") || hdr.Get("X-Robots-Tag") != "" {
		t.Errorf("the open lab's home page: banner or noindex wrong (X-Robots-Tag %q)", hdr.Get("X-Robots-Tag"))
	}
	if body, _ := page("/install"); !strings.Contains(body, "/releases/latest/download") {
		t.Error("the open lab's install line does not serve the latest Lab release")
	}
	if n := count(t, m.path("var/lab/data/reports.db"), `SELECT count(*) FROM reports`); n != 19 {
		t.Errorf("the open lab holds %d reports, want the import's 19", n)
	}

	// The rest of the day is the owner's, a step at a time.
	d.stops(3, "", []string{"--done", "26"}, "step 27 is yours", "omarchy-m-testing.org moved to lab.omarchy-mac.org today.")
	d.stops(3, "", []string{"--done", "27"}, "step 28 is yours", "scripts/cutover/confirmed --run --mini owner@mini.test --old-links <FILE> --acceptance "+d.acceptance+"\n")
	d.stops(1, "", []string{"--done", "29"}, "the day is at step 28, not 29")
	d.stops(1, "", []string{"--done", "28"}, "step 28 is not done yet: no record DATE-confirmed.md")
	d.record(today+"-confirmed.md", "# The Confirmed checklist\n\nResult: pass, what a script can check\n\nConfirmed: "+today+", the owner\n")
	d.stops(3, "", []string{"--done", "28"}, "step 29 is yours", "sudo -u lab rm "+final)
	d.stops(3, "", []string{"--done", "29"}, "step 30 is yours", "gh secret delete RELEASE_SIGNING_KEY")
	d.stops(3, "", []string{"--done", "30"}, "step 31 is yours")
	d.stops(3, "", []string{"--done", "31"}, "step 32 is yours")
	d.stops(0, "", []string{"--done", "32"}, "every step of the day is done")
	d.stops(0, "", nil, "every step of the day is done")
	if out, _ := d.script("", "day", "--where", "--state", d.state); !strings.Contains(out, "every step of the day is done") || !strings.Contains(out, "step 25: done "+today) {
		t.Errorf("day --where: %s", out)
	}

	// What the script never did: a message, a nameserver, a tag, a deletion.
	if got := read(t, d.sshLog); strings.Count(got, "\n") != 4 || strings.Contains(got, "sign-in") || strings.Contains(got, " rm ") || strings.Contains(got, "dump --dump") {
		t.Errorf("ssh was asked for more than the import and the opening:\n%s", got)
	}
	for _, w := range o.writes {
		if !strings.HasSuffix(w, "/purge_cache") && !strings.HasSuffix(w, redirectAt) {
			t.Errorf("Cloudflare was sent %s", w)
		}
	}
	// There is no way back from here.
	if out, code := d.script("", "back", "--run", "--state", d.state); code != 1 || !strings.Contains(out, "point of no return") {
		t.Errorf("back after the opening: exit %d\n%s", code, out)
	}
}

// days is a day's record holding these steps.
func (d *day) days(steps ...string) {
	d.t.Helper()
	d.daysOn("2026-11-20", steps...)
}

// daysOn is the same, each step done on that day.
func (d *day) daysOn(day string, steps ...string) {
	d.t.Helper()
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s + "\t" + day + " 10:00\t\n")
	}
	write(d.t, d.state, b.String(), 0o644)
}

// Nothing after step 25 runs before it: not by a step said done out of
// order, and not by a record that says the lab opened while the Lab address
// still answers as Preview.
func TestNothingAfterStep25RunsBeforeTheLabIsOpen(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	d.o.set(func() { d.o.app = existingApp })
	before := strings.Fields("18 19 20 21 22a 22 23 24 24a")
	d.days(before...)
	for _, n := range []string{"26", "27", "28", "29", "30", "31", "32"} {
		d.stops(1, "", []string{"--done", n}, "the day is at step 25, not "+n)
	}
	d.stops(3, "", nil, "step 25 waits for you to say go")
	if d.done() != strings.Join(before, " ") {
		t.Fatalf("the record changed: %q", d.done())
	}
	// A record that claims the opening, with the lab still in Preview.
	opened := append(before, "25a", "25b", "25c", "25")
	later := strings.Fields("26 27 28 29 30 31 32")
	for i, next := range later {
		d.days(append(append([]string{}, opened...), later[:i]...)...)
		want := d.done()
		out, code := d.run("")
		if code != 1 || !strings.Contains(out, "nothing after step 25 runs before it") {
			t.Errorf("with the record at %q and the lab in Preview: exit %d\n%s", want, code, out)
		}
		if out, code := d.run("", "--done", next); code != 1 || !strings.Contains(out, "nothing after step 25 runs before it") || d.done() != want {
			t.Errorf("--done %s with the lab in Preview: exit %d, record %q\n%s", next, code, d.done(), out)
		}
	}
	// Open, but not Confirmed: steps 29 to 32 wait for step 28.
	d.o.set(func() { d.o.stage = "open" })
	d.days(append(opened, "26", "27")...)
	d.stops(1, "", []string{"--done", "28"}, "step 28 is not done yet")
	d.stops(1, "", []string{"--done", "29"}, "the day is at step 28")
	// A Dark lab is not an open lab.
	d.o.set(func() { d.o.stage = "dark" })
	d.stops(1, "", nil, "does not answer as an open lab (dark)")
}

// Each check of steps 22 to 25 that is false stops the day there.
func TestEachStepBeforeTheOpeningStopsWhenItsCheckIsFalse(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	o := d.o
	// Step 22 on a lab that is not in Preview.
	d.days("18", "19", "20", "21")
	o.set(func() { o.siteUp = false; o.stage = "dark" })
	d.stops(1, "", nil, "step 22 stops here: the lab does not answer as a lab in Preview", "(dark)")
	o.set(func() { o.stage = "open" })
	d.stops(1, "", nil, "step 22 stops here: the lab does not answer as a lab in Preview", "(open)")
	if exists(d.sshLog) {
		t.Fatal("the import was started on a lab that is not in Preview")
	}
	// Step 21 with the old site still up.
	d.days("18", "19", "20")
	o.set(func() { o.siteUp = true; o.stage = "preview" })
	d.stops(1, "", nil, "step 21 stops here: omarchy-m-testing.org still serves the old site")
	// Step 24 with the development app still in place.
	d.days("18", "19", "20", "21", "22a", "22", "23")
	d.stops(1, "", nil, "step 24 stops here: the lab's Sign in link does not name the existing OAuth app")
	// Step 24a with no footer, and step 25 with /terms gone.
	o.set(func() { o.app = existingApp; o.noFooter = true })
	d.days("18", "19", "20", "21", "22a", "22", "23", "24")
	d.stops(1, "", nil, "step 24a stops here: the home page's footer does not link to /terms and /privacy")
	d.days("18", "19", "20", "21", "22a", "22", "23", "24", "24a")
	d.stops(1, "", []string{"--go"}, "step 25 stops here: the home page's footer")
	if exists(d.sshLog) || o.purges != 0 || len(o.writes) != 0 {
		t.Errorf("a stopped step went to the mini or to Cloudflare: %v", o.writes)
	}
	// The mini is not named: the opening does not start.
	o.set(func() { o.noFooter = false })
	out, code := d.script("", "day", "--run", "--go", "--state", d.state, "--acceptance", d.acceptance, "--token-file", d.tokenFile)
	if code != 1 || !strings.Contains(out, "give --mini USER@HOST") || o.purges != 0 {
		t.Errorf("the opening with no mini: exit %d\n%s", code, out)
	}
	// Step 19 without the Alias rehearsal goes on only when the owner says so.
	d.days("18")
	d.record("2026-11-20-front-door.md", "Result: pass, all fourteen\n")
	d.stops(1, "", nil, "step 19 stops here: no passed Alias rehearsal")
	d.stops(3, "", []string{"--without-alias-rehearsal"}, "step 19 is yours")
	d.record("2026-11-20-front-door.md", "Result: 13 of fourteen passed in this run\n")
	d.stops(1, "", []string{"--without-alias-rehearsal"}, "step 19 stops here: step 18 is not green")
}

// The way back before step 25: it says the owner's part for what was done,
// checks from outside that the old site serves again and the lab has its
// development app back, and puts the day's record back to before the
// Freeze. Once the lab has opened it refuses.
func TestTheWayBackBeforeStep25(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	o := d.o
	back := func(args ...string) (string, int) {
		return d.script("", "back", append([]string{"--state", d.state, "--mini", "owner@mini.test"}, args...)...)
	}
	d.days("18", "19")
	if out, code := back("--run"); code != 0 || !strings.Contains(out, "nothing public has changed") {
		t.Errorf("before the Freeze: exit %d\n%s", code, out)
	}
	d.days("18", "19", "20", "21", "22a", "22", "23", "24")
	o.set(func() { o.siteUp = false; o.app = existingApp })
	out, code := back("--run")
	if code != 3 || !strings.Contains(out, "Authorization callback URL: https://omarchy-m-testing.org/auth/github/callback") || !strings.Contains(out, "Redeploy") || !strings.Contains(out, "ssh -t owner@mini.test 'sudo /usr/local/lab/current/scripts/mini/sign-in'") {
		t.Fatalf("the way back: exit %d\n%s", code, out)
	}
	if out, code := back("--run", "--done"); code != 1 || !strings.Contains(out, "does not answer 200 yet") {
		t.Errorf("--done with the old site still stopped: exit %d\n%s", code, out)
	}
	o.set(func() { o.siteUp = true })
	if out, code := back("--run", "--done"); code != 1 || !strings.Contains(out, "still names the existing OAuth app") {
		t.Errorf("--done with the callback's app still in the lab: exit %d\n%s", code, out)
	}
	if d.done() != "18 19 20 21 22a 22 23 24" {
		t.Fatalf("a refused --done changed the record: %q", d.done())
	}
	o.set(func() { o.app = "dev-app-client-id" })
	if out, code := back("--run", "--done"); code != 0 || !strings.Contains(out, "back to before the Freeze") {
		t.Fatalf("--done: exit %d\n%s", code, out)
	}
	if d.done() != "19" {
		t.Errorf("the record after the way back: %q, want the old name's move alone", d.done())
	}
	kept, _ := filepath.Glob(d.state + ".taken-back-*")
	if len(kept) != 1 || !strings.Contains(read(t, kept[0]), "24\t") {
		t.Errorf("the record as it was is not kept: %v", kept)
	}
	if out, _ := d.script("", "day", "--where", "--state", d.state); !strings.Contains(out, "next is step 18") {
		t.Errorf("after the way back the day does not start again at step 18: %s", out)
	}
	// The mini is only asked where the lab is.
	for _, line := range strings.Split(strings.TrimSpace(read(t, d.sshLog)), "\n") {
		if line != "ssh -t owner@mini.test sudo ~/lab-cutover/open stage" {
			t.Errorf("the way back asked the mini: %s", line)
		}
	}
	if len(o.writes) != 0 {
		t.Errorf("the way back wrote to Cloudflare")
	}
	// Step 25 is the point of no return: by the record, and by what the Lab address says.
	d.days("18", "19", "20", "21", "22a", "22", "23", "24", "24a", "25a")
	if out, code := back("--run"); code != 1 || !strings.Contains(out, "point of no return") {
		t.Errorf("with the opening in the record: exit %d\n%s", code, out)
	}
	d.days("18", "19", "20", "21", "22a", "22", "23")
	o.set(func() { o.stage = "open" })
	for _, args := range [][]string{{"--run"}, {"--run", "--done"}} {
		if out, code := back(args...); code != 1 || !strings.Contains(out, "answers as an open lab") || d.done() != "18 19 20 21 22a 22 23" {
			t.Errorf("back %q with an open lab: exit %d\n%s", args, code, out)
		}
	}
}

// The Ready report reads the dated records and says which of the nine
// items are green.
func TestTheReadyReportSaysWhichOfTheNineAreGreen(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	ready := func() (string, int) { return d.script("", "ready", "--records", d.acceptance) }
	if out, code := ready(); code != 1 || !strings.Contains(out, "0 of nine items are green") || strings.Count(out, "not green") != 9 {
		t.Fatalf("with no record: exit %d\n%s", code, out)
	}
	d.ready()
	if out, code := ready(); code != 0 || !strings.Contains(out, "all nine items are green") || strings.Count(out, ": green (") != 9 {
		t.Fatalf("with every record: exit %d\n%s", code, out)
	}
	for _, c := range []struct{ file, text, says string }{
		{"2026-11-11-front-door.md", "Result: 13 of fourteen passed in this run\n", `Ready 1, the Front door's fourteen tests: not green: 2026-11-11-front-door.md says "13 of fourteen`},
		{"2026-11-11-front-door.md", "Result: pass\n", "Ready 1, the Front door's fourteen tests: not green"},
		{"out-of-band.results", "01 pass 2026-11-10 x.md\n02 fail 2026-11-10 y.md\n", "Ready 2, Out-of-band wiring's eight tests: not green: out-of-band.results: 02:fail 03:missing"},
		{"2026-11-11-dead-man.md", "it fired\n", "Ready 3, operations: not green: 2026-11-11-dead-man.md has no Result line"},
		{"2026-11-11-lease.md", "Result: fail\n", "Ready 5, one Lease: not green"},
		{"hardening.results", "", "Ready 8, hardening's fourteen tests: not green: hardening.results: 01:missing"},
		{"builder-gate.results", "builder-gate stopped 2026-11-11 x.md -\n", "Ready 9, the Builder's gate test: not green: builder-gate.results: builder-gate:stopped"},
	} {
		was := read(t, filepath.Join(d.acceptance, c.file))
		d.record(c.file, c.text)
		if out, code := ready(); code != 1 || !strings.Contains(out, c.says) || !strings.Contains(out, "8 of nine items are green") {
			t.Errorf("%s as %q: exit %d\n%s", c.file, c.text, code, out)
		}
		if was == "" {
			os.Remove(filepath.Join(d.acceptance, c.file))
		} else {
			d.record(c.file, was)
		}
	}
	if d.o.asked != 0 {
		t.Errorf("the Ready report asked %d things", d.o.asked)
	}
}

// The Confirmed checklist: what a script can check of the five points,
// against an open lab; the record it writes; and that Confirmed itself
// stays the owner's line.
func TestTheConfirmedChecklistCoversWhatAScriptCan(t *testing.T) {
	m, s := opening(t, 14)
	if out := m.refused("confirmed-mini"); !strings.Contains(out, "the lab has not opened (preview)") {
		t.Fatalf("confirmed-mini before the opening: %s", out)
	}
	m.ok("open", "--m-team", "14")
	opened, _ := s.Live()
	d := newDay(t, m)
	o := d.o
	today := brisbaneDay(time.Now())
	o.set(func() {
		o.stage, o.app, o.published, o.siteUp = "open", existingApp, "0.2.0", false
		o.redirect = []map[string]any{{"action": "redirect"}}
	})
	// The mini's nightly backup and Restore drill, played: each leaves its mark.
	scripts := m.path("usr/local/lab/builds/v0.2.0-rc.9/scripts/mini")
	write(t, scripts+"/backup", "#!/bin/bash\necho backup >> \"$STUB_LOG\"\n[[ -f $STUB_STATE/backup.fails ]] && exit 1\ndate > \"$LAB_SYSROOT/var/lab/log/backup.ok\"\n", 0o755)
	write(t, scripts+"/restore-drill", "#!/bin/bash\necho restore-drill >> \"$STUB_LOG\"\ndate > \"$LAB_SYSROOT/var/lab/log/restore-drill.ok\"\n", 0o755)
	deadLog := m.path("var/lab/log/dead-man.log")
	write(t, deadLog, "dead-man: "+opened.Add(-time.Hour).UTC().Format(time.RFC3339)+" the lab did not answer at https://lab.omarchy-mac.org/healthz: the check was not pinged\n", 0o600)
	links := filepath.Join(m.base, "owner", "old-links")
	secret := "s3cr3t-of-the-deletion-link"
	write(t, links, "https://omarchy-m-testing.org/reports/abc123\nhttps://omarchy-m-testing.org/reports/abc123/deletion?token="+secret+"\n", 0o600)
	record := filepath.Join(d.acceptance, today+"-confirmed.md")
	confirmed := func(args ...string) (string, int) {
		os.Remove(record)
		out, code := d.script("", "confirmed", append([]string{"--run", "--acceptance", d.acceptance}, args...)...)
		if strings.Contains(out+read(t, record)+read(t, d.sshLog), secret) {
			t.Errorf("the deletion link's secret was printed, written down or sent to the mini")
		}
		return out, code
	}

	out, code := confirmed("--mini", "owner@mini.test", "--old-links", links)
	if code != 0 {
		t.Fatalf("the checklist: exit %d\n%s", code, out)
	}
	text := read(t, record)
	for _, want := range []string{
		"- 1. pass: the smoke script is green", "- 2. pass: the old install line gives the lab's installer, which takes the latest Lab release, 0.2.0",
		"- 2. pass: the old report link lands on its page", "- 2. pass: the old deletion link lands on its page", "- 3. pass: the lab's Sign in link names the existing OAuth app",
		"- 5. pass: on the mini: one backup by hand and restored; the daily summary recorded for the Lab group; no Dark minute since the lab opened",
		"People shows the 14 marked", "one Verified run from each", "Result: pass, what a script can check",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the record does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Confirmed:") || !strings.Contains(out, "add your dated line") {
		t.Errorf("the script wrote the owner's dated line, or did not ask for it:\n%s", text)
	}
	if got := read(t, d.runLog); !strings.Contains(got, "smoke SITE="+o.lab.URL) {
		t.Errorf("the smoke script was run as %q", got)
	}
	if got := m.calls(); !strings.Contains(got, "backup\n") || !strings.Contains(got, "restore-drill\n") {
		t.Errorf("the mini's backup and Restore drill were not run:\n%s", got)
	}
	if n := count(t, m.path("var/lab/data/labd.db"), `SELECT count(*) FROM lines WHERE channel = 'lab' AND text LIKE 'Omarchy Mac Lab, %'`); n != 1 {
		t.Errorf("%d daily summaries recorded for the Lab group, want the one by hand", n)
	}
	// The day takes step 28 only with the owner's line.
	d.daysOn(today, strings.Fields("18 19 20 21 22a 22 23 24 24a 25a 25b 25c 25 26 27")...)
	d.stops(1, "", []string{"--done", "28"}, "step 28 is not done yet")
	write(t, record, text+"\nConfirmed: "+today+", the owner\n", 0o644)
	d.stops(3, "", []string{"--done", "28"}, "step 28 is done", "step 29 is yours")

	// Each point that is not so fails the checklist and is named.
	for _, c := range []struct {
		name   string
		change func() func()
		says   string
	}{
		{"the smoke script fails", func() func() {
			write(t, d.smokeFails, "", 0o644)
			return func() { os.Remove(d.smokeFails) }
		}, "point 1: fail: the smoke script against"},
		{"the old name does not forward", func() func() {
			o.set(func() { o.redirect = nil })
			return func() { o.set(func() { o.redirect = []map[string]any{{"action": "redirect"}} }) }
		}, "point 2: fail: the old report link is not forwarded with a 308"},
		{"the latest Lab release is another", func() func() {
			o.set(func() { o.published = "0.1.15" })
			return func() { o.set(func() { o.published = "0.2.0" }) }
		}, "point 2: fail: the latest Lab release is not 0.2.0"},
		{"the lab still has the development app", func() func() {
			o.set(func() { o.app = "dev-app-client-id" })
			return func() { o.set(func() { o.app = existingApp }) }
		}, "point 3: fail: the lab's Sign in link names client id dev-app-client-id"},
		{"the backup fails", func() func() {
			write(t, m.state+"/backup.fails", "", 0o644)
			return func() { os.Remove(m.state + "/backup.fails") }
		}, "point 5: fail: on the mini: backup=fail restore=fail"},
		{"a Dark minute since the opening", func() func() {
			was := read(t, deadLog)
			write(t, deadLog, was+"dead-man: "+opened.Add(time.Minute).UTC().Format(time.RFC3339)+" the lab did not answer 200 at https://lab.omarchy-mac.org/healthz: the check was not pinged\n", 0o600)
			return func() { write(t, deadLog, was, 0o600) }
		}, "dead_man_dark_minutes=1"},
	} {
		undo := c.change()
		out, code := confirmed("--mini", "owner@mini.test", "--old-links", links)
		if code != 1 || !strings.Contains(out, c.says) || !strings.Contains(read(t, record), "Result: fail") {
			t.Errorf("%s: exit %d, want 1 and %q\n%s", c.name, code, c.says, out)
		}
		undo()
	}
	// Refused before anything is asked: a links file others can read, a lab that is not open.
	os.Chmod(links, 0o644)
	asked := o.asked
	if out, code := confirmed("--mini", "owner@mini.test", "--old-links", links); code != 1 || !strings.Contains(out, "can be read by others") || o.asked != asked {
		t.Errorf("a links file others can read: exit %d\n%s", code, out)
	}
	os.Chmod(links, 0o600)
	o.set(func() { o.stage = "preview" })
	if out, code := confirmed("--mini", "owner@mini.test", "--old-links", links); code != 1 || !strings.Contains(out, "the checklist is for a Live lab") || exists(record) {
		t.Errorf("on a lab in Preview: exit %d\n%s", code, out)
	}
}

// Every script of the day parses, passes shellcheck and says what it does.
func TestTheDaysScriptsParseAndPassShellcheck(t *testing.T) {
	var files []string
	for _, name := range []string{"day", "back", "confirmed", "confirmed-mini", "open", "ready", "owner.sh", "mini.sh", "rehearsal-import"} {
		file := filepath.Join(repo(t), "scripts", "cutover", name)
		files = append(files, file)
		if out, err := exec.Command("/bin/bash", "-n", file).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse with macOS's own bash: %s", name, out)
		}
		if st, err := os.Stat(file); err != nil || (!strings.HasSuffix(name, ".sh") && st.Mode().Perm()&0o111 == 0) {
			t.Errorf("%s is not executable", name)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck is not installed")
	}
	cmd := exec.Command("shellcheck", append([]string{"--external-sources"}, files...)...)
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

// An opening whose 25a did not reach the day's record (a dropped SSH, the
// outside check failing): the lab answers open, the mini says since when,
// and the day writes 25a down from it and goes on with the purge and the
// redirect. The mini is not asked to open again.
func TestALostOpeningIsTakenFromTheMiniAndTheDayGoesOn(t *testing.T) {
	m, s := opening(t, 14)
	m.ok("open", "--m-team", "14")
	at, ok := s.Live()
	if !ok {
		t.Fatal("labd did not open")
	}
	d := newDay(t, m)
	o := d.o
	o.set(func() { o.stage, o.app = "open", existingApp })
	d.days(strings.Fields("18 19 20 21 22a 22 23 24 24a")...)
	d.stops(3, "", []string{"--go"}, "the lab opened at", "step 25 is yours", "git push origin v0.2.0")
	if d.done() != "18 19 20 21 22a 22 23 24 24a 25a 25b 25c" || o.purges != 1 || len(o.redirect) != 1 {
		t.Fatalf("the record %q, %d purges, %d redirect rules", d.done(), o.purges, len(o.redirect))
	}
	zone, _ := time.LoadLocation("Australia/Brisbane")
	if want := "25a\t" + at.In(zone).Format("2006-01-02 15:04") + "\t"; !strings.Contains(read(t, d.state), want) {
		t.Errorf("25a is not written down at the opening's time %q:\n%s", want, read(t, d.state))
	}
	if got := read(t, d.sshLog); strings.Contains(got, "open --m-team") || !strings.Contains(got, "lab-cutover/open stage") {
		t.Errorf("ssh was asked:\n%s", got)
	}
	// A lab that answers open from outside while the mini says Preview is not taken for opened.
	m2 := newMini(t, oldlabtest.OnOmarchyGPU)
	d2 := newDay(t, m2)
	d2.o.set(func() { d2.o.stage, d2.o.app = "open", existingApp })
	d2.days(strings.Fields("18 19 20 21 22a 22 23 24 24a")...)
	d2.stops(1, "", []string{"--go"}, "the mini says the lab is in Preview")
	if d2.done() != "18 19 20 21 22a 22 23 24 24a" || d2.o.purges != 0 {
		t.Errorf("the record %q, %d purges", d2.done(), d2.o.purges)
	}
}

// The way back asks the mini itself: an opening that did not reach the
// day's record, while the Lab address is Dark, is still past the point of
// no return.
func TestTheWayBackAsksTheMiniWhetherTheLabOpened(t *testing.T) {
	m, _ := opening(t, 14)
	m.ok("open", "--m-team", "14")
	d := newDay(t, m)
	o := d.o
	o.set(func() { o.stage, o.siteUp, o.app = "dark", false, existingApp })
	d.days(strings.Fields("18 19 20 21 22a 22 23 24 24a")...)
	for _, args := range [][]string{{"--run"}, {"--run", "--done"}} {
		out, code := d.script("", "back", append([]string{"--state", d.state, "--mini", "owner@mini.test"}, args...)...)
		if code != 1 || !strings.Contains(out, "the mini says the lab opened") || !strings.Contains(out, "point of no return") {
			t.Errorf("back %q with the lab open on the mini and Dark outside: exit %d\n%s", args, code, out)
		}
	}
	if out, code := d.script("", "back", "--state", d.state, "--run"); code != 1 || !strings.Contains(out, "give --mini USER@HOST") {
		t.Errorf("back with no mini: exit %d\n%s", code, out)
	}
	if d.done() != "18 19 20 21 22a 22 23 24 24a" {
		t.Errorf("the record changed: %q", d.done())
	}
}

// Asking the mini's stage, sudo's prompt reaches the owner: the way back and
// a lost opening's recovery show it, and still read the answer after it.
func TestSudosPromptOnTheMiniIsShownWhenTheStageIsAsked(t *testing.T) {
	const prompt = "Password for owner on the mini:"
	m, s := opening(t, 14)
	m.ok("open", "--m-team", "14")
	at, _ := s.Live()
	d := newDay(t, m)
	d.env = append(d.env, "DAY_SUDO_ASKS=1")
	o := d.o
	o.set(func() { o.stage, o.siteUp, o.app = "dark", false, existingApp })
	d.days(strings.Fields("18 19 20 21 22a 22 23 24 24a")...)
	out, code := d.script("", "back", "--state", d.state, "--mini", "owner@mini.test", "--run")
	if code != 1 || !strings.Contains(out, prompt) || !strings.Contains(out, "the mini says the lab opened") {
		t.Errorf("back: exit %d\n%s", code, out)
	}
	o.set(func() { o.stage = "open" })
	d.stops(3, "", []string{"--go"}, prompt, "the lab opened at", "step 25 is yours")
	zone, _ := time.LoadLocation("Australia/Brisbane")
	if want := "25a\t" + at.In(zone).Format("2006-01-02 15:04") + "\t"; !strings.Contains(read(t, d.state), want) {
		t.Errorf("25a is not written down at the opening's time %q:\n%s", want, read(t, d.state))
	}
}

// Two modes are refused before anything is asked: which was meant is not
// guessed, and the last one no longer wins.
func TestTheDaysScriptsRefuseTwoModes(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	d.o.set(func() { d.o.app = existingApp })
	d.days(strings.Fields("18 19 20 21 22a 22 23 24 24a")...)
	common := []string{"--state", d.state, "--mini", "owner@mini.test"}
	for _, c := range []struct {
		script string
		args   []string
	}{
		{"day", []string{"--dry-run", "--run", "--go"}},
		{"day", []string{"--run", "--dry-run"}},
		{"day", []string{"--where", "--run"}},
		{"back", []string{"--dry-run", "--run"}},
		{"back", []string{"--run", "--dry-run"}},
		{"confirmed", []string{"--dry-run", "--run"}},
	} {
		args := append(append([]string{}, c.args...), "--mini", "owner@mini.test")
		if c.script != "confirmed" {
			args = append(c.args, common...)
		}
		if out, code := d.script("", c.script, args...); code != 2 || !strings.Contains(out, "two modes") {
			t.Errorf("%s %q: exit %d\n%s", c.script, c.args, code, out)
		}
	}
	if d.o.asked != 0 || exists(d.sshLog) || d.done() != "18 19 20 21 22a 22 23 24 24a" {
		t.Errorf("a refused run asked %d things, went to the mini or wrote the record (%q)", d.o.asked, d.done())
	}
}

// Step 28 takes only a Confirmed record that passed, with the owner's line
// dated on or after the opening.
func TestAStaleOrFailedConfirmedRecordDoesNotPassStep28(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	d.o.set(func() { d.o.stage, d.o.app = "open", existingApp })
	d.days(strings.Fields("18 19 20 21 22a 22 23 24 24a 25a 25b 25c 25 26 27")...)
	for _, c := range []struct{ file, text string }{
		{"2026-11-19-confirmed.md", "Result: pass, what a script can check\n\nConfirmed: 2026-11-19, the owner\n"},
		{"2026-11-21-confirmed.md", "Result: fail\n\nConfirmed: 2026-11-21, the owner\n"},
		{"2026-11-22-confirmed.md", "Result: pass, what a script can check\n\nConfirmed: 2026-11-19, the owner\n"},
	} {
		d.record(c.file, "# The Confirmed checklist\n\n"+c.text)
		if out, code := d.run("", "--done", "28"); code != 1 || !strings.Contains(out, "step 28 is not done yet") {
			t.Errorf("with %s holding %q: exit %d\n%s", c.file, c.text, code, out)
		}
	}
	d.record("2026-11-23-confirmed.md", "# The Confirmed checklist\n\nResult: pass, what a script can check\n\nConfirmed: 2026-11-23, the owner\n")
	d.stops(3, "", []string{"--done", "28"}, "step 28 is done", "step 29 is yours")
}

// A line of the day's record with no time, or with less than its three
// fields, is not a step done: a hand-made 25a does not skip the opening.
func TestALineWithoutItsTimeIsNoStepDone(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	d := newDay(t, m)
	d.o.set(func() { d.o.app = existingApp })
	d.days(strings.Fields("18 19 20 21 22a 22 23 24 24a")...)
	was := read(t, d.state)
	for _, line := range []string{"25a\n", "25a\t\t\n", "25a\tsoon\tthe lab opened\n", "25a\t2026-11-20 10:00\n"} {
		write(t, d.state, was+line, 0o644)
		os.Remove(d.sshLog)
		out, code := d.run("", "--go")
		if code != 1 || !strings.Contains(read(t, d.sshLog), "lab-cutover/open --m-team 14") || d.o.purges != 0 || len(d.o.writes) != 0 {
			t.Errorf("with the line %q: exit %d, %d purges\n%s", line, code, d.o.purges, out)
		}
	}
}
