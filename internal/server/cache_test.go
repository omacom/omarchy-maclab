package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/githubtest"
)

// The header pair of a public page for a visitor with no session: the
// browser's own, and the edge's in the header only Cloudflare reads.
const (
	wantBrowser = "public, max-age=0"
	wantEdge    = "max-age=60, stale-while-revalidate=86400, stale-if-error=86400"
	edgeOnly    = "Cloudflare-CDN-Cache-Control"
)

// cacheLab is a lab with reports and sign-in, behind the tunnel.
type cacheLab struct {
	*publicLab
	gh *githubtest.Server
}

func newCacheLab(t *testing.T, config func(*Config)) *cacheLab {
	t.Helper()
	gh := githubtest.New("the-client-id", "the-client-secret")
	fake := httptest.NewServer(gh)
	t.Cleanup(fake.Close)
	gh.AddUser(ownerID, "owner")
	l := &cacheLab{gh: gh, publicLab: newPublicLab(t, func(c *Config) {
		c.GitHub = &GitHubApp{ClientID: gh.ClientID, ClientSecret: gh.ClientSecret, WebURL: fake.URL, APIURL: fake.URL}
		c.Admins = []int64{ownerID}
		if config != nil {
			config(c)
		}
	})}
	l.production()
	return l
}

// signedIn is a browser signed in as the Admin.
func (l *cacheLab) signedIn() *browser {
	l.t.Helper()
	b := (&signInLab{t: l.t, s: l.s, h: l.h, gh: l.gh}).browser()
	b.mustSignIn(ownerID)
	return b
}

// kept reports whether an answer may be kept at the edge, and fails the
// test if it says so in any way but the pair.
func kept(t *testing.T, what string, w *httptest.ResponseRecorder) bool {
	t.Helper()
	cc, edge := w.Header().Values("Cache-Control"), w.Header().Values(edgeOnly)
	if len(cc) != 1 {
		t.Errorf("%s (%d): %d Cache-Control headers %q, want one", what, w.Code, len(cc), cc)
		return false
	}
	for _, name := range []string{"CDN-Cache-Control", "Surrogate-Control", "Expires", "Pragma", "ETag", "Last-Modified", "Vary"} {
		if v := w.Header().Get(name); v != "" && len(edge) > 0 {
			t.Errorf("%s: a kept answer carries %s: %s", what, name, v)
		}
	}
	if len(edge) == 0 {
		if strings.Contains(cc[0], "s-maxage") || strings.Contains(cc[0], "stale-") {
			t.Errorf("%s: Cache-Control %q gives the edge a lifetime outside its own header", what, cc[0])
		}
		return false
	}
	if cc[0] != wantBrowser || len(edge) != 1 || edge[0] != wantEdge {
		t.Errorf("%s (%d): Cache-Control %q and %s %q, want %q and %q", what, w.Code, cc[0], edgeOnly, edge, wantBrowser, wantEdge)
	}
	if w.Code != http.StatusOK {
		t.Errorf("%s: an answer of %d may be kept", what, w.Code)
	}
	if c := w.Header().Values("Set-Cookie"); len(c) > 0 {
		t.Errorf("%s: an answer that may be kept sets a cookie: %q", what, c)
	}
	return true
}

func neverKept(t *testing.T, what string, w *httptest.ResponseRecorder, want ...string) {
	t.Helper()
	if kept(t, what, w) {
		t.Errorf("%s (%d) may be kept at the edge", what, w.Code)
		return
	}
	cc := w.Header().Get("Cache-Control")
	for _, ok := range want {
		if cc == ok {
			return
		}
	}
	t.Errorf("%s (%d): Cache-Control %q, want one of %q", what, w.Code, cc, want)
}

// firstLink is the first address under prefix a page links to.
func firstLink(t *testing.T, body, prefix string) string {
	t.Helper()
	m := regexp.MustCompile(`href="(` + regexp.QuoteMeta(prefix) + `[^"?#]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page links nothing under %s", prefix)
	}
	return m[1]
}

var exportPaths = []string{
	"/api/v1/reports", "/api/v1/reports.json", "/api/v1/reports.csv", "/api/v1/checks", "/api/v1/checks.csv",
	"/api/v1/matrix", "/api/v1/matrix.json", "/api/v1/benchmarks", "/api/v1/benchmarks.json", "/api/v1/aurora", "/api/v1/aurora.json",
}

// publicPaths are the public pages and exports of a lab with reports: the
// plain ones, and one of each kind that names something.
func (l *cacheLab) publicPaths() []string {
	l.t.Helper()
	paths := []string{"/", "/matrix", "/models", "/features", "/gaps", "/aurora", "/benchmarks", "/candidates", "/reports", "/data", "/terms", "/privacy",
		"/matrix?group=soc", "/reports?page=1"}
	paths = append(paths, exportPaths...)
	paths = append(paths, "/api/v1/matrix.json?group=soc")
	for from, prefix := range map[string]string{"/models": "/models/", "/features": "/features/", "/reports": "/reports/"} {
		paths = append(paths, firstLink(l.t, l.do("GET", from, "").Body.String(), prefix))
	}
	sort.Strings(paths)
	return paths
}

func TestPublicPagesAndExportsAreKeptAtTheEdgeForAVisitorWithNoSession(t *testing.T) {
	for _, preview := range []bool{false, true} {
		l := newCacheLab(t, func(c *Config) { c.Preview = preview })
		for _, target := range l.publicPaths() {
			for _, method := range []string{"GET", "HEAD"} {
				w := l.do(method, target, "")
				if w.Code != http.StatusOK {
					t.Errorf("%s %s: %d", method, target, w.Code)
				}
				if !kept(t, method+" "+target, w) {
					t.Errorf("Preview %v: %s %s (%d) may not be kept: Cache-Control %q", preview, method, target, w.Code, w.Header().Get("Cache-Control"))
				}
			}
		}
	}
}

// The guard: nothing rendered for a session is ever kept, and neither is
// an answer to a request that carries a session cookie labd does not know,
// which the edge's rule cannot tell from one it does.
func TestNothingRenderedForASessionIsEverKept(t *testing.T) {
	l := newCacheLab(t, nil)
	b := l.signedIn()
	for _, target := range l.publicPaths() {
		w := b.do("GET", target, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s signed in: %d", target, w.Code)
		}
		neverKept(t, "GET "+target+" signed in", w, "private, no-store")
		for _, cookie := range []string{"__Host-lab_session=0000", "lab_session=0000", "other=1; __Host-lab_session=0000"} {
			neverKept(t, "GET "+target+" with the cookie "+cookie, l.do("GET", target, "", "Cookie", cookie), "private, no-store")
		}
		neverKept(t, "GET "+target+" with a Lab token", l.do("GET", target, "", "Authorization", "Bearer lab_adm_secret"), "private, no-store", "no-store")
	}
	for _, target := range []string{"/me", "/macs", "/people"} {
		w := b.do("GET", target, "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s signed in: %d", target, w.Code)
		}
		neverKept(t, "GET "+target+" signed in", w, "private, no-store")
	}
	neverKept(t, "the API for a session", b.do("GET", "/api/auth", ""), "private, no-store")
}

func TestOnlyAGoodAnswerToAReadIsKept(t *testing.T) {
	l := newCacheLab(t, nil)
	report := firstLink(t, l.do("GET", "/reports", "").Body.String(), "/reports/")
	for target, want := range map[string]int{
		"/reports/nothing-here": 404, "/models/nothing-here": 404, "/features/nothing-here": 404, "/candidates/nothing-here": 404,
		"/nothing": 404, "/api/v1/nothing": 404,
	} {
		w := l.do("GET", target, "")
		if w.Code != want {
			t.Errorf("GET %s: %d, want %d", target, w.Code, want)
		}
		neverKept(t, "GET "+target, w, "no-store")
	}
	neverKept(t, "a deletion link", l.do("GET", report+"/deletion?token=nothing", ""), "no-store")
	neverKept(t, "a deletion sent", l.do("POST", report, "_method=delete&token=nothing", "Content-Type", "application/x-www-form-urlencoded"), "no-store")
	neverKept(t, "an upload", l.do("POST", "/api/v1/reports", "{}"), "no-store")
	neverKept(t, "the sign-in link", l.do("GET", "/sign-in", ""), "no-store")
	neverKept(t, "/status", l.do("GET", "/status", ""), "no-store")
	neverKept(t, "/healthz", l.do("GET", "/healthz", ""), "no-store")
	// The reports' store gone: an error is never kept, so the edge goes on
	// serving the copy it has.
	l.db.Close()
	l.s.reports.Close()
	w := l.do("GET", report, "")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("GET %s with the store closed: %d, want 500", report, w.Code)
	}
	neverKept(t, "GET "+report+" failing", w, "no-store")
}

// A deletion link's pages are a secret's own: never kept, with the right
// secret too, and neither is the answer to the confirmation.
func TestADeletionLinkIsNeverKept(t *testing.T) {
	l := newCacheLab(t, nil)
	got := l.reportsLab.upload("m2-max-image2")
	w := l.do("GET", pathOf(t, got.DeletionURL), "")
	if w.Code != http.StatusOK {
		t.Fatalf("the deletion link: %d %s", w.Code, w.Body)
	}
	neverKept(t, "the deletion link", w, "no-store")
	if !kept(t, "the report", l.do("GET", pathOf(t, got.ReportURL), "")) {
		t.Error("the report's own page may not be kept")
	}
	body := url.Values{"_method": {"delete"}, "token": {got.token(t)}}.Encode()
	w = l.do("POST", pathOf(t, got.ReportURL), body, "Content-Type", "application/x-www-form-urlencoded")
	if w.Code != http.StatusOK {
		t.Fatalf("the confirmation: %d %s", w.Code, w.Body)
	}
	neverKept(t, "the confirmation, sent", w, "no-store")
}

// net/http takes Cache-Control off a file it could not serve; the answer
// still states a policy.
func TestAFileThatIsNotThereStillStatesAPolicy(t *testing.T) {
	l := newCacheLab(t, nil)
	w := l.do("GET", "/api/jobs/nothing/files/nothing", "", "Authorization", "Bearer lab_adm_secret", "User-Agent", "lab/0.2.0")
	if w.Code != http.StatusNotFound {
		t.Fatalf("a job file that is not there: %d", w.Code)
	}
	neverKept(t, "a job file that is not there", w, "no-store")
}

// /install and the interface's files are the same for everyone and state
// their own policy, signed in or out.
func TestInstallAndTheFilesKeepTheirOwnPolicy(t *testing.T) {
	l := newCacheLab(t, nil)
	b := l.signedIn()
	file := pageFile(t, l.h, labHost, "/", ".css")
	for target, want := range map[string]string{"/install": "public, max-age=300", file: "public, max-age=31536000, immutable"} {
		for who, w := range map[string]*httptest.ResponseRecorder{"signed out": l.do("GET", target, ""), "signed in": b.do("GET", target, "")} {
			if w.Code != http.StatusOK {
				t.Errorf("GET %s %s: %d", target, who, w.Code)
			}
			neverKept(t, "GET "+target+" "+who, w, want)
		}
	}
}

// Under /api/ nothing but an export is ever kept, whoever asks and whatever
// the answer.
func TestTheAPIIsNeverCacheable(t *testing.T) {
	l := newCacheLab(t, nil)
	b := l.signedIn()
	export := map[string]bool{}
	for _, p := range exportPaths {
		export[p] = true
	}
	stream := map[string]bool{"GET /api/stream": true}
	n := 0
	for _, rt := range l.s.routes() {
		method, target, _ := strings.Cut(rt.pattern, " ")
		if !strings.HasPrefix(target, "/api/") || stream[rt.pattern] {
			continue
		}
		target = regexp.MustCompile(`\{[a-z]+(\.\.\.)?\}`).ReplaceAllString(target, "nothing")
		if export[target] && method == "GET" {
			t.Errorf("%s is an export among labd's own routes", rt.pattern)
		}
		n++
		neverKept(t, rt.pattern+" with no credential", l.do(method, target, "{}"), "no-store")
		neverKept(t, rt.pattern+" as a Maintainer", l.do(method, target, "{}", "Authorization", "Bearer lab_adm_secret", "User-Agent", "lab/0.2.0"), "no-store", "private")
		if method == "GET" {
			neverKept(t, rt.pattern+" for a session", b.do(method, target, ""), "private, no-store", "private")
		}
	}
	if n < 80 {
		t.Fatalf("only %d API routes were asked", n)
	}
}

// A forward on the Alias, and its refusal under /api/, are never kept.
func TestAForwardOnTheAliasIsNeverCacheable(t *testing.T) {
	l := newCacheLab(t, nil)
	for _, target := range append(l.publicPaths(), "/install", "/me", "/nothing") {
		w := l.do("GET", target, "", "Host", aliasHost)
		want := http.StatusPermanentRedirect
		if strings.HasPrefix(target, "/api/") {
			want = http.StatusMisdirectedRequest
		}
		if w.Code != want {
			t.Errorf("GET %s on the Alias: %d, want %d", target, w.Code, want)
		}
		neverKept(t, "GET "+target+" on the Alias", w, "no-store")
	}
}

// A lab under a prefix keeps the same pages, by their path under it.
func TestThePolicyFollowsThePrefix(t *testing.T) {
	h := edgeServer(t)
	for _, target := range []string{"/maclab/", "/maclab/matrix", "/maclab/terms", "/maclab/api/v1/matrix.json"} {
		w := edgeDo(h, "GET", target, viaProxy, fromIngress)
		if w.Code != http.StatusOK || !kept(t, "GET "+target, w) {
			t.Errorf("GET %s: %d, Cache-Control %q", target, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	neverKept(t, "the API under the prefix", edgeDo(h, "GET", "/maclab/api/devices", viaProxy, fromIngress), "no-store")
}

// An answer that sets a cookie is somebody's own, whatever its path.
func TestAnAnswerThatSetsACookieIsNeverKept(t *testing.T) {
	r := httptest.NewRequest("GET", "http://"+labHost+"/matrix", nil)
	hdr := http.Header{"Cache-Control": {"no-store"}}
	statePolicy(hdr, r, "/matrix", http.StatusOK)
	if hdr.Get(edgeOnly) != wantEdge {
		t.Fatalf("a public page: %v", hdr)
	}
	hdr = http.Header{"Cache-Control": {"no-store"}, "Set-Cookie": {"a=b"}}
	statePolicy(hdr, r, "/matrix", http.StatusOK)
	if hdr.Get(edgeOnly) != "" || hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("a public page that sets a cookie: %v", hdr)
	}
}

// labd and the edge's cache rules name the same paths: the pages and
// exports labd lets the edge keep are the ones the first rule makes
// cacheable, and the exports are the only paths under /api/ the second
// rule does not bypass. The third bypasses cache for any request labd
// never lets the edge keep: one with a Lab token or the session cookie.
func TestTheEdgesCacheRulesNameThePathsLabdKeeps(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "scripts", "cloud", "edge.json"))
	if err != nil {
		t.Fatal(err)
	}
	var edge struct {
		Rules []struct {
			Phase string `json:"phase"`
			Want  []struct {
				Expression string `json:"expression"`
				Parameters struct {
					Cache bool `json:"cache"`
				} `json:"action_parameters"`
			} `json:"want"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(data, &edge); err != nil {
		t.Fatal(err)
	}
	var public, bypass, guard string
	for _, r := range edge.Rules {
		if r.Phase == "http_request_cache_settings" && len(r.Want) == 3 && r.Want[0].Parameters.Cache && !r.Want[1].Parameters.Cache && !r.Want[2].Parameters.Cache {
			public, bypass, guard = r.Want[0].Expression, r.Want[1].Expression, r.Want[2].Expression
		}
	}
	if public == "" {
		t.Fatal("edge.json has no three cache rules: the public paths, the bypass under /api/, then the bypass for a Lab token or the session cookie")
	}
	const token = `any(lower(http.request.headers.names[*])[*] eq "authorization")`
	if guard != `(`+token+` or http.cookie contains "lab_session=")` {
		t.Errorf("the last cache rule is not a request with a Lab token or the session cookie: %s", guard)
	}
	set := func(x string) []string {
		m := regexp.MustCompile(`http\.request\.uri\.path in \{([^}]*)\}`).FindStringSubmatch(x)
		if m == nil {
			t.Fatalf("no set of paths in %s", x)
		}
		var out []string
		for _, q := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(m[1], -1) {
			out = append(out, q[1])
		}
		sort.Strings(out)
		return out
	}
	var prefixes []string
	for _, m := range regexp.MustCompile(`starts_with\(http\.request\.uri\.path, "([^"]*)"\)`).FindAllStringSubmatch(public, -1) {
		prefixes = append(prefixes, m[1])
	}
	sort.Strings(prefixes)
	exact := append([]string{}, keptPaths...)
	sort.Strings(exact)
	under := append([]string{}, keptUnder...)
	sort.Strings(under)
	if got := set(public); !reflect.DeepEqual(got, exact) {
		t.Errorf("the edge's rule names the paths\n%q\nlabd keeps\n%q", got, exact)
	}
	if !reflect.DeepEqual(prefixes, under) {
		t.Errorf("the edge's rule names the prefixes %q; labd keeps %q", prefixes, under)
	}
	for _, guard := range []string{`not http.cookie contains "lab_session="`, `not ` + token, `not ends_with(http.request.uri.path, "` + neverKeptEnd + `")`} {
		if !strings.Contains(public, guard) {
			t.Errorf("the edge's rule for public paths lacks %s", guard)
		}
	}
	exports := append([]string{}, exportPaths...)
	sort.Strings(exports)
	if got := set(bypass); !reflect.DeepEqual(got, exports) {
		t.Errorf("the bypass under /api/ leaves out\n%q\nthe exports are\n%q", got, exports)
	}
	if !strings.Contains(bypass, `starts_with(http.request.uri.path, "/api/")`) || !strings.Contains(bypass, `http.cookie contains "lab_session="`) {
		t.Errorf("the bypass is not everything under /api/ but an export asked for without the session cookie: %s", bypass)
	}
	for _, p := range exports {
		if !edgeKeeps(p) {
			t.Errorf("labd does not keep the export %s", p)
		}
	}
	for _, p := range []string{"/api/", "/api/auth", "/api/devices", "/api/v1/tester_requests", "/api/v1/reports/x", "/status", "/me", "/sign-in", "/healthz", "/reports/x/deletion", "/installer", "/assets/app.css", "/join"} {
		if edgeKeeps(p) {
			t.Errorf("labd keeps %s", p)
		}
	}
}

// The pair chosen is written down where the Front door's test 13 reads it.
func TestTheHeaderPairIsWrittenDown(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "acceptance", "cache-policy.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Cache-Control: " + wantBrowser, edgeOnly + ": " + wantEdge, "Cache-Control: private, no-store", "Cache-Control: no-store", "FD 13"} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("docs/acceptance/cache-policy.md does not say %q", want)
		}
	}
	if cacheBrowser != wantBrowser || cacheEdge != wantEdge || cacheEdgeHeader != edgeOnly {
		t.Errorf("labd's pair is %q and %s: %q", cacheBrowser, cacheEdgeHeader, cacheEdge)
	}
}
