package server

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/sshsig/sshsigtest"
)

// rolloutLab is a lab with the Agent rollout on, whose one Mac, of model,
// runs lab-agent number and is handed 0.12.0.
func newRolloutLab(t *testing.T, volunteer bool, number, model string) (*softwareLab, []byte) {
	t.Helper()
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.12.0")
	body, _ := os.ReadFile(handed)
	sl := &softwareLab{installed: filepath.Join(t.TempDir(), "lab-agent")}
	if err := os.WriteFile(sl.installed, []byte("#!/bin/sh\necho "+number+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sl.lab = newLabWith(t, labConfig{volunteer: volunteer, model: model, config: func(c *Config) {
		c.AgentBinary, c.LabAgent, c.AgentRollout = handed, "0.12.0", true
	}, agent: func(c *agent.Config) {
		c.Self = &agent.Self{Path: sl.installed, Version: number, Key: k.Public, Restart: func() error { return sl.restart(t, k) }}
	}})
	return sl, body
}

// trial is the rollout of the lab's lab-agent as labd holds it now.
func (sl *softwareLab) trial() agentTrial {
	r := sl.s.rollout()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.st.Trials[sl.s.agentSHA256()]
	if t == nil {
		return agentTrial{}
	}
	c := *t
	return c
}

// reconcileUntil runs the rollout's look until cond holds.
func (sl *softwareLab) reconcileUntil(what string, cond func() bool) {
	sl.t.Helper()
	for end := time.Now().Add(20 * time.Second); !cond(); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(end) {
			sl.t.Fatalf("%s: never\n%s", what, sl.logs.String())
		}
		sl.s.reconcileRollout()
	}
}

func TestTheRolloutTriesTheLabsAgentOnALabOwnedMacThenPromotesItAfterTwoFamilies(t *testing.T) {
	sl, handed := newRolloutLab(t, false, "0.11.0", "Apple MacBook Air (M1, 2020)")
	sl.baseline()
	sl.waitAgent("0.11.0")

	// The trial Mac of its family takes it by itself, and says it stays.
	sl.reconcileUntil("the trial Mac takes the lab's lab-agent", func() bool {
		now, _ := os.ReadFile(sl.installed)
		return bytes.Equal(now, handed)
	})
	sl.waitAgent("0.12.0")
	sl.reconcileUntil("the trial Mac answers a command from its new start", func() bool {
		m := sl.trial().Ran[sl.name]
		return m != nil && m.Answered && m.Family == "m1"
	})
	if sl.trial().State != trialOn {
		t.Fatalf("promoted on one family: %+v", sl.trial())
	}

	// A second family that ran it well for the trial's time promotes it.
	was := rolloutTrialFor
	rolloutTrialFor = 0
	t.Cleanup(func() { rolloutTrialFor = was })
	r := sl.s.rollout()
	r.mu.Lock()
	r.st.Trials[sl.s.agentSHA256()].Ran["other"] = &trialMac{Family: "m2", Start: "s", Since: sl.s.now(), Last: sl.s.now(), Answered: true}
	r.mu.Unlock()
	sl.reconcileUntil("promoted", func() bool { return sl.trial().State == promoted })
	lines := strings.Join(sl.agentsLines(), "\n")
	if !strings.Contains(lines, "lab-agent 0.12.0") || !strings.Contains(lines, "m1, m2") || !strings.Contains(lines, "every Mac takes it") {
		t.Errorf("the Agents line of the promotion: %s", lines)
	}
	if d := sl.s.dev(sl.name).snapshot(); d.Facts.AgentOutcome == nil || d.Facts.AgentOutcome.Outcome != api.AgentStayed {
		t.Errorf("the Mac's record of its last change: %+v", d.Facts.AgentOutcome)
	}
}

// A Volunteer's Mac takes only a promoted lab-agent, inside its Availability
// window; a Maintainer cannot hurry it past the trial.
func TestAVolunteersMacTakesTheLabsAgentOnlyOncePromotedAndInsideItsWindow(t *testing.T) {
	sl, handed := newRolloutLab(t, true, "0.11.0", "Apple MacBook Air (M1, 2020)")
	sl.waitAgent("0.11.0")
	was, _ := os.ReadFile(sl.installed)
	if _, err := sl.volunteer("open", "--until", time.Now().Add(2*time.Hour).Format("15:04")); err != nil {
		t.Fatal(err)
	}
	sl.waitDevice(func(d api.Device) bool { return d.Window != nil && d.Window.OpenUntil.After(time.Now()) })

	for range 5 {
		sl.s.reconcileRollout()
	}
	time.Sleep(time.Second)
	if now, _ := os.ReadFile(sl.installed); !bytes.Equal(now, was) {
		t.Fatal("a Volunteer's Mac took a lab-agent on trial")
	}
	resp, body := sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "on trial") {
		t.Errorf("lab agent-update of a Volunteer's Mac during the trial: %d %s", resp.StatusCode, body)
	}

	r := sl.s.rollout()
	r.mu.Lock()
	r.st.Trials[sl.s.agentSHA256()].State = promoted
	r.mu.Unlock()
	sl.reconcileUntil("the Volunteer's Mac takes the promoted lab-agent", func() bool {
		now, _ := os.ReadFile(sl.installed)
		return bytes.Equal(now, handed)
	})
	sl.waitAgent("0.12.0")
}

// A Mac that returned to its previous lab-agent stops the rollout: Agents is
// told once, and no further Mac takes it.
func TestAMacReturningToItsPreviousAgentStopsTheRollout(t *testing.T) {
	sl, _ := newRolloutLab(t, false, "0.11.0", "Apple MacBook Air (M1, 2020)")
	sl.baseline()
	sl.waitAgent("0.11.0")
	sha := sl.s.agentSHA256()
	v := sl.s.dev(sl.name)
	returned := func() {
		v.mu.Lock()
		f := v.d.Facts
		v.mu.Unlock()
		f.AgentOutcome = &api.AgentOutcome{To: "0.12.0", ToSHA256: sha, Outcome: api.AgentReturned, At: time.Now(), Why: "lab-agent 0.12.0 started 4 times without running well"}
		d := v.snapshot()
		v.heartbeat(api.Checkin{BootID: d.BootID, Kernel: d.Kernel, Facts: &f})
	}
	// A Mac labd never had switch to it cannot stop it, whatever it says.
	returned()
	if st := sl.trial(); st.State != trialOn {
		t.Fatalf("a return from a Mac labd sent nothing to stopped the rollout: %+v", st)
	}
	sl.s.rolloutSent(v.snapshot(), sha)
	returned()
	returned()
	if st := sl.trial(); st.State != rollStops || !strings.Contains(st.Why, "m1") {
		t.Fatalf("the rollout after a return: %+v", st)
	}
	var told int
	for _, l := range sl.agentsLines() {
		if strings.Contains(l, "Agent rollout") && strings.Contains(l, "stopped") {
			told++
		}
	}
	if told != 1 {
		t.Errorf("Agents was told of the stop %d times, want once: %v", told, sl.agentsLines())
	}
	was, _ := os.ReadFile(sl.installed)
	for range 5 {
		sl.s.reconcileRollout()
	}
	time.Sleep(time.Second)
	if now, _ := os.ReadFile(sl.installed); !bytes.Equal(now, was) || sl.restarts != 0 {
		t.Error("a stopped rollout still updated a Mac")
	}
}

// A trial Mac whose new lab-agent keeps starting again in one boot stops the
// rollout; a reboot, which starts it afresh, does not count.
func TestATrialMacWhoseNewAgentKeepsRestartingStopsTheRollout(t *testing.T) {
	sl, handed := newRolloutLab(t, false, "0.11.0", "Apple MacBook Air (M1, 2020)")
	sl.baseline()
	sl.waitAgent("0.11.0")
	sl.reconcileUntil("the trial Mac takes the lab's lab-agent", func() bool {
		now, _ := os.ReadFile(sl.installed)
		return bytes.Equal(now, handed)
	})
	sl.waitAgent("0.12.0")
	v := sl.s.dev(sl.name)
	start := func(boot, run string) {
		v.mu.Lock()
		f := v.d.Facts
		v.mu.Unlock()
		v.heartbeat(api.Checkin{BootID: boot, Kernel: v.snapshot().Kernel, AgentStart: run, Facts: &f})
	}
	boot := v.snapshot().BootID
	start("another-boot", "r0")
	start(boot, "r1")
	if st := sl.trial(); st.State != trialOn {
		t.Fatalf("a reboot stopped the rollout: %+v", st)
	}
	for i := 2; i <= rolloutRestartsMax+2; i++ {
		start(boot, fmt.Sprintf("r%d", i))
	}
	if st := sl.trial(); st.State != rollStops || !strings.Contains(st.Why, "in one boot") {
		t.Fatalf("the rollout after restarts in one boot: %+v", st)
	}
}

// A lab-agent too old to return to itself is updated by hand once: the
// rollout leaves it alone, as nothing would bring the Mac back.
func TestTheRolloutLeavesALabAgentThatCannotReturnToItself(t *testing.T) {
	sl, _ := newRolloutLab(t, false, agentUpdateMinAgent, "Apple MacBook Air (M1, 2020)")
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	was, _ := os.ReadFile(sl.installed)
	for range 5 {
		sl.s.reconcileRollout()
	}
	time.Sleep(time.Second)
	if now, _ := os.ReadFile(sl.installed); !bytes.Equal(now, was) || sl.restarts != 0 {
		t.Fatal("the rollout updated a lab-agent with no way back")
	}
}
