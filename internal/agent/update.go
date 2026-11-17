package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/sshsig"
)

// A new lab-agent comes from the lab server only, and is trusted by its
// signature, checked on the Mac against the release key pinned in the
// lab-agent already running. One that is older than the one installed is
// refused. `sudo lab-agent update` does it at the Mac; on a Lab-owned Mac a
// Maintainer's `lab agent-update` has the running agent do the same. Nothing
// pushes an agent to a Volunteer's Mac.

// Self is the lab-agent installed on a Mac.
type Self struct {
	Path    string // where it is installed
	Version string // its number
	Key     string // the release key it pins, as "type base64"
	// Restart starts the installed lab-agent in place of the running one.
	Restart func() error
}

// self is c.Self, or the real one: this program, installed by setup and run
// by lab-agent.service.
func (c *Config) self() Self {
	s := Self{Path: binPath, Version: Version, Key: ReleaseKey, Restart: restartService}
	if c.Self == nil {
		return s
	}
	if c.Self.Path != "" {
		s.Path = c.Self.Path
	}
	if c.Self.Version != "" {
		s.Version = c.Self.Version
	}
	if c.Self.Key != "" {
		s.Key = c.Self.Key
	}
	if c.Self.Restart != nil {
		s.Restart = c.Self.Restart
	}
	return s
}

func restartService() error {
	if out, err := exec.Command("systemctl", "restart", "lab-agent.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart lab-agent.service: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

const (
	neverPushed    = "lab-agent is never pushed to a Volunteer's Mac: its Volunteer runs `sudo lab-agent update` at the Mac"
	nothingChanged = "Nothing was changed."
	// agentSizeMax bounds the download; lab-agent is about 8 MB.
	agentSizeMax = 256 << 20
)

// update takes the lab's lab-agent, checks it and installs it over the one
// installed. It restarts nothing.
func update(ctx context.Context, cfg *Config, sys System) (api.AgentUpdate, error) {
	self := cfg.self()
	res := api.AgentUpdate{From: self.Version, To: self.Version}
	if self.Key == "" {
		return res, errors.New("this lab-agent carries no release key (it was not built as part of a Lab release), so it cannot check a new lab-agent's signature. " + nothingChanged)
	}
	if _, _, cmdline := sys.Identity(); IsLabBoot(cmdline) {
		return res, errors.New("this Mac is on a Lab boot for a job: update lab-agent once it is back on its own system. " + nothingChanged)
	}
	dir := filepath.Dir(self.Path)
	f, err := os.CreateTemp(dir, ".lab-agent-new-")
	if err != nil {
		return res, fmt.Errorf("%w. %s", err, nothingChanged)
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	c := cfg.client(cfg.transferTimeout())
	base := strings.TrimRight(cfg.Server, "/") + "/api/agent/lab-agent"
	if err := download(ctx, c, base, cfg.Secret, tmp, agentSizeMax); err != nil {
		return res, fmt.Errorf("the lab did not hand out a lab-agent: %w. %s", err, nothingChanged)
	}
	sigReq, _ := http.NewRequestWithContext(ctx, "GET", base+".sig", nil)
	sig, err := doBody(c, sigReq, cfg.Secret)
	if err != nil {
		return res, fmt.Errorf("the lab handed out a lab-agent with no signature (%w): refused. %s", err, nothingChanged)
	}
	body, err := os.Open(tmp)
	if err != nil {
		return res, fmt.Errorf("%w. %s", err, nothingChanged)
	}
	err = sshsig.Verify(self.Key, ReleaseNamespace, body, sig)
	body.Close()
	if err != nil {
		return res, fmt.Errorf("the lab-agent the lab handed out is not signed by the release key this lab-agent pins (%w): refused. %s", err, nothingChanged)
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return res, fmt.Errorf("%w. %s", err, nothingChanged)
	}
	number, release, err := versionOf(ctx, tmp)
	if err != nil {
		return res, fmt.Errorf("the lab-agent the lab handed out does not say its number (%w): refused. %s", err, nothingChanged)
	}
	if !api.VersionAtLeast(number, self.Version) {
		return res, fmt.Errorf("the lab handed out lab-agent %s, older than the %s installed: refused. %s", number, self.Version, nothingChanged)
	}
	res.To, res.LabRelease = number, release
	if have, err := fileSHA256(self.Path); err == nil {
		if got, _ := fileSHA256(tmp); got == have {
			return res, nil
		}
	}
	if err := os.Rename(tmp, self.Path); err != nil {
		return res, fmt.Errorf("%w. %s", err, nothingChanged)
	}
	res.Changed = true
	return res, nil
}

// versionOf asks a lab-agent, whose signature has been checked, for its
// number and the Lab release it came from.
func versionOf(ctx context.Context, path string) (number, release string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	number = strings.TrimSpace(lines[0])
	if number == "" || strings.ContainsAny(number, " \t") {
		return "", "", fmt.Errorf("it answered %q", lines[0])
	}
	for _, l := range lines[1:] {
		if r, ok := strings.CutPrefix(l, "Lab release "); ok {
			release = strings.TrimSpace(r)
		}
	}
	return number, release, nil
}

// download writes what u answers to dst, at most max bytes.
func download(ctx context.Context, c *http.Client, u, secret, dst string, max int64) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = fmt.Errorf("more than %d bytes", max)
	}
	return err
}

// updateCmd is `sudo lab-agent update`, typed at the Mac.
func updateCmd(ctx context.Context, cfg *Config, sys System, args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: sudo lab-agent update")
	}
	res, err := update(ctx, cfg, sys)
	if err != nil {
		return err
	}
	from := ""
	if res.LabRelease != "" {
		from = " (Lab release " + res.LabRelease + ")"
	}
	if !res.Changed {
		fmt.Fprintf(out, "lab-agent %s%s is already the lab's: nothing to update.\n", res.To, from)
		return nil
	}
	fmt.Fprintf(out, "lab-agent %s -> %s%s: taken from %s, its signature checked against the release key this Mac already trusted.\n", res.From, res.To, from, cfg.Server)
	if err := cfg.self().Restart(); err != nil {
		return fmt.Errorf("the new lab-agent is installed but was not started: %w", err)
	}
	fmt.Fprintln(out, "lab-agent restarted.")
	return nil
}

// UpdateArgs is the lab's order to update. RestartWithinMS, when set, is how
// long after its result first went to labd the new lab-agent may still be
// started: labd holds the Mac for a while after it has the result, and an
// answer that comes after that must not restart an agent under whatever
// labd has given the Mac since.
type UpdateArgs struct {
	RestartWithinMS int64 `json:"restart_within_ms,omitempty"`
}

// updateSelf is the lab's order to update, on a Lab-owned Mac that is idle.
// The new agent starts once labd has heard the result, within the time labd
// gave; one labd gave up waiting for, or heard too late, is installed and
// not started.
func (a *Agent) updateSelf(ctx context.Context, c api.Command) (any, *afterAck, error) {
	var args UpdateArgs
	_ = json.Unmarshal(c.Args, &args)
	a.mu.Lock()
	job, cleaning := "", false
	for id, r := range a.running {
		if id != c.ID && r.c.JobID != "" {
			job = r.c.JobID
		}
		if id != c.ID && r.c.Kind == api.CmdCleanup {
			cleaning = true
		}
	}
	a.mu.Unlock()
	if job != "" {
		return nil, nil, fmt.Errorf("%s is not idle: job %s runs on it. %s", a.cfg.Name, job, nothingChanged)
	}
	if cleaning {
		// The restart would cut it as it rewrites the boot entries.
		return nil, nil, fmt.Errorf("%s is not idle: the lab's clean-up of leftover lab kernels runs on it. %s", a.cfg.Name, nothingChanged)
	}
	res, err := update(ctx, a.cfg, a.sys)
	if err != nil || !res.Changed {
		return res, nil, err
	}
	clock, within := a.cfg.clock(), time.Duration(args.RestartWithinMS)*time.Millisecond
	var sent time.Time
	// labd no longer holds the Mac for it: a restart now could cut whatever
	// it has started since. The new one starts with the next.
	notNow := func() { a.log.Printf("lab-agent %s is installed, and starts when lab-agent next restarts", res.To) }
	return res, &afterAck{check: func() error {
		sent = clock.Now()
		return nil
	}, run: func() {
		if late := clock.Now().Sub(sent); within > 0 && late > within {
			a.log.Printf("labd answered the update %s after its result went, past the %s it gave", late.Round(time.Millisecond), within)
			notNow()
			return
		}
		if err := a.cfg.self().Restart(); err != nil {
			a.log.Printf("the new lab-agent is installed but was not started: %v", err)
		}
	}, dropped: notNow}, nil
}
