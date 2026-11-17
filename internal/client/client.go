// Package client is the Go client for Omarchy Mac Lab's API, used by the lab
// CLI and its MCP server. login.go has the one login it keeps and which lab
// a call goes to.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/upload"
)

type Client struct {
	URL   string `json:"url"`
	Token string `json:"token"`
	// CAFile is a PEM file of extra CA certificates for an https labd whose
	// certificate the system doesn't trust: a private CA, or labd's own
	// self-signed certificate (lab login --ca-file, MACLAB_CA_FILE).
	CAFile string `json:"ca_file,omitempty"`
	// AllowPlainHTTP lets an http:// URL reach labd on another machine (lab
	// login --allow-plain-http, MACLAB_ALLOW_PLAIN_HTTP=1). Without it plain
	// http reaches loopback only: the token would cross the network in the
	// clear. See labhttp.
	AllowPlainHTTP bool `json:"allow_plain_http,omitempty"`
	// Note is told what a transfer is doing when it is worth a line, such as
	// an upload that resumes; nil says nothing.
	Note func(format string, args ...any) `json:"-"`
	// UploadFor names the Mac an upload is for. An M team member uploads
	// only for a job on a Mac they lease, and labd asks which.
	UploadFor string `json:"-"`
	// UploadForBuild marks an upload as a build's input (a config, a
	// recipe), so that whoever may not build is told so by the upload.
	UploadForBuild bool `json:"-"`
	http           *http.Client
	// release is the Lab release the lab said it runs, on its last answer.
	release atomic.Value
	// Tests shorten these: the longest file sent in one request, and the
	// wait before a retry.
	partBytes int64
	pause     func(ctx context.Context, n int) error
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", heldType+", application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var b []byte
	if resp.StatusCode == 200 && held(resp) {
		if b, err = heldResult(resp.Body); err != nil {
			return err
		}
	} else {
		b, _ = io.ReadAll(resp.Body)
	}
	if resp.StatusCode != 200 {
		return &StatusError{Code: resp.StatusCode, Msg: strings.TrimSpace(string(b))}
	}
	switch o := out.(type) {
	case nil:
	case *string:
		*o = string(b)
	default:
		return json.Unmarshal(b, out)
	}
	return nil
}

// StatusError is labd turning a request down: the HTTP status, and what labd said.
type StatusError struct {
	Code int
	Msg  string
}

func (e *StatusError) Error() string { return e.Msg }

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

// Device mirrors labd's DeviceView.
type Device struct {
	api.Device
	Tier   int               `json:"tier"`
	Alive  bool              `json:"alive"`
	Queued int               `json:"queued"`
	Events []api.KernelEvent `json:"recent_events,omitempty"`
	// Cable is a Lab-owned Mac's place on the lab server's cables; nil for
	// a Volunteer's Mac.
	Cable *api.Cable `json:"cable,omitempty"`
	// LabAgent is the number of the lab's lab-agent when the Mac's is older.
	LabAgent string `json:"lab_agent,omitempty"`
}

func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	return out, c.do(ctx, "GET", "/api/devices", nil, &out)
}

func (c *Client) Device(ctx context.Context, name string) (*Device, error) {
	var out Device
	return &out, c.do(ctx, "GET", "/api/devices/"+url.PathEscape(name), nil, &out)
}

func (c *Client) PatchDevice(ctx context.Context, name string, p api.DevicePatch) (*Device, error) {
	var out Device
	return &out, c.do(ctx, "PATCH", "/api/devices/"+url.PathEscape(name), jsonBody(p), &out)
}

func (c *Client) EnrollToken(ctx context.Context, req api.EnrollTokenRequest) (*api.EnrollTokenResponse, error) {
	var out api.EnrollTokenResponse
	return &out, c.do(ctx, "POST", "/api/enroll-tokens", jsonBody(req), &out)
}

// RevokeEnrollTokens revokes a name's enrollment tokens, used or not, and
// says how many there were.
func (c *Client) RevokeEnrollTokens(ctx context.Context, name string) (int, error) {
	var out struct {
		Revoked int `json:"revoked"`
	}
	return out.Revoked, c.do(ctx, "DELETE", "/api/enroll-tokens/"+url.PathEscape(name), nil, &out)
}

func (c *Client) Builders(ctx context.Context) ([]api.BuilderStatus, error) {
	var out []api.BuilderStatus
	return out, c.do(ctx, "GET", "/api/builders", nil, &out)
}

// AddBuilder returns the new builder's token, which labd shows only this once.
func (c *Client) AddBuilder(ctx context.Context, name string, kinds []string) (*api.NewBuilder, error) {
	var out api.NewBuilder
	return &out, c.do(ctx, "POST", "/api/builders", jsonBody(api.BuilderRequest{Name: name, Kinds: kinds}), &out)
}

// RemoveBuilder stops a named builder's token and fails the builds it was running.
func (c *Client) RemoveBuilder(ctx context.Context, name string) (*api.RemovedBuilder, error) {
	return c.RemoveBuilderGeneration(ctx, name, "")
}

// RemoveBuilderGeneration removes a named builder only while it is still the
// generation AddBuilder returned; "" removes whatever builder has the name.
func (c *Client) RemoveBuilderGeneration(ctx context.Context, name, generation string) (*api.RemovedBuilder, error) {
	var out api.RemovedBuilder
	path := "/api/builders/" + url.PathEscape(name)
	if generation != "" {
		path += "?generation=" + url.QueryEscape(generation)
	}
	return &out, c.do(ctx, "DELETE", path, nil, &out)
}

// AgentUpdate has a Lab-owned Mac that is idle take the lab's lab-agent.
func (c *Client) AgentUpdate(ctx context.Context, name string) (*api.AgentUpdate, error) {
	var out api.AgentUpdate
	return &out, c.do(ctx, "POST", "/api/devices/"+url.PathEscape(name)+"/agent-update", nil, &out)
}

// EndEnrollment ends a Mac's enrollment: labd forgets it and revokes its device secret at once.
func (c *Client) EndEnrollment(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/api/devices/"+url.PathEscape(name)+"/end-enrollment", nil, nil)
}

func (c *Client) Lease(ctx context.Context, name, holder string, ttl time.Duration) (*api.Lease, error) {
	var out api.Lease
	return &out, c.do(ctx, "POST", "/api/devices/"+url.PathEscape(name)+"/lease", jsonBody(api.LeaseRequest{Holder: holder, TTLSec: int(ttl.Seconds())}), &out)
}

// Release ends holder's lease on a Mac.
func (c *Client) Release(ctx context.Context, name, holder string) error {
	return c.do(ctx, "DELETE", "/api/devices/"+url.PathEscape(name)+"/lease?holder="+url.QueryEscape(holder), nil, nil)
}

// ForceRelease ends a Mac's lease whoever holds it, as a Maintainer ending
// another's: labd records who ended it, and why when a reason is given.
func (c *Client) ForceRelease(ctx context.Context, name, why string) error {
	q := url.Values{"force": {"1"}}
	if why != "" {
		q.Set("why", why)
	}
	return c.do(ctx, "DELETE", "/api/devices/"+url.PathEscape(name)+"/lease?"+q.Encode(), nil, nil)
}

// LeaseRecord is the newest lines of a Mac's Lease record, newest first.
func (c *Client) LeaseRecord(ctx context.Context, name string, limit int) ([]api.LeaseEvent, error) {
	var out []api.LeaseEvent
	return out, c.do(ctx, "GET", "/api/devices/"+url.PathEscape(name)+"/leases?limit="+strconv.Itoa(limit), nil, &out)
}

func (c *Client) Reset(ctx context.Context, name, by string) (string, error) {
	var out string
	return out, c.do(ctx, "POST", "/api/devices/"+url.PathEscape(name)+"/reset?by="+url.QueryEscape(by), nil, &out)
}

// Audit is labd's copy of a Volunteer Mac's Audit log.
func (c *Client) Audit(ctx context.Context, name string) ([]api.AuditEntry, error) {
	var out []api.AuditEntry
	return out, c.do(ctx, "GET", "/api/devices/"+url.PathEscape(name)+"/audit", nil, &out)
}

func (c *Client) OOBStatus(ctx context.Context, name string) (map[string]any, error) {
	var out map[string]any
	return out, c.do(ctx, "GET", "/api/devices/"+url.PathEscape(name)+"/oob", nil, &out)
}

// Upload sends a file to labd's artifact store and returns its sha256: in
// one request up to 32 MiB, in numbered parts beyond, which a later Upload
// of the same file resumes.
func (c *Client) Upload(ctx context.Context, path string) (string, error) {
	q := url.Values{}
	if c.UploadFor != "" {
		q.Set("device", c.UploadFor)
	}
	if c.UploadForBuild {
		q.Set("purpose", "build")
	}
	// Three variables are for the Front door's transfer tests alone.
	// MACLAB_PARTS_AT_ONCE times the same upload with 1 part and with 4 at
	// a time; MACLAB_UPLOAD_PROGRESS=1 has each part said as labd takes it,
	// so that a test can cut an upload it knows labd holds some of; and
	// MACLAB_UPLOAD_HOLD=1 stops one part halfway once labd holds two, so
	// that the cut comes inside a part however fast the rest go.
	atOnce, _ := strconv.Atoi(os.Getenv("MACLAB_PARTS_AT_ONCE"))
	s := upload.Sender{Client: c.http, URL: c.URL + "/api/artifacts", Query: q, Token: c.Token, PartBytes: c.partBytes, Pause: c.pause, Note: c.Note, AtOnce: atOnce,
		Hold: os.Getenv("MACLAB_UPLOAD_HOLD") == "1"}
	if os.Getenv("MACLAB_UPLOAD_PROGRESS") == "1" && c.Note != nil {
		s.Progress = func(held, total int) { c.Note("labd holds %d of %d parts", held, total) }
	}
	return s.File(ctx, path)
}

func (c *Client) Submit(ctx context.Context, spec api.JobSpec) (*api.Job, error) {
	if err := c.requireRecoveryMode(ctx, spec.RecoveryMode); err != nil {
		return nil, err
	}
	if spec.RecoveryTransaction != "" {
		var caps api.RecoveryCapabilities
		if e := c.do(ctx, "GET", "/api/recovery", nil, &caps); e != nil || !caps.TransactionIsolation {
			return nil, fmt.Errorf("controller lacks transaction recovery isolation; no job submitted")
		}
	}
	var out api.Job
	if spec.Mesa != nil {
		var caps api.MesaCapabilities
		if err := c.do(ctx, "GET", "/api/mesa", nil, &caps); err != nil {
			return nil, fmt.Errorf("controller does not support first-class Mesa; no job submitted: %w", err)
		}
		if caps.Schema != 1 || !caps.Staged || (spec.Mesa.OfficialGPU && !caps.OfficialGPU) {
			return nil, fmt.Errorf("controller Mesa capability/mode unavailable; no job submitted")
		}
	}
	route := "/api/jobs"
	if spec.Mesa != nil {
		route = "/api/mesa/jobs"
	}
	if err := c.do(ctx, "POST", route, jsonBody(spec), &out); err != nil {
		return nil, err
	}
	if out.Spec.RecoveryMode != spec.RecoveryMode || out.Spec.RecoveryTransaction != spec.RecoveryTransaction {
		return &out, fmt.Errorf("job %s returned different recovery_mode; do not retry blindly", out.ID)
	}
	if spec.Mesa != nil && out.Spec.Mesa == nil {
		return nil, fmt.Errorf("controller omitted Mesa request identity; job is not valid Mesa evidence")
	}
	return &out, nil
}

func (c *Client) Job(ctx context.Context, id string) (*api.Job, error) {
	var out api.Job
	return &out, c.do(ctx, "GET", "/api/jobs/"+url.PathEscape(id), nil, &out)
}

func (c *Client) Jobs(ctx context.Context, device string, limit int) ([]*api.Job, error) {
	var out []*api.Job
	return out, c.do(ctx, "GET", fmt.Sprintf("/api/jobs?device=%s&limit=%d", url.QueryEscape(device), limit), nil, &out)
}

// Wait long-polls until the job is done or timeout passes, returning its latest state.
// waitSlice is the longest one wait request lasts, and the longest labd holds
// one: the Front door cuts a request that sends nothing for 125 s. Wait asks
// again until timeout instead of holding one request open.
const waitSlice = 50 * time.Second

func (c *Client) Wait(ctx context.Context, id string, timeout time.Duration) (*api.Job, error) {
	deadline := time.Now().Add(timeout)
	for {
		slice := min(time.Until(deadline), waitSlice)
		var out api.Job
		err := c.do(ctx, "GET", fmt.Sprintf("/api/jobs/%s/wait?timeout=%d", url.PathEscape(id), max(int(slice.Seconds()), 1)), nil, &out)
		if err != nil || out.State == api.JobDone || time.Until(deadline) <= 0 {
			return &out, err
		}
	}
}

// Publish uploads a job's omarchy-m-test report and returns its page.
func (c *Client) Publish(ctx context.Context, id string) (string, error) {
	var out struct {
		ReportURL string `json:"report_url"`
	}
	err := c.do(ctx, "POST", "/api/jobs/"+url.PathEscape(id)+"/publish", nil, &out)
	return out.ReportURL, err
}

func (c *Client) Cancel(ctx context.Context, id string) (string, error) {
	var out string
	return out, c.do(ctx, "POST", "/api/jobs/"+url.PathEscape(id)+"/cancel", nil, &out)
}

// Retry queues a finished job again as a new job, held by holder.
func (c *Client) Retry(ctx context.Context, id, holder string) (*api.Job, error) {
	var out api.Job
	return &out, c.do(ctx, "POST", "/api/jobs/"+url.PathEscape(id)+"/retry?holder="+url.QueryEscape(holder), nil, &out)
}

// Exclude sets a finished job aside in every lab gate report, with the
// reason and who asks, until ClearExclusion clears it.
func (c *Client) Exclude(ctx context.Context, id, reason, by string) (*api.Job, error) {
	var out api.Job
	return &out, c.do(ctx, "POST", "/api/jobs/"+url.PathEscape(id)+"/exclude", jsonBody(api.ExcludeRequest{Reason: reason, By: by}), &out)
}

// ClearExclusion counts a job Exclude set aside in lab gate again.
func (c *Client) ClearExclusion(ctx context.Context, id string) (*api.Job, error) {
	var out api.Job
	return &out, c.do(ctx, "DELETE", "/api/jobs/"+url.PathEscape(id)+"/exclude", nil, &out)
}

func (c *Client) File(ctx context.Context, id, name string) ([]byte, error) {
	var out string
	err := c.do(ctx, "GET", "/api/jobs/"+url.PathEscape(id)+"/files/"+name, nil, &out)
	return []byte(out), err
}

// Gate reports whether a kernel (build id or sha256 prefix) passed on every
// required chip family; no families means all of them. volunteers counts
// volunteers' Macs too, and exclude sets those jobs of the kernel aside for
// this report only, besides those Exclude set aside.
func (c *Client) Gate(ctx context.Context, kernel string, families []string, volunteers bool, exclude []string) (*api.GateReport, error) {
	var out api.GateReport
	q := "?families=" + url.QueryEscape(strings.Join(families, ","))
	if volunteers {
		q += "&volunteers=1"
	}
	if len(exclude) > 0 {
		q += "&exclude=" + url.QueryEscape(strings.Join(exclude, ","))
	}
	return &out, c.do(ctx, "GET", "/api/gate/"+url.PathEscape(kernel)+q, nil, &out)
}

func (c *Client) Build(ctx context.Context, req api.BuildRequest) (*api.Build, error) {
	var out api.Build
	return &out, c.do(ctx, "POST", "/api/builds", jsonBody(req), &out)
}

func (c *Client) GetBuild(ctx context.Context, id string) (*api.Build, error) {
	var out api.Build
	return &out, c.do(ctx, "GET", "/api/builds/"+url.PathEscape(id), nil, &out)
}

func (c *Client) Builds(ctx context.Context, limit int) ([]*api.Build, error) {
	var out []*api.Build
	return out, c.do(ctx, "GET", fmt.Sprintf("/api/builds?limit=%d", limit), nil, &out)
}

// BuildLog writes the build log to w; with follow it goes on, polling from
// where the log ended, until the build ends.
func (c *Client) BuildLog(ctx context.Context, id string, follow bool, w io.Writer) error {
	if follow {
		return c.followBuildLog(ctx, id, w)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", c.URL+"/api/builds/"+url.PathEscape(id)+"/log", nil)
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		m, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", strings.TrimSpace(string(m)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// BuildFile downloads one output of a package build, the one the build
// recorded under sha256, into dst. A download that is cut goes on from where
// it stopped, in this call or the next one for the same dst and sha256.
func (c *Client) BuildFile(ctx context.Context, id, name, sha256, dst string) error {
	return c.fetch(ctx, "/api/builds/"+url.PathEscape(id)+"/files/"+url.PathEscape(name), name, sha256, dst)
}

// Installed sends one of the installed-release Maintainer requests
// (/api/installed/<release>[/<what>]) and decodes the reply into out.
func (c *Client) Installed(ctx context.Context, method, release, what string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	p := "/api/installed/" + url.PathEscape(release)
	if what != "" {
		p += "/" + what
	}
	if strings.HasPrefix(what, "entry/") {
		return c.once(ctx, method, p, r, out, "the freeze")
	}
	return c.do(ctx, method, p, r, out)
}

// Releases lists every Release with its verdict, newest first.
func (c *Client) Releases(ctx context.Context) ([]api.Release, error) {
	var out []api.Release
	return out, c.do(ctx, "GET", "/api/installed", nil, &out)
}

func (c *Client) CancelBuild(ctx context.Context, id string) error {
	return c.do(ctx, "POST", "/api/builds/"+url.PathEscape(id)+"/cancel", nil, nil)
}

// Exec runs a command on a Mac and waits for its result. The request goes
// out once: if the answer is cut the command may be running on the Mac, and
// the error is ErrCut.
func (c *Client) Exec(ctx context.Context, device string, a api.ExecArgs) (*api.ExecResult, error) {
	var out api.ExecResult
	return &out, c.once(ctx, "POST", "/api/devices/"+url.PathEscape(device)+"/exec", jsonBody(a), &out, "the command")
}
