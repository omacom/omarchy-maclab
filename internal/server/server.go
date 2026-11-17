// Package server is labd: the controller that owns devices, leases, jobs
// and the recovery ladder. Agents, the lab CLI and the MCP bridge talk to it over HTTP.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omacom/omarchy-maclab/installer"
	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/oob"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

type Config struct {
	DataDir      string
	Interval     time.Duration // agent heartbeat interval
	BootTimeout  time.Duration // default time for a Mac to come back after a reboot
	OfflineAfter time.Duration
	// ResetWait is how long a person's Hard reset may wait its turn at the
	// Out-of-band controller; zero is resetWait.
	ResetWait time.Duration
	// AliveWindow and QuietGrace are timeouts that assume a LAN, as
	// StageTimeout below does; a Mac far from labd needs them longer. Zero
	// keeps the LAN default; labd's flags default to remote-sized values.
	AliveWindow time.Duration // how stale a heartbeat may be before a Mac counts unresponsive
	QuietGrace  time.Duration // silence on the same boot before a command counts lost
	// RebootGrace, LabClockSkew and WatchdogIdleReset are lab-agent's times
	// that labd waits on; zero keeps their real values (a harness with
	// seconds-long boots sets them shorter). RebootGrace: how long a Mac asked
	// to reboot may still be heard on its old boot, 15 s. LabClockSkew: how
	// far a Lab boot's clock may run behind, agent.NoContactMargin.
	// WatchdogIdleReset: how long after its last sign of labd the agent's
	// watchdog resets a Lab boot outside a test, agent.LostLabdReset.
	RebootGrace, LabClockSkew, WatchdogIdleReset time.Duration
	// WatchdogOpenTimeout is watchdog.open_timeout on every Lab boot's command
	// line; 0 leaves it to lab-agent: half the job's boot timeout, at least
	// 120 s.
	WatchdogOpenTimeout time.Duration
	// RebootWarning is how long before a Volunteer job's first reboot its Mac
	// shows the Volunteer a notice, and how long the job waits after it; 0 is
	// no notice and no wait. labd's flag defaults to 5m.
	RebootWarning time.Duration
	// NtfyURL is the notice server's address (https://ntfy.sh): each
	// Volunteer's Mac has its own topic there, which labd keeps. It carries
	// no topic: labd posts nothing but a Notice, and only to a Mac's topic.
	NtfyURL   string
	NotifyCmd string
	// noticeTLS and noticeAllow are for tests alone: the roots a Notice's
	// https is checked against, and addresses a Volunteer's own topic may be
	// at beside the public internet.
	noticeTLS   *tls.Config
	noticeAllow func(netip.AddrPort) bool
	// Person names the person a request without a Maintainer's token or a
	// browser session comes from, by their login, "" for nobody: such a
	// person may look at their own Macs (see scope.go). labd itself sets
	// none; tests of scope name people with it.
	Person func(r *http.Request) string
	// OOB is the Out-of-band controller on this machine, which resets a
	// port; nil when labd was given no token file for it, and then no Mac
	// gets a Hard reset.
	OOB *oob.Client
	// SightEvery is how often labd asks that controller what its ports
	// show, for the Sightings; zero is every 15 s.
	SightEvery time.Duration
	// SourceHosts are the hosts a build's source may come from beside
	// github.com, as SourceHosts returns them (see sources.go).
	SourceHosts []string
	// MTeamBuilds opens builds to the M team. It stays off until the two
	// isolation tests of the Builder have passed (see builders.go); labd
	// has no flag for it.
	MTeamBuilds bool
	// ArtifactBudget caps the artifact store; unreferenced artifacts go first.
	ArtifactBudget int64
	// Transfers are the limits uploads work inside; the zero value is the lab's.
	Transfers Transfers
	// OMT runs omarchy-m-test in every job on Macs with a desktop user.
	OMT bool
	// OMTPublish has the lab publish by itself the runs that became a Mac's
	// Reference: each baseline, and a scheduled refresh once a week. Without
	// it a report is published only by hand.
	OMTPublish bool
	// OMTEvery re-runs omarchy-m-test on each idle Mac's known-good kernel this often; 0 never.
	OMTEvery time.Duration
	// StageTimeout is how long labd waits for one staging attempt (default 20
	// minutes). Tests shorten it.
	StageTimeout time.Duration
	// TestGrace is how long past a test's timeout labd waits for its result
	// (default 2 minutes); GUIGrace is how much longer for a GUI test (default
	// 2 minutes, see guiWait). Tests shorten them.
	TestGrace, GUIGrace time.Duration
	// AgentBinary is the linux/arm64 lab-agent labd hands out: to the Join
	// command, and to an enrolled Mac that updates. Its signature is the
	// file beside it with ".sig", as a Lab release's bundle holds them.
	AgentBinary string
	// LabAgent is the number of that lab-agent; "" is the one labd was
	// built with, which is the one its bundle holds.
	LabAgent string
	// PublicURL is the Lab address, the URL people and Macs reach labd at,
	// its path being labd's prefix (--public-url, see edge.go). Nil serves
	// at / on the loopback port alone and takes each request's own origin.
	PublicURL *url.URL
	// Alias is the one other host name the tunnel carries (--alias): labd
	// forwards its page requests to the Lab address and refuses the rest
	// (see edge.go). It needs PublicURL.
	Alias string
	// Preview is the lab answering at the Lab address, closed, before it
	// opens (--preview): the one switch for everything that changes (see
	// Server.Preview).
	Preview bool
	// Release is the Lab release this labd came from, which it tells a lab
	// on every answer. MinLab is the oldest lab release it takes; empty is
	// MinLabRelease (see labcli.go).
	Release, MinLab string
	// ReleaseKey is the release key this labd was built with, as "ssh-ed25519
	// <base64>": the install line pins it (install_line.go). Empty is the
	// key the install script carries.
	ReleaseKey string
	// MachineIDKey is the 64 bytes a report's Mac is told from another by
	// (reports.NewRules): a permanent secret, reproduced from the old site's.
	// Without it labd takes no reports.
	MachineIDKey []byte
	// GitHub is the OAuth app people sign in through with a browser; nil
	// leaves sign-in off (see ReadSignIn).
	GitHub *GitHubApp
	// Admins are the GitHub account ids of the lab's Admins: Maintainers
	// who may also add and remove Maintainers. They are named here and on
	// no screen.
	Admins []int64
	// Signal is the lab's Signal sender; nil records every line for the two
	// groups and posts none. SignalDir is the folder with the groups' ids
	// (see groupFiles): a group with no id there gets nothing.
	Signal    Sender
	SignalDir string
	// Watch is what labd watches the lab's own parts with, for "Agents"
	// and /status (see watch.go); nil watches nothing.
	Watch *Watch
	// Clock is where labd reads time; nil is the wall clock.
	Clock agent.Clock
}

type Server struct {
	// jobRead, which only a test sets, runs in updateJobByID between its
	// read of the job and its save.
	jobRead atomic.Pointer[func(id string)]
	cfg     Config
	store   *store
	reports *reports.Store
	// uploads takes reports in; nil without the machine-id key.
	uploads   *reports.Uploads
	catalogue *reports.Catalogue
	// installScript and installLabScript are what /install and
	// /install/lab serve while the lab is in Preview (install_line.go);
	// installOpen and installLabOpen are what they serve once it is not.
	installScript, installLabScript []byte
	installOpen, installLabOpen     []byte
	// openedAt is when the lab opened, in microseconds; 0 while it has
	// not (opening.go). opening lets one opening run at a time.
	openedAt atomic.Int64
	opening  sync.Mutex
	// macOSReference gives a board's macOS numbers; nil is the catalogue's.
	macOSReference func(board string) *reports.MacOSReference
	log            *log.Logger
	ctx            context.Context
	start          time.Time

	// macSignIns answers a Mac's sign-in, status and sign-out; nil without
	// the machine-id key, which names the Mac.
	macSignIns *reports.MacSignIns

	// drain holds queued jobs back before a Deploy or a Maintenance run.
	drain drain

	mu          sync.Mutex
	devs        map[string]*dev // by name
	cancels     map[string]context.CancelFunc
	cancelWhy   map[string]string // why a job in cancels was canceled, when whoever did said
	inFlight    jobsInFlight      // under jobMu; see forgetting.go
	hub         hub
	builds      *buildMgr
	notifyHooks []func(Notification)
	conns       maintainerConns
	bconns      maintainerConns // named builders' open requests, by generation
	serialMu    sync.Mutex
	callers     *callerLog
	signIns     signInStates
	site        *web.Site
	// lines is the record of what the two Signal groups are told; labGroup
	// and agents hold a function for each event (lines_lab.go, lines_agents.go).
	lines    *groupLines
	labGroup labGroupLines
	agents   agentsLines

	// watch is what labd saw of the lab's own parts (watch.go).
	watch watcher

	// refusals counts refused credentials by address (refused.go).
	refusals refusals

	// leaseMu makes taking a Lease one step: the count of a person's Leases
	// per Configuration is read on other Macs than the one leased.
	leaseMu sync.Mutex
	// releaseMu makes a verdict one step: whose turn it is depends on the
	// history it is added to.
	releaseMu sync.Mutex
	// forgetting is held by a forgetting's sweep, and read by an Installed
	// run's seal (installed.go).
	forgetting sync.RWMutex
	// auditWrite is read by every write to labd's copies of Audit logs, each
	// under its Mac's auditMu, and held by a forgetting's sweep of them: a
	// Mac that Joins during the sweep waits as an enrolled one does.
	auditWrite sync.RWMutex
	// retiredAudit is where labd's copy of each Mac retired since it started
	// keeps its Audit log, under mu (access.go).
	retiredAudit map[*dev]string
	// cables are the Sightings: which Lab-owned Mac is on which of the
	// Out-of-band controller's ports (see sighting.go).
	cables *cables

	// shippedTerms, which only a test sets, stands in for the terms shipped
	// with this Lab release.
	shippedTerms atomic.Pointer[web.Text]

	// execRecorded, which only a test sets, runs once a lab exec's line is
	// in the Exec record and before its command is queued for the Mac.
	execRecorded func()
}

// clock is where labd reads time: Config.Clock, or the wall clock when unset.
func (s *Server) clock() agent.Clock {
	if s.cfg.Clock == nil {
		return agent.WallClock
	}
	return s.cfg.Clock
}

func (s *Server) now() time.Time                  { return s.clock().Now() }
func (s *Server) since(t time.Time) time.Duration { return s.clock().Now().Sub(t) }

// withTimeout is context.WithTimeout on labd's clock.
func (s *Server) withTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	c := s.clock()
	if c == agent.WallClock {
		return context.WithTimeout(parent, d)
	}
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-ctx.Done():
		case <-c.After(d):
			cancel()
		}
	}()
	return ctx, cancel
}

// SetRebootWarning sets how long before a Volunteer job's first reboot labd
// has the Mac warn its Volunteer (Config.RebootWarning).
func (c *Config) SetRebootWarning(d time.Duration) { c.RebootWarning = d }

func New(ctx context.Context, cfg Config, logger *log.Logger) (*Server, error) {
	if logger == nil {
		logger = log.New(os.Stderr, "labd: ", log.LstdFlags)
	}
	if cfg.Interval == 0 {
		cfg.Interval = 8 * time.Second
	}
	if cfg.BootTimeout == 0 {
		cfg.BootTimeout = 4 * time.Minute
	}
	if cfg.ResetWait == 0 {
		cfg.ResetWait = resetWait
	}
	if cfg.StageTimeout == 0 {
		cfg.StageTimeout = 20 * time.Minute
	}
	if cfg.TestGrace == 0 {
		cfg.TestGrace = 2 * time.Minute
	}
	if cfg.GUIGrace == 0 {
		cfg.GUIGrace = 2 * time.Minute
	}
	if cfg.OfflineAfter == 0 {
		cfg.OfflineAfter = time.Minute
	}
	if cfg.Alias != "" {
		if cfg.PublicURL == nil {
			return nil, fmt.Errorf("--alias needs --public-url: the Alias only leads to the Lab address")
		}
		cfg.Alias = canonHost(cfg.PublicURL.Scheme, cfg.Alias)
		if cfg.Alias == cfg.PublicURL.Host {
			return nil, fmt.Errorf("--alias %s is the Lab address itself", cfg.Alias)
		}
	}
	// The LAN defaults are read once, here: labd's goroutines can outlive a
	// Server, and must not read them while a test sets them for the next one.
	if cfg.AliveWindow == 0 {
		cfg.AliveWindow = aliveWindow
	}
	if cfg.QuietGrace == 0 {
		cfg.QuietGrace = quietGrace
	}
	if cfg.RebootGrace == 0 {
		cfg.RebootGrace = 15 * time.Second
	}
	if cfg.LabClockSkew == 0 {
		cfg.LabClockSkew = agent.NoContactMargin
	}
	if cfg.WatchdogIdleReset == 0 {
		cfg.WatchdogIdleReset = agent.LostLabdReset
	}
	for _, d := range []string{"artifacts", "jobs", "serial", "baselines", "builds"} {
		if err := os.MkdirAll(filepath.Join(cfg.DataDir, d), 0o755); err != nil {
			return nil, err
		}
	}
	features, err := loadCatalogue()
	if err != nil {
		return nil, err
	}
	installLine, err := installScript(cfg, installer.Script)
	if err != nil {
		return nil, err
	}
	installLabLine, err := installScript(cfg, installer.LabScript)
	if err != nil {
		return nil, err
	}
	open := cfg
	open.Preview = false
	installOpen, err := installScript(open, installer.Script)
	if err != nil {
		return nil, err
	}
	installLabOpen, err := installScript(open, installer.LabScript)
	if err != nil {
		return nil, err
	}
	var rules *reports.Rules
	if cfg.MachineIDKey != nil {
		if rules, err = reports.NewRules(cfg.MachineIDKey); err != nil {
			return nil, err
		}
	}
	callers, err := openCallerLog(filepath.Join(cfg.DataDir, "callers"), logger, ctx.Done())
	if err != nil {
		return nil, err
	}
	st, err := openStore(filepath.Join(cfg.DataDir, "labd.db"))
	if err != nil {
		return nil, err
	}
	rep, err := reports.Open(filepath.Join(cfg.DataDir, "reports.db"))
	if err != nil {
		st.db.Close()
		return nil, err
	}
	s := &Server{cfg: cfg, store: st, reports: rep, catalogue: features, installScript: installLine, installLabScript: installLabLine, installOpen: installOpen, installLabOpen: installLabOpen, log: logger, ctx: ctx, devs: map[string]*dev{}, cancels: map[string]context.CancelFunc{}, cancelWhy: map[string]string{}, callers: callers}
	s.start = s.now()
	opened, err := st.liveAt()
	if err != nil {
		st.db.Close()
		rep.Close()
		return nil, err
	}
	if !opened.IsZero() {
		s.openedAt.Store(opened.UnixMicro())
		if cfg.Preview {
			logger.Printf("the lab is Live, opened %s: --preview is not obeyed, and nothing closes an open lab", opened.In(brisbane).Format("2006-01-02 15:04 MST"))
		}
	}
	s.cables = newCables(cfg.OOB)
	if cfg.OOB != nil {
		kept, err := st.sightings()
		if err != nil {
			st.db.Close()
			rep.Close()
			return nil, fmt.Errorf("reading the Sightings: %w", err)
		}
		s.cables.kept, s.cables.saved = kept, kept
	}
	s.lines = newGroupLines(st, s.clock(), logger, cfg.Signal, cfg.SignalDir)
	s.labGroup = labGroupLines{l: s.lines, link: s.pageAddress}
	s.agents = agentsLines{s.lines}
	if rules != nil {
		s.uploads = reports.NewUploads(rep, rules, s.now)
		s.uploads.OnMTeam = s.onMTeam
		var github reports.GitHub
		if cfg.GitHub != nil {
			github = macSignInGitHub{cfg.GitHub}
		}
		s.macSignIns = reports.NewMacSignIns(rep, rules, s.now, github, macSignInPeople{s})
	}
	if s.site, err = web.New(s.basePath()); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "admin.token")); err == nil {
		logger.Printf("%s is ignored: the admin token is retired, and the file can be deleted", filepath.Join(cfg.DataDir, "admin.token"))
	}
	rows, err := st.devices()
	if err != nil {
		return nil, err
	}
	interrupted, err := st.jobs("", 1000, true)
	if err != nil {
		return nil, err
	}
	// The Lab-owned Macs that were up when labd stopped: recovery runs on
	// whichever of them does not return (see recoverAfterStart).
	var wasUp []*dev
	for _, row := range rows {
		unused := neverUsed(st, row.d) // as stored, before the changes below
		d := row.d
		for _, j := range interrupted {
			if j.Spec.Device == d.Name && j.State != api.JobQueued && (j.Spec.RecoveryMode == api.RecoveryNetworkOnly || j.EffectiveRecoveryMode == api.RecoveryNetworkOnly) {
				d.EffectiveRecoveryMode = api.RecoveryNetworkOnly
				d.RecoveryModeJob = j.ID
			}
		}
		d.ActiveJob = ""
		if d.Lease != nil && d.Lease.Job != "" { // no job outlives labd, so none extends a Lease
			l := *d.Lease
			l.Job = ""
			d.Lease = &l
		}
		if d.Kind == "" { // enrolled before Kinds existed: ./volunteer's Macs are Volunteers'
			d.Kind = map[bool]string{false: api.KindLabOwned, true: api.KindVolunteer}[d.Volunteer]
		}
		if d.IsVolunteer() && d.AccessLevel == "" {
			d.AccessLevel = api.AccessQualification // enrolled before Access levels existed
		}
		if d.State == api.StateBusy || d.State == api.StateRecovering {
			d.State = api.StateOffline
		}
		v := newDev(s, d, row.secretHash)
		v.dirty = !unused // labd may have stopped mid-job
		s.devs[d.Name] = v
		if l := s.screens(d.Name); len(l) > 0 {
			v.screenAt, v.screenName = l[0].Time, l[0].Name
		}
		if !d.IsVolunteer() && !row.d.LastSeen.IsZero() && (row.d.State == api.StateReady || row.d.State == api.StateNew || row.d.State == api.StateBusy || row.d.State == api.StateRecovering) {
			wasUp = append(wasUp, v)
		}
		go s.deviceLoop(v)
	}
	s.adoptReleases()
	s.termsTookEffect()
	s.builds = newBuildMgr(s)
	s.reconcileRunningBuilds()
	// A job never sent to its Mac, queued or waiting for its build, is
	// queued again, after the builds the last labd left running have
	// failed: its build is asked for again. Jobs that were running when
	// labd stopped cannot be resumed.
	unfinished, _ := st.jobs("", 1000, true)
	sort.Slice(unfinished, func(i, k int) bool { return unfinished[i].Created.Before(unfinished[k].Created) })
	for _, j := range unfinished {
		if j.State == api.JobQueued || j.State == api.JobBuilding {
			if v := s.devs[j.Spec.Device]; v != nil {
				v.queue <- j.ID
				continue
			}
		}
		s.tellVolunteerJobEndedByRestart(j, api.OutcomeInfra)
		j.State, j.Outcome, j.Summary = api.JobDone, api.OutcomeInfra, "labd restarted while the job was running"
		st.saveJob(j)
	}
	// A labd that starts refuses no address: what the one before counted
	// went with it.
	s.agents.LimiterQuiet()
	go s.monitor()
	go s.leaseKeeper()
	go s.buildMonitor()
	go s.lines.run(ctx)
	go s.summaryLoop()
	go s.joinCommandLoop()
	if cfg.Watch != nil {
		s.startWatch()
		if !cfg.Watch.ByHand {
			go s.watchLoop()
		}
	}
	go s.gcLoop()
	go s.omtScheduler()
	go s.callerLogLoop()
	go s.sightLoop()
	go s.recoverAfterStart(wasUp)
	go s.execRecordLoop()
	return s, nil
}

// neverUsed reports whether a stored Mac has demonstrably never been used: it
// was New when labd stopped, with no known-good kernel and no active job, and
// no job of any state was ever recorded under its name. After a restart labd
// cleans up every other Mac, in case it stopped mid-job; a Mac never used has
// nothing of the lab's to clean, and the cleanup would still rewrite its boot
// entries. Any doubt, a failed lookup included, keeps the cleanup.
func neverUsed(st *store, d api.Device) bool {
	if d.Name == "" || d.State != api.StateNew || d.KnownGood != "" || d.ActiveJob != "" {
		return false
	}
	jobs, err := st.jobs(d.Name, 1, false)
	return err == nil && len(jobs) == 0
}

// OnNotify registers a hook, used by tests to observe notifications.
func (s *Server) OnNotify(f func(Notification)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifyHooks = append(s.notifyHooks, f)
}

func (s *Server) monitor() {
	t := s.clock().NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		}
		s.refusedLook()
		for _, v := range s.allDevs() {
			v.mu.Lock()
			// After a restart, agents need a moment to reconnect before silence means anything.
			idle := (v.d.State == api.StateReady || v.d.State == api.StateNew) && s.since(s.start) > s.cfg.OfflineAfter
			if idle && !v.d.LastSeen.IsZero() && s.since(v.d.LastSeen) > s.cfg.OfflineAfter && !v.quietLocked(s.now()) {
				v.restore = v.d.State
				why := "no heartbeat since " + v.d.LastSeen.Format(time.TimeOnly)
				if !v.asleep.IsZero() {
					why = "asleep since " + v.asleep.Format(time.TimeOnly) + "; a key press wakes it"
				}
				v.setStateLocked(api.StateOffline, why)
			}
			v.mu.Unlock()
		}
	}
}

func (s *Server) allDevs() []*dev {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*dev, 0, len(s.devs))
	for _, v := range s.devs {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].d.Name < out[j].d.Name })
	return out
}

func (s *Server) dev(name string) *dev {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devs[name]
}

func (s *Server) serialPath(name string) string {
	return filepath.Join(s.cfg.DataDir, "serial", name+".log")
}

func (s *Server) appendSerial(name, line string) {
	s.serialMu.Lock()
	defer s.serialMu.Unlock()
	f, err := os.OpenFile(s.serialPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s %s\n", s.now().Format("2006-01-02T15:04:05.000"), line)
	f.Close()
}

func (s *Server) serialOffset(name string) int64 {
	st, err := os.Stat(s.serialPath(name))
	if err != nil {
		return 0
	}
	return st.Size()
}

// copySerial saves the serial console captured during a job alongside its logs.
func (s *Server) copySerial(name string, from int64, job string) {
	s.serialMu.Lock()
	defer s.serialMu.Unlock()
	f, err := os.Open(s.serialPath(name))
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return
	}
	b, _ := io.ReadAll(f)
	if len(b) == 0 {
		return
	}
	os.MkdirAll(s.jobDir(job), 0o755)
	os.WriteFile(filepath.Join(s.jobDir(job), "serial.log"), b, 0o644)
	s.updateJobByID(job, func(j *api.Job) { j.Result.Logs = append(j.Result.Logs, "serial.log") })
}

func (s *Server) jobDir(id string) string { return filepath.Join(s.cfg.DataDir, "jobs", id) }
func (s *Server) baselinePath(name string) string {
	return filepath.Join(s.cfg.DataDir, "baselines", name+"-dmesg-errors.txt")
}

var jobMu sync.Mutex

// jobEnded, under jobMu, holds for a run's copy of a job the stored job a
// cancel found done (updateJobUnlessDone). The run's own next save takes it:
// only the run's goroutine writes the copy it reads without a lock.
var jobEnded = map[*api.Job]*api.Job{}

// updateJob changes j with f and saves it. Only lab exclude (hExclude)
// changes a job's GateExclusion, through f; any other caller's copy of j may
// be older than that, so j takes the stored one first.
func (s *Server) updateJob(j *api.Job, f func(*api.Job)) {
	jobMu.Lock()
	defer jobMu.Unlock()
	s.updateJobLocked(j, f)
}

// updateJobUnlessDone is updateJob, unless the stored job is done: then
// nothing is saved, and j, a run's copy read before the job ended, takes the
// stored one at the run's next save (jobEnded).
func (s *Server) updateJobUnlessDone(j *api.Job, f func(*api.Job)) {
	jobMu.Lock()
	defer jobMu.Unlock()
	if stored, err := s.store.job(j.ID); err == nil && stored.State == api.JobDone {
		jobEnded[j] = stored
		return
	}
	s.updateJobLocked(j, f)
}

// updateJobLocked is updateJob under jobMu.
func (s *Server) updateJobLocked(j *api.Job, f func(*api.Job)) {
	if stored, ok := jobEnded[j]; ok {
		delete(jobEnded, j)
		*j = *stored
		s.forgottenLocked(j)
		return
	}
	j.GateExclusion = s.store.gateExclusion(j.ID)
	f(j)
	s.forgottenLocked(j)
	if j.Result.OMT != nil {
		// said when a job is read (withReport), never kept
		j.Result.OMT.ReportState = ""
	}
	j.Updated = s.now()
	if err := s.store.saveJob(j); err != nil {
		s.log.Printf("save job %s: %v", j.ID, err)
	}
	s.hub.publish("job", j.Spec.Device, map[string]any{"id": j.ID, "device": j.Spec.Device, "state": j.State, "outcome": j.Outcome})
}

// updateJobByID is updateJob of the stored job, read and saved in one hold
// of jobMu: no change between the two is lost.
func (s *Server) updateJobByID(id string, f func(*api.Job)) {
	jobMu.Lock()
	defer jobMu.Unlock()
	j, err := s.store.job(id)
	if h := s.jobRead.Load(); h != nil {
		(*h)(id)
	}
	if err == nil {
		s.updateJobLocked(j, f)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---- HTTP ----

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	for _, rt := range append(s.routes(), s.exportRoutes()...) {
		s.register(m, rt)
	}
	// The interface's own files and pages: each page says who may open it.
	s.site.Register(m)
	for _, p := range pageRoutes {
		m.HandleFunc(p.pattern, s.pageHandler(p))
	}
	return bodyDeadlines(s.edge(s.answerCuts(s.capBodies(s.withPages(m)))))
}

// routes is every API route and who may reach it (see reach).
func (s *Server) routes() []route {
	return []route{
		{"POST /api/agent/enroll", anyone, s.hEnroll},
		{"POST /api/agent/checkin", ownMac, s.hCheckin},
		{"POST /api/agent/report", ownMac, s.hReport},
		{"GET /api/agent/enrollment", ownMac, s.hAgentEnrollment},
		{"POST /api/agent/left", ownMac, s.hAgentLeave},
		{"GET /api/agent/artifacts/{sha}", ownMac, s.hAgentArtifact},
		{"POST /api/agent/upload", ownMac, s.hUpload},
		{"POST /api/agent/leave", ownMac, s.hLeave},
		{"GET /api/agent/published", ownMac, s.hAgentPublished},

		{"GET /api/auth", anyone, s.hAuth},
		{"GET /sign-in", anyone, s.hSignIn},
		{"GET /auth/github/callback", anyone, s.hSignInCallback},
		{"POST /sign-out", anyone, s.hSignOut},
		// Reports from omarchy-m-test, and the two links each is answered
		// with (reports.go): the deletion link's secret is its credential.
		{"POST /api/v1/reports", anyone, s.hUploadReport},
		// A Mac's sign-in, and its status and sign-out (mac_signin.go):
		// each is signed by the Mac's Machine key.
		{"POST /api/v1/tester_bindings", anyone, s.hMacSignIn},
		{"POST /api/v1/tester_requests", anyone, s.hMacRequest},
		{"GET /install", anyone, s.hInstall},
		{"GET /install/lab", anyone, s.hInstallLab},
		{"GET /reports/{id}", anyone, s.hReportPage},
		{"GET /reports/{id}/deletion", anyone, s.hReportDeletion},
		{"POST /reports/{id}", anyone, s.hReportDelete},
		// healthz answers that labd is up, to anyone: a Kubernetes probe. It
		// says nothing else.
		{"GET /healthz", anyone, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") }},
		{"POST /api/devices/{name}/recovery-mode", maintainers, s.hRecoveryMode},
		{"GET /api/recovery", maintainers, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, api.RecoveryCapabilities{Schema: 1, Modes: []string{api.RecoveryNetworkOnly}, PerJobOOBIsolation: true, TransactionIsolation: true})
		}},
		{"GET /api/mesa", maintainers, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, api.MesaCapabilities{Schema: 1, Staged: true, OfficialGPU: false, AgentMin: mesaMinAgent})
		}},
		{"GET /api/devices", scoped, s.hDevices},
		{"GET /api/devices/{name}", macNamed, s.hDevice},
		{"PATCH /api/devices/{name}", maintainers, s.hPatchDevice},
		{"DELETE /api/devices/{name}", maintainers, s.hForgetDevice},
		{"POST /api/devices/{name}/end-enrollment", maintainers, s.hEndEnrollment},
		{"GET /api/retired", maintainers, s.hRetired},
		{"POST /api/devices/{name}/lease", team, s.hLease},
		{"POST /api/devices/{name}/window", macNamed, s.hWindow},
		{"DELETE /api/devices/{name}/lease", team, s.hRelease},
		{"GET /api/devices/{name}/leases", macNamed, s.hLeaseRecord},
		{"POST /api/devices/{name}/reset", team, s.hReset},
		{"GET /api/devices/{name}/serial", team, s.hNoConsole},
		{"GET /api/devices/{name}/oob", macNamed, s.hOOBStatus},
		{"GET /api/stream", scoped, s.hStream},
		{"POST /api/devices/{name}/screenshot", team, s.hScreenshot},
		{"GET /api/devices/{name}/screen", macNamed, s.hScreen},
		{"GET /api/devices/{name}/screens", macNamed, s.hScreens},
		{"GET /api/devices/{name}/screens/{file}", macNamed, s.hScreen},
		{"GET /api/devices/{name}/logs", macNamed, s.hLogs},
		{"GET /api/devices/{name}/console", team, s.hNoConsole},
		{"POST /api/devices/{name}/console", team, s.hNoConsole},
		{"POST /api/devices/{name}/console/arm", team, s.hNoConsole},
		{"POST /api/agent/screen", ownMac, s.hAgentScreen},
		{"POST /api/enroll-tokens", maintainers, s.hEnrollToken},
		{"POST /api/volunteer", anyone, s.hVolunteerGone},
		{"GET /api/volunteer/lab-agent", anyone, s.hVolunteerGone},
		{"GET /join", anyone, s.hJoinScript},
		{"GET /api/join/lab-agent", invited, s.hJoinAgent},
		{"GET /api/join/lab-agent.sig", invited, s.hAgentSig},
		{"GET /api/agent/lab-agent", ownMac, s.hJoinAgent},
		{"GET /api/agent/lab-agent.sig", ownMac, s.hAgentSig},
		{"POST /api/devices/{name}/agent-update", maintainers, s.hAgentUpdate},
		{"GET /api/join/terms", invited, s.hJoinTerms},
		{"POST /api/join/check", invited, s.hJoinCheck},
		{"DELETE /api/enroll-tokens/{name}", maintainers, s.hRevokeEnrollTokens},
		{"POST /api/devices/{name}/artifact-grants", maintainers, s.hArtifactGrants},
		{"POST /api/artifacts", teamBuilds, s.teamUpload(s.hPutArtifact)},
		{"POST /api/artifacts/uploads", teamBuilds, s.teamUpload(s.hStartUpload)},
		{"PUT /api/artifacts/uploads/{sha}/parts/{n}", teamBuilds, s.teamUpload(s.hPutPart)},
		{"POST /api/artifacts/uploads/{sha}/close", teamBuilds, s.teamUpload(s.hCloseUpload)},
		{"POST /api/mesa/jobs", team, s.hSubmit},
		{"POST /api/jobs", team, s.hSubmit},
		{"GET /api/jobs", scoped, s.hJobs},
		{"GET /api/jobs/{id}", jobNamed, s.hJob},
		{"GET /api/jobs/{id}/wait", jobNamed, s.hWait},
		{"POST /api/jobs/{id}/cancel", team, s.hCancel},
		{"POST /api/jobs/{id}/retry", team, s.hRetry},
		{"POST /api/jobs/{id}/publish", team, s.hPublish},
		{"POST /api/jobs/{id}/exclude", maintainers, s.hExclude},
		{"DELETE /api/jobs/{id}/exclude", maintainers, s.hExclude},
		{"GET /api/jobs/{id}/files/{name...}", jobNamed, s.hJobFile},
		{"GET /api/gate/{kernel}", maintainers, s.hGate},
		{"POST /api/builder/poll", builder, s.hBuilderPoll},
		{"POST /api/builder/builds/{id}/progress", builder, s.hBuilderProgress},
		{"POST /api/builder/builds/{id}/done", builder, s.hBuilderDone},
		{"POST /api/builder/artifacts", builder, s.hBuilderArtifact},
		{"POST /api/builder/artifacts/uploads", builder, s.hStartUpload},
		{"PUT /api/builder/artifacts/uploads/{sha}/parts/{n}", builder, s.hPutPart},
		{"POST /api/builder/artifacts/uploads/{sha}/close", builder, s.hCloseUpload},
		{"GET /api/builder/artifacts/{sha}", builder, s.hBuilderGetArtifact},
		{"GET /api/builder/self", builder, s.hBuilderSelf},
		{"POST /api/builds", teamBuilds, s.hCreateBuild},
		// Releases (releases.go, installed.go). The M team freezes a
		// manifest and a Mac's entry and records the first verdict;
		// Promotion, and every later verdict, stay with Maintainers.
		{"GET /api/installed", team, s.hReleases},
		{"POST /api/installed/{release}/manifest", team, s.hFreezeManifest},
		{"POST /api/installed/{release}/entry/{device}", team, s.hFreezeEntry},
		{"POST /api/installed/{release}/verdict", team, s.hVerdict},
		{"POST /api/installed/{release}/promote", maintainers, s.hPromote},
		{"GET /api/installed/{release}", team, s.hInstalledStatus},
		{"GET /api/builds", maintainers, s.hBuilds},
		{"GET /api/builds/{id}", maintainers, s.hBuild},
		{"GET /api/builds/{id}/log", maintainers, s.hBuildLog},
		{"GET /api/builds/{id}/files/{name}", maintainers, s.hBuildFile},
		{"POST /api/builds/{id}/cancel", maintainers, s.hCancelBuild},
		{"GET /api/builders", maintainers, s.hBuilders},
		{"POST /api/builders", maintainers, s.hAddBuilder},
		{"DELETE /api/builders/{name}", maintainers, s.hRemoveBuilder},
		{"POST /api/devices/{name}/exec", team, s.hExec},
		{"GET /api/devices/{name}/audit", macNamed, s.hAudit},
		{"GET /api/devices/{name}/execs", macNamed, s.hExecRecord},
		{"GET /api/maintainers", maintainers, s.hMaintainersGone},
		{"POST /api/maintainers", maintainers, s.hMaintainersGone},
		{"DELETE /api/maintainers/{name}", maintainers, s.hMaintainersGone},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, code int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), code)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// hAuth tells a caller who it is to labd. Signed in with a browser, or sent
// with a Lab token, it names the person, their kind and whether they are a
// Maintainer. With neither, and while labd knows nobody any other way
// (Config.Person), there is nothing to say: labd answers 404, or 401 to a
// token it does not know, which is what lab login reads.
func (s *Server) hAuth(w http.ResponseWriter, r *http.Request) {
	if u := tokenUseOf(r); u != nil {
		if u.Blocked {
			httpErr(w, 403, "%s", blockedSentence)
			return
		}
		if u.expired {
			s.noCredential(w, r)
			return
		}
		role := map[bool]string{true: "admin"}[u.isMaintainer()]
		writeJSON(w, map[string]any{"trusted": u.isMaintainer(), "token_required": false, "role": role,
			"login": u.Handle, "kind": u.Kind(), "account_id": u.AccountID})
		return
	}
	if si := signedInOf(r); si != nil {
		role := map[bool]string{true: "admin"}[si.isMaintainer()]
		writeJSON(w, map[string]any{"trusted": si.isMaintainer(), "token_required": !si.isMaintainer(), "role": role,
			"login": si.Handle, "kind": si.Kind(), "account_id": si.AccountID})
		return
	}
	if s.cfg.Person == nil {
		if r.Header.Get("Authorization") != "" {
			s.noCredential(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	a, who := s.access(r)
	role := map[access]string{viewAccess: "viewer", adminAccess: "admin"}[a]
	writeJSON(w, map[string]any{"trusted": a == adminAccess, "token_required": a == noAccess, "role": role, "login": who})
}

type ctxKey struct{}

func (s *Server) agent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := hashSecret(bearer(r))
		for _, v := range s.allDevs() {
			v.mu.Lock()
			ok := subtle.ConstantTimeCompare([]byte(hash), []byte(v.secretHash)) == 1
			v.mu.Unlock()
			if ok {
				noteCaller(r, "mac:"+v.d.Name)
				h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, v)))
				return
			}
		}
		if s.ended(w, r, bearer(r)) {
			return
		}
		s.refuse(w, r, 401, "unknown device secret")
	}
}

func agentDev(r *http.Request) *dev { return r.Context().Value(ctxKey{}).(*dev) }

var reName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func (s *Server) hEnrollToken(w http.ResponseWriter, r *http.Request) {
	var req api.EnrollTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if !reName.MatchString(req.Name) {
		httpErr(w, 400, "device name must be lowercase letters, digits and dashes")
		return
	}
	// Nobody enrolls a Mac in another person's name: a Volunteer makes the
	// Join command for their own Mac, signed in, on "Lend a Mac".
	if req.Volunteer || req.VolunteerName != "" || req.Contact != "" || req.VolunteerLogin != "" || req.VolunteerID != 0 {
		httpErr(w, 400, "lab enroll is for Lab-owned Macs only: a Volunteer's Mac is lent by its own Volunteer, signed in, from Lend a Mac (%s)", s.publicBase(r)+"/lend")
		return
	}
	// Activation is one-time, so a Mac that enrolled but never checked in
	// still holds its name: a new token for it would only be refused.
	if v := s.dev(req.Name); v != nil {
		if v.checkedIn() {
			httpErr(w, 409, "device %s already enrolled", req.Name)
		} else {
			httpErr(w, 409, "device %s enrolled but never checked in: `lab forget %s`, then enroll it again", req.Name, req.Name)
		}
		return
	}
	if req.Model == "" {
		req.Model = api.ModelLaptop
	}
	var by int64
	if p := credentialOf(r); p != nil {
		by = p.AccountID
	}
	tok := "enr_" + randHex(16)
	now := s.now()
	exp := now.Add(joinCommandTTL)
	if _, err := s.store.addJoinCommandAsked(hashSecret(tok), req, by, askerOf(r), now, exp); errors.Is(err, errNameHeld) {
		httpErr(w, 409, "a Join command for a Mac called %s is waiting already: names are unique", req.Name)
		return
	} else if errors.Is(err, errAskerCut) {
		s.refuse(w, r, 401, "%v", err)
		return
	} else if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.saidOfJoinCommand(r, "made a Join command for "+req.Name)
	s.labGroup.JoinCommandMade(req.Name, maintainerName(r))
	writeJSON(w, api.EnrollTokenResponse{Name: req.Name, Token: tok, Expires: exp, JoinCommand: s.joinCommand(r, tok)})
}

// hRevokeEnrollTokens deletes the enrollment tokens for a name, used or not:
// a Join command that hasn't been used stops working, and a used one can't
// be used again. A Mac that already enrolled with one stays until forgotten.
func (s *Server) hRevokeEnrollTokens(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	unused, _ := s.store.unusedTokensFor(name, s.now())
	n, err := s.store.revokeTokens(name)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	s.log.Printf("Maintainer %s revoked %d enrollment token(s) for %s", maintainerName(r), n, name)
	if unused > 0 {
		s.saidOfJoinCommand(r, "cancelled the Join command for "+name)
		s.labGroup.JoinCommandCancelled(name, maintainerName(r))
	}
	writeJSON(w, map[string]any{"name": name, "revoked": n})
}

// hVolunteerGone answers the two routes of the retired ./volunteer.
func (s *Server) hVolunteerGone(w http.ResponseWriter, r *http.Request) {
	httpErr(w, http.StatusGone, "./volunteer is retired: sign in at %s and open Lend a Mac", s.publicBase(r))
}

// Agents' requests are small, except for command results. Real ones stay far
// below these limits: an enrollment or a check-in carries the Mac's facts, a
// few KB, which every viewer downloads again with /api/devices. A report
// carries command results, the largest an exec's output (1 MB per stream,
// which JSON's escapes can make 6 MB), a journal of up to 20000 lines or the
// kernel's .config, and up to 500 kernel events. Files go to
// /api/agent/upload and /api/agent/screen, which these don't limit.
// enrollBodyTimeout is how long an enrollment's body may take to arrive;
// tests shorten it.
var enrollBodyTimeout = 30 * time.Second

const (
	enrollBodyMax  = 256 << 10
	checkinBodyMax = 256 << 10
	reportBodyMax  = 64 << 20
)

// decodeBody decodes r's JSON body into v. A body over max bytes gets a 413,
// unread if its Content-Length says so, and a malformed one a 400;
// decodeBody answers the request then, and returns false.
func decodeBody(w http.ResponseWriter, r *http.Request, max int64, v any) bool {
	if r.ContentLength > max {
		httpErr(w, 413, "a request body of %d bytes is over labd's limit of %d", r.ContentLength, max)
		return false
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, max)).Decode(v)
	if err == nil {
		return true
	}
	code := 400
	if errors.As(err, new(*http.MaxBytesError)) {
		code = 413
	}
	httpErr(w, code, "%v", err)
	return false
}

var reNameRun = regexp.MustCompile(`[^a-z0-9]+`)

// hJoinAgent hands out the lab-agent built with this labd.
func (s *Server) hJoinAgent(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(s.cfg.AgentBinary)
	if err != nil {
		httpErr(w, 404, "this lab has no lab-agent to hand out: build it with make, next to labd")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "lab-agent", st.ModTime(), f)
}

func (s *Server) tokenValid(hash string) bool {
	_, err := s.store.token(hash, s.now())
	return err == nil
}

// refusedToken answers a request whose enrollment token labd does not
// take, and logs it. A refused token is never posted to a group: anyone on
// the internet can produce one. It counts with the other refused
// credentials (refused.go): past the limit the answer is 429.
func (s *Server) refusedToken(w http.ResponseWriter, r *http.Request, code int, why string) {
	s.log.Printf("refused an enrollment token on %s %s: %s", r.Method, r.URL.Path, why)
	if s.tooManyRefused(w, r) {
		return
	}
	httpErr(w, code, "%s", why)
}

const (
	tokenRefused   = "enrollment token unknown, used or expired"
	tokenOtherMacs = "this Join command was first used on another Mac, and serves that Mac only: make a new one on Lend a Mac"
)

// hEnroll turns an enrollment token into a device. It needs no secret, so
// its body is held to enrollBodyMax before the token is even looked at.
//
// Activation is one-time: the first Mac to use a token binds it, and the
// first enrollment uses it up, for the activation key that Mac sent. Should
// the answer never reach the Mac, only that same activation key enrolls
// again with the token, and only until the Mac's first check-in: the token
// alone, copied to another Mac, is worth nothing once used. The owner must
// have accepted the lab's current terms. A Blocked Volunteer, a cap reached
// and a name taken are refused here, with the reason.
func (s *Server) hEnroll(w http.ResponseWriter, r *http.Request) {
	// Anyone may send this body, before labd knows who they are: it must
	// arrive soon, not trickle in to hold the connection.
	readBodyBy(w, r.Context(), time.Now().Add(enrollBodyTimeout))
	var req api.EnrollRequest
	if !decodeBody(w, r, enrollBodyMax, &req) {
		return
	}
	tokHash := hashSecret(req.Token)
	t, err := s.store.tokenState(tokHash, s.now())
	if err != nil {
		s.refusedToken(w, r, 403, tokenRefused)
		return
	}
	if _, sum := s.terms(); req.TermsSHA256 != sum {
		httpErr(w, 409, "this Mac's owner hasn't accepted the lab's current terms: lab-agent setup shows them")
		return
	}
	keyHash := ""
	if req.ActivationKey != "" {
		if len(req.ActivationKey) < 32 || len(req.ActivationKey) > 128 {
			httpErr(w, 400, "activation key: want 32 to 128 characters")
			return
		}
		keyHash = hashSecret(req.ActivationKey)
	}
	secret := "dev_" + randHex(24)
	if t.ActivatedBy != "" {
		s.enrollAgain(w, r, &t.EnrollTokenRequest, tokHash, keyHash, t.ActivatedBy, secret)
		return
	}
	if keyHash == "" { // an older lab-agent: no enrolling again with this token
		if t.BoundTo != "" {
			s.refusedToken(w, r, 403, tokenOtherMacs)
			return
		}
		keyHash = "none-" + randHex(16)
	} else if mine, err := s.store.bindToken(tokHash, keyHash); err != nil || !mine {
		s.refusedToken(w, r, 403, tokenOtherMacs)
		return
	}
	if err := s.mayEnroll(t); err != nil {
		s.refusedEnrollment(w, t.Name, err)
		return
	}
	d := api.Device{ID: "d-" + randHex(6), Name: t.Name, Label: t.Label, Model: t.Model, VolunteeredBy: t.VolunteeredBy, LinkedLogin: t.VolunteerLogin,
		State: api.StateNew, StateReason: "enrolled; run a baseline job", Facts: req.Facts, Enrollment: enrollment(&t.EnrollTokenRequest, api.Enrollment{}),
		TermsSHA256: req.TermsSHA256, TermsAccepted: s.now()}
	d.Volunteer = d.IsVolunteer()
	d.FirstBaseline = &api.FirstBaseline{}
	if d.IsVolunteer() {
		d.AccessLevel = api.AccessQualification // until its Volunteer chooses
	}
	v := newDev(s, d, hashSecret(secret))
	v.enrolledWith = tokHash
	s.mu.Lock()
	if _, dup := s.devs[d.Name]; dup {
		s.mu.Unlock()
		s.refusedEnrollment(w, d.Name, s.mayEnroll(t))
		return
	}
	// Used up here, under s.mu, in one transaction with the device's row: of
	// two enrollments racing with one token, one gets the Mac and the other
	// a 403, and a crash leaves no used token without its Mac.
	if ok, err := s.store.activateAndSave(tokHash, keyHash, s.now(), &d, hashSecret(secret)); err != nil || !ok {
		s.mu.Unlock()
		s.refusedToken(w, r, 403, tokenRefused)
		return
	}
	s.devs[d.Name] = v
	s.mu.Unlock()
	go s.deviceLoop(v)
	s.log.Printf("enrolled %s (%s, %s); terms %s accepted", d.Name, d.Kind, req.Facts.DTModel, short(req.TermsSHA256))
	s.joined(v)
	writeJSON(w, api.EnrollResponse{DeviceID: d.ID, Name: d.Name, Secret: secret, AgentEnrollment: s.agentEnrollment(d.Enrollment)})
}

// refusedEnrollment answers an enrollment the lab refuses with the reason,
// and leaves the Mac as it was.
func (s *Server) refusedEnrollment(w http.ResponseWriter, name string, err error) {
	var no *refusal
	if !errors.As(err, &no) {
		s.log.Printf("enrolling %s: %v", name, err)
		no = &refusal{http.StatusConflict, fmt.Sprintf("Name taken: a Mac called %s is in the lab already. Make a new Join command with another name.", name)}
	}
	s.log.Printf("refused to enroll %s: %s", name, no.msg)
	httpErr(w, no.code, "%s", no.msg)
}

// enrollAgain answers an enrollment with a used token: the same activation
// enrolling again before its Mac's first check-in, because the first answer
// never reached it. The Mac keeps its enrollment and gets a new secret.
// Anything else is refused as an unknown token would be.
func (s *Server) enrollAgain(w http.ResponseWriter, r *http.Request, t *api.EnrollTokenRequest, tokHash, keyHash, activatedBy, secret string) {
	v := s.dev(t.Name)
	if keyHash == "" || subtle.ConstantTimeCompare([]byte(keyHash), []byte(activatedBy)) != 1 || v == nil {
		s.refusedToken(w, r, 403, tokenRefused)
		return
	}
	v.mu.Lock()
	ok := !v.forgotten && v.d.BootID == "" && v.enrolledWith == tokHash
	if ok {
		v.secretHash = hashSecret(secret)
		v.grants, v.fetchGrant = nil, nil
		v.saveLocked()
	}
	d := v.d
	v.mu.Unlock()
	if !ok {
		s.refusedToken(w, r, 403, tokenRefused)
		return
	}
	s.log.Printf("enrolled %s again with its own activation, before its first check-in", t.Name)
	writeJSON(w, api.EnrollResponse{DeviceID: d.ID, Name: d.Name, Secret: secret, AgentEnrollment: s.agentEnrollment(d.Enrollment)})
}

// enrollment is what token t makes a Mac. A Volunteer's Mac enrolled again
// before its first check-in keeps the topic it was given (old).
func enrollment(t *api.EnrollTokenRequest, old api.Enrollment) api.Enrollment {
	if !t.Volunteer {
		return api.Enrollment{Kind: api.KindLabOwned}
	}
	e := api.Enrollment{Kind: api.KindVolunteer, VolunteerName: t.VolunteerName, VolunteerID: t.VolunteerID, Contact: t.Contact, NtfyTopic: old.NtfyTopic, NtfyOwnURL: old.NtfyOwnURL}
	if e.NtfyTopic == "" {
		e.NtfyTopic = newNtfyTopic()
	}
	return e
}

// newNtfyTopic is an unguessable ntfy topic for one Volunteer Mac: about 106
// random bits, in digits only so that no topic spells a word or a name.
func newNtfyTopic() string {
	n, _ := rand.Int(rand.Reader, new(big.Int).Exp(big.NewInt(10), big.NewInt(32), nil))
	return fmt.Sprintf("maclab-%032d", n)
}

// macTopicURL is where a Volunteer Mac's own notices are posted: the topic
// the Volunteer chose, else the Mac's generated one on the lab's ntfy server.
func (s *Server) macTopicURL(e api.Enrollment) string { return s.agentEnrollment(e).NtfyURL }

// agentEnrollment is e as the Mac is told it, with its topic's URL on the
// lab's ntfy server (the one --ntfy posts to).
func (s *Server) agentEnrollment(e api.Enrollment) api.AgentEnrollment {
	ae := api.AgentEnrollment{Enrollment: e}
	if e.NtfyOwnURL != "" {
		ae.NtfyURL = e.NtfyOwnURL
		return ae
	}
	if u, err := url.Parse(s.cfg.NtfyURL); err == nil && e.NtfyTopic != "" && u.Host != "" {
		ae.NtfyURL = (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/" + e.NtfyTopic}).String()
	}
	return ae
}

// hAgentEnrollment tells a Mac what it is enrolled as, for `lab-agent status`.
func (s *Server) hAgentEnrollment(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	v.mu.Lock()
	e, waits, name, refused := v.d.Enrollment, v.termsWait, v.d.Name, v.d.NoticeTopicRefused
	v.mu.Unlock()
	ae := s.agentEnrollment(e)
	if waits {
		ae.TermsWaiting, ae.MyMacs = s.termsText().Date, s.myMacsLink(name)
	}
	ae.NoticeTopicRefused = refused
	writeJSON(w, ae)
}

func (s *Server) hCheckin(w http.ResponseWriter, r *http.Request) {
	var ci api.Checkin
	if !decodeBody(w, r, checkinBodyMax, &ci) {
		return
	}
	v := agentDev(r)
	v.heartbeat(ci)
	// Long poll: hold the check-in until there is a command for this boot.
	hold, _ := strconv.Atoi(r.URL.Query().Get("wait_ms"))
	wait := min(time.Duration(hold)*time.Millisecond, s.cfg.Interval)
	if wait > 0 {
		v.waitFor(r.Context(), wait, func() bool { return len(v.pending) > 0 || v.d.BootID != ci.BootID })
	}
	resp := v.deliver(ci.BootID)
	resp.AuditHave = v.auditHaveNow()
	writeJSON(w, resp)
}

func (s *Server) hReport(w http.ResponseWriter, r *http.Request) {
	var rep api.Report
	if !decodeBody(w, r, reportBodyMax, &rep) {
		return
	}
	forgotten, wanted := agentDev(r).report(rep)
	agentDev(r).copyAudit(rep.Audit)
	writeJSON(w, api.ReportReply{Forgotten: forgotten, Proto: 1, Wanted: wanted})
}

var reSHA = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Server) artifactPath(sha string) string {
	return filepath.Join(s.cfg.DataDir, "artifacts", sha)
}

func (s *Server) hPutArtifact(w http.ResponseWriter, r *http.Request) {
	sha, n, err := s.storeArtifact(r.Body)
	if err != nil {
		s.artifactErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"sha256": sha, "size": n})
}

// storeArtifact saves content under its sha256.
func (s *Server) storeArtifact(r io.Reader) (string, int64, error) {
	return storeArtifactIn(filepath.Join(s.cfg.DataDir, "artifacts"), r, artifactDiskOps())
}

// cleanName keeps uploaded file names inside the job directory.
func cleanName(name string) (string, bool) {
	c := filepath.ToSlash(filepath.Clean("/" + name))[1:]
	if c == "" || strings.HasPrefix(c, "..") {
		return "", false
	}
	return c, true
}

// DeviceView is a device plus what labd derives about it.
type DeviceView struct {
	api.Device
	Tier     int       `json:"tier"`
	Alive    bool      `json:"alive"`
	Queued   int       `json:"queued"`
	ScreenAt time.Time `json:"screen_at,omitempty"`
	// Cable is what the lab knows of the cable that joins a Lab-owned Mac
	// to the lab server; nil for a Volunteer's Mac.
	Cable *api.Cable `json:"cable,omitempty"`
	// VolunteerHandle is the handle, as it is now, of the person who lends
	// the Mac; "" for a Lab-owned Mac and on the stream.
	VolunteerHandle string `json:"volunteer_handle,omitempty"`
	// Arrival is the stage a Volunteer's Mac has reached since it Joined:
	// one of the Arrival words; "" for a Lab-owned Mac.
	Arrival string `json:"arrival,omitempty"`
	// LabAgent is the number of the lab's lab-agent when the Mac's is
	// older; "" when it is not.
	LabAgent string `json:"lab_agent,omitempty"`
	// TermsWaiting is whether the Mac takes nothing from the lab until its
	// Volunteer accepts the lab's changed terms on My Macs.
	TermsWaiting bool `json:"terms_waiting,omitempty"`
}

func (s *Server) view(v *dev) DeviceView {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.viewLocked()
}

// hDevices lists the Macs the caller sees.
func (s *Server) hDevices(w http.ResponseWriter, r *http.Request) {
	c := callerOf(r)
	out := []DeviceView{}
	for _, v := range s.allDevs() {
		if s.sees(c, v.d.Name) {
			out = append(out, s.viewFor(c, v))
		}
	}
	writeJSON(w, out)
}

func (s *Server) devOr404(w http.ResponseWriter, r *http.Request) *dev {
	v := s.dev(r.PathValue("name"))
	if v == nil {
		httpErr(w, 404, "no device %q", r.PathValue("name"))
	}
	return v
}

func (s *Server) hDevice(w http.ResponseWriter, r *http.Request) {
	if v := s.devOr404(w, r); v != nil {
		writeJSON(w, struct {
			DeviceView
			Events []api.KernelEvent `json:"recent_events"`
		}{s.viewFor(callerOf(r), v), v.eventsSince(s.now().Add(-24 * time.Hour))})
	}
}

func (s *Server) hPatchDevice(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	var p api.DevicePatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if p.OOB != nil {
		httpErr(w, 400, "a Mac's record names no controller and no port: the lab learns which cable a Lab-owned Mac is on by a Sighting, or by a Proving reset (`lab reset <mac> --prove left|middle|right`)")
		return
	}
	link := ""
	if p.VolunteerLogin != nil && *p.VolunteerLogin != "" {
		var err error
		if link, err = volunteerLogin(*p.VolunteerLogin); err != nil {
			httpErr(w, 400, "%v", err)
			return
		}
	}
	v.mu.Lock()
	if p.VolunteerLogin != nil {
		// Checked before anything changes: a refused link leaves the Mac as it was.
		switch {
		case !v.d.IsVolunteer():
			v.mu.Unlock()
			httpErr(w, 409, "%s is a Lab-owned Mac: only a Volunteer's Mac is linked to a login", v.d.Name)
			return
		case v.volunteeredBy != "":
			v.mu.Unlock()
			httpErr(w, 409, "%s is %s's, who volunteered it with ./volunteer: it can't be linked to another login", v.d.Name, v.volunteeredBy)
			return
		}
		if old := v.d.LinkedLogin; old != link {
			s.log.Printf("Maintainer %s linked %s to %q (was %q)", maintainerName(r), v.d.Name, link, old)
		}
		v.d.LinkedLogin = link
	}
	if p.Label != nil {
		v.d.Label = *p.Label
	}
	if p.Model != nil {
		v.d.Model = *p.Model
	}
	if p.NoOOB {
		v.d.OOB = nil
	}
	v.saveLocked()
	v.mu.Unlock()
	writeJSON(w, s.view(v))
}

// hForgetDevice takes a Mac out of the lab: after `lab-agent uninstall`, or so
// it can enroll again under the same name. Its jobs stay in the history. A
// Mac with a job running, queued, or unfinished in the store is refused. So,
// unless ?force=1, is one someone other than ?holder= holds a lease on
// (forgetting it would end the lease and cut their lab exec), and one whose
// agent still checks in.
func (s *Server) hForgetDevice(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	force, _ := strconv.ParseBool(r.URL.Query().Get("force"))
	if code, err := s.forget(v, leaser(r), r.URL.Query().Get("holder"), force); err != nil {
		httpErr(w, code, "%v", err)
		return
	}
	s.log.Printf("forgot %s", r.PathValue("name"))
	writeJSON(w, map[string]string{"forgot": r.PathValue("name")})
}

// dropNamedState clears what labd keeps under a forgotten Mac's name, since
// the name can be taken again by another Mac, or another person's: its
// screens, serial log and baselines go; its Audit log is kept, renamed after
// the enrollment as it leaves the fleet (retireAudit), for Maintainers. Its
// jobs stay, for Maintainers only (seesJob).
func (s *Server) dropNamedState(name string) {
	for _, p := range []string{s.screenDir(name), s.serialPath(name), s.baselinePath(name), s.omtBaselinePath(name)} {
		if err := os.RemoveAll(p); err != nil {
			s.log.Printf("forget %s: %v", name, err)
		}
	}
}

// forget takes v out of the fleet as a Maintainer (by) ending its
// enrollment does (endEnrollment): its Lease ends with its line, its device
// loop stops, its record is Retired and its old secret learns who ended it.
// A lease held by holder doesn't count; force skips the lease and
// live-agent checks, never the job check. A job queued between the checks
// and the ending is canceled by the ending.
func (s *Server) forget(v *dev, by Person, holder string, force bool) (int, error) {
	code, err := s.mayForget(v, holder, force)
	if err != nil {
		return code, err
	}
	switch err := s.endEnrollment(v, leaving{how: leftByMaintainer, by: by}); {
	case errors.Is(err, errMacGone):
		return 404, fmt.Errorf("no device %q", v.snapshot().Name)
	case err != nil:
		return statusOr500(err), err
	}
	close(v.gone)
	return 200, nil
}

// mayForget is why v may not be forgotten now, with its status, or nil.
func (s *Server) mayForget(v *dev, holder string, force bool) (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	name := v.d.Name
	if v.forgotten {
		return 404, fmt.Errorf("no device %q", name)
	}
	// A job queued while the Mac was offline waits in the store, not the queue,
	// and labd would hand it to whatever enrolls next under this name.
	open, err := s.store.jobs(name, 1, true)
	if err != nil {
		return 500, err
	}
	l := v.liveLeaseLocked()
	leased := l != nil
	if leased && len(v.parked) > 0 {
		return 409, fmt.Errorf("%s has %d queued jobs set aside by %s's lease until %s: end the lease (`lab release %s --force`) so they run, or cancel them, first",
			name, len(v.parked), l.Who(), l.Expires.Format(time.DateTime), name)
	}
	if v.d.State == api.StateBusy || v.d.State == api.StateRecovering || len(v.queue) > 0 || len(open) > 0 {
		return 409, fmt.Errorf("%s has a job running or queued; cancel it first", name)
	}
	if v.resetting {
		// it would go to a retired Mac
		return 409, fmt.Errorf("a Hard reset of %s is on its way: forget the Mac once it has gone out and the Mac is back", name)
	}
	if !force && leased && l.Holder != holder {
		return 409, fmt.Errorf("%s is leased by %s until %s: release the lease first (`lab release %s`, or `lab release %s --force` if it is another Maintainer's), or pass --force",
			name, l.Who(), l.Expires.Format(time.DateTime), name, name)
	}
	// An agent that told labd it is leaving may still check in until
	// uninstall has stopped it. Any other one that checks in is still set up,
	// and would only get 401s once its row is gone.
	if seen := v.d.LastSeen; !force && v.d.State != api.StateLeft && !seen.IsZero() && s.since(seen) < s.cfg.OfflineAfter {
		return 409, fmt.Errorf("%s's lab-agent checked in %s ago, so it is still set up: run `sudo lab-agent uninstall` on the Mac first, or pass --force",
			name, s.since(seen).Round(time.Second))
	}
	return 200, nil
}

// resetWait is how long a person's Hard reset may wait its turn at the
// Out-of-band controller, behind another port's.
const resetWait = 40 * time.Second

// hReset is a person's Hard reset of a Lab-owned Mac, or, with ?prove=, a
// Maintainer's Proving reset of the port named. The Hard reset goes out
// before the answer does, so a refusal says why none was sent; the rest of
// the recovery ladder (a second one, then a person) follows by itself.
//
// A Mac that left the lab (lab-agent uninstall) is put back in it first: a
// reset is how a Maintainer recovers one that wedged mid-uninstall, or whose
// agent still runs after an uninstall that failed. Only automatic recovery
// leaves a Mac that left alone.
func (s *Server) hReset(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	p := leaser(r)
	if port := r.URL.Query().Get("prove"); port != "" {
		s.hProve(w, r, v, p, port)
		return
	}
	said, err := s.hardResetBy(r.Context(), v, p)
	if err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	io.WriteString(w, said+"\n")
}

// hardResetBy is p's Hard reset of v's Mac: allowed to a Maintainer and to
// the holder of a Lease on it, never on a Volunteer's Mac, and only where
// the lab can place the Mac. It returns what to tell p once it is sent.
func (s *Server) hardResetBy(ctx context.Context, v *dev, p Person) (said string, err error) {
	// A reset restarts the Mac, so a Lease fences it like a job.
	if err := s.fence(v, p, fenceReset); err != nil {
		return "", err
	}
	v.mu.Lock()
	name, volunteers, noReset := v.d.Name, v.d.IsVolunteer(), v.noHardResetLocked()
	v.mu.Unlock()
	switch {
	case volunteers:
		return "", refuse(http.StatusForbidden, "no Hard reset was sent: %s", noReset)
	case noReset != "":
		return "", refuse(s.cables.status(), "no Hard reset was sent: %s", noReset)
	}
	why := "Hard reset by " + orStr(p.Handle, "a Maintainer")
	back, err := s.rejoin(v, why)
	if err != nil {
		return "", refuse(http.StatusNotFound, "%v", err)
	}
	// One at a time: a second press sends no second reset while the first
	// is on its way, nor while the lab waits for it to bring the Mac back.
	v.mu.Lock()
	prev, reason := v.d.State, v.d.StateReason
	switch {
	case prev == api.StateBusy:
		v.mu.Unlock()
		return "", refuse(http.StatusConflict, "%s is busy; cancel its job instead", name)
	case prev == api.StateRecovering:
		v.mu.Unlock()
		return "", refuse(http.StatusConflict, "%s is being recovered already (%s)", name, reason)
	case v.resetting:
		v.mu.Unlock()
		return "", refuse(http.StatusConflict, "a Hard reset of %s is on its way already", name)
	case v.goneWhy != "":
		v.mu.Unlock()
		return "", refuse(http.StatusNotFound, "no Hard reset was sent: %s", v.goneWhy)
	case v.forgotten:
		v.mu.Unlock()
		return "", refuse(http.StatusNotFound, "no Hard reset was sent: %s is leaving the lab", name)
	}
	v.resetting = true
	v.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, s.cfg.ResetWait)
	go s.fenceWhileWaiting(rctx, cancel, v, p)
	// The Mac as it is when the controller sends the reset: one that
	// restarted by itself while the reset waited is not taken as back.
	sent := sentReset{prev: prev}
	var mark sync.Mutex
	dispatched := false
	rctx = oob.OnSending(rctx, func() {
		mark.Lock()
		defer mark.Unlock()
		sent.at, sent.boot, dispatched = s.now(), v.snapshot().BootID, true
	})
	sent.port, err = v.resetOOB(rctx)
	cancel()
	mark.Lock()
	if !dispatched {
		sent.at, sent.boot = s.now(), v.snapshot().BootID
	}
	mark.Unlock()
	sent.maybe = err != nil && resetMayHaveGone(err)
	v.mu.Lock()
	v.resetting = false
	if err == nil || sent.maybe {
		v.setStateLocked(api.StateRecovering, why)
	}
	v.mu.Unlock()
	if err == nil || sent.maybe {
		go func() {
			steps, ok := v.climb(s.ctx, why, s.cfg.BootTimeout, false, &sent)
			s.log.Printf("%s: %s: ok=%v steps=%v", name, why, ok, steps)
		}()
	}
	if err != nil {
		if ferr := s.fence(v, p, fenceReset); ferr != nil {
			return "", ferr
		}
		if sent.maybe {
			return "", fmt.Errorf("%w; the lab watches %s as after a reset", err, name)
		}
		return "", err
	}
	if back {
		said = name + " had left the lab (lab-agent uninstall); the reset puts it back in. "
	}
	return said + "Hard reset sent to " + name + " through the " + sent.port + " port; watch `lab status " + name + "`", nil
}

// fenceWhileWaiting calls off p's reset of v, through cancel, as soon as the
// Lease would refuse it, for as long as it waits its turn at the controller
// (ctx). Once the controller is sending it, cancel stops nothing.
func (s *Server) fenceWhileWaiting(ctx context.Context, cancel context.CancelFunc, v *dev, p Person) {
	tick := s.clock().NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		if s.fence(v, p, fenceReset) != nil {
			cancel()
			return
		}
		v.mu.Lock()
		ch := v.changed
		v.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-tick.C():
		}
	}
}

// hProve is a Proving reset, which is aimed at a port and not at a Mac:
// Maintainers only.
func (s *Server) hProve(w http.ResponseWriter, r *http.Request, v *dev, p Person, port string) {
	rid, ok := oob.RIDOf(port)
	switch {
	case !p.isMaintainer():
		httpErr(w, http.StatusForbidden, "a Proving reset is a Maintainer's: it is aimed at a port, not at a Mac")
		return
	case !ok:
		httpErr(w, http.StatusBadRequest, "no port %q: the lab server's ports are left, middle and right, facing its back", port)
		return
	}
	if err := s.fence(v, p, fenceReset); err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	s.held(w, r, func() (any, error) { return s.prove(v, rid, p.Handle) })
}

func (s *Server) hSubmit(w http.ResponseWriter, r *http.Request) {
	var spec api.JobSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		httpErr(w, 400, "%v", err)
		return
	}
	if r.URL.Path == "/api/mesa/jobs" && spec.Mesa == nil {
		httpErr(w, 400, "Mesa endpoint requires an explicit Mesa package request")
		return
	}
	j, code, err := s.submit(leaser(r), spec, "")
	if err != nil {
		httpErr(w, code, "%v", err)
		return
	}
	writeJSON(w, j)
}

// Submit validates and queues a job that labd queues itself, for no Maintainer.
func (s *Server) Submit(spec api.JobSpec) (*api.Job, int, error) {
	return s.submit(Person{}, spec, "")
}

// Retry queues a finished job again as a new job: the same Mac, kernel,
// tests and cmdline, recording which job it retries. holder, if given,
// replaces the first job's, so a lease is checked against whoever retries
// it. A source job boots the kernel the first one built; once that is
// collected, it builds again from the commit and the config that build
// recorded, never from wherever a branch points now or the Mac's config as
// it is now.
func (s *Server) Retry(id, holder string) (*api.Job, int, error) {
	return s.retry(Person{}, id, holder)
}

// retry is Retry for the person who asked.
func (s *Server) retry(by Person, id, holder string) (*api.Job, int, error) {
	old, err := s.store.job(id)
	if err != nil {
		return nil, 404, fmt.Errorf("no job %s", id)
	}
	if old.State != api.JobDone {
		return nil, 409, fmt.Errorf("job %s is still %s; only a finished job can be retried", id, old.State)
	}
	spec := old.Spec
	spec.Tests = slices.Clone(spec.Tests)
	spec.CmdlineStrip = slices.Clone(spec.CmdlineStrip)
	spec.OMTAllow = slices.Clone(spec.OMTAllow)
	if holder != "" {
		spec.Holder = holder
	}
	if spec.Source != "" && spec.Kernel != "" {
		// boot the kernel the first job built, unless it has been collected
		if _, err := os.Stat(s.artifactPath(spec.Kernel)); err != nil {
			spec.Kernel = ""
		}
	}
	var notes []string
	if spec.Source != "" && spec.Kernel == "" {
		src, err := s.pinnedSource(old)
		if err != nil {
			return nil, 409, err
		}
		cfg, note, err := s.pinnedConfig(old)
		if err != nil {
			return nil, 409, err
		}
		spec.Source, spec.Build, spec.Config = src, "", cfg
		if note != "" {
			notes = append(notes, note)
		}
	}
	return s.submit(by, spec, old.ID, notes...)
}

// pinnedConfig is the config a retry of old builds its kernel again with: the
// one the first build used, as a config artifact, so the retry builds the
// same kernel and not one from the Mac's config as it is by now. A recorded
// config that has been collected since is refused rather than replaced: a
// build from another config is another kernel, which the retry would pass
// off as the first job's. note, if any, says where the config came from.
func (s *Server) pinnedConfig(old *api.Job) (cfg, note string, err error) {
	again := fmt.Sprintf("run it afresh with `lab run %s --kernel %s`", old.Spec.Device, old.Spec.Source)
	cfg = old.Spec.Config
	switch {
	case cfg != "":
		// built with an uploaded config, which the build request named
	case old.Spec.Build == "":
		// the first job never got as far as a build, so there is no config to keep
		return "", fmt.Sprintf("job %s recorded no build, so this one builds with %s's config as it is now", old.ID, old.Spec.Device), nil
	default:
		b, err := s.getBuild(old.Spec.Build)
		if err != nil || b.ConfigSHA == "" {
			return "", "", fmt.Errorf("job %s's build %s recorded no config, so a retry would build with %s's config as it is now; %s", old.ID, old.Spec.Build, old.Spec.Device, again)
		}
		cfg = b.ConfigSHA
		note = fmt.Sprintf("builds with the config build %s used (%s), not %s's as it is now", b.ID, short(cfg), old.Spec.Device)
	}
	if _, err := os.Stat(s.artifactPath(cfg)); err != nil {
		return "", "", fmt.Errorf("job %s's kernel was built with config %s, which has been collected since, so a retry can't build the same kernel; %s", old.ID, short(cfg), again)
	}
	return cfg, note, nil
}

// pinnedSource is the source a retry of old builds from: the exact commit its
// build resolved, as repo#sha. Its source may name a branch or pull request
// that has moved on since.
func (s *Server) pinnedSource(old *api.Job) (string, error) {
	if src, err := s.source(old.Spec.Source); err == nil && reHex40.MatchString(src.Ref) {
		return old.Spec.Source, nil // a commit already
	}
	again := fmt.Sprintf("run it afresh with `lab run %s --kernel %s`", old.Spec.Device, old.Spec.Source)
	if old.Spec.Build == "" {
		return "", fmt.Errorf("job %s never resolved %s to a commit, so a retry would build whatever it points to now; %s", old.ID, old.Spec.Source, again)
	}
	b, err := s.getBuild(old.Spec.Build)
	if err != nil || b.Source.SHA == "" || b.Source.Repo == "" {
		return "", fmt.Errorf("job %s's build %s recorded no commit, so a retry would build whatever %s points to now; %s", old.ID, old.Spec.Build, old.Spec.Source, again)
	}
	return b.Source.Repo + "#" + b.Source.SHA, nil
}

const watchdogRequireMinAgent = "0.8.1"

func validateRequiredWatchdog(tests []api.TestSpec, d api.Device) error {
	for _, t := range tests {
		if t.RequireWatchdog && !versionAtLeast(d.Facts.AgentVersion, watchdogRequireMinAgent) {
			return fmt.Errorf("%s runs lab-agent %s; test %s requires watchdog support from %s or later",
				d.Name, orStr(d.Facts.AgentVersion, "(unknown)"), t.Name, watchdogRequireMinAgent)
		}
	}
	return nil
}

// submit queues a job for the person who asked, a Maintainer or an M team
// member, or for labd itself (nobody); retryOf names the job it retries,
// whose source may come with the kernel already built from it, and notes go
// in the job's events after the first. The job records what lab gate needs
// to know about its Mac as it is now. An M team member's job needs their
// own Lease on the Mac, and is never a baseline or a build, nor sets the
// Mac's recovery policy: those stay with Maintainers.
func (s *Server) submit(by Person, spec api.JobSpec, retryOf string, notes ...string) (*api.Job, int, error) {
	maintainer := by.Handle
	v := s.dev(spec.Device)
	if v == nil {
		return nil, 404, fmt.Errorf("no device %q", spec.Device)
	}
	if spec.RecoveryMode == api.RecoverySerialAssisted {
		return nil, statusOf(errNoConsole), errNoConsole
	}
	if by.AccountID != 0 && !by.isMaintainer() {
		if err := s.fence(v, by, fenceJobs); err != nil {
			return nil, statusOf(err), err
		}
		switch {
		case spec.Baseline:
			return nil, 403, errors.New("a baseline is a Maintainer's to run")
		case spec.Source != "" && spec.Kernel == "":
			return nil, 403, errors.New("building a kernel from source is a Maintainer's until M team builds open: boot a kernel that is already built")
		case spec.RecoveryMode != "" || spec.RecoveryTransaction != "":
			// The policy stays on the Mac past the job and the Lease.
			return nil, 403, errors.New("a Mac's recovery policy is a Maintainer's to set: leave recovery_mode out")
		}
	}
	v.recoveryMu.Lock()
	defer v.recoveryMu.Unlock()
	if err := validateRecoveryTransaction(v.snapshot(), spec); err != nil {
		return nil, 409, err
	}
	if !api.ValidRecoveryMode(spec.RecoveryMode) {
		return nil, 400, errors.New("recovery_mode must be network-only, or left out")
	}
	if spec.Config != "" {
		if spec.Source == "" {
			return nil, 400, errors.New("--config applies to a kernel built from a source URL")
		}
		if !reSHA.MatchString(spec.Config) {
			return nil, 400, errors.New("config must be an artifact sha256 (upload it first)")
		}
		// a retry that boots the kernel its first job built doesn't build again
		if _, err := os.Stat(s.artifactPath(spec.Config)); err != nil && spec.Kernel == "" {
			return nil, 400, fmt.Errorf("config artifact %s not uploaded", spec.Config)
		}
	}
	if spec.Source != "" {
		if spec.Kernel != "" && retryOf == "" {
			return nil, 400, errors.New("give a kernel artifact or a source to build, not both")
		}
		if spec.Baseline || spec.Crash != "" {
			return nil, 400, errors.New("baseline and crash-test jobs run the known-good kernel; drop the source")
		}
		if _, err := s.source(spec.Source); err != nil {
			return nil, 400, err
		}
	}
	if spec.Kernel != "" {
		if !reSHA.MatchString(spec.Kernel) {
			return nil, 400, errors.New("kernel must be an artifact sha256 (upload it first)")
		}
		if _, err := os.Stat(s.artifactPath(spec.Kernel)); err != nil {
			return nil, 400, fmt.Errorf("artifact %s not uploaded", spec.Kernel)
		}
	}
	for _, t := range spec.Tests {
		if t.Identity != nil {
			return nil, 400, fmt.Errorf("test %s: identity checks are the lab's own, part of an Installed run", t.Name)
		}
		if spec.Mesa == nil && t.Builtin == "mesa-provider" {
			return nil, 400, errors.New("mesa-provider requires a first-class Mesa package request")
		}
		if t.Script != "" {
			if _, err := os.Stat(s.artifactPath(t.Script)); err != nil {
				return nil, 400, fmt.Errorf("test %s: script artifact %s not uploaded", t.Name, t.Script)
			}
		}
		if t.Name == "" || strings.ContainsAny(t.Name, "/ ") {
			return nil, 400, fmt.Errorf("test name %q must be non-empty with no slashes or spaces", t.Name)
		}
	}
	if why := needsFull(spec); why != "" {
		if err := s.mayDo(callerFor(maintainer), v, actScript(why)); err != nil {
			return nil, 403, err
		}
	}
	if spec.Crash != "" && spec.Crash != "panic" {
		return nil, 400, fmt.Errorf("unknown crash mode %q", spec.Crash)
	}
	d := s.view(v)
	if d.State == api.StateLeft {
		return nil, 409, fmt.Errorf("%s left the lab (lab-agent uninstall): `lab forget %s` takes it off the list", d.Name, d.Name)
	}
	if d.TermsWaiting {
		return nil, 409, errors.New(termsWaitingLine(d.Name))
	}
	if code, err := s.validatePayload(spec, d.Device); err != nil {
		return nil, code, err
	}
	if err := validateRequiredWatchdog(spec.Tests, d.Device); err != nil {
		return nil, 409, err
	}
	if spec.CmdlineBase != "" || len(spec.CmdlineStrip) > 0 {
		if spec.Baseline || spec.Crash != "" {
			return nil, 400, errors.New("baseline and crash-test jobs boot the known-good cmdline; drop --cmdline-base/--cmdline-strip")
		}
		if !versionAtLeast(d.Facts.AgentVersion, "0.5.0") {
			return nil, 409, fmt.Errorf("%s runs lab-agent %s, which ignores --cmdline-base/--cmdline-strip; update it to 0.5.0 or later", d.Name, orStr(d.Facts.AgentVersion, "(unknown)"))
		}
		for _, g := range spec.CmdlineStrip {
			if _, err := path.Match(g, ""); err != nil || strings.TrimSpace(g) == "" {
				return nil, 400, fmt.Errorf("--cmdline-strip pattern %q is not a valid glob", g)
			}
		}
	}
	if spec.Crash == "" {
		has := false
		for i, t := range spec.Tests {
			if t.Builtin == omtTest {
				has = true
				spec.Tests[i].GUI = true
				spec.Tests[i].SkipOnScreen = d.Enrollment.IsVolunteer()
				if t.TimeoutSec == 0 {
					spec.Tests[i].TimeoutSec = omtSpec().TimeoutSec
				}
			}
		}
		if !has && s.omtWanted(v.snapshot(), spec) {
			ts := omtSpec()
			ts.SkipOnScreen = d.Enrollment.IsVolunteer()
			spec.Tests = append(spec.Tests, ts)
		}
	}
	if d.KnownGood == "" && !spec.Baseline {
		return nil, 409, fmt.Errorf("%s has not passed a baseline yet: run `lab baseline %s` first", d.Name, d.Name)
	}
	if spec.InstalledManifest != "" {
		if code, err := s.validateInstalled(by, v, spec, d.Device); err != nil {
			return nil, code, err
		}
	}
	if spec.InstalledManifest != "" || spec.Baseline {
		// An install removes the known-good kernel by design, and a baseline is
		// how the running kernel takes its place after a distro kernel update.
		if p := withoutStaleKnownGood(d.Facts.Problems); !d.Facts.PreflightOK && len(p) > 0 {
			return nil, 409, fmt.Errorf("%s fails preflight: %s", d.Name, strings.Join(p, "; "))
		}
	} else if !d.Facts.PreflightOK {
		return nil, 409, fmt.Errorf("%s fails preflight: %s", d.Name, preflightProblems(d.Device))
	}
	if spec.Mesa != nil {
		if code, err := s.validateMesa(spec, d.Device); err != nil {
			return nil, code, err
		}
	}
	owner := jobOwner{account: by.AccountID, holder: spec.Holder, maintainer: maintainer}
	if by.Handle != "" {
		if err := s.fence(v, by, fenceJobs); err != nil {
			return nil, statusOf(err), err
		}
	}
	v.mu.Lock()
	leased := v.leaseHoldsBackLocked(owner)
	v.mu.Unlock()
	if leased != "" {
		return nil, 409, errors.New(leased)
	}
	now := s.now()
	msg := "queued"
	if retryOf != "" {
		msg = "queued: a retry of job " + retryOf
	}
	j := &api.Job{ID: "j" + now.Format("0102-150405") + "-" + randHex(2), Spec: spec, Maintainer: maintainer, AccountID: by.AccountID, DeviceID: d.ID, State: api.JobQueued, Created: now, Updated: now,
		Events: []api.JobEvent{{Time: now, Msg: msg}}, RetryOf: retryOf,
		DeviceAtSubmit: &api.DeviceSnapshot{Family: family(d.Facts), Volunteer: d.Volunteer, OMT: s.omtWanted(d.Device, api.JobSpec{})}}
	for _, n := range notes {
		j.Events = append(j.Events, api.JobEvent{Time: now, Msg: n})
	}
	j.EffectiveRecoveryMode = effectiveRecoveryMode(spec.RecoveryMode, d.EffectiveRecoveryMode)
	jobMu.Lock()
	err := s.saveJobLocked(j)
	jobMu.Unlock()
	if err != nil {
		return nil, 500, err
	}
	// lab forget may have dropped the Mac since it was looked up, and then
	// nothing would run the job. Past this check the saved job keeps forget
	// from dropping it.
	v.mu.Lock()
	gone := v.forgotten
	v.mu.Unlock()
	if gone {
		s.updateJob(j, func(j *api.Job) {
			j.State, j.Outcome, j.Summary = api.JobDone, api.OutcomeCanceled, spec.Device+" was forgotten (lab forget) before the job started"
		})
		return nil, 404, fmt.Errorf("no device %q", spec.Device)
	}
	v.queue <- j.ID
	return j, 200, nil
}

// hRetry submits a finished job again; ?holder= names who retries it.
func (s *Server) hRetry(w http.ResponseWriter, r *http.Request) {
	j, code, err := s.retry(leaser(r), r.PathValue("id"), r.URL.Query().Get("holder"))
	if err != nil {
		httpErr(w, code, "%v", err)
		return
	}
	writeJSON(w, j)
}

// hJobs lists the newest jobs on the Macs the caller sees; ?device= keeps
// to one of them.
func (s *Server) hJobs(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 20
	}
	c, device := callerOf(r), r.URL.Query().Get("device")
	var (
		jobs []*api.Job
		err  error
	)
	switch {
	case c.reads():
		jobs, err = s.store.jobs(device, n, false)
	default:
		// By enrollment, not name: a Mac's name may have been someone else's
		// Mac before it was forgotten (seesJob).
		jobs, err = s.store.jobsOfEnrollments(s.seenEnrollments(c, device), n)
	}
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	if jobs == nil {
		jobs = []*api.Job{}
	}
	for _, j := range jobs {
		s.withReport(j)
	}
	writeJSON(w, jobs)
}

func (s *Server) hJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.job(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "no job %s", r.PathValue("id"))
		return
	}
	writeJSON(w, s.withReport(j))
}

// hWait holds until the job is done, the job has more events than the
// caller says it has seen (?events=), or the time asked for passes (?timeout=,
// in seconds): 50 s at most, so the caller asks again. A waiter that is cut
// leaves its job as it is.
func (s *Server) hWait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	deadline := s.now().Add(waitFor(r, "timeout", maxWait))
	seen := -1
	if q := r.URL.Query(); q.Has("events") {
		seen, _ = strconv.Atoi(q.Get("events"))
	}
	c := callerOf(r)
	for {
		// jobInScope asked once, but a wait lasts a while: a person a
		// Maintainer unlinks meanwhile gets nothing more of the job.
		j, err := s.store.job(id)
		if err != nil || !s.seesJob(c, j) {
			httpErr(w, 404, "no job %s", id)
			return
		}
		left := deadline.Sub(s.now())
		if j.State == api.JobDone || (seen >= 0 && len(j.Events) > seen) || left <= 0 {
			writeJSON(w, s.withReport(j))
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.clock().After(min(500*time.Millisecond, left)):
		}
	}
}

// hPublish publishes a finished job's omarchy-m-test report by hand: a
// Maintainer any job's, an M team member their own job's.
func (s *Server) hPublish(w http.ResponseWriter, r *http.Request) {
	j, err := s.store.job(r.PathValue("id"))
	if err != nil {
		httpErr(w, 404, "no job %s", r.PathValue("id"))
		return
	}
	p := leaser(r)
	if err := mayPublish(p, j); err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	if j.State != api.JobDone {
		httpErr(w, 409, "job %s is still %s", j.ID, j.State)
		return
	}
	url, err := s.omtPublish(j, p)
	if err != nil {
		httpErr(w, 409, "not published: %v", err)
		return
	}
	writeJSON(w, map[string]string{"report_url": url})
}

func (s *Server) hCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.store.job(id)
	if err != nil {
		httpErr(w, 404, "no job %s", id)
		return
	}
	p := leaser(r)
	if err := mayCancel(p, j); err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	// A job someone else cancels says who did.
	why := ""
	if j.AccountID != p.AccountID || j.AccountID == 0 && j.Maintainer != p.Handle {
		why = "by @" + p.Handle
		s.log.Printf("job %s (%s) of %s: canceled by %s", j.ID, j.Spec.Device, accountOr(j.AccountID, orStr(j.Maintainer, "labd")), accountOr(p.AccountID, "someone"))
	}
	s.mu.Lock()
	running := s.cancels[id] != nil
	s.mu.Unlock()
	s.cancelJob(id, why)
	if running {
		w.Write([]byte("canceling; the Mac is being restored to its known-good kernel\n"))
		return
	}
	w.Write([]byte("canceled\n"))
}

func (s *Server) hJobFile(w http.ResponseWriter, r *http.Request) {
	name, ok := cleanName(r.PathValue("name"))
	if !ok || strings.ContainsAny(r.PathValue("id"), "/.") {
		httpErr(w, 400, "bad name")
		return
	}
	if name == oldSiteLinks {
		// a deletion link is its report's credential
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.jobDir(r.PathValue("id")), name))
}
