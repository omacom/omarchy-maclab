package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// joinRun runs a Join command with stand-ins for curl and sudo: curl hands
// out the Join command's script, lab-agent and its signature as the lab
// would, and each records its arguments, what it read on stdin and whether
// LAB_JOIN reached it.
type joinRun struct {
	t        *testing.T
	bin, rec string
	exit     int // the last run's exit status
}

func newJoinRun(t *testing.T, script, agentBody, sig []byte) *joinRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	r := &joinRun{t: t, bin: t.TempDir(), rec: t.TempDir()}
	r.put("join.sh", script)
	r.put("agent", agentBody)
	r.put("sig", sig)
	r.stand("curl", `printf '%s\n' "$@" >> "$REC/curl.argv"
env | grep '^LAB_JOIN=' >> "$REC/leaked"
url= out= hdr=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -H) if [ "$2" = @- ]; then hdr=1; fi; shift ;;
    http*) url=$1 ;;
  esac
  shift
done
if [ -n "$hdr" ]; then cat >> "$REC/curl.stdin"; fi
case "$url" in
  */join) cat "$REC/join.sh" ;;
  */api/join/lab-agent.sig) cp "$REC/sig" "$out" ;;
  */api/join/lab-agent) cp "$REC/agent" "$out"; printf '%s' "${CODE:-200}" ;;
  *) exit 7 ;;
esac
`)
	r.stand("sudo", `printf '%s\n' "$@" > "$REC/sudo.argv"
env | grep '^LAB_JOIN=' >> "$REC/leaked"
if [ -e "$REC/exec" ]; then exec "$@"; fi
cat > "$REC/sudo.stdin"
`)
	if runtime.GOOS != "linux" { // the script runs on Omarchy and calls GNU sha256sum
		r.stand("sha256sum", "exec shasum -a 256 -c -s\n")
	}
	return r
}

func (r *joinRun) put(name string, b []byte) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.rec, name), b, 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *joinRun) stand(name, script string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.bin, name), []byte("#!/bin/bash\n"+script), 0o755); err != nil {
		r.t.Fatal(err)
	}
}

func (r *joinRun) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.rec, name))
	return string(b)
}

func (r *joinRun) env(more ...string) []string {
	return append([]string{"PATH=" + r.bin + ":" + os.Getenv("PATH"), "REC=" + r.rec, "USER=owner", "HOME=" + r.t.TempDir(), "TMPDIR=" + r.t.TempDir()}, more...)
}

// run runs line in bash, as a terminal does, and says whether sudo was
// reached.
func (r *joinRun) run(line string, env ...string) (out string, reached bool) {
	r.t.Helper()
	for _, f := range []string{"curl.argv", "curl.stdin", "sudo.argv", "sudo.stdin", "leaked"} {
		os.Remove(filepath.Join(r.rec, f))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", line)
	cmd.Env = r.env(env...)
	b, err := cmd.CombinedOutput()
	r.exit = cmd.ProcessState.ExitCode()
	if err != nil && cmd.ProcessState == nil {
		r.t.Fatal(err)
	}
	_, err = os.Stat(filepath.Join(r.rec, "sudo.argv"))
	return string(b), err == nil
}

// joinScriptOf is what labd serves at /join, with its answer.
func joinScriptOf(t *testing.T, h http.Handler, url string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", url, nil)
	r.RemoteAddr = viaProxy
	r = r.WithContext(onTunnel(r.Context()))
	r.Header.Set("X-Forwarded-Proto", "https")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The Join command is one short line: curl fetches the script, which holds
// no secret, from the Lab address, and bash runs it with the token in its
// environment. The token is in the line once, never in a URL or an
// argument, and labd never logs it.
func TestTheJoinCommandIsOneShortLine(t *testing.T) {
	j := newJoinLab(t)
	made := j.invite("one-liner")
	if want := " curl -fsSL https://chonkbase.net/maclab/join | LAB_JOIN=" + made.Token + " bash"; made.JoinCommand != want {
		t.Errorf("the Join command is\n%q\nwant\n%q", made.JoinCommand, want)
	}
	j.do("GET", "/api/join/terms", made.Token, "")
	j.do("GET", "/api/join/lab-agent", made.Token, "")
	j.enroll(made.Token, keyA, termsSum())
	j.mu.Lock()
	logs := j.logs.String()
	j.mu.Unlock()
	if strings.Contains(logs, made.Token) || strings.Contains(logs, keyA) {
		t.Errorf("labd logged the token or activation key:\n%s", logs)
	}
	j.s.cfg.AgentBinary = filepath.Join(t.TempDir(), "none")
	if none := j.invite("no-agent"); none.JoinCommand != "" {
		t.Errorf("a Join command without a lab-agent: %s", none.JoinCommand)
	}
}

// /join is the script, to anyone who asks and with no credential: the Lab
// address, the current lab-agent's sha256 and https only written in it, and
// no token. It is made for each request and never kept, by the browser or
// the edge.
func TestTheJoinScriptIsServedToAnyoneAndNeverKept(t *testing.T) {
	j := newJoinLab(t)
	j.invite("someone")
	w := joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/x-shellscript; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get(cacheEdgeHeader) != "" {
		t.Fatalf("/join: %d %v", w.Code, w.Header())
	}
	script := w.Body.String()
	for _, want := range []string{"#!/bin/bash\n", "u='https://chonkbase.net/maclab'", "echo '" + j.s.agentSHA256() + "  '", "sha256sum -c --quiet",
		"--proto '=https' --tlsv1.2", `printf 'Authorization: Bearer %s\n' "$t" | curl`, "-H @-", `"$u/api/join/lab-agent"`,
		`printf '%s\n' "$t" | sudo "$d/lab-agent" setup --server "$u" --token-stdin --gui-user "$USER"`, "unset LAB_JOIN", joinSaysNoToken,
		joinSaysUsed, joinSaysTooMany, joinSaysChallenged} {
		if !strings.Contains(script, want) {
			t.Errorf("the script lacks %q:\n%s", want, script)
		}
	}
	if regexp.MustCompile(`enr_[0-9a-f]`).MatchString(script) || strings.Contains(script, "--allow-plain-http") || strings.Contains(script, "--token ") {
		t.Errorf("the script holds a token, allows plain http or puts a token in an argument:\n%s", script)
	}
	if !strings.HasSuffix(script, "\n{ lab_join_this_mac; }\n") {
		t.Errorf("the script does not end calling its one function:\n%s", script)
	}
	if w := joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join", "Cookie", "lab_session=anything", "Authorization", "Bearer lab_adm_secret"); w.Code != 200 || w.Body.String() != script || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("/join with a credential: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if edgeKeeps("/join") {
		t.Error("the edge may keep /join")
	}
	r := httptest.NewRequest("HEAD", "http://chonkbase.net/maclab/join", nil)
	r.RemoteAddr = viaProxy
	r = r.WithContext(onTunnel(r.Context()))
	r.Header.Set("X-Forwarded-Proto", "https")
	hw := httptest.NewRecorder()
	j.h.ServeHTTP(hw, r)
	if hw.Code != 200 || hw.Body.Len() != 0 {
		t.Errorf("HEAD /join: %d, %d bytes", hw.Code, hw.Body.Len())
	}
	// A new lab-agent is in the next script at once.
	os.WriteFile(j.s.cfg.AgentBinary, []byte("\x7fELF another lab-agent"), 0o755)
	if again := joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join").Body.String(); !strings.Contains(again, j.s.agentSHA256()) || again == script {
		t.Error("the script does not name the lab-agent labd hands out now")
	}
	j.s.cfg.AgentBinary = filepath.Join(t.TempDir(), "none")
	if w := joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join"); w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "#!") {
		t.Errorf("/join with no lab-agent: %d %q %q", w.Code, w.Header().Get("Cache-Control"), w.Body)
	}
}

// Over plain http (a test or LAN labd), the script says so: curl isn't held
// to https and setup must allow plain http. An address that isn't safe in
// single quotes gets no script and no line at all.
func TestJoinCommandForPlainHTTPAndUnsafeURLs(t *testing.T) {
	agentBin := filepath.Join(t.TempDir(), "lab-agent")
	os.WriteFile(agentBin, []byte("x"), 0o755)
	s := &Server{cfg: Config{AgentBinary: agentBin}}
	const tok = "enr_0123456789abcdef0123456789abcdef"
	r := httptest.NewRequest("POST", "http://10.1.1.55:7770/api/enroll-tokens", nil)
	script := string(s.joinScript(r))
	if !strings.Contains(script, "u='http://10.1.1.55:7770'") || strings.Contains(script, "--proto") || !strings.Contains(script, `--gui-user "$USER" --allow-plain-http`) {
		t.Errorf("plain http script:\n%s", script)
	}
	if cmd := s.joinCommand(r, tok); cmd != " curl -fsSL http://10.1.1.55:7770/join | LAB_JOIN="+tok+" bash" {
		t.Errorf("plain http line: %q", cmd)
	}
	if cmd := s.joinCommand(httptest.NewRequest("POST", "http://[::1]:7770/", nil), tok); cmd != " curl -fsSL 'http://[::1]:7770/join' | LAB_JOIN="+tok+" bash" {
		t.Errorf("an address with brackets is not quoted: %q", cmd)
	}
	r.Host = "evil.example';rm -rf ~;'"
	if s.joinScript(r) != nil || s.joinCommand(r, tok) != "" {
		t.Error("a script or a line for an unsafe host")
	}
	if cmd := s.joinCommand(httptest.NewRequest("POST", "http://10.1.1.55:7770/", nil), "enr_x';id;'"); cmd != "" {
		t.Errorf("a line for an unsafe token: %s", cmd)
	}
}

// The Join command itself, run by bash: the token reaches curl and setup on
// stdin only, in no process's arguments, and no program the script starts
// finds it in its environment. A download that isn't labd's lab-agent never
// runs, and each refusal is said once in its own words.
func TestTheJoinCommandRunsWithTheTokenInNoArguments(t *testing.T) {
	j := newJoinLab(t)
	made := j.invite("argv")
	agentBody, _ := os.ReadFile(j.s.cfg.AgentBinary)
	run := newJoinRun(t, joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join").Body.Bytes(), agentBody, nil)
	if out, reached := run.run(made.JoinCommand); !reached {
		t.Fatalf("the Join command did not reach sudo:\n%s", out)
	}
	if strings.Contains(run.read("curl.argv"), made.Token) || strings.Contains(run.read("sudo.argv"), made.Token) || run.read("leaked") != "" {
		t.Errorf("the token is in a command's arguments or environment:\ncurl %q\nsudo %q\nleaked %q", run.read("curl.argv"), run.read("sudo.argv"), run.read("leaked"))
	}
	if run.read("curl.stdin") != "Authorization: Bearer "+made.Token+"\n" || run.read("sudo.stdin") != made.Token+"\n" {
		t.Errorf("stdin: curl %q, sudo %q", run.read("curl.stdin"), run.read("sudo.stdin"))
	}
	if argv := run.read("curl.argv"); !strings.Contains(argv, "https://chonkbase.net/maclab/join\n") || !strings.Contains(argv, "https://chonkbase.net/maclab/api/join/lab-agent\n") {
		t.Errorf("curl was asked for %q", argv)
	}
	if argv := run.read("sudo.argv"); !strings.Contains(argv, "\nsetup\n--server\nhttps://chonkbase.net/maclab\n--token-stdin\n--gui-user\nowner\n") {
		t.Errorf("sudo ran %q", argv)
	}

	for code, says := range map[string]string{"401": joinSaysUsed, "429": joinSaysTooMany, "403challenge": joinSaysChallenged, "502": joinSaysOther + " (502)."} {
		if out, reached := run.run(made.JoinCommand, "CODE="+code); reached || strings.TrimSpace(out) != says {
			t.Errorf("a %s from the lab: sudo reached %v, said %q", code, reached, out)
		}
	}
	run.put("agent", []byte("#!/bin/sh\necho pwned\n"))
	if out, reached := run.run(made.JoinCommand); reached {
		t.Errorf("sudo ran with a wrong download:\n%s", out)
	}
	run.put("agent", agentBody)
	// Without the token, or with something else in its place, the script
	// says where the line comes from and starts nothing.
	for _, line := range []string{" curl -fsSL https://chonkbase.net/maclab/join | bash", " curl -fsSL https://chonkbase.net/maclab/join | LAB_JOIN='enr_x;id' bash"} {
		out, reached := run.run(line)
		if reached || strings.TrimSpace(out) != joinSaysNoToken || strings.Count(run.read("curl.argv"), "\n") != 2 {
			t.Errorf("%s: sudo reached %v, curl asked %q, said %q", line, reached, run.read("curl.argv"), out)
		}
	}
}

// Under curl | bash the shell's stdin is the script itself. setup reads
// the token from its own stdin and asks its questions on the terminal, so
// the person still types their yes there, and the script that follows is
// never eaten.
func TestTheJoinCommandAsksOnTheTerminalUnderCurlPipeBash(t *testing.T) {
	scriptCmd, err := exec.LookPath("script")
	if err != nil {
		t.Skip("no script(1) to give the run a terminal")
	}
	j := newJoinLab(t)
	// The lab-agent handed out stands in for setup: the token from stdin,
	// the answer from the terminal.
	stand := []byte(`#!/bin/bash
IFS= read -r tok
printf '%s' "$tok" > "$REC/setup.token"
[ -t 0 ] && echo tty > "$REC/setup.stdin-is-a-terminal"
printf 'Type yes to accept these terms: ' > /dev/tty
IFS= read -r answer < /dev/tty
printf '%s' "$answer" > "$REC/setup.answer"
`)
	os.WriteFile(j.s.cfg.AgentBinary, stand, 0o755)
	made := j.invite("terminal")
	run := newJoinRun(t, joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join").Body.Bytes(), stand, nil)
	run.put("exec", nil)
	bash, _ := exec.LookPath("bash")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if runtime.GOOS == "linux" {
		cmd = exec.CommandContext(ctx, scriptCmd, "-qec", made.JoinCommand, "/dev/null")
	} else {
		cmd = exec.CommandContext(ctx, scriptCmd, "-q", "/dev/null", bash, "-c", made.JoinCommand)
	}
	cmd.Env = run.env("SHELL=" + bash)
	in, _ := cmd.StdinPipe()
	out := &lockedBuffer{b: &bytes.Buffer{}}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The answer is typed once the question is on the terminal.
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && !strings.Contains(out.String(), "Type yes"); {
		time.Sleep(20 * time.Millisecond)
	}
	io.WriteString(in, "yes\n")
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the Join command under a terminal: %v\n%s", err, out.String())
	}
	if run.read("setup.answer") != "yes" || run.read("setup.token") != made.Token || run.read("setup.stdin-is-a-terminal") != "" {
		t.Errorf("setup read the token %q on stdin (a terminal: %q) and the answer %q on the terminal:\n%s",
			run.read("setup.token"), run.read("setup.stdin-is-a-terminal"), run.read("setup.answer"), out.String())
	}
}

// A script cut short anywhere, as by a dropped connection, runs nothing:
// bash refuses a function or a group left open whole.
func TestACutJoinScriptRunsNothing(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	j := newJoinLab(t)
	script := joinScriptOf(t, j.h, "http://chonkbase.net/maclab/join").Body.Bytes()
	bin, rec := t.TempDir(), t.TempDir()
	for _, tool := range []string{"curl", "sudo", "mktemp", "sha256sum", "shasum", "chmod", "ssh-keygen", "cat", "cp", "env", "grep", "rm"} {
		os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\necho "+tool+" >> \"$REC/ran\"\n"), 0o755)
	}
	ran := func(b []byte) string {
		os.Remove(filepath.Join(rec, "ran"))
		cmd := exec.Command(bash)
		cmd.Stdin = bytes.NewReader(b)
		cmd.Env = []string{"PATH=" + bin, "REC=" + rec, "LAB_JOIN=enr_0123456789abcdef0123456789abcdef", "USER=owner"}
		cmd.Run()
		got, _ := os.ReadFile(filepath.Join(rec, "ran"))
		return string(got)
	}
	if got := ran(script); !strings.HasPrefix(got, "mktemp\n") {
		t.Fatalf("the whole script ran %q", got)
	}
	var cuts []int
	start := 0
	for i, c := range script {
		if c == '\n' {
			cuts = append(cuts, start+(i-start)/2, i, i+1)
			start = i + 1
		}
	}
	for _, n := range cuts {
		if n <= 0 || n >= len(script)-1 { // all but the last newline is the whole script
			continue
		}
		if got := ran(script[:n]); got != "" {
			t.Errorf("the script cut after %d bytes (%q) ran %q", n, script[max(0, n-30):n], got)
		}
	}
}
