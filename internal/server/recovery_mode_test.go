package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// modeCount counts what labd had the Out-of-band controller do to the
// lab's Mac: the resets the reset tool sent.
type modeCount struct{ m *fakeMini }

func (c modeCount) counts() int {
	n, _ := c.m.resetsSent()
	return n
}

// countedModeLab is a lab whose Mac is sighted on the mini's right port.
func countedModeLab(t *testing.T, name string) (*lab, modeCount) {
	l := newLabNamed(t, true, name)
	l.baseline()
	return l, modeCount{l.oob}
}
func TestRecoveryNetworkJobNeverUsesStaleConfiguredController(t *testing.T) {
	l, c := countedModeLab(t, "m1-stale-oob")
	before := c.counts()
	j := l.run(api.JobSpec{RecoveryMode: api.RecoveryNetworkOnly, Kernel: l.artifact("krel=mode-network behavior=ok")})
	l.expect(j, api.OutcomePass)
	v := l.s.dev(l.name)
	v.onSerial("Kernel panic - not syncing: FOREIGN_M3_SERIAL")
	time.Sleep(20 * time.Millisecond)
	if got := c.counts(); got != before {
		t.Fatalf("network-only used configured controller: before%v after%v", before, got)
	}
	if j.Spec.RecoveryMode != api.RecoveryNetworkOnly || j.EffectiveRecoveryMode != api.RecoveryNetworkOnly {
		t.Fatal("requested/effective mode lost", j)
	}
	d := v.snapshot()
	if _, sighted := l.s.cables.of(l.name); !sighted || d.EffectiveRecoveryMode != api.RecoveryNetworkOnly || d.RecoveryModeJob != j.ID || l.s.view(v).Tier > 1 {
		t.Fatal("Sighting dropped or effective isolation lost", d)
	}
	if strings.Contains(modeMessages(j), "ttySAC0") {
		t.Fatal("network boot injected serial cmdline", modeMessages(j))
	}
	if _, e := l.s.store.job(j.ID); e != nil {
		t.Fatal(e)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/devices/x/oob", nil)
	req.SetPathValue("name", l.name)
	l.s.hOOBStatus(rec, byMaintainer(req))
	var status api.OOBStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil || status.Cable == nil || !strings.Contains(status.Cable.NoHardReset, "network-only") {
		t.Fatal("the Mac is not shown as having no Hard reset", rec.Code, rec.Body.String())
	}
	if r, body := l.get("POST", "/api/devices/"+l.name+"/reset"); r.StatusCode != http.StatusConflict || !strings.Contains(body, "network-only") {
		t.Fatal("a Hard reset under the network-only policy", r.Status, body)
	}
	if got := c.counts(); got != before {
		t.Fatal("masked status called controller", got)
	}
	// An old/empty request inherits the network hold and cannot re-arm stale OOB.
	legacy := l.run(api.JobSpec{Kernel: l.artifact("krel=mode-legacy-inherits behavior=ok")})
	l.expect(legacy, api.OutcomePass)
	if legacy.Spec.RecoveryMode != "" || legacy.EffectiveRecoveryMode != api.RecoveryNetworkOnly || c.counts() != before {
		t.Fatal("empty request silently reenabled stale OOB", legacy.EffectiveRecoveryMode, c.counts())
	}
}
func TestRecoveryNetworkSilentLossIsUnknownAndNeverResets(t *testing.T) {
	l, c := countedModeLab(t, "m1-network-loss")
	before := c.counts()
	j := l.run(api.JobSpec{RecoveryMode: api.RecoveryNetworkOnly, Kernel: l.artifact("krel=mode-silent behavior=silent"), BootTimeoutSec: 2})
	l.expect(j, api.OutcomeUnknown)
	if l.s.dev(l.name).snapshot().State != api.StateNeedsHands {
		t.Fatal("unobserved loss not retained as needs_hands")
	}
	if c.counts() != before {
		t.Fatal("offline network mode touched stale controller", before, c.counts())
	}
	if j.Outcome == api.OutcomePass {
		t.Fatal("unknown loss became PASS")
	}
}
func TestRecoveryExplicitSerialJobIsRefusedAndAsksNoController(t *testing.T) {
	l, c := countedModeLab(t, "m3pro")
	before := c.counts()
	_, code, e := l.s.Submit(api.JobSpec{Device: l.name, RecoveryMode: api.RecoverySerialAssisted, Kernel: l.artifact("krel=mode-serial behavior=ok")})
	if e == nil || code != http.StatusGone || e.Error() != api.NoSerialConsole {
		t.Fatal("serial-assisted job admitted", code, e)
	}
	if jobs, _ := l.s.store.jobs(l.name, 100, true); len(jobs) != 0 || c.counts() != before {
		t.Fatal("refused serial-assisted job queued or touched the controller", len(jobs), c.counts())
	}
}
func TestRecoveryRejectsUnknownModeBeforeQueue(t *testing.T) {
	l := newLab(t, false)
	if _, code, e := l.s.Submit(api.JobSpec{Device: l.name, RecoveryMode: "typo"}); e == nil || code != 400 {
		t.Fatal(code, e)
	}
}
func TestRecoveryNetworkMarkerFiltersStaleSerialEvidence(t *testing.T) {
	l, c := countedModeLab(t, "m1-event-filter")
	v := l.s.dev(l.name)
	j := &api.Job{ID: "j-mode-filter", Spec: api.JobSpec{Device: l.name, RecoveryMode: api.RecoveryNetworkOnly}, State: api.JobQueued}
	if e := l.s.beginJobRecoveryMode(context.Background(), v, j); e != nil {
		t.Fatal(e)
	}
	before := c.counts()
	now := time.Now()
	v.mu.Lock()
	v.events = append(v.events, api.KernelEvent{Time: now, Kind: "panic", Source: "serial", Line: "foreign M3"}, api.KernelEvent{Time: now, Kind: "warning", Source: "kmsg", Line: "actual own agent"})
	v.mu.Unlock()
	events := v.eventsSince(now.Add(-time.Second))
	for _, e := range events {
		if e.Source == "serial" {
			t.Fatal("foreign serial event used as evidence")
		}
	}
	if c.counts() != before {
		t.Fatal("evidence read touched OOB")
	}
}

func modeMessages(j *api.Job) string {
	var out []string
	for _, e := range j.Events {
		out = append(out, e.Msg)
	}
	return strings.Join(out, "\n")
}

// A Hard reset that waits its turn at the controller when the network-only
// policy is selected is canceled and never sent, and the policy waits until
// it has stopped before any command goes to the Mac.
func TestRecoveryNetworkTransitionCancelsAndDrainsPriorControllerCall(t *testing.T) {
	l, c := countedModeLab(t, "mode-quiesce")
	v := l.s.dev(l.name)
	// Another port's tool run holds the controller's one lock.
	probing, release := l.oob.holdProbes(t)
	probed := make(chan error, 1)
	go func() { _, err := l.oob.client.Probe(context.Background(), portLeft); probed <- err }()
	<-probing
	resetDone := make(chan error, 1)
	go func() { _, err := v.resetOOB(context.Background()); resetDone <- err }()
	until := time.Now().Add(5 * time.Second)
	for {
		v.mu.Lock()
		waiting := v.oobCallCancel != nil
		v.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(until) {
			t.Fatal("the Hard reset never reached the controller")
		}
		time.Sleep(time.Millisecond)
	}
	l.oob.waitResetCalls(t, "the Hard reset never reached the controller's lock", func(asked, _ int) bool { return asked == 1 })
	j := &api.Job{ID: "mode-quiesce-job", Spec: api.JobSpec{Device: l.name, RecoveryMode: api.RecoveryNetworkOnly}, State: api.JobQueued}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e := l.s.beginJobRecoveryMode(ctx, v, j); e != nil {
		t.Fatal(e)
	}
	// A controller call holds oobCallMu from start to end; resetOOB lets go
	// of it only as it returns, so its goroutine may not have said so yet.
	if !v.oobCallMu.TryLock() {
		t.Fatal("device commands could start before the controller call stopped")
	}
	v.oobCallMu.Unlock()
	if e := <-resetDone; e == nil {
		t.Fatal("prior reset not canceled")
	}
	// The controller learns its caller left when it sees the connection
	// close, a moment after labd let go: the reset ends without its turn.
	l.oob.waitResetCalls(t, "the canceled Hard reset still waits at the controller", func(_, ended int) bool { return ended == 1 })
	release()
	if e := <-probed; e != nil {
		t.Fatal(e)
	}
	time.Sleep(100 * time.Millisecond)
	if c.counts() != 0 {
		t.Fatal("a Hard reset canceled while it waited was sent")
	}
	if _, e := v.resetOOB(ctx); e == nil || c.counts() != 0 {
		t.Fatal("masked reset admitted")
	}
}
func TestRecoveryNetworkMaskPersistsAcrossRestartAndNoSerialJobLiftsIt(t *testing.T) {
	l, c := countedModeLab(t, "mode-restart")
	v := l.s.dev(l.name)
	j := &api.Job{ID: "mode-restart-job", Spec: api.JobSpec{Device: l.name, RecoveryMode: api.RecoveryNetworkOnly}, State: api.JobQueued}
	if e := l.s.beginJobRecoveryMode(context.Background(), v, j); e != nil {
		t.Fatal(e)
	}
	before := c.counts()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, e := New(ctx, l.s.cfg, quietLog())
	if e != nil {
		t.Fatal(e)
	}
	fresh := s.dev(l.name)
	if fresh.snapshot().EffectiveRecoveryMode != api.RecoveryNetworkOnly {
		t.Fatal("restart lost network hold")
	}
	s.sight(ctx)
	if _, e := fresh.resetOOB(ctx); e == nil {
		t.Fatal("masked reset admitted after the restart")
	}
	if c.counts() != before {
		t.Fatal("restart touched isolated controller", before, c.counts())
	}
	serial := &api.Job{ID: "explicit-reenable", Spec: api.JobSpec{Device: l.name, RecoveryMode: api.RecoverySerialAssisted}, State: api.JobQueued}
	if e := s.beginJobRecoveryMode(ctx, fresh, serial); e == nil || e.Error() != api.NoSerialConsole {
		t.Fatal("stored serial-assisted job admitted", e)
	}
	if fresh.snapshot().EffectiveRecoveryMode != api.RecoveryNetworkOnly || c.counts() != before {
		t.Fatal("serial-assisted job lifted the network hold or touched the controller", fresh.snapshot().EffectiveRecoveryMode, c.counts())
	}
}

func TestRecoveryTransactionIsolatesExecBeforeAnyJob(t *testing.T) {
	l, c := countedModeLab(t, "pre-job-network")
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "own", Maintainer: "ada", Expires: time.Now().Add(time.Hour)}
	v.mu.Unlock()
	before := c.counts()
	q := `{"mode":"network-only","holder":"own","transaction":"fresh-preparation"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(q))
	req.SetPathValue("name", l.name)
	l.s.hRecoveryMode(rec, byMaintainer(req))
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if c.counts() != before {
		t.Fatal("network selection used stale configured route", c.counts())
	}
	d := v.snapshot()
	if d.RecoveryTransaction != "fresh-preparation" || d.RecoveryHolder != "own" || d.EffectiveRecoveryMode != api.RecoveryNetworkOnly {
		t.Fatal(d)
	}
	// Before a job exists, an offline /exec must not fall back to foreign serial.
	v.mu.Lock()
	v.d.LastSeen = time.Time{}
	v.mu.Unlock()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"command":"DO_NOT_EXECUTE"}`))
	req.SetPathValue("name", l.name)
	l.s.hExec(rec, byMaintainer(req))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "is offline") || c.counts() != before {
		t.Fatal("pre-job serial fallback escaped transaction", rec.Code, rec.Body.String(), c.counts())
	}
	if _, code, e := l.s.Submit(api.JobSpec{Device: l.name, Holder: "own", RecoveryMode: api.RecoverySerialAssisted, RecoveryTransaction: "fresh-preparation"}); e == nil || code != http.StatusGone {
		t.Fatal("serial-assisted job admitted inside a transaction", code, e)
	}
	for _, spec := range []api.JobSpec{{Device: l.name, Holder: "own", RecoveryMode: api.RecoveryNetworkOnly, RecoveryTransaction: "foreign"}, {Device: l.name, Holder: "own", RecoveryMode: api.RecoveryNetworkOnly}} {
		if _, code, e := l.s.Submit(spec); e == nil || code != 409 {
			t.Fatal("mismatched transaction admitted", code, e)
		}
	}
	if e := validateRecoveryTransaction(d, api.JobSpec{Holder: "own", RecoveryMode: api.RecoveryNetworkOnly, RecoveryTransaction: "fresh-preparation"}); e != nil {
		t.Fatal(e)
	}
	if e := validateRecoveryTransaction(d, api.JobSpec{}); e != nil {
		t.Fatal("legacy inheritance should retain isolation", e)
	}
}
func TestRecoveryTransactionRefusesForeignLeaseBeforeControllerUse(t *testing.T) {
	l, c := countedModeLab(t, "transaction-lease")
	before := c.counts()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "actual-owner", Expires: time.Now().Add(time.Hour)}
	v.mu.Unlock()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"mode":"network-only","holder":"foreign","transaction":"wrong"}`))
	req.SetPathValue("name", l.name)
	l.s.hRecoveryMode(rec, byMaintainer(req))
	if rec.Code != 409 || c.counts() != before {
		t.Fatal("foreign lease touched controller", rec.Code, c.counts())
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"mode":"serial-assisted","holder":"actual-owner","transaction":"own"}`))
	req.SetPathValue("name", l.name)
	l.s.hRecoveryMode(rec, byMaintainer(req))
	if rec.Code != http.StatusGone || rec.Body.String() != api.NoSerialConsole+"\n" || c.counts() != before {
		t.Fatal("serial-assisted mode selected or touched controller", rec.Code, rec.Body.String(), c.counts())
	}
}

// The recovery policy outlasts the job, the Lease and a restart, so it is a
// Maintainer's: an M team member's job under their own Lease sets none.
func TestAnMTeamMembersJobSetsNoRecoveryPolicy(t *testing.T) {
	l, c := countedModeLab(t, "mode-member")
	grace := l.member("grace")
	l.leaseAs(grace, 0)
	before := c.counts()
	code, _, body := l.submitAs(grace.Token, api.JobSpec{RecoveryMode: api.RecoveryNetworkOnly, Kernel: l.artifact("krel=mode-member behavior=ok")})
	if code != http.StatusForbidden || !strings.Contains(body, "recovery_mode") || !strings.Contains(body, "Maintainer") {
		t.Fatalf("an M team member's job with recovery_mode network-only: %d %s", code, body)
	}
	if jobs, _ := l.s.store.jobs(l.name, 100, true); len(jobs) != 0 {
		t.Fatalf("the refused job was queued: %+v", jobs[0])
	}
	id := l.mustSubmitAs(grace, api.JobSpec{Kernel: l.artifact("krel=mode-member behavior=ok")})
	l.expect(l.jobAPI(id, 60*time.Second), api.OutcomePass)
	if d := l.s.dev(l.name).snapshot(); d.EffectiveRecoveryMode != "" || d.RecoveryTransaction != "" {
		t.Fatalf("the Mac's recovery policy after an M team member's jobs: %q, transaction %q", d.EffectiveRecoveryMode, d.RecoveryTransaction)
	}
	if got := l.s.dev(l.name).noHardReset(); got != "" || c.counts() != before {
		t.Fatalf("the Mac takes no Hard reset any more (%q), or one was sent (%d, was %d)", got, c.counts(), before)
	}
}

// What a Maintainer's job or a Maintainer set, a Maintainer lifts: under
// their own Lease on the idle Mac, which then takes a Hard reset again.
func TestAMaintainerLiftsTheNetworkOnlyPolicy(t *testing.T) {
	l, c := countedModeLab(t, "mode-lift")
	grace := l.member("grace")
	l.expect(l.run(api.JobSpec{RecoveryMode: api.RecoveryNetworkOnly, Kernel: l.artifact("krel=mode-lift behavior=ok")}), api.OutcomePass)
	v := l.s.dev(l.name)
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })
	if d := v.snapshot(); d.EffectiveRecoveryMode != api.RecoveryNetworkOnly || !strings.Contains(v.noHardReset(), "network-only") {
		t.Fatalf("after a Maintainer's network-only job the policy is %q and a Hard reset %q", d.EffectiveRecoveryMode, v.noHardReset())
	}
	lift := func(token, holder string) (*http.Response, string) {
		return l.as(token, "POST", "/api/devices/"+l.name+"/recovery-mode", api.RecoveryModeRequest{Mode: api.RecoveryHardReset, Holder: holder, Transaction: "lift-1"})
	}
	if r, body := lift(harnessToken, harnessMaintainer+"@laptop"); r.StatusCode != http.StatusConflict {
		t.Fatalf("a Maintainer with no Lease lifting the policy: %s %s", r.Status, body)
	}
	l.leaseAs(grace, 0)
	if r, body := lift(grace.Token, "grace@laptop"); r.StatusCode != http.StatusForbidden {
		t.Fatalf("an M team member lifting the policy under their Lease: %s %s", r.Status, body)
	}
	if r, body := l.as(grace.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != 200 {
		t.Fatalf("grace releasing: %s %s", r.Status, body)
	}
	if d := v.snapshot(); d.EffectiveRecoveryMode != api.RecoveryNetworkOnly {
		t.Fatalf("a refused request changed the policy to %q", d.EffectiveRecoveryMode)
	}

	l.leaseAs(harnessAda, 0)
	r, body := lift(harnessToken, harnessMaintainer+"@laptop")
	var got api.RecoveryModeSelection
	if json.Unmarshal([]byte(body), &got); r.StatusCode != 200 || got.Mode != api.RecoveryHardReset || got.Device != l.name {
		t.Fatalf("a Maintainer lifting the policy under their own Lease: %s %s", r.Status, body)
	}
	if d := v.snapshot(); d.EffectiveRecoveryMode != "" || d.RecoveryModeJob != "" || d.RecoveryTransaction != "" || d.RecoveryHolder != "" || v.noHardReset() != "" {
		t.Fatalf("the lifted policy: %q, job %q, transaction %q, holder %q; a Hard reset: %q", d.EffectiveRecoveryMode, d.RecoveryModeJob, d.RecoveryTransaction, d.RecoveryHolder, v.noHardReset())
	}
	// It stays lifted across a restart, and a job that names no mode inherits none.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	again, err := New(ctx, l.s.cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if d := again.dev(l.name).snapshot(); d.EffectiveRecoveryMode != "" {
		t.Fatalf("after a restart the lifted policy is %q", d.EffectiveRecoveryMode)
	}
	cancel()
	before := c.counts()
	if r, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/reset", nil); r.StatusCode != 200 || c.counts() != before+1 {
		t.Fatalf("a Hard reset once the policy is lifted: %s %s (%d sent, was %d)", r.Status, body, c.counts(), before)
	}
	if _, code, err := l.s.Submit(api.JobSpec{Device: l.name, RecoveryMode: api.RecoveryHardReset, Kernel: l.artifact("krel=mode-lift behavior=ok")}); err == nil || code != 400 {
		t.Fatalf("a job that carries the lifting mode: %d %v", code, err)
	}
}

// The recovery policy's route asks for the Maintainer's own Lease by
// account, as every fence does: a Maintainer renamed inside their Lease
// still holds it, and another who has their old handle does not.
func TestTheRecoveryPolicyAsksForTheLeaseByAccount(t *testing.T) {
	l := leaseLab(t)
	v := l.lrMac("m1-pro", "j316s", nil)
	l.lrMaintainer().mustLease("m1-pro", "ravi@laptop", 0)
	was := v.snapshot().Lease.Owner()

	l.gh.AddUser(maintainerID, was+"-renamed")
	renamed := l.signedIn(maintainerID)
	l.gh.AddUser(lrKen, was)
	other := l.signedIn(lrKen)
	if err := l.s.setMaintainer(lrKen, true); err != nil {
		t.Fatal(err)
	}
	ask := func(b *browser, holder string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(api.RecoveryModeRequest{Mode: api.RecoveryNetworkOnly, Holder: holder, Transaction: "by-account"})
		return b.write("POST", "/api/devices/m1-pro/recovery-mode", string(body), "Content-Type", "application/json")
	}
	if w := ask(other, "ravi@laptop"); w.Code != http.StatusConflict || v.snapshot().EffectiveRecoveryMode != "" {
		t.Fatalf("another Maintainer, who has the holder's old handle, set the policy under their Lease: %d %s", w.Code, w.Body)
	}
	if w := ask(renamed, "someone-else@laptop"); w.Code != http.StatusConflict || v.snapshot().EffectiveRecoveryMode != "" {
		t.Fatalf("the holder set the policy under another holder label: %d %s", w.Code, w.Body)
	}
	if w := ask(renamed, "ravi@laptop"); w.Code != 200 || v.snapshot().EffectiveRecoveryMode != api.RecoveryNetworkOnly {
		t.Fatalf("the holder, renamed inside their Lease, set the policy: %d %s", w.Code, w.Body)
	}
}

// byMaintainer is req as a Maintainer's, for a handler called past the
// routes' own wrappers.
func byMaintainer(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), maintainerKey{}, "ada"))
}
