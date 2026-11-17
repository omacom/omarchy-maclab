package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/window"
)

// opsOf asks labd as one of the mini's own scripts does.
func opsOf(s *Server, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.OpsHandler().ServeHTTP(w, httptest.NewRequest(method, "http://labd"+path, strings.NewReader("")))
	return w
}

// pollHere polls once as a builder and returns when labd has answered: no
// poll is left waiting in labd to take a build later.
func pollHere(b *builderLab, token string, wait time.Duration) int {
	ctx, cancel := context.WithTimeout(b.ctx, wait)
	defer cancel()
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/builder/poll", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("User-Agent", "lab-builder/test")
	w := httptest.NewRecorder()
	b.s.Handler().ServeHTTP(w, r)
	return w.Code
}

// A Deploy and a Maintenance run restart labd, and a job that runs then is
// lost. While jobs are drained, the one that runs finishes, the one behind
// it waits, and it starts once the drain is lifted.
func TestADrainLetsTheRunningJobFinishAndStartsNoOther(t *testing.T) {
	h := newCommandHold()
	l := newLabWith(t, labConfig{wrap: h.wrap})
	l.baseline()
	h.armed.Store(true)
	first := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-drain-a behavior=ok")})
	select {
	case <-h.held:
	case <-time.After(20 * time.Second):
		t.Fatal("the first job never reached the Mac")
	}
	second := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-drain-b behavior=ok")})

	if w := opsOf(l.s, "POST", "/drain"); w.Code != 200 || w.Body.String() != "1 0\n" {
		t.Fatalf("with one job running: %d %q", w.Code, w.Body)
	}
	close(h.release)
	l.waitJob(first, func(j *api.Job) bool { return j.State == api.JobDone })
	deadline := time.Now().Add(10 * time.Second)
	for opsOf(l.s, "POST", "/drain").Body.String() != "0 0\n" {
		if time.Now().After(deadline) {
			t.Fatalf("the drain never ended: %q", opsOf(l.s, "POST", "/drain").Body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	l.staysQueued(second, 1500*time.Millisecond, "jobs are drained")

	if w := opsOf(l.s, "DELETE", "/drain"); w.Code != 200 || w.Body.String() != "lifted\n" {
		t.Fatalf("lifting: %d %q", w.Code, w.Body)
	}
	if j := l.waitJob(second, func(j *api.Job) bool { return j.State == api.JobDone }); j.Outcome != api.OutcomePass {
		t.Errorf("the job that waited ended %s: %s", j.Outcome, j.Summary)
	}
}

// jobSaid waits until job id has an event containing what, and fails the
// test when none comes.
func jobSaid(t *testing.T, s *Server, id, what string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		j, err := s.store.job(id)
		if err == nil {
			for _, e := range j.Events {
				if strings.Contains(e.Msg, what) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never said %q: %+v", id, what, j.Events)
		}
	}
}

// heldWithin is whether the Mac holds job id at some moment within d.
func heldWithin(v *dev, id string, d time.Duration) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if v.snapshot().ActiveJob == id {
			return true
		}
	}
	return false
}

// A Volunteer's job waiting for its Mac's Availability window is not on the
// Mac: a drain does not wait for it, the window opening during the drain
// does not send it, and it is sent once the drain is lifted.
func TestAJobWaitingForAWindowDoesNotHoldADrain(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	v, secret := l.signedIn(graceID).lent("shelf-mac")
	first := v.snapshot().FirstBaseline.Job
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification})
	jobSaid(t, l.s, first, "no Availability window")

	if w := opsOf(l.s, "POST", "/drain"); w.Code != 200 || w.Body.String() != "0 0\n" {
		t.Fatalf("with a job waiting for its window: %d %q", w.Code, w.Body)
	}
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification, Window: &window.Settings{OpenUntil: l.clock.Now().Add(6 * time.Hour)}})
	if heldWithin(v, first, 1500*time.Millisecond) {
		t.Fatal("a job was sent to the Mac while jobs are drained")
	}
	if w := opsOf(l.s, "POST", "/drain"); w.Body.String() != "0 0\n" {
		t.Fatalf("with the window open during the drain: %q", w.Body)
	}
	opsOf(l.s, "DELETE", "/drain")
	if !heldWithin(v, first, 10*time.Second) {
		t.Fatalf("the job was not sent once the drain was lifted: %s", v.snapshot().StateReason)
	}
}

// idleAfterStop waits until a stopped labd runs no job any more.
func idleAfterStop(t *testing.T, s *Server) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		n := len(s.cancels)
		s.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("labd still runs %d jobs after it stopped", n)
		}
	}
}

// A Deploy drains, then restarts labd. A Volunteer's job waiting for its
// Mac's window was never sent, so the restart takes nothing from it: it
// is still queued in the store, the next labd queues it again, and it is
// sent once the window opens.
func TestAJobWaitingForAWindowStaysQueuedAcrossARestart(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	v, secret := l.signedIn(graceID).lent("shelf-mac")
	first := v.snapshot().FirstBaseline.Job
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification})
	jobSaid(t, l.s, first, "no Availability window")
	if w := opsOf(l.s, "POST", "/drain"); w.Body.String() != "0 0\n" {
		t.Fatalf("with a job waiting for its window: %q", w.Body)
	}

	stopLabd(t, l.s)
	idleAfterStop(t, l.s)
	j, err := l.s.store.job(first)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != api.JobQueued {
		t.Fatalf("after labd stopped the waiting job is %s: %s", j.State, j.Summary)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	again, err := New(ctx, l.s.cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	l.s, l.h = again, again.Handler()
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification, Window: &window.Settings{OpenUntil: l.clock.Now().Add(6 * time.Hour)}})
	if !heldWithin(again.dev("shelf-mac"), first, 10*time.Second) {
		j, _ := again.store.job(first)
		t.Fatalf("the job was not sent after the restart: %s %s %+v", j.State, j.Summary, j.Events)
	}
}

// A job that waited for its build when labd stopped was never sent to its
// Mac either: the next labd queues it again, and its build is asked for
// again, instead of ending it as one that was running.
func TestAJobWaitingForItsBuildIsQueuedAgainAtTheStart(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "labd.db"))
	if err != nil {
		t.Fatal(err)
	}
	d := api.Device{ID: "1", Name: "built-for", State: api.StateReady, KnownGood: "7.2"}
	st.saveDevice(&d, "h1")
	st.saveJob(&api.Job{ID: "j-building", DeviceID: "1", Spec: api.JobSpec{Device: d.Name, Source: "https://github.com/o/r/commit/" + strings.Repeat("a", 40)},
		State: api.JobBuilding, Created: time.Now()})
	st.db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newWith(harnessMaintainer, "lab_mtok", ctx, Config{DataDir: dir}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	jobSaid(t, s, "j-building", "resolving")
	if j, _ := s.store.job("j-building"); strings.Contains(j.Summary, "labd restarted") {
		t.Fatalf("the job that waited for its build: %s %s", j.State, j.Summary)
	}
}

// saidTimes waits until job id has n events containing what.
func saidTimes(t *testing.T, s *Server, id, what string, n int) {
	t.Helper()
	got := 0
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		got = 0
		if j, err := s.store.job(id); err == nil {
			for _, e := range j.Events {
				if strings.Contains(e.Msg, what) {
					got++
				}
			}
		}
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s said %q %d times, want %d", id, what, got, n)
		}
	}
}

// pendingOf counts the commands of this kind waiting for the Mac.
func pendingOf(v *dev, kind string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, c := range v.pending {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

// restartLabd starts a labd again on the data the stopped one left.
func restartLabd(t *testing.T, s *Server) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	again, err := New(ctx, s.cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); idleAfterStop(t, again) })
	return again
}

// A Deploy stops labd while a job from a source prepares: it resolves the
// source and waits for its Mac's kernel config. The job was never sent, so
// the stop ends nothing: it is still unfinished in the store and the next
// labd prepares it again.
func TestAStopWhileASourceJobPreparesKeepsItQueued(t *testing.T) {
	l := rolesLab(t, func(c *Config) { c.AliveWindow, c.OfflineAfter = time.Hour, time.Hour })
	v := l.lrMac("desk-mac", "j313", nil)
	go l.s.deviceLoop(v)
	spec, _ := json.Marshal(api.JobSpec{Device: "desk-mac", Source: gitRepo(t)})
	w := l.signedIn(maintainerID).write("POST", "/api/jobs", string(spec), "Content-Type", "application/json")
	var j api.Job
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &j) != nil {
		t.Fatalf("submit: %d %s", w.Code, w.Body)
	}
	jobSaid(t, l.s, j.ID, "resolving")
	for deadline := time.Now().Add(10 * time.Second); pendingOf(v, api.CmdConfig) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the job never asked the Mac for its kernel config")
		}
	}

	stopLabd(t, l.s)
	idleAfterStop(t, l.s)
	if got, _ := l.s.store.job(j.ID); got.State == api.JobDone {
		t.Fatalf("the stop ended the job: %s: %s", got.Outcome, got.Summary)
	}
	again := restartLabd(t, l.s)
	saidTimes(t, again, j.ID, "resolving", 2)
	again.cancelJob(j.ID, "")
}

// A person cancels a job that waits on its Mac, and labd stops before its
// runner has seen the cancel: the cancel was answered, so it is in the store
// already, and the job never runs, in this labd or the next.
func TestACancelAnsweredBeforeAStopIsNeverUndone(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	v := l.s.dev(l.name)
	reached, release := make(chan struct{}), make(chan struct{})
	v.mu.Lock()
	v.beforeSend = func() {
		v.mu.Lock()
		v.beforeSend = nil
		v.mu.Unlock()
		close(reached)
		<-release
	}
	v.mu.Unlock()
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-canceled behavior=ok")})
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the job never got as far as its send")
	}
	if r, body := l.get("POST", "/api/jobs/"+id+"/cancel"); r.StatusCode != 200 {
		t.Fatalf("cancel: %s %s", r.Status, body)
	}
	if j, _ := l.s.store.job(id); !j.Canceled && j.State != api.JobDone {
		t.Errorf("the cancel was answered and the store does not have it: %s", j.State)
	}
	opsOf(l.s, "POST", "/drain") // a Deploy drains, then stops labd
	l.cancel()
	close(release)
	idleAfterStop(t, l.s)

	again := restartLabd(t, l.s)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		j, _ := again.store.job(id)
		if j.State == api.JobDone {
			if j.Outcome != api.OutcomeCanceled || j.Result.Booted {
				t.Fatalf("the canceled job ended %s (booted %v): %s", j.Outcome, j.Result.Booted, j.Summary)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the restart the canceled job is %s: %v", j.State, j.Events)
		}
	}
}

// A job given its Mac is staging in the store before the Mac gets its first
// command, the clean-up; a cancel then is stored too. labd ending there (no
// Deploy's drain: a crash) does not have the next labd run the job.
func TestACancelThenACrashDuringTheFirstCleanUpNeverRunsTheJob(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.beforeSend = func() {
		v.mu.Lock()
		v.beforeSend = nil
		v.mu.Unlock()
		l.sim.Sleep() // the clean-up stays on its way
	}
	v.mu.Unlock()
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-crashed behavior=ok")})
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		v.mu.Lock()
		given := v.d.ActiveJob == id
		for _, c := range v.sent {
			given = given && c.Kind == api.CmdCleanup || given && len(v.pending) > 0
		}
		v.mu.Unlock()
		if given && (pendingOf(v, api.CmdCleanup) > 0 || func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.sent) > 0 }()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job never sent its Mac the first clean-up")
		}
	}
	if j, _ := l.s.store.job(id); j.State == api.JobQueued {
		t.Fatal("the job is still queued in the store while its Mac has its first command: a labd ending now would run it again")
	}
	if r, body := l.get("POST", "/api/jobs/"+id+"/cancel"); r.StatusCode != 200 {
		t.Fatalf("cancel: %s %s", r.Status, body)
	}
	if j, _ := l.s.store.job(id); !j.Canceled && j.State != api.JobDone {
		t.Errorf("the cancel was answered and the store does not have it: %s", j.State)
	}
	l.cancel()
	idleAfterStop(t, l.s)
	l.sim.Wake()

	again := restartLabd(t, l.s)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		j, _ := again.store.job(id)
		if j.State == api.JobDone {
			if j.Outcome == api.OutcomePass || j.Result.Booted {
				t.Fatalf("the canceled job ran after the restart: %s (booted %v): %s", j.Outcome, j.Result.Booted, j.Summary)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the restart the canceled job is %s: %v", j.State, j.Events)
		}
	}
}

// A job whose cancel is in the store, still queued when labd stopped, is
// ended as canceled by the next labd, not run.
func TestAStoredCancelIsHonouredAtTheStart(t *testing.T) {
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "labd.db"))
	if err != nil {
		t.Fatal(err)
	}
	d := api.Device{ID: "1", Name: "canceled-on", State: api.StateReady, KnownGood: "7.2", Kernel: "7.2", LastSeen: time.Now()}
	st.saveDevice(&d, "h1")
	st.saveJob(&api.Job{ID: "j-asked", DeviceID: "1", Spec: api.JobSpec{Device: d.Name, Kernel: strings.Repeat("c", 64)},
		State: api.JobQueued, Canceled: true, Created: time.Now()})
	st.db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newWith(harnessMaintainer, "lab_mtok", ctx, Config{DataDir: dir, AliveWindow: time.Hour}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		j, _ := s.store.job("j-asked")
		if j.State == api.JobDone {
			if j.Outcome != api.OutcomeCanceled {
				t.Fatalf("the job canceled before the stop ended %s: %s", j.Outcome, j.Summary)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job canceled before the stop is %s: %v", j.State, j.Events)
		}
	}
	cancel()
	idleAfterStop(t, s)
}

// A job waiting for its build when labd stops resumes from that build at
// the next start: the commit it resolved and the config it took, though
// the branch it named has moved on meanwhile.
func TestAJobQueuedAgainBuildsTheCommitItRecorded(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	src := gitRepo(t)
	id := l.submitJob(api.JobSpec{Source: src})
	jobSaid(t, l.s, id, "no builder host is connected")
	j, _ := l.s.store.job(id)
	first, err := l.s.getBuild(j.Spec.Build)
	if err != nil || first.Source.SHA == "" || first.ConfigSHA == "" {
		t.Fatalf("the recorded build: %+v %v", first, err)
	}
	l.cancel()
	idleAfterStop(t, l.s)

	dir := filepath.Join(testGitRoot, strings.TrimSuffix(strings.TrimPrefix(src, testGitHub), "#main"))
	commit := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-q", "--allow-empty", "-m", "moved on")
	commit.Dir = dir
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, out)
	}
	if now, _ := resolve(context.Background(), mustParse(t, src)); now.SHA == first.Source.SHA {
		t.Fatal("main did not move")
	}

	again := restartLabd(t, l.s)
	saidTimes(t, again, id, "no builder host is connected", 2)
	j, _ = again.store.job(id)
	b, err := again.getBuild(j.Spec.Build)
	if err != nil || b.Source.SHA != first.Source.SHA || b.ConfigSHA != first.ConfigSHA {
		t.Fatalf("after the restart the job builds %+v (%v), want commit %s with config %s", b, err, first.Source.SHA, first.ConfigSHA)
	}
	again.cancelJob(id, "")
}

// While a person's Hard reset waits for the controller the Mac takes no
// job, which the reset would cut short; the job is sent once it has gone.
func TestAJobWaitsWhileAHardResetIsOnItsWay(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.resetting = true
	v.mu.Unlock()
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-resetting behavior=ok")})
	jobSaid(t, l.s, id, "a Hard reset of "+l.name+" is on its way")
	l.staysQueued(id, 1500*time.Millisecond, "a Hard reset is on its way")
	v.mu.Lock()
	v.resetting = false
	v.bump()
	v.mu.Unlock()
	l.expect(l.jobAPI(id, 60*time.Second), api.OutcomePass)
}

// With no job running the Builder is handed no build either: the restart
// that follows a drain would end it. A build that runs is counted.
func TestADrainHandsTheBuilderNoBuild(t *testing.T) {
	b := newBuilderLab(t)
	token := b.addBuilder("mini", api.BuilderKernel)
	running := b.queue("")
	if a, code := b.poll(token, "", 5*time.Second); code != 200 || a.Build.ID != running.ID {
		t.Fatalf("the builder got no build: %d %+v", code, a)
	}
	waiting := b.queue("")

	if w := opsOf(b.s, "POST", "/drain"); w.Code != 200 || w.Body.String() != "0 1\n" {
		t.Fatalf("with one build running: %d %q", w.Code, w.Body)
	}
	bd := b.build(running.ID)
	bd.State = api.BuildDone
	b.s.saveBuild(bd)
	b.s.builds.state.Lock()
	delete(b.s.builds.claims, running.ID)
	b.s.builds.state.Unlock()
	if w := opsOf(b.s, "POST", "/drain"); w.Body.String() != "0 0\n" {
		t.Fatalf("with that build done: %q", w.Body)
	}
	if code := pollHere(b, token, time.Second); code == 200 {
		t.Fatal("a build was handed out while jobs are drained")
	}
	if got := b.build(waiting.ID).State; got != api.BuildQueued {
		t.Errorf("the waiting build is %s", got)
	}

	opsOf(b.s, "DELETE", "/drain")
	if a, code := b.poll(token, "", 5*time.Second); code != 200 || a.Build.ID != waiting.ID {
		t.Errorf("after the drain the builder got no build: %d %+v", code, a)
	}
}

// A job from a source asks the drain before it prepares anything: while
// jobs are drained it reads no config from its Mac and resolves nothing,
// and it goes on once the drain is lifted.
func TestADrainHoldsASourceJobBeforeItsPreparation(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	// The baseline stops counting as sent just after its Mac is Ready.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		l.s.drain.mu.Lock()
		sent := l.s.drain.jobs
		l.s.drain.mu.Unlock()
		if sent == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("labd still counts %d jobs sent after the baseline", sent)
		}
	}
	if w := opsOf(l.s, "POST", "/drain"); w.Body.String() != "0 0\n" {
		t.Fatalf("draining: %q", w.Body)
	}
	id := l.submitJob(api.JobSpec{Source: "https://github.com/o/r/commit/" + strings.Repeat("b", 40)})
	jobSaid(t, l.s, id, jobsDrained)
	l.staysQueued(id, 1500*time.Millisecond, "jobs are drained")
	opsOf(l.s, "DELETE", "/drain")
	jobSaid(t, l.s, id, "resolving")
	// The job is ended before the test is, so it writes nothing into the
	// lab's data folder while that is removed.
	l.cancelWhen(id, "resolving")
	l.wait(id, 30*time.Second)
}

// What Deploy asks after its switch: whether the Builder asks this labd for
// work and whether the Out-of-band controller answers it.
func TestPartsSaysHowTheBuilderAndTheControllerAre(t *testing.T) {
	b := newBuilderLab(t)
	if w := opsOf(b.s, "GET", "/parts"); w.Code != 200 || w.Body.String() != "builder none\ncontroller none\n" {
		t.Fatalf("with neither set up: %d %q", w.Code, w.Body)
	}
	token := b.addBuilder("mini", api.BuilderKernel)
	if w := opsOf(b.s, "GET", "/parts"); w.Body.String() != "builder silent\ncontroller none\n" {
		t.Fatalf("with a builder that never asked: %q", w.Body)
	}
	pollHere(b, token, 300*time.Millisecond)
	if w := opsOf(b.s, "GET", "/parts"); w.Body.String() != "builder ok\ncontroller none\n" {
		t.Fatalf("with a builder that asks: %q", w.Body)
	}

	c := newController(t)
	o := newOps(t, func(cfg *Config) { cfg.OOB = c.client(t) })
	if w := opsOf(o.s, "GET", "/parts"); !strings.Contains(w.Body.String(), "controller ok\n") {
		t.Fatalf("with a controller that answers: %q", w.Body)
	}
	c.mu.Lock()
	c.down = true
	c.mu.Unlock()
	if w := opsOf(o.s, "GET", "/parts"); !strings.Contains(w.Body.String(), "controller bad\n") {
		t.Fatalf("with a controller that does not: %q", w.Body)
	}
}

func TestTheDrainIsOnTheOpsSocketOnly(t *testing.T) {
	b := newBuilderLab(t)
	for _, c := range []struct{ method, path string }{{"POST", "/drain"}, {"DELETE", "/drain"}, {"GET", "/parts"}} {
		w := httptest.NewRecorder()
		b.s.Handler().ServeHTTP(w, httptest.NewRequest(c.method, "http://127.0.0.1"+c.path, nil))
		if w.Code == http.StatusOK {
			t.Errorf("%s %s answers on the lab's own routes: %d", c.method, c.path, w.Code)
		}
	}
	if w := opsOf(b.s, "GET", "/drain"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /drain: %d", w.Code)
	}
}

// A job a Leave ends while the Mac's loop holds its own copy (waiting for
// the Mac) stays ended: the cancel that follows marks the loop's copy, read
// before the Leave, and its save must not write the job back queued. Nor
// may two changes by id lose one: each reads and saves in one hold.
func TestAJobEndedByALeaveIsNotWrittenBackByACopyReadBefore(t *testing.T) {
	l := newLab(t, false)
	j := &api.Job{ID: "j-left", Spec: api.JobSpec{Device: "nowhere", Baseline: true}, State: api.JobQueued, Created: time.Now()}
	l.s.store.saveJob(j)
	ended := func(j *api.Job) {
		j.State, j.Outcome, j.Summary = api.JobDone, api.OutcomeCanceled, "canceled: it left the lab"
	}
	mark := func(j *api.Job) {
		if j.State != api.JobDone {
			j.Canceled = true
		}
	}

	held, _ := l.s.store.job(j.ID) // the loop's copy, as it waits for the Mac
	c := &jobCancel{}
	c.hold(held)
	l.s.updateJobByID(j.ID, ended)
	c.ask(l.s, nil, j.ID)
	if got, _ := l.s.store.job(j.ID); got.State != api.JobDone || got.Outcome != api.OutcomeCanceled {
		t.Fatalf("a copy read before the Leave wrote the ended job back: %s %s", got.State, got.Outcome)
	}

	k := &api.Job{ID: "j-left-2", Spec: api.JobSpec{Device: "nowhere"}, State: api.JobQueued, Created: time.Now()}
	l.s.store.saveJob(k)
	other := make(chan struct{})
	var once sync.Once
	hook := func(id string) {
		if id != k.ID {
			return
		}
		once.Do(func() {
			go func() { l.s.updateJobByID(k.ID, ended); close(other) }()
			select { // the other change waits for this one's save, or lands in between
			case <-other:
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
	l.s.jobRead.Store(&hook)
	l.s.updateJobByID(k.ID, mark)
	<-other
	l.s.jobRead.Store(nil)
	if got, _ := l.s.store.job(k.ID); got.State != api.JobDone {
		t.Fatalf("a change by id between another's read and save was lost: %s canceled=%v", got.State, got.Canceled)
	}
}

// A job set aside behind another's Lease, ended by a Leave between the run
// reading it and setting it aside, stays ended: the Mac's loop has let it
// go, so a Queued written back would leave it queued for good.
func TestAJobEndedByALeaveAsItParksStaysDone(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "lender", Maintainer: "lender", Expires: l.s.now().Add(time.Hour)}
	v.mu.Unlock()
	j := &api.Job{ID: "j-parks", Spec: api.JobSpec{Device: l.name, Baseline: true}, Maintainer: "other", State: api.JobQueued, Created: time.Now()}
	l.s.store.saveJob(j)
	held, _ := l.s.store.job(j.ID) // the run's copy
	l.s.updateJobByID(j.ID, func(j *api.Job) {
		j.State, j.Outcome, j.Summary = api.JobDone, api.OutcomeCanceled, "canceled: it left the lab"
	})
	r := &jobRun{s: l.s, v: v, j: held}
	if !r.park() {
		t.Fatal("the job was not held back by the Lease")
	}
	if got, _ := l.s.store.job(j.ID); got.State != api.JobDone || got.Outcome != api.OutcomeCanceled {
		t.Fatalf("a job ended as it parked was written back: %s %s", got.State, got.Outcome)
	}
	v.mu.Lock()
	parked := len(v.parked)
	v.mu.Unlock()
	if parked != 0 {
		t.Errorf("an ended job was set aside: %d parked", parked)
	}
}
