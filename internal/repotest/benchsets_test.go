package repotest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

// frozenSets is the sha256 of every committed Benchmark set. A set is frozen:
// a new tool version, setting, tolerance or file is a new set, bench-N+1,
// added here. Only a set that still holds placeholders may be changed, to
// fill them, with its line here changed in the same pull request.
var frozenSets = map[string]string{
	"bench-1.json": "2f5f1bbc815f7e1a5410dee6b16b44469008b2fc1a5c30dd3e0245b70e7334ca",
	"bench-2.json": "82cc7b3582d6ac6339193b755c710890f2c1f908e156557aca6991415c9b7fd3",
}

func TestEveryBenchmarkSetLoadsAndIsFrozen(t *testing.T) {
	dir := filepath.Join(root(t), "internal", "agent", "tests", "bench", "sets")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		found[e.Name()] = true
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sets.Parse(e.Name()[:len(e.Name())-len(".json")], b); err != nil {
			t.Errorf("%v", err)
		}
		sum := sha256.Sum256(b)
		want, ok := frozenSets[e.Name()]
		switch got := hex.EncodeToString(sum[:]); {
		case !ok:
			t.Errorf("%s is not in frozenSets (its sha256 is %s)", e.Name(), got)
		case got != want:
			t.Errorf("%s changed (sha256 %s): a set is frozen, so a change is a new set", e.Name(), got)
		}
	}
	for name := range frozenSets {
		if !found[name] {
			t.Errorf("%s is gone: a set is never deleted", name)
		}
	}
	for n := 1; n <= len(found); n++ {
		if !found[fmt.Sprintf("bench-%d.json", n)] {
			t.Errorf("no bench-%d.json: sets are numbered from 1 without a gap", n)
		}
	}
	all, err := sets.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(found) {
		t.Errorf("the package holds %d sets, sets/ %d", len(all), len(found))
	}
}
