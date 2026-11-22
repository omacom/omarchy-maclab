package server

// Live data for UIs: one server-sent event stream for everything that
// changes, screen captures from the GUI session, and logs read straight
// from a running Mac. The web UI and a future TUI use the same endpoints.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

type event struct {
	kind, device string
	data         []byte
}

type sub struct {
	ch     chan event
	device string
}

type hub struct {
	mu   sync.Mutex
	subs map[*sub]bool
}

// publish never blocks: a subscriber that can't keep up misses events and
// catches up from the REST endpoints.
func (h *hub) publish(kind, device string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if s.device != "" && device != "" && s.device != device {
			continue
		}
		select {
		case s.ch <- event{kind, device, b}:
		default:
		}
	}
}

func (h *hub) subscribe(device string) *sub {
	s := &sub{ch: make(chan event, 512), device: device}
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[*sub]bool{}
	}
	h.subs[s] = true
	h.mu.Unlock()
	return s
}

func (h *hub) unsubscribe(s *sub) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// streamsTo is whether an event of this kind about the Mac called device
// goes to c. A caller gets only the events of the Macs they see (see sees),
// and the fleet's own (builds) only as a Maintainer. Serial lines carry the
// console, so only Maintainers get them, as with the stored serial log. A
// Mac's kernel events and new screens are its live views: an M team member
// gets them only where they may watch, under their own Lease.
func (s *Server) streamsTo(c caller, kind, device string) bool {
	switch {
	case !s.sees(c, device):
		return false
	case kind == "serial":
		return c.maintainer != ""
	case (kind == "kevent" || kind == "screen") && c.team != "":
		return s.mayDo(c, s.dev(device), actWatch) == nil
	}
	return true
}

// hStream serves server-sent events: device, job, serial, kevent, screen,
// build. ?device= limits it to one Mac, and streamsTo says what of it a
// caller gets. A page reads it with its browser's session; a program with
// fetch and its Lab token.
func (s *Server) hStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	c := callerOf(r)
	sb := s.hub.subscribe(r.URL.Query().Get("device"))
	defer s.hub.unsubscribe(sb)
	keep := s.clock().NewTicker(15 * time.Second)
	defer keep.Stop()
	fmt.Fprint(w, ": maclab stream\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-keep.C():
			fmt.Fprint(w, ": keepalive\n\n")
		case e := <-sb.ch:
			if !s.streamsTo(c, e.kind, e.device) {
				continue
			}
			if e.kind == "device" && c.team != "" {
				e.data = forTeam(e.data)
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.kind, e.data)
		}
		fl.Flush()
	}
}

// viewLocked is view for callers already holding v.mu.
func (v *dev) viewLocked() DeviceView {
	d := v.d
	// labd's own record of which reports are the Mac's: shown to nobody.
	d.MachineID, d.PerfKey = "", ""
	d.Lease = v.liveLeaseLocked()
	if c, _ := v.configurationLocked(); c != (api.Configuration{}) {
		d.Configuration = &c
	} else {
		d.Configuration = nil
	}
	// What the old lab kept of a controller of the Mac's own is no part of
	// a view: its token was a secret, and this lab resets through a port.
	d.OOB = nil
	cable := v.cableLocked()
	tier := 0
	if d.Facts.WatchdogArmed {
		tier = 1
	}
	if cable != nil && cable.NoHardReset == "" {
		tier = 2
	}
	if p := d.SitePause; p != nil && !p.Active(v.s.now()) {
		d.SitePause = nil // its time has passed
	}
	// labd's own note of which lab-agent the Volunteer was told of.
	d.UpdateNoticed, d.TermsNoticed = "", ""
	return DeviceView{Device: d, Tier: tier, Alive: v.aliveLocked(), Queued: len(v.queue) + len(v.parked), ScreenAt: v.screenAt, Cable: cable, Arrival: v.arrivalLocked(),
		LabAgent: v.s.agentBehind(d), TermsWaiting: v.termsWait}
}

// publishLocked sends the device to the web UI, which adds a device it
// hasn't seen: never one lab forget dropped.
func (v *dev) publishLocked() {
	if !v.forgotten {
		v.s.hub.publish("device", v.d.Name, v.viewLocked())
	}
}

// ---- screens ----

func (s *Server) screenDir(name string) string { return filepath.Join(s.cfg.DataDir, "screens", name) }

type screenFile struct {
	Name string    `json:"name"`
	Time time.Time `json:"time"`
	Size int64     `json:"size"`
}

func (s *Server) screens(name string) []screenFile {
	ents, _ := os.ReadDir(s.screenDir(name))
	var out []screenFile
	for _, e := range ents {
		ms, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".jpg"), 10, 64)
		if err != nil {
			continue
		}
		info, _ := e.Info()
		var size int64
		if info != nil {
			size = info.Size()
		}
		out = append(out, screenFile{Name: e.Name(), Time: time.UnixMilli(ms), Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

const keepScreens = 120

func (s *Server) hAgentScreen(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	dir := s.screenDir(v.d.Name)
	os.MkdirAll(dir, 0o755)
	now := s.now()
	name := fmt.Sprintf("%d.jpg", now.UnixMilli())
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	_, err = io.Copy(f, io.LimitReader(r.Body, 32<<20))
	f.Close()
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	for _, old := range s.screens(v.d.Name)[min(keepScreens, len(s.screens(v.d.Name))):] {
		os.Remove(filepath.Join(dir, old.Name))
	}
	v.mu.Lock()
	v.screenAt, v.screenName = now, name
	v.mu.Unlock()
	w.Write([]byte(name))
}

// hScreenshot captures the Mac's screen now. Concurrent requests share one capture.
func (s *Server) hScreenshot(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	if err := s.mayDo(callerOf(r), v, actWatch); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	v.shotMu.Lock()
	defer v.shotMu.Unlock()
	v.mu.Lock()
	fresh := s.since(v.screenAt) < 1500*time.Millisecond
	alive := v.aliveLocked()
	v.mu.Unlock()
	var res api.ScreenResult
	if !fresh {
		if !alive {
			httpErr(w, 409, "%s is not responding", v.d.Name)
			return
		}
		if err := v.call(r.Context(), api.CmdScreen, "", api.ScreenArgs{WaitSec: 5}, 40*time.Second, &res); err != nil {
			httpErr(w, statusOf(err), "%v", err)
			return
		}
		s.hub.publish("screen", v.d.Name, map[string]any{"device": v.d.Name, "name": res.Name, "time": res.Time, "width": res.Width, "height": res.Height})
	}
	v.mu.Lock()
	res.Name, res.Time = v.screenName, v.screenAt
	v.mu.Unlock()
	writeJSON(w, res)
}

func (s *Server) hScreen(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil || !s.teamMayWatch(w, r, v) {
		return
	}
	name := r.PathValue("file")
	if name == "" || name == "latest" {
		l := s.screens(v.d.Name)
		if len(l) == 0 {
			httpErr(w, 404, "no screen captured for %s yet", v.d.Name)
			return
		}
		name = l[0].Name
	}
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
		httpErr(w, 400, "bad name")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Screen-Name", name)
	http.ServeFile(w, r, filepath.Join(s.screenDir(v.d.Name), name))
}

func (s *Server) hScreens(w http.ResponseWriter, r *http.Request) {
	if v := s.devOr404(w, r); v != nil && s.teamMayWatch(w, r, v) {
		l := s.screens(v.d.Name)
		if l == nil {
			l = []screenFile{}
		}
		writeJSON(w, l)
	}
}

// teamMayWatch answers w and reports false for an M team member asking for
// a Mac's kept screens: they see the fleet, and a Mac's screen only as
// mayDo lets them watch it.
func (s *Server) teamMayWatch(w http.ResponseWriter, r *http.Request, v *dev) bool {
	c := callerOf(r)
	if c.team == "" {
		return true
	}
	if err := s.mayDo(c, v, actWatch); err != nil {
		httpErr(w, 409, "%v", err)
		return false
	}
	return true
}

// hLogs reads the journal or kernel log live from the Mac.
func (s *Server) hLogs(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	if err := s.mayDo(callerOf(r), v, actWatch); err != nil {
		httpErr(w, 409, "%v", err)
		return
	}
	v.mu.Lock()
	alive := v.aliveLocked()
	v.mu.Unlock()
	if !alive {
		httpErr(w, 409, "%s is offline, so its live logs can't be read. Job logs are still available.", v.d.Name)
		return
	}
	q := r.URL.Query()
	lines, _ := strconv.Atoi(q.Get("lines"))
	args := api.LogsArgs{Source: q.Get("source"), Lines: lines, Unit: q.Get("unit")}
	var out string
	if err := v.call(r.Context(), api.CmdLogs, "", args, 45*time.Second, &out); err != nil {
		httpErr(w, statusOf(err), "%v", err)
		return
	}
	// The Mac may take up to 45 s to answer: a person a Maintainer unlinked
	// meanwhile doesn't get its journal.
	if !s.sees(callerOf(r), r.PathValue("name")) {
		httpErr(w, 404, "no device %q", r.PathValue("name"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, out)
}

// capture saves a screenshot into the job's files. It is best effort: a Mac
// without a GUI session just gets a note in the timeline.
func (r *jobRun) capture(label string) {
	if r.v.leaving() != "" {
		return // its agent is stopping: nothing would answer
	}
	var res api.ScreenResult
	ctx := r.s.ctx
	err := r.v.call(ctx, api.CmdScreen, r.j.ID, api.ScreenArgs{Label: label, WaitSec: 45}, 90*time.Second, &res)
	if err != nil {
		r.ev("no screenshot (%s): %v", label, err)
		return
	}
	r.s.updateJob(r.j, func(j *api.Job) { j.Result.Logs = append(j.Result.Logs, res.Name) })
	r.s.hub.publish("screen", r.j.Spec.Device, map[string]any{"device": r.j.Spec.Device, "job": r.j.ID, "name": res.Name, "time": res.Time})
}
