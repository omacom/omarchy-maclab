package server

// Jobs and a job's page. The M team and the Maintainers read every job on
// every Mac; a Volunteer opens the jobs of the Mac they lend, and to anyone
// else a job is not there. Cancel and Retry follow what a Lease fences: an
// M team member cancels their own jobs and retries under their own Lease.

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /jobs", web.MTeamUp, (*Server).pJobs)
	addPage("GET /jobs/{id}", web.SignedIn, (*Server).pJob)
	addPage("POST /jobs/{id}/cancel", web.MTeamUp, (*Server).pJobCancel)
	addPage("POST /jobs/{id}/retry", web.MTeamUp, (*Server).pJobRetry)
	addPage("POST /jobs/{id}/publish", web.MTeamUp, (*Server).pJobPublish)
}

// jobsShown is how many jobs Jobs lists, newest first.
const jobsShown = 100

// took is a length of time in a few words: 42 s, 3 min 10 s, 2 h 5 min.
func took(d time.Duration) string {
	sec := int(d.Round(time.Second).Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d s", sec)
	case sec < 3600:
		return fmt.Sprintf("%d min %d s", sec/60, sec%60)
	}
	return fmt.Sprintf("%d h %d min", sec/3600, sec%3600/60)
}

type jobsData struct {
	// Mac is the one Mac whose jobs these are; "" for the fleet's.
	Mac string
	// Member words the page for an M team member.
	Member bool
	Jobs   []jobRow
}

type jobRow struct {
	ID, Mac, What string
	// By is the handle of who asked; "" is the lab itself.
	By                string
	State, StateClass string
	When              string
}

// outcomeWords are a finished job's outcome as a badge says it, and the
// badge's colour.
var outcomeWords = map[string][2]string{
	api.OutcomePass:          {"Passed", "job-ok"},
	api.OutcomeTestsFailed:   {"Tests failed", "job-warn"},
	api.OutcomeUnhealthy:     {"Booted, unhealthy", "job-warn"},
	api.OutcomePanicked:      {"Panicked", "job-bad"},
	api.OutcomeHung:          {"Hung", "job-bad"},
	api.OutcomeCrashedOrHung: {"Crashed or hung", "job-bad"},
	api.OutcomeBootFailed:    {"Boot failed", "job-bad"},
	api.OutcomeLostContact:   {"Lost contact", "job-bad"},
	api.OutcomeStageFailed:   {"Staging failed", "job-warn"},
	api.OutcomeBuildFailed:   {"Build failed", "job-bad"},
	api.OutcomeInfra:         {"Lab error", "job-warn"},
	api.OutcomeCanceled:      {"Canceled", "job-dim"},
	api.OutcomeIncomplete:    {"Incomplete", "job-warn"},
	api.OutcomeUnknown:       {"Unknown", "job-warn"},
}

// jobState is a job's state as its badge says it.
func jobState(j *api.Job) (words, class string) {
	if j.State != api.JobDone {
		return string(j.State), "job-live"
	}
	if w, ok := outcomeWords[j.Outcome]; ok {
		return w[0], w[1]
	}
	return orStr(strings.ReplaceAll(j.Outcome, "_", " "), "done"), "job-dim"
}

var (
	reBuiltKernel  = regexp.MustCompile(`^(?:staged|reusing|kernel) (\S+?)[: ]`)
	reStagedKernel = regexp.MustCompile(`^staged (\S+) as`)
)

// jobWhat says in a few words what a job is: the kernel it boots, by its
// release once the lab knows it.
func jobWhat(j *api.Job) string {
	spec := j.Spec
	switch {
	case spec.Baseline:
		return "Baseline"
	case spec.Crash != "":
		return "Crash test"
	case spec.InstalledManifest != "":
		return "Installed run"
	case spec.Mesa != nil:
		return "Mesa test"
	case spec.Compare != nil:
		return "Compare " + spec.Compare.Before + " and " + spec.Compare.After
	case spec.Performance != nil:
		return "Performance run of Release " + spec.Performance.Release
	case j.Result.BootKernel != "":
		return "Boot " + j.Result.BootKernel
	case spec.Source != "":
		for _, e := range j.Events {
			if m := reBuiltKernel.FindStringSubmatch(e.Msg); m != nil && !strings.HasPrefix(m[1], "owner") && !strings.HasPrefix(m[1], "build") {
				return "Build and boot " + m[1]
			}
		}
		return "Build and boot " + sourceName(spec.Source)
	case spec.Kernel == "":
		return "Boot the current kernel"
	}
	for _, e := range j.Events {
		if m := reStagedKernel.FindStringSubmatch(e.Msg); m != nil {
			return "Boot " + m[1]
		}
	}
	return "Boot kernel " + spec.Kernel[:min(12, len(spec.Kernel))]
}

// sourceName is a source as a row has room for: owner/repo and what follows.
func sourceName(source string) string {
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(source, "https://"), "github.com/"), ".git")
}

func (s *Server) pJobs(w http.ResponseWriter, r *http.Request) {
	p := leaser(r)
	data := jobsData{Mac: r.URL.Query().Get("mac"), Member: !p.isMaintainer()}
	jobs, err := s.store.jobs(data.Mac, jobsShown, false)
	if err != nil {
		s.log.Printf("jobs: %v", err)
		httpErr(w, 500, "The lab couldn't list its jobs.")
		return
	}
	for _, j := range jobs {
		row := jobRow{ID: j.ID, Mac: j.Spec.Device, What: jobWhat(j), By: j.Maintainer, When: whenDay(j.Created)}
		row.State, row.StateClass = jobState(j)
		data.Jobs = append(data.Jobs, row)
	}
	s.render(w, r, "jobs/index", web.Page{Title: "Jobs", Nav: "jobs", Data: data})
}

// seesJobAs reports whether the person may read job j: the M team and the
// Maintainers any, a Volunteer one their Mac ran in its enrollment now.
func (s *Server) seesJobAs(p Person, j *api.Job) bool {
	return s.seesJob(callerAs(p), j)
}

// jobOr404 is the job a page names, for a person who may read it. To anyone
// else it is not there.
func (s *Server) jobOr404(w http.ResponseWriter, r *http.Request) (*api.Job, Person) {
	p := leaser(r)
	j, err := s.store.job(r.PathValue("id"))
	if err != nil || !s.seesJobAs(p, j) {
		s.notFound(w, r)
		return nil, p
	}
	return j, p
}

type jobData struct {
	ID, Mac, What, By string
	State, StateClass string
	// Says is the job's summary, or while it runs its latest event.
	Says string
	// Flash is a refusal, said above the job.
	Flash string
	// Reloads has the page ask again by itself while the job is not done.
	Reloads          bool
	CanCancel, Retry bool
	CancelSentence   string
	Asked, Took      string
	BootedIn         string
	Kernel, Source   string
	Build            string
	BuildOpens       bool
	Cmdline          string
	Tests            string
	Timeout          int
	RetryOf          string
	UnderLease       bool
	FellBack         bool
	Results          []jobTest
	Recovery         []string
	KernelEvents     []jobKernelEvent
	NewErrors        []string
	NewErrorsLeftOut int
	OMT              *jobOMT
	Events           []jobEvent
	Files            []string
	Compare          *compareView
	// CompareAsked is what a Compare was asked to do, in place of a kernel
	// and tests.
	CompareAsked *compareAsked
}

type compareAsked struct {
	Before, After, Tier string
	// Current is a side A of the Mac's own system, with no set.
	Current      bool
	MacOS, Hands bool
	// BeforeHref and AfterHref are the sets' pages while whoever reads the
	// job may open them.
	BeforeHref, AfterHref string
}

type jobTest struct {
	Name, Verdict, Class, Tail string
}

type jobKernelEvent struct{ Kind, Source, Line string }

type jobEvent struct{ When, Msg string }

type jobOMT struct {
	Tool             string
	Pass, Fail, Skip int
	ComparedTo       string
	Changed          []jobCheck
	// Report is what became of the run's report, in one of four words;
	// ReportHref its page while whoever reads the job can open it.
	Report, ReportClass, ReportHref string
	// Deleted is who deleted the report and when; Note why it was not
	// published.
	Deleted, Note string
	// CanPublish is whether the person may publish the report by hand now.
	CanPublish bool
}

var reportClasses = map[string]string{
	api.ReportNotPublished: "job-dim", api.ReportPublished: "job-ok", api.ReportHidden: "job-warn", api.ReportIsDeleted: "job-bad",
}

type jobCheck struct{ Tag, Class, ID, Evidence string }

// newErrorsShown caps the kernel lines a job's page lists; the files have
// them all.
const newErrorsShown = 60

func cancelSentence(j *api.Job) string {
	return "Cancel: job " + j.ID + " on " + j.Spec.Device + " stops, and a Mac it booted returns to its Known-good kernel"
}

func (s *Server) pJob(w http.ResponseWriter, r *http.Request) {
	if j, p := s.jobOr404(w, r); j != nil {
		s.renderJob(w, r, j, p, nil)
	}
}

// renderJob writes a job's page; with a refusal, it is said above the job
// under its own status.
func (s *Server) renderJob(w http.ResponseWriter, r *http.Request, j *api.Job, p Person, refused error) {
	spec, res := j.Spec, j.Result
	data := jobData{ID: j.ID, Mac: spec.Device, What: jobWhat(j), By: j.Maintainer, Says: j.Summary, Asked: whenDay(j.Created),
		Kernel: spec.Kernel, Source: spec.Source, Build: spec.Build, BuildOpens: p.isMaintainer(), Cmdline: spec.Cmdline,
		Timeout: spec.BootTimeoutSec, RetryOf: j.RetryOf, UnderLease: j.UnderLease, FellBack: res.FellBack, Recovery: res.Recovery}
	data.State, data.StateClass = jobState(j)
	if cs := spec.Compare; cs != nil {
		asked := &compareAsked{Before: cs.Before, After: cs.After, Tier: cs.Tier, Current: cs.Before == api.CompareCurrent, MacOS: cs.MacOS, Hands: cs.Hands}
		if p.isMaintainer() {
			asked.AfterHref = s.href("/builds/sets/" + url.PathEscape(cs.After))
			if !asked.Current {
				asked.BeforeHref = s.href("/builds/sets/" + url.PathEscape(cs.Before))
			}
		}
		data.CompareAsked = asked
	}
	if data.Compare = compareViewOf(j); data.Compare != nil {
		data.Compare.CanPublish = p.isMaintainer() && j.State == api.JobDone && j.Result.Compare.Published == nil
		data.Compare.Waiting = data.Compare.Waiting && s.mayHands(p, j)
	}
	page := web.Page{Title: "Job " + j.ID, Nav: "jobs"}
	if refused != nil {
		data.Flash, page.Status = refused.Error(), statusOf(refused)
	}
	done := j.State == api.JobDone
	if done {
		data.Took = took(doneAt(j).Sub(j.Created))
	} else {
		data.Reloads = refused == nil
		if n := len(j.Events); n > 0 {
			data.Says = j.Events[n-1].Msg
		}
	}
	if res.Booted {
		data.BootedIn = took(time.Duration(res.BootSeconds * float64(time.Second)))
	}
	data.CanCancel, data.CancelSentence = !done && mayCancel(p, j) == nil, cancelSentence(j)
	data.Retry = done && p.OnMTeam() && (spec.Compare == nil || p.isMaintainer()) && spec.Performance == nil

	names := []string{"boot-health"}
	for _, t := range spec.Tests {
		name := t.Name
		if t.GUI {
			name += " (gui)"
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if spec.Crash == "" && !spec.Baseline && spec.Compare == nil {
		data.Tests = strings.Join(names, ", ")
	}
	files := slices.Clone(res.Logs)
	for _, t := range res.Tests {
		row := jobTest{Name: t.Name, Verdict: testVerdict(t), Class: "job-bad", Tail: t.Tail}
		switch {
		case t.Passed:
			row.Class = "job-ok"
		case t.Skipped:
			row.Class = "job-dim"
		}
		data.Results = append(data.Results, row)
		for _, f := range t.Files {
			if !slices.Contains(files, f) {
				files = append(files, f)
			}
		}
	}
	data.Files = files
	for _, e := range res.KernelEvents {
		if e.Kind != "warning" {
			data.KernelEvents = append(data.KernelEvents, jobKernelEvent{e.Kind, e.Source, e.Line})
		}
	}
	data.NewErrors = res.NewErrorLines
	if n := len(data.NewErrors); n > newErrorsShown {
		data.NewErrors, data.NewErrorsLeftOut = data.NewErrors[:newErrorsShown], n-newErrorsShown
	}
	if o := res.OMT; o != nil {
		omt := &jobOMT{Tool: o.Tool, Pass: o.Pass, Fail: o.Fail, Skip: o.Skip, ComparedTo: o.ComparedTo}
		for _, group := range []struct {
			tag, class string
			checks     []api.OMTCheck
		}{{"regressed", "job-bad", o.Regressions}, {"fixed", "job-ok", o.Fixed}, {"allowed", "job-dim", o.Allowed}, {"lab boot", "job-dim", o.LabBoot}} {
			for _, c := range group.checks {
				omt.Changed = append(omt.Changed, jobCheck{group.tag, group.class, c.ID, c.Evidence})
			}
		}
		omt.Report = s.reportState(j)
		omt.ReportClass = reportClasses[omt.Report]
		switch omt.Report {
		case api.ReportPublished:
			omt.ReportHref = s.href("/reports/" + reportID(o))
		case api.ReportHidden:
			if p.isMaintainer() {
				omt.ReportHref = s.href("/reports/" + reportID(o))
			}
		case api.ReportIsDeleted:
			if d := o.ReportDeleted; d != nil {
				omt.Deleted = "by " + d.By + ", " + whenDay(d.Time)
			}
		case api.ReportNotPublished:
			omt.Note = o.PublishNote
			omt.CanPublish = done && o.KnownGood && spec.InstalledManifest == "" && spec.Mesa == nil && !s.Preview() && mayPublish(p, j) == nil
		}
		data.OMT = omt
	}
	for _, e := range j.Events {
		data.Events = append(data.Events, jobEvent{whenDay(e.Time), e.Msg})
	}
	page.Data = data
	s.render(w, r, "jobs/show", page)
}

func (s *Server) pJobCancel(w http.ResponseWriter, r *http.Request) {
	j, p := s.jobOr404(w, r)
	if j == nil {
		return
	}
	if err := mayCancel(p, j); err != nil {
		s.renderJob(w, r, j, p, err)
		return
	}
	if !s.sure(w, r, cancelSentence(j), "/jobs/"+j.ID) {
		return
	}
	// A job someone else cancels says who did.
	why := ""
	if j.AccountID != p.AccountID {
		why = "by @" + p.Handle
		s.log.Printf("job %s (%s) of %s: canceled by @%s", j.ID, j.Spec.Device, orStr(j.Maintainer, "labd"), p.Handle)
	}
	s.cancelJob(j.ID, why)
	s.toJob(w, j.ID)
}

func (s *Server) pJobRetry(w http.ResponseWriter, r *http.Request) {
	j, p := s.jobOr404(w, r)
	if j == nil {
		return
	}
	// A job queued on the site has the person's handle as its label, as a
	// Lease taken there has.
	again, code, err := s.retry(p, j.ID, p.Handle)
	if err != nil {
		s.renderJob(w, r, j, p, refuse(code, "%v", err))
		return
	}
	s.toJob(w, again.ID)
}

// pJobPublish publishes a job's report by hand: `lab publish` on the site.
func (s *Server) pJobPublish(w http.ResponseWriter, r *http.Request) {
	j, p := s.jobOr404(w, r)
	if j == nil {
		return
	}
	err := mayPublish(p, j)
	if err == nil && j.State != api.JobDone {
		err = refuse(409, "job %s is still %s", j.ID, j.State)
	}
	if err == nil {
		if _, perr := s.omtPublish(j, p); perr != nil {
			err = refuse(409, "Not published: %v", perr)
		}
	}
	if err != nil {
		if fresh, ferr := s.store.job(j.ID); ferr == nil {
			j = fresh
		}
		s.renderJob(w, r, j, p, err)
		return
	}
	s.toJob(w, j.ID)
}

// doneAt is when a finished job ended: its "done" event. What comes after,
// a Compare published or a Lease's end noted, moves Updated, not the end.
func doneAt(j *api.Job) time.Time {
	for i := len(j.Events) - 1; i >= 0; i-- {
		if strings.HasPrefix(j.Events[i].Msg, "done: ") {
			return j.Events[i].Time
		}
	}
	return j.Updated
}

// toJob sends the browser to a job's page.
func (s *Server) toJob(w http.ResponseWriter, id string) {
	w.Header().Set("Location", s.basePath()+"/jobs/"+id)
	w.WriteHeader(http.StatusSeeOther)
}
