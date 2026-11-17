package server

import (
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// brisbane is the time lines are told in. Brisbane keeps one offset all year.
var brisbane = time.FixedZone("AEST", 10*60*60)

// when is a time as a line says it: Sat 14:30 AEST.
func when(t time.Time) string { return t.In(brisbane).Format("Mon 15:04 MST") }

// agentsLines are the lines the owner's group "Agents" gets: one for each
// change of who someone is, each new Lab token and each cut a Maintainer
// makes, and the lab's operations alerts. The people lines carry the
// handle; the line for a forgetting carries only who carried it out.
type agentsLines struct{ l *groupLines }

func (a agentsLines) MTeamMarked(by, handle string) {
	a.l.post(lineAgents, by+" marked "+handle+" as M team.")
}

func (a agentsLines) MTeamUnmarked(by, handle string) {
	a.l.post(lineAgents, by+" removed the M team mark from "+handle+".")
}

func (a agentsLines) MaintainerAdded(by, handle string) {
	a.l.post(lineAgents, by+" made "+handle+" a Maintainer.")
}

func (a agentsLines) MaintainerRemoved(by, handle string) {
	a.l.post(lineAgents, by+" removed "+handle+" as a Maintainer.")
}

func (a agentsLines) Blocked(by, handle string) {
	a.l.post(lineAgents, by+" blocked "+handle+".")
}

func (a agentsLines) Unblocked(by, handle string) {
	a.l.post(lineAgents, by+" unblocked "+handle+".")
}

func (a agentsLines) RequestToLendAnswered(by, handle string, approved bool) {
	verb := " declined "
	if approved {
		verb = " approved "
	}
	a.l.post(lineAgents, by+verb+handle+"'s Request to lend.")
}

func (a agentsLines) Invited(by, handle string) {
	a.l.post(lineAgents, by+" gave "+handle+" an Invite.")
}

// LabTokenMade is a new Lab token: who, its name, its expiry. A zero
// expires is a token that never expires.
func (a agentsLines) LabTokenMade(handle, name string, expires time.Time) {
	end := "which never expires."
	if !expires.IsZero() {
		end = "which expires on " + expires.In(brisbane).Format(time.DateOnly) + "."
	}
	a.l.post(lineAgents, handle+` made the Lab token "`+name+`", `+end)
}

// LabTokenRevoked is a Maintainer revoking another person's Lab token.
func (a agentsLines) LabTokenRevoked(by, handle, name string) {
	a.l.post(lineAgents, by+" revoked "+handle+`'s Lab token "`+name+`".`)
}

// EndedAllAccess is a Maintainer using "End all access" on another person.
func (a agentsLines) EndedAllAccess(by, handle string) {
	a.l.post(lineAgents, by+` used "End all access" on `+handle+".")
}

// Forgot is a forgetting: who carried it out and when, and nobody's handle
// but theirs.
func (a agentsLines) Forgot(by string, at time.Time) {
	a.l.post(lineAgents, by+" carried out a forgetting at "+at.In(brisbane).Format("2006-01-02 15:04 MST")+".")
}

// Trouble tells "Agents" of a trouble named key, once: while it lasts,
// saying it again posts nothing. It says whether the line was recorded.
func (a agentsLines) Trouble(key, text string) bool {
	if !a.l.markOnce("trouble:"+key, "trouble") {
		return false
	}
	a.l.post(lineAgents, text)
	return true
}

// Recovered tells "Agents" the trouble named key is over, once, and only
// if "Agents" was told of it.
func (a agentsLines) Recovered(key, text string) bool {
	if !a.l.unmark("trouble:" + key) {
		return false
	}
	a.l.post(lineAgents, text)
	return true
}

// NeedsHands is a Lab-owned Mac starting to Need hands, once while it
// does; NoLongerNeedsHands is its end, and only after "Agents" was told.
// A Volunteer's Mac is its Volunteer's to hear of, by Notice.
func (a agentsLines) NeedsHands(key, mac, why string) {
	text := mac + " needs hands"
	if why != "" {
		text += ": " + strings.TrimSuffix(why, ".")
	}
	a.Trouble("needs-hands:"+key, text+". Its page says what to do.")
}

func (a agentsLines) NoLongerNeedsHands(key, mac string, now api.DeviceState) {
	var is string
	switch now {
	case api.StateReady:
		is = "it is ready again"
	case api.StateLeft:
		is = "it left the lab"
	default:
		is = "it is " + string(now) + " now"
	}
	a.Recovered("needs-hands:"+key, mac+" no longer needs hands: "+is+".")
}

// BackAfterRestart is the lab running again after the mini restarted, with
// how long it was Dark: from when labd was last known to run to this start.
func (a agentsLines) BackAfterRestart(from, to time.Time) {
	a.l.post(lineAgents, "The mini restarted and the lab is back. It was Dark for "+howLong(to.Sub(from))+", from "+when(from)+" to "+when(to)+".")
}

// LimiterRefusing is the refused-credential limiter starting to refuse an
// address; LimiterQuiet is it refusing none any more. The line names no
// address: the caller log does.
func (a agentsLines) LimiterRefusing() {
	a.Trouble("limiter", "The refused-credential limiter is refusing an address: over 10 refused credentials came from it in 10 minutes, and further bad ones get 429 for 15 minutes. The caller log names it.")
}

func (a agentsLines) LimiterQuiet() {
	a.Recovered("limiter", "The refused-credential limiter refuses nobody now.")
}

// WeeklyDigest is the one message of Monday 09:00 Brisbane, as
// postWeeklyDigest wrote it, a line for each thing.
func (a agentsLines) WeeklyDigest(text string) {
	a.l.record(lineAgents, text)
}
