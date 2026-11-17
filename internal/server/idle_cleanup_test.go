package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// A Mac waiting for its Volunteer to accept changed terms takes nothing
// from the lab (spec 864), the idle clean-up of what earlier jobs left
// included: that rewrites its boot entries as root. It gets the clean-up
// once its Volunteer has accepted.
func TestAMacWaitingForTheTermsGetsNoIdleCleanUp(t *testing.T) {
	l := rolesLab(t, func(c *Config) {
		bin := filepath.Join(t.TempDir(), "lab-agent")
		if err := os.WriteFile(bin, []byte("\x7fELF lab-agent for the test"), 0o755); err != nil {
			t.Fatal(err)
		}
		c.AgentBinary = bin
		c.AliveWindow, c.OfflineAfter = time.Hour, time.Hour
	})
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	v, secret := l.signedIn(graceID).lent("shelf-mac")
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification})
	first := v.snapshot().FirstBaseline.Job
	jobSaid(t, l.s, first, "no Availability window")
	l.s.cancelJob(first, "")
	idleAfterStop(t, l.s)

	cleanups := func() int {
		v.mu.Lock()
		defer v.mu.Unlock()
		n := 0
		for _, c := range v.pending {
			if c.Kind == api.CmdCleanup {
				n++
			}
		}
		return n
	}
	v.mu.Lock()
	v.dirty, v.termsWait = true, true
	v.mu.Unlock()
	for i := 0; i < 3; i++ {
		l.clock.Advance(11 * time.Second)
		time.Sleep(100 * time.Millisecond)
	}
	if n := cleanups(); n != 0 {
		t.Fatalf("a Mac waiting for the terms was sent %d clean-ups", n)
	}

	v.mu.Lock()
	v.termsWait = false
	v.mu.Unlock()
	for deadline := time.Now().Add(10 * time.Second); cleanups() == 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the Mac got no clean-up once its terms were accepted either: %s", v.snapshot().State)
		}
		l.clock.Advance(11 * time.Second)
	}
}

// An idle clean-up queued just before the Mac comes to wait for the terms
// is not delivered at the check-in that starts the wait: delivery asks the
// hold again.
func TestAnIdleCleanUpQueuedBeforeATermsHoldIsNotDelivered(t *testing.T) {
	l := rolesLab(t, func(c *Config) {
		bin := filepath.Join(t.TempDir(), "lab-agent")
		if err := os.WriteFile(bin, []byte("\x7fELF lab-agent for the test"), 0o755); err != nil {
			t.Fatal(err)
		}
		c.AgentBinary = bin
		c.AliveWindow, c.OfflineAfter = time.Hour, time.Hour
	})
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	v, secret := l.signedIn(graceID).lent("shelf-mac")
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification})
	first := v.snapshot().FirstBaseline.Job
	jobSaid(t, l.s, first, "no Availability window")
	l.s.cancelJob(first, "")
	idleAfterStop(t, l.s)

	v.mu.Lock()
	v.dirty = true
	v.mu.Unlock()
	for deadline := time.Now().Add(10 * time.Second); pendingOf(v, api.CmdCleanup) == 0; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the idle clean-up was never queued")
		}
		l.clock.Advance(11 * time.Second)
	}
	l.s.shippedTerms.Store(&newTerms)
	w := l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification})
	var resp api.CheckinResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("check-in: %d %s", w.Code, w.Body)
	}
	if !l.s.view(v).TermsWaiting {
		t.Fatal("the Mac does not wait for the terms")
	}
	for _, c := range resp.Commands {
		if c.Kind == api.CmdCleanup {
			t.Fatalf("the check-in that started the wait delivered a clean-up: %+v", c)
		}
	}
}
