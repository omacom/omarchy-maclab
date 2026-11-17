// Package hardening runs Hardening's fourteen tests
// (scripts/cutover/hardening/) as far as they go with no mini: their dry
// runs, and their local runs against a labd on loopback with a stand-in for
// GitHub and simulated Macs. Nothing here reaches the Lab address or the
// mini.
package hardening

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/testhome"
)

var (
	binOnce sync.Once
	binDir  string
	binErr  error
	// Each local run starts a labd and simulated Macs: four at a time.
	slots = make(chan struct{}, 4)
)

// No test here reads or writes the home of whoever runs the tests.
func TestMain(m *testing.M) {
	exit := testhome.Set()
	// The labd these tests build runs on the wall clock sped up this much.
	if os.Getenv("MACLAB_TEST_CLOCK_SCALE") == "" {
		os.Setenv("MACLAB_TEST_CLOCK_SCALE", "10")
	}
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	if probePath != "" {
		os.RemoveAll(filepath.Dir(probePath))
	}
	exit(code)
}

var tests = []string{"h01-console", "h02-exec-lease", "h03-freeze-entry", "h04-builder-token", "h05-build-sources",
	"h06-no-secret-shown", "h07-cuts", "h08-agents-lines", "h09-notice-topic", "h10-browser", "h11-exec-record-readers",
	"h12-agent-signature", "h13-lab-own-upload", "h14-volunteers-mac"}

func repo(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func script(t *testing.T, name string) string {
	return filepath.Join(repo(t), "scripts", "cutover", "hardening", name)
}

// bins builds labd, lab, lab-agent for this machine (its sim mode is the
// simulated Mac), the lab-agent labd hands out, the stand-in for GitHub and
// the probe, once.
func bins(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and starts labd")
	}
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "hardening-bin")
		if binErr != nil {
			return
		}
		for _, build := range [][]string{
			{"", "-tags", "labtest", "-o", binDir + "/", "./cmd/labd", "./cmd/lab", "./cmd/lab-agent", "./tools/fake-github", "./tools/hardening-probe"},
			{"GOOS=linux GOARCH=arm64", "-o", binDir + "/lab-agent-linux-arm64", "./cmd/lab-agent"},
		} {
			cmd := exec.Command("go", append([]string{"build"}, build[1:]...)...)
			cmd.Dir = repo(t)
			cmd.Env = append(os.Environ(), strings.Fields(build[0])...)
			if out, err := cmd.CombinedOutput(); err != nil {
				binErr = fmt.Errorf("go build: %v\n%s", err, out)
				return
			}
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binDir
}

// run starts a script with env added to the test's own and stdin at its
// end, and returns what it printed and its exit status.
func run(t *testing.T, env []string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("%s: %v\n%s", name, err, out)
	return "", 0
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// local runs one test against a labd on loopback and returns what it said,
// its exit status, its record and the results file.
func local(t *testing.T, env []string, name string) (out string, code int, record, results string) {
	t.Helper()
	bin := bins(t)
	slots <- struct{}{}
	defer func() { <-slots }()
	records := t.TempDir()
	out, code = run(t, env, script(t, name), "--local", "--bin", bin, "--records", records)
	if files, _ := filepath.Glob(filepath.Join(records, "*-hardening-"+name[1:3]+".md")); len(files) == 1 {
		record = read(t, files[0])
	}
	if b, err := os.ReadFile(filepath.Join(records, "hardening.results")); err == nil {
		results = string(b)
	}
	return out, code, record, results
}

// stubs is a folder of stand-ins for the programs a test may call, which
// write down how they were called and do what the test's text says.
func stubs(t *testing.T, text map[string]string) (dir, calls string) {
	t.Helper()
	dir = t.TempDir()
	calls = filepath.Join(dir, "calls")
	for _, tool := range []string{"lab", "ssh", "curl", "sqlite3", "python3", "go", "ssh-keygen", "hardening-probe"} {
		body := "#!/bin/sh\necho \"" + tool + " $*\" >> " + calls + "\n" + text[tool] + "\n"
		if text[tool] == "" {
			body += "exit 9\n"
		}
		if err := os.WriteFile(filepath.Join(dir, tool), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir, calls
}

// With no option each test says its steps and what each expects, names
// where the owner's hand is needed, and sends nothing.
func TestEveryDryRunSaysItsStepsAndSendsNothing(t *testing.T) {
	dir, calls := stubs(t, nil)
	env := []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
	byHand := map[string]int{"h07-cuts": 1, "h09-notice-topic": 4, "h10-browser": 1}
	for _, name := range tests {
		for _, option := range [][]string{nil, {"--dry-run"}} {
			out, code := run(t, env, script(t, name), option...)
			if code != 0 {
				t.Fatalf("%s %v: exit %d\n%s", name, option, code, out)
			}
			for _, want := range []string{name + ": dry run.", "expects:", "docs/acceptance/", "-hardening-" + name[1:3] + ".md", "hardening.results", "Nothing was sent"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s's dry run does not say %q:\n%s", name, want, out)
				}
			}
			if n := strings.Count(out, "STOP, by hand:"); n != byHand[name] {
				t.Errorf("%s's dry run names %d steps by hand, want %d:\n%s", name, n, byHand[name], out)
			}
		}
		if out, code := run(t, env, script(t, name), "--lab", "--open-everything"); code != 1 || !strings.Contains(out, "Nothing was done") {
			t.Errorf("%s took an option it does not know: exit %d\n%s", name, code, out)
		}
		if out, code := run(t, env, script(t, name), "--dry-run", "--lab"); code != 1 || !strings.Contains(out, "two modes") {
			t.Errorf("%s took two modes: exit %d\n%s", name, code, out)
		}
	}
	for name, why := range map[string]string{"h03-freeze-entry": "--lab needs --release R", "h06-no-secret-shown": "--lab needs --mini", "h09-notice-topic": "--lab needs --volunteers-mac NAME and --redirect URL"} {
		if out, code := run(t, env, script(t, name), "--lab"); code != 1 || !strings.Contains(out, why) {
			t.Errorf("%s --lab without what it needs: exit %d\n%s", name, code, out)
		}
	}
	out, code := run(t, env, script(t, "run"))
	if code != 0 || strings.Count(out, ": dry run.") != 14 || strings.Count(out, ": said ") != 14 {
		t.Errorf("run with no option: exit %d, %d dry runs\n%s", code, strings.Count(out, ": dry run."), out)
	}
	if out, code := run(t, env, script(t, "run"), "--only", "12,3"); code != 0 || strings.Count(out, ": dry run.") != 2 || strings.Index(out, "h12-") > strings.Index(out, "h03-") {
		t.Errorf("run --only 12,3: exit %d\n%s", code, out)
	}
	if out, code := run(t, env, script(t, "run"), "--skip", "1,2,3,4,5,6,7,8,9,10,11,12,13"); code != 0 || strings.Count(out, ": dry run.") != 1 || !strings.Contains(out, "h14-volunteers-mac: dry run.") {
		t.Errorf("run --skip all but 14: exit %d\n%s", code, out)
	}
	if out, code := run(t, env, script(t, "run"), "--dry-run", "--lab"); code != 2 || !strings.Contains(out, "two modes") {
		t.Errorf("run with two modes: exit %d\n%s", code, out)
	}
	if out, code := run(t, env, script(t, "run"), "--for", "3", "--lab --release R"); code != 2 || !strings.Contains(out, "not inside --for") {
		t.Errorf("run with a mode inside --for: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("a dry run, or a run refused for its options, called:\n%s", b)
	}
}

// Each of the fourteen against a labd on loopback: the lab's rules do what
// the test expects of them.
func TestTheFourteenRunLocally(t *testing.T) {
	t.Parallel()
	bins(t)
	// Records of real runs are committed there; a local run adds none.
	kept, _ := filepath.Glob(filepath.Join(repo(t), "docs", "acceptance", "*hardening*"))
	for name, wants := range map[string][]string{
		"h01-console": {"GET /api/devices/m1-pro/console: 410, The serial console is not part of Omarchy Mac Lab.", "POST /api/devices/m1-pro/console/arm: 410",
			"GET /api/devices/m1-pro/serial: 410", "serial-assisted: 410", "lab shell m1-pro: exit 1", "off the network, under the caller's Lease: 409",
			"the Exec record holds it: owner|echo through-the-console|refused|"},
		"h02-exec-lease": {"a Maintainer's lab exec on m1-pro with no Lease: 409, lab exec needs your own Lease on m1-pro, a Maintainer's too",
			"an M team member's lab exec with no Lease: 403", "a Maintainer's lab exec under the member's Lease: 409", "|ran|3|", "member|echo h02-"},
		"h03-freeze-entry": {"with no Lease: 409, freeze-entry on m1-pro needs your own Lease on it, a Maintainer's too", "the Exec record of m1-pro still holds 0 lines"},
		"h04-builder-token": {"a made-up builder token, POST /api/builder/poll: 401", "a Maintainer's Lab token, POST /api/builder/poll: 401",
			"holding no build, was refused both uploaded artifacts: 404", "the running build's config was served to its builder (200); the other artifact was refused (404",
			"a shared builder.token in labd's data folder, after a restart: 401", "its token is refused (401)"},
		"h05-build-sources": {"https://gitlab.com/hardening/linux: 400", "https://hardening:PASSWORD@github.com/hardening/linux: 400, a build's source is public: an address that carries credentials is refused",
			"http://github.com/hardening/linux: 400", "https://github.com/hardening/linux?access_token=PASSWORD: 400, a build's source is public: an address with a query is refused",
			"https://github.com/hardening/linux#access_token=PASSWORD: 400, a build's source is public: after # comes only a branch, tag or commit", "no build was made for any of them", "labd contacted nothing", "and the port does count"},
		"h06-no-secret-shown": {"no secret shows in a process or a service definition, and none is given by value"},
		"h07-cuts": {"the revoked token's next request: 401", "its stream ended: ", "a Maintainer used \"End all access\" on @member, on People",
			"the next request after \"End all access\": 401", "the stream ended: ", "@member's browser is signed out", "the Lease on m1-pro ended with it"},
		"h08-agents-lines": {"owner marked member as M team.", "owner made the Lab token \"h08-", "owner revoked member's Lab token \"h08-",
			"owner used \"End all access\" on member.", "owner removed the M team mark from member.", "People's record shows @owner marked @member"},
		"h10-browser": {"a signed-in browser's form from a page at https://elsewhere.example: 403", "Sec-Fetch-Site: cross-site: 403", "no Lab token was made by either",
			"a signed-out browser's upload from a page at https://elsewhere.example: 403", "sign-in asked to return to https://elsewhere.example/ landed on the home page",
			"sign-in asked to return to //elsewhere.example/ landed on the home page", "it landed there", "/api/themes/default/theme.css: 404",
			"opening the deletion link: 200, no-store, no referrer, a confirmation form", "opening the link deleted nothing"},
		"h11-exec-record-readers": {"a Maintainer reading the Exec record of loaner: 200, sees: maintainer member", "the Mac's Volunteer (@lender) reading the Exec record of loaner: 200, sees: maintainer member",
			"the member who ran one (@member) reading the Exec record of loaner: 200, sees: member", "a member who ran none (@other) reading the Exec record of loaner: 200",
			"a person with no role (@nobody) reading the Exec record of loaner: 403", "on a Mac that is not theirs reading the Exec record of m1-pro: 404",
			"a signed-out browser reading the Exec record of loaner: 401", "labd deleted the line past 12 months at its start"},
		"h12-agent-signature": {"a lab-agent changed after it was signed: refused by the Mac", "a lab-agent signed by another key: refused by the Mac",
			"an older lab-agent, 0.9.0, rightly signed: refused by the Mac", "older than the 0.10.0 installed", "lab-agent 0.10.0 -> 0.10.1"},
		"h13-lab-own-upload": {"in the query, the headers and the User-Agent: 201, stored", "its page does not say \"run by the lab\"",
			"Reports, for a Maintainer, says it arrived by the Mac's own tool", "a report carrying a lab_job key: 422", "the 31st of the hour: 429"},
		"h14-volunteers-mac": {"lab run loaner --installed: refused", "lab installed promote loaner: refused", "Promote on the Release's page: refused",
			"the site's boot form has no field for an Installed run", "POST /api/jobs, an Installed run: refused", "POST /api/installed/0.0.0-hardening-14/promote: refused", "no job was made on loaner"},
		"h09-notice-topic": {"reports its own topic, https://127.0.0.1:PORT/h09", "labd keeps that topic and refuses it: its host 127.0.0.1 is at 127.0.0.1, not a public address",
			"control's Notice reached the stand-in for the lab's notice server", "nothing connected to the topic's port, nor to the lab's notice server: the Notice was dropped"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, results := local(t, nil, name)
			want, word := 0, "pass"
			if code != want {
				t.Fatalf("exit %d, want %d\n%s", code, want, out)
			}
			for _, w := range append(wants, "It proves the script and the lab's rules, not the mini.", "Result: "+word) {
				if !strings.Contains(record, w) {
					t.Errorf("the record lacks %q:\n%s", w, record)
				}
			}
			if !strings.HasPrefix(results, name[1:3]+" "+word+" 20") || strings.Count(results, "\n") != 1 {
				t.Errorf("the results file: %q", results)
			}
			for _, secret := range []string{"enr_", "lab_0", "lab_1", "lab_2", "lab_3", "lab_4", "lab_5", "lab_6", "lab_7", "lab_8", "lab_9", "lab_a", "lab_b", "lab_c", "lab_d", "lab_e", "lab_f"} {
				if strings.Contains(out, secret) {
					t.Errorf("a token was printed (%s):\n%s", secret, out)
				}
			}
		})
	}
	t.Cleanup(func() {
		all, _ := filepath.Glob(filepath.Join(repo(t), "docs", "acceptance", "*hardening*"))
		if left := slices.DeleteFunc(all, func(f string) bool { return slices.Contains(kept, f) }); len(left) != 0 {
			t.Errorf("a local run left a record in docs/acceptance: %v", left)
		}
	})
}

// The deletion link is a secret: it reaches curl on stdin, never in its
// arguments, where the process list would show its token.
func TestH10GivesCurlTheDeletionLinkOnStdin(t *testing.T) {
	t.Parallel()
	real, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("no curl")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "curl.args")
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\necho \"$*\" >> "+log+"\nexec "+real+" \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code, record, _ := local(t, []string{"PATH=" + dir + ":" + os.Getenv("PATH")}, "h10-browser")
	if code != 0 || !strings.Contains(record, "opening the deletion link: 200") {
		t.Fatalf("exit %d\n%s\n%s", code, out, record)
	}
	args := read(t, log)
	if strings.Contains(args, "token=") || strings.Contains(args, "/deletion?") {
		t.Errorf("curl was given the deletion link as an argument:\n%s", args)
	}
}

// A secret put where the rule says it must not be is found: in a service
// definition, and on a command line.
func TestH06FindsASecretShown(t *testing.T) {
	t.Parallel()
	for fault, want := range map[string]string{
		"secret-in-definition": "FAILED: a secret shows in a service definition",
		"secret-in-process":    "FAILED: a secret shows in the process list (1)",
	} {
		t.Run(fault, func(t *testing.T) {
			t.Parallel()
			out, code, record, results := local(t, []string{"HARDENING_FAULT=" + fault}, "h06-no-secret-shown")
			if code != 1 || !strings.Contains(record, want) || !strings.HasPrefix(results, "06 fail ") {
				t.Fatalf("exit %d, results %q\n%s\n%s", code, results, out, record)
			}
			if fault == "secret-in-definition" && !strings.Contains(record, "shape=flag:org.omarchy-mac.lab.fault.plist") {
				t.Errorf("the shape of a --token on the definition's command line was not named:\n%s", record)
			}
		})
	}
}

// probeArgs is the start of a stand-in for the probe: out is its --out file.
const probeArgs = `out=""; prev=""; for a in "$@"; do [ "$prev" != --out ] || out="$a"; prev="$a"; done
`

// labRun runs a test through a made-up Lab address: lab says it is logged
// in, the probe answers as probe (a case on "$*") says, and stdin is what
// the owner types. It returns what the test said, its exit status, its
// record and how the probe and lab were called.
func labRun(t *testing.T, name, lab, probe, stdin string, env []string, args ...string) (out string, code int, record, calls string) {
	t.Helper()
	l := labCommand(t, name, lab, probe, stdin, env, args...)
	b, err := l.cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b), code, l.record(), l.calls()
}

// labTest is a test made ready to run through a made-up Lab address.
type labTest struct {
	t                    *testing.T
	cmd                  *exec.Cmd
	stubs, name, records string
}

func labCommand(t *testing.T, name, lab, probe, stdin string, env []string, args ...string) *labTest {
	t.Helper()
	if lab == "" {
		lab = `case "$1" in status) echo "NAME STATE" ;; *) exit 9 ;; esac`
	}
	dir, _ := stubs(t, map[string]string{"lab": lab, "hardening-probe": probeArgs + probe})
	records := t.TempDir()
	cmd := exec.Command(script(t, name), append([]string{"--lab", "--bin", dir, "--records", records}, args...)...)
	cmd.Env = append(append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "LAB_ADDRESS=https://lab.example"), env...)
	cmd.Stdin = strings.NewReader(stdin)
	return &labTest{t: t, cmd: cmd, stubs: dir, name: name, records: records}
}

func (l *labTest) record() string {
	if files, _ := filepath.Glob(filepath.Join(l.records, "*-hardening-"+l.name[1:3]+".md")); len(files) == 1 {
		return read(l.t, files[0])
	}
	return ""
}

func (l *labTest) calls() string {
	c, _ := os.ReadFile(filepath.Join(l.stubs, "calls"))
	return string(c)
}

// H 8 through the Lab address: the owner's browser makes a token and marks
// a person M team. A "no" at a question leaves the lab as it was. Once the
// mark is posted People shows the person as H08_KIND_AFTER (M team), and
// the newest mark in their record is H08_MARKED_BY's (the owner's).
const h08Probe = `S="$(dirname "$0")"
case "$*" in
  "page "*'data-label="Kind"'*)
    echo status=200
    if [ -f "$S/marked" ]; then kind="${H08_KIND_AFTER-M team}"; else kind="${H08_KIND:-}"; fi
    [ -z "$kind" ] || echo "found=$kind" ;;
  "page "*"marked as M team"*" /people?open=42")
    [ -z "${H08_RECORD_AT:-}" ] || LAB_ADDRESS="$H08_RECORD_AT" exec "$H08_REAL_PROBE" "$@"
    by="${H08_MARKED_BY-owner}"
    [ ! -f "$S/cut" ] || by="${H08_MARKED_AFTER_CUT:-$by}"
    echo status=200; [ -z "$by" ] || echo "found=$by" ;;
  "page "*"--find /me/tokens/"*) [ -z "${H08_ME_FAILS:-}" ] || { echo status=500; exit 0; }; echo status=200; echo found=7 ;;
  "page "*"--find /people/"*"end-all-access"*) echo status=200; echo found=42 ;;
  "page "*"<h1>@"*" /me") echo status=200; echo found=owner ;;
  "page "*" /me") echo status=200 ;;
  "form "*" /me/tokens name="*) echo lab_0123456789012345678901234567890123456789 > "$out"; echo status=200 ;;
  "form "*" /me/tokens/7/revoke sure=yes") echo status=303 ;;
  "form "*" /people/mark handle=someone as=m-team")
    if [ -n "${H08_MARK_HANGS:-}" ]; then
      # labd answers once the test writes the answer: 303 with the mark
      # written, or the post's own deadline (0) with the write still to land.
      touch "$S/posted"
      until [ -f "$S/answer" ]; do sleep 0.05; done
      a="$(cat "$S/answer")"
      if [ "$a" = 0 ]; then (sleep 1; touch "$S/marked") & else touch "$S/marked"; fi
      echo "status=$a"
      exit 0
    fi
    touch "$S/marked"
    echo "status=${H08_MARK:-303}" ;;
  "form "*" /people/42/end-all-access sure=yes") touch "$S/cut"; echo status=303 ;;
  "form "*" /people/42/m-team/remove sure=yes") echo status=303 ;;
  *) echo status=500 ;;
esac`

// At its normal end H 8 looks at the mark again right before it takes it
// off: one another Maintainer gave in its place while the owner answered a
// question stays theirs.
func TestH08sNormalEndTakesOffOnlyItsOwnMark(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env     []string
		code    int
		removed bool
		want    string
	}{
		"its own mark":                        {nil, 0, true, "the owner took the M team mark off someone again"},
		"a mark replaced while it was asking": {[]string{"H08_MARKED_AFTER_CUT=maint"}, 3, false, "left: @maint marked someone as M team, not this test"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, calls := labRun(t, "h08-agents-lines", "", h08Probe, "session\nyes\nyes\nyes\nyes\n", c.env, "--other", "someone")
			if code != c.code || !strings.Contains(record, c.want) {
				t.Fatalf("exit %d, want %d and %q\n%s\n%s", code, c.code, c.want, out, record)
			}
			if got := strings.Contains(calls, "/people/42/m-team/remove"); got != c.removed {
				t.Errorf("the mark taken off: %v, want %v:\n%s", got, c.removed, calls)
			}
		})
	}
}

// A mark whose answer was lost is taken off only when People's record shows
// it is the owner's, whose browser H 8 is: another Maintainer's mark made
// after H 8's look stays. With no answer and no mark the write may still
// land: that is not undone.
func TestH08TakesOffOnlyAMarkOfItsOwnWhoseAnswerWasLost(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		mark    string
		env     []string
		removed bool
		want    string
	}{
		"a lost 409 over another Maintainer's mark": {"0", []string{"H08_MARKED_BY=maint"}, false, "left: @maint marked someone as M team, not this test"},
		"no answer and no mark yet":                 {"0", []string{"H08_KIND_AFTER=", "H08_MARKED_BY="}, false, "NOT UNDONE: the post that marks someone as M team had no answer"},
		"an answer and no mark made":                {"502", []string{"H08_KIND_AFTER=", "H08_MARKED_BY="}, false, "undone: People shows no M team mark on someone"},
		"the test's own mark":                       {"0", nil, true, "undone: the M team mark was taken off someone"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, calls := labRun(t, "h08-agents-lines", "", h08Probe, "session\nyes\n", append(c.env, "H08_MARK="+c.mark), "--other", "someone")
			if code != 1 || !strings.Contains(record, "FAILED: someone was not marked M team: "+c.mark) {
				t.Fatalf("exit %d\n%s\n%s", code, out, record)
			}
			if got := strings.Contains(calls, "/people/42/m-team/remove"); got != c.removed {
				t.Errorf("the mark taken off: %v, want %v:\n%s", got, c.removed, calls)
			}
			if !strings.Contains(record, c.want) {
				t.Errorf("the record lacks %q:\n%s", c.want, record)
			}
		})
	}
}

var (
	probeOnce sync.Once
	probePath string
	probeErr  error
)

// realProbe is tools/hardening-probe, built once.
func realProbe(t *testing.T) string {
	t.Helper()
	probeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "hardening-probe")
		if probeErr = err; err != nil {
			return
		}
		probePath = filepath.Join(dir, "hardening-probe")
		cmd := exec.Command("go", "build", "-o", probePath, "./tools/hardening-probe")
		cmd.Dir = repo(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			probeErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	return probePath
}

// Who gave the newest mark is read from the person's record on People in
// its order, newest first, through the real probe: an older mark by the
// owner under a newer one by another Maintainer is the other's, and stays.
func TestH08ReadsWhoGaveTheNewestMarkInThePagesOrder(t *testing.T) {
	t.Parallel()
	probe := realProbe(t)
	people := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/people" || r.URL.Query().Get("open") != "42" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `<ol class="record">
<li><span class="when">2026-11-16 04:10</span> @zed marked as M team @someone</li>
<li><span class="when">2026-11-16 04:02</span> @zed removed the M team mark from @someone</li>
<li><span class="when">2026-11-15 21:40</span> @owner marked as M team @someone</li>
</ol>`)
	}))
	defer people.Close()
	out, code, record, calls := labRun(t, "h08-agents-lines", "", h08Probe, "session\nyes\n",
		[]string{"H08_MARK=0", "H08_RECORD_AT=" + people.URL, "H08_REAL_PROBE=" + probe}, "--other", "someone")
	if code != 1 || !strings.Contains(record, "FAILED: someone was not marked M team: 0") {
		t.Fatalf("exit %d\n%s\n%s", code, out, record)
	}
	if strings.Contains(calls, "/people/42/m-team/remove") || !strings.Contains(record, "left: @zed marked someone as M team, not this test") {
		t.Errorf("a newer mark by another Maintainer was not left as theirs:\n%s\n%s", calls, record)
	}
}

// A Ctrl-C while the mark's post is out checks the mark once labd's request
// has ended: a mark written just before labd answers is the test's own and
// is taken off; with no answer by the post's deadline and no mark yet, the
// write may still land after, and the record says NOT UNDONE.
func TestACtrlCWhileH08MarksChecksTheMarkOnceLabdAnswered(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		answer  string
		removed bool
		want    string
	}{
		"labd marks and answers after the Ctrl-C": {"303", true, "undone: the M team mark was taken off someone"},
		"the mark lands after the post gave up":   {"0", false, "NOT UNDONE: the post that marks someone as M team had no answer"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := labCommand(t, "h08-agents-lines", "", h08Probe, "session\nyes\n", []string{"H08_MARK_HANGS=1"}, "--other", "someone")
			l.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := l.cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
			for end := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
				if _, err := os.Stat(filepath.Join(l.stubs, "posted")); err == nil {
					break
				}
				if time.Now().After(end) {
					t.Fatalf("the mark was not posted:\n%s\n%s", l.calls(), l.record())
				}
			}
			syscall.Kill(-l.cmd.Process.Pid, syscall.SIGINT)
			done := make(chan error, 1)
			go func() { done <- l.cmd.Wait() }()
			select {
			case <-done:
				t.Fatalf("the run ended before labd answered the mark:\n%s\n%s", l.calls(), l.record())
			case <-time.After(time.Second):
			}
			if err := os.WriteFile(filepath.Join(l.stubs, "answer"), []byte(c.answer), 0o644); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the run did not end after labd answered")
			}
			record := l.record()
			if got := strings.Contains(l.calls(), "/people/42/m-team/remove sure=yes"); got != c.removed {
				t.Errorf("the mark taken off: %v, want %v:\n%s", got, c.removed, l.calls())
			}
			if !strings.Contains(record, c.want) || !strings.Contains(record, "Result: stopped") || strings.Contains(record, "undone: People shows no M team mark") {
				t.Errorf("the record lacks %q:\n%s", c.want, record)
			}
		})
	}
}

func TestANoAtH08sQuestionLeavesTheLabAsItWas(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ stdin, undone, record string }{
		"the token's line":  {"session\nno\n", "/me/tokens/7/revoke sure=yes", "undone: the Lab token h08-"},
		"the M team mark's": {"session\nyes\nno\n", "/people/42/m-team/remove sure=yes", "undone: the M team mark was taken off someone"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, calls := labRun(t, "h08-agents-lines", "", h08Probe, c.stdin, nil, "--other", "someone")
			if code != 1 || !strings.Contains(record, "Result: fail") {
				t.Fatalf("exit %d\n%s\n%s", code, out, record)
			}
			if !strings.Contains(calls, c.undone) {
				t.Errorf("the change was not undone (%s):\n%s", c.undone, calls)
			}
			if !strings.Contains(record, c.record) || strings.Index(record, c.record) > strings.Index(record, "Result: fail") {
				t.Errorf("the record does not say it was undone before its result:\n%s", record)
			}
		})
	}
}

// H 8 never takes away a mark it did not give: a person already on the M
// team is refused before anything is changed, and a mark the lab refused
// is not undone.
func TestH08LeavesAnExistingMemberMarked(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env   []string
		stdin string
		code  int
		want  string
	}{
		"People shows them on the M team": {[]string{"H08_KIND=M team"}, "session\n", 3, "STOPPED: @someone is on the M team already (M team)"},
		"People shows them a Maintainer":  {[]string{"H08_KIND=Maintainer"}, "session\n", 3, "STOPPED: @someone is on the M team already (Maintainer)"},
		"the lab refuses the mark":        {[]string{"H08_MARK=409"}, "session\nyes\n", 1, "FAILED: someone was not marked M team: 409"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, calls := labRun(t, "h08-agents-lines", "", h08Probe, c.stdin, c.env, "--other", "someone")
			if code != c.code || !strings.Contains(record, c.want) {
				t.Fatalf("exit %d, want %d and %q\n%s\n%s", code, c.code, c.want, out, record)
			}
			if strings.Contains(calls, "/m-team/remove") || strings.Contains(record, "the M team mark was taken off") {
				t.Errorf("the mark was taken off a person the test did not mark:\n%s\n%s", calls, record)
			}
			if c.code == 3 && (strings.Contains(calls, "/people/mark") || strings.Contains(calls, "/me/tokens name=")) {
				t.Errorf("the run changed the lab before it refused:\n%s", calls)
			}
		})
	}
}

// The token's undo says it is done only once it has read the owner's page:
// a /me that does not open says nothing of the token, which may be live.
func TestATokenUndoThatCannotReadMeSaysNotUndone(t *testing.T) {
	t.Parallel()
	out, code, record, _ := labRun(t, "h08-agents-lines", "", h08Probe, "session\nno\n", []string{"H08_ME_FAILS=1"}, "--other", "someone")
	if code != 1 || !strings.Contains(record, "Result: fail") {
		t.Fatalf("exit %d\n%s\n%s", code, out, record)
	}
	if strings.Contains(record, "undone: no Lab token") || !strings.Contains(record, "NOT UNDONE: the Lab token h08-") || !strings.Contains(record, "(500)") {
		t.Errorf("the record does not say the token may still be live:\n%s", record)
	}
}

// H 7 through the Lab address: the owner's token and the Lease on the Mac.
// A hold ends when the cut it waits for (cut.1, then cut.2) is made.
const h07Lab = `case "$1" in
  status) echo "NAME STATE" ;;
  lease | release) exit 0 ;;
  *) exit 9 ;;
esac`

const h07Probe = `S="$(dirname "$0")"
case "$*" in
  "hold "*)
    echo opened=200
    n="$(grep -c '^hardening-probe hold' "$S/calls")"
    until [ -f "$S/cut.$n" ]; do sleep 0.1; done
    echo seconds=1
    echo "ended=${H07_ENDED:-closed}" ;;
  "page "*"--find /me/tokens/"*) echo status=200; echo found=7 ;;
  "page "*" /me") if [ -f "$S/cut.2" ]; then echo status=303; else echo status=200; fi ;;
  "form "*" /me/tokens name="*) echo lab_0123456789012345678901234567890123456789 > "$out"; echo status=200 ;;
  "form "*" /me/tokens/7/revoke sure=yes") touch "$S/cut.1"; echo status=303 ;;
  "form "*" /me/end-all-access sure=yes") [ -z "${H07_CUT_FAILS:-}" ] || { echo status=500; exit 0; }; touch "$S/cut.2"; echo status=303 ;;
  "ask "*" GET /api/devices")
    if [ -n "${H07_REFUSED:-}" ]; then echo status=500; elif [ -f "$S/cut.1" ]; then echo status=401; else echo status=200; fi ;;
  *) echo status=500 ;;
esac`

func TestH07UndoesItsTokenAndLeaseWhenItEndsFirst(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env    string
		undone string
	}{
		"a failure before the token is revoked": {"H07_REFUSED=1", "hardening-probe form --out"},
		"a failure before \"End all access\"":   {"H07_CUT_FAILS=1", "lab release m1-pro"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, calls := labRun(t, "h07-cuts", h07Lab, h07Probe, "session\n", []string{c.env})
			if code != 1 || !strings.Contains(record, "Result: fail") {
				t.Fatalf("exit %d\n%s\n%s", code, out, record)
			}
			if name == "a failure before the token is revoked" {
				if !strings.Contains(calls, "/me/tokens/7/revoke sure=yes") || !strings.Contains(record, "undone: the Lab token h07-") {
					t.Errorf("the token was not revoked:\n%s\n%s", calls, record)
				}
				return
			}
			if !strings.Contains(calls, c.undone) || !strings.Contains(record, "undone: the Lease on m1-pro was released") {
				t.Errorf("the Lease was not released:\n%s\n%s", calls, record)
			}
		})
	}
}

// A stream counts as ended by the cut only when labd closed it: a hold that
// ran out or broke proves nothing.
func TestH07WantsTheStreamClosedByTheCut(t *testing.T) {
	t.Parallel()
	for ended, want := range map[string]int{"closed": 0, "timeout": 1, "error": 1} {
		t.Run(ended, func(t *testing.T) {
			t.Parallel()
			out, code, record, _ := labRun(t, "h07-cuts", h07Lab, h07Probe, "session\n\n", []string{"H07_ENDED=" + ended})
			if code != want {
				t.Fatalf("exit %d, want %d\n%s\n%s", code, want, out, record)
			}
			if want == 1 && !strings.Contains(record, "FAILED: the revoked token's stream ended="+ended+", not closed by labd") {
				t.Errorf("the record:\n%s", record)
			}
		})
	}
}

// A run that starts takes its old line out of the results at once: stopped
// half way, even by SIGKILL, it leaves no old pass for run --results.
func TestARunThatStartsTakesItsOldPassOutOfTheResults(t *testing.T) {
	t.Parallel()
	dir, calls := stubs(t, map[string]string{
		"lab":             `case "$1" in status) echo "NAME STATE" ;; *) exit 9 ;; esac`,
		"hardening-probe": "exec sleep 30",
	})
	records := t.TempDir()
	results := filepath.Join(records, "hardening.results")
	if err := os.WriteFile(results, []byte("07 pass 2026-11-14 2026-11-14-hardening-07.md\n08 pass 2026-11-14 2026-11-14-hardening-08.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script(t, "h07-cuts"), "--lab", "--bin", dir, "--records", records)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "LAB_ADDRESS=https://lab.example")
	cmd.Stdin = strings.NewReader("session\n")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	for end := time.Now().Add(20 * time.Second); ; {
		if b, _ := os.ReadFile(calls); strings.Contains(string(b), "hardening-probe page") {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the run did not reach the lab")
		}
		time.Sleep(50 * time.Millisecond)
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait()
	got := read(t, results)
	if !regexp.MustCompile(`(?m)^07 stopped 20\d\d-\d\d-\d\d 20\d\d-\d\d-\d\d-hardening-07\.md$`).MatchString(got) || !strings.Contains(got, "08 pass 2026-11-14 ") || strings.Contains(got, "07 pass") {
		t.Errorf("the results after a run cut half way:\n%s", got)
	}
}

// H 9 through the Lab address, as the owner: an Admin, whose /my-macs is
// Mine on Macs. The card shows a private topic refused until lab status
// sees the Mac report the redirect, and the redirect refused once a job
// has made a Notice.
const h09Lab = `S="$(dirname "$0")"
case "$1" in
  status) echo "NAME STATE"; [ -z "${2:-}" ] || touch "$S/reported" ;;
  run)
    echo "$MACLAB_HOLDER" > "$S/holder"; touch "$S/job"
    [ -z "${H09_RUN_HANGS:-}" ] || { touch "$S/posted"; exec sleep 30; }
    echo "job j-1 queued on $2" ;;
  cancel) echo "$2" >> "$S/canceled"; echo "canceled" ;;
  *) exit 9 ;;
esac`

// h09Jobs: the Mac's jobs as labd lists them: another person's, queued, and
// once lab run was called the run's own, under the holder it was given.
const h09Jobs = `  "ask "*"GET /api/jobs?device=m2-max&limit=50")
    echo status=200
    {
      printf '[{"id":"j-0","spec":{"device":"m2-max","tests":[{"name":"boot-health"}],"holder":"someone@laptop"},"state":"queued","events":[{"state":"queued"}]}'
      [ ! -f "$S/job" ] || printf ',{"id":"j-1","spec":{"device":"m2-max","tests":[{"name":"boot-health"}],"holder":"%s"},"state":"queued","events":[{"state":"queued"}]}' "$(cat "$S/holder")"
      echo ']'
    } > "$out" ;;
`

const h09Probe = `S="$(dirname "$0")"
card() {
  echo '<section class="card mac-card" id="mac-other"><span class="flash">Your own topic is refused: it answers with a redirect, which labd does not follow.</span></section>'
  echo '<section class="card mac-card" id="mac-m2-max">'
  echo "<dt>Notices</dt><dd><span class=\"copyable\"><code data-copy>$1</code> <button class=\"button button-small button-plain copy-button\" type=\"button\" aria-label=\"Copy the Notices topic\" hidden>Copy</button></span> <span class=\"nowrap\">job Notices $2</span>$3</dd>"
  echo '</section>'
}
case "$*" in
` + h09Jobs + `  "page "*" /me") echo status=200 ;;
  "page "*" /my-macs") echo status=303; echo "location=/macs?show=mine" ;;
  "page "*" /macs?show=mine")
    echo status=200
    if [ ! -f "$S/before-read" ]; then
      touch "$S/before-read"
      card https://ntfy.example/mine-h09 off '' > "$out"
    elif [ -f "$S/canceled" ]; then
      # Put back at the Mac: labd keeps a topic the Mac no longer reports.
      if [ -n "${H09_TOPIC_STAYS:-}" ]; then card https://redirect.example/h09 off '' > "$out"; else card https://ntfy.example/mine-h09 off '' > "$out"; fi
    elif [ -f "$S/job" ] && [ -z "${H09_NO_REDIRECT:-}" ]; then
      card https://redirect.example/h09 on '<span class="flash">Your own topic is refused: it answers with a redirect, which labd does not follow.</span>' > "$out"
    elif [ -f "$S/reported" ]; then
      card https://redirect.example/h09 on '' > "$out"
    else
      card https://192.168.0.1/h09 off '<span class="flash">Your own topic is refused: its host 192.168.0.1 is at 192.168.0.1, not a public address.</span>' > "$out"
    fi ;;
  *) echo status=500 ;;
esac`

func TestH09ReadsTheMacsCardAsAnAdmin(t *testing.T) {
	t.Parallel()
	args := []string{"--volunteers-mac", "m2-max", "--redirect", "https://redirect.example/h09"}
	out, code, record, calls := labRun(t, "h09-notice-topic", h09Lab, h09Probe, "session\n\n\n\nyes\nyes\n\n", []string{"H09_WAIT=10"}, args...)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, record)
	}
	for _, want := range []string{"the topic at a private address: m2-max's card says: Your own topic is refused: its host 192.168.0.1",
		"job j-1 on m2-max made a Notice to the topic behind a redirect, and was canceled", "the topic behind a redirect: m2-max's card says: Your own topic is refused: it answers with a redirect",
		"undone: m2-max's topic and job Notices are as they were before the test, as its card shows"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if !strings.Contains(calls, "lab run m2-max --no-wait") || !strings.Contains(calls, "lab cancel j-1") {
		t.Errorf("no job made the Notice, or it was not canceled:\n%s", calls)
	}
	// The Mac's own topic is its only secret: it is read, and never written.
	if strings.Contains(out, "ntfy.example/mine-h09") || strings.Contains(record, "ntfy.example/mine-h09") {
		t.Errorf("the Mac's topic was shown:\n%s\n%s", out, record)
	}
}

// H 9 copies the Mac's Notice settings aside before it changes them, and
// sees them back on the card at its end; what it cannot see back, or did not
// put back, the record names NOT UNDONE.
func TestH09PutsTheMacsNoticeSettingsBack(t *testing.T) {
	t.Parallel()
	args := []string{"--volunteers-mac", "m2-max", "--redirect", "https://redirect.example/h09"}
	t.Run("the topic stays the test's", func(t *testing.T) {
		t.Parallel()
		out, code, record, _ := labRun(t, "h09-notice-topic", h09Lab, h09Probe, "session\n\n\n\nyes\nyes\n\n", []string{"H09_WAIT=4", "H09_TOPIC_STAYS=1"}, args...)
		if code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, record)
		}
		if !strings.Contains(record, "NOT UNDONE: m2-max's Notices still go to another topic than before the test") || strings.Contains(record, "undone: m2-max's topic") {
			t.Errorf("a topic not back was not named:\n%s", record)
		}
		if strings.Contains(record, "job Notices are") {
			t.Errorf("job Notices that came back were named:\n%s", record)
		}
	})
	t.Run("the run ends first", func(t *testing.T) {
		t.Parallel()
		out, code, record, calls := labRun(t, "h09-notice-topic", h09Lab, h09Probe, "session\n\n\n\nyes\n", []string{"H09_WAIT=4", "H09_NO_REDIRECT=1"}, args...)
		if code != 1 || !strings.Contains(record, "FAILED: m2-max's card does not show the topic behind a redirect refused") {
			t.Fatalf("exit %d\n%s\n%s", code, out, record)
		}
		if !strings.Contains(calls, "lab cancel j-1") {
			t.Errorf("the job was left:\n%s", calls)
		}
		if !strings.Contains(record, "NOT UNDONE: m2-max's topic and job Notices are still the test's: at m2-max, put them back: sudo sh -c 'set -e; cd /var/lib/maclab-h09") {
			t.Errorf("the record does not say how to put the settings back:\n%s", record)
		}
	})
	t.Run("nobody there to copy them aside", func(t *testing.T) {
		t.Parallel()
		out, code, record, _ := labRun(t, "h09-notice-topic", h09Lab, h09Probe, "session\n", []string{"H09_WAIT=4"}, args...)
		if code != 3 || !strings.Contains(out, "sudo sh -c 'set -e; mkdir -m 700 /var/lib/maclab-h09") || strings.Contains(record, "UNDONE") {
			t.Errorf("exit %d\n%s\n%s", code, out, record)
		}
	})
}

// H 9's restore command, run as root at the Mac, stops at the first step
// that fails and keeps the copy aside: a topic that could not be copied
// back is not lost with it.
func TestH09sRestoreKeepsTheCopyWhenAStepFails(t *testing.T) {
	t.Parallel()
	out, code := run(t, nil, script(t, "h09-notice-topic"), "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	m := regexp.MustCompile(`put its Notice settings back, as root: sudo sh -c '([^']*)'`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the dry run does not say the restore command:\n%s", out)
	}
	root := t.TempDir()
	agent, saved := filepath.Join(root, "maclab"), filepath.Join(root, "maclab-h09")
	for _, d := range []string{agent, saved} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(saved, "ntfy-topic"), []byte("https://ntfy.example/mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The topic cannot be copied back: the agent's folder takes no file.
	if err := os.Chmod(agent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(agent, 0o700) })
	restore := strings.NewReplacer("/var/lib/maclab-h09", saved, "/var/lib/maclab", agent).Replace(m[1])
	if _, err := exec.Command("sh", "-c", restore).CombinedOutput(); err == nil {
		t.Error("the restore said it worked")
	}
	if got, err := os.ReadFile(filepath.Join(saved, "ntfy-topic")); err != nil || !strings.Contains(string(got), "ntfy.example/mine") {
		t.Errorf("the copy aside was not kept: %q, %v", got, err)
	}
}

// H 9's job is armed by the holder it is asked under before it is asked
// for: a Ctrl-C while lab run is still out cancels it, and nobody else's.
func TestACtrlCRightAfterH09sPostCancelsTheJob(t *testing.T) {
	t.Parallel()
	l := labCommand(t, "h09-notice-topic", h09Lab, h09Probe, "session\n\n\n\nyes\n", []string{"H09_WAIT=10", "H09_RUN_HANGS=1"},
		"--volunteers-mac", "m2-max", "--redirect", "https://redirect.example/h09")
	l.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := l.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-l.cmd.Process.Pid, syscall.SIGKILL)
	for end := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(l.stubs, "posted")); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("lab run was not called:\n%s\n%s", l.calls(), l.record())
		}
	}
	// Ctrl-C at the owner's terminal: the whole group gets it.
	syscall.Kill(-l.cmd.Process.Pid, syscall.SIGINT)
	done := make(chan error, 1)
	go func() { done <- l.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the run did not end after Ctrl-C")
	}
	canceled, _ := os.ReadFile(filepath.Join(l.stubs, "canceled"))
	if string(canceled) != "j-1\n" {
		t.Errorf("canceled %q, want the run's own job j-1 alone:\n%s", canceled, l.calls())
	}
	record := l.record()
	if !strings.Contains(record, "undone: job j-1 on m2-max was canceled") || !strings.Contains(record, "Result: stopped") {
		t.Errorf("the record:\n%s", record)
	}
	if holder := read(t, filepath.Join(l.stubs, "holder")); !strings.HasPrefix(holder, "hardening-tests-h09-") {
		t.Errorf("lab run was given the holder %q, not the run's own tag", holder)
	}
}

// secrets-seen fails where it would read nothing: a ps that shows no
// process, and a secret file replaced by a link, which it would skip.
func TestSecretsSeenFailsWhereItWouldReadNothing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		fault func(root, stubs string)
		says  string
	}{
		"no process": {func(root, stubs string) {
			os.WriteFile(filepath.Join(stubs, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
		}, "ps showed no process"},
		"a planted link": {func(root, stubs string) {
			os.Symlink("/etc/hosts", filepath.Join(root, "var/lab/secrets/cloudflared.token"))
		}, "is a symbolic link"},
		"": {func(string, string) {}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, stubs := t.TempDir(), t.TempDir()
			for _, d := range []string{"usr/local/lab/secrets", "var/lab/secrets", "Library/LaunchDaemons", "usr/local/lab/current/deploy"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			os.WriteFile(filepath.Join(root, "usr/local/lab/secrets/oobd.token"), []byte("0123456789abcdef0123456789abcdef\n"), 0o600)
			os.WriteFile(filepath.Join(root, "Library/LaunchDaemons/org.omarchy-mac.lab.labd.plist"), []byte("<plist></plist>\n"), 0o644)
			c.fault(root, stubs)
			cmd := exec.Command(filepath.Join(repo(t), "scripts", "mini", "secrets-seen"))
			cmd.Env = []string{"PATH=" + stubs + ":" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "LAB_SYSROOT=" + root, "LAB_PROCESSES=^nothing-of-the-lab/"}
			b, err := cmd.CombinedOutput()
			if c.says == "" {
				if err != nil || !regexp.MustCompile(`(?m)^processes=[1-9]`).Match(b) {
					t.Fatalf("%v\n%s", err, b)
				}
				return
			}
			if err == nil || !strings.Contains(string(b), c.says) {
				t.Errorf("want a failure that says %q: %v\n%s", c.says, err, b)
			}
		})
	}
}

// Through the Lab address a step that needs the owner waits for them: with
// nobody there the test ends as stopped, exit 3, and asked for nothing.
func TestAStepForTheOwnerWithNobodyThereStopsTheTest(t *testing.T) {
	bin := bins(t)
	dir, calls := stubs(t, map[string]string{"lab": `case "$1" in status) echo "NAME STATE" ;; *) exit 9 ;; esac`})
	records := t.TempDir()
	out, code := run(t, []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "LAB_ADDRESS=https://lab.example"}, script(t, "h07-cuts"),
		"--lab", "--bin", bin, "--records", records)
	if code != 3 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	files, _ := filepath.Glob(filepath.Join(records, "*-hardening-07.md"))
	if len(files) != 1 {
		t.Fatalf("records: %v\n%s", files, out)
	}
	record := read(t, files[0])
	for _, want := range []string{"h07-cuts --lab`, through https://lab.example", "STOPPED: nobody was there to type it: the value of the __Host-lab_session cookie", "Result: stopped"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if !strings.HasPrefix(read(t, filepath.Join(records, "hardening.results")), "07 stopped ") {
		t.Errorf("the stop was not written in the results")
	}
	if asked := read(t, calls); strings.TrimSpace(asked) != "lab status" {
		t.Errorf("the calls:\n%s", asked)
	}
}

// Test 14 runs once, in step 14a (decision S12): through the Lab address
// with no Mac named it takes that run's pass and does not repeat it.
func TestH14TakesStep14asRun(t *testing.T) {
	dir, calls := stubs(t, nil)
	env := []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
	records := t.TempDir()
	if out, code := run(t, env, script(t, "h14-volunteers-mac"), "--lab", "--records", records); code != 3 || !strings.Contains(out, "runs in step 14a (decision S12)") {
		t.Errorf("with no run of step 14a: exit %d\n%s", code, out)
	}
	record := "2026-11-20-hardening-14.md"
	if err := os.WriteFile(filepath.Join(records, record), []byte("# Hardening's test 14\n\nResult: pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(records, "hardening.results"), []byte("14 pass 2026-11-20 "+record+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, env, script(t, "h14-volunteers-mac"), "--lab", "--records", records)
	if code != 0 || !strings.Contains(out, "taken from the run of 2026-11-20 in step 14a") || !strings.Contains(out, "Not repeated.") {
		t.Errorf("with step 14a's pass: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("taking step 14a's result called:\n%s", b)
	}
	if out, code := run(t, env, script(t, "h14-volunteers-mac"), "--lab", "--volunteers-mac", "m2-max"); code != 1 || !strings.Contains(out, "go together") {
		t.Errorf("--volunteers-mac without --release: exit %d\n%s", code, out)
	}
}

// run --results is what the Ready list reads: green only when all fourteen
// say pass; a test that waits for a slice is not green.
func TestTheResultsAreGreenOnlyWhenAllFourteenPassed(t *testing.T) {
	records := t.TempDir()
	write := func(lines ...string) {
		if err := os.WriteFile(filepath.Join(records, "hardening.results"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var all []string
	for n := 1; n <= 14; n++ {
		all = append(all, fmt.Sprintf("%02d pass 2026-11-20 2026-11-20-hardening-%02d.md", n, n))
	}
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 1 || strings.Count(out, ": not run") != 14 {
		t.Errorf("no results yet: exit %d\n%s", code, out)
	}
	write(all...)
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 0 || !strings.Contains(out, "Ready item 8 is green: all fourteen passed") || !strings.Contains(out, "H  9: pass    2026-11-20") {
		t.Errorf("all fourteen passed: exit %d\n%s", code, out)
	}
	for what, change := range map[string]string{"waits": "09 waits 2026-11-21 x.md", "stopped": "09 stopped 2026-11-21 x.md", "fail": "09 fail 2026-11-21 x.md", "": ""} {
		lines := append([]string{}, all[:8]...)
		if change != "" {
			lines = append(lines, change)
		}
		write(append(lines, all[9:]...)...)
		out, code := run(t, nil, script(t, "run"), "--results", "--records", records)
		if code != 1 || !strings.Contains(out, "Ready item 8 is not green") || (what != "" && !strings.Contains(out, "H  9: "+what)) || (what == "" && !strings.Contains(out, "H  9: not run")) {
			t.Errorf("test 9 %q: exit %d\n%s", what, code, out)
		}
	}
}

func TestTheScriptsPassShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("no shellcheck")
	}
	files := []string{script(t, "lib.sh"), script(t, "run"), filepath.Join(repo(t), "scripts", "mini", "secrets-seen")}
	for _, name := range tests {
		files = append(files, script(t, name))
	}
	cmd := exec.Command("shellcheck", append([]string{"--shell=bash", "--external-sources"}, files...)...)
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}
