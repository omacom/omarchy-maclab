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
	r.ev("resolving the packages the lab adds, %s, on this Mac (pacman -Sw against a copy of its database)", strings.Join(adds, ", "))
	var got api.AddsResolved
	if err := r.v.call(ctx, api.CmdAddsResolve, r.j.ID, api.AddsResolveArgs{Packages: adds}, addsResolveTimeout, &got); err != nil {
		return nil, fmt.Errorf("resolving %s: %s", strings.Join(adds, ", "), strings.TrimPrefix(err.Error(), api.CmdAddsResolve+": "))
	}
	set, err := addsSetOf(rel, base, adds, got, r.s.now())
	if err != nil {
		return nil, err
	}
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
// why it cannot be: a file that is no package, every added package among
// the files, and none the Release's set holds at another version.
func addsSetOf(rel api.Release, base *api.PackageSet, adds []string, got api.AddsResolved, now time.Time) (*api.PackageSet, error) {
	set := &api.PackageSet{Name: api.PerfAddsSet(rel.Name, adds), CreatedBy: "lab", Created: now.UTC(),
		Release: rel.Name, ManifestSHA256: base.ManifestSHA256, Items: slices.Clone(base.Items)}
	have := map[string]bool{}
	for _, f := range got.Files {
		if !reSHA.MatchString(f.SHA256) || f.Size <= 0 || f.Parts < 1 || f.Parts != int((f.Size+api.AddsPart-1)/api.AddsPart) ||
			f.File != filepath.Base(f.File) || !strings.HasPrefix(f.File, f.Name+"-"+f.Version+"-") || !strings.Contains(f.File, ".pkg.tar") {
			return nil, fmt.Errorf("the Mac sent %q, which is not a package file as the lab adds them", f.File)
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
// the job's folder, into the artifact store, when its sha256 and size are
// the file's, and removes the parts.
func (s *Server) joinAddsFile(job string, f api.AddsFile) error {
	dst := s.artifactPath(f.SHA256)
	parts := make([]string, f.Parts)
	for n := range parts {
		parts[n] = filepath.Join(s.jobDir(job), "adds", fmt.Sprintf("%s.%d", f.File, n))
	}
	defer func() {
		for _, p := range parts {
			os.Remove(p)
		}
	}()
	if st, err := os.Stat(dst); err == nil && st.Size() == f.Size {
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
	var size int64
	for _, p := range parts {
		in, err := os.Open(p)
		if err != nil {
			tmp.Close()
			return fmt.Errorf("%s: the Mac sent no part %s", f.File, filepath.Base(p))
		}
		n, err := io.Copy(io.MultiWriter(tmp, h), in)
		in.Close()
		size += n
		if err != nil {
			tmp.Close()
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != f.SHA256 || size != f.Size {
		return fmt.Errorf("%s arrived as %d bytes of sha256 %s, not the %d of %s the Mac named", f.File, size, short(got), f.Size, short(f.SHA256))
	}
	return os.Rename(tmp.Name(), dst)
}
