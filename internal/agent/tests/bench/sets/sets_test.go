package sets

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const sha = "e131ad42621442758a3acb899bfbdfeeab0b40c7e2f7c7e66683f58a09a99aee"

// valid is a small set that passes: one metric per category, all Quick but
// one, and one pinned file on each system.
func valid() *Set {
	s := &Set{Name: "bench-7", Schema: 1, Files: map[string][]File{
		Omarchy: {{ID: "prog", Version: "1", Kind: "tarball", URL: "https://example.org/p.tar.gz", SHA256: sha, Size: 10}},
		MacOS:   {{ID: "prog", Version: "1", Kind: "tarball", URL: "https://example.org/m.tar.gz", SHA256: sha, Size: 10}},
	}}
	for i, c := range Categories {
		m := Metric{ID: strings.ToLower(strings.ReplaceAll(c, " ", "-")), Name: c, Category: c, Version: "1", Unit: "score",
			Direction: "higher", Tolerance: 5, List: Quick, Omarchy: &Side{Files: []string{}}}
		if i == 0 {
			m.Omarchy.Files = []string{"prog"}
		}
		if c != OmarchyOnly {
			m.MacOS = &Side{Files: []string{}}
			if i == 0 {
				m.MacOS.Files = []string{"prog"}
			}
		}
		s.Metrics = append(s.Metrics, m)
	}
	extra := s.Metrics[0]
	extra.ID, extra.List = "cpu-more", Full
	s.Metrics = append(s.Metrics, extra)
	return s
}

func parse(t *testing.T, s *Set) error {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse(s.Name, b)
	return err
}

func refused(t *testing.T, s *Set, want string) {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Parse("bench-7", b)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err %v, want %q", err, want)
	}
}

func TestASmallSetLoads(t *testing.T) {
	if err := parse(t, valid()); err != nil {
		t.Fatal(err)
	}
}

func TestABadDigestIsRefused(t *testing.T) {
	for _, d := range []string{"abc", strings.ToUpper(sha), sha + "0", "", "Placeholder"} {
		s := valid()
		s.Files[MacOS][0].SHA256 = d
		refused(t, s, "sha256")
	}
}

func TestADuplicateMetricIDIsRefused(t *testing.T) {
	s := valid()
	s.Metrics[len(s.Metrics)-1].ID = s.Metrics[0].ID
	refused(t, s, "given twice")
}

func TestAMetricMissingOnOneSystemIsRefused(t *testing.T) {
	s := valid()
	s.Metrics[1].MacOS = nil
	refused(t, s, "missing on macos")
	s = valid()
	s.Metrics[1].Omarchy = nil
	refused(t, s, "missing on omarchy")
	s = valid()
	s.Metrics[len(Categories)-1].MacOS = &Side{}
	refused(t, s, "Omarchy only")
}

func TestAQuickListWithoutOneRowPerCategoryIsRefused(t *testing.T) {
	s := valid()
	s.Metrics[len(s.Metrics)-1].List = Quick
	refused(t, s, "the Quick list has 2 metrics in CPU")
	s = valid()
	s.Metrics[2].List = Full
	refused(t, s, "the Quick list has 0 metrics in Storage")
}

func TestOtherFaultsAreRefused(t *testing.T) {
	cases := map[string]func(s *Set){
		"names itself":  func(s *Set) { s.Name = "bench-8" },
		"category":      func(s *Set) { s.Metrics[0].Category = "Graphics" },
		"direction":     func(s *Set) { s.Metrics[0].Direction = "" },
		"tolerance":     func(s *Set) { s.Metrics[0].Tolerance = 0 },
		"list":          func(s *Set) { s.Metrics[0].List = "default" },
		"no version":    func(s *Set) { s.Metrics[0].Version = "" },
		"does not list": func(s *Set) { s.Metrics[0].MacOS.Files = []string{"nothing"} },
		"used by no metric": func(s *Set) {
			s.Files[MacOS] = append(s.Files[MacOS], File{ID: "spare", Version: "1", Kind: "file", SHA256: Placeholder})
		},
		"file \"prog\" is given": func(s *Set) { s.Files[MacOS] = append(s.Files[MacOS], s.Files[MacOS][0]) },
		"not a system":           func(s *Set) { s.Files["linux"] = nil },
		"pinned with no size":    func(s *Set) { s.Files[MacOS][0].Size = 0 },
		"pinned with no address": func(s *Set) { s.Files[MacOS][0].URL = "" },
		"placeholder with a size": func(s *Set) {
			s.Files[MacOS][0].SHA256 = Placeholder
		},
		"API differs": func(s *Set) { s.Metrics[0].APIDiffers = true },
	}
	for want, f := range cases {
		s := valid()
		f(s)
		refused(t, s, want)
	}
	if _, err := Parse("bench-7", []byte(`{"set":"bench-7","schema":1,"extra":1}`)); err == nil {
		t.Error("an unknown field was taken")
	}
	if _, err := Parse("bench-07", nil); err == nil {
		t.Error("a set named bench-07 was taken")
	}
}

func TestASetWithAPlaceholderIsNeverCurrent(t *testing.T) {
	pinned, held := valid(), valid()
	pinned.Name, pinned.N = "bench-1", 1
	held.Name, held.N = "bench-2", 2
	held.Files[MacOS][0].SHA256, held.Files[MacOS][0].Size = Placeholder, 0
	if got := held.Placeholders(); !slices.Equal(got, []string{"macos/prog"}) {
		t.Errorf("placeholders %v", got)
	}
	if err := parse(t, held); err != nil {
		t.Fatal(err)
	}
	if c, err := current([]*Set{pinned, held}); err != nil || c.Name != "bench-1" {
		t.Errorf("current %v %v, want bench-1", c, err)
	}
	if _, err := current([]*Set{held}); !errors.Is(err, ErrNoSet) {
		t.Errorf("a set with a placeholder was current: %v", err)
	}
	held.Files[MacOS][0].SHA256, held.Metrics[0].Version = sha, Placeholder
	if got := held.Placeholders(); !slices.Equal(got, []string{"cpu"}) {
		t.Errorf("a placeholder version: %v", got)
	}
}

func TestReadOrdersSetsByNumber(t *testing.T) {
	fsys := fstest.MapFS{}
	for _, n := range []string{"bench-10", "bench-2", "bench-1"} {
		s := valid()
		s.Name = n
		b, _ := json.Marshal(s)
		fsys[n+".json"] = &fstest.MapFile{Data: b}
	}
	all, err := Read(fsys)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range all {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"bench-1", "bench-2", "bench-10"}) {
		t.Errorf("order %v", names)
	}
}

func TestBench1HoldsWhatRuns(t *testing.T) {
	s, err := Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	var quick []string
	for _, m := range s.List(Quick) {
		quick = append(quick, m.ID)
	}
	want := []string{"coremark-single", "motionmark", "fio-seq-read", "stream-triad", "speedometer", "decode-h264-1080p",
		"neural-engine-mobilenet-v1", "power-idle", "boot-time"}
	if !slices.Equal(quick, want) {
		t.Errorf("Quick list %v, want %v", quick, want)
	}
	if n := len(s.List(Full)); n != 25 {
		t.Errorf("Full list has %d metrics", n)
	}
	for _, id := range []string{"decode-h264-1080p", "decode-hevc-4k", "llama-prompt-gpu", "llama-gen-gpu", "mlx-matmul-fp16", "neural-engine-mobilenet-v1"} {
		if m, _ := s.Metric(id); !m.APIDiffers {
			t.Errorf("%s is not marked API differs", id)
		}
	}
	if m, _ := s.Metric("clpeak-fp32"); m.APIDiffers || m.Omarchy.API != "Rusticl" || m.MacOS.API != "Apple's OpenCL" {
		t.Errorf("clpeak: %+v", m)
	}
	for _, id := range []string{"coremark", "stream", "clpeak"} {
		for _, sys := range Systems {
			if f, ok := s.File(sys, id); !ok || !f.LabBuilt || !f.Pinned() || f.Entry != "bench-"+id+"/bin/"+id {
				t.Errorf("%s/%s is not a pinned lab-built archive: %+v", sys, id, f)
			}
		}
	}
	for _, id := range []string{"fio", "smc-power", "mlx-bench"} {
		if f, ok := s.File(MacOS, id); !ok || !f.LabBuilt || !f.Pinned() || f.Entry != "bench-"+id+"/bin/"+id {
			t.Errorf("macos/%s is not a pinned lab-built archive: %+v", id, f)
		}
	}
	if got := s.Placeholders(); !slices.Equal(got, []string{"macos/ane-bench"}) {
		t.Errorf("bench-1 still holds the placeholders %v", got)
	}
	if c, err := Current(); err == nil && c.Name == "bench-1" {
		t.Error("bench-1, with its placeholders, is the current set")
	}
}

func TestFilesForTakesEachFileOnce(t *testing.T) {
	s, err := Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range s.FilesFor(Omarchy, s.List(Quick)) {
		ids = append(ids, f.ID)
	}
	if !slices.Equal(ids, []string{"coremark", "chrome", "motionmark", "fio", "stream", "speedometer", "clip-h264-1080p"}) {
		t.Errorf("Omarchy's Quick files %v", ids)
	}
}
