package server

// Packages the lab adds (spec section 22, "Packages the lab adds"): a
// metric whose Omarchy side names packages runs on a Candidate boot of the
// Release's set with them added, release-<version>-adds-<…>. The first
// Lab-owned Mac that needs that set resolves it: lab-agent downloads, with
// pacman, every file the packages need beyond what the Mac has installed and
// sends each in parts; labd joins them into the artifact store, checks each
// sha256 and size, and keeps the set, frozen. Every Mac after boots that
// set, offline. Its values are kept under the Release with the packages,
// api.PerfTarget, never as the Release's own.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// addsResolveTimeout is how long a Mac has to download the packages the lab
// adds and send them to labd.
var addsResolveTimeout = 45 * time.Minute

// perfGroups are the metrics by the packages the lab adds for them, in the
// set's order of their first metric: the Release's own (no adds) first.
func perfGroups(ms []sets.Metric) (keys []string, by map[string][]sets.Metric) {
	by = map[string][]sets.Metric{}
	for _, m := range ms {
		if m.Side(sets.Omarchy) == nil {
			continue
		}
		k := m.AddsKey()
		if _, ok := by[k]; !ok {
			keys = append(keys, k)
		}
		by[k] = append(by[k], m)
	}
	slices.SortStableFunc(keys, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == "":
			return -1
		case b == "":
			return 1
		}
		return 0
	})
	return keys, by
}

// addsOf is the packages a group of metrics adds, sorted.
func addsOf(key string) []string {
	if key == "" {
		return nil
	}
	return strings.Split(key, "+")
}

// keptAddsSet is the Release's set with the packages added, when a Mac has
// resolved it before; nil when none has.
func (s *Server) keptAddsSet(rel api.Release, adds []string) *api.PackageSet {
	set, err := s.packageSet(api.PerfAddsSet(rel.Name, adds))
	if err != nil {
		return nil
	}
	return set
}

// addsRetry is how long labd waits, after a Mac could not resolve a set of
// packages for a Release (one Omarchy does not ship yet, say), before a
// Lab-owned Mac tries again. Such a try is no attempt of the metrics.
var addsRetry = 24 * time.Hour

// perfOfflineAgent is the first lab-agent that stages a Performance run
// offline: an older one would fetch what the set lacks, so it takes no
// Performance run.
const perfOfflineAgent = "0.12.0"

// The most a Mac may send of the packages the lab adds: files, bytes of one,
// bytes of all.
const (
	addsMaxFiles = 64
	addsMaxFile  = 2 << 30
	addsMaxAll   = 6 << 30
)

func addsWaitKey(release string, adds []string) string {
	return api.PerfTarget(release, adds)
}

// addsWaiting is whether labd waits before resolving the packages for the
// Release again.
func (s *Server) addsWaiting(release string, adds []string) bool {
	at, ok := s.addsWait.Load(addsWaitKey(release, adds))
	return ok && s.now().Sub(at.(time.Time)) < addsRetry
}

// perfAddsSet is the set a Performance run that adds packages boots: kept
// from the first Mac that resolved it, else resolved now on this one, a
// Lab-owned Mac, from the Release's set base.
func (r *jobRun) perfAddsSet(ctx context.Context, rel api.Release, base *api.PackageSet, adds []string) (*api.PackageSet, error) {
	if set := r.s.keptAddsSet(rel, adds); set != nil {
		return set, nil
	}
	if r.v.snapshot().IsVolunteer() {
		return nil, refuse(409, "no Lab-owned Mac has resolved %s for Release %s yet: a Volunteer's Mac never chooses what the lab keeps", strings.Join(adds, ", "), rel.Name)
	}
	defer os.RemoveAll(filepath.Join(r.s.jobDir(r.j.ID), "adds"))
	r.ev("resolving the packages the lab adds, %s, on this Mac (pacman -Sw against a copy of its database)", strings.Join(adds, ", "))
	var got api.AddsResolved
	if err := r.v.call(ctx, api.CmdAddsResolve, r.j.ID, api.AddsResolveArgs{Packages: adds}, addsResolveTimeout, &got); err != nil {
		return nil, fmt.Errorf("resolving %s: %s", strings.Join(adds, ", "), strings.TrimPrefix(err.Error(), api.CmdAddsResolve+": "))
	}
	set, err := addsSetOf(rel, base, adds, got, r.s.now())
	if err != nil {
		return nil, err
	}
	// The collector must not take a file between its join and the set that
	// pins it.
	gcMu.Lock()
	defer gcMu.Unlock()
	for _, f := range got.Files {
		if err := r.s.joinAddsFile(r.j.ID, f); err != nil {
			return nil, err
		}
	}
	if _, err := r.s.store.addSet(set); err != nil {
		return nil, err
	}
	r.ev("kept Package set %s: Release %s's set and %d %s for %s", set.Name, rel.Name, len(got.Files),
		plural(len(got.Files), "package", "packages"), strings.Join(adds, ", "))
	return r.s.packageSet(set.Name)
}

// addsSetOf is the Release's set base with the files resolved for adds, or
// why it cannot be: a file that is no package, too many or too big, every
// added package among the files, and none the Release's set holds at
// another version.
func addsSetOf(rel api.Release, base *api.PackageSet, adds []string, got api.AddsResolved, now time.Time) (*api.PackageSet, error) {
	set := &api.PackageSet{Name: api.PerfAddsSet(rel.Name, adds), CreatedBy: "lab", Created: now.UTC(),
		Release: rel.Name, ManifestSHA256: base.ManifestSHA256, Items: slices.Clone(base.Items)}
	if len(got.Files) > addsMaxFiles {
		return nil, fmt.Errorf("the Mac sent %d package files, more than the %d the lab takes", len(got.Files), addsMaxFiles)
	}
	have := map[string]bool{}
	var all int64
	for _, f := range got.Files {
		if !reSHA.MatchString(f.SHA256) || f.Size <= 0 || f.Size > addsMaxFile || f.Parts < 1 || f.Parts != int((f.Size+api.AddsPart-1)/api.AddsPart) ||
			f.File != filepath.Base(f.File) || !strings.HasPrefix(f.File, f.Name+"-"+f.Version+"-") || !strings.Contains(f.File, ".pkg.tar") || have[f.Name] {
			return nil, fmt.Errorf("the Mac sent %q, which is not a package file as the lab adds them", f.File)
		}
		if all += f.Size; all > addsMaxAll {
			return nil, fmt.Errorf("the Mac sent more than the %d GiB of package files the lab takes", addsMaxAll>>30)
		}
		dup := false
		for _, it := range base.Items {
			if it.Package != f.Name {
				continue
			}
			if it.Version != f.Version {
				return nil, fmt.Errorf("%s needs %s %s, and Release %s's set holds %s %s: the lab adds nothing that changes the Release",
					strings.Join(adds, ", "), f.Name, f.Version, rel.Name, it.Package, it.Version)
			}
			dup = true
		}
		have[f.Name] = true
		if !dup {
			set.Items = append(set.Items, api.SetItem{Package: f.Name, Source: api.SetFile, Ref: f.SHA256, Version: f.Version, SHA256: f.SHA256, File: f.File})
		}
	}
	for _, p := range adds {
		if !have[p] {
			return nil, fmt.Errorf("pacman downloaded no %s: the Mac sent no file of it", p)
		}
	}
	return set, nil
}

// joinAddsFile joins the parts the Mac sent of the file, adds/<file>.<n> in
// the job's folder, each exactly api.AddsPart bytes but the last, into the
// artifact store, when the whole is the file's sha256 and size. A blob of
// that sha256 already in the store is checked, and replaced if it is not.
func (s *Server) joinAddsFile(job string, f api.AddsFile) error {
	dst := s.artifactPath(f.SHA256)
	if sum, err := fileSHA(dst); err == nil && sum == f.SHA256 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".upload-adds-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	for n := 0; n < f.Parts; n++ {
		want := api.AddsPart
		if n == f.Parts-1 {
			want = f.Size - int64(n)*api.AddsPart
		}
		p := filepath.Join(s.jobDir(job), "adds", fmt.Sprintf("%s.%d", f.File, n))
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() || st.Size() != want {
			tmp.Close()
			return fmt.Errorf("%s: part %d did not arrive as %d bytes", f.File, n, want)
		}
		in, err := os.Open(p)
		if err != nil {
			tmp.Close()
			return err
		}
		_, err = io.CopyN(io.MultiWriter(tmp, h), in, want)
		in.Close()
		if err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s arrived with the sha256 %s, not the %s the Mac named", f.File, short(got), short(f.SHA256))
	}
	return os.Rename(tmp.Name(), dst)
}

// fileSHA is the sha256 of the file at path.
func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
