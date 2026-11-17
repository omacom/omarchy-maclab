package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// A Volunteer's Mac has an Access level, chosen by its Volunteer on the Mac:
// at Qualification (the default) labd takes no lab exec and no Maintainer's
// script for it, and lab-agent refuses them whatever labd sends. Its screen
// and live journal are watched only while a job or lease holds it inside its
// window. Scope (see mayDo) is where labd decides these. Every Maintainer
// action reaching it lands in its Audit log, which the Mac reports to labd
// for a copy. Lab-owned Macs have none of this.

// errAccessLevel matches lab-agent refusing or stopping a command because
// the Mac's Access level doesn't allow it.
var errAccessLevel = errors.New("the Mac's Access level doesn't allow it")

// accessCut is the outcome of a job cut short (what says when) because the
// Volunteer lowered the Access level: incomplete, as when the window closes.
// lab-agent stopped it by itself.
func accessCut(what string) (string, string) {
	return api.OutcomeIncomplete, "the Volunteer lowered the Mac's Access level to Qualification " + what
}

// syncAccessLocked takes the Access level and publishing opt-in a check-in
// carried as labd's copy, and says whether they changed. Each change is an
// event of the Mac's record and a line for the Lab group. Lowering the level
// ends a lease at once. Only a Volunteer's Mac has them. Caller holds v.mu.
func (v *dev) syncAccessLocked(ci api.Checkin) bool {
	if !v.d.IsVolunteer() || ci.AccessLevel == "" {
		return false
	}
	if v.d.AccessLevel == ci.AccessLevel && v.d.Publish == ci.Publish {
		return false
	}
	lowered := v.d.AccessLevel == api.AccessFull && ci.AccessLevel != api.AccessFull
	raised := v.d.AccessLevel != api.AccessFull && ci.AccessLevel == api.AccessFull
	published := v.d.Publish != ci.Publish
	// A Mac limited from the site was at Qualification for the lab already:
	// lowering it on the Mac takes nothing more away.
	cuts := lowered && !v.d.Limited
	v.d.AccessLevel, v.d.Publish = ci.AccessLevel, ci.Publish
	switch {
	case raised:
		v.said(macAccessLevel, "its Volunteer raised its Access level to Full")
		v.s.labGroup.AccessLevelRaised(v.d.Name)
	case lowered:
		v.said(macAccessLevel, "its Volunteer lowered its Access level to Qualification")
		v.s.labGroup.AccessLevelLowered(v.d.Name)
	}
	if published {
		v.said(macPublishing, "its Volunteer turned publishing "+map[bool]string{true: "on", false: "off"}[ci.Publish])
		v.s.labGroup.Publishing(v.d.Name, ci.Publish)
	}
	if cuts {
		v.endLeaseLocked("its Volunteer lowered the Access level")
	}
	return true
}

// endLeaseLocked ends the Mac's Lease now because its Volunteer took
// something away (why); the keeper records and posts the ending. It says
// whether a Lease was held. Caller holds v.mu.
func (v *dev) endLeaseLocked(why string) bool {
	l := v.liveLeaseLocked()
	if l == nil {
		return false
	}
	v.s.log.Printf("%s: %s; its Lease ends", v.d.Name, why)
	ended := *l
	ended.Expires = v.s.now()
	v.d.Lease, v.leaseCut = &ended, why
	return true
}

// ---- Limit to Qualification, taken on the site ----

// siteLimited is why a Limit its Volunteer took on the site cut a job or a
// Lease short.
const siteLimited = "its Volunteer limited the Mac to Qualification from the site"

// limitMac is the Mac's Volunteer limiting it to Qualification on the
// site, whatever Access level the Mac has: until "Remove limit" the lab
// treats it as at Qualification. As a Pause does, at either level, it stops
// a running job, incomplete as a closing window stops one, and ends a
// Lease.
func (s *Server) limitMac(v *dev, by Person) error {
	if err := s.lends(v, by); err != nil {
		return err
	}
	v.mu.Lock()
	if v.d.Limited {
		v.mu.Unlock()
		return refuse(http.StatusConflict, "This Mac is limited to Qualification already.")
	}
	v.d.Limited = true
	active := v.cutJobLocked(siteLimited)
	v.endLeaseLocked(siteLimited)
	v.said(macLimited, "its Volunteer limited it to Qualification from the site")
	name := v.d.Name
	v.saveLocked()
	v.publishLocked()
	v.bump()
	v.mu.Unlock()
	s.labGroup.Limited(name)
	if active != "" {
		s.cancelJob(active, siteLimited)
	}
	s.lapse(v)
	return nil
}

// unlimitMac is "Remove limit": it undoes the Limit taken on the site and
// nothing else. The Mac is back at the Access level its Volunteer set on
// it, which the site cannot raise.
func (s *Server) unlimitMac(v *dev, by Person) error {
	if err := s.lends(v, by); err != nil {
		return err
	}
	v.mu.Lock()
	if !v.d.Limited {
		v.mu.Unlock()
		return refuse(http.StatusConflict, "No Limit taken on the site holds this Mac. The site undoes only what it took: the Access level is raised on the Mac (sudo lab-agent access full).")
	}
	v.d.Limited = false
	v.said(macLimitRemoved, "its Volunteer removed the limit from the site")
	name := v.d.Name
	v.saveLocked()
	v.publishLocked()
	v.bump()
	v.mu.Unlock()
	s.labGroup.LimitRemoved(name)
	return nil
}

// needsFull is why a job spec needs a Volunteer's Mac at Full, or "": a
// Maintainer's script, as root or in the Volunteer's session.
func needsFull(spec api.JobSpec) string {
	if spec.Mesa != nil {
		return "staged Mesa packages"
	}
	for _, t := range spec.Tests {
		switch {
		case t.MaintainerScript() && t.GUI:
			return "test " + t.Name + " is a --gui-script"
		case t.MaintainerScript():
			return "test " + t.Name + " is a --script"
		}
	}
	return ""
}

// heldLocked is whether a job or lease holds the Mac now. Caller holds v.mu.
func (v *dev) heldLocked() bool {
	return v.d.ActiveJob != "" || v.liveLeaseLocked() != nil
}

// ---- labd's copy of each Mac's Audit log ----

func (s *Server) auditPath(name string) string {
	return filepath.Join(s.cfg.DataDir, "audit", name+".jsonl")
}

// auditFile is labd's copy of v's Audit log: under its name while it is
// enrolled, renamed after its enrollment once it is retired (retireAudit).
func (s *Server) auditFile(v *dev) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.retiredAudit[v]; ok {
		return p
	}
	return s.auditPath(v.d.Name)
}

// readAuditCopy reads labd's copy of a Mac's Audit log.
func (s *Server) readAuditCopy(name string) ([]api.AuditEntry, error) {
	return readAuditFile(s.auditPath(name))
}

func readAuditFile(path string) ([]api.AuditEntry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []api.AuditEntry
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e api.AuditEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// auditHaveLocked is how much of the Mac's Audit log labd's copy holds, in
// bytes of the Mac's log. Caller holds v.auditMu.
func (v *dev) auditHaveLocked() int64 {
	if v.auditHave < 0 {
		v.auditHave = 0
		if es, err := readAuditFile(v.s.auditFile(v)); err == nil && len(es) > 0 {
			v.auditHave = es[len(es)-1].End
		}
	}
	return v.auditHave
}

// auditHaveNow is auditHaveLocked for callers not holding v.auditMu.
func (v *dev) auditHaveNow() int64 {
	v.auditMu.Lock()
	defer v.auditMu.Unlock()
	return v.auditHaveLocked()
}

// copyAudit adds the entries a report carried to labd's copy, each once.
func (v *dev) copyAudit(es []api.AuditEntry) {
	if len(es) == 0 {
		return
	}
	v.s.auditWrite.RLock()
	defer v.s.auditWrite.RUnlock()
	v.auditMu.Lock()
	defer v.auditMu.Unlock()
	have := v.auditHaveLocked()
	var b bytes.Buffer
	for _, e := range es {
		if e.End <= have {
			continue
		}
		e.AccountID = v.s.auditAccount(e)
		if e.AccountID == 0 {
			// A handle that is nobody's now may be a forgotten person's,
			// named by a Mac that was out of reach at the forgetting.
			e.Maintainer = ""
		}
		line, _ := json.Marshal(e)
		b.Write(append(line, '\n'))
		have = e.End
	}
	if b.Len() == 0 {
		return
	}
	p := v.s.auditFile(v)
	err := os.MkdirAll(filepath.Dir(p), 0o755)
	var f *os.File
	if err == nil {
		f, err = os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	}
	if err == nil {
		_, err = f.Write(b.Bytes())
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		v.s.log.Printf("%s: Audit log copy: %v", v.d.Name, err)
		return
	}
	v.auditHave = have
}

// auditAccount is the account of the person an Audit log entry names, as
// labd knows it when the entry arrives: the Mac wrote their handle at the
// time, and a later rename must not change whose entry it is. 0 when labd
// or the Mac's Volunteer acted, or the handle is nobody's now.
func (s *Server) auditAccount(e api.AuditEntry) int64 {
	if e.Maintainer == "" {
		return 0
	}
	if e.Job != "" {
		if j, err := s.store.job(e.Job); err == nil && j.AccountID != 0 && j.Maintainer == e.Maintainer {
			return j.AccountID
		}
	}
	var id int64
	s.store.db.QueryRow(`SELECT account_id FROM people WHERE handle = ? COLLATE NOCASE ORDER BY last_seen DESC LIMIT 1`, e.Maintainer).Scan(&id)
	return id
}

// hAudit is labd's copy of a Volunteer Mac's Audit log, for Maintainers and
// the Mac's Volunteer. The M team reads the fleet's jobs, not this.
func (s *Server) hAudit(w http.ResponseWriter, r *http.Request) {
	if c := callerOf(r); c.team != "" {
		httpErr(w, 403, "@%s is an M team member; the Audit log is for Maintainers and the Mac's Volunteer", c.team)
		return
	}
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	es, err := s.readAuditCopy(v.d.Name)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if es == nil {
		es = []api.AuditEntry{}
	}
	writeJSON(w, es)
}

// hAgentPublished lists the reports the lab published from a Volunteer's
// Mac's jobs with their deletion links, for `lab-agent publish on`.
func (s *Server) hAgentPublished(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	jobs, err := s.store.jobs(v.snapshot().Name, 1000, false)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, s.publishedOf(v.snapshot().ID, jobs, s.publicBase(r)))
}
