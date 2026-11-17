package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// A change of terms (spec 14, decision 7): from the moment new terms take
// effect, each Mac whose Volunteer has not accepted them takes nothing from
// the lab, as when Paused: no job, no Lease. No deadline, never an automatic
// Leave. The Volunteer accepts on My Macs, once for all their Macs, recorded
// as the first acceptance was; they are told on My Macs and by one Notice.
// The M team and the Maintainers accept nothing.

// newTerms are terms other than the shipped ones, as a Deploy brings.
var newTerms = web.Text{Body: "Omarchy Mac Lab: the terms for a Volunteer's Mac\n2026-12-01\n\n1. Test machines only.\n", Date: "2026-12-01",
	SHA256: "1111111111111111111111111111111111111111111111111111111111111111"}

// termsLab is a lab with two Macs lent by Linus, who is External, and one
// lent by Grace, on the M team, each accepted under the shipped terms and
// with its own topic on a fake notice server.
func termsLab(t *testing.T) (*signInLab, *ntfyCatcher) {
	t.Helper()
	ntfy := newNtfyCatcher(t)
	l := myMacsLab(t)
	l.s.cfg.NtfyURL = ntfy.URL
	l.lrMember(graceID)
	for _, m := range []struct {
		name string
		by   int64
	}{{"vol-air", linusID}, {"vol-pro", linusID}, {"team-air", graceID}} {
		v := l.lentMac(m.name, m.by, api.AccessQualification, l.s.now().Add(6*time.Hour))
		v.mu.Lock()
		v.d.TermsSHA256, v.d.TermsAccepted = web.Terms().SHA256, l.s.now().Add(-24*time.Hour)
		v.d.Enrollment.NtfyTopic = m.name + "-topic"
		v.saveLocked()
		v.mu.Unlock()
	}
	return l, ntfy
}

// deploy has the lab start on newTerms.
func deploy(l *signInLab) {
	l.s.shippedTerms.Store(&newTerms)
	l.s.termsTookEffect()
}

func termsNotices(ps []ntfyPost) []ntfyPost {
	var out []ntfyPost
	for _, p := range ps {
		if strings.Contains(p.body, termsNotice) {
			out = append(out, p)
		}
	}
	return out
}

func TestChangedTermsHoldAVolunteersMacsUntilTheyAcceptOnMyMacs(t *testing.T) {
	l, ntfy := termsLab(t)
	ravi, grace, linus := l.signedIn(maintainerID), l.signedIn(graceID), l.signedIn(linusID)
	lease := grace.mustLease("vol-air", "grace@desk", time.Hour)
	if page := linus.myMacs(); page.count(t, "#terms-changed") != 0 {
		t.Fatal("the banner shows before the terms changed")
	}

	deploy(l)
	l.s.termsTookEffect() // a second start tells nobody again

	// Told by one Notice for each Mac, never for the M team's.
	time.Sleep(300 * time.Millisecond)
	for _, mac := range []string{"vol-air", "vol-pro"} {
		got := termsNotices(ntfy.at("/" + mac + "-topic"))
		if len(got) != 1 || got[0].title != noticeTitle || !strings.HasPrefix(got[0].body, mac+": "+termsNotice) || !strings.Contains(got[0].body, "/my-macs/"+mac) {
			t.Errorf("%s's topic got %+v, want the one terms Notice with its link", mac, ntfy.at("/"+mac+"-topic"))
		}
	}
	if got := ntfy.at("/team-air-topic"); len(got) != 0 {
		t.Errorf("an M team member's Mac was told of the terms: %+v", got)
	}

	// No job and no Lease, in the one line lab shows; the Lease held ended.
	line := termsWaitingLine("vol-air") + "\n"
	job, _ := json.Marshal(api.JobSpec{Device: "vol-air", Baseline: true})
	if w := ravi.write("POST", "/api/jobs", string(job), "Content-Type", "application/json"); w.Code != http.StatusConflict || w.Body.String() != line {
		t.Errorf("a job on a Mac waiting for the terms: %d %q, want 409 %q", w.Code, w.Body.String(), line)
	}
	if code, _, body := grace.lease("vol-air", "grace@desk", time.Hour); code != http.StatusConflict || body != line {
		t.Errorf("a Lease on a Mac waiting for the terms: %d %q", code, body)
	}
	if v := l.s.dev("vol-air"); v.snapshot().Lease != nil || !l.s.view(v).TermsWaiting {
		t.Errorf("after the change: Lease %+v (was %+v), waiting %v", v.snapshot().Lease, lease, l.s.view(v).TermsWaiting)
	}
	if code, _, body := ravi.lease("team-air", "ravi@desk", time.Hour); code != http.StatusOK {
		t.Errorf("the M team's Mac is held for terms: %d %s", code, body)
	}

	// No deadline, no Leave: a day on, both Macs are still enrolled.
	for _, mac := range []string{"vol-air", "vol-pro"} {
		if v := l.s.dev(mac); v == nil || v.snapshot().State == api.StateLeft {
			t.Fatalf("%s left the lab over the terms", mac)
		}
	}

	// My Macs: the banner with Accept, for Linus alone.
	banner := linus.myMacs().sole(t, "#terms-changed")
	if !strings.Contains(banner.words(), "2026-12-01") || banner.sole(t, "form").attr("action") != "/my-macs/accept-terms" ||
		banner.sole(t, `form input[name=csrf_token]`).attr("value") == "" {
		t.Fatalf("the banner: %q", banner.words())
	}
	if n := grace.mine().count(t, "#terms-changed"); n != 0 {
		t.Error("an M team member is asked to accept the terms")
	}

	// Accepting, once for both Macs, is recorded as at Join.
	before := l.s.now()
	w := linus.post("/my-macs/accept-terms")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/my-macs" {
		t.Fatalf("Accept: %d %s", w.Code, w.Body)
	}
	for _, mac := range []string{"vol-air", "vol-pro"} {
		v := l.s.dev(mac)
		row := storedDevice(t, l.s, mac)
		if row.TermsSHA256 != newTerms.SHA256 || row.TermsAccepted.Before(before) || row.VolunteerID != linusID || l.s.view(v).TermsWaiting {
			t.Errorf("%s after Accept: sha %s at %s by %d, waiting %v", mac, row.TermsSHA256, row.TermsAccepted, row.VolunteerID, l.s.view(v).TermsWaiting)
		}
		if rec := macRecordOf(t, l.s, row.ID); !strings.Contains(strings.Join(rec, "\n"), "its Volunteer accepted the terms of 2026-12-01 on My Macs (sha256 "+newTerms.SHA256+")") {
			t.Errorf("%s's record: %q", mac, rec)
		}
	}
	if linus.myMacs().count(t, "#terms-changed") != 0 {
		t.Error("the banner stays after Accept")
	}
	if code, _, body := grace.lease("vol-air", "grace@desk", time.Hour); code != http.StatusOK {
		t.Errorf("a Lease after Accept: %d %s", code, body)
	}
	linus.refused("None of your Macs is waiting", "/my-macs/accept-terms")
	time.Sleep(200 * time.Millisecond)
	if got := termsNotices(ntfy.all()); len(got) != 2 {
		t.Errorf("%d terms Notices in all, want 2: %+v", len(got), got)
	}
}

// A Mac that waits holds its queued job as a Pause does, and runs it once
// its Volunteer accepts.
func TestAJobQueuedOnAMacWaitingForTheTermsIsHeldWithTheOneLine(t *testing.T) {
	l, _ := termsLab(t)
	v := l.s.dev("vol-air")
	v.mu.Lock()
	v.termsWait = true
	hold := v.windowHoldLocked(time.Minute)
	v.mu.Unlock()
	if hold != termsWaitingLine("vol-air") {
		t.Fatalf("a queued job waits with %q", hold)
	}
}

func TestAMaintainersMacAndAnMTeamMembersAreAskedToAcceptNothing(t *testing.T) {
	l, _ := termsLab(t)
	ravi := l.signedIn(maintainerID)
	l.lentMac("ravi-air", maintainerID, api.AccessQualification, l.s.now().Add(time.Hour))
	deploy(l)
	for _, mac := range []string{"ravi-air", "team-air"} {
		if l.s.view(l.s.dev(mac)).TermsWaiting {
			t.Errorf("%s waits for terms", mac)
		}
	}
	if l.s.changedTermsFor(Person{AccountID: maintainerID}) != nil || l.s.changedTermsFor(Person{AccountID: graceID}) != nil {
		t.Error("the M team or a Maintainer is shown the banner")
	}
	if n := ravi.mine().count(t, "#terms-changed"); n != 0 {
		t.Error("a Maintainer is asked to accept the terms")
	}
	if l.s.changedTermsFor(Person{AccountID: linusID}) == nil {
		t.Error("the External Volunteer is not asked")
	}
}

// lab-agent status says changed terms wait on My Macs.
func TestTheMacsOwnEnrollmentSaysChangedTermsAreWaiting(t *testing.T) {
	l, _ := termsLab(t)
	deploy(l)
	v := l.s.dev("vol-air")
	w := l.asMac("GET", "/api/agent/enrollment", "dev_vol-air", "")
	var ae api.AgentEnrollment
	if json.Unmarshal(w.Body.Bytes(), &ae); w.Code != 200 || ae.TermsWaiting != "2026-12-01" || !strings.HasSuffix(ae.MyMacs, "/my-macs/vol-air") {
		t.Fatalf("the Mac's own enrollment: %d %s", w.Code, w.Body)
	}
	if _, err := l.s.acceptTerms(Person{AccountID: linusID, Handle: "linus"}); err != nil {
		t.Fatal(err)
	}
	w, ae = l.asMac("GET", "/api/agent/enrollment", "dev_vol-air", ""), api.AgentEnrollment{}
	if json.Unmarshal(w.Body.Bytes(), &ae); ae.TermsWaiting != "" || l.s.view(v).TermsWaiting {
		t.Errorf("after Accept: %s", w.Body)
	}
}

// storedDevice is the Mac's row as labd saved it.
func storedDevice(t *testing.T, s *Server, name string) api.Device {
	t.Helper()
	rows, err := s.store.devices()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.d.Name == name {
			return r.d
		}
	}
	t.Fatalf("no row for %s", name)
	return api.Device{}
}

// The Mac itself: lab-agent status says the changed terms wait on My Macs,
// the one Notice keeps to its wording, and lab is told in one line.
func TestLabAgentStatusSaysChangedTermsWaitOnMyMacs(t *testing.T) {
	n := newNoticeLab(t, labConfig{volunteer: true})
	topic := n.macTopic()
	n.s.shippedTerms.Store(&newTerms)
	n.s.termsTookEffect()
	out, err := n.volunteer("status")
	if err != nil || !strings.Contains(out, "the lab's terms changed (2026-12-01)") || !strings.Contains(out, "accept them on My Macs, /my-macs/"+n.name) {
		t.Fatalf("status: %v\n%s", err, out)
	}
	if !n.waitPosts(topic, 5*time.Second, func(ps []ntfyPost) bool { return len(termsNotices(ps)) == 1 }) {
		t.Fatalf("no terms Notice: %+v", n.ntfy.all())
	}
	checkNotice(t, termsNotices(n.ntfy.at(topic))[0], n.name)
	r, body := n.post("/api/jobs", api.JobSpec{Device: n.name, Kernel: n.artifact("krel=7.2-ok behavior=ok")})
	if r.StatusCode != http.StatusConflict || body != termsWaitingLine(n.name)+"\n" {
		t.Errorf("a job: %s %q", r.Status, body)
	}
}

// A refused topic shows on the Mac's card, and its Notices are dropped.
func TestARefusedTopicShowsOnMyMacs(t *testing.T) {
	l, _ := termsLab(t)
	v := l.s.dev("vol-air")
	v.mu.Lock()
	v.d.NtfyOwnURL, v.d.NoticeTopicRefused = "http://ntfy.example.org/mine", "it is not an https address"
	v.mu.Unlock()
	facts := factsOf(t, l.signedIn(linusID).myMacs().sole(t, "#mac-vol-air"))
	if !strings.Contains(facts, "Your own topic is refused: it is not an https address.") {
		t.Errorf("the card says: %s", facts)
	}
	if facts := factsOf(t, l.signedIn(linusID).myMacs().sole(t, "#mac-vol-pro")); strings.Contains(facts, "refused") {
		t.Errorf("another card says its topic is refused: %s", facts)
	}
}

// Whether a Mac waits for its Volunteer is decided again at each check-in,
// not only at the start: a Volunteer taken off the M team afterwards lends
// with no acceptance no longer, and one marked afterwards is held no more.
func TestTheTermsAreAskedAgainAtEachCheckIn(t *testing.T) {
	l, ntfy := termsLab(t)
	deploy(l)
	if l.s.view(l.s.dev("team-air")).TermsWaiting {
		t.Fatal("an M team member's Mac waits at the start")
	}
	ravi := l.signedIn(maintainerID)
	l.lrUnmark(graceID)
	if w := l.checkin("dev_team-air", api.Checkin{}); w.Code != http.StatusOK {
		t.Fatalf("check-in: %d %s", w.Code, w.Body)
	}
	if !l.s.view(l.s.dev("team-air")).TermsWaiting {
		t.Fatal("a Volunteer taken off the M team after the start still lends with no acceptance")
	}
	if code, _, body := ravi.lease("team-air", "ravi@desk", time.Hour); code != http.StatusConflict || body != termsWaitingLine("team-air")+"\n" {
		t.Errorf("a Lease on it: %d %q", code, body)
	}
	time.Sleep(300 * time.Millisecond)
	if got := termsNotices(ntfy.at("/team-air-topic")); len(got) != 1 {
		t.Errorf("its Volunteer was told %d times, want once: %+v", len(got), got)
	}
	l.checkin("dev_team-air", api.Checkin{})
	time.Sleep(200 * time.Millisecond)
	if got := termsNotices(ntfy.at("/team-air-topic")); len(got) != 1 {
		t.Errorf("a second check-in told them again: %d", len(got))
	}

	l.lrMember(linusID)
	l.checkin("dev_vol-air", api.Checkin{})
	if l.s.view(l.s.dev("vol-air")).TermsWaiting {
		t.Error("a Volunteer marked M team after the start is still held, with no Accept banner")
	}
}

// A Mac that comes to wait for the terms at a check-in, its Volunteer
// taken off the M team mid-job, takes nothing more from the lab: its
// running job is stopped as a Pause stops it, incomplete, and the Mac is
// put back on its own system.
func TestUnmarkingAVolunteerMidJobStopsTheJobAtTheNextCheckIn(t *testing.T) {
	const vera = 4242
	h := newCommandHold()
	l, clock, _ := windowLab(t, labConfig{wrap: h.wrap, lender: vera})
	if err := l.s.store.setMark(vera, "m_team", true); err != nil {
		t.Fatal(err)
	}
	l.s.shippedTerms.Store(&newTerms)
	l.s.termsTookEffect()
	l.addWindow(weekly(clock.Now().Add(-time.Minute), 6*time.Hour))
	h.armed.Store(true)
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-unmarked behavior=ok")})
	select {
	case <-h.held:
	case <-time.After(20 * time.Second):
		t.Fatal("the job was never sent to the Mac")
	}
	if err := l.s.store.setMark(vera, "m_team", false); err != nil {
		t.Fatal(err)
	}
	close(h.release)
	// Stopped: cut short for the terms and restoring the Mac, which can take
	// a while when the cut came as the one-shot was armed, or done.
	v := l.s.dev(l.name)
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		j, _ := l.s.store.job(id)
		if j.State == api.JobDone {
			if j.Outcome != api.OutcomeIncomplete || !strings.Contains(j.Summary, termsCut) {
				t.Fatalf("the job on a Mac that came to wait for the terms ended %s: %s", j.Outcome, j.Summary)
			}
			break
		}
		if j.State == api.JobRestoring && v.siteCutOf(id) == termsCut {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job on a Mac that came to wait for the terms is %s: %v", j.State, j.Events)
		}
	}
	if !l.s.view(l.s.dev(l.name)).TermsWaiting {
		t.Error("the Mac does not wait for its Volunteer")
	}
}

// labd's own log is outside what a forgetting reaches, so an acceptance is
// logged with the account, never the handle.
func TestAnAcceptanceIsLoggedWithNoHandle(t *testing.T) {
	l, _ := termsLab(t)
	logs := &lockedBuffer{b: &bytes.Buffer{}}
	l.s.log.SetOutput(logs)
	deploy(l)
	if w := l.signedIn(linusID).post("/my-macs/accept-terms"); w.Code != http.StatusSeeOther {
		t.Fatalf("Accept: %d %s", w.Code, w.Body)
	}
	got := logs.String()
	if !strings.Contains(got, "accepted the terms") || strings.Contains(got, "linus") {
		t.Errorf("the acceptance in labd's log:\n%s", got)
	}
}
