package server

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// Every Benchmark set's pinned files stay in the artifact store however old,
// and over budget: lab-agent fetches them only from labd.
func TestMirroredBenchmarkFilesAreNeverCollected(t *testing.T) {
	l := newLab(t, false)
	old := time.Now().Add(-48 * time.Hour)
	shas, err := bench.Pinned()
	if err != nil || len(shas) == 0 {
		t.Fatal(shas, err)
	}
	for _, sha := range shas {
		if err := os.WriteFile(l.s.artifactPath(sha), []byte("benchmark fixture "+sha), 0600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(l.s.artifactPath(sha), old, old)
	}
	l.s.cfg.ArtifactBudget = 1
	l.s.gcArtifacts()
	for _, sha := range shas {
		if _, err := os.Stat(l.s.artifactPath(sha)); err != nil {
			t.Errorf("%s collected: %v", sha, err)
		}
	}
}

// A benchmark builtin's grants are its list's pinned Omarchy files.
func TestABenchmarkBuiltinIsGrantedItsListsFiles(t *testing.T) {
	s, err := bench.Set()
	if err != nil {
		t.Fatal(err)
	}
	for builtin, list := range bench.Builtins {
		got := testArtifacts(api.TestSpec{Name: "b", Builtin: builtin})
		for _, f := range bench.Files(s, list) {
			if !slices.Contains(got, f.SHA256) {
				t.Errorf("%s: no grant for %s", builtin, f.ID)
			}
		}
		if mf, ok := s.File(sets.MacOS, "chrome"); ok && mf.Pinned() && slices.Contains(got, mf.SHA256) {
			t.Errorf("%s: granted the macOS Chrome", builtin)
		}
	}
}
