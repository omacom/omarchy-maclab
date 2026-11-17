package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/pbkdf2"

	"crypto/sha256"
	"encoding/pem"

	"github.com/omacom/omarchy-maclab/internal/githubtest"
	"github.com/omacom/omarchy-maclab/internal/oldsite"
	"github.com/omacom/omarchy-maclab/internal/oldsite/oldsitetest"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/server"
)

const typedAddress = "postgresql://postgres:hunter2-not-real@old.site.example:6543/railway"

func oldMachineIDKey() []byte {
	return pbkdf2.Key([]byte("the old site's secret, made up"), []byte("report machine id from key"), 1000, 64, sha256.New)
}

// importLab is a lab's data directory with a labd on it, a made-up old
// site behind the command's Postgres reader, and a stand-in for GitHub.
type importLab struct {
	t      *testing.T
	data   string
	corpus *oldsitetest.Corpus
	github string
	gh     *githubtest.Server
	// opened is every address the command opened a Postgres at.
	opened  []string
	caFiles []string
	said    bytes.Buffer
}

type fakeOldSite struct{ oldsite.Source }

func (fakeOldSite) Close() error { return nil }

func newImportLab(t *testing.T) *importLab {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	l := &importLab{t: t, data: t.TempDir(), corpus: oldsitetest.Made(t, oldMachineIDKey())}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := server.New(ctx, server.Config{DataDir: l.data}, log.New(io.Discard, "", 0)); err != nil {
		t.Fatal(err)
	}
	gh := githubtest.New("id", "secret")
	gh.AddUser(2741, "dhh")
	gh.AddUser(3003, "RyanRHughes")
	fake := httptest.NewServer(gh)
	t.Cleanup(fake.Close)
	l.github, l.gh = fake.URL, gh
	was := openOldSite
	t.Cleanup(func() { openOldSite = was })
	openOldSite = func(address, caFile string) (oldSiteDB, error) {
		l.opened = append(l.opened, address)
		l.caFiles = append(l.caFiles, caFile)
		return fakeOldSite{l.corpus}, nil
	}
	return l
}

// run is `labd import args...` with typed at its prompt, as a pipe hands it.
func (l *importLab) run(typed string, args ...string) (code int, out string) {
	l.t.Helper()
	var stdout, stderr bytes.Buffer
	code = importCommand(context.Background(), args, strings.NewReader(typed), &stdout, &stderr)
	l.said.Write(stdout.Bytes())
	l.said.Write(stderr.Bytes())
	return code, stdout.String() + stderr.String()
}

func (l *importLab) reports() *reports.Store {
	l.t.Helper()
	store, err := reports.Open(filepath.Join(l.data, "reports.db"))
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { store.Close() })
	return store
}

func TestTheImportCommandDumpsTheOldSiteThenImportsFromTheFileAlone(t *testing.T) {
	l := newImportLab(t)
	dump := filepath.Join(t.TempDir(), "old-site.dump")

	code, out := l.run(typedAddress+"\n/var/lab/import/old-site-ca.pem\n", "dump", "--to", dump)
	if code != 0 || !strings.Contains(out, "20 reports, 4 tester_bindings, 6 testers") {
		t.Fatalf("dump: exit %d, %s", code, out)
	}
	if len(l.opened) != 1 || l.opened[0] != typedAddress || l.caFiles[0] != "/var/lab/import/old-site-ca.pem" {
		t.Fatalf("the reader was opened at %d addresses, not the one typed, or not with the CA file typed: %q", len(l.opened), l.caFiles)
	}
	if st, err := os.Stat(dump); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the dump file: %v", err)
	}
	if code, _ := l.run(typedAddress+"\n", "dump", "--to", dump); code != 1 || len(l.opened) != 1 {
		t.Errorf("a second dump onto the same file: exit %d, %d reads of the old site", code, len(l.opened))
	}

	// From here on the old site is never read.
	openOldSite = func(string, string) (oldSiteDB, error) {
		t.Error("the import opened the old site's Postgres")
		return nil, errors.New("no")
	}
	code, out = l.run("", "load", "--from", dump, "--data", l.data, "--github-url", l.github)
	if code != 0 {
		t.Fatalf("load: exit %d, %s", code, out)
	}
	for _, want := range []string{
		"20 reports (2 hidden, 8 Verified runs), 4 Mac sign-ins, 19 digests; 6 people, 6 of them new to the lab.",
		"Listed, 4 (all were carried):",
		"report " + oldsitetest.UnknownCheck + ": it has a check the catalogue lacks: display.gone",
		"report " + oldsitetest.Twice + ": the old site stored it twice",
		"@gone-handle never signed in at the old site, and GitHub knows no such account now",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("load said nothing of %q:\n%s", want, out)
		}
	}
	store := l.reports()
	if held, hidden, signIns, digests, err := store.Holds(); err != nil || held != 20 || hidden != 2 || signIns != 4 || digests != 19 {
		t.Errorf("reports.db holds %d reports (%d hidden), %d sign-ins, %d digests, %v", held, hidden, signIns, digests, err)
	}
	if known, err := server.KnownAccounts(l.data, []int64{1001, 1002, 2741, 3003, 4242, 5005}); err != nil || known != 6 {
		t.Errorf("%d of the 6 people are known, %v", known, err)
	}

	// A second run changes nothing and says why.
	code, out = l.run("", "load", "--from", dump, "--data", l.data, "--github-url", l.github)
	if code != 1 || !strings.Contains(out, "already holds 20 reports") || !strings.Contains(out, "changed nothing") {
		t.Errorf("a second load: exit %d, %s", code, out)
	}
	if held, _, _, _, _ := store.Holds(); held != 20 {
		t.Errorf("after a second load reports.db holds %d reports", held)
	}
	if asked := l.gh.LookedUp(); len(asked) != 3 {
		t.Errorf("GitHub was asked %d times: the second load should ask nothing", len(asked))
	}

	// The three checks.
	if code, out = l.run("", "check", "counts", "--from", dump, "--data", l.data); code != 0 || !strings.Contains(out, "The row counts match.") ||
		!strings.Contains(out, "reports: 20 at the old site, 20 in the dump, 20 in reports.db") || !strings.Contains(out, "hidden reports: 2 in the dump, 2 in reports.db") {
		t.Errorf("check counts: exit %d, %s", code, out)
	}
	keys := t.TempDir()
	idKey, publicKey, other := filepath.Join(keys, "machine-id-key"), filepath.Join(keys, "a.pub"), filepath.Join(keys, "other.pub")
	os.WriteFile(idKey, []byte(hex.EncodeToString(oldMachineIDKey())+"\n"), 0o600)
	os.WriteFile(publicKey, []byte(oldsitetest.PublicKey("a")+" owner@m2-max\n"), 0o644)
	os.WriteFile(other, []byte(oldsitetest.PublicKey("never-seen")+"\n"), 0o644)
	if code, out = l.run("", "check", "machine-id", "--machine-id-key", idKey, "--public-key", publicKey, "--data", l.data); code != 0 || !strings.Contains(out, "is on 4 reports and 1 Mac sign-ins") {
		t.Errorf("check machine-id: exit %d, %s", code, out)
	}
	if strings.Contains(out, "key:") || strings.Contains(out, "AAAA") {
		t.Errorf("the check shows a machine id or a key: %s", out)
	}
	if code, _ = l.run("", "check", "machine-id", "--machine-id-key", idKey, "--public-key", other, "--data", l.data); code != 1 {
		t.Errorf("check machine-id for a Mac the old site never saw: exit %d", code)
	}
	link := "https://omarchy-m-testing.org/reports/" + oldsitetest.Verified + "/deletion?token=" + oldsitetest.VerifiedToken
	if code, out = l.run(link+"\n", "check", "deletion-link", "--data", l.data); code != 0 || !strings.Contains(out, "deleted its report") {
		t.Errorf("check deletion-link: exit %d, %s", code, out)
	}
	if _, err := store.Report(oldsitetest.Verified); !errors.Is(err, reports.ErrNoReport) {
		t.Errorf("the report the old link names is still there: %v", err)
	}
	if code, out = l.run(link+"\n", "check", "deletion-link", "--data", l.data); code != 1 {
		t.Errorf("the link a second time: exit %d, %s", code, out)
	}
	if code, out = l.run("", "check", "counts", "--from", dump, "--data", l.data); code != 1 || !strings.Contains(out, "do not match") {
		t.Errorf("check counts with a report gone: exit %d, %s", code, out)
	}

	for _, secret := range []string{typedAddress, "hunter2", "old.site.example", oldsitetest.VerifiedToken} {
		if strings.Contains(l.said.String(), secret) {
			t.Errorf("the command's output repeats %q", secret)
		}
	}
	text, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"hunter2", "old.site.example", "railway"} {
		if bytes.Contains(text, []byte(secret)) {
			t.Errorf("the dump file holds %q", secret)
		}
	}
}

func TestTheAddressIsNeverAnArgumentAndNeverRepeated(t *testing.T) {
	l := newImportLab(t)
	dump := filepath.Join(t.TempDir(), "old-site.dump")
	for _, args := range [][]string{
		{"dump", "--to", dump, typedAddress},
		{"dump", "--to", dump, "--address", typedAddress},
		{"dump", "--address=" + typedAddress, "--to", dump},
		{"dump", typedAddress},
		{"load", "--from", dump, typedAddress},
	} {
		code, out := l.run("", args...)
		if code != 2 || len(l.opened) != 0 {
			t.Errorf("%v: exit %d, the old site read %d times", args[:1], code, len(l.opened))
		}
		if strings.Contains(out, "hunter2") || strings.Contains(out, "old.site.example") {
			t.Errorf("an address given as an argument is repeated: %s", out)
		}
		if _, err := os.Stat(dump); !os.IsNotExist(err) {
			t.Error("a dump was written")
		}
	}
	if code, out := l.run("\n", "dump", "--to", dump); code != 1 || !strings.Contains(out, "no address was given") || len(l.opened) != 0 {
		t.Errorf("nothing typed: exit %d, %s", code, out)
	}
	if code, _ := l.run(""); code != 2 {
		t.Errorf("no command: exit %d", code)
	}
}

func TestAnAddressTheReaderRefusesIsNotRepeated(t *testing.T) {
	l := newImportLab(t)
	openOldSite = pgOpen
	dump := filepath.Join(t.TempDir(), "old-site.dump")
	code, out := l.run("postgres://postgres:hunter2-not-real@old.site.example/railway?sslmode=disable\n", "dump", "--to", dump)
	if code != 1 || !strings.Contains(out, "TLS") || strings.Contains(out, "hunter2") || strings.Contains(out, "old.site.example") {
		t.Errorf("exit %d, %s", code, out)
	}
	if _, err := os.Stat(dump); !os.IsNotExist(err) {
		t.Error("a dump was written")
	}
}

func TestAnImportCutBeforeItsReportsIsStartedAgain(t *testing.T) {
	l := newImportLab(t)
	dump := filepath.Join(t.TempDir(), "old-site.dump")
	if code, out := l.run(typedAddress+"\n", "dump", "--to", dump); code != 0 {
		t.Fatal(out)
	}
	// GitHub is out of reach: nothing is imported.
	if code, out := l.run("", "load", "--from", dump, "--data", l.data, "--github-url", "http://127.0.0.1:1"); code != 1 || !strings.Contains(out, "Nothing was imported") {
		t.Fatalf("with GitHub out of reach: exit %d, %s", code, out)
	}
	store := l.reports()
	if held, _, signIns, digests, _ := store.Holds(); held+signIns+digests != 0 {
		t.Fatal("something was imported")
	}
	// A run was cut after its people were made and before its reports.
	if _, err := server.CarryPeople(l.data, []reports.CarriedPerson{{AccountID: 1001, Handle: "maralcbr"}, {AccountID: 5005, Handle: "outsider"}}, oldsite.Stamp(1700000000000000).Time()); err != nil {
		t.Fatal(err)
	}
	code, out := l.run("", "load", "--from", dump, "--data", l.data, "--github-url", l.github)
	if code != 0 || !strings.Contains(out, "6 people, 4 of them new to the lab") {
		t.Fatalf("started again: exit %d, %s", code, out)
	}
	if code, out := l.run("", "check", "counts", "--from", dump, "--data", l.data); code != 0 {
		t.Errorf("check counts: %s", out)
	}
	// A dump that was cut is not imported.
	whole, _ := os.ReadFile(dump)
	half := filepath.Join(t.TempDir(), "half.dump")
	os.WriteFile(half, whole[:len(whole)/2], 0o600)
	if code, out := l.run("", "load", "--from", half, "--data", t.TempDir()); code != 1 || !strings.Contains(out, "not whole") {
		t.Errorf("half a dump: exit %d, %s", code, out)
	}
}

// The cutover's script starts the command with no address anywhere in its
// arguments, in the order dump, load, the three checks, and stops at the
// first that fails.
func TestTheCutoverScriptRunsTheImportAndItsChecksInOrder(t *testing.T) {
	root := oldsitetest.Root(t)
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	stub := filepath.Join(dir, "labd")
	os.WriteFile(stub, []byte("#!/bin/sh\necho \"$*\" >> "+calls+"\n[ \"$1 $2 $3\" = \"import check ${FAIL:-none}\" ] && exit 1\n[ \"$2\" = dump ] && : > \"$4\"\nexit 0\n"), 0o755)
	script := filepath.Join(root, "scripts", "cutover", "import-reports")
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command(script, args...)
		cmd.Env = append([]string{"PATH=/usr/bin:/bin", "HOME=" + dir, "LABD=" + stub}, env...)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	dump := filepath.Join(dir, "old-site.dump")
	out, err := run(nil, "--data", "/var/lab/data", "--machine-id-key", "/k", "--public-key", "/p", dump)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	got, _ := os.ReadFile(calls)
	want := "import dump --to " + dump + "\n" +
		"import load --from " + dump + " --data /var/lab/data\n" +
		"import check counts --from " + dump + " --data /var/lab/data\n" +
		"import check machine-id --machine-id-key /k --public-key /p --data /var/lab/data\n" +
		"import check deletion-link --data /var/lab/data\n"
	if string(got) != want {
		t.Errorf("the script ran:\n%s\nwant:\n%s", got, want)
	}
	// With the dump already there the old site is not read again.
	os.Remove(calls)
	if out, err := run(nil, "--data", "/var/lab/data", "--machine-id-key", "/k", "--public-key", "/p", dump); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got, _ := os.ReadFile(calls); strings.Contains(string(got), "import dump") || !strings.HasPrefix(string(got), "import load") {
		t.Errorf("a second run with the dump there ran:\n%s", got)
	}
	os.Remove(calls)
	if out, err := run([]string{"FAIL=counts"}, "--data", "/var/lab/data", "--machine-id-key", "/k", "--public-key", "/p", dump); err == nil {
		t.Errorf("a failed check did not fail the script: %s", out)
	}
	if got, _ := os.ReadFile(calls); strings.Contains(string(got), "machine-id") {
		t.Errorf("the script went on after a failed check:\n%s", got)
	}
	for _, args := range [][]string{{}, {"--data", "/d", dump, "postgres://u:p@h/d"}, {"--address", "postgres://u:p@h/d", dump}} {
		os.Remove(calls)
		out, err := run(nil, args...)
		if err == nil || strings.Contains(out, "u:p@h") {
			t.Errorf("%d arguments: %v, %s", len(args), err, out)
		}
		if _, err := os.Stat(calls); err == nil {
			t.Error("the command was started")
		}
	}
}

func TestTheCountsCheckFailsWhenAPersonIsMissing(t *testing.T) {
	l := newImportLab(t)
	path := l.corpus.Dump(t)
	dump, err := oldsite.OpenDump(path)
	if err != nil {
		t.Fatal(err)
	}
	// The reports are there and the people are not.
	if _, err := l.reports().Carry(context.Background(), dump); err != nil {
		t.Fatal(err)
	}
	code, out := l.run("", "check", "counts", "--from", path, "--data", l.data)
	if code != 1 || !strings.Contains(out, "0 are people") || !strings.Contains(out, "do not match") {
		t.Errorf("exit %d, %s", code, out)
	}
}

// Beyond this machine the dump asks for the CA file the old site's
// certificate chains to, and reads nothing without it.
func TestTheDumpReadsNothingBeyondThisMachineWithoutTheCAFile(t *testing.T) {
	l := newImportLab(t)
	openOldSite = pgOpen
	dump := filepath.Join(t.TempDir(), "old-site.dump")
	for _, typed := range []string{typedAddress + "\n", typedAddress + "\n\n"} {
		code, out := l.run(typed, "dump", "--to", dump)
		if code != 1 || !strings.Contains(out, "CA file") || strings.Contains(out, "hunter2") || strings.Contains(out, "old.site.example") {
			t.Errorf("no CA file: exit %d, %s", code, out)
		}
		if _, err := os.Stat(dump); !os.IsNotExist(err) {
			t.Error("a dump was written")
		}
	}
}

// check certificate shows the chain the old site's Postgres presents, to be
// looked at before its CA file is trusted, and with --save keeps the
// chain's last certificate as that file. Neither user nor password is sent.
func TestCheckCertificateShowsTheChainAndKeepsItsLastCertificate(t *testing.T) {
	l := newImportLab(t)
	a := oldsitetest.NewAuthority(t)
	ca, leaf := a.CA, a.Leaf
	port, heard := a.TLSPostgres(t)
	address := "postgresql://postgres:hunter2-not-real@127.0.0.1:" + port + "/railway"
	saved := filepath.Join(t.TempDir(), "old-site-ca.pem")

	code, out := l.run(address+"\n", "check", "certificate", "--save", saved)
	if code != 0 {
		t.Fatalf("exit %d, %s", code, out)
	}
	for _, want := range []string{"2 certificates", "1. ", "2. ", "throwaway root", fingerprint(leaf.Raw), fingerprint(ca.Raw), "Wrote " + saved} {
		if !strings.Contains(out, want) {
			t.Errorf("the check says nothing of %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "hunter2") {
		t.Errorf("the check repeats the password: %s", out)
	}
	if got := <-heard; len(got) != 8 {
		t.Errorf("the server heard %d bytes, more than the request for TLS", len(got))
	}
	text, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}
	if block, _ := pem.Decode(text); block == nil || !bytes.Equal(block.Bytes, ca.Raw) {
		t.Error("the file saved is not the chain's last certificate")
	}
	// The file is never replaced.
	if code, out := l.run(address+"\n", "check", "certificate", "--save", saved); code != 1 || !strings.Contains(out, "already there") {
		t.Errorf("a second save onto the file: exit %d, %s", code, out)
	}
	if len(l.opened) != 0 {
		t.Error("the check opened the old site's Postgres as the dump does")
	}
}
