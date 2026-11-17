package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/window"
)

// windowFromSite is a lent Mac whose Volunteer, Grace, changes its weekly
// window on My Macs.
type windowFromSite struct {
	l      *signInLab
	grace  *browser
	v      *dev
	secret string
}

func newWindowFromSite(t *testing.T) *windowFromSite {
	t.Helper()
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	v, secret := grace.lent("shelf-mac")
	l.s.cancelJob(v.snapshot().FirstBaseline.Job, "")
	return &windowFromSite{l: l, grace: grace, v: v, secret: secret}
}

// holds is the Mac checking in with these weekly rules, set at rulesAt by
// its own clock; it answers what labd handed back for the Mac to take.
func (w *windowFromSite) holds(rulesAt time.Time, rules ...string) *api.WindowWanted {
	w.l.t.Helper()
	s := &window.Settings{Rules: []window.Rule{}, Zone: "UTC", RulesAt: rulesAt}
	for _, r := range rules {
		p, err := window.ParseRule(r)
		if err != nil {
			w.l.t.Fatal(err)
		}
		s.Rules = append(s.Rules, p)
	}
	rec := w.l.checkin(w.secret, api.Checkin{Window: s})
	var resp api.CheckinResponse
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		w.l.t.Fatalf("check-in: %d %s", rec.Code, rec.Body)
	}
	return resp.Window
}

func (w *windowFromSite) wanted() []string {
	w.l.t.Helper()
	d := w.v.snapshot()
	if d.WindowWanted == nil {
		return nil
	}
	out := []string{}
	for _, r := range d.WindowWanted.Rules {
		out = append(out, r.String())
	}
	return out
}

func (w *windowFromSite) card() string {
	w.l.t.Helper()
	return factsOf(w.l.t, w.grace.myMacs().sole(w.l.t, "#mac-shelf-mac"))
}

func rulesOf(ww *api.WindowWanted) string {
	if ww == nil {
		return "nothing"
	}
	var out []string
	for _, r := range ww.Rules {
		out = append(out, r.String())
	}
	return "[" + strings.Join(out, ", ") + "]"
}

// The Mac's own Volunteer adds and removes weekly windows on My Macs, in
// the window syntax, always included. labd keeps what they want and hands
// it to the Mac at its next check-in; until the Mac holds it, the card says
// it is pending. The change is in the Mac's record and goes to the Lab
// group once, when it is made.
func TestTheVolunteerSetsTheWindowOnMyMacsAndTheMacTakesItAtItsNextCheckin(t *testing.T) {
	w := newWindowFromSite(t)
	at := "/my-macs/shelf-mac/window"
	id := w.v.snapshot().ID
	w.holds(time.Time{})

	w.grace.did(at, "days", "mon-fri", "from", "19:00", "to", "07:00")
	if got := w.wanted(); len(got) != 1 || got[0] != "mon-fri 19:00-07:00" {
		t.Fatalf("labd wants %v", got)
	}
	if card := w.card(); !strings.Contains(card, "mon-fri 19:00-07:00 pending until the Mac checks in") {
		t.Errorf("the card does not say the window waits for the Mac: %s", card)
	}
	if got := w.holds(time.Time{}); rulesOf(got) != "[mon-fri 19:00-07:00]" {
		t.Errorf("the check-in handed the Mac %s", rulesOf(got))
	}
	// The Mac took it: nothing waits, the card shows what the Mac holds.
	if got := w.holds(w.l.clock.Now(), "mon-fri 19:00-07:00"); got != nil {
		t.Errorf("after the Mac took it, labd still hands it %s", rulesOf(got))
	}
	if w.wanted() != nil {
		t.Errorf("labd still wants %v once the Mac holds it", w.wanted())
	}
	if card := w.card(); strings.Contains(card, "pending until the Mac checks in") || !strings.Contains(card, "Availability window mon-fri 19:00-07:00") {
		t.Errorf("the card once the Mac holds the window: %s", card)
	}

	// Always, then a remove: each from what the Mac holds or what waits.
	w.grace.did(at, "always", "yes")
	w.grace.did(at, "remove", "mon-fri 19:00-07:00")
	if got := w.wanted(); len(got) != 1 || got[0] != "always" {
		t.Fatalf("labd wants %v after always and a remove", got)
	}
	w.grace.refused("is not a day", at, "days", "funday", "from", "19:00", "to", "07:00")
	w.grace.refused("not one of this Mac's windows", at, "remove", "sat 10:00-12:00")
	w.grace.refused("already one of this Mac's windows", at, "days", "mon-sun", "from", "00:00", "to", "24:00")
	w.holds(w.l.clock.Now(), "always")
	// Removing the last window closes the Mac: it waits for nothing.
	w.grace.did(at, "remove", "always")
	if got := w.wanted(); got == nil || len(got) != 0 {
		t.Fatalf("labd wants %v after the last window went", got)
	}
	if got := w.holds(w.l.clock.Now(), "always"); got == nil || len(got.Rules) != 0 {
		t.Errorf("the check-in handed the Mac %s, want no windows", rulesOf(got))
	}
	w.holds(w.l.clock.Now())

	record, lines := macRecordOf(t, w.l.s, id), w.l.labLines()
	for _, want := range []string{
		"its Volunteer set its weekly Availability window on My Macs: mon-fri 19:00-07:00",
		"its Volunteer set its weekly Availability window on My Macs: mon-fri 19:00-07:00, always",
		"its Volunteer set its weekly Availability window on My Macs: always",
		"its Volunteer removed its weekly Availability window on My Macs",
	} {
		if !has(record, want) {
			t.Errorf("the Mac's record lacks %q:\n%s", want, strings.Join(record, "\n"))
		}
	}
	if has(record, "its Volunteer set its weekly Availability window: mon-fri 19:00-07:00") {
		t.Errorf("the Mac taking the site's window is a second event:\n%s", strings.Join(record, "\n"))
	}
	if n := strings.Count(strings.Join(lines, "\n"), "shelf-mac changed its weekly Availability window: mon-fri 19:00-07:00."); n != 1 {
		t.Errorf("the Lab group has the site's change %d times:\n%s", n, strings.Join(lines, "\n"))
	}
}

// Last change wins: a window set at the Mac after the one set on My Macs
// stands, and labd stops handing the site's to the Mac. One set at the Mac
// before it gives way.
func TestTheLastChangeOfTheWindowWinsBetweenTheMacAndMyMacs(t *testing.T) {
	w := newWindowFromSite(t)
	at := "/my-macs/shelf-mac/window"
	before := w.l.clock.Now()
	w.holds(before, "sat 10:00-12:00")
	w.l.clock.Advance(time.Minute)
	w.grace.did(at, "always", "yes")
	if got := w.holds(before, "sat 10:00-12:00"); rulesOf(got) != "[sat 10:00-12:00, always]" {
		t.Errorf("a Mac whose window is older than the site's was handed %s", rulesOf(got))
	}
	w.l.clock.Advance(time.Minute)
	if got := w.holds(w.l.clock.Now(), "sun 09:00-17:00"); got != nil {
		t.Errorf("a window set at the Mac after the site's was handed %s", rulesOf(got))
	}
	if w.wanted() != nil {
		t.Errorf("labd still wants %v after a later change at the Mac", w.wanted())
	}
	if d := w.v.snapshot(); d.Window == nil || d.Window.Schedule() != "sun 09:00-17:00" {
		t.Errorf("labd's copy of the window: %+v", d.Window)
	}
	if record := macRecordOf(t, w.l.s, w.v.snapshot().ID); !has(record, "its Volunteer set its weekly Availability window: sun 09:00-17:00") {
		t.Errorf("the Mac's own later change is not on its record:\n%s", strings.Join(record, "\n"))
	}
}

// Nobody but the Mac's own Volunteer widens its window from the site: not
// another Volunteer, not a Maintainer. Its Volunteer's Lab token may, on
// labd's API.
func TestOnlyTheMacsOwnVolunteerSetsItsWindowFromTheSite(t *testing.T) {
	w := newWindowFromSite(t)
	l := w.l
	at := "/my-macs/shelf-mac/window"
	l.signedIn(maintainerID).did("/people/mark", "handle", "edith", "as", "invite")
	for who, b := range map[string]*browser{"another Volunteer": l.signedIn(edithID), "a Maintainer": l.signedIn(maintainerID)} {
		if r := b.post(at, "always", "yes"); r.Code != http.StatusNotFound {
			t.Errorf("%s set the window of a Mac not theirs: %d", who, r.Code)
		}
	}
	post := func(token string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", "http://"+labHost+"/api/devices/shelf-mac/window", strings.NewReader(string(b)))
		r = r.WithContext(onTunnel(r.Context()))
		r.RemoteAddr = "@"
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("CF-Connecting-IP", "203.0.113.9")
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		l.h.ServeHTTP(rec, r)
		return rec
	}
	ravis := l.signedIn(maintainerID).makeToken("ravi's", "30")
	if r := post(ravis, api.WindowChange{Add: "always"}); r.Code != http.StatusNotFound {
		t.Errorf("a Maintainer's Lab token set a Volunteer's window: %d %s", r.Code, r.Body)
	}
	if w.wanted() != nil {
		t.Fatalf("someone else's request changed the window: %v", w.wanted())
	}
	graces := w.grace.makeToken("grace's", "30")
	if r := post(graces, api.WindowChange{Add: "mon-fri 19:00-07:00"}); r.Code != http.StatusOK {
		t.Fatalf("the Volunteer's Lab token: %d %s", r.Code, r.Body)
	}
	if r := post(graces, api.WindowChange{Add: "funday 19:00-07:00"}); r.Code != http.StatusUnprocessableEntity {
		t.Errorf("a bad window on the API: %d %s", r.Code, r.Body)
	}
	if got := w.wanted(); len(got) != 1 || got[0] != "mon-fri 19:00-07:00" {
		t.Errorf("labd wants %v", got)
	}
}

// End to end, with lab-agent: a window set from the site reaches the Mac at
// its next check-in, which then enforces it itself, and labd waits for
// nothing more. A window then set at the Mac is the last change and stands.
func TestAWindowSetFromTheSiteReachesTheMacAndTheMacsOwnLaterOneStands(t *testing.T) {
	l, _, parked := windowLab(t, labConfig{lender: veraID})
	v := l.s.dev(l.name)
	if err := l.s.changeWindow(v, l.vera(), true, window.Always); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		out := l.mustVolunteer("window", "show")
		if strings.Contains(out, "always\n") && v.snapshot().WindowWanted == nil {
			if !strings.Contains(out, "Window: open always") || !strings.Contains(out, strings.Join(parked, " ")) {
				t.Errorf("the Mac after taking always:\n%s", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Mac never took the site's window:\n%s\nlabd wants %+v", out, v.snapshot().WindowWanted)
		}
		time.Sleep(50 * time.Millisecond)
	}
	l.removeWindow([]string{"always"})
	if d := v.snapshot(); d.WindowWanted != nil || strings.Contains(d.Window.Schedule(), "always") {
		t.Errorf("after the Mac removed always: wanted %+v, labd's copy %q", d.WindowWanted, d.Window.Schedule())
	}
}
