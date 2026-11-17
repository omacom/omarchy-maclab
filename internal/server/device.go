package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/detect"
)

// aliveWindow is how stale the last heartbeat may be before a device counts as unresponsive.
var aliveWindow = 20 * time.Second

var errDeviceLost = errors.New("device stopped responding or rebooted")

// dev is labd's live view of one Mac.
type dev struct {
	s            *Server
	reading      *identityRead // the identity check a freeze reads the Mac with now, outside any job
	secretHash   string
	enrolledWith string // hash of the enrollment token that made this Mac, until it first checks in
	// volunteeredBy is d.VolunteeredBy, which never changes: labd counts a
	// volunteer's Macs under s.mu alone, without each Mac's mu.
	volunteeredBy string

	mu sync.Mutex
	d  api.Device
	// owedDone is a job's done notice that waited for the Mac to be back (needs hands or recovering at the job's end). It is memory
	// only: if labd restarts while one is held, the job is already closed and that notice is lost.
	owedDone *Notification
	// termsWait is whether the Mac takes nothing from the lab until its
	// Volunteer accepts the lab's changed terms (terms.go).
	termsWait     bool
	pending       []api.Command
	inflight      map[string]time.Time
	sent          map[string]api.Command
	target        map[string]string // command ID -> the boot it was issued for
	grants        map[string]string // artifact sha256 -> the job a command sent it for (see grantLocked)
	fetchGrant    *fetchGrant       // a Maintainer's explicit grant (see hArtifactGrants)
	results       map[string]api.CommandResult
	gaveUp        map[string]time.Time // commands labd stopped waiting for, and when (see forget)
	taken         map[string]time.Time // results their waiter took, and when (see takeResultLocked)
	changed       chan struct{}
	events        []api.KernelEvent
	dirty         bool // a lab kernel may still be staged on the Mac
	restore       api.DeviceState
	queue         chan string
	parked        []parkedJob   // queued jobs someone else's lease holds back (see park)
	leaseCut      string        // why the Mac's Volunteer or its window cut the Lease short, "" when nothing did
	siteCut       siteCut       // the running job its Volunteer stopped from the site, and why (see cutJobLocked)
	coresLooked   bool          // the Mac's jobs were looked through for its core counts (see configuration)
	gone          chan struct{} // closed when the device is forgotten: ends deviceLoop
	stop          chan struct{} // closed when the Mac's enrollment ends
	goneWhy       string        // why it ended, once it has
	recoveryMu    sync.Mutex
	oobCallMu     sync.Mutex
	oobCallCancel context.CancelFunc
	// resetting: a person's Hard reset is on its way to the controller.
	resetting bool
	// proving: a Proving reset of the Mac was admitted and has not ended.
	proving bool
	// updating: lab agent-update holds the Mac (agent_software.go).
	updating *agentUpdating
	// agentStart is the api.Checkin.AgentStart labd last heard, and
	// factsStart the one its d.Facts came with.
	agentStart, factsStart string
	// heardSinceStart: the Mac has checked in since labd started.
	heardSinceStart bool
	// cleaning: the idle clean-up is on its way to the Mac or running there.
	cleaning bool
	// cleanupCmd is the idle clean-up last handed to the Mac, and
	// cleanupAway that labd's wait for it ended with no result: cleaning
	// then holds until a check-in no longer lists it as running.
	cleanupCmd  string
	cleanupAway bool
	// cleanupWait is how long labd waits for the idle clean-up's result; 0
	// is two minutes. Only a test sets it, under mu.
	cleanupWait time.Duration
	// updateHold is how long lab agent-update still holds the Mac after its
	// call ends; 0 is agentUpdateWait. Only a test sets it, under mu.
	updateHold time.Duration
	// beforeSend, which only a test sets (under mu), runs once a job's wait
	// is over and before labd gives it the Mac.
	beforeSend func()

	// forgotten: lab forget deleted the device's row. Set under mu, which
	// every save holds, so nothing still holding the device writes it back.
	forgotten bool
	// leftBy is what took the Mac out of the lab ("lab-agent uninstall
	// --force"), set with StateLeft; empty after labd restarts.
	leftBy string

	shotMu     sync.Mutex
	screenAt   time.Time
	screenName string

	serialLast time.Time
	serialLine string
	marker     string
	markerTime time.Time

	asleep time.Time // when the agent said the Mac is going to sleep; zero while awake
	awoke  time.Time // when it said it woke up: quietGrace counts from here
	// The agent said boot quietBoot may be off the network until quietUntil
	// (api.Quiet): until then its silence doesn't make it lost or offline.
	quietBoot  string
	quietUntil time.Time

	// heard is the last check-in from the Mac itself, or its report that it
	// is back on the network. LastSeen also moves when labd answers a held
	// check-in, which a Mac that just died can't hear.
	heard time.Time
	// The last reboot labd saw: the boot that ended, the last check-in heard
	// from it, and the first check-in of the boot after it.
	endedBoot            string
	endedHeard, nextSeen time.Time

	auditMu   sync.Mutex
	auditHave int64 // how much of the Mac's Audit log labd's copy holds; -1 until read
}

func newDev(s *Server, d api.Device, secretHash string) *dev {
	v := &dev{s: s, d: d, secretHash: secretHash, volunteeredBy: d.VolunteeredBy, inflight: map[string]time.Time{}, sent: map[string]api.Command{}, target: map[string]string{},
		results: map[string]api.CommandResult{}, gaveUp: map[string]time.Time{}, taken: map[string]time.Time{}, changed: make(chan struct{}), queue: make(chan string, 256), gone: make(chan struct{}), stop: make(chan struct{}), auditHave: -1}
	return v
}

// bump wakes everything waiting on this device. Caller holds mu.
func (v *dev) bump() {
	close(v.changed)
	v.changed = make(chan struct{})
}

func (v *dev) snapshot() api.Device {
	v.mu.Lock()
	defer v.mu.Unlock()
	d := v.d
	return d
}

var errForgotten = errors.New("the device was forgotten (lab forget)")

// storeLocked saves the device, unless lab forget deleted its row: a
// check-in, the monitor or a promotion still holding it must not bring the
// row back, where it would clash with a Mac enrolled again under its name.
func (v *dev) storeLocked() error {
	if v.forgotten {
		return errForgotten
	}
	return v.s.store.saveDevice(&v.d, v.secretHash)
}

func (v *dev) saveLocked() {
	if err := v.storeLocked(); err != nil && !errors.Is(err, errForgotten) {
		v.s.log.Printf("save device %s: %v", v.d.Name, err)
	}
}

// checkedIn is whether the Mac has ever reached labd since it enrolled.
func (v *dev) checkedIn() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.d.BootID != ""
}

// leaving says that lab-agent uninstall took the Mac out of the lab, and
// how, or "" while it is still in the lab.
func (v *dev) leaving() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.d.State != api.StateLeft {
		return ""
	}
	return fmt.Sprintf("%s left the lab (%s)", v.d.Name, orStr(v.leftBy, "lab-agent uninstall"))
}

// heardAt is when labd last heard the Mac itself (see heard).
func (v *dev) heardAt() time.Time {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.heard
}

func (v *dev) aliveLocked() bool {
	return !v.d.LastSeen.IsZero() && v.s.since(v.d.LastSeen) < v.s.aliveWindow()
}

func (v *dev) setState(st api.DeviceState, reason string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.setStateLocked(st, reason)
}

func (v *dev) setStateLocked(st api.DeviceState, reason string) {
	if v.d.State == st && v.d.StateReason == reason {
		return
	}
	// A Mac that left the lab stays left until lab forget: a job ending or a
	// recovery finishing must not put it back in the pool.
	if v.d.State == api.StateLeft {
		return
	}
	was := v.d.State
	v.d.State, v.d.StateReason = st, reason
	if st != api.StateNeedsHands {
		v.d.Action = nil
	}
	if !v.d.IsVolunteer() && v.s.lines != nil {
		key := v.d.ID
		if key == "" {
			key = v.d.Name
		}
		switch {
		case st == api.StateNeedsHands && was != st:
			v.s.agents.NeedsHands(key, v.d.Name, reason)
		case was == api.StateNeedsHands && st != was:
			v.s.agents.NoLongerNeedsHands(key, v.d.Name, st)
		}
	}
	v.saveLocked()
	v.publishLocked()
	v.bump()
}

// idleState is where a device goes when nothing is happening to it.
func (v *dev) idleStateLocked() api.DeviceState {
	if v.d.KnownGood == "" {
		return api.StateNew
	}
	return api.StateReady
}

// ---- arriving ----

// The stages of a Volunteer's Mac from Joined to ready, as My Macs shows
// them. A Mac arrives closed: with no Availability window it takes nothing.
// Its first baseline, which the lab queues itself, runs in the first open
// window; a failed one is shown, and a Maintainer looks.
const (
	ArrivalNoWindow    = "no window yet"
	ArrivalFirstCheck  = "first check pending (restarts twice)"
	ArrivalCheckFailed = "first check failed: a Maintainer looks"
	ArrivalReady       = "ready"
)

// arrivalLocked is the stage a Volunteer's Mac has reached; "" for a
// Lab-owned Mac. Caller holds v.mu.
func (v *dev) arrivalLocked() string {
	if !v.d.IsVolunteer() {
		return ""
	}
	fb := v.d.FirstBaseline
	switch {
	case v.d.KnownGood != "" || fb != nil && fb.Outcome == api.FirstBaselinePassed:
		return ArrivalReady
	case fb != nil && fb.Outcome == api.FirstBaselineFailed:
		return ArrivalCheckFailed
	case (v.d.Window == nil || !v.d.Window.Scheduled()) && v.openUntilLocked().IsZero():
		return ArrivalNoWindow
	}
	return ArrivalFirstCheck
}

// joined is what the lab does when a Mac has Joined: the line of the Mac's
// record and of the Lab group and, for a Volunteer's Mac lent by a person,
// the first baseline, which the lab queues itself. It waits, like any job,
// for the Mac's first open window.
func (s *Server) joined(v *dev) {
	d := v.snapshot()
	configuration := ""
	if c := s.configuration(v); c != (api.Configuration{}) {
		configuration = c.String()
	}
	as := "a Lab-owned Mac"
	if d.IsVolunteer() {
		as = "a Volunteer's Mac, at Qualification with publishing off"
	}
	s.macSaid(d.Name, d.ID, macJoined, "Joined as "+as+" ("+orUnknown(configuration)+")")
	s.labGroup.MacJoined(d.Name, configuration, d.Kind)
	if !d.IsVolunteer() || d.VolunteerID == 0 {
		return
	}
	j, _, err := s.submit(Person{}, api.JobSpec{Device: d.Name, Baseline: true}, "")
	if err != nil {
		s.log.Printf("%s: the first baseline was not queued: %v", d.Name, err)
		return
	}
	v.mu.Lock()
	if v.d.FirstBaseline != nil && v.d.FirstBaseline.Job == "" {
		v.d.FirstBaseline = &api.FirstBaseline{Job: j.ID}
		v.saveLocked()
		v.publishLocked()
	}
	v.mu.Unlock()
	s.log.Printf("%s: the lab queued its first baseline, job %s", d.Name, j.ID)
}

// firstBaselineMoved follows the first baseline to the job that tries it
// again, after a window closed on it.
func (v *dev) firstBaselineMoved(from, to string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if fb := v.d.FirstBaseline; fb != nil && fb.Job == from && fb.Outcome == "" {
		v.d.FirstBaseline = &api.FirstBaseline{Job: to}
		v.saveLocked()
	}
}

// firstBaselineEnded is a baseline job ending on v. Until the Mac has
// passed one, each that passes or fails is its first baseline: an event of
// the Mac's record and a line for the Lab group. One cut short or canceled
// is neither.
func (s *Server) firstBaselineEnded(v *dev, job, outcome string) {
	if outcome == api.OutcomeIncomplete || outcome == api.OutcomeCanceled {
		return
	}
	v.mu.Lock()
	fb := v.d.FirstBaseline
	if fb == nil || fb.Outcome == api.FirstBaselinePassed {
		v.mu.Unlock()
		return
	}
	passed := outcome == api.OutcomePass
	next := api.FirstBaseline{Job: job, Outcome: api.FirstBaselineFailed}
	line := "failed its first baseline (job " + job + ", " + outcome + "): a Maintainer looks"
	if passed {
		next.Outcome, line = api.FirstBaselinePassed, "passed its first baseline (job "+job+")"
	}
	v.d.FirstBaseline = &next
	v.said(macFirstBaseline, line)
	name := v.d.Name
	v.saveLocked()
	v.publishLocked()
	v.mu.Unlock()
	s.labGroup.FirstBaseline(name, passed)
}

// heartbeat records a check-in. Commands are handed out separately by deliver.
func (v *dev) heartbeat(ci api.Checkin) {
	defer v.s.decideTerms(v) // after the unlock below
	// When the check-in came, not when it got the lock: one from the old
	// boot queued behind a slow save must not count as heard after a
	// reboot's grace (recover.go), nor a later one be stamped earlier.
	now := v.s.now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.heard.After(now) {
		now = v.heard
	}
	rebooted := v.d.BootID != "" && ci.BootID != v.d.BootID
	if v.d.BootID == "" {
		if err := v.s.store.spendTokens(v.d.Name); err != nil {
			v.s.log.Printf("spend enrollment tokens for %s: %v", v.d.Name, err)
		}
	}
	persist := rebooted || v.d.BootID == "" || ci.Facts != nil || v.d.Kernel != ci.Kernel || v.d.SnapshotEligible != ci.SnapshotEligible
	if rebooted {
		v.endedBoot, v.endedHeard, v.nextSeen = v.d.BootID, v.heard, now
	}
	v.d.LastSeen, v.heard = now, now
	// Checking in means awake, except for a poll that raced the sleep announcement.
	if !v.asleep.IsZero() && now.Sub(v.asleep) > 5*time.Second {
		v.asleep = time.Time{}
	}
	v.d.BootID, v.d.Kernel, v.d.Cmdline, v.d.Health = ci.BootID, ci.Kernel, ci.Cmdline, ci.Health
	v.d.SnapshotEligible = ci.SnapshotEligible
	if v.syncWindowLocked(ci.Window) {
		persist = true
	}
	if v.syncAccessLocked(ci) {
		persist = true
	}
	if v.capLeaseLocked() {
		persist = true
	}
	if v.d.IsVolunteer() && ci.NtfyOwnURL != "" && v.d.NtfyOwnURL != ci.NtfyOwnURL { // a check-in without it (file unreadable) keeps the stored one
		v.d.NtfyOwnURL, v.d.NoticeTopicRefused = ci.NtfyOwnURL, ""
		persist = true
		go v.s.checkReportedTopic(v.d.Name, ci.NtfyOwnURL)
	}
	if v.d.IsVolunteer() && ci.JobNotices != nil && v.d.JobNotices != *ci.JobNotices { // a check-in without it keeps the stored choice
		v.d.JobNotices = *ci.JobNotices
		persist = true
	}
	v.agentStart, v.heardSinceStart = ci.AgentStart, true
	if ci.Facts != nil {
		v.d.Facts, v.factsStart = *ci.Facts, ci.AgentStart
	}
	v.heardAgentLocked()
	if v.noticeUpdateLocked() {
		persist = true
	}
	if rebooted {
		// Whatever was executing on the previous boot is gone.
		for id := range v.inflight {
			v.results[id] = api.CommandResult{ID: id, Error: errDeviceLost.Error()}
		}
		v.inflight = map[string]time.Time{}
		v.sent = map[string]api.Command{}
		v.s.log.Printf("%s: new boot %s kernel %s", v.d.Name, short(ci.BootID), ci.Kernel)
	}
	for _, r := range ci.Results {
		if _, ok := v.inflight[r.ID]; ok {
			delete(v.inflight, r.ID)
			v.results[r.ID] = r
		}
	}
	for _, e := range ci.Events {
		v.addEventLocked(e)
	}
	switch v.d.State {
	case api.StateOffline, api.StateNeedsHands:
		was := v.d.State
		back := v.restore
		if back == "" {
			back = v.idleStateLocked()
		}
		v.setStateLocked(back, "")
		v.restore = ""
		v.sendOwedDoneLocked()
		if was == api.StateNeedsHands {
			v.s.notify(Notification{Device: v.d.Name, Title: v.d.Name + " is back", Body: "Checked in on " + ci.Kernel + ". Nothing else to do.", Priority: "default"})
		}
		persist = true
	}
	// A command handed to a connection that died (a Mac reset mid long-poll)
	// never arrives. Redeliver it; the agent ignores IDs it has already seen.
	running := map[string]bool{}
	for _, id := range ci.Running {
		running[id] = true
	}
	if v.cleanupAway && !running[v.cleanupCmd] {
		// The agent says the clean-up labd stopped waiting for has ended
		// (or its run or boot did).
		v.cleaning, v.cleanupAway, v.cleanupCmd = false, false, ""
	}
	for id, at := range v.inflight {
		if !running[id] && now.Sub(at) > 15*time.Second {
			if c, ok := v.sent[id]; ok {
				delete(v.inflight, id)
				v.pending = append(v.pending, c)
			}
		}
	}
	if persist {
		v.saveLocked()
		// The write can take seconds on a busy disk, with mu held. The Mac is
		// heard as of its end: a look at its silence taken next must not count
		// labd's own wait.
		v.d.LastSeen = v.s.now()
	}
	v.bump()
	v.publishLocked()
}

// deliver hands pending commands to a check-in from the current boot.
func (v *dev) deliver(bootID string) api.CheckinResponse {
	v.mu.Lock()
	defer v.mu.Unlock()
	resp := api.CheckinResponse{IntervalMS: int(v.s.cfg.Interval / time.Millisecond), Kind: v.d.Kind, Held: v.heldLocked()}
	if v.d.IsVolunteer() {
		resp.Window = v.d.WindowWanted
	}
	if bootID != v.d.BootID {
		return resp
	}
	now := v.s.now()
	v.d.LastSeen = now
	for _, c := range v.pending {
		// A command is for the boot it was issued against. A reboot aimed at a
		// stuck boot must never reach the fresh boot that replaced it.
		if t := v.target[c.ID]; t != "" && t != bootID {
			v.results[c.ID] = api.CommandResult{ID: c.ID, Error: errDeviceLost.Error()}
			delete(v.target, c.ID)
			continue
		}
		// The idle clean-up, decided or queued before the Mac came to wait
		// for its Volunteer, is not delivered while it waits: it stays dirty.
		if v.termsWait && c.Kind == api.CmdCleanup && c.JobID == "" && v.d.ActiveJob == "" {
			v.results[c.ID] = api.CommandResult{ID: c.ID, Error: termsWaitingLine(v.d.Name)}
			delete(v.target, c.ID)
			continue
		}
		// Nor while lab agent-update holds it: the agent's restart would cut it.
		if v.updating != nil && c.Kind == api.CmdCleanup && c.JobID == "" && v.d.ActiveJob == "" {
			v.results[c.ID] = api.CommandResult{ID: c.ID, Error: v.d.Name + ": " + agentBeingUpdated + ", so the idle clean-up waits"}
			delete(v.target, c.ID)
			continue
		}
		if c.Kind == api.CmdCleanup && c.JobID == "" {
			v.cleanupCmd = c.ID
		}
		v.inflight[c.ID] = now
		v.sent[c.ID] = c
		resp.Commands = append(resp.Commands, c)
	}
	v.pending = nil
	if len(resp.Commands) == 0 {
		v.bump()
	}
	return resp
}

// report takes in what lab-agent reports, and returns the results it reports
// for commands labd gave up waiting for, and those it is waiting for or took.
func (v *dev) report(rep api.Report) (forgotten, wanted []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if q := rep.Quiet; q != nil && q.BootID == v.d.BootID {
		switch now := v.s.now(); {
		case q.Seconds > 0:
			d := min(time.Duration(q.Seconds)*time.Second, maxQuiet)
			v.quietBoot, v.quietUntil = q.BootID, now.Add(d)
			v.s.log.Printf("%s: may be off the network for up to %s on boot %s", v.d.Name, d, short(q.BootID))
		case v.quietLocked(now):
			// Back on the network: from now on a silence is quietGrace's
			// again, so a hang in the next step shows at once. The report
			// is the Mac itself, heard now, maybe before its next check-in.
			v.quietBoot, v.quietUntil = "", time.Time{}
			v.d.LastSeen, v.heard = now, now
			v.s.log.Printf("%s: back on the network on boot %s; no longer waiting out its spell off it", v.d.Name, short(q.BootID))
		}
	}
	for _, r := range rep.Results {
		if _, ok := v.inflight[r.ID]; ok {
			delete(v.inflight, r.ID)
			delete(v.sent, r.ID)
			delete(v.target, r.ID)
			v.results[r.ID] = r
			wanted = append(wanted, r.ID)
		} else if _, ok := v.results[r.ID]; ok {
			// Had already: the reply to that report was lost and the agent
			// is sending it again.
			wanted = append(wanted, r.ID)
		} else if _, ok := v.taken[r.ID]; ok {
			// The same, after its waiter took the result.
			wanted = append(wanted, r.ID)
		} else if _, ok := v.gaveUp[r.ID]; ok {
			forgotten = append(forgotten, r.ID)
		}
	}
	for _, e := range rep.Events {
		v.addEventLocked(e)
	}
	v.bump()
	return forgotten, wanted
}

func (v *dev) addEventLocked(e api.KernelEvent) {
	if e.Time.IsZero() {
		e.Time = v.s.now()
	}
	switch e.Kind {
	case detect.Sleep:
		v.asleep = v.s.now()
	case detect.Wake:
		v.asleep, v.awoke = time.Time{}, v.s.now()
	}
	v.events = append(v.events, e)
	v.s.hub.publish("kevent", v.d.Name, struct {
		api.KernelEvent
		Device string `json:"device"`
	}{e, v.d.Name})
	if len(v.events) > 1000 {
		v.events = v.events[len(v.events)-1000:]
	}
	v.s.log.Printf("%s: kernel event %s (%s): %s", v.d.Name, e.Kind, e.Source, e.Line)
}

// silentFor is how long the Mac was out of touch when boot ended: from the
// last check-in labd heard on it to the first one on the boot after it.
func (v *dev) silentFor(boot string) (time.Duration, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if boot == "" || v.endedBoot != boot || v.endedHeard.IsZero() {
		return 0, false
	}
	return v.nextSeen.Sub(v.endedHeard), true
}

func (v *dev) eventsSince(t time.Time) []api.KernelEvent {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []api.KernelEvent
	for _, e := range v.events {
		if !e.Time.Before(t) && !(v.networkOnlyLocked() && e.Source == "serial") {
			out = append(out, e)
		}
	}
	return out
}

// supersededLocked is whether e, a kernel event, cannot be about the boot a
// job is waiting for, a Lab boot that never reached labd. oldBoot is the boot
// the job asked to reboot. Two cases:
//   - e is stamped with the boot before oldBoot, while the Mac is on oldBoot:
//     that boot's event, reported late. A boot labd has not heard from is not
//     one: it may be the test kernel, reporting before its first check-in.
//   - e is from the boot oldBoot survived: the Mac was heard on oldBoot after
//     e (still on it, or labd saw it end after e). A Mac that went on checking
//     in did not die of that line, and the test kernel, which has not checked
//     in, cannot have printed it: it was the boot before's, or a previous
//     job's console. e.BootID "" (older agents, tests) counts as oldBoot's.
//
// A test kernel that panics before it checks in is in no case: the Mac stops
// checking in, or comes back on a new boot. It uses heard, not LastSeen:
// LastSeen also moves when labd answers a held check-in, which a Mac that just
// died can't hear.
func (v *dev) supersededLocked(e api.KernelEvent, oldBoot string) bool {
	if e.BootID != "" && e.BootID != oldBoot {
		return v.d.BootID == oldBoot && e.BootID == v.endedBoot
	}
	return (v.d.BootID == oldBoot && v.heard.After(e.Time)) || (v.endedBoot == oldBoot && v.endedHeard.After(e.Time))
}

// eventsSinceBoot is eventsSince without the events that can't be the test
// kernel's after the Mac was asked to leave oldBoot: those supersededLocked
// names, and those lab-agent read on oldBoot itself (a kmsg line stamped
// oldBoot, say as it shut down for the reboot), whenever they came. A serial
// line is stamped by labd, with the boot it last heard, so it has only
// supersededLocked's rule. (waitBack, which also waits for a boot to die,
// uses supersededLocked alone.)
func (v *dev) eventsSinceBoot(t time.Time, oldBoot string) []api.KernelEvent {
	var out []api.KernelEvent
	for _, e := range v.eventsSince(t) {
		v.mu.Lock()
		sup := v.supersededLocked(e, oldBoot)
		v.mu.Unlock()
		if !sup && !(e.BootID == oldBoot && e.Source != "serial") {
			out = append(out, e)
		}
	}
	return out
}

// eventsOnBoot is eventsSince without the events of any other boot than boot.
func (v *dev) eventsOnBoot(t time.Time, boot string) []api.KernelEvent {
	var out []api.KernelEvent
	for _, e := range v.eventsSince(t) {
		if e.BootID == "" || e.BootID == boot {
			out = append(out, e)
		}
	}
	return out
}

// onSerial ingests one serial console line.
func (v *dev) onSerial(line string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.networkOnlyLocked() {
		return
	}
	now := v.s.now()
	v.serialLast, v.serialLine = now, line
	v.s.hub.publish("serial", v.d.Name, map[string]any{"device": v.d.Name, "time": now, "line": line})
	if m := detect.Marker(line); m != "" {
		v.marker, v.markerTime = m, now
	}
	if k := detect.Classify(line); k != "" {
		// Stamped with the boot the Mac was last heard on, as kmsg events are.
		// Before the next check-in that is still the boot before: see supersededLocked.
		v.addEventLocked(api.KernelEvent{Time: now, Kind: k, Source: "serial", BootID: v.d.BootID, Line: line})
	}
	v.bump()
}

var cmdSeq struct {
	sync.Mutex
	n int
}

func nextCmdID() string {
	cmdSeq.Lock()
	defer cmdSeq.Unlock()
	cmdSeq.n++
	return fmt.Sprintf("c%d-%d", time.Now().Unix(), cmdSeq.n)
}

// send queues a command for the Mac on behalf of the job's Maintainer.
func (v *dev) send(kind, job string, args any) string { return v.sendFor(caller{}, kind, job, args) }

// sendFor queues a command for the Mac on behalf of c: a Maintainer, the
// Mac's own Volunteer, or when neither whoever submitted the job. The Mac
// records who in its Audit log.
func (v *dev) sendFor(c caller, kind, job string, args any) string {
	id, _ := v.sendForBoot(c, "", kind, job, args)
	return id
}

// sendForBoot is sendFor for one boot of the Mac ("" is the one it is on
// now): no other boot ever gets the command. It returns the command's ID
// and that boot.
func (v *dev) sendForBoot(c caller, boot, kind, job string, args any) (string, string) {
	id, boot, _ := v.sendIf(c, boot, kind, job, args, nil)
	return id, boot
}

// sendIf is sendForBoot for a command that goes only while may, when
// given, finds no reason against it. may is asked in the hold of v.mu that
// queues the command, so nothing changes between the two; on its refusal
// nothing is queued.
func (v *dev) sendIf(c caller, boot, kind, job string, args any, may func() error) (string, string, error) {
	maintainer := orStr(c.maintainer, c.team) // the handle of whoever asked, in the field the agent has
	if maintainer == "" && c.login == "" && job != "" {
		if j, err := v.s.store.job(job); err == nil {
			maintainer = j.Maintainer
		}
	}
	b, _ := json.Marshal(args)
	cmd := api.Command{ID: nextCmdID(), Kind: kind, JobID: job, Maintainer: maintainer, VolunteerLogin: c.login, Args: b}
	v.mu.Lock()
	defer v.mu.Unlock()
	if may != nil {
		if err := may(); err != nil {
			return "", "", err
		}
	}
	v.grantLocked(cmd)
	v.pending = append(v.pending, cmd)
	if boot == "" {
		boot = v.d.BootID
	}
	v.target[cmd.ID] = boot
	v.bump()
	return cmd.ID, boot, nil
}

// waitFor blocks until cond (called with mu held) returns true, the timeout
// passes, ctx ends or the Mac's enrollment ends (errMacGone). Conditions are
// also re-checked every second.
func (v *dev) waitFor(ctx context.Context, timeout time.Duration, cond func() bool) error {
	var deadline <-chan time.Time
	if timeout > 0 {
		deadline = v.s.clock().After(timeout)
	}
	tick := v.s.clock().NewTicker(time.Second)
	defer tick.Stop()
	for {
		v.mu.Lock()
		ok := cond()
		ch := v.changed
		v.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-v.stop:
			return errMacGone
		case <-deadline:
			return context.DeadlineExceeded
		case <-ch:
		case <-tick.C():
		}
	}
}

// errAgentRefused matches an error the agent itself returned, as opposed to
// the command being lost or timing out, when it may or may not have run.
var errAgentRefused = errors.New("the agent returned an error")

// errWindowClosed matches lab-agent refusing or stopping a command because
// the Mac's Availability window is closed.
var errWindowClosed = errors.New("the Mac's Availability window is closed")

type agentError struct {
	kind, msg    string
	windowClosed bool
	accessLevel  bool
}

func (e agentError) Error() string { return e.kind + ": " + e.msg }
func (e agentError) Is(target error) bool {
	return target == errAgentRefused || (e.windowClosed && target == errWindowClosed) || (e.accessLevel && target == errAccessLevel)
}

// call sends a command and waits for its result. It gives up early with
// errDeviceLost if the Mac reboots or stops heartbeating.
func (v *dev) call(ctx context.Context, kind, job string, args any, timeout time.Duration, out any) error {
	return v.callNote(ctx, kind, job, args, timeout, out, nil)
}

// quietGrace is how long a Mac may go silent on the same boot, without having
// said it is going to sleep, before a command on it counts as lost. Wi-Fi
// reconnecting after a resume takes tens of seconds.
var quietGrace = 60 * time.Second

// aliveWindow and quietGrace are Config's, or the package defaults New
// put there.
func (s *Server) aliveWindow() time.Duration { return s.cfg.AliveWindow }

func (s *Server) quietGrace() time.Duration { return s.cfg.QuietGrace }

// maxSleep is how long a command waits for a Mac that went to sleep.
var maxSleep = time.Hour

// maxQuiet is the longest spell off the network an agent can announce
// (api.Quiet) that labd honours.
var maxQuiet = 15 * time.Minute

// quietLocked is whether the Mac is within a spell off the network it
// announced for the boot it is on. Caller holds mu.
func (v *dev) quietLocked(now time.Time) bool {
	return v.quietBoot != "" && v.quietBoot == v.d.BootID && now.Before(v.quietUntil)
}

// lostLocked is whether a Mac silent on the same boot, and not asleep, has
// been silent too long: quietGrace from its last check-in and from its last
// wake-up, and past any spell off the network it announced for this boot.
// Caller holds mu.
func (v *dev) lostLocked(now time.Time) bool {
	return now.Sub(v.d.LastSeen) > v.s.quietGrace() && now.Sub(v.awoke) > v.s.quietGrace() && !v.quietLocked(now)
}

// callNote is call for commands that may outlive a quiet spell. A Mac that
// goes quiet on the same boot is asleep or off the network, and the command
// is still running there: it waits, with the command's clock stopped, for as
// long as the Mac said it is asleep (up to maxSleep), as long as it said it
// may be off the network (up to maxQuiet), and quietGrace otherwise. note, if
// set, hears about sleeping and waking.
func (v *dev) callNote(ctx context.Context, kind, job string, args any, timeout time.Duration, out any, note func(string)) error {
	return v.callNoteOn(ctx, "", kind, job, args, timeout, out, note)
}

// callNoteOn is callNote for a command meant for one boot of the Mac ("" is
// the one it is on now). If the Mac has left that boot, even before the
// command goes out, the command is lost: a test labd sends for a Lab boot
// that has already gone down must not run on the boot that replaced it.
func (v *dev) callNoteOn(ctx context.Context, boot, kind, job string, args any, timeout time.Duration, out any, note func(string)) error {
	return v.callWhile(ctx, boot, kind, job, args, timeout, out, note, nil)
}

// callIf is call for a command that goes only while may finds no reason
// against it (see sendIf): its refusal is returned, and nothing was sent.
func (v *dev) callIf(ctx context.Context, kind, job string, args any, timeout time.Duration, out any, may func() error) error {
	return v.callWhile(ctx, "", kind, job, args, timeout, out, nil, may)
}

func (v *dev) callWhile(ctx context.Context, boot, kind, job string, args any, timeout time.Duration, out any, note func(string), may func() error) error {
	v.mu.Lock()
	name := v.d.Name
	v.mu.Unlock()
	id, boot, err := v.sendIf(callerOfCtx(ctx), boot, kind, job, args, may)
	if err != nil {
		return err
	}
	deadline := v.s.now().Add(timeout)
	var (
		res            api.CommandResult
		lost, timedOut bool
		quietSince     time.Time // last check-in before the Mac went quiet
		sleptNoted     bool
		askedHuman     bool
		notes          []string
	)
	tick := v.s.clock().NewTicker(time.Second)
	defer tick.Stop()
	for {
		v.mu.Lock()
		done := func() bool {
			if r, ok := v.takeResultLocked(id); ok {
				res = r
				return true
			}
			if v.goneWhy != "" || v.d.BootID != boot {
				lost = true
				return true
			}
			now := v.s.now()
			if v.aliveLocked() {
				if !quietSince.IsZero() {
					away := v.d.LastSeen.Sub(quietSince)
					deadline = deadline.Add(away)
					what := "unreachable"
					if sleptNoted {
						what = "asleep"
					}
					notes = append(notes, fmt.Sprintf("%s is back on the same boot after %s %s; still waiting for %s", name, away.Round(time.Second), what, kind))
					quietSince, sleptNoted, askedHuman = time.Time{}, false, false
				}
				if now.After(deadline) {
					timedOut = true
					return true
				}
				return false
			}
			if quietSince.IsZero() {
				quietSince = v.d.LastSeen
			}
			if !v.asleep.IsZero() {
				if !sleptNoted {
					sleptNoted = true
					notes = append(notes, fmt.Sprintf("%s went to sleep; waiting for it to wake. The lab can't wake it: a key press, the power button or opening the lid does", name))
				}
				if !askedHuman && now.Sub(v.asleep) > 2*time.Minute {
					askedHuman = true
					notes = append(notes, "!asleep")
				}
				if now.Sub(v.asleep) > maxSleep {
					lost = true
					return true
				}
				return false
			}
			if v.lostLocked(now) {
				lost = true
				return true
			}
			return false
		}()
		if lost || timedOut {
			// in the same hold of mu: a result reported from now on is
			// answered forgotten, never taken and then dropped
			v.forgetLocked(id)
		}
		ch := v.changed
		v.mu.Unlock()
		for _, n := range notes {
			if n == "!asleep" {
				v.s.notify(Notification{Device: name, Title: name + " is asleep during a test",
					Body: "Press a key on " + name + " to wake it; the lab can't. The job continues when it wakes.", Priority: "high"})
				continue
			}
			v.s.log.Printf("%s: %s", name, n)
			if note != nil {
				note(n)
			}
		}
		notes = nil
		if done {
			break
		}
		select {
		case <-ctx.Done():
			v.forget(id)
			return ctx.Err()
		case <-ch:
		case <-tick.C():
		}
	}
	if timedOut {
		return fmt.Errorf("%s: no result after %s", kind, timeout)
	}
	if lost {
		return errDeviceLost
	}
	if !res.OK {
		if res.Error == errDeviceLost.Error() {
			return errDeviceLost
		}
		return agentError{kind, res.Error, res.WindowClosed, res.AccessLevel}
	}
	if out != nil && len(res.Output) > 0 {
		return json.Unmarshal(res.Output, out)
	}
	return nil
}

// takeResultLocked takes command id's result, if it is in, and remembers for
// an hour that it was taken: if the agent's report of it is sent again (the
// reply to it was lost), labd still wants it, and the agent goes on with what
// follows it, such as a Lab boot's reboot.
func (v *dev) takeResultLocked(id string) (api.CommandResult, bool) {
	r, ok := v.results[id]
	if !ok {
		return r, false
	}
	delete(v.results, id)
	now := v.s.now()
	for t, at := range v.taken {
		if now.Sub(at) > time.Hour {
			delete(v.taken, t)
		}
	}
	v.taken[id] = now
	return r, true
}

// forget stops waiting for command id. A result for it that comes later is
// answered as forgotten, so lab-agent doesn't act on it (a Lab boot's reboot
// for an arm labd timed out on); labd remembers that for an hour.
func (v *dev) forget(id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.forgetLocked(id)
}

func (v *dev) forgetLocked(id string) {
	now := v.s.now()
	for g, at := range v.gaveUp {
		if now.Sub(at) > time.Hour {
			delete(v.gaveUp, g)
		}
	}
	v.gaveUp[id] = now
	delete(v.taken, id)
	delete(v.inflight, id)
	delete(v.sent, id)
	delete(v.target, id)
	delete(v.results, id)
	for i, c := range v.pending {
		if c.ID == id {
			v.pending = append(v.pending[:i], v.pending[i+1:]...)
			break
		}
	}
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
