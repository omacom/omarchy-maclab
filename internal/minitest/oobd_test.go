package minitest

import (
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/oob"
	"github.com/omacom/omarchy-maclab/internal/oobd"
)

const (
	rootToken = "usr/local/lab/secrets/oobd.token"
	labToken  = "var/lab/secrets/oobd.token"
	oobdPlist = "Library/LaunchDaemons/" + oobdLabel + ".plist"
)

// The controller runs as root, from the Deployed build, on a loopback port,
// with its token and its tool named by file and nothing of the old lab's:
// no token flag, no variable, no serial device, no camera.
func TestTheControllersLaunchDaemon(t *testing.T) {
	file := filepath.Join(repo(t), "deploy", oobdLabel+".plist")
	values, arrays := plist(t, file)
	for key, want := range map[string]string{
		"Label":     oobdLabel,
		"RunAtLoad": "true",
		"KeepAlive": "true",
		"Umask":     "63",
		"UserName":  "",
		"GroupName": "",
	} {
		if values[key] != want {
			t.Errorf("%s is %q, want %q", key, values[key], want)
		}
	}
	want := []string{
		"/usr/local/lab/current/bin/oobd",
		"--listen", shell(t, "OOBD_LISTEN"),
		"--token-file", "/usr/local/lab/secrets/oobd.token",
		"--tool", "/usr/local/lab/current/bin/" + shell(t, "RESET_TOOL_NAME"),
	}
	if got := arrays["ProgramArguments"]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("oobd is started as %v, want %v", got, want)
	}
	if !strings.HasPrefix(shell(t, "OOBD_LISTEN"), "127.0.0.1:") {
		t.Errorf("the controller listens at %s, which is not loopback", shell(t, "OOBD_LISTEN"))
	}
	text := read(t, file)
	for _, word := range []string{"TOKEN", "SECRET", "PASSWORD", "EnvironmentVariables", "--serial", "--camera", "--baud", "<string>--token</string>"} {
		if strings.Contains(text, word) {
			t.Errorf("the controller's LaunchDaemon file holds %s", word)
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("plutil", "-lint", file).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v\n%s", err, out)
		}
	}

	main := read(t, filepath.Join(repo(t), "cmd", "oobd", "main.go"))
	for _, flag := range []string{"listen", "token-file", "tool"} {
		if !regexp.MustCompile(`flag\.\w+\("` + flag + `"`).MatchString(main) {
			t.Errorf("oobd has no flag --%s", flag)
		}
	}
	for _, gone := range []string{`"token"`, `"serial"`, `"baud"`, `"camera"`, `flag.String("log"`, "OOBD_TOKEN", "Getenv"} {
		if strings.Contains(main, gone) {
			t.Errorf("oobd still has %s", gone)
		}
	}
	if n := len(regexp.MustCompile(`flag\.\w+\("`).FindAllString(main, -1)); n != 3 {
		t.Errorf("oobd has %d flags, want 3", n)
	}
}

// deployed makes tag the mini's running build, with a controller in it and,
// when tool is not empty, a reset tool with that text.
func (m *mini) deployed(tag, tool string) {
	m.t.Helper()
	bin := m.path("usr/local/lab/builds/" + tag + "/bin")
	write(m.t, bin+"/oobd", "#!/bin/sh\n", 0o755)
	if tool != "" {
		write(m.t, bin+"/macvdmtool", tool, 0o755)
	}
	os.Remove(m.path("usr/local/lab/current"))
	if err := os.Symlink("builds/"+tag, m.path("usr/local/lab/current")); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mini) oobdLoaded() bool {
	return exists(filepath.Join(filepath.Dir(m.root), "state", "loaded."+oobdLabel))
}

func TestSetupMakesTheControllersTokenForRootAndForLab(t *testing.T) {
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)

	dry := m.ok("setup", "--dry-run", "--release-key", k.signers)
	for _, want := range []string{m.path(rootToken), m.path(labToken), m.path(oobdPlist)} {
		if !strings.Contains(dry, "would") || !strings.Contains(dry, want) {
			t.Errorf("the dry run doesn't mention %s:\n%s", want, dry)
		}
	}
	if exists(m.path(rootToken)) || exists(m.path(labToken)) || exists(m.path(oobdPlist)) {
		t.Fatal("a dry run made the token or the LaunchDaemon file")
	}

	out := m.ok("setup", "--release-key", k.signers)
	token := strings.TrimSpace(read(t, m.path(rootToken)))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(token) {
		t.Fatalf("root's token is not 64 hex digits: %d characters", len(token))
	}
	if read(t, m.path(labToken)) != read(t, m.path(rootToken)) {
		t.Error("labd's copy is not root's token")
	}
	for path, want := range map[string]os.FileMode{
		"usr/local/lab/secrets": 0o700,
		rootToken:               0o600,
		"var/lab/secrets":       0o700,
		labToken:                0o600,
		oobdPlist:               0o644,
	} {
		if got := mode(t, m.path(path)); got != want {
			t.Errorf("%s is %v, want %v", path, got, want)
		}
	}
	// Each side reads its copy the way its daemon does.
	for _, path := range []string{rootToken, labToken} {
		if got, err := oob.ReadToken(m.path(path)); err != nil || got != token {
			t.Errorf("%s: %v", path, err)
		}
	}
	if strings.Contains(out, token) || strings.Contains(m.calls(), token) {
		t.Error("the token was printed, or given to a command")
	}
	if read(t, m.path(oobdPlist)) != read(t, filepath.Join(repo(t), "deploy", oobdLabel+".plist")) {
		t.Error("the installed LaunchDaemon file is not deploy/'s")
	}
	// No build yet: nothing to run the controller from.
	if strings.Contains(m.calls(), "bootstrap") || m.oobdLoaded() {
		t.Errorf("the controller was started with no build Deployed:\n%s", m.calls())
	}
	if !strings.Contains(out, "the first Deploy starts it") {
		t.Errorf("the set-up doesn't say when the controller starts:\n%s", out)
	}

	// Again: the same token, nothing changed.
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("a second run changed something:\n%s", out)
	}
	if strings.TrimSpace(read(t, m.path(rootToken))) != token {
		t.Error("a second run made another token")
	}
}

func TestSetupStartsTheControllerOnceABuildIsDeployed(t *testing.T) {
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)
	m.ok("setup", "--release-key", k.signers)
	m.deployed("v0.2.0-rc.1", "#!/bin/sh\n")

	out := m.ok("setup")
	if !m.oobdLoaded() || !strings.Contains(m.calls(), "launchctl bootstrap system "+m.path(oobdPlist)) {
		t.Fatalf("the controller was not started:\n%s\n%s", out, m.calls())
	}
	if m.loaded() {
		t.Error("the set-up started labd: that is Deploy's")
	}
	os.Remove(m.log)
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") || strings.Contains(m.calls(), "bootstrap") || strings.Contains(m.calls(), "bootout") {
		t.Errorf("a run with the controller up changed something:\n%s\n%s", out, m.calls())
	}

	// labd's copy lost, or readable by others: made again from root's, and
	// the controller, whose token did not change, keeps running.
	token := read(t, m.path(rootToken))
	os.Remove(m.path(labToken))
	m.ok("setup")
	os.Chmod(m.path(labToken), 0o644)
	m.ok("setup")
	if read(t, m.path(labToken)) != token || mode(t, m.path(labToken)) != 0o600 {
		t.Error("labd's copy was not put back as root's token, its own alone")
	}
	os.Chmod(m.path(rootToken), 0o644)
	m.ok("setup")
	if mode(t, m.path(rootToken)) != 0o600 || read(t, m.path(rootToken)) != token {
		t.Error("root's token was not given back to root alone")
	}
	if strings.Contains(m.calls(), "bootout") {
		t.Errorf("the controller was restarted for nothing:\n%s", m.calls())
	}

	// A new token reaches both sides and the running controller.
	os.Remove(m.path(rootToken))
	os.Remove(m.log)
	m.ok("setup")
	fresh := read(t, m.path(rootToken))
	if fresh == token || fresh == "" || read(t, m.path(labToken)) != fresh {
		t.Error("a token made again did not reach both sides")
	}
	calls := m.calls()
	if out, in := strings.Index(calls, "launchctl bootout system/"+oobdLabel), strings.Index(calls, "launchctl bootstrap system "+m.path(oobdPlist)); out < 0 || in < out {
		t.Errorf("the controller was not started again with its new token:\n%s", calls)
	}

	// A build with no reset tool: said, and the controller still answers status.
	m.deployed("v0.2.0-rc.2", "")
	if out := m.ok("setup"); !strings.Contains(out, "no reset tool") {
		t.Errorf("a build with no reset tool was not remarked on:\n%s", out)
	}
}

// The reset tool's pin is the release workflow's: it comes in the bundle,
// and the set-up downloads and installs nothing for it.
func TestTheSetUpInstallsNothingForTheResetToolsPin(t *testing.T) {
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)
	pins := read(t, filepath.Join(repo(t), "deploy", "pins"))
	line := regexp.MustCompile(`(?m)^macvdmtool [0-9a-f]{40} [0-9a-f]{64} https://github\.com/omacom/macvdmtool$`).FindString(pins)
	if line == "" {
		t.Fatalf("deploy/pins has no line for the reset tool: its commit, the checksum of its source, the lab's fork:\n%s", pins)
	}
	// Its line alone: the file's other pins are packages the set-up would
	// download.
	write(t, m.tree+"/deploy/pins", line+"\n", 0o644)
	out := m.ok("setup", "--release-key", k.signers)
	if strings.Contains(m.calls(), "installer") || strings.Contains(out, "macvdmtool") {
		t.Errorf("the set-up acted on the reset tool's pin:\n%s\n%s", out, m.calls())
	}
}

// strictTool behaves as omacom/macvdmtool does with its arguments, on a mini
// with a Mac on RID 0 and RID 2, and writes down every run.
const strictTool = `#!/bin/sh
echo "$*" >> "$STUB_STATE/tool.args"
rid=""
if [ "$1" = --rid ]; then rid="$2"; shift 2; fi
[ -n "$rid" ] || { echo "--rid is required"; exit 255; }
case "$rid" in *[!0-9]*) echo "Invalid --rid value (expected a non-negative 32-bit integer)"; exit 255 ;; esac
case "$rid" in
  0|2) echo "Found RID $rid"; echo "Connection: Source"; echo "Status: DBMa" ;;
  1) echo "Found RID 1"; echo "Connection: None"; echo "No connection detected"; exit 255 ;;
  *) echo "No matching devices"; exit 255 ;;
esac
`

// looseTool reads a RID as the unmerged -p patch does: what is no number is 0.
const looseTool = `#!/bin/sh
echo "$*" >> "$STUB_STATE/tool.args"
echo "Found RID 0"; echo "Connection: Source"
`

const lsofStub = `#!/bin/sh
echo "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME"
echo "oobd 71 root 3u IPv4 0x0 0t0 TCP ${STUB_LISTENS:-127.0.0.1:7780} (LISTEN)"
`

// ports sets a mini up with a Deployed build, the three rear ports as they
// were read on 2026-11-14 and the controller itself, run here with the
// build's reset tool. It returns the token.
func portsMini(t *testing.T, tool string) (*mini, string) {
	t.Helper()
	return portsMiniOf(t, tool, "ioreg-applehpm.txt")
}

// portsMiniOf is portsMini with the ports of another ioreg capture.
func portsMiniOf(t *testing.T, tool, ioreg string) (*mini, string) {
	t.Helper()
	need(t, "openssl", "awk")
	m := newMini(t)
	k := newKey(t)
	m.ok("setup", "--release-key", k.signers)
	// The controller gives the tool no environment, so the made-up tool is
	// told in its own text where to write its runs down.
	m.deployed("v0.2.0-rc.1", strings.ReplaceAll(tool, "$STUB_STATE", filepath.Join(filepath.Dir(m.root), "state")))
	m.ok("setup")
	data := filepath.Join(repo(t), "internal", "oobd", "testdata")
	write(t, m.bin+"/ioreg", "#!/bin/sh\ncat "+data+"/"+ioreg+"\n", 0o755)
	write(t, m.bin+"/system_profiler", "#!/bin/sh\ncat "+data+"/thunderbolt.json\n", 0o755)
	write(t, m.bin+"/lsof", lsofStub, 0o755)
	token, err := oob.ReadToken(m.path(rootToken))
	if err != nil {
		t.Fatal(err)
	}
	s, err := oobd.New(oobd.Config{
		Token: token,
		Tool:  oobd.ExecTool{Path: m.path("usr/local/lab/current/bin/macvdmtool")},
		Ports: oobd.SystemPorts{Ioreg: m.bin + "/ioreg", Profiler: m.bin + "/system_profiler"},
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	m.env = append(m.env, "LAB_OOBD_LOOPBACK="+strings.TrimPrefix(srv.URL, "http://"))
	os.Remove(m.log)
	return m, token
}

func (m *mini) toolRuns() []string {
	text := strings.TrimSpace(read(m.t, filepath.Join(filepath.Dir(m.root), "state", "tool.args")))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestThePortCheckAsksEveryPortAndResetsNothing(t *testing.T) {
	m, token := portsMini(t, strictTool)

	out := m.ok("ports")

	for _, want := range []string{
		"RID 0 (Port-USB-C@1): connected, transports CC,USB2, product id 29453, plug count 42, connection id 5A0E1B7C-0000-4000-8000-00000000A001",
		"RID 1 (Port-USB-C@2): nothing connected, plug count 27",
		"RID 2 (Port-USB-C@3): connected, transports CC,USB2,CIO, product id 29448, plug count 40",
		`Connection: Source`,
		"no RID: --rid is required",
		"a RID that is not a number: Invalid --rid value",
		"a negative RID: Invalid --rid value",
		"a RID with no such port (3): No matching devices",
		oobdLabel + " is loaded",
		`"name":"omarchy-m1-pro"`,
		`"product_id":29448`,
		`"plug_count":27`,
		"RID 3 is refused",
		"oobd listens on this machine only: 127.0.0.1:7780",
		"every check passed; nothing was reset",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the port check doesn't say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a check failed:\n%s", out)
	}
	// While the controller runs it alone gives the tool a port, so that the
	// tool never runs twice at once: three ports through the controller, and
	// by the script the four bad RIDs, which reach no port.
	if strings.Contains(out, "the tool answers") || strings.Contains(out, "the tool says") {
		t.Errorf("the script ran the tool on a port beside the controller:\n%s", out)
	}
	runs := m.toolRuns()
	if len(runs) != 7 {
		t.Errorf("the tool ran %d times, want 7:\n%s", len(runs), strings.Join(runs, "\n"))
	}
	for _, run := range runs {
		if !strings.HasSuffix(run, "nop") || strings.Contains(run, "reboot") {
			t.Errorf("the port check ran the tool with %q", run)
		}
	}
	if strings.Contains(out, token) || strings.Contains(m.calls(), token) || strings.Contains(strings.Join(runs, "\n"), token) {
		t.Error("the token was printed, or given to a command")
	}
}

func TestThePortCheckFailsWhenABadRIDIsNotRefused(t *testing.T) {
	m, _ := portsMini(t, looseTool)
	out := m.refused("ports")
	for _, want := range []string{"FAIL  no RID was not refused", "FAIL  a RID that is not a number was not refused", "FAIL  a negative RID was not refused", "checks failed; nothing was reset"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	for _, run := range m.toolRuns() {
		if !strings.HasSuffix(run, "nop") {
			t.Errorf("the port check ran the tool with %q", run)
		}
	}
}

// A tool that fails on a connected port is a failed check, though the
// controller answers 200 with what it saw of the port.
func TestThePortCheckFailsWhenTheToolFailsThroughTheController(t *testing.T) {
	m, _ := portsMini(t, strings.Replace(strictTool, `0|2) echo`, `2) echo "IOKit says no"; exit 1 ;;
  0) echo`, 1))
	out := m.refused("ports")
	if !strings.Contains(out, "FAIL  RID 2: the tool failed on a connected port") || !strings.Contains(out, "IOKit says no") {
		t.Errorf("a tool that fails through the controller passed:\n%s", out)
	}
	if strings.Contains(out, "FAIL  RID 1") || strings.Contains(out, "FAIL  RID 0") {
		t.Errorf("a port with nothing on it, or a good one, failed the check:\n%s", out)
	}
}

// The mini on 2026-11-16: the owner's display on RID 1, connected on CC
// alone, where the tool says "No connection detected". It is no Mac and is
// left alone, through the controller and without it; a port that carries
// USB2 or CIO and gets that answer still fails.
func TestThePortCheckLeavesADisplayAlone(t *testing.T) {
	m, _ := portsMiniOf(t, strictTool, "ioreg-applehpm-display.txt")
	display := "ok    RID 1: not a Mac (a display or power on CC only), left alone"
	out := m.ok("ports")
	for _, want := range []string{
		"ok    RID 1 (Port-USB-C@2): connected, transports CC, product id none, plug count 2",
		display,
		"every check passed; nothing was reset",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the port check doesn't say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a display failed the check:\n%s", out)
	}

	os.Remove(filepath.Join(filepath.Dir(m.root), "state", "loaded."+oobdLabel))
	out = m.refused("ports")
	if !strings.Contains(out, display) || strings.Contains(out, "FAIL  RID 1") {
		t.Errorf("without the controller a display failed the check:\n%s", out)
	}

	m, _ = portsMiniOf(t, strings.Replace(strictTool, `0|2) echo`, `2) echo "Found RID 2"; echo "Connection: None"; echo "No connection detected"; exit 255 ;;
  0) echo`, 1), "ioreg-applehpm-display.txt")
	out = m.refused("ports")
	if !strings.Contains(out, "FAIL  RID 2: the tool failed on a connected port") || strings.Contains(out, "FAIL  RID 1") || strings.Contains(out, "RID 2: not a Mac") {
		t.Errorf("a port on USB2 and CIO the tool finds no connection on passed, or the display failed:\n%s", out)
	}
	os.Remove(filepath.Join(filepath.Dir(m.root), "state", "loaded."+oobdLabel))
	out = m.refused("ports")
	if !strings.Contains(out, "FAIL  RID 2: the tool failed on a connected port (exit") || strings.Contains(out, "FAIL  RID 1") {
		t.Errorf("without the controller, a port on USB2 and CIO the tool finds no connection on passed:\n%s", out)
	}
}

func TestThePortCheckFailsWhenTheControllerIsStoppedOrListensWider(t *testing.T) {
	m, _ := portsMini(t, strictTool)
	m.env = append(m.env, "STUB_LISTENS=*:7780")
	if out := m.refused("ports"); !strings.Contains(out, "FAIL  oobd listens beyond this machine: *:7780") {
		t.Errorf("a controller on every interface passed:\n%s", out)
	}

	os.Remove(filepath.Join(filepath.Dir(m.root), "state", "loaded."+oobdLabel))
	if out := m.refused("ports"); !strings.Contains(out, "FAIL  "+oobdLabel+" is not loaded") || !strings.Contains(out, "RID 2: the tool answers") {
		t.Errorf("a stopped controller passed, or the tool was not checked:\n%s", out)
	}
}

func TestThePortCheckNeedsTheResetTool(t *testing.T) {
	m, _ := portsMini(t, "")
	if out := m.refused("ports"); !strings.Contains(out, "no reset tool at") {
		t.Errorf("%s", out)
	}
	if out := m.refused("ports", "--tool"); !strings.Contains(out, "sudo scripts/mini/ports") {
		t.Errorf("%s", out)
	}
}
