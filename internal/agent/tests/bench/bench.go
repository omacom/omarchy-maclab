// Package bench holds the lab's benchmark runner, bench.py, which runs a
// Benchmark set's Quick or Full list on Omarchy by metric id. lab-agent runs
// it with the list's pinned Omarchy files, fetched from labd by sha256; `lab
// bench mirror` puts every set's pinned files in labd's artifact store.
package bench

import (
	_ "embed"
	"errors"

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
