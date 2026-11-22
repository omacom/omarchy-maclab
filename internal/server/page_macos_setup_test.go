package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

var reMacOSLine = regexp.MustCompile(`echo (mos_enr_[0-9a-f]{32}) \| sudo /usr/local/lab/bin/lab-macos-runner enroll --server \S+ --token-stdin`)

// "Set up macOS" gives a Mac's Volunteer the same one-time line `lab macos
// enroll` gives a Maintainer, for their own Mac only, shown once on its
// card; the card says whether the Mac is Attended or Unattended.
func TestSetUpMacOSOnMyMacsGivesTheVolunteerTheRunnersLine(t *testing.T) {
	l := myMacsLab(t)
	open := l.s.now().Add(2 * time.Hour)
	l.lentMac("linus-air", linusID, api.AccessQualification, open)
	l.lentMac("graces-air", graceID, api.AccessQualification, open)
	linus := l.signedIn(linusID)

	card := linus.myMacs().sole(t, "#mac-linus-air")
	if facts := factsOf(t, card); !strings.Contains(facts, "macOS not set up") {
		t.Errorf("a Mac with no runner: %s", facts)
	}
	card.sole(t, `#macos-setup-linus-air form[action="/my-macs/linus-air/macos"] button`)
	if strings.Contains(linus.page("/my-macs"), "mos_enr_") {
		t.Fatal("My Macs shows a runner's line before Set up macOS")
	}

	w := linus.post("/my-macs/linus-air/macos")
	if w.Code != http.StatusOK {
		t.Fatalf("Set up macOS: %d %s", w.Code, refusalIn(w.Body.String()))
	}
	body := w.Body.String()
	m := reMacOSLine.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no runner's line on the answer:\n%s", part(t, body, `id="mac-linus-air"`, "</section>\n"))
	}
	offersToCopy(t, body, "the macOS runner's line", m[0])
	fence := mainOf(t, body).sole(t, "#macos-linus-air").words()
	for _, want := range []string{"only time", "sudo ./install.sh", "sudo lab-macos-runner setup", "Attended"} {
		if !strings.Contains(fence, want) {
			t.Errorf("the line's card lacks %q: %s", want, fence)
		}
	}
	if strings.Contains(linus.page("/my-macs"), m[1]) {
		t.Error("the line is shown again")
	}

	// It is the runner's: it enrolls this Mac's enrollment, once.
	r := l.browser().do("POST", "/api/macos/runner/enroll", "", "Authorization", "Bearer "+m[1])
	if r.Code != 200 {
		t.Fatalf("enrolling with the Volunteer's line: %d %s", r.Code, r.Body)
	}
	e := decodeInto[api.MacOSEnrolled](t, r.Body.String())
	if e.Mac != "linus-air" {
		t.Errorf("enrolled %+v", e)
	}
	if facts := factsOf(t, linus.myMacs().sole(t, "#mac-linus-air")); !strings.Contains(facts, "macOS Attended") {
		t.Errorf("a runner just enrolled: %s", facts)
	}
	if r := l.browser().do("POST", "/api/macos/runner/attendance", `{"unattended":true}`, "Authorization", "Bearer "+e.Secret, "Content-Type", "application/json"); r.Code != 200 {
		t.Fatalf("attendance: %d %s", r.Code, r.Body)
	}
	if facts := factsOf(t, linus.myMacs().sole(t, "#mac-linus-air")); !strings.Contains(facts, "macOS Unattended") {
		t.Errorf("an Unattended Mac: %s", facts)
	}

	// Nobody else's Mac, and nothing for someone signed out.
	if w := linus.post("/my-macs/graces-air/macos"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "mos_enr_") {
		t.Errorf("Set up macOS on another Volunteer's Mac: %d", w.Code)
	}
	if w := linus.post("/my-macs/no-such-mac/macos"); w.Code != http.StatusNotFound {
		t.Errorf("Set up macOS on no Mac: %d", w.Code)
	}
	if w := l.browser().post("/my-macs/linus-air/macos"); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "mos_enr_") {
		t.Errorf("Set up macOS signed out: %d", w.Code)
	}
}

// An M team member has their cards under "Mine" on Macs: Set up macOS
// answers there, with the line on their card.
func TestSetUpMacOSOnMineOnMacs(t *testing.T) {
	l := myMacsLab(t)
	l.lentMac("graces-air", graceID, api.AccessFull, l.s.now().Add(2*time.Hour))
	grace := l.lrMember(graceID)
	w := grace.post("/my-macs/graces-air/macos")
	if w.Code != http.StatusOK || reMacOSLine.FindString(w.Body.String()) == "" {
		t.Fatalf("Set up macOS from Mine: %d", w.Code)
	}
	mainOf(t, w.Body.String()).sole(t, "#mac-graces-air #macos-graces-air")
}
