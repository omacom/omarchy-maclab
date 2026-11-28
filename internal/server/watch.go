package server

// labd watches the lab's own parts. Once a minute it looks at each, tells
// the owner's group "Agents" one line when a part goes wrong and one when it
// is well again, never the same thing twice (agentsLines.Trouble and
// Recovered keep that across restarts), and keeps what it saw for /status.
// Once a week it posts a digest there.
//
// What labd sees by itself: the disk, the Builder's polls, the Out-of-band
// controller's answer, the marker files of the nightly backup and the
// Restore drill, its own starts, and the Alias from outside. What only the
// mini can say (its boot time, the tunnel, Litestream, how often launchd
// started a daemon) comes from scripts/mini/parts, which labd runs.
//
// A part must stay wrong for a while before "Agents" is told: a Deploy
// stops each for a moment.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/omacom/omarchy-maclab/internal/api"
)

const (
	lookEvery = time.Minute
	// tunnelHold is the spec's: the tunnel disconnected for over 5 minutes.
	tunnelHold = 5 * time.Minute
	// partHold is how long Litestream, the Builder or the controller must
	// stay wrong before "Agents" is told.
	partHold     = 2 * time.Minute
	backupTooOld = 36 * time.Hour
	// diskLow is the free space, in percent, under which "Agents" is told.
	diskLow = 15
	// A daemon launchd started restartsMany times within restartsWindow is
	// restarting repeatedly.
	restartsWindow = 10 * time.Minute
	restartsMany   = 3
	// The Alias is asked from outside every aliasEvery; aliasMisses checks
	// in a row without it, while the Lab address answers, is no longer active.
	aliasEvery  = 15 * time.Minute
	aliasMisses = 2
	lookTimeout = 45 * time.Second
)

// Watch is what labd is given to watch the lab's own parts with
// (Config.Watch). Without one labd watches nothing and tells "Agents" of no
// part.
type Watch struct {
	// Mini asks the machine labd runs on what labd cannot see by itself
	// (scripts/mini/parts, see MiniParts); nil watches none of that.
	Mini func(context.Context) (Mini, error)
	// Marks is the folder where the nightly backup and the Restore drill
	// leave backup.ok and restore-drill.ok; "" knows of neither.
	Marks string
	// Disk is the free and the total bytes of the volume the data is on;
	// nil asks the system about DataDir.
	Disk func() (free, total uint64, err error)
	// Updates lists what waits for a Maintenance run, a line for each
	// (scripts/mini/pending-updates, see PendingUpdates); nil leaves the
	// weekly digest without them.
	Updates func(context.Context) ([]string, error)
	// Ask fetches an https address from outside, following nothing, and
	// gives the status and where the answer leads (see AskOutside); nil
	// leaves the Alias unwatched.
	Ask func(ctx context.Context, address string) (status int, location string, err error)
	// ByHand leaves the looking to the caller: labd starts no loop. Tests
	// set it and look when they choose.
	ByHand bool
}

// Mini is what the mini's helper says of the machine.
type Mini struct {
	// Boot is when the machine started.
	Boot time.Time
	// Tunnel is "up" or "down"; "" on a machine with no tunnel.
	Tunnel string
	// Litestream is "running" or "stopped", "" on a machine without it;
	// LitestreamErrors counts the errors in its log.
	Litestream       string
	LitestreamErrors int
	// Starts is how many times launchd started each daemon it keeps alive.
	Starts map[string]int
}

// A part's state on /status.
const (
	partOK   = "ok"
	partBad  = "bad"
	partNone = "none"
)

// StatusPart is one line of /status.
type StatusPart struct{ Name, State, Text string }

// Status is /status: the lab's parts as labd last saw them.
type Status struct {
	Looked string
	Parts  []StatusPart
}

type watcher struct {
	// mu is held for a whole look; statusMu only to read or replace what
	// the last look saw, so that /status never waits for one.
	mu       sync.Mutex
	statusMu sync.Mutex
	// since is when each trouble was first seen, while it lasts.
	since map[string]time.Time
	// lastAlive is when the labd before this one was last known to run.
	lastAlive time.Time
	greeted   bool
	starts    []time.Time // labd's own, within restartsWindow of this start
	runs      map[string]int
	restarts  map[string][]time.Time
	errors    int // Litestream's, at the last look
	errorsOK  bool
	aliasAt   time.Time
	aliasMiss int
	miniFail  bool
	status    Status

	digestMu   sync.Mutex
	nextDigest time.Time
}

const (
	markAlive  = "ops:alive"
	markStarts = "ops:starts"
	markAlias  = "ops:alias"
)

// startWatch reads what the labd before this one left, and notes this start.
func (s *Server) startWatch() {
	w := &s.watch
	w.since, w.runs, w.restarts = map[string]time.Time{}, map[string]int{}, map[string][]time.Time{}
	w.nextDigest = nextDigest(s.start)
	if v, ok := s.lines.mark(markAlive); ok {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			w.lastAlive = time.Unix(sec, 0)
		}
	}
	prev, _ := s.lines.mark(markStarts)
	var kept []string
	for _, f := range strings.Fields(prev) {
		if sec, err := strconv.ParseInt(f, 10, 64); err == nil && s.start.Sub(time.Unix(sec, 0)) < restartsWindow {
			w.starts = append(w.starts, time.Unix(sec, 0))
			kept = append(kept, f)
		}
	}
	w.starts = append(w.starts, s.start)
	s.lines.setMark(markStarts, strings.Join(append(kept, strconv.FormatInt(s.start.Unix(), 10)), " "))
}

// watchLoop looks once as labd starts and then every minute.
func (s *Server) watchLoop() {
	t := s.clock().NewTicker(lookEvery)
	defer t.Stop()
	s.look(s.ctx)
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		}
		s.look(s.ctx)
		s.digestIfDue(s.ctx)
	}
}

// judge follows one trouble: bad since when, "Agents" told once it has
// lasted hold, and told once when it is over. trouble and recovered are the
// two lines; each is given when the trouble was first seen (zero for a
// recovery labd did not see the start of). It returns that time while the
// trouble lasts.
func (s *Server) judge(now time.Time, key string, bad bool, hold time.Duration, trouble, recovered func(since time.Time) string) time.Time {
	w := &s.watch
	since := w.since[key]
	if !bad {
		delete(w.since, key)
		s.agents.Recovered(key, recovered(since))
		return time.Time{}
	}
	if since.IsZero() {
		since = now
		w.since[key] = since
	}
	if now.Sub(since) >= hold {
		s.agents.Trouble(key, trouble(since))
	}
	return since
}

func fixed(text string) func(time.Time) string { return func(time.Time) string { return text } }

// look looks at every part once.
func (s *Server) look(ctx context.Context) {
	w := &s.watch
	w.mu.Lock()
	defer w.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, lookTimeout)
	defer cancel()
	now := s.now()

	var mini *Mini
	if s.cfg.Watch.Mini != nil {
		m, err := s.cfg.Watch.Mini(ctx)
		switch {
		case err != nil && ctx.Err() != nil && s.ctx.Err() != nil:
			return
		case err != nil:
			if !w.miniFail {
				s.log.Printf("the mini's helper did not answer; what it tells of is not known until it does: %v", err)
			}
			w.miniFail = true
		default:
			mini, w.miniFail = &m, false
		}
	}

	s.lookRestart(now, mini)
	s.lookStarts(now, mini)
	parts := []StatusPart{
		{"Server", partOK, "up since " + whenDated(s.start) + s.releaseText()},
		s.lookTunnel(now, mini),
		s.lookBuilder(now),
		s.lookController(ctx, now),
		s.lookBackup(now),
		s.lookDrill(),
		s.lastRestart(mini),
	}
	s.lookLitestream(now, mini)
	s.lookDisk(now)
	s.lookSightings(now)
	s.lookAlias(ctx, now)

	w.statusMu.Lock()
	w.status = Status{Looked: whenDated(now), Parts: parts}
	w.statusMu.Unlock()
	s.lines.setMark(markAlive, strconv.FormatInt(now.Unix(), 10))
}

func (s *Server) releaseText() string {
	if s.cfg.Release == "" {
		return ""
	}
	return ", release " + s.cfg.Release
}

// statusNow is what labd last saw.
func (s *Server) statusNow() Status {
	if s.cfg.Watch == nil {
		st := Status{Looked: whenDated(s.now()), Parts: []StatusPart{{"Server", partOK, "up since " + whenDated(s.start) + s.releaseText()}}}
		for _, name := range []string{"Tunnel", "Builder", "Out-of-band controller", "Last good backup", "Last Restore drill"} {
			st.Parts = append(st.Parts, StatusPart{name, partNone, "not watched on this machine"})
		}
		st.Parts = append(st.Parts, s.lastRestart(nil))
		return st
	}
	s.watch.statusMu.Lock()
	defer s.watch.statusMu.Unlock()
	return s.watch.status
}

// lookRestart tells "Agents" the lab is back when the mini started after
// the labd before this one was last known to run, and how long that was.
func (s *Server) lookRestart(now time.Time, mini *Mini) {
	w := &s.watch
	if w.greeted || (s.cfg.Watch.Mini != nil && mini == nil) {
		return
	}
	w.greeted = true
	if mini == nil || mini.Boot.IsZero() || w.lastAlive.IsZero() || !mini.Boot.After(w.lastAlive) {
		return
	}
	s.agents.BackAfterRestart(w.lastAlive, s.start)
}

func (s *Server) lastRestart(mini *Mini) StatusPart {
	if mini == nil || mini.Boot.IsZero() {
		return StatusPart{"Last restart", partOK, "labd on " + whenDated(s.start)}
	}
	return StatusPart{"Last restart", partOK, "the mini on " + whenDated(mini.Boot)}
}

// lookStarts follows labd's own starts and those of the daemons launchd
// keeps alive.
func (s *Server) lookStarts(now time.Time, mini *Mini) {
	w := &s.watch
	if len(w.starts) >= restartsMany && now.Sub(s.start) < restartsWindow {
		s.agents.Trouble("restarting:labd", fmt.Sprintf("labd on the mini keeps restarting: it started %d times in the last %s.", len(w.starts), minutes(restartsWindow)))
	}
	if now.Sub(s.start) >= restartsWindow {
		s.agents.Recovered("restarting:labd", "labd on the mini has stayed up for "+minutes(restartsWindow)+".")
	}
	if mini == nil {
		return
	}
	names := make([]string, 0, len(mini.Starts))
	for name := range mini.Starts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		runs := mini.Starts[name]
		// A count that went down is a daemon loaded afresh, by the set-up
		// or a Deploy: the count starts over, and that is no news.
		if before, known := w.runs[name]; known && runs > before {
			for i := before; i < runs; i++ {
				w.restarts[name] = append(w.restarts[name], now)
			}
		}
		w.runs[name] = runs
		recent := w.restarts[name][:0]
		for _, at := range w.restarts[name] {
			if now.Sub(at) < restartsWindow {
				recent = append(recent, at)
			}
		}
		w.restarts[name] = recent
		switch {
		case len(recent) >= restartsMany:
			s.agents.Trouble("restarting:"+name, fmt.Sprintf("%s on the mini keeps restarting: launchd started it %d times in the last %s.", name, len(recent), minutes(restartsWindow)))
		case len(recent) == 0:
			s.agents.Recovered("restarting:"+name, name+" on the mini has stopped restarting: launchd started it no more in "+minutes(restartsWindow)+".")
		}
	}
}

const notKnown = "not known: the mini's helper did not answer"

func (s *Server) lookTunnel(now time.Time, mini *Mini) StatusPart {
	const name = "Tunnel"
	switch {
	case s.cfg.Watch.Mini == nil:
		return StatusPart{name, partNone, "not watched on this machine"}
	case mini == nil:
		return StatusPart{name, partNone, notKnown}
	case mini.Tunnel == "":
		return StatusPart{name, partNone, "not set up"}
	}
	since := s.judge(now, "tunnel", mini.Tunnel == "down", tunnelHold,
		func(since time.Time) string {
			return "The tunnel has been disconnected for over " + minutes(tunnelHold) + ", since " + when(since) + ": the lab is Dark until it connects again."
		},
		func(since time.Time) string {
			if since.IsZero() {
				return "The tunnel is connected again."
			}
			return "The tunnel is connected again. It was disconnected for " + howLong(now.Sub(since)) + "."
		})
	if !since.IsZero() {
		return StatusPart{name, partBad, "disconnected since " + whenDated(since)}
	}
	return StatusPart{name, partOK, "connected"}
}

func (s *Server) lookLitestream(now time.Time, mini *Mini) {
	w := &s.watch
	if mini == nil || mini.Litestream == "" {
		return
	}
	why := ""
	switch {
	case mini.Litestream == "stopped":
		why = "it is not running"
	case w.errorsOK && mini.LitestreamErrors > w.errors:
		why = "it keeps logging errors"
	}
	w.errors, w.errorsOK = mini.LitestreamErrors, true
	s.judge(now, "litestream", why != "", partHold,
		fixed("Litestream is failing on the mini: "+why+". The databases are not being copied to the house."),
		fixed("Litestream copies the databases to the house again."))
}

// builderAlive reports whether the builder named name asked labd for work
// within the last minute, and when it last did.
func (s *Server) builderAlive(name string) (alive bool, seen time.Time) {
	s.builds.mu.Lock()
	defer s.builds.mu.Unlock()
	t, ok := s.builds.builders[name]
	return ok && s.since(t) < time1Minute, t
}

func (s *Server) lookBuilder(now time.Time) StatusPart {
	const name = "Builder"
	named, err := s.store.builders()
	if err != nil {
		return StatusPart{name, partNone, "not known"}
	}
	if len(named) == 0 {
		return StatusPart{name, partNone, "none named"}
	}
	silent, last := false, time.Time{}
	for _, b := range named {
		alive, seen := s.builderAlive(b.Name)
		if !alive {
			silent = true
			if seen.After(last) {
				last = seen
			}
		}
	}
	// A builder that has not asked since labd started was last heard
	// before that.
	if last.IsZero() {
		last = s.start
	}
	since := s.judge(now, "builder", silent, partHold,
		fixed("The Builder is not healthy: it has not asked labd for work since "+when(last)+". Builds wait until it does."),
		fixed("The Builder asks labd for work again."))
	if !since.IsZero() {
		return StatusPart{name, partBad, "not heard from since " + whenDated(last)}
	}
	return StatusPart{name, partOK, "asking for work"}
}

func (s *Server) lookController(ctx context.Context, now time.Time) StatusPart {
	const name = "Out-of-band controller"
	c := s.cfg.OOB
	if c == nil {
		return StatusPart{name, partNone, "not set up"}
	}
	if _, err := os.Lstat(c.TokenFile); errors.Is(err, os.ErrNotExist) {
		return StatusPart{name, partNone, "not set up"}
	}
	// What a port shows, which runs no tool: the reset tool's own answer
	// (Probe) takes the mini's port controller into its debug mode.
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err := c.Port(actx, 0)
	cancel()
	if err != nil && s.watch.since["oob"].IsZero() {
		s.log.Printf("the Out-of-band controller: %v", err)
	}
	since := s.judge(now, "oob", err != nil, partHold,
		fixed("The Out-of-band controller on the mini is not healthy: it does not answer as it should. No Mac gets a Hard reset until it does; jobs still run."),
		fixed("The Out-of-band controller on the mini answers again."))
	if !since.IsZero() {
		return StatusPart{name, partBad, "not answering since " + whenDated(since)}
	}
	return StatusPart{name, partOK, "answering"}
}

// markTime is the time a marker file holds: the last good run's, as the
// script wrote it, or the file's own. ok is false when there is none.
func (s *Server) markTime(name string) (at time.Time, ok bool) {
	if s.cfg.Watch.Marks == "" {
		return time.Time{}, false
	}
	path := filepath.Join(s.cfg.Watch.Marks, name)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return time.Time{}, false
	}
	if b, err := os.ReadFile(path); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b))); err == nil {
			return t, true
		}
	}
	return st.ModTime(), true
}

func (s *Server) lookBackup(now time.Time) StatusPart {
	const name = "Last good backup"
	at, ok := s.markTime("backup.ok")
	if !ok {
		return StatusPart{name, partNone, "none yet"}
	}
	age := now.Sub(at)
	text := whenDated(at) + ", " + howLong(age) + " ago"
	old := age > backupTooOld
	s.judge(now, "backup", old, 0,
		fixed("The last good nightly backup is from "+text+": older than 36 hours."),
		fixed("The nightly backup ran again: the last good one is from "+whenDated(at)+"."))
	if old {
		return StatusPart{name, partBad, text + ": older than 36 hours"}
	}
	return StatusPart{name, partOK, text}
}

// drillKey is the trouble the Restore drill's script names when a drill
// fails, and takes back when one passes (scripts/mini/restore-drill).
const drillKey = "restore-drill"

func (s *Server) lookDrill() StatusPart {
	const name = "Last Restore drill"
	at, ok := s.markTime("restore-drill.ok")
	if _, failed, bad := s.lines.markAt("trouble:" + drillKey); bad {
		text := "failed on " + whenDated(failed)
		if ok {
			text += "; the last one that passed was on " + whenDated(at)
		}
		return StatusPart{name, partBad, text}
	}
	if !ok {
		return StatusPart{name, partNone, "none yet"}
	}
	return StatusPart{name, partOK, "passed on " + whenDated(at)}
}

func (s *Server) disk() (free, total uint64, err error) {
	if s.cfg.Watch.Disk != nil {
		return s.cfg.Watch.Disk()
	}
	var st unix.Statfs_t
	if err := unix.Statfs(s.cfg.DataDir, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), uint64(st.Blocks) * uint64(st.Bsize), nil
}

func (s *Server) lookDisk(now time.Time) {
	free, total, err := s.disk()
	if err != nil || total == 0 {
		return
	}
	percent := free * 100 / total
	size := fmt.Sprintf("%d GB of %d GB", free>>30, total>>30)
	s.judge(now, "disk", percent < diskLow, 0,
		fixed(fmt.Sprintf("The mini's disk is %d %% free (%s): under %d %%.", percent, size, diskLow)),
		fixed(fmt.Sprintf("The mini's disk is %d %% free again (%s).", percent, size)))
}

// lookSightings watches Lab-owned Macs that have held a Sighting and tells
// "Agents" once when one stays unsighted past partHold while the Out-of-band
// controller itself is healthy.
func (s *Server) lookSightings(now time.Time) {
	if s.oob.Empty() {
		return
	}
	s.cables.mu.Lock()
	down := s.cables.down != ""
	s.cables.mu.Unlock()
	if down {
		return
	}
	for _, v := range s.allDevs() {
		v.mu.Lock()
		d := v.d
		v.mu.Unlock()
		if d.IsVolunteer() || d.State == api.StateLeft {
			continue
		}
		s.cables.mu.Lock()
		sg, sighted := s.cables.held[d.Name]
		ever := s.cables.everSighted[d.Name]
		lost := s.cables.lost[d.Name]
		s.cables.mu.Unlock()
		key := "sighting:" + d.ID
		if sighted {
			port := s.oob.PortName(sg.rid)
			s.judge(now, key, false, partHold,
				fixed(""),
				fixed(fmt.Sprintf("%s is sighted again on the %s port.", d.Name, port)))
			continue
		}
		if !ever {
			continue
		}
		detail := ""
		if lost != "" {
			detail = " (" + lost + ")"
		}
		s.judge(now, key, true, partHold,
			fixed(fmt.Sprintf("%s lost its Sighting%s. It has no Hard reset until sighted again or proved.", d.Name, detail)),
			fixed(d.Name+" is sighted again."))
	}
}

// lookAlias asks the Alias from outside whether it still leads to the Lab
// address. An Alias that was never active is not missed, and while the Lab
// address itself does not answer the Alias is not blamed.
func (s *Server) lookAlias(ctx context.Context, now time.Time) {
	w := &s.watch
	ask := s.cfg.Watch.Ask
	if ask == nil || s.cfg.Alias == "" || s.cfg.PublicURL == nil {
		return
	}
	if !w.aliasAt.IsZero() && now.Sub(w.aliasAt) < aliasEvery {
		return
	}
	w.aliasAt = now
	const path = "/healthz"
	lab := s.cfg.PublicURL.Scheme + "://" + s.cfg.PublicURL.Host
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, to, err := ask(actx, s.cfg.PublicURL.Scheme+"://"+s.cfg.Alias+path)
	if err == nil && status == 308 && to == lab+path {
		w.aliasMiss = 0
		s.lines.setMark(markAlias, "active")
		s.agents.Recovered("alias", "The Alias is active again.")
		return
	}
	if _, was := s.lines.mark(markAlias); !was {
		return
	}
	if status, _, err := ask(actx, lab+s.basePath()+path); err != nil || status != 200 {
		return
	}
	if w.aliasMiss++; w.aliasMiss >= aliasMisses {
		s.agents.Trouble("alias", "The Alias is no longer active: it stopped leading to the Lab address, which answers.")
	}
}

// ---- the weekly digest ----

// nextDigest is the first Monday 09:00 Brisbane after t.
func nextDigest(t time.Time) time.Time {
	b := t.In(brisbane)
	at := time.Date(b.Year(), b.Month(), b.Day(), 9, 0, 0, 0, brisbane)
	for at.Weekday() != time.Monday || !at.After(t) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// digestIfDue posts the weekly digest once its hour has come, once a week:
// a labd that restarts in that hour does not post it again, and one that
// was not running then posts none that week. It says whether it posted one.
func (s *Server) digestIfDue(ctx context.Context) bool {
	w := &s.watch
	w.digestMu.Lock()
	defer w.digestMu.Unlock()
	now := s.now()
	if now.Before(w.nextDigest) {
		return false
	}
	day := w.nextDigest
	w.nextDigest = nextDigest(now)
	if !s.lines.markOnce("weekly-digest", day.In(brisbane).Format(time.DateOnly)) {
		return false
	}
	s.postWeeklyDigest(ctx, day)
	return true
}

// postWeeklyDigest records the weekly digest for "Agents" now: what waits
// for a Maintenance run, and the dates of the last good backup and the
// last good Restore drill.
func (s *Server) postWeeklyDigest(ctx context.Context, day time.Time) {
	var b strings.Builder
	b.WriteString("Omarchy Mac Lab, weekly digest, " + day.In(brisbane).Format("Mon 2 Jan") + "\n")
	switch ask := s.cfg.Watch.Updates; {
	case ask == nil:
		b.WriteString("Pending updates: not looked at on this machine\n")
	default:
		actx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		updates, err := ask(actx)
		cancel()
		switch {
		case err != nil:
			s.log.Printf("the weekly digest: the pending updates could not be listed: %v", err)
			b.WriteString("Pending updates: could not be listed (see labd's log)\n")
		case len(updates) == 0:
			b.WriteString("Pending updates: none\n")
		default:
			b.WriteString("Pending updates:\n")
			for _, u := range updates {
				b.WriteString("- " + oneLine(u) + "\n")
			}
		}
	}
	now := s.now()
	b.WriteString("Last good backup: ")
	if at, ok := s.markTime("backup.ok"); ok {
		b.WriteString(whenDated(at) + ", " + howLong(now.Sub(at)) + " ago\n")
	} else {
		b.WriteString("none yet\n")
	}
	b.WriteString("Last good Restore drill: ")
	if at, ok := s.markTime("restore-drill.ok"); ok {
		b.WriteString(whenDated(at))
	} else {
		b.WriteString("none yet")
	}
	s.agents.WeeklyDigest(b.String())
}

// ---- how a line says a time ----

// whenDated is a time with its day, for what may be weeks old: Sat 14 Nov 14:30 AEST.
func whenDated(t time.Time) string { return t.In(brisbane).Format("Mon 2 Jan 15:04 MST") }

func minutes(d time.Duration) string { return strconv.Itoa(int(d/time.Minute)) + " minutes" }

// howLong is a length of time as a line says it: 51 min, 2 h 14 min, 3 d 4 h.
func howLong(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	m := int(d / time.Minute)
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m < 48*60:
		if m%60 == 0 {
			return fmt.Sprintf("%d h", m/60)
		}
		return fmt.Sprintf("%d h %d min", m/60, m%60)
	}
	h := m / 60
	if h%24 == 0 {
		return fmt.Sprintf("%d d", h/24)
	}
	return fmt.Sprintf("%d d %d h", h/24, h%24)
}
