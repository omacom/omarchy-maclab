package server

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// addsSet is the set Performance runs measure with, with the Quick list's
// AI metric adding the package add: a set as the lab runs it, its files
// placeholders, as perfLab has them.
func addsSet(t *testing.T, add string) (*sets.Set, string) {
	t.Helper()
	base, err := perfCurrentSet()
	if err != nil {
		t.Fatal(err)
	}
	s := *base
	s.Metrics = slices.Clone(base.Metrics)
	id := ""
	for i, m := range s.Metrics {
		if m.Category == "AI" && m.List == sets.Quick {
			o := *m.Omarchy
			o.Adds = []string{add}
			s.Metrics[i].Omarchy = &o
			id = m.ID
		}
	}
	if id == "" {
		t.Fatal("no Quick AI metric")
	}
	return &s, id
}

// A metric that adds a package is measured apart from the Release: after the
// Release's own run, a Lab-owned Mac resolves the package and what it needs,
// labd keeps them with the Release's set, frozen, and the Mac boots that set
// offline; its value is kept under the Release and the package, and
// published apart from the Release's.
func TestAMetricThatAddsAPackageIsMeasuredApartOnTheReleaseWithIt(t *testing.T) {
	l, _ := perfLab(t, labConfig{}, nil)
	set, added := addsSet(t, "omarchy-test-add")
	was := perfCurrentSet
	perfCurrentSet = func() (*sets.Set, error) { return set, nil }
	t.Cleanup(func() { perfCurrentSet = was })
	oldPart := api.AddsPart
	api.AddsPart = 16
	t.Cleanup(func() { api.AddsPart = oldPart })
	rel := l.perfRelease("7.3", l.s.now(), []string{l.name})
	l.s.perfKick()

	own := l.nthPerfJob(1)
	if own.Outcome != api.OutcomePass || slices.Contains(own.Spec.Performance.Metrics, added) || len(own.Spec.Performance.Adds) != 0 {
		t.Fatalf("the Release's own run: %s, %v adds %v", own.Outcome, own.Spec.Performance.Metrics, own.Spec.Performance.Adds)
	}
	with := l.nthPerfJob(2)
	ps := with.Spec.Performance
	if with.Outcome != api.OutcomePass || !slices.Equal(ps.Metrics, []string{added}) || !slices.Equal(ps.Adds, []string{"omarchy-test-add"}) {
		t.Fatalf("the run with the package: %s %s, %v adds %v", with.Outcome, with.Summary, ps.Metrics, ps.Adds)
	}
	name := api.PerfAddsSet(rel.Name, ps.Adds)
	kept, err := l.s.packageSet(name)
	if err != nil || ps.Packages.Name != name {
		t.Fatalf("the kept set %s: %v (the run booted %s)", name, err, ps.Packages.Name)
	}
	var names []string
	for _, it := range kept.Items {
		names = append(names, it.Package)
		if it.Source != api.SetFile || l.s.setFilesStored(&api.PackageSet{Items: []api.SetItem{it}}) != nil {
			t.Errorf("%s is not a file in the artifact store: %+v", it.Package, it)
		}
	}
	if !slices.Equal(names, []string{"linux-aurora", "omarchy-test-add", "omarchy-test-add-lib"}) {
		t.Errorf("the kept set holds %v", names)
	}
	ops := strings.Join(l.sim.CandidateOps(), ", ")
	if !strings.Contains(ops, "resolve adds omarchy-test-add") || !strings.Contains(ops, "stage "+name+" offline") || !strings.Contains(ops, "stage release-7.3 offline") {
		t.Errorf("candidate ops: %s", ops)
	}
	cur, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, set.Name, "7.3+omarchy-test-add")
	if len(cur) != 1 || cur[0].Metric != added {
		t.Errorf("values with the package: %+v", cur)
	}
	if own, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, set.Name, "7.3"); slices.ContainsFunc(own, func(r perf.Result) bool { return r.Metric == added }) {
		t.Errorf("the Release's own values hold %s", added)
	}
	p, err := l.s.publishedPerf()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range p.Values {
		if v.Metric == added && v.Adds != "omarchy-test-add" {
			t.Errorf("published %s as the Release's own: %+v", added, v)
		}
	}
	l.noMorePerf(2)

	// The next run that needs it boots the kept set; nothing is resolved again.
	resolves := strings.Count(ops, "resolve adds")
	if again := l.s.keptAddsSet(rel, ps.Adds); again == nil || again.Name != name {
		t.Fatalf("the set is not kept: %+v", again)
	}
	if n := strings.Count(strings.Join(l.sim.CandidateOps(), ", "), "resolve adds"); n != resolves {
		t.Errorf("resolved %d times", n)
	}
}

// A Volunteer's Mac never resolves what the lab adds: it measures those
// metrics only once a Lab-owned Mac has.
func TestAVolunteersMacMeasuresWhatTheLabAddsOnlyOnAKeptSet(t *testing.T) {
	l, _ := perfLab(t, labConfig{volunteer: true}, func(l *lab) {
		l.mustVolunteer("window", "add", "always")
	})
	set, added := addsSet(t, "omarchy-test-add")
	was := perfCurrentSet
	perfCurrentSet = func() (*sets.Set, error) { return set, nil }
	t.Cleanup(func() { perfCurrentSet = was })
	l.perfRelease("7.3", l.s.now(), []string{l.name})
	l.mustVolunteer("performance", "on")
	l.mustVolunteer("access", "full")
	if j := l.nthPerfJob(1); slices.Contains(j.Spec.Performance.Metrics, added) {
		t.Fatalf("the Release's own run measured %s", added)
	}
	l.noMorePerf(1)
	if strings.Contains(strings.Join(l.sim.CandidateOps(), ", "), "resolve adds") {
		t.Error("a Volunteer's Mac resolved packages")
	}
}

// What a Mac sends that is no package the lab adds, or that would change the
// Release, makes no set.
func TestAddsSetOfRefusesWhatWouldChangeTheRelease(t *testing.T) {
	rel := api.Release{Name: "7.3"}
	base := &api.PackageSet{Name: "release-7.3", Items: []api.SetItem{{Package: "mesa", Version: "1-1", SHA256: strings.Repeat("a", 64)}}}
	file := func(name, version string) api.AddsFile {
		return api.AddsFile{Name: name, Version: version, File: name + "-" + version + "-aarch64.pkg.tar.zst", SHA256: strings.Repeat("b", 64), Size: 10, Parts: 1}
	}
	for why, got := range map[string]api.AddsResolved{
		"a newer Mesa":        {Files: []api.AddsFile{file("x", "1-1"), file("mesa", "2-1")}},
		"no file of x":        {Files: []api.AddsFile{file("y", "1-1")}},
		"a path for a file":   {Files: []api.AddsFile{{Name: "x", Version: "1-1", File: "../x-1-1-aarch64.pkg.tar.zst", SHA256: strings.Repeat("b", 64), Size: 1, Parts: 1}}},
		"parts that add up":   {Files: []api.AddsFile{{Name: "x", Version: "1-1", File: "x-1-1-aarch64.pkg.tar.zst", SHA256: strings.Repeat("b", 64), Size: 1, Parts: 3}}},
		"not a package file":  {Files: []api.AddsFile{{Name: "x", Version: "1-1", File: "x-1-1.tar.gz", SHA256: strings.Repeat("b", 64), Size: 1, Parts: 1}}},
		"a sha256 that isn't": {Files: []api.AddsFile{{Name: "x", Version: "1-1", File: "x-1-1-aarch64.pkg.tar.zst", SHA256: "x", Size: 1, Parts: 1}}},
	} {
		if _, err := addsSetOf(rel, base, []string{"x"}, got, time.Now()); err == nil {
			t.Errorf("%s made a set", why)
		}
	}
	set, err := addsSetOf(rel, base, []string{"x"}, api.AddsResolved{Files: []api.AddsFile{file("x", "1-1"), file("mesa", "1-1")}}, time.Now())
	if err != nil || len(set.Items) != 2 {
		t.Errorf("the Release's own Mesa again: %+v %v", set, err)
	}
}
