package server

// Releases, for the M team and Maintainers: every Release whose manifest is
// frozen, newest first, and one opened in place (?open=<name>): what it is,
// its Macs, its verdict history. An M team member records the first
// verdict; a Maintainer also changes it, revokes it and promotes a Mac from
// its passed Installed run. Each asks twice: the first post answers with
// the page and the sentence the lab would confirm, the second, sent with
// sure=yes, does it. The page starts nothing: freezing a Release and
// starting an Installed run stay in `lab`.
//
// A visitor sees one line of all this, on Candidates, beside the candidate
// set a Release's packages match (labVerdicts).

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /releases", web.MTeamUp, (*Server).pReleases)
	addPage("POST /releases/{release}/verdict", web.MTeamUp, (*Server).pVerdict)
	addPage("POST /releases/{release}/macs/{mac}/promote", web.Maintainers, (*Server).pPromote)
}

type releaseSet struct{ Name, Path string }

type releaseRow struct {
	Name, Path         string // Path opens the row, or shuts it when it is open
	FrozenBy, FrozenAt string
	State              stateBadge
	StateDay           string
	Coverage, Promoted string
	Sets               []releaseSet
	Open               *releaseOpen
}

type stateBadge struct{ Class, Words string }

type releaseMacRow struct {
	ID, Mac, Configuration string
	Run, RunPath, RunWords string
	RunOutcome             string
	Runs                   int
	Promoted               string // who promoted it and when; "" when it is not promoted
	Mark                   string
	PromoteAction          string // "" when the viewer has no Promote here
	Never                  string // why this Mac is never promoted
}

type verdictLine struct {
	State          stateBadge
	By, When, Note string
}

// releaseAsking is the second time of asking, in place: the sentence the
// lab would confirm, as the button that confirms it.
type releaseAsking struct {
	Action, Sentence, Back string
	Fields                 []web.Field
}

// releaseOpen is the inside of an opened row.
type releaseOpen struct {
	Kernel        string
	Packages      []api.ReleasePackage
	ApprovedBy    []string
	Coverage      string
	Macs          []releaseMacRow
	Warnings      []string
	History       []verdictLine
	VerdictAction string
	// Record is the first verdict, or one after a revoke; Change is a
	// Maintainer's over a verdict that stands, with Revoke beside it.
	Record, Change bool
	// Told is why the viewer has no button.
	Told   string
	Asking *releaseAsking
}

type releasesData struct {
	Refused string
	Rows    []releaseRow
}

func stateBadgeOf(state string) stateBadge {
	class := map[string]string{api.ReleaseQualified: "qualified", api.ReleaseNotQualified: "not-qualified", api.ReleaseRevoked: "revoked"}[state]
	if class == "" {
		class = "none"
	}
	return stateBadge{Class: "state-" + class, Words: stateWords(state)}
}

// runWords says how an Installed run ended, as the page does: passed,
// failed or incomplete; a run still going says where it is.
func runWords(outcome string) string {
	switch outcome {
	case api.OutcomePass:
		return "passed"
	case api.OutcomeCanceled:
		return "canceled"
	case "", api.OutcomeInfra, api.OutcomeBuildFailed, api.OutcomeIncomplete, api.OutcomeUnknown:
		return "incomplete"
	case string(api.JobQueued), string(api.JobBuilding), string(api.JobStaging), string(api.JobBooting), string(api.JobTesting),
		string(api.JobCollecting), string(api.JobRestoring):
		return outcome
	}
	return "failed"
}

func releasePath(name string) string { return "/releases?open=" + url.QueryEscape(name) }

// packagesOf is a Release's packages, name to version. A Release stored
// with one package named twice (before the freeze refused that) has none,
// so it links no candidate set.
func packagesOf(r *api.Release) map[string]string {
	packages := map[string]string{}
	for _, pkg := range r.Packages {
		if _, seen := packages[pkg.Name]; seen {
			return nil
		}
		packages[pkg.Name] = pkg.Version
	}
	return packages
}

func (s *Server) pReleases(w http.ResponseWriter, r *http.Request) {
	s.showReleases(w, r, http.StatusOK, r.URL.Query().Get("open"), "", nil)
}

// showReleases renders Releases with the Release called open opened, a
// refusal said at its top when there is one, and the second time of asking
// in the open Release when there is one.
func (s *Server) showReleases(w http.ResponseWriter, r *http.Request, status int, open, refused string, asking *releaseAsking) {
	me := PersonOf(r)
	all, err := s.store.releases("")
	for i := range all {
		if err == nil {
			err = s.fillRelease(&all[i])
		}
	}
	if err != nil {
		s.log.Printf("Releases: %v", err)
		httpErr(w, 500, "The lab couldn't read its Releases.")
		return
	}
	published, err := s.reports.Published(s.catalogue)
	if err != nil {
		s.log.Printf("Releases: the candidate sets: %v", err)
	}
	page := releasesData{Refused: refused}
	for i := range all {
		rel := &all[i]
		row := releaseRow{Name: rel.Name, Path: releasePath(rel.Name), FrozenBy: rel.FrozenBy, State: stateBadgeOf(rel.State),
			Coverage: fmt.Sprintf("%d of %d %s", rel.Passed, rel.Configurations, plural(rel.Configurations, "Configuration", "Configurations")),
			Promoted: "0"}
		if !rel.FrozenAt.IsZero() && rel.FrozenAt.Unix() > 0 {
			row.FrozenAt = utcMinute(rel.FrozenAt)
		}
		if rel.StateAt != nil {
			row.StateDay = dayOf(*rel.StateAt)
		}
		if rel.Promoted > 0 {
			row.Promoted = fmt.Sprintf("%d %s", rel.Promoted, plural(rel.Promoted, "Mac", "Macs"))
		}
		if published != nil {
			for _, set := range published.SetsCarrying(packagesOf(rel)) {
				row.Sets = append(row.Sets, releaseSet{Name: set, Path: "/candidates/" + set})
			}
		}
		if rel.Name == open {
			row.Path = "/releases"
			row.Open = s.openRelease(me, rel)
			row.Open.Asking = asking
		}
		page.Rows = append(page.Rows, row)
	}
	s.render(w, r, "releases", web.Page{Title: "Releases", Nav: "releases", Status: status, Data: page})
}

func (s *Server) openRelease(me *Person, rel *api.Release) *releaseOpen {
	o := &releaseOpen{Kernel: rel.Kernel, Packages: rel.Packages, ApprovedBy: rel.ApprovedBy, Warnings: rel.Warnings,
		VerdictAction: "/releases/" + url.PathEscape(rel.Name) + "/verdict",
		Coverage:      fmt.Sprintf("%d of %d %s a passed run", rel.Passed, rel.Configurations, plural(rel.Configurations, "Configuration has", "Configurations have"))}
	for _, m := range rel.Macs {
		row := releaseMacRow{ID: "release-" + rel.Name + "-mac-" + m.Mac, Mac: m.Mac, Configuration: m.Configuration, Runs: m.Runs}
		if m.LatestRun != "" {
			row.Run, row.RunPath, row.RunWords, row.RunOutcome = m.LatestRun, "/jobs/"+url.PathEscape(m.LatestRun), runWords(m.LatestOutcome), m.LatestOutcome
		}
		if p := m.Promoted; p != nil {
			row.Promoted, row.Mark = "promoted by @"+p.Handle+" "+utcMinute(p.Time), p.Mark
		}
		switch v := s.dev(m.Mac); {
		case v != nil && v.snapshot().IsVolunteer():
			row.Never = "a Volunteer's Mac is never promoted"
		case me.isMaintainer() && m.PassedRun != "" && (m.Promoted == nil || m.Promoted.Job != m.PassedRun):
			row.PromoteAction = "/releases/" + url.PathEscape(rel.Name) + "/macs/" + url.PathEscape(m.Mac) + "/promote"
		}
		o.Macs = append(o.Macs, row)
	}
	for _, v := range rel.Verdicts {
		o.History = append(o.History, verdictLine{State: stateBadgeOf(v.State), By: v.Handle, When: utcMinute(v.Time), Note: v.Note})
	}
	switch stands := rel.State == api.ReleaseQualified || rel.State == api.ReleaseNotQualified; {
	case len(rel.Verdicts) == 0:
		o.Record = true
	case me.isMaintainer():
		o.Record, o.Change = !stands, stands
	case stands:
		o.Told = "Only a Maintainer changes or revokes it."
	default:
		o.Told = "Only a Maintainer records a verdict again after a revoke."
	}
	return o
}

// formFields are the fields of r's form that the second time of asking
// sends again.
func formFields(r *http.Request, names ...string) []web.Field {
	var fields []web.Field
	for _, name := range names {
		fields = append(fields, web.Field{Name: name, Value: r.PostFormValue(name)})
	}
	return fields
}

// releaseDone answers an action on Releases: back to the page with the
// Release open, or the page itself saying why the lab refused.
func (s *Server) releaseDone(w http.ResponseWriter, r *http.Request, release string, err error) {
	var no *refusal
	switch {
	case err == nil:
		http.Redirect(w, r, s.basePath()+releasePath(release), http.StatusSeeOther)
	case errors.As(err, &no) && no.code == http.StatusNotFound:
		s.notFound(w, r)
	case errors.As(err, &no):
		s.showReleases(w, r, no.code, release, no.msg, nil)
	default:
		s.log.Printf("Releases, %s: %v", r.URL.Path, err)
		s.showReleases(w, r, http.StatusInternalServerError, release, "The lab couldn't do that.", nil)
	}
}

// pVerdict records a verdict on a Release: Record, Change and Revoke.
func (s *Server) pVerdict(w http.ResponseWriter, r *http.Request) {
	release := r.PathValue("release")
	req := api.VerdictRequest{State: r.PostFormValue("state"), Note: r.PostFormValue("note"), Confirm: r.PostFormValue("sure") == "yes"}
	a, err := s.verdict(*PersonOf(r), release, req)
	if err == nil && !req.Confirm {
		s.showReleases(w, r, http.StatusOK, release, "", &releaseAsking{Action: "/releases/" + url.PathEscape(release) + "/verdict",
			Sentence: a.Confirm, Back: releasePath(release), Fields: formFields(r, "state", "note")})
		return
	}
	s.releaseDone(w, r, release, err)
}

// pPromote is Promote on a Mac's row: the Mac's latest passed Installed run
// of the Release becomes its reference and Known-good.
func (s *Server) pPromote(w http.ResponseWriter, r *http.Request) {
	release, mac := r.PathValue("release"), r.PathValue("mac")
	rel, err := s.releaseNow(release)
	if err != nil {
		s.releaseDone(w, r, release, err)
		return
	}
	var of *api.ReleaseMac
	for i := range rel.Macs {
		if rel.Macs[i].Mac == mac {
			of = &rel.Macs[i]
		}
	}
	if of == nil {
		s.notFound(w, r)
		return
	}
	if v := s.dev(mac); of.PassedRun == "" && (v == nil || !v.snapshot().IsVolunteer()) {
		s.releaseDone(w, r, release, refuse(http.StatusConflict, "%s has no passed Installed run of Release %s: a Promotion needs one.", mac, release))
		return
	}
	req := promoteReq{Device: mac, Job: of.PassedRun, Confirm: r.PostFormValue("sure") == "yes"}
	out, err := s.promotion(r.Context(), *PersonOf(r), release, req)
	// The Release and its Mac are there: what the Promotion lacks is said
	// on the page, never as a page that is not found.
	if no := (*refusal)(nil); errors.As(err, &no) && no.code == http.StatusNotFound {
		err = refuse(http.StatusConflict, "%s", no.msg)
	}
	if sentence, _ := out["confirm"].(string); err == nil && !req.Confirm {
		s.showReleases(w, r, http.StatusOK, release, "", &releaseAsking{Action: "/releases/" + url.PathEscape(release) + "/macs/" + url.PathEscape(mac) + "/promote",
			Sentence: sentence, Back: releasePath(release)})
		return
	}
	s.releaseDone(w, r, release, err)
}

// judgedReleases are the Releases that have a verdict, each with its
// packages and the verdict that stands: the newest line of its history.
func (st *store) judgedReleases() ([]api.Release, error) {
	rows, err := st.db.Query(`SELECT r.name, r.packages, v.state, v.at FROM releases r JOIN release_verdicts v ON v.release = r.name
		WHERE v.id = (SELECT max(id) FROM release_verdicts WHERE release = r.name)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.Release
	for rows.Next() {
		var r api.Release
		var packages string
		var at int64
		if err := rows.Scan(&r.Name, &packages, &r.State, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(packages), &r.Packages); err != nil {
			return nil, fmt.Errorf("Release %s: %w", r.Name, err)
		}
		when := time.UnixMicro(at)
		r.StateAt = &when
		out = append(out, r)
	}
	return out, rows.Err()
}

// labVerdict is the one line a visitor reads of a Release's verdict: the
// state and its date, never who, why or on which Mac.
func labVerdict(state string, at time.Time) string {
	switch state {
	case api.ReleaseQualified:
		return "Qualified by the lab, " + dayOf(at)
	case api.ReleaseNotQualified:
		return "Not qualified, " + dayOf(at)
	case api.ReleaseRevoked:
		return "Qualification revoked, " + dayOf(at)
	}
	return ""
}

// labVerdicts is the lab's line for each candidate set a Release with a
// verdict matches, by the set's name. A set two Releases match carries the
// verdict recorded last. A Release matching no set is nowhere in it, and
// when the Releases cannot be read the page goes without its lines.
func (s *Server) labVerdicts(p *reports.Published) map[string]string {
	judged, err := s.store.judgedReleases()
	if err != nil {
		s.log.Printf("Candidates: the Releases' verdicts: %v", err)
		return nil
	}
	sort.SliceStable(judged, func(i, j int) bool { return judged[i].StateAt.Before(*judged[j].StateAt) })
	lines := map[string]string{}
	for i := range judged {
		for _, set := range p.SetsCarrying(packagesOf(&judged[i])) {
			lines[set] = labVerdict(judged[i].State, *judged[i].StateAt)
		}
	}
	return lines
}
