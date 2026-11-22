package server

// macOS runs: the same benchmarks in macOS, on the same Mac, unattended.
//
// A run is queued for a Mac (queueMacOSRun, or POST
// /api/devices/{name}/macos-runs). lab-agent boot-macos-once tells labd it is
// booting macOS once (POST /api/agent/macos-started), which starts the run's
// deadline. In macOS, lab-macos-runner, a LaunchDaemon enrolled once per Mac
// with a one-time token, asks labd at boot for its run (GET
// /api/macos/runner/poll), fetches the run's pinned artifacts by sha256, runs
// the tier's list and uploads the results, then reboots back to Omarchy. A
// run not reported by its deadline (a FileVault Mac waits for its password
// at the screen) leaves the Mac Needs hands.
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
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

const macosTokenTTL = time.Hour

// macosDeadline is how long a started run has to report: the boot, the
// benchmarks of its tier and the upload.
var macosDeadline = func(tier string) time.Duration {
	if tier == api.TierExtended {
		return 60 * time.Minute
	}
	return 30 * time.Minute
}

type macosToken struct {
	Mac     string    `json:"mac"`
	Expires time.Time `json:"expires"`
}

type macosState struct {
	Tokens  map[string]macosToken    `json:"tokens"`  // hash of a one-time token
	Runners map[string]string        `json:"runners"` // hash of a runner's secret -> Mac
	Runs    map[string]*api.MacOSRun `json:"runs"`    // the Mac's latest run
}

type macosRuns struct {
	mu     sync.Mutex
	path   string
	st     macosState
	timers map[string]chan struct{} // closed to stop the Mac's deadline
}

func openMacOSRuns(dir string) *macosRuns {
	m := &macosRuns{path: filepath.Join(dir, "macos-runs.json"), timers: map[string]chan struct{}{}}
	if b, err := os.ReadFile(m.path); err == nil {
		json.Unmarshal(b, &m.st)
	}
	if m.st.Tokens == nil {
		m.st.Tokens = map[string]macosToken{}
	}
	if m.st.Runners == nil {
		m.st.Runners = map[string]string{}
	}
	if m.st.Runs == nil {
		m.st.Runs = map[string]*api.MacOSRun{}
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

func (m *macosRuns) queue(mac, tier string, arts []api.MacOSArtifact, now time.Time) (*api.MacOSRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.st.Runs[mac]; r != nil && r.State != api.MacOSDone && r.State != api.MacOSNeedsHands {
		return nil, fmt.Errorf("%w (%s, %s)", errRunWaiting, r.ID, r.State)
	}
	r := &api.MacOSRun{ID: "mos-" + randHex(6), Mac: mac, Tier: tier, State: api.MacOSQueued, Artifacts: arts, Queued: now}
	m.st.Runs[mac] = r
	return cloneRun(r), m.saveLocked()
}

func (m *macosRuns) addToken(hash, mac string, exp time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st.Tokens[hash] = macosToken{Mac: mac, Expires: exp}
	return m.saveLocked()
}

// useToken spends a one-time token and returns its Mac.
func (m *macosRuns) useToken(hash string, now time.Time) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.st.Tokens[hash]
	if !ok {
		return "", false
	}
	delete(m.st.Tokens, hash)
	m.saveLocked()
	return t.Mac, now.Before(t.Expires)
}

// enrolSecret makes secret the Mac's runner secret, ending any earlier one.
func (m *macosRuns) enrolSecret(mac, secret string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, v := range m.st.Runners {
		if v == mac {
			delete(m.st.Runners, h)
		}
	}
	m.st.Runners[hashSecret(secret)] = mac
	return mac, m.saveLocked()
}

// enrolled is whether the Mac's macOS runner is enrolled.
func (m *macosRuns) enrolled(mac string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range m.st.Runners {
		if v == mac {
			return true
		}
	}
	return false
}

func (m *macosRuns) runnerOf(secret string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mac, ok := m.st.Runners[hashSecret(secret)]
	return mac, ok
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
		var started []*api.MacOSRun
		for _, r := range s.macos.st.Runs {
			if (r.State == api.MacOSStarted || r.State == api.MacOSRunning) && r.Deadline != nil {
				started = append(started, cloneRun(r))
			}
		}
		s.macos.mu.Unlock()
		for _, r := range started {
			s.armMacOSDeadline(r.Mac, r.ID, r.Deadline.Sub(s.now()))
		}
	})
	return s.macos
}

// macOSBenchSet is the Benchmark set a macOS run takes its files from.
var macOSBenchSet = sets.Newest

// macOSBench is the pinned macOS files of the set's Quick list (default) or
// Full list (extended), by file id; a placeholder is left out. Each must be
// in the artifact store, where lab bench mirror puts it.
func (s *Server) macOSBench(tier string) ([]api.MacOSArtifact, error) {
	set, err := macOSBenchSet()
	if err != nil {
		return nil, err
	}
	list := sets.Quick
	if tier == api.TierExtended {
		list = sets.Full
	}
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
	arts, err := s.macOSBench(tier)
	if err != nil {
		return nil, err
	}
	r, err := s.macOS().queue(mac, tier, arts, s.now())
	if err == nil {
		s.log.Printf("macOS run %s (%s) queued for %s", r.ID, tier, mac)
	}
	return r, err
}

func (s *Server) armMacOSDeadline(mac, id string, d time.Duration) {
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
			s.macOSDeadlinePassed(mac, id)
		case <-stop:
		case <-s.ctx.Done():
		}
	}()
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
		// The runner's own: a one-time token, then its secret.
		{"POST /api/macos/runner/enroll", anyone, s.hMacOSRunnerEnroll},
		{"GET /api/macos/runner/poll", anyone, s.macosRunner(s.hMacOSPoll)},
		{"GET /api/macos/runner/artifacts/{sha}", anyone, s.macosRunner(s.hMacOSArtifact)},
		{"POST /api/macos/runner/results", anyone, s.macosRunner(s.hMacOSResults)},
	}
}

type macosRunnerKey struct{}

// macosRunner admits a request carrying an enrolled runner's secret.
func (s *Server) macosRunner(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		mac, ok := "", false
		if strings.HasPrefix(tok, "mos_") && len(tok) < 200 {
			mac, ok = s.macOS().runnerOf(tok)
		}
		if !ok {
			s.refuse(w, r, 401, "unknown macOS runner secret")
			return
		}
		noteCaller(r, "macos-runner:"+mac)
		h(w, r.WithContext(context.WithValue(r.Context(), macosRunnerKey{}, mac)))
	}
}

func (s *Server) hMacOSEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mac string `json:"mac"`
	}
	if !decodeBody(w, r, 4096, &req) {
		return
	}
	if s.dev(req.Mac) == nil {
		httpErr(w, 404, "no device %q", req.Mac)
		return
	}
	tok := "mos_enr_" + randHex(16)
	exp := s.now().Add(macosTokenTTL)
	if err := s.macOS().addToken(hashSecret(tok), req.Mac, exp); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.log.Printf("Maintainer %s made a macOS runner token for %s", maintainerName(r), req.Mac)
	line := fmt.Sprintf("echo %s | sudo /usr/local/lab/bin/lab-macos-runner enroll --server %s --token-stdin", tok, s.publicBase(r))
	writeJSON(w, api.MacOSEnrollToken{Mac: req.Mac, Token: tok, Expires: exp, Line: line})
}

func (s *Server) hMacOSRunnerEnroll(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	mac, ok := "", false
	if strings.HasPrefix(tok, "mos_enr_") && len(tok) < 200 {
		mac, ok = s.macOS().useToken(hashSecret(tok), s.now())
	}
	if !ok || s.dev(mac) == nil {
		s.refuse(w, r, 401, "macOS runner token unknown, used or expired")
		return
	}
	secret := "mos_" + randHex(24)
	if _, err := s.macOS().enrolSecret(mac, secret); err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.log.Printf("the macOS runner of %s enrolled", mac)
	writeJSON(w, api.MacOSEnrolled{Mac: mac, Secret: secret})
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

// hMacOSStarted is lab-agent boot-macos-once, before it blesses macOS for
// the next boot: refused unless a run is queued, so that a Mac never boots
// macOS for nothing.
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

func (s *Server) hMacOSPoll(w http.ResponseWriter, r *http.Request) {
	mac := r.Context().Value(macosRunnerKey{}).(string)
	run, err := s.macOS().change(mac, func(r *api.MacOSRun) error {
		if r.State != api.MacOSStarted && r.State != api.MacOSRunning {
			return errNoRun
		}
		r.State = api.MacOSRunning
		return nil
	})
	if err != nil {
		writeJSON(w, api.MacOSPoll{})
		return
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
	m := s.macOS()
	m.mu.Lock()
	if stop := m.timers[mac]; stop != nil {
		close(stop)
		delete(m.timers, mac)
	}
	m.mu.Unlock()
	s.log.Printf("%s: macOS run %s done, %d results", mac, run.ID, len(run.Results))
	writeJSON(w, map[string]string{"run": run.ID, "state": run.State})
}
