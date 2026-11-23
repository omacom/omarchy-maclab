package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
	"github.com/omacom/omarchy-maclab/internal/reports"
)

// Publishing performance values to reports.db, and Counted (spec section
// 22, "Publishing and Counted").

// pubMac enrolls a Mac with its Configuration known: a Lab-owned one, or a
// Volunteer's with publishing on or off at the Mac. Its name, label,
// hostname and Volunteer's name are what reports.db must never hold.
func (l *signInLab) pubMac(name string, volunteer, publish bool) *dev {
	l.t.Helper()
	return l.lrMac(name, "j314s", func(d *api.Device) {
		d.Label, d.Facts.Hostname = name+"-on-the-desk", name+"-host"
		d.Configuration = &api.Configuration{CPU: 10, GPU: 16}
		if volunteer {
			lrVolunteers(api.AccessFull, l.s.now().Add(2*time.Hour))(d)
			d.VolunteerName, d.Publish = name+"-owner", publish
		}
	})
}

// pubRun is a run of bench-1 on the Mac's enrollment that succeeded with
// these values, metric then value: on Omarchy of the Release, or in macOS
// at build 27C101 when release is "".
func (l *signInLab) pubRun(mac, release string, values ...any) perf.Run {
	l.t.Helper()
	run := perf.Run{Mac: mac, Enrollment: l.s.dev(mac).snapshot().ID, Set: "bench-1", Side: perf.SideOmarchy, Release: release, List: sets.Quick}
	if release == "" {
		run.Side, run.MacOSVersion, run.MacOSBuild = perf.SideMacOS, "27.2.1", "27C101"
	}
	return l.pubRunOf(run, values...)
}

func (l *signInLab) pubRunOf(run perf.Run, values ...any) perf.Run {
	l.t.Helper()
	run, err := l.s.store.openPerfRun(run, l.s.now())
	if err != nil {
		l.t.Fatal(err)
	}
	var entries []map[string]any
	for i := 0; i+1 < len(values); i += 2 {
		entries = append(entries, perfEntry(l.t, values[i].(string), values[i+1].(float64), ""))
	}
	c := perfChecked(l.t, run.Side, perf.Succeeded, entries...)
	if run.Side == perf.SideOmarchy {
		c.Booted = map[string]string{"linux-asahi": "6.18.2-1"}
	}
	if _, err := l.s.store.takePerf(run.ID, c, l.s.now()); err != nil {
		l.t.Fatal(err)
	}
	return run
}

// pubRelease lists a Release as a freeze of its manifest would, of this
// kernel.
func (l *signInLab) pubRelease(name, kernel string, at time.Time) {
	l.t.Helper()
	if err := l.s.store.addRelease(api.Release{Name: name, FrozenBy: "ravi", FrozenByID: maintainerID, FrozenAt: at, ManifestSHA256: relSHA(name),
		Kernel: kernel, Packages: kernelPackage(kernel), Macs: []api.ReleaseMac{}}); err != nil {
		l.t.Fatal(err)
	}
}

// published is reports.db's Performance.
func (l *signInLab) published() reports.Performance {
	l.t.Helper()
	p, err := l.s.reports.Performance()
	if err != nil {
		l.t.Fatal(err)
	}
	return p
}

// kinds is each published Mac's kind and its count of values, sorted.
func kinds(p reports.Performance) string {
	n := map[string]int{}
	for _, v := range p.Values {
		n[v.Mac]++
	}
	var out []string
	for _, m := range p.Macs {
		out = append(out, fmt.Sprintf("%s:%d", m.Kind, n[m.Key]))
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func (l *signInLab) keyOf(mac string) string {
	l.s.dev(mac).mu.Lock()
	defer l.s.dev(mac).mu.Unlock()
	return l.s.dev(mac).d.PerfKey
}

func (l *signInLab) kindOf(mac string) string {
	for _, m := range l.published().Macs {
		if m.Key == l.keyOf(mac) && m.Key != "" {
			return m.Kind
		}
	}
	return "absent"
}

// atTheMac is the Mac's check-in after its Volunteer turned publishing on
// or off at it.
func (l *signInLab) atTheMac(mac string, publish bool) {
	l.t.Helper()
	v := l.s.dev(mac)
	d := v.snapshot()
	v.heartbeat(api.Checkin{BootID: "boot-1", Kernel: d.Kernel, AccessLevel: d.AccessLevel, Publish: publish})
	if got := v.snapshot().Publish; got != publish {
		l.t.Fatalf("%s's publishing is %v after the check-in", mac, got)
	}
}

// privacyHeld fails the test when reports.db's Performance names any of
// these.
func (l *signInLab) privacyHeld(never ...string) {
	l.t.Helper()
	b, _ := json.Marshal(l.published())
	for _, s := range never {
		if strings.Contains(string(b), s) {
			l.t.Errorf("reports.db holds %q: %s", s, b)
		}
	}
}

func TestAVolunteersMacWithPublishingOffIsNeverInReportsDB(t *testing.T) {
	l := leaseLab(t)
	l.pubRelease("rel-alpha", "6.18.2-1-asahi", l.s.now())
	for _, m := range []struct {
		name               string
		volunteer, publish bool
	}{{"lab-mac-alpha", false, false}, {"vera-macbook", true, true}, {"quiet-macbook", true, false}} {
		l.pubMac(m.name, m.volunteer, m.publish)
		l.pubRun(m.name, "rel-alpha", "coremark-single", 100.0, "fio-seq-read", 2000.0)
		l.pubRun(m.name, "", "speedometer", 30.0)
	}
	l.s.publishPerf()

	p := l.published()
	if got := kinds(p); got != "lab_owned:3 volunteer:3" {
		t.Fatalf("published Macs: %s", got)
	}
	if l.kindOf("quiet-macbook") != "absent" {
		t.Fatalf("a Volunteer's Mac with publishing off is in reports.db: %s", kinds(p))
	}
	if len(p.Releases) != 1 || p.Releases[0].Kernel != "6.18.2-1-asahi" || p.Releases[0].Verdict != reports.PerfNoVerdictYet || p.Releases[0].Position != 1 {
		t.Fatalf("the Release: %+v", p.Releases)
	}
	for _, v := range p.Values {
		if v.Side == perf.SideMacOS && (v.MacOSVersion != "27.2.1" || v.MacOSBuild != "27C101" || v.Release != "") {
			t.Errorf("a macOS value: %+v", v)
		}
		if v.Metric == "fio-seq-read" && (v.Capacity == nil || *v.Capacity != 512) {
			t.Errorf("a Storage value without its SSD's capacity: %+v", v)
		}
		if v.Side == perf.SideOmarchy && v.Booted["linux-asahi"] != "6.18.2-1" {
			t.Errorf("an Omarchy value without what it booted: %+v", v)
		}
	}
	never := []string{"lab-mac-alpha", "vera-macbook", "quiet-macbook", "-host", "-on-the-desk", "-owner", "rel-alpha", "d-", "linus", "ravi"}
	l.privacyHeld(never...)

	// Turned off at the Mac, its values leave reports.db with that
	// check-in; on again, they come back under the same key.
	key := l.keyOf("vera-macbook")
	l.atTheMac("vera-macbook", false)
	if got := l.kindOf("vera-macbook"); got != "absent" {
		t.Fatalf("with publishing turned off: %s", got)
	}
	if got := kinds(l.published()); got != "lab_owned:3" {
		t.Fatalf("after vera-macbook turned publishing off: %s", got)
	}
	l.atTheMac("vera-macbook", true)
	if got := l.kindOf("vera-macbook"); got != reports.PerfVolunteer || l.keyOf("vera-macbook") != key {
		t.Fatalf("with publishing on again: %s", got)
	}

	// Checked at each copy: a value that comes while publishing is off
	// does not reach reports.db.
	l.atTheMac("quiet-macbook", false)
	l.pubRun("quiet-macbook", "", "jetstream", 200.0)
	l.s.publishPerf()
	if l.kindOf("quiet-macbook") != "absent" {
		t.Fatal("a copy took the values of a Mac whose publishing is off")
	}
	l.privacyHeld(never...)
}

func TestCountedChangesOnlyTheKindItsValuesCarry(t *testing.T) {
	l := leaseLab(t)
	l.pubRelease("rel-alpha", "6.18.2-1-asahi", l.s.now())
	l.pubMac("lab-mac-alpha", false, false)
	l.pubMac("vera-macbook", true, true)
	l.pubMac("lin-macbook", true, true)
	l.lrLend("lin-macbook", lrLinus)
	for _, m := range []string{"lab-mac-alpha", "vera-macbook", "lin-macbook"} {
		l.pubRun(m, "rel-alpha", "coremark-single", 100.0)
	}
	ravi, linus := l.lrMaintainer(), l.signedIn(lrLinus)
	grace := l.lrMember(lrGrace)
	l.s.publishPerf()
	if got := l.kindOf("vera-macbook"); got != reports.PerfVolunteer {
		t.Fatalf("before any mark: %s", got)
	}
	if page := ravi.macPage("vera-macbook", ""); !strings.Contains(page, "not Counted") || !strings.Contains(page, "Mark Counted") {
		t.Errorf("the Mac's page has no Counted mark to give: %s", part(t, page, "<dl class=\"facts\">", "</dl>"))
	}

	// A Maintainer's mark, on the Mac's page: its values from before are
	// Counted, and those after.
	if w := ravi.post("/macs/vera-macbook/counted", "counted", "on", "back", "mac"); w.Code != http.StatusSeeOther {
		t.Fatalf("Mark Counted: %d %s", w.Code, refusalIn(w.Body.String()))
	}
	if got := l.kindOf("vera-macbook"); got != reports.PerfCounted {
		t.Fatalf("marked: %s", got)
	}
	l.pubRun("vera-macbook", "", "speedometer", 30.0)
	l.s.publishPerf()
	if got := kinds(l.published()); got != "counted:2 lab_owned:1 volunteer:1" {
		t.Fatalf("a value after the mark: %s", got)
	}
	if page := ravi.macPage("vera-macbook", ""); !strings.Contains(page, "Counted: a Maintainer marked it") || !strings.Contains(page, "Take the Counted mark away") {
		t.Errorf("the page of a marked Mac: %s", part(t, page, "<dl class=\"facts\">", "</dl>"))
	}
	if page := ravi.macPage("vera-macbook", tabRecords); !strings.Contains(page, "@ravi marked it Counted") {
		t.Error("the mark is not in the Mac's record")
	}
	if w := grace.post("/macs/vera-macbook/counted", "counted", "off", "back", "mac"); w.Code == http.StatusSeeOther {
		t.Error("an M team member took the mark away")
	}

	// And through the API.
	counted := func(b *browser, mac string, on bool) int {
		body, _ := json.Marshal(api.CountedRequest{Counted: &on})
		return b.write("POST", "/api/devices/"+mac+"/counted", string(body), "Content-Type", "application/json").Code
	}
	if code := counted(ravi, "vera-macbook", false); code != 200 || l.kindOf("vera-macbook") != reports.PerfVolunteer {
		t.Fatalf("unmarked through the API: %d %s", code, l.kindOf("vera-macbook"))
	}
	if code := counted(ravi, "vera-macbook", false); code != http.StatusConflict {
		t.Errorf("unmarked twice: %d", code)
	}
	if code := counted(ravi, "lab-mac-alpha", true); code != http.StatusConflict {
		t.Errorf("a Lab-owned Mac marked: %d", code)
	}
	if code := counted(grace, "vera-macbook", true); code == 200 {
		t.Error("an M team member marked a Mac Counted")
	}
	if code := counted(linus, "lin-macbook", true); code == 200 {
		t.Error("a Volunteer marked their own Mac Counted")
	}

	// An M team volunteer's Macs are Counted while they are one.
	l.lrMark(lrLinus)
	l.s.changed(lrLinus)
	if got := l.kindOf("lin-macbook"); got != reports.PerfCounted {
		t.Fatalf("an M team volunteer's Mac: %s", got)
	}
	if page := ravi.macPage("lin-macbook", ""); !strings.Contains(page, "Counted while its Volunteer is on the M team") {
		t.Error("the page of an M team volunteer's Mac does not say it is Counted")
	}
	l.lrUnmark(lrLinus)
	l.s.changed(lrLinus)
	if got := l.kindOf("lin-macbook"); got != reports.PerfVolunteer {
		t.Fatalf("after leaving the M team: %s", got)
	}

	// A Counted Mac whose publishing is off is in no headline: it is not in
	// reports.db at all.
	if code := counted(ravi, "vera-macbook", true); code != 200 {
		t.Fatal(code)
	}
	l.atTheMac("vera-macbook", false)
	if got := l.kindOf("vera-macbook"); got != "absent" {
		t.Fatalf("a Counted Mac with publishing off: %s", got)
	}
	if got := kinds(l.published()); got != "lab_owned:1 volunteer:1" {
		t.Fatalf("what is left: %s", got)
	}
}

func TestAReleaseRecordedNotQualifiedLeavesTheData(t *testing.T) {
	l := leaseLab(t)
	l.pubRelease("rel-alpha", "6.18.2-1-asahi", l.s.now().Add(-time.Hour))
	l.pubRelease("rel-beta", "6.18.3-1-asahi", l.s.now())
	l.pubMac("lab-mac-alpha", false, false)
	l.pubRun("lab-mac-alpha", "rel-alpha", "coremark-single", 100.0)
	l.pubRun("lab-mac-alpha", "rel-beta", "coremark-single", 110.0)
	l.pubRun("lab-mac-alpha", "", "coremark-single", 120.0)
	l.s.publishPerf()
	releases := func() string {
		var out []string
		for _, r := range l.published().Releases {
			out = append(out, fmt.Sprintf("%d %s %s", r.Position, r.Kernel, r.Verdict))
		}
		return strings.Join(out, "; ")
	}
	if got := releases(); got != "1 6.18.2-1-asahi no verdict yet; 2 6.18.3-1-asahi no verdict yet" {
		t.Fatalf("the Releases: %s", got)
	}

	if err := l.s.store.addVerdict("rel-alpha", api.ReleaseVerdict{Time: l.s.now(), State: api.ReleaseQualified, Note: "boots", Handle: "ravi"}); err != nil {
		t.Fatal(err)
	}
	l.s.publishPerf()
	if got := releases(); got != "1 6.18.2-1-asahi qualified; 2 6.18.3-1-asahi no verdict yet" {
		t.Fatalf("one qualified: %s", got)
	}

	ravi := Person{Handle: "ravi", AccountID: maintainerID, Maintainer: true}
	relVerdict(t, l.s, ravi, "rel-beta", api.ReleaseNotQualified, "panics at boot")
	if got := releases(); got != "1 6.18.2-1-asahi qualified" {
		t.Fatalf("one not qualified: %s", got)
	}
	if got := kinds(l.published()); got != "lab_owned:2" {
		t.Fatalf("the values of a Release not qualified stay: %s", got)
	}
	relVerdict(t, l.s, ravi, "rel-alpha", api.ReleaseRevoked, "it was the cable")
	p := l.published()
	if len(p.Releases) != 0 || len(p.Values) != 1 || p.Values[0].Side != perf.SideMacOS {
		t.Fatalf("after a revoked verdict: %+v", p)
	}
	l.privacyHeld("rel-alpha", "rel-beta", "panics", "cable", "ravi")
}

func TestADeletionInLabdIsCarriedToReportsDB(t *testing.T) {
	l := leaseLab(t)
	l.pubRelease("rel-alpha", "6.18.2-1-asahi", l.s.now())
	l.pubMac("lab-mac-alpha", false, false)
	l.pubMac("vera-macbook", true, true)
	value := func(mac, metric string) string {
		for _, v := range l.published().Values {
			if v.Mac == l.keyOf(mac) && v.Metric == metric && v.Value != nil {
				return fmt.Sprint(*v.Value)
			}
		}
		return "none"
	}

	// A rerun that succeeds supersedes the value, through the upload.
	run := l.pubRun("lab-mac-alpha", "rel-alpha", "coremark-single", 100.0)
	l.s.publishPerf()
	if got := value("lab-mac-alpha", "coremark-single"); got != "100" {
		t.Fatalf("the first value: %s", got)
	}
	again, err := l.s.store.openPerfRun(run, l.s.now())
	if err != nil {
		t.Fatal(err)
	}
	up, _ := json.Marshal(map[string]any{"entries": []any{perfEntry(t, "coremark-single", 105, "")}, "state": perf.Succeeded})
	w := l.browser().do("POST", fmt.Sprintf("/api/agent/performance/%d/results", again.ID), string(up), "Authorization", "Bearer dev_lab-mac-alpha")
	if w.Code != 200 {
		t.Fatalf("the rerun's upload: %d %s", w.Code, w.Body)
	}
	if got := value("lab-mac-alpha", "coremark-single"); got != "105" {
		t.Fatalf("after the rerun: %s", got)
	}

	// An earlier enrollment's values under the same name are not this Mac's.
	old := perf.Run{Mac: "vera-macbook", Enrollment: "d-an-earlier-one", Set: "bench-1", Side: perf.SideMacOS, MacOSVersion: "27.1", MacOSBuild: "27B50", List: sets.Quick}
	l.pubRunOf(old, "speedometer", 20.0)
	l.s.publishPerf()
	if got := l.kindOf("vera-macbook"); got != "absent" {
		t.Fatalf("an earlier enrollment's values were published: %s", got)
	}
	l.pubRun("vera-macbook", "rel-alpha", "coremark-single", 90.0)
	l.s.publishPerf()
	if got := kinds(l.published()); got != "lab_owned:1 volunteer:1" {
		t.Fatalf("both Macs: %s", got)
	}

	// A Mac that leaves the lab takes its values with it; back, it brings
	// them back. Retired, they are gone.
	v := l.s.dev("vera-macbook")
	if _, err := l.s.leave(v, "lab-agent uninstall"); err != nil {
		t.Fatal(err)
	}
	if got := l.kindOf("vera-macbook"); got != "absent" {
		t.Fatalf("a Mac that left: %s", got)
	}
	if _, err := l.s.rejoin(v, "lab reset"); err != nil {
		t.Fatal(err)
	}
	if got := l.kindOf("vera-macbook"); got != reports.PerfVolunteer {
		t.Fatalf("a Mac back: %s", got)
	}
	key := l.keyOf("vera-macbook")
	if err := l.s.endEnrollment(v, leaving{how: leftByMaintainer, by: Person{Handle: "ravi", AccountID: maintainerID, Maintainer: true}}); err != nil {
		t.Fatal(err)
	}
	for _, m := range l.published().Macs {
		if m.Key == key {
			t.Fatal("a Retired Mac is still in reports.db")
		}
	}
	if got := kinds(l.published()); got != "lab_owned:1" {
		t.Fatalf("after Retiring: %s", got)
	}
}

// The opaque key is labd's own: no view shows it.
func TestAMacsPerformanceKeyIsShownToNobody(t *testing.T) {
	l := leaseLab(t)
	l.pubRelease("rel-alpha", "6.18.2-1-asahi", l.s.now())
	l.pubMac("lab-mac-alpha", false, false)
	l.pubRun("lab-mac-alpha", "rel-alpha", "coremark-single", 100.0)
	l.s.publishPerf()
	key := l.keyOf("lab-mac-alpha")
	if len(key) != 32 {
		t.Fatalf("the key: %q", key)
	}
	b, _ := json.Marshal(l.s.view(l.s.dev("lab-mac-alpha")))
	if strings.Contains(string(b), key) {
		t.Fatalf("a view shows the key: %s", b)
	}
	ravi := l.lrMaintainer()
	if w := ravi.do("GET", "/api/devices/lab-mac-alpha", ""); strings.Contains(w.Body.String(), key) {
		t.Fatalf("the API shows the key: %s", w.Body)
	}
}
