package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// The benchmark builtins: one tier each. The runner (tests/bench/bench.py)
// runs as root and starts what draws as the GUI user in the Wayland session.
const (
	benchTest         = "bench"
	benchExtendedTest = "bench-extended"
)

var (
	benchPython     = "python3"
	benchCache      = "/var/cache/lab-bench"
	benchWorkParent = "/var/tmp"
	benchManifest   = bench.Manifest
	benchGUIEnv     = guiEnv
)

func isBench(builtin string) bool { return builtin == benchTest || builtin == benchExtendedTest }

func benchTier(builtin string) string {
	if builtin == benchExtendedTest {
		return "extended"
	}
	return "default"
}

// benchTimeout covers the first run's fetch of every pinned file from labd
// as well as the tier itself (about 10 and 30 minutes).
func benchTimeout(builtin string) time.Duration {
	if builtin == benchExtendedTest {
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
	tier := benchTier(t.Builtin)
	dir := filepath.Join(l.jobDir(job), "bench")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	runner := filepath.Join(dir, "bench.py")
	manifest := filepath.Join(dir, "manifest.json")
	items, err := benchManifest()
	if err != nil {
		return fail(err)
	}
	mb, err := bench.JSON(items)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(runner, bench.Runner, 0o600); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(manifest, mb, 0o644); err != nil {
		return fail(err)
	}

	out, err := exec.CommandContext(ctx, benchPython, "-I", runner, "--tier", tier, "--needs").Output()
	if err != nil {
		return fail(fmt.Errorf("benchmark runner: %w", err))
	}
	byName := map[string]bench.Item{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if err := os.MkdirAll(benchCache, 0o755); err != nil {
		return fail(err)
	}
	os.Chmod(benchCache, 0o755)
	for _, n := range strings.Fields(string(out)) {
		it, ok := byName[n]
		if !ok {
			return fail(fmt.Errorf("the runner needs %s, which nothing pins", n))
		}
		if err := benchFetch(ctx, fetch, it); err != nil {
			return fail(fmt.Errorf("%s %s: %w", it.Name, it.Version, err))
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
	work, err := os.MkdirTemp(benchWorkParent, "lab-bench-*")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(work)
	os.Chmod(work, 0o755)
	if tier == "extended" && quiet != nil {
		// The extended tier suspends the Mac once.
		quiet(ctx, testTimeout(t))
		defer quiet(context.WithoutCancel(ctx), 0)
	}

	tctx, cancel := context.WithTimeout(ctx, testTimeout(t))
	defer cancel()
	logPath := filepath.Join(outDir, "log.txt")
	logf, err := os.Create(logPath)
	if err != nil {
		return fail(err)
	}
	cmd := exec.CommandContext(tctx, benchPython, "-I", runner, "--tier", tier, "--manifest", manifest,
		"--cache", benchCache, "--work", work, "--out", filepath.Join(outDir, "bench.json"),
		"--gui-user", u.Username, "--gui-env-file", envFile)
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
func benchFetch(ctx context.Context, fetch Fetcher, it bench.Item) error {
	dst := filepath.Join(benchCache, it.SHA256)
	if st, err := os.Stat(dst); err == nil && st.Mode().IsRegular() && (it.Size == 0 || st.Size() == it.Size) {
		return nil
	}
	tmp := dst + ".part"
	os.Remove(tmp)
	if err := fetch(ctx, it.SHA256, tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("from labd (lab bench mirror puts it there): %w", err)
	}
	got, err := fileSHA256(tmp)
	if err == nil && got != it.SHA256 {
		err = fmt.Errorf("sha256 %s, want %s", got, it.SHA256)
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
