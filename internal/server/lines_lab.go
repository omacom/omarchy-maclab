package server

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// labGroupLines are the lines the Lab group gets: one for each Lease taken,
// released, expired or ended, one for each event of lending a Mac, and the
// daily summary. A line about a Mac names the Mac only, never its
// Volunteer. A line about a request, an Invite or a Join command names the
// Maintainer who acted, never the person asking, and links to People.
// Renewals and extensions of a Lease are not posted, nor is a refused
// enrollment token, nor a window opening or closing on schedule.
type labGroupLines struct {
	l *groupLines
	// link is the address of a page of the lab, "" when labd has no Lab address.
	link func(path string) string
}

func (g labGroupLines) LeaseTaken(handle, mac string, until time.Time) {
	g.l.post(lineLab, handle+" leased "+mac+" until "+when(until)+".")
}

func (g labGroupLines) LeaseReleased(handle, mac string) {
	g.l.post(lineLab, handle+" released "+mac+".")
}

func (g labGroupLines) LeaseExpired(handle, mac string) {
	g.l.post(lineLab, handle+"'s Lease on "+mac+" expired.")
}

// LeaseEndedByMaintainer says who ended it and why; why may be empty.
func (g labGroupLines) LeaseEndedByMaintainer(by, handle, mac, why string) {
	text := by + " ended " + handle + "'s Lease on " + mac
	if why = strings.TrimRight(strings.TrimSpace(why), "."); why != "" {
		text += ": " + why
	}
	g.l.post(lineLab, text+".")
}

func (g labGroupLines) LeaseEndedLeftTeam(handle, mac string) {
	g.l.post(lineLab, handle+"'s Lease on "+mac+" ended: "+handle+" left the M team.")
}

func (g labGroupLines) LeaseEndedBlocked(handle, mac string) {
	g.l.post(lineLab, handle+"'s Lease on "+mac+" ended: "+handle+" was Blocked.")
}

func (g labGroupLines) LeaseEndedMacLeft(handle, mac string) {
	g.l.post(lineLab, handle+"'s Lease on "+mac+" ended: "+mac+" left the lab.")
}

// people posts a line of the People record with its link to People.
func (g labGroupLines) people(text string) {
	if at := g.link("/people"); at != "" {
		text += " " + at
	}
	g.l.post(lineLab, text)
}

func (g labGroupLines) RequestToLendMade() { g.people("A Request to lend is waiting.") }

func (g labGroupLines) RequestToLendWithdrawn() { g.people("A Request to lend was withdrawn.") }

func (g labGroupLines) RequestToLendAnswered(by string, approved bool) {
	if approved {
		g.people(by + " approved a Request to lend.")
	} else {
		g.people(by + " declined a Request to lend.")
	}
}

func (g labGroupLines) InviteMade(by string) { g.people(by + " gave an Invite.") }

func (g labGroupLines) CapRaised(by string, macs int) {
	g.people(by + " raised a person's cap to " + strconv.Itoa(macs) + " Macs.")
}

// JoinCommandMade names the Mac the command is for, and the Maintainer who
// made it for a Lab-owned Mac; by is empty when a person made their own.
func (g labGroupLines) JoinCommandMade(mac, by string) {
	if by == "" {
		g.people("A Join command was made for " + mac + ".")
	} else {
		g.people(by + " made a Join command for " + mac + ".")
	}
}

func (g labGroupLines) JoinCommandCancelled(mac, by string) {
	if by == "" {
		g.people("The Join command for " + mac + " was cancelled.")
	} else {
		g.people(by + " cancelled the Join command for " + mac + ".")
	}
}

func (g labGroupLines) JoinCommandExpired(mac string) {
	g.people("The Join command for " + mac + " expired unused.")
}

// MacJoined names the Mac, its Configuration and its kind (api.KindLabOwned
// or api.KindVolunteer).
func (g labGroupLines) MacJoined(mac, configuration, kind string) {
	as := "a Lab-owned Mac"
	if kind == api.KindVolunteer {
		as = "a Volunteer's Mac"
	}
	g.l.post(lineLab, mac+" Joined as "+as+" ("+orUnknown(configuration)+").")
}

func orUnknown(configuration string) string {
	if strings.TrimSpace(configuration) == "" {
		return "Configuration unknown"
	}
	return configuration
}

func (g labGroupLines) FirstBaseline(mac string, passed bool) {
	if passed {
		g.l.post(lineLab, mac+" passed its first baseline.")
	} else {
		g.l.post(lineLab, mac+" failed its first baseline.")
	}
}

// WindowChanged is a change of the weekly schedule; schedule, which may be
// empty, is the new one in words.
func (g labGroupLines) WindowChanged(mac, schedule string) {
	text := mac + " changed its weekly Availability window"
	if schedule = strings.TrimRight(strings.TrimSpace(schedule), "."); schedule != "" {
		text += ": " + schedule
	}
	g.l.post(lineLab, text+".")
}

// Paused is a Pause until a time, or until Resume when until is zero.
func (g labGroupLines) Paused(mac string, until time.Time) {
	if until.IsZero() {
		g.l.post(lineLab, mac+" is Paused.")
	} else {
		g.l.post(lineLab, mac+" is Paused until "+when(until)+".")
	}
}

func (g labGroupLines) OpenNow(mac string, until time.Time) {
	if until.IsZero() {
		g.l.post(lineLab, mac+" is open now.")
	} else {
		g.l.post(lineLab, mac+" is open now, until "+when(until)+".")
	}
}

func (g labGroupLines) Resumed(mac string) { g.l.post(lineLab, mac+" resumed.") }

func (g labGroupLines) AccessLevelRaised(mac string) {
	g.l.post(lineLab, mac+" raised its Access level to Full.")
}

func (g labGroupLines) AccessLevelLowered(mac string) {
	g.l.post(lineLab, mac+" lowered its Access level to Qualification.")
}

// Limited is "Limit to Qualification" from the site; LimitRemoved undoes it.
func (g labGroupLines) Limited(mac string) {
	g.l.post(lineLab, mac+" is limited to Qualification.")
}

func (g labGroupLines) LimitRemoved(mac string) {
	g.l.post(lineLab, "The limit on "+mac+" was removed.")
}

func (g labGroupLines) Publishing(mac string, on bool) {
	if on {
		g.l.post(lineLab, mac+" turned publishing on.")
	} else {
		g.l.post(lineLab, mac+" turned publishing off.")
	}
}

// MacOSRunDue is a Lab-owned Attended Mac whose macOS run is due: it waits
// for a person at the Mac until then, and links to the Mac's page, where
// they say so.
func (g labGroupLines) MacOSRunDue(mac string, until time.Time) {
	text := "The macOS run of " + mac + " is due: it waits for a person at the Mac until " + when(until) + ", else it is dropped."
	if at := g.link("/macs/" + mac); at != "" {
		text += " Say you are at it on its page: " + at
	}
	g.l.post(lineLab, text)
}

func (g labGroupLines) MacLeftByVolunteer(mac string) {
	g.l.post(lineLab, mac+" left the lab: its Volunteer ended it.")
}

func (g labGroupLines) MacLeftByMaintainer(mac string) {
	g.l.post(lineLab, mac+" left the lab: a Maintainer ended it.")
}

func (g labGroupLines) MacLeftVolunteerBlocked(mac string) {
	g.l.post(lineLab, mac+" left the lab: its Volunteer was Blocked.")
}

// MacSummary is one Mac in the daily summary: its name, its Configuration
// and its state, which is one of the Mac* words or MacLeased.
type MacSummary struct {
	Name, Configuration, State string
}

const (
	MacFree          = "free"
	MacRunningJob    = "running a job"
	MacOutsideWindow = "outside its window"
	MacPaused        = "Paused"
	MacNotSeen       = "not seen"
)

// MacLeased is the state of a Mac under a Lease: by whom, until when.
func MacLeased(handle string, until time.Time) string {
	return "leased by " + handle + " until " + when(until)
}

// DailySummary is the one message of 08:00 Brisbane: each Mac with its
// Configuration and state, the count of Requests to lend waiting, and a
// link to Macs.
func (g labGroupLines) DailySummary(day time.Time, macs []MacSummary, requestsWaiting int) {
	sort.Slice(macs, func(i, k int) bool { return macs[i].Name < macs[k].Name })
	var b strings.Builder
	b.WriteString("Omarchy Mac Lab, " + day.In(brisbane).Format("Mon 2 Jan") + "\n")
	for _, m := range macs {
		b.WriteString(oneLine(m.Name) + " (" + oneLine(orUnknown(m.Configuration)) + "): " + oneLine(m.State) + "\n")
	}
	if len(macs) == 0 {
		b.WriteString("No Mac is enrolled.\n")
	}
	b.WriteString("Requests to lend waiting: " + strconv.Itoa(requestsWaiting))
	if at := g.link("/macs"); at != "" {
		b.WriteString("\nMacs: " + at)
	}
	g.l.record(lineLab, b.String())
}
