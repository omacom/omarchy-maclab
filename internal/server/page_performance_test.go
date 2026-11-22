package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/perf"
	"github.com/omacom/omarchy-maclab/internal/reports"
)

// perfFixture is what reports.db holds of Performance for the page tests:
//
//   - j314s 10c-16g: a Lab-owned Mac (512 GB) and a Counted one (1 TB) on
//     macOS 27.2.1, a Lab-owned one still on 27.1, and a Volunteer's Mac;
//     three Releases, the newest with no verdict yet, its two Macs booting
//     different Mesa.
//   - j313 8c-7g: a Volunteer's Mac alone.
//   - j274 8c-8g: a Lab-owned Mac with Omarchy values and no macOS result.
type perfFixture struct {
	p   reports.Performance
	at  time.Time
	key func(byte) string
}

// perfQuick are bench-1's Quick metrics, one per category, with the macOS
// value each Mac gets.
var perfQuick = []struct {
	metric string
	macOS  float64
}{
	{"coremark-single", 30000}, {"motionmark", 4000}, {"fio-seq-read", 5000}, {"stream-triad", 180},
	{"speedometer", 28}, {"decode-h264-1080p", 900}, {"neural-engine-mobilenet-v1", 2000}, {"power-idle", 4},
}

func newPerfFixture() *perfFixture {
	f := &perfFixture{at: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC), key: func(c byte) string { return strings.Repeat(string(c), 32) }}
	k := f.key
	f.p.Macs = []reports.PerfMac{
		{Key: k('a'), Kind: reports.PerfLabOwned, Board: "j314s", CPU: 10, GPU: 16},
		{Key: k('b'), Kind: reports.PerfCounted, Board: "j314s", CPU: 10, GPU: 16},
		{Key: k('c'), Kind: reports.PerfLabOwned, Board: "j314s", CPU: 10, GPU: 16},
		{Key: k('d'), Kind: reports.PerfVolunteer, Board: "j314s", CPU: 10, GPU: 16},
		{Key: k('e'), Kind: reports.PerfVolunteer, Board: "j313", CPU: 8, GPU: 7},
		{Key: k('f'), Kind: reports.PerfLabOwned, Board: "j274", CPU: 8, GPU: 8},
	}
	f.p.Releases = []reports.PerfRelease{
		{Key: k('1'), Kernel: "6.18.0-1", Verdict: reports.PerfQualified, Position: 1},
		{Key: k('2'), Kernel: "6.18.1-1", Verdict: reports.PerfQualified, Position: 2},
		{Key: k('3'), Kernel: "6.18.2-1", Verdict: reports.PerfNoVerdictYet, Position: 3},
	}
	capacity := map[byte]int64{'a': 512, 'b': 1000, 'c': 512, 'd': 1000, 'e': 256, 'f': 256}
	build := map[byte][2]string{'a': {"27.2.1", "27C101"}, 'b': {"27.2.1", "27C101"}, 'c': {"27.1", "27B5"}, 'd': {"27.2.1", "27C101"}, 'e': {"27.2.1", "27C101"}}
	// share is Omarchy's value as a share of macOS's, per Release.
	share := map[byte]float64{'1': 0.70, '2': 0.80, '3': 0.85}
	for _, mac := range []byte{'a', 'b', 'c', 'd', 'e', 'f'} {
		for _, m := range perfQuick {
			var gb *int64
			if m.metric == "fio-seq-read" {
				c := capacity[mac]
				gb = &c
			}
			if b, ok := build[mac]; ok {
				f.value(mac, perf.SideMacOS, 0, m.metric, m.macOS, gb, b, nil)
			}
			for _, r := range []byte{'1', '2', '3'} {
				if r == '1' && mac != 'a' && mac != 'b' {
					continue
				}
				v := m.macOS * share[r]
				if m.metric == "power-idle" {
					v = m.macOS / share[r]
				}
				if mac == 'd' || mac == 'e' {
					v *= 0.9
				}
				booted := map[string]string{"linux-asahi": "6.18." + string(r-1), "mesa": "26.0.3"}
				switch {
				case r == '1':
					booted["mesa"] = "26.0.2"
				case r == '3' && mac == 'b':
					booted["mesa"] = "26.1.0"
				}
				f.value(mac, perf.SideOmarchy, r, m.metric, v, gb, [2]string{}, booted)
			}
		}
		for _, r := range []byte{'2', '3'} {
			if mac == 'a' || mac == 'b' {
				f.value(mac, perf.SideOmarchy, r, "boot-time", map[byte]float64{'2': 10, '3': 9.5}[r], nil, [2]string{}, nil)
			}
		}
	}
	return f
}

func (f *perfFixture) value(mac byte, side string, release byte, metric string, v float64, capacity *int64, macOS [2]string, booted map[string]string) {
	pv := reports.PerfValue{Mac: f.key(mac), Set: "bench-1", Side: side, Metric: metric, Value: &v, Capacity: capacity, Booted: booted, Measured: f.at}
	if side == perf.SideMacOS {
		pv.MacOSVersion, pv.MacOSBuild = macOS[0], macOS[1]
	} else {
		pv.Release = f.key(release)
	}
	f.p.Values = append(f.p.Values, pv)
}

func perfPagesLab(t *testing.T) (*publicLab, *perfFixture) {
	t.Helper()
	l := newPublicLab(t, nil)
	f := newPerfFixture()
	if _, err := l.s.reports.PublishPerformance(f.p, f.at); err != nil {
		t.Fatal(err)
	}
	return l, f
}

// noMacNamed fails when a page carries any Mac's or Release's opaque key, or
// an inline style the content security policy would refuse.
func noMacNamed(t *testing.T, l *publicLab, f *perfFixture, target string) {
	t.Helper()
	code, body, _ := l.get(target)
	if code != http.StatusOK {
		t.Fatalf("GET %s: %d", target, code)
	}
	for _, c := range []byte("abcdef123") {
		if strings.Contains(body, f.key(c)) {
			t.Errorf("%s carries the key %s", target, f.key(c))
		}
	}
	if page := parseHTML(t, body); page.count(t, "[style]") != 0 {
		t.Errorf("%s has a style attribute", target)
	}
}

func TestThePerformanceOverviewGroupsConfigurationsByChipFamily(t *testing.T) {
	l, f := perfPagesLab(t)
	noMacNamed(t, l, f, "/performance")
	page := l.page("/performance")
	if !page.hasText(t, ".site-nav a.current", "Performance") || page.count(t, `.site-nav a[href="/benchmarks"]`) != 0 {
		t.Error("row 1 does not have Performance in place of Benchmarks")
	}
	if page.count(t, `select[name="release"] option[selected][value="6.18.1-1"]`) != 1 || page.count(t, `select[name="set"] option[selected][value="bench-1"]`) != 1 ||
		page.count(t, `input[type="checkbox"][name="volunteers"][checked]`) != 1 {
		t.Error("the pickers: the newest qualified Release, the set and Show Volunteer Macs on")
	}

	family := func(name string) *node {
		for _, d := range page.all(t, "details.perf-family") {
			if d.sole(t, ".perf-family-name").words() == name {
				return d
			}
		}
		t.Fatalf("no family %s", name)
		return nil
	}
	pro := family("M1 Pro/Max/Ultra")
	if !pro.has["open"] {
		t.Error("a family with runs is closed")
	}
	row := rowLinking(t, pro, "/performance/j314s/10c-16g")
	if !row.says(t, "td", "2 Lab-owned · 1 Counted · 1 Volunteer") || !row.says(t, "td", "macOS 27.2.1 and 27.1") || !row.says(t, "td", "SSD 512 GB · 1 TB") {
		t.Errorf("the j314s row: %q", row.words())
	}
	if !row.hasText(t, ".perf-overall .perf-text", "80%") || row.sole(t, ".perf-overall rect.perf-orange").attr("width") != "80.0" ||
		!row.says(t, ".perf-overall", "of macOS 27.2.1") || row.count(t, "td.perf-num .perf-text") != 8 {
		t.Errorf("the j314s row's figures: %q", row.words())
	}
	if pro.count(t, "line.perf-tick[x1=\"100\"]") == 0 {
		t.Error("no tick at 100%")
	}

	m1 := family("M1")
	volunteer := rowLinking(t, m1, "/performance/j313/8c-7g")
	if !volunteer.hasText(t, ".badge-volunteer", "Volunteer only") || !volunteer.hasText(t, ".perf-overall .perf-text", "74%") || !volunteer.hasText(t, "td.perf-num .perf-text", "72%") {
		t.Errorf("the Volunteer-only row: %q", volunteer.words())
	}
	waiting := rowLinking(t, m1, "/performance/j274/8c-8g")
	if !waiting.says(t, "td", "Its Macs wait for their macOS result.") {
		t.Errorf("a Configuration with no macOS result: %q", waiting.words())
	}
	unmeasured := rowNamed(t, m1, "j293")
	if unmeasured.count(t, `a[href="/lend"]`) != 1 || !unmeasured.says(t, ".row-meta", "j293 · 8 CPU · 8 GPU") || !unmeasured.says(t, "td", "No runs yet.") {
		t.Errorf("a Configuration with no runs: %q", unmeasured.words())
	}
	if !strings.Contains(family("M2").words(), "0 with runs") {
		t.Error("the M2 family's header")
	}
	for _, d := range page.all(t, "details.perf-family") {
		open := d.has["open"]
		withRuns := !strings.Contains(d.sole(t, "summary").words(), " 0 with runs")
		if open != withRuns {
			t.Errorf("%s: open %v, with runs %v", d.sole(t, ".perf-family-name").words(), open, withRuns)
		}
	}

	gaps := page.all(t, ".perf-lab-gaps .perf-gaps li")
	if len(gaps) == 0 || !gaps[0].says(t, ".perf-gap-where", "MacBook") {
		t.Fatalf("the biggest gaps across the lab: %d", len(gaps))
	}
	if page.count(t, `a[href="/api/v1/performance.json"]`) != 1 {
		t.Error("no link to performance.json")
	}
	if page.count(t, "table") != page.count(t, "div.scroll table") {
		t.Error("a table outside a box that scrolls")
	}
}

func TestHidingVolunteerMacsHidesAVolunteerOnlyHeadline(t *testing.T) {
	l, f := perfPagesLab(t)
	noMacNamed(t, l, f, "/performance?volunteers=false")
	page := l.page("/performance?volunteers=false&volunteers=false")
	if page.count(t, `input[type="checkbox"][name="volunteers"][checked]`) != 0 || page.count(t, ".badge-volunteer") != 0 ||
		page.count(t, `a[href^="/performance/j313/"]`) != 0 {
		t.Error("a Volunteer-only headline shows with Volunteers hidden")
	}
	row := rowLinking(t, page, "/performance/j314s/10c-16g?volunteers=false")
	if !row.says(t, "td", "2 Lab-owned · 1 Counted") || strings.Contains(row.words(), "Volunteer") {
		t.Errorf("the j314s row with Volunteers hidden: %q", row.words())
	}
	// The form's hidden field and its checkbox: the checkbox, last, says.
	if page := l.page("/performance?volunteers=false&volunteers=true"); page.count(t, ".badge-volunteer") == 0 {
		t.Error("the checkbox ticked does not show Volunteers")
	}
	if code, _, _ := l.get("/performance/j313/8c-7g?volunteers=false"); code != http.StatusNotFound {
		t.Errorf("the Volunteer-only Configuration with Volunteers hidden: %d", code)
	}
}

func TestAConfigurationsPageShowsItsColumnsBuildsCapacitiesAndVolunteers(t *testing.T) {
	l, f := perfPagesLab(t)
	target := "/performance/j314s/10c-16g"
	noMacNamed(t, l, f, target)
	page := l.page(target)
	if !page.hasText(t, "h1", "MacBook Pro (14-inch, M1 Pro, 2021)") || !page.says(t, ".crumb", "10-core CPU · 16-core GPU") || page.count(t, `.crumb a[href="/models/j314s"]`) != 1 {
		t.Errorf("the title: %q", page.sole(t, "h1").words())
	}
	facts := page.sole(t, ".perf-facts")
	if !facts.says(t, ".perf-fact", "macOS 27.2.1 build 27C101") || !facts.says(t, ".perf-fact", "measured 2026-11-18 · 2 Macs paired") ||
		!facts.says(t, ".perf-fact", "1 Mac on macOS 27.1 (27B5), shown apart: it waits for its macOS run on the newer build") {
		t.Errorf("the macOS facts: %q", facts.words())
	}
	if !page.says(t, "#perf-summary-title", "6.18.1-1 against macOS 27.2.1 · overall 80%") || page.count(t, ".perf-tiles .perf-tile") != 8 {
		t.Errorf("the summary: %q", page.sole(t, "#perf-summary-title").words())
	}

	cpu := page.sole(t, "details#category-cpu")
	if !cpu.has["open"] {
		t.Error("CPU is closed")
	}
	for id, open := range map[string]bool{"category-gpu": true, "category-ai": true, "category-storage": false, "category-power": false} {
		if got := page.sole(t, "details#"+id).has["open"]; got != open {
			t.Errorf("%s: open %v", id, got)
		}
	}
	heads := cpu.all(t, "thead th")
	var names []string
	for _, h := range heads[1:] {
		names = append(names, h.sole(t, ".mono").words())
	}
	same(t, "the columns", names, []string{"macOS 27.2.1", "6.18.2-1", "6.18.1-1", "6.18.0-1"})
	if !heads[2].hasText(t, ".badge-no-verdict", "no verdict yet") || strings.Contains(heads[2].attr("class"), "perf-current") {
		t.Errorf("the no-verdict column and the highlighted one: %q, %q", heads[2].attr("class"), heads[3].attr("class"))
	}
	if !strings.Contains(heads[3].attr("class"), "perf-current") {
		t.Error("the newest qualified Release is not highlighted")
	}
	if !heads[2].says(t, ".perf-flag", "its Macs booted different mesa") {
		t.Errorf("the no-verdict column does not say its Macs booted different Mesa: %q", heads[2].words())
	}
	if !heads[3].hasText(t, ".perf-flag", "other mesa than 6.18.0-1") || heads[4].count(t, ".perf-flag") != 0 {
		t.Errorf("the column whose Mesa changed from the one before: %q, %q", heads[3].words(), heads[4].words())
	}
	if cpu.hasText(t, "th", "RC") || page.says(t, "main", "not qualified") || page.says(t, "main", "baseline") {
		t.Error("the mock-up's words")
	}

	core := cpu.all(t, "tr.perf-metric")[0]
	cells := core.all(t, "td")
	if !core.says(t, "td", "CoreMark, one core") || cells[1].words() != "30,000" || !cells[3].says(t, ".perf-text", "80%") || !cells[2].says(t, ".perf-text", "85%") {
		t.Errorf("CoreMark's row: %q", core.words())
	}
	if !cells[2].says(t, ".perf-trend", "+5 pts") {
		t.Errorf("the no-verdict column's trend: %q", cells[2].words())
	}
	line := cpu.sole(t, "tr.perf-volunteer-line")
	if !line.hasText(t, ".badge-volunteer", "Volunteer") || !line.says(t, "td", "1 Volunteer Mac · macOS 27.2.1") || !line.says(t, ".perf-cell .perf-text", "72%") {
		t.Errorf("the Volunteer line: %q", line.words())
	}
	if core.sole(t, "button[data-volunteer-line]").attr("aria-controls") != line.attr("id") {
		t.Error("the Volunteer line's button")
	}

	storage := page.sole(t, "details#category-storage")
	var groups []string
	for _, g := range storage.all(t, "tr.perf-group th") {
		groups = append(groups, g.words())
	}
	same(t, "Storage per SSD capacity", groups, []string{"SSD 512 GB", "SSD 1 TB"})
	if storage.count(t, "tr.perf-metric") != 8 {
		t.Errorf("%d Storage rows, want bench-1's four per capacity", storage.count(t, "tr.perf-metric"))
	}

	if !page.says(t, "#omarchy-only", "Boot time") || !page.says(t, "#omarchy-only", "9.50 s") || !page.says(t, "#omarchy-only", "−5.0% on the Release before") {
		t.Errorf("Omarchy only: %q", page.sole(t, "#omarchy-only").words())
	}
	stacks := page.sole(t, "details#stacks")
	var packages, newest []string
	for _, th := range stacks.all(t, "thead th") {
		packages = append(packages, th.words())
	}
	for _, td := range stacks.all(t, "tbody tr")[0].all(t, "td") {
		newest = append(newest, td.words())
	}
	same(t, "the Stacks' packages", packages, []string{"Release", "linux-asahi", "mesa"})
	same(t, "what 6.18.2-1 booted", newest, []string{"6.18.2-1", "6.18.2", "differs between its Macs"})
	if page.count(t, "table") != page.count(t, "div.scroll table") {
		t.Error("a table outside a box that scrolls")
	}
	if strings.Contains(page.words(), "of macOS on") || strings.Contains(page.words(), "of macOS ·") {
		t.Error("an \"of macOS\" that names no version")
	}
}

func TestAVolunteerOnlyConfigurationSaysSoAndUnknownOnesAreNotFound(t *testing.T) {
	l, _ := perfPagesLab(t)
	page := l.page("/performance/j313/8c-7g")
	if !page.hasText(t, ".perf-facts .badge-volunteer", "Volunteer only") || page.count(t, "tr.perf-volunteer-line") != 0 {
		t.Error("the Volunteer-only Configuration")
	}
	for _, target := range []string{"/performance/j314s/8c-14g", "/performance/j314s/10-16", "/performance/nope/1c-1g", "/performance?set=99"} {
		if code, _, _ := l.get(target); code != http.StatusNotFound {
			t.Errorf("GET %s: %d", target, code)
		}
	}
	// A Release the set does not have falls back to the newest qualified.
	if page := l.page("/performance?release=9.9"); page.count(t, `select[name="release"] option[selected][value="6.18.1-1"]`) != 1 {
		t.Error("an unknown Release")
	}
	// The no-verdict Release picked.
	page = l.page("/performance?release=6.18.2-1")
	if !rowLinking(t, page, "/performance/j314s/10c-16g?release=6.18.2-1").hasText(t, ".perf-overall .perf-text", "85%") {
		t.Error("the no-verdict Release's figures")
	}
}

func TestBenchmarksRedirectsToPerformanceAndModelsLinkTheirConfigurations(t *testing.T) {
	l, _ := perfPagesLab(t)
	w := l.do("GET", "/benchmarks", "")
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/performance" {
		t.Errorf("GET /benchmarks: %d %s", w.Code, w.Header().Get("Location"))
	}
	l.add(runWith(t, 0.6, "m1-pro-converged", 0), "b")
	page := l.page("/models/j314s")
	if page.count(t, `a[href="/performance/j314s/10c-16g"]`) != 1 || !page.says(t, ".card", "80% of macOS 27.2.1 on 6.18.1-1") {
		t.Error("the model's page does not link its Configuration's Performance page")
	}
	if page.count(t, `a[href^="/benchmarks"]`) != 0 {
		t.Error("the model's page links the old Benchmarks page")
	}
}

// rowNamed is the one Configuration row under n of that board.
func rowNamed(t *testing.T, n *node, board string) *node {
	t.Helper()
	var found []*node
	for _, row := range n.all(t, "tr.perf-config-row") {
		if strings.HasPrefix(row.sole(t, ".row-meta.mono").words(), board+" ") || row.sole(t, ".row-meta.mono").words() == board {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d rows of %s, want 1", len(found), board)
	}
	return found[0]
}

// rowLinking is the one Configuration row under n with a link to href.
func rowLinking(t *testing.T, n *node, href string) *node {
	t.Helper()
	var found []*node
	for _, row := range n.all(t, "tr.perf-config-row") {
		if row.count(t, `a[href="`+href+`"]`) > 0 {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d rows link %s, want 1", len(found), href)
	}
	return found[0]
}
