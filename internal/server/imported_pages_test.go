package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/oldsite"
	"github.com/omacom/omarchy-maclab/internal/oldsite/oldsitetest"
)

// The parity gate's last question, asked of the import's fixture: the old
// site's reports, read from a dump file into an empty reports.db, are
// served like any others. The public pages and exports count the published
// ones, a report today's rules would refuse still has its page, the hidden
// ones are a Maintainer's to read, and an old deletion link still deletes.
func TestTheOldSitesReportsOnceImportedAreServedLikeAnyOthers(t *testing.T) {
	l := newIssuesLabWith(t, func(c *Config) { c.MachineIDKey = testMachineIDKey() })
	corpus := oldsitetest.Made(t, testMachineIDKey())
	dump, err := oldsite.OpenDump(corpus.Dump(t))
	if err != nil {
		t.Fatal(err)
	}
	carried, err := l.s.reports.Carry(t.Context(), dump)
	if err != nil {
		t.Fatal(err)
	}
	var published, hidden []string
	uploader := map[string]string{}
	for _, row := range corpus.Reports {
		if row.HiddenAt != nil {
			hidden = append(hidden, row.PublicID)
		} else {
			published = append(published, row.PublicID)
		}
		uploader[row.PublicID] = "nobody signed in"
		if row.TesterLogin != nil {
			uploader[row.PublicID] = "@" + *row.TesterLogin
		}
	}
	if carried.Reports != len(corpus.Reports) || carried.Hidden != len(hidden) || len(hidden) == 0 || carried.Verified == 0 {
		t.Fatalf("carried %+v of %d reports, %d hidden", carried, len(corpus.Reports), len(hidden))
	}

	// Every public page and export answers, and counts the published reports alone.
	for _, target := range []string{"/", "/matrix", "/matrix?group=build", "/models", "/features", "/gaps", "/aurora", "/performance", "/candidates", "/data",
		"/api/v1/reports.csv", "/api/v1/checks.csv", "/api/v1/matrix.json", "/api/v1/benchmarks.json", "/api/v1/aurora.json"} {
		if w := l.do("GET", target, ""); w.Code != http.StatusOK {
			t.Errorf("GET %s: %d", target, w.Code)
		}
	}
	if got := l.page("/reports").count(t, "tr.report-row"); got != len(published) {
		t.Errorf("the public list has %d rows, want the %d published", got, len(published))
	}
	export := l.do("GET", "/api/v1/reports.json", "").Body.String()
	for _, id := range published {
		if got := l.status("/reports/" + id); got != http.StatusOK {
			t.Errorf("the published report %s: %d", id, got)
		}
		if !strings.Contains(export, `"`+id+`"`) {
			t.Errorf("the published report %s is not exported", id)
		}
	}
	// A report with a check the catalogue has since lost is shown, check and all.
	if page := l.page("/reports/" + oldsitetest.UnknownCheck); page.count(t, `li[id="check-`+oldsitetest.GoneCheck+`"]`) != 1 {
		t.Error("a report today's rules would refuse does not show its check")
	}
	for _, id := range hidden {
		if got := l.status("/reports/" + id); got != http.StatusNotFound {
			t.Errorf("the hidden report %s, signed out: %d", id, got)
		}
		if strings.Contains(export, id) {
			t.Errorf("the hidden report %s is exported", id)
		}
	}

	// A Maintainer lists them all, hidden ones too, each with who uploaded it.
	list := l.seen("/reports")
	if got := list.count(t, "tr.admin-row"); got != len(corpus.Reports) {
		t.Errorf("the Maintainer's list has %d rows, want %d", got, len(corpus.Reports))
	}
	if got := list.count(t, `tr.admin-row[data-state="hidden"]`); got != len(hidden) {
		t.Errorf("the Maintainer's list has %d hidden rows, want %d", got, len(hidden))
	}
	named := 0
	for id, want := range uploader {
		if got := list.sole(t, `tr[id="admin-report-`+id+`"] .uploader`).words(); got != want {
			t.Errorf("who uploaded %s: %q, want %q", id, got, want)
		}
		if want != "nobody signed in" {
			named++
		}
	}
	if named == 0 {
		t.Fatal("the fixture has no report with a handle")
	}
	for _, id := range hidden {
		if !l.seen("/reports/"+id).hasText(t, "#hidden", "hidden by a Maintainer") {
			t.Errorf("the Maintainer's view of the hidden report %s", id)
		}
	}
	l.maintainer.did("/reports/" + hidden[0] + "/show")
	if got := l.status("/reports/" + hidden[0]); got != http.StatusOK {
		t.Errorf("an imported report shown again: %d", got)
	}

	// The old site's deletion link works with only the host changed.
	link := "/reports/" + oldsitetest.Verified + "/deletion?token=" + oldsitetest.VerifiedToken
	if got := l.status(link); got != http.StatusOK {
		t.Fatalf("the old deletion link: %d", got)
	}
	w := l.do("POST", "/reports/"+oldsitetest.Verified, "_method=delete&token="+oldsitetest.VerifiedToken, "Content-Type", "application/x-www-form-urlencoded", "Origin", labAddress)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Report deleted") || l.reportState(oldsitetest.Verified) != "" {
		t.Errorf("the old deletion link, confirmed: %d", w.Code)
	}
}
