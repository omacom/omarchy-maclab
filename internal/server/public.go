package server

// The public results pages: the home page, Matrix, Models and Features (and,
// built on the same pieces, the pages that follow them). Each reads the
// published reports once from reports.db (reports.Published), asks it for
// what the page counts, and turns that into the plain values its template
// prints. Nothing here reads labd.db, and no page names a person: a run is a
// Verified run or a Community run and no more.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

// published reads the published reports for one request; nil when it has
// answered w itself.
func (s *Server) published(w http.ResponseWriter) *reports.Published {
	p, err := s.reports.Published(s.catalogue)
	if err != nil {
		s.pageFailed(w, err)
		return nil
	}
	return p
}

func (s *Server) href(path string) string { return s.basePath() + path }

func reportPath(run *reports.Run) string { return "/reports/" + run.PublicID }

func uploadedAtRun(run *reports.Run) string {
	return run.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
}

func dayOf(at time.Time) string { return at.UTC().Format(time.DateOnly) }

// tipView is a result's tooltip: what its colour means, in that colour, how
// sure it is, then where it's from.
type tipView struct {
	State, Swatch, Glyph, Meaning, Confidence, Heading string
	Lines                                              []string
}

// cellView is one result square.
type cellView struct {
	Class, Title, Feature, State, Verified string
	Kernels, TestedAt                      string
	Link, Glyph                            string
	Tip                                    tipView
}

func upcaseFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// confidence says how sure a result is, in plain words: from Verified runs,
// agreed, or outlined until it's confirmed.
func confidence(cell *reports.Cell) string {
	switch {
	case cell.Hidden():
		return "Unconfirmed, hidden by the confirmed-only filter."
	case len(cell.Tallies) == 0:
		return "No run tested it."
	case cell.Verified():
		return "Solid: Verified runs from " + reports.Plural(cell.VerifiedMachines(), "machine") + "."
	case cell.Confirmed():
		return "Solid: " + reports.Plural(cell.Machines(), "machine") + " agree."
	}
	who := fmt.Sprintf("%d machines have", cell.Machines())
	if cell.Machines() == 1 {
		who = "Only 1 machine has"
	}
	return fmt.Sprintf("Outlined: unconfirmed. %s reported this; it fills in once %d machines agree or a Verified run confirms.", who, reports.Agreement)
}

// resultWords is a feature result in a few words: the machines' states, and
// whether Verified runs decide them or they're unconfirmed.
func resultWords(cell *reports.Cell) string {
	switch {
	case cell.Hidden():
		return "unconfirmed, hidden"
	case len(cell.Tallies) == 0:
		return "not tested"
	case cell.Verified():
		return cell.Counts() + " · verified"
	case cell.Unconfirmed():
		return cell.Counts() + " · unconfirmed"
	}
	return cell.Counts()
}

func tip(cell *reports.Cell, heading string, lines ...string) tipView {
	state := cell.DisplayState()
	return tipView{State: state, Swatch: strings.TrimPrefix(cell.CSSClass(), "cell "), Glyph: reports.StateGlyph(state),
		Meaning: upcaseFirst(reports.StateMeaning(state)), Confidence: confidence(cell), Heading: heading, Lines: lines}
}

// cell is one matrix cell: a coloured square once machines agree or a
// Verified run confirms, an outlined hint while unconfirmed. source is the
// run it links, nil for none; provenance adds the kernels and the date its
// result is from; context are the lines that place it (feature, then model
// and stack).
func (s *Server) cell(cell *reports.Cell, feature string, source *reports.Run, provenance bool, context ...string) cellView {
	v := cellView{Class: cell.CSSClass(), Title: cell.Summary(), Feature: feature, State: cell.State(), Glyph: reports.StateGlyph(cell.DisplayState())}
	switch {
	case cell.Hidden():
		v.State = "hidden"
	case v.State == "":
		v.State = "unconfirmed"
	}
	if cell.Verified() {
		v.Verified = "tester"
	}
	lines := append([]string(nil), context[min(1, len(context)):]...)
	if sources := cell.Sources(); provenance && len(sources) > 0 {
		kernels := []string{}
		seen := map[string]bool{}
		for _, run := range sources {
			if !seen[run.Kernel()] {
				seen[run.Kernel()] = true
				kernels = append(kernels, run.Kernel())
			}
		}
		list, _ := json.Marshal(kernels)
		v.Kernels, v.TestedAt = string(list), sources[0].CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
		v.Title += " · kernels: " + strings.Join(kernels, ", ") + " · latest tested upload: " + uploadedAtRun(sources[0])
		lines = append(lines, "Last tested "+uploadedAtRun(sources[0])+" · kernel "+strings.Join(kernels, ", "))
	}
	if source != nil && len(cell.Runs) > 0 {
		v.Link = s.href(reportPath(source))
	}
	heading := ""
	if len(context) > 0 {
		heading = context[0]
	}
	v.Tip = tip(cell, heading, lines...)
	return v
}

func firstRun(runs []*reports.Run) *reports.Run {
	if len(runs) == 0 {
		return nil
	}
	return runs[0]
}

type featureHead struct{ ID, Name, Layer, LayerWords, Href string }

// matrixRowView is one row of a matrix table: a model's summary, one of the
// builds behind it, or a plain row.
type matrixRowView struct {
	Summary, History               bool
	Board, Stack, Version, BuildID string
	Href, Name, Meta, Build        string
	ToggleID                       string
	Builds                         int
	Cells                          []cellView
}

type matrixGroup struct {
	Row       matrixRowView
	HistoryID string
	Builds    []matrixRowView
}

// matrixView is the matrix as a table; Grouped when each row is a summary
// with its builds under it.
type matrixView struct {
	Grouped  bool
	Features []featureHead
	Groups   []matrixGroup
}

func (s *Server) matrixRow(row *reports.Row, features []*reports.Object, summary, history bool) matrixRowView {
	c := row.Configuration
	v := matrixRowView{Summary: summary, History: history, Board: c.Board, Stack: c.Stack, Version: c.Version, BuildID: c.BuildID}
	if history {
		query := url.Values{"model": {c.Board}, "stack": {c.Stack}}
		if c.Version != "" {
			query.Set("release", c.Version)
		}
		if c.Build != "" {
			query.Set("build", c.Build)
		}
		v.Href, v.Name, v.Meta = s.href("/reports")+reports.Query(query), c.ReleaseLabel(), reports.Plural(len(row.Runs), "report")
	} else {
		v.Href, v.Name, v.Meta = s.href("/models/"+c.Board), row.ModelName, c.ReleaseLabel()+" · "+reports.Plural(len(row.Runs), "report")
	}
	if c.Build != "" {
		v.Build = "build " + c.Build
	}
	for _, feature := range features {
		id, name := words(member(feature, "id")), words(member(feature, "name"))
		cell := row.Cell(id)
		source := firstRun(cell.Runs)
		if summary {
			source = firstRun(cell.Sources())
		}
		v.Cells = append(v.Cells, s.cell(cell, id, source, summary, name, row.ModelName+" · "+c.Label()))
	}
	return v
}

func (s *Server) featureHeads(features []*reports.Object) []featureHead {
	var heads []featureHead
	for _, feature := range features {
		id, layer := words(member(feature, "id")), words(member(feature, "layer"))
		heads = append(heads, featureHead{ID: id, Name: words(member(feature, "name")), Layer: layer, LayerWords: reports.LayerWords[layer], Href: s.href("/features/" + id)})
	}
	return heads
}

// matrixOf is some rows of a matrix as a plain table.
func (s *Server) matrixOf(features []*reports.Object, rows []*reports.Row) matrixView {
	v := matrixView{Features: s.featureHeads(features)}
	for _, row := range rows {
		v.Groups = append(v.Groups, matrixGroup{Row: s.matrixRow(row, features, false, false)})
	}
	return v
}

// matrixTable is the matrix as its table; with a history, each row is a
// model's summary and the history's rows are its builds.
func (s *Server) matrixTable(m *reports.Matrix, history reports.History) matrixView {
	v := matrixView{Grouped: history != nil}
	features := m.Features()
	v.Features = s.featureHeads(features)
	for _, row := range m.Rows() {
		group := matrixGroup{Row: s.matrixRow(row, features, history != nil, false)}
		if history != nil {
			group.HistoryID = "builds-" + row.Configuration.Board + "-" + row.Configuration.Stack
			group.Row.ToggleID, group.Row.Builds = group.HistoryID, len(history.Of(row))
			for _, build := range history.Of(row) {
				group.Builds = append(group.Builds, s.matrixRow(build, features, false, true))
			}
		}
		v.Groups = append(v.Groups, group)
	}
	return v
}

type optionView struct {
	Value, Words string
	Selected     bool
}

type selectView struct {
	Key, Label string
	Options    []optionView
}

type linkView struct{ Name, Href string }

type chipView struct{ Key, Words, Href string }

// filterBarView is the filter bar: a form of the filters a view takes, then
// how many runs match, each picked filter as a chip that removes it, and
// the exports with the same filters.
type filterBarView struct {
	Action      string
	Hidden      []web.Field
	Selects     []selectView
	Dates       []web.Field
	HasCheck    bool
	Confirmed   bool
	Count       int
	CountWords  string
	Verb        string
	Chips       []chipView
	Clear       string
	Exports     []linkView
	NoneMatches bool
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// filterBar is the filter bar of the view at path. keys are the filters the
// view takes, count the runs that match; extra are further parameters the
// view keeps (name, value pairs); exports are downloads, already linked.
func (s *Server) filterBar(filter *reports.Filter, options *reports.Options, path string, keys []string, count int, extra []string, exports ...linkView) filterBarView {
	v := filterBarView{Action: s.href(path), Count: count, CountWords: reports.Plural(count, "run"), Verb: "match", Confirmed: filter.Confirmed(),
		HasCheck: contains(keys, "confirmed"), Exports: exports}
	if count == 1 {
		v.Verb = "matches"
	}
	kept := url.Values{}
	for i := 0; i+1 < len(extra); i += 2 {
		if extra[i+1] != "" {
			v.Hidden = append(v.Hidden, web.Field{Name: extra[i], Value: extra[i+1]})
			kept.Set(extra[i], extra[i+1])
		}
	}
	for _, key := range keys {
		switch key {
		case "from", "to":
			v.Dates = append(v.Dates, web.Field{Name: key, Value: filter.Get(key)})
			continue
		case "confirmed":
			continue
		}
		field := selectView{Key: key, Label: reports.FilterLabels[key]}
		picked, offered := filter.Get(key), false
		for _, choice := range options.For(key) {
			offered = offered || choice.Value == picked
			field.Options = append(field.Options, optionView{choice.Value, choice.Words, picked != "" && choice.Value == picked})
		}
		if picked != "" && !offered {
			field.Options = append([]optionView{{picked, picked, true}}, field.Options...)
		}
		v.Selects = append(v.Selects, field)
	}
	for _, chip := range filter.Chips() {
		v.Chips = append(v.Chips, chipView{chip.Key, chip.Words, s.href(path) + reports.Query(filter.Params(chip.Key, extra...))})
	}
	if filter.Any() {
		v.Clear = s.href(path) + reports.Query(kept)
	}
	return v
}

// runView is one run in a list of reports.
type runView struct {
	PublicID, Uploaded, Href, Model, Label, Kernel string
	BuildID                                        string
	Build                                          *buildView
	Counts                                         reports.Counts
	Verified                                       bool
}

// buildView is a run's build: its label, linked to the reports on it, and
// the candidate set its packages match.
type buildView struct {
	Label, Words, Href  string
	MatchedSet, SetHref string
	// On a report's own page: the page of the set its image names, its
	// runtime build, then its kernel, boot package, build time and tool.
	SetPage, Runtime string
	// Release is the image release the Mac was installed from, "" when unknown.
	Release string
	Extras  []string
}

func (s *Server) buildOf(p *reports.Published, run *reports.Run) *buildView {
	build := run.Build()
	if build == nil {
		return nil
	}
	v := &buildView{Label: build.Label(), Words: build.Words(), Href: s.href("/reports") + reports.Query(url.Values{"build": {build.Words()}}), MatchedSet: build.MatchedSet(), Release: build.Release()}
	if v.MatchedSet != "" && p.NamesSet(v.MatchedSet) {
		v.SetHref = s.href("/candidates/" + v.MatchedSet)
	}
	return v
}

// runList is a list of runs as the reports table shows them; confirmed
// leaves the answers given by a bare Enter out of the counts.
func (s *Server) runList(p *reports.Published, runs []*reports.Run, confirmed bool) []runView {
	list := make([]runView, 0, len(runs))
	for _, run := range runs {
		v := runView{PublicID: run.PublicID, Uploaded: uploadedAtRun(run), Href: s.href(reportPath(run)), Model: run.ShortModel(),
			Label: run.Configuration(reports.ByRelease).Label(), Kernel: run.Kernel(), Build: s.buildOf(p, run), Counts: run.ResultCounts(confirmed), Verified: run.Verified}
		if build := run.Build(); build != nil {
			v.BuildID = build.ID
		}
		list = append(list, v)
	}
	return list
}

// expectedView is what the catalogue expects of a feature in one layer.
type expectedView struct{ Class, Words string }

func (s *Server) expected(states *reports.Object) []expectedView {
	views := make([]expectedView, 0, len(reports.Layers))
	for _, layer := range reports.Layers {
		state, _ := member(states, layer).(*reports.Object)
		status := words(member(state, "status"))
		v := expectedView{Class: "expected expected-no", Words: expectedWords(state)}
		good := status == "supported" || status == "asahi"
		if layer == "asahi" {
			good = contains(reports.AsahiSupported, status)
		}
		switch {
		case state == nil || status == "" || status == "absent":
			v.Class = "expected expected-none"
		case good:
			v.Class = "expected expected-yes"
		}
		views = append(views, v)
	}
	return views
}

// expectedWords is a catalogue state ("upstream 6.2", "linux-asahi", "supported 7.1.12") for one layer.
func expectedWords(state *reports.Object) string {
	if state == nil {
		return "–"
	}
	return strings.TrimSpace(words(member(state, "status")) + " " + words(member(state, "version")))
}

// legendStates are the states the colour code lists.
var legendStates = []string{"works", "partial", "regression", "fails", "missing", "unknown", "not-tested"}

type swatch struct{ State, Glyph, Words string }

// legendView is the colour code under a table of results.
type legendView struct {
	States    []swatch
	Works     string
	Agreement int
}

func legend() legendView {
	v := legendView{Works: reports.StateGlyph("works"), Agreement: reports.Agreement}
	for _, state := range legendStates {
		v.States = append(v.States, swatch{state, reports.StateGlyph(state), reports.StateWords(state)})
	}
	return v
}
