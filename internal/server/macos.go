package server

// macOS runs: the same benchmarks in macOS, on the same Mac, unattended.
//
// A run is queued for a Mac: its macOS result (queueMacOSSide,
// macos_side.go), or by a Maintainer (queueMacOSRun, or POST
// /api/devices/{name}/macos-runs). lab-agent boot-macos-once tells labd it is
// booting macOS once (POST /api/agent/macos-started), which starts the run's
// deadline. In macOS, lab-macos-runner, a LaunchDaemon enrolled once per Mac
// with a one-time token, reports the macOS build and asks labd at boot for
// its run (GET /api/macos/runner/poll), which is handed only to the boot
// that began after labd started it; it fetches the run's pinned artifacts by
// sha256, runs the list and uploads the results, then reboots back to
// Omarchy. A run not reported by its deadline (a FileVault Mac waits for its
// password at the screen) leaves the Mac Needs hands.
//
// A runner's secret belongs to the Mac's enrollment (Device.ID): it works
// only while that enrollment lasts, and it is revoked when the Mac Leaves or
// is Retired. A Mac starts Attended; the runner says it is Unattended once a
// boot has reached its user's desktop with nobody typing (POST
// /api/macos/runner/attendance), and Attended again when one did not.
//
// The queue and the runners' secrets are kept in macos-runs.json in the data
// directory; the tier's artifacts are the newest Benchmark set's macOS files.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

const macosTokenTTL = time.Hour

// macosDeadline is how long a started run has to report: the boot, the
// benchmarks of its list and the upload. A run past it is needs_hands and
// its Mac Needs hands; results that come later are still taken.
var macosDeadline = func(tier string) time.Duration { return 60 * time.Minute }

// macosWaitForPerson is how long labd waits for a person at an Attended Mac
// once its macOS run is due: past it, the macOS side is dropped.
var macosWaitForPerson = 24 * time.Hour

// macosBuild is what a Mac's runner last reported of its macOS, and for
// which enrollment.
type macosBuild struct {
	Enrollment string    `json:"enrollment"`
	Version    string    `json:"version"`
	Build      string    `json:"build"`
	Said       time.Time `json:"said"`
}

type macosToken struct {
	Mac        string    `json:"mac"`
	Enrollment string    `json:"enrollment"`
	Expires    time.Time `json:"expires"`
}

// macosRunner is an enrolled runner: the Mac and the enrollment its secret
// belongs to, and whether the runner said the Mac is Unattended, and when.
type macosRunner struct {
	Mac        string    `json:"mac"`
	Enrollment string    `json:"enrollment"`
	Unattended bool      `json:"unattended,omitempty"`
	Said       time.Time `json:"said,omitempty"`
}

type macosState struct {
	Tokens   map[string]macosToken    `json:"tokens"`   // hash of a one-time token
	Enrolled map[string]macosRunner   `json:"enrolled"` // hash of a runner's secret
	Runs     map[string]*api.MacOSRun `json:"runs"`     // the Mac's latest run
	Builds   map[string]macosBuild    `json:"builds,omitempty"`
	// Runners are the secrets of before they belonged to an enrollment
	// (hash -> Mac). They are dropped as labd opens the file: such a
	// runner enrolls again.
	Runners map[string]string `json:"runners,omitempty"`
}

type macosRuns struct {
	mu     sync.Mutex
	path   string
	st     macosState
	timers map[string]chan struct{} // closed to stop the Mac's deadline, or its wait for a person
	// dropped is the Performance runs of runs revoke dropped.
	dropped []int64
}

func openMacOSRuns(dir string) *macosRuns {
	m := &macosRuns{path: filepath.Join(dir, "macos-runs.json"), timers: map[string]chan struct{}{}}
	if b, err := os.ReadFile(m.path); err == nil {
		json.Unmarshal(b, &m.st)
	}
	if m.st.Tokens == nil {
		m.st.Tokens = map[string]macosToken{}
	}
	if m.st.Enrolled == nil {
		m.st.Enrolled = map[string]macosRunner{}
	}
	m.st.Runners = nil
	for h, t := range m.st.Tokens {
		if t.Enrollment == "" {
			delete(m.st.Tokens, h)
		}
	}
	if m.st.Runs == nil {
		m.st.Runs = map[string]*api.MacOSRun{}
	}
	if m.st.Builds == nil {
		m.st.Builds = map[string]macosBuild{}
	}
	return m
}

func (m *macosRuns) saveLocked() error {
	b, err := json.MarshalIndent(m.st, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func cloneRun(r *api.MacOSRun) *api.MacOSRun {
	if r == nil {
		return nil
	}
	c := *r
	c.Artifacts = append([]api.MacOSArtifact(nil), r.Artifacts...)
	c.Results = append([]api.BenchResult(nil), r.Results...)
	return &c
}

func (m *macosRuns) run(mac string) *api.MacOSRun {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneRun(m.st.Runs[mac])
}

var errRunWaiting = errors.New("a macOS run waits for this Mac already")

func (m *macosRuns) queue(mac, tier, set string, arts []api.MacOSArtifact, now time.Time) (*api.MacOSRun, error) {
	return m.queueRun(&api.MacOSRun{Mac: mac, Tier: tier, Set: set, Artifacts: arts}, now)
}

// queueRun queues r as the Mac's run, unless one waits already.
func (m *macosRuns) queueRun(r *api.MacOSRun, now time.Time) (*api.MacOSRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if was := m.st.Runs[r.Mac]; was != nil && !api.MacOSEnded(was.State) {
		return nil, fmt.Errorf("%w (%s, %s)", errRunWaiting, was.ID, was.State)
	}
	r = cloneRun(r)
	r.ID, r.State, r.Queued = "mos-"+randHex(6), api.MacOSQueued, now
	m.st.Runs[r.Mac] = r
	return cloneRun(r), m.saveLocked()
}

// sayBuild keeps the macOS version and build the runner of the Mac's
// enrollment reported.
func (m *macosRuns) sayBuild(mac, enrollment, version, build string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if was, ok := m.st.Builds[mac]; ok && was.Enrollment == enrollment && was.Version == version && was.Build == build {
		was.Said = at
		m.st.Builds[mac] = was
		return nil
	}
	m.st.Builds[mac] = macosBuild{Enrollment: enrollment, Version: version, Build: build, Said: at}
	return m.saveLocked()
}

// buildOf is the macOS the runner of the Mac's enrollment last reported.
func (m *macosRuns) buildOf(mac, enrollment string) (macosBuild, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.st.Builds[mac]
	return b, ok && b.Enrollment == enrollment && b.Build != ""
}

func (m *macosRuns) addToken(hash, mac, enrollment string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Tokens[hash] = macosToken{Mac: mac, Enrollment: enrollment, Expires: exp}
	return m.saveLocked()
}

// useToken spends a one-time token and returns the Mac and enrollment it
// was made for.
func (m *macosRuns) useToken(hash string, now time.Time) (macosToken, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.st.Tokens[hash]
	if !ok {
		return t, false
	}
	delete(m.st.Tokens, hash)
	m.saveLocked()
	return t, now.Before(t.Expires)
}

// enrolSecret makes secret the runner secret of the Mac's enrollment,
// ending any earlier one of the Mac's. A runner enrolled again starts
// Attended.
func (m *macosRuns) enrolSecret(mac, enrollment, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, e := range m.st.Enrolled {
		if e.Mac == mac {
			delete(m.st.Enrolled, h)
		}
	}
	m.st.Enrolled[hashSecret(secret)] = macosRunner{Mac: mac, Enrollment: enrollment}
	return m.saveLocked()
}

// runnerFor is the enrolled runner of the Mac's enrollment.
func (m *macosRuns) runnerFor(mac, enrollment string) (macosRunner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.st.Enrolled {
		if e.Mac == mac && e.Enrollment == enrollment {
			return e, true
		}
	}
	return macosRunner{}, false
}

// enrolled is whether the macOS runner of the Mac's enrollment is enrolled.
func (m *macosRuns) enrolled(mac, enrollment string) bool {
	_, ok := m.runnerFor(mac, enrollment)
	return ok
}

func (m *macosRuns) runnerOf(secret string) (macosRunner, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.st.Enrolled[hashSecret(secret)]
	return e, ok
}

// sayAttendance keeps what the runner of secret said of its Mac, and
// returns the runner as it was before.
func (m *macosRuns) sayAttendance(secret string, unattended bool, at time.Time) (macosRunner, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := hashSecret(secret)
	e, ok := m.st.Enrolled[h]
	if !ok {
		return e, errNoRunner
	}
	now := e
	now.Unattended, now.Said = unattended, at
	m.st.Enrolled[h] = now
	return e, m.saveLocked()
}

var errNoRunner = errors.New("no macOS runner is enrolled")

// revoke ends the Mac's macOS runner as its enrollment ends: its secret and
// one-time tokens stop working, and a run not done yet is dropped, so that a
// Mac enrolled later under the name takes none of it. A finished run stays.
func (m *macosRuns) revoke(mac string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for h, e := range m.st.Enrolled {
		if e.Mac == mac {
			delete(m.st.Enrolled, h)
			n++
		}
	}
	for h, t := range m.st.Tokens {
		if t.Mac == mac {
			delete(m.st.Tokens, h)
		}
	}
	if r := m.st.Runs[mac]; r != nil && !api.MacOSEnded(r.State) {
		m.dropped = append(m.dropped, r.PerfRun)
		delete(m.st.Runs, mac)
	}
	delete(m.st.Builds, mac)
	if stop := m.timers[mac]; stop != nil {
		close(stop)
		delete(m.timers, mac)
	}
	return n, m.saveLocked()
}

// takeDropped is the Performance runs of the runs revoke dropped, for labd
// to cancel.
func (m *macosRuns) takeDropped() []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.dropped
	m.dropped = nil
	return d
}

// change applies f to the Mac's run under the lock and saves it.
func (m *macosRuns) change(mac string, f func(r *api.MacOSRun) error) (*api.MacOSRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.st.Runs[mac]
	if r == nil {
		return nil, errNoRun
	}
	if err := f(r); err != nil {
		return nil, err
	}
	return cloneRun(r), m.saveLocked()
}

var errNoRun = errors.New("no macOS run for this Mac")

// macOS is labd's macOS runs, opened at the first use; it arms the deadline
// of a run that was started before labd restarted.
func (s *Server) macOS() *macosRuns {
	s.macosOnce.Do(func() {
		s.macos = openMacOSRuns(s.cfg.DataDir)
		s.macos.mu.Lock()
		var started, waiting []*api.MacOSRun
		for _, r := range s.macos.st.Runs {
			switch {
			case (r.State == api.MacOSStarted || r.State == api.MacOSRunning) && r.Deadline != nil:
				started = append(started, cloneRun(r))
			case r.State == api.MacOSQueued && r.Expires != nil && r.AtMac == nil:
				waiting = append(waiting, cloneRun(r))
			}
		}
		s.macos.mu.Unlock()
		for _, r := range started {
			s.armMacOSDeadline(r.Mac, r.ID, r.Deadline.Sub(s.now()))
		}
		for _, r := range waiting {
			s.armMacOSTimer(r.Mac, r.ID, r.Expires.Sub(s.now()), s.macOSNobodyCame)
		}
	})
	return s.macos
}

// macOSBenchSet is the Benchmark set a macOS run a Maintainer queues takes
// its files from.
var macOSBenchSet = sets.Newest

// macOSCurrentSet is the set a Mac's macOS result is taken of: the current
// set.
var macOSCurrentSet = sets.Current

// macOSBench is the pinned macOS files of the set's Quick list (default) or
// Full list (extended), by file id; a placeholder is left out. Each must be
// in the artifact store, where lab bench mirror puts it. The set's name goes
// with the run, for the runner to find the list.
func (s *Server) macOSBench(tier string) (string, []api.MacOSArtifact, error) {
	set, err := macOSBenchSet()
	if err != nil {
		return "", nil, err
	}
	list := sets.Quick
	if tier == api.TierExtended {
		list = sets.Full
	}
	arts, err := s.macOSFiles(set, list)
	return set.Name, arts, err
}

// macOSFiles is the pinned macOS files of the set's list, each in the
// artifact store.
func (s *Server) macOSFiles(set *sets.Set, list string) ([]api.MacOSArtifact, error) {
	var out []api.MacOSArtifact
	for _, f := range set.FilesFor(sets.MacOS, set.List(list)) {
		if !f.Pinned() {
			continue
		}
		if _, err := os.Stat(s.artifactPath(f.SHA256)); err != nil {
			return nil, fmt.Errorf("%s: the macOS file %s (%s) is not in the artifact store: lab bench mirror", set.Name, f.ID, short(f.SHA256))
		}
		out = append(out, api.MacOSArtifact{Name: f.ID, SHA256: f.SHA256})
	}
	return out, nil
}

var errNoMac = errors.New("no such Mac")

// queueMacOSRun queues a macOS run of the tier's benchmarks on the Mac. It
// runs when lab-agent boot-macos-once boots the Mac into macOS.
func (s *Server) queueMacOSRun(mac, tier string) (*api.MacOSRun, error) {
	if tier == "" {
		tier = api.TierDefault
	}
	if tier != api.TierDefault && tier != api.TierExtended {
		return nil, fmt.Errorf("tier %q: want default or extended", tier)
	}
	if s.dev(mac) == nil {
		return nil, fmt.Errorf("%w %q", errNoMac, mac)
	}
	set, arts, err := s.macOSBench(tier)
	if err != nil {
		return nil, err
	}
	r, err := s.macOS().queue(mac, tier, set, arts, s.now())
	if err == nil {
		s.log.Printf("macOS run %s (%s) queued for %s", r.ID, tier, mac)
	}
	return r, err
}

func (s *Server) armMacOSDeadline(mac, id string, d time.Duration) {
	s.armMacOSTimer(mac, id, d, s.macOSDeadlinePassed)
}

// armMacOSTimer has f called for the Mac's run id after d, in place of any
// earlier timer of the Mac's: a run waits for a person, then for its
// deadline, never both.
func (s *Server) armMacOSTimer(mac, id string, d time.Duration, f func(mac, id string)) {
	m := s.macOS()
	m.mu.Lock()
	defer m.mu.Unlock()
	if stop := m.timers[mac]; stop != nil {
		close(stop)
	}
	stop := make(chan struct{})
	m.timers[mac] = stop
	at := s.clock().After(max(d, 0))
	go func() {
		select {
		case <-at:
			f(mac, id)
		case <-stop:
		case <-s.ctx.Done():
		}
	}()
}

// stopTimer stops the Mac's timer.
func (m *macosRuns) stopTimer(mac string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stop := m.timers[mac]; stop != nil {
		close(stop)
		delete(m.timers, mac)
	}
}

func (s *Server) macOSDeadlinePassed(mac, id string) {
	r, err := s.macOS().change(mac, func(r *api.MacOSRun) error {
		if r.ID != id || (r.State != api.MacOSStarted && r.State != api.MacOSRunning) {
			return errNoRun
		}
		r.State = api.MacOSNeedsHands
		r.Error = "the macOS runner did not report by its deadline"
		return nil
	})
	if err != nil {
		return
	}
	s.log.Printf("macOS run %s on %s: no report by its deadline: Needs hands", r.ID, mac)
	if r.PerfRun > 0 {
		if err := s.store.endPerfRun(r.PerfRun, perf.NeedsHands, s.now()); err != nil {
			s.log.Printf("macOS run %s: its performance run %d: %v", r.ID, r.PerfRun, err)
		}
	}
	if v := s.dev(mac); v != nil {
		v.setState(api.StateNeedsHands, "macOS run "+r.ID+" did not report in time: a FileVault Mac waits for its password at the screen; type it, or hold the power button and boot Omarchy")
	}
}

func (s *Server) macosRoutes() []route {
	s.macOS()
	return []route{
		{"POST /api/agent/macos-started", ownMac, s.hMacOSStarted},
		{"POST /api/macos/enroll-tokens", maintainers, s.hMacOSEnrollToken},
		{"POST /api/devices/{name}/macos-runs", maintainers, s.hQueueMacOSRun},
		{"GET /api/devices/{name}/macos-runs", maintainers, s.hMacOSRun},
		{"GET /api/devices/{name}/macos", maintainers, s.hMacOSSide},
		// A person at an Attended Mac whose macOS run is due: its
		// Volunteer, or a Maintainer (atTheMac says who).
		{"POST /api/devices/{name}/macos/at-mac", macNamed, s.hMacOSAtMac},
		// The runner's own: a one-time token, then its secret.
		{"POST /api/macos/runner/enroll", anyone, s.hMacOSRunnerEnroll},
		{"GET /api/macos/runner/poll", anyone, s.macosRunner(s.hMacOSPoll)},
		{"GET /api/macos/runner/artifacts/{sha}", anyone, s.macosRunner(s.hMacOSArtifact)},
		{"POST /api/macos/runner/results", anyone, s.macosRunner(s.hMacOSResults)},
		{"POST /api/macos/runner/attendance", anyone, s.macosRunner(s.hMacOSAttendance)},
	}
}

type macosRunnerKey struct{}

// enrollmentOf is the enrollment of the Mac called name, while it is in
// the fleet.
func (s *Server) enrollmentOf(name string) (string, bool) {
	v := s.dev(name)
	if v == nil || v.goneReason() != "" {
		return "", false
	}
	return v.snapshot().ID, true
}

// macosRunner admits a request carrying the secret of a runner whose Mac's
// enrollment still lasts.
func (s *Server) macosRunner(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		var e macosRunner
		ok := false
		if strings.HasPrefix(tok, "mos_") && len(tok) < 200 {
			e, ok = s.macOS().runnerOf(tok)
		}
		if ok {
			id, in := s.enrollmentOf(e.Mac)
			ok = in && id == e.Enrollment
		}
		if !ok {
			s.refuse(w, r, 401, "unknown macOS runner secret")
			return
		}
		noteCaller(r, "macos-runner:"+e.Mac)
		h(w, r.WithContext(context.WithValue(r.Context(), macosRunnerKey{}, e.Mac)))
	}
}

// macOSEnrollLine makes a one-time token for the runner of v's enrollment,
// good for an hour, and the line that spends it in macOS.
func (s *Server) macOSEnrollLine(r *http.Request, v *dev) (api.MacOSEnrollToken, error) {
	d := v.snapshot()
	tok := "mos_enr_" + randHex(16)
	exp := s.now().Add(macosTokenTTL)
	if err := s.macOS().addToken(hashSecret(tok), d.Name, d.ID, exp); err != nil {
		return api.MacOSEnrollToken{}, err
	}
	line := fmt.Sprintf("echo %s | sudo /usr/local/lab/bin/lab-macos-runner enroll --server %s --token-stdin", tok, s.publicBase(r))
	return api.MacOSEnrollToken{Mac: d.Name, Token: tok, Expires: exp, Line: line}, nil
}

func (s *Server) hMacOSEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mac string `json:"mac"`
	}
	if !decodeBody(w, r, 4096, &req) {
		return
	}
	v := s.dev(req.Mac)
	if v == nil {
		httpErr(w, 404, "no device %q", req.Mac)
		return
	}
	t, err := s.macOSEnrollLine(r, v)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.log.Printf("Maintainer %s made a macOS runner token for %s", maintainerName(r), req.Mac)
	writeJSON(w, t)
}

func (s *Server) hMacOSRunnerEnroll(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	var t macosToken
	ok := false
	if strings.HasPrefix(tok, "mos_enr_") && len(tok) < 200 {
		t, ok = s.macOS().useToken(hashSecret(tok), s.now())
	}
	if ok {
		id, in := s.enrollmentOf(t.Mac)
		ok = in && id == t.Enrollment
	}
	if !ok {
		s.refuse(w, r, 401, "macOS runner token unknown, used or expired")
		return
	}
	secret := "mos_" + randHex(24)
	if err := s.macOS().enrolSecret(t.Mac, t.Enrollment, secret); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.log.Printf("the macOS runner of %s enrolled", t.Mac)
	s.macOSBuildSaid(t.Mac, t.Enrollment, r)
	writeJSON(w, api.MacOSEnrolled{Mac: t.Mac, Secret: secret})
}

var (
	reMacOSVersion = regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){0,3}$`)
	reMacOSBuild   = regexp.MustCompile(`^[0-9]{2}[A-Z][0-9]{1,5}[a-z]?$`)
)

// macOSBuildSaid keeps the macOS version and build a runner reported, at
// enrollment and at every macOS boot (sw_vers, as ?version=&build=): a
// person's own boot reports this and nothing else. What does not read as a
// macOS version and build is not kept. With the build known, a missing
// macOS result is queued.
func (s *Server) macOSBuildSaid(mac, enrollment string, r *http.Request) {
	q := r.URL.Query()
	version, build := q.Get("version"), q.Get("build")
	if !reMacOSVersion.MatchString(version) || !reMacOSBuild.MatchString(build) {
		return
	}
	if err := s.macOS().sayBuild(mac, enrollment, version, build, s.now()); err != nil {
		s.log.Printf("%s: its macOS build: %v", mac, err)
		return
	}
	if run, err := s.queueMacOSSide(mac); err == nil && run != nil {
		s.log.Printf("%s: no macOS result of %s at macOS %s (%s): macOS run %s queued", mac, run.Set, version, build, run.ID)
	}
}

// hMacOSAttendance is the runner saying, after a boot, whether it reached
// its user's desktop with nobody typing: Unattended, or Attended again.
func (s *Server) hMacOSAttendance(w http.ResponseWriter, r *http.Request) {
	mac := r.Context().Value(macosRunnerKey{}).(string)
	var a api.MacOSAttendance
	if !decodeBody(w, r, 4096, &a) {
		return
	}
	before, err := s.macOS().sayAttendance(bearer(r), a.Unattended, s.now())
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if before.Unattended != a.Unattended || before.Said.IsZero() {
		s.log.Printf("%s: an %s Mac in macOS, its runner says", mac, attendanceWord(a.Unattended))
	}
	writeJSON(w, s.macOSSide(mac))
}

func attendanceWord(unattended bool) string {
	if unattended {
		return api.MacOSUnattended
	}
	return api.MacOSAttended
}

// macOSSide is the macOS side of the Mac as labd keeps it: whether its
// enrollment's runner is enrolled, and whether the Mac is Attended or
// Unattended. A Mac is Attended until its runner says otherwise.
func (s *Server) macOSSide(mac string) api.MacOSSide {
	side := api.MacOSSide{Mac: mac, Attendance: api.MacOSAttended}
	id, in := s.enrollmentOf(mac)
	if !in {
		return side
	}
	e, ok := s.macOS().runnerFor(mac, id)
	side.Enrolled = ok
	side.Attendance = attendanceWord(ok && e.Unattended)
	if ok && !e.Said.IsZero() {
		said := e.Said
		side.Said = &said
	}
	if b, ok := s.macOS().buildOf(mac, id); ok {
		side.Version, side.Build = b.Version, b.Build
	}
	return side
}

func (s *Server) hMacOSSide(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.dev(name) == nil {
		httpErr(w, 404, "no device %q", name)
		return
	}
	writeJSON(w, s.macOSSide(name))
}

func (s *Server) hQueueMacOSRun(w http.ResponseWriter, r *http.Request) {
	var req api.MacOSRunRequest
	if r.ContentLength != 0 && !decodeBody(w, r, 4096, &req) {
		return
	}
	run, err := s.queueMacOSRun(r.PathValue("name"), req.Tier)
	switch {
	case errors.Is(err, errNoMac):
		httpErr(w, 404, "%v", err)
	case errors.Is(err, errRunWaiting):
		httpErr(w, 409, "%v", err)
	case err != nil:
		httpErr(w, 400, "%v", err)
	default:
		writeJSON(w, run)
	}
}

func (s *Server) hMacOSRun(w http.ResponseWriter, r *http.Request) {
	run := s.macOS().run(r.PathValue("name"))
	if run == nil {
		httpErr(w, 404, "no macOS run for %s", r.PathValue("name"))
		return
	}
	writeJSON(w, run)
}

// hMacOSStarted is lab-agent boot-macos-once, once asahi-bless has set
// macOS as the next boot: refused unless a run is queued, which has
// lab-agent put the next boot back, so that a Mac never boots macOS for
// nothing. The run's deadline starts here.
func (s *Server) hMacOSStarted(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	v.mu.Lock()
	mac := v.d.Name
	v.mu.Unlock()
	now := s.now()
	run, err := s.macOS().change(mac, func(r *api.MacOSRun) error {
		if r.State != api.MacOSQueued {
			return errNoRun
		}
		dl := now.Add(macosDeadline(r.Tier))
		r.State, r.Started, r.Deadline = api.MacOSStarted, &now, &dl
		return nil
	})
	if err != nil {
		httpErr(w, 409, "no macOS run is queued for %s", mac)
		return
	}
	s.armMacOSDeadline(mac, run.ID, run.Deadline.Sub(now))
	s.log.Printf("%s: macOS run %s started", mac, run.ID)
	writeJSON(w, run)
}

// A started run is the boot's that began from macosClockSkew before the
// run started to macosStartWindow after, as the runner has it too.
const (
	macosClockSkew   = 2 * time.Minute
	macosStartWindow = 15 * time.Minute
)

var reBootSession = regexp.MustCompile(`^[0-9a-f]{32}$`)

// hMacOSPoll is a runner's poll at a macOS boot. It reports the boot's
// macOS build, which labd keeps, and the boot (?boot=, its session hashed,
// and ?booted=, when it began, in Unix seconds). A started run is handed
// only to a boot that began after labd started it, and is bound to the
// first such boot: no other boot gets it, and a person's own boot gets
// nothing.
func (s *Server) hMacOSPoll(w http.ResponseWriter, r *http.Request) {
	mac := r.Context().Value(macosRunnerKey{}).(string)
	if id, ok := s.enrollmentOf(mac); ok {
		s.macOSBuildSaid(mac, id, r)
	}
	q := r.URL.Query()
	boot := q.Get("boot")
	secs, berr := strconv.ParseInt(q.Get("booted"), 10, 64)
	version, build := q.Get("version"), q.Get("build")
	retarget := false
	run, err := s.macOS().change(mac, func(m *api.MacOSRun) error {
		switch {
		case m.State != api.MacOSStarted && m.State != api.MacOSRunning:
			return errNoRun
		case m.Boot != "":
			if m.Boot != boot {
				return errNoRun
			}
			return nil
		case !reBootSession.MatchString(boot) || berr != nil || m.Started == nil:
			return errNoRun
		}
		at := time.Unix(secs, 0)
		if at.Before(m.Started.Add(-macosClockSkew)) || at.After(m.Started.Add(macosStartWindow)) {
			return errNoRun
		}
		m.Boot, m.State = boot, api.MacOSRunning
		if reMacOSBuild.MatchString(build) && reMacOSVersion.MatchString(version) && build != m.Build && m.Build != "" {
			m.Version, m.Build, retarget = version, build, true
		}
		return nil
	})
	if err != nil {
		writeJSON(w, api.MacOSPoll{})
		return
	}
	if retarget && run.PerfRun > 0 {
		if err := s.store.retargetMacOSPerfRun(run.PerfRun, version, build); err != nil {
			s.log.Printf("macOS run %s: its performance run at macOS %s: %v", run.ID, build, err)
		}
	}
	writeJSON(w, api.MacOSPoll{Run: run})
}

func (s *Server) hMacOSArtifact(w http.ResponseWriter, r *http.Request) {
	mac, sha := r.Context().Value(macosRunnerKey{}).(string), r.PathValue("sha")
	run := s.macOS().run(mac)
	if run == nil || run.State != api.MacOSRunning || !reSHA.MatchString(sha) {
		httpErr(w, 404, "no artifact %s for this Mac now", short(sha))
		return
	}
	for _, a := range run.Artifacts {
		if a.SHA256 == sha {
			serveArtifact(w, r, s.artifactPath(sha))
			return
		}
	}
	httpErr(w, 404, "no artifact %s for this Mac now", short(sha))
}

func (s *Server) hMacOSResults(w http.ResponseWriter, r *http.Request) {
	mac := r.Context().Value(macosRunnerKey{}).(string)
	var up api.MacOSResults
	if !decodeBody(w, r, 1<<20, &up) {
		return
	}
	now := s.now()
	run, err := s.macOS().change(mac, func(r *api.MacOSRun) error {
		if r.ID != up.RunID || (r.State != api.MacOSRunning && r.State != api.MacOSNeedsHands) {
			return errNoRun
		}
		r.State, r.Done, r.Results, r.Error = api.MacOSDone, &now, up.Results, up.Error
		return nil
	})
	if err != nil {
		httpErr(w, 409, "no macOS run %s is running on %s", up.RunID, mac)
		return
	}
	s.macOS().stopTimer(mac)
	s.log.Printf("%s: macOS run %s done, %d results", mac, run.ID, len(run.Results))
	writeJSON(w, map[string]string{"run": run.ID, "state": run.State})
}
