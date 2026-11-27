package server

import (
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /reports", web.Anyone, (*Server).pReports)
	addPage("POST /reports/{id}/hide", web.Maintainers, (*Server).pHideReport)
	addPage("POST /reports/{id}/show", web.Maintainers, (*Server).pShowReport)
	addPage("POST /reports/{id}/delete", web.Maintainers, (*Server).pDeleteReport)
}

type reportsPage struct {
	Bar     filterBarView
	Count   string
	Any     bool
	Reports []runView
	// Every is every report, Hidden ones too, with who uploaded each and the
	// actions on it: a Maintainer's, nil for anyone else.
	Every *everyReport
}

type everyReport struct {
	Count string
	Rows  []maintainerRow
}

// maintainerRow is a report as a Maintainer lists it.
type maintainerRow struct {
	runView
	Short, Uploader string
	Hidden          bool
	Arrived         arrivedView
}

// arrivedView is how a report arrived: by its Mac's own tool, or by the lab
// from a job's run, with who asked for it to be published. Maintainers
// alone are told.
type arrivedView struct {
	Job, JobHref string
	// Asked is who asked, "" when the lab published the report by itself.
	Asked string
	// Mac and Queued are the job's Mac and who queued the job, read from the
	// job as it is kept; both "" when the job is gone.
	Mac, Queued string
}

func (s *Server) arrived(report *reports.Report) arrivedView {
	if !report.ByLab() {
		return arrivedView{}
	}
	v := arrivedView{Job: report.LabJob, JobHref: s.href("/jobs/" + report.LabJob)}
	switch {
	case report.AskedHandle != "":
		v.Asked = "@" + report.AskedHandle
	case report.AskedAccountID != 0:
		v.Asked = "a person whose name is not kept"
	}
	if j, err := s.store.job(report.LabJob); err == nil {
		v.Mac, v.Queued = j.Spec.Device, "the lab"
		if j.Maintainer != "" {
			v.Queued = "@" + j.Maintainer
		}
	}
	return v
}

// maintains reports whether r comes from a Maintainer, who hides, shows and
// deletes reports, reads the Hidden ones and sees who uploaded each.
func (s *Server) maintains(r *http.Request) bool {
	return web.Maintainers.Allows(s.viewerOf(r).Kind)
}

// uploader is who a report's Mac was signed in as when the report came.
func uploader(report *reports.Report) string {
	if report.NameNotKept() {
		return "a person whose name is not kept"
	}
	if report.Handle == "" {
		return "nobody signed in"
	}
	return "@" + report.Handle
}

// everyReport is the Maintainers' list, newest first.
func (s *Server) everyReport(w http.ResponseWriter) *everyReport {
	every, err := s.reports.Every(s.catalogue)
	if err != nil {
		s.pageFailed(w, err)
		return nil
	}
	runs := every.Runs()
	list := &everyReport{Count: reports.Plural(len(runs), "report")}
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		list.Rows = append(list.Rows, maintainerRow{runView: s.runList(every, []*reports.Run{run}, false)[0],
			Short: run.PublicID[:min(8, len(run.PublicID))], Uploader: uploader(run.Report), Hidden: run.Hidden, Arrived: s.arrived(run.Report)})
	}
	return list
}

// pReports is every published run, newest first, that the filters pass:
// ?build= lists the runs on one build.
func (s *Server) pReports(w http.ResponseWriter, r *http.Request) {
	p := s.published(w)
	if p == nil {
		return
	}
	filter := p.Filter(r.URL.Query(), reports.ListKeys)
	all := p.Runs()
	var newest []*reports.Run
	for i := len(all) - 1; i >= 0; i-- {
		newest = append(newest, all[i])
	}
	runs := filter.Runs(newest)
	params := reports.Query(filter.Params(""))
	page := reportsPage{Count: reports.Plural(len(runs), "report"), Any: filter.Any(), Reports: s.runList(p, runs, filter.Confirmed())}
	page.Bar = s.filterBar(filter, p.Options(nil), "/reports", reports.ListKeys, len(runs), nil,
		linkView{"reports.json", s.href("/api/v1/reports.json") + params},
		linkView{"reports.csv", s.href("/api/v1/reports.csv") + params},
		linkView{"checks.csv", s.href("/api/v1/checks.csv") + params})
	if s.maintains(r) {
		if page.Every = s.everyReport(w); page.Every == nil {
			return
		}
	}
	s.render(w, r, "reports/index", web.Page{Title: "Reports", Nav: "reports", Data: page})
}

// A Maintainer's three actions on a report. Hide takes it off everything
// the public reads and Show puts it back; Delete removes it and its
// evidence for good, asked twice. Each writes a record line.

func (s *Server) pHideReport(w http.ResponseWriter, r *http.Request) {
	if report := s.reportAt(w, r); report != nil {
		s.reportChanged(w, r, report, !report.Hidden, s.reports.Hide(report.PublicID, s.now()), "hid the report %s", "/reports#admin-report-"+report.PublicID)
	}
}

func (s *Server) pShowReport(w http.ResponseWriter, r *http.Request) {
	if report := s.reportAt(w, r); report != nil {
		s.reportChanged(w, r, report, report.Hidden, s.reports.Show(report.PublicID, s.now()), "showed the report %s again", "/reports#admin-report-"+report.PublicID)
	}
}

func (s *Server) pDeleteReport(w http.ResponseWriter, r *http.Request) {
	report := s.reportAt(w, r)
	if report == nil || !s.sure(w, r, "Delete this report and its evidence for good", "/reports#admin-report-"+report.PublicID) {
		return
	}
	err := s.reports.Delete(report.PublicID)
	if p := PersonOf(r); err == nil && p != nil {
		s.reportDeleted(report, "@"+p.Handle, p.AccountID)
	}
	s.reportChanged(w, r, report, true, err, "deleted the report %s", "/reports#all-reports")
}

// reportAt is the report an action's address names, Hidden or not; nil when
// there is none and it has answered w.
func (s *Server) reportAt(w http.ResponseWriter, r *http.Request) *reports.Report {
	report, err := s.reports.Report(r.PathValue("id"))
	switch {
	case errors.Is(err, reports.ErrNoReport):
		s.notFound(w, r)
		return nil
	case err != nil:
		s.pageFailed(w, err)
		return nil
	}
	return report
}

// reportChanged answers a Maintainer's action on a report, which ended with
// err: an action that changed something writes its record line, did with
// the report's id and Mac in place of %s, and the Maintainer goes back to
// the list at to.
func (s *Server) reportChanged(w http.ResponseWriter, r *http.Request, report *reports.Report, changed bool, err error, did, to string) {
	switch {
	case errors.Is(err, reports.ErrNoReport):
		s.notFound(w, r)
		return
	case err != nil:
		s.pageFailed(w, err)
		return
	}
	if changed {
		model := strings.TrimPrefix(words(member(report.Document(), "machine", "model")), "Apple ")
		if err := s.record(r, fmt.Sprintf(did, report.PublicID+" ("+model+")"), nil); err != nil {
			s.log.Printf("Reports, %s: %v", r.URL.Path, err)
		}
	}
	http.Redirect(w, r, s.basePath()+to, http.StatusSeeOther)
}

// issueView is a Maintainer's prefilled GitHub issue, opened in a new tab.
type issueView struct{ Repo, URL, Title string }

func issueOf(draft *reports.IssueDraft) *issueView {
	return &issueView{Repo: draft.Repo, URL: draft.URL(), Title: draft.Title}
}

// issues drafts issues for a Maintainer; nil for anyone else, who sees no
// issue link.
func (s *Server) issues(r *http.Request, p *reports.Published) *reports.Issues {
	if r == nil || !web.Maintainers.Allows(s.viewerOf(r).Kind) {
		return nil
	}
	from := s.publicBase(r)
	if u, err := url.Parse(from); err == nil && u.Host != "" {
		from = u.Host
	}
	return p.Issues(from, s.reportURL(r))
}

type reportPage struct {
	Title, PublicID, Model                  string
	Verified                                bool
	Chip, SoC, Board, Storage, Kernel       string
	Stack, CandidateSet, CandidateHref      string
	Build                                   *buildView
	Image                                   string
	Distro, BootLoader, Encryption          string
	ToolName, ToolVersion, CatalogueVersion string
	Uploaded                                string
	Counts                                  reports.Counts
	Unattended                              bool
	// ByLab is whether the lab published the report from a job's run. The
	// page says so and names nobody.
	ByLab     bool
	Packages  []reportPackage
	Sections  []*reportSection
	Inventory *inventoryView
	ModelHref string
	// For a Maintainer alone: whether the report is Hidden, and who its Mac
	// was signed in as ("" for anyone else).
	Hidden   bool
	Uploader string
}

type reportPackage struct{ Name, Version string }

type reportSection struct {
	Name   string
	Checks []reportCheck
}

type regressionView struct{ Href, Title string }

type scoreView struct{ Value, Unit, Tool, Suite, Compare string }

type reportCheck struct {
	ID, StatusClass, StatusLabel string
	Classified, Unconfirmed      bool
	Outcome, OutcomeWords, State string
	Feature, Layer               string
	Regression                   *regressionView
	Issue                        *issueView
	Score                        *scoreView
	Evidence                     string
}

// buildLong is a run's build as its own page says it: the label, linked to
// the reports on it, then its set, runtime, kernel, boot package, when its
// image was built and the tool that ran it.
func (s *Server) buildLong(p *reports.Published, run *reports.Run) *buildView {
	v := s.buildOf(p, run)
	if v == nil {
		return nil
	}
	build := run.Build()
	if build.CandidateSet != "" {
		if p.NamesSet(build.CandidateSet) {
			v.SetPage = s.href("/candidates/" + build.CandidateSet)
		}
		v.Runtime = build.ID
	}
	if build.Kernel != "" {
		v.Extras = append(v.Extras, "linux-aurora "+build.Kernel)
	}
	if build.Boot != "" {
		v.Extras = append(v.Extras, "omarchy-mac-boot "+build.Boot)
	}
	if built := build.Built(); built != "" {
		v.Extras = append(v.Extras, "built "+built)
	}
	if tool := run.ToolVersion(); tool != "" {
		v.Extras = append(v.Extras, "tool "+tool)
	}
	return v
}

// reportPage is what a report's page shows: the machine, its build, each
// check with its result in the catalogue's words, and the hardware
// inventory. r is whose page it is: a Maintainer gets the issue links.
func (s *Server) reportPage(r *http.Request, p *reports.Published, run *reports.Run) reportPage {
	doc := run.Document()
	text := func(path ...string) string { return words(member(doc, path...)) }
	page := reportPage{
		PublicID: run.PublicID, Model: run.Model(), Title: run.ShortModel() + " report", Verified: run.Verified,
		Chip: run.Chip(), SoC: run.SoC(), Board: run.Board(), Storage: run.Storage(), Kernel: run.Kernel(),
		Stack: run.StackWords() + " " + run.OmarchyVersion(), CandidateSet: run.CandidateSet(), Build: s.buildLong(p, run),
		Distro: text("system", "distro"), BootLoader: text("system", "boot_loader"), Encryption: text("system", "encryption"),
		ToolName: text("tool", "name"), ToolVersion: text("tool", "version"), CatalogueVersion: text("catalogue_version"),
		Uploaded: uploadedAtRun(run), Counts: run.ResultCounts(false), Unattended: run.Unattended(), ByLab: run.ByLab(), ModelHref: s.href("/models/" + run.Board()),
	}
	if page.CandidateSet != "" {
		page.CandidateHref = s.href("/candidates/" + page.CandidateSet)
	}
	if build := run.Build(); build != nil {
		page.Image = build.ImageWords()
	}
	packages, _ := member(doc, "system", "packages").([]any)
	for _, pkg := range packages {
		page.Packages = append(page.Packages, reportPackage{words(member(pkg, "name")), words(member(pkg, "version"))})
	}
	issues := s.issues(r, p)
	if r != nil && s.maintains(r) {
		page.Hidden, page.Uploader = run.Hidden, uploader(run.Report)
	}
	if run.Hidden {
		// A Hidden report's link opens for nobody the issue would reach.
		issues = nil
	}
	sections := map[string]*reportSection{}
	for _, check := range run.Checks() {
		c := reportCheck{ID: check.ID, StatusClass: check.Status, StatusLabel: strings.ToUpper(check.Status), Outcome: check.Outcome}
		if check.ExpectedGap() {
			c.StatusClass, c.StatusLabel = "gap", "GAP"
			if check.Outcome == "not-applicable" {
				c.StatusLabel = "N/A"
			}
		}
		if check.Classified() {
			c.Classified, c.Unconfirmed = true, check.AnsweredByDefault
			c.OutcomeWords, c.State = s.catalogue.OutcomeWords(check.Outcome), reports.StateOfOutcome(check.Outcome)
			c.Feature, c.Layer = s.catalogue.FeatureName(check.Feature), check.Layer
			if check.Outcome == "fails" && run.Regression(check.Feature) {
				pass := run.RegressedFrom(check.Feature)
				c.Regression = &regressionView{Href: s.href(reportPath(pass)),
					Title: "An earlier Verified run on this model and stack found it working (" + pass.StackWords() + " " + pass.OmarchyVersion() + ", " + uploadedAtRun(pass) + ")"}
			}
			if issues != nil && reports.WorthAnIssue(check) {
				c.Issue = issueOf(issues.ForCheck(run, check))
			}
		}
		if score := check.Score(); score != nil {
			value, _ := score.Get("value")
			suite := words(member(score, "suite"))
			c.Score = &scoreView{Value: reports.ScoreWords(value), Unit: reports.UnitWords(words(member(score, "unit"))), Tool: words(member(score, "tool")), Suite: suite,
				Compare: s.href("/models/"+run.Board()) + "#benchmarks"}
		}
		c.Evidence = strings.Join(check.Evidence(), "\n")
		name, _, _ := strings.Cut(check.ID, ".")
		section := sections[name]
		if section == nil {
			section = &reportSection{Name: name}
			sections[name] = section
			page.Sections = append(page.Sections, section)
		}
		section.Checks = append(section.Checks, c)
	}
	if inventory, ok := member(doc, "inventory").(*reports.Object); ok {
		page.Inventory = s.inventory(inventory)
	}
	return page
}

type inventoryEntry struct {
	Compatible, Nodes     string
	Outcome, OutcomeWords string
	Feature, Layer        string
}

type configDifference struct{ Option, Asahi, Kernel string }

type kernelConfigView struct {
	Options, Reference, Listed string
	Differences                []configDifference
}

// inventoryView is the report's hardware inventory: what no driver claimed
// (unknown hardware first) and the kernel's build options against Asahi's.
type inventoryView struct {
	HasNodes                                            bool
	Nodes, Bound, None, Containers, Disabled, Unclaimed int
	Unknown, Known                                      []inventoryEntry
	Config                                              *kernelConfigView
}

func whole(value any) int {
	if n, ok := value.(*big.Int); ok && n.IsInt64() {
		return int(n.Int64())
	}
	return 0
}

func (s *Server) inventory(inventory *reports.Object) *inventoryView {
	v := &inventoryView{}
	if nodes, ok := member(inventory, "nodes").([]any); ok {
		v.HasNodes = true
		states := map[string]int{}
		for _, node := range nodes {
			state := "disabled"
			if words(member(node, "status")) == "okay" {
				state = words(member(node, "driver"))
			}
			states[state] += whole(member(node, "count"))
			v.Nodes += whole(member(node, "count"))
		}
		unclaimed, _ := member(inventory, "unclaimed").([]any)
		for _, entry := range unclaimed {
			outcome := words(member(entry, "outcome"))
			e := inventoryEntry{Compatible: words(member(entry, "compatible")), Nodes: reports.Plural(whole(member(entry, "count")), "node"),
				Outcome: outcome, OutcomeWords: s.catalogue.OutcomeWords(outcome),
				Feature: s.catalogue.FeatureName(words(member(entry, "feature"))), Layer: words(member(entry, "layer"))}
			v.Unclaimed += whole(member(entry, "count"))
			if outcome == "unknown-hardware" {
				v.Unknown = append(v.Unknown, e)
			} else {
				v.Known = append(v.Known, e)
			}
		}
		v.Bound, v.None, v.Disabled = states["bound"], states["none"], states["disabled"]
		v.Containers = max(0, states["unbound"]-v.Unclaimed)
	}
	if config, ok := member(inventory, "kernel_config").(*reports.Object); ok {
		differences, _ := member(config, "differences").([]any)
		omitted := whole(member(config, "omitted"))
		c := &kernelConfigView{Options: reports.Plural(len(differences)+omitted, "option"), Reference: words(member(config, "reference"))}
		if omitted > 0 {
			c.Listed = fmt.Sprintf(" (the first %d are listed)", len(differences))
		}
		for _, difference := range differences {
			c.Differences = append(c.Differences, configDifference{words(member(difference, "option")), words(member(difference, "asahi")), words(member(difference, "kernel"))})
		}
		v.Config = c
	}
	return v
}
