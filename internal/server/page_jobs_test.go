package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// pjJob stores a job on mac as account asked for it; account 0 is the lab's
// own. change sets what the test is about.
func (l *signInLab) pjJob(id, mac string, account int64, handle string, at time.Time, change func(*api.Job)) *api.Job {
	l.t.Helper()
	j := &api.Job{ID: id, Spec: api.JobSpec{Device: mac, Holder: handle}, Maintainer: handle, AccountID: account,
		DeviceID: l.s.dev(mac).snapshot().ID, State: api.JobDone, Outcome: api.OutcomePass, Summary: "booted and healthy",
		Created: at, Updated: at.Add(3 * time.Minute), Events: []api.JobEvent{{Time: at, Msg: "queued"}}}
	if change != nil {
		change(j)
	}
	if err := l.s.store.saveJob(j); err != nil {
		l.t.Fatal(err)
	}
	return j
}

var rePageRow = regexp.MustCompile(`(?s)<tr id="(?:job|build)-([a-z0-9-]+)">(.*?)</tr>`)

// pageRows are a page's rows by job or build, and their order.
func pageRows(page string) (rows map[string]string, order []string) {
	rows = map[string]string{}
	for _, m := range rePageRow.FindAllStringSubmatch(page, -1) {
		rows[m[1]] = m[2]
		order = append(order, m[1])
	}
	return rows, order
}

func currentInLabNav(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`aria-current="page">([^<]*)<`).FindStringSubmatch(part(t, page, `<nav class="lab-nav"`, "</nav>"))
	if m == nil {
		return ""
	}
	return m[1]
}

// An M team member has Jobs; a Maintainer has Jobs and Builds. Nobody else
// has either.
func TestAnMTeamMemberHasJobsAndAMaintainerHasJobsAndBuilds(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("vol-air", "j313", lrVolunteers(api.AccessFull, l.s.now().Add(2*time.Hour)))
	l.lrLend("vol-air", lrLinus)
	grace, ravi, linus := l.lrMember(lrGrace), l.lrMaintainer(), l.signedIn(lrLinus)

	now := l.s.now()
	l.pjJob("j-ravis", "m1-pro", maintainerID, "ravi", now.Add(-3*time.Hour), func(j *api.Job) {
		j.Spec.Kernel = strings.Repeat("a", 64)
		j.Result.Booted, j.Result.BootKernel = true, "7.2.0-lab1"
	})
	l.pjJob("j-labs", "vol-air", 0, "", now.Add(-2*time.Hour), func(j *api.Job) { j.Spec.Baseline = true })
	l.pjJob("j-graces", "m1-pro", lrGrace, "grace", now.Add(-time.Hour), func(j *api.Job) {
		j.State, j.Outcome, j.Summary = api.JobTesting, "", ""
		j.Spec.Kernel = strings.Repeat("b", 64)
	})

	for who, b := range map[string]*browser{"an M team member": grace, "a Maintainer": ravi} {
		page := b.page("/jobs")
		if got := currentInLabNav(t, page); got != "Jobs" {
			t.Errorf("%s on Jobs: the lab's row marks %q as the page", who, got)
		}
		rows, order := pageRows(page)
		if got := strings.Join(order, " "); got != "j-graces j-labs j-ravis" {
			t.Fatalf("%s: every job on every Mac, newest first: %q", who, got)
		}
		for id, wants := range map[string][]string{
			"j-ravis":  {"m1-pro", "7.2.0-lab1", "@ravi", "Passed", whenDay(now.Add(-3 * time.Hour))},
			"j-labs":   {"vol-air", "Baseline", "the lab", "Passed"},
			"j-graces": {"m1-pro", "kernel bbbbbbbbbb", "@grace", "testing"},
		} {
			row := rowText(rows[id])
			for _, want := range wants {
				if !strings.Contains(row, want) {
					t.Errorf("%s: the row of %s lacks %q: %s", who, id, want, row)
				}
			}
			if !strings.Contains(rows[id], `href="/jobs/`+id+`"`) {
				t.Errorf("%s: the row of %s does not open the job", who, id)
			}
			if mac := wants[0]; !strings.Contains(rows[id], `href="/macs/`+mac+`"`) {
				t.Errorf("%s: the row of %s does not open its Mac's page", who, id)
			}
		}
	}
	if w := grace.do("GET", "/builds", ""); w.Code != http.StatusNotFound {
		t.Errorf("an M team member on Builds: %d", w.Code)
	}
	if page := ravi.page("/builds"); currentInLabNav(t, page) != "Builds" {
		t.Error("a Maintainer on Builds: it is not the current page of the lab's row")
	}
	for who, b := range map[string]*browser{"an External volunteer": linus, "someone with no role": l.signedIn(nobodyID)} {
		for _, target := range []string{"/jobs", "/builds", "/boot"} {
			if w := b.do("GET", target, ""); w.Code != http.StatusNotFound {
				t.Errorf("%s on %s: %d", who, target, w.Code)
			}
		}
		if w := b.form("/boot", "mac", "m1-pro", "kernel", "current"); w.Code != http.StatusNotFound {
			t.Errorf("%s posting the form of Boot a kernel: %d", who, w.Code)
		}
	}
	for _, target := range []string{"/jobs", "/jobs/j-ravis", "/builds", "/boot"} {
		if w := l.browser().do("GET", target, ""); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/sign-in?return=") {
			t.Errorf("signed out on %s: %d to %q", target, w.Code, w.Header().Get("Location"))
		}
	}
	// One Mac's jobs, as a Mac's page links them.
	if _, order := pageRows(grace.page("/jobs?mac=vol-air")); strings.Join(order, " ") != "j-labs" {
		t.Errorf("the jobs of one Mac: %q", order)
	}
}

// A job a person may not see is not found: an External volunteer sees the
// jobs of the Mac they lend, in its enrollment now, and no other.
func TestAJobAPersonMayNotSeeIsNotFound(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("vol-air", "j313", lrVolunteers(api.AccessFull, l.s.now().Add(2*time.Hour)))
	l.lrLend("vol-air", lrLinus)
	grace, ravi, linus, nobody := l.lrMember(lrGrace), l.lrMaintainer(), l.signedIn(lrLinus), l.signedIn(nobodyID)

	now := l.s.now()
	l.pjJob("j-on-lab", "m1-pro", maintainerID, "ravi", now.Add(-time.Hour), nil)
	l.pjJob("j-on-his", "vol-air", lrGrace, "grace", now.Add(-time.Hour), nil)
	l.pjJob("j-before", "vol-air", maintainerID, "ravi", now.Add(-90*24*time.Hour), func(j *api.Job) { j.DeviceID = "d-someone-elses-air" })

	for _, c := range []struct {
		who  string
		b    *browser
		want map[string]int
	}{
		{"a Maintainer", ravi, map[string]int{"j-on-lab": 200, "j-on-his": 200, "j-before": 200, "j-none": 404}},
		{"an M team member", grace, map[string]int{"j-on-lab": 200, "j-on-his": 200, "j-before": 200, "j-none": 404}},
		{"the Mac's Volunteer", linus, map[string]int{"j-on-lab": 404, "j-on-his": 200, "j-before": 404, "j-none": 404}},
		{"someone with no role", nobody, map[string]int{"j-on-lab": 404, "j-on-his": 404, "j-before": 404, "j-none": 404}},
	} {
		for id, want := range c.want {
			w := c.b.do("GET", "/jobs/"+id, "")
			if w.Code != want {
				t.Errorf("%s opening %s: %d, want %d", c.who, id, w.Code, want)
			}
			if want == 404 && (strings.Contains(w.Body.String(), "vol-air") || strings.Contains(w.Body.String(), "m1-pro")) {
				t.Errorf("%s, refused %s, is told its Mac", c.who, id)
			}
		}
	}
	// What a job did can be read by its Volunteer; its buttons are the
	// M team's and the Maintainers'.
	page := linus.page("/jobs/j-on-his")
	if !strings.Contains(rowText(page), "@grace") || buttons(part(t, page, `<main`, `</main>`)) != "" {
		t.Errorf("the Volunteer's view of a job on their Mac: buttons %q", buttons(part(t, page, `<main`, `</main>`)))
	}
	for _, action := range []string{"cancel", "retry"} {
		if w := linus.form("/jobs/j-on-his/" + action); w.Code != http.StatusNotFound {
			t.Errorf("the Volunteer's %s of a job on their Mac: %d", action, w.Code)
		}
	}
}

// A job's page says what was asked, by whom, and what happened; Cancel asks
// twice and is the person's own, or a Maintainer's, who is then named.
func TestAJobsPageShowsTheJobAndItsButtons(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	grace, linus, ravi := l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()
	now := l.s.now()

	l.pjJob("j-done", "m1-pro", lrGrace, "grace", now.Add(-time.Hour), func(j *api.Job) {
		j.Spec.Kernel, j.Spec.Cmdline, j.Spec.BootTimeoutSec = strings.Repeat("c", 64), "nvme_apple.flush_interval=0", 300
		j.Spec.Tests = []api.TestSpec{{Name: "gui-smoke", Builtin: "gui-smoke", GUI: true}}
		j.Outcome, j.Summary, j.UnderLease = api.OutcomeTestsFailed, "1 of 2 tests failed", true
		j.Events = append(j.Events, api.JobEvent{Time: now.Add(-59 * time.Minute), Msg: "staged 7.2.0-lab1 as the next boot"})
		j.Result = api.JobResult{Booted: true, BootKernel: "7.2.0-lab1", BootSeconds: 21, Logs: []string{"boot0/kernel.txt"},
			Tests:    []api.TestResult{{Name: "boot-health", Passed: true, Seconds: 2}, {Name: "gui-smoke", ExitCode: 1, Tail: "no monitor <is> up", Files: []string{"gui-smoke/screen.png"}}},
			Recovery: []string{"watchdog reset"}, NewErrorLines: []string{"apple-dcp: timeout"}}
	})
	page := grace.page("/jobs/j-done")
	offersToCopy(t, page, "the job id", "j-done")
	offersToCopy(t, page, "the kernel", strings.Repeat("c", 64))
	offersToCopy(t, page, "the arguments", "nvme_apple.flush_interval=0")
	text := rowText(page)
	for _, want := range []string{"j-done", "m1-pro", "@grace", "Tests failed", "1 of 2 tests failed", "7.2.0-lab1", "nvme_apple.flush_interval=0",
		"boot-health", "gui-smoke", "exit 1", "no monitor <is> up", "watchdog reset", "apple-dcp: timeout", "staged 7.2.0-lab1 as the next boot", "under a Lease", "300 s"} {
		if !strings.Contains(text, want) {
			t.Errorf("the job's page lacks %q", want)
		}
	}
	for _, file := range []string{"boot0/kernel.txt", "gui-smoke/screen.png"} {
		if !strings.Contains(page, `href="/api/jobs/j-done/files/`+file+`"`) {
			t.Errorf("the job's page does not link its file %s", file)
		}
	}
	if got := currentInLabNav(t, page); got != "Jobs" {
		t.Errorf("a job's page is under %q in the lab's row", got)
	}
	if got := buttons(part(t, page, `<main`, `</main>`)); got != "Retry" {
		t.Errorf("a finished job's buttons: %q", got)
	}
	if strings.Contains(page, `http-equiv="refresh"`) {
		t.Error("a finished job's page reloads itself")
	}

	grace.mustLease("m1-pro", "grace@laptop", 0)
	queued, code, err := l.s.submit(*l.personOf(lrGrace), api.JobSpec{Device: "m1-pro", Holder: "grace"}, "")
	if err != nil {
		t.Fatalf("grace's job: %d %v", code, err)
	}
	path := "/jobs/" + queued.ID
	page = grace.page(path)
	if got := buttons(part(t, page, `<main`, `</main>`)); got != "Cancel" || !strings.Contains(page, `http-equiv="refresh"`) {
		t.Errorf("her queued job: buttons %q, reloading itself %v", got, strings.Contains(page, `http-equiv="refresh"`))
	}
	if got := buttons(part(t, linus.page(path), `<main`, `</main>`)); got != "" {
		t.Errorf("another member's buttons on her queued job: %q", got)
	}
	if w := linus.form(path+"/cancel", "sure", "yes"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "own jobs") {
		t.Errorf("another member canceling her job: %d", w.Code)
	}
	w := grace.form(path + "/cancel")
	if j, _ := l.s.store.job(queued.ID); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Cancel: job "+queued.ID+" on m1-pro stops") || j.State != api.JobQueued {
		t.Fatalf("the first Cancel: %d, the job %s", w.Code, j.State)
	}
	if w := grace.form(path+"/cancel", "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != path {
		t.Fatalf("the second Cancel: %d to %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if j, _ := l.s.store.job(queued.ID); j.Outcome != api.OutcomeCanceled || strings.Contains(j.Summary, "@") {
		t.Fatalf("her canceled job: %s %q", j.Outcome, j.Summary)
	}

	// A Maintainer cancels any job, and the job says who did.
	hers, _, err := l.s.submit(*l.personOf(lrGrace), api.JobSpec{Device: "m1-pro", Holder: "grace"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := buttons(part(t, ravi.page("/jobs/"+hers.ID), `<main`, `</main>`)); got != "Cancel" {
		t.Errorf("a Maintainer's buttons on her queued job: %q", got)
	}
	if w := ravi.form("/jobs/"+hers.ID+"/cancel", "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("a Maintainer canceling her job: %d %s", w.Code, w.Body)
	}
	if j, _ := l.s.store.job(hers.ID); j.Outcome != api.OutcomeCanceled || !strings.Contains(j.Summary, "by @ravi") {
		t.Fatalf("her job, canceled by ravi: %s %q", j.Outcome, j.Summary)
	}
	if text := rowText(ravi.page("/jobs/" + hers.ID)); !strings.Contains(text, "Canceled") || !strings.Contains(text, "by @ravi") {
		t.Error("the canceled job's page does not say who canceled it")
	}

	// Retry queues the finished job again, as whoever asks.
	w = grace.form("/jobs/" + hers.ID + "/retry")
	again := strings.TrimPrefix(w.Header().Get("Location"), "/jobs/")
	j, err := l.s.store.job(again)
	if w.Code != http.StatusSeeOther || err != nil || j.RetryOf != hers.ID || j.AccountID != lrGrace || j.State != api.JobQueued {
		t.Fatalf("Retry: %d to %q: %+v %v", w.Code, w.Header().Get("Location"), j, err)
	}
	if page := grace.page("/jobs/" + again); !strings.Contains(page, `href="/jobs/`+hers.ID+`"`) {
		t.Error("a retry's page does not link the job it retries")
	}
	// Without her own Lease a member's retry is refused, on the job's page.
	if w := linus.form("/jobs/" + hers.ID + "/retry"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `role="alert"`) || !strings.Contains(rowText(w.Body.String()), "@linus needs their own Lease on m1-pro") {
		t.Errorf("a retry without a Lease: %d", w.Code)
	}
}

func (l *signInLab) personOf(id int64) *Person {
	l.t.Helper()
	p, err := l.s.person(id)
	if err != nil {
		l.t.Fatal(err)
	}
	return &p
}

var reOption = regexp.MustCompile(`<option value="([^"]*)"( selected)?>`)

// bootMacs are the Macs the form of "Boot a kernel" offers, and the one
// chosen.
func bootMacs(page string) (offered []string, chosen string) {
	for _, m := range reOption.FindAllStringSubmatch(page, -1) {
		offered = append(offered, m[1])
		if m[2] != "" {
			chosen = m[1]
		}
	}
	return offered, chosen
}

// "Boot a kernel" is a page of its own, reached from Jobs and from a Mac's
// page with that Mac chosen. It queues the job as the person, on a Mac
// they may use.
func TestBootAKernelIsReachedFromJobsAndFromAMacsPage(t *testing.T) {
	l := leaseLab(t)
	for _, name := range []string{"m1-pro", "m2-max", "m4-mini"} {
		l.lrMac(name, "", nil)
	}
	l.lrMac("no-baseline", "", func(d *api.Device) { d.KnownGood = "" })
	grace, linus, ravi := l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()

	for who, b := range map[string]*browser{"an M team member": grace, "a Maintainer": ravi} {
		if page := b.page("/jobs"); !regexp.MustCompile(`<a [^>]*href="/boot"[^>]*>Boot a kernel</a>`).MatchString(page) {
			t.Errorf("%s: Jobs does not lead to Boot a kernel", who)
		}
	}
	// A member boots only on a Mac they lease.
	page := grace.page("/boot")
	if offered, _ := bootMacs(page); len(offered) != 0 || !strings.Contains(page, "You hold no Lease") || !strings.Contains(page, `href="/macs"`) || buttons(part(t, page, `<main`, `</main>`)) != "" {
		t.Errorf("a member with no Lease is offered %q", offered)
	}
	if got := currentInLabNav(t, page); got != "Jobs" {
		t.Errorf("Boot a kernel is under %q in the lab's row", got)
	}
	grace.mustLease("m2-max", "grace@laptop", 0)
	linus.mustLease("m4-mini", "linus", 0)
	page = grace.page("/boot")
	if offered, chosen := bootMacs(page); strings.Join(offered, " ") != "m2-max" || chosen != "m2-max" {
		t.Errorf("a member holding m2-max is offered %q, %q chosen", offered, chosen)
	}
	if strings.Contains(page, `name="source"`) || strings.Contains(page, "Crash test") || !strings.Contains(page, "M team builds are not open yet") {
		t.Error("a member's form: a build from source and a Crash test are a Maintainer's")
	}
	// A Maintainer: any free Mac that passed its baseline, or one they hold.
	page = ravi.page("/boot?mac=m1-pro")
	if offered, chosen := bootMacs(page); strings.Join(offered, " ") != "m1-pro" || chosen != "m1-pro" {
		t.Errorf("a Maintainer is offered %q, %q chosen", offered, chosen)
	}
	if !strings.Contains(page, `name="source"`) || !strings.Contains(buttons(page), "Crash test") {
		t.Error("a Maintainer's form lacks the build from source or the Crash test")
	}
	offersToCopy(t, page, "the lab run command", "lab run m1-pro --kernel kernel-7.2.0-lab1.tar.zst --gui-test gui-smoke")
	offersToCopy(t, page, "lab mcp", "lab mcp")
	if !strings.Contains(rowText(page), "lab run m1-pro --kernel") || !strings.Contains(rowText(page), "lab mcp") {
		t.Error("the page lacks the artifacts-and-agents guide for the Mac chosen")
	}
	// From a Mac's page the Mac is chosen; one the person may not use is said.
	l.s.releaseLease(l.s.dev("m4-mini"), *l.personOf(lrLinus), "", false, "")
	if page := ravi.page("/macs/m4-mini"); !regexp.MustCompile(`<a [^>]*href="/boot\?mac=m4-mini"[^>]*>Boot a kernel</a>`).MatchString(page) {
		t.Error("m4-mini's page does not lead to Boot a kernel with it chosen")
	}
	if _, chosen := bootMacs(ravi.page("/boot?mac=m4-mini")); chosen != "m4-mini" {
		t.Errorf("from m4-mini's page, %q is chosen", chosen)
	}
	page = ravi.page("/boot?mac=m2-max")
	if _, chosen := bootMacs(page); chosen == "m2-max" || !strings.Contains(rowText(page), "m2-max is leased by grace@laptop (grace)") {
		t.Errorf("from the page of a Mac grace holds, a Maintainer has %q chosen", chosen)
	}

	sha, err := putArtifactForTest(l.s, "krel=7.2.0-lab1 behavior=ok")
	if err != nil {
		t.Fatal(err)
	}
	script, err := putArtifactForTest(l.s, "#!/bin/sh\ntrue\n")
	if err != nil {
		t.Fatal(err)
	}
	w := grace.form("/boot", "mac", "m2-max", "kernel", "artifact", "sha", " "+sha+" ", "cmdline", " dyndbg=+p ", "test", "gui-smoke", "timeout", "300",
		"scripts", "probe "+script+"\n", "scripts_gui", "on")
	id := strings.TrimPrefix(w.Header().Get("Location"), "/jobs/")
	j, err := l.s.store.job(id)
	if w.Code != http.StatusSeeOther || err != nil {
		t.Fatalf("Boot it: %d to %q %s", w.Code, w.Header().Get("Location"), rowText(w.Body.String()))
	}
	if j.AccountID != lrGrace || j.Maintainer != "grace" || j.Spec.Holder != "grace" || j.Spec.Device != "m2-max" || j.Spec.Kernel != sha || j.Spec.Cmdline != "dyndbg=+p" || j.Spec.BootTimeoutSec != 300 || j.State != api.JobQueued {
		t.Errorf("the job Boot it queued: %+v", j)
	}
	var tests []string
	for _, ts := range j.Spec.Tests {
		tests = append(tests, ts.Name+":"+ts.Builtin+ts.Script+map[bool]string{true: ":gui"}[ts.GUI])
	}
	if got := strings.Join(tests, " "); !strings.HasPrefix(got, "gui-smoke:gui-smoke:gui probe:"+script+":gui") {
		t.Errorf("the job's tests: %q", got)
	}
	if rows, _ := pageRows(grace.page("/jobs")); !strings.Contains(rowText(rows[id]), "queued") {
		t.Error("the job does not appear under Jobs")
	}

	// A refusal is said on the page, with what was typed, and queues nothing.
	queuedNow := func() int {
		jobs, err := l.s.store.jobs("", 100, true)
		if err != nil {
			t.Fatal(err)
		}
		return len(jobs)
	}
	before := queuedNow()
	for _, c := range []struct {
		what   string
		b      *browser
		fields []string
		status int
		saying string
	}{
		{"a member on a Mac they do not lease", grace, []string{"mac", "m1-pro", "kernel", "current"}, 403, "@grace needs their own Lease on m1-pro for jobs"},
		{"a member's build from source", grace, []string{"mac", "m2-max", "kernel", "source", "source", "https://github.com/labtest/linux"}, 403, "a Maintainer's until M team builds open"},
		{"a member's Crash test", grace, []string{"mac", "m2-max", "crash", "panic", "sure", "yes"}, 403, "a Crash test is a Maintainer's"},
		{"a Maintainer under her Lease", ravi, []string{"mac", "m2-max", "kernel", "current"}, 409, "m2-max is leased by grace@laptop (grace)"},
		{"no sha256", ravi, []string{"mac", "m1-pro", "kernel", "artifact", "sha", "7.2.0-lab1"}, 400, "sha256"},
		{"a source the lab does not take", ravi, []string{"mac", "m1-pro", "kernel", "source", "source", "https://gitlab.example/x/linux"}, 400, "github.com"},
		{"a script line that is no artifact", ravi, []string{"mac", "m1-pro", "kernel", "current", "scripts", "probe.sh"}, 400, "name and the sha256"},
		{"a timeout out of range", ravi, []string{"mac", "m1-pro", "kernel", "current", "timeout", "5"}, 400, "60 to 1800"},
		{"no Mac", ravi, []string{"kernel", "current"}, 400, "Choose a Mac"},
		{"a Mac that is not there", ravi, []string{"mac", "nope", "kernel", "current"}, 404, "nope"},
	} {
		w := c.b.form("/boot", append(c.fields, "cmdline", "keep=this")...)
		body := w.Body.String()
		if w.Code != c.status || !strings.Contains(body, `role="alert"`) || !strings.Contains(rowText(part(t, body, `role="alert"`, "</p>")+">"), c.saying) {
			t.Errorf("%s: %d, want %d saying %q: %s", c.what, w.Code, c.status, c.saying, clip(rowText(body), 400))
		}
		if !strings.Contains(body, `value="keep=this"`) {
			t.Errorf("%s: the form lost what was typed", c.what)
		}
	}
	if got := queuedNow(); got != before {
		t.Errorf("the refusals queued %d jobs", got-before)
	}

	// A Maintainer builds from source, and crash-tests asking twice.
	w = ravi.form("/boot", "mac", "m1-pro", "kernel", "source", "source", "https://github.com/labtest/linux/tree/fix")
	j, err = l.s.store.job(strings.TrimPrefix(w.Header().Get("Location"), "/jobs/"))
	if w.Code != http.StatusSeeOther || err != nil || j.Spec.Source != "https://github.com/labtest/linux/tree/fix" || j.Spec.Kernel != "" || j.AccountID != maintainerID {
		t.Fatalf("a Maintainer's build from source: %d %+v %v", w.Code, j, err)
	}
	w = ravi.form("/boot", "mac", "m4-mini", "crash", "panic")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Crash test: the Mac panics") || queuedNow() != before+1 {
		t.Fatalf("the first Crash test: %d", w.Code)
	}
	w = ravi.form("/boot", "mac", "m4-mini", "crash", "panic", "sure", "yes")
	j, err = l.s.store.job(strings.TrimPrefix(w.Header().Get("Location"), "/jobs/"))
	if w.Code != http.StatusSeeOther || err != nil || j.Spec.Crash != "panic" || j.Spec.Device != "m4-mini" || j.Spec.Kernel != "" || len(j.Spec.Tests) != 0 {
		t.Fatalf("the second Crash test: %d %+v %v", w.Code, j, err)
	}
}

// Builds shows who asked for each build.
func TestBuildsShowsWhoAsked(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()
	now := l.s.now()
	build := func(id string, at time.Time, change func(*api.Build)) {
		t.Helper()
		b := &api.Build{ID: id, Key: id, State: api.BuildDone, Device: "m1-pro", Release: "7.2.0-lab1", Created: at, Updated: at.Add(20 * time.Minute), Seconds: 1200,
			Source: api.Source{Input: "https://github.com/labtest/linux/tree/fix", Repo: "https://github.com/labtest/linux", Ref: "fix", SHA: strings.Repeat("d", 40)}}
		change(b)
		if err := l.s.store.saveBuild(b); err != nil {
			t.Fatal(err)
		}
	}
	build("b-ravis", now.Add(-4*time.Hour), func(b *api.Build) {
		b.AskedBy, b.AskedByID, b.Artifact, b.Size = "ravi", maintainerID, strings.Repeat("e", 64), 90<<20
	})
	build("b-labs", now.Add(-3*time.Hour), func(b *api.Build) { b.AskedBy = "labd" })
	build("b-old", now.Add(-2*time.Hour), func(b *api.Build) { b.State, b.Error = api.BuildFailed, "make: *** [vmlinux] Error 2" })
	build("b-graces", now.Add(-time.Hour), func(b *api.Build) {
		b.AskedBy, b.AskedByID, b.ByTeam, b.State, b.Stage, b.Progress = "grace", lrGrace, true, api.BuildRunning, "build", 0.4
		b.Kind, b.Recipe, b.RecipeDir, b.Device = api.BuildPackage, strings.Repeat("f", 64), "linux-aurora", ""
	})
	if err := os.MkdirAll(l.s.buildDir("b-old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.s.buildDir("b-old"), "build.log"), []byte("CC init/main.o\nmake: *** [vmlinux] <Error> 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, order := pageRows(ravi.page("/builds"))
	if got := strings.Join(order, " "); got != "b-graces b-old b-labs b-ravis" {
		t.Fatalf("the builds, newest first: %q", got)
	}
	for id, wants := range map[string][]string{
		"b-ravis":  {"@ravi", "7.2.0-lab1", "labtest/linux", "fix", "dddddddddddd", "m1-pro", "done", whenDay(now.Add(-4 * time.Hour))},
		"b-labs":   {"the lab", "done"},
		"b-old":    {"not recorded", "failed"},
		"b-graces": {"@grace", "M team", "packages", "linux-aurora", "running", "build", "40%"},
	} {
		row := rowText(rows[id])
		for _, want := range wants {
			if !strings.Contains(row, want) {
				t.Errorf("the row of %s lacks %q: %s", id, want, row)
			}
		}
		if !strings.Contains(rows[id], `href="/builds/`+id+`"`) {
			t.Errorf("the row of %s does not open the build", id)
		}
	}

	// A build's page: who asked, what it made, the end of its log.
	page := ravi.page("/builds/b-old")
	text := rowText(page)
	for _, want := range []string{"b-old", "not recorded", "failed", "make: *** [vmlinux] Error 2", "make: *** [vmlinux] <Error> 2", "labtest/linux"} {
		if !strings.Contains(text, want) {
			t.Errorf("the failed build's page lacks %q", want)
		}
	}
	if !strings.Contains(page, `href="/api/builds/b-old/log"`) || currentInLabNav(t, page) != "Builds" {
		t.Error("the build's page lacks its whole log, or is not under Builds")
	}
	offersToCopy(t, ravi.page("/builds/b-ravis"), "the build id", "b-ravis")
	offersToCopy(t, ravi.page("/builds/b-ravis"), "the artifact sha256", strings.Repeat("e", 64))
	if text := rowText(ravi.page("/builds/b-ravis")); !strings.Contains(text, "@ravi") || !strings.Contains(text, strings.Repeat("e", 64)) || !strings.Contains(text, "90 MB") {
		t.Error("a finished build's page lacks who asked or its artifact")
	}
	for _, target := range []string{"/builds/b-ravis", "/builds/b-none"} {
		if w := map[string]*browser{"/builds/b-ravis": grace, "/builds/b-none": ravi}[target].do("GET", target, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", target, w.Code)
		}
	}

	// Cancel asks twice.
	if got := buttons(part(t, ravi.page("/builds/b-graces"), `<main`, `</main>`)); got != "Cancel" {
		t.Errorf("a running build's buttons: %q", got)
	}
	if got := buttons(part(t, ravi.page("/builds/b-ravis"), `<main`, `</main>`)); got != "" {
		t.Errorf("a finished build's buttons: %q", got)
	}
	build("b-queued", now, func(b *api.Build) { b.State, b.AskedBy = api.BuildQueued, "ravi" })
	if w := ravi.form("/builds/b-queued/cancel"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Cancel: build b-queued stops") {
		t.Fatalf("the first Cancel: %d", w.Code)
	}
	if b, _ := l.s.getBuild("b-queued"); b.State != api.BuildQueued {
		t.Fatalf("one click canceled the build: %s", b.State)
	}
	if w := grace.form("/builds/b-queued/cancel", "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("an M team member canceling a build: %d", w.Code)
	}
	if w := ravi.form("/builds/b-queued/cancel", "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/builds/b-queued" {
		t.Fatalf("the second Cancel: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if b, _ := l.s.getBuild("b-queued"); b.State != api.BuildCanceled {
		t.Fatalf("the canceled build: %s", b.State)
	}

	// A job's page names its build, and links it for who may open Builds.
	l.pjJob("j-built", "m1-pro", maintainerID, "ravi", now, func(j *api.Job) {
		j.Spec.Source, j.Spec.Build, j.Spec.Kernel = "https://github.com/labtest/linux/tree/fix", "b-ravis", strings.Repeat("e", 64)
	})
	if page := ravi.page("/jobs/j-built"); !strings.Contains(page, `href="/builds/b-ravis"`) {
		t.Error("a Maintainer's view of a job does not link its build")
	}
	if page := grace.page("/jobs/j-built"); strings.Contains(page, `href="/builds/b-ravis"`) || !strings.Contains(rowText(page), "b-ravis") {
		t.Error("an M team member's view of a job links a page they may not open, or does not name the build")
	}
}

// A job's build is asked for by the job's person, by account and handle; a
// job the lab queued itself is the lab's.
func TestAJobsBuildRecordsWhoAskedForTheJob(t *testing.T) {
	l := newLab(t, true)
	l.baseline()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builds := 0
	go l.fakeBuilder(ctx, &builds)

	j := l.jobAPI(l.submitJob(api.JobSpec{Source: gitRepo(t)}), 60*time.Second)
	l.expect(j, api.OutcomePass)
	b, err := l.s.getBuild(j.Spec.Build)
	if err != nil || b.AskedBy != harnessMaintainer || b.AskedByID != testAccount(harnessMaintainer) || b.ByTeam {
		t.Fatalf("the build of ada's job: asked by %q, account %d, err %v", b.AskedBy, b.AskedByID, err)
	}
	j = l.run(api.JobSpec{Source: gitRepo(t)})
	l.expect(j, api.OutcomePass)
	if b, err := l.s.getBuild(j.Spec.Build); err != nil || b.AskedBy != "labd" || b.AskedByID != 0 {
		t.Fatalf("the build of the lab's own job: asked by %q, account %d, err %v", b.AskedBy, b.AskedByID, err)
	}
}
