package macosrunner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

func TestAPersonalBootReportsItsBuildAndNothingElse(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	lab := newFakeLab(t, nil, nil)
	r, rebooted := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lab.polls) != 1 {
		t.Fatalf("polled %d times", len(lab.polls))
	}
	q := lab.polls[0]
	if q.Get("version") != "27.2.1" || q.Get("build") != "25C56" {
		t.Errorf("the boot reported %v", q)
	}
	if q.Get("boot") == "" || strings.Contains(q.Encode(), "6A1E1C9B") || q.Get("booted") != itoa(int(h.booted.Unix())) {
		t.Errorf("the boot is not told apart, or its session id is sent as it is: %v", q)
	}
	leftAlone(t, lab, h, b, *rebooted)
}

// leftAlone is a session nothing touched: no file fetched, no tool run, no
// setting changed, nothing uploaded, no reboot.
func leftAlone(t *testing.T, lab *fakeLab, h *fakeHost, b *bench, rebooted int) {
	t.Helper()
	if rebooted != 0 || lab.got != nil || len(lab.fetched) != 0 {
		t.Errorf("rebooted %d, uploaded %v, fetched %d files", rebooted, lab.got != nil, len(lab.fetched))
	}
	if ran := b.ran(); ran != "" {
		t.Errorf("tools ran:\n%s", ran)
	}
	if len(h.lpmSets) != 0 || len(h.brightSets) != 0 {
		t.Errorf("changed Low Power Mode %v, the backlight %v", h.lpmSets, h.brightSets)
	}
}

// A one-shot that never reached macOS leaves a started run behind in labd.
// The next macOS boot is a person's own, and the runner leaves it alone.
func TestAOneShotThatNeverStartedThenAPersonalBoot(t *testing.T) {
	cases := map[string]func(h *fakeHost, run *api.MacOSRun){
		"a boot hours after labd started the run": func(h *fakeHost, run *api.MacOSRun) {
			at := h.booted.Add(-3 * time.Hour)
			run.Started = &at
		},
		"a boot before labd started the run": func(h *fakeHost, run *api.MacOSRun) {
			at := h.booted.Add(10 * time.Minute)
			run.Started = &at
		},
		"a run labd never marked started": func(h *fakeHost, run *api.MacOSRun) {
			run.State, run.Started = api.MacOSQueued, nil
		},
		"a started run with no start time": func(h *fakeHost, run *api.MacOSRun) {
			run.Started = nil
		},
		"a boot that cannot be told apart": func(h *fakeHost, run *api.MacOSRun) {
			h.session = ""
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			h := newFakeHost()
			run := startedRun(h, "mos-1", api.TierDefault)
			change(h, run)
			lab := newFakeLab(t, run, b.files)
			r, rebooted := newRunner(t, lab, h, b)
			if err := r.Boot(context.Background()); err != nil {
				t.Fatal(err)
			}
			if q := lab.polls[0]; q.Get("version") != "27.2.1" || q.Get("build") != "25C56" {
				t.Errorf("the boot reported %v", q)
			}
			leftAlone(t, lab, h, b, *rebooted)
		})
	}
}

// A run's boot that ended before it uploaded (a crash, the power) leaves the
// run running in labd: a later boot, even within the window, is not its.
func TestARunTakenByAnEarlierBootIsNotMeasuredAgain(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	run := startedRun(h, "mos-1", api.TierDefault)
	lab := newFakeLab(t, run, b.files)
	r, rebooted := newRunner(t, lab, h, b)
	if err := r.take("mos-1", "an earlier boot"); err != nil {
		t.Fatal(err)
	}
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	leftAlone(t, lab, h, b, *rebooted)
}

// A run with no performance run has nowhere to upload: its boot measures
// nothing and goes back to Omarchy.
func TestARunWithNoPerformanceRunMeasuresNothingAndReboots(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	run := startedRun(h, "mos-1", api.TierDefault)
	run.PerfRun = 0
	lab := newFakeLab(t, run, b.files)
	r, rebooted := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *rebooted != 1 || lab.got != nil || len(lab.fetched) != 0 {
		t.Errorf("rebooted %d, uploaded %v, fetched %v", *rebooted, lab.got, lab.fetched)
	}
}

func TestTheRunsOwnBootRunsTheQuickListUploadsAndReboots(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	lab := newFakeLab(t, startedRun(h, "mos-1", api.TierDefault), b.files)
	r, rebooted := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *rebooted != 1 || lab.got == nil {
		t.Fatalf("rebooted %d, uploaded %v", *rebooted, lab.got)
	}
	up := lab.got
	if lab.upPath != "/api/macos/runner/performance/1/results" || up.State != Succeeded {
		t.Errorf("upload to %s: %s", lab.upPath, up.State)
	}
	var want []string
	for _, m := range b.set.List(sets.Quick) {
		if m.MacOS != nil {
			want = append(want, m.ID)
		}
	}
	var got []string
	for _, res := range up.Entries {
		got = append(got, res.Metric)
		if res.Set != "bench-1" {
			t.Errorf("%s of set %s", res.Metric, res.Set)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("measured %v, want the Quick list's macOS metrics %v", got, want)
	}
	m := byMetric(up.Entries)
	checks := map[string]float64{
		"coremark-single": 10000.5, "motionmark": 950.25, "fio-seq-read": 3000, "stream-triad": 121,
		"speedometer": 400.5, "decode-h264-1080p": 300, "neural-engine-mobilenet-v1": 2500.5, "power-idle": 7.25,
	}
	for id, v := range checks {
		if res := m[id]; val(res) != v || res.Reason != "" {
			t.Errorf("%s: %+v, want %v", id, res, v)
		}
	}
	if s := m["motionmark"]; s.Spread == nil || *s.Spread != 30 || s.Unit != "score" || !s.HigherIsBetter || s.Version != "1.3" || s.Category != "GPU" {
		t.Errorf("motionmark: %+v", s)
	}
	if f := m["fio-seq-read"]; f.Capacity == nil || *f.Capacity != 1001 || f.Fill == nil || *f.Fill != 42.2 {
		t.Errorf("Storage without its SSD's capacity in GB and the disk's fill: %+v", f)
	}
	for _, res := range up.Entries {
		if res.Category != "Storage" && (res.Capacity != nil || res.Fill != nil) {
			t.Errorf("%s carries Storage's capacity or fill", res.Metric)
		}
	}
	if d := m["decode-h264-1080p"]; d.API != "VideoToolbox" {
		t.Errorf("decode: %+v", d)
	}
	if p := m["power-idle"]; p.HigherIsBetter || p.Unit != "W" {
		t.Errorf("power: %+v", p)
	}
	for _, res := range up.Entries {
		if res.CoolDown != 30 {
			t.Errorf("%s: cool-down %v, want 30 s with no sensor", res.Metric, res.CoolDown)
		}
	}
	ran := b.ran()
	for _, w := range []string{"coremark M1 0x0 0x0 0x66 0 7 1 2000\n", "smc-power --seconds 60", "-hwaccel videotoolbox", "MobileNetV1.mlmodelc\n", "stream 50331648\n"} {
		if !strings.Contains(ran, w) {
			t.Errorf("no %q in what ran:\n%s", w, ran)
		}
	}
	for _, not := range []string{"7zz", "clpeak", "llama-bench", "mlx-bench", "--rw=randread", "M12"} {
		if strings.Contains(ran, not) {
			t.Errorf("%q ran in the Quick list", not)
		}
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "fio-scratch")); !os.IsNotExist(err) {
		t.Errorf("Storage's file was left behind: %v", err)
	}
	if c := up.Conditions; !c.OnAC || c.Backlight == nil || *c.Backlight != 50 || c.RefreshHz == nil || *c.RefreshHz != 60 ||
		c.FreePercent == nil || *c.FreePercent != 57.8 || c.CPUBusy != 4 || c.LowPowerMode {
		t.Errorf("conditions %+v", c)
	}
	if h.bright != 0.8 || !slices.Equal(h.brightSets, []float64{0.5, 0.8}) {
		t.Errorf("the backlight went %v", h.brightSets)
	}
	for _, f := range []string{"bench.json", "bench-conditions.json", "bench.log"} {
		if st, err := os.Stat(filepath.Join(r.Dir, f)); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, st, err)
		}
	}
	if log, _ := os.ReadFile(filepath.Join(r.Dir, "bench.log")); !strings.Contains(string(log), "CoreMark 1.0") {
		t.Error("the tools' own output is not in the private log")
	}
}

func TestTheFullListMeasuresEveryMacOSMetric(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
	r, _ := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	up := lab.got
	if up.State != Succeeded {
		t.Errorf("upload %s", up.State)
	}
	m := byMetric(up.Entries)
	if _, ok := m["boot-time"]; ok {
		t.Error("an Omarchy only metric ran on macOS")
	}
	want := map[string]float64{
		"coremark-single": 10000.5, "coremark-all": 120000.5, "7zip": 44515, "motionmark": 950.25,
		"clpeak-global-bandwidth": 350, "clpeak-fp32": 13500,
		"fio-seq-read": 3000, "fio-seq-write": 2000, "fio-rand-read-4k": 500, "fio-rand-write-4k": 300,
		"stream-triad": 121, "stream-copy": 110.001, "speedometer": 400.5, "jetstream": 260.75,
		"decode-h264-1080p": 300, "neural-engine-mobilenet-v1": 2500.5,
		"llama-prompt-gpu": 900.5, "llama-gen-gpu": 80.5, "llama-gen-cpu": 30.25, "mlx-matmul-fp16": 13.5,
		"power-idle": 7.25,
	}
	for id, v := range want {
		if res := m[id]; val(res) != v || res.Reason != "" {
			t.Errorf("%s: %+v, want %v", id, res, v)
		}
	}
	if j := m["jetstream"]; j.Spread != nil {
		t.Errorf("jetstream has a spread it does not give: %v", *j.Spread)
	}
	if l := m["llama-gen-gpu"]; l.Spread == nil || *l.Spread != 0.5 || l.API != "Metal" {
		t.Errorf("llama: %+v", l)
	}
	for _, id := range []string{"power-playback-1080p", "power-all-core"} {
		if p := m[id]; p.Value == nil || *p.Value <= 0 || p.Unit != "J" || p.Reason != "" {
			t.Errorf("%s: %+v", id, p)
		}
	}
	if hevc := m["decode-hevc-4k"]; hevc.Value != nil || hevc.Reason != PlaceholderFile {
		t.Errorf("a metric with a placeholder file: %+v", hevc)
	}
	ran := b.ran()
	for _, w := range []string{
		"coremark M12 0x0 0x0 0x66 0 7 1 2000\n", "coremark M12 0x0 0x0 0x66 333334 7 1 2000\n", "7zz b", "clpeak --opencl --global-memory-bandwidth --single-precision-compute",
		"--size=8g --rw=write --bs=1m --direct=1 --ioengine=psync --iodepth=1 --refill_buffers --end_fsync=1",
		"--direct=1 --ioengine=psync --output-format=json --rw=randread --bs=4k --numjobs=32 --iodepth=1 --size=256m --offset_increment=256m --time_based --runtime=30 --group_reporting",
		"-p 512 -n 128 -ngl 999", "-p 0 -n 128 -ngl 0", "-re -hwaccel videotoolbox",
	} {
		if !strings.Contains(ran, w) {
			t.Errorf("no %q in what ran:\n%s", w, ran)
		}
	}
	if n := strings.Count(ran, "--name=fill"); n != 1 {
		t.Errorf("Storage's file was filled %d times", n)
	}
}

func TestARunThatNamesMetricsMeasuresThemAlone(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	run := startedRun(h, "mos-3", api.TierExtended)
	run.Metrics = []string{"clpeak-fp32", "7zip"}
	lab := newFakeLab(t, run, b.files)
	r, _ := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := byMetric(lab.got.Entries)
	if len(lab.got.Entries) != 2 || val(m["clpeak-fp32"]) != 13500 || val(m["7zip"]) != 44515 {
		t.Errorf("uploaded %+v", lab.got.Entries)
	}
	if ran := b.ran(); strings.Contains(ran, "coremark") || strings.Contains(ran, "--rw=") {
		t.Errorf("ran more than was asked:\n%s", ran)
	}
}

func TestBrowsersRunHeadlessOnTheGPUInAFreshProfile(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	lab := newFakeLab(t, startedRun(h, "mos-1", api.TierDefault), b.files)
	r, _ := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(b.log + ".chrome-args")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"--headless", "--enable-gpu", "--window-size=1920,1080", "--screen-info={0,0 3840x2160 devicePixelRatio=2}", "--remote-debugging-port=0"} {
		if !strings.Contains(string(args), f+"\n") {
			t.Errorf("chrome ran without %s: %s", f, args)
		}
	}
	if !strings.Contains(string(args), "--user-data-dir="+r.ShareDir) {
		t.Errorf("the profile is not a fresh one: %s", args)
	}
	if !strings.Contains(string(args), "/?startAutomatically&iterationCount=10\n") {
		t.Errorf("speedometer's page: %s", args)
	}
	pages, _ := os.ReadFile(b.log + ".pages")
	if !strings.Contains(string(pages), "MotionMark 1.3") || !strings.Contains(string(pages), "Speedometer 3.1") {
		t.Errorf("the pages served: %q", pages)
	}
	if left, _ := os.ReadDir(r.ShareDir); len(left) != 0 {
		t.Errorf("the share folder was left behind: %v", left)
	}
}

func TestABrowserNotDrawnOnTheGPUAt60HzDoesNotCount(t *testing.T) {
	for _, c := range []struct {
		renderer string
		hz       float64
		want     Reason
	}{
		{"Google SwiftShader", 60, SoftwareRenderer},
		{"", 60, SoftwareRenderer},
		{"ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Max)", 30, FellBack},
		{"ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Max)", 120, FellBack},
	} {
		b := newBench(t)
		b.dt.renderer, b.dt.hz = c.renderer, c.hz
		h := newFakeHost()
		lab := newFakeLab(t, startedRun(h, "mos-1", api.TierDefault), b.files)
		r, _ := newRunner(t, lab, h, b)
		r.Boot(context.Background())
		m := byMetric(lab.got.Entries)
		for _, id := range []string{"motionmark", "speedometer"} {
			if res := m[id]; res.Value != nil || res.Reason != c.want {
				t.Errorf("%q at %v Hz: %s %+v, want %s", c.renderer, c.hz, id, res, c.want)
			}
		}
		want := Succeeded
		if c.want == SoftwareRenderer {
			want = Failed
		}
		if lab.got.State != want {
			t.Errorf("%q at %v Hz: the run %s, want %s", c.renderer, c.hz, lab.got.State, want)
		}
	}
}

func TestWorkThatDidNotRunWhereItSaysFellBack(t *testing.T) {
	for env, ids := range map[string][]string{
		"FAKE_CL_DEVICE=Apple CPU": {"clpeak-global-bandwidth", "clpeak-fp32"},
		"FAKE_FFMPEG_SW=1":         {"decode-h264-1080p", "power-playback-1080p"},
		"FAKE_ANE_PLACEMENT=cpu":   {"neural-engine-mobilenet-v1"},
		"FAKE_LLAMA_OFFLOADED=20":  {"llama-prompt-gpu", "llama-gen-gpu"},
		"FAKE_MLX_NOGPU=1":         {"mlx-matmul-fp16"},
		"FAKE_FIO_DIRECT=0":        {"fio-seq-read", "fio-seq-write", "fio-rand-read-4k", "fio-rand-write-4k"},
	} {
		t.Run(env, func(t *testing.T) {
			k, v, _ := strings.Cut(env, "=")
			t.Setenv(k, v)
			b := newBench(t)
			h := newFakeHost()
			lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
			r, _ := newRunner(t, lab, h, b)
			r.Boot(context.Background())
			m := byMetric(lab.got.Entries)
			for _, id := range ids {
				if res := m[id]; res.Value != nil || res.Reason != FellBack {
					t.Errorf("%s: %+v", id, res)
				}
			}
			if res := m["coremark-single"]; res.Value == nil {
				t.Errorf("the rest did not run: %+v", res)
			}
		})
	}
	t.Run("no OpenCL platform", func(t *testing.T) {
		t.Setenv("FAKE_CL_PLATFORM", "")
		b := newBench(t)
		b.files["clpeak"] = tarGz(map[string]string{"clpeak": "#!/bin/sh\necho 'no platforms found'\n"})
		h := newFakeHost()
		for i, f := range b.set.Files[sets.MacOS] {
			if f.ID == "clpeak" {
				b.set.Files[sets.MacOS][i].SHA256 = sum(b.files["clpeak"])
			}
		}
		lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
		r, _ := newRunner(t, lab, h, b)
		r.Boot(context.Background())
		if res := byMetric(lab.got.Entries)["clpeak-fp32"]; res.Reason != NoDriver {
			t.Errorf("clpeak with no OpenCL: %+v", res)
		}
	})
}

func TestPowerNeedsPSTROnACWithTheBatteryNotCharging(t *testing.T) {
	power := []string{"power-idle", "power-playback-1080p", "power-all-core"}
	t.Run("no PSTR", func(t *testing.T) {
		t.Setenv("FAKE_SMC_ABSENT", "1")
		b := newBench(t)
		h := newFakeHost()
		lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
		r, _ := newRunner(t, lab, h, b)
		r.Boot(context.Background())
		m := byMetric(lab.got.Entries)
		for _, id := range power {
			if res := m[id]; res.Value != nil || res.Reason != NoComparableSensor {
				t.Errorf("%s: %+v", id, res)
			}
		}
		if lab.got.State != Succeeded {
			t.Errorf("no sensor failed the run: %s", lab.got.State)
		}
	})
	t.Run("charging", func(t *testing.T) {
		b := newBench(t)
		h := newFakeHost()
		h.power.Charging = true
		lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
		r, _ := newRunner(t, lab, h, b)
		r.Boot(context.Background())
		m := byMetric(lab.got.Entries)
		for _, id := range power {
			if res := m[id]; res.Reason != BatteryCharging {
				t.Errorf("%s: %+v", id, res)
			}
		}
		if res := m["coremark-single"]; res.Value == nil {
			t.Errorf("a charging battery stopped the rest: %+v", res)
		}
		if strings.Contains(b.ran(), "smc-power") {
			t.Error("smc-power ran while the battery charged")
		}
	})
}

func TestARunOffACOrBusyIsRefusedAndMeasuresNothing(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*fakeHost)
		want   Reason
	}{
		"on battery": {func(h *fakeHost) { h.power.OnAC = false }, NotOnAC},
		"busy":       {func(h *fakeHost) { h.busy = 60 }, NotIdle},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBench(t)
			h := newFakeHost()
			h.lpm = true
			c.change(h)
			lab := newFakeLab(t, startedRun(h, "mos-1", api.TierDefault), b.files)
			r, rebooted := newRunner(t, lab, h, b)
			r.Boot(context.Background())
			if *rebooted != 1 || lab.got == nil {
				t.Fatalf("rebooted %d, uploaded %v", *rebooted, lab.got != nil)
			}
			if lab.got.State != Failed || len(lab.got.Entries) == 0 {
				t.Errorf("upload %s with %d results", lab.got.State, len(lab.got.Entries))
			}
			for _, res := range lab.got.Entries {
				if res.Value != nil || res.Reason != c.want {
					t.Errorf("%s: %+v", res.Metric, res)
				}
			}
			if ran := b.ran(); ran != "" {
				t.Errorf("tools ran:\n%s", ran)
			}
			if len(h.lpmSets) != 0 || len(h.brightSets) != 0 {
				t.Errorf("a refused run changed Low Power Mode %v, the backlight %v", h.lpmSets, h.brightSets)
			}
		})
	}
}

func TestLowPowerModeIsTurnedOffAndPutBack(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	h.lpm = true
	lab := newFakeLab(t, startedRun(h, "mos-1", api.TierDefault), b.files)
	r, _ := newRunner(t, lab, h, b)
	r.Boot(context.Background())
	if !slices.Equal(h.lpmSets, []bool{false, true}) || !h.lpm {
		t.Errorf("Low Power Mode went %v", h.lpmSets)
	}
	if c := lab.got.Conditions; c.LowPowerMode {
		t.Errorf("measured with Low Power Mode on: %+v", c)
	}
}

func TestTheCoolDownWaitsForTheHottestSensor(t *testing.T) {
	h := newFakeHost()
	s := &session{r: &Runner{Host: h}}
	h.temps = []float64{70, 62, 55, 49}
	if got := s.coolDown(context.Background()); got != 15 {
		t.Errorf("cooled %v s, want 15", got)
	}
	h.temps = []float64{90}
	if got := s.coolDown(context.Background()); got != 60 {
		t.Errorf("cooled %v s, want at most 60", got)
	}
	h.temps = nil
	if got := s.coolDown(context.Background()); got != 30 {
		t.Errorf("cooled %v s with no sensor, want 30", got)
	}
}

func TestStorageKeepsAFifthOfTheDiskFree(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	h.total, h.free = 256<<30, 58<<30 // 50 GiB left after the file, under a fifth
	lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
	r, _ := newRunner(t, lab, h, b)
	r.Boot(context.Background())
	m := byMetric(lab.got.Entries)
	for _, id := range []string{"fio-seq-read", "fio-seq-write", "fio-rand-read-4k", "fio-rand-write-4k"} {
		if res := m[id]; res.Value != nil || res.Reason != DiskTooFull {
			t.Errorf("%s: %+v", id, res)
		}
	}
	if strings.Contains(b.ran(), "fio ") {
		t.Error("fio ran on a disk too full")
	}
	if res := m["fio-seq-read"]; res.Capacity == nil || res.Fill == nil || *res.Fill != 80.5 {
		t.Errorf("a full disk's row without its capacity and fill: %+v", res)
	}
	if lab.got.State != Succeeded {
		t.Errorf("a full disk failed the run: %s", lab.got.State)
	}
}

func TestACancelledRunRemovesStoragesFileAndPutsEverythingBack(t *testing.T) {
	t.Setenv("FAKE_FIO_SLEEP", "20")
	b := newBench(t)
	h := newFakeHost()
	h.lpm = true
	run := startedRun(h, "mos-2", api.TierExtended)
	lab := newFakeLab(t, run, b.files)
	r, _ := newRunner(t, lab, h, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !strings.Contains(b.ran(), "--rw=randread") {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	up, _ := r.Run(ctx, run)
	if time.Since(start) > 15*time.Second {
		t.Errorf("fio was not stopped: %v", time.Since(start))
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "fio-scratch")); !os.IsNotExist(err) {
		t.Errorf("Storage's file was left behind: %v", err)
	}
	if !h.lpm || h.bright != 0.8 || !up.Conditions.Restored || !slices.Equal(h.lpmSets, []bool{false, true}) {
		t.Errorf("not put back: Low Power Mode %v, backlight %v, %+v", h.lpm, h.bright, up.Conditions)
	}
	m := byMetric(up.Entries)
	for _, id := range []string{"fio-rand-read-4k", "stream-triad", "power-all-core"} {
		if res := m[id]; res.Reason != NotRun {
			t.Errorf("%s: %+v", id, res)
		}
	}
	if res := m["fio-seq-read"]; res.Value == nil {
		t.Errorf("what ran before is lost: %+v", res)
	}
}

// The results carry only the allowlisted fields and fixed reason codes,
// every string the set's: the tools' text (a user name, a path) never.
func TestResultsCarryNoFreeText(t *testing.T) {
	t.Setenv("FAKE_ANE_PLACEMENT", "cpu")
	b := newBench(t)
	h := newFakeHost()
	run := startedRun(h, "mos-2", api.TierExtended)
	run.PerfRun = 7
	lab := newFakeLab(t, run, b.files)
	r, _ := newRunner(t, lab, h, b)
	r.Boot(context.Background())
	if lab.upPath != "/api/macos/runner/performance/7/results" {
		t.Errorf("uploaded to %s", lab.upPath)
	}
	if strings.Contains(strings.ToLower(string(lab.raw)), "alice") || strings.Contains(string(lab.raw), "/Users") {
		t.Errorf("the upload carries a tool's text: %s", lab.raw)
	}
	var up map[string]json.RawMessage
	json.Unmarshal(lab.raw, &up)
	for k := range up {
		if !slices.Contains([]string{"entries", "conditions", "state"}, k) {
			t.Errorf("the upload carries %q", k)
		}
	}
	var conditions map[string]any
	json.Unmarshal(up["conditions"], &conditions)
	for k := range conditions {
		if !slices.Contains(ConditionFields, k) {
			t.Errorf("the conditions carry %q", k)
		}
	}
	var results []map[string]any
	json.Unmarshal(up["entries"], &results)
	if len(results) == 0 {
		t.Fatal("no results")
	}
	for _, res := range results {
		for k, v := range res {
			if !slices.Contains(ResultFields, k) {
				t.Errorf("a result carries %q", k)
			}
			if k == "reason" && v != nil && !Reason(v.(string)).Known() {
				t.Errorf("reason %q is not a fixed code", v)
			}
		}
		if _, ok := res["reason"]; !ok {
			t.Errorf("a result has no reason field, null or a code: %v", res)
		}
		m, ok := b.set.Metric(res["metric"].(string))
		if !ok || res["version"] != m.Version || res["unit"] != m.Unit || res["category"] != m.Category || res["set"] != "bench-1" {
			t.Errorf("a result's strings are not the set's: %v", res)
		}
		if api, ok := res["api"]; ok && api != m.MacOS.API {
			t.Errorf("api %v, the set's %q", api, m.MacOS.API)
		}
	}
	var log []byte
	log, _ = os.ReadFile(filepath.Join(r.Dir, "bench.log"))
	if !strings.Contains(string(log), "alice") {
		t.Error("the tools' text is not kept in the private log")
	}
}

func TestAToolThatFailsIsAnErrorAndTheRunGoesOn(t *testing.T) {
	b := newBench(t)
	b.files["7zip"] = tarGz(map[string]string{"7zz": "#!/bin/sh\necho 'cannot open /Users/alice' >&2\nexit 2\n"})
	for i, f := range b.set.Files[sets.MacOS] {
		if f.ID == "7zip" {
			b.set.Files[sets.MacOS][i].SHA256 = sum(b.files["7zip"])
		}
	}
	h := newFakeHost()
	lab := newFakeLab(t, startedRun(h, "mos-2", api.TierExtended), b.files)
	r, rebooted := newRunner(t, lab, h, b)
	r.Boot(context.Background())
	m := byMetric(lab.got.Entries)
	if res := m["7zip"]; res.Value != nil || res.Reason != Error {
		t.Errorf("7zip: %+v", res)
	}
	if res := m["jetstream"]; res.Value == nil {
		t.Errorf("the run stopped: %+v", res)
	}
	if lab.got.State != Failed || *rebooted != 1 {
		t.Errorf("an error did not fail the run: %s, rebooted %d", lab.got.State, *rebooted)
	}
}

// A file that is not what labd pinned is not used, and the Mac still goes
// back to Omarchy.
func TestAFileNotMatchingItsSHA256IsRefused(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	run := startedRun(h, "mos-1", api.TierDefault)
	lab := newFakeLab(t, run, b.files)
	lab.files[run.Artifacts[0].SHA256] = []byte("changed")
	r, rebooted := newRunner(t, lab, h, b)
	if err := r.Boot(context.Background()); err == nil {
		t.Error("ran with a changed file")
	}
	if lab.got == nil || lab.got.State != Failed || strings.Contains(string(lab.raw), "sha256") {
		t.Fatalf("uploaded %s", lab.raw)
	}
	for _, res := range lab.got.Entries {
		if res.Reason != Error {
			t.Errorf("%s: %+v", res.Metric, res)
		}
	}
	if *rebooted != 1 || b.ran() != "" {
		t.Errorf("rebooted %d, ran %q", *rebooted, b.ran())
	}
}

// A file labd hands that is not the set's is never run either.
func TestAFileThatIsNotTheSetsIsAnError(t *testing.T) {
	b := newBench(t)
	h := newFakeHost()
	b.files["stream"] = tarGz(map[string]string{"stream": "#!/bin/sh\necho other\n"})
	lab := newFakeLab(t, startedRun(h, "mos-1", api.TierDefault), b.files)
	r, _ := newRunner(t, lab, h, b)
	r.Boot(context.Background())
	if res := byMetric(lab.got.Entries)["stream-triad"]; res.Reason != Error {
		t.Errorf("stream-triad: %+v", res)
	}
	if strings.Contains(b.ran(), "other") || strings.Contains(b.ran(), "stream ") {
		t.Error("a file not the set's ran")
	}
}

func TestEnrollKeepsTheSecretPrivate(t *testing.T) {
	lab := newFakeLab(t, nil, nil)
	path := filepath.Join(t.TempDir(), "etc", "macos-runner.json")
	h := newFakeHost()
	if err := Enroll(context.Background(), lab.srv.URL, "mos_enr_once", "maralc", path, h); err != nil {
		t.Fatal(err)
	}
	// The build is told at enrollment, as at every boot, and nothing else.
	if q := lab.enrolQuery; len(q) != 2 || q.Get("version") != h.version || q.Get("build") != h.build {
		t.Errorf("enrolled telling labd %v", q)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	c, err := LoadConfig(path)
	if err != nil || c.Secret != "mos_secret" || c.Mac != "m2-max" || c.GUIUser != "maralc" || c.Server != lab.srv.URL {
		t.Errorf("%+v %v", c, err)
	}
	if err := Enroll(context.Background(), lab.srv.URL, "mos_enr_once", "", path, h); err == nil {
		t.Error("enrolled twice with one token")
	}
}

func TestMacOSOwnWordsAreRead(t *testing.T) {
	if v, err := parse7zip("junk\nTot:             977   4554  44515\n"); err != nil || v != 44515 {
		t.Errorf("%v %v", v, err)
	}
	if p := parseBatt("Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t80%; charging; 1:00 remaining present: true\n"); !p.OnAC || !p.Battery || !p.Charging {
		t.Errorf("charging: %+v", p)
	}
	if p := parseBatt("Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t80%; AC attached; not charging present: true\n"); !p.OnAC || p.Charging {
		t.Errorf("not charging: %+v", p)
	}
	if p := parseBatt("Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging; 4:00 remaining present: true\n"); p.OnAC {
		t.Errorf("on battery: %+v", p)
	}
	if p := parseBatt("Now drawing from 'AC Power'\n"); !p.OnAC || p.Battery {
		t.Errorf("a Mac with no battery: %+v", p)
	}
	if b, err := parseTopBusy("CPU usage: 50.0% user, 20.0% sys, 30.0% idle\nCPU usage: 3.5% user, 2.5% sys, 94.0% idle\n"); err != nil || b != 6 {
		t.Errorf("busy %v %v", b, err)
	}
	custom := "Battery Power:\n lowpowermode         1\n sleep 1\nAC Power:\n lowpowermode         0\n sleep 10\n"
	if parseLowPowerMode(custom) {
		t.Error("read the battery's Low Power Mode for AC")
	}
	if !parseLowPowerMode("AC Power:\n lowpowermode         1\nBattery Power:\n lowpowermode 0\n") || !parseLowPowerMode(" lowpowermode 1\n") {
		t.Error("Low Power Mode on, not read")
	}
	if v, b, err := checkSWVers("27.2.1\n", "25C56\n"); err != nil || v != "27.2.1" || b != "25C56" {
		t.Errorf("%q %q %v", v, b, err)
	}
	if _, _, err := checkSWVers("ProductName: macOS /Users/alice", "25C56"); err == nil {
		t.Error("sw_vers text other than a version is reported")
	}
	if rates := parseStream("Copy:   110000.5  0.1\nTriad:   121000.4 0.1\n"); rates["copy"] != 110000.5 || rates["triad"] != 121000.4 {
		t.Errorf("stream %v", rates)
	}
}
