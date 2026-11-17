package cutovertest

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The Front door's stream and edge tests (1, 2, 6 to 13) and the runner of
// all fourteen. A local run starts labd with its tunnel socket and the
// probe's stand-in for the edge in front of it.

var edgeTests = []string{
	"fd01-exec-keepalives", "fd02-freeze-entry", "fd06-ten-clients", "fd07-clients-pass", "fd08-forged-address",
	"fd09-tunnel-restart", "fd10-origin-closed", "fd11-cut-job-wait", "fd12-limiter", "fd13-cache",
}

var (
	probeOnce sync.Once
	probeErr  error
)

// edgeBins is bins with the probe beside the lab's own programs.
func edgeBins(t *testing.T) string {
	t.Helper()
	dir := bins(t)
	probeOnce.Do(func() {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "front-door-probe"), "./tools/front-door-probe")
		cmd.Dir = repo(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			probeErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	return dir
}

// edge runs one of the tests with --local, the stand-in misbehaving as
// fault says, and returns what it printed, its exit status and its record.
func edge(t *testing.T, fault, name string, args ...string) (out string, code int, record string) {
	t.Helper()
	records := t.TempDir()
	out, code = run(t, []string{"FRONT_DOOR_FAULT=" + fault}, "", frontDoor(t, name),
		append([]string{"--local", "--bin", edgeBins(t), "--records", records}, args...)...)
	files, _ := filepath.Glob(filepath.Join(records, "*-front-door-"+name[2:4]+".md"))
	if len(files) == 1 {
		record = read(t, files[0])
	}
	return out, code, record
}

func passes(t *testing.T, name string, wants ...string) string {
	t.Helper()
	out, code, record := edge(t, "", name)
	if code != 0 || !strings.Contains(record, "\nResult: pass\n") {
		t.Fatalf("%s --local: exit %d\n%s\nrecord:\n%s", name, code, out, record)
	}
	for _, want := range wants {
		if !strings.Contains(record, want) {
			t.Errorf("%s's record lacks %q:\n%s", name, want, record)
		}
	}
	noAddress(t, name, record)
	return record
}

func fails(t *testing.T, fault, name string, want string, args ...string) string {
	t.Helper()
	out, code, record := edge(t, fault, name, args...)
	if code != 1 || !strings.Contains(record, "\nResult: fail\n") || !strings.Contains(record, want) {
		t.Fatalf("%s --local with the fault %q: exit %d, wanted a failure saying %q\n%s\nrecord:\n%s", name, fault, code, want, out, record)
	}
	if page.MatchString(out + record) {
		t.Errorf("%s showed a line of a web page:\n%s", name, out)
	}
	return record
}

var (
	page = regexp.MustCompile(`(?i)<!doctype|<html|<body|<title`)
	// The two addresses kept for documents that the tests name on purpose.
	anAddress = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
)

// noAddress fails when a record names an address that is not one of the
// made-up ones, or anything that looks like a token.
func noAddress(t *testing.T, name, record string) {
	t.Helper()
	for _, a := range anAddress.FindAllString(record, -1) {
		if a != "203.0.113.77" && a != "198.51.100.7" && a != "127.0.0.1" && a != "0.0.0.0" {
			t.Errorf("%s's record names the address %s", name, a)
		}
	}
	if regexp.MustCompile(`lab_[0-9a-f]{8}|enr_[0-9a-f]{8}`).MatchString(record) {
		t.Errorf("%s's record holds a token:\n%s", name, record)
	}
}

func TestTheStreamAndEdgeScriptsAreSound(t *testing.T) {
	dir := frontDoor(t, "")
	var files []string
	for _, name := range append([]string{"edge.sh", "run", "wait-a-minute.sh"}, edgeTests...) {
		file := filepath.Join(dir, name)
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if name != "edge.sh" && info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", name)
		}
		if out, err := exec.Command("/bin/bash", "-n", file).CombinedOutput(); err != nil {
			t.Errorf("/bin/bash -n %s: %v\n%s", name, err, out)
		}
		files = append(files, file)
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

// stubs puts made-up lab, curl, ssh, go and python3 first on PATH: each
// writes down that it was run.
func stubs(t *testing.T) (env []string, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	for _, tool := range []string{"lab", "curl", "ssh", "go", "python3", "front-door-probe"} {
		if err := os.WriteFile(filepath.Join(dir, tool), []byte("#!/bin/sh\necho \""+tool+" $*\" >> \""+log+"\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"PATH=" + dir + ":" + os.Getenv("PATH")}, log
}

// With no option every test says what it would do and runs nothing: no lab,
// no curl, no ssh, no probe.
func TestADryRunOfAStreamOrEdgeTestSendsNothing(t *testing.T) {
	added := recordsAdded(t, "*front-door*")
	env, log := stubs(t)
	for name, wants := range map[string][]string{
		"fd01-exec-keepalives": {"lab exec --timeout 3600 m1-pro 'sleep 3590", "one every 30 s (120 in all)", "front-door-01.md", "Exec record"},
		"fd02-freeze-entry":    {"freeze-entry m1-pro --release R", "frozen once", "front-door-02.md"},
		"fd06-ten-clients":     {"six times over, lab status m1-pro", "the event stream", "lab logs JOB", "front-door-06.md"},
		"fd07-clients-pass":    {"lab status", "Python CLI", "curl -fsSL https://lab.omarchy-mac.org/install", "curl -fsSL https://lab.omarchy-mac.org/join, with no token", "never runs", "front-door-07.md"},
		"fd08-forged-address":  {"CF-Connecting-IP: 203.0.113.77", "edge-facts callers", "No address goes into the record", "front-door-08.md"},
		"fd09-tunnel-restart":  {"launchctl kickstart -k system/org.omarchy-mac.lab.cloudflared", "connection lost; the command may still be running", "Exec record to hold lab's command once", "front-door-09.md"},
		"fd10-origin-closed":   {"edge-facts origin", "7770 7780 20241 80 443 8080", "UPnP", "--no-forwards", "front-door-10.md"},
		"fd11-cut-job-wait":    {"lab run m1-pro --script wait-a-minute.sh --no-wait", "kill it after 3 s", "it was not canceled", "front-door-11.md"},
		"fd12-limiter":         {"eleven times", "401 ten times, then 429", "15 minutes", "front-door-12.md"},
		"fd13-cache":           {"cf-cache-status is HIT", "private, no-store", "UPDATING", "/api/auth", "front-door-13.md"},
	} {
		out, code := run(t, env, "", frontDoor(t, name))
		if code != 0 || !strings.Contains(out, "Nothing was sent.") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s does not say %q:\n%s", name, want, out)
			}
		}
		if again, _ := run(t, env, "", frontDoor(t, name), "--dry-run"); again != out {
			t.Errorf("%s --dry-run says something else than no option", name)
		}
		if out, code := run(t, env, "", frontDoor(t, name), "--everything"); code == 0 {
			t.Errorf("%s took an option it does not know:\n%s", name, out)
		}
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("a dry run ran:\n%s", b)
	}
	if entries := added(); len(entries) != 0 {
		t.Errorf("a dry run left a record: %v", entries)
	}
}

// What a run through the Lab address cannot do without is asked for before
// anything is sent.
func TestARunThroughTheLabAddressSaysWhatItLacks(t *testing.T) {
	env, log := stubs(t)
	for name, want := range map[string]string{
		"fd02-freeze-entry":   "--lab needs --release R",
		"fd08-forged-address": "--lab needs --mini USER@HOST",
		"fd09-tunnel-restart": "--lab needs --mini USER@HOST",
		"fd10-origin-closed":  "--lab needs --mini USER@HOST",
	} {
		out, code := run(t, append(env, "LAB_ADDRESS=http://127.0.0.1:9"), "", frontDoor(t, name), "--lab", "--bin", t.TempDir())
		if code != 1 || !strings.Contains(out, want) {
			t.Errorf("%s --lab: exit %d, wanted %q\n%s", name, code, want, out)
		}
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("it ran something first:\n%s", b)
	}
}

// The runner runs all fourteen in the order of their numbers and passes its
// options on.
func TestTheRunnerRunsTheFourteenInOrder(t *testing.T) {
	env, log := stubs(t)
	out, code := run(t, env, "", frontDoor(t, "run"))
	if code != 0 || !strings.Contains(out, "14 test(s)") || !strings.Contains(out, "Nothing was sent.") {
		t.Fatalf("run: exit %d\n%s", code, out)
	}
	last := -1
	for _, n := range []string{"1", "2", "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "15", "16"} {
		at := strings.Index(out, "run: test "+n+": fd")
		if at <= last {
			t.Errorf("test %s is missing or out of order:\n%s", n, out)
		}
		last = at
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("a dry run of the runner ran:\n%s", b)
	}

	out, code = run(t, env, "", frontDoor(t, "run"), "--only", "2,13", "--mac", "m2-max", "--for", "2", "--release 9.9")
	if code != 0 || strings.Count(out, "run: test ") != 2 || !strings.Contains(out, "freeze-entry m2-max --release 9.9") || !strings.Contains(out, "fd13-cache --dry-run --mac m2-max") {
		t.Errorf("run --only 2,13: exit %d\n%s", code, out)
	}
	if out, _ := run(t, env, "", frontDoor(t, "run"), "--skip", "1,16"); strings.Count(out, "run: test ") != 12 || strings.Contains(out, "fd01-") {
		t.Errorf("run --skip 1,16:\n%s", out)
	}
	for _, bad := range [][]string{{"--only", "3"}, {"--only", "17"}, {"--everything"}, {"--for", "2"}} {
		if out, code := run(t, env, "", frontDoor(t, "run"), bad...); code != 2 || strings.Contains(out, "run: test ") {
			t.Errorf("run %v: exit %d\n%s", bad, code, out)
		}
	}
}

// The runner's list: pass, fail, stopped and no script, the worst of them
// as its exit status, and never a line of a web page.
func TestTheRunnerListsEachResultAndShowsNoPage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run"), []byte(read(t, frontDoor(t, "run"))), 0o755); err != nil {
		t.Fatal(err)
	}
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/bash\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	records := t.TempDir()
	script("fd01-good", `echo "fd01-good: pass. The record is `+records+`/x-front-door-01.md"; echo "got: $*"`)
	script("fd02-page", `echo '<!DOCTYPE html><html><body>error code: 1015 from 192.0.2.9</body></html>'; echo "fd02-page: an error page (status 1015, ray id 0123456789abcdef)"; exit 1`)
	script("fd04-owner", `echo "fd04-owner: stopped"; exit 3`)
	script("fd05-member.sh", `echo "the member's script was run"; exit 1`)
	out, code := run(t, nil, "", filepath.Join(dir, "run"), "--local", "--only", "1,2,4,5", "--records", records, "--for", "1", "--seconds 70")
	if code != 1 {
		t.Errorf("exit %d, want 1 for a failed test\n%s", code, out)
	}
	for _, want := range []string{"run: test 1: pass", "run: test 2: fail", "run: test 4: stopped", "run: test 5: no script", "1 of 4 passed",
		"got: --local --records " + records + " --seconds 70", "status 1015, ray id 0123456789abcdef"} {
		if !strings.Contains(out, want) {
			t.Errorf("the run does not say %q:\n%s", want, out)
		}
	}
	if page.MatchString(out) || strings.Contains(out, "192.0.2.9") || strings.Contains(out, "the member's script was run") {
		t.Errorf("the run showed a page, or ran the member's script:\n%s", out)
	}
	lists, _ := filepath.Glob(filepath.Join(records, "*-front-door.md"))
	if len(lists) != 1 {
		t.Fatalf("the list: %v", lists)
	}
	list := read(t, lists[0])
	for _, want := range []string{"- Test 1: pass (`x-front-door-01.md`)", "- Test 2: fail", "- Test 4: stopped", "- Test 5: no script", "Result: 1 of fourteen passed in this run", "It proves the scripts, not the Front door."} {
		if !strings.Contains(list, want) {
			t.Errorf("the list lacks %q:\n%s", want, list)
		}
	}

	// Stopped alone is status 3; everything passing is 0.
	if _, code := run(t, nil, "", filepath.Join(dir, "run"), "--local", "--only", "1,4"); code != 3 {
		t.Errorf("a run with one test stopped: exit %d, want 3", code)
	}
	if _, code := run(t, nil, "", filepath.Join(dir, "run"), "--local", "--only", "1"); code != 0 {
		t.Errorf("a run that passed: exit %d", code)
	}
}

// Each test against a labd on loopback, behind the stand-in for the edge.

func TestFD01HoldsASilentExecOnKeepalives(t *testing.T) {
	t.Parallel()
	record := passes(t, "fd01-exec-keepalives", "the command was silent for 70 s", "Keepalives: 3;", "the last line of the stream was the command's result", "lab exec, held for the same 70 s, came back")
	if !regexp.MustCompile(`from (29|3[01])\.\d\d s to (29|3[0-4])\.\d\d s apart`).MatchString(record) {
		t.Errorf("the Keepalives were not 30 s apart:\n%s", record)
	}
}

// With the Keepalives taken out of the stream, the stand-in cuts it as the
// edge cuts a silent answer, and the test fails.
func TestFD01FailsWithoutKeepalives(t *testing.T) {
	t.Parallel()
	fails(t, "no-keepalives", "fd01-exec-keepalives", "the stream was cut after")
}

func TestFD02FreezeEntryAnswersAsAStream(t *testing.T) {
	t.Parallel()
	passes(t, "fd02-freeze-entry", "answered as a stream", "labd's error and its status (502)")
	fails(t, "no-keepalives", "fd02-freeze-entry", "no Keepalive came within 5 s")
}

func TestFD06TenClientsAtOnce(t *testing.T) {
	t.Parallel()
	passes(t, "fd06-ten-clients", "0 went unanswered", "lab exec 1, silent for 40 s", "lab exec 2, silent for 40 s", "the event stream was held", "each whole (8388608 bytes)", "went on checking in")
}

func TestFD06FailsWhenTheEdgeThrottles(t *testing.T) {
	t.Parallel()
	fails(t, "throttle", "fd06-ten-clients", "downloads came short of 8388608 bytes")
}

func TestFD07TheLabsClientsPass(t *testing.T) {
	t.Parallel()
	passes(t, "fd07-clients-pass", "lab (Go): lab status was answered by labd", "omarchy-m-test (Python): /install came as the script", "lab-agent: m1-pro is checking in",
		"curl -fsSL …/install: the install script", "curl -fsSL …/join: labd's own answer", "a body of shell, SQL and script text reached m1-pro")
	fails(t, "challenge", "fd07-clients-pass", "FAIL: omarchy-m-test (Python): /install gave HTTP 403")
}

// The text test 7 sends as a command runs nothing: a shell given it prints
// one word and touches no file.
func TestFD07sShellTextRunsNothing(t *testing.T) {
	text := read(t, frontDoor(t, "fd07-clients-pass"))
	from, to := strings.Index(text, ": <<'FD07'\n"), strings.Index(text, "\nFD07\nTEXT\n")
	if from < 0 || to < from {
		t.Fatal("the shell text is not where it was")
	}
	body := text[from:to] + "\nFD07\necho only-this\n"
	for _, shell := range []string{"/bin/sh", "/bin/bash"} {
		dir := t.TempDir()
		cmd := exec.Command(shell, "-c", body)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + dir, "HOME=" + dir}
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != "only-this\n" {
			t.Errorf("%s ran the text: %v %q", shell, err, out)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Errorf("%s left files behind", shell)
		}
	}
}

func TestFD08AForgedAddressIsNotLogged(t *testing.T) {
	t.Parallel()
	passes(t, "fd08-forged-address", "in labd's caller log once, from this machine's own address", "carries the edge's ray id",
		"named 203.0.113.77 in three headers (HTTP 404) is logged from this machine's own address", "at labd's loopback port the same forged request is logged from 127.0.0.1")
	fails(t, "pass-client-address", "fd08-forged-address", "FAIL: labd logged a request as coming from 203.0.113.77")
}

func TestFD09TheTunnelRestartedMidStream(t *testing.T) {
	t.Parallel()
	passes(t, "fd09-tunnel-restart", "in labd's caller log with the edge's ray id", "saying: lab: connection lost; the command may still be running",
		"the probe's stream was cut after", "the Exec record holds lab's command once", "was checking in again", "a new lab exec on it worked")
}

func TestFD10NothingAnswersBeyondLoopback(t *testing.T) {
	t.Parallel()
	passes(t, "fd10-origin-closed", "labd refuses to listen beyond loopback", "local: no router is asked")
	// A port that does answer on every address is found.
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	out, code, record := edge(t, "", "fd10-origin-closed", "--port", port)
	if strings.Contains(record, "no address beyond loopback to ask") {
		t.Skip("this machine has no address beyond loopback")
	}
	if code != 1 || !strings.Contains(record, "FAIL: something answers beyond loopback on port(s) "+port) {
		t.Errorf("with a port open on every address: exit %d\n%s", code, out)
	}
	noAddress(t, "fd10-origin-closed", record)
}

// An address the mini shares with the machine asking it, such as the
// bridge Apple's container gives both Macs, reaches the asker itself: it is
// not asked, and the record says why without naming it.
func TestFD10DoesNotAskAnAddressThatIsAlsoThisMachines(t *testing.T) {
	t.Parallel()
	stubs, bin, records := t.TempDir(), t.TempDir(), t.TempDir()
	calls := filepath.Join(stubs, "calls")
	write := func(file, body string) {
		if err := os.WriteFile(file, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(stubs, "ssh"), `#!/bin/bash
case "$*" in
  *edge-facts\ origin*) printf 'lab_listeners=3\nbeyond_loopback=0\nbeyond_loopback_ports=\n' ;;
  */sbin/ifconfig*) printf 'lo0: flags=8049\n\tinet 127.0.0.1 netmask 0xff000000\nen0: flags=8863\n\tinet 198.51.100.7 netmask 0xffffff00\nbridge100: flags=8863\n\tinet 192.0.2.1 netmask 0xffffff00\nutun4: flags=8051\n\tinet 203.0.113.77 --> 203.0.113.77\n' ;;
  *) exit 1 ;;
esac
`)
	write(filepath.Join(stubs, "ifconfig"), "#!/bin/sh\nprintf 'lo0: flags=8049\\n\\tinet 127.0.0.1 netmask 0xff000000\\nen0: flags=8863\\n\\tinet 192.0.2.50 netmask 0xffffff00\\nbridge100: flags=8863\\n\\tinet 192.0.2.1 netmask 0xffffff00\\n'\n")
	// The stand-in for the probe: SSH answers everywhere, and 7780 answers
	// at the shared address, as this machine's own listener did.
	write(filepath.Join(bin, "front-door-probe"), `#!/bin/bash
echo "probe $*" >> "`+calls+`"
case "$1" in
  answers)
    shift
    asked=$# open=0 ports=""
    for a in "$@"; do
      if [[ $a == *:22 || $a == 192.0.2.1:7780 ]]; then open=$((open + 1)); ports="${a##*:}"; fi
    done
    printf 'asked=%s\nopen=%s\nclosed=%s\nopen_ports=%s\n' "$asked" "$open" "$((asked - open))" "$ports" ;;
  router) printf 'upnp=silent\nnat_pmp=silent\n' ;;
esac
`)
	out, code := run(t, []string{"PATH=" + stubs + ":" + os.Getenv("PATH")}, "", frontDoor(t, "fd10-origin-closed"),
		"--lab", "--mini", "owner@mini.example", "--bin", bin, "--records", records, "--no-forwards", "--upnp-off")
	files, _ := filepath.Glob(filepath.Join(records, "*-front-door-10.md"))
	if len(files) != 1 {
		t.Fatalf("no record: exit %d\n%s", code, out)
	}
	record := read(t, files[0])
	if code != 0 || !strings.Contains(record, "\nResult: pass\n") {
		t.Errorf("fd10 --lab: exit %d\n%s\nrecord:\n%s", code, out, record)
	}
	for _, want := range []string{
		"the mini has 3 address(es) beyond loopback; 1 of them is also this machine's own address: not asked",
		"2 of the rest can be reached from this machine (SSH answers there)",
		"asked on 2 address(es) and ports 7770 7780 20241 80 443 8080: none takes a connection",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if probed := read(t, calls); strings.Contains(probed, "192.0.2.1:") {
		t.Errorf("the shared address was asked:\n%s", probed)
	}
	if !strings.Contains(out, "192.0.2.1 is also this machine's own address: not asked") {
		t.Errorf("the run does not name the address it left out:\n%s", out)
	}
	if strings.Contains(record, "192.0.2.1") {
		t.Errorf("the record names an address:\n%s", record)
	}
	noAddress(t, "fd10-origin-closed", record)
}

func TestFD11ACutJobWaitLeavesItsJob(t *testing.T) {
	t.Parallel()
	passes(t, "fd11-cut-job-wait", "lab wait was killed 3 s into its call", "the job is still going", "it passed: the cut did not cancel it")
}

func TestFD12TheEleventhBadCredential(t *testing.T) {
	t.Parallel()
	passes(t, "fd12-limiter", "eleven wrong credentials got: 401 401 401 401 401 401 401 401 401 401 429", "in labd's own words", "the saved login passed from the same address right after (HTTP 200)", "a twelfth wrong one still got 429")
	fails(t, "roaming-address", "fd12-limiter", "FAIL: the eleventh got 401, not 429")
}

func TestFD13TheCachePolicyOfEachKindOfAnswer(t *testing.T) {
	t.Parallel()
	passes(t, "fd13-cache", `each of the 12 signed-out public pages says "public, max-age=0" to the browser and "max-age=60, stale-while-revalidate=86400, stale-if-error=86400" to the edge`,
		"the exports are kept the same way", `says "public, max-age=31536000, immutable"`, `/install says "public, max-age=300"`,
		`a page asked with the session cookie says "private, no-store"`, "/api/devices under the saved login")
	fails(t, "public-cache", "fd13-cache", `FAIL: / asked with the session cookie says Cache-Control: public, max-age=60, not "private, no-store"`)
}
