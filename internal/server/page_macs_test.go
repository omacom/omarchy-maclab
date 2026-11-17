package server

import (
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func (b *browser) macsPage(query string) string {
	b.l.t.Helper()
	w := b.do("GET", "/macs"+query, "")
	if w.Code != http.StatusOK {
		b.l.t.Fatalf("GET /macs%s: %d", query, w.Code)
	}
	return w.Body.String()
}

var reMacRow = regexp.MustCompile(`(?s)<tr id="mac-([a-z0-9-]+)">(.*?)</tr>`)

// macRows are the page's rows by Mac, tags taken out, and their order.
func macRows(page string) (rows map[string]string, order []string) {
	rows = map[string]string{}
	for _, m := range reMacRow.FindAllStringSubmatch(page, -1) {
		rows[m[1]] = m[2]
		order = append(order, m[1])
	}
	return rows, order
}

// reHiddenButton is a button the page renders hidden for its script to show,
// such as Copy: no reader sees it before then.
var reHiddenButton = regexp.MustCompile(`(?s)<button [^>]*\bhidden>.*?</button>`)

func rowText(row string) string {
	row = reHiddenButton.ReplaceAllString(row, "")
	return html.UnescapeString(strings.Join(strings.Fields(reTag.ReplaceAllString(row, " ")), " "))
}

func buttons(row string) string {
	row = reHiddenButton.ReplaceAllString(row, "")
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<button[^>]*>(.*?)</button>`).FindAllStringSubmatch(row, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return strings.Join(out, ", ")
}

// Macs is the M team's and the Maintainers' page: the fleet, each Mac's
// Configuration and who holds what.
func TestMacsShowsTheFleetAndWhoHoldsWhat(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-max", "j414c", nil)
	l.lrMac("vol-air", "j313", lrVolunteers(api.AccessQualification, l.s.now().Add(2*time.Hour)))
	l.lrMac("gone", "j274", func(d *api.Device) { d.State = api.StateLeft })
	grace, linus, ravi := l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()

	if w := l.browser().do("GET", "/macs", ""); w.Code != http.StatusSeeOther {
		t.Errorf("signed out: %d", w.Code)
	}
	if w := l.signedIn(nobodyID).do("GET", "/macs", ""); w.Code != http.StatusNotFound {
		t.Errorf("signed in with no role: %d", w.Code)
	}

	lease := grace.mustLease("m1-pro", "grace@laptop", 0)
	rows, order := macRows(linus.macsPage(""))
	if got := strings.Join(order, " "); got != "m1-pro m2-max vol-air" {
		t.Fatalf("the fleet, by name, without the Mac that left: %q", got)
	}
	pro := rowText(rows["m1-pro"])
	for _, want := range []string{"Lab-owned", "j316s, 10 CPU and 16 GPU cores", "@grace", "M team", "until " + when(lease.Expires)} {
		if !strings.Contains(pro, want) {
			t.Errorf("m1-pro's row lacks %q: %s", want, pro)
		}
	}
	if got := buttons(rows["m1-pro"]); got != "" {
		t.Errorf("another member's buttons on a Mac grace holds: %q", got)
	}
	if got := buttons(rows["m2-max"]); got != "Lease" {
		t.Errorf("a member's buttons on a free Mac: %q", got)
	}
	if air := rowText(rows["vol-air"]); !strings.Contains(air, "Volunteer") || !strings.Contains(air, "j313, core counts unknown") || buttons(rows["vol-air"]) != "Lease" {
		t.Errorf("a Volunteer's Mac at Qualification, inside its window: %s", air)
	}
	if strings.Contains(linus.macsPage(""), `name="cpu"`) {
		t.Error("an M team member is shown the control that sets a Configuration")
	}

	// The holder's own row: Renew and Release, and no Lease.
	rows, _ = macRows(grace.macsPage(""))
	if got := buttons(rows["m1-pro"]); got != "Renew, Release" {
		t.Errorf("the holder's buttons: %q", got)
	}
	// A Maintainer sees the same, with the Configuration control.
	page := ravi.macsPage("")
	rows, _ = macRows(page)
	if !strings.Contains(rows["m2-max"], `name="cpu"`) || !strings.Contains(rows["m2-max"], `name="gpu"`) {
		t.Errorf("a Maintainer's control to set a Configuration: %s", rows["m2-max"])
	}
	if got := buttons(rows["m1-pro"]); strings.Contains(got, "Lease") || strings.Contains(got, "Renew") || strings.Contains(got, "Release") {
		t.Errorf("a Maintainer's buttons on a Mac grace holds: %q", got)
	}
	if !strings.Contains(part(t, page, `<nav class="lab-nav"`, "</nav>"), `aria-current="page"`) {
		t.Error("Macs is not the current page of the lab's row")
	}
}

// Lease, Renew and Release are buttons on Macs.
func TestMacsLeasesRenewsAndReleases(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-mini", "j473", nil)
	grace, ravi := l.lrMember(lrGrace), l.lrMaintainer()

	if w := grace.form("/macs/m1-pro/lease"); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/macs" {
		t.Fatalf("Lease: %d to %q %s", w.Code, w.Header().Get("Location"), w.Body)
	}
	q := l.leaseOn("m1-pro")
	if q == nil || q.AccountID != lrGrace || q.Maintainer != "grace" || q.Holder != "grace" || !q.Expires.Equal(l.s.now().Add(4*time.Hour)) {
		t.Fatalf("the Lease taken on the site: %+v", q)
	}
	l.clock.Advance(time.Hour)
	if w := grace.form("/macs/m1-pro/renew"); w.Code != http.StatusSeeOther {
		t.Fatalf("Renew: %d %s", w.Code, w.Body)
	}
	if q := l.leaseOn("m1-pro"); !q.Expires.Equal(l.s.now().Add(4 * time.Hour)) {
		t.Fatalf("renewed, it ends %s, want 4 hours from now", q.Expires)
	}
	// Renew and Release are the person's, whatever label their lab gave.
	grace.mustLease("m2-mini", "grace@laptop", 0)
	l.clock.Advance(time.Hour)
	if w := grace.form("/macs/m2-mini/renew"); w.Code != http.StatusSeeOther {
		t.Fatalf("Renew of a Lease lab took: %d %s", w.Code, w.Body)
	}
	if q := l.leaseOn("m2-mini"); q.Holder != "grace@laptop" || !q.Expires.Equal(l.s.now().Add(4*time.Hour)) {
		t.Fatalf("renewed on the site, a Lease lab took: %+v", q)
	}

	// Release takes something away, so it asks twice.
	w := grace.form("/macs/m1-pro/release")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Release: your Lease on m1-pro ends now") || l.leaseOn("m1-pro") == nil {
		t.Fatalf("the first Release: %d, Lease %+v", w.Code, l.leaseOn("m1-pro"))
	}
	if w := grace.form("/macs/m1-pro/release", "sure", "yes"); w.Code != http.StatusSeeOther || l.leaseOn("m1-pro") != nil {
		t.Fatalf("the second Release: %d, Lease %+v", w.Code, l.leaseOn("m1-pro"))
	}

	// A refusal is said on the page, and nothing changes.
	if w := ravi.form("/macs/m2-mini/lease"); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `role="alert"`) || !strings.Contains(w.Body.String(), "m2-mini is leased by grace@laptop (grace) until") {
		t.Fatalf("a Lease on a Mac someone holds: %d %s", w.Code, w.Body)
	}
	if w := ravi.form("/macs/m2-mini/release", "sure", "yes"); w.Code != http.StatusConflict || l.leaseOn("m2-mini") == nil {
		t.Fatalf("releasing someone else's Lease from Macs: %d", w.Code)
	}
	if w := ravi.form("/macs/m2-mini/renew"); w.Code != http.StatusConflict {
		t.Fatalf("renewing someone else's Lease: %d", w.Code)
	}
	// A Maintainer picks the hours, up to 24; an M team member's are 4.
	if w := ravi.form("/macs/m1-pro/lease", "hours", "24"); w.Code != http.StatusSeeOther {
		t.Fatalf("a Maintainer's Lease of 24 hours: %d %s", w.Code, w.Body)
	}
	if q := l.leaseOn("m1-pro"); q == nil || !q.Expires.Equal(l.s.now().Add(24*time.Hour)) {
		t.Fatalf("a Maintainer's Lease of 24 hours: %+v", q)
	}
	ravi.form("/macs/m1-pro/release", "sure", "yes")
	if w := grace.form("/macs/m1-pro/lease", "hours", "24"); w.Code != http.StatusBadRequest || l.leaseOn("m1-pro") != nil {
		t.Fatalf("an M team member's Lease of 24 hours: %d", w.Code)
	}
	if w := l.signedIn(nobodyID).form("/macs/m1-pro/lease"); w.Code != http.StatusNotFound {
		t.Fatalf("a Lease from Macs by someone with no role: %d", w.Code)
	}
	// A write that is not the lab's own page's is refused.
	if w := grace.do("POST", "/macs/m1-pro/lease", ""); w.Code != http.StatusForbidden || l.leaseOn("m1-pro") != nil {
		t.Fatalf("a Lease without the page's token: %d", w.Code)
	}
}

// The filters: All, Mine, Leased by me.
func TestMacsFiltersAllMineAndLeasedByMe(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("m1-pro", "j316s", nil)
	l.lrMac("m2-mini", "j473", nil)
	l.lrMac("graces-air", "j313", lrVolunteers(api.AccessFull, l.s.now().Add(2*time.Hour)))
	l.lrLend("graces-air", lrGrace)
	grace, linus := l.lrMember(lrGrace), l.lrMember(lrLinus)
	grace.mustLease("m2-mini", "grace@laptop", 0)
	linus.mustLease("m1-pro", "linus@desk", 0)

	page := grace.macsPage("")
	chips := part(t, page, `<nav class="chips"`, "</nav>")
	if got := linkLabels(chips); got != "All, Mine, Leased by me" {
		t.Fatalf("the filters: %q", got)
	}
	for query, want := range map[string]string{
		"":             "graces-air m1-pro m2-mini",
		"?show=all":    "graces-air m1-pro m2-mini",
		"?show=leased": "m2-mini",
		"?show=other":  "graces-air m1-pro m2-mini",
	} {
		_, order := macRows(grace.macsPage(query))
		if got := strings.Join(order, " "); got != want {
			t.Errorf("/macs%s for grace: %q, want %q", query, got, want)
		}
	}
	// Mine is the cards of My Macs, in place of the table.
	if got := ids(mainOf(t, grace.macsPage("?show=mine")).all(t, "section.mac-card")); got != "mac-graces-air" {
		t.Errorf("/macs?show=mine for grace: %q", got)
	}
	if rows, _ := macRows(linus.macsPage("")); !strings.Contains(rowText(rows["graces-air"]), "Volunteer's of @grace") {
		t.Errorf("an M team member is not shown whose Mac it is: %s", rowText(rows["graces-air"]))
	}
	if _, order := macRows(linus.macsPage("?show=leased")); strings.Join(order, " ") != "m1-pro" {
		t.Errorf("Leased by me, for linus: %v", order)
	}
	if page := linus.macsPage("?show=mine"); !strings.Contains(page, "You lend no Mac.") {
		t.Errorf("Mine, for someone who lends none, does not say so")
	}
	if !strings.Contains(part(t, grace.macsPage("?show=leased"), `<nav class="chips"`, "</nav>"), `class="chip current" href="/macs?show=leased"`) {
		t.Error("the filter in use is not marked")
	}
	// A button pressed under a filter comes back to it.
	if w := grace.form("/macs/m2-mini/renew", "show", "leased"); w.Header().Get("Location") != "/macs?show=leased" {
		t.Errorf("after Renew under Leased by me: %q", w.Header().Get("Location"))
	}
}

// A row that takes no Lease says why.
func TestAMacsRowThatTakesNoLeaseSaysWhy(t *testing.T) {
	l := leaseLab(t)
	l.lrMac("new-mac", "j274", func(d *api.Device) { d.KnownGood = "" })
	l.lrMac("vol-closed", "j293", lrVolunteers(api.AccessQualification, time.Time{}))
	l.lrMac("pro-a", "j316s", nil)
	l.lrMac("pro-b", "j316s", nil)
	l.lrMac("max-a", "j414c", nil)
	l.lrMac("max-b", "j414c", nil)
	l.lrMac("free", "j473", nil)
	grace, linus, ravi := l.lrMember(lrGrace), l.lrMember(lrLinus), l.lrMaintainer()
	grace.mustLease("pro-a", "grace@laptop", 0)
	grace.mustLease("max-a", "grace@laptop", 0)
	lease := linus.mustLease("free", "linus@desk", 0)

	rows, _ := macRows(grace.macsPage(""))
	for mac, why := range map[string]string{
		"new-mac":    "has not passed its baseline",
		"vol-closed": "outside its Availability window",
		"pro-b":      "you hold pro-a, the same Configuration",
		"max-b":      "you hold max-a, and its core counts are unknown",
		"free":       "leased by @linus",
	} {
		if text := rowText(rows[mac]); !strings.Contains(text, why) || buttons(rows[mac]) != "" {
			t.Errorf("%s: buttons %q, and the row does not say %q: %s", mac, buttons(rows[mac]), why, text)
		}
	}
	if text := rowText(rows["free"]); !strings.Contains(text, "until "+when(lease.Expires)) {
		t.Errorf("a leased Mac's row does not say until when: %s", text)
	}

	// A Maintainer is warned on the row and may go on.
	ravi.mustLease("pro-b", "ravi@mini", 0)
	l.lrMac("pro-c", "j316s", nil)
	rows, _ = macRows(ravi.macsPage(""))
	if text := rowText(rows["pro-c"]); buttons(rows["pro-c"]) != "Set, Lease" || !strings.Contains(text, "you hold pro-b, the same Configuration") {
		t.Errorf("a Maintainer's row for a Configuration they hold: buttons %q: %s", buttons(rows["pro-c"]), text)
	}
	if text := rowText(rows["new-mac"]); !strings.Contains(text, "has not passed its baseline") || buttons(rows["new-mac"]) != "Set" {
		t.Errorf("a Maintainer's row for a Mac with no baseline: %s", text)
	}
}
