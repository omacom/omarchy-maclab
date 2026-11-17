package reports

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// Translated from site/test/integration/report_upload_test.rb,
// upload_rules_test.rb and unattended_run_test.rb: what the upload answers
// and what is stored. The pages are the server's.

const site = "http://www.example.com"

// desk is an upload pipeline over an empty reports.db, with a clock the test
// moves.
type desk struct {
	store   *Store
	path    string
	uploads *Uploads
	now     time.Time
}

func newDesk(t *testing.T) *desk {
	t.Helper()
	store, path := openStore(t)
	d := &desk{store: store, path: path, now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	d.uploads = NewUploads(store, testRules(t), func() time.Time { return d.now })
	return d
}

// answer is what an upload was answered with.
type answer struct {
	status int
	header http.Header
	body   map[string]any
}

func (a answer) text(key string) string {
	s, _ := a.body[key].(string)
	return s
}

func (a answer) details() []string {
	list, _ := a.body["details"].([]any)
	var out []string
	for _, d := range list {
		out = append(out, d.(string))
	}
	return out
}

// sent is a report as a request body: a document is signed by machine first,
// text goes as it is, anything else as JSON.
func sent(t testing.TB, report any, machine string) []byte {
	t.Helper()
	switch report := report.(type) {
	case *Object:
		return encode(t, sign(t, report, machine))
	case string:
		return []byte(report)
	case []byte:
		return report
	}
	return encode(t, report)
}

// post uploads as the old tests' upload_report does: from machine "a" at
// 10.0.0.1 unless told otherwise.
func (d *desk) post(t testing.TB, report any, from ...string) answer {
	t.Helper()
	machine, ip := "a", "10.0.0.1"
	if len(from) > 0 {
		machine = from[0]
	}
	if len(from) > 1 {
		ip = from[1]
	}
	r := httptest.NewRequest("POST", site+"/api/v1/reports", bytes.NewReader(sent(t, report, machine)))
	r.Header.Set("Content-Type", "application/json")
	return d.serve(t, r, ip)
}

func (d *desk) serve(t testing.TB, r *http.Request, ip string) answer {
	t.Helper()
	w := httptest.NewRecorder()
	if err := d.uploads.Handle(w, r, ip, site); err != nil {
		t.Fatal(err)
	}
	a := answer{status: w.Code, header: w.Header()}
	if err := json.Unmarshal(w.Body.Bytes(), &a.body); err != nil {
		t.Fatalf("the answer is not a JSON object: %d %q", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type %q", got)
	}
	return a
}

func (d *desk) count(t *testing.T) int {
	t.Helper()
	return len(d.store.list(t, `SELECT public_id FROM reports`))
}

// run is the golden report of another run: the same Mac, other evidence.
func run(t testing.TB, n int) *Object {
	t.Helper()
	report := golden(t, "m2-max-image2")
	evidence := at(report, "checks", 0, "evidence").([]any)
	return put(report, append(evidence, fmt.Sprintf("run: %d", n)), "checks", 0, "evidence")
}

func TestPostingTheGoldenReportStoresItAndReturnsReportAndDeletionLinks(t *testing.T) {
	d := newDesk(t)
	links := d.post(t, golden(t, "m2-max-image2"))
	if links.status != http.StatusCreated {
		t.Fatalf("status %d: %v", links.status, links.body)
	}
	ids := d.store.list(t, `SELECT public_id FROM reports`)
	if len(ids) != 1 {
		t.Fatalf("%d reports stored", len(ids))
	}
	report, err := d.store.Report(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := encode(t, golden(t, "m2-max-image2")); !bytes.Equal(report.Body, want) {
		t.Error("the report stored is not the golden report")
	}
	if report.SchemaVersion != 1 {
		t.Errorf("schema version %d", report.SchemaVersion)
	}
	if !regexp.MustCompile(`\A[1-9A-HJ-NP-Za-km-z]{24}\z`).MatchString(report.PublicID) {
		t.Errorf("public id %q is not 24 base58 characters", report.PublicID)
	}
	if got, want := links.text("report_url"), site+"/reports/"+report.PublicID; got != want {
		t.Errorf("report_url %q, want %q", got, want)
	}
	if !regexp.MustCompile(`\A` + regexp.QuoteMeta(site+"/reports/"+report.PublicID+"/deletion?token=") + `[\w-]{40,}\z`).MatchString(links.text("deletion_url")) {
		t.Errorf("deletion_url %q", links.text("deletion_url"))
	}
	if links.text("id") != report.PublicID {
		t.Errorf("id %q, want %q", links.text("id"), report.PublicID)
	}
	if tester, ok := links.body["tester"].(bool); !ok || tester {
		t.Errorf("tester is %v, want false: this Mac is signed in as nobody", links.body["tester"])
	}
	if !report.CreatedAt.Equal(d.now) || report.Hidden {
		t.Errorf("stored at %v, hidden %v", report.CreatedAt, report.Hidden)
	}
	if want := testRules(t).MachineID(testKeyBlob("a")); report.MachineID != want {
		t.Errorf("machine id %q, want %q", report.MachineID, want)
	}
}

// Only a digest of the deletion link's secret is kept, and neither the key
// nor the signature.
func TestTheDeletionSecretTheKeyAndTheSignatureAreNotStored(t *testing.T) {
	d := newDesk(t)
	body := sent(t, golden(t, "m2-max-image2"), "a")
	links := d.post(t, body)
	u, err := url.Parse(links.text("deletion_url"))
	if err != nil {
		t.Fatal(err)
	}
	token := u.Query().Get("token")
	sum := sha256.Sum256([]byte(token))
	if got := d.store.list(t, `SELECT deletion_token_digest FROM reports`); len(got) != 1 || got[0] != hex.EncodeToString(sum[:]) {
		t.Errorf("deletion_token_digest %v, want the SHA-256 of the token in hex", got)
	}
	d.store.Close()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(d.path), "*"))
	if err != nil || len(files) == 0 {
		t.Fatal(err, files)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for what, secret := range map[string]string{"the deletion secret": token, "the Mac's key": strings.Fields(testPublicKey("a"))[1], "a signature": "SSH SIGNATURE"} {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s holds %s", filepath.Base(file), what)
			}
		}
	}
}

func TestEveryGoldenReportIsAccepted(t *testing.T) {
	d := newDesk(t)
	paths := goldenPaths(t, "*.json")
	if len(paths) == 0 {
		t.Fatal("no golden reports")
	}
	for _, path := range paths {
		if a := d.post(t, parse(t, read(t, path))); a.status != http.StatusCreated {
			t.Errorf("%s: %d %v", filepath.Base(path), a.status, a.body)
		}
	}
	if d.count(t) != len(paths) {
		t.Errorf("%d reports stored, want %d", d.count(t), len(paths))
	}
}

func TestReportsThatDontMatchTheSchemaAreRejectedAndNotStored(t *testing.T) {
	d := newDesk(t)
	g := func() *Object { return golden(t, "m2-max-image2") }
	withSerial := put(g(), "C02XXXXXXXXX", "machine", "serial_number")
	badCheckID := put(g(), "Not An Id", "checks", 0, "id")
	unclassified := g()
	put(unclassified, at(unclassified, "checks", 0).(*Object).without("classification"), "checks", 0)
	noCatalogueVersion := g().without("catalogue_version")
	newer := g().set("schema_version", integer(2))
	for name, report := range map[string]any{"a serial number": withSerial, "a bad check id": badCheckID, "no classification": unclassified,
		"no catalogue version": noCatalogueVersion, "a newer schema": newer, "not an object": "[]"} {
		a := d.post(t, report)
		if a.status != http.StatusUnprocessableEntity || !strings.Contains(a.text("error"), "does not match report schema v1") || len(a.details()) == 0 {
			t.Errorf("%s: %d %v", name, a.status, a.body)
		}
	}
	if d.count(t) != 0 {
		t.Errorf("%d reports stored", d.count(t))
	}
}

func TestCheckIDsTheFeatureCatalogueDoesntKnowAreRejectedWithAnUpgradeMessage(t *testing.T) {
	d := newDesk(t)
	a := d.post(t, put(golden(t, "m2-max-image2"), "made.up", "checks", 0, "id"))
	if a.status != http.StatusUnprocessableEntity || !strings.Contains(a.text("error"), "Update omarchy-m-test") {
		t.Errorf("%d %v", a.status, a.body)
	}
	if !strings.Contains(a.text("error"), "(curl -fsSL http://www.example.com/install | bash)") {
		t.Errorf("the upgrade instructions name another install line: %q", a.text("error"))
	}
	if got := a.details(); len(got) != 1 || got[0] != "unknown check id made.up" {
		t.Errorf("details %q", got)
	}
	if d.count(t) != 0 {
		t.Errorf("%d reports stored", d.count(t))
	}
}

func TestABodyThatIsntJSONIsABadRequestAndNotStored(t *testing.T) {
	d := newDesk(t)
	a := d.post(t, "not json")
	if a.status != http.StatusBadRequest || a.text("error") != "The report is not valid JSON." {
		t.Errorf("%d %v", a.status, a.body)
	}
	if d.count(t) != 0 {
		t.Errorf("%d reports stored", d.count(t))
	}
}

func TestOversizedReportsAreRefusedBeforeParsing(t *testing.T) {
	d := newDesk(t)
	report := sign(t, golden(t, "m2-max-image2"), "a").set("padding", strings.Repeat("x", 300<<10))
	a := d.post(t, encode(t, report))
	if a.status != http.StatusRequestEntityTooLarge || a.text("error") != "The report is larger than 256 KiB." {
		t.Errorf("%d %v", a.status, a.body)
	}
	// Exactly 256 KiB is read and judged: this one is refused for what it is.
	if a := d.post(t, "["+strings.Repeat(" ", 256<<10-2)+"]"); a.status != http.StatusUnprocessableEntity {
		t.Errorf("256 KiB: %d %v", a.status, a.body)
	}
	if a := d.post(t, "["+strings.Repeat(" ", 256<<10-1)+"]"); a.status != http.StatusRequestEntityTooLarge {
		t.Errorf("256 KiB and a byte: %d %v", a.status, a.body)
	}
	if d.count(t) != 0 {
		t.Errorf("%d reports stored", d.count(t))
	}
}

// A real server and real connections: what a body without a Content-Length
// looks like is the HTTP server's to say.
func TestAReportSentWithoutAContentLengthIsRefused(t *testing.T) {
	d := newDesk(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := d.uploads.Handle(w, r, "10.0.0.1", site); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	body := string(sent(t, golden(t, "m2-max-image2"), "a"))
	ask := func(request string) (int, string) {
		t.Helper()
		c, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		io.WriteString(c, request)
		reply, err := io.ReadAll(c)
		if err != nil {
			t.Fatal(err)
		}
		var status int
		fmt.Sscanf(string(reply), "HTTP/1.1 %d", &status)
		_, text, _ := strings.Cut(string(reply), "\r\n\r\n")
		return status, text
	}
	head := "POST /api/v1/reports HTTP/1.1\r\nHost: lab\r\nConnection: close\r\nContent-Type: application/json\r\n"
	chunked := head + "Transfer-Encoding: chunked\r\n\r\n" + fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(body), body)
	for name, request := range map[string]string{"in chunks": chunked, "with no body": head + "\r\n"} {
		status, text := ask(request)
		if status != http.StatusLengthRequired || !strings.Contains(text, `{"error":"Send the report with a Content-Length."}`) {
			t.Errorf("%s: %d %q", name, status, text)
		}
	}
	if status, text := ask(head + "Content-Length: 0\r\n\r\n"); status != http.StatusBadRequest {
		t.Errorf("an empty body with its length: %d %q", status, text)
	}
	if status, text := ask(head + fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)); status != http.StatusCreated {
		t.Errorf("with its length: %d %q", status, text)
	}
	if d.count(t) != 1 {
		t.Errorf("%d reports stored, want 1", d.count(t))
	}
}

func TestAReportInAnOutdatedSchemaIsRefusedWithTheLabsInstallLine(t *testing.T) {
	d := newDesk(t)
	for _, version := range []int64{0, -1} {
		a := d.post(t, golden(t, "m2-max-image2").set("schema_version", integer(version)))
		want := fmt.Sprintf("This report uses report schema v%d, which is outdated: the site accepts v1. "+
			"Update omarchy-m-test (curl -fsSL http://www.example.com/install | bash) and run it again.", version)
		if a.status != http.StatusUnprocessableEntity || a.text("error") != want {
			t.Errorf("%d %v", a.status, a.body)
		}
		if got := a.details(); len(got) != 1 || got[0] != fmt.Sprintf("schema_version %d is older than 1", version) {
			t.Errorf("details %q", got)
		}
	}
	if d.count(t) != 0 {
		t.Errorf("%d reports stored", d.count(t))
	}
}

func TestUploadsAreRateLimitedPerAddressWhicheverMachinesTheyComeFrom(t *testing.T) {
	d := newDesk(t)
	var last answer
	for n := range UploadsPerHour {
		last = d.post(t, golden(t, "m2-max-image2"), fmt.Sprintf("m%d", n), "10.0.0.1")
	}
	if last.status != http.StatusCreated {
		t.Fatalf("upload %d: %d %v", UploadsPerHour, last.status, last.body)
	}
	a := d.post(t, golden(t, "m2-max-image2"), "another", "10.0.0.1")
	if a.status != http.StatusTooManyRequests || a.text("error") != "Too many uploads from your network: at most 30 an hour. Try again later; the report is saved on your Mac." {
		t.Errorf("%d %v", a.status, a.body)
	}
	// Refused uploads count too, so a flood of junk is limited as well.
	if a := d.post(t, "not json", "a", "10.0.0.1"); a.status != http.StatusTooManyRequests {
		t.Errorf("junk past the limit: %d %v", a.status, a.body)
	}
	if a := d.post(t, golden(t, "m2-max-image2"), "another", "10.0.0.2"); a.status != http.StatusCreated {
		t.Errorf("another address: %d %v", a.status, a.body)
	}
	if d.count(t) != UploadsPerHour+1 {
		t.Errorf("%d reports stored, want %d", d.count(t), UploadsPerHour+1)
	}
}

func TestAFloodOfJunkUsesUpAnAddresssUploads(t *testing.T) {
	d := newDesk(t)
	for range UploadsPerHour {
		if a := d.post(t, "not json", "a", "10.0.0.1"); a.status != http.StatusBadRequest {
			t.Fatalf("%d %v", a.status, a.body)
		}
	}
	if a := d.post(t, golden(t, "m2-max-image2"), "a", "10.0.0.1"); a.status != http.StatusTooManyRequests {
		t.Errorf("after %d refused uploads: %d %v", UploadsPerHour, a.status, a.body)
	}
}

func TestUploadsAreRateLimitedPerMachineKeyFromAnyNetwork(t *testing.T) {
	d := newDesk(t)
	var last answer
	for n := range UploadsPerHourPerMachine {
		last = d.post(t, run(t, n), "a", fmt.Sprintf("10.0.1.%d", n))
	}
	if last.status != http.StatusCreated {
		t.Fatalf("upload %d: %d %v", UploadsPerHourPerMachine, last.status, last.body)
	}
	a := d.post(t, run(t, 99), "a", "10.0.2.1")
	if a.status != http.StatusTooManyRequests || a.text("error") != "Too many uploads from this Mac: at most 10 an hour. Try again later; the report is saved on your Mac." {
		t.Errorf("%d %v", a.status, a.body)
	}
	if a := d.post(t, run(t, 99), "b", "10.0.2.1"); a.status != http.StatusCreated {
		t.Errorf("another Mac: %d %v", a.status, a.body)
	}
	if d.count(t) != UploadsPerHourPerMachine+1 {
		t.Errorf("%d reports stored, want %d", d.count(t), UploadsPerHourPerMachine+1)
	}
}

func TestReportsWithAForgedSignatureDontUseUpTheRealMachinesUploads(t *testing.T) {
	d := newDesk(t)
	forged := sign(t, golden(t, "m2-max-image2"), "mallory")
	put(forged, testPublicKey("a"), "signature", "public_key")
	for n := range UploadsPerHourPerMachine + 1 {
		if a := d.post(t, encode(t, forged), "", fmt.Sprintf("10.0.1.%d", n)); a.status != http.StatusUnprocessableEntity {
			t.Fatalf("%d %v", a.status, a.body)
		}
	}
	if a := d.post(t, golden(t, "m2-max-image2"), "a", "10.0.2.1"); a.status != http.StatusCreated {
		t.Errorf("the real Mac: %d %v", a.status, a.body)
	}
}

func TestReportsTheLabRefusesAnywayDontCountTowardsTheMachinesLimit(t *testing.T) {
	d := newDesk(t)
	for n := range UploadsPerHourPerMachine + 1 {
		unknown := put(golden(t, "m2-max-image2"), "made.up", "checks", 0, "id")
		if a := d.post(t, unknown, "a", fmt.Sprintf("10.0.1.%d", n)); a.status != http.StatusUnprocessableEntity {
			t.Fatalf("%d %v", a.status, a.body)
		}
	}
	if a := d.post(t, golden(t, "m2-max-image2"), "a", "10.0.2.1"); a.status != http.StatusCreated {
		t.Errorf("a good report after %d refused ones: %d %v", UploadsPerHourPerMachine+1, a.status, a.body)
	}
}

// Both limits are by the hour: an hour after the first upload that counted,
// the count starts again.
func TestTheLimitsStartAgainAfterAnHour(t *testing.T) {
	d := newDesk(t)
	start := d.now
	for n := range UploadsPerHourPerMachine {
		d.now = start.Add(time.Duration(n) * 5 * time.Minute)
		d.post(t, run(t, n), "a", fmt.Sprintf("10.0.1.%d", n))
	}
	d.now = start
	for range UploadsPerHour {
		d.post(t, "not json", "a", "10.0.9.9")
	}
	d.now = start.Add(time.Hour - time.Second)
	if a := d.post(t, run(t, 50), "a", "10.0.2.1"); a.status != http.StatusTooManyRequests {
		t.Errorf("a second before the hour: %d %v", a.status, a.body)
	}
	if a := d.post(t, "not json", "a", "10.0.9.9"); a.status != http.StatusTooManyRequests {
		t.Errorf("a second before the hour, the address: %d %v", a.status, a.body)
	}
	d.now = start.Add(time.Hour + time.Second)
	if a := d.post(t, run(t, 51), "a", "10.0.2.1"); a.status != http.StatusCreated {
		t.Errorf("past the hour: %d %v", a.status, a.body)
	}
	if a := d.post(t, "not json", "a", "10.0.9.9"); a.status != http.StatusBadRequest {
		t.Errorf("past the hour, the address: %d %v", a.status, a.body)
	}

	// Each count has its own hour, from its own first upload.
	later := start.Add(90 * time.Minute)
	d.now = later
	for range UploadsPerHour {
		d.post(t, "not json", "a", "10.0.9.10")
	}
	d.now = later.Add(40 * time.Minute)
	d.post(t, "not json", "a", "10.0.9.11")
	d.now = later.Add(time.Hour - time.Second)
	if a := d.post(t, "not json", "a", "10.0.9.10"); a.status != http.StatusTooManyRequests {
		t.Errorf("a second before its own hour: %d %v", a.status, a.body)
	}
	d.now = later.Add(time.Hour)
	if a := d.post(t, "not json", "a", "10.0.9.10"); a.status != http.StatusBadRequest {
		t.Errorf("at its own hour: %d %v", a.status, a.body)
	}
}

// The eleven steps end an upload in the old site's order: the address's
// limit, 411, 413, 400, the 422s as Judge orders them, the Mac's limit, 201.
func TestTheStepsComeInTheOldOrder(t *testing.T) {
	d := newDesk(t)
	junk := func(length int64, body string) *http.Request {
		r := httptest.NewRequest("POST", site+"/api/v1/reports", strings.NewReader(body))
		r.ContentLength = length
		return r
	}
	huge := strings.Repeat("x", 300<<10)
	if a := d.serve(t, junk(-1, huge), "10.1.0.1"); a.status != http.StatusLengthRequired {
		t.Errorf("no length, too large and not JSON: %d, want 411", a.status)
	}
	if a := d.serve(t, junk(int64(len(huge)), huge), "10.1.0.1"); a.status != http.StatusRequestEntityTooLarge {
		t.Errorf("too large and not JSON: %d, want 413", a.status)
	}
	if a := d.post(t, "{", "a", "10.1.0.1"); a.status != http.StatusBadRequest {
		t.Errorf("not JSON: %d, want 400", a.status)
	}
	outdatedUnsigned := encode(t, golden(t, "m2-max-image2").set("schema_version", integer(0)))
	if a := d.post(t, outdatedUnsigned, "a", "10.1.0.1"); a.status != http.StatusUnprocessableEntity || !strings.Contains(a.text("error"), "outdated") {
		t.Errorf("outdated and unsigned: %d %v", a.status, a.body)
	}
	unsignedBadSchema := encode(t, golden(t, "m2-max-image2").without("checks"))
	if a := d.post(t, unsignedBadSchema, "a", "10.1.0.1"); a.status != http.StatusUnprocessableEntity || !strings.Contains(a.text("error"), "isn't signed") {
		t.Errorf("unsigned and off the schema: %d %v", a.status, a.body)
	}

	// The Mac's limit is the last refusal: past it, a report with something
	// wrong is still refused for that, and a good one for the limit.
	for n := range UploadsPerHourPerMachine {
		d.post(t, run(t, n), "a", fmt.Sprintf("10.2.0.%d", n))
	}
	badEvidence := put(run(t, 70), []any{"a\x00b"}, "checks", 0, "evidence")
	if a := d.post(t, badEvidence, "a", "10.1.0.2"); a.status != http.StatusUnprocessableEntity || !strings.Contains(a.text("error"), "evidence") {
		t.Errorf("bad evidence from a Mac past its limit: %d %v", a.status, a.body)
	}
	if a := d.post(t, run(t, 71), "a", "10.1.0.2"); a.status != http.StatusTooManyRequests || !strings.Contains(a.text("error"), "from this Mac") {
		t.Errorf("a good report from a Mac past its limit: %d %v", a.status, a.body)
	}

	// The address's limit comes before everything: past it, nothing is read.
	for range UploadsPerHour {
		d.post(t, "{", "a", "10.3.0.1")
	}
	for name, r := range map[string]*http.Request{"no length": junk(-1, huge), "too large": junk(int64(len(huge)), huge), "not JSON": junk(1, "{")} {
		if a := d.serve(t, r, "10.3.0.1"); a.status != http.StatusTooManyRequests || !strings.Contains(a.text("error"), "from your network") {
			t.Errorf("%s from an address past its limit: %d %v", name, a.status, a.body)
		}
	}
}

func TestTheSameReportTwiceIsRefused(t *testing.T) {
	d := newDesk(t)
	body := sent(t, golden(t, "m2-max-image2"), "a")
	first := d.post(t, body)
	if first.status != http.StatusCreated {
		t.Fatalf("%d %v", first.status, first.body)
	}
	again := d.post(t, body, "a", "10.0.0.2")
	if again.status != http.StatusConflict || again.text("error") != "Already uploaded" {
		t.Fatalf("a repeat: %d %v", again.status, again.body)
	}
	if again.text("report_url") != first.text("report_url") {
		t.Errorf("a repeat names %q, want the report's link %q", again.text("report_url"), first.text("report_url"))
	}
	if _, has := again.body["deletion_url"]; has || len(again.body) != 2 {
		t.Errorf("a repeat is answered with more than the report's link: %v", again.body)
	}
	u, _ := url.Parse(first.text("deletion_url"))
	if raw, _ := json.Marshal(again.body); strings.Contains(string(raw), u.Query().Get("token")) {
		t.Error("a repeat was given the deletion link's secret")
	}

	// The same report however it is written, and signed again by its Mac.
	var spaced bytes.Buffer
	if err := json.Indent(&spaced, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	resigned := signed(t, golden(t, "m2-max-image2"), "a", NamespaceReport, func(s *sshsig) { s.hash = "sha256" }, nil)
	for name, repeat := range map[string][]byte{"indented": spaced.Bytes(), "signed again": encode(t, resigned)} {
		if a := d.post(t, repeat); a.status != http.StatusConflict || a.text("report_url") != first.text("report_url") {
			t.Errorf("%s: %d %v", name, a.status, a.body)
		}
	}
	if d.count(t) != 1 {
		t.Errorf("%d reports stored, want 1", d.count(t))
	}

	// Another Mac's report with the same words is that Mac's own, and
	// another run on the first Mac is another report.
	if a := d.post(t, golden(t, "m2-max-image2"), "b"); a.status != http.StatusCreated {
		t.Errorf("another Mac: %d %v", a.status, a.body)
	}
	if a := d.post(t, run(t, 2)); a.status != http.StatusCreated {
		t.Errorf("another run: %d %v", a.status, a.body)
	}
}

// A repeat is not a report that would be stored, so it uses up none of its
// Mac's uploads; and a Mac past its limit still hears "Already uploaded".
func TestARepeatDoesNotCountAgainstItsMac(t *testing.T) {
	d := newDesk(t)
	first := sent(t, run(t, 0), "a")
	d.post(t, first, "a", "10.0.1.0")
	for n := range 3 * UploadsPerHourPerMachine {
		if a := d.post(t, first, "a", fmt.Sprintf("10.0.3.%d", n)); a.status != http.StatusConflict {
			t.Fatalf("repeat %d: %d %v", n, a.status, a.body)
		}
	}
	for n := 1; n < UploadsPerHourPerMachine; n++ {
		if a := d.post(t, run(t, n), "a", fmt.Sprintf("10.0.1.%d", n)); a.status != http.StatusCreated {
			t.Fatalf("upload %d: %d %v", n+1, a.status, a.body)
		}
	}
	if a := d.post(t, run(t, 99), "a", "10.0.4.1"); a.status != http.StatusTooManyRequests {
		t.Errorf("upload %d: %d %v", UploadsPerHourPerMachine+1, a.status, a.body)
	}
	if a := d.post(t, first, "a", "10.0.4.2"); a.status != http.StatusConflict {
		t.Errorf("a repeat past the limit: %d %v", a.status, a.body)
	}
}

func deletionToken(t testing.TB, links answer) string {
	t.Helper()
	u, err := url.Parse(links.text("deletion_url"))
	if err != nil || u.Query().Get("token") == "" {
		t.Fatalf("deletion_url %q: %v", links.text("deletion_url"), err)
	}
	return u.Query().Get("token")
}

func TestADeletedReportsDigestStillRefuses(t *testing.T) {
	d := newDesk(t)
	body := sent(t, golden(t, "m2-max-image2"), "a")
	links := d.post(t, body)
	if err := d.store.DeleteByLink(links.text("id"), deletionToken(t, links)); err != nil {
		t.Fatal(err)
	}
	if d.count(t) != 0 {
		t.Fatalf("%d reports after the deletion", d.count(t))
	}
	a := d.post(t, body)
	if a.status != http.StatusConflict || a.text("error") != "Already uploaded" {
		t.Fatalf("a deleted report posted back: %d %v", a.status, a.body)
	}
	if len(a.body) != 2 || len(a.details()) != 1 || a.details()[0] != "The report was deleted since." {
		t.Errorf("a deleted report posted back is answered %v", a.body)
	}
	if raw, _ := json.Marshal(a.body); strings.Contains(string(raw), links.text("id")) {
		t.Error("the answer names the deleted report")
	}
	if d.count(t) != 0 {
		t.Errorf("the deleted report is back")
	}
	// What stays of a deleted report is its digest and nothing of the report.
	if got := d.store.list(t, `SELECT typeof(report_id) FROM report_digests`); len(got) != 1 || got[0] != "null" {
		t.Errorf("the digest of a deleted report still points somewhere: %v", got)
	}
}

// The old site's row lock is a write transaction here: of one report sent
// many times at once, one is stored.
func TestOneReportSentManyTimesAtOnceIsStoredOnce(t *testing.T) {
	d := newDesk(t)
	body := sent(t, golden(t, "m2-max-image2"), "a")
	const senders = 8
	statuses := make([]int, senders)
	var wg sync.WaitGroup
	for n := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("POST", site+"/api/v1/reports", bytes.NewReader(body))
			w := httptest.NewRecorder()
			if err := d.uploads.Handle(w, r, fmt.Sprintf("10.0.5.%d", n), site); err != nil {
				t.Error(err)
			}
			statuses[n] = w.Code
		}()
	}
	wg.Wait()
	counts := map[int]int{}
	for _, status := range statuses {
		counts[status]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != senders-1 {
		t.Errorf("answers %v, want one 201 and %d 409", counts, senders-1)
	}
	if d.count(t) != 1 {
		t.Errorf("%d reports stored, want 1", d.count(t))
	}
}

// A report is stamped with the person its Mac is signed in as at upload.
func TestAReportCarriesWhoItsMacWasSignedInAs(t *testing.T) {
	d := newDesk(t)
	machine := testRules(t).MachineID(testKeyBlob("a"))
	at := d.now.UnixMicro()
	if _, err := d.store.db.Exec(`INSERT INTO mac_sign_ins (machine_id, account_id, handle, created_at, updated_at) VALUES (?, 4242, 'ada', ?, ?)`, machine, at, at); err != nil {
		t.Fatal(err)
	}
	signedIn := d.post(t, run(t, 1), "a")
	anonymous := d.post(t, run(t, 1), "b")
	for _, c := range []struct {
		links   answer
		handle  string
		account int64
	}{{signedIn, "ada", 4242}, {anonymous, "", 0}} {
		report, err := d.store.Report(c.links.text("id"))
		if err != nil {
			t.Fatal(err)
		}
		if report.Handle != c.handle || report.AccountID != c.account {
			t.Errorf("stamped %q %d, want %q %d", report.Handle, report.AccountID, c.handle, c.account)
		}
	}
	// Signing out later changes no stored report.
	if _, err := d.store.db.Exec(`DELETE FROM mac_sign_ins`); err != nil {
		t.Fatal(err)
	}
	if report, _ := d.store.Report(signedIn.text("id")); report == nil || report.Handle != "ada" {
		t.Error("the stamp went with the sign-in")
	}
}

func TestADeletionLinkDeletesItsReportAndNoOther(t *testing.T) {
	d := newDesk(t)
	kept := d.post(t, run(t, 1))
	links := d.post(t, run(t, 2))
	token := deletionToken(t, links)

	report, err := d.store.Report(links.text("id"))
	if err != nil {
		t.Fatal(err)
	}
	for _, wrong := range []string{"", "wrong", token + "x", token[:len(token)-1], deletionToken(t, kept)} {
		if report.DeletionTokenMatches(wrong) {
			t.Errorf("the token %q opens the deletion", wrong)
		}
		if err := d.store.DeleteByLink(links.text("id"), wrong); err != ErrNoReport {
			t.Errorf("deleting with the token %q: %v", wrong, err)
		}
	}
	// A row whose digest is that of no secret at all is opened by none.
	if _, err := d.store.db.Exec(`UPDATE reports SET deletion_token_digest = ? WHERE public_id = ?`, tokenDigest(""), kept.text("id")); err != nil {
		t.Fatal(err)
	}
	if err := d.store.DeleteByLink(kept.text("id"), ""); err != ErrNoReport {
		t.Errorf("deleting with no secret a report whose digest is the empty secret's: %v", err)
	}
	if err := d.store.DeleteByLink("nosuchreport", token); err != ErrNoReport {
		t.Errorf("deleting a report that is not there: %v", err)
	}
	if d.count(t) != 2 {
		t.Fatalf("%d reports, want 2: a wrong token deleted something", d.count(t))
	}
	if !report.DeletionTokenMatches(token) {
		t.Fatal("the report's own token does not match")
	}
	if err := d.store.DeleteByLink(links.text("id"), token); err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.Report(links.text("id")); err != ErrNoReport {
		t.Errorf("the deleted report: %v", err)
	}
	if _, err := d.store.Report(kept.text("id")); err != nil {
		t.Errorf("the other report: %v", err)
	}
	if err := d.store.DeleteByLink(links.text("id"), token); err != ErrNoReport {
		t.Errorf("deleting twice: %v", err)
	}
}

func TestDeletingRemovesTheReportAndItsEvidenceCompletelyHiddenOrNot(t *testing.T) {
	d := newDesk(t)
	links := d.post(t, golden(t, "m2-max-image2"))
	if _, err := d.store.db.Exec(`UPDATE reports SET hidden_at = ?`, d.now.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if report, err := d.store.Report(links.text("id")); err != nil || !report.Hidden {
		t.Fatalf("the hidden report: %v %v", report, err)
	}
	var evidence string
	for _, check := range at(golden(t, "m2-max-image2"), "checks").([]any) {
		if at(check, "id") == "setup.first-boot-hardware" {
			evidence = at(check, "evidence", 0).(string)
		}
	}
	if evidence == "" {
		t.Fatal("the golden report has no first-boot evidence")
	}
	holds := func() int {
		return len(d.store.list(t, `SELECT public_id FROM reports WHERE instr(body, ?) > 0`, evidence))
	}
	if holds() != 1 {
		t.Fatal("the evidence is not in the stored report")
	}
	if err := d.store.DeleteByLink(links.text("id"), deletionToken(t, links)); err != nil {
		t.Fatal(err)
	}
	if d.count(t) != 0 || holds() != 0 {
		t.Error("the report or its evidence is still stored")
	}
}

// A row carried from the old site: its body as Postgres wrote it out, its
// machine id from before Machine keys, the digest of a token the old site
// issued. Its links work as they did.
func TestAReportCarriedFromTheOldSiteIsReadAndDeletedByItsOldLink(t *testing.T) {
	d := newDesk(t)
	const oldToken = "Zm9yIHRoZSBvbGQgc2l0ZSdzIGRlbGV0aW9uIGxpbms_-x"
	var body bytes.Buffer
	if err := json.Indent(&body, encode(t, golden(t, "m2-max-image2")), "", " "); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 26, 3, 4, 5, 123456000, time.UTC)
	if _, err := d.store.db.Exec(`INSERT INTO reports (id, public_id, deletion_token_digest, schema_version, body, created_at, updated_at, machine_id, handle) VALUES (132, 'h7Qm2ZpX4cVb9NkT3sWd8RfY', ?, 1, ?, ?, ?, 'ip:0123456789abcdef0123', 'ada')`,
		tokenDigest(oldToken), body.String(), created.UnixMicro(), created.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	report, err := d.store.Report("h7Qm2ZpX4cVb9NkT3sWd8RfY")
	if err != nil {
		t.Fatal(err)
	}
	if report.ID != 132 || !report.CreatedAt.Equal(created) || report.Handle != "ada" || report.AccountID != 0 || report.MachineID != "ip:0123456789abcdef0123" {
		t.Errorf("the carried report: %+v", report)
	}
	if model := at(report.Document(), "machine", "model"); model != "Apple MacBook Pro (16-inch, M2 Max, 2023)" {
		t.Errorf("its model reads %v", model)
	}
	if !report.DeletionTokenMatches(oldToken) || report.DeletionTokenMatches(oldToken+"x") {
		t.Error("the old site's token does not open its deletion, or another does")
	}
	if err := d.store.DeleteByLink(report.PublicID, oldToken); err != nil {
		t.Fatal(err)
	}
	if d.count(t) != 0 {
		t.Error("the carried report is still there")
	}
	// The next report's id goes on from the carried one.
	links := d.post(t, golden(t, "m2-max-image2"))
	if next, err := d.store.Report(links.text("id")); err != nil || next.ID != 133 {
		t.Errorf("the next report: %+v %v", next, err)
	}
}

func TestAnUnattendedReportIsAcceptedAndUnattendedCanOnlyBeTrue(t *testing.T) {
	d := newDesk(t)
	links := d.post(t, golden(t, "m2-max-image2").set("unattended", true))
	if links.status != http.StatusCreated {
		t.Fatalf("%d %v", links.status, links.body)
	}
	report, err := d.store.Report(links.text("id"))
	if err != nil {
		t.Fatal(err)
	}
	if unattended, _ := report.Document().Get("unattended"); unattended != true {
		t.Errorf("the stored report's unattended is %v", unattended)
	}
	if a := d.post(t, golden(t, "m2-max-image2").set("unattended", false)); a.status != http.StatusUnprocessableEntity {
		t.Errorf("unattended false: %d %v", a.status, a.body)
	}
}

// The old site could not store a report with a NUL outside its evidence, and
// failed. Nothing here refuses one, and the store holds it.
func TestAReportWithANulInItsTextIsStored(t *testing.T) {
	d := newDesk(t)
	links := d.post(t, put(golden(t, "m2-max-image2"), "7.1.12\x00-ARCH", "machine", "kernel"))
	if links.status != http.StatusCreated {
		t.Fatalf("%d %v", links.status, links.body)
	}
	report, err := d.store.Report(links.text("id"))
	if err != nil {
		t.Fatal(err)
	}
	if kernel := at(report.Document(), "machine", "kernel"); kernel != "7.1.12\x00-ARCH" {
		t.Errorf("the kernel reads back as %q", kernel)
	}
	if got := d.store.list(t, `SELECT body -> 'machine' ->> 'board' FROM reports`); len(got) != 1 || got[0] != "j416c" {
		t.Errorf("the stored body is not read as JSON: %v", got)
	}
}

// A lab run's report comes in by Receive, inside labd: the same rules and
// the Mac's limit, and no address to count.
func TestAReportHandedOverInsideIsJudgedAndCountedForItsMac(t *testing.T) {
	d := newDesk(t)
	for n := range UploadsPerHourPerMachine {
		stored, refusal, err := d.uploads.Receive(sent(t, run(t, n), "a"), site+"/install")
		if err != nil || refusal != nil || stored.PublicID == "" || stored.DeletionToken == "" {
			t.Fatalf("report %d: %+v %+v", n, stored, refusal)
		}
	}
	if _, refusal, _ := d.uploads.Receive(sent(t, run(t, 99), "a"), site+"/install"); refusal == nil || refusal.Status != http.StatusTooManyRequests {
		t.Errorf("past the Mac's limit: %+v", refusal)
	}
	stored, refusal, err := d.uploads.Receive(sent(t, run(t, 0), "a"), site+"/install")
	if err != nil || stored != nil || refusal == nil || refusal.Status != http.StatusConflict || refusal.Error != "Already uploaded" || refusal.Held == "" {
		t.Errorf("a repeat: %+v %+v", stored, refusal)
	}
	if _, refusal, _ := d.uploads.Receive([]byte("not json"), site+"/install"); refusal == nil || refusal.Status != http.StatusBadRequest {
		t.Errorf("junk: %+v", refusal)
	}
	if d.count(t) != UploadsPerHourPerMachine {
		t.Errorf("%d reports stored, want %d", d.count(t), UploadsPerHourPerMachine)
	}
}

// SignedBy is the machine id of the Machine key that signed a report, read
// when the lab collects a run and before anything is published: it stores
// nothing and counts nothing for the Mac.
func TestSignedByIsTheMachineIDAReportIsStoredUnderAndCountsNothing(t *testing.T) {
	d := newDesk(t)
	a, ok := d.uploads.SignedBy(sent(t, run(t, 1), "a"))
	b, okB := d.uploads.SignedBy(sent(t, run(t, 1), "b"))
	if !ok || !okB || a == "" || a == b {
		t.Fatalf("signed by a: %q %v; by b: %q %v", a, ok, b, okB)
	}
	forged := sign(t, run(t, 2), "mallory")
	put(forged, testPublicKey("a"), "signature", "public_key")
	for name, body := range map[string][]byte{"forged": encode(t, forged), "unsigned": encode(t, run(t, 3)), "junk": []byte("not json")} {
		if id, ok := d.uploads.SignedBy(body); ok || id != "" {
			t.Errorf("%s: signed by %q", name, id)
		}
	}
	for range UploadsPerHourPerMachine * 2 {
		d.uploads.SignedBy(sent(t, run(t, 4), "a"))
	}
	stored, refusal, err := d.uploads.ReceiveFromLab(sent(t, run(t, 5), "a"), site+"/install", Lab{Job: "j1", Machine: a})
	if err != nil || refusal != nil || stored.MachineID != a {
		t.Fatalf("the Mac's own report after SignedBy: %+v %+v %v", stored, refusal, err)
	}
	if d.count(t) != 1 {
		t.Errorf("%d reports stored, want 1", d.count(t))
	}
	_, refusal, _ = d.uploads.ReceiveFromLab(sent(t, run(t, 6), "b"), site+"/install", Lab{Job: "j2", Machine: a})
	if refusal == nil || refusal.Status != http.StatusForbidden || refusal.Error != AnotherMachineKey {
		t.Errorf("a report of another key: %+v", refusal)
	}
}

func TestTheMachineIDKeyIsReadFromAFileOnlyItsAccountReads(t *testing.T) {
	dir := t.TempDir()
	key := railsMachineIDKey(t, paritySecretKeyBase)
	path := filepath.Join(dir, "machine-id-key")
	write := func(text string, mode os.FileMode) {
		t.Helper()
		os.Remove(path)
		if err := os.WriteFile(path, []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ReadMachineIDKey(""); got != nil || err != nil {
		t.Errorf("no file named: %v %v", got, err)
	}
	if got, err := ReadMachineIDKey(path); got != nil || err != nil {
		t.Errorf("no file there: %v %v", got, err)
	}
	write(hex.EncodeToString(key)+"\n", 0o600)
	if got, err := ReadMachineIDKey(path); err != nil || !bytes.Equal(got, key) {
		t.Errorf("the key: %v", err)
	}
	if _, err := NewRules(key); err != nil {
		t.Error(err)
	}
	write(hex.EncodeToString(key)+"\n", 0o644)
	if _, err := ReadMachineIDKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a key other accounts can read: %v", err)
	}
	for name, text := range map[string]string{"short": hex.EncodeToString(key[:32]), "not hex": strings.Repeat("z", 128), "empty": ""} {
		write(text, 0o600)
		got, err := ReadMachineIDKey(path)
		if err == nil || got != nil {
			t.Errorf("a %s key was taken", name)
		}
		if err != nil && strings.Contains(err.Error(), hex.EncodeToString(key[:8])) {
			t.Errorf("the refusal of a %s key repeats it: %v", name, err)
		}
	}
}
