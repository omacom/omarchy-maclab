package server

import (
	"net/http"
	"sort"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("GET /models", web.Anyone, (*Server).pModels)
	addPage("GET /models/{board}", web.Anyone, (*Server).pModel)
}

type badgeView struct{ State, Label string }

// lineupStatusView is one area of a bring-up row: what the runs found, or
// what the catalogue expects.
type lineupStatusView struct {
	Area, State, Source, Title string
	Badge                      badgeView
	Unconfirmed                bool
	Note                       string
	Boards                     []lineupTested
}

type lineupTested struct {
	Board, Name, State, Title string
	Badge                     badgeView
	Unconfirmed               bool
}

type lineupBoardView struct {
	Board, Name, Href, Runs, Last string
	Tested                        bool
	Verified                      int
}

type lineupRowView struct {
	Name, Chip, ChipName, Boards, BoardNames string
	Tested                                   bool
	Statuses                                 []lineupStatusView
	Testing                                  []lineupBoardView
}

type modelRow struct {
	Board, Href, Name, Chip, SoC, Labels string
	Reports                              int
}

type modelsPage struct {
	Tested, Boards int
	TestedWords    string
	Untested       string
	Lineup         []lineupRowView
	Models         []modelRow
	Agreement      int
}

func (s *Server) lineupRow(row *reports.LineupRow) lineupRowView {
	v := lineupRowView{Name: row.Name, Chip: row.Chip, ChipName: s.catalogue.ChipName(row.Chip), Tested: row.Tested()}
	var boards, names []string
	for _, board := range row.Boards {
		boards, names = append(boards, board.Board), append(names, strings.ToUpper(board.Board))
		b := lineupBoardView{Board: board.Board, Name: strings.ToUpper(board.Board), Tested: board.Tested()}
		if board.Tested() {
			b.Href, b.Runs, b.Verified, b.Last = s.href("/models/"+board.Board), reports.Plural(len(board.Runs), "run"), board.VerifiedRuns(), dayOf(board.LastTestedAt())
		}
		v.Testing = append(v.Testing, b)
	}
	v.Boards, v.BoardNames = strings.Join(boards, " "), strings.Join(names, "/")
	for _, area := range reports.LineupAreas {
		status := row.Status(area.Area)
		sv := lineupStatusView{Area: area.Area, State: status.State, Source: status.Source, Badge: badgeView{status.State, status.Words()},
			Unconfirmed: status.Unconfirmed(), Note: status.Note}
		if status.Cell != nil {
			sv.Title = status.Cell.Summary()
		}
		if len(row.Boards) > 1 {
			for _, board := range row.Boards {
				if cell := board.Result(area.Area); len(cell.Tallies) > 0 {
					sv.Boards = append(sv.Boards, lineupTested{Board: board.Board, Name: strings.ToUpper(board.Board), State: cell.DisplayState(), Title: cell.Summary(),
						Badge: badgeView{cell.DisplayState(), reports.LineupWords(cell.DisplayState())}, Unconfirmed: cell.Unconfirmed()})
				}
			}
		}
		v.Statuses = append(v.Statuses, sv)
	}
	return v
}

// pModels is every Apple Silicon Mac: how far Omarchy has brought it up,
// which still need testing, and the Macs with reports.
func (s *Server) pModels(w http.ResponseWriter, r *http.Request) {
	p := s.published(w)
	if p == nil {
		return
	}
	lineup := p.Lineup()
	page := modelsPage{Boards: len(lineup.Boards()), Agreement: reports.Agreement}
	page.Tested = page.Boards - len(lineup.UntestedBoards())
	page.TestedWords = reports.Plural(page.Tested, "board")
	var untested []string
	for _, board := range lineup.UntestedBoards() {
		untested = append(untested, strings.ToUpper(board.Board))
	}
	if page.Untested = strings.Join(untested, ", "); page.Untested == "" {
		page.Untested = "none"
	}
	for _, row := range lineup.Rows() {
		page.Lineup = append(page.Lineup, s.lineupRow(row))
	}
	at := map[string]int{}
	for _, row := range p.Matrix(reports.ByBuild, nil).Rows() {
		board := row.Configuration.Board
		i, seen := at[board]
		if !seen {
			i = len(page.Models)
			at[board] = i
			page.Models = append(page.Models, modelRow{Board: board, Href: s.href("/models/" + board), Name: row.ModelName, Chip: row.Chip, SoC: row.SoC})
		} else {
			page.Models[i].Labels += ", "
		}
		page.Models[i].Labels += row.Configuration.Label()
		page.Models[i].Reports += len(row.Runs)
	}
	s.render(w, r, "models/index", web.Page{Title: "Models", Nav: "models", Data: page})
}

type configurationView struct {
	N     int
	Label string
}

type modelFeature struct {
	ID, Name, Layer, Href string
	Expected              []expectedView
	Cells                 []cellView
}

type modelPage struct {
	Name, Chip, Generation, SoC, Board string
	Configurations                     []configurationView
	Features                           []modelFeature
	Legend                             legendView
	Scores                             []scoreRowView
	Performance                        []perfModelLink
	PerformanceHref                    string
	Reports                            []runView
}

// perfModelLink is one of a model's Configurations, linking to its
// Performance page with its Overall on the default Release.
type perfModelLink struct {
	Cores, Href, Of string
	Overall         perfFigureView
}

// modelPerformance are the board's Configurations on the Performance pages.
func (s *Server) modelPerformance(board string) []perfModelLink {
	set, err := reports.PerfSet("")
	if err != nil {
		return nil
	}
	f, err := s.reports.PerformanceFigures(s.catalogue, set, true)
	if err != nil {
		s.log.Printf("performance figures for /models/%s: %v", board, err)
		return nil
	}
	var out []perfModelLink
	release := f.DefaultRelease()
	for _, c := range f.Configurations {
		if c.Board != board {
			continue
		}
		l := perfModelLink{Cores: perfCores(c.CPU, c.GPU), Href: s.href("/performance/" + c.Path())}
		if release != nil {
			if col := c.Column(release.Key); col != nil && col.Overall != nil {
				l.Overall = perfFigure(col.Overall, col.OverallColour, nil)
				l.Of = "of macOS " + c.Comparison().MacOSVersion + " on " + release.Kernel
			}
		}
		out = append(out, l)
	}
	return out
}

// pModel is one Mac model: what its chip is expected to do, and what each
// build tested on it found.
func (s *Server) pModel(w http.ResponseWriter, r *http.Request) {
	p := s.published(w)
	if p == nil {
		return
	}
	matrix := p.Matrix(reports.ByBuild, nil)
	rows := matrix.RowsForBoard(r.PathValue("board"))
	if len(rows) == 0 {
		s.notFound(w, r)
		return
	}
	var runs []*reports.Run
	for _, row := range rows {
		runs = append(runs, row.Runs...)
	}
	latest := runs[0]
	for _, run := range runs {
		if run.CreatedAt.After(latest.CreatedAt) {
			latest = run
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	chip := s.catalogue.ChipForSoC(latest.SoC())
	page := modelPage{Name: latest.ShortModel(), Chip: latest.Chip(), Generation: s.catalogue.ChipName(chip), SoC: latest.SoC(), Board: latest.Board(),
		Legend: legend(), Scores: s.boardScores(p, r.PathValue("board")), Reports: s.runList(p, runs, false),
		Performance: s.modelPerformance(r.PathValue("board")), PerformanceHref: s.href("/performance")}
	for i, row := range rows {
		page.Configurations = append(page.Configurations, configurationView{i + 1, row.Configuration.Label()})
	}
	for _, feature := range matrix.Features() {
		id, name := words(member(feature, "id")), words(member(feature, "name"))
		f := modelFeature{ID: id, Name: name, Layer: words(member(feature, "layer")), Href: s.href("/features/" + id),
			Expected: s.expected(s.catalogue.Expected(feature, chip, latest.Board()))}
		for _, row := range rows {
			cell := row.Cell(id)
			f.Cells = append(f.Cells, s.cell(cell, id, firstRun(cell.Runs), false, name, row.ModelName+" · "+row.Configuration.Label()))
		}
		page.Features = append(page.Features, f)
	}
	s.render(w, r, "models/show", web.Page{Title: page.Name, Nav: "models", Data: page})
}
