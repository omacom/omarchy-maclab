package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// SetBenchAnswer sets what the simulated bench runner answers on a
// Performance run for each metric: a value, or a reason code. Unset, every
// metric measures 100.
func (s *Sim) SetBenchAnswer(f func(run int64, metric string) (float64, string)) {
	s.mu.Lock()
	s.benchAnswer = f
	s.mu.Unlock()
}

// BenchRuns are the metrics of each Performance run the simulated Mac ran,
// in the order bench.json has them.
func (s *Sim) BenchRuns() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.benchRuns)
}

// simBench stands in for bench.py on a Performance run: bench.json with a
// result for each metric of the set's list labd named, in the set's order,
// and bench-conditions.json beside it, as the runner writes them. lab-agent
// uploads them.
func (s *Sim) simBench(t api.TestSpec, outDir string) api.TestResult {
	res := api.TestResult{Name: t.Name, ExitCode: -1}
	b := t.Bench
	set, err := sets.Load(b.Set)
	if err == nil {
		_, err = bench.FilesOf(set, b.List, b.Metrics)
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	s.mu.Lock()
	answer := s.benchAnswer
	s.mu.Unlock()
	var entries []perf.Entry
	var ran []string
	code := 0
	for _, m := range set.List(b.List) {
		if !slices.Contains(b.Metrics, m.ID) {
			continue
		}
		v, reason := 100.0, ""
		if answer != nil {
			v, reason = answer(b.Run, m.ID)
		}
		e := perf.Entry{Set: set.Name, Metric: m.ID, Category: m.Category, Version: m.Version, Unit: m.Unit,
			HigherIsBetter: m.HigherIsBetter(), Seconds: 30, CoolDown: 5, API: m.Omarchy.API}
		if reason != "" {
			e.Reason = &reason
			if reason == perf.Error || reason == perf.SoftwareRenderer {
				code = 1
			}
		} else {
			e.Value = &v
		}
		if m.Category == "Storage" {
			gb := int64(512)
			e.Capacity = &gb
		}
		entries = append(entries, e)
		ran = append(ran, m.ID)
	}
	s.mu.Lock()
	s.benchRuns = append(s.benchRuns, ran)
	s.mu.Unlock()
	rows, _ := json.MarshalIndent(entries, "", "  ")
	cond, _ := json.MarshalIndent(map[string]any{"set": set.Name, "list": b.List, "refused": nil, "on_ac": true,
		"battery": true, "cpu_busy_percent": 2.5, "power_profile": "balanced", "backlight_percent": 50}, "", "  ")
	os.WriteFile(filepath.Join(outDir, "bench.json"), append(rows, '\n'), 0o644)
	os.WriteFile(filepath.Join(outDir, "bench-conditions.json"), append(cond, '\n'), 0o644)
	os.WriteFile(filepath.Join(outDir, "log.txt"), []byte("sim bench "+t.Name+"\n"), 0o644)
	res.Passed, res.ExitCode, res.Seconds = code == 0, code, 0.01
	return res
}
