package server

// omarchy-m-test (github.com/maralcbr/omarchy-m-testing) checks every hardware
// feature of a Mac under Omarchy and writes a signed report. The lab runs it
// in the desktop session after a boot, compares the report with the same
// Mac's report on its known-good kernel, and can publish the report of a
// Known-good run.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/reports"
)

// omtPackagingChecks look for the running kernel among the installed packages
// and in /boot. A lab boot runs a kernel that is neither, so they fail for
// every lab kernel and say nothing about it. boot.chain runs Omarchy's boot
// check, which wants the installed linux-aurora running (Limine Macs).
var omtPackagingChecks = map[string]bool{"boot.files": true, "boot.kernel-package": true, "boot.chain": true}

const (
	omtTest     = "omarchy-m-test"
	omtFile     = "test-" + omtTest + "/omt-report.json" // omtReportFile(omtTest)
	omtMinAgent = "0.3.0"                                // first agent with the builtin test
)

// omtReportFile is where a job keeps the report of its omarchy-m-test run
// named name: the agent uploads a test's files under test-<name>/. The test
// is the builtin omarchy-m-test whatever its name in the job.
func omtReportFile(name string) string { return "test-" + name + "/omt-report.json" }

// omtReport is the part of a report the lab reads; the file itself is kept whole.
type omtReport struct {
	Tool struct {
		Version string `json:"version"`
	} `json:"tool"`
	Catalogue int `json:"catalogue_version"`
	Machine   struct {
		Kernel string `json:"kernel"`
	} `json:"machine"`
	Inventory struct {
		Unclaimed []struct {
			Compatible string `json:"compatible"`
			Count      int    `json:"count"` // its nodes no driver claimed
			Outcome    string `json:"outcome"`
		} `json:"unclaimed"`
	} `json:"inventory"`
	Checks []struct {
		ID             string   `json:"id"`
		Status         string   `json:"status"`
		Evidence       []string `json:"evidence"`
		Classification struct {
			Outcome string `json:"outcome"`
		} `json:"classification"`
	} `json:"checks"`
	Unattended bool `json:"unattended"` // --unattended: only the automatic checks ran
}

// omtBaseline is the known-good report a Mac's other runs are compared with.
type omtBaseline struct {
	NotCompared []string          `json:"not_compared,omitempty"`
	Job         string            `json:"job"`
	Time        time.Time         `json:"time"`
	Kernel      string            `json:"kernel"`
	Tool        string            `json:"tool"`
	Status      map[string]string `json:"status"`              // check id -> pass, fail or skip
	Unclaimed   []string          `json:"unclaimed,omitempty"` // compatibles of the device-tree nodes no driver claimed
	// UnclaimedNodes counts those nodes per compatible. References from
	// before it was kept have only Unclaimed.
	UnclaimedNodes map[string]int `json:"unclaimed_nodes,omitempty"`
}

func (s *Server) omtBaselinePath(device string) string {
	return filepath.Join(s.cfg.DataDir, "baselines", device+".omt.json")
}

func (s *Server) omtBaseline(device string) *omtBaseline {
	b, err := os.ReadFile(s.omtBaselinePath(device))
	if err != nil {
		return nil
	}
	var base omtBaseline
	if json.Unmarshal(b, &base) != nil {
		return nil
	}
	return &base
}

// omtWanted says whether a job on this Mac runs omarchy-m-test without being asked:
// it needs a desktop user and an agent that carries the test.
func (s *Server) omtWanted(d api.Device, spec api.JobSpec) bool {
	return s.cfg.OMT && spec.Crash == "" && d.Facts.GUIUser != "" && versionAtLeast(d.Facts.AgentVersion, omtMinAgent)
}

func omtSpec() api.TestSpec {
	return api.TestSpec{Name: omtTest, Builtin: omtTest, GUI: true, TimeoutSec: 900}
}

// omtResult reads the report a finished omarchy-m-test left in the job,
// compares it with the Mac's known-good report, and records it as the new
// baseline when this run was on the known-good kernel. name is the test labd
// asked for, never the name the Mac answered with: the report is that
// test's, inside the job's own folder.
func (r *jobRun) omtResult(name string, tr *api.TestResult) {
	j := r.j
	file := omtReportFile(name)
	var data []byte
	err := errors.New("not the job's own file")
	if filepath.IsLocal(file) && !strings.ContainsAny(name, `/\`) {
		data, err = os.ReadFile(filepath.Join(r.s.jobDir(j.ID), file))
	}
	if err != nil {
		if tr.Passed {
			tr.Passed, tr.Error = false, "omarchy-m-test wrote no report"
		}
		return
	}
	var rep omtReport
	if err := json.Unmarshal(data, &rep); err != nil {
		tr.Passed, tr.Error = false, "omarchy-m-test report is not JSON: "+err.Error()
		return
	}
	sum := sha256.Sum256(data)
	res := &api.OMTResult{Report: file, ReportSHA256: hex.EncodeToString(sum[:]), Tool: rep.Tool.Version, Catalogue: rep.Catalogue, Kernel: rep.Machine.Kernel,
		KnownGood: j.Spec.Kernel == "" && j.Spec.Source == ""}
	if !r.bindMachineKey(data) {
		res.PublishNote = reports.AnotherMachineKey
	}
	status := map[string]string{}
	byID := map[string]api.OMTCheck{}
	for _, c := range rep.Checks {
		status[c.ID] = c.Status
		ev := ""
		if len(c.Evidence) > 0 {
			ev = c.Evidence[0]
		}
		chk := api.OMTCheck{ID: c.ID, Outcome: c.Classification.Outcome, Evidence: ev}
		byID[c.ID] = chk
		switch c.Status {
		case "pass":
			res.Pass++
		case "fail":
			res.Fail++
			res.Fails = append(res.Fails, chk)
		default:
			res.Skip++
		}
	}
	base := r.s.omtBaseline(j.Spec.Device)
	res.NotCompared = omtMissingCoverage(rep.Unattended, base, status)
	var unclaimed []string
	for _, u := range rep.Inventory.Unclaimed {
		unclaimed = append(unclaimed, u.Compatible)
	}
	sort.Strings(unclaimed)
	nodes := unclaimedNodes(rep)
	if r.ir != nil {
		// an installed run is judged against the frozen pre-install report and
		// never becomes a reference
		res.KnownGood = false
		r.ir.omt(tr, res, data)
		tr.Error += omtFrozenSkipNote(r.ir.c.omtUnattended, rep.Unattended, res.Regressions)
		r.s.updateJob(j, func(j *api.Job) { j.Result.OMT = res })
		r.ev("omarchy-m-test %s: %d pass, %d fail, %d skipped; %s vs %s", res.Tool, res.Pass, res.Fail, res.Skip, r.ir.res.OMT, res.ComparedTo)
		return
	}
	switch {
	case res.KnownGood:
		res.ComparedTo = "this is the known-good run: later runs are compared with it"
		if base != nil {
			res.Regressions, res.Fixed = omtDiff(base.Status, status, byID)
			nr, nf := omtNodes(base, status, rep)
			res.Regressions, res.Fixed = append(res.Regressions, nr...), append(res.Fixed, nf...)
			res.ComparedTo, res.BaselineJob = "the previous known-good run "+base.Job, base.Job
		}
	case base == nil:
		res.ComparedTo = "no known-good run of omarchy-m-test on this Mac yet (lab baseline makes one)"
	default:
		res.Regressions, res.Fixed = omtDiff(base.Status, status, byID)
		nr, nf := omtNodes(base, status, rep)
		res.Regressions, res.Fixed = append(res.Regressions, nr...), append(res.Fixed, nf...)
		kept := res.Regressions[:0]
		for _, c := range res.Regressions {
			if omtPackagingChecks[c.ID] {
				res.LabBoot = append(res.LabBoot, c)
			} else {
				kept = append(kept, c)
			}
		}
		res.Regressions = kept
		res.Regressions, res.Allowed = omtAllowed(res.Regressions, j.Spec.OMTAllow, rep)
		res.ComparedTo, res.BaselineJob = "known-good run "+base.Job+" on "+base.Kernel, base.Job
		if base.Tool != res.Tool {
			res.ComparedTo += fmt.Sprintf(" (omarchy-m-test %s then, %s now)", base.Tool, res.Tool)
		}
	}
	if len(res.NotCompared) > 0 {
		res.ComparedTo += "; coverage not run: " + strings.Join(res.NotCompared, ", ")
	}
	r.s.updateJob(j, func(j *api.Job) { j.Result.OMT = res })
	r.ev("omarchy-m-test %s: %d pass, %d fail, %d skipped; %d regressed, %d fixed vs %s",
		res.Tool, res.Pass, res.Fail, res.Skip, len(res.Regressions), len(res.Fixed), res.ComparedTo)
	var changed []string
	for _, c := range res.Regressions {
		changed = append(changed, c.ID+" regressed")
	}
	for _, c := range res.Fixed {
		changed = append(changed, c.ID+" fixed")
	}
	if len(changed) > 0 {
		r.ev("omarchy-m-test: %s", strings.Join(changed, ", "))
	}
	if j.Spec.Mesa != nil {
		res.Reference = "staged Mesa diagnostic: kept existing installed-desktop reference"
		r.s.updateJob(j, func(j *api.Job) { j.Result.OMT = res })
	} else if res.KnownGood {
		// A known-good kernel can't regress against itself in a way that blames
		// the kernel, so the test isn't failed. But a pass that turned into a
		// fail says something else changed (boot.bin, firmware, a cable), and a
		// run like that must not become the reference later runs are judged by.
		kg := r.v.snapshot().KnownGood
		switch {
		case j.Spec.Baseline:
			// lab baseline is how a person sets the references on purpose
			r.omtRef, _ = json.MarshalIndent(omtBaseline{Job: j.ID, Time: r.s.now(), Kernel: res.Kernel, Tool: res.Tool, Status: status,
				Unclaimed: unclaimed, UnclaimedNodes: nodes, NotCompared: res.NotCompared}, "", "  ")
			res.Reference = "a baseline: becomes the reference if it passes"
		case len(res.Regressions) > 0:
			res.Reference = fmt.Sprintf("kept %s: %s regressed since", base.Job, strings.Join(omtIDs(res.Regressions), ", "))
		case kg == "" || res.Kernel != kg:
			res.Reference = fmt.Sprintf("not a reference: ran on %s, the known-good kernel is %q", res.Kernel, kg)
		case j.Spec.Cmdline != "" || len(j.Spec.CmdlineStrip) > 0 || (j.Spec.CmdlineBase != "" && j.Spec.CmdlineBase != "known-good"):
			res.Reference = "not a reference: the job changed the known-good cmdline"
		default:
			r.omtRef, _ = json.MarshalIndent(omtBaseline{Job: j.ID, Time: r.s.now(), Kernel: res.Kernel, Tool: res.Tool, Status: status,
				Unclaimed: unclaimed, UnclaimedNodes: nodes, NotCompared: res.NotCompared}, "", "  ")
			res.Reference = "becomes the reference if the job passes"
		}
		r.s.updateJob(j, func(j *api.Job) { j.Result.OMT = res })
		return
	}
	if len(res.Regressions) > 0 {
		tr.Passed = false
		tr.Error = fmt.Sprintf("%d regressed from the known-good kernel: %s", len(res.Regressions), strings.Join(omtIDs(res.Regressions), ", "))
	}
}

// bindMachineKey records the Machine key that signed a collected report as
// the Mac's, when the Mac has none yet and the job is of its enrollment: the
// first report the lab collects binds it, published or not. It is false for
// a report signed by another key than the one recorded.
func (r *jobRun) bindMachineKey(report []byte) bool {
	if r.s.uploads == nil || r.v == nil {
		return true
	}
	id, ok := r.s.uploads.SignedBy(report)
	if !ok {
		return true
	}
	r.v.mu.Lock()
	defer r.v.mu.Unlock()
	if r.v.d.ID != r.j.DeviceID {
		return true
	}
	if r.v.d.MachineID == "" {
		r.v.d.MachineID = id
		r.v.saveLocked()
	}
	return r.v.d.MachineID == id
}

// omtIDs lists the ids of the checks.
func omtIDs(cs []api.OMTCheck) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	return ids
}

// omtAllowed moves the regressions a job said to expect out of the way. A
// plain check id allows that check; "hardware.drivers:<compatible>" allows the
// driver check only when every node left unbound is one of the named ones, and
// allows that node's own regression (omtNodes).
func omtAllowed(regressed []api.OMTCheck, allow []string, rep omtReport) (kept, allowed []api.OMTCheck) {
	ids, compat := map[string]bool{}, map[string]bool{}
	for _, a := range allow {
		if c, ok := strings.CutPrefix(a, "hardware.drivers:"); ok {
			compat[c] = true
		} else {
			ids[a] = true
		}
	}
	for _, c := range regressed {
		ok := ids[c.ID]
		if node, isNode := strings.CutPrefix(c.ID, "hardware.drivers:"); isNode {
			ok = ids["hardware.drivers"] || compat[node]
		} else if !ok && c.ID == "hardware.drivers" && len(compat) > 0 && len(rep.Inventory.Unclaimed) > 0 {
			ok = true
			for _, u := range rep.Inventory.Unclaimed {
				ok = ok && compat[u.Compatible]
			}
		}
		if ok {
			allowed = append(allowed, c)
		} else {
			kept = append(kept, c)
		}
	}
	return kept, allowed
}

// omtDiff lists the checks that went from pass to fail, and from fail to pass.
// Skips on either side compare as nothing: a skip says nothing about the kernel.
func omtDiff(before, after map[string]string, byID map[string]api.OMTCheck) (regressed, fixed []api.OMTCheck) {
	ids := make([]string, 0, len(after))
	for id := range after {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		switch {
		case before[id] == "pass" && after[id] == "fail":
			regressed = append(regressed, byID[id])
		case before[id] == "fail" && after[id] == "pass":
			fixed = append(fixed, byID[id])
		}
	}
	return
}

// omtNotRun lists the checks a reference passed that an unattended run skipped.
// omtDiff rightly calls them neither regressed nor fixed, so without this a
// lab kernel looks checked for what nothing ran on it. Missing coverage is
// carried by omtBaseline.NotCompared when the reference is replaced.
func omtNotRun(unattended bool, before, after map[string]string) []string {
	if !unattended {
		return nil
	}
	var ids []string
	for id, was := range before {
		if was == "pass" && after[id] == "skip" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// omtMissingCoverage retains missed reference coverage across known-good
// reference updates. On-screen video skipped by unattended mode is always
// visible to the release gate, including when the first reference skipped it.
func omtMissingCoverage(unattended bool, base *omtBaseline, after map[string]string) []string {
	missing := map[string]bool{}
	if base != nil {
		for _, id := range base.NotCompared {
			if after[id] != "pass" {
				missing[id] = true
			}
		}
		for _, id := range omtNotRun(unattended, base.Status, after) {
			missing[id] = true
		}
	}
	if unattended {
		for _, id := range []string{"video.h264-on-screen", "video.hevc-on-screen"} {
			if after[id] == "skip" {
				missing[id] = true
			}
		}
	}
	ids := make([]string, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// omtFrozenSkipNote names what an unattended Installed run regressed by
// skipping, and what to do: a frozen pass that is now a skip stays a regression
// (CompareOMT). Only a report frozen from an attended run can have run more than
// this mode does, so only then is taking the references again any use.
func omtFrozenSkipNote(frozenUnattended, unattended bool, regressed []api.OMTCheck) string {
	if !unattended {
		return ""
	}
	var ids []string
	for _, c := range regressed {
		if c.Outcome == "pass -> skip" {
			ids = append(ids, c.ID)
		}
	}
	switch {
	case len(ids) == 0:
		return ""
	case frozenUnattended:
		return "; this unattended run skipped what the unattended pre-install report passed: " + strings.Join(ids, ", ") +
			": see their evidence, and if the screen was locked or covered run it again"
	}
	return "; this unattended run skipped what the attended pre-install report passed: " + strings.Join(ids, ", ") +
		": if the screen was locked or covered run it again, otherwise take the preinstall references again"
}

// unclaimedNodes counts a report's unclaimed device-tree nodes per
// compatible. A report that gives no count lists each one as one node.
func unclaimedNodes(rep omtReport) map[string]int {
	n := map[string]int{}
	for _, u := range rep.Inventory.Unclaimed {
		n[u.Compatible] += max(u.Count, 1)
	}
	return n
}

// omtNodes compares the device-tree nodes no driver claims, compatible by
// compatible, when hardware.drivers fails on both runs and omtDiff sees no
// change. More nodes of a compatible unclaimed now than on the known-good run
// (none then, or 3 against 1) regress as "hardware.drivers:<compatible>";
// fewer are fixed. A reference from before the lab counted nodes has only
// their compatibles: then a compatible regresses only if it wasn't unclaimed
// at all, and is fixed only once none is.
func omtNodes(base *omtBaseline, status map[string]string, rep omtReport) (regressed, fixed []api.OMTCheck) {
	if base.Status["hardware.drivers"] != "fail" || status["hardware.drivers"] != "fail" ||
		len(base.Unclaimed) == 0 || len(rep.Inventory.Unclaimed) == 0 {
		return nil, nil
	}
	var evidence []string
	for _, c := range rep.Checks {
		if c.ID == "hardware.drivers" {
			evidence = c.Evidence
		}
	}
	counted := base.UnclaimedNodes != nil
	before := map[string]int{}
	for _, c := range base.Unclaimed {
		before[c] = 1
	}
	for c, n := range base.UnclaimedNodes {
		before[c] = n
	}
	now := unclaimedNodes(rep)
	seen := map[string]bool{}
	for _, u := range rep.Inventory.Unclaimed {
		c := u.Compatible
		if seen[c] {
			continue
		}
		seen[c] = true
		if b := before[c]; b > 0 && (!counted || now[c] <= b) {
			continue
		}
		chk := api.OMTCheck{ID: "hardware.drivers:" + c, Outcome: u.Outcome}
		for _, e := range evidence {
			if strings.HasPrefix(e, "unclaimed: "+c+" ") {
				chk.Evidence = e
			}
		}
		if b := before[c]; b > 0 {
			chk.Evidence = strings.TrimSuffix(fmt.Sprintf("%d nodes unclaimed, %d on the known-good kernel; %s", now[c], b, chk.Evidence), "; ")
		}
		regressed = append(regressed, chk)
	}
	seen = map[string]bool{}
	for _, c := range base.Unclaimed {
		if seen[c] {
			continue
		}
		seen[c] = true
		switch n, b := now[c], before[c]; {
		case n == 0:
			fixed = append(fixed, api.OMTCheck{ID: "hardware.drivers:" + c, Evidence: "claimed by a driver now"})
		case counted && n < b:
			fixed = append(fixed, api.OMTCheck{ID: "hardware.drivers:" + c, Evidence: fmt.Sprintf("%d of its %d nodes claimed by a driver now", b-n, b)})
		}
	}
	return regressed, fixed
}

// The lab publishing a job's report. It goes in by the way every report
// does, handed over inside labd (reports.Uploads.ReceiveFromLab): the same
// schema, signature, check ids and evidence, and the Mac's own ten an hour.
// The one signature on it is the one the Mac's Machine key made; the lab
// adds none, and records only how the report arrived.

// refreshPublishedEvery is how often the lab publishes a Mac's scheduled
// refresh by itself.
const refreshPublishedEvery = 7 * 24 * time.Hour

// publishing has one report published at a time: two asks for one job
// publish it once.
var publishing sync.Mutex

// publishOMT publishes a job's report, asked for by a person or, for the
// zero Person, by the lab itself. Only a Known-good run on a packaged kernel
// qualifies: a report's build is named from the installed packages, and a
// lab kernel isn't one, so it would be filed under the wrong kernel.
func (s *Server) publishOMT(j *api.Job, by Person) (string, error) {
	res := j.Result.OMT
	switch {
	case s.Preview():
		return "", errors.New("Omarchy Mac Lab is not open yet and publishes no report")
	case j.Spec.Mesa != nil:
		return "", errors.New("staged Mesa diagnostics are private and never published")
	case res == nil:
		return "", errors.New("this job has no omarchy-m-test report")
	case j.Spec.InstalledManifest != "":
		return "", errors.New("an Installed run is never published")
	}
	switch s.reportState(j) {
	case api.ReportPublished:
		return res.Published, nil
	case api.ReportHidden:
		return "", errors.New("its report is Hidden: a Maintainer shows it again on Reports")
	case api.ReportIsDeleted:
		return "", fmt.Errorf("its report was deleted%s, and a deleted report is not published again", deletedBy(res.ReportDeleted))
	}
	if !res.KnownGood {
		return "", fmt.Errorf("ran on test kernel %s, which the installed packages don't describe; only a Known-good run is published", res.Kernel)
	}
	if s.uploads == nil {
		return "", errors.New("this lab takes no reports: it was given no machine-id key")
	}
	// The Mac is asked as it is at this moment: still the enrollment that ran
	// the job, its Volunteer's switch, its known-good kernel.
	v := s.dev(j.Spec.Device)
	if v == nil {
		return "", fmt.Errorf("%s was forgotten (lab forget), so there is no known-good kernel to check the report against", j.Spec.Device)
	}
	d := v.snapshot()
	switch {
	case d.ID != j.DeviceID && j.DeviceID != "":
		return "", fmt.Errorf("%s has joined again since job %s ran: the report of an earlier enrollment stays private", d.Name, j.ID)
	case d.State == api.StateLeft:
		return "", fmt.Errorf("%s has left the lab: its unpublished reports stay private", d.Name)
	case d.IsVolunteer() && !d.Publish:
		return "", fmt.Errorf("publishing is off on %s, a Volunteer's Mac: only its Volunteer turns it on (sudo lab-agent publish on)", d.Name)
	}
	// A baseline makes the kernel it booted the known-good one only if it
	// passes; one that did not is compared with what it booted.
	if j.Spec.Baseline {
		if res.Kernel != j.Result.BootKernel {
			return "", fmt.Errorf("report names kernel %s but the baseline booted %s", res.Kernel, j.Result.BootKernel)
		}
	} else if kg := d.KnownGood; kg != "" && res.Kernel != kg {
		return "", fmt.Errorf("report names kernel %s but the known-good kernel is %s", res.Kernel, kg)
	}
	body, err := os.ReadFile(filepath.Join(s.jobDir(j.ID), res.Report))
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(body); res.ReportSHA256 != "" && hex.EncodeToString(sum[:]) != res.ReportSHA256 {
		return "", errors.New("the report in the job's files is not the one its verdict was computed from: it stays private")
	}
	if err := packagedOMTReport(body, res.Kernel); err != nil {
		return "", fmt.Errorf("report remains private: %w", err)
	}
	site := s.labAddress()
	stored, refusal, err := s.uploads.ReceiveFromLab(body, site+"/install", reports.Lab{Job: j.ID, Handle: by.Handle, AccountID: by.AccountID, Machine: d.MachineID})
	switch {
	case refusal != nil && refusal.Held != "":
		return "", fmt.Errorf("%s: %s/reports/%s", refusal.Error, site, refusal.Held)
	case refusal != nil:
		if err != nil {
			s.log.Printf("publishing the report of job %s: %v", j.ID, err)
		}
		msg := refusal.Error
		if len(refusal.Details) > 0 {
			msg += " (" + strings.Join(refusal.Details, "; ") + ")"
		}
		return "", errors.New(msg)
	}
	// The first report collected of a Mac recorded its machine id
	// (bindMachineKey); a job collected before then records it here. A
	// recorded one is never replaced.
	v.mu.Lock()
	if v.d.ID == j.DeviceID && v.d.MachineID == "" {
		v.d.MachineID = stored.MachineID
		v.saveLocked()
	}
	v.mu.Unlock()
	if d.IsVolunteer() {
		// The deletion link takes the report down again: `lab-agent publish
		// on` lists it at the Mac. It is kept beside the jobs, not in one.
		s.keepDeletionLink(j.ID, stored)
	}
	res.ReportID = stored.PublicID
	return site + "/reports/" + stored.PublicID, nil
}

func deletedBy(d *api.ReportDeleted) string {
	if d == nil {
		return ""
	}
	return fmt.Sprintf(" by %s on %s", d.By, d.Time.UTC().Format("2006-01-02"))
}

// omtPublish publishes a job's report and records the outcome on the job. A
// finished job is read again first, so that of two asks at once the second
// finds what the first did; a job still running is its runner's copy, the
// one the runner saves.
func (s *Server) omtPublish(j *api.Job, by Person) (string, error) {
	publishing.Lock()
	defer publishing.Unlock()
	if j.State == api.JobDone {
		if fresh, err := s.store.job(j.ID); err == nil {
			j = fresh
		}
	}
	was := ""
	if j.Result.OMT != nil {
		was = j.Result.OMT.ReportID
	}
	url, err := s.publishOMT(j, by)
	id := ""
	if j.Result.OMT != nil {
		id = j.Result.OMT.ReportID
	}
	s.updateJob(j, func(j *api.Job) {
		o := j.Result.OMT
		switch {
		case o == nil || was != "":
			// nothing to say, or the report it has is the answer
		case err != nil:
			o.PublishNote = err.Error()
		default:
			o.Published, o.ReportID, o.PublishNote = url, id, ""
		}
	})
	return url, err
}

// mayPublish is why p may not publish j's report by hand, or nil: a
// Maintainer any job's, an M team member their own job's.
func mayPublish(p Person, j *api.Job) error {
	switch {
	case !p.OnMTeam():
		return refuse(403, "publishing a job's report is for Maintainers, and for an M team member's own job")
	case p.isMaintainer():
		return nil
	case j.AccountID == 0 || j.AccountID != p.AccountID:
		return refuse(403, "an M team member publishes only their own job's report: job %s is %s. Ask a Maintainer", j.ID, orStr(prefixed("@", j.Maintainer), "the lab's own"))
	}
	return nil
}

// isRefresh reports whether labd's scheduler queued j: the refresh of a
// Mac's Reference.
func isRefresh(j *api.Job) bool {
	return j.AccountID == 0 && j.Maintainer == "" && j.Spec.Holder == omtHolder && !j.Spec.Baseline
}

// publishRun publishes the run's report once the job is over. By hand: the
// job asked for it (run --publish), and whoever submitted it is still a
// Maintainer or on the M team. By itself the lab publishes only a run that
// became the Mac's Reference: each baseline, and one refresh a week.
func (r *jobRun) publishRun(becameReference bool) {
	s, j := r.s, r.j
	if j.Result.OMT == nil {
		return
	}
	not := func(why string) {
		r.ev("omarchy-m-test report not published: %s", why)
		s.updateJob(j, func(j *api.Job) {
			if j.Result.OMT != nil && j.Result.OMT.ReportID == "" {
				j.Result.OMT.PublishNote = why
			}
		})
	}
	var by Person
	refresh := false
	switch {
	case j.Spec.Publish && j.AccountID != 0:
		p, err := s.person(j.AccountID)
		if err == nil {
			err = mayPublish(p, j)
		}
		if err != nil {
			not(err.Error())
			return
		}
		by = p
	case !s.cfg.OMTPublish:
		if j.Spec.Publish {
			not("this lab publishes no report by itself (labd --omt-publish)")
		}
		return
	case !j.Spec.Baseline && !isRefresh(j):
		if j.Spec.Publish {
			not("by itself the lab publishes only a baseline or a scheduled refresh")
		}
		return
	case !becameReference:
		not("by itself the lab publishes only a run that became the Mac's Reference")
		return
	case isRefresh(j):
		refresh = true
		if v := s.dev(j.Spec.Device); v != nil {
			if last := v.snapshot().RefreshPublished; !last.IsZero() && j.Created.Sub(last) < refreshPublishedEvery {
				not(fmt.Sprintf("by itself the lab publishes one refresh a week, and it published the one queued on %s", last.UTC().Format("2006-01-02")))
				return
			}
		}
	}
	url, err := s.omtPublish(j, by)
	if err != nil {
		r.ev("omarchy-m-test report not published: %v", err)
		return
	}
	r.ev("omarchy-m-test report published: %s", url)
	if v := s.dev(j.Spec.Device); refresh && v != nil {
		v.mu.Lock()
		v.d.RefreshPublished = j.Created
		v.saveLocked()
		v.mu.Unlock()
	}
}

// reportID is the id of the report j's run was published as, "" for none. A
// job the old lab published names only the report's page, which ends in it.
func reportID(o *api.OMTResult) string {
	if o.ReportID != "" || o.Published == "" {
		return o.ReportID
	}
	return o.Published[strings.LastIndex(o.Published, "/")+1:]
}

// reportState is what became of j's omarchy-m-test report, in one of four
// words; "" for a job that has no report.
func (s *Server) reportState(j *api.Job) string {
	o := j.Result.OMT
	if o == nil {
		return ""
	}
	id := reportID(o)
	switch {
	case o.ReportDeleted != nil:
		return api.ReportIsDeleted
	case id == "":
		return api.ReportNotPublished
	}
	report, err := s.reports.Report(id)
	switch {
	case errors.Is(err, reports.ErrNoReport):
		return api.ReportIsDeleted
	case err != nil:
		s.log.Printf("the report of job %s: %v", j.ID, err)
		return api.ReportPublished
	case report.Hidden:
		return api.ReportHidden
	}
	return api.ReportPublished
}

// withReport says on j what became of its report, for whoever reads the job.
func (s *Server) withReport(j *api.Job) *api.Job {
	if j != nil && j.Result.OMT != nil {
		j.Result.OMT.ReportState = s.reportState(j)
	}
	return j
}

// reportDeleted records on the job a deleted report was published from who
// deleted it, in words, and when. The link that deleted it is kept no more.
func (s *Server) reportDeleted(report *reports.Report, by string, accountID int64) {
	if !report.ByLab() {
		return
	}
	j, err := s.store.job(report.LabJob)
	if err != nil || j.Result.OMT == nil || j.Result.OMT.ReportID != report.PublicID {
		return
	}
	s.updateJob(j, func(j *api.Job) {
		j.Result.OMT.ReportDeleted = &api.ReportDeleted{By: by, AccountID: accountID, Time: s.now()}
	})
	os.Remove(s.deletionLinkPath(j.ID))
}

// keptLink is the deletion link of a Volunteer's Mac's published report.
type keptLink struct {
	ReportID      string `json:"report_id"`
	DeletionToken string `json:"deletion_token"`
}

func (s *Server) deletionLinkPath(job string) string {
	return filepath.Join(s.cfg.DataDir, "published", job+".json")
}

func (s *Server) keepDeletionLink(job string, stored *reports.Stored) {
	text, _ := json.Marshal(keptLink{ReportID: stored.PublicID, DeletionToken: stored.DeletionToken})
	path := s.deletionLinkPath(job)
	err := os.MkdirAll(filepath.Dir(path), 0o700)
	if err == nil {
		err = os.WriteFile(path, text, 0o600)
	}
	if err != nil {
		s.log.Printf("keeping the deletion link of job %s: %v", job, err)
	}
}

// publishedOf lists what the lab published from the jobs of a Mac's
// enrollment and has not been deleted, oldest first, each with its deletion
// link: at site. jobs are the jobs under the Mac's name, newest first.
func (s *Server) publishedOf(enrollment string, jobs []*api.Job, site string) []api.Published {
	out := []api.Published{}
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		o := j.Result.OMT
		// A name may have been another Volunteer's Mac before.
		if j.DeviceID != enrollment || o == nil || o.Published == "" || s.reportState(j) == api.ReportIsDeleted {
			continue
		}
		p := api.Published{Job: j.ID, ReportURL: o.Published}
		var kept keptLink
		if text, err := os.ReadFile(s.deletionLinkPath(j.ID)); err == nil && json.Unmarshal(text, &kept) == nil && kept.ReportID == o.ReportID {
			p.ReportURL = site + "/reports/" + kept.ReportID
			p.DeletionURL = p.ReportURL + "/deletion?token=" + kept.DeletionToken
		} else if text, err := os.ReadFile(filepath.Join(s.jobDir(j.ID), oldSiteLinks)); err == nil {
			// a job the old lab published on the old site
			var reply struct {
				DeletionURL string `json:"deletion_url"`
			}
			json.Unmarshal(text, &reply)
			p.DeletionURL = reply.DeletionURL
		}
		out = append(out, p)
	}
	return out
}

// oldSiteLinks is the file in a job's folder where the old lab kept what the
// old site answered an upload with, the deletion link among it. It is the
// one file of a job nobody is sent.
const oldSiteLinks = "omt-published.json"

// omtHolder holds the jobs omtScheduler queues.
const omtHolder = "maclab-omt"

// omtRefresh is the job omtScheduler queues on a Mac.
func omtRefresh(device string) api.JobSpec {
	return api.JobSpec{Device: device, Holder: omtHolder}
}

// omtScheduler re-runs omarchy-m-test on each Mac's known-good kernel once
// every OMTEvery: the Reference that lab kernels are compared with stays
// fresh. The run that becomes the Reference is the refresh the lab publishes
// by itself, once a week (publishRun).
func (s *Server) omtScheduler() {
	if s.cfg.OMTEvery <= 0 {
		return
	}
	t := s.clock().NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C():
		}
		for _, v := range s.allDevs() {
			d := v.snapshot()
			if d.State != api.StateReady || d.ActiveJob != "" || !s.omtWanted(d, api.JobSpec{}) {
				continue
			}
			if s.leased(v) {
				continue
			}
			spec := omtRefresh(d.Name)
			length := (&jobRun{s: s, v: v, j: &api.Job{Spec: spec}, boot: s.cfg.BootTimeout}).expectedLength()
			v.mu.Lock()
			held := v.windowHoldLocked(length) != ""
			v.mu.Unlock()
			if held {
				continue // a Volunteer's Mac is refreshed only when it fits in a window
			}
			if base := s.omtBaseline(d.Name); base != nil && s.since(base.Time) < s.cfg.OMTEvery {
				continue
			}
			if open, _ := s.store.jobs(d.Name, 1, true); len(open) > 0 {
				continue
			}
			j, _, err := s.Submit(spec)
			if err != nil {
				s.log.Printf("%s: scheduled omarchy-m-test: %v", d.Name, err)
				continue
			}
			s.log.Printf("%s: scheduled omarchy-m-test on the known-good kernel: job %s", d.Name, j.ID)
		}
	}
}

// versionAtLeast compares lab-agent's dotted numbers.
func versionAtLeast(have, want string) bool { return api.VersionAtLeast(have, want) }

// promoteOMT makes a known-good run the reference once the whole job passed:
// a failed test, an unhealthy boot or a failed restore keeps the old one. It
// reports whether the run became the reference.
func (r *jobRun) promoteOMT(outcome string) bool {
	j := r.j
	if outcome != api.OutcomePass {
		r.ev("omarchy-m-test reference kept: the job ended %s", outcome)
		r.s.updateJob(j, func(j *api.Job) {
			if j.Result.OMT != nil {
				j.Result.OMT.Reference = "kept the previous reference: the job ended " + outcome
			}
		})
		return false
	}
	if err := writeFileAtomic(r.s.omtBaselinePath(j.Spec.Device), r.omtRef); err != nil {
		r.ev("omarchy-m-test reference: %v", err)
		return false
	}
	r.ev("omarchy-m-test reference is now this run")
	r.s.updateJob(j, func(j *api.Job) {
		if j.Result.OMT != nil {
			j.Result.OMT.Reference = "became the reference"
		}
	})
	return true
}

// writeFileAtomic replaces path with data, never leaving a partial file, and
// syncs the file and its directory.
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// packagedOMTReport uses the tester's pacman ownership result, not a release
// name heuristic. Missing, skipped, duplicate or malformed identity fails
// closed without altering the local report or qualification reference.
func packagedOMTReport(body []byte, kernel string) error {
	var report omtReport
	if err := json.Unmarshal(body, &report); err != nil {
		return fmt.Errorf("invalid omarchy-m-test report: %w", err)
	}
	if kernel == "" || report.Machine.Kernel != kernel {
		return errors.New("report kernel does not match the tested kernel")
	}
	count, pass := 0, false
	for _, check := range report.Checks {
		if check.ID == "boot.kernel-package" {
			count++
			pass = check.Status == "pass"
		}
	}
	if count != 1 || !pass {
		return errors.New("running kernel has no unique passing boot.kernel-package ownership check")
	}
	return nil
}
