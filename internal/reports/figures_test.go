package reports

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

// perfLab builds what reports.db would hold of Performance, value by value,
// on bench-1.
type perfLab struct {
	t   *testing.T
	set *sets.Set
	p   Performance
	at  time.Time
}

func newPerfLab(t *testing.T) *perfLab {
	t.Helper()
	set, err := sets.Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	return &perfLab{t: t, set: set, at: time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)}
}

func (l *perfLab) mac(c byte, kind, board string, cpu, gpu int) string {
	l.p.Macs = append(l.p.Macs, PerfMac{Key: perfKey(c), Kind: kind, Board: board, CPU: cpu, GPU: gpu})
	return perfKey(c)
}

func (l *perfLab) release(c byte, kernel, verdict string) string {
	l.p.Releases = append(l.p.Releases, PerfRelease{Key: perfKey(c), Kernel: kernel, Verdict: verdict, Position: len(l.p.Releases) + 1})
	return perfKey(c)
}

func (l *perfLab) value(mac, side, release, build, metric string, value float64, reason string, capacity int64) {
	m, ok := l.set.Metric(metric)
	if !ok {
		l.t.Fatalf("no metric %s", metric)
	}
	v := PerfValue{Mac: mac, Set: l.set.Name, Side: side, Metric: metric, Unit: m.Unit, Measured: l.at}
	if side == "omarchy" {
		v.Release = release
	} else {
		v.MacOSBuild, v.MacOSVersion = build, macOSVersions[build]
	}
	if reason != "" {
		v.Reason = &reason
	} else {
		v.Value = &value
	}
	if m.Category == "Storage" {
		v.Capacity = &capacity
	}
	for i, old := range l.p.Values {
		if old.key() == v.key() {
			l.p.Values[i] = v
			return
		}
	}
	l.p.Values = append(l.p.Values, v)
}

var macOSVersions = map[string]string{"27C101": "27.2.1", "27B74": "27.1", "27C5": "27.2"}

// macOS gives a Mac a macOS result at build: 100 of everything.
func (l *perfLab) macOS(mac, build string, capacity int64) {
	for _, m := range l.set.Metrics {
		if m.MacOS != nil {
			l.value(mac, "macos", "", build, m.ID, 100, "", capacity)
		}
	}
}

// omarchy gives a Mac values on release at ratio percent of macOS's 100 for
// every row, and a boot time of 20 s.
func (l *perfLab) omarchy(mac, release string, ratio float64, capacity int64) {
	for _, m := range l.set.Metrics {
		switch {
		case m.Category == sets.OmarchyOnly:
			l.value(mac, "omarchy", release, "", m.ID, 20, "", 0)
		default:
			l.ratio(mac, release, m.ID, ratio, capacity)
		}
	}
}

// ratio sets one Omarchy value so that its ratio to macOS's 100 is ratio.
func (l *perfLab) ratio(mac, release, metric string, ratio float64, capacity int64) {
	m, _ := l.set.Metric(metric)
	value := ratio
	if !m.HigherIsBetter() {
		value = 100 * 100 / ratio
	}
	l.value(mac, "omarchy", release, "", metric, value, "", capacity)
}

func (l *perfLab) reason(mac, release, metric, reason string) {
	l.value(mac, "omarchy", release, "", metric, 0, reason, 512)
}

func (l *perfLab) figures(volunteers bool) *PerfFigures {
	l.t.Helper()
	if _, errs := l.p.Pruned(); len(errs) > 0 {
		l.t.Fatalf("not publishable: %v", errs)
	}
	return l.p.Figures(l.set, testCatalogue(l.t), volunteers)
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-9 }

func show(f *float64) string {
	if f == nil {
		return "none"
	}
	return fmt.Sprint(*f)
}

func perfRowOf(t *testing.T, col *PerfColumn, metric string, capacity int64) *PerfRow {
	t.Helper()
	for _, r := range col.Rows {
		if r.Metric == metric && (r.Capacity == nil || *r.Capacity == capacity) {
			return r
		}
	}
	t.Fatalf("no row %s at %d GB", metric, capacity)
	return nil
}

func perfCategoryOf(t *testing.T, col *PerfColumn, name string) *PerfCategory {
	t.Helper()
	for _, c := range col.Categories {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no category %s", name)
	return nil
}

func TestAMacsRatioIsAPercentOfMacOSAndPowerIsInverted(t *testing.T) {
	l := newPerfLab(t)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	r := l.release('f', "6.18.2-1", PerfQualified)
	l.macOS(a, "27C101", 512)
	l.omarchy(a, r, 100, 512)
	l.value(a, "omarchy", r, "", "coremark-single", 90, "", 0)
	l.value(a, "omarchy", r, "", "power-idle", 8, "", 0)
	l.value(a, "macos", "", "27C101", "power-idle", 6, "", 0)
	l.value(a, "omarchy", r, "", "power-all-core", 50, "", 0)
	l.value(a, "omarchy", r, "", "speedometer", 50, "", 0)
	l.value(a, "omarchy", r, "", "stream-triad", 103, "", 0)
	col := l.figures(true).Configurations[0].Column(r)
	for _, c := range []struct {
		metric string
		ratio  float64
		colour string
	}{
		{"coremark-single", 90, PerfYellow},
		{"power-idle", 75, PerfOrange},
		{"power-all-core", 200, PerfGreen},
		{"speedometer", 50, PerfRed},
		{"stream-triad", 103, PerfGrey},
		{"coremark-all", 100, PerfGrey},
	} {
		row := perfRowOf(t, col, c.metric, 0)
		if !near(row.Ratio, c.ratio) || row.Colour != c.colour || row.Macs != 1 {
			t.Errorf("%s: %s%% %s over %d Macs, want %v%% %s", c.metric, show(row.Ratio), row.Colour, row.Macs, c.ratio, c.colour)
		}
	}
	if row := perfRowOf(t, col, "power-idle", 0); !near(row.Omarchy, 8) || !near(row.MacOS, 6) {
		t.Errorf("power-idle's values beside it: %s and %s", show(row.Omarchy), show(row.MacOS))
	}
	for _, colour := range []struct {
		ratio, tolerance float64
		want             string
	}{{105, 5, PerfGrey}, {95, 5, PerfGrey}, {105.1, 5, PerfGreen}, {94.9, 5, PerfYellow}, {85, 5, PerfYellow}, {84.9, 5, PerfOrange}, {60, 5, PerfOrange}, {59.9, 5, PerfRed}} {
		if got := perfColour(colour.ratio, colour.tolerance); got != colour.want {
			t.Errorf("%v%% within %v%%: %s, want %s", colour.ratio, colour.tolerance, got, colour.want)
		}
	}
}

func TestACategoryIsTheGeometricMeanOfItsRowsAndOverallOfTheEight(t *testing.T) {
	l := newPerfLab(t)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	r := l.release('f', "6.18.2-1", PerfQualified)
	l.macOS(a, "27C101", 512)
	l.omarchy(a, r, 100, 512)
	l.ratio(a, r, "coremark-single", 50, 0)
	l.ratio(a, r, "coremark-all", 100, 0)
	l.ratio(a, r, "7zip", 200, 0)
	l.ratio(a, r, "stream-triad", 64, 0)
	l.ratio(a, r, "stream-copy", 100, 0)
	l.reason(a, r, "mlx-matmul-fp16", "no_build")
	col := l.figures(true).Configurations[0].Column(r)
	if cpu := perfCategoryOf(t, col, "CPU"); !near(cpu.Figure, 100) || cpu.Worst.Metric != "coremark-single" {
		t.Errorf("CPU: %s, worst %s", show(cpu.Figure), cpu.Worst.Metric)
	}
	if mem := perfCategoryOf(t, col, "Memory"); !near(mem.Figure, 80) {
		t.Errorf("Memory: %s", show(mem.Figure))
	}
	ai := perfCategoryOf(t, col, "AI")
	if !near(ai.Figure, 100) || len(ai.Excluded) != 1 || ai.Excluded[0] != (PerfExcluded{Metric: "mlx-matmul-fp16", Reason: "no_build"}) || ai.Coverage != (PerfCoverage{4, 5}) {
		t.Errorf("AI: %s, excluded %+v, coverage %+v", show(ai.Figure), ai.Excluded, ai.Coverage)
	}
	if want := math.Pow(80, 1.0/8) * math.Pow(100, 7.0/8); !near(col.Overall, want) || col.Coverage != (PerfCoverage{23, 24}) || len(col.Excluded) != 1 {
		t.Errorf("Overall %s, want %v; coverage %+v; excluded %+v", show(col.Overall), want, col.Coverage, col.Excluded)
	}

	// A category with no row left leaves Overall blank.
	l.reason(a, r, "decode-h264-1080p", "no_driver")
	l.reason(a, r, "decode-hevc-4k", "fell_back")
	col = l.figures(true).Configurations[0].Column(r)
	if video := perfCategoryOf(t, col, "Video"); video.Figure != nil || len(video.Excluded) != 2 || video.Coverage != (PerfCoverage{0, 2}) {
		t.Errorf("Video: %s, excluded %+v", show(video.Figure), video.Excluded)
	}
	if col.Overall != nil || col.Coverage != (PerfCoverage{21, 24}) {
		t.Errorf("Overall %s with Video empty; coverage %+v", show(col.Overall), col.Coverage)
	}
}

func TestAConfigurationsCellIsTheMedianOfItsHeadlineMacsAndVolunteersAreALineApart(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfQualified)
	for i, c := range []struct {
		key   byte
		kind  string
		ratio float64
	}{{'a', PerfLabOwned, 80}, {'b', PerfCounted, 90}, {'c', PerfLabOwned, 120}, {'d', PerfVolunteer, 10}, {'e', PerfVolunteer, 30}} {
		mac := l.mac(c.key, c.kind, "j314s", 10, 16)
		l.macOS(mac, "27C101", 512)
		l.omarchy(mac, r, c.ratio, 512)
		l.value(mac, "macos", "", "27C101", "coremark-single", float64(100*(i+1)), "", 0)
		l.value(mac, "omarchy", r, "", "coremark-single", float64(100*(i+1))*c.ratio/100, "", 0)
	}
	c := l.figures(true).Configurations[0]
	if c.LabOwned != 2 || c.Counted != 1 || c.Volunteer != 2 || c.VolunteerHeadline || c.Headline.Volunteer {
		t.Fatalf("the Macs by kind: %+v", c)
	}
	cohort := c.Comparison()
	col := c.Column(r)
	row := perfRowOf(t, col, "coremark-single", 0)
	if cohort.MacOSBuild != "27C101" || cohort.MacOSVersion != "27.2.1" || cohort.Macs != 3 || col.Macs != 3 || !near(row.Ratio, 90) || !near(row.MacOS, 200) || !near(row.Omarchy, 180) || row.Macs != 3 {
		t.Errorf("the headline: %s %s over %d Macs; coremark %s%% of %s, Omarchy %s", cohort.MacOSVersion, cohort.MacOSBuild, cohort.Macs, show(row.Ratio), show(row.MacOS), show(row.Omarchy))
	}
	if line := c.Line(r, row); line == nil || !near(line.Ratio, 20) || line.Macs != 2 || !c.Volunteers.Volunteer {
		t.Errorf("the Volunteer line: %+v", line)
	}
	if hidden := l.figures(false).Configurations[0]; hidden.Volunteer != 0 || hidden.Volunteers != nil || hidden.Comparison().Macs != 3 {
		t.Errorf("Volunteers hidden: %+v", hidden)
	}
}

func TestAVolunteerOnlyConfigurationShowsItsVolunteersMarkedAndHidesWithThem(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfNoVerdictYet)
	v := l.mac('d', PerfVolunteer, "j313", 8, 7)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	for _, mac := range []string{v, a} {
		l.macOS(mac, "27C101", 256)
		l.omarchy(mac, r, 95, 256)
	}
	f := l.figures(true)
	air := f.Configuration("j313", 8, 7)
	if air == nil || !air.VolunteerHeadline || !air.Headline.Volunteer || air.Volunteers != nil || air.Column(r) == nil || !near(air.Column(r).Overall, 95) {
		t.Fatalf("the Volunteer-only headline: %+v", air)
	}
	if air.Name != "MacBook Air (M1, 2020)" || air.ChipName != "M1" || air.Path() != "j313/8c-7g" {
		t.Errorf("named %q, chip %q, at %q", air.Name, air.ChipName, air.Path())
	}
	var export struct {
		Volunteers     bool
		Configurations []struct {
			Board    string
			Headline *struct{ Volunteer bool }
		}
	}
	json.Unmarshal(f.JSON(f.DefaultRelease(), perfAt), &export)
	if len(export.Configurations) != 2 || export.Configurations[0].Board != "j313" || !export.Configurations[0].Headline.Volunteer {
		t.Errorf("the data: %+v", export)
	}

	f = l.figures(false)
	if f.Configuration("j313", 8, 7) != nil || len(f.Configurations) != 1 {
		t.Errorf("Volunteers hidden, the Volunteer-only Configuration still shows: %d", len(f.Configurations))
	}
	data := string(f.JSON(f.DefaultRelease(), perfAt))
	if strings.Contains(data, "j313") || !strings.Contains(data, `"volunteers":false`) {
		t.Errorf("the data with Volunteers hidden: %s", data)
	}
}

func TestMacsOnAnOlderMacOSBuildAreApartAndWaitForTheirMacOSRun(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfQualified)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	b := l.mac('b', PerfLabOwned, "j314s", 10, 16)
	c := l.mac('c', PerfCounted, "j314s", 10, 16)
	l.macOS(a, "27C101", 512)
	l.macOS(b, "27B74", 512)
	l.macOS(c, "27B74", 512)
	l.macOS(c, "27C101", 512)
	l.omarchy(a, r, 80, 512)
	l.omarchy(b, r, 40, 512)
	l.omarchy(c, r, 100, 512)
	conf := l.figures(true).Configurations[0]
	if len(conf.Headline.Cohorts) != 2 {
		t.Fatalf("%d cohorts", len(conf.Headline.Cohorts))
	}
	newest, older := conf.Headline.Cohorts[0], conf.Headline.Cohorts[1]
	if newest.MacOSBuild != "27C101" || newest.Macs != 2 || !near(perfRowOf(t, newest.Column(r), "coremark-single", 0).Ratio, 90) {
		t.Errorf("the comparison: %s over %d Macs", newest.MacOSBuild, newest.Macs)
	}
	if older.MacOSBuild != "27B74" || older.MacOSVersion != "27.1" || older.Macs != 1 || !near(older.Column(r).Overall, 40) {
		t.Errorf("apart: %s over %d Macs", older.MacOSBuild, older.Macs)
	}
	for _, c := range []struct {
		a, b []string
		want int
	}{
		{[]string{"27.2.1", "27C101"}, []string{"27.1", "27B74"}, 1},
		{[]string{"27.2", "27C101"}, []string{"27.2", "27C5"}, 1},
		{[]string{"", "27C5a"}, []string{"", "27C5"}, 1},
		{[]string{"27.10", "27K1"}, []string{"27.9", "27J1"}, 1},
		{[]string{"27.2", "27C5"}, []string{"27.2", "27C5"}, 0},
	} {
		if got := compareMacOS(c.a[0], c.a[1], c.b[0], c.b[1]); got != c.want || compareMacOS(c.b[0], c.b[1], c.a[0], c.a[1]) != -c.want {
			t.Errorf("%v against %v: %d", c.a, c.b, got)
		}
	}
}

func TestStorageIsPerSSDCapacity(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfQualified)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	b := l.mac('b', PerfLabOwned, "j314s", 10, 16)
	l.macOS(a, "27C101", 512)
	l.macOS(b, "27C101", 1000)
	l.omarchy(a, r, 50, 512)
	l.omarchy(b, r, 100, 1000)
	conf := l.figures(true).Configurations[0]
	col := conf.Column(r)
	small, big := perfRowOf(t, col, "fio-seq-read", 512), perfRowOf(t, col, "fio-seq-read", 1000)
	if small == big || !near(small.Ratio, 50) || !near(big.Ratio, 100) || small.Macs != 1 || big.Macs != 1 {
		t.Errorf("fio-seq-read at 512 GB %s%%, at 1000 GB %s%%", show(small.Ratio), show(big.Ratio))
	}
	if cpu := perfRowOf(t, col, "coremark-single", 0); !near(cpu.Ratio, 75) || cpu.Capacity != nil {
		t.Errorf("CPU is not split: %s%%", show(cpu.Ratio))
	}
	if storage := perfCategoryOf(t, col, "Storage"); storage.Coverage != (PerfCoverage{4, 4}) || !near(storage.Figure, math.Sqrt(50*100)) {
		t.Errorf("Storage: %s, coverage %+v", show(storage.Figure), storage.Coverage)
	}
	if len(conf.Capacities) != 2 || conf.Capacities[0] != 512 || conf.Capacities[1] != 1000 {
		t.Errorf("capacities %v", conf.Capacities)
	}
	var macOSRows int
	for _, row := range conf.Comparison().MacOS {
		if row.Metric == "fio-seq-read" && near(row.MacOS, 100) {
			macOSRows++
		}
	}
	if macOSRows != 2 {
		t.Errorf("the macOS column has %d fio-seq-read rows", macOSRows)
	}
}

func TestALostSlowRowDrawsNoOverallOrCategoryTrend(t *testing.T) {
	l := newPerfLab(t)
	r1 := l.release('1', "6.18.1-1", PerfQualified)
	r2 := l.release('2', "6.18.2-1", PerfQualified)
	r3 := l.release('3', "6.18.3-1", PerfNoVerdictYet)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	l.macOS(a, "27C101", 512)
	for _, r := range []string{r1, r2, r3} {
		l.omarchy(a, r, 100, 512)
		l.ratio(a, r, "coremark-single", 40, 0)
	}
	l.ratio(a, r2, "stream-triad", 90, 0)
	l.reason(a, r3, "coremark-single", "not_run")
	conf := l.figures(true).Configurations[0]

	// r2 against r1, like for like: every trend drawn.
	col := conf.Column(r2)
	if cpu := perfCategoryOf(t, col, "CPU"); !near(cpu.Trend, 0) {
		t.Errorf("CPU trend %s", show(cpu.Trend))
	}
	if mem := perfCategoryOf(t, col, "Memory"); mem.Trend == nil || *mem.Trend >= 0 {
		t.Errorf("Memory trend %s", show(mem.Trend))
	}
	if col.OverallTrend == nil || *col.OverallTrend >= 0 || !near(perfRowOf(t, col, "stream-triad", 0).Trend, -10) {
		t.Errorf("Overall trend %s", show(col.OverallTrend))
	}

	// r3 lost the slow row: CPU and Overall rise, and draw no trend.
	col = conf.Column(r3)
	cpu := perfCategoryOf(t, col, "CPU")
	if !near(cpu.Figure, 100) || cpu.Trend != nil || col.Overall == nil || col.OverallTrend != nil {
		t.Errorf("after the lost row: CPU %s trend %s, Overall %s trend %s", show(cpu.Figure), show(cpu.Trend), show(col.Overall), show(col.OverallTrend))
	}
	if gpu := perfCategoryOf(t, col, "GPU"); !near(gpu.Trend, 0) {
		t.Errorf("GPU kept its rows and draws its trend: %s", show(gpu.Trend))
	}
	if row := perfRowOf(t, col, "coremark-all", 0); !near(row.Trend, 0) {
		t.Errorf("a row kept draws its trend: %s", show(row.Trend))
	}

	// Another Mac joining is not like for like either.
	b := l.mac('b', PerfLabOwned, "j314s", 10, 16)
	l.macOS(b, "27C101", 512)
	l.omarchy(b, r2, 100, 512)
	l.ratio(b, r2, "coremark-single", 40, 0)
	col = l.figures(true).Configurations[0].Column(r2)
	if col.Macs != 2 || col.OverallTrend != nil || perfCategoryOf(t, col, "GPU").Trend != nil || perfRowOf(t, col, "coremark-all", 0).Trend != nil {
		t.Errorf("a Mac joined and a trend is drawn")
	}
}

func TestTheBiggestGapsAreTheRowsFurthestUnderMacOSThenNoBuild(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfQualified)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	b := l.mac('b', PerfLabOwned, "j313", 8, 7)
	for _, mac := range []string{a, b} {
		l.macOS(mac, "27C101", 512)
		l.omarchy(mac, r, 100, 512)
	}
	l.ratio(a, r, "speedometer", 70, 0)
	l.ratio(a, r, "coremark-single", 96, 0)
	l.ratio(a, r, "7zip", 80, 0)
	l.ratio(b, r, "motionmark", 30, 0)
	l.ratio(b, r, "power-idle", 150, 0)
	l.reason(a, r, "mlx-matmul-fp16", "no_build")
	l.reason(b, r, "decode-hevc-4k", "fell_back")
	f := l.figures(true)
	var got []string
	for _, gap := range f.Gaps(r) {
		got = append(got, gap.Configuration.Board+" "+gap.Row.Metric)
	}
	if strings.Join(got, ", ") != "j313 motionmark, j314s speedometer, j314s 7zip, j314s mlx-matmul-fp16" {
		t.Errorf("gaps: %v", got)
	}
	got = nil
	for _, gap := range f.Configuration("j314s", 10, 16).Gaps(r) {
		got = append(got, gap.Row.Metric)
	}
	if strings.Join(got, ", ") != "speedometer, 7zip, mlx-matmul-fp16" {
		t.Errorf("a Configuration's gaps, worst first: %v", got)
	}
}

func TestTheDefaultReleaseIsTheNewestQualifiedElseTheNewestWithNoVerdict(t *testing.T) {
	l := newPerfLab(t)
	l.release('1', "6.18.1-1", PerfNoVerdictYet)
	l.release('2', "6.18.2-1", PerfNoVerdictYet)
	if r := l.figures(true).DefaultRelease(); r == nil || r.Kernel != "6.18.2-1" {
		t.Errorf("no qualified Release: %+v", r)
	}
	l.release('3', "6.18.3-1", PerfQualified)
	l.release('4', "6.18.4-1", PerfNoVerdictYet)
	f := l.figures(true)
	if r := f.DefaultRelease(); r == nil || r.Kernel != "6.18.3-1" {
		t.Errorf("a qualified Release: %+v", r)
	}
	if r := f.Release("6.18.1-1"); r == nil || r.Key != perfKey('1') || f.Release(perfKey('4')).Kernel != "6.18.4-1" || f.Release("6.0") != nil {
		t.Errorf("a Release by name: %+v", r)
	}
	if (&PerfFigures{Set: l.set}).DefaultRelease() != nil {
		t.Error("a Release with none published")
	}
}

func TestShownColumnsAreTheNewestWithNoVerdictAndFourQualified(t *testing.T) {
	l := newPerfLab(t)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	l.macOS(a, "27C101", 512)
	for i, verdict := range []string{PerfQualified, PerfQualified, PerfQualified, PerfQualified, PerfNoVerdictYet, PerfQualified, PerfNoVerdictYet, PerfNoVerdictYet} {
		r := l.release(byte('1'+i), "6.18."+string(rune('1'+i))+"-1", verdict)
		l.omarchy(a, r, 100, 512)
	}
	cohort := l.figures(true).Configurations[0].Comparison()
	kernels := func(cols []*PerfColumn) string {
		var out []string
		for _, col := range cols {
			out = append(out, col.Release.Kernel)
		}
		return strings.Join(out, " ")
	}
	if got := kernels(cohort.Shown(false)); got != "6.18.8-1 6.18.6-1 6.18.4-1 6.18.3-1 6.18.2-1" {
		t.Errorf("shown: %s", got)
	}
	if got := kernels(cohort.Shown(true)); got != "6.18.8-1 6.18.6-1 6.18.4-1 6.18.3-1 6.18.2-1 6.18.1-1" {
		t.Errorf("with older: %s", got)
	}
	// A trend is from the next older qualified Release.
	if col := cohort.Column(perfKey('8')); !near(col.OverallTrend, 0) {
		t.Errorf("the newest's trend: %s", show(col.OverallTrend))
	}
}

func TestBootTimeIsOmarchyOnlyWithItsChangeFromTheReleaseBefore(t *testing.T) {
	l := newPerfLab(t)
	r1 := l.release('1', "6.18.1-1", PerfQualified)
	r2 := l.release('2', "6.18.2-1", PerfQualified)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	b := l.mac('b', PerfLabOwned, "j314s", 10, 16)
	l.macOS(a, "27C101", 512)
	for _, mac := range []string{a, b} {
		l.omarchy(mac, r1, 100, 512)
		l.omarchy(mac, r2, 100, 512)
	}
	l.value(a, "omarchy", r1, "", "boot-time", 20, "", 0)
	l.value(b, "omarchy", r1, "", "boot-time", 30, "", 0)
	l.value(a, "omarchy", r2, "", "boot-time", 18, "", 0)
	l.value(b, "omarchy", r2, "", "boot-time", 27, "", 0)
	conf := l.figures(true).Configurations[0]
	only := conf.OmarchyOnlyColumn(r2)
	if only == nil || len(only.Rows) != 1 {
		t.Fatalf("Omarchy only: %+v", only)
	}
	if row := only.Rows[0]; row.Metric != "boot-time" || !near(row.Value, 22.5) || row.Macs != 2 || !near(row.Change, -10) || row.HigherIsBetter {
		t.Errorf("boot time %s over %d Macs, change %s%%", show(row.Value), row.Macs, show(row.Change))
	}
	if row := conf.OmarchyOnlyColumn(r1).Rows[0]; row.Change != nil {
		t.Errorf("the oldest has a change: %s", show(row.Change))
	}
	for _, row := range conf.Column(r2).Rows {
		if row.Metric == "boot-time" {
			t.Error("boot time is compared with macOS")
		}
	}
}

func TestAColumnSaysWhatItsMacsBooted(t *testing.T) {
	l := newPerfLab(t)
	r1 := l.release('1', "6.18.1-1", PerfQualified)
	r2 := l.release('2', "6.18.2-1", PerfQualified)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	b := l.mac('b', PerfLabOwned, "j314s", 10, 16)
	for _, mac := range []string{a, b} {
		l.macOS(mac, "27C101", 512)
		l.omarchy(mac, r1, 100, 512)
		l.omarchy(mac, r2, 100, 512)
	}
	for i := range l.p.Values {
		v := &l.p.Values[i]
		switch {
		case v.Release == r1:
			v.Booted = map[string]string{"linux-asahi": "6.18.1-1", "mesa": "25.3.0"}
		case v.Release == r2 && v.Mac == a:
			v.Booted = map[string]string{"linux-asahi": "6.18.2-1", "mesa": "25.3.0"}
		case v.Release == r2:
			v.Booted = map[string]string{"linux-asahi": "6.18.2-1", "mesa": "25.3.1"}
		}
	}
	conf := l.figures(true).Configurations[0]
	old, now := conf.Column(r1), conf.Column(r2)
	if len(old.Stack) != 2 || len(old.StackDiffers) != 0 || old.StackChanged {
		t.Errorf("r1: %v differs %v", old.Stack, old.StackDiffers)
	}
	if now.Stack["linux-asahi"] != "6.18.2-1" || len(now.Stack) != 1 || strings.Join(now.StackDiffers, " ") != "mesa" || !now.StackChanged {
		t.Errorf("r2: %v differs %v changed %v", now.Stack, now.StackDiffers, now.StackChanged)
	}
}

func TestChipFamiliesHoldTheirConfigurations(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfQualified)
	for i, c := range []struct {
		board    string
		cpu, gpu int
		ratio    float64
	}{{"j314s", 10, 16, 80}, {"j314s", 8, 14, 100}, {"j313", 8, 7, 90}, {"j313", 8, 8, 0}} {
		mac := l.mac(byte('a'+i), PerfLabOwned, c.board, c.cpu, c.gpu)
		if c.ratio > 0 {
			l.macOS(mac, "27C101", 512)
			l.omarchy(mac, r, c.ratio, 512)
		}
	}
	f := l.figures(true)
	families := map[string]*PerfFamily{}
	for _, fam := range f.Families(r) {
		families[fam.Chip] = fam
	}
	m1, pro := families["m1"], families["m1-pro-max-ultra"]
	if m1 == nil || len(m1.Configurations) != 2 || m1.WithRuns != 1 || !near(m1.Median, 90) {
		t.Errorf("M1: %+v", m1)
	}
	if pro == nil || pro.Name != "M1 Pro/Max/Ultra" || len(pro.Configurations) != 2 || pro.WithRuns != 2 || !near(pro.Median, 90) {
		t.Errorf("M1 Pro/Max/Ultra: %+v", pro)
	}
	if pro.Configurations[0].CPU != 8 || families["m2"] == nil || len(families["m2"].Configurations) != 0 {
		t.Errorf("the order or an empty family")
	}
	if f.Configuration("j313", 8, 8).Headline != nil {
		t.Error("a Configuration with no macOS result has a headline")
	}
}

func TestThePerformanceDataNamesNoMacAndAReleaseByKernelAndVerdict(t *testing.T) {
	l := newPerfLab(t)
	r := l.release('f', "6.18.2-1", PerfQualified)
	a := l.mac('a', PerfLabOwned, "j314s", 10, 16)
	v := l.mac('d', PerfVolunteer, "j314s", 10, 16)
	for _, mac := range []string{a, v} {
		l.macOS(mac, "27C101", 512)
		l.omarchy(mac, r, 90, 512)
	}
	l.reason(a, r, "mlx-matmul-fp16", "no_build")
	f := l.figures(true)
	data := f.JSON(f.DefaultRelease(), perfAt)
	for _, secret := range []string{perfKey('a'), perfKey('d'), perfKey('f')} {
		if strings.Contains(string(data), secret) {
			t.Errorf("the data names %s", secret)
		}
	}
	var export struct {
		License     struct{ ID string }
		GeneratedAt string `json:"generated_at"`
		Set         string
		Release     map[string]string
		Configs     []struct {
			Board      string
			CPU        int `json:"cpu_cores"`
			Headline   *perfExportComparison
			Volunteers *perfExportComparison
		} `json:"configurations"`
	}
	if err := json.Unmarshal(data, &export); err != nil {
		t.Fatal(err)
	}
	if export.License.ID != LicenseID || export.GeneratedAt != "2026-11-22T10:00:00Z" || export.Set != "bench-1" || len(export.Release) != 2 || export.Release["kernel"] != "6.18.2-1" || export.Release["verdict"] != PerfQualified {
		t.Errorf("the head: %+v", export)
	}
	if len(export.Configs) != 1 || export.Configs[0].Headline == nil || export.Configs[0].Volunteers == nil {
		t.Fatalf("configurations: %s", data)
	}
	h := export.Configs[0].Headline
	if h.MacOSBuild != "27C101" || h.MacOSVersion != "27.2.1" || h.MacsPaired != 1 || h.Coverage != (PerfCoverage{23, 24}) || len(h.Categories) != 8 {
		t.Errorf("the headline: %+v", h)
	}
	var mlx, boot, fio *perfExportMetric
	for i, m := range h.Metrics {
		switch m.Metric {
		case "mlx-matmul-fp16":
			mlx = &h.Metrics[i]
		case "boot-time":
			boot = &h.Metrics[i]
		case "fio-seq-read":
			fio = &h.Metrics[i]
		}
	}
	if mlx == nil || mlx.Ratio != nil || mlx.Reason == nil || *mlx.Reason != "no_build" {
		t.Errorf("mlx: %+v", mlx)
	}
	if boot == nil || boot.Ratio != nil || !near(boot.Omarchy, 20) || boot.Category != sets.OmarchyOnly {
		t.Errorf("boot time: %+v", boot)
	}
	if fio == nil || fio.Capacity == nil || *fio.Capacity != 512 || !near(fio.Ratio, 90) || !near(fio.MacOS, 100) {
		t.Errorf("fio: %+v", fio)
	}
	if none := (&PerfFigures{Set: l.set}).JSON(nil, perfAt); !strings.Contains(string(none), `"release":null,"volunteers":false,"configurations":[]`) {
		t.Errorf("nothing published: %s", none)
	}
}

func TestPerfSetIsNamedByNumberOrName(t *testing.T) {
	for _, name := range []string{"1", "bench-1"} {
		if s, err := PerfSet(name); err != nil || s.Name != "bench-1" {
			t.Errorf("%q: %v %v", name, s, err)
		}
	}
	want, err := sets.Current()
	if errors.Is(err, sets.ErrNoSet) {
		want, err = sets.Newest()
	}
	if s, err2 := PerfSet(""); err != nil || err2 != nil || s.Name != want.Name {
		t.Errorf(`"": %v %v, want %s`, s, err2, want.Name)
	}
	if _, err := PerfSet("bench-99"); err == nil {
		t.Error("bench-99 is a set")
	}
}
