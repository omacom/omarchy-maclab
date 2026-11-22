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
	"github.com/omacom/omarchy-maclab/internal/api"
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
	l.sim.TestFiles = func(test, kernel string) map[string][]byte {
		switch {
		case strings.HasSuffix(test, "-bench"):
			return map[string][]byte{"bench.json": simBench(kernel)}
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

// simBench is a default-tier bench.json: on the candidate kernel
// Speedometer is 25% faster, 7-Zip 1% (within its 2% noise), glmark2 20%
// slower, and boot time (lower is better) 10% shorter.
func simBench(kernel string) []byte {
	cand := kernel == candidateKernel
	pick := func(a, b float64) float64 {
		if cand {
			return b
		}
		return a
	}
	rs := []map[string]any{
		{"name": "speedometer-chrome", "version": "3.1", "value": pick(20, 25), "unit": "score", "higher_is_better": true, "spread": 0.4, "seconds": 120},
		{"name": "motionmark-chrome", "version": "1.3", "value": pick(1000, 1010), "unit": "score", "higher_is_better": true, "spread": nil, "seconds": 100},
		{"name": "7zip", "version": "26.06-1", "value": pick(100, 101), "unit": "MIPS", "higher_is_better": true, "spread": nil, "seconds": 60},
		{"name": "glmark2", "version": "2023.01-2", "value": pick(5000, 4000), "unit": "score", "higher_is_better": true, "spread": nil, "seconds": 40},
		{"name": "vkmark", "version": "1:2025.01-2", "value": nil, "unit": nil, "higher_is_better": nil, "spread": nil, "seconds": 0, "skipped": "no Vulkan device"},
		{"name": "boot", "version": "systemd", "value": pick(10, 9), "unit": "s", "higher_is_better": false, "spread": nil, "seconds": 0},
	}
	b, _ := json.Marshal(rs)
	return b
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

	sp := rowOf(t, res, "speedometer-chrome")
	if !near(sp.A, 20) || !near(sp.B, 25) || !near(sp.Delta, 25) || sp.Verdict != api.VerdictBetter || sp.Noise != 3 || sp.NoiseFrom != "default" {
		t.Errorf("Speedometer: %+v", sp)
	}
	if z := rowOf(t, res, "7zip"); !near(z.Delta, 1) || z.Verdict != api.VerdictNoChange || z.Noise != 2 {
		t.Errorf("7-Zip: %+v", z)
	}
	if g := rowOf(t, res, "glmark2"); !near(g.Delta, -20) || g.Verdict != api.VerdictWorse || g.Noise != 5 {
		t.Errorf("glmark2: %+v", g)
	}
	if bt := rowOf(t, res, "boot"); !near(bt.Delta, -10) || bt.Verdict != api.VerdictBetter {
		t.Errorf("boot time, lower is better: %+v", bt)
	}
	if vk := rowOf(t, res, "vkmark"); vk.Verdict != api.VerdictNotCompared || !strings.Contains(vk.Note, "no Vulkan device") {
		t.Errorf("a skipped benchmark: %+v", vk)
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
	if r.StatusCode != 200 || !strings.Contains(page, "speedometer-chrome") || !strings.Contains(page, "25.0%") || !strings.Contains(page, "better") {
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
	if sp := rowOf(t, j.Result.Compare, "speedometer-chrome"); sp.Verdict != api.VerdictNotCompared || sp.B != nil {
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

// --macos adds the same tier run in macOS, where the macOS runner uploads
// it: its values sit beside A's, compared where the API is the same.
func TestCompareWithMacOS(t *testing.T) {
	l, _ := compareLab(t, nil)
	macosBench(t, l)
	if _, err := l.s.macOS().enrolSecret(l.name, "mos_test-secret"); err != nil {
		t.Fatal(err)
	}
	l.sim.MacOSFor = 2 * time.Second
	l.sim.InMacOS = func() {
		r, body := l.as("mos_test-secret", "GET", "/api/macos/runner/poll", nil)
		var poll api.MacOSPoll
		if r.StatusCode != 200 || json.Unmarshal([]byte(body), &poll) != nil || poll.Run == nil {
			t.Errorf("poll: %s %s", r.Status, body)
			return
		}
		v := func(f float64) *float64 { return &f }
		up := api.MacOSResults{RunID: poll.Run.ID, Results: []api.BenchResult{
			{Name: "speedometer/chrome", Version: "3.1", Value: v(30), Unit: "score", HigherIsBetter: true},
			{Name: "7zip", Version: "26.06", Value: v(90), Unit: "MIPS", HigherIsBetter: true},
			{Name: "glmark2", Note: api.NoMacOSEquivalent},
		}}
		if r, body := l.as("mos_test-secret", "POST", "/api/macos/runner/results", up); r.StatusCode != 200 {
			t.Errorf("results: %s %s", r.Status, body)
		}
	}
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3", MacOS: true})
	l.expect(j, api.OutcomePass)
	res := j.Result.Compare
	if m := res.Side(api.SideMacOS); m == nil || m.Boot != api.MacOSDone || len(m.Bench) != 3 {
		t.Fatalf("macOS: %+v", m)
	}
	if sp := rowOf(t, res, "speedometer-chrome"); !near(sp.MacOS, 30) || !near(sp.MacOSDelta, 50) || sp.MacOSVerdict != api.VerdictBetter {
		t.Errorf("Speedometer in macOS: %+v", sp)
	}
	if g := rowOf(t, res, "glmark2"); g.MacOS != nil || g.MacOSNote != api.NoMacOSEquivalent {
		t.Errorf("glmark2 in macOS: %+v", g)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Kernel == agent.SimKnownGood })
}

// Noise is twice the spread of the Mac's last 3 A-side runs of a
// benchmark, and its default until 3 exist.
func TestCompareNoise(t *testing.T) {
	for _, c := range []struct {
		name    string
		history []float64
		noise   float64
		from    string
	}{
		{"speedometer-chrome", nil, 3, "default"},
		{"jetstream-chrome", []float64{1, 2}, 3, "default"},
		{"7zip", nil, 2, "default"},
		{"vkmark", nil, 5, "default"},
		{"geekbench-single", nil, 5, "default"},
		{"speedometer-chrome", []float64{50, 90, 100, 110}, 40, "last 3 A-side runs"},
		{"7zip", []float64{100, 100, 100}, 0, "last 3 A-side runs"},
	} {
		n, from := compareNoise(c.name, c.history)
		if math.Abs(n-c.noise) > 0.01 || from != c.from {
			t.Errorf("%s %v: %.2f%% from %s, want %.2f%% from %s", c.name, c.history, n, from, c.noise, c.from)
		}
	}
	for _, c := range []struct {
		a, b   float64
		higher bool
		noise  float64
		want   string
	}{
		{100, 103, true, 2, api.VerdictBetter},
		{100, 102, true, 2, api.VerdictNoChange},
		{100, 97, true, 2, api.VerdictWorse},
		{10, 9, false, 5, api.VerdictBetter},
		{10, 11, false, 5, api.VerdictWorse},
	} {
		d, got := verdict(c.a, c.b, c.higher, c.noise)
		if got != c.want {
			t.Errorf("%v→%v (higher better %v, noise %v): %s (%.1f%%), want %s", c.a, c.b, c.higher, c.noise, got, d, c.want)
		}
	}
}

// Each Compare's A values add to the Mac's history, so the fourth Compare
// on a Mac judges by its own noise.
func TestCompareNoiseLearnsFromEarlierCompares(t *testing.T) {
	l, _ := compareLab(t, nil)
	for i := 0; i < 3; i++ {
		l.s.addNoise(l.name, map[string]float64{"speedometer-chrome": 20 + float64(i)})
	}
	j := l.mustCompare(api.CompareRequest{Before: api.CompareCurrent, After: "7.3"})
	sp := rowOf(t, j.Result.Compare, "speedometer-chrome")
	// history 21, 22 and this run's 20: spread 2/21, twice it 19.05%
	if sp.NoiseFrom != "last 3 A-side runs" || math.Abs(sp.Noise-19.05) > 0.01 || sp.Verdict != api.VerdictBetter {
		t.Errorf("Speedometer: %+v", sp)
	}
	if fmt.Sprint(l.s.noiseOf(l.name)["speedometer-chrome"]) != "[20 21 22 20]" {
		t.Errorf("history %v", l.s.noiseOf(l.name))
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
