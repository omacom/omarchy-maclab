package server

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// Each event has its own function, and each function its own line.
func TestEveryEventHasItsLine(t *testing.T) {
	h := newLines4Test(t, nil, "")
	a := agentsLines{h.l}
	g := labGroupLines{l: h.l, link: func(path string) string { return "https://lab.omarchy-mac.org" + path }}
	until := time.Date(2026, 11, 14, 4, 30, 0, 0, time.UTC) // Sat 14:30 in Brisbane
	cases := []struct {
		channel string
		post    func()
		want    string
	}{
		// "Agents": who someone is, new Lab tokens, a Maintainer's cuts.
		{"agents", func() { a.MTeamMarked("grace", "ada") }, "grace marked ada as M team."},
		{"agents", func() { a.MTeamUnmarked("grace", "ada") }, "grace removed the M team mark from ada."},
		{"agents", func() { a.MaintainerAdded("owner", "grace") }, "owner made grace a Maintainer."},
		{"agents", func() { a.MaintainerRemoved("owner", "grace") }, "owner removed grace as a Maintainer."},
		{"agents", func() { a.Blocked("grace", "ada") }, "grace blocked ada."},
		{"agents", func() { a.Unblocked("grace", "ada") }, "grace unblocked ada."},
		{"agents", func() { a.RequestToLendAnswered("grace", "ada", true) }, "grace approved ada's Request to lend."},
		{"agents", func() { a.RequestToLendAnswered("grace", "ada", false) }, "grace declined ada's Request to lend."},
		{"agents", func() { a.Invited("grace", "ada") }, "grace gave ada an Invite."},
		{"agents", func() { a.LabTokenMade("ada", "laptop", time.Date(2027, 2, 12, 20, 0, 0, 0, time.UTC)) }, `ada made the Lab token "laptop", which expires on 2027-02-13.`},
		{"agents", func() { a.LabTokenMade("ada", "ci", time.Time{}) }, `ada made the Lab token "ci", which never expires.`},
		{"agents", func() { a.LabTokenRevoked("grace", "ada", "laptop") }, `grace revoked ada's Lab token "laptop".`},
		{"agents", func() { a.EndedAllAccess("grace", "ada") }, `grace used "End all access" on ada.`},
		{"agents", func() { a.Forgot("grace", time.Date(2026, 11, 14, 23, 12, 0, 0, time.UTC)) }, "grace carried out a forgetting at 2026-11-15 09:12 AEST."},
		{"agents", func() { a.NeedsHands("d-1", "m1-pro", "did not come back from a test kernel") }, "m1-pro needs hands: did not come back from a test kernel. Its page says what to do."},
		{"agents", func() { a.NoLongerNeedsHands("d-1", "m1-pro", api.StateReady) }, "m1-pro no longer needs hands: it is ready again."},
		{"agents", func() { a.NeedsHands("d-2", "m2-max", "") }, "m2-max needs hands. Its page says what to do."},
		{"agents", func() { a.NoLongerNeedsHands("d-2", "m2-max", api.StateLeft) }, "m2-max no longer needs hands: it left the lab."},

		// The Lab group: Leases.
		{"lab", func() { g.LeaseTaken("ada", "m1-pro", until) }, "ada leased m1-pro until Sat 14:30 AEST."},
		{"lab", func() { g.LeaseReleased("ada", "m1-pro") }, "ada released m1-pro."},
		{"lab", func() { g.LeaseExpired("ada", "m1-pro") }, "ada's Lease on m1-pro expired."},
		{"lab", func() { g.LeaseEndedByMaintainer("grace", "ada", "m1-pro", "needed for the Release.") }, "grace ended ada's Lease on m1-pro: needed for the Release."},
		{"lab", func() { g.LeaseEndedByMaintainer("grace", "ada", "m1-pro", "") }, "grace ended ada's Lease on m1-pro."},
		{"lab", func() { g.LeaseEndedLeftTeam("ada", "m1-pro") }, "ada's Lease on m1-pro ended: ada left the M team."},
		{"lab", func() { g.LeaseEndedBlocked("ada", "m1-pro") }, "ada's Lease on m1-pro ended: ada was Blocked."},

		// The Lab group: the People record's lines, with no handle but a
		// Maintainer's.
		{"lab", func() { g.RequestToLendMade() }, "A Request to lend is waiting. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.RequestToLendWithdrawn() }, "A Request to lend was withdrawn. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.RequestToLendAnswered("grace", true) }, "grace approved a Request to lend. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.RequestToLendAnswered("grace", false) }, "grace declined a Request to lend. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.InviteMade("grace") }, "grace gave an Invite. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.CapRaised("grace", 8) }, "grace raised a person's cap to 8 Macs. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.JoinCommandMade("m2-air", "") }, "A Join command was made for m2-air. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.JoinCommandMade("m1-pro", "grace") }, "grace made a Join command for m1-pro. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.JoinCommandCancelled("m2-air", "") }, "The Join command for m2-air was cancelled. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.JoinCommandCancelled("m2-air", "grace") }, "grace cancelled the Join command for m2-air. https://lab.omarchy-mac.org/people"},
		{"lab", func() { g.JoinCommandExpired("m2-air") }, "The Join command for m2-air expired unused. https://lab.omarchy-mac.org/people"},

		// The Lab group: the Mac's record's lines, which name the Mac only.
		{"lab", func() { g.MacJoined("m2-air", "M2, 4+4 cores", "volunteer") }, "m2-air Joined as a Volunteer's Mac (M2, 4+4 cores)."},
		{"lab", func() { g.MacJoined("m1-pro", "", "lab-owned") }, "m1-pro Joined as a Lab-owned Mac (Configuration unknown)."},
		{"lab", func() { g.FirstBaseline("m2-air", true) }, "m2-air passed its first baseline."},
		{"lab", func() { g.FirstBaseline("m2-air", false) }, "m2-air failed its first baseline."},
		{"lab", func() { g.WindowChanged("m2-air", "Mon-Fri 22:00-06:00") }, "m2-air changed its weekly Availability window: Mon-Fri 22:00-06:00."},
		{"lab", func() { g.WindowChanged("m2-air", "") }, "m2-air changed its weekly Availability window."},
		{"lab", func() { g.Paused("m2-air", until) }, "m2-air is Paused until Sat 14:30 AEST."},
		{"lab", func() { g.Paused("m2-air", time.Time{}) }, "m2-air is Paused."},
		{"lab", func() { g.OpenNow("m2-air", until) }, "m2-air is open now, until Sat 14:30 AEST."},
		{"lab", func() { g.OpenNow("m2-air", time.Time{}) }, "m2-air is open now."},
		{"lab", func() { g.Resumed("m2-air") }, "m2-air resumed."},
		{"lab", func() { g.AccessLevelRaised("m2-air") }, "m2-air raised its Access level to Full."},
		{"lab", func() { g.AccessLevelLowered("m2-air") }, "m2-air lowered its Access level to Qualification."},
		{"lab", func() { g.Limited("m2-air") }, "m2-air is limited to Qualification."},
		{"lab", func() { g.LimitRemoved("m2-air") }, "The limit on m2-air was removed."},
		{"lab", func() { g.Publishing("m2-air", true) }, "m2-air turned publishing on."},
		{"lab", func() { g.Publishing("m2-air", false) }, "m2-air turned publishing off."},
		{"lab", func() { g.MacLeftByVolunteer("m2-air") }, "m2-air left the lab: its Volunteer ended it."},
		{"lab", func() { g.MacLeftByMaintainer("m2-air") }, "m2-air left the lab: a Maintainer ended it."},
		{"lab", func() { g.MacLeftVolunteerBlocked("m2-air") }, "m2-air left the lab: its Volunteer was Blocked."},
	}
	for _, c := range cases {
		c.post()
	}
	rec := h.recorded()
	if len(rec) != len(cases) {
		t.Fatalf("%d lines recorded for %d events", len(rec), len(cases))
	}
	for i, c := range cases {
		if rec[i].text != c.want || rec[i].channel != c.channel {
			t.Errorf("%s got %q\n              want %s %q", rec[i].channel, rec[i].text, c.channel, c.want)
		}
		if strings.Contains(rec[i].text, "\n") {
			t.Errorf("%q is more than one line", rec[i].text)
		}
	}
}

// With no Lab address a line of the People record carries no link.
func TestALineLinksToPeopleOnlyAtALabAddress(t *testing.T) {
	h := newLines4Test(t, nil, "")
	s := &Server{}
	labGroupLines{l: h.l, link: s.pageAddress}.RequestToLendMade()
	u, _ := url.Parse("https://lab.omarchy-mac.org")
	s.cfg.PublicURL = u
	labGroupLines{l: h.l, link: s.pageAddress}.RequestToLendMade()
	rec := h.recorded()
	if rec[0].text != "A Request to lend is waiting." || rec[1].text != "A Request to lend is waiting. https://lab.omarchy-mac.org/people" {
		t.Fatalf("recorded %q and %q", rec[0].text, rec[1].text)
	}
}

func TestTheDailySummaryNamesEachMacItsConfigurationAndState(t *testing.T) {
	sig := &fakeSignal{}
	h := newLines4Test(t, sig, signalDir(t))
	g := labGroupLines{l: h.l, link: func(path string) string { return "https://lab.omarchy-mac.org" + path }}
	until := time.Date(2026, 11, 15, 2, 30, 0, 0, time.UTC)
	g.DailySummary(time.Date(2026, 11, 14, 22, 0, 0, 0, time.UTC), []MacSummary{
		{"m2-max", "M2 Max, 8+4 cores", MacLeased("ada", until)},
		{"m1-pro", "M1 Pro, 8+2 cores", MacFree},
		{"m2-air", "", MacOutsideWindow},
		{"studio", "M1 Ultra, 16+4 cores", MacRunningJob},
		{"mini-7", "M2, 4+4 cores", MacPaused},
		{"air-3", "M1, 4+4 cores", MacNotSeen},
	}, 2)
	h.settle()
	want := `Omarchy Mac Lab, Sun 15 Nov
air-3 (M1, 4+4 cores): not seen
m1-pro (M1 Pro, 8+2 cores): free
m2-air (Configuration unknown): outside its window
m2-max (M2 Max, 8+4 cores): leased by ada until Sun 12:30 AEST
mini-7 (M2, 4+4 cores): Paused
studio (M1 Ultra, 16+4 cores): running a job
Requests to lend waiting: 2
Macs: https://lab.omarchy-mac.org/macs`
	got := sig.lines()
	if len(got) != 1 || got[0].group != testLabGroup || got[0].text != want {
		t.Fatalf("the summary went as %+v\nwant\n%s", got, want)
	}

	g.DailySummary(time.Date(2026, 11, 15, 22, 0, 0, 0, time.UTC), nil, 0)
	h.settle()
	if got := sig.lines()[1].text; got != "Omarchy Mac Lab, Mon 16 Nov\nNo Mac is enrolled.\nRequests to lend waiting: 0\nMacs: https://lab.omarchy-mac.org/macs" {
		t.Errorf("with no Mac the summary is %q", got)
	}
}
