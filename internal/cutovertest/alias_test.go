package cutovertest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	labName   = "lab.omarchy-mac.org"
	labURL    = "https://" + labName
	rehearsal = "alias-rehearsal.omarchy-m-testing.org"
	third     = "alias-rehearsal-third.omarchy-m-testing.org"
	plainLine = "Omarchy Mac Lab is at " + labURL + ": use that address"
)

func cutover(t *testing.T, name string) string {
	return filepath.Join(repo(t), "scripts", "cutover", name)
}

func TestTheRehearsalAndTheRecordingsAreSound(t *testing.T) {
	var files []string
	for _, name := range []string{"visitor.sh", "alias-rehearsal", "dark-recordings"} {
		file := cutover(t, name)
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if name != "visitor.sh" && info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", name)
		}
		if out, err := exec.Command("/bin/bash", "-n", file).CombinedOutput(); err != nil {
			t.Errorf("/bin/bash -n %s: %v\n%s", name, err, out)
		}
		files = append(files, file)
		// Neither sends a message, calls Cloudflare itself or holds a token.
		text := read(t, file)
		for _, never := range []string{"signal-cli", "sendmail", "mail -s", "ntfy", "api.cloudflare.com", "api.porkbun.com", "Authorization: Bearer $", "security find-generic-password"} {
			if strings.Contains(text, never) {
				t.Errorf("%s names %s", name, never)
			}
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

// With no option the rehearsal and the recordings say their steps and run
// nothing: no cloud script, no ssh, no dig, no curl, no lab.
func TestTheRehearsalsAndTheRecordingsDryRunsSendNothing(t *testing.T) {
	added := map[string]func() []string{"*alias-rehearsal*": recordsAdded(t, "*alias-rehearsal*"), "*dark-recordings*": recordsAdded(t, "*dark-recordings*")}
	stubs := t.TempDir()
	log := filepath.Join(stubs, "calls")
	for _, tool := range []string{"lab", "curl", "ssh", "dig", "alias.sh", "rehearsal-record.sh"} {
		if err := os.WriteFile(filepath.Join(stubs, tool), []byte("#!/bin/sh\necho \""+tool+" $*\" >> \""+log+"\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + stubs + ":" + os.Getenv("PATH")}
	for name, c := range map[string]struct {
		args  []string
		wants []string
	}{
		"alias-rehearsal": {[]string{"--cloud", stubs}, []string{
			"alias.sh --apply --name " + rehearsal, "rehearsal-record.sh --apply --name " + rehearsal, "nothing but the CNAME in place",
			"rehearsal-alias " + rehearsal, "308 to the same path and query at " + labURL, plainLine, "fails and stores nothing", "no answer carries a cookie",
			"https://" + third + ": 404 at the tunnel", "fd13-cache --lab", "once the name has been up 16 minutes", "lets the hostname lapse", "\"Agents\" gets \"The Alias is no longer active\"",
			"alias.sh --apply --remove", "rehearsal-alias deployed", "alias-rehearsal.md",
		}},
		"dark-recordings": {nil, []string{
			"the name of every cookie an answer sets, Cloudflare's included", "until cf-cache-status says HIT", "hold cloudflared 150", "hold labd 150",
			labURL + "/status (never cached)", "its number and ray id", "dark-recordings.md", "as seen",
		}},
	} {
		out, code := run(t, env, "", cutover(t, name), c.args...)
		if code != 0 || !strings.Contains(out, "Nothing was sent.") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
		for _, want := range c.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not say %q:\n%s", name, want, out)
			}
		}
		if again, _ := run(t, env, "", cutover(t, name), append(c.args, "--dry-run")...); again != out {
			t.Errorf("%s --dry-run says something else than no option", name)
		}
		if out, code := run(t, env, "", cutover(t, name), "--everything"); code == 0 {
			t.Errorf("%s took an option it does not know:\n%s", name, out)
		}
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("a dry run ran:\n%s", b)
	}
	for _, stem := range []string{"*alias-rehearsal*", "*dark-recordings*"} {
		if entries := added[stem](); len(entries) != 0 {
			t.Errorf("a dry run left a record: %v", entries)
		}
	}
}

// What is provable today: a labd started with the Alias, asked on the
// socket the tunnel would use, forwards pages to the Lab address with path
// and query kept, refuses the API in the plain line, and sets no cookie.
func TestOnTheAliasLabdForwardsPagesAndRefusesTheAPI(t *testing.T) {
	added := recordsAdded(t, "*alias-rehearsal*")
	records := t.TempDir()
	out, code := run(t, nil, "", cutover(t, "alias-rehearsal"), "--local", "--bin", bins(t), "--records", records)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"the forward: pass. 4 page requests on " + rehearsal + ", one a POST, each got 308 to the same path and query at " + labURL,
		"the API: pass. a read and a POST under /api/ on " + rehearsal + " were refused (421) with the line \"" + plainLine + "\"",
		"cookies: pass. no answer on " + rehearsal + " carried a cookie",
		"the Lab address: pass. " + labName + " is served (200)",
		"a third name: pass. " + third + " is refused by labd itself (421)",
		": pass",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the local rehearsal does not say %q:\n%s", want, out)
		}
	}
	files, _ := filepath.Glob(filepath.Join(records, "*-alias-rehearsal.md"))
	if len(files) != 1 {
		t.Fatalf("records: %v", files)
	}
	record := read(t, files[0])
	for _, want := range []string{"# The Alias rehearsal", "--local", "not the route", "- the forward: pass.", "- not here: the certificate", "Result: pass"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if entries := added(); len(entries) != 0 {
		t.Errorf("a local run left a record in docs/acceptance: %v", entries)
	}
}

// labdWithout is a folder of the built programs whose labd is started
// without one of its options.
func labdWithout(t *testing.T, option string) string {
	t.Helper()
	real, dir := bins(t), t.TempDir()
	wrapper := "#!/bin/bash\nargs=()\nfor a in \"$@\"; do [[ $a == " + option + "=* ]] || args+=(\"$a\"); done\nexec \"" + real + "/labd\" \"${args[@]}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "labd"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// With the behaviour turned off the local rehearsal fails: a labd that was
// given no Alias refuses the name instead of forwarding it.
func TestTheLocalRehearsalFailsWhenLabdDoesNotForward(t *testing.T) {
	out, code := run(t, nil, "", cutover(t, "alias-rehearsal"), "--local", "--bin", labdWithout(t, "--alias"))
	if code != 1 || !strings.Contains(out, "the forward: FAIL. / answered 421") || !strings.Contains(out, "the API: FAIL.") || !strings.Contains(out, ": fail") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// played is the rehearsal's whole world, made up on this machine: a real
// labd on a socket, the edge and the tunnel as one stand-in, Cloudflare's
// and Porkbun's scripts as stand-ins that keep what was made in files, and
// ssh and dig as stand-ins.
type played struct {
	t       *testing.T
	state   string
	stubs   string
	cloud   string
	edge    *httptest.Server
	env     []string
	mu      sync.Mutex
	cookie  bool
	records string
}

const cloudStub = `#!/bin/bash
# A stand-in for scripts/cloud/%[1]s: what it made is a file under $STATE.
me=%[1]s; mode=dry; want=present; name=""
echo "$me $*" >> "$STATE/calls"
while (( $# )); do
  case "$1" in
    --check) mode=check ;; --apply) mode=apply ;; --remove) want=absent ;;
    --name) name="$2"; shift ;; --token-file) [[ -f $2 ]] || { echo "no token file"; exit 1; }; shift ;;
    --porkbun-keys) [[ $me == rehearsal-record.sh && -f $2 ]] || { echo "no keys file, or not Porkbun's script"; exit 1; }; shift ;;
  esac
  shift
done
[[ $mode != dry ]] || exit 0
if [[ $me == rehearsal-record.sh ]]; then
  f="$STATE/record.$name"
  if [[ $mode == apply ]]; then if [[ $want == present ]]; then : > "$f"; else rm -f "$f"; fi; fi
  if [[ $want == present ]]; then [[ -e $f ]]; else [[ ! -e $f ]]; fi
  exit
fi
f="$STATE/hostname.$name"
if [[ $mode == apply ]]; then
  if [[ $want == absent ]]; then rm -f "$f"; elif [[ ! -e $f ]]; then echo pending > "$f"; fi
fi
status=none
if [[ -e $f ]]; then
  # Cloudflare looks at the name's record: active a look after it is there,
  # moved once it is gone.
  status="$(<"$f")"
  if [[ -e $STATE/record.$name ]]; then
    case "$status" in pending) echo seen > "$f" ;; seen) echo active > "$f"; status=active ;; esac
  elif [[ $status == active && ! -e $STATE/never-lapses ]]; then
    echo moved > "$f"; status=moved
  fi
  [[ $status != seen ]] || status=pending
fi
certificate=none; [[ $status == none ]] || certificate=initializing; [[ $status != active && $status != moved ]] || certificate=active
echo "state    $name: hostname=$status certificate=$certificate method=http"
# As the real script: a hostname that waits is drift to a check only.
if [[ $want == absent ]]; then [[ $status == none ]]; else [[ $status == active || ($mode == apply && $status != none) ]]; fi
`

func newPlayed(t *testing.T, bin string) *played {
	t.Helper()
	p := &played{t: t, state: t.TempDir(), stubs: t.TempDir(), cloud: t.TempDir(), records: t.TempDir()}
	sockets, err := os.MkdirTemp("/tmp", "alias-played")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockets) })
	socket := filepath.Join(sockets, "labd.sock")
	start(t, regexp127, filepath.Join(bin, "labd"), "--listen=127.0.0.1:0", "--socket="+socket, "--data="+filepath.Join(t.TempDir(), "data"),
		"--public-url="+labURL, "--alias="+rehearsal, "--omt=false")

	// The edge and the tunnel: a name is asked at /NAME/...; the tunnel
	// routes the Lab address and, once the mini was told, the rehearsal's
	// name, and answers 404 itself for any other. A name with no record
	// and no active hostname does not reach it at all.
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}},
		ModifyResponse: func(resp *http.Response) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.cookie {
				resp.Header.Add("Set-Cookie", "__cf_bm=a-value-nobody-prints; path=/; HttpOnly; Secure")
			}
			return nil
		},
	}
	p.edge = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if name != labName {
			if !p.has("record."+name) || p.said("hostname."+name) != "active" {
				http.Error(w, "<html><body>error code: 1001</body></html>", 530)
				return
			}
			if !p.has("switched") || (name != rehearsal && !p.has("routes-every-name")) {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		r.URL.Scheme, r.URL.Host, r.URL.Path, r.Host = "http", name, "/"+rest, name
		r.Header.Set("X-Forwarded-Proto", "https")
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(p.edge.Close)

	for _, name := range []string{"alias.sh", "rehearsal-record.sh"} {
		p.write(filepath.Join(p.cloud, name), fmt.Sprintf(cloudStub, name))
	}
	p.write(filepath.Join(p.stubs, "ssh"), `#!/bin/bash
echo "ssh $*" >> "$STATE/calls"
case "$*" in
  *"rehearsal-alias deployed") rm -f "$STATE/switched" ;;
  *"rehearsal-alias "*) [[ ! -e $STATE/mini-fails ]] || exit 1; : > "$STATE/switched" ;;
esac
`)
	p.write(filepath.Join(p.stubs, "dig"), `#!/bin/bash
echo "dig $*" >> "$STATE/calls"
case "$*" in
  *" NS "*) cat "$STATE/nameservers" ;;
  *" CNAME "*) name="$3"; [[ ! -e $STATE/record.$name ]] || echo "`+labName+`." ;;
esac
`)
	p.write(filepath.Join(p.stubs, "fd13"), "#!/bin/bash\necho \"fd13 $*\" >> \"$STATE/calls\"\n[[ ! -e $STATE/fd13-fails ]] || { echo 'fd13-cache: fail'; exit 1; }\necho 'fd13-cache: pass'\n")
	p.write(filepath.Join(p.state, "nameservers"), "curitiba.ns.porkbun.com.\nfortaleza.ns.porkbun.com.\nmaceio.ns.porkbun.com.\nsalvador.ns.porkbun.com.\n")
	p.write(filepath.Join(p.state, "token"), "not-a-token\n")
	if err := os.WriteFile(filepath.Join(p.state, "porkbun.keys"), []byte("pk1_not-a-key\nsk1_not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.env = []string{"PATH=" + p.stubs + ":" + os.Getenv("PATH"), "STATE=" + p.state, "LAB_CUTOVER_EDGE=" + p.edge.URL, "LAB_COMMAND=" + filepath.Join(bins(t), "lab")}
	return p
}

var regexp127 = regexp.MustCompile(`labd listening on (http://127\.0\.0\.1:\d+)`)

func (p *played) write(path, text string) {
	p.t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		p.t.Fatal(err)
	}
}

func (p *played) has(name string) bool {
	_, err := os.Stat(filepath.Join(p.state, name))
	return err == nil
}

func (p *played) said(name string) string {
	b, _ := os.ReadFile(filepath.Join(p.state, name))
	return strings.TrimSpace(string(b))
}

func (p *played) calls() string { return read(p.t, filepath.Join(p.state, "calls")) }

// rehearse runs the rehearsal with --lab in the made-up world.
func (p *played) rehearse(args ...string) (out string, code int, record string) {
	p.t.Helper()
	all := append([]string{"--lab", "--mini", "owner@mini.example", "--token-file", filepath.Join(p.state, "token"), "--porkbun-keys", filepath.Join(p.state, "porkbun.keys"), "--cloud", p.cloud,
		"--fd13", filepath.Join(p.stubs, "fd13"), "--records", p.records, "--every", "1", "--alert-minutes", "0", "--issue-minutes", "1", "--settle-minutes", "0", "--lapse-minutes", "1"}, args...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, code = run(p.t, p.env, "", cutover(p.t, "alias-rehearsal"), all...)
	}()
	select {
	case <-done:
	case <-time.After(4 * time.Minute):
		p.t.Fatal("the rehearsal did not end")
	}
	if files, _ := filepath.Glob(filepath.Join(p.records, "*-alias-rehearsal.md")); len(files) == 1 {
		record = read(p.t, files[0])
	}
	return out, code, record
}

// left is what of the rehearsal is still there after a run.
func (p *played) left() []string {
	var out []string
	for _, pattern := range []string{"hostname.*", "record.*", "switched"} {
		m, _ := filepath.Glob(filepath.Join(p.state, pattern))
		for _, f := range m {
			out = append(out, filepath.Base(f))
		}
	}
	return out
}

// The whole rehearsal as the owner starts it, with the route played: made,
// issued, asked, the record taken away, the lapse seen, everything deleted.
func TestTheRehearsalRunsFromMakingToDeleting(t *testing.T) {
	added := recordsAdded(t, "*alias-rehearsal*")
	p := newPlayed(t, bins(t))
	out, code, record := p.rehearse("--agents-line", "seen", "--charged", "nothing")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"# The Alias rehearsal", "--lab", "through " + labURL,
		"- before: omarchy-m-testing.org is on Porkbun's nameservers (4 of them)",
		"Charged, as he saw it: nothing",
		"- made: custom hostnames " + rehearsal + " and " + third,
		"- the certificate: pass. issued ", "with nothing but the CNAME in place (hostname=active certificate=active method=http)",
		"- the mini: the tunnel routes " + rehearsal,
		"- the forward: pass. 4 page requests", "- the API: pass.", "- lab: pass. lab login pointed at " + rehearsal + " failed, named the Lab address and stored nothing",
		"- cookies: pass.", "- a third name: pass. " + third + ", a custom hostname the tunnel's route file does not list, got 404 at the tunnel",
		"- the Front door's test 13: pass. still green on the Lab address: fd13-cache: pass",
		"- the record removed: pass. " + rehearsal + " no longer resolves at curitiba.ns.porkbun.com",
		"- the lapse: pass. Cloudflare shows the hostname as \"moved\"",
		"- the alert: pass. the owner saw \"Agents\" get \"The Alias is no longer active\"",
		"- deleted: pass.", "- the mini, after: pass.", "Result: pass",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if strings.Contains(record, "FAIL") || strings.Contains(out+record, "not-a-token") {
		t.Errorf("the record:\n%s", record)
	}
	if left := p.left(); len(left) != 0 {
		t.Errorf("after the run these are still there: %v", left)
	}
	calls := p.calls()
	if keys := "rehearsal-record.sh --apply --name " + rehearsal + " --token-file " + filepath.Join(p.state, "token") + " --porkbun-keys " + filepath.Join(p.state, "porkbun.keys"); !strings.Contains(calls, keys) {
		t.Errorf("the record's script was not given the Porkbun keys file:\n%s", calls)
	}
	order := []string{
		"dig +short NS omarchy-m-testing.org",
		"alias.sh --apply --name " + rehearsal, "rehearsal-record.sh --apply --name " + rehearsal,
		"alias.sh --apply --name " + third, "rehearsal-record.sh --apply --name " + third,
		"alias.sh --check --name " + rehearsal,
		"ssh -t owner@mini.example sudo /usr/local/lab/current/scripts/mini/rehearsal-alias " + rehearsal,
		"fd13 --lab --records",
		"rehearsal-record.sh --apply --remove --name " + rehearsal,
		"dig +short CNAME " + rehearsal + " @curitiba.ns.porkbun.com",
		"alias.sh --apply --remove --name " + rehearsal, "alias.sh --apply --remove --name " + third,
		"ssh -t owner@mini.example sudo /usr/local/lab/current/scripts/mini/rehearsal-alias deployed",
		"alias.sh --check --remove --name " + third,
	}
	at := 0
	for _, want := range order {
		i := strings.Index(calls[at:], want)
		if i < 0 {
			t.Fatalf("no %q after the calls before it:\n%s", want, calls)
		}
		at += i
	}
	if entries := added(); len(entries) != 0 {
		t.Errorf("the run left a record in docs/acceptance: %v", entries)
	}
}

// Each thing the rehearsal looks for fails the run when it is not seen, and
// whatever was made is deleted all the same.
func TestTheRehearsalFailsOnWhatItDoesNotSeeAndStillDeletesEverything(t *testing.T) {
	for what, c := range map[string]struct {
		bin    func() string
		before func(*played)
		args   []string
		says   string
	}{
		"labd does not forward the name":  {bin: func() string { return labdWithout(t, "--alias") }, args: []string{"--agents-line", "seen"}, says: "the forward: FAIL. / answered 421"},
		"an answer carries a cookie":      {before: func(p *played) { p.mu.Lock(); p.cookie = true; p.mu.Unlock() }, args: []string{"--agents-line", "seen"}, says: "cookies: FAIL. / set __cf_bm"},
		"the tunnel routes a third name":  {before: func(p *played) { p.write(filepath.Join(p.state, "routes-every-name"), "") }, args: []string{"--agents-line", "seen"}, says: "a third name: FAIL. / answered 421"},
		"the Front door's test 13 is red": {before: func(p *played) { p.write(filepath.Join(p.state, "fd13-fails"), "") }, args: []string{"--agents-line", "seen"}, says: "the Front door's test 13: FAIL. it failed on the Lab address"},
		"the hostname never lapses":       {before: func(p *played) { p.write(filepath.Join(p.state, "never-lapses"), "") }, args: []string{"--agents-line", "seen"}, says: "the lapse: FAIL. not seen within 1 minutes: Cloudflare still shows the hostname as active"},
		"\"Agents\" got no line":          {args: []string{"--agents-line", "not-seen"}, says: "the alert: FAIL. the owner did not see"},
		"nobody says whether it did":      {says: "the alert: FAIL. nobody was at the terminal"},
		"the mini does not take the name": {before: func(p *played) { p.write(filepath.Join(p.state, "mini-fails"), "") }, args: []string{"--agents-line", "seen"}, says: "the mini: FAIL."},
	} {
		bin := bins(t)
		if c.bin != nil {
			bin = c.bin()
		}
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			p := newPlayed(t, bin)
			if c.before != nil {
				c.before(p)
			}
			out, code, record := p.rehearse(c.args...)
			if code != 1 || !strings.Contains(record, c.says) || !strings.Contains(record, "Result: fail") {
				t.Errorf("exit %d, want 1 and %q\n%s\n%s", code, c.says, out, record)
			}
			if strings.Contains(record, "a-value-nobody-prints") || strings.Contains(out, "a-value-nobody-prints") {
				t.Error("a cookie's value was printed")
			}
			if left := p.left(); len(left) != 0 {
				t.Errorf("after the run these are still there: %v", left)
			}
			if !strings.Contains(record, "- deleted: pass.") {
				t.Errorf("the record does not say everything was deleted:\n%s", record)
			}
		})
	}
}

// The rehearsal needs the old site's domain still at Porkbun, a token file
// and the mini: without one of them nothing is made.
func TestTheRehearsalMakesNothingWhenItCannotRun(t *testing.T) {
	p := newPlayed(t, bins(t))
	p.write(filepath.Join(p.state, "nameservers"), "garret.ns.cloudflare.com.\nholly.ns.cloudflare.com.\n")
	out, code, record := p.rehearse("--agents-line", "seen")
	if code == 0 || !strings.Contains(out, "no longer on Porkbun's nameservers") || !strings.Contains(out, "step 19") || record != "" {
		t.Errorf("with the domain moved: exit %d\n%s", code, out)
	}
	p.write(filepath.Join(p.state, "nameservers"), "curitiba.ns.porkbun.com.\n")
	shared := filepath.Join(p.state, "shared.keys")
	if err := os.WriteFile(shared, []byte("pk1_x\nsk1_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for what, args := range map[string][]string{
		"no mini":           {"--lab", "--token-file", filepath.Join(p.state, "token"), "--cloud", p.cloud},
		"no token file":     {"--lab", "--mini", "owner@mini.example", "--cloud", p.cloud},
		"a missing token":   {"--lab", "--mini", "owner@mini.example", "--token-file", filepath.Join(p.state, "none"), "--cloud", p.cloud},
		"shell in the name": {"--lab", "--mini", "owner@mini.example", "--token-file", filepath.Join(p.state, "token"), "--cloud", p.cloud, "--name", "x;id"},
		"the site's name":   {"--lab", "--mini", "owner@mini.example", "--token-file", filepath.Join(p.state, "token"), "--cloud", p.cloud, "--name", "www"},
		"a mini with shell": {"--lab", "--mini", "owner@mini;id", "--token-file", filepath.Join(p.state, "token"), "--cloud", p.cloud},
		"no Porkbun keys":   {"--lab", "--mini", "owner@mini.example", "--token-file", filepath.Join(p.state, "token"), "--porkbun-keys", filepath.Join(p.state, "none"), "--cloud", p.cloud},
		"keys others read":  {"--lab", "--mini", "owner@mini.example", "--token-file", filepath.Join(p.state, "token"), "--porkbun-keys", shared, "--cloud", p.cloud},
	} {
		if out, code := run(t, p.env, "", cutover(t, "alias-rehearsal"), args...); code == 0 {
			t.Errorf("%s: it ran\n%s", what, out)
		}
	}
	if calls, _ := os.ReadFile(filepath.Join(p.state, "calls")); strings.Contains(string(calls), "--apply") || strings.Contains(string(calls), "ssh ") {
		t.Errorf("something was made or the mini was asked:\n%s", calls)
	}
	if left := p.left(); len(left) != 0 {
		t.Errorf("these were made: %v", left)
	}
}

// A 0600 Porkbun keys file whose ACL lets another account read it stops
// the rehearsal before anything is made, as a mode others read does.
func TestTheRehearsalRefusesPorkbunKeysWithAnACL(t *testing.T) {
	p := newPlayed(t, bins(t))
	keys := filepath.Join(p.state, "porkbun.keys")
	if exec.Command("chmod", "+a", "everyone allow read", keys).Run() != nil && exec.Command("setfacl", "-m", "u:nobody:r", keys).Run() != nil {
		t.Skip("no access control lists here")
	}
	if st, err := os.Stat(keys); err != nil || st.Mode().Perm() != 0o600 {
		t.Skip("the ACL changed the keys file's mode: the mode check covers it")
	}
	out, code, record := p.rehearse("--agents-line", "seen")
	if code == 0 || !strings.Contains(out, "ACL") || record != "" {
		t.Errorf("keys with an ACL: exit %d\n%s", code, out)
	}
	if calls, _ := os.ReadFile(filepath.Join(p.state, "calls")); strings.Contains(string(calls), "--apply") || strings.Contains(string(calls), "ssh ") {
		t.Errorf("something was made or the mini was asked:\n%s", calls)
	}
	if left := p.left(); len(left) != 0 {
		t.Errorf("these were made: %v", left)
	}
}

// Before the owner has entered payment details Cloudflare takes no call
// about custom hostnames: the run says what Cloudflare said, makes nothing
// at Porkbun and leaves the mini alone. The write it sent counts as made,
// so it is asked to be deleted, and the record does not claim it was.
func TestTheRehearsalStopsWhenCloudflareTakesNoCustomHostname(t *testing.T) {
	p := newPlayed(t, bins(t))
	p.write(filepath.Join(p.cloud, "alias.sh"), "#!/bin/bash\necho \"alias.sh $*\" >> \"$STATE/calls\"\necho 'alias.sh: GET /zones/{zone}/custom_hostnames: status 403, [1404] not enabled, ray 8f00ba4dfeed1234-BNE'\necho 'alias.sh: Cloudflare did not take a call about custom hostnames. They need payment details'\nexit 1\n")
	out, code, record := p.rehearse("--agents-line", "seen")
	if code != 1 || !strings.Contains(record, "- made: FAIL. Cloudflare did not take the custom hostname "+rehearsal) || !strings.Contains(out, "status 403") || !strings.Contains(out, "payment details") {
		t.Errorf("exit %d\n%s\n%s", code, out, record)
	}
	if !strings.Contains(record, "- deleted: FAIL. not seen gone, though the first write failed and may have made nothing") || strings.Contains(record, "deleted: pass") || !strings.Contains(record, "Result: fail") {
		t.Errorf("the record:\n%s", record)
	}
	calls := p.calls()
	if strings.Contains(calls, "rehearsal-record.sh --apply --name") || strings.Contains(calls, "ssh ") || !strings.Contains(calls, "alias.sh --apply --remove --name "+rehearsal) || len(p.left()) != 0 {
		t.Errorf("after Cloudflare's refusal the run went on, or did not ask for the deletion:\n%s", calls)
	}
}

// Cloudflare can take the hostname and the call still fail after it (a GET
// in alias.sh): the run counts it as made before it writes, and deletes it.
func TestAHostnameTakenByAFailedWriteIsDeletedAllTheSame(t *testing.T) {
	p := newPlayed(t, bins(t))
	p.write(filepath.Join(p.cloud, "alias.sh"), "#!/bin/bash\necho \"alias.sh $*\" >> \"$STATE/calls\"\nname=\"\"; want=present\nwhile (( $# )); do case \"$1\" in --name) name=\"$2\"; shift ;; --remove) want=absent ;; esac; shift; done\nif [[ $want == absent ]]; then rm -f \"$STATE/hostname.$name\"; exit 0; fi\necho pending > \"$STATE/hostname.$name\"\necho 'alias.sh: GET /zones/{zone}/custom_hostnames: status 502, ray 8f00ba4dfeed1234-BNE'\nexit 1\n")

	out, code, record := p.rehearse("--agents-line", "seen")

	if code != 1 || !strings.Contains(record, "- made: FAIL. Cloudflare did not take the custom hostname "+rehearsal) {
		t.Errorf("exit %d\n%s\n%s", code, out, record)
	}
	if left := p.left(); len(left) != 0 {
		t.Errorf("after the run these are still there: %v\n%s", left, p.calls())
	}
	if strings.Contains(p.calls(), "ssh ") {
		t.Errorf("the mini was asked:\n%s", p.calls())
	}
}

// The edge's stand-in is the tests' and takes a loopback address only.
func TestTheEdgesStandInTakesOnlyLoopback(t *testing.T) {
	for _, name := range []string{"alias-rehearsal", "dark-recordings"} {
		for _, edge := range []string{"https://example.org", "http://127.0.0.1:1@lab.invalid", "http://127.0.0.1:80/x"} {
			out, code := run(t, []string{"LAB_CUTOVER_EDGE=" + edge}, "", cutover(t, name), "--lab", "--mini", "owner@mini.example")
			if code == 0 || !strings.Contains(out, "loopback") {
				t.Errorf("%s with LAB_CUTOVER_EDGE=%s: exit %d\n%s", name, edge, code, out)
			}
		}
	}
}

// The draft of the ask is the ticket's, with the record the rehearsal
// proves, and nothing sends it.
func TestTheDraftOfTheAskForTheAlias(t *testing.T) {
	text := read(t, cutover(t, "alias-ask.md"))
	for _, want := range []string{
		"Hi Emir,", "Nobody sends it for him", "the lab is Confirmed", "Result: pass",
		"maclab   CNAME   lab.omarchy-mac.org   proxy status: DNS only (grey cloud)",
		"no access to your account and no tokens are involved", "If you'd rather not, no problem: the lab works without it.",
		"scripts/cloud/alias.sh --apply", "One reminder after 14 days, then dropped", "a redirect in their own zone", "nobody is chased",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the draft lacks %q", want)
		}
	}
	spec := read(t, filepath.Join(repo(t), "docs", "spec", "spec.md"))
	for _, want := range []string{"The owner asks Emir", "one reminder after 14 days, then dropped", "with the rehearsal green"} {
		if !strings.Contains(spec, want) {
			t.Errorf("the spec no longer says %q", want)
		}
	}
}
