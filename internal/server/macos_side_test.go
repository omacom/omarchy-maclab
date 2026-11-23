package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// macOSResultSet makes bench-1, with every macOS file in the artifact
// store, the current set a macOS result is taken of.
func macOSResultSet(t *testing.T, l *lab) *sets.Set {
	t.Helper()
	set, err := sets.Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	sha := l.artifact("bench-1's macOS files")
	files := map[string][]sets.File{}
	for sys, fs := range set.Files {
		for _, f := range fs {
			if sys == sets.MacOS {
				f.SHA256 = sha
			}
			files[sys] = append(files[sys], f)
		}
	}
	set.Files = files
	macOSSetMu.Lock()
	cur := macOSCurrentSet
	macOSCurrentSet = func() (*sets.Set, error) { return set, nil }
	macOSSetMu.Unlock()
	t.Cleanup(func() {
		macOSSetMu.Lock()
		macOSCurrentSet = cur
		macOSSetMu.Unlock()
	})
	return set
}

const (
	testMacOSVersion = "27.2.1"
	testMacOSBuild   = "25C56"
)

// reportBuild is the runner's poll at a person's own boot: the build, and a
// boot that began long ago.
func reportBuild(t *testing.T, l *lab, secret, version, build string) api.MacOSPoll {
	t.Helper()
	resp, body := l.as(secret, "GET", "/api/macos/runner/poll?version="+version+"&build="+build, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("poll: %d %s", resp.StatusCode, body)
	}
	return decodeInto[api.MacOSPoll](t, body)
}

// pollAs is the runner's poll at the boot session (hashed) that began at.
func pollAs(t *testing.T, l *lab, secret, boot string, at time.Time, build string) *api.MacOSRun {
	t.Helper()
	q := fmt.Sprintf("?boot=%s&booted=%d", boot, at.Unix())
	if build != "" {
		q += "&version=27.3&build=" + build
	}
	resp, body := l.as(secret, "GET", "/api/macos/runner/poll"+q, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("poll: %d %s", resp.StatusCode, body)
	}
	return decodeInto[api.MacOSPoll](t, body).Run
}

func bootHash(n int) string { return fmt.Sprintf("%032x", n) }

// macOSJobs are the Mac's macOS runs' jobs, oldest first.
func (l *lab) macOSJobs() []*api.Job {
	l.t.Helper()
	all, err := l.s.store.jobs(l.name, 100, false)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []*api.Job
	for _, j := range all {
		if j.Spec.MacOS != nil {
			out = append(out, j)
		}
	}
	slices.Reverse(out)
	return out
}

// nthMacOSJob waits for the job of the Mac's nth macOS run to end.
func (l *lab) nthMacOSJob(n int) *api.Job {
	l.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if js := l.macOSJobs(); len(js) >= n {
			return l.wait(js[n-1].ID, time.Until(deadline))
		}
		l.s.perfKick()
		time.Sleep(100 * time.Millisecond)
	}
	l.t.Fatalf("no macOS run job %d; the Mac has %d", n, len(l.macOSJobs()))
	return nil
}

// holdFromScheduler keeps the Performance scheduler off the Mac, as a
// Lease does, until the func it answers is called.
func (l *lab) holdFromScheduler() func() {
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "tester", Expires: time.Now().Add(time.Hour)}
	v.mu.Unlock()
	return func() {
		v.mu.Lock()
		v.d.Lease = nil
		v.mu.Unlock()
	}
}

// macOSUpload is what the runner sends for its performance run: bench-1
// results, and the state that ends the attempt.
func macOSUpload(t *testing.T, state string, values map[string]float64) map[string]any {
	t.Helper()
	var entries []map[string]any
	for metric, v := range values {
		entries = append(entries, perfEntry(t, metric, v, ""))
	}
	return map[string]any{"entries": entries, "state": state}
}

func (l *lab) perfRunOf(id int64) perf.Run {
	l.t.Helper()
	r, err := l.s.store.perfRun(id)
	if err != nil {
		l.t.Fatal(err)
	}
	return r
}

// The runner reports the macOS build at enrollment and at every boot, a
// person's own boot that and nothing else; with no macOS result of the
// current set at that build, labd queues a macOS run of the Full list with
// its attempt opened.
func TestTheRunnersBuildReportQueuesAMissingMacOSResult(t *testing.T) {
	l := newLab(t, false)
	set := macOSResultSet(t, l)
	_, body := l.as(harnessToken, "POST", "/api/macos/enroll-tokens", map[string]string{"mac": l.name})
	tok := decodeInto[api.MacOSEnrollToken](t, body)
	resp, body := l.as(tok.Token, "POST", "/api/macos/runner/enroll?version="+testMacOSVersion+"&build="+testMacOSBuild, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("enroll: %d %s", resp.StatusCode, body)
	}
	secret := decodeInto[api.MacOSEnrolled](t, body).Secret
	if side := l.s.macOSSide(l.name); side.Version != testMacOSVersion || side.Build != testMacOSBuild {
		t.Fatalf("the build told at enrollment: %+v", side)
	}
	run := l.s.macOS().run(l.name)
	if run == nil || run.State != api.MacOSQueued || run.Set != set.Name || run.Build != testMacOSBuild || run.Tier != api.TierExtended || run.PerfRun == 0 || len(run.Artifacts) == 0 {
		t.Fatalf("queued: %+v", run)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.Side != perf.SideMacOS || pr.MacOSBuild != testMacOSBuild || pr.List != sets.Full || pr.State != perf.Running || pr.Attempt != 1 {
		t.Errorf("its attempt: %+v", pr)
	}

	// A person's own boot: the build, kept, and nothing handed; the run
	// queued already is the one that waits.
	if p := reportBuild(t, l, secret, "27.3", "25D10"); p.Run != nil {
		t.Errorf("a person's own boot was handed %+v", p.Run)
	}
	if side := l.s.macOSSide(l.name); side.Build != "25D10" || side.Version != "27.3" {
		t.Errorf("the build of a person's boot: %+v", side)
	}
	if again := l.s.macOS().run(l.name); again.ID != run.ID {
		t.Errorf("a second run queued: %+v", again)
	}
	// What is not a version and a build is not kept.
	reportBuild(t, l, secret, "27.3;x", "my-mac")
	if side := l.s.macOSSide(l.name); side.Build != "25D10" {
		t.Errorf("junk kept as the build: %+v", side)
	}
}

// labd hands a started run only to a boot that began after it started it,
// within the runner's own window, and binds it to that boot: an earlier
// boot, a late one and any other boot get nothing. The boot's build is the
// one its attempt measures.
func TestAStartedRunIsHandedOnlyToTheLabsOwnBoot(t *testing.T) {
	l := newLab(t, false)
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	run := l.s.macOS().run(l.name)
	if run == nil || run.PerfRun == 0 {
		t.Fatalf("queued: %+v", run)
	}
	resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("started: %d %s", resp.StatusCode, body)
	}
	started := *decodeInto[api.MacOSRun](t, body).Started
	if got := pollAs(t, l, secret, bootHash(1), started.Add(-time.Hour), ""); got != nil {
		t.Errorf("a boot from before the start was handed the run: %+v", got)
	}
	if got := pollAs(t, l, secret, bootHash(1), started.Add(20*time.Minute), ""); got != nil {
		t.Errorf("a boot long after the start was handed the run: %+v", got)
	}
	got := pollAs(t, l, secret, bootHash(2), started.Add(time.Minute), "25D10")
	if got == nil || got.ID != run.ID || got.State != api.MacOSRunning || got.Boot != bootHash(2) || got.Build != "25D10" {
		t.Fatalf("the lab's own boot: %+v", got)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.MacOSBuild != "25D10" || pr.MacOSVersion != "27.3" || pr.Attempt != 1 {
		t.Errorf("the attempt measures the boot's build: %+v", pr)
	}
	if got := pollAs(t, l, secret, bootHash(3), started.Add(2*time.Minute), ""); got != nil {
		t.Errorf("another boot was handed a run one boot took: %+v", got)
	}
	if got := pollAs(t, l, secret, bootHash(2), started.Add(time.Minute), ""); got == nil || got.ID != run.ID {
		t.Errorf("the boot that took it asks again: %+v", got)
	}
	if resp, _ := l.as(secret, "GET", "/api/macos/runner/poll?boot=nothex&booted=1", nil); resp.StatusCode != 200 {
		t.Errorf("a poll with a junk boot: %d", resp.StatusCode)
	}
}

// A macOS run whose one-shot comes back to Omarchy with no macOS boot having
// taken it is cancelled, so the person's own macOS boot right after is
// handed nothing.
func TestAMacOSRunNoBootTookEndsWhenTheMacIsBackInOmarchy(t *testing.T) {
	l := newLab(t, false)
	macOSResultSet(t, l)
	l.holdFromScheduler()
	secret := enrollRunner(t, l)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	run := l.s.macOS().run(l.name)
	resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("started: %d %s", resp.StatusCode, body)
	}
	started := *decodeInto[api.MacOSRun](t, body).Started
	l.stopAgent()
	v := l.s.dev(l.name)
	d := v.snapshot()
	v.heartbeat(api.Checkin{BootID: d.BootID, Kernel: d.Kernel, Cmdline: d.Cmdline})
	if r := l.s.macOS().run(l.name); r.State != api.MacOSStarted {
		t.Fatalf("a check-in from the boot that started it: %+v", r)
	}
	v.heartbeat(api.Checkin{BootID: "the-next-boot", Kernel: d.Kernel, Cmdline: d.Cmdline})
	if r := l.s.macOS().run(l.name); r.ID != run.ID || r.State != api.MacOSCancelled || r.Error == "" {
		t.Fatalf("back in Omarchy: %+v", r)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.State != perf.Cancelled {
		t.Errorf("its attempt: %+v", pr)
	}
	if got := pollAs(t, l, secret, bootHash(3), started.Add(time.Minute), ""); got != nil {
		t.Errorf("the person's own boot was handed %+v", got)
	}
}

// A macOS attempt that failed, as of a busy Mac, is made once more, of the
// benchmarks that failed or were skipped in passing alone.
func TestAFailedMacOSResultIsMeasuredOnceMore(t *testing.T) {
	l := newLab(t, false)
	macOSResultSet(t, l)
	l.holdFromScheduler()
	secret := enrollRunner(t, l)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	enrollment := l.s.dev(l.name).snapshot().ID
	ended := func(run *api.MacOSRun, c perf.Checked) {
		t.Helper()
		take(t, l.s.store, run.PerfRun, c)
		if _, err := l.s.macOS().change(l.name, func(m *api.MacOSRun) error { m.State = api.MacOSDone; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	first := l.s.macOS().run(l.name)
	if len(first.Metrics) != 0 {
		t.Fatalf("the first run measures %v, want its whole list", first.Metrics)
	}
	ended(first, perfChecked(t, perf.SideMacOS, perf.Failed, perfEntry(t, "coremark-single", 30000, ""),
		perfEntry(t, "stream-triad", 0, perf.NotIdle), perfEntry(t, "speedometer", 0, perf.FellBack)))
	second, err := l.s.queueMacOSSide(l.name)
	if err != nil || second == nil || second.ID == first.ID || !slices.Equal(second.Metrics, []string{"stream-triad"}) {
		t.Fatalf("the second run: %+v (%v)", second, err)
	}
	if pr := l.perfRunOf(second.PerfRun); !slices.Equal(pr.Metrics, second.Metrics) {
		t.Errorf("its attempt: %+v", pr)
	}
	ended(second, perfChecked(t, perf.SideMacOS, perf.Failed, perfEntry(t, "stream-triad", 0, perf.Error)))
	if third, err := l.s.queueMacOSSide(l.name); err != nil || third != nil {
		t.Fatalf("a third run: %+v (%v)", third, err)
	}
	got, _ := l.s.store.perfCurrentMacOS(l.name, enrollment, "bench-1", testMacOSBuild)
	if c := current(t, got, "coremark-single"); c != "30000@1" {
		t.Errorf("the first attempt's value: %s", c)
	}
}

// A started run must report within 60 minutes, else it is needs_hands and
// the Mac Needs hands; results that come late are still taken, and the run
// is done.
func TestAMacOSRunPastItsHourNeedsHandsAndItsLateResultsAreTaken(t *testing.T) {
	if d := macosDeadline(api.TierDefault); d != time.Hour {
		t.Fatalf("the deadline is %s, want 60 minutes", d)
	}
	was := macosDeadline
	macosDeadline = func(string) time.Duration { return 300 * time.Millisecond }
	t.Cleanup(func() { macosDeadline = was })
	l := newLab(t, false)
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	run := l.s.macOS().run(l.name)
	resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("started: %d %s", resp.StatusCode, body)
	}
	started := *decodeInto[api.MacOSRun](t, body).Started
	l.stopAgent()
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands })
	if !strings.Contains(d.StateReason, "macOS") {
		t.Errorf("reason: %q", d.StateReason)
	}
	if r := l.s.macOS().run(l.name); r.State != api.MacOSNeedsHands {
		t.Fatalf("run: %+v", r)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.State != perf.NeedsHands {
		t.Errorf("its attempt: %+v", pr)
	}

	// The runner comes late: its boot is still handed the run, and the
	// results are taken.
	if got := pollAs(t, l, secret, bootHash(7), started.Add(time.Minute), ""); got != nil {
		t.Errorf("a needs_hands run handed again: %+v", got)
	}
	path := fmt.Sprintf("/api/macos/runner/performance/%d/results", run.PerfRun)
	if resp, body := l.as(secret, "POST", path, macOSUpload(t, perf.Succeeded, map[string]float64{"coremark-single": 31000})); resp.StatusCode != 200 {
		t.Fatalf("late results: %d %s", resp.StatusCode, body)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.State != perf.Succeeded {
		t.Errorf("its attempt after the late results: %+v", pr)
	}
	if r := l.s.macOS().run(l.name); r.State != api.MacOSDone {
		t.Errorf("the run after its late results: %+v", r)
	}
	if kept, _ := l.s.store.perfCurrentMacOS(l.name, l.s.dev(l.name).snapshot().ID, "bench-1", testMacOSBuild); len(kept) != 1 || *kept[0].Value != 31000 {
		t.Errorf("the macOS result: %+v", kept)
	}
}

// An Attended Mac's run waits in Omarchy: the Lab group is told it is due
// (a Lab-owned Mac), once, and labd boots macOS only once a person says
// they are at the Mac, on its page. The run then measures, and the job ends
// once the Mac is back on its Known-good kernel.
func TestAnAttendedMacsRunBootsMacOSOnlyOnceAPersonIsAtTheMac(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	v := l.s.dev(l.name)
	if spec := l.s.macOSWanted(v); spec != nil {
		t.Fatalf("an Attended Mac's run wanted with nobody at it: %+v", spec)
	}
	run := l.s.macOS().run(l.name)
	if !run.Attended || run.Told == nil || run.Expires == nil || run.Expires.Sub(*run.Told) != 24*time.Hour {
		t.Fatalf("told: %+v", run)
	}
	l.s.macOSWanted(v)
	// The scheduler runs here too, and may be the one telling: it marks the
	// run told, then writes the line.
	for deadline := time.Now().Add(10 * time.Second); len(macOSDueLines(t, l)) == 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if lines := macOSDueLines(t, l); len(lines) != 1 || !strings.Contains(lines[0], "sim-mac") || !strings.Contains(lines[0], "person at the Mac") {
		t.Fatalf("the Lab group was told: %q", lines)
	}

	l.sim.MacOSFor = 2 * time.Second
	booted := 0
	l.sim.InMacOS = func() {
		booted++
		got := pollAs(t, l, secret, bootHash(9), l.s.now(), "")
		if got == nil {
			t.Error("the lab's boot was handed no run")
			return
		}
		path := fmt.Sprintf("/api/macos/runner/performance/%d/results", got.PerfRun)
		if r, body := l.as(secret, "POST", path, macOSUpload(t, perf.Succeeded, map[string]float64{"coremark-single": 30000, "stream-triad": 100})); r.StatusCode != 200 {
			t.Errorf("results: %d %s", r.StatusCode, body)
		}
	}

	ada, err := l.s.person(testAccount(harnessMaintainer))
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]string{"name": l.name}
	page := l.pageAs(ada, (*Server).pMac, "/macs/"+l.name, at)
	if !strings.Contains(page.Body.String(), `id="macos-at-mac"`) || !strings.Contains(page.Body.String(), "I am at the Mac") {
		t.Fatalf("the Mac's page has no button for a person at the Mac: %s", page.Body)
	}
	if booted != 0 {
		t.Fatal("macOS booted before anyone was at the Mac")
	}
	if w := l.pageAs(ada, (*Server).pMacOSAtMac, "/macs/"+l.name+"/macos/at-mac", at); w.Code != http.StatusSeeOther {
		t.Fatalf("at the Mac: %d %s", w.Code, w.Body)
	}
	run = l.s.macOS().run(l.name)
	if run.AtMac == nil || run.AtMacBy != harnessMaintainer || run.Job == "" {
		t.Fatalf("after a person said so: %+v", run)
	}
	j := l.wait(run.Job, 60*time.Second)
	l.expect(j, api.OutcomePass)
	if booted != 1 {
		t.Errorf("macOS booted %d times", booted)
	}
	if r := l.s.macOS().run(l.name); r.State != api.MacOSDone {
		t.Errorf("run: %+v", r)
	}
	if kept, _ := l.s.store.perfCurrentMacOS(l.name, l.s.dev(l.name).snapshot().ID, "bench-1", testMacOSBuild); len(kept) != 2 {
		t.Errorf("the macOS result: %+v", kept)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Kernel == agent.SimKnownGood })
	// It has its macOS result: nothing more is wanted of it.
	if spec := l.s.macOSWanted(v); spec != nil || l.s.macOS().run(l.name).ID != run.ID {
		t.Errorf("wanted again after its macOS result: %+v", spec)
	}
	if resp, _ := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos/at-mac", nil); resp.StatusCode != 409 {
		t.Errorf("at the Mac with no run waiting: %d", resp.StatusCode)
	}
}

func macOSDueLines(t *testing.T, l *lab) []string {
	t.Helper()
	rows, err := l.s.store.db.Query(`SELECT text FROM lines WHERE channel = 'lab' AND text LIKE 'The macOS run of %' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// With nobody at an Attended Mac within a day, the macOS side is dropped
// and its attempt cancelled; labd does not queue it again until the Mac's
// next Omarchy side has ended.
func TestAnAttendedMacsRunIsDroppedWithNobodyInADay(t *testing.T) {
	clock := agent.NewFakeClock(time.Now())
	l := newLabWith(t, labConfig{clock: clock})
	l.passedBaseline()
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	v := l.s.dev(l.name)
	l.s.macOSWanted(v)
	run := l.s.macOS().run(l.name)
	if run.Told == nil {
		t.Fatalf("not told: %+v", run)
	}
	clock.Advance(23 * time.Hour)
	time.Sleep(100 * time.Millisecond)
	if r := l.s.macOS().run(l.name); r.State != api.MacOSQueued {
		t.Fatalf("dropped before a day: %+v", r)
	}
	clock.Advance(time.Hour + time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for l.s.macOS().run(l.name).State != api.MacOSDropped {
		if time.Now().After(deadline) {
			t.Fatalf("not dropped after a day: %+v", l.s.macOS().run(l.name))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.State != perf.Cancelled {
		t.Errorf("its attempt: %+v", pr)
	}
	if resp, _ := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos/at-mac", nil); resp.StatusCode != 409 {
		t.Errorf("at the Mac after the drop: %d", resp.StatusCode)
	}
	if spec := l.s.macOSWanted(v); spec != nil || l.s.macOS().run(l.name).ID != run.ID {
		t.Fatalf("queued again before an Omarchy side: %+v", l.s.macOS().run(l.name))
	}

	// The Mac's next Omarchy side ends: the macOS side is due again.
	om, err := l.s.store.openPerfRun(perf.Run{Mac: l.name, Enrollment: v.snapshot().ID, Set: "bench-1", Side: perf.SideOmarchy, Release: "2026.11", List: sets.Quick}, l.s.now())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := l.s.store.endPerfRun(om.ID, perf.Failed, l.s.now()); err != nil {
		t.Fatal(err)
	}
	l.s.macOSWanted(v)
	again := l.s.macOS().run(l.name)
	if again.ID == run.ID || again.State != api.MacOSQueued || again.Told == nil {
		t.Errorf("after the Omarchy side: %+v", again)
	}
	if pr := l.perfRunOf(again.PerfRun); pr.Attempt != 2 {
		t.Errorf("the new attempt: %+v", pr)
	}
}

// A Volunteer's Attended Mac: its Volunteer gets the Notice, in its fixed
// wording, and says they are at the Mac on My Macs.
func TestAVolunteersAttendedMacTellsItsVolunteer(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	n.passedBaseline()
	macOSResultSet(t, n.lab)
	secret := enrollRunner(t, n.lab)
	var said []Notification
	n.s.mu.Lock()
	n.s.notifyHooks = append(n.s.notifyHooks, func(x Notification) { said = append(said, x) })
	n.s.mu.Unlock()
	reportBuild(t, n.lab, secret, testMacOSVersion, testMacOSBuild)
	n.s.macOSWanted(n.s.dev(n.name))
	n.s.mu.Lock()
	got := append([]Notification(nil), said...)
	n.s.mu.Unlock()
	found := false
	for _, x := range got {
		if x.Volunteer && x.Body == n.name+": "+macOSNotice {
			found = true
		}
	}
	if !found {
		t.Fatalf("the Volunteer was not told: %+v", got)
	}
	if lines := macOSDueLines(t, n.lab); len(lines) != 0 {
		t.Errorf("the Lab group was told of a Volunteer's Mac: %q", lines)
	}
	if w := n.s.macOSWaitWords(n.s.dev(n.name), Person{}); w != "" {
		t.Errorf("someone not the Volunteer is offered the button: %q", w)
	}
}

// boot-macos-once failing to set macOS as the next boot cancels the run and
// its attempt; the Mac stays in Omarchy and is ready again.
func TestAFailedMacOSStartIsCancelled(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	if resp, body := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 200 {
		t.Fatalf("attendance: %d %s", resp.StatusCode, body)
	}
	l.sim.BlessFails = errors.New("asahi-bless --next --set-boot 1: NVRAM write failed")
	l.sim.InMacOS = func() { t.Error("booted macOS") }
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	v := l.s.dev(l.name)
	run := l.s.macOS().run(l.name)
	j := l.nthMacOSJob(1)
	l.expect(j, api.OutcomeInfra)
	if !strings.Contains(j.Summary, "NVRAM write failed") {
		t.Errorf("summary: %s", j.Summary)
	}
	if r := l.s.macOS().run(l.name); r.State != api.MacOSCancelled || r.Started != nil {
		t.Errorf("run: %+v", r)
	}
	if pr := l.perfRunOf(run.PerfRun); pr.State != perf.Cancelled {
		t.Errorf("its attempt: %+v", pr)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	if spec := l.s.macOSWanted(v); spec != nil {
		t.Errorf("a cancelled start queued again before an Omarchy side: %+v", spec)
	}
}

// A Compare with --macos uses the Mac's kept macOS result of the job's set
// at the Mac's current build in place of booting macOS.
func TestCompareUsesAKeptMacOSResult(t *testing.T) {
	l, _ := compareLab(t, nil)
	macOSResultSet(t, l)
	free := l.holdFromScheduler() // the result is kept before the scheduler would take the run
	secret := enrollRunner(t, l)
	if resp, body := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 200 {
		t.Fatalf("attendance: %d %s", resp.StatusCode, body)
	}
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	run := l.s.macOS().run(l.name)
	if run == nil || run.PerfRun == 0 {
		t.Fatalf("queued: %+v", run)
	}
	c := perfChecked(t, perf.SideMacOS, perf.Succeeded, perfEntry(t, "coremark-single", 30000, ""), perfEntry(t, "stream-triad", 0, perf.NoBuild))
	if _, err := l.s.store.takePerf(run.PerfRun, c, l.s.now()); err != nil {
		t.Fatal(err)
	}
	now := l.s.now()
	l.s.macOS().change(l.name, func(m *api.MacOSRun) error {
		m.State, m.Done = api.MacOSDone, &now
		return nil
	})
	l.sim.InMacOS = func() { t.Error("booted macOS for a kept result") }
	free()

	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3", MacOS: true})
	l.expect(j, api.OutcomePass)
	if j.Spec.Compare.Bench != "bench-1" {
		t.Errorf("the job's set: %q", j.Spec.Compare.Bench)
	}
	m := j.Result.Compare.Side(api.SideMacOS)
	if m == nil || m.Boot != macOSKept || len(m.Bench) != 2 {
		t.Fatalf("macOS: %+v", m)
	}
	byName := map[string]api.BenchResult{}
	for _, b := range m.Bench {
		byName[b.Name] = b
	}
	if b := byName["coremark-single"]; b.Value == nil || *b.Value != 30000 || !b.HigherIsBetter {
		t.Errorf("coremark-single: %+v", b)
	}
	if b := byName["stream-triad"]; b.Value != nil || b.Note != perf.NoBuild {
		t.Errorf("stream-triad: %+v", b)
	}
	evs, _ := json.Marshal(j.Events)
	if !strings.Contains(string(evs), "macOS is not booted") {
		t.Errorf("events: %s", evs)
	}
}

// A Compare with --macos and no kept macOS result measures one: it opens
// an attempt at the Mac's macOS result of the job's set, even where the
// scheduler queues none (two attempts failed), boots macOS once, and
// compares its values by metric id. The result is kept for the next Compare.
// With no build reported, the Compare's macOS side says why it ran none.
func TestACompareWithNoKeptMacOSResultMeasuresOneAndKeepsIt(t *testing.T) {
	l, _ := compareLab(t, nil)
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	if resp, body := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 200 {
		t.Fatalf("attendance: %d %s", resp.StatusCode, body)
	}
	none := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3", MacOS: true})
	if m := none.Result.Compare.Side(api.SideMacOS); m == nil || !strings.Contains(m.Error, "has not reported its macOS build") {
		t.Fatalf("a Compare with no build reported: %+v", m)
	}
	enrollment := l.s.dev(l.name).snapshot().ID
	for i := 0; i < macOSTriesMax; i++ {
		pr, err := l.s.store.openPerfRun(perf.Run{Mac: l.name, Enrollment: enrollment, Set: "bench-1", Side: perf.SideMacOS,
			MacOSVersion: testMacOSVersion, MacOSBuild: testMacOSBuild, List: sets.Full}, l.s.now())
		if err != nil {
			t.Fatal(err)
		}
		if err := l.s.store.endPerfRun(pr.ID, perf.Failed, l.s.now()); err != nil {
			t.Fatal(err)
		}
	}
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	if r := l.s.macOS().run(l.name); r != nil {
		t.Fatalf("a run queued after two failed attempts: %+v", r)
	}
	var booted atomic.Int32
	l.sim.MacOSFor = 2 * time.Second
	l.sim.InMacOS = func() {
		booted.Add(1)
		got := pollAs(t, l, secret, bootHash(11), l.s.now(), "")
		if got == nil || got.PerfRun == 0 || got.Build != testMacOSBuild {
			t.Errorf("the lab's boot was handed %+v", got)
			return
		}
		path := fmt.Sprintf("/api/macos/runner/performance/%d/results", got.PerfRun)
		if r, body := l.as(secret, "POST", path, macOSUpload(t, perf.Succeeded, map[string]float64{"coremark-single": 102, "speedometer": 30})); r.StatusCode != 200 {
			t.Errorf("results: %d %s", r.StatusCode, body)
		}
	}

	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3", MacOS: true})
	l.expect(j, api.OutcomePass)
	res := j.Result.Compare
	if m := res.Side(api.SideMacOS); m == nil || m.Boot != api.MacOSDone || len(m.Bench) != 2 || booted.Load() != 1 {
		t.Fatalf("macOS: %+v, %d boots", m, booted.Load())
	}
	if c := rowOf(t, res, "coremark-single"); !near(c.MacOS, 102) || !near(c.MacOSDelta, 2) || c.MacOSVerdict != api.VerdictNoChange {
		t.Errorf("CoreMark in macOS: %+v", c)
	}
	if sp := rowOf(t, res, "speedometer"); !near(sp.MacOS, 30) || sp.MacOSVerdict != api.VerdictBetter {
		t.Errorf("Speedometer in macOS: %+v", sp)
	}
	run := l.s.macOS().run(l.name)
	if pr := l.perfRunOf(run.PerfRun); pr.Side != perf.SideMacOS || pr.Set != "bench-1" || pr.MacOSBuild != testMacOSBuild || pr.State != perf.Succeeded || pr.Attempt != macOSTriesMax+1 {
		t.Errorf("its attempt: %+v", pr)
	}
	if kept, _ := l.s.store.perfCurrentMacOS(l.name, l.s.dev(l.name).snapshot().ID, "bench-1", testMacOSBuild); len(kept) != 2 {
		t.Errorf("the macOS result: %+v", kept)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.ActiveJob == "" })
	if spec := l.s.macOSWanted(l.s.dev(l.name)); spec != nil {
		t.Errorf("the scheduler wants another macOS run: %+v", spec)
	}

	again := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3", MacOS: true})
	l.expect(again, api.OutcomePass)
	if m := again.Result.Compare.Side(api.SideMacOS); m == nil || m.Boot != macOSKept || booted.Load() != 1 {
		t.Errorf("the next Compare's macOS: %+v, %d boots", m, booted.Load())
	}
	if c := rowOf(t, again.Result.Compare, "coremark-single"); !near(c.MacOS, 102) {
		t.Errorf("CoreMark in macOS, kept: %+v", c)
	}
}
