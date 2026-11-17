package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/reports"
)

// The reports list, a report's page and the exports, translated from the
// old site's tests: what pages_test.rb, build_identity_test.rb,
// filters_test.rb, regressions_test.rb and answered_by_default_test.rb ask
// of them, and exports_test.rb and report_inventory_test.rb whole.

type produced struct {
	ID         string `json:"id"`
	UploadedAt string `json:"uploaded_at"`
	Report     doc    `json:"report"`
}

func productionReports(t *testing.T) []produced {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "production", "reports.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Reports []produced `json:"reports"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Reports
}

// idsWhere are the production reports' ids a test picks, sorted.
func idsWhere(list []produced, pick func(report doc, entry produced) bool) []string {
	ids := []string{}
	for _, entry := range list {
		if pick(entry.Report, entry) {
			ids = append(ids, entry.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func at(body doc, path ...string) any {
	var value any = body
	for _, key := range path {
		object, _ := value.(doc)
		value = object[key]
	}
	return value
}

func sorted(list []string) []string {
	out := append([]string{}, list...)
	sort.Strings(out)
	return out
}

func (l *publicLab) get(target string) (int, string, http.Header) {
	l.t.Helper()
	w := l.do("GET", target, "")
	return w.Code, w.Body.String(), w.Header()
}

// export is an export's body; it must be there.
func (l *publicLab) export(target string) string {
	l.t.Helper()
	code, body, _ := l.get(target)
	if code != http.StatusOK {
		l.t.Fatalf("GET %s: %d %s", target, code, body)
	}
	return body
}

func (l *publicLab) exported(target string) doc {
	l.t.Helper()
	var body doc
	if err := json.Unmarshal([]byte(l.export(target)), &body); err != nil {
		l.t.Fatalf("GET %s: %v", target, err)
	}
	return body
}

var reCSVField = regexp.MustCompile(`"((?:[^"]|"")*)"`)

// parseCSV reads an export's rows: every field quoted, quotes doubled, CRLF ends.
func parseCSV(t *testing.T, text string) [][]string {
	t.Helper()
	var rows [][]string
	if !strings.HasSuffix(text, "\r\n") {
		t.Fatalf("the CSV does not end its last line: %q", text[max(0, len(text)-40):])
	}
	for _, line := range strings.Split(strings.TrimSuffix(text, "\r\n"), "\r\n") {
		var row []string
		for _, field := range reCSVField.FindAllStringSubmatch(line, -1) {
			row = append(row, strings.ReplaceAll(field[1], `""`, `"`))
		}
		rows = append(rows, row)
	}
	return rows
}

// byColumn are a CSV's rows, each by its column names.
func byColumn(rows [][]string) []map[string]string {
	var out []map[string]string
	for _, row := range rows[1:] {
		entry := map[string]string{}
		for i, name := range rows[0] {
			entry[name] = row[i]
		}
		out = append(out, entry)
	}
	return out
}

func column(rows [][]string, name string) []string {
	var out []string
	for _, row := range byColumn(rows) {
		out = append(out, row[name])
	}
	return out
}

func uniqSorted(list []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, item := range list {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

func rowsOf(body doc, key string) []doc {
	var out []doc
	list, _ := body[key].([]any)
	for _, item := range list {
		out = append(out, item.(doc))
	}
	return out
}

func keysInOrder(t *testing.T, text string) []string {
	t.Helper()
	value, err := reports.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*reports.Object).Keys()
}

// listed are the reports list's rows under a query, as public ids, in the page's order.
func (l *publicLab) listed(query string) ([]string, *node) {
	l.t.Helper()
	page := l.page("/reports" + query)
	ids := []string{}
	for _, id := range attrsOf(page.all(l.t, "tr.report-row"), "id") {
		ids = append(ids, strings.TrimPrefix(id, "report-"))
	}
	return ids, page
}

func countOf(t *testing.T, page *node) int {
	t.Helper()
	n, err := strconv.Atoi(page.sole(t, ".filter-count").attr("data-count"))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func same(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n  got  %v\n  want %v", what, got, want)
	}
}

func (l *publicLab) verify(ids ...string) {
	l.t.Helper()
	for _, id := range ids {
		l.exec(`UPDATE reports SET verified = 1, handle = 'maralcbr', account_id = 7001 WHERE public_id = ?`, id)
	}
}

func (l *publicLab) hideReport(id string) {
	l.t.Helper()
	l.exec(`UPDATE reports SET hidden_at = ? WHERE public_id = ?`, time.Now().UnixMicro(), id)
}

func TestTheReportsPageListsEveryPublishedReportNewestFirst(t *testing.T) {
	l := newPublicLab(t, nil)
	first := l.add(goldenDoc(t, "m1-pro-mx-mac"), "a")
	second := l.add(goldenDoc(t, "m2-max-image2"), "a")
	ids, page := l.listed("")
	same(t, "the rows", ids, []string{second, first})
	if !page.hasText(t, "tr#report-"+second+" td", "converged 4.0.0-alpha") {
		t.Error("the row lacks its stack and version")
	}
	// hardware.drivers and hardware.probe-errors fail where the catalogue expects it: not yet supported, not failures.
	if !page.says(t, "tr#report-"+second+" td.nowrap", "37 pass 2 fail 2 gap 45 skip") {
		t.Errorf("the counts: %q", page.all(t, "tr#report-"+second+" td.nowrap")[1].words())
	}
	if got := page.sole(t, "tr#report-"+second+" td.build-cell").attr("data-build"); got != "99ace40.361571887310001" {
		t.Errorf("data-build %q", got)
	}
	if page.count(t, `tr#report-`+first+` td.build-cell a[href="/reports?build=aae2586"]`) != 1 {
		t.Error("the build does not link the reports on it")
	}
	if page.count(t, "tr.report-row .badge-community") != 2 || page.count(t, ".site-nav a.current") != 1 || !page.hasText(t, ".site-nav a.current", "Reports") {
		t.Error("the badges or the current page are wrong")
	}
	if !page.hasText(t, ".card-title", "2 reports") {
		t.Error("the card does not count the reports")
	}
	l.hideReport(first)
	if ids, _ := l.listed(""); !reflect.DeepEqual(ids, []string{second}) {
		t.Errorf("with one hidden: %v", ids)
	}
}

func TestEveryOldReportShowsTheBuildItWasOnAsTheCLIDerivesIt(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	data, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "production", "builds.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Builds map[string]struct{ Build, Words string } `json:"builds"`
	}
	if err := json.Unmarshal(data, &golden); err != nil || len(golden.Builds) != 11 {
		t.Fatalf("builds.json: %d, %v", len(golden.Builds), err)
	}
	_, page := l.listed("")
	for id, build := range golden.Builds {
		if got := page.sole(t, "tr#report-"+id+" td.build-cell").attr("data-build"); got != build.Build {
			t.Errorf("%s: data-build %q, want %q", id, got, build.Build)
		}
		if got := page.sole(t, "tr#report-"+id+" td.build-cell a.build-id").attr("title"); got != build.Words {
			t.Errorf("%s: words %q, want %q", id, got, build.Words)
		}
	}
	var third []string
	for _, row := range page.all(t, "tr.report-row") {
		third = append(third, row.all(t, "td")[2].words())
	}
	same(t, "the stack and version of every row", uniqSorted(third), []string{"converged 4.0.0-alpha"})
	ids, _ := l.listed("?build=" + m3Build)
	same(t, "?build=", ids, []string{m3ID})
}

func TestEveryRunFilterNarrowsTheReportsListAndTheCountSaysHowManyMatch(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	all := productionReports(t)
	listed := func(query string) ([]string, *node) {
		ids, page := l.listed(query)
		return sorted(ids), page
	}
	ids, page := listed("")
	if len(ids) != len(all) || countOf(t, page) != len(all) || page.count(t, ".filter-chip") != 0 || page.count(t, ".filter-clear") != 0 {
		t.Errorf("unfiltered: %d rows, count %d", len(ids), countOf(t, page))
	}
	chip := func(r doc) string { return at(r, "machine", "chip").(string) }
	ids, page = listed("?chip=M1+Pro")
	same(t, "chip", ids, idsWhere(all, func(r doc, _ produced) bool { return chip(r) == "M1 Pro" }))
	same(t, "chip count", countOf(t, page), 4)
	ids, page = listed("?gen=m1")
	same(t, "gen", ids, idsWhere(all, func(r doc, _ produced) bool { return strings.HasPrefix(chip(r), "M1") }))
	same(t, "gen count", countOf(t, page), 6)
	ids, _ = listed("?soc=t6021")
	same(t, "soc", ids, idsWhere(all, func(r doc, _ produced) bool { return at(r, "machine", "soc") == "t6021" }))
	ids, _ = listed("?model=j613")
	same(t, "model", ids, idsWhere(all, func(r doc, _ produced) bool { return at(r, "machine", "board") == "j613" }))
	ids, _ = listed("?gen=M3")
	same(t, "gen=M3", ids, []string{m3ID})
	ids, page = listed("?tool=0.1.8")
	same(t, "tool", ids, idsWhere(all, func(r doc, _ produced) bool { return at(r, "tool", "version") == "0.1.8" }))
	same(t, "tool count", countOf(t, page), 4)
	ids, _ = listed("?stack=converged")
	same(t, "stack", ids, idsWhere(all, func(r doc, _ produced) bool { return at(r, "system", "stack") == "converged" }))
	ids, _ = listed("?stack=mx-mac")
	same(t, "stack=mx-mac", ids, []string{})
	if ids, _ = listed("?release=4.0.0-alpha"); len(ids) != len(all) {
		t.Errorf("release: %d", len(ids))
	}
	ids, page = listed("?release=4.0.0")
	same(t, "release=4.0.0", ids, []string{})
	if !page.says(t, ".card .dim", "No reports match these filters yet") {
		t.Error("an empty list does not say nothing matches")
	}
	byBuild, _ := listed("?build=" + m1AirBuild)
	bySet, page := listed("?set=apple-test-0052582276d9-20260927")
	same(t, "set", bySet, byBuild)
	same(t, "set count", countOf(t, page), 4)

	// The upload date range takes whole UTC days, each end included.
	ids, page = listed("?from=2026-09-27&to=2026-09-27")
	same(t, "one day", ids, idsWhere(all, func(_ doc, e produced) bool { return strings.HasPrefix(e.UploadedAt, "2026-09-27") }))
	same(t, "one day's count", countOf(t, page), 7)
	ids, _ = listed("?from=2026-09-28")
	same(t, "from", ids, []string{m3ID})
	ids, _ = listed("?to=2026-09-26")
	same(t, "to", ids, idsWhere(all, func(_ doc, e produced) bool { return e.UploadedAt < "2026-09-27" }))
	ids, page = listed("?from=yesterday&to=2026-02-31")
	if len(ids) != len(all) || page.count(t, ".filter-chip") != 0 {
		t.Errorf("malformed days: %d rows, %d chips", len(ids), page.count(t, ".filter-chip"))
	}
}

func TestVerifiedRunsOnlyOrCommunityRunsOnly(t *testing.T) {
	l := newPublicLab(t, nil)
	all := l.production()
	var other string
	for _, id := range all {
		if strings.HasPrefix(id, "Gbu7hc") {
			other = id
		}
	}
	l.verify(m3ID, other)
	ids, page := l.listed("?runs=tester")
	if len(ids) != 2 || page.count(t, "tr.report-row .badge-tester") != 2 {
		t.Errorf("runs=tester: %d rows, %d badges", len(ids), page.count(t, "tr.report-row .badge-tester"))
	}
	if !page.hasText(t, "tr.report-row .badge-tester", "verified") {
		t.Error("the badge does not say verified")
	}
	ids, page = l.listed("?runs=community")
	if len(ids) != len(all)-2 || page.count(t, "tr.report-row .badge-tester") != 0 {
		t.Errorf("runs=community: %d rows", len(ids))
	}
	if ids, _ = l.listed("?runs=all"); len(ids) != len(all) {
		t.Errorf("runs=all: %d rows", len(ids))
	}
	// Nobody is named: not on the page, not in an export.
	for _, target := range []string{"/reports", "/reports/" + m3ID, "/api/v1/reports.json", "/api/v1/reports.csv", "/api/v1/checks.csv", "/api/v1/matrix.json",
		"/api/v1/benchmarks.json", "/api/v1/aurora.json", "/gaps", "/aurora", "/benchmarks", "/candidates", "/data"} {
		if body := l.export(target); strings.Contains(body, "maralcbr") || strings.Contains(body, "7001") || strings.Contains(body, "key:machine-") {
			t.Errorf("%s names a person or a Machine key", target)
		}
	}
}

func TestTheKernelFilterNarrowsPagesAndExportsAndKeepsUnknownChoices(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	all := productionReports(t)
	expected := idsWhere(all, func(r doc, _ produced) bool { return at(r, "machine", "kernel") == newKernel })
	ids, page := l.listed("?kernel=" + newKernel)
	same(t, "the list", sorted(ids), expected)
	if page.count(t, `select[name="kernel"] option[selected][value="`+newKernel+`"]`) != 1 || !page.hasText(t, `.filter-chip[data-filter="kernel"]`, "kernel: "+newKernel+" ×") {
		t.Error("the picked kernel is not shown")
	}
	same(t, "the matrix's count", countOf(t, l.page("/matrix?group=build&kernel="+newKernel)), len(expected))
	body := l.exported("/api/v1/reports.json?kernel=" + newKernel)
	same(t, "reports.json's filters", body["filters"], any(doc{"kernel": newKernel}))
	var got []string
	for _, report := range rowsOf(body, "reports") {
		got = append(got, report["id"].(string))
	}
	same(t, "reports.json", sorted(got), expected)
	same(t, "checks.csv", uniqSorted(column(parseCSV(t, l.export("/api/v1/checks.csv?kernel="+newKernel)), "report_id")), expected)
	same(t, "matrix.json's runs", l.exported("/api/v1/matrix.json?kernel=" + newKernel)["runs"], any(float64(len(expected))))
	ids, page = l.listed("?kernel=unknown-kernel")
	if len(ids) != 0 || page.count(t, `select[name="kernel"] option[selected][value="unknown-kernel"]`) != 1 {
		t.Error("an unknown kernel is not kept as picked")
	}

	// Kernel choices sort version numbers naturally.
	other := productionReports(t)[0].Report
	delete(other, "signature")
	other["machine"].(doc)["kernel"] = "7.1.12-2-100-ARCH"
	added := l.add(other, "z")
	_, page = l.listed("")
	choices := attrsOf(page.all(t, `select[name="kernel"] option`), "value")[1:]
	index := func(kernel string) int {
		for i, choice := range choices {
			if choice == kernel {
				return i
			}
		}
		return -1
	}
	if choices[0] != "7.1.12-2-100-ARCH" || index("7.1.12-2-11-ARCH") > index("7.1.12-2-10-ARCH") {
		t.Errorf("the kernels: %v", choices)
	}
	ids, _ = l.listed("?kernel=7.1.12-2-100-ARCH")
	same(t, "the new kernel", ids, []string{added})
}

func TestFiltersCombineShowAsRemovableChipsAndClearGoesBackToEverything(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	all := productionReports(t)
	ids, page := l.listed("?gen=m1&tool=0.1.8&from=2026-09-27")
	same(t, "the three filters", sorted(ids), idsWhere(all, func(r doc, e produced) bool {
		return strings.HasPrefix(at(r, "machine", "chip").(string), "M1") && at(r, "tool", "version") == "0.1.8" && e.UploadedAt >= "2026-09-27"
	}))
	same(t, "the count", countOf(t, page), 2)
	chips := page.all(t, ".filter-chip")
	same(t, "the chips", attrsOf(chips, "data-filter"), []string{"gen", "tool", "from"})
	if !strings.Contains(chips[0].words(), "generation: M1") || !strings.Contains(chips[1].words(), "tool version: 0.1.8") {
		t.Errorf("the chips say %q, %q", chips[0].words(), chips[1].words())
	}
	same(t, "the first chip's link", chips[0].attr("href"), "/reports?from=2026-09-27&tool=0.1.8")
	for _, selector := range []string{`.filter-clear[href="/reports"]`, `form.filter-bar[method="get"][action="/reports"]`, `select[name="gen"] option[selected][value="m1"]`,
		`select[name="tool"] option[selected][value="0.1.8"]`, `input[name="from"][value="2026-09-27"]`} {
		if page.count(t, selector) != 1 {
			t.Errorf("no %s", selector)
		}
	}
	ids, page = l.listed("?gen=m1&chip=M2+Max")
	if len(ids) != 0 || countOf(t, page) != 0 {
		t.Errorf("filters that exclude each other: %v", ids)
	}

	// A build filter as long as an image's build words is kept.
	ids, page = l.listed("?build=" + url.QueryEscape("apple-test-1937418f520b-20260926 ("+strings.Repeat("x", 400)+")"))
	if len(ids) != 0 || page.count(t, `.filter-chip[data-filter="build"]`) != 1 {
		t.Error("a long build filter is dropped")
	}

	// An unknown value matches nothing, and a value past the choices still shows as picked.
	ids, page = l.listed("?model=j999")
	if len(ids) != 0 || page.count(t, `select[name="model"] option[selected][value="j999"]`) != 1 {
		t.Error("an unknown model")
	}
	if ids, _ = l.listed("?tool=9.9.9"); len(ids) != 0 {
		t.Error("an unknown tool version")
	}
	ids, page = l.listed("?stack=bogus&runs=someone&gen=m9&confirmed=yes")
	if len(ids) != len(all) || page.count(t, ".filter-chip") != 0 {
		t.Errorf("malformed values: %d rows", len(ids))
	}

	// The page links the exports with the same filters.
	_, page = l.listed("?gen=m1")
	for _, href := range []string{"/api/v1/reports.json?gen=m1", "/api/v1/reports.csv?gen=m1", "/api/v1/checks.csv?gen=m1"} {
		if page.count(t, `.filter-exports a[href="`+href+`"]`) != 1 {
			t.Errorf("no export link %s", href)
		}
	}
}

var rePass = regexp.MustCompile(`(\d+) pass`)

func TestConfirmedOnlyLeavesTheAnswersGivenByABareEnterOutOfTheCounts(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	var m3 doc
	for _, entry := range productionReports(t) {
		if entry.ID == m3ID {
			m3 = entry.Report
		}
	}
	byDefault := 0
	for _, check := range m3["checks"].([]any) {
		if check.(doc)["answered_by_default"] == true {
			byDefault++
		}
	}
	passes := func(page *node) int {
		cells := page.all(t, "tr#report-"+m3ID+" td.nowrap")
		n, _ := strconv.Atoi(rePass.FindStringSubmatch(cells[len(cells)-1].words())[1])
		return n
	}
	_, page := l.listed("")
	all := passes(page)
	ids, page := l.listed("?confirmed=1")
	if len(ids) != 11 || !page.says(t, `.filter-chip[data-filter="confirmed"]`, "results: confirmed only") {
		t.Error("confirmed only narrows the list or shows no chip")
	}
	if confirmed := passes(page); byDefault == 0 || all-confirmed != byDefault {
		t.Errorf("%d passes, %d confirmed, %d answered by default", all, confirmed, byDefault)
	}
}

func TestTheReportPageReadsLikeTerminalOutputWithItsBuildAndBadge(t *testing.T) {
	l := newPublicLab(t, nil)
	id := l.add(goldenDoc(t, "m2-max-image2"), "a")
	page := l.page("/reports/" + id)
	if !page.says(t, ".terminal .terminal-bar", "omarchy-m-test") || !page.hasText(t, ".terminal .badge-community", "Community run") {
		t.Error("the terminal bar or the badge")
	}
	// One title per check-id prefix, plus the hardware inventory.
	sections := strings.Fields("system boot packages setup hardware gpu video display audio network sleep input sep touch-id camera ports power cpu thermal sensors storage graphics engines benchmark")
	if got := page.count(t, "h2.section-title"); got != len(sections)+1 {
		t.Errorf("%d section titles, want %d", got, len(sections)+1)
	}
	for _, check := range []string{"sep.attach", "touch-id.ready", "touch-id.unlock"} {
		if !page.hasText(t, `li[id="check-`+check+`"] .status.status-skip`, "SKIP") {
			t.Errorf("%s is not SKIP", check)
		}
	}
	if !page.hasText(t, "dd", "converged 4.0.0-alpha") || !page.hasText(t, "dd", "omarchy, limine, encryption on") {
		t.Error("the stack or the system line")
	}
	if !page.hasText(t, `li[id="check-setup.first-boot-hardware"] .status.status-fail`, "FAIL") ||
		!page.hasText(t, `li[id="check-setup.first-boot-hardware"] .badge-fails`, "doesn't work, but should on this Mac") {
		t.Error("the failing check")
	}
	if !page.hasText(t, ".packages summary", "packages (13)") {
		t.Error("the packages")
	}
	if page.count(t, `.credit a[href="/models/j416c"]`) != 1 {
		t.Error("no link to the Mac's page")
	}
	l.verify(id)
	if !l.page("/reports/"+id).hasText(t, ".terminal .badge-tester", "Verified run") {
		t.Error("a Verified run's badge")
	}
}

func TestTheReportPageHeaderNamesTheBuildItsKernelBootPackageAndTool(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	page := l.page("/reports/" + m3ID)
	if !page.hasText(t, "dd#build .build-id", m3Build) || !page.says(t, "dd#build", "linux-aurora 7.1.12.aurora2-11 · omarchy-mac-boot 20260926-1 · tool 0.1.8") {
		t.Errorf("the build: %q", page.sole(t, "dd#build").words())
	}
	if page.count(t, "details#packages[open] tr") != 13 || !page.hasText(t, "details#packages tr td", "4.0.0.alpha.quattro.r1790450903.gf60e1ba4c476-1.2026092602") {
		t.Error("the packages")
	}
	if page.count(t, "dd#image") != 0 {
		t.Error("an image record on a report without one")
	}
	// The M3's GPU failures read as not yet supported, not as failures.
	for _, check := range []string{"gpu.driver", "gpu.opengl", "benchmark.opengl", "benchmark.vulkan"} {
		if !page.hasText(t, `li[id="check-`+check+`"] .status.status-gap`, "GAP") || !page.hasText(t, `li[id="check-`+check+`"] .badge-missing`, "not yet supported by Asahi") {
			t.Errorf("%s does not read as a gap", check)
		}
	}
	if !regexp.MustCompile(`\A\d+ gap\z`).MatchString(page.sole(t, "dd .gap").words()) {
		t.Errorf("the gap count: %q", page.sole(t, "dd .gap").words())
	}
}

const imageSet = "apple-test-1937418f520b-20260926"

func imageRecord() doc {
	return doc{"format": "1", "platform": "apple-silicon", "candidate_set": imageSet,
		"candidate_source_commit": "1937418f520b" + strings.Repeat("0", 28), "builder_commit": "c6fedc32c" + strings.Repeat("1", 31),
		"builder_tree_clean": "true", "image_profile": "test", "package_set_sha256": strings.Repeat("ab", 32), "built": "2026-09-28T03:04:05Z"}
}

func onNewImage(t *testing.T, image doc) doc {
	body := goldenDoc(t, "m2-max-converged")
	body["system"].(doc)["candidate_set"] = imageSet
	body["system"].(doc)["image"] = image
	return body
}

func TestARunOnAnImageNamingItsSetShowsTheImageRecordAndCountsForTheSet(t *testing.T) {
	l := newPublicLab(t, nil)
	id := l.add(onNewImage(t, imageRecord()), "a")
	words := imageSet + " (runtime 1937418.362376005140001, linux-aurora 7.1.12.aurora2-10, omarchy-mac-boot 20260926-1, built 2026-09-28T03:04:05Z, package set abababababab)"
	page := l.page("/reports/" + id)
	if !page.hasText(t, "dd#build .build-id", imageSet) || page.count(t, `dd#build a[href="/candidates/`+imageSet+`"]`) == 0 {
		t.Error("the build is not labelled by its set, or does not link it")
	}
	if got := page.sole(t, "dd#build .build-id").attr("title"); got != words {
		t.Errorf("the build's words %q", got)
	}
	if !page.says(t, "dd#build", "runtime 1937418.362376005140001 · linux-aurora 7.1.12.aurora2-10 · omarchy-mac-boot 20260926-1 · built 2026-09-28T03:04:05Z · tool") {
		t.Errorf("the build: %q", page.sole(t, "dd#build").words())
	}
	if want := "format 1, platform apple-silicon, candidate_set " + imageSet + ", candidate_source_commit 1937418f520b, builder_commit c6fedc32c111, " +
		"builder_tree_clean true, image_profile test, package_set_sha256 abababababab, built 2026-09-28T03:04:05Z"; page.sole(t, "dd#image").words() != want {
		t.Errorf("the image: %q", page.sole(t, "dd#image").words())
	}
	if !page.says(t, "dd", "candidate set "+imageSet) {
		t.Error("the stack line does not name the set")
	}
	candidates := l.page("/candidates")
	if !candidates.hasText(t, `tr.build-row[data-set-source="image"] a.build-id`, imageSet) || candidates.count(t, `tr[id="candidate-`+imageSet+`"]`) != 1 {
		t.Error("the candidates page does not count the run for its set")
	}

	// Two images of one set are two builds: the image's package set and build time tell them apart.
	later, other := imageRecord(), imageRecord()
	later["built"], other["package_set_sha256"] = "2026-09-29T01:00:00Z", strings.Repeat("cd", 32)
	second, third := l.add(onNewImage(t, later), "b"), l.add(onNewImage(t, other), "c")
	if got := l.page("/candidates").count(t, `tr.build-row[data-set="`+imageSet+`"]`); got != 3 {
		t.Errorf("%d builds of the set, want 3", got)
	}
	p, err := l.s.reports.Published(l.s.catalogue)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{id, second, third} {
		ids, _ := l.listed("?build=" + url.QueryEscape(p.Run(id).Build().Words()))
		same(t, "the runs on one build", ids, []string{id})
	}
	if ids, _ := l.listed("?build=" + imageSet); len(ids) != 3 {
		t.Errorf("?build=<set>: %v", ids)
	}
}

func TestARegressionOnAReportLinksTheVerifiedPass(t *testing.T) {
	l := newPublicLab(t, nil)
	gpuFails := []string{"gpu.driver", "fails", "gpu.vulkan", "fails"}
	badges := func(id string) []*node { return l.page("/reports/"+id).all(t, `li[id="check-gpu.driver"] .badge`) }
	regression := func(id string) *node {
		for _, badge := range badges(id) {
			if badge.words() == "regression" {
				return badge
			}
		}
		return nil
	}
	gpu := func() []string {
		var states []string
		for _, row := range rowsOf(l.exported("/api/v1/matrix.json"), "rows") {
			if row["board"] == m2Board {
				states = append(states, at(row, "cells", "gpu", "tentative").(string))
			}
		}
		return states
	}

	// A failure with no Verified earlier pass is not a regression.
	failing := l.add(goldenWith(t, "m2-max-image2", gpuFails...), "a")
	if got := badges(failing); len(got) != 1 || got[0].words() != "doesn't work, but should on this Mac" {
		t.Errorf("the badges of a plain failure: %d", len(got))
	}
	same(t, "matrix.json", gpu(), []string{"fails"})
	gaps := l.page("/gaps")
	if gaps.count(t, "#regressions tr.regression-row") != 0 || !gaps.says(t, "#regressions", "None open") {
		t.Error("a regression is open with no Verified pass")
	}

	// A Community pass does not make it one; a Verified pass before it does, a hidden one does not.
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	second := l.add(goldenWith(t, "m2-max-image2", gpuFails...), "a")
	if regression(second) != nil {
		t.Error("a Community pass made a regression")
	}
	pass := l.add(goldenDoc(t, "m2-max-image2"), "verified:t")
	later := l.add(setPackage(t, goldenWith(t, "m2-max-image2", gpuFails...), "omarchy", "4.1.0-1"), "a")
	badge := regression(later)
	if badge == nil || badge.attr("href") != "/reports/"+pass || !strings.Contains(badge.attr("class"), "badge-regression") {
		t.Fatalf("no regression badge linking the pass: %v", badge)
	}
	if !contains(gpu(), "regression") {
		t.Errorf("matrix.json has no regression: %v", gpu())
	}
	row := l.page("/gaps").sole(t, `#regressions tr.regression-row[data-feature="gpu"][data-board="`+m2Board+`"][data-stack="converged"]`)
	if row.sole(t, "td.pass a").attr("href") != "/reports/"+pass || !strings.Contains(row.words(), "converged 4.1.0") {
		t.Errorf("the open regression: %q", row.words())
	}
	if regression(second) != nil {
		t.Error("a pass uploaded after the failure made it a regression")
	}
	l.hideReport(pass)
	if regression(later) != nil {
		t.Error("a hidden Verified pass still makes a regression")
	}
}

func TestOnlyTheSameModelAndStackCountAndARegressionStaysOpenUntilItWorksAgain(t *testing.T) {
	l := newPublicLab(t, nil)
	gpuFails := []string{"gpu.driver", "fails", "gpu.vulkan", "fails"}
	isRegression := func(id string) bool {
		for _, badge := range l.page("/reports/"+id).all(t, `li[id="check-gpu.driver"] .badge`) {
			if badge.words() == "regression" {
				return true
			}
		}
		return false
	}
	onMx := goldenDoc(t, "m2-max-image2")
	onMx["system"].(doc)["stack"] = "mx-mac"
	l.add(onMx, "verified:t")
	otherStack := l.add(goldenWith(t, "m2-max-image2", gpuFails...), "a")
	otherModel := l.add(goldenWith(t, "m1-pro-converged", gpuFails...), "a")
	if isRegression(otherStack) || isRegression(otherModel) {
		t.Error("a pass on another stack or another Mac made a regression")
	}

	l.add(goldenDoc(t, "m2-max-image2"), "verified:t")
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "a")
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "b")
	if got := l.page("/gaps").count(t, `#regressions tr.regression-row[data-feature="gpu"] td a[href^="/reports/"]`); got != 3 {
		t.Errorf("%d report links on the open regression, want the pass and both runs with it", got)
	}
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	if got := l.page("/gaps").count(t, `#regressions tr.regression-row[data-feature="gpu"]`); got != 0 {
		t.Error("the regression stays open after a newer run works again")
	}
}

func TestReportsJSONHasEveryReportAsUploadedUnderCC0(t *testing.T) {
	l := newPublicLab(t, nil)
	m2 := l.add(goldenDoc(t, "m2-max-image2"), "a")
	m1 := l.add(goldenDoc(t, "m1-pro-mx-mac"), "b")
	code, text, header := l.get("/api/v1/reports.json")
	if code != 200 || !strings.HasPrefix(header.Get("Content-Type"), "application/json") || header.Get("Set-Cookie") != "" {
		t.Fatalf("%d %s", code, header.Get("Content-Type"))
	}
	var body doc
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatal(err)
	}
	same(t, "the members", keysInOrder(t, text), []string{"license", "generated_at", "catalogue_version", "reports"})
	if at(body, "license", "id") != "CC0-1.0" {
		t.Error("the licence")
	}
	if _, err := time.Parse("2006-01-02T15:04:05Z", body["generated_at"].(string)); err != nil {
		t.Errorf("generated_at: %v", err)
	}
	list := rowsOf(body, "reports")
	same(t, "the ids, oldest first", []string{list[0]["id"].(string), list[1]["id"].(string)}, []string{m2, m1})
	same(t, "the report as uploaded", list[0]["report"], any(goldenDoc(t, "m2-max-image2")))
	same(t, "its link", list[0]["url"], any(labAddress+"/reports/"+m2))
	same(t, "its upload time", list[0]["uploaded_at"], any("2026-10-04T12:00:00Z"))
	for _, secret := range []string{"key:a", "SSH SIGNATURE", "deletion", "digest", "machine_id", "ssh-ed25519"} {
		if strings.Contains(text, secret) {
			t.Errorf("reports.json carries %q", secret)
		}
	}
	// The bare path answers the same, as the old site's did.
	if again := l.exported("/api/v1/reports"); len(rowsOf(again, "reports")) != 2 {
		t.Error("/api/v1/reports does not answer the JSON export")
	}

	// The filters, and without any the export is unchanged.
	filtered := l.export("/api/v1/reports.json?gen=m1&tool=0.2.0&bogus=1")
	same(t, "the members with a filter", keysInOrder(t, filtered), []string{"license", "generated_at", "catalogue_version", "filters", "reports"})
}

func TestReportsCSVHasOneRowPerReport(t *testing.T) {
	l := newPublicLab(t, nil)
	m2 := l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "b")
	code, text, header := l.get("/api/v1/reports.csv")
	if code != 200 || header.Get("Content-Type") != "text/csv; charset=utf-8" || !strings.Contains(header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("%d %q %q", code, header.Get("Content-Type"), header.Get("Content-Disposition"))
	}
	rows := parseCSV(t, text)
	same(t, "the header", rows[0], reports.ReportColumns)
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	row := byColumn(rows)[0]
	for name, want := range map[string]string{"id": m2, "board": "j416c", "stack": "converged", "omarchy_version": "4.0.0-alpha", "checks": "86",
		"pass": "37", "fail": "4", "skip": "45", "encryption": "on", "url": labAddress + "/reports/" + m2, "schema_version": "1", "expected_gaps": "2"} {
		if row[name] != want {
			t.Errorf("%s is %q, want %q", name, row[name], want)
		}
	}

	// Uploaded text that looks like a spreadsheet formula is exported as text.
	formula := goldenDoc(t, "m2-max-image2")
	formula["machine"].(doc)["kernel"] = `=HYPERLINK("http://example.com")`
	l.add(formula, "c")
	if !strings.Contains(l.export("/api/v1/reports.csv"), `"'=HYPERLINK(""http://example.com"")"`) {
		t.Error("a formula is exported as a formula")
	}
}

func TestChecksCSVHasOneRowPerCheckResult(t *testing.T) {
	l := newPublicLab(t, nil)
	m2 := l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "b")
	rows := parseCSV(t, l.export("/api/v1/checks.csv"))
	same(t, "the header", rows[0], reports.CheckColumns)
	want := 1 + len(goldenDoc(t, "m2-max-image2")["checks"].([]any)) + len(goldenDoc(t, "m1-pro-mx-mac")["checks"].([]any))
	if len(rows) != want {
		t.Errorf("%d rows, want %d", len(rows), want)
	}
	for _, row := range byColumn(rows) {
		if row["report_id"] == m2 && row["status"] == "fail" {
			for name, want := range map[string]string{"check_id": "setup.first-boot-hardware", "outcome": "fails", "feature": "first-boot-hardware-setup",
				"layer": "omarchy", "expected_aurora": "supported", "expected_omarchy": "supported", "expected_asahi": "", "kind": "automatic",
				"answered_by_default": "false", "build": "99ace40.361571887310001", "uploaded_at": "2026-10-04T12:00:00Z"} {
				if row[name] != want {
					t.Errorf("%s is %q, want %q", name, row[name], want)
				}
			}
			break
		}
	}
	if code, _, header := l.get("/api/v1/checks"); code != 200 || header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Errorf("/api/v1/checks: %d %s", code, header.Get("Content-Type"))
	}
}

func TestChecksCSVSaysWhichAnswersWereTheDefault(t *testing.T) {
	l := newPublicLab(t, nil)
	byDefault := goldenWith(t, "m2-max-image2", "input.trackpad-gestures", "works")
	checkOf(t, byDefault, "input.trackpad-gestures")["answered_by_default"] = true
	l.add(byDefault, "a")
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	var answers []string
	for _, row := range byColumn(parseCSV(t, l.export("/api/v1/checks.csv"))) {
		if row["check_id"] == "input.trackpad-gestures" {
			answers = append(answers, row["answered_by_default"])
		}
	}
	same(t, "answered_by_default", sorted(answers), []string{"false", "true"})
}

func TestMatrixJSONHasEachCellsStateAndPerMachineAgreement(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "b")
	l.add(goldenDoc(t, "m2-max-image2"), "c")
	text := l.export("/api/v1/matrix.json")
	same(t, "the members", keysInOrder(t, text), []string{"license", "generated_at", "catalogue_version", "agreement", "features", "rows"})
	body := l.exported("/api/v1/matrix.json")
	if at(body, "license", "id") != "CC0-1.0" {
		t.Error("the licence")
	}
	for _, row := range rowsOf(body, "rows") {
		switch row["board"] {
		case "j416c":
			same(t, "the M2's gpu", at(row, "cells", "gpu"), any(doc{"state": "works", "tentative": "works", "machines": doc{"works": 2.0}, "tester_machines": doc{}}))
		case "j314s":
			same(t, "the M1's vendor firmware", at(row, "cells", "vendor-firmware"), any(doc{"state": nil, "tentative": "fails", "machines": doc{"fails": 1.0}, "tester_machines": doc{}}))
		}
	}
	if len(rowsOf(body, "rows")) != 2 {
		t.Errorf("%d rows", len(rowsOf(body, "rows")))
	}
	if again := l.exported("/api/v1/matrix"); len(rowsOf(again, "rows")) != 2 {
		t.Error("/api/v1/matrix does not answer the export")
	}
}

func TestHiddenAndDeletedReportsAreLeftOutOfEveryExport(t *testing.T) {
	l := newPublicLab(t, nil)
	m2 := l.add(goldenDoc(t, "m2-max-image2"), "a")
	m1 := l.add(goldenDoc(t, "m1-pro-mx-mac"), "b")
	l.hideReport(m1)
	l.exec(`DELETE FROM reports WHERE public_id = ?`, m2)
	if got := rowsOf(l.exported("/api/v1/reports.json"), "reports"); len(got) != 0 {
		t.Errorf("reports.json: %d reports", len(got))
	}
	if got := parseCSV(t, l.export("/api/v1/reports.csv")); len(got) != 1 {
		t.Errorf("reports.csv: %d rows", len(got))
	}
	if got := parseCSV(t, l.export("/api/v1/checks.csv")); len(got) != 1 {
		t.Errorf("checks.csv: %d rows", len(got))
	}
	if got := rowsOf(l.exported("/api/v1/matrix.json"), "rows"); len(got) != 0 {
		t.Errorf("matrix.json: %d rows", len(got))
	}
	if got := rowsOf(l.exported("/api/v1/benchmarks.json"), "benchmarks"); len(got) != 0 {
		t.Errorf("benchmarks.json: %d benchmarks", len(got))
	}
	if got := rowsOf(l.exported("/api/v1/aurora.json"), "chips"); len(got) != 0 {
		t.Errorf("aurora.json: %d chips", len(got))
	}
}

func TestTheExportsCarryTheBuildOfEveryReport(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	exportedBy := map[string]doc{}
	for _, entry := range rowsOf(l.exported("/api/v1/reports.json"), "reports") {
		exportedBy[entry["id"].(string)] = entry
	}
	m3 := exportedBy[m3ID]["build"].(doc)
	same(t, "the M3's build", m3, doc{"id": m3Build, "label": m3Build, "words": m3Words, "commit": "f60e1ba", "stamp": "2026092602", "linux_aurora": "7.1.12.aurora2-11",
		"omarchy_mac_boot": "20260926-1", "tool_version": "0.1.8"})
	same(t, "a matched set", at(exportedBy["q8avqMwJH59VSjxM5TxAyF38"], "build", "candidate_set"), any("apple-test-0052582276d9-20260927"))
	for _, entry := range productionReports(t) {
		if entry.ID == m3ID {
			delete(entry.Report, "signature")
			same(t, "the M3's report", exportedBy[m3ID]["report"], any(entry.Report))
		}
	}
	for _, row := range byColumn(parseCSV(t, l.export("/api/v1/reports.csv"))) {
		if row["id"] != m3ID {
			continue
		}
		same(t, "reports.csv's build", []string{row["build"], row["build_commit"], row["build_stamp"], row["linux_aurora"], row["omarchy_mac_boot"], row["build_candidate_set"], row["build_words"]},
			[]string{m3Build, "f60e1ba", "2026092602", "7.1.12.aurora2-11", "20260926-1", "", m3Words})
		if gaps, _ := strconv.Atoi(row["expected_gaps"]); gaps < 4 {
			t.Errorf("expected_gaps %q", row["expected_gaps"])
		}
	}
	var builds []string
	for _, row := range byColumn(parseCSV(t, l.export("/api/v1/checks.csv"))) {
		if row["report_id"] == m3ID {
			builds = append(builds, row["build"])
		}
	}
	same(t, "checks.csv's build", uniqSorted(builds), []string{m3Build})
	for _, row := range rowsOf(l.exported("/api/v1/matrix.json"), "rows") {
		if row["board"] == "j613" {
			same(t, "matrix.json's row", []any{row["version"], at(row, "build", "id")}, []any{"4.0.0-alpha", m3Build})
		}
	}
}

func TestTheReportsExportsTakeTheFilters(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	all := productionReports(t)
	if got := rowsOf(l.exported("/api/v1/reports.json"), "reports"); len(got) != len(all) {
		t.Errorf("%d reports", len(got))
	}
	body := l.exported("/api/v1/reports.json?gen=m1&tool=0.1.8")
	same(t, "the filters", body["filters"], any(doc{"gen": "m1", "tool": "0.1.8"}))
	var ids []string
	for _, report := range rowsOf(body, "reports") {
		ids = append(ids, report["id"].(string))
	}
	same(t, "the filtered reports", sorted(ids), idsWhere(all, func(r doc, _ produced) bool {
		return strings.HasPrefix(at(r, "machine", "chip").(string), "M1") && at(r, "tool", "version") == "0.1.8"
	}))
	rows := parseCSV(t, l.export("/api/v1/reports.csv?chip=M2+Max&from=2026-09-27"))
	same(t, "the header", rows[0], reports.ReportColumns)
	same(t, "the filtered rows", sorted(column(rows, "id")), idsWhere(all, func(r doc, e produced) bool {
		return at(r, "machine", "chip") == "M2 Max" && e.UploadedAt >= "2026-09-27"
	}))
	var m3 doc
	for _, entry := range all {
		if entry.ID == m3ID {
			m3 = entry.Report
		}
	}
	counted := 0
	for _, check := range m3["checks"].([]any) {
		if check.(doc)["answered_by_default"] != true {
			counted++
		}
	}
	rows = parseCSV(t, l.export("/api/v1/reports.csv?model=j613&confirmed=1"))
	if len(rows) != 2 || byColumn(rows)[0]["checks"] != fmt.Sprint(counted) {
		t.Errorf("confirmed only: %d rows, checks %q, want %d", len(rows), byColumn(rows)[0]["checks"], counted)
	}
}

func TestChecksCSVTakesTheRunFiltersAndTheResultFilters(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	rowsFor := func(query string) [][]string { return parseCSV(t, l.export("/api/v1/checks.csv"+query))[1:] }
	index := func(name string) int {
		for i, column := range reports.CheckColumns {
			if column == name {
				return i
			}
		}
		t.Fatalf("no column %s", name)
		return -1
	}
	pick := func(rows [][]string, keep func([]string) bool) [][]string {
		var out [][]string
		for _, row := range rows {
			if keep(row) {
				out = append(out, row)
			}
		}
		return out
	}
	all := rowsFor("")
	m3 := rowsFor("?model=j613")
	same(t, "model", m3, pick(all, func(row []string) bool { return row[index("board")] == "j613" }))
	same(t, "layer", rowsFor("?model=j613&layer=aurora"), pick(m3, func(row []string) bool { return row[index("layer")] == "aurora" }))
	same(t, "confirmed", rowsFor("?model=j613&confirmed=1"), pick(m3, func(row []string) bool { return row[index("answered_by_default")] != "true" }))
	gaps := rowsFor("?model=j613&state=gap")
	if len(gaps) == 0 {
		t.Fatal("no gap rows")
	}
	for _, row := range gaps {
		if !contains([]string{"not-in-aurora", "not-in-asahi", "not-in-omarchy"}, row[index("outcome")]) {
			t.Errorf("a gap row with outcome %q", row[index("outcome")])
		}
	}
	for _, row := range rowsFor("?model=j613&state=works&layer=omarchy&confirmed=1") {
		if row[index("outcome")] != "works" || row[index("layer")] != "omarchy" || row[index("answered_by_default")] != "false" {
			t.Errorf("a row past the three filters: %v", row)
		}
	}
}

func TestMatrixJSONTakesTheFiltersAndTheGrouping(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	all := productionReports(t)
	_, tested, byLayer := testedFeatures(t)
	plain := l.exported("/api/v1/matrix.json")
	if got := len(plain["features"].([]any)); got != tested {
		t.Errorf("%d features, want %d", got, tested)
	}
	m2Boards := []string{}
	for _, entry := range all {
		if strings.HasPrefix(at(entry.Report, "machine", "chip").(string), "M2") {
			m2Boards = append(m2Boards, at(entry.Report, "machine", "board").(string))
		}
	}
	m2Boards = uniqSorted(m2Boards)
	text := l.export("/api/v1/matrix.json?gen=m2&layer=asahi")
	same(t, "the members with a filter", keysInOrder(t, text), []string{"license", "generated_at", "filters", "runs", "catalogue_version", "agreement", "features", "rows"})
	body := l.exported("/api/v1/matrix.json?gen=m2&layer=asahi")
	same(t, "the filters", body["filters"], any(doc{"gen": "m2", "layer": "asahi"}))
	same(t, "the runs", body["runs"], any(4.0))
	var boards, featureIDs, cellIDs []string
	for _, row := range rowsOf(body, "rows") {
		boards = append(boards, row["board"].(string))
	}
	same(t, "the boards", uniqSorted(boards), m2Boards)
	for _, feature := range rowsOf(body, "features") {
		featureIDs = append(featureIDs, feature["id"].(string))
		if feature["layer"] != "asahi" {
			t.Errorf("a feature of layer %v", feature["layer"])
		}
	}
	if len(featureIDs) != byLayer["asahi"] {
		t.Errorf("%d asahi features, want %d", len(featureIDs), byLayer["asahi"])
	}
	for id := range rowsOf(body, "rows")[0]["cells"].(doc) {
		cellIDs = append(cellIDs, id)
	}
	same(t, "the cells of a row", sorted(cellIDs), sorted(featureIDs))
	if got := len(rowsOf(l.exported("/api/v1/matrix.json?gen=m2&group=release"), "rows")); got != len(m2Boards) {
		t.Errorf("group=release: %d rows, want %d", got, len(m2Boards))
	}
	hidden := 0
	for _, row := range rowsOf(l.exported("/api/v1/matrix.json?confirmed=1"), "rows") {
		for _, cell := range row["cells"].(doc) {
			cell := cell.(doc)
			if cell["hidden"] == true {
				hidden++
			} else if cell["state"] == nil {
				t.Error("confirmed only shows an unconfirmed cell")
			}
		}
	}
	if hidden == 0 {
		t.Error("confirmed only hides no cell")
	}
	byModel := l.export("/api/v1/matrix.json?group=model")
	keys := keysInOrder(t, byModel)
	if keys[len(keys)-1] != "group" || !strings.Contains(byModel, `"filters":{},"runs":11,`) || !strings.Contains(byModel, `"last_tested_at":"2026-`) || !strings.Contains(byModel, `"releases":["4.0.0-alpha"]`) {
		t.Errorf("group=model: %v", keys)
	}
}

func TestTheDataPageLinksEveryExport(t *testing.T) {
	l := newPublicLab(t, nil)
	page := l.page("/data")
	for _, path := range []string{"/api/v1/reports.json", "/api/v1/reports.csv", "/api/v1/checks.csv", "/api/v1/matrix.json", "/api/v1/benchmarks.json", "/api/v1/aurora.json"} {
		if page.count(t, `a[href="`+path+`"]`) != 1 {
			t.Errorf("no link to %s", path)
		}
		if code, _, _ := l.get(path); code != http.StatusOK {
			t.Errorf("%s: %d", path, code)
		}
	}
	if page.count(t, `main a[href="https://creativecommons.org/publicdomain/zero/1.0/"]`) != 1 {
		t.Error("no link to CC0")
	}
	if !page.hasText(t, ".site-nav a.current", "Data") {
		t.Error("Data is not the current page")
	}
}

const mystery = "apple,t6020-mystery"

func TestTheReportPageListsTheHardwareNoDriverClaimed(t *testing.T) {
	l := newPublicLab(t, nil)
	body := goldenDoc(t, "m2-max-image2")
	inventory := body["inventory"].(doc)
	inventory["nodes"] = append(inventory["nodes"].([]any), doc{"compatible": mystery, "status": "okay", "driver": "unbound", "count": 1})
	inventory["unclaimed"] = append(inventory["unclaimed"].([]any), doc{"compatible": mystery, "count": 1, "outcome": "unknown-hardware"})
	page := l.page("/reports/" + l.add(body, "a"))
	if !page.hasText(t, "#inventory h2.section-title", "hardware inventory") {
		t.Error("no inventory title")
	}
	if want := "425 hardware nodes: 366 claimed by a driver, 27 with no device of their own, 5 bus or register containers with no driver of their own, 23 disabled, 4 unclaimed."; page.sole(t, "#inventory .inventory-summary").words() != want {
		t.Errorf("the summary: %q", page.sole(t, "#inventory .inventory-summary").words())
	}
	if page.count(t, "#inventory ul.unknown-hardware li") != 1 || !page.says(t, "#inventory ul.unknown-hardware li", mystery+" (1 node): unknown hardware") {
		t.Error("the unknown hardware")
	}
	if !page.says(t, "#inventory ul.unclaimed-hardware li", "apple,t6020-avd (1 node): doesn't work, but should on this Mac (Video decoder, asahi layer)") ||
		!page.says(t, "#inventory ul.unclaimed-hardware li", "apple,t6020-dpaudio (2 nodes): not yet supported by Asahi (DisplayPort audio, asahi layer)") {
		t.Error("the known hardware no driver claimed")
	}

	// The kernel's build options that differ from Asahi's.
	page = l.page("/reports/" + l.add(goldenDoc(t, "m2-max-image2"), "b"))
	if !page.says(t, "#inventory p", "22 options differ from Asahi's linux-asahi 7.1.13.asahi3-2 (asahi-alarm/PKGBUILDs@d585b07)") {
		t.Error("the kernel's build options")
	}
	if page.count(t, "#inventory table.kernel-config tbody tr") != 22 || !page.hasText(t, "#inventory table.kernel-config td code", "CONFIG_USB4") {
		t.Error("the options table")
	}

	// The M1 has no unknown hardware: its DisplayPort audio isn't in Asahi yet.
	page = l.page("/reports/" + l.add(goldenDoc(t, "m1-pro-mx-mac"), "c"))
	if page.count(t, "#inventory ul.unknown-hardware") != 0 || !page.says(t, "#inventory p.dim", "None: every node on this Mac is either claimed by a driver or known to the feature catalogue") {
		t.Error("the M1's unknown hardware")
	}
	if !page.says(t, "#inventory ul.unclaimed-hardware li", "apple,t6000-dpaudio (1 node): not yet supported by Asahi (DisplayPort audio, asahi layer)") {
		t.Error("the M1's DisplayPort audio")
	}

	// A report without an inventory shows no hardware section.
	without := goldenDoc(t, "m2-max-image2")
	delete(without, "inventory")
	if l.page("/reports/"+l.add(without, "d")).count(t, "#inventory") != 0 {
		t.Error("an inventory section on a report without one")
	}
}
