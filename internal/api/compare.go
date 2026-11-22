package api

import "time"

// A Compare runs the same benchmarks and omarchy-m-test on one Mac on two
// sides: A, the Mac's known-good system or a Package set, and B, a Package
// set, each set through a Candidate boot; and, when asked, in macOS too. The
// report is private to Maintainers until one publishes it.

// CompareCurrent as Before is the Mac's own known-good system, with no
// Candidate boot.
const CompareCurrent = "current"

// CompareRequest asks for a Compare (POST /api/compares).
type CompareRequest struct {
	Mac    string `json:"mac"`
	Before string `json:"before"` // a set's name, or CompareCurrent
	After  string `json:"after"`  // a set's name
	MacOS  bool   `json:"macos,omitempty"`
	Tier   string `json:"tier,omitempty"` // TierDefault or TierExtended
	Hands  bool   `json:"hands,omitempty"`
}

// CompareSpec is a Compare job's part of its JobSpec: what was asked, and
// the sets as they were when it was asked (a set never changes).
type CompareSpec struct {
	Before    string      `json:"before"`
	After     string      `json:"after"`
	MacOS     bool        `json:"macos,omitempty"`
	Tier      string      `json:"tier"`
	Hands     bool        `json:"hands,omitempty"`
	BeforeSet *PackageSet `json:"before_set,omitempty"` // nil for CompareCurrent
	AfterSet  *PackageSet `json:"after_set"`
	// Bench is the Benchmark set current when the Compare was asked: with
	// MacOS, its macOS side is the Mac's macOS result of this set.
	Bench string `json:"bench,omitempty"`
	// LeaseTaken is a Lease labd took for the job, since its person held
	// none on the Mac: it ends with the job.
	LeaseTaken bool `json:"lease_taken,omitempty"`
}

// The sides of a Compare.
const (
	SideA     = "A"
	SideB     = "B"
	SideMacOS = "macOS"
)

// A benchmark's verdict, B (or macOS) against A.
const (
	VerdictBetter      = "better"
	VerdictWorse       = "worse"
	VerdictNoChange    = "no change"
	VerdictNotCompared = "not compared"
)

// CompareSide is one side as it ran.
type CompareSide struct {
	Side string `json:"side"`
	Set  string `json:"set"` // a set's name, CompareCurrent, or "macOS"
	// Boot is how the side booted: "known-good", CandidateConfirmed,
	// CandidateBootFailed, CandidateNotBooted (the candidate never ran), or
	// for macOS the run's state.
	Boot        string             `json:"boot,omitempty"`
	Kernel      string             `json:"kernel,omitempty"`
	BootSeconds float64            `json:"boot_seconds,omitempty"`
	Packages    []CandidatePackage `json:"packages,omitempty"` // every file the side installed, by sha256
	GPUFirmware string             `json:"gpu_firmware,omitempty"`
	SIO         string             `json:"sio,omitempty"`
	Thunderbolt string             `json:"thunderbolt,omitempty"`
	FailedUnits []string           `json:"failed_units,omitempty"`
	// Journal is a candidate boot's own journal, read back on known-good
	// after a boot that was not confirmed.
	Journal string        `json:"journal,omitempty"`
	Bench   []BenchResult `json:"bench,omitempty"`
	OMT     *CompareOMT   `json:"omt,omitempty"`
	Error   string        `json:"error,omitempty"`
}

// CompareOMT is one side's omarchy-m-test run.
type CompareOMT struct {
	Tool   string            `json:"tool,omitempty"`
	Pass   int               `json:"pass"`
	Fail   int               `json:"fail"`
	Skip   int               `json:"skip"`
	Status map[string]string `json:"status,omitempty"` // check id -> pass, fail or skip
}

// CompareRow is one benchmark across the sides. Delta is B against A in
// percent; Noise is the Mac's, in percent, from NoiseFrom.
type CompareRow struct {
	Name           string   `json:"name"`
	Unit           string   `json:"unit,omitempty"`
	HigherIsBetter bool     `json:"higher_is_better"`
	A              *float64 `json:"a"`
	B              *float64 `json:"b"`
	VersionA       string   `json:"version_a,omitempty"`
	VersionB       string   `json:"version_b,omitempty"`
	Delta          *float64 `json:"delta_pct"`
	Noise          float64  `json:"noise_pct"`
	NoiseFrom      string   `json:"noise_from"`
	Verdict        string   `json:"verdict"`
	Note           string   `json:"note,omitempty"`
	MacOS          *float64 `json:"macos,omitempty"`
	MacOSDelta     *float64 `json:"macos_delta_pct,omitempty"` // macOS against A
	MacOSVerdict   string   `json:"macos_verdict,omitempty"`
	MacOSNote      string   `json:"macos_note,omitempty"`
}

// The change of one omarchy-m-test check from A to B.
const (
	CheckPass      = "pass"
	CheckFail      = "fail"
	CheckRegressed = "regressed"
	CheckFixed     = "fixed"
	// CheckNotCompared is a check a Candidate boot itself makes fail: its
	// Note says why.
	CheckNotCompared = "not compared"
)

// CompareCheck is one omarchy-m-test check on both sides.
type CompareCheck struct {
	ID     string `json:"id"`
	A      string `json:"a"`
	B      string `json:"b"`
	Change string `json:"change"` // CheckPass, CheckFail, CheckRegressed, CheckFixed or CheckNotCompared
	Note   string `json:"note,omitempty"`
}

// CompareHands is the pause for a person a Compare asked for (--hands).
type CompareHands struct {
	Waiting bool       `json:"waiting,omitempty"`
	Done    *time.Time `json:"done,omitempty"`
	By      string     `json:"by,omitempty"`
	Note    string     `json:"note,omitempty"`
}

// CompareResult is a Compare's report.
type CompareResult struct {
	Before      string         `json:"before"`
	After       string         `json:"after"`
	Tier        string         `json:"tier"`
	Sides       []CompareSide  `json:"sides"`
	Rows        []CompareRow   `json:"rows,omitempty"`
	Checks      []CompareCheck `json:"checks,omitempty"`
	Regressed   int            `json:"regressed"`
	Fixed       int            `json:"fixed"`
	Hands       *CompareHands  `json:"hands,omitempty"`
	Published   *time.Time     `json:"published,omitempty"`
	PublishedBy string         `json:"published_by,omitempty"`
}

// Side is the named side, or nil.
func (r *CompareResult) Side(name string) *CompareSide {
	for i := range r.Sides {
		if r.Sides[i].Side == name {
			return &r.Sides[i]
		}
	}
	return nil
}

// Agent commands a Compare adds to a Candidate boot's.
const (
	// CmdCandidateResolve finds, on the Mac's known-good system, the file
	// of each repo package of a set at its exact version: in pacman's cache,
	// else downloaded from the Mac's repositories (pacman -Sw). The answer
	// names each file by its local path and sha256, as candidate_stage takes
	// it.
	CmdCandidateResolve = "candidate_resolve"
	// CmdBootMacOS is lab-agent boot-macos-once sent by labd: it tells labd
	// the queued macOS run started, sets macOS as the next boot only and
	// reboots once the command's result is in.
	CmdBootMacOS = "boot_macos_once"
)

// CandidateResolveArgs names the repo packages to find, by name and version.
type CandidateResolveArgs struct {
	Packages []CandidatePackage `json:"packages"`
}

// CandidateResolved is each package with URL (a local path) and SHA256.
type CandidateResolved struct {
	Packages []CandidatePackage `json:"packages"`
}
