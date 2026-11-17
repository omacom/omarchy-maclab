package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/installed"
)

// approvedBy is the manifest of manifestNaming with approved_by set to
// these values as they are written in JSON.
func approvedBy(t *testing.T, release string, approvers ...any) (manifest, payload string) {
	t.Helper()
	manifest, payload = manifestNaming(release, "payload/linux-aurora.files.sha256")
	var m map[string]any
	if err := json.Unmarshal([]byte(manifest), &m); err != nil {
		t.Fatal(err)
	}
	m["approved_by"] = approvers
	b, _ := json.Marshal(m)
	return string(b), payload
}

// A manifest frozen from now on names who approved it by account id, and
// the lab shows each by the handle it knows them by. A handle, or an
// account the lab does not know, is refused before anything is frozen.
func TestANewManifestNamesItsApproversByAccountID(t *testing.T) {
	l, _, _ := installedLab(t, nil)
	grace := l.member("grace")
	l.member("ada")
	l.freeLease()
	l.leaseAs(grace, 0)
	freeze := func(release string, approvers ...any) (int, string) {
		t.Helper()
		manifest, payload := approvedBy(t, release, approvers...)
		r, body := l.as(grace.Token, "POST", "/api/installed/"+release+"/manifest", map[string]any{"manifest": l.artifact(manifest), "payload": l.artifact(payload)})
		return r.StatusCode, body
	}
	for what, approvers := range map[string][]any{
		"a handle":           {"grace"},
		"an unknown person":  {int64(424242)},
		"no account":         {0},
		"a handle and an id": {testAccount("grace"), "ada"},
	} {
		if code, body := freeze("10.1", approvers...); code != http.StatusBadRequest || !strings.Contains(body, "account id") {
			t.Errorf("%s in approved_by: %d %s", what, code, body)
		}
	}
	absent(t, "a refused manifest", filepath.Join(l.s.frozenDir("10.1"), "PINS.expected"))

	if code, body := freeze("10.1", testAccount("grace"), testAccount("ada")); code != http.StatusOK {
		t.Fatalf("approved by two account ids: %d %s", code, body)
	}
	frozen, _ := os.ReadFile(filepath.Join(l.s.frozenDir("10.1"), "expected", "manifest.json"))
	if strings.Contains(string(frozen), "grace") || !strings.Contains(string(frozen), `"approved_by":[`) {
		t.Errorf("the frozen manifest names its approvers otherwise than by account id: %s", frozen)
	}
	if got := l.release(grace.Token, "10.1").ApprovedBy; !reflect.DeepEqual(got, []string{"grace", "ada"}) {
		t.Errorf("the Release says it was approved by %v", got)
	}

	// Forgetting takes ada off what the lab shows; the frozen file stays.
	if err := l.s.store.forget(newForgetting(testAccount("ada"), "ada")); err != nil {
		t.Fatal(err)
	}
	if got := l.release(grace.Token, "10.1").ApprovedBy; !reflect.DeepEqual(got, []string{"grace"}) {
		t.Errorf("after ada was forgotten the Release says it was approved by %v", got)
	}
	var left int
	l.s.store.db.QueryRow(`SELECT count(*) FROM release_approvals WHERE account_id = ?`, testAccount("ada")).Scan(&left)
	if left != 0 {
		t.Errorf("%d approvals of a forgotten person are kept", left)
	}
	if again, _ := os.ReadFile(filepath.Join(l.s.frozenDir("10.1"), "expected", "manifest.json")); string(again) != string(frozen) {
		t.Error("the forgetting rewrote a frozen manifest")
	}
}

// A manifest frozen before names its approvers by handle: it still loads,
// as it was, and the lab shows nothing of it as an approval.
func TestAManifestFrozenBeforeKeepsItsApproversHandles(t *testing.T) {
	var m struct {
		ApprovedBy []json.RawMessage `json:"approved_by"`
	}
	old := `{"schema":"maclab.installed-manifest/2","release":"r9","approved_by":["edith",1001]}`
	var parsed installed.Manifest
	if err := json.Unmarshal([]byte(old), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.ApprovedBy) != 2 || parsed.ApprovedBy[0].Handle != "edith" || parsed.ApprovedBy[1].AccountID != 1001 {
		t.Fatalf("read %+v", parsed.ApprovedBy)
	}
	b, _ := json.Marshal(parsed)
	json.Unmarshal(b, &m)
	if len(m.ApprovedBy) != 2 || string(m.ApprovedBy[0]) != `"edith"` || string(m.ApprovedBy[1]) != `1001` {
		t.Errorf("written back as %s", b)
	}
}

// A freeze whose approver is forgotten between its check and its writing
// keeps no approval of them, and a Release is listed with its approvals at
// once or not at all.
func TestAFreezeRacingAForgettingStoresNoApprovalOfThem(t *testing.T) {
	l, _, _ := installedLab(t, nil)
	l.member("grace")
	l.member("bob")
	grace, bob := testAccount("grace"), testAccount("bob")
	m := installed.Manifest{Release: "10.2", ApprovedBy: []installed.Approver{{AccountID: grace}, {AccountID: bob}}}
	m.Kernel.Release = "7.1"
	if err := l.s.approversOf(&m); err != nil {
		t.Fatal(err)
	}
	if err := l.s.store.forget(newForgetting(bob, "bob")); err != nil {
		t.Fatal(err)
	}
	by := Person{AccountID: testAccount(harnessMaintainer), Handle: harnessMaintainer, Maintainer: true}
	if err := l.s.listRelease(&m, strings.Repeat("e", 64), by, l.s.now()); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) (n int) {
		l.s.store.db.QueryRow(q, args...).Scan(&n)
		return n
	}
	if n := count(`SELECT count(*) FROM release_approvals WHERE account_id = ?`, bob); n != 0 {
		t.Errorf("%d approvals of a person forgotten before the freeze wrote them", n)
	}
	if n := count(`SELECT count(*) FROM release_approvals WHERE release = '10.2' AND account_id = ?`, grace); n != 1 {
		t.Errorf("%d approvals of grace, want 1", n)
	}

	if _, err := l.s.store.db.Exec(`CREATE TRIGGER full_disk BEFORE INSERT ON release_approvals BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	m.Release = "10.3"
	if err := l.s.listRelease(&m, strings.Repeat("f", 64), by, l.s.now()); err == nil {
		t.Fatal("a Release whose approvals could not be written was listed")
	}
	if n := count(`SELECT count(*) FROM releases WHERE name = '10.3'`); n != 0 {
		t.Error("a Release is listed without its approvals")
	}
}

// addApprovals adds approvals to a Release listed already, as a test's
// Release would have had them from its freeze.
func (st *store) addApprovals(release string, ids []int64) error {
	for i, id := range ids {
		if _, err := st.db.Exec(`INSERT INTO release_approvals (release, position, account_id) VALUES (?, ?, ?)`, release, i, id); err != nil {
			return err
		}
	}
	return nil
}

// labd's log, which a forgetting does not reach, names who recorded a
// verdict by account id, never by handle.
func TestTheLogNamesAVerdictsPersonByAccountID(t *testing.T) {
	l, _, _, _ := twoReleases(t)
	logs := &lockedBuffer{b: &bytes.Buffer{}}
	l.s.log.SetOutput(logs)
	ravi := l.who(maintainerID)
	relVerdict(t, l.s, ravi, "9.8", api.ReleaseNotQualified, "m2-max panics at boot")
	got := logs.String()
	if strings.Contains(got, ravi.Handle) || !strings.Contains(got, "account "+strconv.FormatInt(maintainerID, 10)) {
		t.Errorf("the log of a verdict:\n%s", got)
	}
}
