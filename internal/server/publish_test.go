package server

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/reports"
)

// The lab publishing a job's report (spec, section 9). The jobs here are
// left as the runner leaves them, with the report omarchy-m-test signed on
// the Mac among their files; publish_run_test.go has the runner itself.

// publishLab is a lab that takes reports and publishes by itself.
func publishLab(t *testing.T, config func(*Config)) *signInLab {
	t.Helper()
	return macLab(t, func(c *Config) {
		c.OMT, c.OMTPublish = true, true
		if config != nil {
			config(c)
		}
	})
}

var labRuns atomic.Int64

var ranJobs int

// labReport is the report omarchy-m-test leaves after a run on kernel,
// signed by the Mac's Machine key. Each has evidence of its own, so no two
// are the same report; change edits it before it is signed.
func labReport(t testing.TB, mac, kernel string, change func(map[string]any)) []byte {
	t.Helper()
	golden, err := os.ReadFile(filepath.Join("..", "..", "schema", "golden", "m2-max-image2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(golden, &body); err != nil {
		t.Fatal(err)
	}
	body["machine"].(map[string]any)["kernel"] = kernel
	first := body["checks"].([]any)[0].(map[string]any)
	first["evidence"] = append(first["evidence"].([]any), fmt.Sprintf("run: %d", labRuns.Add(1)))
	if change != nil {
		change(body)
	}
	text, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(macSigned(t, mac, reports.NamespaceReport, string(text)))
}

// check is the check of a report's document with that id.
func check(body map[string]any, id string) map[string]any {
	for _, c := range body["checks"].([]any) {
		if c := c.(map[string]any); c["id"] == id {
			return c
		}
	}
	return nil
}

// machineIDOf is the machine id the lab gives the test Mac of that name.
func machineIDOf(t testing.TB, mac string) string {
	t.Helper()
	rules, err := reports.NewRules(testMachineIDKey())
	if err != nil {
		t.Fatal(err)
	}
	blob := sshPacked([]byte("ssh-ed25519"), macKey(mac).Public().(ed25519.PublicKey))
	id, err := rules.MachineIDOf("ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// ran leaves a finished Known-good run on mac, asked for by account (0: the
// lab itself), with report among its files; nil is a fresh good one.
func (l *signInLab) ran(id, mac string, account int64, report []byte, change func(*api.Job)) *api.Job {
	l.t.Helper()
	handle := ""
	if account != 0 {
		handle = l.who(account).Handle
	}
	if report == nil {
		report = labReport(l.t, mac, lrKnownGood, nil)
	}
	// Each a second after the one before, so a list of them has an order.
	ranJobs++
	j := l.pjJob(id, mac, account, handle, l.s.now().Add(-10*time.Minute+time.Duration(ranJobs)*time.Second), func(j *api.Job) {
		j.Result.Booted, j.Result.BootKernel = true, lrKnownGood
		j.Result.OMT = &api.OMTResult{Report: omtFile, Tool: "0.2.0", Kernel: lrKnownGood, KnownGood: true, Pass: 40, Fail: 2, Skip: 3}
		if change != nil {
			change(j)
		}
	})
	path := filepath.Join(l.s.jobDir(id), omtFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(path, report, 0o644); err != nil {
		l.t.Fatal(err)
	}
	return j
}

// publish is `lab publish <job>` as the person.
func (b *browser) publish(job string) (status int, said string) {
	b.l.t.Helper()
	w := b.write("POST", "/api/jobs/"+job+"/publish", "")
	return w.Code, strings.TrimSpace(w.Body.String())
}

var reReportLink = regexp.MustCompile(`^` + regexp.QuoteMeta(labAddress) + `/reports/([1-9A-HJ-NP-Za-km-z]{24})$`)

// published is publish for a report that must go up: its public id.
func (b *browser) published(job string) string {
	b.l.t.Helper()
	status, said := b.publish(job)
	var out struct {
		ReportURL string `json:"report_url"`
	}
	json.Unmarshal([]byte(said), &out)
	m := reReportLink.FindStringSubmatch(out.ReportURL)
	if status != http.StatusOK || m == nil {
		b.l.t.Fatalf("publish %s: %d %s", job, status, said)
	}
	return m[1]
}

// notPublished is publish for a report the lab must refuse, saying each of
// these.
func (b *browser) notPublished(job string, status int, saying ...string) {
	b.l.t.Helper()
	got, said := b.publish(job)
	if got != status {
		b.l.t.Errorf("publish %s: %d %s, want %d", job, got, said, status)
	}
	for _, want := range saying {
		if !strings.Contains(said, want) {
			b.l.t.Errorf("the refusal to publish %s lacks %q: %s", job, want, said)
		}
	}
}

// jobSaid is the job as `lab job` reads it.
func (b *browser) jobSaid(id string) *api.Job {
	b.l.t.Helper()
	w := b.do("GET", "/api/jobs/"+id, "")
	var j api.Job
	if err := json.Unmarshal(w.Body.Bytes(), &j); w.Code != http.StatusOK || err != nil {
		b.l.t.Fatalf("job %s: %d %s", id, w.Code, w.Body)
	}
	return &j
}

func (l *signInLab) everyReport() []*reports.Run {
	l.t.Helper()
	every, err := l.s.reports.Every(l.s.catalogue)
	if err != nil {
		l.t.Fatal(err)
	}
	return every.Runs()
}

func (l *signInLab) report(id string) *reports.Report {
	l.t.Helper()
	report, err := l.s.reports.Report(id)
	if err != nil {
		l.t.Fatalf("report %s: %v", id, err)
	}
	return report
}

// A lab run's report passes schema, signature, check ids and evidence inside
// the application, and no request from outside reaches that path.
func TestALabRunsReportPassesTheSameChecksInsideLabd(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()

	unsigned := labReport(t, "m2-max", lrKnownGood, nil)
	unsigned = append(unsigned[:strings.Index(string(unsigned), `,"signature":`)], '}')
	forged := []byte(strings.Replace(string(labReport(t, "m2-max", lrKnownGood, nil)), `"board":"j416c"`, `"board":"j416s"`, 1))
	for name, c := range map[string]struct {
		report []byte
		saying string
	}{
		"unsigned":  {unsigned, "signature"},
		"forged":    {forged, "signature"},
		"schema":    {labReport(t, "m2-max", lrKnownGood, func(b map[string]any) { b["machine"].(map[string]any)["arch"] = "x86_64" }), "schema"},
		"check-ids": {labReport(t, "m2-max", lrKnownGood, func(b map[string]any) { check(b, "boot.files")["id"] = "boot.made-up" }), "boot.made-up"},
		"evidence": {labReport(t, "m2-max", lrKnownGood, func(b map[string]any) {
			check(b, "boot.files")["evidence"] = []any{strings.Repeat("x", 70<<10)}
		}), "vidence"},
	} {
		id := "j-" + name
		l.ran(id, "m2-max", maintainerID, c.report, nil)
		// The lab's own answer is the one the upload gives the same file.
		outside := l.fromMac("/api/v1/reports", string(c.report), macAddress)
		if outside.status != http.StatusUnprocessableEntity {
			t.Fatalf("%s, uploaded: %d %s", name, outside.status, outside.raw)
		}
		ravi.notPublished(id, http.StatusConflict, "not published", outside.body["error"].(string))
		if said := ravi.jobSaid(id); said.Result.OMT.ReportState != api.ReportNotPublished || !strings.Contains(strings.ToLower(said.Result.OMT.PublishNote), strings.ToLower(c.saying)) {
			t.Errorf("%s: the job says %q: %q", name, said.Result.OMT.ReportState, said.Result.OMT.PublishNote)
		}
	}
	if n := len(l.everyReport()); n != 0 {
		t.Fatalf("%d refused reports were stored", n)
	}

	l.ran("j-good", "m2-max", maintainerID, nil, nil)
	id := ravi.published("j-good")
	if report := l.report(id); !report.ByLab() || report.LabJob != "j-good" {
		t.Errorf("the lab's report: %+v", report)
	}

	// An upload claiming to be the lab's is anyone's.
	a := l.fromMac("/api/v1/reports?lab_job=j-good&job=j-good&lab=1", string(labReport(t, "m2-max", lrKnownGood, nil)), macAddress)
	if a.status != http.StatusCreated {
		t.Fatalf("an upload from outside: %d %s", a.status, a.raw)
	}
	if report := l.storedReport(a); report.ByLab() || report.AskedHandle != "" {
		t.Errorf("an upload from outside was recorded as the lab's: %+v", report)
	}

	// The way in from inside is called from one place, the lab's own publishing.
	var callers []string
	root := filepath.Join("..", "..")
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			if d != nil && d.IsDir() && (d.Name() == "site" || d.Name() == ".git" || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if text, _ := os.ReadFile(path); strings.Contains(string(text), "ReceiveFromLab(") {
			rel, _ := filepath.Rel(root, path)
			callers = append(callers, filepath.ToSlash(rel))
		}
		return nil
	})
	if got := strings.Join(callers, " "); got != "internal/reports/upload.go internal/server/omt.go" {
		t.Errorf("the way in from inside labd is named in %q", got)
	}
}

// The one signature is the Mac's Machine key, and the machine id it gives is
// recorded on the Mac's lab record.
func TestTheOneSignatureIsTheMachineKeyAndTheMachineIDIsOnTheLabRecord(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	l.lrMac("m1-pro", "j316s", nil)
	ravi := l.lrMaintainer()
	want := machineIDOf(t, "m2-max")

	if got := l.s.dev("m2-max").snapshot().MachineID; got != "" {
		t.Fatalf("a Mac the lab published nothing of has the machine id %q", got)
	}
	sent := labReport(t, "m2-max", lrKnownGood, nil)
	l.ran("j1", "m2-max", 0, sent, nil)
	report := l.report(ravi.published("j1"))
	if report.MachineID != want {
		t.Errorf("the report's machine id is %q, want the one of the Mac's Machine key", report.MachineID)
	}
	if strings.Contains(string(report.Body), "signature") || !strings.Contains(string(sent), strings.TrimSuffix(strings.TrimSpace(string(report.Body)), "}")) {
		t.Error("what is stored is not the text the Mac signed, with nothing added")
	}
	if got := l.s.dev("m2-max").snapshot().MachineID; got != want {
		t.Errorf("the Mac's lab record has the machine id %q, want %q", got, want)
	}
	if got := l.s.dev("m1-pro").snapshot().MachineID; got != "" {
		t.Errorf("another Mac got the machine id %q", got)
	}
	// It is kept, and shown to nobody.
	stored, err := l.s.store.devices()
	if err != nil {
		t.Fatal(err)
	}
	kept := ""
	for _, d := range stored {
		if d.d.Name == "m2-max" {
			kept = d.d.MachineID
		}
	}
	if kept != want {
		t.Errorf("the stored record has the machine id %q", kept)
	}
	for _, target := range []string{"/api/devices/m2-max", "/api/devices", "/macs/m2-max", "/macs", "/jobs/j1", "/api/jobs/j1"} {
		if w := ravi.do("GET", target, ""); w.Code != http.StatusOK || strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), "machine_id") {
			t.Errorf("%s: %d, and it shows the machine id: %v", target, w.Code, strings.Contains(w.Body.String(), want))
		}
	}
	// A job of an earlier enrollment under the Mac's name is not this Mac's:
	// nothing of it is published, and it gives the record no machine id.
	l.ran("j-earlier", "m1-pro", 0, labReport(t, "another-mac", lrKnownGood, nil), func(j *api.Job) { j.DeviceID = "d-earlier" })
	ravi.notPublished("j-earlier", http.StatusConflict, "m1-pro has joined again since job j-earlier ran", "stays private")
	if got := l.s.dev("m1-pro").snapshot().MachineID; got != "" || len(l.everyReport()) != 1 {
		t.Errorf("a job of an earlier enrollment set the machine id %q", got)
	}
	// Nor is anything of a Mac that has left.
	l.lrMac("gone", "j274", func(d *api.Device) { d.State = api.StateLeft })
	l.ran("j-gone", "gone", 0, nil, nil)
	ravi.notPublished("j-gone", http.StatusConflict, "gone has left the lab")
}

// Only a Known-good run on a packaged kernel is published: an Installed run
// and a test-kernel run never.
func TestOnlyAKnownGoodRunOnAPackagedKernelIsPublished(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	ownership := func(status string) func(map[string]any) {
		return func(b map[string]any) { check(b, "boot.kernel-package")["status"] = status }
	}
	sha := strings.Repeat("ab", 32)
	for name, c := range map[string]struct {
		report []byte
		job    func(*api.Job)
		saying string
	}{
		"test-kernel": {labReport(t, "m2-max", "7.2.0-lab1", nil), func(j *api.Job) {
			j.Spec.Kernel, j.Result.BootKernel, j.Result.OMT.Kernel, j.Result.OMT.KnownGood = sha, "7.2.0-lab1", "7.2.0-lab1", false
		}, "only a Known-good run is published"},
		"built-kernel": {labReport(t, "m2-max", "7.2.0-lab2", nil), func(j *api.Job) {
			j.Spec.Source, j.Result.BootKernel, j.Result.OMT.Kernel, j.Result.OMT.KnownGood = "https://github.com/labtest/linux", "7.2.0-lab2", "7.2.0-lab2", false
		}, "only a Known-good run is published"},
		"installed":                     {nil, func(j *api.Job) { j.Spec.InstalledManifest, j.Result.OMT.KnownGood = sha, false }, "an Installed run is never published"},
		"installed-claiming-known-good": {nil, func(j *api.Job) { j.Spec.InstalledManifest = sha }, "an Installed run is never published"},
		"mesa":                          {nil, func(j *api.Job) { j.Spec.Mesa = &api.MesaSpec{} }, "never published"},
		"not-a-package":                 {labReport(t, "m2-max", lrKnownGood, ownership("fail")), nil, "report remains private"},
		"ownership-skipped":             {labReport(t, "m2-max", lrKnownGood, ownership("skip")), nil, "report remains private"},
		"ownership-twice": {labReport(t, "m2-max", lrKnownGood, func(b map[string]any) {
			b["checks"] = append(b["checks"].([]any), check(b, "boot.kernel-package"))
		}), nil, ""},
		"ownership-missing": {labReport(t, "m2-max", lrKnownGood, func(b map[string]any) {
			var kept []any
			for _, c := range b["checks"].([]any) {
				if c.(map[string]any)["id"] != "boot.kernel-package" {
					kept = append(kept, c)
				}
			}
			b["checks"] = kept
		}), nil, "report remains private"},
		"another-kernel": {labReport(t, "m2-max", "7.1.12-other", nil), nil, "report remains private"},
		"not-the-known-good": {labReport(t, "m2-max", "7.1.11-older", nil), func(j *api.Job) {
			j.Result.BootKernel, j.Result.OMT.Kernel = "7.1.11-older", "7.1.11-older"
		}, "the known-good kernel is " + lrKnownGood},
		"no-report": {nil, func(j *api.Job) { j.Result.OMT = nil }, "no omarchy-m-test report"},
	} {
		id := "j-" + name
		l.ran(id, "m2-max", maintainerID, c.report, c.job)
		ravi.notPublished(id, http.StatusConflict, "not published", c.saying)
	}
	if n := len(l.everyReport()); n != 0 {
		t.Fatalf("%d reports that may never be published were stored", n)
	}
	// A job still running publishes nothing.
	l.ran("j-running", "m2-max", maintainerID, nil, func(j *api.Job) { j.State = api.JobTesting })
	ravi.notPublished("j-running", http.StatusConflict, "still")

	for name, job := range map[string]func(*api.Job){
		"known-good": nil,
		"baseline":   func(j *api.Job) { j.Spec.Baseline = true },
		"refresh":    func(j *api.Job) { j.Spec.Holder, j.Maintainer, j.AccountID = omtHolder, "", 0 },
	} {
		l.ran("j-"+name, "m2-max", maintainerID, nil, job)
		ravi.published("j-" + name)
	}
	if n := len(l.everyReport()); n != 3 {
		t.Fatalf("%d reports stored, want the 3 Known-good runs on the packaged kernel", n)
	}
	// Publishing leaves the job's own copy as it was.
	if _, err := os.Stat(filepath.Join(l.s.jobDir("j-known-good"), omtFile)); err != nil {
		t.Error(err)
	}
}

// By hand: a Maintainer any job, an M team member their own.
func TestByHandAMaintainerPublishesAnyJobAndAnMTeamMemberTheirOwn(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	l.lrMac("vol-air", "j313", lrVolunteers(api.AccessFull, l.s.now().Add(2*time.Hour)))
	l.lrLend("vol-air", linusID)
	l.s.dev("vol-air").d.Publish = true
	ravi, edith, linus := l.lrMaintainer(), l.lrMember(edithID), l.signedIn(linusID)
	l.lrMember(graceID)

	l.ran("j-ediths", "m2-max", edithID, nil, nil)
	l.ran("j-graces", "m2-max", graceID, nil, nil)
	l.ran("j-labs", "m2-max", 0, nil, nil)
	l.ran("j-ravis", "m2-max", maintainerID, nil, nil)
	l.ran("j-on-linus-mac", "vol-air", 0, nil, nil)

	// Nobody but the two: not its Mac's Volunteer, not a visitor.
	for who, b := range map[string]*browser{"the Mac's Volunteer": linus, "nobody": l.browser()} {
		if status, said := b.publish("j-on-linus-mac"); status != http.StatusForbidden && status != http.StatusUnauthorized && status != http.StatusNotFound {
			t.Errorf("%s published a job: %d %s", who, status, said)
		}
	}
	edith.notPublished("j-graces", http.StatusForbidden, "an M team member publishes only their own job's report", "@grace", "Ask a Maintainer")
	edith.notPublished("j-labs", http.StatusForbidden, "the lab's own")
	edith.notPublished("j-ravis", http.StatusForbidden, "@ravi")
	if n := len(l.everyReport()); n != 0 {
		t.Fatalf("%d reports stored by people who may not publish", n)
	}

	own := l.report(edith.published("j-ediths"))
	if own.LabJob != "j-ediths" || own.AskedHandle != "edith" || own.AskedAccountID != edithID {
		t.Errorf("a member's own job: %+v", own)
	}
	for _, job := range []string{"j-graces", "j-labs", "j-ravis", "j-on-linus-mac"} {
		report := l.report(ravi.published(job))
		if report.LabJob != job || report.AskedHandle != "ravi" || report.AskedAccountID != maintainerID {
			t.Errorf("a Maintainer publishing %s: %+v", job, report)
		}
	}

	// The mark gone, so is the leave.
	l.ran("j-ediths-next", "m2-max", edithID, nil, nil)
	l.lrUnmark(edithID)
	if status, said := edith.publish("j-ediths-next"); status != http.StatusForbidden {
		t.Errorf("after leaving the M team: %d %s", status, said)
	}
}

// The Volunteer's switch is checked at the moment of publishing. Off, the
// run stays readable; turning it off leaves Published reports up; the Mac
// still lists the deletion links.
func TestTheVolunteersSwitchIsCheckedAtTheMomentOfPublishing(t *testing.T) {
	l := publishLab(t, nil)
	v := l.lrMac("vol-air", "j313", lrVolunteers(api.AccessQualification, l.s.now().Add(2*time.Hour)))
	l.lrMac("m2-max", "j416c", nil)
	l.lrLend("vol-air", linusID)
	ravi, edith := l.lrMaintainer(), l.lrMember(edithID)
	turn := func(on bool) {
		v.mu.Lock()
		v.d.Publish = on
		v.mu.Unlock()
	}
	listed := func() []api.Published {
		t.Helper()
		w := l.asMac("GET", "/api/agent/published", "dev_vol-air", "")
		var out []api.Published
		if err := json.Unmarshal(w.Body.Bytes(), &out); w.Code != http.StatusOK || err != nil {
			t.Fatalf("the Mac's list of its published reports: %d %s", w.Code, w.Body)
		}
		return out
	}

	l.ran("j-off", "vol-air", 0, nil, nil)
	ravi.notPublished("j-off", http.StatusConflict, "publishing is off on vol-air", "only its Volunteer turns it on", "sudo lab-agent publish on")
	if _, err := l.s.omtPublish(l.ran("j-off-itself", "vol-air", 0, nil, func(j *api.Job) { j.Spec.Baseline = true }), Person{}); err == nil || !strings.Contains(err.Error(), "publishing is off") {
		t.Errorf("the lab by itself, with the switch off: %v", err)
	}
	if n := len(l.everyReport()); n != 0 || len(listed()) != 0 {
		t.Fatalf("published with the switch off: %d reports", n)
	}
	// The run was made all the same, and the M team reads it.
	for who, b := range map[string]*browser{"a Maintainer": ravi, "an M team member": edith} {
		said := b.jobSaid("j-off")
		if o := said.Result.OMT; o == nil || o.Pass != 40 || o.ReportState != api.ReportNotPublished || !strings.Contains(o.PublishNote, "publishing is off") {
			t.Errorf("%s reads the unpublished run as %+v", who, o)
		}
		if w := b.do("GET", "/api/jobs/j-off/files/"+omtFile, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"schema_version"`) {
			t.Errorf("%s reads the run's report: %d", who, w.Code)
		}
	}

	// The same job, a moment later: the switch is read when publishing, not when the job ran.
	turn(true)
	id := ravi.published("j-off")
	l.ran("j-on", "vol-air", 0, nil, nil)
	second := ravi.published("j-on")

	turn(false)
	l.ran("j-off-again", "vol-air", 0, nil, nil)
	ravi.notPublished("j-off-again", http.StatusConflict, "publishing is off on vol-air")
	for _, id := range []string{id, second} {
		if w := l.browser().do("GET", "/reports/"+id, ""); w.Code != http.StatusOK {
			t.Errorf("turning the switch off took the published report %s down: %d", id, w.Code)
		}
	}
	if ravi.jobSaid("j-off").Result.OMT.ReportState != api.ReportPublished {
		t.Error("the job no longer says published")
	}

	// `lab-agent publish on` lists the links, the switch on or off.
	got := listed()
	if len(got) != 2 || got[0].Job != "j-off" || got[1].Job != "j-on" || got[0].ReportURL != labAddress+"/reports/"+id {
		t.Fatalf("the Mac lists %+v", got)
	}
	link, err := url.Parse(got[0].DeletionURL)
	if err != nil || !strings.HasPrefix(got[0].DeletionURL, labAddress+"/reports/"+id+"/deletion?token=") || len(link.Query().Get("token")) < 40 {
		t.Fatalf("the deletion link %q", got[0].DeletionURL)
	}
	// The link is nowhere a reader of the job finds it.
	token := link.Query().Get("token")
	for _, target := range []string{"/api/jobs/j-off", "/jobs/j-off", "/api/jobs/j-off/files/omt-published.json", "/api/jobs", "/reports", "/reports/" + id} {
		if w := ravi.do("GET", target, ""); strings.Contains(w.Body.String(), token) {
			t.Errorf("%s shows the deletion link's secret", target)
		}
	}
	names, _ := os.ReadDir(l.s.jobDir("j-off"))
	for _, name := range names {
		if text, _ := os.ReadFile(filepath.Join(l.s.jobDir("j-off"), name.Name())); strings.Contains(string(text), token) {
			t.Errorf("the job's file %s holds the deletion link's secret", name.Name())
		}
	}
	// What the old lab kept of the old site's answer in a job's folder is sent to nobody.
	os.WriteFile(filepath.Join(l.s.jobDir("j-off"), "omt-published.json"), []byte(`{"deletion_url":"https://omarchy-m-testing.org/reports/x/deletion?token=old-secret"}`), 0o600)
	if w := ravi.do("GET", "/api/jobs/j-off/files/omt-published.json", ""); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "old-secret") {
		t.Errorf("the old site's deletion link is served: %d", w.Code)
	}

	// The link deletes: the report is gone, the Mac lists one, and the job says who and when.
	visitor := l.browser()
	if w := visitor.do("GET", link.RequestURI(), ""); w.Code != http.StatusOK {
		t.Fatalf("the deletion link's page: %d", w.Code)
	}
	if w := visitor.do("POST", "/reports/"+id, url.Values{"_method": {"delete"}, "token": {token}}.Encode(), "Content-Type", formType, "Origin", labAddress); w.Code != http.StatusOK {
		t.Fatalf("the deletion, sent: %d %s", w.Code, w.Body)
	}
	if got := listed(); len(got) != 1 || got[0].Job != "j-on" {
		t.Errorf("after the deletion the Mac lists %+v", got)
	}
	if _, err := os.Stat(l.s.deletionLinkPath("j-off")); !os.IsNotExist(err) {
		t.Errorf("the link of a deleted report is still kept: %v", err)
	}
	said := ravi.jobSaid("j-off").Result.OMT
	if said.ReportState != api.ReportIsDeleted || said.ReportDeleted == nil || said.ReportDeleted.By != "whoever holds its deletion link" || !said.ReportDeleted.Time.Equal(l.s.now()) {
		t.Errorf("the job of a report deleted by its link says %q, %+v", said.ReportState, said.ReportDeleted)
	}

	// A job the Mac's name ran in an earlier enrollment is another Mac's: its link is not this one's.
	l.ran("j-earlier", "vol-air", 0, nil, func(j *api.Job) {
		j.DeviceID, j.Result.OMT.ReportID, j.Result.OMT.Published = "d-earlier", second, labAddress+"/reports/"+second
	})
	if got := listed(); len(got) != 1 || got[0].Job != "j-on" {
		t.Errorf("the Mac lists an earlier enrollment's report: %+v", got)
	}

	// A Lab-owned Mac has no Volunteer to hand a link to: none is kept.
	l.ran("j-labs", "m2-max", 0, nil, nil)
	ravi.published("j-labs")
	if _, err := os.Stat(l.s.deletionLinkPath("j-labs")); !os.IsNotExist(err) {
		t.Errorf("a deletion link was kept for a Lab-owned Mac's report: %v", err)
	}
	if st, err := os.Stat(l.s.deletionLinkPath("j-on")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the kept link of a Volunteer's Mac: %v", err)
	}
}

// Verified or Community follows the Mac's sign-in at upload: a lab run on an
// External volunteer's Mac is a Community run, unless an M team member
// signed that Mac in (decision S7).
func TestVerifiedOrCommunityFollowsTheMacsSignInAtUpload(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("lab-mini", "j474s", nil)
	for _, mac := range []string{"vol-by-member", "vol-by-volunteer", "vol-by-nobody"} {
		l.lrMac(mac, "j313", lrVolunteers(api.AccessQualification, l.s.now().Add(2*time.Hour)))
		l.lrLend(mac, linusID) // linus is outside the M team
		l.s.dev(mac).d.Publish = true
	}
	ravi, _ := l.lrMaintainer(), l.lrMember(graceID)
	l.signedIn(linusID)
	l.mustMacSignIn("lab-mini", maintainerID)
	l.mustMacSignIn("vol-by-member", graceID)
	l.mustMacSignIn("vol-by-volunteer", linusID)

	ids := map[string]string{}
	for mac, c := range map[string]struct {
		verified bool
		handle   string
	}{
		"lab-mini":         {true, "ravi"},
		"vol-by-member":    {true, "grace"},
		"vol-by-volunteer": {false, "linus"},
		"vol-by-nobody":    {false, ""},
	} {
		// Who asked changes nothing: the lab by itself for one, a Maintainer for the next.
		job := l.ran("j-"+mac, mac, 0, nil, func(j *api.Job) { j.Spec.Baseline = true })
		var id string
		if mac == "vol-by-member" || mac == "vol-by-nobody" {
			if _, err := l.s.omtPublish(job, Person{}); err != nil {
				t.Fatalf("%s: %v", mac, err)
			}
			id = ravi.jobSaid(job.ID).Result.OMT.ReportID
		} else {
			id = ravi.published(job.ID)
		}
		ids[mac] = id
		report := l.report(id)
		if report.Verified != c.verified || report.Handle != c.handle {
			t.Errorf("a lab run on %s: Verified %v, stamped %q; want %v, %q", mac, report.Verified, report.Handle, c.verified, c.handle)
		}
		want := "Community run"
		if c.verified {
			want = "Verified run"
		}
		if page := l.browser().page("/reports/" + id); !strings.Contains(page, want) {
			t.Errorf("the page of the run on %s does not say %q", mac, want)
		}
	}
	// Stamped then, never recomputed.
	l.lrUnmark(graceID)
	if !l.report(ids["vol-by-member"]).Verified {
		t.Error("the verdict changed when the member left the M team")
	}
	l.ran("j-after", "vol-by-member", 0, nil, nil)
	if l.report(ravi.published("j-after")).Verified {
		t.Error("a run published after the member left the M team is a Verified run")
	}
}

// A repeat gets "Already uploaded" and the report's link, never its
// deletion link.
func TestARepeatGetsAlreadyUploadedAndTheReportsLink(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()

	// The Mac's own tool uploaded this run first.
	own := labReport(t, "m2-max", lrKnownGood, nil)
	first := l.fromMac("/api/v1/reports", string(own), macAddress)
	if first.status != http.StatusCreated {
		t.Fatalf("%d %s", first.status, first.raw)
	}
	link := labAddress + "/reports/" + first.body["id"].(string)
	token := first.body["deletion_url"].(string)[strings.Index(first.body["deletion_url"].(string), "token=")+6:]
	l.ran("j-same", "m2-max", 0, own, nil)
	status, said := ravi.publish("j-same")
	if status != http.StatusConflict || !strings.Contains(said, "Already uploaded: "+link) || strings.Contains(said, "deletion") || strings.Contains(said, token) {
		t.Errorf("the lab's repeat of the tool's upload: %d %s", status, said)
	}
	job := ravi.jobSaid("j-same").Result.OMT
	if job.ReportState != api.ReportNotPublished || job.Published != "" || job.ReportID != "" || !strings.Contains(job.PublishNote, "Already uploaded: "+link) {
		t.Errorf("the job of a repeat says %+v", job)
	}
	if report := l.report(first.body["id"].(string)); report.ByLab() {
		t.Error("the tool's report became the lab's")
	}

	// The lab's own report, published again: the same link, nothing new.
	l.ran("j-lab", "m2-max", 0, nil, nil)
	id := ravi.published("j-lab")
	if again := ravi.published("j-lab"); again != id {
		t.Errorf("publishing a published job again gave %s, not %s", again, id)
	}
	// And the same report from another job, or back from the Mac's own tool.
	text, _ := os.ReadFile(filepath.Join(l.s.jobDir("j-lab"), omtFile))
	l.ran("j-lab-copy", "m2-max", 0, text, nil)
	ravi.notPublished("j-lab-copy", http.StatusConflict, "Already uploaded: "+labAddress+"/reports/"+id)
	if a := l.fromMac("/api/v1/reports", string(text), macAddress); a.status != http.StatusConflict || a.body["error"] != "Already uploaded" || a.body["report_url"] != labAddress+"/reports/"+id || a.body["deletion_url"] != nil {
		t.Errorf("the tool's repeat of the lab's report: %d %s", a.status, a.raw)
	}
	if n := len(l.everyReport()); n != 2 {
		t.Errorf("%d reports stored, want 2", n)
	}
}

// How a report arrived is recorded with the job and who asked. Maintainers
// see it on Reports; the public page gains "run by the lab" beside
// "unattended" and names nobody.
func TestHowAReportArrivedIsOnReportsAndThePublicPageSaysRunByTheLab(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi, edith := l.lrMaintainer(), l.lrMember(edithID)
	unattended := func(b map[string]any) { b["unattended"] = true }

	tool := l.fromMac("/api/v1/reports", string(labReport(t, "m2-max", lrKnownGood, unattended)), macAddress)
	if tool.status != http.StatusCreated {
		t.Fatalf("%d %s", tool.status, tool.raw)
	}
	byTool := tool.body["id"].(string)
	if _, err := l.s.omtPublish(l.ran("j-itself", "m2-max", 0, labReport(t, "m2-max", lrKnownGood, unattended), func(j *api.Job) { j.Spec.Baseline = true }), Person{}); err != nil {
		t.Fatal(err)
	}
	byItself := ravi.jobSaid("j-itself").Result.OMT.ReportID
	l.ran("j-by-hand", "m2-max", edithID, nil, nil)
	byHand := edith.published("j-by-hand")

	list := parseHTML(t, ravi.page("/reports"))
	for id, want := range map[string]string{
		byTool:   "the Mac's own tool",
		byItself: "the lab by itself, job j-itself",
		byHand:   "the lab, job j-by-hand, asked by @edith",
	} {
		row := list.sole(t, `tr[id="admin-report-`+id+`"]`)
		if got := row.sole(t, "td.arrived").words(); got != want {
			t.Errorf("Reports says the report %s arrived by %q, want %q", id, got, want)
		}
	}
	if got := list.sole(t, `tr[id="admin-report-`+byItself+`"] td.arrived a`).attr("href"); got != "/jobs/j-itself" {
		t.Errorf("the job's link on Reports: %q", got)
	}
	// Nobody else is told: not the M team, not a visitor.
	for who, b := range map[string]*browser{"an M team member": edith, "a visitor": l.browser()} {
		page := b.page("/reports")
		if strings.Contains(page, "arrived") || strings.Contains(page, "j-by-hand") || strings.Contains(page, "j-itself") || strings.Contains(page, "own tool") {
			t.Errorf("Reports tells %s how a report arrived", who)
		}
	}

	visitor := l.browser()
	for id, lab := range map[string]bool{byTool: false, byItself: true, byHand: true} {
		page := parseHTML(t, visitor.page("/reports/"+id))
		if got := page.count(t, "#run-by-the-lab"); (got == 1) != lab {
			t.Errorf("the page of %s: %d lines saying run by the lab, by the lab %v", id, got, lab)
		}
		if lab && page.sole(t, "#run-by-the-lab").words() != "run by the lab" {
			t.Errorf("the line reads %q", page.sole(t, "#run-by-the-lab").words())
		}
		text := visitor.page("/reports/" + id)
		for _, name := range []string{"edith", "ravi", "j-by-hand", "j-itself", "asked by"} {
			if strings.Contains(text, name) {
				t.Errorf("the public page of %s says %q", id, name)
			}
		}
	}
	// Beside "unattended": the line after it among the machine's facts.
	if !regexp.MustCompile(`<dd id="unattended">[^<]*nobody was at the Mac[^<]*</dd>\s*<dt>run</dt><dd id="run-by-the-lab">run by the lab</dd>`).MatchString(visitor.page("/reports/" + byItself)) {
		t.Error("run by the lab is not beside unattended")
	}
	if page := visitor.page("/reports/" + byHand); strings.Contains(page, `id="unattended"`) || !strings.Contains(page, `<dt>run</dt><dd id="run-by-the-lab">run by the lab</dd>`) {
		t.Error("an attended lab run's line")
	}
	// A Maintainer's own view of the page names nobody either.
	if text := ravi.page("/reports/" + byHand); !strings.Contains(text, "run by the lab") || strings.Contains(text, "asked by") {
		t.Error("a Maintainer's view of a lab run's page")
	}
}

// A job says what became of its report, in four words; after a delete it
// says who deleted it and when, and the lab never publishes it again.
func TestAJobSaysWhatBecameOfItsReport(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi, edith := l.lrMaintainer(), l.lrMember(edithID)
	state := func(job string) string { return edith.jobSaid(job).Result.OMT.ReportState }
	listed := func() map[string]string {
		t.Helper()
		w := edith.do("GET", "/api/jobs?limit=50", "")
		var jobs []*api.Job
		if err := json.Unmarshal(w.Body.Bytes(), &jobs); w.Code != http.StatusOK || err != nil {
			t.Fatalf("jobs: %d %s", w.Code, w.Body)
		}
		out := map[string]string{}
		for _, j := range jobs {
			out[j.ID] = "no report"
			if j.Result.OMT != nil {
				out[j.ID] = j.Result.OMT.ReportState
			}
		}
		return out
	}
	both := func(job, want string) {
		t.Helper()
		if got, list := state(job), listed()[job]; got != want || list != want {
			t.Errorf("job %s says %q, and in the list %q; want %q", job, got, list, want)
		}
	}

	l.pjJob("j-no-test", "m2-max", 0, "", l.s.now(), nil)
	l.ran("j1", "m2-max", edithID, nil, nil)
	l.ran("j-other", "m2-max", 0, nil, nil)
	both("j1", api.ReportNotPublished)
	if got := listed()["j-no-test"]; got != "no report" {
		t.Errorf("a job with no omarchy-m-test run says %q", got)
	}

	id := edith.published("j1")
	both("j1", api.ReportPublished)
	both("j-other", api.ReportNotPublished)
	if said := edith.jobSaid("j1").Result.OMT; said.Published != labAddress+"/reports/"+id || said.ReportID != id || said.PublishNote != "" {
		t.Errorf("a published job: %+v", said)
	}

	ravi.did("/reports/" + id + "/hide")
	both("j1", api.ReportHidden)
	edith.notPublished("j1", http.StatusConflict, "its report is Hidden")
	ravi.did("/reports/" + id + "/show")
	both("j1", api.ReportPublished)

	l.clock.Advance(26 * time.Hour)
	at := l.s.now()
	ravi.did("/reports/"+id+"/delete", "sure", "yes")
	both("j1", api.ReportIsDeleted)
	said := edith.jobSaid("j1").Result.OMT
	if d := said.ReportDeleted; d == nil || d.By != "@ravi" || d.AccountID != maintainerID || !d.Time.Equal(at) {
		t.Fatalf("the job does not say who deleted its report and when: %+v", d)
	}

	// Never again: not by itself, and not by hand either.
	day := at.UTC().Format("2006-01-02")
	job, _ := l.s.store.job("j1")
	if _, err := l.s.omtPublish(job, Person{}); err == nil || !strings.Contains(err.Error(), "its report was deleted by @ravi on "+day) {
		t.Errorf("the lab published a job again after its report was deleted: %v", err)
	}
	edith.notPublished("j1", http.StatusConflict, "its report was deleted by @ravi on "+day, "not published again")
	ravi.notPublished("j1", http.StatusConflict, "its report was deleted")
	if n := len(l.everyReport()); n != 0 {
		t.Errorf("%d reports stored after the delete", n)
	}
	both("j1", api.ReportIsDeleted)
	// The same run from a job of its own is the same report: its digest stays.
	text, _ := os.ReadFile(filepath.Join(l.s.jobDir("j1"), omtFile))
	l.ran("j1-again", "m2-max", 0, text, func(j *api.Job) { j.Spec.Baseline = true })
	again, _ := l.s.store.job("j1-again")
	if _, err := l.s.omtPublish(again, Person{}); err == nil || !strings.Contains(err.Error(), "Already uploaded") || !strings.Contains(err.Error(), "deleted since") {
		t.Errorf("the deleted report came back from another job: %v", err)
	}
	// Public pages say nothing.
	if w := l.browser().do("GET", "/reports/"+id, ""); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "ravi") {
		t.Errorf("the deleted report's page: %d", w.Code)
	}

	// A report gone with nobody recorded is deleted all the same.
	l.ran("j2", "m2-max", 0, nil, nil)
	gone := ravi.published("j2")
	if err := l.s.reports.Delete(gone); err != nil {
		t.Fatal(err)
	}
	both("j2", api.ReportIsDeleted)
	ravi.notPublished("j2", http.StatusConflict, "its report was deleted, and a deleted report is not published again")

	// A job the old lab published on the old site says published while its report is carried, by its old id.
	carried := l.fromMac("/api/v1/reports", string(labReport(t, "m2-max", lrKnownGood, nil)), macAddress).body["id"].(string)
	l.ran("j-old", "m2-max", 0, nil, func(j *api.Job) { j.Result.OMT.Published = "https://omarchy-m-testing.org/reports/" + carried })
	l.ran("j-old-gone", "m2-max", 0, nil, func(j *api.Job) { j.Result.OMT.Published = "https://omarchy-m-testing.org/reports/NoSuchReport" })
	both("j-old", api.ReportPublished)
	both("j-old-gone", api.ReportIsDeleted)
}

// With Preview on, the lab publishes nothing.
func TestWithPreviewOnTheLabPublishesNothing(t *testing.T) {
	l := publishLab(t, func(c *Config) { c.Preview = true })
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	l.ran("j-by-hand", "m2-max", maintainerID, nil, nil)
	ravi.notPublished("j-by-hand", http.StatusConflict, "Omarchy Mac Lab is not open yet and publishes no report")
	job := l.ran("j-itself", "m2-max", 0, nil, func(j *api.Job) { j.Spec.Baseline = true })
	if _, err := l.s.omtPublish(job, Person{}); err == nil || !strings.Contains(err.Error(), "not open yet") {
		t.Errorf("the lab by itself in Preview: %v", err)
	}
	if n := len(l.everyReport()); n != 0 {
		t.Fatalf("%d reports published in Preview", n)
	}
	for _, id := range []string{"j-by-hand", "j-itself"} {
		if o := ravi.jobSaid(id).Result.OMT; o.ReportState != api.ReportNotPublished || !strings.Contains(o.PublishNote, "not open yet") {
			t.Errorf("job %s in Preview says %+v", id, o)
		}
	}
	if got := l.s.dev("m2-max").snapshot().MachineID; got != "" {
		t.Errorf("Preview recorded the machine id %q", got)
	}
}

// A lab that was given no machine-id key publishes nothing and says so.
func TestALabWithoutItsMachineIDKeyPublishesNothing(t *testing.T) {
	l := rolesLab(t, func(c *Config) { c.OMT, c.OMTPublish = true, true })
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.signedIn(maintainerID)
	l.ran("j1", "m2-max", maintainerID, nil, nil)
	ravi.notPublished("j1", http.StatusConflict, "this lab takes no reports: it was given no machine-id key")
}

// A job's page shows its report's state, and Publish to whoever may publish
// it by hand.
func TestAJobsPageShowsItsReportsStateAndPublish(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	l.lrMac("vol-air", "j313", lrVolunteers(api.AccessQualification, l.s.now().Add(2*time.Hour)))
	l.lrLend("vol-air", linusID)
	l.s.dev("vol-air").d.Publish = true
	ravi, edith, linus := l.lrMaintainer(), l.lrMember(edithID), l.signedIn(linusID)
	l.lrMember(graceID)
	report := func(b *browser, job string) *node {
		t.Helper()
		return parseHTML(t, b.page("/jobs/"+job)).sole(t, "#report")
	}
	publishes := func(b *browser, job string) bool {
		t.Helper()
		return parseHTML(t, b.page("/jobs/"+job)).count(t, `form[action="/jobs/`+job+`/publish"] button`) == 1
	}

	l.ran("j-ediths", "m2-max", edithID, nil, nil)
	l.ran("j-graces", "m2-max", graceID, nil, nil)
	l.ran("j-vol", "vol-air", 0, nil, nil)
	l.ran("j-test-kernel", "m2-max", edithID, labReport(t, "m2-max", "7.2.0-lab1", nil), func(j *api.Job) {
		j.Spec.Kernel, j.Result.BootKernel, j.Result.OMT.Kernel, j.Result.OMT.KnownGood = strings.Repeat("ab", 32), "7.2.0-lab1", "7.2.0-lab1", false
	})
	l.pjJob("j-no-test", "m2-max", 0, "", l.s.now(), nil)

	if got := report(edith, "j-ediths").words(); got != "Report: not published" {
		t.Errorf("an unpublished job's page says %q", got)
	}
	if strings.Contains(edith.page("/jobs/j-no-test"), `id="report"`) {
		t.Error("a job with no omarchy-m-test run says something of a report")
	}
	// Publish: a member on their own job, a Maintainer on any; never on a test-kernel run, never for the Volunteer.
	for name, c := range map[string]struct {
		b    *browser
		job  string
		want bool
	}{
		"a member, their own job":         {edith, "j-ediths", true},
		"a member, another's job":         {edith, "j-graces", false},
		"a member, the lab's job":         {edith, "j-vol", false},
		"a member, a test-kernel run":     {edith, "j-test-kernel", false},
		"a Maintainer, a member's job":    {ravi, "j-graces", true},
		"a Maintainer, the lab's job":     {ravi, "j-vol", true},
		"a Maintainer, a test-kernel run": {ravi, "j-test-kernel", false},
		"the Volunteer, their Mac's job":  {linus, "j-vol", false},
	} {
		if got := publishes(c.b, c.job); got != c.want {
			t.Errorf("%s: Publish offered %v, want %v", name, got, c.want)
		}
	}
	// A forged post is refused as `lab publish` is.
	if w := edith.form("/jobs/j-graces/publish"); w.Code != http.StatusForbidden || !strings.Contains(refusalIn(w.Body.String()), "an M team member publishes only their own job's report") {
		t.Errorf("a member publishing another's job from the page: %d %s", w.Code, refusalIn(w.Body.String()))
	}
	if w := linus.form("/jobs/j-vol/publish"); w.Code != http.StatusNotFound && w.Code != http.StatusForbidden {
		t.Errorf("the Volunteer publishing from the page: %d", w.Code)
	}
	if w := edith.form("/jobs/j-test-kernel/publish"); w.Code != http.StatusConflict || !strings.Contains(refusalIn(w.Body.String()), "Not published: ran on test kernel 7.2.0-lab1") {
		t.Errorf("a test-kernel run published from the page: %d %s", w.Code, refusalIn(w.Body.String()))
	}
	if got := report(edith, "j-test-kernel").words(); !strings.HasPrefix(got, "Report: not published ran on test kernel 7.2.0-lab1") {
		t.Errorf("the page of a run that was refused says %q", got)
	}
	if n := len(l.everyReport()); n != 0 {
		t.Fatalf("%d reports published by refused posts", n)
	}

	w := edith.form("/jobs/j-ediths/publish")
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/jobs/j-ediths" {
		t.Fatalf("Publish: %d to %q", w.Code, w.Header().Get("Location"))
	}
	id := l.everyReport()[0].PublicID
	if r := l.report(id); r.LabJob != "j-ediths" || r.AskedHandle != "edith" {
		t.Errorf("published from the page: %+v", r)
	}
	line := report(edith, "j-ediths")
	if line.words() != "Report: published the report" || line.sole(t, "a").attr("href") != "/reports/"+id || line.sole(t, ".badge").attr("class") != "badge job-ok" || publishes(edith, "j-ediths") {
		t.Errorf("a published job's page says %q", line.words())
	}

	ravi.did("/reports/" + id + "/hide")
	if line := report(edith, "j-ediths"); line.words() != "Report: hidden" || line.count(t, "a") != 0 {
		t.Errorf("a member reads a hidden report's job as %q", line.words())
	}
	if line := report(ravi, "j-ediths"); line.words() != "Report: hidden the report" || line.sole(t, "a").attr("href") != "/reports/"+id {
		t.Errorf("a Maintainer reads a hidden report's job as %q", line.words())
	}
	ravi.did("/reports/"+id+"/delete", "sure", "yes")
	want := "Report: deleted by @ravi, " + whenDay(l.s.now())
	for who, b := range map[string]*browser{"its member": edith, "a Maintainer": ravi} {
		if line := report(b, "j-ediths"); line.words() != want || line.count(t, "a") != 0 || publishes(b, "j-ediths") {
			t.Errorf("%s reads the job of a deleted report as %q, want %q", who, line.words(), want)
		}
	}

	// The Volunteer reads their Mac's job, the state with it, and the Volunteer's own delete is told too.
	ravi.published("j-vol")
	if got := report(linus, "j-vol").words(); got != "Report: published the report" {
		t.Errorf("the Volunteer reads %q", got)
	}
}

// ended is a run's end for a job left as the runner has it then: still
// collecting, on the runner's own copy.
func (l *signInLab) ended(j *api.Job, becameReference bool) *api.OMTResult {
	l.t.Helper()
	j.State = api.JobCollecting
	(&jobRun{s: l.s, j: j, v: l.s.dev(j.Spec.Device)}).publishRun(becameReference)
	j.State = api.JobDone
	l.s.updateJob(j, func(*api.Job) {})
	return j.Result.OMT
}

// At a run's end: `run --publish` is by hand, for whoever submitted the
// job, if they may still publish; everything else is the lab by itself.
func TestAtARunsEndWhoAskedIsWhoSubmittedTheJob(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	l.lrMaintainer()
	l.lrMember(edithID)
	l.lrMember(graceID)
	asks := func(j *api.Job) { j.Spec.Publish = true }

	// A member's own run, asked to be published: theirs, whether or not it became the Reference.
	o := l.ended(l.ran("j-ediths", "m2-max", edithID, nil, asks), false)
	if report := l.report(o.ReportID); report.AskedHandle != "edith" || report.AskedAccountID != edithID || report.LabJob != "j-ediths" {
		t.Errorf("a member's run --publish: %+v", report)
	}
	// A baseline a Maintainer asked to publish is asked by them; one nobody asked is the lab's own.
	o = l.ended(l.ran("j-ravis-baseline", "m2-max", maintainerID, nil, func(j *api.Job) { j.Spec.Baseline, j.Spec.Publish = true, true }), true)
	if report := l.report(o.ReportID); report.AskedHandle != "ravi" {
		t.Errorf("a Maintainer's baseline --publish: %+v", report)
	}
	o = l.ended(l.ran("j-baseline", "m2-max", maintainerID, nil, func(j *api.Job) { j.Spec.Baseline = true }), true)
	if report := l.report(o.ReportID); report.AskedHandle != "" || report.AskedAccountID != 0 {
		t.Errorf("a baseline nobody asked to publish names who ran it: %+v", report)
	}
	before := len(l.everyReport())

	// Whoever left the M team, or was Blocked, between asking and the run's end asked for nothing.
	left := l.ran("j-left", "m2-max", graceID, nil, asks)
	l.lrUnmark(graceID)
	if o := l.ended(left, true); o.ReportID != "" || !strings.Contains(o.PublishNote, "publishing a job's report is for Maintainers") {
		t.Errorf("a run whose member left the M team: %+v", o)
	}
	blocked := l.ran("j-blocked", "m2-max", edithID, nil, asks)
	if err := l.s.store.block(edithID, maintainerID, "a test", l.s.now()); err != nil {
		t.Fatal(err)
	}
	if o := l.ended(blocked, true); o.ReportID != "" || o.PublishNote == "" {
		t.Errorf("a run whose member was Blocked: %+v", o)
	}
	// A person's job under the scheduler's label is no refresh, and a
	// person's Known-good run that became the Reference waits to be asked.
	for name, change := range map[string]func(*api.Job){
		"j-labelled": func(j *api.Job) { j.Spec.Holder = omtHolder },
		"j-plain":    nil,
	} {
		if o := l.ended(l.ran(name, "m2-max", maintainerID, nil, change), true); o.ReportID != "" || o.PublishNote != "" {
			t.Errorf("%s was published by itself: %+v", name, o)
		}
	}
	// A baseline that did not become the Reference.
	if o := l.ended(l.ran("j-failed-baseline", "m2-max", maintainerID, nil, func(j *api.Job) { j.Spec.Baseline = true }), false); o.ReportID != "" ||
		!strings.Contains(o.PublishNote, "only a run that became the Mac's Reference") {
		t.Errorf("a baseline that did not become the Reference: %+v", o)
	}
	// An Installed run has no end that publishes.
	if o := l.ended(l.ran("j-installed", "m2-max", maintainerID, nil, func(j *api.Job) { j.Spec.InstalledManifest, j.Spec.Publish = strings.Repeat("ab", 32), true }), true); o.ReportID != "" {
		t.Errorf("an Installed run was published: %+v", o)
	}
	if n := len(l.everyReport()); n != before {
		t.Errorf("%d more reports published", n-before)
	}
}

// Two asks for one job at once publish it once, and the second finds what
// the first did.
func TestTwoAsksForOneJobPublishItOnce(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	l.ran("j1", "m2-max", 0, nil, nil)
	stale, _ := l.s.store.job("j1")
	id := ravi.published("j1")
	url, err := l.s.omtPublish(stale, Person{})
	if err != nil || url != labAddress+"/reports/"+id {
		t.Fatalf("the second ask: %q %v", url, err)
	}
	if o := ravi.jobSaid("j1").Result.OMT; o.ReportID != id || o.ReportState != api.ReportPublished || o.PublishNote != "" || len(l.everyReport()) != 1 {
		t.Errorf("after two asks the job says %+v", o)
	}
}

// A lab run's report is the one the test labd asked for left in the job's
// own folder. A Mac that answers with another name, one that climbs into
// another job's folder, gives its job no report, and nothing of the other
// job is published as this one's.
func TestALabRunsReportIsTheAskedTestsInTheJobsOwnFolder(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	l.lrMac("m1-pro", "j316s", nil)
	ravi := l.lrMaintainer()
	l.ran("j-victim", "m1-pro", 0, labReport(t, "m1-pro", lrKnownGood, nil), nil)
	for i, name := range []string{"../../../j-victim/test-omarchy-m-test", "x/../../../j-victim/test-omarchy-m-test", "../../../j-victim/test-omarchy-m-test"} {
		j := l.ran("j-own", "m2-max", 0, nil, func(j *api.Job) { j.Result.OMT = nil })
		os.RemoveAll(l.s.jobDir("j-own"))
		j.State = api.JobCollecting
		tr := api.TestResult{Name: name, Passed: true}
		asked := omtTest
		if i == 2 {
			asked = name // a name labd would never have asked for
		}
		(&jobRun{s: l.s, j: j, v: l.s.dev("m2-max")}).omtResult(asked, &tr)
		j.State = api.JobDone
		l.s.updateJob(j, func(*api.Job) {})
		if tr.Passed || j.Result.OMT != nil {
			t.Errorf("a Mac answering %q gave its job a report: passed %v, %+v", name, tr.Passed, j.Result.OMT)
		}
		ravi.notPublished("j-own", http.StatusConflict, "no omarchy-m-test report")
	}
	if n := len(l.everyReport()); n != 0 {
		t.Errorf("%d reports were published", n)
	}
}

// Once the lab has a Mac's machine id, a report signed by another Machine
// key is not that Mac's: it is refused, and the id the lab recorded stays.
func TestAReportOfAnotherMachineKeyIsRefusedAndTheRecordedIDStays(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	want := machineIDOf(t, "m2-max")
	l.ran("j1", "m2-max", 0, nil, nil)
	ravi.published("j1")
	l.ran("j2", "m2-max", 0, labReport(t, "m1-pro", lrKnownGood, nil), nil)
	ravi.notPublished("j2", http.StatusConflict, "another Machine key")
	if got := l.s.dev("m2-max").snapshot().MachineID; got != want {
		t.Errorf("the Mac's machine id is %q, want the one it had, %q", got, want)
	}
	if n := len(l.everyReport()); n != 1 {
		t.Errorf("%d reports were published, want 1", n)
	}
}

// collected is a finished run on mac whose report labd has read as the
// runner does, from the job's files: nobody asked to publish it.
func (l *signInLab) collected(id, mac string, report []byte) *api.Job {
	l.t.Helper()
	j := l.ran(id, mac, 0, report, func(j *api.Job) { j.Result.OMT = nil })
	j.State = api.JobCollecting
	tr := api.TestResult{Name: omtTest, Passed: true}
	(&jobRun{s: l.s, j: j, v: l.s.dev(mac)}).omtResult(omtTest, &tr)
	j.State = api.JobDone
	l.s.updateJob(j, func(*api.Job) {})
	if j.Result.OMT == nil {
		l.t.Fatalf("job %s has no verdict", id)
	}
	return j
}

// A Mac's Machine key is the one that signed the first report the lab
// collected of it, published or not. Private runs under key A bind A, and a
// later run signed by key B is refused when it is published.
func TestAMacsMachineKeyIsBoundAtItsFirstCollectedReport(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	a := machineIDOf(t, "m2-max")
	for _, id := range []string{"j-private-1", "j-private-2"} {
		l.collected(id, "m2-max", labReport(t, "m2-max", lrKnownGood, nil))
		if got := l.s.dev("m2-max").snapshot().MachineID; got != a {
			t.Fatalf("after private run %s the Mac's machine id is %q, want its first report's, %q", id, got, a)
		}
	}
	other := l.collected("j-other-key", "m2-max", labReport(t, "m1-pro", lrKnownGood, nil))
	if note := ravi.jobSaid(other.ID).Result.OMT.PublishNote; note != reports.AnotherMachineKey {
		t.Errorf("a run of another key is not marked as not publishable: %q", note)
	}
	ravi.notPublished("j-other-key", http.StatusConflict, reports.AnotherMachineKey)
	if got := l.s.dev("m2-max").snapshot().MachineID; got != a {
		t.Errorf("the Mac's machine id is %q after a report of another key, want %q", got, a)
	}
	if n := len(l.everyReport()); n != 0 {
		t.Errorf("%d reports were published, want none", n)
	}
	ravi.published("j-private-2")
	// A report of another enrollment under the Mac's name binds nothing.
	l.lrMac("m1-pro", "j316s", nil)
	l.ran("j-earlier", "m1-pro", 0, labReport(t, "another-mac", lrKnownGood, nil), func(j *api.Job) { j.DeviceID = "d-earlier"; j.Result.OMT = nil })
	earlier, _ := l.s.store.job("j-earlier")
	earlier.State = api.JobCollecting
	(&jobRun{s: l.s, j: earlier, v: l.s.dev("m1-pro")}).omtResult(omtTest, &api.TestResult{Name: omtTest, Passed: true})
	if got := l.s.dev("m1-pro").snapshot().MachineID; got != "" {
		t.Errorf("a report of an earlier enrollment bound %q", got)
	}
}

// A first-ever run that is published binds its own key, as before.
func TestAFirstEverRunThatIsPublishedBindsItsOwnKey(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	l.collected("j1", "m2-max", labReport(t, "m2-max", lrKnownGood, nil))
	report := l.report(ravi.published("j1"))
	if want := machineIDOf(t, "m2-max"); report.MachineID != want || l.s.dev("m2-max").snapshot().MachineID != want {
		t.Errorf("the report's machine id %q, the Mac's %q, want %q", report.MachineID, l.s.dev("m2-max").snapshot().MachineID, want)
	}
}

// What is published is the report the run's verdict was computed from. A
// later test of the same name that leaves another report in its place
// changes nothing that is published: that job's report stays private.
func TestASecondTestOfTheSameNameDoesNotChangeWhatIsPublished(t *testing.T) {
	l := publishLab(t, nil)
	l.lrMac("m2-max", "j416c", nil)
	ravi := l.lrMaintainer()
	first := labReport(t, "m2-max", lrKnownGood, nil)
	j := l.ran("j-two", "m2-max", 0, first, func(j *api.Job) { j.Result.OMT = nil })
	j.State = api.JobCollecting
	tr := api.TestResult{Name: omtTest, Passed: true}
	(&jobRun{s: l.s, j: j, v: l.s.dev("m2-max")}).omtResult(omtTest, &tr)
	if j.Result.OMT == nil {
		t.Fatal("the run has no verdict")
	}
	later := labReport(t, "m2-max", lrKnownGood, func(body map[string]any) { check(body, "hardware.firmware")["status"] = "pass" })
	if err := os.WriteFile(filepath.Join(l.s.jobDir("j-two"), omtFile), later, 0o644); err != nil {
		t.Fatal(err)
	}
	j.State = api.JobDone
	l.s.updateJob(j, func(*api.Job) {})
	ravi.notPublished("j-two", http.StatusConflict, "not the one its verdict was computed from")
	if n := len(l.everyReport()); n != 0 {
		t.Errorf("%d reports were published", n)
	}
}
