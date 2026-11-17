package server

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/installed"
	"github.com/omacom/omarchy-maclab/internal/reports"
)

// Forgetting a person (spec section 14, "Forgetting a person"): each row of
// its table, the request on People, who carries it out, and a search of
// everything labd keeps for what is left of the person afterwards.

const (
	wenID     = 7700123
	wenHandle = "wendelin"
)

// forgotten is a lab where wendelin, a Maintainer, did a bit of everything
// and was then Forgotten at her own asking, by the owner, an Admin.
type forgotten struct {
	*signInLab
	owner, ravi *browser
	token       string
	report      string // a report her Mac sent while signed in as her
	published   string // a report the lab published at her asking
	digests     int
	lines       int // People record lines before the forgetting
	leaseLines  int
	sentLine    int64 // a line about her that Signal already had
}

func forgottenLab(t *testing.T) *forgotten {
	t.Helper()
	l := macLab(t, nil)
	l.gh.AddUser(wenID, wenHandle)
	f := &forgotten{signInLab: l, owner: l.signedIn(ownerID), ravi: l.signedIn(maintainerID)}
	l.signedIn(linusID)

	f.ravi.did("/people/mark", "handle", wenHandle, "as", "m-team")
	f.owner.did(personPath(wenID, "maintainer"))
	wen := l.signedIn(wenID)
	f.token = wen.makeToken("laptop", "30")
	wen.did(personPath(linusID, "block"), "reason", "spam, says wendelin", "sure", "yes")

	// A Lease on a Lab-owned Mac, and a job and a build she asked for.
	l.lrMac("m1", "j293", nil)
	wen.mustLease("m1", "wendelin-laptop", 0)
	l.clock.Advance(time.Minute)
	l.pjJob("j-wen", "m1", wenID, wenHandle, l.clock.Now(), func(j *api.Job) {
		j.Result = api.JobResult{OMT: &api.OMTResult{ReportDeleted: &api.ReportDeleted{By: "@" + wenHandle, AccountID: wenID, Time: l.clock.Now()}}}
	})
	if err := l.s.store.saveBuild(&api.Build{ID: "b-wen", Key: "k", State: api.BuildDone, Created: l.clock.Now(), AskedBy: wenHandle, AskedByID: wenID}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.s.store.addExec("d-m1", api.ExecEvent{Time: l.clock.Now(), Mac: "m1", AccountID: wenID, Handle: wenHandle, Command: "uname -a", State: "ran"}); err != nil {
		t.Fatal(err)
	}

	// A Mac she lends, which accepted the terms, and one Join command more.
	v := l.addMac("wen-air", wenID)
	v.mu.Lock()
	v.d.TermsSHA256, v.d.TermsAccepted, v.d.VolunteerName = strings.Repeat("a", 64), l.clock.Now(), wenHandle
	v.saveLocked()
	v.mu.Unlock()
	if _, err := l.s.store.addJoinCommand(hashSecret("join-wen"), api.EnrollTokenRequest{Name: "wen-pro", Volunteer: true, VolunteerID: wenID}, wenID, l.clock.Now(), l.clock.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	// Another Volunteer's Mac whose Audit log names her.
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: wenHandle, Action: "screenshot", End: 40}})

	// A Mac signed in as her sends a report; the lab publishes another at her asking.
	l.mustMacSignIn("air", wenID)
	f.report = l.storedReport(l.macUpload("air", "m2-max-image2")).PublicID
	golden, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "m1-pro-converged.json"))
	if err != nil {
		t.Fatal(err)
	}
	stored, refusal, err := l.s.uploads.ReceiveFromLab([]byte(macSigned(t, "pro", reports.NamespaceReport, string(golden))), "https://"+labHost+"/install", reports.Lab{Job: "j-wen", Handle: wenHandle, AccountID: wenID})
	if err != nil || refusal != nil {
		t.Fatalf("the lab's own report: %v %+v", err, refusal)
	}
	f.published = stored.PublicID

	// A Release she froze, judged and promoted from, with its files.
	if err := l.s.store.addRelease(api.Release{Name: "r1", FrozenBy: wenHandle, FrozenByID: wenID, FrozenAt: l.clock.Now(), ManifestSHA256: strings.Repeat("b", 64), Kernel: "7.1"}); err != nil {
		t.Fatal(err)
	}
	if err := l.s.store.addVerdict("r1", api.ReleaseVerdict{Time: l.clock.Now(), State: "qualified", Note: "fine on m1", AccountID: wenID, Handle: wenHandle}); err != nil {
		t.Fatal(err)
	}
	wenPerson := Person{AccountID: wenID, Handle: wenHandle}
	if err := l.s.store.addEntryFrozen("r1", "m1", strings.Repeat("c", 64), l.clock.Now(), wenPerson); err != nil {
		t.Fatal(err)
	}
	if err := l.s.store.addPromotion("r1", "d-m1", api.ReleasePromotion{Time: l.clock.Now(), Mac: "m1", Job: "j-wen", Kernel: "7.1", AccountID: wenID, Handle: wenHandle}); err != nil {
		t.Fatal(err)
	}
	dir := l.s.frozenDir("r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.s.freezeEntry(dir, "m1", strings.Repeat("b", 64), &installed.InstallerEntry{}, wenPerson, l.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "promotions.log"), []byte("2026-11-14T12:01:00Z\tm1\tj-wen\t7.1\taccepted-by="+wenHandle+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A line about her that went to Signal before she asked.
	if err := l.s.store.db.QueryRow(`SELECT min(id) FROM lines WHERE text LIKE '%' || ? || '%'`, wenHandle).Scan(&f.sentLine); err != nil {
		t.Fatal(err)
	}
	if _, err := l.s.store.db.Exec(`UPDATE lines SET state = 'sent', settled = 1 WHERE id = ?`, f.sentLine); err != nil {
		t.Fatal(err)
	}

	// She asks on her own page; the owner carries it out from People.
	l.clock.Advance(time.Hour)
	wen.did("/me/forget", "sure", "yes")
	f.digests = l.countIn("reports.db", `SELECT count(*) FROM report_digests`)
	f.lines = l.count(`SELECT count(*) FROM people_record`)
	f.leaseLines = l.count(`SELECT count(*) FROM lease_record`)
	l.clock.Advance(time.Hour)
	f.owner.did(personPath(wenID, "forget"), "sure", "yes")
	return f
}

// countIn counts in one of the lab's two databases, read as a file.
func (l *signInLab) countIn(db, query string, args ...any) (n int) {
	l.t.Helper()
	conn := l.openDB(db)
	if err := conn.QueryRow(query, args...).Scan(&n); err != nil {
		l.t.Fatal(err)
	}
	return n
}

func (l *signInLab) openDB(name string) *sql.DB {
	l.t.Helper()
	conn, err := sql.Open("sqlite", filepath.Join(l.dir, name)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		l.t.Fatal(err)
	}
	l.t.Cleanup(func() { conn.Close() })
	return conn
}

// mentions finds the handle as a name in text, whatever its case, and the
// account id as a number.
func mentions(handle string, id int64) func(string) bool {
	byName := regexp.MustCompile(`(?i)(^|[^a-z0-9-])` + regexp.QuoteMeta(handle) + `($|[^a-z0-9-])`)
	byID := regexp.MustCompile(`(^|[^0-9])` + strconv.FormatInt(id, 10) + `($|[^0-9])`)
	return func(text string) bool { return byName.MatchString(text) || byID.MatchString(text) }
}

// traces searches both databases, every row of every table, and the files
// labd keeps beside them (the caller log, its copies of Audit logs, the
// frozen Releases) for the handle and the account id. It says where each
// was found: "labd.db exec_record.handle", "callers/2026-11-14.log".
func (l *signInLab) traces(handle string, id int64) []string {
	l.t.Helper()
	found := mentions(handle, id)
	seen := map[string]bool{}
	for _, name := range []string{"labd.db", "reports.db"} {
		conn := l.openDB(name)
		rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			l.t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var t string
			rows.Scan(&t)
			tables = append(tables, t)
		}
		rows.Close()
		for _, table := range tables {
			rows, err := conn.Query(`SELECT * FROM "` + table + `"`)
			if err != nil {
				l.t.Fatal(err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				values := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range values {
					ptrs[i] = &values[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					l.t.Fatal(err)
				}
				for i, v := range values {
					hit := false
					switch v := v.(type) {
					case int64:
						hit = v == id
					case string:
						hit = found(v)
					case []byte:
						hit = found(string(v))
					}
					if hit {
						seen[name+" "+table+"."+cols[i]] = true
					}
				}
			}
			rows.Close()
		}
	}
	for _, sub := range []string{"callers", "audit", "frozen"} {
		filepath.WalkDir(filepath.Join(l.dir, sub), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			file, err := os.Open(path)
			if err != nil {
				l.t.Fatal(err)
			}
			defer file.Close()
			sc := bufio.NewScanner(file)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				if found(sc.Text()) {
					rel, _ := filepath.Rel(l.dir, path)
					seen[rel] = true
				}
			}
			return nil
		})
	}
	var out []string
	for where := range seen {
		out = append(out, where)
	}
	slices.Sort(out)
	return out
}

// After a forgetting, what is left of the person in everything labd keeps
// is what the spec keeps, and nothing more.
func TestAfterAForgettingOnlyWhatTheSpecKeepsNamesThePerson(t *testing.T) {
	f := forgottenLab(t)
	kept := map[string]string{
		// "Exec record lines they ran: kept with the handle until each is 12
		// months old, then deleted."
		"labd.db exec_record.handle":     "Exec record lines are kept with the handle for 12 months",
		"labd.db exec_record.account_id": "Exec record lines are kept with the handle for 12 months",
	}
	// "Lines already posted to the Lab group or 'Agents': kept on the
	// members' phones." The lab's own copy of a sent line loses the handle
	// with everything else; what Signal delivered is beyond its reach.
	var unexpected []string
	for _, where := range f.traces(wenHandle, wenID) {
		if _, ok := kept[where]; !ok {
			unexpected = append(unexpected, where)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("the forgotten person is still named in:\n%s", strings.Join(unexpected, "\n"))
	}
	if f.count(`SELECT count(*) FROM exec_record WHERE handle = ? AND account_id = ?`, wenHandle, wenID) != 1 {
		t.Error("the Exec record line she ran was not kept with her handle")
	}
}

// One check for each row of the forgetting table.
func TestForgettingDoesWhatEachRowOfTheTableSays(t *testing.T) {
	f := forgottenLab(t)
	l := f.signInLab

	t.Run("the person's record is erased", func(t *testing.T) {
		if l.known(wenID) {
			t.Error("the person's row is kept")
		}
		for _, q := range []string{
			`SELECT count(*) FROM requests_to_lend WHERE account_id = ?`,
			`SELECT count(*) FROM join_commands WHERE volunteer_id = ? OR made_by = ?`,
		} {
			if n := l.count(q, wenID, wenID); n != 0 {
				t.Errorf("%s: %d", q, n)
			}
		}
		if l.count(`SELECT count(*) FROM tokens WHERE name = 'wen-pro'`) != 0 {
			t.Error("her unused Join command still enrolls a Mac")
		}
	})
	t.Run("browser sessions and Lab tokens are erased and stop at once", func(t *testing.T) {
		if n := l.count(`SELECT count(*) FROM browser_sessions WHERE account_id = ?`, wenID) + l.count(`SELECT count(*) FROM lab_tokens WHERE account_id = ?`, wenID); n != 0 {
			t.Errorf("%d sessions and tokens kept", n)
		}
		if w := l.asToken(f.token, "GET", "/api/devices", "203.0.113.4"); w.Code != http.StatusUnauthorized {
			t.Errorf("her Lab token after the forgetting: %d %s", w.Code, w.Body)
		}
	})
	t.Run("Leases end as when a Maintainer ends them", func(t *testing.T) {
		if lease := l.leaseOn("m1"); lease != nil {
			t.Errorf("m1 is still leased: %+v", lease)
		}
		lines := l.leaseRecordOf("m1")
		if len(lines) == 0 || lines[len(lines)-1] != "someone's Lease on m1 was ended by @owner" {
			t.Errorf("the Lease record of m1: %q", lines)
		}
	})
	t.Run("Mac sign-ins are erased", func(t *testing.T) {
		if n := l.countIn("reports.db", `SELECT count(*) FROM mac_sign_ins WHERE account_id = ?`, wenID); n != 0 {
			t.Errorf("%d Mac sign-ins kept", n)
		}
	})
	t.Run("the Macs they lend Leave, Retired without their Volunteer", func(t *testing.T) {
		if l.s.dev("wen-air") != nil {
			t.Fatal("wen-air is still enrolled")
		}
		retired, err := l.s.store.retiredMacs(10)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(retired, func(m RetiredMac) bool { return m.ID == "d-wen-air" })
		if i < 0 {
			t.Fatalf("wen-air was not Retired: %+v", retired)
		}
		m := retired[i]
		if m.Name != "wen-air" || m.Device.Name != "wen-air" {
			t.Errorf("a Retired Mac's name is left as typed: %q, %q", m.Name, m.Device.Name)
		}
		if m.VolunteerID != 0 || m.Device.VolunteerID != 0 || m.Device.VolunteerName != "" || m.Device.Contact != "" || m.Device.TermsSHA256 != "" || !m.Device.TermsAccepted.IsZero() {
			t.Errorf("the Retired record keeps its Volunteer: %+v", m)
		}
		if !slices.ContainsFunc(l.labLines(), func(s string) bool { return strings.HasPrefix(s, "wen-air ") }) {
			t.Errorf("the Lab group was not told wen-air left: %q", l.labLines())
		}
	})
	t.Run("the handle and account id on their reports are erased; the reports stay", func(t *testing.T) {
		r, err := l.s.reports.Report(f.report)
		if err != nil {
			t.Fatalf("the report went: %v", err)
		}
		if r.Handle != "" || r.AccountID == wenID || !r.NameNotKept() {
			t.Errorf("the report still names her: %q %d", r.Handle, r.AccountID)
		}
		if !r.Verified || r.Hidden || r.MachineID == "" {
			t.Errorf("the report lost Verified, Published or its machine id: %+v", r)
		}
		if page := f.ravi.page("/reports/" + f.report); !strings.Contains(page, "a person whose name is not kept") {
			t.Errorf("the report's page for Maintainers does not say whose name is not kept:\n%s", page)
		}
	})
	t.Run("report digests are kept", func(t *testing.T) {
		if n := l.countIn("reports.db", `SELECT count(*) FROM report_digests`); n != f.digests {
			t.Errorf("%d digests, want %d", n, f.digests)
		}
	})
	t.Run("Exec record lines are kept with the handle", func(t *testing.T) {
		record, err := l.s.store.execRecord("d-m1", 0, 10)
		if err != nil || len(record) != 1 || record[0].Handle != wenHandle {
			t.Errorf("the Exec record: %+v %v", record, err)
		}
	})
	t.Run("the Audit log on a Volunteer's own Mac is kept", func(t *testing.T) {
		// It is on the Mac, which labd never writes to: nothing is queued for it.
		if jobs, _ := l.s.store.jobs("ravi-mini", 10, false); len(jobs) != 0 {
			t.Errorf("the forgetting queued something for a Volunteer's Mac: %+v", jobs)
		}
	})
	t.Run("labd's copy of an Audit log, Lease record lines and who asked keep, without the handle", func(t *testing.T) {
		audit, err := l.s.readAuditCopy("ravi-mini")
		if err != nil || len(audit) != 1 || audit[0].Action != "screenshot" || audit[0].Maintainer != "" || audit[0].AccountID != 0 {
			t.Errorf("labd's copy of ravi-mini's Audit log: %+v %v", audit, err)
		}
		if n := l.count(`SELECT count(*) FROM lease_record`); n < f.leaseLines {
			t.Errorf("%d Lease record lines, want at least %d", n, f.leaseLines)
		}
		j, err := l.s.store.job("j-wen")
		if err != nil || j.Maintainer != "" || j.AccountID != 0 || j.Result.OMT.ReportDeleted.By == "@"+wenHandle {
			t.Errorf("the job she asked for: %+v %v", j, err)
		}
		builds, _ := l.s.store.builds(`id = 'b-wen'`)
		if len(builds) != 1 || builds[0].AskedBy != "" || builds[0].AskedByID != 0 {
			t.Errorf("the build she asked for: %+v", builds)
		}
	})
	t.Run("People record lines are kept, the handle removed", func(t *testing.T) {
		if n := l.count(`SELECT count(*) FROM people_record`); n <= f.lines {
			t.Errorf("%d lines in the People record, want more than %d", n, f.lines)
		}
		got := strings.Join(l.recorded(), "\n")
		for _, want := range []string{"@ravi marked as M team (not signed in yet) someone", "@owner made a Maintainer of someone", "someone blocked @linus", "someone asked to be forgotten", "@owner carried out a forgetting"} {
			if !strings.Contains(got, want) {
				t.Errorf("the People record lacks %q:\n%s", want, got)
			}
		}
		// What she typed beside an action of hers is kept, her name off it.
		if !strings.Contains(got, "someone blocked @linus: spam, says someone") {
			t.Errorf("a line by her lost its note:\n%s", got)
		}
	})
	t.Run("verdict history, freezing and Promotion keep the time and note, the handle removed", func(t *testing.T) {
		verdicts, err := l.s.store.verdicts("r1")
		if err != nil || len(verdicts) != 1 || verdicts[0].Note != "fine on m1" || verdicts[0].Handle != "" || verdicts[0].AccountID != 0 || verdicts[0].Time.IsZero() {
			t.Errorf("the verdict history: %+v %v", verdicts, err)
		}
		releases, _ := l.s.store.releases("r1")
		if len(releases) != 1 || releases[0].FrozenBy != "" || releases[0].FrozenByID != 0 {
			t.Errorf("who froze r1: %+v", releases)
		}
		promotions, _ := l.s.store.promotions("r1")
		if len(promotions) != 1 || promotions[0].Handle != "" || promotions[0].Kernel != "7.1" {
			t.Errorf("who promoted: %+v", promotions)
		}
		pins, _ := os.ReadFile(filepath.Join(l.s.frozenDir("r1"), "PINS.installer-entry.m1"))
		ledger, _ := os.ReadFile(filepath.Join(l.s.frozenDir("r1"), "promotions.log"))
		if !strings.Contains(string(pins), "read for someone,") || !strings.Contains(string(ledger), "accepted-by=someone") {
			t.Errorf("the frozen files:\n%s\n%s", pins, ledger)
		}
		if got, err := readPins(filepath.Join(l.s.frozenDir("r1"), "PINS.installer-entry.m1")); err != nil || len(got) != 1 {
			t.Errorf("the pins no longer read: %v %v", got, err)
		}
	})
	t.Run("a lab-published report's who asked loses the handle", func(t *testing.T) {
		r, err := l.s.reports.Report(f.published)
		if err != nil || r.AskedHandle != "" || r.AskedAccountID == wenID || !r.AskedNameNotKept() || r.LabJob != "j-wen" {
			t.Errorf("the lab-published report: %+v %v", r, err)
		}
	})
	t.Run("lines already posted stay sent; the lab's copy loses the handle", func(t *testing.T) {
		var state, text string
		l.s.store.db.QueryRow(`SELECT state, text FROM lines WHERE id = ?`, f.sentLine).Scan(&state, &text)
		if state != "sent" || strings.Contains(text, wenHandle) || !strings.Contains(text, "someone") {
			t.Errorf("the line Signal had: %s %q", state, text)
		}
	})
	t.Run("the lines for a forgetting carry no handle", func(t *testing.T) {
		agents := l.agentsLines()
		if last := agents[len(agents)-1]; !regexp.MustCompile(`^owner carried out a forgetting at \d{4}-\d\d-\d\d \d\d:\d\d AEST\.$`).MatchString(last) {
			t.Errorf(`the last line for "Agents": %q`, last)
		}
	})
	t.Run("it cannot be undone", func(t *testing.T) {
		if w := f.owner.post(personPath(wenID, "forget"), "sure", "yes"); w.Code != http.StatusNotFound {
			t.Errorf("forgetting again: %d", w.Code)
		}
		if page := f.owner.page("/people"); strings.Contains(page, wenHandle) || strings.Contains(page, fmt.Sprint(wenID)) {
			t.Errorf("People still shows her")
		}
	})
}

// The same GitHub account signing in later is a new person with no history.
func TestSigningInAfterAForgettingIsANewPersonWithNoHistory(t *testing.T) {
	f := forgottenLab(t)
	back := f.signedIn(wenID)
	if me := back.whoami(); me.Login != wenHandle || me.Kind != "Signed in, no role" {
		t.Errorf("signed in again: %+v", me)
	}
	if p := f.who(wenID); p.Maintainer || p.MTeam || p.Lend || p.Blocked || p.Volunteer {
		t.Errorf("signed in again, the person is %+v", p)
	}
	if lines, _ := f.s.store.recordLinesOf(wenID, 10); len(lines) != 0 {
		t.Errorf("the new person has a People record: %v", lines)
	}
	if lines, _ := f.s.store.leaseRecordOf(wenID, 10); len(lines) != 0 {
		t.Errorf("the new person has a Lease record: %v", lines)
	}
	if tokens, _ := f.s.store.labTokens(wenID); len(tokens) != 0 {
		t.Errorf("the new person has Lab tokens: %v", tokens)
	}
	if page := back.page("/me"); !strings.Contains(page, "Ask to be forgotten") || strings.Contains(page, "You asked to be forgotten") {
		t.Errorf("the new person's own page:\n%s", page)
	}
}

// The request shows on People with its date; a Maintainer carries it out,
// an Admin for a Maintainer; an Admin, named in the configuration, is not
// forgotten from People.
func TestARequestToBeForgottenAndWhoCarriesItOut(t *testing.T) {
	l := rolesLab(t, nil)
	owner, ravi := l.signedIn(ownerID), l.signedIn(maintainerID)
	edith, grace := l.signedIn(edithID), l.signedIn(graceID)

	w := edith.post("/me/forget")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Ask to be forgotten: a Maintainer erases what the lab keeps of you within 30 days. It cannot be undone") {
		t.Fatalf("the first click asks: %d\n%s", w.Code, w.Body)
	}
	if n := l.count(`SELECT count(*) FROM people WHERE forget_asked IS NOT NULL`); n != 0 {
		t.Fatal("the first click asked")
	}
	edith.did("/me/forget", "sure", "yes")
	if page := edith.page("/me"); !strings.Contains(page, "You asked to be forgotten on 2026-11-14. A Maintainer carries it out by 2026-12-14.") {
		t.Errorf("her own page after asking:\n%s", page)
	}
	edith.refused("You asked already", "/me/forget", "sure", "yes")

	page := ravi.page("/people")
	request := part(t, page, `id="forget-7"`, "</li>")
	for _, want := range []string{"@edith", "asks to be forgotten", "2026-11-14", "by 2026-12-14", ">Forget</button>"} {
		if !strings.Contains(request, want) {
			t.Errorf("the request on People lacks %q:\n%s", want, request)
		}
	}
	if n := l.s.pendingRequests(); n != 1 {
		t.Errorf("People's count in the navigation: %d, want 1", n)
	}

	// A Maintainer: only an Admin forgets one. An Admin: not from People.
	if err := l.s.setMaintainer(graceID, true); err != nil {
		t.Fatal(err)
	}
	ravi.refused("Only an Admin forgets a Maintainer.", personPath(graceID, "forget"), "sure", "yes")
	owner.refused("An Admin is named in the server's configuration", personPath(ownerID, "forget"), "sure", "yes")
	ravi.refused("An Admin is named in the server's configuration", personPath(ownerID, "forget"), "sure", "yes")
	if w := ravi.post(personPath(424242, "forget"), "sure", "yes"); w.Code != http.StatusNotFound {
		t.Errorf("forgetting someone the lab does not know: %d", w.Code)
	}

	// The first click states what goes; the second does it.
	w = ravi.post(personPath(edithID, "forget"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Forget @edith now: everything the lab keeps of them goes, 1 browser signed out. It cannot be undone") {
		t.Errorf("the first click on Forget: %d\n%s", w.Code, w.Body)
	}
	if !l.known(edithID) {
		t.Fatal("the first click forgot")
	}
	ravi.did(personPath(edithID, "forget"), "sure", "yes")
	if l.known(edithID) || edith.whoami().Login != "" {
		t.Error("edith is still known, or still signed in")
	}
	if l.s.pendingRequests() != 0 || strings.Contains(ravi.page("/people"), `id="forget-7"`) {
		t.Error("the request is still waiting")
	}
	owner.did(personPath(graceID, "forget"), "sure", "yes")
	if l.known(graceID) || grace.whoami().Login != "" {
		t.Error("an Admin did not forget a Maintainer")
	}
	want := []string{"@ravi carried out a forgetting", "@owner carried out a forgetting"}
	got := l.recorded()
	if fmt.Sprint(got[len(got)-2:]) != fmt.Sprint(want) {
		t.Errorf("the People record ends:\n%s", strings.Join(got, "\n"))
	}
}

// A Blocked person who is Forgotten stays Blocked: the account id,
// "Blocked", the date and who blocked are kept; the handle and the typed
// reason are erased.
func TestABlockedPersonWhoIsForgottenStaysBlocked(t *testing.T) {
	const eddaID = 7700789
	l := rolesLab(t, nil)
	l.gh.AddUser(eddaID, "edda")
	ravi := l.signedIn(maintainerID)
	l.signedIn(eddaID)
	ravi.did(personPath(eddaID, "block"), "reason", "uploads spam", "sure", "yes")
	var blockedAt int64
	l.s.store.db.QueryRow(`SELECT blocked_at FROM people WHERE account_id = ?`, eddaID).Scan(&blockedAt)
	l.clock.Advance(24 * time.Hour)
	ravi.did(personPath(eddaID, "forget"), "sure", "yes")

	var handle, reason string
	var at, by int64
	if err := l.s.store.db.QueryRow(`SELECT handle, blocked_reason, blocked_at, blocked_by FROM people WHERE account_id = ?`, eddaID).Scan(&handle, &reason, &at, &by); err != nil {
		t.Fatalf("the Blocked state was not kept: %v", err)
	}
	if handle != "" || reason != "" || at != blockedAt || by != maintainerID {
		t.Errorf("the Blocked state: handle %q, reason %q, at %d (want %d), by %d", handle, reason, at, blockedAt, by)
	}
	if traces := l.traces("edda", eddaID); fmt.Sprint(traces) != "[labd.db people.account_id]" {
		// "The Blocked state: kept: the account id, 'Blocked', the date and who blocked."
		t.Errorf("after forgetting a Blocked person, found in: %v; want only the Blocked state's account id", traces)
	}
	if got := strings.Join(l.recorded(), "\n"); strings.Contains(got, "uploads spam") || !strings.Contains(got, "@ravi blocked someone") {
		t.Errorf("the Block's line keeps the typed reason, or lost the Block:\n%s", got)
	}
	again := l.browser().signIn(eddaID, "")
	if again.Code != http.StatusForbidden || strings.TrimSpace(again.Body.String()) != blockedSentence {
		t.Errorf("the account signing in again: %d %q", again.Code, again.Body)
	}
	if l.countIn("labd.db", `SELECT count(*) FROM people WHERE handle != '' AND account_id = ?`, eddaID) != 0 {
		t.Error("a refused sign-in wrote the handle back")
	}
	page := ravi.page("/people?open=7700789")
	row := part(t, page, `id="person-7700789"`, `id="person-7700789-end"`)
	for _, want := range []string{"account 7700789", "forgotten", "Blocked 2026-11-14 12:00 UTC by @ravi"} {
		if !strings.Contains(row, want) {
			t.Errorf("the Blocked row lacks %q:\n%s", want, row)
		}
	}
	ravi.refused("forgotten already", personPath(eddaID, "forget"), "sure", "yes")
}

// writePinned writes a frozen Release's files and a pins file for them,
// named in its comment line by who froze them.
func writePinned(t *testing.T, dir, pinsName, by string, files map[string]string) {
	t.Helper()
	pins := "# " + pinsName + " 2026-11-14T12:00:00Z by maclab, frozen by @" + by + ": do not rewrite\n"
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(files[rel]), 0o644); err != nil {
			t.Fatal(err)
		}
		pins += installed.SHA256([]byte(files[rel])) + "  " + rel + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, pinsName), []byte(pins), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A frozen Release's pinned files stay byte for byte through a forgetting,
// though they name the person: a manifest's approved_by, a key that is the
// handle. Their pins still match, so its Installed runs still load; the
// pins files' notes lose the handle.
func TestForgettingLeavesAFrozenReleasesPinnedFilesAsTheyWere(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	dir := l.s.frozenDir("r9")
	expected := map[string]string{
		"expected/manifest.json": `{"schema":"maclab.installed-manifest/2","release":"r9","approved_by":["edith"],"kernel":{"release":"7.1","payload_manifest":"k.sha256"},"macs":{"m1":{"board":"edith"}}}` + "\n",
		"expected/k.sha256":      strings.Repeat("d", 64) + "  usr/lib/modules/edith\n",
	}
	preinstall := map[string]string{"preinstall/m1/omt-report.json": `{"asked_by":"@edith","edith":true}` + "\n"}
	writePinned(t, dir, "PINS.expected", "edith", expected)
	writePinned(t, dir, "PINS.preinstall", "edith", preinstall)

	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	for rel, want := range maps.All(expected) {
		if got, _ := os.ReadFile(filepath.Join(dir, rel)); string(got) != want {
			t.Errorf("the pinned %s changed:\n%s", rel, got)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "preinstall/m1/omt-report.json")); string(got) != preinstall["preinstall/m1/omt-report.json"] {
		t.Errorf("the pinned preinstall reference changed:\n%s", got)
	}
	if m, _, err := frozenManifest(dir); err != nil || m.Release != "r9" {
		t.Errorf("the Release's manifest no longer loads: %v", err)
	}
	pre, err := readPins(filepath.Join(dir, "PINS.preinstall"))
	if _, _, perr := pinned(dir, "preinstall/m1/omt-report.json", pre); err != nil || perr != nil {
		t.Errorf("the preinstall reference no longer matches its pin: %v %v", err, perr)
	}
	for _, name := range []string{"PINS.expected", "PINS.preinstall"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		if strings.Contains(string(b), "edith") || !strings.Contains(string(b), "frozen by someone") {
			t.Errorf("%s's note still names her, or lost its words:\n%s", name, b)
		}
	}
}

// A job that runs while its person is Forgotten goes on, and what it writes
// afterwards does not bring them back: the copy the run holds loses them at
// its next save, which only the run makes (a scrub from the forgetting's
// goroutine would race the run's reads), and so does a copy read before it
// and saved after.
func TestAJobRunningThroughAForgettingWritesNoHandleBack(t *testing.T) {
	l := macLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	l.lrMac("m1", "j293", nil)
	l.pjJob("j-41", "m1", edithID, "edith", l.clock.Now(), func(j *api.Job) {
		j.State, j.Outcome, j.Summary = api.JobQueued, "", "waits for @edith's kernel"
	})
	running, _ := l.s.store.job("j-41")
	l.s.updateJob(running, func(j *api.Job) { j.State = api.JobTesting })
	older, _ := l.s.store.job("j-41")

	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	l.s.updateJob(running, func(j *api.Job) { j.Events = append(j.Events, api.JobEvent{Time: l.clock.Now(), Msg: "test 1 passed"}) })
	if running.Maintainer != "" || running.AccountID != 0 || running.Spec.Holder != "" {
		t.Errorf("the running job's own copy still names her: %q %d %q", running.Maintainer, running.AccountID, running.Spec.Holder)
	}
	l.s.updateJob(older, func(j *api.Job) { j.Events = append(j.Events, api.JobEvent{Time: l.clock.Now(), Msg: "test 2 passed"}) })
	l.s.updateJob(running, func(j *api.Job) { j.State, j.Outcome = api.JobDone, api.OutcomePass })
	j, err := l.s.store.job("j-41")
	if err != nil || j.State != api.JobDone {
		t.Fatalf("the job: %+v %v", j, err)
	}
	if j.Maintainer != "" || j.AccountID != 0 || j.Spec.Holder != "" || strings.Contains(j.Summary, "edith") {
		t.Errorf("the job's later events wrote her back: %q %d %q %q", j.Maintainer, j.AccountID, j.Spec.Holder, j.Summary)
	}
	var data string
	l.s.store.db.QueryRow(`SELECT data FROM jobs WHERE id = 'j-41'`).Scan(&data)
	if strings.Contains(data, "edith") || strings.Contains(data, `"account_id":7`) {
		t.Errorf("the stored job names her:\n%s", data)
	}
}

// A Block that lands while a forgetting is under way holds: the forgetting
// read the person before it, and still keeps the Blocked state.
func TestABlockDuringAForgettingStaysBlocked(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	before, err := l.s.store.knownPerson(edithID)
	if err != nil || before.Blocked {
		t.Fatalf("edith before: %+v %v", before, err)
	}
	ravi.did(personPath(edithID, "block"), "reason", "spam", "sure", "yes")

	by := Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}
	if err := l.s.forgetHeld(by, before, newForgetting(edithID, before.Handle)); err != nil {
		t.Fatal(err)
	}
	var handle string
	var at sql.NullInt64
	if err := l.s.store.db.QueryRow(`SELECT handle, blocked_at FROM people WHERE account_id = ?`, edithID).Scan(&handle, &at); err != nil || !at.Valid || handle != "" {
		t.Errorf("the Blocked state after the forgetting: %q %v %v", handle, at, err)
	}
	if again := l.browser().signIn(edithID, ""); again.Code != http.StatusForbidden {
		t.Errorf("the account signing in again: %d %q", again.Code, again.Body)
	}
}

// Blocked ends the person's Leases, Mac sign-ins and enrollments, along with
// the rest: their Macs Leave, and the Block says so before it is pressed.
func TestBlockedEndsLeasesMacSignInsAndEnrollments(t *testing.T) {
	l := macLab(t, nil)
	ravi := l.signedIn(maintainerID)
	edith := l.signedIn(edithID)
	l.lrMark(edithID)
	l.lrMac("m1", "j293", nil)
	edith.mustLease("m1", "edith-laptop", 0)
	l.addMac("edith-air", edithID)
	l.mustMacSignIn("air", edithID)

	w := ravi.post(personPath(edithID, "block"))
	if !strings.Contains(w.Body.String(), "1 Lease ended, 1 Mac signed out, 1 Mac leaves the lab") {
		t.Errorf("the Block's sentence:\n%s", refusalIn(w.Body.String()))
	}
	ravi.did(personPath(edithID, "block"), "sure", "yes")
	if lease := l.leaseOn("m1"); lease != nil {
		t.Errorf("the Lease of a Blocked person: %+v", lease)
	}
	if signIns := l.macSignIns(edithID); len(signIns) != 0 {
		t.Errorf("the Mac sign-ins of a Blocked person: %v", signIns)
	}
	if l.s.dev("edith-air") != nil {
		t.Error("the Mac of a Blocked person is still enrolled")
	}
}

// A file whose lines run past any buffer is swept whole: a changed line
// before a long one never leaves the file cut short at it.
func TestForgettingSweepsAFileWithALongLineWhole(t *testing.T) {
	f := newForgetting(edithID, "edith")
	long := `{"maintainer":"edith","action":"` + strings.Repeat("x", 2<<20) + `"}`
	in := `{"maintainer":"edith","action":"screenshot"}` + "\n" + long + "\n" + `{"action":"reboot"}`
	out, changed := forgetInLines([]byte(in), f.stored)
	lines := strings.Split(string(out), "\n")
	if !changed || len(lines) != 3 || len(lines[1]) < 2<<20 || strings.Contains(string(out), "edith") || lines[2] != `{"action":"reboot"}` {
		t.Errorf("swept: changed %v, %d lines, the long one %d bytes, last %q", changed, len(lines), len(lines[1]), lines[len(lines)-1])
	}
}

// The files are swept before the record goes, and a file that cannot be
// swept fails the forgetting: the person is still there to forget again,
// and the second time takes them off the file too.
func TestAFileTheForgettingCannotSweepFailsItAndItCanBeDoneAgain(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40}})
	audit := filepath.Join(l.dir, "audit")
	if err := os.Chmod(audit, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(audit, 0o755) })

	if w := ravi.post(personPath(edithID, "forget"), "sure", "yes"); w.Code == http.StatusSeeOther {
		t.Errorf("a forgetting that could not sweep a file said done")
	}
	if !l.known(edithID) {
		t.Fatal("the person is gone though a file still names her")
	}
	if err := os.Chmod(audit, 0o755); err != nil {
		t.Fatal(err)
	}
	ravi.did(personPath(edithID, "forget"), "sure", "yes")
	if l.known(edithID) {
		t.Error("forgetting again left the person")
	}
	if es, err := l.s.readAuditCopy("ravi-mini"); err != nil || len(es) != 1 || es[0].Maintainer != "" {
		t.Errorf("labd's copy of the Audit log: %+v %v", es, err)
	}
}

// A person renamed on GitHub since the lab wrote of them is forgotten under
// every handle the lab kept beside their account id, and a name kept beside
// their account id goes whatever it says. Another person's handle of today
// is theirs, though the forgotten person once had it.
func TestAForgettingFindsTheHandlesThePersonHadBefore(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	l.signedIn(linusID)
	now := l.clock.Now()
	// Written while she was "edie", and once, long ago, "linus".
	for _, line := range []recordLine{
		{At: now, ActorID: edithID, Actor: "edie", What: "marked as M team", AboutID: graceID, About: "grace", Note: "asked by @edie"},
		{At: now, ActorID: edithID, Actor: "linus", What: "asked to be forgotten"},
		{At: now, ActorID: linusID, Actor: "linus", What: "made a Lab token"},
	} {
		if err := l.s.store.addRecord(line); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := l.s.store.db.Exec(`INSERT INTO lease_record (at, mac, device_id, event, account_id, handle, holder, until, by_id, by, why)
		VALUES (?, 'm1', 'd-m1', 'ended', 0, '', 'grace-laptop', ?, ?, 'Edie R.', 'ended for @edie')`, now.UnixMicro(), now.UnixMicro(), edithID); err != nil {
		t.Fatal(err)
	}
	if err := l.s.store.saveJob(&api.Job{ID: "j-41", Spec: api.JobSpec{Device: "m1"}, Maintainer: "edie", AccountID: edithID, State: api.JobDone, Summary: "asked by @edie", Created: now}); err != nil {
		t.Fatal(err)
	}

	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	if traces := l.traces("edie", -1); len(traces) != 0 {
		t.Errorf("her earlier handle is still in: %v", traces)
	}
	var by string
	l.s.store.db.QueryRow(`SELECT by FROM lease_record WHERE holder = 'grace-laptop'`).Scan(&by)
	if by != "" {
		t.Errorf("the name beside her account id in the Lease record: %q", by)
	}
	if got := strings.Join(l.recorded(), "\n"); !strings.Contains(got, "@linus made a Lab token") {
		t.Errorf("another person's handle of today went:\n%s", got)
	}
}

// An Audit entry that reaches labd after the person it names was Forgotten,
// from a Mac that was out of reach, is kept without the handle.
func TestALateAuditEntryOfAForgottenPersonIsKeptWithoutTheHandle(t *testing.T) {
	l := rolesLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	mini := l.addMac("ravi-mini", maintainerID)
	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	mini.copyAudit([]api.AuditEntry{
		{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40},
		{Time: l.clock.Now(), Maintainer: "ravi", Action: "reboot", End: 80},
		{Time: l.clock.Now(), Action: "kernel errors baseline", End: 120},
	})
	es, err := l.s.readAuditCopy("ravi-mini")
	if err != nil || len(es) != 3 {
		t.Fatalf("labd's copy: %+v %v", es, err)
	}
	if es[0].Maintainer != "" || es[0].Action != "screenshot" {
		t.Errorf("the late entry of the forgotten person: %+v", es[0])
	}
	if es[1].Maintainer != "ravi" || es[1].AccountID != maintainerID {
		t.Errorf("an entry of a person the lab knows: %+v", es[1])
	}
}

// An Installed run's record, which the frozen store keeps beside its
// evidence, loses its person at a forgetting as the job does, and the job's
// own copy with it: the Release's pinned files stay byte for byte, and a
// Promotion still finds the frozen copy to be the job's evidence.
func TestForgettingSomeoneWhoRanAnInstalledRunLeavesNoHandleInFrozen(t *testing.T) {
	l, msha, _ := installedLab(t, nil)
	id := testAccount(harnessMaintainer)
	v := l.s.dev("sim-mac")
	v.mu.Lock()
	v.d.Lease.Maintainer, v.d.Lease.AccountID = harnessMaintainer, id
	v.mu.Unlock()
	resp, body := l.post("/api/jobs", api.JobSpec{Device: "sim-mac", InstalledManifest: msha, Holder: "tester"})
	var sub api.Job
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &sub) != nil {
		t.Fatalf("submit: %s %s", resp.Status, body)
	}
	j := l.wait(sub.ID, 60*time.Second)
	l.expect(j, api.OutcomePass)
	if j.Maintainer != harnessMaintainer || j.AccountID != id {
		t.Fatalf("the run was asked by %q %d", j.Maintainer, j.AccountID)
	}
	dir := l.s.frozenDir("9.9")
	pinnedNow := func() map[string]string {
		out := map[string]string{}
		names, _ := filepath.Glob(filepath.Join(dir, "PINS.*"))
		for _, p := range names {
			pins, err := readPins(p)
			if err != nil {
				t.Fatal(err)
			}
			for rel := range pins {
				b, _ := os.ReadFile(filepath.Join(dir, rel))
				out[rel] = string(b)
			}
		}
		return out
	}
	before := pinnedNow()
	k, err := l.s.store.knownPerson(id)
	if err != nil {
		t.Fatal(err)
	}
	by := Person{AccountID: testAccount("owner"), Handle: "owner", Maintainer: true, Admin: true}
	if err := l.s.forgetHeld(by, k, newForgetting(id, k.Handle)); err != nil {
		t.Fatal(err)
	}

	found := mentions(harnessMaintainer, id)
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if _, ok := before[rel]; ok || strings.HasPrefix(rel, "PINS.") {
			return nil
		}
		if b, _ := os.ReadFile(path); found(string(b)) {
			t.Errorf("frozen/9.9/%s still names the person:\n%s", rel, b)
		}
		return nil
	})
	if after := pinnedNow(); !maps.Equal(before, after) {
		t.Error("the forgetting changed a pinned file")
	}
	jb, _ := os.ReadFile(filepath.Join(l.s.jobDir(j.ID), "installed", "job.json"))
	fb, _ := os.ReadFile(filepath.Join(dir, "installed", "sim-mac", j.ID, "job.json"))
	var rec api.Job
	if string(jb) != string(fb) || json.Unmarshal(fb, &rec) != nil || rec.ID != j.ID || rec.Outcome != api.OutcomePass {
		t.Errorf("the frozen record and the job's own: equal %v, read %+v", string(jb) == string(fb), rec.ID)
	}
	if _, err := l.s.promotion(context.Background(), by, "9.9", promoteReq{Device: "sim-mac", Job: j.ID}); err != nil {
		t.Errorf("a Promotion after the forgetting: %v", err)
	}
}

// A forgetting during an Installed run reads nothing the run writes as it
// goes: the run's result names nobody, and the scrub at the run's next save
// leaves it alone, the run's own pointer and all.
func TestAForgettingDuringAnInstalledRunReadsNothingTheRunWrites(t *testing.T) {
	l := macLab(t, nil)
	l.signedIn(edithID)
	l.lrMac("m1", "j293", nil)
	l.pjJob("j-41", "m1", edithID, "edith", l.clock.Now(), func(j *api.Job) { j.State, j.Outcome = api.JobQueued, "" })
	running, _ := l.s.store.job("j-41")
	res := &api.InstalledResult{Release: "9.9", Verdicts: map[string]string{}}
	l.s.updateJob(running, func(j *api.Job) { j.State, j.Result.Installed = api.JobTesting, res })

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			res.Verdicts[[]string{"before", "booted", "after"}[i%3]] = []string{"MATCH", "MISMATCH"}[i%2]
		}
	}()
	for range 200 {
		end, err := l.s.forgetInJobs(newForgetting(edithID, "edith"))
		if err != nil {
			t.Fatal(err)
		}
		end()
	}
	close(stop)
	<-done
	l.s.updateJob(running, func(*api.Job) {})
	if running.Maintainer != "" || running.AccountID != 0 {
		t.Errorf("the run's copy still names her: %q %d", running.Maintainer, running.AccountID)
	}
	if running.Result.Installed != res {
		t.Error("the scrub replaced the run's own Installed result")
	}
}

// A whole job saved while a forgetting is under way brings nobody back: an
// exclusion saved before the forgetting swept the jobs table, and a run's
// save of its recovery mode, from a copy read before the forgetting, after.
func TestAJobSavedDuringAForgettingBringsNobodyBack(t *testing.T) {
	l := macLab(t, nil)
	l.signedIn(edithID)
	l.lrMac("m1", "j293", nil)
	blocker := l.addMac("ravi-mini", maintainerID)
	blocker.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40}})
	l.pjJob("j-done", "m1", edithID, "edith", l.clock.Now(), nil)
	l.pjJob("j-run", "m1", edithID, "edith", l.clock.Now(), func(j *api.Job) { j.State, j.Outcome = api.JobQueued, "" })
	stale, _ := l.s.store.job("j-run")
	names := func(id string) bool {
		var data string
		l.s.store.db.QueryRow(`SELECT data FROM jobs WHERE id = ?`, id).Scan(&data)
		return strings.Contains(data, "edith") || strings.Contains(data, `"account_id":7`)
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the forgetting never reached %s", what)
			}
		}
	}
	k, err := l.s.store.knownPerson(edithID)
	if err != nil {
		t.Fatal(err)
	}

	// The forgetting waits at the caller log, before the databases, and at
	// a Mac, after them.
	l.s.callers.mu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- l.s.forgetHeld(Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}, k, newForgetting(edithID, k.Handle))
	}()
	waitFor("the jobs", func() bool {
		jobMu.Lock()
		defer jobMu.Unlock()
		return len(l.s.inFlight.forgot["j-run"]) > 0
	})
	req := httptest.NewRequest("POST", "/api/jobs/j-done/exclude", strings.NewReader(`{"reason":"the access point went down","by":"ravi"}`))
	req.SetPathValue("id", "j-done")
	w := httptest.NewRecorder()
	l.s.hExclude(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("the exclusion: %d %s", w.Code, w.Body)
	}
	if names("j-done") {
		t.Error("an exclusion during the forgetting saved the job with her in it")
	}
	// The sweep of the Audit log copies reads every Mac under its lock: the
	// Mac is held only once the forgetting is past it, at the caller log.
	waitFor("the Audit log copies", func() bool {
		es, err := l.s.readAuditCopy("ravi-mini")
		return err == nil && len(es) == 1 && es[0].Maintainer == ""
	})
	blocker.mu.Lock()
	l.s.callers.mu.Unlock()
	waitFor("the databases", func() bool { return !l.known(edithID) })
	if err := l.s.beginJobRecoveryMode(context.Background(), l.s.dev("m1"), stale); err != nil {
		t.Fatal(err)
	}
	blocker.mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"j-done", "j-run"} {
		if names(id) {
			t.Errorf("job %s names her after the forgetting", id)
		}
	}
}

// An Audit entry that reaches labd while a forgetting sweeps labd's copy
// waits for the sweep, and the sweep for it: the entry is neither lost nor
// kept with the handle, though it was matched to the person before the
// forgetting.
func TestAnAuditEntryArrivingDuringAForgettingIsKeptWithoutTheHandle(t *testing.T) {
	l := rolesLab(t, nil)
	l.signedIn(edithID)
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40}})
	k, err := l.s.store.knownPerson(edithID)
	if err != nil {
		t.Fatal(err)
	}

	// The Mac's report is being copied: its entry was matched to her already.
	l.s.auditWrite.RLock()
	mini.auditMu.Lock()
	done := make(chan error, 1)
	go func() {
		done <- l.s.forgetHeld(Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}, k, newForgetting(edithID, k.Handle))
	}()
	select {
	case err := <-done:
		done <- err
	case <-time.After(time.Second):
	}
	line, _ := json.Marshal(api.AuditEntry{Time: l.clock.Now(), Maintainer: "edith", AccountID: edithID, Action: "reboot", End: 80})
	f, err := os.OpenFile(l.s.auditPath("ravi-mini"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(line, '\n'))
	f.Close()
	mini.auditHave = 80
	mini.auditMu.Unlock()
	l.s.auditWrite.RUnlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	es, err := l.s.readAuditCopy("ravi-mini")
	if err != nil || len(es) != 2 || es[1].Action != "reboot" {
		t.Fatalf("labd's copy of the Audit log: %+v %v", es, err)
	}
	for _, e := range es {
		if e.Maintainer != "" || e.AccountID != 0 {
			t.Errorf("an entry still names her: %+v", e)
		}
	}
}

// A handle the person had that only an Audit entry kept beside their
// account id is forgotten too: in labd's copies of Audit logs, and on an
// old report that names its person by handle alone.
func TestAForgettingFindsAnEarlierHandleInTheAuditLogs(t *testing.T) {
	l := macLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40}})
	line, _ := json.Marshal(api.AuditEntry{Time: l.clock.Now(), Maintainer: "edie", AccountID: edithID, Action: "reboot", End: 80})
	f, err := os.OpenFile(l.s.auditPath("ravi-mini"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write(append(line, '\n'))
	f.Close()
	l.mustMacSignIn("air", linusID)
	old := l.storedReport(l.macUpload("air", "m2-max-image2")).PublicID
	if _, err := l.openDB("reports.db").Exec(`UPDATE reports SET handle = 'edie', account_id = NULL WHERE public_id = ?`, old); err != nil {
		t.Fatal(err)
	}

	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	if traces := l.traces("edie", 1<<40); len(traces) != 0 {
		t.Errorf("her earlier handle is still in: %v", traces)
	}
	if r, err := l.s.reports.Report(old); err != nil || r.Handle != "" {
		t.Errorf("the old report: %v", err)
	}
}

// submitInstalledRun has the harness's Maintainer, under a Lease of
// sim-mac, submit an Installed run of the frozen Release; it is not waited for.
func submitInstalledRun(t *testing.T, l *lab, msha string) string {
	t.Helper()
	v := l.s.dev("sim-mac")
	v.mu.Lock()
	v.d.Lease.Maintainer, v.d.Lease.AccountID = harnessMaintainer, testAccount(harnessMaintainer)
	v.mu.Unlock()
	resp, body := l.post("/api/jobs", api.JobSpec{Device: "sim-mac", InstalledManifest: msha, Holder: "tester"})
	var sub api.Job
	if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &sub) != nil {
		t.Fatalf("submit: %s %s", resp.Status, body)
	}
	return sub.ID
}

// A forgetting that comes while an Installed run is being sealed waits for
// the seal: the record the run publishes to frozen/ is swept with the rest,
// and the job's own copy with it.
func TestAForgettingWhileAnInstalledRunIsSealedLeavesNoHandleInFrozen(t *testing.T) {
	l, msha, _ := installedLab(t, nil)
	id := testAccount(harnessMaintainer)
	k, err := l.s.store.knownPerson(id)
	if err != nil {
		t.Fatal(err)
	}
	by := Person{AccountID: testAccount("owner"), Handle: "owner", Maintainer: true, Admin: true}
	frozenMacs := filepath.Join(l.s.frozenDir("9.9"), "installed", "sim-mac")
	forgot := make(chan error, 1)
	started := false
	saved := syncDir
	t.Cleanup(func() { syncDir = saved })
	// The run has written its record and is about to publish it.
	syncDir = func(p string) error {
		if p == frozenMacs && !started {
			started = true
			go func() { forgot <- l.s.forgetHeld(by, k, newForgetting(id, k.Handle)) }()
			select {
			case err := <-forgot:
				forgot <- err
			case <-time.After(2 * time.Second):
			}
		}
		return saved(p)
	}
	j := l.wait(submitInstalledRun(t, l, msha), 60*time.Second)
	select {
	case err := <-forgot:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the forgetting never ended")
	}
	if !started {
		t.Fatalf("the run was never sealed: %s %s", j.Outcome, j.Summary)
	}
	found := mentions(harnessMaintainer, id)
	for _, p := range []string{filepath.Join(frozenMacs, j.ID, "job.json"), filepath.Join(l.s.jobDir(j.ID), "installed", "job.json")} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if found(string(b)) {
			rel, _ := filepath.Rel(l.s.cfg.DataDir, p)
			t.Errorf("%s still names the person:\n%s", rel, b)
		}
	}
}

// Forgetting someone whose handle is a word of an Installed run's kernel
// error lines breaks none of the run's evidence: the compared logs and their
// comparisons name nobody and stay as they were, and a Promotion still passes.
func TestForgettingSomeoneTheKernelLogNamesLeavesTheRunPromotable(t *testing.T) {
	l, msha, _ := installedLab(t, nil)
	j := l.wait(submitInstalledRun(t, l, msha), 60*time.Second)
	l.expect(j, api.OutcomePass)
	frozen := filepath.Join(l.s.frozenDir("9.9"), "installed", "sim-mac", j.ID)
	evidence := func() map[string]string {
		out := map[string]string{}
		for _, dir := range []string{frozen, filepath.Join(l.s.jobDir(j.ID), "installed")} {
			for _, name := range []string{"dmesg-errors.txt", "dmesg-compare.json", "omt-compare.json"} {
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				out[filepath.Join(dir, name)] = string(b)
			}
		}
		return out
	}
	before := evidence()
	if dm := before[filepath.Join(frozen, "dmesg-errors.txt")]; !mentions("fault", -1)(dm) {
		t.Fatalf("the run's kernel errors do not say fault:\n%s", dm)
	}
	by := Person{AccountID: testAccount("owner"), Handle: "owner", Maintainer: true, Admin: true}
	fault := knownPerson{Person: Person{AccountID: 4242, Handle: "fault"}}
	if err := l.s.forgetHeld(by, fault, newForgetting(fault.AccountID, fault.Handle)); err != nil {
		t.Fatal(err)
	}
	if after := evidence(); !maps.Equal(before, after) {
		t.Error("the forgetting rewrote the run's compared evidence")
	}
	if _, err := l.s.promotion(context.Background(), by, "9.9", promoteReq{Device: "sim-mac", Job: j.ID}); err != nil {
		t.Errorf("a Promotion after the forgetting: %v", err)
	}
}

// A finished job read before a forgetting and saved after it, as a cancel
// that finds it done saves it, brings nobody back, however many such copies
// were read.
func TestAFinishedJobReadBeforeAForgettingAndSavedAfterNamesNobody(t *testing.T) {
	l := macLab(t, nil)
	ravi := l.signedIn(maintainerID)
	l.signedIn(edithID)
	l.lrMac("m1", "j293", nil)
	l.pjJob("j-41", "m1", edithID, "edith", l.clock.Now(), func(j *api.Job) { j.Summary = "booted for @edith" })
	first, _ := l.s.store.job("j-41")
	second, _ := l.s.store.job("j-41")

	ravi.did(personPath(edithID, "forget"), "sure", "yes")

	names := func() string {
		var data string
		l.s.store.db.QueryRow(`SELECT data FROM jobs WHERE id = 'j-41'`).Scan(&data)
		if strings.Contains(data, "edith") || strings.Contains(data, `"account_id":`+strconv.Itoa(edithID)) {
			return data
		}
		return ""
	}
	if data := names(); data != "" {
		t.Fatalf("the forgetting left her on the job:\n%s", data)
	}
	// cancelJob's save of a job it finds done changes nothing in it.
	l.s.updateJob(first, func(j *api.Job) {})
	if data := names(); data != "" {
		t.Errorf("a cancel's copy read before the forgetting wrote her back:\n%s", data)
	}
	l.s.updateJob(second, func(j *api.Job) { j.Result.Logs = append(j.Result.Logs, "serial.log") })
	if data := names(); data != "" {
		t.Errorf("a second copy read before the forgetting wrote her back:\n%s", data)
	}
}

// A Mac that Joins while a forgetting sweeps labd's copies of Audit logs
// adds its entries once the sweep is done, not during it: an append between
// the sweep's read of a copy and its replacing would be lost. The entry is
// kept, without the handle.
func TestAnAuditEntryFromAMacJoinedDuringAForgettingWaitsForTheSweepAndIsKept(t *testing.T) {
	l := rolesLab(t, nil)
	l.signedIn(edithID)
	mini := l.addMac("ravi-mini", maintainerID)
	mini.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "screenshot", End: 40}})
	k, err := l.s.store.knownPerson(edithID)
	if err != nil {
		t.Fatal(err)
	}
	// The sweep reads this first, and waits there until it is written.
	held := filepath.Join(l.s.cfg.DataDir, "audit", "0-held.jsonl")
	if err := syscall.Mkfifo(held, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- l.s.forgetHeld(Person{AccountID: maintainerID, Handle: "ravi", Maintainer: true}, k, newForgetting(edithID, k.Handle))
	}()
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(held, os.O_WRONLY, 0)
		if err == nil {
			opened <- f
		}
	}()
	var w *os.File
	select {
	case w = <-opened:
	case err := <-done:
		t.Fatalf("the forgetting ended without sweeping the Audit log copies: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the forgetting never reached the Audit log copies")
	}

	newcomer := l.addMac("zed-air", maintainerID)
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		newcomer.copyAudit([]api.AuditEntry{{Time: l.clock.Now(), Maintainer: "edith", Action: "reboot", End: 30}})
	}()
	select {
	case <-copied:
		t.Error("a Mac Joined during the sweep added to its Audit log copy while the sweep ran")
	case <-time.After(300 * time.Millisecond):
	}
	os.Remove(held)
	w.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-copied

	es, err := l.s.readAuditCopy("zed-air")
	if err != nil || len(es) != 1 || es[0].Action != "reboot" {
		t.Fatalf("the newcomer's copy of its Audit log: %+v %v", es, err)
	}
	if es[0].Maintainer != "" || es[0].AccountID != 0 {
		t.Errorf("the newcomer's entry still names her: %+v", es[0])
	}
}
