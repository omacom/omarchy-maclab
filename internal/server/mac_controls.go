package server

// The Volunteer's controls on the site. The Mac sets the ceiling: Open now,
// raising to Full and turning publishing on are `sudo lab-agent …` at the
// Mac. The site gives one thing, the weekly schedule, which the Mac takes at
// its next check-in. Otherwise it only takes away, and undoes only what it
// took: Pause until a time or Resume, Limit to Qualification until "Remove
// limit", and Leave. They are the buttons of a Mac's card on My Macs, and
// work for the Mac's own Volunteer only. Pause, Limit and Leave take
// something away, so they ask twice.

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("POST /my-macs/{name}/pause", web.SignedIn, (*Server).pPauseMac)
	addPage("POST /my-macs/{name}/resume", web.SignedIn, controlOf((*Server).resumeMac))
	addPage("POST /my-macs/{name}/limit", web.SignedIn, (*Server).pLimitMac)
	addPage("POST /my-macs/{name}/remove-limit", web.SignedIn, controlOf((*Server).unlimitMac))
	addPage("POST /my-macs/{name}/leave", web.SignedIn, (*Server).pLeaveMac)
	addPage("POST /my-macs/{name}/window", web.SignedIn, (*Server).pWindowMac)
	addPage("POST /people/{id}/enrollments/{mac}/end", web.Maintainers, (*Server).pEndEnrollment)
}

// myMac is the Mac a button of My Macs names, when the person signed in on
// r lends it; otherwise it answers as for a page the lab does not have.
func (s *Server) myMac(w http.ResponseWriter, r *http.Request) *dev {
	v := s.dev(r.PathValue("name"))
	if p := PersonOf(r); v == nil || p == nil || s.lends(v, *p) != nil {
		s.notFound(w, r)
		return nil
	}
	return v
}

// controlDone answers a button of My Macs: back to the Mac's card, or the
// cards saying why the lab refused. They are on My Macs, and under "Mine"
// on Macs for the M team and up.
func (s *Server) controlDone(w http.ResponseWriter, r *http.Request, v *dev, err error) {
	var no *refusal
	switch {
	case err == nil:
		to := myMacsPath(leaser(r))
		// A Mac that left has no card to come back to.
		if name := r.PathValue("name"); s.dev(name) == v {
			to += "#mac-" + name
		}
		http.Redirect(w, r, s.basePath()+to, http.StatusSeeOther)
	case errors.As(err, &no) && no.code == http.StatusNotFound:
		s.notFound(w, r)
	case errors.As(err, &no):
		s.renderMyMacs(w, r, err)
	default:
		s.log.Printf("My Macs, %s: %v", r.URL.Path, err)
		s.renderMyMacs(w, r, refuse(http.StatusInternalServerError, "The lab couldn't do that."))
	}
}

// leaveSentence is what the Leave button turns into.
func leaveSentence(name string) string {
	return "Leave: " + name + " leaves the lab now, any job on it stops, and the lab is removed from it when it next reaches the lab"
}

// pauseSentence and limitSentence are what Pause and Limit turn into: they
// take a running job and a Lease away, so they ask twice.
func pauseSentence(name string) string {
	return "Pause: " + name + " takes nothing from the lab, any job on it stops, and its Lease ends"
}

func limitSentence(name string) string {
	return "Limit to Qualification: any job on " + name + " stops, and its Lease ends"
}

// PauseSentence and LimitSentence are the card's, for its buttons.
func (c macCard) PauseSentence() string { return pauseSentence(c.Name) }
func (c macCard) LimitSentence() string { return limitSentence(c.Name) }

func controlOf(control func(*Server, *dev, Person) error) func(*Server, http.ResponseWriter, *http.Request) {
	return func(s *Server, w http.ResponseWriter, r *http.Request) {
		if v := s.myMac(w, r); v != nil {
			s.controlDone(w, r, v, control(s, v, *PersonOf(r)))
		}
	}
}

// pPauseMac takes a Pause: with no "until", until Resume; otherwise until
// that time, written as the Mac's own clock reads (2026-11-20T07:00, in the
// zone of its Availability window) or with its zone (RFC 3339).
func (s *Server) pPauseMac(w http.ResponseWriter, r *http.Request) {
	v := s.myMac(w, r)
	if v == nil {
		return
	}
	var until time.Time
	if raw := r.PostFormValue("until"); raw != "" {
		var err error
		if until, err = time.Parse(time.RFC3339, raw); err != nil {
			v.mu.Lock()
			settings, _ := v.windowStateLocked()
			v.mu.Unlock()
			if until, err = time.ParseInLocation("2006-01-02T15:04", raw, settings.Location()); err != nil {
				s.controlDone(w, r, v, refuse(http.StatusUnprocessableEntity, "Pause until a date and a time, like 2026-11-20T07:00, or leave it empty to Pause until you Resume."))
				return
			}
		}
	}
	if !s.sure(w, r, pauseSentence(v.snapshot().Name), myMacsPath(leaser(r))) {
		return
	}
	s.controlDone(w, r, v, s.pauseMac(v, *PersonOf(r), until))
}

// pWindowMac adds a weekly window, days and a from and to time, or always,
// or removes one: what the card's window form posts.
func (s *Server) pWindowMac(w http.ResponseWriter, r *http.Request) {
	v := s.myMac(w, r)
	if v == nil {
		return
	}
	var c api.WindowChange
	switch {
	case r.PostFormValue("remove") != "":
		c.Remove = r.PostFormValue("remove")
	case r.PostFormValue("always") != "":
		c.Add = "always"
	default:
		c.Add = strings.TrimSpace(r.PostFormValue("days")) + " " + strings.TrimSpace(r.PostFormValue("from")) + "-" + strings.TrimSpace(r.PostFormValue("to"))
	}
	add, rule, err := windowChangeOf(c)
	if err == nil {
		err = s.changeWindow(v, *PersonOf(r), add, rule)
	}
	s.controlDone(w, r, v, err)
}

// pLimitMac is Limit to Qualification, which asks twice.
func (s *Server) pLimitMac(w http.ResponseWriter, r *http.Request) {
	v := s.myMac(w, r)
	if v == nil {
		return
	}
	if !s.sure(w, r, limitSentence(v.snapshot().Name), myMacsPath(leaser(r))) {
		return
	}
	s.controlDone(w, r, v, s.limitMac(v, *PersonOf(r)))
}

// pLeaveMac is Leave, which asks twice.
func (s *Server) pLeaveMac(w http.ResponseWriter, r *http.Request) {
	v := s.myMac(w, r)
	if v == nil {
		return
	}
	if !s.sure(w, r, leaveSentence(v.snapshot().Name), myMacsPath(leaser(r))) {
		return
	}
	s.controlDone(w, r, v, s.leaveMac(v, *PersonOf(r)))
}

// pEndEnrollment is "End enrollment" beside one of a person's Macs on
// People: a Maintainer ends it as `lab remove` does. It asks twice.
func (s *Server) pEndEnrollment(w http.ResponseWriter, r *http.Request) {
	k, err := s.personAt(r)
	mac := r.PathValue("mac")
	v := s.dev(mac)
	if err == nil && (v == nil || v.snapshot().VolunteerID != k.AccountID) {
		err = refuse(http.StatusConflict, "@%s lends no Mac called %s.", k.Handle, mac)
	}
	if err == nil {
		if !s.sure(w, r, "End the enrollment of "+mac+": it leaves the lab now, and any job on it stops", "/people?open="+r.PathValue("id")) {
			return
		}
		if err = s.endEnrollment(v, leaving{how: leftByMaintainer, by: *PersonOf(r)}); errors.Is(err, errMacGone) {
			err = refuse(http.StatusConflict, "@%s lends no Mac called %s.", k.Handle, mac)
		}
	}
	s.done(w, r, k.AccountID, err)
}
