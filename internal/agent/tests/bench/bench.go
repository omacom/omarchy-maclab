// Package bench holds the lab's benchmark runner and the files it runs,
// pinned by sha256: the newest Benchmark set's Omarchy files (package sets)
// and the runner's legacy items. lab-agent runs the runner; `lab bench
// mirror` puts every pinned file in labd's artifact store, from where
// lab-agent fetches it.
package bench

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

//go:embed bench.py
var Runner []byte

// Item is one pinned file under the runner's name for it: a benchmark web
// app, a program, a source tree or a clip.
type Item struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Root    string `json:"root,omitempty"`
	Entry   string `json:"entry,omitempty"`
	Note    string `json:"note,omitempty"`
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Manifest is the items the runner reads: the newest set's pinned Omarchy
// files, by their ids, and the legacy items, which win on a name both hold.
func Manifest() ([]Item, error) {
	s, err := sets.Newest()
	if err != nil {
		return nil, err
	}
	return manifest(s)
}

func manifest(s *sets.Set) ([]Item, error) {
	legacy := map[string]bool{}
	for _, it := range legacyItems {
		legacy[it.Name] = true
	}
	var out []Item
	for _, f := range s.Files[sets.Omarchy] {
		if f.Pinned() && !legacy[f.ID] {
			out = append(out, Item{Name: f.ID, Version: f.Version, Kind: f.Kind, URL: f.URL, SHA256: f.SHA256, Size: f.Size,
				Root: f.Root, Entry: f.Entry, Note: f.Note})
		}
	}
	out = append(out, Legacy()...)
	for _, it := range out {
		if !reSHA.MatchString(it.SHA256) || it.URL == "" || it.Version == "" {
			return nil, fmt.Errorf("benchmark item %s is not pinned", it.Name)
		}
	}
	return out, nil
}

// Legacy is the runner's legacy items, which `lab bench mirror` mirrors
// beside the sets' files.
func Legacy() []Item { return append([]Item(nil), legacyItems...) }

// JSON is the manifest the runner reads, for these items.
func JSON(items []Item) ([]byte, error) {
	return json.MarshalIndent(struct {
		Schema int    `json:"schema"`
		Items  []Item `json:"items"`
	}{1, items}, "", "  ")
}
