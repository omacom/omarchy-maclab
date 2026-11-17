package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/sshsig/sshsigtest"
)

// The lab's own software on a Mac: lab-agent comes from the lab server
// only, signed; a Volunteer's Mac is never sent one; a Maintainer updates a
// Lab-owned Mac's while it is idle; the test tool comes from the lab's
// install line before each job.

// signedAgent is a lab-agent that says it is number, with its signature by
// k beside it, as the bundle of a Lab release holds the two.
func signedAgent(t *testing.T, k *sshsigtest.Key, number string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lab-agent-linux-arm64")
	body := []byte("#!/bin/sh\necho " + number + "\necho 'Lab release v0.2.0-rc.2'\n")
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if k != nil {
		if err := os.WriteFile(path+".sig", k.Sign(agent.ReleaseNamespace, body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// softwareLab is a lab whose one Mac runs lab-agent number, installed where
// the test can see it, and trusts k. restarted is the Mac's lab-agent started
// again as the number it then is.
type softwareLab struct {
	*lab
	installed string
	restarts  int
	// beforeRestart, when set, runs as the Mac's lab-agent is about to restart.
	beforeRestart func()
}

func newSoftwareLab(t *testing.T, k *sshsigtest.Key, volunteer bool, number string, config func(*Config)) *softwareLab {
	t.Helper()
	return newWrappedSoftwareLab(t, k, volunteer, number, config, nil)
}

// newWrappedSoftwareLab is newSoftwareLab with wrap in front of labd.
func newWrappedSoftwareLab(t *testing.T, k *sshsigtest.Key, volunteer bool, number string, config func(*Config), wrap func(http.Handler) http.Handler) *softwareLab {
	t.Helper()
	sl := &softwareLab{installed: filepath.Join(t.TempDir(), "lab-agent")}
	if err := os.WriteFile(sl.installed, []byte("#!/bin/sh\necho "+number+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sl.lab = newLabWith(t, labConfig{volunteer: volunteer, config: config, wrap: wrap, agent: func(c *agent.Config) {
		c.Self = &agent.Self{Path: sl.installed, Version: number, Key: k.Public, Restart: func() error { return sl.restart(t, k) }}
	}})
	return sl
}

// restart is what restarting lab-agent.service does: the running agent
// stops, and the one installed starts and says its own number.
func (sl *softwareLab) restart(t *testing.T, k *sshsigtest.Key) error {
	if sl.beforeRestart != nil {
		sl.beforeRestart()
	}
	sl.restarts++
	out, err := exec.Command(sl.installed, "version").Output()
	if err != nil {
		return err
	}
	number := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	sl.stopAgent()
	next := *sl.agent
	next.Self = &agent.Self{Path: sl.installed, Version: number, Key: k.Public, Restart: func() error { return sl.restart(t, k) }}
	ctx, stop := context.WithCancel(sl.ctx)
	sl.agent, sl.stopAgent = &next, stop
	go agent.New(&next, sl.sim, log.New(sl.logs, "labd: ", 0)).Run(ctx)
	return nil
}

func (sl *softwareLab) view() DeviceView {
	sl.t.Helper()
	resp, body := sl.get("GET", "/api/devices/"+sl.name)
	var d DeviceView
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &d) != nil {
		sl.t.Fatalf("GET the Mac: %d %s", resp.StatusCode, body)
	}
	return d
}

func (sl *softwareLab) waitAgent(number string) DeviceView {
	sl.t.Helper()
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if d := sl.view(); d.Facts.AgentVersion == number && d.Alive {
			return d
		}
	}
	sl.t.Fatalf("the Mac never said it runs lab-agent %s: %+v", number, sl.view().Facts)
	return DeviceView{}
}

// updateNotices are the Notices about a waiting update, to the Mac's own topic.
func (sl *softwareLab) updateNotices() []Notification {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	var out []Notification
	for _, n := range sl.notes {
		if strings.Contains(n.Body, "update is waiting") {
			out = append(out, n)
		}
	}
	return out
}

const payloadSpecMinAgent = payloadMinAgent

// needsNewerAgent submits a job that needs lab-agent 0.9.0 or later.
func (sl *softwareLab) needsNewerAgent() (int, string) {
	spec := api.JobSpec{Device: sl.name, Tests: []api.TestSpec{{Name: "probe", Script: sl.artifact("#!/bin/bash\ntrue\n"),
		Payload: []api.PayloadFile{{Name: "probe.bin", SHA256: sl.artifact("payload")}}}}}
	resp, body := sl.post("/api/jobs", spec)
	return resp.StatusCode, body
}

// An update of lab-agent is never pushed to a Volunteer's Mac. My Macs and
// one Notice tell its Volunteer, who runs one line at the Mac; until then
// the lab refuses what needs the newer agent.
func TestAVolunteersMacGetsANewAgentOnlyWhenItsVolunteerRunsTheLine(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, agent.Version)
	sl := newSoftwareLab(t, k, true, "0.8.0", func(c *Config) { c.AgentBinary = handed })
	was, _ := os.ReadFile(sl.installed)

	d := sl.waitAgent("0.8.0")
	if d.LabAgent != agent.Version {
		t.Fatalf("the Mac's view does not say the lab's lab-agent is newer: lab_agent %q", d.LabAgent)
	}
	if line := agentUpdateLine(d); line != "sudo lab-agent update" {
		t.Errorf("the one line for its Volunteer: %q", line)
	}

	// One Notice, fixed wording, the Mac's name, nothing to run.
	var told []Notification
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && len(told) == 0; time.Sleep(20 * time.Millisecond) {
		told = sl.updateNotices()
	}
	if len(told) != 1 {
		t.Fatalf("Notices about the update: %+v, want one", told)
	}
	n := told[0]
	if !n.Volunteer || n.Title != "Omarchy Mac Lab" || n.Body != sl.name+": An update is waiting for this Mac. Open My Macs." {
		t.Errorf("the Notice: %+v", n)
	}
	for _, never := range []string{"sudo", "lab-agent", "0.8.0", agent.Version} {
		if strings.Contains(n.Title+n.Body, never) {
			t.Errorf("the Notice says %q: a Notice never gives a command or a version: %q", never, n.Body)
		}
	}

	// Nothing pushes it: a Maintainer's lab agent-update is refused by labd,
	// and no command reaches the Mac.
	resp, body := sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "never pushed to a Volunteer's Mac") || !strings.Contains(body, "sudo lab-agent update") {
		t.Errorf("lab agent-update on a Volunteer's Mac: %d %s", resp.StatusCode, body)
	}
	if now, _ := os.ReadFile(sl.installed); !bytes.Equal(now, was) || sl.restarts != 0 {
		t.Fatal("the Volunteer's Mac got a new lab-agent without its Volunteer")
	}

	// Until then the lab refuses what needs the newer agent.
	if _, err := sl.volunteer("access", "full"); err != nil {
		t.Fatal(err)
	}
	if _, err := sl.volunteer("open", "--until", time.Now().Add(2*time.Hour).Format("15:04")); err != nil {
		t.Fatal(err)
	}
	sl.waitDevice(func(d api.Device) bool { return d.AccessLevel == api.AccessFull })
	code, said := sl.needsNewerAgent()
	if code != http.StatusConflict || !strings.Contains(said, "runs lab-agent 0.8.0") || !strings.Contains(said, payloadSpecMinAgent+" or later") {
		t.Fatalf("a job that needs lab-agent %s, on 0.8.0: %d %s", payloadSpecMinAgent, code, said)
	}

	// The Volunteer runs the one line at the Mac.
	out, err := sl.volunteer("update")
	if err != nil {
		t.Fatalf("sudo lab-agent update: %v\n%s", err, out)
	}
	if now, _ := os.ReadFile(sl.installed); bytes.Equal(now, was) || sl.restarts != 1 {
		t.Fatalf("after update: restarts %d, installed changed %v\n%s", sl.restarts, !bytes.Equal(now, was), out)
	}
	if d := sl.waitAgent(agent.Version); d.LabAgent != "" || agentUpdateLine(d) != "" {
		t.Errorf("an up-to-date Mac is still said to be behind: lab_agent %q", d.LabAgent)
	}
	if code, said := sl.needsNewerAgent(); strings.Contains(said, "runs lab-agent") {
		t.Errorf("after the update the lab still refuses for the agent: %d %s", code, said)
	}
	time.Sleep(500 * time.Millisecond)
	if told := sl.updateNotices(); len(told) != 1 {
		t.Errorf("Notices about the update after it: %d, want the one", len(told))
	}
}

// labd restarting, or the Mac checking in again, sends no second Notice for
// the same lab-agent.
func TestTheNoticeAboutAnUpdateGoesOncePerAgentOfTheLab(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, agent.Version)
	sl := newSoftwareLab(t, k, true, "0.8.0", func(c *Config) { c.AgentBinary = handed })
	sl.waitAgent("0.8.0")
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end) && len(sl.updateNotices()) == 0; time.Sleep(20 * time.Millisecond) {
	}
	v := sl.s.dev(sl.name)
	for i := 0; i < 3; i++ {
		v.mu.Lock()
		f := v.d.Facts
		v.mu.Unlock()
		v.heartbeat(api.Checkin{BootID: v.snapshot().BootID, Kernel: v.snapshot().Kernel, Facts: &f})
	}
	if got := sl.updateNotices(); len(got) != 1 {
		t.Fatalf("Notices about the update: %d, want one", len(got))
	}
	if d := v.snapshot(); d.UpdateNoticed != agent.Version {
		t.Errorf("the Mac's record does not keep which lab-agent its Volunteer was told of: %q", d.UpdateNoticed)
	}
}

// lab agent-update is a Maintainer's, for a Lab-owned Mac, only while it is
// idle; the Mac checks the new agent itself.
func TestAMaintainerUpdatesALabOwnedMacsAgentWhileItIsIdle(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.11.0")
	sl := newSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, "0.11.0" })
	sl.baseline()
	if d := sl.waitAgent(agentUpdateMinAgent); d.LabAgent != "0.11.0" {
		t.Fatalf("lab_agent %q, want 0.11.0", d.LabAgent)
	}
	if len(sl.updateNotices()) != 0 {
		t.Error("a Lab-owned Mac got a Notice about its lab-agent")
	}
	path := "/api/devices/" + sl.name + "/agent-update"

	// Maintainers only.
	grace := sl.member("grace")
	if resp, body := sl.as(grace.Token, "POST", path, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("an M team member's agent-update: %d %s", resp.StatusCode, body)
	}
	if resp, _ := sl.as("", "POST", path, nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("agent-update with no token: %d", resp.StatusCode)
	}

	// Not while a Lease holds it.
	if resp, body := sl.post("/api/devices/"+sl.name+"/lease", api.LeaseRequest{Holder: "t"}); resp.StatusCode != 200 {
		t.Fatalf("lease: %d %s", resp.StatusCode, body)
	}
	if resp, body := sl.post(path, nil); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "not idle") || !strings.Contains(body, "Lease") {
		t.Errorf("agent-update under a Lease: %d %s", resp.StatusCode, body)
	}
	if resp, body := sl.as(harnessToken, "DELETE", "/api/devices/"+sl.name+"/lease", nil); resp.StatusCode != 200 {
		t.Fatalf("release: %d %s", resp.StatusCode, body)
	}

	// Not while a job runs on it.
	sl.sim.SetTestLength(3 * time.Second)
	j, _, err := sl.s.Submit(api.JobSpec{Device: sl.name, Tests: []api.TestSpec{{Name: "boot-health", Builtin: "boot-health"}}})
	if err != nil {
		t.Fatal(err)
	}
	sl.waitDevice(func(d api.Device) bool { return d.ActiveJob == j.ID })
	if resp, body := sl.post(path, nil); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "not idle") || !strings.Contains(body, j.ID) {
		t.Errorf("agent-update during a job: %d %s", resp.StatusCode, body)
	}
	sl.wait(j.ID, 60*time.Second)
	sl.waitDevice(func(d api.Device) bool { return d.ActiveJob == "" && d.State == api.StateReady })
	if sl.restarts != 0 {
		t.Fatal("the agent was restarted by a refused update")
	}

	// Idle: it updates, and says from what to what.
	sl.waitAgent(agentUpdateMinAgent)
	resp, body := sl.post(path, nil)
	var res api.AgentUpdate
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &res) != nil || res != (api.AgentUpdate{From: agentUpdateMinAgent, To: "0.11.0", LabRelease: "v0.2.0-rc.2", Changed: true}) {
		t.Fatalf("agent-update: %d %s", resp.StatusCode, body)
	}
	if d := sl.waitAgent("0.11.0"); d.LabAgent != "" {
		t.Errorf("after the update lab_agent is %q", d.LabAgent)
	}
	if sl.restarts != 1 {
		t.Errorf("restarts: %d", sl.restarts)
	}
	// Again: nothing to do.
	resp, body = sl.post(path, nil)
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &res) != nil || res.Changed || res.To != "0.11.0" {
		t.Errorf("agent-update of an up-to-date Mac: %d %s", resp.StatusCode, body)
	}
	if resp, _ := sl.post("/api/devices/nope/agent-update", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("agent-update of no Mac: %d", resp.StatusCode)
	}
}

// While lab agent-update is on its way, and until the Mac's lab-agent is
// back as the new one, nothing new starts on the Mac: no Lease, no job, no
// exec (spec: "while it is idle").
func TestAgentUpdateReservesTheMacUntilTheNewAgentIsBack(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.11.0")
	fetching, fetch := make(chan struct{}), make(chan struct{})
	var once sync.Once
	wrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && r.URL.Path == "/api/agent/lab-agent" {
				once.Do(func() { close(fetching); <-fetch })
			}
			h.ServeHTTP(w, r)
		})
	}
	sl := newWrappedSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, "0.11.0" }, wrap)
	restarting, restart := make(chan struct{}), make(chan struct{})
	sl.beforeRestart = func() { close(restarting); <-restart }
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	leasePath, execPath := "/api/devices/"+sl.name+"/lease", "/api/devices/"+sl.name+"/exec"

	type answer struct {
		code int
		body string
	}
	updated := make(chan answer, 1)
	go func() {
		resp, body := sl.post("/api/devices/"+sl.name+"/agent-update", nil)
		updated <- answer{resp.StatusCode, body}
	}()
	select {
	case <-fetching:
	case <-time.After(15 * time.Second):
		t.Fatal("the Mac never fetched the lab's lab-agent")
	}

	reserved := func(when string) {
		t.Helper()
		if resp, body := sl.post(leasePath, api.LeaseRequest{Holder: "t"}); resp.StatusCode != http.StatusConflict || !strings.Contains(body, "lab-agent is being updated") {
			t.Errorf("a Lease %s: %d %s", when, resp.StatusCode, body)
			sl.as(harnessToken, "DELETE", leasePath, nil)
		}
		if resp, body := sl.post(execPath, api.ExecArgs{Command: "true"}); resp.StatusCode == 200 {
			t.Errorf("an exec %s: %d %s", when, resp.StatusCode, body)
		}
	}
	reserved("while the update downloads")
	sl.sim.SetTestLength(0)
	j, _, err := sl.s.Submit(api.JobSpec{Device: sl.name, Tests: []api.TestSpec{{Name: "boot-health", Builtin: "boot-health"}}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if d := sl.s.dev(sl.name).snapshot(); d.ActiveJob != "" {
		t.Errorf("job %s was sent while the update downloads", d.ActiveJob)
	}

	close(fetch)
	var res api.AgentUpdate
	if a := <-updated; a.code != 200 || json.Unmarshal([]byte(a.body), &res) != nil || !res.Changed || res.To != "0.11.0" {
		t.Fatalf("agent-update: %d %s", a.code, a.body)
	}
	select {
	case <-restarting:
	case <-time.After(15 * time.Second):
		t.Fatal("the Mac's lab-agent never restarted")
	}
	reserved("before the new lab-agent is back")
	if d := sl.s.dev(sl.name).snapshot(); d.ActiveJob != "" {
		t.Errorf("job %s was sent before the new lab-agent was back", d.ActiveJob)
	}

	close(restart)
	sl.waitAgent("0.11.0")
	if got := sl.wait(j.ID, 60*time.Second); got.Outcome != api.OutcomePass {
		t.Errorf("the job held back by the update: %s %s", got.Outcome, got.Summary)
	}
	sl.waitDevice(func(d api.Device) bool { return d.ActiveJob == "" && d.State == api.StateReady })
	if resp, body := sl.post(leasePath, api.LeaseRequest{Holder: "t"}); resp.StatusCode != 200 {
		t.Fatalf("a Lease once the new lab-agent is back: %d %s", resp.StatusCode, body)
	}
	if resp, body := sl.post(execPath, api.ExecArgs{Command: "true"}); resp.StatusCode != 200 {
		t.Errorf("an exec once the new lab-agent is back: %d %s", resp.StatusCode, body)
	}
}

// The usual update hands out new bytes of the same lab-agent number: the
// Mac stays reserved until the restarted lab-agent checks in, not from the
// moment the answer comes, when the number labd holds already matches.
func TestASameVersionUpdateHoldsTheMacUntilTheRestartedAgentChecksIn(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, agentUpdateMinAgent)
	sl := newSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, agentUpdateMinAgent })
	restarting, restart := make(chan struct{}), make(chan struct{})
	sl.beforeRestart = func() { close(restarting); <-restart }
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	leasePath := "/api/devices/" + sl.name + "/lease"

	resp, body := sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	var res api.AgentUpdate
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &res) != nil || !res.Changed || res.To != agentUpdateMinAgent {
		t.Fatalf("agent-update: %d %s", resp.StatusCode, body)
	}
	select {
	case <-restarting:
	case <-time.After(15 * time.Second):
		t.Fatal("the Mac's lab-agent never restarted")
	}
	time.Sleep(500 * time.Millisecond) // the old agent's check-ins go on meanwhile
	if resp, body := sl.post(leasePath, api.LeaseRequest{Holder: "t"}); resp.StatusCode != http.StatusConflict || !strings.Contains(body, agentBeingUpdated) {
		t.Fatalf("a Lease before the restarted lab-agent is back: %d %s", resp.StatusCode, body)
	}
	close(restart)
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		resp, body := sl.post(leasePath, api.LeaseRequest{Holder: "t"})
		if resp.StatusCode == 200 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the Mac stayed reserved after its restarted lab-agent checked in: %d %s", resp.StatusCode, body)
		}
	}
}

// A Maintainer who goes away mid-update cuts labd's wait, not the Mac's
// download: the Mac stays reserved for the hold after the call ends, and the
// update labd gave up on installs without restarting lab-agent.
func TestADisconnectedUpdateHoldsTheMacAndRestartsNothing(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.11.0")
	fetching, fetch := make(chan struct{}), make(chan struct{})
	var once, let sync.Once
	letFetch := func() { let.Do(func() { close(fetch) }) }
	wrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && r.URL.Path == "/api/agent/lab-agent" {
				once.Do(func() { close(fetching); <-fetch })
			}
			h.ServeHTTP(w, r)
		})
	}
	sl := newWrappedSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, "0.11.0" }, wrap)
	t.Cleanup(letFetch) // before the lab's own cleanup, which waits on the download
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	v := sl.s.dev(sl.name)
	v.mu.Lock()
	v.updateHold = 3 * time.Second
	v.mu.Unlock()
	leasePath := "/api/devices/" + sl.name + "/lease"

	ctx, cut := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", sl.url+"/api/devices/"+sl.name+"/agent-update", nil)
	req.Header.Set("Authorization", "Bearer "+harnessToken)
	go func() {
		if resp, err := sl.client().Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-fetching:
	case <-time.After(15 * time.Second):
		t.Fatal("the Mac never fetched the lab's lab-agent")
	}
	cut()
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		v.mu.Lock()
		gaveUp := len(v.gaveUp)
		v.mu.Unlock()
		if gaveUp > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("labd never stopped waiting for the update its Maintainer left")
		}
	}
	time.Sleep(200 * time.Millisecond)
	if resp, body := sl.post(leasePath, api.LeaseRequest{Holder: "t"}); resp.StatusCode != http.StatusConflict || !strings.Contains(body, agentBeingUpdated) {
		sl.as(harnessToken, "DELETE", leasePath, nil)
		t.Fatalf("a Lease just after the Maintainer left, the download still going: %d %s", resp.StatusCode, body)
	}

	letFetch()
	for end := time.Now().Add(15 * time.Second); !strings.Contains(sl.logs.String(), "labd gave up waiting for it"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the Mac never heard labd gave up on the update:\n%s", sl.logs.String())
		}
	}
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		resp, body := sl.post(leasePath, api.LeaseRequest{Holder: "t"})
		if resp.StatusCode == 200 {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("the Mac stayed reserved past the hold: %d %s", resp.StatusCode, body)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if sl.restarts != 0 {
		t.Fatalf("an update labd gave up on restarted lab-agent %d times", sl.restarts)
	}
}

// The idle clean-up rewrites boot entries as root: it is not sent while lab
// agent-update holds the Mac, whose restart could cut it.
func TestAnIdleCleanUpIsNotSentWhileTheAgentIsUpdated(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.11.0")
	fetching, fetch := make(chan struct{}), make(chan struct{})
	var once, let sync.Once
	letFetch := func() { let.Do(func() { close(fetch) }) }
	wrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && r.URL.Path == "/api/agent/lab-agent" {
				once.Do(func() { close(fetching); <-fetch })
			}
			h.ServeHTTP(w, r)
		})
	}
	sl := newWrappedSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, "0.11.0" }, wrap)
	t.Cleanup(letFetch)
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	v := sl.s.dev(sl.name)
	updated := make(chan struct{})
	go func() {
		sl.post("/api/devices/"+sl.name+"/agent-update", nil)
		close(updated)
	}()
	select {
	case <-fetching:
	case <-time.After(15 * time.Second):
		t.Fatal("the Mac never fetched the lab's lab-agent")
	}
	v.mu.Lock()
	v.dirty = true // a lab kernel left staged
	v.mu.Unlock()
	time.Sleep(11 * time.Second) // past the Mac's loop's look at it
	v.mu.Lock()
	dirty := v.dirty
	v.mu.Unlock()
	if !dirty || strings.Contains(sl.logs.String(), "removed leftover lab kernels") {
		t.Fatal("the idle clean-up was sent while lab agent-update held the Mac")
	}
	letFetch()
	<-updated
}

func TestAgentUpdateRefusesWhatItCannotDo(t *testing.T) {
	k := sshsigtest.NewKey(t)

	// A Mac whose agent was not signed by the key its running agent pins
	// keeps the agent it has.
	other := sshsigtest.NewKey(t)
	sl := newSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = signedAgent(t, other, "0.11.0"), "0.11.0" })
	sl.passedBaseline()
	sl.waitAgent(agentUpdateMinAgent)
	was, _ := os.ReadFile(sl.installed)
	resp, body := sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "not signed by the release key") || !strings.Contains(body, "Nothing was changed") {
		t.Errorf("an agent signed by another key: %d %s", resp.StatusCode, body)
	}
	if now, _ := os.ReadFile(sl.installed); !bytes.Equal(now, was) || sl.restarts != 0 {
		t.Error("the Mac took an agent signed by another key")
	}
	// A failed update holds the Mac no longer.
	if resp, body := sl.post("/api/devices/"+sl.name+"/lease", api.LeaseRequest{Holder: "t"}); resp.StatusCode != 200 {
		t.Errorf("a Lease after a failed update: %d %s", resp.StatusCode, body)
	}

	// The lab hands out an agent older than the Mac's.
	sl = newSoftwareLab(t, k, false, "0.12.0", func(c *Config) { c.AgentBinary, c.LabAgent = signedAgent(t, k, "0.11.0"), "0.11.0" })
	sl.passedBaseline()
	if d := sl.waitAgent("0.12.0"); d.LabAgent != "" {
		t.Errorf("a Mac ahead of the lab is said to be behind: %q", d.LabAgent)
	}
	resp, body = sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	if resp.StatusCode != http.StatusBadGateway || !strings.Contains(body, "older than the 0.12.0 installed") {
		t.Errorf("an older agent: %d %s", resp.StatusCode, body)
	}

	// An agent from before update existed cannot be told to.
	sl = newSoftwareLab(t, k, false, "0.9.0", func(c *Config) { c.AgentBinary = signedAgent(t, k, agent.Version) })
	sl.passedBaseline()
	sl.waitAgent("0.9.0")
	resp, body = sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "runs lab-agent 0.9.0") || !strings.Contains(body, agentUpdateMinAgent+" or later") {
		t.Errorf("an agent older than %s: %d %s", agentUpdateMinAgent, resp.StatusCode, body)
	}

	// A lab with no signed agent hands none out.
	sl = newSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = signedAgent(t, nil, "0.11.0"), "0.11.0" })
	sl.passedBaseline()
	sl.waitAgent(agentUpdateMinAgent)
	resp, body = sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body, "no signed lab-agent") {
		t.Errorf("a lab with no signed agent: %d %s", resp.StatusCode, body)
	}
	if resp, _ := sl.as(sl.secret, "GET", "/api/agent/lab-agent.sig", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("the signature of an unsigned agent: %d", resp.StatusCode)
	}
}

// The agent and its signature are the Mac's own to fetch, with its device
// secret, and nobody else's.
func TestTheLabHandsItsAgentToAnEnrolledMacOnly(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, agent.Version)
	sl := newSoftwareLab(t, k, false, agent.Version, func(c *Config) { c.AgentBinary = handed })
	want, _ := os.ReadFile(handed)
	sig, _ := os.ReadFile(handed + ".sig")
	if resp, body := sl.as(sl.secret, "GET", "/api/agent/lab-agent", nil); resp.StatusCode != 200 || body != string(want) {
		t.Errorf("the Mac's own download: %d", resp.StatusCode)
	}
	if resp, body := sl.as(sl.secret, "GET", "/api/agent/lab-agent.sig", nil); resp.StatusCode != 200 || body != string(sig) {
		t.Errorf("the Mac's own download of the signature: %d", resp.StatusCode)
	}
	for _, path := range []string{"/api/agent/lab-agent", "/api/agent/lab-agent.sig"} {
		for who, token := range map[string]string{"nobody": "", "a Maintainer": harnessToken, "a made-up secret": "dev_nope"} {
			if resp, _ := sl.as(token, "GET", path, nil); resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s fetching %s: %d", who, path, resp.StatusCode)
			}
		}
	}
}

// Macs and a Mac's page show an agent older than the lab's with its
// version. The one line is its Volunteer's, on their own Mac; a Maintainer
// is given the command for a Lab-owned Mac.
func TestMacsShowsAnAgentOlderThanTheLabsAndWhoUpdatesIt(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", func(d *api.Device) { d.Facts.AgentVersion = "0.2.0" })
	l.lrMac("m2-air", "j413", func(d *api.Device) { d.Facts.AgentVersion = agent.Version })
	l.lrMac("vol-air", "j313", func(d *api.Device) {
		lrVolunteers(api.AccessQualification, l.s.now().Add(2*time.Hour))(d)
		d.Facts.AgentVersion = "0.2.0"
	})
	l.lrLend("vol-air", lrLinus)
	grace, ravi, linus := l.lrMember(lrGrace), l.lrMaintainer(), l.signedIn(lrLinus)

	rows, _ := macRows(ravi.macsPage(""))
	behind := "lab-agent 0.2.0, older than the lab's " + agent.Version
	for _, mac := range []string{"m1-pro", "vol-air"} {
		if !strings.Contains(rowText(rows[mac]), behind) {
			t.Errorf("Macs does not show %s's older agent: %s", mac, rowText(rows[mac]))
		}
	}
	if strings.Contains(rowText(rows["m2-air"]), "lab-agent") {
		t.Errorf("Macs says something of an agent that is the lab's: %s", rowText(rows["m2-air"]))
	}

	facts := func(b *browser, mac string) string {
		return rowText(part(t, b.macPage(mac, ""), `id="mac-facts"`, "</section>"))
	}
	if got := facts(ravi, "m1-pro"); !strings.Contains(got, "lab-agent 0.2.0 older than the lab's "+agent.Version) || !strings.Contains(got, "lab agent-update m1-pro") {
		t.Errorf("a Maintainer's page of a Lab-owned Mac: %s", got)
	}
	if got := facts(grace, "m1-pro"); !strings.Contains(got, "older than the lab's") || strings.Contains(got, "agent-update") {
		t.Errorf("an M team member's page of a Lab-owned Mac: %s", got)
	}
	if got := facts(ravi, "vol-air"); !strings.Contains(got, "older than the lab's") || strings.Contains(got, "agent-update") || !strings.Contains(got, "its Volunteer updates it at the Mac") {
		t.Errorf("a Maintainer's page of a Volunteer's Mac: %s", got)
	}
	own := part(t, linus.macPage("vol-air", ""), `id="mac-facts"`, "</section>")
	if !strings.Contains(own, "<code data-copy>sudo lab-agent update</code>") || strings.Count(own, "lab-agent update") != 1 {
		t.Errorf("the Volunteer's own page does not give the one line: %s", rowText(own))
	}
	offersToCopy(t, own, "the update command", "sudo lab-agent update")
	offersToCopy(t, part(t, ravi.macPage("m1-pro", ""), `id="mac-facts"`, "</section>"), "the update command", "lab agent-update m1-pro")
	if got := facts(ravi, "m2-air"); strings.Contains(got, "older than") || strings.Contains(got, "agent-update") {
		t.Errorf("a Mac on the lab's agent: %s", got)
	}

	// What My Macs puts on a card (slice 38).
	for mac, want := range map[string]string{"vol-air": "sudo lab-agent update", "m1-pro": "", "m2-air": ""} {
		if got := agentUpdateLine(l.s.view(l.s.dev(mac))); got != want {
			t.Errorf("the line for %s's Volunteer: %q, want %q", mac, got, want)
		}
	}
}

// The Join command checks the first agent against the release key written
// into the script labd serves at /join for the current release.
func TestTheJoinCommandChecksTheAgentsSignatureAgainstTheKeyInItsScript(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	k, other := sshsigtest.NewKey(t), sshsigtest.NewKey(t)
	handed := signedAgent(t, k, agent.Version)
	pub, _ := ParsePublicURL("https://lab.omarchy-mac.org")
	s, err := newWith("ada", "lab_adm_secret", t.Context(), Config{DataDir: t.TempDir(), PublicURL: pub, AgentBinary: handed, ReleaseKey: k.Public}, log.New(os.Stderr, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	const token = "enr_0123456789abcdef0123456789abcdef"
	line := s.joinCommand(httptest.NewRequest("POST", "https://lab.omarchy-mac.org/api/enroll-tokens", nil), token)
	script := string(s.joinScript(httptest.NewRequest("GET", "https://lab.omarchy-mac.org/join", nil)))
	for _, want := range []string{`"$u/api/join/lab-agent.sig"`, "ssh-keygen -Y verify", agent.ReleaseSigner + ` namespaces="` + agent.ReleaseNamespace + `" ` + k.Public,
		`-n '` + agent.ReleaseNamespace + `'`, `-I '` + agent.ReleaseSigner + `'`, `-s "$d/lab-agent.sig" < "$d/lab-agent"`, "sha256sum -c --quiet"} {
		if !strings.Contains(script, want) {
			t.Fatalf("the Join command's script lacks %q:\n%s", want, script)
		}
	}
	if strings.Count(line, token) != 1 || strings.Contains(line, "\n") || strings.Contains(script, token) {
		t.Errorf("the token is in the line more than once, or in the script, or the line is more than one:\n%s", line)
	}
	if i, j := strings.Index(script, "ssh-keygen -Y verify"), strings.Index(script, "sudo "); i < 0 || j < i {
		t.Errorf("the signature is not checked before anything runs as root:\n%s", script)
	}

	// The line itself, run by bash: curl hands out what the lab would, sudo
	// records that it was reached.
	agentBody, _ := os.ReadFile(handed)
	sig, _ := os.ReadFile(handed + ".sig")
	jr := newJoinRun(t, []byte(script), agentBody, sig)
	run := func(agentBody, sig []byte) (string, bool) {
		jr.put("agent", agentBody)
		jr.put("sig", sig)
		return jr.run(line)
	}
	if out, reached := run(agentBody, sig); !reached {
		t.Fatalf("the signed agent was not run:\n%s", out)
	}
	if out, reached := run(agentBody, other.Sign(agent.ReleaseNamespace, agentBody)); reached || !strings.Contains(out, "release key") {
		t.Errorf("an agent signed by another key was run, or the line did not say why not:\n%s", out)
	}
	if out, reached := run(agentBody, []byte("not a signature\n")); reached {
		t.Errorf("an agent with no signature was run:\n%s", out)
	}

	// The signature is the token's to fetch, like the agent.
	h := s.Handler()
	ask := func(bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "https://lab.omarchy-mac.org/api/join/lab-agent.sig", nil)
		r = r.WithContext(onTunnel(r.Context()))
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("CF-Connecting-IP", "203.0.113.9")
		r.Header.Set("User-Agent", "lab-join")
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		r.RemoteAddr = viaProxy
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	tok, _ := s.store.addTokenForTest("shelf")
	if w := ask(tok); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), sig) {
		t.Errorf("the signature with the Join command's token: %d", w.Code)
	}
	if w := ask(""); w.Code != http.StatusUnauthorized {
		t.Errorf("the signature with no token: %d", w.Code)
	}

	// A labd built as a Lab release with no signature beside its agent makes
	// no Join command: its line would have nothing to check.
	unsigned := signedAgent(t, nil, agent.Version)
	s2 := &Server{cfg: Config{AgentBinary: unsigned, Release: "v0.2.0-rc.1", PublicURL: pub}}
	if line := s2.joinCommand(httptest.NewRequest("POST", "https://lab.omarchy-mac.org/", nil), token); line != "" {
		t.Errorf("a Lab release with an unsigned agent makes a Join command:\n%s", line)
	}
}

// omtRelease is a release of the test tool as the install line finds it: the
// tarball and its signature by k, in a folder.
func omtRelease(t *testing.T, k *sshsigtest.Key, version string) string {
	t.Helper()
	tool := "#!/bin/sh\ncase \"$1\" in --version) echo \"omarchy-m-test " + version + "\"; exit 0 ;; --help) echo '--unattended --dry-run --output'; exit 0 ;; esac\n" +
		"out=\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output ] && out=$2; shift; done\necho '  PASS wifi'\necho '{\"tool_version\":\"" + version + "\"}' > \"$out\"\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{{"omarchy-m-test/bin/omarchy-m-test", tool, 0o755}, {"omarchy-m-test/VERSION", version + "\n", 0o644}} {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "omarchy-m-test.tar.gz"), buf.Bytes(), 0o644)
	os.WriteFile(filepath.Join(dir, "omarchy-m-test.tar.gz.sig"), k.Sign(agent.ReleaseNamespace, buf.Bytes()), 0o644)
	return dir
}

// Before each job the built-in test installs the test tool from the lab's
// install line, whose script checks the release's signature.
func TestBeforeEachJobTheBuiltInTestInstallsTheTestToolFromTheLabsInstallLine(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "ssh-keygen", "tar", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	k, other := sshsigtest.NewKey(t), sshsigtest.NewKey(t)
	l := newLabWith(t, labConfig{config: func(c *Config) { c.ReleaseKey = k.Public }})
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	mac := &agent.Linux{WorkDir: t.TempDir(), Server: l.url, ReleaseKey: k.Public}
	run := func(job, releases string) (api.TestResult, string) {
		t.Setenv("OMARCHY_M_TEST_RELEASES", "file://"+releases)
		out := filepath.Join(t.TempDir(), job)
		os.MkdirAll(out, 0o755)
		res := mac.RunTest(t.Context(), job, api.TestSpec{Name: "omarchy-m-test", Builtin: "omarchy-m-test", TimeoutSec: 120}, nil, nil, out)
		log, _ := os.ReadFile(filepath.Join(out, "log.txt"))
		return res, string(log)
	}
	installed := func() string {
		b, _ := os.ReadFile(filepath.Join(home, ".local", "share", "omarchy-m-test", "VERSION"))
		return strings.TrimSpace(string(b))
	}

	// A release signed by another key is not installed, and with no tool
	// there is nothing to run.
	res, said := run("j1", omtRelease(t, other, "9.9.9"))
	if res.Passed || !strings.Contains(said, "signature doesn't match") || !strings.Contains(said, "FAIL: could not install omarchy-m-test") || installed() != "" {
		t.Fatalf("a release signed by another key: passed %v, installed %q\n%s", res.Passed, installed(), said)
	}

	// The lab's release: installed from the lab's own line, then run.
	res, said = run("j2", omtRelease(t, k, "9.9.9"))
	if !res.Passed || installed() != "9.9.9" || !strings.Contains(said, l.url+"/install") || !strings.Contains(said, "Signature verified") || !strings.Contains(said, "omarchy-m-test 9.9.9 on ") {
		t.Fatalf("the lab's release: passed %v, installed %q\n%s", res.Passed, installed(), said)
	}

	// The next job installs again: the latest before each run.
	res, said = run("j3", omtRelease(t, k, "9.9.10"))
	if !res.Passed || installed() != "9.9.10" || !strings.Contains(said, "Upgraded omarchy-m-test 9.9.9 -> 9.9.10") {
		t.Fatalf("the next job: passed %v, installed %q\n%s", res.Passed, installed(), said)
	}

	// A later release with a bad signature leaves the checked one in place,
	// and the job runs with it.
	res, said = run("j4", omtRelease(t, other, "9.9.11"))
	if !res.Passed || installed() != "9.9.10" || !strings.Contains(said, "install failed; running 9.9.10") {
		t.Fatalf("a bad release over a good one: passed %v, installed %q\n%s", res.Passed, installed(), said)
	}

	// The key lab-agent pins is the one asked, not the one the line pins.
	mac.ReleaseKey = other.Public
	res, said = run("j5", omtRelease(t, k, "9.9.12"))
	if installed() != "9.9.10" || !strings.Contains(said, "doesn't match the release key this lab-agent pins") {
		t.Fatalf("a release signed by the line's key, not lab-agent's: installed %q\n%s", installed(), said)
	}

	// The script names the lab and nobody else's install line.
	script, err := os.ReadFile(filepath.Join("..", "agent", "tests", "omarchy-m-test.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, never := range []string{"omarchy-m-testing.org/install", "releases/latest", "maralcbr/omarchy-m-testing/releases"} {
		if strings.Contains(string(script), never) {
			t.Errorf("the built-in test still names %q", never)
		}
	}
}

// The lab's install line is read for where the release is and never run:
// what the built-in test installs is a release whose signature it checked
// itself, against the release key the running lab-agent pins. A line that
// pins that key in a comment and runs a command runs nothing.
func TestTheBuiltInTestRunsNothingOfTheLabsInstallLine(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "ssh-keygen", "tar", "gzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	k, other := sshsigtest.NewKey(t), sshsigtest.NewKey(t)
	real := newReportsLab(t, func(c *Config) { c.ReleaseKey = k.Public }).do("GET", "/install", "").Body.String()
	const latest = "https://github.com/omacom/omarchy-maclab/releases/latest/download"
	var line string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/install" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, line)
	}))
	t.Cleanup(stub.Close)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("OMARCHY_M_TEST_RELEASES", "")
	os.Unsetenv("OMARCHY_M_TEST_RELEASES")
	ran := filepath.Join(home, "ran")
	mac := &agent.Linux{WorkDir: t.TempDir(), Server: stub.URL, ReleaseKey: k.Public}
	run := func(job string) (api.TestResult, string) {
		out := filepath.Join(t.TempDir(), job)
		os.MkdirAll(out, 0o755)
		res := mac.RunTest(t.Context(), job, api.TestSpec{Name: "omarchy-m-test", Builtin: "omarchy-m-test", TimeoutSec: 120}, nil, nil, out)
		log, _ := os.ReadFile(filepath.Join(out, "log.txt"))
		return res, string(log)
	}
	installed := func() string {
		b, _ := os.ReadFile(filepath.Join(home, ".local", "share", "omarchy-m-test", "VERSION"))
		return strings.TrimSpace(string(b))
	}

	// A line that pins the key in a comment and runs a command.
	line = "#!/usr/bin/env bash\n# release@omarchy-m-testing.org namespaces=\"omarchy-m-test-release\" " + k.Public + "\ntouch \"$HOME/ran\"\n"
	res, said := run("j1")
	if _, err := os.Stat(ran); err == nil || res.Passed || installed() != "" {
		t.Fatalf("the lab's line ran: passed %v, installed %q\n%s", res.Passed, installed(), said)
	}

	// The line as labd serves it, naming a release here, with a command
	// added: the release it names is installed, and the command never runs.
	line = strings.Replace(real, latest, "file://"+omtRelease(t, k, "9.9.9"), 1) + "\ntouch \"$HOME/ran\"\n"
	res, said = run("j2")
	if _, err := os.Stat(ran); err == nil || !res.Passed || installed() != "9.9.9" || !strings.Contains(said, "Signature verified") {
		t.Fatalf("the lab's line with a command: passed %v, installed %q\n%s", res.Passed, installed(), said)
	}

	// A release signed by a key the line pins and lab-agent does not.
	line = strings.Replace(strings.Replace(real, k.Public, other.Public, 1), latest, "file://"+omtRelease(t, other, "9.9.10"), 1)
	res, said = run("j3")
	if installed() != "9.9.9" || !strings.Contains(said, "signature doesn't match") || !strings.Contains(said, "install failed; running 9.9.9") {
		t.Fatalf("a release signed by the line's key: installed %q\n%s", installed(), said)
	}

	// A lab-agent that pins no release key installs nothing.
	mac.ReleaseKey = ""
	line = strings.Replace(real, latest, "file://"+omtRelease(t, k, "9.9.11"), 1)
	if _, said = run("j4"); installed() != "9.9.9" || !strings.Contains(said, "pins no release key") {
		t.Fatalf("a lab-agent with no key: installed %q\n%s", installed(), said)
	}
}

// labRelease is a Lab release's lab for this machine as the install line
// finds it: a program that answers `version` as lab does, and its signature.
func labRelease(t *testing.T, k *sshsigtest.Key, tag string) string {
	t.Helper()
	arch := map[string]string{"arm64": "arm64", "amd64": "amd64"}[runtime.GOARCH]
	body := []byte("#!/bin/sh\necho 'lab " + tag + "'\necho 'Not logged in to Omarchy Mac Lab. Make a Lab token on your own page.'\n")
	dir := t.TempDir()
	name := "lab-" + runtime.GOOS + "-" + arch
	os.WriteFile(filepath.Join(dir, name), body, 0o644)
	os.WriteFile(filepath.Join(dir, name+".sig"), k.Sign(agent.ReleaseNamespace, body), 0o644)
	return dir
}

// /install/lab: the latest Lab release's lab for the machine, checked
// against the pinned release key, into ~/.local/bin/lab with no root;
// running it again updates; it stops at a lab that is not ours.
func TestInstallLabInstallsChecksUpdatesAndStopsAtALabThatIsNotOurs(t *testing.T) {
	for _, tool := range []string{"bash", "curl", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("no " + tool)
		}
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("lab is built for macOS on arm64 only")
	}
	k, other := sshsigtest.NewKey(t), sshsigtest.NewKey(t)
	l := newReportsLab(t, func(c *Config) { c.ReleaseKey = k.Public })
	w := l.do("GET", "/install/lab", "")
	if w.Code != 200 {
		t.Fatalf("/install/lab: %d", w.Code)
	}
	script := filepath.Join(t.TempDir(), "install-lab")
	os.WriteFile(script, w.Body.Bytes(), 0o644)
	home := t.TempDir()
	run := func(releases string) (string, error) {
		cmd := exec.Command("bash", script)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + t.TempDir(), "OMARCHY_LAB_RELEASES=file://" + releases,
			"MACLAB_URL=https://another.example", "MACLAB_TOKEN=lab_never"}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	bin := filepath.Join(home, ".local", "bin", "lab")
	version := func() string {
		out, _ := exec.Command(bin, "version").Output()
		return strings.SplitN(string(out), "\n", 2)[0]
	}

	// A release signed by another key: nothing installed.
	out, err := run(labRelease(t, other, "v0.2.0"))
	if _, serr := os.Stat(bin); err == nil || serr == nil || !strings.Contains(out, "signature") || !strings.Contains(out, "Nothing was installed") {
		t.Fatalf("a lab signed by another key: %v\n%s", err, out)
	}

	out, err = run(labRelease(t, k, "v0.2.0"))
	if err != nil || version() != "lab v0.2.0" || !strings.Contains(out, "Signature verified.") || !strings.Contains(out, "Installed lab v0.2.0") {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if st, _ := os.Stat(bin); st == nil || st.Mode().Perm()&0o111 == 0 {
		t.Error("the installed lab cannot be run")
	}
	if ents, _ := os.ReadDir(filepath.Dir(bin)); len(ents) != 1 {
		t.Errorf("the install left more than lab in ~/.local/bin: %v", ents)
	}

	// Running it again updates.
	out, err = run(labRelease(t, k, "v0.2.1"))
	if err != nil || version() != "lab v0.2.1" || !strings.Contains(out, "Updated lab v0.2.0 -> v0.2.1") {
		t.Fatalf("update: %v\n%s", err, out)
	}
	// A bad release over a good lab leaves the good one.
	if out, err = run(labRelease(t, other, "v0.2.2")); err == nil || version() != "lab v0.2.1" {
		t.Fatalf("a bad release over a good lab: %v\n%s", err, out)
	}

	// A lab that is not ours, a person's own build for another lab, is never
	// replaced.
	theirs := []byte("#!/bin/sh\necho 'lab: run kernels and tests on real Macs'\nexit 2\n")
	os.WriteFile(bin, theirs, 0o755)
	out, err = run(labRelease(t, k, "v0.2.3"))
	if now, _ := os.ReadFile(bin); err == nil || !bytes.Equal(now, theirs) || !strings.Contains(out, "is not Omarchy Mac Lab's") || !strings.Contains(out, "Nothing was installed") {
		t.Fatalf("a lab that is not ours: %v\n%s", err, out)
	}
	// One that answers `version` but is not ours, too.
	theirs = []byte("#!/bin/sh\necho 'lab 3.1'\n")
	os.WriteFile(bin, theirs, 0o755)
	if out, err = run(labRelease(t, k, "v0.2.3")); err == nil || !strings.Contains(out, "is not Omarchy Mac Lab's") {
		t.Fatalf("another lab that has a version: %v\n%s", err, out)
	}
	if now, _ := os.ReadFile(bin); !bytes.Equal(now, theirs) {
		t.Fatal("a lab that is not ours was replaced")
	}
}

// Decision S4, for both install lines: in Preview each serves the Release
// candidate the mini runs; off, the latest Lab release. Each pins the release
// key its labd was built with, is public for five minutes and the same for
// whoever asks.
func TestBothInstallLinesServeTheReleaseCandidateInPreviewAndTheLatestAfter(t *testing.T) {
	const latest = "https://github.com/omacom/omarchy-maclab/releases/latest/download"
	files := map[string]string{}
	for line, file := range map[string]string{"/install": "install.sh", "/install/lab": "install-lab.sh"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "installer", file))
		if err != nil {
			t.Fatal(err)
		}
		files[line] = string(b)
		if strings.Count(files[line], latest) != 1 || strings.Count(files[line], realReleaseKey) != 1 {
			t.Fatalf("installer/%s names the latest Lab release or the release key more or less than once", file)
		}
	}
	const other = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIThrowawayKeyForALocalReleaseCandidate0000000"
	for line, file := range files {
		served := func(config func(*Config)) string {
			t.Helper()
			w := newReportsLab(t, config).do("GET", line, "")
			if w.Code != 200 || w.Header().Get("Cache-Control") != "public, max-age=300" || w.Header().Get("Content-Type") != "text/x-shellscript; charset=utf-8" || len(w.Header().Values("Set-Cookie")) != 0 {
				t.Fatalf("%s: %d %v", line, w.Code, w.Header())
			}
			return w.Body.String()
		}
		for _, tag := range []string{"v0.2.0-rc.3", "v0.2.0"} {
			want := strings.Replace(file, latest, "https://github.com/omacom/omarchy-maclab/releases/download/"+tag, 1)
			if got := served(func(c *Config) { c.Preview, c.Release = true, tag }); got != want {
				t.Errorf("%s in Preview on %s does not serve that release and nothing else changed", line, tag)
			}
			if got := served(func(c *Config) { c.Release = tag }); got != file {
				t.Errorf("%s of an open lab on %s does not serve the latest Lab release", line, tag)
			}
		}
		if got := served(func(c *Config) { c.Preview, c.Release = true, "dev" }); got != file {
			t.Errorf("%s in Preview of a build that is no release is not the script as it is", line)
		}
		if got, want := served(func(c *Config) { c.ReleaseKey = other }), strings.Replace(file, realReleaseKey, other, 1); got != want {
			t.Errorf("%s of a lab built with another key does not pin that key in the one place", line)
		}
	}
	l := newReportsLab(t, nil)
	if w := l.do("GET", "/install/lab", "", "Cookie", "lab_session=anything", "Authorization", "Bearer lab_adm_secret"); w.Body.String() != files["/install/lab"] || w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Errorf("/install/lab with a credential: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := l.do("HEAD", "/install/lab", ""); w.Code != 200 || w.Body.Len() != 0 {
		t.Errorf("HEAD /install/lab: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := l.do("POST", "/install/lab", ""); w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Errorf("POST /install/lab: %d", w.Code)
	}
	if w := l.do("GET", "/install/lab", "", "Host", aliasHost); w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != labAddress+"/install/lab" {
		t.Errorf("the Alias: %d to %q", w.Code, w.Header().Get("Location"))
	}
	if !edgeKeeps("/install/lab") {
		t.Error("the edge is not told it may keep /install/lab")
	}
	// The Join command's script leads from the Alias to the Lab address
	// too, which curl -L follows.
	if w := l.do("GET", "/join", "", "Host", aliasHost); w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != labAddress+"/join" {
		t.Errorf("/join on the Alias: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

// lab agent-update counts on the agent's start number to know when the new
// one is back. A labd that has heard none since it started cannot tell an
// old run from a new one, and waits for a check-in; an agent that sends none
// (every lab-agent before the start number) would restart after the hold
// ended, and is Joined again instead.
func TestAgentUpdateWaitsForAStartNumberHeardSinceLabdStarted(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.11.0")
	sl := newSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, "0.11.0" })
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	why := func(v *dev) string {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.notUpdatableLocked(true)
	}

	// labd started again: the Mac's lab-agent has not checked in with it.
	fresh, err := New(sl.ctx, sl.s.cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	fv := fresh.dev(sl.name)
	fv.mu.Lock()
	fv.d.LastSeen = fresh.now() // as alive as it gets
	fv.mu.Unlock()
	if got := why(fv); !strings.Contains(got, "not checked in since labd started") {
		t.Errorf("agent-update before the first check-in since labd started: %q", got)
	}
	d := fv.snapshot()
	fv.heartbeat(api.Checkin{BootID: d.BootID, Kernel: d.Kernel, AgentStart: "a-new-run"})
	if got := why(fv); got != "" {
		t.Errorf("agent-update once the Mac checked in with its start number: %q", got)
	}

	// An agent that sends no start number.
	sl.stopAgent()
	v := sl.s.dev(sl.name)
	d = v.snapshot()
	v.heartbeat(api.Checkin{BootID: d.BootID, Kernel: d.Kernel})
	if got := why(v); !strings.Contains(got, "sends no start number") || !strings.Contains(got, "Join the Mac again") {
		t.Errorf("agent-update of an agent that sends no start number: %q", got)
	}
}

// labd's answer to the update's result can come late (the agent waits up to
// two minutes for it). The new lab-agent starts only within the time labd
// gave with the update, counted from when the agent sent its result: one
// started later could cut what labd has given the Mac since it let it go.
func TestAnUpdateWhoseAnswerComesPastTheHoldRestartsNothing(t *testing.T) {
	k := sshsigtest.NewKey(t)
	handed := signedAgent(t, k, "0.11.0")
	slow := make(chan struct{})
	var slowOnce sync.Once
	wrap := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-slow:
				if r.Method == "POST" && r.URL.Path == "/api/agent/report" {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, r)
					time.Sleep(4 * time.Second)
					for key, vals := range rec.Header() {
						w.Header()[key] = vals
					}
					w.WriteHeader(rec.Code)
					w.Write(rec.Body.Bytes())
					return
				}
			default:
			}
			h.ServeHTTP(w, r)
		})
	}
	sl := newWrappedSoftwareLab(t, k, false, agentUpdateMinAgent, func(c *Config) { c.AgentBinary, c.LabAgent = handed, "0.11.0" }, wrap)
	sl.baseline()
	sl.waitAgent(agentUpdateMinAgent)
	v := sl.s.dev(sl.name)
	v.mu.Lock()
	v.updateHold = 3 * time.Second
	v.mu.Unlock()
	slowOnce.Do(func() { close(slow) })
	resp, body := sl.post("/api/devices/"+sl.name+"/agent-update", nil)
	var res api.AgentUpdate
	if resp.StatusCode != 200 || json.Unmarshal([]byte(body), &res) != nil || !res.Changed {
		t.Fatalf("agent-update: %d %s", resp.StatusCode, body)
	}
	for end := time.Now().Add(15 * time.Second); !strings.Contains(sl.logs.String(), "starts when lab-agent next restarts"); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(end) || sl.restarts != 0 {
			t.Fatalf("an update whose answer came past labd's hold: restarts %d\n%s", sl.restarts, sl.logs.String())
		}
	}
	time.Sleep(500 * time.Millisecond)
	if sl.restarts != 0 {
		t.Fatalf("an update whose answer came past labd's hold restarted lab-agent %d times", sl.restarts)
	}
}

// The idle clean-up rewrites boot entries as root, with no deadline of its
// own on the Mac: when labd's wait for it ends first, lab agent-update stays
// off until the agent says the clean-up has ended.
func TestAnIdleCleanUpPastLabdsWaitKeepsAgentUpdateOffUntilItEnds(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	l.stopAgent()
	v := l.s.dev(l.name)
	d := v.snapshot()
	var running []string
	cleanup := ""
	checkIn := func() {
		v.heartbeat(api.Checkin{BootID: d.BootID, Kernel: d.Kernel, AgentStart: "run-1", Running: running})
		for _, c := range v.deliver(d.BootID).Commands {
			if c.Kind == api.CmdCleanup && c.JobID == "" {
				cleanup, running = c.ID, []string{c.ID}
			}
		}
	}
	cleaning := func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return strings.Contains(v.notUpdatableLocked(true), "removing leftover lab kernels")
	}
	checkIn()
	v.mu.Lock()
	v.cleanupWait, v.dirty = time.Second, true
	v.mu.Unlock()
	for end := time.Now().Add(25 * time.Second); cleanup == ""; time.Sleep(100 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the idle clean-up never reached the Mac")
		}
		checkIn()
	}
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		checkIn()
		v.mu.Lock()
		_, gaveUp := v.gaveUp[cleanup]
		v.mu.Unlock()
		if gaveUp {
			break
		}
		if time.Now().After(end) {
			t.Fatal("labd never stopped waiting for the clean-up")
		}
	}
	checkIn()
	time.Sleep(200 * time.Millisecond)
	checkIn()
	if !cleaning() {
		t.Fatal("lab agent-update is let on while the clean-up labd stopped waiting for still runs")
	}
	running = nil
	checkIn()
	if cleaning() {
		t.Fatal("lab agent-update is still kept off after the agent said the clean-up ended")
	}
}
