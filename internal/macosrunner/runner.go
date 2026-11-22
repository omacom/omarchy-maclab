// Package macosrunner is lab-macos-runner: in macOS on a lab Mac, at boot,
// it tells labd the macOS build and asks whether a macOS run was started for
// this boot and, if one was, runs the Benchmark set's list, uploads the
// results and reboots. The Mac was booted into macOS for one boot only
// (lab-agent boot-macos-once, asahi-bless --next), so the reboot returns it
// to Omarchy.
package macosrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// Version is the runner's own number.
var Version = "0.1.0"

// ConfigPath is where enroll keeps the runner's secret, root's only.
const ConfigPath = "/Library/Application Support/Omarchy Mac Lab/macos-runner.json"

// WorkDir is where a run keeps its files, its private log and its results.
const WorkDir = "/Library/Application Support/Omarchy Mac Lab/work"

// Config is the enrolled runner.
type Config struct {
	Server  string `json:"server"`
	Mac     string `json:"mac"`
	Secret  string `json:"secret"`
	GUIUser string `json:"gui_user,omitempty"` // whose desktop the browsers run in
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Server == "" || c.Secret == "" {
		return c, fmt.Errorf("%s: not enrolled", path)
	}
	return c, nil
}

// Enroll spends the one-time token from `lab macos enroll` for this Mac's
// runner secret and keeps it at path, mode 0600. It tells labd the macOS
// version and build, as every boot does; host nil is macOS itself.
func Enroll(ctx context.Context, server, token, guiUser, path string, host Host) error {
	server = strings.TrimRight(server, "/")
	if host == nil {
		host = MacHost{}
	}
	u := server + "/api/macos/runner/enroll"
	if v, b, err := host.SWVers(ctx); err == nil {
		u += BootFacts{Version: v, Build: b}.pollQuery()
	}
	var e api.MacOSEnrolled
	if err := call(ctx, http.DefaultClient, "POST", u, token, nil, &e); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(Config{Server: server, Mac: e.Mac, Secret: e.Secret, GUIUser: guiUser}, "", "  ")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Runner is one boot's run.
type Runner struct {
	Config Config
	Dir    string
	// Host is the Mac; nil is macOS itself.
	Host Host
	// Set is the Benchmark set of that name, "" for the newest; nil is the
	// sets the runner was built with.
	Set func(name string) (*sets.Set, error)
	// ShareDir is where the files the logged-in user runs are unpacked; ""
	// is a new folder under /private/var/tmp.
	ShareDir string
	HTTP     *http.Client
	// Reboot returns the Mac to Omarchy; nil is shutdown -r now.
	Reboot func() error
	// BrowserTimeout bounds one browser benchmark; 0 is the benchmark's own.
	BrowserTimeout time.Duration
	// NetworkWait is how long the first poll retries while the network
	// comes up after boot; 0 is 5 minutes.
	NetworkWait time.Duration
	Log         func(string, ...any)
}

func (r *Runner) say(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}

func (r *Runner) host() Host {
	if r.Host == nil {
		r.Host = MacHost{GUIUser: r.Config.GUIUser}
	}
	return r.Host
}

func (r *Runner) set(name string) (*sets.Set, error) {
	if r.Set != nil {
		return r.Set(name)
	}
	if name == "" {
		return sets.Newest()
	}
	return sets.Load(name)
}

func (r *Runner) client() *http.Client {
	if r.HTTP != nil {
		return r.HTTP
	}
	return &http.Client{Timeout: 2 * time.Minute}
}

func (r *Runner) url(p string) string { return strings.TrimRight(r.Config.Server, "/") + p }

// Boot is the LaunchDaemon's run at boot. Every boot tells labd the macOS
// build. A boot labd did not start for a macOS run does nothing else: the
// Mac stays in macOS, booted by a person, and its session is left alone.
// The run's own boot, whatever happens, measures, reports and reboots.
func (r *Runner) Boot(ctx context.Context) error {
	facts := r.bootFacts(ctx)
	run, err := r.poll(ctx, facts)
	if err != nil {
		return err
	}
	if run == nil {
		r.say("no macOS run queued for %s", r.Config.Mac)
		return nil
	}
	if why := r.notThisBoot(run, facts); why != "" {
		r.say("macOS run %s is not this boot's (%s): nothing is measured", run.ID, why)
		return nil
	}
	if err := r.take(run.ID, facts.Session); err != nil {
		r.say("macOS run %s: this boot cannot record that it took the run (%v): nothing is measured", run.ID, err)
		return nil
	}
	r.say("macOS run %s (%s) on %s", run.ID, run.Tier, r.Config.Mac)
	up, runErr := r.Run(ctx, run)
	path := "/api/macos/runner/results"
	if run.PerfRun > 0 {
		path = "/api/macos/runner/performance/" + strconv.FormatInt(run.PerfRun, 10) + "/results"
	} else {
		up.RunID = run.ID
	}
	var upErr error
	for i := 0; i < 5; i++ {
		if upErr = call(context.WithoutCancel(ctx), r.client(), "POST", r.url(path), r.Config.Secret, up, nil); upErr == nil {
			break
		}
		r.say("upload: %v", upErr)
		sleep(ctx, 10*time.Second)
	}
	reboot := r.Reboot
	if reboot == nil {
		reboot = func() error { return exec.Command("/sbin/shutdown", "-r", "now").Run() }
	}
	r.say("rebooting to Omarchy")
	return errors.Join(runErr, upErr, reboot())
}

func (r *Runner) poll(ctx context.Context, facts BootFacts) (*api.MacOSRun, error) {
	wait := r.NetworkWait
	if wait == 0 {
		wait = 5 * time.Minute
	}
	end := time.Now().Add(wait)
	for {
		var p api.MacOSPoll
		err := call(ctx, r.client(), "GET", r.url("/api/macos/runner/poll"+facts.pollQuery()), r.Config.Secret, nil, &p)
		var se *statusError
		if err == nil || errors.As(err, &se) || time.Now().After(end) {
			return p.Run, err
		}
		r.say("labd not reached yet: %v", err)
		sleep(ctx, 10*time.Second)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// fetch gets the run's files into the work directory by file id, each
// checked against its sha256.
func (r *Runner) fetch(ctx context.Context, run *api.MacOSRun) (map[string]string, error) {
	dir := filepath.Join(r.Dir, "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	got := map[string]string{}
	for _, a := range run.Artifacts {
		p := filepath.Join(dir, a.SHA256)
		if have, err := fileSHA256(p); err == nil && have == a.SHA256 {
			got[a.Name] = p
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", r.url("/api/macos/runner/artifacts/"+a.SHA256), nil)
		body, err := do(r.client(), req, r.Config.Secret)
		if err != nil {
			return nil, fmt.Errorf("file %s: %w", a.Name, err)
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != a.SHA256 {
			return nil, fmt.Errorf("file %s: its sha256 is not the pinned %s", a.Name, a.SHA256)
		}
		if err := os.WriteFile(p, body, 0o600); err != nil {
			return nil, err
		}
		got[a.Name] = p
	}
	return got, nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
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

type statusError struct {
	Code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func call(ctx context.Context, c *http.Client, method, u, secret string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx, method, u, body)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	b, err := do(c, req, secret)
	if err == nil && out != nil {
		err = json.Unmarshal(b, out)
	}
	return err
}

func do(c *http.Client, req *http.Request, secret string) ([]byte, error) {
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("User-Agent", "lab-macos-runner/"+Version)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, &statusError{Code: resp.StatusCode, msg: fmt.Sprintf("labd: %s: %s", resp.Status, strings.TrimSpace(string(b)))}
	}
	return b, nil
}
