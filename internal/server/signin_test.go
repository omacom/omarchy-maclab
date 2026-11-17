package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/githubtest"
)

// signInLab is a labd at the Lab address, behind the tunnel, whose GitHub is
// the fake. Account 1 is the Admin its configuration names.
type signInLab struct {
	t     *testing.T
	s     *Server
	h     http.Handler
	gh    *githubtest.Server
	clock *agent.FakeClock
	dir   string
}

const ownerID = 1

func newSignInLab(t *testing.T, config func(*Config)) *signInLab {
	t.Helper()
	l := &signInLab{t: t, gh: githubtest.New("the-client-id", "the-client-secret"), clock: agent.NewFakeClock(time.Date(2026, time.November, 14, 12, 0, 0, 0, time.UTC))}
	fake := httptest.NewServer(l.gh)
	t.Cleanup(fake.Close)
	l.gh.AddUser(ownerID, "owner")
	l.s = labServer(t, labAddress, func(c *Config) {
		l.dir = c.DataDir
		c.Clock = l.clock
		c.GitHub = &GitHubApp{ClientID: l.gh.ClientID, ClientSecret: l.gh.ClientSecret, WebURL: fake.URL, APIURL: fake.URL}
		c.Admins = []int64{ownerID}
		if config != nil {
			config(c)
		}
	})
	l.h = l.s.Handler()
	return l
}

// browser is one browser: it keeps the cookies labd sets and sends them back.
type browser struct {
	l   *signInLab
	jar map[string]*http.Cookie
}

func (l *signInLab) browser() *browser { return &browser{l: l, jar: map[string]*http.Cookie{}} }

// do asks labd through the tunnel, by the Lab address.
func (b *browser) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	b.l.t.Helper()
	r := httptest.NewRequest(method, "http://"+labHost+target, strings.NewReader(body))
	r = r.WithContext(onTunnel(r.Context()))
	r.RemoteAddr = "@"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	r.Header.Set("User-Agent", "TestBrowser/9.75")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	for _, c := range b.jar {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.l.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.jar, c.Name)
		} else {
			b.jar[c.Name] = c
		}
	}
	return w
}

// write is a change sent as the lab's own pages send it: with their Origin
// and the session's CSRF token.
func (b *browser) write(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	b.l.t.Helper()
	return b.do(method, target, body, append([]string{"Origin", labAddress, "X-CSRF-Token", b.csrf()}, hdr...)...)
}

func (b *browser) session() *http.Cookie { return b.jar[sessionCookie(true)] }

func (b *browser) csrf() string {
	if c := b.session(); c != nil {
		return csrfToken(c.Value)
	}
	return ""
}

// toGitHub follows the Sign in link to the fake GitHub, signed in there as
// account id, and returns the address GitHub sends the browser back to.
func (b *browser) toGitHub(id int64, returnTo string) (callback string) {
	b.l.t.Helper()
	target := "/sign-in"
	if returnTo != "" {
		target += "?return=" + url.QueryEscape(returnTo)
	}
	w := b.do("GET", target, "")
	if w.Code != http.StatusFound {
		b.l.t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
	}
	b.l.gh.SignInAs(id)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := noFollow.Get(w.Header().Get("Location"))
	if err != nil {
		b.l.t.Fatal(err)
	}
	res.Body.Close()
	back, err := url.Parse(res.Header.Get("Location"))
	if err != nil || res.StatusCode != http.StatusFound || back.Scheme+"://"+back.Host != labAddress {
		b.l.t.Fatalf("GitHub sent the browser to %q (%d), not back to the Lab address", res.Header.Get("Location"), res.StatusCode)
	}
	return back.RequestURI()
}

// signIn signs the browser in as account id and returns labd's answer to the
// callback.
func (b *browser) signIn(id int64, returnTo string) *httptest.ResponseRecorder {
	b.l.t.Helper()
	return b.do("GET", b.toGitHub(id, returnTo), "")
}

func (b *browser) mustSignIn(id int64) {
	b.l.t.Helper()
	if w := b.signIn(id, ""); w.Code != http.StatusSeeOther || b.session() == nil {
		b.l.t.Fatalf("sign-in as account %d: %d %s", id, w.Code, w.Body)
	}
}

// whoami is what labd says of the browser on /api/auth.
func (b *browser) whoami() (out struct {
	Login     string `json:"login"`
	Kind      string `json:"kind"`
	AccountID int64  `json:"account_id"`
}) {
	json.Unmarshal(b.do("GET", "/api/auth", "").Body.Bytes(), &out)
	return out
}

// onDisk is every byte labd.db holds, its write-ahead log included.
func (l *signInLab) onDisk() []byte {
	l.t.Helper()
	var all []byte
	for _, name := range []string{"labd.db", "labd.db-wal", "reports.db", "reports.db-wal"} {
		b, err := os.ReadFile(filepath.Join(l.dir, name))
		if err != nil && !os.IsNotExist(err) {
			l.t.Fatal(err)
		}
		all = append(all, b...)
	}
	return all
}

func (l *signInLab) columns(table string) []string {
	l.t.Helper()
	rows, err := l.s.store.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY name`, table)
	if err != nil {
		l.t.Fatal(err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		rows.Scan(&c)
		cols = append(cols, c)
	}
	return cols
}

func (l *signInLab) count(query string, args ...any) (n int) {
	l.t.Helper()
	if err := l.s.store.db.QueryRow(query, args...).Scan(&n); err != nil {
		l.t.Fatal(err)
	}
	return n
}

// Sign-in makes a Person kept by GitHub's account id: a renamed handle is
// the same person. labd asks GitHub for no scope and keeps nothing GitHub
// says of the person but the id and the handle, and not GitHub's token.
func TestSignInMakesAPersonByAccountID(t *testing.T) {
	l := newSignInLab(t, nil)
	l.gh.AddUser(7, "ada")
	b := l.browser()
	if w := b.signIn(7, ""); w.Code != http.StatusSeeOther || w.Header().Get("Location") != labAddress+"/" {
		t.Fatalf("the callback: %d to %q %s, want 303 to the home page", w.Code, w.Header().Get("Location"), w.Body)
	}
	if me := b.whoami(); me.AccountID != 7 || me.Login != "ada" || me.Kind != "Signed in, no role" {
		t.Fatalf("signed in as %+v", me)
	}
	asked := l.gh.Asked()
	if len(asked) != 1 {
		t.Fatalf("GitHub's authorize page was asked %d times", len(asked))
	}
	q := asked[0]
	if !q.Has("scope") || q.Get("scope") != "" {
		t.Errorf("labd asked GitHub for scope %q, want it empty", q.Get("scope"))
	}
	if q.Get("client_id") != l.gh.ClientID || q.Get("redirect_uri") != labAddress+"/auth/github/callback" || q.Get("state") == "" {
		t.Errorf("labd sent GitHub %v", q)
	}
	if q.Has("client_secret") || strings.Contains(q.Encode(), l.gh.ClientSecret) {
		t.Error("the app's secret went through the browser")
	}

	// The handle changes on GitHub; the person signs in from another browser.
	l.gh.AddUser(7, "ada-lovelace")
	b2 := l.browser()
	b2.mustSignIn(7)
	if me := b2.whoami(); me.AccountID != 7 || me.Login != "ada-lovelace" {
		t.Errorf("after the rename: %+v", me)
	}
	if me := b.whoami(); me.Login != "ada-lovelace" {
		t.Errorf("the first browser still reads %+v", me)
	}
	if n := l.count(`SELECT count(*) FROM people WHERE account_id != ?`, testAccount("ada")); n != 1 {
		t.Errorf("%d people after one account signed in twice under two handles", n)
	}
	if n := l.count(`SELECT count(*) FROM people WHERE account_id = 7 AND handle = 'ada-lovelace'`); n != 1 {
		t.Error("the person is not kept by account id with the new handle")
	}

	if got, want := l.columns("people"), []string{"account_id", "added", "blocked_at", "blocked_by", "blocked_reason", "forget_asked", "handle", "last_seen", "leave_to_lend", "lend_cap", "m_team", "maintainer"}; !slices.Equal(got, want) {
		t.Errorf("people keeps %v, want %v", got, want)
	}
	disk := l.onDisk()
	for _, kept := range []string{"mail.example", "Full Name of", "avatars.example", "gho_"} {
		if bytes.Contains(disk, []byte(kept)) {
			t.Errorf("labd's stores hold %q, which GitHub said and labd must not keep", kept)
		}
	}
}

// The session is a random id in an HttpOnly, Secure, SameSite=Lax cookie;
// labd stores its hash alone, and neither the address nor the browser's name.
func TestTheSessionCookieAndWhatIsStored(t *testing.T) {
	l := newSignInLab(t, nil)
	b := l.browser()
	w := b.signIn(ownerID, "")
	var c *http.Cookie
	for _, sc := range w.Result().Cookies() {
		if sc.Name == sessionCookie(true) {
			c = sc
		}
	}
	if c == nil {
		t.Fatalf("no session cookie in %v", w.Header().Values("Set-Cookie"))
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" || !strings.HasPrefix(c.Name, "__Host-") {
		t.Errorf("the session cookie: %s", c.String())
	}
	if len(c.Value) < 40 {
		t.Errorf("a session id of %d characters", len(c.Value))
	}
	if b2 := l.browser(); func() bool { b2.mustSignIn(ownerID); return b2.session().Value == c.Value }() {
		t.Error("two sign-ins got the same session id")
	}
	disk := l.onDisk()
	if bytes.Contains(disk, []byte(c.Value)) {
		t.Error("the session id itself is stored")
	}
	if !bytes.Contains(disk, []byte(hashSecret(c.Value))) {
		t.Error("the session's hash is not stored")
	}
	for _, kept := range []string{"203.0.113.9", "TestBrowser"} {
		if bytes.Contains(disk, []byte(kept)) {
			t.Errorf("labd.db holds %q for a session", kept)
		}
	}
	if got, want := l.columns("browser_sessions"), []string{"account_id", "created", "hash", "id", "last_used"}; !slices.Equal(got, want) {
		t.Errorf("browser_sessions keeps %v, want %v", got, want)
	}
}

// A session left unused for 30 days ends; each use starts the 30 days again.
func TestASessionEndsAfter30DaysUnused(t *testing.T) {
	l := newSignInLab(t, nil)
	b := l.browser()
	b.mustSignIn(ownerID)
	for range 3 {
		l.clock.Advance(29 * 24 * time.Hour)
		if me := b.whoami(); me.AccountID != ownerID {
			t.Fatalf("a session used 29 days ago is gone: %+v", me)
		}
	}
	l.clock.Advance(30*24*time.Hour + time.Minute)
	if w := b.do("GET", "/api/auth", ""); w.Code != http.StatusNotFound {
		t.Errorf("a session unused for 30 days still signs in: %d %s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM browser_sessions`); n != 0 {
		t.Errorf("%d sessions kept after the only one ended", n)
	}
}

// Sign-out ends the session: the cookie goes and its id signs nobody in.
func TestSignOutEndsTheSession(t *testing.T) {
	l := newSignInLab(t, nil)
	b, other := l.browser(), l.browser()
	b.mustSignIn(ownerID)
	other.mustSignIn(ownerID)
	old := b.session()
	w := b.do("POST", "/sign-out", "csrf_token="+b.csrf(), "Origin", labAddress, "Content-Type", "application/x-www-form-urlencoded")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != labAddress+"/" {
		t.Fatalf("sign-out: %d to %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	if b.session() != nil {
		t.Error("sign-out left the cookie in the browser")
	}
	b.jar[old.Name] = old
	if w := b.do("GET", "/api/auth", ""); w.Code != http.StatusNotFound {
		t.Errorf("the signed-out session's id still signs in: %d %s", w.Code, w.Body)
	}
	if me := other.whoami(); me.AccountID != ownerID {
		t.Errorf("sign-out in one browser ended another's session: %+v", me)
	}

	// Signing in again in a browser replaces its session: the one it held
	// is gone, not left behind.
	held := other.session()
	other.mustSignIn(ownerID)
	if other.session().Value == held.Value {
		t.Fatal("a second sign-in kept the session id")
	}
	if n := l.count(`SELECT count(*) FROM browser_sessions`); n != 1 {
		t.Errorf("%d sessions stored for one signed-in browser", n)
	}
}

// Roles are read on every request: a change takes effect on the next one
// and cuts the person's open streams. An Admin is whoever labd's
// configuration names by account id, and is a Maintainer.
func TestARoleChangeTakesEffectOnTheNextRequest(t *testing.T) {
	l := newSignInLab(t, nil)
	l.gh.AddUser(8, "grace")
	b := l.browser()
	b.mustSignIn(8)
	if w := b.do("GET", "/api/devices", ""); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "@grace") {
		t.Fatalf("a signed-in person with no role on a Maintainer's route: %d %s", w.Code, w.Body)
	}
	if err := l.s.setMaintainer(8, true); err != nil {
		t.Fatal(err)
	}
	if w := b.do("GET", "/api/devices", ""); w.Code != http.StatusOK {
		t.Fatalf("made a Maintainer, the next request: %d %s", w.Code, w.Body)
	}
	if me := b.whoami(); me.Kind != "Maintainer" {
		t.Errorf("a Maintainer reads as %+v", me)
	}
	if w := b.write("POST", "/api/builders", `{"name":"new"}`); w.Code != http.StatusOK {
		t.Errorf("a Maintainer's write from the lab's own page: %d %s", w.Code, w.Body)
	}

	// An open stream ends with the role.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-Proto", "https")
		l.h.ServeHTTP(w, r.WithContext(onTunnel(r.Context())))
	}))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/api/stream", nil)
	req.Host = labHost
	req.AddCookie(b.session())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	if line, err := rd.ReadString('\n'); err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("a Maintainer's stream: %d %q %v", res.StatusCode, line, err)
	}
	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := rd.ReadString('\n'); err != nil {
				ended <- err
				return
			}
		}
	}()
	if err := l.s.setMaintainer(8, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Error("the stream stayed open after the role went")
	}
	if w := b.do("GET", "/api/devices", ""); w.Code != http.StatusForbidden {
		t.Errorf("no longer a Maintainer, the next request: %d %s", w.Code, w.Body)
	}

	owner := l.browser()
	owner.mustSignIn(ownerID)
	if me := owner.whoami(); me.Kind != "Admin" || me.Login != "owner" {
		t.Errorf("the account the configuration names reads as %+v", me)
	}
	if w := owner.do("GET", "/api/builders", ""); w.Code != http.StatusOK {
		t.Errorf("an Admin on a Maintainer's route: %d %s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM people WHERE maintainer != 0 AND account_id != ?`, testAccount("ada")); n != 0 {
		t.Errorf("%d people stored as Maintainers: the Admin is named in the configuration, on no screen", n)
	}
}

// A signed-out page carries nothing per visitor: no cookie, no token, no
// flash line, whatever the visitor did before.
func TestASignedOutPageCarriesNothingPerVisitor(t *testing.T) {
	l := newSignInLab(t, nil)
	first := l.browser().do("GET", "/", "")
	if first.Code != http.StatusOK {
		t.Fatalf("the home page: %d", first.Code)
	}
	failed := l.browser()
	if w := failed.do("GET", "/auth/github/callback?code=x&state=y", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("a callback nobody asked for: %d %s", w.Code, w.Body)
	}
	out := l.browser()
	out.mustSignIn(ownerID)
	out.write("POST", "/sign-out", "")
	for who, b := range map[string]*browser{"a new visitor": l.browser(), "after a refused sign-in": failed, "after signing out": out} {
		for _, target := range []string{"/", "/terms", "/privacy", "/no-such-page", "/healthz", "/api/devices"} {
			w := b.do("GET", target, "")
			if sc := w.Header().Values("Set-Cookie"); len(sc) != 0 {
				t.Errorf("%s, GET %s set a cookie: %v", who, target, sc)
			}
		}
		if w := b.do("GET", "/", ""); !bytes.Equal(w.Body.Bytes(), first.Body.Bytes()) {
			t.Errorf("%s, the home page differs from any other visitor's", who)
		}
		if len(b.jar) != 0 {
			t.Errorf("%s the browser still holds %v", who, b.jar)
		}
	}
}

// Sign in is a plain link to a no-store address that sets a short-lived
// state cookie, and the state works once.
func TestSignInIsALinkWhoseStateWorksOnce(t *testing.T) {
	l := newSignInLab(t, nil)
	b := l.browser()
	w := b.do("GET", "/sign-in", "")
	if w.Code != http.StatusFound || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /sign-in: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("GET /sign-in set %v", w.Header().Values("Set-Cookie"))
	}
	c := cookies[0]
	if c.Name != stateCookie(true) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 || c.MaxAge > 600 {
		t.Errorf("the state cookie: %s", c.String())
	}
	to, _ := url.Parse(w.Header().Get("Location"))
	if to.Query().Get("state") != c.Value {
		t.Errorf("the state GitHub is sent is not the cookie's")
	}

	callback := b.toGitHub(ownerID, "")
	state := b.jar[stateCookie(true)]
	if w := b.do("GET", callback, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("the callback: %d %s", w.Code, w.Body)
	}
	if b.jar[stateCookie(true)] != nil {
		t.Error("the state cookie outlived the sign-in")
	}
	// The same address again, from a browser still holding the state cookie.
	replay := l.browser()
	replay.jar[state.Name] = state
	if w := replay.do("GET", callback, ""); w.Code != http.StatusBadRequest || replay.session() != nil || !strings.Contains(w.Body.String(), "Sign in again") {
		t.Errorf("a used state signed in again: %d %s", w.Code, w.Body)
	}

	// A state from one browser is nothing in another, with or without a
	// state cookie of its own.
	callback = l.browser().toGitHub(ownerID, "")
	stranger := l.browser()
	if w := stranger.do("GET", callback, ""); w.Code != http.StatusBadRequest || stranger.session() != nil {
		t.Errorf("a callback without the state cookie: %d %s", w.Code, w.Body)
	}
	callback = l.browser().toGitHub(ownerID, "")
	stranger.do("GET", "/sign-in", "")
	if w := stranger.do("GET", callback, ""); w.Code != http.StatusBadRequest || stranger.session() != nil {
		t.Errorf("a callback with another browser's state: %d %s", w.Code, w.Body)
	}

	// A state left for more than ten minutes is stale.
	late := l.browser()
	callback = late.toGitHub(ownerID, "")
	l.clock.Advance(11 * time.Minute)
	if w := late.do("GET", callback, ""); w.Code != http.StatusBadRequest || late.session() != nil {
		t.Errorf("a state 11 minutes old: %d %s", w.Code, w.Body)
	}
}

// After sign-in a person returns to the path they came from, and only to a
// path on the Lab address: anything else lands on the home page.
func TestAfterSignInAPersonReturnsOnlyToAPathOnTheLabAddress(t *testing.T) {
	l := newSignInLab(t, nil)
	home := labAddress + "/"
	for returnTo, want := range map[string]string{
		"":                            home,
		"/":                           home,
		"/macs/m1?tab=live&x=a%20b":   labAddress + "/macs/m1?tab=live&x=a%20b",
		"/people":                     labAddress + "/people",
		"https://evil.example/":       home,
		"https://evil.example/people": home,
		"//evil.example/people":       home,
		"///evil.example":             home,
		`/\evil.example`:              home,
		`\\evil.example`:              home,
		"/%5Cevil.example":            home,
		"/%2F%2Fevil.example":         home,
		"javascript:alert(1)":         home,
		"people":                      home,
		"/people\r\nSet-Cookie: x=1":  home,
		"http://lab.omarchy-mac.org@evil.example/": home,
		" /people":      home,
		"/a/../../evil": home,
	} {
		b := l.browser()
		w := b.signIn(ownerID, returnTo)
		if w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
			t.Errorf("return=%q: %d to %q, want %q", returnTo, w.Code, w.Header().Get("Location"), want)
		}
	}
}

// Every write from a browser must come from the lab's own pages, said by
// Origin, or by Sec-Fetch-Site when there is none; a signed-in write carries
// the CSRF token as well.
func TestABrowsersWriteMustComeFromTheLabsOwnPages(t *testing.T) {
	l := newSignInLab(t, nil)
	b := l.browser()
	b.mustSignIn(ownerID)
	csrf := b.csrf()
	const form = "application/x-www-form-urlencoded"
	for _, c := range []struct {
		what string
		hdr  []string
		body string
		want int
	}{
		{"the lab's own Origin and the token", []string{"Origin", labAddress, "X-CSRF-Token", csrf}, `{"name":"m1"}`, 200},
		{"no Origin, same-origin by Sec-Fetch-Site, and the token", []string{"Sec-Fetch-Site", "same-origin", "X-CSRF-Token", csrf}, `{"name":"m2"}`, 200},
		{"neither Origin nor Sec-Fetch-Site", []string{"X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"another site's Origin", []string{"Origin", "https://evil.example", "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"the lab's name over http", []string{"Origin", "http://" + labHost, "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"the Alias's Origin", []string{"Origin", "https://" + aliasHost, "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"Origin null", []string{"Origin", "null", "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"no Origin, cross-site", []string{"Sec-Fetch-Site", "cross-site", "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"no Origin, same-site", []string{"Sec-Fetch-Site", "same-site", "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"no Origin, typed by the user", []string{"Sec-Fetch-Site", "none", "X-CSRF-Token", csrf}, `{"name":"m3"}`, 403},
		{"the lab's own Origin without the token", []string{"Origin", labAddress}, `{"name":"m3"}`, 403},
		{"the lab's own Origin and a wrong token", []string{"Origin", labAddress, "X-CSRF-Token", strings.Repeat("0", len(csrf))}, `{"name":"m3"}`, 403},
		{"the token as a query parameter", []string{"Origin", labAddress}, `{"name":"m3"}`, 403},
	} {
		target := "/api/enroll-tokens"
		if c.what == "the token as a query parameter" {
			target += "?csrf_token=" + csrf
		}
		if w := b.do("POST", target, c.body, c.hdr...); w.Code != c.want {
			t.Errorf("a signed-in write with %s: %d %s, want %d", c.what, w.Code, w.Body, c.want)
		}
	}
	if w := b.do("GET", "/api/devices", ""); w.Code != http.StatusOK {
		t.Errorf("a signed-in read needs neither: %d %s", w.Code, w.Body)
	}

	// A form carries the token in a field.
	if w := b.do("POST", "/sign-out", "csrf_token=wrong", "Origin", labAddress, "Content-Type", form); w.Code != http.StatusForbidden || b.session() == nil {
		t.Errorf("a signed-in form with a wrong token: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/sign-out", "", "Origin", labAddress, "Content-Type", form); w.Code != http.StatusForbidden || b.session() == nil {
		t.Errorf("a signed-in form without the token: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/sign-out", "csrf_token="+csrf, "Origin", "https://evil.example", "Content-Type", form); w.Code != http.StatusForbidden || b.session() == nil {
		t.Errorf("another site's form with the token: %d %s", w.Code, w.Body)
	}
	if w := b.do("POST", "/sign-out", "csrf_token="+csrf, "Origin", labAddress, "Content-Type", form); w.Code != http.StatusSeeOther {
		t.Errorf("the lab's own form with the token: %d %s", w.Code, w.Body)
	}

	// Signed out, a browser's write still needs the lab's own pages; the
	// lab's CLIs, which are no browser and send neither header, do not.
	out := l.browser()
	for _, hdr := range [][]string{
		{"Origin", "https://evil.example"},
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-site"},
	} {
		if w := out.do("POST", "/sign-out", "", hdr...); w.Code != http.StatusForbidden {
			t.Errorf("a signed-out write with %v: %d %s", hdr, w.Code, w.Body)
		}
		if w := out.do("POST", "/api/enroll-tokens", `{"name":"m4"}`, append([]string{"Authorization", "Bearer lab_adm_secret"}, hdr...)...); w.Code != http.StatusForbidden {
			t.Errorf("a token's write with %v: %d %s", hdr, w.Code, w.Body)
		}
	}
	if w := out.do("POST", "/api/enroll-tokens", `{"name":"m4"}`, "Authorization", "Bearer lab_adm_secret"); w.Code != http.StatusOK {
		t.Errorf("the CLI's write, by its token: %d %s", w.Code, w.Body)
	}
}

// GitHub refusing, or naming nobody labd can keep, signs nobody in.
func TestGitHubsRefusalSignsNobodyIn(t *testing.T) {
	l := newSignInLab(t, func(c *Config) { c.GitHub.ClientSecret = "not-the-secret" })
	b := l.browser()
	if w := b.signIn(ownerID, ""); w.Code != http.StatusBadGateway || b.session() != nil || !strings.Contains(w.Body.String(), "GitHub didn't sign you in") {
		t.Errorf("GitHub refused the app's secret: %d %s", w.Code, w.Body)
	}
	if strings.Contains(l.browser().do("GET", "/", "").Body.String(), "GitHub didn't") {
		t.Error("the refusal shows on a later page")
	}

	l = newSignInLab(t, nil)
	l.gh.AddUser(9, "not a handle")
	b = l.browser()
	if w := b.signIn(9, ""); w.Code != http.StatusBadGateway || b.session() != nil {
		t.Errorf("GitHub named %q: %d %s", "not a handle", w.Code, w.Body)
	}
	b = l.browser()
	callback := b.toGitHub(ownerID, "")
	u, _ := url.Parse(callback)
	q := u.Query()
	q.Del("code")
	if w := b.do("GET", u.Path+"?"+q.Encode(), ""); w.Code != http.StatusBadRequest || b.session() != nil {
		t.Errorf("a callback without a code: %d %s", w.Code, w.Body)
	}
	b = l.browser()
	u, _ = url.Parse(b.toGitHub(ownerID, ""))
	q = u.Query()
	q.Set("code", "not-the-code")
	if w := b.do("GET", u.Path+"?"+q.Encode(), ""); w.Code != http.StatusBadGateway || b.session() != nil {
		t.Errorf("a callback with a code GitHub never gave: %d %s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM people WHERE account_id != ?`, testAccount("ada")); n != 0 {
		t.Errorf("%d people after nobody signed in", n)
	}
}

// An Admin is whoever the configuration names when the request comes: a
// labd started without the account id serves the same session as no Admin.
func TestAnAdminIsWhoeverTheConfigurationNamesNow(t *testing.T) {
	l := newSignInLab(t, nil)
	b := l.browser()
	b.mustSignIn(ownerID)
	if me := b.whoami(); me.Kind != "Admin" {
		t.Fatalf("named in the configuration: %+v", me)
	}
	again := labServer(t, labAddress, func(c *Config) {
		c.DataDir = l.dir
		c.GitHub = l.s.cfg.GitHub
		c.Admins = []int64{4711}
	})
	b.l = &signInLab{t: t, s: again, h: again.Handler(), gh: l.gh, clock: l.clock, dir: l.dir}
	if me := b.whoami(); me.AccountID != ownerID || me.Kind != "Signed in, no role" {
		t.Errorf("no longer named in the configuration: %+v", me)
	}
	if w := b.do("GET", "/api/maintainers", ""); w.Code != http.StatusForbidden {
		t.Errorf("no longer an Admin, on a Maintainer's route: %d %s", w.Code, w.Body)
	}
}

// No route of labd's makes or removes a Maintainer any more: maclab's three
// answer that they are gone, to a Maintainer and to an Admin alike, and
// change nobody. Only an Admin adds and removes Maintainers, on People.
func TestNoRouteAddsOrRemovesAMaintainer(t *testing.T) {
	l := newSignInLab(t, nil)
	l.gh.AddUser(8, "grace")
	grace, owner := l.browser(), l.browser()
	grace.mustSignIn(8)
	owner.mustSignIn(ownerID)
	if err := l.s.setMaintainer(8, true); err != nil {
		t.Fatal(err)
	}
	before := l.count(`SELECT count(*) FROM people WHERE maintainer != 0`)
	for who, b := range map[string]*browser{"a Maintainer": grace, "an Admin": owner} {
		if w := b.do("GET", "/api/maintainers", ""); w.Code != http.StatusGone {
			t.Errorf("%s reading maclab's Maintainers: %d %s", who, w.Code, w.Body)
		}
		if w := b.write("POST", "/api/maintainers", `{"name":"new"}`); w.Code != http.StatusGone || strings.Contains(w.Body.String(), "token\"") {
			t.Errorf("%s adding a Maintainer: %d %s", who, w.Code, w.Body)
		}
		if w := b.write("DELETE", "/api/maintainers/grace", ""); w.Code != http.StatusGone {
			t.Errorf("%s removing a Maintainer: %d %s", who, w.Code, w.Body)
		}
	}
	if after := l.count(`SELECT count(*) FROM people WHERE maintainer != 0`); after != before || l.count(`SELECT count(*) FROM maintainers`) != 0 {
		t.Errorf("the tries changed who is a Maintainer: %d people before, %d after", before, after)
	}
	if me := grace.whoami(); me.Kind != "Maintainer" {
		t.Errorf("grace reads as %+v", me)
	}
}

// Without the OAuth app's values there is no sign-in.
func TestThereIsNoSignInWithoutTheOAuthApp(t *testing.T) {
	l := newSignInLab(t, func(c *Config) { c.GitHub = nil })
	b := l.browser()
	for _, target := range []string{"/sign-in", "/auth/github/callback?code=x&state=y"} {
		if w := b.do("GET", target, ""); w.Code != http.StatusNotFound || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("GET %s with no OAuth app: %d %v", target, w.Code, w.Header().Values("Set-Cookie"))
		}
	}
}

// At a plain-http loopback address, where a lab under development is
// reached, the cookies are not Secure: a browser would never send them back.
func TestALabUnderDevelopmentSignsInOverLoopback(t *testing.T) {
	gh := githubtest.New("id", "secret")
	gh.AddUser(5, "dev")
	gh.SignInAs(5)
	fake := httptest.NewServer(gh)
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newWith("ada", "lab_adm_secret", ctx, Config{DataDir: t.TempDir(),
		GitHub: &GitHubApp{ClientID: "id", ClientSecret: "secret", WebURL: fake.URL, APIURL: fake.URL}, Admins: []int64{5}}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	labd := httptest.NewServer(s.Handler())
	defer labd.Close()
	jar := &memJar{}
	c := &http.Client{Jar: jar, Transport: named{}}
	res, err := c.Get(labd.URL + "/sign-in?return=/api/auth")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var me struct {
		Kind  string
		Login string
	}
	json.NewDecoder(res.Body).Decode(&me)
	if res.StatusCode != http.StatusOK || me.Kind != "Admin" || me.Login != "dev" {
		t.Fatalf("sign-in over loopback ended at %s: %d %+v", res.Request.URL, res.StatusCode, me)
	}
	if len(jar.cookies) != 1 || jar.cookies[0].Name != sessionCookie(false) || jar.cookies[0].Secure || !jar.cookies[0].HttpOnly {
		t.Errorf("the browser holds %v", jar.cookies)
	}
}

// memJar keeps every cookie for every address: the test's two servers are
// both on 127.0.0.1.
type memJar struct{ cookies []*http.Cookie }

func (j *memJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	for _, c := range cs {
		j.cookies = slices.DeleteFunc(j.cookies, func(o *http.Cookie) bool { return o.Name == c.Name })
		if c.MaxAge >= 0 {
			j.cookies = append(j.cookies, c)
		}
	}
}

func (j *memJar) Cookies(u *url.URL) []*http.Cookie { return j.cookies }

func TestSignInFilesAreReadAsTheSpecSaysSecretsAre(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(dir, name)
		os.Remove(p)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
	}
	if app, admins, err := ReadSignIn("", ""); app != nil || admins != nil || err != nil {
		t.Errorf("no folder: %v %v %v", app, admins, err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "not-made-yet")} {
		if app, admins, err := ReadSignIn(d, ""); app != nil || admins != nil || err != nil {
			t.Errorf("a folder without the files: %v %v %v, want sign-in off", app, admins, err)
		}
	}
	write("admins", "# the owner\n 4711 \n\n12\n", 0o644)
	if app, admins, err := ReadSignIn(dir, ""); app != nil || err != nil || !slices.Equal(admins, []int64{4711, 12}) {
		t.Errorf("admins alone: %v %v %v", app, admins, err)
	}
	write("github-client-id", "Iv1.abc\n", 0o644)
	if _, _, err := ReadSignIn(dir, ""); err == nil || !strings.Contains(err.Error(), "github-client-secret") {
		t.Errorf("a client id without its secret: %v", err)
	}
	write("github-client-secret", " s3cret\n", 0o644)
	if _, _, err := ReadSignIn(dir, ""); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("a secret others can read: %v", err)
	}
	write("github-client-secret", " s3cret\n", 0o600)
	app, _, err := ReadSignIn(dir, "")
	if err != nil || app == nil || app.ClientID != "Iv1.abc" || app.ClientSecret != "s3cret" || app.WebURL != "https://github.com" || app.APIURL != "https://api.github.com" {
		t.Errorf("the app: %+v %v", app, err)
	}
	for _, bad := range []string{"0\n", "-4\n", "owner\n", "12 13\n", "1.5\n"} {
		write("admins", bad, 0o644)
		if _, _, err := ReadSignIn(dir, ""); err == nil || !strings.Contains(err.Error(), "admins") {
			t.Errorf("admins holding %q: %v", bad, err)
		}
	}
	write("admins", "4711\n", 0o644)

	// Another GitHub is for a lab under development: a loopback address only.
	app, _, err = ReadSignIn(dir, "http://127.0.0.1:7791/")
	if err != nil || app.WebURL != "http://127.0.0.1:7791" || app.APIURL != "http://127.0.0.1:7791" {
		t.Errorf("a GitHub stand-in on loopback: %+v %v", app, err)
	}
	for _, bad := range []string{"https://github.example", "http://192.168.1.20:7791", "http://lab.omarchy-mac.org", "127.0.0.1:7791", "http://127.0.0.1:7791/x?y"} {
		if _, _, err := ReadSignIn(dir, bad); err == nil {
			t.Errorf("--github-url %s was taken", bad)
		}
	}
}

// A form is judged by the session as it is when its body has come. Whoever
// holds a stolen session and keeps a form open, its body unsent, changes
// nothing by sending the body after the session was ended or its person
// lost the role the form takes.
func TestAFormHeldOpenAcrossACutChangesNothing(t *testing.T) {
	for name, c := range map[string]struct {
		cut    func(owner, ravi *browser)
		status int
	}{
		"End all access": {func(owner, _ *browser) {
			owner.did(personPath(maintainerID, "end-all-access"), "sure", "yes")
		}, http.StatusSeeOther},
		"Sign out everywhere": {func(_, ravi *browser) {
			if w := ravi.form("/me/sign-out-everywhere", "sure", "yes"); w.Code != http.StatusSeeOther {
				t.Fatalf("Sign out everywhere: %d %s", w.Code, w.Body)
			}
		}, http.StatusSeeOther},
		"no longer a Maintainer": {func(owner, _ *browser) {
			owner.did(personPath(maintainerID, "maintainer/remove"), "sure", "yes")
		}, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			l, _ := lendLab(t)
			l.t = t
			owner, thief, ravi := l.signedIn(ownerID), l.signedIn(maintainerID), l.signedIn(maintainerID)
			l.signedIn(graceID)
			block := thief.heldForm(personPath(graceID, "block"), "sure", "yes", "reason", "held past the cut")
			join := thief.heldForm("/lend/join-command", "model", lendModelID, "name", "held-mac")
			c.cut(owner, ravi)
			w := block()
			if w.Code != c.status || (w.Code == http.StatusSeeOther && w.Header().Get("Location") != "/sign-in") {
				t.Errorf("the People action's answer: %d to %q, want %d", w.Code, w.Header().Get("Location"), c.status)
			}
			if l.who(graceID).Blocked {
				t.Errorf("a form sent after the cut Blocked a person")
			}
			if n := l.count(`SELECT count(*) FROM people_record WHERE note = 'held past the cut'`); n != 0 {
				t.Errorf("the People record has %d lines for an action that was refused", n)
			}
			w = join()
			if strings.Contains(w.Body.String(), "enr_") {
				t.Errorf("a form sent after the cut shows a Join command")
			}
			if n := l.count(`SELECT count(*) FROM tokens WHERE name = 'held-mac'`); n != 0 && name != "no longer a Maintainer" {
				t.Errorf("a form sent after the cut made %d Join commands", n)
			}
		})
	}
}

// A slow form of a session that is still there does what it asks.
func TestAHeldFormOfALiveSessionActs(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(graceID)
	if w := ravi.heldForm(personPath(graceID, "block"), "sure", "yes")(); w.Code != http.StatusSeeOther || !l.who(graceID).Blocked {
		t.Errorf("a slow form of a live session: %d %s", w.Code, refusalIn(w.Body.String()))
	}
}

// A body that is no form is bound to its session the same way: sent after
// the cut, it is not read, and what it asked for is not done.
func TestABodyHeldOpenAcrossACutChangesNothing(t *testing.T) {
	for name, cut := range map[string]string{"End all access": "end-all-access", "no longer a Maintainer": "maintainer/remove"} {
		t.Run(name, func(t *testing.T) {
			l, _ := lendLab(t)
			l.t = t
			owner, thief := l.signedIn(ownerID), l.signedIn(maintainerID)
			send := thief.heldPost("/api/enroll-tokens", "application/json", `{"name":"held-mini","model":"desktop"}`)
			owner.did(personPath(maintainerID, cut), "sure", "yes")
			if w := send(); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "enr_") {
				t.Errorf("a body sent after the cut was taken: %d %s", w.Code, w.Body)
			}
			if n := l.count(`SELECT count(*) FROM tokens WHERE name = 'held-mini'`); n != 0 {
				t.Errorf("a body sent after the cut made %d Join commands", n)
			}
		})
	}
}

// A slow body of a session that is still there is taken.
func TestAHeldBodyOfALiveSessionIsTaken(t *testing.T) {
	l, _ := lendLab(t)
	w := l.signedIn(maintainerID).heldPost("/api/enroll-tokens", "application/json", `{"name":"slow-mini","model":"desktop"}`)()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "enr_") {
		t.Errorf("a slow body of a live session: %d %s", w.Code, w.Body)
	}
}

// countedBody counts the bytes labd reads of a body.
type countedBody struct {
	r io.Reader
	n int
}

func (c *countedBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// A form with a session cookie that names no session is not read: labd
// looks the session up first, and a made-up cookie costs it no byte of the
// body, however long the body says it is.
func TestAFormOfAMadeUpSessionIsNotRead(t *testing.T) {
	l := rolesLab(t, nil)
	l.signedIn(graceID)
	body := &countedBody{r: strings.NewReader("sure=yes&x=" + strings.Repeat("a", formMax-11))}
	r := httptest.NewRequest("POST", "http://"+labHost+personPath(graceID, "block"), body)
	r = r.WithContext(onTunnel(r.Context()))
	r.RemoteAddr = "@"
	for name, value := range map[string]string{"X-Forwarded-Proto": "https", "CF-Connecting-IP": "203.0.113.9", "User-Agent": "TestBrowser/9.75",
		"Origin": labAddress, "Content-Type": formType} {
		r.Header.Set(name, value)
	}
	r.AddCookie(&http.Cookie{Name: sessionCookie(true), Value: "made-up"})
	r.Header.Set("X-CSRF-Token", csrfToken("made-up"))
	w := httptest.NewRecorder()
	l.h.ServeHTTP(w, r)
	if body.n != 0 {
		t.Errorf("labd read %d bytes of a form whose cookie names no session", body.n)
	}
	if l.who(graceID).Blocked {
		t.Errorf("a made-up session Blocked a person: %d", w.Code)
	}
}

// A signed-in browser's form must come whole within formWait: one that
// trickles in is cut off at the deadline and does nothing, and the
// connection is not held open for it.
func TestASlowFormOfASessionIsCutOffAndDoesNothing(t *testing.T) {
	old := formWait
	formWait = 300 * time.Millisecond
	defer func() { formWait = old }()
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(graceID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(onTunnel(r.Context()))
		r.RemoteAddr = "@"
		l.h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	form := "sure=yes&csrf_token=" + ravi.csrf() + "&x=" + strings.Repeat("a", 100)
	start := time.Now()
	fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: %s\r\nX-Forwarded-Proto: https\r\nCF-Connecting-IP: 203.0.113.9\r\nUser-Agent: TestBrowser/9.75\r\nOrigin: %s\r\nCookie: %s=%s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		personPath(graceID, "block"), labHost, labAddress, sessionCookie(true), ravi.session().Value, formType, len(form))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := range len(form) {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if _, err := c.Write([]byte{form[i]}); err != nil {
				return
			}
		}
	}()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	answer, _ := bufio.NewReader(c).ReadString('\n')
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the slow form held the connection %s", took)
	}
	if !strings.Contains(answer, " 400 ") && !strings.Contains(answer, " 408 ") {
		t.Errorf("answer to a slow form: %q", answer)
	}
	if l.who(graceID).Blocked {
		t.Errorf("a form that came after its deadline Blocked a person")
	}
}

// sendHeld posts to target over a real connection, as from the tunnel: the
// headers, then the first bytes of a body of length bytes, then one more
// byte every gap until the body is sent or the test ends. It returns the
// first line of labd's answer and how long it took.
func sendHeld(t *testing.T, h http.Handler, target string, hdr map[string]string, body string, length int, gap time.Duration) (answer string, took time.Duration) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(onTunnel(r.Context()))
		r.RemoteAddr = "@"
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nX-Forwarded-Proto: https\r\nCF-Connecting-IP: 203.0.113.9\r\nUser-Agent: TestBrowser/9.75\r\nOrigin: %s\r\nContent-Length: %d\r\n", target, labHost, labAddress, length)
	for name, value := range hdr {
		head += name + ": " + value + "\r\n"
	}
	start := time.Now()
	first, rest := body, ""
	if gap > 0 {
		first, rest = body[:min(len(body), 16)], body[min(len(body), 16):]
	}
	if _, err := io.WriteString(c, head+"\r\n"+first); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := range len(rest) {
			select {
			case <-stop:
				return
			case <-time.After(gap):
			}
			if _, err := c.Write([]byte{rest[i]}); err != nil {
				return
			}
		}
	}()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	answer, _ = bufio.NewReader(c).ReadString('\n')
	return answer, time.Since(start)
}

// A cookie that names no session leaves its body unread, and the body may
// not hold the connection either: Go's server drains a small unread body
// after the answer, and that drain meets the read deadline every body has
// from its start.
func TestAMadeUpSessionsHeldBodyIsCutAtTheDeadline(t *testing.T) {
	old := bodyWait
	bodyWait = 300 * time.Millisecond
	defer func() { bodyWait = old }()
	l := rolesLab(t, nil)
	l.signedIn(graceID)
	answer, took := sendHeld(t, l.h, personPath(graceID, "block"), map[string]string{
		"Cookie": sessionCookie(true) + "=made-up", "Content-Type": formType, "X-CSRF-Token": csrfToken("made-up"),
	}, "sure=yes&x=aaaa", 64<<10, 0)
	if took > 3*time.Second || answer == "" {
		t.Errorf("a made-up session's held body kept the connection %s, answered %q", took, answer)
	}
	if l.who(graceID).Blocked {
		t.Errorf("a made-up session Blocked a person")
	}
}

// The deletion link's form is anyone's, and must come whole within formWait,
// as a signed-in browser's must: one that trickles in is cut off.
func TestASlowReportDeletionFormIsCutOff(t *testing.T) {
	old := formWait
	formWait = 300 * time.Millisecond
	defer func() { formWait = old }()
	l := rolesLab(t, nil)
	form := "_method=delete&token=" + strings.Repeat("a", 2000)
	answer, took := sendHeld(t, l.h, "/reports/8c1f02ab", map[string]string{"Content-Type": formType}, form, len(form), 20*time.Millisecond)
	if took > 3*time.Second || answer == "" {
		t.Errorf("a slow deletion form held the connection %s, answered %q", took, answer)
	}
}
