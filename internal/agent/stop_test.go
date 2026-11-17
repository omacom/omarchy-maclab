package agent

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// execSystem is a simulated Mac whose lab exec runs for real, with bash, as
// a Mac's does.
type execSystem struct{ *Sim }

func (s execSystem) Exec(ctx context.Context, a api.ExecArgs) api.ExecResult {
	return (&Linux{}).Exec(ctx, a)
}

// stopHelperEnv makes the test binary a lab-agent: it runs an Agent on
// execSystem against the labd at its value, until SIGTERM.
const stopHelperEnv = "MACLAB_STOP_HELPER_LABD"

// SIGTERM stops lab-agent within a bounded time, even while a command it
// runs ignores SIGTERM and has children that keep its output pipe open, and
// nothing that command started is left running.
func TestSIGTERMStopsLabAgentAndEveryProcessItsCommandStarted(t *testing.T) {
	if url := os.Getenv(stopHelperEnv); url != "" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
		defer stop()
		a := New(&Config{Server: url, Secret: "test", WorkDir: t.TempDir()}, execSystem{NewSim(t.TempDir())}, log.New(os.Stderr, "", 0))
		a.Run(ctx)
		return
	}
	dir := t.TempDir()
	pids := filepath.Join(dir, "pids")
	script := "trap '' TERM; sleep 600 & echo $! >> " + pids + "; sleep 600 & echo $! >> " + pids +
		"; echo $$ >> " + pids + "; touch " + filepath.Join(dir, "ready") + "; wait"
	args, _ := json.Marshal(api.ExecArgs{Command: script, TimeoutSec: 600})
	var sent atomic.Bool
	labd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/checkin" {
			io.WriteString(w, "ok")
			return
		}
		var resp api.CheckinResponse
		if !sent.Swap(true) {
			resp.Commands = []api.Command{{ID: "c1", Kind: api.CmdExec, Args: args}}
		} else {
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer labd.Close()

	agent := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	agent.Env = append(os.Environ(), stopHelperEnv+"="+labd.URL)
	var out logBuf
	agent.Stdout, agent.Stderr = &out, &out
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- agent.Wait() }()
	t.Cleanup(func() {
		agent.Process.Kill()
		for _, pid := range readPids(t, pids) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the exec never started:\n%s", out.String())
		}
	}
	started := readPids(t, pids)
	if len(started) != 3 {
		t.Fatalf("the exec wrote pids %v", started)
	}

	stopped := time.Now()
	agent.Process.Signal(syscall.SIGTERM)
	bound := stopGrace + 5*time.Second
	select {
	case <-exited:
	case <-time.After(bound):
		t.Fatalf("lab-agent had not ended %v after SIGTERM:\n%s", bound, out.String())
	}
	t.Logf("lab-agent ended %v after SIGTERM", time.Since(stopped).Round(time.Millisecond))
	for _, pid := range started {
		if !ended(pid) {
			t.Errorf("process %d, started by the exec, still runs after lab-agent ended", pid)
		}
	}
}

// A command's group ends with its program: a child that honours SIGTERM goes
// at once when the command is stopped, and one left behind by a program that
// ended on its own goes too.
func TestACommandsProcessGroupEndsWithIt(t *testing.T) {
	for _, c := range []struct {
		name, script string
		stop         bool
		within       time.Duration
	}{
		{"stopped", "sleep 600 & echo $! > PIDS; wait", true, 2 * time.Second},
		{"ended, a child left", "sleep 600 > /dev/null 2>&1 & echo $! > PIDS", false, 2 * time.Second},
		{"ended, a child ignoring SIGTERM left", "(trap '' TERM; exec sleep 600) > /dev/null 2>&1 & echo $! > PIDS; sleep 0.2", false, stopGrace + 2*time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			pids := filepath.Join(t.TempDir(), "pids")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "-c", strings.ReplaceAll(c.script, "PIDS", pids))
			if c.stop {
				go func() {
					for len(readPids(t, pids)) == 0 {
						time.Sleep(10 * time.Millisecond)
					}
					cancel()
				}()
			}
			start := time.Now()
			runGroup(cmd)
			if took := time.Since(start); took > c.within {
				t.Errorf("runGroup took %v", took)
			}
			for _, pid := range readPids(t, pids) {
				if !ended(pid) {
					syscall.Kill(pid, syscall.SIGKILL)
					t.Errorf("process %d outlived its command", pid)
				}
			}
		})
	}
}

// ended is whether process pid is gone, given a moment to be reaped.
func ended(pid int) bool {
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return true
		}
	}
	return false
}

func readPids(t *testing.T, path string) []int {
	b, _ := os.ReadFile(path)
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil && n > 1 {
			pids = append(pids, n)
		}
	}
	return pids
}

// fifoKernel is a kernel artifact whose usr/lib/modules/<release>/vmlinuz
// is a FIFO, and a Fetcher that hands it over.
func fifoKernel(t *testing.T, krel string) Fetcher {
	t.Helper()
	art := filepath.Join(t.TempDir(), "artifact.tar")
	f, err := os.Create(art)
	if err != nil {
		t.Fatal(err)
	}
	w := tar.NewWriter(f)
	dir := "usr/lib/modules/" + krel + "/"
	for _, h := range []*tar.Header{
		{Name: "usr/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/lib/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "usr/lib/modules/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: dir + "vmlinuz", Typeflag: tar.TypeFifo, Mode: 0o644},
	} {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return func(_ context.Context, _, dst string) error {
		b, err := os.ReadFile(art)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	}
}

// A kernel image must be a regular file: a FIFO passes a plain stat, and the
// copy to the boot files would hang opening it, holding lab-agent's stop.
func TestAKernelImageThatIsNotARegularFileIsRefused(t *testing.T) {
	l := &Linux{WorkDir: t.TempDir()}
	booted := false
	_, err := l.install(context.Background(), "j1", strings.Repeat("a", 64), fifoKernel(t, "6.99.0-labtest-fifo"), func(krel, image string) error {
		booted = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("staging a FIFO vmlinuz: %v", err)
	}
	if booted {
		t.Error("a FIFO vmlinuz was handed to the boot files")
	}
}

// The programs that stage a kernel run in the command's context and group:
// a copy that blocks (here on a FIFO, which the install refuses first) ends
// when the command is stopped.
func TestStagingABootFileEndsWhenItsCommandStops(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "vmlinuz")
	if err := syscall.Mkfifo(image, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Lets a copy still blocked on the FIFO read its end.
		if f, err := os.OpenFile(image, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- grubBootFiles(ctx, "6.99.0-labtest", image, t.TempDir()) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a stopped copy of the kernel image succeeded")
		}
	case <-time.After(stopGrace + 3*time.Second):
		t.Fatal("copying the kernel image did not end when its command stopped")
	}
}

// pacman runs as a process group: what it leaves behind (a hook's daemon,
// say) ends with the install.
func TestPacmansChildEndsWithTheInstall(t *testing.T) {
	bin, pids := t.TempDir(), filepath.Join(t.TempDir(), "pids")
	fake := "#!/bin/sh\nsleep 600 > /dev/null 2>&1 &\necho $! > " + pids + "\n"
	if err := os.WriteFile(filepath.Join(bin, "pacman"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l := &Linux{WorkDir: t.TempDir()}
	a := api.InstallArgs{Release: "r1", Kernel: "6.99.0-labtest", Packages: []api.InstallPackage{{File: "linux-labtest-1-1-aarch64.pkg.tar.zst", SHA256: strings.Repeat("a", 64)}}}
	fetch := func(_ context.Context, _, dst string) error { return os.WriteFile(dst, []byte("pkg"), 0o644) }
	if err := l.Install(context.Background(), "j1", a, fetch); err != nil {
		t.Fatal(err)
	}
	started := readPids(t, pids)
	if len(started) != 1 {
		t.Fatalf("the fake pacman wrote pids %v", started)
	}
	if !ended(started[0]) {
		syscall.Kill(started[0], syscall.SIGKILL)
		t.Error("pacman's child outlived the install")
	}
}

// A system tool lab-agent runs outside a command's context (grub-editenv
// here) is bounded: one that hangs is ended, with its group, and lab-agent
// goes on.
func TestASystemToolThatHangsIsEnded(t *testing.T) {
	bin, pids := t.TempDir(), filepath.Join(t.TempDir(), "pids")
	fake := "#!/bin/sh\necho $$ >> " + pids + "\nsleep 600 &\necho $! >> " + pids + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "grub-editenv"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	was := toolTimeout
	toolTimeout = time.Second
	t.Cleanup(func() { toolTimeout = was })
	env := filepath.Join(t.TempDir(), "maclab.env")
	os.WriteFile(env, nil, 0o644)
	t.Cleanup(func() {
		for _, pid := range readPids(t, pids) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	done := make(chan string, 1)
	go func() { done <- (&grubLayout{EnvFile: env}).armed() }()
	select {
	case <-done:
	case <-time.After(toolTimeout + stopGrace + 3*time.Second):
		t.Fatal("a hung grub-editenv held lab-agent")
	}
	for _, pid := range readPids(t, pids) {
		if !ended(pid) {
			t.Errorf("process %d of the hung tool still runs", pid)
		}
	}
}

// The lab-boot lock wait's last step, ending the Volunteer's unlocked
// session, is a bounded system tool too: a loginctl that hangs is ended,
// with its group, and the wait returns, so lab-agent can stop.
func TestAHungLoginctlInTheLockWaitIsEnded(t *testing.T) {
	bin, pids := t.TempDir(), filepath.Join(t.TempDir(), "pids")
	fake := "#!/bin/sh\necho $$ >> " + pids + "\nsleep 600 &\necho $! >> " + pids + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "loginctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	was := toolTimeout
	toolTimeout = time.Second
	t.Cleanup(func() { toolTimeout = was })
	t.Cleanup(func() {
		for _, pid := range readPids(t, pids) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- (&Linux{GUIUser: "volunteer"}).awaitLock(context.Background(), filepath.Join(t.TempDir(), "locked"), 0)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "terminated") {
			t.Errorf("the lock wait: %v", err)
		}
	case <-time.After(toolTimeout + stopGrace + 3*time.Second):
		t.Fatal("a hung loginctl held the lock wait, and so lab-agent's stop")
	}
	if len(readPids(t, pids)) != 2 {
		t.Fatalf("the fake loginctl wrote pids %v", readPids(t, pids))
	}
	for _, pid := range readPids(t, pids) {
		if !ended(pid) {
			t.Errorf("process %d of the hung loginctl still runs", pid)
		}
	}
}

// lockWaitSystem is a simulated Mac whose tests wait for the lab-boot lock as
// a Mac's do, on a lock note that never says "locked".
type lockWaitSystem struct {
	*Sim
	locked string
}

func (s lockWaitSystem) RunTest(ctx context.Context, job string, t api.TestSpec, fetch Fetcher, quiet Quieter, outDir string) api.TestResult {
	res := api.TestResult{Name: t.Name}
	if err := (&Linux{GUIUser: "volunteer"}).awaitLock(ctx, s.locked, 0); err != nil {
		res.Error, res.ExitCode = err.Error(), -1
	}
	return res
}

// A running lab-agent asked to stop while its lock wait's loginctl hangs
// stops within the tool's bound, and the hung loginctl goes with it.
func TestAHungLoginctlLetsARunningAgentStop(t *testing.T) {
	bin, pids := t.TempDir(), filepath.Join(t.TempDir(), "pids")
	fake := "#!/bin/sh\necho $$ >> " + pids + "\nsleep 600 &\necho $! >> " + pids + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "loginctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	was := toolTimeout
	toolTimeout = time.Second
	t.Cleanup(func() { toolTimeout = was })
	t.Cleanup(func() {
		for _, pid := range readPids(t, pids) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	args, _ := json.Marshal(api.TestSpec{Name: "t1", Script: "true"})
	var sent atomic.Bool
	labd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/checkin" {
			io.WriteString(w, "ok")
			return
		}
		var resp api.CheckinResponse
		if !sent.Swap(true) {
			resp.Commands = []api.Command{{ID: "c1", JobID: "j1", Kind: api.CmdRunTest, Args: args}}
		} else {
			select {
			case <-r.Context().Done():
			case <-time.After(200 * time.Millisecond):
			}
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer labd.Close()

	var out logBuf
	sys := lockWaitSystem{NewSim(t.TempDir()), filepath.Join(t.TempDir(), "locked")}
	a := New(&Config{Server: labd.URL, Secret: "test", WorkDir: t.TempDir()}, sys, log.New(&out, "", 0))
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	ran := make(chan error, 1)
	go func() { ran <- a.Run(ctx) }()
	for deadline := time.Now().Add(30 * time.Second); len(readPids(t, pids)) < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the lock wait never ran loginctl:\n%s", out.String())
		}
	}

	stop()
	select {
	case <-ran:
	case <-time.After(toolTimeout + stopGrace + 3*time.Second):
		t.Fatalf("a hung loginctl held lab-agent's stop:\n%s", out.String())
	}
	for _, pid := range readPids(t, pids) {
		if !ended(pid) {
			t.Errorf("process %d of the hung loginctl still runs", pid)
		}
	}
}

// sync(2) cannot be cut: a flush that hangs holds lab-agent no longer than
// a system tool's bound, and a second flush meanwhile waits on the first
// rather than starting another.
func TestAHungSyncHoldsLabAgentNoLongerThanTheBound(t *testing.T) {
	noFlush(t)
	was, wasFS := toolTimeout, syncFS
	toolTimeout = time.Second
	hung := make(chan struct{})
	var started atomic.Int32
	syncFS = func() { started.Add(1); <-hung }
	t.Cleanup(func() { close(hung); noFlush(t); toolTimeout, syncFS = was, wasFS })
	for i := 0; i < 2; i++ {
		done := make(chan struct{})
		go func() { syncDisks(); close(done) }()
		select {
		case <-done:
		case <-time.After(toolTimeout + 3*time.Second):
			t.Fatal("a hung sync held lab-agent")
		}
	}
	if n := started.Load(); n != 1 {
		t.Errorf("%d flushes were started while the first one hung", n)
	}
}

// noFlush waits until no flush an earlier test started is under way, so the
// next one starts the test's own.
func noFlush(t *testing.T) {
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		syncMu.Lock()
		idle := syncing == nil
		syncMu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a flush from before the test never ended")
		}
	}
}

// hangSync makes every flush hang until the test ends, with a 1 s bound.
func hangSync(t *testing.T) {
	noFlush(t)
	was, wasFS := toolTimeout, syncFS
	toolTimeout = time.Second
	hung := make(chan struct{})
	syncFS = func() { <-hung }
	t.Cleanup(func() { close(hung); noFlush(t); toolTimeout, syncFS = was, wasFS })
}

// A flush that did not end refuses the panic: SysRq c with the lab's writes
// still unflushed can leave the ESP's FAT corrupt, which a crash test is not
// there to check.
func TestAHungSyncRefusesThePanic(t *testing.T) {
	hangSync(t)
	dir := t.TempDir()
	wasEnable, wasTrigger := sysrqEnable, sysrqTrigger
	sysrqEnable, sysrqTrigger = filepath.Join(dir, "sysrq"), filepath.Join(dir, "sysrq-trigger")
	t.Cleanup(func() { sysrqEnable, sysrqTrigger = wasEnable, wasTrigger })
	err := (&Linux{}).Crash("panic")
	if err == nil || !strings.Contains(err.Error(), "flush") {
		t.Errorf("the crash: %v", err)
	}
	for _, f := range []string{sysrqEnable, sysrqTrigger} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was written after a flush that did not end", filepath.Base(f))
		}
	}
}

// A stage, install, clean-up or one-shot whose flush did not end fails its
// result: labd reboots or resets on it, and the change may not be on disk.
func TestAHungSyncFailsTheResultsThatWaitOnIt(t *testing.T) {
	hangSync(t)
	l := &Linux{WorkDir: t.TempDir()}
	ctx := context.Background()
	steps := map[string]func() error{
		"stage": func() error {
			_, err := l.Stage(ctx, "bad/job", api.StageArgs{}, nil)
			return err
		},
		"install": func() error { return l.Install(ctx, "bad/job", api.InstallArgs{}, nil) },
	}
	// The rest write the bootloader when they find one: run them only where
	// there is none to write.
	if _, err := detectBootloader(); err != nil {
		steps["clean-up"] = func() error { return l.Cleanup("", false) }
		steps["boot once"] = func() error { return l.BootOnce("lab-x") }
		steps["cancel boot once"] = l.CancelBootOnce
	}
	for name, step := range steps {
		if err := step(); err == nil || !strings.Contains(err.Error(), "flush") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A stopped lab exec's leader has the whole grace between SIGTERM and
// SIGKILL: one that takes four seconds to clean up gets to finish.
func TestAStoppedExecsLeaderHasTheWholeGrace(t *testing.T) {
	cleaned := filepath.Join(t.TempDir(), "cleaned")
	ready := filepath.Join(t.TempDir(), "ready")
	script := "trap 'sleep 4; touch " + cleaned + "; exit 0' TERM; touch " + ready + "; while :; do sleep 0.1; done"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	(&Linux{}).Exec(ctx, api.ExecArgs{Command: script, TimeoutSec: 60})
	if _, err := os.Stat(cleaned); err != nil {
		t.Errorf("the exec's leader was killed within %v of SIGTERM, before its four seconds of clean-up", stopGrace)
	}
}

// When a command's program ends on its own, its group is signalled while the
// program is not yet reaped, so the group's number cannot have gone to
// another process: a child left holding the output pipe gets SIGTERM at
// once, and the command ends without waiting out the pipe's grace.
func TestAGroupIsSignalledBeforeItsLeaderIsReaped(t *testing.T) {
	pids := filepath.Join(t.TempDir(), "pids")
	cmd := exec.CommandContext(context.Background(), "bash", "-c", "sleep 600 & echo $! > "+pids)
	var out strings.Builder
	cmd.Stdout = &out
	start := time.Now()
	runGroup(cmd)
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("a command whose child held its output took %v to end", took)
	}
	for _, pid := range readPids(t, pids) {
		if !ended(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Errorf("process %d outlived its command", pid)
		}
	}
}
