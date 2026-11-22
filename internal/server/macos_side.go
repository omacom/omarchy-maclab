package server

// The macOS side of Performance (spec section 22, "The macOS side"). A
// macOS result is one Mac's measurements of one Benchmark set in macOS at
// one macOS build: the Full list, kept in perf_runs and perf_results as a
// run of side macos. When the Mac has none for the current set at the
// build its runner last reported, labd queues a macOS run, opening its
// attempt (queueMacOSSide), and the Mac takes it in its next slot after its
// Omarchy side, as a job of the Mac (spec.macos, macOSWanted): the job
// sends boot_macos_once, waits for the runner's results or the run's
// deadline, and ends once the Mac is back on its Known-good kernel.
//
// An Unattended Mac's run needs nobody. An Attended Mac's waits in Omarchy:
// labd tells the Mac's Volunteer, or the Lab group for a Lab-owned Mac, and
// boots macOS only once a person says they are at the Mac (atTheMac, on its
// page, on My Macs or the API). With nobody within a day the macOS side is
// dropped; the Omarchy side stands.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("POST /macs/{name}/macos/at-mac", web.SignedIn, (*Server).pMacOSAtMac)
}

// macosScheduled labels the macOS runs labd queues itself.
const macosScheduled = "maclab-macos"

// macOSTriesMax is how many attempts at one set and build may fail or be
// left needs_hands before labd queues no more of them.
const macOSTriesMax = 2

var errNoMacOSWait = errors.New("no macOS run of this Mac waits for a person")

// queueMacOSSide queues the Mac's macOS run when it has no macOS result for
// the current set at the build its runner last reported, and none is
// queued: its attempt is opened (perf_runs, side macos) and the run carries
// it. It answers nil, nil when nothing is missing or the run cannot be
// queued yet: no runner enrolled, no build reported, an attempt waiting,
// two attempts that failed, or a run dropped for nobody at the Mac, until
// the Mac's next Omarchy side has ended.
func (s *Server) queueMacOSSide(mac string) (*api.MacOSRun, error) {
	set, err := macOSCurrentSet()
	if err != nil {
		return nil, err
	}
	return s.queueMacOSSideOf(mac, set)
}

func (s *Server) queueMacOSSideOf(mac string, set *sets.Set) (*api.MacOSRun, error) {
	if s.dev(mac) == nil {
		return nil, fmt.Errorf("%w %q", errNoMac, mac)
	}
	enrollment, in := s.enrollmentOf(mac)
	if !in || !s.macOS().enrolled(mac, enrollment) {
		return nil, nil
	}
	b, ok := s.macOS().buildOf(mac, enrollment)
	if !ok {
		return nil, nil
	}
	if kept, err := s.store.perfCurrentMacOS(mac, set.Name, b.Build); err != nil || len(kept) > 0 {
		return nil, err
	}
	if last := s.macOS().run(mac); last != nil {
		if !api.MacOSEnded(last.State) {
			return nil, nil
		}
		if (last.State == api.MacOSDropped || last.State == api.MacOSCancelled) && last.Set == set.Name && last.Build == b.Build {
			since := last.Queued
			if last.Done != nil {
				since = *last.Done
			}
			if n, err := s.store.omarchySidesEnded(mac, since); err != nil || n == 0 {
				return nil, err
			}
		}
	}
	if n, err := s.store.macOSTries(mac, set.Name, b.Build); err != nil || n >= macOSTriesMax {
		return nil, err
	}
	arts, err := s.macOSFiles(set, sets.Full)
	if err != nil {
		return nil, err
	}
	now := s.now()
	pr, err := s.store.openPerfRun(perf.Run{Mac: mac, Enrollment: enrollment, Set: set.Name, Side: perf.SideMacOS,
		MacOSVersion: b.Version, MacOSBuild: b.Build, List: sets.Full}, now)
	if errors.Is(err, errPerfRunning) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	run, err := s.macOS().queueRun(&api.MacOSRun{Mac: mac, Tier: api.TierExtended, Set: set.Name, PerfRun: pr.ID,
		Version: b.Version, Build: b.Build, Artifacts: arts}, now)
	if err != nil {
		s.store.endPerfRun(pr.ID, perf.Cancelled, now)
		if errors.Is(err, errRunWaiting) {
			return nil, nil
		}
		return nil, err
	}
	s.log.Printf("macOS run %s queued for %s: %s's Full list at macOS %s (%s), attempt %d", run.ID, mac, set.Name, b.Version, b.Build, pr.Attempt)
	return run, nil
}

// macOSWanted is the job of the Mac's macOS run, for its next slot after
// its Omarchy side: the Performance scheduler asks it of a Mac that misses
// nothing on Omarchy now, and claims the Mac for the job it answers. It
// queues the run first when the Mac's macOS result is missing. Nil when no
// run is due, its job is queued already, or an Attended Mac waits for a
// person: the first time, the Mac's Volunteer or the Lab group is told the
// run is due, and the day labd waits starts.
func (s *Server) macOSWanted(v *dev) *api.JobSpec {
	name := v.snapshot().Name
	if _, err := s.queueMacOSSide(name); err != nil {
		s.log.Printf("%s: its macOS run: %v", name, err)
	}
	run := s.macOS().run(name)
	if run == nil || run.State != api.MacOSQueued || run.PerfRun == 0 || s.macOSJobOpen(run) {
		return nil
	}
	if s.macOSSide(name).Attendance == api.MacOSAttended && run.AtMac == nil {
		s.macOSDue(v, run)
		return nil
	}
	return macOSJobSpec(name, run.ID)
}

func macOSJobSpec(mac, run string) *api.JobSpec {
	return &api.JobSpec{Device: mac, Holder: macosScheduled, MacOS: &api.MacOSJobSpec{Run: run}}
}

// macOSJobOpen is whether the run's job is queued or running.
func (s *Server) macOSJobOpen(run *api.MacOSRun) bool {
	if run.Job == "" {
		return false
	}
	j, err := s.store.job(run.Job)
	return err == nil && j.State != api.JobDone
}

// macOSDue tells, once, that an Attended Mac's macOS run is due and waits
// for a person: the Mac's Volunteer by a Notice, or the Lab group for a
// Lab-owned Mac. labd waits a day for them.
func (s *Server) macOSDue(v *dev, run *api.MacOSRun) {
	now := s.now()
	until := now.Add(macosWaitForPerson)
	told, err := s.macOS().change(run.Mac, func(m *api.MacOSRun) error {
		if m.ID != run.ID || m.State != api.MacOSQueued || m.Told != nil {
			return errNoRun
		}
		m.Attended, m.Told, m.Expires = true, &now, &until
		return nil
	})
	if err != nil {
		return
	}
	s.armMacOSTimer(told.Mac, told.ID, until.Sub(now), s.macOSNobodyCame)
	d := v.snapshot()
	if d.IsVolunteer() {
		s.notify(s.notice(d, noticeMacOS, jobWording{}))
	} else {
		s.labGroup.MacOSRunDue(d.Name, until)
	}
	s.log.Printf("%s: macOS run %s is due on an Attended Mac: it waits for a person at the Mac until %s", d.Name, told.ID, when(until))
}

// macOSNobodyCame drops an Attended Mac's macOS run nobody came to in a
// day: its attempt is cancelled, and the Omarchy side stands.
func (s *Server) macOSNobodyCame(mac, id string) {
	now := s.now()
	run, err := s.macOS().change(mac, func(m *api.MacOSRun) error {
		if m.ID != id || m.State != api.MacOSQueued || m.AtMac != nil {
			return errNoRun
		}
		m.State, m.Done, m.Error = api.MacOSDropped, &now, "nobody was at the Mac within a day of the run being due: the macOS side is dropped"
		return nil
	})
	if err != nil {
		return
	}
	s.endMacOSPerfRun(run, perf.Cancelled)
	s.log.Printf("%s: macOS run %s dropped: nobody was at the Mac within a day", mac, id)
}

// endMacOSPerfRun ends the run's attempt from labd's side.
func (s *Server) endMacOSPerfRun(run *api.MacOSRun, state string) {
	if run.PerfRun == 0 {
		return
	}
	if err := s.store.endPerfRun(run.PerfRun, state, s.now()); err != nil && !errors.Is(err, errPerfEnded) {
		s.log.Printf("macOS run %s: its performance run %d: %v", run.ID, run.PerfRun, err)
	}
}

// cancelDroppedMacOSRuns cancels the attempts of the runs a revoked runner
// left unfinished.
func (s *Server) cancelDroppedMacOSRuns() {
	for _, id := range s.macOS().takeDropped() {
		if id > 0 {
			if err := s.store.endPerfRun(id, perf.Cancelled, s.now()); err != nil && !errors.Is(err, errPerfEnded) {
				s.log.Printf("performance run %d of a revoked macOS runner: %v", id, err)
			}
		}
	}
}

// macOSPerfEnded is the runner's upload ending the macOS run's attempt:
// the run is done, whether it was running or past its deadline.
func (s *Server) macOSPerfEnded(mac string, perfRun int64, state string) {
	now := s.now()
	run, err := s.macOS().change(mac, func(m *api.MacOSRun) error {
		if m.PerfRun != perfRun || (m.State != api.MacOSStarted && m.State != api.MacOSRunning && m.State != api.MacOSNeedsHands) {
			return errNoRun
		}
		late := m.State == api.MacOSNeedsHands
		m.State, m.Done = api.MacOSDone, &now
		if state != perf.Succeeded {
			m.Error = "the macOS runner ended the run " + state
		} else if late {
			m.Error = ""
		}
		return nil
	})
	if err != nil {
		return
	}
	s.macOS().stopTimer(mac)
	s.log.Printf("%s: macOS run %s done: its performance run %d %s", mac, run.ID, perfRun, state)
}

// mayAtMac is whether p may say they are at the Mac d: a Maintainer, the
// Volunteer of a Volunteer's Mac, or an M team member at a Lab-owned Mac.
func mayAtMac(p Person, d api.Device) bool {
	switch {
	case p.isMaintainer():
		return true
	case d.IsVolunteer():
		return d.VolunteerID != 0 && d.VolunteerID == p.AccountID
	}
	return p.OnMTeam()
}

// macOSWaiting is the Mac's macOS run waiting for a person, or nil.
func (s *Server) macOSWaiting(name string) *api.MacOSRun {
	run := s.macOS().run(name)
	if run == nil || run.State != api.MacOSQueued || run.Told == nil || run.AtMac != nil {
		return nil
	}
	return run
}

// atTheMac is a person saying they are at the Mac whose macOS run is due:
// only now does labd queue the job that boots macOS, for them to type its
// password and log in. The day's wait ends.
func (s *Server) atTheMac(v *dev, p Person) (*api.MacOSRun, error) {
	d := v.snapshot()
	if !mayAtMac(p, d) {
		return nil, refuse(403, "only %s's Volunteer or a Maintainer says they are at it", d.Name)
	}
	now := s.now()
	run, err := s.macOS().change(d.Name, func(m *api.MacOSRun) error {
		if m.State != api.MacOSQueued || m.PerfRun == 0 {
			return errNoMacOSWait
		}
		if m.AtMac == nil {
			m.AtMac, m.AtMacBy = &now, cmpOr(p.Handle, "a Maintainer")
		}
		return nil
	})
	if err != nil {
		return nil, refuse(409, "%s: %v", d.Name, errNoMacOSWait)
	}
	s.macOS().stopTimer(d.Name)
	s.log.Printf("%s: %s is at the Mac for macOS run %s", d.Name, run.AtMacBy, run.ID)
	if !s.macOSJobOpen(run) && s.claimForPerf(v, *macOSJobSpec(d.Name, run.ID)) == nil {
		s.log.Printf("%s: the job of macOS run %s waits for the Mac's next slot", d.Name, run.ID)
		s.perfKick()
	}
	return s.macOS().run(d.Name), nil
}

// submitMacOSRun queues the job of the Mac's macOS run, once.
func (s *Server) submitMacOSRun(v *dev, run *api.MacOSRun) (*api.Job, error) {
	if s.macOSJobOpen(run) {
		return nil, fmt.Errorf("job %s boots macOS for it already", run.Job)
	}
	d := v.snapshot()
	j, _, err := s.submit(Person{}, *macOSJobSpec(d.Name, run.ID), "", fmt.Sprintf("macOS run %s: %s's Full list at macOS %s (%s)", run.ID, run.Set, run.Version, run.Build))
	if err != nil {
		return nil, err
	}
	s.macOS().change(d.Name, func(m *api.MacOSRun) error {
		if m.ID == run.ID {
			m.Job = j.ID
		}
		return nil
	})
	return j, nil
}

func (s *Server) hMacOSAtMac(w http.ResponseWriter, r *http.Request) {
	v := s.dev(r.PathValue("name"))
	if v == nil {
		httpErr(w, 404, "no device %q", r.PathValue("name"))
		return
	}
	run, err := s.atTheMac(v, leaser(r))
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, run)
}

// pMacOSAtMac is "I am at the Mac", on the Mac's page and on My Macs.
func (s *Server) pMacOSAtMac(w http.ResponseWriter, r *http.Request) {
	v := s.dev(r.PathValue("name"))
	p := leaser(r)
	if v == nil || !mayAtMac(p, v.snapshot()) {
		s.notFound(w, r)
		return
	}
	if _, err := s.atTheMac(v, p); err != nil {
		s.renderMac(w, r, v, tabOverview, err)
		return
	}
	to := "/macs/" + url.PathEscape(v.snapshot().Name)
	if r.FormValue("back") == "my-macs" {
		to = "/my-macs"
	}
	w.Header().Set("Location", s.basePath()+to)
	w.WriteHeader(http.StatusSeeOther)
}

// macOSWaitWords is when the Mac's macOS run stops waiting for p at the
// Mac, "" when it waits for nobody p may be.
func (s *Server) macOSWaitWords(v *dev, p Person) string {
	run := s.macOSWaiting(v.snapshot().Name)
	if run == nil || run.Expires == nil || !mayAtMac(p, v.snapshot()) {
		return ""
	}
	return when(*run.Expires)
}

// ---- the job ----

// macOSRun is the job of a Mac's macOS run: boot macOS once for it, wait
// for its results or its deadline, and for the Mac back on its Known-good
// kernel.
func (r *jobRun) macOSRun(ctx context.Context) (string, string) {
	name := r.v.snapshot().Name
	id := r.j.Spec.MacOS.Run
	run, err := r.s.macOS().change(name, func(m *api.MacOSRun) error {
		if m.ID != id || m.State != api.MacOSQueued {
			return errNoRun
		}
		m.Job = r.j.ID
		return nil
	})
	if err != nil {
		state := "gone"
		if m := r.s.macOS().run(name); m != nil && m.ID == id {
			state = m.State
		}
		return api.OutcomeCanceled, "not run: macOS run " + id + " is " + state + ", not queued"
	}
	if run.AtMac == nil && run.Told != nil && r.s.macOSSide(name).Attendance == api.MacOSAttended {
		return api.OutcomeCanceled, "not run: nobody said they are at " + name + ", an Attended Mac"
	}
	if run.AtMac != nil {
		r.ev("macOS: @%s is at the Mac", run.AtMacBy)
	}
	m, outcome, why := r.bootMacOSFor(ctx, run)
	if outcome != "" {
		return outcome, why
	}
	if m.Error != "" {
		return api.OutcomeTestsFailed, "macOS run " + m.ID + ": " + m.Error
	}
	return api.OutcomePass, fmt.Sprintf("macOS run %s: %s at macOS %s (%s), performance run %d", m.ID, m.Set, m.Version, m.Build, m.PerfRun)
}

// bootMacOSFor sends boot_macos_once for the queued run, waits for its
// results, or for its deadline to leave the Mac Needs hands, and then for
// the Mac back on its Known-good kernel. A start that failed is cancelled.
// outcome is "" when the run is done and the Mac back.
func (r *jobRun) bootMacOSFor(ctx context.Context, run *api.MacOSRun) (m *api.MacOSRun, outcome, why string) {
	v, j, name := r.v, r.j, r.v.snapshot().Name
	r.ev("macOS: booting macOS once for run %s", run.ID)
	r.state(api.JobBooting)
	oldBoot := v.snapshot().BootID
	if err := v.call(ctx, api.CmdBootMacOS, j.ID, nil, 2*time.Minute, nil); err != nil {
		said := strings.TrimPrefix(err.Error(), api.CmdBootMacOS+": ")
		if c := r.s.cancelMacOSStart(name, run.ID, said); c != nil {
			r.ev("macOS: the start failed, run %s is cancelled: %s", run.ID, said)
			return c, api.OutcomeInfra, "booting macOS: " + said
		}
		r.ev("macOS: boot-macos-once answered %s, but labd had the run started", said)
	}
	r.state(api.JobTesting)
	for {
		m = r.s.macOS().run(name)
		if m == nil || m.ID != run.ID {
			return m, api.OutcomeCanceled, "the macOS run is gone"
		}
		switch m.State {
		case api.MacOSDone:
		case api.MacOSNeedsHands:
			why := "the macOS runner did not report within " + macosDeadline(m.Tier).String() + ": " + name + " Needs hands (an Attended Mac waits for its password at the screen); its results are still taken when they come"
			r.ev("macOS: %s", why)
			return m, api.OutcomeLostContact, why
		case api.MacOSCancelled, api.MacOSDropped:
			return m, api.OutcomeCanceled, "macOS run " + m.ID + " " + m.State
		default:
			select {
			case <-ctx.Done():
				return m, api.OutcomeCanceled, "canceled while macOS ran"
			case <-r.s.clock().After(time.Second):
			}
			continue
		}
		break
	}
	r.ev("macOS: run %s done; waiting for the Mac back on its Known-good kernel", run.ID)
	back := func() bool {
		return v.aliveLocked() && v.d.BootID != oldBoot && (v.d.KnownGood == "" || v.d.Kernel == v.d.KnownGood)
	}
	if err := v.waitFor(ctx, 3*r.boot+r.s.cfg.RebootGrace+5*time.Minute, back); err != nil {
		return m, api.OutcomeLostContact, "the Mac did not come back from macOS on its Known-good kernel: " + err.Error()
	}
	r.ev("macOS: back on %s", v.snapshot().Kernel)
	return m, "", ""
}

// cancelMacOSStart cancels the run whose start failed, while it is still
// queued, and its attempt; nil when the run had started after all.
func (s *Server) cancelMacOSStart(mac, id, why string) *api.MacOSRun {
	now := s.now()
	run, err := s.macOS().change(mac, func(m *api.MacOSRun) error {
		if m.ID != id || m.State != api.MacOSQueued {
			return errNoRun
		}
		m.State, m.Done, m.Error = api.MacOSCancelled, &now, "boot-macos-once failed: "+why
		return nil
	})
	if err != nil {
		return nil
	}
	s.endMacOSPerfRun(run, perf.Cancelled)
	return run
}

// ---- Compare ----

// keptMacOS is the Mac's macOS result of the set at the build its runner
// last reported, as a Compare's side, or nil when none is kept.
func (s *Server) keptMacOS(mac, setName string) ([]api.BenchResult, macosBuild) {
	enrollment, in := s.enrollmentOf(mac)
	if setName == "" || !in {
		return nil, macosBuild{}
	}
	b, ok := s.macOS().buildOf(mac, enrollment)
	if !ok {
		return nil, b
	}
	kept, err := s.store.perfCurrentMacOS(mac, setName, b.Build)
	if err != nil || len(kept) == 0 {
		return nil, b
	}
	return benchOf(setName, kept), b
}

// benchOf is a macOS result as a Compare side's benchmarks, by metric id.
func benchOf(setName string, rs []perf.Result) []api.BenchResult {
	set, _ := perfSet(setName)
	out := make([]api.BenchResult, 0, len(rs))
	for _, p := range rs {
		b := api.BenchResult{Name: p.Metric, Value: p.Value, Unit: p.Unit, Seconds: p.Seconds, API: p.API}
		if p.Spread != nil {
			b.Spread = *p.Spread
		}
		if p.Reason != nil {
			b.Note = *p.Reason
		}
		if set != nil {
			if m, ok := set.Metric(p.Metric); ok {
				b.Version, b.HigherIsBetter = m.Version, m.HigherIsBetter()
			}
		}
		out = append(out, b)
	}
	return out
}

// compareMacOSRun is the macOS run a Compare boots macOS for: the Mac's
// queued run of the set, a new one that is its macOS result, or, with no
// build reported yet, a run of the tier's list.
func (s *Server) compareMacOSRun(mac, tier, setName string) (*api.MacOSRun, error) {
	if run := s.macOS().run(mac); run != nil && run.State == api.MacOSQueued && run.PerfRun > 0 && run.Set == setName {
		return run, nil
	}
	if setName != "" {
		if set, err := perfSet(setName); err == nil {
			if run, err := s.queueMacOSSideOf(mac, set); err != nil || run != nil {
				return run, err
			}
		}
	}
	return s.queueMacOSRun(mac, tier)
}

// ---- the store ----

// retargetMacOSPerfRun has a running macOS attempt measure the build of the
// boot that took it: the next attempt at that build.
func (st *store) retargetMacOSPerfRun(id int64, version, build string) error {
	_, err := st.db.Exec(`UPDATE perf_runs SET macos_version=?, macos_build=?,
attempt=(SELECT coalesce(max(o.attempt), 0) + 1 FROM perf_runs o WHERE o.mac=perf_runs.mac AND o.set_name=perf_runs.set_name
  AND o.side=perf_runs.side AND o.release_version='' AND o.macos_build=?)
WHERE id=? AND side=? AND state=? AND macos_build<>?`, version, build, build, id, perf.SideMacOS, perf.Running, build)
	return err
}

// perfAttemptResults is what one attempt uploaded.
func (st *store) perfAttemptResults(id int64) ([]perf.Result, error) {
	return scanPerfResults(st.db.Query(`SELECT `+perfResultCols+` FROM perf_results p
JOIN perf_runs r ON r.id = p.run WHERE p.run=? ORDER BY p.id`, id))
}

// omarchySidesEnded is how many of the Mac's Omarchy attempts ended after t.
func (st *store) omarchySidesEnded(mac string, t time.Time) (int, error) {
	var n int
	err := st.db.QueryRow(`SELECT count(*) FROM perf_runs WHERE mac=? AND side=? AND ended > ?`, mac, perf.SideOmarchy, t.UnixMicro()).Scan(&n)
	return n, err
}

// macOSTries is how many of the Mac's macOS attempts at the set and build
// failed or were left needs_hands.
func (st *store) macOSTries(mac, set, build string) (int, error) {
	var n int
	err := st.db.QueryRow(`SELECT count(*) FROM perf_runs WHERE mac=? AND set_name=? AND side=? AND macos_build=? AND state IN (?, ?)`,
		mac, set, perf.SideMacOS, build, perf.Failed, perf.NeedsHands).Scan(&n)
	return n, err
}
