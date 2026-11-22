package bench

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/manifest.json")

func TestTheManifestPinsEveryItem(t *testing.T) {
	items, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"speedometer", "motionmark", "jetstream", "chromium", "chrome", "7zip", "glmark2", "vkmark",
		"assimp", "clpeak", "vkpeak", "geekbench", "kernel", "clip"}
	have := map[string]Item{}
	for _, it := range items {
		if _, dup := have[it.Name]; dup {
			t.Errorf("%s given twice", it.Name)
		}
		have[it.Name] = it
	}
	for _, n := range want {
		if _, ok := have[n]; !ok {
			t.Errorf("no %s in the manifest", n)
		}
	}
	if have["speedometer"].Version != "3.1" || have["motionmark"].Version != "1.3" || have["jetstream"].Version != "2.2" {
		t.Errorf("browser benchmark versions: %+v", have)
	}
	if have["coremark"].Kind != "source" || have["clpeak"].Kind != "pacman" {
		t.Errorf("the set's placeholders took the legacy items' places: %+v %+v", have["coremark"], have["clpeak"])
	}
}

func TestEveryItemTheRunnerNeedsIsPinned(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	out, err := exec.Command(py, "-I", "bench.py", "--tier", "extended", "--needs").Output()
	if err != nil {
		t.Fatal(err)
	}
	items, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, it := range items {
		have[it.Name] = true
	}
	for _, n := range strings.Fields(string(out)) {
		if !have[n] {
			t.Errorf("the runner needs %s, which nothing pins", n)
		}
	}
}

// The runner's own tests read testdata/manifest.json, a copy of Manifest.
func TestTheRunnersTestsReadTheManifest(t *testing.T) {
	items, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	b, err := JSON(items)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	p := filepath.Join("testdata", "manifest.json")
	if *update {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, b) {
		t.Errorf("%s is not the manifest (%v): go test ./internal/agent/tests/bench -run TheRunnersTests -update", p, err)
	}
}
