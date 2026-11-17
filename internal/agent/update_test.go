package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/sshsig/sshsigtest"
)

// agentFile is a lab-agent as the lab hands it out, for a test: a program
// that answers `version` as lab-agent does.
func agentFile(number, release string) []byte {
	return []byte("#!/bin/sh\necho " + number + "\necho 'Lab release " + release + "'\n")
}

// updateLab is a lab handing out body and sig to the Mac whose device secret
// is "mac-secret", and what it was asked.
type updateLab struct {
	*httptest.Server
	mu    sync.Mutex
	asked []string
}

func newUpdateLab(t *testing.T, body, sig []byte) *updateLab {
	t.Helper()
	l := &updateLab{}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.asked = append(l.asked, r.Method+" "+r.URL.Path)
		l.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer mac-secret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/api/agent/lab-agent":
			w.Write(body)
		case r.URL.Path == "/api/agent/lab-agent.sig" && sig != nil:
			w.Write(sig)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(l.Close)
	return l
}

func (l *updateLab) requests() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.asked...)
}

// updatingMac is a Mac with lab-agent 0.9.0 installed, trusting key, and
// the update command typed at it; restarts counts the restarts of the agent.
type updatingMac struct {
	cfg       *Config
	sim       *Sim
	installed string
	was       []byte
	restarts  int
}

func newUpdatingMac(t *testing.T, server, key string) *updatingMac {
	t.Helper()
	m := &updatingMac{sim: NewSim(t.TempDir()), installed: filepath.Join(t.TempDir(), "lab-agent"), was: agentFile("0.9.0", "v0.2.0-rc.1")}
	if err := os.WriteFile(m.installed, m.was, 0o755); err != nil {
		t.Fatal(err)
	}
	m.cfg = &Config{Server: server, Name: "shelf-mac", Secret: "mac-secret", WorkDir: t.TempDir(),
		Self: &Self{Path: m.installed, Version: "0.9.0", Key: key, Restart: func() error { m.restarts++; return nil }}}
	return m
}

func (m *updatingMac) update() (string, error) {
	var out bytes.Buffer
	err := Volunteer(context.Background(), m.cfg, m.sim, []string{"update"}, &out)
	return out.String(), err
}

// unchanged fails the test unless the installed agent is the one from
// before, nothing restarted, and the update left no file beside it.
func (m *updatingMac) unchanged(t *testing.T, what string) {
	t.Helper()
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, m.was) {
		t.Errorf("%s: the installed lab-agent changed", what)
	}
	if m.restarts != 0 {
		t.Errorf("%s: lab-agent was restarted", what)
	}
	if ents, _ := os.ReadDir(filepath.Dir(m.installed)); len(ents) != 1 {
		t.Errorf("%s: the update left files beside the installed lab-agent: %v", what, ents)
	}
}

func TestUpdateTakesTheAgentFromTheLabAndChecksItAgainstTheKeyTheRunningAgentPins(t *testing.T) {
	k := sshsigtest.NewKey(t)
	next := agentFile("0.10.0", "v0.2.0-rc.2")
	l := newUpdateLab(t, next, k.Sign(ReleaseNamespace, next))
	m := newUpdatingMac(t, l.URL, k.Public)

	out, err := m.update()
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, next) {
		t.Error("the installed lab-agent is not the one the lab handed out")
	}
	if st, err := os.Stat(m.installed); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("the installed lab-agent's mode: %v %v", st.Mode(), err)
	}
	if m.restarts != 1 {
		t.Errorf("lab-agent was restarted %d times, want once", m.restarts)
	}
	for _, want := range []string{"0.9.0", "0.10.0", "v0.2.0-rc.2", "signature"} {
		if !strings.Contains(out, want) {
			t.Errorf("update said %q, which lacks %q", out, want)
		}
	}
	if got := l.requests(); len(got) != 2 || got[0] != "GET /api/agent/lab-agent" || got[1] != "GET /api/agent/lab-agent.sig" {
		t.Errorf("update asked the lab for %v, want the agent and its signature", got)
	}
	if ents, _ := os.ReadDir(filepath.Dir(m.installed)); len(ents) != 1 {
		t.Errorf("the update left files beside the installed lab-agent: %v", ents)
	}
}

func TestUpdateRefusesAnAgentWithABadSignature(t *testing.T) {
	k, other := sshsigtest.NewKey(t), sshsigtest.NewKey(t)
	next := agentFile("0.10.0", "v0.2.0-rc.2")
	for name, sig := range map[string][]byte{
		"signed by another key":        other.Sign(ReleaseNamespace, next),
		"the signature of other bytes": k.Sign(ReleaseNamespace, agentFile("0.10.1", "v0.2.0-rc.2")),
		"signed for another purpose":   k.Sign("file", next),
		"not a signature":              []byte("ok\n"),
		"no signature":                 nil,
	} {
		l := newUpdateLab(t, next, sig)
		m := newUpdatingMac(t, l.URL, k.Public)
		out, err := m.update()
		if err == nil {
			t.Errorf("%s: update went through: %s", name, out)
			continue
		}
		if !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: %v, which does not say that nothing was changed", name, err)
		}
		m.unchanged(t, name)
	}
}

func TestUpdateRefusesAnAgentOlderThanTheOneInstalled(t *testing.T) {
	k := sshsigtest.NewKey(t)
	for _, number := range []string{"0.8.9", "0.8", "what"} {
		older := agentFile(number, "v0.1.9")
		l := newUpdateLab(t, older, k.Sign(ReleaseNamespace, older))
		m := newUpdatingMac(t, l.URL, k.Public)
		_, err := m.update()
		if err == nil || !strings.Contains(err.Error(), "0.9.0") || !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("a signed lab-agent %s over 0.9.0: %v", number, err)
		}
		m.unchanged(t, number)
	}
}

func TestUpdateWithTheLabsAgentAlreadyInstalledChangesNothing(t *testing.T) {
	k := sshsigtest.NewKey(t)
	same := agentFile("0.9.0", "v0.2.0-rc.1")
	l := newUpdateLab(t, same, k.Sign(ReleaseNamespace, same))
	m := newUpdatingMac(t, l.URL, k.Public)
	out, err := m.update()
	if err != nil || !strings.Contains(out, "already") {
		t.Fatalf("update: %q, %v", out, err)
	}
	m.unchanged(t, "the same agent")
}

func TestUpdateIsRefusedByAnAgentThatPinsNoKeyAndOnALabBoot(t *testing.T) {
	k := sshsigtest.NewKey(t)
	next := agentFile("0.10.0", "v0.2.0-rc.2")
	l := newUpdateLab(t, next, k.Sign(ReleaseNamespace, next))

	was := ReleaseKey
	ReleaseKey = ""
	t.Cleanup(func() { ReleaseKey = was })
	m := newUpdatingMac(t, l.URL, "")
	if _, err := m.update(); err == nil || !strings.Contains(err.Error(), "release key") {
		t.Errorf("an agent that pins no key: %v", err)
	}
	m.unchanged(t, "no pinned key")
	if got := l.requests(); len(got) != 0 {
		t.Errorf("an agent that pins no key asked the lab for %v", got)
	}

	m = newUpdatingMac(t, l.URL, k.Public)
	m.sim.mu.Lock()
	m.sim.cmdline += " " + labBootMarker
	m.sim.mu.Unlock()
	if _, err := m.update(); err == nil || !strings.Contains(err.Error(), "Lab boot") {
		t.Errorf("on a Lab boot: %v", err)
	}
	m.unchanged(t, "a Lab boot")
}

// The lab's own order to update (lab agent-update) is a Lab-owned Mac's
// alone: a Volunteer's Mac refuses it whatever labd thought, downloads
// nothing and writes the refusal in its Audit log.
func TestAnAgentIsNeverPushedToAVolunteersMac(t *testing.T) {
	k := sshsigtest.NewKey(t)
	next := agentFile("0.10.0", "v0.2.0-rc.2")
	l := newUpdateLab(t, next, k.Sign(ReleaseNamespace, next))
	m := newUpdatingMac(t, l.URL, k.Public)
	m.cfg.Enrollment.Kind = api.KindVolunteer
	a := New(m.cfg, m.sim, log.New(io.Discard, "", 0))

	_, after, err := a.exec(context.Background(), api.Command{ID: "c1", Kind: api.CmdAgentUpdate, Maintainer: "ravi"})
	if err == nil || after != nil || !strings.Contains(err.Error(), "never pushed to a Volunteer's Mac") || !strings.Contains(err.Error(), "sudo lab-agent update") {
		t.Fatalf("a pushed update on a Volunteer's Mac: %v", err)
	}
	m.unchanged(t, "a pushed update")
	if got := l.requests(); len(got) != 0 {
		t.Errorf("the Volunteer's Mac asked the lab for %v", got)
	}
	es, err := readAudit(m.cfg.workDir(), 0, 0)
	if err != nil || len(es) != 1 || es[0].Maintainer != "ravi" || es[0].Action != "update lab-agent" || !strings.Contains(es[0].Refused, "never pushed") {
		t.Errorf("the Audit log: %+v, %v", es, err)
	}
}

func TestALabOwnedMacUpdatesWhenTheLabSaysSoAndRestartsOnceTheLabHeard(t *testing.T) {
	k := sshsigtest.NewKey(t)
	next := agentFile("0.10.0", "v0.2.0-rc.2")
	l := newUpdateLab(t, next, k.Sign(ReleaseNamespace, next))
	m := newUpdatingMac(t, l.URL, k.Public)
	m.cfg.Enrollment.Kind = api.KindLabOwned
	a := New(m.cfg, m.sim, log.New(io.Discard, "", 0))

	out, after, err := a.exec(context.Background(), api.Command{ID: "c1", Kind: api.CmdAgentUpdate})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var res api.AgentUpdate
	if json.Unmarshal(b, &res) != nil || res != (api.AgentUpdate{From: "0.9.0", To: "0.10.0", LabRelease: "v0.2.0-rc.2", Changed: true}) {
		t.Errorf("the result: %s", b)
	}
	if got, _ := os.ReadFile(m.installed); !bytes.Equal(got, next) {
		t.Error("the installed lab-agent is not the one the lab handed out")
	}
	if m.restarts != 0 || after == nil {
		t.Fatalf("restarts before the lab heard: %d (after: %v)", m.restarts, after)
	}
	after.run()
	if m.restarts != 1 {
		t.Errorf("restarts once the lab heard: %d", m.restarts)
	}

	// With a job's command running, the Mac is not idle.
	m = newUpdatingMac(t, l.URL, k.Public)
	a = New(m.cfg, m.sim, log.New(io.Discard, "", 0))
	a.running["c0"] = runningCmd{c: api.Command{ID: "c0", Kind: api.CmdRunTest, JobID: "j1"}}
	if _, _, err := a.exec(context.Background(), api.Command{ID: "c2", Kind: api.CmdAgentUpdate}); err == nil || !strings.Contains(err.Error(), "j1") {
		t.Errorf("an update during a job: %v", err)
	}
	m.unchanged(t, "an update during a job")

	// Nor while the lab's clean-up runs, which labd may have stopped waiting for.
	m = newUpdatingMac(t, l.URL, k.Public)
	a = New(m.cfg, m.sim, log.New(io.Discard, "", 0))
	a.running["c3"] = runningCmd{c: api.Command{ID: "c3", Kind: api.CmdCleanup}}
	if _, _, err := a.exec(context.Background(), api.Command{ID: "c4", Kind: api.CmdAgentUpdate}); err == nil || !strings.Contains(err.Error(), "clean-up") {
		t.Errorf("an update during the idle clean-up: %v", err)
	}
	m.unchanged(t, "an update during the idle clean-up")
}

func TestEachCheckinCarriesTheNumberOfTheAgentInstalled(t *testing.T) {
	checkins := make(chan api.Checkin, 1)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var ci api.Checkin
		json.NewDecoder(r.Body).Decode(&ci)
		checkins <- ci
		io.WriteString(w, "{}")
	}))
	t.Cleanup(hs.Close)
	a := New(&Config{Server: hs.URL, Secret: "test", Self: &Self{Version: "0.8.0"}}, NewSim(t.TempDir()), log.New(io.Discard, "", 0))
	if err := a.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ci := <-checkins; ci.Facts == nil || ci.Facts.AgentVersion != "0.8.0" {
		t.Fatalf("the check-in's facts: %+v", ci.Facts)
	}
}

func TestUpdateIsACommandOfLabAgent(t *testing.T) {
	if !IsVolunteerCommand("update") {
		t.Fatal("lab-agent update is not routed")
	}
}
