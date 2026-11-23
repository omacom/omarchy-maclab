package macosrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// Only the lab's own boot measures. labd marks a macOS run started (POST
// /api/agent/macos-started) just before lab-agent reboots the Mac into
// macOS once, so the boot that runs it began right after that mark. A boot
// is the run's only when all of these hold:
//
//   - labd handed the run as started or running, with the time it started;
//   - this boot began no earlier than clockSkew before that, and no later
//     than startWindow after it;
//   - no other boot of this Mac took the run (taken.json, by boot session).
//
// Any other boot is a person's own: the runner tells labd the macOS build,
// as every boot does, and leaves the session alone. A one-shot that never
// reached macOS leaves a started run behind; labd cancels it when the Mac
// comes back to Omarchy, and the next macOS boot a person makes is later
// than the window, or is a boot the run was not taken by.
const (
	clockSkew   = 2 * time.Minute
	startWindow = 5 * time.Minute
)

// BootFacts is what every boot tells labd: the macOS build, and which boot
// this is.
type BootFacts struct {
	Version, Build string
	// Session is kern.bootsessionuuid, hashed: one boot, never the Mac.
	Session string
	At      time.Time
}

func (r *Runner) bootFacts(ctx context.Context) BootFacts {
	var f BootFacts
	if v, b, err := r.host().SWVers(ctx); err == nil {
		f.Version, f.Build = v, b
	} else {
		r.say("sw_vers: %v", err)
	}
	if s, at, err := r.host().Boot(ctx); err == nil && s != "" {
		sum := sha256.Sum256([]byte(s))
		f.Session, f.At = hex.EncodeToString(sum[:16]), at
	} else {
		r.say("this boot's session and start are unknown: %v", err)
	}
	return f
}

// pollQuery is the boot's report to labd, on the poll: the build, and the
// boot, so that labd can hand a started run to the boot that began after it.
func (f BootFacts) pollQuery() string {
	q := url.Values{}
	if f.Version != "" {
		q.Set("version", f.Version)
		q.Set("build", f.Build)
	}
	if f.Session != "" {
		q.Set("boot", f.Session)
		q.Set("booted", strconv.FormatInt(f.At.Unix(), 10))
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// notThisBoot is why the run is not this boot's to measure, or "".
func (r *Runner) notThisBoot(run *api.MacOSRun, f BootFacts) string {
	switch {
	case run.State != api.MacOSStarted && run.State != api.MacOSRunning:
		return "labd did not mark it started"
	case run.Started == nil:
		return "labd gave no time it started"
	case f.Session == "":
		return "this boot cannot be told apart from another"
	case f.At.Before(run.Started.Add(-clockSkew)):
		return "this boot began before labd started the run"
	case f.At.After(run.Started.Add(startWindow)):
		return "this boot began long after labd started the run: a person's own boot"
	}
	if s, ok := r.taken()[run.ID]; ok && s != f.Session {
		return "an earlier boot took the run"
	}
	return ""
}

func (r *Runner) takenPath() string { return filepath.Join(r.Dir, "taken.json") }

// taken is the runs this Mac's boots took, by boot session.
func (r *Runner) taken() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(r.takenPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// take records that this boot took the run, before it measures anything.
func (r *Runner) take(id, session string) error {
	m := r.taken()
	m[id] = session
	b, _ := json.Marshal(m)
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	tmp := r.takenPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.takenPath())
}
