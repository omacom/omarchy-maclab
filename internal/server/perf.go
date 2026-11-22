package server

// Performance results (spec section 22, "Performance run"): labd opens an
// attempt for a Mac (openPerfRun), the Mac uploads its results to it, once
// or in parts, and ends it with succeeded or failed. Each attempt is kept
// with every result it uploaded. The current value per Mac, set, metric and
// Release (or macOS build) is the newest succeeded attempt's; a failed
// attempt only fills a metric that has none, so a failed rerun keeps the old
// values.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// perfSet is the Benchmark set of a run, by name.
var perfSet = sets.Load

var (
	errNoPerfRun    = errors.New("no such performance run")
	errPerfEnded    = errors.New("the performance run has ended")
	errPerfRunning  = errors.New("a performance run is running on this Mac already")
	errPerfErrorRun = errors.New("an attempt with a result in error has failed, not succeeded")
)

const perfRunCols = `id, mac, enrollment, set_name, side, release_version, macos_version, macos_build, list,
attempt, state, booted, boot_seconds, conditions, started, ended`

// openPerfRun opens the next attempt of r's Mac, set, side and Release or
// macOS build, running from now.
func (st *store) openPerfRun(r perf.Run, now time.Time) (perf.Run, error) {
	switch {
	case r.Mac == "" || r.Enrollment == "" || r.Set == "":
		return r, fmt.Errorf("a performance run names its Mac, enrollment and set")
	case r.List != sets.Quick && r.List != sets.Full:
		return r, fmt.Errorf("list %q: want quick or full", r.List)
	case r.Side == perf.SideOmarchy && (r.Release == "" || r.MacOSBuild != "" || r.MacOSVersion != ""):
		return r, fmt.Errorf("an Omarchy run names its Release and no macOS build")
	case r.Side == perf.SideMacOS && (r.MacOSBuild == "" || r.Release != ""):
		return r, fmt.Errorf("a macOS run names its macOS build and no Release")
	case r.Side != perf.SideOmarchy && r.Side != perf.SideMacOS:
		return r, fmt.Errorf("side %q: want omarchy or macos", r.Side)
	}
	tx, err := st.db.Begin()
	if err != nil {
		return r, err
	}
	defer tx.Rollback()
	var running int
	if err := tx.QueryRow(`SELECT count(*) FROM perf_runs WHERE mac=? AND side=? AND state=?`, r.Mac, r.Side, perf.Running).Scan(&running); err != nil {
		return r, err
	}
	if running > 0 {
		return r, errPerfRunning
	}
	if err := tx.QueryRow(`SELECT coalesce(max(attempt), 0) + 1 FROM perf_runs
WHERE mac=? AND set_name=? AND side=? AND release_version=? AND macos_build=?`,
		r.Mac, r.Set, r.Side, r.Release, r.MacOSBuild).Scan(&r.Attempt); err != nil {
		return r, err
	}
	r.State, r.Started, r.Ended, r.Booted, r.BootSeconds, r.Conditions = perf.Running, now, nil, nil, nil, nil
	res, err := tx.Exec(`INSERT INTO perf_runs(mac, enrollment, set_name, side, release_version, macos_version,
macos_build, list, attempt, state, started) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		r.Mac, r.Enrollment, r.Set, r.Side, r.Release, r.MacOSVersion, r.MacOSBuild, r.List, r.Attempt, r.State, now.UnixMicro())
	if err != nil {
		return r, err
	}
	if r.ID, err = res.LastInsertId(); err != nil {
		return r, err
	}
	return r, tx.Commit()
}

type rowScanner interface{ Scan(...any) error }

func scanPerfRun(row rowScanner) (perf.Run, error) {
	var r perf.Run
	var booted, cond string
	var boot sql.NullFloat64
	var started int64
	var ended sql.NullInt64
	err := row.Scan(&r.ID, &r.Mac, &r.Enrollment, &r.Set, &r.Side, &r.Release, &r.MacOSVersion, &r.MacOSBuild,
		&r.List, &r.Attempt, &r.State, &booted, &boot, &cond, &started, &ended)
	if err != nil {
		return r, err
	}
	if booted != "{}" {
		json.Unmarshal([]byte(booted), &r.Booted)
	}
	if cond != "{}" {
		r.Conditions = json.RawMessage(cond)
	}
	if boot.Valid {
		r.BootSeconds = &boot.Float64
	}
	r.Started = time.UnixMicro(started)
	if ended.Valid {
		t := time.UnixMicro(ended.Int64)
		r.Ended = &t
	}
	return r, nil
}

func (st *store) perfRun(id int64) (perf.Run, error) {
	r, err := scanPerfRun(st.db.QueryRow(`SELECT `+perfRunCols+` FROM perf_runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		err = errNoPerfRun
	}
	return r, err
}

// takePerf takes a checked upload for the run: each result once per metric,
// the booted versions, boot time and conditions the first time they come,
// and, with a state, the attempt's end. A run that has ended takes again
// only what it already has.
func (st *store) takePerf(id int64, c perf.Checked, now time.Time) (perf.Taken, error) {
	tx, err := st.db.Begin()
	if err != nil {
		return perf.Taken{}, err
	}
	defer tx.Rollback()
	r, err := scanPerfRun(tx.QueryRow(`SELECT `+perfRunCols+` FROM perf_runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return perf.Taken{}, errNoPerfRun
	}
	if err != nil {
		return perf.Taken{}, err
	}
	t := perf.Taken{Run: r.ID, Attempt: r.Attempt, State: r.State}
	if perf.Ended(r.State) {
		for _, e := range c.Entries {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM perf_results WHERE run=? AND metric=?`, id, e.Metric).Scan(&n); err != nil {
				return t, err
			}
			if n == 0 {
				return t, fmt.Errorf("%w (%s): %s comes too late", errPerfEnded, r.State, e.Metric)
			}
			t.Already++
		}
		if c.State != "" && c.State != r.State {
			return t, fmt.Errorf("%w as %s", errPerfEnded, r.State)
		}
		return t, nil
	}
	for _, e := range c.Entries {
		res, err := tx.Exec(`INSERT INTO perf_results(run, metric, value, spread, unit, api, capacity, reason, seconds, cool_down, taken)
VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(run, metric) DO NOTHING`,
			id, e.Metric, e.Value, e.Spread, e.Unit, e.API, e.Capacity, e.Reason, e.Seconds, e.CoolDown, now.UnixMicro())
		if err != nil {
			return t, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			t.Taken++
		} else {
			t.Already++
		}
	}
	if len(c.Booted) > 0 {
		b, _ := json.Marshal(c.Booted)
		if _, err := tx.Exec(`UPDATE perf_runs SET booted=? WHERE id=? AND booted='{}'`, string(b), id); err != nil {
			return t, err
		}
	}
	if c.BootSeconds != nil {
		if _, err := tx.Exec(`UPDATE perf_runs SET boot_seconds=? WHERE id=? AND boot_seconds IS NULL`, *c.BootSeconds, id); err != nil {
			return t, err
		}
	}
	if c.Conditions != "" {
		if _, err := tx.Exec(`UPDATE perf_runs SET conditions=? WHERE id=? AND conditions='{}'`, c.Conditions, id); err != nil {
			return t, err
		}
	}
	if c.State != "" {
		if err := endPerfRunTx(tx, r, c.State, now); err != nil {
			return t, err
		}
		t.State = c.State
	}
	return t, tx.Commit()
}

// endPerfRun ends a run from labd's side: failed (a Mac that never
// reported, a start that failed), cancelled, or needs_hands, which still
// takes the results that come late.
func (st *store) endPerfRun(id int64, state string, now time.Time) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := scanPerfRun(tx.QueryRow(`SELECT `+perfRunCols+` FROM perf_runs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return errNoPerfRun
	}
	if err != nil {
		return err
	}
	if perf.Ended(r.State) {
		return fmt.Errorf("%w as %s", errPerfEnded, r.State)
	}
	switch state {
	case perf.NeedsHands:
		if _, err := tx.Exec(`UPDATE perf_runs SET state=? WHERE id=?`, state, id); err != nil {
			return err
		}
	case perf.Failed, perf.Cancelled:
		if err := endPerfRunTx(tx, r, state, now); err != nil {
			return err
		}
	default:
		return fmt.Errorf("labd ends a run as failed, cancelled or needs_hands, not %q", state)
	}
	return tx.Commit()
}

func endPerfRunTx(tx *sql.Tx, r perf.Run, state string, now time.Time) error {
	if state == perf.Succeeded {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM perf_results WHERE run=? AND reason=?`, r.ID, perf.Error).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errPerfErrorRun
		}
	}
	if _, err := tx.Exec(`UPDATE perf_runs SET state=?, ended=? WHERE id=?`, state, now.UnixMicro(), r.ID); err != nil {
		return err
	}
	conflict := ``
	switch state {
	case perf.Succeeded:
		conflict = `DO UPDATE SET result=excluded.result`
	case perf.Failed:
		conflict = `DO NOTHING`
	default:
		return nil
	}
	_, err := tx.Exec(`INSERT INTO perf_current(mac, set_name, side, target, metric, result)
SELECT ?, ?, ?, ?, metric, id FROM perf_results WHERE run=? ORDER BY id
ON CONFLICT(mac, set_name, side, target, metric) `+conflict, r.Mac, r.Set, r.Side, r.Target(), r.ID)
	return err
}

const perfResultCols = `p.run, r.attempt, p.metric, p.value, p.spread, p.unit, p.api, p.capacity, p.reason, p.seconds, p.cool_down, p.taken`

func scanPerfResults(rows *sql.Rows, err error) ([]perf.Result, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []perf.Result{}
	for rows.Next() {
		var p perf.Result
		var value, spread sql.NullFloat64
		var capacity sql.NullInt64
		var reason sql.NullString
		var taken int64
		if err := rows.Scan(&p.Run, &p.Attempt, &p.Metric, &value, &spread, &p.Unit, &p.API, &capacity, &reason, &p.Seconds, &p.CoolDown, &taken); err != nil {
			return nil, err
		}
		if value.Valid {
			p.Value = &value.Float64
		}
		if spread.Valid {
			p.Spread = &spread.Float64
		}
		if capacity.Valid {
			p.Capacity = &capacity.Int64
		}
		if reason.Valid {
			p.Reason = &reason.String
		}
		p.Taken = time.UnixMicro(taken)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (st *store) perfCurrentOf(mac, set, side, target string) ([]perf.Result, error) {
	return scanPerfResults(st.db.Query(`SELECT `+perfResultCols+` FROM perf_current c
JOIN perf_results p ON p.id = c.result JOIN perf_runs r ON r.id = p.run
WHERE c.mac=? AND c.set_name=? AND c.side=? AND c.target=? ORDER BY c.metric`, mac, set, side, target))
}

// perfCurrent is the Mac's current Omarchy values of the set for the
// Release, one per metric.
func (st *store) perfCurrent(mac, set, release string) ([]perf.Result, error) {
	return st.perfCurrentOf(mac, set, perf.SideOmarchy, release)
}

// perfCurrentMacOS is the Mac's macOS result of the set at the macOS build,
// one value per metric.
func (st *store) perfCurrentMacOS(mac, set, build string) ([]perf.Result, error) {
	return st.perfCurrentOf(mac, set, perf.SideMacOS, build)
}

// perfAttempts is every attempt of the Mac's runs of the set on side at
// target (a Release, or a macOS build), oldest first, each with what it
// uploaded.
func (st *store) perfAttempts(mac, set, side, target string) ([]perf.Attempt, error) {
	release, build := target, ""
	if side == perf.SideMacOS {
		release, build = "", target
	}
	rows, err := st.db.Query(`SELECT `+perfRunCols+` FROM perf_runs
WHERE mac=? AND set_name=? AND side=? AND release_version=? AND macos_build=? ORDER BY attempt`, mac, set, side, release, build)
	if err != nil {
		return nil, err
	}
	var out []perf.Attempt
	for rows.Next() {
		r, err := scanPerfRun(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, perf.Attempt{Run: r})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		res, err := scanPerfResults(st.db.Query(`SELECT `+perfResultCols+` FROM perf_results p
JOIN perf_runs r ON r.id = p.run WHERE p.run=? ORDER BY p.id`, out[i].Run.ID))
		if err != nil {
			return nil, err
		}
		out[i].Results = res
	}
	return out, nil
}

func (s *Server) perfRoutes() []route {
	return []route{
		{"POST /api/agent/performance/{run}/results", ownMac, s.hAgentPerfResults},
		{"POST /api/macos/runner/performance/{run}/results", anyone, s.macosRunner(s.hMacOSPerfResults)},
	}
}

// hAgentPerfResults takes lab-agent's results of a Performance run of this
// Mac's enrollment.
func (s *Server) hAgentPerfResults(w http.ResponseWriter, r *http.Request) {
	v := agentDev(r)
	v.mu.Lock()
	enrollment := v.d.ID
	v.mu.Unlock()
	s.takePerfUpload(w, r, perf.SideOmarchy, func(run perf.Run) bool { return run.Enrollment == enrollment })
}

// hMacOSPerfResults takes the macOS runner's results of this Mac's macOS run.
func (s *Server) hMacOSPerfResults(w http.ResponseWriter, r *http.Request) {
	mac := r.Context().Value(macosRunnerKey{}).(string)
	s.takePerfUpload(w, r, perf.SideMacOS, func(run perf.Run) bool { return run.Mac == mac })
}

const perfUploadMax = 1 << 20

func (s *Server) takePerfUpload(w http.ResponseWriter, r *http.Request, side string, owns func(perf.Run) bool) {
	id, err := strconv.ParseInt(r.PathValue("run"), 10, 64)
	var run perf.Run
	if err == nil {
		run, err = s.store.perfRun(id)
	}
	if err != nil || run.Side != side || !owns(run) {
		httpErr(w, 404, "no performance run %s for this Mac", r.PathValue("run"))
		return
	}
	if r.ContentLength > perfUploadMax {
		httpErr(w, 413, "a request body of %d bytes is over labd's limit of %d", r.ContentLength, perfUploadMax)
		return
	}
	var up perf.Upload
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, perfUploadMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&up); err != nil {
		code := 400
		if errors.As(err, new(*http.MaxBytesError)) {
			code = 413
		}
		httpErr(w, code, "%v", err)
		return
	}
	set, err := perfSet(run.Set)
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	c, err := perf.Check(up, set, side)
	if err != nil {
		httpErr(w, 400, "%s: %v", set.Name, err)
		return
	}
	t, err := s.store.takePerf(id, c, s.now())
	switch {
	case errors.Is(err, errPerfEnded):
		httpErr(w, 409, "%v", err)
		return
	case errors.Is(err, errPerfErrorRun):
		httpErr(w, 400, "%v", err)
		return
	case err != nil:
		httpErr(w, 500, "%v", err)
		return
	}
	if t.Taken > 0 || c.State != "" {
		s.log.Printf("%s: performance run %d (attempt %d, %s): %d results taken, %d had already; %s",
			run.Mac, t.Run, t.Attempt, run.Set, t.Taken, t.Already, t.State)
	}
	writeJSON(w, t)
}
