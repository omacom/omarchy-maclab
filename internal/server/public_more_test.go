package server

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/reports"
)

// Kernel gaps, Candidates, Aurora and Benchmarks, translated from the old
// site's tests: gaps_inventory_test.rb, aurora_support_test.rb and
// benchmarks_test.rb whole, and what pages_test.rb, build_identity_test.rb,
// regressions_test.rb and answered_by_default_test.rb ask of these pages.

func TestTheKernelGapListShowsReportedGapsAndWhatAsahiHasButAuroraHasNotVerified(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenWith(t, "m2-max-image2", "input.ambient-light", "not-in-aurora", "display.outputs", "not-in-asahi", "power.battery", "unknown-hardware"), "a")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "a")
	page := l.page("/gaps")
	if !page.says(t, `#reported tr.gap-row[data-outcome="not-in-aurora"][data-feature="aop"]`, "M2 Max") {
		t.Error("the AOP gap")
	}
	if page.count(t, `#reported tr.gap-row[data-outcome="not-in-asahi"][data-feature="main-display"]`) != 1 ||
		page.count(t, `#reported tr.gap-row[data-outcome="unknown-hardware"][data-feature="battery-info"] a[href^="/reports/"]`) != 1 {
		t.Error("the display or battery gap")
	}
	// Both Macs' unclaimed hardware.
	if got := page.count(t, `#reported tr.gap-row[data-outcome="not-in-aurora"][data-feature="hardware-drivers"]`); got != 2 {
		t.Errorf("%d hardware-drivers gaps, want 2", got)
	}
	if got := page.count(t, "#reported tr.gap-row"); got != 5 {
		t.Errorf("%d reported gaps, want 5", got)
	}
	if !page.says(t, `#unverified-in-aurora tr.gap-row[data-feature="video-decoder"]`, "M3") ||
		!page.says(t, "#missing-in-aurora", "nothing Asahi supports that Aurora is known to lack") {
		t.Error("the catalogue's gaps")
	}
	if !page.hasText(t, ".site-nav a.current", "Kernel gaps") || page.count(t, "a.issue-link") != 0 {
		t.Error("the current page, or an issue link for a visitor")
	}
}

func TestTheKernelGapPageListsHardwareNoDriverClaimsUnknownHardwareFirst(t *testing.T) {
	l := newPublicLab(t, nil)
	with := goldenDoc(t, "m2-max-image2")
	inventory := with["inventory"].(doc)
	inventory["unclaimed"] = append(inventory["unclaimed"].([]any), doc{"compatible": mystery, "count": 1, "outcome": "unknown-hardware"})
	l.add(with, "a")
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "c")
	page := l.page("/gaps")
	same(t, "the rows", attrsOf(page.all(t, "#unclaimed-hardware tr.unclaimed-row"), "data-compatible"),
		[]string{mystery, "apple,t6000-dpaudio", "apple,t6020-avd", "apple,t6020-dpaudio"})
	row := func(compatible string) string {
		return `#unclaimed-hardware tr.unclaimed-row[data-compatible="` + compatible + `"]`
	}
	if !page.hasText(t, row(mystery)+`[data-outcome="unknown-hardware"] td`, "unknown hardware") || !page.hasText(t, row(mystery)+" td.machines", "1") {
		t.Error("the unknown hardware")
	}
	// DisplayPort audio has no Asahi driver yet: known hardware, linked to its feature.
	dp := row("apple,t6020-dpaudio")
	if !page.hasText(t, dp+`[data-outcome="not-in-asahi"] td a[href="/features/dp-audio"]`, "DisplayPort audio") || !page.hasText(t, dp+" td", "M2 Max (t6021)") ||
		!page.hasText(t, dp+" td", "MacBook Pro (16-inch, M2 Max, 2023)") || !page.hasText(t, dp+" td.machines", "2") {
		t.Error("the M2's DisplayPort audio")
	}
	if !page.hasText(t, row("apple,t6000-dpaudio")+" td.machines", "1") || !page.hasText(t, row("apple,t6000-dpaudio")+" td", "MacBook Pro (14-inch, M1 Pro, 2021)") {
		t.Error("the M1's DisplayPort audio")
	}
	avd := row("apple,t6020-avd")
	if !page.hasText(t, avd+`[data-outcome="fails"] td a[href="/features/video-decoder"]`, "Video decoder") || page.count(t, avd+` td a[href^="/reports/"]`) != 2 {
		t.Error("the video decoder")
	}

	// Reports without an inventory add nothing.
	l = newPublicLab(t, nil)
	without := goldenDoc(t, "m2-max-image2")
	delete(without, "inventory")
	l.add(without, "a")
	page = l.page("/gaps")
	if page.count(t, "#unclaimed-hardware tr.unclaimed-row") != 0 || !page.says(t, "#unclaimed-hardware", "None reported") {
		t.Error("a report without an inventory added hardware")
	}
}

func TestTheCandidatesPageMapsEachBuildToItsCandidateSetWhenItIsKnown(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	page := l.page("/candidates")
	air := page.sole(t, `tr.build-row[data-build="`+m1AirBuild+`"]`)
	if air.attr("data-set") != "apple-test-0052582276d9-20260927" || air.attr("data-set-source") != "packages" || !strings.Contains(air.words(), "matched by package") {
		t.Errorf("the M1 Air's build: %v %q", air.attrs, air.words())
	}
	m3 := page.sole(t, `tr.build-row[data-build="`+m3Build+`"]`)
	if m3.attr("data-set") != "" || !m3.hasText(t, "td", "unknown") {
		t.Errorf("the M3's build: %v", m3.attrs)
	}
	if got := page.count(t, `tr.build-row[data-build="1937418.362376005140001"]`); got != 3 {
		t.Errorf("%d builds of one runtime, want 3", got)
	}
	if page.count(t, "tr.candidate-row") != 0 || !page.says(t, ".card", "No runs on candidate images yet") {
		t.Error("a candidate set with no image naming one")
	}
	if !page.hasText(t, ".site-nav a.current", "Candidates") {
		t.Error("Candidates is not the current page")
	}
	for _, missing := range []string{"/candidates/nothing-names-this", "/candidates/apple-test-0052582276d9-20260927", "/candidates/a%20b", "/candidates/x/y"} {
		if got := l.status(missing); got != 404 {
			t.Errorf("%s: %d", missing, got)
		}
	}
}

// working is the golden report with every failing check working, then these outcomes.
func working(t *testing.T, name string, outcomes ...string) doc {
	t.Helper()
	var fixed []string
	for _, check := range goldenDoc(t, name)["checks"].([]any) {
		if at(check.(doc), "classification", "outcome") == "fails" {
			fixed = append(fixed, check.(doc)["id"].(string), "works")
		}
	}
	return goldenWith(t, name, append(fixed, outcomes...)...)
}

func onSet(body doc, set string) doc {
	body["system"].(doc)["candidate_set"] = set
	return body
}

func TestCandidatesSaysPassingFailingOrWaiting(t *testing.T) {
	l := newPublicLab(t, nil)
	verdict := func(set string) (*node, *node) {
		page := l.page("/candidates/" + set)
		return page.sole(t, "#verdict"), page
	}
	row := func(set string) *node {
		return l.page("/candidates").sole(t, `tr.candidate-row[id="candidate-`+set+`"]`)
	}

	// Community runs are listed and do not count.
	l.add(onSet(working(t, "m2-max-image2"), "edge-1"), "a")
	line, page := verdict("edge-1")
	if line.attr("data-verdict") != "waiting" || line.words() != "Waiting" || !strings.Contains(line.attr("class"), "verdict-waiting") {
		t.Errorf("with a Community run only: %v %q", line.attrs, line.words())
	}
	if !page.says(t, ".subtitle", "1 run, 0 Verified runs") || !page.says(t, ".card", "No Verified run on this candidate set yet") || page.count(t, "tr.report-row") != 1 {
		t.Error("the set's page with a Community run only")
	}
	if got := row("edge-1"); got.attr("data-verdict") != "waiting" || !got.hasText(t, ".verdict", "Waiting") {
		t.Errorf("the list: %q", got.words())
	}

	// Verified runs on a model, nothing failing.
	l.add(onSet(working(t, "m2-max-image2"), "edge-1"), "verified:t")
	line, page = verdict("edge-1")
	if line.attr("data-verdict") != "ready" || line.words() != "Passing: Verified runs on 1 model, nothing failing" {
		t.Errorf("with a Verified run: %v %q", line.attrs, line.words())
	}
	if page.count(t, "#failures") != 0 || page.count(t, "tr.matrix-row") != 1 || page.count(t, "tr.report-row") != 2 || !page.says(t, ".subtitle", "2 runs, 1 Verified run.") {
		t.Error("the passing set's page")
	}
	if page.count(t, `tr.matrix-row td[data-feature="gpu"][data-verified="tester"]`) != 1 {
		t.Error("the set's matrix does not count Verified runs")
	}
	l.add(onSet(working(t, "m1-pro-converged"), "edge-1"), "verified:u")
	if line, _ = verdict("edge-1"); line.words() != "Passing: Verified runs on 2 models, nothing failing" {
		t.Errorf("on two models: %q", line.words())
	}
	if got := row("edge-1"); !got.hasText(t, ".verdict", "Passing: Verified runs on 2 models, nothing failing") {
		t.Errorf("the list: %q", got.words())
	}

	// A Verified run's regression on the set is named.
	l.add(working(t, "m2-max-image2"), "verified:t")
	l.add(onSet(working(t, "m2-max-image2", "gpu.driver", "fails", "gpu.vulkan", "fails"), "edge-2"), "verified:t")
	line, page = verdict("edge-2")
	if line.attr("data-verdict") != "blocked" || line.words() != "Failing: 1 failure, 1 regression" {
		t.Errorf("with a regression: %v %q", line.attrs, line.words())
	}
	if !page.says(t, `#failures li.finding[data-state="regression"]`, "GPU") {
		t.Error("the regression is not named")
	}

	// A failure with no earlier Verified pass is a failure, not a regression.
	l.add(onSet(working(t, "m1-pro-mx-mac", "gpu.driver", "fails", "gpu.vulkan", "fails"), "edge-3"), "verified:u")
	if line, _ = verdict("edge-3"); line.words() != "Failing: 1 failure, 0 regressions" {
		t.Errorf("with a plain failure: %q", line.words())
	}
	if got := l.page("/candidates").count(t, "tr.candidate-row"); got != 3 {
		t.Errorf("%d sets listed, want 3", got)
	}
	same(t, "the sets, latest run first", attrsOf(l.page("/candidates").all(t, "tr.candidate-row"), "id"), []string{"candidate-edge-3", "candidate-edge-2", "candidate-edge-1"})
}

func TestACandidateSetWhoseVerifiedRunsCountedNothingIsStillWaiting(t *testing.T) {
	l := newPublicLab(t, nil)
	body := onSet(goldenDoc(t, "m2-max-image2"), "edge-2026.09.27")
	for _, check := range body["checks"].([]any) {
		check.(doc)["answered_by_default"] = true
	}
	l.add(body, "verified:a")
	page := l.page("/candidates/edge-2026.09.27")
	if line := page.sole(t, "#verdict"); line.words() != "Waiting" || strings.Contains(page.words(), "Passing") {
		t.Errorf("Verified runs that counted nothing: %q", line.words())
	}
}

func TestRunsMatchedByPackageAreListedOnTheirSetNotCounted(t *testing.T) {
	l := newPublicLab(t, nil)
	l.production()
	set := "apple-test-0052582276d9-20260927"
	air := productionReports(t)[0].Report
	for _, entry := range productionReports(t) {
		if at(entry.Report, "machine", "board") == "j313" {
			air = entry.Report
		}
	}
	delete(air, "signature")
	named := l.add(onSet(air, set), "verified:t")
	page := l.page("/candidates/" + set)
	if got := page.count(t, "#matched tr.report-row"); got != 4 {
		t.Errorf("%d runs matched by package, want 4", got)
	}
	if page.count(t, "#matched tr#report-"+named) != 0 || !page.says(t, ".subtitle", "1 run, 1 Verified run.") {
		t.Error("the run whose image names the set is counted as matched, or the matched ones as the set's")
	}
}

const (
	m2Gen = "m2-pro-max-ultra"
	m1Gen = "m1-pro-max-ultra"
)

func auroraRow(t *testing.T, page *node, chip, feature string) *node {
	t.Helper()
	return page.sole(t, `tr.aurora-row[data-chip="`+chip+`"][data-feature="`+feature+`"]`)
}

func TestCommunityRunsDoNotMakeTheAuroraTable(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	page := l.page("/aurora")
	if page.count(t, "tr.aurora-row") != 0 || !page.says(t, ".card", "No Verified run on the Aurora kernel has been uploaded yet") {
		t.Error("Community runs made the table")
	}
}

func TestAVerifiedRunOnAuroraFillsItsChipGenerationNextToTheExpectedStates(t *testing.T) {
	l := newPublicLab(t, nil)
	id := l.add(goldenDoc(t, "m2-max-image2"), "verified:a")
	page := l.page("/aurora")
	chip := `section[id="chip-` + m2Gen + `"]`
	if !page.hasText(t, chip+" .card-title", "M2 Pro/Max/Ultra") || !page.says(t, chip, "1 Verified machine, 1 run: MacBook Pro (16-inch, M2 Max, 2023)") ||
		!page.says(t, chip, "Aurora 7.1.12.aurora2-2") {
		t.Errorf("the chip's card: %q", page.sole(t, chip+" p.dim").words())
	}
	gpu := auroraRow(t, page, m2Gen, "gpu")
	if gpu.attr("data-state") != "works" || gpu.all(t, "td.expected-yes")[0].words() != "asahi" {
		t.Errorf("the gpu row: %v", gpu.attrs)
	}
	if gpu.sole(t, "td.sources a").attr("href") != "/reports/"+id || !strings.Contains(gpu.sole(t, "td.sources").words(), "7.1.12.aurora2-2") {
		t.Error("the gpu row's sources")
	}
	// Kernel features only: Omarchy integration isn't Aurora's.
	if page.count(t, `tr.aurora-row[data-feature="first-boot-hardware-setup"]`) != 0 || page.count(t, `tr.aurora-row[data-feature="aop"]`) != 1 {
		t.Error("the rows are not the kernel's features")
	}
	// Chip generations nobody has verified yet are named, not guessed.
	if !page.says(t, "#untested-chips", "M1 Pro/Max/Ultra") ||
		page.count(t, `#sources a[href="https://github.com/omacom/linux"]`) != 1 || !page.hasText(t, `nav a[href="/aurora"]`, "Aurora") {
		t.Error("the untested chips, the sources or the navigation")
	}
	if page.count(t, `#sources a[href="/api/v1/aurora.json"]`) != 1 {
		t.Error("no link to aurora.json")
	}

	// Runs off the Aurora kernel don't count.
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, "m1-pro-mx-mac"), "verified:a")
	page = l.page("/aurora")
	if page.count(t, `tr.aurora-row[data-chip="`+m1Gen+`"]`) != 0 || !page.says(t, "#untested-chips", "M1 Pro/Max/Ultra") {
		t.Error("a run off the Aurora kernel counted")
	}
}

func TestEachVerifiedMachineCountsOnceOnAuroraAndDisagreementMakesItPartial(t *testing.T) {
	l := newPublicLab(t, nil)
	gpuFails := []string{"gpu.driver", "fails", "gpu.vulkan", "fails"}
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "verified:a")
	l.add(goldenDoc(t, "m2-max-image2"), "verified:a")
	l.add(goldenDoc(t, "m2-max-converged"), "verified:b")
	page := l.page("/aurora")
	if auroraRow(t, page, m2Gen, "gpu").attr("data-state") != "works" || !page.says(t, `section[id="chip-`+m2Gen+`"]`, "2 Verified machines, 3 runs") {
		t.Errorf("two machines that agree: %q", auroraRow(t, page, m2Gen, "gpu").attr("data-state"))
	}
	l.add(goldenWith(t, "m2-max-converged", gpuFails...), "verified:b")
	gpu := auroraRow(t, l.page("/aurora"), m2Gen, "gpu")
	if gpu.attr("data-state") != "partial" || !strings.Contains(gpu.sole(t, "td.verified").attr("title"), "verified") {
		t.Errorf("two machines that disagree: %q, %q", gpu.attr("data-state"), gpu.sole(t, "td.verified").attr("title"))
	}
}

func TestAuroraJSONHasTheTablePerChipEachCellsSourcesAndTheCataloguesSources(t *testing.T) {
	l := newPublicLab(t, nil)
	id := l.add(goldenDoc(t, "m2-max-image2"), "verified:a")
	l.add(goldenDoc(t, "m1-pro-converged"), "c")
	text := l.export("/api/v1/aurora.json")
	same(t, "the members", keysInOrder(t, text), []string{"license", "generated_at", "catalogue_version", "method", "sources", "features", "chips", "untested_chips"})
	body := l.exported("/api/v1/aurora.json")
	if at(body, "license", "id") != "CC0-1.0" || body["catalogue_version"] != float64(l.s.catalogue.Version()) {
		t.Error("the licence or the catalogue's version")
	}
	chips := rowsOf(body, "chips")
	if len(chips) != 1 || chips[0]["chip"] != m2Gen {
		t.Fatalf("the chips: %v (the Community M1 run must not count)", len(chips))
	}
	if !contains(strs(body["untested_chips"]), m1Gen) {
		t.Error("the M1 generation is not among the untested")
	}
	m2 := chips[0]
	same(t, "socs", m2["socs"], any([]any{"t6021"}))
	same(t, "aurora_versions", m2["aurora_versions"], any([]any{"7.1.12.aurora2-2"}))
	same(t, "machines and runs", []any{m2["machines"], m2["runs"], m2["name"]}, []any{1.0, 1.0, "M2 Pro/Max/Ultra"})
	gpu := at(m2, "cells", "gpu").(doc)
	same(t, "the gpu cell", []any{gpu["state"], gpu["tester_machines"], at(gpu, "expected", "aurora", "status"), at(gpu, "expected", "asahi", "status"), gpu["reports"]},
		[]any{"works", doc{"works": 1.0}, "asahi", "linux-asahi", []any{labAddress + "/reports/" + id}})
	if at(body, "sources", "asahi", "license") != nil || at(body, "sources", "aurora", "url") != "https://github.com/omacom/linux" {
		t.Error("the catalogue's sources")
	}
	for _, feature := range rowsOf(body, "features") {
		if feature["layer"] == "omarchy" {
			t.Errorf("an Omarchy feature in the table: %v", feature["id"])
		}
	}
}

func strs(value any) []string {
	var out []string
	list, _ := value.([]any)
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

const benchmarkGolden = "benchmarks/m2-max-converged"

// runWith is the golden benchmark run with its scores scaled, optionally on
// another Mac (a corpus golden's machine and system) or in another suite.
func runWith(t *testing.T, factor float64, on string, suite int) doc {
	t.Helper()
	body := goldenDoc(t, benchmarkGolden)
	if on != "" {
		other := goldenDoc(t, on)
		body["machine"], body["system"] = other["machine"], other["system"]
	}
	for _, check := range body["checks"].([]any) {
		score, ok := check.(doc)["score"].(doc)
		if !ok {
			continue
		}
		score["value"] = math.Round(score["value"].(float64) * factor)
		if suite != 0 {
			score["suite"] = suite
		}
	}
	return body
}

// storageRun is a golden run with a storage score, on the given capacity
// ("" for a report from before capacity was recorded).
func storageRun(t *testing.T, value int, capacity string) doc {
	t.Helper()
	body := runWith(t, 1, "", 0)
	if capacity != "" {
		body["machine"].(doc)["storage"] = capacity
	}
	body["checks"] = append(body["checks"].([]any), doc{"id": "benchmark.storage", "kind": "automatic", "status": "pass", "evidence": []any{fmt.Sprintf("sequential read: %d MB/s", value)},
		"classification": doc{"outcome": "works", "feature": "nvme", "layer": "asahi", "expected": doc{"asahi": "upstream"}},
		"score":          doc{"value": value, "unit": "MBps", "tool": "fio 3.38", "suite": 1}})
	return body
}

// withReference gives the lab a macOS reference for one board.
func (l *publicLab) withReference(board string) {
	reference := reports.NewMacOSReference([]byte(`{"conditions": {"power": "ac", "brightness": "about 50%", "displays": 1}, "macos": {"version": "26.6"}, "captured": "2026-11-01",
		"metrics": {"h264_decode_fps": 900}, "storage": {"1TB": {"read_mbps": 6000}}}`))
	l.s.macOSReference = func(asked string) *reports.MacOSReference {
		if asked == board {
			return reference
		}
		return nil
	}
}

func scoreRows(t *testing.T, page *node, benchmark string) []*node {
	return page.all(t, `section[id="benchmark-`+benchmark+`"] tr.score-row`)
}

func TestStorageRowsSplitByCapacityAndOtherBenchmarksDoNot(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(storageRun(t, 3000, "1TB"), "a")
	l.add(storageRun(t, 6000, "2TB"), "b")
	page := l.page("/benchmarks")
	storage := `section[id="benchmark-storage-suite-1"]`
	if !page.hasText(t, storage+" .card-title", "storage · suite 1") || !page.hasText(t, storage+" th", "MB/s") || !page.hasText(t, storage+" tr.score-row .score-value", "6,000") {
		t.Error("the storage card")
	}
	var capacities []string
	for _, row := range scoreRows(t, page, "storage-suite-1") {
		capacities = append(capacities, row.sole(t, ".capacity-cell").words())
	}
	same(t, "the capacities, best first", capacities, []string{"2TB", "1TB"})
	if len(scoreRows(t, page, "opengl-suite-1")) != 1 || page.count(t, `section[id="benchmark-opengl-suite-1"] .capacity-cell`) != 0 {
		t.Error("another benchmark splits by capacity")
	}

	// A report without a capacity is accepted and shows as unknown.
	l = newPublicLab(t, nil)
	l.add(storageRun(t, 3000, ""), "a")
	if !l.page("/benchmarks").hasText(t, storage+" .capacity-cell", "unknown") {
		t.Error("a storage score without a capacity")
	}
}

func TestTheMacOSColumnShowsTheSameBoardsNumberWithItsConditions(t *testing.T) {
	l := newPublicLab(t, nil)
	l.withReference("j416c")
	l.add(storageRun(t, 3000, "1TB"), "a")
	l.add(storageRun(t, 6000, "2TB"), "b")
	page := l.page("/benchmarks")
	h264 := `section[id="benchmark-h264-decode-suite-1"] .macos-cell`
	if !page.says(t, h264, "900") || !page.says(t, h264, "macOS 26.6, on AC, brightness about 50%, 1 display, 2026-11-01") {
		t.Errorf("the macOS cell: %q", page.all(t, h264)[0].words())
	}
	// GL and Vulkan have no macOS column.
	if page.hasText(t, `section[id="benchmark-opengl-suite-1"] th`, "macOS") || page.count(t, `section[id="benchmark-vulkan-suite-1"] .macos-cell`) != 0 {
		t.Error("a macOS column where macOS has no counterpart")
	}
	// Storage shows macOS only at the same capacity.
	rows := scoreRows(t, page, "storage-suite-1")
	if !strings.Contains(rows[1].sole(t, ".macos-cell").words(), "6,000") || !strings.Contains(rows[0].sole(t, ".macos-cell").words(), "no macOS reference at this capacity") {
		t.Errorf("storage's macOS cells: %q, %q", rows[0].sole(t, ".macos-cell").words(), rows[1].sole(t, ".macos-cell").words())
	}
	// The export carries the capacity and the macOS number.
	for _, benchmark := range rowsOf(l.exported("/api/v1/benchmarks"), "benchmarks") {
		if benchmark["check"] == "benchmark.storage" {
			row := rowsOf(benchmark, "rows")[1]
			same(t, "the export's row", []any{row["capacity"], row["macos"], row["macos_conditions"]}, []any{"1TB", 6000.0, "macOS 26.6, on AC, brightness about 50%, 1 display, 2026-11-01"})
		}
	}

	// A board with no reference shows a dash.
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, benchmarkGolden), "a")
	if !l.page("/benchmarks").hasText(t, h264, "–") {
		t.Error("a board with no reference")
	}
}

func TestTheRunPageShowsTheSSDsCapacityAndEachScore(t *testing.T) {
	l := newPublicLab(t, nil)
	with := l.add(storageRun(t, 3000, "1TB"), "a")
	without := l.add(goldenDoc(t, benchmarkGolden), "b")
	if !l.page("/reports/"+with).hasText(t, "dd#storage", "1TB") || l.page("/reports/"+without).count(t, "dd#storage") != 0 {
		t.Error("the SSD's capacity on the run's page")
	}
	m1 := l.add(runWith(t, 0.6, "m1-pro-converged", 0), "c")
	page := l.page("/reports/" + m1)
	if !page.says(t, `li[id="check-benchmark.opengl"] .score`, "score 1,792 points (glmark2 2023.01, suite 1)") ||
		page.count(t, `li[id="check-benchmark.hevc-decode"] .score a[href="/benchmarks#benchmark-hevc-decode-suite-1"]`) != 1 {
		t.Errorf("the scores on the run's page: %q", page.sole(t, `li[id="check-benchmark.opengl"] .score`).words())
	}
}

func TestEachBenchmarkComparesMacsBestFirstEachMachineCountedOnceWithItsLatestScore(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(runWith(t, 0.5, "", 0), "a") // an older run on the same M2: replaced by the next
	l.add(goldenDoc(t, benchmarkGolden), "a")
	l.add(runWith(t, 1.1, "", 0), "c") // a second M2 Max
	l.add(runWith(t, 0.6, "m1-pro-converged", 0), "b")
	page := l.page("/benchmarks")
	if !page.hasText(t, `section[id="benchmark-opengl-suite-1"] .card-title`, "opengl · suite 1") {
		t.Error("the card's title")
	}
	rows := scoreRows(t, page, "opengl-suite-1")
	same(t, "the boards, best first", attrsOf(rows, "data-board"), []string{"j416c", "j314s"})
	m2, m1 := rows[0], rows[1]
	// The M2's two machines: 2987 and 3286 (the median is between them); the older 1494 run doesn't count.
	if m2.sole(t, ".score-value").words() != "3,136.5" || m2.sole(t, ".score-range").words() != "2,987–3,286" || !m2.hasText(t, "td", "2 (3 runs)") {
		t.Errorf("the M2's row: %q", m2.words())
	}
	if m2.sole(t, ".score-bar rect").attr("width") != "100.0" || m1.sole(t, ".score-bar rect").attr("width") != "57.1" || m1.sole(t, ".score-value").words() != "1,792" {
		t.Errorf("the bars: %q, %q", m2.sole(t, ".score-bar rect").attr("width"), m1.sole(t, ".score-bar rect").attr("width"))
	}
	if !m1.hasText(t, "td", "converged 4.0.0-alpha · build 99ace40.361571887310001 (linux-aurora 7.1.12.aurora2-10, omarchy-mac-boot 20260926-1.45)") ||
		m1.attr("data-build") != "99ace40.361571887310001" {
		t.Errorf("the M1's row: %q", m1.words())
	}
	if !page.hasText(t, `section[id="benchmark-h264-decode-suite-1"] tr.score-row .score-value`, "691") ||
		!page.hasText(t, `section[id="benchmark-vulkan-suite-1"] tr.score-row td.dim`, "vkmark 2025.01") || !page.hasText(t, ".site-nav a.current", "Benchmarks") {
		t.Error("the decode score, the tool or the current page")
	}
	if page.count(t, `a[href="/api/v1/benchmarks.json"]`) != 1 || page.count(t, "[style]") != 0 {
		t.Error("no link to benchmarks.json, or a style attribute")
	}

	// Scores from another suite are never compared with these.
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, benchmarkGolden), "a")
	l.add(runWith(t, 2.0, "", 2), "b")
	page = l.page("/benchmarks")
	if len(scoreRows(t, page, "opengl-suite-1")) != 1 || !page.hasText(t, `section[id="benchmark-opengl-suite-2"] tr.score-row .score-value`, "5,974") ||
		!page.hasText(t, `section[id="benchmark-opengl-suite-1"] tr.score-row .score-value`, "2,987") {
		t.Error("two suites are compared with each other")
	}
	same(t, "the newest suite first", attrsOf(page.all(t, "section.benchmark"), "id")[:2], []string{"benchmark-opengl-suite-2", "benchmark-opengl-suite-1"})
}

func TestRunsWithoutScoresAndHiddenReportsAddNothing(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-converged"), "a") // its benchmarks were skipped
	l.hideReport(l.add(goldenDoc(t, benchmarkGolden), "b"))
	page := l.page("/benchmarks")
	if page.count(t, ".score-row") != 0 || !page.says(t, ".card", "No report has a benchmark score yet") {
		t.Error("a run without scores or a hidden report added a score")
	}
}

func TestAModelPageShowsItsScoresAgainstTheBestMac(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, benchmarkGolden), "a")
	l.add(runWith(t, 0.6, "m1-pro-converged", 0), "b")
	page := l.page("/models/j314s")
	if got := page.count(t, "#benchmarks tr.score-row"); got != 4 {
		t.Errorf("%d score rows, want 4", got)
	}
	row := page.sole(t, `#benchmarks tr.score-row[data-benchmark="opengl-suite-1"]`)
	if row.sole(t, ".score-value").words() != "1,792" || row.sole(t, ".score-bar rect").attr("width") != "60.0" {
		t.Errorf("the model's opengl row: %q, bar %q", row.words(), row.sole(t, ".score-bar rect").attr("width"))
	}
	if l.page("/models/j416c").count(t, "#benchmarks tr.score-row") != 4 {
		t.Error("the M2's scores")
	}
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	if l.page("/models/j416c").count(t, "#benchmarks") != 0 {
		t.Error("a benchmarks card on a model without scores")
	}
}

func TestTheComparisonAndEveryScoreDownloadUnderCC0(t *testing.T) {
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, benchmarkGolden), "a")
	text := l.export("/api/v1/benchmarks.json")
	same(t, "the members", keysInOrder(t, text), []string{"license", "generated_at", "comparison", "benchmarks"})
	body := l.exported("/api/v1/benchmarks.json")
	if at(body, "license", "id") != "CC0-1.0" {
		t.Error("the licence")
	}
	tool := at(goldenDoc(t, benchmarkGolden), "tool", "version")
	for _, benchmark := range rowsOf(body, "benchmarks") {
		if benchmark["check"] != "benchmark.opengl" {
			continue
		}
		same(t, "the benchmark", []any{benchmark["name"], benchmark["suite"], benchmark["unit"]}, []any{"OpenGL", 1.0, "points"})
		rows := rowsOf(benchmark, "rows")
		if len(rows) != 1 {
			t.Fatalf("%d rows", len(rows))
		}
		want := doc{"model": "MacBook Pro (16-inch, M2 Max, 2023)", "board": "j416c", "soc": "t6021", "chip": "M2 Max",
			"stack": "converged", "version": "4.0.0-alpha", "build": doc{
				"id": "1937418.362376005140001", "label": "1937418.362376005140001", "words": "1937418.362376005140001 (linux-aurora 7.1.12.aurora2-10, omarchy-mac-boot 20260926-1)",
				"commit": "1937418", "stamp": "362376005140001", "linux_aurora": "7.1.12.aurora2-10", "omarchy_mac_boot": "20260926-1",
				"candidate_set": "apple-test-1937418f520b-20260926", "candidate_set_source": "packages",
				"image": doc{"format": "1", "platform": "apple-silicon"}, "tool_version": tool,
			}, "capacity": nil, "score": 2987.0, "low": 2987.0, "high": 2987.0, "machines": 1.0,
			"runs": 1.0, "tools": []any{"glmark2 2023.01"}, "macos": nil, "macos_conditions": nil}
		if !reflect.DeepEqual(rows[0], want) {
			got, _ := json.Marshal(rows[0])
			t.Errorf("the opengl row: %s", got)
		}
	}
	if strings.Contains(text, "key:a") {
		t.Error("benchmarks.json carries a Machine key")
	}
	for _, row := range byColumn(parseCSV(t, l.export("/api/v1/checks.csv"))) {
		if row["check_id"] == "benchmark.vulkan" {
			same(t, "checks.csv's score", []string{row["score"], row["score_unit"], row["score_tool"], row["score_suite"]}, []string{"6124", "points", "vkmark 2025.01", "1"})
		}
	}
	// Two machines that straddle the median: a number with a fraction, as the old site wrote it.
	l.add(runWith(t, 1.1, "", 0), "c")
	if text := l.export("/api/v1/benchmarks.json"); !strings.Contains(text, `"score":3136.5,"low":2987,"high":3286,"machines":2,"runs":2,`) {
		t.Error("the median of two machines")
	}
}

// What tester_gating_test.rb asks: Verified runs colour the matrix on their
// own, and a candidate set's page shows what they found per model and
// feature. Its case of a person taken off the list is not carried: a report
// keeps the verdict stamped when it came.
func m2Summary(t *testing.T, l *publicLab, feature string) *node {
	t.Helper()
	return summaryCell(t, l.page("/matrix"), feature, "converged")
}

func TestVerifiedRunsColourTheMatrixOnTheirOwnAndOutweighCommunityRuns(t *testing.T) {
	gpuFails := []string{"gpu.driver", "fails", "gpu.vulkan", "fails"}

	// One Verified run colours the cell on its own; a Community run does not.
	l := newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "a")
	if got := m2Summary(t, l, "gpu").attr("data-state"); got != "unconfirmed" {
		t.Errorf("one Community run: %q", got)
	}
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "verified:a")
	gpu := m2Summary(t, l, "gpu")
	if gpu.attr("data-state") != "works" || gpu.attr("data-verified") != "tester" || !strings.Contains(gpu.attr("class"), "cell-works cell-tester") ||
		!strings.Contains(gpu.attr("title"), "works: verified, 1 verified machine works") {
		t.Errorf("one Verified run: %v", gpu.attrs)
	}
	for _, row := range rowsOf(l.exported("/api/v1/matrix.json"), "rows") {
		if row["board"] == m2Board {
			same(t, "matrix.json's cell", at(row, "cells", "gpu"), any(doc{"state": "works", "tentative": "works", "machines": doc{"works": 1.0}, "tester_machines": doc{"works": 1.0}}))
		}
	}

	// Verified runs outweigh Community runs; a newer Verified pass retires an older Verified failure.
	l = newPublicLab(t, nil)
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "verified:a")
	l.add(goldenDoc(t, "m2-max-image2"), "b")
	l.add(goldenDoc(t, "m2-max-image2"), "c")
	if gpu = m2Summary(t, l, "gpu"); gpu.attr("data-state") != "fails" || !strings.Contains(gpu.attr("title"), "; 3 machines in all") {
		t.Errorf("a Verified failure against two Community passes: %v", gpu.attrs)
	}
	l.add(goldenDoc(t, "m2-max-image2"), "verified:d")
	if got := m2Summary(t, l, "gpu").attr("data-state"); got != "works" {
		t.Errorf("after a newer Verified pass: %q", got)
	}

	// A newer Verified failure stands: an older pass doesn't retire it, so the machines disagree.
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "verified:a")
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "verified:d")
	if got := m2Summary(t, l, "gpu").attr("data-state"); got != "partial" {
		t.Errorf("a newer Verified failure: %q", got)
	}
	// A machine that fails after it passed can't retire another machine's failure.
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "verified:a")
	if got := m2Summary(t, l, "gpu").attr("data-state"); got != "regression" {
		t.Errorf("both machines failing after a pass: %q", got)
	}

	// A machine's latest run decides: a Community run after its Verified run takes the colour away.
	l = newPublicLab(t, nil)
	l.add(goldenDoc(t, "m2-max-image2"), "verified:a")
	l.store("later", goldenWith(t, "m2-max-image2", gpuFails...), "key:verified:a", l.at, false)
	if gpu = m2Summary(t, l, "gpu"); gpu.attr("data-state") != "unconfirmed" || gpu.attr("data-verified") != "" {
		t.Errorf("a Community run after the machine's Verified run: %v", gpu.attrs)
	}
}

func TestACandidateSetsPageShowsWhatVerifiedRunsFoundPerModelAndFeature(t *testing.T) {
	const set = "edge-2026.09.27"
	gpuFails := []string{"gpu.driver", "fails", "gpu.vulkan", "fails"}

	// Only Community runs: the set waits.
	l := newPublicLab(t, nil)
	first := l.add(onSet(goldenDoc(t, "m2-max-image2"), set), "a")
	l.add(onSet(goldenDoc(t, "m2-max-image2"), set), "b")
	if l.page("/candidates").count(t, `tr.candidate-row[id="candidate-`+set+`"][data-verdict="waiting"]`) != 1 {
		t.Error("the list does not show the set waiting")
	}
	page := l.page("/candidates/" + set)
	if !page.hasText(t, "#verdict", "Waiting") || page.count(t, "tr.matrix-row") != 0 || page.count(t, "tr.report-row") != 2 {
		t.Error("a set with Community runs only")
	}
	// The report's page links its candidate set.
	if !l.page("/reports/"+first).hasText(t, `a[href="/candidates/`+set+`"]`, set) {
		t.Error("the report does not link its set")
	}
	// Hidden reports don't count.
	l.exec(`UPDATE reports SET hidden_at = ?`, l.at.UnixMicro())
	if l.status("/candidates/"+set) != 404 || l.page("/candidates").count(t, "tr.candidate-row") != 0 {
		t.Error("hidden reports still make a set")
	}

	// Verified runs with every feature working: the view is per model and feature, Verified runs only.
	l = newPublicLab(t, nil)
	l.add(onSet(working(t, "m2-max-image2"), set), "verified:a")
	l.add(onSet(goldenWith(t, "m2-max-image2", gpuFails...), set), "b")
	l.add(goldenWith(t, "m2-max-image2", gpuFails...), "verified:a")
	page = l.page("/candidates/" + set)
	if !page.hasText(t, `#verdict[data-verdict="ready"]`, "Passing: Verified runs on 1 model, nothing failing") {
		t.Errorf("the verdict: %q", page.sole(t, "#verdict").words())
	}
	if page.count(t, `tr.matrix-row[data-board="`+m2Board+`"] td[data-feature="gpu"][data-state="works"][data-verified="tester"]`) != 1 ||
		page.count(t, "tr.report-row") != 2 || page.count(t, "tr.report-row .badge-tester") != 1 {
		t.Error("the set's matrix or its runs")
	}

	// A Verified failure blocks the set and is listed.
	l = newPublicLab(t, nil)
	l.add(onSet(working(t, "m2-max-image2"), set), "verified:a")
	l.add(onSet(working(t, "m1-pro-mx-mac", gpuFails...), set), "verified:b")
	page = l.page("/candidates/" + set)
	if !page.hasText(t, `#verdict[data-verdict="blocked"]`, "Failing: 1 failure, 0 regressions") || page.count(t, `#failures li.finding[data-state="fails"]`) != 1 {
		t.Errorf("the verdict: %q", page.sole(t, "#verdict").words())
	}
	if finding := page.sole(t, "#failures li.finding").words(); !strings.Contains(finding, "MacBook Pro") || !strings.Contains(finding, "GPU") {
		t.Errorf("the finding: %q", finding)
	}
	if l.page("/candidates").count(t, `tr.candidate-row[data-verdict="blocked"]`) != 1 {
		t.Error("the list does not show the set failing")
	}
}
