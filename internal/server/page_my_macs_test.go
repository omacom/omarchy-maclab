package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/window"
)

// My Macs (spec, section 5): one card per Mac with everything its Volunteer
// may see and do, and above the cards what is on the way to the lab.

// myMacsLab is a lab that makes Join commands, takes reports and publishes.
func myMacsLab(t *testing.T) *signInLab {
	t.Helper()
	return rolesLab(t, func(c *Config) {
		bin := filepath.Join(t.TempDir(), "lab-agent")
		if err := os.WriteFile(bin, []byte("\x7fELF lab-agent for the test"), 0o755); err != nil {
			t.Fatal(err)
		}
		c.AgentBinary = bin
		c.MachineIDKey = testMachineIDKey()
		c.OMT, c.OMTPublish = true, true
	})
}

// mainOf is the page's own part, read as a browser reads it.
func mainOf(t *testing.T, body string) *node {
	t.Helper()
	return parseHTML(t, part(t, body, `<main class="site-main">`, `</main>`))
}

// myMacs is My Macs as b sees it.
func (b *browser) myMacs() *node {
	b.l.t.Helper()
	return mainOf(b.l.t, b.page("/my-macs"))
}

// mine is "Mine" on Macs as b sees it.
func (b *browser) mine() *node {
	b.l.t.Helper()
	return mainOf(b.l.t, b.page("/macs?show=mine"))
}

func ids(nodes []*node) string { return strings.Join(attrsOf(nodes, "id"), " ") }

func texts(nodes []*node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.words())
	}
	return out
}

// factsOf are a card's facts, each as its label and what it says.
func factsOf(t *testing.T, card *node) string {
	t.Helper()
	var out []string
	for _, c := range card.sole(t, "dl.facts").children {
		if c.tag == "dt" || c.tag == "dd" {
			out = append(out, c.words())
		}
	}
	return strings.Join(out, " ")
}

// lentMac enrolls a Volunteer's Mac that passed its baseline, lent by this
// account, with a weekly window that is open now until then.
func (l *signInLab) lentMac(name string, volunteer int64, level string, openUntil time.Time) *dev {
	l.t.Helper()
	rule, err := window.ParseRule("mon-fri 19:00-07:00")
	if err != nil {
		l.t.Fatal(err)
	}
	return l.lrMac(name, "j313", func(d *api.Device) {
		lrVolunteers(level, openUntil)(d)
		d.VolunteerID = volunteer
		if d.Window == nil {
			d.Window = &window.Settings{}
		}
		d.Window.Rules, d.Window.Zone = []window.Rule{rule}, "Australia/Brisbane"
	})
}

// Above the cards: an unused Join command with its expiry and Cancel, "Lend
// another Mac, n of 5", and the place of the changed-terms banner, which
// stays empty while the terms are the ones accepted.
func TestAboveTheCardsAreTheJoinCommandAndLendAnotherMac(t *testing.T) {
	l := myMacsLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	grace.lent("shelf-mac")
	_, token := grace.makeJoin("desk-mac")
	expires := utcMinute(l.s.now().Add(time.Hour))

	body := grace.page("/my-macs")
	page := mainOf(t, body)
	if got := page.sole(t, "h1").words(); got != "My Macs" {
		t.Errorf("the page is headed %q", got)
	}
	join := page.sole(t, "#join-command")
	if said := join.words(); !strings.Contains(said, "desk-mac") || !strings.Contains(said, "good until "+expires) {
		t.Errorf("the unused Join command: %q, want its Mac's name and its expiry %s", said, expires)
	}
	cancel := join.sole(t, "form")
	if cancel.attr("action") != "/lend/join-command/cancel" || cancel.sole(t, "button").words() != "Cancel" ||
		cancel.sole(t, "button").attr("data-sentence") != "Cancel: this Join command stops working now" {
		t.Errorf("the Join command's Cancel: %q, %q", cancel.attr("action"), cancel.words())
	}
	if strings.Contains(body, token) || strings.Contains(body, "lab-agent setup") {
		t.Error("My Macs shows the Join command's line, which the lab shows once")
	}
	lend := page.sole(t, "#lend-another")
	if said := lend.words(); !strings.Contains(said, "Lend another Mac") || !strings.Contains(said, "2 of 5, the unused Join command among them") {
		t.Errorf("under the Join command: %q, want Lend another Mac, 2 of 5", said)
	}
	if href := lend.sole(t, "a").attr("href"); !strings.HasPrefix(href, "/lend") {
		t.Errorf("Lend another Mac leads to %q", href)
	}
	if page.count(t, "#terms-changed") != 0 {
		t.Error("the changed-terms banner shows while the terms are the ones accepted")
	}
	if got := ids(page.all(t, "section")); got != "join-command lend-another mac-shelf-mac" {
		t.Errorf("the page's parts, in order: %q", got)
	}
	// A Mac that just Joined: its stage in the words of the flow, and none of labd's notes to Maintainers.
	if facts := factsOf(t, page.sole(t, "#mac-shelf-mac")); !strings.Contains(facts, "State "+ArrivalNoWindow+" Availability window none yet") || strings.Contains(facts, "run a baseline") {
		t.Errorf("a Mac that just Joined: %s", facts)
	}

	// Cancel asks twice and comes back to My Macs.
	asked := grace.post("/lend/join-command/cancel", "back", "my-macs")
	if asked.Code != http.StatusOK || !strings.Contains(asked.Body.String(), "Cancel: this Join command stops working now") ||
		!strings.Contains(asked.Body.String(), `<a href="/my-macs">Leave it as it is</a>`) {
		t.Fatalf("the first Cancel: %d %s", asked.Code, refusalIn(asked.Body.String()))
	}
	if page := grace.myMacs(); page.count(t, "#join-command") != 1 {
		t.Fatal("the Join command went at the first click")
	}
	if w := grace.post("/lend/join-command/cancel", "back", "my-macs", "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs" {
		t.Fatalf("Cancel, sure: %d to %q", w.Code, w.Header().Get("Location"))
	}
	page = grace.myMacs()
	if page.count(t, "#join-command") != 0 || !strings.Contains(page.sole(t, "#lend-another").words(), "1 of 5") {
		t.Errorf("after Cancel: %q", page.sole(t, "#lend-another").words())
	}

	// A cap a Maintainer raised, and a person at their cap.
	ravi.did(personPath(graceID, "cap"), "macs", "7")
	if said := grace.myMacs().sole(t, "#lend-another").words(); !strings.Contains(said, "1 of 7") {
		t.Errorf("after the cap was raised to 7: %q", said)
	}
	for _, name := range []string{"b", "c", "d", "e", "f", "g"} {
		l.lentMac("mac-"+name, graceID, api.AccessQualification, time.Time{})
	}
	at := grace.myMacs().sole(t, "#lend-another")
	if said := at.words(); !strings.Contains(said, "7 of 7") || !strings.Contains(said, "your cap") || at.count(t, "a") != 0 {
		t.Errorf("at the cap: %q", said)
	}
}

// A card shows the Mac's name, Configuration and state in plain words; its
// Availability window, Access level, publishing and whether it is Eligible;
// its ntfy topic and whether job Notices are on; its jobs with who asked and
// how each ended; the Audit log, the Mac's record and the Exec record; who
// holds a Lease and until when; Pause, Limit and Leave as buttons; and the
// commands for what only the Mac can give.
func TestACardShowsEverythingItsVolunteerMaySeeAndDo(t *testing.T) {
	l := myMacsLab(t)
	windowEnd := l.s.now().Add(6 * time.Hour)
	v := l.lentMac("vol-air", linusID, api.AccessFull, windowEnd)
	v.mu.Lock()
	v.d.Publish, v.d.SnapshotEligible, v.d.JobNotices = true, true, true
	v.d.NtfyOwnURL, v.d.Facts.AgentVersion = "https://ntfy.example/linus-own-topic", "0.2.0"
	id := v.d.ID
	v.mu.Unlock()
	linus := l.signedIn(linusID)
	grace := l.lrMember(graceID)
	at := l.s.now().Add(-time.Hour)
	l.macJob("vol-air", "j-air", "ravi", at)
	failed := &api.Job{ID: "j-first", Spec: api.JobSpec{Device: "vol-air", Baseline: true}, DeviceID: id,
		State: api.JobDone, Outcome: api.OutcomeTestsFailed, Created: at.Add(-time.Hour), Updated: at}
	earlier := &api.Job{ID: "j-earlier", Spec: api.JobSpec{Device: "vol-air", Baseline: true}, DeviceID: "d-another-enrollment",
		State: api.JobDone, Outcome: api.OutcomePass, Created: at.Add(-48 * time.Hour), Updated: at, Maintainer: "edith"}
	for _, j := range []*api.Job{failed, earlier} {
		if err := l.s.store.saveJob(j); err != nil {
			t.Fatal(err)
		}
	}
	v.copyAudit([]api.AuditEntry{{Time: at, Maintainer: "ravi", Job: "j-air", Action: "boot linux-aurora 7.2", End: 10}})
	if _, err := l.s.store.addExec(id, api.ExecEvent{Time: at, Mac: "vol-air", AccountID: graceID, Handle: "grace", Command: "uname -a", State: api.ExecRefused, Why: "no Lease"}); err != nil {
		t.Fatal(err)
	}
	l.s.macSaid("vol-air", id, macJoined, "Joined as a Volunteer's Mac, at Qualification with publishing off")
	lease := grace.mustLease("vol-air", "grace@desk", 0)

	cardHTML := part(t, linus.page("/my-macs"), `id="mac-vol-air"`, "</section>\n")
	offersToCopy(t, cardHTML, "the Notices topic", "https://ntfy.example/linus-own-topic")
	offersToCopy(t, cardHTML, "the update command", "sudo lab-agent update")
	for _, c := range onlyAtTheMac {
		offersToCopy(t, cardHTML, c.Line, c.Line)
	}
	card := linus.myMacs().sole(t, "#mac-vol-air")
	if got := card.sole(t, "h2").words(); got != "vol-air" {
		t.Errorf("the card is named %q", got)
	}
	facts := factsOf(t, card)
	for _, want := range []string{
		"Configuration j313, core counts unknown",
		"State leased",
		"Availability window mon-fri 19:00-07:00 (Australia/Brisbane) open until " + windowEnd.In(brisbane).Format("Mon 2 Jan 15:04 MST"),
		"Access level Full",
		"Publishing on",
		"Installed runs Eligible",
		"Notices https://ntfy.example/linus-own-topic job Notices on",
		"Lease @grace M team until " + when(lease.Expires),
		"lab-agent 0.2.0 older than the lab's " + agent.Version + " Update it at the Mac: sudo lab-agent update",
	} {
		if !strings.Contains(facts, want) {
			t.Errorf("the card's facts lack %q:\n%s", want, facts)
		}
	}
	jobs := texts(card.all(t, "#jobs-vol-air tbody tr"))
	if len(jobs) != 2 || !has(jobs[:1], "j-air", "@ravi", "pass") || !has(jobs[1:], "j-first", "Baseline", "the lab", "tests failed") {
		t.Errorf("the card's jobs, who asked and how each ended, of this enrollment only: %q", jobs)
	}
	if href := card.all(t, "#jobs-vol-air tbody tr a")[0].attr("href"); href != "/jobs/j-air" {
		t.Errorf("a job links %q", href)
	}
	for part, want := range map[string]string{
		"#record-vol-air": "Joined as a Volunteer's Mac, at Qualification with publishing off",
		"#audit-vol-air":  "@ravi · job j-air · boot linux-aurora 7.2",
		"#exec-vol-air":   "@grace on vol-air: uname -a (refused: no Lease)",
	} {
		if !card.says(t, part+" li", want) {
			t.Errorf("%s lacks %q: %q", part, want, card.sole(t, part).words())
		}
	}
	if href := card.sole(t, "#record-vol-air a").attr("href"); href != "/macs/vol-air?tab=records" {
		t.Errorf("the records lead to %q", href)
	}
	controls := card.sole(t, "#controls-vol-air")
	if got := texts(controls.all(t, "button")); strings.Join(got, ", ") != "Pause, Limit to Qualification, Leave" {
		t.Errorf("the card's buttons: %q", got)
	}
	if got := attrsOf(controls.all(t, "form"), "action"); strings.Join(got, " ") != "/my-macs/vol-air/pause /my-macs/vol-air/limit /my-macs/vol-air/leave" {
		t.Errorf("the buttons post to %q", got)
	}
	if controls.count(t, `form[action="/my-macs/vol-air/pause"] input[name=until]`) != 1 {
		t.Error("Pause has no place for a time")
	}
	if got := controls.sole(t, `form[action="/my-macs/vol-air/leave"] button`).attr("data-sentence"); got != leaveSentence("vol-air") {
		t.Errorf("Leave turns into %q", got)
	}
	for _, f := range controls.all(t, "form") {
		if f.count(t, `input[name=csrf_token]`) != 1 || f.attr("method") != "post" {
			t.Errorf("the form to %s is no post bound to the session", f.attr("action"))
		}
	}
	commands := strings.Join(texts(card.all(t, "#commands-vol-air code")), "\n")
	for _, want := range []string{"sudo lab-agent window add mon-fri 19:00-07:00", "sudo lab-agent open --until 23:00", "sudo lab-agent access full",
		"sudo lab-agent publish on", "sudo lab-agent notify --topic", "sudo lab-agent notify --jobs on", "sudo lab-agent update", "sudo lab-agent leave"} {
		if !strings.Contains(commands, want) {
			t.Errorf("the commands for what only the Mac can give lack %q:\n%s", want, commands)
		}
	}
	// Nothing on the site gives but the weekly window (window_from_site_test.go).
	for _, f := range card.all(t, "form") {
		for _, gives := range []string{"open", "access", "publish", "full"} {
			if strings.HasSuffix(f.attr("action"), "/"+gives) {
				t.Errorf("the card has a form that gives: %s", f.attr("action"))
			}
		}
	}

	// Pause and Limit, taken: the card says so and offers only their undoing.
	linus.did("/my-macs/vol-air/pause", "sure", "yes")
	linus.did("/my-macs/vol-air/limit", "sure", "yes")
	card = linus.myMacs().sole(t, "#mac-vol-air")
	facts = factsOf(t, card)
	for _, want := range []string{"State Paused", "Paused here until you Resume", "Access level Qualification here, by your Limit; Full on the Mac", "Lease none"} {
		if !strings.Contains(facts, want) {
			t.Errorf("Paused and limited, the facts lack %q:\n%s", want, facts)
		}
	}
	if got := texts(card.all(t, "#controls-vol-air button")); strings.Join(got, ", ") != "Resume, Remove limit, Leave" {
		t.Errorf("Paused and limited, the buttons: %q", got)
	}
	until := l.s.now().Add(26 * time.Hour).Truncate(time.Minute)
	linus.did("/my-macs/vol-air/resume")
	linus.did("/my-macs/vol-air/pause", "until", until.In(brisbane).Format("2006-01-02T15:04"), "sure", "yes")
	if facts := factsOf(t, linus.myMacs().sole(t, "#mac-vol-air")); !strings.Contains(facts, "Paused here until "+until.In(brisbane).Format("Mon 2 Jan 15:04 MST")) {
		t.Errorf("Paused until a time, the facts: %s", facts)
	}

	// A refusal is said on My Macs.
	linus.did("/my-macs/vol-air/resume")
	w := linus.post("/my-macs/vol-air/resume")
	if w.Code != http.StatusConflict || mainOf(t, w.Body.String()).sole(t, "h1").words() != "My Macs" || !strings.Contains(refusalIn(w.Body.String()), "The site undoes only what it took") {
		t.Errorf("a refused Resume: %d %q", w.Code, refusalIn(w.Body.String()))
	}

	// What the Mac has not given, and what it has turned off.
	v.mu.Lock()
	v.d.AccessLevel, v.d.Publish, v.d.SnapshotEligible, v.d.JobNotices, v.d.NtfyOwnURL = api.AccessQualification, false, false, false, ""
	v.d.Window = nil
	v.d.Facts.AgentVersion = agent.Version
	v.mu.Unlock()
	linus.did("/my-macs/vol-air/remove-limit")
	facts = factsOf(t, linus.myMacs().sole(t, "#mac-vol-air"))
	for _, want := range []string{"Availability window none yet", "Access level Qualification", "Publishing off", "Installed runs not Eligible", "job Notices off", "lab-agent " + agent.Version} {
		if !strings.Contains(facts, want) {
			t.Errorf("the facts lack %q:\n%s", want, facts)
		}
	}
	if strings.Contains(facts, "older than") || strings.Contains(facts, "Update it") {
		t.Errorf("a Mac on the lab's lab-agent is told to update it:\n%s", facts)
	}
}

// For a Mac that Needs hands the card has the steps, the reason and the
// last log line.
func TestACardOfAMacThatNeedsHandsHasTheStepsTheReasonAndTheLastLogLine(t *testing.T) {
	l := myMacsLab(t)
	v := l.lentMac("vol-air", linusID, api.AccessQualification, l.s.now().Add(time.Hour))
	linus := l.signedIn(linusID)
	if linus.myMacs().count(t, "#hands-vol-air") != 0 {
		t.Fatal("a Mac that needs nothing has the steps")
	}
	v.mu.Lock()
	v.d.State, v.d.StateReason = api.StateNeedsHands, "did not come back from a test kernel"
	v.d.Action = &api.HumanAction{Title: "vol-air needs a power cycle", Why: "It did not come back from a test kernel, and its own watchdog did not restart it.",
		Steps: []string{"Find vol-air. Open the lid if it's closed.", "Hold the power button for 10 seconds."}, LastLog: "nvme nvme0: I/O timeout", Since: l.s.now()}
	v.mu.Unlock()

	card := linus.myMacs().sole(t, "#mac-vol-air")
	if facts := factsOf(t, card); !strings.Contains(facts, "State needs hands") {
		t.Errorf("the state: %s", facts)
	}
	hands := card.sole(t, "#hands-vol-air")
	if got := texts(hands.all(t, "ol li")); len(got) != 2 || got[0] != "Find vol-air. Open the lid if it's closed." || got[1] != "Hold the power button for 10 seconds." {
		t.Errorf("the steps: %q", got)
	}
	for _, want := range []string{"vol-air needs a power cycle", "It did not come back from a test kernel, and its own watchdog did not restart it.", "nvme nvme0: I/O timeout"} {
		if !strings.Contains(hands.words(), want) {
			t.Errorf("the card lacks %q: %s", want, hands.words())
		}
	}
}

// The Mac's Volunteer deletes a report the lab published from their Mac,
// for good, from its card; it asks twice. The job then says who deleted its
// report and when. Nobody else deletes it there, and nothing else is
// deleted there.
func TestTheVolunteerDeletesALabPublishedReportFromMyMacs(t *testing.T) {
	l := myMacsLab(t)
	v := l.lentMac("vol-air", linusID, api.AccessQualification, l.s.now().Add(2*time.Hour))
	l.lentMac("other-air", edithID, api.AccessQualification, l.s.now().Add(2*time.Hour))
	l.lrMac("m2-max", "j416c", nil)
	for _, name := range []string{"vol-air", "other-air"} {
		m := l.s.dev(name)
		m.mu.Lock()
		m.d.Publish = true
		m.mu.Unlock()
	}
	ravi, linus, edith := l.lrMaintainer(), l.signedIn(linusID), l.signedIn(edithID)
	l.ran("j-one", "vol-air", 0, nil, nil)
	l.ran("j-two", "vol-air", 0, nil, nil)
	l.ran("j-others", "other-air", 0, nil, nil)
	l.ran("j-labs", "m2-max", 0, nil, nil)
	one, two, others, labs := ravi.published("j-one"), ravi.published("j-two"), ravi.published("j-others"), ravi.published("j-labs")
	ownID := l.storedReport(l.macUpload("vol-air", "m2-max-converged")).PublicID
	// The Mac's name was another person's Mac before: what the lab published then is not this Mac's.
	l.ran("j-earlier", "vol-air", 0, nil, func(j *api.Job) {
		j.DeviceID, j.Result.OMT.ReportID, j.Result.OMT.Published = "d-earlier", labs, labAddress+"/reports/"+labs
	})

	card := linus.myMacs().sole(t, "#mac-vol-air")
	rows := card.all(t, "#reports-vol-air li")
	if len(rows) != 2 {
		t.Fatalf("the card lists %d lab-published reports, want 2: %q", len(rows), texts(rows))
	}
	// Newest first, each with its link, its job and Delete.
	if href := rows[0].sole(t, "a").attr("href"); href != "/reports/"+two {
		t.Errorf("the newest report links %q, want %s", href, two)
	}
	del := rows[1].sole(t, "form")
	if del.attr("action") != "/my-macs/vol-air/reports/"+one+"/delete" || del.sole(t, "button").words() != "Delete" || !strings.Contains(rows[1].words(), "j-one") {
		t.Errorf("the older report's Delete: %q in %q", del.attr("action"), rows[1].words())
	}
	if body := linus.page("/my-macs"); strings.Contains(body, "deletion?token=") || strings.Contains(body, ownID) || strings.Contains(body, others) || strings.Contains(body, labs) {
		t.Error("My Macs shows a deletion link, the Mac's own upload, another Mac's report or an earlier enrollment's")
	}

	at := func(mac, id string) string { return "/my-macs/" + mac + "/reports/" + id + "/delete" }
	// Nobody else, and nothing else: another Volunteer, a Maintainer who
	// lends no such Mac, another Mac's report, one the lab published when
	// the name was another enrollment's, the Mac's own upload, a report that
	// is not there.
	for why, c := range map[string]struct {
		b      *browser
		target string
	}{
		"another Volunteer":                   {edith, at("vol-air", one)},
		"a Maintainer, from My Macs":          {ravi, at("vol-air", one)},
		"another Mac's report":                {linus, at("vol-air", others)},
		"a report under another's Mac's name": {linus, at("other-air", others)},
		"an earlier enrollment's report":      {linus, at("vol-air", labs)},
		"the Mac's own upload":                {linus, at("vol-air", ownID)},
		"no such report":                      {linus, at("vol-air", "nope")},
	} {
		if w := c.b.post(c.target, "sure", "yes"); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", why, w.Code)
		}
	}
	if n := len(l.everyReport()); n != 5 {
		t.Fatalf("%d reports are left of 5 after the refused deletions", n)
	}

	// It asks twice.
	asked := linus.post(at("vol-air", one))
	if asked.Code != http.StatusOK || !strings.Contains(asked.Body.String(), "Delete this report and its evidence for good") || len(l.everyReport()) != 5 {
		t.Fatalf("the first Delete: %d, %d reports", asked.Code, len(l.everyReport()))
	}
	l.clock.Advance(time.Minute)
	if w := linus.post(at("vol-air", one), "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs#mac-vol-air" {
		t.Fatalf("Delete, sure: %d to %q %s", w.Code, w.Header().Get("Location"), refusalIn(w.Body.String()))
	}
	if w := l.browser().do("GET", "/reports/"+one, ""); w.Code != http.StatusNotFound {
		t.Errorf("the deleted report's page: %d", w.Code)
	}
	if w := l.browser().do("GET", "/reports/"+two, ""); w.Code != http.StatusOK {
		t.Errorf("the other report's page: %d", w.Code)
	}
	said := ravi.jobSaid("j-one").Result.OMT
	if said.ReportState != api.ReportIsDeleted || said.ReportDeleted == nil || said.ReportDeleted.By != "the Mac's Volunteer" ||
		said.ReportDeleted.AccountID != linusID || !said.ReportDeleted.Time.Equal(l.s.now()) {
		t.Errorf("the job of the deleted report says %q, %+v", said.ReportState, said.ReportDeleted)
	}
	if _, err := os.Stat(l.s.deletionLinkPath("j-one")); !os.IsNotExist(err) {
		t.Errorf("the link of the deleted report is still kept: %v", err)
	}
	if got := l.recorded(); !has(got, "@linus", "deleted the report "+one, "vol-air") {
		t.Errorf("the record lacks the deletion:\n%s", strings.Join(got, "\n"))
	}
	if rows := linus.myMacs().all(t, "#reports-vol-air li"); len(rows) != 1 || rows[0].sole(t, "a").attr("href") != "/reports/"+two {
		t.Errorf("after the deletion the card lists %q", texts(rows))
	}
	// The lab does not publish that job again.
	ravi.notPublished("j-one", http.StatusConflict, "its report was deleted by the Mac's Volunteer")
	// Again is a report that is not there.
	if w := linus.post(at("vol-air", one), "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("deleting it again: %d", w.Code)
	}

	// A report a Maintainer hid is still the Volunteer's to delete.
	ravi.did("/reports/" + two + "/hide")
	row := linus.myMacs().sole(t, "#reports-vol-air li")
	if !strings.Contains(row.words(), "Hidden by a Maintainer") || row.count(t, "a") != 0 {
		t.Errorf("a Hidden report on the card: %q", row.words())
	}
	linus.did(at("vol-air", two), "sure", "yes")
	if n := len(linus.myMacs().all(t, "#reports-vol-air li")); n != 0 {
		t.Errorf("%d reports left on the card", n)
	}
	// The switch gates publishing only: off, the card still lists what is up.
	l.ran("j-three", "vol-air", 0, nil, nil)
	three := ravi.published("j-three")
	v.mu.Lock()
	v.d.Publish = false
	v.mu.Unlock()
	if row := linus.myMacs().sole(t, "#reports-vol-air li"); row.sole(t, "a").attr("href") != "/reports/"+three {
		t.Errorf("with publishing off the card lists %q", row.words())
	}
}

// An M team member or a Maintainer sees the same cards under "Mine" on
// Macs. An External volunteer sees only their own. To someone who lends and
// lent nothing there is no such page.
func TestMineOnMacsHasTheSameCardsAndAnExternalVolunteerSeesOnlyTheirOwn(t *testing.T) {
	l := myMacsLab(t)
	open := l.s.now().Add(2 * time.Hour)
	l.lentMac("graces-air", graceID, api.AccessFull, open)
	l.lentMac("linus-air", linusID, api.AccessQualification, open)
	l.lentMac("ravis-air", maintainerID, api.AccessQualification, open)
	l.lrMac("m1-pro", "j316s", nil)
	for _, name := range []string{"graces-air", "linus-air", "ravis-air"} {
		v := l.s.dev(name)
		v.mu.Lock()
		v.d.NtfyOwnURL = "https://ntfy.example/topic-of-" + name
		v.mu.Unlock()
	}
	grace, linus, ravi := l.lrMember(graceID), l.signedIn(linusID), l.lrMaintainer()

	if w := l.browser().do("GET", "/my-macs", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/sign-in?return="+url.QueryEscape("/my-macs") {
		t.Errorf("signed out: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if w := l.signedIn(nobodyID).do("GET", "/my-macs", ""); w.Code != http.StatusNotFound {
		t.Errorf("someone who lends nothing: %d", w.Code)
	}

	// An External volunteer: their own Mac and no other, anywhere on the page.
	body := linus.page("/my-macs")
	if got := ids(mainOf(t, body).all(t, "section.mac-card")); got != "mac-linus-air" {
		t.Errorf("an External volunteer's cards: %q", got)
	}
	for _, others := range []string{"graces-air", "ravis-air", "m1-pro"} {
		if strings.Contains(body, others) {
			t.Errorf("an External volunteer's My Macs names %s", others)
		}
	}
	if w := linus.do("GET", "/macs?show=mine", ""); w.Code != http.StatusNotFound {
		t.Errorf("Macs, for an External volunteer: %d", w.Code)
	}

	// The M team and the Maintainers have them under Mine.
	for who, c := range map[string]struct {
		b   *browser
		mac string
	}{"an M team member": {grace, "graces-air"}, "a Maintainer": {ravi, "ravis-air"}} {
		if w := c.b.do("GET", "/my-macs", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/macs?show=mine" {
			t.Errorf("%s, My Macs: %d to %q", who, w.Code, w.Header().Get("Location"))
		}
		page := c.b.mine()
		if got := ids(page.all(t, "section.mac-card")); got != "mac-"+c.mac {
			t.Errorf("%s's cards under Mine: %q", who, got)
		}
		if page.sole(t, "h1").words() != "Macs" || page.sole(t, "nav.chips a.current").words() != "Mine" {
			t.Errorf("%s's Mine is not under Macs", who)
		}
		card := page.sole(t, "#mac-"+c.mac)
		if got := texts(card.all(t, "#controls-"+c.mac+" button")); strings.Join(got, ", ") != "Pause, Limit to Qualification, Leave" {
			t.Errorf("%s's buttons: %q", who, got)
		}
		if !strings.Contains(strings.Join(texts(card.all(t, "#commands-"+c.mac+" code")), "\n"), "sudo lab-agent update") {
			t.Errorf("%s's card lacks the commands", who)
		}
		if page.count(t, "#lend-another") != 1 {
			t.Errorf("%s's Mine lacks Lend another Mac", who)
		}
		// What is the Volunteer's own is on their card, whatever else they are.
		if facts := factsOf(t, card); !strings.Contains(facts, "Notices https://ntfy.example/topic-of-"+c.mac) {
			t.Errorf("%s's card lacks their Mac's topic: %s", who, facts)
		}
		// A button pressed there comes back there.
		if w := c.b.post("/my-macs/"+c.mac+"/pause", "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/macs?show=mine#mac-"+c.mac {
			t.Errorf("%s's Pause: %d to %q", who, w.Code, w.Header().Get("Location"))
		}
		w := c.b.post("/my-macs/" + c.mac + "/remove-limit")
		if refused := mainOf(t, w.Body.String()); w.Code != http.StatusConflict || refused.sole(t, "h1").words() != "Macs" || refused.count(t, "#mac-"+c.mac) != 1 {
			t.Errorf("%s's refused button: %d %q", who, w.Code, refusalIn(w.Body.String()))
		}
	}
	// The same card, whoever lends: an External volunteer's has the same parts.
	theirs, hers := linus.myMacs().sole(t, "#mac-linus-air"), grace.mine().sole(t, "#mac-graces-air")
	partsOf := func(card *node, name string) string {
		return strings.ReplaceAll(ids(card.all(t, "[id]")), name, "MAC")
	}
	if a, b := partsOf(theirs, "linus-air"), partsOf(hers, "graces-air"); a != b {
		t.Errorf("the cards differ:\n%s\n%s", a, b)
	}
	if said := ravi.mine().sole(t, "#lend-another").words(); !strings.Contains(said, "no cap") {
		t.Errorf("a Maintainer's count: %q", said)
	}
	// The M team is shown whose Mac it is and no more of what is its Volunteer's.
	if body := grace.page("/macs") + grace.page("/macs/linus-air") + grace.page("/macs?show=mine"); strings.Contains(body, "topic-of-linus-air") || strings.Contains(body, "topic-of-ravis-air") {
		t.Error("an M team member is shown the topic of a Mac they do not lend")
	}
	// All and Leased by me are the fleet's table still.
	if page := mainOf(t, grace.page("/macs")); page.count(t, "section.mac-card") != 0 || page.count(t, "tr#mac-m1-pro") != 1 {
		t.Error("Macs, All, is not the fleet's table")
	}
	// A Mac's own page leads back to where its cards are.
	if own := linus.macPage("linus-air", ""); !strings.Contains(own, `<a href="/my-macs">My Macs</a>`) {
		t.Errorf("a Volunteer's Mac's page: %s", own)
	}
}

// The link in a Notice opens that Mac on My Macs, for whoever its Volunteer
// is, signed in or not yet.
func TestTheLinkInANoticeOpensThatMacOnMyMacs(t *testing.T) {
	l := myMacsLab(t)
	open := l.s.now().Add(2 * time.Hour)
	l.lentMac("graces-air", graceID, api.AccessFull, open)
	l.lentMac("linus-air", linusID, api.AccessQualification, open)
	grace, linus := l.lrMember(graceID), l.signedIn(linusID)
	if got := l.s.myMacLink(labAddress, "linus-air"); got != labAddress+"/my-macs/linus-air" {
		t.Fatalf("the link a Notice carries: %q", got)
	}

	if w := l.browser().do("GET", "/my-macs/linus-air", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/sign-in?return="+url.QueryEscape("/my-macs/linus-air") {
		t.Errorf("signed out: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if w := linus.do("GET", "/my-macs/linus-air", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs#mac-linus-air" {
		t.Errorf("its Volunteer: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if linus.myMacs().count(t, "section#mac-linus-air") != 1 {
		t.Error("the page has no part the link opens")
	}
	if w := grace.do("GET", "/my-macs/graces-air", ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/macs?show=mine#mac-graces-air" {
		t.Errorf("an M team member who lends it: %d to %q", w.Code, w.Header().Get("Location"))
	}
	// A name says nothing of a Mac that is not the reader's.
	for _, name := range []string{"graces-air", "no-such-mac"} {
		if w := linus.do("GET", "/my-macs/"+name, ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs#mac-"+name {
			t.Errorf("another's Mac, %s: %d to %q", name, w.Code, w.Header().Get("Location"))
		}
	}
	if w := linus.do("GET", "/my-macs/Not%20A%20Name", ""); w.Code != http.StatusNotFound {
		t.Errorf("what is no Mac's name: %d", w.Code)
	}
	if w := l.signedIn(nobodyID).do("GET", "/my-macs/linus-air", ""); w.Code != http.StatusNotFound {
		t.Errorf("someone who lends nothing: %d", w.Code)
	}
}

// My Macs says who ended an enrollment: the Mac is no card any more, and
// the page keeps its name, when it left and who ended it, for its Volunteer
// even when it was their last.
func TestMyMacsSaysWhoEndedAnEnrollment(t *testing.T) {
	l := myMacsLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	ravi.did("/people/mark", "handle", "edith", "as", "invite")
	grace, edith := l.signedIn(graceID), l.signedIn(edithID)
	grace.lent("shelf-mac")
	grace.lent("desk-mac")
	edith.lent("ediths-mac")

	if w := grace.post("/my-macs/desk-mac/leave", "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs" {
		t.Fatalf("Leave: %d to %q", w.Code, w.Header().Get("Location"))
	}
	left := l.s.now()
	l.clock.Advance(time.Hour)
	ravi.did(personPath(graceID, "enrollments/shelf-mac/end"), "sure", "yes")
	ravi.did(personPath(edithID, "enrollments/ediths-mac/end"), "sure", "yes")

	// Her last Mac left: she has no role, and the page is still hers.
	if got := l.who(graceID).Role().String(); got != "Signed in, no role" {
		t.Fatalf("grace is %q", got)
	}
	body := grace.page("/my-macs")
	page := mainOf(t, body)
	if page.count(t, "section.mac-card") != 0 {
		t.Error("a Mac that left is still a card")
	}
	rows := texts(page.all(t, "#left-the-lab li"))
	if len(rows) != 2 || !has(rows[:1], "shelf-mac", "a Maintainer ended its enrollment", whenDay(l.s.now())) || !has(rows[1:], "desk-mac", "you left with it", whenDay(left)) {
		t.Errorf("the Macs that left, newest first: %q", rows)
	}
	if page.count(t, "li#mac-shelf-mac") != 1 {
		t.Error("the link of the Notice that an enrollment was ended opens nothing")
	}
	if strings.Contains(body, "ediths-mac") || strings.Contains(body, "@ravi") {
		t.Error("the page names another person's Mac or the Maintainer")
	}
	if said := page.sole(t, "#lend-another").words(); !strings.Contains(said, "Lend a Mac") || !strings.Contains(said, "0 of 5") {
		t.Errorf("with no Mac left: %q", said)
	}
	// The name is free: enrolled again, it is a card, and the one that left stays below it.
	grace.lent("shelf-mac")
	page = grace.myMacs()
	if page.count(t, "section#mac-shelf-mac") != 1 || page.count(t, "#left-the-lab li") != 2 || page.count(t, "li#mac-shelf-mac") != 0 {
		t.Errorf("enrolled again under its name: %q", ids(page.all(t, "[id]")))
	}
}

// Pause and Limit take something away (a running job, a Lease), so they ask
// twice in place: the first click turns the button into what it does, and a
// post without the second changes nothing.
func TestPauseAndLimitAskTwice(t *testing.T) {
	l := myMacsLab(t)
	v := l.lentMac("vol-air", linusID, api.AccessFull, l.s.now().Add(6*time.Hour))
	linus := l.signedIn(linusID)
	controls := linus.myMacs().sole(t, "#controls-vol-air")
	for path, want := range map[string]string{"pause": pauseSentence("vol-air"), "limit": limitSentence("vol-air")} {
		if got := controls.sole(t, `form[action="/my-macs/vol-air/`+path+`"] button`).attr("data-sentence"); got != want {
			t.Errorf("%s turns into %q, want %q", path, got, want)
		}
	}
	until := l.s.now().Add(26 * time.Hour).In(brisbane).Format("2006-01-02T15:04")
	w := linus.post("/my-macs/vol-air/pause", "until", until)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), pauseSentence("vol-air")) || !strings.Contains(w.Body.String(), `value="`+until+`"`) {
		t.Errorf("one click on Pause: %d\n%s", w.Code, w.Body)
	}
	if w := linus.post("/my-macs/vol-air/limit"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), limitSentence("vol-air")) {
		t.Errorf("one click on Limit: %d", w.Code)
	}
	if d := v.snapshot(); d.SitePause != nil || d.Limited {
		t.Fatalf("one click changed the Mac: pause %+v, limited %v", d.SitePause, d.Limited)
	}
	linus.did("/my-macs/vol-air/pause", "until", until, "sure", "yes")
	linus.did("/my-macs/vol-air/limit", "sure", "yes")
	if d := v.snapshot(); d.SitePause == nil || d.SitePause.Until.IsZero() || !d.Limited {
		t.Errorf("the second click: pause %+v, limited %v", d.SitePause, d.Limited)
	}
}

// countedReaderAt counts the bytes read from it.
type countedReaderAt struct {
	r    *bytes.Reader
	read int64
}

func (c *countedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}

// A card shows a few lines of the Mac's Audit log, which root on the Mac can
// make long: they come from a bounded tail of labd's copy, not the whole.
func TestACardReadsABoundedTailOfTheAuditCopy(t *testing.T) {
	var long bytes.Buffer
	for i := 0; long.Len() < 16<<20; i++ {
		b, _ := json.Marshal(api.AuditEntry{Time: time.Unix(int64(i), 0), Action: fmt.Sprintf("boot %d", i), End: int64(i)})
		long.Write(append(b, '\n'))
	}
	r := &countedReaderAt{r: bytes.NewReader(long.Bytes())}
	es, err := auditTail(r, int64(long.Len()), 5)
	if err != nil || len(es) != 5 || es[4].Action != fmt.Sprintf("boot %d", es[4].End) || es[0].End != es[4].End-4 {
		t.Fatalf("the tail: %v %+v", err, es)
	}
	if r.read > auditTailMax {
		t.Errorf("read %d bytes for five lines of a %d byte copy", r.read, long.Len())
	}

	// A copy that starts with a line longer than any read still shows its
	// newest lines.
	l := myMacsLab(t)
	l.lentMac("vol-air", linusID, api.AccessFull, l.s.now().Add(6*time.Hour))
	path := l.s.auditPath("vol-air")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	last, _ := json.Marshal(api.AuditEntry{Time: l.s.now(), Maintainer: "ravi", Action: "the newest line", End: 1})
	junk := append(bytes.Repeat([]byte("x"), 2<<20), '\n')
	if err := os.WriteFile(path, append(junk, append(last, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	card := l.signedIn(linusID).myMacs().sole(t, "#mac-vol-air")
	if !card.says(t, "#audit-vol-air li", "@ravi · the newest line") {
		t.Errorf("the card's Audit log: %q", card.sole(t, "#audit-vol-air").words())
	}
}

// A Mac lent always shows its window as always, open with no end, and the
// command for the window at the Mac says always is an answer.
func TestACardOfAMacLentAlwaysSaysAlways(t *testing.T) {
	l := myMacsLab(t)
	v := l.lentMac("vol-air", linusID, api.AccessQualification, time.Time{})
	always, err := window.ParseRule("always")
	if err != nil {
		t.Fatal(err)
	}
	v.mu.Lock()
	v.d.Window.Rules = []window.Rule{always}
	v.mu.Unlock()
	linus := l.signedIn(linusID)
	card := linus.myMacs().sole(t, "#mac-vol-air")
	if facts := factsOf(t, card); !strings.Contains(facts, "Availability window always (Australia/Brisbane) open always") {
		t.Errorf("the card of an always Mac: %s", facts)
	}
	if !strings.Contains(card.words(), "or window add always") {
		t.Errorf("the card does not say always is a window: %s", card.words())
	}
}
