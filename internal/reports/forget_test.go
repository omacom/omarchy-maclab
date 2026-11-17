package reports

import (
	"net/http"
	"testing"
)

// Forgetting a person, as section 14's table has it for reports.db: the
// handle and account id stamped on their reports are erased, and so is a
// lab-published report's "who asked"; the reports stay, Published, with
// Verified or Community and the machine id; their digests stay; their Mac
// sign-ins are erased.
func TestForgetTakesThePersonOffReportsAndKeepsTheReports(t *testing.T) {
	d := newDesk(t)
	const vera, ravi = 7700123, 7700456
	var ids []string
	for i, machine := range []string{"a", "b", "c"} {
		a := d.post(t, run(t, i), machine)
		if a.status != http.StatusCreated {
			t.Fatalf("upload %d: %d %v", i, a.status, a.body)
		}
		ids = append(ids, a.text("id"))
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.store.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE reports SET handle = 'Vera', account_id = ?, verified = 1 WHERE public_id IN (?, ?)`, vera, ids[0], ids[1])
	exec(`UPDATE reports SET lab_job = 'j-1', asked_handle = 'vera', asked_account_id = ? WHERE public_id = ?`, vera, ids[1])
	exec(`UPDATE reports SET handle = 'ravi', account_id = ?, lab_job = 'j-2', asked_handle = 'ravi', asked_account_id = ? WHERE public_id = ?`, ravi, ravi, ids[2])
	if _, err := d.store.signIn("machine-of-vera", vera, "vera", d.now); err != nil {
		t.Fatal(err)
	}
	if _, err := d.store.signIn("machine-of-ravi", ravi, "ravi", d.now); err != nil {
		t.Fatal(err)
	}

	if err := d.store.Forget(vera, "vera"); err != nil {
		t.Fatal(err)
	}

	var stands int64
	for _, id := range ids[:2] {
		r, err := d.store.Report(id)
		if err != nil {
			t.Fatalf("the report %s was not kept: %v", id, err)
		}
		if r.Handle != "" || r.AccountID >= 0 || !r.NameNotKept() {
			t.Errorf("report %s still names its person: %q %d", id, r.Handle, r.AccountID)
		}
		if stands != 0 && r.AccountID != stands {
			t.Errorf("the person's two reports no longer count as one person's: %d and %d", stands, r.AccountID)
		}
		stands = r.AccountID
		if !r.Verified || r.MachineID == "" || r.Hidden {
			t.Errorf("report %s lost what it keeps: verified %v, machine id %q, hidden %v", id, r.Verified, r.MachineID, r.Hidden)
		}
	}
	asked, _ := d.store.Report(ids[1])
	if asked.AskedHandle != "" || asked.AskedAccountID >= 0 || !asked.AskedNameNotKept() || asked.LabJob != "j-1" {
		t.Errorf("a lab-published report's who asked: %q %d, job %q", asked.AskedHandle, asked.AskedAccountID, asked.LabJob)
	}
	other, _ := d.store.Report(ids[2])
	if other.Handle != "ravi" || other.AccountID != ravi || other.AskedHandle != "ravi" || other.NameNotKept() || other.AskedNameNotKept() {
		t.Errorf("another person's report changed: %+v", other)
	}
	if n := len(d.store.list(t, `SELECT digest FROM report_digests`)); n != 3 {
		t.Errorf("%d report digests kept, want 3", n)
	}
	if left, _ := d.store.SignInsOf(vera); len(left) != 0 {
		t.Errorf("the forgotten person's Mac sign-ins are kept: %v", left)
	}
	if left, _ := d.store.SignInsOf(ravi); len(left) != 1 {
		t.Errorf("another person's Mac sign-in went: %v", left)
	}
	if n := len(d.store.list(t, `SELECT public_id FROM reports WHERE handle = 'vera' COLLATE NOCASE OR asked_handle = 'vera' OR account_id = ? OR asked_account_id = ?`, vera, vera)); n != 0 {
		t.Errorf("%d reports still name the person", n)
	}
}

// A report kept from before the lab knew account ids names its person by
// handle alone: it goes by every handle the person had, the ones before a
// rename too.
func TestForgetGoesByEveryHandleThePersonHad(t *testing.T) {
	d := newDesk(t)
	const vera = 7700123
	var ids []string
	for i, machine := range []string{"a", "b", "c"} {
		a := d.post(t, run(t, i), machine)
		if a.status != http.StatusCreated {
			t.Fatalf("upload %d: %d %v", i, a.status, a.body)
		}
		ids = append(ids, a.text("id"))
	}
	for i, handle := range []string{"vera", "vee", "ravi"} {
		if _, err := d.store.db.Exec(`UPDATE reports SET handle = ?, account_id = NULL WHERE public_id = ?`, handle, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.store.Forget(vera, "vera", "vee"); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"", "", "ravi"} {
		if r, err := d.store.Report(ids[i]); err != nil || r.Handle != want {
			t.Errorf("report %d names %q, want %q (%v)", i, r.Handle, want, err)
		}
	}
}
