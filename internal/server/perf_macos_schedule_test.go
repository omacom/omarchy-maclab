package server

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// runnerInMacOS has the Mac's macOS runner, at each boot into macOS, take
// the run its boot is handed and upload values for it. seen, when set, is
// called first, while the Mac is in macOS. It answers how many boots went
// into macOS.
func (l *lab) runnerInMacOS(secret string, values map[string]float64, seen func()) *atomic.Int32 {
	t := l.t
	var booted atomic.Int32
	l.sim.MacOSFor = 2 * time.Second
	l.sim.InMacOS = func() {
		n := booted.Add(1)
		if seen != nil {
			seen()
		}
		got := pollAs(t, l, secret, bootHash(100+int(n)), l.s.now(), "")
		if got == nil {
			t.Error("the lab's boot was handed no run")
			return
		}
		path := fmt.Sprintf("/api/macos/runner/performance/%d/results", got.PerfRun)
		if r, body := l.as(secret, "POST", path, macOSUpload(t, perf.Succeeded, values)); r.StatusCode != 200 {
			t.Errorf("results: %d %s", r.StatusCode, body)
		}
	}
	return &booted
}

// claimedInMacOS checks, while the Mac is in macOS, that its macOS run's
// job still claims it: no Lease is taken.
func (l *lab) claimedInMacOS() func() {
	return func() {
		v := l.s.dev(l.name)
		v.mu.Lock()
		claim, job := v.perfClaim, v.d.ActiveJob
		v.mu.Unlock()
		if claim == "" || claim != job {
			l.t.Errorf("in macOS the Mac is claimed by %q, its job is %q", claim, job)
		}
		if r, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/lease", api.LeaseRequest{Holder: "tester"}); r.StatusCode != 409 || !strings.Contains(body, "Performance run") {
			l.t.Errorf("a Lease while the Mac is in macOS: %s %s", r.Status, body)
		}
	}
}

// An Unattended Mac's macOS run takes its next slot after its Omarchy side:
// the scheduler claims the Mac for it once the Release is measured, boots
// macOS once, and the results land as the Mac's macOS result. The claim
// holds through the macOS boot and ends with the job, the Mac ready again
// on its Known-good kernel.
func TestAnUnattendedMacsMacOSRunTakesTheSlotAfterItsOmarchySide(t *testing.T) {
	l, a := perfLab(t, labConfig{}, nil)
	macOSResultSet(t, l)
	free := l.holdFromScheduler()
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	secret := enrollRunner(t, l)
	if resp, body := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 200 {
		t.Fatalf("attendance: %d %s", resp.StatusCode, body)
	}
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	run := l.s.macOS().run(l.name)
	if run == nil || run.State != api.MacOSQueued {
		t.Fatalf("queued: %+v", run)
	}
	booted := l.runnerInMacOS(secret, map[string]float64{"coremark-single": 30000, "stream-triad": 100}, l.claimedInMacOS())
	free()

	om := l.nthPerfJob(1)
	l.expect(om, api.OutcomePass)
	j := l.nthMacOSJob(1)
	l.expect(j, api.OutcomePass)
	if !j.Created.After(om.Created) || j.Created.Before(om.Updated) {
		t.Errorf("the macOS run's job (%s) before the Omarchy side ended (%s)", j.Created, om.Updated)
	}
	if j.Spec.Holder != macosScheduled || j.Spec.MacOS.Run != run.ID || j.UnderLease || j.AccountID != 0 {
		t.Errorf("the scheduled macOS job: %+v", j.Spec)
	}
	if booted.Load() != 1 || len(a.runs()) != 1 {
		t.Errorf("macOS booted %d times, bench ran %d times", booted.Load(), len(a.runs()))
	}
	if r := l.s.macOS().run(l.name); r.State != api.MacOSDone || r.Job != j.ID {
		t.Errorf("run: %+v", r)
	}
	if kept, _ := l.s.store.perfCurrentMacOS(l.name, l.s.dev(l.name).snapshot().ID, testSet, testMacOSBuild); len(kept) != 2 {
		t.Errorf("the macOS result: %+v", kept)
	}
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.ActiveJob == "" })
	if d.Kernel != agent.SimKnownGood || l.s.perfClaimHolds(l.s.dev(l.name)) != "" {
		t.Errorf("after the macOS run: on %s, claimed %q", d.Kernel, l.s.perfClaimHolds(l.s.dev(l.name)))
	}
	l.noMorePerf(1)
	if js := l.macOSJobs(); len(js) != 1 {
		t.Errorf("%d macOS runs, want 1", len(js))
	}
}

// An Attended Mac's run is due once the Mac misses nothing on Omarchy: the
// Lab group is told, and the scheduler boots nothing until a person says
// they are at the Mac (`lab macos at-mac`); then it claims the Mac for the
// run and boots macOS.
func TestTheSchedulerWaitsForAPersonAtAnAttendedMac(t *testing.T) {
	l, _ := perfLab(t, labConfig{}, nil)
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)
	booted := l.runnerInMacOS(secret, map[string]float64{"coremark-single": 30000}, l.claimedInMacOS())
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	deadline := time.Now().Add(10 * time.Second)
	for r := l.s.macOS().run(l.name); r == nil || r.Told == nil || len(macOSDueLines(t, l)) == 0; r = l.s.macOS().run(l.name) {
		if time.Now().After(deadline) {
			t.Fatalf("an Attended Mac's run due, nobody told: %+v", r)
		}
		l.s.perfKick()
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		l.s.reconcilePerf()
		time.Sleep(50 * time.Millisecond)
	}
	if js := l.macOSJobs(); len(js) != 0 || booted.Load() != 0 {
		t.Fatalf("with nobody at the Mac: %d jobs, %d boots", len(js), booted.Load())
	}
	if lines := macOSDueLines(t, l); len(lines) != 1 {
		t.Errorf("the Lab group was told %q", lines)
	}

	resp, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos/at-mac", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("at the Mac: %d %s", resp.StatusCode, body)
	}
	j := l.nthMacOSJob(1)
	l.expect(j, api.OutcomePass)
	if booted.Load() != 1 || j.Spec.Holder != macosScheduled {
		t.Errorf("macOS booted %d times, by %q", booted.Load(), j.Spec.Holder)
	}
	if kept, _ := l.s.store.perfCurrentMacOS(l.name, l.s.dev(l.name).snapshot().ID, testSet, testMacOSBuild); len(kept) != 1 {
		t.Errorf("the macOS result: %+v", kept)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.ActiveJob == "" })
	if js := l.macOSJobs(); len(js) != 1 {
		t.Errorf("%d macOS runs, want 1", len(js))
	}
}

// A Volunteer's Mac with Performance runs off gets neither side, and its
// Volunteer is not asked to come to it; turned on, it gets both, the
// Omarchy side first.
func TestAVolunteersMacWithPerformanceOffGetsNeitherSide(t *testing.T) {
	l, _ := perfLab(t, labConfig{volunteer: true}, func(l *lab) {
		l.mustVolunteer("window", "add", "always")
	})
	l.mustVolunteer("access", "full")
	macOSResultSet(t, l)
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	secret := enrollRunner(t, l)
	if resp, body := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 200 {
		t.Fatalf("attendance: %d %s", resp.StatusCode, body)
	}
	booted := l.runnerInMacOS(secret, map[string]float64{"coremark-single": 30000}, nil)
	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	l.noMorePerf(0)
	if js := l.macOSJobs(); len(js) != 0 || booted.Load() != 0 {
		t.Fatalf("performance off: %d macOS jobs, %d boots", len(js), booted.Load())
	}

	l.mustVolunteer("performance", "on")
	om := l.nthPerfJob(1)
	l.expect(om, api.OutcomePass)
	j := l.nthMacOSJob(1)
	l.expect(j, api.OutcomePass)
	if !j.Created.After(om.Created) || booted.Load() != 1 {
		t.Errorf("the macOS run after the Omarchy side: %s then %s, %d boots", om.Created, j.Created, booted.Load())
	}
}
