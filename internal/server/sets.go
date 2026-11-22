package server

// Package sets (spec "Package sets and Compare"): frozen lists of packages
// a Maintainer makes, by hand from repository versions, uploaded files and
// Builder builds, or captured from the stack a Mac last reported. Every file
// is resolved to its sha256 as the set is made; a set is never changed.

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func (st *store) addSet(set *api.PackageSet) (bool, error) {
	d, _ := json.Marshal(set)
	res, err := st.db.Exec(`INSERT INTO package_sets(name,created,data) VALUES(?,?,?) ON CONFLICT(name) DO NOTHING`,
		set.Name, set.Created.UnixNano(), string(d))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (st *store) sets(where string, args ...any) ([]api.PackageSet, error) {
	rows, err := st.db.Query(`SELECT data FROM package_sets WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.PackageSet{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var set api.PackageSet
		if err := json.Unmarshal([]byte(data), &set); err != nil {
			return nil, err
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

func (s *Server) packageSet(name string) (*api.PackageSet, error) {
	l, err := s.store.sets(`name=?`, name)
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, refuse(404, "no package set %q (lab set list)", name)
	}
	return &l[0], nil
}

// pkgFileVersion splits a package file's name, NAME-PKGVER-PKGREL-ARCH.pkg.tar.*,
// into the package's name and its version (PKGVER-PKGREL).
func pkgFileVersion(file string) (name, version string, ok bool) {
	i := strings.Index(file, ".pkg.tar")
	if i < 0 {
		return "", "", false
	}
	f := strings.Split(file[:i], "-")
	if len(f) < 4 {
		return "", "", false
	}
	n := len(f)
	return strings.Join(f[:n-3], "-"), f[n-3] + "-" + f[n-2], true
}

// resolveItem fills in the sha256 (and the version, where it can) of one
// item as asked for, or says why it cannot.
func (s *Server) resolveItem(it api.SetItem) (api.SetItem, error) {
	it.SHA256, it.File = "", ""
	if err := it.Check(); err != nil {
		return it, refuse(400, "%v", err)
	}
	switch it.Source {
	case api.SetRepo:
		it.Version = it.Ref
	case api.SetFile:
		if _, err := os.Stat(s.artifactPath(it.Ref)); err != nil {
			return it, refuse(400, "%s: the lab holds no file %s (lab upload it first)", it.Package, it.Ref)
		}
		it.SHA256 = it.Ref
	case api.SetBuild:
		b, err := s.getBuild(it.Ref)
		if err != nil {
			return it, refuse(400, "%s: no build %s (lab builds)", it.Package, it.Ref)
		}
		if b.State != api.BuildDone {
			return it, refuse(400, "%s: build %s is %s, not done", it.Package, b.ID, b.State)
		}
		if b.Kind != api.BuildPackage {
			if b.Artifact == "" {
				return it, refuse(400, "%s: kernel build %s has no kernel", it.Package, b.ID)
			}
			it.SHA256, it.Version = b.Artifact, b.Release
			for _, f := range b.Files {
				if f.SHA256 == b.Artifact {
					it.File = f.Name
				}
			}
			break
		}
		var names []string
		for _, f := range b.Files {
			name, ver, ok := pkgFileVersion(f.Name)
			if ok && name == it.Package {
				it.SHA256, it.Version, it.File = f.SHA256, ver, f.Name
			}
			if ok {
				names = append(names, name)
			}
		}
		if it.SHA256 == "" {
			return it, refuse(400, "%s: build %s made no such package; it made %s", it.Package, b.ID, strings.Join(names, ", "))
		}
		if _, err := os.Stat(s.artifactPath(it.SHA256)); err != nil {
			return it, refuse(400, "%s: build %s's file %s is no longer stored", it.Package, b.ID, it.File)
		}
	}
	return it, nil
}

// makeSet freezes a set: its name, who made it, its items as resolved.
func (s *Server) makeSet(r *http.Request, set *api.PackageSet) error {
	if err := api.CheckSetName(set.Name); err != nil {
		return refuse(400, "%v", err)
	}
	if strings.HasPrefix(set.Name, api.PerfPackageSet("")) {
		return refuse(400, "%s: a name release-<version> is a Release's own set, which the lab makes from its frozen manifest", set.Name)
	}
	if len(set.Items) == 0 {
		return refuse(400, "a package set needs at least one package")
	}
	seen := map[string]bool{}
	for i, it := range set.Items {
		if seen[it.Package] {
			return refuse(400, "%s is in the set twice", it.Package)
		}
		seen[it.Package] = true
		var err error
		if set.Items[i], err = s.resolveItem(it); err != nil {
			return err
		}
	}
	set.CreatedBy, set.Created = leaser(r).Handle, s.now().UTC()
	added, err := s.store.addSet(set)
	if err != nil {
		return err
	}
	if !added {
		return refuse(409, "package set %q already exists, and a set is frozen once made: pick another name", set.Name)
	}
	return nil
}

func (s *Server) answerSet(w http.ResponseWriter, set *api.PackageSet, err error) {
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	writeJSON(w, set)
}

func (s *Server) hCreateSet(w http.ResponseWriter, r *http.Request) {
	var req api.SetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	set := &api.PackageSet{Name: req.Name, Items: slices.Clone(req.Items)}
	s.answerSet(w, set, s.makeSet(r, set))
}

// hCaptureSet makes a set of what a Mac last reported of its stack: each
// package at its installed version, from the repositories.
func (s *Server) hCaptureSet(w http.ResponseWriter, r *http.Request) {
	var req api.SetCapture
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	v := s.dev(req.Mac)
	if v == nil {
		httpErr(w, 404, "no Mac %q", req.Mac)
		return
	}
	stack := v.snapshot().Facts.Stack
	if len(stack) == 0 {
		httpErr(w, 409, "%s has not reported its installed packages: its lab-agent is older than the lab's, or it has not checked in since (lab agent-update)", req.Mac)
		return
	}
	set := &api.PackageSet{Name: req.Name, CapturedFrom: req.Mac}
	names := make([]string, 0, len(stack))
	for name := range stack {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		set.Items = append(set.Items, api.SetItem{Package: name, Source: api.SetRepo, Ref: stack[name]})
	}
	s.answerSet(w, set, s.makeSet(r, set))
}

func (s *Server) hSets(w http.ResponseWriter, r *http.Request) {
	l, err := s.store.sets(`1=1 ORDER BY created DESC`)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	writeJSON(w, l)
}

func (s *Server) hSet(w http.ResponseWriter, r *http.Request) {
	set, err := s.packageSet(r.PathValue("name"))
	s.answerSet(w, set, err)
}
