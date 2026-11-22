// Package bench holds the lab's benchmark runner, bench.py, which runs a
// Benchmark set's Quick or Full list on Omarchy by metric id. lab-agent runs
// it with the list's pinned Omarchy files, fetched from labd by sha256; `lab
// bench mirror` puts every set's pinned files in labd's artifact store.
package bench

import (
	_ "embed"
	"errors"
	"fmt"
	"slices"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

//go:embed bench.py
var Runner []byte

// Builtins are the bench builtins and the list of the set each runs.
var Builtins = map[string]string{"bench": sets.Quick, "bench-extended": sets.Full}

// Set is the Benchmark set the builtins run: the current set, or the newest
// while no set is current, whose metrics with a placeholder file read
// placeholder_file.
func Set() (*sets.Set, error) {
	s, err := sets.Current()
	if errors.Is(err, sets.ErrNoSet) {
		return sets.Newest()
	}
	return s, err
}

// Files is the pinned Omarchy files the list needs, each once: what
// lab-agent fetches from labd before it runs the list.
func Files(s *sets.Set, list string) []sets.File {
	var out []sets.File
	for _, f := range s.FilesFor(sets.Omarchy, s.List(list)) {
		if f.Pinned() {
			out = append(out, f)
		}
	}
	return out
}

// FilesOf is the pinned Omarchy files a Performance run's metrics need, each
// once: what lab-agent fetches before it runs them. Each id must be a metric
// of the list that runs on Omarchy.
func FilesOf(s *sets.Set, list string, ids []string) ([]sets.File, error) {
	if list != sets.Quick && list != sets.Full {
		return nil, fmt.Errorf("list %q: want quick or full", list)
	}
	if len(ids) == 0 {
		return nil, errors.New("a Performance run with no metric")
	}
	in := s.List(list)
	var ms []sets.Metric
	for _, id := range ids {
		i := slices.IndexFunc(in, func(m sets.Metric) bool { return m.ID == id })
		if i < 0 || in[i].Omarchy == nil {
			return nil, fmt.Errorf("metric %q is not in %s's %s list on Omarchy", id, s.Name, list)
		}
		ms = append(ms, in[i])
	}
	var out []sets.File
	for _, f := range s.FilesFor(sets.Omarchy, ms) {
		if f.Pinned() {
			out = append(out, f)
		}
	}
	return out, nil
}

// Pinned is the sha256 of every set's pinned file on either system, each
// once: labd's artifact store never collects them.
func Pinned() ([]string, error) {
	all, err := sets.All()
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, s := range all {
		for _, sys := range sets.Systems {
			for _, f := range s.Files[sys] {
				if f.Pinned() && !seen[f.SHA256] {
					seen[f.SHA256] = true
					out = append(out, f.SHA256)
				}
			}
		}
	}
	return out, nil
}
