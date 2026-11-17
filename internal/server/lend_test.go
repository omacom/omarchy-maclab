package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// A model of the site's list: a laptop, and what the lab suggests naming it.
const (
	lendModelID   = "j314s"
	lendModelName = "MacBook Pro (14-inch, M1 Pro, 2021)"
	lendSuggested = "macbook-pro-14-m1-pro-2021"
)

// lendLab is rolesLab with a lab-agent to hand out, so it makes Join
// commands, and its log.
func lendLab(t *testing.T) (*signInLab, *lockedBuffer) {
	t.Helper()
	logs := &lockedBuffer{b: &bytes.Buffer{}}
	l := rolesLab(t, func(c *Config) {
		bin := filepath.Join(t.TempDir(), "lab-agent")
		if err := os.WriteFile(bin, []byte("\x7fELF lab-agent for the test"), 0o755); err != nil {
			t.Fatal(err)
		}
		c.AgentBinary = bin
	})
	l.s.log.SetOutput(logs)
	return l, logs
}

// lockedBuffer is a log several goroutines write.
type lockedBuffer struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (w *lockedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

var reJoinLine = regexp.MustCompile(`(?s)<pre class="command"><span class="prompt" aria-hidden="true">\$ </span><code data-copy>(.*?)</code></pre>`)

// joinCommandOn is the Join command a page shows: its one line, as a shell
// reads it, and its token.
func joinCommandOn(t *testing.T, body string) (line, token string) {
	t.Helper()
	m := reJoinLine.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page shows no Join command:\n%s", refusalIn(body))
	}
	line = html.UnescapeString(m[1])
	offersToCopy(t, body, "the Join command", line)
	token = regexp.MustCompile(`enr_[0-9a-f]{32}`).FindString(line)
	if token == "" {
		t.Fatalf("the Join command carries no token: %s", line)
	}
	return line, token
}

// makeJoin has b make a Join command for a Mac called name.
func (b *browser) makeJoin(name string) (line, token string) {
	b.l.t.Helper()
	w := b.post("/lend/join-command", "model", lendModelID, "name", name)
	if w.Code != http.StatusOK {
		b.l.t.Fatalf("a Join command for %q: %d %s", name, w.Code, refusalIn(w.Body.String()))
	}
	return joinCommandOn(b.l.t, w.Body.String())
}

// asMac asks the lab as a Mac does: through the tunnel, with no cookie.
func (l *signInLab) asMac(method, target, bearer, body string) *httptest.ResponseRecorder {
	l.t.Helper()
	hdr := []string{"Content-Type", "application/json"}
	if bearer != "" {
		hdr = append(hdr, "Authorization", "Bearer "+bearer)
	}
	return l.browser().do(method, target, body, hdr...)
}

func (l *signInLab) joinCheck(token, key string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(api.JoinCheckRequest{ActivationKey: key})
	return l.asMac("POST", "/api/join/check", token, string(b))
}

func (l *signInLab) enrollWith(token, key string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(api.EnrollRequest{Token: token, ActivationKey: key, TermsSHA256: termsSum()})
	return l.asMac("POST", "/api/agent/enroll", "", string(b))
}

// labLines is every line labd has for the Lab group, oldest first.
func (l *signInLab) labLines() []string { return l.told("lab") }

func since(lines []string, n int) []string { return lines[min(n, len(lines)):] }

// "Lend a Mac" opens with the Test machine rule, then the table of what the
// lab may and may not do, then the terms; signed out it leads to sign-in.
func TestLendAMacOpensWithTheTestMachineRuleTheTableThenTheTerms(t *testing.T) {
	l, _ := lendLab(t)
	if w := l.browser().do("GET", "/lend", ""); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/sign-in?return=") {
		t.Errorf("signed out: %d to %q", w.Code, w.Header().Get("Location"))
	}
	for who, id := range map[string]int64{"no role": linusID, "a Maintainer": maintainerID, "an Admin": ownerID} {
		w := l.signedIn(id).do("GET", "/lend", "")
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", who, w.Code)
		}
		at := func(piece string) int {
			i := strings.Index(body, piece)
			if i < 0 {
				t.Fatalf("%s: the page lacks %q", who, piece)
			}
			return i
		}
		rule := at("Lend only a Test machine: a Mac with nothing on it, in Omarchy or in macOS, that you cannot afford to lose or to have read.")
		table := at(`<tr><td><b>Qualification</b>`)
		full, never := at(`<tr><td><b>Full</b>`), at(`<tr><td><b>Never</b>`)
		terms := at(html.EscapeString(web.Terms().Body))
		if !(rule < table && table < full && full < never && never < terms) {
			t.Errorf("%s: the rule (%d), the table (%d, %d, %d) and the terms (%d) are out of order", who, rule, table, full, never, terms)
		}
		for _, piece := range []string{"A Hard reset", "Anything outside the Availability window or on battery", "Publishing its reports unless you turn that on",
			"Showing your handle to the public", "the M team's own included", web.Terms().SHA256, "never the public site"} {
			if id == linusID && piece == "never the public site" || strings.Contains(body, piece) {
				continue
			}
			t.Errorf("%s: the page lacks %q", who, piece)
		}
		if strings.Contains(body, "roll") && strings.Contains(body, "rolls the system back") {
			t.Errorf("%s: the table offers an install with rollback", who)
		}
	}
	if w := l.signedIn(linusID).do("GET", "/", ""); !strings.Contains(w.Body.String(), `<a href="/lend">Lend a Mac</a>`) {
		t.Error("signed in with no role, row 2 does not link Lend a Mac")
	}
}

// An M team member and a Maintainer have leave to lend; a person with an
// Invite has it from their first sign-in; anyone else sends a Request.
func TestWhoHasLeaveToLend(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "edith", "as", "m-team")
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	const joinForm, requestForm = `action="/lend/join-command"`, `action="/lend/request"`
	for who, id := range map[string]int64{"an M team member": edithID, "a Maintainer": maintainerID, "an Admin": ownerID, "an invited person at first sign-in": graceID} {
		body := l.signedIn(id).do("GET", "/lend", "").Body.String()
		if !strings.Contains(body, joinForm) || strings.Contains(body, requestForm) {
			t.Errorf("%s does not get the join form", who)
		}
	}
	linus := l.signedIn(linusID)
	if body := linus.do("GET", "/lend", "").Body.String(); strings.Contains(body, joinForm) || !strings.Contains(body, requestForm) {
		t.Error("a person with no leave gets the join form, or no request form")
	}
	linus.refused("leave to lend", "/lend/join-command", "model", lendModelID, "name", "sneak")
	if n := l.count(`SELECT count(*) FROM tokens`); n != 0 {
		t.Errorf("a person with no leave made %d Join commands", n)
	}
	ravi.refused("leave to lend already", "/lend/request", "model", lendModelID)
}

// A Request to lend: the model from the site's list, an optional note and
// an optional contact only Maintainers see; one per person; waiting with
// Withdraw, approved, or declined with "Ask again". Approval is of the
// person. Each step is recorded and posted, naming the Maintainer who
// acted and never the person asking.
func TestARequestToLendIsAnsweredByAMaintainer(t *testing.T) {
	l, _ := lendLab(t)
	ravi, linus := l.signedIn(maintainerID), l.signedIn(linusID)

	linus.refused("from the list", "/lend/request", "model", "a-toaster")
	linus.refused("from the list", "/lend/request", "note", "no model")
	linus.did("/lend/request", "model", lendModelID, "note", "It sits on a shelf", "contact", "signal: 0400 111 222")
	page := linus.do("GET", "/lend", "").Body.String()
	for _, piece := range []string{"Waiting since 2026-11-14 12:00 UTC", lendModelName, "A Maintainer answers here; nothing is sent to you.", `action="/lend/request/withdraw"`} {
		if !strings.Contains(page, html.EscapeString(piece)) && !strings.Contains(page, piece) {
			t.Errorf("waiting, the page lacks %q", piece)
		}
	}
	if strings.Contains(page, `action="/lend/request"`) || strings.Contains(page, `action="/lend/join-command"`) {
		t.Error("waiting, the page still offers a form")
	}
	linus.refused("waiting already", "/lend/request", "model", lendModelID)
	if n := l.s.requestsToLendWaiting(); n != 1 {
		t.Errorf("the daily summary counts %d requests waiting, want 1", n)
	}

	// People shows it to Maintainers, with the note and the contact, and
	// carries the count in the navigation.
	people := ravi.do("GET", "/people", "").Body.String()
	request := part(t, people, `<li id="request-9">`, `</li>`)
	for _, piece := range []string{"@linus", html.EscapeString(lendModelName), "It sits on a shelf", "signal: 0400 111 222", `action="/people/9/request/approve"`, `action="/people/9/request/decline"`, `name="reason"`} {
		if !strings.Contains(request, piece) {
			t.Errorf("People, the request lacks %q:\n%s", piece, request)
		}
	}
	if !strings.Contains(people, `People<b class="count">1</b>`) {
		t.Error("People carries no count of pending requests in the navigation")
	}
	if strings.Contains(page, "signal: 0400") {
		t.Error("the contact is shown back on Lend a Mac")
	}

	// Withdraw asks twice, and the form is back.
	if w := linus.post("/lend/request/withdraw"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Withdraw: your Request to lend is taken back") {
		t.Errorf("Withdraw without the second click: %d", w.Code)
	}
	linus.did("/lend/request/withdraw", "sure", "yes")
	if page := linus.do("GET", "/lend", "").Body.String(); !strings.Contains(page, `action="/lend/request"`) || strings.Contains(page, "Waiting since") {
		t.Error("withdrawn, the page does not ask again")
	}
	ravi.refused("No Request to lend of @linus is waiting", "/people/9/request/approve")

	// Declined, with the Maintainer's one-line reason; "Ask again" works at once.
	linus.did("/lend/request", "model", lendModelID)
	l.clock.Advance(time.Hour)
	ravi.refused("one line", "/people/9/request/decline", "reason", strings.Repeat("x", 201))
	ravi.did("/people/9/request/decline", "reason", "Not a test machine, by your note")
	page = linus.do("GET", "/lend", "").Body.String()
	for _, piece := range []string{"Declined 2026-11-14 13:00 UTC: Not a test machine, by your note.", "Ask again</button>", `action="/lend/request"`} {
		if !strings.Contains(page, piece) {
			t.Errorf("declined, the page lacks %q", piece)
		}
	}
	if l.who(linusID).MayLend() {
		t.Fatal("a declined person has leave to lend")
	}
	linus.did("/lend/request", "model", lendModelID, "note", "It is one now")

	// Approved: the page becomes the join form, and the leave is the person's.
	ravi.did("/people/9/request/approve")
	if p := l.who(linusID); !p.Lend || !p.MayLend() {
		t.Fatalf("approved, account %d is %+v", linusID, p)
	}
	if page := linus.do("GET", "/lend", "").Body.String(); !strings.Contains(page, `action="/lend/join-command"`) || strings.Contains(page, `action="/lend/request"`) {
		t.Error("approved, the page is not the join form")
	}
	if n := l.s.requestsToLendWaiting(); n != 0 {
		t.Errorf("%d requests still wait", n)
	}
	if row := part(t, ravi.do("GET", "/people", "").Body.String(), `id="person-9"`, `</tr>`); !strings.Contains(row, "has leave to lend, no Mac yet") {
		t.Errorf("People does not say the person has leave to lend and no Mac yet:\n%s", row)
	}
	// It stands when their last Mac leaves.
	v := l.addMac("linus-mini", linusID)
	v.setState(api.StateLeft, "left")
	if p := l.who(linusID); !p.MayLend() || p.Volunteer {
		t.Errorf("their last Mac left, account %d is %+v", linusID, p)
	}

	want := []string{
		"@linus sent a Request to lend",
		"@linus withdrew their Request to lend",
		"@linus sent a Request to lend",
		"@ravi declined the Request to lend of @linus: Not a test machine, by your note",
		"@linus sent a Request to lend",
		"@ravi approved the Request to lend of @linus",
	}
	if got := l.recorded(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the People record:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	wantLab := []string{
		"A Request to lend is waiting. " + labAddress + "/people",
		"A Request to lend was withdrawn. " + labAddress + "/people",
		"A Request to lend is waiting. " + labAddress + "/people",
		"ravi declined a Request to lend. " + labAddress + "/people",
		"A Request to lend is waiting. " + labAddress + "/people",
		"ravi approved a Request to lend. " + labAddress + "/people",
	}
	if got := l.labLines(); fmt.Sprint(got) != fmt.Sprint(wantLab) {
		t.Errorf("the Lab group's lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantLab, "\n"))
	}
	for _, line := range l.labLines() {
		if strings.Contains(line, "linus") {
			t.Errorf("a Lab group line names the person asking: %s", line)
		}
	}
	wantAgents := []string{"ravi declined linus's Request to lend.", "ravi approved linus's Request to lend."}
	if got := l.agentsLines(); fmt.Sprint(got) != fmt.Sprint(wantAgents) {
		t.Errorf("the lines for Agents: %q, want %q", got, wantAgents)
	}
}

// Blocked takes a waiting request and an unused Join command back.
func TestBlockedTakesBackWhatWasOnTheWayToLending(t *testing.T) {
	l, _ := lendLab(t)
	ravi, linus := l.signedIn(maintainerID), l.signedIn(linusID)
	linus.did("/lend/request", "model", lendModelID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	_, token := l.signedIn(graceID).makeJoin("shelf-mac")
	ravi.did(personPath(linusID, "block"), "sure", "yes")
	ravi.did(personPath(graceID, "block"), "sure", "yes")
	if n := l.s.requestsToLendWaiting(); n != 0 {
		t.Errorf("a Blocked person's request still waits (%d)", n)
	}
	if w := l.enrollWith(token, keyA); w.Code != http.StatusForbidden {
		t.Errorf("a Blocked person's Join command enrolls: %d %s", w.Code, w.Body)
	}
	ravi.refused("Unblock first", "/people/9/request/approve")
}

// The Join command is one line; its token is in a header and on stdin
// only; it lasts 1 hour; a person has one unused, and a new one cancels the
// old; the first Mac to use it binds it and may run it again; any other Mac
// is refused.
func TestTheJoinCommandIsGoodForOneMacOnce(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)

	line, first := grace.makeJoin("shelf-mac")
	if strings.ContainsAny(line, "\n\r") {
		t.Errorf("the Join command is more than one line: %q", line)
	}
	if want := " curl -fsSL " + labAddress + "/join | LAB_JOIN=" + first + " bash"; line != want {
		t.Errorf("the Join command is\n%q\nwant\n%q", line, want)
	}
	// Every address in a line a person is told to run, and in the script
	// it runs, is the Lab address.
	script := l.asMac("GET", "/join", "", "").Body.String()
	for _, u := range regexp.MustCompile(`https?://[^\s'"]+`).FindAllString(line+"\n"+script, -1) {
		if u != labAddress && u != labAddress+"/join" {
			t.Errorf("the Join command names %s, which is not the Lab address", u)
		}
	}
	if n := l.count(`SELECT count(*) FROM tokens WHERE token_hash = ?`, hashSecret(first)); n != 1 {
		t.Fatalf("the lab keeps %d rows for the token's hash", n)
	}
	if bytes.Contains(l.onDisk(), []byte(first)) {
		t.Error("the lab keeps the token itself")
	}
	// The page shows the line once: afterwards only the Mac's name, the
	// expiry and Cancel.
	page := grace.do("GET", "/lend", "").Body.String()
	if strings.Contains(page, first) || !strings.Contains(page, "An unused Join command for <b>shelf-mac</b> is good until 2026-11-14 13:00 UTC") || !strings.Contains(page, `action="/lend/join-command/cancel"`) {
		t.Errorf("the page after the Join command was shown:\n%s", part(t, page, `id="lend"`, `</section>`))
	}

	// A new one cancels the old.
	_, second := grace.makeJoin("shelf-mac")
	if w := l.joinCheck(first, keyA); w.Code != http.StatusUnauthorized {
		t.Errorf("the cancelled Join command still passes its test: %d", w.Code)
	}
	if n := l.count(`SELECT count(*) FROM tokens`); n != 1 {
		t.Errorf("the person holds %d unused Join commands, want 1", n)
	}

	// The first Mac to use it binds it; any other Mac is refused, at the
	// test and at enrollment.
	w := l.joinCheck(second, keyA)
	var check api.JoinCheck
	if json.Unmarshal(w.Body.Bytes(), &check); w.Code != http.StatusOK || check.Name != "shelf-mac" || check.Kind != api.KindVolunteer || check.TermsSHA256 != termsSum() || check.Enrolled {
		t.Fatalf("the first Mac's test: %d %s", w.Code, w.Body)
	}
	if w := l.joinCheck(second, keyB); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "another Mac") {
		t.Errorf("another Mac's test: %d %s", w.Code, w.Body)
	}
	if w := l.enrollWith(second, keyB); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "another Mac") {
		t.Errorf("another Mac enrolling: %d %s", w.Code, w.Body)
	}
	if w := l.joinCheck(second, keyA); w.Code != http.StatusOK {
		t.Errorf("the first Mac's test again: %d %s", w.Code, w.Body)
	}
	var enrolled api.EnrollResponse
	if w := l.enrollWith(second, keyA); json.Unmarshal(w.Body.Bytes(), &enrolled) != nil || w.Code != http.StatusOK || enrolled.Name != "shelf-mac" {
		t.Fatalf("the first Mac enrolling: %d %s", w.Code, w.Body)
	}
	d := l.s.dev("shelf-mac").snapshot()
	if d.VolunteerID != graceID || d.Kind != api.KindVolunteer || !d.Volunteer || d.AccessLevel != api.AccessQualification || d.Publish || d.Label != lendModelName || d.Model != api.ModelLaptop ||
		d.TermsSHA256 != termsSum() || !d.TermsAccepted.Equal(l.clock.Now()) || d.VolunteerName != "" || d.Contact != "" {
		t.Errorf("the Mac that joined: %+v", d)
	}
	if p := l.who(graceID); p.Role() != web.External {
		t.Errorf("lending a Mac, the person is %s", p.Role())
	}
	// Interrupted, the same Mac runs the command again; another still can't.
	again := l.joinCheck(second, keyA)
	if json.Unmarshal(again.Body.Bytes(), &check); again.Code != http.StatusOK || !check.Enrolled {
		t.Errorf("the same Mac's test after enrolling: %d %s", again.Code, again.Body)
	}
	if w := l.enrollWith(second, keyA); w.Code != http.StatusOK {
		t.Errorf("the same Mac enrolling again before its first check-in: %d %s", w.Code, w.Body)
	}
	if w := l.enrollWith(second, keyB); w.Code != http.StatusForbidden {
		t.Errorf("another Mac with the used token: %d", w.Code)
	}

	// It lasts 1 hour.
	_, third := grace.makeJoin("second-mac")
	l.clock.Advance(59 * time.Minute)
	if w := l.joinCheck(third, keyB); w.Code != http.StatusOK {
		t.Errorf("at 59 minutes: %d %s", w.Code, w.Body)
	}
	l.clock.Advance(2 * time.Minute)
	if w := l.joinCheck(third, keyB); w.Code != http.StatusUnauthorized {
		t.Errorf("after an hour, the test: %d", w.Code)
	}
	if w := l.enrollWith(third, keyB); w.Code != http.StatusForbidden {
		t.Errorf("after an hour, enrolling: %d", w.Code)
	}
}

// Join commands made, cancelled and expired unused are recorded and posted;
// a refused token is logged and never posted.
func TestJoinCommandsAreRecordedAndRefusedTokensOnlyLogged(t *testing.T) {
	l, logs := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	told, recorded := len(l.labLines()), len(l.recorded())

	grace.makeJoin("one")
	grace.makeJoin("two")
	if w := grace.post("/lend/join-command/cancel"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Cancel: this Join command stops working now") {
		t.Errorf("Cancel without the second click: %d", w.Code)
	}
	grace.did("/lend/join-command/cancel", "sure", "yes")
	grace.refused("no unused Join command", "/lend/join-command/cancel", "sure", "yes")
	_, third := grace.makeJoin("three")
	l.clock.Advance(61 * time.Minute)
	l.s.expireJoinCommands()

	// A Maintainer's lab enroll, for a Lab-owned Mac, names the Maintainer.
	if w := l.asMac("POST", "/api/enroll-tokens", "lab_adm_secret", `{"name":"m1-pro"}`); w.Code != http.StatusOK {
		t.Fatalf("lab enroll: %d %s", w.Code, w.Body)
	}
	if w := l.asMac("DELETE", "/api/enroll-tokens/m1-pro", "lab_adm_secret", ""); w.Code != http.StatusOK {
		t.Fatalf("lab enroll --revoke: %d %s", w.Code, w.Body)
	}

	wantLab := []string{
		"A Join command was made for one.",
		"The Join command for one was cancelled.",
		"A Join command was made for two.",
		"The Join command for two was cancelled.",
		"A Join command was made for three.",
		"The Join command for three expired unused.",
		"ada made a Join command for m1-pro.",
		"ada cancelled the Join command for m1-pro.",
	}
	for i := range wantLab {
		wantLab[i] += " " + labAddress + "/people"
	}
	if got := since(l.labLines(), told); fmt.Sprint(got) != fmt.Sprint(wantLab) {
		t.Errorf("the Lab group's lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantLab, "\n"))
	}
	want := []string{
		"@grace made a Join command for one",
		"@grace cancelled the Join command for one",
		"@grace made a Join command for two",
		"@grace cancelled the Join command for two",
		"@grace made a Join command for three",
		"@grace let the Join command for three expire unused",
		"@ada made a Join command for m1-pro",
		"@ada cancelled the Join command for m1-pro",
	}
	if got := since(l.recorded(), recorded); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the People record:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range l.labLines() {
		if strings.Contains(line, "grace") {
			t.Errorf("a Lab group line names the person asking: %s", line)
		}
	}

	// Refused tokens: logged, not posted, not recorded.
	told, recorded = len(l.labLines()), len(l.recorded())
	made := "enr_" + strings.Repeat("0", 32)
	for _, w := range []*httptest.ResponseRecorder{
		l.asMac("GET", "/api/join/lab-agent", made, ""), l.asMac("GET", "/api/join/terms", third, ""),
		l.joinCheck(made, keyA), l.enrollWith(made, keyA), l.enrollWith(third, keyA),
	} {
		if w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("a refused token got %d %s", w.Code, w.Body)
		}
	}
	if got := since(l.labLines(), told); len(got) != 0 {
		t.Errorf("refused tokens were posted: %q", got)
	}
	if got := since(l.recorded(), recorded); len(got) != 0 {
		t.Errorf("refused tokens were recorded: %q", got)
	}
	if n := strings.Count(logs.String(), "refused an enrollment token"); n != 5 {
		t.Errorf("labd logged %d refused tokens, want 5:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), third) || strings.Contains(logs.String(), made) {
		t.Error("labd logged a token")
	}
}

// Blocked, cap reached and name taken are refused at enrollment with the
// reason, and at the Join command's test before that.
func TestEnrollmentIsRefusedWithTheReason(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	ravi.did("/people/mark", "handle", "edith", "as", "invite")
	grace, edith := l.signedIn(graceID), l.signedIn(edithID)

	refusedBoth := func(what, token string, code int, why string) {
		t.Helper()
		for step, w := range map[string]*httptest.ResponseRecorder{"the test": l.joinCheck(token, keyA), "enrollment": l.enrollWith(token, keyA)} {
			if w.Code != code || !strings.Contains(w.Body.String(), why) {
				t.Errorf("%s, %s: %d %q, want %d saying %q", what, step, w.Code, strings.TrimSpace(w.Body.String()), code, why)
			}
		}
		if l.s.dev("taken") != nil && l.s.dev("taken").snapshot().VolunteerID == edithID {
			t.Errorf("%s: a Mac enrolled", what)
		}
	}

	// Name taken, by a Lab-owned Mac that enrolled after the command was made.
	_, token := grace.makeJoin("taken")
	l.addMac("taken", 0)
	refusedBoth("name taken", token, http.StatusConflict, "Name taken")

	// Cap reached, by Macs that enrolled after the command was made.
	_, token = edith.makeJoin("edith-6")
	for i := 1; i <= lendCap; i++ {
		l.addMac(fmt.Sprintf("edith-%d", i), edithID)
	}
	refusedBoth("cap reached", token, http.StatusConflict, "Cap reached")

	// Blocked.
	_, token = grace.makeJoin("graces")
	if err := l.s.store.block(graceID, maintainerID, "", l.clock.Now()); err != nil {
		t.Fatal(err)
	}
	refusedBoth("Blocked", token, http.StatusForbidden, blockedSentence)
}

// The cap is 5: enrolled Macs and an unused Join command count, Macs that
// left do not; a Maintainer raises one person's cap; Maintainers have none.
func TestTheCapOnAPersonsMacs(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "edith", "as", "m-team")
	edith := l.signedIn(edithID)
	title := func(b *browser) string {
		return part(t, b.do("GET", "/lend", "").Body.String(), `<section class="card" id="lend">`, `</h2>`)
	}

	for i := 1; i <= 4; i++ {
		l.addMac(fmt.Sprintf("edith-%d", i), edithID)
	}
	if got := title(edith); !strings.Contains(got, "4 of 5") {
		t.Errorf("with 4 Macs: %s", got)
	}
	edith.makeJoin("edith-5")
	if got := title(edith); !strings.Contains(got, "5 of 5") {
		t.Errorf("with 4 Macs and an unused Join command: %s", got)
	}
	// A new one takes the old one's place, so the count stays at 5.
	edith.makeJoin("edith-five")
	if got := title(edith); !strings.Contains(got, "5 of 5") {
		t.Errorf("after a new Join command: %s", got)
	}
	l.addMac("edith-5", edithID)
	edith.refused("your cap", "/lend/join-command", "model", lendModelID, "name", "edith-6")
	if page := edith.do("GET", "/lend", "").Body.String(); strings.Contains(page, `action="/lend/join-command"`) || !strings.Contains(page, "You lend 5 Macs, which is your cap.") {
		t.Error("at the cap, the page still offers the join form")
	}

	// A Mac that left does not count.
	l.s.dev("edith-1").setState(api.StateLeft, "left")
	edith.makeJoin("edith-6")
	edith.did("/lend/join-command/cancel", "sure", "yes")
	l.addMac("edith-7", edithID)

	// A Maintainer raises one person's cap, on People; never lowers it.
	open := ravi.do("GET", "/people?open=7", "").Body.String()
	if !strings.Contains(open, "Cap: 5 of 5") || !strings.Contains(open, `action="/people/7/cap"`) {
		t.Error("People shows no cap control for the person")
	}
	ravi.refused("raise it to a number from 6", "/people/7/cap", "macs", "5")
	ravi.refused("a number of Macs", "/people/7/cap", "macs", "many")
	ravi.did("/people/7/cap", "macs", "7")
	edith.makeJoin("edith-8")
	if got := title(edith); !strings.Contains(got, "6 of 7") {
		t.Errorf("after the cap was raised: %s", got)
	}
	if got := l.recorded(); !strings.Contains(strings.Join(got, "\n"), "@ravi raised the cap to 7 Macs for @edith") {
		t.Errorf("the People record lacks the raised cap:\n%s", strings.Join(got, "\n"))
	}
	if got := strings.Join(l.labLines(), "\n"); !strings.Contains(got, "ravi raised a person's cap to 7 Macs. "+labAddress+"/people") || strings.Contains(got, "edith raised") {
		t.Errorf("the Lab group's lines lack the raised cap:\n%s", got)
	}

	// Maintainers have none.
	for i := 1; i <= 6; i++ {
		l.addMac(fmt.Sprintf("ravi-%d", i), maintainerID)
	}
	ravi.makeJoin("ravi-7")
	if got := title(ravi); !strings.Contains(got, "· 7</h2>") && !strings.Contains(got, "· 7") || strings.Contains(got, " of ") {
		t.Errorf("a Maintainer's count: %s", got)
	}
	ravi.refused("has no cap", "/people/3003/cap", "macs", "9")
}

// The suggested name is the Mac's model alone, numbered on a clash; a name
// is lowercase letters, digits and dashes, up to 31, unique in the lab, and
// the page says who sees it.
func TestNamingAMac(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	ravi.did("/people/mark", "handle", "edith", "as", "invite")
	grace, edith := l.signedIn(graceID), l.signedIn(edithID)

	page := grace.do("GET", "/lend?model="+lendModelID, "").Body.String()
	if !strings.Contains(page, `name="name" maxlength="31" pattern="[a-z0-9][a-z0-9-]{0,30}" value="`+lendSuggested+`"`) {
		t.Errorf("the suggested name is not the model alone:\n%s", part(t, page, `id="lend"`, `</section>`))
	}
	if strings.Contains(page, "grace-") {
		t.Error("the suggestion carries the handle")
	}
	for _, piece := range []string{"Maintainers, the M team, the Lab group on Signal and the notice service; never the public site", "There is no rename"} {
		if !strings.Contains(page, piece) {
			t.Errorf("the page does not say %q", piece)
		}
	}
	// Left empty, the lab uses its suggestion; numbered on a clash with a
	// Mac or with another person's unused Join command.
	if w := grace.post("/lend/join-command", "model", lendModelID, "name", ""); !strings.Contains(w.Body.String(), "the join command for "+lendSuggested+"<") {
		t.Errorf("an empty name did not take the suggestion: %d", w.Code)
	}
	if w := edith.post("/lend/join-command", "model", lendModelID); !strings.Contains(w.Body.String(), "the join command for "+lendSuggested+"-2<") {
		t.Errorf("the second Mac of the model is not numbered: %d %s", w.Code, refusalIn(w.Body.String()))
	}
	l.addMac(lendSuggested+"-3", 0)
	edith.makeJoin("edith-own")
	if page := edith.do("GET", "/lend?model="+lendModelID, "").Body.String(); !strings.Contains(page, `value="`+lendSuggested+`-2"`) {
		t.Error("a cancelled Join command still holds its name")
	}

	for _, bad := range []string{"Shelf-Mac", "-mac", "mac_1", "mäc", strings.Repeat("a", 32), "a b"} {
		edith.refused("lowercase letters, digits and dashes, up to 31", "/lend/join-command", "model", lendModelID, "name", bad)
	}
	edith.makeJoin(strings.Repeat("a", 31))
	edith.refused("names are unique", "/lend/join-command", "model", lendModelID, "name", lendSuggested+"-3")
	edith.refused("names are unique", "/lend/join-command", "model", lendModelID, "name", lendSuggested)
	edith.refused("from the list", "/lend/join-command", "model", "a-toaster", "name", "toaster")
}

// The retired ./volunteer's routes say so, naming the Lab address.
func TestTheVolunteerScriptsRoutesAreGone(t *testing.T) {
	l, _ := lendLab(t)
	for _, c := range [][2]string{{"POST", "/api/volunteer"}, {"GET", "/api/volunteer/lab-agent"}} {
		for who, bearer := range map[string]string{"nobody": "", "a Maintainer": "lab_adm_secret"} {
			if w := l.asMac(c[0], c[1], bearer, "{}"); w.Code != http.StatusGone || !strings.Contains(w.Body.String(), labAddress) || !strings.Contains(w.Body.String(), "Lend a Mac") {
				t.Errorf("%s %s by %s: %d %s", c[0], c[1], who, w.Code, w.Body)
			}
		}
	}
	if _, err := os.Stat(filepath.Join("..", "..", "volunteer")); err == nil {
		t.Error("the ./volunteer script is still in the repository")
	}
}

// lab enroll's token lasts 1 hour.
func TestLabEnrollsTokenLastsAnHour(t *testing.T) {
	l, _ := lendLab(t)
	w := l.asMac("POST", "/api/enroll-tokens", "lab_adm_secret", `{"name":"m2-max","model":"laptop"}`)
	var tok api.EnrollTokenResponse
	if json.Unmarshal(w.Body.Bytes(), &tok); w.Code != http.StatusOK || !tok.Expires.Equal(l.clock.Now().Add(time.Hour)) || tok.JoinCommand == "" {
		t.Fatalf("lab enroll: %d %s", w.Code, w.Body)
	}
	l.clock.Advance(61 * time.Minute)
	if w := l.enrollWith(tok.Token, keyA); w.Code != http.StatusForbidden {
		t.Errorf("after an hour: %d", w.Code)
	}
}

// The Join command tests its token before anything changes, fails once and
// names the case. Run by bash with a stand-in for curl that answers as the
// lab or its front door would.
func TestTheJoinCommandNamesWhyItWasRefused(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	line, _ := ravi.makeJoin("shelf-mac")
	run := newJoinRun(t, l.asMac("GET", "/join", "", "").Body.Bytes(), []byte("not the agent"), nil)
	for _, c := range []struct{ answer, says string }{
		{"401", "expired or was used: make a new one on Lend a Mac"},
		{"429", "Too many refused attempts from this network: wait 15 minutes"},
		{"403challenge", "challenged this request"},
		{"503", "did not answer as expected (503)"},
	} {
		out, reached := run.run(line, "CODE="+c.answer)
		if run.exit == 0 || reached || !strings.Contains(out, c.says) {
			t.Errorf("answered %s: exit %d, sudo reached %v, said %q, want %q", c.answer, run.exit, reached, out, c.says)
		}
		if n := strings.Count(run.read("curl.argv"), "/api/join/lab-agent\n"); n != 1 {
			t.Errorf("answered %s: the agent was asked for %d times, want once", c.answer, n)
		}
	}
}

// labJoin has a Maintainer's Lab token make the Join command for a Lab-owned
// Mac called name, as `lab enroll` does.
func (l *signInLab) labJoin(labToken, name string) string {
	l.t.Helper()
	w := l.asMac("POST", "/api/enroll-tokens", labToken, `{"name":"`+name+`","model":"desktop"}`)
	var tok api.EnrollTokenResponse
	if json.Unmarshal(w.Body.Bytes(), &tok); w.Code != http.StatusOK || tok.Token == "" {
		l.t.Fatalf("lab enroll %s: %d %s", name, w.Code, w.Body)
	}
	return tok.Token
}

// A Join command dies with its maker's access. A Maintainer whose access was
// ended, who is no longer a Maintainer or who was Blocked leaves no line
// that passes the check or enrolls a Mac, and each cancelled Join command
// has its line in the People record and for the Lab group.
func TestAJoinCommandDiesWithItsMakersAccess(t *testing.T) {
	for name, cut := range map[string]func(owner, ravi *browser){
		"End all access by a Maintainer": func(owner, _ *browser) {
			owner.did(personPath(maintainerID, "end-all-access"), "sure", "yes")
		},
		"End all access by the person": func(_, ravi *browser) {
			if w := ravi.form("/me/end-all-access", "sure", "yes"); w.Code != http.StatusSeeOther {
				t.Fatalf("End all access: %d %s", w.Code, w.Body)
			}
		},
		"no longer a Maintainer": func(owner, _ *browser) {
			owner.did(personPath(maintainerID, "maintainer/remove"), "sure", "yes")
		},
		"Blocked": func(owner, _ *browser) {
			owner.did(personPath(maintainerID, "block"), "sure", "yes")
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := lendLab(t)
			l.t = t
			owner, ravi := l.signedIn(ownerID), l.signedIn(maintainerID)
			token := l.labJoin(ravi.makeToken("laptop", "90"), "shelf-mini")
			records, lines := len(l.recorded()), len(l.labLines())
			cut(owner, ravi)
			if w := l.joinCheck(token, keyA); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
				t.Errorf("the kept line passes the check: %d %s", w.Code, w.Body)
			}
			if w := l.enrollWith(token, keyA); w.Code != http.StatusForbidden {
				t.Errorf("the kept line enrolls a Mac: %d %s", w.Code, w.Body)
			}
			if l.s.dev("shelf-mini") != nil {
				t.Errorf("a Mac called shelf-mini is in the lab")
			}
			if n := l.count(`SELECT count(*) FROM tokens WHERE name = 'shelf-mini'`); n != 0 {
				t.Errorf("%d tokens for shelf-mini are kept", n)
			}
			said := strings.Join(since(l.recorded(), records), "\n")
			if !strings.Contains(said, "@owner cancelled the Join command for shelf-mini made by @ravi") && !strings.Contains(said, "@ravi cancelled the Join command for shelf-mini") {
				t.Errorf("the People record has no line for the cancelled Join command:\n%s", said)
			}
			if got := strings.Join(since(l.labLines(), lines), "\n"); !strings.Contains(got, "The Join command for shelf-mini was cancelled.") {
				t.Errorf("the Lab group has no line for the cancelled Join command:\n%s", got)
			}
		})
	}
}

// "End all access" cancels the unused Join command a person made for a Mac
// of their own too.
func TestEndAllAccessCancelsAPersonsOwnJoinCommand(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	_, token := l.signedIn(graceID).makeJoin("shelf-mac")
	ravi.did(personPath(graceID, "end-all-access"), "sure", "yes")
	if w := l.enrollWith(token, keyA); w.Code != http.StatusForbidden {
		t.Errorf("after End all access the person's Join command enrolls a Mac: %d %s", w.Code, w.Body)
	}
	if got := l.recorded(); !strings.Contains(strings.Join(got, "\n"), "@ravi cancelled the Join command for shelf-mac made by @grace") {
		t.Errorf("the People record has no line for the cancelled Join command:\n%s", strings.Join(got, "\n"))
	}
}

// Whatever left a Lab-owned Mac's Join command behind, it enrolls no Mac
// once its maker is no longer a Maintainer.
func TestALabOwnedJoinCommandAsksAfterItsMaker(t *testing.T) {
	l, _ := lendLab(t)
	token := l.labJoin(l.signedIn(maintainerID).makeToken("laptop", "90"), "shelf-mini")
	if err := l.s.store.setMaintainer(maintainerID, false); err != nil {
		t.Fatal(err)
	}
	l.s.changed(maintainerID)
	for what, w := range map[string]*httptest.ResponseRecorder{"the check": l.joinCheck(token, keyA), "the enrollment": l.enrollWith(token, keyA)} {
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "no longer a Maintainer") {
			t.Errorf("%s: %d %s", what, w.Code, w.Body)
		}
	}
}
