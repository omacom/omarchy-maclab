// Package perf is what a Performance run or a macOS run uploads to labd and
// what labd keeps of it (spec section 22, "Measuring" and "Performance
// run"): the fields a result may carry, the fixed reason codes, the run
// conditions and booted versions, and the check every upload passes before
// labd takes any of it.
package perf

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

// Fields are the only fields a result carries, as bench.json has them.
var Fields = []string{
	"set", "metric", "category", "version", "value", "unit", "higher_is_better",
	"spread", "seconds", "cool_down", "api", "capacity", "fill", "reason",
}

// required are the fields every result names, null where it has none; api
// and capacity only where they apply.
var required = []string{
	"set", "metric", "category", "version", "value", "unit", "higher_is_better",
	"spread", "seconds", "cool_down", "reason",
}

// Entry is one measurement of bench.json. Value and Reason: exactly one is
// set. Capacity is the SSD's, in GB, on every Storage value; Fill is how
// full the disk was, in percent, beside it.
type Entry struct {
	Set            string   `json:"set"`
	Metric         string   `json:"metric"`
	Category       string   `json:"category"`
	Version        string   `json:"version"`
	Value          *float64 `json:"value"`
	Unit           string   `json:"unit"`
	HigherIsBetter bool     `json:"higher_is_better"`
	Spread         *float64 `json:"spread"`
	Seconds        float64  `json:"seconds"`
	CoolDown       float64  `json:"cool_down"`
	API            string   `json:"api,omitempty"`
	Capacity       *int64   `json:"capacity,omitempty"`
	Fill           *float64 `json:"fill,omitempty"`
	Reason         *string  `json:"reason"`
}

// ConditionFields are the only fields of bench-conditions.json. Each is a
// boolean, a number or a short lowercase word.
var ConditionFields = []string{
	"on_ac", "cpu_busy_percent", "backlight_percent", "power_profile",
	"low_power_mode", "refresh_hz", "free_percent",
}

// Upload is what a Mac sends for a run, once or in parts: its results, the
// conditions it ran in, and on Omarchy the versions it booted, by exact
// package name, and its boot time. State, when set, ends the attempt.
type Upload struct {
	Entries     []json.RawMessage `json:"entries"`
	Conditions  json.RawMessage   `json:"conditions,omitempty"`
	Booted      map[string]string `json:"booted,omitempty"`
	BootSeconds *float64          `json:"boot_seconds,omitempty"`
	State       string            `json:"state,omitempty"`
}

// Checked is an Upload that passed Check.
type Checked struct {
	Entries     []Entry
	Conditions  string // canonical JSON, "" when the upload had none
	Booted      map[string]string
	BootSeconds *float64
	State       string
}

// Taken is labd's answer to an upload: how many results it took and how
// many it had already, from an earlier upload of the same attempt.
type Taken struct {
	Run     int64  `json:"run"`
	Attempt int    `json:"attempt"`
	State   string `json:"state"`
	Taken   int    `json:"taken"`
	Already int    `json:"already"`
}

const (
	SideOmarchy = sets.Omarchy
	SideMacOS   = sets.MacOS
)

// A run's states. Running and NeedsHands take uploads; the rest have ended.
const (
	Running    = "running"
	NeedsHands = "needs_hands"
	Succeeded  = "succeeded"
	Failed     = "failed"
	Cancelled  = "cancelled"
)

// Ended is whether a run in state takes no new results.
func Ended(state string) bool {
	return state == Succeeded || state == Failed || state == Cancelled
}

// Run is one attempt at a Performance run (Side omarchy, of a Release) or a
// macOS run (Side macos, at a macOS build) of one set on one Mac.
type Run struct {
	ID           int64             `json:"id"`
	Mac          string            `json:"mac"`
	Enrollment   string            `json:"enrollment"`
	Set          string            `json:"set"`
	Side         string            `json:"side"`
	Release      string            `json:"release,omitempty"`
	MacOSVersion string            `json:"macos_version,omitempty"`
	MacOSBuild   string            `json:"macos_build,omitempty"`
	List         string            `json:"list"`
	Attempt      int               `json:"attempt"`
	State        string            `json:"state"`
	Booted       map[string]string `json:"booted,omitempty"`
	BootSeconds  *float64          `json:"boot_seconds,omitempty"`
	Conditions   json.RawMessage   `json:"conditions,omitempty"`
	Started      time.Time         `json:"started"`
	Ended        *time.Time        `json:"ended,omitempty"`
}

// Target is what a run measures against: its Release, or its macOS build.
func (r Run) Target() string {
	if r.Side == SideMacOS {
		return r.MacOSBuild
	}
	return r.Release
}

// Result is one kept result of a run's attempt.
type Result struct {
	Run      int64     `json:"run"`
	Attempt  int       `json:"attempt"`
	Metric   string    `json:"metric"`
	Value    *float64  `json:"value"`
	Spread   *float64  `json:"spread"`
	Unit     string    `json:"unit"`
	API      string    `json:"api,omitempty"`
	Capacity *int64    `json:"capacity,omitempty"`
	Fill     *float64  `json:"fill,omitempty"`
	Reason   *string   `json:"reason"`
	Seconds  float64   `json:"seconds"`
	CoolDown float64   `json:"cool_down"`
	Taken    time.Time `json:"taken"`
}

// Attempt is one attempt with every result it uploaded.
type Attempt struct {
	Run     Run      `json:"run"`
	Results []Result `json:"results"`
}

var (
	rePackage = regexp.MustCompile(`^[a-z0-9][a-z0-9+._-]{0,63}$`)
	reVersion = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+._:~-]{0,63}$`)
	reWord    = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

const maxEntries = 256

// Check checks an upload for a run of set on side: every result names only
// the allowed fields and a metric of the set that runs on that side, with
// the set's category, version, unit and direction, and either a value or a
// fixed reason code; the conditions and booted versions carry nothing else.
// Nothing of an upload that fails is taken.
func Check(up Upload, set *sets.Set, side string) (Checked, error) {
	c := Checked{Booted: up.Booted, BootSeconds: up.BootSeconds, State: up.State}
	switch up.State {
	case "", Succeeded, Failed:
	default:
		return c, fmt.Errorf("state %s: want succeeded or failed, or none while results are still coming", clip(up.State))
	}
	if len(up.Entries) > maxEntries {
		return c, fmt.Errorf("%d results in one upload: at most %d", len(up.Entries), maxEntries)
	}
	seen := map[string]bool{}
	for i, raw := range up.Entries {
		e, err := CheckEntry(raw, set, side)
		if err != nil {
			return c, fmt.Errorf("result %d: %w", i+1, err)
		}
		if seen[e.Metric] {
			return c, fmt.Errorf("result %d: %s twice in one upload", i+1, e.Metric)
		}
		seen[e.Metric] = true
		c.Entries = append(c.Entries, e)
	}
	if len(up.Conditions) > 0 && !bytes.Equal(bytes.TrimSpace(up.Conditions), []byte("null")) {
		cond, err := checkConditions(up.Conditions)
		if err != nil {
			return c, err
		}
		c.Conditions = cond
	}
	if side == SideMacOS && (len(up.Booted) > 0 || up.BootSeconds != nil) {
		return c, fmt.Errorf("a macOS run reports its build, not booted versions or a boot time")
	}
	if len(up.Booted) > 64 {
		return c, fmt.Errorf("%d booted versions: at most 64", len(up.Booted))
	}
	for _, name := range sortedKeys(up.Booted) {
		if !rePackage.MatchString(name) {
			return c, fmt.Errorf("booted: %s is not a package name", clip(name))
		}
		if !reVersion.MatchString(up.Booted[name]) {
			return c, fmt.Errorf("booted: %s: %s is not a version", name, clip(up.Booted[name]))
		}
	}
	if b := up.BootSeconds; b != nil && (*b < 0 || *b > 3600) {
		return c, fmt.Errorf("boot time %v s: want 0 to 3600", *b)
	}
	return c, nil
}

// CheckEntry checks one result of bench.json against the set, for side.
func CheckEntry(raw json.RawMessage, set *sets.Set, side string) (Entry, error) {
	var e Entry
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return e, fmt.Errorf("not a JSON object")
	}
	for _, k := range sortedKeys(fields) {
		if !slices.Contains(Fields, k) {
			return e, fmt.Errorf("field %s is not one a result carries (%v)", clip(k), Fields)
		}
	}
	for _, k := range required {
		if _, ok := fields[k]; !ok {
			return e, fmt.Errorf("field %s is missing", k)
		}
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, err
	}
	if e.Set != set.Name {
		return e, fmt.Errorf("set %s: the run is of %s", clip(e.Set), set.Name)
	}
	m, ok := set.Metric(e.Metric)
	if !ok {
		return e, fmt.Errorf("metric %s is not in %s", clip(e.Metric), set.Name)
	}
	s := m.Side(side)
	if s == nil {
		return e, fmt.Errorf("%s does not run on %s", m.ID, side)
	}
	switch {
	case e.Category != m.Category:
		return e, fmt.Errorf("%s: category %s, the set says %s", m.ID, clip(e.Category), m.Category)
	case e.Version != m.Version:
		return e, fmt.Errorf("%s: version %s, the set says %s", m.ID, clip(e.Version), m.Version)
	case e.Unit != m.Unit:
		return e, fmt.Errorf("%s: unit %s, the set says %s", m.ID, clip(e.Unit), m.Unit)
	case e.HigherIsBetter != m.HigherIsBetter():
		return e, fmt.Errorf("%s: higher_is_better %v, the set says %s", m.ID, e.HigherIsBetter, m.Direction)
	case e.API != "" && e.API != s.API:
		return e, fmt.Errorf("%s: api %s, the set says %q", m.ID, clip(e.API), s.API)
	}
	if (e.Value == nil) == (e.Reason == nil) {
		return e, fmt.Errorf("%s: a value or a reason code, not both or neither", m.ID)
	}
	if e.Reason != nil && !KnownReason(*e.Reason) {
		return e, fmt.Errorf("%s: reason %s is not a reason code (%v)", m.ID, clip(*e.Reason), Reasons)
	}
	for name, v := range map[string]float64{"value": deref(e.Value), "spread": deref(e.Spread), "seconds": e.Seconds, "cool_down": e.CoolDown} {
		if v < 0 || math.IsInf(v, 0) {
			return e, fmt.Errorf("%s: %s %v", m.ID, name, v)
		}
	}
	switch {
	case e.Capacity != nil && m.Category != "Storage":
		return e, fmt.Errorf("%s: capacity is kept with Storage values only", m.ID)
	case e.Capacity == nil && m.Category == "Storage" && e.Value != nil:
		return e, fmt.Errorf("%s: a Storage value is kept with the SSD's capacity", m.ID)
	case e.Capacity != nil && (*e.Capacity <= 0 || *e.Capacity > 1<<20):
		return e, fmt.Errorf("%s: capacity %d GB", m.ID, *e.Capacity)
	case e.Fill != nil && e.Capacity == nil:
		return e, fmt.Errorf("%s: fill is kept with a capacity only", m.ID)
	case e.Fill != nil && (*e.Fill < 0 || *e.Fill > 100 || math.IsNaN(*e.Fill)):
		return e, fmt.Errorf("%s: fill %v%%", m.ID, *e.Fill)
	}
	return e, nil
}

func checkConditions(raw json.RawMessage) (string, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return "", fmt.Errorf("conditions: not a JSON object")
	}
	for _, k := range sortedKeys(fields) {
		if !slices.Contains(ConditionFields, k) {
			return "", fmt.Errorf("conditions: field %s is not one the conditions carry (%v)", clip(k), ConditionFields)
		}
		switch v := fields[k].(type) {
		case bool, float64:
		case string:
			if !reWord.MatchString(v) {
				return "", fmt.Errorf("conditions: %s is not a short lowercase word", k)
			}
		default:
			return "", fmt.Errorf("conditions: %s is a boolean, a number or a word", k)
		}
	}
	b, err := json.Marshal(fields)
	return string(b), err
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// clip quotes what a Mac sent, cut short, for an error the Mac reads.
func clip(s string) string {
	if len(s) > 40 {
		s = s[:40] + "..."
	}
	return fmt.Sprintf("%q", s)
}
