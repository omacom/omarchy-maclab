package reports

import (
	"encoding/json"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

// The Performance data, GET /api/v1/performance.json (CC0): one set's
// figures for one Release, per Configuration the macOS build, the Macs
// paired, coverage, and per metric the median values, the ratio and any
// reason code. A Release is named by its kernel version and verdict state
// alone, and a Mac by nothing at all.

type perfExport struct {
	License        perfLicense        `json:"license"`
	GeneratedAt    string             `json:"generated_at"`
	Set            string             `json:"set"`
	Release        *perfExportRelease `json:"release"`
	Volunteers     bool               `json:"volunteers"`
	Configurations []perfExportConfig `json:"configurations"`
}

type perfLicense struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

type perfExportRelease struct {
	Kernel  string `json:"kernel"`
	Verdict string `json:"verdict"`
}

type perfExportConfig struct {
	Board      string                `json:"board"`
	Name       string                `json:"name"`
	Chip       string                `json:"chip"`
	CPU        int                   `json:"cpu_cores"`
	GPU        int                   `json:"gpu_cores"`
	Headline   *perfExportComparison `json:"headline"`
	Volunteers *perfExportComparison `json:"volunteers,omitempty"`
}

type perfExportComparison struct {
	Volunteer    bool                 `json:"volunteer"`
	MacOSVersion string               `json:"macos_version"`
	MacOSBuild   string               `json:"macos_build"`
	MacsPaired   int                  `json:"macs_paired"`
	Coverage     PerfCoverage         `json:"coverage"`
	Overall      *float64             `json:"overall"`
	Categories   []perfExportCategory `json:"categories"`
	Metrics      []perfExportMetric   `json:"metrics"`
}

type perfExportCategory struct {
	Name     string         `json:"name"`
	Figure   *float64       `json:"figure"`
	Coverage PerfCoverage   `json:"coverage"`
	Excluded []PerfExcluded `json:"excluded"`
}

type perfExportMetric struct {
	Metric         string   `json:"metric"`
	Category       string   `json:"category"`
	Unit           string   `json:"unit"`
	HigherIsBetter bool     `json:"higher_is_better"`
	Capacity       *int64   `json:"capacity,omitempty"`
	Macs           int      `json:"macs"`
	Omarchy        *float64 `json:"omarchy"`
	MacOS          *float64 `json:"macos"`
	Ratio          *float64 `json:"ratio"`
	Reason         *string  `json:"reason"`
}

// JSON is the Performance data for release (nil: none published).
func (f *PerfFigures) JSON(release *PerfRelease, at time.Time) []byte {
	out := perfExport{License: perfLicense{LicenseID, LicenseURL}, GeneratedAt: iso8601(at), Set: f.Set.Name,
		Volunteers: f.Volunteers, Configurations: []perfExportConfig{}}
	key := ""
	if release != nil {
		out.Release, key = &perfExportRelease{release.Kernel, release.Verdict}, release.Key
	}
	for _, c := range f.Configurations {
		e := perfExportConfig{Board: c.Board, Name: c.Name, Chip: c.ChipName, CPU: c.CPU, GPU: c.GPU}
		e.Headline = perfComparison(c.Headline, key, c.OmarchyOnlyColumn(key))
		e.Volunteers = perfComparison(c.Volunteers, key, nil)
		out.Configurations = append(out.Configurations, e)
	}
	b, _ := json.Marshal(out)
	return b
}

func perfComparison(g *PerfGroup, release string, only *PerfOnlyColumn) *perfExportComparison {
	if g == nil {
		return nil
	}
	cohort := g.Cohorts[0]
	col := cohort.Column(release)
	if col == nil {
		return nil
	}
	e := &perfExportComparison{Volunteer: g.Volunteer, MacOSVersion: cohort.MacOSVersion, MacOSBuild: cohort.MacOSBuild,
		MacsPaired: col.Macs, Coverage: col.Coverage, Overall: col.Overall, Metrics: []perfExportMetric{}}
	for _, cat := range col.Categories {
		excluded := cat.Excluded
		if excluded == nil {
			excluded = []PerfExcluded{}
		}
		e.Categories = append(e.Categories, perfExportCategory{cat.Name, cat.Figure, cat.Coverage, excluded})
	}
	for _, row := range col.Rows {
		e.Metrics = append(e.Metrics, perfExportMetric{Metric: row.Metric, Category: row.Category, Unit: row.Unit,
			HigherIsBetter: row.HigherIsBetter, Capacity: row.Capacity, Macs: row.Macs, Omarchy: row.Omarchy, MacOS: row.MacOS,
			Ratio: row.Ratio, Reason: reasonOrNil(row.Reason)})
	}
	if only != nil {
		for _, row := range only.Rows {
			e.Metrics = append(e.Metrics, perfExportMetric{Metric: row.Metric, Category: sets.OmarchyOnly, Unit: row.Unit,
				HigherIsBetter: row.HigherIsBetter, Macs: row.Macs, Omarchy: row.Value, Reason: reasonOrNil(row.Reason)})
		}
	}
	return e
}

func reasonOrNil(reason string) *string {
	if reason == "" {
		return nil
	}
	return &reason
}
