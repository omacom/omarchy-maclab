package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/client"
)

const mcpInstructions = `Omarchy Mac Lab: boot kernels on real Apple Silicon Macs running Omarchy and run tests on them.
Each tool is the lab command of its name: lab_status is "lab status", lab_installed_verdict is "lab installed verdict". You act as the person whose Lab token lab is logged in with, and the lab records what you do under their name.
Flow: lab_status -> lab_lease -> lab_run with source=<GitHub URL> (or a kernel artifact path) -> lab_wait until done -> read lab_job and lab_logs.
Each job stages the kernel beside the known-good one, boots it exactly once, runs the tests, collects logs, and puts the Mac back on its known-good kernel.
If a Mac crashes or hangs, the lab recovers it (panic reboot, watchdog, a Hard reset of a Lab-owned Mac) or tells a person exactly what to press; a Mac in state needs_hands is waiting for a person, so do not retry, tell the user.
Kernels are best given as source: a GitHub branch, commit or pull request URL. The lab builds it on its Builder with the target Mac's own config and reuses identical builds. Use lab_exec instead of ssh to run commands on a Mac: it needs your own Lease, and its text is kept for 12 months, so never put a secret in a command.
A tool never answers with a token: lab_enroll and lab_builder_add write theirs into a file the person names. Logging in is the person's: there is no tool for it.`

func serveMCP(ctx context.Context) error {
	c, err := client.Load()
	if err != nil {
		return err
	}
	return newMCP(c).Run(ctx, &mcp.StdioTransport{})
}

// newMCP is the server `lab mcp` runs: one tool for each command that asks
// the lab something, named after it.
func newMCP(c *client.Client) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "omarchy-mac-lab", Version: strings.TrimPrefix(version, "v")}, &mcp.ServerOptions{Instructions: mcpInstructions})
	who := "mcp:" + holder()

	text := func(v any) *mcp.CallToolResult {
		var t string
		if s, ok := v.(string); ok {
			t = s
		} else {
			b, _ := json.MarshalIndent(v, "", "  ")
			t = string(b)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: t}}}
	}
	// withAsked is a confirmed call's answer: what the lab asked, then what was done.
	withAsked := func(asked string, done any) *mcp.CallToolResult {
		b, _ := json.MarshalIndent(done, "", "  ")
		return text("The lab asked: " + asked + "\n\nConfirmed. Done:\n" + string(b))
	}

	type none struct{}
	type statusArg struct {
		Device string `json:"device,omitempty" jsonschema:"one Mac by name, e.g. m1-pro; omit for the fleet"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_status", Description: "The Macs: state (ready, busy, recovering, needs_hands, offline, new, left), recovery tier, running kernel, the Lease and when it ends, each Lab-owned Mac's cable (sighted, not sighted or no cable), and any action a person must take. With device: that Mac in full, with preflight problems, recent kernel events and, when it is free, how its last Lease ended."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a statusArg) (*mcp.CallToolResult, any, error) {
			if a.Device == "" {
				d, err := c.Devices(ctx)
				return text(d), nil, err
			}
			d, err := c.Device(ctx, a.Device)
			if err != nil {
				return nil, nil, err
			}
			if last := lastLeaseEnding(ctx, c, a.Device); d.Lease == nil && last != "" {
				return text(struct {
					*client.Device
					LastLease string `json:"last_lease"`
				}{d, last}), nil, nil
			}
			return text(d), nil, nil
		})

	type devArg struct {
		Device string `json:"device" jsonschema:"the Mac's name, e.g. m1-pro"`
	}
	type enrollArg struct {
		Name   string `json:"name" jsonschema:"the name the Lab-owned Mac will have"`
		Label  string `json:"label,omitempty" jsonschema:"where the Mac physically is"`
		Model  string `json:"model,omitempty" jsonschema:"laptop (default) or desktop"`
		Revoke bool   `json:"revoke,omitempty" jsonschema:"cancel the Join commands for this name instead of making one"`
		Out    string `json:"out,omitempty" jsonschema:"a new file to write the Join command into (mode 0600); required unless revoke. The command carries a token and is never part of the answer"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_enroll", Description: "Make the Join command for a Lab-owned Mac (Maintainers): one Mac, once, good for 1 hour. It is written into the file out, for the person to run on the Mac; the answer says only where it is and when it expires. A Volunteer's Mac is lent by its own Volunteer, signed in, from Lend a Mac on the site. Only when a person asks for it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a enrollArg) (*mcp.CallToolResult, any, error) {
			if a.Revoke {
				n, err := c.RevokeEnrollTokens(ctx, a.Name)
				return text(fmt.Sprintf("revoked %d Join command(s) for %s", n, a.Name)), nil, err
			}
			if a.Out == "" {
				return nil, nil, errors.New("name a new file in out: the Join command carries a token, and a tool never answers with one")
			}
			f, err := os.OpenFile(a.Out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return nil, nil, err
			}
			t, err := c.EnrollToken(ctx, api.EnrollTokenRequest{Name: a.Name, Label: a.Label, Model: cmpOr(a.Model, api.ModelLaptop)})
			if err != nil {
				f.Close()
				os.Remove(a.Out)
				return nil, nil, err
			}
			line, what := t.JoinCommand, "The Join command"
			if line == "" {
				line, what = t.Token, "Only the enrollment token ("+noJoinCommand+")"
			}
			_, err = f.WriteString(line + "\n")
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				// The Join command this call made reached no file: nobody can use it.
				os.Remove(a.Out)
				c.RevokeEnrollTokens(ctx, a.Name)
				return nil, nil, fmt.Errorf("writing the Join command to %s failed (%v); it was canceled", a.Out, err)
			}
			return text(fmt.Sprintf("%s for %s, a Lab-owned Mac, is in %s (mode 0600) and nowhere else. It joins one Mac, once, and expires %s. The person runs it in a terminal on the Mac, as themselves. Cancel it unused with lab_enroll revoke; then lab_baseline %s.",
				what, a.Name, a.Out, t.Expires.Format(time.DateTime), a.Name)), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_remove", Description: "End a Mac's enrollment (Maintainers): its device secret stops working at once, its record is Retired, and lab-agent removes the lab from the Mac at its next check-in; a Volunteer is told. Only when a person asks for it: it cannot be undone, the Mac has to Join again."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a devArg) (*mcp.CallToolResult, any, error) {
			if err := c.EndEnrollment(ctx, a.Device); err != nil {
				return nil, nil, err
			}
			return text(fmt.Sprintf("ended %s's enrollment; its device secret no longer works", a.Device)), nil, nil
		})
	type deviceArg struct {
		Device string  `json:"device" jsonschema:"the Mac's name"`
		Label  *string `json:"label,omitempty" jsonschema:"where the Mac physically is, for the instructions a person gets"`
		Model  *string `json:"model,omitempty" jsonschema:"laptop or desktop"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_device", Description: "Set a Mac's label and its model (Maintainers), and nothing else: whose a Mac is, its Configuration and its cable are not set here. Returns the Mac. To read a Mac use lab_status."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a deviceArg) (*mcp.CallToolResult, any, error) {
			d, err := c.PatchDevice(ctx, a.Device, api.DevicePatch{Label: a.Label, Model: a.Model})
			return text(d), nil, err
		})

	type runArg struct {
		Mesa              *api.MesaSpec `json:"mesa,omitempty" jsonschema:"optional first-class staged Mesa job: mode staged, exact split package artifact hashes; automatic provider probe; installed mode refused"`
		Device            string        `json:"device"`
		Source            string        `json:"source,omitempty" jsonschema:"build and boot this: a GitHub URL of a repo, branch (/tree/...), commit or pull request, or git URL#ref. Built with the Mac's own config; reused if built before"`
		KernelPath        string        `json:"kernel_path,omitempty" jsonschema:"local path of a kernel artifact to upload; omit to reboot on the current kernel"`
		KernelSHA256      string        `json:"kernel_sha256,omitempty" jsonschema:"sha256 of an artifact already uploaded"`
		BuildID           string        `json:"build_id,omitempty" jsonschema:"boot the kernel of a finished build, e.g. the linux package of a lab_package build"`
		Cmdline           string        `json:"cmdline,omitempty" jsonschema:"extra kernel command line arguments, appended to the base"`
		CmdlineBase       string        `json:"cmdline_base,omitempty" jsonschema:"cmdline the test boot starts from: known-good (default), default (the distro's stock KERNEL_CMDLINE[default]), or a literal cmdline with root="`
		CmdlineStrip      []string      `json:"cmdline_strip,omitempty" jsonschema:"globs of base parameters to drop, e.g. asahi.* apple_t6030_display.*"`
		VerboseBoot       bool          `json:"verbose_boot,omitempty" jsonschema:"boot with loglevel=7 and without quiet/splash; by default a test boot keeps the Mac's own"`
		InstalledManifest string        `json:"installed_manifest,omitempty" jsonschema:"sha256 of a frozen expected manifest: boot the INSTALLED release through its installer entry, check identity, compare with the frozen pre-install baseline, never promote (needs a lease)"`
		Tests             []string      `json:"tests,omitempty" jsonschema:"builtin tests: boot-health (always run), gui-smoke, gui-terminal, suspend-devices"`
		GUITests          []string      `json:"gui_tests,omitempty" jsonschema:"builtin tests to run inside the GUI user's Hyprland session: gui-smoke, gui-terminal"`
		Scripts           []string      `json:"scripts,omitempty" jsonschema:"local paths of shell scripts to run as tests (as root); write outputs to $MACLAB_OUT"`
		Payload           []string      `json:"payload,omitempty" jsonschema:"files for the scripts to read from $MACLAB_IN, each NAME=PATH (or NAME=sha256 of an uploaded file); given to every script, not to gui_scripts"`
		GUIScripts        []string      `json:"gui_scripts,omitempty" jsonschema:"local paths of shell scripts to run in the Wayland session"`
		BootTimeoutSec    int           `json:"boot_timeout_sec,omitempty"`
		ScriptTimeoutSec  int           `json:"script_timeout_sec,omitempty" jsonschema:"time each script test may run (default 300, at most 14400), e.g. 5400 for a hands-on session that waits for a person"`
		OMTAllow          []string      `json:"omt_allow,omitempty" jsonschema:"omarchy-m-test checks this kernel is expected to fail; hardware.drivers:<compatible> allows one unbound node"`
		Publish           bool          `json:"publish,omitempty" jsonschema:"publish the run's omarchy-m-test report on the lab, as asked by you; only a Known-good run qualifies, never a test kernel"`
		WaitSec           int           `json:"wait_sec,omitempty" jsonschema:"wait up to this long for the result (max 900); 0 returns once queued"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_run", Description: "Boot a kernel on a Mac once and run tests. Give source (a GitHub URL) to build it first, or a kernel artifact. Returns the job; use lab_wait for the result. An M team member needs their own Lease on the Mac first (lab_lease), and an Installed run is refused on a Volunteer's Mac."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a runArg) (*mcp.CallToolResult, any, error) {
			spec := api.JobSpec{Device: a.Device, Kernel: a.KernelSHA256, Source: a.Source, Cmdline: a.Cmdline, BootTimeoutSec: a.BootTimeoutSec, Holder: who, Publish: a.Publish, OMTAllow: a.OMTAllow,
				CmdlineBase: a.CmdlineBase, CmdlineStrip: a.CmdlineStrip, VerboseBoot: a.VerboseBoot, InstalledManifest: a.InstalledManifest, Mesa: a.Mesa}
			// Its uploads are for the Mac, as lab run's are: an M team member
			// uploads only for a job on a Mac they lease.
			up := *c
			up.UploadFor = a.Device
			if spec.Mesa != nil && spec.Mesa.Mode == "" {
				spec.Mesa.Mode = api.MesaStaged
			}
			if a.KernelPath != "" {
				sha, err := up.Upload(ctx, a.KernelPath)
				if err != nil {
					return nil, nil, err
				}
				spec.Kernel = sha
			}
			if a.BuildID != "" {
				b, err := c.GetBuild(ctx, a.BuildID)
				if err != nil {
					return nil, nil, err
				}
				if b.State != api.BuildDone || b.Artifact == "" {
					return nil, nil, fmt.Errorf("build %s is %s and has no kernel to boot", b.ID, b.State)
				}
				spec.Kernel = b.Artifact
			}
			for _, t := range a.Tests {
				spec.Tests = append(spec.Tests, api.TestSpec{Name: t, Builtin: t})
			}
			for _, t := range a.GUITests {
				spec.Tests = append(spec.Tests, api.TestSpec{Name: t, Builtin: t, GUI: true})
			}
			for i, list := range [][]string{a.Scripts, a.GUIScripts} {
				for _, p := range list {
					sha, err := up.Upload(ctx, p)
					if err != nil {
						return nil, nil, err
					}
					spec.Tests = append(spec.Tests, api.TestSpec{Name: strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)), Script: sha, GUI: i == 1, TimeoutSec: min(a.ScriptTimeoutSec, 14400)})
				}
			}
			files, err := payloadFiles(ctx, &up, a.Payload)
			if err != nil {
				return nil, nil, err
			}
			if spec.Tests, err = withPayload(spec.Tests, files); err != nil {
				return nil, nil, err
			}
			return submitAndWait(ctx, c, spec, a.WaitSec, text)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "lab_publish", Description: "Publish a finished job's omarchy-m-test report on the lab: a Maintainer any job's, an M team member their own job's. Only a Known-good run on a packaged kernel qualifies; a test-kernel run is refused because it would be filed under the wrong kernel, and an Installed run is never published. A repeat answers Already uploaded with the report's link."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a struct {
			JobID string `json:"job_id"`
		}) (*mcp.CallToolResult, any, error) {
			url, err := c.Publish(ctx, a.JobID)
			return text(url), nil, err
		})

	type waitDev struct {
		Device  string `json:"device"`
		WaitSec int    `json:"wait_sec,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_baseline", Description: "Baseline a newly enrolled Mac (Maintainers): proves the one-shot boot selects and clears, records baseline dmesg. Required once before other jobs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a waitDev) (*mcp.CallToolResult, any, error) {
			return submitAndWait(ctx, c, api.JobSpec{Device: a.Device, Baseline: true, Holder: who}, a.WaitSec, text)
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_crashtest", Description: "Deliberately panic a Mac and verify it recovers by itself."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a waitDev) (*mcp.CallToolResult, any, error) {
			return submitAndWait(ctx, c, api.JobSpec{Device: a.Device, Crash: "panic", Holder: who}, a.WaitSec, text)
		})

	type jobArg struct {
		JobID string `json:"job_id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_job", Description: "A job's state, outcome, summary, boot and test results, recovery steps, new kernel errors vs baseline, log file names and, under omt, its report's state: not published, published, hidden or deleted."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a jobArg) (*mcp.CallToolResult, any, error) {
			j, err := c.Job(ctx, a.JobID)
			return text(j), nil, err
		})
	type waitArg struct {
		JobID      string `json:"job_id"`
		TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"max 900, default 600"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_wait", Description: "Wait for a job to finish (or the timeout) and return it. Call again if state is not done."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a waitArg) (*mcp.CallToolResult, any, error) {
			t := a.TimeoutSec
			if t <= 0 || t > 900 {
				t = 600
			}
			j, err := c.Wait(ctx, a.JobID, time.Duration(t)*time.Second)
			return text(j), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_cancel", Description: "Cancel a job. A running job stops and the Mac is restored to its known-good kernel. An M team member cancels only their own jobs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a jobArg) (*mcp.CallToolResult, any, error) {
			out, err := c.Cancel(ctx, a.JobID)
			return text(out), nil, err
		})
	type retryArg struct {
		JobID   string `json:"job_id"`
		WaitSec int    `json:"wait_sec,omitempty" jsonschema:"wait up to this long for the result (max 900); 0 returns once queued"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_retry", Description: "Run a finished job again as a new job: same Mac, kernel (for a source job, the same commit), tests and cmdline. A stage_failed job's summary says why, and whether labd already tried it again (up to 3 attempts); retry it once that cause is fixed. Returns the new job; use lab_wait for the result."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a retryArg) (*mcp.CallToolResult, any, error) {
			j, err := c.Retry(ctx, a.JobID, who)
			if err != nil {
				return nil, nil, err
			}
			return waitJob(ctx, c, j, a.WaitSec, text)
		})
	type jobsArg struct {
		Device string `json:"device,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_jobs", Description: "Recent jobs, newest first, each with its report's state when it has one: not published, published, hidden or deleted."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a jobsArg) (*mcp.CallToolResult, any, error) {
			jobs, err := c.Jobs(ctx, a.Device, 20)
			if err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			for _, j := range jobs {
				fmt.Fprintf(&b, "%s %s %s %s: %s", j.ID, j.Spec.Device, j.State, j.Outcome, j.Summary)
				if state := reportState(j.Result.OMT); state != "" {
					fmt.Fprintf(&b, " (report: %s)", state)
				}
				b.WriteString("\n")
			}
			return text(b.String()), nil, nil
		})

	type gateArg struct {
		Kernel     string   `json:"kernel" jsonschema:"the candidate kernel: a build id, or its artifact sha256 or a prefix of it"`
		Families   []string `json:"families,omitempty" jsonschema:"chip families that must pass (default all: m1, m1-pro, m2, m2-pro, m3, m3-pro)"`
		Volunteers bool     `json:"volunteers,omitempty" jsonschema:"count Volunteers' Macs too; by default their runs are listed under not_counted"`
		Exclude    []string `json:"exclude,omitempty" jsonschema:"job ids of this kernel to set aside for this report only, e.g. a failure the lab caused (an access point outage reads as crashed_or_hung); the report lists them in excluded, and in not_counted marked excluded. Nothing is kept: lab_exclude sets a job aside for every report"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_gate", Description: "Release gate: whether a kernel passed on real hardware for every required chip family, from each Mac's plain runs on it: the known-good cmdline, at least the tests every job on that Mac runs, no omt_allow, and, where the Mac runs omarchy-m-test, a comparison with its known-good run (a pass with no OMT reference is not counted). Each job is judged by its Mac as it was when the job was submitted (chip family, volunteer, omarchy-m-test); a Mac the lab no longer has (lab remove) is listed as device gone, not counted. A run with a changed cmdline counts neither way; one that tested less counts only if it failed. Canceled, infra_error and crash-test runs, and tests_failed runs whose failed tests never started (bash couldn't open the test script: exit 126 Permission denied or 127 No such file or directory for test-<name>.sh), say nothing; a test that ran and exited 126 or 127 itself is a failure. A job canceled after its kernel had already failed keeps that failure as its outcome. A Mac fails if it failed, passes if it passed, and is flaky (not a pass) if it did both, so a retry's pass doesn't hide a failure (retry_of links a retry to the job it retries); a Mac that couldn't stage the kernel counts only when it has neither. A family's result is one of pass, fail, flaky, stage_failed or untested: fail if any Mac failed, flaky if any is flaky, pass if one passed, stage_failed if its only results are Macs that couldn't stage the kernel (each such run gives the job's reason; run it again once that's fixed), and untested if none has a result. Anything checking results for \"fail\" must treat \"flaky\" as not passing too. Runs that don't count are in each family's not_counted with the reason (and the report's not_counted when no family is known); volunteers' Macs are there too unless volunteers is set, and excluded jobs, marked excluded. Jobs set aside with lab_exclude are excluded in every report: exclusions lists them with who set each aside, why and when, and their not_counted runs carry the same as exclusion. result is pass only when every required family passed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a gateArg) (*mcp.CallToolResult, any, error) {
			r, err := c.Gate(ctx, a.Kernel, a.Families, a.Volunteers, a.Exclude)
			return text(r), nil, err
		})

	type excludeArg struct {
		JobID  string `json:"job_id"`
		Reason string `json:"reason,omitempty" jsonschema:"why the job says nothing about its kernel, e.g. the access point went down mid-test; required unless clear"`
		Clear  bool   `json:"clear,omitempty" jsonschema:"count the job in lab_gate again"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_exclude", Description: "Set a finished job aside in every lab_gate report until cleared, say a failure the lab caused (an access point outage reads as crashed_or_hung, which would keep its family flaky for good). A Maintainer's, with a reason; the gate lists the job in exclusions with who set it aside, why and when. Only for a failure that isn't the kernel's: never to make a gate pass. clear counts the job again. Returns the job."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a excludeArg) (*mcp.CallToolResult, any, error) {
			if a.Clear {
				j, err := c.ClearExclusion(ctx, a.JobID)
				return text(j), nil, err
			}
			if strings.TrimSpace(a.Reason) == "" {
				return nil, nil, errors.New("give a reason: why the job says nothing about its kernel")
			}
			j, err := c.Exclude(ctx, a.JobID, a.Reason, who)
			return text(j), nil, err
		})

	type logsArg struct {
		JobID     string `json:"job_id"`
		File      string `json:"file,omitempty" jsonschema:"a name from the job's logs or test files, e.g. boot0/kernel.txt, prevboot/journal.txt, test-boot-health/log.txt, test-gui-smoke/screenshot.png; omit to list the names"`
		TailLines int    `json:"tail_lines,omitempty" jsonschema:"only the last N lines of a text file (default 400)"`
		Grep      string `json:"grep,omitempty" jsonschema:"only lines containing this substring"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_logs", Description: "A job's files. With no file: their names. With a file: a text log, by default its last 400 lines, or a picture a GUI test saved (PNG or JPEG), shown as an image."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a logsArg) (*mcp.CallToolResult, any, error) {
			if a.File == "" {
				j, err := c.Job(ctx, a.JobID)
				if err != nil {
					return nil, nil, err
				}
				names := append([]string{}, j.Result.Logs...)
				for _, t := range j.Result.Tests {
					names = append(names, t.Files...)
				}
				return text(strings.Join(names, "\n")), nil, nil
			}
			b, err := c.File(ctx, a.JobID, a.File)
			if err != nil {
				return nil, nil, err
			}
			switch path.Ext(a.File) {
			case ".png":
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: b, MIMEType: "image/png"}}}, nil, nil
			case ".jpg", ".jpeg":
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: b, MIMEType: "image/jpeg"}}}, nil, nil
			}
			lines := strings.Split(string(b), "\n")
			if a.Grep != "" {
				var keep []string
				for _, l := range lines {
					if strings.Contains(l, a.Grep) {
						keep = append(keep, l)
					}
				}
				lines = keep
			}
			n := a.TailLines
			if n <= 0 {
				n = 400
			}
			if len(lines) > n {
				lines = append([]string{fmt.Sprintf("[... %d earlier lines]", len(lines)-n)}, lines[len(lines)-n:]...)
			}
			return text(strings.Join(lines, "\n")), nil, nil
		})

	type leaseArg struct {
		Device     string `json:"device"`
		TTLMinutes int    `json:"ttl_minutes,omitempty" jsonschema:"default 240, the lab's 4 hours; a Maintainer may ask for up to 1440"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_lease", Description: "Take a Lease on a Mac so only your jobs run on it until it ends, 4 hours by default: others' new jobs are refused and their queued ones wait. A job already running finishes first. Calling it again renews the Lease: it ends 4 hours from then. A 409 saying a Mac is leased by someone else means wait for it to end or use another Mac; one naming a Configuration already held means an M team member holds one Mac of each Configuration. A warning in the answer is for the person to read."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a leaseArg) (*mcp.CallToolResult, any, error) {
			l, err := c.Lease(ctx, a.Device, who, time.Duration(max(a.TTLMinutes, 0))*time.Minute)
			return text(l), nil, err
		})
	type releaseArg struct {
		Device string `json:"device"`
		Force  bool   `json:"force,omitempty" jsonschema:"end another person's Lease, which only a Maintainer may; the lab records who ended it. Only when a person asks for it, never to get a Mac sooner"`
		Why    string `json:"why,omitempty" jsonschema:"with force: the person's reason, one line, kept in the Lease record and said in the Lab group"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_release", Description: "Release your Lease on a Mac; only the person who took a Lease releases it, unless a Maintainer uses force, which ends anyone's and is recorded."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a releaseArg) (*mcp.CallToolResult, any, error) {
			if a.Force {
				return text("released"), nil, c.ForceRelease(ctx, a.Device, a.Why)
			}
			return text("released"), nil, c.Release(ctx, a.Device, who)
		})
	type resetArg struct {
		Device string `json:"device"`
		Prove  string `json:"prove,omitempty" jsonschema:"a Proving reset (Maintainers only): reset this port of the lab server, left, middle or right, to place a Mac the lab has no Sighting of. Only when a person asks for it: it restarts whatever Mac is on that port"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_reset", Description: "Hard reset a Lab-owned Mac that is wedged or offline, through its cable to the lab server: a Maintainer, or the holder of a Lease on it. Refused for a Mac the lab cannot place (lab_oob and lab_status say why, under cable) and for a Volunteer's Mac. If the Mac does not return, the lab sends one more and then asks a person. A Mac that left the lab (lab-agent uninstall) goes back in it first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a resetArg) (*mcp.CallToolResult, any, error) {
			if a.Prove != "" {
				p, err := c.ProvingReset(ctx, a.Device, a.Prove)
				return text(p), nil, err
			}
			out, err := c.Reset(ctx, a.Device, who)
			return text(out), nil, err
		})
	type buildArg struct {
		Source     string `json:"source" jsonschema:"GitHub URL of a repo, branch, commit or pull request (or git URL#ref)"`
		Device     string `json:"device,omitempty" jsonschema:"build with this Mac's running kernel config"`
		ConfigPath string `json:"config_path,omitempty" jsonschema:"or: local path of a .config to build with; the result boots on every Mac"`
		Force      bool   `json:"force,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_build", Description: "Build a kernel from source without booting it. Identical builds (same commit and config) are reused. Returns the build; poll lab_build_log. A source off the lab's list of hosts, or one that carries credentials, is refused; an M team member's build is refused until M team builds open."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a buildArg) (*mcp.CallToolResult, any, error) {
			req := api.BuildRequest{Source: a.Source, Device: a.Device, Force: a.Force}
			if a.ConfigPath != "" {
				sha, err := c.Upload(ctx, a.ConfigPath)
				if err != nil {
					return nil, nil, err
				}
				req.Config, req.ConfigName, req.Device = sha, filepath.Base(a.ConfigPath), ""
			}
			b, err := c.Build(ctx, req)
			return text(b), nil, err
		})
	type packageArg struct {
		RecipeDir string `json:"recipe_dir" jsonschema:"local directory holding the PKGBUILD and its local sources (config, patches, rust-toolchain.toml...)"`
		Commit    string `json:"commit,omitempty" jsonschema:"point the recipe's _commit at this GitHub commit, branch or PR URL; source checksums are refreshed"`
		Pkgrel    string `json:"pkgrel,omitempty" jsonschema:"set pkgrel, e.g. 11.14"`
		Force     bool   `json:"force,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_package", Description: "Build release packages (e.g. linux-aurora and linux-aurora-headers) from a PKGBUILD recipe with makepkg on Arch Linux ARM, the way the release machine does. Takes 15-30 minutes. Poll lab_build_log; the build's files list names and sha256s every package. Boot the kernel package once with lab_run build_id; download with lab_build_get."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a packageArg) (*mcp.CallToolResult, any, error) {
			sha, _, err := uploadRecipe(ctx, c, a.RecipeDir)
			if err != nil {
				return nil, nil, err
			}
			abs, _ := filepath.Abs(a.RecipeDir)
			b, err := c.Build(ctx, api.BuildRequest{Recipe: sha, RecipeDir: abs, Source: a.Commit, Pkgrel: a.Pkgrel, Force: a.Force})
			return text(b), nil, err
		})
	type buildID struct {
		BuildID   string `json:"build_id"`
		TailLines int    `json:"tail_lines,omitempty" jsonschema:"include the last N log lines (default 30)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_builds", Description: "The 20 newest builds, kernels and packages: state, source, commit, who asked, and the files of each."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			l, err := c.Builds(ctx, 20)
			return text(l), nil, err
		})
	type buildGetArg struct {
		BuildID string `json:"build_id"`
		Dir     string `json:"dir" jsonschema:"local directory to download into; made if missing"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_build_get", Description: "Download a build's named files into a local directory, each checked against the sha256 the build recorded; a package build's recipe comes with them, under recipe/. A cut download resumes when asked again."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a buildGetArg) (*mcp.CallToolResult, any, error) {
			if a.Dir == "" {
				return nil, nil, errors.New("name the directory to download into in dir")
			}
			b, err := c.GetBuild(ctx, a.BuildID)
			if err != nil {
				return nil, nil, err
			}
			var out bytes.Buffer
			err = getBuildFiles(ctx, c, b, a.Dir, &out)
			return text(out.String()), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_build_log", Description: "A build's state, stage, progress, ETA and the tail of its log. Call again while state is not done or failed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a buildID) (*mcp.CallToolResult, any, error) {
			b, err := c.GetBuild(ctx, a.BuildID)
			if err != nil {
				return nil, nil, err
			}
			var log strings.Builder
			c.BuildLog(ctx, a.BuildID, false, &log)
			lines := strings.Split(strings.TrimRight(log.String(), "\n"), "\n")
			n := a.TailLines
			if n <= 0 {
				n = 30
			}
			if len(lines) > n {
				lines = lines[len(lines)-n:]
			}
			j, _ := json.MarshalIndent(b, "", "  ")
			return text(string(j) + "\n\nlog tail:\n" + strings.Join(lines, "\n")), nil, nil
		})
	type execArg struct {
		Device     string `json:"device"`
		Command    string `json:"command" jsonschema:"shell command, run as root with bash -c"`
		TimeoutSec int    `json:"timeout_sec,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_exec", Description: "Run a shell command on a Mac as root, through lab-agent, and get stdout, stderr and the exit code. Use this instead of ssh. It needs your own Lease on the Mac (lab_lease), a Maintainer's too. The command's text is kept in the Exec record for 12 months: never put a secret in a command."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a execArg) (*mcp.CallToolResult, any, error) {
			r, err := c.Exec(ctx, a.Device, api.ExecArgs{Command: a.Command, TimeoutSec: a.TimeoutSec})
			return text(r), nil, err
		})

	mcp.AddTool(s, &mcp.Tool{Name: "lab_oob", Description: "A Lab-owned Mac's cable to the lab server: sighted (with the port), not sighted or no cable, since when, whether a Hard reset can reach it, and what each of the three ports shows. A Volunteer's Mac has no cable."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a devArg) (*mcp.CallToolResult, any, error) {
			st, err := c.Cable(ctx, a.Device)
			if err != nil {
				return nil, nil, err
			}
			var out bytes.Buffer
			showCable(&out, st)
			return text(out.String()), nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_audit", Description: "The lab's copy of a Volunteer's Mac's Audit log: every Maintainer action on it, one line each."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a devArg) (*mcp.CallToolResult, any, error) {
			es, err := c.Audit(ctx, a.Device)
			if err != nil {
				return nil, nil, err
			}
			var out strings.Builder
			for _, e := range es {
				out.WriteString(e.Line(time.Local) + "\n")
			}
			return text(out.String()), nil, nil
		})
	type uploadArg struct {
		Path string `json:"path" jsonschema:"local file to upload"`
		For  string `json:"for,omitempty" jsonschema:"the Mac the file is for; an M team member uploads only for a job on a Mac they lease"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_upload", Description: "Upload a local file to the lab's artifact store, in 32 MiB parts, and return its sha256, which lab_run takes as kernel_sha256 or in payload. Cut, the same call resumes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a uploadArg) (*mcp.CallToolResult, any, error) {
			up := *c
			up.UploadFor = a.For
			sha, err := up.Upload(ctx, a.Path)
			return text(sha), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_agent_update", Description: "Update a Lab-owned Mac's lab-agent to the lab's, while the Mac is idle (Maintainers). Idle is no job running or waiting and no Lease. The Mac checks the new agent's signature itself and refuses an older one; it needs lab-agent 0.10.0 or later, the first that updates itself. A Volunteer's Mac is never sent one: its Volunteer runs sudo lab-agent update at the Mac."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a devArg) (*mcp.CallToolResult, any, error) {
			res, err := c.AgentUpdate(ctx, a.Device)
			return text(res), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_version", Description: "The Lab release this lab came from, and the one Omarchy Mac Lab runs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			who, err := c.WhoAmI(ctx)
			if err != nil {
				return nil, nil, err
			}
			return text(fmt.Sprintf("lab %s\nOmarchy Mac Lab %s", version, orDash(who.LabRelease, "did not say which release it runs"))), nil, nil
		})

	type releaseOnly struct {
		Release string `json:"release" jsonschema:"the Release's name, e.g. 12.0"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_installed_list", Description: "Every Release with its verdict (qualified, not qualified, revoked or none), who froze it, how many Configurations have a passed Installed run, and its Macs."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			all, err := c.Releases(ctx)
			return text(all), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_installed_status", Description: "One Release: its Macs with Configuration, runs and Promotion, its verdict history, and its frozen files."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a releaseOnly) (*mcp.CallToolResult, any, error) {
			var out map[string]any
			err := c.Installed(ctx, "GET", a.Release, "", nil, &out)
			return text(out), nil, err
		})
	type freezeManifestArg struct {
		Release      string   `json:"release"`
		ManifestPath string   `json:"manifest_path" jsonschema:"local path of the expected manifest (JSON)"`
		PayloadPath  string   `json:"payload_path" jsonschema:"local path of the payload's .sha256 file"`
		ApprovedBy   []string `json:"approved_by,omitempty" jsonschema:"who approved the manifest"`
		For          string   `json:"for,omitempty" jsonschema:"as an M team member: one of the Release's Macs, which you lease"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_installed_freeze_manifest", Description: "Freeze a Release's expected manifest and payload digest: from then the Release is listed, with the person as who froze it. An M team member holds a Lease on one of the Release's Macs and names it in for. Only when a person asks for it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a freezeManifestArg) (*mcp.CallToolResult, any, error) {
			up := *c
			up.UploadFor = a.For
			m, err1 := up.Upload(ctx, a.ManifestPath)
			p, err2 := up.Upload(ctx, a.PayloadPath)
			if err1 != nil || err2 != nil {
				return nil, nil, errors.Join(err1, err2)
			}
			var out map[string]any
			err := c.Installed(ctx, "POST", a.Release, "manifest", map[string]any{"manifest": m, "payload": p, "approved_by": a.ApprovedBy}, &out)
			return text(out), nil, err
		})
	type freezeEntryArg struct {
		Device  string `json:"device"`
		Release string `json:"release"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_installed_freeze_entry", Description: "Freeze the installer's boot entry of a Mac, after the install: the lab reads the Mac through its read-only identity check, not an exec. Needs your own Lease on the Mac; refused outside the Availability window and on a Volunteer's Mac."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a freezeEntryArg) (*mcp.CallToolResult, any, error) {
			var out map[string]any
			err := c.Installed(ctx, "POST", a.Release, "entry/"+a.Device, nil, &out)
			return text(out), nil, err
		})
	const askFirst = "\n\nNothing was done. Show this to the person, and call again with confirm set only when they say yes."
	// confirm in the first call is lab's --yes: the lab is still asked, and
	// what it said is answered beside what was done.
	const oneCall = " With confirm set in one call it goes on at once, as `lab installed %s --yes` does: only when the person has already said yes to exactly this. The answer then starts with what the lab asked, warnings and all."
	type verdictArg struct {
		Release string `json:"release"`
		Verdict string `json:"verdict" jsonschema:"qualified, not-qualified, or revoke (Maintainers)"`
		Note    string `json:"note" jsonschema:"why, in the person's words: a verdict needs a note every time"`
		Confirm bool   `json:"confirm,omitempty" jsonschema:"true goes on: after the person read what a first call answered and said yes, or in one call when they already said yes to exactly this, as lab's --yes"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_installed_verdict", Description: "Record a Release's verdict: the first is any M team member's, every later one and a revoke a Maintainer's. qualified needs a passed Installed run of the Release. The verdict is the person's judgment, never yours: without confirm the lab only answers with what it is about to record and its warnings, for the person to read." + fmt.Sprintf(oneCall, "verdict")},
		func(ctx context.Context, _ *mcp.CallToolRequest, a verdictArg) (*mcp.CallToolResult, any, error) {
			req := api.VerdictRequest{Note: a.Note}
			switch a.Verdict {
			case "qualified":
				req.State = api.ReleaseQualified
			case "not-qualified":
				req.State = api.ReleaseNotQualified
			case "revoke":
				req.State = api.ReleaseRevoked
			default:
				return nil, nil, errors.New("verdict is qualified, not-qualified or revoke")
			}
			var ans api.ReleaseAnswer
			if err := c.Installed(ctx, "POST", a.Release, "verdict", req, &ans); err != nil {
				return nil, nil, err
			}
			if !a.Confirm {
				return text(ans.Confirm + askFirst), nil, nil
			}
			req.Confirm = true
			var done api.ReleaseAnswer
			if err := c.Installed(ctx, "POST", a.Release, "verdict", req, &done); err != nil {
				return nil, nil, err
			}
			return withAsked(ans.Confirm, done.Release), nil, nil
		})
	type promoteArg struct {
		Release string `json:"release"`
		Device  string `json:"device"`
		JobID   string `json:"job_id" jsonschema:"that Mac's own passed Installed run of the Release"`
		Confirm bool   `json:"confirm,omitempty" jsonschema:"true goes on: after the person read what a first call answered and said yes, or in one call when they already said yes to exactly this, as lab's --yes"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_installed_promote", Description: "Promotion (Maintainers): make a Mac's passed Installed run its references and known-good, the lab's and the agent's. Refused on a Volunteer's Mac. Without confirm the lab only answers with what it is about to do and its warnings, for the person to read." + fmt.Sprintf(oneCall, "promote")},
		func(ctx context.Context, _ *mcp.CallToolRequest, a promoteArg) (*mcp.CallToolResult, any, error) {
			ask := map[string]any{"device": a.Device, "job": a.JobID}
			var asked struct {
				Confirm string `json:"confirm"`
			}
			if err := c.Installed(ctx, "POST", a.Release, "promote", ask, &asked); err != nil {
				return nil, nil, err
			}
			if !a.Confirm {
				return text(asked.Confirm + askFirst), nil, nil
			}
			ask["confirm"] = true
			var out map[string]any
			if err := c.Installed(ctx, "POST", a.Release, "promote", ask, &out); err != nil {
				return nil, nil, err
			}
			return withAsked(asked.Confirm, out), nil, nil
		})

	type builderAddArg struct {
		Name  string   `json:"name"`
		Kinds []string `json:"kinds,omitempty" jsonschema:"the builds it may take: kernel (default), package, or both"`
		Out   string   `json:"out" jsonschema:"a new file to write the builder's token into (mode 0600). The token is never part of the answer"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_builder_add", Description: "Add a named builder with a token of its own, which may only build (Maintainers). The token is written into the file out and nowhere else. A builder's kernels boot as root on the lab's Macs: only when a person asks for it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a builderAddArg) (*mcp.CallToolResult, any, error) {
			if a.Out == "" {
				return nil, nil, errors.New("name a new file in out: a tool never answers with a token")
			}
			if len(a.Kinds) == 0 {
				a.Kinds = []string{api.BuilderKernel}
			}
			var out bytes.Buffer
			err := addBuilder(ctx, c, a.Name, a.Kinds, a.Out, &out)
			return text(out.String()), nil, err
		})
	type builderRemoveArg struct {
		Name       string `json:"name"`
		Generation string `json:"generation,omitempty" jsonschema:"remove it only while it is this generation"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "lab_builder_remove", Description: "Remove a builder (Maintainers): its token stops working and the builds it was running fail. Only when a person asks for it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a builderRemoveArg) (*mcp.CallToolResult, any, error) {
			r, err := c.RemoveBuilderGeneration(ctx, a.Name, a.Generation)
			return text(r), nil, err
		})
	mcp.AddTool(s, &mcp.Tool{Name: "lab_builder_list", Description: "The builders: the kinds of build each takes, who added it and when, when it was last seen, and the build it is running."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ none) (*mcp.CallToolResult, any, error) {
			l, err := c.Builders(ctx)
			return text(l), nil, err
		})

	return s
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func submitAndWait(ctx context.Context, c *client.Client, spec api.JobSpec, waitSec int, text func(any) *mcp.CallToolResult) (*mcp.CallToolResult, any, error) {
	j, err := c.Submit(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	return waitJob(ctx, c, j, waitSec, text)
}

// waitJob waits up to waitSec (at most 900) for a job to finish, and returns it.
func waitJob(ctx context.Context, c *client.Client, j *api.Job, waitSec int, text func(any) *mcp.CallToolResult) (*mcp.CallToolResult, any, error) {
	var err error
	if waitSec > 0 {
		if waitSec > 900 {
			waitSec = 900
		}
		if j, err = c.Wait(ctx, j.ID, time.Duration(waitSec)*time.Second); err != nil {
			return nil, nil, err
		}
	}
	return text(j), nil, nil
}
