package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/reports"
)

func TestThePerformanceDataTakesItsSetReleaseAndVolunteers(t *testing.T) {
	l := newPublicLab(t, nil)
	key := func(c byte) string { return strings.Repeat(string(c), 32) }
	at := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	value := func(mac, side, release, metric string, v float64) reports.PerfValue {
		pv := reports.PerfValue{Mac: mac, Set: testSet, Side: side, Release: release, Metric: metric, Value: &v, Unit: "iterations/s", Measured: at}
		if side == "macos" {
			pv.Release, pv.MacOSVersion, pv.MacOSBuild = "", "27.2.1", "27C101"
		}
		return pv
	}
	p := reports.Performance{
		Macs: []reports.PerfMac{{Key: key('a'), Kind: reports.PerfLabOwned, Board: "j314s", CPU: 10, GPU: 16}, {Key: key('b'), Kind: reports.PerfVolunteer, Board: "j313", CPU: 8, GPU: 7}},
		Releases: []reports.PerfRelease{{Key: key('1'), Kernel: "6.18.1-1", Verdict: reports.PerfQualified, Position: 1},
			{Key: key('2'), Kernel: "6.18.2-1", Verdict: reports.PerfNoVerdictYet, Position: 2}},
		Values: []reports.PerfValue{
			value(key('a'), "macos", "", "coremark-single", 100), value(key('a'), "omarchy", key('1'), "coremark-single", 90),
			value(key('a'), "omarchy", key('2'), "coremark-single", 95),
			value(key('b'), "macos", "", "coremark-single", 100), value(key('b'), "omarchy", key('1'), "coremark-single", 80),
		},
	}
	if _, err := l.s.reports.PublishPerformance(p, at); err != nil {
		t.Fatal(err)
	}
	type data struct {
		Set     string
		Release struct{ Kernel, Verdict string }
		Configs []struct {
			Board    string
			Headline struct {
				Volunteer  bool
				MacOSBuild string `json:"macos_build"`
				MacsPaired int    `json:"macs_paired"`
				Metrics    []struct {
					Metric string
					Ratio  *float64
				}
			}
		} `json:"configurations"`
	}
	get := func(target string) data {
		t.Helper()
		code, body, header := l.get(target)
		if code != http.StatusOK || !strings.HasPrefix(header.Get("Content-Type"), "application/json") {
			t.Fatalf("GET %s: %d %s", target, code, body)
		}
		for _, secret := range []string{key('a'), key('b'), key('1'), key('2')} {
			if strings.Contains(body, secret) {
				t.Errorf("GET %s names %s", target, secret)
			}
		}
		var d data
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	ratio := func(d data, board string) float64 {
		for _, c := range d.Configs {
			if c.Board == board {
				for _, m := range c.Headline.Metrics {
					if m.Metric == "coremark-single" && m.Ratio != nil {
						return *m.Ratio
					}
				}
			}
		}
		return -1
	}

	d := get("/api/v1/performance.json")
	if d.Set != testSet || d.Release.Kernel != "6.18.1-1" || d.Release.Verdict != reports.PerfQualified || len(d.Configs) != 2 || ratio(d, "j314s") != 90 || ratio(d, "j313") != 80 {
		t.Errorf("the default: %+v", d)
	}
	if !d.Configs[0].Headline.Volunteer || d.Configs[1].Headline.MacOSBuild != "27C101" || d.Configs[1].Headline.MacsPaired != 1 {
		t.Errorf("the headlines: %+v", d.Configs)
	}
	if d = get("/api/v1/performance.json?set=" + testSet + "&release=6.18.2-1"); d.Release.Verdict != reports.PerfNoVerdictYet || ratio(d, "j314s") != 95 || ratio(d, "j313") != -1 {
		t.Errorf("release=6.18.2-1: %+v", d)
	}
	if d = get("/api/v1/performance.json?set=" + testSet + "&volunteers=false"); len(d.Configs) != 1 || d.Configs[0].Board != "j314s" {
		t.Errorf("volunteers=false: %+v", d)
	}
	for _, target := range []string{"/api/v1/performance.json?set=99", "/api/v1/performance.json?release=6.0"} {
		if code, _, _ := l.get(target); code != http.StatusNotFound {
			t.Errorf("GET %s: %d", target, code)
		}
	}
}
