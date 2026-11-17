package server

// My Macs: one card per Mac a person lends, with everything its Volunteer
// may see and do. Above the cards: an unused Join command with its expiry
// and Cancel, "Lend another Mac, n of 5", and the place of the banner that
// asks for changed terms to be accepted. An External volunteer has the page
// as My Macs; an M team member or a Maintainer has the same cards under
// "Mine" on Macs. Nobody is shown a Mac they do not lend.
//
// The card gives the weekly window, which the Mac takes at its next
// check-in; otherwise it only takes away: Pause, Limit and Leave are its
// buttons (see mac_controls.go), and Delete beside each report the lab
// published from the Mac. What only the Mac can give is shown as the
// command to type there.

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
	"github.com/omacom/omarchy-maclab/internal/window"
)

func init() {
	addPage("GET /my-macs", web.SignedIn, (*Server).pMyMacs)
	addPage("GET /my-macs/{name}", web.SignedIn, (*Server).pMyMac)
	addPage("POST /my-macs/{name}/reports/{id}/delete", web.SignedIn, (*Server).pDeleteMyReport)
}

type myMacsPage struct {
	// Flash is a refusal of one of a card's buttons.
	Flash string
	Mine  *myMacs
}

// myMacs is what a person lends, as both pages show it.
type myMacs struct {
	// Terms is the banner of changed terms; nil while the terms are the
	// ones the person accepted.
	Terms *changedTerms
	// Unused is the person's unused Join command, without its line.
	Unused *lendCommand
	// Leave is whether the person has leave to lend. Lends counts toward
	// Cap, which is 0 for a Maintainer; AtCap leaves no room for another.
	Leave      bool
	Lends, Cap int
	AtCap      bool
	Cards      []macCard
	// Left are the person's Macs whose enrollment ended, newest first.
	Left []leftMac
}

// changedTerms is the terms a Volunteer has not accepted yet.
type changedTerms struct{ Date, SHA256 string }

// changedTermsFor is the banner My Macs shows a Volunteer whose Macs wait
// for the lab's changed terms, with Accept (terms.go); nil when none does.
func (s *Server) changedTermsFor(p Person) *changedTerms {
	date := s.termsWaiting(p)
	if date == "" {
		return nil
	}
	return &changedTerms{Date: date, SHA256: s.termsText().SHA256}
}

type macCard struct {
	Name, Page    string
	Configuration string
	// State is the Mac's state in plain words and Reason what the lab adds
	// to it; Hands the steps of a Mac that Needs hands.
	State, Reason string
	Hands         *cardHands
	// Schedule is the weekly window as the Mac holds it, Zone the Mac's own
	// time zone and Window what holds now. Rules are the windows to remove
	// on the card, and Pending the schedule set here that the Mac does not
	// hold yet ("none" for no window).
	Schedule, Zone, Window string
	Rules                  []string
	Pending                string
	// PausedHere and Limited are what the site took and can give back.
	PausedHere, Limited bool
	AccessLevel         string
	Publishing          bool
	Eligible            bool
	Topic               string
	// TopicRefused is why labd refuses the Volunteer's own topic.
	TopicRefused string
	JobNotices   bool
	// Holder is who holds its Lease, HolderKind their kind, Until its end.
	Holder, HolderKind, HolderClass, Until string
	// Agent is the Mac's lab-agent; LabAgent the lab's when the Mac's is
	// older, and UpdateLine what its Volunteer runs at the Mac then.
	Agent, LabAgent, UpdateLine string
	Jobs                        []macJobRow
	Reports                     []cardReport
	Record, Audit, Exec         []recordShownLine
	Commands                    []cardCommand
	LeaveSentence               string
}

type cardHands struct {
	Title, Why, LastLog, Since string
	Steps                      []string
}

// cardReport is a report the lab published from one of the Mac's jobs.
type cardReport struct {
	ID, Job, When string
	Hidden        bool
}

// cardCommand is something only the Mac can give, as the command typed there.
type cardCommand struct{ Line, What string }

// cardLines is one record on a card: its newest lines, where all of it is,
// and what is said when it is empty.
type cardLines struct {
	Lines     []recordShownLine
	All, None string
}

func (c macCard) Lines(lines []recordShownLine, none string) cardLines {
	return cardLines{Lines: lines, All: c.Page + "?tab=" + tabRecords, None: none}
}

// leftMac is a Mac of the person's whose enrollment ended.
type leftMac struct {
	Name, When, Who string
	// Anchor is the place the link of a Notice opens, unless a Mac enrolled
	// under the name since has it.
	Anchor string
}

// How many jobs and how many lines of each record a card shows; the rest is
// on the Mac's own page.
const (
	cardJobsShown  = 5
	cardLinesShown = 5
	leftMacsShown  = 20
)

// onlyAtTheMac are the commands for what only the Mac can give.
var onlyAtTheMac = []cardCommand{
	{"sudo lab-agent window add mon-fri 19:00-07:00", "the weekly Availability window at the Mac, as the form above sets it here, or window add always (window show lists it, window remove takes one away)"},
	{"sudo lab-agent open --until 23:00", "Open now: open to the lab from this moment until that time"},
	{"sudo lab-agent access full", "raise the Access level to Full: also commands and scripts as root (access qualification lowers it)"},
	{"sudo lab-agent publish on", "let the lab publish this Mac's reports; it lists each one's deletion link (publish off stops it)"},
	{"sudo lab-agent notify --topic https://ntfy.sh/a-topic-of-yours", "send this Mac's Notices to a topic of your own"},
	{"sudo lab-agent notify --jobs on", "a Notice when a job starts here and when it is done"},
	{"sudo lab-agent update", "take the lab's newest lab-agent: the lab never updates its own software here"},
	{"sudo lab-agent leave", "leave the lab from the Mac itself, as Leave does here"},
}

// myMacsPath is where p's cards are: My Macs, or "Mine" on Macs for the M
// team and up.
func myMacsPath(p Person) string {
	if web.MTeamUp.Allows(p.Role()) {
		return macsPath(showMine)
	}
	return "/my-macs"
}

// myMacLink is the link a Notice about the Mac called name carries: it
// opens that Mac's card for its Volunteer, once signed in.
func (s *Server) myMacLink(base, name string) string {
	return base + "/my-macs/" + url.PathEscape(name)
}

func (st *store) retiredMacsOf(volunteer int64, limit int) ([]RetiredMac, error) {
	rows, err := st.db.Query(`SELECT id, name, retired, how FROM retired_macs WHERE volunteer_id = ? ORDER BY retired DESC, id LIMIT ?`, volunteer, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RetiredMac
	for rows.Next() {
		m := RetiredMac{VolunteerID: volunteer}
		var at int64
		if err := rows.Scan(&m.ID, &m.Name, &at, &m.How); err != nil {
			return nil, err
		}
		m.Retired = time.UnixMicro(at)
		out = append(out, m)
	}
	return out, rows.Err()
}

// myMacsOf is what p lends and lent.
func (s *Server) myMacsOf(p Person) (*myMacs, error) {
	s.expireJoinCommands()
	l, err := s.lendsOf(p)
	if err != nil {
		return nil, err
	}
	m := &myMacs{Terms: s.changedTermsFor(p), Leave: p.MayLend(), Lends: l.Count(), Cap: l.Cap, AtCap: l.Cap != 0 && len(l.Macs) >= l.Cap}
	if l.Unused != nil {
		m.Unused = &lendCommand{Name: l.Unused.Name, Expires: utcMinute(l.Unused.Expires)}
	}
	enrolled := map[string]bool{}
	for _, name := range l.Macs {
		if v := s.dev(name); v != nil && s.lends(v, p) == nil {
			m.Cards = append(m.Cards, s.macCard(v, p))
			enrolled[name] = true
		}
	}
	left, err := s.store.retiredMacsOf(p.AccountID, leftMacsShown)
	if err != nil {
		return nil, err
	}
	for _, r := range left {
		row := leftMac{Name: r.Name, When: whenDay(r.Retired), Who: "you left with it"}
		switch r.How {
		case leftByMaintainer:
			row.Who = "a Maintainer ended its enrollment"
		case leftBlocked:
			row.Who = "its enrollment ended when you were Blocked"
		}
		if !enrolled[r.Name] {
			row.Anchor, enrolled[r.Name] = "mac-"+r.Name, true
		}
		m.Left = append(m.Left, row)
	}
	return m, nil
}

// empty is whether there is nothing of the person's to show: no Mac, none
// that left and no Join command.
func (m *myMacs) empty() bool { return len(m.Cards) == 0 && len(m.Left) == 0 && m.Unused == nil }

// macCard is the card of v for p, its Volunteer.
func (s *Server) macCard(v *dev, p Person) macCard {
	// The Mac as its Volunteer sees it: with what is their own.
	d := s.view(v)
	row := s.macRow(v, d, p, s.holdsLease(v, p))
	c := macCard{Name: d.Name, Page: "/macs/" + url.PathEscape(d.Name), Configuration: row.Configuration, State: row.State, Reason: d.StateReason,
		Limited: d.Limited, Publishing: d.Publish, Eligible: d.SnapshotEligible, Topic: s.macTopicURL(d.Enrollment), TopicRefused: d.NoticeTopicRefused, JobNotices: d.JobNotices,
		Holder: row.Holder, HolderKind: row.HolderKind, HolderClass: row.HolderClass, Until: row.Until,
		Agent: d.Facts.AgentVersion, LabAgent: d.LabAgent, UpdateLine: agentUpdateLine(d), Commands: onlyAtTheMac, LeaveSentence: leaveSentence(d.Name)}
	// Until its first baseline has passed, the stage says what the Mac
	// waits for; labd's own note then is a Maintainer's.
	if d.Arrival != "" && d.Arrival != ArrivalReady {
		c.Reason = ""
	}
	if a := d.Action; a != nil && d.State == api.StateNeedsHands {
		c.Hands = &cardHands{Title: a.Title, Why: a.Why, LastLog: a.LastLog, Steps: a.Steps}
		if !a.Since.IsZero() {
			c.Hands.Since = whenDay(a.Since)
		}
	}
	switch {
	case d.AccessLevel == api.AccessFull && d.Limited:
		c.AccessLevel = "Qualification here, by your Limit; Full on the Mac"
	case d.AccessLevel == api.AccessFull:
		c.AccessLevel = "Full"
	case d.Limited:
		c.AccessLevel = "Qualification, and held there by your Limit"
	default:
		c.AccessLevel = "Qualification"
	}
	c.Schedule, c.Zone, c.Window, c.PausedHere = s.cardWindow(v)
	c.Rules, c.Pending = cardRules(d.Device)

	jobs, err := s.store.jobs(d.Name, 1000, false)
	if err != nil {
		s.log.Printf("%s's jobs for its card: %v", d.Name, err)
	}
	for _, j := range jobs {
		// A name may have been another Volunteer's Mac before.
		if j.DeviceID == "" || j.DeviceID != d.ID {
			continue
		}
		if len(c.Jobs) < cardJobsShown {
			job := macJobRow{ID: j.ID, What: jobWhat(j), By: j.Maintainer, State: string(j.State), When: whenDay(j.Created)}
			if j.State == api.JobDone {
				job.State = strings.ReplaceAll(j.Outcome, "_", " ")
			}
			c.Jobs = append(c.Jobs, job)
		}
		if id, state := s.labReportOf(j); id != "" {
			c.Reports = append(c.Reports, cardReport{ID: id, Job: j.ID, When: whenDay(j.Created), Hidden: state == api.ReportHidden})
		}
	}
	c.Record = first(s.macRecord(d), cardLinesShown)
	c.Audit = s.auditShown(d.Name, cardLinesShown)
	if events, err := s.execRecordOf(p, v, cardLinesShown); err != nil {
		s.log.Printf("the Exec record of %s for its card: %v", d.Name, err)
	} else {
		for _, e := range events {
			c.Exec = append(c.Exec, recordShownLine{whenDay(e.Time), e.Line()})
		}
	}
	return c
}

func first[T any](s []T, n int) []T { return s[:min(n, len(s))] }

// labReportOf is the report the lab published from j that is still there,
// Published or Hidden, and which of the two; "" for a job with none.
func (s *Server) labReportOf(j *api.Job) (id, state string) {
	o := j.Result.OMT
	if o == nil {
		return "", ""
	}
	if state = s.reportState(j); state != api.ReportPublished && state != api.ReportHidden {
		return "", ""
	}
	return reportID(o), state
}

// cardWindow is a Volunteer's Mac's Availability window for its card: the
// weekly schedule as typed at the Mac, the Mac's own zone, what holds now in
// words, and whether a Pause taken on the site holds.
func (s *Server) cardWindow(v *dev) (schedule, zone, now string, pausedHere bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	settings, st := v.windowStateLocked()
	at := s.now()
	when := func(t time.Time) string { return t.In(settings.Location()).Format("Mon 2 Jan 15:04 MST") }
	schedule, zone = settings.Schedule(), settings.Zone
	switch p := v.d.SitePause; {
	case p.Active(at) && p.Until.IsZero():
		return schedule, zone, "Paused here until you Resume", true
	case p.Active(at):
		return schedule, zone, "Paused here until " + when(p.Until), true
	case st.Always:
		now = "open always"
	case st.Open:
		now = "open until " + when(st.End)
	case settings.PausedUntil.After(at):
		now = "Paused at the Mac until " + when(settings.PausedUntil)
	case !st.Next.IsZero():
		now = "closed until " + when(st.Next)
	case schedule != "":
		now = "closed"
	default:
		now = "The Mac takes nothing until it has one."
	}
	if st.Open && v.d.Health.OnBattery {
		now += ", on battery: jobs start only on AC power"
	}
	return schedule, zone, now, false
}

// cardRules are the weekly windows the card offers to remove, what waits
// for the Mac when there is a change set here, else what the Mac holds; and
// that change in words.
func cardRules(d api.Device) (rules []string, pending string) {
	var rs []window.Rule
	switch {
	case d.WindowWanted != nil:
		rs = d.WindowWanted.Rules
		if pending = (window.Settings{Rules: rs}).Schedule(); pending == "" {
			pending = "none"
		}
	case d.Window != nil:
		rs = d.Window.Rules
	}
	for _, r := range rs {
		rules = append(rules, r.String())
	}
	return rules, pending
}

func (s *Server) pMyMacs(w http.ResponseWriter, r *http.Request) {
	if p := leaser(r); web.MTeamUp.Allows(p.Role()) {
		http.Redirect(w, r, s.basePath()+myMacsPath(p), http.StatusSeeOther)
		return
	}
	s.renderMyMacs(w, r, nil)
}

// renderMyMacs writes the cards of the person signed in on r, where they
// have them; with a refusal of one of a card's buttons, it is said above
// them under its own status.
func (s *Server) renderMyMacs(w http.ResponseWriter, r *http.Request, refused error) {
	p := leaser(r)
	if web.MTeamUp.Allows(p.Role()) {
		s.renderMacs(w, r, showMine, refused)
		return
	}
	mine, err := s.myMacsOf(p)
	if err != nil {
		s.pageFailed(w, err)
		return
	}
	// Row 2 gives My Macs to who lends a Mac. It is also there for who lent
	// one, or has a Join command on its way to one.
	if mine.empty() && refused == nil {
		s.notFound(w, r)
		return
	}
	page := web.Page{Title: "My Macs", Nav: "my-macs"}
	data := myMacsPage{Mine: mine}
	if refused != nil {
		data.Flash, page.Status = refused.Error(), statusOf(refused)
	}
	page.Data = data
	s.render(w, r, "my_macs", page)
}

// pMyMac is the link of a Notice: it leads to the Mac's card. It says
// nothing of whether a Mac of that name is there.
func (s *Server) pMyMac(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p := leaser(r)
	if !reName.MatchString(name) {
		s.notFound(w, r)
		return
	}
	if !web.MTeamUp.Allows(p.Role()) {
		if mine, err := s.myMacsOf(p); err != nil || mine.empty() {
			s.notFound(w, r)
			return
		}
	}
	http.Redirect(w, r, s.basePath()+myMacsPath(p)+"#mac-"+name, http.StatusSeeOther)
}

// pDeleteMyReport is Delete beside a report on a card: the Mac's Volunteer
// deletes, for good, a report the lab published from their Mac. It asks
// twice. The job then says who deleted its report and when.
func (s *Server) pDeleteMyReport(w http.ResponseWriter, r *http.Request) {
	v := s.myMac(w, r)
	if v == nil {
		return
	}
	d, id, p := v.snapshot(), r.PathValue("id"), leaser(r)
	jobs, err := s.store.jobs(d.Name, 1000, false)
	if err != nil {
		s.pageFailed(w, err)
		return
	}
	// Only a report the lab published from a job of this enrollment.
	from := ""
	for _, j := range jobs {
		if j.DeviceID == "" || j.DeviceID != d.ID {
			continue
		}
		if published, _ := s.labReportOf(j); published != "" && published == id {
			from = j.ID
		}
	}
	if from == "" {
		s.notFound(w, r)
		return
	}
	if !s.sure(w, r, "Delete this report and its evidence for good", myMacsPath(p)) {
		return
	}
	report, err := s.reports.Report(id)
	if err == nil {
		err = s.reports.Delete(id)
	}
	switch {
	case errors.Is(err, reports.ErrNoReport):
		s.notFound(w, r)
		return
	case err != nil:
		s.pageFailed(w, err)
		return
	}
	s.reportDeleted(report, "the Mac's Volunteer", p.AccountID)
	if err := s.record(r, fmt.Sprintf("deleted the report %s, which the lab published from job %s on their Mac %s", id, from, d.Name), nil); err != nil {
		s.log.Printf("My Macs, %s: %v", r.URL.Path, err)
	}
	http.Redirect(w, r, s.basePath()+myMacsPath(p)+"#mac-"+d.Name, http.StatusSeeOther)
}
