package server

// A Mac's enrollment ends from either side: its Volunteer runs
// `sudo lab-agent leave` or leaves from My Macs, a Maintainer ends it in
// labd, or its Volunteer is Blocked. Either way the Mac is gone from labd at
// once, its record Retired and its device secret revoked, and a job running
// on it ends incomplete. When it was ended in the lab, labd remembers whose
// secret it was and who ended it, so a Mac that checks in again with that
// secret learns it was ended, not just revoked, and cleans up.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// endedHeader carries, on the 401 an ended Mac's old secret gets, who
// ended its enrollment.
const endedHeader = api.EndedHeader

var errMacGone = errors.New("the Mac is no longer enrolled")

// goneReason is why the Mac left the lab, or "" while it is enrolled.
func (v *dev) goneReason() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.goneWhy
}

// leaving is how a Mac's enrollment ends: who ended it (leftByVolunteer,
// leftByMaintainer, leftBlocked) and the person who acted. by is the
// Maintainer who ended it or who Blocked its Volunteer; for the Volunteer
// it is the Volunteer when they left from the site, and nobody when the
// Mac itself told the lab (`sudo lab-agent leave`). atMac is that case:
// the Mac has left already and has nothing more to learn.
type leaving struct {
	how   string
	by    Person
	atMac bool
}

// reason is the ending in the words a job cut short by it carries.
func (l leaving) reason() string {
	switch l.how {
	case leftByMaintainer:
		return "the Maintainer " + l.by.Handle + " ended its enrollment"
	case leftBlocked:
		return "its Volunteer was Blocked"
	case leftForgotten:
		return "its Volunteer was Forgotten"
	}
	return "its Volunteer left the lab"
}

// endEnrollment has v leave the lab, as l says. Its record is Retired at
// once: out of the fleet, its name free, counting toward no cap, its device
// secret revoked. Its screenshots, serial log and baselines are deleted;
// its jobs, their results, labd's copy of its Audit log and the Mac's
// record stay for Maintainers. A job running on it ends incomplete. The
// leaving is a line of the Mac's record, of the People record and for the
// Lab group. Unless the Mac itself said it left, labd remembers whose
// secret it was and who ended it: a Mac that was out of reach learns it
// was ended when it next reaches the lab, and finishes leaving then. It is
// errMacGone when the Mac was not enrolled any more. The retirement is
// written first: when it cannot be, nothing has changed and the error says
// why.
func (s *Server) endEnrollment(v *dev, l leaving) error {
	reason := l.reason()
	v.mu.Lock()
	name, secretHash, e := v.d.Name, v.secretHash, v.d.Enrollment
	// As lab forget does: nothing still holding v may write its row back.
	if v.forgotten {
		v.mu.Unlock()
		return errMacGone
	}
	if v.resetting {
		// it would go to a retired Mac
		v.mu.Unlock()
		return refuse(http.StatusConflict, "a Hard reset of %s is on its way: end its enrollment once it has gone out and the Mac is back", name)
	}
	v.forgotten = true
	// The recovery ladder's Hard reset waiting at the controller is called
	// off, and no other sets out (resetOOB): the Mac is retired once the
	// controller's call has ended.
	if v.oobCallCancel != nil {
		v.oobCallCancel()
	}
	d := v.d
	v.mu.Unlock()
	v.oobCallMu.Lock()
	v.oobCallMu.Unlock()
	if s.dev(name) != v {
		return errMacGone
	}
	// Who ended it is written with the retirement, so its secret is never
	// unknown without an answer to why.
	var by *endedRow
	if !l.atMac {
		by = &endedRow{secretHash: secretHash, name: name, by: l.by.Handle}
	}
	if err := s.store.retire(&d, l.how, s.now(), by); err != nil {
		v.mu.Lock()
		v.forgotten = false
		v.mu.Unlock()
		s.log.Printf("retire %s: %v", name, err)
		return fmt.Errorf("%s's enrollment could not be ended: %w", name, err)
	}
	s.endLeaseOfLeaving(v, reason)
	s.retireAudit(v, name, d.ID)
	s.afterRetiring(&d, l)

	v.mu.Lock()
	v.goneWhy = reason
	v.d.Lease = nil
	active := v.d.ActiveJob
	close(v.stop)
	v.bump()
	v.mu.Unlock()
	s.log.Printf("%s: enrollment ended (%s)", name, l.how)

	// The job holding the Mac ends incomplete (see runJob); the ones queued
	// behind it, or waiting for the Mac or its window, have no Mac to run on.
	jobs, err := s.store.jobs(name, 1000, true)
	if err != nil {
		s.log.Printf("%s: its unfinished jobs: %v", name, err)
	}
	for _, j := range jobs {
		if j.ID != active {
			s.updateJobByID(j.ID, func(j *api.Job) {
				j.State, j.Outcome, j.Summary = api.JobDone, api.OutcomeCanceled, "canceled: "+reason
			})
		}
	}
	// After the marking: the Mac's loop may have read one as queued just
	// before it (see cancelJob), and its cancel is set by now.
	s.mu.Lock()
	cancels := []context.CancelFunc{s.cancels[active]}
	for _, j := range jobs {
		if j.ID != active {
			cancels = append(cancels, s.cancels[j.ID])
		}
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		if cancel != nil {
			cancel()
		}
	}

	if l.how != leftByVolunteer && e.IsVolunteer() {
		s.notify(s.notice(d, noticeEnded, jobWording{}))
	}
	return nil
}

// statusOr500 is a refusal's status, or 500 for any other error.
func statusOr500(err error) int {
	var r *refusal
	if errors.As(err, &r) {
		return r.code
	}
	return http.StatusInternalServerError
}

// retireAudit takes v out of the fleet, and renames labd's copy of its Audit
// log after the enrollment (id), while no report adds to it: an entry
// copied as the Mac leaves is in the renamed copy, and a report for it still
// on its way adds to the renamed copy too, never to a new one under the name
// the next Mac may take. A forgetting's sweep of the copies waits for those
// reports as it does for an enrolled Mac's (auditWrite).
func (s *Server) retireAudit(v *dev, name, id string) {
	s.auditWrite.RLock()
	defer s.auditWrite.RUnlock()
	v.auditMu.Lock()
	defer v.auditMu.Unlock()
	a := s.auditPath(name)
	renamed := strings.TrimSuffix(a, ".jsonl") + "." + id + ".jsonl"
	if err := os.Rename(a, renamed); err != nil && !os.IsNotExist(err) {
		s.log.Printf("forget %s: audit log: %v", name, err)
	}
	s.mu.Lock()
	delete(s.devs, name)
	if s.retiredAudit == nil {
		s.retiredAudit = map[*dev]string{}
	}
	s.retiredAudit[v] = renamed
	s.mu.Unlock()
}

// afterRetiring is what follows a Mac's record being Retired.
func (s *Server) afterRetiring(d *api.Device, l leaving) {
	line := "left the lab: its Volunteer ended it"
	switch l.how {
	case leftByMaintainer:
		line = "left the lab: a Maintainer ended it"
		s.labGroup.MacLeftByMaintainer(d.Name)
	case leftBlocked:
		line = "left the lab: its Volunteer was Blocked"
		s.labGroup.MacLeftVolunteerBlocked(d.Name)
	case leftForgotten:
		line = "left the lab: a Maintainer ended it"
		s.labGroup.MacLeftByMaintainer(d.Name)
	default:
		s.labGroup.MacLeftByVolunteer(d.Name)
	}
	s.macSaid(d.Name, d.ID, macLeft, line)
	s.leftOnPeopleRecord(d, l)
	if _, err := s.store.dropTokens(d.Name); err != nil {
		s.log.Printf("retire %s: its enrollment tokens: %v", d.Name, err)
	}
	s.dropNamedState(d.Name)
}

// leftOnPeopleRecord writes a Mac's leaving in the People record, which is
// for Maintainers: who ended it, and whose Mac it was.
func (s *Server) leftOnPeopleRecord(d *api.Device, l leaving) {
	var whose *Person
	if d.VolunteerID != 0 {
		if p, err := s.person(d.VolunteerID); err == nil {
			whose = &p
		}
	}
	line := recordLine{At: s.now(), ActorID: l.by.AccountID, Actor: l.by.Handle}
	switch {
	case l.how == leftByVolunteer && whose == nil:
		return // a Mac enrolled before the lab kept people: nobody to name
	case l.how == leftByVolunteer:
		line.ActorID, line.Actor, line.What = whose.AccountID, whose.Handle, "left the lab with "+d.Name
	case l.by.Handle == "":
		return
	default:
		line.What = "ended the enrollment of " + d.Name
		switch l.how {
		case leftBlocked:
			line.What = "blocked its Volunteer, which ended the enrollment of " + d.Name
		case leftForgotten:
			line.What = "forgot its Volunteer, which ended the enrollment of " + d.Name
		}
		if whose != nil {
			line.What += ", a Mac of"
			line.AboutID, line.About = whose.AccountID, whose.Handle
		}
	}
	if err := s.store.addRecord(line); err != nil {
		s.log.Printf("the People record, %s's leaving: %v", d.Name, err)
	}
}

// retiringWait is how long endEnrollmentsOf waits for a Mac of the
// person's that is leaving the lab already.
var retiringWait = 10 * time.Second

// endEnrollmentsOf has every Mac enrolled under the person's account id
// leave the lab, as l says, the ones that told the lab they are leaving
// among them. A Mac another ending holds is waited for: when that ending
// fails, this one ends it. A Mac that could not leave is in the error, and
// still enrolled: its secret still works.
func (s *Server) endEnrollmentsOf(accountID int64, l leaving) error {
	if accountID == 0 {
		return nil
	}
	deadline := s.now().Add(retiringWait)
	for {
		names, err := scanNames(s.store.db.Query(`SELECT name FROM devices WHERE json_extract(data, '$.volunteer_id') = ? ORDER BY name`, accountID))
		if err != nil {
			return err
		}
		var errs []error
		for _, name := range names {
			v := s.dev(name)
			if v == nil {
				continue
			}
			if err := s.endEnrollment(v, l); err != nil && !errors.Is(err, errMacGone) {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 || len(names) == 0 {
			return errors.Join(errs...)
		}
		if s.now().After(deadline) {
			return fmt.Errorf("%s is leaving the lab already, and has not left yet", strings.Join(names, ", "))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// leaveMac is the Mac's Volunteer leaving the lab from the site. The Mac
// learns of it when it next reaches the lab, and removes the lab then.
func (s *Server) leaveMac(v *dev, by Person) error {
	if err := s.lends(v, by); err != nil {
		return err
	}
	err := s.endEnrollment(v, leaving{how: leftByVolunteer, by: by})
	if errors.Is(err, errMacGone) {
		return refuse(http.StatusNotFound, "No Mac of yours has that name.")
	}
	return err
}

// hEndEnrollment is a Maintainer ending a Mac's enrollment (`lab remove`).
func (s *Server) hEndEnrollment(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	v := s.dev(name)
	err := errMacGone
	if v != nil {
		err = s.endEnrollment(v, leaving{how: leftByMaintainer, by: leaser(r)})
	}
	switch {
	case errors.Is(err, errMacGone):
		httpErr(w, 404, "no device %q", name)
	case err != nil:
		httpErr(w, statusOr500(err), "%v", err)
	default:
		w.Write([]byte("enrollment ended\n"))
	}
}

// hAgentLeave is a Mac telling labd its Volunteer has left the lab.
func (s *Server) hAgentLeave(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	if err := s.endEnrollment(v, leaving{how: leftByVolunteer, atMac: true}); err != nil && !errors.Is(err, errMacGone) {
		httpErr(w, 500, "%v", err)
		return
	}
	w.Write([]byte("left\n"))
}

// ended answers a request carrying the secret of a Mac whose enrollment a
// Maintainer ended: 401, with the Maintainer's name for the Mac to read. A
// Maintainer Forgotten since is "a Maintainer": the Mac leaves on any name,
// and on none at all it would not.
func (s *Server) ended(w http.ResponseWriter, r *http.Request, secret string) bool {
	by, err := s.store.endedBy(hashSecret(secret))
	if err != nil {
		return false
	}
	by = orStr(by, "a Maintainer")
	w.Header().Set(endedHeader, by)
	s.refuse(w, r, 401, "enrollment ended by %s", by)
	return true
}

// endedRow is who ended a Mac's enrollment from the lab, for the Mac that
// holds secretHash to learn when it next reaches the lab.
type endedRow struct{ secretHash, name, by string }

// endedBy is the Maintainer who ended the enrollment the secret belonged to.
func (st *store) endedBy(secretHash string) (string, error) {
	var by string
	err := st.db.QueryRow(`SELECT maintainer FROM ended WHERE secret_hash=?`, secretHash).Scan(&by)
	return by, err
}
