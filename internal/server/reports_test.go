package server

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// The upload and the report's two old links, through labd's own front: the
// tunnel's socket, the Lab address, the caller log. What an upload is
// refused for, rule by rule, is internal/reports' to test.

// testMachineIDKey is 64 bytes that are no secret.
func testMachineIDKey() []byte {
	var key []byte
	for i := range 2 {
		sum := sha256.Sum256([]byte{byte(i)})
		key = append(key, sum[:]...)
	}
	return key
}

// signedGolden is one of the two reports the test tool signed.
func signedGolden(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "signed", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// signedKey is the public key that report is signed with, without its type.
func signedKey(t *testing.T, name string) string {
	t.Helper()
	var report struct {
		Signature struct {
			PublicKey string `json:"public_key"`
		} `json:"signature"`
	}
	if err := json.Unmarshal([]byte(signedGolden(t, name)), &report); err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(report.Signature.PublicKey)
	if len(fields) != 2 || len(fields[1]) < 40 {
		t.Fatalf("public key %q", report.Signature.PublicKey)
	}
	return fields[1]
}

// reportsLab is a labd at the Lab address that takes reports.
type reportsLab struct {
	t      *testing.T
	s      *Server
	h      http.Handler
	log    *bytes.Buffer
	remote string
}

func newReportsLab(t *testing.T, config func(*Config)) *reportsLab {
	t.Helper()
	u, err := ParsePublicURL(labAddress)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: t.TempDir(), PublicURL: u, Alias: aliasHost, MachineIDKey: testMachineIDKey()}
	if config != nil {
		config(&cfg)
	}
	l := &reportsLab{t: t, log: &bytes.Buffer{}, remote: viaProxy}
	l.s, err = newWith("ada", "lab_adm_secret", t.Context(), cfg, log.New(l.log, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	l.h = l.s.Handler()
	return l
}

// do asks labd as cloudflared does, for a caller at 203.0.113.9, unless hdr
// or l.remote say otherwise.
func (l *reportsLab) do(method, target, body string, hdr ...string) *httptest.ResponseRecorder {
	l.t.Helper()
	host := labHost
	if l.remote != viaProxy {
		host = loopback
	}
	r := httptest.NewRequest(method, "http://"+host+target, strings.NewReader(body))
	r.RemoteAddr = l.remote
	if l.remote == viaProxy {
		r = r.WithContext(onTunnel(r.Context()))
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("CF-Connecting-IP", "203.0.113.9")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i] == "Host" {
			r.Host = hdr[i+1]
			continue
		}
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	l.h.ServeHTTP(w, r)
	return w
}

// links are what a stored upload was answered with.
type links struct {
	ID          string `json:"id"`
	ReportURL   string `json:"report_url"`
	DeletionURL string `json:"deletion_url"`
	Error       string `json:"error"`
}

func (l links) token(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(l.DeletionURL)
	if err != nil || u.Query().Get("token") == "" {
		t.Fatalf("deletion_url %q: %v", l.DeletionURL, err)
	}
	return u.Query().Get("token")
}

// path is a link without the Lab address: only the host changed.
func pathOf(t *testing.T, link string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(link, labAddress)
	if !ok || !strings.HasPrefix(rest, "/") {
		t.Fatalf("the link %q is not at the Lab address", link)
	}
	return rest
}

func (l *reportsLab) upload(name string) links {
	l.t.Helper()
	w := l.do("POST", "/api/v1/reports", signedGolden(l.t, name), "Content-Type", "application/json")
	if w.Code != http.StatusCreated {
		l.t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	var out links
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		l.t.Fatal(err)
	}
	return out
}

func (l *reportsLab) stored() int {
	l.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(l.s.cfg.DataDir, "reports.db"))
	if err != nil {
		l.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM reports`).Scan(&n); err != nil {
		l.t.Fatal(err)
	}
	return n
}

func (l *reportsLab) hide(publicID string) {
	l.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(l.s.cfg.DataDir, "reports.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		l.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE reports SET hidden_at = ? WHERE public_id = ?`, time.Now().UnixMicro(), publicID); err != nil {
		l.t.Fatal(err)
	}
}

func TestAnUploadIsAnsweredWithLinksAtTheLabAddress(t *testing.T) {
	l := newReportsLab(t, nil)
	got := l.upload("m2-max-image2")
	if got.ReportURL != labAddress+"/reports/"+got.ID || !strings.HasPrefix(got.DeletionURL, got.ReportURL+"/deletion?token=") {
		t.Errorf("links %+v", got)
	}
	if l.stored() != 1 {
		t.Errorf("%d reports stored", l.stored())
	}
	// The refusals that ask for an update name the lab's own install line.
	old := strings.Replace(signedGolden(t, "m2-max-image2"), `"schema_version": 1`, `"schema_version": 0`, 1)
	w := l.do("POST", "/api/v1/reports", old)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "(curl -fsSL "+labAddress+"/install | bash)") {
		t.Errorf("an outdated report: %d %s", w.Code, w.Body)
	}
	// The same report again, as the test tool sends it with --upload.
	w = l.do("POST", "/api/v1/reports", signedGolden(t, "m2-max-image2"))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error":"Already uploaded"`) || !strings.Contains(w.Body.String(), `"report_url":"`+got.ReportURL+`"`) {
		t.Errorf("a repeat: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "deletion") || strings.Contains(w.Body.String(), got.token(t)) {
		t.Errorf("a repeat was given the deletion link: %s", w.Body)
	}
	// On the Alias the upload is refused, naming the Lab address.
	w = l.do("POST", "/api/v1/reports", signedGolden(t, "m2-max-image2-awkward-kernel"), "Host", aliasHost)
	if w.Code != http.StatusMisdirectedRequest || !strings.Contains(w.Body.String(), labAddress) {
		t.Errorf("on the Alias: %d %s", w.Code, w.Body)
	}
	if l.stored() != 1 {
		t.Errorf("%d reports stored, want 1", l.stored())
	}
}

// 30 an hour for one address, and the address is what the edge names in
// CF-Connecting-IP on the tunnel's socket: nothing else a caller sends moves
// an upload to another count.
func TestUploadsAreCountedOnCFConnectingIP(t *testing.T) {
	l := newReportsLab(t, nil)
	junk := func(cf string, n int) int {
		return l.do("POST", "/api/v1/reports", "not json", "CF-Connecting-IP", cf,
			"X-Forwarded-For", fmt.Sprintf("198.51.100.%d", n), "X-Real-IP", fmt.Sprintf("198.51.100.%d", n)).Code
	}
	for n := range reports.UploadsPerHour {
		if code := junk("203.0.113.9", n); code != http.StatusBadRequest {
			t.Fatalf("upload %d: %d", n+1, code)
		}
	}
	w := l.do("POST", "/api/v1/reports", signedGolden(t, "m2-max-image2"), "CF-Connecting-IP", "203.0.113.9", "X-Forwarded-For", "198.51.100.200")
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "Too many uploads from your network: at most 30 an hour") {
		t.Errorf("upload 31 from one address: %d %s", w.Code, w.Body)
	}
	if code := junk("203.0.113.10", 0); code != http.StatusBadRequest {
		t.Errorf("another address: %d", code)
	}

	// At the loopback port the header is anyone's to forge, and is not
	// believed: every caller there is this machine.
	l.remote = direct
	for n := range reports.UploadsPerHour {
		if code := junk(fmt.Sprintf("192.0.2.%d", n), n); code != http.StatusBadRequest {
			t.Fatalf("loopback upload %d: %d", n+1, code)
		}
	}
	if code := junk("192.0.2.99", 0); code != http.StatusTooManyRequests {
		t.Errorf("upload 31 at the loopback port, each naming another address: %d", code)
	}
}

func TestAReportsOldLinkShowsTheMachineAndEachCheck(t *testing.T) {
	l := newReportsLab(t, nil)
	got := l.upload("m2-max-image2")
	w := l.do("GET", pathOf(t, got.ReportURL), "")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	page := w.Body.String()
	offersToCopy(t, page, "the report id", got.ID)
	for _, want := range []string{
		`<h1>Apple MacBook Pro (16-inch, M2 Max, 2023)</h1>`,
		`<dd>M2 Max (t6021, board j416c)</dd>`,
		`<dd>7.1.12-2-2-ARCH</dd>`,
		`<li class="check check-pass" id="check-system.identity">`,
		`<span class="status status-pass">PASS</span>`,
		`kernel: 7.1.12-2-2-ARCH`,
		`<span class="badge badge-works outcome outcome-works">works</span>`,
		`<span class="feature">Device tree, asahi layer</span>`,
		`<dd>feature catalogue v18</dd>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the report's page lacks %q", want)
		}
	}
	if strings.Contains(page, "CC BY") {
		t.Error("the report's page credits Asahi")
	}
	if strings.Contains(page, `id="unattended"`) {
		t.Error("an attended report says unattended")
	}
	if strings.Contains(page, got.token(t)) || strings.Contains(page, "deletion") {
		t.Error("the report's page carries its deletion link")
	}
	if key := signedKey(t, "m2-max-image2"); strings.Contains(page, key) || strings.Contains(page, "SSH SIGNATURE") {
		t.Error("the report's page shows its Mac's key or signature")
	}
	if c := w.Header().Get("Set-Cookie"); c != "" {
		t.Errorf("the page sets a cookie: %s", c)
	}
	for _, missing := range []string{"/reports/1111111111111111aaaaaaaa", "/reports/" + got.ID + "x", "/reports/" + got.ID + "/nothing"} {
		if w := l.do("GET", missing, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", missing, w.Code)
		}
	}
	// A Hidden report is not found, and its page says nothing of it.
	l.hide(got.ID)
	if w := l.do("GET", pathOf(t, got.ReportURL), ""); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "MacBook") {
		t.Errorf("a hidden report: %d %s", w.Code, w.Body)
	}
}

func reportOf(t *testing.T, change func(map[string]any)) *reports.Report {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "m2-max-image2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	change(body)
	text, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &reports.Report{PublicID: "8c1f02ab", Body: text, CreatedAt: time.Date(2026, 9, 26, 3, 4, 5, 0, time.UTC)}
}

func TestTheReportPageExplainsEachResultInPlainWords(t *testing.T) {
	l := newReportsLab(t, nil)
	page := func(change func(map[string]any)) string {
		t.Helper()
		out := httptest.NewRecorder()
		published := reports.NewPublished(l.s.catalogue, []*reports.Report{reportOf(t, change)})
		if err := l.s.site.Render(out, "reports/show", web.Request{}, web.Page{Data: l.s.reportPage(nil, published, published.Runs()[0])}); err != nil {
			t.Fatal(err)
		}
		return out.Body.String()
	}
	firstCheck := func(body map[string]any) map[string]any {
		return body["checks"].([]any)[0].(map[string]any)
	}

	unattended := page(func(body map[string]any) { body["unattended"] = true })
	if !regexp.MustCompile(`<dd id="unattended">[^<]*nobody was at the Mac[^<]*</dd>`).MatchString(unattended) {
		t.Error("an unattended report does not say nobody was at the Mac")
	}
	if !strings.Contains(unattended, "<dd>2026-09-26 03:04 UTC</dd>") {
		t.Error("the page does not say when the report was uploaded")
	}

	failed := page(func(body map[string]any) {
		check := firstCheck(body)
		check["status"] = "fail"
		check["classification"].(map[string]any)["outcome"] = "not-in-aurora"
	})
	for _, want := range []string{
		`<span class="badge badge-missing outcome outcome-not-in-aurora">not yet supported by Aurora</span>`,
		`<li class="check check-gap" id="check-system.identity">`,
		`<span class="status status-gap">GAP</span>`,
	} {
		if !strings.Contains(failed, want) {
			t.Errorf("a failure the catalogue expects lacks %q", want)
		}
	}
	if !strings.Contains(page(func(body map[string]any) {
		check := firstCheck(body)
		check["status"] = "fail"
		check["classification"].(map[string]any)["outcome"] = "not-applicable"
	}), `<span class="status status-gap">N/A</span>`) {
		t.Error("hardware this Mac does not have is not N/A")
	}
	if !strings.Contains(page(func(body map[string]any) { firstCheck(body)["answered_by_default"] = true }),
		`<span class="badge badge-unconfirmed outcome outcome-works answered-by-default"`) {
		t.Error("an answer given by a bare Enter is not shown as unconfirmed")
	}

	// Whatever a report says is text on its page, never markup.
	hostile := page(func(body map[string]any) {
		body["machine"].(map[string]any)["model"] = `<script>alert(1)</script>`
		firstCheck(body)["evidence"] = []any{`</pre><img src=x onerror=alert(1)>`}
	})
	if strings.Contains(hostile, "<script>") || strings.Contains(hostile, "<img src=x") || strings.Contains(hostile, "onerror=alert(1)>") {
		t.Error("a report's words reached its page as markup")
	}
}

// The deletion link opens a confirmation and deletes only when that is sent;
// the page is no-store and sends no referrer.
func TestTheDeletionLinkAsksBeforeDeleting(t *testing.T) {
	l := newReportsLab(t, nil)
	got := l.upload("m2-max-image2")
	other := l.upload("m2-max-image2-awkward-kernel")
	token := got.token(t)
	link, report := pathOf(t, got.DeletionURL), pathOf(t, got.ReportURL)

	for _, method := range []string{"GET", "HEAD", "GET"} {
		w := l.do(method, link, "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s the deletion link: %d %s", method, w.Code, w.Body)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control %q, want no-store", cc)
		}
		if rp := w.Header().Get("Referrer-Policy"); rp != "no-referrer" {
			t.Errorf("Referrer-Policy %q, want no-referrer", rp)
		}
		if c := w.Header().Get("Set-Cookie"); c != "" {
			t.Errorf("the confirmation sets a cookie: %s", c)
		}
	}
	page := l.do("GET", link, "").Body.String()
	for _, want := range []string{
		"<h1>Delete this report?</h1>",
		"This permanently removes the Apple MacBook Pro (16-inch, M2 Max, 2023) report uploaded ",
		"It can't be undone.",
		`<form method="post" action="/reports/` + got.ID + `">`,
		`<input type="hidden" name="_method" value="delete">`,
		`<input type="hidden" name="token" value="` + token + `">`,
		`<button type="submit" class="button button-danger">Delete report</button>`,
		`<meta name="referrer" content="no-referrer">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the confirmation lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "csrf_token") || strings.Count(page, token) != 1 || strings.Contains(page, "token=") {
		t.Errorf("the confirmation of a signed-out visitor carries a token of its own, or the link's secret outside its form:\n%s", page)
	}
	if l.stored() != 2 {
		t.Fatalf("opening the link deleted something: %d reports", l.stored())
	}

	form := func(values ...string) string {
		v := url.Values{}
		for i := 0; i+1 < len(values); i += 2 {
			v.Set(values[i], values[i+1])
		}
		return v.Encode()
	}
	urlencoded := []string{"Content-Type", "application/x-www-form-urlencoded", "Origin", labAddress}
	// What a browser sends with the confirmation's form: the page sends no
	// referrer, so the Origin of its POST is null, and Sec-Fetch-Site says
	// where the form was.
	noReferrer := func(site string) []string {
		hdr := []string{"Content-Type", "application/x-www-form-urlencoded", "Origin", "null"}
		if site != "" {
			hdr = append(hdr, "Sec-Fetch-Site", site)
		}
		return hdr
	}
	// What does not delete: a wrong secret, none, another report's, the
	// form without its method, the secret in the address, another site's page.
	for name, c := range map[string]struct {
		method, target, body string
		hdr                  []string
		status               int
	}{
		"a wrong secret on the link":                 {"GET", report + "/deletion?token=wrong", "", nil, 404},
		"no secret on the link":                      {"GET", report + "/deletion", "", nil, 404},
		"another report's secret":                    {"GET", report + "/deletion?token=" + other.token(t), "", nil, 404},
		"sending a wrong secret":                     {"POST", report, form("_method", "delete", "token", "wrong"), urlencoded, 404},
		"sending no secret":                          {"POST", report, form("_method", "delete"), urlencoded, 404},
		"sending another's secret":                   {"POST", report, form("_method", "delete", "token", other.token(t)), urlencoded, 404},
		"the form without its method":                {"POST", report, form("token", token), urlencoded, 404},
		"the secret in the address":                  {"POST", report + "?_method=delete&token=" + token, "", urlencoded, 404},
		"the form with its secret in the address":    {"POST", report + "?token=" + token, form("_method", "delete"), urlencoded, 404},
		"a DELETE with the secret":                   {"DELETE", report + "?token=" + token, "", nil, 405},
		"a POST to the link itself":                  {"POST", link, form("_method", "delete", "token", token), urlencoded, 405},
		"another site's page":                        {"POST", report, form("_method", "delete", "token", token), []string{"Content-Type", "application/x-www-form-urlencoded", "Origin", "https://evil.example"}, 403},
		"another site's page, no Origin":             {"POST", report, form("_method", "delete", "token", token), []string{"Content-Type", "application/x-www-form-urlencoded", "Sec-Fetch-Site", "cross-site"}, 403},
		"another site's page that sends no referrer": {"POST", report, form("_method", "delete", "token", token), noReferrer("cross-site"), 403},
		"a page of a neighbour of the lab's name":    {"POST", report, form("_method", "delete", "token", token), noReferrer("same-site"), 403},
		"a null Origin and nothing else":             {"POST", report, form("_method", "delete", "token", token), noReferrer(""), 403},
		"a null Origin from no page":                 {"POST", report, form("_method", "delete", "token", token), noReferrer("none"), 403},
	} {
		if w := l.do(c.method, c.target, c.body, c.hdr...); w.Code != c.status {
			t.Errorf("%s: %d, want %d", name, w.Code, c.status)
		}
		if l.stored() != 2 {
			t.Fatalf("%s deleted something: %d reports", name, l.stored())
		}
	}

	w := l.do("POST", report, form("_method", "delete", "token", token), noReferrer("same-origin")...)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h1>Report deleted</h1>") || !strings.Contains(w.Body.String(), "The report and its evidence have been removed.") {
		t.Fatalf("sending the confirmation: %d %s", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control %q after the deletion", cc)
	}
	if l.stored() != 1 {
		t.Fatalf("%d reports after the deletion, want the other one", l.stored())
	}
	for name, target := range map[string]string{"the report": report, "its deletion link": link} {
		if w := l.do("GET", target, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s after the deletion: %d", name, w.Code)
		}
	}
	if w := l.do("POST", report, form("_method", "delete", "token", token), urlencoded...); w.Code != http.StatusNotFound {
		t.Errorf("sending the confirmation twice: %d", w.Code)
	}
	if w := l.do("GET", pathOf(t, other.ReportURL), ""); w.Code != http.StatusOK {
		t.Errorf("the other report: %d", w.Code)
	}
}

// A report carried from the old site, with the id and the digest it had
// there: both its old links work with only the host changed.
func TestTheOldSitesLinksWorkWithOnlyTheHostChanged(t *testing.T) {
	l := newReportsLab(t, func(c *Config) { c.MachineIDKey = nil })
	const (
		oldReport   = "https://omarchy-m-testing.org/reports/h7Qm2ZpX4cVb9NkT3sWd8RfY"
		oldToken    = "Zm9yIHRoZSBvbGQgc2l0ZSdzIGRlbGV0aW9uIGxpbms_-x"
		oldDeletion = oldReport + "/deletion?token=" + oldToken
	)
	body, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "m2-max-image2.json"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(l.s.cfg.DataDir, "reports.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 9, 26, 3, 4, 5, 0, time.UTC).UnixMicro()
	if _, err := db.Exec(`INSERT INTO reports (id, public_id, deletion_token_digest, schema_version, body, created_at, updated_at) VALUES (132, 'h7Qm2ZpX4cVb9NkT3sWd8RfY', ?, 1, ?, ?, ?)`,
		fmt.Sprintf("%x", sha256.Sum256([]byte(oldToken))), string(body), at, at); err != nil {
		t.Fatal(err)
	}
	here := func(old string) string { return strings.TrimPrefix(old, "https://omarchy-m-testing.org") }
	if w := l.do("GET", here(oldReport), ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<dd>2026-09-26 03:04 UTC</dd>") {
		t.Errorf("the old report link: %d", w.Code)
	}
	if w := l.do("GET", here(oldDeletion), ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Delete this report?") {
		t.Errorf("the old deletion link: %d", w.Code)
	}
	// On the old name itself the page is forwarded here, path and query kept.
	if w := l.do("GET", here(oldDeletion), "", "Host", aliasHost); w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != labAddress+here(oldDeletion) {
		t.Errorf("the link on the Alias: %d %s", w.Code, w.Header().Get("Location"))
	}
	send := url.Values{"_method": {"delete"}, "token": {oldToken}}.Encode()
	if w := l.do("POST", here(oldReport), send, "Content-Type", "application/x-www-form-urlencoded"); w.Code != http.StatusOK || l.stored() != 0 {
		t.Errorf("deleting by the old link: %d, %d reports left", w.Code, l.stored())
	}
}

// A Hidden report's deletion link still deletes it.
func TestTheDeletionLinkDeletesAHiddenReport(t *testing.T) {
	l := newReportsLab(t, nil)
	got := l.upload("m2-max-image2")
	l.hide(got.ID)
	if w := l.do("GET", pathOf(t, got.DeletionURL), ""); w.Code != http.StatusOK {
		t.Fatalf("the deletion link of a hidden report: %d", w.Code)
	}
	body := url.Values{"_method": {"delete"}, "token": {got.token(t)}}.Encode()
	if w := l.do("POST", pathOf(t, got.ReportURL), body, "Content-Type", "application/x-www-form-urlencoded"); w.Code != http.StatusOK {
		t.Fatalf("deleting a hidden report: %d %s", w.Code, w.Body)
	}
	if l.stored() != 0 {
		t.Errorf("%d reports", l.stored())
	}
}

// The secret of a deletion link, as issued and as used, is in neither the
// caller log nor labd's own log.
func TestTheDeletionSecretLabdIssuedIsInNoLog(t *testing.T) {
	l := newReportsLab(t, nil)
	got := l.upload("m2-max-image2")
	token := got.token(t)
	l.do("GET", pathOf(t, got.DeletionURL), "", "Referer", got.DeletionURL)
	l.do("GET", pathOf(t, got.ReportURL)+"/deletion?token=wrong", "")
	l.do("POST", pathOf(t, got.ReportURL), url.Values{"_method": {"delete"}, "token": {"wrong"}}.Encode(), "Content-Type", "application/x-www-form-urlencoded")
	l.do("POST", pathOf(t, got.ReportURL), url.Values{"_method": {"delete"}, "token": {token}}.Encode(), "Content-Type", "application/x-www-form-urlencoded", "Referer", got.DeletionURL)
	if l.stored() != 0 {
		t.Fatal("the report was not deleted")
	}
	raw := l.s.callers.rawForTest(t)
	if n := strings.Count(raw, "\n"); n != 5 {
		t.Fatalf("%d lines in the caller log, want 5:\n%s", n, raw)
	}
	for name, text := range map[string]string{"the caller log": raw, "labd's log": l.log.String()} {
		if strings.Contains(text, token) || strings.Contains(text, "token=") {
			t.Errorf("%s holds the deletion link's secret:\n%s", name, text)
		}
		if strings.Contains(text, signedKey(t, "m2-max-image2")) || strings.Contains(text, "SSH SIGNATURE") {
			t.Errorf("%s holds the report's key or signature:\n%s", name, text)
		}
	}
	for _, want := range []string{`"path":"/reports/` + got.ID + `/deletion"`, `"method":"POST","path":"/reports/` + got.ID + `","status":200`, `"addr":"203.0.113.9"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("the caller log lacks %s:\n%s", want, raw)
		}
	}
}

// A signed-in person's confirmation carries their session's token, as every
// write of a signed-in browser must.
func TestASignedInPersonDeletesByTheLinkToo(t *testing.T) {
	sl := newSignInLab(t, func(c *Config) { c.MachineIDKey = testMachineIDKey() })
	b := sl.browser()
	b.mustSignIn(ownerID)
	w := sl.browser().do("POST", "/api/v1/reports", signedGolden(t, "m2-max-image2"))
	var got links
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	page := b.do("GET", pathOf(t, got.DeletionURL), "")
	field := `<input type="hidden" name="csrf_token" value="` + b.csrf() + `">`
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), field) {
		t.Fatalf("the confirmation of a signed-in person: %d, without their token", page.Code)
	}
	if cc := page.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control %q", cc)
	}
	send := func(values url.Values, site string) int {
		return b.do("POST", pathOf(t, got.ReportURL), values.Encode(), "Content-Type", "application/x-www-form-urlencoded", "Origin", "null", "Sec-Fetch-Site", site).Code
	}
	whole := url.Values{"_method": {"delete"}, "token": {got.token(t)}, "csrf_token": {b.csrf()}}
	if code := send(url.Values{"_method": {"delete"}, "token": {got.token(t)}}, "same-origin"); code != http.StatusForbidden {
		t.Errorf("without the session's token: %d", code)
	}
	for _, site := range []string{"cross-site", "same-site", "none", ""} {
		if code := send(whole, site); code != http.StatusForbidden {
			t.Errorf("with the session's token, a null Origin and Sec-Fetch-Site %q: %d", site, code)
		}
	}
	if code := send(whole, "same-origin"); code != http.StatusOK {
		t.Errorf("with the session's token, as a browser sends the page's form: %d", code)
	}
	if w := b.do("GET", pathOf(t, got.ReportURL), ""); w.Code != http.StatusNotFound {
		t.Errorf("the report after the deletion: %d", w.Code)
	}
}

// In Preview the lab takes no reports: the old site is the live one. What it
// holds is still served, and a deletion link still deletes.
func TestInPreviewAnUploadIsRefusedPointingAtTheOldSite(t *testing.T) {
	l := newReportsLab(t, func(c *Config) { c.Preview = true })
	for n := range reports.UploadsPerHour + 5 {
		w := l.do("POST", "/api/v1/reports", signedGolden(t, "m2-max-image2"), "Content-Type", "application/json")
		var answer struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &answer); err != nil || w.Code != http.StatusForbidden {
			t.Fatalf("upload %d in Preview: %d %s", n+1, w.Code, w.Body)
		}
		if want := "Omarchy Mac Lab is not open yet and takes no reports. Until it opens, omarchy-m-testing.org is the live site: upload there."; answer.Error != want {
			t.Fatalf("upload %d in Preview: %q, want %q", n+1, answer.Error, want)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
			t.Fatalf("Content-Type %q", ct)
		}
	}
	if l.stored() != 0 {
		t.Errorf("%d reports stored in Preview", l.stored())
	}
}

func TestPreviewStillServesAndDeletesWhatItHolds(t *testing.T) {
	dir := t.TempDir()
	first := newReportsLab(t, func(c *Config) { c.DataDir = dir })
	got := first.upload("m2-max-image2")
	// The same data, as the lab in Preview holds the rehearsal copy.
	l := newReportsLab(t, func(c *Config) { c.DataDir = dir; c.Preview = true })
	if w := l.do("GET", pathOf(t, got.ReportURL), ""); w.Code != http.StatusOK || w.Header().Get("X-Robots-Tag") != "noindex" {
		t.Errorf("a report's page in Preview: %d, X-Robots-Tag %q", w.Code, w.Header().Get("X-Robots-Tag"))
	}
	if w := l.do("GET", pathOf(t, got.DeletionURL), ""); w.Code != http.StatusOK {
		t.Errorf("a deletion link in Preview: %d", w.Code)
	}
	body := url.Values{"_method": {"delete"}, "token": {got.token(t)}}.Encode()
	if w := l.do("POST", pathOf(t, got.ReportURL), body, "Content-Type", "application/x-www-form-urlencoded"); w.Code != http.StatusOK || l.stored() != 0 {
		t.Errorf("deleting in Preview: %d, %d reports left", w.Code, l.stored())
	}
}

// Without the machine-id key a report's Mac cannot be told from another, so
// nothing is taken; the pages need no key.
func TestALabWithoutItsMachineIDKeyTakesNoReports(t *testing.T) {
	dir := t.TempDir()
	got := newReportsLab(t, func(c *Config) { c.DataDir = dir }).upload("m2-max-image2")
	l := newReportsLab(t, func(c *Config) { c.DataDir = dir; c.MachineIDKey = nil })
	w := l.do("POST", "/api/v1/reports", signedGolden(t, "m2-max-image2-awkward-kernel"), "Content-Type", "application/json")
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"error":"This lab takes no reports yet`) {
		t.Errorf("an upload: %d %s", w.Code, w.Body)
	}
	if l.stored() != 1 {
		t.Errorf("%d reports stored", l.stored())
	}
	if w := l.do("GET", pathOf(t, got.ReportURL), ""); w.Code != http.StatusOK {
		t.Errorf("a report's page: %d", w.Code)
	}
	if _, err := newWith("ada", "lab_adm_secret", t.Context(), Config{DataDir: t.TempDir(), MachineIDKey: []byte("short")}, log.New(io.Discard, "", 0)); err == nil {
		t.Error("labd started with a machine-id key of the wrong size")
	}
}

// Reports come only from the test tool: no page of the lab is an upload form.
func TestThereIsNoBrowserUploadForm(t *testing.T) {
	l := newReportsLab(t, nil)
	form := url.Values{"report": {signedGolden(t, "m2-max-image2")}}.Encode()
	if w := l.do("POST", "/api/v1/reports", form, "Content-Type", "application/x-www-form-urlencoded"); w.Code != http.StatusBadRequest {
		t.Errorf("a form post: %d", w.Code)
	}
	for _, target := range []string{"/reports/new", "/upload", "/reports/upload"} {
		if w := l.do("GET", target, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", target, w.Code)
		}
	}
	if l.stored() != 0 {
		t.Errorf("%d reports stored", l.stored())
	}
}
