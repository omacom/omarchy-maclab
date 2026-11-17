package reports

import (
	"strings"
	"time"
)

// Everything public, for download: the published reports as JSON (each
// report as uploaded, with the build the lab derives from it beside it) and
// as CSV, one row per report or one row per check result; and the matrix as
// JSON. Published under CC0. Hidden and deleted reports are never included,
// nor is a Machine key, nor who a Mac was signed in as: a run is a Verified
// run or a Community run and no more. The member names are the old site's.

const (
	LicenseID  = "CC0-1.0"
	LicenseURL = "https://creativecommons.org/publicdomain/zero/1.0/"
)

func license() *Object { return obj("id", LicenseID, "url", LicenseURL) }

var ReportColumns = strings.Fields(`
	id url uploaded_at model board soc chip kernel stack omarchy_version distro boot_loader encryption candidate_set
	tool_version schema_version catalogue_version checks pass fail skip
	build build_words build_commit build_stamp linux_aurora omarchy_mac_boot build_candidate_set build_candidate_set_source
	image_profile image_built expected_gaps`)

var CheckColumns = strings.Fields(`
	report_id uploaded_at model board soc chip kernel stack omarchy_version
	check_id kind status outcome feature layer expected_asahi expected_aurora expected_omarchy
	score score_unit score_tool score_suite answered_by_default build`)

func iso8601(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05Z") }

// Values are the filters picked, by name, in the order the lab lists them.
func (f *Filter) Values() *Object {
	values := obj()
	for _, key := range f.keys {
		values.put(key, f.values[key])
	}
	return values
}

// Checks are the checks of a run this filter keeps (the checks export): all
// but answers by default when confirmed, of one layer, with one result.
func (f *Filter) Checks(run *Run) []*Check {
	var kept []*Check
	for _, check := range run.Checks() {
		switch {
		case f.Confirmed() && check.AnsweredByDefault:
		case f.Layer() != "" && check.Layer != f.Layer():
		case f.State() != "" && !oneIn(filterStates[f.State()], run.CheckState(check)):
		default:
			kept = append(kept, check)
		}
	}
	return kept
}

func oneIn(list []string, item string) bool {
	for _, known := range list {
		if known == item {
			return true
		}
	}
	return false
}

// JSON is the build as the exports carry it.
func (b *Build) JSON() *Object {
	if b == nil {
		return nil
	}
	var image *Object
	if len(b.Image) > 0 {
		image = obj()
		for _, fact := range b.Image {
			image.put(fact.Key, fact.Value)
		}
	}
	return obj("id", b.ID, "label", b.Label(), "words", b.Words(), "commit", b.Commit, "stamp", b.Stamp, "linux_aurora", b.Kernel, "omarchy_mac_boot", b.Boot,
		"candidate_set", b.Set(), "candidate_set_source", b.SetSource(), "image", image, "tool_version", b.ToolVersion).compact()
}

func buildJSON(run *Run) any {
	if build := run.Build(); build != nil {
		return build.JSON()
	}
	return nil
}

// ReportsJSON is every published report the filter's run filters pass,
// oldest first. urlFor gives a report's link.
func (p *Published) ReportsJSON(filter *Filter, urlFor func(*Run) string, at time.Time) []byte {
	list := []any{}
	for _, run := range filter.Runs(p.runs) {
		list = append(list, obj("id", run.PublicID, "url", urlFor(run), "uploaded_at", iso8601(run.CreatedAt), "build", buildJSON(run), "report", run.doc).compact())
	}
	export := obj("license", license(), "generated_at", iso8601(at), "catalogue_version", p.cat.version)
	if filter.Any() {
		export.put("filters", filter.Values())
	}
	export.put("reports", list)
	return JSON(export)
}

// ReportsCSV is one row per published report the filter passes. With
// confirmed only, the check counts leave out the answers given by a bare Enter.
func (p *Published) ReportsCSV(filter *Filter, urlFor func(*Run) string) []byte {
	rows := [][]any{}
	for _, run := range filter.Runs(p.runs) {
		checks := run.Checks()
		if filter.Confirmed() {
			checks = run.CountedChecks()
		}
		statuses := map[string]int{}
		for _, check := range checks {
			statuses[check.Status]++
		}
		build := run.Build()
		if build == nil {
			build = &Build{}
		}
		rows = append(rows, []any{run.PublicID, urlFor(run), iso8601(run.CreatedAt), run.Model(), run.Board(), run.SoC(), run.Chip(),
			run.Kernel(), run.Stack(), run.OmarchyVersion(), member(run.doc, "system", "distro"), member(run.doc, "system", "boot_loader"),
			member(run.doc, "system", "encryption"), run.CandidateSet(), run.ToolVersion(), run.SchemaVersion, member(run.doc, "catalogue_version"),
			len(checks), statuses["pass"], statuses["fail"], statuses["skip"],
			build.ID, wordsOf(run), build.Commit, build.Stamp, build.Kernel, build.Boot, build.Set(), build.SetSource(),
			build.fact("image_profile"), build.fact("built"), run.ResultCounts(filter.Confirmed()).Gap})
	}
	return csv(ReportColumns, rows)
}

func wordsOf(run *Run) string {
	if build := run.Build(); build != nil {
		return build.Words()
	}
	return ""
}

func buildID(run *Run) string {
	if build := run.Build(); build != nil {
		return build.ID
	}
	return ""
}

// ChecksCSV is one row per check result of the published reports the filter
// passes; its result filters pick the rows too (Filter.Checks).
func (p *Published) ChecksCSV(filter *Filter) []byte {
	rows := [][]any{}
	for _, run := range filter.Runs(p.runs) {
		for _, check := range filter.Checks(run) {
			row := []any{run.PublicID, iso8601(run.CreatedAt), run.Model(), run.Board(), run.SoC(), run.Chip(), run.Kernel(),
				run.Stack(), run.OmarchyVersion(), check.ID, check.Kind(), check.Status, check.Outcome, check.Feature, check.Layer}
			for _, layer := range Layers {
				row = append(row, check.Expected(layer))
			}
			for _, field := range []string{"value", "unit", "tool", "suite"} {
				row = append(row, member(check.Score(), field))
			}
			rows = append(rows, append(row, check.AnsweredByDefault, buildID(run)))
		}
	}
	return csv(CheckColumns, rows)
}

// csv is RFC 4180: every field quoted, quotes doubled, CRLF line ends.
// Uploaded text starting like a formula gets a leading ' so spreadsheets
// show it as text.
func csv(header []string, rows [][]any) []byte {
	var out []byte
	line := func(fields []any) {
		for i, field := range fields {
			if i > 0 {
				out = append(out, ',')
			}
			out = append(out, '"')
			out = append(out, strings.ReplaceAll(defuse(rubyText(field)), `"`, `""`)...)
			out = append(out, '"')
		}
		out = append(out, "\r\n"...)
	}
	line(texts(header))
	for _, row := range rows {
		line(row)
	}
	return out
}

func defuse(text string) string {
	if text != "" && strings.ContainsRune("=+-@\t\r", rune(text[0])) {
		return "'" + text
	}
	return text
}

// JSON is the matrix as the export carries it: each cell's state and its
// agreement per machine.
func (m *Matrix) JSON() *Object {
	features := []any{}
	for _, feature := range m.Features() {
		features = append(features, obj("id", id(feature), "name", text(member(feature, "name")), "layer", layer(feature)))
	}
	byModel := m.by == ByModel
	rows := []any{}
	for _, row := range m.Rows() {
		cells := obj()
		for _, feature := range m.Features() {
			cell := row.Cell(id(feature))
			c := obj("state", orNull(cell.State()), "tentative", cell.Tentative(), "machines", cell.Tallies, "tester_machines", cell.VerifiedTallies)
			if byModel {
				sources := cell.Sources()
				var last any
				if len(sources) > 0 {
					last = iso8601(sources[0].CreatedAt)
				}
				c.put("kernels", kernelsOf(sources))
				c.put("last_tested_at", last)
			}
			if cell.Hidden() {
				c.put("hidden", true)
			}
			cells.put(id(feature), c)
		}
		var version, build any
		if !byModel {
			version, build = row.Configuration.Version, buildJSON(row.Runs[0])
		}
		r := obj("model", row.ModelName, "board", row.Configuration.Board, "soc", row.SoC, "chip", row.Chip,
			"stack", row.Configuration.Stack, "version", version, "build", build, "reports", len(row.Runs), "cells", cells)
		if byModel {
			r.put("releases", uniq(row.Runs, (*Run).OmarchyVersion))
		}
		rows = append(rows, r)
	}
	return obj("catalogue_version", m.pub.cat.version,
		"agreement", "a cell's state is set by tester runs on their own, or once 2 or more distinct machines agree; null while unconfirmed",
		"features", features, "rows", rows)
}

func kernelsOf(runs []*Run) []any { return uniq(runs, (*Run).Kernel) }

// uniq is what of gives for each run, each value once, in the runs' order.
func uniq(runs []*Run, of func(*Run) string) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, run := range runs {
		if value := of(run); !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func uniqTexts(runs []*Run, of func(*Run) string) []string {
	var out []string
	for _, value := range uniq(runs, of) {
		out = append(out, value.(string))
	}
	return out
}

// MatrixJSON is the matrix export: grouped by build, or with group
// "release" or "model"; with the filter's filters and the runs behind it
// when any is picked.
func (p *Published) MatrixJSON(group string, filter *Filter, at time.Time) []byte {
	by := ByBuild
	switch group {
	case "release":
		by = ByRelease
	case "model":
		by = ByModel
	}
	matrix := p.Matrix(by, filter)
	export := obj("license", license(), "generated_at", iso8601(at))
	if filter.Any() || by == ByModel {
		export.put("filters", filter.Values())
		export.put("runs", matrix.RunsCount())
	}
	body := matrix.JSON()
	for _, key := range body.keys {
		export.put(key, body.members[key])
	}
	if by == ByModel {
		export.put("group", "model")
	}
	return JSON(export)
}
