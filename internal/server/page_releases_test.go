package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// Releases, the M team's and the Maintainers' page: every Release whose
// manifest is frozen, one opened in place, the verdict and Promotion. And
// the one line of it a visitor sees, on Candidates.

func relSHA(name string) string {
	sum := sha256.Sum256([]byte("manifest of " + name))
	return hex.EncodeToString(sum[:])
}

// relFreeze lists a Release as a freeze of its manifest would.
func relFreeze(t *testing.T, s *Server, name string, at time.Time, by Person, packages []api.ReleasePackage, macs ...api.ReleaseMac) {
	t.Helper()
	if macs == nil {
		macs = []api.ReleaseMac{}
	}
	err := s.store.addRelease(api.Release{Name: name, FrozenBy: by.Handle, FrozenByID: by.AccountID, FrozenAt: at, ManifestSHA256: relSHA(name),
		Kernel: "7.1.12-aurora-" + name, Packages: packages, Macs: macs})
	if err != nil {
		t.Fatal(err)
	}
}

// relRun keeps a finished Installed run of the Release on that Mac.
func relRun(t *testing.T, s *Server, id, release, mac string, at time.Time, outcome string) {
	t.Helper()
	j := &api.Job{ID: id, Spec: api.JobSpec{Device: mac, InstalledManifest: relSHA(release)}, State: api.JobDone, Outcome: outcome, Created: at, Updated: at}
	if err := s.store.saveJob(j); err != nil {
		t.Fatal(err)
	}
}

func relVerdict(t *testing.T, s *Server, by Person, release, state, note string) {
	t.Helper()
	if _, err := s.verdict(by, release, api.VerdictRequest{State: state, Note: note, Confirm: true}); err != nil {
		t.Fatalf("the verdict %s on %s: %v", state, release, err)
	}
}

func (s *Server) releaseNowOrNil(name string) *api.Release {
	r, _ := s.releaseNow(name)
	return r
}

func kernelPackage(version string) []api.ReleasePackage {
	return []api.ReleasePackage{{Name: "linux-aurora", Version: version}}
}

// setsPackages are every package the run names, at its versions: a Release
// of exactly these matches the run's set.
func setsPackages(t *testing.T, body doc) []api.ReleasePackage {
	t.Helper()
	var packages []api.ReleasePackage
	for _, item := range at(body, "system", "packages").([]any) {
		pkg := item.(doc)
		packages = append(packages, api.ReleasePackage{Name: pkg["name"].(string), Version: pkg["version"].(string)})
	}
	return packages
}

func (b *browser) releases(query string) *node {
	b.l.t.Helper()
	w := b.do("GET", "/releases"+query, "")
	if w.Code != http.StatusOK {
		b.l.t.Fatalf("GET /releases%s: %d %s", query, w.Code, w.Body)
	}
	return parseHTML(b.l.t, w.Body.String())
}

// relReport publishes a run on a candidate set, as reports.db holds it.
func relReport(t *testing.T, dir, id string, body doc, at time.Time, verified bool) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "reports.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	text, _ := json.Marshal(body)
	var handle, account any
	if verified {
		handle, account = "team-a", int64(77)
	}
	_, err = db.Exec(`INSERT INTO reports (public_id, deletion_token_digest, schema_version, body, created_at, updated_at, machine_id, handle, account_id, verified)
		VALUES (?, 'digest', 1, ?, ?, ?, ?, ?, ?, ?)`, id, string(text), at.UnixMicro(), at.UnixMicro(), "key:"+id, handle, account, verified)
	if err != nil {
		t.Fatal(err)
	}
}

// twoReleases is a lab with Release 9.8, frozen first, and Release 9.9 on
// two Macs of two Configurations: m1-pro has a passed Installed run after a
// failed one, m2-max a failed one.
func twoReleases(t *testing.T) (l *signInLab, grace, linus, ravi *browser) {
	t.Helper()
	l = leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-max", "j414c", nil)
	grace, linus, ravi = l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()
	at := l.s.now()
	relFreeze(t, l.s, "9.8", at.Add(-48*time.Hour), l.who(maintainerID), kernelPackage("9.8-1"))
	relFreeze(t, l.s, "9.9", at.Add(-24*time.Hour), l.who(lrGrace),
		[]api.ReleasePackage{{Name: "linux-aurora", Version: "7.1.12.aurora2-2"}, {Name: "omarchy-mac-boot", Version: "20260925-3"}},
		api.ReleaseMac{Mac: "m1-pro", DeviceID: "d-m1-pro", Board: "apple,j316s", Configuration: "j316s, 10 CPU and 16 GPU cores"},
		api.ReleaseMac{Mac: "m2-max", DeviceID: "d-m2-max", Board: "apple,j414c", Configuration: "j414c, 12 CPU and 38 GPU cores"})
	relRun(t, l.s, "job-a1", "9.9", "m1-pro", at.Add(-3*time.Hour), api.OutcomeTestsFailed)
	relRun(t, l.s, "job-a2", "9.9", "m1-pro", at.Add(-2*time.Hour), api.OutcomePass)
	relRun(t, l.s, "job-b1", "9.9", "m2-max", at.Add(-time.Hour), api.OutcomePanicked)
	return l, grace, linus, ravi
}

func TestReleasesOpensToTheMTeamAndMaintainersAndIsNowherePublic(t *testing.T) {
	l, grace, _, ravi := twoReleases(t)
	if w := l.browser().do("GET", "/releases", ""); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/sign-in") {
		t.Errorf("signed out: %d to %q", w.Code, w.Header().Get("Location"))
	}
	nobody := l.signedIn(nobodyID)
	if w := nobody.do("GET", "/releases", ""); w.Code != http.StatusNotFound {
		t.Errorf("signed in with no role: %d", w.Code)
	}
	for _, target := range []string{"/releases/9.9/verdict", "/releases/9.9/macs/m1-pro/promote"} {
		if w := nobody.form(target, "state", api.ReleaseNotQualified, "note", "no", "sure", "yes"); w.Code != http.StatusNotFound {
			t.Errorf("a person with no role posting %s: %d", target, w.Code)
		}
	}
	if r := l.s.releaseNowOrNil("9.9"); r == nil || r.State != api.ReleaseNoVerdict {
		t.Fatalf("a person with no role changed Release 9.9: %+v", r)
	}
	for who, b := range map[string]*browser{"an M team member": grace, "a Maintainer": ravi} {
		w := b.do("GET", "/releases", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", who, w.Code)
		}
		if !strings.Contains(part(t, w.Body.String(), `<nav class="lab-nav"`, "</nav>"), `aria-current="page">Releases`) {
			t.Errorf("%s: Releases is not the current page of the lab's row", who)
		}
		if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
			t.Errorf("%s: the page may be kept: %q", who, got)
		}
	}
	// No public page names a Release that matches no candidate set.
	for _, target := range []string{"/", "/candidates", "/reports", "/data"} {
		w := l.browser().do("GET", target, "")
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "Release 9.9") {
			t.Errorf("%s signed out: %d, or it names Release 9.9", target, w.Code)
		}
		if strings.Contains(w.Body.String(), `href="/releases`) {
			t.Errorf("%s links Releases for a visitor", target)
		}
	}
}

func TestReleasesListsOneRowPerReleaseNewestFirst(t *testing.T) {
	l, grace, _, _ := twoReleases(t)
	run := onSet(goldenDoc(t, "m2-max-image2"), "edge-1")
	run["system"].(doc)["packages"] = []any{doc{"name": "linux-aurora", "version": "7.1.12.aurora2-2"}, doc{"name": "omarchy-mac-boot", "version": "20260925-3"}}
	relReport(t, l.dir, "run-edge-1", run, l.s.now().Add(-72*time.Hour), true)
	relVerdict(t, l.s, l.who(lrLinus), "9.9", api.ReleaseQualified, "both laptops boot it")
	if err := l.s.store.addPromotion("9.9", "d-m1-pro", api.ReleasePromotion{Time: l.s.now(), Mac: "m1-pro", Job: "job-a2", Kernel: "7.1.12-aurora-9.9", AccountID: maintainerID, Handle: "ravi"}); err != nil {
		t.Fatal(err)
	}

	page := grace.releases("")
	same(t, "one row per Release, newest first", attrsOf(page.all(t, "tr.release-row"), "id"), []string{"release-9.9", "release-9.8"})
	if page.count(t, "tr.release-detail") != 0 {
		t.Error("a Release is open before anyone opened it")
	}
	row := page.sole(t, `tr.release-row[id="release-9.9"]`)
	day := l.s.now().UTC().Format(time.DateOnly)
	for label, want := range map[string]string{
		"Release":       "9.9",
		"Frozen":        "@grace " + utcMinute(l.s.now().Add(-24*time.Hour)),
		"Verdict":       "qualified " + day,
		"Coverage":      "1 of 2 Configurations",
		"Promoted":      "1 Mac",
		"Candidate set": "edge-1",
	} {
		if got := row.sole(t, `td[data-label="`+label+`"]`).words(); !strings.Contains(got, want) {
			t.Errorf("9.9's %s: %q, want %q", label, got, want)
		}
	}
	if !row.hasText(t, `td[data-label="Verdict"] .badge.state-qualified`, "qualified") {
		t.Error("the state is not a badge")
	}
	if !row.hasText(t, `td[data-label="Candidate set"] a[href="/candidates/edge-1"]`, "edge-1") {
		t.Error("the matching candidate set is not linked")
	}
	if got := row.sole(t, "a.person-toggle").attr("href"); got != "/releases?open=9.9" {
		t.Errorf("the row opens at %q", got)
	}

	old := page.sole(t, `tr.release-row[id="release-9.8"]`)
	for label, want := range map[string]string{"Frozen": "@ravi", "Verdict": "no verdict", "Coverage": "0 of 0", "Promoted": "0", "Candidate set": "–"} {
		if got := old.sole(t, `td[data-label="`+label+`"]`).words(); !strings.Contains(got, want) {
			t.Errorf("9.8's %s: %q, want %q", label, got, want)
		}
	}
	if old.count(t, `td[data-label="Candidate set"] a`) != 0 {
		t.Error("a Release matching no set links one")
	}
}

func TestAReleaseOpensInPlaceWithWhatItIsItsMacsAndItsVerdictHistory(t *testing.T) {
	l, grace, _, ravi := twoReleases(t)
	relVerdict(t, l.s, l.who(lrLinus), "9.9", api.ReleaseNotQualified, "m2-max panics at boot")
	l.clock.Advance(time.Hour)
	relVerdict(t, l.s, l.who(maintainerID), "9.9", api.ReleaseQualified, "the panic was the cable")
	if err := l.s.store.addPromotion("9.9", "d-m1-pro", api.ReleasePromotion{Time: l.s.now(), Mac: "m1-pro", Job: "job-a2", Kernel: "7.1.12-aurora-9.9", AccountID: maintainerID, Handle: "ravi"}); err != nil {
		t.Fatal(err)
	}
	// Its manifest's approvers, by account id: the page says their handles.
	if err := l.s.store.addApprovals("9.9", []int64{lrLinus, lrGrace}); err != nil {
		t.Fatal(err)
	}

	for who, b := range map[string]*browser{"an M team member": grace, "a Maintainer": ravi} {
		page := b.releases("?open=9.9")
		if page.count(t, "tr.release-row") != 2 || page.count(t, "tr.release-detail") != 1 {
			t.Fatalf("%s: the list with one Release open: %d rows, %d open", who, page.count(t, "tr.release-row"), page.count(t, "tr.release-detail"))
		}
		if got := page.sole(t, `tr.release-row[id="release-9.9"] a.person-toggle`); got.attr("href") != "/releases" || got.attr("aria-expanded") != "true" {
			t.Errorf("%s: the open row shuts at %q", who, got.attr("href"))
		}
		open := page.sole(t, "tr.release-detail")

		what := open.sole(t, ".release-what")
		if !strings.Contains(what.words(), "Kernel 7.1.12-aurora-9.9") {
			t.Errorf("%s: what it is lacks the kernel: %q", who, what.words())
		}
		var packages []string
		for _, li := range what.all(t, "li.release-package") {
			packages = append(packages, li.words())
		}
		same(t, who+": its packages with versions", packages, []string{"linux-aurora 7.1.12.aurora2-2", "omarchy-mac-boot 20260925-3"})
		if got := what.sole(t, "p.release-approved").words(); got != "Approved by @"+l.who(lrLinus).Handle+", @"+l.who(lrGrace).Handle {
			t.Errorf("%s: who approved it: %q", who, got)
		}

		macs := open.sole(t, ".release-macs")
		if got := macs.sole(t, "p.coverage").words(); got != "1 of 2 Configurations have a passed run" {
			t.Errorf("%s: the line above its Macs: %q", who, got)
		}
		same(t, who+": its Macs", attrsOf(macs.all(t, "tr.release-mac"), "id"), []string{"release-9.9-mac-m1-pro", "release-9.9-mac-m2-max"})
		pro := macs.sole(t, `tr.release-mac[id="release-9.9-mac-m1-pro"]`)
		for label, want := range map[string]string{"Mac": "m1-pro", "Configuration": "j316s, 10 CPU and 16 GPU cores", "Installed run": "job-a2 passed", "Runs": "2", "Promoted": "promoted by @ravi"} {
			if got := pro.sole(t, `td[data-label="`+label+`"]`).words(); !strings.Contains(got, want) {
				t.Errorf("%s: m1-pro's %s: %q, want %q", who, label, got, want)
			}
		}
		if !pro.hasText(t, `td[data-label="Installed run"] a[href="/jobs/job-a2"]`, "job-a2") {
			t.Errorf("%s: the latest Installed run is not linked to its job", who)
		}
		max := macs.sole(t, `tr.release-mac[id="release-9.9-mac-m2-max"]`)
		for label, want := range map[string]string{"Installed run": "job-b1 failed", "Runs": "1", "Promoted": "not promoted"} {
			if got := max.sole(t, `td[data-label="`+label+`"]`).words(); !strings.Contains(got, want) {
				t.Errorf("%s: m2-max's %s: %q, want %q", who, label, got, want)
			}
		}

		var history []string
		for _, li := range open.all(t, "ol.verdict-history li") {
			history = append(history, li.words())
		}
		same(t, who+": the verdict history, newest first", history, []string{
			"qualified @ravi " + utcMinute(l.s.now()) + " the panic was the cable",
			"not qualified @linus " + utcMinute(l.s.now().Add(-time.Hour)) + " m2-max panics at boot",
		})

		// The page starts nothing, and the kernel gate is not on it.
		for _, form := range page.all(t, "main form") {
			if action := form.attr("action"); !strings.HasPrefix(action, "/releases/9.9/verdict") && !strings.HasPrefix(action, "/releases/9.9/macs/") {
				t.Errorf("%s: a form posts to %q", who, action)
			}
		}
		text := strings.ToLower(page.sole(t, "main").words())
		for _, never := range []string{"gate", "freeze-manifest", "freeze-entry", "start an installed run", "run installed"} {
			if strings.Contains(text, never) {
				t.Errorf("%s: the page says %q", who, never)
			}
		}
	}

	// A Release nobody froze is not opened, and the list still shows.
	if page := grace.releases("?open=7.7"); page.count(t, "tr.release-detail") != 0 || page.count(t, "tr.release-row") != 2 {
		t.Error("opening a Release that is not listed")
	}
}

// verdictButtons are the labels of the verdict's buttons on an open Release.
func verdictButtons(t *testing.T, page *node) string {
	t.Helper()
	var labels []string
	for _, b := range page.all(t, ".release-verdict button") {
		labels = append(labels, b.words())
	}
	return strings.Join(labels, ", ")
}

func TestAnMTeamMemberRecordsAVerdictOnlyWhileThereIsNoneAndIsAskedTwice(t *testing.T) {
	l, grace, linus, _ := twoReleases(t)
	if got := verdictButtons(t, grace.releases("?open=9.9")); got != "Record qualified, Record not qualified" {
		t.Fatalf("an M team member's buttons while there is no verdict: %q", got)
	}

	// The first click states the effect and records nothing.
	w := grace.form("/releases/9.9/verdict", "state", api.ReleaseQualified, "note", "boots on the Pro")
	if w.Code != http.StatusOK {
		t.Fatalf("the first click: %d %s", w.Code, w.Body)
	}
	asked := parseHTML(t, w.Body.String())
	sentence := asked.sole(t, "tr.release-detail .asking button.armed").words()
	for _, want := range []string{`Record "qualified" for Release 9.9 as @grace`, "boots on the Pro", "only 1 of 2 Configurations have a passed Installed run", "none on m2-max", "job-b1"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the sentence lacks %q: %s", want, sentence)
		}
	}
	form := asked.sole(t, "tr.release-detail .asking form")
	if form.attr("action") != "/releases/9.9/verdict" || form.count(t, `input[name="sure"][value="yes"]`) != 1 ||
		form.count(t, `input[name="state"][value="qualified"]`) != 1 || form.count(t, `input[name="note"][value="boots on the Pro"]`) != 1 || form.count(t, `input[name="csrf_token"]`) != 1 {
		t.Errorf("the second click does not carry what the first asked")
	}
	if got := verdictButtons(t, asked); got != "" {
		t.Errorf("the verdict's own buttons stay beside the sentence: %q", got)
	}
	if !asked.hasText(t, `.asking a[href="/releases?open=9.9"]`, "Leave it as it is") {
		t.Error("the sentence offers no way back")
	}
	if r := l.s.releaseNowOrNil("9.9"); r.State != api.ReleaseNoVerdict {
		t.Fatalf("the first click recorded a verdict: %+v", r.Verdicts)
	}

	// The second records it, with who, when and the note.
	w = grace.form("/releases/9.9/verdict", "state", api.ReleaseQualified, "note", "boots on the Pro", "sure", "yes")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/releases?open=9.9" {
		t.Fatalf("the second click: %d to %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	r := l.s.releaseNowOrNil("9.9")
	if r.State != api.ReleaseQualified || len(r.Verdicts) != 1 || r.Verdicts[0].Handle != "grace" || r.Verdicts[0].AccountID != lrGrace || r.Verdicts[0].Note != "boots on the Pro" || !r.Verdicts[0].Time.Equal(l.s.now()) {
		t.Fatalf("the verdict recorded: %+v", r.Verdicts)
	}

	// Once there is one, no M team member has a button, and a post is refused.
	for who, b := range map[string]*browser{"its author": grace, "another member": linus} {
		page := b.releases("?open=9.9")
		if got := verdictButtons(t, page); got != "" {
			t.Errorf("%s has buttons once there is a verdict: %q", who, got)
		}
		if !page.says(t, ".release-verdict", "Only a Maintainer changes or revokes it") {
			t.Errorf("%s is not told who changes a verdict", who)
		}
		for _, state := range []string{api.ReleaseNotQualified, api.ReleaseRevoked} {
			w := b.form("/releases/9.9/verdict", "state", state, "note", "second thoughts", "sure", "yes")
			if w.Code != http.StatusForbidden || !parseHTML(t, w.Body.String()).says(t, ".flash-alert", "Maintainer") {
				t.Errorf("%s posting %s over a verdict: %d", who, state, w.Code)
			}
		}
	}
	if r := l.s.releaseNowOrNil("9.9"); len(r.Verdicts) != 1 {
		t.Fatalf("an M team member changed a verdict: %+v", r.Verdicts)
	}

	// No Promote for an M team member, on the page or by a post.
	if page := grace.releases("?open=9.9"); page.count(t, ".release-macs form") != 0 || page.count(t, ".release-macs button") != 0 {
		t.Error("an M team member has a button on a Mac's row")
	}
	if w := grace.form("/releases/9.9/macs/m1-pro/promote", "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("an M team member posting a Promotion: %d", w.Code)
	}
}

func TestAVerdictNeedsANoteAndQualifiedNeedsAPassedRun(t *testing.T) {
	l, grace, _, ravi := twoReleases(t)
	relFreeze(t, l.s, "10.0", l.s.now(), l.who(lrGrace), kernelPackage("10.0-1"),
		api.ReleaseMac{Mac: "m1-pro", DeviceID: "d-m1-pro", Board: "apple,j316s", Configuration: "j316s, 10 CPU and 16 GPU cores"})
	for _, c := range []struct {
		who            *browser
		release, state string
		note           string
		status         int
		says           string
	}{
		{grace, "9.9", api.ReleaseNotQualified, "   ", http.StatusUnprocessableEntity, "needs a note"},
		{ravi, "9.9", api.ReleaseQualified, strings.Repeat("x", 1001), http.StatusUnprocessableEntity, "at most 1000"},
		{grace, "10.0", api.ReleaseQualified, "looks fine", http.StatusConflict, "no passed Installed run"},
		{ravi, "9.9", api.ReleaseRevoked, "nothing to revoke", http.StatusConflict, "no verdict to revoke"},
		{grace, "9.9", "splendid", "x", http.StatusBadRequest, "qualified"},
	} {
		for _, sure := range []string{"", "yes"} {
			w := c.who.form("/releases/"+c.release+"/verdict", "state", c.state, "note", c.note, "sure", sure)
			page := parseHTML(t, w.Body.String())
			if w.Code != c.status || !page.says(t, ".flash-alert", c.says) {
				t.Errorf("%s on %s with note %.12q (sure %q): %d %s", c.state, c.release, c.note, sure, w.Code, w.Body)
				continue
			}
			if page.count(t, `tr.release-detail`) != 1 || page.count(t, ".asking") != 0 {
				t.Errorf("a refused verdict on %s does not leave its Release open, unasked", c.release)
			}
		}
	}
	for _, name := range []string{"9.9", "10.0"} {
		if r := l.s.releaseNowOrNil(name); r.State != api.ReleaseNoVerdict {
			t.Errorf("a refused verdict was recorded on %s: %+v", name, r.Verdicts)
		}
	}
	if w := grace.form("/releases/7.7/verdict", "state", api.ReleaseNotQualified, "note", "x", "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("a verdict on a Release that is not listed: %d", w.Code)
	}
}

func TestAMaintainerChangesRevokesAndRecordsAgainEachAskedTwice(t *testing.T) {
	l, _, _, ravi := twoReleases(t)
	if got := verdictButtons(t, ravi.releases("?open=9.9")); got != "Record qualified, Record not qualified" {
		t.Fatalf("a Maintainer's buttons while there is no verdict: %q", got)
	}
	relVerdict(t, l.s, l.who(lrGrace), "9.9", api.ReleaseQualified, "boots on the Pro")
	if err := l.s.store.addPromotion("9.9", "d-m1-pro", api.ReleasePromotion{Time: l.s.now(), Mac: "m1-pro", Job: "job-a2", Kernel: "7.1.12-aurora-9.9", AccountID: maintainerID, Handle: "ravi"}); err != nil {
		t.Fatal(err)
	}
	if got := verdictButtons(t, ravi.releases("?open=9.9")); got != "Change to qualified, Change to not qualified, Revoke" {
		t.Fatalf("a Maintainer's buttons over a verdict: %q", got)
	}

	steps := []struct {
		state, note string
		sentence    []string
		after       string
	}{
		{api.ReleaseNotQualified, "the Max panics", []string{`Record "not qualified" for Release 9.9 as @ravi`, `this replaces "qualified", recorded by @grace`}, "Change to qualified, Change to not qualified, Revoke"},
		{api.ReleaseRevoked, "judged on the wrong build", []string{"Revoke the verdict on Release 9.9", "m1-pro was promoted from it and will stay promoted"}, "Record qualified, Record not qualified"},
		{api.ReleaseQualified, "judged again on the right one", []string{`Record "qualified" for Release 9.9 as @ravi`, `this replaces "revoked"`}, "Change to qualified, Change to not qualified, Revoke"},
	}
	for i, step := range steps {
		w := ravi.form("/releases/9.9/verdict", "state", step.state, "note", step.note)
		if w.Code != http.StatusOK {
			t.Fatalf("%s, the first click: %d %s", step.state, w.Code, w.Body)
		}
		sentence := parseHTML(t, w.Body.String()).sole(t, ".asking button.armed").words()
		for _, want := range step.sentence {
			if !strings.Contains(sentence, want) {
				t.Errorf("%s: the sentence lacks %q: %s", step.state, want, sentence)
			}
		}
		if r := l.s.releaseNowOrNil("9.9"); len(r.Verdicts) != i+1 {
			t.Fatalf("%s: the first click recorded: %+v", step.state, r.Verdicts)
		}
		l.clock.Advance(time.Minute)
		if w := ravi.form("/releases/9.9/verdict", "state", step.state, "note", step.note, "sure", "yes"); w.Code != http.StatusSeeOther {
			t.Fatalf("%s, the second click: %d %s", step.state, w.Code, w.Body)
		}
		r := l.s.releaseNowOrNil("9.9")
		if r.State != step.state || len(r.Verdicts) != i+2 || r.Verdicts[0].Handle != "ravi" || r.Verdicts[0].Note != step.note {
			t.Fatalf("after %s: %s, %+v", step.state, r.State, r.Verdicts)
		}
		if got := verdictButtons(t, ravi.releases("?open=9.9")); got != step.after {
			t.Errorf("a Maintainer's buttons after %s: %q", step.state, got)
		}
	}

	// Revoking undoes nothing: the Mac stays promoted, and is marked.
	l.clock.Advance(time.Minute)
	relVerdict(t, l.s, l.who(maintainerID), "9.9", api.ReleaseRevoked, "once more")
	page := ravi.releases("?open=9.9")
	if got := page.sole(t, `tr.release-mac[id="release-9.9-mac-m1-pro"] td[data-label="Promoted"]`).words(); !strings.Contains(got, api.PromotedFromRevoked) {
		t.Errorf("a Mac promoted from a revoked Release: %q", got)
	}
	if got := page.sole(t, `tr.release-row[id="release-9.9"] td[data-label="Verdict"]`).words(); !strings.Contains(got, "revoked") {
		t.Errorf("the revoked Release's badge: %q", got)
	}
	if page.count(t, "ol.verdict-history li") != 5 {
		t.Errorf("the history keeps %d verdicts, want 5", page.count(t, "ol.verdict-history li"))
	}
}

// An Installed run that fails after "qualified" changes nothing by itself:
// the page says so beside the verdict.
func TestALaterFailedRunShowsAsAWarningOnTheRelease(t *testing.T) {
	l, grace, _, _ := twoReleases(t)
	relVerdict(t, l.s, l.who(lrGrace), "9.9", api.ReleaseQualified, "boots on the Pro")
	l.clock.Advance(time.Hour)
	relRun(t, l.s, "job-a3", "9.9", "m1-pro", l.s.now(), api.OutcomeHung)
	page := grace.releases("?open=9.9")
	if !page.says(t, ".release-verdict .release-warning", "job-a3 on m1-pro failed after this verdict") {
		t.Errorf("no warning of the later failure: %q", page.sole(t, ".release-verdict").words())
	}
	if got := page.sole(t, `tr.release-row[id="release-9.9"] td[data-label="Verdict"]`).words(); !strings.Contains(got, "qualified") || strings.Contains(got, "not qualified") {
		t.Errorf("the state changed by itself: %q", got)
	}
}

func TestPromoteIsOnEachMacRowWithAPassedRunForAMaintainer(t *testing.T) {
	l, _, _, ravi := twoReleases(t)
	page := ravi.releases("?open=9.9")
	pro := page.sole(t, `tr.release-mac[id="release-9.9-mac-m1-pro"]`)
	if got := pro.sole(t, `td[data-label="Promoted"] form`); got.attr("action") != "/releases/9.9/macs/m1-pro/promote" || got.sole(t, "button").words() != "Promote" {
		t.Errorf("Promote on a Mac with a passed run: %v", got.attrs)
	}
	if page.count(t, `tr.release-mac[id="release-9.9-mac-m2-max"] form`) != 0 {
		t.Error("Promote on a Mac with no passed run")
	}
	// A Mac with no passed run is refused whatever is posted, asked or sure.
	for _, sure := range []string{"", "yes"} {
		w := ravi.form("/releases/9.9/macs/m2-max/promote", "sure", sure)
		if w.Code != http.StatusConflict || !parseHTML(t, w.Body.String()).says(t, ".flash-alert", "no passed Installed run") {
			t.Errorf("a Promotion with no passed run (sure %q): %d %s", sure, w.Code, w.Body)
		}
	}
	// What a Promotion lacks is the lab's to say, on the page: here a run
	// with no evidence behind it.
	relFreeze(t, l.s, "9.7", l.s.now().Add(-72*time.Hour), l.who(maintainerID), kernelPackage("9.7-1"),
		api.ReleaseMac{Mac: "left-mac", DeviceID: "d-left-mac", Board: "apple,j313", Configuration: "j313, 8 CPU and 8 GPU cores"})
	relRun(t, l.s, "job-l1", "9.7", "left-mac", l.s.now().Add(-71*time.Hour), api.OutcomePass)
	if w := ravi.form("/releases/9.7/macs/left-mac/promote"); w.Code != http.StatusBadRequest || !parseHTML(t, w.Body.String()).says(t, ".flash-alert", "job-l1") ||
		parseHTML(t, w.Body.String()).count(t, "tr.release-detail") != 1 {
		t.Errorf("a Promotion the lab cannot make for a Mac of the Release: %d %s", w.Code, w.Body)
	}
	for _, target := range []string{"/releases/9.9/macs/no-such-mac/promote", "/releases/7.7/macs/m1-pro/promote"} {
		if w := ravi.form(target, "sure", "yes"); w.Code != http.StatusNotFound {
			t.Errorf("POST %s: %d", target, w.Code)
		}
	}
	if got, _ := l.s.store.promotions("9.9"); len(got) != 0 {
		t.Fatalf("a refused Promotion was recorded: %+v", got)
	}
}

func TestAPromotionOfAVolunteersMacAskedThroughTheSiteIsRefused(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("vol-air", "j313", lrVolunteers(api.AccessQualification, l.s.now().Add(2*time.Hour)))
	ravi := l.lrMaintainer()
	relFreeze(t, l.s, "9.9", l.s.now().Add(-time.Hour), l.who(maintainerID), kernelPackage("9.9-1"),
		api.ReleaseMac{Mac: "vol-air", DeviceID: "d-vol-air", Board: "apple,j313", Configuration: "j313, 8 CPU and 8 GPU cores"})
	relRun(t, l.s, "job-v1", "9.9", "vol-air", l.s.now().Add(-time.Minute), api.OutcomePass)

	page := ravi.releases("?open=9.9")
	cell := page.sole(t, `tr.release-mac[id="release-9.9-mac-vol-air"] td[data-label="Promoted"]`)
	if cell.count(t, "form") != 0 || !strings.Contains(cell.words(), "a Volunteer's Mac is never promoted") {
		t.Errorf("a Volunteer's Mac with a passed run: %q", cell.words())
	}
	for _, sure := range []string{"", "yes"} {
		w := ravi.form("/releases/9.9/macs/vol-air/promote", "sure", sure)
		if w.Code != http.StatusForbidden || !parseHTML(t, w.Body.String()).says(t, ".flash-alert", "it is a Volunteer's Mac") {
			t.Errorf("a Promotion of a Volunteer's Mac through the site (sure %q): %d %s", sure, w.Code, w.Body)
		}
	}
	if got, _ := l.s.store.promotions("9.9"); len(got) != 0 {
		t.Fatalf("a Volunteer's Mac was promoted: %+v", got)
	}
	if kg := l.s.dev("vol-air").snapshot().KnownGood; kg != lrKnownGood {
		t.Errorf("the Volunteer's Mac's Known-good changed to %s", kg)
	}
}

// pageAs calls a page's handler for a person signed in, on a lab whose Mac
// is the simulator: the page's own code, without the browser's sign-in.
func (l *lab) pageAs(p Person, h func(*Server, http.ResponseWriter, *http.Request), target string, values map[string]string, fields ...string) *httptest.ResponseRecorder {
	l.t.Helper()
	form := url.Values{}
	for i := 0; i+1 < len(fields); i += 2 {
		form.Set(fields[i], fields[i+1])
	}
	r := httptest.NewRequest("POST", target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", formType)
	for name, value := range values {
		r.SetPathValue(name, value)
	}
	r = r.WithContext(context.WithValue(r.Context(), signedInKey{}, &signedIn{Person: p, csrf: "the-csrf-token"}))
	w := httptest.NewRecorder()
	h(l.s, w, r)
	return w
}

func TestPromoteOnTheReleasesPageAsksTwiceThenPromotesFromThePassedRun(t *testing.T) {
	l, msha, _ := installedLab(t, nil)
	j := l.run(api.JobSpec{InstalledManifest: msha, Holder: "tester"})
	l.expect(j, api.OutcomePass)
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Kernel == j.Result.BootKernel })
	ada, err := l.s.person(testAccount(harnessMaintainer))
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]string{"release": "9.9", "mac": "sim-mac"}

	// A Mac of the Release that the lab no longer has: said on the page,
	// which is still there.
	l.s.mu.Lock()
	gone := l.s.devs["sim-mac"]
	delete(l.s.devs, "sim-mac")
	l.s.mu.Unlock()
	if w := l.pageAs(ada, (*Server).pPromote, "/releases/9.9/macs/sim-mac/promote", at, "sure", "yes"); w.Code != http.StatusConflict ||
		!parseHTML(t, w.Body.String()).says(t, ".flash-alert", "no device sim-mac") {
		t.Errorf("a Promotion for a Mac the lab no longer has: %d %s", w.Code, w.Body)
	}
	l.s.mu.Lock()
	l.s.devs["sim-mac"] = gone
	l.s.mu.Unlock()
	// A check-in while the Mac was out of the fleet got 401, and lab-agent
	// tries again only 5s later, past these tests' quietGrace (3s): the
	// Promotion's call to the Mac would count it lost. The Mac is back once
	// it has checked in again.
	back := l.s.now()
	l.waitDevice(func(d api.Device) bool { return d.LastSeen.After(back) })

	w := l.pageAs(ada, (*Server).pPromote, "/releases/9.9/macs/sim-mac/promote", at)
	if w.Code != http.StatusOK {
		t.Fatalf("the first click: %d %s", w.Code, w.Body)
	}
	asked := parseHTML(t, w.Body.String())
	sentence := asked.sole(t, "tr.release-detail .asking button.armed").words()
	for _, want := range []string{"Promote sim-mac from Release 9.9 as @" + harnessMaintainer, j.ID, j.Result.BootKernel, "Release 9.9 has no verdict yet"} {
		if !strings.Contains(sentence, want) {
			t.Errorf("the sentence lacks %q: %s", want, sentence)
		}
	}
	if form := asked.sole(t, ".asking form"); form.attr("action") != "/releases/9.9/macs/sim-mac/promote" || form.count(t, `input[name="sure"][value="yes"]`) != 1 {
		t.Error("the second click does not post the Promotion")
	}
	if got, _ := l.s.store.promotions("9.9"); len(got) != 0 {
		t.Fatalf("the first click promoted: %+v", got)
	}

	w = l.pageAs(ada, (*Server).pPromote, "/releases/9.9/macs/sim-mac/promote", at, "sure", "yes")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/releases?open=9.9" {
		t.Fatalf("the second click: %d to %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	got, _ := l.s.store.promotions("9.9")
	if len(got) != 1 || got[0].Mac != "sim-mac" || got[0].Job != j.ID || got[0].Handle != harnessMaintainer {
		t.Fatalf("the Promotion recorded: %+v", got)
	}
	if kg := l.s.dev("sim-mac").snapshot().KnownGood; kg != j.Result.BootKernel {
		t.Errorf("the Mac's Known-good is %s, not the Release's %s", kg, j.Result.BootKernel)
	}

	// The page then shows it promoted, with no button.
	r := httptest.NewRequest("GET", "/releases?open=9.9", nil)
	r = r.WithContext(context.WithValue(r.Context(), signedInKey{}, &signedIn{Person: ada, csrf: "the-csrf-token"}))
	shown := httptest.NewRecorder()
	l.s.pReleases(shown, r)
	cell := parseHTML(t, shown.Body.String()).sole(t, `tr.release-mac[id="release-9.9-mac-sim-mac"] td[data-label="Promoted"]`)
	if cell.count(t, "form") != 0 || !strings.Contains(cell.words(), "promoted by @"+harnessMaintainer) {
		t.Errorf("a promoted Mac's row: %q", cell.words())
	}
}

// What a visitor sees: the lab's verdict beside the candidate set the
// Release matches, and nothing else of a Release.
func TestCandidatesShowsTheLabsVerdictBesideTheMatchingSet(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(onSet(working(t, "m2-max-image2"), "edge-1"), "verified:t")
	l.add(onSet(working(t, "m1-pro-converged"), "edge-2"), "verified:u")
	kernel := checkedPackage(t, goldenDoc(t, "m2-max-image2"), "linux-aurora")
	if kernel == checkedPackage(t, goldenDoc(t, "m1-pro-converged"), "linux-aurora") {
		t.Fatal("the two sets are on one kernel package: the test tells nothing apart")
	}
	linus := Person{AccountID: 8102, Handle: "linus-the-judge", MTeam: true}
	ravi := Person{AccountID: 3003, Handle: "ravi-the-maintainer", Maintainer: true}
	frozen := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	mac := api.ReleaseMac{Mac: "secret-mac-name", DeviceID: "d-1", Board: "apple,j414c", Configuration: "j414c, 12 CPU and 38 GPU cores"}

	lines := func() map[string]string {
		got := map[string]string{}
		page := l.page("/candidates")
		for _, set := range []string{"edge-1", "edge-2"} {
			row := page.sole(t, `tr.candidate-row[id="candidate-`+set+`"]`)
			onRow := ""
			if found := row.all(t, ".lab-verdict"); len(found) == 1 {
				onRow = found[0].words()
			} else if len(found) > 1 {
				t.Fatalf("%s carries %d lines of the lab's", set, len(found))
			}
			onPage := ""
			if found := l.page("/candidates/"+set).all(t, "#lab-verdict"); len(found) == 1 {
				onPage = found[0].words()
			}
			if onRow != onPage {
				t.Errorf("%s: the list says %q and the set's page %q", set, onRow, onPage)
			}
			got[set] = onRow
			if !row.says(t, ".verdict", "Passing: Verified runs on 1 model, nothing failing") {
				t.Errorf("%s lost its own line: %q", set, row.words())
			}
		}
		return got
	}
	same(t, "with no Release", lines(), map[string]string{"edge-1": "", "edge-2": ""})

	// A Release with no verdict says nothing; one matching no set shows
	// nowhere, the one of the set's kernel package alone included.
	relFreeze(t, l.s, "rel-2026.11", frozen, linus, setsPackages(t, goldenDoc(t, "m2-max-image2")), mac)
	relFreeze(t, l.s, "rel-unmatched", frozen, linus, kernelPackage("0.0.1-1"), mac)
	relFreeze(t, l.s, "rel-kernel-only", frozen, linus, kernelPackage(kernel), mac)
	relRun(t, l.s, "job-secret-1", "rel-2026.11", "secret-mac-name", frozen.Add(time.Hour), api.OutcomePass)
	relRun(t, l.s, "job-secret-2", "rel-unmatched", "secret-mac-name", frozen.Add(time.Hour), api.OutcomePass)
	same(t, "with no verdict", lines(), map[string]string{"edge-1": "", "edge-2": ""})

	day := func(at time.Time) string { return at.UTC().Format(time.DateOnly) }
	judge := func(by Person, release, state, note string) time.Time {
		t.Helper()
		relVerdict(t, l.s, by, release, state, note)
		return l.s.releaseNowOrNil(release).Verdicts[0].Time
	}
	at := judge(linus, "rel-2026.11", api.ReleaseQualified, "a note nobody outside reads")
	judge(linus, "rel-unmatched", api.ReleaseQualified, "another private note")
	judge(linus, "rel-kernel-only", api.ReleaseNotQualified, "a kernel alone")
	same(t, "qualified", lines(), map[string]string{"edge-1": "Qualified by the lab, " + day(at), "edge-2": ""})
	at = judge(ravi, "rel-2026.11", api.ReleaseNotQualified, "a second private note")
	same(t, "not qualified", lines(), map[string]string{"edge-1": "Not qualified, " + day(at), "edge-2": ""})
	at = judge(ravi, "rel-2026.11", api.ReleaseRevoked, "a third private note")
	same(t, "revoked", lines(), map[string]string{"edge-1": "Qualification revoked, " + day(at), "edge-2": ""})

	// No handle, note, Mac, job or Release name reaches a visitor, on any public page.
	for _, target := range []string{"/", "/candidates", "/candidates/edge-1", "/candidates/edge-2", "/reports", "/matrix", "/data"} {
		body := l.do("GET", target, "").Body.String()
		for _, never := range []string{"linus-the-judge", "ravi-the-maintainer", "private note", "secret-mac-name", "job-secret", "rel-2026.11", "rel-unmatched", "rel-kernel-only", "j414c, 12 CPU", "/releases"} {
			if strings.Contains(body, never) {
				t.Errorf("%s says %q", target, never)
			}
		}
	}
	// Signed out there is no Releases page: not found on a lab with no sign-in.
	if got := l.status("/releases"); got != http.StatusNotFound {
		t.Errorf("/releases for a visitor: %d", got)
	}
}

// Two Releases can carry one set's packages: the set shows the verdict
// recorded last.
func TestASetMatchedByTwoReleasesShowsTheVerdictRecordedLast(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(onSet(working(t, "m2-max-image2"), "edge-1"), "verified:t")
	packages := setsPackages(t, goldenDoc(t, "m2-max-image2"))
	ravi := Person{AccountID: 3003, Handle: "ravi", Maintainer: true}
	relFreeze(t, l.s, "first", time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), ravi, packages)
	relFreeze(t, l.s, "second", time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), ravi, packages)
	relVerdict(t, l.s, ravi, "second", api.ReleaseNotQualified, "no")
	time.Sleep(2 * time.Millisecond)
	relVerdict(t, l.s, ravi, "first", api.ReleaseNotQualified, "no")
	time.Sleep(2 * time.Millisecond)
	relVerdict(t, l.s, ravi, "first", api.ReleaseRevoked, "recorded on the wrong one")
	if got := l.page("/candidates").sole(t, `tr.candidate-row .lab-verdict`).words(); !strings.HasPrefix(got, "Qualification revoked, ") {
		t.Errorf("the set's line: %q", got)
	}
}

func checkedPackage(t *testing.T, body doc, name string) string {
	t.Helper()
	for _, item := range at(body, "system", "packages").([]any) {
		if pkg := item.(doc); pkg["name"] == name {
			return pkg["version"].(string)
		}
	}
	t.Fatalf("the report has no package %s", name)
	return ""
}

// The lab's line is the page's own read of labd.db: with it gone the
// public page still shows, without the line.
func TestCandidatesStillShowsWhenTheReleasesCannotBeRead(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(onSet(working(t, "m2-max-image2"), "edge-1"), "verified:t")
	relFreeze(t, l.s, "rel-1", time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), Person{AccountID: 3003, Handle: "ravi", Maintainer: true},
		setsPackages(t, goldenDoc(t, "m2-max-image2")))
	relVerdict(t, l.s, Person{AccountID: 3003, Handle: "ravi", Maintainer: true}, "rel-1", api.ReleaseNotQualified, "no")
	if l.page("/candidates").count(t, ".lab-verdict") != 1 {
		t.Fatal("the lab's line is not on the page")
	}
	l.s.store.db.Close()
	for _, target := range []string{"/candidates", "/candidates/edge-1"} {
		if page := l.page(target); page.count(t, "main") != 1 || page.count(t, ".lab-verdict") != 0 || page.count(t, "#lab-verdict") != 0 {
			t.Errorf("%s with labd.db shut", target)
		}
	}
}

// A Release stored before the freeze refused a package named twice says
// nothing certain of that package: it links no set, whichever version of
// it comes last, and shows no verdict on Candidates.
func TestAStoredReleaseNamingAPackageTwiceLinksNoSet(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(onSet(working(t, "m2-max-image2"), "edge-1"), "verified:t")
	packages := setsPackages(t, goldenDoc(t, "m2-max-image2"))
	ravi := Person{AccountID: 3003, Handle: "ravi", Maintainer: true}
	kernel := checkedPackage(t, goldenDoc(t, "m2-max-image2"), "linux-aurora")
	twice := append([]api.ReleasePackage{{Name: "linux-aurora", Version: "0.0.1-1"}}, packages...)
	if twice[len(twice)-1].Name == "linux-aurora" || kernel == "0.0.1-1" {
		t.Fatal("the run's kernel package is not after the other version")
	}
	relFreeze(t, l.s, "twice", time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), ravi, twice)
	relVerdict(t, l.s, ravi, "twice", api.ReleaseNotQualified, "no")
	if n := l.page("/candidates").count(t, ".lab-verdict"); n != 0 {
		t.Errorf("a Release naming linux-aurora twice put %d verdict lines on Candidates", n)
	}
	r := httptest.NewRequest("GET", "/releases", nil)
	r = r.WithContext(context.WithValue(r.Context(), signedInKey{}, &signedIn{Person: ravi, csrf: "the-csrf-token"}))
	shown := httptest.NewRecorder()
	l.s.pReleases(shown, r)
	if n := parseHTML(t, shown.Body.String()).sole(t, `tr.release-row[id="release-twice"]`).count(t, `td[data-label="Candidate set"] a`); n != 0 {
		t.Errorf("a Release naming linux-aurora twice links %d sets on Releases", n)
	}
}
