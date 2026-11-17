package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// joinLab is a lab that counts: every request a Mac sends it, in order.
type joinLab struct {
	srv   *httptest.Server
	terms *api.JoinTerms
	kind  string
	// refuse answers the Join command's check with a refusal, when set.
	refuse func() (int, string)

	mu     sync.Mutex
	seen   []string          // "METHOD path", as they came
	bodies map[string][]byte // the last body sent to a path
	auth   map[string]string // the last Authorization sent to a path
}

func newJoinLab(t *testing.T) *joinLab {
	t.Helper()
	l := &joinLab{terms: testTerms("Only a Test machine is lent.\n"), kind: api.KindVolunteer, bodies: map[string][]byte{}, auth: map[string]string{}}
	l.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		l.mu.Lock()
		l.seen = append(l.seen, r.Method+" "+r.URL.Path)
		l.bodies[r.URL.Path], l.auth[r.URL.Path] = body, r.Header.Get("Authorization")
		n := len(l.seen)
		l.mu.Unlock()
		answer := func(v any) { json.NewEncoder(w).Encode(v) }
		switch r.Method + " " + r.URL.Path {
		case "GET /api/join/terms":
			answer(l.terms)
		case "POST /api/join/check":
			if l.refuse != nil {
				code, why := l.refuse()
				http.Error(w, why, code)
				return
			}
			answer(api.JoinCheck{Name: "shelf-mac", Kind: l.kind, TermsSHA256: l.terms.SHA256})
		case "POST /api/agent/enroll":
			answer(api.EnrollResponse{DeviceID: fmt.Sprintf("d-new%d", n), Name: "shelf-mac", Secret: fmt.Sprintf("dev_new%d", n),
				AgentEnrollment: api.AgentEnrollment{Enrollment: api.Enrollment{Kind: l.kind, VolunteerID: 42, NtfyTopic: "lab-shelf-mac-topic"}}})
		case "POST /api/agent/left":
			answer(struct{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(l.srv.Close)
	return l
}

func (l *joinLab) heard() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.seen)
}

func (l *joinLab) sent(path string, v any) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v != nil {
		json.Unmarshal(l.bodies[path], v)
	}
	return l.auth[path]
}

// joinRun is one run of the Join command's `lab-agent setup` on a simulated
// Mac: what it said, what it asked on the terminal, and how it ended.
type joinRun struct {
	err  error
	said string
	tty  string
}

// join runs setup on sim against lab, the person typing answers on the
// terminal ("" is a terminal that says nothing); edit changes the options.
func (l *joinLab) join(sim *Sim, answers string, edit func(*SetupOptions)) joinRun {
	tty := &fakeTTY{in: strings.NewReader(answers)}
	var said []string
	o := SetupOptions{Server: l.srv.URL, Token: "enr_" + strings.Repeat("ab", 16), Mac: sim, TTY: tty,
		Log: func(f string, a ...any) { said = append(said, fmt.Sprintf(f, a...)) }}
	if edit != nil {
		edit(&o)
	}
	err := Setup(context.Background(), o)
	return joinRun{err: err, said: strings.Join(said, "\n"), tty: tty.out.String()}
}

// A Mac the pre-checks pass: the simulated Mac as it comes.
func goodMac(t *testing.T) *Sim {
	t.Helper()
	return NewSim(t.TempDir())
}

// stopped is the test every pre-check row passes: setup stopped saying want,
// before the terms were shown, before the token was used (no lab heard a
// thing) and before anything on the Mac changed.
func stopped(t *testing.T, sim *Sim, before map[string]string, run joinRun, labs []*joinLab, want ...string) {
	t.Helper()
	if run.err == nil {
		t.Fatalf("setup went on:\n%s", run.said)
	}
	for _, w := range want {
		if !strings.Contains(run.err.Error(), w) {
			t.Errorf("the stop does not say %q:\n%v", w, run.err)
		}
	}
	if strings.Contains(run.tty, "Only a Test machine is lent.") {
		t.Errorf("the terms were shown first:\n%s", run.tty)
	}
	for _, l := range labs {
		if h := l.heard(); len(h) != 0 {
			t.Errorf("a lab heard %q", h)
		}
	}
	if after := snapshot(t, sim.WorkDir); !reflect.DeepEqual(before, after) {
		t.Errorf("the Mac changed:\nbefore %v\nafter  %v", before, after)
	}
	if n := sim.SetUps(); n != 0 {
		t.Errorf("the Mac was set up %d time(s)", n)
	}
}

// stops runs the Join command on a Mac whose facts edit spoils, and checks
// that it stopped as a pre-check does.
func stops(t *testing.T, edit func(*JoinFacts), want ...string) {
	t.Helper()
	lab, sim := newJoinLab(t), goodMac(t)
	sim.SetJoinFacts(edit)
	before := snapshot(t, sim.WorkDir)
	stopped(t, sim, before, lab.join(sim, "yes\n", nil), []*joinLab{lab}, want...)
}

// Row 1 of the pre-check table: an Apple Silicon Mac on Linux, and the
// system is Omarchy.
func TestJoinStopsUnlessTheMacIsAppleSiliconOnLinuxRunningOmarchy(t *testing.T) {
	stops(t, func(f *JoinFacts) { f.OS = "darwin" }, "not Linux", "Omarchy on Apple Silicon Macs")
	stops(t, func(f *JoinFacts) { f.Compatible = []string{"qemu,virt"} }, "not an Apple Silicon Mac")
	stops(t, func(f *JoinFacts) { f.NotOmarchy = "no Omarchy package is installed" }, "not Omarchy", "no Omarchy package is installed")
}

// Row 2: run as yourself, with sudo.
func TestJoinStopsUnlessRunAsYourselfWithSudo(t *testing.T) {
	stops(t, func(f *JoinFacts) { f.EUID = 1000 }, "needs root", "sudo")
	stops(t, func(f *JoinFacts) { f.SudoUID = "" }, "as yourself")
	stops(t, func(f *JoinFacts) { f.SudoUID = "0" }, "as yourself", "not as root")
}

// Row 3: already in this lab. The Mac is named, and My Macs pointed to.
func TestJoinStopsOnAMacAlreadyInThisLab(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	in := &Config{Server: lab.srv.URL + "/", DeviceID: "d-1", Name: "desk-mac", Secret: "dev_s", Enrollment: api.AgentEnrollment{Enrollment: api.Enrollment{Kind: api.KindVolunteer}}}
	if err := in.Save(filepath.Join(sim.WorkDir, "agent.json")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, sim.WorkDir)
	stopped(t, sim, before, lab.join(sim, "yes\n", nil), []*joinLab{lab}, "already in this lab", "desk-mac", "My Macs", lab.srv.URL+"/my-macs")
}

// otherLabMac is a Mac enrolled in another lab, as kind.
func otherLabMac(t *testing.T, other *joinLab, kind string) *Sim {
	t.Helper()
	sim := goodMac(t)
	there := &Config{Server: other.srv.URL, DeviceID: "d-old", Name: "old-name", Secret: "dev_old", HTTPTimeoutSec: 77, GUIUser: "pat",
		Enrollment: api.AgentEnrollment{Enrollment: api.Enrollment{Kind: kind}}}
	if err := there.Save(filepath.Join(sim.WorkDir, "agent.json")); err != nil {
		t.Fatal(err)
	}
	return sim
}

// Row 4: in another lab. That lab is named and the person asked once; on
// yes the Mac leaves it properly after the terms are accepted and before it
// enrolls. It arrives with a new device id and secret, and nothing of the
// other enrollment.
func TestJoinFromAnotherLabNamesItAsksOnceAndLeavesItAfterTheTerms(t *testing.T) {
	lab, other := newJoinLab(t), newJoinLab(t)
	sim := otherLabMac(t, other, api.KindVolunteer)
	os.WriteFile(filepath.Join(sim.WorkDir, auditFile), []byte(`{"maintainer":"someone there"}`+"\n"), 0o600)

	run := lab.join(sim, "yes\nyes\n\n", nil)
	if run.err != nil {
		t.Fatalf("setup: %v\n%s", run.err, run.said)
	}
	if n := strings.Count(run.tty, other.srv.URL); n < 1 || strings.Count(run.tty, "Leave it") != 1 {
		t.Errorf("the other lab is not named and asked about once:\n%s", run.tty)
	}
	if asked, terms := strings.Index(run.tty, "Leave it"), strings.Index(run.tty, "Only a Test machine is lent."); asked < 0 || terms < asked {
		t.Errorf("the question does not come before the terms:\n%s", run.tty)
	}
	if got, want := lab.heard(), []string{"GET /api/join/terms", "POST /api/join/check", "POST /api/agent/enroll"}; !slices.Equal(got, want) {
		t.Errorf("the lab heard %q, want %q", got, want)
	}
	if got := other.heard(); !slices.Equal(got, []string{"POST /api/agent/left"}) {
		t.Errorf("the other lab heard %q", got)
	}
	if auth := other.sent("/api/agent/left", nil); auth != "Bearer dev_old" {
		t.Errorf("the other lab was told with %q", auth)
	}
	if sim.LeavingStagedEntries() != 0 || sim.Left() != 1 {
		t.Errorf("the other lab was removed from the Mac %d time(s)", sim.Left())
	}
	cfg, err := LoadConfig(filepath.Join(sim.WorkDir, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server != lab.srv.URL || cfg.DeviceID == "d-old" || cfg.Secret == "dev_old" || cfg.Name != "shelf-mac" || cfg.HTTPTimeoutSec != 0 || cfg.GUIUser != "" {
		t.Errorf("the Mac kept something of the other lab: %+v", cfg)
	}
	var enrolled map[string]any
	lab.sent("/api/agent/enroll", &enrolled)
	if b, _ := json.Marshal(enrolled); strings.Contains(string(b), "d-old") || strings.Contains(string(b), "old-name") || strings.Contains(string(b), "dev_old") {
		t.Errorf("the enrollment carries the other lab's record: %s", b)
	}
	// No history follows: the other lab's Audit log is not this lab's to copy.
	if entries, err := readAudit(sim.WorkDir, 0, 10); err != nil || len(entries) != 0 {
		t.Errorf("the other lab's Audit log would be sent to this lab: %v %v", entries, err)
	}
}

func TestJoinFromAnotherLabDeclinedOrLabOwnedThereChangesNothing(t *testing.T) {
	// The person says no: nothing is sent, to either lab.
	lab, other := newJoinLab(t), newJoinLab(t)
	sim := otherLabMac(t, other, api.KindVolunteer)
	before := snapshot(t, sim.WorkDir)
	stopped(t, sim, before, lab.join(sim, "no\nyes\n", nil), []*joinLab{lab, other}, "still in the lab at "+other.srv.URL)

	// Lab-owned there: it cannot leave by itself.
	sim = otherLabMac(t, other, api.KindLabOwned)
	before = snapshot(t, sim.WorkDir)
	run := lab.join(sim, "yes\nyes\n", nil)
	stopped(t, sim, before, run, []*joinLab{lab, other}, "Lab-owned", other.srv.URL, "old-name", "Maintainer")
	if strings.Contains(run.tty, "Leave it") {
		t.Errorf("a Lab-owned Mac was offered to leave:\n%s", run.tty)
	}

	// No terminal to ask on.
	sim = otherLabMac(t, other, api.KindVolunteer)
	before = snapshot(t, sim.WorkDir)
	stopped(t, sim, before, lab.join(sim, "", func(o *SetupOptions) { o.TTY, o.AcceptTerms = nil, lab.terms.SHA256 }), []*joinLab{lab, other}, "terminal", "lab-agent leave")
}

// A lab that cannot be told is not left: the Mac stays in it, whole, and
// does not enroll here.
func TestJoinDoesNotLeaveALabItCannotTell(t *testing.T) {
	lab, other := newJoinLab(t), newJoinLab(t)
	sim := otherLabMac(t, other, api.KindVolunteer)
	other.srv.Close()
	run := lab.join(sim, "yes\nyes\n", nil)
	if run.err == nil || !strings.Contains(run.err.Error(), "still in") {
		t.Fatalf("setup: %v", run.err)
	}
	if slices.Contains(lab.heard(), "POST /api/agent/enroll") {
		t.Errorf("the Mac enrolled here while still there: %q", lab.heard())
	}
	if cfg, err := LoadConfig(filepath.Join(sim.WorkDir, "agent.json")); err != nil || cfg.DeviceID != "d-old" || sim.Left() != 0 {
		t.Errorf("the Mac no longer holds its enrollment there: %+v %v", cfg, err)
	}
}

// A Mac that left a lab which could not be reached then, and now joins
// another: the lab it left hears of it before the record of it goes.
func TestJoinTellsALabTheMacLeftUnheard(t *testing.T) {
	lab, other, sim := newJoinLab(t), newJoinLab(t), goodMac(t)
	if err := writeLeft(sim.WorkDir, &left{Server: other.srv.URL, DeviceID: "d-old", Name: "old-name", Secret: "dev_old"}); err != nil {
		t.Fatal(err)
	}
	if run := lab.join(sim, "yes\n\n", nil); run.err != nil {
		t.Fatal(run.err)
	}
	if got := other.heard(); !slices.Equal(got, []string{"POST /api/agent/left"}) || other.sent("/api/agent/left", nil) != "Bearer dev_old" {
		t.Errorf("the lab the Mac had left heard %q", got)
	}
	if HasLeft(sim.WorkDir) {
		t.Error("the Mac is in the lab and still on record as having left")
	}
}

// Row 5: running a lab kernel.
func TestJoinStopsOnALabKernel(t *testing.T) {
	stops(t, func(f *JoinFacts) { f.Cmdline = "root=UUID=x rw maclab.job=j-123" }, "lab kernel", "j-123", "your own kernel")
}

// Row 6: the disk waits for a passphrase. Two ways out are named.
func TestJoinStopsOnADiskThatWaitsForAPassphrase(t *testing.T) {
	stops(t, func(f *JoinFacts) { f.Passphrase = "/ is on dm-crypt: root unlocks with a passphrase" },
		"waits for a passphrase", "root unlocks with a passphrase", "reinstall", "without encryption", "lab-only-autounlock", "whoever holds the Mac can then read that disk")
}

// Row 7: the bootloader cannot restart unattended, tools missing, under
// 300 MB. Each is named, in one stop.
func TestJoinStopsNamingEachThingTheLabNeedsToRestartTheMac(t *testing.T) {
	stops(t, func(f *JoinFacts) {
		f.Bootloader = []string{"Limine config enrollment is on"}
		f.Missing = []string{"missing objcopy", "missing chattr"}
		f.Space = "less than 300 MB free where test kernels are staged (/boot/maclab)"
	}, "Limine config enrollment is on", "missing objcopy", "missing chattr", "less than 300 MB free")
}

// Every install of an agent refuses an older one: a Join command whose
// lab-agent is older than the one on the Mac stops before anything changes.
func TestJoinStopsWhenTheInstalledAgentIsNewer(t *testing.T) {
	stops(t, func(f *JoinFacts) { f.Installed = "99.1.0" }, "lab-agent 99.1.0 is installed", "older", Version)
	lab, sim := newJoinLab(t), goodMac(t)
	sim.SetJoinFacts(func(f *JoinFacts) { f.Installed = Version })
	if run := lab.join(sim, "yes\n\n", nil); run.err != nil {
		t.Errorf("the same lab-agent again: %v", run.err)
	}
}

// Rows 8 and 9 only warn: the Mac joins.
func TestJoinWarnsAndGoesOnWhenTheSystemCannotBeSnapshottedOrIsOnBattery(t *testing.T) {
	for want, edit := range map[string]func(*JoinFacts){
		"no Installed runs on this Mac": func(f *JoinFacts) { f.Snapshot = fmt.Errorf("/ is ext4, not btrfs") },
		"jobs start only on AC power":   func(f *JoinFacts) { f.OnBattery = true },
	} {
		lab, sim := newJoinLab(t), goodMac(t)
		sim.SetJoinFacts(edit)
		run := lab.join(sim, "yes\n\n", nil)
		if run.err != nil {
			t.Fatalf("%s: setup stopped: %v", want, run.err)
		}
		warned, terms := strings.Index(run.said, want), strings.Index(run.said, "accepted")
		if warned < 0 || terms < warned {
			t.Errorf("no warning %q before the terms:\n%s", want, run.said)
		}
		if sim.SetUps() != 1 {
			t.Errorf("%s: the Mac was not set up", want)
		}
	}
}

// A declined terms prompt posts nothing: the lab handed over its text, as it
// hands it to anyone, and heard nothing of the Mac or of the answer.
func TestADeclinedTermsPromptSendsTheLabNothing(t *testing.T) {
	for _, answer := range []string{"no\n", "\n", "y\n", ""} {
		lab, sim := newJoinLab(t), goodMac(t)
		before := snapshot(t, sim.WorkDir)
		run := lab.join(sim, answer, nil)
		if run.err == nil || !strings.Contains(run.err.Error(), "not accepted") {
			t.Fatalf("answer %q: %v", answer, run.err)
		}
		if got := lab.heard(); !slices.Equal(got, []string{"GET /api/join/terms"}) {
			t.Errorf("answer %q: the lab heard %q", answer, got)
		}
		if b := lab.bodies["/api/join/terms"]; len(b) != 0 {
			t.Errorf("answer %q: the read of the terms carried %q", answer, b)
		}
		if after := snapshot(t, sim.WorkDir); !reflect.DeepEqual(before, after) || sim.SetUps() != 0 {
			t.Errorf("answer %q: the Mac changed", answer)
		}
	}
}

// The typed yes is what the lab records: the enrollment carries the sha256
// of the very text shown, with the key the check bound. --accept-terms keeps
// its meaning: that sha256, given beforehand, for a run with no terminal.
func TestTheEnrollmentCarriesTheTermsThePersonAccepted(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	run := lab.join(sim, "yes\n\n", nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if !strings.Contains(run.tty, "Only a Test machine is lent.") || !strings.Contains(run.tty, lab.terms.SHA256) || !strings.Contains(run.tty, "Type yes") {
		t.Errorf("the terms were not shown with their sha256 and the question:\n%s", run.tty)
	}
	if got, want := lab.heard(), []string{"GET /api/join/terms", "POST /api/join/check", "POST /api/agent/enroll"}; !slices.Equal(got, want) {
		t.Errorf("the lab heard %q, want %q", got, want)
	}
	var check api.JoinCheckRequest
	var enroll api.EnrollRequest
	if auth := lab.sent("/api/join/check", &check); !strings.HasPrefix(auth, "Bearer enr_") {
		t.Errorf("the check carried %q", auth)
	}
	lab.sent("/api/agent/enroll", &enroll)
	if enroll.TermsSHA256 != lab.terms.SHA256 || len(check.ActivationKey) < 32 || enroll.ActivationKey != check.ActivationKey {
		t.Errorf("enrollment %+v after check %+v", enroll, check)
	}

	lab, sim = newJoinLab(t), goodMac(t)
	run = lab.join(sim, "", func(o *SetupOptions) { o.TTY, o.AcceptTerms = nil, lab.terms.SHA256 })
	if run.err != nil || sim.SetUps() != 1 {
		t.Errorf("--accept-terms with the terms' sha256: %v", run.err)
	}
	lab, sim = newJoinLab(t), goodMac(t)
	run = lab.join(sim, "", func(o *SetupOptions) { o.TTY, o.AcceptTerms = nil, strings.Repeat("0", 64) })
	if run.err == nil || len(lab.heard()) != 1 || sim.SetUps() != 0 {
		t.Errorf("--accept-terms with another sha256: %v, the lab heard %q", run.err, lab.heard())
	}
}

// A Volunteer's Mac arrives at Qualification with publishing off, whatever
// it had; a window it had is kept, and then nothing is asked.
func TestAMacArrivesAtQualificationWithPublishingOffAndKeepsItsWindow(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	os.WriteFile(filepath.Join(sim.WorkDir, accessFile), []byte(`{"access_level":"full","publish":true}`), 0o600)
	window := `{"rules":["mon-fri 19:00-07:00"]}`
	os.WriteFile(filepath.Join(sim.WorkDir, windowFile), []byte(window), 0o600)

	run := lab.join(sim, "yes\n", nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if ac, err := loadAccess(sim.WorkDir); err != nil || ac.Level != api.AccessQualification || ac.Publish {
		t.Errorf("the Mac arrived at %+v (%v)", ac, err)
	}
	if b, _ := os.ReadFile(filepath.Join(sim.WorkDir, windowFile)); string(b) != window {
		t.Errorf("the window became %s", b)
	}
	if strings.Contains(run.tty, "When may the lab use this Mac") || !strings.Contains(run.said, "mon-fri 19:00-07:00") {
		t.Errorf("a kept window was asked for again, or not named:\n%s\n%s", run.tty, run.said)
	}
	if !strings.Contains(run.said, "Qualification") || !strings.Contains(run.said, "publishing is off") {
		t.Errorf("setup does not say how the Mac arrived:\n%s", run.said)
	}
	// The topic, as a QR code, at Join.
	if !strings.Contains(run.said, "lab-shelf-mac-topic") || !strings.Contains(run.said, "█") {
		t.Errorf("no ntfy topic with its QR code:\n%s", run.said)
	}
}

// Join asks nothing about the weekly Availability window: the Mac arrives
// closed, and setup's last line says where its Volunteer sets the window.
func TestJoinAsksNoWindowAndPointsToMyMacs(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	run := lab.join(sim, "yes\nmon-fri 19:00-07:00\n", nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	if strings.Contains(run.tty, "When may the lab") || strings.Contains(run.tty, "Availability window") {
		t.Errorf("Join asked for a window:\n%s", run.tty)
	}
	if s, set, err := readWindow(sim.WorkDir); err != nil || set || len(s.Rules) != 0 {
		t.Errorf("the Mac joined with a window: %+v (set %v, %v)", s, set, err)
	}
	want := "Set when the lab may use this Mac on My Macs: " + lab.srv.URL + "/my-macs"
	if lines := strings.Split(strings.TrimSpace(run.said), "\n"); !strings.Contains(lines[len(lines)-1], want) {
		t.Errorf("setup does not end with %q:\n%s", want, run.said)
	}
	// A Lab-owned Mac has no window to point to.
	lab, sim = newJoinLab(t), goodMac(t)
	lab.kind = api.KindLabOwned
	if run := lab.join(sim, "yes\n", nil); run.err != nil || strings.Contains(run.said, "Set when the lab may use") {
		t.Errorf("a Lab-owned Mac: %v\n%s", run.err, run.said)
	}
}

// Blocked, cap reached, name taken, another Mac's token: the lab says so at
// the check, the reason is shown, and the Mac is left untouched.
func TestARefusedJoinShowsTheReasonAndLeavesTheMacUntouched(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	lab.refuse = func() (int, string) { return 409, "Name taken: a Mac called shelf-mac is in the lab already." }
	before := snapshot(t, sim.WorkDir)
	run := lab.join(sim, "yes\n", nil)
	if run.err == nil || !strings.Contains(run.err.Error(), "Name taken: a Mac called shelf-mac is in the lab already.") {
		t.Fatalf("setup: %v", run.err)
	}
	if after := snapshot(t, sim.WorkDir); !reflect.DeepEqual(before, after) || sim.SetUps() != 0 {
		t.Errorf("the Mac changed: %v", after)
	}
	if slices.Contains(lab.heard(), "POST /api/agent/enroll") {
		t.Errorf("the Mac went on to enroll: %q", lab.heard())
	}

	// A Join command that expired while the person read: said as its case.
	lab, sim = newJoinLab(t), goodMac(t)
	lab.refuse = func() (int, string) { return 401, "enrollment token unknown, used or expired" }
	if run := lab.join(sim, "yes\n", nil); run.err == nil || !strings.Contains(run.err.Error(), "expired or was used") || !strings.Contains(run.err.Error(), lab.srv.URL+"/lend") {
		t.Errorf("an expired Join command: %v", run.err)
	}
}

// A Volunteer's Mac refuses --autologin and --stay-awake before it enrolls,
// not after: no record is left in the lab for a Mac that was never set up.
func TestAVolunteersMacRefusesAutologinBeforeItEnrolls(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	run := lab.join(sim, "yes\n", func(o *SetupOptions) { o.GUIUser, o.Autologin = "pat", true })
	if run.err == nil || !strings.Contains(run.err.Error(), "--autologin is refused on a Volunteer's Mac") {
		t.Fatalf("setup: %v", run.err)
	}
	if slices.Contains(lab.heard(), "POST /api/agent/enroll") || sim.SetUps() != 0 {
		t.Errorf("the Mac enrolled first: %q", lab.heard())
	}
}

// Setup interrupted after the Mac enrolled runs again with the same Join
// command: the same activation key, which the lab takes as the same Mac, and
// no "already in this lab".
func TestAnInterruptedJoinRunsAgainWithTheSameKey(t *testing.T) {
	lab, sim := newJoinLab(t), goodMac(t)
	sim.FailSetUp(fmt.Errorf("the power went"))
	if run := lab.join(sim, "yes\n", nil); run.err == nil || !strings.Contains(run.err.Error(), "the power went") {
		t.Fatalf("setup: %v", run.err)
	}
	var first, second api.EnrollRequest
	lab.sent("/api/agent/enroll", &first)
	sim.FailSetUp(nil)
	if run := lab.join(sim, "yes\n\n", nil); run.err != nil {
		t.Fatalf("the second run: %v", run.err)
	}
	lab.sent("/api/agent/enroll", &second)
	if first.ActivationKey == "" || first.ActivationKey != second.ActivationKey {
		t.Errorf("the second run enrolled with another key")
	}
	if exists(filepath.Join(sim.WorkDir, activationFile)) {
		t.Error("the activation key was kept after setup succeeded")
	}
	// Once set up, the same line again is a Mac already in this lab.
	if run := lab.join(sim, "yes\n", nil); run.err == nil || !strings.Contains(run.err.Error(), "already in this lab") {
		t.Errorf("a third run: %v", run.err)
	}
}

// How the Join command tells that a system is Omarchy: by the packages
// pacman's own records list as installed, and for an install made from a
// checkout, by Omarchy's omarchy-version in that checkout. It is
// omarchy-m-test's rule, read from files: nothing is run.
func TestHowASystemIsRecognisedAsOmarchy(t *testing.T) {
	system := func(pkgs []string, files ...string) (root, home string) {
		root = t.TempDir()
		home = filepath.Join(root, "home/pat")
		os.MkdirAll(filepath.Join(root, "var/lib/pacman/local"), 0o755)
		for _, p := range pkgs {
			os.MkdirAll(filepath.Join(root, "var/lib/pacman/local", p), 0o755)
		}
		for _, f := range files {
			os.MkdirAll(filepath.Dir(filepath.Join(root, f)), 0o755)
			os.WriteFile(filepath.Join(root, f), []byte("#!/bin/bash\n"), 0o755)
		}
		return root, home
	}
	for name, c := range map[string]struct {
		pkgs  []string
		files []string
		is    bool
	}{
		"a converged image":             {pkgs: []string{"omarchy-4.0.0.alpha.quattro.r1790420202.g1937418f520b-1.362376005140001", "omarchy-mac-0.1.0-5.362376005140001", "linux-aurora-7.1.12.aurora2-10"}, is: true},
		"omarchy-mac alone":             {pkgs: []string{"omarchy-mac-0.1.0-5"}, is: true},
		"the mx-mac fork":               {pkgs: []string{"omarchy-dev-3.2.0-1"}, is: true},
		"a legacy install, its keyring": {pkgs: []string{"omarchy-mac-keyring-20260101-1"}, is: true},
		"a legacy checkout":             {pkgs: []string{"linux-asahi-6.12-1"}, files: []string{"home/pat/.local/share/omarchy/bin/omarchy-version"}, is: true},
		"Arch Linux ARM with Asahi":     {pkgs: []string{"linux-asahi-6.12-1", "mesa-1:26.2.3-1"}},
		"a package that only sounds so": {pkgs: []string{"omarchy-mac-boot-20260926-1", "omarchy-wallpapers-1-1"}},
		"no pacman at all":              {},
	} {
		root, home := system(c.pkgs, c.files...)
		if name == "no pacman at all" {
			os.RemoveAll(filepath.Join(root, "var"))
		}
		if why := notOmarchy(root, home); (why == "") != c.is {
			t.Errorf("%s: recognised as Omarchy %v (%q), want %v", name, why == "", why, c.is)
		}
	}
}

// The facts of an interrupted setup: a key kept for this very token.
func TestAJoinIsResumedOnlyWithItsOwnToken(t *testing.T) {
	dir := t.TempDir()
	if resuming(dir, "enr_a") {
		t.Error("a Mac with no key kept is resuming")
	}
	activationKey(dir, "enr_a")
	if !resuming(dir, "enr_a") || resuming(dir, "enr_b") || resuming(dir, "") {
		t.Error("the key kept for one token serves another, or not its own")
	}
}
