package api

import "time"

// macOS runs: the same benchmarks, run in macOS on the same Mac. labd queues
// a run; lab-agent boot-macos-once boots macOS once (asahi-bless --next);
// lab-macos-runner, a LaunchDaemon enrolled once per Mac, asks labd at boot
// whether a run waits, runs it, uploads the results and reboots back.

// Tiers of a benchmark run, as on Linux.
const (
	TierDefault  = "default"
	TierExtended = "extended"
)

// States of a macOS run.
const (
	MacOSQueued     = "queued"      // waiting for boot-macos-once
	MacOSStarted    = "started"     // the Mac is booting into macOS
	MacOSRunning    = "running"     // the runner took it
	MacOSDone       = "done"        // the results are in
	MacOSNeedsHands = "needs_hands" // the runner did not report within the deadline
)

// NoMacOSEquivalent is the note of a benchmark with no macOS build.
const NoMacOSEquivalent = "no macOS equivalent"

// BenchResult is one benchmark's result, the shape of the Linux runner's.
// Value is null when the benchmark did not run; Note says why. API names a
// different graphics or video API than the Linux run's (Metal, VideoToolbox):
// such a number is labelled, not compared as like for like.
type BenchResult struct {
	Name           string   `json:"name"`
	Version        string   `json:"version"`
	Value          *float64 `json:"value"`
	Unit           string   `json:"unit"`
	HigherIsBetter bool     `json:"higher_is_better"`
	Spread         float64  `json:"spread"`
	Seconds        float64  `json:"seconds"`
	API            string   `json:"api,omitempty"`
	Note           string   `json:"note,omitempty"`
}

// MacOSArtifact is a pinned file of the benchmark bundle in labd's artifact
// store: the runner fetches it by sha256 and checks it.
type MacOSArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// MacOSRun is a Mac's macOS run as labd keeps it.
type MacOSRun struct {
	ID        string          `json:"id"`
	Mac       string          `json:"mac"`
	Tier      string          `json:"tier"`
	Set       string          `json:"set,omitempty"` // the Benchmark set whose list it runs
	State     string          `json:"state"`
	Artifacts []MacOSArtifact `json:"artifacts,omitempty"`
	Queued    time.Time       `json:"queued"`
	Started   *time.Time      `json:"started,omitempty"`
	Deadline  *time.Time      `json:"deadline,omitempty"`
	Done      *time.Time      `json:"done,omitempty"`
	Results   []BenchResult   `json:"results,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// MacOSRunRequest queues a macOS run (POST /api/devices/{name}/macos-runs).
type MacOSRunRequest struct {
	Tier string `json:"tier,omitempty"`
}

// MacOSEnrollToken is a one-time token for one Mac's runner and the line
// that enrolls it, to run in macOS (POST /api/macos/enroll-tokens).
type MacOSEnrollToken struct {
	Mac     string    `json:"mac"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
	Line    string    `json:"line"`
}

// MacOSEnrolled is labd's answer to a runner's enrolment: its own secret.
type MacOSEnrolled struct {
	Mac    string `json:"mac"`
	Secret string `json:"secret"`
}

// Whether a Mac's macOS needs a person at it: an Attended Mac waits for its
// password at the screen; an Unattended Mac starts and logs in alone.
const (
	MacOSAttended   = "attended"
	MacOSUnattended = "unattended"
)

// MacOSAttendance is the runner's word after a boot: whether that boot
// reached its user's desktop with nobody typing (POST
// /api/macos/runner/attendance).
type MacOSAttendance struct {
	Unattended bool `json:"unattended"`
}

// MacOSSide is a Mac's macOS side as labd keeps it (GET
// /api/devices/{name}/macos): whether the runner of its enrollment is
// enrolled, Attended or Unattended, and when the runner last said so.
type MacOSSide struct {
	Mac        string     `json:"mac"`
	Enrolled   bool       `json:"enrolled"`
	Attendance string     `json:"attendance"`
	Said       *time.Time `json:"said,omitempty"`
}

// MacOSPoll is labd's answer to a runner's poll: the run to do, or none.
type MacOSPoll struct {
	Run *MacOSRun `json:"run"`
}

// MacOSResults is what the runner uploads.
type MacOSResults struct {
	RunID   string        `json:"run_id"`
	Results []BenchResult `json:"results"`
	Error   string        `json:"error,omitempty"`
}
