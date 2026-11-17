package server

// Releases and their verdicts. A Release is listed from the moment its
// manifest is frozen: its name, who froze it and when, its packages and its
// Macs, fixed at that moment. It carries one of four states: no verdict,
// qualified, not qualified, revoked. The first verdict is any M team
// member's; every later one a Maintainer's. A note is required every time.
// One thing is refused, "qualified" with no passed Installed run of the
// Release; everything else is a warning in the sentence the person
// confirms. Every verdict stays in the history, and nothing changes by
// itself: an Installed run that fails later adds a warning.
//
// Each person is kept as the account id and the handle they had then.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/detect"
	"github.com/omacom/omarchy-maclab/internal/installed"
)

// noteMax is how long a verdict's note may be.
const noteMax = 1000

// addRelease lists the Release with who approved its manifest, in its
// order, all at once. An approver who is not a person the lab knows when
// it is written, forgotten since the manifest was checked, is left out: the
// read follows a write, so a forgetting commits before all of it or after.
// An account id of 0, an approver a manifest frozen before named by handle,
// is kept as it was.
func (st *store) addRelease(r api.Release, approvers ...int64) error {
	pkgs, _ := json.Marshal(r.Packages)
	macs, _ := json.Marshal(r.Macs)
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO releases (name, frozen_at, frozen_by_id, frozen_by, manifest_sha256, kernel, packages, macs) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.FrozenAt.UnixMicro(), r.FrozenByID, r.FrozenBy, r.ManifestSHA256, r.Kernel, string(pkgs), string(macs)); err != nil {
		return err
	}
	for i, id := range approvers {
		if _, err := tx.Exec(`INSERT INTO release_approvals (release, position, account_id) SELECT ?1, ?2, ?3
			WHERE ?3 = 0 OR EXISTS (SELECT 1 FROM people WHERE account_id = ?3 AND handle != '')`, r.Name, i, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// approvers are the handles of who approved the Release's manifest, as
// the lab knows them now: a forgotten person is not among them.
func (st *store) approvers(release string) ([]string, error) {
	rows, err := st.db.Query(`SELECT p.handle FROM release_approvals a JOIN people p ON p.account_id = a.account_id
		WHERE a.release = ? AND a.account_id != 0 AND p.handle != '' ORDER BY a.position`, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

const releaseColumns = `name, frozen_at, frozen_by_id, frozen_by, manifest_sha256, kernel, packages, macs`

// releases is every Release as it was frozen, newest first; name keeps to
// one of them.
func (st *store) releases(name string) ([]api.Release, error) {
	rows, err := st.db.Query(`SELECT `+releaseColumns+` FROM releases WHERE ? = '' OR name = ? ORDER BY frozen_at DESC, name DESC`, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []api.Release{}
	for rows.Next() {
		var r api.Release
		var at int64
		var pkgs, macs string
		if err := rows.Scan(&r.Name, &at, &r.FrozenByID, &r.FrozenBy, &r.ManifestSHA256, &r.Kernel, &pkgs, &macs); err != nil {
			return nil, err
		}
		r.FrozenAt = time.UnixMicro(at)
		if err := errors.Join(json.Unmarshal([]byte(pkgs), &r.Packages), json.Unmarshal([]byte(macs), &r.Macs)); err != nil {
			return nil, fmt.Errorf("Release %s: %w", r.Name, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (st *store) addVerdict(release string, v api.ReleaseVerdict) error {
	w, _ := json.Marshal(v.Warnings)
	_, err := st.db.Exec(`INSERT INTO release_verdicts (release, at, state, note, account_id, handle, warnings) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		release, v.Time.UnixMicro(), v.State, v.Note, v.AccountID, v.Handle, string(w))
	return err
}

// verdicts is a Release's verdict history, newest first.
func (st *store) verdicts(release string) ([]api.ReleaseVerdict, error) {
	rows, err := st.db.Query(`SELECT at, state, note, account_id, handle, warnings FROM release_verdicts WHERE release = ? ORDER BY id DESC`, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.ReleaseVerdict
	for rows.Next() {
		var v api.ReleaseVerdict
		var at int64
		var w string
		if err := rows.Scan(&at, &v.State, &v.Note, &v.AccountID, &v.Handle, &w); err != nil {
			return nil, err
		}
		v.Time = time.UnixMicro(at)
		json.Unmarshal([]byte(w), &v.Warnings)
		out = append(out, v)
	}
	return out, rows.Err()
}

func (st *store) addEntryFrozen(release, mac, entrySHA string, at time.Time, by Person) error {
	_, err := st.db.Exec(`INSERT INTO release_entries (release, mac, at, account_id, handle, entry_sha256) VALUES (?, ?, ?, ?, ?, ?)`,
		release, mac, at.UnixMicro(), by.AccountID, by.Handle, entrySHA)
	return err
}

type entryFrozen struct {
	by string
	at time.Time
}

// entriesFrozen is who froze each Mac's installer entry of a Release.
func (st *store) entriesFrozen(release string) (map[string]entryFrozen, error) {
	rows, err := st.db.Query(`SELECT mac, at, handle FROM release_entries WHERE release = ?`, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]entryFrozen{}
	for rows.Next() {
		var mac string
		var e entryFrozen
		var at int64
		if err := rows.Scan(&mac, &at, &e.by); err != nil {
			return nil, err
		}
		e.at = time.UnixMicro(at)
		out[mac] = e
	}
	return out, rows.Err()
}

func (st *store) addPromotion(release, deviceID string, p api.ReleasePromotion) error {
	_, err := st.db.Exec(`INSERT INTO release_promotions (release, at, mac, device_id, job, kernel, account_id, handle) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		release, p.Time.UnixMicro(), p.Mac, deviceID, p.Job, p.Kernel, p.AccountID, p.Handle)
	return err
}

// promotions is the Promotions from a Release, oldest first.
func (st *store) promotions(release string) ([]api.ReleasePromotion, error) {
	rows, err := st.db.Query(`SELECT at, mac, job, kernel, account_id, handle FROM release_promotions WHERE release = ? ORDER BY id`, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.ReleasePromotion
	for rows.Next() {
		var p api.ReleasePromotion
		var at int64
		if err := rows.Scan(&at, &p.Mac, &p.Job, &p.Kernel, &p.AccountID, &p.Handle); err != nil {
			return nil, err
		}
		p.Time = time.UnixMicro(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// lastPromotion is the latest Promotion of one enrollment of a Mac: the
// Release it came from and the kernel it made the Mac's Known-good.
func (st *store) lastPromotion(deviceID string) (release, kernel string, err error) {
	err = st.db.QueryRow(`SELECT release, kernel FROM release_promotions WHERE device_id = ? ORDER BY id DESC LIMIT 1`, deviceID).Scan(&release, &kernel)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return release, kernel, err
}

// installedRuns is every Installed run of the Release whose expected
// manifest has this sha256, newest first.
func (st *store) installedRuns(manifestSHA string) ([]*api.Job, error) {
	return scanJobs(st.db.Query(`SELECT data FROM jobs WHERE json_extract(data, '$.spec.installed_manifest') = ? ORDER BY created DESC`, manifestSHA))
}

// listRelease puts a Release on the list as its manifest is frozen: by is
// who froze it, and its Macs are the manifest's, each with the enrollment
// and Configuration labd knows it by at this moment.
func (s *Server) listRelease(m *installed.Manifest, manifestSHA string, by Person, at time.Time) error {
	r := api.Release{Name: m.Release, FrozenBy: by.Handle, FrozenByID: by.AccountID, FrozenAt: at, ManifestSHA256: manifestSHA, Kernel: m.Kernel.Release,
		Packages: []api.ReleasePackage{}, Macs: []api.ReleaseMac{}}
	for _, a := range m.Artifacts {
		r.Packages = append(r.Packages, api.ReleasePackage{Name: a.Name, Version: a.Version})
	}
	for name, e := range m.Macs {
		mac := api.ReleaseMac{Mac: name, Board: e.Board}
		conf := api.Configuration{Board: detect.Board([]string{e.Board})}
		if conf.Board == "" {
			conf.Board = e.Board
		}
		if v := s.dev(name); v != nil {
			mac.DeviceID = v.snapshot().ID
			if c := s.configuration(v); c.Board != "" {
				conf = c
			}
		}
		mac.Configuration = conf.String()
		r.Macs = append(r.Macs, mac)
	}
	sort.Slice(r.Macs, func(i, j int) bool { return r.Macs[i].Mac < r.Macs[j].Mac })
	var ids []int64
	for _, a := range m.ApprovedBy {
		ids = append(ids, a.AccountID)
	}
	return s.store.addRelease(r, ids...)
}

// approversOf is why the manifest's approved_by is refused, or nil: from
// 2026-11-16 it names each approver by the account id of a person the lab
// knows, once.
func (s *Server) approversOf(m *installed.Manifest) error {
	seen := map[int64]bool{}
	for _, a := range m.ApprovedBy {
		if a.Handle != "" || a.AccountID <= 0 {
			what := strconv.FormatInt(a.AccountID, 10)
			if a.Handle != "" {
				what = strconv.Quote(a.Handle)
			}
			return refuse(400, "approved_by names each approver by account id (the number on their People page): %s is not one", what)
		}
		if seen[a.AccountID] {
			return refuse(400, "approved_by names account id %d twice", a.AccountID)
		}
		seen[a.AccountID] = true
		if k, err := s.store.knownPerson(a.AccountID); err != nil || k.Handle == "" {
			return refuse(400, "approved_by names account id %d, whom the lab does not know", a.AccountID)
		}
	}
	return nil
}

// adoptReleases lists every Release whose manifest is frozen in the data
// directory and that has no record: one frozen before labd kept Releases.
// Who froze it is not known; when is the time its pins were written.
func (s *Server) adoptReleases() {
	pins, _ := filepath.Glob(filepath.Join(s.cfg.DataDir, "frozen", "*", "PINS.expected"))
	for _, p := range pins {
		dir := filepath.Dir(p)
		name := filepath.Base(dir)
		if have, err := s.store.releases(name); err != nil || len(have) > 0 {
			continue
		}
		m, sha, err := frozenManifest(dir)
		if err == nil && m.Release != name {
			err = fmt.Errorf("its manifest is for Release %q", m.Release)
		}
		var at time.Time
		if st, serr := os.Stat(p); serr == nil {
			at = st.ModTime()
		}
		if err == nil {
			err = s.listRelease(m, sha, Person{}, at)
		}
		if err != nil {
			s.log.Printf("the frozen Release %s is not listed: %v", name, err)
		}
	}
}

// frozenManifest reads a Release's expected manifest from its frozen store,
// checked against its pin.
func frozenManifest(dir string) (*installed.Manifest, string, error) {
	exp, err := readPins(filepath.Join(dir, "PINS.expected"))
	if err != nil {
		return nil, "", err
	}
	mb, sha, err := pinned(dir, "expected/manifest.json", exp)
	if err != nil {
		return nil, "", err
	}
	var m installed.Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, "", err
	}
	return &m, sha, nil
}

// failedRun is whether a finished Installed run speaks against its
// Release: it ran and did not pass. A run that was canceled, or that the
// lab itself failed, says nothing.
func failedRun(j *api.Job) bool {
	if j.State != api.JobDone {
		return false
	}
	res := gateResult(j.Outcome)
	return res != "" && res != api.GatePass
}

func passedRun(j *api.Job) bool { return j.State == api.JobDone && j.Outcome == api.OutcomePass }

// releaseNow is the Release called name with what happened since its
// freeze: its state and verdict history, each Mac's entry, Installed runs
// and Promotion, and its warnings. A refusal with 404 when none is listed.
func (s *Server) releaseNow(name string) (*api.Release, error) {
	found, err := s.store.releases(name)
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, refuse(404, "no Release %q: a Release is listed once its manifest is frozen (`lab installed freeze-manifest`)", name)
	}
	r := &found[0]
	if err := s.fillRelease(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (s *Server) fillRelease(r *api.Release) error {
	var err error
	if r.Verdicts, err = s.store.verdicts(r.Name); err != nil {
		return err
	}
	if r.ApprovedBy, err = s.store.approvers(r.Name); err != nil {
		return err
	}
	r.State = api.ReleaseNoVerdict
	if len(r.Verdicts) > 0 {
		r.State = r.Verdicts[0].State
		at := r.Verdicts[0].Time
		r.StateAt = &at
	}
	entries, err := s.store.entriesFrozen(r.Name)
	if err != nil {
		return err
	}
	runs, err := s.store.installedRuns(r.ManifestSHA256)
	if err != nil {
		return err
	}
	promos, err := s.store.promotions(r.Name)
	if err != nil {
		return err
	}
	passed := map[string]bool{}
	confs := map[string]bool{}
	for i := range r.Macs {
		m := &r.Macs[i]
		confs[m.Configuration] = true
		if e, ok := entries[m.Mac]; ok {
			at := e.at
			m.EntryFrozenBy, m.EntryFrozenAt = e.by, &at
		}
		for _, j := range runs { // newest first
			if j.Spec.Device != m.Mac || m.DeviceID != "" && j.DeviceID != "" && j.DeviceID != m.DeviceID {
				continue
			}
			m.Runs++
			if m.LatestRun == "" {
				m.LatestRun, m.LatestOutcome = j.ID, j.Outcome
				if j.State != api.JobDone {
					m.LatestOutcome = string(j.State)
				}
			}
			if m.PassedRun == "" && passedRun(j) {
				m.PassedRun = j.ID
			}
			if r.StateAt != nil && failedRun(j) && j.Updated.After(*r.StateAt) {
				r.Warnings = append(r.Warnings, fmt.Sprintf("Installed run %s on %s failed after this verdict (%s)", j.ID, m.Mac, j.Outcome))
			}
		}
		if m.PassedRun != "" {
			passed[m.Configuration] = true
		}
		for _, p := range promos {
			if p.Mac == m.Mac {
				p := p
				if r.State == api.ReleaseRevoked {
					p.Mark = api.PromotedFromRevoked
				}
				m.Promoted = &p
			}
		}
		if m.Promoted != nil {
			r.Promoted++
		}
	}
	r.Configurations, r.Passed = len(confs), len(passed)
	return nil
}

// promotedFrom is the Release v was last promoted from, while the kernel
// that Promotion made its Known-good still is, and whether that Release's
// Qualification has been revoked since: the Mac is then "promoted from a
// revoked Release".
func (s *Server) promotedFrom(v *dev) (release string, revoked bool) {
	d := v.snapshot()
	release, kernel, err := s.store.lastPromotion(d.ID)
	if err != nil || release == "" || kernel != d.KnownGood {
		return "", false
	}
	h, err := s.store.verdicts(release)
	return release, err == nil && len(h) > 0 && h[0].State == api.ReleaseRevoked
}

// stateWords says a state as a sentence does.
func stateWords(state string) string {
	switch state {
	case api.ReleaseQualified:
		return "qualified"
	case api.ReleaseNotQualified:
		return "not qualified"
	case api.ReleaseRevoked:
		return "revoked"
	}
	return "no verdict"
}

func coverage(r *api.Release) string {
	return fmt.Sprintf("%d of %d %s a passed Installed run", r.Passed, r.Configurations, plural(r.Configurations, "Configuration has", "Configurations have"))
}

// verdict records by's verdict on a Release, once it is confirmed. Before
// that it answers with the sentence to confirm and records nothing. The
// Releases page and `lab installed verdict` both come through here.
func (s *Server) verdict(by Person, release string, req api.VerdictRequest) (api.ReleaseAnswer, error) {
	var a api.ReleaseAnswer
	note := strings.TrimSpace(req.Note)
	switch {
	case !by.OnMTeam():
		return a, refuse(403, "a verdict is the M team's and the Maintainers' to record")
	case req.State != api.ReleaseQualified && req.State != api.ReleaseNotQualified && req.State != api.ReleaseRevoked:
		return a, refuse(400, "a verdict is qualified, not_qualified or revoked, not %q", req.State)
	case note == "":
		return a, refuse(http.StatusUnprocessableEntity, "A verdict needs a note every time: say why.")
	case len([]rune(note)) > noteMax:
		return a, refuse(http.StatusUnprocessableEntity, "A verdict's note is at most %d characters.", noteMax)
	}
	s.releaseMu.Lock()
	defer s.releaseMu.Unlock()
	r, err := s.releaseNow(release)
	if err != nil {
		return a, err
	}
	var warnings []string
	switch {
	case req.State == api.ReleaseRevoked && !by.isMaintainer():
		return a, refuse(403, "only a Maintainer revokes a verdict")
	case req.State == api.ReleaseRevoked && r.State == api.ReleaseNoVerdict:
		return a, refuse(409, "Release %s has no verdict to revoke", r.Name)
	case req.State == api.ReleaseRevoked && r.State == api.ReleaseRevoked:
		return a, refuse(409, "Release %s's verdict is already revoked", r.Name)
	case len(r.Verdicts) > 0 && !by.isMaintainer():
		return a, refuse(403, "Release %s already has a verdict (%s, by @%s): only a Maintainer changes it, revokes it or records one again", r.Name, stateWords(r.State), r.Verdicts[0].Handle)
	case req.State == api.ReleaseQualified && r.Passed == 0:
		return a, refuse(409, "Release %s has no passed Installed run, so it cannot be recorded as qualified", r.Name)
	}
	if len(r.Verdicts) > 0 {
		was := r.Verdicts[0]
		warnings = append(warnings, fmt.Sprintf("this replaces \"%s\", recorded by @%s on %s", stateWords(was.State), was.Handle, was.Time.UTC().Format("2006-01-02")))
	}
	switch req.State {
	case api.ReleaseQualified:
		if r.Passed < r.Configurations {
			var without []string
			for _, m := range r.Macs {
				if m.PassedRun == "" {
					without = append(without, m.Mac)
				}
			}
			warnings = append(warnings, fmt.Sprintf("only %s; none on %s", coverage(r), strings.Join(without, ", ")))
		}
		for _, m := range r.Macs {
			if m.LatestRun != "" && m.LatestRun != m.PassedRun && m.LatestOutcome != "" && m.LatestOutcome != api.OutcomeCanceled {
				warnings = append(warnings, fmt.Sprintf("the latest Installed run on %s, %s, did not pass (%s)", m.Mac, m.LatestRun, m.LatestOutcome))
			}
		}
	case api.ReleaseNotQualified:
		if r.Configurations > 0 && r.Passed == r.Configurations {
			warnings = append(warnings, coverage(r))
		}
	case api.ReleaseRevoked:
		if r.Promoted > 0 {
			var names []string
			for _, m := range r.Macs {
				if m.Promoted != nil {
					names = append(names, m.Mac)
				}
			}
			warnings = append(warnings, fmt.Sprintf("revoking undoes nothing: %s %s promoted from it and will stay promoted, marked \"%s\"",
				strings.Join(names, ", "), plural(len(names), "was", "were"), api.PromotedFromRevoked))
		}
	}
	if !req.Confirm {
		what := fmt.Sprintf("Record \"%s\" for Release %s", stateWords(req.State), r.Name)
		if req.State == api.ReleaseRevoked {
			what = fmt.Sprintf("Revoke the verdict on Release %s (\"%s\")", r.Name, stateWords(r.State))
		}
		a.Confirm = fmt.Sprintf("%s as @%s, with the note %q?", what, by.Handle, note)
		if len(warnings) > 0 {
			a.Confirm += " Warning: " + strings.Join(warnings, "; ") + "."
		}
		a.Warnings = warnings
		return a, nil
	}
	if err := s.store.addVerdict(r.Name, api.ReleaseVerdict{Time: s.now(), State: req.State, Note: note, AccountID: by.AccountID, Handle: by.Handle, Warnings: warnings}); err != nil {
		return a, err
	}
	s.log.Printf("Release %s: %s recorded by %s", r.Name, stateWords(req.State), accountOr(by.AccountID, "someone"))
	if a.Release, err = s.releaseNow(r.Name); err != nil {
		return a, err
	}
	a.Warnings = warnings
	return a, nil
}

// hReleases lists every Release with its verdict, newest first.
func (s *Server) hReleases(w http.ResponseWriter, r *http.Request) {
	all, err := s.store.releases("")
	if err != nil {
		httpErr(w, 500, "%v", err)
		return
	}
	for i := range all {
		if err := s.fillRelease(&all[i]); err != nil {
			httpErr(w, 500, "%v", err)
			return
		}
	}
	writeJSON(w, all)
}

func (s *Server) hVerdict(w http.ResponseWriter, r *http.Request) {
	var req api.VerdictRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErr(w, 400, "send {\"state\": \"qualified|not_qualified|revoked\", \"note\": \"...\"}")
		return
	}
	a, err := s.verdict(leaser(r), r.PathValue("release"), req)
	if err != nil {
		httpErr(w, releaseStatus(err), "%v", err)
		return
	}
	writeJSON(w, a)
}

// releaseStatus is the status labd answers err with: a refusal's own, and
// 500 for anything else.
func releaseStatus(err error) int {
	var r *refusal
	if errors.As(err, &r) {
		return r.code
	}
	return 500
}
