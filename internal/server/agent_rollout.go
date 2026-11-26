package server

// The Agent rollout: labd puts the lab-agent it was Deployed with on every
// enrolled Mac by itself, by its exact bytes (sha256), never by its number.
//
// A lab-agent is first on trial: labd gives it to one idle Lab-owned Mac of
// each chip family, and promotes it once two Macs of different families have
// run it well for rolloutTrialFor (checked in from a fresh start of it,
// answered a command, still checking in). A promoted lab-agent goes to every
// Mac whose lab-agent differs, each when it is idle; a Volunteer's Mac also
// inside its Availability window with rolloutWindowLeft of it left. A Mac
// that returned to its previous lab-agent stops that lab-agent's rollout
// for good, and "Agents" is told once. The state is kept in
// agent-rollout.json in the data folder.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

const (
	// rolloutMinAgent is the first lab-agent that stages a new one and
	// returns to itself when that one does not run well. A Volunteer's Mac
	// below it takes the rollout only after its Volunteer has run
	// `sudo lab-agent update` once; a Lab-owned Mac needs agentUpdateMinAgent.
	rolloutMinAgent = "0.11.0"
	// rolloutFamilies is how many chip families the trial Macs cover.
	rolloutFamilies = 2
	// rolloutSilent is how long a trial Mac may go without checking in and
	// still count as running well.
	rolloutSilent = 5 * time.Minute
	// rolloutWindowLeft is what a Volunteer's Mac must have left of its
	// Availability window for an update.
	rolloutWindowLeft = 20 * time.Minute
	// rolloutOverdue is how long a Mac may wait for its update before the
	// Performance scheduler leaves it alone to take it, for as long again at
	// most: an update that keeps failing does not hold its runs back for ever.
	rolloutOverdue = 24 * time.Hour
	// rolloutRestartsMax is how many times a trial Mac's new lab-agent may
	// start again before its rollout stops.
	rolloutRestartsMax = 3
	rolloutEvery   = time.Minute
	// rolloutAtOnce is how many Macs take an update at the same time.
	rolloutAtOnce = 3
)

// rolloutTrialFor is how long the trial Macs run a lab-agent well before
// labd promotes it.
var rolloutTrialFor = time.Hour

const (
	trialOn   = "trial"
	promoted  = "promoted"
	rollStops = "stopped"
)

// agentTrial is the rollout of one lab-agent, by its bytes.
type agentTrial struct {
	SHA256     string    `json:"sha256"`
	Version    string    `json:"version"`
	LabRelease string    `json:"lab_release,omitempty"`
	State      string    `json:"state"`
	Since      time.Time `json:"since"`
	At         time.Time `json:"at,omitzero"` // promoted or stopped
	Why        string    `json:"why,omitempty"`
	// Ran is each Lab-owned Mac seen running it during the trial, by name.
	Ran map[string]*trialMac `json:"ran,omitempty"`
}

// trialMac is a Lab-owned Mac running the lab-agent on trial.
type trialMac struct {
	Family   string    `json:"family"`
	Start    string    `json:"start"`    // the lab-agent's start number
	Since    time.Time `json:"since"`    // its first check-in from that start
	Last     time.Time `json:"last"`     // its last check-in from that start
	Answered bool      `json:"answered"` // a command round trip from that start
	// Boot is the boot it started in; Restarts how many times it started
	// again in one boot, as a reboot starts it afresh without fault.
	Boot     string `json:"boot,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
}

type rolloutState struct {
	Trials map[string]*agentTrial `json:"trials"`
	// Due is when each Mac was first seen needing the lab's lab-agent.
	Due map[string]time.Time `json:"due,omitempty"`
}

type agentRollout struct {
	mu   sync.Mutex
	path string
	st   rolloutState
	// busy is the Macs being updated or probed now, with their chip family.
	busy map[string]string
}

func openAgentRollout(dir string) *agentRollout {
	r := &agentRollout{path: filepath.Join(dir, "agent-rollout.json"), busy: map[string]string{}}
	if b, err := os.ReadFile(r.path); err == nil {
		json.Unmarshal(b, &r.st)
	}
	if r.st.Trials == nil {
		r.st.Trials = map[string]*agentTrial{}
	}
	if r.st.Due == nil {
		r.st.Due = map[string]time.Time{}
	}
	return r
}

func (r *agentRollout) saveLocked() error {
	b, err := json.MarshalIndent(r.st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(r.path, b)
}

// rollout is labd's Agent rollout, opened at its first use.
func (s *Server) rollout() *agentRollout {
	s.rolloutOnce.Do(func() { s.agentRollout = openAgentRollout(s.cfg.DataDir) })
	return s.agentRollout
}

// trialLocked is the rollout of the lab-agent labd hands out now, begun if
// it is new.
func (r *agentRollout) trialLocked(sha, version, release string, now time.Time) *agentTrial {
	t := r.st.Trials[sha]
	if t == nil {
		t = &agentTrial{SHA256: sha, Version: version, LabRelease: release, State: trialOn, Since: now, Ran: map[string]*trialMac{}}
		r.st.Trials[sha] = t
		// Only this Deploy's lab-agent is rolled out; older ones are history.
		for k, o := range r.st.Trials {
			if k != sha && o.State == trialOn {
				delete(r.st.Trials, k)
			}
		}
		r.saveLocked()
	}
	if t.Ran == nil {
		t.Ran = map[string]*trialMac{}
	}
	return t
}

// rolloutWords names a lab-agent in a line: its number, Lab release and the
// start of its sha256.
func rolloutWords(t *agentTrial) string {
	w := "lab-agent " + t.Version
	if t.LabRelease != "" {
		w += " (Lab release " + t.LabRelease + ", " + short(t.SHA256) + ")"
	} else {
		w += " (" + short(t.SHA256) + ")"
	}
	return w
}

// rolloutHeard is a check-in from v, whose facts and start labd now holds:
// a trial Mac's health, a Mac's return to its previous lab-agent, and when a
// Mac began to need the lab's lab-agent. Caller holds v.mu; rolloutHeard
// takes only the rollout's own lock.
func (s *Server) rolloutHeardLocked(v *dev) {
	if !s.cfg.AgentRollout {
		return
	}
	sha := s.agentSHA256()
	if sha == "" {
		return
	}
	d, now := v.d, s.now()
	r := s.rollout()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.trialLocked(sha, s.labAgent(), agent.LabRelease, now)
	changed := false
	if d.Facts.AgentSHA256 == sha {
		if _, ok := r.st.Due[d.Name]; ok {
			delete(r.st.Due, d.Name)
			changed = true
		}
	} else if _, ok := r.st.Due[d.Name]; !ok && d.Facts.AgentVersion != "" {
		r.st.Due[d.Name] = now
		changed = true
	}
	fam := orStr(family(d.Facts), "(unknown)")
	if o := d.Facts.AgentOutcome; o != nil && o.Outcome == api.AgentReturned && o.ToSHA256 == sha && t.State != rollStops {
		s.stopRolloutLocked(t, now, fmt.Sprintf("a Mac of chip family %s returned to its previous lab-agent: %s", fam, o.Why))
		s.log.Printf("agent rollout: %s returned to its previous lab-agent", d.Name)
		changed = true
	}
	if t.State == trialOn && !d.IsVolunteer() && d.Facts.AgentSHA256 == sha && v.factsStart == v.agentStart && v.agentStart != "" {
		m := t.Ran[d.Name]
		if m == nil || m.Start != v.agentStart {
			restarts := 0
			if m != nil && m.Boot == d.BootID {
				restarts = m.Restarts + 1
			}
			m = &trialMac{Family: family(d.Facts), Start: v.agentStart, Since: now, Boot: d.BootID, Restarts: restarts}
			t.Ran[d.Name] = m
			changed = true
		}
		m.Last = now
		if m.Restarts > rolloutRestartsMax {
			s.stopRolloutLocked(t, now, fmt.Sprintf("on a trial Mac of chip family %s it started again %d times in one boot", fam, m.Restarts))
			s.log.Printf("agent rollout: %s keeps restarting its new lab-agent", d.Name)
			changed = true
		} else if s.promoteLocked(t, now) {
			changed = true
		}
	}
	if changed {
		r.saveLocked()
	}
}

// stopRolloutLocked stops t for good, and tells "Agents". Caller holds the
// rollout's lock.
func (s *Server) stopRolloutLocked(t *agentTrial, now time.Time, why string) {
	t.State, t.At, t.Why = rollStops, now, why
	s.lines.post(lineAgents, fmt.Sprintf("Agent rollout: %s stopped: %s. It goes to no further Mac; the next Deploy's lab-agent starts its own trial.", rolloutWords(t), why))
	s.log.Printf("agent rollout: %s stopped: %s", short(t.SHA256), why)
}

// promoteLocked promotes t once two Macs of different families have run it
// well for rolloutTrialFor. Caller holds the rollout's lock.
func (s *Server) promoteLocked(t *agentTrial, now time.Time) bool {
	if t.State != trialOn {
		return false
	}
	fams := map[string]bool{}
	for _, m := range t.Ran {
		if m.Family != "" && m.Answered && now.Sub(m.Since) >= rolloutTrialFor && now.Sub(m.Last) <= rolloutSilent {
			fams[m.Family] = true
		}
	}
	if len(fams) < rolloutFamilies {
		return false
	}
	names := make([]string, 0, len(fams))
	for f := range fams {
		names = append(names, f)
	}
	sort.Strings(names)
	t.State, t.At = promoted, now
	s.lines.post(lineAgents, fmt.Sprintf("Agent rollout: %s ran well for %s on chip families %s: every Mac takes it when idle.", rolloutWords(t), rolloutTrialFor, strings.Join(names, ", ")))
	s.log.Printf("agent rollout: %s promoted", short(t.SHA256))
	return true
}

// rolloutOverdueFor is whether a promoted lab-agent has waited for the Mac
// longer than rolloutOverdue, and not twice that: the Performance scheduler
// then leaves it to take it. A Mac whose lab-agent cannot take the rollout
// is never overdue.
func (s *Server) rolloutOverdueFor(d api.Device) bool {
	if !s.cfg.AgentRollout || !versionAtLeast(d.Facts.AgentVersion, rolloutMin(d)) {
		return false
	}
	r := s.rollout()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.st.Trials[s.agentSHA256()]
	due, ok := r.st.Due[d.Name]
	late := s.now().Sub(due)
	return ok && t != nil && t.State == promoted && late > rolloutOverdue && late <= 2*rolloutOverdue
}

// rolloutMin is the oldest lab-agent of d that the rollout updates.
func rolloutMin(d api.Device) string {
	if d.IsVolunteer() {
		return rolloutMinAgent
	}
	return agentUpdateMinAgent
}

func (s *Server) rolloutScheduler() {
	if !s.cfg.AgentRollout {
		return
	}
	t := s.clock().NewTicker(rolloutEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		}
		s.reconcileRollout()
	}
}

// reconcileRollout gives the lab's lab-agent to each Mac that should take
// it now, and asks each trial Mac that has not answered a command since its
// start for one.
func (s *Server) reconcileRollout() {
	sha := s.agentSHA256()
	if _, signed := s.agentSig(); sha == "" || !signed || s.drained() {
		return
	}
	r := s.rollout()
	r.mu.Lock()
	t := r.trialLocked(sha, s.labAgent(), agent.LabRelease, s.now())
	state := t.State
	tried := map[string]bool{} // families with a trial Mac, or one being updated
	for _, m := range t.Ran {
		if m.Family != "" {
			tried[m.Family] = true
		}
	}
	for _, fam := range r.busy {
		tried[fam] = true
	}
	r.mu.Unlock()
	if state == rollStops {
		return
	}
	devs := s.allDevs()
	slices.SortFunc(devs, func(a, b *dev) int { return strings.Compare(a.snapshot().Name, b.snapshot().Name) })
	for _, v := range devs {
		d := v.snapshot()
		if d.Facts.AgentSHA256 == sha {
			if state == trialOn && !d.IsVolunteer() {
				s.rolloutProbe(v, sha)
			}
			continue
		}
		fam := family(d.Facts)
		if state == trialOn && (d.IsVolunteer() || fam == "" || tried[fam]) {
			continue
		}
		if why := s.rolloutNotNow(v); why != "" {
			continue
		}
		if !r.take(d.Name, fam) {
			continue
		}
		if state == trialOn {
			tried[fam] = true
		}
		go func() {
			defer r.done(d.Name)
			res, _, err := s.updateAgent(s.ctx, v, "the Agent rollout")
			switch {
			case err != nil:
				s.log.Printf("agent rollout: %s did not take %s: %v", d.Name, short(sha), err)
			case res.Changed:
				s.log.Printf("agent rollout: %s takes %s (%s -> %s)", d.Name, short(sha), res.From, res.To)
			}
		}()
	}
}

// take marks name, of chip family fam, as being updated, if fewer than
// rolloutAtOnce are and it is not already.
func (r *agentRollout) take(name, fam string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.busy[name]; ok || len(r.busy) >= rolloutAtOnce {
		return false
	}
	r.busy[name] = fam
	return true
}

func (r *agentRollout) done(name string) {
	r.mu.Lock()
	delete(r.busy, name)
	r.mu.Unlock()
}

// rolloutProbe asks a trial Mac for a command from its new start, once: its
// kernel's config, which every lab-agent answers and which changes nothing.
func (s *Server) rolloutProbe(v *dev, sha string) {
	d := v.snapshot()
	r := s.rollout()
	r.mu.Lock()
	t := r.st.Trials[sha]
	m := (*trialMac)(nil)
	if t != nil {
		m = t.Ran[d.Name]
	}
	need := m != nil && !m.Answered
	r.mu.Unlock()
	if !need || !r.take(d.Name, family(d.Facts)) {
		return
	}
	go func() {
		defer r.done(d.Name)
		v.mu.Lock()
		start := v.agentStart
		v.mu.Unlock()
		var text string
		ctx, cancel := context.WithTimeout(s.ctx, time.Minute)
		defer cancel()
		if err := v.call(ctx, api.CmdConfig, "", nil, time.Minute, &text); err != nil {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if m := t.Ran[d.Name]; m != nil && m.Start == start {
			m.Answered = true
			s.promoteLocked(t, s.now())
			r.saveLocked()
		}
	}()
}

// rolloutNotNow is why the rollout leaves v alone now, or "": not idle as
// lab agent-update has it, claimed for a Performance run, a Hard reset under
// way; a Volunteer's Mac also outside its window, with too little of it
// left, or Paused; a lab-agent too old to take it unattended.
func (s *Server) rolloutNotNow(v *dev) string {
	_, signed := s.agentSig()
	v.mu.Lock()
	defer v.mu.Unlock()
	d := v.d
	if why := v.notUpdatableLocked(signed); why != "" {
		return why
	}
	min := rolloutMin(d)
	switch {
	case d.State == api.StateLeft || v.forgotten:
		return "it is leaving the lab"
	case !versionAtLeast(d.Facts.AgentVersion, min):
		return fmt.Sprintf("it runs lab-agent %s, which takes the rollout from %s", orStr(d.Facts.AgentVersion, "(unknown)"), min)
	case v.perfClaim != "":
		return "a Performance run claims it"
	case v.resetting || v.termsWait:
		return "it is busy"
	}
	return v.windowHoldLocked(rolloutWindowLeft)
}

// rolloutRefusalLocked is why `lab agent-update` may not give v the lab's
// lab-agent now: a Volunteer's Mac takes only a promoted one. Caller holds
// v.mu.
func (s *Server) rolloutRefusalLocked(v *dev) string {
	if !s.cfg.AgentRollout || !v.d.IsVolunteer() {
		return ""
	}
	sha := s.agentSHA256()
	r := s.rollout()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.st.Trials[sha]
	switch {
	case t == nil || t.State == trialOn:
		return fmt.Sprintf("%s is a Volunteer's Mac, and the lab's lab-agent is still on trial on Lab-owned Macs: it reaches a Volunteer's Mac once it has run well there", v.d.Name)
	case t.State == rollStops:
		return fmt.Sprintf("the lab's lab-agent goes to no further Mac: %s", t.Why)
	}
	return ""
}
