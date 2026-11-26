package agent

// The Agent rollout on the Mac. A new lab-agent is staged beside the one
// installed and switched in only once labd has heard the answer. The one it
// replaces is kept as the previous lab-agent, and a guard run by that previous
// lab-agent returns the Mac to it when the new one does not run well: at each
// start of lab-agent.service and every minute, with no network and no reboot.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

const (
	prevPath    = "/var/lib/maclab/lab-agent-prev"
	attemptFile = "agent-update.json"  // in the work dir: the change on trial
	outcomeFile = "agent-outcome.json" // in the work dir: how the last change ended
	// guardStartsMax is how many starts the new lab-agent gets to run well.
	guardStartsMax = 4
	// guardWithin is how long after the switch the new lab-agent has to run well.
	guardWithin = 10 * time.Minute
	// healthyAfter is how long a new lab-agent that labd has not answered yet
	// runs before it counts as running well: a Mac on Wi-Fi may be out of
	// reach, which says nothing about the agent.
	healthyAfter = 2 * time.Minute

	guardUnitName  = "lab-agent-guard.service"
	guardTimerName = "lab-agent-guard.timer"
	guardUnitPath  = "/etc/systemd/system/" + guardUnitName
	guardTimerPath = "/etc/systemd/system/" + guardTimerName
)

const guardUnit = `[Unit]
Description=Omarchy Mac Lab: return to the previous lab-agent when a new one does not run well

[Service]
Type=oneshot
ExecStart=-` + prevPath + ` guard

[Install]
Also=` + guardTimerName + `
`

const guardTimer = `[Unit]
Description=Omarchy Mac Lab: check a new lab-agent every minute

[Timer]
OnBootSec=1min
OnUnitActiveSec=1min
AccuracySec=10s

[Install]
WantedBy=timers.target
`

// agentAttempt is a change of lab-agent on trial: written as the new one is
// switched in, removed once it has run well or the Mac has returned.
type agentAttempt struct {
	From       string    `json:"from"`
	FromSHA256 string    `json:"from_sha256"`
	To         string    `json:"to"`
	ToSHA256   string    `json:"to_sha256"`
	LabRelease string    `json:"lab_release,omitempty"`
	At         time.Time `json:"at"`
	Starts     int       `json:"starts"`
}

func readJSONFile(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'))
}

func (c *Config) attemptPath() string { return filepath.Join(c.workDir(), attemptFile) }
func (c *Config) outcomePath() string { return filepath.Join(c.workDir(), outcomeFile) }

// lastOutcome is how the Mac's last change of lab-agent ended, or nil.
func (c *Config) lastOutcome() *api.AgentOutcome {
	var o api.AgentOutcome
	if !readJSONFile(c.outcomePath(), &o) || o.Outcome == "" {
		return nil
	}
	return &o
}

// installExecutable copies src over dst, through a file beside dst.
func installExecutable(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	if err := copyFile(src, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// activate switches the staged lab-agent in: the guard is set up first, the
// running one is kept as the previous lab-agent, and the change is written
// down for the guard. It restarts nothing.
func activate(cfg *Config, staged string, res api.AgentUpdate) error {
	self := cfg.self()
	if err := self.InstallGuard(); err != nil {
		return fmt.Errorf("the guard that returns the Mac to its lab-agent could not be set up: %w", err)
	}
	from, err := fileSHA256(self.Path)
	switch {
	case os.IsNotExist(err):
		// Nothing installed to keep, nor to return to: the change is not on trial.
		return os.Rename(staged, self.Path)
	case err != nil:
		return err
	}
	if err := installExecutable(self.Path, self.Prev); err != nil {
		return fmt.Errorf("keeping lab-agent %s as the previous one: %w", self.Version, err)
	}
	a := agentAttempt{From: self.Version, FromSHA256: from, To: res.To, ToSHA256: res.ToSHA256,
		LabRelease: res.LabRelease, At: cfg.clock().Now()}
	if err := writeJSONFile(cfg.attemptPath(), a); err != nil {
		return err
	}
	if err := os.Rename(staged, self.Path); err != nil {
		os.Remove(cfg.attemptPath())
		return err
	}
	return nil
}

// ranWell is the new lab-agent, running as sha, saying it runs well: the
// change on trial, if it is this one, stayed.
func ranWell(cfg *Config, sha string) bool {
	var a agentAttempt
	if sha == "" || !readJSONFile(cfg.attemptPath(), &a) || a.ToSHA256 != sha {
		return false
	}
	o := api.AgentOutcome{To: a.To, ToSHA256: a.ToSHA256, LabRelease: a.LabRelease, Outcome: api.AgentStayed, At: cfg.clock().Now()}
	if err := writeJSONFile(cfg.outcomePath(), o); err != nil {
		return false
	}
	return os.Remove(cfg.attemptPath()) == nil
}

// Guard is `lab-agent guard`, run by the previous lab-agent: before each
// start of lab-agent.service (start), and every minute. It returns the Mac to
// the previous lab-agent when the new one started guardStartsMax times, or
// guardWithin passed, without running well. It says what it did, "" for
// nothing.
func Guard(cfg *Config, start bool) (string, error) {
	var a agentAttempt
	if !readJSONFile(cfg.attemptPath(), &a) {
		return "", nil
	}
	self := cfg.self()
	cur, err := fileSHA256(self.Path)
	if err != nil {
		return "", err
	}
	if cur != a.ToSHA256 {
		// Another lab-agent was put in place since: not this trial's to judge.
		return "the lab-agent installed is not the one on trial: its trial ends", os.Remove(cfg.attemptPath())
	}
	now := cfg.clock().Now()
	if start {
		a.Starts++
		if err := writeJSONFile(cfg.attemptPath(), a); err != nil {
			return "", err
		}
	}
	var why string
	switch {
	case a.Starts > guardStartsMax:
		why = fmt.Sprintf("lab-agent %s started %d times without running well", a.To, a.Starts-1)
	case now.Sub(a.At) > guardWithin:
		why = fmt.Sprintf("lab-agent %s had not run well %s after it was put in place", a.To, guardWithin)
	default:
		return "", nil
	}
	if _, err := os.Stat(self.Prev); err != nil {
		return "", fmt.Errorf("%s, and there is no previous lab-agent to return to: %w", why, err)
	}
	if err := installExecutable(self.Prev, self.Path); err != nil {
		return "", fmt.Errorf("%s, and returning to the previous one failed: %w", why, err)
	}
	o := api.AgentOutcome{To: a.To, ToSHA256: a.ToSHA256, LabRelease: a.LabRelease, Outcome: api.AgentReturned, At: now, Why: why}
	if err := writeJSONFile(cfg.outcomePath(), o); err != nil {
		return "", err
	}
	if err := os.Remove(cfg.attemptPath()); err != nil {
		return "", err
	}
	done := fmt.Sprintf("%s: returned to lab-agent %s", why, a.From)
	if !start { // a start goes on with the one now in place
		if err := self.Restart(); err != nil {
			return done, fmt.Errorf("%s, but it was not started: %w", done, err)
		}
	}
	return done, nil
}

// installGuardUnits puts the guard in place on a real Mac: lab-agent.service
// runs it before each start, and its timer every minute. Files already as
// wanted are left alone.
func installGuardUnits() error {
	if err := os.MkdirAll(filepath.Dir(prevPath), 0o755); err != nil {
		return err
	}
	changed := false
	for path, want := range map[string]string{unitPath: unit, guardUnitPath: guardUnit, guardTimerPath: guardTimer} {
		if have, err := os.ReadFile(path); err == nil && string(have) == want {
			continue
		}
		if err := writeAtomic(path, []byte(want)); err != nil {
			return err
		}
		changed = true
	}
	var steps [][]string
	if changed {
		steps = append(steps, []string{"daemon-reload"})
	}
	steps = append(steps, []string{"enable", "--now", guardTimerName})
	for _, args := range steps {
		if out, err := exec.Command("systemctl", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
