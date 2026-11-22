package server

// A Mac's page: Overview (its facts, its Lease and the Lease's buttons, its
// jobs, the way to "Boot a kernel"), Live (its screen and its logs) and
// Records (the Lease record, the Mac's record, the Audit log, the Exec
// record). It is for whoever sees the Mac: the M team and the Maintainers
// any Mac, a Volunteer their own. Each part is shown to who may see it: an
// M team member has Live only under their own Lease, a Volunteer's Live is
// view only, and the Audit log is for Maintainers and the Mac's Volunteer.
// There is no serial console here.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /macs/{name}", web.SignedIn, (*Server).pMac)
	addPage("POST /macs/{name}/hard-reset", web.MTeamUp, (*Server).pMacHardReset)
}

const (
	tabOverview = "overview"
	tabLive     = "live"
	tabRecords  = "records"
)

const (
	logsJournal = "journal"
	logsKernel  = "kernel"
	logsEvents  = "events"
)

type macPageData struct {
	Tab string
	// Flash is a refusal of one of the Lease's buttons.
	Flash string
	Mac   macRow
	// Path is the page's own path; Back and BackLabel the list it belongs to.
	Path, Back, BackLabel string
	// Team is the M team and up: the Lease's buttons and "Boot a kernel".
	Team, Maintainer bool
	// Stream and Changes are for the page's script: the Mac's events, and
	// what of the Mac the page was made from (see macChanges).
	Stream, Changes string

	Facts macFacts
	Jobs  []macJobRow
	// Hands is the Compare waiting on this Mac for a person to press Done.
	Hands string

	// Fence stands in place of Live for an M team member without their own
	// Lease on the Mac.
	Fence *liveFence
	Live  *liveData

	Leases, MacRecord, Audit, Exec []recordShownLine
	// ShowAudit: the Audit log is for Maintainers and the Mac's Volunteer.
	ShowAudit bool
}

type macFacts struct {
	Volunteers                         bool
	AccessLevel, Window                string
	KnownGood, Kernel, Agent, LastSeen string
	Reason                             string
	Queued                             int
	// LabAgent is the lab's lab-agent when the Mac's is older, and
	// UpdateLine the one line its own Volunteer runs at the Mac.
	LabAgent, UpdateLine string
	// Cable is a Lab-owned Mac's place on the lab server's cables, and
	// NoHardReset why no Hard reset can reach it now.
	Cable, NoHardReset string
	// Counted says whether a Volunteer's Mac's performance values make the
	// headline, and why; Marked is a Maintainer's mark on it.
	Counted string
	Marked  bool
}

type macJobRow struct {
	ID, What, By, State, StateClass, When string
}

type liveFence struct {
	// Holder and Until name the Lease that holds the Mac, when one does.
	Holder, Until string
	CanLease      bool
	Why           string
}

type liveData struct {
	API, CSRF string
	// ViewOnly is a Volunteer's Live: nothing on it asks the Mac for a
	// capture.
	ViewOnly bool
	// NotNow is why the Mac's screen and journal are not read now.
	NotNow string
	// Screen is the latest kept capture's address, ScreenAt when it was made.
	Screen, ScreenAt string
	Logs             string
	Events           []recordShownLine
	// Reset is the Hard reset of a Lab-owned Mac, for who may send one.
	Reset *liveReset
}

type liveReset struct {
	// Why is why the viewer can send none now; "" shows the button, which
	// turns into Sentence.
	Why, Sentence string
}

// hardResetSentence is what the Hard reset button turns into.
func hardResetSentence(name string) string {
	return "Hard reset: " + name + " is forced to restart now, whatever it is doing"
}

// whenDay is a time as a record says it, in Brisbane.
func whenDay(t time.Time) string { return t.In(brisbane).Format("2006-01-02 15:04 MST") }

// callerAs is the caller a page acts as for the person signed in on it: who
// they are to Scope (see Person.access).
func callerAs(p Person) caller {
	switch {
	case p.Blocked:
	case p.isMaintainer():
		return caller{maintainer: p.Handle}
	case p.MTeam:
		return caller{team: p.Handle, account: p.AccountID}
	case p.Volunteer:
		return caller{login: p.Handle, account: p.AccountID}
	}
	return caller{}
}

func tabOf(raw string) string {
	if raw == tabLive || raw == tabRecords {
		return raw
	}
	return tabOverview
}

func logsOf(raw string) string {
	if raw == logsKernel || raw == logsEvents {
		return raw
	}
	return logsJournal
}

// macPagePath is a Mac's page under a tab.
func macPagePath(name, tab string) string {
	p := "/macs/" + url.PathEscape(name)
	if tab != tabOverview && tab != "" {
		p += "?tab=" + tab
	}
	return p
}

// macChanges is what of a Mac a page shows that the stream can tell has
// changed: its state, its job and its Lease. The page's script makes the
// same line from a device event and compares.
func macChanges(d DeviceView) string {
	var account, expires int64
	if d.Lease != nil {
		account, expires = d.Lease.AccountID, d.Lease.Expires.Unix()
	}
	return fmt.Sprintf("%s|%s|%d|%d", d.State, d.ActiveJob, account, expires)
}

func (s *Server) pMac(w http.ResponseWriter, r *http.Request) {
	v := s.dev(r.PathValue("name"))
	if v == nil || v.snapshot().State == api.StateLeft || !s.sees(callerAs(leaser(r)), r.PathValue("name")) {
		s.notFound(w, r)
		return
	}
	s.renderMac(w, r, v, tabOf(r.URL.Query().Get("tab")), nil)
}

// renderMac writes a Mac's page; with a refusal of one of its buttons, it
// is said above the tabs under its own status.
func (s *Server) renderMac(w http.ResponseWriter, r *http.Request, v *dev, tab string, refused error) {
	p := leaser(r)
	c := callerAs(p)
	d := s.viewFor(c, v)
	own := d.VolunteerID != 0 && d.VolunteerID == p.AccountID
	data := macPageData{Tab: tab, Mac: s.macRow(v, d, p, s.holdsLease(v, p)), Path: "/macs/" + url.PathEscape(d.Name),
		Back: "/macs", BackLabel: "Macs", Team: p.OnMTeam(), Maintainer: p.isMaintainer(),
		Stream: "/api/stream?device=" + url.QueryEscape(d.Name), Changes: macChanges(d)}
	page := web.Page{Title: d.Name, Nav: "macs"}
	if !data.Team {
		data.Back, data.BackLabel, page.Nav = "/my-macs", "My Macs", "my-macs"
		data.Mac.CanLease, data.Mac.Why, data.Mac.Warning = false, "", ""
	}
	if refused != nil {
		data.Flash, page.Status = refused.Error(), statusOf(refused)
	}
	if id := s.handsWaiting(v); id != "" {
		if j, err := s.store.job(id); err == nil && s.mayHands(p, j) {
			data.Hands = id
		}
	}
	switch tab {
	case tabLive:
		data.Fence, data.Live = s.macLive(r, v, d, c, data.Mac)
	case tabRecords:
		s.macRecords(&data, v, d, p, p.isMaintainer() || own)
	default:
		data.Facts, data.Jobs = s.macFacts(v, d), s.macJobs(c, d)
		if data.Facts.LabAgent = d.LabAgent; own {
			data.Facts.UpdateLine = agentUpdateLine(d)
		}
	}
	page.Data = data
	s.render(w, r, "mac", page)
}

func (s *Server) macFacts(v *dev, d DeviceView) macFacts {
	f := macFacts{Volunteers: d.IsVolunteer(), AccessLevel: "Full", KnownGood: d.KnownGood, Kernel: d.Kernel,
		Agent: d.Facts.AgentVersion, Reason: d.StateReason, Queued: d.Queued}
	if d.Cable != nil {
		f.Cable, f.NoHardReset = d.Cable.String(), d.Cable.NoHardReset
	}
	if !d.LastSeen.IsZero() {
		f.LastSeen = whenDay(d.LastSeen)
	}
	if f.Volunteers {
		f.Counted, f.Marked = s.countedWords(d.Device), d.Counted
		switch {
		case d.AccessLevel != api.AccessFull:
			f.AccessLevel = "Qualification"
		case d.Limited:
			f.AccessLevel = "Qualification (limited by its Volunteer on the site; Full on the Mac)"
		}
		v.mu.Lock()
		until := v.openUntilLocked()
		v.mu.Unlock()
		f.Window = "closed"
		switch {
		case d.SitePause != nil && d.SitePause.Until.IsZero():
			f.Window = "Paused by its Volunteer until they resume it"
		case d.SitePause != nil:
			f.Window = "Paused by its Volunteer until " + when(d.SitePause.Until)
		case !until.IsZero():
			f.Window = "open until " + when(until)
		}
	}
	return f
}

// macJobsShown is how many of a Mac's jobs Overview lists.
const macJobsShown = 20

// macJobs are the Mac's newest jobs as c may read them: a Volunteer only
// those of the Mac's enrollment now.
func (s *Server) macJobs(c caller, d DeviceView) []macJobRow {
	jobs, err := s.store.jobs(d.Name, macJobsShown, false)
	if err != nil {
		s.log.Printf("%s's jobs: %v", d.Name, err)
		return nil
	}
	var out []macJobRow
	for _, j := range jobs {
		if !c.reads() && (j.DeviceID == "" || j.DeviceID != d.ID) {
			continue
		}
		row := macJobRow{ID: j.ID, What: jobWhat(j), By: j.Maintainer, When: whenDay(j.Created)}
		row.State, row.StateClass = jobState(j)
		out = append(out, row)
	}
	return out
}

// macEventsShown is how many kernel events Live starts with.
const macEventsShown = 100

// macLive is Live for c: the fence for an M team member who may not watch
// the Mac, else what the page's script shows and keeps fresh.
func (s *Server) macLive(r *http.Request, v *dev, d DeviceView, c caller, row macRow) (*liveFence, *liveData) {
	watch := s.mayDo(c, v, actWatch)
	if c.team != "" && watch != nil {
		f := &liveFence{CanLease: row.CanLease}
		if d.Lease != nil {
			f.Holder, f.Until = row.Holder, row.Until
		} else if !row.CanLease {
			f.Why = row.Why
		}
		return f, nil
	}
	base := s.basePath() + "/api/devices/" + url.PathEscape(d.Name)
	live := &liveData{API: base, ViewOnly: !c.reads(), Logs: logsOf(r.URL.Query().Get("logs"))}
	if !live.ViewOnly {
		live.CSRF = CSRFToken(r)
	}
	if watch != nil {
		live.NotNow = watch.Error()
	}
	if kept := s.screens(d.Name); len(kept) > 0 {
		live.Screen, live.ScreenAt = base+"/screens/"+url.PathEscape(kept[0].Name), whenDay(kept[0].Time)
	}
	if d.Cable != nil && !live.ViewOnly {
		live.Reset = &liveReset{Why: d.Cable.NoHardReset, Sentence: hardResetSentence(d.Name)}
		if err := s.fence(v, leaser(r), fenceReset); err != nil {
			live.Reset.Why = err.Error()
		}
	}
	if live.Logs == logsEvents {
		events := v.eventsSince(s.now().Add(-24 * time.Hour))
		for i := len(events) - 1; i >= 0 && len(live.Events) < macEventsShown; i-- {
			e := events[i]
			live.Events = append(live.Events, recordShownLine{whenDay(e.Time), e.Kind + ": " + e.Line})
		}
	}
	return nil, live
}

// macRecordsShown is how many lines of each record Records shows.
const macRecordsShown = 100

func (s *Server) macRecords(data *macPageData, v *dev, d DeviceView, p Person, audit bool) {
	events, err := s.store.leaseRecord(d.ID, macRecordsShown)
	if err != nil {
		s.log.Printf("%s's Lease record: %v", d.Name, err)
	}
	for _, e := range events {
		data.Leases = append(data.Leases, recordShownLine{whenDay(e.Time), e.Line()})
	}
	data.MacRecord = s.macRecord(d)
	data.Exec = s.execRecordFor(v, p)
	if data.ShowAudit = audit; audit {
		data.Audit = s.auditShown(d.Name, macRecordsShown)
	}
}

// auditShown is the newest n lines of the lab's copy of a Mac's Audit log,
// newest first, for whoever may read it: Maintainers and the Mac's Volunteer.
func (s *Server) auditShown(name string, n int) []recordShownLine {
	entries, err := s.auditTailed(name, n)
	if err != nil {
		s.log.Printf("%s's Audit log: %v", name, err)
	}
	var out []recordShownLine
	for i := len(entries) - 1; i >= 0 && len(out) < n; i-- {
		e := entries[i]
		who := "the lab"
		switch {
		case e.VolunteerLogin != "":
			who = "its Volunteer " + e.VolunteerLogin
		case e.Maintainer != "":
			who = "@" + e.Maintainer
		}
		line := who + " · "
		if e.Job != "" {
			line += "job " + e.Job + " · "
		}
		if e.Refused != "" {
			line += "REFUSED " + e.Action + " (" + e.Refused + ")"
		} else {
			line += e.Action
		}
		out = append(out, recordShownLine{whenDay(e.Time), line})
	}
	return out
}

// execRecordFor is the Exec record's lines on v that p may read, newest
// first: Maintainers and the Mac's Volunteer all of them, anyone else their
// own (see execRecordOf).
func (s *Server) execRecordFor(v *dev, p Person) []recordShownLine {
	events, err := s.execRecordOf(p, v, execRecordShown)
	if err != nil {
		s.log.Printf("the Exec record of %s for its page: %v", v.snapshot().Name, err)
		return nil
	}
	var out []recordShownLine
	for _, e := range events {
		out = append(out, recordShownLine{whenDay(e.Time), e.Line()})
	}
	return out
}

// pMacHardReset is the Hard reset button of a Lab-owned Mac's Live.
func (s *Server) pMacHardReset(w http.ResponseWriter, r *http.Request) {
	v := s.macOr404(w, r)
	if v == nil {
		return
	}
	name := v.snapshot().Name
	if !s.sure(w, r, hardResetSentence(name), macPagePath(name, tabLive)) {
		return
	}
	p := leaser(r)
	_, err := s.hardResetBy(r.Context(), v, p)
	s.backToMacs(w, r, err)
}

// auditTailMax is the most of labd's copy of an Audit log a page reads:
// root on the Mac can make the log long, and a page shows its newest lines.
const auditTailMax = 1 << 20

// auditTailed is the newest n entries of labd's copy of a Mac's Audit log,
// oldest first, read from its end.
func (s *Server) auditTailed(name string, n int) ([]api.AuditEntry, error) {
	f, err := os.Open(s.auditPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return auditTail(f, st.Size(), n)
}

// auditTail is the newest n entries in the last bytes of f, oldest first. It
// reads more of f's end until it has n or has read auditTailMax; a line cut
// by where it starts is left out.
func auditTail(f io.ReaderAt, size int64, n int) ([]api.AuditEntry, error) {
	for chunk := int64(64 << 10); ; chunk *= 4 {
		chunk = min(chunk, auditTailMax, size)
		buf := make([]byte, chunk)
		if _, err := f.ReadAt(buf, size-chunk); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		lines := bytes.Split(buf, []byte("\n"))
		if chunk < size {
			lines = lines[1:]
		}
		var out []api.AuditEntry
		for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
			var e api.AuditEntry
			if json.Unmarshal(lines[i], &e) == nil {
				out = append(out, e)
			}
		}
		if len(out) == n || chunk == size || chunk == auditTailMax {
			slices.Reverse(out)
			return out, nil
		}
	}
}
