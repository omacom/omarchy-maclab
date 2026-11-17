package minitest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// The guest's restic: enough of it for the set-up and the pruning. It
// reaches the guest as its pin does, as a download.
const guestRestic = `#!/bin/bash
echo "restic $* [repository=${RESTIC_REPOSITORY:-} password-file=${RESTIC_PASSWORD_FILE:-}]" >> "$STUB_LOG"
[[ -s ${RESTIC_PASSWORD_FILE:-} ]] || { echo "Fatal: no password" >&2; exit 1; }
case "$1" in
  init) mkdir -p "$RESTIC_REPOSITORY" && echo made > "$RESTIC_REPOSITORY/config" ;;
  key) [[ -s $4 ]] || exit 1 ;;
  snapshots) cat "$STUB_STATE/snapshots.json" 2>/dev/null || echo '[]' ;;
  forget) [[ ! -f $STUB_STATE/forget.fails ]] || exit 1 ;;
esac
`

const guestStubs = `#!/bin/bash
me="$(basename "$0")"
echo "$me $*" >> "$STUB_LOG"
case "$me" in
  dpkg) grep -qx "$2" "$STUB_STATE/packages" 2>/dev/null ;;
  apt-get) for p in "$@"; do echo "$p" >> "$STUB_STATE/packages"; done ;;
  systemctl)
    # As systemd's: is-active of several units passes when any one is active.
    verb="$1"
    shift
    [[ ${1:-} != --quiet ]] || shift
    case "$verb" in
      is-active) for u in "$@"; do [[ ! -f $STUB_STATE/active.$u ]] || exit 0; done; exit 3 ;;
      reload|reload-or-restart|restart)
        for u in "$@"; do
          [[ ! -f $STUB_STATE/fails.$verb.$u ]] || { echo "Job for $u.service failed because the control process exited with error code." >&2; exit 1; }
          touch "$STUB_STATE/active.$u"
        done ;;
    esac ;;
  sshd) [[ ! -f $STUB_STATE/sshd.refuses ]] || { echo "/etc/ssh/sshd_config line 3: Bad configuration option: Nonsense" >&2; exit 255; } ;;
  htpasswd) cat > /dev/null; echo "mini:\$2y\$05\$made-up" > "$4" ;;
esac
`

type guest struct {
	t                  *testing.T
	root, dir, log     string
	state              string
	env                []string
	pins               *httptest.Server
	resticSum, restSum string
}

func (g *guest) path(p string) string { return filepath.Join(g.root, p) }

func newGuest(t *testing.T) *guest {
	t.Helper()
	need(t, "bash", "curl", "jq", "openssl", "ssh-keygen", "bzip2", "tar")
	base := t.TempDir()
	g := &guest{t: t, root: filepath.Join(base, "r"), dir: filepath.Join(repo(t), "scripts", "mini", "guest"), log: filepath.Join(base, "stubs.log"), state: filepath.Join(base, "state")}
	os.MkdirAll(g.state, 0o755)
	for _, name := range []string{"dpkg", "apt-get", "systemctl", "htpasswd", "sshd", "useradd"} {
		write(t, filepath.Join(base, "stubs", name), guestStubs, 0o755)
	}
	// Debian makes a host key of each kind; Litestream on the mini pins the ed25519 one.
	os.MkdirAll(g.path("etc/ssh"), 0o755)
	for _, kind := range []string{"ed25519", "ecdsa"} {
		if out, err := exec.Command("ssh-keygen", "-q", "-t", kind, "-N", "", "-C", "root@lab-backup", "-f", g.path("etc/ssh/ssh_host_"+kind+"_key")).CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
	}

	// The two pins, as GitHub serves them: restic as .bz2, rest-server in a .tar.gz.
	served := filepath.Join(base, "served")
	write(t, served+"/restic_linux_amd64", guestRestic, 0o755)
	if out, err := exec.Command("bzip2", served+"/restic_linux_amd64").CombinedOutput(); err != nil {
		t.Fatalf("bzip2: %v\n%s", err, out)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	rest := "#!/bin/sh\necho rest-server\n"
	tw.WriteHeader(&tar.Header{Name: "rest-server_linux_amd64/rest-server", Mode: 0o755, Size: int64(len(rest))})
	tw.Write([]byte(rest))
	tw.Close()
	gz.Close()
	write(t, served+"/rest-server_linux_amd64.tar.gz", buf.String(), 0o644)
	g.pins = httptest.NewServer(http.FileServer(http.Dir(served)))
	t.Cleanup(g.pins.Close)
	sumOf := func(f string) string {
		b, err := os.ReadFile(filepath.Join(served, f))
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(b))
	}
	g.resticSum, g.restSum = sumOf("restic_linux_amd64.bz2"), sumOf("rest-server_linux_amd64.tar.gz")
	g.pinned(g.resticSum)

	g.env = append(os.Environ(),
		"LAB_GUEST_ROOT="+g.root,
		"LAB_GUEST_PINS="+filepath.Join(g.state, "pins"),
		"PATH="+filepath.Join(base, "stubs")+":"+g.path("usr/local/bin")+":"+os.Getenv("PATH"),
		"STUB_LOG="+g.log,
		"STUB_STATE="+g.state,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"MACLAB_MINI_ADDRESS=192.168.1.20",
	)
	return g
}

func (g *guest) pinned(resticSum string) {
	write(g.t, filepath.Join(g.state, "pins"), fmt.Sprintf("restic 9.9 %s %s/restic_linux_amd64.bz2\nrest-server 8.8 %s %s/rest-server_linux_amd64.tar.gz\n", resticSum, g.pins.URL, g.restSum, g.pins.URL), 0o644)
}

func (g *guest) run(script string, args ...string) (string, int) {
	g.t.Helper()
	cmd := exec.Command(filepath.Join(g.dir, script), args...)
	cmd.Env = g.env
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		g.t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return string(out), 0
}

func (g *guest) ok(script string, args ...string) string {
	g.t.Helper()
	out, code := g.run(script, args...)
	if code != 0 {
		g.t.Fatalf("%s %s: exit %d\n%s", script, strings.Join(args, " "), code, out)
	}
	return out
}

func (g *guest) calls() string { return read(g.t, g.log) }

func (g *guest) status(job string) []string {
	return strings.Fields(read(g.t, g.path("srv/lab-backup/sftp/status/"+job)))
}

func TestTheGuestsSetUp(t *testing.T) {
	g := newGuest(t)
	out := g.ok("setup", "--dry-run")
	for _, want := range []string{
		"would install from Debian: openssh-server git curl jq openssl apache2-utils bzip2 ca-certificates",
		"would install restic 9.9 at its pin",
		"would install rest-server 8.8 at its pin",
		"would make the mini's restic repository at " + g.path("srv/lab-backup/restic/mini"),
		"would make the mini's keys and leave them in " + g.path("root/for-the-mini"),
		"would write " + g.path("etc/ssh/sshd_config.d/lab-backup.conf"),
		"would write " + g.path("etc/systemd/system/lab-backup-rest.service"),
		"changes would be made; nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run does not say %q:\n%s", want, out)
		}
	}
	if exists(g.path("srv")) || exists(g.path("root")) || exists(g.path("etc/lab-backup")) || strings.Contains(g.calls(), "apt-get") {
		t.Fatalf("the dry run changed something:\n%s", g.calls())
	}

	out = g.ok("setup")
	unit := read(t, g.path("etc/systemd/system/lab-backup-rest.service"))
	for _, want := range []string{
		"ExecStart=/usr/local/bin/rest-server --path /srv/lab-backup/restic --listen :8000 --append-only --private-repos --tls --tls-cert /etc/lab-backup/tls.crt --tls-key /etc/lab-backup/tls.key --htpasswd-file /etc/lab-backup/htpasswd\n",
		"IPAddressDeny=any\nIPAddressAllow=192.168.1.20\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("rest-server's unit lacks %q:\n%s", want, unit)
		}
	}
	sshd := read(t, g.path("etc/ssh/sshd_config.d/lab-backup.conf"))
	for _, want := range []string{"ChrootDirectory /srv/lab-backup/sftp\n", "ForceCommand internal-sftp\n", "AuthorizedKeysFile /etc/lab-backup/litestream.authorized_keys\n", "PasswordAuthentication no\n", "AllowTcpForwarding no\n"} {
		if !strings.Contains(sshd, want) {
			t.Errorf("the SFTP account's rules lack %q:\n%s", want, sshd)
		}
	}
	for timer, at := range map[string]string{"prune": "04:30:00", "mirror": "03:30:00", "trim": "05:00:00"} {
		if !strings.Contains(read(t, g.path("etc/systemd/system/lab-backup-"+timer+".timer")), "OnCalendar=*-*-* "+at+" Australia/Brisbane\n") ||
			!strings.Contains(read(t, g.path("etc/systemd/system/lab-backup-"+timer+".service")), "ExecStart=/usr/local/lib/lab-backup/"+timer+"\n") {
			t.Errorf("the nightly %s is not at %s", timer, at)
		}
		if read(t, g.path("usr/local/lib/lab-backup/"+timer)) != read(t, filepath.Join(g.dir, timer)) {
			t.Errorf("the %s script is not installed", timer)
		}
	}

	// What waits for the mini is what the mini's set-up asks for, no more.
	entries, _ := os.ReadDir(g.path("root/for-the-mini"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if info, _ := e.Info(); info.Mode().Perm() != 0o600 {
			t.Errorf("%s waits for the mini with mode %v", e.Name(), info.Mode())
		}
	}
	wanted := strings.Fields(shell(t, "BACKUP_KEYS_HOUSE"))
	if len(names) != len(wanted) {
		t.Errorf("for the mini: %v, want %v", names, wanted)
	}
	for _, name := range wanted {
		if read(t, g.path("root/for-the-mini/"+name)) == "" {
			t.Errorf("nothing for the mini named %s", name)
		}
	}
	if info, err := os.Stat(g.path("root/for-the-mini")); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the folder for the mini: %v", info.Mode())
	}
	if got := read(t, g.path("root/for-the-mini/host-key")); !strings.HasPrefix(got, "ssh-ed25519 ") || got != strings.Join(strings.Fields(read(t, g.path("etc/ssh/ssh_host_ed25519_key.pub")))[:2], " ")+"\n" {
		t.Errorf("the host key for the mini is %q", got)
	}
	minis, guests := read(t, g.path("root/for-the-mini/restic-password")), read(t, g.path("etc/lab-backup/restic-password"))
	if minis == guests || len(minis) < 40 || len(guests) < 40 {
		t.Error("the mini and the guest do not each have a restic key of their own")
	}
	for _, s := range []string{minis, guests, read(t, g.path("root/for-the-mini/rest-password"))} {
		if strings.Contains(out, strings.TrimSpace(s)) || strings.Contains(g.calls(), strings.TrimSpace(s)) {
			t.Error("a secret was printed or given as an argument")
		}
	}
	public, err := exec.Command("ssh-keygen", "-y", "-f", g.path("root/for-the-mini/litestream-sftp-key")).Output()
	if authorized := strings.Fields(read(t, g.path("etc/lab-backup/litestream.authorized_keys"))); err != nil || len(authorized) < 2 || !strings.Contains(string(public), authorized[1]) {
		t.Errorf("the SFTP account does not take the key made for the mini: %v", err)
	}
	if cert, err := exec.Command("openssl", "x509", "-in", g.path("etc/lab-backup/tls.crt"), "-noout", "-text").Output(); err != nil || !strings.Contains(string(cert), "DNS:lab-backup") {
		t.Errorf("rest-server's certificate is not for lab-backup: %v", err)
	}
	if read(t, g.path("root/for-the-mini/guest.crt")) != read(t, g.path("etc/lab-backup/tls.crt")) {
		t.Error("the mini is not given rest-server's certificate")
	}
	repository := g.path("srv/lab-backup/restic/mini")
	for _, want := range []string{
		"restic init [repository=" + repository + " password-file=" + g.path("etc/lab-backup/restic-password") + "]",
		"restic key add --new-password-file " + g.path("root/for-the-mini/restic-password") + " --user mini --host lab-mini [repository=" + repository,
		"htpasswd -B -i -c ",
		"sshd -t",
		"systemctl reload-or-restart ssh",
		"systemctl enable --quiet lab-backup-rest.service lab-backup-prune.timer lab-backup-mirror.timer lab-backup-trim.timer",
	} {
		if !strings.Contains(g.calls(), want) {
			t.Errorf("the set-up did not run %q:\n%s", want, g.calls())
		}
	}

	before := g.calls()
	out = g.ok("setup")
	if !strings.Contains(out, "setup: nothing to change") || !strings.Contains(out, "carry that folder to the mini") {
		t.Errorf("a second run:\n%s", out)
	}
	if after := strings.TrimPrefix(g.calls(), before); strings.Contains(after, "restic ") || strings.Contains(after, "restart") {
		t.Errorf("a second run did more than look:\n%s", after)
	}
	if read(t, g.path("root/for-the-mini/restic-password")) != minis {
		t.Error("a second run made the mini's keys again")
	}

	// The mini's set-up takes the folder as the guest left it.
	m, k := withBackups(t)
	carried := t.TempDir()
	for _, name := range wanted {
		write(t, filepath.Join(carried, name), read(t, g.path("root/for-the-mini/"+name)), 0o600)
	}
	m.ok("setup", "--release-key", k.signers, "--backup-guest", "192.168.0.186", "--backup-keys", carried)
	if !m.daemonLoaded(litestreamLabel) {
		t.Error("with the guest's folder the mini did not start Litestream")
	}
}

// On the first real guest (Debian 13, OpenSSH 10.0) reloading ssh failed,
// and the set-up stopped there with rest-server never started.
func TestTheGuestStartsItsBackupsWhateverSSHDoes(t *testing.T) {
	units := []string{"lab-backup-rest.service", "lab-backup-prune.timer", "lab-backup-mirror.timer", "lab-backup-trim.timer"}
	started := func(g *guest) {
		t.Helper()
		calls := g.calls()
		if !strings.Contains(calls, "systemctl enable --quiet "+strings.Join(units, " ")) {
			t.Errorf("rest-server and the timers were not enabled:\n%s", calls)
		}
		for _, u := range units {
			if !exists(filepath.Join(g.state, "active."+u)) {
				t.Errorf("%s was not started:\n%s", u, calls)
			}
		}
	}

	// Reload fails, restart works: the set-up goes on.
	g := newGuest(t)
	write(t, filepath.Join(g.state, "fails.reload.ssh"), "", 0o644)
	write(t, filepath.Join(g.state, "fails.reload-or-restart.ssh"), "", 0o644)
	g.ok("setup")
	started(g)
	if !strings.Contains(g.calls(), "systemctl restart ssh") {
		t.Errorf("a failed reload of ssh was not followed by a restart:\n%s", g.calls())
	}

	// Neither works: the backups are started all the same, and the set-up fails saying so.
	g = newGuest(t)
	for _, verb := range []string{"reload", "reload-or-restart", "restart"} {
		write(t, filepath.Join(g.state, "fails."+verb+".ssh"), "", 0o644)
	}
	out, code := g.run("setup")
	started(g)
	if code == 0 || !strings.Contains(out, "setup: ssh did not take "+g.path("etc/ssh/sshd_config.d/lab-backup.conf")) || !strings.Contains(out, "rest-server and the three nightly timers are started") {
		t.Errorf("ssh that neither reloads nor restarts, exit %d:\n%s", code, out)
	}

	// sshd refuses its configuration: said plainly, before ssh is touched, after the backups start.
	g = newGuest(t)
	g.ok("setup")
	os.Remove(g.path("etc/ssh/sshd_config.d/lab-backup.conf"))
	write(t, filepath.Join(g.state, "sshd.refuses"), "", 0o644)
	before := g.calls()
	out, code = g.run("setup")
	if after := strings.TrimPrefix(g.calls(), before); code == 0 || !strings.Contains(out, "setup: sshd does not take its configuration") || exists(g.path("etc/ssh/sshd_config.d/lab-backup.conf")) || strings.Contains(after, "systemctl reload") || strings.Contains(after, "systemctl restart ssh") {
		t.Errorf("a configuration sshd refuses, exit %d:\n%s\n%s", code, out, after)
	}
	os.Remove(filepath.Join(g.state, "sshd.refuses"))

	// A run after one that stopped early starts whatever is down, though nothing else changed.
	g = newGuest(t)
	g.ok("setup")
	os.Remove(filepath.Join(g.state, "active.lab-backup-rest.service"))
	out = g.ok("setup")
	if !strings.Contains(out, "start what is not running: lab-backup-rest.service\n") || !exists(filepath.Join(g.state, "active.lab-backup-rest.service")) {
		t.Errorf("rest-server down and the timers up, a second run:\n%s", out)
	}
}

// Litestream's SSH client takes only the key it pins, and asks for ECDSA
// before ed25519: the guest's SSH server offers the ed25519 key alone.
func TestTheGuestOffersOnlyTheHostKeyTheMiniPins(t *testing.T) {
	g := newGuest(t)
	g.ok("setup")
	conf := read(t, g.path("etc/ssh/sshd_config.d/lab-backup.conf"))
	key, match := strings.Index(conf, "\nHostKey /etc/ssh/ssh_host_ed25519_key\n"), strings.Index(conf, "\nMatch User ")
	if key < 0 || match < 0 || key > match || strings.Count(conf, "HostKey ") != 1 {
		t.Errorf("lab-backup.conf does not name the ed25519 host key alone, before its Match block:\n%s", conf)
	}
	public, err := exec.Command("ssh-keygen", "-y", "-f", g.path("etc/ssh/ssh_host_ed25519_key")).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := read(t, g.path("root/for-the-mini/host-key")), strings.Join(strings.Fields(string(public))[:2], " ")+"\n"; got != want {
		t.Errorf("the mini is given %q, and the server offers %q", got, want)
	}

	// A key of another kind where the ed25519 one should be is not given to the mini.
	g = newGuest(t)
	write(t, g.path("etc/ssh/ssh_host_ed25519_key"), read(t, g.path("etc/ssh/ssh_host_ecdsa_key")), 0o600)
	if out, code := g.run("setup"); code == 0 || !strings.Contains(out, "is not an ed25519 key") || exists(g.path("root/for-the-mini/host-key")) {
		t.Errorf("an ECDSA key in the ed25519 key's place, exit %d:\n%s", code, out)
	}
}

func TestTheGuestInstallsNothingThatIsNotItsPin(t *testing.T) {
	g := newGuest(t)
	g.pinned(strings.Repeat("0", 64))
	out, code := g.run("setup")
	if code == 0 || !strings.Contains(out, "restic 9.9: the download's checksum is not the pinned one") || exists(g.path("usr/local/bin/restic")) {
		t.Errorf("a download that is not the pin:\n%s", out)
	}
	if out, code := g.run("setup", "--mini", "the-mini"); code == 0 || !strings.Contains(out, "is not an address") {
		t.Errorf("a name for the mini's address:\n%s", out)
	}
	// The mini's address is named by its owner: the repository names none.
	named := g.env
	g.env = append(g.env, "MACLAB_MINI_ADDRESS=")
	if out, code := g.run("setup", "--dry-run"); code == 0 || !strings.Contains(out, "name the mini's address") {
		t.Errorf("no address for the mini:\n%s", out)
	}
	g.env = named
	// An account exists before a folder is given to it.
	if !strings.Contains(read(t, filepath.Join(g.dir, "setup")), "\npackages\naccounts\nfolders\npins\n") {
		t.Error("the guest's set-up does not make its accounts before their folders")
	}
	// The real pins: a version, a checksum of 64 hex digits, a release file on GitHub.
	text := read(t, filepath.Join(g.dir, "setup"))
	for _, tool := range []string{"restic", "rest-server"} {
		if !regexp.MustCompile(`(?m)^(PINS=")?` + tool + ` [0-9.]+ [0-9a-f]{64} https://github\.com/restic/` + tool + `/releases/download/v[0-9.]+/` + tool + `_[0-9.]+_linux_amd64\.(bz2|tar\.gz)"?$`).MatchString(text) {
			t.Errorf("the guest's set-up has no pin for %s", tool)
		}
	}
}

func TestTheGuestKeepsTheHouseCopyToTheRetention(t *testing.T) {
	g := newGuest(t)
	g.ok("setup")
	day := func(ago int) string {
		return time.Now().UTC().AddDate(0, 0, -ago).Format("2006-01-02T15:04:05.000000000+10:00")
	}
	write(t, filepath.Join(g.state, "snapshots.json"), `[{"id":"aaaa1111","time":"`+day(200)+`"},{"id":"bbbb2222","time":"`+day(150)+`"},{"id":"cccc3333","time":"`+day(1)+`"}]`, 0o644)
	old, fresh := g.path("srv/lab-backup/sftp/db/labd/ltx/0/old.ltx"), g.path("srv/lab-backup/sftp/db/labd/ltx/0/fresh.ltx")
	write(t, old, "x", 0o600)
	write(t, fresh, "x", 0o600)
	then := time.Now().AddDate(0, 0, -200)
	os.Chtimes(old, then, then)
	// Litestream's folder is the SFTP account's alone: the restic account,
	// which prunes, cannot enter it. A folder nobody enters stands in for it.
	db := g.path("srv/lab-backup/sftp/db")
	shut := func(mode os.FileMode) {
		if err := os.Chmod(db, mode); err != nil {
			t.Fatal(err)
		}
	}
	shut(0)
	t.Cleanup(func() { os.Chmod(db, 0o700) })
	os.Remove(g.log)

	g.ok("prune")
	opened := " [repository=" + g.path("srv/lab-backup/restic/mini") + " password-file=" + g.path("etc/lab-backup/restic-password") + "]\n"
	want := "restic forget --group-by host --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune" + opened +
		"restic snapshots --json" + opened +
		"restic forget --prune aaaa1111" + opened
	if g.calls() != want {
		t.Errorf("the pruning ran\n%s\nwant\n%s", g.calls(), want)
	}
	if s := g.status("prune"); len(s) != 3 || s[0] != "ok" {
		t.Errorf("the pruning's own line is %v", s)
	}

	// Litestream's files are kept to six months by a job of the SFTP
	// account's own, which says how it went on a line of its own.
	if os.Getuid() != 0 {
		if out, code := g.run("trim"); code == 0 {
			t.Errorf("a clean-up that could not enter Litestream's folder passed:\n%s", out)
		}
		if s := strings.Join(g.status("trim"), " "); !strings.HasPrefix(s, "failed ") || !strings.HasSuffix(s, " removing Litestream files older than six months") {
			t.Errorf("after a failed clean-up the line is %q", s)
		}
	}
	shut(0o700)
	if !exists(old) {
		t.Fatal("the pruning, or a failed clean-up, removed a Litestream file")
	}
	g.ok("trim")
	if exists(old) || !exists(fresh) {
		t.Error("a Litestream file older than six months stays, or a fresh one went")
	}
	if s := g.status("trim"); len(s) != 3 || s[0] != "ok" {
		t.Errorf("the clean-up's own line is %v", s)
	}
	if s := g.status("prune"); len(s) != 3 || s[0] != "ok" {
		t.Errorf("the clean-up wrote over the pruning's line: %v", s)
	}
	account := strings.TrimSpace(must(t, nil, "id", "-un"))
	if unit := read(t, g.path("etc/systemd/system/lab-backup-trim.service")); !strings.Contains(unit, "\nUser="+account+"\nExecStart=/usr/local/lib/lab-backup/trim\n") {
		t.Errorf("the clean-up does not run as the SFTP account:\n%s", unit)
	}

	write(t, filepath.Join(g.state, "forget.fails"), "", 0o644)
	if out, code := g.run("prune"); code == 0 {
		t.Errorf("a failed restic forget passed:\n%s", out)
	}
	if s := strings.Join(g.status("prune"), " "); !strings.HasPrefix(s, "failed ") || !strings.HasSuffix(s, " restic forget by the keep rules") {
		t.Errorf("after a failed forget the line is %q", s)
	}

	// The mini and the guest keep by the same rules.
	rule := regexp.MustCompile(`(?m)^KEEP=\(--keep-daily 7 --keep-weekly 4 --keep-monthly 6\)\n(#.*\n)*KEEP_DAYS=183$`)
	for _, f := range []string{"scripts/mini/backup", "scripts/mini/guest/lib.sh"} {
		if !rule.MatchString(read(t, filepath.Join(repo(t), f))) {
			t.Errorf("%s does not keep 7 daily, 4 weekly, 6 monthly and at most 183 days", f)
		}
	}
}

// hub plays GitHub for the mirror: a repository to clone, the list of
// releases and their files.
type hub struct {
	t        *testing.T
	repo     string
	srv      *httptest.Server
	mu       sync.Mutex
	releases []map[string]any
	fetched  map[string]int
	down     bool
}

func newHub(t *testing.T, g *guest) *hub {
	t.Helper()
	need(t, "git")
	h := &hub{t: t, repo: filepath.Join(t.TempDir(), "omarchy-maclab"), fetched: map[string]int{}}
	h.git("init", "-q", "-b", "main", h.repo)
	h.commit("one")
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case h.down:
			http.Error(w, "no", http.StatusBadGateway)
		case r.URL.Path == "/api":
			if r.URL.Query().Get("page") != "1" {
				w.Write([]byte("[]"))
				return
			}
			json.NewEncoder(w).Encode(h.releases)
		default:
			h.fetched[r.URL.Path]++
			w.Write([]byte("the file " + r.URL.Path + "\n"))
		}
	}))
	t.Cleanup(h.srv.Close)
	g.env = append(g.env, "LAB_MIRROR_REPOSITORY="+h.repo, "LAB_MIRROR_API="+h.srv.URL+"/api", "LAB_MIRROR_FILES="+h.srv.URL+"/download/")
	return h
}

func (h *hub) git(args ...string) string {
	h.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *hub) commit(text string) {
	write(h.t, h.repo+"/file", text+"\n", 0o644)
	h.git("-C", h.repo, "add", "file")
	h.git("-C", h.repo, "commit", "-q", "-m", text)
}

// release adds a Lab release with files of these names.
func (h *hub) release(tag string, names ...string) {
	h.git("-C", h.repo, "tag", tag)
	var assets []map[string]any
	for _, name := range names {
		path := "/download/" + tag + "/" + name
		assets = append(assets, map[string]any{"name": name, "size": len("the file " + path + "\n"), "browser_download_url": h.srv.URL + path})
	}
	h.mu.Lock()
	h.releases = append(h.releases, map[string]any{"tag_name": tag, "assets": assets})
	h.mu.Unlock()
}

func TestTheOwnersCopyIsAMirrorOfTheRepositoryAndOfEveryReleasesFiles(t *testing.T) {
	g := newGuest(t)
	g.ok("setup")
	h := newHub(t, g)
	h.release("v0.2.0-rc.1", "lab-server-darwin-arm64.tar.gz", "lab-server-darwin-arm64.tar.gz.sig")
	g.ok("mirror")

	mirror := g.path("srv/lab-backup/mirror")
	if got := h.git("--git-dir", mirror+"/omarchy-maclab.git", "rev-parse", "v0.2.0-rc.1"); got != h.git("-C", h.repo, "rev-parse", "HEAD") {
		t.Errorf("the mirror's v0.2.0-rc.1 is %s", got)
	}
	for _, f := range []string{"lab-server-darwin-arm64.tar.gz", "lab-server-darwin-arm64.tar.gz.sig"} {
		if got := read(t, mirror+"/releases/v0.2.0-rc.1/"+f); got != "the file /download/v0.2.0-rc.1/"+f+"\n" {
			t.Errorf("%s in the mirror is %q", f, got)
		}
	}
	if s := g.status("mirror"); len(s) != 3 || s[0] != "ok" {
		t.Errorf("the mirror's own line is %v", s)
	}

	// The next night: a new commit, a new release; what is there is not fetched again.
	h.commit("two")
	h.release("v0.2.0", "lab-server-darwin-arm64.tar.gz")
	g.ok("mirror")
	if got := h.git("--git-dir", mirror+"/omarchy-maclab.git", "rev-parse", "main"); got != h.git("-C", h.repo, "rev-parse", "HEAD") {
		t.Errorf("the mirror's main is %s after a new commit", got)
	}
	h.mu.Lock()
	fetched := maps.Clone(h.fetched)
	h.mu.Unlock()
	if !exists(mirror+"/releases/v0.2.0/lab-server-darwin-arm64.tar.gz") || fetched["/download/v0.2.0-rc.1/lab-server-darwin-arm64.tar.gz"] != 1 {
		t.Errorf("the second night fetched %v", fetched)
	}

	// A file whose name would leave the mirror's folder stops the run.
	h.release("v0.2.1", "../../../../etc/cron.d/x")
	out, code := g.run("mirror")
	if s := strings.Join(g.status("mirror"), " "); code == 0 || !strings.HasPrefix(s, "failed ") || !strings.HasSuffix(s, " a release file whose name is no file name") {
		t.Errorf("a release file named ../..: %q\n%s", s, out)
	}
}

func TestAFailedMirrorWritesItsAlertAndTheMiniSendsIt(t *testing.T) {
	g := newGuest(t)
	g.ok("setup")
	h := newHub(t, g)
	h.release("v0.2.0", "lab-server-darwin-arm64.tar.gz")
	h.mu.Lock()
	h.down = true
	h.mu.Unlock()
	out, code := g.run("mirror")
	line := read(t, g.path("srv/lab-backup/sftp/status/mirror"))
	if s := strings.Fields(line); code == 0 || len(s) < 4 || s[0] != "failed" || !strings.HasSuffix(strings.TrimSpace(line), " the list of Lab releases") {
		t.Fatalf("with GitHub away the mirror's own line is %q\n%s", line, out)
	}

	// The mini reads that line over SFTP on its nightly run and tells the owner.
	b := newBackedUp(t)
	write(t, filepath.Join(b.state, "guest/status/mirror"), line, 0o644)
	b.refused("backup")
	if want := "agents The nightly backup failed: the owner's copy (the nightly mirror) failed on lab-backup: the list of Lab releases."; !strings.Contains(b.told(), want) {
		t.Errorf("the owner is told %q, want %q", b.told(), want)
	}

	// With the repository gone too, the line names it.
	g.env = append(g.env, "LAB_MIRROR_REPOSITORY="+filepath.Join(t.TempDir(), "gone"))
	os.RemoveAll(g.path("srv/lab-backup/mirror/omarchy-maclab.git"))
	g.run("mirror")
	if s := strings.TrimSpace(read(t, g.path("srv/lab-backup/sftp/status/mirror"))); !strings.HasPrefix(s, "failed ") || !strings.HasSuffix(s, " the repository") {
		t.Errorf("with the repository away the line is %q", s)
	}
}

func TestTheGuestsScriptsPassShellcheck(t *testing.T) {
	dir := filepath.Join(repo(t), "scripts", "mini", "guest")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 5 {
		t.Fatalf("scripts/mini/guest: %v, %d files", err, len(entries))
	}
	var files []string
	for _, e := range entries {
		f := filepath.Join(dir, e.Name())
		files = append(files, f)
		if out, err := exec.Command("bash", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %s", e.Name(), out)
		}
		if text := read(t, f); e.Name() != "lib.sh" && !strings.HasPrefix(text, "#!/bin/bash\nset -euo pipefail\n") && !strings.HasPrefix(text, "#!/bin/bash\n#") {
			t.Errorf("%s does not start with bash", e.Name())
		}
		if strings.Contains(read(t, f), "/opt/homebrew") || regexp.MustCompile(`\bbrew\b`).MatchString(read(t, f)) {
			t.Errorf("%s names Homebrew: the guest is Debian", e.Name())
		}
	}
	need(t, "shellcheck")
	cmd := exec.Command("shellcheck", append([]string{"--shell=bash", "--external-sources"}, files...)...)
	cmd.Dir = repo(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}
