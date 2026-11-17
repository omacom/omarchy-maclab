package server

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const formType = "application/x-www-form-urlencoded"

var reLabToken = regexp.MustCompile(`lab_[0-9a-f]{40}`)

// asToken asks labd through the tunnel with a Lab token, from address.
func (l *signInLab) asToken(token, method, target, address string) *httptest.ResponseRecorder {
	l.t.Helper()
	r := httptest.NewRequest(method, "http://"+labHost+target, nil)
	r = r.WithContext(onTunnel(r.Context()))
	r.RemoteAddr = "@"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("CF-Connecting-IP", address)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	l.h.ServeHTTP(w, r)
	return w
}

// form posts a form of the lab's own page.
func (b *browser) form(target string, fields ...string) *httptest.ResponseRecorder {
	b.l.t.Helper()
	v := url.Values{}
	for i := 0; i+1 < len(fields); i += 2 {
		v.Set(fields[i], fields[i+1])
	}
	return b.write("POST", target, v.Encode(), "Content-Type", formType)
}

// makeToken makes a Lab token on the person's own page and returns the
// secret the page shows.
func (b *browser) makeToken(name, period string) string {
	b.l.t.Helper()
	w := b.form("/me/tokens", "name", name, "period", period)
	found := reLabToken.FindAllString(w.Body.String(), -1)
	if w.Code != http.StatusOK || len(found) != 1 {
		b.l.t.Fatalf("make the Lab token %q: %d, and the page shows %d secrets:\n%s", name, w.Code, len(found), w.Body)
	}
	offersToCopy(b.l.t, w.Body.String(), "the Lab token", found[0])
	offersToCopy(b.l.t, w.Body.String(), "lab login", "lab login")
	return found[0]
}

// tokenID is the id of the person's Lab token named name.
func (l *signInLab) tokenID(account int64, name string) int64 {
	l.t.Helper()
	var id int64
	if err := l.s.store.db.QueryRow(`SELECT id FROM lab_tokens WHERE account_id = ? AND name = ?`, account, name).Scan(&id); err != nil {
		l.t.Fatalf("the Lab token %q of account %d: %v", name, account, err)
	}
	return id
}

// tokenRow is the row of the token named name on the own page b sees.
func (b *browser) tokenRow(name string) string {
	b.l.t.Helper()
	w := b.do("GET", "/me", "")
	if w.Code != http.StatusOK {
		b.l.t.Fatalf("GET /me: %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	i := strings.Index(body, `<td>`+name+`</td>`)
	if i < 0 {
		b.l.t.Fatalf("the own page lists no Lab token %q:\n%s", name, body)
	}
	return body[i : i+strings.Index(body[i:], "</tr>")]
}

// agentsTold reads the lines labd records for "Agents".
type agentsTold struct {
	l    *signInLab
	from int64
}

func (l *signInLab) toldAgents() *agentsTold { return &agentsTold{l: l} }

// lines are the lines recorded since the last forget, oldest first.
func (a *agentsTold) lines() []string {
	a.l.t.Helper()
	rows, err := a.l.s.store.db.Query(`SELECT text FROM lines WHERE channel = 'agents' AND id > ? ORDER BY id`, a.from)
	if err != nil {
		a.l.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			a.l.t.Fatal(err)
		}
		out = append(out, text)
	}
	return out
}

func (a *agentsTold) forget() {
	a.from = int64(a.l.count(`SELECT COALESCE(max(id), 0) FROM lines`))
}

// A person makes Lab tokens on their own page, up to 10. A token is shown
// once and stored as a hash; it has a name, when it was made, when it
// expires, and when and from which address it was last used.
func TestAPersonMakesUpToTenLabTokensEachShownOnce(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.signedIn(nobodyID)
	if w := b.do("GET", "/me", ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "0 of 10") {
		t.Fatalf("the own page of a person with no token: %d\n%s", w.Code, w.Body)
	}
	secret := b.makeToken("laptop", "90")
	if again := b.do("GET", "/me", "").Body.String(); strings.Contains(again, secret) || reLabToken.MatchString(again) {
		t.Error("the own page shows the secret a second time")
	}
	if disk := string(l.onDisk()); strings.Contains(disk, secret) || !strings.Contains(disk, hashSecret(secret)) {
		t.Error("labd.db holds the secret, or not its hash")
	}
	row := b.tokenRow("laptop")
	for _, want := range []string{"2026-11-14", "2027-02-12", "never used"} {
		if !strings.Contains(row, want) {
			t.Errorf("a new token's row lacks %q:\n%s", want, row)
		}
	}
	l.clock.Advance(2 * time.Hour)
	if w := l.asToken(secret, "GET", "/api/auth", "198.51.100.7"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"login":"nobody-yet"`) {
		t.Fatalf("the token names its person: %d %s", w.Code, w.Body)
	}
	row = b.tokenRow("laptop")
	for _, want := range []string{"2026-11-14 14:00 UTC", "198.51.100.7"} {
		if !strings.Contains(row, want) {
			t.Errorf("a used token's row lacks %q:\n%s", want, row)
		}
	}
	for i := 2; i <= 10; i++ {
		b.makeToken(fmt.Sprintf("agent-%d", i), "30")
	}
	if body := b.do("GET", "/me", "").Body.String(); !strings.Contains(body, "10 of 10") {
		t.Errorf("the own page does not say 10 of 10:\n%s", body)
	}
	w := b.form("/me/tokens", "name", "one-too-many", "period", "30")
	if w.Code != http.StatusConflict || reLabToken.MatchString(w.Body.String()) || !strings.Contains(w.Body.String(), "10 Lab tokens") {
		t.Errorf("the eleventh token: %d\n%s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, nobodyID); n != 10 {
		t.Errorf("%d tokens stored, want 10", n)
	}
	// Another person's ten are their own.
	if other := l.signedIn(maintainerID); !reLabToken.MatchString(other.form("/me/tokens", "name", "mine", "period", "never").Body.String()) {
		t.Error("another person could not make a token while the first has ten")
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 65), "two\nlines"} {
		if w := l.signedIn(ownerID).form("/me/tokens", "name", bad, "period", "90"); w.Code != http.StatusBadRequest || reLabToken.MatchString(w.Body.String()) {
			t.Errorf("a token named %q: %d", bad, w.Code)
		}
	}
	if w := l.signedIn(ownerID).form("/me/tokens", "name", "odd", "period", "7"); w.Code != http.StatusBadRequest {
		t.Errorf("a period of 7 days: %d", w.Code)
	}
}

// The period is 30 days, 90 days (the default), 1 year or never, counted
// from the last use: every use pushes the date out. An expired token stays
// listed, and Renew makes the same secret work again.
func TestALabTokenExpiresUnusedAndRenewMakesTheSameSecretWorkAgain(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.signedIn(ownerID)
	day := 24 * time.Hour
	made := l.clock.Now()
	tokens := map[string]string{}
	for _, period := range []string{"30", "90", "365", "never", ""} {
		tokens[period] = b.makeToken("period-"+period, period)
	}
	for period, days := range map[string]int{"30": 30, "90": 90, "365": 365, "": 90} {
		var expires int64
		l.s.store.db.QueryRow(`SELECT expires FROM lab_tokens WHERE name = ?`, "period-"+period).Scan(&expires)
		if got := time.UnixMicro(expires).Sub(made); got != time.Duration(days)*day {
			t.Errorf("a token made for %q expires after %s, want %d days", period, got, days)
		}
	}
	if n := l.count(`SELECT count(*) FROM lab_tokens WHERE name = 'period-never' AND expires IS NULL`); n != 1 {
		t.Error("a token made for never has a date")
	}
	use := func(period string) int { return l.asToken(tokens[period], "GET", "/api/devices", "198.51.100.7").Code }

	// Used on day 20 and day 45, the 30-day token lives to day 75.
	for _, d := range []int{20, 25} {
		l.clock.Advance(time.Duration(d) * day)
		if code := use("30"); code != http.StatusOK {
			t.Fatalf("a token used within its period: %d", code)
		}
	}
	l.clock.Advance(29 * day) // day 74
	if code := use("30"); code != http.StatusOK {
		t.Fatalf("29 days after its last use: %d", code)
	}
	if code := use("never"); code != http.StatusOK {
		t.Errorf("a token that never expires, unused for 74 days: %d", code)
	}
	l.clock.Advance(30 * day) // day 104: 30 days unused
	w := l.asToken(tokens["30"], "GET", "/api/devices", "198.51.100.7")
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "expired") || !strings.Contains(w.Body.String(), labAddress+"/me") {
		t.Fatalf("30 days unused: %d %s", w.Code, w.Body)
	}
	if w := l.asToken(tokens["90"], "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusUnauthorized {
		t.Errorf("the 90-day token, never used, on day 104: %d", w.Code)
	}
	if code := use("365"); code != http.StatusOK {
		t.Errorf("the 1-year token on day 104: %d", code)
	}
	// A refused use pushes nothing out and is not a use.
	l.clock.Advance(day)
	if code := use("30"); code != http.StatusUnauthorized {
		t.Errorf("an expired token, tried again: %d", code)
	}
	b = l.signedIn(ownerID) // the browser's own 30 days ran out on the way
	row := b.tokenRow("period-30")
	if !strings.Contains(row, "expired") || !strings.Contains(row, "/me/tokens/"+fmt.Sprint(l.tokenID(ownerID, "period-30"))+"/renew") {
		t.Fatalf("an expired token's row does not say so, or has no Renew:\n%s", row)
	}
	if live := b.tokenRow("period-365"); strings.Contains(live, "/renew") || strings.Contains(live, "expired") {
		t.Errorf("a token in use offers Renew:\n%s", live)
	}
	// Only its person renews it.
	renew := fmt.Sprintf("/me/tokens/%d/renew", l.tokenID(ownerID, "period-30"))
	if w := l.signedIn(maintainerID).form(renew); w.Code != http.StatusNotFound {
		t.Errorf("another person renewing it: %d", w.Code)
	}
	if code := use("30"); code != http.StatusUnauthorized {
		t.Errorf("after another person's Renew: %d", code)
	}
	if w := b.form(renew); w.Code != http.StatusSeeOther {
		t.Fatalf("Renew: %d %s", w.Code, w.Body)
	}
	if code := use("30"); code != http.StatusOK {
		t.Fatalf("the same secret after Renew: %d", code)
	}
	if n := l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, ownerID); n != 5 {
		t.Errorf("%d tokens after a Renew, want the same 5", n)
	}
}

// The address a token was last used from is replaced at the next use and
// gone when the token is revoked.
func TestTheLastUseAddressIsReplacedAtTheNextUseAndGoneAtRevoke(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.signedIn(ownerID)
	secret := b.makeToken("laptop", "90")
	l.asToken(secret, "GET", "/api/devices", "198.51.100.7")
	if row := b.tokenRow("laptop"); !strings.Contains(row, "198.51.100.7") {
		t.Fatalf("the row after a use:\n%s", row)
	}
	l.asToken(secret, "GET", "/api/devices", "2001:db8::42")
	if row := b.tokenRow("laptop"); !strings.Contains(row, "2001:db8::42") || strings.Contains(row, "198.51.100.7") {
		t.Errorf("the row after the next use, from another address:\n%s", row)
	}
	if n := l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ? AND last_used_address != '2001:db8::42'`, ownerID); n != 0 {
		t.Error("the row keeps another address than that of its last use")
	}
	id := l.tokenID(ownerID, "laptop")
	first := b.form(fmt.Sprintf("/me/tokens/%d/revoke", id))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `Revoke “laptop”`) || l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, ownerID) != 1 {
		t.Fatalf("the first click on Revoke: %d\n%s", first.Code, first.Body)
	}
	if w := b.form(fmt.Sprintf("/me/tokens/%d/revoke", id), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("Revoke: %d %s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ? OR hash = ?`, ownerID, hashSecret(secret)); n != 0 {
		t.Error("a revoked token's row, with its address and hash, is still there")
	}
	if body := b.do("GET", "/me", "").Body.String(); strings.Contains(body, "<td>laptop</td>") || !strings.Contains(body, "0 of 10") {
		t.Errorf("a revoked token is still listed:\n%s", body)
	}
}

// A token does only what its person may do at that moment, and opens no
// page: it cannot be turned into a browser's session or another token.
func TestALabTokenDoesOnlyWhatItsPersonMayDoAtThatMoment(t *testing.T) {
	l := pagesLab(t, nil)
	secret := l.signedIn(maintainerID).makeToken("ci", "never")
	get := func() *httptest.ResponseRecorder { return l.asToken(secret, "GET", "/api/devices", "198.51.100.7") }
	if w := get(); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "@ravi") {
		t.Fatalf("the token of a person with no role on a Maintainer's route: %d %s", w.Code, w.Body)
	}
	if err := l.s.setMaintainer(maintainerID, true); err != nil {
		t.Fatal(err)
	}
	if w := get(); w.Code != http.StatusOK {
		t.Fatalf("its person made a Maintainer, the next request: %d %s", w.Code, w.Body)
	}
	if w := l.asToken(secret, "POST", "/api/enroll-tokens", "198.51.100.7"); w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Errorf("a Maintainer's token on a Maintainer's write: %d %s", w.Code, w.Body)
	}
	if err := l.s.setMaintainer(maintainerID, false); err != nil {
		t.Fatal(err)
	}
	if w := get(); w.Code != http.StatusForbidden {
		t.Errorf("no longer a Maintainer, the next request: %d", w.Code)
	}
	admin := l.signedIn(ownerID).makeToken("owner", "90")
	if w := l.asToken(admin, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusOK {
		t.Errorf("an Admin's token: %d %s", w.Code, w.Body)
	}
	for _, target := range []string{"/me", "/test-only/maintainers"} {
		if w := l.asToken(admin, "GET", target, "198.51.100.7"); w.Code != http.StatusSeeOther {
			t.Errorf("a token on the page %s: %d", target, w.Code)
		}
	}
	w := l.asToken(admin, "POST", "/me/tokens?name=more&period=never", "198.51.100.7")
	if reLabToken.MatchString(w.Body.String()) || l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, ownerID) != 1 {
		t.Errorf("a token made another token: %d %s", w.Code, w.Body)
	}
	for _, other := range []string{"lab_" + strings.Repeat("0", 40), "lab_", secret + "0", strings.TrimPrefix(secret, "lab_")} {
		if w := l.asToken(other, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "not a Lab token") {
			t.Errorf("%q on a Maintainer's route: %d %s", other, w.Code, w.Body)
		}
	}
}

// stream opens /api/stream with a Lab token or a browser's session and
// reports on ended when labd closes it.
func (l *signInLab) stream(token string, session *http.Cookie) (ended chan error) {
	l.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
		l.h.ServeHTTP(w, r.WithContext(onTunnel(r.Context())))
	}))
	l.t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Host = labHost
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != nil {
		req.AddCookie(session)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { res.Body.Close() })
	rd := bufio.NewReader(res.Body)
	if line, err := rd.ReadString('\n'); err != nil || res.StatusCode != http.StatusOK {
		l.t.Fatalf("the stream: %d %q %v", res.StatusCode, line, err)
	}
	ended = make(chan error, 1)
	go func() {
		for {
			if _, err := rd.ReadString('\n'); err != nil {
				ended <- err
				return
			}
		}
	}()
	return ended
}

func open(ended chan error) bool {
	select {
	case <-ended:
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

func closes(ended chan error) bool {
	select {
	case <-ended:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// The three cuts, smallest first. Revoking one token stops its next request
// and ends its open stream, and nothing else of the person's.
func TestRevokeStopsTheNextRequestAndEndsAnOpenStream(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.signedIn(ownerID)
	stolen, kept := b.makeToken("stolen", "90"), b.makeToken("kept", "90")
	stolenStream, keptStream, browserStream := l.stream(stolen, nil), l.stream(kept, nil), l.stream("", b.session())
	if w := b.form(fmt.Sprintf("/me/tokens/%d/revoke", l.tokenID(ownerID, "stolen")), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("Revoke: %d %s", w.Code, w.Body)
	}
	if !closes(stolenStream) {
		t.Error("the revoked token's stream stayed open")
	}
	if w := l.asToken(stolen, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusUnauthorized {
		t.Errorf("the revoked token's next request: %d", w.Code)
	}
	if !open(keptStream) || !open(browserStream) {
		t.Error("revoking one token ended another token's stream, or the browser's")
	}
	if w := l.asToken(kept, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusOK {
		t.Errorf("the person's other token: %d", w.Code)
	}
	if w := b.do("GET", "/me", ""); w.Code != http.StatusOK {
		t.Errorf("the person's browser: %d", w.Code)
	}
	// Nobody revokes another person's token from their own page.
	other := l.signedIn(maintainerID)
	if w := other.form(fmt.Sprintf("/me/tokens/%d/revoke", l.tokenID(ownerID, "kept")), "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("revoking another person's token from one's own page: %d", w.Code)
	}
	if w := l.asToken(kept, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusOK {
		t.Errorf("the token after that: %d", w.Code)
	}
}

// "Sign out everywhere" is browsers only.
func TestSignOutEverywhereEndsBrowsersOnly(t *testing.T) {
	l := pagesLab(t, nil)
	here, there := l.signedIn(ownerID), l.signedIn(ownerID)
	someoneElse := l.signedIn(maintainerID)
	secret := here.makeToken("laptop", "90")
	tokenStream, thereStream := l.stream(secret, nil), l.stream("", there.session())
	if body := here.do("GET", "/me", "").Body.String(); !strings.Contains(body, "2 browsers") {
		t.Errorf("the own page does not count 2 browsers:\n%s", body)
	}
	if w := here.form("/me/sign-out-everywhere"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Sign out everywhere: 2 browsers") || l.count(`SELECT count(*) FROM browser_sessions WHERE account_id = ?`, ownerID) != 2 {
		t.Fatalf("the first click: %d\n%s", w.Code, w.Body)
	}
	w := here.form("/me/sign-out-everywhere", "sure", "yes")
	if w.Code != http.StatusSeeOther || here.session() != nil {
		t.Fatalf("Sign out everywhere: %d, this browser's session: %v", w.Code, here.session())
	}
	if !closes(thereStream) {
		t.Error("the other browser's stream stayed open")
	}
	for name, b := range map[string]*browser{"this browser": here, "the other browser": there} {
		if w := b.do("GET", "/me", ""); w.Code != http.StatusSeeOther {
			t.Errorf("%s after Sign out everywhere: %d", name, w.Code)
		}
	}
	if n := l.count(`SELECT count(*) FROM browser_sessions WHERE account_id = ?`, ownerID); n != 0 {
		t.Errorf("%d sessions left", n)
	}
	if !open(tokenStream) {
		t.Error("Sign out everywhere ended a Lab token's stream")
	}
	if w := l.asToken(secret, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusOK {
		t.Errorf("the Lab token after Sign out everywhere: %d", w.Code)
	}
	if w := someoneElse.do("GET", "/me", ""); w.Code != http.StatusOK {
		t.Errorf("another person's browser: %d", w.Code)
	}
}

// "End all access" ends every browser session and every Lab token of the
// person. The person stays and signs in again.
func TestEndAllAccessEndsEveryBrowserSessionAndLabToken(t *testing.T) {
	l := pagesLab(t, nil)
	here, there := l.signedIn(ownerID), l.signedIn(ownerID)
	someoneElse := l.signedIn(maintainerID)
	theirs := someoneElse.makeToken("theirs", "90")
	one, two := here.makeToken("one", "90"), here.makeToken("two", "never")
	streams := []chan error{l.stream(one, nil), l.stream(two, nil), l.stream("", there.session())}
	if w := here.form("/me/end-all-access"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "End all access: 2 browsers and 2 Lab tokens stop now") {
		t.Fatalf("the first click: %d\n%s", w.Code, w.Body)
	}
	if w := here.form("/me/end-all-access", "sure", "yes"); w.Code != http.StatusSeeOther || here.session() != nil {
		t.Fatalf("End all access: %d", w.Code)
	}
	for i, s := range streams {
		if !closes(s) {
			t.Errorf("stream %d stayed open", i)
		}
	}
	for _, secret := range []string{one, two} {
		if w := l.asToken(secret, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusUnauthorized {
			t.Errorf("a Lab token after End all access: %d", w.Code)
		}
	}
	if w := there.do("GET", "/me", ""); w.Code != http.StatusSeeOther {
		t.Errorf("the other browser after End all access: %d", w.Code)
	}
	if n := l.count(`SELECT (SELECT count(*) FROM browser_sessions WHERE account_id = ?1) + (SELECT count(*) FROM lab_tokens WHERE account_id = ?1)`, ownerID); n != 0 {
		t.Errorf("%d sessions and tokens left", n)
	}
	if w := l.asToken(theirs, "GET", "/api/auth", "198.51.100.7"); w.Code != http.StatusOK || someoneElse.do("GET", "/me", "").Code != http.StatusOK {
		t.Error("End all access reached another person")
	}
	again := l.signedIn(ownerID)
	if me := again.whoami(); me.Kind != "Admin" || me.AccountID != ownerID {
		t.Errorf("signing in again after End all access: %+v", me)
	}
}

// A Maintainer makes the cuts for another person: any Maintainer revokes a
// token; "End all access" on a Maintainer takes an Admin.
func TestAMaintainerCutsForAnotherPerson(t *testing.T) {
	l := pagesLab(t, nil)
	l.gh.AddUser(5005, "mona")
	owner, ravi, nobody, mona := l.signedIn(ownerID), l.signedIn(maintainerID), l.signedIn(nobodyID), l.signedIn(5005)
	for _, id := range []int64{maintainerID, 5005} {
		if err := l.s.setMaintainer(id, true); err != nil {
			t.Fatal(err)
		}
	}
	stolen := nobody.makeToken("stolen", "90")
	revoke := fmt.Sprintf("/people/%d/tokens/%d/revoke", nobodyID, l.tokenID(nobodyID, "stolen"))
	if w := nobody.form(revoke, "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("a person with no role on a Maintainer's cut: %d", w.Code)
	}
	if w := ravi.form(revoke); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Revoke “stolen” of @nobody-yet") {
		t.Fatalf("the first click: %d\n%s", w.Code, w.Body)
	}
	if w := ravi.form(revoke, "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("a Maintainer revoking a person's token: %d %s", w.Code, w.Body)
	}
	if w := l.asToken(stolen, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusUnauthorized {
		t.Errorf("the token a Maintainer revoked: %d", w.Code)
	}
	if w := ravi.form(fmt.Sprintf("/people/%d/tokens/%d/revoke", ownerID, 999), "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("revoking a token that is not there: %d", w.Code)
	}
	// Any Maintainer revokes a Maintainer's or an Admin's token.
	ownersToken := owner.makeToken("owners", "90")
	if w := ravi.form(fmt.Sprintf("/people/%d/tokens/%d/revoke", ownerID, l.tokenID(ownerID, "owners")), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Errorf("a Maintainer revoking an Admin's token: %d", w.Code)
	}
	if w := l.asToken(ownersToken, "GET", "/api/devices", "198.51.100.7"); w.Code != http.StatusUnauthorized {
		t.Errorf("the Admin's token a Maintainer revoked: %d", w.Code)
	}

	nobody.makeToken("another", "90")
	if w := ravi.form(fmt.Sprintf("/people/%d/end-all-access", nobodyID), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatalf("a Maintainer ending all access of a person: %d %s", w.Code, w.Body)
	}
	if w := nobody.do("GET", "/me", ""); w.Code != http.StatusSeeOther || l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, nobodyID) != 0 {
		t.Errorf("the person after a Maintainer's End all access: %d", w.Code)
	}
	for who, target := range map[string]int64{"another Maintainer": 5005, "an Admin": ownerID} {
		if w := ravi.form(fmt.Sprintf("/people/%d/end-all-access", target), "sure", "yes"); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Admin") {
			t.Errorf("a Maintainer ending all access of %s: %d %s", who, w.Code, w.Body)
		}
	}
	if w := mona.do("GET", "/me", ""); w.Code != http.StatusOK {
		t.Errorf("the Maintainer a Maintainer could not cut: %d", w.Code)
	}
	if w := owner.form("/people/5005/end-all-access", "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Errorf("an Admin ending all access of a Maintainer: %d %s", w.Code, w.Body)
	}
	if w := mona.do("GET", "/me", ""); w.Code != http.StatusSeeOther {
		t.Errorf("the Maintainer an Admin cut: %d", w.Code)
	}
	if w := owner.form("/people/424242/end-all-access", "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("End all access on nobody the lab knows: %d", w.Code)
	}
}

// Each new token, and each cut a Maintainer makes for another person, has
// its line for "Agents"; a person's own cuts have none. Every action writes
// a line of the People record.
func TestEachNewTokenAndEachMaintainersCutHasItsLineForAgents(t *testing.T) {
	l := pagesLab(t, nil)
	told := l.toldAgents()
	owner, nobody := l.signedIn(ownerID), l.signedIn(nobodyID)
	nobody.makeToken("laptop", "90")
	nobody.makeToken("ci", "never")
	if want := []string{
		`nobody-yet made the Lab token "laptop", which expires on 2027-02-12.`,
		`nobody-yet made the Lab token "ci", which never expires.`,
	}; strings.Join(told.lines(), "\n") != strings.Join(want, "\n") {
		t.Fatalf("told of two new tokens:\n%s", strings.Join(told.lines(), "\n"))
	}
	told.forget()
	// A person's own cuts are theirs to make: no line.
	if w := nobody.form(fmt.Sprintf("/me/tokens/%d/revoke", l.tokenID(nobodyID, "ci")), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	spare := l.signedIn(nobodyID)
	spare.form("/me/sign-out-everywhere", "sure", "yes")
	nobody = l.signedIn(nobodyID)
	nobody.form("/me/end-all-access", "sure", "yes")
	if len(told.lines()) != 0 {
		t.Errorf("told of a person's own cuts: %q", told.lines())
	}
	nobody = l.signedIn(nobodyID)
	nobody.makeToken("phone", "30")
	told.forget()
	if w := owner.form(fmt.Sprintf("/people/%d/tokens/%d/revoke", nobodyID, l.tokenID(nobodyID, "phone")), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	if w := owner.form(fmt.Sprintf("/people/%d/end-all-access", nobodyID), "sure", "yes"); w.Code != http.StatusSeeOther {
		t.Fatal(w.Code)
	}
	if want := []string{`owner revoked nobody-yet's Lab token "phone".`, `owner used "End all access" on nobody-yet.`}; strings.Join(told.lines(), "\n") != strings.Join(want, "\n") {
		t.Errorf("told of a Maintainer's two cuts:\n%s", strings.Join(told.lines(), "\n"))
	}
	// A Maintainer's cut of their own token, from People, is still their own.
	owner.makeToken("mine", "90")
	told.forget()
	if w := owner.form(fmt.Sprintf("/people/%d/tokens/%d/revoke", ownerID, l.tokenID(ownerID, "mine")), "sure", "yes"); w.Code != http.StatusSeeOther || len(told.lines()) != 0 {
		t.Errorf("a Maintainer revoking their own token from People: %d, told %q", w.Code, told.lines())
	}
	lines, err := l.s.store.recordLines(20)
	if err != nil {
		t.Fatal(err)
	}
	var record []string
	for i := len(lines) - 1; i >= 0; i-- {
		line := "@" + lines[i].Actor + " " + lines[i].What
		if lines[i].About != "" {
			line += " @" + lines[i].About
		}
		record = append(record, line)
	}
	if want := []string{
		`@nobody-yet made the Lab token "laptop"`,
		`@nobody-yet made the Lab token "ci"`,
		`@nobody-yet revoked their Lab token "ci"`,
		`@nobody-yet signed out everywhere`,
		`@nobody-yet used "End all access"`,
		`@nobody-yet made the Lab token "phone"`,
		`@owner revoked the Lab token "phone" of @nobody-yet`,
		`@owner used "End all access" on @nobody-yet`,
		`@owner made the Lab token "mine"`,
		`@owner revoked the Lab token "mine" of @owner`,
	}; strings.Join(record, "\n") != strings.Join(want, "\n") {
		t.Errorf("the People record:\n%s", strings.Join(record, "\n"))
	}
}

// Break-glass mints a Lab token on the server itself, for a named person,
// by writing to labd's own data: no route of labd's does it.
func TestBreakGlassMintsOnTheServerForANamedPerson(t *testing.T) {
	l := pagesLab(t, nil)
	told := l.toldAgents()
	l.signedIn(nobodyID)
	now := l.clock.Now()
	if _, err := BreakGlass(l.dir, "who-is-this", 0, now); err == nil || !strings.Contains(err.Error(), "--account-id") {
		t.Errorf("break-glass for a handle the lab does not know: %v", err)
	}
	if _, err := BreakGlass(filepath.Join(l.dir, "nowhere"), "owner", ownerID, now); err == nil {
		t.Error("break-glass made a data directory")
	}
	if _, err := BreakGlass(l.dir, "someone-else", nobodyID, now); err == nil || !strings.Contains(err.Error(), "@nobody-yet") {
		t.Errorf("break-glass for an account id under another handle: %v", err)
	}
	// The owner has never signed in: the lab learns the person here.
	minted, err := BreakGlass(l.dir, "@owner", ownerID, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reLabToken.MatchString(minted.Token) || minted.Handle != "owner" || minted.AccountID != ownerID || !minted.Expires.Equal(now.Add(30*24*time.Hour)) || !strings.HasPrefix(minted.Name, "break-glass") {
		t.Fatalf("minted %+v", minted)
	}
	if disk := string(l.onDisk()); strings.Contains(disk, minted.Token) || !strings.Contains(disk, hashSecret(minted.Token)) {
		t.Error("labd.db holds the break-glass secret, or not its hash")
	}
	if len(told.lines()) != 0 {
		t.Fatalf("told before the token was used: %q", told.lines())
	}
	// It is used against the loopback listener, and is an Admin's there
	// because the configuration names the account.
	ask := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://"+loopback+"/api/devices", nil)
		r.RemoteAddr = direct
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		l.h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 2; i++ {
		if w := ask(minted.Token); w.Code != http.StatusOK {
			t.Fatalf("the break-glass token at the loopback listener: %d %s", w.Code, w.Body)
		}
	}
	if want := `owner made the Lab token "` + minted.Name + `", which expires on 2026-12-14.`; len(told.lines()) != 1 || told.lines()[0] != want {
		t.Errorf("told of the break-glass token, once, at its first use: %q", told.lines())
	}
	if lines, _ := l.s.store.recordLines(1); len(lines) != 1 || lines[0].Actor != "owner" || !strings.Contains(lines[0].What, "break-glass") {
		t.Errorf("the People record of a break-glass token: %+v", lines)
	}
	row := l.signedIn(ownerID).tokenRow(minted.Name)
	if !strings.Contains(row, "127.0.0.1") {
		t.Errorf("the break-glass token on the owner's own page:\n%s", row)
	}
	// By handle alone, for a person the lab knows; with no role it opens nothing.
	second, err := BreakGlass(l.dir, "Nobody-Yet", 0, now)
	if err != nil || second.AccountID != nobodyID {
		t.Fatalf("break-glass by handle: %+v %v", second, err)
	}
	if w := ask(second.Token); w.Code != http.StatusForbidden {
		t.Errorf("a break-glass token for a person with no role: %d", w.Code)
	}
	// It counts among the person's ten.
	for i := 0; i < 8; i++ {
		if _, err := BreakGlass(l.dir, "owner", 0, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := BreakGlass(l.dir, "owner", 0, now); err != nil {
		t.Fatalf("the owner's tenth token: %v", err)
	}
	if _, err := BreakGlass(l.dir, "owner", 0, now); err == nil || !strings.Contains(err.Error(), "10 Lab tokens") {
		t.Errorf("the owner's eleventh token: %v", err)
	}
	// No route mints for a named person.
	for _, target := range []string{"/api/break-glass", "/api/lab-tokens", "/api/tokens", "/break-glass"} {
		r := httptest.NewRequest("POST", "http://"+loopback+target, strings.NewReader(`{"handle":"owner"}`))
		r.RemoteAddr = direct
		r.Header.Set("Authorization", "Bearer "+minted.Token)
		w := httptest.NewRecorder()
		l.h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", target, w.Code)
		}
	}
}

// An admin.token on disk is ignored, labd makes none, and a Maintainer
// token of maclab's opens nothing.
func TestAnAdminTokenOnDiskAndMaclabsMaintainerTokensOpenNothing(t *testing.T) {
	var dir string
	const old = "mnt_00112233445566778899aabbccddeeff00112233"
	l := newSignInLab(t, func(c *Config) {
		dir = c.DataDir
		if err := os.WriteFile(filepath.Join(dir, "admin.token"), []byte(old), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	if _, err := l.s.store.db.Exec(`INSERT INTO maintainers(name, token_hash, added, added_by) VALUES ('chris', ?, 1, '')`, hashSecret(old)); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/api/devices", "/api/jobs", "/api/auth"} {
		if w := l.asToken(old, "GET", target, "198.51.100.7"); w.Code != http.StatusUnauthorized && w.Code != http.StatusNotFound {
			t.Errorf("maclab's Maintainer token on %s: %d %s", target, w.Code, w.Body)
		}
	}
	for _, method := range []string{"GET", "POST"} {
		if w := l.signedIn(ownerID).write(method, "/api/maintainers", `{"name":"new"}`); w.Code != http.StatusGone || strings.Contains(w.Body.String(), "mnt_") {
			t.Errorf("%s /api/maintainers as an Admin: %d %s", method, w.Code, w.Body)
		}
	}
	fresh := labServer(t, labAddress, nil)
	if _, err := os.Stat(filepath.Join(fresh.cfg.DataDir, "admin.token")); !os.IsNotExist(err) {
		t.Errorf("a fresh labd made an admin.token: %v", err)
	}
	if n := -1; fresh.store.db.QueryRow(`SELECT count(*) FROM maintainers`).Scan(&n) != nil || n != 0 {
		t.Errorf("a fresh labd has %d of maclab's Maintainers", n)
	}
}

// held is a request body the browser holds back: nothing of it arrives
// until send, and asked closes when labd first reads for it.
type held struct {
	body  io.Reader
	asked chan struct{}
	once  sync.Once
	send  chan struct{}
}

func (h *held) Read(p []byte) (int, error) {
	h.once.Do(func() { close(h.asked) })
	<-h.send
	return h.body.Read(p)
}

// heldForm starts a form of the lab's own page whose body the browser holds
// back, waits until labd has let the request in and reads for the body, and
// returns what sends the body and gives labd's answer.
func (b *browser) heldForm(target string, fields ...string) (send func() *httptest.ResponseRecorder) {
	b.l.t.Helper()
	v := url.Values{}
	for i := 0; i+1 < len(fields); i += 2 {
		v.Set(fields[i], fields[i+1])
	}
	return b.heldPost(target, formType, v.Encode())
}

// heldPost is heldForm for a body of any content type.
func (b *browser) heldPost(target, contentType, body string) (send func() *httptest.ResponseRecorder) {
	b.l.t.Helper()
	h := &held{body: strings.NewReader(body), asked: make(chan struct{}), send: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://"+labHost+target, h)
	r = r.WithContext(onTunnel(r.Context()))
	r.RemoteAddr = "@"
	for name, value := range map[string]string{"X-Forwarded-Proto": "https", "CF-Connecting-IP": "203.0.113.9", "User-Agent": "TestBrowser/9.75",
		"Origin": labAddress, "X-CSRF-Token": b.csrf(), "Content-Type": contentType} {
		r.Header.Set(name, value)
	}
	r.AddCookie(b.session())
	return b.l.holding(r, h)
}

// heldAsToken is heldPost for a request with a Lab token, as lab sends it.
func (l *signInLab) heldAsToken(token, target, body string) (send func() *httptest.ResponseRecorder) {
	l.t.Helper()
	h := &held{body: strings.NewReader(body), asked: make(chan struct{}), send: make(chan struct{})}
	r := httptest.NewRequest("POST", "http://"+labHost+target, h)
	r = r.WithContext(onTunnel(r.Context()))
	r.RemoteAddr = "@"
	for name, value := range map[string]string{"X-Forwarded-Proto": "https", "CF-Connecting-IP": "198.51.100.7", "User-Agent": "lab/0.2.0",
		"Authorization": "Bearer " + token, "Content-Type": "application/json"} {
		r.Header.Set(name, value)
	}
	return l.holding(r, h)
}

// holding starts r, whose body is h, and waits until labd reads for it.
func (l *signInLab) holding(r *http.Request, h *held) (send func() *httptest.ResponseRecorder) {
	l.t.Helper()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.h.ServeHTTP(w, r)
	}()
	select {
	case <-h.asked:
	case <-done:
		l.t.Fatalf("labd answered %s before its body came: %d %s", r.URL.Path, w.Code, w.Body)
	case <-time.After(10 * time.Second):
		l.t.Fatalf("labd never read for the body of %s", r.URL.Path)
	}
	return func() *httptest.ResponseRecorder {
		close(h.send)
		<-done
		return w
	}
}

// A Lab token's body is bound to the token as a browser's is to its session:
// whoever holds a stolen token and keeps `lab enroll` open, its body unsent,
// makes no Join command by sending the body after the token's person was
// cut off or lost the role the request takes.
func TestALabTokenBodyHeldOpenAcrossACutChangesNothing(t *testing.T) {
	for name, cut := range map[string]func(owner *browser){
		"End all access": func(owner *browser) {
			owner.did(personPath(maintainerID, "end-all-access"), "sure", "yes")
		},
		"Blocked": func(owner *browser) {
			owner.did(personPath(maintainerID, "block"), "sure", "yes")
		},
		"no longer a Maintainer": func(owner *browser) {
			owner.did(personPath(maintainerID, "maintainer/remove"), "sure", "yes")
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := lendLab(t)
			l.t = t
			owner := l.signedIn(ownerID)
			token := l.signedIn(maintainerID).makeToken("stolen", "90")
			send := l.heldAsToken(token, "/api/enroll-tokens", `{"name":"held-mini","model":"desktop"}`)
			cut(owner)
			if w := send(); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "enr_") {
				t.Errorf("a token's body sent after the cut was taken: %d %s", w.Code, w.Body)
			}
			if n := l.count(`SELECT count(*) FROM tokens WHERE name = 'held-mini'`); n != 0 {
				t.Errorf("a token's body sent after the cut made %d Join commands", n)
			}
		})
	}
}

// A body cut on its way is answered with why: a Block with the Blocked
// sentence and nothing else, as everywhere a Blocked person meets the lab;
// a revoked or ended credential with 401; a lost role with 403.
func TestABodyCutOnItsWayIsAnsweredWithWhy(t *testing.T) {
	for name, c := range map[string]struct {
		cut   string
		token bool
		code  int
	}{
		"Lab token, Blocked":                {"block", true, http.StatusForbidden},
		"Lab token, End all access":         {"end-all-access", true, http.StatusUnauthorized},
		"Lab token, no longer a Maintainer": {"maintainer/remove", true, http.StatusForbidden},
		"session, Blocked":                  {"block", false, http.StatusForbidden},
		"session, End all access":           {"end-all-access", false, http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := lendLab(t)
			l.t = t
			owner, ravi := l.signedIn(ownerID), l.signedIn(maintainerID)
			body := `{"name":"cut-mini","model":"desktop"}`
			var send func() *httptest.ResponseRecorder
			if c.token {
				send = l.heldAsToken(ravi.makeToken("cut", "90"), "/api/enroll-tokens", body)
			} else {
				send = ravi.heldPost("/api/enroll-tokens", "application/json", body)
			}
			owner.did(personPath(maintainerID, c.cut), "sure", "yes")
			w := send()
			answer := strings.TrimSpace(w.Body.String())
			blocked := c.cut == "block"
			if w.Code != c.code || (answer == blockedSentence) != blocked {
				t.Errorf("a body cut by %s: %d %q", c.cut, w.Code, answer)
			}
		})
	}
}

// A Join command asked for before End all access, its body read whole, and
// stored only after the cut has swept the person's unused Join commands, is
// not kept: the store checks in the one transaction that the credential that
// asked is still there.
func TestAJoinCommandAskedBeforeACutAndStoredAfterItIsNotKept(t *testing.T) {
	for name, ask := range map[string]func(l *signInLab, ravi *browser) *httptest.ResponseRecorder{
		"Lab token": func(l *signInLab, ravi *browser) *httptest.ResponseRecorder {
			return l.heldAsToken(ravi.makeToken("stolen", "90"), "/api/enroll-tokens", `{"name":"late-mini","model":"desktop"}`)()
		},
		"browser session": func(_ *signInLab, ravi *browser) *httptest.ResponseRecorder {
			return ravi.write("POST", "/api/enroll-tokens", `{"name":"late-mini","model":"desktop"}`, "Content-Type", "application/json")
		},
		"Lend a Mac": func(_ *signInLab, ravi *browser) *httptest.ResponseRecorder {
			return ravi.form("/lend/join-command", "model", lendModelID, "name", "late-mini")
		},
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := lendLab(t)
			l.t = t
			owner, ravi := l.signedIn(ownerID), l.signedIn(maintainerID)
			var once sync.Once
			beforeJoinCommandStored = func() {
				once.Do(func() { owner.did(personPath(maintainerID, "end-all-access"), "sure", "yes") })
			}
			defer func() { beforeJoinCommandStored = nil }()
			w := ask(l, ravi)
			if strings.Contains(w.Body.String(), "enr_") {
				t.Errorf("the answer shows a Join command made across the cut: %d", w.Code)
			}
			if n := l.count(`SELECT count(*) FROM tokens WHERE name = 'late-mini'`); n != 0 {
				t.Errorf("%d Join commands were kept across the cut", n)
			}
		})
	}
}

// A slow body of a Lab token that is still good is taken.
func TestAHeldBodyOfALiveLabTokenIsTaken(t *testing.T) {
	l, _ := lendLab(t)
	token := l.signedIn(maintainerID).makeToken("slow", "90")
	w := l.heldAsToken(token, "/api/enroll-tokens", `{"name":"slow-mini","model":"desktop"}`)()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "enr_") {
		t.Errorf("a slow body of a live Lab token: %d %s", w.Code, w.Body)
	}
}

// A cut outlives a request that was let in before it: whoever holds a stolen
// session and keeps "make a Lab token" open, its body unsent, gets no token
// by sending the body after the session was ended.
func TestACutSessionMakesNoLabTokenThroughARequestItHeldOpen(t *testing.T) {
	for name, cut := range map[string]string{"End all access": "/me/end-all-access", "Sign out everywhere": "/me/sign-out-everywhere"} {
		t.Run(name, func(t *testing.T) {
			l := pagesLab(t, nil)
			thief, person := l.signedIn(ownerID), l.signedIn(ownerID)
			told := l.toldAgents()
			told.forget()
			send := thief.heldForm("/me/tokens", "name", "kept past the cut", "period", "never")
			if w := person.form(cut, "sure", "yes"); w.Code != http.StatusSeeOther {
				t.Fatalf("%s: %d %s", name, w.Code, w.Body)
			}
			w := send()
			if found := reLabToken.FindAllString(w.Body.String(), -1); len(found) != 0 {
				t.Errorf("the answer shows a Lab token's secret")
			}
			if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/sign-in" {
				t.Errorf("the answer is not a signed-out browser's: %d to %q\n%s", w.Code, w.Header().Get("Location"), w.Body)
			}
			if n := l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, ownerID); n != 0 {
				t.Errorf("%d Lab tokens were made after the cut", n)
			}
			for _, line := range told.lines() {
				if strings.Contains(line, "kept past the cut") {
					t.Errorf("\"Agents\" was told of a token that was not made: %s", line)
				}
			}
			if n := l.count(`SELECT count(*) FROM people_record WHERE what LIKE '%kept past the cut%'`); n != 0 {
				t.Errorf("the People record has %d lines for a token that was not made", n)
			}
		})
	}
}

// A request held open by a session that is still there makes its token.
func TestAHeldRequestOfALiveSessionMakesItsLabToken(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.signedIn(ownerID)
	w := b.heldForm("/me/tokens", "name", "slow", "period", "90")()
	if found := reLabToken.FindAllString(w.Body.String(), -1); w.Code != http.StatusOK || len(found) != 1 {
		t.Fatalf("a slow form of a live session: %d, %d secrets", w.Code, len(found))
	}
}
