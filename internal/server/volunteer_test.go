package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/window"
)

// Tokens and devices stored before the volunteer flag existed read as a
// lab Mac's.
func TestTokensFromBeforeVolunteers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "labd.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE devices (id TEXT PRIMARY KEY, name TEXT UNIQUE NOT NULL, secret_hash TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE tokens (token_hash TEXT PRIMARY KEY, name TEXT NOT NULL, label TEXT, model TEXT, expires INTEGER NOT NULL);
INSERT INTO tokens VALUES ('old', 'm1air', 'left desk', 'laptop', 9999999999);
INSERT INTO devices VALUES ('d-1', 'm2air', 'x', '{"id":"d-1","name":"m2air","label":"right desk","model":"laptop","state":"ready"}');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	tok, err := st.token("old", time.Now())
	if err != nil || tok.Name != "m1air" || tok.Volunteer {
		t.Fatalf("old token: %+v, %v", tok, err)
	}
	rows, err := st.devices()
	if err != nil || len(rows) != 1 || rows[0].d.Volunteer {
		t.Fatalf("old device: %+v, %v", rows, err)
	}
	if _, err := openStore(path); err != nil {
		t.Fatalf("opening it again: %v", err)
	}
}

// filler reads as n bytes of one character, made as they are read.
type filler struct {
	n     int64
	chunk []byte
}

func fill(n int64, c byte) *filler { return &filler{n, bytes.Repeat([]byte{c}, 32<<10)} }

func (f *filler) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(int64(len(p)), f.n)], f.chunk)
	f.n -= int64(n)
	return n, nil
}

// Enrollments, check-ins and reports have size limits real agents are far
// below; uploads go elsewhere and have none.
func TestAgentBodyLimits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := newWith("ada", "lab_adm_secret", ctx, Config{DataDir: t.TempDir()}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	// post sends body, of length bytes if length > 0, else of a length it
	// doesn't say.
	post := func(path, secret string, body io.Reader, length ...int64) (int, string) {
		t.Helper()
		r := httptest.NewRequest("POST", labdAt+path, body)
		r.ContentLength = -1
		for _, n := range length {
			r.ContentLength = n
		}
		if secret != "" {
			r.Header.Set("Authorization", "Bearer "+secret)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code, strings.TrimSpace(w.Body.String())
	}
	// huge is a JSON body with a string of n bytes in it.
	huge := func(prefix string, n int64, suffix string) io.Reader {
		return io.MultiReader(strings.NewReader(prefix), fill(n, 'a'), strings.NewReader(suffix))
	}
	// The facts of a Mac with plenty to say: a long command line and many
	// preflight problems and warnings.
	facts := api.Facts{Hostname: "omarchy", Machine: "aarch64", DTModel: "Apple MacBook Air (13-inch, M2, 2022)",
		Compatible: []string{"apple,j413", "apple,t8112", "apple,arm-platform"}, Bootloader: "limine", Initramfs: "mkinitcpio",
		Watchdog: true, PanicTimeout: 10, GUIUser: "omarchy", AgentVersion: "0.9.0"}
	for i := range 20 {
		facts.Problems = append(facts.Problems, fmt.Sprintf("problem %d: /boot has %d MB free; a lab kernel and its initramfs need 200 MB, so clear old kernels out first", i, i))
		facts.Warnings = append(facts.Warnings, fmt.Sprintf("warning %d: journald keeps the journal in memory only, so a lab boot that fails takes its journal with it", i))
	}
	enrollBody, _ := json.Marshal(api.EnrollRequest{Token: "enr_test_m1", Facts: facts, ActivationKey: testActivationKey, TermsSHA256: termsSum()})
	checkin, _ := json.Marshal(api.Checkin{BootID: "b1", Kernel: "6.17.0-asahi", Cmdline: strings.Repeat("x", 4096), Facts: &facts,
		Health: api.Health{Uptime: 100, SystemState: "running"}, Running: []string{"c1", "c2"}})
	if len(enrollBody) > enrollBodyMax/16 || len(checkin) > checkinBodyMax/16 {
		t.Errorf("a full enrollment is %d bytes and a full check-in %d: less headroom than wanted", len(enrollBody), len(checkin))
	}

	tok, err := s.store.addTokenForTest("m1")
	if err != nil {
		t.Fatal(err)
	}
	if code, body := post("/api/agent/enroll", "", huge(`{"token":"`+tok+`","facts":{"hostname":"`, enrollBodyMax, `"}}`)); code != 413 {
		t.Errorf("an enrollment over %d bytes: %d %s", enrollBodyMax, code, body)
	}
	code, body := post("/api/agent/enroll", "", bytes.NewReader(enrollBody))
	var e api.EnrollResponse
	if json.Unmarshal([]byte(body), &e); code != 200 || e.Secret == "" {
		t.Fatalf("enroll after the refusal, with the same token: %d %s", code, body)
	}

	if code, body := post("/api/agent/checkin", e.Secret, bytes.NewReader(checkin)); code != 200 {
		t.Errorf("a full check-in: %d %s", code, body)
	}
	if code, body := post("/api/agent/checkin", e.Secret, huge(`{"boot_id":"b1","facts":{"hostname":"`, checkinBodyMax, `"}}`)); code != 413 {
		t.Errorf("a check-in over %d bytes: %d %s", checkinBodyMax, code, body)
	}
	if d := s.dev("m1").snapshot(); d.Facts.Hostname != "omarchy" {
		t.Errorf("the refused check-in's facts were kept: hostname of %d bytes", len(d.Facts.Hostname))
	}

	// The largest real report: an exec's whole output, 1 MB per stream, all
	// control characters, which JSON writes six bytes each.
	junk := strings.Repeat("\x01", 1<<20)
	out, _ := json.Marshal(api.ExecResult{Stdout: junk, Stderr: junk, Via: "agent"})
	rep, _ := json.Marshal(api.Report{Results: []api.CommandResult{{ID: "c1", OK: true, Output: out}}})
	if code, body := post("/api/agent/report", e.Secret, bytes.NewReader(rep)); code != 200 {
		t.Errorf("a report of %d bytes: %d %s", len(rep), code, body)
	}
	// One that says it's too large is turned away unread; one that doesn't
	// say is cut off where it passes the limit, as check-ins are above.
	if code, body := post("/api/agent/report", e.Secret, fill(reportBodyMax+1, ' '), reportBodyMax+1); code != 413 {
		t.Errorf("a report over %d bytes: %d %s", reportBodyMax, code, body)
	}

	// The Mac adds files to its running job, which was submitted for it.
	if err := s.store.saveJob(&api.Job{ID: "j1016-120000-ab12", Spec: api.JobSpec{Device: "m1"}, DeviceID: e.DeviceID, State: api.JobTesting}); err != nil {
		t.Fatal(err)
	}
	v := s.dev("m1")
	v.mu.Lock()
	v.d.ActiveJob = "j1016-120000-ab12"
	v.mu.Unlock()
	if code, body := post("/api/agent/upload?job=j1016-120000-ab12&name=boot0/journal.txt", e.Secret, fill(reportBodyMax+1, 'j')); code != 200 {
		t.Errorf("an upload larger than any report: %d %s", code, body)
	}
	if fi, err := os.Stat(filepath.Join(s.jobDir("j1016-120000-ab12"), "boot0", "journal.txt")); err != nil || fi.Size() != reportBodyMax+1 {
		t.Errorf("the upload: %v, %v", fi, err)
	}
}

func TestVolunteerLabRunsVolunteerCommands(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true})
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	if _, err := l.volunteer("no-such-command"); err == nil || !strings.Contains(err.Error(), `"no-such-command"`) {
		t.Fatalf("an unknown Volunteer command: %v", err)
	}
}

func TestEncryptedMacWaitsForItsPassword(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true})
	l.sim.Encrypted = true
	before := l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	l.sim.PowerCycle()
	time.Sleep(time.Second) // several reboot delays
	if d := l.s.dev(l.name).snapshot(); d.BootID != before.BootID {
		t.Fatal("an encrypted Mac booted without its disk password")
	}
	l.sim.TypePassword()
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" && d.BootID != before.BootID })
}

func TestRollbackReturnsTheMacToItsSnapshot(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true})
	l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	l.sim.Snapshot()
	l.sim.DefaultKernel = "7.3-installed" // what an installer leaves behind
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.Kernel == "7.3-installed" })
	if err := l.sim.Rollback(); err != nil {
		t.Fatal(err)
	}
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.Kernel == agent.SimKnownGood })
}

func TestLabBootThatLosesTheNetworkNeverChecksIn(t *testing.T) {
	l := newLabWith(t, labConfig{volunteer: true, oob: true})
	l.openForTheLab(time.Now())
	l.baseline()
	j := l.run(api.JobSpec{Kernel: l.artifact("krel=7.2-offline behavior=no-network")})
	if j.Result.Booted || j.Outcome == api.OutcomePass {
		t.Fatalf("a Lab boot with no network reached labd: %s %+v", j.Outcome, j.Result)
	}
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Kernel == agent.SimKnownGood })
}

// ---- a Volunteer's Mac: arriving and the site's controls ----

// lent has the person signed in on b make a Join command for a Mac called
// name and the Mac enroll with it, and returns the Mac and its device secret.
func (b *browser) lent(name string) (*dev, string) {
	b.l.t.Helper()
	_, token := b.makeJoin(name)
	w := b.l.enrollWith(token, "activation-key-of-"+name+"-0123456789abcdef")
	var out api.EnrollResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Secret == "" {
		b.l.t.Fatalf("enrolling %s: %d %s", name, w.Code, w.Body)
	}
	v := b.l.s.dev(name)
	if v == nil {
		b.l.t.Fatalf("%s is not in the lab after it enrolled", name)
	}
	return v, out.Secret
}

// checkin is one check-in of the Mac whose device secret this is.
func (l *signInLab) checkin(secret string, ci api.Checkin) *httptest.ResponseRecorder {
	l.t.Helper()
	if ci.BootID == "" {
		ci.BootID, ci.Kernel = "boot-1", "6.17.0-own"
	}
	b, _ := json.Marshal(ci)
	return l.asMac("POST", "/api/agent/checkin", secret, string(b))
}

// macRecordOf is the Mac's record of one enrollment, oldest first.
func macRecordOf(t *testing.T, s *Server, deviceID string) []string {
	t.Helper()
	events, err := s.store.macRecord(deviceID, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- {
		out = append(out, events[i].Line)
	}
	return out
}

// labLinesOf is every line labd has for the Lab group, oldest first.
func labLinesOf(t *testing.T, s *Server) []string {
	t.Helper()
	rows, err := s.store.db.Query(`SELECT text FROM lines WHERE channel = 'lab' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var text string
		rows.Scan(&text)
		out = append(out, text)
	}
	return out
}

// allows is whether the lab may do on the Mac what only Full allows.
func allows(v *dev) bool {
	d := v.snapshot()
	return d.AllowsAnything()
}

// A Mac that Joined arrives closed: at Qualification with publishing off,
// with no Availability window, taking nothing. The lab queues its first
// baseline itself, with no Maintainer step. Its Join is a line of the Mac's
// record and of the Lab group, naming the Mac only.
func TestAMacArrivesClosedAtQualificationAndTheLabQueuesItsFirstBaseline(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	v, secret := l.signedIn(graceID).lent("shelf-mac")

	d := l.s.view(v)
	if d.Kind != api.KindVolunteer || d.AccessLevel != api.AccessQualification || d.Publish || d.Limited || d.SitePause != nil {
		t.Errorf("the Mac arrived as %s at %q, publishing %v", d.Kind, d.AccessLevel, d.Publish)
	}
	if d.Arrival != "no window yet" {
		t.Errorf("a Mac that just Joined reads %q, want %q", d.Arrival, "no window yet")
	}
	jobs, err := l.s.store.jobs("shelf-mac", 10, true)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("the lab queued %d jobs for the Mac that Joined (%v), want its first baseline", len(jobs), err)
	}
	if j := jobs[0]; !j.Spec.Baseline || j.Maintainer != "" || j.AccountID != 0 || j.State != api.JobQueued {
		t.Errorf("the first baseline: baseline %v, asked by %q (%d), %s; want the lab's own, queued", j.Spec.Baseline, j.Maintainer, j.AccountID, j.State)
	}
	if fb := d.FirstBaseline; fb == nil || fb.Job != jobs[0].ID || fb.Outcome != "" {
		t.Errorf("the Mac's first baseline on record: %+v, want job %s pending", fb, jobs[0].ID)
	}
	if got := macRecordOf(t, l.s, d.ID); len(got) != 1 || !strings.HasPrefix(got[0], "Joined as a Volunteer's Mac, at Qualification with publishing off") {
		t.Errorf("the Mac's record after the Join: %q", got)
	}
	lines := l.labLines()
	if !has(lines, "shelf-mac Joined as a Volunteer's Mac (") {
		t.Errorf("the Lab group has no line for the Join:\n%s", strings.Join(lines, "\n"))
	}
	for _, line := range append(lines, macRecordOf(t, l.s, d.ID)...) {
		if strings.Contains(line, "grace") {
			t.Errorf("a line about a Mac names its Volunteer: %q", line)
		}
	}

	// With no window the Mac takes nothing: the job waits, and says why.
	if r := l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification}); r.Code != http.StatusOK {
		t.Fatalf("the Mac's first check-in: %d %s", r.Code, r.Body)
	}
	v.mu.Lock()
	why := v.windowHoldLocked(time.Minute)
	v.mu.Unlock()
	if !strings.Contains(why, "no Availability window") {
		t.Errorf("a Mac with no window may take a job: %q", why)
	}
	if got := l.s.view(v).Arrival; got != ArrivalNoWindow {
		t.Errorf("checked in with no window, the Mac reads %q", got)
	}

	// Its Volunteer sets a weekly window at the Mac: the first check waits
	// for that window, and the change is a line of both.
	rule, err := window.ParseRule("mon 03:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification, Window: &window.Settings{Rules: []window.Rule{rule}, Zone: "Australia/Brisbane"}})
	if got := l.s.view(v).Arrival; got != "first check pending (restarts twice)" {
		t.Errorf("with a window and no baseline yet, the Mac reads %q", got)
	}
	if got := macRecordOf(t, l.s, d.ID); !has(got, "its Volunteer set its weekly Availability window: mon 03:00-04:00") {
		t.Errorf("the Mac's record lacks the new window: %q", got)
	}
	if got := l.labLines(); !has(got, "shelf-mac changed its weekly Availability window: mon 03:00-04:00.") {
		t.Errorf("the Lab group lacks the new window:\n%s", strings.Join(got, "\n"))
	}
	// A Lab-owned Mac's Join is posted too, and the lab queues nothing for it.
	ravi := l.signedIn(maintainerID)
	w := ravi.write("POST", "/api/enroll-tokens", `{"name":"bench-1"}`, "Content-Type", "application/json")
	var made api.EnrollTokenResponse
	if json.Unmarshal(w.Body.Bytes(), &made); made.Token == "" {
		t.Fatalf("a Join command for a Lab-owned Mac: %d %s", w.Code, w.Body)
	}
	if e := l.enrollWith(made.Token, "activation-key-of-bench-1-0123456789abcdef"); e.Code != http.StatusOK {
		t.Fatalf("enrolling bench-1: %d %s", e.Code, e.Body)
	}
	if got := l.labLines(); !has(got, "bench-1 Joined as a Lab-owned Mac (") {
		t.Errorf("the Lab group lacks the Lab-owned Mac's Join:\n%s", strings.Join(got, "\n"))
	}
	if jobs, _ := l.s.store.jobs("bench-1", 10, true); len(jobs) != 0 {
		t.Errorf("the lab queued %d jobs for a Lab-owned Mac that Joined", len(jobs))
	}
	if got := l.s.view(l.s.dev("bench-1")).Arrival; got != "" {
		t.Errorf("a Lab-owned Mac has an arrival stage: %q", got)
	}
}

// The site only takes away, and undoes only what it took: Pause until a
// time or Resume, Limit to Qualification until "Remove limit". They are the
// Mac's own Volunteer's. Open now, raising to Full and turning publishing on
// have no route: they happen at the Mac. The weekly schedule is the one
// thing the site gives (window_from_site_test.go).
func TestTheSiteOnlyTakesAwayAndUndoesOnlyWhatItTook(t *testing.T) {
	l, _ := lendLab(t)
	ravi := l.signedIn(maintainerID)
	ravi.did("/people/mark", "handle", "grace", "as", "invite")
	grace := l.signedIn(graceID)
	v, secret := grace.lent("shelf-mac")
	id := v.snapshot().ID
	at := func(action string) string { return "/my-macs/shelf-mac/" + action }
	// This Mac never answers a command: its first baseline is taken off.
	l.s.cancelJob(v.snapshot().FirstBaseline.Job, "")

	// Its Volunteer set, at the Mac, a window that is open now, Full and
	// publishing on. Those reach the lab by check-in and are each an event.
	rule, _ := window.ParseRule("mon-sun 00:00-23:59")
	mac := api.Checkin{AccessLevel: api.AccessFull, Publish: true, Window: &window.Settings{Rules: []window.Rule{rule}, Zone: "UTC"}}
	l.checkin(secret, mac)
	if d := v.snapshot(); !d.AllowsAnything() || !d.Publish {
		t.Fatalf("the Mac's own settings did not reach the lab: %q, publishing %v", d.AccessLevel, d.Publish)
	}
	for _, want := range []string{"shelf-mac raised its Access level to Full.", "shelf-mac turned publishing on."} {
		if got := l.labLines(); !has(got, want) {
			t.Errorf("the Lab group lacks %q:\n%s", want, strings.Join(got, "\n"))
		}
	}
	open := func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return !v.openUntilLocked().IsZero()
	}
	if !open() {
		t.Fatal("the Mac is not open inside its own window")
	}

	// Nobody but its Volunteer: another person and a Maintainer are told
	// what a Mac the lab does not have would be.
	l.signedIn(maintainerID).did("/people/mark", "handle", "edith", "as", "invite")
	for who, b := range map[string]*browser{"another Volunteer": l.signedIn(edithID), "a Maintainer": ravi} {
		for _, action := range []string{"pause", "resume", "limit", "remove-limit", "leave"} {
			if w := b.post(at(action), "sure", "yes"); w.Code != http.StatusNotFound {
				t.Errorf("%s pressed %s on a Mac that is not theirs: %d", who, action, w.Code)
			}
		}
	}
	if d := v.snapshot(); d.SitePause != nil || d.Limited || l.s.dev("shelf-mac") == nil {
		t.Fatal("someone else's button changed the Mac")
	}

	// Pause until Resume closes the Mac, an open window or not.
	grace.did(at("pause"), "sure", "yes")
	if open() || !v.paused() {
		t.Error("Paused from the site, the Mac is still open to the lab")
	}
	v.mu.Lock()
	why := v.windowHoldLocked(time.Minute)
	v.mu.Unlock()
	if !strings.Contains(why, "Paused by its Volunteer until they resume it") {
		t.Errorf("a job waiting on a Paused Mac says %q", why)
	}
	// What the Mac says cannot undo it: an Open now is the Mac's to give
	// inside what the site left, and the site took everything.
	opened := mac
	settings := *mac.Window
	settings.OpenUntil = l.clock.Now().Add(3 * time.Hour)
	opened.Window = &settings
	l.checkin(secret, opened)
	if open() {
		t.Error("an Open now at the Mac opened a Mac Paused from the site")
	}
	grace.did(at("resume"))
	if !open() {
		t.Error("Resume did not open the Mac again")
	}
	grace.refused("The site undoes only what it took", at("resume"))

	// Pause until a time ends by itself, and is refused for a time gone by.
	until := l.clock.Now().Add(2 * time.Hour)
	grace.refused("a time still to come", at("pause"), "until", l.clock.Now().Add(-time.Minute).Format(time.RFC3339), "sure", "yes")
	grace.refused("Pause until a date and a time", at("pause"), "until", "tomorrow")
	grace.did(at("pause"), "until", until.Format(time.RFC3339), "sure", "yes")
	if open() {
		t.Error("Paused until a time, the Mac is still open")
	}
	if p := l.s.view(v).SitePause; p == nil || !p.Until.Equal(until.Truncate(time.Second)) {
		t.Errorf("the Pause on record: %+v, want until %s", p, until)
	}
	l.clock.Advance(2*time.Hour + time.Second)
	if !open() || l.s.view(v).SitePause != nil {
		t.Error("the Pause did not end at its time")
	}
	grace.refused("The site undoes only what it took", at("resume"))

	// A Pause the Volunteer set on the Mac is not the site's to undo.
	own := mac
	paused := *mac.Window
	paused.PausedUntil = l.clock.Now().Add(48 * time.Hour)
	own.Window = &paused
	l.checkin(secret, own)
	if open() {
		t.Fatal("Paused at the Mac, the Mac is still open")
	}
	grace.refused("The site undoes only what it took", at("resume"))
	if open() {
		t.Error("the site's Resume undid a Pause set on the Mac")
	}
	l.checkin(secret, mac)

	// Limit to Qualification holds the Mac at Qualification for the lab,
	// whatever the Mac says, until "Remove limit", which gives back only
	// what the Mac has.
	grace.did(at("limit"), "sure", "yes")
	if d := v.snapshot(); d.AllowsAnything() || d.AccessLevel != api.AccessFull || !d.Limited {
		t.Errorf("Limited from the site: allows anything %v, the Mac's own level %q", d.AllowsAnything(), d.AccessLevel)
	}
	if err := refuseBelowFull(&api.Device{Name: "shelf-mac", Enrollment: api.Enrollment{Kind: api.KindVolunteer}, AccessLevel: api.AccessFull, Limited: true}, actMesa); err == nil || !strings.Contains(err.Error(), "limited to Access level Qualification") {
		t.Errorf("what Full allows, on a limited Mac: %v", err)
	}
	grace.refused("limited to Qualification already", at("limit"), "sure", "yes")
	l.checkin(secret, mac) // the Mac still says Full
	if allows(v) {
		t.Error("the Mac's check-in lifted the site's Limit")
	}
	grace.did(at("remove-limit"))
	if !allows(v) {
		t.Error("Remove limit did not give back the Mac's own Full")
	}
	grace.refused("The site undoes only what it took", at("remove-limit"))
	// At Qualification on the Mac, the site has nothing to give.
	lowered := mac
	lowered.AccessLevel = api.AccessQualification
	l.checkin(secret, lowered)
	grace.did(at("limit"), "sure", "yes")
	grace.did(at("remove-limit"))
	if d := v.snapshot(); d.AllowsAnything() || d.AccessLevel != api.AccessQualification {
		t.Errorf("Remove limit raised a Mac its Volunteer keeps at Qualification: %q", d.AccessLevel)
	}

	// Only the Mac gives: the site has no button for it.
	for _, action := range []string{"open", "open-now", "schedule", "access", "full", "raise", "publish", "publishing"} {
		if w := grace.post(at(action), "sure", "yes", "until", "23:00", "level", "full", "on", "true"); w.Code != http.StatusNotFound {
			t.Errorf("POST %s: %d, want no such route", at(action), w.Code)
		}
	}
	if d := v.snapshot(); d.AccessLevel != api.AccessQualification || d.Window.OpenUntil.After(l.clock.Now()) {
		t.Error("a request to the site gave what only the Mac gives")
	}

	// Each event is in the Mac's record and went to the Lab group, naming
	// the Mac only.
	record, lines := macRecordOf(t, l.s, id), l.labLines()
	for _, want := range []string{
		"its Volunteer raised its Access level to Full",
		"its Volunteer turned publishing on",
		"its Volunteer Paused it from the site until they resume it",
		"its Volunteer opened it now, until ",
		"its Volunteer resumed it from the site",
		"its Volunteer Paused it from the site until ",
		"its Volunteer Paused it at the Mac until ",
		"its Volunteer limited it to Qualification from the site",
		"its Volunteer removed the limit from the site",
		"its Volunteer lowered its Access level to Qualification",
	} {
		if !has(record, want) {
			t.Errorf("the Mac's record lacks %q:\n%s", want, strings.Join(record, "\n"))
		}
	}
	for _, want := range []string{
		"shelf-mac is Paused.", "shelf-mac is open now, until ", "shelf-mac resumed.", "shelf-mac is Paused until ",
		"shelf-mac is limited to Qualification.", "The limit on shelf-mac was removed.", "shelf-mac lowered its Access level to Qualification.",
	} {
		if !has(lines, want) {
			t.Errorf("the Lab group lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	for _, line := range append(record, lines...) {
		if strings.Contains(line, "grace") && !strings.Contains(line, "Invite") {
			t.Errorf("a line about a Mac names its Volunteer: %q", line)
		}
	}
	// The same check-in again, and a window passing on schedule, are no event.
	// The day's summary goes to the Lab group as the day passes: it is the
	// lab's line, not one about the Mac.
	summaries := func() (n int) {
		for _, line := range l.labLines() {
			if strings.HasPrefix(line, "Omarchy Mac Lab, ") {
				n++
			}
		}
		return n
	}
	events := func() int { return len(macRecordOf(t, l.s, id)) + len(l.labLines()) - summaries() }
	before, summariesBefore := events(), summaries()
	l.checkin(secret, lowered)
	l.clock.Advance(26 * time.Hour)
	for deadline := time.Now().Add(10 * time.Second); summaries() == summariesBefore; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a day passed and the lab posted no summary")
		}
	}
	l.checkin(secret, lowered)
	if after := events(); after != before {
		t.Errorf("a check-in that changed nothing, and a day of the schedule, made %d lines", after-before)
	}
}

// arrival is the stage of the lab's Mac as My Macs words it.
func (l *lab) arrival() string {
	return l.s.view(l.s.dev(l.name)).Arrival
}

// The first baseline of a Volunteer's Mac is queued by the lab itself and
// runs in the Mac's first open window, with no Maintainer step. The stages
// read "no window yet", "first check pending (restarts twice)", "ready".
func TestTheFirstBaselineRunsInTheFirstOpenWindow(t *testing.T) {
	clock := agent.NewFakeClock(time.Date(2026, time.October, 19, 12, 0, 0, 0, time.Local))
	l := newLabWith(t, labConfig{volunteer: true, lender: veraID, clock: clock})
	t.Cleanup(pumpClock(clock))
	d := l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	if d.FirstBaseline == nil || d.FirstBaseline.Job == "" {
		t.Fatalf("the lab queued no first baseline for the Mac that Joined: %+v", d.FirstBaseline)
	}
	first := d.FirstBaseline.Job
	if got := l.arrival(); got != "no window yet" {
		t.Errorf("with no window the Mac reads %q", got)
	}
	l.staysQueued(first, 2*time.Second, "the Mac has no Availability window")

	// The Volunteer answers the window question a little late: a window
	// that opens in two minutes. The first check waits for it.
	start := clock.Now().Truncate(time.Minute).Add(2 * time.Minute)
	l.addWindow(weekly(start, 6*time.Hour))
	if got := l.arrival(); got != "first check pending (restarts twice)" {
		t.Errorf("with a window ahead and no baseline yet the Mac reads %q", got)
	}
	l.staysQueued(first, time.Second, "the Mac's first window has not opened")
	advanceTo(clock, start.Add(time.Second))

	j := l.jobAPI(first, 90*time.Second)
	l.expect(j, api.OutcomePass)
	if j.Maintainer != "" || j.AccountID != 0 {
		t.Errorf("the first baseline was asked by %q (%d), want the lab itself", j.Maintainer, j.AccountID)
	}
	d = l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.KnownGood != "" })
	if fb := d.FirstBaseline; fb == nil || fb.Job != first || fb.Outcome != api.FirstBaselinePassed {
		t.Errorf("the first baseline on record: %+v", fb)
	}
	if got := l.arrival(); got != "ready" {
		t.Errorf("after its first baseline the Mac reads %q", got)
	}
	if got := labLinesOf(t, l.s); !has(got, l.name+" passed its first baseline.") {
		t.Errorf("the Lab group's lines:\n%s", strings.Join(got, "\n"))
	}
	if got := macRecordOf(t, l.s, d.ID); !has(got, "passed its first baseline (job "+first+")") {
		t.Errorf("the Mac's record: %q", got)
	}
	// A later baseline is not a first one.
	l.baseline()
	n := 0
	for _, line := range labLinesOf(t, l.s) {
		if strings.Contains(line, "first baseline") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d lines about a first baseline, want one", n)
	}
}

// A first baseline that fails is shown and posted, and a Maintainer looks:
// theirs, once it passes, is the Mac's first.
func TestAFailedFirstBaselineIsShownAndPosted(t *testing.T) {
	clock := agent.NewFakeClock(time.Date(2026, time.October, 19, 12, 0, 0, 0, time.Local))
	l := newLabWith(t, labConfig{volunteer: true, lender: veraID, clock: clock})
	t.Cleanup(pumpClock(clock))
	d := l.waitDevice(func(d api.Device) bool { return d.BootID != "" })
	first := d.FirstBaseline.Job
	l.sim.FailStage(func(int) error { return errors.New("no space left on the ESP") })
	l.openForTheLab(clock.Now())

	j := l.jobAPI(first, 90*time.Second)
	if j.Outcome == api.OutcomePass || j.Outcome == api.OutcomeIncomplete || j.Outcome == api.OutcomeCanceled {
		t.Fatalf("the baseline that could not stage ended %s: %s", j.Outcome, j.Summary)
	}
	d = l.waitDevice(func(d api.Device) bool { return d.FirstBaseline != nil && d.FirstBaseline.Outcome != "" })
	if d.FirstBaseline.Outcome != api.FirstBaselineFailed || d.FirstBaseline.Job != first {
		t.Errorf("the first baseline on record: %+v", d.FirstBaseline)
	}
	if got := l.arrival(); got != "first check failed: a Maintainer looks" {
		t.Errorf("after a failed first baseline the Mac reads %q", got)
	}
	if got := labLinesOf(t, l.s); !has(got, l.name+" failed its first baseline.") {
		t.Errorf("the Lab group's lines:\n%s", strings.Join(got, "\n"))
	}
	if got := macRecordOf(t, l.s, d.ID); !has(got, "failed its first baseline (job "+first) {
		t.Errorf("the Mac's record: %q", got)
	}
	if got := l.s.macState(l.s.dev(l.name), l.s.view(l.s.dev(l.name))); got != "first check failed: a Maintainer looks" {
		t.Errorf("Macs shows the Mac as %q", got)
	}

	l.sim.FailStage(nil)
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady || d.State == api.StateNew })
	l.baseline()
	if got := l.arrival(); got != "ready" {
		t.Errorf("after a Maintainer's baseline passed the Mac reads %q", got)
	}
	if got := labLinesOf(t, l.s); !has(got, l.name+" passed its first baseline.") {
		t.Errorf("the Lab group's lines:\n%s", strings.Join(got, "\n"))
	}
}

// A Limit taken on the site holds a Mac its Volunteer raised to Full at
// Qualification: it ends a Lease, stops a running job that needs Full as a
// closing window does, and holds such a job back until "Remove limit".
func TestALimitFromTheSiteEndsALeaseAndStopsAJobThatNeedsFull(t *testing.T) {
	l, _ := windowEndLab(t, labConfig{lender: veraID})
	v := l.s.dev(l.name)
	l.mustVolunteer("access", "full")
	l.waitDevice(func(d api.Device) bool { return d.AccessLevel == api.AccessFull })

	// A Maintainer's script runs, and another waits behind it.
	l.sim.SetTestLength(time.Minute)
	script := l.submitJob(l.alScriptJob("7.2-limited", "echo a Maintainer's script", false))
	l.waitTesting(script, "7.2-limited")
	queued := l.submitJob(l.alScriptJob("7.2-queued", "echo another", false))
	labBoot, _, _ := l.sim.Identity()
	if err := l.s.limitMac(v, l.vera()); err != nil {
		t.Fatal(err)
	}
	l.backOnItsOwnSystem(labBoot, 15*time.Second, "its Volunteer limited the Mac from the site")
	j := l.jobAPI(script, 60*time.Second)
	l.expect(j, api.OutcomeIncomplete)
	if !strings.Contains(j.Summary, "its Volunteer limited the Mac to Qualification from the site") {
		t.Errorf("the job a Limit stopped says %q", j.Summary)
	}
	l.sim.SetTestLength(0)
	// The one queued before the Limit needs Full too: the lab holds it,
	// where lab-agent, which still reads Full, would run it.
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady && d.Health.JobTag == "" })
	l.staysQueued(queued, 2*time.Second, "the Mac is limited to Qualification")
	if held, _ := l.jobNow(queued); !strings.Contains(fmt.Sprint(held.Events), "limited to Access level Qualification by its Volunteer") {
		t.Errorf("the job waiting on the limited Mac does not say why: %v", held.Events)
	}
	if r, body := l.alSubmit(l.alScriptJob("7.2-refused", "echo no", false)); !alIs4xx(r) || !strings.Contains(body, "limited to Access level Qualification") {
		t.Errorf("a Maintainer's script for a limited Mac: %s %s", r.Status, body)
	}
	if err := l.s.unlimitMac(v, l.vera()); err != nil {
		t.Fatal(err)
	}
	l.expect(l.jobAPI(queued, 90*time.Second), api.OutcomePass)

	// At Full again, a new Limit ends a Lease, and under it a Lease gives
	// no lab exec.
	l.takeLease("connor (homework)", time.Hour)
	if err := l.s.limitMac(v, l.vera()); err != nil {
		t.Fatal(err)
	}
	if lease := l.leaseNow(); lease != nil {
		t.Fatalf("the Lease outlived the Limit: %+v", lease)
	}
	l.takeLease("connor (homework)", time.Hour)
	if r, _, body := l.alExec(harnessToken, "id"); !alIs4xx(r) || !strings.Contains(body, "limited to Access level Qualification") {
		t.Errorf("lab exec on a limited Mac: %s %s", r.Status, body)
	}
	if got := strings.Join(l.leaseEvents(), "\n"); !strings.Contains(got, "its Volunteer limited the Mac to Qualification from the site") {
		t.Errorf("the Lease record does not say why the Lease ended:\n%s", got)
	}
}

// A Pause taken on the site after a job's wait is over, and before labd
// gives it the Mac, still holds it: the job is not sent, and says why.
func TestAPauseJustBeforeAJobIsSentHoldsIt(t *testing.T) {
	l, _ := lendLab(t)
	l.signedIn(maintainerID).did("/people/mark", "handle", "grace", "as", "invite")
	v, secret := l.signedIn(graceID).lent("shelf-mac")
	first := v.snapshot().FirstBaseline.Job
	grace, err := l.s.person(graceID)
	if err != nil {
		t.Fatal(err)
	}
	paused := make(chan error, 1)
	v.mu.Lock()
	v.beforeSend = func() {
		v.mu.Lock()
		v.beforeSend = nil
		v.mu.Unlock()
		paused <- l.s.pauseMac(v, grace, time.Time{})
	}
	v.mu.Unlock()

	l.checkin(secret, api.Checkin{AccessLevel: api.AccessQualification, Window: &window.Settings{OpenUntil: l.clock.Now().Add(6 * time.Hour)}})
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the job's wait never ended")
	}
	if heldWithin(v, first, 1500*time.Millisecond) {
		t.Fatal("the job was sent to a Mac its Volunteer had just Paused")
	}
	jobSaid(t, l.s, first, "Paused by its Volunteer")
}

// A Limit taken on the site stops a running job and ends a Lease as a Pause
// does, whatever the Access level: on a Mac at Qualification too.
func TestALimitAtQualificationEndsTheLeaseAndStopsTheRunningJob(t *testing.T) {
	l, _ := windowEndLab(t, labConfig{lender: veraID})
	v := l.s.dev(l.name)
	l.takeLease("connor (homework)", time.Hour)
	l.sim.SetTestLength(time.Minute)
	id := l.submitJob(api.JobSpec{Kernel: l.artifact("krel=7.2-limited-q behavior=ok")})
	l.waitTesting(id, "7.2-limited-q")
	labBoot, _, _ := l.sim.Identity()
	if err := l.s.limitMac(v, l.vera()); err != nil {
		t.Fatal(err)
	}
	if lease := l.leaseNow(); lease != nil {
		t.Errorf("the Lease outlived a Limit at Qualification: %+v", lease)
	}
	l.backOnItsOwnSystem(labBoot, 15*time.Second, "its Volunteer limited the Mac from the site")
	j := l.jobAPI(id, 60*time.Second)
	l.expect(j, api.OutcomeIncomplete)
	if !strings.Contains(j.Summary, siteLimited) {
		t.Errorf("the job a Limit stopped says %q", j.Summary)
	}
}
