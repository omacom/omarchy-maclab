package minitest

import (
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	signalLabel = "org.omarchy-mac.lab.signal-cli"
	signalPlist = "Library/LaunchDaemons/" + signalLabel + ".plist"
	signalCLI   = "usr/local/lab/tools/current/signal-cli/bin/signal-cli"
	signalData  = "var/lab/secrets/signal-cli"
	groupID     = "bGFiLWdyb3VwLWlkLWxhYi1ncm91cC1pZC1sYWItZ3I="
)

// The sender is signal-cli as the lab account, from the copy of the Homebrew
// tools only root can write, on a socket of the lab account, with no number,
// no group and no secret in its file, and nothing it receives written down.
func TestTheSendersLaunchDaemon(t *testing.T) {
	file := filepath.Join(repo(t), "deploy", signalLabel+".plist")
	values, arrays := plist(t, file)
	for key, want := range map[string]string{
		"Label":     shell(t, "SIGNAL_LABEL"),
		"UserName":  "lab",
		"GroupName": "lab",
		"RunAtLoad": "true",
		"KeepAlive": "true",
		"Umask":     "63",
	} {
		if values[key] != want {
			t.Errorf("%s is %q, want %q", key, values[key], want)
		}
	}
	if shell(t, "SIGNAL_LABEL") != signalLabel {
		t.Errorf("SIGNAL_LABEL is %q", shell(t, "SIGNAL_LABEL"))
	}
	args := arrays["ProgramArguments"]
	want := []string{
		"/" + signalCLI,
		"--data-dir", "/" + signalData,
		"--scrub-log",
		"daemon",
		"--socket", "/var/lab/run/signal-cli.sock",
		"--receive-mode", "on-start",
		"--no-receive-stdout",
		"--ignore-attachments", "--ignore-stories", "--ignore-avatars", "--ignore-stickers",
	}
	if !slices.Equal(args, want) {
		t.Errorf("signal-cli is started as %v, want %v", args, want)
	}
	text := read(t, file)
	for _, word := range []string{"/opt/homebrew", "EnvironmentVariables", "TOKEN", "SECRET", "PASSWORD", "<string>-a</string>", "--account", "--tcp", "--http", "--dbus", "--send-read-receipts"} {
		if strings.Contains(text, word) {
			t.Errorf("the sender's LaunchDaemon file holds %s", word)
		}
	}
	if regexp.MustCompile(`\+\d{6,}`).MatchString(text) {
		t.Error("the sender's LaunchDaemon file holds a phone number")
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.HasPrefix(values[key], "/var/lab/log/") {
			t.Errorf("%s is %q, outside the lab account's log folder", key, values[key])
		}
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("plutil", "-lint", file).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v\n%s", err, out)
		}
	}

	// The mini's scripts and labd name the same places.
	lib := read(t, filepath.Join(repo(t), "scripts", "mini", "lib.sh"))
	for _, line := range []string{
		`SIGNAL_CLI="$LAB_TOOLS/current/signal-cli/bin/signal-cli"`,
		`SIGNAL_DATA="$LAB_HOME/secrets/signal-cli"`,
		`SIGNAL_SOCKET="$LAB_HOME/run/signal-cli.sock"`,
	} {
		if !strings.Contains(lib, line) {
			t.Errorf("scripts/mini/lib.sh lacks %s", line)
		}
	}
	_, labd := plist(t, filepath.Join(repo(t), "deploy", labdLabel+".plist"))
	flags := strings.Join(labd["ProgramArguments"], " ")
	for _, want := range []string{"--signal-socket /var/lab/run/signal-cli.sock", "--signal /var/lab/secrets"} {
		if !strings.Contains(flags, want) {
			t.Errorf("labd is not started with %s", want)
		}
	}
	if !regexp.MustCompile(`(?m)^signal-cli\s*$`).MatchString(read(t, filepath.Join(repo(t), "deploy", "homebrew-formulae"))) {
		t.Error("deploy/homebrew-formulae does not name signal-cli")
	}
	// The files the owner's script writes are the ones labd reads.
	server := read(t, filepath.Join(repo(t), "internal", "server", "lines.go"))
	script := read(t, filepath.Join(repo(t), "scripts", "mini", "signal"))
	for _, name := range []string{"signal-lab-group", "signal-agents-group"} {
		if !strings.Contains(server, `"`+name+`"`) || !strings.Contains(script, "file="+name) {
			t.Errorf("labd and scripts/mini/signal do not both name %s", name)
		}
	}
}

// withSender has the made-up machine's set-up install the sender.
func (m *mini) withSender() {
	m.t.Helper()
	write(m.t, m.tree+"/deploy/homebrew-formulae", "signal-cli\n", 0o644)
}

func (m *mini) senderLoaded() bool {
	return exists(filepath.Join(filepath.Dir(m.root), "state", "loaded."+signalLabel))
}

func TestSetupStartsTheSenderFromTheCopy(t *testing.T) {
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)
	m.withSender()

	dry := m.ok("setup", "--dry-run", "--release-key", k.signers)
	for _, want := range []string{"install signal-cli from Homebrew", m.path(signalPlist), "start " + signalLabel} {
		if !strings.Contains(dry, want) {
			t.Errorf("the dry run doesn't mention %q:\n%s", want, dry)
		}
	}
	if exists(m.path(signalPlist)) || exists(m.path(signalData)) || m.senderLoaded() {
		t.Fatal("a dry run installed or started the sender")
	}

	out := m.ok("setup", "--release-key", k.signers)
	if read(t, m.path(signalPlist)) != read(t, filepath.Join(repo(t), "deploy", signalLabel+".plist")) || mode(t, m.path(signalPlist)) != 0o644 {
		t.Error("the installed LaunchDaemon file is not deploy/'s, or not 644")
	}
	if got := mode(t, m.path(signalData)); got != 0o700 {
		t.Errorf("the account's folder is %v, want 700", got)
	}
	if !exists(m.path(signalCLI)) {
		t.Fatal("signal-cli is not in the copy of the tools")
	}
	if !m.senderLoaded() || !strings.Contains(m.calls(), "launchctl bootstrap system "+m.path(signalPlist)) {
		t.Fatalf("the sender was not started:\n%s\n%s", out, m.calls())
	}
	if !strings.Contains(out, "scripts/mini/signal link") {
		t.Errorf("the set-up doesn't say the sender has no number yet:\n%s", out)
	}
	if strings.Contains(m.calls(), "brew services") || m.loaded() {
		t.Errorf("brew started a service, or the set-up started labd:\n%s", m.calls())
	}

	os.Remove(m.log)
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") || strings.Contains(m.calls(), "bootstrap") || strings.Contains(m.calls(), "bootout") {
		t.Errorf("a run with the sender up changed something:\n%s\n%s", out, m.calls())
	}

	// A Maintenance run's newer signal-cli: copied afresh, and the sender
	// started again so that it runs the new copy.
	m.upgrade("signal-cli", "2.0")
	os.Remove(m.log)
	m.ok("setup")
	calls := m.calls()
	if out, in := strings.Index(calls, "launchctl bootout system/"+signalLabel), strings.Index(calls, "launchctl bootstrap system "+m.path(signalPlist)); out < 0 || in < out {
		t.Errorf("the sender was not started again from the new copy:\n%s", calls)
	}
	if got := read(t, m.path(signalCLI)); !strings.Contains(got, "signal-cli 2.0") {
		t.Errorf("the copy in use: %q", got)
	}
}

// signalStub plays signal-cli run by hand: it writes down its arguments,
// "receives" what a member wrote, and lists one group.
const signalStub = `#!/bin/bash
echo "signal-cli $*" >> "$STUB_LOG"
case " $* " in
  *" receive "*) echo "Envelope from: +61491570157 Body: the launch code is tangerine" ;;
  *" listGroups "*) echo "Id: ` + groupID + ` Name: Omarchy Mac Lab  Active: true Blocked: false" ;;
esac
`

// signal starts scripts/mini/signal and types answers at its prompts.
func (m *mini) signal(answers string, args ...string) (string, int) {
	m.t.Helper()
	cmd := exec.Command(filepath.Join(m.tree, "scripts", "mini", "signal"), args...)
	cmd.Env = m.env
	cmd.Dir = filepath.Dir(m.tree)
	cmd.Stdin = strings.NewReader(answers)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	if err != nil {
		m.t.Fatalf("signal: %v\n%s", err, out)
	}
	return string(out), 0
}

func TestTheOwnerRegistersTheSenderAndGivesLabdItsGroups(t *testing.T) {
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)
	m.withSender()

	if out, code := m.signal("", "groups"); code == 0 || !strings.Contains(out, "scripts/mini/setup") {
		t.Fatalf("before the set-up: exit %d\n%s", code, out)
	}
	m.ok("setup", "--release-key", k.signers)
	os.Chmod(filepath.Dir(m.path(signalCLI)), 0o755)
	write(t, m.path(signalCLI), signalStub, 0o755)
	data := m.path(signalData)

	// A dry run asks nothing and runs nothing.
	os.Remove(m.log)
	for _, verb := range [][]string{{"register"}, {"groups"}, {"accept", groupID}, {"test"}, {"group", "lab", groupID}} {
		if out, code := m.signal("", append([]string{"--dry-run"}, verb...)...); code != 0 || !strings.Contains(out, "would") {
			t.Errorf("--dry-run %v: exit %d\n%s", verb, code, out)
		}
	}
	if exists(m.log) || exists(m.path("var/lab/secrets/signal-lab-group")) {
		t.Fatalf("a dry run called something or wrote a group id:\n%s", m.callsOrNone())
	}

	// What is no number, link or code is refused before signal-cli runs.
	for _, bad := range []string{"0491570156\n", "+61491570156\nhttps://example.org/\n"} {
		if out, code := m.signal(bad, "register"); code == 0 {
			t.Errorf("answers %q were taken:\n%s", bad, out)
		}
	}
	if strings.Contains(m.callsOrNone(), "signal-cli") {
		t.Fatalf("signal-cli ran on refused answers:\n%s", m.calls())
	}

	out, code := m.signal("+61491570156\nsignalcaptcha://signal-hcaptcha.abc.registration.def\n123-456\n", "register")
	if code != 0 {
		t.Fatalf("register: exit %d\n%s", code, out)
	}
	calls := m.calls()
	var order []int
	for _, want := range []string{
		"launchctl bootout system/" + signalLabel,
		"signal-cli --data-dir " + data + " -a +61491570156 register --captcha signalcaptcha://signal-hcaptcha.abc.registration.def",
		"signal-cli --data-dir " + data + " -a +61491570156 verify 123456",
		"signal-cli --data-dir " + data + " -a +61491570156 updateProfile --given-name Omarchy Mac Lab",
		"launchctl bootstrap system " + m.path(signalPlist),
	} {
		i := strings.Index(calls, want)
		if i < 0 {
			t.Fatalf("register did not run %q:\n%s", want, calls)
		}
		order = append(order, i)
	}
	if !slices.IsSorted(order) || !m.senderLoaded() {
		t.Errorf("register ran out of order, or left the sender stopped:\n%s", calls)
	}

	// The groups are listed; what members wrote is fetched and not shown.
	out, code = m.signal("", "groups")
	if code != 0 || !strings.Contains(out, "Id: "+groupID+" Name: Omarchy Mac Lab") {
		t.Fatalf("groups: exit %d\n%s", code, out)
	}
	if strings.Contains(out, "tangerine") || strings.Contains(out, "+61491570157") {
		t.Errorf("groups showed what a member wrote:\n%s", out)
	}
	if !strings.Contains(m.calls(), "signal-cli --data-dir "+data+" receive ") || !m.senderLoaded() {
		t.Errorf("groups did not fetch what waited, or left the sender stopped:\n%s", m.calls())
	}

	// labd is given each group's id in a file of the lab account alone.
	for group, file := range map[string]string{"lab": "signal-lab-group", "agents": "signal-agents-group"} {
		if out, code := m.signal("", "group", group, groupID); code != 0 {
			t.Fatalf("group %s: exit %d\n%s", group, code, out)
		}
		path := m.path("var/lab/secrets/" + file)
		if read(t, path) != groupID+"\n" || mode(t, path) != 0o600 {
			t.Errorf("%s holds %q with mode %v", file, read(t, path), mode(t, path))
		}
	}
	for _, bad := range [][]string{{"group", "lab", "not-an-id"}, {"group", "team", groupID}, {"accept", "x"}} {
		if out, code := m.signal("", bad...); code == 0 {
			t.Errorf("%v was taken:\n%s", bad, out)
		}
	}
	if out, code := m.signal("", "accept", groupID); code != 0 || !strings.Contains(m.calls(), "signal-cli --data-dir "+data+" updateGroup -g "+groupID) {
		t.Errorf("accept: exit %d\n%s", code, out)
	}

	// The test line is labd's own sender's, told where the socket and the ids are.
	if out, code := m.signal("", "test"); code == 0 || !strings.Contains(out, "no build is Deployed") {
		t.Errorf("test with no build: exit %d\n%s", code, out)
	}
	m.deployed("v0.2.0-rc.1", "")
	write(t, m.path("usr/local/lab/current/bin/labd"), "#!/bin/bash\necho \"labd $*\" >> \"$STUB_LOG\"\necho '\"Agents\": sent'\n", 0o755)
	out, code = m.signal("", "test")
	if code != 0 || !strings.Contains(out, `"Agents": sent`) {
		t.Fatalf("test: exit %d\n%s", code, out)
	}
	if want := "labd --signal-socket " + m.path("var/lab/run/signal-cli.sock") + " --signal " + m.path("var/lab/secrets") + " --signal-test"; !strings.Contains(m.calls(), want) {
		t.Errorf("labd was not asked for the test line as %q:\n%s", want, m.calls())
	}
}

// The daemon's socket comes back a moment after launchd starts it again: a
// command that restarted it returns once the socket answers, so that the
// test line and labd's own lines that follow reach it; or says it does not.
func TestARestartedSenderIsWaitedForUntilItsSocketAnswers(t *testing.T) {
	need(t, "openssl", "nc")
	m := newMini(t)
	k := newKey(t)
	m.withSender()
	m.ok("setup", "--release-key", k.signers)
	os.Chmod(filepath.Dir(m.path(signalCLI)), 0o755)
	write(t, m.path(signalCLI), signalStub, 0o755)
	sock := m.path("var/lab/run/signal-cli.sock")
	started := func() int { return strings.Count(m.callsOrNone(), "launchctl bootstrap system "+m.path(signalPlist)) }
	before := started()
	done := make(chan net.Listener, 1)
	go func() {
		for deadline := time.Now().Add(20 * time.Second); started() == before && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(1500 * time.Millisecond)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Error(err)
		}
		done <- ln
	}()
	m.env = append(m.env, "LAB_SENDER_WAIT=15")

	for _, verb := range []string{"groups", "accept"} {
		args := []string{verb}
		if verb == "accept" {
			args = append(args, groupID)
		}
		out, code := m.signal("", args...)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", verb, code, out)
		}
		if verb == "groups" {
			c, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatalf("groups returned before the sender's socket answered: %v\n%s", err, out)
			}
			c.Close()
			if strings.Contains(out, "does not answer") {
				t.Errorf("groups said the socket does not answer:\n%s", out)
			}
			(<-done).Close()
			os.Remove(sock)
			m.env = append(m.env, "LAB_SENDER_WAIT=1")
			continue
		}
		if !strings.Contains(out, "its socket "+sock+" does not answer") {
			t.Errorf("accept did not say the socket does not answer:\n%s", out)
		}
	}
}

// linkStub plays signal-cli link: the link for the phone to scan, then, once
// scanned, the number it was linked to.
const linkStub = `#!/bin/bash
echo "signal-cli $*" >> "$STUB_LOG"
case " $* " in
  *" link "*)
    key=dGVzdC1wdWJsaWMta2V5 # apart, so that this file does not hold the link
    echo "sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=$key"
    echo "Associated with: +61491570156" ;;
esac
`

const linkURL = "sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=dGVzdC1wdWJsaWMta2V5"

// labdQR plays the Deployed labd's qr: it reads one line and draws, in place
// of a QR code, the line's SHA-256, so that a test sees which line it drew
// without the line itself reaching any output.
const labdQR = `#!/bin/bash
echo "labd $*" >> "$STUB_LOG"
[[ $1 == qr ]] || exit 9
IFS= read -r line
printf '%s' "$line" | openssl dgst -sha256 -r | sed 's/ .*//; s/^/[QR /; s/$/]/'
`

// The owner links the sender to his own number: the link is shown only as a
// QR code, drawn by the Deployed labd from its input, to scan in Signal on his
// phone. It is never on the terminal as text, traced, an argument of anything
// nor written down, and with no labd to draw it nothing is stopped.
func TestTheOwnerLinksTheSenderToHisOwnNumber(t *testing.T) {
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)
	m.withSender()
	m.terminal("24 80")

	if out, code := m.signal("", "link"); code == 0 || !strings.Contains(out, "scripts/mini/setup") {
		t.Fatalf("before the set-up: exit %d\n%s", code, out)
	}
	m.ok("setup", "--release-key", k.signers)
	os.Chmod(filepath.Dir(m.path(signalCLI)), 0o755)
	write(t, m.path(signalCLI), linkStub, 0o755)
	data := m.path(signalData)

	os.Remove(m.log)
	if out, code := m.signal("", "--dry-run", "link"); code != 0 || !strings.Contains(out, "would") || !strings.Contains(out, "labd") || strings.Contains(out, "qrencode") || exists(m.log) {
		t.Fatalf("--dry-run link: exit %d\n%s\n%s", code, out, m.callsOrNone())
	}

	// No Deployed labd to draw the code: refused before the sender is stopped,
	// and no link is made. Homebrew's qrencode is no way round it.
	write(t, m.path("opt/homebrew/bin/qrencode"), "#!/bin/bash\necho \"qrencode $*\" >> \"$STUB_LOG\"\ncat\n", 0o755)
	if out, code := m.signal("", "link"); code == 0 || !strings.Contains(out, "Deploy") || strings.Contains(out, "sgnl://") || exists(m.log) || !m.senderLoaded() {
		t.Fatalf("link with no labd: exit %d, sender up %v\n%s\n%s", code, m.senderLoaded(), out, m.callsOrNone())
	}

	m.deployed("v0.2.0-rc.1", "")
	write(t, m.path("usr/local/lab/current/VERSION"), "v0.2.0-rc.1\n", 0o644)
	labd := m.path("usr/local/lab/current/bin/labd")
	write(t, labd, labdQR, 0o755)
	drawn := fmt.Sprintf("[QR %x]", sha256.Sum256([]byte(linkURL)))
	secret := linkURL[strings.Index(linkURL, "pub_key="):]

	run := func(args ...string) (string, string, error) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = m.env
		cmd.Dir = filepath.Dir(m.tree)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		return stdout.String(), stderr.String(), err
	}
	script := filepath.Join(m.tree, "scripts", "mini", "signal")
	stdout, stderr, err := run(script, "link")
	if err != nil || !strings.Contains(stdout+stderr, drawn) || !strings.Contains(stdout, "Associated with") || !strings.Contains(stderr, "Link a new device") {
		t.Fatalf("link: %v\nstdout:\n%s\nstderr:\n%s\n%s", err, stdout, stderr, m.callsOrNone())
	}
	for name, text := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if strings.Contains(text, "sgnl://") || strings.Contains(text, secret) {
			t.Errorf("the link is on %s as text:\n%s", name, text)
		}
	}
	calls := m.calls()
	var order []int
	for _, want := range []string{
		"launchctl bootout system/" + signalLabel,
		"as-lab " + m.path(signalCLI) + " --data-dir " + data + " link -n Omarchy Mac Lab",
		"as-lab " + labd + " qr\n",
		"launchctl bootstrap system " + m.path(signalPlist),
	} {
		i := strings.Index(calls, want)
		if i < 0 {
			t.Fatalf("link did not run %q:\n%s", want, calls)
		}
		order = append(order, i)
	}
	if !slices.IsSorted(order[:2]) || order[1] > order[3] || order[2] > order[3] || !m.senderLoaded() {
		t.Errorf("link ran out of order, or left the sender stopped:\n%s", calls)
	}
	if strings.Contains(calls, "updateProfile") || strings.Contains(calls, " -a ") {
		t.Errorf("link changed the owner's profile or named an account:\n%s", calls)
	}
	if strings.Contains(calls, "sgnl://") || strings.Contains(calls, "qrencode") {
		t.Errorf("the link was an argument of a command, or a qrencode ran:\n%s", calls)
	}
	m.nothingHolds(linkURL)

	// Run under bash -x, the trace shows no link either.
	cmd := exec.Command("bash", "-x", script, "link")
	cmd.Env = m.env
	traced, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(traced), drawn) {
		t.Fatalf("link under bash -x: %v\n%s", err, traced)
	}
	if strings.Contains(string(traced), secret) {
		t.Errorf("the trace shows the link:\n%s", traced)
	}
	m.nothingHolds(linkURL)

	// labd failing to draw it: the link is still not shown, and the sender is
	// started again.
	write(t, labd, "#!/bin/bash\ncat >/dev/null\nexit 1\n", 0o755)
	stdout, stderr, err = run(script, "link")
	if err == nil || !strings.Contains(stderr, "did not draw") || strings.Contains(stdout+stderr, "sgnl://") || !m.senderLoaded() {
		t.Errorf("labd failing to draw: %v, sender up %v\nstdout:\n%s\nstderr:\n%s", err, m.senderLoaded(), stdout, stderr)
	}

	// signal-cli failing is a failure, and the sender is started again.
	write(t, labd, labdQR, 0o755)
	write(t, m.path(signalCLI), "#!/bin/bash\necho \"signal-cli $*\" >> \"$STUB_LOG\"\necho 'Link request error: Connection closed!' >&2\nexit 3\n", 0o755)
	if out, code := m.signal("", "link"); code == 0 || !strings.Contains(out, "did not link") || !m.senderLoaded() {
		t.Errorf("a failed link: exit %d, sender up %v\n%s", code, m.senderLoaded(), out)
	}
}

// nothingHolds fails the test when any file under the test's folders holds
// secret.
func (m *mini) nothingHolds(secret string) {
	m.t.Helper()
	filepath.Walk(filepath.Dir(m.root), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.Contains(read(m.t, p), secret) {
			m.t.Errorf("%s holds %q", p, secret)
		}
		return nil
	})
}

// linkReady is a mini set up with the sender running and a build Deployed
// whose labd draws, at version, for link.
func linkReady(t *testing.T, version string) (*mini, string) {
	t.Helper()
	need(t, "openssl")
	m := newMini(t)
	k := newKey(t)
	m.withSender()
	m.ok("setup", "--release-key", k.signers)
	os.Chmod(filepath.Dir(m.path(signalCLI)), 0o755)
	write(t, m.path(signalCLI), linkStub, 0o755)
	m.deployed(version, "")
	write(t, m.path("usr/local/lab/current/VERSION"), version+"\n", 0o644)
	labd := m.path("usr/local/lab/current/bin/labd")
	write(t, labd, labdQR, 0o755)
	m.terminal("24 80")
	return m, labd
}

// terminal plays stty for the owner's terminal: its size as rows and
// columns, or "" for no terminal at all.
func (m *mini) terminal(size string) {
	m.t.Helper()
	answer := "exit 1"
	if size != "" {
		answer = "echo " + size
	}
	write(m.t, m.bin+"/stty", "#!/bin/bash\n[[ \"$*\" == '-f /dev/tty size' ]] || exit 1\n"+answer+"\n", 0o755)
}

// A Deployed labd from before labd qr (v0.2.0-rc.0 and older) starts a lab
// given qr: link refuses before the sender is stopped, runs no labd, and says
// which build to Deploy.
func TestLinkRefusesALabdThatCannotDraw(t *testing.T) {
	m, labd := linkReady(t, "v0.2.0-rc.0")
	write(t, labd, "#!/bin/bash\necho \"labd $*\" >> \"$STUB_LOG\"\necho 'a lab starts'\nsleep 5\n", 0o755)
	version := m.path("usr/local/lab/current/VERSION")
	for _, v := range []string{"v0.2.0-rc.0", "v0.1.15", "dev", ""} {
		if v == "" {
			os.Remove(version)
		} else {
			write(t, version, v+"\n", 0o644)
		}
		os.Remove(m.log)
		out, code := m.signal("", "link")
		if code == 0 || !strings.Contains(out, "e61152eb") || !strings.Contains(out, "Nothing was changed") || exists(m.log) || !m.senderLoaded() {
			t.Errorf("link over %q: exit %d, sender up %v\n%s\n%s", v, code, m.senderLoaded(), out, m.callsOrNone())
		}
	}
}

// A device link's code is 23 lines of 45 columns: in a terminal with fewer
// than 24 rows (the line above it too) or 45 columns, or with no terminal,
// link refuses before the sender is stopped and says what to do.
func TestLinkRefusesATerminalTheCodeDoesNotFit(t *testing.T) {
	m, _ := linkReady(t, "v0.2.0-rc.1")
	for size, want := range map[string]string{"23 80": "taller", "40 44": "wider", "": "terminal"} {
		m.terminal(size)
		os.Remove(m.log)
		out, code := m.signal("", "link")
		if code == 0 || !strings.Contains(out, want) || !strings.Contains(out, "Nothing was changed") || exists(m.log) || !m.senderLoaded() {
			t.Errorf("link in a terminal of %q: exit %d, sender up %v\n%s\n%s", size, code, m.senderLoaded(), out, m.callsOrNone())
		}
	}
	m.terminal("24 45")
	if out, code := m.signal("", "link"); code != 0 {
		t.Errorf("link in 24 rows of 45 columns: exit %d\n%s", code, out)
	}
}

// A code labd fails to draw ends signal-cli link, which would wait for a
// scan with the sender stopped until Ctrl-C, and link fails; the sender is
// started again.
func TestLinkEndsSignalCLIWhenTheCodeIsNotDrawn(t *testing.T) {
	m, labd := linkReady(t, "v0.2.0-rc.1")
	pid := filepath.Join(filepath.Dir(m.root), "state", "signal-cli.pid")
	write(t, m.path(signalCLI), `#!/bin/bash
echo "signal-cli $*" >> "$STUB_LOG"
echo $$ > "$STUB_STATE/signal-cli.pid"
key=dGVzdC1wdWJsaWMta2V5
echo "sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=$key"
exec sleep 60
`, 0o755)
	write(t, labd, "#!/bin/bash\ncat >/dev/null\nexit 1\n", 0o755)
	start := time.Now()
	out, code := m.signal("", "link")
	if code == 0 || !strings.Contains(out, "did not draw") || strings.Contains(out, "sgnl://") || strings.Contains(out, "linked as") || !m.senderLoaded() {
		t.Errorf("a failed draw: exit %d, sender up %v\n%s\n%s", code, m.senderLoaded(), out, m.callsOrNone())
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("link waited %v for signal-cli after the draw failed", time.Since(start))
	}
	n, err := strconv.Atoi(strings.TrimSpace(read(t, pid)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; syscall.Kill(n, 0) == nil; i++ {
		if i == 50 {
			t.Fatalf("signal-cli link (%d) still runs after link failed", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A link signal-cli prints after other words, or on its stderr, is drawn
// and never shown as text either.
func TestLinkDrawsALinkPrefixedOrOnStderr(t *testing.T) {
	m, _ := linkReady(t, "v0.2.0-rc.1")
	drawn := fmt.Sprintf("[QR %x]", sha256.Sum256([]byte(linkURL)))
	secret := linkURL[strings.Index(linkURL, "pub_key="):]
	for name, print := range map[string]string{
		"prefixed":  `echo "Link this device: sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=$key"`,
		"on stderr": `echo "sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=$key" >&2`,
		"both":      `echo "INFO LinkCommand - sgnl://linkdevice?uuid=dGVzdC11dWlk&pub_key=$key" >&2`,
	} {
		write(t, m.path(signalCLI), "#!/bin/bash\necho \"signal-cli $*\" >> \"$STUB_LOG\"\nkey=dGVzdC1wdWJsaWMta2V5\n"+print+"\necho 'Associated with: +61491570156'\n", 0o755)
		out, code := m.signal("", "link")
		if code != 0 || !strings.Contains(out, drawn) || !strings.Contains(out, "Associated with") {
			t.Errorf("a link %s: exit %d\n%s", name, code, out)
		}
		if strings.Contains(out, "sgnl://") || strings.Contains(out, secret) {
			t.Errorf("a link %s is shown as text:\n%s", name, out)
		}
	}
	m.nothingHolds(linkURL)
}
