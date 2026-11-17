package minitest

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The daemons that stay up, and the account each runs as by the spec's
// table. The others run at their times.
var keptUp = map[string]string{
	labdLabel:        "lab",
	litestreamLabel:  "lab",
	cloudflaredLabel: "lab",
	signalLabel:      "lab",
	oobdLabel:        "root",
	builderLabel:     "root",
	tailscaledLabel:  "root",
}

var atTheirTimes = []string{backupLabel, drillLabel, deadManLabel, containerLabel}

// whole is a made-up mini with everything the owner brings to the first
// set-up: a Deployed build, labd under launchd as Deploy leaves it, a key to
// come in with, and one file or folder for each flag.
type whole struct {
	*mini
	flags []string
	// brought are the files and folders the set-up is to take and remove.
	brought []string
	secrets []string
}

func wholeMini(t *testing.T) *whole {
	t.Helper()
	need(t, "openssl", "jq", "shasum")
	m := builderMini(t)
	k := newKey(t)
	w := &whole{mini: m}
	const tag = "v0.2.0-rc.1"

	write(t, m.tree+"/deploy/homebrew-formulae", read(t, filepath.Join(repo(t), "deploy", "homebrew-formulae")), 0o644)
	write(t, m.path("etc/hosts"), hostsBefore, 0o644)
	m.builderBuild(tag)
	build := m.path("usr/local/lab/builds/" + tag)
	for _, name := range []string{"backup", "restore-drill", "dead-man"} {
		write(t, build+"/scripts/mini/"+name, "#!/bin/bash\n", 0o755)
	}
	write(t, build+"/bin/labd", "#!/bin/sh\n", 0o755)
	write(t, build+"/bin/macvdmtool", "#!/bin/sh\n# the reset tool\n", 0o755)
	write(t, build+"/deploy/cloudflared.yml", "# routes\n", 0o644)
	m.ownerHasAKey()

	// Deploy installs labd's file and starts it; the set-up never does.
	write(t, m.path("Library/LaunchDaemons/"+labdLabel+".plist"), read(t, filepath.Join(repo(t), "deploy", labdLabel+".plist")), 0o644)
	write(t, filepath.Join(filepath.Dir(m.root), "state", "loaded"), "", 0o644)

	// A pinned package from a server of the test's, and the reset tool's line.
	pkg := "a made-up container package\n"
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { fmt.Fprint(rw, pkg) }))
	t.Cleanup(srv.Close)
	write(t, m.tree+"/deploy/pins", fmt.Sprintf("macvdmtool d57c4850b386fea7c0c7e406addde2703a5f9f29 %s https://github.com/omacom/macvdmtool\ncontainer 1.7.1 %x %s/container-1.7.1.pkg\n",
		strings.Repeat("2", 64), sha256.Sum256([]byte(pkg)), srv.URL), 0o644)

	cred := credentialFile(t, madeUpCredential+"\n", 0o600)
	keys := keysFolder(t, houseSecrets, offsiteSecrets)
	checks := checksCarried(t, "dead-man", "backup", "restore-drill")
	token := tokenFile(t, madeUpToken+"\n", 0o600)
	tailnet := tailnetKey(t, goodTailnetKey+"\n", 0o600)
	w.flags = []string{
		"--release-key", k.signers,
		"--tunnel-credential", cred,
		"--backup-guest", "192.168.0.186", "--backup-keys", keys,
		"--checks", checks,
		"--container-user", account(t), "--builder-token", token, "--builder-rootfs", rootfs(t),
		"--tailscale-key", tailnet,
	}
	w.brought = []string{cred, keys + "/restic-password", keys + "/r2-secret-access-key", checks + "/dead-man", token, tailnet}
	w.secrets = []string{madeUpCredential, madeUpToken, goodTailnetKey, houseSecrets["restic-password"], offsiteSecrets["r2-secret-access-key"], checkIDs["dead-man"]}
	return w
}

// running writes down, as launchd and ps would show it, that each daemon
// that stays up runs under its account.
func (w *whole) running() {
	w.t.Helper()
	state := filepath.Join(filepath.Dir(w.root), "state")
	pid := 400
	for label, account := range keptUp {
		pid++
		write(w.t, filepath.Join(state, "pid."+label), fmt.Sprint(pid), 0o644)
		w.set(fmt.Sprintf("user.%d", pid), account+"\n")
	}
	for _, label := range []string{"com.openssh.sshd", "com.apple.screensharing"} {
		write(w.t, filepath.Join(state, "loaded."+label), "", 0o644)
	}
}

func (w *whole) pid(label string) string {
	return strings.TrimSpace(read(w.t, filepath.Join(filepath.Dir(w.root), "state", "pid."+label)))
}

func (w *whole) installed(label string) string {
	return w.path("Library/LaunchDaemons/" + label + ".plist")
}

// One run with everything the owner brings sets the whole machine up, in the
// order of the spec's list; the dry run before it says so and changes
// nothing; the run after it changes nothing.
func TestOneRunSetsTheWholeMachineUp(t *testing.T) {
	t.Parallel()
	w := wholeMini(t)
	w.set("pmset.sleep", "10\n")

	dry := w.ok("setup", append([]string{"--dry-run"}, w.flags...)...)
	at := -1
	for _, step := range []string{
		"would make the group /Groups/lab",
		"would pin the release key",
		"would install cloudflared from Homebrew",
		"would install tailscale from Homebrew",
		"would copy the Homebrew tools",
		"would install container 1.7.1",
		"would make the Out-of-band controller's token",
		"would put the tunnel's credential",
		"would install " + w.installed(signalLabel),
		"would put restic-password",
		"would put the check dead-man",
		"would write down that container runs under",
		"would start tailscaled (" + tailscaledLabel + ")",
		"would turn Wi-Fi (en1) off",
		"would write " + w.path(sshdKeysOnly),
		"would turn AutomaticallyInstallMacOSUpdates off",
		"would turn CriticalUpdateInstall off",
		"would set sleep 0",
		"changes would be made; nothing was changed",
	} {
		i := strings.Index(dry, step)
		if i < 0 {
			t.Fatalf("the dry run does not say: %s\n%s", step, dry)
		}
		if i < at {
			t.Errorf("the dry run says %q before the step the spec puts ahead of it:\n%s", step, dry)
		}
		at = i
	}
	if exists(w.path("usr/local/lab/tools")) || exists(w.path("var/lab/data")) || exists(w.path(sshdKeysOnly)) || exists(w.installed(tailscaledLabel)) || w.is("wifi") == "Off" {
		t.Fatal("the dry run changed the machine")
	}
	for _, f := range w.brought {
		if !exists(f) {
			t.Fatalf("the dry run took %s", f)
		}
	}
	for _, call := range []string{"brew install", "installer", "bootstrap", "-create", " up ", "defaults write", "pmset -a", "-setairportpower"} {
		if strings.Contains(w.calls(), call) {
			t.Fatalf("the dry run ran %q:\n%s", call, w.calls())
		}
	}

	os.Remove(w.log)
	out := w.ok("setup", w.flags...)

	for label := range keptUp {
		if label != labdLabel && !w.daemonLoaded(label) {
			t.Errorf("%s is not loaded after the run", label)
		}
	}
	for _, label := range atTheirTimes {
		if !w.daemonLoaded(label) {
			t.Errorf("%s is not loaded after the run", label)
		}
	}
	if strings.Contains(w.calls(), "system/"+labdLabel) {
		t.Errorf("the set-up touched labd, which is Deploy's:\n%s", w.calls())
	}
	for _, f := range w.brought {
		if exists(f) {
			t.Errorf("%s was left where the owner brought it", f)
		}
	}
	for _, secret := range w.secrets {
		if s := strings.TrimSpace(secret); strings.Contains(out, s) || strings.Contains(w.calls(), s) {
			t.Errorf("a secret was printed or was an argument: %.12s...", s)
		}
	}
	if w.is("wifi") != "Off" || w.is("defaults.CriticalUpdateInstall") != "0" || w.is("pmset.sleep") != "0" || !exists(w.path(sshdKeysOnly)) {
		t.Errorf("the machine's own settings were not all made:\n%s", out)
	}
	if got := read(t, w.path("usr/local/lab/installed-pins")); !strings.HasPrefix(got, "container 1.7.1 ") {
		t.Errorf("installed-pins: %q", got)
	}
	sealed(t, w.path("usr/local/lab/tools"))

	// No LaunchDaemon file on the machine names a path under Homebrew, none
	// is Homebrew's own, and Homebrew was only ever asked to install.
	files, _ := filepath.Glob(w.path("Library/LaunchDaemons/*.plist"))
	if len(files) != len(keptUp)+len(atTheirTimes) {
		t.Errorf("%d LaunchDaemon files are installed, want one for each of the lab's %d daemons: %v", len(files), len(keptUp)+len(atTheirTimes), files)
	}
	for _, f := range files {
		if text := read(t, f); strings.Contains(text, "/opt/homebrew") || strings.Contains(text, "homebrew.mxcl") {
			t.Errorf("%s names Homebrew", f)
		}
	}
	for _, line := range strings.Split(w.calls(), "\n") {
		if !strings.HasPrefix(line, "brew ") {
			continue
		}
		verb := strings.Fields(line)[1]
		if verb != "list" && verb != "deps" && verb != "--prefix" && verb != "install" {
			t.Errorf("the set-up ran: %s", line)
		}
		if !strings.Contains(line, "auto_update=1 upgrade=1") || strings.HasSuffix(line, "uid=0]") {
			t.Errorf("brew was not told to leave itself and what is installed alone, or ran as root: %s", line)
		}
	}

	// Safe to run again, without what was brought or with the very same
	// line: what the first run took is gone from where the owner left it.
	os.Remove(w.log)
	for _, args := range [][]string{{}, {"--dry-run"}, w.flags} {
		if out := w.ok("setup", args...); !strings.Contains(out, "setup: nothing to change") {
			t.Errorf("a second run (%v) changed something:\n%s", args, out)
		}
	}
	again := w.ok("setup", w.flags...)
	for _, want := range []string{"the tunnel's credential is in place", "the Builder's token is in place", "every key of the backups is in place", "every check's address is in place", "the mini's place on the tailnet is in place"} {
		if !strings.Contains(again, want+": a run before this one took it") {
			t.Errorf("given the same line again, the set-up does not say that %s:\n%s", want, again)
		}
	}
	for _, call := range []string{"bootstrap", "bootout", "brew install", "installer", " up ", "defaults write", "pmset -a", "-setairportpower"} {
		if strings.Contains(w.calls(), call) {
			t.Errorf("a second run ran %q:\n%s", call, w.calls())
		}
	}
}

func TestTheCheckPassesOnAMiniSetUpInFullAndChangesNothing(t *testing.T) {
	t.Parallel()
	w := wholeMini(t)
	w.ok("setup", w.flags...)
	w.running()
	before := tree(t, w.root)
	os.Remove(w.log)

	out := w.ok("check")

	for _, want := range []string{
		"ok    " + labdLabel + ": pid " + w.pid(labdLabel) + " as lab, /usr/local/lab/builds/v0.2.0-rc.1/bin/labd, nothing of Homebrew's loaded",
		"ok    " + tailscaledLabel + ": pid " + w.pid(tailscaledLabel) + " as root, /usr/local/lab/tools/",
		"ok    " + oobdLabel + ": pid " + w.pid(oobdLabel) + " as root",
		"ok    " + backupLabel + ": as lab, /usr/local/lab/builds/v0.2.0-rc.1/scripts/mini/backup; it runs at its times, not now",
		"ok    " + containerLabel + ": as root",
		"ok    no service of Homebrew's own",
		"ok    no LaunchDaemon file names a path under /opt/homebrew",
		"ok    the Tailscale Standalone app is gone",
		"ok    the copy the daemons run from, ",
		"cloudflared 1.0",
		"note  no previous copy yet",
		"ok    tailscale 1.0",
		"ok    litestream 1.0",
		"ok    the Deployed build is v0.2.0-rc.1",
		"ok    container 1.7.1, installed from the package with sha256 ",
		"ok    macvdmtool, built from d57c4850b386fea7c0c7e406addde2703a5f9f29",
		"ok    the mini is on the tailnet as marcelos-mac-mini.tail0000.ts.net, 100.64.0.7,fd7a::7",
		"ok    the node is tagged tag:lab",
		"ok    no other node has this machine's name: the old node is gone",
		"ok    sshd -T: passwordauthentication no",
		"ok    sshd -T: kbdinteractiveauthentication no",
		"ok    Remote Login is on",
		"ok    Screen Sharing is on",
		"ok    SSH answers on 192.168.1.20 (the LAN)",
		"ok    Screen Sharing answers on 100.64.0.7 (the tailnet)",
		"ok    Wi-Fi (en1) is off",
		"ok    Software Update: AutomaticallyInstallMacOSUpdates 0",
		"ok    Software Update: CriticalUpdateInstall 0",
		"ok    Software Update: ConfigDataInstall 1",
		"ok    pmset: autorestart 1",
		"ok    pmset: sleep 0",
		"ok    no process has a token",
		"check: every check passed; nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the check does not show: %s", want)
		}
	}
	if strings.Contains(out, "FAIL") {
		t.Errorf("a check failed on a mini set up in full:\n%s", out)
	}
	if t.Failed() {
		t.Logf("the check said:\n%s", out)
	}
	for label := range keptUp {
		if !strings.Contains(out, "ok    "+label+": pid ") {
			t.Errorf("%s is not shown running", label)
		}
	}
	for _, secret := range w.secrets {
		if strings.Contains(out, strings.TrimSpace(secret)) {
			t.Errorf("the check printed a secret: %.12s...", secret)
		}
	}
	if after := tree(t, w.root); after != before {
		t.Errorf("the check changed the machine:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	for _, call := range []string{"bootstrap", "bootout", "kickstart", "brew ", "defaults write", "pmset -a", "-setairportpower", " up ", "installer", "-create"} {
		if strings.Contains(w.calls(), call) {
			t.Errorf("the check ran %q:\n%s", call, w.calls())
		}
	}

	// After an upgrade the copy it replaced is the way back, and is shown.
	w.upgrade("restic", "2.0")
	w.ok("setup")
	out = w.ok("check")
	if !strings.Contains(out, "ok    the previous copy, the way back after a bad upgrade, ") || !strings.Contains(out, "restic 1.0") || !strings.Contains(out, "ok    restic 2.0") {
		t.Errorf("after an upgrade the previous copy is not shown:\n%s", out)
	}
	// A formula upgraded outside a Maintenance run is not what the daemons run.
	w.upgrade("restic", "3.0")
	if out = w.ok("check"); !strings.Contains(out, "note  restic: the daemons run 2.0 and Homebrew now has 3.0") {
		t.Errorf("a formula ahead of the copy is not said:\n%s", out)
	}
}

// tree is every file under dir with its mode, size and where a link leads.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		to, _ := os.Readlink(p)
		fmt.Fprintf(&b, "%s %v %d %s\n", strings.TrimPrefix(p, dir), info.Mode(), info.Size(), to)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Each thing the check looks at, broken: the check fails and names it, and
// passes again once all is mended. Things that do not hide one another are
// broken together, each daemon in one way only, so the check runs four
// times and not seventy.
func TestTheCheckNamesWhatIsNotAsTheSetUpLeavesIt(t *testing.T) {
	t.Parallel()
	w := wholeMini(t)
	w.ok("setup", w.flags...)
	w.running()
	w.upgrade("restic", "2.0")
	w.ok("setup")
	// sshd -T as macOS 27's prints it, keywords capitalised.
	w.set("sshd-capitals", "")
	if out, code := w.run("check"); code != 0 {
		t.Fatalf("the check fails before anything is broken:\n%s", out)
	}
	state := filepath.Join(filepath.Dir(w.root), "state")
	tools := w.path("usr/local/lab/tools")
	edit := func(file, old, new string) func() {
		t.Helper()
		was := read(t, file)
		if !strings.Contains(was, old) {
			t.Fatalf("%s has no %q", file, old)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		os.Chmod(file, 0o644)
		write(t, file, strings.Replace(was, old, new, 1), info.Mode().Perm())
		return func() { os.Chmod(file, 0o644); write(t, file, was, info.Mode().Perm()) }
	}
	file := func(path, text string, mode os.FileMode) func() {
		write(t, path, text, mode)
		return func() { os.Remove(path) }
	}
	gone := func(path string) func() {
		was := read(t, path)
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		os.Remove(path)
		return func() { write(t, path, was, info.Mode().Perm()) }
	}
	fact := func(name, text string) func() {
		was, had := w.is(name), exists(filepath.Join(state, "machine", name))
		w.set(name, text)
		return func() {
			if had {
				w.set(name, was+"\n")
			} else {
				w.unset(name)
			}
		}
	}
	chmod := func(path string, mode os.FileMode) func() {
		was := mode
		if info, err := os.Stat(path); err == nil {
			was = info.Mode().Perm()
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return func() { os.Chmod(path, was) }
	}
	labdPid, tailscaledPid := w.pid(labdLabel), w.pid(tailscaledLabel)

	cases := []struct {
		name  string
		round int
		broke func() func()
		want  string
	}{
		{"an installed file names a path under Homebrew", 0, func() func() {
			return edit(w.installed(cloudflaredLabel), "/usr/local/lab/tools/current/cloudflared/bin/cloudflared", "/opt/homebrew/bin/cloudflared")
		}, "FAIL  " + cloudflaredLabel + ": /Library/LaunchDaemons/" + cloudflaredLabel + ".plist names a path under /opt/homebrew"},
		{"another LaunchDaemon file names a path under Homebrew", 0, func() func() {
			return file(w.path("Library/LaunchDaemons/org.example.other.plist"), "<plist><string>/opt/homebrew/bin/other</string></plist>\n", 0o644)
		}, "FAIL  /Library/LaunchDaemons/org.example.other.plist names a path under /opt/homebrew"},
		{"a service file of Homebrew's own", 0, func() func() {
			return file(w.path("Library/LaunchDaemons/homebrew.mxcl.tailscale.plist"), "<plist/>\n", 0o644)
		}, "FAIL  /Library/LaunchDaemons/homebrew.mxcl.tailscale.plist is a service file of Homebrew's"},
		{"tailscaled's own system daemon", 0, func() func() {
			return file(w.path("Library/LaunchDaemons/com.tailscale.tailscaled.plist"), "<plist/>\n", 0o644)
		}, "FAIL  /Library/LaunchDaemons/com.tailscale.tailscaled.plist is a service file"},
		{"the Standalone app is back", 0, func() func() {
			file(w.path("Applications/Tailscale.app/Contents/Info.plist"), "<plist/>\n", 0o644)
			return func() { os.RemoveAll(w.path("Applications/Tailscale.app")) }
		}, "FAIL  the Tailscale Standalone app is still at /Applications/Tailscale.app"},
		{"a daemon's file gives it another account", 0, func() func() {
			return edit(w.installed(backupLabel), "<key>UserName</key><string>lab</string>", "<key>UserName</key><string>root</string>")
		}, "FAIL  " + backupLabel + ": its file runs it as root; the spec's table says lab"},
		{"a daemon's file holds an environment", 0, func() func() {
			return edit(w.installed(oobdLabel), "<key>RunAtLoad</key>", "<key>EnvironmentVariables</key><dict><key>OOBD_TOKEN</key><string>x</string></dict><key>RunAtLoad</key>")
		}, "FAIL  " + oobdLabel + ": /Library/LaunchDaemons/" + oobdLabel + ".plist holds an environment"},
		{"a daemon is run from outside the lab's folder", 0, func() func() {
			return edit(w.installed(signalLabel), "/usr/local/lab/tools/current/signal-cli/bin/signal-cli", "/usr/local/bin/signal-cli")
		}, "FAIL  " + signalLabel + " runs /usr/local/bin/signal-cli, outside /usr/local/lab"},
		{"a daemon's program can be written by another account", 1, func() func() {
			return chmod(w.path("usr/local/lab/builds/v0.2.0-rc.1/bin"), 0o775)
		}, "/usr/local/lab/builds/v0.2.0-rc.1/bin can be changed by another account"},
		{"a daemon's file is missing", 0, func() func() { return gone(w.installed(deadManLabel)) },
			"FAIL  " + deadManLabel + ": no /Library/LaunchDaemons/" + deadManLabel + ".plist"},
		{"a daemon is not loaded", 0, func() func() { return gone(filepath.Join(state, "loaded."+litestreamLabel)) },
			"FAIL  " + litestreamLabel + " is installed and not loaded"},
		{"a daemon that stays up is not running", 0, func() func() { return gone(filepath.Join(state, "pid."+builderLabel)) },
			"FAIL  " + builderLabel + " is loaded and not running"},
		{"a process runs under another account", 0, func() func() { return fact("user."+labdPid, "root\n") },
			"FAIL  " + labdLabel + " (pid " + labdPid + ") runs as root; the spec's table says lab"},
		{"a process has a file of Homebrew's loaded", 0, func() func() {
			return fact("lsof."+tailscaledPid, "tailscaled "+tailscaledPid+" root txt REG 1,4 100 200 /opt/homebrew/Cellar/go/1.26/lib/libgo.dylib\n")
		}, "FAIL  " + tailscaledLabel + " (pid " + tailscaledPid + ") has a file under /opt/homebrew loaded"},
		{"a file of the copy can be written by another account", 0, func() func() {
			return chmod(tools+"/current/restic/bin/restic", 0o777)
		}, "FAIL  the copy the daemons run from"},
		{"the previous copy can be written by another account", 0, func() func() {
			return chmod(tools+"/previous/cloudflared/bin", 0o775)
		}, "FAIL  the previous copy, the way back after a bad upgrade"},
		{"a file of the copy leads back to Homebrew", 1, func() func() {
			return file(tools+"/current/restic/bin/wrapper", "#!/bin/sh\nexec /opt/homebrew/opt/restic/bin/restic \"$@\"\n", 0o755)
		}, "restic/bin/wrapper names Homebrew's folder (/opt/homebrew/opt/restic/bin/restic)"},
		{"a link of the previous copy leaves it", 1, func() func() {
			if err := os.Symlink("/usr/lib", tools+"/previous/restic/lib"); err != nil {
				t.Fatal(err)
			}
			return func() { os.Remove(tools + "/previous/restic/lib") }
		}, "FAIL  the previous copy, the way back after a bad upgrade, "},
		{"a key of a known shape is an argument", 2, func() func() {
			return fact("ps", "/sbin/launchd\n/usr/local/lab/tools/current/tailscale/bin/tailscale up --auth-key=tskey-auth-kMadeUp11CNTRL-onlyATest\n")
		}, "FAIL  a secret is an argument of: /usr/local/lab/tools/current/tailscale/bin/tailscale"},
		{"a formula is not installed", 0, func() func() {
			os.Rename(w.path("opt/homebrew/opt/cloudflared"), w.path("opt/homebrew/opt/away"))
			return func() { os.Rename(w.path("opt/homebrew/opt/away"), w.path("opt/homebrew/opt/cloudflared")) }
		}, "FAIL  cloudflared is not installed"},
		{"a formula is not in the copy", 0, func() func() {
			return edit(w.tree+"/deploy/homebrew-formulae", "restic ", "restic \nmadeup ")
		}, "FAIL  madeup is not installed"},
		{"container is not at its pin", 0, func() func() {
			return edit(w.tree+"/deploy/pins", "container 1.7.1", "container 1.7.2")
		}, "FAIL  container: deploy/pins names 1.7.2"},
		{"the Deployed build has no reset tool", 0, func() func() { return gone(w.path("usr/local/lab/builds/v0.2.0-rc.1/bin/macvdmtool")) },
			"FAIL  no reset tool at /usr/local/lab/current/bin/macvdmtool"},
		{"the node is not tagged", 0, func() func() {
			return fact("tailnet", `{"BackendState":"Running","Self":{"HostName":"marcelos-mac-mini","DNSName":"marcelos-mac-mini.tail0000.ts.net.","TailscaleIPs":["100.64.0.7"],"Tags":null},"Peer":{}}`)
		}, "FAIL  the node is not tagged tag:lab"},
		{"the old node is still there", 1, func() func() {
			return fact("tailnet", `{"BackendState":"Running","Self":{"HostName":"marcelos-mac-mini","DNSName":"marcelos-mac-mini-1.tail0000.ts.net.","TailscaleIPs":["100.64.0.7"],"Tags":["tag:lab"]},"Peer":{"nodekey:aa":{"HostName":"omarchy-gpu","DNSName":"omarchy-gpu.tail0000.ts.net."},"nodekey:bb":{"HostName":"marcelos-mac-mini","DNSName":"marcelos-mac-mini.tail0000.ts.net."}}}`)
		}, "FAIL  another node has this machine's name (marcelos-mac-mini.tail0000.ts.net)"},
		{"the mini is off the tailnet", 2, func() func() {
			return fact("tailnet", `{"BackendState":"NeedsLogin","Self":{"HostName":"marcelos-mac-mini","Tags":null},"Peer":null}`)
		}, "FAIL  the mini is not on the tailnet (NeedsLogin)"},
		{"sshd takes a password", 0, func() func() { return gone(w.path(sshdKeysOnly)) },
			"FAIL  sshd -T does not say passwordauthentication no"},
		{"a Match block takes a password", 1, func() func() {
			return file(w.path("etc/ssh/sshd_config.d/900-match.conf"), "Match Address 10.0.0.0/8\n  PasswordAuthentication yes\n", 0o644)
		}, "FAIL  a Match block lets SSH take a password after Unlock (" + w.path("etc/ssh/sshd_config.d/900-match.conf")},
		{"sshd -T prints nothing", 2, func() func() { return fact("sshd-silent", "") },
			"FAIL  sshd does not say what it takes (sshd -T gave nothing)"},
		{"the owner has no key", 0, func() func() { return gone(w.path("Users/" + account(t) + "/.ssh/authorized_keys")) },
			"has no key in /Users/"},
		{"Remote Login is off", 0, func() func() { return gone(filepath.Join(state, "loaded.com.openssh.sshd")) },
			"FAIL  Remote Login is off"},
		{"Screen Sharing is off", 0, func() func() { return gone(filepath.Join(state, "loaded.com.apple.screensharing")) },
			"FAIL  Screen Sharing is off"},
		{"Screen Sharing does not answer", 0, func() func() { return fact("closed.5900", "") },
			"FAIL  Screen Sharing does not answer on 192.168.1.20:5900 (the LAN)"},
		{"Wi-Fi is on", 0, func() func() { return fact("wifi", "On\n") }, "FAIL  Wi-Fi (en1) is On"},
		{"macOS updates install by themselves", 0, func() func() { return fact("defaults.AutomaticallyInstallMacOSUpdates", "1\n") },
			"FAIL  Software Update: AutomaticallyInstallMacOSUpdates is 1, want 0"},
		{"configuration data no longer comes by itself", 0, func() func() { return fact("defaults.ConfigDataInstall", "0\n") },
			"FAIL  Software Update: ConfigDataInstall is 0, want 1"},
		{"the mini would not start after a power loss", 0, func() func() { return fact("pmset.autorestart", "0\n") },
			"FAIL  pmset: autorestart is 0, want 1"},
		{"the mini sleeps", 0, func() func() { return fact("pmset.sleep", "10\n") }, "FAIL  pmset: sleep is 10, want 0"},
		{"a token of a known shape is an argument", 0, func() func() {
			return fact("ps", "/sbin/launchd\n/usr/local/lab/current/bin/lab-builder --server http://127.0.0.1:7770 --token "+madeUpToken+"\n")
		}, "FAIL  a secret is an argument of: /usr/local/lab/current/bin/lab-builder"},
		{"what a secret's file holds is an argument", 1, func() func() {
			return fact("ps", "/sbin/launchd\n/usr/local/lab/tools/current/cloudflared/bin/cloudflared tunnel run "+madeUpCredential+"\n")
		}, "FAIL  a secret is an argument of: /usr/local/lab/tools/current/cloudflared/bin/cloudflared"},
	}
	for round := 0; round < 3; round++ {
		var mends []func()
		for _, c := range cases {
			if c.round == round {
				mends = append(mends, c.broke())
			}
		}
		out, code := w.run("check")
		if code != 1 || !strings.Contains(out, "checks failed; nothing was changed") {
			t.Errorf("round %d: exit %d:\n%s", round, code, out)
		}
		for _, c := range cases {
			if c.round == round && !strings.Contains(out, c.want) {
				t.Errorf("%s: no line %q in\n%s", c.name, c.want, out)
			}
		}
		for _, secret := range w.secrets {
			if strings.Contains(out, strings.TrimSpace(secret)) {
				t.Errorf("round %d: the check printed a secret", round)
			}
		}
		for i := len(mends) - 1; i >= 0; i-- {
			mends[i]()
		}
	}
	if out, code := w.run("check"); code != 0 {
		t.Fatalf("the check still fails once everything is mended:\n%s", out)
	}
}

func TestTheCheckTakesNoArgumentAndNeedsItsDeployFolder(t *testing.T) {
	m := newMini(t)
	if out := m.refused("check", "--fix"); !strings.Contains(out, "change nothing") {
		t.Errorf("an argument was not answered with the usage:\n%s", out)
	}
	os.Remove(m.tree + "/deploy/pins")
	if out := m.refused("check"); !strings.Contains(out, "no deploy/ folder") {
		t.Errorf("with no deploy folder:\n%s", out)
	}
}

// On a mini nothing was done to, the check fails and says so line by line:
// it is no set-up and makes nothing.
func TestTheCheckOnAnUntouchedMiniFailsAndMakesNothing(t *testing.T) {
	need(t, "jq")
	m := newMini(t)
	write(t, m.tree+"/deploy/homebrew-formulae", read(t, filepath.Join(repo(t), "deploy", "homebrew-formulae")), 0o644)
	out := m.refused("check")
	for _, want := range []string{
		"FAIL  " + labdLabel + ": no /Library/LaunchDaemons/",
		"FAIL  no copy in use",
		"FAIL  cloudflared is not installed",
		"FAIL  no build is Deployed",
		"FAIL  no tailscale in the copy",
		"FAIL  sshd -T does not say passwordauthentication no",
		"FAIL  Wi-Fi (en1) is On",
		"FAIL  Software Update: CriticalUpdateInstall is 1, want 0",
		"checks failed; nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no line %q in\n%s", want, out)
		}
	}
	if exists(m.path("usr/local/lab")) || exists(m.path("var/lab")) {
		t.Error("the check made folders")
	}
}
