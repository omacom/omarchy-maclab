package server

// Publishing performance values (spec section 22, "Publishing and
// Counted"): labd copies to reports.db what the public may see of the
// Performance runs and macOS results, each time any of it may have changed,
// and reports.db becomes exactly that. What is published is worked out
// afresh at each copy: a value current in labd.db, of a Mac still enrolled
// in the enrollment that measured it, whose Configuration labd knows and,
// for a Volunteer's Mac, whose publishing is on at the Mac now; on Omarchy,
// of a Release qualified or with no verdict yet. Who is Counted is worked
// out at each copy too, so that a mark, or an M team membership, covers the
// Mac's values before and after. Candidate boots' reports, Compares and the
// tools' logs are never in perf_current, so never copied.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
	"github.com/omacom/omarchy-maclab/internal/reports"
	"github.com/omacom/omarchy-maclab/internal/web"
)

func init() {
	addPage("POST /macs/{name}/counted", web.Maintainers, (*Server).pMacCounted)
}

func (s *Server) perfPublishRoutes() []route {
	return []route{
		{"POST /api/devices/{name}/counted", maintainers, s.hCounted},
	}
}

// publishPerf copies the published performance values to reports.db.
func (s *Server) publishPerf() {
	s.perfPublishMu.Lock()
	defer s.perfPublishMu.Unlock()
	p, err := s.publishedPerf()
	if err != nil {
		s.log.Printf("performance values for reports.db: %v", err)
		return
	}
	p, left := p.Pruned()
	for _, err := range left {
		s.log.Printf("performance values for reports.db: left out: %v", err)
	}
	if _, err := s.reports.PublishPerformance(p, s.now()); err != nil {
		s.log.Printf("performance values to reports.db: %v", err)
	}
}

// perfMacShown is what reports.db is told of one Mac, and of which
// enrollment its values are published; ok is false for a Mac none of whose
// values are.
type perfMacShown struct {
	mac        reports.PerfMac
	enrollment string
	ok         bool
}

func (s *Server) perfMacShown(name string) perfMacShown {
	v := s.dev(name)
	if v == nil {
		return perfMacShown{}
	}
	d := v.snapshot()
	if d.State == api.StateLeft || (d.IsVolunteer() && !d.Publish) {
		return perfMacShown{}
	}
	c := s.configuration(v)
	if c.Board == "" || !c.Known() {
		return perfMacShown{}
	}
	key := v.perfKey()
	if key == "" {
		return perfMacShown{}
	}
	return perfMacShown{mac: reports.PerfMac{Key: key, Kind: s.perfKind(d), Board: c.Board, CPU: c.CPU, GPU: c.GPU},
		enrollment: d.ID, ok: true}
}

// perfKind is the Mac's kind on the Performance pages: a Volunteer's Mac is
// Counted while a Maintainer's mark is on it or its Volunteer is on the M
// team.
func (s *Server) perfKind(d api.Device) string {
	switch {
	case !d.IsVolunteer():
		return reports.PerfLabOwned
	case d.Counted || (d.VolunteerID != 0 && s.onMTeam(d.VolunteerID)):
		return reports.PerfCounted
	}
	return reports.PerfVolunteer
}

// perfKey is the Mac's opaque key in reports.db, made the first time it is
// asked for; "" once the Mac is leaving the fleet.
func (v *dev) perfKey() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.forgotten {
		return ""
	}
	if v.d.PerfKey == "" {
		v.d.PerfKey = randHex(16)
		v.saveLocked()
	}
	return v.d.PerfKey
}

// perfReleaseKey is a Release's opaque key in reports.db: of its frozen
// manifest's digest, which nothing public names.
func perfReleaseKey(manifestSHA string) string {
	sum := sha256.Sum256([]byte("performance release\x00" + manifestSHA))
	return hex.EncodeToString(sum[:16])
}

// publishedPerf is what reports.db is to hold of Performance now.
func (s *Server) publishedPerf() (reports.Performance, error) {
	var p reports.Performance
	releases, err := s.store.perfReleases()
	if err != nil {
		return p, err
	}
	current, err := s.store.perfCurrentAll()
	if err != nil {
		return p, err
	}
	runs := map[int64]perf.Run{}
	macs := map[string]perfMacShown{}
	usedMacs, usedReleases := map[string]bool{}, map[string]bool{}
	for _, c := range current {
		run, ok := runs[c.Run]
		if !ok {
			if run, err = s.store.perfRun(c.Run); err != nil {
				return p, err
			}
			runs[c.Run] = run
		}
		m, ok := macs[run.Mac]
		if !ok {
			m = s.perfMacShown(run.Mac)
			macs[run.Mac] = m
		}
		if !m.ok || run.Enrollment != m.enrollment {
			continue
		}
		v := reports.PerfValue{Mac: m.mac.Key, Set: run.Set, Side: run.Side, Metric: c.Metric, Value: c.Value, Spread: c.Spread,
			Unit: c.Unit, API: c.API, Capacity: c.Capacity, Reason: c.Reason, Measured: c.Taken}
		switch run.Side {
		case perf.SideOmarchy:
			r, ok := releases.lookup(run.Release)
			if !ok {
				continue
			}
			v.Release, v.Booted = r.Key, run.Booted
			usedReleases[run.Release] = true
		case perf.SideMacOS:
			v.MacOSVersion, v.MacOSBuild = run.MacOSVersion, run.MacOSBuild
		default:
			continue
		}
		usedMacs[run.Mac] = true
		p.Values = append(p.Values, v)
	}
	for name := range usedMacs {
		p.Macs = append(p.Macs, macs[name].mac)
	}
	for _, name := range releases.order {
		if usedReleases[name] {
			r := releases.of[name]
			r.Position = len(p.Releases) + 1
			p.Releases = append(p.Releases, r)
		}
	}
	return p, nil
}

// perfReleaseList is every Release the Performance pages may name, by
// name, oldest frozen first in order: frozen, and qualified or with no
// verdict yet. A Release recorded not qualified, or whose verdict is
// revoked, is not in it.
type perfReleaseList struct {
	of    map[string]reports.PerfRelease
	order []string
}

func (l perfReleaseList) lookup(name string) (reports.PerfRelease, bool) {
	r, ok := l.of[name]
	return r, ok
}

func (st *store) perfReleases() (perfReleaseList, error) {
	l := perfReleaseList{of: map[string]reports.PerfRelease{}}
	rows, err := st.db.Query(`SELECT r.name, r.kernel, r.manifest_sha256,
coalesce((SELECT v.state FROM release_verdicts v WHERE v.release = r.name ORDER BY v.id DESC LIMIT 1), '')
FROM releases r ORDER BY r.frozen_at, r.name`)
	if err != nil {
		return l, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, kernel, manifest, state string
		if err := rows.Scan(&name, &kernel, &manifest, &state); err != nil {
			return l, err
		}
		r := reports.PerfRelease{Key: perfReleaseKey(manifest), Kernel: kernel}
		switch state {
		case api.ReleaseQualified:
			r.Verdict = reports.PerfQualified
		case "", api.ReleaseNoVerdict:
			r.Verdict = reports.PerfNoVerdictYet
		default:
			continue
		}
		l.of[name] = r
		l.order = append(l.order, name)
	}
	return l, rows.Err()
}

// perfCurrentAll is every current value of every Mac, Omarchy and macOS.
func (st *store) perfCurrentAll() ([]perf.Result, error) {
	return scanPerfResults(st.db.Query(`SELECT ` + perfResultCols + ` FROM perf_current c
JOIN perf_results p ON p.id = c.result JOIN perf_runs r ON r.id = p.run
ORDER BY c.mac, c.set_name, c.side, c.target, c.metric`))
}

// ---- Counted ----

// setCounted is a Maintainer marking a Volunteer's Mac Counted, or taking
// the mark away: its values, before and after, join its Configuration's
// headline, or leave it. It changes only the kind its published values
// carry, never what is published.
func (s *Server) setCounted(v *dev, by Person, counted bool) error {
	if !by.isMaintainer() {
		return refuse(http.StatusForbidden, "Only a Maintainer marks a Mac Counted.")
	}
	v.mu.Lock()
	switch {
	case v.forgotten:
		v.mu.Unlock()
		return errMacGone
	case !v.d.IsVolunteer():
		v.mu.Unlock()
		return refuse(http.StatusConflict, "%s is a Lab-owned Mac: its values make the headline already.", v.d.Name)
	case v.d.Counted == counted:
		v.mu.Unlock()
		return refuse(http.StatusConflict, "%s is %s already.", v.d.Name, map[bool]string{true: "marked Counted", false: "not marked Counted"}[counted])
	}
	v.d.Counted = counted
	line := "@" + by.Handle + " marked it Counted"
	if !counted {
		line = "@" + by.Handle + " took its Counted mark away"
	}
	v.said(macCounted, line)
	v.saveLocked()
	v.publishLocked()
	v.bump()
	v.mu.Unlock()
	s.publishPerf()
	return nil
}

// countedWords says whether a Volunteer's Mac is Counted, and why.
func (s *Server) countedWords(d api.Device) string {
	switch {
	case !d.IsVolunteer():
		return ""
	case d.Counted:
		return "Counted: a Maintainer marked it"
	case d.VolunteerID != 0 && s.onMTeam(d.VolunteerID):
		return "Counted while its Volunteer is on the M team"
	}
	return "not Counted: its performance values show on their own line, marked Volunteer"
}

// hCounted is a Maintainer's mark on a Volunteer's Mac, through the API:
// {"counted": true} or false.
func (s *Server) hCounted(w http.ResponseWriter, r *http.Request) {
	v := s.devOr404(w, r)
	if v == nil {
		return
	}
	var req api.CountedRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Counted == nil {
		httpErr(w, 400, `want {"counted": true} or {"counted": false}`)
		return
	}
	switch err := s.setCounted(v, leaser(r), *req.Counted); {
	case errors.Is(err, errMacGone):
		httpErr(w, 404, "no device %q", r.PathValue("name"))
	case err != nil:
		httpErr(w, statusOr500(err), "%v", err)
	default:
		writeJSON(w, s.view(v))
	}
}

// pMacCounted is the Counted mark's button on a Mac's page.
func (s *Server) pMacCounted(w http.ResponseWriter, r *http.Request) {
	v := s.macOr404(w, r)
	if v == nil {
		return
	}
	var err error
	switch r.PostFormValue("counted") {
	case "on":
		err = s.setCounted(v, leaser(r), true)
	case "off":
		err = s.setCounted(v, leaser(r), false)
	default:
		err = refuse(http.StatusBadRequest, "Say counted on or off.")
	}
	s.backToMacs(w, r, err)
}
