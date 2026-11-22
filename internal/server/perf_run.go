package server

// A Performance run (spec section 22, "Performance run"): a job of the Mac
// (spec.performance) that measures one Release with one Benchmark set. Its
// Package set is the Release's frozen manifest, release-<version>, made from
// the manifest by its digest. It boots as a Candidate boot, so the Mac's own
// system never changes, and measures only once the running kernel is the
// Release's. labd records the versions that boot holds, by exact package
// name; lab-agent's bench builtin runs the metrics it is given and uploads
// each result to the run's attempt (perf.go).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// perfCurrentSet is the Benchmark set Performance runs measure with.
var perfCurrentSet = sets.Current

const (
	// perfHolder holds the Lease `lab perf rerun` takes for a caller who
	// holds none, and labels the rerun's job.
	perfHolder = "perf"
	// perfScheduled labels the Performance runs labd queues itself.
	perfScheduled = "maclab-perf"
	perfTest      = "perf-bench"
	perfSide      = "perf"
)

func (s *Server) perfRunRoutes() []route {
	return []route{{"POST /api/perf/rerun", team, s.hPerfRerun}}
}

// perfTests are what a Performance run's Candidate boot runs: the bench
// builtin on the run's metrics, uploading to its attempt.
func perfTests(ps *api.PerformanceSpec, run int64) []api.TestSpec {
	timeout := 30 * 60
	if ps.List == sets.Full {
		timeout = 75 * 60
	}
	return []api.TestSpec{{Name: perfTest, Builtin: "bench", TimeoutSec: timeout,
		Bench: &api.BenchRun{Set: ps.Bench, List: ps.List, Metrics: slices.Clone(ps.Metrics), Run: run}}}
}

// benchFiles are the pinned files of the set the bench builtin fetches for
// the run's metrics on Omarchy.
func benchFiles(b *api.BenchRun) []string {
	set, err := perfSet(b.Set)
	if err != nil {
		return nil
	}
	var ms []sets.Metric
	for _, id := range b.Metrics {
		if m, ok := set.Metric(id); ok {
			ms = append(ms, m)
		}
	}
	var out []string
	for _, f := range set.FilesFor(sets.Omarchy, ms) {
		if f.Pinned() {
			out = append(out, f.SHA256)
		}
	}
	return out
}

// releaseSet is the Release's Package set, release-<version>: every package
// of its frozen manifest, read from the frozen store and checked against
// the digest the Release was listed with, each a file in the artifact
// store. It is kept with the Package sets the first time, and is that set
// after.
func (s *Server) releaseSet(rel api.Release) (*api.PackageSet, error) {
	m, sha, err := frozenManifest(s.frozenDir(rel.Name))
	switch {
	case err != nil:
		return nil, refuse(409, "Release %s: its frozen manifest cannot be read: %v", rel.Name, err)
	case sha != rel.ManifestSHA256:
		return nil, refuse(409, "Release %s: its frozen manifest's digest is %s, not %s, the one it was listed with", rel.Name, short(sha), short(rel.ManifestSHA256))
	case len(m.Artifacts) == 0:
		return nil, refuse(409, "Release %s: its frozen manifest names no package", rel.Name)
	}
	set := &api.PackageSet{Name: api.PerfPackageSet(rel.Name), CreatedBy: rel.FrozenBy, Created: rel.FrozenAt.UTC(),
		Release: rel.Name, ManifestSHA256: sha}
	for _, a := range m.Artifacts {
		sum := strings.ToLower(a.SHA256)
		set.Items = append(set.Items, api.SetItem{Package: a.Name, Source: api.SetFile, Ref: sum, Version: a.Version, SHA256: sum, File: a.File})
	}
	if err := s.setFilesStored(set); err != nil {
		return nil, err
	}
	if _, err := s.store.addSet(set); err != nil {
		return nil, err
	}
	kept, err := s.packageSet(set.Name)
	if err != nil {
		return nil, err
	}
	if kept.ManifestSHA256 != sha {
		return nil, refuse(409, "Package set %s is not Release %s's frozen manifest (%s)", set.Name, rel.Name, short(sha))
	}
	return kept, nil
}

// setFilesStored is why a Release's set cannot be staged: a file of it that
// is not in the artifact store.
func (s *Server) setFilesStored(set *api.PackageSet) error {
	for _, it := range set.Items {
		if _, err := os.Stat(s.artifactPath(it.SHA256)); err != nil {
			return refuse(409, "%s: %s %s (%s) is not in the artifact store: lab upload it", set.Name, it.Package, it.Version, short(it.SHA256))
		}
	}
	return nil
}

// releaseSupports is why the Release does not support the Mac's board, or
// nil: its manifest is for its Macs and their boards.
func releaseSupports(rel api.Release, d api.Device) error {
	board := ""
	if len(d.Facts.Compatible) > 0 {
		board = d.Facts.Compatible[0]
	}
	var boards []string
	for _, m := range rel.Macs {
		if m.Mac == d.Name || board != "" && m.Board == board {
			return nil
		}
		if m.Board != "" && !slices.Contains(boards, m.Board) {
			boards = append(boards, m.Board)
		}
	}
	return refuse(409, "Release %s does not support %s's board (%s): its manifest is for %s", rel.Name, d.Name,
		cmpOr(board, "not reported"), cmpOr(strings.Join(boards, ", "), "no board"))
}

// perfBootLoader is the Sighting rule of every Candidate boot: a set whose
// m1n1 or U-Boot differs from the Mac's is staged only on a Mac with a
// Sighting.
func (s *Server) perfBootLoader(set *api.PackageSet, d api.Device) error {
	if _, sighted := s.cables.of(d.Name); sighted {
		return nil
	}
	if ch := api.BootLoaderChanges(set.Items, d.Facts.Stack); len(ch) > 0 {
		return refuse(409, "%s changes the boot loader (%s), and %s has no Sighting: a boot loader is swapped only on a Mac cabled to a controller, never over Wi-Fi alone",
			set.Name, strings.Join(ch, ", "), d.Name)
	}
	return nil
}

// perfSpec is a Performance run of the Release with bench's list: the
// metrics given, in the set's order.
func perfSpec(rel api.Release, set *api.PackageSet, bench *sets.Set, list string, metrics []string) *api.PerformanceSpec {
	return &api.PerformanceSpec{Release: rel.Name, ManifestSHA256: rel.ManifestSHA256, Kernel: rel.Kernel, Packages: set,
		Bench: bench.Name, List: list, Metrics: metrics}
}

// perfList is the list a Release is measured with: Quick while it has no
// verdict, Full once it is qualified, none when it is not qualified or its
// verdict is revoked.
func perfList(state string) string {
	switch state {
	case api.ReleaseNoVerdict, "":
		return sets.Quick
	case api.ReleaseQualified:
		return sets.Full
	}
	return ""
}

// releaseState is the Release's verdict state now.
func (s *Server) releaseState(name string) (string, error) {
	vs, err := s.store.verdicts(name)
	if err != nil || len(vs) == 0 {
		return api.ReleaseNoVerdict, err
	}
	return vs[0].State, nil
}

// omarchyMetrics are the ids of the metrics that run on Omarchy.
func omarchyMetrics(ms []sets.Metric) []string {
	var out []string
	for _, m := range ms {
		if m.Side(sets.Omarchy) != nil {
			out = append(out, m.ID)
		}
	}
	return out
}

// ---- the job ----

func (r *jobRun) savePerformance(res *api.PerformanceResult) {
	b, _ := json.Marshal(res)
	r.s.updateJob(r.j, func(j *api.Job) {
		var p api.PerformanceResult
		json.Unmarshal(b, &p)
		j.Result.Performance = &p
	})
}

func (r *jobRun) performance(ctx context.Context) (outcome, summary string) {
	ps, v := r.j.Spec.Performance, r.v
	d := v.snapshot()
	res := &api.PerformanceResult{}
	run, err := r.s.openPerf(d, ps)
	if err != nil {
		return api.OutcomeInfra, "the Performance run's attempt was not opened: " + err.Error()
	}
	res.Run, res.Attempt, res.State = run.ID, run.Attempt, run.State
	r.savePerformance(res)
	r.ev("Performance run %d, attempt %d: Release %s, %s's %s list, %d %s", run.ID, run.Attempt, ps.Release, ps.Bench, ps.List,
		len(ps.Metrics), plural(len(ps.Metrics), "benchmark", "benchmarks"))
	defer func() {
		state := perf.Failed
		if outcome == api.OutcomeCanceled || outcome == api.OutcomeIncomplete {
			state = perf.Cancelled
		}
		if now, err := r.s.store.perfRun(run.ID); err == nil && !perf.Ended(now.State) {
			if err := r.s.store.endPerfRun(run.ID, state, r.s.now()); err != nil {
				r.ev("ending Performance run %d: %v", run.ID, err)
			}
		}
		if now, err := r.s.store.perfRun(run.ID); err == nil {
			res.State = now.State
		}
		r.s.perfRunEnded(run.ID)
		if outcome != api.OutcomePass {
			res.Error = summary
		}
		r.savePerformance(res)
	}()
	rels, err := r.s.store.releases(ps.Release)
	if err != nil || len(rels) == 0 {
		return api.OutcomeStageFailed, "staging refused: no Release " + ps.Release
	}
	for _, check := range []func() error{
		func() error { return releaseSupports(rels[0], d) },
		func() error { return r.s.setFilesStored(ps.Packages) },
		func() error { return r.s.perfBootLoader(ps.Packages, d) },
	} {
		if err := check(); err != nil {
			return api.OutcomeStageFailed, "staging refused: " + err.Error()
		}
	}
	tests := perfTests(ps, run.ID)
	side := r.candidateSide(ctx, perfSide, ps.Packages.Name, ps.Packages, tests, func(*api.CompareSide) (string, string) {
		d := v.snapshot()
		res.Kernel = d.Kernel
		if d.Kernel != ps.Kernel {
			r.savePerformance(res)
			return api.OutcomeBootFailed, fmt.Sprintf("the candidate boot runs kernel %s, not Release %s's %s: nothing was measured", d.Kernel, ps.Release, ps.Kernel)
		}
		res.Booted = maps.Clone(d.Facts.Stack)
		if _, err := r.s.store.takePerf(run.ID, perf.Checked{Booted: res.Booted}, r.s.now()); err != nil {
			r.ev("recording the booted versions: %v", err)
		}
		r.savePerformance(res)
		r.ev("measuring on %s, the Release's kernel; booted %s", d.Kernel, bootedLine(res.Booted))
		return r.perfBench(ctx, tests[0], run.ID)
	})
	if side.outcome != "" {
		return side.outcome, side.summary
	}
	return api.OutcomePass, r.perfSummary(ps, run.ID)
}

// perfRunEnded is a Performance run's attempt having ended, whether it
// superseded the Mac's values (a success) or kept them: what is published
// of them follows.
func (s *Server) perfRunEnded(run int64) { s.publishPerf() }

// bootedLine names each booted package with its version.
func bootedLine(b map[string]string) string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(b)) {
		out = append(out, name+" "+b[name])
	}
	return cmpOr(strings.Join(out, ", "), "no package reported")
}

// perfBench runs the bench test on the confirmed candidate boot and says how
// the run ends if it ends there: its attempt failed, or the test ended
// without ending it.
func (r *jobRun) perfBench(ctx context.Context, t api.TestSpec, run int64) (string, string) {
	v, j := r.v, r.j
	r.state(api.JobTesting)
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
	att, err := r.s.store.perfRun(run)
	if err != nil {
		return api.OutcomeInfra, "reading Performance run " + fmt.Sprint(run) + ": " + err.Error()
	}
	switch att.State {
	case perf.Succeeded:
		return "", ""
	case perf.Failed:
		return api.OutcomeTestsFailed, "the Performance run ended failed: " + r.perfSummary(r.j.Spec.Performance, run)
	}
	if err := r.s.store.endPerfRun(run, perf.Failed, r.s.now()); err != nil {
		r.ev("ending Performance run %d: %v", run, err)
	}
	why := "test " + t.Name + " ended without ending its Performance run"
	if tr.Error != "" {
		why += ": " + tr.Error
	}
	return api.OutcomeTestsFailed, why
}

// perfSummary says what the attempt measured.
func (r *jobRun) perfSummary(ps *api.PerformanceSpec, run int64) string {
	values, reasons := 0, 0
	if got, err := r.s.store.perfResultsOf(run); err == nil {
		for _, p := range got {
			if p.Value != nil {
				values++
			} else {
				reasons++
			}
		}
	}
	return fmt.Sprintf("Release %s with %s's %s list: %d of %d %s measured, %d with a reason", ps.Release, ps.Bench, ps.List,
		values, len(ps.Metrics), plural(len(ps.Metrics), "benchmark", "benchmarks"), reasons)
}

// openPerf opens the attempt of a Performance run on the Mac as it is
// enrolled now. One the last labd left running belongs to no job, as a Mac
// runs one job at a time: it ends failed first.
func (s *Server) openPerf(d api.Device, ps *api.PerformanceSpec) (perf.Run, error) {
	want := perf.Run{Mac: d.Name, Enrollment: d.ID, Set: ps.Bench, Side: perf.SideOmarchy, Release: ps.Release, List: ps.List, Metrics: ps.Metrics}
	run, err := s.store.openPerfRun(want, s.now())
	if errors.Is(err, errPerfRunning) {
		left, lerr := s.store.perfRunning(d.Name, perf.SideOmarchy)
		for _, id := range left {
			if err := s.store.endPerfRun(id, perf.Failed, s.now()); err != nil && lerr == nil {
				lerr = err
			}
		}
		if lerr != nil {
			return run, lerr
		}
		run, err = s.store.openPerfRun(want, s.now())
	}
	return run, err
}

// perfRunning is the Mac's attempts on side still running.
func (st *store) perfRunning(mac, side string) ([]int64, error) {
	rows, err := st.db.Query(`SELECT id FROM perf_runs WHERE mac=? AND side=? AND state=?`, mac, side, perf.Running)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// perfResultsOf is what one attempt uploaded.
func (st *store) perfResultsOf(run int64) ([]perf.Result, error) {
	return scanPerfResults(st.db.Query(`SELECT `+perfResultCols+` FROM perf_results p
JOIN perf_runs r ON r.id = p.run WHERE p.run=? ORDER BY p.id`, run))
}

// perfTries is, for the Mac's Omarchy runs of the set on the Release, each
// metric's current reason ("" for a value), and how many attempts that
// ended asked for it.
func (st *store) perfTries(mac, set, release string) (map[string]string, map[string]int, error) {
	cur := map[string]string{}
	rows, err := st.db.Query(`SELECT p.metric, coalesce(p.reason, '') FROM perf_current c JOIN perf_results p ON p.id = c.result
WHERE c.mac=? AND c.set_name=? AND c.side=? AND c.target=?`, mac, set, perf.SideOmarchy, release)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var m, reason string
		if err := rows.Scan(&m, &reason); err != nil {
			rows.Close()
			return nil, nil, err
		}
		cur[m] = reason
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	tries := map[string]int{}
	rows, err = st.db.Query(`SELECT metrics FROM perf_runs WHERE mac=? AND set_name=? AND side=? AND release_version=? AND state IN (?, ?)`,
		mac, set, perf.SideOmarchy, release, perf.Succeeded, perf.Failed)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, nil, err
		}
		var ms []string
		json.Unmarshal([]byte(b), &ms)
		for _, m := range ms {
			tries[m]++
		}
	}
	return cur, tries, rows.Err()
}

// ---- lab perf rerun ----

// queuePerfRerun runs a Mac's Performance run of the Release again: the
// Release's list as its verdict has it, every metric of it, under p's
// Lease, taken for them when they hold none. Its values replace the last
// only when it succeeds (perf.go).
func (s *Server) queuePerfRerun(p Person, req api.PerfRerunRequest) (*api.Job, error) {
	v := s.dev(req.Mac)
	if v == nil {
		return nil, refuse(404, "no Mac %q", req.Mac)
	}
	if !p.isMaintainer() && !s.holdsLease(v, p) {
		return nil, refuse(403, "a Performance rerun is a Maintainer's, or the Lease holder's of %s: `lab lease %s` first", req.Mac, req.Mac)
	}
	bench, err := perfCurrentSet()
	if err != nil {
		return nil, refuse(409, "no Benchmark set to measure with: %v", err)
	}
	rels, err := s.store.releases(req.Release)
	if err != nil {
		return nil, err
	}
	if len(rels) == 0 {
		return nil, refuse(404, "no Release %q: a Release is listed once its manifest is frozen", req.Release)
	}
	rel := rels[0]
	state, err := s.releaseState(rel.Name)
	if err != nil {
		return nil, err
	}
	list := perfList(state)
	if list == "" {
		return nil, refuse(409, "Release %s is %s: its values have left the Performance pages, and it is measured no more", rel.Name, stateWords(state))
	}
	metrics := omarchyMetrics(bench.List(list))
	set, err := s.releaseSet(rel)
	if err != nil {
		return nil, err
	}
	d := v.snapshot()
	if err := releaseSupports(rel, d); err != nil {
		return nil, err
	}
	if err := s.perfBootLoader(set, d); err != nil {
		return nil, err
	}
	ps := perfSpec(rel, set, bench, list, metrics)
	ps.Rerun = true
	if !s.holdsLease(v, p) {
		if _, err := s.takeLease(v, p, perfHolder, leaseDefault); err != nil {
			return nil, err
		}
		ps.LeaseTaken = true
	}
	j, code, err := s.submit(p, api.JobSpec{Device: req.Mac, Holder: perfHolder, Performance: ps}, "",
		fmt.Sprintf("a Performance rerun of Release %s with %s's %s list: its values replace the last only if it succeeds", rel.Name, bench.Name, list))
	if err != nil {
		if ps.LeaseTaken {
			s.releaseLease(v, p, perfHolder, false, "")
		}
		return nil, refuse(code, "%v", err)
	}
	return j, nil
}

func (s *Server) hPerfRerun(w http.ResponseWriter, r *http.Request) {
	var req api.PerfRerunRequest
	if !decodeBody(w, r, 1<<12, &req) {
		return
	}
	j, err := s.queuePerfRerun(leaser(r), req)
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, j)
}
