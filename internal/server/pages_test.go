package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/web"
)

// Two pages only these tests have: one Maintainers alone may open, and an
// action on it that takes something away.
func init() {
	addPage("GET /test-only/maintainers", web.Maintainers, func(s *Server, w http.ResponseWriter, r *http.Request) {
		s.render(w, r, "not_found", web.Page{Title: "For Maintainers", Nav: "people"})
	})
	addPage("POST /test-only/maintainers/take", web.Maintainers, func(s *Server, w http.ResponseWriter, r *http.Request) {
		if !s.sure(w, r, "Remove: 1 Lease ends now", "/test-only/maintainers") {
			return
		}
		if err := s.record(r, "removed "+r.PostFormValue("what")+" from", &Person{AccountID: 4004, Handle: "petra"}); err != nil {
			httpErr(w, 500, "%v", err)
			return
		}
		http.Redirect(w, r, "/test-only/maintainers", http.StatusSeeOther)
	})
}

const (
	nobodyID     = 2002
	maintainerID = 3003
)

func pagesLab(t *testing.T, config func(*Config)) *signInLab {
	t.Helper()
	l := newSignInLab(t, config)
	l.gh.AddUser(nobodyID, "nobody-yet")
	l.gh.AddUser(maintainerID, "ravi")
	return l
}

func (l *signInLab) signedIn(id int64) *browser {
	l.t.Helper()
	b := l.browser()
	b.mustSignIn(id)
	return b
}

var (
	rePageLink = regexp.MustCompile(`<a [^>]*>(.*?)</a>`)
	reTag      = regexp.MustCompile(`<[^>]+>`)
)

func linkLabels(piece string) string {
	var out []string
	for _, m := range rePageLink.FindAllStringSubmatch(piece, -1) {
		out = append(out, strings.TrimSpace(reTag.ReplaceAllString(m[1], " ")))
	}
	return strings.Join(out, ", ")
}

func part(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("no %q in:\n%s", start, s)
	}
	j := strings.Index(s[i:], end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return s[i : i+j]
}

func TestEveryPageHasTheOneHeaderForWhoeverLooks(t *testing.T) {
	l := pagesLab(t, nil)
	maintainer := l.signedIn(maintainerID)
	if err := l.s.setMaintainer(maintainerID, true); err != nil {
		t.Fatal(err)
	}
	signIn := regexp.MustCompile(`<a class="sign-in"[^>]*>Sign in</a>\n`)
	var row1 string
	for _, c := range []struct {
		who            string
		b              *browser
		row2, handle   string
		badge          string
		seesTheFleet   bool
		mayOpenForThem bool
	}{
		{"signed out", l.browser(), "", "", "", false, false},
		{"signed in, no role", l.signedIn(nobodyID), "Lend a Mac", "nobody-yet", `<span class="badge kind-none">no role</span>`, false, false},
		{"a Maintainer", maintainer, "Macs, Jobs, Builds, Releases, People", "ravi", `<span class="badge kind-maintainer">Maintainer</span>`, true, true},
		{"an Admin", l.signedIn(ownerID), "Macs, Jobs, Builds, Releases, People", "owner", `<span class="badge kind-admin">Admin</span>`, true, true},
	} {
		for _, target := range []string{"/", "/terms", "/privacy", "/no-such-page"} {
			w := c.b.do("GET", target, "")
			body := w.Body.String()
			if want := map[bool]int{true: 404, false: 200}[target == "/no-such-page"]; w.Code != want {
				t.Fatalf("%s, GET %s: %d, want %d", c.who, target, w.Code, want)
			}
			row := part(t, body, `<div class="header-row">`, `</div>`)
			if got := signIn.MatchString(row); got != (c.row2 == "") {
				t.Errorf("%s, %s: Sign in at the end of row 1: %v", c.who, target, got)
			}
			row = signIn.ReplaceAllString(row, "")
			if row1 == "" {
				row1 = row
				if got, want := linkLabels(row), "omarchy  mac lab, Matrix, Models, Features, Kernel gaps, Aurora, Benchmarks, Candidates, Reports, Data, Install"; got != want {
					t.Errorf("row 1 is %q, want %q", got, want)
				}
			}
			if row != row1 {
				t.Errorf("%s, %s: row 1 differs:\n%s\n%s", c.who, target, row, row1)
			}
			foot := part(t, body, `<footer class="site-footer">`, `</footer>`)
			if !strings.Contains(foot, `<a href="/terms">Terms</a>`) || !strings.Contains(foot, `<a href="/privacy">Privacy</a>`) {
				t.Errorf("%s, %s: the footer does not link the two texts:\n%s", c.who, target, foot)
			}
			if c.row2 == "" {
				if strings.Contains(body, "lab-row") {
					t.Errorf("%s, %s: there is a row 2", c.who, target)
				}
				continue
			}
			row2 := part(t, body, `<div class="lab-row">`, `</div>`)
			if got := linkLabels(part(t, row2, `<nav class="lab-nav"`, `</nav>`)); got != c.row2 {
				t.Errorf("%s, %s: row 2 is %q, want %q", c.who, target, got, c.row2)
			}
			for _, piece := range []string{`<a class="handle" href="/me">@` + c.handle + `</a>`, c.badge, `name="csrf_token" value="` + c.b.csrf() + `"`, "Sign out"} {
				if !strings.Contains(row2, piece) {
					t.Errorf("%s, %s: row 2 lacks %s:\n%s", c.who, target, piece, row2)
				}
			}
			if got := strings.Contains(row2, `<span class="fleet">0/0 Macs free · 0 jobs running</span>`); got != c.seesTheFleet {
				t.Errorf("%s, %s: the fleet status: %v:\n%s", c.who, target, got, row2)
			}
			if cc := w.Header().Get("Cache-Control"); cc != "private, no-store" {
				t.Errorf("%s, %s: Cache-Control %q", c.who, target, cc)
			}
		}
	}
}

func TestAPageARoleMayNotOpenIsNotFoundAndSignedOutLeadsToSignIn(t *testing.T) {
	l := pagesLab(t, nil)
	w := l.browser().do("GET", "/test-only/maintainers?tab=2", "")
	if want := "/sign-in?return=" + url.QueryEscape("/test-only/maintainers?tab=2"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Errorf("signed out: %d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), want)
	}
	if sc := w.Header().Values("Set-Cookie"); len(sc) != 0 {
		t.Errorf("signed out, the way to sign-in set a cookie: %v", sc)
	}
	nobody := l.signedIn(nobodyID)
	w = nobody.do("GET", "/test-only/maintainers", "")
	missing := nobody.do("GET", "/no-such-page", "")
	if w.Code != 404 || !bytes.Equal(w.Body.Bytes(), bytes.ReplaceAll(missing.Body.Bytes(), []byte("no-such-page"), []byte("test-only/maintainers"))) {
		t.Errorf("signed in with no role: %d, and not the answer a page that is not there gets:\n%s", w.Code, w.Body)
	}
	if w := nobody.write("POST", "/test-only/maintainers/take", "sure=yes", "Content-Type", "application/x-www-form-urlencoded"); w.Code != 404 {
		t.Errorf("signed in with no role, the page's action: %d", w.Code)
	}
	if w := l.signedIn(ownerID).do("GET", "/test-only/maintainers", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `<a href="/people" class="current" aria-current="page">People</a>`) {
		t.Errorf("an Admin: %d:\n%s", w.Code, w.Body)
	}
	// Coming back from sign-in lands on the page asked for.
	b := l.browser()
	if w := b.signIn(ownerID, "/test-only/maintainers?tab=2"); w.Header().Get("Location") != labAddress+"/test-only/maintainers?tab=2" {
		t.Errorf("after sign-in: to %q", w.Header().Get("Location"))
	}
}

func TestALabWithNoSignInAnswersNotFoundForItsLabPages(t *testing.T) {
	s := aliasServer(t, nil)
	if w := edgeAt(s.Handler(), "GET", labHost, "/test-only/maintainers", viaProxy, fromIngress); w.Code != 404 {
		t.Errorf("GET a Maintainers' page: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestTermsAndPrivacyServeTheShippedTexts(t *testing.T) {
	l := pagesLab(t, nil)
	for target, c := range map[string]struct {
		file, title string
		text        web.Text
	}{
		"/terms":   {"terms.txt", "Terms", web.Terms()},
		"/privacy": {"privacy.txt", "Privacy", web.Privacy()},
	} {
		shipped, err := os.ReadFile("../web/texts/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		want := sha256.Sum256(shipped)
		w := l.browser().do("GET", target, "")
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, "<title>"+c.title+" · Omarchy Mac Lab</title>") {
			t.Fatalf("GET %s: %d", target, w.Code)
		}
		served := sha256.Sum256([]byte(html.UnescapeString(part(t, body, `<pre class="legal">`, `</pre>`)[len(`<pre class="legal">`):])))
		if served != want {
			t.Errorf("GET %s: the sha256 of the text served is %x, the shipped file's is %x", target, served, want)
		}
		offersToCopy(t, body, "the sha256 of this text", hex.EncodeToString(want[:]))
		for _, piece := range []string{`<time datetime="` + c.text.Date + `">` + c.text.Date + `</time>`, hex.EncodeToString(want[:])} {
			if !strings.Contains(body, piece) {
				t.Errorf("GET %s: the page lacks %s", target, piece)
			}
		}
		if sc := w.Header().Values("Set-Cookie"); len(sc) != 0 {
			t.Errorf("GET %s set a cookie: %v", target, sc)
		}
	}
}

func TestTheOldInterfaceIsGone(t *testing.T) {
	l := pagesLab(t, nil)
	admin := l.signedIn(ownerID)
	for _, target := range []string{"/api/themes", "/api/themes/current/background", "/ui/", "/ui/app.js", "/ui/index.html", "/ui/manifest.json"} {
		for who, b := range map[string]*browser{"signed out": l.browser(), "an Admin": admin} {
			if w := b.do("GET", target, ""); w.Code != 404 {
				t.Errorf("%s, GET %s: %d, want 404", who, target, w.Code)
			}
		}
	}
	body := admin.do("GET", "/", "").Body.String()
	for _, never := range []string{"cookie", "consent", "theme-picker", "data-theme", "<base "} {
		if strings.Contains(strings.ToLower(body), never) {
			t.Errorf("the home page has %q", never)
		}
	}
}

func TestAPagesFilesAreFingerprintedKeptAYearAndCompressed(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.browser()
	body := b.do("GET", "/", "").Body.String()
	files := regexp.MustCompile(`(?:href|src)="(/assets/[^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(files) < 5 {
		t.Fatalf("the home page loads %d files of the lab's:\n%s", len(files), body)
	}
	for _, m := range files {
		if !regexp.MustCompile(`^/assets/[A-Za-z0-9-]+-[0-9a-f]{16}\.[a-z0-9]+$`).MatchString(m[1]) {
			t.Errorf("%s carries no fingerprint", m[1])
		}
		w := b.do("GET", m[1], "", "Accept-Encoding", "gzip, br")
		if cc := w.Header().Values("Cache-Control"); w.Code != 200 || len(cc) != 1 || cc[0] != "public, max-age=31536000, immutable" {
			t.Errorf("GET %s: %d, Cache-Control %q", m[1], w.Code, cc)
		}
		if text := strings.HasSuffix(m[1], ".css") || strings.HasSuffix(m[1], ".js") || strings.HasSuffix(m[1], ".svg"); text != (w.Header().Get("Content-Encoding") == "br") {
			t.Errorf("GET %s: Content-Encoding %q", m[1], w.Header().Get("Content-Encoding"))
		}
		if sc := w.Header().Values("Set-Cookie"); len(sc) != 0 {
			t.Errorf("GET %s set a cookie", m[1])
		}
	}
	if w := b.do("GET", "/robots.txt", ""); w.Code != 200 {
		t.Errorf("GET /robots.txt: %d", w.Code)
	}
}

func TestAnActionThatTakesSomethingAwayAsksTwiceAndIsRecorded(t *testing.T) {
	l := pagesLab(t, nil)
	admin := l.signedIn(ownerID)
	form := "Content-Type"
	first := admin.write("POST", "/test-only/maintainers/take", "what=a+Lease", form, "application/x-www-form-urlencoded")
	body := first.Body.String()
	if first.Code != 200 || !strings.Contains(body, `<button class="button button-danger armed" type="submit">Remove: 1 Lease ends now</button>`) {
		t.Fatalf("the first click: %d, and no button that is the sentence:\n%s", first.Code, body)
	}
	for _, piece := range []string{
		`<form method="post" action="/test-only/maintainers/take">`,
		`<input type="hidden" name="csrf_token" value="` + admin.csrf() + `">`,
		`<input type="hidden" name="what" value="a Lease">`,
		`<input type="hidden" name="sure" value="yes">`,
		`<a href="/test-only/maintainers">`,
	} {
		if !strings.Contains(body, piece) {
			t.Errorf("the page that asks again lacks %s", piece)
		}
	}
	if lines, err := l.s.store.recordLines(10); err != nil || len(lines) != 0 {
		t.Fatalf("after the first click the record has %d lines (%v)", len(lines), err)
	}
	l.clock.Advance(90 * time.Second)
	second := admin.write("POST", "/test-only/maintainers/take", "what=a+Lease&sure=yes", form, "application/x-www-form-urlencoded")
	if second.Code != http.StatusSeeOther {
		t.Fatalf("the second click: %d %s", second.Code, second.Body)
	}
	lines, err := l.s.store.recordLines(10)
	if err != nil || len(lines) != 1 {
		t.Fatalf("after the second click the record has %d lines (%v)", len(lines), err)
	}
	got := lines[0]
	if got.ActorID != ownerID || got.Actor != "owner" || !got.At.Equal(l.clock.Now()) || got.What != "removed a Lease from" || got.AboutID != 4004 || got.About != "petra" {
		t.Errorf("the record line: %+v", got)
	}
	if w := l.browser().do("POST", "/test-only/maintainers/take", "sure=yes", form, "application/x-www-form-urlencoded", "Origin", labAddress); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/sign-in") {
		t.Errorf("signed out, the action: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if lines, _ := l.s.store.recordLines(10); len(lines) != 1 {
		t.Errorf("a signed-out visitor wrote a record line")
	}
}

func TestPreviewPutsTheBannerOnPublicPages(t *testing.T) {
	banner := `The live site is <a href="https://omarchy-m-testing.org">omarchy-m-testing.org</a>.`
	for _, preview := range []bool{true, false} {
		l := pagesLab(t, func(c *Config) { c.Preview = preview })
		admin := l.signedIn(ownerID)
		for _, target := range []string{"/", "/terms", "/privacy"} {
			for who, b := range map[string]*browser{"signed out": l.browser(), "an Admin": admin} {
				if got := strings.Contains(b.do("GET", target, "").Body.String(), banner); got != preview {
					t.Errorf("Preview %v, %s, GET %s: the banner: %v", preview, who, target, got)
				}
			}
		}
		if body := admin.do("GET", "/test-only/maintainers", "").Body.String(); strings.Contains(body, banner) {
			t.Errorf("Preview %v: a page of the lab's own carries the banner", preview)
		}
	}
}

func TestAnAnswerThatIsNoPageStaysPlain(t *testing.T) {
	l := pagesLab(t, nil)
	b := l.browser()
	if w := b.do("GET", "/api/no-such-thing", ""); w.Code != 404 || strings.Contains(w.Body.String(), "<html") {
		t.Errorf("GET under /api/: %d %q", w.Code, w.Body)
	}
	if w := b.do("GET", "/sign-out", ""); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /sign-out: %d, want 405", w.Code)
	}
	w := b.do("GET", "/no-such-page", "")
	page, _ := io.ReadAll(w.Body)
	if w.Code != 404 || !bytes.Contains(page, []byte("There is no such page here.")) || len(w.Header().Values("Set-Cookie")) != 0 {
		t.Errorf("GET a page that is not there: %d:\n%s", w.Code, page)
	}
}
