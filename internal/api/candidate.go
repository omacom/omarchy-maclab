package api

import (
	"regexp"
	"strings"
)

// A Candidate boot boots a Package set on a Mac without changing its own
// system: lab-agent snapshots @ to @lab-candidate, installs the set's files
// into the snapshot and boots it once through a staged entry. labd sends, in
// order: candidate_stage, boot_once (with the staged Entry), candidate_check
// on the candidate boot until it is confirmed or the deadline passes, then,
// back on the known-good boot, candidate_result when it was not confirmed,
// and candidate_cleanup.
const (
	CmdCandidateStage   = "candidate_stage"
	CmdCandidateCheck   = "candidate_check"
	CmdCandidateResult  = "candidate_result"
	CmdCandidateCleanup = "candidate_cleanup"
)

// The states a Candidate boot ends in.
const (
	CandidateConfirmed  = "candidate confirmed"
	CandidateBootFailed = "boot failed"
	CandidateNotBooted  = "not booted"
	CandidateNotYet     = "not yet"
)

// CandidateSubvolume is the btrfs subvolume a Candidate boot runs from.
const CandidateSubvolume = "@lab-candidate"

// CandidatePackage is one file of a set: a local path or a URL, and the
// sha256 lab-agent checks before installing it. A URL is fetched from labd's
// artifact store by its sha256.
type CandidatePackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// CandidateSet is the Package set a Candidate boot installs.
type CandidateSet struct {
	Name     string             `json:"name"`
	Packages []CandidatePackage `json:"packages"`
	// GPUDriver is the DRM driver the candidate boot must bind the GPU to
	// ("aurora" or "asahi"); "" checks none.
	GPUDriver string `json:"gpu_driver,omitempty"`
}

type CandidateStageArgs struct {
	Set CandidateSet `json:"set"`
	// Sighting is labd's word that the Mac has a Sighting (cabled to a
	// controller). Without it a set carrying m1n1 or U-Boot is refused.
	Sighting               bool `json:"sighting,omitempty"`
	BootTimeoutSec         int  `json:"boot_timeout_sec,omitempty"`
	WatchdogOpenTimeoutSec int  `json:"watchdog_open_timeout_sec,omitempty"`
	// Offline is labd's word that the set is the whole of what is
	// installed: pacman installs its files with no repository to fetch
	// from, and staging fails on a package they need that it lacks.
	Offline bool `json:"offline,omitempty"`
}

type CandidateStageResult struct {
	Entry         string `json:"entry"`
	KernelRelease string `json:"kernel_release"`
	Cmdline       string `json:"cmdline"`
	// BootLoader is a set that changed boot.bin; the old one is kept beside
	// it as boot-prev.bin, and RestoreSteps are what a person does at the Mac
	// when the boot does not come back (labd marks it Needs hands).
	BootLoader   bool   `json:"boot_loader,omitempty"`
	RestoreSteps string `json:"restore_steps,omitempty"`
}

type CandidateCheckArgs struct {
	GPUDriver string `json:"gpu_driver,omitempty"`
}

// CandidateCheck is the candidate boot looked at from inside: State is
// CandidateConfirmed or CandidateNotYet, with what is still missing.
type CandidateCheck struct {
	State       string   `json:"state"`
	Desktop     bool     `json:"desktop"`
	GPUDrivers  []string `json:"gpu_drivers,omitempty"`
	FailedUnits []string `json:"failed_units,omitempty"`
	Problems    []string `json:"problems,omitempty"`
}

// CandidateFailure is read on the known-good boot after a candidate boot that
// was not confirmed: the candidate's own journal, from its subvolume.
// NotBooted is a candidate that never ran: its journal is empty and no early
// unit noted a Lab boot in it, so the Mac booted another entry (or the
// candidate's kernel stopped before its root). BootID and Deadline are the
// early unit's note in the candidate: the boot, and the deadline it rebooted
// it at, if one did. Files are the job files the journal was kept in.
type CandidateFailure struct {
	State     string   `json:"state"`
	Journal   string   `json:"journal"`
	NotBooted bool     `json:"not_booted,omitempty"`
	BootID    string   `json:"boot_id,omitempty"`
	Deadline  Deadline `json:"deadline,omitempty"`
	Files     []string `json:"files,omitempty"`
}

// CandidateResultArgs name the job files the candidate's journal is kept
// in: <Name>/journal.txt ("candidate" when empty).
type CandidateResultArgs struct {
	Name string `json:"name,omitempty"`
}

type CandidateCleanupArgs struct {
	Keep bool `json:"keep,omitempty"`
}

// bootLoaderPackage matches the packages that put m1n1 or U-Boot on a Mac.
var bootLoaderPackage = regexp.MustCompile(`^(m1n1(-.+)?|uboot-.+|u-boot.*|asahi-boot)$`)

// IsBootLoaderPackage is a package that puts m1n1 or U-Boot on the Mac.
func IsBootLoaderPackage(name string) bool { return bootLoaderPackage.MatchString(name) }

// BootLoaderChanges are the set's boot loader packages at another version
// than installed (a Mac's Stack) holds, as "name version": the set changes
// the boot loader every boot shares. A captured set, at the Mac's own
// versions, changes nothing.
func BootLoaderChanges(items []SetItem, installed map[string]string) []string {
	var out []string
	for _, it := range items {
		if !IsBootLoaderPackage(it.Package) {
			continue
		}
		v := it.Version
		if v == "" && it.Source == SetRepo {
			v = it.Ref
		}
		if v == "" || installed[it.Package] != v {
			out = append(out, strings.TrimSpace(it.Package+" "+v))
		}
	}
	return out
}
