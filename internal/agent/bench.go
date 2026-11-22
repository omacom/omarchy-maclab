package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// The benchmark builtins: a Benchmark set's Quick list and its Full list.
// The runner (tests/bench/bench.py) runs as root and starts what draws as
// the GUI user in the Wayland session.
const (
	benchTest         = "bench"
	benchExtendedTest = "bench-extended"
)

var (
	benchPython     = "python3"
	benchCache      = "/var/cache/lab-bench"
	benchWorkParent = "/var/tmp"
	benchSet        = bench.Set
	benchSetNamed   = sets.Load
	benchGUIEnv     = guiEnv
)

func isBench(builtin string) bool { _, ok := bench.Builtins[builtin]; return ok }

// benchTimeout covers the first run's fetch of the list's pinned files from
// labd as well as the list itself (at most 45 minutes for the Full list).
func benchTimeout(builtin string) time.Duration {
	if bench.Builtins[builtin] == sets.Full {
		return 75 * time.Minute
	}
	return 30 * time.Minute
}

func (l *Linux) runBench(ctx context.Context, job string, t api.TestSpec, fetch Fetcher, quiet Quieter, outDir string) api.TestResult {
	res := api.TestResult{Name: t.Name, ExitCode: -1}
	fail := func(err error) api.TestResult { res.Error = err.Error(); return res }
	if l.GUIUser == "" {
		return fail(errors.New("benchmarks run in the desktop session, but no gui_user is configured (lab-agent setup --gui-user)"))
	}
	if strings.ContainsAny(job, "/\\ \n\r") || job == "" || job == "." || job == ".." {
		return fail(errors.New("invalid job"))
	}
	list := bench.Builtins[t.Builtin]
	var s *sets.Set
	var err error
	var files []sets.File
	var only []string
	if b := t.Bench; b != nil {
		list, only = b.List, b.Metrics
		if s, err = benchSetNamed(b.Set); err == nil {
			files, err = bench.FilesOf(s, list, only)
		}
	} else if s, err = benchSet(); err == nil {
		files = bench.Files(s, list)
	}
	if err != nil {
		return fail(err)
	}
	dir := filepath.Join(l.jobDir(job), "bench")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	runner := filepath.Join(dir, "bench.py")
	setFile := filepath.Join(dir, s.Name+".json")
	sb, err := json.Marshal(s)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(runner, bench.Runner, 0o600); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(setFile, sb, 0o644); err != nil {
		return fail(err)
	}

	if err := os.MkdirAll(benchCache, 0o755); err != nil {
		return fail(err)
	}
	os.Chmod(benchCache, 0o755)
	for _, f := range files {
		if err := benchFetch(ctx, fetch, f); err != nil {
			return fail(fmt.Errorf("%s %s: %w", f.ID, f.Version, err))
		}
	}

	u, genv, err := benchGUIEnv(l.GUIUser, guiSessionWait)
	if err != nil {
		return fail(err)
	}
	envFile := filepath.Join(dir, "gui.env")
	if err := os.WriteFile(envFile, []byte(strings.Join(genv, "\n")+"\n"), 0o600); err != nil {
		return fail(err)
	}
	// A run that never ended, by a crash or a power cut, left its work
	// folder behind, Storage's 8 GiB scratch file with it.
	old, _ := filepath.Glob(filepath.Join(benchWorkParent, "lab-bench-*"))
	for _, d := range old {
		os.RemoveAll(d)
	}
	work, err := os.MkdirTemp(benchWorkParent, "lab-bench-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(work)
	os.Chmod(work, 0o755)

	tctx, cancel := context.WithTimeout(ctx, testTimeout(t))
	defer cancel()
	logPath := filepath.Join(outDir, "log.txt")
	logf, err := os.Create(logPath)
	if err != nil {
		return fail(err)
	}
	args := []string{"-I", runner, "--set", setFile, "--list", list}
	if only != nil {
		args = append(args, "--only", strings.Join(only, ","))
	}
	cmd := exec.CommandContext(tctx, benchPython, append(args, "--cache", benchCache, "--work", work,
		"--out", filepath.Join(outDir, "bench.json"), "--gui-user", u.Username, "--gui-env-file", envFile)...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err = runGroup(cmd)
	logf.Close()
	res.Seconds = time.Since(start).Seconds()
	res.Tail = tail(logPath, 40)
	if tctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res
	}
	var ee *exec.ExitError
	switch {
	case err == nil:
		res.Passed, res.ExitCode = true, 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		return fail(err)
	}
	return res
}

// benchFetch puts one pinned file in the cache under its sha256, from labd,
// unless it is there already. The cache is readable by the GUI user.
func benchFetch(ctx context.Context, fetch Fetcher, f sets.File) error {
	dst := filepath.Join(benchCache, f.SHA256)
	if st, err := os.Stat(dst); err == nil && st.Mode().IsRegular() && (f.Size == 0 || st.Size() == f.Size) {
		return nil
	}
	tmp := dst + ".part"
	os.Remove(tmp)
	if err := fetch(ctx, f.SHA256, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("from labd (lab bench mirror puts it there): %w", err)
	}
	got, err := fileSHA256(tmp)
	if err == nil && got != f.SHA256 {
		err = fmt.Errorf("sha256 %s, want %s", got, f.SHA256)
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
