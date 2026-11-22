package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// lab compare queues the job with what it was asked, and done, publish and
// show are one call each.
func TestLabCompareCallsTheAPI(t *testing.T) {
	var got []string
	var req api.CompareRequest
	job := `{"id":"j1","state":"done","outcome":"pass","spec":{"device":"m2-max","compare":{"before":"current","after":"7.3","tier":"default"}},
"result":{"booted":false,"compare":{"before":"current","after":"7.3","tier":"default","bench":"bench-1","regressed":1,"fixed":0,
"sides":[{"side":"A","set":"current","boot":"known-good"},{"side":"B","set":"7.3","boot":"candidate confirmed"}],
"rows":[{"name":"speedometer","higher_is_better":true,"a":20,"b":25,"delta_pct":25,"tolerance_pct":3,"verdict":"better"},
{"name":"stream-triad","higher_is_better":true,"a":100,"b":null,"delta_pct":null,"tolerance_pct":5,"verdict":"not compared","note":"B: fell_back"}],
"checks":[{"id":"system.snapshots","a":"pass","b":"fail","change":"not compared","note":"Candidate boot: it runs from @lab-candidate"},{"id":"wifi.connected","a":"pass","b":"fail","change":"regressed"}]}}}`
	c := labAt(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		if r.URL.Path == "/api/compares" {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &req)
		}
		io.WriteString(w, job)
	})
	ctx := context.Background()
	for _, args := range [][]string{
		{"m2-max", "--before", "7.2", "--after", "7.3", "--macos", "--tier", "extended", "--hands", "--no-wait"},
		{"done", "j1"}, {"publish", "j1"}, {"show", "j1"},
	} {
		if err := dispatch(ctx, c, "compare", args); err != nil {
			t.Fatalf("lab compare %v: %v", args, err)
		}
	}
	want := []string{"POST /api/compares", "POST /api/compares/j1/done", "POST /api/compares/j1/publish", "GET /api/jobs/j1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("asked %v, want %v", got, want)
	}
	if req != (api.CompareRequest{Mac: "m2-max", Before: "7.2", After: "7.3", MacOS: true, Tier: "extended", Hands: true}) {
		t.Errorf("sent %+v", req)
	}
	var j api.Job
	json.Unmarshal([]byte(job), &j)
	var b strings.Builder
	if err := printCompare(&b, &j); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"default tier of bench-1", "speedometer", "+25.0%", "better", "±3%", "not compared", "B: fell_back", "wifi.connected: A pass, B fail, regressed", "system.snapshots: A pass, B fail, not compared (Candidate boot: it runs from @lab-candidate)", "candidate confirmed", "lab compare publish j1"} {
		if !strings.Contains(b.String(), s) {
			t.Errorf("the report lacks %q:\n%s", s, b.String())
		}
	}
	for _, bad := range [][]string{{"m2-max"}, {"m2-max", "--before", "current"}, {"done"}, {}} {
		if err := dispatch(ctx, c, "compare", bad); err == nil {
			t.Errorf("lab compare %v: no error", bad)
		}
	}
}
