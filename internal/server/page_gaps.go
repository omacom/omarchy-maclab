package server

import (
	"net/http"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /gaps", web.Anyone, (*Server).pGaps)
	addPage("GET /aurora", web.Anyone, (*Server).pAurora)
}

type runLink struct{ Name, Href, Title string }

// runLinksView links the first five of some runs by the start of their ids,
// and counts the rest.
type runLinksView struct {
	Links []runLink
	More  int
}

func (s *Server) runLinks(runs []*reports.Run, title func(*reports.Run) string) runLinksView {
	var v runLinksView
	for i, run := range runs {
		if i == 5 {
			v.More = len(runs) - 5
			break
		}
		link := runLink{Name: run.PublicID[:min(8, len(run.PublicID))], Href: s.href(reportPath(run))}
		if title != nil {
			link.Title = title(run)
		}
		v.Links = append(v.Links, link)
	}
	return v
}

type regressionRow struct {
	Feature, FeatureHref, FeatureName, Layer string
	Board, BoardHref, Model, Stack, On       string
	PassHref, PassWords                      string
	Runs                                     runLinksView
	Issue                                    *issueView
}

type gapRow struct {
	Outcome, Feature, FeatureHref, FeatureName string
	Chip, SoC, Models                          string
	Runs                                       runLinksView
	Issue                                      *issueView
}

type reportedGroup struct {
	Outcome, Title string
	Gaps           []gapRow
}

type unclaimedRow struct {
	Compatible, Outcome            string
	FeatureHref, FeatureName, What string
	Chips, Models                  string
	Machines                       int
	Runs                           runLinksView
	Issue                          *issueView
}

type catalogueGapRow struct {
	Feature, Href, Name, Layer, Chips string
	Issue                             *issueView
}

type catalogueGapsView struct {
	Issues bool
	Gaps   []catalogueGapRow
}

type gapsPage struct {
	Issues              bool
	Regressions         []regressionRow
	Reported            []reportedGroup
	Unclaimed           []unclaimedRow
	Missing, Unverified catalogueGapsView
}

var reportedTitles = map[string]string{"not-in-aurora": "Not yet in Aurora", "not-in-asahi": "Not yet in Asahi", "unknown-hardware": "Unknown hardware"}

func (s *Server) catalogueGaps(r *http.Request, issues *reports.Issues, gaps []reports.CatalogueGap, missing bool) catalogueGapsView {
	var rows []catalogueGapRow
	for _, gap := range gaps {
		id := words(member(gap.Feature, "id"))
		var chips []string
		for _, chip := range gap.Chips {
			chips = append(chips, s.catalogue.ChipName(chip))
		}
		row := catalogueGapRow{Feature: id, Href: s.href("/features/" + id), Name: words(member(gap.Feature, "name")), Layer: words(member(gap.Feature, "layer")), Chips: strings.Join(chips, ", ")}
		if issues != nil {
			row.Issue = issueOf(issues.ForCatalogueGap(gap, missing, s.publicBase(r)+"/features/"+id))
		}
		rows = append(rows, row)
	}
	return catalogueGapsView{Issues: issues != nil, Gaps: rows}
}

// pGaps is what to work on next in the kernel: open regressions, what real
// Macs found missing or unclaimed, and what Asahi supports that Aurora lacks
// or hasn't verified.
func (s *Server) pGaps(w http.ResponseWriter, r *http.Request) {
	p := s.published(w)
	if p == nil {
		return
	}
	cat, gaps, issues := s.catalogue, p.Gaps(), s.issues(r, p)
	page := gapsPage{Issues: issues != nil}
	for _, open := range p.OpenRegressions(p.Runs()) {
		latest, pass := open.Latest(), open.Pass()
		row := regressionRow{Feature: open.Feature, FeatureHref: s.href("/features/" + open.Feature), FeatureName: cat.FeatureName(open.Feature),
			Layer: words(member(cat.Feature(open.Feature), "layer")), Board: open.Board, BoardHref: s.href("/models/" + open.Board), Model: latest.ShortModel(), Stack: open.Stack,
			On: latest.StackWords() + " " + latest.OmarchyVersion(), PassHref: s.href(reportPath(pass)), PassWords: pass.StackWords() + " " + pass.OmarchyVersion(),
			Runs: s.runLinks(open.Runs, nil)}
		if issues != nil {
			row.Issue = issueOf(issues.ForRegression(open))
		}
		page.Regressions = append(page.Regressions, row)
	}
	for _, outcome := range reports.ReportedOutcomes {
		group := reportedGroup{Outcome: outcome, Title: reportedTitles[outcome]}
		for _, gap := range gaps.Reported(outcome) {
			row := gapRow{Outcome: outcome, Feature: gap.Feature, FeatureHref: s.href("/features/" + gap.Feature), FeatureName: cat.FeatureName(gap.Feature),
				Chip: gap.Chip, SoC: gap.SoC, Models: strings.Join(gap.Models(), ", "), Runs: s.runLinks(gap.Runs, nil)}
			if issues != nil {
				row.Issue = issueOf(issues.ForReportedGap(gap))
			}
			group.Gaps = append(group.Gaps, row)
		}
		page.Reported = append(page.Reported, group)
	}
	for _, hardware := range gaps.UnclaimedHardware() {
		var chips []string
		for _, chip := range hardware.Chips() {
			chips = append(chips, chip[0]+" ("+chip[1]+")")
		}
		row := unclaimedRow{Compatible: hardware.Compatible, Outcome: hardware.Outcome, What: cat.OutcomeWords(hardware.Outcome), Chips: strings.Join(chips, ", "),
			Models: strings.Join(hardware.Models(), ", "), Machines: hardware.Machines(), Runs: s.runLinks(hardware.Runs, nil)}
		if hardware.Feature != "" {
			row.FeatureHref, row.FeatureName = s.href("/features/"+hardware.Feature), cat.FeatureName(hardware.Feature)
		}
		if issues != nil {
			row.Issue = issueOf(issues.ForUnclaimed(hardware))
		}
		page.Unclaimed = append(page.Unclaimed, row)
	}
	page.Missing = s.catalogueGaps(r, issues, gaps.MissingInAurora(), true)
	page.Unverified = s.catalogueGaps(r, issues, gaps.UnverifiedInAurora(), false)
	s.render(w, r, "gaps", web.Page{Title: "Kernel gaps", Nav: "gaps", Data: page})
}

type auroraRowView struct {
	Chip, Feature, State                   string
	FeatureHref, FeatureName, Layer, Title string
	Badge                                  badgeView
	Tested                                 bool
	Machines                               string
	Aurora, Asahi                          expectedView
	Runs                                   runLinksView
	Versions                               string
}

type auroraChipView struct {
	Key, Name, Line string
	Rows            []auroraRowView
}

type auroraPage struct {
	Empty                        bool
	Chips                        []auroraChipView
	Untested                     string
	Package                      string
	SourceURL, SourceTitle, Note string
	Version                      int
}

// pAurora is the Aurora feature-support table per chip generation, from
// Verified runs on the Aurora kernel, next to what the catalogue expects.
func (s *Server) pAurora(w http.ResponseWriter, r *http.Request) {
	p := s.published(w)
	if p == nil {
		return
	}
	cat, support := s.catalogue, p.Aurora()
	page := auroraPage{Empty: support.Empty(), Package: reports.AuroraPackage, SourceURL: cat.Source("aurora", "url"), SourceTitle: cat.Source("aurora", "title"),
		Note: cat.Source("aurora", "note"), Version: cat.Version()}
	for _, chip := range support.Chips() {
		v := auroraChipView{Key: chip.Key, Name: cat.ChipName(chip.Key),
			Line: reports.Plural(chip.Machines(), "Verified machine") + ", " + reports.Plural(len(chip.Runs), "run") + ": " + strings.Join(chip.Models(), ", ") +
				". Aurora " + strings.Join(chip.AuroraVersions(), ", ") + "."}
		for _, feature := range support.Features() {
			id := words(member(feature, "id"))
			cell := chip.Cell(id)
			expected := s.expected(cell.Expected)
			row := auroraRowView{Chip: chip.Key, Feature: id, State: cell.State(), FeatureHref: s.href("/features/" + id), FeatureName: words(member(feature, "name")),
				Layer: words(member(feature, "layer")), Title: cell.Summary(), Badge: badgeView{cell.State(), reports.StateWords(cell.State())}, Tested: cell.Tested(),
				Asahi: expected[0], Aurora: expected[1], Versions: strings.Join(cell.AuroraVersions(), ", ")}
			if cell.Tested() {
				row.Machines = reports.Plural(cell.Machines(), "machine")
				row.Runs = s.runLinks(cell.Runs(), func(run *reports.Run) string {
					return run.ShortModel() + ", linux-aurora " + run.AuroraVersion() + ", " + uploadedAtRun(run)
				})
			}
			v.Rows = append(v.Rows, row)
		}
		page.Chips = append(page.Chips, v)
	}
	var untested []string
	for _, key := range support.UntestedChips() {
		untested = append(untested, cat.ChipName(key))
	}
	page.Untested = strings.Join(untested, ", ")
	s.render(w, r, "aurora", web.Page{Title: "Aurora feature support", Nav: "aurora", Data: page})
}
