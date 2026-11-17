package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// What a Lease fences: jobs, lab exec, screenshots, the live journal and
// reset. An M team member needs their own Lease for each; a Maintainer acts
// on a free Mac and waits under another person's Lease.

type fencedAsk struct {
	what string
	ask  func(token string) (*http.Response, string)
}

func (l *lab) fenced() []fencedAsk {
	mac := "/api/devices/" + l.name
	return []fencedAsk{
		{"a job", func(token string) (*http.Response, string) {
			return l.as(token, "POST", "/api/jobs", api.JobSpec{Device: l.name, Kernel: l.artifact("krel=7.2-fenced behavior=ok")})
		}},
		{"lab exec", func(token string) (*http.Response, string) {
			return l.as(token, "POST", mac+"/exec", api.ExecArgs{Command: "uname -r"})
		}},
		{"a screenshot", func(token string) (*http.Response, string) { return l.as(token, "POST", mac+"/screenshot", nil) }},
		{"the kept screens", func(token string) (*http.Response, string) { return l.as(token, "GET", mac+"/screens", nil) }},
		{"the live journal", func(token string) (*http.Response, string) { return l.as(token, "GET", mac+"/logs", nil) }},
		{"a reset", func(token string) (*http.Response, string) { return l.as(token, "POST", mac+"/reset", nil) }},
	}
}

func TestAnMTeamMemberNeedsTheirOwnLeaseForEachFencedThing(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	grace, linus := l.member("grace"), l.member("linus")

	refusedAll := func(when string) {
		t.Helper()
		for _, f := range l.fenced() {
			if r, body := f.ask(grace.Token); r.StatusCode != http.StatusForbidden && r.StatusCode != http.StatusConflict || !strings.Contains(body, "Lease") && !strings.Contains(body, "leased") {
				t.Errorf("%s, grace asked for %s: %s %s", when, f.what, r.Status, body)
			}
		}
		if jobs, _ := l.s.store.jobs(l.name, 100, true); len(jobs) != 0 {
			t.Fatalf("%s, a job of grace's was queued: %+v", when, jobs[0])
		}
	}
	refusedAll("with no Lease")
	l.leaseAs(linus, 0)
	refusedAll("under linus's Lease")
	if r, body := l.as(linus.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != 200 {
		t.Fatalf("linus releasing: %s %s", r.Status, body)
	}

	l.leaseAs(grace, 0)
	for _, f := range l.fenced() {
		r, body := f.ask(grace.Token)
		if r.StatusCode != 200 {
			t.Fatalf("under her own Lease, grace asked for %s: %s %s", f.what, r.Status, body)
		}
		if f.what == "a job" {
			var j api.Job
			json.Unmarshal([]byte(body), &j)
			done := l.jobAPI(j.ID, 60*time.Second)
			l.expect(done, api.OutcomePass)
			if done.Maintainer != "grace" || done.AccountID != testAccount("grace") || !done.UnderLease {
				t.Fatalf("grace's job: handle %q, account %d, under_lease %v", done.Maintainer, done.AccountID, done.UnderLease)
			}
		}
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })

	// What stays with Maintainers, a Lease or not.
	for what, spec := range map[string]api.JobSpec{
		"a baseline":           {Device: l.name, Baseline: true},
		"a kernel from source": {Device: l.name, Source: "https://github.com/AsahiLinux/linux#asahi"},
	} {
		if r, body := l.as(grace.Token, "POST", "/api/jobs", spec); r.StatusCode != http.StatusForbidden || !strings.Contains(body, "Maintainer") {
			t.Errorf("grace asked for %s under her Lease: %s %s", what, r.Status, body)
		}
	}
	for what, ask := range map[string][2]string{
		"another person's Lease ended": {"DELETE", "/api/devices/" + l.name + "/lease?force=1"},
		"the Audit log":                {"GET", "/api/devices/" + l.name + "/audit"},
		"the Mac forgotten":            {"DELETE", "/api/devices/" + l.name},
	} {
		if r, body := l.as(linus.Token, ask[0], ask[1], nil); r.StatusCode != http.StatusForbidden {
			t.Errorf("linus asked for %s: %s %s", what, r.Status, body)
		}
	}
	if !l.s.leased(l.s.dev(l.name)) {
		t.Fatal("grace's Lease did not outlive what linus was refused")
	}
}

// An M team member cancels only their own jobs. A Maintainer cancels any,
// and the job says who did.
func TestAnMTeamMemberCancelsOnlyTheirOwnJobs(t *testing.T) {
	wallClock(t)
	l := newLab(t, true)
	l.baseline()
	grace, linus := l.member("grace"), l.member("linus")
	ada := testMaintainer{Name: harnessMaintainer, Token: harnessToken}

	adas := l.longJobAs(ada, "7.2-adas", 4*time.Second)
	for _, m := range []testMaintainer{grace, linus} {
		if r, body := l.as(m.Token, "POST", "/api/jobs/"+adas+"/cancel", nil); r.StatusCode != http.StatusForbidden || !strings.Contains(body, "own jobs") {
			t.Fatalf("%s canceling ada's job: %s %s", m.Name, r.Status, body)
		}
	}
	l.expect(l.jobAPI(adas, 60*time.Second), api.OutcomePass)

	l.leaseAs(grace, 0)
	hers := l.longJobAs(grace, "7.2-hers", time.Minute)
	if r, body := l.as(linus.Token, "POST", "/api/jobs/"+hers+"/cancel", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("linus canceling grace's job: %s %s", r.Status, body)
	}
	if r, body := l.as(grace.Token, "POST", "/api/jobs/"+hers+"/cancel", nil); r.StatusCode != 200 {
		t.Fatalf("grace canceling her own job: %s %s", r.Status, body)
	}
	if j := l.jobAPI(hers, 60*time.Second); j.Outcome != api.OutcomeCanceled {
		t.Fatalf("grace's canceled job: %s %s", j.Outcome, j.Summary)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })

	// A Maintainer cancels a job under another person's Lease; the Lease
	// stays, and the job says who canceled it.
	hers = l.longJobAs(grace, "7.2-again", time.Minute)
	if r, body := l.as(ada.Token, "POST", "/api/jobs/"+hers+"/cancel", nil); r.StatusCode != 200 {
		t.Fatalf("ada canceling grace's job: %s %s", r.Status, body)
	}
	j := l.jobAPI(hers, 60*time.Second)
	if j.Outcome != api.OutcomeCanceled || !strings.Contains(j.Summary, "@ada") {
		t.Fatalf("grace's job canceled by ada: %s %s", j.Outcome, j.Summary)
	}
	if !l.s.holdsLease(l.s.dev(l.name), Person{AccountID: testAccount("grace")}) {
		t.Fatal("canceling a job ended the Lease it ran under")
	}
}

// A Maintainer acts without a Lease on a free Mac, lab exec excepted
// (exec_record_test.go). Under another person's Lease a Maintainer is
// refused jobs, exec and a reset, still looks, and ends the Lease, which is
// recorded.
func TestAMaintainerActsOnAFreeMacAndWaitsUnderAnothersLease(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	grace := l.member("grace")
	looks := map[string]bool{"a screenshot": true, "the kept screens": true, "the live journal": true}

	for _, f := range l.fenced() {
		if f.what == "a reset" || f.what == "a job" || f.what == "lab exec" {
			continue // each has tests of its own on a free Mac
		}
		if r, body := f.ask(harnessToken); r.StatusCode != 200 {
			t.Fatalf("on a free Mac, ada asked for %s: %s %s", f.what, r.Status, body)
		}
	}
	taken := l.leaseAs(grace, 0)
	for _, f := range l.fenced() {
		r, body := f.ask(harnessToken)
		switch {
		case looks[f.what] && r.StatusCode != 200:
			t.Errorf("under grace's Lease, ada asked for %s: %s %s", f.what, r.Status, body)
		case !looks[f.what] && (r.StatusCode != http.StatusConflict || !strings.Contains(body, "leased by grace@laptop (grace) until "+taken.Expires.Local().Format(time.TimeOnly))):
			t.Errorf("under grace's Lease, ada asked for %s: %s %s; want a refusal naming the holder and the end", f.what, r.Status, body)
		}
	}
	if r, body := l.get("DELETE", "/api/devices/"+l.name+"/lease?force=1&why="+url.QueryEscape("needed for the release")); r.StatusCode != 200 {
		t.Fatalf("ada ending grace's Lease: %s %s", r.Status, body)
	}
	l.wantLeaseEvents("taken", "ended by @ada: needed for the release")
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-after behavior=ok")})
	l.expect(l.jobAPI(id, 60*time.Second), api.OutcomePass)
}

// The M team reads every job, result and collected file on the fleet; the
// Audit log is for Maintainers and the Mac's Volunteer.
func TestTheMTeamReadsEveryJobAndTheAuditLogIsItsVolunteers(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	grace := l.member("grace")
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-adas behavior=ok")})
	done := l.jobAPI(id, 60*time.Second)
	l.expect(done, api.OutcomePass)
	if len(done.Result.Logs) == 0 {
		t.Fatalf("ada's job collected no file: %+v", done.Result)
	}
	for what, path := range map[string]string{
		"the job":          "/api/jobs/" + id,
		"the list of jobs": "/api/jobs?device=" + l.name,
		"a collected file": "/api/jobs/" + id + "/files/" + done.Result.Logs[0],
		"the Lease record": "/api/devices/" + l.name + "/leases",
	} {
		if r, body := l.as(grace.Token, "GET", path, nil); r.StatusCode != 200 || what == "the list of jobs" && !strings.Contains(body, id) {
			t.Errorf("grace reading %s: %s %s", what, r.Status, clip(body, 200))
		}
	}

	s := leaseLab(t)
	s.lrMac("m2-max", "j414c", lrVolunteers(api.AccessQualification, s.s.now().Add(time.Hour)))
	s.lrLend("m2-max", lrLinus)
	member, volunteer, maintainer, other := s.lrMember(lrGrace), s.signedIn(lrLinus), s.lrMaintainer(), s.signedIn(nobodyID)
	for who, c := range map[string]struct {
		b    *browser
		want int
	}{"an M team member": {member, 403}, "its Volunteer": {volunteer, 200}, "a Maintainer": {maintainer, 200}, "someone with no role": {other, 403}} {
		if w := c.b.do("GET", "/api/devices/m2-max/audit", ""); w.Code != c.want {
			t.Errorf("%s reading the Audit log: %d %s, want %d", who, w.Code, w.Body, c.want)
		}
	}
	if w := volunteer.do("GET", "/api/devices/m2-max/leases", ""); w.Code != 200 {
		t.Errorf("its Volunteer reading the Lease record: %d %s", w.Code, w.Body)
	}
}

func (l *signInLab) labLinesRecorded() []string {
	l.t.Helper()
	rows, err := l.s.store.db.Query(`SELECT text FROM lines WHERE channel = 'lab' ORDER BY id`)
	if err != nil {
		l.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		rows.Scan(&text)
		out = append(out, text)
	}
	return out
}

func (l *signInLab) leaseRecordOf(mac string) []string {
	l.t.Helper()
	events, err := l.s.store.leaseRecord(l.s.dev(mac).snapshot().ID, 100)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []string
	for i := len(events) - 1; i >= 0; i-- {
		out = append(out, events[i].Line())
	}
	return out
}

func wantLines(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	ok := len(got) == len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = strings.HasPrefix(got[i], want[i])
	}
	if !ok {
		t.Fatalf("%s:\n  %s\nwant:\n  %s", what, strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// People shows the Leases a person holds with End Lease, which asks twice,
// ends the Lease as a Maintainer does and is on the person's record.
func TestPeopleEndsALeaseAndKeepsItOnThePersonsRecord(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()
	grace.mustLease("m1-pro", "grace@laptop", 0)
	open := "/people?open=" + strconv.Itoa(lrGrace)
	end := "/people/" + strconv.Itoa(lrGrace) + "/leases/m1-pro/end"

	page := ravi.do("GET", open, "").Body.String()
	if !strings.Contains(page, end) || !strings.Contains(page, "End Lease") {
		t.Fatalf("grace's row offers no End Lease for m1-pro:\n%s", page)
	}
	if w := ravi.post(end, "why", "needed for the release"); w.Code != 200 || !strings.Contains(w.Body.String(), "End @grace&#39;s Lease on m1-pro") && !strings.Contains(w.Body.String(), "End @grace's Lease on m1-pro") {
		t.Fatalf("End Lease did not ask twice: %d %s", w.Code, w.Body)
	}
	if l.leaseOn("m1-pro") == nil {
		t.Fatal("the first click ended the Lease")
	}
	if w := grace.post(end, "sure", "yes"); w.Code == http.StatusSeeOther {
		t.Fatal("an M team member ended a Lease on People")
	}
	ravi.did(end, "why", "needed for the release", "sure", "yes")
	if q := l.leaseOn("m1-pro"); q != nil {
		t.Fatalf("the Lease outlived End Lease: %+v", q)
	}
	const ended = "@grace's Lease on m1-pro was ended by @ravi: needed for the release"
	wantLines(t, "the Lease record", l.leaseRecordOf("m1-pro"), "@grace took a Lease on m1-pro", ended)
	wantLines(t, "the Lab group's lines", l.labLinesRecorded(), "grace leased m1-pro until ", "ravi ended grace's Lease on m1-pro: needed for the release.")
	page = ravi.do("GET", open, "").Body.String()
	for _, want := range []string{"@grace took a Lease on m1-pro", "was ended by @ravi: needed for the release", "Holds no Lease."} {
		if !strings.Contains(html2text(page), want) {
			t.Errorf("grace's row on People lacks %q", want)
		}
	}
	ravi.refused("holds no Lease on m1-pro", end, "sure", "yes")
}

func html2text(page string) string {
	return strings.NewReplacer("&#39;", "'", "&#34;", `"`, "&amp;", "&").Replace(page)
}

// Leaving the M team and being Blocked end the person's Leases at once, and
// the Lab group is told which.
func TestLeavingTheMTeamOrBeingBlockedEndsTheLeasesAndSaysSo(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-max", "j414c", nil)
	grace, linus, ravi := l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()
	grace.mustLease("m1-pro", "grace@laptop", 0)
	linus.mustLease("m2-max", "linus@laptop", 0)

	ravi.did("/people/"+strconv.Itoa(lrGrace)+"/m-team/remove", "sure", "yes")
	ravi.did("/people/"+strconv.Itoa(lrLinus)+"/block", "reason", "spam", "sure", "yes")
	for _, mac := range []string{"m1-pro", "m2-max"} {
		if q := l.leaseOn(mac); q != nil {
			t.Fatalf("the Lease on %s outlived its holder's standing: %+v", mac, q)
		}
	}
	wantLines(t, "m1-pro's Lease record", l.leaseRecordOf("m1-pro"), "@grace took a Lease on m1-pro", "@grace's Lease on m1-pro was ended by @ravi: left the M team")
	wantLines(t, "m2-max's Lease record", l.leaseRecordOf("m2-max"), "@linus took a Lease on m2-max", "@linus's Lease on m2-max was ended by @ravi: Blocked")
	wantLines(t, "the Lab group's lines", l.labLinesRecorded(), "grace leased m1-pro until ", "linus leased m2-max until ",
		"grace's Lease on m1-pro ended: grace left the M team.", "linus's Lease on m2-max ended: linus was Blocked.")
}

// Jobs, Leases and the Audit log keep the handle at the time, with the
// account id on the server: a rename rewrites nothing, and the Lease and
// what runs under it stay the person's.
func TestARenameRewritesNothingAndTheLeaseStaysThePersons(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	grace := l.lrMember(lrGrace)
	grace.mustLease("m1-pro", "grace@laptop", 0)
	kernel, err := putArtifactForTest(l.s, "krel=7.2-renamed behavior=ok")
	if err != nil {
		t.Fatal(err)
	}
	submit := func(b *browser) api.Job {
		t.Helper()
		body, _ := json.Marshal(api.JobSpec{Device: "m1-pro", Kernel: kernel})
		w := b.write("POST", "/api/jobs", string(body), "Content-Type", "application/json")
		var j api.Job
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &j) != nil {
			t.Fatalf("a job under the Lease: %d %s", w.Code, w.Body)
		}
		return j
	}
	before := submit(grace)

	l.gh.AddUser(lrGrace, "gracehopper")
	renamed := l.signedIn(lrGrace)
	after := submit(renamed)
	renamed.mustLease("m1-pro", "grace@laptop", 0)

	if j, _ := l.s.store.job(before.ID); j.Maintainer != "grace" || j.AccountID != lrGrace {
		t.Errorf("the job from before the rename: handle %q, account %d", j.Maintainer, j.AccountID)
	}
	if after.Maintainer != "gracehopper" || after.AccountID != lrGrace {
		t.Errorf("the job from after the rename: handle %q, account %d", after.Maintainer, after.AccountID)
	}
	wantLines(t, "the Lease record", l.leaseRecordOf("m1-pro"), "@grace took a Lease on m1-pro", "@gracehopper renewed the Lease on m1-pro")
	events, _ := l.s.store.leaseRecordOf(lrGrace, 10)
	if len(events) != 2 || events[0].AccountID != lrGrace || events[1].AccountID != lrGrace {
		t.Errorf("the person's Lease record across a rename: %+v", events)
	}

	// labd's copy of the Audit log keeps the handle the Mac wrote and adds
	// the account it was then.
	v := l.s.dev("m1-pro")
	v.copyAudit([]api.AuditEntry{{Time: l.s.now(), Maintainer: "gracehopper", Action: "exec uname -r", End: 10}, {Time: l.s.now(), Maintainer: "grace", Job: before.ID, Action: "stage", End: 20}})
	got, err := l.s.readAuditCopy("m1-pro")
	if err != nil || len(got) != 2 || got[0].Maintainer != "gracehopper" || got[0].AccountID != lrGrace || got[1].Maintainer != "grace" || got[1].AccountID != lrGrace {
		t.Errorf("labd's copy of the Audit log: %+v %v", got, err)
	}
}
