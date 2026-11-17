package server

// Forgetting a person (spec section 14, "Forgetting a person"). The person
// asks on their own page, or by mail; a Maintainer carries it out with one
// button, an Admin for a Maintainer, and it cannot be undone. Everything the
// person had ends as when a Maintainer ends it (sessions, Lab tokens,
// Leases, Mac sign-ins, the Macs they lend, what was on the way to lending),
// their record goes, and everything else labd keeps loses their handle and
// account id: both databases, labd's copies of Audit logs, what the frozen
// Releases keep but their pinned files, the caller log, and the lines for
// Signal, sent or not. Exec record lines keep the handle until they are 12
// months old. A Blocked person stays Blocked: the account id, the date and
// who blocked are kept. No list of forgettings is kept; the lines for one
// name only who carried it out.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// forgetWithin is how long a Maintainer has to carry out a request.
const forgetWithin = 30 * 24 * time.Hour

// forgottenWord stands where a forgotten person's handle stood in words. A
// field that held the handle alone is left empty instead.
const forgottenWord = "someone"

// leftForgotten is the Retired record's how for a Mac whose Volunteer was
// Forgotten.
const leftForgotten = "forgotten"

// forgetting is one person being forgotten: how their handles and account
// id are found in what labd keeps. handles are the handle of today first,
// then those the lab kept beside their account id before a rename.
type forgetting struct {
	id      int64
	idText  string
	handles []string
}

func newForgetting(id int64, handle string, earlier ...string) forgetting {
	f := forgetting{id: id, idText: strconv.FormatInt(id, 10)}
	for _, h := range append([]string{handle}, earlier...) {
		if h != "" && !slices.ContainsFunc(f.handles, func(have string) bool { return strings.EqualFold(have, h) }) {
			f.handles = append(f.handles, h)
		}
	}
	return f
}

// handleByte is a byte a GitHub handle may hold: a name is a mention of the
// handle only where neither side goes on with one.
func handleByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-'
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// words is text with every mention of a handle, "@" and all, in any case,
// replaced by forgottenWord.
func (f forgetting) words(s string) string {
	for _, h := range f.handles {
		s = wordsWithout(s, h)
	}
	return s
}

func wordsWithout(s, handle string) string {
	lower, h := asciiLower(s), asciiLower(handle)
	var b strings.Builder
	i := 0
	for {
		j := strings.Index(lower[i:], h)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		j += i
		end := j + len(h)
		if (j > 0 && handleByte(s[j-1])) || (end < len(s) && handleByte(s[end])) {
			b.WriteString(s[i : j+1])
			i = j + 1
			continue
		}
		start := j
		if start > 0 && s[start-1] == '@' && (start == 1 || !handleByte(s[start-2])) {
			start--
		}
		b.WriteString(s[i:start])
		b.WriteString(forgottenWord)
		i = end
	}
}

// value is a stored value with the person taken out: one that names a
// handle alone becomes empty, and words lose their mentions.
func (f forgetting) value(s string) string {
	name := strings.TrimPrefix(strings.TrimSpace(s), "@")
	if slices.ContainsFunc(f.handles, func(h string) bool { return strings.EqualFold(name, h) }) {
		return ""
	}
	return f.words(s)
}

// keptNames are the keys of a stored object that hold a Mac's, a job's or a
// Release's name, which is left as it was typed.
var keptNames = map[string]bool{"name": true, "mac": true, "device": true, "id": true, "device_id": true, "release": true, "job": true}

// idKey is a key or a column that holds a person's account id.
func idKey(key string) bool {
	k := asciiLower(key)
	return strings.HasSuffix(k, "_id") || strings.HasSuffix(k, "_by") || k == "accountid"
}

// json is a stored JSON document with the person taken out, and whether
// anything changed. Numbers are kept as written.
func (f forgetting) json(raw string) (string, bool) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw, false
	}
	v, changed := f.walk("", v)
	if !changed {
		return raw, false
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return raw, false
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

func (f forgetting) walk(key string, v any) (any, bool) {
	switch x := v.(type) {
	case string:
		if keptNames[asciiLower(key)] {
			return x, false
		}
		y := f.value(x)
		return y, y != x
	case json.Number:
		if idKey(key) && x.String() == f.idText {
			return json.Number("0"), true
		}
	case map[string]any:
		did := false
		for k, e := range x {
			if n, changed := f.walk(k, e); changed {
				x[k], did = n, true
			}
		}
		return x, did
	case []any:
		did := false
		for i, e := range x {
			if n, changed := f.walk(key, e); changed {
				x[i], did = n, true
			}
		}
		return x, did
	}
	return v, false
}

// stored is a stored text with the person taken out: a JSON document by its
// keys, anything else as words.
func (f forgetting) stored(s string) string {
	if t := strings.TrimSpace(s); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		out, _ := f.json(s)
		return out
	}
	return f.value(s)
}

// accountOr names a person in labd's own log, which a forgetting does not
// reach: by account id, or def for nobody.
func accountOr(id int64, def string) string {
	if id == 0 {
		return def
	}
	return "account " + strconv.FormatInt(id, 10)
}

// ---- who may forget whom ----

// errForgottenAlready is a Blocked account forgotten before: all that is
// left of it is that it is Blocked.
var errForgottenAlready = refuse(http.StatusConflict, "This account is forgotten already: the lab keeps only that it is Blocked.")

func mayForget(by *Person, k knownPerson) error {
	switch {
	case k.Admin:
		return refuse(http.StatusForbidden, "An Admin is named in the server's configuration: take them out of it before forgetting them.")
	case k.Handle == "" && k.Blocked:
		return errForgottenAlready
	case k.Maintainer && !by.Admin:
		return refuse(http.StatusForbidden, "Only an Admin forgets a Maintainer.")
	}
	return nil
}

// forgetEnds is the sentence Forget states before it is pressed again.
func (s *Server) forgetEnds(k knownPerson) string {
	var ends []string
	if n, _ := s.store.countSessions(k.AccountID, s.now()); n > 0 {
		ends = append(ends, counted(n, "browser", "browsers")+" signed out")
	}
	if tokens, _ := s.store.labTokens(k.AccountID); len(tokens) > 0 && !k.Blocked {
		ends = append(ends, counted(len(tokens), "Lab token", "Lab tokens")+" stopped")
	}
	if n := len(s.leasesOf(k.Person)); n > 0 {
		ends = append(ends, counted(n, "Lease", "Leases")+" ended")
	}
	if signIns, _ := s.reports.SignInsOf(k.AccountID); len(signIns) > 0 {
		ends = append(ends, counted(len(signIns), "Mac", "Macs")+" signed out")
	}
	if n := len(s.macsLentBy(k.AccountID)); n > 0 {
		ends = append(ends, counted(n, "Mac leaves", "Macs leave")+" the lab")
	}
	out := "Forget @" + k.Handle + " now: everything the lab keeps of them goes"
	if len(ends) > 0 {
		out += ", " + strings.Join(ends, ", ")
	}
	if k.Blocked {
		out += "; the account stays Blocked"
	}
	return out + ". It cannot be undone"
}

// ---- the request ----

// askToBeForgotten records the person's request, once.
func (st *store) askToBeForgotten(accountID int64, now time.Time) (bool, error) {
	res, err := st.db.Exec(`UPDATE people SET forget_asked = ? WHERE account_id = ? AND forget_asked IS NULL`, now.UnixMicro(), accountID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// forgetAsked is when the person asked to be forgotten, zero if they did not.
func (st *store) forgetAsked(accountID int64) (time.Time, error) {
	var at sql.NullInt64
	err := st.db.QueryRow(`SELECT forget_asked FROM people WHERE account_id = ?`, accountID).Scan(&at)
	if err != nil || !at.Valid {
		return time.Time{}, err
	}
	return time.UnixMicro(at.Int64), nil
}

// forgetRequest is a request to be Forgotten, waiting on People.
type forgetRequest struct {
	AccountID         int64
	Handle, Asked, By string
	// Maintainer is a request only an Admin carries out.
	Maintainer bool
	// Sentence is Forget's for the viewer, Why why they may not.
	Sentence, Why string
}

func (st *store) forgetRequests() ([]forgetRequest, error) {
	rows, err := st.db.Query(`SELECT account_id, handle, maintainer, forget_asked FROM people WHERE forget_asked IS NOT NULL ORDER BY forget_asked, account_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []forgetRequest
	for rows.Next() {
		var q forgetRequest
		var at int64
		if err := rows.Scan(&q.AccountID, &q.Handle, &q.Maintainer, &at); err != nil {
			return nil, err
		}
		q.Asked, q.By = time.UnixMicro(at).UTC().Format(pageDate), time.UnixMicro(at).Add(forgetWithin).UTC().Format(pageDate)
		out = append(out, q)
	}
	return out, rows.Err()
}

// forgetRequestsWaiting is how many requests to be Forgotten wait.
func (s *Server) forgetRequestsWaiting() int {
	var n int
	if err := s.store.db.QueryRow(`SELECT count(*) FROM people WHERE forget_asked IS NOT NULL`).Scan(&n); err != nil {
		s.log.Printf("requests to be forgotten: %v", err)
	}
	return n
}

// ---- carrying it out ----

// forgetPerson forgets k for the person signed in on r.
func (s *Server) forgetPerson(r *http.Request, k knownPerson) error {
	by := *PersonOf(r)
	if err := mayForget(&by, k); err != nil {
		return err
	}
	audited, err := auditedHandles(filepath.Join(s.cfg.DataDir, "audit"), k.AccountID)
	if err != nil {
		return fmt.Errorf("the Audit log copies: %w", err)
	}
	earlier, err := s.store.earlierHandles(k.AccountID, k.Handle, audited...)
	if err != nil {
		return fmt.Errorf("labd.db: %w", err)
	}
	f := newForgetting(k.AccountID, k.Handle, earlier...)

	// No line leaves for Signal until the handle is off every one.
	s.lines.sending.Lock()
	err = s.forgetHeld(by, k, f)
	s.lines.sending.Unlock()
	if err != nil {
		return err
	}
	at := s.now()
	if err := s.store.addRecord(recordLine{At: at, ActorID: by.AccountID, Actor: by.Handle, What: "carried out a forgetting"}); err != nil {
		s.log.Printf("the People record: %v", err)
	}
	s.agents.Forgot(by.Handle, at)
	return nil
}

func (s *Server) forgetHeld(by Person, k knownPerson, f forgetting) error {
	// What the person had ends as when a Maintainer ends it. Each ending
	// writes its lines with the handle, which come off below.
	if err := s.store.endAllAccess(k.AccountID); err != nil {
		return err
	}
	s.changed(k.AccountID)
	s.endLendingOf(k.Person, by)
	s.endLeasesOf(k.Person, endedByMaintainer(by, ""))
	if err := s.endEnrollmentsOf(k.AccountID, leaving{how: leftForgotten, by: by}); err != nil {
		return refuse(http.StatusInternalServerError, "Nothing is forgotten yet: a Mac they lend could not leave the lab (%v). Forget again.", err)
	}

	// An Installed run being sealed publishes its record before or after.
	s.forgetting.Lock()
	defer s.forgetting.Unlock()
	end, err := s.forgetInJobs(f)
	if err != nil {
		return fmt.Errorf("the jobs: %w", err)
	}
	defer end()
	// The files before the record: one that cannot be swept fails the
	// forgetting while the person is still there to forget again.
	if err := s.forgetInFilesOf(f); err != nil {
		return err
	}
	if err := s.reports.Forget(f.id, f.handles...); err != nil {
		return fmt.Errorf("reports.db: %w", err)
	}
	if err := s.store.forget(f); err != nil {
		return fmt.Errorf("labd.db: %w", err)
	}
	s.forgetInFleet(f)
	// Once more, for what was written to them while the databases were.
	if err := s.forgetInFilesOf(f); err != nil {
		s.log.Printf("a forgetting, once more: %v", err)
	}
	return nil
}

// forgetInFilesOf takes the person off the files labd keeps beside its
// databases: its copies of Audit logs, what the frozen Releases keep but
// their pinned files, and the caller log.
func (s *Server) forgetInFilesOf(f forgetting) error {
	if err := s.forgetInAudit(f); err != nil {
		return fmt.Errorf("the Audit log copies: %w", err)
	}
	if err := forgetInFrozen(filepath.Join(s.cfg.DataDir, "frozen"), filepath.Join(s.cfg.DataDir, "jobs"), f); err != nil {
		return fmt.Errorf("the frozen Releases: %w", err)
	}
	if err := s.callers.forget(f); err != nil {
		return fmt.Errorf("the caller log: %w", err)
	}
	return nil
}

// forgetInAudit takes the person off labd's copies of Audit logs while no
// Mac's entries are being added, a retired Mac's and one Joined during the
// sweep included: an entry added between the sweep's read and its write
// would be lost, and one matched to the person before the forgetting and
// added after it would keep their handle.
func (s *Server) forgetInAudit(f forgetting) error {
	s.auditWrite.Lock()
	defer s.auditWrite.Unlock()
	return forgetInFiles(filepath.Join(s.cfg.DataDir, "audit"), f)
}

// auditedHandles are the handles labd's copies of Audit logs keep beside
// this account id.
func auditedHandles(dir string, id int64) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			var e api.AuditEntry
			if json.Unmarshal([]byte(line), &e) == nil && e.AccountID == id && id != 0 && e.Maintainer != "" {
				out = append(out, e.Maintainer)
			}
		}
		return nil
	})
	return out, err
}

// forgetInFleet takes the person off the Macs enrolled now, as labd holds
// them: a Lease's attribution, a recovery's holder, a login linked to a Mac.
func (s *Server) forgetInFleet(f forgetting) {
	for _, v := range s.allDevs() {
		v.mu.Lock()
		b, err := json.Marshal(v.d)
		if err == nil {
			if out, changed := f.json(string(b)); changed {
				var d api.Device
				if json.Unmarshal([]byte(out), &d) == nil {
					v.d = d
					v.saveLocked()
				}
			}
		}
		v.mu.Unlock()
	}
}

// jobsInFlight are the forgettings carried out since each job was stored,
// which every later save of any copy of it applies again: a run's own copy
// too, which only the run's goroutine writes, at its next save (a forgetting
// that rewrote it in place would race the run's reads). Those stay for as
// long as labd runs, on a finished job too: a copy read before the
// forgetting, a cancel's or a publishing's, can be saved at any time after
// it, and a restart leaves no such copy. under are the forgettings under
// way: every whole job saved meanwhile, done or not, loses their person too,
// as one read before the forgetting swept the jobs table would bring them
// back. Under jobMu.
type jobsInFlight struct {
	forgot map[string][]forgetting
	under  []*forgetting
}

// forgottenLocked takes off j the people forgotten since it was read: the
// forgettings carried out since it was stored, and those under way. Under
// jobMu, before j is saved.
func (s *Server) forgottenLocked(j *api.Job) {
	fl := &s.inFlight
	for _, f := range fl.forgot[j.ID] {
		f.job(j)
	}
	for _, f := range fl.under {
		f.job(j)
	}
}

// saveJobLocked saves a whole job read outside a run, say to set it aside
// in lab gate, without the people forgotten since it was read. Under jobMu.
func (s *Server) saveJobLocked(j *api.Job) error {
	s.forgottenLocked(j)
	return s.store.saveJob(j)
}

// forgetInJobs takes the person off the jobs not done yet and the finished
// jobs that name them: every copy saved from now on, runs' own included,
// which would otherwise write them back whole; and off every job
// saved until end is called, once the forgetting is done with the jobs table
// and the files.
func (s *Server) forgetInJobs(f forgetting) (end func(), err error) {
	jobMu.Lock()
	defer jobMu.Unlock()
	jobs, err := s.store.jobs("", -1, false)
	if err != nil {
		return func() {}, err
	}
	fl := &s.inFlight
	if fl.forgot == nil {
		fl.forgot = map[string][]forgetting{}
	}
	for _, j := range jobs {
		if j.State != api.JobDone || f.names(j) {
			fl.forgot[j.ID] = append(fl.forgot[j.ID], f)
		}
	}
	under := &f
	fl.under = append(fl.under, under)
	return func() {
		jobMu.Lock()
		defer jobMu.Unlock()
		fl.under = slices.DeleteFunc(fl.under, func(u *forgetting) bool { return u == under })
	}, nil
}

// names is whether j names the person. It takes them off j to find out.
func (f forgetting) names(j *api.Job) bool {
	before, err := json.Marshal(j)
	if err != nil {
		return true
	}
	f.job(j)
	after, err := json.Marshal(j)
	return err != nil || !bytes.Equal(before, after)
}

// job takes the person off j, in place. An Installed run's result is left
// alone: it names nobody, and the run writes it as it goes, outside jobMu.
func (f forgetting) job(j *api.Job) {
	installed := j.Result.Installed
	j.Result.Installed = nil
	defer func() { j.Result.Installed = installed }()
	b, err := json.Marshal(j)
	if err != nil {
		return
	}
	out, changed := f.json(string(b))
	if !changed {
		return
	}
	var n api.Job
	if json.Unmarshal([]byte(out), &n) == nil {
		*j = n
	}
}

// unscrubbed are the tables a forgetting leaves alone: the Exec record keeps
// the handle until each line is 12 months old.
var unscrubbed = map[string]bool{"exec_record": true}

// forget erases the person's record, or for a Blocked person keeps only the
// Blocked state, and takes their handle and account id off every other
// table but the Exec record, in one transaction. Whether they are Blocked is
// read inside it: a Block that landed since the person was read holds.
func (st *store) forget(f forgetting) error {
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	exec := func(q string, args ...any) {
		if err == nil {
			_, err = tx.Exec(q, args...)
		}
	}
	exec(`DELETE FROM browser_sessions WHERE account_id = ?`, f.id)
	var blocked bool
	if err == nil {
		// After a write: no Block commits between this read and the rest.
		err = tx.QueryRow(`SELECT blocked_at IS NOT NULL FROM people WHERE account_id = ?`, f.id).Scan(&blocked)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
	}
	exec(`DELETE FROM lab_tokens WHERE account_id = ?`, f.id)
	exec(`DELETE FROM requests_to_lend WHERE account_id = ?`, f.id)
	// A frozen manifest keeps its bytes; the lab stops showing them as its approver.
	exec(`DELETE FROM release_approvals WHERE account_id = ?`, f.id)
	exec(`DELETE FROM tokens WHERE token_hash IN (SELECT token_hash FROM join_commands WHERE volunteer_id = ?)`, f.id)
	exec(`DELETE FROM join_commands WHERE volunteer_id = ?`, f.id)
	// What others typed about them goes with the handle: a Block's reason.
	exec(`UPDATE people_record SET note = '' WHERE about_id = ?`, f.id)
	if blocked {
		exec(`UPDATE people SET handle = '', blocked_reason = '', last_seen = NULL, added = blocked_at, lend_cap = NULL, forget_asked = NULL,
			maintainer = 0, m_team = 0, leave_to_lend = 0 WHERE account_id = ?`, f.id)
	} else {
		exec(`DELETE FROM people WHERE account_id = ?`, f.id)
	}
	if err != nil {
		return err
	}
	if err := forgetRetired(tx, f); err != nil {
		return err
	}
	if err := forgetEverywhere(tx, f); err != nil {
		return err
	}
	return tx.Commit()
}

// forgetRetired takes the Volunteer off the records of their Retired Macs:
// who they were, how to reach them, and the terms they accepted.
func forgetRetired(tx *sql.Tx, f forgetting) error {
	rows, err := tx.Query(`SELECT id, data FROM retired_macs WHERE volunteer_id = ?`, f.id)
	if err != nil {
		return err
	}
	type retired struct{ id, data string }
	var all []retired
	for rows.Next() {
		var m retired
		if err := rows.Scan(&m.id, &m.data); err != nil {
			rows.Close()
			return err
		}
		all = append(all, m)
	}
	rows.Close()
	for _, m := range all {
		var d api.Device
		if err := json.Unmarshal([]byte(m.data), &d); err != nil {
			return err
		}
		d.VolunteerID, d.VolunteerName, d.Contact, d.NtfyOwnURL = 0, "", "", ""
		d.TermsSHA256, d.TermsAccepted = "", time.Time{}
		b, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE retired_macs SET volunteer_id = 0, data = ? WHERE id = ?`, string(b), m.id); err != nil {
			return err
		}
	}
	return nil
}

// forgetEverywhere goes through every table but the unscrubbed: a column
// that holds an account id loses theirs (to 0), a text loses their handle.
// The Blocked state's own account id stays, and so does every Mac's name.
// Their own row is gone or emptied already.
func forgetEverywhere(tx *sql.Tx, f forgetting) error {
	rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var t string
		rows.Scan(&t)
		tables = append(tables, t)
	}
	rows.Close()
	for _, table := range tables {
		if unscrubbed[table] {
			continue
		}
		cols, err := tx.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		type column struct{ name, kind string }
		var columns []column
		for cols.Next() {
			var c column
			cols.Scan(&c.name, &c.kind)
			columns = append(columns, c)
		}
		cols.Close()
		// A name kept beside their account id is theirs, whatever it says.
		if table != "people" {
			for _, c := range columns {
				if beside := besideID(c.name, func(name string) bool {
					return slices.ContainsFunc(columns, func(t column) bool { return t.name == name && textKind(t.kind) })
				}); beside != "" && strings.EqualFold(c.kind, "INTEGER") {
					if _, err := tx.Exec(`UPDATE "`+table+`" SET "`+beside+`" = '' WHERE "`+c.name+`" = ?`, f.id); err != nil {
						return err
					}
				}
			}
		}
		for _, c := range columns {
			q := `"` + table + `"`
			name := `"` + c.name + `"`
			switch {
			// The people left are another person's each: a handle there is
			// theirs, even one the forgotten person once had too.
			case table == "people" && (c.name == "account_id" || c.name == "handle"), keptNames[asciiLower(c.name)]:
			case strings.EqualFold(c.kind, "INTEGER"):
				if idKey(c.name) {
					if _, err := tx.Exec(`UPDATE `+q+` SET `+name+` = 0 WHERE `+name+` = ?`, f.id); err != nil {
						return err
					}
				}
			case textKind(c.kind):
				if err := forgetInColumn(tx, q, name, f); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func textKind(kind string) bool { return strings.EqualFold(kind, "TEXT") || kind == "" }

// besideID is the column that holds the name of the person whose account id
// column holds (handle beside account_id, actor beside actor_id), or "" when
// has finds none.
func besideID(column string, has func(string) bool) string {
	name := strings.TrimSuffix(column, "_id")
	if column == "account_id" {
		name = "handle"
	}
	if name == column || !has(name) {
		return ""
	}
	return name
}

// earlierHandles are the handles other than this one that labd kept beside
// the account id, in its database and as kept (found elsewhere): what the
// person was called before a rename on GitHub. A handle another person has
// today is theirs, and is left out.
func (st *store) earlierHandles(id int64, handle string, kept ...string) ([]string, error) {
	tables, err := st.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'people'`)
	if err != nil {
		return nil, err
	}
	var names []string
	for tables.Next() {
		var t string
		tables.Scan(&t)
		names = append(names, t)
	}
	tables.Close()
	type column struct{ name, kind string }
	var found []string
	for _, table := range names {
		cols, err := st.db.Query(`SELECT name, type FROM pragma_table_info(?)`, table)
		if err != nil {
			return nil, err
		}
		var columns []column
		for cols.Next() {
			var c column
			cols.Scan(&c.name, &c.kind)
			columns = append(columns, c)
		}
		cols.Close()
		for _, c := range columns {
			beside := besideID(c.name, func(name string) bool {
				return slices.ContainsFunc(columns, func(t column) bool { return t.name == name && textKind(t.kind) })
			})
			if beside == "" || !strings.EqualFold(c.kind, "INTEGER") {
				continue
			}
			names, err := scanNames(st.db.Query(`SELECT DISTINCT "`+beside+`" FROM "`+table+`" WHERE "`+c.name+`" = ?`, id))
			if err != nil {
				return nil, err
			}
			found = append(found, names...)
		}
	}
	for _, q := range []string{
		`SELECT DISTINCT json_extract(data, '$.maintainer') FROM jobs WHERE json_extract(data, '$.account_id') = ?`,
		`SELECT DISTINCT json_extract(data, '$.asked_by') FROM builds WHERE json_extract(data, '$.asked_by_id') = ?`,
	} {
		names, err := scanNames(st.db.Query(q, id))
		if err != nil {
			return nil, err
		}
		found = append(found, names...)
	}
	var out []string
	for _, name := range append(found, kept...) {
		name = strings.TrimPrefix(strings.TrimSpace(name), "@")
		if name == "" || strings.EqualFold(name, handle) || slices.Contains(out, name) || strings.IndexFunc(name, func(r rune) bool { return r > 127 || !handleByte(byte(r)) }) >= 0 {
			continue
		}
		var other int
		if err := st.db.QueryRow(`SELECT count(*) FROM people WHERE handle = ? COLLATE NOCASE AND account_id != ?`, name, id).Scan(&other); err != nil {
			return nil, err
		}
		if other == 0 {
			out = append(out, name)
		}
	}
	return out, nil
}

func scanNames(rows *sql.Rows, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name.Valid {
			out = append(out, name.String)
		}
	}
	return out, rows.Err()
}

func forgetInColumn(tx *sql.Tx, table, column string, f forgetting) error {
	pattern := []any{}
	where := []string{}
	for _, h := range f.handles {
		where, pattern = append(where, column+` LIKE '%' || ? || '%'`), append(pattern, h)
	}
	where, pattern = append(where, column+` LIKE '%' || ? || '%'`), append(pattern, f.idText)
	rows, err := tx.Query(`SELECT rowid, `+column+` FROM `+table+` WHERE typeof(`+column+`) = 'text' AND (`+strings.Join(where, " OR ")+`)`, pattern...)
	if err != nil {
		return err
	}
	type change struct {
		rowid int64
		text  string
	}
	var changes []change
	for rows.Next() {
		var c change
		var text string
		if err := rows.Scan(&c.rowid, &text); err != nil {
			rows.Close()
			return err
		}
		if c.text = f.stored(text); c.text != text {
			changes = append(changes, c)
		}
	}
	rows.Close()
	for _, c := range changes {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ? WHERE rowid = ?`, c.text, c.rowid); err != nil {
			return err
		}
	}
	return nil
}

// forgetInFiles takes the person off the files under dir, a line at a
// time: labd's copies of Audit logs (JSON lines). A file that changes is
// replaced whole, keeping its mode.
func forgetInFiles(dir string, f forgetting) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || d.IsDir() {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return forgetInFile(path, func(b []byte) ([]byte, bool) { return forgetInLines(b, f.stored) })
	})
}

// forgetInFrozen takes the person off what the frozen Releases keep: the
// comment lines of each pins file, and every file no pins file names, an
// Installed run's record and the Promotion ledger among them. A file a pins
// file names stays as it was, so its pin still matches: a handle in a
// manifest's approved_by is the Release's own, frozen with it. The frozen
// copy of an Installed run's evidence is the job's own copy byte for byte,
// which a Promotion checks: the job's copy under jobs is swept the same way
// first, and the compared logs and comparisons in both stay as they were.
func forgetInFrozen(dir, jobs string, f forgetting) error {
	releases, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range releases {
		if !r.IsDir() {
			continue
		}
		root := filepath.Join(dir, r.Name())
		pinned, err := frozenPinned(root)
		if err != nil {
			return err
		}
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !d.Type().IsRegular() {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			switch {
			case strings.HasPrefix(rel, "PINS."):
				return forgetInFile(path, func(b []byte) ([]byte, bool) {
					return forgetInLines(b, func(line string) string {
						if !strings.HasPrefix(line, "#") {
							return line
						}
						return f.words(line)
					})
				})
			case pinned[rel]:
				return nil
			}
			if parts := strings.Split(rel, "/"); len(parts) == 4 && parts[0] == "installed" {
				if slices.Contains(comparedEvidence, parts[3]) {
					return nil
				}
				own := filepath.Join(jobs, parts[2], "installed", parts[3])
				if err := forgetInFile(own, f.document); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return forgetInFile(path, f.document)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// frozenPinned is every file a pins file of the Release frozen in root names,
// by its path in root.
func frozenPinned(root string) (map[string]bool, error) {
	names, err := filepath.Glob(filepath.Join(root, "PINS.*"))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, name := range names {
		pins, err := readPins(name)
		if err != nil {
			return nil, err
		}
		for rel := range pins {
			out[filepath.ToSlash(filepath.Clean(rel))] = true
		}
	}
	return out, nil
}

// document is a file's bytes with the person taken out: a whole JSON
// document by its keys, indented as it was, anything else a line at a time.
func (f forgetting) document(b []byte) ([]byte, bool) {
	t := bytes.TrimSpace(b)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') || !json.Valid(t) {
		return forgetInLines(b, f.stored)
	}
	out, changed := f.json(string(t))
	if !changed {
		return b, false
	}
	if lines := bytes.SplitN(t, []byte("\n"), 3); len(lines) > 1 {
		indent := lines[1][:len(lines[1])-len(bytes.TrimLeft(lines[1], " \t"))]
		var ib bytes.Buffer
		if json.Indent(&ib, []byte(out), "", string(indent)) == nil {
			out = ib.String()
		}
	}
	if bytes.HasSuffix(b, []byte("\n")) {
		out += "\n"
	}
	return []byte(out), true
}

// forgetInFile passes the file through change, and replaces it when it
// changed, keeping its mode and its time: a Release first frozen before
// labd kept Releases is dated by its pins file.
func forgetInFile(path string, change func([]byte) ([]byte, bool)) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, changed := change(b)
	if !changed {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := replaceFile(path, out, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(path, info.ModTime(), info.ModTime())
}

// forgetInLines is b with each line passed through change, and whether any
// line changed. A line is as long as it is: the file is in memory already,
// and a buffer's limit would cut it short there.
func forgetInLines(b []byte, change func(string) string) ([]byte, bool) {
	lines := strings.Split(string(b), "\n")
	changed := false
	for i, line := range lines {
		if c := change(line); c != line {
			lines[i], changed = c, true
		}
	}
	return []byte(strings.Join(lines, "\n")), changed
}

// replaceFile puts b in path's place with this mode, all at once.
func replaceFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".forget"
	os.Remove(tmp)
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// forget takes the person off the caller log: the name they were logged
// under, and their account id where an address names it (/people/<id>/...).
// Lines written meanwhile wait for each file.
func (l *callerLog) forget(f forgetting) error {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		if err := l.forgetIn(filepath.Join(l.dir, e.Name()), e.Name(), f); err != nil {
			return err
		}
	}
	return nil
}

func (l *callerLog) forgetIn(path, name string, f forgetting) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	out, changed := forgetInLines(b, func(line string) string {
		var c callerLine
		if json.Unmarshal([]byte(line), &c) != nil {
			return line
		}
		who, path := f.words(c.Who), forgetInPath(f.words(c.Path), f.idText)
		if who == c.Who && path == c.Path {
			return line
		}
		c.Who, c.Path = who, path
		j, err := json.Marshal(c)
		if err != nil {
			return line
		}
		return string(j)
	})
	if !changed {
		return nil
	}
	if name == l.day+".log" {
		l.closeLocked()
	}
	return replaceFile(path, out, 0o600)
}

// forgetInPath is an address with every part that is the account id, and
// every mention of the person's handle, made "-".
func forgetInPath(p, id string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		if part == id {
			parts[i] = "-"
		}
	}
	return strings.Join(parts, "/")
}

// notePath has the caller log keep this address for r instead of its own:
// a request that forgets someone names them in its address.
func notePath(r *http.Request, path string) {
	if c, ok := r.Context().Value(callKey{}).(*call); ok {
		c.path = path
	}
}
