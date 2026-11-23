package bench

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

func runner(t *testing.T, arg string) []string {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	out, err := exec.Command(py, "-I", "bench.py", arg).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

func TestTheRunnerRunsEveryMetricOfEverySetByItsId(t *testing.T) {
	have := runner(t, "--metrics")
	all, err := sets.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range all {
		for _, m := range s.Metrics {
			if m.Omarchy != nil && !slices.Contains(have, m.ID) {
				t.Errorf("%s: the runner has no %s", s.Name, m.ID)
			}
		}
	}
}

// labd keeps the same list; bench.json carries no other reason.
func TestTheRunnersReasonCodes(t *testing.T) {
	want := []string{"no_build", "no_driver", "fell_back", "no_comparable_sensor", "not_run", "skipped", "error",
		"disk_too_full", "not_on_ac", "not_idle", "software_renderer", "placeholder_file", "battery_charging"}
	if got := runner(t, "--reasons"); !slices.Equal(got, want) {
		t.Errorf("reason codes %v, want %v", got, want)
	}
}

func TestTheBuiltinsRunTheQuickAndFullLists(t *testing.T) {
	if Builtins["bench"] != sets.Quick || Builtins["bench-extended"] != sets.Full || len(Builtins) != 2 {
		t.Errorf("builtins %v", Builtins)
	}
	s, err := Set()
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := sets.Current(); err == nil && cur.Name != s.Name {
		t.Errorf("runs %s, but %s is current", s.Name, cur.Name)
	}
	quick, full := Files(s, sets.Quick), Files(s, sets.Full)
	if len(quick) == 0 || len(full) <= len(quick) {
		t.Fatalf("files: quick %d, full %d", len(quick), len(full))
	}
	ids := map[string]bool{}
	for _, f := range full {
		if !f.Pinned() || ids[f.ID] {
			t.Errorf("%s fetched unpinned or twice", f.ID)
		}
		ids[f.ID] = true
		if _, ok := s.File(sets.Omarchy, f.ID); !ok {
			t.Errorf("%s is not an Omarchy file", f.ID)
		}
	}
	for _, f := range quick {
		if !ids[f.ID] {
			t.Errorf("quick fetches %s, full does not", f.ID)
		}
	}
}

// A Performance run of a whole list fetches what the builtin does; of one
// metric, only that metric's files; of a metric outside the list, nothing.
func TestAPerformanceRunFetchesItsMetricsFiles(t *testing.T) {
	s, err := Set()
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{sets.Quick, sets.Full} {
		var ids []string
		for _, m := range s.List(list) {
			if m.Omarchy != nil {
				ids = append(ids, m.ID)
			}
		}
		got, err := FilesOf(s, list, ids)
		if err != nil || !slices.Equal(got, Files(s, list)) {
			t.Errorf("%s: %v (%v), want %v", list, got, err, Files(s, list))
		}
	}
	for _, m := range s.Metrics {
		got, _ := FilesOf(s, sets.Full, []string{m.ID})
		for _, f := range got {
			if !slices.Contains(m.Omarchy.Files, f.ID) {
				t.Errorf("%s fetches %s", m.ID, f.ID)
			}
		}
	}
	full := slices.IndexFunc(s.Metrics, func(m sets.Metric) bool { return m.List == sets.Full })
	for _, ids := range [][]string{{s.Metrics[full].ID}, {"nope"}, nil} {
		if _, err := FilesOf(s, sets.Quick, ids); err == nil {
			t.Errorf("quick %v: no error", ids)
		}
	}
}

func TestEveryPinnedFileOfEverySetIsKept(t *testing.T) {
	shas, err := Pinned()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := sets.All()
	for _, s := range all {
		for _, sys := range sets.Systems {
			for _, f := range s.Files[sys] {
				if f.Pinned() != slices.Contains(shas, f.SHA256) {
					t.Errorf("%s/%s/%s pinned %v, kept %v", s.Name, sys, f.ID, f.Pinned(), !f.Pinned())
				}
			}
		}
	}
	if len(slices.Compact(slices.Sorted(slices.Values(shas)))) != len(shas) {
		t.Error("a sha256 given twice")
	}
}
