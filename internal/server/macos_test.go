package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// macosBench makes the macOS runs' Benchmark set one with a single macOS
// file, in the artifact store, and a placeholder beside it.
func macosBench(t *testing.T, l *lab) string {
	t.Helper()
	sha := l.artifact("web benchmarks bundle")
	set := &sets.Set{Name: "bench-9", N: 9,
		Metrics: []sets.Metric{
			{ID: "web", List: sets.Quick, MacOS: &sets.Side{Files: []string{"web-benchmarks"}}},
			{ID: "later", List: sets.Full, MacOS: &sets.Side{Files: []string{"web-benchmarks", "not-built"}}},
		},
		Files: map[string][]sets.File{sets.MacOS: {
			{ID: "web-benchmarks", Version: "1", Kind: "tarball", SHA256: sha},
			{ID: "not-built", Version: "1", Kind: "tarball", SHA256: sets.Placeholder},
		}}}
	old := macOSBenchSet
	macOSBenchSet = func() (*sets.Set, error) { return set, nil }
	t.Cleanup(func() { macOSBenchSet = old })
	return sha
}

func TestAMacOSRunIsRefusedWhenItsSetsFilesAreNotMirrored(t *testing.T) {
	l := newLab(t, false)
	if _, err := l.s.queueMacOSRun(l.name, api.TierDefault); err == nil || !strings.Contains(err.Error(), "lab bench mirror") {
		t.Fatalf("err %v", err)
	}
}

func decodeInto[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return v
}

// enrollRunner makes a one-time token for the Mac's runner and enrolls with
// it, returning the runner's secret.
func enrollRunner(t *testing.T, l *lab) string {
	t.Helper()
	resp, body := l.as(harnessToken, "POST", "/api/macos/enroll-tokens", map[string]string{"mac": l.name})
	if resp.StatusCode != 200 {
		t.Fatalf("enroll token: %d %s", resp.StatusCode, body)
	}
	tok := decodeInto[api.MacOSEnrollToken](t, body)
	if !strings.HasPrefix(tok.Token, "mos_enr_") || !strings.Contains(tok.Line, "lab-macos-runner enroll") || !strings.Contains(tok.Line, tok.Token) {
		t.Fatalf("enroll token: %+v", tok)
	}
	resp, body = l.as(tok.Token, "POST", "/api/macos/runner/enroll", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("runner enroll: %d %s", resp.StatusCode, body)
	}
	if resp, _ := l.as(tok.Token, "POST", "/api/macos/runner/enroll", nil); resp.StatusCode != 401 {
		t.Errorf("an enrolment token worked twice: %d", resp.StatusCode)
	}
	e := decodeInto[api.MacOSEnrolled](t, body)
	if e.Mac != l.name || !strings.HasPrefix(e.Secret, "mos_") {
		t.Fatalf("enrolled: %+v", e)
	}
	return e.Secret
}

func TestAMacOSRunGoesFromQueuedToItsResults(t *testing.T) {
	l := newLab(t, false)
	sha := macosBench(t, l)
	secret := enrollRunner(t, l)

	// Nothing queued: the runner hears so, and boot-macos-once is refused
	// before anything changes on the Mac.
	if _, body := l.as(secret, "GET", "/api/macos/runner/poll", nil); decodeInto[api.MacOSPoll](t, body).Run != nil {
		t.Fatalf("a run with none queued: %s", body)
	}
	if resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil); resp.StatusCode != 409 {
		t.Fatalf("started with none queued: %d %s", resp.StatusCode, body)
	}

	run, err := l.s.queueMacOSRun(l.name, api.TierExtended)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != api.MacOSQueued || len(run.Artifacts) != 1 || run.Artifacts[0].SHA256 != sha {
		t.Fatalf("queued: %+v", run)
	}
	if _, err := l.s.queueMacOSRun(l.name, api.TierDefault); err == nil {
		t.Error("a second run queued over the first")
	}
	// Queued but not started: the runner waits for the boot.
	if _, body := l.as(secret, "GET", "/api/macos/runner/poll", nil); decodeInto[api.MacOSPoll](t, body).Run != nil {
		t.Fatalf("a run before boot-macos-once: %s", body)
	}
	resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil)
	if resp.StatusCode != 200 || decodeInto[api.MacOSRun](t, body).State != api.MacOSStarted {
		t.Fatalf("started: %d %s", resp.StatusCode, body)
	}

	_, body = l.as(secret, "GET", "/api/macos/runner/poll", nil)
	got := decodeInto[api.MacOSPoll](t, body).Run
	if got == nil || got.ID != run.ID || got.Tier != api.TierExtended || got.State != api.MacOSRunning {
		t.Fatalf("poll: %s", body)
	}
	if resp, body := l.as(secret, "GET", "/api/macos/runner/artifacts/"+sha, nil); resp.StatusCode != 200 || body != "web benchmarks bundle" {
		t.Errorf("artifact: %d %q", resp.StatusCode, body)
	}
	other := l.artifact("not in the run")
	if resp, _ := l.as(secret, "GET", "/api/macos/runner/artifacts/"+other, nil); resp.StatusCode != 404 {
		t.Errorf("an artifact outside the run: %d", resp.StatusCode)
	}
	if resp, _ := l.as("mos_wrong", "GET", "/api/macos/runner/poll", nil); resp.StatusCode != 401 {
		t.Errorf("a wrong runner secret: %d", resp.StatusCode)
	}

	v := 412.5
	up := api.MacOSResults{RunID: run.ID, Results: []api.BenchResult{
		{Name: "speedometer/chrome", Version: "3.1", Value: &v, Unit: "score", HigherIsBetter: true, Spread: 3, Seconds: 120},
		{Name: "kernel-compile", Note: api.NoMacOSEquivalent},
	}}
	if resp, body := l.as(secret, "POST", "/api/macos/runner/results", up); resp.StatusCode != 200 {
		t.Fatalf("results: %d %s", resp.StatusCode, body)
	}
	_, body = l.get("GET", "/api/devices/"+l.name+"/macos-runs")
	done := decodeInto[api.MacOSRun](t, body)
	if done.State != api.MacOSDone || len(done.Results) != 2 || *done.Results[0].Value != v || done.Results[1].Value != nil {
		t.Fatalf("done: %s", body)
	}
	if resp, _ := l.as(secret, "POST", "/api/macos/runner/results", up); resp.StatusCode != 409 {
		t.Errorf("results taken twice: %d", resp.StatusCode)
	}
	// Done, the Mac takes a new run.
	if resp, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos-runs", api.MacOSRunRequest{}); resp.StatusCode != 200 {
		t.Errorf("a new run after one done: %d %s", resp.StatusCode, body)
	}
}

func TestAMacOSRunIsQueuedByAMaintainerOnly(t *testing.T) {
	l := newLab(t, false)
	macosBench(t, l)
	if resp, body := l.as("", "POST", "/api/devices/"+l.name+"/macos-runs", api.MacOSRunRequest{Tier: "default"}); resp.StatusCode == 200 {
		t.Fatalf("queued with no credential: %s", body)
	}
	if resp, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos-runs", api.MacOSRunRequest{Tier: "fast"}); resp.StatusCode != 400 {
		t.Errorf("an unknown tier: %d %s", resp.StatusCode, body)
	}
	if resp, body := l.as(harnessToken, "POST", "/api/devices/nobody/macos-runs", api.MacOSRunRequest{}); resp.StatusCode != 404 {
		t.Errorf("an unknown Mac: %d %s", resp.StatusCode, body)
	}
	resp, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos-runs", api.MacOSRunRequest{})
	if resp.StatusCode != 200 || decodeInto[api.MacOSRun](t, body).Tier != api.TierDefault {
		t.Fatalf("queue: %d %s", resp.StatusCode, body)
	}
	if resp, _ := l.as(harnessToken, "POST", "/api/macos/enroll-tokens", map[string]string{"mac": "nobody"}); resp.StatusCode != 404 {
		t.Errorf("a token for an unknown Mac: %d", resp.StatusCode)
	}
}

// A runner that does not report within the deadline (a FileVault Mac waits
// for its password at the screen) leaves the Mac Needs hands.
func TestAMacOSRunPastItsDeadlineNeedsHands(t *testing.T) {
	was := macosDeadline
	macosDeadline = func(string) time.Duration { return 300 * time.Millisecond }
	t.Cleanup(func() { macosDeadline = was })
	l := newLab(t, false)
	macosBench(t, l)
	if _, err := l.s.queueMacOSRun(l.name, api.TierDefault); err != nil {
		t.Fatal(err)
	}
	if resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil); resp.StatusCode != 200 {
		t.Fatalf("started: %d %s", resp.StatusCode, body)
	}
	// The Mac is in macOS now: its lab-agent is silent. A check-in from
	// Linux would be the Mac back, which ends Needs hands.
	l.stopAgent()
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands })
	if !strings.Contains(d.StateReason, "macOS") || !strings.Contains(d.StateReason, "FileVault") {
		t.Errorf("reason: %q", d.StateReason)
	}
	_, body := l.get("GET", "/api/devices/"+l.name+"/macos-runs")
	if r := decodeInto[api.MacOSRun](t, body); r.State != api.MacOSNeedsHands {
		t.Errorf("run: %s", body)
	}
}

// Queued runs and enrolled runners outlive a restart of labd.
func TestMacOSRunsAreKeptInTheDataDir(t *testing.T) {
	dir := t.TempDir()
	m := openMacOSRuns(dir)
	if _, err := m.enrolSecret("m2-max", "mos_x"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.queue("m2-max", api.TierDefault, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	m2 := openMacOSRuns(dir)
	if mac, ok := m2.runnerOf("mos_x"); !ok || mac != "m2-max" {
		t.Errorf("runner lost: %q %v", mac, ok)
	}
	if r := m2.run("m2-max"); r == nil || r.State != api.MacOSQueued {
		t.Errorf("run lost: %+v", r)
	}
}
