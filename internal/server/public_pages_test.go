package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/catalogue"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// The home page, Matrix, Models and Features, translated from the old
// site's page tests: site/test/integration/matrix_test.rb,
// model_results_test.rb, mac_lineup_test.rb, filters_test.rb,
// build_identity_test.rb, answered_by_default_test.rb, regressions_test.rb,
// pages_test.rb and site_test.rb. The ids, classes and data- attributes
// those tests pin are asked for here by the same selectors. What they ask of
// the reports list, a report's page, Kernel gaps, Candidates and the exports
// waits for those pages.

// publicLab is a lab whose reports.db the test fills as an import would.
type publicLab struct {
	*reportsLab
	db *sql.DB
	at time.Time
	n  int
}

func newPublicLab(t *testing.T, config func(*Config)) *publicLab {
	t.Helper()
	l := &publicLab{reportsLab: newReportsLab(t, config), at: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	db, err := sql.Open("sqlite", filepath.Join(l.s.cfg.DataDir, "reports.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	l.db = db
	return l
}

type doc = map[string]any

func goldenDoc(t *testing.T, name string) doc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var body doc
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	delete(body, "signature")
	return body
}

func checkOf(t *testing.T, body doc, id string) doc {
	t.Helper()
	for _, c := range body["checks"].([]any) {
		if c.(doc)["id"] == id {
			return c.(doc)
		}
	}
	t.Fatalf("the report has no check %s", id)
	return nil
}

// goldenWith is the golden report with some checks' status and outcome changed.
func goldenWith(t *testing.T, name string, outcomes ...string) doc {
	t.Helper()
	body := goldenDoc(t, name)
	for i := 0; i+1 < len(outcomes); i += 2 {
		c := checkOf(t, body, outcomes[i])
		c["status"] = map[string]string{"works": "pass", "not-tested": "skip"}[outcomes[i+1]]
		if c["status"] == "" {
			c["status"] = "fail"
		}
		c["classification"].(doc)["outcome"] = outcomes[i+1]
	}
	return body
}

func setPackage(t *testing.T, body doc, name, version string) doc {
	t.Helper()
	for _, p := range body["system"].(doc)["packages"].([]any) {
		if p.(doc)["name"] == name {
			p.(doc)["version"] = version
			return body
		}
	}
	t.Fatalf("the report has no package %s", name)
	return nil
}

// add stores a report from a machine, a minute after the one before, and
// gives its public id. A machine named "verified:x" was signed in as an M
// team member: its runs are Verified runs.
func (l *publicLab) add(body doc, machine string) string {
	l.t.Helper()
	l.n++
	id := fmt.Sprintf("run%021d", l.n)
	l.store(id, body, "key:"+machine, l.at, strings.HasPrefix(machine, "verified:"))
	l.at = l.at.Add(time.Minute)
	return id
}

func (l *publicLab) store(id string, body doc, machineID string, at time.Time, verified bool) {
	l.t.Helper()
	text, err := json.Marshal(body)
	if err != nil {
		l.t.Fatal(err)
	}
	var handle, account any
	if verified {
		handle, account = "team-"+strings.TrimPrefix(machineID, "key:verified:"), int64(len(machineID))*1000+int64(machineID[len(machineID)-1])
	}
	_, err = l.db.Exec(`INSERT INTO reports (public_id, deletion_token_digest, schema_version, body, created_at, updated_at, machine_id, handle, account_id, verified)
		VALUES (?, 'digest', 1, ?, ?, ?, ?, ?, ?, ?)`, id, string(text), at.UnixMicro(), at.UnixMicro(), machineID, handle, account, verified)
	if err != nil {
		l.t.Fatal(err)
	}
}

func (l *publicLab) exec(query string, args ...any) {
	l.t.Helper()
	if _, err := l.db.Exec(query, args...); err != nil {
		l.t.Fatal(err)
	}
}

// production stores the old site's production reports as it held them,
// public ids kept.
func (l *publicLab) production() []string {
	l.t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "production", "reports.json"))
	if err != nil {
		l.t.Fatal(err)
	}
	var file struct {
		Reports []struct {
			ID         string `json:"id"`
			UploadedAt string `json:"uploaded_at"`
			Report     doc    `json:"report"`
		} `json:"reports"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		l.t.Fatal(err)
	}
	var ids []string
	for _, entry := range file.Reports {
		at, err := time.Parse(time.RFC3339, entry.UploadedAt)
		if err != nil {
			l.t.Fatal(err)
		}
		delete(entry.Report, "signature")
		l.store(entry.ID, entry.Report, "key:machine-"+entry.ID, at, false)
		ids = append(ids, entry.ID)
	}
	return ids
}

// page is a public page, asked for signed out through the tunnel.
func (l *publicLab) page(target string) *node {
	l.t.Helper()
	w := l.do("GET", target, "")
	if w.Code != http.StatusOK {
		l.t.Fatalf("GET %s: %d %s", target, w.Code, w.Body)
	}
	return parseHTML(l.t, w.Body.String())
}

func (l *publicLab) status(target string) int {
	l.t.Helper()
	return l.do("GET", target, "").Code
}

const (
	m2Board    = "j416c"
	m3ID       = "xHGd4Toq7io8CSHYYFhTjZBD"
	m3Build    = "f60e1ba.2026092602"
	m3Words    = "f60e1ba.2026092602 (linux-aurora 7.1.12.aurora2-11, omarchy-mac-boot 20260926-1)"
	m1AirBuild = "0052582.362956774610001"
	oldKernel  = "7.1.12-2-2-ARCH"
	newKernel  = "7.1.12-2-11-ARCH"
)

func buildCell(t *testing.T, page *node, board, stack, version, feature string) *node {
	t.Helper()
	return page.sole(t, fmt.Sprintf(`tr.matrix-row[data-board="%s"][data-stack="%s"][data-version="%s"] td[data-feature="%s"]`, board, stack, version, feature))
}

func m2Cell(t *testing.T, page *node, feature string) *node {
	return buildCell(t, page, m2Board, "converged", "4.0.0-alpha", feature)
}

func testedFeatures(t *testing.T) (all, tested int, byLayer map[string]int) {
	t.Helper()
	cat, err := reports.LoadCatalogue(catalogue.JSON)
	if err != nil {
		t.Fatal(err)
	}
	byLayer = map[string]int{}
	for _, feature := range cat.TestedFeatures() {
		layer, _ := feature.Get("layer")
		byLayer[layer.(string)]++
	}
	return len(cat.Features()), len(cat.TestedFeatures()), byLayer
}

func TestASingleCommunityRunIsListedButLeavesTheMatrixUnconfirmed(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	page := l.page("/matrix?group=build")
	gpu := m2Cell(t, page, "gpu")
	if gpu.attr("data-state") != "unconfirmed" || !strings.Contains(gpu.attr("class"), "cell-unconfirmed cell-hint-works") ||
		!strings.Contains(gpu.attr("title"), "community, unconfirmed: 1 machine works") {
		t.Errorf("gpu: %v", gpu.attrs)
	}
	if !page.hasText(t, "tr.matrix-row .row-meta", "converged 4.0.0-alpha · 1 report") {
		t.Errorf("the row says %q", page.sole(t, "tr.matrix-row .row-meta").words())
	}

	// Two reports from the same machine don't confirm each other.
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	page = l.page("/matrix?group=build")
	if m2Cell(t, page, "gpu").attr("data-state") != "unconfirmed" || !page.hasText(t, "tr.matrix-row .row-meta", "converged 4.0.0-alpha · 2 reports") {
		t.Errorf("two runs of one machine: %v", m2Cell(t, page, "gpu").attrs)
	}

	// Two distinct machines agreeing colour the cell.
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	page = l.page("/matrix?group=build")
	gpu = m2Cell(t, page, "gpu")
	if gpu.attr("data-state") != "works" || !strings.Contains(gpu.attr("class"), "cell-works") || gpu.attr("title") != "works: 2 machines works" {
		t.Errorf("gpu: %v", gpu.attrs)
	}
	if got := m2Cell(t, page, "first-boot-hardware-setup").attr("data-state"); got != "partial" {
		t.Errorf("first-boot-hardware-setup: %q", got)
	}
	if page.count(t, `td[data-feature="gpu"] a[href^="/reports/"]`) == 0 {
		t.Error("the gpu cell links no report")
	}

	// Reports uploaded before machine ids existed count as one machine.
	l.exec(`UPDATE reports SET machine_id = NULL`)
	if got := m2Cell(t, l.page("/matrix?group=build"), "gpu").attr("data-state"); got != "unconfirmed" {
		t.Errorf("without machine ids: %q", got)
	}
}

func TestAgreeingFailuresAreOrangeMissingSupportBlueAndUnknownHardwareMagenta(t *testing.T) {
	l := newPublicLab(t, nil)
	for _, machine := range []string{"a", "b"} {
		l.add(goldenWith(t, "m2-max-image2", "display.backlight", "fails", "input.ambient-light", "not-in-aurora", "power.battery", "unknown-hardware",
			"network.bluetooth", "not-applicable"), machine)
	}
	page := l.page("/matrix?group=build")
	for feature, want := range map[string]string{"brightness": "fails", "aop": "missing", "battery-info": "unknown", "bluetooth": "not-applicable"} {
		if got := m2Cell(t, page, feature).attr("data-state"); got != want {
			t.Errorf("%s: %q, want %q", feature, got, want)
		}
	}
	for _, words := range []string{"regression", "doesn't work", "expected missing", "unknown hardware"} {
		if !page.says(t, ".legend li", words) {
			t.Errorf("the legend lacks %q", words)
		}
	}
}

func TestRowsArePerModelAndStackVersionAndTheStackFilterNarrowsThem(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "a")
	newer := goldenDoc(t, "m2-max-image2")
	newer["system"].(doc)["packages"].([]any)[0].(doc)["version"] = "4.1.0-1"
	l.add(newer, "a")
	_, tested, _ := testedFeatures(t)

	page := l.page("/matrix?group=build")
	if page.count(t, "tr.matrix-row") != 3 || page.count(t, `tr.matrix-row[data-board="j416c"]`) != 2 || page.count(t, "th.feature-head") != tested {
		t.Errorf("%d rows, %d columns", page.count(t, "tr.matrix-row"), page.count(t, "th.feature-head"))
	}
	if got := attrsOf(page.all(t, `tr.matrix-row[data-board="j416c"]`), "data-version"); !reflect.DeepEqual(got, []string{"4.1.0", "4.0.0-alpha"}) {
		t.Errorf("the M2's versions: %v", got)
	}
	page = l.page("/matrix?group=build&stack=mx-mac")
	if page.count(t, "tr.matrix-row") != 1 || page.count(t, `tr.matrix-row[data-board="j314s"][data-stack="mx-mac"]`) != 1 {
		t.Errorf("stack=mx-mac: %d rows", page.count(t, "tr.matrix-row"))
	}
	if !page.hasText(t, `select[name="stack"] option[selected]`, "mx-mac") || !page.says(t, `.filter-chip[data-filter="stack"]`, "stack: mx-mac") {
		t.Error("the picked stack does not show in the form and as a chip")
	}
}

func TestTheHomePageShowsTheMatrixOrSaysThereAreNoReportsYet(t *testing.T) {
	l := newPublicLab(t, nil)
	page := l.page("/")
	if !page.says(t, ".card", "No reports yet") {
		t.Error("an empty lab does not say so")
	}
	if got := page.sole(t, "#install").words(); got != "curl -fsSL "+labAddress+"/install | bash" {
		t.Errorf("the install command: %q", got)
	}
	home := l.do("GET", "/", "").Body.String()
	offersToCopy(t, home, "the install command", "curl -fsSL "+labAddress+"/install | bash")
	offersToCopy(t, home, "omarchy-m-test", "omarchy-m-test")
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	page = l.page("/")
	if page.count(t, "table.matrix tr.model-summary") != 1 || page.count(t, "tbody.matrix-history tr.build-row") != 1 {
		t.Errorf("%d summaries, %d builds", page.count(t, "table.matrix tr.model-summary"), page.count(t, "tbody.matrix-history tr.build-row"))
	}
	for _, link := range []string{"/matrix", "/models", "/features", "/gaps", "/reports", "/data"} {
		if page.count(t, fmt.Sprintf(`main a[href="%s"]`, link)) == 0 {
			t.Errorf("the home page does not link %s", link)
		}
	}
}

// Model summaries span releases and builds, with their evidence and history intact.
func (l *publicLab) upload(outcome, kernel, release, machine, stack string) string {
	body := goldenWith(l.t, "m2-max-image2", "display.backlight", outcome)
	body["machine"].(doc)["kernel"] = kernel
	body["system"].(doc)["stack"] = stack
	setPackage(l.t, body, "omarchy", release)
	setPackage(l.t, body, "linux-aurora", kernel)
	return l.add(body, machine)
}

func summaryCell(t *testing.T, page *node, feature, stack string) *node {
	t.Helper()
	return page.sole(t, fmt.Sprintf(`tr.model-summary[data-board="j416c"][data-stack="%s"] td[data-feature="%s"]`, stack, feature))
}

func kernelsOf(t *testing.T, cell *node) []string {
	t.Helper()
	var kernels []string
	if err := json.Unmarshal([]byte(cell.attr("data-kernels")), &kernels); err != nil {
		t.Fatalf("data-kernels %q: %v", cell.attr("data-kernels"), err)
	}
	sort.Strings(kernels)
	return kernels
}

func TestTheDefaultModelViewReplacesAFailureWithALaterPassAndPreservesItThroughASkip(t *testing.T) {
	l := newPublicLab(t, nil)
	l.upload("fails", oldKernel, "4.0.0-1", "verified:a", "converged")
	passed := l.upload("works", newKernel, "4.1.0-1", "verified:a", "converged")
	l.upload("not-tested", "7.1.13-1-1-ARCH", "4.2.0-1", "verified:a", "converged")

	page := l.page("/matrix")
	cell := summaryCell(t, page, "brightness", "converged")
	if !page.hasText(t, `.matrix-view-switch a[aria-current=page][data-view=model]`, "Model") || page.count(t, "tr.model-summary") != 1 {
		t.Error("the model view is not the default")
	}
	if cell.attr("data-state") != "works" || !reflect.DeepEqual(kernelsOf(t, cell), []string{newKernel}) || cell.count(t, fmt.Sprintf(`a[href="/reports/%s"]`, passed)) != 1 {
		t.Errorf("the summary cell: %v", cell.attrs)
	}
	if page.count(t, "tbody.matrix-history tr.matrix-row") != 3 || page.count(t, "tbody.matrix-history[hidden]") != 0 {
		t.Error("builds remain available without JavaScript: want 3 rows, none hidden")
	}
	if page.count(t, "button.model-toggle[hidden][aria-controls]") != 1 || page.count(t, "form.filter-bar input[name=group][value=model]") != 1 ||
		page.count(t, `.filter-exports a[href="/api/v1/matrix.json?group=model"]`) != 1 || page.count(t, ".filter-count[data-count=3]") != 1 {
		t.Error("the toggle, the group field, the export link or the count is not as the old page's")
	}
	if page.count(t, `script[src^="/assets/model-results-"]`) != 1 || page.count(t, `script[src^="/assets/result-tooltips-"]`) != 1 {
		t.Error("the page does not load its two scripts")
	}

	page = l.page("/")
	if page.count(t, "tr.model-summary") != 1 || summaryCell(t, page, "brightness", "converged").attr("data-state") != "works" || page.count(t, "tbody.matrix-history tr.matrix-row") != 3 {
		t.Error("the home page's summary differs")
	}
}

func TestALaterFailureSupersedesThePassAndUploadOrderDecides(t *testing.T) {
	l := newPublicLab(t, nil)
	l.upload("works", oldKernel, "4.0.0-1", "verified:a", "converged")
	failure := l.upload("fails", newKernel, "4.1.0-1", "verified:a", "converged")
	l.upload("not-tested", "7.1.13-1-1-ARCH", "4.2.0-1", "verified:a", "converged")
	page := l.page("/matrix?group=model&state=regression")
	cell := summaryCell(t, page, "brightness", "converged")
	if cell.attr("data-state") != "regression" || cell.count(t, fmt.Sprintf(`a[href="/reports/%s"]`, failure)) != 1 || page.count(t, "tbody.matrix-history tr.matrix-row") != 3 {
		t.Errorf("the regression: %v; %d builds in the history", cell.attrs, page.count(t, "tbody.matrix-history tr.matrix-row"))
	}

	// Upload order decides the latest result even when a machine retests an older kernel.
	l = newPublicLab(t, nil)
	l.upload("works", newKernel, "4.0.0-1", "verified:a", "converged")
	l.upload("fails", oldKernel, "4.1.0-1", "verified:a", "converged")
	cell = summaryCell(t, l.page("/matrix?group=model"), "brightness", "converged")
	if cell.attr("data-state") != "regression" || !reflect.DeepEqual(kernelsOf(t, cell), []string{oldKernel}) {
		t.Errorf("after the retest: %v", cell.attrs)
	}
	page = l.page("/matrix?group=model&kernel=" + newKernel)
	cell = summaryCell(t, page, "brightness", "converged")
	if cell.attr("data-state") != "works" || !reflect.DeepEqual(kernelsOf(t, cell), []string{newKernel}) || page.count(t, "tbody.matrix-history tr.matrix-row") != 1 {
		t.Errorf("kernel=%s: %v", newKernel, cell.attrs)
	}
	if page.count(t, `.filter-chip[data-filter=kernel][href="/matrix?group=model"]`) != 1 ||
		page.count(t, fmt.Sprintf(`.filter-exports a[href="/api/v1/matrix.json?group=model&kernel=%s"]`, newKernel)) != 1 {
		t.Error("the kernel's chip or the export link is not as the old page's")
	}
	if !page.hasText(t, "select[name=kernel] option[selected]", newKernel) || !page.hasText(t, ".filter-chip[data-filter=kernel]", "kernel: "+newKernel+" ×") {
		t.Error("the picked kernel does not show")
	}
	// A kernel past the choices still shows as picked.
	if l.page("/matrix?group=build&kernel=unknown-kernel").count(t, "select[name=kernel] option[selected][value=unknown-kernel]") != 1 {
		t.Error("an unknown kernel is not shown as picked")
	}
}

func TestACarriedResultIdentifiesItsActualSourceKernelAndDate(t *testing.T) {
	l := newPublicLab(t, nil)
	earlier := l.upload("works", oldKernel, "4.0.0-1", "verified:a", "converged")
	l.at = l.at.Add(59 * time.Minute)
	l.upload("not-tested", newKernel, "4.0.0-1", "verified:a", "converged")
	cell := summaryCell(t, l.page("/matrix?group=model"), "brightness", "converged")
	if !reflect.DeepEqual(kernelsOf(t, cell), []string{oldKernel}) || cell.attr("data-tested-at") != "2026-10-04T12:00:00Z" ||
		!strings.Contains(cell.attr("title"), oldKernel) || !strings.Contains(cell.attr("title"), "2026-10-04 12:00 UTC") {
		t.Errorf("the carried cell: %v", cell.attrs)
	}
	link := cell.sole(t, "a")
	if link.attr("href") != "/reports/"+earlier || link.attr("aria-label") != cell.attr("title") {
		t.Errorf("its link: %v", link.attrs)
	}
}

func TestSkippedChecksConfirmedSummariesAndWhoMayClaimAResult(t *testing.T) {
	// Skipping one check retains its previous result when another check of the feature passes.
	l := newPublicLab(t, nil)
	l.add(goldenWith(t, "m2-max-image2", "gpu.driver", "fails", "gpu.vulkan", "works"), "a")
	later := goldenWith(t, "m2-max-image2", "gpu.driver", "not-tested", "gpu.vulkan", "works")
	later["machine"].(doc)["kernel"] = newKernel
	l.add(later, "a")
	gpu := summaryCell(t, l.page("/matrix?group=model"), "gpu", "converged")
	if gpu.attr("data-state") != "unconfirmed" || !strings.Contains(gpu.attr("class"), "cell-hint-partial") || !reflect.DeepEqual(kernelsOf(t, gpu), []string{newKernel, oldKernel}) {
		t.Errorf("gpu: %v", gpu.attrs)
	}

	// Confirmed summaries still show the unconfirmed runs that established agreement.
	l = newPublicLab(t, nil)
	l.upload("works", oldKernel, "4.0.0-1", "b", "converged")
	l.upload("works", newKernel, "4.0.0-1", "c", "converged")
	page := l.page("/matrix?group=model&confirmed=1")
	if summaryCell(t, page, "brightness", "converged").attr("data-state") != "works" || page.count(t, "tbody.matrix-history tr.matrix-row") != 2 ||
		page.count(t, "tbody.matrix-history td[data-feature=brightness][data-state=unconfirmed]") != 2 {
		t.Error("the confirmed summary lost its history")
	}

	// A newer Community result cannot claim the source of a result Verified runs decide.
	l = newPublicLab(t, nil)
	l.upload("works", oldKernel, "4.0.0-1", "verified:a", "converged")
	l.upload("fails", newKernel, "4.0.0-1", "b", "converged")
	cell := summaryCell(t, l.page("/matrix?group=model"), "brightness", "converged")
	if cell.attr("data-state") != "works" || cell.attr("data-verified") != "tester" || !reflect.DeepEqual(kernelsOf(t, cell), []string{oldKernel}) {
		t.Errorf("with a Verified run: %v", cell.attrs)
	}
	cell = summaryCell(t, l.page("/matrix?group=model&runs=community"), "brightness", "converged")
	if cell.attr("data-state") != "unconfirmed" || !reflect.DeepEqual(kernelsOf(t, cell), []string{newKernel}) {
		t.Errorf("runs=community: %v", cell.attrs)
	}

	// Different machines still disagree and different stacks have separate summaries.
	l = newPublicLab(t, nil)
	l.upload("works", oldKernel, "4.0.0-1", "verified:a", "converged")
	l.upload("fails", newKernel, "4.0.0-1", "verified:bb", "converged")
	l.upload("works", newKernel, "4.0.0-1", "verified:a", "mx-mac")
	page = l.page("/matrix?group=model")
	if page.count(t, "tr.model-summary") != 2 || summaryCell(t, page, "brightness", "converged").attr("data-state") != "partial" ||
		summaryCell(t, page, "brightness", "mx-mac").attr("data-state") != "works" {
		t.Error("two machines, two stacks: not two summaries, partial and works")
	}
}

// A human check answered yes by a bare Enter never colours the matrix.
func TestAnEnterOnlyYesColoursNoCell(t *testing.T) {
	const trackpad = "input.trackpad-gestures"
	yes := func(byDefault bool) doc {
		body := goldenWith(t, "m2-max-image2", trackpad, "works")
		delete(checkOf(t, body, trackpad), "answered_by_default")
		if byDefault {
			checkOf(t, body, trackpad)["answered_by_default"] = true
		}
		return body
	}
	touchpad := func(l *publicLab) *node {
		return l.page("/matrix").sole(t, `tr.model-summary[data-board="j416c"] td[data-feature="touchpad"]`)
	}
	l := newPublicLab(t, nil)
	l.add(yes(true), "a")
	l.add(yes(true), "b")
	if got := touchpad(l).attr("data-state"); got != "not-tested" {
		t.Errorf("Enter-only yeses from two machines: %q", got)
	}
	l.add(yes(false), "c")
	l.add(yes(false), "d")
	if got := touchpad(l).attr("data-state"); got != "works" {
		t.Errorf("typed yeses from two machines: %q", got)
	}
	l = newPublicLab(t, nil)
	l.add(yes(true), "verified:a")
	if cell := touchpad(l); cell.attr("data-state") != "not-tested" || cell.has["data-verified"] {
		t.Errorf("a Verified run's Enter-only yes: %v", cell.attrs)
	}
	l.add(yes(false), "verified:a")
	if cell := touchpad(l); cell.attr("data-state") != "works" || cell.attr("data-verified") != "tester" {
		t.Errorf("a Verified run's typed yes: %v", cell.attrs)
	}
}

func TestTwoMachinesAgreeingOnARegressionColourTheCellRed(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "verified:t")
	for _, machine := range []string{"a", "b"} {
		l.add(setPackage(t, goldenWith(t, "m2-max-image2", "gpu.driver", "fails", "gpu.vulkan", "fails"), "omarchy", "4.1.0-1"), machine)
	}
	page := l.page("/matrix")
	gpu := page.sole(t, `tr.matrix-row[data-board="j416c"][data-version="4.1.0"] td[data-feature="gpu"]`)
	if gpu.attr("data-state") != "regression" || !strings.Contains(gpu.attr("class"), "cell-regression") {
		t.Errorf("4.1.0: %v", gpu.attrs)
	}
	if got := page.sole(t, `tr.matrix-row[data-board="j416c"][data-version="4.0.0-alpha"] td[data-feature="gpu"]`).attr("data-state"); got != "works" {
		t.Errorf("4.0.0-alpha: %q", got)
	}
	if !page.says(t, ".legend li", "regression") || !page.says(t, ".legend li", "doesn't work") {
		t.Error("the legend does not tell regressions from failures")
	}
}

// The bring-up table on Models: every Mac, tested or not.
func lineupStatus(t *testing.T, page *node, board, area string) *node {
	t.Helper()
	return page.sole(t, fmt.Sprintf(`tr.lineup-row[data-boards~="%s"] td.lineup-status[data-area="%s"]`, board, area))
}

func stateAndSource(t *testing.T, page *node, board, area string) string {
	td := lineupStatus(t, page, board, area)
	return td.attr("data-state") + " " + td.attr("data-source")
}

func TestTheBringUpTableShowsEveryMacTestedOrNot(t *testing.T) {
	l := newPublicLab(t, nil)
	cat := l.s.catalogue
	page := l.page("/models")
	if got := page.count(t, "tr.lineup-row"); got != len(cat.LineupModels()) || got == 0 {
		t.Errorf("%d rows, want %d", got, len(cat.LineupModels()))
	}
	var boards []string
	for _, row := range page.all(t, "tr.lineup-row") {
		boards = append(boards, strings.Fields(row.attr("data-boards"))...)
	}
	sort.Strings(boards)
	want := append([]string(nil), cat.Boards().Keys()...)
	sort.Strings(want)
	if !reflect.DeepEqual(boards, want) {
		t.Errorf("the boards shown are not the catalogue's: %v", boards)
	}
	j504 := page.sole(t, `tr.lineup-row[data-boards~="j504"]`)
	if j504.attr("data-tested") != "false" || !strings.Contains(j504.sole(t, `td.lineup-testing div[data-board="j504"]`).words(), "needs testing") {
		t.Error("an untested Mac does not ask for testing")
	}
	for at, want := range map[string]string{"j616s gpu": "missing catalogue", "j416c gpu": "works catalogue", "j413 display": "works catalogue",
		"j700 gpu": "missing catalogue", "j514c gpu": "missing catalogue"} {
		board, area, _ := strings.Cut(at, " ")
		if got := stateAndSource(t, page, board, area); got != want {
			t.Errorf("%s: %q, want %q", at, got, want)
		}
	}
	if !strings.Contains(lineupStatus(t, page, "j704", "gpu").words(), "Asahi: not in Asahi's tables") ||
		!strings.Contains(lineupStatus(t, page, "j514c", "gpu").words(), "catalogue: not supported yet") {
		t.Error("the catalogue's words are missing")
	}

	// A tested board shows its runs and what they found; one Community machine is unconfirmed.
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	page = l.page("/models")
	row := page.sole(t, `tr.lineup-row[data-boards~="j416c"]`)
	if row.attr("data-tested") != "true" || !strings.Contains(row.sole(t, `td.lineup-testing div[data-board="j416c"]`).words(), "1 run") ||
		row.sole(t, `td[data-area="gpu"] .lineup-tested[data-board="j416c"]`).attr("data-state") != "works" ||
		!strings.Contains(row.sole(t, `td.lineup-testing div[data-board="j414c"]`).words(), "needs testing") {
		t.Errorf("the tested row: %s", row.words())
	}
	if stateAndSource(t, page, "j416c", "gpu") != "works runs" || !strings.Contains(lineupStatus(t, page, "j416c", "gpu").words(), "unconfirmed") {
		t.Error("one Community machine does not read as unconfirmed")
	}
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	if strings.Contains(lineupStatus(t, l.page("/models"), "j416c", "gpu").words(), "unconfirmed") {
		t.Error("two machines that agree still read as unconfirmed")
	}
}

func TestTheRunsDecideOnceAMacIsTestedOverTheCatalogue(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenWith(t, "m2-max-image2", "gpu.vulkan", "fails"), "verified:one")
	page := l.page("/models")
	if stateAndSource(t, page, "j416c", "gpu") != "partial runs" || stateAndSource(t, page, "j416c", "display") != "works runs" ||
		strings.Contains(lineupStatus(t, page, "j416c", "gpu").words(), "unconfirmed") {
		t.Errorf("gpu %q, display %q", stateAndSource(t, page, "j416c", "gpu"), stateAndSource(t, page, "j416c", "display"))
	}

	// A newer pass retires older failures on its own board, not on another.
	l = newPublicLab(t, nil)
	older := func() doc {
		return goldenWith(t, "m2-max-image2", "gpu.driver", "not-in-asahi", "gpu.vulkan", "not-in-asahi")
	}
	otherBoard := older()
	otherBoard["machine"].(doc)["board"] = "j414c"
	l.add(older(), "verified:one")
	l.add(otherBoard, "verified:three")
	l.add(goldenDoc(t, "m2-max-image2"), "verified:twoo")
	gpu := lineupStatus(t, l.page("/models"), "j416c", "gpu")
	if gpu.sole(t, `.lineup-tested[data-board="j416c"]`).attr("data-state") != "works" || gpu.sole(t, `.lineup-tested[data-board="j414c"]`).attr("data-state") != "missing" ||
		gpu.attr("data-state") != "partial" {
		t.Errorf("the row: %s", gpu.words())
	}
}

func TestTheModelsPageListsEachMacWithItsStacksAndReportCount(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "a")
	page := l.page("/models")
	for selector, words := range map[string]string{
		"tr#model-j416c td": "MacBook Pro (16-inch, M2 Max, 2023)",
		"tr#model-j314s td": "mx-mac 4.0.2-dev · build aae2586",
	} {
		if !page.hasText(t, selector, words) {
			t.Errorf("no %s says %q", selector, words)
		}
	}
	if !page.hasText(t, "tr#model-j416c td", "converged 4.0.0-alpha · build 99ace40.361571887310001 (linux-aurora 7.1.12.aurora2-2, omarchy-mac-boot 20260925-3)") {
		t.Error("the M2 Max's build is not named")
	}
}

func TestAModelPageShowsWhatItsChipIsExpectedToDoAndEachStacksResults(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	page := l.page("/models/j416c")
	if !page.hasText(t, "h1", "MacBook Pro (16-inch, M2 Max, 2023)") || !page.says(t, ".subtitle", "M2 Pro/Max/Ultra generation · SoC t6021 · board j416c") {
		t.Errorf("the heading: %q", page.sole(t, ".subtitle").words())
	}
	var expected []string
	for _, td := range page.all(t, "tr#feature-aop td.expected") {
		expected = append(expected, td.words())
	}
	if !reflect.DeepEqual(expected, []string{"–", "supported 7.1.12", "–"}) {
		t.Errorf("what is expected of aop: %q", expected)
	}
	label := "converged 4.0.0-alpha · build 99ace40.361571887310001 (linux-aurora 7.1.12.aurora2-2, omarchy-mac-boot 20260925-3)"
	head := page.sole(t, "th.config-head a")
	if !page.hasText(t, "tr#feature-gpu td.expected.expected-yes", "linux-asahi") || page.count(t, `tr#feature-gpu td[data-state="works"]`) != 1 ||
		head.attr("title") != label || head.attr("href") != "#configuration-1" || head.words() != "#1" || !page.hasText(t, ".configuration-key li#configuration-1", label) {
		t.Error("the features table is not as the old page's")
	}
	if page.count(t, "tr.report-row") != 2 {
		t.Errorf("%d reports listed", page.count(t, "tr.report-row"))
	}
	if page.count(t, "tr.report-row .badge-community") != 2 || !page.says(t, "tr.report-row td.nowrap", "37 pass 2 fail 2 gap 45 skip") ||
		page.count(t, `tr.report-row td.build-cell[data-build="99ace40.361571887310001"]`) != 2 {
		t.Errorf("the report list: %s", page.sole(t, "table.list tr.report-row td.build-cell").words())
	}

	// A model without visible reports is not found.
	if got := l.status("/models/j999"); got != http.StatusNotFound {
		t.Errorf("/models/j999: %d", got)
	}
	l.exec(`UPDATE reports SET hidden_at = created_at`)
	if got := l.status("/models/j416c"); got != http.StatusNotFound {
		t.Errorf("a model whose reports are all hidden: %d", got)
	}
}

func TestTheFeaturesPageKeepsCompactResultsWithAccessibleModelAndBuildDetails(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m2-max-converged"), "b")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "c")
	all, _, _ := testedFeatures(t)
	page := l.page("/features")
	if !page.hasText(t, "#layer-asahi .card-title", "asahi hardware") || page.count(t, "#layer-aurora tr#feature-usb4-displays") != 1 ||
		page.count(t, "#layer-omarchy tr#feature-notch-bar .chip-result.cell-unconfirmed") == 0 {
		t.Error("the three layers' cards are not as the old page's")
	}
	if page.count(t, "tr#feature-gpu .feature-result[data-board=j416c]") != 2 ||
		page.count(t, `tr#feature-gpu .feature-result[data-board=j416c] .chip-result[href="/models/j416c#feature-gpu"]`) != 2 ||
		!page.hasText(t, "tr#feature-gpu .feature-result[data-board=j416c] .cell-tooltip strong", "MacBook Pro (16-inch, M2 Max, 2023)") ||
		page.count(t, `tr#feature-gpu .feature-result[data-board=j314s] .chip-result[href="/models/j314s#feature-gpu"]`) != 1 || page.count(t, ".tested-model-name") != 0 {
		t.Error("the gpu row's result boxes are not as the old page's")
	}
	details := attrsOf(page.all(t, "tr#feature-gpu .feature-result[data-board=j416c] .chip-result"), "aria-label")
	if len(details) != 2 || details[0] == details[1] || !strings.Contains(details[0], "linux-aurora") || !strings.Contains(details[1], "linux-aurora") {
		t.Errorf("the boxes' details: %q", details)
	}
	if page.count(t, "tr.feature-row") != all {
		t.Errorf("%d feature rows, want %d", page.count(t, "tr.feature-row"), all)
	}
}

func TestAFeaturePageShowsPerChipExpectationsAndResultsPerMac(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "a")
	page := l.page("/features/first-boot-hardware-setup")
	if !page.hasText(t, "h1", "First-boot hardware setup") || page.count(t, "tr.result-row") != 2 || page.count(t, "tr.result-summary") != 2 {
		t.Errorf("%d rows, %d summaries", page.count(t, "tr.result-row"), page.count(t, "tr.result-summary"))
	}
	if page.count(t, `tr.result-row[data-board="j416c"] td[data-state="partial"]`) != 1 || !page.hasText(t, `tr.result-row[data-board="j416c"] .summary`, "2 machines partial") ||
		page.count(t, `tr.result-summary[data-board="j416c"] td[data-state="partial"]`) != 1 || page.count(t, `tr.result-row[data-board="j314s"] td[data-state="unconfirmed"]`) != 1 {
		t.Error("the results are not as the old page's")
	}
	var expected []string
	for _, td := range page.all(t, "tr#chip-m2-pro-max-ultra td.expected") {
		expected = append(expected, td.words())
	}
	if !reflect.DeepEqual(expected, []string{"–", "supported 7.1.12", "supported"}) || !page.hasText(t, "code", "setup.first-boot-hardware") {
		t.Errorf("what is expected: %q", expected)
	}

	// A feature no check covers says so; Touch ID lists its two checks; an unknown feature is not found.
	page = l.page("/features/prores-codec")
	if !page.says(t, ".card", "no check covers it") || page.count(t, "tr#chip-m1 td.expected-none") == 0 {
		t.Error("prores-codec does not say that no check covers it")
	}
	page = l.page("/features/touch-id")
	if !page.says(t, ".card", "touch-id.ready, touch-id.unlock") || !page.says(t, ".card", "No report has tested this yet") {
		t.Error("touch-id does not list its checks")
	}
	if got := l.status("/features/warp-drive"); got != http.StatusNotFound {
		t.Errorf("/features/warp-drive: %d", got)
	}
}

func count(t *testing.T, page *node) string {
	t.Helper()
	return page.sole(t, ".filter-count").attr("data-count")
}

func TestFeatureFiltersKeepTheCatalogueAndNarrowTheResultBoxesByModelAndBuild(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	all, _, _ := testedFeatures(t)
	page := l.page("/features?model=j313&build=" + m1AirBuild)
	boxes := page.all(t, ".feature-result")
	if got := attrsOf(page.all(t, "form.filter-bar select"), "name"); !reflect.DeepEqual(got, []string{"model", "build"}) || page.count(t, "form.filter-bar input") != 0 {
		t.Errorf("the form's fields: %v", got)
	}
	if page.count(t, "tr.feature-row") != all || len(boxes) == 0 || count(t, page) == "0" {
		t.Errorf("%d rows, %d boxes, %s runs", page.count(t, "tr.feature-row"), len(boxes), count(t, page))
	}
	for _, box := range boxes {
		if box.attr("data-board") != "j313" || box.attr("data-build") != m1AirBuild {
			t.Errorf("a box of %s on %s", box.attr("data-board"), box.attr("data-build"))
		}
	}
	for selector, href := range map[string]string{
		".filter-chip[data-filter=model]": "/features?build=" + m1AirBuild,
		".filter-chip[data-filter=build]": "/features?model=j313",
		".filter-clear":                   "/features",
		"tr#feature-gpu td a":             "/features/gpu?build=" + m1AirBuild + "&model=j313",
	} {
		if got := page.all(t, selector); len(got) == 0 || got[0].attr("href") != href {
			t.Errorf("%s does not lead to %s", selector, href)
		}
	}

	page = l.page("/features/gpu?model=j313&build=" + m1AirBuild)
	rows := page.all(t, "tr.result-row")
	for _, row := range rows {
		if row.attr("data-board") != "j313" || row.attr("data-build") != m1AirBuild {
			t.Errorf("a row of %s on %s", row.attr("data-board"), row.attr("data-build"))
		}
	}
	if len(rows) == 0 || page.count(t, `.filter-chip[data-filter=build][href="/features/gpu?model=j313"]`) != 1 || page.count(t, "select[name=model] option[value=j313][selected]") != 1 {
		t.Error("the feature's page does not keep the filters")
	}
	if page.count(t, fmt.Sprintf(`tr.result-row[data-board="j313"][data-build="%s"]`, m1AirBuild)) == 0 {
		t.Error("no row names the build")
	}

	// The feature pages take the model and the build, and no other filter.
	if only, more := l.page("/features?model=j313"), l.page("/features?model=j313&stack=mx-mac&tool=9.9.9&confirmed=1"); count(t, only) == "0" ||
		count(t, more) != count(t, only) || more.count(t, ".filter-chip") != 1 || more.count(t, ".feature-result") != only.count(t, ".feature-result") {
		t.Error("the feature catalogue takes a filter that is not its own")
	}

	// Build choices follow the model and distinguish variants of one runtime.
	page = l.page("/features?model=j416c")
	var builds []string
	for _, option := range page.all(t, "select[name=build] option") {
		if option.attr("value") != "" {
			builds = append(builds, option.attr("value"))
			if strings.Contains(option.attr("value"), "1937418.362376005140001") && !strings.HasPrefix(option.words(), option.attr("value")) {
				t.Errorf("a variant of one runtime reads %q", option.words())
			}
		}
	}
	if len(builds) < 2 || page.count(t, "select[name=model] option[value=j313]") != 1 {
		t.Errorf("the M2 Max's builds: %v", builds)
	}

	// Unknown filters show no results without hiding the catalogue.
	page = l.page("/features?model=unknown-model&build=unknown-build")
	if count(t, page) != "0" || page.count(t, "tr.feature-row") != all || page.count(t, ".feature-result") != 0 ||
		page.count(t, "select[name=model] option[value=unknown-model][selected]") != 1 || page.count(t, "select[name=build] option[value=unknown-build][selected]") != 1 ||
		!page.says(t, ".dim", "No reports match these filters yet") {
		t.Error("unknown filters are not shown as picked over an empty result")
	}
}

func TestTheMatrixTakesEveryRunFilterWithTheCountOfRunsBehindIt(t *testing.T) {
	l := newPublicLab(t, nil)
	ids := l.production()
	_, tested, byLayer := testedFeatures(t)
	boards := func(page *node) []string {
		seen := map[string]bool{}
		var list []string
		for _, board := range attrsOf(page.all(t, "tr.matrix-row"), "data-board") {
			if !seen[board] {
				seen[board] = true
				list = append(list, board)
			}
		}
		sort.Strings(list)
		return list
	}
	page := l.page("/matrix?group=build&gen=m1")
	if count(t, page) != "6" || len(boards(page)) == 0 {
		t.Errorf("gen=m1: %s runs", count(t, page))
	}
	page = l.page("/matrix?group=build&chip=M3&tool=0.1.8&to=2026-09-28")
	if count(t, page) != "1" || !reflect.DeepEqual(boards(page), []string{"j613"}) {
		t.Errorf("the M3 alone: %s runs on %v", count(t, page), boards(page))
	}
	page = l.page("/matrix?group=build&gen=m1&state=regression")
	if page.count(t, "tr.matrix-row") != 0 || count(t, page) != "0" {
		t.Error("state=regression shows rows")
	}
	page = l.page("/matrix?group=build&from=2026-09-29")
	if page.count(t, "tr.matrix-row") != 0 || !page.says(t, ".card .dim", "No reports match these filters yet") {
		t.Error("a day past the last run shows rows")
	}

	// The layer filter keeps one layer's columns.
	page = l.page("/matrix?group=build&layer=aurora")
	for _, th := range page.all(t, "th.feature-head") {
		if !strings.Contains(th.attr("class"), "layer-aurora") {
			t.Errorf("layer=aurora shows a column of %s", th.attr("class"))
		}
	}
	if page.count(t, "th.feature-head") != byLayer["aurora"] || !page.says(t, `.filter-chip[data-filter="layer"]`, "layer: Aurora addition") {
		t.Errorf("%d aurora columns, want %d", page.count(t, "th.feature-head"), byLayer["aurora"])
	}

	// The state filter keeps the rows and columns with a cell in that state.
	shown := func(td *node) string {
		if m := regexp.MustCompile(`cell-hint-([\w-]+)`).FindStringSubmatch(td.attr("class")); m != nil {
			return m[1]
		}
		return td.attr("data-state")
	}
	for state, wanted := range map[string]string{"works": "works", "fails": "fails", "gap": "missing", "untested": "not-tested"} {
		page = l.page("/matrix?group=build&state=" + state)
		rows := page.all(t, "tr.matrix-row")
		columns := page.count(t, "th.feature-head")
		found := make([]bool, columns)
		for _, row := range rows {
			inRow := false
			for i, td := range row.all(t, "td[data-feature]") {
				if shown(td) == wanted {
					inRow, found[i] = true, true
				}
			}
			if !inRow {
				t.Errorf("state=%s: a row without such a cell", state)
			}
		}
		for i, ok := range found {
			if !ok {
				t.Errorf("state=%s: column %d has no such cell", state, i)
			}
		}
		if len(rows) == 0 || columns > tested {
			t.Errorf("state=%s: %d rows, %d columns", state, len(rows), columns)
		}
	}

	// Confirmed only hides the cells Community machines don't agree on.
	page = l.page("/matrix?group=build")
	all := page.count(t, "tr.matrix-row")
	if page.count(t, `td[data-state="unconfirmed"]`) == 0 || !reflect.DeepEqual(boards(l.page("/matrix?group=build&model=j613")), []string{"j613"}) {
		t.Error("most production runs are the only machine on their build: want unconfirmed cells")
	}
	page = l.page("/matrix?group=build&confirmed=1")
	rows := page.all(t, "tr.matrix-row")
	sum := 0
	for _, meta := range page.all(t, "tr.matrix-row .row-meta") {
		if m := regexp.MustCompile(`(\d+) reports?`).FindStringSubmatch(meta.words()); m != nil {
			n := 0
			fmt.Sscan(m[1], &n)
			sum += n
		}
	}
	if page.count(t, `td[data-state="unconfirmed"]`) != 0 || page.count(t, `td[data-state="hidden"].cell-hidden`) == 0 || len(rows) >= all ||
		count(t, page) != fmt.Sprint(sum) || sum >= len(ids) || page.count(t, `tr.matrix-row[data-board="j613"]`) != 0 {
		t.Errorf("confirmed only: %d of %d rows, %s runs", len(rows), all, count(t, page))
	}
	// A Verified run colours cells on its own, so its row comes back.
	l.exec(`UPDATE reports SET verified = 1, handle = 'maralcbr', account_id = 5 WHERE public_id = ?`, m3ID)
	if l.page("/matrix?group=build&confirmed=1").count(t, `tr.matrix-row[data-board="j613"] td[data-verified="tester"]`) == 0 {
		t.Error("the M3's Verified run does not bring its row back")
	}
	if got := count(t, l.page("/matrix?group=build&runs=tester")); got != "1" {
		t.Errorf("runs=tester: %s runs", got)
	}
}

func TestTheOldMatrixLinksKeepWorkingAndKeepTheOtherFilters(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	page := l.page("/matrix?group=build&build=" + m1AirBuild)
	for _, build := range attrsOf(page.all(t, "tr.matrix-row"), "data-build") {
		if build != m1AirBuild {
			t.Errorf("build=%s shows %s", m1AirBuild, build)
		}
	}
	if page.count(t, `.filter-chip[data-filter="build"]`) != 1 || page.count(t, "tr.matrix-row") == 0 {
		t.Error("the build filter has no chip or no rows")
	}

	page = l.page("/matrix?group=release&gen=m2")
	for _, row := range page.all(t, "tr.matrix-row") {
		if row.attr("data-build") != "" || !row.has["data-build"] {
			t.Errorf("a release row names the build %q", row.attr("data-build"))
		}
	}
	for selector, want := range map[string]string{
		`form.filter-bar input[type="hidden"][name="group"][value="release"]`: "",
		`.filter-chip[href="/matrix?group=release"]`:                          "",
		`.filter-clear[href="/matrix?group=release"]`:                         "",
		`.matrix-view-switch a[href="/matrix?gen=m2&group=build"]`:            "Build",
		`.matrix-view-switch a[href="/matrix?gen=m2&group=model"]`:            "Model",
		`.matrix-view-switch a[aria-current=page]`:                            "Release",
		`.filter-exports a[href="/api/v1/matrix.json?gen=m2&group=release"]`:  "matrix.json",
		`.filter-exports a[href="/api/v1/checks.csv?gen=m2"]`:                 "checks.csv",
	} {
		if !page.hasText(t, selector, want) {
			t.Errorf("no %s saying %q", selector, want)
		}
	}
	if page.count(t, "tr.matrix-row .row-build") != 0 {
		t.Error("a release row shows a build")
	}
	page = l.page("/matrix?gen=m1&layer=aurora&group=release")
	if page.count(t, `.filter-exports a[href="/api/v1/matrix.json?gen=m1&group=release&layer=aurora"]`) != 1 ||
		page.count(t, `.filter-exports a[href="/api/v1/checks.csv?gen=m1&layer=aurora"]`) != 1 {
		t.Error("the exports are not linked with the same filters")
	}
}

func TestTheMatrixModelsAndFeaturesShowEachBuild(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	page := l.page("/matrix?group=build")
	m3 := page.sole(t, `tr.matrix-row[data-board="j613"]`)
	if m3.attr("data-build") != m3Build || !m3.hasText(t, ".row-build", "build "+m3Words) || page.count(t, fmt.Sprintf(`select[name=build] option[value="%s"]`, m3Words)) != 1 {
		t.Errorf("the M3's row: %v", m3.attrs)
	}
	seen := map[string]bool{}
	var m2Builds []string
	for _, row := range page.all(t, "tr.matrix-row") {
		key := row.attr("data-board") + " " + row.sole(t, ".row-build").words()
		if seen[key] {
			t.Errorf("two rows for %s", key)
		}
		seen[key] = true
		if row.attr("data-board") == m2Board {
			m2Builds = append(m2Builds, row.sole(t, ".row-build").words())
		}
	}
	sort.Strings(m2Builds)
	if want := []string{"build 1937418.362376005140001 (linux-aurora 7.1.12.aurora2-10, omarchy-mac-boot 20260926-1)",
		"build 1937418.362376005140001 (linux-aurora 7.1.12.aurora2-10.90, omarchy-mac-boot 20260926-1)"}; !reflect.DeepEqual(m2Builds, want) {
		t.Errorf("the M2 Max's builds: %q", m2Builds)
	}

	page = l.page("/models/j613")
	label := "converged 4.0.0-alpha · build " + m3Words
	if head := page.sole(t, "th.config-head a"); head.attr("title") != label || head.words() != "#1" || !page.hasText(t, ".configuration-key li#configuration-1", label) ||
		page.count(t, fmt.Sprintf(`tr.report-row td.build-cell[data-build="%s"]`, m3Build)) != 1 {
		t.Error("the M3's page does not name its build")
	}
	if link := page.sole(t, "tr#report-"+m3ID+" td.build-cell a.build-id"); link.attr("title") != m3Words || link.attr("href") != "/reports?build="+strings.NewReplacer(" ", "+", "(", "%28", ")", "%29", ",", "%2C").Replace(m3Words) {
		t.Errorf("the build's link: %v", link.attrs)
	}
	if l.page("/features/gpu").count(t, fmt.Sprintf(`tr.result-row[data-board="j613"][data-build="%s"]`, m3Build)) != 1 {
		t.Error("the gpu feature's page has no row for the M3's build")
	}
	// A run matched to a known set by its package says so, the set unlinked while no run names it.
	air := l.page("/models/j313")
	if !air.says(t, "tr.report-row td.build-cell", "set apple-test-0052582276d9-20260927") || air.count(t, `td.build-cell a[href^="/candidates/"]`) != 0 {
		t.Error("the M1 Air's runs do not show their matched set")
	}
}

func TestARunOnAnImageNamingItsSetIsOfferedByTheSetsName(t *testing.T) {
	const set = "apple-test-1937418f520b-20260926"
	l := newPublicLab(t, nil)
	body := goldenDoc(t, "m2-max-converged")
	body["system"].(doc)["candidate_set"] = set
	body["system"].(doc)["image"] = doc{"format": "1", "platform": "apple-silicon", "candidate_set": set, "package_set_sha256": strings.Repeat("ab", 32), "built": "2026-09-28T03:04:05Z"}
	l.add(body, "a")
	words := set + " (runtime 1937418.362376005140001, linux-aurora 7.1.12.aurora2-10, omarchy-mac-boot 20260926-1, built 2026-09-28T03:04:05Z, package set abababababab)"
	page := l.page("/matrix?group=build")
	if !page.hasText(t, fmt.Sprintf(`select[name=build] option[value="%s"]`, words), set+" (1)") {
		t.Error("the build is not offered by its set's name")
	}
	if l.page("/models/j416c").count(t, fmt.Sprintf(`tr.report-row td.build-cell a.build-id[title="%s"]`, words)) != 1 {
		t.Error("the report list does not name the build by its set")
	}
}

// The public pages need reports.db alone: they render with labd.db gone.
// laterPages are the public pages that came after the first four, on the
// production reports: each must do what those do.
var laterPages = []string{"/reports", "/reports?gen=m1&confirmed=1", "/reports/" + m3ID, "/gaps", "/aurora", "/benchmarks", "/candidates", "/data"}

func TestEachPublicPageRendersWithLabdDBAbsent(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	l.s.store.db.Close()
	gone, err := filepath.Glob(filepath.Join(l.s.cfg.DataDir, "labd.db*"))
	if err != nil || len(gone) == 0 {
		t.Fatalf("labd.db is not where the test looks: %v", err)
	}
	for _, file := range gone {
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range append([]string{"/", "/matrix", "/matrix?group=build", "/matrix?group=release&gen=m1", "/models", "/models/j613", "/features", "/features/gpu"}, laterPages...) {
		page := l.page(target)
		if page.count(t, "main") != 1 || page.count(t, "footer.site-footer") != 1 {
			t.Errorf("%s is not a whole page", target)
		}
	}
}

// The content security policy is host-based: the lab's own files and
// nothing written into a page. The old site's two scripts are files.
func TestThePublicPagesPolicyIsHostBasedAndTheyCarryNothingInline(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	l.exec(`UPDATE reports SET verified = 1, handle = 'maralcbr', account_id = 5 WHERE public_id = ?`, m3ID)
	for _, target := range append([]string{"/", "/matrix", "/matrix?group=build", "/models", "/models/j613", "/features", "/features/gpu"}, laterPages...) {
		w := l.do("GET", target, "")
		csp := w.Header().Get("Content-Security-Policy")
		if csp != web.CSP || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") {
			t.Errorf("%s: policy %q", target, csp)
		}
		for _, never := range []string{"nonce", "unsafe-inline", "unsafe-eval"} {
			if strings.Contains(csp, never) {
				t.Errorf("%s: the policy has %s", target, never)
			}
		}
		body := w.Body.String()
		for _, never := range []string{"<style", " style=", " onclick=", "javascript:"} {
			if strings.Contains(body, never) {
				t.Errorf("%s carries %q", target, never)
			}
		}
		page := parseHTML(t, body)
		for _, script := range page.all(t, "script") {
			if !strings.HasPrefix(script.attr("src"), "/assets/") || script.text() != "" {
				t.Errorf("%s: a script that is not one of the lab's files: %v", target, script.attrs)
			}
		}
		// Pages show Verified or Community only: nobody's handle.
		if strings.Contains(body, "maralcbr") || strings.Contains(body, "key:machine-") {
			t.Errorf("%s names a person or a Machine key", target)
		}
		if w.Header().Get("Set-Cookie") != "" {
			t.Errorf("%s sets a cookie", target)
		}
	}
	page := l.page("/models/j613")
	if !page.hasText(t, "tr.report-row .badge-tester", "verified") {
		t.Error("the M3's Verified run is not marked on its model's page")
	}
	if got := l.page("/models").sole(t, `td.lineup-testing div[data-board="j613"]`).words(); !strings.Contains(got, "1 run, 1 verified") {
		t.Errorf("the bring-up table says %q", got)
	}
	for _, words := range []string{"tester run", "a tester", "Tester"} {
		for _, target := range append([]string{"/matrix?group=build", "/models", "/models/j613", "/features"}, laterPages...) {
			if strings.Contains(l.do("GET", target, "").Body.String(), words) {
				t.Errorf("%s still says %q", target, words)
			}
		}
	}
}

func TestInPreviewThePublicPagesCarryTheBanner(t *testing.T) {
	l := newPublicLab(t, func(c *Config) { c.Preview = true })
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	for _, target := range []string{"/", "/matrix", "/models", "/models/j416c", "/features", "/features/gpu", "/reports", "/gaps", "/aurora", "/benchmarks", "/candidates", "/data"} {
		if l.page(target).count(t, "p.preview-banner") != 1 {
			t.Errorf("%s has no Preview banner", target)
		}
	}
}
