package cutovertest

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

const mib = 1 << 20

// local runs one of the tests with --local against the binaries built here.
func local(t *testing.T, name string, args ...string) (string, int) {
	t.Helper()
	return run(t, nil, "", frontDoor(t, name), append([]string{"--local", "--bin", bins(t)}, args...)...)
}

// The Front door's test 4 against a labd on loopback: lab upload is killed
// while it sends, the same command resumes, and the checksum matches.
func TestFD04CutsAnUploadAndResumesIt(t *testing.T) {
	records := t.TempDir()
	out, code := local(t, "fd04-upload-resume", "--size", fmt.Sprint(200*mib), "--records", records)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	cut := regexp.MustCompile(`cut: lab upload killed while it sent, with (\d) of 7 parts taken by labd and [1-9]\d* bytes of another received`).FindStringSubmatch(out)
	m := regexp.MustCompile(`resume: labd held (\d) of 7 parts; the same command sent the other (\d) in`).FindStringSubmatch(out)
	if cut == nil || m == nil || cut[1] < "2" || m[1] < cut[1] || m[1] == "7" || m[1][0]-'0'+m[2][0]-'0' != 7 {
		t.Errorf("no cut with two parts held, or no resume from at least those and not all:\n%s", out)
	}
	for _, want := range []string{"file: 209715200 bytes, 7 parts of 32 MiB", "cut: lab upload killed while it sent", "checksum: the SHA-256 labd stored is the file's", ": pass"} {
		if !strings.Contains(out, want) {
			t.Errorf("the run does not say %q:\n%s", want, out)
		}
	}
	files, _ := filepath.Glob(filepath.Join(records, "*-front-door-04.md"))
	if len(files) != 1 {
		t.Fatalf("records: %v", files)
	}
	record := read(t, files[0])
	for _, want := range []string{"# Front door test 4:", "--local", "It proves the script, not the Front door.", "- resume: labd held", "Result: pass"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if out, code := local(t, "fd04-upload-resume", "--size", fmt.Sprint(100*mib)); code == 0 || !strings.Contains(out, "longer than four parts") {
		t.Errorf("a file a cut cannot split was taken: exit %d\n%s", code, out)
	}
}

// With resuming turned off the test fails: the run after the cut does not
// say it resumed.
func TestFD04FailsWhenNoRunResumes(t *testing.T) {
	real := bins(t)
	dir := t.TempDir()
	for _, tool := range []string{"labd", "lab-agent"} {
		if err := os.Symlink(filepath.Join(real, tool), filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := "#!/bin/bash\nif [[ $1 == upload ]]; then exec \"" + real + "/lab\" \"$@\" 2> >(grep --line-buffered -v resuming >&2); fi\nexec \"" + real + "/lab\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "lab"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, nil, "", frontDoor(t, "fd04-upload-resume"), "--local", "--bin", dir, "--size", fmt.Sprint(200*mib))
	if code != 1 || !strings.Contains(out, ": fail") || !strings.Contains(out, "did not say it resumed") || strings.Contains(out, "checksum") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A cut counts only inside a part: a lab that says labd takes its parts and
// sends none, so that labd never holds part of one, does not pass, though
// the run after it says what a resume says and prints the right SHA-256.
func TestFD04BetweenTwoPartsDoesNotPass(t *testing.T) {
	real := bins(t)
	dir := t.TempDir()
	for _, tool := range []string{"labd", "lab-agent"} {
		if err := os.Symlink(filepath.Join(real, tool), filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := `#!/bin/bash
[[ $1 == upload ]] || exec "` + real + `/lab" "$@"
if [[ ${MACLAB_UPLOAD_PROGRESS:-} == 1 ]]; then
  for n in 1 2 3 4 5 6; do echo "lab: labd holds $n of 7 parts" >&2; sleep 0.3; done
  exit 1
fi
echo "lab: resuming: labd holds 2 of 7 parts" >&2
shasum -a 256 "$2" | cut -d' ' -f1
`
	if err := os.WriteFile(filepath.Join(dir, "lab"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, nil, "", frontDoor(t, "fd04-upload-resume"), "--local", "--bin", dir, "--size", fmt.Sprint(200*mib))
	if code != 1 || !strings.Contains(out, ": fail") || !strings.Contains(out, "while labd held part of a part") || strings.Contains(out, "checksum") {
		t.Errorf("exit %d\n%s", code, out)
	}
	// Through the Lab address labd's upload folder is on the mini.
	if out, code := run(t, nil, "", frontDoor(t, "fd04-upload-resume"), "--lab"); code != 1 || !strings.Contains(out, "--lab needs --mini USER@HOST") {
		t.Errorf("--lab with no --mini: exit %d\n%s", code, out)
	}
}

// A part seen partly received when lab is stopped and finished after the
// kill, from bytes lab had sent before it, was not cut: FD 4 fails though
// the resume says it resumed and prints the right SHA-256.
func TestFD04APartFinishedAfterTheKillDoesNotPass(t *testing.T) {
	real := bins(t)
	dir := t.TempDir()
	for _, tool := range []string{"labd", "lab-agent"} {
		if err := os.Symlink(filepath.Join(real, tool), filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := `#!/bin/bash
[[ $1 == upload ]] || exec "` + real + `/lab" "$@"
sha="$(shasum -a 256 "$2" | cut -d' ' -f1)"
folder="$(dirname "$HOME")/root/var/lab/data/artifacts/.upload-$sha"
if [[ ${MACLAB_UPLOAD_PROGRESS:-} == 1 ]]; then
  mkdir -p "$folder"
  echo one > "$folder/1"
  echo two > "$folder/2"
  head -c 1000 /dev/zero > "$folder/.part-h3"
  me=$$
  # What lab had sent of part 3 reaches labd after the kill, which makes
  # the part whole.
  ( while kill -0 "$me" 2> /dev/null; do sleep 0.05; done; mv "$folder/.part-h3" "$folder/3" ) > /dev/null 2>&1 &
  echo "lab: labd holds 2 of 7 parts" >&2
  sleep 30
  exit 1
fi
echo "lab: resuming: labd holds 3 of 7 parts" >&2
echo "$sha"
`
	if err := os.WriteFile(filepath.Join(dir, "lab"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, nil, "", frontDoor(t, "fd04-upload-resume"), "--local", "--bin", dir, "--size", fmt.Sprint(200*mib))
	if code != 1 || !strings.Contains(out, ": fail") || !strings.Contains(out, "no part was cut mid-way") || strings.Contains(out, "checksum") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A part labd holds whole already and receives again, a retry, is no part
// cut mid-way when the retry is cut: its file of its own goes, the counts
// match, and nothing is resent. FD 4 knows a file labd receives by its part,
// from its first bytes, and fails.
func TestFD04ARetryOfAPartHeldWholeDoesNotPass(t *testing.T) {
	real := bins(t)
	dir := t.TempDir()
	for _, tool := range []string{"labd", "lab-agent"} {
		if err := os.Symlink(filepath.Join(real, tool), filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	wrapper := `#!/bin/bash
[[ $1 == upload ]] || exec "` + real + `/lab" "$@"
sha="$(shasum -a 256 "$2" | cut -d' ' -f1)"
folder="$(dirname "$HOME")/root/var/lab/data/artifacts/.upload-$sha"
if [[ ${MACLAB_UPLOAD_PROGRESS:-} == 1 ]]; then
  mkdir -p "$folder"
  dd if="$2" of="$folder/1" bs=1048576 count=32 2> /dev/null
  dd if="$2" of="$folder/2" bs=1048576 skip=32 count=32 2> /dev/null
  # Part 2 sent again and cut after 1000 bytes: labd drops its file once
  # lab is gone.
  dd if="$2" bs=1048576 skip=32 count=1 2> /dev/null | head -c 1000 > "$folder/.part-r2"
  me=$$
  ( while kill -0 "$me" 2> /dev/null; do sleep 0.05; done; rm -f "$folder/.part-r2" ) > /dev/null 2>&1 &
  echo "lab: labd holds 2 of 7 parts" >&2
  sleep 30
  exit 1
fi
echo "lab: resuming: labd holds 2 of 7 parts" >&2
echo "$sha"
`
	if err := os.WriteFile(filepath.Join(dir, "lab"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := run(t, nil, "", frontDoor(t, "fd04-upload-resume"), "--local", "--bin", dir, "--size", fmt.Sprint(200*mib))
	if code != 1 || !strings.Contains(out, ": fail") || !strings.Contains(out, "part 2, which it held whole already") || strings.Contains(out, "checksum") {
		t.Errorf("exit %d\n%s", code, out)
	}
}

// A stop while lab is stopped for the cut (here, while labd's folder is
// read) ends the run: the stopped lab is killed, not waited for.
func TestFD04StoppedWhileLabIsStoppedEnds(t *testing.T) {
	bin := bins(t)
	stubs := t.TempDir()
	mark := filepath.Join(stubs, "looking")
	if err := os.WriteFile(filepath.Join(stubs, "find"), []byte("#!/bin/bash\ntouch \""+mark+"\"\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(frontDoor(t, "fd04-upload-resume"), "--local", "--bin", bin, "--size", fmt.Sprint(200*mib))
	cmd.Env = append(os.Environ(), "PATH="+stubs+":"+os.Getenv("PATH"))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	for end := time.Now().Add(60 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(mark); err == nil {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the run never stopped lab to look at labd's folder")
		}
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the run did not end: its cleanup waits for the stopped lab")
	}
}

// The Front door's tests 5, 15 and 16 against a labd on loopback: the
// member's script, the measuring and the decision, which is shown and
// written nowhere.
func TestTheTransferTestsRunAgainstALocalLabd(t *testing.T) {
	added := recordsAdded(t, "*front-door*")
	plist := read(t, filepath.Join(repo(t), "deploy", "org.omarchy-mac.lab.labd.plist"))
	routes := read(t, filepath.Join(repo(t), "deploy", "cloudflared.yml"))
	for name, wants := range map[string][]string{
		"fd05-outside-transfers": {"upload, 1 part at a time: 75497472 bytes in ", "upload, 4 parts at a time: 75497472 bytes in ", "download: 75497472 bytes in ", "None under 50", " at a time", ": pass"},
		"fd15-transport":         {"quic, up: 50331648 bytes in ", "quic, down: 50331648 bytes in ", "http2, up: ", "http2, down: ", "of a core", "Mbit/s over both directions", ": pass"},
		"fd16-performance":       {"target 1: pass. first byte in ", "target 2: pass. labd took ", " requests for 1", "target 3: pass.", "target 4: pass.", ": pass"},
	} {
		out, code := local(t, name)
		if code != 0 {
			t.Errorf("%s --local: exit %d\n%s", name, code, out)
		}
		for _, want := range wants {
			if !strings.Contains(out, want) {
				t.Errorf("%s --local does not say %q:\n%s", name, want, out)
			}
		}
	}
	if read(t, filepath.Join(repo(t), "deploy", "org.omarchy-mac.lab.labd.plist")) != plist || read(t, filepath.Join(repo(t), "deploy", "cloudflared.yml")) != routes {
		t.Error("a local run wrote in deploy/")
	}
	if entries := added(); len(entries) != 0 {
		t.Errorf("a local run left a record in docs/acceptance: %v", entries)
	}
}

const sent = `----------
fd05_format=1
upload_bytes=1073741824
upload_1_seconds=95.0
upload_1_mbit=90.4
upload_4_seconds=%s
upload_4_mbit=%s
download_bytes=1090519040
download_seconds=%s
download_mbit=%s
----------
`

// What the member sent back is read as nine numbers and nothing else, and
// decides how many parts go at once; under 50 Mbit/s the run stops.
func TestTheMembersNumbersDecideAndAnythingElseIsRefused(t *testing.T) {
	record := func(text string) (out string, code int, parts, recorded string) {
		t.Helper()
		dir, plist, _ := deployCopy(t)
		records := t.TempDir()
		file := filepath.Join(t.TempDir(), "sent")
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		stubs := t.TempDir()
		for _, tool := range []string{"lab", "curl", "ssh"} {
			os.WriteFile(filepath.Join(stubs, tool), []byte("#!/bin/sh\necho \"$0 was run\" >&2\nexit 9\n"), 0o755)
		}
		out, code = run(t, []string{"PATH=" + stubs + ":" + os.Getenv("PATH")}, "", frontDoor(t, "fd05-outside-transfers"), "--lab", "--record", file, "--deploy", dir, "--records", records)
		if files, _ := filepath.Glob(filepath.Join(records, "*-front-door-05.md")); len(files) == 1 {
			recorded = read(t, files[0])
		}
		return out, code, setting(t, partsLine, plist), recorded
	}

	out, code, parts, recorded := record(fmt.Sprintf(sent, "70.0", "122.7", "60.1", "145.2"))
	if code != 0 || parts != "4" {
		t.Fatalf("4 parts at a time 36%% faster: exit %d, --parts-at-once %s\n%s", code, parts, out)
	}
	for _, want := range []string{"under the member's own login, with a Lease on m1-pro", "upload, 1 part at a time: 1073741824 bytes in 95.0 s, 90.4 Mbit/s", "upload, 4 parts at a time: 1073741824 bytes in 70.0 s, 122.7 Mbit/s", "download: 1090519040 bytes in 60.1 s, 145.2 Mbit/s", "4 parts at a time", "Result: pass"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("the record lacks %q:\n%s", want, recorded)
		}
	}
	if strings.Contains(out, "was run") {
		t.Errorf("reading the member's numbers called the lab:\n%s", out)
	}

	if out, code, parts, _ := record(fmt.Sprintf(sent, "90.0", "95.4", "60.1", "145.2")); code != 0 || parts != "1" {
		t.Errorf("4 at a time 6%% faster: exit %d, --parts-at-once %s\n%s", code, parts, out)
	}

	out, code, parts, recorded = record(fmt.Sprintf(sent, "70.0", "122.7", "200.0", "43.6"))
	if code != 3 || parts != "1" || !strings.Contains(out, "STOP") || !strings.Contains(recorded, "Result: stopped") || !strings.Contains(recorded, "43.6 Mbit/s, under 50") {
		t.Errorf("a download under 50 Mbit/s: exit %d, --parts-at-once %s\n%s\n%s", code, parts, out, recorded)
	}

	good := fmt.Sprintf(sent, "70.0", "122.7", "60.1", "145.2")
	for what, text := range map[string]string{
		"an address":         good + "from 203.0.113.77\n",
		"a token":            strings.Replace(good, "fd05_format=1", "fd05_format=1\ntoken=lab_0123456789abcdef", 1),
		"a name":             strings.Replace(good, "upload_bytes=1073741824", "upload_bytes=1073741824 grace@desk", 1),
		"a number of no key": good + "uplink_mbit=940\n",
		"a number twice":     good + "download_mbit=900\n",
		"a number missing":   strings.Replace(good, "download_seconds=60.1\n", "", 1),
		"another format":     strings.Replace(good, "fd05_format=1", "fd05_format=2", 1),
		"a negative number":  strings.Replace(good, "upload_1_mbit=90.4", "upload_1_mbit=-90.4", 1),
	} {
		out, code, parts, recorded := record(text)
		if code != 1 || parts != "1" || recorded != "" {
			t.Errorf("%s was taken: exit %d, --parts-at-once %s, record %q\n%s", what, code, parts, recorded, out)
		}
		for _, leak := range []string{"203.0.113.77", "lab_0123456789abcdef", "grace@desk"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s was printed back:\n%s", what, out)
			}
		}
	}
}

// madeUpLab is a labd on loopback with sign-in through a stand-in for
// GitHub, an Admin (owner) with a login in a home of their own, and a
// simulated m1-pro with its baseline passed.
type madeUpLab struct {
	t         *testing.T
	url       string
	root      string
	ownerHome string
	bin       string
}

func (l *madeUpLab) env(home string) []string {
	return []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home + "/.config", "XDG_STATE_HOME=" + home + "/.local/state", "XDG_DATA_HOME=" + home + "/.local/share", "XDG_CACHE_HOME=" + home + "/.cache", "TMPDIR=" + home}
}

// start runs a program until the test ends and returns the address it says
// it serves at.
func start(t *testing.T, said *regexp.Regexp, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	pipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			if m := said.FindStringSubmatch(sc.Text()); m != nil {
				select {
				case found <- m[1]:
				default:
				}
			}
		}
	}()
	select {
	case addr := <-found:
		return addr
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not say where it serves", filepath.Base(name))
		return ""
	}
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func newMadeUpLab(t *testing.T) *madeUpLab {
	t.Helper()
	l := &madeUpLab{t: t, bin: bins(t), root: t.TempDir(), ownerHome: t.TempDir()}
	signIn := filepath.Join(l.root, "sign-in")
	os.MkdirAll(signIn, 0o700)
	github := start(t, regexp.MustCompile(`on (http://127\.0\.0\.1:\d+) with`), filepath.Join(l.bin, "fake-github"), "--listen", freePort(t), "--files", signIn, "--user", "owner:1001", "--user", "member:2002")
	if err := os.WriteFile(filepath.Join(signIn, "admins"), []byte("1001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(l.root, "var", "lab", "data")
	l.url = start(t, regexp.MustCompile(`labd listening on (http://127\.0\.0\.1:\d+)`), filepath.Join(l.bin, "labd"), "--listen=127.0.0.1:0", "--data="+data, "--sign-in="+signIn, "--github-url="+github, "--omt=false")

	token, code := run(t, l.env(l.ownerHome), "", "/bin/sh", "-c", `"$0" break-glass --data="$1" --account-id=1001 owner 2>/dev/null`, filepath.Join(l.bin, "labd"), data)
	if code != 0 {
		t.Fatalf("break-glass: %s", token)
	}
	l.lab(l.ownerHome, token, "login", l.url)
	enroll := l.lab(l.ownerHome, "", "enroll", "m1-pro", "--model", "desktop")
	enr := regexp.MustCompile(`enr_[0-9a-f]+`).FindString(enroll)
	if enr == "" {
		t.Fatalf("no Join command:\n%s", enroll)
	}
	sim := exec.Command(filepath.Join(l.bin, "lab-agent"), "sim", "--server", l.url, "--token", enr)
	sim.Env = append(os.Environ(), l.env(t.TempDir())...)
	if err := sim.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sim.Process.Kill(); sim.Wait() })
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if out, code := run(t, l.env(l.ownerHome), "", filepath.Join(l.bin, "lab"), "status", "m1-pro"); code == 0 && strings.Contains(out, "m1-pro") {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the simulated Mac did not Join")
		}
	}
	l.lab(l.ownerHome, "", "baseline", "m1-pro")
	return l
}

// lab runs lab under a home, and fails the test when lab does.
func (l *madeUpLab) lab(home, stdin string, args ...string) string {
	l.t.Helper()
	out, code := run(l.t, l.env(home), stdin, filepath.Join(l.bin, "lab"), args...)
	if code != 0 {
		l.t.Fatalf("lab %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out
}

type browser struct {
	t    *testing.T
	c    *http.Client
	base string
}

type named struct{ http.RoundTripper }

func (n named) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("User-Agent", "cutovertest/1")
	return n.RoundTripper.RoundTrip(r)
}

// signIn signs a browser in to the lab as one of the stand-in's accounts.
func (l *madeUpLab) signIn(account string) *browser {
	l.t.Helper()
	jar, _ := cookiejar.New(nil)
	b := &browser{t: l.t, base: l.url, c: &http.Client{Jar: jar, Transport: named{http.DefaultTransport}}}
	b.c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if strings.HasSuffix(req.URL.Path, "/login/oauth/authorize") {
			q := req.URL.Query()
			q.Set("as", account)
			req.URL.RawQuery = q.Encode()
		}
		return nil
	}
	if page := b.get("/sign-in"); !strings.Contains(page, "csrf_token") {
		l.t.Fatalf("account %s is not signed in:\n%.400s", account, page)
	}
	return b
}

func (b *browser) get(path string) string {
	b.t.Helper()
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

var csrfField = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// post sends a form of the page at from, as a browser would.
func (b *browser) post(from, path string, form url.Values) string {
	b.t.Helper()
	m := csrfField.FindStringSubmatch(b.get(from))
	if m == nil {
		b.t.Fatalf("%s has no form", from)
	}
	form.Set("csrf_token", m[1])
	req, _ := http.NewRequest("POST", b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.base)
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		b.t.Fatalf("POST %s: %s\n%.400s", path, resp.Status, body)
	}
	return string(body)
}

// The Front door's test 5 as decision S15 has it: the owner marks one M
// team member; the member signs in with their own GitHub account, makes a
// Lab token, and runs the script the owner sent, which installs lab from
// the lab's install line, takes a Lease on m1-pro under the member's own
// login, uploads for a job on that Mac, downloads a job's file, releases
// the Lease and prints numbers only.
func TestTheMemberRunsTheScriptUnderTheirOwnLogin(t *testing.T) {
	l := newMadeUpLab(t)

	job := l.transferJob()

	// The member signs in for the first time; the owner marks them on
	// People; the member makes a Lab token on their own page.
	member := l.signIn("2002")
	owner := l.signIn("1001")
	owner.post("/people", "/people/2002/m-team", url.Values{})
	page := member.post("/me", "/me/tokens", url.Values{"name": {"fd05"}, "period": {"30"}})
	token := regexp.MustCompile(`lab_[0-9A-Za-z_-]{16,}`).FindString(page)
	if token == "" {
		t.Fatalf("the member's page shows no new Lab token:\n%.600s", page)
	}

	// The lab's install line, played here: it puts lab where the real one does.
	target, _ := url.Parse(l.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/install/lab" {
			fmt.Fprintf(w, "mkdir -p \"$HOME/.local/bin\" && cp %q \"$HOME/.local/bin/lab\"\n", filepath.Join(l.bin, "lab"))
			return
		}
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	home := t.TempDir()
	env := append(l.env(home), "LAB_ADDRESS="+front.URL, "LAB_BYTES="+fmt.Sprint(40*mib), "MACLAB_URL=https://another.example", "MACLAB_TOKEN=not-this-labs")
	script := exec.Command("/bin/sh", frontDoor(t, "fd05-member.sh"), job)
	script.Env = append(os.Environ(), env...)
	script.Stdin = strings.NewReader(token + "\n")
	var said strings.Builder
	script.Stderr = &said
	printed, err := script.Output()
	if err != nil {
		t.Fatalf("the member's script: %v\n%s\n%s", err, printed, said.String())
	}

	// Nine numbers between two rules, and nothing else.
	lines := strings.Split(strings.TrimSpace(string(printed)), "\n")
	if len(lines) != 11 || lines[0] != "----------" || lines[10] != "----------" {
		t.Fatalf("the member's script printed:\n%s", printed)
	}
	for _, line := range lines[1:10] {
		if !regexp.MustCompile(`^[a-z0-9_]+=[0-9]+(\.[0-9]+)?$`).MatchString(line) {
			t.Errorf("not a number: %q", line)
		}
	}
	for _, want := range []string{"upload_bytes=41943040", "download_bytes=41943040"} {
		if !strings.Contains(string(printed), want) {
			t.Errorf("the numbers lack %s:\n%s", want, printed)
		}
	}
	for _, leak := range []string{token, "127.0.0.1", "member", "another.example"} {
		if strings.Contains(string(printed), leak) {
			t.Errorf("the numbers hold %q", leak)
		}
	}
	if strings.Contains(said.String(), token) {
		t.Error("the script showed the Lab token")
	}

	// The owner reads them as the member's script printed them.
	sentBack := filepath.Join(t.TempDir(), "sent")
	os.WriteFile(sentBack, printed, 0o600)
	dir, _, _ := deployCopy(t)
	if out, code := run(t, nil, "", frontDoor(t, "fd05-outside-transfers"), "--lab", "--record", sentBack, "--deploy", dir, "--records", t.TempDir()); code != 0 || !strings.Contains(out, "None under 50") {
		t.Errorf("--record of the member's own output: exit %d\n%s", code, out)
	}

	// lab came from the install line, into the member's own home, and the
	// login is the member's, at the address they were given.
	if _, err := os.Stat(filepath.Join(home, ".local", "bin", "lab")); err != nil {
		t.Errorf("lab was not installed in the member's home: %v", err)
	}
	if strings.Contains(said.String(), "another.example") {
		t.Errorf("the run went to the lab the member's environment named:\n%s", said.String())
	}

	// The Lease was the member's own and is released; the lab holds it in
	// the Mac's Lease record under their name.
	if status := l.lab(l.ownerHome, "", "status", "m1-pro"); strings.Contains(status, "leased by") || strings.Contains(status, "front-door-test-5") {
		t.Errorf("the Lease was not released:\n%s", status)
	}
	leases := l.api(token, "GET", "/api/devices/m1-pro/leases", "")
	if !strings.Contains(leases, "member") {
		t.Errorf("the Lease record does not name the member:\n%s", leases)
	}
	if strings.Contains(leases, hostname()) && hostname() != "" {
		t.Errorf("the Lease carried this machine's name:\n%s", leases)
	}

	// The member is M team, not a Maintainer: with no Lease the same upload
	// is refused, so the uploads above went under the rule for a job on a
	// Mac they lease.
	file := filepath.Join(home, "small")
	os.WriteFile(file, []byte("no Lease now\n"), 0o600)
	if out, code := run(t, l.env(home), "", filepath.Join(home, ".local", "bin", "lab"), "upload", file, "--for", "m1-pro"); code == 0 || !strings.Contains(out, "ease") {
		t.Errorf("an upload with no Lease was taken: exit %d\n%s", code, out)
	}
	if strings.Contains(l.api(token, "GET", "/api/builds", ""), "[") {
		t.Error("the member's token reads what only Maintainers read")
	}
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// api asks labd one thing with a token and returns the body.
func (l *madeUpLab) api(token, method, path, body string) string {
	l.t.Helper()
	req, _ := http.NewRequest(method, l.url+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "cutovertest/1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// owner is the environment of the owner's own run through the Lab address,
// which here is the labd on loopback.
func (l *madeUpLab) owner(address string) []string {
	home := l.ownerHome
	if address != l.url {
		// The same lab behind another address: the owner logs in there.
		home = l.t.TempDir()
		token, code := run(l.t, l.env(home), "", "/bin/sh", "-c", `"$0" break-glass --data="$1" --account-id=1001 owner 2>/dev/null`, filepath.Join(l.bin, "labd"), filepath.Join(l.root, "var", "lab", "data"))
		if code != 0 {
			l.t.Fatalf("break-glass: %s", token)
		}
		l.lab(home, token, "login", address)
	}
	return append(l.env(home), "LAB_ADDRESS="+address, "LAB_COMMAND="+filepath.Join(l.bin, "lab"))
}

// transferJob is the owner's side of the download: --prepare runs the job
// that leaves the file on the Mac. The simulated Mac runs no script, so the
// test plays its part and writes the file among the job's.
func (l *madeUpLab) transferJob() string {
	l.t.Helper()
	out, code := run(l.t, l.owner(l.url), "", frontDoor(l.t, "fd05-outside-transfers"), "--lab", "--prepare")
	m := regexp.MustCompile(`The member's command line is: sh fd05-member\.sh (j[0-9a-z-]+)`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		l.t.Fatalf("--prepare: exit %d\n%s", code, out)
	}
	made := filepath.Join(l.root, "var", "lab", "data", "jobs", m[1], "test-transfer-file")
	os.MkdirAll(made, 0o755)
	if out, code := run(l.t, []string{"MACLAB_OUT=" + made, "TRANSFER_BYTES=" + fmt.Sprint(8*mib)}, "", "/bin/bash", frontDoor(l.t, "transfer-file.sh")); code != 0 {
		l.t.Fatalf("transfer-file.sh: %s", out)
	}
	return m[1]
}

// ssh plays the mini as the owner reaches it: it restarts no tunnel, counts
// CPU seconds as cloudflared's metrics would, and runs the real page-times
// against the labd of the test, after the line sudo would ask with.
const sshStub = `#!/bin/bash
echo "ssh $*" >> "$SSH_STATE/calls"
case "$*" in
  *"/transport http2") [[ ! -f $SSH_STATE/http2-fails ]] || { echo "transport: the tunnel did not connect under http2" >&2; exit 1; } ;;
  *"/transport quic"|*"/transport deployed") ;;
  *"/metrics")
    n=$(( $(cat "$SSH_STATE/cpu" 2>/dev/null || echo 0) + ${SSH_CPU_STEP:-1} ))
    echo "$n" > "$SSH_STATE/cpu"
    echo "# TYPE process_cpu_seconds_total counter"
    awk -v n="$n" 'BEGIN { printf "process_cpu_seconds_total %.2f\n", n / 100 }' ;;
  *"sudo -u lab /usr/local/lab/current/scripts/mini/page-times")
    printf 'Password:'
    LAB_SYSROOT="$SSH_ROOT" LAB_LOOPBACK="$SSH_LOOPBACK" "$SSH_REPO/scripts/mini/page-times" ;;
  *) echo "ssh: not expected: $*" >&2; exit 64 ;;
esac
`

// mini puts the stand-in for ssh first in PATH and returns the environment
// and the file its calls are written to.
func (l *madeUpLab) mini(address string) (env []string, state string) {
	l.t.Helper()
	state = l.t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "ssh"), []byte(sshStub), 0o755); err != nil {
		l.t.Fatal(err)
	}
	root, _ := filepath.Abs(repo(l.t))
	return append(l.owner(address), "PATH="+state+":"+os.Getenv("PATH"), "SSH_STATE="+state, "SSH_ROOT="+l.root,
		"SSH_LOOPBACK="+strings.TrimPrefix(l.url, "http://"), "SSH_REPO="+root), state
}

func calls(t *testing.T, state string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(state, "calls"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// The Front door's test 15 as the owner runs it, with the mini played: the
// tunnel is put under QUIC, then HTTP/2, then back under the Deployed
// files, cloudflared's CPU is read around each transfer, and the transport
// that won is written in deploy/.
func TestFD15SwitchesTheTunnelMeasuresBothAndPutsItBack(t *testing.T) {
	l := newMadeUpLab(t)
	job := l.transferJob()
	const transport = "ssh -t owner@mini sudo /usr/local/lab/current/scripts/mini/transport "
	const metrics = "ssh owner@mini curl -fsS --max-time 10 http://127.0.0.1:20241/metrics"

	for _, lacking := range [][]string{{"--job", job}, {"--mini", "owner@mini"}, {"--mini", "owner@mini; reboot", "--job", job}, {"--mini", "owner@mini", "--job", "../etc"}} {
		env, state := l.mini(l.url)
		if out, code := run(t, env, "", frontDoor(t, "fd15-transport"), append([]string{"--lab", "--records", t.TempDir()}, lacking...)...); code == 0 || calls(t, state)[0] != "" {
			t.Errorf("fd15 --lab %v: exit %d, calls %v\n%s", lacking, code, calls(t, state), out)
		}
	}

	env, state := l.mini(l.url)
	dir, _, routes := deployCopy(t)
	records := t.TempDir()
	out, code := run(t, env, "", frontDoor(t, "fd15-transport"), "--lab", "--mini", "owner@mini", "--job", job, "--size", fmt.Sprint(40*mib), "--deploy", dir, "--records", records)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	want := []string{transport + "quic", metrics, metrics, metrics, transport + "http2", metrics, metrics, metrics, transport + "deployed"}
	if got := calls(t, state); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the mini was asked:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	files, _ := filepath.Glob(filepath.Join(records, "*-front-door-15.md"))
	if len(files) != 1 {
		t.Fatalf("records: %v\n%s", files, out)
	}
	record := read(t, files[0])
	for _, re := range []string{`--lab\x60, through http://127\.0\.0\.1:\d+\.`, `- quic, up: 41943040 bytes in [0-9.]+ s, [0-9.]+ Mbit/s, cloudflared at 0\.\d+ of a core`, `- quic, down: 41943040 bytes in `, `- http2, up: 41943040 bytes`, `- http2, down: 41943040 bytes`, `Result: pass`} {
		if !regexp.MustCompile(re).MatchString(record) {
			t.Errorf("the record lacks %s:\n%s", re, record)
		}
	}
	chosen := setting(t, protocolLine, routes)
	if (chosen == "http2") != strings.Contains(record, "- HTTP/2: ") || (chosen == "quic") != strings.Contains(record, "- QUIC stays: ") {
		t.Errorf("deploy says %s and the record says otherwise:\n%s", chosen, record)
	}

	// A login for another lab than the one the test runs through: nothing
	// is measured and the mini is asked nothing.
	env, state = l.mini(l.url)
	env = append(env, "LAB_ADDRESS=http://127.0.0.1:9")
	if out, code := run(t, env, "", frontDoor(t, "fd15-transport"), "--lab", "--mini", "owner@mini", "--job", job, "--records", t.TempDir()); code == 0 || calls(t, state)[0] != "" || !strings.Contains(out, "not logged in to Omarchy Mac Lab at http://127.0.0.1:9") {
		t.Errorf("a login for another address: exit %d, calls %v\n%s", code, calls(t, state), out)
	}

	// A tunnel that does not come up under HTTP/2 ends the run, and the
	// tunnel is still put back under the Deployed files.
	env, state = l.mini(l.url)
	os.WriteFile(filepath.Join(state, "http2-fails"), nil, 0o644)
	dir, _, routes = deployCopy(t)
	out, code = run(t, env, "", frontDoor(t, "fd15-transport"), "--lab", "--mini", "owner@mini", "--job", job, "--size", fmt.Sprint(40*mib), "--deploy", dir, "--records", t.TempDir())
	got := calls(t, state)
	if code == 0 || got[len(got)-1] != transport+"deployed" || setting(t, protocolLine, routes) != "quic" || strings.Contains(out, ": pass") {
		t.Errorf("exit %d, the mini was asked %v\n%s", code, got, out)
	}
}

// The Front door's test 16 as the owner runs it, with the edge and the
// mini played: each of the four targets is judged, and one missed fails
// the run.
func TestFD16JudgesEachOfTheFourTargets(t *testing.T) {
	l := newMadeUpLab(t)
	job := l.transferJob()
	target, _ := url.Parse(l.url)
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.Request.URL.Path == "/" {
			r.Header.Set("CF-Cache-Status", "HIT")
		}
		return nil
	}
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = target.Host
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(edge.Close)

	fd16 := func(address string, more []string, args ...string) (string, string, int) {
		t.Helper()
		env, _ := l.mini(address)
		records := t.TempDir()
		out, code := run(t, append(env, more...), "", frontDoor(t, "fd16-performance"),
			append([]string{"--lab", "--mini", "owner@mini", "--job", job, "--size", fmt.Sprint(40 * mib), "--records", records}, args...)...)
		files, _ := filepath.Glob(filepath.Join(records, "*-front-door-16.md"))
		if len(files) != 1 {
			return out, "", code
		}
		return out, read(t, files[0]), code
	}
	verdicts := func(record string) string {
		var v []string
		for _, m := range regexp.MustCompile(`- target (\d): (pass|FAIL)\.`).FindAllStringSubmatch(record, -1) {
			v = append(v, m[1]+m[2])
		}
		return strings.Join(v, " ")
	}

	out, record, code := fd16(edge.URL, nil, "--link-up", "1", "--link-down", "1")
	if code != 0 || verdicts(record) != "1pass 2pass 3pass 4pass" || !strings.Contains(record, "Result: pass") {
		t.Fatalf("exit %d\n%s\n%s", code, out, record)
	}
	for _, re := range []string{`18 of 18 were an edge HIT`, `labd took \d+ ms at the 95th percentile over 2\d\d requests for 1\d signed-out pages`, `up [0-9.]+ Mbit/s, \d+% of a 1 Mbit/s link`, `cloudflared used 0\.\d+ of a core while uploading and 0\.\d+ while downloading`} {
		if !regexp.MustCompile(re).MatchString(record) {
			t.Errorf("the record lacks %s:\n%s", re, record)
		}
	}
	if strings.Contains(record, "Password") {
		t.Errorf("sudo's prompt is in the record:\n%s", record)
	}

	// No edge in front: the page is no HIT.
	if out, record, code := fd16(l.url, nil, "--link-up", "1", "--link-down", "1"); code != 1 || verdicts(record) != "1FAIL 2pass 3pass 4pass" || !strings.Contains(record, "0 of 18 were an edge HIT") || !strings.Contains(record, "Result: fail") {
		t.Errorf("with no edge: exit %d\n%s\n%s", code, out, record)
	}
	// A link the transfers do not fill to 80%.
	if out, record, code := fd16(edge.URL, nil, "--link-up", "1", "--link-down", "100000000"); code != 1 || verdicts(record) != "1pass 2pass 3FAIL 4pass" {
		t.Errorf("under 80%% of the link: exit %d\n%s\n%s", code, out, record)
	}
	// cloudflared over one core.
	if out, record, code := fd16(edge.URL, []string{"SSH_CPU_STEP=100000"}, "--link-up", "1", "--link-down", "1"); code != 1 || verdicts(record) != "1pass 2pass 3pass 4FAIL" {
		t.Errorf("over one core: exit %d\n%s\n%s", code, out, record)
	}
	// Without the link speeds there is nothing to compare with.
	if out, _, code := fd16(edge.URL, nil); code == 0 || !strings.Contains(out, "--link-up and --link-down") {
		t.Errorf("without link speeds: exit %d\n%s", code, out)
	}
}
