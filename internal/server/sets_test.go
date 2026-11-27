package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// doneBuild stores a finished build with the files given, as the Builder
// leaves one.
func (l *lab) doneBuild(id, kind string, files ...api.BuildFile) {
	l.t.Helper()
	b := &api.Build{ID: id, Kind: kind, Key: "key-" + id, State: api.BuildDone, Files: files, Created: time.Now(), Release: "6.18.0-aurora1"}
	if kind == "" && len(files) > 0 {
		b.Artifact = files[0].SHA256
	}
	if err := l.s.store.saveBuild(b); err != nil {
		l.t.Fatal(err)
	}
}

func (l *lab) set(token string, req any, path string) (int, api.PackageSet, string) {
	l.t.Helper()
	r, body := l.as(token, "POST", path, req)
	var set api.PackageSet
	if r.StatusCode == 200 {
		if err := json.Unmarshal([]byte(body), &set); err != nil {
			l.t.Fatalf("%s: %v", body, err)
		}
	}
	return r.StatusCode, set, body
}

// A Package set names every package's source, and labd resolves each file,
// an uploaded one or a Builder build's, to its sha256 as the set is made.
// Once made it never changes: the same name is refused.
func TestAPackageSetResolvesEveryFileToItsSHA256(t *testing.T) {
	l := newLab(t, false)
	mesa := l.artifact("mesa package")
	kpkg := l.artifact("linux-aurora package")
	l.doneBuild("b-pkg", api.BuildPackage,
		api.BuildFile{Name: "linux-aurora-6.18.0.aurora1-2-aarch64.pkg.tar.zst", SHA256: kpkg, Size: 20},
		api.BuildFile{Name: "linux-aurora-headers-6.18.0.aurora1-2-aarch64.pkg.tar.zst", SHA256: l.artifact("headers"), Size: 7})
	req := api.SetRequest{Name: "7.3-try", Items: []api.SetItem{
		{Package: "linux-aurora", Source: api.SetBuild, Ref: "b-pkg"},
		{Package: "mesa-aurora", Source: api.SetFile, Ref: mesa, Version: "25.3.0-1"},
		{Package: "m1n1-aurora", Source: api.SetRepo, Ref: "1.5.2-1"},
	}}
	code, set, body := l.set(harnessToken, req, "/api/sets")
	if code != 200 {
		t.Fatalf("create: %d %s", code, body)
	}
	want := []api.SetItem{
		{Package: "linux-aurora", Source: api.SetBuild, Ref: "b-pkg", Version: "6.18.0.aurora1-2", SHA256: kpkg, File: "linux-aurora-6.18.0.aurora1-2-aarch64.pkg.tar.zst"},
		{Package: "mesa-aurora", Source: api.SetFile, Ref: mesa, Version: "25.3.0-1", SHA256: mesa},
		{Package: "m1n1-aurora", Source: api.SetRepo, Ref: "1.5.2-1", Version: "1.5.2-1"},
	}
	if set.Name != "7.3-try" || set.CreatedBy != harnessMaintainer || len(set.Items) != 3 {
		t.Fatalf("made %+v", set)
	}
	for i, it := range want {
		if set.Items[i] != it {
			t.Errorf("item %d: %+v, want %+v", i, set.Items[i], it)
		}
	}

	if code, _, body := l.set(harnessToken, req, "/api/sets"); code != http.StatusConflict || !strings.Contains(body, "frozen") {
		t.Errorf("the same name again: %d %s", code, body)
	}
	r, body := l.get("GET", "/api/sets/7.3-try")
	var got api.PackageSet
	if r.StatusCode != 200 || json.Unmarshal([]byte(body), &got) != nil || len(got.Items) != 3 || got.Items[0].SHA256 != kpkg {
		t.Fatalf("show: %s %s", r.Status, body)
	}
	r, body = l.get("GET", "/api/sets")
	var list []api.PackageSet
	if r.StatusCode != 200 || json.Unmarshal([]byte(body), &list) != nil || len(list) != 1 || list[0].Name != "7.3-try" {
		t.Fatalf("list: %s %s", r.Status, body)
	}
	if r, _ := l.get("GET", "/api/sets/nope"); r.StatusCode != 404 {
		t.Errorf("an unknown set: %s", r.Status)
	}
	for _, m := range []string{"PUT", "PATCH", "DELETE"} {
		if r, _ := l.get(m, "/api/sets/7.3-try"); r.StatusCode/100 == 2 {
			t.Errorf("%s of a frozen set: %s", m, r.Status)
		}
	}

	// A kernel build's set item is the kernel it built.
	kernel := l.artifact("kernel tarball")
	l.doneBuild("b-kern", "", api.BuildFile{Name: "kernel.tar.zst", SHA256: kernel})
	code, set, body = l.set(harnessToken, api.SetRequest{Name: "k", Items: []api.SetItem{{Package: "linux-aurora", Source: api.SetBuild, Ref: "b-kern"}}}, "/api/sets")
	if code != 200 || set.Items[0].SHA256 != kernel || set.Items[0].Version != "6.18.0-aurora1" {
		t.Errorf("a kernel build: %d %s", code, body)
	}
}

// What cannot be resolved makes no set.
func TestAPackageSetRefusesWhatItCannotResolve(t *testing.T) {
	l := newLab(t, false)
	l.doneBuild("b-pkg", api.BuildPackage, api.BuildFile{Name: "mesa-25.3.0-1-aarch64.pkg.tar.zst", SHA256: l.artifact("m")})
	if err := l.s.store.saveBuild(&api.Build{ID: "b-open", Key: "k2", State: api.BuildRunning, Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for why, it := range map[string]api.SetItem{
		"no such file":        {Package: "mesa", Source: api.SetFile, Ref: strings.Repeat("ab", 32)},
		"no such build":       {Package: "mesa", Source: api.SetBuild, Ref: "b-none"},
		"an unfinished build": {Package: "mesa", Source: api.SetBuild, Ref: "b-open"},
		"not in the build":    {Package: "linux-aurora", Source: api.SetBuild, Ref: "b-pkg"},
		"no source":           {Package: "mesa", Source: "ftp", Ref: "x"},
	} {
		if code, _, body := l.set(harnessToken, api.SetRequest{Name: "s", Items: []api.SetItem{it}}, "/api/sets"); code != 400 {
			t.Errorf("%s: %d %s", why, code, body)
		}
	}
	for why, req := range map[string]api.SetRequest{
		"no items":   {Name: "s"},
		"a bad name": {Name: "a b", Items: []api.SetItem{{Package: "m", Source: api.SetRepo, Ref: "1"}}},
		"twice":      {Name: "s", Items: []api.SetItem{{Package: "m", Source: api.SetRepo, Ref: "1"}, {Package: "m", Source: api.SetRepo, Ref: "2"}}},
	} {
		if code, _, body := l.set(harnessToken, req, "/api/sets"); code != 400 {
			t.Errorf("%s: %d %s", why, code, body)
		}
	}
	if r, body := l.get("GET", "/api/sets"); r.StatusCode != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("sets after refusals: %s", body)
	}
}

// lab set capture records the stack a Mac last reported installed, and
// nothing else it reported.
func TestCaptureRecordsTheStackAMacReported(t *testing.T) {
	l := newLab(t, false)
	l.waitDevice(func(d api.Device) bool { return len(d.Facts.Stack) > 0 })
	code, set, body := l.set(harnessToken, api.SetCapture{Name: "7.2", Mac: l.name}, "/api/sets/capture")
	if code != 200 {
		t.Fatalf("capture: %d %s", code, body)
	}
	if set.CapturedFrom != l.name || len(set.Items) != 4 {
		t.Fatalf("captured %+v", set)
	}
	if it := set.Items[0]; it != (api.SetItem{Package: "linux-aurora", Source: api.SetRepo, Ref: "6.17.5.aurora1-1", Version: "6.17.5.aurora1-1"}) {
		t.Errorf("first item %+v", it)
	}
	if code, _, body := l.set(harnessToken, api.SetCapture{Name: "x", Mac: "no-mac"}, "/api/sets/capture"); code != 404 {
		t.Errorf("an unknown Mac: %d %s", code, body)
	}
}

// Only a Maintainer makes or reads a set.
func TestOnlyAMaintainerMakesAPackageSet(t *testing.T) {
	l := newLab(t, false)
	m := l.addMaintainer(harnessToken, "grace")
	l.removeMaintainer(m.Name)
	req := api.SetRequest{Name: "s", Items: []api.SetItem{{Package: "m", Source: api.SetRepo, Ref: "1"}}}
	if code, _, body := l.set(m.Token, req, "/api/sets"); code != 403 && code != 404 {
		t.Errorf("not a Maintainer: %d %s", code, body)
	}
	if code, _, _ := l.set(harnessToken, req, "/api/sets"); code != 200 {
		t.Fatalf("a Maintainer: %d", code)
	}
	if r, _ := l.as(m.Token, "GET", "/api/sets", nil); r.StatusCode == 200 {
		t.Errorf("not a Maintainer lists sets: %s", r.Status)
	}
}

// Builds shows Maintainers the Package sets, read only, and a set's page
// every package with its source and sha256.
func TestPackageSetsArePagesUnderBuildsForMaintainers(t *testing.T) {
	l := leaseLab(t)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()
	sha := strings.Repeat("c", 64)
	set := &api.PackageSet{Name: "7.3-try", CreatedBy: "ravi", Created: l.s.now(), Items: []api.SetItem{
		{Package: "mesa-aurora", Source: api.SetFile, Ref: sha, Version: "25.3.0-1", SHA256: sha},
		{Package: "m1n1-aurora", Source: api.SetRepo, Ref: "1.5.2-1", Version: "1.5.2-1"},
	}}
	if _, err := l.s.store.addSet(set); err != nil {
		t.Fatal(err)
	}
	page := ravi.page("/builds/sets")
	if !strings.Contains(page, `href="/builds/sets/7.3-try"`) || !strings.Contains(page, "@ravi") || currentInLabNav(t, page) != "Builds" {
		t.Errorf("the sets page:\n%s", page)
	}
	if !strings.Contains(ravi.page("/builds"), `href="/builds/sets"`) {
		t.Error("Builds does not lead to the Package sets")
	}
	text := rowText(ravi.page("/builds/sets/7.3-try"))
	for _, want := range []string{"7.3-try", "mesa-aurora", "25.3.0-1", sha, "m1n1-aurora", "repo", "1.5.2-1", "@ravi"} {
		if !strings.Contains(text, want) {
			t.Errorf("the set's page lacks %q", want)
		}
	}
	for _, target := range []string{"/builds/sets", "/builds/sets/7.3-try"} {
		if w := grace.do("GET", target, ""); w.Code != http.StatusNotFound {
			t.Errorf("an M team member on %s: %d", target, w.Code)
		}
	}
	if w := ravi.do("GET", "/builds/sets/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("an unknown set: %d", w.Code)
	}
}
