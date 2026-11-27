package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// A Performance run (spec section 22, "Performance run") is a job of the Mac
// that measures one Release with one Benchmark set: the Release's frozen
// manifest, as a Package set, booted as a Candidate boot, then the bench
// builtin runs the metrics labd chose and uploads each result to the run's
// attempt, POST /api/agent/performance/{run}/results.

// PerformanceSpec is what a Performance run measures. Set and Metrics are
// read when it is queued and kept in the job, as Compare keeps its sets.
type PerformanceSpec struct {
	Release        string `json:"release"`
	ManifestSHA256 string `json:"manifest_sha256"`
	// Kernel is the Release's kernel release: the job measures only once
	// the running kernel is it.
	Kernel string `json:"kernel"`
	// Packages is the Release's Package set, release-<version>, every item
	// a file in the artifact store by its sha256.
	Packages *PackageSet `json:"packages"`
	// Bench is the Benchmark set, List its quick or full list, and Metrics
	// the metric ids of that list the run measures, in the set's order.
	Bench   string   `json:"bench"`
	List    string   `json:"list"`
	Metrics []string `json:"metrics"`
	// Adds are the packages the lab adds to the Release for the metrics,
	// by name, sorted (the metrics' sets.Metric.AddsKey): the run boots the
	// Release's set with them, release-<version>+<adds>, frozen the first
	// time a Mac resolves them, and its values are kept apart from the
	// Release's, under PerfTarget.
	Adds []string `json:"adds,omitempty"`
	// Rerun is `lab perf rerun`, under its caller's Lease; LeaseTaken a
	// Lease labd took for it, which ends with the job.
	Rerun      bool `json:"rerun,omitempty"`
	LeaseTaken bool `json:"lease_taken,omitempty"`
}

// BenchRun is the bench builtin's task in a Performance run: the metrics of
// the set to run, in order, on this system, and the attempt they upload to.
// A Compare's has no Run: labd reads its bench.json with the test's files.
// Each metric's files are the set's for Omarchy; the Mac fetches them from
// labd by sha256 while the job runs. The upload carries only the fields and
// reason codes of internal/perf, and ends the attempt with succeeded or
// failed once every metric has its result.
type BenchRun struct {
	Set     string   `json:"set"`
	List    string   `json:"list"`
	Metrics []string `json:"metrics"`
	Run     int64    `json:"run"`
}

// BenchUpload is what became of the bench builtin's upload to its
// Performance run: Taken when labd took it, with how many results were new
// and how many it had, and the state the upload ended the attempt with;
// else Error, after the last of Tries.
type BenchUpload struct {
	Run     int64  `json:"run"`
	Taken   bool   `json:"taken"`
	State   string `json:"state,omitempty"`
	Results int    `json:"results"`
	Already int    `json:"already,omitempty"`
	Tries   int    `json:"tries"`
	Error   string `json:"error,omitempty"`
}

// PerformanceResult is what a Performance run did: its attempt, the kernel
// it measured on, and the versions it booted, by exact package name.
type PerformanceResult struct {
	Run     int64             `json:"run,omitempty"`
	Attempt int               `json:"attempt,omitempty"`
	State   string            `json:"state,omitempty"`
	Kernel  string            `json:"kernel,omitempty"`
	Booted  map[string]string `json:"booted,omitempty"`
	Error   string            `json:"error,omitempty"`
}

// PerfRerunRequest is `lab perf rerun <mac> <release>` (POST /api/perf/rerun).
type PerfRerunRequest struct {
	Mac     string `json:"mac"`
	Release string `json:"release"`
}

// PerfPackageSet is the name of a Release's Package set.
func PerfPackageSet(release string) string { return "release-" + release }

// PerfTarget is what a Performance run's Omarchy values are kept under: the
// Release, or with packages the lab added for them, the Release and those
// packages, "7.3+omarchy-mlx", apart from the Release's own.
func PerfTarget(release string, adds []string) string {
	if len(adds) == 0 {
		return release
	}
	return release + "+" + strings.Join(adds, "+")
}

// PerfAddsSet is the name of the Package set of the Release with the
// packages the lab adds, release-<version>-adds-<12 hex digits of their
// names' sha256>: a set's name holds no "+".
func PerfAddsSet(release string, adds []string) string {
	sum := sha256.Sum256([]byte(strings.Join(adds, "+")))
	return PerfPackageSet(release) + "-adds-" + hex.EncodeToString(sum[:6])
}

// AddsResolveArgs asks lab-agent, on a Lab-owned Mac's known-good system,
// for every package file the packages need beyond what the Mac has
// installed: their names, as pacman takes them.
type AddsResolveArgs struct {
	Packages []string `json:"packages"`
}

// AddsResolved is each file pacman downloaded for the packages, its
// signature checked by pacman, uploaded to the job in parts of at most
// AddsPart bytes, "adds/<file>.<n>", n from 0.
type AddsResolved struct {
	Files []AddsFile `json:"files"`
}

// AddsFile is one package file of the added packages.
type AddsFile struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Parts   int    `json:"parts"`
}

// AddsPart is the most a part of an added package file holds: under the
// 90 MB a job file may be. Tests make it smaller.
var AddsPart int64 = 64 << 20

// CmdAddsResolve is labd asking lab-agent to resolve the packages the lab
// adds (AddsResolveArgs, answered with AddsResolved).
const CmdAddsResolve = "adds_resolve"
