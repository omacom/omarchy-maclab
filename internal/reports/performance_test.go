package reports

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func perfKey(c byte) string { return strings.Repeat(string(c), 32) }

var perfAt = time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)

func perfFixture() Performance {
	v, reason := 1234.5, "no_build"
	capacity := int64(512)
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	return Performance{
		Macs:     []PerfMac{{Key: perfKey('a'), Kind: PerfLabOwned, Board: "j314s", CPU: 10, GPU: 16}, {Key: perfKey('b'), Kind: PerfVolunteer, Board: "j313", CPU: 8, GPU: 7}},
		Releases: []PerfRelease{{Key: perfKey('c'), Kernel: "6.18.2-1", Verdict: PerfNoVerdictYet, Position: 1}},
		Values: []PerfValue{
			{Mac: perfKey('a'), Set: "bench-1", Side: "omarchy", Release: perfKey('c'), Metric: "fio-seq-read", Value: &v, Unit: "MB/s", Capacity: &capacity,
				Booted: map[string]string{"linux-asahi": "6.18.2-1"}, Measured: at},
			{Mac: perfKey('a'), Set: "bench-1", Side: "macos", MacOSVersion: "27.2.1", MacOSBuild: "27C101", Metric: "fio-seq-read", Value: &v, Unit: "MB/s", Capacity: &capacity, Measured: at},
			{Mac: perfKey('b'), Set: "bench-1", Side: "omarchy", Release: perfKey('c'), Metric: "mlx-matmul-fp16", Reason: &reason, Unit: "TFLOPS", API: "Metal", Measured: at},
		},
	}
}

func TestPerformanceIsExactlyWhatLabdLastHandedOver(t *testing.T) {
	s, _ := openStore(t)
	if p, err := s.Performance(); err != nil || len(p.Macs)+len(p.Releases)+len(p.Values) != 0 {
		t.Fatalf("a new reports.db: %+v %v", p, err)
	}
	p := perfFixture()
	if changed, err := s.PublishPerformance(p, perfAt); err != nil || !changed {
		t.Fatalf("the first copy: %v %v", changed, err)
	}
	got, err := s.Performance()
	if err != nil {
		t.Fatal(err)
	}
	want := perfFixture()
	want.sort()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("read back:\n%+v\nwant\n%+v", got, want)
	}
	if changed, err := s.PublishPerformance(perfFixture(), perfAt); err != nil || changed {
		t.Fatalf("the same again changed it: %v %v", changed, err)
	}

	// A value superseded, a Mac's kind changed, a Mac gone: so here too,
	// and what stays keeps its row and when it first came.
	rows := func() string {
		return strings.Join(s.list(t, `SELECT mac_key || ' ' || id || ' ' || created_at || ' ' || kind FROM perf_macs ORDER BY mac_key`), "; ")
	}
	before := rows()
	p = perfFixture()
	newer := 1300.0
	p.Macs, p.Values = p.Macs[:1], p.Values[:2]
	p.Macs[0].Kind, p.Values[0].Value = PerfCounted, &newer
	if changed, err := s.PublishPerformance(p, perfAt.Add(time.Hour)); err != nil || !changed {
		t.Fatalf("a Mac gone: %v %v", changed, err)
	}
	if got, _ := s.Performance(); len(got.Macs) != 1 || len(got.Values) != 2 || *got.Values[1].Value != newer || got.Macs[0].Kind != PerfCounted {
		t.Fatalf("after a Mac went: %+v", got)
	}
	if after, want := rows(), strings.Replace(strings.Split(before, "; ")[0], "lab_owned", "counted", 1); after != want {
		t.Fatalf("the Mac that stayed: %s, want %s", after, want)
	}
	if _, err := s.PublishPerformance(Performance{}, perfAt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Performance(); len(got.Macs)+len(got.Releases)+len(got.Values) != 0 {
		t.Fatalf("nothing published: %+v", got)
	}
}

func TestPerformanceRefusesWhatIsNotPublished(t *testing.T) {
	s, _ := openStore(t)
	if _, err := s.PublishPerformance(perfFixture(), perfAt); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Performance){
		"a Mac by its name":                   func(p *Performance) { p.Macs[0].Key = "lab-mac-alpha"; p.Values[0].Mac = "lab-mac-alpha" },
		"a Release not qualified":             func(p *Performance) { p.Releases[0].Verdict = "not qualified" },
		"a Release by its name":               func(p *Performance) { p.Releases[0].Key = "2026.11"; p.Values[0].Release = "2026.11" },
		"a value of a Release not here":       func(p *Performance) { p.Values[0].Release = perfKey('d') },
		"a value of a Mac not here":           func(p *Performance) { p.Values[0].Mac = perfKey('e') },
		"a reason in a tool's words":          func(p *Performance) { r := "chrome: /home/bob"; p.Values[2].Reason = &r },
		"a value and a reason":                func(p *Performance) { p.Values[2].Value = p.Values[0].Value },
		"a macOS value with a Release":        func(p *Performance) { p.Values[1].Release = perfKey('c') },
		"an Omarchy value with a macOS build": func(p *Performance) { p.Values[0].MacOSBuild = "27C101" },
		"an unknown kind":                     func(p *Performance) { p.Macs[1].Kind = "m_team" },
		"an unknown Configuration":            func(p *Performance) { p.Macs[1].GPU = 0 },
		"a booted name that is not a package": func(p *Performance) { p.Values[0].Booted = map[string]string{"/home/bob": "1"} },
	} {
		p := perfFixture()
		change(&p)
		if _, err := s.PublishPerformance(p, perfAt); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	got, _ := s.Performance()
	want := perfFixture()
	want.sort()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a refused copy changed reports.db: %+v", got)
	}
}
