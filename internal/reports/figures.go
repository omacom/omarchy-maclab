package reports

// The figures (spec section 22, "The figures"): from what reports.db holds of
// Performance, one Benchmark set's comparison of Omarchy with macOS on the
// same Macs, Configuration by Configuration, for the pages and their data.

import (
	"errors"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// A figure's colour. Within its tolerance grey; beyond it, faster than macOS
// green, 85% and over yellow, 60% to 85% orange, under 60% red.
const (
	PerfGrey   = "grey"
	PerfGreen  = "green"
	PerfYellow = "yellow"
	PerfOrange = "orange"
	PerfRed    = "red"
)

// PerfCategories are the eight categories Overall weighs equally, in page
// order.
var PerfCategories = slices.DeleteFunc(slices.Clone(sets.Categories), func(c string) bool { return c == sets.OmarchyOnly })

// perfGapReasons are the reason codes a row is listed with among the
// biggest gaps, after the rows that have a ratio.
var perfGapReasons = []string{perf.NoBuild, perf.NoDriver}

// PerfFigures is one set's figures: every Configuration with a Mac shown,
// each across every published Release it has values for.
type PerfFigures struct {
	Set            *sets.Set
	Volunteers     bool
	Releases       []PerfRelease
	Configurations []*PerfConfiguration
	cat            *Catalogue
}

// PerfConfiguration is one Configuration: its Macs shown by kind and their
// SSD capacities, its headline comparison and its Volunteer line. Headline
// is nil when it has no Mac with a macOS result for the set, or when only
// Volunteers' Macs have and Volunteers are hidden. VolunteerHeadline: no
// Lab-owned or Counted Mac, so the headline is its Volunteers', marked.
type PerfConfiguration struct {
	Board             string
	Name              string
	Chip              string
	ChipName          string
	CPU               int
	GPU               int
	LabOwned          int
	Counted           int
	Volunteer         int
	Capacities        []int64
	VolunteerHeadline bool
	Headline          *PerfGroup
	Volunteers        *PerfGroup
	OmarchyOnly       []*PerfOnlyColumn
}

// PerfGroup is a group of Macs compared with macOS: by macOS build, the
// newest first. Cohorts[0] is the comparison; the Macs of the others are on
// older builds, shown apart, and wait for their macOS run.
type PerfGroup struct {
	Volunteer bool
	Cohorts   []*PerfCohort
}

// PerfCohort is the Macs of a group whose newest macOS result for the set is
// at one macOS build. MacOS is the macOS column; Columns are the Releases
// they have Omarchy values for, the newest first.
type PerfCohort struct {
	MacOSVersion string
	MacOSBuild   string
	Measured     time.Time
	Macs         int
	MacOS        []*PerfRow
	Columns      []*PerfColumn
}

// PerfColumn is one Release against macOS: its rows in set order (Storage per
// SSD capacity), the eight categories and Overall, the Macs it pairs, and
// what they booted. Stack holds the versions every Mac booted alike;
// StackDiffers names the packages they did not; StackChanged says it differs
// from the trend's Release.
type PerfColumn struct {
	Release       PerfRelease
	Macs          int
	Rows          []*PerfRow
	Categories    []*PerfCategory
	Overall       *float64
	OverallColour string
	OverallTrend  *float64
	Coverage      PerfCoverage
	Excluded      []PerfExcluded
	Stack         map[string]string
	StackDiffers  []string
	StackChanged  bool
	overallSig    string
}

// PerfRow is one metric (on Storage, at one SSD capacity): the median
// Omarchy and macOS values of the Macs it pairs, the median of their ratios
// as a percent of macOS (higher is better throughout), or, with no ratio,
// the reason code most of them gave. Trend is the change in points from the
// trend's Release, drawn only for the same Macs.
type PerfRow struct {
	Metric         string
	Name           string
	Category       string
	Unit           string
	HigherIsBetter bool
	Tolerance      float64
	APIDiffers     bool
	OmarchyAPI     string
	MacOSAPI       string
	Capacity       *int64
	Macs           int
	Omarchy        *float64
	MacOS          *float64
	Ratio          *float64
	Reason         string
	Colour         string
	Trend          *float64
	macs           []string
}

// PerfCategory is a category's figure, the geometric mean of its rows with a
// ratio, and its worst row; rows with a reason code are listed in Excluded
// and count in no mean.
type PerfCategory struct {
	Name     string
	Figure   *float64
	Colour   string
	Trend    *float64
	Coverage PerfCoverage
	Excluded []PerfExcluded
	Worst    *PerfRow
	sig      string
}

// PerfCoverage is the rows measured, with a ratio, of the set's rows.
type PerfCoverage struct {
	Measured int `json:"measured"`
	Rows     int `json:"rows"`
}

// PerfExcluded is a row kept out of the means, and why.
type PerfExcluded struct {
	Metric   string `json:"metric"`
	Capacity *int64 `json:"capacity,omitempty"`
	Reason   string `json:"reason"`
}

// PerfGap is a row among the biggest gaps, of its Configuration.
type PerfGap struct {
	Configuration *PerfConfiguration
	Row           *PerfRow
}

// PerfOnlyColumn is one Release's Omarchy-only rows (boot time) over the
// headline Macs, whatever their macOS build.
type PerfOnlyColumn struct {
	Release PerfRelease
	Rows    []*PerfOnlyRow
}

// PerfOnlyRow is an Omarchy-only metric's median value, or its reason code,
// and Change, its change in percent from the Release before with values,
// drawn only for the same Macs.
type PerfOnlyRow struct {
	Metric         string
	Name           string
	Unit           string
	HigherIsBetter bool
	Macs           int
	Value          *float64
	Reason         string
	Change         *float64
	macs           []string
}

// PerfSet is the Benchmark set a page or the data asks for: "bench-N" or N,
// or, with none named, the current set, or the newest while every set holds
// a placeholder.
func PerfSet(name string) (*sets.Set, error) {
	if name == "" {
		s, err := sets.Current()
		if errors.Is(err, sets.ErrNoSet) {
			return sets.Newest()
		}
		return s, err
	}
	if _, err := strconv.Atoi(name); err == nil {
		name = "bench-" + name
	}
	return sets.Load(name)
}

// PerformanceFigures is one set's figures from what reports.db holds, with
// Volunteers' Macs or without.
func (s *Store) PerformanceFigures(cat *Catalogue, set *sets.Set, volunteers bool) (*PerfFigures, error) {
	p, err := s.Performance()
	if err != nil {
		return nil, err
	}
	return p.Figures(set, cat, volunteers), nil
}

type perfIndex struct {
	omarchy map[string]map[string]map[string]*PerfValue // Mac, Release, metric
	macos   map[string]map[string]map[string]*PerfValue // Mac, build, metric
}

func (x perfIndex) om(mac, release, metric string) *PerfValue {
	return x.omarchy[mac][release][metric]
}

// Figures is set's figures from p.
func (p Performance) Figures(set *sets.Set, cat *Catalogue, volunteers bool) *PerfFigures {
	f := &PerfFigures{Set: set, Volunteers: volunteers, cat: cat}
	f.Releases = slices.Clone(p.Releases)
	sort.Slice(f.Releases, func(i, j int) bool { return f.Releases[i].Position > f.Releases[j].Position })

	x := perfIndex{omarchy: map[string]map[string]map[string]*PerfValue{}, macos: map[string]map[string]map[string]*PerfValue{}}
	put := func(m map[string]map[string]map[string]*PerfValue, a, b string, v *PerfValue) {
		if m[a] == nil {
			m[a] = map[string]map[string]*PerfValue{}
		}
		if m[a][b] == nil {
			m[a][b] = map[string]*PerfValue{}
		}
		m[a][b][v.Metric] = v
	}
	for i := range p.Values {
		v := &p.Values[i]
		if v.Set != set.Name {
			continue
		}
		if _, ok := set.Metric(v.Metric); !ok {
			continue
		}
		if v.Side == perf.SideOmarchy {
			put(x.omarchy, v.Mac, v.Release, v)
		} else {
			put(x.macos, v.Mac, v.MacOSBuild, v)
		}
	}

	type configKey struct {
		board    string
		cpu, gpu int
	}
	byConfig := map[configKey][]PerfMac{}
	var keys []configKey
	for _, m := range p.Macs {
		if m.Kind == PerfVolunteer && !volunteers {
			continue
		}
		k := configKey{m.Board, m.CPU, m.GPU}
		if byConfig[k] == nil {
			keys = append(keys, k)
		}
		byConfig[k] = append(byConfig[k], m)
	}
	for _, k := range keys {
		f.Configurations = append(f.Configurations, f.configuration(x, k.board, k.cpu, k.gpu, byConfig[k]))
	}
	sort.Slice(f.Configurations, func(i, j int) bool {
		a, b := f.Configurations[i], f.Configurations[j]
		if ra, rb := f.chipRank(a.Chip), f.chipRank(b.Chip); ra != rb {
			return ra < rb
		}
		if a.Board != b.Board {
			return a.Board < b.Board
		}
		if a.CPU != b.CPU {
			return a.CPU < b.CPU
		}
		return a.GPU < b.GPU
	})
	return f
}

func (f *PerfFigures) chipRank(chip string) int {
	if chip == "" || f.cat == nil {
		return math.MaxInt
	}
	return f.cat.generationRank(chip)
}

func (f *PerfFigures) configuration(x perfIndex, board string, cpu, gpu int, macs []PerfMac) *PerfConfiguration {
	c := &PerfConfiguration{Board: board, Name: board, CPU: cpu, GPU: gpu}
	if f.cat != nil {
		if name, ok := member(f.cat.Boards(), board, "name").(string); ok {
			c.Name = name
		}
		if chip, ok := member(f.cat.Boards(), board, "chip").(string); ok {
			c.Chip, c.ChipName = chip, f.cat.ChipName(chip)
		}
	}
	var headline, others []string
	capacities := map[int64]bool{}
	for _, m := range macs {
		switch m.Kind {
		case PerfLabOwned:
			c.LabOwned++
		case PerfCounted:
			c.Counted++
		case PerfVolunteer:
			c.Volunteer++
		}
		if m.Kind == PerfVolunteer {
			others = append(others, m.Key)
		} else {
			headline = append(headline, m.Key)
		}
		for _, side := range []map[string]map[string]map[string]*PerfValue{x.omarchy, x.macos} {
			for _, values := range side[m.Key] {
				for _, v := range values {
					if v.Capacity != nil {
						capacities[*v.Capacity] = true
					}
				}
			}
		}
	}
	for capacity := range capacities {
		c.Capacities = append(c.Capacities, capacity)
	}
	slices.Sort(c.Capacities)
	sort.Strings(headline)
	sort.Strings(others)
	if len(headline) == 0 {
		headline, others, c.VolunteerHeadline = others, nil, true
	}
	c.Headline = f.group(x, headline, c.VolunteerHeadline)
	c.Volunteers = f.group(x, others, true)
	c.OmarchyOnly = f.omarchyOnly(x, headline)
	return c
}

// group is macs' cohorts by the newest macOS build each has a result at, or
// nil when none has one.
func (f *PerfFigures) group(x perfIndex, macs []string, volunteer bool) *PerfGroup {
	type build struct {
		version, build string
		measured       time.Time
	}
	builds := map[string]*build{}
	newest := map[string]*build{}
	for _, mac := range macs {
		for name, values := range x.macos[mac] {
			b := builds[name]
			if b == nil {
				b = &build{build: name}
				builds[name] = b
			}
			for _, v := range values {
				if b.version == "" {
					b.version = v.MacOSVersion
				}
				if v.Measured.After(b.measured) {
					b.measured = v.Measured
				}
			}
		}
	}
	for _, mac := range macs {
		for name := range x.macos[mac] {
			if n := newest[mac]; n == nil || compareMacOS(builds[name].version, name, n.version, n.build) > 0 {
				newest[mac] = builds[name]
			}
		}
	}
	cohortMacs := map[*build][]string{}
	for _, mac := range macs {
		if b := newest[mac]; b != nil {
			cohortMacs[b] = append(cohortMacs[b], mac)
		}
	}
	if len(cohortMacs) == 0 {
		return nil
	}
	var order []*build
	for b := range cohortMacs {
		order = append(order, b)
	}
	sort.Slice(order, func(i, j int) bool {
		return compareMacOS(order[i].version, order[i].build, order[j].version, order[j].build) > 0
	})
	g := &PerfGroup{Volunteer: volunteer}
	for _, b := range order {
		in := cohortMacs[b]
		measured := time.Time{}
		for _, mac := range in {
			for _, v := range x.macos[mac][b.build] {
				if v.Measured.After(measured) {
					measured = v.Measured
				}
			}
		}
		g.Cohorts = append(g.Cohorts, f.cohort(x, in, b.version, b.build, measured))
	}
	return g
}

func (f *PerfFigures) comparable() []sets.Metric {
	var out []sets.Metric
	for _, m := range f.Set.Metrics {
		if m.Category != sets.OmarchyOnly && m.Omarchy != nil && m.MacOS != nil {
			out = append(out, m)
		}
	}
	return out
}

func (f *PerfFigures) cohort(x perfIndex, macs []string, version, build string, measured time.Time) *PerfCohort {
	c := &PerfCohort{MacOSVersion: version, MacOSBuild: build, Measured: measured, Macs: len(macs)}
	mac := func(m sets.Metric) func(string) (*PerfValue, *PerfValue) {
		return func(key string) (*PerfValue, *PerfValue) { return nil, x.macos[key][build][m.ID] }
	}
	for _, m := range f.comparable() {
		c.MacOS = append(c.MacOS, perfRows(m, macs, true, mac(m))...)
	}
	for _, r := range f.Releases {
		var in []string
		for _, key := range macs {
			if len(x.omarchy[key][r.Key]) > 0 {
				in = append(in, key)
			}
		}
		if len(in) > 0 {
			c.Columns = append(c.Columns, f.column(x, r, in, build))
		}
	}
	for i, col := range c.Columns {
		for _, older := range c.Columns[i+1:] {
			if older.Release.Verdict == PerfQualified {
				col.trend(older)
				break
			}
		}
	}
	return c
}

func (f *PerfFigures) column(x perfIndex, r PerfRelease, macs []string, build string) *PerfColumn {
	col := &PerfColumn{Release: r, Macs: len(macs)}
	byCategory := map[string][]*PerfRow{}
	measured := map[string]bool{}
	for _, m := range f.comparable() {
		rows := perfRows(m, macs, false, func(key string) (*PerfValue, *PerfValue) {
			return x.om(key, r.Key, m.ID), x.macos[key][build][m.ID]
		})
		for _, row := range rows {
			if row.Ratio != nil {
				measured[m.ID] = true
			}
		}
		col.Rows = append(col.Rows, rows...)
		byCategory[m.Category] = append(byCategory[m.Category], rows...)
	}
	var figures []float64
	var sigs []string
	// A category's figure, and Overall's, is grey within the widest
	// tolerance of the rows it counts.
	tolerance := 0.0
	for _, name := range PerfCategories {
		cat := &PerfCategory{Name: name}
		var ratios []float64
		catTolerance := 0.0
		metrics := map[string]bool{}
		for _, row := range byCategory[name] {
			metrics[row.Metric] = true
			switch {
			case row.Ratio != nil:
				ratios = append(ratios, *row.Ratio)
				catTolerance = max(catTolerance, row.Tolerance)
				sigs = append(sigs, row.sig())
				if cat.Worst == nil || *row.Ratio < *cat.Worst.Ratio {
					cat.Worst = row
				}
			case row.Reason != "":
				cat.Excluded = append(cat.Excluded, PerfExcluded{Metric: row.Metric, Capacity: row.Capacity, Reason: row.Reason})
			}
		}
		for metric := range metrics {
			cat.Coverage.Rows++
			if measured[metric] {
				cat.Coverage.Measured++
			}
		}
		cat.sig = strings.Join(rowSigs(byCategory[name]), ";")
		if len(ratios) > 0 {
			g := geomean(ratios)
			cat.Figure, cat.Colour = &g, perfColour(g, catTolerance)
			figures = append(figures, g)
			tolerance = max(tolerance, catTolerance)
		}
		col.Coverage.Rows += cat.Coverage.Rows
		col.Coverage.Measured += cat.Coverage.Measured
		col.Excluded = append(col.Excluded, cat.Excluded...)
		col.Categories = append(col.Categories, cat)
	}
	if len(figures) == len(PerfCategories) {
		g := geomean(figures)
		col.Overall, col.OverallColour = &g, perfColour(g, tolerance)
		col.overallSig = strings.Join(sigs, ";")
	}

	var booted []map[string]string
	names := map[string]bool{}
	for _, key := range macs {
		b := map[string]string{}
		for _, v := range x.omarchy[key][r.Key] {
			for name, version := range v.Booted {
				b[name], names[name] = version, true
			}
		}
		booted = append(booted, b)
	}
	for name := range names {
		version, alike := booted[0][name], true
		for _, b := range booted[1:] {
			alike = alike && b[name] == version
		}
		switch {
		case !alike:
			col.StackDiffers = append(col.StackDiffers, name)
		case col.Stack == nil:
			col.Stack = map[string]string{name: version}
		default:
			col.Stack[name] = version
		}
	}
	sort.Strings(col.StackDiffers)
	return col
}

// trend draws col's trends from older, like with like: a row's for the same
// Macs, a category's and Overall's for the same rows and the same Macs in
// each. The macOS build is the cohort's, and Storage's rows are per SSD
// capacity.
func (col *PerfColumn) trend(older *PerfColumn) {
	rows := map[string]*PerfRow{}
	for _, row := range older.Rows {
		rows[row.key()] = row
	}
	for _, row := range col.Rows {
		if o := rows[row.key()]; o != nil && row.Ratio != nil && o.Ratio != nil && slices.Equal(row.macs, o.macs) {
			t := *row.Ratio - *o.Ratio
			row.Trend = &t
		}
	}
	for i, cat := range col.Categories {
		o := older.Categories[i]
		if cat.Figure != nil && o.Figure != nil && cat.sig == o.sig {
			t := *cat.Figure - *o.Figure
			cat.Trend = &t
		}
	}
	if col.Overall != nil && older.Overall != nil && col.overallSig == older.overallSig {
		t := *col.Overall - *older.Overall
		col.OverallTrend = &t
	}
	col.StackChanged = !maps.Equal(col.Stack, older.Stack) || !slices.Equal(col.StackDiffers, older.StackDiffers)
}

func (r *PerfRow) key() string {
	if r.Capacity == nil {
		return r.Metric
	}
	return r.Metric + "@" + strconv.FormatInt(*r.Capacity, 10)
}

func (r *PerfRow) sig() string { return r.key() + ":" + strings.Join(r.macs, ",") }

// rowSigs are the rows with a ratio, each with the Macs it pairs: two
// columns whose rows give the same are like for like.
func rowSigs(rows []*PerfRow) []string {
	var out []string
	for _, row := range rows {
		if row.Ratio != nil {
			out = append(out, row.sig())
		}
	}
	return out
}

// perfRows is metric m's rows over macs, one per SSD capacity on Storage.
// values gives a Mac's Omarchy value (nil for the macOS column) and macOS
// value. macOSColumn: the macOS column's rows, of macOS values alone.
func perfRows(m sets.Metric, macs []string, macOSColumn bool, values func(string) (*PerfValue, *PerfValue)) []*PerfRow {
	type entry struct {
		mac    string
		om, mc *PerfValue
	}
	split := m.Category == "Storage"
	groups := map[int64][]entry{}
	var none []entry
	for _, mac := range macs {
		om, mc := values(mac)
		if om == nil && mc == nil {
			continue
		}
		var capacity *int64
		for _, v := range []*PerfValue{om, mc} {
			if capacity == nil && v != nil {
				capacity = v.Capacity
			}
		}
		if split && capacity != nil {
			groups[*capacity] = append(groups[*capacity], entry{mac, om, mc})
		} else {
			none = append(none, entry{mac, om, mc})
		}
	}
	row := func(capacity *int64, entries []entry) *PerfRow {
		r := &PerfRow{Metric: m.ID, Name: m.Name, Category: m.Category, Unit: m.Unit, HigherIsBetter: m.HigherIsBetter(),
			Tolerance: m.Tolerance, APIDiffers: m.APIDiffers, OmarchyAPI: m.Omarchy.API, MacOSAPI: m.MacOS.API, Capacity: capacity}
		var oms, mcs, ratios []float64
		var reasons []string
		for _, e := range entries {
			if macOSColumn {
				if e.mc.Value != nil {
					mcs = append(mcs, *e.mc.Value)
					r.macs = append(r.macs, e.mac)
				} else {
					reasons = append(reasons, *e.mc.Reason)
				}
				continue
			}
			switch {
			case e.om != nil && e.om.Reason != nil:
				reasons = append(reasons, *e.om.Reason)
			case e.om == nil || e.mc == nil:
			case e.mc.Reason != nil:
				reasons = append(reasons, *e.mc.Reason)
			default:
				if ratio, ok := perfRatio(m.HigherIsBetter(), *e.om.Value, *e.mc.Value); ok {
					oms, mcs, ratios = append(oms, *e.om.Value), append(mcs, *e.mc.Value), append(ratios, ratio)
					r.macs = append(r.macs, e.mac)
				}
			}
		}
		r.Macs = len(r.macs)
		r.Omarchy, r.MacOS = median(oms), median(mcs)
		if r.Ratio = median(ratios); r.Ratio != nil {
			r.Colour = perfColour(*r.Ratio, m.Tolerance)
		} else if !macOSColumn || r.MacOS == nil {
			r.Reason = mostReason(reasons)
		}
		return r
	}
	var out []*PerfRow
	var capacities []int64
	for capacity := range groups {
		capacities = append(capacities, capacity)
	}
	slices.Sort(capacities)
	for _, capacity := range capacities {
		out = append(out, row(&capacity, groups[capacity]))
	}
	if len(none) > 0 || len(out) == 0 {
		out = append(out, row(nil, none))
	}
	return out
}

func (f *PerfFigures) omarchyOnly(x perfIndex, macs []string) []*PerfOnlyColumn {
	var metrics []sets.Metric
	for _, m := range f.Set.Metrics {
		if m.Category == sets.OmarchyOnly {
			metrics = append(metrics, m)
		}
	}
	if len(metrics) == 0 {
		return nil
	}
	var out []*PerfOnlyColumn
	for _, r := range f.Releases {
		col := &PerfOnlyColumn{Release: r}
		measured := false
		for _, m := range metrics {
			row := &PerfOnlyRow{Metric: m.ID, Name: m.Name, Unit: m.Unit, HigherIsBetter: m.HigherIsBetter()}
			var values []float64
			var reasons []string
			for _, mac := range macs {
				v := x.om(mac, r.Key, m.ID)
				switch {
				case v == nil:
				case v.Value != nil:
					values = append(values, *v.Value)
					row.macs = append(row.macs, mac)
				default:
					reasons = append(reasons, *v.Reason)
				}
				measured = measured || v != nil
			}
			row.Macs = len(values)
			if row.Value = median(values); row.Value == nil {
				row.Reason = mostReason(reasons)
			}
			col.Rows = append(col.Rows, row)
		}
		if measured {
			out = append(out, col)
		}
	}
	for i, col := range out {
		if i+1 == len(out) {
			break
		}
		for j, row := range col.Rows {
			o := out[i+1].Rows[j]
			if row.Value != nil && o.Value != nil && *o.Value != 0 && slices.Equal(row.macs, o.macs) {
				change := (*row.Value - *o.Value) / *o.Value * 100
				row.Change = &change
			}
		}
	}
	return out
}

// perfRatio is Omarchy's value as a percent of macOS's, so that higher is
// better: for a metric where lower is better (Power, as the energy or watts
// drawn), macOS's over Omarchy's.
func perfRatio(higherIsBetter bool, omarchy, macos float64) (float64, bool) {
	if higherIsBetter {
		if macos <= 0 || omarchy < 0 {
			return 0, false
		}
		return omarchy / macos * 100, true
	}
	if omarchy <= 0 || macos < 0 {
		return 0, false
	}
	return macos / omarchy * 100, true
}

func perfColour(ratio, tolerance float64) string {
	switch {
	case math.Abs(ratio-100) <= tolerance:
		return PerfGrey
	case ratio > 100:
		return PerfGreen
	case ratio >= 85:
		return PerfYellow
	case ratio >= 60:
		return PerfOrange
	}
	return PerfRed
}

func median(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	s := slices.Clone(values)
	slices.Sort(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + m) / 2
	}
	return &m
}

func geomean(values []float64) float64 {
	sum := 0.0
	for _, v := range values {
		if v <= 0 {
			return 0
		}
		sum += math.Log(v)
	}
	return math.Exp(sum / float64(len(values)))
}

// mostReason is the reason code given most, the earlier in perf.Reasons on a
// tie.
func mostReason(reasons []string) string {
	counts := map[string]int{}
	for _, r := range reasons {
		counts[r]++
	}
	best := ""
	for _, code := range perf.Reasons {
		if counts[code] > counts[best] {
			best = code
		}
	}
	return best
}

// compareMacOS orders two macOS results by version, then build ("27C101"
// after "27B5"), and says which is newer.
func compareMacOS(aVersion, aBuild, bVersion, bBuild string) int {
	if aVersion != "" && bVersion != "" {
		if c := compareParts(strings.Split(aVersion, "."), strings.Split(bVersion, ".")); c != 0 {
			return c
		}
	}
	return compareParts(buildParts(aBuild), buildParts(bBuild))
}

func buildParts(build string) []string {
	var parts []string
	for i := 0; i < len(build); {
		j := i + 1
		digit := build[i] >= '0' && build[i] <= '9'
		for j < len(build) && (build[j] >= '0' && build[j] <= '9') == digit {
			j++
		}
		parts = append(parts, build[i:j])
		i = j
	}
	return parts
}

func compareParts(a, b []string) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		switch {
		case i >= len(a):
			return -1
		case i >= len(b):
			return 1
		case a[i] == "" || b[i] == "" || !numeral(a[i]) || !numeral(b[i]):
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		default:
			if c := compareNumbers(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return 0
}

// DefaultRelease is the Release the pages show first: the newest qualified,
// else the newest with no verdict yet; nil with none published.
func (f *PerfFigures) DefaultRelease() *PerfRelease {
	for _, verdict := range []string{PerfQualified, PerfNoVerdictYet} {
		for i := range f.Releases {
			if f.Releases[i].Verdict == verdict {
				return &f.Releases[i]
			}
		}
	}
	return nil
}

// Release is the published Release named by its key or its kernel version
// (the newest with that kernel), or nil.
func (f *PerfFigures) Release(name string) *PerfRelease {
	for i := range f.Releases {
		if f.Releases[i].Key == name {
			return &f.Releases[i]
		}
	}
	for i := range f.Releases {
		if f.Releases[i].Kernel == name {
			return &f.Releases[i]
		}
	}
	return nil
}

// Configuration is the Configuration of that board and core counts, or nil.
func (f *PerfFigures) Configuration(board string, cpu, gpu int) *PerfConfiguration {
	for _, c := range f.Configurations {
		if c.Board == board && c.CPU == cpu && c.GPU == gpu {
			return c
		}
	}
	return nil
}

// Path is the Configuration's place under /performance/: "j314s/10c-16g".
func (c *PerfConfiguration) Path() string {
	return c.Board + "/" + strconv.Itoa(c.CPU) + "c-" + strconv.Itoa(c.GPU) + "g"
}

// Comparison is the headline's comparison, the Macs on the newest macOS
// build, or nil.
func (c *PerfConfiguration) Comparison() *PerfCohort {
	if c.Headline == nil {
		return nil
	}
	return c.Headline.Cohorts[0]
}

// Column is the headline comparison's column for the Release of that key,
// or nil when it has no values for it.
func (c *PerfConfiguration) Column(release string) *PerfColumn {
	return c.Comparison().Column(release)
}

// Column is the cohort's column for the Release of that key, or nil.
func (c *PerfCohort) Column(release string) *PerfColumn {
	if c == nil {
		return nil
	}
	for _, col := range c.Columns {
		if col.Release.Key == release {
			return col
		}
	}
	return nil
}

// Line is the Volunteer line of the row with that key in the Release's
// column: the Volunteers' comparison, or nil.
func (c *PerfConfiguration) Line(release string, row *PerfRow) *PerfRow {
	if c.Volunteers == nil {
		return nil
	}
	if col := c.Volunteers.Cohorts[0].Column(release); col != nil {
		for _, r := range col.Rows {
			if r.key() == row.key() {
				return r
			}
		}
	}
	return nil
}

// Shown are the columns a Configuration's page shows: the newest Release
// with no verdict yet and the four newest qualified, or, with older, every
// qualified one; the newest first.
func (c *PerfCohort) Shown(older bool) []*PerfColumn {
	var out []*PerfColumn
	verdictless, qualified := false, 0
	for _, col := range c.Columns {
		switch {
		case col.Release.Verdict == PerfNoVerdictYet && !verdictless:
			verdictless = true
			out = append(out, col)
		case col.Release.Verdict == PerfQualified && (older || qualified < 4):
			qualified++
			out = append(out, col)
		}
	}
	return out
}

// OmarchyOnlyColumn is the Omarchy-only column for the Release of that key,
// or nil.
func (c *PerfConfiguration) OmarchyOnlyColumn(release string) *PerfOnlyColumn {
	for _, col := range c.OmarchyOnly {
		if col.Release.Key == release {
			return col
		}
	}
	return nil
}

// Gaps are the Configuration's biggest gaps in the Release's column,
// worst first, then the "no build" and "no driver" rows.
func (c *PerfConfiguration) Gaps(release string) []PerfGap {
	var under, missing []PerfGap
	if col := c.Column(release); col != nil {
		for _, row := range col.Rows {
			switch {
			case row.Ratio != nil && *row.Ratio < 100-row.Tolerance:
				under = append(under, PerfGap{c, row})
			case row.Ratio == nil && slices.Contains(perfGapReasons, row.Reason):
				missing = append(missing, PerfGap{c, row})
			}
		}
	}
	gaps := append(under, missing...)
	sortGaps(gaps)
	return gaps
}

// Gaps are the biggest gaps across the lab in the Release's columns: the
// rows furthest under macOS beyond their tolerance, worst first, then the
// "no build" and "no driver" rows.
func (f *PerfFigures) Gaps(release string) []PerfGap {
	var all []PerfGap
	for _, c := range f.Configurations {
		all = append(all, c.Gaps(release)...)
	}
	sortGaps(all)
	return all
}

func sortGaps(gaps []PerfGap) {
	sort.SliceStable(gaps, func(i, j int) bool {
		a, b := gaps[i].Row.Ratio, gaps[j].Row.Ratio
		if (a == nil) != (b == nil) {
			return a != nil
		}
		return a != nil && *a < *b
	})
}

// PerfFamily is a chip family's Configurations, how many have values for
// the Release, and the median of their Overall figures.
type PerfFamily struct {
	Chip           string
	Name           string
	Configurations []*PerfConfiguration
	WithRuns       int
	Median         *float64
}

// Families are the catalogue's chip families in its order, each with its
// Configurations, and last those of boards the catalogue does not know.
func (f *PerfFigures) Families(release string) []*PerfFamily {
	var out []*PerfFamily
	byChip := map[string]*PerfFamily{}
	if f.cat != nil && f.cat.Chips() != nil {
		for _, key := range f.cat.Chips().keys {
			fam := &PerfFamily{Chip: key, Name: f.cat.ChipName(key)}
			byChip[key] = fam
			out = append(out, fam)
		}
	}
	for _, c := range f.Configurations {
		fam := byChip[c.Chip]
		if fam == nil {
			fam = &PerfFamily{Chip: c.Chip, Name: "Other"}
			byChip[c.Chip] = fam
			out = append(out, fam)
		}
		fam.Configurations = append(fam.Configurations, c)
	}
	for _, fam := range out {
		var overall []float64
		for _, c := range fam.Configurations {
			if col := c.Column(release); col != nil {
				fam.WithRuns++
				if col.Overall != nil {
					overall = append(overall, *col.Overall)
				}
			}
		}
		fam.Median = median(overall)
	}
	return out
}
