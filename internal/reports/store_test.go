package reports

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reports.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func (s *Store) list(t *testing.T, q string, args ...any) []string {
	t.Helper()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func (s *Store) tables(t *testing.T) []string {
	return s.list(t, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
}

const insertReport = `INSERT INTO reports (public_id, deletion_token_digest, schema_version, body, created_at, updated_at, hidden_at) VALUES (?, 'digest', 1, ?, ?, ?, ?)`

func (s *Store) addReport(t *testing.T, publicID, body string, at time.Time) int64 {
	t.Helper()
	res, err := s.db.Exec(insertReport, publicID, body, at.UnixMicro(), at.UnixMicro(), nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// perfRow is what a Performance table's row holds that its checks take,
// beside its times.
var perfRow = map[string]any{
	"mac_key": strings.Repeat("a", 32), "release_key": strings.Repeat("b", 32), "kind": "lab_owned", "board": "j314s",
	"verdict": "qualified", "side": "macos", "macos_build": "27C101", "booted": "{}",
}

func TestTimesAreIntegerMicroseconds(t *testing.T) {
	s, _ := openStore(t)
	at := time.Date(2026, 9, 26, 3, 4, 5, 123456789, time.UTC)
	id := s.addReport(t, "first", `{}`, at)
	var kind string
	var created int64
	if err := s.db.QueryRow(`SELECT typeof(created_at), created_at FROM reports WHERE id = ?`, id).Scan(&kind, &created); err != nil {
		t.Fatal(err)
	}
	if kind != "integer" || created != 1790391845123456 || !time.UnixMicro(created).Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("created_at is %s %d, want the integer 1790391845123456", kind, created)
	}

	// Every column that holds a time, in every table, takes microseconds and
	// nothing else.
	var checked int
	for _, table := range s.tables(t) {
		var cols, times []string
		for _, c := range s.list(t, `SELECT name || ' ' || type FROM pragma_table_info(?) ORDER BY cid`, table) {
			name, kind, _ := strings.Cut(c, " ")
			cols = append(cols, name)
			if strings.HasSuffix(name, "_at") {
				times = append(times, name)
				if kind != "INTEGER" {
					t.Errorf("%s.%s is %s, want INTEGER", table, name, kind)
				}
			}
		}
		if len(times) == 0 {
			t.Errorf("%s keeps no time", table)
		}
		for _, col := range times {
			for what, v := range map[string]any{
				"text":         "2026-09-26 03:04:05.123456",
				"seconds":      at.Unix(),
				"milliseconds": at.UnixMilli(),
				"nanoseconds":  at.UnixNano(),
				"a fraction":   float64(at.UnixMicro()) + 0.5,
			} {
				vals := make([]any, len(cols))
				for i, c := range cols {
					switch {
					case c == col:
						vals[i] = v
					case strings.HasSuffix(c, "_at"):
						vals[i] = at.UnixMicro()
					case c == "id":
						vals[i] = nil
					case c == "body":
						vals[i] = `{}`
					case c == "public_id" || c == "machine_id":
						vals[i] = "x"
					case c == "release_key" && table == "perf_values", c == "adds":
						vals[i] = ""
					case c == "reason":
						vals[i] = nil
					case perfRow[c] != nil:
						vals[i] = perfRow[c]
					default:
						vals[i] = 1
					}
				}
				q := `INSERT INTO ` + table + ` (` + strings.Join(cols, ",") + `) VALUES (?` + strings.Repeat(",?", len(cols)-1) + `)`
				if _, err := s.db.Exec(q, vals...); err == nil {
					t.Errorf("%s.%s took %s (%v)", table, col, what, v)
				}
				vals[indexOf(cols, col)] = at.UnixMicro()
				if _, err := s.db.Exec(q, vals...); err != nil {
					t.Fatalf("%s.%s refused microseconds: %v", table, col, err)
				}
				if _, err := s.db.Exec(`DELETE FROM `+table+` WHERE `+col+` = ?`, at.UnixMicro()); err != nil {
					t.Fatal(err)
				}
				checked++
			}
		}
	}
	if checked < 25 {
		t.Errorf("only %d refusals checked", checked)
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

// A deleted row's id is never given again: upload order is settled by id.
func TestIDsAreAutoincrement(t *testing.T) {
	s, _ := openStore(t)
	for _, sql := range s.list(t, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`) {
		if !strings.Contains(sql, "id INTEGER PRIMARY KEY AUTOINCREMENT") {
			t.Errorf("no AUTOINCREMENT id in: %s", sql)
		}
	}
	at := time.Now()
	for _, name := range []string{"a", "b", "c"} {
		s.addReport(t, name, `{}`, at)
	}
	if _, err := s.db.Exec(`DELETE FROM reports WHERE public_id IN ('b', 'c')`); err != nil {
		t.Fatal(err)
	}
	if id := s.addReport(t, "d", `{}`, at); id != 4 {
		t.Errorf("after ids 2 and 3 were deleted the next report got id %d, want 4", id)
	}
	// An imported report keeps its id, and the count goes on from there.
	if _, err := s.db.Exec(`INSERT INTO reports (id, public_id, deletion_token_digest, schema_version, body, created_at, updated_at) VALUES (132, 'old', 'digest', 1, '{}', ?, ?)`, at.UnixMicro(), at.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	if id := s.addReport(t, "e", `{}`, at); id != 133 {
		t.Errorf("after an imported id 132 the next report got id %d, want 133", id)
	}

	sign := func(machine string) int64 {
		t.Helper()
		var id int64
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		err = tx.QueryRow(`INSERT INTO mac_sign_ins (machine_id, account_id, handle, created_at, updated_at) VALUES (?, 42, 'someone', ?, ?)
ON CONFLICT (machine_id) DO UPDATE SET account_id = excluded.account_id, handle = excluded.handle, updated_at = excluded.updated_at RETURNING id`,
			machine, at.UnixMicro(), time.Now().UnixMicro()).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	if a, again := sign("key:one"), sign("key:one"); a != 1 || again != 1 {
		t.Errorf("signing in again on one Mac: ids %d and %d, want the same row", a, again)
	}
	if _, err := s.db.Exec(`DELETE FROM mac_sign_ins`); err != nil {
		t.Fatal(err)
	}
	if id := sign("key:two"); id <= 1 {
		t.Errorf("after sign-in 1 was deleted the next got id %d again", id)
	}
}

func TestAReportHasOnePublicIDAndAJSONBody(t *testing.T) {
	s, _ := openStore(t)
	at := time.Now()
	s.addReport(t, "same", `{}`, at)
	if _, err := s.db.Exec(insertReport, "same", `{}`, at.UnixMicro(), at.UnixMicro(), nil); err == nil {
		t.Error("two reports took one public id")
	}
	if _, err := s.db.Exec(insertReport, "other", `{"machine":`, at.UnixMicro(), at.UnixMicro(), nil); err == nil {
		t.Error("a body that is not JSON was stored")
	}
}

// The old site's four JSON queries, word for word (site/app/models/
// candidate_set.rb and regressions.rb).
func TestTheOldSitesJSONQueriesRunUnchanged(t *testing.T) {
	s, _ := openStore(t)
	at := time.Now()
	for _, name := range []string{"m1-pro-mx-mac", "m2-max-converged", "m2-max-image2"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if name == "m2-max-image2" {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			body["system"].(map[string]any)["candidate_set"] = "2026-11-01"
			raw, _ = json.Marshal(body)
		}
		s.addReport(t, name, string(raw), at)
	}
	visible := func(where string, args ...any) []string {
		got := s.list(t, `SELECT public_id FROM reports WHERE hidden_at IS NULL AND `+where+` ORDER BY public_id`, args...)
		sort.Strings(got)
		return got
	}
	for _, c := range []struct {
		got  []string
		want string
	}{
		{visible(`(body -> 'system' ->> 'candidate_set') IS NOT NULL`), "m2-max-image2"},
		{visible(`body -> 'system' ->> 'candidate_set' = ?`, "2026-11-01"), "m2-max-image2"},
		{visible(`body -> 'system' ->> 'candidate_set' = ?`, "2026-12-01"), ""},
		{visible(`body -> 'machine' ->> 'board' = ? AND body -> 'system' ->> 'stack' = ?`, "j416c", "converged"), "m2-max-converged m2-max-image2"},
		{visible(`body -> 'machine' ->> 'board' = ? AND body -> 'system' ->> 'stack' = ?`, "j314s", "mx-mac"), "m1-pro-mx-mac"},
		{s.list(t, `SELECT DISTINCT body -> 'system' ->> 'candidate_set' FROM reports WHERE hidden_at IS NULL AND (body -> 'system' ->> 'candidate_set') IS NOT NULL`), "2026-11-01"},
	} {
		if got := strings.Join(c.got, " "); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestASecondStartChangesNothing(t *testing.T) {
	s, path := openStore(t)
	files, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	schema := func(s *Store) string {
		v, err := s.db.Version()
		if err != nil || v != len(files) {
			t.Fatalf("version %d (%v) with %d migrations", v, err, len(files))
		}
		return strings.Join(s.list(t, `SELECT type || ' ' || name || ' ' || coalesce(sql, '') FROM sqlite_master ORDER BY name`), "\n")
	}
	before := schema(s)
	s.addReport(t, "kept", `{}`, time.Now())
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("a second start: %v", err)
	}
	defer again.Close()
	if after := schema(again); after != before {
		t.Errorf("a second start changed the schema:\n%s\nwas:\n%s", after, before)
	}
	if got := again.list(t, `SELECT public_id FROM reports`); len(got) != 1 || got[0] != "kept" {
		t.Errorf("the report: %v", got)
	}
}

// reports.db holds reports and Mac sign-ins, each with account id and
// handle, the digests of the reports it has stored, and nothing else. A
// digest row is a fingerprint of a file: it names no Mac and no person.
func TestWhatReportsDBHolds(t *testing.T) {
	s, _ := openStore(t)
	if got := strings.Join(s.tables(t), " "); got != "mac_sign_ins perf_macs perf_releases perf_values report_digests reports" {
		t.Errorf("tables: %s", got)
	}
	// Performance names a Mac by an opaque key and a Release by its kernel
	// and verdict state: no name, no person, no machine id.
	for table, want := range map[string]string{
		"perf_macs":     "id mac_key kind board cpu_cores gpu_cores created_at",
		"perf_releases": "id release_key kernel verdict position created_at",
		"perf_values":   "id mac_key set_name side release_key macos_version macos_build metric value spread unit api capacity reason booted measured_at adds",
	} {
		if got := strings.Join(s.list(t, `SELECT name FROM pragma_table_info(?)`, table), " "); got != want {
			t.Errorf("%s holds: %s", table, got)
		}
	}
	for _, table := range []string{"mac_sign_ins", "reports"} {
		cols := " " + strings.Join(s.list(t, `SELECT name FROM pragma_table_info(?)`, table), " ") + " "
		for _, want := range []string{" account_id ", " handle ", " machine_id "} {
			if !strings.Contains(cols, want) {
				t.Errorf("%s has no%scolumn:%s", table, want, cols)
			}
		}
	}
	if got := strings.Join(s.list(t, `SELECT name FROM pragma_table_info('report_digests')`), " "); got != "id digest report_id created_at" {
		t.Errorf("report_digests holds: %s", got)
	}
}
