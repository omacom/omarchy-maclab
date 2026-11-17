package minitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	tailscaledLabel = "org.omarchy-mac.lab.tailscaled"
	sshdKeysOnly    = "etc/ssh/sshd_config.d/000-lab.conf"
	goodTailnetKey  = "tskey-auth-goodMadeUpKey11CNTRL-notASecretOnlyATest00"
)

// What follows plays the parts of macOS that no folder can stand in for.
// Each keeps its state in $STUB_STATE/machine and starts as the mini was
// read on 2026-11-13: Ethernet and Wi-Fi both up, updates installing by
// themselves, autorestart 1, sleep 0, a stock sshd.

const networksetupStub = `#!/bin/bash
echo "networksetup $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"; mkdir -p "$s"
case "$1" in
  -listallhardwareports)
    [[ -f $s/no-wifi ]] || printf '\nHardware Port: Wi-Fi\nDevice: en1\nEthernet Address: 00:00:00:00:00:02\n'
    printf '\nHardware Port: Ethernet\nDevice: en0\nEthernet Address: 00:00:00:00:00:01\n\nHardware Port: Thunderbolt Bridge\nDevice: bridge0\nEthernet Address: 00:00:00:00:00:03\n' ;;
  -getairportpower) echo "Wi-Fi Power ($2): $(cat "$s/wifi" 2>/dev/null || echo On)" ;;
  -setairportpower) if [[ $3 == off ]]; then echo Off > "$s/wifi"; else echo On > "$s/wifi"; fi ;;
  *) echo "networksetup $1: not expected" >&2; exit 64 ;;
esac
`

const ipconfigStub = `#!/bin/bash
s="$STUB_STATE/machine"
[[ $1 == getifaddr ]] || exit 64
case "$2" in
  en0) [[ ! -f $s/unplugged ]] || exit 1; echo 192.168.1.20 ;;
  en1) [[ "$(cat "$s/wifi" 2>/dev/null || echo On)" == On ]] || exit 1; echo 192.168.0.8 ;;
  bridge0) echo 169.254.1.1 ;;
  *) exit 1 ;;
esac
`

const defaultsStub = `#!/bin/bash
echo "defaults $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"; mkdir -p "$s"
[[ $2 == "$LAB_SYSROOT/Library/Preferences/com.apple.SoftwareUpdate" ]] || { echo "defaults: another domain: $2" >&2; exit 64; }
case "$1" in
  read) cat "$s/defaults.$3" 2>/dev/null || echo 1 ;;
  write) [[ $4 == -bool ]] || exit 64; if [[ $5 == true ]]; then echo 1; else echo 0; fi > "$s/defaults.$3" ;;
  *) exit 64 ;;
esac
`

const pmsetStub = `#!/bin/bash
echo "pmset $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"; mkdir -p "$s"
case "$1" in
  -g)
    echo "System-wide power settings:"
    echo "Currently in use:"
    echo " standby              0"
    echo " autorestart          $(cat "$s/pmset.autorestart" 2>/dev/null || echo 1)"
    echo " sleep                $(cat "$s/pmset.sleep" 2>/dev/null || echo 0) (sleep prevented by powerd)"
    echo " displaysleep         0" ;;
  -a) echo "$3" > "$s/pmset.$2" ;;
  *) exit 64 ;;
esac
`

// sshd reads the made-up machine's own sshd_config.d as the real one does:
// in name order, the first value of a key standing, and -T leaves out what
// follows a Match. sshd-capitals has -T
// print its keywords as macOS 27's sshd does, PasswordAuthentication;
// sshd-silent has it print nothing.
const sshdStub = `#!/bin/bash
echo "sshd $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"
case "$1" in
  -t) [[ ! -f $s/sshd-broken ]] || { echo "bad configuration" >&2; exit 255; } ;;
  -T)
    [[ ! -f $s/sshd-silent ]] || exit 0
    p=yes k=yes ps=0 ks=0
    for f in "$LAB_SYSROOT"/etc/ssh/sshd_config.d/*.conf; do
      [[ -f $f ]] || continue
      while read -r key val; do
        case "$key" in
          [Mm]atch) break ;;
          PasswordAuthentication) (( ps )) || { p="$val"; ps=1; } ;;
          KbdInteractiveAuthentication) (( ks )) || { k="$val"; ks=1; } ;;
        esac
      done < "$f"
    done
    echo "port 22"
    if [[ -f $s/sshd-capitals ]]; then
      echo "PasswordAuthentication $p"
      echo "KbdInteractiveAuthentication $k"
    else
      echo "passwordauthentication $p"
      echo "kbdinteractiveauthentication $k"
    fi
    echo "pubkeyauthentication yes" ;;
  *) exit 64 ;;
esac
`

// ps, lsof and nc answer what a test wrote down, and otherwise as a mini set
// up in full would: every process under the account its file names, nothing
// loaded from Homebrew, no secret in an argument, every port answering.
const machinePsStub = `#!/bin/bash
s="$STUB_STATE/machine"
if [[ "$*" == "-o user= -p "* ]]; then
  cat "$s/user.$4" 2>/dev/null || exit 1
elif [[ -f $s/ps.once ]]; then
  # One look that sees something else than the next.
  cat "$s/ps.once"
  rm -f "$s/ps.once"
else
  cat "$s/ps" 2>/dev/null || printf '/sbin/launchd\n/usr/local/lab/current/bin/labd --listen 127.0.0.1:7770 --oobd-token-file /var/lab/secrets/oobd.token\n'
fi
`

const machineLsofStub = `#!/bin/bash
s="$STUB_STATE/machine"
echo "COMMAND PID USER FD TYPE DEVICE SIZE/OFF NODE NAME"
cat "$s/lsof.${!#}" 2>/dev/null || echo "daemon ${!#} lab txt REG 1,4 100 200 /usr/lib/dyld"
`

const machineNcStub = `#!/bin/bash
echo "nc $*" >> "$STUB_LOG"
[[ ! -f $STUB_STATE/machine/closed.${!#} ]]
`

// tailscale plays the command of the open-source client, from its keg:
// "up" joins when the key's file holds a good key, and never prints it.
const tailscaleStub = `#!/bin/bash
echo "tailscale $*" >> "$STUB_LOG"
s="$STUB_STATE/machine"; mkdir -p "$s"
[[ $1 != --socket=* ]] || shift
case "$1" in
  status)
    [[ -f $STUB_STATE/loaded.org.omarchy-mac.lab.tailscaled ]] || { echo "failed to connect to local tailscaled" >&2; exit 1; }
    cat "$s/tailnet" 2>/dev/null || echo '{"BackendState":"NeedsLogin","Self":{"HostName":"marcelos-mac-mini","Tags":null},"Peer":null}' ;;
  up)
    [[ -f $STUB_STATE/loaded.org.omarchy-mac.lab.tailscaled ]] || exit 1
    key="" tags=""
    for a in "$@"; do
      case "$a" in
        --auth-key=file:*) key="${a#--auth-key=file:}" ;;
        --advertise-tags=*) tags="${a#--advertise-tags=}" ;;
        --auth-key=*) echo "a key on the command line" >> "$STUB_LOG" ;;
      esac
    done
    grep -q '^tskey-auth-good' "$key" 2>/dev/null || { echo "invalid key: unable to validate API key" >&2; exit 1; }
    printf '{"BackendState":"Running","Self":{"HostName":"marcelos-mac-mini","DNSName":"marcelos-mac-mini.tail0000.ts.net.","TailscaleIPs":["100.64.0.7","fd7a::7"],"Tags":["%s"]},"Peer":{}}\n' "$tags" > "$s/tailnet" ;;
  *) echo "tailscale $1: not expected" >&2; exit 64 ;;
esac
`

// machine gives the made-up mini its stand-ins for macOS's own tools.
func (m *mini) machine() {
	m.t.Helper()
	for name, text := range map[string]string{
		"networksetup": networksetupStub, "ipconfig": ipconfigStub, "defaults": defaultsStub,
		"pmset": pmsetStub, "sshd": sshdStub, "ps": machinePsStub, "lsof": machineLsofStub, "nc": machineNcStub,
		"sysctl": sysctlStub,
	} {
		write(m.t, filepath.Join(m.tools, name), text, 0o755)
	}
	write(m.t, filepath.Join(m.tools, "tailscale"), tailscaleStub, 0o755)
	m.env = append(m.env, "LAB_SYSTEM_TOOLS="+m.tools, "STUB_TAILSCALE="+filepath.Join(m.tools, "tailscale"))
}

// set writes down one thing about the made-up machine; unset takes it back.
func (m *mini) set(name, text string) {
	m.t.Helper()
	write(m.t, filepath.Join(filepath.Dir(m.root), "state", "machine", name), text, 0o644)
}

func (m *mini) unset(name string) {
	os.Remove(filepath.Join(filepath.Dir(m.root), "state", "machine", name))
}

func (m *mini) is(name string) string {
	return strings.TrimSpace(read(m.t, filepath.Join(filepath.Dir(m.root), "state", "machine", name)))
}

// ownerHasAKey gives the account running the set-up a key to come in with.
func (m *mini) ownerHasAKey() string {
	m.t.Helper()
	file := m.path("Users/" + account(m.t) + "/.ssh/authorized_keys")
	write(m.t, file, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOwnerOwnerOwnerOwnerOwnerOwnerOwnerOwnerOwnerO owner\n", 0o600)
	return file
}

// withTailscale puts tailscale on the Homebrew list, already installed, as a
// keg whose two programs are the stand-ins above.
func (m *mini) withTailscale() {
	m.t.Helper()
	write(m.t, m.tree+"/deploy/homebrew-formulae", "tailscale\n", 0o644)
	m.tailscaleKeg("1.0")
}

func (m *mini) tailscaleKeg(version string) {
	m.t.Helper()
	keg := m.path("opt/homebrew/Cellar/tailscale/" + version)
	write(m.t, keg+"/bin/tailscale", tailscaleStub, 0o755)
	write(m.t, keg+"/bin/tailscaled", "#!/bin/sh\n# tailscaled "+version+"\n", 0o755)
	os.Remove(m.path("opt/homebrew/opt/tailscale"))
	if err := os.MkdirAll(m.path("opt/homebrew/opt"), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/tailscale/"+version, m.path("opt/homebrew/opt/tailscale")); err != nil {
		m.t.Fatal(err)
	}
}

func tailnetKey(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "tailscale-key")
	write(t, file, text, mode)
	return file
}

func TestSetupTurnsWiFiOffOnlyWhileTheMiniIsWired(t *testing.T) {
	m := newMini(t)
	k := newKey(t)

	out := m.ok("setup", "--dry-run", "--release-key", k.signers)
	if !strings.Contains(out, "would turn Wi-Fi (en1) off") || strings.Contains(m.calls(), "-setairportpower") {
		t.Fatalf("the dry run:\n%s\n%s", out, m.calls())
	}

	// With no wired address, turning Wi-Fi off would take the mini off the network.
	m.set("unplugged", "")
	out = m.ok("setup", "--release-key", k.signers)
	if !strings.Contains(out, "Wi-Fi (en1) is left on") || strings.Contains(m.calls(), "-setairportpower") {
		t.Fatalf("unplugged, Wi-Fi was not left alone:\n%s\n%s", out, m.calls())
	}

	m.unset("unplugged")
	out = m.ok("setup")
	if !strings.Contains(out, "turn Wi-Fi (en1) off: the mini is wired at 192.168.1.20") || m.is("wifi") != "Off" {
		t.Fatalf("wired, Wi-Fi is %q:\n%s", m.is("wifi"), out)
	}
	if !strings.Contains(m.calls(), "networksetup -setairportpower en1 off") {
		t.Errorf("Wi-Fi was not turned off by networksetup:\n%s", m.calls())
	}

	os.Remove(m.log)
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") || strings.Contains(m.calls(), "-setairportpower") {
		t.Errorf("a second run:\n%s", out)
	}

	// A Mac with no Wi-Fi has nothing to turn off.
	m.set("no-wifi", "")
	m.set("wifi", "On")
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("with no Wi-Fi port:\n%s", out)
	}
}

func TestSetupMakesSSHKeysOnlyOnceTheOwnerHasAKey(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	conf := m.path(sshdKeysOnly)

	// Without a key of the owner's in place, keys-only would lock him out.
	out := m.ok("setup", "--release-key", k.signers)
	if exists(conf) || !strings.Contains(out, "SSH still takes a password") || !strings.Contains(out, "authorized_keys") {
		t.Fatalf("with no key of the owner's:\n%s", out)
	}
	write(t, m.path("Users/"+account(t)+"/.ssh/authorized_keys"), "# no key yet\n", 0o600)
	if m.ok("setup"); exists(conf) {
		t.Fatal("a file with no key in it was taken for a key")
	}

	keys := m.ownerHasAKey()
	out = m.ok("setup", "--dry-run")
	if exists(conf) || !strings.Contains(out, "would write "+conf) {
		t.Fatalf("the dry run:\n%s", out)
	}

	out = m.ok("setup")
	want := "PasswordAuthentication no\nKbdInteractiveAuthentication no\n"
	if got := read(t, conf); !strings.HasSuffix(got, want) || strings.Count(got, "Authentication") != 2 {
		t.Fatalf("%s:\n%s\n%s", conf, got, out)
	}
	if mode(t, conf) != 0o644 || !strings.Contains(out, keys) {
		t.Errorf("the file is %v; the set-up said:\n%s", mode(t, conf), out)
	}
	if !strings.Contains(m.calls(), "sshd -t") || !strings.Contains(m.calls(), "sshd -T") {
		t.Errorf("sshd was not asked whether it takes the file and what it now does:\n%s", m.calls())
	}
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("a second run:\n%s", out)
	}

	// A file sshd refuses is taken back: a broken sshd lets nobody in.
	os.Remove(conf)
	m.set("sshd-broken", "")
	out = m.refused("setup")
	if exists(conf) || !strings.Contains(out, "sshd does not take") {
		t.Errorf("a file sshd refuses was left in place:\n%s", out)
	}
	m.unset("sshd-broken")

	// A file read before the lab's that says otherwise wins, and is named.
	write(t, m.path("etc/ssh/sshd_config.d/000-earlier.conf"), "PasswordAuthentication yes\n", 0o644)
	m.set("pmset.sleep", "10\n")
	out = m.refused("setup")
	if !strings.Contains(out, "still takes a password") || !strings.Contains(out, "sshd_config.d") {
		t.Errorf("an earlier file that allows passwords was not noticed:\n%s", out)
	}
	// The steps after it are done all the same.
	if m.is("pmset.sleep") != "0" {
		t.Errorf("the run stopped at sshd and left the rest undone:\n%s", out)
	}
}

// sshd -T prints the settings outside every Match block: a Match block that
// takes a password again is read from the files and fails the set-up.
func TestAMatchBlockThatTakesAPasswordFailsTheSetUp(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	m.ownerHasAKey()
	m.ok("setup", "--release-key", k.signers)
	match := m.path("etc/ssh/sshd_config.d/900-match.conf")
	for _, text := range []string{
		"# a Mac on the LAN\nMatch Address 192.168.0.0/16\n  PasswordAuthentication yes\n",
		"Match User lab\n\tkbdinteractiveauthentication=yes\n",
	} {
		write(t, match, text, 0o644)
		out := m.refused("setup")
		if !strings.Contains(out, "a Match block lets SSH take a password after Unlock") || !strings.Contains(out, match) {
			t.Errorf("a Match block %q was not noticed:\n%s", text, out)
		}
	}
	// One that turns it off, or a setting before any Match, is no Match's doing.
	write(t, match, "PasswordAuthentication no\nMatch User lab\n  PasswordAuthentication no\n", 0o644)
	if out := m.ok("setup"); strings.Contains(out, "Match block") {
		t.Errorf("a Match block that says no:\n%s", out)
	}
}

// sshd's keywords are any case: macOS 27's sshd -T prints them capitalised.
// "no answer" is said only when sshd -T printed nothing.
func TestSetupReadsSshdInAnyCaseAndSaysNoAnswerOnlyForNothing(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	m.ownerHasAKey()
	m.set("sshd-capitals", "")

	out := m.ok("setup", "--release-key", k.signers)
	if strings.Contains(out, "still takes a password") || !exists(m.path(sshdKeysOnly)) {
		t.Fatalf("capitalised keywords were not read:\n%s", out)
	}
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("a second run with capitalised keywords:\n%s", out)
	}

	// Capitals that say otherwise are seen, and not as no answer.
	write(t, m.path("etc/ssh/sshd_config.d/000-earlier.conf"), "PasswordAuthentication yes\n", 0o644)
	out = m.refused("setup")
	if !strings.Contains(out, "still takes a password") || !strings.Contains(out, "passwordauthentication yes") || strings.Contains(out, "no answer") {
		t.Errorf("a capitalised yes:\n%s", out)
	}
	os.Remove(m.path("etc/ssh/sshd_config.d/000-earlier.conf"))

	m.set("sshd-silent", "")
	if out := m.refused("setup"); !strings.Contains(out, "(sshd -T: no answer)") {
		t.Errorf("an sshd -T that printed nothing:\n%s", out)
	}
}

func TestSetupStopsUpdatesThatRestartByThemselves(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	prefs := m.path("Library/Preferences/com.apple.SoftwareUpdate")

	out := m.ok("setup", "--dry-run", "--release-key", k.signers)
	for _, want := range []string{"would turn AutomaticallyInstallMacOSUpdates off", "would turn CriticalUpdateInstall off"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say it %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ConfigDataInstall") || strings.Contains(m.calls(), "defaults write") {
		t.Errorf("the dry run wrote, or would touch what is already right:\n%s", out)
	}

	m.ok("setup", "--release-key", k.signers)
	calls := m.calls()
	for _, want := range []string{
		"defaults write " + prefs + " AutomaticallyInstallMacOSUpdates -bool false",
		"defaults write " + prefs + " CriticalUpdateInstall -bool false",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("no %q in\n%s", want, calls)
		}
	}
	if strings.Contains(calls, "ConfigDataInstall -bool") {
		t.Error("ConfigDataInstall was on and was written again")
	}

	os.Remove(m.log)
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") || strings.Contains(m.calls(), "defaults write") {
		t.Errorf("a second run:\n%s", out)
	}

	// XProtect and configuration data stay automatic.
	m.set("defaults.ConfigDataInstall", "0\n")
	m.ok("setup")
	if !strings.Contains(m.calls(), "defaults write "+prefs+" ConfigDataInstall -bool true") {
		t.Errorf("ConfigDataInstall was not turned back on:\n%s", m.calls())
	}
}

func TestSetupKeepsAutorestartAndNoSleep(t *testing.T) {
	m := newMini(t)
	k := newKey(t)

	m.ok("setup", "--release-key", k.signers)
	if strings.Contains(m.calls(), "pmset -a") {
		t.Errorf("power was as it should be and was set again:\n%s", m.calls())
	}

	m.set("pmset.autorestart", "0\n")
	m.set("pmset.sleep", "10\n")
	out := m.ok("setup", "--dry-run")
	if !strings.Contains(out, "would set autorestart 1") || !strings.Contains(out, "would set sleep 0") || strings.Contains(m.calls(), "pmset -a") {
		t.Fatalf("the dry run:\n%s", out)
	}
	m.ok("setup")
	if m.is("pmset.autorestart") != "1" || m.is("pmset.sleep") != "0" {
		t.Errorf("autorestart %s, sleep %s", m.is("pmset.autorestart"), m.is("pmset.sleep"))
	}
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("a second run:\n%s", out)
	}
}

// tailscaled is root's, from the copy of the Homebrew tools, under the lab's
// own file: not Homebrew's service and not tailscaled's own system daemon.
func TestTheTailscaledLaunchDaemon(t *testing.T) {
	file := filepath.Join(repo(t), "deploy", tailscaledLabel+".plist")
	values, arrays := plist(t, file)
	for key, want := range map[string]string{"Label": tailscaledLabel, "UserName": "", "RunAtLoad": "true", "KeepAlive": "true"} {
		if values[key] != want {
			t.Errorf("%s is %q, want %q", key, values[key], want)
		}
	}
	args := arrays["ProgramArguments"]
	if len(args) != 3 || args[0] != "/usr/local/lab/tools/current/tailscale/bin/tailscaled" {
		t.Fatalf("tailscaled is not run from the copy of the Homebrew tools: %v", args)
	}
	if args[1] != "--state=/Library/Tailscale/tailscaled.state" || args[2] != "--socket=/var/run/tailscaled.socket" {
		t.Errorf("its state and socket are not where tailscaled keeps them on macOS: %v", args[1:])
	}
	if got := shell(t, "TAILSCALED_SOCKET"); got != "$SYSROOT/var/run/tailscaled.socket" {
		t.Errorf("the scripts ask tailscaled at %s", got)
	}
	for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
		if !strings.HasPrefix(values[key], "/var/log/") {
			t.Errorf("%s is %q", key, values[key])
		}
	}
	if !strings.Contains(" "+strings.Join(formulaeList(t), " ")+" ", " tailscale ") {
		t.Error("tailscale is not in deploy/homebrew-formulae")
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("plutil", "-lint", file).CombinedOutput(); err != nil {
			t.Errorf("plutil: %v\n%s", err, out)
		}
	}
}

func TestSetupStartsTailscaledFromTheCopyAndJoinsTaggedOnce(t *testing.T) {
	need(t, "jq")
	m := newMini(t)
	k := newKey(t)
	m.withTailscale()
	installed := m.path("Library/LaunchDaemons/" + tailscaledLabel + ".plist")
	copyOf := m.path("usr/local/lab/tools/current/tailscale/bin") + "/"

	// The Standalone app and tailscaled must not run on one machine, and the
	// app goes by the owner's hand, at the screen.
	if err := os.MkdirAll(m.path("Applications/Tailscale.app/Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := m.ok("setup", "--release-key", k.signers)
	if m.daemonLoaded(tailscaledLabel) || exists(installed) || !strings.Contains(out, "Standalone app is still") {
		t.Fatalf("tailscaled was started beside the Standalone app:\n%s", out)
	}
	os.RemoveAll(m.path("Applications/Tailscale.app"))

	// With no key it starts and waits to be joined.
	out = m.ok("setup")
	if !m.daemonLoaded(tailscaledLabel) || !strings.Contains(out, "--tailscale-key") {
		t.Fatalf("with no key:\n%s", out)
	}
	if read(t, installed) != read(t, filepath.Join(repo(t), "deploy", tailscaledLabel+".plist")) {
		t.Error("the installed file is not the repository's")
	}
	if strings.Contains(m.calls(), " up ") {
		t.Errorf("tailscale up ran with no key:\n%s", m.calls())
	}

	// What is not a key, or not kept to its owner, is refused before anything.
	for name, file := range map[string]string{
		"readable by others": tailnetKey(t, goodTailnetKey+"\n", 0o644),
		"not a key":          tailnetKey(t, "hello\n", 0o600),
		"two lines":          tailnetKey(t, goodTailnetKey+"\n"+goodTailnetKey+"\n", 0o600),
		"an API key":         tailnetKey(t, "tskey-api-kMadeUp11CNTRL-notASecretOnlyATest\n", 0o600),
	} {
		if out := m.refused("setup", "--tailscale-key", file); strings.Contains(out, "tskey-") {
			t.Errorf("%s: the refusal shows the key:\n%s", name, out)
		}
		if strings.Contains(m.calls(), " up ") {
			t.Fatalf("%s: it was handed to tailscale all the same:\n%s", name, m.calls())
		}
	}
	m.refused("setup", "--tailscale-key", filepath.Join(t.TempDir(), "absent"))

	// A key Tailscale refuses joins nothing and is left for the owner to see.
	bad := tailnetKey(t, "tskey-auth-usedUp11CNTRL-notASecretOnlyATest\n", 0o600)
	out = m.refused("setup", "--tailscale-key", bad)
	if !exists(bad) || !strings.Contains(out, "did not join") {
		t.Errorf("a refused key:\n%s", out)
	}

	key := tailnetKey(t, goodTailnetKey+"\n", 0o600)
	out = m.ok("setup", "--dry-run", "--tailscale-key", key)
	if !strings.Contains(out, "would join the tailnet as a node tagged tag:lab") || !exists(key) {
		t.Fatalf("the dry run:\n%s", out)
	}
	os.Remove(m.log)
	out = m.ok("setup", "--tailscale-key", key)
	calls := m.calls()
	if !strings.Contains(calls, " up --auth-key=file:"+key+" --advertise-tags=tag:lab") {
		t.Fatalf("the mini was not joined with the key's file and the tag:\n%s\n%s", calls, out)
	}
	if exists(key) {
		t.Error("the key's file was left behind")
	}
	if !strings.Contains(out, "marcelos-mac-mini.tail0000.ts.net") || !strings.Contains(out, "100.64.0.7") || !strings.Contains(out, "old node") {
		t.Errorf("the set-up does not say what the mini is on the tailnet now, or to remove the old node:\n%s", out)
	}
	for _, text := range []string{out, calls} {
		if strings.Contains(text, goodTailnetKey) || strings.Contains(text, "a key on the command line") {
			t.Errorf("the key was printed or was an argument:\n%s", text)
		}
	}
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "tailscale ") && !strings.HasPrefix(line, "tailscale --socket="+m.path("var/run/tailscaled.socket")+" ") {
			t.Errorf("tailscale was not asked at the lab's tailscaled: %s", line)
		}
	}
	// Every call of the command is the copy's: the keg's own is never run.
	if !exists(copyOf+"tailscale") || !exists(copyOf+"tailscaled") {
		t.Error("tailscale and tailscaled are not both in the copy")
	}

	// Safe to run again; a key brought a second time is only removed.
	os.Remove(m.log)
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") || strings.Contains(m.calls(), " up ") || strings.Contains(m.calls(), "bootstrap") {
		t.Errorf("a second run:\n%s\n%s", out, m.calls())
	}
	again := tailnetKey(t, goodTailnetKey+"\n", 0o600)
	if m.ok("setup", "--tailscale-key", again); exists(again) || strings.Contains(m.calls(), " up ") {
		t.Errorf("a key brought again was used or kept:\n%s", m.calls())
	}

	// A new copy of the tools starts tailscaled again and joins nothing anew.
	m.tailscaleKeg("2.0")
	os.RemoveAll(m.path("opt/homebrew/Cellar/tailscale/1.0"))
	os.Remove(m.log)
	m.ok("setup")
	if !strings.Contains(m.calls(), "launchctl bootstrap system "+installed) || strings.Contains(m.calls(), " up ") {
		t.Errorf("after a new copy of the tools:\n%s", m.calls())
	}
	if got := read(t, copyOf+"tailscaled"); !strings.Contains(got, "tailscaled 2.0") {
		t.Errorf("the copy in use: %q", got)
	}

	// On the tailnet without the tag, its key would expire: said, and put
	// right with a new key.
	m.set("tailnet", `{"BackendState":"Running","Self":{"HostName":"marcelos-mac-mini","Tags":null},"Peer":{}}`)
	if out := m.ok("setup"); !strings.Contains(out, "without tag:lab") {
		t.Errorf("an untagged node was not noticed:\n%s", out)
	}
	fresh := tailnetKey(t, goodTailnetKey+"\n", 0o600)
	os.Remove(m.log)
	m.ok("setup", "--tailscale-key", fresh)
	if !strings.Contains(m.calls(), "--advertise-tags=tag:lab --force-reauth") {
		t.Errorf("an untagged node was not joined again under the tag:\n%s", m.calls())
	}

	all := read(t, m.log)
	for _, never := range []string{"brew services", "install-system-daemon", "brew upgrade"} {
		if strings.Contains(all, never) {
			t.Errorf("the set-up ran %s", never)
		}
	}
}

// A run that stops part of the way is finished by the next one, which does
// only what is left.
func TestSetupTakesUpWhereAFailedRunStopped(t *testing.T) {
	m := newMini(t)
	k := newKey(t)
	m.ownerHasAKey()
	write(t, m.tree+"/deploy/pins", "madeup 1.0 "+strings.Repeat("0", 64)+" file:///nowhere/madeup-1.0.pkg\n", 0o644)

	out := m.refused("setup", "--release-key", k.signers)
	if !strings.Contains(out, "could not download") {
		t.Fatalf("the run was to stop at the pin:\n%s", out)
	}
	if !exists(m.path("var/lab/data")) || !exists(m.path("usr/local/lab/allowed_signers")) {
		t.Fatal("what came before the failed step was not done")
	}
	if exists(m.path(sshdKeysOnly)) || m.is("wifi") == "Off" {
		t.Fatal("a step after the failed one ran")
	}

	write(t, m.tree+"/deploy/pins", "# none\n", 0o644)
	os.Remove(m.log)
	out = m.ok("setup")
	if strings.Contains(m.calls(), "-create") || strings.Contains(out, "allowed_signers") {
		t.Errorf("the second run did again what the first had done:\n%s", out)
	}
	if !exists(m.path(sshdKeysOnly)) || m.is("wifi") != "Off" {
		t.Errorf("the second run did not finish:\n%s", out)
	}
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Errorf("a third run:\n%s", out)
	}
}
