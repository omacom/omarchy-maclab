package cutovertest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const edgeRay = "8f00ba4dfeed1234-BNE"

// darkEdge plays the edge in front of a real labd for the Dark recordings.
// It keeps a copy of a public page as the edge does, and while the mini
// holds a daemon (a file the stand-in for ssh makes) it answers in one of
// two ways, since which one Cloudflare takes is what the recording is for:
// serving the copy it has, or its own error page.
type darkEdge struct {
	t      *testing.T
	state  string
	srv    *httptest.Server
	mu     sync.Mutex
	copies map[string][]byte
	kept   map[string]time.Time
	stale  bool   // serve the copy while the origin is gone
	cookie string // a path whose answer carries a cookie of the edge's
}

func newDarkEdge(t *testing.T, stale bool) *darkEdge {
	t.Helper()
	e := &darkEdge{t: t, state: t.TempDir(), copies: map[string][]byte{}, kept: map[string]time.Time{}, stale: stale}
	labd := start(t, regexp.MustCompile(`labd listening on (http://127\.0\.0\.1:\d+)`), filepath.Join(bins(t), "labd"), "--listen=127.0.0.1:0", "--data="+filepath.Join(t.TempDir(), "data"), "--omt=false")
	origin, _ := url.Parse(labd)
	proxy := httputil.NewSingleHostReverseProxy(origin)
	proxy.ModifyResponse = func(resp *http.Response) error {
		e.mu.Lock()
		defer e.mu.Unlock()
		path := resp.Request.URL.Path
		if e.cookie == path {
			resp.Header.Add("Set-Cookie", "__cf_bm=a-value-nobody-prints; path=/; HttpOnly")
		}
		resp.Header.Set("CF-Ray", edgeRay)
		if resp.StatusCode == 200 && resp.Header.Get("Cloudflare-Cdn-Cache-Control") != "" {
			body, _ := io.ReadAll(resp.Body)
			resp.Body = io.NopCloser(strings.NewReader(string(body)))
			e.copies[path], e.kept[path] = body, time.Now()
			resp.Header.Set("CF-Cache-Status", "MISS")
			resp.Header.Del("Cloudflare-Cdn-Cache-Control")
		} else {
			resp.Header.Set("CF-Cache-Status", "DYNAMIC")
		}
		return nil
	}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if name != labName {
			http.NotFound(w, r)
			return
		}
		r.URL.Path, r.Host = "/"+rest, origin.Host
		down := e.held("cloudflared") || e.held("labd")
		e.mu.Lock()
		body, have := e.copies[r.URL.Path]
		age := int(time.Since(e.kept[r.URL.Path]).Seconds())
		e.mu.Unlock()
		w.Header().Set("CF-Ray", edgeRay)
		switch {
		case have && (!down || e.stale):
			status := "HIT"
			if down {
				status = "STALE"
			}
			w.Header().Set("CF-Cache-Status", status)
			w.Header().Set("Age", strconv.Itoa(age))
			w.Write(body)
		case down:
			code, number := 530, "1033"
			if e.held("labd") {
				code, number = 502, "502"
			}
			w.WriteHeader(code)
			io.WriteString(w, "<!DOCTYPE html><html><body><h1>a whole page of Cloudflare's</h1><span>error code: "+number+"</span></body></html>")
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *darkEdge) held(part string) bool {
	_, err := os.Stat(filepath.Join(e.state, "held."+part))
	return err == nil
}

// record runs the recordings with --lab against the stand-in, the mini
// played by a stand-in for ssh whose hold says what the real one does,
// after sudo's prompt on the same line.
func (e *darkEdge) record(ssh string, args ...string) (out string, code int, record string) {
	e.t.Helper()
	stubs, records := e.t.TempDir(), e.t.TempDir()
	if ssh == "" {
		ssh = `#!/bin/bash
echo "ssh $*" >> "$STATE/calls"
part="${*: -2:1}" seconds="${*: -1}"
printf 'Password:'
: > "$STATE/held.$part"
echo "stopped $(date +%s)"
sleep "$seconds"
rm -f "$STATE/held.$part"
echo "started $(date +%s)"
echo "back $(date +%s)"
`
	}
	if err := os.WriteFile(filepath.Join(stubs, "ssh"), []byte(ssh), 0o755); err != nil {
		e.t.Fatal(err)
	}
	env := []string{"PATH=" + stubs + ":" + os.Getenv("PATH"), "STATE=" + e.state, "LAB_CUTOVER_EDGE=" + e.srv.URL}
	out, code = run(e.t, env, "", cutover(e.t, "dark-recordings"), append([]string{"--lab", "--mini", "owner@mini.example", "--records", records, "--seconds", "3", "--every", "1", "--tail", "2"}, args...)...)
	if files, _ := filepath.Glob(filepath.Join(records, "*-dark-recordings.md")); len(files) == 1 {
		record = read(e.t, files[0])
	}
	return out, code, record
}

// The recordings write down what the edge did, whichever it was: here it
// goes on serving the copy it has while the tunnel, then labd, is stopped.
func TestTheDarkRecordingsWriteDownAnEdgeThatServesItsCopy(t *testing.T) {
	added := recordsAdded(t, "*dark-recordings*")
	e := newDarkEdge(t, true)
	out, code, record := e.record("")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"# The Dark recordings", "Recorded, not gates", "through " + labURL,
		"- cookies: a signed-out load of 7 public pages (/ /reports /matrix /terms /privacy /install /status) set no cookie, the edge's own included",
		"- the cached page: / was an edge HIT at request ",
		"- cloudflared stopped, labd running, for ", "- labd alone stopped, cloudflared running, for ",
		"answers: status 200, cf-cache-status STALE, age ",
		"/status, never cached: ", "answers: status 530 (Cloudflare error 1033, ray id " + edgeRay + ")",
		"answers: status 502 (Cloudflare error 502, ray id " + edgeRay + ")",
		"so: a signed-out visitor kept getting the public page for the whole ",
		"after: /status answered 200 again ",
		"Result: recorded",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if strings.Count(record, "so: a signed-out visitor kept getting the public page") != 2 || strings.Contains(record, "got no page") {
		t.Errorf("the record does not say it twice:\n%s", record)
	}
	for _, never := range []string{"<html", "a whole page of Cloudflare's", "Password"} {
		if strings.Contains(record, never) || (never != "Password" && strings.Contains(out, never)) {
			t.Errorf("an error page or the prompt is in the record or the output (%q):\n%s", never, out)
		}
	}
	calls := read(t, filepath.Join(e.state, "calls"))
	want := "ssh -t owner@mini.example sudo /usr/local/lab/current/scripts/mini/hold cloudflared 3\nssh -t owner@mini.example sudo /usr/local/lab/current/scripts/mini/hold labd 3\n"
	if calls != want {
		t.Errorf("the mini was asked:\n%s", calls)
	}
	if entries := added(); len(entries) != 0 {
		t.Errorf("the run left a record in docs/acceptance: %v", entries)
	}
}

// And here the edge gives its own error page as soon as the origin is
// gone, and sets a cookie of its own on one page: both are written down,
// the cookie by name only.
func TestTheDarkRecordingsWriteDownAnEdgeThatGivesItsErrorPage(t *testing.T) {
	e := newDarkEdge(t, false)
	e.cookie = "/reports"
	out, code, record := e.record("")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"- cookies: of 7 public pages loaded signed out: /reports (200) set __cf_bm",
		"the cached page /: ", "answers: status 530 (Cloudflare error 1033, ray id " + edgeRay + ")",
		"so: a signed-out visitor got no page for the whole ",
		"Result: recorded",
	} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	if strings.Contains(record, "kept getting") || strings.Contains(out+record, "a-value-nobody-prints") || strings.Contains(out+record, "<html") {
		t.Errorf("the record:\n%s", record)
	}
}

// When the mini does not hold the daemon nothing is made up: the run fails
// and says so.
func TestTheDarkRecordingsFailWhenTheMiniDoesNotHold(t *testing.T) {
	e := newDarkEdge(t, true)
	out, code, record := e.record("#!/bin/bash\necho \"ssh $*\" >> \"$STATE/calls\"\necho 'hold: cloudflared is not running: nothing was stopped' >&2\nexit 1\n")
	if code != 1 || !strings.Contains(record, "the mini did not hold cloudflared") || !strings.Contains(record, "Result: fail") || strings.Contains(record, "so: a signed-out visitor") {
		t.Errorf("exit %d\n%s\n%s", code, out, record)
	}
	for what, args := range map[string][]string{
		"five minutes":    {"--seconds", "300"},
		"an API path":     {"--page", "/api/devices"},
		"shell in a page": {"--page", "/;id"},
	} {
		if out, code, _ := e.record("", args...); code == 0 || strings.Contains(out, "stopped") {
			t.Errorf("%s was taken:\n%s", what, out)
		}
	}
	if out, code := run(t, nil, "", cutover(t, "dark-recordings"), "--lab"); code == 0 || !strings.Contains(out, "--mini") {
		t.Errorf("with no mini: exit %d\n%s", code, out)
	}
}

// Against a labd on this machine the recordings run whole, and show labd's
// own side: a signed-out page load sets no cookie.
func TestTheDarkRecordingsRunAgainstALocalLabd(t *testing.T) {
	added := recordsAdded(t, "*dark-recordings*")
	out, code := run(t, nil, "", cutover(t, "dark-recordings"), "--local", "--bin", bins(t))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{
		"cookies: a signed-out load of 7 public pages (/ /reports /matrix /terms /privacy /install /status) set no cookie. These are labd's own answers",
		"There is no edge here, so nothing is cached: labd says Cache-Control \"public, max-age=0\" and Cloudflare-CDN-Cache-Control \"max-age=60, stale-while-revalidate=86400, stale-if-error=86400\"",
		"labd stopped (there is no tunnel and no edge here), for ", "answers: status 000 (nothing answered)",
		"so: a signed-out visitor got no page for the whole ", "after: /status answered 200 again ", ": recorded",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the local recording does not say %q:\n%s", want, out)
		}
	}
	if entries := added(); len(entries) != 0 {
		t.Errorf("a local run left a record in docs/acceptance: %v", entries)
	}
}
