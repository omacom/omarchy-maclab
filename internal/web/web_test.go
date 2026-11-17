package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

var update = flag.Bool("update", false, "write static/*.br again from the files beside them")

func site(t *testing.T, base string) *Site {
	t.Helper()
	s, err := New(base)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func page(t *testing.T, s *Site, name string, req Request, p Page) (string, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	if err := s.Render(w, name, req, p); err != nil {
		t.Fatal(err)
	}
	return w.Body.String(), w
}

func viewer(k Kind) Request {
	req := Request{Viewer: Viewer{Kind: k}, Path: "/terms", Fleet: &Fleet{Macs: 9, Free: 4, JobsRunning: 1, NeedsHands: 1}}
	if k != SignedOut {
		req.Viewer.Handle, req.Viewer.CSRF = "ravi-okonkwo", "the-csrf-token"
	}
	return req
}

// between is the part of s from the first start to the next end.
func between(t *testing.T, s, start, end string) string {
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

var reLink = regexp.MustCompile(`<a [^>]*>(.*?)</a>`)

// labels are the texts of the links in a piece of a page.
func labels(piece string) string {
	var out []string
	for _, m := range reLink.FindAllStringSubmatch(piece, -1) {
		out = append(out, strings.TrimSpace(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], " ")))
	}
	return strings.Join(out, ", ")
}

func TestRow1IsTheSameForEveryViewer(t *testing.T) {
	s := site(t, "")
	signIn := regexp.MustCompile(`<a class="sign-in"[^>]*>Sign in</a>\n`)
	var first string
	for _, k := range Kinds {
		body, _ := page(t, s, "home", viewer(k), Page{})
		row := between(t, body, `<div class="header-row">`, `</div>`)
		if got := signIn.MatchString(row); got != (k == SignedOut) {
			t.Errorf("%s: a Sign in link at the end of row 1: %v", k, got)
		}
		row = signIn.ReplaceAllString(row, "")
		if first == "" {
			first = row
			if got, want := labels(row), "omarchy  mac lab, Matrix, Models, Features, Kernel gaps, Aurora, Benchmarks, Candidates, Reports, Data, Install"; got != want {
				t.Errorf("row 1 is %q, want %q", got, want)
			}
		}
		if row != first {
			t.Errorf("%s: row 1 differs from a signed-out visitor's:\n%s\n%s", k, row, first)
		}
	}
}

func TestRow2MatchesTheTableForEachOfTheSixViewers(t *testing.T) {
	s := site(t, "")
	for k, want := range map[Kind]string{
		NoRole:     "Lend a Mac",
		External:   "My Macs",
		MTeam:      "Macs, Jobs, Releases",
		Maintainer: "Macs, Jobs, Builds, Releases, People",
		Admin:      "Macs, Jobs, Builds, Releases, People",
	} {
		body, _ := page(t, s, "home", viewer(k), Page{})
		row := between(t, body, `<div class="lab-row">`, `</div>`)
		if !strings.Contains(row, `<span class="lab-tag">lab</span>`) {
			t.Errorf("%s: row 2 has no lab tag", k)
		}
		if got := labels(between(t, row, `<nav class="lab-nav"`, `</nav>`)); got != want {
			t.Errorf("%s: row 2 is %q, want %q", k, got, want)
		}
		if got := strings.Contains(row, `<span class="fleet">4/9 Macs free · 1 job running · <b class="bad">1 needs hands</b></span>`); got != MTeamUp.Allows(k) {
			t.Errorf("%s: the fleet status in row 2: %v", k, got)
		}
		for _, piece := range []string{
			`<a class="handle" href="/me">@ravi-okonkwo</a>`,
			`<span class="badge ` + k.BadgeClass() + `">` + k.Badge() + `</span>`,
			`action="/sign-out"><input type="hidden" name="csrf_token" value="the-csrf-token"><button class="link-button" type="submit">Sign out</button>`,
		} {
			if !strings.Contains(row, piece) {
				t.Errorf("%s: row 2 lacks %s:\n%s", k, piece, row)
			}
		}
	}
	body, _ := page(t, s, "home", viewer(SignedOut), Page{})
	for _, never := range []string{"lab-row", "lab-tag", "csrf", "fleet"} {
		if strings.Contains(body, never) {
			t.Errorf("signed out, the page has %q", never)
		}
	}
}

func TestPeopleCarriesItsCountOfPendingRequests(t *testing.T) {
	s := site(t, "")
	req := viewer(Maintainer)
	req.PendingRequests = 2
	body, _ := page(t, s, "home", req, Page{Nav: "people"})
	if want := `<a href="/people" class="current" aria-current="page">People<b class="count">2</b></a>`; !strings.Contains(body, want) {
		t.Errorf("People in row 2 lacks its count, or is not the current page:\n%s", between(t, body, `<nav class="lab-nav"`, `</nav>`))
	}
	req.PendingRequests = 0
	if body, _ := page(t, s, "home", req, Page{}); strings.Contains(body, `class="count"`) {
		t.Error("People carries a count with nothing pending")
	}
}

func TestEveryPagesFooterLinksTheTwoTexts(t *testing.T) {
	s := site(t, "")
	for name := range s.pages {
		for _, k := range Kinds {
			data := map[string]any{"text": Terms(), "ask_twice": Asking{Action: "/people/7/remove", Sentence: "Remove: 1 Lease ends now", Back: "/people"}}[name]
			body, _ := page(t, s, name, viewer(k), Page{Title: "A page", Data: data})
			foot := between(t, body, `<footer class="site-footer">`, `</footer>`)
			for _, link := range []string{`<a href="/terms">Terms</a>`, `<a href="/privacy">Privacy</a>`} {
				if !strings.Contains(foot, link) {
					t.Errorf("%s, %s: the footer lacks %s", name, k, link)
				}
			}
			for _, never := range []string{"cookie", "consent", "theme"} {
				if strings.Contains(strings.ToLower(strings.ReplaceAll(body, "theme-color", "")), never) && name != "text" {
					t.Errorf("%s, %s: the page says %q", name, k, never)
				}
			}
		}
	}
}

func TestATextShowsItsDateAndIsTheShippedFile(t *testing.T) {
	s := site(t, "")
	dates := map[string]string{"terms.txt": "2026-11-14", "privacy.txt": "2026-11-16"}
	for file, text := range map[string]Text{"terms.txt": Terms(), "privacy.txt": Privacy()} {
		date := dates[file]
		shipped, err := os.ReadFile("texts/" + file)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(shipped)
		if text.SHA256 != hex.EncodeToString(sum[:]) || text.Body != string(shipped) {
			t.Errorf("%s: labd carries a text that is not the file", file)
		}
		if text.Date != date {
			t.Errorf("%s: its date is %q", file, text.Date)
		}
		body, _ := page(t, s, "text", viewer(SignedOut), Page{Title: "Terms", Data: text})
		served := sha256.Sum256([]byte(html.UnescapeString(between(t, body, `<pre class="legal">`, `</pre>`)[len(`<pre class="legal">`):])))
		if hex.EncodeToString(served[:]) != text.SHA256 {
			t.Errorf("%s: the page's text is not the shipped file's", file)
		}
		for _, piece := range []string{`<time datetime="` + date + `">` + date + `</time>`, `<code data-copy>` + text.SHA256 + `</code>`} {
			if !strings.Contains(body, piece) {
				t.Errorf("%s: the page lacks %s", file, piece)
			}
		}
	}
	if !strings.HasPrefix(Terms().Body, "OMARCHY MAC LAB: LENDING A MAC\nTerms of 2026-11-14\n") || !strings.HasPrefix(Privacy().Body, "PRIVACY\nStatement of 2026-11-16\n") {
		t.Error("the two texts do not open as the spec's do")
	}
	// What a forgetting leaves, as the spec's Forgetting table has it.
	if said := strings.Join(strings.Fields(Privacy().Body), " "); !strings.Contains(said, "the approver's handle in a Release frozen before 2026-11-16") {
		t.Error("the privacy statement does not name the approver's handle in a Release frozen before 2026-11-16 as kept")
	}
}

func TestPreviewPutsTheBannerOnPublicPagesOnly(t *testing.T) {
	s := site(t, "")
	banner := `The live site is <a href="https://omarchy-m-testing.org">omarchy-m-testing.org</a>.`
	for _, c := range []struct {
		preview, public, want bool
	}{{true, true, true}, {true, false, false}, {false, true, false}} {
		req := viewer(SignedOut)
		req.Preview = c.preview
		body, _ := page(t, s, "home", req, Page{Public: c.public})
		if got := strings.Contains(body, banner); got != c.want {
			t.Errorf("Preview %v, a public page %v: the banner: %v", c.preview, c.public, got)
		}
		if got := strings.Contains(body, `<meta name="robots" content="noindex">`); got != c.preview {
			t.Errorf("Preview %v: noindex in the page: %v", c.preview, got)
		}
	}
}

func TestAPageUnderAPrefixNamesNothingOutsideIt(t *testing.T) {
	s := site(t, "/maclab")
	for _, k := range []Kind{SignedOut, Admin} {
		body, _ := page(t, s, "home", viewer(k), Page{})
		for _, m := range regexp.MustCompile(`(?:href|src|action)="([^"]*)"`).FindAllStringSubmatch(body, -1) {
			if u := m[1]; !strings.HasPrefix(u, "/maclab/") && !strings.HasPrefix(u, "https://") {
				t.Errorf("%s: the page names %s, outside the prefix", k, u)
			}
		}
		if k == SignedOut && !strings.Contains(body, `href="/maclab/sign-in?return=%2Fterms"`) {
			t.Errorf("Sign in does not come back to the page:\n%s", between(t, body, `<div class="header-row">`, `</div>`))
		}
	}
}

func TestASignedInPageIsNeverStored(t *testing.T) {
	s := site(t, "")
	if _, w := page(t, s, "home", viewer(NoRole), Page{}); w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("a signed-in page: Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	_, w := page(t, s, "home", viewer(SignedOut), Page{})
	if cc := w.Header().Get("Cache-Control"); cc != "" {
		t.Errorf("a signed-out page states %q itself", cc)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("the page's content security policy: %q", csp)
	}
	if body, _ := page(t, s, "home", viewer(Admin), Page{}); strings.Contains(body, "<style") || strings.Contains(body, ` style="`) || regexp.MustCompile(`<script(>| [^>]*[^c]>)[^<]`).MatchString(body) {
		t.Error("the page carries inline style or script, which its policy refuses")
	}
}

func TestAnActionThatTakesSomethingAwayCarriesItsSentence(t *testing.T) {
	s := site(t, "/maclab")
	v := view{Request: viewer(Maintainer), site: s}
	var buf bytes.Buffer
	if err := s.pages["home"].ExecuteTemplate(&buf, "take", v.Take("/people/7/remove", "Remove", "Remove: 1 Lease ends now", "who", "7")); err != nil {
		t.Fatal(err)
	}
	want := `<form class="inline take" method="post" action="/maclab/people/7/remove"><input type="hidden" name="csrf_token" value="the-csrf-token"><input type="hidden" name="who" value="7"><button class="button button-danger button-small" type="submit" data-sentence="Remove: 1 Lease ends now">Remove</button></form>`
	if buf.String() != want {
		t.Errorf("the button:\n%s\nwant:\n%s", buf.String(), want)
	}
	js, err := staticFiles.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, piece := range []string{"button[data-sentence]", "dataset.sentence", `sure.name = "sure"`, "preventDefault"} {
		if !bytes.Contains(js, []byte(piece)) {
			t.Errorf("app.js lacks %q: the first click would not turn the button into its sentence", piece)
		}
	}
}

func get(s *Site, target string, hdr ...string) *httptest.ResponseRecorder {
	m := http.NewServeMux()
	s.Register(m)
	r := httptest.NewRequest("GET", target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

func brotli11(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	if _, err := w.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Run with -update after changing a file in static/.
func TestAssetsAreFingerprintedKeptAYearAndShippedCompressed(t *testing.T) {
	s := site(t, "")
	reServed := regexp.MustCompile(`^/assets/[A-Za-z0-9-]+-[0-9a-f]{16}\.[a-z0-9]+$`)
	for name, a := range s.assets {
		addr, err := s.Asset(name)
		if err != nil || !reServed.MatchString(addr) {
			t.Errorf("%s is loaded from %q (%v), which carries no fingerprint", name, addr, err)
			continue
		}
		sum := sha256.Sum256(a.body)
		if !strings.Contains(addr, "-"+hex.EncodeToString(sum[:8])+".") {
			t.Errorf("%s: the fingerprint in %s is not its content's", name, addr)
		}
		plain := get(s, addr)
		if plain.Code != 200 || !bytes.Equal(plain.Body.Bytes(), a.body) || plain.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s as it is: %d, %d bytes, encoding %q", name, plain.Code, plain.Body.Len(), plain.Header().Get("Content-Encoding"))
		}
		if cc := plain.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("%s: Cache-Control %q, want public for one year", name, cc)
		}
		if got, want := plain.Header().Get("Content-Type"), contentTypes[name[strings.LastIndex(name, "."):]]; got != want || want == "" {
			t.Errorf("%s: Content-Type %q, want %q", name, got, want)
		}
		small := get(s, addr, "Accept-Encoding", "gzip, deflate, br, zstd")
		if !compressed[name[strings.LastIndex(name, "."):]] {
			if small.Header().Get("Content-Encoding") != "" || a.br != nil {
				t.Errorf("%s is compressed again", name)
			}
			continue
		}
		want := brotli11(t, a.body)
		if *update {
			if err := os.WriteFile("static/"+name+".br", want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if !bytes.Equal(a.br, want) {
			t.Errorf("static/%s.br is not %s at Brotli's highest level: run go test ./internal/web -update", name, name)
			continue
		}
		if small.Header().Get("Content-Encoding") != "br" || small.Header().Get("Vary") != "Accept-Encoding" || !bytes.Equal(small.Body.Bytes(), a.br) {
			t.Errorf("%s for a browser that takes Brotli: encoding %q, Vary %q, %d bytes; want the %d shipped", name,
				small.Header().Get("Content-Encoding"), small.Header().Get("Vary"), small.Body.Len(), len(a.br))
		}
		back, err := io.ReadAll(brotli.NewReader(bytes.NewReader(small.Body.Bytes())))
		if err != nil || !bytes.Equal(back, a.body) {
			t.Errorf("%s does not come back from its Brotli form (%v)", name, err)
		}
		if w := get(s, addr, "Accept-Encoding", "gzip, br;q=0"); w.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s was sent as Brotli to a browser that refuses it", name)
		}
	}
	if *update {
		t.Skip("wrote static/*.br")
	}
	entries, _ := os.ReadDir("static")
	for _, e := range entries {
		if plain, isBr := strings.CutSuffix(e.Name(), ".br"); isBr && s.assets[plain] == nil {
			t.Errorf("static/%s is left over: static/ holds no %s", e.Name(), plain)
		}
	}
	for _, target := range []string{"/assets/app.css", "/assets/app-0000000000000000.css", "/assets/app.css.br", "/assets/JetBrainsMono-OFL.txt"} {
		if w := get(s, target); w.Code != 404 {
			t.Errorf("GET %s: %d, want 404", target, w.Code)
		}
	}
	if w := get(s, "/robots.txt"); w.Code != 200 || !strings.Contains(w.Body.String(), "User-agent: *") {
		t.Errorf("GET /robots.txt: %d %q", w.Code, w.Body)
	}
}

func TestTheStylesheetLoadsItsFontsByFingerprint(t *testing.T) {
	s := site(t, "")
	css := string(s.assets["app.css"].body)
	refs := reCSSURL.FindAllStringSubmatch(css, -1)
	if len(refs) != 3 {
		t.Fatalf("the stylesheet loads %d files, want the three weights of JetBrains Mono", len(refs))
	}
	for _, m := range refs {
		if w := get(s, "/assets/"+m[1]); w.Code != 200 || w.Header().Get("Content-Type") != "font/woff2" {
			t.Errorf("the stylesheet loads %s: %d %s", m[1], w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, rule := range []string{
		"@media (max-width: 760px)",
		".header-row .site-nav { flex-wrap: nowrap; order: 5; overflow-x: auto;",
		"overflow-x: auto;\n  padding: 0.45rem clamp(1rem, 4vw, 3rem);",
		".stack td[data-label]::before",
		".stack tr { border-bottom: 1px solid rgb(65 72 104 / 0.35); display: block;",
	} {
		if !strings.Contains(css, rule) {
			t.Errorf("the stylesheet lacks %q: at phone width each row scrolls sideways and tables stack", rule)
		}
	}
	if strings.Contains(css, "prefers-color-scheme") || strings.Contains(css, "data-theme") {
		t.Error("the stylesheet has a second theme; the lab is dark only")
	}
}

func TestAPageThatIsNotThereIsAnError(t *testing.T) {
	s := site(t, "")
	if err := s.Render(httptest.NewRecorder(), "nope", viewer(SignedOut), Page{}); err == nil {
		t.Error("rendering a page templates/ does not hold gave no error")
	}
	if _, err := s.Asset("nope.css"); err == nil {
		t.Error("a file static/ does not hold has an address")
	}
}

// A value to copy has its Copy button beside it in one .copyable: a real
// button, hidden until app.js shows it, naming what it copies.
func TestAValueToCopyHasItsCopyButtonBesideIt(t *testing.T) {
	s := site(t, "")
	button := `<button class="button button-small button-plain copy-button" type="button" aria-label="Copy the Join command" hidden>Copy</button>`
	for piece, want := range map[string]string{
		"copy":       `<span class="copyable"><code data-copy>sudo lab-agent &lt;update&gt;</code> ` + strings.Replace(button, "the Join command", "the update command", 1) + `</span>`,
		"copy-block": `<div class="copyable copyable-block"><pre class="command"><span class="prompt" aria-hidden="true">$ </span><code data-copy>curl -fsSL x | bash</code></pre> ` + button + `</div>`,
		"copy-title": `<div class="copyable copyable-title"><h1 data-copy>j-1</h1> ` + strings.Replace(button, "the Join command", "the job id", 1) + `</div>`,
	} {
		c := map[string]Copyable{
			"copy":       copyOf("the update command", "sudo lab-agent <update>"),
			"copy-block": copyOf("the Join command", "curl -fsSL x | bash"),
			"copy-title": copyOf("the job id", "j-1"),
		}[piece]
		var buf bytes.Buffer
		if err := s.pages["home"].ExecuteTemplate(&buf, piece, c); err != nil {
			t.Fatal(err)
		}
		if buf.String() != want {
			t.Errorf("%s:\n%s\nwant:\n%s", piece, buf.String(), want)
		}
	}
	if c := copyOf("the install command", "curl", "install"); c.ID != "install" {
		t.Errorf("copy with an id: %+v", c)
	}
}

// The Copy buttons work from app.js, the lab's own file: served as
// JavaScript, kept a year, loaded by every page under a policy that refuses
// inline script, with no handler written into the page.
func TestTheCopyButtonsWorkFromTheLabsOwnScript(t *testing.T) {
	s := site(t, "")
	body, w := page(t, s, "text", viewer(SignedOut), Page{Title: "Terms", Data: Terms()})
	addr, err := s.Asset("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `<script src="`+addr+`" defer></script>`) {
		t.Errorf("the page does not load %s", addr)
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("the page's policy: %q", csp)
	}
	if !strings.Contains(body, `aria-label="Copy the sha256 of this text" hidden>Copy</button>`) {
		t.Error("the text's sha256 has no Copy button")
	}
	if regexp.MustCompile(`<script(>| [^>]*[^c]>)[^<]`).MatchString(body) || regexp.MustCompile(`\son[a-z]+="`).MatchString(body) || strings.Contains(body, ` style="`) {
		t.Error("the page carries inline script, a handler or inline style, which its policy refuses")
	}
	js := get(s, addr)
	if js.Code != 200 || js.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("GET %s: %d %q", addr, js.Code, js.Header().Get("Content-Type"))
	}
	for _, piece := range []string{`"button.copy-button"`, `.closest(".copyable")`, `"[data-copy]"`, "hidden = false", "navigator.clipboard.writeText", "selectNodeContents", `"aria-live"`, `"Copied"`, "2000"} {
		if !strings.Contains(js.Body.String(), piece) {
			t.Errorf("app.js lacks %q: a Copy button would not show, copy, fall back to selecting or say so", piece)
		}
	}
	css := get(s, mustAsset(t, s, "app.css")).Body.String()
	for _, piece := range []string{".copy-button[hidden] { display: none; }", ".copyable-block", ".visually-hidden"} {
		if !strings.Contains(css, piece) {
			t.Errorf("app.css lacks %q", piece)
		}
	}
}

func mustAsset(t *testing.T, s *Site, name string) string {
	t.Helper()
	addr, err := s.Asset(name)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
