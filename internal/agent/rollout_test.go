package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/sshsig/sshsigtest"
	"github.com/omacom/omarchy-maclab/internal/window"
)

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func openWindow(t *testing.T, cfg *Config) {
	t.Helper()
	if err := saveWindow(cfg, window.Settings{OpenUntil: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

// switched is a Mac whose `sudo lab-agent update` has just put the lab's
// lab-agent in place, on trial, with clock as its clock.
func switched(t *testing.T, clock *FakeClock) (*updatingMac, []byte) {
	t.Helper()
	k := sshsigtest.NewKey(t)
	next := agentFile("0.10.0", "v0.2.0-rc.2")
	l := newUpdateLab(t, next, k.Sign(ReleaseNamespace, next))
	m := newUpdatingMac(t, l.URL, k.Public)
	m.cfg.Clock = clock
	if out, err := m.update(); err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	m.restarts = 0
	return m, next
}

func TestTheGuardReturnsToThePreviousAgentWhenTheNewOneKeepsFailingToStart(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC))
	m, next := switched(t, clock)
	for i := 1; i <= guardStartsMax; i++ {
		if did, err := Guard(m.cfg, true); err != nil || did != "" {
			t.Fatalf("start %d: %q, %v", i, did, err)
		}
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, next) {
		t.Fatal("the guard returned before the new lab-agent had its starts")
	}
	did, err := Guard(m.cfg, true)
	if err != nil || !strings.Contains(did, "returned to lab-agent 0.9.0") {
		t.Fatalf("one start too many: %q, %v", did, err)
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, m.was) {
		t.Error("the previous lab-agent is not back in place")
	}
	if m.restarts != 0 {
		t.Error("a guard run before a start restarted lab-agent itself")
	}
	o := m.cfg.lastOutcome()
	if o == nil || o.Outcome != api.AgentReturned || o.To != "0.10.0" || o.ToSHA256 != sha256Hex(next) || !strings.Contains(o.Why, "started") {
		t.Errorf("the outcome: %+v", o)
	}
	if _, err := os.Stat(m.cfg.attemptPath()); !os.IsNotExist(err) {
		t.Error("the trial is still on")
	}
}

// One that starts but never runs well, hung or cut off before its first
// check-in, is returned from by the timer, which restarts lab-agent.
func TestTheGuardReturnsToThePreviousAgentWhenTheNewOneHasNotRunWellInTime(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC))
	m, _ := switched(t, clock)
	Guard(m.cfg, true)
	clock.Advance(guardWithin - time.Second)
	if did, _ := Guard(m.cfg, false); did != "" {
		t.Fatalf("within its time: %q", did)
	}
	clock.Advance(2 * time.Second)
	if did, err := Guard(m.cfg, false); err != nil || !strings.Contains(did, "returned") {
		t.Fatalf("past its time: %q, %v", did, err)
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, m.was) || m.restarts != 1 {
		t.Errorf("past its time the previous lab-agent is not back and started (restarts %d)", m.restarts)
	}
}

func TestANewAgentThatRunsWellStaysAndTheGuardLeavesIt(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC))
	m, next := switched(t, clock)
	if ranWell(m.cfg, sha256Hex(m.was)) {
		t.Fatal("the previous lab-agent ended the new one's trial")
	}
	if !ranWell(m.cfg, sha256Hex(next)) {
		t.Fatal("the new lab-agent's word did not end its trial")
	}
	if o := m.cfg.lastOutcome(); o == nil || o.Outcome != api.AgentStayed || o.To != "0.10.0" {
		t.Errorf("the outcome: %+v", o)
	}
	clock.Advance(time.Hour)
	for _, start := range []bool{true, false} {
		if did, err := Guard(m.cfg, start); did != "" || err != nil {
			t.Errorf("the guard after the trial: %q, %v", did, err)
		}
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, next) {
		t.Error("the guard took a lab-agent that runs well out")
	}
}

// Another lab-agent put in place by hand ends the trial: the guard never
// overwrites what it did not put there.
func TestTheGuardLeavesALabAgentItDidNotPutInPlace(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC))
	m, _ := switched(t, clock)
	other := agentFile("0.10.1", "v0.2.0-rc.3")
	os.WriteFile(m.installed, other, 0o755)
	clock.Advance(time.Hour)
	if _, err := Guard(m.cfg, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, other) || m.restarts != 0 {
		t.Error("the guard replaced a lab-agent it did not put in place")
	}
}

// checkinFacts is the facts a's next check-in carries.
func checkinFacts(t *testing.T, a *Agent) api.Facts {
	t.Helper()
	checkins := make(chan api.Checkin, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ci api.Checkin
		json.NewDecoder(r.Body).Decode(&ci)
		checkins <- ci
		io.WriteString(w, "{}")
	}))
	t.Cleanup(hs.Close)
	a.cfg.Server, a.cfg.Secret = hs.URL, "test"
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ci := <-checkins
	if ci.Facts == nil {
		t.Fatal("the check-in carried no facts")
	}
	return *ci.Facts
}

func TestEachCheckinCarriesTheAgentsBytesAndHowItsLastChangeEnded(t *testing.T) {
	clock := NewFakeClock(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC))
	m, next := switched(t, clock)
	a := New(m.cfg, m.sim, nil)
	a.selfSHA = sha256Hex(next)
	a.runsWell()
	f := checkinFacts(t, a)
	if f.AgentSHA256 != sha256Hex(next) || f.AgentOutcome == nil || f.AgentOutcome.Outcome != api.AgentStayed {
		t.Errorf("the facts: sha %q outcome %+v", f.AgentSHA256, f.AgentOutcome)
	}
}
