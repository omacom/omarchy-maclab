package server

// A change of terms. The terms' version is the sha256 of the shipped text;
// a change reaches the lab by a Deploy, so it takes effect when labd starts.
// From then each Mac whose Volunteer has not accepted the new terms takes
// nothing from the lab, as when Paused: no job, no Lease. No deadline, and
// never an automatic Leave. The Volunteer accepts on My Macs, signed in,
// once for all their Macs, and it is recorded as the first acceptance was:
// on each Mac's row the sha256 of the text and the time, the Mac's
// Volunteer being the person, and a line of the Mac's record. They are told
// on My Macs and by one Notice. The M team and the Maintainers accept no
// terms, so their Macs are never held for it.

import (
	"net/http"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// termsText is the Volunteer's terms as shipped with this Lab release, the
// same text /terms serves and "Lend a Mac" shows; a test may ship others.
func (s *Server) termsText() web.Text {
	if t := s.shippedTerms.Load(); t != nil {
		return *t
	}
	return web.Terms()
}

// terms is the text a Mac's owner accepts before the Mac joins, and its
// sha256. lab-agent setup shows them, the owner types yes, and the Mac's
// record keeps the sha256 of the text accepted, with the person, the Mac
// and the time.
func (s *Server) terms() (text, sum string) {
	t := s.termsText()
	return t.Body, t.SHA256
}

// termsWaitingLine is the one line `lab` says when a job or a Lease is
// refused because the Mac waits for its Volunteer.
func termsWaitingLine(mac string) string {
	return mac + " is waiting for its Volunteer to accept the lab's changed terms on My Macs: until then it takes no job and no Lease"
}

// macTermsAccepted is the Mac's record's event for an acceptance on My Macs.
const macTermsAccepted = "terms-accepted"

// termsCut is why changed terms ended a Lease.
const termsCut = "the lab's terms changed and its Volunteer has not accepted them yet"

// acceptsNoTerms is whether the person is on the M team or a Maintainer.
func (s *Server) acceptsNoTerms(accountID int64) bool {
	if accountID == 0 {
		return false
	}
	p, err := s.person(accountID)
	return err == nil && p.OnMTeam()
}

// termsTookEffect holds each Mac whose Volunteer has the shipped terms to
// accept. labd calls it as it starts, which is when changed terms reach the
// lab.
func (s *Server) termsTookEffect() {
	for _, v := range s.allDevs() {
		s.decideTerms(v)
	}
}

// decideTerms is whether v waits for its Volunteer: v is a Volunteer's
// Mac in the lab, the terms it was accepted under are not the shipped ones,
// and its Volunteer is not on the M team or a Maintainer. It is decided at
// the start and again at each check-in, since a mark may change between. A
// Mac that waits has its running job stopped and its Lease ended as a Pause
// does, and its Volunteer is told by one Notice for each Mac and each text.
func (s *Server) decideTerms(v *dev) {
	_, sum := s.terms()
	d := v.snapshot()
	noTerms := d.IsVolunteer() && s.acceptsNoTerms(d.VolunteerID)
	v.mu.Lock()
	waits := v.d.VolunteerID == d.VolunteerID && !noTerms && v.d.IsVolunteer() && v.d.State != api.StateLeft && v.d.TermsSHA256 != sum
	changed := v.termsWait != waits
	v.termsWait = waits
	active := ""
	if waits {
		if v.siteCut.job != v.d.ActiveJob {
			active = v.cutJobLocked(termsCut)
		}
		if v.d.TermsNoticed != sum {
			v.d.TermsNoticed = sum
			v.saveLocked()
			v.s.notify(v.s.notice(v.d, noticeTerms, jobWording{}))
		}
		if l := v.d.Lease; l != nil && l.Expires.After(s.now()) {
			cut := *l
			cut.Expires = s.now()
			v.d.Lease, v.leaseCut = &cut, termsCut
			v.saveLocked()
			changed = true
		}
	}
	if changed {
		v.publishLocked()
		v.bump()
	}
	v.mu.Unlock()
	if active != "" {
		s.cancelJob(active, termsCut)
	}
	if waits {
		s.lapse(v)
	}
}

// termsWaiting is the date of the terms p has to accept, "" when none of
// their Macs waits for them.
func (s *Server) termsWaiting(p Person) string {
	if p.AccountID == 0 || s.acceptsNoTerms(p.AccountID) {
		return ""
	}
	for _, v := range s.allDevs() {
		v.mu.Lock()
		waits := v.termsWait && v.d.VolunteerID == p.AccountID && !v.forgotten
		v.mu.Unlock()
		if waits {
			return s.termsText().Date
		}
	}
	return ""
}

// acceptTerms is p accepting the shipped terms on My Macs, for all their
// Macs at once: each one's row keeps the sha256 of the text and the time, as
// at Join, its record says so, and each takes from the lab again. It names
// the Macs.
func (s *Server) acceptTerms(p Person) ([]string, error) {
	if p.AccountID == 0 {
		return nil, refuse(http.StatusForbidden, "Sign in to accept the lab's terms.")
	}
	if p.Blocked {
		return nil, refuse(http.StatusForbidden, "%s", blockedSentence)
	}
	t := s.termsText()
	var macs []string
	for _, v := range s.allDevs() {
		v.mu.Lock()
		if v.forgotten || v.d.VolunteerID != p.AccountID || !v.d.IsVolunteer() || v.d.State == api.StateLeft || v.d.TermsSHA256 == t.SHA256 {
			v.mu.Unlock()
			continue
		}
		v.d.TermsSHA256, v.d.TermsAccepted = t.SHA256, s.now()
		v.termsWait = false
		v.said(macTermsAccepted, "its Volunteer accepted the terms of "+t.Date+" on My Macs (sha256 "+t.SHA256+")")
		v.saveLocked()
		v.publishLocked()
		v.bump()
		macs = append(macs, v.d.Name)
		v.mu.Unlock()
	}
	if len(macs) == 0 {
		return nil, refuse(http.StatusConflict, "None of your Macs is waiting for the lab's terms.")
	}
	s.log.Printf("account %d accepted the terms %s on My Macs for %v", p.AccountID, short(t.SHA256), macs)
	return macs, nil
}
