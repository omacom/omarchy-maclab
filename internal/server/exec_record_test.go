package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// lab exec: Lease first, for everyone; and the Exec record, which keeps
// every exec and every refused one, with no output.

func (l *lab) execAs(m testMaintainer, command string) (*http.Response, string) {
	l.t.Helper()
	return l.as(m.Token, "POST", "/api/devices/"+l.name+"/exec", api.ExecArgs{Command: command})
}

// execLines is the Mac's Exec record, oldest first.
func execLines(t *testing.T, s *Server, mac string) []api.ExecEvent {
	t.Helper()
	events, err := s.store.execRecord(s.dev(mac).snapshot().ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

func TestNobodyRunsLabExecWithoutTheirOwnLease(t *testing.T) {
	wallClock(t)
	l := newLab(t, true)
	l.baseline()
	grace := l.member("grace")
	ada := testMaintainer{Name: harnessMaintainer, Token: harnessToken}
	started := time.Now()

	refused := func(when string, m testMaintainer, command string, status int, saying string) {
		t.Helper()
		if r, body := l.execAs(m, command); r.StatusCode != status || !strings.Contains(body, saying) || strings.Contains(body, "sim ran") {
			t.Fatalf("%s, %s ran lab exec: %s %s; want %d saying %q", when, m.Name, r.Status, body, status, saying)
		}
	}
	refused("on a free Mac", ada, "id ada-free", http.StatusConflict, "your own Lease on "+l.name+", a Maintainer's too: `lab lease "+l.name+"`")
	refused("on a free Mac", grace, "id grace-free", http.StatusForbidden, "@grace needs their own Lease on "+l.name)

	l.leaseAs(grace, 0)
	refused("under grace's Lease", ada, "id ada-under-grace", http.StatusConflict, "leased by grace@laptop (grace)")
	r, body := l.execAs(grace, "uname -r")
	var res api.ExecResult
	if json.Unmarshal([]byte(body), &res); r.StatusCode != 200 || res.Stdout != "sim ran: uname -r\n" || res.Via != "agent" {
		t.Fatalf("under her own Lease, grace ran lab exec: %s %s", r.Status, body)
	}
	if r, body := l.as(grace.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != 200 {
		t.Fatalf("grace releasing: %s %s", r.Status, body)
	}

	l.leaseAs(ada, 0)
	refused("under ada's Lease", grace, "id grace-under-ada", http.StatusForbidden, "@grace needs their own Lease on "+l.name)
	if r, body := l.execAs(ada, "exit 3"); r.StatusCode != 200 || !strings.Contains(body, `"exit_code":3`) {
		t.Fatalf("under her own Lease, ada ran lab exec: %s %s", r.Status, body)
	}

	got := execLines(t, l.s, l.name)
	want := []struct {
		handle, command, state string
		exit                   int
	}{
		{"ada", "id ada-free", api.ExecRefused, -1},
		{"grace", "id grace-free", api.ExecRefused, -1},
		{"ada", "id ada-under-grace", api.ExecRefused, -1},
		{"grace", "uname -r", api.ExecRan, 0},
		{"grace", "id grace-under-ada", api.ExecRefused, -1},
		{"ada", "exit 3", api.ExecRan, 3},
	}
	if len(got) != len(want) {
		t.Fatalf("the Exec record has %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		e := got[i]
		if e.Handle != w.handle || e.AccountID != testAccount(w.handle) || e.Mac != l.name || e.Command != w.command || e.State != w.state {
			t.Errorf("line %d of the Exec record: %+v, want %+v", i, e, w)
		}
		if e.Time.Before(started.Add(-time.Second)) || e.Time.After(time.Now().Add(time.Second)) {
			t.Errorf("line %d of the Exec record says it happened at %s", i, e.Time)
		}
		switch {
		case w.state == api.ExecRefused && (e.ExitCode != nil || e.Why == "" || !strings.Contains(e.Why, "Lease") && !strings.Contains(e.Why, "leased")):
			t.Errorf("a refused attempt is recorded with why and no exit code: %+v", e)
		case w.state == api.ExecRan && (e.ExitCode == nil || *e.ExitCode != w.exit || e.Seconds <= 0 || e.Why != ""):
			t.Errorf("a command that ran is recorded with its exit code and how long it ran: %+v", e)
		}
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "sim ran") {
		t.Fatalf("the Exec record keeps output: %s", b)
	}
	var kept string
	rows, err := l.s.store.db.Query(`SELECT sql FROM sqlite_master WHERE name = 'exec_record'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		rows.Scan(&kept)
	}
	if kept == "" || strings.Contains(kept, "stdout") || strings.Contains(kept, "output") {
		t.Fatalf("the Exec record's table: %q", kept)
	}
}

// A command sent to a Mac whose answer never came is in the record too,
// with no exit code and why.
func TestAnExecWhoseAnswerNeverCameIsRecorded(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	ada := testMaintainer{Name: harnessMaintainer, Token: harnessToken}
	l.leaseAs(ada, 0)
	asked := make(chan struct{})
	l.sim.OnExec = func(string) (string, bool) {
		close(asked)
		time.Sleep(2 * time.Second)
		return "late", true
	}
	done := make(chan string, 1)
	go func() {
		_, body := l.execAs(ada, "sleep 600")
		done <- body
	}()
	<-asked
	if r, body := l.as(ada.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != 200 {
		t.Fatalf("ada releasing: %s %s", r.Status, body)
	}
	if body := <-done; !strings.Contains(body, "Lease on "+l.name+" ended") {
		t.Fatalf("the cut exec answered %q", body)
	}
	got := execLines(t, l.s, l.name)
	if len(got) != 1 || got[0].State != api.ExecLost || got[0].ExitCode != nil || got[0].Command != "sleep 600" || !strings.Contains(got[0].Why, "Lease on "+l.name+" ended") {
		t.Fatalf("the Exec record of a cut exec: %+v", got)
	}
}

// A Lease that ends while the Exec record's line is written leaves nothing
// to run: the command is queued for the Mac only in the moment that finds
// the Lease still its person's and their session not cut. The line that
// said it was going ends as refused.
func TestAnExecIsNotQueuedOnceItsLeaseEnded(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	grace := l.member("grace")
	v := l.s.dev(l.name)
	var reached atomic.Int32
	l.sim.OnExec = func(string) (string, bool) {
		reached.Add(1)
		return "", false
	}
	ends := []struct {
		when string
		end  func()
	}{
		// What endLease leaves for a moment: the Lease gone, its line not
		// yet in the Lease record, the holder's sessions not yet cut.
		{"gone-not-cut", func() {
			v.mu.Lock()
			v.d.Lease = nil
			v.mu.Unlock()
		}},
		{"released", func() {
			if r, body := l.as(grace.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != 200 {
				t.Errorf("grace releasing: %s %s", r.Status, body)
			}
		}},
	}
	for _, c := range ends {
		l.leaseAs(grace, 0)
		l.s.execRecorded = c.end
		r, body := l.execAs(grace, "id "+c.when)
		l.s.execRecorded = nil
		if r.StatusCode != http.StatusConflict || !strings.Contains(body, "it was not sent") {
			t.Errorf("an exec whose Lease ended (%s) before it was queued: %s %s", c.when, r.Status, body)
		}
	}
	// Long enough for the Mac to have taken a command that was queued.
	time.Sleep(1500 * time.Millisecond)
	if n := reached.Load(); n != 0 {
		t.Errorf("%d commands reached the Mac after their Lease ended", n)
	}
	got := execLines(t, l.s, l.name)
	if len(got) != len(ends) {
		t.Fatalf("the Exec record has %d lines, want %d: %+v", len(got), len(ends), got)
	}
	for i, e := range got {
		if e.State != api.ExecRefused || e.ExitCode != nil || e.Command != "id "+ends[i].when || !strings.Contains(e.Why, "Lease on "+l.name+" ended") {
			t.Errorf("the Exec record of an exec whose Lease ended (%s) before it was queued: %+v", ends[i].when, e)
		}
	}
}

const lrKen = 8103

func TestTheExecRecordIsShownToItsThreeKindsOfPeople(t *testing.T) {
	s := leaseLab(t)
	s.gh.AddUser(lrKen, "ken")
	s.lrMac("m2-max", "j414c", lrVolunteers(api.AccessFull, s.s.now().Add(time.Hour)))
	s.lrLend("m2-max", lrLinus)
	s.lrMac("m1-air", "j313", nil)
	grace, ken, linus, ravi, other := s.lrMember(lrGrace), s.lrMember(lrKen), s.signedIn(lrLinus), s.lrMaintainer(), s.signedIn(nobodyID)

	ran := func(mac string, id int64, handle, command string) {
		t.Helper()
		v := s.s.dev(mac)
		n, err := s.s.store.addExec(v.snapshot().ID, api.ExecEvent{Time: s.s.now(), Mac: mac, AccountID: id, Handle: handle, Command: command, State: api.ExecRunning})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.s.store.endExec(n, api.ExecRan, 0, 0.5, ""); err != nil {
			t.Fatal(err)
		}
	}
	ran("m2-max", lrGrace, "grace", "echo graces-own")
	ran("m2-max", lrKen, "ken", "echo kens-own")
	ran("m1-air", lrKen, "ken", "echo ken-on-the-air")
	// A refused attempt, through the route: ravi holds no Lease on it.
	if w := ravi.write("POST", "/api/devices/m2-max/exec", `{"command":"echo ravis-refused"}`, "Content-Type", "application/json"); w.Code != http.StatusConflict {
		t.Fatalf("ravi's exec with no Lease: %d %s", w.Code, w.Body)
	}

	sees := func(who string, b *browser, mac string, status int, want ...string) {
		t.Helper()
		w := b.do("GET", "/api/devices/"+mac+"/execs", "")
		if w.Code != status {
			t.Errorf("%s reading the Exec record of %s: %d %s, want %d", who, mac, w.Code, w.Body, status)
			return
		}
		var got []string
		if status == 200 {
			var events []api.ExecEvent
			if err := json.Unmarshal(w.Body.Bytes(), &events); err != nil {
				t.Errorf("%s reading the Exec record of %s: %v in %s", who, mac, err, w.Body)
				return
			}
			for i := len(events) - 1; i >= 0; i-- {
				got = append(got, events[i].Command)
			}
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s sees of the Exec record of %s: %q, want %q", who, mac, got, want)
		}
		for _, command := range []string{"graces-own", "kens-own", "ravis-refused", "ken-on-the-air"} {
			if strings.Contains(w.Body.String(), command) && !strings.Contains(strings.Join(want, "|"), command) {
				t.Errorf("%s was shown %q of the Exec record of %s: %s", who, command, mac, w.Body)
			}
		}
	}
	all := []string{"echo graces-own", "echo kens-own", "echo ravis-refused"}
	sees("a Maintainer", ravi, "m2-max", 200, all...)
	sees("the Mac's Volunteer", linus, "m2-max", 200, all...)
	sees("the person who ran one", grace, "m2-max", 200, "echo graces-own")
	sees("the person who ran another", ken, "m2-max", 200, "echo kens-own")
	sees("a member who ran none there", grace, "m1-air", 200)
	sees("a Volunteer, of a Mac that is not theirs", linus, "m1-air", 404)
	sees("someone with no role", other, "m2-max", 403)
	sees("nobody signed in", s.browser(), "m2-max", 401)

	// The same lines are on Records of the Mac's page, newest first.
	onPage := func(who string, b *browser, mac string, want ...string) {
		t.Helper()
		record := rowText(part(t, b.macPage(mac, "records"), `id="exec-record"`, "</section>"))
		at := -1
		for i := len(want) - 1; i >= 0; i-- {
			next := strings.Index(record, want[i])
			if next < 0 || next < at {
				t.Errorf("%s's Exec record on the page of %s lacks %q, or is not newest first: %s", who, mac, want[i], record)
			}
			at = next
		}
		for _, command := range []string{"graces-own", "kens-own", "ravis-refused", "ken-on-the-air"} {
			if strings.Contains(record, command) && !strings.Contains(strings.Join(want, "|"), command) {
				t.Errorf("%s was shown %q on the page of %s: %s", who, command, mac, record)
			}
		}
		if len(want) == 0 && !strings.Contains(record, "No lab exec is recorded") {
			t.Errorf("%s's empty Exec record on the page of %s: %s", who, mac, record)
		}
	}
	lines := []string{"@grace on m2-max: echo graces-own (exit 0, 0.5 s)", "@ken on m2-max: echo kens-own (exit 0, 0.5 s)", "@ravi on m2-max: echo ravis-refused (refused: lab exec needs your own Lease on m2-max"}
	onPage("a Maintainer", ravi, "m2-max", lines...)
	onPage("the Mac's Volunteer", linus, "m2-max", lines...)
	onPage("the person who ran one", grace, "m2-max", lines[0])
	onPage("the person who ran another", ken, "m2-max", lines[1])
	onPage("a member who ran none there", grace, "m1-air")

	// An M team member who is the Mac's Volunteer reads all of it.
	s.lrLend("m1-air", lrGrace)
	sees("a member who lends the Mac", grace, "m1-air", 200, "echo ken-on-the-air")
	onPage("a member who lends the Mac", grace, "m1-air", "@ken on m1-air: echo ken-on-the-air")
}

func TestAnExecRecordLinePast12MonthsIsDeleted(t *testing.T) {
	s := leaseLab(t)
	s.lrMac("m1-air", "j313", nil)
	id := s.s.dev("m1-air").snapshot().ID
	now := s.s.now()
	year := now.AddDate(0, -12, 0)
	for command, at := range map[string]time.Time{
		"a year and a minute ago": year.Add(-time.Minute),
		"two years ago":           now.AddDate(-2, 0, 0),
		"a year less an hour ago": year.Add(time.Hour),
		"now":                     now,
	} {
		if _, err := s.s.store.addExec(id, api.ExecEvent{Time: at, Mac: "m1-air", AccountID: lrGrace, Handle: "grace", Command: command, State: api.ExecRefused, Why: "no Lease"}); err != nil {
			t.Fatal(err)
		}
	}
	kept := func() string {
		var out []string
		for _, e := range execLines(t, s.s, "m1-air") {
			out = append(out, e.Command)
		}
		return strings.Join(out, "|")
	}
	s.s.dropOldExecLines()
	if got := kept(); got != "a year less an hour ago|now" && got != "now|a year less an hour ago" {
		t.Fatalf("after 12 months the Exec record keeps %q", got)
	}

	// labd does it by itself, every hour.
	s.clock.Advance(61 * time.Minute)
	deadline := time.Now().Add(5 * time.Second)
	for kept() != "now" {
		if time.Now().After(deadline) {
			t.Fatalf("an hour later the Exec record still keeps %q", kept())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
