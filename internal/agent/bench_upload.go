package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/perf"
)

// A Performance run's upload is tried a few times within the job: after
// the run, labd may be restarting or the network not back yet.
var (
	benchUploadTries = 4
	benchUploadWait  = 5 * time.Second
)

// benchResults is the upload of a Performance run from its test's output:
// bench.json's result for each of the run's metrics, and what labd keeps of
// bench-conditions.json. It ends the attempt succeeded, or failed when a
// metric has no result or one in error or drawn by a software renderer.
func benchResults(b *api.BenchRun, dir string) perf.Upload {
	up := perf.Upload{Entries: []json.RawMessage{}, State: perf.Succeeded}
	var rows []json.RawMessage
	if raw, err := os.ReadFile(filepath.Join(dir, "bench.json")); err != nil || json.Unmarshal(raw, &rows) != nil {
		rows = nil
	}
	got := map[string]bool{}
	for _, row := range rows {
		var e struct {
			Metric string  `json:"metric"`
			Reason *string `json:"reason"`
		}
		if json.Unmarshal(row, &e) != nil || got[e.Metric] || !slices.Contains(b.Metrics, e.Metric) {
			continue
		}
		got[e.Metric] = true
		up.Entries = append(up.Entries, row)
		if e.Reason != nil && (*e.Reason == perf.Error || *e.Reason == perf.SoftwareRenderer) {
			up.State = perf.Failed
		}
	}
	if len(got) < len(b.Metrics) {
		up.State = perf.Failed
	}
	var cond map[string]any
	if raw, err := os.ReadFile(filepath.Join(dir, "bench-conditions.json")); err == nil && json.Unmarshal(raw, &cond) == nil {
		keep := map[string]any{}
		for k, v := range cond {
			if v != nil && slices.Contains(perf.ConditionFields, k) {
				keep[k] = v
			}
		}
		if len(keep) > 0 {
			up.Conditions, _ = json.Marshal(keep)
		}
	}
	return up
}

// benchUpload posts the run's results to its attempt with the Mac's own
// credentials, and says whether labd took them. A refusal is not tried
// again; a failure to reach labd, or labd failing, is.
func (a *Agent) benchUpload(ctx context.Context, b *api.BenchRun, dir string) *api.BenchUpload {
	up := benchResults(b, dir)
	out := &api.BenchUpload{Run: b.Run, State: up.State, Results: len(up.Entries)}
	u := a.url(fmt.Sprintf("/api/agent/performance/%d/results", b.Run))
	wait := benchUploadWait
	for {
		out.Tries++
		var t perf.Taken
		err := postJSON(ctx, a.http, u, a.cfg.Secret, up, &t)
		if err == nil {
			out.Taken, out.State, out.Already, out.Error = true, t.State, t.Already, ""
			return out
		}
		out.Error = err.Error()
		var se *statusError
		refused := errors.As(err, &se) && se.Code >= 400 && se.Code < 500 &&
			se.Code != http.StatusRequestTimeout && se.Code != http.StatusTooManyRequests
		if refused || out.Tries >= benchUploadTries {
			return out
		}
		a.log.Printf("Performance run %d: upload: %v; trying again in %s", b.Run, err, wait)
		select {
		case <-ctx.Done():
			return out
		case <-a.clock().After(wait):
		}
		wait *= 2
	}
}

// benchUploaded puts the upload in the test's result: the test passes only
// when labd took it and the attempt succeeded.
func benchUploaded(res *api.TestResult, up *api.BenchUpload) {
	res.Bench = up
	if up.Taken && up.State == perf.Succeeded {
		return
	}
	res.Passed = false
	if !up.Taken {
		why := fmt.Sprintf("Performance run %d: labd did not take the upload (tries: %d): %s", up.Run, up.Tries, up.Error)
		if res.Error != "" {
			why = res.Error + "; " + why
		}
		res.Error = why
	}
}
