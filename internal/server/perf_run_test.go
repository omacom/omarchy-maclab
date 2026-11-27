package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/installed"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// perfAnswers is what the simulated Mac's bench builtin answers for each
// metric it is given, by attempt: a value, or a reason code.
type perfAnswers struct {
	mu     sync.Mutex
	answer func(metric string, attempt int) (float64, string)
	sim    *agent.Sim
}

func (a *perfAnswers) set(f func(metric string, attempt int) (float64, string)) {
	a.mu.Lock()
	a.answer = f
	a.mu.Unlock()
}

// runs are the metrics of each run the Mac's bench builtin ran, in order.
func (a *perfAnswers) runs() [][]string { return a.sim.BenchRuns() }

// perfLab is a baselined Mac with bench-1 as the Benchmark set Performance
// runs measure with. Its bench builtin writes a result for each metric labd
// gives it, and lab-agent uploads them to the run labd names and ends the
// attempt: failed when a result is in error.
func perfLab(t *testing.T, c labConfig, setup func(l *lab)) (*lab, *perfAnswers) {
	t.Helper()
	compareKnobs.Do(func() { compareConfirmGrace, compareCheckEvery = 2*time.Second, 200*time.Millisecond })
	old := perfCurrentSet
	// The set's own files are not mirrored here: its Omarchy files are
	// placeholders, which the bench builtin answers for itself.
	bench, err := sets.Load(testSet)
	if err != nil {
		t.Fatal(err)
	}
	unmirrored := *bench
	unmirrored.Files = map[string][]sets.File{}
	for sys, fs := range bench.Files {
		for _, f := range fs {
			f.SHA256 = sets.Placeholder
			unmirrored.Files[sys] = append(unmirrored.Files[sys], f)
		}
	}
	perfCurrentSet = func() (*sets.Set, error) { return &unmirrored, nil }
	t.Cleanup(func() { perfCurrentSet = old })
	l := newLabWith(t, c)
	a := &perfAnswers{answer: func(string, int) (float64, string) { return 100, "" }, sim: l.sim}
	l.sim.SetBenchAnswer(func(run int64, metric string) (float64, string) {
		att, err := l.s.store.perfRun(run)
		if err != nil {
			t.Errorf("the bench test ran outside a Performance run: %v", err)
			return 0, perf.Error
		}
		a.mu.Lock()
		answer := a.answer
		a.mu.Unlock()
		return answer(metric, att.Attempt)
	})
	if setup != nil {
		setup(l)
	}
	l.baseline()
	return l, a
}

// perfRelease freezes Release name for the Macs given: one package, the
// kernel the simulated Mac boots, at candidateKernel, and any more
// packages given as NAME=VERSION.
func (l *lab) perfRelease(name string, at time.Time, macs []string, more ...string) api.Release {
	l.t.Helper()
	m := installed.Manifest{Schema: installed.ManifestSchema, Release: name, Macs: map[string]installed.MacExpect{}}
	m.Kernel.Release = candidateKernel
	for _, p := range append([]string{"linux-aurora=" + candidateKernel}, more...) {
		pkg, ver, _ := strings.Cut(p, "=")
		file := pkg + "-" + ver + "-aarch64.pkg.tar.zst"
		m.Artifacts = append(m.Artifacts, installed.Artifact{Name: pkg, File: file, SHA256: l.artifact("Release " + name + ": " + file), Version: ver})
	}
	for _, mac := range macs {
		m.Macs[mac] = installed.MacExpect{Board: "apple,sim"}
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	dir := l.s.frozenDir(name)
	os.MkdirAll(filepath.Join(dir, "expected"), 0o755)
	os.WriteFile(filepath.Join(dir, "expected", "manifest.json"), mb, 0o444)
	os.WriteFile(filepath.Join(dir, "PINS.expected"), []byte(pinsFor(dir, "expected/manifest.json")), 0o444)
	if err := l.s.listRelease(&m, installed.SHA256(mb), Person{}, at); err != nil {
		l.t.Fatal(err)
	}
	rels, _ := l.s.store.releases(name)
	return rels[0]
}

// perfJobs are the Mac's Performance runs, oldest first.
func (l *lab) perfJobs() []*api.Job {
	l.t.Helper()
	all, err := l.s.store.jobs(l.name, 100, false)
	if err != nil {
		l.t.Fatal(err)
	}
	var out []*api.Job
	for _, j := range all {
		if j.Spec.Performance != nil {
			out = append(out, j)
		}
	}
	slices.Reverse(out)
	return out
}

// nthPerfJob waits for the Mac's nth Performance run to end.
func (l *lab) nthPerfJob(n int) *api.Job {
	l.t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if js := l.perfJobs(); len(js) >= n {
			return l.wait(js[n-1].ID, time.Until(deadline))
		}
		l.s.perfKick()
		time.Sleep(100 * time.Millisecond)
	}
	l.t.Fatalf("no Performance run %d; the Mac has %d", n, len(l.perfJobs()))
	return nil
}

// noMorePerf checks that the lab queues no other Performance run.
func (l *lab) noMorePerf(have int) {
	l.t.Helper()
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.ActiveJob == "" })
	for i := 0; i < 5; i++ {
		l.s.reconcilePerf()
		time.Sleep(100 * time.Millisecond)
	}
	if js := l.perfJobs(); len(js) != have {
		l.t.Fatalf("%d Performance runs, want %d: the last measures %v", len(js), have, js[len(js)-1].Spec.Performance.Metrics)
	}
}

func bench1(t *testing.T, list string) []string {
	t.Helper()
	set, err := sets.Load(testSet)
	if err != nil {
		t.Fatal(err)
	}
	return omarchyMetrics(set.List(list))
}

func (l *lab) qualify(release string) {
	l.t.Helper()
	if err := l.s.store.addVerdict(release, api.ReleaseVerdict{Time: l.s.now(), State: api.ReleaseQualified, Note: "qualified"}); err != nil {
		l.t.Fatal(err)
	}
	l.s.perfKick()
}

// A frozen Release is measured on a Lab-owned Mac by itself: its frozen
// manifest becomes the Package set release-<version>, booted as a Candidate
// boot; the run measures on the Release's kernel, records what it booted by
// package name, and leaves the Mac on its own system. Nobody holds a Lease,
// and none is taken while the run claims the Mac.
func TestAFrozenReleaseIsMeasuredAsACandidateBoot(t *testing.T) {
	l, a := perfLab(t, labConfig{}, nil)
	hold := l.sim.HoldTests()
	rel := l.perfRelease("7.3", l.s.now(), []string{l.name}, "omarchy-ane=0.5.6-1")
	l.waitDevice(func(d api.Device) bool { return d.ActiveJob != "" })
	if r, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/lease", api.LeaseRequest{Holder: "tester"}); r.StatusCode != 409 || !strings.Contains(body, "Performance run") {
		t.Errorf("a Lease while a Performance run claims the Mac: %s %s", r.Status, body)
	}
	hold()
	j := l.nthPerfJob(1)
	l.expect(j, api.OutcomePass)
	if j.UnderLease || j.Spec.Holder != perfScheduled || j.AccountID != 0 {
		t.Errorf("a scheduled run: under a Lease %v, holder %q, account %d", j.UnderLease, j.Spec.Holder, j.AccountID)
	}
	ps, res := j.Spec.Performance, j.Result.Performance
	if ps.Release != "7.3" || ps.List != sets.Quick || !slices.Equal(ps.Metrics, bench1(t, sets.Quick)) || ps.Packages.Name != "release-7.3" || ps.ManifestSHA256 != rel.ManifestSHA256 {
		t.Errorf("spec %+v", ps)
	}
	if res.Kernel != candidateKernel || res.Booted["linux-aurora"] != candidateKernel || res.Booted["omarchy-ane"] != "0.5.6-1" || res.State != perf.Succeeded {
		t.Errorf("result %+v", res)
	}
	att, err := l.s.store.perfAttempts(l.name, testSet, perf.SideOmarchy, "7.3")
	if err != nil || len(att) != 1 || att[0].Run.Booted["omarchy-ane"] != "0.5.6-1" || !slices.Equal(att[0].Run.Metrics, ps.Metrics) {
		t.Fatalf("attempts %+v (%v)", att, err)
	}
	cur, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, testSet, "7.3")
	if len(cur) != len(ps.Metrics) {
		t.Errorf("%d current values, want %d", len(cur), len(ps.Metrics))
	}
	set, err := l.s.packageSet("release-7.3")
	if err != nil || set.Release != "7.3" || set.ManifestSHA256 != rel.ManifestSHA256 || len(set.Items) != 2 || set.Items[0].Source != api.SetFile {
		t.Errorf("the Release's set: %+v (%v)", set, err)
	}
	ops := strings.Join(l.sim.CandidateOps(), ", ")
	if !strings.Contains(ops, "stage release-7.3") || !strings.Contains(ops, "cleanup") {
		t.Errorf("candidate ops: %s", ops)
	}
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	if d.Kernel != agent.SimKnownGood || d.Lease != nil {
		t.Errorf("after the run: on %s, Lease %+v", d.Kernel, d.Lease)
	}
	if len(a.runs()) != 1 {
		t.Errorf("bench ran %d times", len(a.runs()))
	}
	l.noMorePerf(1)
}

// lab-agent runs the bench builtin on exactly the metrics labd named and
// uploads them with the Mac's own credentials: labd takes the upload, the
// values and conditions land as the attempt's, and the test's result says so.
func TestLabAgentRunsAPerformanceRunsMetricsAndLabdTakesTheUpload(t *testing.T) {
	l, a := perfLab(t, labConfig{}, nil)
	quick := bench1(t, sets.Quick)
	value := func(m string) float64 { return float64(10 + slices.Index(quick, m)) }
	a.set(func(m string, attempt int) (float64, string) { return value(m), "" })
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	j := l.nthPerfJob(1)
	l.expect(j, api.OutcomePass)
	if runs := a.runs(); len(runs) != 1 || !slices.Equal(runs[0], j.Spec.Performance.Metrics) {
		t.Fatalf("bench ran %v, labd named %v", runs, j.Spec.Performance.Metrics)
	}
	if len(j.Result.Tests) != 1 {
		t.Fatalf("tests %+v", j.Result.Tests)
	}
	tr := j.Result.Tests[0]
	up := tr.Bench
	if tr.Name != perfTest || !tr.Passed || up == nil || !up.Taken || up.Run != j.Result.Performance.Run ||
		up.State != perf.Succeeded || up.Results != len(quick) || up.Tries != 1 {
		t.Fatalf("the test's result: %+v, upload %+v", tr, up)
	}
	att, err := l.s.store.perfAttempts(l.name, testSet, perf.SideOmarchy, "7.3")
	if err != nil || len(att) != 1 || att[0].Run.State != perf.Succeeded {
		t.Fatalf("attempts %+v (%v)", att, err)
	}
	var cond map[string]any
	if json.Unmarshal(att[0].Run.Conditions, &cond) != nil || cond["on_ac"] != true || cond["backlight_percent"] != 50.0 || cond["set"] != nil {
		t.Errorf("conditions %s", att[0].Run.Conditions)
	}
	if len(att[0].Results) != len(quick) {
		t.Errorf("%d results, want %d", len(att[0].Results), len(quick))
	}
	for _, r := range att[0].Results {
		if r.Value == nil || *r.Value != value(r.Metric) {
			t.Errorf("%s: %+v, want %v", r.Metric, r, value(r.Metric))
		}
	}
	cur, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, testSet, "7.3")
	if len(cur) != len(quick) {
		t.Errorf("%d current values, want %d", len(cur), len(quick))
	}
}

// Quick while a Release has no verdict, the rest of Full once it is
// qualified: no benchmark is measured twice.
func TestQuickThenFullMeasuresNothingTwice(t *testing.T) {
	l, a := perfLab(t, labConfig{}, nil)
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	l.expect(l.nthPerfJob(1), api.OutcomePass)
	l.noMorePerf(1)
	l.qualify("7.3")
	j := l.nthPerfJob(2)
	l.expect(j, api.OutcomePass)
	quick, full := bench1(t, sets.Quick), bench1(t, sets.Full)
	if j.Spec.Performance.List != sets.Full {
		t.Errorf("list %s", j.Spec.Performance.List)
	}
	runs := a.runs()
	var measured []string
	for _, r := range runs {
		for _, m := range r {
			if slices.Contains(measured, m) {
				t.Errorf("%s measured twice: %v", m, runs)
			}
			measured = append(measured, m)
		}
	}
	slices.Sort(measured)
	slices.Sort(full)
	if !slices.Equal(measured, full) || !slices.Equal(runs[0], quick) {
		t.Errorf("measured %v, want the Full list %v, Quick first", runs, full)
	}
	l.noMorePerf(2)
}

// A benchmark that failed runs once more, alone, at the Mac's next slot;
// failing again, it is left as it is.
func TestAFailedBenchmarkRunsOnceMoreAlone(t *testing.T) {
	l, a := perfLab(t, labConfig{}, nil)
	a.set(func(m string, attempt int) (float64, string) {
		if m == "stream-triad" {
			return 0, perf.Error
		}
		if m == "fio-seq-read" && attempt == 1 {
			return 0, perf.DiskTooFull
		}
		if m == "boot-time" {
			return 0, perf.NoDriver // what the Stack lacks does not pass
		}
		return 100, ""
	})
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	l.expect(l.nthPerfJob(1), api.OutcomeTestsFailed)
	l.expect(l.nthPerfJob(2), api.OutcomePass)
	l.expect(l.nthPerfJob(3), api.OutcomeTestsFailed)
	l.noMorePerf(3)
	if runs := a.runs(); len(runs) != 3 || !slices.Equal(runs[1], []string{"fio-seq-read"}) || !slices.Equal(runs[2], []string{"stream-triad"}) {
		t.Fatalf("runs %v", runs)
	}
	cur, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, testSet, "7.3")
	for _, c := range cur {
		switch c.Metric {
		case "fio-seq-read":
			if c.Value == nil || c.Attempt != 2 {
				t.Errorf("fio, run again: %+v", c)
			}
		case "stream-triad":
			if c.Reason == nil || *c.Reason != perf.Error {
				t.Errorf("STREAM, in error twice: %+v", c)
			}
		}
	}
}

// lab perf rerun runs the Release again under the caller's Lease, taken for
// them; a rerun that fails keeps the old values, and every attempt stays.
func TestAFailedRerunKeepsTheOldValues(t *testing.T) {
	l, a := perfLab(t, labConfig{}, nil)
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	l.expect(l.nthPerfJob(1), api.OutcomePass)
	l.noMorePerf(1)
	before, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, testSet, "7.3")

	m := l.member("grace")
	if r, body := l.as(m.Token, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: "7.3"}); r.StatusCode != 403 {
		t.Errorf("an M team member with no Lease: %s %s", r.Status, body)
	}
	if r, body := l.as(harnessToken, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: "7.9"}); r.StatusCode != 404 {
		t.Errorf("no such Release: %s %s", r.Status, body)
	}
	a.set(func(m string, attempt int) (float64, string) {
		if m == "coremark-single" {
			return 0, perf.Error
		}
		return 50, ""
	})
	r, body := l.as(harnessToken, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: "7.3"})
	if r.StatusCode != 200 {
		t.Fatalf("rerun: %s %s", r.Status, body)
	}
	j := l.wait(decodeInto[api.Job](t, body).ID, 90*time.Second)
	l.expect(j, api.OutcomeTestsFailed)
	if !j.UnderLease || !j.Spec.Performance.Rerun || !j.Spec.Performance.LeaseTaken {
		t.Errorf("the rerun: under a Lease %v, %+v", j.UnderLease, j.Spec.Performance)
	}
	after, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, testSet, "7.3")
	for i := range after {
		if after[i].Run != before[i].Run || *after[i].Value != 100 {
			t.Errorf("%s: %+v, was %+v", after[i].Metric, after[i], before[i])
		}
	}
	att, _ := l.s.store.perfAttempts(l.name, testSet, perf.SideOmarchy, "7.3")
	if len(att) != 2 || att[1].Run.State != perf.Failed || len(att[1].Results) != len(before) {
		t.Errorf("attempts kept: %+v", att)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Lease == nil })
	// A failed benchmark of a rerun under a Lease is not run again by labd.
	l.noMorePerf(2)

	a.set(func(string, int) (float64, string) { return 60, "" })
	r, body = l.as(harnessToken, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: "7.3"})
	if r.StatusCode != 200 {
		t.Fatalf("rerun: %s %s", r.Status, body)
	}
	l.expect(l.wait(decodeInto[api.Job](t, body).ID, 90*time.Second), api.OutcomePass)
	after, _ = l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, testSet, "7.3")
	for _, c := range after {
		if *c.Value != 60 || c.Attempt != 3 {
			t.Errorf("a rerun that succeeds replaces the values: %+v", c)
		}
	}
}

// A Volunteer's Mac is measured only once its Volunteer turns Performance
// runs on at the Mac, and only at Access level Full.
func TestAVolunteersMacWithPerformanceOffIsNeverClaimed(t *testing.T) {
	l, _ := perfLab(t, labConfig{volunteer: true}, func(l *lab) {
		l.mustVolunteer("window", "add", "always")
	})
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	l.noMorePerf(0)
	l.mustVolunteer("performance", "on")
	l.waitDevice(func(d api.Device) bool { return d.Performance })
	l.noMorePerf(0) // still at Qualification
	if d := l.s.dev(l.name).snapshot(); !d.IsVolunteer() {
		t.Fatalf("not a Volunteer's Mac: %+v", d.Enrollment)
	}
	l.mustVolunteer("access", "full")
	l.expect(l.nthPerfJob(1), api.OutcomePass)
	l.mustVolunteer("performance", "off")
	l.waitDevice(func(d api.Device) bool { return !d.Performance })
	l.qualify("7.3")
	l.noMorePerf(1)
}

// A Release is measured newest first, on the Macs whose board it supports.
// One that changes the boot loader is not run on a Mac with no Sighting, and
// one whose files are not all in the artifact store is not staged.
func TestReleasesAreMeasuredNewestFirstWhereTheyCanBe(t *testing.T) {
	l, _ := perfLab(t, labConfig{}, nil)
	now := l.s.now()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "tester", Expires: time.Now().Add(time.Hour)} // no run is claimed while they are frozen
	v.mu.Unlock()
	l.perfRelease("7.1", now.Add(-3*time.Hour), []string{l.name})
	l.perfRelease("7.2", now.Add(-2*time.Hour), []string{"m2-max"})
	bl := l.perfRelease("7.4", now.Add(-time.Hour), []string{l.name}, "m1n1-aurora=1.6.0-1")
	l.perfRelease("7.3", now, []string{l.name})
	l.noMorePerf(0)
	v.mu.Lock()
	v.d.Lease = nil
	v.mu.Unlock()
	first := l.nthPerfJob(1)
	second := l.nthPerfJob(2)
	if first.Spec.Performance.Release != "7.3" || second.Spec.Performance.Release != "7.1" {
		t.Errorf("measured %s then %s", first.Spec.Performance.Release, second.Spec.Performance.Release)
	}
	l.noMorePerf(2)

	r, body := l.as(harnessToken, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: "7.2"})
	if r.StatusCode != 409 || !strings.Contains(body, "does not support") {
		t.Errorf("a rerun of a Release for another board: %s %s", r.Status, body)
	}
	r, body = l.as(harnessToken, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: bl.Name})
	if r.StatusCode != 409 || !strings.Contains(body, "no Sighting") {
		t.Errorf("a rerun of a Release that changes the boot loader: %s %s", r.Status, body)
	}
	set, _ := l.s.packageSet("release-7.1")
	os.Remove(l.s.artifactPath(set.Items[0].SHA256))
	r, body = l.as(harnessToken, "POST", "/api/perf/rerun", api.PerfRerunRequest{Mac: l.name, Release: "7.1"})
	if r.StatusCode != 409 || !strings.Contains(body, "not in the artifact store") {
		t.Errorf("a rerun of a Release with a file missing: %s %s", r.Status, body)
	}
}
