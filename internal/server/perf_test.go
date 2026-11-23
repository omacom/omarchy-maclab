package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

func perfStore(t *testing.T) *store {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "labd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	return st
}

// perfEntry is a result of bench-1 as bench.json has it: a value, or with
// a reason code in place of one.
func perfEntry(t *testing.T, metric string, value float64, reason string) map[string]any {
	t.Helper()
	set, err := sets.Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := set.Metric(metric)
	if !ok {
		t.Fatalf("no %s in bench-1", metric)
	}
	e := map[string]any{"set": set.Name, "metric": m.ID, "category": m.Category, "version": m.Version,
		"value": value, "unit": m.Unit, "higher_is_better": m.HigherIsBetter(), "spread": 0.5,
		"seconds": 30.0, "cool_down": 5.0, "reason": nil}
	if reason != "" {
		e["value"], e["spread"], e["reason"] = nil, nil, reason
	}
	if m.Category == "Storage" {
		e["capacity"] = 512
	}
	return e
}

func perfChecked(t *testing.T, side, state string, entries ...map[string]any) perf.Checked {
	t.Helper()
	up := perf.Upload{State: state}
	for _, e := range entries {
		b, _ := json.Marshal(e)
		up.Entries = append(up.Entries, b)
	}
	set, _ := sets.Load("bench-1")
	c, err := perf.Check(up, set, side)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var perfNow = time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)

func openOmarchy(t *testing.T, st *store, mac, release, list string) perf.Run {
	t.Helper()
	r, err := st.openPerfRun(perf.Run{Mac: mac, Enrollment: "enr-" + mac, Set: "bench-1", Side: perf.SideOmarchy, Release: release, List: list}, perfNow)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func take(t *testing.T, st *store, id int64, c perf.Checked) perf.Taken {
	t.Helper()
	tk, err := st.takePerf(id, c, perfNow)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

// current is the metric's current value (or reason) and the attempt it
// came from.
func current(t *testing.T, got []perf.Result, metric string) string {
	t.Helper()
	for _, r := range got {
		if r.Metric == metric {
			if r.Reason != nil {
				return fmt.Sprintf("%s@%d", *r.Reason, r.Attempt)
			}
			return fmt.Sprintf("%g@%d", *r.Value, r.Attempt)
		}
	}
	return "none"
}

func TestARerunSupersedesTheCurrentValuesOnlyWhenItSucceeds(t *testing.T) {
	st := perfStore(t)
	r1 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r1.ID, perfChecked(t, perf.SideOmarchy, perf.Succeeded,
		perfEntry(t, "coremark-single", 100, ""), perfEntry(t, "speedometer", 20, "")))

	r2 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r2.ID, perfChecked(t, perf.SideOmarchy, "", perfEntry(t, "coremark-single", 120, "")))
	got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if c := current(t, got, "coremark-single"); c != "100@1" {
		t.Fatalf("a running rerun's value is current: %s", c)
	}
	take(t, st, r2.ID, perfChecked(t, perf.SideOmarchy, perf.Failed, perfEntry(t, "speedometer", 0, perf.Error)))
	got, _ = st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if a, b := current(t, got, "coremark-single"), current(t, got, "speedometer"); a != "100@1" || b != "20@1" {
		t.Fatalf("a failed rerun replaced the values: %s %s", a, b)
	}

	r3 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r3.ID, perfChecked(t, perf.SideOmarchy, perf.Succeeded, perfEntry(t, "coremark-single", 130, "")))
	got, _ = st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if a, b := current(t, got, "coremark-single"), current(t, got, "speedometer"); a != "130@3" || b != "20@1" {
		t.Fatalf("after a rerun that succeeded: %s %s", a, b)
	}
	if len(got) != 2 {
		t.Fatalf("%d current values, want one per metric: %+v", len(got), got)
	}
	// Another Release has values of its own.
	if got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.12"); len(got) != 0 {
		t.Fatalf("another Release: %+v", got)
	}
}

func TestAFailedAttemptFillsOnlyAMetricWithNoValue(t *testing.T) {
	st := perfStore(t)
	r1 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r1.ID, perfChecked(t, perf.SideOmarchy, perf.Failed,
		perfEntry(t, "stream-triad", 90, ""), perfEntry(t, "motionmark", 0, perf.Error)))
	got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if a, b := current(t, got, "stream-triad"), current(t, got, "motionmark"); a != "90@1" || b != "error@1" {
		t.Fatalf("a failed first attempt: %s %s", a, b)
	}
	r2 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r2.ID, perfChecked(t, perf.SideOmarchy, perf.Succeeded, perfEntry(t, "motionmark", 800, "")))
	got, _ = st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if a, b := current(t, got, "stream-triad"), current(t, got, "motionmark"); a != "90@1" || b != "800@2" {
		t.Fatalf("the retry: %s %s", a, b)
	}
	// labd cancelling a run makes nothing current.
	r3 := openOmarchy(t, st, "m1", "2026.11", sets.Full)
	take(t, st, r3.ID, perfChecked(t, perf.SideOmarchy, "", perfEntry(t, "7zip", 5000, "")))
	if err := st.endPerfRun(r3.ID, perf.Cancelled, perfNow); err != nil {
		t.Fatal(err)
	}
	got, _ = st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if c := current(t, got, "7zip"); c != "none" {
		t.Fatalf("a cancelled run's value is current: %s", c)
	}
}

func TestAFailedAttemptPutsAValueWhereThereIsOnlyAReason(t *testing.T) {
	st := perfStore(t)
	r1 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r1.ID, perfChecked(t, perf.SideOmarchy, perf.Failed,
		perfEntry(t, "coremark-single", 0, perf.NotIdle), perfEntry(t, "speedometer", 0, perf.NotIdle)))
	r2 := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r2.ID, perfChecked(t, perf.SideOmarchy, perf.Failed,
		perfEntry(t, "coremark-single", 100, ""), perfEntry(t, "speedometer", 0, perf.Error)))
	got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if a, b := current(t, got, "coremark-single"), current(t, got, "speedometer"); a != "100@2" || b != "not_idle@1" {
		t.Fatalf("after the second attempt: %s %s", a, b)
	}
}

// A Mac enrolled under a name used before has no values and no attempts,
// and its own first attempt is current.
func TestAReusedNameStartsWithNoResults(t *testing.T) {
	st := perfStore(t)
	for i := 0; i < 2; i++ {
		old := perf.Run{Mac: "m1", Enrollment: "enr-old", Set: "bench-1", Side: perf.SideOmarchy, Release: "2026.11", List: sets.Quick, Metrics: []string{"coremark-single"}}
		r, err := st.openPerfRun(old, perfNow)
		if err != nil {
			t.Fatal(err)
		}
		take(t, st, r.ID, perfChecked(t, perf.SideOmarchy, perf.Succeeded, perfEntry(t, "coremark-single", 100, "")))
		mos := perf.Run{Mac: "m1", Enrollment: "enr-old", Set: "bench-1", Side: perf.SideMacOS, MacOSVersion: "27.2.1", MacOSBuild: "27C101", List: sets.Full}
		if r, err = st.openPerfRun(mos, perfNow); err != nil {
			t.Fatal(err)
		}
		take(t, st, r.ID, perfChecked(t, perf.SideMacOS, perf.Failed, perfEntry(t, "coremark-single", 0, perf.Error)))
	}
	cur, tries, err := st.perfTries("m1", "enr-m1", "bench-1", "2026.11")
	if err != nil || len(cur) != 0 || len(tries) != 0 {
		t.Fatalf("the new enrollment has %v, tried %v (%v)", cur, tries, err)
	}
	if n, err := st.macOSTries("m1", "enr-m1", "bench-1", "27C101"); err != nil || n != 0 {
		t.Fatalf("the new enrollment's macOS attempts: %d (%v)", n, err)
	}
	if got, _ := st.perfCurrentMacOS("m1", "enr-m1", "bench-1", "27C101"); len(got) != 0 {
		t.Fatalf("the new enrollment's macOS result: %+v", got)
	}
	r := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	take(t, st, r.ID, perfChecked(t, perf.SideOmarchy, perf.Failed, perfEntry(t, "coremark-single", 0, perf.Error)))
	got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if c := current(t, got, "coremark-single"); c != "error@3" {
		t.Fatalf("its own first attempt: %s", c)
	}
	if got, _ = st.perfCurrent("m1", "enr-old", "bench-1", "2026.11"); current(t, got, "coremark-single") != "100@2" {
		t.Fatalf("the old enrollment's: %+v", got)
	}
}

func TestAnUploadIsTakenOncePerAttemptAndMetric(t *testing.T) {
	st := perfStore(t)
	r := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	first := perfChecked(t, perf.SideOmarchy, "", perfEntry(t, "coremark-single", 100, ""))
	if tk := take(t, st, r.ID, first); tk.Taken != 1 || tk.Already != 0 {
		t.Fatalf("first: %+v", tk)
	}
	if tk := take(t, st, r.ID, first); tk.Taken != 0 || tk.Already != 1 {
		t.Fatalf("the same again: %+v", tk)
	}
	other := perfChecked(t, perf.SideOmarchy, "", perfEntry(t, "coremark-single", 999, ""), perfEntry(t, "stream-triad", 90, ""))
	if tk := take(t, st, r.ID, other); tk.Taken != 1 || tk.Already != 1 {
		t.Fatalf("another value for a metric taken: %+v", tk)
	}
	end := perfChecked(t, perf.SideOmarchy, perf.Succeeded, perfEntry(t, "stream-triad", 90, ""))
	if tk := take(t, st, r.ID, end); tk.State != perf.Succeeded {
		t.Fatalf("end: %+v", tk)
	}
	// The Mac sends its last upload again, its answer lost: taken as before.
	if tk := take(t, st, r.ID, end); tk.Taken != 0 || tk.Already != 1 || tk.State != perf.Succeeded {
		t.Fatalf("the last upload again: %+v", tk)
	}
	if _, err := st.takePerf(r.ID, perfChecked(t, perf.SideOmarchy, "", perfEntry(t, "speedometer", 20, "")), perfNow); !errors.Is(err, errPerfEnded) {
		t.Fatalf("a result after the end: %v", err)
	}
	if _, err := st.takePerf(r.ID, perfChecked(t, perf.SideOmarchy, perf.Failed), perfNow); !errors.Is(err, errPerfEnded) {
		t.Fatalf("an end over the end: %v", err)
	}
	a, _ := st.perfAttempts("m1", "bench-1", perf.SideOmarchy, "2026.11")
	if len(a) != 1 || len(a[0].Results) != 2 || *a[0].Results[0].Value != 100 {
		t.Fatalf("attempts: %+v", a)
	}
	got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if c := current(t, got, "coremark-single"); c != "100@1" {
		t.Fatalf("current: %s", c)
	}
}

func TestEveryAttemptIsKept(t *testing.T) {
	st := perfStore(t)
	boot := 17.5
	for i, state := range []string{perf.Failed, perf.Succeeded, perf.Failed} {
		r := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
		if r.Attempt != i+1 {
			t.Fatalf("attempt %d, want %d", r.Attempt, i+1)
		}
		c := perfChecked(t, perf.SideOmarchy, state, perfEntry(t, "coremark-single", float64(100+i), ""))
		c.Booted, c.BootSeconds, c.Conditions = map[string]string{"linux-asahi": fmt.Sprintf("6.18.%d-1", i)}, &boot, `{"on_ac":true}`
		take(t, st, r.ID, c)
	}
	// Another Mac, Release or side counts its own attempts.
	if r := openOmarchy(t, st, "m2", "2026.11", sets.Quick); r.Attempt != 1 {
		t.Fatalf("another Mac's attempt %d", r.Attempt)
	}
	if r := openOmarchy(t, st, "m1", "2026.12", sets.Quick); r.Attempt != 1 {
		t.Fatalf("another Release's attempt %d", r.Attempt)
	}
	if _, err := st.openPerfRun(perf.Run{Mac: "m1", Enrollment: "enr-m1", Set: "bench-1", Side: perf.SideOmarchy, Release: "2026.12", List: sets.Full}, perfNow); !errors.Is(err, errPerfRunning) {
		t.Fatalf("a second run beside a running one: %v", err)
	}
	mos, err := st.openPerfRun(perf.Run{Mac: "m1", Enrollment: "enr-m1", Set: "bench-1", Side: perf.SideMacOS, MacOSVersion: "27.2.1", MacOSBuild: "27C101", List: sets.Quick}, perfNow)
	if err != nil || mos.Attempt != 1 {
		t.Fatalf("macOS: %+v %v", mos, err)
	}
	take(t, st, mos.ID, perfChecked(t, perf.SideMacOS, perf.Succeeded, perfEntry(t, "coremark-single", 150, "")))
	if got, _ := st.perfCurrentMacOS("m1", "enr-m1", "bench-1", "27C101"); current(t, got, "coremark-single") != "150@1" {
		t.Fatalf("the macOS result: %+v", got)
	}

	a, err := st.perfAttempts("m1", "bench-1", perf.SideOmarchy, "2026.11")
	if err != nil || len(a) != 3 {
		t.Fatalf("%d attempts %v", len(a), err)
	}
	for i, at := range a {
		if at.Run.Attempt != i+1 || len(at.Results) != 1 || *at.Results[0].Value != float64(100+i) ||
			at.Run.Booted["linux-asahi"] != fmt.Sprintf("6.18.%d-1", i) || *at.Run.BootSeconds != boot ||
			string(at.Run.Conditions) != `{"on_ac":true}` || at.Run.Ended == nil {
			t.Errorf("attempt %d: %+v", i+1, at)
		}
	}
	if a[0].Run.State != perf.Failed || a[1].Run.State != perf.Succeeded || a[2].Run.State != perf.Failed {
		t.Errorf("states: %s %s %s", a[0].Run.State, a[1].Run.State, a[2].Run.State)
	}
	got, _ := st.perfCurrent("m1", "enr-m1", "bench-1", "2026.11")
	if c := current(t, got, "coremark-single"); c != "101@2" {
		t.Fatalf("current: %s", c)
	}
	if m, _ := st.perfAttempts("m1", "bench-1", perf.SideMacOS, "27C101"); len(m) != 1 || m[0].Run.MacOSVersion != "27.2.1" {
		t.Fatalf("macOS attempts: %+v", m)
	}
}

func TestAnAttemptWithAnErrorCannotSucceed(t *testing.T) {
	st := perfStore(t)
	r := openOmarchy(t, st, "m1", "2026.11", sets.Quick)
	_, err := st.takePerf(r.ID, perfChecked(t, perf.SideOmarchy, perf.Succeeded,
		perfEntry(t, "coremark-single", 100, ""), perfEntry(t, "motionmark", 0, perf.Error)), perfNow)
	if !errors.Is(err, errPerfErrorRun) {
		t.Fatalf("succeeded with an error: %v", err)
	}
	if a, _ := st.perfAttempts("m1", "bench-1", perf.SideOmarchy, "2026.11"); len(a[0].Results) != 0 || a[0].Run.State != perf.Running {
		t.Fatalf("a refused upload left: %+v", a)
	}
}

func TestResultsThatComeAfterNeedsHandsAreTaken(t *testing.T) {
	st := perfStore(t)
	r, _ := st.openPerfRun(perf.Run{Mac: "m1", Enrollment: "enr-m1", Set: "bench-1", Side: perf.SideMacOS, MacOSBuild: "27C101", List: sets.Quick}, perfNow)
	if err := st.endPerfRun(r.ID, perf.NeedsHands, perfNow); err != nil {
		t.Fatal(err)
	}
	if tk := take(t, st, r.ID, perfChecked(t, perf.SideMacOS, perf.Succeeded, perfEntry(t, "speedometer", 25, ""))); tk.Taken != 1 || tk.State != perf.Succeeded {
		t.Fatalf("late: %+v", tk)
	}
	if got, _ := st.perfCurrentMacOS("m1", "enr-m1", "bench-1", "27C101"); current(t, got, "speedometer") != "25@1" {
		t.Fatalf("current: %+v", got)
	}
}

func TestThePerformanceUploadsRefuseWhatIsNotTheirs(t *testing.T) {
	l := newLab(t, false)
	enr := l.s.dev(l.name).snapshot().ID
	run, err := l.s.store.openPerfRun(perf.Run{Mac: l.name, Enrollment: enr, Set: "bench-1", Side: perf.SideOmarchy, Release: "2026.11", List: sets.Quick}, l.s.now())
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/agent/performance/%d/results", run.ID)
	entry := perfEntry(t, "coremark-single", 100, "")
	up := map[string]any{"entries": []any{entry}, "booted": map[string]string{"linux-asahi": "6.18.2-1"}}

	if resp, _ := l.as("wrong", "POST", path, up); resp.StatusCode != 401 {
		t.Errorf("a wrong device secret: %d", resp.StatusCode)
	}
	for name, bad := range map[string]map[string]any{
		"a field outside the upload": {"entries": []any{entry}, "hostname": "someones-mac"},
		"a field outside a result":   {"entries": []any{mapWith(entry, "note", "chrome: /home/bob")}},
		"an unknown metric":          {"entries": []any{mapWith(entry, "metric", "geekbench")}},
		"a reason that is not code":  {"entries": []any{mapWith(mapWith(entry, "value", nil), "reason", "timed out")}},
		"a condition outside":        {"entries": []any{entry}, "conditions": map[string]any{"ssid": "HomeNet"}},
	} {
		if resp, body := l.as(l.secret, "POST", path, bad); resp.StatusCode != 400 {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if a, _ := l.s.store.perfAttempts(l.name, "bench-1", perf.SideOmarchy, "2026.11"); len(a[0].Results) != 0 {
		t.Fatalf("a refused upload left results: %+v", a[0].Results)
	}

	resp, body := l.as(l.secret, "POST", path, up)
	if resp.StatusCode != 200 || decodeInto[perf.Taken](t, body).Taken != 1 {
		t.Fatalf("upload: %d %s", resp.StatusCode, body)
	}
	up["state"] = perf.Succeeded
	resp, body = l.as(l.secret, "POST", path, up)
	if tk := decodeInto[perf.Taken](t, body); resp.StatusCode != 200 || tk.Already != 1 || tk.State != perf.Succeeded {
		t.Fatalf("again, ending it: %d %s", resp.StatusCode, body)
	}
	if got, _ := l.s.store.perfCurrent(l.name, l.s.dev(l.name).snapshot().ID, "bench-1", "2026.11"); current(t, got, "coremark-single") != "100@1" {
		t.Fatalf("current: %+v", got)
	}
	if resp, _ := l.as(l.secret, "POST", path, map[string]any{"entries": []any{perfEntry(t, "speedometer", 20, "")}}); resp.StatusCode != 409 {
		t.Errorf("a result after the end: %d", resp.StatusCode)
	}

	// The macOS runner reaches only its Mac's macOS runs; lab-agent only
	// its enrollment's Omarchy runs.
	secret := enrollRunner(t, l)
	if resp, _ := l.as(secret, "POST", fmt.Sprintf("/api/macos/runner/performance/%d/results", run.ID), up); resp.StatusCode != 404 {
		t.Errorf("the runner on an Omarchy run: %d", resp.StatusCode)
	}
	mos, err := l.s.store.openPerfRun(perf.Run{Mac: l.name, Enrollment: enr, Set: "bench-1", Side: perf.SideMacOS, MacOSVersion: "27.2.1", MacOSBuild: "27C101", List: sets.Quick}, l.s.now())
	if err != nil {
		t.Fatal(err)
	}
	mosPath := fmt.Sprintf("/api/macos/runner/performance/%d/results", mos.ID)
	if resp, _ := l.as(l.secret, "POST", fmt.Sprintf("/api/agent/performance/%d/results", mos.ID), up); resp.StatusCode != 404 {
		t.Errorf("lab-agent on a macOS run: %d", resp.StatusCode)
	}
	if resp, _ := l.as("mos_wrong", "POST", mosPath, up); resp.StatusCode != 401 {
		t.Errorf("a wrong runner secret: %d", resp.StatusCode)
	}
	if resp, body := l.as(secret, "POST", mosPath, up); resp.StatusCode != 400 || !strings.Contains(body, "booted") {
		t.Errorf("booted versions from macOS: %d %s", resp.StatusCode, body)
	}
	resp, body = l.as(secret, "POST", mosPath, map[string]any{"entries": []any{entry}, "state": perf.Succeeded})
	if resp.StatusCode != 200 || decodeInto[perf.Taken](t, body).Taken != 1 {
		t.Fatalf("macOS upload: %d %s", resp.StatusCode, body)
	}
	if got, _ := l.s.store.perfCurrentMacOS(l.name, l.s.dev(l.name).snapshot().ID, "bench-1", "27C101"); current(t, got, "coremark-single") != "100@1" {
		t.Fatalf("macOS current: %+v", got)
	}
	if resp, _ := l.as(l.secret, "POST", "/api/agent/performance/999999/results", up); resp.StatusCode != 404 {
		t.Errorf("no such run: %d", resp.StatusCode)
	}
}

func mapWith(m map[string]any, k string, v any) map[string]any {
	c := map[string]any{}
	for a, b := range m {
		c[a] = b
	}
	c[k] = v
	return c
}
