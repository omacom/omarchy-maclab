package perf

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

func bench1(t *testing.T) *sets.Set {
	t.Helper()
	s, err := sets.Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func entry(edit func(m map[string]any)) json.RawMessage {
	m := map[string]any{
		"set": "bench-1", "metric": "coremark-single", "category": "CPU", "version": "1.01",
		"value": 31250.5, "unit": "iterations/s", "higher_is_better": true, "spread": 0.4,
		"seconds": 62.0, "cool_down": 12.0, "reason": nil,
	}
	if edit != nil {
		edit(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func TestAResultAsBenchJSONHasItIsTaken(t *testing.T) {
	set := bench1(t)
	e, err := CheckEntry(entry(nil), set, SideOmarchy)
	if err != nil || e.Metric != "coremark-single" || *e.Value != 31250.5 || e.Reason != nil {
		t.Fatalf("%+v %v", e, err)
	}
	e, err = CheckEntry(entry(func(m map[string]any) {
		m["metric"], m["category"], m["version"], m["unit"] = "fio-seq-read", "Storage", "placeholder", "MB/s"
		m["capacity"] = 512
	}), set, SideMacOS)
	if err != nil || *e.Capacity != 512 {
		t.Fatalf("a Storage value with its capacity: %+v %v", e, err)
	}
	e, err = CheckEntry(entry(func(m map[string]any) {
		m["metric"], m["category"], m["version"], m["unit"] = "mlx-matmul-fp16", "AI", "placeholder", "TFLOPS"
		m["value"], m["spread"], m["reason"] = nil, nil, NoBuild
	}), set, SideOmarchy)
	if err != nil || *e.Reason != NoBuild || e.Value != nil {
		t.Fatalf("a reason code in place of a value: %+v %v", e, err)
	}
}

func TestAResultWithAFieldOutsideTheAllowlistIsRefused(t *testing.T) {
	set := bench1(t)
	for name, edit := range map[string]func(m map[string]any){
		"a tool's own text":         func(m map[string]any) { m["note"] = "/home/someone/.cache/chrome crashed" },
		"a field in another case":   func(m map[string]any) { m["Value"] = 1.0 },
		"a hostname":                func(m map[string]any) { m["host"] = "someones-mac" },
		"a field missing":           func(m map[string]any) { delete(m, "reason") },
		"a reason that is not code": func(m map[string]any) { m["value"], m["reason"] = nil, "Chrome said: no GPU for user bob" },
		"a value and a reason":      func(m map[string]any) { m["reason"] = Skipped },
		"neither":                   func(m map[string]any) { m["value"] = nil },
		"an unknown metric":         func(m map[string]any) { m["metric"] = "geekbench" },
		"another set":               func(m map[string]any) { m["set"] = "bench-2" },
		"another unit":              func(m map[string]any) { m["unit"] = "MIPS" },
		"another version":           func(m map[string]any) { m["version"] = "1.0" },
		"another api":               func(m map[string]any) { m["api"] = "OpenGL" },
		"a capacity off Storage":    func(m map[string]any) { m["capacity"] = 512 },
		"a negative value":          func(m map[string]any) { m["value"] = -1.0 },
		"not an object":             nil,
	} {
		raw := entry(edit)
		if edit == nil {
			raw = json.RawMessage(`["coremark-single", 1]`)
		}
		if e, err := CheckEntry(raw, set, SideOmarchy); err == nil {
			t.Errorf("%s: taken: %+v", name, e)
		}
	}
	if _, err := CheckEntry(entry(func(m map[string]any) {
		m["metric"], m["category"], m["version"], m["unit"] = "fio-seq-read", "Storage", "placeholder", "MB/s"
	}), set, SideOmarchy); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Errorf("a Storage value with no capacity: %v", err)
	}
	if _, err := CheckEntry(entry(func(m map[string]any) {
		m["metric"], m["category"], m["version"], m["unit"], m["higher_is_better"] = "boot-time", "Omarchy only", "1", "s", false
	}), set, SideMacOS); err == nil || !strings.Contains(err.Error(), "does not run on macos") {
		t.Errorf("boot time from macOS: %v", err)
	}
}

func TestEveryReasonCodeIsKnownAndNothingElse(t *testing.T) {
	if len(Reasons) != 12 {
		t.Fatalf("%d reason codes", len(Reasons))
	}
	for _, r := range Reasons {
		if !KnownReason(r) || strings.ToLower(r) != r || strings.ContainsAny(r, " -") {
			t.Errorf("reason code %q", r)
		}
	}
	for _, r := range []string{"", "fell back", "Error", "timeout"} {
		if KnownReason(r) {
			t.Errorf("%q is a reason code", r)
		}
	}
}

func TestAnUploadCarriesOnlyAllowedConditionsAndBootedVersions(t *testing.T) {
	set := bench1(t)
	boot := 18.4
	ok := Upload{
		Entries:     []json.RawMessage{entry(nil)},
		Conditions:  json.RawMessage(`{"on_ac":true,"cpu_busy_percent":4.2,"power_profile":"balanced"}`),
		Booted:      map[string]string{"linux-asahi": "6.18.2.asahi1-1", "mesa": "1:25.3.1-1"},
		BootSeconds: &boot,
		State:       Succeeded,
	}
	c, err := Check(ok, set, SideOmarchy)
	if err != nil || len(c.Entries) != 1 || c.Conditions != `{"cpu_busy_percent":4.2,"on_ac":true,"power_profile":"balanced"}` {
		t.Fatalf("%+v %v", c, err)
	}
	for name, edit := range map[string]func(u *Upload){
		"a condition outside the list": func(u *Upload) { u.Conditions = json.RawMessage(`{"on_ac":true,"wifi":"HomeNet"}`) },
		"free text in a condition":     func(u *Upload) { u.Conditions = json.RawMessage(`{"power_profile":"set by /home/bob"}`) },
		"a booted name with a path":    func(u *Upload) { u.Booted = map[string]string{"/home/bob/mesa": "1"} },
		"a booted version with spaces": func(u *Upload) { u.Booted = map[string]string{"mesa": "built by bob"} },
		"a metric twice":               func(u *Upload) { u.Entries = append(u.Entries, entry(nil)) },
		"another state":                func(u *Upload) { u.State = "done" },
	} {
		up := ok
		up.Booted = map[string]string{"mesa": "25.3.1-1"}
		edit(&up)
		if _, err := Check(up, set, SideOmarchy); err == nil {
			t.Errorf("%s: taken", name)
		}
	}
	if _, err := Check(ok, set, SideMacOS); err == nil {
		t.Error("booted versions from macOS taken")
	}
}
