package server

// A Lease holds a Mac for one person, a Maintainer or an M team member: the
// account id here, the handle in the field the agent's commands already
// carry, the free-text holder label inside it. Everything about it is
// labd's; the running agent knows nothing of Leases.
//
// A Lease fences jobs, lab exec, screenshots, the live journal and reset
// (fence, and mayDo for the two views). How it ends decides what becomes of
// its holder's work (endLease, lapse). Every event is in the Lease record
// (lease_record.go) and, except a renewal and an extension, a line for the
// Lab group.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// A Lease lasts leaseDefault unless a Maintainer asks for longer, up to
// leaseLongest. A renewal with no length asked ends leaseDefault from now.
const (
	leaseDefault = 4 * time.Hour
	leaseLongest = 24 * time.Hour
)

// leaser is the person r asks about a Lease as, by Lab token or browser
// session; nobody for neither, who takes no Lease. A request a Maintainer's
// route admitted with neither acts as the Maintainer it named.
func leaser(r *http.Request) Person {
	if p := credentialOf(r); p != nil {
		return *p
	}
	if name := maintainerName(r); name != "" {
		return Person{Handle: name, Maintainer: true}
	}
	return Person{}
}

// teamUp admits a Maintainer or an M team member: the routes of a Lease
// and of what a Lease fences.
// The request ends when the role or the mark goes, or the session or the
// Lab token it came with.
func (s *Server) teamUp(h http.HandlerFunc) http.HandlerFunc {
	const need = "this is for the M team and Maintainers"
	return func(w http.ResponseWriter, r *http.Request) {
		switch a, who := s.access(r); a {
		case adminAccess:
			s.asMaintainer(w, r, who, h)
		case teamAccess:
			s.asSignedIn(w, r, teamKey{}, h)
		case ownAccess:
			httpErr(w, 403, "@%s lends a Mac and sees their own Macs; %s", who, need)
		case signedInAccess:
			httpErr(w, 403, "@%s is signed in with no role in the lab; %s", who, need)
		case viewAccess:
			httpErr(w, 403, "%s may only look; %s", who, need)
		case blockedAccess:
			httpErr(w, 403, "%s", blockedSentence)
		default:
			s.noCredential(w, r)
		}
	}
}

// leaseIsOf is whether l is p's Lease, whatever its holder label: by account
// id, and for a Lease taken before labd kept people, by the name recorded.
func leaseIsOf(l *api.Lease, p Person) bool {
	if l.AccountID != 0 {
		return l.AccountID == p.AccountID
	}
	return l.Owner() != "" && l.Owner() == p.Handle
}

// liveLeaseLocked is the Mac's Lease while it runs, else nil: until its
// end, and past it while the holder's job that was running then still runs
// (see lapse). A Lease the Mac's Volunteer or its window cut short is never
// stretched so. Caller holds v.mu.
func (v *dev) liveLeaseLocked() *api.Lease {
	l := v.d.Lease
	switch {
	case l == nil:
		return nil
	case v.s.now().Before(l.Expires):
		return l
	case l.Job != "" && l.Job == v.d.ActiveJob && v.leaseCut == "":
		return l
	}
	return nil
}

// leased is whether a Lease holds the Mac now: labd's scheduled refresh
// skips it, and it is not free.
func (s *Server) leased(v *dev) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.liveLeaseLocked() != nil
}

// holdsLease is whether p holds the Lease on v now, under any holder label.
func (s *Server) holdsLease(v *dev, p Person) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	l := v.liveLeaseLocked()
	return l != nil && leaseIsOf(l, p)
}

// leaseAsked is what the Lease rule found for a person asking for a Lease
// on a Mac: that they would renew the one they hold, or what a Maintainer
// is warned of before going on.
type leaseAsked struct {
	renews  bool
	warning string
}

// mayLease is the Lease rule for p taking or renewing a Lease on v under
// this holder label: a refusal, or what was found. Only a Maintainer or an
// M team member; only a Mac that passed its baseline and has not left; a
// Volunteer's Mac only inside its Availability window, at either Access
// level; not while someone else holds it; and one Lease per Configuration
// for an M team member, where a Maintainer is only warned.
func (s *Server) mayLease(v *dev, p Person, holder string) (leaseAsked, error) {
	var found leaseAsked
	if !p.OnMTeam() {
		return found, refuse(403, "a Lease is for the M team and Maintainers")
	}
	v.mu.Lock()
	name := v.d.Name
	var err error
	switch l := v.liveLeaseLocked(); {
	case v.forgotten || v.d.State == api.StateLeft:
		err = refuse(409, "%s has left the lab", name)
	case v.d.KnownGood == "":
		err = refuse(409, "%s has not passed its baseline yet, so it takes no Lease", name)
	case v.termsWait:
		err = refuse(409, "%s", termsWaitingLine(name))
	case v.d.IsVolunteer() && v.openUntilLocked().IsZero():
		err = refuse(409, "%s's Availability window is closed: a Lease on a Volunteer's Mac lasts only inside it", name)
	case l != nil && !(leaseIsOf(l, p) && l.Holder == holder):
		err = refuse(409, "%s is leased by %s %s", name, l.Who(), until(l))
	case l != nil:
		found.renews = true
	}
	v.mu.Unlock()
	if err != nil || found.renews {
		return found, err
	}
	if held, same := s.heldOfConfiguration(v, p); held != "" {
		if !p.isMaintainer() {
			return found, refuse(409, "@%s already holds a Lease on %s: one Lease per Configuration for an M team member. Release %s first", p.Handle, same, held)
		}
		found.warning = "you already hold a Lease on " + same + "; an M team member would be refused a second"
	}
	return found, nil
}

// heldOfConfiguration names a Mac other than v that p leases now and that
// counts as v's Configuration, and says in what way it does; "" when p
// holds none.
func (s *Server) heldOfConfiguration(v *dev, p Person) (held, same string) {
	mine := s.configuration(v)
	for _, o := range s.allDevs() {
		if o == v || !s.holdsLease(o, p) {
			continue
		}
		theirs := s.configuration(o)
		if !mine.Same(theirs) {
			continue
		}
		name := o.snapshot().Name
		if mine.Known() && theirs.Known() {
			return name, fmt.Sprintf("%s, the same Configuration (%s)", name, theirs)
		}
		return name, fmt.Sprintf("%s (%s), and %s (%s) counts as the same Configuration until a Maintainer sets the core counts", name, theirs, v.snapshot().Name, mine)
	}
	return "", ""
}

// takeLease takes a Lease on v for p, or renews the one p holds under this
// holder label, for ttl; 0 is leaseDefault. On a Volunteer's Mac it is cut
// to the window's end. The Lease it returns carries the warning, which is
// not kept.
func (s *Server) takeLease(v *dev, p Person, holder string, ttl time.Duration) (api.Lease, error) {
	if holder == "" {
		return api.Lease{}, refuse(400, "holder required")
	}
	if ttl <= 0 {
		ttl = leaseDefault
	}
	switch {
	case p.isMaintainer() && ttl > leaseLongest:
		return api.Lease{}, refuse(400, "a Lease lasts 24 hours at most")
	case !p.isMaintainer() && ttl > leaseDefault:
		return api.Lease{}, refuse(400, "an M team member's Lease lasts 4 hours at most; renew it at any time, and it ends 4 hours from then")
	}
	s.lapse(v)
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	found, err := s.mayLease(v, p, holder)
	if err != nil {
		return api.Lease{}, err
	}
	v.mu.Lock()
	if err := v.updatingRefusalLocked("no Lease was taken"); err != nil {
		v.mu.Unlock()
		return api.Lease{}, err
	}
	lease := &api.Lease{Holder: holder, Maintainer: p.Handle, By: p.Handle, AccountID: p.AccountID, Expires: s.now().Add(ttl)}
	if had := v.liveLeaseLocked(); had != nil {
		lease.Job = had.Job // a renewal keeps the job running under it, and is no longer an extension
	}
	v.d.Lease, v.leaseCut = lease, ""
	v.capLeaseLocked()
	v.saveLocked()
	v.publishLocked()
	taken, name, id := *v.d.Lease, v.d.Name, v.d.ID
	v.mu.Unlock()
	if found.renews {
		s.recordLease(name, id, &taken, api.LeaseRenewed, p, "")
	} else {
		s.recordLease(name, id, &taken, api.LeaseTaken, p, "")
		s.labGroup.LeaseTaken(p.Handle, name, taken.Expires)
	}
	taken.Warning = found.warning
	return taken, nil
}

// releaseLease ends p's Lease on v; holder, when given, must be its label.
// With force a Maintainer ends another person's, saying why if they wish,
// which is recorded.
func (s *Server) releaseLease(v *dev, p Person, holder string, force bool, why string) error {
	s.lapse(v)
	v.mu.Lock()
	l := v.liveLeaseLocked()
	name := v.d.Name
	v.mu.Unlock()
	changed := func() error {
		return refuse(409, "%s's lease changed meanwhile; look again before releasing it", name)
	}
	switch {
	case l == nil:
	case leaseIsOf(l, p) && (holder == "" || l.Holder == holder):
		if !s.endLease(v, l, leaseEnding{event: api.LeaseReleased}) {
			return changed()
		}
	case !force && !leaseIsOf(l, p):
		return refuse(409, "%s is leased by %s %s; only %s can release it, or a Maintainer with `lab release %s --force`", name, l.Who(), until(l), orStr(l.Owner(), l.Holder), name)
	case !force:
		return refuse(409, "leased by %s, not %s", l.Holder, holder)
	case !p.isMaintainer():
		return refuse(403, "only a Maintainer ends another person's Lease")
	default:
		end := endedByMaintainer(p, why)
		end.note = "ended early by Maintainer " + p.Handle + " (lab release --force)"
		if !s.endLease(v, l, end) {
			return changed()
		}
	}
	return nil
}

func (s *Server) hLease(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	var req api.LeaseRequest
	json.NewDecoder(r.Body).Decode(&req)
	l, err := s.takeLease(v, leaser(r), req.Holder, time.Duration(req.TTLSec)*time.Second)
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, l)
}

func (s *Server) hRelease(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	q := r.URL.Query()
	force, _ := strconv.ParseBool(q.Get("force"))
	why, err := leaseWhy(q.Get("why"))
	if err == nil {
		err = s.releaseLease(v, leaser(r), q.Get("holder"), force, why)
	}
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	w.Write([]byte("ok"))
}

// leaseWhy is the reason a Maintainer typed for ending a Lease: one line,
// which the Lab group reads.
func leaseWhy(raw string) (string, error) {
	why := strings.TrimSpace(raw)
	if len([]rune(why)) > 200 || strings.ContainsFunc(why, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return "", refuse(http.StatusUnprocessableEntity, "The reason for ending a Lease is one line of up to 200 characters.")
	}
	return why, nil
}

// ---- what a Lease fences ----

// fenced is something a Lease fences on its Mac.
type fenced struct {
	what string
	// own: nobody does it without their own Lease, a Maintainer included.
	own bool
}

var (
	fenceJobs  = fenced{what: "jobs"}
	fenceReset = fenced{what: "a reset"}
	// fenceExec is lab exec: the one exception to "a Maintainer may do
	// everything on any Mac".
	fenceExec = fenced{what: "lab exec", own: true}
)

// fence is why p may not do f on v now, or nil. An M team member needs
// their own Lease on the Mac. A Maintainer acts on a free Mac, and under
// another person's Lease waits or ends it. A Lease extended for its running
// job passes nothing new, its holder's either. Screenshots and the live
// journal are fenced in mayDo, where a Volunteer's Mac's rules for them are.
func (s *Server) fence(v *dev, p Person, f fenced) error {
	if !p.OnMTeam() {
		return refuse(403, "%s on a Mac is for the M team and Maintainers", f.what)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	l, name := v.liveLeaseLocked(), v.d.Name
	switch {
	case l != nil && leaseIsOf(l, p):
		if l.Extended {
			return refuse(409, "%s", extendedOnly(l, name, f.what))
		}
		return nil
	case !p.isMaintainer():
		return refuse(403, "@%s needs their own Lease on %s for %s: `lab lease %s`%s", p.Handle, name, f.what, name, leasedBy(l))
	case l != nil:
		return refuse(409, "%s is leased by %s %s; its holder does that, or a Maintainer ends the Lease first (`lab release %s --force`)", name, l.Who(), until(l), name)
	case f.own:
		return refuse(409, "%s needs your own Lease on %s, a Maintainer's too: `lab lease %s`", f.what, name, name)
	}
	return nil
}

// extendedOnly is what the holder of l, a Lease extended for its running
// job, is told when they ask for something new under it: the extension is
// that job's and passes nothing else.
func extendedOnly(l *api.Lease, mac, what string) string {
	return fmt.Sprintf("your Lease on %s ran out at %s and lasts only until job %s ends, with nothing new under it: renew it (`lab lease %s`) for %s", mac, l.Expires.Format(time.TimeOnly), l.Job, mac, what)
}

// until says when l ends: at its end, or, extended past it, with its job.
func until(l *api.Lease) string {
	if l.Extended {
		return "until its job " + l.Job + " ends (extended: it ran out at " + l.Expires.Format(time.TimeOnly) + ")"
	}
	return "until " + l.Expires.Format(time.TimeOnly)
}

// leasedBy says, after a refusal, who holds the Mac instead; "" when free.
func leasedBy(l *api.Lease) string {
	if l == nil {
		return ""
	}
	return " (it is leased by " + l.Who() + " " + until(l) + ")"
}

// leaseConnKey names the sessions a person has open under their Lease on a
// Mac: they are cut when it ends.
func leaseConnKey(mac string, accountID int64) string {
	return "lease:" + mac + ":" + strconv.FormatInt(accountID, 10)
}

// underLease is r as a session of p's Lease on v, when p holds it: it is
// cut when the Lease ends. done must be called when the request is over.
func (s *Server) underLease(r *http.Request, v *dev, p Person) (*http.Request, func()) {
	if !s.holdsLease(v, p) {
		return r, func() {}
	}
	ctx, cancel := context.WithCancel(r.Context())
	return r.WithContext(ctx), s.conns.add(cancel, leaseConnKey(v.snapshot().Name, p.AccountID))
}

// ---- whose a job is ----

// jobOwner is who a queued job is for, as a Lease asks it.
type jobOwner struct {
	account            int64
	holder, maintainer string
}

func ownerOf(j *api.Job) jobOwner {
	return jobOwner{account: j.AccountID, holder: j.Spec.Holder, maintainer: j.Maintainer}
}

// leaseCovers is whether a job of o's runs under l: its person's own
// Lease, whatever its holder label. A Lease or a job from before labd kept
// the account goes by the names recorded.
func leaseCovers(l *api.Lease, o jobOwner) bool {
	if l.AccountID != 0 && o.account != 0 {
		return l.AccountID == o.account
	}
	return l.HeldBy(o.holder, o.maintainer)
}

// submitter is the person a job acts for, with their roles now; nobody for
// a job labd queued itself. A job from before labd kept the account acts
// for the Maintainer named.
func (s *Server) submitter(j *api.Job) Person {
	if j.AccountID != 0 {
		if p, err := s.person(j.AccountID); err == nil {
			return p
		}
		return Person{AccountID: j.AccountID, Handle: j.Maintainer}
	}
	return Person{Handle: j.Maintainer, Maintainer: j.Maintainer != ""}
}

// withoutItsLease is why a job may not start on v now, or "": an M team
// member's job runs only under their own Lease.
func (r *jobRun) withoutItsLease() string {
	if r.j.AccountID == 0 {
		return ""
	}
	p := r.s.submitter(r.j)
	r.v.mu.Lock()
	defer r.v.mu.Unlock()
	return r.withoutItsLeaseLocked(p)
}

// withoutItsLeaseLocked is withoutItsLease for p, the job's submitter.
// Caller holds r.v.mu.
func (r *jobRun) withoutItsLeaseLocked(p Person) string {
	if r.j.AccountID == 0 || p.isMaintainer() {
		return ""
	}
	if l := r.v.liveLeaseLocked(); l != nil && leaseIsOf(l, p) {
		return ""
	}
	return "@" + r.j.Maintainer + " holds no Lease on " + r.j.Spec.Device + " any more"
}

// parkedJob is a queued job someone else's lease holds back: who it is for,
// so labd can tell when the lease no longer holds it.
type parkedJob struct {
	id string
	of jobOwner
}

// leaseHoldsBackLocked says why the Mac's lease keeps a job of o's from
// running now, or "" when it doesn't: no lease is live, or the job is its
// holder's own. Caller holds v.mu.
func (v *dev) leaseHoldsBackLocked(o jobOwner) string {
	l := v.liveLeaseLocked()
	if l == nil || leaseCovers(l, o) {
		return ""
	}
	return fmt.Sprintf("%s is leased by %s %s", v.d.Name, l.Who(), until(l))
}

// park sets a queued job aside while someone else's lease holds the Mac, so
// the lease holder's jobs queued after it run first, and says whether it did,
// or found the job ended (a Leave, a cancel): either way its run stops here.
// unpark queues it again once the lease no longer holds it.
func (r *jobRun) park() bool {
	v := r.v
	v.mu.Lock()
	why := v.leaseHoldsBackLocked(ownerOf(r.j))
	v.mu.Unlock()
	if why == "" {
		return false
	}
	msg := why + "; waiting for the lease to end"
	saved := false // f runs only on a job that is not done
	r.s.updateJobUnlessDone(r.j, func(j *api.Job) {
		saved = true
		j.State = api.JobQueued
		j.Events = append(j.Events, api.JobEvent{Time: r.s.now(), Msg: msg})
	})
	if !saved {
		return true
	}
	r.s.log.Printf("job %s (%s): %s", r.j.ID, r.j.Spec.Device, msg)
	v.mu.Lock()
	v.parked = append(v.parked, parkedJob{r.j.ID, ownerOf(r.j)})
	v.mu.Unlock()
	return true
}

// unpark queues again, in the order they were set aside, the jobs a lease no
// longer holds back: it ended, ran out or was released. It never waits for
// room in the queue: deviceLoop, the queue's only reader, calls it, so a
// full queue would stop the Mac for good. What doesn't fit stays parked, in
// order, for the next pass.
func (v *dev) unpark() {
	v.mu.Lock()
	defer v.mu.Unlock()
	keep := v.parked[:0]
	full := false
	for _, p := range v.parked {
		if !full && v.leaseHoldsBackLocked(p.of) == "" {
			select {
			case v.queue <- p.id:
				continue
			default:
				full = true
			}
		}
		keep = append(keep, p)
	}
	clear(v.parked[len(keep):])
	v.parked = keep
}

// ---- how a Lease ends ----

// leaseEnding is how a Lease ends: released by its holder, expired, or
// ended over its holder by someone, for a cause.
type leaseEnding struct {
	event string // api.LeaseReleased, LeaseExpired or LeaseEnded
	by    Person // who ended it, when that is not its holder
	why   string
	cause leaseCause
	// note is what labd's log and the jobs the Lease had set aside say, when
	// that is worded apart from why.
	note string
	// asToday is a Lease its window's end, a Pause or a lowered Access level
	// ended: lab-agent stops what runs, and the holder's queued jobs stay
	// queued, as they did before Leases had endings. The Lease of a Mac that
	// leaves is one too: its jobs are the leaving's to end.
	asToday bool
}

type leaseCause int

const (
	byAMaintainer leaseCause = iota
	leftTeam
	blocked
	macWentAway
)

// endedByMaintainer is a Lease a Maintainer ended; why may be empty.
func endedByMaintainer(by Person, why string) leaseEnding {
	return leaseEnding{event: api.LeaseEnded, by: by, why: why}
}

// leftTheTeam is the Lease of a person whom by took off the M team.
func leftTheTeam(by Person) leaseEnding {
	return leaseEnding{event: api.LeaseEnded, by: by, why: "left the M team", cause: leftTeam}
}

// wasBlocked is the Lease of a person whom by Blocked.
func wasBlocked(by Person) leaseEnding {
	return leaseEnding{event: api.LeaseEnded, by: by, why: "Blocked", cause: blocked}
}

// allAccessEnded is the Lease of a person for whom "End all access" was
// used, by themselves or by a Maintainer: it ends as when a Maintainer ends
// it.
func allAccessEnded(by Person) leaseEnding {
	return leaseEnding{event: api.LeaseEnded, by: by, why: `"End all access"`}
}

// leftWithItsMac is the Lease on a Mac that leaves the lab, how says in
// which way. The leaving itself ends every job on the Mac, so the Lease's
// end only cuts its holder's sessions.
func leftWithItsMac(how string) leaseEnding {
	return leaseEnding{event: api.LeaseEnded, why: "the Mac left the lab (" + how + ")", cause: macWentAway, asToday: true}
}

// endLeaseOfLeaving ends the Lease on v, a Mac that is leaving the lab,
// before it goes: a Lease is never dropped without its line.
func (s *Server) endLeaseOfLeaving(v *dev, how string) {
	v.mu.Lock()
	l := v.d.Lease
	v.mu.Unlock()
	s.endLease(v, l, leftWithItsMac(how))
}

// over is whether the Lease ends over its holder: its running job is then
// canceled as `lab cancel` does. Released or run out, the job finishes.
func (e leaseEnding) over() bool { return e.event == api.LeaseEnded }

// logged is an ending over the holder in a few words for labd's log, which
// names people by account only.
func (e leaseEnding) logged() string {
	switch e.cause {
	case leftTeam:
		return "ended: its holder left the M team"
	case blocked:
		return "ended: its holder was Blocked"
	case macWentAway:
		return "ended: the Mac left the lab"
	}
	return "ended by " + accountOr(e.by.AccountID, "labd")
}

// said is the ending in a few words, for a job it cancels.
func (e leaseEnding) said(l *api.Lease) string {
	switch {
	case e.event == api.LeaseReleased:
		return "the Lease it was queued under was released"
	case e.event == api.LeaseExpired:
		return "the Lease it was queued under ran out"
	case e.cause == byAMaintainer && e.why != "":
		return "its Lease was ended by @" + e.by.Handle + ": " + e.why
	case e.cause == byAMaintainer:
		return "its Lease was ended by @" + e.by.Handle
	case e.cause == macWentAway:
		return "its Lease ended: " + e.why
	}
	return "its Lease ended: @" + l.Owner() + " " + map[leaseCause]string{leftTeam: "left the M team", blocked: "was Blocked"}[e.cause]
}

// endLease ends l, v's Lease as the caller saw it, in the way e says, and
// reports whether it did: not when l is no longer v's Lease, renewed or
// ended meanwhile. The Lease record gets its line and the Lab group is
// told. The holder's queued jobs on the Mac are canceled and their sessions
// under the Lease cut; the running job finishes unless the Lease ends over
// its holder, when it is canceled as `lab cancel` does and the Mac returns
// to its Known-good kernel. The jobs others queued before the Lease run
// again. Caller doesn't hold v.mu.
//
// The record's line is written before v.mu is released: anyone who sees
// the Lease gone, or loses the race to end it, finds the line there.
func (s *Server) endLease(v *dev, l *api.Lease, e leaseEnding) bool {
	v.mu.Lock()
	if l == nil || v.d.Lease != l {
		v.mu.Unlock()
		return false
	}
	v.d.Lease, v.leaseCut = nil, ""
	name, id := v.d.Name, v.d.ID
	s.recordLease(name, id, l, e.event, e.by, e.why)
	v.saveLocked()
	v.publishLocked()
	var aside []string
	for _, p := range v.parked {
		aside = append(aside, p.id)
	}
	v.mu.Unlock()

	handle := orStr(l.Owner(), l.Holder)
	switch {
	case e.event == api.LeaseReleased:
		s.labGroup.LeaseReleased(handle, name)
	case e.event == api.LeaseExpired:
		s.labGroup.LeaseExpired(handle, name)
	case e.cause == leftTeam:
		s.labGroup.LeaseEndedLeftTeam(handle, name)
	case e.cause == blocked:
		s.labGroup.LeaseEndedBlocked(handle, name)
	case e.cause == macWentAway:
		s.labGroup.LeaseEndedMacLeft(handle, name)
	default:
		s.labGroup.LeaseEndedByMaintainer(e.by.Handle, handle, name, e.why)
	}
	if e.over() {
		msg := fmt.Sprintf("%s's lease by %s, until %s, %s", name, l.Who(), l.Expires.Format(time.DateTime), orStr(e.note, e.said(l)))
		// By account: labd's log is beyond a forgetting's reach, and the
		// holder's text and a Maintainer's why may name people.
		s.log.Printf("%s: the Lease of %s, until %s, %s", name, accountOr(l.AccountID, "no account"), l.Expires.Format(time.DateTime), e.logged())
		for _, id := range aside {
			s.updateJobByID(id, func(j *api.Job) { j.Events = append(j.Events, api.JobEvent{Time: s.now(), Msg: msg}) })
		}
	}
	if e.asToday {
		s.cutSessions(name, l)
	} else {
		s.cutHolder(v, l, e.said(l), e.over())
	}
	v.unpark()
	return true
}

// cutSessions cuts the sessions the holder of l has open under it.
func (s *Server) cutSessions(mac string, l *api.Lease) {
	if l.AccountID != 0 {
		s.conns.end(leaseConnKey(mac, l.AccountID))
	}
}

// cutHolder cancels the queued jobs on v of the holder of l and cuts the
// sessions they have open under it; running too cancels their job that is
// on the Mac now. why is said on each job.
func (s *Server) cutHolder(v *dev, l *api.Lease, why string, running bool) {
	name := v.snapshot().Name
	s.cutSessions(name, l)
	jobs, err := s.store.jobs(name, 1000, true)
	if err != nil {
		s.log.Printf("%s: the jobs its Lease's end cancels: %v", name, err)
		return
	}
	for _, j := range jobs {
		if !leaseCovers(l, ownerOf(j)) {
			continue
		}
		if v.snapshot().ActiveJob == j.ID && !running {
			continue
		}
		s.cancelJob(j.ID, why)
	}
}

// lapse does what the clock asks of v's Lease. Past its end with its
// holder's job still running, the Lease is extended until that job ends and
// no further: the record says so once, and the holder's queued jobs and
// sessions are cut at the original end. Past its end otherwise, it expires.
func (s *Server) lapse(v *dev) {
	v.mu.Lock()
	l := v.d.Lease
	if l == nil || s.now().Before(l.Expires) {
		v.mu.Unlock()
		return
	}
	if v.liveLeaseLocked() == nil {
		cut := v.leaseCut
		v.mu.Unlock()
		s.endLease(v, l, leaseEnding{event: api.LeaseExpired, why: cut, asToday: cut != ""})
		return
	}
	if l.Extended {
		v.mu.Unlock()
		return
	}
	ext := *l
	ext.Extended = true
	v.d.Lease = &ext
	v.saveLocked()
	v.publishLocked()
	name, id := v.d.Name, v.d.ID
	v.mu.Unlock()
	s.recordLease(name, id, &ext, api.LeaseExtended, Person{}, "until job "+ext.Job+" ends")
	s.cutHolder(v, &ext, "the Lease it was queued under ran out", false)
}

// leaseKeeper has every Lease end on time.
func (s *Server) leaseKeeper() {
	t := s.clock().NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		}
		for _, v := range s.allDevs() {
			s.lapse(v)
		}
	}
}

// endLeasesOf ends, in the way e says, every Lease p holds: nobody else
// could release them, and the jobs they hold back would wait for them to
// run out.
func (s *Server) endLeasesOf(p Person, e leaseEnding) {
	for _, v := range s.allDevs() {
		v.mu.Lock()
		l := v.liveLeaseLocked()
		v.mu.Unlock()
		if l != nil && leaseIsOf(l, p) {
			s.endLease(v, l, e)
		}
	}
}

// leasesOf names the Macs the person holds a Lease on now.
func (s *Server) leasesOf(p Person) []string {
	var names []string
	for _, v := range s.allDevs() {
		v.mu.Lock()
		if l := v.liveLeaseLocked(); l != nil && leaseIsOf(l, p) {
			names = append(names, v.d.Name)
		}
		v.mu.Unlock()
	}
	return names
}

// ---- canceling a job ----

// cancelJob cancels a job that is queued or running, and reports whether
// there was one to cancel. A running job is canceled as `lab cancel` does:
// its Mac returns to its Known-good kernel. why, when given, is said in the
// job's summary.
func (s *Server) cancelJob(id, why string) bool {
	running := func() bool {
		s.mu.Lock()
		cancel := s.cancels[id]
		if cancel != nil && why != "" {
			s.cancelWhy[id] = why
		}
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return cancel != nil
	}
	if running() {
		return true
	}
	canceled := false
	s.updateJobByID(id, func(j *api.Job) {
		if j.State != api.JobQueued {
			return
		}
		canceled = true
		j.State, j.Outcome, j.Summary, j.Canceled = api.JobDone, api.OutcomeCanceled, "canceled before it started", true
		if why != "" {
			j.Summary += ": " + why
		}
	})
	// A Mac's loop takes a job from the queue by setting its cancel and then
	// reading it once more: if it read the job as queued before it was marked
	// done above, its cancel is set by now.
	return running() || canceled
}

// canceledWhy is why job id was canceled, when whoever canceled it said.
func (s *Server) canceledWhy(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelWhy[id]
}

// mayCancel is why p may not cancel j, or nil: an M team member cancels
// only their own jobs, a Maintainer any.
func mayCancel(p Person, j *api.Job) error {
	switch {
	case p.isMaintainer():
		return nil
	case !p.OnMTeam():
		return refuse(403, "canceling a job is for the M team and Maintainers")
	case j.AccountID == 0 || j.AccountID != p.AccountID:
		return refuse(403, "an M team member cancels only their own jobs: job %s is %s", j.ID, orStr(prefixed("@", j.Maintainer), "the lab's own"))
	}
	return nil
}

func prefixed(prefix, s string) string {
	if s == "" {
		return ""
	}
	return prefix + s
}
