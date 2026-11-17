package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// edgeServer is a labd served as https://chonkbase.net/maclab through the
// tunnel.
func edgeServer(t *testing.T) http.Handler {
	t.Helper()
	return labServer(t, "https://chonkbase.net/maclab", nil).Handler()
}

// labServer is a labd whose Lab address is pub.
func labServer(t *testing.T, pub string, config func(*Config)) *Server {
	t.Helper()
	u, err := ParsePublicURL(pub)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := Config{DataDir: t.TempDir(), PublicURL: u}
	if config != nil {
		config(&cfg)
	}
	s, err := newWith("ada", "lab_adm_secret", ctx, cfg, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	stops.Store(s, cancel)
	return s
}

// stops is each labServer's stop: the end of the context it started with,
// as a signal ends labd's.
var stops sync.Map

// stopLabd stops a labServer as a Deploy's restart does.
func stopLabd(t *testing.T, s *Server) {
	t.Helper()
	stop, ok := stops.Load(s)
	if !ok {
		t.Fatal("this labd was not made by labServer")
	}
	stop.(context.CancelFunc)()
}

const (
	viaProxy = "@"               // the tunnel's socket, where cloudflared connects
	direct   = "127.0.0.1:40000" // a caller on the mini, at the loopback port
	loopback = "127.0.0.1:7770"  // the name such a caller asks for
)

var fromIngress = map[string]string{"X-Forwarded-Proto": "https"}

// edgeDo asks h as cloudflared does when remote is viaProxy, keeping the
// Host, and as a caller on the mini otherwise.
func edgeDo(h http.Handler, method, path, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	host := "chonkbase.net"
	if remote != viaProxy {
		host = loopback
	}
	return edgeAt(h, method, host, path, remote, hdr)
}

// edgeAt is edgeDo for a request that names host.
func edgeAt(h http.Handler, method, host, path, remote string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://"+host+path, strings.NewReader(`{}`))
	r.RemoteAddr = remote
	if remote == viaProxy {
		r = r.WithContext(onTunnel(r.Context()))
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// pageFile is the address the page at home loads its file ending in ext
// from, as cloudflared would ask for it.
func pageFile(t *testing.T, h http.Handler, host, home, ext string) string {
	t.Helper()
	body := edgeAt(h, "GET", host, home, viaProxy, fromIngress).Body.String()
	m := regexp.MustCompile(`(?:href|src)="([^"]*/assets/[^"]+` + regexp.QuoteMeta(ext) + `)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("the page at %s loads no %s file:\n%s", home, ext, body)
	}
	return m[1]
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// labd serves everything under its public URL's path, once; nothing outside
// it, not /maclabx, and no X-Forwarded-Prefix changes that.
func TestPrefixIsServedExactlyOnce(t *testing.T) {
	h := edgeServer(t)
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	script := pageFile(t, h, "chonkbase.net", "/maclab/", ".js")
	for _, c := range []struct {
		name, path string
		hdr        map[string]string
		want       int
	}{
		{"the UI", "/maclab/", fromIngress, 200},
		{"the UI without its slash", "/maclab", fromIngress, 200},
		{"its script", script, fromIngress, 200},
		{"its script outside the prefix", strings.TrimPrefix(script, "/maclab"), fromIngress, 404},
		{"a page", "/maclab/terms", fromIngress, 200},
		{"the API", "/maclab/api/devices", tok, 200},
		{"the API outside the prefix", "/api/devices", tok, 404},
		{"the UI outside the prefix", "/", fromIngress, 404},
		{"a sibling path", "/maclabx/api/devices", tok, 404},
		{"a sibling path's UI", "/maclab-old/", fromIngress, 404},
		{"the prefix twice", "/maclab/maclab/api/devices", tok, 404},
		{"the probe", "/maclab/healthz", fromIngress, 200},
		{"the probe outside the prefix", "/healthz", fromIngress, 404},
		{"a forwarded prefix", "/other/api/devices", with(tok, "X-Forwarded-Prefix", "/other"), 404},
		{"a forwarded prefix inside", "/maclab/api/devices", with(tok, "X-Forwarded-Prefix", "/other"), 200},
	} {
		if w := edgeDo(h, "GET", c.path, viaProxy, c.hdr); w.Code != c.want {
			t.Errorf("%s: GET %s = %d, want %d: %s", c.name, c.path, w.Code, c.want, w.Body)
		}
	}
	for _, home := range []string{"/maclab/", "/maclab", "/maclab/terms", "/maclab/no-such-page"} {
		page := edgeDo(h, "GET", home, viaProxy, fromIngress).Body.String()
		urls := regexp.MustCompile(`(?:href|src|action)="([^"]*)"`).FindAllStringSubmatch(page, -1)
		if len(urls) < 20 {
			t.Fatalf("GET %s: a page with %d URLs:\n%s", home, len(urls), page)
		}
		for _, m := range urls {
			if !strings.HasPrefix(m[1], "/maclab/") && !strings.HasPrefix(m[1], "https://") {
				t.Errorf("GET %s: the page has the URL %s, which would leave the prefix", home, m[1])
			}
		}
	}
	js := edgeDo(h, "GET", script, viaProxy, fromIngress).Body.String()
	for _, p := range []string{"'/api/", "`/api/", `"/api/`, "'/assets/", "`/assets/", `"/assets/`, "location.origin"} {
		if js == "" || strings.Contains(js, p) {
			t.Errorf("app.js still has %q, which would leave the prefix", p)
		}
	}
	css := edgeDo(h, "GET", pageFile(t, h, "chonkbase.net", "/maclab/", ".css"), viaProxy, fromIngress).Body.String()
	if css == "" || strings.Contains(css, `url("/`) || strings.Contains(css, "url(/") {
		t.Errorf("the stylesheet loads a file from the root, which would leave the prefix")
	}
}

// labd answers to its Lab address on the tunnel's socket and to this
// machine's own names on the loopback port; any other name is refused, and
// what a caller on the mini forwards is not believed.
func TestAThirdHostNameIsRefused(t *testing.T) {
	h := edgeServer(t)
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	for _, c := range []struct {
		name, host, remote string
		hdr                map[string]string
		want               int
	}{
		{"the Lab address", "chonkbase.net", viaProxy, tok, 200},
		{"its default port named", "chonkbase.net:443", viaProxy, tok, 200},
		{"the host in another case", "ChonkBase.NET", viaProxy, tok, 200},
		{"no scheme forwarded", "chonkbase.net", viaProxy, map[string]string{"Authorization": "Bearer lab_adm_secret"}, 200},
		{"plain http forwarded", "chonkbase.net", viaProxy, with(tok, "X-Forwarded-Proto", "http"), 421},
		{"a third name", "evil.example", viaProxy, tok, 421},
		{"a third name, the Lab address forwarded", "evil.example", viaProxy, with(tok, "X-Forwarded-Host", "chonkbase.net"), 421},
		{"another port", "chonkbase.net:8443", viaProxy, tok, 421},
		{"a subdomain", "www.chonkbase.net", viaProxy, tok, 421},
		{"this machine's own name on the socket", loopback, viaProxy, tok, 421},
		{"the loopback port", loopback, direct, tok, 200},
		{"the loopback port by name", "localhost:7770", direct, tok, 200},
		{"the loopback port over IPv6", "[::1]:7770", direct, tok, 200},
		{"the loopback port: what is forwarded is ignored", loopback, direct, with(tok, "X-Forwarded-Host", "evil.example", "X-Forwarded-Proto", "gopher"), 200},
		{"the Lab address on the loopback port", "chonkbase.net", direct, tok, 421},
		{"a third name on the loopback port", "evil.example", direct, tok, 421},
		{"a LAN address on the loopback port", "192.168.1.20:7770", direct, tok, 421},
	} {
		w := edgeAt(h, "GET", c.host, "/maclab/api/devices", c.remote, c.hdr)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
		if w.Code == 421 && !strings.Contains(w.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s: the refusal isn't sandboxed", c.name)
		}
		if w.Code == 421 && !strings.Contains(w.Body.String(), "https://chonkbase.net/maclab") {
			t.Errorf("%s: the refusal doesn't name the Lab address: %s", c.name, w.Body)
		}
	}
}

// A browser's Origin must be the very origin labd was reached at: scheme,
// host and port.
func TestBrowserOriginMustMatchSchemeHostAndPort(t *testing.T) {
	h := edgeServer(t)
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	for _, c := range []struct {
		name, remote string
		hdr          map[string]string
		refused      bool
	}{
		{"labd's own page", viaProxy, with(tok, "Origin", "https://chonkbase.net"), false},
		{"labd's own page, default port named", viaProxy, with(tok, "Origin", "https://chonkbase.net:443"), false},
		{"an http page on labd's host", viaProxy, with(tok, "Origin", "http://chonkbase.net"), true},
		{"another port", viaProxy, with(tok, "Origin", "https://chonkbase.net:8443"), true},
		{"a subdomain", viaProxy, with(tok, "Origin", "https://evil.chonkbase.net"), true},
		{"another site", viaProxy, with(tok, "Origin", "https://evil.example"), true},
		{"a sandboxed frame", viaProxy, with(tok, "Origin", "null"), true},
		{"a sibling site, no Origin", viaProxy, with(tok, "Sec-Fetch-Site", "same-site"), true},
		// At the loopback port labd was reached at http://127.0.0.1:7770:
		// the https page is another origin, forwarded headers or not.
		{"an https page, at the loopback port", direct, with(tok, "Origin", "https://chonkbase.net"), true},
		{"a page at the loopback port", direct, with(tok, "Origin", "http://127.0.0.1:7770"), false},
	} {
		w := edgeDo(h, "POST", "/maclab/api/jobs", c.remote, c.hdr)
		if refused := w.Code == 403 && strings.Contains(w.Body.String(), "refused: a"); refused != c.refused {
			t.Errorf("%s: %d %s, want refused=%v", c.name, w.Code, w.Body, c.refused)
		}
	}
}

// Every answer is nosniff and no-referrer; API answers, uploaded job files and
// screens included, are sandboxed, so an uploaded page runs no script on
// labd's origin; the UI can't be framed; https answers carry HSTS.
func TestEveryAnswerCarriesTheSecurityHeaders(t *testing.T) {
	h := edgeServer(t)
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	for _, c := range []struct {
		path    string
		sandbox bool
	}{
		{"/maclab/", false},
		{pageFile(t, h, "chonkbase.net", "/maclab/", ".js"), false},
		{"/maclab/terms", false},
		{"/maclab/no-such-page", false},
		{"/maclab/api/devices", true},
		{"/maclab/api/jobs/nope/files/evil.html", true},
		{"/maclab/api/devices/nope/screens/x.jpg", true},
		{"/nowhere", true},
	} {
		w := edgeDo(h, "GET", c.path, viaProxy, tok)
		hd := w.Header()
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: %v", c.path, hd)
		}
		csp := hd.Get("Content-Security-Policy")
		if strings.Contains(csp, "sandbox") != c.sandbox || (!c.sandbox && !strings.Contains(csp, "frame-ancestors 'none'")) {
			t.Errorf("%s: Content-Security-Policy %q", c.path, csp)
		}
		if !strings.HasPrefix(hd.Get("Strict-Transport-Security"), "max-age=") {
			t.Errorf("%s: no HSTS over https", c.path)
		}
	}
	if w := edgeDo(h, "GET", "/maclab/", direct, nil); w.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS on a plain http answer")
	}
}

func TestPublicURLAndAliasRefuseWhatTheyCantMean(t *testing.T) {
	for _, bad := range []string{"ftp://lab", "https://", "lab.example.com/maclab", "https://u:p@lab/maclab", "https://lab/maclab?x=1",
		"https://lab/maclab#x", "https://lab/a/../b", "https://lab/a b", "https://lab/%2e%2e", "https://lab/.hidden"} {
		if _, err := ParsePublicURL(bad); err == nil {
			t.Errorf("--public-url %q was taken", bad)
		}
	}
	u, err := ParsePublicURL("https://ChonkBase.NET:443/maclab/")
	if err != nil || u.String() != "https://chonkbase.net/maclab" {
		t.Errorf("a public URL: %v %v", u, err)
	}
	for _, bad := range []string{"https://maclab.omarchy.org", "maclab.omarchy.org/x", "a b", "maclab.omarchy.org:8443", ""} {
		if a, err := ParseAlias(bad); err == nil {
			t.Errorf("--alias %q was taken as %q", bad, a)
		}
	}
	if a, err := ParseAlias("MacLab.Omarchy.ORG."); err != nil || a != "maclab.omarchy.org" {
		t.Errorf("an Alias: %q %v", a, err)
	}
	pub, _ := ParsePublicURL("https://lab.omarchy-mac.org")
	for name, cfg := range map[string]Config{
		"an Alias without a Lab address":   {Alias: "maclab.omarchy.org"},
		"an Alias that is the Lab address": {Alias: "lab.omarchy-mac.org", PublicURL: pub},
	} {
		cfg.DataDir = t.TempDir()
		if _, err := New(context.Background(), cfg, log.New(io.Discard, "", 0)); err == nil {
			t.Errorf("%s: labd started", name)
		}
	}
}

// Nothing labd answers under its prefix points outside it: a path the router
// would clean is refused, and a redirect it or the file server makes keeps
// the prefix.
func TestRedirectsStayUnderThePrefix(t *testing.T) {
	h := edgeServer(t)
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	for _, p := range []string{"/maclab//api/devices", "/maclab/api/../api/devices", "/maclab/./api/devices", "/maclab/assets/../api/devices", "/maclab/api/%2e%2e/api/devices"} {
		if w := edgeDo(h, "GET", p, viaProxy, tok); w.Code != 404 {
			t.Errorf("GET %s: %d %s, want 404", p, w.Code, w.Header().Get("Location"))
		}
	}
	// The way to sign-in, the one redirect labd makes itself.
	signIn := labServer(t, "https://chonkbase.net/maclab", func(c *Config) { c.GitHub = &GitHubApp{ClientID: "the-client-id"} }).Handler()
	w := edgeDo(signIn, "GET", "/maclab/test-only/maintainers?tab=2", viaProxy, fromIngress)
	if want := "/maclab/sign-in?return=%2Fmaclab%2Ftest-only%2Fmaintainers%3Ftab%3D2"; w.Code != http.StatusSeeOther || w.Header().Get("Location") != want {
		t.Errorf("GET a Maintainers' page, signed out: %d to %q, want 303 to %q", w.Code, w.Header().Get("Location"), want)
	}
	for _, p := range []string{"/maclab/assets", "/maclab/assets/", "/maclab/terms/"} {
		if w := edgeDo(h, "GET", p, viaProxy, tok); w.Code != 404 {
			t.Errorf("GET %s: %d %s, want 404", p, w.Code, w.Header().Get("Location"))
		}
	}
}

const (
	labAddress = "https://lab.omarchy-mac.org"
	labHost    = "lab.omarchy-mac.org"
	aliasHost  = "maclab.omarchy.org"
)

// aliasServer is a labd at the Lab address with its Alias.
func aliasServer(t *testing.T, config func(*Config)) *Server {
	t.Helper()
	return labServer(t, labAddress, func(c *Config) {
		c.Alias = aliasHost
		if config != nil {
			config(c)
		}
	})
}

// On the Alias a page request is forwarded for good to the same path and
// query at the Lab address, and anything under /api/ is refused in one plain
// line naming the Lab address. Neither sets a cookie, reads a credential or
// reaches a route.
func TestTheAliasForwardsPagesAndRefusesTheAPI(t *testing.T) {
	s := aliasServer(t, func(c *Config) { c.Preview = true })
	h := s.Handler()
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	for _, c := range []struct{ method, target, want string }{
		{"GET", "/", labAddress + "/"},
		{"GET", "/install", labAddress + "/install"},
		{"GET", "/install/lab?v=0.2.0&x=a%20b", labAddress + "/install/lab?v=0.2.0&x=a%20b"},
		{"GET", "/reports/r%2F1?page=2", labAddress + "/reports/r%2F1?page=2"},
		{"HEAD", "/models", labAddress + "/models"},
		{"POST", "/sign-in", labAddress + "/sign-in"},
		{"GET", "//evil.example/x", labAddress + "//evil.example/x"},
		{"GET", "/healthz", labAddress + "/healthz"},
		{"GET", "/apiary", labAddress + "/apiary"},
	} {
		w := edgeAt(h, c.method, aliasHost, c.target, viaProxy, tok)
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != c.want {
			t.Errorf("%s %s on the Alias: %d to %q, want 308 to %q", c.method, c.target, w.Code, w.Header().Get("Location"), c.want)
		}
		if len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("%s %s on the Alias set a cookie", c.method, c.target)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s %s on the Alias: Cache-Control %q, and a forward is never cached", c.method, c.target, cc)
		}
	}
	for _, c := range []struct{ method, target string }{
		{"GET", "/api/devices"},
		{"POST", "/api/agent/checkin"},
		{"POST", "/api/agent/enroll"},
		{"GET", "/api/nothing/here?x=1"},
		{"GET", "/api/"},
	} {
		w := edgeAt(h, c.method, aliasHost, c.target, viaProxy, tok)
		body := w.Body.String()
		if w.Code != http.StatusMisdirectedRequest || w.Header().Get("Location") != "" {
			t.Errorf("%s %s on the Alias: %d %s", c.method, c.target, w.Code, body)
		}
		if strings.Count(body, "\n") != 1 || !strings.Contains(body, labAddress) || strings.Contains(body, aliasHost) ||
			!strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
			t.Errorf("%s %s on the Alias: want one plain line naming the Lab address, got %q (%s)", c.method, c.target, body, w.Header().Get("Content-Type"))
		}
		if len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("%s %s on the Alias set a cookie", c.method, c.target)
		}
	}
	// Plain http at the edge still leads to the Lab address.
	if w := edgeAt(h, "GET", aliasHost, "/install", viaProxy, map[string]string{"X-Forwarded-Proto": "http"}); w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != labAddress+"/install" {
		t.Errorf("plain http on the Alias: %d to %q", w.Code, w.Header().Get("Location"))
	}
	// The same requests at the Lab address reach labd.
	if w := edgeAt(h, "GET", labHost, "/api/devices", viaProxy, tok); w.Code != 200 {
		t.Errorf("the API at the Lab address: %d %s", w.Code, w.Body)
	}
	// The Alias is a name of the tunnel's only: at the loopback port it is a third name.
	if w := edgeAt(h, "GET", aliasHost, "/", direct, nil); w.Code != http.StatusMisdirectedRequest {
		t.Errorf("the Alias at the loopback port: %d", w.Code)
	}
}

// Every link labd builds names the Lab address, whichever listener the
// request came in on.
func TestEveryLinkNamesTheLabAddress(t *testing.T) {
	agentBin := filepath.Join(t.TempDir(), "lab-agent-linux-arm64")
	if err := os.WriteFile(agentBin, []byte("\x7fELF lab-agent for the test"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := aliasServer(t, func(c *Config) { c.AgentBinary = agentBin })
	h := s.Handler()
	for _, c := range []struct{ name, host, remote string }{
		{"through the tunnel", labHost, viaProxy},
		{"at the loopback port", loopback, direct},
	} {
		r := httptest.NewRequest("POST", "http://"+c.host+"/api/enroll-tokens", strings.NewReader(`{"name":"m1-pro"}`))
		r.Header.Set("Authorization", "Bearer lab_adm_secret")
		if r.RemoteAddr = c.remote; c.remote == viaProxy {
			r = r.WithContext(onTunnel(r.Context()))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var resp struct {
			JoinCommand string `json:"join_command"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != 200 {
			t.Fatalf("%s: an invite: %d %s", c.name, w.Code, w.Body)
		}
		if !strings.Contains(resp.JoinCommand, " "+labAddress+"/join ") || strings.Contains(resp.JoinCommand, "127.0.0.1") || strings.Contains(resp.JoinCommand, aliasHost) {
			t.Errorf("%s: the join line doesn't name the Lab address alone:\n%s", c.name, resp.JoinCommand)
		}
	}
	r := httptest.NewRequest("GET", "http://"+loopback+"/", nil)
	if got := s.publicBase(r); got != labAddress {
		t.Errorf("labd's own address, asked at the loopback port: %q", got)
	}
	if got := s.labName(r); got != labHost {
		t.Errorf("labd's own name, asked at the loopback port: %q", got)
	}
}

// 0-RTT data can be replayed: the edge marks a request that came in it, and
// labd answers 425 to any such request under /api/.
func TestEarlyDataUnderTheAPIGets425(t *testing.T) {
	h := aliasServer(t, nil).Handler()
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	early := with(tok, "Early-Data", "1")
	for _, c := range []struct {
		name, method, target string
		hdr                  map[string]string
		want                 int
	}{
		{"an early API read", "GET", "/api/devices", early, http.StatusTooEarly},
		{"an early API write", "POST", "/api/jobs", early, http.StatusTooEarly},
		{"an early check-in", "POST", "/api/agent/checkin", early, http.StatusTooEarly},
		{"the same read, not early", "GET", "/api/devices", tok, 200},
		{"an early page", "GET", "/healthz", early, 200},
		{"a path that only starts like the API", "GET", "/apiary", early, 404},
	} {
		if w := edgeAt(h, c.method, labHost, c.target, viaProxy, c.hdr); w.Code != c.want {
			t.Errorf("%s: %d, want %d: %s", c.name, w.Code, c.want, w.Body)
		}
	}
}

// A response whose handler states no cache policy says no-store; one that
// states its own keeps it.
func TestTheDefaultCachePolicyIsNoStore(t *testing.T) {
	h := aliasServer(t, nil).Handler()
	tok := with(fromIngress, "Authorization", "Bearer lab_adm_secret")
	for _, target := range []string{"/healthz", "/api/devices", "/api/devices/nope", "/api/nothing", "/nothing", "/api/maintainers"} {
		w := edgeAt(h, "GET", labHost, target, viaProxy, tok)
		if cc := w.Header().Values("Cache-Control"); len(cc) != 1 || cc[0] != "no-store" {
			t.Errorf("GET %s (%d): Cache-Control %q, want no-store", target, w.Code, cc)
		}
	}
	if w := edgeAt(h, "GET", labHost, "/api/devices", viaProxy, nil); w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a refusal: %d, Cache-Control %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := edgeAt(h, "GET", "evil.example", "/", viaProxy, nil); w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a third name's refusal: Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	w := edgeAt(h, "GET", labHost, pageFile(t, h, labHost, "/", ".js"), viaProxy, fromIngress)
	if cc := w.Header().Values("Cache-Control"); w.Code != 200 || len(cc) != 1 || cc[0] == "no-store" {
		t.Errorf("a file whose handler states its own policy: %d, Cache-Control %q", w.Code, cc)
	}
}

// With Preview on, pages tell crawlers not to index them; with it off they
// don't.
func TestPreviewPagesCarryNoindex(t *testing.T) {
	for _, preview := range []bool{true, false} {
		s := aliasServer(t, func(c *Config) { c.Preview = preview })
		if s.Preview() != preview {
			t.Errorf("Preview() is %v with the switch at %v", s.Preview(), preview)
		}
		h := s.Handler()
		for _, target := range []string{"/", "/terms", pageFile(t, h, labHost, "/", ".css"), "/healthz", "/nothing"} {
			w := edgeAt(h, "GET", labHost, target, viaProxy, fromIngress)
			if got := w.Header().Get("X-Robots-Tag") == "noindex"; got != preview {
				t.Errorf("Preview %v: GET %s (%d): X-Robots-Tag %q", preview, target, w.Code, w.Header().Get("X-Robots-Tag"))
			}
		}
	}
}
