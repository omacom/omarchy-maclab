package server

// Scheduling Performance runs (spec section 22, "When" and "Never
// repeated"): scheduled lab work, never under a Lease. labd works out what
// is missing per Mac, set, metric and Release and claims an idle Mac for
// the newest Release it misses something of: on a Lab-owned Mac, or a
// Volunteer's Mac whose Volunteer turned Performance runs on, inside its
// Availability window, at Access level Full, on AC, not Paused and not
// leaving, with no Lease and no job. The claim holds the Mac until the
// run's cleanup ends. The Quick list while a Release has no verdict, the
// rest of the Full list once it is qualified, so nothing is measured twice;
// a benchmark that failed, or was skipped for a passing reason, runs once
// more at the Mac's next slot, and after that stays as it is.

import (
	"fmt"
	"os"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// perfEvery is how often labd looks for what is missing, besides when a
// Release is frozen, a Mac enrolls, turns Performance runs on or comes
// free, or a verdict is recorded.
var perfEvery = 10 * time.Minute

// perfTriesMax is how many ended attempts measure a benchmark: the first,
// and one more when it failed or was skipped for a passing reason.
const perfTriesMax = 2

// perfClaiming is the claim of a Mac whose run is being queued.
const perfClaiming = "claiming"

// perfKick has the scheduler look again now.
func (s *Server) perfKick() {
	select {
	case s.perfWake <- struct{}{}:
	default:
	}
}

func (s *Server) perfScheduler() {
	t := s.clock().NewTicker(perfEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		case <-s.perfWake:
		}
		s.reconcilePerf()
	}
}

// perfRelease is a Release as the scheduler weighs it; its set is resolved
// once a Mac misses something of it.
type perfRelease struct {
	rel     api.Release
	list    string
	set     *api.PackageSet
	setErr  error
	weighed bool
}

func (pr *perfRelease) packages(s *Server) (*api.PackageSet, error) {
	if !pr.weighed {
		pr.set, pr.setErr = s.releaseSet(pr.rel)
		pr.weighed = true
	}
	return pr.set, pr.setErr
}

// reconcilePerf claims each idle Mac for the newest Release it misses a
// measurement of, and a Mac that misses nothing on Omarchy for its macOS
// run.
func (s *Server) reconcilePerf() {
	var weigh []*perfRelease
	bench, err := perfCurrentSet()
	if err == nil {
		rels, _ := s.store.releases("") // newest first
		for _, rel := range rels {
			state, err := s.releaseState(rel.Name)
			if err != nil {
				continue
			}
			if list := perfList(state); list != "" {
				weigh = append(weigh, &perfRelease{rel: rel, list: list})
			}
		}
	}
	for _, v := range s.allDevs() {
		spec := s.perfWanted(v, bench, weigh)
		if spec == nil && s.macOSSlot(v) {
			spec = s.macOSWanted(v)
		}
		if spec != nil {
			s.claimForPerf(v, *spec)
		}
	}
}

// macOSSlot is whether the Mac could take its macOS run now: the lab
// measures on it and it is idle, so that an Attended Mac's person is told
// only when the Mac would boot macOS for them.
func (s *Server) macOSSlot(v *dev) bool {
	if s.drained() {
		return false
	}
	length := (&jobRun{s: s, v: v, j: &api.Job{Spec: *macOSJobSpec(v.snapshot().Name, "")}, boot: s.cfg.BootTimeout}).expectedLength()
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.perfIdleLocked(length) == ""
}

// perfMacWanted is whether the lab measures on the Mac at all: a Lab-owned
// Mac, or a Volunteer's Mac with Performance runs on, that passed its
// baseline and has not left.
func perfMacWanted(d api.Device) bool {
	if d.State == api.StateLeft || d.KnownGood == "" {
		return false
	}
	return !d.IsVolunteer() || d.Performance
}

// perfWanted is the run the Mac misses first, newest Release first, or nil.
func (s *Server) perfWanted(v *dev, bench *sets.Set, rels []*perfRelease) *api.JobSpec {
	d := v.snapshot()
	if !perfMacWanted(d) {
		return nil
	}
	for _, pr := range rels {
		if releaseSupports(pr.rel, d) != nil {
			continue
		}
		metrics, err := s.perfMissing(d, bench, pr.rel.Name, pr.list)
		if err != nil || len(metrics) == 0 {
			continue
		}
		set, err := pr.packages(s)
		if err != nil {
			s.perfNote(pr.rel.Name, err)
			continue
		}
		if s.perfBootLoader(set, d) != nil {
			continue
		}
		if err := s.benchFilesStored(bench, metrics); err != nil {
			s.perfNote(bench.Name, err)
			continue
		}
		return &api.JobSpec{Device: d.Name, Holder: perfScheduled, Performance: perfSpec(pr.rel, set, bench, pr.list, metrics)}
	}
	return nil
}

// perfNote logs why the lab measures nothing of what, once per reason.
func (s *Server) perfNote(what string, err error) {
	if _, said := s.perfNoted.LoadOrStore(what+": "+err.Error(), true); !said {
		s.log.Printf("Performance runs: %s: %v", what, err)
	}
}

// perfMissing is what the Mac's next run of the Release's list measures, in
// the set's order: every metric with no result yet; with none left, the
// first that failed or was skipped for a passing reason, alone, since a run
// with an error replaces no value. Neither once two attempts asked for it.
func (s *Server) perfMissing(d api.Device, bench *sets.Set, release, list string) ([]string, error) {
	cur, tries, err := s.store.perfTries(d.Name, d.ID, bench.Name, release)
	if err != nil {
		return nil, err
	}
	var fresh, again []string
	for _, id := range omarchyMetrics(bench.List(list)) {
		reason, has := cur[id]
		switch {
		case tries[id] >= perfTriesMax:
		case !has:
			fresh = append(fresh, id)
		case reason != "" && perf.RunAgain(reason):
			again = append(again, id)
		}
	}
	if len(fresh) > 0 {
		return fresh, nil
	}
	if len(again) > 0 {
		return again[:1], nil
	}
	return nil, nil
}

// benchFilesStored is why the metrics cannot run on Omarchy yet: a pinned
// file of theirs not in the artifact store.
func (s *Server) benchFilesStored(bench *sets.Set, metrics []string) error {
	var ms []sets.Metric
	for _, id := range metrics {
		if m, ok := bench.Metric(id); ok {
			ms = append(ms, m)
		}
	}
	for _, f := range bench.FilesFor(sets.Omarchy, ms) {
		if !f.Pinned() {
			continue
		}
		if _, err := os.Stat(s.artifactPath(f.SHA256)); err != nil {
			return fmt.Errorf("the Omarchy file %s (%s) is not in the artifact store: lab bench mirror", f.ID, short(f.SHA256))
		}
	}
	return nil
}

// perfIdleLocked is why the Mac may not be claimed for a run of length now,
// or "". Caller holds v.mu.
func (v *dev) perfIdleLocked(length time.Duration) string {
	d := &v.d
	switch {
	case v.forgotten || d.State == api.StateLeft:
		return "it left the lab"
	case !perfMacWanted(*d):
		return "it takes no Performance runs"
	case !v.aliveLocked() || d.State != api.StateReady:
		return fmt.Sprintf("it is %s", d.State)
	case d.ActiveJob != "":
		return "it runs job " + d.ActiveJob
	case v.liveLeaseLocked() != nil:
		return "it is leased"
	case v.perfClaim != "":
		return "a Performance run claims it"
	case v.termsWait || v.updating != nil || v.resetting || v.proving:
		return "it is busy"
	case d.Health.OnBattery:
		return "it is on battery"
	case !d.Facts.PreflightOK:
		return "it fails preflight"
	case d.IsVolunteer() && (d.Limited || d.AccessLevel != api.AccessFull):
		return "it is not at Access level Full"
	}
	return v.windowHoldLocked(length)
}

// claimForPerf claims the Mac for spec, a Performance run or its macOS
// run, and queues it, if the Mac is idle: no Lease, no job, and as
// perfIdleLocked has it. It reports the job.
func (s *Server) claimForPerf(v *dev, spec api.JobSpec) *api.Job {
	if s.drained() {
		return nil
	}
	s.perfClaimHolds(v)
	length := (&jobRun{s: s, v: v, j: &api.Job{Spec: spec}, boot: s.cfg.BootTimeout}).expectedLength()
	v.mu.Lock()
	why := v.perfIdleLocked(length)
	if why == "" {
		v.perfClaim = perfClaiming
	}
	v.mu.Unlock()
	if why != "" {
		return nil
	}
	unclaim := func() {
		v.mu.Lock()
		if v.perfClaim == perfClaiming {
			v.perfClaim = ""
		}
		v.mu.Unlock()
	}
	if open, err := s.store.jobs(spec.Device, 1, true); err != nil || len(open) > 0 {
		unclaim()
		return nil
	}
	j, what, err := s.submitScheduled(v, spec)
	if err != nil {
		unclaim()
		s.perfNote(spec.Device, err)
		return nil
	}
	v.mu.Lock()
	if v.perfClaim == perfClaiming {
		v.perfClaim = j.ID
	}
	v.mu.Unlock()
	s.log.Printf("%s: claimed for %s: job %s", spec.Device, what, j.ID)
	return j
}

// submitScheduled queues the job of a Mac just claimed: a Performance run,
// or its macOS run. what names it in the log.
func (s *Server) submitScheduled(v *dev, spec api.JobSpec) (j *api.Job, what string, err error) {
	if m := spec.MacOS; m != nil {
		run := s.macOS().run(spec.Device)
		if run == nil || run.ID != m.Run || run.State != api.MacOSQueued {
			return nil, "", fmt.Errorf("macOS run %s is not queued", m.Run)
		}
		j, err := s.submitMacOSRun(v, run)
		return j, "its macOS run " + run.ID, err
	}
	ps := spec.Performance
	j, _, err = s.submit(Person{}, spec, "", fmt.Sprintf("scheduled: Release %s with %s's %s list, %d %s missing on this Mac",
		ps.Release, ps.Bench, ps.List, len(ps.Metrics), plural(len(ps.Metrics), "benchmark", "benchmarks")))
	return j, fmt.Sprintf("a Performance run of Release %s (%s list)", ps.Release, ps.List), err
}

// perfClaimHolds is the run that claims v now, "" when none; a claim whose
// job has ended (canceled before it started) is let go here.
func (s *Server) perfClaimHolds(v *dev) string {
	v.mu.Lock()
	id := v.perfClaim
	v.mu.Unlock()
	if id == "" || id == perfClaiming {
		return id
	}
	if j, err := s.store.job(id); err == nil && j.State != api.JobDone {
		return id
	}
	v.mu.Lock()
	if v.perfClaim == id {
		v.perfClaim = ""
	}
	v.mu.Unlock()
	return ""
}

// claimWords names a claim in a refusal.
func claimWords(claim string) string {
	if claim == perfClaiming {
		return "being queued"
	}
	return "job " + claim
}

// performanceChangedLocked is the Volunteer turning Performance runs on or
// off at the Mac: on, the lab looks for what is missing; off, a run that
// claims the Mac is canceled. Caller holds v.mu.
func (v *dev) performanceChangedLocked() {
	if id := v.perfClaim; !v.d.Performance && id != "" && id != perfClaiming {
		go v.s.cancelJob(id, "its Volunteer turned Performance runs off")
	}
	v.s.perfKick()
}
