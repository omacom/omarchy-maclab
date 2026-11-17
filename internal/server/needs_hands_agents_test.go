package server

import (
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// agentsSent waits for want lines "Agents" got that contain part.
func agentsSent(t *testing.T, sig *fakeSignal, part string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var got []string
		for _, l := range sig.lines() {
			if l.group == testAgentsGroup && strings.Contains(l.text, part) {
				got = append(got, l.text)
			}
		}
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A Lab-owned Mac that starts Needing hands is a line to "Agents", and so
// is its end: once each, in the words of the other "Agents" lines.
func TestALabOwnedMacNeedingHandsIsALineToAgentsWhenItStartsAndWhenItStops(t *testing.T) {
	sig := &fakeSignal{}
	l := newLabWith(t, labConfig{config: func(c *Config) { c.Signal, c.SignalDir = sig, signalDir(t) }})
	l.baseline()
	j := l.run(api.JobSpec{Kernel: l.artifact("krel=7.2-hang behavior=hang")})
	l.expect(j, api.OutcomeHung)
	d := l.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands })

	started := agentsSent(t, sig, "needs hands", 1)
	if len(started) != 1 || started[0] != "sim-mac needs hands: "+d.StateReason+". Its page says what to do." {
		t.Fatalf("the line when it started: %q", started)
	}
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	stopped := agentsSent(t, sig, "no longer needs hands", 1)
	if len(stopped) != 1 || stopped[0] != "sim-mac no longer needs hands: it is ready again." {
		t.Fatalf("the line when it stopped: %q", stopped)
	}
	time.Sleep(300 * time.Millisecond)
	if got := agentsSent(t, sig, "needs hands", 0); len(got) != 2 {
		t.Errorf("Agents got %d lines about hands, want 2: %q", len(got), got)
	}
	for _, line := range sig.lines() {
		if line.group != testAgentsGroup && strings.Contains(line.text, "needs hands") {
			t.Errorf("the Lab group got %q", line.text)
		}
	}
}

// With no "Agents" group configured the lines are recorded and nothing is sent.
func TestNoLineAboutHandsIsSentWithoutAConfiguredGroup(t *testing.T) {
	sig := &fakeSignal{}
	l := newLabWith(t, labConfig{config: func(c *Config) { c.Signal, c.SignalDir = sig, t.TempDir() }})
	l.baseline()
	l.expect(l.run(api.JobSpec{Kernel: l.artifact("krel=7.2-hang behavior=hang")}), api.OutcomeHung)
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateNeedsHands })
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	lines := l.agentsLines()
	if !has(lines, "sim-mac needs hands") || !has(lines, "sim-mac no longer needs hands") {
		t.Errorf("the lines were not recorded: %q", lines)
	}
	time.Sleep(300 * time.Millisecond)
	if sent := sig.lines(); len(sent) != 0 || sig.tried() != 0 {
		t.Errorf("sent with no group: %+v", sent)
	}
}
