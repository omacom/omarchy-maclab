package macosrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// Run runs the run's list of its Benchmark set: the Quick list for the
// default tier, the Full list for extended. A benchmark that cannot run
// says why with a reason code, and the run goes on; the error returned is
// one that stopped the whole run. The upload is kept beside the private log
// as bench.json and bench-conditions.json.
func (r *Runner) Run(ctx context.Context, run *api.MacOSRun) (Upload, error) {
	up := Upload{list: sets.Quick, State: Succeeded}
	if run.Tier == api.TierExtended {
		up.list = sets.Full
	}
	set, err := r.set(run.Set)
	if err != nil {
		up.reason, up.State = Error, Failed
		return up, err
	}
	up.set = set.Name
	var metrics []sets.Metric
	for _, m := range set.List(up.list) {
		if m.MacOS != nil {
			metrics = append(metrics, m)
		}
	}
	got := map[string]outcome{}
	reason, err := r.measure(ctx, run, set, metrics, got, &up.Conditions)
	up.reason = reason
	if reason != "" {
		up.State = Failed
	}
	for _, m := range metrics {
		o, ok := got[m.ID]
		if !ok {
			o = because(Reason(or(string(reason), string(NotRun))))
		}
		res := result(set, m, o)
		if res.Reason.Fails() {
			up.State = Failed
		}
		up.Entries = append(up.Entries, res)
	}
	r.keep(up)
	return up, err
}

// result is a measurement as bench.json has it: every string from the set.
func result(set *sets.Set, m sets.Metric, o outcome) Result {
	res := Result{Set: set.Name, Metric: m.ID, Category: m.Category, Version: m.Version, Unit: m.Unit,
		HigherIsBetter: m.HigherIsBetter(), Seconds: round(o.seconds, 3), CoolDown: round(o.cool, 1),
		API: m.MacOS.API, Reason: o.reason}
	if !o.reason.Known() {
		res.Reason = Error
	}
	if m.Category == "Storage" {
		res.Capacity, res.Fill = o.capacity, o.fill
	}
	if res.Reason == "" {
		v := round(o.value, 3)
		res.Value = &v
		if o.spread != nil {
			sp := round(*o.spread, 3)
			res.Spread = &sp
		}
	}
	return res
}

// measure measures the metrics into got, under the conditions it sets and
// puts back. Its reason is the whole run's when it measured nothing.
func (r *Runner) measure(ctx context.Context, run *api.MacOSRun, set *sets.Set, metrics []sets.Metric, got map[string]outcome, c *Conditions) (Reason, error) {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return Error, err
	}
	logf, err := os.OpenFile(filepath.Join(r.Dir, "bench.log"), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return Error, err
	}
	defer logf.Close()
	s := &session{r: r, run: run, set: set, unpacked: map[string]string{}, log: logf}
	if s.files, err = r.fetch(ctx, run); err != nil {
		fmt.Fprintf(logf, "fetch: %v\n", err)
		return Error, err
	}
	s.conditionsOut = c
	unlock, err := lock(filepath.Join(r.Dir, "bench.lock"))
	if err != nil {
		return NotIdle, nil
	}
	defer unlock()
	restore, refused := s.conditions(ctx, c)
	if refused != "" {
		return refused, nil
	}
	defer func() {
		c.Restored = restore()
		fmt.Fprintf(logf, "put back: %v\n", c.Restored)
	}()
	for _, m := range metrics {
		if why := s.missing(m); why != "" {
			got[m.ID] = because(why)
		}
	}
	s.user = or(r.Config.GUIUser, s.host().ConsoleUser(ctx))
	if s.share, err = r.shareDir(); err != nil {
		fmt.Fprintf(logf, "share folder: %v\n", err)
		return Error, err
	}
	defer os.RemoveAll(s.share)
	defer s.fioCleanup()

	want := map[string]bool{}
	for _, m := range metrics {
		if _, done := got[m.ID]; !done {
			want[m.ID] = true
		}
	}
	for _, st := range orderedSteps(set) {
		var mine []string
		for _, id := range st.metrics {
			if want[id] {
				mine = append(mine, id)
			}
		}
		if len(mine) == 0 || ctx.Err() != nil {
			continue
		}
		if st.prepare != nil {
			if why := st.prepare(s, ctx); why != "" {
				for _, id := range mine {
					o := because(why)
					if s.fio != nil {
						o.capacity, o.fill = s.fio.capacity, s.fio.fill
					}
					got[id] = o
				}
				continue
			}
		}
		cool := s.coolDown(ctx)
		start := time.Now()
		outs := st.run(s, ctx)
		secs := time.Since(start).Seconds()
		for _, id := range mine {
			o, ok := outs[id]
			if !ok {
				o = because(Error)
			}
			if ctx.Err() != nil && o.reason == Error {
				o = because(NotRun)
			}
			if o.seconds == 0 {
				o.seconds = secs
			}
			o.cool = cool
			got[id] = o
			r.say("%s: %s", id, describe(o))
		}
	}
	return "", nil
}

// orderedSteps are the steps in the set's page order, by each step's first
// metric.
func orderedSteps(set *sets.Set) []step {
	at := func(st step) int {
		return slices.IndexFunc(set.Metrics, func(m sets.Metric) bool { return m.ID == st.metrics[0] })
	}
	out := slices.Clone(steps)
	slices.SortStableFunc(out, func(a, b step) int { return at(a) - at(b) })
	return out
}

func describe(o outcome) string {
	if o.reason != "" {
		return string(o.reason)
	}
	return fmt.Sprintf("%g", round(o.value, 3))
}

func round(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	if v < 0 {
		return -float64(int64(-v*p+0.5)) / p
	}
	return float64(int64(v*p+0.5)) / p
}

// missing is why the metric cannot run for want of a file, or "": the set
// still holds a placeholder for it, or labd did not hand the file it pins.
func (s *session) missing(m sets.Metric) Reason {
	for _, id := range m.MacOS.Files {
		f, ok := s.set.File(sets.MacOS, id)
		if !ok {
			return Error
		}
		if !f.Pinned() {
			return PlaceholderFile
		}
		p, ok := s.files[id]
		if !ok {
			return Error
		}
		if sum, err := fileSHA256(p); err != nil || sum != f.SHA256 {
			return Error
		}
	}
	return ""
}

// conditions checks and sets the run's conditions, else refuses it: on AC
// (a Mac with no battery counts) and idle; then Low Power Mode off and the
// backlight at half. restore puts them back and says whether it could.
func (s *session) conditions(ctx context.Context, c *Conditions) (restore func() bool, refused Reason) {
	h := s.host()
	none := func() bool { return true }
	p, err := h.Power(ctx)
	if err != nil {
		fmt.Fprintf(s.log, "power source: %v\n", err)
		return none, NotOnAC
	}
	c.OnAC = p.OnAC
	if !p.OnAC {
		return none, NotOnAC
	}
	busy, err := h.CPUBusy(ctx, busyWindow)
	if err != nil {
		fmt.Fprintf(s.log, "CPU use: %v\n", err)
		return none, NotIdle
	}
	c.CPUBusy = round(busy, 1)
	if busy >= busyLimit {
		return none, NotIdle
	}
	var undo []func() error
	if lpm, err := h.LowPowerMode(ctx); err != nil {
		fmt.Fprintf(s.log, "Low Power Mode: %v\n", err)
	} else if lpm {
		if err := h.SetLowPowerMode(ctx, false); err != nil {
			fmt.Fprintf(s.log, "Low Power Mode off: %v\n", err)
			c.LowPowerMode = true
		}
		undo = append(undo, func() error { return h.SetLowPowerMode(context.Background(), true) })
	}
	if was, ok, err := h.Brightness(ctx); err != nil {
		fmt.Fprintf(s.log, "backlight: %v\n", err)
	} else if ok {
		if err := h.SetBrightness(ctx, brightness); err != nil {
			fmt.Fprintf(s.log, "backlight to half: %v\n", err)
		} else {
			c.Backlight = spread(100 * brightness)
		}
		undo = append(undo, func() error { return h.SetBrightness(context.Background(), was) })
	}
	return func() bool {
		ok := true
		for i := len(undo) - 1; i >= 0; i-- {
			if err := undo[i](); err != nil {
				fmt.Fprintf(s.log, "putting back: %v\n", err)
				ok = false
			}
		}
		return ok
	}, ""
}

// coolDown waits until the hottest sensor is under coolLimit, at most
// coolMax, or coolNoSensor with no sensor; it is the seconds it waited.
func (s *session) coolDown(ctx context.Context) float64 {
	h := s.host()
	start := h.Now()
	t, ok := h.Temperature(ctx)
	if !ok {
		h.Sleep(ctx, coolNoSensor)
		return h.Now().Sub(start).Seconds()
	}
	for ok && t >= coolLimit && h.Now().Sub(start) < coolMax && ctx.Err() == nil {
		h.Sleep(ctx, coolStep)
		t, ok = h.Temperature(ctx)
	}
	return h.Now().Sub(start).Seconds()
}

// lock is the one benchmark run of this Mac, released when the runner ends.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another benchmark run holds the lock")
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (r *Runner) shareDir() (string, error) {
	parent := r.ShareDir
	if parent == "" {
		parent = "/private/var/tmp"
	}
	d, err := os.MkdirTemp(parent, "lab-bench-")
	if err == nil {
		err = os.Chmod(d, 0o755)
	}
	return d, err
}

// keep writes the upload's bench.json and bench-conditions.json beside the
// private log.
func (r *Runner) keep(up Upload) {
	if b, err := json.MarshalIndent(up.Entries, "", "  "); err == nil {
		os.WriteFile(filepath.Join(r.Dir, "bench.json"), append(b, '\n'), 0o600)
	}
	if b, err := json.MarshalIndent(up.Conditions, "", "  "); err == nil {
		os.WriteFile(filepath.Join(r.Dir, "bench-conditions.json"), append(b, '\n'), 0o600)
	}
}

// file is the fetched file of that id.
func (s *session) file(id string) string { return s.files[id] }

// dir is the file of that id unpacked under the share folder, once.
func (s *session) dir(ctx context.Context, id string) (string, error) {
	if d, ok := s.unpacked[id]; ok {
		return d, nil
	}
	src, ok := s.files[id]
	if !ok {
		return "", fmt.Errorf("no file %s in this run", id)
	}
	d := filepath.Join(s.share, id)
	if err := unpack(ctx, src, d); err != nil {
		return "", fmt.Errorf("%s: %w", id, err)
	}
	s.unpacked[id] = d
	return d, nil
}

// entries are where each program is in its archive when the set does not
// say.
var entries = map[string]string{
	"coremark":  "coremark",
	"7zip":      "7zz",
	"chrome":    "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
	"clpeak":    "clpeak",
	"fio":       "fio",
	"stream":    "stream",
	"ffmpeg":    "ffmpeg",
	"ane-bench": "ane-bench",
	"llama.cpp": "llama-bench",
	"mlx-bench": "mlx-bench",
	"smc-power": "smc-power",
}

// program is the path of the program of that file id.
func (s *session) program(ctx context.Context, id string) (string, error) {
	d, err := s.dir(ctx, id)
	if err != nil {
		return "", err
	}
	f, _ := s.set.File(sets.MacOS, id)
	return filepath.Join(d, or(f.Entry, entries[id])), nil
}

// tool runs a program, its output to the private log only; a tool not
// ending in time, or cancelled, is killed with what it started.
func (s *session) tool(ctx context.Context, timeout time.Duration, name string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	fmt.Fprintf(s.log, "$ %s %s\n%s%s", filepath.Base(name), strings.Join(args, " "), o.String(), e.String())
	if err != nil {
		fmt.Fprintf(s.log, "(%v)\n", err)
	}
	return o.String(), e.String(), err
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}
