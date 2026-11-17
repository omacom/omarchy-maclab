package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /features", web.Anyone, (*Server).pFeatures)
	addPage("GET /features/{id}", web.Anyone, (*Server).pFeature)
}

// featureResult is one compact result box: a feature on one Mac and build.
type featureResult struct {
	Board, BuildID, Class, Href, Details, Glyph string
	Tip                                         tipView
}

type featureRow struct {
	ID, Name, Href, Checks string
	Results                []featureResult
}

type featureLayer struct {
	Layer, Title string
	Features     []featureRow
}

type featuresPage struct {
	Version     int
	Bar         filterBarView
	NoneMatches bool
	Layers      []featureLayer
	Legend      legendView
}

// featureResults reads the filters the feature pages take and counts the
// builds they pass.
func (s *Server) featureResults(p *reports.Published, r *http.Request, path string) (*reports.Filter, *reports.Matrix, filterBarView) {
	filter := p.Filter(r.URL.Query(), reports.FeatureKeys)
	matrix := p.Matrix(reports.ByBuild, filter)
	return filter, matrix, s.filterBar(filter, p.Options(filter), path, reports.FeatureKeys, matrix.RunsCount(), nil)
}

// pFeatures is the feature catalogue: every feature, in three layers, with
// what each tested Mac and build found.
func (s *Server) pFeatures(w http.ResponseWriter, r *http.Request) {
	p := s.published(w)
	if p == nil {
		return
	}
	filter, matrix, bar := s.featureResults(p, r, "/features")
	page := featuresPage{Version: s.catalogue.Version(), Bar: bar, NoneMatches: filter.Any() && matrix.Empty(), Legend: legend()}
	for _, layer := range reports.Layers {
		l := featureLayer{Layer: layer, Title: strings.ToLower(reports.LayerWords[layer])}
		for _, feature := range s.catalogue.Features() {
			if words(member(feature, "layer")) != layer {
				continue
			}
			id := words(member(feature, "id"))
			row := featureRow{ID: id, Name: words(member(feature, "name")), Href: s.href("/features/"+id) + reports.Query(filter.Params("")), Checks: "–"}
			if n := len(s.catalogue.ChecksFor(id)); n > 0 {
				row.Checks = strconv.Itoa(n)
			}
			for _, tested := range matrix.RowsForFeature(id) {
				cell, c := tested.Cell(id), tested.Configuration
				state := cell.DisplayState()
				row.Results = append(row.Results, featureResult{Board: c.Board, BuildID: c.BuildID, Class: cell.CSSClass(), Href: s.href("/models/" + c.Board + "#feature-" + id),
					Details: tested.ModelName + ", " + c.Label() + ": " + reports.StateMeaning(state) + ". " + confidence(cell), Glyph: reports.StateGlyph(state),
					Tip: tip(cell, tested.ModelName, c.Label())})
			}
			l.Features = append(l.Features, row)
		}
		page.Layers = append(page.Layers, l)
	}
	s.render(w, r, "features/index", web.Page{Title: "Features", Nav: "features", Data: page})
}

type featureBuild struct {
	Board, Stack, BuildID, Version, Build string
	Cell                                  cellView
	Words, Day, Href                      string
}

type featureSummary struct {
	Board, Stack, StackWords, Name, Href string
	HistoryID                            string
	Cell                                 cellView
	Words, Day, DayHref, DayTitle        string
	Builds                               []featureBuild
}

type chipExpected struct {
	Key, Name string
	Expected  []expectedView
}

type featurePage struct {
	ID, Name, Layer, LayerWords  string
	BuildsOn, BuildsOnHref, Note string
	Bar                          filterBarView
	Any                          bool
	Results                      []featureSummary
	Legend                       legendView
	Checks                       []string
	Chips                        []chipExpected
	PerBoard                     string
}

// pFeature is one feature: one summary row per model and stack, each with
// its builds' rows as history, then what each chip is expected to do.
func (s *Server) pFeature(w http.ResponseWriter, r *http.Request) {
	feature := s.catalogue.Feature(r.PathValue("id"))
	if feature == nil {
		s.notFound(w, r)
		return
	}
	p := s.published(w)
	if p == nil {
		return
	}
	id, name := words(member(feature, "id")), words(member(feature, "name"))
	filter, matrix, bar := s.featureResults(p, r, "/features/"+id)
	layer := words(member(feature, "layer"))
	page := featurePage{ID: id, Name: name, Layer: layer, LayerWords: reports.LayerWords[layer], Note: words(member(feature, "note")), Bar: bar, Any: filter.Any(),
		Legend: legend(), Checks: s.catalogue.ChecksFor(id)}
	if on := words(member(feature, "asahi_feature")); on != "" {
		page.BuildsOn, page.BuildsOnHref = s.catalogue.FeatureName(on), s.href("/features/"+on)
	}
	history := map[[2]string][]*reports.Row{}
	for _, row := range matrix.RowsForFeature(id) {
		key := [2]string{row.Configuration.Board, row.Configuration.Stack}
		history[key] = append(history[key], row)
	}
	for _, row := range p.Matrix(reports.ByModel, filter).RowsForFeature(id) {
		cell, c := row.Cell(id), row.Configuration
		latest := firstRun(cell.Sources())
		summary := featureSummary{Board: c.Board, Stack: c.Stack, StackWords: c.StackWords(), Name: row.ModelName, Href: s.href("/models/" + c.Board),
			HistoryID: "builds-" + c.Board + "-" + c.Stack, Cell: s.cell(cell, id, latest, true, name, row.ModelName+" · "+c.ReleaseLabel()), Words: resultWords(cell)}
		if latest != nil {
			summary.Day, summary.DayHref, summary.DayTitle = dayOf(latest.CreatedAt), s.href(reportPath(latest)), "kernel "+latest.Kernel()
		}
		for _, build := range history[[2]string{c.Board, c.Stack}] {
			buildCell, bc := build.Cell(id), build.Configuration
			first := buildCell.Runs[0]
			summary.Builds = append(summary.Builds, featureBuild{Board: bc.Board, Stack: bc.Stack, BuildID: bc.BuildID, Version: bc.Version, Build: bc.Build,
				Cell: s.cell(buildCell, id, first, false, name, build.ModelName+" · "+bc.Label()), Words: resultWords(buildCell), Day: dayOf(first.CreatedAt), Href: s.href(reportPath(first))})
		}
		page.Results = append(page.Results, summary)
	}
	if chips := s.catalogue.Chips(); chips != nil {
		for _, key := range chips.Keys() {
			page.Chips = append(page.Chips, chipExpected{Key: key, Name: words(member(chips, key, "name")), Expected: s.expected(s.catalogue.Expected(feature, key, ""))})
		}
	}
	if models, _ := member(feature, "models").(*reports.Object); models != nil {
		var boards []string
		for _, board := range models.Keys() {
			var layers []string
			states, _ := member(models, board).(*reports.Object)
			if states != nil {
				for _, l := range states.Keys() {
					state, _ := member(states, l).(*reports.Object)
					layers = append(layers, l+" "+expectedWords(state))
				}
			}
			boards = append(boards, board+" "+strings.Join(layers, ", "))
		}
		page.PerBoard = strings.Join(boards, "; ")
	}
	s.render(w, r, "features/show", web.Page{Title: name, Nav: "features", Data: page})
}
