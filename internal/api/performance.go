package api

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
	// Rerun is `lab perf rerun`, under its caller's Lease; LeaseTaken a
	// Lease labd took for it, which ends with the job.
	Rerun      bool `json:"rerun,omitempty"`
	LeaseTaken bool `json:"lease_taken,omitempty"`
}

// BenchRun is the bench builtin's task in a Performance run: the metrics of
// the set to run, in order, on this system, and the attempt they upload to.
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
