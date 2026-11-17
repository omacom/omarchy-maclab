// Package cutovertest runs the Front door's transfer and performance tests
// (scripts/cutover/front-door/) against a labd it starts on loopback, and
// the rules that turn their numbers into the lab's settings. Nothing here
// reaches the Lab address, Cloudflare or the mini.
package cutovertest

import (
	"bytes"
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

func repo(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// recordsAdded snapshots the records in docs/acceptance that match pattern
// and returns a check of the ones added since: the records of real runs are
// committed there and keep arriving, and a local or dry run adds none.
func recordsAdded(t *testing.T, pattern string) func() []string {
	t.Helper()
	glob := filepath.Join(repo(t), "docs", "acceptance", pattern)
	kept, _ := filepath.Glob(glob)
	return func() []string {
		all, _ := filepath.Glob(glob)
		return slices.DeleteFunc(all, func(f string) bool { return slices.Contains(kept, f) })
	}
}

func frontDoor(t *testing.T, name string) string {
	return filepath.Join(repo(t), "scripts", "cutover", "front-door", name)
}

// bins builds labd, lab, lab-agent and the stand-in for GitHub once.
func bins(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds and starts labd")
	}
	binOnce.Do(func() {
		binDir, binErr = os.MkdirTemp("", "cutovertest-bin")
		if binErr != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", binDir+"/", "./cmd/labd", "./cmd/lab", "./cmd/lab-agent", "./tools/fake-github")
		cmd.Dir = repo(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binDir
}

// run starts a script with env added to the test's own, and returns what it
// printed and its exit status.
func run(t *testing.T, env []string, stdin string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(stdin)
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

// deployCopy is a copy of the two files of deploy/ the rules write in.
func deployCopy(t *testing.T) (dir, plist, routes string) {
	t.Helper()
	dir = t.TempDir()
	plist, routes = filepath.Join(dir, "org.omarchy-mac.lab.labd.plist"), filepath.Join(dir, "cloudflared.yml")
	for _, f := range []string{plist, routes} {
		if err := os.WriteFile(f, []byte(read(t, filepath.Join(repo(t), "deploy", filepath.Base(f)))), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, plist, routes
}

var (
	partsLine    = regexp.MustCompile(`<string>--parts-at-once</string><string>(\d+)</string>`)
	protocolLine = regexp.MustCompile(`(?m)^protocol: (\w+)$`)
)

func setting(t *testing.T, re *regexp.Regexp, file string) string {
	t.Helper()
	m := re.FindAllStringSubmatch(read(t, file), -1)
	if len(m) != 1 {
		t.Fatalf("%s has the setting %d times", file, len(m))
	}
	return m[0][1]
}

func TestTheScriptsAreSound(t *testing.T) {
	dir := frontDoor(t, "")
	var files []string
	for _, name := range []string{"lib.sh", "settle", "fd04-upload-resume", "fd05-outside-transfers", "fd05-member.sh", "fd15-transport", "fd16-performance", "transfer-file.sh"} {
		file := filepath.Join(dir, name)
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if name != "lib.sh" && info.Mode().Perm()&0o111 == 0 {
			t.Errorf("%s is not executable", name)
		}
		shell := "/bin/bash"
		if name == "fd05-member.sh" {
			shell = "/bin/sh"
		}
		if out, err := exec.Command(shell, "-n", file).CombinedOutput(); err != nil {
			t.Errorf("%s -n %s: %v\n%s", shell, name, err, out)
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

// The member's script runs on a machine that is not the lab's: it uses a
// shell, curl and what every Unix has, and names no other program.
func TestTheMembersScriptNeedsOnlyAShellAndCurl(t *testing.T) {
	text := read(t, frontDoor(t, "fd05-member.sh"))
	if !strings.HasPrefix(text, "#!/bin/sh\n") {
		t.Error("the member's script is not a plain sh script")
	}
	for _, tool := range []string{"python", "perl", "jq", "ssh ", "sudo", "wget", "openssl", "shasum", "sha256sum", "git ", "brew", "apt", "pacman", "source ", "[["} {
		if strings.Contains(text, tool) {
			t.Errorf("the member's script uses %q", tool)
		}
	}
	// It prints nine numbers and no line of what lab said.
	block := text[strings.LastIndex(text, "cat <<EOF"):]
	for _, line := range strings.Split(block, "\n")[1:] {
		if line == "EOF" {
			break
		}
		if !regexp.MustCompile(`^(-+|[a-z0-9_]+=.+)$`).MatchString(line) {
			t.Errorf("the member's script prints %q", line)
		}
	}
	for _, name := range []string{"MACLAB_TOKEN", "MACLAB_URL"} {
		if !strings.Contains(text, "unset MACLAB_URL MACLAB_TOKEN") {
			t.Errorf("a %s in the member's environment would send the run to another lab", name)
		}
	}
}

// The draft of the ask says what the member does first, and nothing in the
// folder sends it.
func TestTheDraftOfTheAskSaysWhatTheMemberDoesFirst(t *testing.T) {
	text := read(t, frontDoor(t, "fd05-ask.md"))
	for _, want := range []string{"sign in with your GitHub account", "make a Lab token", "sh fd05-member.sh JOB", "Nobody sends it for him", "https://lab.omarchy-mac.org/me", "no address, no name and no token", "mark the member as M team on People"} {
		if !strings.Contains(text, want) {
			t.Errorf("the draft lacks %q", want)
		}
	}
	entries, err := os.ReadDir(frontDoor(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		text := read(t, frontDoor(t, e.Name()))
		for _, sender := range []string{"signal-cli", "sendmail", "mail -s", "ntfy", "api.cloudflare.com", "cloudflared tunnel"} {
			if strings.Contains(text, sender) {
				t.Errorf("%s names %s: these tests send nothing and make nothing public", e.Name(), sender)
			}
		}
	}
}

// With no option every test says what it would do and runs nothing: no lab,
// no curl, no ssh.
func TestADryRunSaysTheStepsAndSendsNothing(t *testing.T) {
	added := recordsAdded(t, "*front-door*")
	stubs := t.TempDir()
	log := filepath.Join(stubs, "calls")
	for _, tool := range []string{"lab", "curl", "ssh"} {
		if err := os.WriteFile(filepath.Join(stubs, tool), []byte("#!/bin/sh\necho \""+tool+" $*\" >> \""+log+"\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"PATH=" + stubs + ":" + os.Getenv("PATH")}
	for name, wants := range map[string][]string{
		"fd04-upload-resume":     {"lab upload FILE", "resuming: labd holds N of 32 parts", "1073741824", "front-door-04.md"},
		"fd05-outside-transfers": {"lab run m1-pro --script", "fd05-member.sh", "at least 20% faster", "stop under 50 Mbit/s", "front-door-05.md", "fd05-ask.md"},
		"fd15-transport":         {"transport quic|http2", "transport deployed", "within 10% QUIC stays", "front-door-15.md"},
		"fd16-performance":       {"cf-cache-status HIT", "page-times", "80%", "under one core", "front-door-16.md"},
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

// How many parts go at once: 4 only when 4 at a time were at least 20%
// faster, written as labd's --parts-at-once.
func TestPartsAreFourOnlyWhenAtLeastTwentyPercentFaster(t *testing.T) {
	for _, c := range []struct{ one, four, want string }{
		{"100", "120", "4"}, {"100", "119.9", "1"}, {"100", "250.5", "4"}, {"100", "100", "1"}, {"100", "60", "1"}, {"82.4", "98.9", "4"}, {"82.4", "98.8", "1"},
	} {
		dir, plist, routes := deployCopy(t)
		before := read(t, routes)
		out, code := run(t, nil, "", frontDoor(t, "settle"), "parts", c.one, c.four, "--deploy", dir)
		if code != 0 {
			t.Fatalf("settle parts %s %s: exit %d\n%s", c.one, c.four, code, out)
		}
		if got := setting(t, partsLine, plist); got != c.want {
			t.Errorf("1 at a time %s, 4 at a time %s: --parts-at-once %s, want %s\n%s", c.one, c.four, got, c.want, out)
		}
		if !strings.Contains(out, c.want+" part") || read(t, routes) != before {
			t.Errorf("settle parts said:\n%s", out)
		}
		if out, err := exec.Command("plutil", "-lint", plist).CombinedOutput(); runtime.GOOS == "darwin" && err != nil {
			t.Errorf("the file settle wrote: %v\n%s", err, out)
		}
	}
	// A dry run decides and writes nothing; back to 1 is written too.
	dir, plist, _ := deployCopy(t)
	if out, _ := run(t, nil, "", frontDoor(t, "settle"), "parts", "100", "200", "--deploy", dir, "--dry-run"); setting(t, partsLine, plist) != "1" || !strings.Contains(out, "would write") {
		t.Errorf("a dry run wrote, or did not say what it would:\n%s", out)
	}
	run(t, nil, "", frontDoor(t, "settle"), "parts", "100", "200", "--deploy", dir)
	run(t, nil, "", frontDoor(t, "settle"), "parts", "100", "101", "--deploy", dir)
	if got := setting(t, partsLine, plist); got != "1" {
		t.Errorf("after a second measurement that 4 is no faster, --parts-at-once is %s", got)
	}
}

// The transport: the faster for the same bytes both ways, and within 10%
// QUIC stays.
func TestTheTransportIsTheFasterAndWithinTenPercentQUICStays(t *testing.T) {
	for _, c := range []struct {
		quicUp, quicDown, h2Up, h2Down, want string
	}{
		{"200", "600", "200", "600", "quic"},
		{"200", "600", "220", "660", "quic"},  // exactly 10%
		{"200", "600", "221", "663", "http2"}, // more than 10%
		{"100", "300", "210", "640", "http2"}, // QUIC at half, as people report
		{"200", "600", "100", "300", "quic"},
		{"200", "600", "150", "5000", "quic"}, // a fast download does not hide a slow upload
		{"200", "100", "200", "150", "http2"},
	} {
		dir, plist, routes := deployCopy(t)
		before := read(t, plist)
		out, code := run(t, nil, "", frontDoor(t, "settle"), "transport", c.quicUp, c.quicDown, c.h2Up, c.h2Down, "--deploy", dir)
		if code != 0 {
			t.Fatalf("settle transport %v: exit %d\n%s", c, code, out)
		}
		if got := setting(t, protocolLine, routes); got != c.want {
			t.Errorf("%+v: protocol %s\n%s", c, got, out)
		}
		if read(t, plist) != before {
			t.Error("settle transport wrote in labd's file")
		}
		if (c.want == "quic") != strings.Contains(out, "UDP buffer") {
			t.Errorf("the UDP buffer is the set-up's only under QUIC:\n%s", out)
		}
	}
}

// Under 50 Mbit/s from outside the house the run says so and stops for the
// owner: status 3, and nothing is written.
func TestUnderFiftyMbitTheRunStopsForTheOwner(t *testing.T) {
	for _, c := range []struct {
		one, four, down string
		status          int
		says            string
	}{
		{"80", "120", "49.9", 3, "downloads from outside the house came at 49.9 Mbit/s, under 50"},
		{"30", "45", "200", 3, "uploads from outside the house came at 45 Mbit/s at best, under 50"},
		{"30", "55", "50", 0, "None under 50"},
		{"50", "40", "50.0", 0, "None under 50"},
	} {
		out, code := run(t, nil, "", frontDoor(t, "settle"), "outside", c.one, c.four, c.down)
		if code != c.status || !strings.Contains(out, c.says) {
			t.Errorf("settle outside %s %s %s: exit %d, want %d\n%s", c.one, c.four, c.down, code, c.status, out)
		}
		if c.status == 3 && (!strings.Contains(out, "STOP") || !strings.Contains(out, "for the owner to decide")) {
			t.Errorf("the stop does not say whose it is:\n%s", out)
		}
	}
	if out, _ := run(t, nil, "", frontDoor(t, "settle"), "outside", "80", "120", "20"); !strings.Contains(out, "R2") || !strings.Contains(out, "not built") {
		t.Errorf("a slow download does not name the spec's next step:\n%s", out)
	}
	// What is not a speed is refused before anything is decided.
	dir, plist, _ := deployCopy(t)
	for _, bad := range [][]string{{"parts", "fast", "120"}, {"parts", "100"}, {"parts", "0", "10"}, {"parts", "-5", "10"}, {"parts", "100", "1e3"}, {"transport", "1", "2", "3"}, {"sideways", "1"}, {"parts", "100", "200", "--force"}} {
		if out, code := run(t, nil, "", frontDoor(t, "settle"), append(bad, "--deploy", dir)...); code == 0 || code == 3 || setting(t, partsLine, plist) != "1" {
			t.Errorf("settle %v: exit %d\n%s", bad, code, out)
		}
	}
	// A file that does not hold the setting once is left alone.
	os.WriteFile(plist, bytes.ReplaceAll([]byte(read(t, plist)), []byte("--parts-at-once"), []byte("--parts")), 0o644)
	if out, code := run(t, nil, "", frontDoor(t, "settle"), "parts", "100", "200", "--deploy", dir); code == 0 || !strings.Contains(out, "left alone") {
		t.Errorf("a file without the setting: exit %d\n%s", code, out)
	}
}
