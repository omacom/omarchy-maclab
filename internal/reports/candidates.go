package reports

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// CandidateSet is the runs on one candidate set's images (the report's
// system.candidate_set, from the image's target record), and how the set
// stands: Verified runs only, per Mac model and feature. Community runs on
// the set are listed but don't count.
//
//	waiting  no Verified run yet
//	blocked  Verified runs found a feature failing where it should work (a
//	         regression, or a failure with no Verified earlier pass)
//	ready    Verified runs on at least one model, and none found a failure
//
// The three words are the old site's and stay as the pages' hooks; a page
// says Waiting, Failing or Passing.
type CandidateSet struct {
	Name string
	// Runs are the published runs whose image names the set, oldest first.
	Runs []*Run

	pub    *Published
	matrix *Matrix
}

// CandidateSetName is the shape of a set's name.
var CandidateSetName = regexp.MustCompile(`\A[A-Za-z0-9._-]{1,128}\z`)

// Finding is one feature on one row of a set's matrix.
type Finding struct {
	Row     *Row
	Feature *Object
	Cell    *Cell
}

// CandidateSets are the sets published runs name, the one with the latest run first.
func (p *Published) CandidateSets() []*CandidateSet {
	byName := map[string]*CandidateSet{}
	var sets []*CandidateSet
	for _, run := range p.runs {
		name := run.CandidateSet()
		if name == "" {
			continue
		}
		set := byName[name]
		if set == nil {
			set = &CandidateSet{Name: name, pub: p}
			byName[name] = set
			sets = append(sets, set)
		}
		set.Runs = append(set.Runs, run)
	}
	sort.SliceStable(sets, func(i, j int) bool {
		if a, b := sets[i].Latest().CreatedAt.Unix(), sets[j].Latest().CreatedAt.Unix(); a != b {
			return a > b
		}
		return sets[i].Name < sets[j].Name
	})
	return sets
}

// CandidateSet is the set with that name, or nil when no published run names it.
func (p *Published) CandidateSet(name string) *CandidateSet {
	set := &CandidateSet{Name: name, pub: p}
	for _, run := range p.runs {
		if name != "" && run.CandidateSet() == name {
			set.Runs = append(set.Runs, run)
		}
	}
	if len(set.Runs) == 0 {
		return nil
	}
	return set
}

// MatchedBy are the published runs whose image names no set but whose
// omarchy package is this set's: listed, never counted.
func (s *CandidateSet) MatchedBy() []*Run {
	var matched []*Run
	for _, run := range s.pub.runs {
		if build := run.Build(); build != nil && build.MatchedSet() == s.Name {
			matched = append(matched, run)
		}
	}
	return matched
}

func (s *CandidateSet) VerifiedRuns() []*Run {
	var verified []*Run
	for _, run := range s.Runs {
		if run.Verified {
			verified = append(verified, run)
		}
	}
	return verified
}

// Carries reports whether a Release with these packages (name to version)
// is this set's: a Verified run on the set names every package of the
// Release at the Release's version, and the Release every package the run
// names. A run naming one package at two versions links nothing. A package alone, the omarchy package the lab knows the set by
// included, links nothing, even when the run names only it. Community runs
// link nothing.
func (s *CandidateSet) Carries(packages map[string]string) bool {
	if len(packages) < 2 {
		return false
	}
	for _, run := range s.VerifiedRuns() {
		named := map[string]string{}
		on := true
		for _, pkg := range run.packages() {
			name, version := text(member(asObject(pkg), "name")), text(member(asObject(pkg), "version"))
			if was, seen := named[name]; seen && was != version {
				on = false
			}
			named[name] = version
		}
		on = on && len(named) == len(packages)
		for name, version := range packages {
			on = on && version != "" && named[name] == version
		}
		if on {
			return true
		}
	}
	return false
}

// SetsCarrying are the names of the sets a Release with these packages is
// linked to, in the order of CandidateSets.
func (p *Published) SetsCarrying(packages map[string]string) []string {
	var names []string
	for _, set := range p.CandidateSets() {
		if set.Carries(packages) {
			names = append(names, set.Name)
		}
	}
	return names
}

// Latest is the set's latest run.
func (s *CandidateSet) Latest() *Run {
	latest := s.Runs[0]
	for _, run := range s.Runs {
		if run.CreatedAt.After(latest.CreatedAt) {
			latest = run
		}
	}
	return latest
}

// Matrix is the set's runs as a matrix that counts Verified runs only.
func (s *CandidateSet) Matrix() *Matrix {
	if s.matrix == nil {
		s.matrix = s.pub.MatrixOf(s.Runs, ByBuild, nil, true)
	}
	return s.matrix
}

// TestedRows are the rows Verified runs tested.
func (s *CandidateSet) TestedRows() []*Row {
	var rows []*Row
	for _, row := range s.Matrix().Rows() {
		for _, feature := range s.pub.cat.TestedFeatures() {
			if row.Cell(id(feature)).Verified() {
				rows = append(rows, row)
				break
			}
		}
	}
	return rows
}

func (s *CandidateSet) findings(state string) []Finding {
	var found []Finding
	for _, row := range s.Matrix().Rows() {
		for _, feature := range s.Matrix().Features() {
			if cell := row.Cell(id(feature)); cell.Verified() && cell.State() == state {
				found = append(found, Finding{row, feature, cell})
			}
		}
	}
	return found
}

func (s *CandidateSet) Regressions() []Finding { return s.findings("regression") }
func (s *CandidateSet) Failures() []Finding    { return append(s.Regressions(), s.findings("fails")...) }
func (s *CandidateSet) Partials() []Finding    { return s.findings("partial") }

// Verdict is "waiting", "blocked" or "ready". Verified runs that counted
// nothing (every answer a bare Enter) leave it waiting.
func (s *CandidateSet) Verdict() string {
	switch {
	case len(s.VerifiedRuns()) == 0 || len(s.TestedRows()) == 0:
		return "waiting"
	case len(s.Failures()) > 0:
		return "blocked"
	}
	return "ready"
}

// VerdictWords is the Candidates page's own line for the set.
func (s *CandidateSet) VerdictWords() string {
	switch s.Verdict() {
	case "waiting":
		return "Waiting"
	case "blocked":
		return fmt.Sprintf("Failing: %s, %s", Plural(len(s.Failures()), "failure"), Plural(len(s.Regressions()), "regression"))
	}
	models := map[string]bool{}
	for _, row := range s.TestedRows() {
		models[row.ModelName] = true
	}
	return fmt.Sprintf("Passing: Verified runs on %s, nothing failing", Plural(len(models), "model"))
}

// The old site asked Postgres four things of a report's JSON. They run on
// SQLite as they were written, for what must not read every report: one
// report's page, and a candidate set nothing names.
const (
	whereNamesASet       = `(body -> 'system' ->> 'candidate_set') IS NOT NULL`
	selectSetName        = `body -> 'system' ->> 'candidate_set'`
	whereOnSet           = `body -> 'system' ->> 'candidate_set' = ?`
	whereOnBoardAndStack = `body -> 'machine' ->> 'board' = ? AND body -> 'system' ->> 'stack' = ?`
)

func (s *Store) texts(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		list = append(list, text)
	}
	return list, rows.Err()
}

// CandidateSetNames are the candidate sets published reports name.
func (s *Store) CandidateSetNames() ([]string, error) {
	return s.texts(`SELECT DISTINCT ` + selectSetName + ` FROM reports WHERE hidden_at IS NULL AND ` + whereNamesASet)
}

// OnCandidateSet are the published reports whose image names that set, as
// public ids, oldest first.
func (s *Store) OnCandidateSet(name string) ([]string, error) {
	return s.texts(`SELECT public_id FROM reports WHERE hidden_at IS NULL AND `+whereOnSet+` ORDER BY created_at, id`, name)
}

// VerifiedOn are the published Verified runs on one board and stack, oldest
// first: where a regression's earlier pass comes from.
func (s *Store) VerifiedOn(board, stack string) ([]*Report, error) {
	return s.publishedWhere(`verified AND `+whereOnBoardAndStack, board, stack)
}

// Around reads what one report's page needs and no more: the published
// report with that public id, the Verified runs on its model and stack, and
// the names of the candidate sets published runs name. The run is nil when
// there is no such report, or it is Hidden.
func (s *Store) Around(cat *Catalogue, publicID string) (*Published, *Run, error) {
	report, err := s.Report(publicID)
	if errors.Is(err, ErrNoReport) || (err == nil && report.Hidden) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	p, err := s.around(cat, report)
	if err != nil {
		return nil, nil, err
	}
	return p, p.Run(publicID), nil
}

// around counts what a report's page says of it: the report, unless it is
// Hidden, the Verified runs on its model and stack, and the set names.
func (s *Store) around(cat *Catalogue, report *Report) (*Published, error) {
	doc := report.Document()
	list, err := s.VerifiedOn(text(member(doc, "machine", "board")), text(member(doc, "system", "stack")))
	if err != nil {
		return nil, err
	}
	among := false
	for _, other := range list {
		among = among || other.ID == report.ID
	}
	if !among {
		list = append(list, report)
	}
	names, err := s.CandidateSetNames()
	if err != nil {
		return nil, err
	}
	p := NewPublished(cat, list)
	p.sets = map[string]bool{}
	for _, name := range names {
		p.sets[name] = true
	}
	return p, nil
}
