package server

// Macs: the fleet with who holds each Lease, for the M team and the
// Maintainers. Lease, Renew and Release are its buttons, and a row that
// takes no Lease says why.

import (
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /macs", web.MTeamUp, (*Server).pMacs)
	addPage("POST /macs/{name}/lease", web.MTeamUp, (*Server).pMacsLease)
	addPage("POST /macs/{name}/renew", web.MTeamUp, (*Server).pMacsRenew)
	addPage("POST /macs/{name}/release", web.MTeamUp, (*Server).pMacsRelease)
}

// macsShow are the filters of Macs.
const (
	showAll    = "all"
	showMine   = "mine"
	showLeased = "leased"
)

type macsData struct {
	Show string
	// Flash is a refusal, said above the table.
	Flash string
	// Maintainer adds the hours of a Lease.
	Maintainer bool
	Macs       []macRow
	// NeedsHands are the Macs waiting for a person, said above the table.
	NeedsHands []handsRow
	// Empty is said in place of the table.
	Empty string
	// Mine, under the filter of that name, is the cards of the Macs the
	// viewer lends, as My Macs shows them, in place of the table.
	Mine *myMacs
}

type macRow struct {
	Name string
	// Kind is whose Mac it is in words; Volunteer its Volunteer's handle,
	// when the lab knows it.
	Kind, Volunteer string
	// Cable is a Lab-owned Mac as sighted, not sighted or no cable, and
	// NoHardReset why no Hard reset can reach it now.
	Cable, NoHardReset string
	Configuration      string
	// State is the Mac's state in plain words and StateClass its colour.
	State, StateClass string
	// OldAgent is the Mac's lab-agent when it is older than the lab's,
	// which is LabAgent.
	OldAgent, LabAgent string
	// Holder is who holds its Lease, HolderKind their kind and Until its end.
	Holder, HolderKind, HolderClass, Until string
	// CanLease, or Mine: the viewer holds its Lease, and renews or releases.
	CanLease, Mine bool
	// Why is why the row takes no Lease; Warning what a Maintainer is told
	// beside a Lease they may still take.
	Why, Warning string
}

// handsRow is a Mac that waits for a person, and why.
type handsRow struct{ Name, Why string }

func showOf(raw string) string {
	if raw == showMine || raw == showLeased {
		return raw
	}
	return showAll
}

// macsPath is Macs under a filter.
func macsPath(show string) string {
	if show == showAll {
		return "/macs"
	}
	return "/macs?show=" + show
}

func (s *Server) pMacs(w http.ResponseWriter, r *http.Request) {
	s.renderMacs(w, r, showOf(r.URL.Query().Get("show")), nil)
}

// renderMacs writes Macs; with a refusal, it is said above the table under
// its own status.
func (s *Server) renderMacs(w http.ResponseWriter, r *http.Request, show string, refused error) {
	s.renderMacsWith(w, r, show, refused, nil)
}

func (s *Server) renderMacsWith(w http.ResponseWriter, r *http.Request, show string, refused error, lines map[string]*macOSLine) {
	p := leaser(r)
	data := macsData{Show: show, Maintainer: p.isMaintainer()}
	page := web.Page{Title: "Macs", Nav: "macs"}
	if refused != nil {
		data.Flash, page.Status = refused.Error(), statusOf(refused)
	}
	if show == showMine {
		mine, err := s.myMacsOf(p)
		if err != nil {
			s.pageFailed(w, err)
			return
		}
		mine.show(lines)
		data.Mine = mine
		page.Data = data
		s.render(w, r, "macs", page)
		return
	}
	type mac struct {
		v *dev
		d DeviceView
	}
	var macs []mac
	for _, v := range s.allDevs() {
		macs = append(macs, mac{v, s.view(v)})
	}
	rank := map[string]int{}
	for _, m := range macs {
		_, class := s.macState(m.v, m.d)
		rank[m.d.Name] = slices.Index(macStateOrder, class)
	}
	sort.Slice(macs, func(i, j int) bool {
		a, b := macs[i].d, macs[j].d
		if rank[a.Name] != rank[b.Name] {
			return rank[a.Name] < rank[b.Name]
		}
		return a.Name < b.Name
	})
	for _, m := range macs {
		v, d := m.v, m.d
		if d.State == api.StateLeft {
			continue
		}
		mine := s.holdsLease(v, p)
		if show == showLeased && !mine {
			continue
		}
		data.Macs = append(data.Macs, s.macRow(v, d, p, mine))
		if d.State == api.StateNeedsHands {
			data.NeedsHands = append(data.NeedsHands, handsRow{d.Name, d.StateReason})
		}
	}
	if len(data.Macs) == 0 {
		data.Empty = map[string]string{showAll: "No Mac is enrolled.", showLeased: "You hold no Lease."}[show]
	}
	page.Data = data
	s.render(w, r, "macs", page)
}

func (s *Server) macRow(v *dev, d DeviceView, p Person, mine bool) macRow {
	row := macRow{Name: d.Name, Kind: "Lab-owned", Mine: mine}
	row.State, row.StateClass = s.macState(v, d)
	if d.LabAgent != "" {
		row.OldAgent, row.LabAgent = d.Facts.AgentVersion, d.LabAgent
	}
	if d.Cable != nil {
		row.Cable, row.NoHardReset = d.Cable.State, d.Cable.NoHardReset
	}
	if d.IsVolunteer() {
		row.Kind = "Volunteer's"
		if volunteer, err := s.person(d.VolunteerID); err == nil {
			row.Volunteer = volunteer.Handle
		}
	}
	conf := s.configuration(v)
	row.Configuration = conf.String()
	if l := d.Lease; l != nil {
		row.Holder, row.Until = orStr(l.Owner(), l.Holder), when(l.Expires)
		if l.Extended {
			row.Until += ", extended until its job ends"
		}
		if holder, err := s.person(l.AccountID); err == nil {
			// Who is an Admin is on no screen: here an Admin is a Maintainer.
			kind := min(holder.Role(), web.Maintainer)
			row.Holder, row.HolderKind, row.HolderClass = holder.Handle, kind.Badge(), kind.BadgeClass()
		}
	}
	if mine {
		return row
	}
	found, err := s.mayLease(v, p, p.Handle)
	switch {
	case err == nil:
		row.CanLease = true
		if held, _ := s.heldOfConfiguration(v, p); found.warning != "" && held != "" {
			row.Warning = "you hold " + held + ", the same Configuration: an M team member would be refused"
		}
	case d.Lease != nil:
		row.Why = "leased by @" + row.Holder + " until " + row.Until
	case d.KnownGood == "":
		row.Why = "has not passed its baseline"
	case d.IsVolunteer() && v.windowClosed():
		row.Why = "outside its Availability window"
	default:
		row.Why = err.Error()
		if held, _ := s.heldOfConfiguration(v, p); held != "" {
			row.Why = "you hold " + held + ", the same Configuration"
			if !conf.Known() || !s.configuration(s.dev(held)).Known() {
				row.Why = "you hold " + held + ", and its core counts are unknown: the two count as the same Configuration until the lab reads them"
			}
		}
	}
	return row
}

// macStateOrder is the order of the Macs table's rows by state: a Mac
// waiting for a person first, one that left the lab last.
var macStateOrder = []string{"hands", "recovering", "running", "baseline", "ready", "paused", "offline", "left"}

// macState is a Mac's state in plain words, and the class that colours it.
func (s *Server) macState(v *dev, d DeviceView) (words, class string) {
	switch {
	case d.State == api.StateNeedsHands:
		return "Needs hands", "hands"
	case d.State == api.StateRecovering:
		return "Recovering", "recovering"
	case d.ActiveJob != "" || d.State == api.StateBusy:
		return "Running a job", "running"
	case d.Arrival != "" && d.Arrival != ArrivalReady:
		return upperFirst(d.Arrival), "baseline"
	case d.State == api.StateNew || d.KnownGood == "":
		return "Needs baseline", "baseline"
	case d.SitePause != nil:
		return "Paused", "paused"
	case d.IsVolunteer() && v.windowClosed():
		return "Outside its window", "paused"
	case d.State == api.StateReady && d.Lease != nil:
		return "Leased", "ready"
	case d.State == api.StateReady:
		return "Free", "ready"
	case d.State == api.StateOffline:
		return "Not seen", "offline"
	case d.State == api.StateLeft:
		return "Left the lab", "left"
	}
	return upperFirst(strings.ReplaceAll(string(d.State), "_", " ")), "offline"
}

// upperFirst is words with its first letter a capital.
func upperFirst(words string) string {
	if words == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(words)
	return string(unicode.ToUpper(r)) + words[n:]
}

// macOr404 is the Mac a button of Macs names.
func (s *Server) macOr404(w http.ResponseWriter, r *http.Request) *dev {
	v := s.dev(r.PathValue("name"))
	if v == nil || v.snapshot().State == api.StateLeft {
		s.notFound(w, r)
		return nil
	}
	return v
}

// macTabPressed is the tab of a Mac's own page a button was pressed on, for
// a button that was: the Lease's buttons are on Macs and on a Mac's page.
func macTabPressed(r *http.Request) (tab string, ok bool) {
	switch r.PostFormValue("back") {
	case "mac":
		return tabOverview, true
	case tabLive:
		return tabLive, true
	}
	return "", false
}

// backToMacs answers a button of Macs or of a Mac's page: back to the page
// it was pressed on, Macs under its filter, or that page saying why the lab
// refused.
func (s *Server) backToMacs(w http.ResponseWriter, r *http.Request, err error) {
	show, to := showOf(r.PostFormValue("show")), ""
	if tab, ok := macTabPressed(r); ok {
		if v := s.dev(r.PathValue("name")); v != nil {
			if err != nil {
				s.renderMac(w, r, v, tab, err)
				return
			}
			to = macPagePath(v.snapshot().Name, tab)
		}
	}
	if to == "" {
		if err != nil {
			s.renderMacs(w, r, show, err)
			return
		}
		to = macsPath(show)
	}
	w.Header().Set("Location", s.basePath()+to)
	w.WriteHeader(http.StatusSeeOther)
}

func (s *Server) pMacsLease(w http.ResponseWriter, r *http.Request) {
	v := s.macOr404(w, r)
	if v == nil {
		return
	}
	p := leaser(r)
	var ttl time.Duration
	if raw := r.PostFormValue("hours"); raw != "" {
		hours, err := strconv.Atoi(raw)
		if err != nil || hours < 1 {
			s.backToMacs(w, r, refuse(400, "a Lease's length is a number of hours"))
			return
		}
		ttl = time.Duration(hours) * time.Hour
	}
	// A Lease taken on the site has the person's handle as its label.
	_, err := s.takeLease(v, p, p.Handle, ttl)
	s.backToMacs(w, r, err)
}

func (s *Server) pMacsRenew(w http.ResponseWriter, r *http.Request) {
	v := s.macOr404(w, r)
	if v == nil {
		return
	}
	p := leaser(r)
	v.mu.Lock()
	l := v.liveLeaseLocked()
	v.mu.Unlock()
	if l == nil || !leaseIsOf(l, p) {
		s.backToMacs(w, r, refuse(409, "you hold no Lease on %s to renew", v.snapshot().Name))
		return
	}
	_, err := s.takeLease(v, p, l.Holder, 0)
	s.backToMacs(w, r, err)
}

func (s *Server) pMacsRelease(w http.ResponseWriter, r *http.Request) {
	v := s.macOr404(w, r)
	if v == nil {
		return
	}
	name := v.snapshot().Name
	back := macsPath(showOf(r.PostFormValue("show")))
	if tab, ok := macTabPressed(r); ok {
		back = macPagePath(name, tab)
	}
	if !s.sure(w, r, "Release: your Lease on "+name+" ends now", back) {
		return
	}
	p := leaser(r)
	if !s.holdsLease(v, p) {
		s.backToMacs(w, r, refuse(409, "you hold no Lease on %s to release", name))
		return
	}
	s.backToMacs(w, r, s.releaseLease(v, p, "", false, ""))
}
