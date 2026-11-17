package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The pinned elements of each public page, on the seeded corpus, against the
// old site's own pages: every element inside <main> with an id or a data-
// attribute, as tag, id, classes and data- attributes, in the page's order.
// site/test/integration/page_parity_test.rb writes the old site's
// (PAGE_PARITY_DIR/rails-pages.json) from the corpus internal/reports'
// TestTheCountsAreTheOldSites wrote there. Skipped without that folder: the
// old site runs in CI, not here.

type pinnedElement struct {
	Tag   string            `json:"tag"`
	ID    string            `json:"id"`
	Class []string          `json:"class"`
	Data  map[string]string `json:"data"`
}

// portOnly are the data- attributes the port marks and the old site never
// had: data-copy is the value a Copy button beside it copies. An element
// pinned by one of them alone is not pinned; one with an id still is.
var portOnly = map[string]bool{"data-copy": true}

func pinnedIn(main *node) []pinnedElement {
	var list []pinnedElement
	var walk func(*node)
	walk = func(at *node) {
		for _, c := range at.children {
			if c.tag == "" {
				continue
			}
			e := pinnedElement{Tag: c.tag, ID: c.attrs["id"], Class: c.classes(), Data: map[string]string{}}
			sort.Strings(e.Class)
			if e.Class == nil {
				e.Class = []string{}
			}
			for name, value := range c.attrs {
				if strings.HasPrefix(name, "data-") && !portOnly[name] {
					e.Data[name] = value
				}
			}
			if e.ID != "" || len(e.Data) > 0 {
				list = append(list, e)
			}
			walk(c)
		}
	}
	walk(main)
	return list
}

// parityPages are the pages page_parity_test.rb asks the old site for.
var parityPages = []string{"/", "/matrix", "/matrix?group=build", "/matrix?group=release", "/matrix?group=model&confirmed=1", "/matrix?group=build&state=works&layer=aurora",
	"/models", "/models/j416c", "/models/j613", "/features", "/features?model=j416c", "/features/gpu", "/features/brightness",
	"/reports", "/reports?gen=m1&confirmed=1", "/reports?runs=tester&set=edge-demo", "/reports/preview-0000000000000016", "/reports/preview-0000000000000019",
	"/gaps", "/aurora", "/benchmarks", "/candidates", "/candidates/edge-demo", "/candidates/edge-passing", "/candidates/edge-waiting"}

// parityExports are the exports page_parity_test.rb asks the old site for.
var parityExports = []string{"/api/v1/reports.json?model=j613", "/api/v1/reports.json?set=edge-demo", "/api/v1/reports.csv", "/api/v1/reports.csv?confirmed=1&runs=tester",
	"/api/v1/checks.csv", "/api/v1/checks.csv?model=j416c&state=regression", "/api/v1/checks.csv?confirmed=1&layer=aurora&state=gap",
	"/api/v1/matrix.json", "/api/v1/matrix.json?group=release", "/api/v1/matrix.json?group=model", "/api/v1/matrix.json?confirmed=1&gen=m2&group=model",
	"/api/v1/matrix.json?layer=asahi&state=works", "/api/v1/benchmarks.json", "/api/v1/aurora.json"}

// parityLab is a lab that holds the seeded corpus internal/reports'
// TestTheCountsAreTheOldSites wrote to dir.
func parityLab(t *testing.T, dir string) *publicLab {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus []struct {
		PublicID  string `json:"public_id"`
		CreatedAt string `json:"created_at"`
		MachineID string `json:"machine_id"`
		Verified  bool   `json:"verified"`
		Body      doc    `json:"body"`
	}
	if err := json.Unmarshal(data, &corpus); err != nil || len(corpus) < 10 {
		t.Fatalf("the corpus: %d reports, %v", len(corpus), err)
	}
	l := newPublicLab(t, nil)
	for _, entry := range corpus {
		at, err := time.Parse(time.RFC3339, entry.CreatedAt)
		if err != nil {
			t.Fatal(err)
		}
		l.store(entry.PublicID, entry.Body, entry.MachineID, at, false)
		if entry.Verified {
			l.exec(`UPDATE reports SET verified = 1, handle = 'demo-tester', account_id = 4242 WHERE public_id = ?`, entry.PublicID)
		}
	}
	return l
}

var reMade = regexp.MustCompile(`"generated_at":"[^"]*"`)

// Each export, on the seeded corpus, against the old site's own answer
// (PAGE_PARITY_DIR/rails-exports.json): byte for byte but for the time it
// was made and the site's address; reports.json as data, since Postgres
// kept a report's members in its own order. Skipped without that folder.
func TestTheExportsAreTheOldSites(t *testing.T) {
	dir := os.Getenv("PAGE_PARITY_DIR")
	if dir == "" {
		t.Skip("PAGE_PARITY_DIR is not set")
	}
	l := parityLab(t, dir)
	export := func(target string) string {
		return strings.ReplaceAll(reMade.ReplaceAllString(l.export(target), `"generated_at":"-"`), labAddress, "http://www.example.com")
	}
	ours := map[string]string{}
	for _, target := range parityExports {
		ours[target] = export(target)
	}
	if text, err := json.MarshalIndent(ours, "", "  "); err == nil {
		os.WriteFile(filepath.Join(dir, "go-exports.json"), text, 0o644)
	}
	old := map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, "rails-exports.json"))
	if err == nil {
		err = json.Unmarshal(data, &old)
	}
	if err != nil || len(old) < 10 {
		t.Fatalf("the old site's exports: %d, %v", len(old), err)
	}
	for target, want := range old {
		want = reMade.ReplaceAllString(want, `"generated_at":"-"`)
		got, asked := ours[target]
		if !asked {
			got = export(target)
		}
		if strings.HasPrefix(target, "/api/v1/reports.json") {
			var a, b any
			if json.Unmarshal([]byte(want), &a) != nil || json.Unmarshal([]byte(got), &b) != nil || !reflect.DeepEqual(a, b) {
				t.Errorf("%s: the port's export is not the old site's", target)
			}
			continue
		}
		if got != want {
			at := 0
			for at < len(got) && at < len(want) && got[at] == want[at] {
				at++
			}
			t.Errorf("%s differs at byte %d:\n  the old site …%s\n  the port     …%s", target, at, want[max(0, at-60):min(len(want), at+80)], got[max(0, at-60):min(len(got), at+80)])
		}
	}
}

func TestThePagesPinWhatTheOldSitesPin(t *testing.T) {
	dir := os.Getenv("PAGE_PARITY_DIR")
	if dir == "" {
		t.Skip("PAGE_PARITY_DIR is not set")
	}
	l := parityLab(t, dir)
	old := map[string][]pinnedElement{}
	data, err := os.ReadFile(filepath.Join(dir, "rails-pages.json"))
	if err == nil {
		err = json.Unmarshal(data, &old)
	}
	// The port's own, beside the old site's, to look at when they differ.
	ours := map[string][]pinnedElement{}
	targets := parityPages
	if len(old) > 0 {
		targets = nil
		for target := range old {
			targets = append(targets, target)
		}
	}
	for _, target := range targets {
		ours[target] = pinnedIn(l.page(target).sole(t, "main"))
	}
	if text, err := json.MarshalIndent(ours, "", "  "); err == nil {
		os.WriteFile(filepath.Join(dir, "go-pages.json"), text, 0o644)
	}
	if err != nil || len(old) < 10 {
		t.Fatalf("the old site's pages: %d, %v", len(old), err)
	}
	for target, want := range old {
		got, kept := ours[target], want
		if len(kept) == 0 {
			t.Errorf("%s: the old page pins nothing", target)
		}
		differ := 0
		for i := 0; i < len(kept) || i < len(got); i++ {
			var a, b string
			if i < len(kept) {
				a = fmt.Sprintf("%+v", kept[i])
			}
			if i < len(got) {
				b = fmt.Sprintf("%+v", got[i])
			}
			if (i >= len(kept) || i >= len(got) || !reflect.DeepEqual(kept[i], got[i])) && differ < 5 {
				differ++
				t.Errorf("%s, pinned element %d:\n  the old site %s\n  the port     %s", target, i, a, b)
			}
		}
	}
}

// A Copy button's target is the port's own mark: the parity of pinned
// elements reads past it, and still reads every other id and data- attribute.
func TestACopyMarkIsNotAPinnedElement(t *testing.T) {
	main := parseHTML(t, `<main><div class="copyable copyable-block"><pre class="command"><code data-copy id="install">curl</code></pre> <button class="copy-button" type="button" aria-label="Copy the install command" hidden>Copy</button></div>`+
		`<span class="dim">report <span class="copyable"><code data-copy>preview-1</code></span></span>`+
		`<tr data-build="b1" data-copy><td data-label="sha256">x</td></tr></main>`).children[0]
	got, _ := json.Marshal(pinnedIn(main))
	want := `[{"tag":"code","id":"install","class":[],"data":{}},{"tag":"tr","id":"","class":[],"data":{"data-build":"b1"}},{"tag":"td","id":"","class":[],"data":{"data-label":"sha256"}}]`
	if string(got) != want {
		t.Errorf("pinned:\n%s\nwant:\n%s", got, want)
	}
}
