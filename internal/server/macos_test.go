package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// A macOS run's set must have its macOS files in the artifact store, where
// lab bench mirror puts them; a placeholder is left out.
func TestAMacOSRunIsRefusedWhenItsSetsFilesAreNotMirrored(t *testing.T) {
	l := newLab(t, false)
	sha := l.artifact("web benchmarks bundle")
	set := &sets.Set{Name: "bench-9", N: 9,
		Metrics: []sets.Metric{
			{ID: "web", List: sets.Quick, MacOS: &sets.Side{Files: []string{"web-benchmarks"}}},
			{ID: "later", List: sets.Full, MacOS: &sets.Side{Files: []string{"web-benchmarks", "not-built", "not-mirrored"}}},
		},
		Files: map[string][]sets.File{sets.MacOS: {
			{ID: "web-benchmarks", Version: "1", Kind: "tarball", SHA256: sha},
			{ID: "not-built", Version: "1", Kind: "tarball", SHA256: sets.Placeholder},
			{ID: "not-mirrored", Version: "1", Kind: "tarball", SHA256: strings.Repeat("ab", 32)},
		}}}
	if _, err := l.s.openMacOSSide(l.name, "e1", set, macosBuild{}, nil); err == nil || !strings.Contains(err.Error(), "not-mirrored") || !strings.Contains(err.Error(), "lab bench mirror") {
		t.Fatalf("err %v", err)
	}
	if arts, err := l.s.macOSFiles(set, sets.Quick); err != nil || len(arts) != 1 || arts[0].SHA256 != sha {
		t.Errorf("the Quick list's files: %+v %v", arts, err)
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
	macOSResultSet(t, l)
	secret := enrollRunner(t, l)

	// Nothing queued: the runner hears so, and boot-macos-once is refused
	// before anything changes on the Mac.
	if _, body := l.as(secret, "GET", "/api/macos/runner/poll", nil); decodeInto[api.MacOSPoll](t, body).Run != nil {
		t.Fatalf("a run with none queued: %s", body)
	}
	if resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil); resp.StatusCode != 409 {
		t.Fatalf("started with none queued: %d %s", resp.StatusCode, body)
	}

	reportBuild(t, l, secret, testMacOSVersion, testMacOSBuild)
	run := l.s.macOS().run(l.name)
	if run == nil || run.State != api.MacOSQueued || run.Set != testSet || run.PerfRun == 0 || len(run.Artifacts) == 0 {
		t.Fatalf("queued: %+v", run)
	}
	sha := run.Artifacts[0].SHA256
	if again, err := l.s.queueMacOSSide(l.name); err != nil || again != nil {
		t.Errorf("a second run queued over the first: %+v %v", again, err)
	}
	// Queued but not started: the runner waits for the boot.
	if _, body := l.as(secret, "GET", "/api/macos/runner/poll", nil); decodeInto[api.MacOSPoll](t, body).Run != nil {
		t.Fatalf("a run before boot-macos-once: %s", body)
	}
	resp, body := l.as(l.secret, "POST", "/api/agent/macos-started", nil)
	if resp.StatusCode != 200 || decodeInto[api.MacOSRun](t, body).State != api.MacOSStarted {
		t.Fatalf("started: %d %s", resp.StatusCode, body)
	}

	// A poll that says no boot is handed nothing; the boot after the start is.
	if _, body := l.as(secret, "GET", "/api/macos/runner/poll", nil); decodeInto[api.MacOSPoll](t, body).Run != nil {
		t.Fatalf("a run to a boot that said not which: %s", body)
	}
	got := pollAs(t, l, secret, bootHash(1), *decodeInto[api.MacOSRun](t, body).Started, "")
	if got == nil || got.ID != run.ID || got.Tier != api.TierExtended || got.State != api.MacOSRunning {
		t.Fatalf("poll: %+v", got)
	}
	if resp, body := l.as(secret, "GET", "/api/macos/runner/artifacts/"+sha, nil); resp.StatusCode != 200 || body != "bench-1's macOS files" {
		t.Errorf("artifact: %d %q", resp.StatusCode, body)
	}
	other := l.artifact("not in the run")
	if resp, _ := l.as(secret, "GET", "/api/macos/runner/artifacts/"+other, nil); resp.StatusCode != 404 {
		t.Errorf("an artifact outside the run: %d", resp.StatusCode)
	}
	if resp, _ := l.as("mos_wrong", "GET", "/api/macos/runner/poll", nil); resp.StatusCode != 401 {
		t.Errorf("a wrong runner secret: %d", resp.StatusCode)
	}

	path := fmt.Sprintf("/api/macos/runner/performance/%d/results", run.PerfRun)
	if resp, body := l.as(secret, "POST", path, macOSUpload(t, perf.Succeeded, map[string]float64{"coremark-single": 31000})); resp.StatusCode != 200 {
		t.Fatalf("results: %d %s", resp.StatusCode, body)
	}
	_, body = l.get("GET", "/api/devices/"+l.name+"/macos-runs")
	if done := decodeInto[api.MacOSRun](t, body); done.State != api.MacOSDone {
		t.Fatalf("done: %s", body)
	}
	// Its result is kept: nothing more is queued for the set and build.
	if again, err := l.s.queueMacOSSide(l.name); err != nil || again != nil {
		t.Errorf("queued again after its result: %+v %v", again, err)
	}
	// A Maintainer queues no macOS run by hand, nor does the runner upload
	// anywhere but to its performance run.
	if resp, _ := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/macos-runs", map[string]string{}); resp.StatusCode/100 == 2 {
		t.Errorf("a macOS run queued by hand: %d", resp.StatusCode)
	}
	if resp, _ := l.as(secret, "POST", "/api/macos/runner/results", map[string]string{"run_id": run.ID}); resp.StatusCode/100 == 2 {
		t.Errorf("results to the retired endpoint: %d", resp.StatusCode)
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
	macOSResultSet(t, l)
	reportBuild(t, l, enrollRunner(t, l), testMacOSVersion, testMacOSBuild)
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

// Queued runs and enrolled runners outlive a restart of labd. A secret kept
// before secrets belonged to an enrollment is dropped: that runner enrolls
// again.
func TestMacOSRunsAreKeptInTheDataDir(t *testing.T) {
	dir := t.TempDir()
	m := openMacOSRuns(dir)
	if err := m.enrolSecret("m2-max", "enr-1", "mos_x"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.sayAttendance("mos_x", true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.queueRun(&api.MacOSRun{Mac: "m2-max", Tier: api.TierExtended, Set: testSet, PerfRun: 3}, time.Now()); err != nil {
		t.Fatal(err)
	}
	m2 := openMacOSRuns(dir)
	if e, ok := m2.runnerOf("mos_x"); !ok || e.Mac != "m2-max" || e.Enrollment != "enr-1" || !e.Unattended {
		t.Errorf("runner lost: %+v %v", e, ok)
	}
	if r := m2.run("m2-max"); r == nil || r.State != api.MacOSQueued || r.PerfRun != 3 {
		t.Errorf("run lost: %+v", r)
	}

	old := filepath.Join(t.TempDir(), "macos-runs.json")
	b, _ := json.Marshal(map[string]any{"runners": map[string]string{hashSecret("mos_old"): "m2-max"},
		"tokens": map[string]any{hashSecret("mos_enr_old"): map[string]any{"mac": "m2-max", "expires": time.Now().Add(time.Hour)}}})
	if err := os.WriteFile(old, b, 0o600); err != nil {
		t.Fatal(err)
	}
	m3 := openMacOSRuns(filepath.Dir(old))
	if _, ok := m3.runnerOf("mos_old"); ok {
		t.Error("a secret of no enrollment still works")
	}
	if _, ok := m3.useToken(hashSecret("mos_enr_old"), time.Now()); ok {
		t.Error("a one-time token of no enrollment still works")
	}
}

// The runner's secret is the Mac's enrollment's: when the Volunteer leaves,
// it stops working at once, with its one-time tokens and its unfinished run,
// and a Mac that Joins again under the name gets none of them.
func TestAMacOSRunnerSecretIsRevokedWhenItsVolunteerLeaves(t *testing.T) {
	n, _ := leavingLab(t, labConfig{})
	macOSResultSet(t, n.lab)
	secret := enrollRunner(t, n.lab)
	_, body := n.as(harnessToken, "POST", "/api/macos/enroll-tokens", map[string]string{"mac": n.name})
	unused := decodeInto[api.MacOSEnrollToken](t, body).Token
	reportBuild(t, n.lab, secret, testMacOSVersion, testMacOSBuild)
	if r := n.s.macOS().run(n.name); r == nil || r.State != api.MacOSQueued {
		t.Fatalf("queued: %+v", r)
	}

	n.mustVolunteer("leave", "--yes")
	n.leavingRequireGone(n.name)
	macOSRunnerRevoked(t, n.lab, secret, unused)
}

// The same when a Maintainer ends the enrollment and the Mac is Retired.
func TestAMacOSRunnerSecretIsRevokedWhenItsMacIsRetired(t *testing.T) {
	n, grace := leavingLab(t, labConfig{})
	macOSResultSet(t, n.lab)
	secret := enrollRunner(t, n.lab)
	_, body := n.as(harnessToken, "POST", "/api/macos/enroll-tokens", map[string]string{"mac": n.name})
	unused := decodeInto[api.MacOSEnrollToken](t, body).Token
	reportBuild(t, n.lab, secret, testMacOSVersion, testMacOSBuild)
	if r := n.s.macOS().run(n.name); r == nil || r.State != api.MacOSQueued {
		t.Fatalf("queued: %+v", r)
	}

	n.leavingMustEnd(grace, n.name)
	n.leavingRequireGone(n.name)
	macOSRunnerRevoked(t, n.lab, secret, unused)
}

func macOSRunnerRevoked(t *testing.T, l *lab, secret, unused string) {
	t.Helper()
	if _, ok := l.s.macOS().runnerOf(secret); ok {
		t.Error("labd still keeps the runner's secret")
	}
	if r := l.s.macOS().run(l.name); r != nil {
		t.Errorf("an unfinished run outlived the enrollment: %+v", r)
	}
	if resp, _ := l.as(secret, "GET", "/api/macos/runner/poll", nil); resp.StatusCode != 401 {
		t.Errorf("the runner's secret works after the Mac left: %d", resp.StatusCode)
	}
	// Joined again under the same name: a new enrollment, and nothing of
	// the old one's runner works for it.
	if resp, body := l.enrollRaw(l.enrollToken(l.name)); resp.StatusCode != 200 {
		t.Fatalf("enrolling again: %s %s", resp.Status, body)
	}
	if resp, _ := l.as(secret, "GET", "/api/macos/runner/poll", nil); resp.StatusCode != 401 {
		t.Errorf("the old runner's secret works for the new enrollment: %d", resp.StatusCode)
	}
	if resp, _ := l.as(unused, "POST", "/api/macos/runner/enroll", nil); resp.StatusCode != 401 {
		t.Errorf("the old enrollment's one-time token enrolled the new one's runner: %d", resp.StatusCode)
	}
	if side := l.s.macOSSide(l.name); side.Enrolled || side.Attendance != api.MacOSAttended {
		t.Errorf("the new enrollment's macOS side: %+v", side)
	}
}

// A one-time token belongs to the enrollment it was made for: one made just
// before the Mac left enrolls nothing after.
func TestAMacOSEnrollTokenIsForOneEnrollment(t *testing.T) {
	l := newLab(t, false)
	_, body := l.as(harnessToken, "POST", "/api/macos/enroll-tokens", map[string]string{"mac": l.name})
	tok := decodeInto[api.MacOSEnrollToken](t, body)
	if err := l.s.macOS().addToken(hashSecret(tok.Token+"x"), l.name, "another-enrollment", tok.Expires); err != nil {
		t.Fatal(err)
	}
	if resp, _ := l.as(tok.Token+"x", "POST", "/api/macos/runner/enroll", nil); resp.StatusCode != 401 {
		t.Errorf("a token of another enrollment enrolled the runner: %d", resp.StatusCode)
	}
	if resp, body := l.as(tok.Token, "POST", "/api/macos/runner/enroll", nil); resp.StatusCode != 200 {
		t.Errorf("the token of this enrollment: %d %s", resp.StatusCode, body)
	}
}

// A Mac starts Attended. The runner says Unattended after a boot that
// reached its user's desktop with nobody typing, and Attended after one that
// did not; labd keeps the last word. A runner enrolled again starts over.
func TestLabdKeepsWhetherAMacIsAttendedOrUnattended(t *testing.T) {
	l := newLab(t, false)
	side := func() api.MacOSSide {
		t.Helper()
		resp, body := l.get("GET", "/api/devices/"+l.name+"/macos")
		if resp.StatusCode != 200 {
			t.Fatalf("macOS side: %d %s", resp.StatusCode, body)
		}
		return decodeInto[api.MacOSSide](t, body)
	}
	if s := side(); s.Enrolled || s.Attendance != api.MacOSAttended {
		t.Fatalf("before the runner enrolled: %+v", s)
	}
	secret := enrollRunner(t, l)
	if s := side(); !s.Enrolled || s.Attendance != api.MacOSAttended || s.Said != nil {
		t.Fatalf("a runner just enrolled: %+v", s)
	}
	say := func(unattended bool) {
		t.Helper()
		if resp, body := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: unattended}); resp.StatusCode != 200 {
			t.Fatalf("attendance: %d %s", resp.StatusCode, body)
		}
	}
	say(true)
	if s := side(); s.Attendance != api.MacOSUnattended || s.Said == nil {
		t.Errorf("after an unattended boot: %+v", s)
	}
	say(false)
	if s := side(); s.Attendance != api.MacOSAttended {
		t.Errorf("after a boot somebody typed at: %+v", s)
	}
	say(true)
	if resp, _ := l.as("mos_wrong", "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 401 {
		t.Errorf("attendance with a wrong secret: %d", resp.StatusCode)
	}
	if resp, _ := l.as("", "GET", "/api/devices/"+l.name+"/macos", nil); resp.StatusCode == 200 {
		t.Error("the macOS side with no credential")
	}
	enrollRunner(t, l)
	if s := side(); s.Attendance != api.MacOSAttended {
		t.Errorf("a runner enrolled again: %+v", s)
	}
	if resp, _ := l.as(secret, "POST", "/api/macos/runner/attendance", api.MacOSAttendance{Unattended: true}); resp.StatusCode != 401 {
		t.Errorf("the runner's earlier secret still works: %d", resp.StatusCode)
	}
}
