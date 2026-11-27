package agent

// Packages the lab adds (spec section 22, "Packages the lab adds"): on a
// Lab-owned Mac's known-good system, lab-agent has pacman download every
// package file some packages need beyond what the Mac has installed, against
// copies of its pacman database, and sends each file to labd in parts; labd
// freezes them with the Release's set. Every Mac after installs that set
// offline: staging gives pacman no repository to fetch from.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// addsSystem is a System that can resolve the packages the lab adds: the
// files, in a folder of their own the caller removes.
type addsSystem interface {
	ResolveAdds(ctx context.Context, a api.AddsResolveArgs) (dir string, files []api.AddsFile, err error)
}

// pacmanDB is the known-good system's pacman database.
var pacmanDB = "/var/lib/pacman"

// ResolveAdds downloads, with pacman, every package file the packages need
// beyond what this Mac has installed, into a folder of its own, against a
// copy of its pacman database refreshed there: the Mac's own database,
// cache and system stay as they are. pacman checks each package's
// signature as it downloads it.
func (l *Linux) ResolveAdds(ctx context.Context, a api.AddsResolveArgs) (string, []api.AddsFile, error) {
	if err := checkAdds(a.Packages); err != nil {
		return "", nil, err
	}
	tmp, err := os.MkdirTemp("/var/tmp", "lab-adds-")
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, []api.AddsFile, error) {
		os.RemoveAll(tmp)
		return "", nil, err
	}
	// pacman downloads as its own user (DownloadUser), who must reach the
	// folders below.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return fail(err)
	}
	db, cache := filepath.Join(tmp, "db"), filepath.Join(tmp, "files")
	for _, d := range []string{filepath.Join(db, "sync"), cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fail(err)
		}
	}
	if out, err := combinedGroup(exec.CommandContext(ctx, "cp", "-a", filepath.Join(pacmanDB, "local"), db)); err != nil {
		return fail(fmt.Errorf("copying pacman's database: %v: %s", err, lastLines(string(out), 3)))
	}
	pacman := func(args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		base := []string{"--dbpath", db, "--cachedir", cache, "--logfile", "/dev/null", "--noconfirm"}
		out, err := combinedGroup(exec.CommandContext(ctx, "pacman", append(base, args...)...))
		if err != nil {
			return fmt.Errorf("pacman %s: %v: %s", strings.Join(args, " "), err, lastLines(string(out), 5))
		}
		return nil
	}
	if err := pacman("-Sy"); err != nil {
		return fail(err)
	}
	if err := pacman(append([]string{"-Sw"}, a.Packages...)...); err != nil {
		return fail(err)
	}
	files, err := addsFiles(cache)
	if err != nil {
		return fail(err)
	}
	return cache, files, nil
}

// checkAdds is why the package names will not do.
func checkAdds(pkgs []string) error {
	if len(pkgs) == 0 {
		return errors.New("no package to add")
	}
	for _, p := range pkgs {
		if !reCandidatePkg.MatchString(p) || strings.HasPrefix(p, "-") {
			return fmt.Errorf("bad package %q", p)
		}
	}
	return nil
}

// addsFiles are the package files in dir, by name, with their sha256s.
func addsFiles(dir string) ([]api.AddsFile, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []api.AddsFile
	for _, e := range ents {
		name, version, ok := pkgFile(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		sum, err := fileSHA256(p)
		if err != nil {
			return nil, err
		}
		out = append(out, api.AddsFile{Name: name, Version: version, File: e.Name(), SHA256: sum, Size: st.Size(),
			Parts: int((st.Size() + api.AddsPart - 1) / api.AddsPart)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if len(out) == 0 {
		return nil, errors.New("pacman downloaded no package: the packages are installed already, with all they need")
	}
	return out, nil
}

// resolveAdds is CmdAddsResolve: the files, each sent to the job as
// adds/<file>.<n> in parts of at most api.AddsPart bytes.
func (a *Agent) resolveAdds(ctx context.Context, c api.Command) (any, error) {
	as, ok := a.sys.(addsSystem)
	if !ok {
		return nil, errors.New("agent system cannot add packages")
	}
	var args api.AddsResolveArgs
	if err := json.Unmarshal(c.Args, &args); err != nil {
		return nil, err
	}
	dir, files, err := as.ResolveAdds(ctx, args)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(filepath.Dir(dir))
	for _, f := range files {
		if err := a.uploadParts(ctx, c.JobID, filepath.Join(dir, f.File), f); err != nil {
			return nil, fmt.Errorf("sending %s to labd: %w", f.File, err)
		}
	}
	return api.AddsResolved{Files: files}, nil
}

// uploadParts sends the file to the job in f.Parts parts.
func (a *Agent) uploadParts(ctx context.Context, job, path string, f api.AddsFile) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	for n := 0; n < f.Parts; n++ {
		part := fmt.Sprintf("%s.%d", path, n)
		if err := copyPart(src, part); err != nil {
			return err
		}
		err := a.upload(ctx, job, fmt.Sprintf("adds/%s.%d", f.File, n), part)
		os.Remove(part)
		if err != nil {
			return err
		}
	}
	return nil
}

func copyPart(src io.Reader, part string) error {
	dst, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.CopyN(dst, src, api.AddsPart)
	if errors.Is(err, io.EOF) {
		err = nil
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	return err
}

// offlineConf is the pacman configuration an offline stage installs with,
// inside the candidate: the candidate's own [options] and no repository, so
// pacman installs the set's files and fetches nothing besides.
const offlineConf = "etc/pacman-lab-offline.conf"

// writeOfflineConf writes offlineConf into the candidate from its
// pacman.conf's options.
func writeOfflineConf(cand string) error {
	b, err := os.ReadFile(filepath.Join(cand, "etc/pacman.conf"))
	if err != nil {
		return err
	}
	var keep []string
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(line)
		if strings.HasPrefix(s, "[") {
			in = s == "[options]"
		}
		if in && !strings.HasPrefix(s, "Include") {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		keep = []string{"[options]"}
	}
	return os.WriteFile(filepath.Join(cand, offlineConf), []byte(strings.Join(keep, "\n")+"\n"), 0o644)
}
