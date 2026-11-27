package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
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
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// candidateKernel is the release the simulated Mac boots for a set whose
// linux-aurora is a repo item at that version.
const candidateKernel = "7.3.0.aurora1-1"

// compareLab is a lab with a baselined Mac sighted on a controller, a set
// "7.3" of a repo kernel and an uploaded Mesa, and benchmark results that
// differ by the kernel the side runs: A (known-good) and B (the set's).
var compareKnobs sync.Once

func compareLab(t *testing.T, mod func(*labConfig)) (*lab, string) {
	t.Helper()
	compareKnobs.Do(func() { compareConfirmGrace, compareCheckEvery = 2*time.Second, 200*time.Millisecond })
	c := labConfig{oob: true}
	if mod != nil {
		mod(&c)
	}
	l := newLabWith(t, c)
	l.sim.SetBenchAnswer(func(_ int64, metric string) (float64, string) {
		_, kernel, _ := l.sim.Identity()
		return simBench(kernel, metric)
	})
	l.sim.TestFiles = func(test, kernel string) map[string][]byte {
		switch {
		case strings.HasSuffix(test, "-facts"):
			units := []string{}
			if kernel == candidateKernel {
				units = []string{"bluetooth.service"}
			}
			b, _ := json.Marshal(map[string]any{"gpu_firmware": "asahi: firmware version 13.5 on " + kernel, "sio": "sio: version 2",
				"thunderbolt": "2 devices: hub, dock", "failed_units": units, "boot_time": "Startup finished in 9.1s"})
			return map[string][]byte{"facts.json": b}
		}
		return nil
	}
	l.baseline()
	mesa := l.artifact("mesa-aurora package")
	code, _, body := l.set(harnessToken, api.SetRequest{Name: "7.3", Items: []api.SetItem{
		{Package: "linux-aurora", Source: api.SetRepo, Ref: candidateKernel},
		{Package: "mesa-aurora", Source: api.SetFile, Ref: mesa, Version: "25.3.0-1"},
	}}, "/api/sets")
	if code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	return l, mesa
}

// simBench is what the simulated bench runner measures of bench-1's Quick
// list on the kernel: on the candidate kernel Speedometer is 25% faster
// (beyond its 3%), CoreMark 4% (within its 5%), MotionMark 4% slower (beyond
// its 3%), idle power 10% lower and boot time 10% longer (lower is better
// for both); STREAM fell back on it, and the Neural engine has no driver on
// either.
func simBench(kernel, metric string) (float64, string) {
	cand := kernel == candidateKernel
	pick := func(a, b float64) (float64, string) {
		if cand {
			return b, ""
		}
		return a, ""
	}
	switch metric {
	case "speedometer":
		return pick(20, 25)
	case "coremark-single":
		return pick(100, 104)
	case "motionmark":
		return pick(1000, 960)
	case "power-idle":
		return pick(10, 9)
	case "boot-time":
		return pick(10, 11)
	case "stream-triad":
		if cand {
			return 0, perf.FellBack
		}
		return 100, ""
	case "neural-engine-mobilenet-v1":
		return 0, perf.NoDriver
	}
	return 100, ""
}

func (l *lab) compare(token string, req api.CompareRequest) (int, *api.Job, string) {
	l.t.Helper()
	if req.Mac == "" {
		req.Mac = l.name
	}
	r, body := l.as(token, "POST", "/api/compares", req)
	if r.StatusCode != 200 {
		return r.StatusCode, nil, body
	}
	var j api.Job
	if err := json.Unmarshal([]byte(body), &j); err != nil {
		l.t.Fatalf("%s: %v", body, err)
	}
	return 200, &j, body
}

func (l *lab) mustCompare(req api.CompareRequest) *api.Job {
	l.t.Helper()
	code, j, body := l.compare(harnessToken, req)
	if code != 200 {
		l.t.Fatalf("compare: %d %s", code, body)
	}
	return l.wait(j.ID, 90*time.Second)
}

func rowOf(t *testing.T, res *api.CompareResult, name string) api.CompareRow {
	t.Helper()
	for _, r := range res.Rows {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no row %s in %+v", name, res.Rows)
	return api.CompareRow{}
}

func near(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 0.01 }

// A Compare of the Mac's own system against a set benchmarks and checks the
// known-good system as A, boots the set as a Candidate boot for B, and
// reports each benchmark's change against the Mac's noise, omarchy-m-test's
// checks that regressed or were fixed, and each side's facts.
func TestCompareCurrentAgainstASet(t *testing.T) {
	l, mesa := compareLab(t, nil)
	l.sim.SetCandidateBehavior("7.3", "omt-regress")
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3"})
	l.expect(j, api.OutcomePass)
	res := j.Result.Compare
	if res == nil || len(res.Sides) != 2 {
		t.Fatalf("report %+v", res)
	}
	a, b := res.Side(api.SideA), res.Side(api.SideB)
	if a.Boot != "known-good" || a.Kernel != agent.SimKnownGood || a.Set != api.CompareCurrent || len(a.Packages) != 0 {
		t.Errorf("A: %+v", a)
	}
	if b.Boot != api.CandidateConfirmed || b.Kernel != candidateKernel || b.Set != "7.3" {
		t.Errorf("B: %+v", b)
	}
	if len(b.Packages) != 2 || b.Packages[1].SHA256 != mesa || b.Packages[0].Name != "linux-aurora" || b.Packages[0].SHA256 == "" || !strings.HasPrefix(b.Packages[0].URL, "/") {
		t.Errorf("B's files: %+v", b.Packages)
	}
	if !strings.Contains(a.GPUFirmware, "13.5") || a.SIO != "sio: version 2" || a.Thunderbolt != "2 devices: hub, dock" || len(a.FailedUnits) != 0 {
		t.Errorf("A's facts: %+v", a)
	}
	if !slices.Equal(b.FailedUnits, []string{"bluetooth.service"}) {
		t.Errorf("B's failed units: %v", b.FailedUnits)
	}

	if j.Spec.Compare.Bench != testSet || res.Bench != testSet {
		t.Errorf("the job's Benchmark set: %q, the report's %q", j.Spec.Compare.Bench, res.Bench)
	}
	set, _ := sets.Load(testSet)
	var ids []string
	for _, r := range res.Rows {
		ids = append(ids, r.Name)
	}
	var quick []string
	for _, m := range set.List(sets.Quick) {
		quick = append(quick, m.ID)
	}
	if !slices.Equal(ids, quick) {
		t.Errorf("rows %v, want bench-1's Quick list %v", ids, quick)
	}
	sp := rowOf(t, res, "speedometer")
	if !near(sp.A, 20) || !near(sp.B, 25) || !near(sp.Delta, 25) || sp.Verdict != api.VerdictBetter || sp.Tolerance != 3 {
		t.Errorf("Speedometer: %+v", sp)
	}
	if c := rowOf(t, res, "coremark-single"); !near(c.Delta, 4) || c.Verdict != api.VerdictNoChange || c.Tolerance != 5 {
		t.Errorf("CoreMark, within its tolerance: %+v", c)
	}
	if m := rowOf(t, res, "motionmark"); !near(m.Delta, -4) || m.Verdict != api.VerdictWorse || m.Tolerance != 3 {
		t.Errorf("MotionMark, beyond its tolerance: %+v", m)
	}
	if p := rowOf(t, res, "power-idle"); !near(p.Delta, -10) || p.Verdict != api.VerdictBetter || p.HigherIsBetter {
		t.Errorf("idle power, lower is better: %+v", p)
	}
	if bt := rowOf(t, res, "boot-time"); !near(bt.Delta, 10) || bt.Verdict != api.VerdictWorse {
		t.Errorf("boot time, lower is better: %+v", bt)
	}
	if st := rowOf(t, res, "stream-triad"); st.Verdict != api.VerdictNotCompared || st.Delta != nil || !near(st.A, 100) || st.Note != "B: "+perf.FellBack {
		t.Errorf("a benchmark that fell back on B: %+v", st)
	}
	if ne := rowOf(t, res, "neural-engine-mobilenet-v1"); ne.Verdict != api.VerdictNotCompared || ne.Note != "A: no_driver; B: no_driver" {
		t.Errorf("a benchmark with no driver: %+v", ne)
	}
	tests := l.sim.BenchRuns()
	var omarchy []string
	for _, m := range set.List(sets.Quick) {
		if m.Omarchy != nil {
			omarchy = append(omarchy, m.ID)
		}
	}
	if len(tests) != 2 || !slices.Equal(tests[0], omarchy) || !slices.Equal(tests[1], omarchy) {
		t.Errorf("each side ran %v, want bench-1's Quick list on Omarchy %v", tests, omarchy)
	}
	for _, tr := range j.Result.Tests {
		if strings.HasSuffix(tr.Name, "-bench") && (tr.Bench != nil || tr.Error != "") {
			t.Errorf("%s uploaded to a Performance run or failed: %+v", tr.Name, tr)
		}
	}

	var wifi *api.CompareCheck
	for i, c := range res.Checks {
		if c.ID == "wifi.connected" {
			wifi = &res.Checks[i]
		}
	}
	if wifi == nil || wifi.A != "pass" || wifi.B != "fail" || wifi.Change != api.CheckRegressed || res.Regressed != 1 {
		t.Errorf("omarchy-m-test: %+v (%d regressed)", res.Checks, res.Regressed)
	}
	// The Candidate boot's root holds no /.snapshots subvolume: the lab's
	// doing, not the set's.
	seen := 0
	for _, c := range res.Checks {
		if c.ID == "system.snapshots" || c.ID == "boot.chain" {
			seen++
		}
		if (c.ID == "system.snapshots" || c.ID == "boot.chain") && (c.A != "pass" || c.B != "fail" || c.Change != api.CheckNotCompared || !strings.Contains(c.Note, "Candidate boot")) {
			t.Errorf("%s: %+v", c.ID, c)
		}
	}
	if seen != 2 {
		t.Errorf("system.snapshots and boot.chain: %d of 2 in %+v", seen, res.Checks)
	}

	ops := strings.Join(l.sim.CandidateOps(), ", ")
	for _, want := range []string{"resolve linux-aurora=" + candidateKernel, "stage 7.3", "check", "cleanup"} {
		if !strings.Contains(ops, want) {
			t.Errorf("candidate ops %q lack %q", ops, want)
		}
	}
	// The Lease the Compare took ends as the job frees the Mac.
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Lease == nil })
	if d.Kernel != agent.SimKnownGood || d.Health.JobTag != "" {
		t.Errorf("after the Compare the Mac is on %s (job tag %q)", d.Kernel, d.Health.JobTag)
	}
}

// The Lease a Compare took ends before the Mac is ready again: nobody sees
// a ready Mac still under it, however long labd takes after freeing it.
func TestACompareEndsItsLeaseBeforeTheMacIsReady(t *testing.T) {
	l, _ := compareLab(t, nil)
	v := l.s.dev("sim-mac")
	seen := make(chan api.Device, 1)
	v.mu.Lock()
	v.afterFree = func() {
		seen <- v.snapshot()
		time.Sleep(300 * time.Millisecond)
	}
	v.mu.Unlock()
	l.expect(l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3"}), api.OutcomePass)
	select {
	case d := <-seen:
		if d.State == api.StateReady && d.Lease != nil {
			t.Errorf("the Mac was ready under the Compare's Lease: %+v", d.Lease)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job never freed the Mac")
	}
}

// A Compare is private to Maintainers until one publishes it: the M team
// sees neither the job nor its page, nor does anyone its public page.
// Published, its report has a public page.
func TestACompareIsPrivateUntilPublished(t *testing.T) {
	l, _ := compareLab(t, nil)
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3"})
	l.expect(j, api.OutcomePass)
	m := l.member("grace")
	if r, _ := l.as(m.Token, "GET", "/api/jobs/"+j.ID, nil); r.StatusCode != 404 {
		t.Errorf("an M team member reads an unpublished Compare: %s", r.Status)
	}
	if r, _ := l.as("", "GET", "/compares/"+j.ID, nil); r.StatusCode != 404 {
		t.Errorf("the public page of an unpublished Compare: %s", r.Status)
	}
	if r, body := l.as(m.Token, "POST", "/api/compares/"+j.ID+"/publish", nil); r.StatusCode != 403 {
		t.Errorf("an M team member publishes: %s %s", r.Status, body)
	}
	r, body := l.as(harnessToken, "POST", "/api/compares/"+j.ID+"/publish", nil)
	if r.StatusCode != 200 {
		t.Fatalf("publish: %s %s", r.Status, body)
	}
	got, _ := l.s.store.job(j.ID)
	if got.Result.Compare.Published == nil || got.Result.Compare.PublishedBy != harnessMaintainer {
		t.Errorf("published: %+v", got.Result.Compare)
	}
	if r, _ := l.as(m.Token, "GET", "/api/jobs/"+j.ID, nil); r.StatusCode != 200 {
		t.Errorf("an M team member reads a published Compare: %s", r.Status)
	}
	r, page := l.as("", "GET", "/compares/"+j.ID, nil)
	if r.StatusCode != 200 || !strings.Contains(page, "speedometer") || !strings.Contains(page, "25.0%") || !strings.Contains(page, "better") {
		t.Errorf("public page: %s\n%s", r.Status, page)
	}
}

// A set that does not boot to a confirmed desktop is "boot failed": the Mac
// comes back on its known-good system, the candidate's journal is read back
// and kept, the candidate is removed, and the report has A alone.
func TestACandidateThatIsNotConfirmedIsBootFailed(t *testing.T) {
	l, _ := compareLab(t, nil)
	l.sim.SetCandidateBehavior("7.3", "unconfirmed")
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3"})
	l.expect(j, api.OutcomeBootFailed)
	b := j.Result.Compare.Side(api.SideB)
	if b.Boot != api.CandidateBootFailed || !strings.Contains(b.Journal, "gdm: no session") || len(b.Bench) != 0 {
		t.Errorf("B: %+v", b)
	}
	if a := j.Result.Compare.Side(api.SideA); len(a.Bench) == 0 {
		t.Errorf("A ran no benchmarks: %+v", a)
	}
	ops := l.sim.CandidateOps()
	if !slices.Contains(ops, "result") || ops[len(ops)-1] != "cleanup" {
		t.Errorf("candidate ops %v", ops)
	}
	if b, err := os.ReadFile(filepath.Join(l.s.jobDir(j.ID), "candidate-b", "journal.txt")); err != nil || !strings.Contains(string(b), "gdm: no session") {
		t.Errorf("the candidate's journal is not kept with the job: %q %v", b, err)
	}
	if sp := rowOf(t, j.Result.Compare, "speedometer"); sp.Verdict != api.VerdictNotCompared || sp.B != nil || sp.Note != "B did not run it" {
		t.Errorf("a row with no B: %+v", sp)
	}
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	if d.Kernel != agent.SimKnownGood {
		t.Errorf("the Mac is on %s", d.Kernel)
	}
}

// A candidate that never ran (the Mac booted another entry) is said to be
// not booted, not to have missed its confirmation.
func TestACandidateThatNeverRanIsNotBooted(t *testing.T) {
	l, _ := compareLab(t, nil)
	l.sim.SetCandidateBehavior("7.3", "skipped")
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3"})
	l.expect(j, api.OutcomeBootFailed)
	b := j.Result.Compare.Side(api.SideB)
	if b.Boot != api.CandidateNotBooted || !strings.Contains(b.Error, "the candidate entry was not booted") || strings.Contains(b.Error, "in time") {
		t.Errorf("B: %+v", b)
	}
	if !strings.Contains(j.Summary, "the candidate entry was not booted") {
		t.Errorf("summary %q", j.Summary)
	}
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	if d.Kernel != agent.SimKnownGood {
		t.Errorf("the Mac is on %s", d.Kernel)
	}
}

// Both sides can be sets: A is then a Candidate boot too.
func TestCompareTwoSets(t *testing.T) {
	l, _ := compareLab(t, nil)
	if code, _, body := l.set(harnessToken, api.SetRequest{Name: "7.2", Items: []api.SetItem{
		{Package: "mesa-aurora", Source: api.SetRepo, Ref: "25.2.0-1"},
	}}, "/api/sets"); code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	j := l.mustCompare(api.CompareRequest{Before: "7.2", After: "7.3"})
	l.expect(j, api.OutcomePass)
	a := j.Result.Compare.Side(api.SideA)
	if a.Boot != api.CandidateConfirmed || a.Set != "7.2" || a.Kernel != agent.SimKnownGood || len(a.Packages) != 1 {
		t.Errorf("A: %+v", a)
	}
	n := 0
	for _, op := range l.sim.CandidateOps() {
		if strings.HasPrefix(op, "stage ") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("staged %d candidates: %v", n, l.sim.CandidateOps())
	}
}

// What cannot run is refused before anything is queued.
func TestCompareRefusals(t *testing.T) {
	l, _ := compareLab(t, nil)
	m := l.member("grace")
	l.doneBuild("b-kern", "", api.BuildFile{Name: "Image.tar", SHA256: l.artifact("krel=7.3.0-k"), Size: 9})
	if code, _, body := l.set(harnessToken, api.SetRequest{Name: "kernel-only", Items: []api.SetItem{{Package: "linux-aurora", Source: api.SetBuild, Ref: "b-kern"}}}, "/api/sets"); code != 200 {
		t.Fatalf("set: %d %s", code, body)
	}
	for _, c := range []struct {
		token string
		req   api.CompareRequest
		code  int
		says  string
	}{
		{m.Token, api.CompareRequest{Before: "current", After: "7.3"}, 403, "needs a Maintainer"},
		{harnessToken, api.CompareRequest{Before: "current", After: "nope"}, 404, "no package set"},
		{harnessToken, api.CompareRequest{Before: "current", After: "current"}, 400, "--after"},
		{harnessToken, api.CompareRequest{Before: "current", After: "7.3", Tier: "huge"}, 400, "tier"},
		{harnessToken, api.CompareRequest{Before: "current", After: "kernel-only"}, 400, "lab package"},
		{harnessToken, api.CompareRequest{Before: "current", After: "7.3", MacOS: true}, 409, "lab macos enroll"},
		{harnessToken, api.CompareRequest{Mac: "nowhere", Before: "current", After: "7.3"}, 404, "nowhere"},
	} {
		code, _, body := l.compare(c.token, c.req)
		if code != c.code || !strings.Contains(body, c.says) {
			t.Errorf("%+v: %d %s, want %d saying %q", c.req, code, body, c.code, c.says)
		}
	}
}

// --hands pauses the Compare on B's candidate boot for a person, until they
// press Done (or run lab compare done).
func TestCompareWaitsForHandsUntilDone(t *testing.T) {
	l, _ := compareLab(t, nil)
	code, j, body := l.compare(harnessToken, api.CompareRequest{Before: api.CompareCurrent, After: "7.3", Hands: true})
	if code != 200 {
		t.Fatalf("compare: %d %s", code, body)
	}
	l.waitJob(j.ID, func(j *api.Job) bool {
		return j.Result.Compare != nil && j.Result.Compare.Hands != nil && j.Result.Compare.Hands.Waiting
	})
	if d := l.s.dev(l.name).snapshot(); d.Kernel != candidateKernel {
		t.Errorf("waiting for hands on %s, not the candidate", d.Kernel)
	}
	if got := l.s.handsWaiting(l.s.dev(l.name)); got != j.ID {
		t.Errorf("the Mac's page shows Done for %q, want %s", got, j.ID)
	}
	if l.s.mayHands(Person{Handle: "vera", AccountID: 77}, j) {
		t.Errorf("a person who neither maintains nor lends the Mac may press Done")
	}
	if r, body := l.as(harnessToken, "POST", "/api/compares/"+j.ID+"/done", nil); r.StatusCode != 200 {
		t.Fatalf("done: %s %s", r.Status, body)
	}
	done := l.wait(j.ID, 60*time.Second)
	l.expect(done, api.OutcomePass)
	h := done.Result.Compare.Hands
	if h == nil || h.Waiting || h.Done == nil || h.By != harnessMaintainer {
		t.Errorf("hands: %+v", h)
	}
}

// --macos with no macOS result kept boots macOS once for the Mac's macOS
// run of the job's set: its values sit beside A's by metric id, compared
// against A by the metric's tolerance, "API differs" marked and compared.
func TestCompareWithMacOS(t *testing.T) {
	l, _ := compareLab(t, nil)
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	// An Attended Mac: the scheduler waits for a person, and the Compare
	// takes the run it queued.
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	l.sim.MacOSFor = 2 * time.Second
	l.sim.InMacOS = func() {
		run := pollAs(t, l, secret, bootHash(9), l.s.now(), "")
		if run == nil {
			t.Error("the lab's boot was handed no run")
			return
		}
		up := macOSUpload(t, perf.Succeeded, map[string]float64{"speedometer": 30, "coremark-single": 102, "decode-h264-1080p": 150})
		up["entries"] = append(up["entries"].([]map[string]any), perfEntry(t, "motionmark", 0, perf.NoBuild))
		if r, body := l.as(secret, "POST", fmt.Sprintf("/api/macos/runner/performance/%d/results", run.PerfRun), up); r.StatusCode != 200 {
			t.Errorf("results: %d %s", r.StatusCode, body)
		}
	}
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3", MacOS: true})
	l.expect(j, api.OutcomePass)
	res := j.Result.Compare
	if m := res.Side(api.SideMacOS); m == nil || m.Boot != api.MacOSDone || len(m.Bench) != 4 {
		t.Fatalf("macOS: %+v", m)
	}
	if sp := rowOf(t, res, "speedometer"); !near(sp.MacOS, 30) || !near(sp.MacOSDelta, 50) || sp.MacOSVerdict != api.VerdictBetter || sp.MacOSNote != "" {
		t.Errorf("Speedometer in macOS: %+v", sp)
	}
	if c := rowOf(t, res, "coremark-single"); !near(c.MacOSDelta, 2) || c.MacOSVerdict != api.VerdictNoChange {
		t.Errorf("CoreMark in macOS: %+v", c)
	}
	set, _ := sets.Load(testSet)
	dm, _ := set.Metric("decode-h264-1080p")
	if d := rowOf(t, res, "decode-h264-1080p"); !near(d.MacOSDelta, 50) || d.MacOSVerdict != api.VerdictBetter ||
		d.MacOSNote != "API differs: "+dm.Omarchy.API+" on Omarchy, VideoToolbox on macOS" {
		t.Errorf("H.264 decode in macOS: %+v", d)
	}
	if m := rowOf(t, res, "motionmark"); m.MacOS != nil || m.MacOSVerdict != api.VerdictNotCompared || m.MacOSNote != perf.NoBuild {
		t.Errorf("MotionMark in macOS: %+v", m)
	}
	if p := rowOf(t, res, "power-idle"); p.MacOSVerdict != api.VerdictNotCompared || p.MacOSNote != "macOS did not run it" {
		t.Errorf("idle power in macOS: %+v", p)
	}
	if b := rowOf(t, res, "boot-time"); b.MacOSVerdict != "" || b.MacOS != nil {
		t.Errorf("boot time, Omarchy only, in macOS: %+v", b)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Kernel == agent.SimKnownGood })
}

// A change within the metric's tolerance in the set is no change; beyond
// it, better or worse by the set's direction, lower better where it says
// so: the direction is the set's, whatever a side's entry says.
func TestCompareVerdictsByTheSetsTolerance(t *testing.T) {
	set := &sets.Set{Name: "bench-9", N: 9, Metrics: []sets.Metric{
		{ID: "cpu", Unit: "score", Direction: "higher", Tolerance: 2, List: sets.Quick, Omarchy: &sets.Side{}, MacOS: &sets.Side{}},
		{ID: "watts", Unit: "W", Direction: "lower", Tolerance: 5, List: sets.Quick, Omarchy: &sets.Side{}, MacOS: &sets.Side{}},
		{ID: "gpu", Unit: "score", Direction: "higher", Tolerance: 3, List: sets.Full, Omarchy: &sets.Side{}},
	}}
	v := func(f float64) *float64 { return &f }
	side := func(name string, vals map[string]float64) *api.CompareSide {
		s := &api.CompareSide{Side: name}
		for id, x := range vals {
			s.Bench = append(s.Bench, api.BenchResult{Name: id, Value: v(x), HigherIsBetter: true})
		}
		return s
	}
	for _, c := range []struct {
		metric string
		a, b   float64
		want   string
	}{
		{"cpu", 100, 102, api.VerdictNoChange},
		{"cpu", 100, 98, api.VerdictNoChange},
		{"cpu", 100, 103, api.VerdictBetter},
		{"cpu", 100, 97, api.VerdictWorse},
		{"watts", 10, 10.5, api.VerdictNoChange},
		{"watts", 10, 9, api.VerdictBetter},
		{"watts", 10, 11, api.VerdictWorse},
	} {
		rows := compareRows(set, sets.Quick, side("A", map[string]float64{c.metric: c.a}), side("B", map[string]float64{c.metric: c.b}), nil)
		var r *api.CompareRow
		for i := range rows {
			if rows[i].Name == c.metric {
				r = &rows[i]
			}
		}
		if len(rows) != 2 || r == nil {
			t.Fatalf("rows %+v, want the Quick list's two", rows)
		}
		if r.Verdict != c.want || r.Delta == nil || r.Tolerance != set.Metrics[slices.IndexFunc(set.Metrics, func(m sets.Metric) bool { return m.ID == c.metric })].Tolerance {
			t.Errorf("%s %v→%v: %+v, want %s", c.metric, c.a, c.b, r, c.want)
		}
	}
	if _, got := verdict(0, 5, true, 2); got != api.VerdictNotCompared {
		t.Errorf("against 0: %s", got)
	}
}

// A metric a side did not run, or ran with a reason code, is not compared,
// with the reason.
func TestCompareRowsNotComparedSayWhy(t *testing.T) {
	set, err := sets.Load(testSet)
	if err != nil {
		t.Fatal(err)
	}
	v := func(f float64) *float64 { return &f }
	a := &api.CompareSide{Side: api.SideA, Bench: []api.BenchResult{
		{Name: "speedometer", Value: v(20)}, {Name: "motionmark", Value: v(1000)}, {Name: "stream-triad", Note: perf.DiskTooFull},
	}}
	b := &api.CompareSide{Side: api.SideB, Bench: []api.BenchResult{
		{Name: "speedometer", Note: perf.SoftwareRenderer}, {Name: "stream-triad", Value: v(90)},
	}}
	rows := compareRows(set, sets.Quick, a, b, nil)
	byID := map[string]api.CompareRow{}
	for _, r := range rows {
		byID[r.Name] = r
		if r.Verdict != api.VerdictNotCompared || r.Delta != nil || r.MacOSVerdict != "" {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
	for id, want := range map[string]string{
		"speedometer":     "B: software_renderer",
		"motionmark":      "B did not run it",
		"stream-triad":    "A: disk_too_full",
		"coremark-single": "A did not run it; B did not run it",
	} {
		if byID[id].Note != want {
			t.Errorf("%s: note %q, want %q", id, byID[id].Note, want)
		}
	}
}

var _ = http.StatusOK

// A set that changes the boot loader is refused at once on a Mac with no
// Sighting, before side A runs: a boot loader is swapped only on a cabled
// Mac. A set at the Mac's own m1n1 (a captured one) changes nothing.
func TestABootLoaderSetNeedsASightingFromTheStart(t *testing.T) {
	l, _ := compareLab(t, func(c *labConfig) { c.oob = false })
	l.waitDevice(func(d api.Device) bool { return len(d.Facts.Stack) > 0 })
	for _, s := range []api.SetRequest{
		{Name: "new-m1n1", Items: []api.SetItem{{Package: "m1n1-aurora", Source: api.SetRepo, Ref: "1.6.0-1"}}},
		{Name: "own-m1n1", Items: []api.SetItem{{Package: "m1n1-aurora", Source: api.SetRepo, Ref: "1.5.2-1"}, {Package: "linux-aurora", Source: api.SetRepo, Ref: candidateKernel}}},
	} {
		if code, _, body := l.set(harnessToken, s, "/api/sets"); code != 200 {
			t.Fatalf("set: %d %s", code, body)
		}
	}
	for _, req := range []api.CompareRequest{{Before: "current", After: "new-m1n1"}, {Before: "new-m1n1", After: "7.3"}} {
		code, _, body := l.compare(harnessToken, req)
		if code != 409 || !strings.Contains(body, "set new-m1n1 changes the boot loader (m1n1-aurora 1.6.0-1)") || !strings.Contains(body, "no Sighting") {
			t.Errorf("%+v: %d %s", req, code, body)
		}
	}
	if code, _, body := l.compare(harnessToken, api.CompareRequest{Before: "current", After: "own-m1n1"}); code != 200 {
		t.Errorf("a set at the Mac's own m1n1: %d %s", code, body)
	}
}

// The runner of a Benchmark set writes metric ids and reason codes.
func TestCompareReadsABenchmarkSetsEntries(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bench.json")
	os.WriteFile(p, []byte(`[{"set":"`+testSet+`","metric":"7zip","category":"CPU","version":"26.06","value":50450,"unit":"MIPS","higher_is_better":true,"spread":null,"seconds":60,"cool_down":null,"api":null,"capacity":null,"reason":null},
{"set":"`+testSet+`","metric":"clpeak-fp32","category":"GPU","version":"3.0.5","value":null,"unit":"GFLOPS","higher_is_better":true,"spread":null,"seconds":2,"cool_down":{"seconds":0,"temp_c":41.0},"api":"Rusticl","capacity":null,"reason":"fell_back"}]`), 0o644)
	rs := readBench(p)
	if len(rs) != 2 || rs[0].Name != "7zip" || rs[0].Value == nil || *rs[0].Value != 50450 || rs[0].Note != "" {
		t.Fatalf("read %+v", rs)
	}
	if rs[1].Name != "clpeak-fp32" || rs[1].Value != nil || rs[1].Note != "fell_back" || rs[1].API != "Rusticl" {
		t.Errorf("read %+v", rs[1])
	}
}
