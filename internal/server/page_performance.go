package server

// The Performance pages (spec section 22, "Pages"): Omarchy against macOS
// on the same Macs, the lab's overview at /performance and one
// Configuration's page at /performance/<board>/<cpu>c-<gpu>g. Both render
// from reports.db alone, name a Release by its kernel version and verdict
// state and a Mac by nothing: kinds, counts, SSD sizes and macOS versions.

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/detect"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /performance", web.Anyone, (*Server).pPerformance)
	addPage("GET /performance/{board}/{cores}", web.Anyone, (*Server).pPerformanceConfiguration)
	addPage("GET /benchmarks", web.Anyone, (*Server).pBenchmarksMoved)
}

// pBenchmarksMoved sends the old Benchmarks page's visitors to Performance.
func (s *Server) pBenchmarksMoved(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, s.href("/performance"), http.StatusMovedPermanently)
}

// perfOpen are the categories whose section a Configuration's page opens.
var perfOpen = []string{"CPU", "GPU", "AI"}

// perfAsk is what a Performance page was asked for: the set, the Release,
// whether Volunteers' Macs show and whether older Releases do.
type perfAsk struct {
	set        *sets.Set
	defaultSet string
	figures    *reports.PerfFigures
	release    *reports.PerfRelease
	volunteers bool
	older      bool
}

// perfAsked reads the query: ?set= (bench-N or N), ?release= (a kernel
// version), ?volunteers=false and ?older=1. A Release the set does not have
// falls back to the default, as a picker that changed set asks for one.
// nil when it has answered w itself.
func (s *Server) perfAsked(w http.ResponseWriter, r *http.Request) *perfAsk {
	q := r.URL.Query()
	set, err := reports.PerfSet(q.Get("set"))
	if err != nil {
		s.notFound(w, r)
		return nil
	}
	a := &perfAsk{set: set, volunteers: true, older: q.Get("older") == "1"}
	if def, err := reports.PerfSet(""); err == nil {
		a.defaultSet = def.Name
	}
	// The picker's form sends volunteers=false before its checkbox's
	// volunteers=true, so the last one says.
	if v := q["volunteers"]; len(v) > 0 {
		a.volunteers = v[len(v)-1] != "false"
	}
	if a.figures, err = s.reports.PerformanceFigures(s.catalogue, set, a.volunteers); err != nil {
		s.pageFailed(w, err)
		return nil
	}
	if name := q.Get("release"); name != "" {
		a.release = a.figures.Release(name)
	}
	if a.release == nil {
		a.release = a.figures.DefaultRelease()
	}
	return a
}

func (a *perfAsk) releaseKey() string {
	if a.release == nil {
		return ""
	}
	return a.release.Key
}

// link is path with what was asked that is not the default.
func (a *perfAsk) link(s *Server, path string, older bool) string {
	q := url.Values{}
	if a.set.Name != a.defaultSet {
		q.Set("set", a.set.Name)
	}
	if def := a.figures.DefaultRelease(); a.release != nil && (def == nil || def.Key != a.release.Key) {
		q.Set("release", a.release.Kernel)
	}
	if !a.volunteers {
		q.Set("volunteers", "false")
	}
	if older {
		q.Set("older", "1")
	}
	if len(q) == 0 {
		return s.href(path)
	}
	return s.href(path) + "?" + q.Encode()
}

type perfOption struct {
	Value, Label string
	Selected     bool
}

// perfPickerView is the form both pages carry: the set, the Release and
// "Show Volunteer Macs".
type perfPickerView struct {
	Action     string
	Sets       []perfOption
	Releases   []perfOption
	Volunteers bool
	Older      bool
}

func (s *Server) perfPicker(a *perfAsk, action string) perfPickerView {
	p := perfPickerView{Action: s.href(action), Volunteers: a.volunteers, Older: a.older}
	all, _ := sets.All()
	for i := len(all) - 1; i >= 0; i-- {
		label := all[i].Name
		if all[i].Name == a.defaultSet {
			label += " (current)"
		}
		p.Sets = append(p.Sets, perfOption{Value: all[i].Name, Label: label, Selected: all[i].Name == a.set.Name})
	}
	def := a.figures.DefaultRelease()
	for _, r := range a.figures.Releases {
		label := r.Kernel + " · " + r.Verdict
		if def != nil && def.Key == r.Key && r.Verdict == reports.PerfQualified {
			label = r.Kernel + " · newest qualified"
		}
		p.Releases = append(p.Releases, perfOption{Value: r.Kernel, Label: label, Selected: a.release != nil && a.release.Key == r.Key})
	}
	return p
}

// perfFigureView is a percent of macOS: its words, its colour and its bar,
// drawn to 150% against a tick at 100%.
type perfFigureView struct {
	Has          bool
	Text, Colour string
	Width        string
	Trend        string
}

func perfFigure(v *float64, colour string, trend *float64) perfFigureView {
	if v == nil {
		return perfFigureView{Text: "–", Colour: "none", Width: "0"}
	}
	f := perfFigureView{Has: true, Text: perfPercent(*v), Colour: colour, Width: strconv.FormatFloat(math.Min(math.Max(*v, 0), 150), 'f', 1, 64)}
	if trend != nil {
		f.Trend = perfPoints(*trend)
	}
	return f
}

func perfPercent(v float64) string { return strconv.FormatFloat(math.Round(v), 'f', 0, 64) + "%" }

// perfPoints is a change of a percent of macOS, in points.
func perfPoints(v float64) string {
	r := math.Round(v)
	switch {
	case r > 0:
		return "+" + strconv.FormatFloat(r, 'f', 0, 64) + " pts"
	case r < 0:
		return "−" + strconv.FormatFloat(-r, 'f', 0, 64) + " pts"
	}
	return "±0 pts"
}

// perfNumber is a measured value as a page prints it.
func perfNumber(v float64) string {
	places := 2
	switch a := math.Abs(v); {
	case a >= 100:
		places = 0
	case a >= 10:
		places = 1
	}
	text := strconv.FormatFloat(v, 'f', places, 64)
	whole, fraction, _ := strings.Cut(text, ".")
	sign := ""
	if strings.HasPrefix(whole, "-") {
		sign, whole = "−", whole[1:]
	}
	var b strings.Builder
	for i, c := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if fraction != "" {
		return sign + b.String() + "." + fraction
	}
	return sign + b.String()
}

func perfValue(v *float64) string {
	if v == nil {
		return "–"
	}
	return perfNumber(*v)
}

// perfReason is a reason code in words: "no build", "fell back".
func perfReason(code string) string {
	if code == "" {
		return "not run"
	}
	return strings.ReplaceAll(code, "_", " ")
}

func perfCapacity(gb int64) string {
	if gb >= 1000 && gb%1000 == 0 {
		return strconv.FormatInt(gb/1000, 10) + " TB"
	}
	return strconv.FormatInt(gb, 10) + " GB"
}

func perfCapacities(c []int64) string {
	var out []string
	for _, gb := range c {
		out = append(out, perfCapacity(gb))
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, " · ")
}

func perfCores(cpu, gpu int) string {
	if cpu == 0 && gpu == 0 {
		return ""
	}
	return fmt.Sprintf("%d CPU · %d GPU", cpu, gpu)
}

// perfKinds is a Configuration's Macs by kind: "2 Lab-owned · 1 Volunteer".
func perfKinds(c *reports.PerfConfiguration) string {
	var out []string
	for _, k := range []struct {
		n    int
		kind string
	}{{c.LabOwned, "Lab-owned"}, {c.Counted, "Counted"}, {c.Volunteer, "Volunteer"}} {
		if k.n > 0 {
			out = append(out, fmt.Sprintf("%d %s", k.n, k.kind))
		}
	}
	return strings.Join(out, " · ")
}

// perfMacOSVersions are the macOS versions a Configuration's headline Macs
// last ran, newest first: "27.2.1 and 27.1".
func perfMacOSVersions(c *reports.PerfConfiguration) string {
	if c.Headline == nil {
		return ""
	}
	var versions []string
	for _, cohort := range c.Headline.Cohorts {
		if !slices.Contains(versions, cohort.MacOSVersion) {
			versions = append(versions, cohort.MacOSVersion)
		}
	}
	if len(versions) > 1 {
		return strings.Join(versions[:len(versions)-1], ", ") + " and " + versions[len(versions)-1]
	}
	return strings.Join(versions, "")
}

func perfRowName(row *reports.PerfRow) string {
	if row.Capacity != nil {
		return row.Name + ", SSD " + perfCapacity(*row.Capacity)
	}
	return row.Name
}

func perfAPI(row *reports.PerfRow) string {
	if !row.APIDiffers {
		return ""
	}
	return "API differs: " + row.OmarchyAPI + ", " + row.MacOSAPI
}

func perfDirection(higher bool) string {
	if higher {
		return "higher is better"
	}
	return "lower is better"
}

// perfGapView is one of the biggest gaps.
type perfGapView struct {
	Category, Name, Where, WhereHref, API, Of string
	Figure                                    perfFigureView
	Reason                                    string
	Trend                                     string
}

func perfGap(g reports.PerfGap, older *reports.PerfColumn) perfGapView {
	row := g.Row
	v := perfGapView{Category: row.Category, Name: perfRowName(row), API: perfAPI(row)}
	if row.Ratio == nil {
		v.Reason = perfReason(row.Reason)
	} else {
		v.Figure = perfFigure(row.Ratio, row.Colour, nil)
		if row.Trend != nil && older != nil {
			v.Trend = older.Release.Kernel + " " + perfPercent(*row.Ratio-*row.Trend) + " → " + perfPercent(*row.Ratio)
		}
	}
	return v
}

// The overview.

type perfConfigRowView struct {
	Name, Board, Cores, Href string
	Kinds, SSD, MacOS, Of    string
	VolunteerOnly            bool
	Measured                 bool
	Overall                  perfFigureView
	Categories               []perfFigureView
	Waiting, LendHref, Lend  string
}

type perfFamilyView struct {
	Name, Count string
	Open        bool
	Median      perfFigureView
	Rows        []perfConfigRowView
}

type perfPage struct {
	Picker     perfPickerView
	Release    *reports.PerfRelease
	Categories []string
	Span       int
	Gaps       []perfGapView
	MoreGaps   int
	Families   []perfFamilyView
	DataHref   string
}

// perfGapsShown is how many of the lab's biggest gaps the overview lists.
const perfGapsShown = 5

// perfConfigGapsShown is how many measured gaps a Configuration's page lists
// before its rows with no value.
const perfConfigGapsShown = 7

// pPerformance is the lab's overview: every Configuration against macOS on
// one Release, chip family by chip family.
func (s *Server) pPerformance(w http.ResponseWriter, r *http.Request) {
	a := s.perfAsked(w, r)
	if a == nil {
		return
	}
	f, key := a.figures, a.releaseKey()
	page := perfPage{Picker: s.perfPicker(a, "/performance"), Release: a.release, Categories: reports.PerfCategories, Span: len(reports.PerfCategories) + 1, DataHref: a.link(s, "/api/v1/performance.json", false)}
	gaps := f.Gaps(key)
	for i, g := range gaps {
		if i == perfGapsShown {
			page.MoreGaps = len(gaps) - i
			break
		}
		v := perfGap(g, nil)
		v.Where, v.WhereHref = g.Configuration.Name+" · "+perfCores(g.Configuration.CPU, g.Configuration.GPU), a.link(s, "/performance/"+g.Configuration.Path(), false)
		v.Of = "of macOS " + g.Configuration.Comparison().MacOSVersion
		page.Gaps = append(page.Gaps, v)
	}
	shown := map[string]bool{}
	for _, c := range f.Configurations {
		shown[c.Board] = true
	}
	unmeasured := s.perfUnmeasured(shown)
	for _, fam := range f.Families(key) {
		v := perfFamilyView{Name: fam.Name, Open: fam.WithRuns > 0, Median: perfFigure(fam.Median, perfMedianColour(fam.Median), nil)}
		for _, c := range fam.Configurations {
			v.Rows = append(v.Rows, s.perfConfigRow(a, c))
		}
		v.Rows = append(v.Rows, unmeasured[fam.Chip]...)
		if len(v.Rows) == 0 {
			continue
		}
		v.Count = reports.Plural(len(v.Rows), "Configuration") + " · " + strconv.Itoa(fam.WithRuns) + " with runs"
		page.Families = append(page.Families, v)
	}
	s.render(w, r, "performance/index", web.Page{Title: "Performance", Nav: "performance", Wide: true, Data: page})
}

// perfMedianColour colours a family's median as a figure with no tolerance
// of its own: the widest of bench-1's, 5 points.
func perfMedianColour(v *float64) string {
	if v == nil {
		return "none"
	}
	switch {
	case math.Abs(*v-100) <= 5:
		return reports.PerfGrey
	case *v > 100:
		return reports.PerfGreen
	case *v >= 85:
		return reports.PerfYellow
	case *v >= 60:
		return reports.PerfOrange
	}
	return reports.PerfRed
}

func (s *Server) perfConfigRow(a *perfAsk, c *reports.PerfConfiguration) perfConfigRowView {
	v := perfConfigRowView{Name: c.Name, Board: c.Board, Cores: perfCores(c.CPU, c.GPU), Href: a.link(s, "/performance/"+c.Path(), false),
		Kinds: perfKinds(c), SSD: perfCapacities(c.Capacities), MacOS: perfMacOSVersions(c), VolunteerOnly: c.VolunteerHeadline}
	col := c.Column(a.releaseKey())
	switch {
	case col != nil:
		v.Measured, v.Of = true, "of macOS "+c.Comparison().MacOSVersion
		v.Overall = perfFigure(col.Overall, col.OverallColour, col.OverallTrend)
		for _, cat := range col.Categories {
			v.Categories = append(v.Categories, perfFigure(cat.Figure, cat.Colour, nil))
		}
	case c.Comparison() == nil:
		v.Waiting = "Its Macs wait for their macOS result."
	case len(c.Comparison().Columns) > 0 && a.release != nil:
		v.Waiting = "Not measured on " + a.release.Kernel + " yet."
	default:
		v.Waiting, v.Lend, v.LendHref = "No runs yet.", "Lend a Mac of this Configuration", s.href("/lend")
	}
	return v
}

// perfUnmeasured are the catalogue's boards with no Mac on the pages, by
// chip family, each as a row that says so.
func (s *Server) perfUnmeasured(shown map[string]bool) map[string][]perfConfigRowView {
	out := map[string][]perfConfigRowView{}
	boards := s.catalogue.Boards()
	if boards == nil {
		return out
	}
	for _, board := range boards.Keys() {
		if shown[board] {
			continue
		}
		chip, _ := member(boards, board, "chip").(string)
		name, _ := member(boards, board, "name").(string)
		if name == "" {
			name = board
		}
		cpu, gpu, _ := detect.OneConfiguration(board)
		out[chip] = append(out[chip], perfConfigRowView{Name: name, Board: board, Cores: perfCores(cpu, gpu), Kinds: "none",
			Waiting: "No runs yet.", Lend: "Lend a Mac of this model", LendHref: s.href("/lend")})
	}
	return out
}

// One Configuration.

type perfColumnHead struct {
	Kernel, Verdict string
	NoVerdict       bool
	Current         bool
	Macs            string
	Differs         string
	Changed         string
}

type perfCellView struct {
	Value   string
	Figure  perfFigureView
	Reason  string
	Current bool
}

type perfVolunteerLine struct {
	ID, Label, MacOS string
	Cells            []perfCellView
}

type perfLineView struct {
	Group           string
	Name, Unit, API string
	MacOS           string
	Cells           []perfCellView
	Volunteer       *perfVolunteerLine
}

type perfSectionView struct {
	ID, Name string
	Open     bool
	Figure   perfFigureView
	Of       string
	Count    string
	Coverage string
	Excluded string
	Lines    []perfLineView
}

type perfTileView struct {
	ID, Name, Worst, Coverage string
	Figure                    perfFigureView
}

type perfCohortView struct {
	Version, Build, Measured, Macs, Note string
}

type perfStackRow struct {
	Kernel   string
	Versions []string
}

type perfOnlyCell struct {
	Value, Change string
	Current       bool
}

type perfOnlyLine struct {
	Name, Unit string
	Cells      []perfOnlyCell
}

type perfConfigPage struct {
	Name, Board, Cores, Chip string
	BackHref, ModelHref      string
	Picker                   perfPickerView
	Kinds, SSD               string
	VolunteerHeadline        bool
	VolunteersHidden         bool
	Comparison               *perfCohortView
	Older                    []perfCohortView
	Release                  *reports.PerfRelease
	MacOS                    string
	Measured                 bool
	Overall                  perfFigureView
	Tiles                    []perfTileView
	Gaps                     []perfGapView
	MoreGaps                 int
	TrendFrom                string
	Columns                  []perfColumnHead
	Span                     int
	Sections                 []perfSectionView
	OlderHref, FewerHref     string
	Packages                 []string
	Stacks                   []perfStackRow
	OmarchyOnly              []perfOnlyLine
	DataHref                 string
}

var rePerfCores = regexp.MustCompile(`^([1-9][0-9]?)c-([1-9][0-9]?)g$`)

func perfKey(row *reports.PerfRow) string {
	if row.Capacity == nil {
		return row.Metric
	}
	return row.Metric + "@" + strconv.FormatInt(*row.Capacity, 10)
}

func perfRowsByKey(rows []*reports.PerfRow) map[string]*reports.PerfRow {
	out := map[string]*reports.PerfRow{}
	for _, row := range rows {
		out[perfKey(row)] = row
	}
	return out
}

func perfCell(row *reports.PerfRow, current bool) perfCellView {
	if row == nil || (row.Ratio == nil && row.Reason == "") {
		return perfCellView{Value: "–", Current: current}
	}
	c := perfCellView{Value: perfValue(row.Omarchy), Current: current}
	if row.Ratio == nil {
		c.Value, c.Reason = "–", perfReason(row.Reason)
	} else {
		c.Figure = perfFigure(row.Ratio, row.Colour, row.Trend)
	}
	return c
}

func perfMacOSCell(row *reports.PerfRow) string {
	switch {
	case row == nil || (row.MacOS == nil && row.Reason == ""):
		return "–"
	case row.MacOS == nil:
		return perfReason(row.Reason)
	}
	return perfNumber(*row.MacOS)
}

// pPerformanceConfiguration is one Configuration against macOS, Release by
// Release.
func (s *Server) pPerformanceConfiguration(w http.ResponseWriter, r *http.Request) {
	m := rePerfCores.FindStringSubmatch(r.PathValue("cores"))
	if m == nil {
		s.notFound(w, r)
		return
	}
	a := s.perfAsked(w, r)
	if a == nil {
		return
	}
	cpu, _ := strconv.Atoi(m[1])
	gpu, _ := strconv.Atoi(m[2])
	c := a.figures.Configuration(r.PathValue("board"), cpu, gpu)
	if c == nil {
		s.notFound(w, r)
		return
	}
	path := "/performance/" + c.Path()
	page := perfConfigPage{Name: c.Name, Board: c.Board, Cores: fmt.Sprintf("%d-core CPU · %d-core GPU", c.CPU, c.GPU), Chip: c.ChipName,
		BackHref: a.link(s, "/performance", false), ModelHref: s.href("/models/" + c.Board), Picker: s.perfPicker(a, path),
		Kinds: perfKinds(c), SSD: perfCapacities(c.Capacities), VolunteerHeadline: c.VolunteerHeadline, VolunteersHidden: !a.volunteers,
		Release: a.release, DataHref: a.link(s, "/api/v1/performance.json", false)}
	cohort := c.Comparison()
	if cohort == nil {
		s.render(w, r, "performance/show", web.Page{Title: c.Name + " performance", Nav: "performance", Wide: true, Data: page})
		return
	}
	page.MacOS = "macOS " + cohort.MacOSVersion
	page.Comparison = &perfCohortView{Version: cohort.MacOSVersion, Build: cohort.MacOSBuild, Measured: dayOf(cohort.Measured), Macs: reports.Plural(cohort.Macs, "Mac") + " paired"}
	for _, older := range c.Headline.Cohorts[1:] {
		note := "they wait for their macOS run on the newer build"
		if older.Macs == 1 {
			note = "it waits for its macOS run on the newer build"
		}
		page.Older = append(page.Older, perfCohortView{Version: older.MacOSVersion, Build: older.MacOSBuild, Measured: dayOf(older.Measured), Macs: reports.Plural(older.Macs, "Mac"), Note: note})
	}

	key := a.releaseKey()
	shown := cohort.Shown(a.older)
	if !a.older && cohort.Column(key) != nil && !slices.ContainsFunc(shown, func(col *reports.PerfColumn) bool { return col.Release.Key == key }) {
		shown, a.older = cohort.Shown(true), true
	}
	if len(cohort.Shown(true)) > len(cohort.Shown(false)) {
		if a.older {
			page.FewerHref = a.link(s, path, false)
		} else {
			page.OlderHref = a.link(s, path, true)
		}
	}
	page.Picker.Older = a.older

	col := cohort.Column(key)
	if col != nil {
		page.Measured = true
		page.Overall = perfFigure(col.Overall, col.OverallColour, col.OverallTrend)
		for _, cat := range col.Categories {
			t := perfTileView{ID: perfSectionID(cat.Name), Name: cat.Name, Figure: perfFigure(cat.Figure, cat.Colour, cat.Trend),
				Coverage: fmt.Sprintf("%d of %d measured", cat.Coverage.Measured, cat.Coverage.Rows)}
			if cat.Worst != nil {
				t.Worst = "worst: " + perfRowName(cat.Worst)
			}
			page.Tiles = append(page.Tiles, t)
		}
		var trendFrom *reports.PerfColumn
		for i, other := range cohort.Columns {
			if other.Release.Key != key {
				continue
			}
			for _, older := range cohort.Columns[i+1:] {
				if older.Release.Verdict == reports.PerfQualified {
					trendFrom = older
					break
				}
			}
		}
		if trendFrom != nil {
			page.TrendFrom = trendFrom.Release.Kernel
		}
		measured := 0
		for _, g := range c.Gaps(key) {
			v := perfGap(g, trendFrom)
			if v.Reason == "" {
				if measured == perfConfigGapsShown {
					page.MoreGaps++
					continue
				}
				measured++
			}
			v.Of = "of macOS " + cohort.MacOSVersion
			page.Gaps = append(page.Gaps, v)
		}
	}

	for i, sc := range shown {
		h := perfColumnHead{Kernel: sc.Release.Kernel, Verdict: sc.Release.Verdict, NoVerdict: sc.Release.Verdict == reports.PerfNoVerdictYet,
			Current: sc.Release.Key == key, Macs: reports.Plural(sc.Macs, "Mac")}
		if len(sc.StackDiffers) > 0 {
			h.Differs = "its Macs booted different " + strings.Join(sc.StackDiffers, ", ")
		}
		if i+1 < len(shown) {
			if changed := perfStackChanges(sc, shown[i+1]); len(changed) > 0 {
				h.Changed = "other " + strings.Join(changed, ", ") + " than " + shown[i+1].Release.Kernel
			}
		}
		page.Columns = append(page.Columns, h)
	}

	page.Span = len(page.Columns) + 2
	macOS := perfRowsByKey(cohort.MacOS)
	byColumn := make([]map[string]*reports.PerfRow, len(shown))
	for i, sc := range shown {
		byColumn[i] = perfRowsByKey(sc.Rows)
	}
	var volunteers *reports.PerfCohort
	if c.Volunteers != nil {
		volunteers = c.Volunteers.Cohorts[0]
	}
	for _, name := range reports.PerfCategories {
		sec := perfSectionView{ID: perfSectionID(name), Name: name, Open: slices.Contains(perfOpen, name), Of: "of macOS " + cohort.MacOSVersion}
		if a.release != nil {
			sec.Of += " on " + a.release.Kernel
		}
		if col != nil {
			for _, cat := range col.Categories {
				if cat.Name == name {
					sec.Figure = perfFigure(cat.Figure, cat.Colour, cat.Trend)
					sec.Coverage = fmt.Sprintf("%d of %d measured", cat.Coverage.Measured, cat.Coverage.Rows)
					if len(cat.Excluded) > 0 {
						sec.Excluded = reports.Plural(len(cat.Excluded), "row") + " out of the mean"
					}
				}
			}
		}
		group := ""
		for _, k := range perfKeys(a.set, name, cohort.MacOS, shown) {
			metric, _ := a.set.Metric(k.metric)
			line := perfLineView{Name: metric.Name, Unit: metric.Unit + " · " + perfDirection(metric.HigherIsBetter()), MacOS: perfMacOSCell(macOS[k.key])}
			if metric.APIDiffers {
				line.API = "API differs: " + metric.Omarchy.API + ", " + metric.MacOS.API
			}
			if name == "Storage" {
				g := "SSD size not known"
				if k.capacity != nil {
					g = "SSD " + perfCapacity(*k.capacity)
				}
				if g != group {
					line.Group, group = g, g
				}
			}
			for i, sc := range shown {
				line.Cells = append(line.Cells, perfCell(byColumn[i][k.key], sc.Release.Key == key))
			}
			if volunteers != nil {
				probe := &reports.PerfRow{Metric: k.metric, Capacity: k.capacity}
				vl := &perfVolunteerLine{ID: "volunteer-" + strings.ReplaceAll(k.key, "@", "-"), Label: reports.Plural(volunteers.Macs, "Volunteer Mac") + " · macOS " + volunteers.MacOSVersion,
					MacOS: perfMacOSCell(perfRowsByKey(volunteers.MacOS)[k.key])}
				found := false
				for _, sc := range shown {
					row := c.Line(sc.Release.Key, probe)
					found = found || (row != nil && (row.Ratio != nil || row.Reason != ""))
					vl.Cells = append(vl.Cells, perfCell(row, sc.Release.Key == key))
				}
				if found {
					line.Volunteer = vl
				}
			}
			sec.Lines = append(sec.Lines, line)
		}
		sec.Count = reports.Plural(perfSetRows(a.set, name), "benchmark")
		page.Sections = append(page.Sections, sec)
	}

	names := map[string]bool{}
	for _, sc := range shown {
		for name := range sc.Stack {
			names[name] = true
		}
		for _, name := range sc.StackDiffers {
			names[name] = true
		}
	}
	for name := range names {
		page.Packages = append(page.Packages, name)
	}
	sort.Strings(page.Packages)
	for _, sc := range shown {
		row := perfStackRow{Kernel: sc.Release.Kernel}
		for _, name := range page.Packages {
			v, ok := sc.Stack[name]
			switch {
			case slices.Contains(sc.StackDiffers, name):
				v = "differs between its Macs"
			case !ok:
				v = "–"
			}
			row.Versions = append(row.Versions, v)
		}
		page.Stacks = append(page.Stacks, row)
	}

	for _, m := range a.set.Metrics {
		if m.Category != sets.OmarchyOnly {
			continue
		}
		line := perfOnlyLine{Name: m.Name, Unit: m.Unit + " · " + perfDirection(m.HigherIsBetter())}
		measured := false
		for _, sc := range shown {
			cell := perfOnlyCell{Value: "–", Current: sc.Release.Key == key}
			if only := c.OmarchyOnlyColumn(sc.Release.Key); only != nil {
				for _, row := range only.Rows {
					if row.Metric != m.ID {
						continue
					}
					measured = true
					if row.Value == nil {
						cell.Value = perfReason(row.Reason)
					} else {
						cell.Value = perfNumber(*row.Value) + " " + m.Unit
					}
					if row.Change != nil {
						cell.Change = perfChange(*row.Change) + " on the Release before"
					}
				}
			}
			line.Cells = append(line.Cells, cell)
		}
		if measured {
			page.OmarchyOnly = append(page.OmarchyOnly, line)
		}
	}
	s.render(w, r, "performance/show", web.Page{Title: c.Name + " performance", Nav: "performance", Wide: true, Data: page})
}

// perfStackChanges are the packages col booted at other versions than
// older, the column after it, but the kernel's own, which every Release
// changes.
func perfStackChanges(col, older *reports.PerfColumn) []string {
	var out []string
	for name, version := range col.Stack {
		if strings.HasPrefix(name, "linux") || slices.Contains(older.StackDiffers, name) {
			continue
		}
		if was, ok := older.Stack[name]; !ok || was != version {
			out = append(out, name)
		}
	}
	for name := range older.Stack {
		if _, ok := col.Stack[name]; !ok && !strings.HasPrefix(name, "linux") && !slices.Contains(col.StackDiffers, name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func perfChange(v float64) string {
	r := math.Round(v*10) / 10
	switch {
	case r > 0:
		return "+" + strconv.FormatFloat(r, 'f', 1, 64) + "%"
	case r < 0:
		return "−" + strconv.FormatFloat(-r, 'f', 1, 64) + "%"
	}
	return "±0%"
}

func perfSectionID(category string) string {
	return "category-" + strings.ToLower(strings.ReplaceAll(category, " ", "-"))
}

// perfSetRows is how many of the set's benchmarks are in the category.
func perfSetRows(set *sets.Set, category string) int {
	n := 0
	for _, m := range set.Metrics {
		if m.Category == category {
			n++
		}
	}
	return n
}

type perfRowKey struct {
	key, metric string
	capacity    *int64
}

// perfKeys are a category's rows across the macOS column and the columns
// shown, in the set's order: every benchmark of the category, Storage's at
// each SSD capacity measured, smallest first, then any row with a reason
// and no capacity.
func perfKeys(set *sets.Set, category string, macOS []*reports.PerfRow, shown []*reports.PerfColumn) []perfRowKey {
	seen := map[string]bool{}
	var all []perfRowKey
	capacities := map[int64]bool{}
	put := func(metric string, capacity *int64) {
		k := perfRowKey{key: metric, metric: metric, capacity: capacity}
		if capacity != nil {
			k.key = metric + "@" + strconv.FormatInt(*capacity, 10)
			capacities[*capacity] = true
		}
		if !seen[k.key] {
			seen[k.key] = true
			all = append(all, k)
		}
	}
	add := func(rows []*reports.PerfRow) {
		for _, row := range rows {
			if row.Category == category && (row.Ratio != nil || row.MacOS != nil || row.Reason != "") {
				put(row.Metric, row.Capacity)
			}
		}
	}
	add(macOS)
	for _, col := range shown {
		add(col.Rows)
	}
	order := map[string]int{}
	var sizes []int64
	for gb := range capacities {
		sizes = append(sizes, gb)
	}
	slices.Sort(sizes)
	for i, m := range set.Metrics {
		order[m.ID] = i
		if m.Category != category {
			continue
		}
		if len(sizes) == 0 {
			put(m.ID, nil)
		}
		for _, gb := range sizes {
			put(m.ID, &gb)
		}
	}
	capacity := func(k perfRowKey) int64 {
		if k.capacity == nil {
			return math.MaxInt64
		}
		return *k.capacity
	}
	sort.SliceStable(all, func(i, j int) bool {
		if a, b := capacity(all[i]), capacity(all[j]); a != b {
			return a < b
		}
		return order[all[i].metric] < order[all[j].metric]
	})
	return all
}
