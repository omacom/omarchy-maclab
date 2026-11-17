package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// Seams invented for #33 (the ticket names none):
//   - `sudo lab-agent leave` asks to confirm; l.volunteer("leave", "--yes")
//     is the Volunteer saying yes. Without --yes (no terminal in the harness)
//     it must refuse and change nothing.
//   - A Maintainer ends a Mac's enrollment with DELETE /api/devices/{name}
//     under their own token. Afterwards the Mac is gone from labd's API and
//     its device secret is rejected with 401 at once.
//   - "Told on both channels": the Mac's ntfy topic gets an "enrollment
//     ended" notice and the Sim shows the Volunteer a desktop notice, both
//     naming the Maintainer.

const leavingMaintainer = "grace"

// leavingLink is a transport that can drop labd off the network.
type leavingLink struct{ down atomic.Bool }

func (k *leavingLink) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k.down.Load() {
			http.Error(w, "labd is unreachable", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// leavingLab is a Volunteer lab with a second Maintainer (grace) and a window open.
func leavingLab(t *testing.T, c labConfig) (*noticeLab, string) {
	t.Helper()
	c.volunteer = true
	n := newNoticeLab(t, c)
	return n, n.addMaintainer(harnessToken, leavingMaintainer).Token
}

// leavingEnd is a Maintainer ending the enrollment of the Mac called name.
func (l *lab) leavingEnd(token, name string) (*http.Response, string) {
	l.t.Helper()
	return l.as(token, "POST", "/api/devices/"+name+"/end-enrollment", nil)
}

func (l *lab) leavingMustEnd(token, name string) {
	l.t.Helper()
	if r, body := l.leavingEnd(token, name); r.StatusCode/100 != 2 {
		l.t.Fatalf("ending %s's enrollment: %s %s", name, r.Status, body)
	}
}

// leavingGone waits up to d for labd to know no Mac called name.
func (l *lab) leavingGone(name string, d time.Duration) bool {
	l.t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(50 * time.Millisecond) {
		if r, _ := l.get("GET", "/api/devices/"+name); r.StatusCode == 404 {
			return true
		}
		if time.Now().After(end) {
			return false
		}
	}
}

func (l *lab) leavingRequireGone(name string) {
	l.t.Helper()
	if !l.leavingGone(name, 15*time.Second) {
		_, body := l.get("GET", "/api/devices/"+name)
		l.t.Fatalf("labd still knows %s: %s", name, body)
	}
}

func (l *lab) leavingStillEnrolled() {
	l.t.Helper()
	time.Sleep(500 * time.Millisecond)
	if r, body := l.get("GET", "/api/devices/"+l.name); r.StatusCode != 200 {
		l.t.Fatalf("%s is no longer enrolled: %s %s", l.name, r.Status, body)
	}
	if r := l.checkinAs(l.secret); r.StatusCode != 200 {
		l.t.Fatalf("the Mac's device secret stopped working: %s", r.Status)
	}
}

// leavingTopic is the path of a Volunteer Mac's own topic from labd's enroll answer.
func (n *noticeLab) leavingTopic(topic string) string {
	topic = strings.TrimPrefix(topic, n.ntfy.URL)
	return "/" + strings.TrimPrefix(topic, "/")
}

func (n *noticeLab) leavingEndedNotices(path string) []ntfyPost {
	var out []ntfyPost
	for _, p := range n.ntfy.at(path) {
		if p.endedEnrollment() {
			out = append(out, p)
		}
	}
	return out
}

// leavingDesktopNotice is whether the Volunteer was shown a desktop notice
// that the Maintainer ended the enrollment.
func leavingDesktopNotice(sim *agent.Sim, maintainer string) bool {
	for _, t := range sim.Notices() {
		low := strings.ToLower(t)
		if strings.Contains(low, "ended") && strings.Contains(low, maintainer) {
			return true
		}
	}
	return false
}

// ---- the Volunteer leaving ----

func TestLeaveAsksToConfirmFirstAndWithoutAnAnswerChangesNothing(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	if out, err := n.volunteer("leave"); err == nil {
		t.Fatalf("leave went ahead with no confirmation: %s", out)
	}
	n.leavingStillEnrolled()
}

func TestLeaveRefusesStrayArgumentsAndChangesNothing(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	for _, args := range [][]string{{"leave", "--yes", "now"}, {"leave", "--bogus"}, {"leave", "--yes", "--yes=maybe"}} {
		if out, err := n.volunteer(args...); err == nil {
			t.Fatalf("lab-agent %s was accepted: %s", strings.Join(args, " "), out)
		}
	}
	n.leavingStillEnrolled()
}

func TestVolunteerLeavingRemovesTheMacFromLabdAndRevokesItsDeviceSecret(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	n.mustVolunteer("leave", "--yes")
	n.leavingRequireGone(n.name)
	if r := n.checkinAs(n.secret); r.StatusCode != 401 {
		t.Fatalf("the device secret of a Mac that left still works: %s", r.Status)
	}
	if _, _, err := n.s.Submit(api.JobSpec{Device: n.name, Kernel: n.artifact("krel=7.2-ok behavior=ok")}); err == nil {
		t.Fatal("a job was accepted for a Mac that left")
	}
}

func TestVolunteerLeavingPrintsWhatIsLeftToRemoveByHand(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	out := strings.ToLower(n.mustVolunteer("leave", "--yes"))
	if strings.Contains(out, "tailscale") || strings.Contains(out, "tailnet") {
		t.Fatalf("leave still names Tailscale, which the lab does not use:\n%s", out)
	}
	if !strings.Contains(out, "lab-agent") || !strings.Contains(out, "package") {
		t.Fatalf("leave does not mention the lab-agent package left to remove:\n%s", out)
	}
}

func TestVolunteerLeavingStopsAJobInProgressIncompleteAndReturnsToTheKnownGoodKernel(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	id, _ := n.longJob("7.2-leaving")
	labBoot, _, _ := n.sim.Identity()
	n.mustVolunteer("leave", "--yes")
	n.backOnItsOwnSystem(labBoot, 8*time.Second, "the Volunteer left during the Lab boot")
	n.expect(n.jobAPI(id, 30*time.Second), api.OutcomeIncomplete)
	n.leavingRequireGone(n.name)
	if k := n.sim.LeavingStagedEntries(); k != 0 {
		t.Fatalf("%d lab boot entries are still staged on the Mac that left", k)
	}
}

func TestVolunteerLeavingEndsALeaseOnTheirMac(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	n.takeLease("connor (homework)", time.Hour)
	n.mustVolunteer("leave", "--yes")
	n.leavingRequireGone(n.name)
	if r, body := n.post("/api/devices/"+n.name+"/lease", api.LeaseRequest{Holder: "connor", TTLSec: 60}); r.StatusCode == 200 {
		t.Fatalf("a lease was granted on a Mac that left: %s", body)
	}
}

func TestVolunteerLeavingWorksAtOnceWithLabdDownAndTellsLabdAtTheNextChance(t *testing.T) {
	var link leavingLink
	n, _ := leavingLab(t, labConfig{wrap: link.wrap})
	link.down.Store(true)
	n.mustVolunteer("leave", "--yes") // immediate: never waits for labd
	link.down.Store(false)
	n.leavingRequireGone(n.name)
	if r := n.checkinAs(n.secret); r.StatusCode != 401 {
		t.Fatalf("the Mac left while labd was down, yet its device secret works: %s", r.Status)
	}
}

func TestVolunteerLeavingMidJobWithLabdDownStillReturnsTheMacToItsOwnSystem(t *testing.T) {
	var link leavingLink
	n, _ := leavingLab(t, labConfig{wrap: link.wrap})
	id, _ := n.longJob("7.2-leaving-dark")
	labBoot, _, _ := n.sim.Identity()
	link.down.Store(true)
	n.mustVolunteer("leave", "--yes")
	n.backOnItsOwnSystem(labBoot, 8*time.Second, "the Volunteer left during the Lab boot with labd down")
	link.down.Store(false)
	n.leavingRequireGone(n.name)
	if j := n.jobAPI(id, 60*time.Second); j.Outcome == api.OutcomePass {
		t.Fatalf("a job its Mac left in the middle of passed: %v", j.Events)
	}
}

func TestMacThatLeftRunsNoMoreLabWorkAndNothingComesBackWithoutANewEnrollment(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	n.mustVolunteer("leave", "--yes")
	n.leavingRequireGone(n.name)
	boot, _, _ := n.sim.Identity()
	time.Sleep(2 * time.Second) // the agent must not check in again and bring the Mac back
	if !n.leavingGone(n.name, time.Second) {
		t.Fatal("the Mac that left is listed again without being enrolled")
	}
	if b, _, _ := n.sim.Identity(); b != boot {
		t.Fatal("the Mac that left rebooted")
	}
}

func TestMacThatLeftCanEnrollAgainOnlyWithANewToken(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	oldSecret := n.secret
	n.mustVolunteer("leave", "--yes")
	n.leavingRequireGone(n.name)
	// the old, spent enrollment token does not work
	if r, _ := n.enrollRaw("enr_test_" + n.name); r.StatusCode == 200 {
		t.Fatal("the spent enrollment token enrolled a Mac that left")
	}
	// the old device secret does not work
	if r := n.checkinAs(oldSecret); r.StatusCode != 401 {
		t.Fatalf("old secret: %s", r.Status)
	}
	// a Maintainer's new token for the same name does
	tok := n.enrollToken(n.name)
	resp, body := n.enrollRaw(tok)
	if resp.StatusCode != 200 {
		t.Fatalf("enrolling again with a new token: %s %s", resp.Status, body)
	}
	if strings.Contains(body, oldSecret) {
		t.Fatal("the new enrollment reuses the old device secret")
	}
	// and the Mac's kind is whatever the new token says: this one is Lab-owned
	if d := n.deviceJSON(n.name); d["kind"] == kindVolunteer {
		t.Fatalf("a Mac that left and enrolled again with a Lab-owned token is still a Volunteer's: %v", d)
	}
}

func TestLabOwnedMacHasNoLeaveAndStaysEnrolled(t *testing.T) {
	n := newNoticeLab(t, labConfig{})
	if out, err := n.volunteer("leave", "--yes"); err == nil {
		t.Fatalf("a Lab-owned Mac left with a Volunteer command: %s", out)
	}
	n.leavingStillEnrolled()
}

// ---- the Maintainers ending an enrollment ----

func TestMaintainerEndingAnEnrollmentRemovesTheMacAndRevokesItsDeviceSecretAtOnce(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	n.leavingMustEnd(grace, n.name)
	// no wait: the secret is revoked on labd the moment the call returns
	if r := n.checkinAs(n.secret); r.StatusCode != 401 {
		t.Fatalf("the device secret of a Mac whose enrollment ended still works: %s", r.Status)
	}
	n.leavingRequireGone(n.name)
}

func TestMaintainerEndingAnEnrollmentOfAMacThatNeverChecksInAgainRevokesItsSecret(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	out := n.enrollMac("lost-mbp", ada, agent.NewSim(t.TempDir()), false)
	secret, _ := out["secret"].(string)
	if r := n.checkinAs(secret); r.StatusCode != 200 {
		t.Fatalf("first check-in of lost-mbp: %s", r.Status)
	}
	n.leavingMustEnd(grace, "lost-mbp") // the Mac is lost: it never comes back
	if r := n.checkinAs(secret); r.StatusCode != 401 {
		t.Fatalf("a lost Mac's device secret works after its enrollment ended: %s", r.Status)
	}
	n.leavingRequireGone("lost-mbp")
}

// The Notice names nobody: who ended it is on My Macs and on the Mac's
// desktop.
func TestVolunteerIsToldOnTheirMacsTopicThatTheEnrollmentEndedWithNoHandle(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	topic := n.macTopic()
	n.leavingMustEnd(grace, n.name)
	if !n.waitPosts(topic, 10*time.Second, anyPost(ntfyPost.endedEnrollment)) {
		t.Fatalf("no 'enrollment ended' notice on the Mac's topic: %+v", n.ntfy.at(topic))
	}
	for _, p := range n.ntfy.at(topic) {
		if strings.Contains(p.text(), leavingMaintainer) || p.body != n.name+": "+endedNotice+"\n/my-macs/"+n.name {
			t.Fatalf("the Notice: %q", p.body)
		}
	}
}

func TestVolunteerIsToldOnTheDesktopWhichMaintainerEndedTheEnrollment(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	n.leavingMustEnd(grace, n.name)
	deadline := time.Now().Add(10 * time.Second)
	for !leavingDesktopNotice(n.sim, leavingMaintainer) {
		if time.Now().After(deadline) {
			t.Fatalf("no desktop notice naming %s: %q", leavingMaintainer, n.sim.Notices())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestVolunteerOfAMacThatNeverReturnsStillGetsTheEnrollmentEndedNoticeOnItsTopic(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	out := n.enrollMac("lost-mbp", ada, agent.NewSim(t.TempDir()), false)
	topic := n.leavingTopic(out["ntfy_topic"].(string))
	if r := n.checkinAs(out["secret"].(string)); r.StatusCode != 200 {
		t.Fatalf("first check-in of lost-mbp: %s", r.Status)
	}
	n.leavingMustEnd(grace, "lost-mbp")
	if !n.waitPosts(topic, 10*time.Second, anyPost(func(p ntfyPost) bool { return p.endedEnrollment() })) {
		t.Fatalf("no 'enrollment ended' notice on lost-mbp's topic %s: %+v", topic, n.ntfy.all())
	}
}

func TestEnrollmentEndedNoticeIsOnlyForTheVolunteersMacTopicNotTheLabWideOne(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	topic := n.macTopic()
	n.leavingMustEnd(grace, n.name)
	n.waitPosts(topic, 10*time.Second, anyPost(ntfyPost.endedEnrollment))
	time.Sleep(500 * time.Millisecond)
	for _, p := range n.ntfy.all() {
		if p.path != topic {
			t.Fatalf("a Volunteer's enrollment-ended notice reached another topic: %+v", p)
		}
	}
	if got := n.leavingEndedNotices(topic); len(got) != 1 {
		t.Fatalf("%d enrollment-ended notices on the Mac's topic, want exactly one: %+v", len(got), n.ntfy.at(topic))
	}
}

func TestMaintainerEndingAnEnrollmentMidJobHasTheMacCleanUpAtItsNextCheckin(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	id, _ := n.longJob("7.2-ended")
	labBoot, _, _ := n.sim.Identity()
	n.leavingMustEnd(grace, n.name)
	n.backOnItsOwnSystem(labBoot, 10*time.Second, "the Maintainers ended the enrollment during the Lab boot")
	if j := n.jobAPI(id, 60*time.Second); j.Outcome == api.OutcomePass {
		t.Fatalf("a job on a Mac whose enrollment ended passed: %v", j.Events)
	}
	if !leavingDesktopNotice(n.sim, leavingMaintainer) {
		// the notice may come just after the cleanup
		time.Sleep(time.Second)
		if !leavingDesktopNotice(n.sim, leavingMaintainer) {
			t.Fatalf("the Volunteer was not told on the desktop: %q", n.sim.Notices())
		}
	}
}

func TestMacWhoseEnrollmentEndedDoesNotComeBackOnItsOwn(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	n.leavingMustEnd(grace, n.name)
	n.leavingRequireGone(n.name)
	time.Sleep(2 * time.Second) // lab-agent keeps checking in until it learns; it must not re-create the Mac
	if !n.leavingGone(n.name, time.Second) {
		t.Fatal("the Mac whose enrollment ended is listed again")
	}
}

func TestMacWhoseEnrollmentEndedCanEnrollAgainOnlyWithANewToken(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	oldSecret := n.secret
	n.leavingMustEnd(grace, n.name)
	n.leavingRequireGone(n.name)
	if r, _ := n.enrollRaw("enr_test_" + n.name); r.StatusCode == 200 {
		t.Fatal("the spent enrollment token enrolled a Mac whose enrollment ended")
	}
	resp, body := n.enrollRaw(n.enrollToken(n.name))
	if resp.StatusCode != 200 || strings.Contains(body, oldSecret) {
		t.Fatalf("enrolling again with a new token: %s %s", resp.Status, body)
	}
}

func TestEndingAnEnrollmentIsRecordedInTheMacsOwnAuditLogNamingTheMaintainer(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	n.leavingMustEnd(grace, n.name)
	deadline := time.Now().Add(10 * time.Second)
	var out string
	for {
		out, _ = n.volunteer("log")
		if l := strings.ToLower(out); strings.Contains(l, leavingMaintainer) && strings.Contains(l, "enrollment") && strings.Contains(l, "end") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("`log` does not show %s ending the enrollment:\n%s", leavingMaintainer, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestAuditLogStillReadsAfterTheVolunteerLeaves(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	n.takeLease("connor (homework)", time.Hour) // a Maintainer action to leave a record
	_ = grace
	before, err := n.volunteer("log")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	n.mustVolunteer("leave", "--yes")
	n.leavingRequireGone(n.name)
	after, err := n.volunteer("log")
	if err != nil {
		t.Fatalf("`log` fails after leave: %v", err)
	}
	if strings.TrimSpace(before) != "" && !strings.Contains(after, strings.Split(strings.TrimSpace(before), "\n")[0]) {
		t.Fatalf("`log` lost its entries after leave:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// ---- bad input ----

func TestEndingTheEnrollmentOfAMacLabdDoesNotKnowIsNotFound(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	if r, body := n.leavingEnd(grace, "no-such-mac"); r.StatusCode != 404 {
		t.Fatalf("ending an unknown Mac: %s %s", r.Status, body)
	}
	n.leavingStillEnrolled()
}

func TestEndingAnEnrollmentTwiceIsNotFoundTheSecondTimeAndNoticesOnlyOnce(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	topic := n.macTopic()
	n.leavingMustEnd(grace, n.name)
	n.leavingRequireGone(n.name)
	if r, body := n.leavingEnd(grace, n.name); r.StatusCode != 404 {
		t.Fatalf("ending an enrollment twice: %s %s", r.Status, body)
	}
	n.waitPosts(topic, 10*time.Second, anyPost(ntfyPost.endedEnrollment))
	time.Sleep(500 * time.Millisecond)
	if got := n.leavingEndedNotices(topic); len(got) != 1 {
		t.Fatalf("%d enrollment-ended notices, want 1: %+v", len(got), got)
	}
}

func TestOnlyAMaintainerCanEndAnEnrollmentNotAMacNorAnUnknownToken(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	other := n.enrollMac("other-mbp", ada, agent.NewSim(t.TempDir()), false)
	for who, tok := range map[string]string{"no token": "", "an unknown token": "nope", "a Mac's own device secret": n.secret, "another Mac's device secret": other["secret"].(string)} {
		for _, name := range []string{n.name, "other-mbp"} {
			if r, body := n.as(tok, "POST", "/api/devices/"+name+"/end-enrollment", nil); r.StatusCode != 401 && r.StatusCode != 403 {
				t.Fatalf("%s ended %s's enrollment: %s %s", who, name, r.Status, body)
			}
		}
	}
	n.leavingStillEnrolled()
	if r, _ := n.get("GET", "/api/devices/other-mbp"); r.StatusCode != 200 {
		t.Fatalf("other-mbp was removed: %s", r.Status)
	}
}

func TestRemovedMaintainersTokenCannotEndAnEnrollment(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	n.removeMaintainer(leavingMaintainer)
	if r, _ := n.leavingEnd(grace, n.name); r.StatusCode != 403 {
		t.Fatalf("a removed Maintainer ended an enrollment: %s", r.Status)
	}
	n.leavingStillEnrolled()
}

func TestMaintainerEndingALabOwnedMacSendsNoVolunteerNotice(t *testing.T) {
	n := newNoticeLab(t, labConfig{})
	n.leavingMustEnd(harnessToken, n.name)
	n.leavingRequireGone(n.name)
	time.Sleep(500 * time.Millisecond)
	for _, p := range n.ntfy.all() {
		if p.endedEnrollment() {
			t.Fatalf("a Lab-owned Mac has no Volunteer, yet %+v was posted", p)
		}
	}
	if r := n.checkinAs(n.secret); r.StatusCode != 401 {
		t.Fatalf("a Lab-owned Mac's secret works after its enrollment ended: %s", r.Status)
	}
}

// ---- leaving Retires the record ----

// retiredNow is the lab's Retired Macs as a Maintainer reads them.
func (l *signInLab) retiredNow(maintainer *browser) []RetiredMac {
	l.t.Helper()
	w := maintainer.do("GET", "/api/retired", "")
	var out []RetiredMac
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		l.t.Fatalf("GET /api/retired: %d %s", w.Code, w.Body)
	}
	return out
}

// kept writes what labd keeps under a Mac's name: a screenshot, its serial
// log, its two baselines and labd's copy of its Audit log; and one job it
// ran. It returns the files, in that order, and the job.
func (l *signInLab) kept(name string) (files []string, job string) {
	l.t.Helper()
	files = []string{filepath.Join(l.s.screenDir(name), "shot.png"), l.s.serialPath(name), l.s.baselinePath(name), l.s.omtBaselinePath(name), l.s.auditPath(name)}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			l.t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("{}\n"), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
	j := &api.Job{ID: "j-kept-" + name, Spec: api.JobSpec{Device: name}, Created: l.clock.Now(), State: api.JobDone, Outcome: api.OutcomePass, Summary: "ok"}
	if err := l.s.store.saveJob(j); err != nil {
		l.t.Fatal(err)
	}
	return files, j.ID
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Leaving, by the Volunteer on My Macs: it asks twice; then the record is
// Retired at once, its name is free and it stops counting toward the cap.
// Screenshots, serial log and baselines are deleted; jobs, the Audit log
// copy and the Mac's record stay for Maintainers. The Mac, which was not
// reaching the lab, learns of it when it next does. The leaving is a line
// of the Mac's record, of the People record and of the Lab group.
func TestLeavingFromMyMacsRetiresTheRecordAtOnce(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	for i := 1; i <= 4; i++ {
		l.addMac(fmt.Sprintf("grace-%d", i), graceID)
	}
	v, secret := grace.lent("shelf-mac")
	id := v.snapshot().ID
	first := v.snapshot().FirstBaseline.Job
	files, job := l.kept("shelf-mac")
	grace.refused("your cap", "/lend/join-command", "model", lendModelID, "name", "one-more")

	// Leave asks twice: the first press changes nothing.
	if w := grace.post("/my-macs/shelf-mac/leave"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "shelf-mac leaves the lab now") || !strings.Contains(w.Body.String(), `name="sure" value="yes"`) {
		t.Fatalf("Leave, pressed once: %d\n%.600s", w.Code, w.Body)
	}
	if l.s.dev("shelf-mac") == nil || len(l.retiredNow(ravi)) != 0 {
		t.Fatal("one press of Leave ended the enrollment")
	}
	if w := grace.post("/my-macs/shelf-mac/leave", "sure", "yes"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs" {
		t.Fatalf("Leave, pressed twice: %d to %q", w.Code, w.Header().Get("Location"))
	}

	// In the lab: out of the fleet, at once.
	if l.s.dev("shelf-mac") != nil {
		t.Fatal("the Mac that left is still in the fleet")
	}
	if got := grace.macs(); slices.Contains(got, "shelf-mac") {
		t.Errorf("its Volunteer still sees the Mac that left: %v", got)
	}
	if n := l.count(`SELECT count(*) FROM devices WHERE name = 'shelf-mac'`); n != 0 {
		t.Errorf("the Mac that left still has %d rows in the fleet's table", n)
	}
	retired := l.retiredNow(ravi)
	if len(retired) != 1 || retired[0].ID != id || retired[0].Name != "shelf-mac" || retired[0].How != leftByVolunteer || retired[0].VolunteerID != graceID || !retired[0].Retired.Equal(l.clock.Now()) {
		t.Fatalf("the Retired record: %+v", retired)
	}
	if d := retired[0].Device; d.Kind != api.KindVolunteer || d.TermsSHA256 != termsSum() || d.Lease != nil {
		t.Errorf("the Retired Mac as it stood: %+v", d)
	}
	var record []string
	for _, e := range retired[0].Record {
		record = append(record, e.Line)
	}
	if !has(record, "Joined as a Volunteer's Mac") || !has(record, "left the lab: its Volunteer ended it") {
		t.Errorf("the Retired Mac's record: %q", record)
	}
	// Retired Macs are for Maintainers.
	if w := grace.do("GET", "/api/retired", ""); w.Code == http.StatusOK {
		t.Errorf("a Volunteer reads the Retired Macs: %d", w.Code)
	}

	// Screenshots, serial log and baselines are deleted; the Audit log copy
	// and the jobs stay.
	for _, f := range files[:4] {
		if exists(f) {
			t.Errorf("%s is kept after the Mac left", strings.TrimPrefix(f, l.dir))
		}
	}
	if exists(files[4]) || !exists(strings.TrimSuffix(files[4], ".jsonl")+"."+id+".jsonl") {
		t.Error("labd's copy of the Audit log is not kept under the enrollment that ended")
	}
	if j, err := l.s.store.job(job); err != nil || j.Outcome != api.OutcomePass {
		t.Errorf("a job the Mac ran is gone with it: %v", err)
	}
	if j, err := l.s.store.job(first); err != nil || j.State != api.JobDone || j.Outcome != api.OutcomeCanceled {
		t.Errorf("the first baseline of a Mac that left: %+v, %v; want canceled, and kept", j, err)
	}
	if w := grace.do("GET", "/api/jobs/"+job, ""); w.Code == http.StatusOK {
		t.Error("the Volunteer still reads the jobs of the Mac that left")
	}

	// The three lines.
	if got := macRecordOf(t, l.s, id); !has(got, "left the lab: its Volunteer ended it") {
		t.Errorf("the Mac's record: %q", got)
	}
	if got := l.labLines(); !has(got, "shelf-mac left the lab: its Volunteer ended it.") {
		t.Errorf("the Lab group's lines:\n%s", strings.Join(got, "\n"))
	}
	if got := l.recorded(); !has(got, "@grace left the lab with shelf-mac") {
		t.Errorf("the People record:\n%s", strings.Join(got, "\n"))
	}

	// The Mac did not hear. When it next reaches the lab it learns its
	// enrollment ended, and by whom: lab-agent removes the lab then.
	w := l.checkin(secret, api.Checkin{})
	if w.Code != http.StatusUnauthorized || w.Header().Get(api.EndedHeader) != "grace" {
		t.Errorf("the Mac's next check-in: %d, ended by %q; want 401 naming who ended it", w.Code, w.Header().Get(api.EndedHeader))
	}

	// It stopped counting toward the cap, and its name is free: the same
	// person lends another Mac under it.
	if lends, err := l.s.lendsOf(l.who(graceID)); err != nil || lends.Count() != 4 {
		t.Errorf("after one of 5 left, the person lends %d (%v)", lends.Count(), err)
	}
	again, _ := grace.lent("shelf-mac")
	if again.snapshot().ID == id {
		t.Error("the name's new Mac is the old enrollment")
	}
	if got := macRecordOf(t, l.s, again.snapshot().ID); len(got) != 1 {
		t.Errorf("the new Mac under a freed name starts with the old one's record: %q", got)
	}
	if got := l.retiredNow(ravi); len(got) != 1 {
		t.Errorf("%d Retired records after the name was taken again, want the one", len(got))
	}
}

// A Maintainer ends an enrollment with `lab remove`, and a Block ends
// every enrollment of the person: each Retires the record the same way,
// with its own line, and the Mac learns who ended it.
func TestAMaintainerAndABlockEndEnrollmentsTheSameWay(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	one, secretOne := grace.lent("shelf-one")
	two, secretTwo := grace.lent("shelf-two")
	three, _ := grace.lent("shelf-three")
	ids := []string{one.snapshot().ID, two.snapshot().ID, three.snapshot().ID}

	// `lab remove shelf-one`.
	if w := ravi.write("POST", "/api/devices/shelf-one/end-enrollment", ""); w.Code != http.StatusOK {
		t.Fatalf("a Maintainer ending an enrollment: %d %s", w.Code, w.Body)
	}
	if l.s.dev("shelf-one") != nil || l.s.dev("shelf-two") == nil {
		t.Fatal("lab remove did not take out the one Mac it named")
	}
	if got := l.labLines(); !has(got, "shelf-one left the lab: a Maintainer ended it.") {
		t.Errorf("the Lab group's lines:\n%s", strings.Join(got, "\n"))
	}
	if got := l.recorded(); !has(got, "@ravi ended the enrollment of shelf-one, a Mac of @grace") {
		t.Errorf("the People record:\n%s", strings.Join(got, "\n"))
	}
	if got := macRecordOf(t, l.s, ids[0]); !has(got, "left the lab: a Maintainer ended it") {
		t.Errorf("the Mac's record: %q", got)
	}
	if w := l.checkin(secretOne, api.Checkin{}); w.Code != http.StatusUnauthorized || w.Header().Get(api.EndedHeader) != "ravi" {
		t.Errorf("the ended Mac's next check-in: %d, by %q", w.Code, w.Header().Get(api.EndedHeader))
	}
	if w := ravi.write("POST", "/api/devices/shelf-one/end-enrollment", ""); w.Code != http.StatusNotFound {
		t.Errorf("ending it twice: %d", w.Code)
	}

	// "End enrollment" on People does the same, asking twice.
	four, _ := grace.lent("shelf-four")
	end := personPath(graceID, "enrollments/shelf-four/end")
	if page := ravi.do("GET", "/people?open=8", "").Body.String(); !strings.Contains(page, `action="`+end+`"`) || !strings.Contains(page, "End enrollment") {
		t.Error("People has no End enrollment beside the person's Mac")
	}
	if w := ravi.post(end); w.Code != http.StatusOK || l.s.dev("shelf-four") == nil {
		t.Fatalf("End enrollment, pressed once: %d", w.Code)
	}
	ravi.did(end, "sure", "yes")
	if l.s.dev("shelf-four") != nil {
		t.Fatal("End enrollment on People left the Mac in the fleet")
	}
	if got := macRecordOf(t, l.s, four.snapshot().ID); !has(got, "left the lab: a Maintainer ended it") {
		t.Errorf("the Mac's record: %q", got)
	}
	ravi.refused("lends no Mac called shelf-two", personPath(maintainerID, "enrollments/shelf-two/end"), "sure", "yes")
	if w := grace.post(personPath(graceID, "enrollments/shelf-two/end"), "sure", "yes"); w.Code != http.StatusNotFound || l.s.dev("shelf-two") == nil {
		t.Errorf("a Volunteer pressing People's End enrollment: %d", w.Code)
	}

	// Blocked: every Mac the person lends leaves.
	ravi.did(personPath(graceID, "block"), "reason", "floods the group", "sure", "yes")
	if l.s.dev("shelf-two") != nil || l.s.dev("shelf-three") != nil {
		t.Fatal("a Blocked person's Macs are still in the fleet")
	}
	for i, name := range []string{"shelf-two", "shelf-three"} {
		if got := l.labLines(); !has(got, name+" left the lab: its Volunteer was Blocked.") {
			t.Errorf("the Lab group's lines lack %s:\n%s", name, strings.Join(got, "\n"))
		}
		if got := macRecordOf(t, l.s, ids[i+1]); !has(got, "left the lab: its Volunteer was Blocked") {
			t.Errorf("%s's record: %q", name, got)
		}
	}
	if got := l.recorded(); !has(got, "@ravi blocked its Volunteer, which ended the enrollment of shelf-two, a Mac of @grace") {
		t.Errorf("the People record:\n%s", strings.Join(got, "\n"))
	}
	if w := l.checkin(secretTwo, api.Checkin{}); w.Code != http.StatusUnauthorized || w.Header().Get(api.EndedHeader) != "ravi" {
		t.Errorf("a Blocked person's Mac at its next check-in: %d, by %q", w.Code, w.Header().Get(api.EndedHeader))
	}
	how := map[string]string{}
	for _, m := range l.retiredNow(ravi) {
		how[m.Name] = m.How
	}
	if len(how) != 4 || how["shelf-one"] != leftByMaintainer || how["shelf-four"] != leftByMaintainer || how["shelf-two"] != leftBlocked || how["shelf-three"] != leftBlocked {
		t.Errorf("the Retired records: %v", how)
	}
	for _, line := range l.labLines() {
		if strings.Contains(line, "left the lab") && strings.Contains(line, "grace") {
			t.Errorf("a line about a Mac names its Volunteer: %q", line)
		}
	}
}

// A Mac that is not reaching the lab when its Volunteer ends it from the
// site finishes leaving when it next does: any job stops, the Mac is on its
// own system, the lab is removed from it. What stays is the Volunteer's:
// lab-agent, the Audit log, the window and the Access level.
func TestAMacEndedFromTheSiteFinishesLeavingWhenItNextReachesTheLab(t *testing.T) {
	link := &leavingLink{}
	n, _ := leavingLab(t, labConfig{lender: veraID, wrap: link.wrap})
	v := n.s.dev(n.name)
	n.mustVolunteer("window", "add", "mon-fri", "19:00-07:00")
	n.mustVolunteer("access", "full")
	n.waitDevice(func(d api.Device) bool { return d.AccessLevel == api.AccessFull })
	id, _ := n.longJob("7.2-site-leave")
	labBoot, _, _ := n.sim.Identity()

	link.down.Store(true)
	if err := n.s.leaveMac(v, n.vera()); err != nil {
		t.Fatal(err)
	}
	// In the lab it is done at once, with the Mac out of reach.
	if n.s.dev(n.name) != nil {
		t.Fatal("the Mac is still in the fleet")
	}
	time.Sleep(time.Second)
	if leavingDesktopNotice(n.sim, "vera") {
		t.Fatal("the Mac left before it could have heard")
	}
	link.down.Store(false)

	n.backOnItsOwnSystem(labBoot, 20*time.Second, "its Volunteer left from the site during the Lab boot")
	for deadline := time.Now().Add(15 * time.Second); !leavingDesktopNotice(n.sim, "vera"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the Mac never learned its enrollment ended once it reached the lab again: %q", n.sim.Notices())
		}
	}
	if k := n.sim.LeavingStagedEntries(); k != 0 {
		t.Errorf("%d lab boot entries are still staged on the Mac that left", k)
	}
	if j := n.jobAPI(id, 60*time.Second); j.Outcome != api.OutcomeIncomplete {
		t.Errorf("the job running when the Mac left ended %s: %s", j.Outcome, j.Summary)
	}
	// The Volunteer's own stay on the Mac.
	for _, f := range []string{"audit.log", "availability.json", "access.json"} {
		if !exists(filepath.Join(n.agent.WorkDir, f)) {
			t.Errorf("%s did not stay on the Mac that left", f)
		}
	}
	if out := n.mustVolunteer("window", "show"); !strings.Contains(out, "mon-fri 19:00-07:00") {
		t.Errorf("the Volunteer's window did not stay:\n%s", out)
	}
	if out := n.mustVolunteer("log"); !strings.Contains(out, "ended this Mac's enrollment in the lab") {
		t.Errorf("the Audit log does not record the ending:\n%s", out)
	}
}

// Leaving at the Mac says what stays and names the one command that
// removes it.
func TestLeavingNamesWhatStaysAndTheOneCommandThatRemovesIt(t *testing.T) {
	n, _ := leavingLab(t, labConfig{lender: veraID})
	id := n.s.dev(n.name).snapshot().ID
	out := n.mustVolunteer("leave", "--yes")
	for _, want := range []string{"lab-agent", "Audit log", "Availability window", "Access level", "`sudo lab-agent uninstall`"} {
		if !strings.Contains(out, want) {
			t.Errorf("leave does not name %q:\n%s", want, out)
		}
	}
	n.leavingRequireGone(n.name)
	// Told by the Mac itself, the lab keeps no word for it to come back to.
	if got := labLinesOf(t, n.s); !has(got, n.name+" left the lab: its Volunteer ended it.") {
		t.Errorf("the Lab group's lines:\n%s", strings.Join(got, "\n"))
	}
	if got := macRecordOf(t, n.s, id); !has(got, "left the lab: its Volunteer ended it") {
		t.Errorf("the Mac's record: %q", got)
	}
	var how string
	if err := n.s.store.db.QueryRow(`SELECT how FROM retired_macs WHERE id = ?`, id).Scan(&how); err != nil || how != leftByVolunteer {
		t.Errorf("the Retired record: %q, %v", how, err)
	}
	if r := n.checkinAs(n.secret); r.StatusCode != 401 || r.Header.Get(api.EndedHeader) != "" {
		t.Errorf("the secret of a Mac that left by itself: %s, ended by %q", r.Status, r.Header.Get(api.EndedHeader))
	}
}

// A Mac that Joined, opened no window and left: the first baseline that
// waited for a window ends with the enrollment, nothing of it is left
// waiting in labd, and no drain waits for it.
func TestLeavingEndsAJobThatWaitedForAWindow(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	v, secret := grace.lent("shelf-mac")
	first := v.snapshot().FirstBaseline.Job
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification})
	jobSaid(t, l.s, first, "no Availability window")

	if w := grace.post("/my-macs/shelf-mac/leave", "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("Leave: %d %s", w.Code, w.Body)
	}
	waiting := func() bool {
		l.s.mu.Lock()
		defer l.s.mu.Unlock()
		return l.s.cancels[first] != nil
	}
	for deadline := time.Now().Add(10 * time.Second); waiting(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the job that waited for a window still waits after its Mac left")
		}
	}
	if w := opsOf(l.s, "POST", "/drain"); w.Body.String() != "0 0\n" {
		t.Errorf("a drain after the Mac left: %q", w.Body)
	}
	if j, _ := l.s.store.job(first); j.State != api.JobDone || j.Outcome != api.OutcomeCanceled || !strings.Contains(j.Summary, "its Volunteer left the lab") {
		t.Errorf("the job that waited ended %s %s: %s", j.State, j.Outcome, j.Summary)
	}
}

// A leaving whose retirement cannot be written (a full disk) changes
// nothing: the Volunteer gets an error, and the Mac stays in the lab with
// its secret and its files. Leaving again, once it can be written, works.
func TestALeavingThatCannotBeWrittenChangesNothing(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	v, secret := grace.lent("shelf-mac")
	files, _ := l.kept("shelf-mac")
	if _, err := l.s.store.db.Exec(`CREATE TRIGGER full_disk BEFORE INSERT ON retired_macs BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}

	if w := grace.post("/my-macs/shelf-mac/leave", "sure", "yes"); w.Code != http.StatusInternalServerError {
		t.Fatalf("a leaving that could not be written answers %d", w.Code)
	}
	if l.s.dev("shelf-mac") != v {
		t.Fatal("the Mac is gone from the lab")
	}
	select {
	case <-v.stop:
		t.Fatal("the Mac's enrollment was ended")
	default:
	}
	for _, f := range files[:4] {
		if !exists(f) {
			t.Errorf("%s was deleted", f)
		}
	}
	if r := l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification}); r.Code != http.StatusOK {
		t.Errorf("the Mac's secret after the failed leaving: %d %s", r.Code, r.Body)
	}

	if _, err := l.s.store.db.Exec(`DROP TRIGGER full_disk`); err != nil {
		t.Fatal(err)
	}
	if w := grace.post("/my-macs/shelf-mac/leave", "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("leaving again: %d", w.Code)
	}
	if l.s.dev("shelf-mac") != nil {
		t.Error("the Mac is still in the lab after it left")
	}
}

// A Mac ended while it was out of reach still learns it was ended when the
// Maintainer who ended it has been Forgotten since: the answer says "a
// Maintainer", never nothing, and the Mac leaves.
func TestAMacEndedByAForgottenMaintainerStillLeavesAtItsNextCheckin(t *testing.T) {
	l, _ := lendLab(t)
	owner, ravi := l.signedIn(ownerID), l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	_, secret := grace.lent("shelf-mac")
	if w := ravi.write("POST", "/api/devices/shelf-mac/end-enrollment", ""); w.Code != http.StatusOK {
		t.Fatalf("a Maintainer ending an enrollment: %d %s", w.Code, w.Body)
	}
	owner.did(personPath(maintainerID, "forget"), "sure", "yes")

	w := l.checkin(secret, api.Checkin{})
	if w.Code != http.StatusUnauthorized || w.Header().Get(api.EndedHeader) != "a Maintainer" {
		t.Errorf("the ended Mac's next check-in: %d, by %q", w.Code, w.Header().Get(api.EndedHeader))
	}
	if strings.Contains(w.Body.String(), "ravi") {
		t.Errorf("the answer names the forgotten Maintainer: %s", w.Body)
	}
}

// labd's own log, which a forgetting never reaches, names nobody for a
// build queued or an enrollment ended: the records do, and lose the name.
func TestTheLogNamesNobodyForABuildOrAnEnding(t *testing.T) {
	l, logs := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	grace.lent("shelf-mac")
	if _, err := l.s.queueBuild(&api.Build{ID: "b-41", Key: "k-41", State: api.BuildQueued, Source: api.Source{Repo: "https://github.com/labtest/linux", Ref: "main", SHA: strings.Repeat("e", 40)}, AskedBy: "grace", AskedByID: graceID, Created: l.clock.Now()}, true); err != nil {
		t.Fatal(err)
	}
	if w := ravi.write("POST", "/api/devices/shelf-mac/end-enrollment", ""); w.Code != http.StatusOK {
		t.Fatalf("a Maintainer ending an enrollment: %d %s", w.Code, w.Body)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if (strings.Contains(line, "b-41") || strings.Contains(line, "shelf-mac")) && (strings.Contains(line, "grace") || strings.Contains(line, "ravi")) {
			t.Errorf("the log names a person: %s", line)
		}
	}
}

// Who ended a Mac's enrollment is written with its retirement, or neither
// is: a Mac whose ending could not be recorded stays enrolled, never
// "unknown" to the lab with nothing to tell it why.
func TestAnEndingWhoseRecordCannotBeWrittenLeavesTheMacEnrolled(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	v, secret := grace.lent("shelf-mac")
	if _, err := l.s.store.db.Exec(`CREATE TRIGGER full_disk BEFORE INSERT ON ended BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	if w := ravi.write("POST", "/api/devices/shelf-mac/end-enrollment", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("an ending that could not be recorded answers %d %s", w.Code, w.Body)
	}
	if l.s.dev("shelf-mac") != v {
		t.Fatal("the Mac is gone from the lab")
	}
	if r := l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification}); r.Code != http.StatusOK {
		t.Errorf("the Mac's secret after the failed ending: %d %s", r.Code, r.Body)
	}
	if _, err := l.s.store.db.Exec(`DROP TRIGGER full_disk`); err != nil {
		t.Fatal(err)
	}
	if w := ravi.write("POST", "/api/devices/shelf-mac/end-enrollment", ""); w.Code != http.StatusOK {
		t.Fatalf("ending it again: %d %s", w.Code, w.Body)
	}
	if w := l.checkin(secret, api.Checkin{}); w.Code != http.StatusUnauthorized || w.Header().Get(api.EndedHeader) != "ravi" {
		t.Errorf("the ended Mac's next check-in: %d, by %q", w.Code, w.Header().Get(api.EndedHeader))
	}
}

// A Block or a Forget whose Mac cannot leave says so instead of done: the
// Mac's secret would still work. A Forget changes nothing then, and can be
// done again.
func TestABlockOrAForgetWhoseMacCannotLeaveFails(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	v, _ := grace.lent("shelf-mac")
	if _, err := l.s.store.db.Exec(`CREATE TRIGGER full_disk BEFORE INSERT ON retired_macs BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}

	if w := ravi.post(personPath(graceID, "block"), "reason", "spam", "sure", "yes"); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "could not leave the lab") {
		t.Errorf("a Block whose Mac could not leave: %d\n%s", w.Code, refusalIn(w.Body.String()))
	}
	if l.s.dev("shelf-mac") != v || !l.who(graceID).Blocked {
		t.Fatalf("after the Block: the Mac %v, Blocked %v", l.s.dev("shelf-mac") != nil, l.who(graceID).Blocked)
	}
	if w := ravi.post(personPath(graceID, "forget"), "sure", "yes"); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "Nothing is forgotten yet") {
		t.Errorf("a Forget whose Mac could not leave: %d\n%s", w.Code, refusalIn(w.Body.String()))
	}
	if l.who(graceID).Handle != "grace" {
		t.Fatal("the person is forgotten though her Mac is still enrolled")
	}

	if _, err := l.s.store.db.Exec(`DROP TRIGGER full_disk`); err != nil {
		t.Fatal(err)
	}
	ravi.did(personPath(graceID, "forget"), "sure", "yes")
	if l.s.dev("shelf-mac") != nil || !l.who(graceID).Blocked {
		t.Errorf("forgetting again: the Mac %v, Blocked %v", l.s.dev("shelf-mac") != nil, l.who(graceID).Blocked)
	}
}

// A Forget leaves no Mac of the person enrolled: not one whose retirement
// was under way and then failed, nor one that told the lab it is leaving
// and waits for `lab remove`. Each is found in the store by account id.
func TestAForgetEndsAMacMidRetirementOrLeftAsWell(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	retiring, left := l.addMac("edith-air", edithID), l.addMac("edith-pro", edithID)
	retiring.mu.Lock()
	retiring.forgotten = true // another ending has it, and will fail
	retiring.mu.Unlock()
	left.mu.Lock()
	left.d.State = api.StateLeft
	left.saveLocked()
	left.mu.Unlock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		retiring.mu.Lock()
		retiring.forgotten = false
		retiring.mu.Unlock()
	}()

	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	for _, name := range []string{"edith-air", "edith-pro"} {
		if l.s.dev(name) != nil || l.count(`SELECT count(*) FROM devices WHERE name = ?`, name) != 0 {
			t.Errorf("%s is still enrolled after its Volunteer was forgotten", name)
		}
	}
}

// A retirement still under way when a Forget has waited long enough fails
// the Forget, which can be done again.
func TestAForgetWhoseMacIsStillLeavingFails(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	v := l.addMac("edith-air", edithID)
	v.mu.Lock()
	v.forgotten = true
	v.mu.Unlock()
	saved := retiringWait
	retiringWait = 200 * time.Millisecond
	t.Cleanup(func() { retiringWait = saved })

	if w := ravi.post(personPath(edithID, "forget"), "sure", "yes"); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "Nothing is forgotten yet") {
		t.Errorf("a Forget whose Mac is still leaving: %d\n%s", w.Code, refusalIn(w.Body.String()))
	}
	if !l.known(edithID) {
		t.Fatal("the person is forgotten though her Mac is still enrolled")
	}
}

// retiredAudit is labd's copy of a retired Mac's Audit log, renamed after
// its enrollment.
func retiredAudit(t *testing.T, s *Server, v *dev) []api.AuditEntry {
	t.Helper()
	d := v.snapshot()
	b, err := os.ReadFile(strings.TrimSuffix(s.auditPath(d.Name), ".jsonl") + "." + d.ID + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var out []api.AuditEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e api.AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// A report copying a Mac's Audit entries while the Mac is retired keeps
// them: the retirement renames labd's copy once the entries are in, and an
// entry that reaches the retired Mac's copy afterwards goes to the renamed
// one, not to a new file under the name the next Mac may take.
func TestAnAuditEntryInFlightAtRetirementIsKept(t *testing.T) {
	l := rolesLab(t, nil)
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Action: "screenshot", End: 40}})
	by := Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}

	// The Mac's report is being copied when the Maintainer ends it.
	mini.auditMu.Lock()
	done := make(chan error, 1)
	go func() { done <- l.s.endEnrollment(mini, leaving{how: leftByMaintainer, by: by}) }()
	select {
	case err := <-done:
		done <- err
	case <-time.After(time.Second):
	}
	line, _ := json.Marshal(api.AuditEntry{Time: l.clock.Now(), Action: "reboot", End: 80})
	f, err := os.OpenFile(l.s.auditPath("ravi-mini"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(line, '\n'))
	f.Close()
	mini.auditHave = 80
	mini.auditMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// One more report the retired Mac's check-in carried.
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Action: "kernel errors baseline", End: 120}})

	var actions []string
	for _, e := range retiredAudit(t, l.s, mini) {
		actions = append(actions, e.Action)
	}
	if want := []string{"screenshot", "reboot", "kernel errors baseline"}; !slices.Equal(actions, want) {
		t.Errorf("the retired Mac's Audit log copy: %q, want %q", actions, want)
	}
	if _, err := os.Stat(l.s.auditPath("ravi-mini")); !os.IsNotExist(err) {
		t.Errorf("a copy is left under the Mac's name, free for the next Mac: %v", err)
	}
}

// A forgetting while a retired Mac's last report copies its Audit entries
// waits for the copy, as it does for an enrolled Mac's: the entry is kept,
// without the handle.
func TestAForgettingWaitsForARetiredMacsAuditCopy(t *testing.T) {
	l := rolesLab(t, nil)
	l.signedIn(edithID)
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40}})
	if err := l.s.endEnrollment(mini, leaving{how: leftByMaintainer, by: Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}}); err != nil {
		t.Fatal(err)
	}
	k, err := l.s.store.knownPerson(edithID)
	if err != nil {
		t.Fatal(err)
	}

	l.s.auditWrite.RLock()
	mini.auditMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- l.s.forgetHeld(Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}, k, newForgetting(edithID, k.Handle))
	}()
	select {
	case err := <-done:
		done <- err
	case <-time.After(time.Second):
	}
	d := mini.snapshot()
	line, _ := json.Marshal(api.AuditEntry{Time: l.clock.Now(), Maintainer: "edith", AccountID: edithID, Action: "reboot", End: 80})
	f, err := os.OpenFile(strings.TrimSuffix(l.s.auditPath(d.Name), ".jsonl")+"."+d.ID+".jsonl", os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(line, '\n'))
	f.Close()
	mini.auditHave = 80
	mini.auditMu.Unlock()
	l.s.auditWrite.RUnlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	es := retiredAudit(t, l.s, mini)
	if len(es) != 2 || es[1].Action != "reboot" {
		t.Fatalf("the retired Mac's Audit log copy: %+v", es)
	}
	for _, e := range es {
		if e.Maintainer != "" || e.AccountID != 0 {
			t.Errorf("an entry still names her: %+v", e)
		}
	}
}

// The Maintainer who ended a Mac's enrollment during an Installed run, and
// was Forgotten before the run was sealed, is named in neither copy of the
// run's record: the seal's own summary and last event say who ended it.
func TestAnInstalledRunSealedAfterItsEnderIsForgottenNamesNobody(t *testing.T) {
	l := macLab(t, nil)
	l.signedIn(edithID)
	l.lrMac("m1", "j293", nil)
	l.pjJob("j-41", "m1", maintainerID, "ravi", l.clock.Now(), func(j *api.Job) { j.State, j.Outcome, j.Summary = api.JobTesting, "", "" })
	j, err := l.s.store.job("j-41")
	if err != nil {
		t.Fatal(err)
	}
	end, err := l.s.forgetInJobs(newForgetting(edithID, "edith"))
	if err != nil {
		t.Fatal(err)
	}
	end()

	ir := &installedRun{r: &jobRun{s: l.s, v: l.s.dev("m1"), j: j},
		c: &installedCtx{release: "9.9", dir: l.s.frozenDir("9.9")}, res: api.InstalledResult{Release: "9.9"}}
	gone := leaving{how: leftByMaintainer, by: Person{AccountID: edithID, Handle: "edith", Maintainer: true}}
	outcome, summary := ir.seal(api.OutcomeCanceled, "canceled: "+gone.reason(), l.clock.Now())
	if outcome != api.OutcomeCanceled {
		t.Fatalf("sealed %s: %s", outcome, summary)
	}
	byName := regexp.MustCompile(`(?i)(^|[^a-z0-9-])edith($|[^a-z0-9-])`)
	for _, p := range []string{filepath.Join(ir.evDir(), "job.json"), filepath.Join(ir.c.dir, "installed", "m1", "j-41", "job.json")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if byName.Match(b) {
			rel, _ := filepath.Rel(l.s.cfg.DataDir, p)
			t.Errorf("%s still names the Maintainer who ended the enrollment:\n%s", rel, b)
		}
	}
}
