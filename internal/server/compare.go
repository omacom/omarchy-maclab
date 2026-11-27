package server

// A Compare (spec "Package sets and Compare"): on one Mac, under a Lease the
// job holds, side A (the Mac's known-good system, or a Package set) and side
// B (a Package set), each set through a Candidate boot, run the same list of
// the job's Benchmark set, omarchy-m-test unattended and a read of the Mac's
// facts; with --macos the Mac's macOS result of that set is beside them,
// measured for the job when none is kept; with --hands the job waits on B
// for a person to press Done. The report is the job's, private to
// Maintainers until one publishes it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// compareHandsLimit is how long a Compare waits for a person to press Done.
var compareHandsLimit = time.Hour

// compareConfirmGrace is how long past the boot timeout a candidate boot
// has to be confirmed: its desktop session starts after it reaches labd.
var compareConfirmGrace = 2 * time.Minute

// compareCheckEvery is how often a candidate boot is checked until it is
// confirmed.
var compareCheckEvery = 5 * time.Second

const (
	compareKnownGood = "known-good"
	compareHolder    = "compare"
)

func (s *Server) compareRoutes() []route {
	return []route{
		{"POST /api/compares", maintainers, s.hCompare},
		{"POST /api/compares/{id}/done", maintainers, s.hCompareDone},
		{"POST /api/compares/{id}/publish", maintainers, s.hComparePublish},
	}
}

// compareBenchSet is the Benchmark set a Compare runs, read when it is
// asked: the current set, or the newest while no set is current, as the
// bench builtins take it.
var compareBenchSet = bench.Set

// compareList is the tier's list of the job's Benchmark set.
func compareList(tier string) string {
	if tier == api.TierExtended {
		return sets.Full
	}
	return sets.Quick
}

// compareTests are what each side runs, in order, named for the side: the
// bench builtin runs the metrics of the job's set's list that run on
// Omarchy.
func compareTests(side string, cs *api.CompareSpec, volunteer bool) []api.TestSpec {
	p := strings.ToLower(side) + "-"
	list := compareList(cs.Tier)
	bench := api.TestSpec{Name: p + "bench", Builtin: "bench", TimeoutSec: 30 * 60}
	if list == sets.Full {
		bench = api.TestSpec{Name: p + "bench", Builtin: "bench-extended", TimeoutSec: 75 * 60}
	}
	if set, err := perfSet(cs.Bench); err == nil {
		run := &api.BenchRun{Set: set.Name, List: list}
		for _, m := range set.List(list) {
			if m.Omarchy != nil {
				run.Metrics = append(run.Metrics, m.ID)
			}
		}
		bench.Bench = run
	}
	omt := omtSpec()
	omt.Name, omt.SkipOnScreen = p+omtTest, volunteer
	return []api.TestSpec{{Name: p + "facts", Builtin: "compare-facts", TimeoutSec: 120}, bench, omt}
}

// hCompare queues a Compare. Its sets are read now and kept in the job, as
// they were; the caller's Lease on the Mac is taken for them when they hold
// none, and released when the job ends.
func (s *Server) hCompare(w http.ResponseWriter, r *http.Request) {
	var req api.CompareRequest
	if !decodeBody(w, r, 1<<16, &req) {
		return
	}
	j, err := s.queueCompare(leaser(r), req)
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, j)
}

func (s *Server) queueCompare(p Person, req api.CompareRequest) (*api.Job, error) {
	if !p.isMaintainer() {
		return nil, refuse(403, "a Compare is for Maintainers")
	}
	v := s.dev(req.Mac)
	if v == nil {
		return nil, refuse(404, "no Mac %q", req.Mac)
	}
	cs := api.CompareSpec{Before: cmpOr(req.Before, api.CompareCurrent), After: req.After, MacOS: req.MacOS, Tier: cmpOr(req.Tier, api.TierDefault), Hands: req.Hands}
	if cs.Tier != api.TierDefault && cs.Tier != api.TierExtended {
		return nil, refuse(400, "tier %q: want default or extended", cs.Tier)
	}
	if cs.After == "" || cs.After == api.CompareCurrent {
		return nil, refuse(400, "--after names a Package set (lab set list): the side the Mac boots as a Candidate boot")
	}
	var err error
	if cs.Before != api.CompareCurrent {
		if cs.BeforeSet, err = s.compareSet(cs.Before); err != nil {
			return nil, err
		}
	}
	if cs.AfterSet, err = s.compareSet(cs.After); err != nil {
		return nil, err
	}
	set, err := compareBenchSet()
	if err != nil {
		return nil, refuse(409, "no Benchmark set to run: %v", err)
	}
	cs.Bench = set.Name
	return s.submitCompare(p, v, req.Mac, cs, "")
}

// retryCompare queues old's Compare again, as p: the same Mac, the Package
// sets and Benchmark set as old kept them (a set never changes), the same
// tier and flags.
func (s *Server) retryCompare(p Person, old *api.Job) (*api.Job, error) {
	if !p.isMaintainer() {
		return nil, refuse(403, "a Compare is for Maintainers")
	}
	v := s.dev(old.Spec.Device)
	if v == nil {
		return nil, refuse(404, "no Mac %q", old.Spec.Device)
	}
	cs := *old.Spec.Compare
	cs.LeaseTaken = false
	if cs.Bench == "" {
		set, err := compareBenchSet()
		if err != nil {
			return nil, refuse(409, "no Benchmark set to run: %v", err)
		}
		cs.Bench = set.Name
	}
	return s.submitCompare(p, v, old.Spec.Device, cs, old.ID)
}

// submitCompare queues the Compare cs on Mac v under p's Lease, taken for
// the job when p holds none.
func (s *Server) submitCompare(p Person, v *dev, mac string, cs api.CompareSpec, retryOf string) (*api.Job, error) {
	if cs.MacOS && !s.macOS().enrolled(mac, v.snapshot().ID) {
		return nil, refuse(409, "%s's macOS runner is not enrolled: enroll it first (lab macos enroll %s), or leave out --macos", mac, mac)
	}
	if _, sighted := s.cables.of(mac); !sighted {
		for _, set := range []*api.PackageSet{cs.BeforeSet, cs.AfterSet} {
			if set == nil {
				continue
			}
			if ch := api.BootLoaderChanges(set.Items, v.snapshot().Facts.Stack); len(ch) > 0 {
				return nil, refuse(409, "set %s changes the boot loader (%s), and %s has no Sighting: a boot loader is swapped only on a Mac cabled to a controller, never over Wi-Fi alone. Prove its port first (lab reset %s --prove <port>)",
					set.Name, strings.Join(ch, ", "), mac, mac)
			}
		}
	}
	if !s.holdsLease(v, p) {
		if _, err := s.takeLease(v, p, compareHolder, leaseDefault); err != nil {
			return nil, err
		}
		cs.LeaseTaken = true
	}
	j, code, err := s.submit(p, api.JobSpec{Device: mac, Holder: compareHolder, Compare: &cs}, retryOf,
		fmt.Sprintf("a Compare of %s (A) and %s (B), %s tier", cs.Before, cs.After, cs.Tier))
	if err != nil {
		if cs.LeaseTaken {
			s.releaseLease(v, p, compareHolder, false, "")
		}
		return nil, refuse(code, "%v", err)
	}
	return j, nil
}

func cmpOr(s, or string) string {
	if s == "" {
		return or
	}
	return s
}

// compareSet is the set as a Candidate boot can install it: every item a
// package file. A kernel build is a lab kernel, not a package.
func (s *Server) compareSet(name string) (*api.PackageSet, error) {
	set, err := s.packageSet(name)
	if err != nil {
		return nil, err
	}
	for _, it := range set.Items {
		if it.Source == api.SetBuild && !strings.Contains(it.File, ".pkg.tar") {
			return nil, refuse(400, "set %s: %s is a kernel build (%s), which a Candidate boot cannot install with pacman: build the kernel's package with lab package and make a set of that", name, it.Package, it.Ref)
		}
	}
	return set, nil
}

// ---- the job ----

// compareSideRun is one side as it ran, and how the job ends if it ends there.
type compareSideRun struct {
	side             api.CompareSide
	outcome, summary string
}

func (r *jobRun) saveCompare(res *api.CompareResult) {
	b, _ := json.Marshal(res)
	r.s.updateJob(r.j, func(j *api.Job) {
		var c api.CompareResult
		json.Unmarshal(b, &c)
		j.Result.Compare = &c
	})
}

func (r *jobRun) compare(ctx context.Context) (string, string) {
	cs := r.j.Spec.Compare
	res := &api.CompareResult{Before: cs.Before, After: cs.After, Tier: cs.Tier, Bench: cs.Bench}
	if cs.Hands {
		res.Hands = &api.CompareHands{}
	}
	r.saveCompare(res)
	defer r.saveCompare(res)

	a := r.compareSide(ctx, api.SideA, cs.Before, cs.BeforeSet, nil)
	res.Sides = append(res.Sides, a.side)
	r.saveCompare(res)
	if a.outcome != "" {
		r.compareReport(res)
		return a.outcome, "side A (" + cs.Before + "): " + a.summary
	}
	b := r.compareSide(ctx, api.SideB, cs.After, cs.AfterSet, res)
	res.Sides = append(res.Sides, b.side)
	r.saveCompare(res)
	if b.outcome != "" && b.outcome != api.OutcomeBootFailed {
		r.compareReport(res)
		return b.outcome, "side B (" + cs.After + "): " + b.summary
	}
	if cs.MacOS && ctx.Err() == nil {
		m := r.compareMacOS(ctx)
		res.Sides = append(res.Sides, m)
		r.saveCompare(res)
	}
	r.compareReport(res)
	if b.outcome != "" {
		return b.outcome, "side B (" + cs.After + "): " + b.summary
	}
	better, worse := 0, 0
	for _, row := range res.Rows {
		switch row.Verdict {
		case api.VerdictBetter:
			better++
		case api.VerdictWorse:
			worse++
		}
	}
	sum := fmt.Sprintf("compared %s (A) and %s (B): %d benchmarks better, %d worse; omarchy-m-test %d regressed, %d fixed", cs.Before, cs.After, better, worse, res.Regressed, res.Fixed)
	if m := res.Side(api.SideMacOS); m != nil && m.Boot == macOSKept {
		sum += "; macOS: the Mac's kept macOS result"
	} else if m != nil && m.Boot != api.MacOSDone {
		sum += "; macOS: " + m.Error
	}
	return api.OutcomePass, sum
}

// compareSide runs one side: on the known-good system as it is (set nil),
// or through a Candidate boot of the set. hands, on side B with --hands, is
// the report the pause for a person is recorded in.
func (r *jobRun) compareSide(ctx context.Context, label, name string, set *api.PackageSet, hands *api.CompareResult) compareSideRun {
	v := r.v
	run := compareSideRun{side: api.CompareSide{Side: label, Set: name}}
	side := &run.side
	fail := func(outcome, why string) compareSideRun {
		side.Error = why
		run.outcome, run.summary = outcome, why
		return run
	}
	if set == nil {
		d := v.snapshot()
		if d.Health.JobTag != "" {
			if err := r.restore(ctx); err != nil {
				return fail(api.OutcomeInfra, "could not go back to the known-good system: "+err.Error())
			}
			d = v.snapshot()
		}
		side.Boot, side.Kernel = compareKnownGood, d.Kernel
		r.ev("side %s: %s on the known-good system (%s)", label, name, d.Kernel)
		r.bootID, r.bootedAt = d.BootID, r.s.now()
		if o, why := r.compareRunTests(ctx, label, side); o != "" {
			return fail(o, why)
		}
		return run
	}
	tests := compareTests(label, r.j.Spec.Compare, v.snapshot().IsVolunteer())
	return r.candidateSide(ctx, label, name, set, tests, func(side *api.CompareSide) (string, string) {
		if o, why := r.compareRunTests(ctx, label, side); o != "" {
			return o, why
		}
		if hands != nil && hands.Hands != nil {
			r.waitHands(ctx, hands)
		}
		return "", ""
	})
}

// candidateSide runs set as a Candidate boot (spec "Candidate boot"):
// candidate_stage, boot_once with tests armed for the Lab boot, and
// candidate_check until the boot is confirmed or its deadline passes. On a
// confirmed boot onBoot runs, saying how the side ends if it ends there.
// Then back on the known-good system, candidate_result when the boot was not
// confirmed, and candidate_cleanup always.
func (r *jobRun) candidateSide(ctx context.Context, label, name string, set *api.PackageSet, tests []api.TestSpec, onBoot func(side *api.CompareSide) (string, string)) compareSideRun {
	v, j := r.v, r.j
	run := compareSideRun{side: api.CompareSide{Side: label, Set: name}}
	side := &run.side
	fail := func(outcome, why string) compareSideRun {
		side.Error = why
		run.outcome, run.summary = outcome, why
		return run
	}
	r.state(api.JobStaging)
	pkgs, err := r.resolveSet(ctx, set)
	if err != nil {
		return fail(api.OutcomeStageFailed, err.Error())
	}
	side.Packages = pkgs
	_, sighted := r.s.cables.of(v.snapshot().Name)
	args := api.CandidateStageArgs{Set: api.CandidateSet{Name: set.Name, Packages: pkgs}, Sighting: sighted,
		BootTimeoutSec: int(r.boot / time.Second), WatchdogOpenTimeoutSec: int(r.s.cfg.WatchdogOpenTimeout / time.Second),
		Offline: r.j.Spec.Performance != nil}
	r.ev("side %s: staging set %s (%d packages) as a Candidate boot", label, set.Name, len(pkgs))
	var sr api.CandidateStageResult
	v.mu.Lock()
	v.dirty = true
	v.mu.Unlock()
	cleanup := func() {
		if err := v.call(context.WithoutCancel(ctx), api.CmdCandidateCleanup, j.ID, api.CandidateCleanupArgs{}, 5*time.Minute, nil); err != nil {
			r.ev("side %s: removing the candidate: %v", label, err)
		}
	}
	if err := v.call(ctx, api.CmdCandidateStage, j.ID, args, r.s.cfg.StageTimeout+15*time.Minute, &sr); err != nil {
		cleanup()
		if errors.Is(err, errWindowClosed) {
			return fail(windowCut("while staging set " + set.Name))
		}
		return fail(api.OutcomeStageFailed, "staging set "+set.Name+": "+strings.TrimPrefix(err.Error(), api.CmdCandidateStage+": "))
	}
	r.ev("side %s: staged as boot entry %s (kernel %s)", label, sr.Entry, sr.KernelRelease)

	r.state(api.JobBooting)
	oldBoot := v.snapshot().BootID
	t0 := r.s.now()
	r.armedBoot, r.armedAt = oldBoot, t0
	arm := api.BootOnceArgs{Entry: sr.Entry, BootTimeoutSec: int(r.boot / time.Second), JobSec: int(r.labBootJob() / time.Second), Tests: tests}
	armTimeout := 2 * time.Minute
	if d := v.snapshot(); d.IsVolunteer() && r.s.cfg.RebootWarning > 0 {
		arm.RebootWarningSec = int(r.s.cfg.RebootWarning / time.Second)
		armTimeout += r.s.cfg.RebootWarning
	}
	if err := v.call(ctx, api.CmdBootOnce, j.ID, arm, armTimeout, nil); err != nil {
		if ctx.Err() == nil {
			r.armedBoot = ""
		}
		cleanup()
		switch {
		case errors.Is(err, errWindowClosed):
			return fail(windowCut("before the Candidate boot (" + err.Error() + ")"))
		case errors.Is(err, errAccessLevel):
			return fail(accessCut("before the Candidate boot (" + err.Error() + ")"))
		}
		return fail(api.OutcomeInfra, "arming the Candidate boot failed: "+err.Error())
	}
	r.ev("side %s: one-shot armed, rebooting into the candidate", label)
	steps, ok, _, err := v.waitBack(ctx, oldBoot, t0, t0.Add(r.s.cfg.RebootGrace), r.boot, r.selfRecovery())
	r.addRecovery(steps)
	if err != nil {
		return fail(api.OutcomeCanceled, "canceled during the Candidate boot")
	}
	d := v.snapshot()
	confirmed := false
	if ok && d.Health.JobTag == j.ID {
		r.bootID, r.bootedAt = d.BootID, r.s.now()
		side.Kernel, side.BootSeconds = d.Kernel, r.s.since(t0).Seconds()
		r.ev("side %s: up on %s in %.0fs; checking the candidate", label, d.Kernel, side.BootSeconds)
		deadline := t0.Add(r.boot + compareConfirmGrace)
		for {
			var chk api.CandidateCheck
			err := v.callNoteOn(ctx, r.bootID, api.CmdCandidateCheck, j.ID, api.CandidateCheckArgs{}, time.Minute, &chk, nil)
			if err == nil {
				side.FailedUnits = chk.FailedUnits
				if chk.State == api.CandidateConfirmed {
					confirmed = true
					break
				}
			}
			if ctx.Err() != nil || errors.Is(err, errDeviceLost) || !r.s.now().Before(deadline) {
				why := strings.Join(chk.Problems, "; ")
				if err != nil {
					why = err.Error()
				}
				r.ev("side %s: not confirmed: %s", label, cmpOr(why, "the deadline passed"))
				break
			}
			select {
			case <-ctx.Done():
			case <-r.s.clock().After(compareCheckEvery):
			}
		}
	} else if !ok && sr.BootLoader {
		v.setState(api.StateNeedsHands, "the Candidate boot of set "+set.Name+" changed the boot loader and did not come back: "+sr.RestoreSteps)
		return fail(api.OutcomeBootFailed, "the Candidate boot changed the boot loader and did not come back; the Mac Needs hands: "+sr.RestoreSteps)
	}

	if confirmed {
		side.Boot = api.CandidateConfirmed
		r.ev("side %s: %s", label, api.CandidateConfirmed)
		if o, why := onBoot(side); o != "" {
			if v.snapshot().State != api.StateNeedsHands {
				r.restore(context.WithoutCancel(ctx))
			}
			cleanup()
			return fail(o, why)
		}
	} else {
		side.Boot = api.CandidateBootFailed
	}
	if err := r.restore(ctx); err != nil {
		cleanup()
		return fail(api.OutcomeInfra, "back to the known-good system: "+err.Error())
	}
	var f api.CandidateFailure
	if !confirmed {
		name := "candidate-" + strings.ToLower(label)
		if err := v.call(ctx, api.CmdCandidateResult, j.ID, api.CandidateResultArgs{Name: name}, 2*time.Minute, &f); err != nil {
			r.ev("side %s: reading the candidate's journal: %v", label, err)
		} else {
			side.Journal = f.Journal
			if len(f.Files) > 0 {
				r.ev("side %s: the candidate's journal is kept in %s", label, strings.Join(f.Files, ", "))
			}
		}
	}
	cleanup()
	switch {
	case confirmed:
		return run
	case f.NotBooted:
		side.Boot = api.CandidateNotBooted
		return fail(api.OutcomeBootFailed, "set "+set.Name+": the candidate entry was not booted: the Mac came back on its known-good system "+
			"and the candidate never ran (its journal is empty and it noted no Lab boot)")
	case f.Deadline != "":
		return fail(api.OutcomeBootFailed, "set "+set.Name+" did not boot to a confirmed candidate (desktop up, no failed units): "+
			"the candidate's early unit rebooted it at its "+f.Deadline.String()+" deadline: "+api.CandidateBootFailed)
	}
	return fail(api.OutcomeBootFailed, "set "+set.Name+" did not boot to a confirmed candidate (desktop up, no failed units) in time: "+api.CandidateBootFailed)
}

// resolveSet is every package of the set as a file with its sha256: a
// file's and a build's from labd's artifact store, a repo item's found on
// the Mac's known-good system at its exact version (candidate_resolve).
func (r *jobRun) resolveSet(ctx context.Context, set *api.PackageSet) ([]api.CandidatePackage, error) {
	var out, repo []api.CandidatePackage
	for _, it := range set.Items {
		p := api.CandidatePackage{Name: it.Package, Version: it.Version, SHA256: it.SHA256}
		if it.Source == api.SetRepo {
			repo = append(repo, p)
		}
		out = append(out, p)
	}
	if len(repo) > 0 {
		var got api.CandidateResolved
		if err := r.v.call(ctx, api.CmdCandidateResolve, r.j.ID, api.CandidateResolveArgs{Packages: repo}, 15*time.Minute, &got); err != nil {
			return nil, fmt.Errorf("finding set %s's repo packages on the Mac: %s", set.Name, strings.TrimPrefix(err.Error(), api.CmdCandidateResolve+": "))
		}
		for i := range out {
			for _, g := range got.Packages {
				if g.Name == out[i].Name && g.Version == out[i].Version && out[i].SHA256 == "" {
					out[i] = g
				}
			}
		}
	}
	for _, p := range out {
		if !reSHA.MatchString(p.SHA256) {
			return nil, fmt.Errorf("set %s: %s %s was not found as a file", set.Name, p.Name, p.Version)
		}
	}
	return out, nil
}

// compareRunTests runs a side's tests on r.bootID and reads what they left.
func (r *jobRun) compareRunTests(ctx context.Context, label string, side *api.CompareSide) (string, string) {
	v, j := r.v, r.j
	r.state(api.JobTesting)
	for _, t := range compareTests(label, j.Spec.Compare, v.snapshot().IsVolunteer()) {
		r.ev("test %s: running", t.Name)
		var tr api.TestResult
		r.testStarted, r.testLimit = r.s.now(), r.testTimeout(t)
		err := v.callNoteOn(ctx, r.bootID, api.CmdRunTest, j.ID, t, r.testTimeout(t), &tr, func(n string) { r.ev("%s", n) })
		switch {
		case errors.Is(err, errDeviceLost):
			return r.lostDuringTest(ctx, t.Name)
		case errors.Is(err, errWindowClosed):
			return windowCut("before test " + t.Name + " (" + err.Error() + ")")
		case errors.Is(err, errAccessLevel):
			return accessCut("during test " + t.Name + " (" + err.Error() + ")")
		case err != nil && ctx.Err() != nil:
			return api.OutcomeCanceled, "canceled during test " + t.Name
		case err != nil:
			tr = api.TestResult{Name: t.Name, ExitCode: -1, Error: err.Error()}
		}
		r.s.updateJob(j, func(j *api.Job) { j.Result.Tests = append(j.Result.Tests, tr) })
		r.ev("test %s: %s", t.Name, testVerdict(tr))
		dir := filepath.Join(r.s.jobDir(j.ID), "test-"+t.Name)
		switch t.Builtin {
		case "compare-facts":
			readFacts(filepath.Join(dir, "facts.json"), side)
		case "bench", "bench-extended":
			side.Bench = readBench(filepath.Join(dir, "bench.json"))
			if len(side.Bench) == 0 && tr.Error != "" {
				side.Error = "benchmarks: " + tr.Error
			}
		case omtTest:
			side.OMT = readOMT(filepath.Join(dir, "omt-report.json"))
		}
	}
	return "", ""
}

func readFacts(path string, side *api.CompareSide) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f struct {
		GPU         string   `json:"gpu_firmware"`
		SIO         string   `json:"sio"`
		Thunderbolt string   `json:"thunderbolt"`
		Failed      []string `json:"failed_units"`
	}
	if json.Unmarshal(b, &f) != nil {
		return
	}
	side.GPUFirmware, side.SIO, side.Thunderbolt = f.GPU, f.SIO, f.Thunderbolt
	if len(f.Failed) > 0 || side.FailedUnits == nil {
		side.FailedUnits = f.Failed
	}
}

// readBench reads bench.json, by metric id: an entry with a reason code has
// no value, and its note is the code.
func readBench(path string) []api.BenchResult {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rs []struct {
		Metric         string   `json:"metric"`
		Version        string   `json:"version"`
		Value          *float64 `json:"value"`
		Unit           string   `json:"unit"`
		HigherIsBetter bool     `json:"higher_is_better"`
		Spread         *float64 `json:"spread"`
		Seconds        float64  `json:"seconds"`
		API            *string  `json:"api"`
		Reason         *string  `json:"reason"`
	}
	if json.Unmarshal(b, &rs) != nil {
		return nil
	}
	out := make([]api.BenchResult, 0, len(rs))
	for _, r := range rs {
		res := api.BenchResult{Name: r.Metric, Version: r.Version, Value: r.Value, Unit: r.Unit, HigherIsBetter: r.HigherIsBetter, Seconds: r.Seconds}
		if r.Spread != nil {
			res.Spread = *r.Spread
		}
		if r.API != nil {
			res.API = *r.API
		}
		if r.Reason != nil {
			res.Value, res.Note = nil, *r.Reason
		}
		out = append(out, res)
	}
	return out
}

func readOMT(path string) *api.CompareOMT {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rep omtReport
	if json.Unmarshal(b, &rep) != nil {
		return nil
	}
	o := &api.CompareOMT{Tool: rep.Tool.Version, Status: map[string]string{}}
	for _, c := range rep.Checks {
		o.Status[c.ID] = c.Status
		switch c.Status {
		case "pass":
			o.Pass++
		case "fail":
			o.Fail++
		default:
			o.Skip++
		}
	}
	return o
}

// ---- the pause for a person ----

type handsWaits struct {
	mu sync.Mutex
	m  map[string]chan string // job id -> who pressed Done
}

var compareHandsOf sync.Map // *Server -> *handsWaits

func (s *Server) hands() *handsWaits {
	h, _ := compareHandsOf.LoadOrStore(s, &handsWaits{m: map[string]chan string{}})
	return h.(*handsWaits)
}

// waitHands waits, on B's candidate boot, until a person presses Done or
// compareHandsLimit passes. A candidate boot keeps its speakers muted, so
// what is checked by ear is not checked here.
func (r *jobRun) waitHands(ctx context.Context, res *api.CompareResult) {
	h := r.s.hands()
	ch := make(chan string, 1)
	h.mu.Lock()
	h.m[r.j.ID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.m, r.j.ID)
		h.mu.Unlock()
	}()
	res.Hands.Waiting = true
	r.saveCompare(res)
	r.ev("waiting up to %s for a person at %s (attended checks, lid, Touch ID; the candidate's speakers stay muted): press Done on the Mac's page, or run lab compare done %s",
		took(compareHandsLimit), r.j.Spec.Device, r.j.ID)
	select {
	case by := <-ch:
		now := r.s.now()
		res.Hands.Done, res.Hands.By = &now, by
		r.ev("@%s pressed Done", by)
	case <-ctx.Done():
		res.Hands.Note = "canceled while waiting for a person"
	case <-r.s.clock().After(compareHandsLimit):
		res.Hands.Note = "no one pressed Done within " + took(compareHandsLimit)
		r.ev("%s", res.Hands.Note)
	}
	res.Hands.Waiting = false
	r.saveCompare(res)
}

func (s *Server) compareDone(id string, p Person) error {
	h := s.hands()
	h.mu.Lock()
	ch := h.m[id]
	h.mu.Unlock()
	if ch == nil {
		return refuse(409, "job %s is not waiting for a person", id)
	}
	select {
	case ch <- p.Handle:
	default:
	}
	return nil
}

// mayHands is whether p may press Done for the job: a Maintainer, or the
// Volunteer who lends its Mac.
func (s *Server) mayHands(p Person, j *api.Job) bool {
	if p.isMaintainer() {
		return true
	}
	v := s.dev(j.Spec.Device)
	if v == nil || p.AccountID == 0 {
		return false
	}
	d := v.snapshot()
	return d.VolunteerID != 0 && d.VolunteerID == p.AccountID && j.DeviceID == d.ID
}

func (s *Server) hCompareDone(w http.ResponseWriter, r *http.Request) {
	if err := s.compareDone(r.PathValue("id"), leaser(r)); err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, map[string]string{"job": r.PathValue("id"), "hands": "done"})
}

// handsWaiting is the Compare on v waiting for a person now, or "".
func (s *Server) handsWaiting(v *dev) string {
	id := v.snapshot().ActiveJob
	if id == "" {
		return ""
	}
	h := s.hands()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m[id] == nil {
		return ""
	}
	return id
}

// ---- macOS ----

// macOSKept is a Compare's macOS side taken from the Mac's kept macOS
// result, with no boot.
const macOSKept = "kept"

// compareMacOS is the Mac's macOS result of the job's set at its current
// macOS build when one is kept; else it queues an attempt at that result,
// boots macOS once for it and waits for its results, or for its deadline to
// leave the Mac Needs hands. Its values are kept as the Mac's macOS result.
func (r *jobRun) compareMacOS(ctx context.Context) api.CompareSide {
	j := r.j
	side := api.CompareSide{Side: api.SideMacOS, Set: "macOS"}
	name := r.v.snapshot().Name
	bench := j.Spec.Compare.Bench
	if kept, b := r.s.keptMacOS(name, bench); kept != nil {
		side.Boot, side.Bench = macOSKept, kept
		r.ev("macOS: the Mac's macOS result of %s at macOS %s (%s) is kept: macOS is not booted", bench, b.Version, b.Build)
		return side
	}
	run, err := r.s.compareMacOSRun(name, bench)
	if err != nil {
		side.Error = "not queued: " + err.Error()
		return side
	}
	r.ev("macOS: run %s queued, attempt %d at the Mac's macOS result of %s", run.ID, run.PerfRun, bench)
	m, outcome, why := r.bootMacOSFor(ctx, run)
	if m != nil {
		side.Boot = m.State
	}
	if m != nil && m.State == api.MacOSDone {
		side.Error = m.Error
		res, err := r.s.store.perfAttemptResults(m.PerfRun)
		if err != nil {
			side.Error = cmpOr(side.Error, "its results: "+err.Error())
		}
		side.Bench = benchOf(m.Set, res)
	}
	if outcome != "" {
		side.Error = why
	}
	return side
}

// ---- the report ----

// verdict is b against a: the change in percent, and no change when the
// change is within the metric's tolerance, in percent; else better or worse
// by the metric's direction.
func verdict(a, b float64, higherIsBetter bool, tolerance float64) (float64, string) {
	if a == 0 {
		return 0, api.VerdictNotCompared
	}
	d := (b - a) / math.Abs(a) * 100
	switch {
	case math.Abs(d) <= tolerance+1e-9:
		return d, api.VerdictNoChange
	case (d > 0) == higherIsBetter:
		return d, api.VerdictBetter
	}
	return d, api.VerdictWorse
}

// compareRows is a row per metric of the set's list, in the set's order:
// each side's value by metric id, B and macOS each against A, judged by the
// metric's tolerance and direction in the set. A metric a side did not run,
// or ran with a reason code, is not compared, with the reason.
func compareRows(set *sets.Set, list string, a, b, m *api.CompareSide) []api.CompareRow {
	byID := func(s *api.CompareSide) map[string]api.BenchResult {
		out := map[string]api.BenchResult{}
		if s != nil {
			for _, x := range s.Bench {
				out[x.Name] = x
			}
		}
		return out
	}
	am, bm, mm := byID(a), byID(b), byID(m)
	var rows []api.CompareRow
	for _, mt := range set.List(list) {
		row := api.CompareRow{Name: mt.ID, Unit: mt.Unit, HigherIsBetter: mt.HigherIsBetter(), Tolerance: mt.Tolerance, Verdict: api.VerdictNotCompared}
		ax, aok := am[mt.ID]
		bx, bok := bm[mt.ID]
		row.A, row.B, row.VersionA, row.VersionB = ax.Value, bx.Value, ax.Version, bx.Version
		if mt.Omarchy == nil {
			row.Note = "macOS only"
		} else {
			var notes []string
			for _, s := range []struct {
				side string
				x    api.BenchResult
				ran  bool
			}{{api.SideA, ax, aok}, {api.SideB, bx, bok}} {
				switch {
				case !s.ran:
					notes = append(notes, s.side+" did not run it")
				case s.x.Value == nil:
					notes = append(notes, s.side+": "+cmpOr(s.x.Note, "no value"))
				}
			}
			if len(notes) == 0 {
				d, v := verdict(*row.A, *row.B, row.HigherIsBetter, row.Tolerance)
				if row.Verdict = v; v == api.VerdictNotCompared {
					notes = append(notes, "A is 0")
				} else {
					row.Delta = &d
				}
			}
			row.Note = strings.Join(notes, "; ")
		}
		if m != nil && mt.MacOS != nil {
			mx, ok := mm[mt.ID]
			row.MacOS, row.MacOSVerdict = mx.Value, api.VerdictNotCompared
			var notes []string
			if mt.APIDiffers {
				notes = append(notes, "API differs: "+apiName(mt.Omarchy)+" on Omarchy, "+apiName(mt.MacOS)+" on macOS")
			}
			switch {
			case !ok:
				notes = append(notes, "macOS did not run it")
			case mx.Value == nil:
				notes = append(notes, cmpOr(mx.Note, "no value"))
			case row.A == nil:
				notes = append(notes, "A has no value")
			default:
				if d, v := verdict(*row.A, *mx.Value, row.HigherIsBetter, row.Tolerance); v != api.VerdictNotCompared {
					row.MacOSDelta, row.MacOSVerdict = &d, v
				}
			}
			row.MacOSNote = strings.Join(notes, "; ")
		}
		rows = append(rows, row)
	}
	return rows
}

func apiName(s *sets.Side) string {
	if s == nil || s.API == "" {
		return "-"
	}
	return s.API
}

// compareReport fills in the rows, by the job's Benchmark set, and the
// checks from the sides.
func (r *jobRun) compareReport(res *api.CompareResult) {
	a, b, m := res.Side(api.SideA), res.Side(api.SideB), res.Side(api.SideMacOS)
	if a == nil {
		return
	}
	res.Rows = nil
	cs := r.j.Spec.Compare
	if set, err := perfSet(cs.Bench); err == nil {
		res.Rows = compareRows(set, compareList(cs.Tier), a, b, m)
	}
	res.Checks, res.Regressed, res.Fixed = nil, 0, 0
	if b == nil || a.OMT == nil || b.OMT == nil {
		return
	}
	var ids []string
	for id := range a.OMT.Status {
		ids = append(ids, id)
	}
	for id := range b.OMT.Status {
		if _, ok := a.OMT.Status[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	candidate := a.Boot == api.CandidateConfirmed || b.Boot == api.CandidateConfirmed
	for _, id := range ids {
		c := api.CompareCheck{ID: id, A: cmpOr(a.OMT.Status[id], "-"), B: cmpOr(b.OMT.Status[id], "-")}
		why, byLab := candidateFails[id]
		switch {
		case candidate && byLab && c.A != c.B:
			c.Change, c.Note = api.CheckNotCompared, "Candidate boot: "+why
		case c.A == "pass" && c.B == "fail":
			c.Change = api.CheckRegressed
			res.Regressed++
		case c.A == "fail" && c.B == "pass":
			c.Change = api.CheckFixed
			res.Fixed++
		case c.B == "fail":
			c.Change = api.CheckFail
		default:
			c.Change = api.CheckPass
		}
		res.Checks = append(res.Checks, c)
	}
}

// candidateFails are the omarchy-m-test checks a Candidate boot itself
// fails, whatever its set, with why. They are not compared when a side is
// one: the check is right about the root it sees, which is the lab's.
var candidateFails = map[string]string{
	"system.snapshots": "it runs from @lab-candidate, a snapshot of @, in which /.snapshots (a subvolume inside @) is an empty directory",
	"boot.chain":       "it boots its kernel as a one-shot UKI from the ESP, not the /boot/vmlinuz-* the installed boot chain names",
}

// ---- publishing ----

func (s *Server) publishCompare(id string, p Person) (*api.Job, error) {
	j, err := s.store.job(id)
	if err != nil || j.Spec.Compare == nil {
		return nil, refuse(404, "no Compare %s", id)
	}
	if j.State != api.JobDone || j.Result.Compare == nil {
		return nil, refuse(409, "Compare %s is not done yet", id)
	}
	now := s.now()
	s.updateJobByID(id, func(j *api.Job) {
		if j.Result.Compare != nil && j.Result.Compare.Published == nil {
			j.Result.Compare.Published, j.Result.Compare.PublishedBy = &now, p.Handle
		}
	})
	s.log.Printf("Compare %s published by @%s", id, p.Handle)
	return s.store.job(id)
}

func (s *Server) hComparePublish(w http.ResponseWriter, r *http.Request) {
	j, err := s.publishCompare(r.PathValue("id"), leaser(r))
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, j)
}

// comparePrivate is whether j is a Compare nobody has published yet.
func comparePrivate(j *api.Job) bool {
	return j.Spec.Compare != nil && (j.Result.Compare == nil || j.Result.Compare.Published == nil)
}
