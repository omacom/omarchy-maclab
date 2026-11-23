package agent

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

func (f *benchFixture) named(t *testing.T) {
	s, _ := benchSet()
	old := benchSetNamed
	benchSetNamed = func(name string) (*sets.Set, error) {
		if name != s.Name {
			return sets.Load(name)
		}
		return s, nil
	}
	t.Cleanup(func() { benchSetNamed = old })
}

func TestAPerformanceRunRunsOnlyItsMetricsAndFetchesTheirFiles(t *testing.T) {
	f := newBenchFixture(t)
	f.named(t)
	b, _ := pinnedFile("b")
	c, _ := pinnedFile("c")
	out := t.TempDir()
	spec := api.TestSpec{Name: "perf-bench", Builtin: "bench", Bench: &api.BenchRun{Set: "bench-7", List: sets.Full, Metrics: []string{"f"}, Run: 3}}
	if r := f.l.RunTest(context.Background(), "j1", spec, f.fetch, noQuiet, out); !r.Passed {
		t.Fatalf("not passed: %+v", r)
	}
	if !slices.Equal(f.fetched, []string{b.SHA256, c.SHA256}) {
		t.Errorf("fetched %v, want b and c alone", f.fetched)
	}
	run := f.runs(t)
	if len(run) != 1 || !strings.Contains(run[0], "--list full --only f ") {
		t.Errorf("runner calls %q", run)
	}

	// The builtin without a Performance run runs its whole list, as Compare has it.
	f.fetched = nil
	if r := f.l.RunTest(context.Background(), "j2", api.TestSpec{Name: "b", Builtin: "bench"}, f.fetch, noQuiet, t.TempDir()); !r.Passed {
		t.Fatalf("not passed: %+v", r)
	}
	if run := f.runs(t); strings.Contains(run[len(run)-1], "--only") {
		t.Errorf("runner call %q", run[len(run)-1])
	}

	for _, bad := range []*api.BenchRun{
		{Set: "bench-7", List: sets.Quick, Metrics: []string{"f"}},
		{Set: "bench-7", List: sets.Full, Metrics: []string{"nope"}},
		{Set: "bench-7", List: sets.Full},
		{Set: "bench-7", List: "most", Metrics: []string{"q"}},
		{Set: "bench-999", List: sets.Full, Metrics: []string{"q"}},
	} {
		spec.Bench = bad
		if r := f.l.RunTest(context.Background(), "j3", spec, f.fetch, noQuiet, t.TempDir()); r.Passed || r.Error == "" {
			t.Errorf("%+v: %+v", bad, r)
		}
	}
	if n := len(f.runs(t)); n != 2 {
		t.Errorf("the runner ran %d times, want 2", n)
	}
}

func benchRow(metric, reason string) map[string]any {
	r := map[string]any{"set": "bench-1", "metric": metric, "value": 1.0, "reason": nil}
	if reason != "" {
		r["value"], r["reason"] = nil, reason
	}
	return r
}

func writeBenchOut(t *testing.T, dir string, rows []map[string]any, cond map[string]any) {
	t.Helper()
	b, _ := json.Marshal(rows)
	os.WriteFile(filepath.Join(dir, "bench.json"), b, 0o644)
	if cond != nil {
		b, _ = json.Marshal(cond)
		os.WriteFile(filepath.Join(dir, "bench-conditions.json"), b, 0o644)
	}
}

func TestAPerformanceRunsUploadSucceedsOnlyWithEveryMetricAndNoneInError(t *testing.T) {
	b := &api.BenchRun{Set: "bench-1", List: sets.Quick, Metrics: []string{"a", "b"}, Run: 9}
	for _, c := range []struct {
		name  string
		rows  []map[string]any
		state string
		n     int
	}{
		{"every metric", []map[string]any{benchRow("a", ""), benchRow("b", perf.FellBack)}, perf.Succeeded, 2},
		{"in error", []map[string]any{benchRow("a", ""), benchRow("b", perf.Error)}, perf.Failed, 2},
		{"software renderer", []map[string]any{benchRow("a", perf.SoftwareRenderer), benchRow("b", "")}, perf.Failed, 2},
		{"one missing", []map[string]any{benchRow("a", "")}, perf.Failed, 1},
		{"one not asked for", []map[string]any{benchRow("a", ""), benchRow("b", ""), benchRow("c", "")}, perf.Succeeded, 2},
		{"none", nil, perf.Failed, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBenchOut(t, dir, c.rows, nil)
			up := benchResults(b, dir)
			if up.State != c.state || len(up.Entries) != c.n || up.Conditions != nil {
				t.Errorf("upload %+v", up)
			}
		})
	}
	up := benchResults(b, t.TempDir())
	if up.State != perf.Failed || up.Entries == nil || len(up.Entries) != 0 {
		t.Errorf("no bench.json: %+v", up)
	}

	dir := t.TempDir()
	writeBenchOut(t, dir, []map[string]any{benchRow("a", ""), benchRow("b", "")}, map[string]any{
		"set": "bench-1", "list": "quick", "refused": nil, "battery": true, "on_ac": true,
		"cpu_busy_percent": 3.5, "power_profile": nil, "backlight_percent": 50})
	var cond map[string]any
	if err := json.Unmarshal(benchResults(b, dir).Conditions, &cond); err != nil || len(cond) != 3 ||
		cond["on_ac"] != true || cond["cpu_busy_percent"] != 3.5 || cond["backlight_percent"] != 50.0 {
		t.Errorf("conditions %v (%v)", cond, err)
	}
}

// fakePerfLabd answers lab-agent's uploads with the statuses given, then 200.
type fakePerfLabd struct {
	mu       sync.Mutex
	statuses []int
	got      []perf.Upload
	auth     []string
	paths    []string
}

func (f *fakePerfLabd) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var up perf.Upload
	json.NewDecoder(r.Body).Decode(&up)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got, f.auth, f.paths = append(f.got, up), append(f.auth, r.Header.Get("Authorization")), append(f.paths, r.Method+" "+r.URL.Path)
	if len(f.statuses) > 0 {
		code := f.statuses[0]
		f.statuses = f.statuses[1:]
		http.Error(w, "not now", code)
		return
	}
	json.NewEncoder(w).Encode(perf.Taken{Run: 9, Attempt: 1, State: up.State, Taken: len(up.Entries)})
}

func perfAgent(t *testing.T, labd http.Handler) *Agent {
	t.Helper()
	srv := httptest.NewServer(labd)
	t.Cleanup(srv.Close)
	old := benchUploadWait
	benchUploadWait = time.Millisecond
	t.Cleanup(func() { benchUploadWait = old })
	dir := t.TempDir()
	return New(&Config{Server: srv.URL, Secret: "dev_x", WorkDir: dir}, NewSim(dir), log.New(io.Discard, "", 0))
}

func TestAPerformanceRunsUploadIsTriedAgainUntilLabdTakesIt(t *testing.T) {
	labd := &fakePerfLabd{statuses: []int{503, 502}}
	a := perfAgent(t, labd)
	b := &api.BenchRun{Set: "bench-1", List: sets.Quick, Metrics: []string{"a"}, Run: 9}
	dir := t.TempDir()
	writeBenchOut(t, dir, []map[string]any{benchRow("a", "")}, map[string]any{"on_ac": true})
	up := a.benchUpload(context.Background(), b, dir)
	if !up.Taken || up.Tries != 3 || up.State != perf.Succeeded || up.Results != 1 || up.Error != "" {
		t.Fatalf("upload %+v", up)
	}
	if labd.paths[0] != "POST /api/agent/performance/9/results" || labd.auth[0] != "Bearer dev_x" {
		t.Errorf("sent %v with %v", labd.paths, labd.auth)
	}
	if got := labd.got[2]; len(got.Entries) != 1 || got.State != perf.Succeeded || string(got.Conditions) != `{"on_ac":true}` {
		t.Errorf("labd got %+v", got)
	}
	res := api.TestResult{Name: "perf-bench", Passed: true}
	benchUploaded(&res, up)
	if !res.Passed || res.Bench != up {
		t.Errorf("result %+v", res)
	}
}

func TestAPerformanceRunsUploadLabdRefusesOrNeverTakesFailsTheTest(t *testing.T) {
	b := &api.BenchRun{Set: "bench-1", List: sets.Quick, Metrics: []string{"a"}, Run: 9}
	dir := t.TempDir()
	writeBenchOut(t, dir, []map[string]any{benchRow("a", "")}, nil)
	for _, c := range []struct {
		statuses []int
		tries    int
	}{
		{[]int{400}, 1},
		{[]int{409}, 1},
		{[]int{503, 503, 503, 503, 503}, benchUploadTries},
	} {
		up := perfAgent(t, &fakePerfLabd{statuses: c.statuses}).benchUpload(context.Background(), b, dir)
		if up.Taken || up.Tries != c.tries || up.Error == "" {
			t.Errorf("%v: upload %+v", c.statuses, up)
		}
		res := api.TestResult{Name: "perf-bench", Passed: true}
		benchUploaded(&res, up)
		if res.Passed || !strings.Contains(res.Error, "did not take the upload") {
			t.Errorf("%v: result %+v", c.statuses, res)
		}
	}

	// Taken, but the attempt failed: the test fails, and says nothing more.
	writeBenchOut(t, dir, []map[string]any{benchRow("a", perf.Error)}, nil)
	up := perfAgent(t, &fakePerfLabd{}).benchUpload(context.Background(), b, dir)
	res := api.TestResult{Name: "perf-bench", Passed: true}
	benchUploaded(&res, up)
	if !up.Taken || up.State != perf.Failed || res.Passed || res.Error != "" {
		t.Errorf("upload %+v, result %+v", up, res)
	}
}
