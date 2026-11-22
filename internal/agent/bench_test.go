package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// benchFixture stands a fake runner, a fake set and a fake labd in for the
// real ones: the runner records how it was called.
type benchFixture struct {
	l       *Linux
	root    string
	cache   string
	calls   string
	fetched []string
	blobs   map[string][]byte
}

func pinnedFile(id string) (sets.File, []byte) {
	b := []byte("pinned " + id)
	s := sha256.Sum256(b)
	return sets.File{ID: id, Version: "1", Kind: "file", URL: "https://example.org/" + id, SHA256: hex.EncodeToString(s[:]), Size: int64(len(b))}, b
}

// fakeSet has a Quick metric needing a and b, a Full one needing b and c,
// and one whose file is still a placeholder.
func (f *benchFixture) fakeSet() *sets.Set {
	var files []sets.File
	for _, id := range []string{"a", "b", "c"} {
		pf, b := pinnedFile(id)
		f.blobs[pf.SHA256] = b
		files = append(files, pf)
	}
	files = append(files, sets.File{ID: "p", Version: "1", Kind: "tarball", SHA256: sets.Placeholder})
	return &sets.Set{Name: "bench-7", Schema: 1, Files: map[string][]sets.File{sets.Omarchy: files},
		Metrics: []sets.Metric{
			{ID: "q", List: sets.Quick, Omarchy: &sets.Side{Files: []string{"a", "b"}}},
			{ID: "f", List: sets.Full, Omarchy: &sets.Side{Files: []string{"b", "c"}}},
			{ID: "x", List: sets.Full, Omarchy: &sets.Side{Files: []string{"p"}}},
		}}
}

func newBenchFixture(t *testing.T) *benchFixture {
	t.Helper()
	root := t.TempDir()
	f := &benchFixture{l: &Linux{WorkDir: filepath.Join(root, "agent"), GUIUser: "gui"}, root: root,
		cache: filepath.Join(root, "cache"), calls: filepath.Join(root, "calls"), blobs: map[string][]byte{}}
	py := filepath.Join(root, "python3")
	script := "#!/bin/sh\necho \"$@\" >> " + f.calls + "\n" +
		"while [ $# -gt 0 ]; do [ \"$1\" = --out ] && echo '[]' > \"$2\"; shift; done\necho ran\n"
	if err := os.WriteFile(py, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := f.fakeSet()
	op, oc, ow, os_, og := benchPython, benchCache, benchWorkParent, benchSet, benchGUIEnv
	benchPython, benchCache, benchWorkParent = py, f.cache, root
	benchSet = func() (*sets.Set, error) { return s, nil }
	benchGUIEnv = func(name string, wait time.Duration) (*user.User, []string, error) {
		return &user.User{Username: name, Uid: "1000", Gid: "1000"}, []string{"WAYLAND_DISPLAY=wayland-1"}, nil
	}
	t.Cleanup(func() { benchPython, benchCache, benchWorkParent, benchSet, benchGUIEnv = op, oc, ow, os_, og })
	return f
}

func (f *benchFixture) fetch(ctx context.Context, sha, dst string) error {
	f.fetched = append(f.fetched, sha)
	b, ok := f.blobs[sha]
	if !ok {
		return errors.New("labd has no artifact " + sha)
	}
	return os.WriteFile(dst, b, 0o600)
}

func (f *benchFixture) runs(t *testing.T) []string {
	t.Helper()
	calls, _ := os.ReadFile(f.calls)
	return strings.Split(strings.TrimSpace(string(calls)), "\n")
}

func noQuiet(context.Context, time.Duration) error { return nil }

func TestBenchIsABuiltinForEachList(t *testing.T) {
	for _, n := range []string{"bench", "bench-extended"} {
		if !slices.Contains(Builtins(), n) {
			t.Errorf("Builtins() = %v, no %s", Builtins(), n)
		}
	}
	if d := testTimeout(api.TestSpec{Builtin: "bench"}); d < 20*time.Minute {
		t.Errorf("Quick list timeout %s", d)
	}
	if d := testTimeout(api.TestSpec{Builtin: "bench-extended"}); d < 60*time.Minute {
		t.Errorf("Full list timeout %s", d)
	}
}

func TestBenchFetchesItsListsPinnedFilesFromLabdBySha(t *testing.T) {
	f := newBenchFixture(t)
	a, _ := pinnedFile("a")
	b, _ := pinnedFile("b")
	c, _ := pinnedFile("c")
	out := t.TempDir()
	r := f.l.RunTest(context.Background(), "j1", api.TestSpec{Name: "b", Builtin: "bench"}, f.fetch, noQuiet, out)
	if !r.Passed {
		t.Fatalf("not passed: %+v", r)
	}
	if !slices.Equal(f.fetched, []string{a.SHA256, b.SHA256}) {
		t.Fatalf("fetched %v, want a and b", f.fetched)
	}
	for _, sha := range f.fetched {
		got, err := os.ReadFile(filepath.Join(f.cache, sha))
		if err != nil || string(got) != string(f.blobs[sha]) {
			t.Errorf("cache %s = %q, %v", sha, got, err)
		}
		if st, _ := os.Stat(filepath.Join(f.cache, sha)); st.Mode().Perm() != 0o644 {
			t.Errorf("cache file mode %v", st.Mode())
		}
	}
	run := f.runs(t)
	if len(run) != 1 {
		t.Fatalf("runner calls %q", run)
	}
	setFile := filepath.Join(f.l.jobDir("j1"), "bench", "bench-7.json")
	for _, w := range []string{"--set " + setFile, "--list quick", "--cache " + f.cache, "--out " + filepath.Join(out, "bench.json"),
		"--gui-user gui", "--gui-env-file "} {
		if !strings.Contains(run[0], w) {
			t.Errorf("runner call %q lacks %q", run[0], w)
		}
	}
	var s sets.Set
	if b, err := os.ReadFile(setFile); err != nil || json.Unmarshal(b, &s) != nil || s.Name != "bench-7" || len(s.Metrics) != 3 {
		t.Errorf("the set the runner reads: %+v, %v", s, err)
	}
	if _, err := os.Stat(filepath.Join(out, "bench.json")); err != nil {
		t.Error("no bench.json in the test's output")
	}

	// The Full list fetches only what the cache lacks, and never a placeholder.
	f.fetched = nil
	if r := f.l.RunTest(context.Background(), "j2", api.TestSpec{Name: "b", Builtin: "bench-extended"}, f.fetch, noQuiet, t.TempDir()); !r.Passed {
		t.Fatalf("second run: %+v", r)
	}
	if !slices.Equal(f.fetched, []string{c.SHA256}) {
		t.Errorf("fetched %v, want c alone", f.fetched)
	}
	if run := f.runs(t); !strings.Contains(run[len(run)-1], "--list full") {
		t.Errorf("Full list not asked for: %q", run)
	}
}

func TestBenchRemovesWhatAnUnfinishedRunLeft(t *testing.T) {
	f := newBenchFixture(t)
	stale := filepath.Join(f.root, "lab-bench-123", "fio")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(stale, "scratch"), []byte("8 GiB"), 0o644)
	if r := f.l.RunTest(context.Background(), "j1", api.TestSpec{Name: "b", Builtin: "bench"}, f.fetch, noQuiet, t.TempDir()); !r.Passed {
		t.Fatalf("result %+v", r)
	}
	if left, _ := filepath.Glob(filepath.Join(f.root, "lab-bench-*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
}

func TestBenchRefusesAFetchThatIsNotThePinnedFile(t *testing.T) {
	f := newBenchFixture(t)
	for sha := range f.blobs {
		f.blobs[sha] = []byte("something else")
	}
	r := f.l.RunTest(context.Background(), "j1", api.TestSpec{Name: "b", Builtin: "bench"}, f.fetch, noQuiet, t.TempDir())
	if r.Passed || !strings.Contains(r.Error, "sha256") {
		t.Fatalf("result %+v", r)
	}
	if ents, _ := os.ReadDir(f.cache); len(ents) != 0 {
		t.Errorf("cache kept %v", ents)
	}
}

func TestBenchNeedsTheDesktopSession(t *testing.T) {
	f := newBenchFixture(t)
	f.l.GUIUser = ""
	r := f.l.RunTest(context.Background(), "j1", api.TestSpec{Name: "b", Builtin: "bench"}, f.fetch, noQuiet, t.TempDir())
	if r.Passed || !strings.Contains(r.Error, "gui") {
		t.Fatalf("result %+v", r)
	}
}
