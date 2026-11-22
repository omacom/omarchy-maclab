package perf

import "slices"

// The fixed reason codes a result carries in place of a value. A tool's own
// text never goes in a result: it can carry a user name or a path. lab-agent's
// bench runner and the macOS runner write these spellings, and labd refuses
// any other.
const (
	NoBuild            = "no_build"
	NoDriver           = "no_driver"
	FellBack           = "fell_back"
	NoComparableSensor = "no_comparable_sensor"
	NotRun             = "not_run"
	Skipped            = "skipped"
	Error              = "error"
	DiskTooFull        = "disk_too_full"
	NotOnAC            = "not_on_ac"
	NotIdle            = "not_idle"
	SoftwareRenderer   = "software_renderer"
	PlaceholderFile    = "placeholder_file"
	BatteryCharging    = "battery_charging"
)

// Reasons is every reason code.
var Reasons = []string{
	NoBuild, NoDriver, FellBack, NoComparableSensor, NotRun, Skipped,
	Error, DiskTooFull, NotOnAC, NotIdle, SoftwareRenderer, PlaceholderFile,
	BatteryCharging,
}

// KnownReason is whether r is one of the fixed reason codes.
func KnownReason(r string) bool { return slices.Contains(Reasons, r) }

// passing are the reasons a benchmark did not run that may not hold at the
// Mac's next slot: what the Stack or the hardware lacks does not pass.
var passing = []string{NotRun, Skipped, DiskTooFull, NotOnAC, NotIdle}

// RunAgain is whether a benchmark with reason r failed or was skipped for a
// passing reason, and so is run once more, alone, at the Mac's next slot.
func RunAgain(r string) bool { return r == Error || slices.Contains(passing, r) }
