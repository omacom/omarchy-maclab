package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/window"
)

const (
	lrGrace = 8101
	lrLinus = 8102
)

// leaseLab is a lab whose GitHub knows the owner (its Admin), ravi, grace
// and linus, and someone the lab has not met.
func leaseLab(t *testing.T) *signInLab {
	t.Helper()
	l := pagesLab(t, nil)
	l.gh.AddUser(lrGrace, "grace")
	l.gh.AddUser(lrLinus, "linus")
	return l
}

// lrMaintainer signs ravi in as a Maintainer.
func (l *signInLab) lrMaintainer() *browser {
	l.t.Helper()
	b := l.signedIn(maintainerID)
	if err := l.s.setMaintainer(maintainerID, true); err != nil {
		l.t.Fatal(err)
	}
	return b
}

// lrMember signs the account in and marks it as M team.
func (l *signInLab) lrMember(id int64) *browser {
	l.t.Helper()
	b := l.signedIn(id)
	l.lrMark(id)
	return b
}

const lrKnownGood = "7.1.12-known-good"

// lrMac enrolls a Lab-owned Mac of this board that has passed its baseline.
func (l *signInLab) lrMac(name, board string, change func(*api.Device)) *dev {
	l.t.Helper()
	d := api.Device{ID: "d-" + name, Name: name, Model: api.ModelLaptop, State: api.StateReady, KnownGood: lrKnownGood, Kernel: lrKnownGood,
		LastSeen: l.s.now(), Facts: api.Facts{PreflightOK: true}}
	if board != "" {
		d.Facts.Compatible = []string{"apple," + board, "apple,arm-platform"}
	}
	d.Kind = api.KindLabOwned
	if change != nil {
		change(&d)
	}
	if err := l.s.store.saveDevice(&d, hashSecret("dev_"+name)); err != nil {
		l.t.Fatal(err)
	}
	v := newDev(l.s, d, hashSecret("dev_"+name))
	l.s.mu.Lock()
	l.s.devs[name] = v
	l.s.mu.Unlock()
	return v
}

// lrVolunteers makes the Mac a Volunteer's, open until then at this level.
func lrVolunteers(level string, openUntil time.Time) func(*api.Device) {
	return func(d *api.Device) {
		d.Kind, d.Volunteer, d.VolunteerName, d.AccessLevel = api.KindVolunteer, true, "a volunteer", level
		if !openUntil.IsZero() {
			d.Window = &window.Settings{OpenUntil: openUntil}
		}
	}
}

// lease asks for a Lease as `lab lease` does; ttl 0 asks for no length.
func (b *browser) lease(mac, holder string, ttl time.Duration) (int, api.Lease, string) {
	b.l.t.Helper()
	body, _ := json.Marshal(api.LeaseRequest{Holder: holder, TTLSec: int(ttl.Seconds())})
	w := b.write("POST", "/api/devices/"+mac+"/lease", string(body), "Content-Type", "application/json")
	var l api.Lease
	json.Unmarshal(w.Body.Bytes(), &l)
	return w.Code, l, w.Body.String()
}

func (b *browser) mustLease(mac, holder string, ttl time.Duration) api.Lease {
	b.l.t.Helper()
	code, l, body := b.lease(mac, holder, ttl)
	if code != http.StatusOK {
		b.l.t.Fatalf("a Lease on %s for %s: %d %s", mac, holder, code, body)
	}
	return l
}

// refusedLease is a Lease the lab must refuse with this status, saying each
// of these.
func (b *browser) refusedLease(mac, holder string, ttl time.Duration, status int, saying ...string) {
	b.l.t.Helper()
	code, _, body := b.lease(mac, holder, ttl)
	if code != status {
		b.l.t.Errorf("a Lease on %s for %s: %d %s, want %d", mac, holder, code, body, status)
	}
	for _, want := range saying {
		if !strings.Contains(body, want) {
			b.l.t.Errorf("the refusal of a Lease on %s lacks %q: %s", mac, want, body)
		}
	}
}

func (b *browser) release(mac, query string) (int, string) {
	b.l.t.Helper()
	w := b.write("DELETE", "/api/devices/"+mac+"/lease"+query, "")
	return w.Code, w.Body.String()
}

func (l *signInLab) leaseOn(mac string) *api.Lease {
	l.t.Helper()
	d := l.s.view(l.s.dev(mac))
	return d.Lease
}

// A Lease is a person's: the account id on the server, the handle in the
// field the agent's commands already carry, the holder label inside it. A
// rename changes none of that.
func TestALeaseBelongsToThePersonWhoTookIt(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	grace := l.lrMember(lrGrace)

	got := grace.mustLease("m1-pro", "grace@laptop", 0)
	if got.Holder != "grace@laptop" || got.Maintainer != "grace" || got.AccountID != lrGrace {
		t.Fatalf("the Lease is not grace's: %+v", got)
	}
	if want := l.s.now().Add(4 * time.Hour); !got.Expires.Equal(want) {
		t.Fatalf("a Lease with no length asked ends %s, want 4 hours on, %s", got.Expires, want)
	}
	stored := l.s.dev("m1-pro").snapshot().Lease
	if stored == nil || stored.AccountID != lrGrace || stored.Maintainer != "grace" || stored.Holder != "grace@laptop" || stored.Warning != "" {
		t.Fatalf("stored: %+v", stored)
	}

	// Another person naming the same holder label is not its holder.
	linus := l.lrMember(lrLinus)
	linus.refusedLease("m1-pro", "grace@laptop", 0, http.StatusConflict, "m1-pro is leased by grace@laptop (grace) until ")
	if code, body := linus.release("m1-pro", "?holder="+url.QueryEscape("grace@laptop")); code != http.StatusConflict {
		t.Fatalf("linus releasing grace's Lease: %d %s", code, body)
	}

	// GitHub renames grace: the Lease is still hers, by account id.
	l.gh.AddUser(lrGrace, "gracehopper")
	renamed := l.signedIn(lrGrace)
	l.clock.Advance(time.Hour)
	again := renamed.mustLease("m1-pro", "grace@laptop", 0)
	if again.AccountID != lrGrace || again.Maintainer != "gracehopper" || !again.Expires.Equal(l.s.now().Add(4*time.Hour)) {
		t.Fatalf("the renewal after a rename: %+v", again)
	}
	if code, body := renamed.release("m1-pro", "?holder="+url.QueryEscape("grace@laptop")); code != http.StatusOK {
		t.Fatalf("grace releasing her Lease: %d %s", code, body)
	}
	if q := l.leaseOn("m1-pro"); q != nil {
		t.Fatalf("the Lease outlived its release: %+v", q)
	}
}

// A Maintainer or an M team member takes a Lease; nobody else does.
func TestOnlyTheMTeamAndMaintainersTakeALease(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)

	if code, _, body := l.browser().lease("m1-pro", "x@y", 0); code != http.StatusUnauthorized {
		t.Errorf("signed out: %d %s", code, body)
	}
	l.signedIn(nobodyID).refusedLease("m1-pro", "x@y", 0, http.StatusForbidden, "no role", "M team")
	// The rule itself refuses whoever is neither, whatever let them in.
	v := l.s.dev("m1-pro")
	if _, err := l.s.takeLease(v, Person{AccountID: nobodyID, Handle: "nobody-yet"}, "x@y", 0); err == nil || statusOf(err) != http.StatusForbidden {
		t.Errorf("the Lease rule for a person with no role: %v", err)
	}
	if q := l.leaseOn("m1-pro"); q != nil {
		t.Fatalf("a refused Lease was taken: %+v", q)
	}

	grace := l.lrMember(lrGrace)
	grace.mustLease("m1-pro", "grace@laptop", 0)
	if code, body := grace.release("m1-pro", ""); code != http.StatusOK {
		t.Fatalf("release: %d %s", code, body)
	}
	ravi := l.lrMaintainer()
	if got := ravi.mustLease("m1-pro", "ravi@mini", 0); got.Maintainer != "ravi" || got.AccountID != maintainerID {
		t.Fatalf("a Maintainer's Lease: %+v", got)
	}
	ravi.release("m1-pro", "")
	if got := l.signedIn(ownerID).mustLease("m1-pro", "owner@mini", 0); got.Maintainer != "owner" {
		t.Fatalf("an Admin's Lease: %+v", got)
	}

	// A Lab token takes a Lease as its person, as lab does.
	l.lrMac("m2-mini", "j473", nil)
	token := grace.makeToken("laptop", "90")
	body, _ := json.Marshal(api.LeaseRequest{Holder: "grace@laptop"})
	byToken := func(token string) *httptest.ResponseRecorder {
		return l.browser().do("POST", "/api/devices/m2-mini/lease", string(body), "Authorization", "Bearer "+token, "Content-Type", "application/json")
	}
	if w := byToken(token); w.Code != http.StatusOK {
		t.Fatalf("a Lease by an M team member's Lab token: %d %s", w.Code, w.Body)
	}
	if q := l.leaseOn("m2-mini"); q == nil || q.AccountID != lrGrace || q.Maintainer != "grace" {
		t.Fatalf("the Lease a Lab token took: %+v", q)
	}
	if w := byToken(l.signedIn(nobodyID).makeToken("mine", "90")); w.Code != http.StatusForbidden {
		t.Errorf("a Lease by the Lab token of a person with no role: %d %s", w.Code, w.Body)
	}
	if w := byToken("lab_0000000000000000000000000000000000000000"); w.Code != http.StatusUnauthorized {
		t.Errorf("a Lease by no Lab token: %d %s", w.Code, w.Body)
	}

	// The mark is read on every request.
	l.lrUnmark(lrGrace)
	grace.refusedLease("m1-pro", "grace@laptop", 0, http.StatusForbidden, "M team")
	if w := byToken(token); w.Code != http.StatusForbidden {
		t.Errorf("a Lab token after its person lost the mark: %d %s", w.Code, w.Body)
	}

	// A Blocked person takes none, and meets the one sentence.
	l.lrMark(lrGrace)
	if err := l.s.store.block(lrGrace, ownerID, "", l.s.now()); err != nil {
		t.Fatal(err)
	}
	if w := byToken(token); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), blockedSentence) {
		t.Errorf("a Blocked person's Lab token: %d %s", w.Code, w.Body)
	}
	if _, err := l.s.takeLease(v, Person{AccountID: lrGrace, Handle: "grace", MTeam: true, Blocked: true}, "x@y", 0); err == nil || statusOf(err) != http.StatusForbidden {
		t.Errorf("the Lease rule for a Blocked person: %v", err)
	}
}

// Only a Mac that has passed its baseline and has not left takes a Lease.
func TestOnlyAMacThatPassedItsBaselineAndHasNotLeftIsLeased(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("new-mac", "j316s", func(d *api.Device) { d.KnownGood = "" })
	l.lrMac("left-mac", "j274", func(d *api.Device) { d.State = api.StateLeft })
	l.lrMac("m1-pro", "j473", nil)
	for _, b := range []*browser{l.lrMember(lrGrace), l.lrMaintainer()} {
		b.refusedLease("new-mac", "x@y", 0, http.StatusConflict, "new-mac has not passed its baseline")
		b.refusedLease("left-mac", "x@y", 0, http.StatusConflict, "left-mac has left the lab")
		if code, _, body := b.lease("no-such-mac", "x@y", 0); code != http.StatusNotFound {
			t.Errorf("a Mac that is not there: %d %s", code, body)
		}
	}
}

// A Volunteer's Mac is leased only inside its Availability window, at
// either Access level, and the Lease is cut to the window's end.
func TestAVolunteersMacIsLeasedInsideItsWindowAtEitherAccessLevel(t *testing.T) {
	l := leaseLab(t)
	end := l.s.now().Add(90 * time.Minute)
	l.lrMac("vol-qual", "j274", lrVolunteers(api.AccessQualification, end))
	l.lrMac("vol-full", "j293", lrVolunteers(api.AccessFull, end))
	l.lrMac("vol-long", "j473", lrVolunteers(api.AccessQualification, l.s.now().Add(9*time.Hour)))
	l.lrMac("vol-closed", "j493", lrVolunteers(api.AccessFull, time.Time{}))
	grace := l.lrMember(lrGrace)

	for _, mac := range []string{"vol-qual", "vol-full"} {
		if got := grace.mustLease(mac, "grace@laptop", 0); !got.Expires.Equal(end) {
			t.Errorf("%s: the Lease ends %s, want the window's end %s", mac, got.Expires, end)
		}
	}
	if got := grace.mustLease("vol-long", "grace@laptop", 0); !got.Expires.Equal(l.s.now().Add(4 * time.Hour)) {
		t.Errorf("a window longer than the Lease: it ends %s", got.Expires)
	}
	grace.refusedLease("vol-closed", "grace@laptop", 0, http.StatusConflict, "vol-closed's Availability window is closed")
	l.lrMaintainer().refusedLease("vol-closed", "ravi@mini", 0, http.StatusConflict, "Availability window is closed")
}

// 4 hours by default for everyone. A Maintainer may ask for up to 24; an M
// team member is refused above 4. A renewal sets the end to 4 hours from now.
func TestALeaseLastsFourHoursAndAMaintainersUpToTwentyFour(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-mini", "j473", nil)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()

	grace.refusedLease("m1-pro", "grace@laptop", 4*time.Hour+time.Second, http.StatusBadRequest, "4 hours")
	grace.refusedLease("m1-pro", "grace@laptop", 24*time.Hour, http.StatusBadRequest, "4 hours")
	if l.leaseOn("m1-pro") != nil {
		t.Fatal("a refused length took the Lease")
	}
	if got := grace.mustLease("m1-pro", "grace@laptop", time.Hour); !got.Expires.Equal(l.s.now().Add(time.Hour)) {
		t.Errorf("a shorter Lease: %s", got.Expires)
	}
	if got := grace.mustLease("m1-pro", "grace@laptop", 4*time.Hour); !got.Expires.Equal(l.s.now().Add(4 * time.Hour)) {
		t.Errorf("a Lease of 4 hours: %s", got.Expires)
	}

	ravi.refusedLease("m2-mini", "ravi@mini", 24*time.Hour+time.Second, http.StatusBadRequest, "24 hours")
	if got := ravi.mustLease("m2-mini", "ravi@mini", 0); !got.Expires.Equal(l.s.now().Add(4 * time.Hour)) {
		t.Errorf("a Maintainer's Lease with no length asked: %s", got.Expires)
	}
	if got := ravi.mustLease("m2-mini", "ravi@mini", 24*time.Hour); !got.Expires.Equal(l.s.now().Add(24 * time.Hour)) {
		t.Errorf("a Maintainer's Lease of 24 hours: %s", got.Expires)
	}

	// Renewal: at any time, and the end becomes 4 hours from now.
	l.clock.Advance(3*time.Hour + 59*time.Minute)
	for b, mac := range map[*browser]string{grace: "m1-pro", ravi: "m2-mini"} {
		holder := l.leaseOn(mac).Holder
		for range 3 {
			l.clock.Advance(10 * time.Second)
			if got := b.mustLease(mac, holder, 0); !got.Expires.Equal(l.s.now().Add(4 * time.Hour)) {
				t.Errorf("%s renewed: it ends %s, want 4 hours from now, %s", mac, got.Expires, l.s.now().Add(4*time.Hour))
			}
		}
	}
}

// One Lease per Configuration for an M team member, counted per person
// whatever the holder label: a second is refused, naming the one held. A
// Maintainer is warned and goes on.
func TestASecondLeaseOnAConfigurationIsRefusedForTheMTeam(t *testing.T) {
	l := leaseLab(t)
	for _, name := range []string{"pro-a", "pro-b", "pro-c", "pro-d"} {
		l.lrMac(name, "j316s", nil)
	}
	l.lrMac("mini", "j473", nil)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()

	grace.mustLease("pro-a", "grace@laptop", 0)
	grace.refusedLease("pro-b", "grace@laptop", 0, http.StatusConflict, "pro-a", "j316s, 10 CPU and 16 GPU cores", "one Lease per Configuration")
	grace.refusedLease("pro-b", "grace@another-agent", 0, http.StatusConflict, "pro-a")
	if got := grace.mustLease("mini", "grace@laptop", 0); got.Warning != "" {
		t.Errorf("a Lease on another Configuration warns: %q", got.Warning)
	}
	// Renewing the one held is no second Lease.
	grace.mustLease("pro-a", "grace@laptop", 0)

	if got := ravi.mustLease("pro-c", "ravi@mini", 0); got.Warning != "" {
		t.Errorf("a Maintainer's first Lease on a Configuration warns: %q", got.Warning)
	}
	got := ravi.mustLease("pro-d", "ravi@mini", 0)
	if !strings.Contains(got.Warning, "pro-c") || !strings.Contains(got.Warning, "j316s, 10 CPU and 16 GPU cores") {
		t.Errorf("a Maintainer's second Lease on a Configuration: warning %q", got.Warning)
	}
	if q := l.leaseOn("pro-d"); q == nil || q.Maintainer != "ravi" || q.Warning != "" {
		t.Fatalf("the warned Maintainer did not go on: %+v", q)
	}

	// Once the first is released, or has run out, the second is taken.
	if code, body := grace.release("pro-a", ""); code != http.StatusOK {
		t.Fatalf("release: %d %s", code, body)
	}
	grace.mustLease("pro-b", "grace@laptop", 0)
	l.clock.Advance(4*time.Hour + time.Second)
	grace.mustLease("pro-a", "grace@laptop", 0)
}

// lrKernelLog leaves the kernel log of a job the Mac ran on this kernel.
func (l *signInLab) lrKernelLog(mac, id, kernel string, at time.Time, log string) {
	l.t.Helper()
	v := l.s.dev(mac)
	j := &api.Job{ID: id, Spec: api.JobSpec{Device: mac}, DeviceID: v.snapshot().ID, State: api.JobDone, Outcome: api.OutcomePass, Created: at, Updated: at}
	j.Result.Booted, j.Result.BootKernel = true, kernel
	if err := l.s.store.saveJob(j); err != nil {
		l.t.Fatal(err)
	}
	dir := filepath.Join(l.s.jobDir(id), "boot0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kernel.txt"), []byte(log), 0o644); err != nil {
		l.t.Fatal(err)
	}
	l.s.forgetCoreCounts(v)
}

func lrLog(cpus, gpu string) string {
	log := "[    0.000000] Linux version 7.1\n"
	if cpus != "" {
		log += "[    0.412345] smp: Brought up 1 node, " + cpus + " CPUs\n"
	}
	if gpu != "" {
		log += "[    3.100000] asahi 406400000.gpu:   Active cores: " + gpu + "\n"
	}
	return log
}

// The Configuration comes from the table for a board with one, else from
// the kernel log of the Mac's latest job on its Known-good kernel. Unknown
// counts on one board count as the same Configuration, until the lab reads
// them.
func TestTheConfigurationComesFromTheTableElseTheKernelLog(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("pro-16", "j316s", nil)
	l.lrMac("max-a", "j414c", nil)
	l.lrMac("max-b", "j414c", nil)
	l.lrMac("max-c", "j414c", nil)
	l.lrMac("no-board", "", nil)
	t0 := l.s.now().Add(-48 * time.Hour)

	conf := func(mac string) string { return l.s.configuration(l.s.dev(mac)).String() }
	if got := conf("pro-16"); got != "j316s, 10 CPU and 16 GPU cores" {
		t.Errorf("a board with one Configuration, from the table: %q", got)
	}
	if got := conf("max-a"); got != "j414c, core counts unknown" {
		t.Errorf("a board with two and no kernel log: %q", got)
	}
	if got := conf("no-board"); got != "board unknown, core counts unknown" {
		t.Errorf("a Mac whose check-in named no board: %q", got)
	}

	// The latest job on the Known-good kernel decides; a job on a test
	// kernel, however late, does not.
	l.lrKernelLog("max-a", "j-old", lrKnownGood, t0, lrLog("12", "38"))
	l.lrKernelLog("max-a", "j-new", lrKnownGood, t0.Add(2*time.Hour), lrLog("12", "30"))
	l.lrKernelLog("max-a", "j-test-kernel", "7.2.0-rc1-test", t0.Add(3*time.Hour), lrLog("4", "2"))
	if got := conf("max-a"); got != "j414c, 12 CPU and 30 GPU cores" {
		t.Errorf("from the kernel log of the latest job on the Known-good kernel: %q", got)
	}
	// A log without the GPU's line leaves that count unknown.
	l.lrKernelLog("max-b", "j-b", lrKnownGood, t0, lrLog("12", ""))
	if got := conf("max-b"); got != "j414c, 12 CPU cores, GPU cores unknown" {
		t.Errorf("a kernel log with no GPU line: %q", got)
	}
	// What labd read stays with the Mac across a restart of labd.
	if c := l.s.dev("max-a").snapshot().Configuration; c == nil || c.CPU != 12 || c.GPU != 30 {
		t.Errorf("the counts labd read are not kept with the Mac: %+v", c)
	}
	// The view carries the Configuration as labd works it out.
	ravi := l.lrMaintainer()
	var view DeviceView
	if w := ravi.do("GET", "/api/devices/pro-16", ""); json.Unmarshal(w.Body.Bytes(), &view) != nil || view.Configuration == nil || *view.Configuration != (api.Configuration{Board: "j316s", CPU: 10, GPU: 16}) {
		t.Errorf("the view of pro-16: %s", w.Body)
	}

	// max-b and max-c have counts labd does not know: with max-a they are
	// one Configuration for an M team member, and the refusal says why.
	grace := l.lrMember(lrGrace)
	grace.mustLease("max-a", "grace@laptop", 0)
	grace.refusedLease("max-b", "grace@laptop", 0, http.StatusConflict, "max-a", "GPU cores unknown", "until the lab reads")
	grace.refusedLease("max-c", "grace@laptop", 0, http.StatusConflict, "max-a", "core counts unknown", "until the lab reads")

	// A later log gives the count an earlier one lacked, and a log without
	// the GPU's line keeps the count an earlier one gave.
	l.lrKernelLog("max-b", "j-b2", lrKnownGood, t0.Add(time.Hour), lrLog("12", "38"))
	if got := conf("max-b"); got != "j414c, 12 CPU and 38 GPU cores" {
		t.Errorf("a later log with the GPU line: %q", got)
	}
	l.lrKernelLog("max-b", "j-b3", lrKnownGood, t0.Add(2*time.Hour), lrLog("12", ""))
	if got := conf("max-b"); got != "j414c, 12 CPU and 38 GPU cores" {
		t.Errorf("a still later log without the GPU line: %q", got)
	}
	grace.mustLease("max-b", "grace@laptop", 0)
	// A Mac whose latest log lacks the GPU's line takes it from an older log,
	// though nothing read that older log before.
	l.lrMac("max-d", "j414c", nil)
	l.lrKernelLog("max-d", "j-d1", lrKnownGood, t0, lrLog("12", "38"))
	l.lrKernelLog("max-d", "j-d2", lrKnownGood, t0.Add(time.Hour), lrLog("12", ""))
	if got := conf("max-d"); got != "j414c, 12 CPU and 38 GPU cores" {
		t.Errorf("the GPU count from an older log: %q", got)
	}
	// A GPU count no Known-good log has comes from a test kernel's log; its
	// CPU count never does.
	l.lrMac("max-e", "j414c", nil)
	l.lrKernelLog("max-e", "j-e1", lrKnownGood, t0, lrLog("12", ""))
	l.lrKernelLog("max-e", "j-e2", "7.2.0-rc1-test", t0.Add(time.Hour), lrLog("4", "38"))
	if got := conf("max-e"); got != "j414c, 12 CPU and 38 GPU cores" {
		t.Errorf("the GPU count from a test kernel's log: %q", got)
	}
}

// A leased Mac refuses a Maintainer's jobs, naming the holder and the end
// time; labd's scheduled refresh skips it.
func TestALeasedMacRefusesAMaintainersJobs(t *testing.T) {
	l := leaseLab(t)
	v := l.lrMac("m1-pro", "j316s", nil)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()
	lease := grace.mustLease("m1-pro", "grace@laptop", 0)

	spec, _ := json.Marshal(api.JobSpec{Device: "m1-pro", Holder: "ravi@mini", Baseline: true})
	w := ravi.write("POST", "/api/jobs", string(spec), "Content-Type", "application/json")
	want := "m1-pro is leased by grace@laptop (grace) until " + lease.Expires.Format(time.TimeOnly)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), want) {
		t.Fatalf("a Maintainer's job on a leased Mac: %d %s, want 409 saying %q", w.Code, w.Body, want)
	}
	// Naming the holder's label does not make it the Maintainer's.
	spec, _ = json.Marshal(api.JobSpec{Device: "m1-pro", Holder: "grace@laptop", Baseline: true})
	if w := ravi.write("POST", "/api/jobs", string(spec), "Content-Type", "application/json"); w.Code != http.StatusConflict {
		t.Fatalf("a Maintainer's job under the holder's label: %d %s", w.Code, w.Body)
	}
	if !l.s.leased(v) {
		t.Fatal("labd's scheduled refresh would not skip the leased Mac")
	}
	l.clock.Advance(4*time.Hour + time.Second)
	if l.s.leased(v) {
		t.Fatal("a Lease that ran out still keeps the scheduled refresh away")
	}
}

// Only its holder releases a Lease; a Maintainer may end another person's,
// an M team member may not.
func TestOnlyItsHolderReleasesALease(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	grace, linus, ravi := l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()
	grace.mustLease("m1-pro", "grace@laptop", 0)

	if code, body := linus.release("m1-pro", ""); code != http.StatusConflict || !strings.Contains(body, "grace") {
		t.Errorf("another member releasing it: %d %s", code, body)
	}
	if code, body := linus.release("m1-pro", "?force=1"); code != http.StatusForbidden || !strings.Contains(body, "only a Maintainer ends another person's Lease") {
		t.Errorf("another member forcing it: %d %s", code, body)
	}
	if code, body := grace.release("m1-pro", "?holder="+url.QueryEscape("grace@another-agent")); code != http.StatusConflict {
		t.Errorf("released under another holder label: %d %s", code, body)
	}
	if l.leaseOn("m1-pro") == nil {
		t.Fatal("a refused release ended the Lease")
	}
	if code, body := ravi.release("m1-pro", ""); code != http.StatusConflict || !strings.Contains(body, "--force") {
		t.Errorf("a Maintainer releasing it without force: %d %s", code, body)
	}
	if code, body := ravi.release("m1-pro", "?force=1"); code != http.StatusOK {
		t.Errorf("a Maintainer ending it: %d %s", code, body)
	}
	if q := l.leaseOn("m1-pro"); q != nil {
		t.Fatalf("the Lease outlived a Maintainer ending it: %+v", q)
	}
}

// The daily summary names each Mac's Configuration and who leases it.
func TestTheDailySummaryCarriesConfigurationsAndLeaseHolders(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-max", "j414c", nil)
	lease := l.lrMember(lrGrace).mustLease("m1-pro", "grace@laptop", 0)
	got := map[string]MacSummary{}
	for _, m := range l.s.summaryMacs() {
		got[m.Name] = m
	}
	if m := got["m1-pro"]; m.Configuration != "j316s, 10 CPU and 16 GPU cores" || m.State != MacLeased("grace", lease.Expires) {
		t.Errorf("m1-pro in the summary: %+v", m)
	}
	if m := got["m2-max"]; m.Configuration != "j414c, core counts unknown" || m.State != MacFree {
		t.Errorf("m2-max in the summary: %+v", m)
	}
}

// lrMark and lrUnmark set and take the M team's mark.
func (l *signInLab) lrMark(id int64)   { l.lrSetMark(id, true) }
func (l *signInLab) lrUnmark(id int64) { l.lrSetMark(id, false) }

func (l *signInLab) lrSetMark(id int64, on bool) {
	l.t.Helper()
	if err := l.s.store.setMark(id, "m_team", on); err != nil {
		l.t.Fatal(err)
	}
}

// lrLend makes the account the Mac's Volunteer.
func (l *signInLab) lrLend(mac string, id int64) {
	v := l.s.dev(mac)
	v.mu.Lock()
	v.d.VolunteerID = id
	v.mu.Unlock()
}

// passedBaseline has the lab's Mac count as one that passed its baseline,
// for a test that leases it and runs no job: only such a Mac takes a Lease.
func (l *lab) passedBaseline() {
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.KnownGood = agent.SimKnownGood
	v.mu.Unlock()
}
