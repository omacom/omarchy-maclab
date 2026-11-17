package carrytest

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"

	"github.com/omacom/omarchy-maclab/internal/githubtest"
	"github.com/omacom/omarchy-maclab/internal/oldlabtest"
	"github.com/omacom/omarchy-maclab/internal/oldsite/oldsitetest"
)

// The old site's machine-id key, as the old site derives it, from a secret
// made up here.
func machineIDKey() []byte {
	return pbkdf2.Key([]byte("the old site's secret, made up"), []byte("report machine id from key"), 1000, 64, sha256.New)
}

const oldLink = "https://omarchy-m-testing.org/reports/" + oldsitetest.Verified + "/deletion?token=" + oldsitetest.VerifiedToken

// rehearsal is a mini with a stand-in for GitHub and a dump of a made-up
// old site where the lab account reads it.
func rehearsal(t *testing.T) (m *mini, dump, public string) {
	t.Helper()
	m = newMini(t, oldlabtest.OnOmarchyGPU)
	gh := githubtest.New("id", "secret")
	gh.AddUser(2741, "dhh")
	gh.AddUser(3003, "RyanRHughes")
	fake := httptest.NewServer(gh)
	t.Cleanup(fake.Close)
	m.env = append(m.env, "LAB_GITHUB_URL="+fake.URL, "STUB_LABD="+labd(t), "STUB_DATA="+m.path("var/lab/data"))
	made := oldsitetest.Made(t, machineIDKey()).Dump(t)
	dump = m.path("var/lab/import/old-site.dump")
	write(t, dump, read(t, made), 0o600)
	public = filepath.Join(m.base, "home", "m1-pro.pub")
	write(t, public, oldsitetest.PublicKey("a")+"\n", 0o644)
	return m, dump, public
}

// The machine-id key is asked for, never an argument and never printed, and
// only 128 hex digits become the file.
func TestTheMachineIDKeyIsTypedAtAPromptAndKeptForTheLabAccountAlone(t *testing.T) {
	m, _, _ := rehearsal(t)
	file := m.path("var/lab/secrets/machine-id-key")
	key := hex.EncodeToString(machineIDKey())

	if out, code := m.run(key+"\n", "rehearsal-import", "key", "--dry-run"); code != 0 || !strings.Contains(out, "would ask") || exists(file) {
		t.Fatalf("the dry run: exit %d, %s", code, out)
	}
	for _, typed := range []string{"", "\n", "not a key\n", key[:126] + "\n", key + "00\n", strings.Replace(key, key[:1], "g", 1) + "\n"} {
		if out, code := m.run(typed, "rehearsal-import", "key"); code == 0 || exists(file) || !strings.Contains(out, "Nothing was changed") {
			t.Errorf("typed %d characters that are no key: exit %d, %s", len(typed), code, out)
		}
	}
	if out, code := m.run("", "rehearsal-import", "key", key); code != 2 || exists(file) || strings.Contains(out, key) {
		t.Errorf("the key as an argument: exit %d, %s", code, out)
	}
	os.Remove(m.log)
	out, code := m.run(key+"\n", "rehearsal-import", "key")
	if code != 0 || !strings.Contains(out, "off-machine copy") {
		t.Fatalf("exit %d, %s", code, out)
	}
	if st, err := os.Stat(file); err != nil || st.Mode().Perm() != 0o600 || read(t, file) != key+"\n" {
		t.Errorf("the key's file: %v", err)
	}
	if strings.Contains(out+m.calls(), key) {
		t.Error("the key was printed or passed as an argument")
	}
	if !strings.Contains(m.calls(), "launchctl kickstart -k system/"+labdLabel) {
		t.Errorf("labd was not restarted to read the key:\n%s", m.calls())
	}
	os.Remove(m.log)
	if out, code := m.run(key+"\n", "rehearsal-import", "key"); code != 0 || !strings.Contains(out, "in place already") || strings.Contains(m.calls(), "kickstart") {
		t.Errorf("the same key again: exit %d, %s", code, out)
	}

	// An open lab keeps the key it has.
	plist := m.path("Library/LaunchDaemons/" + labdLabel + ".plist")
	write(t, plist, strings.Replace(read(t, plist), "    <string>--preview</string>\n", "", 1), 0o644)
	other := strings.Repeat("ab", 64)
	if out, code := m.run(other+"\n", "rehearsal-import", "key"); code == 0 || !strings.Contains(out, "the lab is open") || read(t, file) != key+"\n" {
		t.Errorf("another key for an open lab: exit %d, %s", code, out)
	}
}

// The rehearsal: reports.db emptied, filled from the dump, the three checks
// passed, and lines for docs/acceptance that hold counts only. It can be
// run again.
func TestTheRehearsalImportFillsAnEmptiedReportsDBAndPassesItsThreeChecks(t *testing.T) {
	m, dump, public := rehearsal(t)
	key := hex.EncodeToString(machineIDKey())
	reports := m.path("var/lab/data/reports.db")

	if out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dump", dump); code == 0 || !strings.Contains(out, "rehearsal-import key") {
		t.Fatalf("with no machine-id key: exit %d, %s", code, out)
	}
	if strings.Contains(m.calls(), "launchctl bootout") {
		t.Fatalf("a refused run stopped a daemon:\n%s", m.calls())
	}
	if out, code := m.run(key+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	if out, code := m.run("", "rehearsal-import", "run", "--dry-run", "--public-key", public, "--dump", dump); code != 0 || !strings.Contains(out, "the old site is not read") || !strings.Contains(out, "would stop labd") {
		t.Fatalf("the dry run: exit %d, %s", code, out)
	}
	if out, code := m.run("", "rehearsal-import", "run", "--dry-run", "--public-key", public); code != 0 || !strings.Contains(out, "ask for the address of the old site's Postgres") ||
		!strings.Contains(out, "show its certificate chain") || !strings.Contains(out, "and its CA file") {
		t.Fatalf("the dry run with no dump: exit %d, %s", code, out)
	}
	if n := count(t, reports, `SELECT count(*) FROM reports`); n != 0 {
		t.Fatalf("a dry run imported %d reports", n)
	}

	for round := 1; round <= 2; round++ {
		os.Remove(m.log)
		out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dump", dump)
		if code != 0 {
			t.Fatalf("round %d: exit %d\n%s", round, code, out)
		}
		for _, line := range []string{"The row counts match.", "That Mac's machine id, recomputed from its public key, is on ", "The old site's link deleted its report on the lab", "the import and its three checks passed"} {
			if !strings.Contains(out, line) {
				t.Errorf("round %d did not say %q:\n%s", round, line, out)
			}
		}
		// 20 reports in the dump, and the check deleted one.
		if n := count(t, reports, `SELECT count(*) FROM reports`); n != 19 {
			t.Errorf("round %d: reports.db holds %d reports, want 19", round, n)
		}
		_, lines, ok := strings.Cut(out, "---- rehearsal import, ")
		if !ok || !strings.Contains(lines, "Brisbane") || !strings.Contains(lines, "reports: 20 at the old site, 20 in the dump, 20 in reports.db") {
			t.Fatalf("round %d: no lines for docs/acceptance:\n%s", round, out)
		}
		for _, never := range []string{oldsitetest.Verified, oldsitetest.Hidden, oldsitetest.Twice, oldsitetest.VerifiedToken, key, dump, "demo-tester"} {
			if strings.Contains(lines, never) {
				t.Errorf("round %d: the lines for docs/acceptance name %q:\n%s", round, never, lines)
			}
		}
		if strings.Contains(m.calls(), key) || strings.Contains(m.calls(), oldsitetest.VerifiedToken) {
			t.Errorf("round %d: a secret was passed as an argument", round)
		}
		calls := m.calls()
		stop, moved, start, load := strings.Index(calls, "launchctl bootout system/"+labdLabel), strings.Index(calls, "as-lab mv "+reports+" "), strings.Index(calls, "launchctl bootstrap system"), strings.Index(calls, " import load ")
		if stop < 0 || moved < stop || start < moved || load < start {
			t.Errorf("round %d: reports.db was not emptied with labd stopped, before the import:\n%s", round, calls)
		}
		if !m.loaded(labdLabel) || !m.loaded(litestreamLabel) {
			t.Errorf("round %d: labd and Litestream are not running again", round)
		}
		// What reports.db held before is kept, once.
		kept := m.path("var/lab/import/reports-before/reports.db")
		if want := map[int]int{1: 0, 2: 19}[round]; count(t, kept, `SELECT count(*) FROM reports`) != want {
			t.Errorf("round %d: the copy kept aside does not hold the %d reports of before", round, want)
		}
	}

	// An open lab's reports are never emptied.
	plist := m.path("Library/LaunchDaemons/" + labdLabel + ".plist")
	write(t, plist, strings.Replace(read(t, plist), "    <string>--preview</string>\n", "", 1), 0o644)
	os.Remove(m.log)
	if out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dump", dump); code == 0 || !strings.Contains(out, "the lab is open") {
		t.Errorf("a rehearsal on an open lab: exit %d, %s", code, out)
	}
	if n := count(t, reports, `SELECT count(*) FROM reports`); n != 19 || strings.Contains(m.calls(), "bootout") {
		t.Errorf("an open lab's reports.db was touched: %d reports\n%s", n, m.calls())
	}
}

// A check that fails stops the run there, with a status that says so.
func TestTheRehearsalStopsAtTheFirstCheckThatFails(t *testing.T) {
	m, dump, public := rehearsal(t)
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	wrong := "https://omarchy-m-testing.org/reports/" + oldsitetest.Verified + "/deletion?token=not-the-token"
	out, code := m.run(wrong+"\n", "rehearsal-import", "run", "--public-key", public, "--dump", dump)
	if code == 0 || strings.Contains(out, "three checks passed") || strings.Contains(out, "---- rehearsal import") {
		t.Errorf("with a link that deletes nothing: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "The row counts match.") {
		t.Errorf("the checks before it did not run:\n%s", out)
	}
	other := filepath.Join(m.base, "home", "another.pub")
	write(t, other, oldsitetest.PublicKey("never seen")+"\n", 0o644)
	out, code = m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", other, "--dump", dump)
	if code == 0 || strings.Contains(out, "deleted its report") {
		t.Errorf("with a Machine public key of no report: exit %d\n%s", code, out)
	}
}

// openingAsLab plays the lab account and, as the import writes the Machine
// public key down, opens the lab from a second terminal.
const openingAsLab = `#!/bin/bash
echo "as-lab $*" >> "$STUB_LOG"
if [[ "$*" == *machine.pub* && ! -f $STUB_STATE/opened ]]; then
  : > "$STUB_STATE/opened"
  curl -sS --unix-socket "$LAB_SYSROOT/var/lab/run/ops.sock" -X POST "http://labd/open?m-team=14" >> "$STUB_LOG" 2>&1
fi
exec "$@"
`

// An opening between the import's first check and its stop of labd: the
// import asks again, and the open lab's reports stay where they are.
func TestAnOpeningDuringTheImportLeavesTheReports(t *testing.T) {
	m, dump, public := rehearsal(t)
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	s := openingOn(t, m, 14)
	stub := filepath.Join(m.base, "opening-as-lab")
	write(t, stub, openingAsLab, 0o755)
	for i, e := range m.env {
		if strings.HasPrefix(e, "LAB_AS_LAB=") {
			m.env[i] = "LAB_AS_LAB=" + stub
		}
	}
	reports := everyFile(t, m.path("var/lab/data"))["reports.db"]
	os.Remove(m.log)
	out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dumped", dump)
	if _, live := s.Live(); !live {
		t.Fatalf("the stand-in did not open the lab: %s", m.calls())
	}
	if code == 0 || !strings.Contains(out, "the lab is open (live ") {
		t.Errorf("the import on a lab opened meanwhile: exit %d, %s", code, out)
	}
	if strings.Contains(m.calls(), "launchctl bootout") || everyFile(t, m.path("var/lab/data"))["reports.db"] != reports || exists(m.path("var/lab/import/reports-before/reports.db")) {
		t.Errorf("the open lab's labd was stopped or its reports.db moved:\n%s", m.calls())
	}
}

// openingInTheImport plays the lab account and, as the import is about to
// take its earlier pass's mark away, starts the opening from a second
// terminal: the mark of today is still there, labd runs in Preview.
const openingInTheImport = `#!/bin/bash
echo "as-lab $*" >> "$STUB_LOG"
if [[ $1 == rm && $3 == */import/imported && ! -f $STUB_STATE/opening.code ]]; then
  : > "$STUB_STATE/opening.code"
  "$STUB_OPEN" --m-team 14 > "$STUB_STATE/opening.out" 2>&1
  echo $? > "$STUB_STATE/opening.code"
fi
exec "$@"
`

// The opening and the import's emptying take one lock on the mini: an
// opening started once the import has passed its last look at the stage
// is refused, the lab stays in Preview and the import goes on to its pass.
func TestAnOpeningStartedWhileTheImportEmptiesTheReportsIsRefused(t *testing.T) {
	m, dump, public := rehearsal(t)
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	s := openingOn(t, m, 14)
	stub := filepath.Join(m.base, "opening-in-the-import")
	write(t, stub, openingInTheImport, 0o755)
	for i, e := range m.env {
		if strings.HasPrefix(e, "LAB_AS_LAB=") {
			m.env[i] = "LAB_AS_LAB=" + stub
		}
	}
	m.env = append(m.env, "STUB_OPEN="+filepath.Join(repo(t), "scripts", "cutover", "open"))
	out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dumped", dump)
	opening := read(t, filepath.Join(m.state, "opening.out"))
	if strings.TrimSpace(read(t, filepath.Join(m.state, "opening.code"))) == "0" || !strings.Contains(opening, "changes the lab now") || !strings.Contains(opening, "Nothing was changed") {
		t.Errorf("the opening during the import was not refused:\n%s", opening)
	}
	if !s.Preview() {
		t.Fatal("the lab opened while the import emptied its reports")
	}
	if code != 0 || !strings.Contains(out, "the import and its three checks passed") {
		t.Errorf("the import: exit %d, %s", code, out)
	}
	if exists(m.path(cutoverLock)) {
		t.Error("the import left its lock behind")
	}
}

// cutoverLock is the mini's lock for the opening and the import.
const cutoverLock = "var/run/org.omarchy-mac.lab.cutover"

// A lock held by a process that runs refuses the opening and the import
// before either changes anything; a lock whose process is gone is taken.
func TestTheCutoversLockHeldElsewhereRefusesAndALeftOneIsTaken(t *testing.T) {
	m, dump, public := rehearsal(t)
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	s := openingOn(t, m, 14)
	mark := m.path("var/lab/import/imported")
	lock := m.path(cutoverLock)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strconv.Itoa(os.Getpid()), lock); err != nil {
		t.Fatal(err)
	}
	reports := everyFile(t, m.path("var/lab/data"))["reports.db"]
	os.Remove(m.log)
	if out := m.refused("open", "--m-team", "14"); !strings.Contains(out, "changes the lab now") {
		t.Errorf("the opening under a held lock: %s", out)
	}
	out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dumped", dump)
	if code == 0 || !strings.Contains(out, "changes the lab now") {
		t.Errorf("the import under a held lock: exit %d, %s", code, out)
	}
	if !s.Preview() || !exists(mark) || strings.Contains(m.calls(), "launchctl bootout") || everyFile(t, m.path("var/lab/data"))["reports.db"] != reports {
		t.Errorf("a refused run changed the lab:\n%s", m.calls())
	}
	if got, _ := os.Readlink(lock); got != strconv.Itoa(os.Getpid()) {
		t.Errorf("a refused run took the lock: %q", got)
	}

	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	os.Remove(lock)
	if err := os.Symlink(strconv.Itoa(gone.Process.Pid), lock); err != nil {
		t.Fatal(err)
	}
	if out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dumped", dump); code != 0 {
		t.Fatalf("the import under a left lock: exit %d, %s", code, out)
	}
	if exists(lock) {
		t.Error("the import left its lock behind")
	}
}

// lockRacer takes the Cutover's lock and holds it a moment.
const lockRacer = `. "$1"
hold_lab
echo "took $$"
sleep 3
`

// deadPS is a ps for which the dead holder's pid is gone only once both
// racers have asked: the first goes on at once, the second a second later,
// after the first has taken the lock again.
const deadPS = `#!/bin/bash
if [[ $1 == -p && $2 == "$DEAD_HOLDER" ]]; then
  first=0
  mkdir "$STUB_STATE/ps.first" 2>/dev/null && first=1
  touch "$STUB_STATE/ps.asked.$PPID"
  for n in $(seq 100); do
    (( $(ls "$STUB_STATE" | grep -c '^ps\.asked\.') >= 2 )) && break
    sleep 0.05
  done
  (( first )) || sleep 1
  exit 1
fi
exec /bin/ps "$@"
`

// Two scripts that find the same dead holder's lock: one takes it, and the
// other is refused by the lock the first now holds; it never removes it.
func TestTwoRunsThatFindALeftLockTakeItOnce(t *testing.T) {
	m := newMini(t, oldlabtest.OnOmarchyGPU)
	gone := exec.Command("true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	lock := m.path(cutoverLock)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strconv.Itoa(gone.Process.Pid), lock); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(m.base, "stubs", "ps"), deadPS, 0o755)
	env := append(m.env, "DEAD_HOLDER="+strconv.Itoa(gone.Process.Pid))
	lib := filepath.Join(repo(t), "scripts", "cutover", "mini.sh")
	outs := make([]strings.Builder, 2)
	cmds := make([]*exec.Cmd, 2)
	for i := range cmds {
		cmds[i] = exec.Command("bash", "-c", lockRacer, "racer", lib)
		cmds[i].Env = env
		cmds[i].Stdout, cmds[i].Stderr = &outs[i], &outs[i]
		if err := cmds[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	took, refused := 0, 0
	for i, c := range cmds {
		c.Wait()
		out := outs[i].String()
		t.Logf("racer %d:\n%s", i, out)
		if strings.Contains(out, "took ") {
			took++
		}
		if strings.Contains(out, "changes the lab now") {
			refused++
		}
	}
	if took != 1 || refused != 1 {
		t.Errorf("%d took the lock and %d were refused, want one each", took, refused)
	}
	if exists(lock) {
		t.Error("the lock was left behind")
	}
}

// A run that empties the reports and then fails leaves no pass from earlier
// the same day for the opening to read.
func TestAFailedImportTakesAwayTheDaysEarlierPass(t *testing.T) {
	m, dump, public := rehearsal(t)
	mark := m.path("var/lab/import/imported")
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	if out, code := m.run(oldLink+"\n", "rehearsal-import", "run", "--public-key", public, "--dumped", dump); code != 0 || !exists(mark) {
		t.Fatalf("the first import: exit %d, %s", code, out)
	}
	wrong := "https://omarchy-m-testing.org/reports/" + strings.Repeat("0", 32) + "/deletion?token=not-the-one"
	if out, code := m.run(wrong+"\n", "rehearsal-import", "run", "--public-key", public, "--dumped", dump); code == 0 || !strings.Contains(out, "the third check failed") {
		t.Fatalf("the second import, with a deletion link that is not the old site's: exit %d, %s", code, out)
	}
	if exists(mark) {
		t.Errorf("the failed import left the earlier pass's mark: %q", read(t, mark))
	}
}

// Reading the old site, the rehearsal shows the certificate chain its
// Postgres presents, keeps the chain's last certificate as the CA file,
// and the dump checks the certificate against the file given at its
// prompt. Here the made-up Postgres passes TLS and then refuses the user:
// no dump, and nothing of the lab changed.
func TestTheRehearsalShowsTheCertificateChainBeforeTheDumpChecksIt(t *testing.T) {
	m, _, public := rehearsal(t)
	if out, code := m.run(hex.EncodeToString(machineIDKey())+"\n", "rehearsal-import", "key"); code != 0 {
		t.Fatalf("the key: exit %d, %s", code, out)
	}
	a := oldsitetest.NewAuthority(t)
	port, _ := a.TLSPostgres(t)
	address := "postgresql://postgres:hunter2-not-real@127.0.0.1:" + port + "/railway"
	ca := m.path("var/lab/import/old-site-ca.pem")
	os.Remove(m.log)
	out, code := m.run(address+"\n"+address+"\n"+ca+"\n", "rehearsal-import", "run", "--public-key", public)
	if code == 0 || !strings.Contains(out, "did not accept that user and password") || !strings.Contains(out, "no dump was written") {
		t.Fatalf("exit %d, %s", code, out)
	}
	sum := sha256.Sum256(a.CA.Raw)
	if !strings.Contains(out, "SHA-256 ") || !strings.Contains(out, strings.ToUpper(hex.EncodeToString(sum[:1]))+":") || !strings.Contains(out, "at the dump's CA file prompt give "+ca) {
		t.Errorf("the chain was not shown:\n%s", out)
	}
	if strings.Contains(out, "hunter2") {
		t.Error("the run repeats the password")
	}
	if !strings.Contains(read(t, ca), "BEGIN CERTIFICATE") {
		t.Error("the CA file was not kept")
	}
	calls := m.calls()
	if c, d := strings.Index(calls, "import check certificate --save "), strings.Index(calls, "import dump --to "); c < 0 || d < c {
		t.Errorf("the chain was not shown before the dump:\n%s", calls)
	}
	if strings.Contains(calls, "launchctl bootout") || strings.Contains(calls, " import load ") {
		t.Errorf("a run with no dump changed the lab:\n%s", calls)
	}

	// The day's dump asks for the CA file the rehearsal kept.
	os.Remove(ca)
	day := m.path("var/lab/import/the-day.dump")
	if out, code := m.run("", "rehearsal-import", "dump", "--dump", day); code == 0 || !strings.Contains(out, "no CA file at") || exists(day) {
		t.Errorf("the day's dump with no CA file: exit %d, %s", code, out)
	}
}
