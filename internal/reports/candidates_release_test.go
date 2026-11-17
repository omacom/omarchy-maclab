package reports

import (
	"reflect"
	"testing"
)

func onSetNamed(t testing.TB, name string) *Object {
	t.Helper()
	doc := imageRun(t, nil)
	at(doc, "system").(*Object).set("candidate_set", name)
	return doc
}

// packagesOn is every package a run lists, name to version.
func packagesOn(t testing.TB, doc *Object) map[string]string {
	t.Helper()
	packages := map[string]string{}
	for _, item := range at(doc, "system", "packages").([]any) {
		pkg := asObject(item)
		packages[text(member(pkg, "name"))] = text(member(pkg, "version"))
	}
	if len(packages) < 3 {
		t.Fatalf("the run lists %d packages", len(packages))
	}
	return packages
}

func with(packages map[string]string, change func(map[string]string)) map[string]string {
	copied := map[string]string{}
	for name, version := range packages {
		copied[name] = version
	}
	change(copied)
	return copied
}

// A Release and a candidate set are two records, linked when the Release's
// packages match the set's: every package a Verified run of the set names,
// at its version, and no package the run does not name.
func TestAReleaseIsLinkedToTheSetWhoseVerifiedRunsNameExactlyItsPackages(t *testing.T) {
	s := newShelf(t)
	run := onSetNamed(t, "edge-1")
	s.add(run, "verified:a")
	set := s.published().CandidateSet("edge-1")
	all := packagesOn(t, run)
	kernel, boot := all["linux-aurora"], all["omarchy-mac-boot"]
	for _, c := range []struct {
		what     string
		packages map[string]string
		want     bool
	}{
		{"every package of the set", all, true},
		{"its kernel package alone", map[string]string{"linux-aurora": kernel}, false},
		{"its kernel and boot packages alone", map[string]string{"linux-aurora": kernel, "omarchy-mac-boot": boot}, false},
		{"its omarchy package alone", map[string]string{"omarchy": all["omarchy"]}, false},
		{"another build of the kernel", with(all, func(p map[string]string) { p["linux-aurora"] = "7.1.12.aurora2-11" }), false},
		{"one package missing", with(all, func(p map[string]string) { delete(p, "mesa") }), false},
		{"a package the run does not list", with(all, func(p map[string]string) { p["linux-firmware"] = "1-1" }), false},
		{"no package", map[string]string{}, false},
	} {
		if got := set.Carries(c.packages); got != c.want {
			t.Errorf("a Release of %s: linked %v, want %v", c.what, got, c.want)
		}
	}
}

// Community runs are listed and do not count: what one says of its set's
// packages links nothing.
func TestACommunityRunLinksNoRelease(t *testing.T) {
	s := newShelf(t)
	run := onSetNamed(t, "edge-1")
	s.add(run, "a")
	p := s.published()
	packages := packagesOn(t, run)
	if p.CandidateSet("edge-1").Carries(packages) || len(p.SetsCarrying(packages)) != 0 {
		t.Error("a Community run's packages linked a Release to its set")
	}
}

// The lab knowing a set by its omarchy package (candidate_sets.yml) links
// no Release by that package alone: the set's other packages must match too.
func TestAKnownSetsOmarchyPackageAloneLinksNoRelease(t *testing.T) {
	const set = "apple-test-1937418f520b-20260926"
	known := ""
	for _, k := range KnownSets() {
		if k.Name == set {
			known = k.Omarchy
		}
	}
	if known == "" {
		t.Fatalf("candidate_sets.yml no longer has %s", set)
	}
	s := newShelf(t)
	named := imageRun(t, nil)
	s.add(named, "a")
	s.add(onSetNamed(t, "edge-1"), "verified:b")
	p := s.published()
	all := packagesOn(t, named)
	if all["omarchy"] != known {
		t.Fatalf("the run is not on the known set's omarchy package: %s", all["omarchy"])
	}
	if p.CandidateSet(set).Carries(map[string]string{"omarchy": known, "linux-aurora": "9.9-1"}) {
		t.Error("a Release with the known set's omarchy package and another kernel is the set's")
	}
	if p.CandidateSet(set).Carries(map[string]string{"omarchy": known}) {
		t.Error("a Release of the known set's omarchy package alone is the set's")
	}
	if p.CandidateSet(set).Carries(all) {
		t.Error("a Community run on the known set linked a Release")
	}

	// A Verified run on the known set makes it linkable like any other.
	s.add(imageRun(t, nil), "verified:c")
	p = s.published()
	if got := p.SetsCarrying(all); !reflect.DeepEqual(got, []string{set, "edge-1"}) && !reflect.DeepEqual(got, []string{"edge-1", set}) {
		t.Errorf("the sets of a Release of every package on both: %v", got)
	}
	if got := p.SetsCarrying(map[string]string{"omarchy": known}); len(got) != 0 {
		t.Errorf("the sets of a Release of the omarchy package alone: %v", got)
	}
}

// onlyPackages makes the run list exactly these packages, in order.
func onlyPackages(doc *Object, pairs ...string) *Object {
	var list []any
	for i := 0; i+1 < len(pairs); i += 2 {
		list = append(list, object("name", pairs[i], "version", pairs[i+1]))
	}
	at(doc, "system").(*Object).set("packages", list)
	return doc
}

// Spec: "One package alone ... links nothing", whatever the run names. A
// Verified run that lists one package links no Release of that package.
func TestAVerifiedRunNamingOnePackageLinksNoRelease(t *testing.T) {
	s := newShelf(t)
	s.add(onlyPackages(onSetNamed(t, "edge-1"), "omarchy", "3.1.0-1"), "verified:a")
	s.add(onlyPackages(onSetNamed(t, "edge-2"), "linux-aurora", "7.1.12.aurora1-1"), "verified:b")
	p := s.published()
	for set, release := range map[string]map[string]string{
		"edge-1": {"omarchy": "3.1.0-1"},
		"edge-2": {"linux-aurora": "7.1.12.aurora1-1"},
	} {
		if p.CandidateSet(set).Carries(release) {
			t.Errorf("a Release of %v alone is %s's, whose Verified run names only it", release, set)
		}
		if got := p.SetsCarrying(release); len(got) != 0 {
			t.Errorf("the sets of a Release of %v alone: %v", release, got)
		}
	}
}

// A run that names one package at two versions says nothing certain of
// that package: it links no Release, whichever of the two it has.
func TestARunNamingAPackageAtTwoVersionsLinksNoRelease(t *testing.T) {
	s := newShelf(t)
	s.add(onlyPackages(onSetNamed(t, "edge-1"),
		"omarchy", "3.1.0-1", "omarchy", "3.2.0-1", "linux-aurora", "7.1.12.aurora1-1"), "verified:a")
	set := s.published().CandidateSet("edge-1")
	for _, omarchy := range []string{"3.1.0-1", "3.2.0-1"} {
		if set.Carries(map[string]string{"omarchy": omarchy, "linux-aurora": "7.1.12.aurora1-1"}) {
			t.Errorf("a Release with omarchy %s is the set's, whose run names omarchy at two versions", omarchy)
		}
	}
}
