package minitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	containerLabel = "org.omarchy-mac.lab.container"
	builderLabel   = "org.omarchy-mac.lab.builder"
	builderToken   = "usr/local/lab/secrets/builder.token"
	madeUpToken    = "bldr_0123456789abcdef0123456789abcdef01234567"
	builderIO      = "usr/local/lab/io"
	rootfsName     = "ArchLinuxARM-aarch64-latest.tar.gz"
	pkgContext     = "usr/local/lab/pkg-image"
)

// container plays Apple's container: its server is up while the file "up"
// exists, and cannot be started while "broken" does. It builds an image only
// from a folder that holds every file its Containerfile adds. It writes down
// what it was asked.
const containerStub = `#!/bin/bash
echo "container $*" >> "$STUB_LOG"
s="$STUB_STATE/container"
mkdir -p "$s"
case "$1 $2" in
  "system status") [[ -f $s/up ]] || { echo "apiserver is not running and not registered with launchd" >&2; exit 1; } ;;
  "system start") [[ ! -f $s/broken ]] || { echo "could not start" >&2; exit 1; }; touch "$s/up" ;;
  "volume inspect")
    [[ -f $s/up && -f $s/volume.$3 ]] || exit 1
    if [[ -f $s/$3.img ]]; then printf '[{"name":"%s","source":"%s"}]\n' "$3" "$s/$3.img"; else printf '[{"name":"%s"}]\n' "$3"; fi ;;
  "volume create") [[ -f $s/up && ! -f $s/full ]] || exit 1; echo "$4" > "$s/volume.$5" ;;
  "volume delete") [[ -f $s/up && -f $s/volume.$3 ]] || exit 1; rm "$s/volume.$3" ;;
  "image inspect") [[ -f $s/up && -f $s/image.$3 ]] || exit 1 ;;
  "image delete") [[ -f $s/up && -f $s/image.$3 ]] || exit 1; rm "$s/image.$3" ;;
  build\ *) [[ -f $s/up && -f $4/Containerfile ]] || exit 1
    while read -r verb file _; do
      [[ $verb != ADD || -f $4/$file ]] || { echo "ADD $file: no such file in the build context" >&2; exit 1; }
    done < "$4/Containerfile"
    touch "$s/image.$3" ;;
  *) echo "container $*: not expected" >&2; exit 64 ;;
esac
`

// sudo -n -u USER COMMAND: written down, and COMMAND run.
const sudoStub = `#!/bin/bash
echo "sudo $*" >> "$STUB_LOG"
while [[ $1 == -* ]]; do
  case "$1" in -u) shift 2 ;; *) shift ;; esac
done
exec "$@"
`

// launchctl asuser UID COMMAND: written down, and COMMAND run; anything
// else is the made-up launchctl of every other test.
const asuserStub = `#!/bin/bash
if [[ $1 == asuser ]]; then
  echo "launchctl $*" >> "$STUB_LOG"
  shift 2
  exec "$@"
fi
exec "$(dirname "$0")/launchctl.real" "$@"
`

// builderMini is a made-up mini with container installed, stopped, and the
// Builder's files in the bundle the set-up runs from.
func builderMini(t *testing.T) *mini {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("container never runs as root, and these tests name the account running them")
	}
	m := newMini(t)
	if err := os.Rename(m.bin+"/launchctl", m.bin+"/launchctl.real"); err != nil {
		t.Fatal(err)
	}
	write(t, m.bin+"/launchctl", asuserStub, 0o755)
	write(t, m.bin+"/sudo", sudoStub, 0o755)
	write(t, m.path("usr/local/bin/container"), containerStub, 0o755)
	for _, label := range []string{containerLabel, builderLabel} {
		write(t, m.tree+"/deploy/"+label+".plist", read(t, filepath.Join(repo(t), "deploy", label+".plist")), 0o644)
	}
	if err := os.MkdirAll(m.path("var/lab/log"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.path("usr/local/lab"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.env = append(m.env, "LAB_BOOT=1", "LAB_START_TRIES=1", "LAB_WAIT_STEP=0.1")
	return m
}

// account is the name of the account running the tests, which stands in
// for the one container runs under.
func account(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func (m *mini) state(name string) string {
	return filepath.Join(filepath.Dir(m.root), "state", "container", name)
}

func (m *mini) serverUp() bool { return exists(m.state("up")) }

func (m *mini) kill() { os.Remove(m.state("up")) }

func (m *mini) touch(name string) { write(m.t, m.state(name), "", 0o644) }

// told is the lines "Agents" was told, without their times.
func (m *mini) told() []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(read(m.t, m.path("var/lab/log/notify.log"))), "\n") {
		if f := strings.SplitN(line, " ", 3); len(f) == 3 {
			out = append(out, f[2])
		}
	}
	return out
}

// builderBuild makes tag the Deployed build, with the Builder in it: the
// watch script beside what it sources, lab-builder, and the two images'
// folders.
func (m *mini) builderBuild(tag string) {
	m.t.Helper()
	m.deployed(tag, "")
	build := m.path("usr/local/lab/builds/" + tag)
	for _, name := range []string{"container", "lib.sh", "notify"} {
		write(m.t, build+"/scripts/mini/"+name, read(m.t, filepath.Join(m.tree, "scripts", "mini", name)), 0o755)
	}
	write(m.t, build+"/bin/lab-builder", "#!/bin/sh\n", 0o755)
	write(m.t, build+"/builder/Containerfile", "FROM scratch\n", 0o644)
	write(m.t, build+"/builder/pkg/Containerfile", "FROM scratch\nADD "+rootfsName+" /\n", 0o644)
	write(m.t, build+"/builder/pkg/pkgbuild", "#!/bin/bash\n", 0o755)
}

// builderRuns is what the Builder's installed LaunchDaemon file gives
// lab-builder for option, which names one of its images: the image's name
// and its tag.
func (m *mini) builderRuns(option string) string {
	m.t.Helper()
	f := regexp.MustCompile(`<string>` + option + `</string><string>([^<]*)</string>`).FindStringSubmatch(read(m.t, m.path("Library/LaunchDaemons/"+builderLabel+".plist")))
	if f == nil || !regexp.MustCompile(`^maclab-(kbuild|pkgbuild):[0-9a-f]{12}$`).MatchString(f[1]) {
		m.t.Fatalf("the Builder's installed file gives %s no image tagged by what it is made from: %q", option, f)
	}
	return f[1]
}

// rootfs is a made-up archive of Arch Linux ARM's root file system, as the
// owner brings it.
func rootfs(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), rootfsName)
	write(t, file, "\x1f\x8b\x08 a made-up root file system\n", 0o644)
	return file
}

// The two LaunchDaemon files: root's, from the Deployed build, with no
// secret. The one starts container's server at boot and looks every minute;
// the other waits for it and becomes lab-builder, with its token in a file.
func TestTheBuildersLaunchDaemons(t *testing.T) {
	watch := filepath.Join(repo(t), "deploy", containerLabel+".plist")
	values, arrays := plist(t, watch)
	for key, want := range map[string]string{"Label": containerLabel, "RunAtLoad": "true", "StartInterval": "60", "UserName": "", "KeepAlive": ""} {
		if values[key] != want {
			t.Errorf("%s: %s is %q, want %q", containerLabel, key, values[key], want)
		}
	}
	if got := strings.Join(arrays["ProgramArguments"], " "); got != "/usr/local/lab/current/scripts/mini/container watch CONTAINER_USER" {
		t.Errorf("container's server is watched by: %s", got)
	}

	file := filepath.Join(repo(t), "deploy", builderLabel+".plist")
	values, arrays = plist(t, file)
	for key, want := range map[string]string{"Label": builderLabel, "RunAtLoad": "true", "KeepAlive": "true", "Umask": "63", "UserName": ""} {
		if values[key] != want {
			t.Errorf("%s: %s is %q, want %q", builderLabel, key, values[key], want)
		}
	}
	want := "/usr/local/lab/current/scripts/mini/container wait CONTAINER_USER -- /usr/local/lab/current/bin/lab-builder" +
		" --server http://" + shell(t, "LABD_LISTEN") + " --token-file /usr/local/lab/secrets/builder.token --as-user CONTAINER_USER --io /usr/local/lab/io --cpus BUILDER_CPUS" +
		" --image BUILDER_IMAGE --pkg-image BUILDER_PKG_IMAGE"
	if got := strings.Join(arrays["ProgramArguments"], " "); got != want {
		t.Errorf("the Builder is started as:\n %s\nwant\n %s", got, want)
	}
	main := read(t, filepath.Join(repo(t), "cmd", "lab-builder", "main.go"))
	for _, flag := range []string{"server", "token-file", "as-user", "io", "cpus", "image", "pkg-image"} {
		if !regexp.MustCompile(`fl\.\w+\((&[\w.]+, )?"` + flag + `"`).MatchString(main) {
			t.Errorf("lab-builder has no flag --%s", flag)
		}
	}
	// The scratch folder is the one the set-up makes: root's, in no home.
	if shell(t, "BUILDER_IO") != "$LAB_ROOT/io" || shell(t, "LAB_ROOT") != "$SYSROOT/usr/local/lab" || !strings.Contains(main, `miniScratch = "/usr/local/lab/io"`) {
		t.Errorf("the set-up's scratch folder is %s, and lab-builder's is another", shell(t, "BUILDER_IO"))
	}
	for _, f := range []string{watch, file} {
		text := read(t, f)
		for _, word := range []string{"EnvironmentVariables", "MACLAB_", "bldr_", "<string>--token</string>", "PASSWORD", "/opt/homebrew", "/Users/"} {
			if strings.Contains(text, word) {
				t.Errorf("%s holds %s", filepath.Base(f), word)
			}
		}
		if runtime.GOOS == "darwin" {
			if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
				t.Errorf("plutil: %v\n%s", err, out)
			}
		}
	}
	// Only the watch starts the server.
	if strings.Contains(main, `"system"`) || strings.Contains(main, "system start") {
		t.Error("lab-builder knows how to start container's server")
	}
	// The volumes the set-up makes are the sizes lab-builder says, and the
	// artifact store is labd's own 20 GB.
	if shell(t, "BUILDER_VOLUMES") != "maclab-cache:40G maclab-pkgcache:120G" ||
		!strings.Contains(main, `kernelVolumeSize  = "40G"`) || !strings.Contains(main, `packageVolumeSize = "120G"`) {
		t.Errorf("the volumes are %s", shell(t, "BUILDER_VOLUMES"))
	}
	if strings.Contains(read(t, filepath.Join(repo(t), "deploy", labdLabel+".plist")), "artifact-budget") ||
		!strings.Contains(read(t, filepath.Join(repo(t), "cmd", "labd", "main.go")), `"artifact-budget-gb", 20,`) {
		t.Error("the artifact store is not labd's own 20 GB")
	}
	// container is pinned by version and checksum.
	if !regexp.MustCompile(`(?m)^container [0-9]+\.[0-9]+\.[0-9]+ [0-9a-f]{64} https://github\.com/apple/container/releases/download/\S+\.pkg$`).MatchString(read(t, filepath.Join(repo(t), "deploy", "pins"))) {
		t.Error("deploy/pins has no line for container with its version, checksum and package")
	}
}

// At boot the server is started and nothing is said. Found stopped, it is
// started again through the wrapper in the same look. Each change is one
// line for "Agents", and no line is said twice in a row.
func TestTheWatchStartsContainerAgainAndTellsEachChangeOnce(t *testing.T) {
	m := builderMini(t)
	user := account(t)
	uid, _ := exec.Command("id", "-u").Output()
	wrapper := "launchctl asuser " + strings.TrimSpace(string(uid)) + " sudo -n -u " + user + " " + m.path("usr/local/bin/container")
	look := func(wantOK bool, wantUp bool, wantTold ...string) {
		t.Helper()
		out, code := m.run("container", "watch", user)
		if (code == 0) != wantOK || m.serverUp() != wantUp {
			t.Fatalf("exit %d, server up %v; want ok %v, up %v\n%s", code, m.serverUp(), wantOK, wantUp, out)
		}
		told := m.told()
		if len(told) != len(wantTold) {
			t.Fatalf("\"Agents\" was told %d lines, want %d:\n%s", len(told), len(wantTold), strings.Join(told, "\n"))
		}
		for i, want := range wantTold {
			if !strings.Contains(told[i], want) {
				t.Fatalf("line %d is %q, want it to say %q", i+1, told[i], want)
			}
		}
	}
	const (
		stopped = "found stopped"
		again   = "started again"
		failed  = "could not be started"
		back    = "running again"
	)

	look(true, true)
	calls := m.calls()
	if !strings.Contains(calls, wrapper+" system status") || !strings.Contains(calls, wrapper+" system start --enable-kernel-install") {
		t.Fatalf("the server was not started through the wrapper:\n%s", calls)
	}
	for _, line := range strings.Split(calls, "\n") {
		if strings.HasPrefix(line, "container ") && !strings.Contains(calls, "sudo -n -u "+user+" "+m.path("usr/local/bin/container")+strings.TrimPrefix(line, "container")) {
			t.Errorf("container was run outside the wrapper: %s", line)
		}
	}
	look(true, true)

	m.kill()
	look(true, true, stopped, again)
	look(true, true, stopped, again)

	m.kill()
	m.touch("broken")
	look(false, false, stopped, again, stopped, failed)
	look(false, false, stopped, again, stopped, failed)
	look(false, false, stopped, again, stopped, failed)
	os.Remove(m.state("broken"))
	look(true, true, stopped, again, stopped, failed, again)

	// Stopped, not startable, then started by someone else.
	m.kill()
	m.touch("broken")
	look(false, false, stopped, again, stopped, failed, again, stopped, failed)
	m.touch("up")
	look(true, true, stopped, again, stopped, failed, again, stopped, failed, back)
	look(true, true, stopped, again, stopped, failed, again, stopped, failed, back)

	// Each line went out as the lab account, whose log it is.
	if n := strings.Count(m.calls(), "as-lab "+filepath.Join(m.tree, "scripts", "mini", "notify")+" agents "); n != 8 {
		t.Errorf("%d lines were handed to the lab account, want 8", n)
	}

	// After a restart of the mini the server is stopped, and starting it is
	// no news; a server that cannot be started then is.
	m.kill()
	os.Remove(m.state("broken"))
	m.env = append(m.env, "LAB_BOOT=2")
	look(true, true, stopped, again, stopped, failed, again, stopped, failed, back)
	m.kill()
	m.touch("broken")
	m.env = append(m.env, "LAB_BOOT=3")
	look(false, false, stopped, again, stopped, failed, again, stopped, failed, back, failed)
}

// The set-up runs a watch just as launchd runs the LaunchDaemon's first one:
// two watches at once both save the state and neither fails on the other's file.
func TestTwoWatchesAtOnceBothSaveTheState(t *testing.T) {
	m := builderMini(t)
	user := account(t)
	for round := 0; round < 10; round++ {
		var wg sync.WaitGroup
		outs := make([]string, 6)
		codes := make([]int, 6)
		for i := range outs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				outs[i], codes[i] = m.run("container", "watch", user)
			}(i)
		}
		wg.Wait()
		for i, code := range codes {
			if code != 0 {
				t.Fatalf("round %d, watch %d: exit %d\n%s", round, i, code, outs[i])
			}
		}
	}
}

// The Builder's LaunchDaemon waits until the server answers and starts
// nothing itself.
func TestTheBuilderWaitsForContainerAndNeverStartsIt(t *testing.T) {
	m := builderMini(t)
	cmd := exec.Command(filepath.Join(m.tree, "scripts", "mini", "container"), "wait", account(t), "--", "/bin/echo", "lab-builder would poll now")
	cmd.Env = m.env
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("it did not wait for the server: %v\n%s", err, out.String())
	case <-time.After(time.Second):
	}
	if strings.Contains(m.calls(), "system start") || m.serverUp() {
		t.Fatalf("waiting started the server:\n%s", m.calls())
	}
	m.touch("up")
	select {
	case err := <-done:
		if err != nil || !strings.Contains(out.String(), "lab-builder would poll now") {
			t.Fatalf("after the server came up: %v\n%s", err, out.String())
		}
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("it went on waiting with the server up")
	}
	if strings.Contains(m.calls(), "system start") {
		t.Fatalf("waiting started the server:\n%s", m.calls())
	}
}

func tokenFile(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "mini.token")
	write(t, file, text, mode)
	return file
}

func TestSetupRefusesWhatIsNoAccountNoTokenAndNoLimit(t *testing.T) {
	m := builderMini(t)
	k := newKey(t)
	good := tokenFile(t, madeUpToken, 0o600)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	for what, args := range map[string][]string{
		"an account that is not there":    {"--container-user", "nobody-of-that-name"},
		"root":                            {"--container-user", "root"},
		"the lab account":                 {"--container-user", "lab"},
		"no name":                         {"--container-user", "../x"},
		"twelve cores":                    {"--builder-cpus", "12"},
		"a token others read":             {"--builder-token", tokenFile(t, madeUpToken, 0o644)},
		"a token that is a link":          {"--builder-token", link},
		"a file that holds no token":      {"--builder-token", tokenFile(t, "lab_0123456789abcdef0123456789abcdef01234567", 0o600)},
		"a file that holds two":           {"--builder-token", tokenFile(t, madeUpToken+"\n"+madeUpToken+"\n", 0o600)},
		"a token file that is not there":  {"--builder-token", filepath.Join(t.TempDir(), "none")},
		"a token with something after it": {"--builder-token", tokenFile(t, madeUpToken+" x", 0o600)},
	} {
		out := m.refused("setup", append([]string{"--release-key", k.signers}, args...)...)
		if strings.Contains(out, madeUpToken) {
			t.Errorf("%s: the refusal shows the token:\n%s", what, out)
		}
	}
	if exists(m.path(builderToken)) || exists(m.path("usr/local/lab/container-user")) || strings.Contains(m.calls(), "container ") {
		t.Errorf("a refused run changed something:\n%s", m.calls())
	}
	if !exists(good) {
		t.Error("a refused run removed a token file")
	}
}

// The set-up names the account once, starts the watch, makes the volumes at
// their sizes and the images, puts the token where only root reads it and
// starts the Builder; a second run changes nothing.
func TestSetupBringsTheBuilderUp(t *testing.T) {
	need(t, "openssl")
	m := builderMini(t)
	k := newKey(t)
	user := account(t)
	containerPlist := m.path("Library/LaunchDaemons/" + containerLabel + ".plist")
	builderPlist := m.path("Library/LaunchDaemons/" + builderLabel + ".plist")
	loaded := func(label string) bool { return exists(filepath.Join(filepath.Dir(m.root), "state", "loaded."+label)) }

	// Before anything is named, and before a Deploy.
	out := m.ok("setup", "--release-key", k.signers)
	if !strings.Contains(out, "--container-user") || exists(containerPlist) {
		t.Fatalf("with no account named:\n%s", out)
	}
	out = m.ok("setup", "--container-user", user)
	if !strings.Contains(out, "once a build is Deployed") || !exists(containerPlist) || loaded(containerLabel) || strings.Contains(m.calls(), "container ") {
		t.Fatalf("before a Deploy:\n%s", out)
	}
	if info, err := os.Lstat(m.path(builderIO)); err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
		t.Fatalf("the Builder's scratch folder was not made for the account to reach: %v %v", info, err)
	}
	m.builderBuild("v0.2.0")

	token := tokenFile(t, madeUpToken+"\n", 0o600)
	dry := m.ok("setup", "--dry-run", "--builder-token", token)
	if !strings.Contains(dry, "would") || loaded(containerLabel) || m.serverUp() || !exists(token) || exists(m.path(builderToken)) {
		t.Fatalf("the dry run:\n%s", dry)
	}

	out = m.ok("setup", "--builder-rootfs", rootfs(t))
	if !loaded(containerLabel) || !m.serverUp() || loaded(builderLabel) || !strings.Contains(out, "lab builder add mini --kinds kernel,package") {
		t.Fatalf("without a token the Builder waits, and the rest is up:\n%s", out)
	}
	calls := m.calls()
	for _, want := range []string{
		"container volume create -s 40G maclab-cache",
		"container volume create -s 120G maclab-pkgcache",
		"container build -t " + m.builderRuns("--image") + " " + m.path("usr/local/lab/current/builder"),
		"container build -t " + m.builderRuns("--pkg-image") + " " + m.path(pkgContext),
		"sudo -n -u " + user + " " + m.path("usr/local/bin/container") + " system start --enable-kernel-install",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("the set-up did not run: %s", want)
		}
	}
	if len(m.told()) != 0 {
		t.Errorf("the set-up's own start told \"Agents\": %v", m.told())
	}

	out = m.ok("setup", "--builder-token", token)
	if strings.Contains(out, madeUpToken) || strings.Contains(m.calls(), madeUpToken) {
		t.Fatal("the token was shown or handed to a command")
	}
	if exists(token) || strings.TrimSpace(read(t, m.path(builderToken))) != madeUpToken || mode(t, m.path(builderToken)) != 0o600 {
		t.Fatalf("the token is not root's alone, or its first file is still there:\n%s", out)
	}
	if !loaded(builderLabel) {
		t.Fatalf("the Builder was not started:\n%s", out)
	}
	for file, wants := range map[string][]string{
		containerPlist: {"<string>watch</string>\n    <string>" + user + "</string>"},
		builderPlist:   {"<string>--as-user</string><string>" + user + "</string>", "<string>--io</string><string>/usr/local/lab/io</string>", "<string>--cpus</string><string>10</string>", "<string>wait</string>\n    <string>" + user + "</string>"},
	} {
		text := read(t, file)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %s", filepath.Base(file), want)
			}
		}
		if strings.Contains(text, "CONTAINER_USER") || strings.Contains(text, "BUILDER_") || mode(t, file) != 0o644 {
			t.Errorf("%s was not filled in, or is not 0644", filepath.Base(file))
		}
	}

	os.Remove(m.log)
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") {
		t.Fatalf("a second run:\n%s", out)
	}
	if calls := m.calls(); strings.Contains(calls, "bootstrap") || strings.Contains(calls, "create") || strings.Contains(calls, "container build") || strings.Contains(calls, "system start") {
		t.Fatalf("a second run did something:\n%s", calls)
	}

	// Fewer cores after a failed gate test: the Builder alone starts again,
	// and the setting stays.
	os.Remove(m.log)
	out = m.ok("setup", "--builder-cpus", "8")
	if !strings.Contains(read(t, builderPlist), "<string>--cpus</string><string>8</string>") ||
		strings.Count(m.calls(), "launchctl bootstrap") != 1 || !strings.Contains(m.calls(), "bootstrap system "+builderPlist) {
		t.Fatalf("at 8 cores:\n%s\n%s", out, m.calls())
	}
	if out := m.ok("setup"); !strings.Contains(out, "nothing to change") || !strings.Contains(read(t, builderPlist), "<string>8</string>") {
		t.Fatalf("after the change of cores:\n%s", out)
	}

	// The server killed by hand: the set-up starts it again too.
	m.kill()
	m.ok("setup")
	if !m.serverUp() {
		t.Fatal("the set-up left the server stopped")
	}
}

// The package image is built from Arch Linux ARM's root file system, which
// the set-up does not download. Without it the Builder's step stops before
// container's server is started, and says what to bring; brought, it is
// copied into a build context that is root's, beside the Deployed build's
// files, and the image is built from there.
func TestSetupBuildsThePackageImageFromTheRootFileSystemBrought(t *testing.T) {
	need(t, "openssl")
	m := builderMini(t)
	k := newKey(t)
	loaded := func(label string) bool { return exists(filepath.Join(filepath.Dir(m.root), "state", "loaded."+label)) }
	m.ok("setup", "--release-key", k.signers, "--container-user", account(t))
	m.builderBuild("v0.2.0")
	os.Remove(m.log)

	for _, args := range [][]string{{"--dry-run"}, {}} {
		out := m.ok("setup", args...)
		if !strings.Contains(out, "--builder-rootfs FILE") || !strings.Contains(out, rootfsName) {
			t.Errorf("setup %v does not say what to bring:\n%s", args, out)
		}
		if m.serverUp() || loaded(containerLabel) || strings.Contains(m.callsOrNone(), "container ") {
			t.Fatalf("setup %v reached container with no root file system:\n%s\n%s", args, out, m.callsOrNone())
		}
	}

	link := filepath.Join(t.TempDir(), rootfsName)
	if err := os.Symlink(rootfs(t), link); err != nil {
		t.Fatal(err)
	}
	notGzip := filepath.Join(t.TempDir(), rootfsName)
	write(t, notGzip, "<html>not found</html>\n", 0o644)
	for what, file := range map[string]string{"a file that is not there": filepath.Join(t.TempDir(), "none"), "a link": link, "a page in place of the archive": notGzip} {
		m.refused("setup", "--builder-rootfs", file)
		if m.serverUp() || exists(m.path(pkgContext)+"/"+rootfsName) {
			t.Fatalf("%s was taken", what)
		}
	}

	brought := rootfs(t)
	dry := m.ok("setup", "--dry-run", "--builder-rootfs", brought)
	if !strings.Contains(dry, "would") || m.serverUp() || exists(m.path(pkgContext)+"/"+rootfsName) {
		t.Fatalf("the dry run:\n%s", dry)
	}
	out := m.ok("setup", "--builder-rootfs", brought)
	if !m.serverUp() || !regexp.MustCompile(`container build -t maclab-pkgbuild:[0-9a-f]{12} `+regexp.QuoteMeta(m.path(pkgContext))).MatchString(m.calls()) {
		t.Fatalf("the image was not built from its own context:\n%s\n%s", out, m.calls())
	}
	if !regexp.MustCompile(`sha256 [0-9a-f]{64}`).MatchString(out) {
		t.Errorf("the run does not say which archive it took:\n%s", out)
	}
	if read(t, m.path(pkgContext)+"/"+rootfsName) != read(t, brought) || mode(t, m.path(pkgContext)+"/"+rootfsName) != 0o644 || mode(t, m.path(pkgContext)) != 0o755 {
		t.Error("the archive is not in the build context as it was brought, for the account to read")
	}
	for _, name := range []string{"Containerfile", "pkgbuild"} {
		if read(t, m.path(pkgContext)+"/"+name) != read(t, m.path("usr/local/lab/current/builder/pkg/"+name)) {
			t.Errorf("the build context lacks the Deployed build's %s", name)
		}
	}
	if !exists(m.path("usr/local/lab/current/builder/pkg/Containerfile")) || exists(m.path("usr/local/lab/current/builder/pkg/"+rootfsName)) {
		t.Error("the Deployed build was changed")
	}

	os.Remove(m.log)
	for _, args := range [][]string{{}, {"--builder-rootfs", brought}} {
		if out := m.ok("setup", args...); strings.Contains(m.callsOrNone(), "container build") || strings.Contains(out, "root file system") {
			t.Fatalf("setup %v, a second run, did something:\n%s\n%s", args, out, m.callsOrNone())
		}
	}
}

// An image is tagged by what it is made from. A Deploy whose builder/ is
// other names another tag: the set-up builds that image alone and starts
// the Builder with it. Of the images it made before, the last is kept as
// the way back and the older ones removed; a Deploy with the same builder/
// builds nothing.
func TestSetupBuildsTheImageOfAChangedBuilderAndKeepsTheWayBack(t *testing.T) {
	need(t, "openssl")
	m := builderMini(t)
	k := newKey(t)
	builderPlist := m.path("Library/LaunchDaemons/" + builderLabel + ".plist")
	image := func(tag string) bool { return exists(m.state("image." + tag)) }
	m.ok("setup", "--release-key", k.signers, "--container-user", account(t))
	m.builderBuild("v0.2.0")
	// Made before the tags followed what an image is made from.
	m.touch("image.maclab-kbuild:1")
	m.touch("image.maclab-pkgbuild:1")
	m.ok("setup", "--builder-rootfs", rootfs(t), "--builder-token", tokenFile(t, madeUpToken+"\n", 0o600))
	k1, p1 := m.builderRuns("--image"), m.builderRuns("--pkg-image")
	if !image(k1) || !image(p1) || !image("maclab-kbuild:1") || !image("maclab-pkgbuild:1") {
		t.Fatalf("the first images: %s %s\n%s", k1, p1, m.calls())
	}

	built := func(version string, change func(build string)) string {
		t.Helper()
		m.builderBuild(version)
		if change != nil {
			change(m.path("usr/local/lab/builds/" + version))
		}
		os.Remove(m.log)
		return m.ok("setup")
	}
	restarted := func() bool { return strings.Contains(m.callsOrNone(), "bootstrap system "+builderPlist) }

	// The same builder/ in a new build.
	if out := built("v0.2.1", nil); !strings.Contains(out, "nothing to change") || strings.Contains(m.callsOrNone(), "container build") || restarted() {
		t.Fatalf("a build with the same builder/:\n%s\n%s", out, m.callsOrNone())
	}

	// The fix of 7227e8aa: kbuild reads its config at /in.
	kbuild := func(text string) func(string) {
		return func(build string) { write(t, build+"/builder/kbuild", text, 0o755) }
	}
	out := built("v0.3.0", kbuild("#!/bin/bash\ncp /in/config .config\n"))
	k2 := m.builderRuns("--image")
	if k2 == k1 || m.builderRuns("--pkg-image") != p1 || !restarted() {
		t.Fatalf("a changed builder/kbuild: %s then %s, the Builder started again: %v\n%s", k1, k2, restarted(), out)
	}
	if calls := m.calls(); !strings.Contains(calls, "container build -t "+k2+" "+m.path("usr/local/lab/current/builder")) || strings.Contains(calls, "-t maclab-pkgbuild") {
		t.Fatalf("not the kernel image alone was built:\n%s", calls)
	}
	if !image(k1) || image("maclab-kbuild:1") || !image("maclab-pkgbuild:1") {
		t.Errorf("%s is not kept as the way back, or the image tagged 1 is left:\n%s", k1, out)
	}

	out = built("v0.3.1", kbuild("#!/bin/bash\ncp /in/config .config\nmake\n"))
	k3 := m.builderRuns("--image")
	if k3 == k2 || k3 == k1 || !image(k3) || !image(k2) || image(k1) {
		t.Fatalf("a second change: %s, %s, %s:\n%s", k1, k2, k3, out)
	}

	// builder/pkg changed: the package image alone.
	out = built("v0.3.2", func(build string) {
		kbuild("#!/bin/bash\ncp /in/config .config\nmake\n")(build)
		write(t, build+"/builder/pkg/pkgbuild", "#!/bin/bash\nmakepkg\n", 0o755)
	})
	p2 := m.builderRuns("--pkg-image")
	if p2 == p1 || m.builderRuns("--image") != k3 || !image(p2) || !image(p1) || image("maclab-pkgbuild:1") || strings.Contains(m.calls(), "-t maclab-kbuild") {
		t.Fatalf("a changed builder/pkg: %s then %s:\n%s\n%s", p1, p2, out, m.calls())
	}
	// Another root file system: another package image.
	os.Remove(m.log)
	other := rootfs(t)
	write(t, other, "\x1f\x8b\x08 another root file system\n", 0o644)
	out = m.ok("setup", "--builder-rootfs", other)
	if p3 := m.builderRuns("--pkg-image"); p3 == p2 || !image(p3) || !image(p2) || image(p1) {
		t.Fatalf("another root file system: %s then %s:\n%s", p2, p3, out)
	}

	// Deployed back to v0.3.0: its kernel image is there, and is not built.
	m.deployed("v0.3.0", "")
	os.Remove(m.log)
	out = m.ok("setup")
	if m.builderRuns("--image") != k2 || strings.Contains(m.callsOrNone(), "-t maclab-kbuild") || !restarted() {
		t.Fatalf("back to v0.3.0:\n%s\n%s", out, m.callsOrNone())
	}
}

// Given the installed token as its own source, the set-up would find it
// "already" in place and remove it: the Builder could not start again until
// a Maintainer adds a builder anew.
func TestSetupRefusesItsOwnInstalledBuilderTokenAsTheSource(t *testing.T) {
	need(t, "openssl")
	m := builderMini(t)
	k := newKey(t)
	m.ok("setup", "--release-key", k.signers, "--container-user", account(t), "--builder-token", tokenFile(t, madeUpToken+"\n", 0o600))
	installed := m.path(builderToken)
	if strings.TrimSpace(read(t, installed)) != madeUpToken {
		t.Fatalf("the token was not installed at %s", installed)
	}
	os.Remove(m.log)

	for _, args := range [][]string{{"--builder-token", installed}, {"--dry-run", "--builder-token", installed}} {
		out := m.refused("setup", args...)
		if !strings.Contains(out, "is where the set-up keeps the Builder's token") {
			t.Errorf("setup %v does not say why:\n%s", args, out)
		}
		if strings.Contains(out, "setup: remove ") || strings.Contains(out, "would remove ") {
			t.Errorf("setup %v went on to remove it:\n%s", args, out)
		}
		if strings.Contains(out, madeUpToken) {
			t.Errorf("setup %v shows the token:\n%s", args, out)
		}
	}
	if got := read(t, installed); got != madeUpToken+"\n" || mode(t, installed) != 0o600 {
		t.Fatalf("the Builder's token is %d bytes at %v after the set-up was given it as its own source", len(got), mode(t, installed))
	}
	if strings.Contains(m.callsOrNone(), "bootout") {
		t.Errorf("a refused run stopped something:\n%s", m.callsOrNone())
	}
}

// The scratch folder is root's own. A link at its name is refused: root
// would hand every build's folder to whatever the link names.
func TestSetupRefusesALinkAtTheBuildersScratchFolder(t *testing.T) {
	need(t, "openssl")
	m := builderMini(t)
	k := newKey(t)
	v := m.victim()
	plant(t, m.path(builderIO), v.dir)

	out := m.refused("setup", "--release-key", k.signers, "--container-user", account(t))

	v.intact("a link at the Builder's scratch folder")
	saysLink(t, out, m.path(builderIO))
	if exists(m.path("Library/LaunchDaemons/" + builderLabel + ".plist")) {
		t.Errorf("the Builder's LaunchDaemon file was installed all the same:\n%s", out)
	}
}

// The watch runs as root and tells "Agents" through a log the lab account
// owns: a link planted there is never written through.
func TestTheWatchWritesThroughNoLinkPlantedAtTheLog(t *testing.T) {
	m := builderMini(t)
	v := m.victim()
	user := account(t)
	m.ok("container", "watch", user)
	plant(t, m.path("var/lab/log/notify.log"), v.file)
	m.kill()

	out := m.ok("container", "watch", user)

	v.intact("a link at the log of \"Agents\"")
	if !m.serverUp() {
		t.Error("the server was not started again")
	}
	saysLink(t, out, m.path("var/lab/log/notify.log"))
	if !strings.Contains(out, "found stopped") || !strings.Contains(out, "that line was not sent") {
		t.Errorf("the watch's own log does not carry the lines:\n%s", out)
	}
	if n := strings.Count(m.calls(), "as-lab "); n != 2 {
		t.Errorf("%d lines were handed to the lab account, want 2:\n%s", n, m.calls())
	}
}
