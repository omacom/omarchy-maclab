// Command perf-demo fills a lab's reports.db with made-up Performance values,
// so that the Performance pages can be looked at in a browser with no Mac:
//
//	go run ./cmd/labd --data /tmp/perf-lab --listen 127.0.0.1:7770 &
//	go run ./tools/perf-demo --data /tmp/perf-lab
//
// then open http://127.0.0.1:7770/performance. labd first, as it copies its
// own published values (none, with no Mac) to reports.db when it starts,
// and again only when one of its Macs changes. The values are invented,
// one Benchmark set's (bench-1 unless --set), over a handful of
// Configurations and six Releases: Lab-owned, Counted and Volunteers' Macs,
// a Volunteer-only Configuration, one Mac on an older macOS build, one with
// no macOS result yet, Storage at two SSD sizes, rows with no build or no
// driver, and a column whose Macs booted different Mesa. It replaces what
// reports.db holds of Performance, and nothing else.
package main

import (
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/perf"
	"github.com/omacom/omarchy-maclab/internal/reports"
)

type mac struct {
	kind, board string
	cpu, gpu    int
	ssd         int64
	macOS       string // "new", "old" or "" for no macOS result
}

var macs = []mac{
	{reports.PerfLabOwned, "j274", 8, 8, 256, "new"},
	{reports.PerfLabOwned, "j313", 8, 7, 256, "new"},
	{reports.PerfLabOwned, "j313", 8, 7, 512, "new"},
	{reports.PerfVolunteer, "j313", 8, 7, 512, "new"},
	{reports.PerfVolunteer, "j313", 8, 8, 512, "new"},
	{reports.PerfLabOwned, "j314s", 8, 14, 512, "new"},
	{reports.PerfLabOwned, "j314s", 10, 16, 512, "new"},
	{reports.PerfCounted, "j314s", 10, 16, 1000, "new"},
	{reports.PerfLabOwned, "j314s", 10, 16, 1000, "old"},
	{reports.PerfVolunteer, "j314s", 10, 16, 1000, "new"},
	{reports.PerfLabOwned, "j316c", 10, 32, 1000, "new"},
	{reports.PerfLabOwned, "j413", 8, 10, 512, "new"},
	{reports.PerfVolunteer, "j473", 8, 10, 256, "new"},
	{reports.PerfLabOwned, "j414c", 12, 38, 1000, "new"},
	{reports.PerfLabOwned, "j504", 8, 10, 512, ""},
}

var releases = []struct{ kernel, verdict, mesa string }{
	{"6.17.9-1", reports.PerfQualified, "25.3.2"},
	{"6.18.0-1", reports.PerfQualified, "25.3.2"},
	{"6.18.1-1", reports.PerfQualified, "26.0.1"},
	{"6.18.2-1", reports.PerfQualified, "26.0.3"},
	{"6.18.3-1", reports.PerfQualified, "26.0.3"},
	{"6.18.4-1", reports.PerfNoVerdictYet, "26.1.0"},
}

// share is Omarchy's usual share of macOS per category.
var share = map[string]float64{"CPU": 0.97, "GPU": 0.62, "Storage": 0.86, "Memory": 0.95, "Web": 0.83, "Video": 0.52, "AI": 0.38, "Power": 0.66}

// macOSValue is a made-up macOS value per unit.
var macOSValue = map[string]float64{"iterations/s": 30000, "MIPS": 60000, "score": 300, "GB/s": 180, "GFLOPS": 2600, "MB/s": 5000,
	"kIOPS": 400, "frames/s": 900, "inferences/s": 2100, "tokens/s": 60, "TFLOPS": 4.2, "W": 4.5, "J": 900, "s": 10}

// jitter is a steady made-up variation in [-1, 1] for the words given.
func jitter(words ...string) float64 {
	h := fnv.New32a()
	h.Write([]byte(strings.Join(words, "/")))
	return float64(h.Sum32()%2001)/1000 - 1
}

func main() {
	data := flag.String("data", "", "labd's --data directory; reports.db is made there if it is not")
	setName := flag.String("set", "", "the Benchmark set (bench-N); the current set by default")
	flag.Parse()
	if *data == "" {
		log.Fatal("give --data DIR")
	}
	set, err := reports.PerfSet(*setName)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		log.Fatal(err)
	}
	store, err := reports.Open(filepath.Join(*data, "reports.db"))
	if err != nil {
		log.Fatal(err)
	}
	now := time.Now()
	if _, err := store.PublishPerformance(demo(set, now), now); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("reports.db in %s holds %d made-up Macs and %d Releases of %s\n", *data, len(macs), len(releases), set.Name)
}

func key(kind string, i int) string { return fmt.Sprintf("%032x", uint64(i+1)<<8|uint64(len(kind))) }

func demo(set *sets.Set, now time.Time) reports.Performance {
	var p reports.Performance
	for i, r := range releases {
		p.Releases = append(p.Releases, reports.PerfRelease{Key: key("release", 100+i), Kernel: r.kernel, Verdict: r.verdict, Position: i + 1})
	}
	for i, m := range macs {
		k := key("mac", i)
		p.Macs = append(p.Macs, reports.PerfMac{Key: k, Kind: m.kind, Board: m.board, CPU: m.cpu, GPU: m.gpu})
		config := fmt.Sprintf("%s-%dc-%dg", m.board, m.cpu, m.gpu)
		for _, metric := range set.Metrics {
			unit := metric.Unit
			var capacity *int64
			if metric.Category == "Storage" {
				gb := m.ssd
				capacity = &gb
			}
			base := macOSValue[unit] * (1 + 0.1*jitter(config, metric.ID, fmt.Sprint(m.ssd)))
			if metric.Category != sets.OmarchyOnly && metric.MacOS != nil && m.macOS != "" {
				v := reports.PerfValue{Mac: k, Set: set.Name, Side: perf.SideMacOS, Metric: metric.ID, Unit: unit, API: metric.MacOS.API, Capacity: capacity,
					MacOSVersion: "27.2.1", MacOSBuild: "27C101", Measured: now.Add(-96 * time.Hour)}
				if m.macOS == "old" {
					v.MacOSVersion, v.MacOSBuild, v.Measured = "27.1", "27B5", now.Add(-33*24*time.Hour)
				}
				value := base
				v.Value = &value
				p.Values = append(p.Values, v)
			}
			for j, r := range releases {
				// The older Releases were measured on the Lab-owned Macs alone.
				if j < 2 && m.kind == reports.PerfVolunteer {
					continue
				}
				v := reports.PerfValue{Mac: k, Set: set.Name, Side: perf.SideOmarchy, Release: p.Releases[j].Key, Metric: metric.ID, Unit: unit,
					Capacity: capacity, Measured: now.Add(-time.Duration(len(releases)-j) * 72 * time.Hour)}
				if metric.Omarchy != nil {
					v.API = metric.Omarchy.API
				}
				v.Booted = map[string]string{"linux-asahi": strings.TrimSuffix(r.kernel, "-1"), "mesa": r.mesa, "m1n1": "1.5.2", "asahi-firmware": "20261102", "llama.cpp": "b6210"}
				if j >= 3 {
					v.Booted["libane"] = "0.5.0"
				}
				if j == len(releases)-1 && i%2 == 0 {
					v.Booted["mesa"] = "26.0.3"
				}
				switch {
				case metric.ID == "mlx-matmul-fp16":
					v.Reason = reason(perf.NoBuild)
				case metric.ID == "neural-engine-mobilenet-v1" && j < 3:
					v.Reason = reason(perf.NoDriver)
				case metric.ID == "power-playback-1080p" && m.board == "j274":
					v.Reason = reason(perf.NoComparableSensor)
				case metric.Category == sets.OmarchyOnly:
					value := 12 - 0.4*float64(j) + 0.5*jitter(config, r.kernel)
					v.Value = &value
				default:
					ratio := share[metric.Category] * (1 + 0.012*float64(j) + 0.05*jitter(config, metric.ID, r.kernel))
					if m.kind == reports.PerfVolunteer {
						ratio *= 0.95
					}
					value := base * ratio
					if !metric.HigherIsBetter() {
						value = base / ratio
					}
					v.Value = &value
				}
				p.Values = append(p.Values, v)
			}
		}
	}
	return p
}

func reason(code string) *string { return &code }
