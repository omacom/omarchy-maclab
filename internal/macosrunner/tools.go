package macosrunner

// The benchmarks other than the browser's. Each runs the set's own file,
// reads only numbers from the tool's output and checks the work ran where
// the set says (section 22, "The work ran where it says"); one that did not
// is fell_back, with no value. The programs the lab builds itself are run
// as the release workflow builds them:
//
//   coremark M<threads> 0x0 0x0 0x66 <iterations> 7 1 2000   up to 64 threads; 0 iterations lets it choose
//   stream <elements>                                          each of its three arrays, 8-byte elements
//   ane-bench MODEL.mlmodelc                                   {"inferences_per_s", "placement", "model_sha256"}
//   mlx-bench                                                  {"device", "tflops"}, or {"error"}
//   smc-power --seconds N                                      {"valid", "mean_watts"}, or {"error"} and non-zero

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

func num(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// setting is a number in the metric's settings.
func (s *session) setting(metric, key string, d float64) float64 {
	m, ok := s.set.Metric(metric)
	if !ok {
		return d
	}
	if v, ok := m.Settings[key].(float64); ok {
		return v
	}
	return d
}

// --- CPU

var (
	reCoreMark    = regexp.MustCompile(`(?m)^Iterations/Sec\s*:\s*([0-9.]+)`)
	reCoreMarkCRC = regexp.MustCompile(`(?mi)^ERROR!.*crc`)
)

// parseCoreMark is CoreMark's iterations a second, when it validated its
// own work. A fixed count of iterations under 10 s is not validated, so the
// power run checks only the CRCs.
func parseCoreMark(out string) (float64, error) {
	if !strings.Contains(out, "Correct operation validated") || reCoreMarkCRC.MatchString(out) {
		return 0, errors.New("CoreMark did not validate its own work")
	}
	m := reCoreMark.FindStringSubmatch(out)
	if m == nil || num(m[1]) <= 0 {
		return 0, errors.New("no CoreMark score")
	}
	return num(m[1]), nil
}

// coremarkMaxThreads is the most threads the lab's CoreMark is built for.
const coremarkMaxThreads = 64

func (s *session) coremark(ctx context.Context, threads, iterations int) (string, error) {
	exe, err := s.program(ctx, "coremark")
	if err != nil {
		return "", err
	}
	threads = min(max(threads, 1), coremarkMaxThreads)
	out, _, err := s.tool(ctx, 20*time.Minute, exe, "M"+strconv.Itoa(threads), "0x0", "0x0", "0x66", strconv.Itoa(iterations), "7", "1", "2000")
	return out, err
}

// coremarkStep is CoreMark on threads threads, 0 being one per core.
func coremarkStep(metric string, threads int) func(*session, context.Context) map[string]outcome {
	return func(s *session, ctx context.Context) map[string]outcome {
		n := threads
		if n == 0 {
			n = s.host().CPUs()
		}
		out, err := s.coremark(ctx, n, 0)
		v, perr := parseCoreMark(out)
		if err != nil || perr != nil {
			return all([]string{metric}, because(Error))
		}
		return map[string]outcome{metric: measured(v)}
	}
}

var reTot = regexp.MustCompile(`(?m)^Tot:\s+.*?(\d+)\s*$`)

// parse7zip reads the total rating (MIPS) from `7zz b`.
func parse7zip(out string) (float64, error) {
	m := reTot.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("no Tot: line in 7zz b")
	}
	return num(m[1]), nil
}

func sevenZip(s *session, ctx context.Context) map[string]outcome {
	exe, err := s.program(ctx, "7zip")
	if err != nil {
		return all([]string{"7zip"}, because(Error))
	}
	out, _, err := s.tool(ctx, 20*time.Minute, exe, "b")
	v, perr := parse7zip(out)
	if err != nil || perr != nil {
		return all([]string{"7zip"}, because(Error))
	}
	return map[string]outcome{"7zip": measured(v)}
}

// --- GPU: clpeak on Apple's OpenCL

var (
	reCLPlatform = regexp.MustCompile(`(?m)^\s*Platform \d+: (.+)$`)
	reCLDevice   = regexp.MustCompile(`(?m)^\s*Device \d+: (.+)$`)
	reCLSoftware = regexp.MustCompile(`(?i)\bcpu\b|llvmpipe|pocl|swiftshader|software`)
	reCLValue    = regexp.MustCompile(`^\s+\w+\s*:\s*([0-9.]+) ([MGT])(?:FLOPS|B/s)\s*$`)
	reCLNone     = regexp.MustCompile(`(?i)no platforms|CL_PLATFORM_NOT_FOUND|clGetPlatformIDs \(-1001\)`)
)

// clScale is the unit clpeak gives a value, in GFLOPS or GB/s.
var clScale = map[string]float64{"M": 1e-3, "G": 1, "T": 1e3}

// clpeakSection is the best value of a clpeak section, such as "Global
// memory bandwidth" or "Single-precision compute".
func clpeakSection(out, head string) (float64, bool) {
	i := strings.Index(out, head)
	if i < 0 {
		return 0, false
	}
	best, found := 0.0, false
	for _, l := range strings.Split(out[i:], "\n")[1:] {
		m := reCLValue.FindStringSubmatch(l)
		if m == nil {
			break
		}
		best, found = math.Max(best, num(m[1])*clScale[m[2]]), true
	}
	return best, found
}

// clpeak runs on Apple's OpenCL, and only on the physical GPU: a CPU or
// software device fell back.
func clpeak(s *session, ctx context.Context) map[string]outcome {
	ids := []string{"clpeak-global-bandwidth", "clpeak-fp32"}
	exe, err := s.program(ctx, "clpeak")
	if err != nil {
		return all(ids, because(Error))
	}
	out, _, err := s.tool(ctx, 15*time.Minute, exe, "--opencl", "--global-memory-bandwidth", "--single-precision-compute")
	p, d := reCLPlatform.FindStringSubmatch(out), reCLDevice.FindStringSubmatch(out)
	switch {
	case reCLNone.MatchString(out):
		return all(ids, because(NoDriver))
	case p == nil:
		return all(ids, because(Error))
	case !strings.Contains(p[1], "Apple") || d == nil || reCLSoftware.MatchString(d[1]):
		return all(ids, because(FellBack))
	case err != nil:
		return all(ids, because(Error))
	}
	res := map[string]outcome{}
	for id, head := range map[string]string{ids[0]: "Global memory bandwidth", ids[1]: "Single-precision compute"} {
		if v, ok := clpeakSection(out, head); ok {
			res[id] = measured(v)
		} else {
			res[id] = because(Error)
		}
	}
	return res
}

// --- Storage: fio, section 22 "Measuring": one 8 GiB file filled before
// timing, direct I/O (F_NOCACHE on APFS), 20% of the disk kept free.

const (
	fioFileSize = 8 << 30
	fioFreeKept = 0.20
	// fioCacheMBs is faster than any Mac's SSD reads: a sequential read
	// above it came from the cache, not the disk.
	fioCacheMBs = 15000
)

type fioState struct {
	file     string
	capacity *int64
	fill     *float64
	why      Reason
	done     bool
}

// fioFill makes and fills the scratch file once, or says why Storage does
// not run.
func fioFill(s *session, ctx context.Context) Reason {
	if s.fio != nil && s.fio.done {
		return s.fio.why
	}
	st := &fioState{done: true, file: filepath.Join(s.r.Dir, "fio-scratch")}
	s.fio = st
	total, free, err := s.host().Disk(s.r.Dir)
	if err != nil || total == 0 {
		st.why = Error
		return st.why
	}
	ssd, err := s.host().SSD(ctx)
	if err != nil || ssd == 0 {
		ssd = total
	}
	gb := int64(math.Round(float64(ssd) / 1e9))
	st.capacity = &gb
	have := int64(0)
	if fi, err := os.Stat(st.file); err == nil {
		have = fi.Size()
	}
	after := float64(free) - float64(max(fioFileSize-have, 0))
	fill := round(100*(1-after/float64(total)), 1)
	st.fill = &fill
	if c := s.conditionsOut; c != nil {
		c.FreePercent = spread(round(100-fill, 1))
	}
	if after < fioFreeKept*float64(total) {
		st.why = DiskTooFull
		return st.why
	}
	exe, err := s.program(ctx, "fio")
	if err != nil {
		st.why = Error
		return st.why
	}
	if _, _, err := s.tool(ctx, 30*time.Minute, exe, "--name=fill", "--filename="+st.file, "--size=8g",
		"--rw=write", "--bs=1m", "--direct=1", "--ioengine=psync", "--iodepth=1", "--refill_buffers", "--end_fsync=1", "--output-format=json"); err != nil {
		st.why = Error
	}
	return st.why
}

func (s *session) fioCleanup() {
	if s.fio != nil {
		os.Remove(s.fio.file)
	}
}

// fioArgs are each Storage metric's job, as the set's settings say.
var fioArgs = map[string][]string{
	"fio-seq-read":      {"--rw=read", "--bs=1m", "--size=8g"},
	"fio-seq-write":     {"--rw=write", "--bs=1m", "--size=8g"},
	"fio-rand-read-4k":  {"--rw=randread", "--bs=4k", "--numjobs=32", "--iodepth=1", "--size=256m", "--offset_increment=256m", "--time_based", "--runtime=30", "--group_reporting"},
	"fio-rand-write-4k": {"--rw=randwrite", "--bs=4k", "--numjobs=32", "--iodepth=1", "--size=256m", "--offset_increment=256m", "--time_based", "--runtime=30", "--group_reporting"},
}

type fioSide struct {
	BW      float64 `json:"bw_bytes"`
	BWDev   float64 `json:"bw_dev"` // KiB/s
	IOPS    float64 `json:"iops"`
	IOPSDev float64 `json:"iops_stddev"`
}

type fioJSON struct {
	Jobs []struct {
		Options map[string]string `json:"job options"`
		Read    fioSide           `json:"read"`
		Write   fioSide           `json:"write"`
	} `json:"jobs"`
}

func fioStep(metric string) func(*session, context.Context) map[string]outcome {
	return func(s *session, ctx context.Context) map[string]outcome {
		o := s.fioRun(ctx, metric)
		o.capacity, o.fill = s.fio.capacity, s.fio.fill
		return map[string]outcome{metric: o}
	}
}

func (s *session) fioRun(ctx context.Context, metric string) outcome {
	exe, err := s.program(ctx, "fio")
	if err != nil {
		return because(Error)
	}
	args := append([]string{"--name=" + metric, "--filename=" + s.fio.file, "--direct=1", "--ioengine=psync", "--output-format=json"}, fioArgs[metric]...)
	out, stderr, err := s.tool(ctx, 15*time.Minute, exe, args...)
	var j fioJSON
	if err != nil || json.Unmarshal([]byte(out), &j) != nil || len(j.Jobs) == 0 {
		return because(Error)
	}
	job := j.Jobs[0]
	if job.Options["direct"] != "1" || strings.Contains(stderr, "direct") {
		return because(FellBack)
	}
	side := job.Read
	if strings.Contains(metric, "write") {
		side = job.Write
	}
	var o outcome
	if strings.Contains(metric, "rand") {
		o = outcome{value: side.IOPS / 1000, spread: spread(side.IOPSDev / 1000)}
	} else {
		o = outcome{value: side.BW / 1e6, spread: spread(side.BWDev * 1024 / 1e6)}
		if metric == "fio-seq-read" && o.value > fioCacheMBs {
			return because(FellBack)
		}
	}
	if o.value <= 0 {
		return because(Error)
	}
	return o
}

// --- Memory: STREAM's arrays at least four times the largest cache. macOS
// does not show the system level cache, the largest, so it is taken at the
// biggest any Apple Silicon Mac has, as on Omarchy.
const (
	streamLargestCache = 96 << 20
	streamCacheFactor  = 4
	streamElements     = streamCacheFactor * streamLargestCache / 8
)

var reStream = regexp.MustCompile(`(?m)^(Copy|Scale|Add|Triad):\s+([0-9.]+)`)

// parseStream reads STREAM's best rates, in MB/s, by kernel.
func parseStream(out string) map[string]float64 {
	m := map[string]float64{}
	for _, x := range reStream.FindAllStringSubmatch(out, -1) {
		m[strings.ToLower(x[1])] = num(x[2])
	}
	return m
}

func stream(s *session, ctx context.Context) map[string]outcome {
	ids := []string{"stream-triad", "stream-copy"}
	exe, err := s.program(ctx, "stream")
	if err != nil {
		return all(ids, because(Error))
	}
	out, _, err := s.tool(ctx, 15*time.Minute, exe, strconv.Itoa(streamElements))
	if err != nil || !strings.Contains(out, "Solution Validates") {
		return all(ids, because(Error))
	}
	rates := parseStream(out)
	res := map[string]outcome{}
	for _, id := range ids {
		if v, ok := rates[strings.TrimPrefix(id, "stream-")]; ok && v > 0 {
			res[id] = measured(v / 1000)
		} else {
			res[id] = because(Error)
		}
	}
	return res
}

// --- Video: FFmpeg through VideoToolbox, every frame of the clip

var (
	reFrame   = regexp.MustCompile(`frame=\s*(\d+)`)
	reRTime   = regexp.MustCompile(`bench:.*rtime=([0-9.]+)s`)
	reHWFail  = regexp.MustCompile(`(?i)failed setup for format videotoolbox|hwaccel initialisation returned error|no device available for decoder`)
	reHWFrame = regexp.MustCompile(`Video: wrapped_avframe, videotoolbox_vld`)
)

// decodeArgs decode the clip in hardware with frames left on the GPU, so a
// fall-back to software shows in the output stream.
func decodeArgs(clip string, realtime bool) []string {
	a := []string{"-hide_banner", "-nostdin", "-benchmark"}
	if realtime {
		a = append(a, "-re")
	}
	return append(a, "-hwaccel", "videotoolbox", "-hwaccel_output_format", "videotoolbox_vld", "-i", clip, "-map", "0:v:0", "-f", "null", "-")
}

// decoded is the frames decoded, and whether in hardware.
func decoded(stderr string) (frames int, hardware bool) {
	if m := reFrame.FindAllStringSubmatch(stderr, -1); m != nil {
		frames, _ = strconv.Atoi(m[len(m)-1][1])
	}
	out := stderr
	if i := strings.Index(stderr, "Output #0"); i >= 0 {
		out = stderr[i:]
	}
	return frames, reHWFrame.MatchString(out) && !reHWFail.MatchString(stderr)
}

func decodeStep(metric, clip string) func(*session, context.Context) map[string]outcome {
	return func(s *session, ctx context.Context) map[string]outcome {
		exe, err := s.program(ctx, "ffmpeg")
		if err != nil {
			return all([]string{metric}, because(Error))
		}
		_, stderr, err := s.tool(ctx, 15*time.Minute, exe, decodeArgs(s.file(clip), false)...)
		frames, hw := decoded(stderr)
		switch {
		case err != nil:
			return all([]string{metric}, because(Error))
		case !hw:
			return all([]string{metric}, because(FellBack))
		}
		rt := reRTime.FindStringSubmatch(stderr)
		if frames == 0 || rt == nil || num(rt[1]) <= 0 {
			return all([]string{metric}, because(Error))
		}
		return map[string]outcome{metric: measured(float64(frames) / num(rt[1]))}
	}
}

// --- AI

// modelDigest is ane-bench's model_sha256 of a compiled model: the sha256
// of `shasum -a 256` lines for every file in it, sorted bytewise.
func modelDigest(root string) (string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, p)
			files = append(files, rel)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	slices.Sort(files)
	var lines strings.Builder
	for _, rel := range files {
		sum, err := fileSHA256(filepath.Join(root, rel))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&lines, "%s  ./%s\n", sum, rel)
	}
	d := sha256.Sum256([]byte(lines.String()))
	return hex.EncodeToString(d[:]), nil
}

// coreMLModel is the pinned model as ane-bench takes it, a compiled
// .mlmodelc folder, and the model_sha256 it must report.
func (s *session) coreMLModel(ctx context.Context, id string) (string, string, error) {
	d, err := s.dir(ctx, id)
	if err != nil {
		return "", "", err
	}
	model := d
	filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
		if err == nil && e.IsDir() && strings.HasSuffix(p, ".mlmodelc") {
			model = p
			return filepath.SkipAll
		}
		return nil
	})
	sum, err := modelDigest(model)
	return model, sum, err
}

// neuralEngine runs the pinned MobileNet in Core ML; it counts only when
// Core ML placed it on the Neural Engine.
func neuralEngine(s *session, ctx context.Context) map[string]outcome {
	id := "neural-engine-mobilenet-v1"
	exe, err := s.program(ctx, "ane-bench")
	if err != nil {
		return all([]string{id}, because(Error))
	}
	model, want, err := s.coreMLModel(ctx, "mobilenet-v1-coreml")
	if err != nil {
		return all([]string{id}, because(Error))
	}
	out, _, err := s.tool(ctx, 15*time.Minute, exe, model)
	var j struct {
		Value     float64 `json:"inferences_per_s"`
		Placement string  `json:"placement"`
		Model     string  `json:"model_sha256"`
	}
	if err != nil || json.Unmarshal([]byte(lastLine(out)), &j) != nil || j.Model != want {
		return all([]string{id}, because(Error))
	}
	if j.Placement != "neural_engine" {
		return all([]string{id}, because(FellBack))
	}
	if j.Value <= 0 {
		return all([]string{id}, because(Error))
	}
	return map[string]outcome{id: measured(j.Value)}
}

var reOffloaded = regexp.MustCompile(`offloaded (\d+)/(\d+) layers to GPU`)

type llamaTest struct {
	Prompt   int     `json:"n_prompt"`
	Gen      int     `json:"n_gen"`
	TS       float64 `json:"avg_ts"`
	TSDev    float64 `json:"stddev_ts"`
	Backends string  `json:"backends"`
}

// llamaAllLayers offloads every layer of any model, as on Omarchy.
const llamaAllLayers = 999

// llama runs llama-bench on the pinned model with gpuLayers offloaded, and
// says how many of its layers went to the GPU.
func (s *session) llama(ctx context.Context, prompt, gen, gpuLayers int) ([]llamaTest, int, int, error) {
	exe, err := s.program(ctx, "llama.cpp")
	if err != nil {
		return nil, 0, 0, err
	}
	out, stderr, err := s.tool(ctx, 30*time.Minute, exe, "-m", s.file("llama-model"), "-p", strconv.Itoa(prompt),
		"-n", strconv.Itoa(gen), "-ngl", strconv.Itoa(gpuLayers), "-o", "json")
	if err != nil {
		return nil, 0, 0, err
	}
	var tests []llamaTest
	if err := json.Unmarshal([]byte(out), &tests); err != nil {
		return nil, 0, 0, err
	}
	m := reOffloaded.FindStringSubmatch(stderr)
	if m == nil {
		return tests, 0, -1, nil
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return tests, a, b, nil
}

func llamaGPU(s *session, ctx context.Context) map[string]outcome {
	ids := []string{"llama-prompt-gpu", "llama-gen-gpu"}
	prompt := int(s.setting(ids[0], "prompt_tokens", 512))
	gen := int(s.setting(ids[1], "generated_tokens", 128))
	tests, on, of, err := s.llama(ctx, prompt, gen, llamaAllLayers)
	switch {
	case err != nil:
		return all(ids, because(Error))
	case of <= 0 || on != of:
		return all(ids, because(FellBack))
	}
	res := map[string]outcome{}
	for _, t := range tests {
		if !strings.Contains(t.Backends, "Metal") {
			return all(ids, because(FellBack))
		}
		o := outcome{value: t.TS, spread: spread(t.TSDev)}
		switch {
		case t.Prompt == prompt && t.Gen == 0:
			res[ids[0]] = o
		case t.Gen == gen && t.Prompt == 0:
			res[ids[1]] = o
		}
	}
	return res
}

func llamaCPU(s *session, ctx context.Context) map[string]outcome {
	id := "llama-gen-cpu"
	gen := int(s.setting(id, "generated_tokens", 128))
	tests, on, _, err := s.llama(ctx, 0, gen, 0)
	switch {
	case err != nil:
		return all([]string{id}, because(Error))
	case on != 0:
		return all([]string{id}, because(FellBack))
	}
	for _, t := range tests {
		if t.Gen == gen && t.Prompt == 0 {
			return map[string]outcome{id: {value: t.TS, spread: spread(t.TSDev)}}
		}
	}
	return all([]string{id}, because(Error))
}

// mlx is MLX's fp16 matmul on the GPU, synchronised before timing; with no
// Metal GPU it fell back.
func mlx(s *session, ctx context.Context) map[string]outcome {
	id := "mlx-matmul-fp16"
	exe, err := s.program(ctx, "mlx-bench")
	if err != nil {
		return all([]string{id}, because(Error))
	}
	out, _, err := s.tool(ctx, 15*time.Minute, exe)
	var j struct {
		Device string  `json:"device"`
		TFLOPS float64 `json:"tflops"`
		Error  string  `json:"error"`
	}
	if json.Unmarshal([]byte(lastLine(out)), &j) != nil {
		return all([]string{id}, because(Error))
	}
	switch {
	case j.Error == "no_gpu" || (err == nil && j.Device != "gpu"):
		return all([]string{id}, because(FellBack))
	case err != nil || j.TFLOPS <= 0:
		return all([]string{id}, because(Error))
	}
	return map[string]outcome{id: measured(j.TFLOPS)}
}

// --- Power: the SMC's PSTR through smc-power, on AC with the battery not
// charging; idle as mean watts, fixed work as the energy it took.

const (
	powerIdleSeconds = 60
	// powerCoreMarkIterations is the fixed work of power-all-core, CoreMark
	// iterations in all, shared among a thread per core, as on Omarchy.
	powerCoreMarkIterations = 4000000
	// powerValidShare is the share of the work's seconds that must have a
	// valid sample.
	powerValidShare = 0.8
)

type smcReading struct {
	Valid int     `json:"valid"`
	Watts float64 `json:"mean_watts"`
	Error string  `json:"error"`
}

// powerReady is why power cannot be compared now, or "".
func (s *session) powerReady(ctx context.Context) Reason {
	p, err := s.host().Power(ctx)
	switch {
	case err != nil || !p.OnAC:
		return NoComparableSensor
	case p.Charging:
		return BatteryCharging
	}
	return ""
}

// smc is one smc-power reading over seconds: its mean watts and valid
// samples, or why there is none. A Mac whose SMC has no usable PSTR has no
// comparable sensor.
func (s *session) smc(ctx context.Context, exe string, seconds int) (smcReading, Reason) {
	out, _, err := s.tool(ctx, time.Duration(seconds+30)*time.Second, exe, "--seconds", strconv.Itoa(seconds))
	var j smcReading
	perr := json.Unmarshal([]byte(lastLine(out)), &j)
	switch {
	case ctx.Err() != nil:
		return j, NotRun
	case perr != nil || j.Error == "usage":
		return j, Error
	case err != nil || j.Error != "" || j.Valid == 0 || j.Watts <= 0:
		return j, NoComparableSensor
	}
	return j, ""
}

func powerIdle(s *session, ctx context.Context) map[string]outcome {
	id := "power-idle"
	if why := s.powerReady(ctx); why != "" {
		return all([]string{id}, because(why))
	}
	exe, err := s.program(ctx, "smc-power")
	if err != nil {
		return all([]string{id}, because(Error))
	}
	secs := int(s.setting(id, "seconds", powerIdleSeconds))
	j, why := s.smc(ctx, exe, secs)
	if why == "" && float64(j.Valid) < powerValidShare*float64(secs) {
		why = NoComparableSensor
	}
	if why != "" {
		return all([]string{id}, because(why))
	}
	return map[string]outcome{id: measured(j.Watts)}
}

// energy is the joules the work took: smc-power reads a second at a time
// while the work runs, and the energy is their mean watts over the work's
// seconds. The work says how long it took, or why it does not count.
func (s *session) energy(ctx context.Context, id string, work func(context.Context) (float64, Reason)) map[string]outcome {
	if why := s.powerReady(ctx); why != "" {
		return all([]string{id}, because(why))
	}
	exe, err := s.program(ctx, "smc-power")
	if err != nil {
		return all([]string{id}, because(Error))
	}
	if _, why := s.smc(ctx, exe, 1); why != "" {
		return all([]string{id}, because(why))
	}
	wctx, stop := context.WithCancel(ctx)
	type sampled struct {
		watts []float64
		why   Reason
	}
	done := make(chan sampled, 1)
	go func() {
		var got sampled
		for wctx.Err() == nil {
			j, why := s.smc(wctx, exe, 1)
			switch {
			case why == NotRun:
			case why != "":
				got.why = why
			default:
				got.watts = append(got.watts, j.Watts)
			}
		}
		done <- got
	}()
	secs, why := work(ctx)
	stop()
	got := <-done
	switch {
	case why != "":
		return all([]string{id}, because(why))
	case got.why != "" && got.why != NoComparableSensor:
		return all([]string{id}, because(got.why))
	case float64(len(got.watts)) < math.Max(1, powerValidShare*math.Floor(secs)):
		return all([]string{id}, because(NoComparableSensor))
	}
	mean := 0.0
	for _, w := range got.watts {
		mean += w
	}
	mean /= float64(len(got.watts))
	return map[string]outcome{id: {value: mean * secs, seconds: secs}}
}

// powerPlayback is the energy of playing every frame of the pinned H.264
// clip, at its own rate, decoded in hardware.
func powerPlayback(s *session, ctx context.Context) map[string]outcome {
	id := "power-playback-1080p"
	return s.energy(ctx, id, func(ctx context.Context) (float64, Reason) {
		exe, err := s.program(ctx, "ffmpeg")
		if err != nil {
			return 0, Error
		}
		start := time.Now()
		_, stderr, err := s.tool(ctx, 15*time.Minute, exe, decodeArgs(s.file("clip-h264-1080p"), true)...)
		secs := time.Since(start).Seconds()
		frames, hw := decoded(stderr)
		switch {
		case err != nil || frames == 0:
			return 0, Error
		case !hw:
			return 0, FellBack
		}
		return secs, ""
	})
}

// powerAllCore is the energy of a fixed count of CoreMark iterations, a
// thread per core.
func powerAllCore(s *session, ctx context.Context) map[string]outcome {
	id := "power-all-core"
	return s.energy(ctx, id, func(ctx context.Context) (float64, Reason) {
		n := min(s.host().CPUs(), coremarkMaxThreads)
		total := int(s.setting(id, "iterations", powerCoreMarkIterations))
		start := time.Now()
		out, err := s.coremark(ctx, n, (total+n-1)/n)
		secs := time.Since(start).Seconds()
		if err != nil || reCoreMarkCRC.MatchString(out) || !strings.Contains(out, "Iterations") {
			return 0, Error
		}
		return secs, ""
	})
}
