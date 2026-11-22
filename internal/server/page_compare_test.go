package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// pcSets stores the sets "7.2" (captured) and "kernel-aurora16" for a
// Compare to name.
func (l *signInLab) pcSets() (before, after *api.PackageSet) {
	l.t.Helper()
	before = &api.PackageSet{Name: "7.2", CreatedBy: "ravi", Created: l.s.now(), CapturedFrom: "m1-pro", Items: []api.SetItem{
		{Package: "linux-aurora", Source: api.SetRepo, Ref: "7.2.0.aurora1-1", Version: "7.2.0.aurora1-1"}}}
	after = &api.PackageSet{Name: "kernel-aurora16", CreatedBy: "ravi", Created: l.s.now(), Items: []api.SetItem{
		{Package: "linux-aurora", Source: api.SetRepo, Ref: "7.3.0.aurora16-1", Version: "7.3.0.aurora16-1"}}}
	for _, set := range []*api.PackageSet{before, after} {
		if _, err := l.s.store.addSet(set); err != nil {
			l.t.Fatal(err)
		}
	}
	return before, after
}

// pcCompare stores a finished Compare on m1-pro asked by ravi at at, which
// ran for 20 minutes; change sets what the test is about.
func (l *signInLab) pcCompare(id string, at time.Time, cs api.CompareSpec, change func(*api.Job)) *api.Job {
	l.t.Helper()
	return l.pjJob(id, "m1-pro", maintainerID, "ravi", at, func(j *api.Job) {
		j.Spec.Holder, j.Spec.Compare = compareHolder, &cs
		j.Summary = "Compare " + cs.Before + " and " + cs.After + ": 1 better, 1 worse"
		j.Events = append(j.Events, api.JobEvent{Time: at.Add(20 * time.Minute), Msg: "done: pass: " + j.Summary})
		j.Updated = at.Add(20 * time.Minute)
		better, worse := 25.0, -20.0
		a, b := 20.0, 25.0
		j.Result.Compare = &api.CompareResult{Before: cs.Before, After: cs.After, Tier: cs.Tier, Regressed: 1, Fixed: 0,
			Sides: []api.CompareSide{{Side: api.SideA, Set: cs.Before, Boot: "known-good"}, {Side: api.SideB, Set: cs.After, Boot: "confirmed"}},
			Rows: []api.CompareRow{
				{Name: "speedometer-chrome", Unit: "score", A: &a, B: &b, Delta: &better, Verdict: api.VerdictBetter, HigherIsBetter: true},
				{Name: "glmark2", Unit: "score", A: &a, B: &b, Delta: &worse, Verdict: api.VerdictWorse, HigherIsBetter: true},
				{Name: "vkmark", Verdict: api.VerdictNotCompared},
			}}
		if change != nil {
			change(j)
		}
	})
}

// A Compare's job page says what the Compare is (its sets, tier, macOS and
// hands), not a kernel and tests it does not have, and how long it ran, not
// how long until it was published.
func TestACompareJobPageSaysWhatTheCompareIs(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	ravi := l.lrMaintainer()
	_, after := l.pcSets()
	at := l.s.now().Add(-3 * time.Hour)
	l.pcCompare("j-cmp", at, api.CompareSpec{Before: api.CompareCurrent, After: "kernel-aurora16", AfterSet: after, Tier: api.TierDefault, MacOS: true}, func(j *api.Job) {
		published := at.Add(2*time.Hour + 13*time.Minute)
		j.Result.Compare.Published, j.Result.Compare.PublishedBy = &published, "ravi"
		j.Updated = published
	})
	page := ravi.page("/jobs/j-cmp")
	facts := rowText(part(t, page, `<dl class="facts">`, `</dl>`))
	for _, want := range []string{"Took 20 min 0 s", "Before (A) current: the Mac's known-good system", "After (B) kernel-aurora16",
		"Tier default", "macOS yes", "A person at the Mac no"} {
		if !strings.Contains(facts, want) {
			t.Errorf("the Compare's facts lack %q:\n%s", want, facts)
		}
	}
	for _, unwanted := range []string{"2 h 13 min", "Kernel", "Known-good kernel", "boot-health", "Tests"} {
		if strings.Contains(facts, unwanted) {
			t.Errorf("the Compare's facts say %q:\n%s", unwanted, facts)
		}
	}
	if !strings.Contains(page, `href="/builds/sets/kernel-aurora16"`) {
		t.Error("the Compare's job page does not link its after set")
	}
	main := part(t, page, `<main`, `</main>`)
	if got := buttons(main); got != "Retry" {
		t.Errorf("a finished, published Compare's buttons: %q", got)
	}
	if text := rowText(main); !strings.Contains(text, "Queues the same Compare again") || strings.Contains(text, "kernel, tests and arguments") {
		t.Errorf("Retry on a Compare says:\n%s", text)
	}
}

// Retry of a Compare queues the same Compare: the same Mac, Package sets,
// Benchmark set (not the one current now), tier and flags, as whoever asks,
// under a Lease labd takes for them. Only a Maintainer is offered it.
func TestRetryOfACompareQueuesTheSameCompare(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	ravi, grace := l.lrMaintainer(), l.lrMember(lrGrace)
	before, after := l.pcSets()
	at := l.s.now().Add(-time.Hour)
	old := l.pcCompare("j-cmp", at, api.CompareSpec{Before: "7.2", After: "kernel-aurora16", BeforeSet: before, AfterSet: after,
		Tier: api.TierExtended, Hands: true, Bench: "bench-0", LeaseTaken: true}, func(j *api.Job) {
		published := at.Add(30 * time.Minute)
		j.Result.Compare.Published, j.Result.Compare.PublishedBy = &published, "ravi"
	})

	if got := buttons(part(t, grace.page("/jobs/"+old.ID), `<main`, `</main>`)); got != "" {
		t.Errorf("an M team member's buttons on a published Compare: %q", got)
	}
	if w := grace.form("/jobs/" + old.ID + "/retry"); w.Code != http.StatusForbidden {
		t.Errorf("an M team member retrying a Compare: %d", w.Code)
	}

	w := ravi.form("/jobs/" + old.ID + "/retry")
	again := strings.TrimPrefix(w.Header().Get("Location"), "/jobs/")
	j, err := l.s.store.job(again)
	if w.Code != http.StatusSeeOther || err != nil {
		t.Fatalf("Retry: %d to %q: %v %s", w.Code, w.Header().Get("Location"), err, w.Body)
	}
	cs := j.Spec.Compare
	if cs == nil || j.RetryOf != old.ID || j.AccountID != maintainerID || j.Spec.Device != "m1-pro" || j.Spec.Holder != compareHolder {
		t.Fatalf("the retry: %+v", j)
	}
	if cs.Before != "7.2" || cs.After != "kernel-aurora16" || cs.Tier != api.TierExtended || !cs.Hands || cs.MacOS || cs.Bench != "bench-0" ||
		cs.BeforeSet == nil || cs.BeforeSet.Name != "7.2" || cs.AfterSet == nil || cs.AfterSet.Name != "kernel-aurora16" {
		t.Errorf("the retry's Compare: %+v", cs)
	}
	if !cs.LeaseTaken || !l.s.holdsLease(l.s.dev("m1-pro"), *l.personOf(maintainerID)) {
		t.Errorf("the retry runs without a Lease of ravi's: taken %v", cs.LeaseTaken)
	}
	if !strings.Contains(rowText(ravi.page("/jobs/"+again)), "Compare 7.2 and kernel-aurora16") {
		t.Error("the retry's page is not the Compare's")
	}
}

// Compares lists every Compare for Maintainers, private and published,
// newest first, and is reached from the lab's row. Nobody else has it.
func TestCompares(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	ravi, grace := l.lrMaintainer(), l.lrMember(lrGrace)
	_, after := l.pcSets()
	now := l.s.now()
	l.pcCompare("j-private", now.Add(-time.Hour), api.CompareSpec{Before: api.CompareCurrent, After: "kernel-aurora16", AfterSet: after, Tier: api.TierDefault}, nil)
	l.pcCompare("j-published", now.Add(-2*time.Hour), api.CompareSpec{Before: api.CompareCurrent, After: "kernel-aurora16", AfterSet: after, Tier: api.TierExtended, MacOS: true}, func(j *api.Job) {
		published := now.Add(-30 * time.Minute)
		j.Result.Compare.Published, j.Result.Compare.PublishedBy = &published, "ravi"
	})
	l.pcCompare("j-running", now.Add(-10*time.Minute), api.CompareSpec{Before: api.CompareCurrent, After: "kernel-aurora16", AfterSet: after, Tier: api.TierDefault, Hands: true}, func(j *api.Job) {
		j.State, j.Outcome, j.Summary, j.Result.Compare = api.JobTesting, "", "", nil
		j.Events = j.Events[:1]
	})
	l.pjJob("j-boot", "m1-pro", maintainerID, "ravi", now.Add(-5*time.Minute), nil)

	page := ravi.page("/compares")
	if got := currentInLabNav(t, page); got != "Compares" {
		t.Errorf("Compares is under %q in the lab's row", got)
	}
	rows, order := pageRows(page)
	if strings.Join(order, " ") != "j-running j-private j-published" {
		t.Fatalf("Compares lists %v", order)
	}
	for id, wants := range map[string][]string{
		"j-private":   {"m1-pro", "current → kernel-aurora16", "default", "1 better, 1 worse, 1 not compared", "1 regressed", "private"},
		"j-published": {"m1-pro", "current → kernel-aurora16", "extended", "macOS", "published"},
		"j-running":   {"m1-pro", "current → kernel-aurora16", "default", "a person", "testing"},
	} {
		text := rowText(rows[id])
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("the row of %s lacks %q: %s", id, want, text)
			}
		}
		if !strings.Contains(rows[id], `href="/jobs/`+id+`"`) {
			t.Errorf("the row of %s does not link its job", id)
		}
	}
	if !strings.Contains(rows["j-published"], `href="/compares/j-published"`) || strings.Contains(rows["j-private"], `href="/compares/`) {
		t.Error("only a published Compare links its public page")
	}
	if w := grace.do("GET", "/compares", ""); w.Code != http.StatusNotFound {
		t.Errorf("an M team member on Compares: %d", w.Code)
	}
}
