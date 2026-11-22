package macosrunner

import (
	"encoding/json"
	"slices"
)

// Reason is a fixed reason code: why a measurement has no value. "" is
// none, null in JSON. The spellings are internal/perf's, which labd checks.
type Reason string

const (
	NoBuild            Reason = "no_build"
	NoDriver           Reason = "no_driver"
	FellBack           Reason = "fell_back"
	NoComparableSensor Reason = "no_comparable_sensor"
	NotRun             Reason = "not_run"
	Skipped            Reason = "skipped"
	Error              Reason = "error"
	DiskTooFull        Reason = "disk_too_full"
	NotOnAC            Reason = "not_on_ac"
	NotIdle            Reason = "not_idle"
	SoftwareRenderer   Reason = "software_renderer"
	PlaceholderFile    Reason = "placeholder_file"
	BatteryCharging    Reason = "battery_charging"
)

// Reasons are every reason code.
var Reasons = []Reason{NoBuild, NoDriver, FellBack, NoComparableSensor, NotRun, Skipped, Error,
	DiskTooFull, NotOnAC, NotIdle, SoftwareRenderer, PlaceholderFile, BatteryCharging}

// Known is whether r is one of the fixed codes, or none.
func (r Reason) Known() bool { return r == "" || slices.Contains(Reasons, r) }

// Fails is whether a result with this reason fails the run.
func (r Reason) Fails() bool { return r == Error || r == SoftwareRenderer }

func (r Reason) MarshalJSON() ([]byte, error) {
	if r == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(r))
}

func (r *Reason) UnmarshalJSON(b []byte) error {
	var s *string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*r = ""
	if s != nil {
		*r = Reason(*s)
	}
	return nil
}

// Result is one measurement of bench.json, the same shape as Omarchy's:
// only these fields, every string taken from the Benchmark set and never
// from a tool's output. Capacity (the SSD, in whole GB) and Fill (percent
// of the disk full, the scratch file in place) are on Storage rows only.
type Result struct {
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
	Reason         Reason   `json:"reason"`
}

// ResultFields are the only fields a Result carries.
var ResultFields = []string{"set", "metric", "category", "version", "value", "unit", "higher_is_better",
	"spread", "seconds", "cool_down", "api", "capacity", "fill", "reason"}

// Conditions is bench-conditions.json: the conditions the run measured in.
type Conditions struct {
	OnAC      bool     `json:"on_ac"`
	CPUBusy   float64  `json:"cpu_busy_percent"`
	Backlight *float64 `json:"backlight_percent,omitempty"`
	// LowPowerMode is whether Low Power Mode was on while the run measured.
	LowPowerMode bool     `json:"low_power_mode"`
	RefreshHz    *float64 `json:"refresh_hz,omitempty"`
	FreePercent  *float64 `json:"free_percent,omitempty"`
	// Restored is whether everything the run changed was put back; it goes
	// to the private log, not to labd.
	Restored bool `json:"-"`
}

// ConditionFields are the only fields Conditions carries.
var ConditionFields = []string{"on_ac", "cpu_busy_percent", "backlight_percent", "low_power_mode", "refresh_hz", "free_percent"}

// The states an upload ends its run with.
const (
	Succeeded = "succeeded"
	Failed    = "failed"
)

// Upload is what the runner sends labd at the end of a macOS run: its
// bench.json and bench-conditions.json, and whether it succeeded. RunID is
// only for labd's older endpoint (/api/macos/runner/results), which finds
// the run by it; the Performance one has the run in its path.
type Upload struct {
	RunID      string     `json:"run_id,omitempty"`
	Entries    []Result   `json:"entries"`
	Conditions Conditions `json:"conditions"`
	State      string     `json:"state"`

	set, list string
	reason    Reason // the whole run's, when it measured nothing
}
