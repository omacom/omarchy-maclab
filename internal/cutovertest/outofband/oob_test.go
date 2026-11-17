// Package outofband runs the Out-of-band wiring's eight tests
// (scripts/cutover/out-of-band/) as far as they go with no hardware: their
// dry runs, and their local runs against a labd on loopback with a made-up
// mini (tools/fake-mini) and simulated Macs. Nothing here reaches the Lab
// address, the mini or a Mac, and no reset leaves this machine.
package outofband

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/testhome"
)

var (
	binOnce sync.Once
	binDir  string
	binErr  error
)

// No test here reads or writes the home of whoever runs the tests.
func TestMain(m *testing.M) {
	exit := testhome.Set()
	code := m.Run()
	if binDir != "" {
		os.RemoveAll(binDir)
	}
	exit(code)
}

var tests = []string{"oob01-tool", "oob02-bad-rid", "oob03-two-at-once", "oob04-sighting", "oob05-hung", "oob06-auto-unlock", "oob07-restart", "oob08-refusals"}

func repo(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func script(t *testing.T, name string) string {
	return filepath.Join(repo(t), "scripts", "cutover", "out-of-band", name)
}

// bins builds labd, lab, the made-up mini and the lab-agent labd hands out
// in a Join command, once.
func bins(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and starts labd")
	}
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "outofband-bin")
		if binErr != nil {
			return
		}
		for _, build := range [][]string{
			{"", "-o", binDir + "/", "./cmd/labd", "./cmd/lab", "./tools/fake-mini"},
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

// local runs one test against the made-up mini and returns what it said,
// its exit status, its record and the results file.
func local(t *testing.T, env []string, name string, args ...string) (out string, code int, record, results string) {
	t.Helper()
	records := t.TempDir()
	out, code = run(t, env, script(t, name), append([]string{"--local", "--bin", bins(t), "--records", records}, args...)...)
	if files, _ := filepath.Glob(filepath.Join(records, "*-out-of-band-"+name[3:5]+".md")); len(files) == 1 {
		record = read(t, files[0])
	}
	if b, err := os.ReadFile(filepath.Join(records, "out-of-band.results")); err == nil {
		results = string(b)
	}
	return out, code, record, results
}

// stubs is a folder of stand-ins for lab, ssh and curl that write down how
// they were called and do what the test's text says.
func stubs(t *testing.T, text map[string]string) (dir, calls string) {
	t.Helper()
	dir = t.TempDir()
	calls = filepath.Join(dir, "calls")
	for _, tool := range []string{"lab", "ssh", "curl"} {
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
// where a hand is needed, and sends nothing.
func TestEveryDryRunSaysItsStepsAndSendsNothing(t *testing.T) {
	dir, calls := stubs(t, nil)
	env := []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
	byHand := map[string]int{"oob01-tool": 2, "oob02-bad-rid": 0, "oob03-two-at-once": 0, "oob04-sighting": 4, "oob05-hung": 2, "oob06-auto-unlock": 0, "oob07-restart": 3, "oob08-refusals": 3}
	for _, name := range tests {
		for _, option := range [][]string{nil, {"--dry-run"}} {
			out, code := run(t, env, script(t, name), option...)
			if code != 0 {
				t.Fatalf("%s %v: exit %d\n%s", name, option, code, out)
			}
			for _, want := range []string{name + ": dry run.", "expects:", "docs/acceptance/", "-out-of-band-" + name[3:5] + ".md", "out-of-band.results", "Nothing was sent"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s's dry run does not say %q:\n%s", name, want, out)
				}
			}
			if n := strings.Count(out, "STOP, by hand:"); n != byHand[name] {
				t.Errorf("%s's dry run names %d steps by hand, want %d:\n%s", name, n, byHand[name], out)
			}
		}
		if out, code := run(t, env, script(t, name), "--lab", "--mini", "owner@mini", "--reset-everything"); code != 1 || !strings.Contains(out, "Nothing was done") {
			t.Errorf("%s took an option it does not know: exit %d\n%s", name, code, out)
		}
		if out, code := run(t, env, script(t, name), "--lab"); code != 1 || !strings.Contains(out, "--lab needs --mini") {
			t.Errorf("%s --lab with no mini: exit %d\n%s", name, code, out)
		}
	}
	out, code := run(t, env, script(t, "run"))
	if code != 0 || strings.Count(out, ": dry run.") != 8 {
		t.Errorf("run with no option: exit %d, %d dry runs\n%s", code, strings.Count(out, ": dry run."), out)
	}
	if out, code := run(t, env, script(t, "run"), "--only", "6,2"); code != 0 || strings.Count(out, ": dry run.") != 2 || strings.Index(out, "oob06") > strings.Index(out, "oob02") {
		t.Errorf("run --only 6,2: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("a dry run, or a run refused for its options, called:\n%s", b)
	}
}

// Each of the eight against a labd on loopback and the made-up mini: the
// lab's own rules do what the test expects of them, hands played.
func TestTheEightRunAgainstAMadeUpMini(t *testing.T) {
	t.Parallel()
	bins(t)
	// Records of real runs are committed there; a local run adds none.
	kept, _ := filepath.Glob(filepath.Join(repo(t), "docs", "acceptance", "*out-of-band*"))
	for name, wants := range map[string][]string{
		"oob01-tool": {"m1-pro is sighted on the right port by the name it shows", "left port, reset 3 of 3: Hard reset sent to m1-pro through the left port",
			"middle port, power-on from shutdown by a Hard reset", "middle port: m2-max kept its boot id through all four"},
		"oob02-bad-rid": {`the controller: RID 3 is refused: no port with RID "3"`, `RID 01 is refused`, "nothing was reset"},
		"oob03-two-at-once": {"m2-max shows no name on the middle port (cable or kernel): placed there by a Proving reset", "each Mac is still on the boot it came back on: each was reset once",
			"the reset tool ran twice, once for each port, the second run ", "two presses for m1-pro at once: one reset was sent, the other press refused"},
		"oob04-sighting": {"position 1: m1-pro is sighted on the left port, m2-max is sighted on the middle port by a Proving reset", "position 4: m1-pro is sighted on the middle port, m2-max is sighted on the left port by a Proving reset",
			"the two cables swapped: each reset followed its Mac, and the other Mac kept its boot id", "across a Hard reset: plug count 44 -> 44, connection id unchanged", "across a cable pulled and put back: plug count 44 -> 45, connection id changed",
			"the hung Mac: m1-pro back by a Hard reset", "it tells an unplug from a restart", "the product id was read on the powered-off Mac and on the hung Mac"},
		"oob05-hung":        {"hung: 3 s later m1-pro is still on boot", "on the hung Mac the mini reads: product id 29448", "brought back from the mini by a Hard reset through the right port"},
		"oob06-auto-unlock": {"reset 1 of 3, a Proving reset through the middle port", "reset 3 of 3, a Hard reset through the middle port: back on a new boot and ready, unattended", "the three returns, in seconds:"},
		"oob07-restart": {"before: m2-max is not sighted: it shows the lab no name", "after labd restarted alone: m1-pro kept its Sighting on the right port, since ", "after an Unlock: m1-pro kept its Sighting on the right port, since ", "after a planned restart: m1-pro kept its Sighting on the right port, since ",
			"after a planned restart: the lab says the controller is healthy"},
		"oob08-refusals": {"a Mac with no Sighting: refused, and m1-pro kept its boot id: lab: no Hard reset was sent: the lab has no Sighting of m1-pro",
			"the port changed between the lab's look and the reset: refused by the controller under its lock", "The Sighting of m1-pro is dropped",
			"a Hard reset of a Volunteer's Mac: refused, and loaner kept its boot id", "a Proving reset aimed at a Volunteer's Mac: refused",
			"a Hard reset with the controller stopped: refused, and m2-max kept its boot id: lab: no Hard reset was sent: the Out-of-band controller is not healthy",
			"/status: Out-of-band controller: not answering since", `"Agents" was told: The Out-of-band controller on the mini is not healthy`, `"Agents" was told: The Out-of-band controller on the mini answers again.`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, code, record, results := local(t, nil, name, "--m2-port", "middle")
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			for _, want := range append(wants, "It proves the script and the lab's rules, not the hardware.", "Result: pass") {
				if !strings.Contains(record, want) {
					t.Errorf("the record lacks %q:\n%s", want, record)
				}
			}
			if !strings.HasPrefix(results, name[3:5]+" pass 20") || strings.Count(results, "\n") != 1 {
				t.Errorf("the results file: %q", results)
			}
			if strings.Contains(out, "enr_") || strings.Contains(out, "lab_") {
				t.Errorf("a token was printed:\n%s", out)
			}
		})
	}
	t.Cleanup(func() {
		all, _ := filepath.Glob(filepath.Join(repo(t), "docs", "acceptance", "*out-of-band*"))
		if left := slices.DeleteFunc(all, func(f string) bool { return slices.Contains(kept, f) }); len(left) != 0 {
			t.Errorf("a local run left a record in docs/acceptance: %v", left)
		}
	})
}

// A reset that moves the plug count and the connection id as a pull does,
// as the real mini counts a restart: the lab tells the two apart by the
// Mac's boot id, and test 4 passes saying so.
func TestOOB04PassesWhenARestartMovesThePlugCount(t *testing.T) {
	t.Parallel()
	out, code, record, results := local(t, []string{"OOB_FAKE_MINI=--reset-counts-as-plug"}, "oob04-sighting")
	if code != 0 || !strings.HasPrefix(results, "04 pass ") {
		t.Fatalf("exit %d, results %q\n%s", code, results, out)
	}
	if m := regexp.MustCompile(`across a Hard reset: plug count (\d+) -> (\d+), connection id changed`).FindStringSubmatch(record); m == nil || m[1] == m[2] {
		t.Errorf("the record does not say the reset moved the plug count:\n%s", record)
	}
	for _, want := range []string{"the plug count moved through a restart too, and with the pull: the lab tells an unplug from the Mac's own restart by its boot id", "Result: pass"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
}

// A reset that does not keep to its port restarts the other cabled Mac:
// test 1 fails at its first reset.
func TestOOB01FailsWhenAResetDisturbsTheOtherMac(t *testing.T) {
	t.Parallel()
	out, code, record, results := local(t, []string{"OOB_FAKE_MINI=--reset-disturbs-all"}, "oob01-tool")
	if code != 1 || !strings.HasPrefix(results, "01 fail ") || !strings.Contains(record, "m2-max restarted while m1-pro was reset on the right port: a second cabled Mac was disturbed") || strings.Contains(record, "reset 2 of 3") {
		t.Fatalf("exit %d, results %q\n%s", code, results, out)
	}
}

// An M2 Max with no auto-unlock stops at its passphrase: test 6 fails.
func TestOOB06FailsForAMacThatWaitsAtItsPassphrase(t *testing.T) {
	t.Parallel()
	out, code, record, results := local(t, []string{"OOB_FAKE_MINI=--encrypted m2-max"}, "oob06-auto-unlock", "--m2-port", "middle")
	if code != 1 || !strings.HasPrefix(results, "06 fail ") || !strings.Contains(record, "m2-max did not return on a new boot within 30s") || !strings.Contains(record, "Result: fail") {
		t.Fatalf("exit %d, results %q\n%s", code, results, out)
	}
}

// A Mac that comes back by itself was not truly hung: test 5 fails before
// any reset is asked.
func TestOOB05FailsWhenTheWatchdogWasLeftOn(t *testing.T) {
	t.Parallel()
	out, code, record, _ := local(t, []string{"OOB_FAKE_MINI=--watchdog"}, "oob05-hung")
	if code != 1 || !strings.Contains(record, "m1-pro came back by itself within 3 s") || strings.Contains(record, "brought back from the mini by") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// Through the Lab address a step by hand waits for a person: with nobody
// there the test ends as stopped, exit 3, having asked for no reset.
func TestAStepByHandWithNobodyThereStopsTheTest(t *testing.T) {
	dir, calls := stubs(t, map[string]string{
		"lab": `case "$1 $2" in
"status ") echo "NAME STATE" ;;
"status m1-pro") printf 'm1-pro  READY\n  boot:        c26ce0d9-0000-4000-8000-000000000001\n' ;;
"oob m1-pro") printf 'm1-pro: sighted on the right port\n  right:       connected, product id 29448, plug count 40, shows the name m1-pro: m1-pro\n' ;;
*) exit 9 ;;
esac`,
		"ssh": `printf 'rid=1 port=left connected=no product=none plugs=27 connection=none\nrid=0 port=middle connected=yes product=29453 plugs=42 connection=A\nrid=2 port=right connected=yes product=29448 plugs=40 connection=B\n'`,
	})
	records := t.TempDir()
	out, code := run(t, []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "LAB_ADDRESS=https://lab.example"}, script(t, "oob05-hung"), "--lab", "--mini", "owner@mini", "--records", records)
	if code != 3 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	files, _ := filepath.Glob(filepath.Join(records, "*-out-of-band-05.md"))
	if len(files) != 1 {
		t.Fatalf("records: %v\n%s", files, out)
	}
	record := read(t, files[0])
	for _, want := range []string{"oob05-hung --lab`, through https://lab.example", "STOPPED: nobody was there for a step by hand: Be at the m1-pro", "Result: stopped"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if !strings.Contains(out, "STOP, by hand: Be at the m1-pro") || !strings.HasPrefix(read(t, filepath.Join(records, "out-of-band.results")), "05 stopped ") {
		t.Errorf("the stop was not said, or not written in the results:\n%s", out)
	}
	if asked, _ := os.ReadFile(calls); strings.Contains(string(asked), "lab reset") {
		t.Errorf("the calls:\n%s", asked)
	}
}

// Tests 5 and 8 may place a Mac that shows no name by a Proving reset,
// which restarts it: with nobody there they stop at their first step by
// hand, before any placing, and no reset is asked.
func TestNobodyThereMeansNoProvingResetToPlaceAMac(t *testing.T) {
	for _, name := range []string{"oob05-hung", "oob08-refusals"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, calls := stubs(t, map[string]string{
				"lab": `case "$1 $2" in
"status ") echo "NAME STATE" ;;
"status m1-pro") printf 'm1-pro  READY\n  boot:        b1\n' ;;
"oob m1-pro") printf 'm1-pro: not sighted: it shows the lab no name\n  right:       connected, product id 29448, plug count 40, shows no name\n' ;;
*) exit 9 ;;
esac`,
			})
			// The waits for a Sighting pass at once: each look at the clock is
			// a hundred seconds on.
			for tool, text := range map[string]string{
				"sleep": "#!/bin/sh\nexit 0\n",
				"date":  "#!/bin/sh\nif [ \"$1\" = +%s ]; then n=$(cat " + dir + "/clock 2>/dev/null || echo 1800000000); n=$((n + 100)); echo $n > " + dir + "/clock; echo $n; else exec /bin/date \"$@\"; fi\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, tool), []byte(text), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			records := t.TempDir()
			out, code := run(t, []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "LAB_ADDRESS=https://lab.example"}, script(t, name), "--lab", "--mini", "owner@mini", "--m1-port", "right", "--records", records)
			if code != 3 {
				t.Errorf("exit %d\n%s", code, out)
			}
			if asked, _ := os.ReadFile(calls); strings.Contains(string(asked), "lab reset") {
				t.Errorf("a reset was asked with nobody there:\n%s\n%s", out, asked)
			}
		})
	}
}

// run --results is what the Ready list reads: green only when all eight
// say pass, each with its record beside it ending "Result: pass".
func TestTheResultsAreGreenOnlyWhenAllEightPassed(t *testing.T) {
	records := t.TempDir()
	write := func(lines ...string) {
		if err := os.WriteFile(filepath.Join(records, "out-of-band.results"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	record := func(n int, result string) {
		text := fmt.Sprintf("# Out-of-band wiring's test %d\n\n- a line\n\nResult: %s\n", n, result)
		if err := os.WriteFile(filepath.Join(records, fmt.Sprintf("2026-11-20-out-of-band-0%d.md", n)), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var all []string
	for n := 1; n <= 8; n++ {
		all = append(all, fmt.Sprintf("0%d pass 2026-11-20 2026-11-20-out-of-band-0%d.md", n, n))
	}
	// Eight pass lines and no record: not green.
	write(all...)
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 1 || !strings.Contains(out, "Ready item 2 is not green") || !strings.Contains(out, "OOB 4: no record") {
		t.Errorf("pass lines with no records: exit %d\n%s", code, out)
	}
	for n := 1; n <= 8; n++ {
		record(n, "pass")
	}
	// A record that does not end in a pass, under a pass line: not green.
	record(4, "fail")
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 1 || !strings.Contains(out, "OOB 4: no record") || strings.Contains(out, "OOB 3: no record") {
		t.Errorf("test 4's record says fail: exit %d\n%s", code, out)
	}
	record(4, "pass")
	// A record named outside the folder: not green.
	write(append(append(append([]string{}, all[:3]...), "04 pass 2026-11-20 ../2026-11-20-out-of-band-04.md"), all[4:]...)...)
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 1 || !strings.Contains(out, "OOB 4: no record") {
		t.Errorf("test 4's record named outside the folder: exit %d\n%s", code, out)
	}
	os.Remove(filepath.Join(records, "out-of-band.results"))
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 1 || strings.Count(out, ": not run") != 8 {
		t.Errorf("no results yet: exit %d\n%s", code, out)
	}
	write(all...)
	if out, code := run(t, nil, script(t, "run"), "--results", "--records", records); code != 0 || !strings.Contains(out, "Ready item 2 is green: all eight passed") || !strings.Contains(out, "OOB 4: pass    2026-11-20  Sighting (2026-11-20-out-of-band-04.md)") {
		t.Errorf("all eight passed: exit %d\n%s", code, out)
	}
	for what, change := range map[string]string{"stopped": "04 stopped 2026-11-21 x.md", "fail": "04 fail 2026-11-21 x.md", "": ""} {
		lines := append([]string{}, all[:3]...)
		if change != "" {
			lines = append(lines, change)
		}
		write(append(lines, all[4:]...)...)
		out, code := run(t, nil, script(t, "run"), "--results", "--records", records)
		if code != 1 || !strings.Contains(out, "Ready item 2 is not green") || (what != "" && !strings.Contains(out, "OOB 4: "+what)) || (what == "" && !strings.Contains(out, "OOB 4: not run")) {
			t.Errorf("test 4 %q: exit %d\n%s", what, code, out)
		}
	}
}

// Test 1 is of all three ports: --ports names each of them once.
func TestTest1TakesTheThreePortsEachOnce(t *testing.T) {
	dir, calls := stubs(t, nil)
	env := []string{"PATH=" + dir + ":" + os.Getenv("PATH")}
	for _, ports := range []string{"", "right", "right left", "right left left", "right left middle middle", "left middle top"} {
		if out, code := run(t, env, script(t, "oob01-tool"), "--ports", ports); code != 1 || !strings.Contains(out, "Nothing was done") {
			t.Errorf("--ports %q: exit %d\n%s", ports, code, out)
		}
	}
	if out, code := run(t, env, script(t, "oob01-tool"), "--ports", "middle right left"); code != 0 || !strings.Contains(out, "(middle right left)") {
		t.Errorf("--ports in another order: exit %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(calls); len(b) != 0 {
		t.Errorf("a refused run ran something:\n%s", b)
	}
}

func TestTheScriptsPassShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("no shellcheck")
	}
	files := []string{script(t, "lib.sh"), script(t, "run")}
	for _, name := range tests {
		files = append(files, script(t, name))
	}
	cmd := exec.Command("shellcheck", append([]string{"--shell=bash", "--external-sources"}, files...)...)
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}
