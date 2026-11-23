package macosrunner

import (
	"context"
	"io"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// The conditions of section 22, "Measuring", the same on both systems.
const (
	busyLimit    = 25.0 // percent of CPU over busyWindow
	busyWindow   = 3 * time.Second
	brightness   = 0.5
	coolLimit    = 50.0 // °C, the hottest sensor
	coolMax      = 60 * time.Second
	coolNoSensor = 30 * time.Second
	coolStep     = 5 * time.Second
)

// A step is one run of a tool, measuring one or more of the set's metrics.
// prepare, when there is one, runs before the cool-down: Storage fills its
// file there.
type step struct {
	metrics []string
	prepare func(*session, context.Context) Reason
	run     func(*session, context.Context) map[string]outcome
}

// outcome is a step's measurement of one metric: a value, or a reason.
type outcome struct {
	value    float64
	spread   *float64
	reason   Reason
	capacity *int64
	fill     *float64
	seconds  float64
	cool     float64
}

func measured(v float64) outcome { return outcome{value: v} }
func because(r Reason) outcome   { return outcome{reason: r} }
func spread(v float64) *float64  { return &v }
func all(ms []string, o outcome) map[string]outcome {
	out := map[string]outcome{}
	for _, m := range ms {
		out[m] = o
	}
	return out
}

// steps are the macOS side of every metric the runner knows, by metric id.
var steps = []step{
	{metrics: []string{"coremark-single"}, run: coremarkStep("coremark-single", 1)},
	{metrics: []string{"coremark-all"}, run: coremarkStep("coremark-all", 0)},
	{metrics: []string{"7zip"}, run: sevenZip},
	{metrics: []string{"motionmark"}, run: browserStep("motionmark")},
	{metrics: []string{"clpeak-global-bandwidth", "clpeak-fp32"}, run: clpeak},
	{metrics: []string{"fio-seq-read"}, prepare: fioFill, run: fioStep("fio-seq-read")},
	{metrics: []string{"fio-seq-write"}, prepare: fioFill, run: fioStep("fio-seq-write")},
	{metrics: []string{"fio-rand-read-4k"}, prepare: fioFill, run: fioStep("fio-rand-read-4k")},
	{metrics: []string{"fio-rand-write-4k"}, prepare: fioFill, run: fioStep("fio-rand-write-4k")},
	{metrics: []string{"stream-triad", "stream-copy"}, run: stream},
	{metrics: []string{"speedometer"}, run: browserStep("speedometer")},
	{metrics: []string{"jetstream"}, run: browserStep("jetstream")},
	{metrics: []string{"decode-h264-1080p"}, run: decodeStep("decode-h264-1080p", "clip-h264-1080p")},
	{metrics: []string{"decode-hevc-4k"}, run: decodeStep("decode-hevc-4k", "clip-hevc-4k")},
	{metrics: []string{"neural-engine-mobilenet-v1"}, run: neuralEngine},
	{metrics: []string{"llama-prompt-gpu", "llama-gen-gpu"}, run: llamaGPU},
	{metrics: []string{"llama-gen-cpu"}, run: llamaCPU},
	{metrics: []string{"mlx-matmul-fp16"}, run: mlx},
	{metrics: []string{"power-idle"}, run: powerIdle},
	{metrics: []string{"power-playback-1080p"}, run: powerPlayback},
	{metrics: []string{"power-all-core"}, run: powerAllCore},
}

// session is one macOS run of a set's list on this boot.
type session struct {
	r        *Runner
	run      *api.MacOSRun
	set      *sets.Set
	files    map[string]string // fetched, by file id
	unpacked map[string]string
	share    string
	user     string
	log      io.Writer
	fio      *fioState
	// conditionsOut is the run's bench-conditions.json, which the browser
	// and Storage add to.
	conditionsOut *Conditions
}

func (s *session) host() Host { return s.r.host() }
