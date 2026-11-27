package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// The simulated Mac's Candidate boots. A set's candidate boots its own
// kernel when it carries a linux-aurora package (its release is the
// package's version), else the known-good one. SetCandidateBehavior picks
// how a set's candidate boot goes: "ok" (confirmed), "unconfirmed" (it boots
// but never confirms: its desktop never comes up), "skipped" (the boot
// loader boots the default entry instead: the candidate never runs), or any kernel behavior
// of the Sim ("panic" falls back to known-good at once, "omt-regress"
// confirms but fails Wi-Fi in omarchy-m-test).

// SetCandidateBehavior sets how the named set's candidate boots go.
func (s *Sim) SetCandidateBehavior(set, behavior string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidateBehavior == nil {
		s.candidateBehavior = map[string]string{}
	}
	s.candidateBehavior[set] = behavior
}

// SetRepoMissing makes the simulated repositories lack the package.
func (s *Sim) SetRepoMissing(pkg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.repoMissing == nil {
		s.repoMissing = map[string]bool{}
	}
	s.repoMissing[pkg] = true
}

// CandidateOps lists what was done of Candidate boots, in order: "stage SET",
// "check", "result", "cleanup", "resolve PKG=VER".
func (s *Sim) CandidateOps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.candidateOps)
}

func (s *Sim) candidateOp(op string) {
	s.mu.Lock()
	s.candidateOps = append(s.candidateOps, op)
	s.mu.Unlock()
}

// simRepoFile is what the simulated repositories hold for a package.
func simRepoFile(name, version string) string { return "sim package " + name + " " + version + "\n" }

func (s *Sim) ResolveCandidate(ctx context.Context, a api.CandidateResolveArgs) (api.CandidateResolved, error) {
	var out api.CandidateResolved
	dir := filepath.Join(s.WorkDir, "pacman-cache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, err
	}
	for _, p := range a.Packages {
		s.candidateOp("resolve " + p.Name + "=" + p.Version)
		s.mu.Lock()
		missing := s.repoMissing[p.Name]
		s.mu.Unlock()
		if missing {
			return out, fmt.Errorf("%s %s: in neither pacman's cache nor the Mac's repositories", p.Name, p.Version)
		}
		path := filepath.Join(dir, p.Name+"-"+p.Version+"-aarch64.pkg.tar.zst")
		if err := os.WriteFile(path, []byte(simRepoFile(p.Name, p.Version)), 0o644); err != nil {
			return out, err
		}
		sum, err := fileSHA256(path)
		if err != nil {
			return out, err
		}
		p.URL, p.SHA256 = path, sum
		out.Packages = append(out.Packages, p)
	}
	return out, nil
}

// ResolveAdds is the simulated repositories' files for the packages: each
// package and one it needs, "<package>-lib", as a file of its own; a
// package the repositories lack fails.
func (s *Sim) ResolveAdds(ctx context.Context, a api.AddsResolveArgs) (string, []api.AddsFile, error) {
	if err := checkAdds(a.Packages); err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp(s.WorkDir, "adds-")
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(tmp, "files")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	for _, p := range a.Packages {
		s.candidateOp("resolve adds " + p)
		s.mu.Lock()
		missing := s.repoMissing[p]
		s.mu.Unlock()
		if missing {
			os.RemoveAll(tmp)
			return "", nil, fmt.Errorf("pacman -Sw %s: error: target not found: %s", p, p)
		}
		for _, n := range []string{p, p + "-lib"} {
			if err := os.WriteFile(filepath.Join(dir, n+"-1.0-1-aarch64.pkg.tar.zst"), []byte(simRepoFile(n, "1.0-1")), 0o644); err != nil {
				return "", nil, err
			}
		}
	}
	files, err := addsFiles(dir)
	if err != nil {
		os.RemoveAll(tmp)
	}
	return dir, files, err
}

func (s *Sim) StageCandidate(ctx context.Context, job string, a api.CandidateStageArgs, fetch Fetcher) (api.CandidateStageResult, error) {
	var res api.CandidateStageResult
	if a.Offline {
		s.candidateOp("stage " + a.Set.Name + " offline")
	} else {
		s.candidateOp("stage " + a.Set.Name)
	}
	if len(a.Set.Packages) == 0 {
		return res, errors.New("the set has no packages")
	}
	dir := filepath.Join(s.WorkDir, "candidate-files", job)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	defer os.RemoveAll(dir)
	krel := ""
	for _, p := range a.Set.Packages {
		if !reSHA256.MatchString(p.SHA256) {
			return res, fmt.Errorf("bad package %q (sha256 %q)", p.Name, p.SHA256)
		}
		if api.IsBootLoaderPackage(p.Name) && s.stackVersion(p.Name) != p.Version {
			res.BootLoader = true
		}
		dst := filepath.Join(dir, p.Name+".pkg.tar.zst")
		var err error
		if strings.HasPrefix(p.URL, "/") {
			err = copyFile(p.URL, dst)
		} else {
			err = fetch(ctx, p.SHA256, dst)
		}
		if err == nil {
			var got string
			if got, err = fileSHA256(dst); err == nil && got != p.SHA256 {
				err = fmt.Errorf("sha256 is %s, want %s", got, p.SHA256)
			}
		}
		if err != nil {
			return res, fmt.Errorf("%s %s: %w", p.Name, p.Version, err)
		}
		if p.Name == "linux-aurora" {
			krel = p.Version
		}
	}
	if res.BootLoader && !a.Sighting {
		return res, errors.New("the set changes the boot loader (m1n1 or U-Boot), and this Mac has no Sighting: refused")
	}
	s.mu.Lock()
	if krel == "" {
		krel = s.knownGoodLocked()
	}
	behavior := s.candidateBehavior[a.Set.Name]
	if behavior == "" {
		behavior = "ok"
	}
	base := s.cmdline
	s.mu.Unlock()
	cmdline := testCmdline(base+" rootflags=subvol="+api.CandidateSubvolume, api.StageArgs{BootTimeoutSec: a.BootTimeoutSec, WatchdogOpenTimeoutSec: a.WatchdogOpenTimeoutSec}, job)
	e := simEntry{krel: krel, behavior: behavior, cmdline: cmdline, stack: map[string]string{}}
	for _, p := range a.Set.Packages {
		if api.BootedPackage(p.Name) {
			e.stack[p.Name] = p.Version
		}
	}
	if behavior == "ok" || behavior == "unconfirmed" || behavior == "skipped" {
		e.behavior = "candidate-" + behavior
	}
	s.mu.Lock()
	s.staged[job] = e
	s.mu.Unlock()
	if res.BootLoader {
		res.RestoreSteps = candidateRestoreSteps
	}
	res.Entry, res.KernelRelease, res.Cmdline = entryPrefix+job, krel, cmdline
	return res, nil
}

func (s *Sim) CheckCandidate(ctx context.Context, a api.CandidateCheckArgs) (api.CandidateCheck, error) {
	s.candidateOp("check")
	s.mu.Lock()
	cmdline, b := s.cmdline, s.behavior
	s.mu.Unlock()
	if !IsCandidateBoot(cmdline) {
		return api.CandidateCheck{}, errors.New("this is not a candidate boot")
	}
	c := api.CandidateCheck{State: api.CandidateConfirmed, Desktop: true, GPUDrivers: []string{"asahi"}}
	if b == "candidate-unconfirmed" {
		c.State, c.Desktop, c.Problems = api.CandidateNotYet, false, []string{"the desktop session is not up"}
	}
	return c, nil
}

func (s *Sim) CandidateResult(ctx context.Context) (api.CandidateFailure, error) {
	s.candidateOp("result")
	_, _, cmdline := s.Identity()
	if IsCandidateBoot(cmdline) {
		return api.CandidateFailure{}, errors.New("still on the candidate boot")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidateSkipped {
		return api.CandidateFailure{State: api.CandidateBootFailed, Journal: "-- No entries --\n", NotBooted: true}, nil
	}
	return api.CandidateFailure{State: api.CandidateBootFailed, Journal: "sim candidate journal: gdm: no session\n"}, nil
}

func (s *Sim) CleanupCandidate(ctx context.Context, job string, keep bool) error {
	s.candidateOp("cleanup")
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.staged, job)
	return nil
}

// BootMacOS is boot-macos-once on the simulated Mac: the next boot is
// macOS, for MacOSFor, then the Mac is back on its own system; labd is told
// once it is set, and a refusal puts the next boot back.
func (s *Sim) BootMacOS(ctx context.Context, started func() error) error {
	s.mu.Lock()
	fails := s.BlessFails
	if fails == nil {
		s.macOSNext = true
	}
	s.mu.Unlock()
	if fails != nil {
		return fails
	}
	if err := started(); err != nil {
		s.mu.Lock()
		s.macOSNext = false
		s.mu.Unlock()
		return err
	}
	return nil
}

// macOSBoot is the boot of generation gen spent in macOS, if it is one.
func (s *Sim) macOSBoot(gen int) {
	s.mu.Lock()
	if !s.macOSNext || gen != s.gen {
		s.mu.Unlock()
		return
	}
	s.macOSNext = false
	in, d := s.InMacOS, s.MacOSFor
	s.mu.Unlock()
	if d == 0 {
		d = time.Second
	}
	if in != nil {
		go in()
	}
	s.Clock.Sleep(d)
}

// stackVersion is the version of name the sim reports installed.
func (s *Sim) stackVersion(name string) string {
	s.mu.Lock()
	st := s.Stack
	s.mu.Unlock()
	if st == nil {
		st = simStack
	}
	return st[name]
}
