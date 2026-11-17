package server

// The lab's own software on a Mac.
//
// lab-agent comes from the lab server only: labd hands out the one built
// with it, with its signature beside it, to the Join command's token and to
// an enrolled Mac's own device secret. The Mac checks it: the Join command
// against the release key written into its script (/join), a running
// lab-agent against the key it pins (`sudo lab-agent update`), refusing an
// older one.
//
// Nothing pushes an agent to a Volunteer's Mac. Its Volunteer is told once,
// by a Notice, and My Macs gives the one line to run at the Mac; until then
// the "needs lab-agent X or later" refusals apply. A Lab-owned Mac's agent
// is updated by a Maintainer, through its running agent, while it is idle
// (`lab agent-update`). There is no fleet-wide minimum agent.

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/omacom/omarchy-maclab/installer"
	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// agentUpdateMinAgent is the first lab-agent that updates itself.
const agentUpdateMinAgent = "0.10.0"

// agentUpdateWait is how long labd waits for a Mac to fetch, check and
// install the lab's lab-agent.
const agentUpdateWait = 45 * time.Second

const (
	// volunteerUpdateLine is what a Volunteer runs at their Mac.
	volunteerUpdateLine = "sudo lab-agent update"
)

// labAgent is the number of the lab-agent this labd hands out: the one it
// was built with.
func (s *Server) labAgent() string {
	if s.cfg.LabAgent != "" {
		return s.cfg.LabAgent
	}
	return agent.Version
}

// agentBehind is the lab's lab-agent number when d's is older, else "".
func (s *Server) agentBehind(d api.Device) string {
	if lab := s.labAgent(); d.Facts.AgentVersion != "" && !versionAtLeast(d.Facts.AgentVersion, lab) {
		return lab
	}
	return ""
}

// agentUpdateLine is the one line My Macs gives the Volunteer of a Mac whose
// lab-agent is older than the lab's; "" for any other Mac.
func agentUpdateLine(d DeviceView) string {
	if d.LabAgent != "" && d.IsVolunteer() {
		return volunteerUpdateLine
	}
	return ""
}

// noticeUpdateLocked tells the Volunteer of a Mac whose lab-agent is older
// than the lab's, once for each lab-agent of the lab. It reports whether the
// Mac's record changed. Caller holds v.mu.
func (v *dev) noticeUpdateLocked() bool {
	lab := v.s.agentBehind(v.d)
	if lab == "" || !v.d.IsVolunteer() || v.d.UpdateNoticed == lab {
		return false
	}
	v.d.UpdateNoticed = lab
	v.s.notify(v.s.notice(v.d, noticeUpdate, jobWording{}))
	return true
}

// agentSig is the signature beside the lab-agent labd hands out, or false.
func (s *Server) agentSig() ([]byte, bool) {
	if s.cfg.AgentBinary == "" {
		return nil, false
	}
	b, err := os.ReadFile(s.cfg.AgentBinary + ".sig")
	return b, err == nil && len(b) > 0
}

// hAgentSig hands out the signature of the lab-agent built with this labd.
func (s *Server) hAgentSig(w http.ResponseWriter, r *http.Request) {
	sig, ok := s.agentSig()
	if !ok {
		httpErr(w, 404, "this lab's lab-agent has no signature: it was not built as part of a Lab release")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(sig)
}

// releaseKey is the release key this labd's files are checked against: the
// one it was built with, else the repository's.
func (s *Server) releaseKey() string {
	if s.cfg.ReleaseKey != "" {
		return s.cfg.ReleaseKey
	}
	if m := rePinnedKey.FindSubmatch(installer.Script); m != nil {
		return string(m[2])
	}
	return ""
}

// agentUpdating marks a Mac that `lab agent-update` holds: no job is sent,
// no Lease taken and no exec run on it until its lab-agent is back as to
// (once the Mac has answered), or agentUpdateWait after that. Back means
// started again: a check-in from another run than start, the one asked,
// whose facts say to. The same number in new bytes is the usual update, so
// the number alone, which labd already holds, says nothing.
type agentUpdating struct{ to, start string }

const agentBeingUpdated = "lab-agent is being updated"

// hAgentUpdate is `lab agent-update <mac>`: a Maintainer has a Lab-owned Mac
// that is idle take the lab's lab-agent.
func (s *Server) hAgentUpdate(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	_, signed := s.agentSig()
	mark := &agentUpdating{}
	v.mu.Lock()
	d := v.d
	hold := cmp.Or(v.updateHold, agentUpdateWait)
	why := v.notUpdatableLocked(signed)
	if why == "" {
		mark.start = v.agentStart
		v.updating = mark
	}
	v.mu.Unlock()
	if why != "" {
		httpErr(w, 409, "%s", why)
		return
	}
	var res api.AgentUpdate
	// The agent restarts within two thirds of the hold from sending its
	// result, which is before labd has it: well inside the hold, which
	// starts once labd's wait has ended.
	args := agent.UpdateArgs{RestartWithinMS: (hold * 2 / 3).Milliseconds()}
	if err := v.call(r.Context(), api.CmdAgentUpdate, "", args, agentUpdateWait, &res); err != nil {
		if errors.Is(err, errAgentRefused) {
			v.endUpdating(mark) // the agent's own refusal: nothing was installed
		} else {
			// A Maintainer gone, a timeout: the Mac may still be downloading.
			// Its result, if it comes, is answered forgotten, and a forgotten
			// update restarts nothing; hold the Mac a while for it.
			v.holdUpdating(mark)
		}
		httpErr(w, 502, "%s did not update its lab-agent: %v", d.Name, err)
		return
	}
	if !res.Changed {
		v.endUpdating(mark) // nothing restarts
		writeJSON(w, res)
		return
	}
	v.mu.Lock()
	mark.to = res.To
	v.heardAgentLocked()
	v.mu.Unlock()
	v.holdUpdating(mark)
	s.log.Printf("%s: lab-agent %s -> %s by %s", d.Name, res.From, res.To, callerOf(r).maintainer)
	writeJSON(w, res)
}

// notUpdatableLocked is why lab agent-update cannot update v now, or "".
// Caller holds v.mu.
func (v *dev) notUpdatableLocked(signed bool) string {
	d, lease := v.d, v.liveLeaseLocked()
	switch {
	case d.IsVolunteer():
		return fmt.Sprintf("%s is a Volunteer's Mac, and lab-agent is never pushed to a Volunteer's Mac: its Volunteer runs `%s` at the Mac, and My Macs shows them the line", d.Name, volunteerUpdateLine)
	case !signed:
		return "this lab has no signed lab-agent to hand out: it was not Deployed from a Lab release"
	case v.updating != nil:
		return fmt.Sprintf("%s: %s already", d.Name, agentBeingUpdated)
	case d.ActiveJob != "":
		return fmt.Sprintf("%s is not idle: job %s runs on it. lab agent-update waits for no job and no Lease", d.Name, d.ActiveJob)
	case lease != nil:
		return fmt.Sprintf("%s is not idle: %s holds a Lease on it until %s. lab agent-update waits for no job and no Lease", d.Name, lease.Who(), lease.Expires.Format(time.TimeOnly))
	case v.proving:
		return fmt.Sprintf("%s is not idle: a Proving reset of it is under way. lab agent-update waits for no job and no Lease", d.Name)
	case v.cleaning:
		return fmt.Sprintf("%s is not idle: the lab is removing leftover lab kernels from it. lab agent-update waits for no job and no Lease", d.Name)
	case len(v.queue)+len(v.parked) > 0:
		return fmt.Sprintf("%s is not idle: a job waits for it. lab agent-update waits for no job and no Lease", d.Name)
	case !v.aliveLocked():
		return fmt.Sprintf("%s is offline, so its lab-agent cannot be updated now", d.Name)
	case d.State != api.StateNew && d.State != api.StateReady:
		return fmt.Sprintf("%s is not idle: it is %s. lab agent-update waits for no job and no Lease", d.Name, d.State)
	case !versionAtLeast(d.Facts.AgentVersion, agentUpdateMinAgent):
		return fmt.Sprintf("%s runs lab-agent %s; lab agent-update needs %s or later, the first that updates itself: Join the Mac again to give it the lab's", d.Name, orStr(d.Facts.AgentVersion, "(unknown)"), agentUpdateMinAgent)
	// The start number is how labd knows the new lab-agent is back.
	case !v.heardSinceStart:
		return fmt.Sprintf("%s has not checked in since labd started, so labd could not tell when its new lab-agent is back: try again once it has", d.Name)
	case v.agentStart == "":
		return fmt.Sprintf("%s runs a lab-agent that sends no start number, so labd could not tell when a new one is back, nor keep it from restarting after labd let the Mac go: Join the Mac again to give it the lab's", d.Name)
	}
	return ""
}

// heardAgentLocked lets the Mac go once its lab-agent, started again, says
// it is the one lab agent-update installed. Caller holds v.mu.
func (v *dev) heardAgentLocked() {
	if u := v.updating; u != nil && u.to != "" && v.agentStart != u.start && v.factsStart == v.agentStart && v.d.Facts.AgentVersion == u.to {
		v.updating = nil
		v.bump()
	}
}

// holdUpdating lets the Mac go agentUpdateWait from now, if mark still
// holds it then.
func (v *dev) holdUpdating(mark *agentUpdating) {
	v.mu.Lock()
	hold := cmp.Or(v.updateHold, agentUpdateWait)
	v.mu.Unlock()
	time.AfterFunc(hold, func() { v.endUpdating(mark) })
}

// endUpdating lets the Mac go, if mark still holds it.
func (v *dev) endUpdating(mark *agentUpdating) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.updating == mark {
		v.updating = nil
		v.bump()
	}
}

// updatingRefusalLocked is the refusal of a Lease or an exec while lab
// agent-update holds the Mac, or nil. Caller holds v.mu.
func (v *dev) updatingRefusalLocked(what string) error {
	if v.updating == nil {
		return nil
	}
	return refuse(409, "%s: %s, so %s; try again once it is back", v.d.Name, agentBeingUpdated, what)
}
