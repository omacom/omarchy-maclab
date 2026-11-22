package macosrunner

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
	"github.com/omacom/omarchy-maclab/internal/api"
)

// fakeLab is labd as the runner sees it: one run, its files, the upload.
type fakeLab struct {
	t      *testing.T
	mu     sync.Mutex
	run    *api.MacOSRun
	files  map[string][]byte
	polls  []url.Values
	got    *Upload
	raw    []byte
	upPath string
	enrols int
	// enrolQuery is what the enrolment told labd beside its token.
	enrolQuery url.Values
	fetched    []string
	srv        *httptest.Server
}

func newFakeLab(t *testing.T, run *api.MacOSRun, files map[string][]byte) *fakeLab {
	f := &fakeLab{t: t, run: run, files: map[string][]byte{}}
	if run != nil {
		for name, b := range files {
			sha := sum(b)
			f.files[sha] = b
			run.Artifacts = append(run.Artifacts, api.MacOSArtifact{Name: name, SHA256: sha})
		}
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (f *fakeLab) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	auth := r.Header.Get("Authorization")
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/macos/runner/enroll":
		if auth != "Bearer mos_enr_once" || f.enrols > 0 {
			http.Error(w, "used", 401)
			return
		}
		f.enrols++
		f.enrolQuery = r.URL.Query()
		json.NewEncoder(w).Encode(api.MacOSEnrolled{Mac: "m2-max", Secret: "mos_secret"})
		return
	case auth != "Bearer mos_secret":
		http.Error(w, "unknown macOS runner secret", 401)
	case r.URL.Path == "/api/macos/runner/poll":
		f.polls = append(f.polls, r.URL.Query())
		json.NewEncoder(w).Encode(api.MacOSPoll{Run: f.run})
	case strings.HasPrefix(r.URL.Path, "/api/macos/runner/artifacts/"):
		sha := strings.TrimPrefix(r.URL.Path, "/api/macos/runner/artifacts/")
		f.fetched = append(f.fetched, sha)
		b, ok := f.files[sha]
		if !ok {
			http.Error(w, "no", 404)
			return
		}
		w.Write(b)
	case r.URL.Path == "/api/macos/runner/results" || strings.HasPrefix(r.URL.Path, "/api/macos/runner/performance/"):
		f.upPath = r.URL.Path
		f.raw, _ = io.ReadAll(r.Body)
		var up Upload
		json.Unmarshal(f.raw, &up)
		f.got = &up
		w.Write([]byte(`{}`))
	default:
		f.t.Errorf("asked %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", 404)
	}
}

// fakeHost is a Mac: a clock that moves when the run waits, and what the
// run reads and changes.
type fakeHost struct {
	mu         sync.Mutex
	now        time.Time
	slept      time.Duration
	power      Power
	busy       float64
	lpm        bool
	lpmSets    []bool
	bright     float64
	hasBright  bool
	brightSets []float64
	temps      []float64 // one per look; none is no sensor
	total      uint64
	free       uint64
	version    string
	build      string
	session    string
	booted     time.Time
	console    string
}

func newFakeHost() *fakeHost {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	return &fakeHost{now: now, power: Power{OnAC: true, Battery: true}, busy: 4, bright: 0.8, hasBright: true,
		total: 1 << 40, free: 600 << 30, version: "27.2.1", build: "25C56", session: "6A1E1C9B-boot-1",
		booted: now.Add(-time.Minute), console: "alice"}
}

func (h *fakeHost) Now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
func (h *fakeHost) Sleep(_ context.Context, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
	h.slept += d
}
func (h *fakeHost) SWVers(context.Context) (string, string, error) { return h.version, h.build, nil }
func (h *fakeHost) Boot(context.Context) (string, time.Time, error) {
	return h.session, h.booted, nil
}
func (h *fakeHost) Power(context.Context) (Power, error)                    { return h.power, nil }
func (h *fakeHost) CPUBusy(context.Context, time.Duration) (float64, error) { return h.busy, nil }
func (h *fakeHost) LowPowerMode(context.Context) (bool, error)              { return h.lpm, nil }
func (h *fakeHost) SetLowPowerMode(_ context.Context, on bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lpm = on
	h.lpmSets = append(h.lpmSets, on)
	return nil
}
func (h *fakeHost) Brightness(context.Context) (float64, bool, error) {
	return h.bright, h.hasBright, nil
}
func (h *fakeHost) SetBrightness(_ context.Context, v float64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bright = v
	h.brightSets = append(h.brightSets, v)
	return nil
}
func (h *fakeHost) Temperature(context.Context) (float64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.temps) == 0 {
		return 0, false
	}
	t := h.temps[0]
	if len(h.temps) > 1 {
		h.temps = h.temps[1:]
	}
	return t, true
}
func (h *fakeHost) Disk(string) (uint64, uint64, error) { return h.total, h.free, nil }
func (h *fakeHost) SSD(context.Context) (uint64, error) { return 1_000_555_581_440, nil }
func (h *fakeHost) CPUs() int                           { return 12 }
func (h *fakeHost) ConsoleUser(context.Context) string  { return "" }

// fakeDevtools is Chrome's DevTools as the runner reads it: a page that
// answers the runner's expressions.
type fakeDevtools struct {
	mu       sync.Mutex
	renderer string
	hz       float64
	evals    []string
	srv      *httptest.Server
}

func newFakeDevtools(t *testing.T) *fakeDevtools {
	d := &fakeDevtools{renderer: "ANGLE (Apple, ANGLE Metal Renderer: Apple M2 Max, Unspecified Version)", hz: 60}
	d.srv = httptest.NewServer(http.HandlerFunc(d.serve))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDevtools) port() string { return d.srv.URL[strings.LastIndex(d.srv.URL, ":")+1:] }

func (d *fakeDevtools) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/json/list":
		fmt.Fprintf(w, `[{"type":"service_worker","webSocketDebuggerUrl":"ws://%s/sw"},{"type":"page","webSocketDebuggerUrl":"ws://%s/devtools/page/1"}]`, r.Host, r.Host)
	case "/devtools/page/1":
		key := r.Header.Get("Sec-WebSocket-Key")
		conn, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		acc := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(acc[:]))
		c := &cdp{conn: conn, br: bufio.NewReader(brw)}
		for {
			msg, err := c.read()
			if err != nil {
				return
			}
			var m struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params struct {
					Expression string `json:"expression"`
				} `json:"params"`
			}
			json.Unmarshal(msg, &m)
			c.write(0x1, []byte(`{"method":"Runtime.consoleAPICalled","params":{"args":[{"value":"/Users/alice"}]}}`))
			if m.Method != "Runtime.evaluate" {
				c.write(0x1, []byte(fmt.Sprintf(`{"id":%d,"result":{}}`, m.ID)))
				continue
			}
			d.mu.Lock()
			d.evals = append(d.evals, m.Params.Expression)
			v := d.value(m.Params.Expression)
			d.mu.Unlock()
			b, _ := json.Marshal(map[string]any{"id": m.ID, "result": map[string]any{"result": map[string]any{"type": "object", "value": v}}})
			c.write(0x1, b)
		}
	default:
		http.NotFound(w, r)
	}
}

func (d *fakeDevtools) value(expr string) any {
	switch {
	case strings.Contains(expr, "UNMASKED_RENDERER"):
		return d.renderer
	case strings.Contains(expr, "requestAnimationFrame"):
		return d.hz
	case strings.Contains(expr, "benchmarkClient._metrics"):
		return map[string]any{"value": 400.5, "spread": 2.5}
	case strings.Contains(expr, "benchmarkRunnerClient.results"):
		return map[string]any{"value": 950.25, "spread": 30}
	case strings.Contains(expr, "geomean"):
		return map[string]any{"value": 260.75, "spread": nil}
	}
	return true
}

// tarGz is an archive of name to script or content, every file runnable.
func tarGz(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

const chromeEntry = "chrome-mac-arm64/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"

// chromeZip is Chrome for Testing as its zip has it, a framework link
// included.
func chromeZip(script string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: chromeEntry, Method: zip.Deflate}
	h.SetMode(0o755)
	w, _ := zw.CreateHeader(h)
	w.Write([]byte(script))
	l := &zip.FileHeader{Name: "chrome-mac-arm64/Google Chrome for Testing.app/Contents/Frameworks/Current"}
	l.SetMode(0o777 | os.ModeSymlink)
	w, _ = zw.CreateHeader(l)
	w.Write([]byte("../MacOS"))
	zw.Close()
	return buf.Bytes()
}

// The fake tools: each writes what it was asked to FAKE_TOOLS_LOG and
// prints what the real one does, with a user's name and paths in it, which
// must never reach the results. FAKE_* switches make one fall back.
const (
	fakeCoreMark = `echo "coremark $*" >> "$FAKE_TOOLS_LOG"
n="${1#M}"; iterations="$5"
echo "2K performance run parameters for coremark, run by alice in /Users/alice"
echo "Iterations/Sec   : $((n * 10000)).5"
if [ "$iterations" != 0 ]; then sleep 0.5; echo "ERROR! Must execute for at least 10 secs for a valid result!"; exit 0; fi
echo "Correct operation validated. See README.md for run and reporting rules."
echo "CoreMark 1.0 : $((n * 10000)).5 / Clang 17.0.0 -O2 / Heap"
`
	fake7zz = `echo "7zz $*" >> "$FAKE_TOOLS_LOG"
[ "$1" = b ] || exit 2
echo "7-Zip (z) 26.06 (arm64) : /Users/alice"
echo "Tot:             977   4554  44515"
`
	fakeClpeak = `echo "clpeak $*" >> "$FAKE_TOOLS_LOG"
cat <<EOF
Platform: ${FAKE_CL_PLATFORM:-Apple}
  Device: ${FAKE_CL_DEVICE:-Apple M2 Max}
    Driver version  : 1.2 1.0 (Macintosh)

    Global memory bandwidth (GBPS)
      float   : 300.5
      float2  : 350.25
      float4  : 340

    Single-precision compute (GFLOPS)
      float   : 12000
      float2  : 13500.75

EOF
`
	fakeFio = `echo "fio $*" >> "$FAKE_TOOLS_LOG"
file=""; rw=""; for a in "$@"; do case "$a" in --filename=*) file="${a#--filename=}";; --rw=*) rw="${a#--rw=}";; esac; done
[ -n "$file" ] && : >> "$file"
[ "$rw" = randread ] && [ -n "$FAKE_FIO_SLEEP" ] && sleep "$FAKE_FIO_SLEEP"
echo '{"fio version":"fio-3.41","jobs":[{"jobname":"/Users/alice","job options":{"direct":"'"${FAKE_FIO_DIRECT:-1}"'"},"read":{"bw_bytes":3000000000,"bw_dev":1000,"iops":500000,"iops_stddev":2000},"write":{"bw_bytes":2000000000,"bw_dev":500,"iops":300000,"iops_stddev":1000}}]}'
`
	fakeStream = `echo "stream $*" >> "$FAKE_TOOLS_LOG"
cat <<EOF
STREAM version 5.10, on alice's Mac
Function    Best Rate MB/s  Avg time     Min time     Max time
Copy:          110000.5     0.012        0.011        0.013
Scale:         100000.0     0.012        0.011        0.013
Add:           120000.0     0.012        0.011        0.013
Triad:         121000.4     0.012        0.011        0.013
Solution Validates: avg error less than 1.000000e-13 on all three arrays
EOF
`
	fakeFFmpeg = `echo "ffmpeg $*" >> "$FAKE_TOOLS_LOG"
echo "$*" | grep -q -- "-hwaccel videotoolbox" || exit 3
echo "$*" | grep -q -- "-re " && sleep 0.5
fmt=videotoolbox_vld; [ -n "$FAKE_FFMPEG_SW" ] && fmt=yuv420p
cat >&2 <<EOF
Input #0, mov,mp4,m4a,3gp,3g2,mj2, from '/Users/alice/clip.mp4':
  Stream #0:0: Video: h264 (High), yuv420p, 1920x1080
Output #0, null, to 'pipe:':
  Stream #0:0: Video: wrapped_avframe, $fmt(tv, bt709), 1920x1080
frame=  120 fps=0.0 q=-0.0 size=N/A
frame=  600 fps=300 q=-0.0 Lsize=N/A
bench: utime=0.500s stime=0.100s rtime=2.000s
EOF
`
	fakeLlama = `echo "llama-bench $*" >> "$FAKE_TOOLS_LOG"
ngl=0; p=0; n=0; while [ $# -gt 0 ]; do case "$1" in -ngl) ngl="$2"; shift;; -p) p="$2"; shift;; -n) n="$2"; shift;; esac; shift; done
if [ "$ngl" = 0 ]; then off=0; ts=30.25; else off="${FAKE_LLAMA_OFFLOADED:-29}"; ts=80.5; fi
echo "load_tensors: offloaded $off/29 layers to GPU (/Users/alice/model.gguf)" >&2
out="["
[ "$p" != 0 ] && out="$out{\"model_filename\":\"/Users/alice/m.gguf\",\"n_prompt\":$p,\"n_gen\":0,\"avg_ts\":900.5,\"stddev_ts\":5.5,\"backends\":\"Metal,BLAS\"},"
echo "$out{\"n_prompt\":0,\"n_gen\":$n,\"avg_ts\":$ts,\"stddev_ts\":0.5,\"backends\":\"Metal,BLAS\"}]"
`
	fakeMLX = `echo "mlx-bench $*" >> "$FAKE_TOOLS_LOG"
echo "warming up on alice's GPU" >&2
[ -n "$FAKE_MLX_NOGPU" ] && { echo '{"error":"no_gpu"}'; exit 1; }
echo '{"device":"gpu","dtype":"float16","n":4096,"warmup":5,"iterations":50,"seconds":0.5,"tflops":13.5,"mlx":"0.29.1"}'
`
	fakeSMC = `echo "smc-power $*" >> "$FAKE_TOOLS_LOG"
[ "$1" = --seconds ] || { echo '{"key":"PSTR","error":"usage"}'; exit 2; }
[ -n "$FAKE_SMC_ABSENT" ] && { echo "no PSTR key on /Users/alice's Mac" >&2; echo '{"key":"PSTR","error":"no_pstr"}'; exit 4; }
w=7.25; [ "$2" = 1 ] && { w=20.5; sleep 0.2; }
echo "{\"key\":\"PSTR\",\"seconds\":$2,\"samples\":$2,\"valid\":$2,\"mean_watts\":$w}"
`
)

// fakeChrome writes the DevTools port into its profile, as Chrome does, and
// fetches its page from the runner.
func fakeChrome(port string) string {
	return `echo "chrome $*" >> "$FAKE_TOOLS_LOG"
printf '%s\n' "$@" > "$FAKE_TOOLS_LOG.chrome-args"
profile=""; url=""; for a in "$@"; do case "$a" in --user-data-dir=*) profile="${a#--user-data-dir=}";; http://*) url="$a";; esac; done
curl -fsS "$url" >> "$FAKE_TOOLS_LOG.pages" || exit 1
printf '` + port + `\n/devtools/browser/x\n' > "$profile/DevToolsActivePort"
sleep 30
`
}

func fakeANE(modelSHA string) string {
	return `echo "ane-bench $*" >> "$FAKE_TOOLS_LOG"
[ -d "$1" ] || { echo '{"error":"model_load"}'; exit 1; }
echo "loading /Users/alice/model" >&2
echo '{"compute_units":"cpu_and_neural_engine","inferences_per_s":2500.5,"iterations":200,"model_sha256":"` + modelSHA + `","ops":31,"ops_neural_engine":31,"placement":"'"${FAKE_ANE_PLACEMENT:-neural_engine}"'","seconds":0.08,"warmup":20}'
`
}

// coreMLModel is a compiled Core ML model, as the set pins it, and the
// model_sha256 ane-bench gives for it.
func coreMLModel(t *testing.T) ([]byte, string) {
	b := tarGz(map[string]string{
		"MobileNetV1.mlmodelc/model.mil":            "program(1.0)",
		"MobileNetV1.mlmodelc/weights/weight.bin":   "weights",
		"MobileNetV1.mlmodelc/metadata.json":        "{}",
		"MobileNetV1.mlmodelc/analytics/coremldata": "x",
	})
	f := filepath.Join(t.TempDir(), "model")
	os.WriteFile(f, b, 0o600)
	d := t.TempDir()
	if err := unpack(context.Background(), f, d); err != nil {
		t.Fatal(err)
	}
	sum, err := modelDigest(filepath.Join(d, "MobileNetV1.mlmodelc"))
	if err != nil {
		t.Fatal(err)
	}
	return b, sum
}

// bench is a Benchmark set and its macOS files as the fakes: bench-1 with
// every macOS file but one pinned to a fake, the HEVC clip still a
// placeholder.
type bench struct {
	set   *sets.Set
	files map[string][]byte
	log   string
	dt    *fakeDevtools
}

func newBench(t *testing.T) *bench {
	t.Helper()
	set, err := sets.Load("bench-1")
	if err != nil {
		t.Fatal(err)
	}
	dt := newFakeDevtools(t)
	model, modelSHA := coreMLModel(t)
	sh := func(s string) string { return "#!/bin/sh\n" + s }
	web := func(id, content string) []byte {
		f, _ := set.File(sets.MacOS, id)
		return tarGz(map[string]string{filepath.Join(f.Root, f.Entry): content, filepath.Join(f.Root, "resources/x.js"): "1"})
	}
	files := map[string][]byte{
		"coremark":            tarGz(map[string]string{"coremark": sh(fakeCoreMark)}),
		"7zip":                tarGz(map[string]string{"7zz": sh(fake7zz)}),
		"chrome":              chromeZip(sh(fakeChrome(dt.port()))),
		"motionmark":          web("motionmark", "<html>MotionMark 1.3</html>"),
		"clpeak":              tarGz(map[string]string{"clpeak": sh(fakeClpeak)}),
		"fio":                 tarGz(map[string]string{"fio": sh(fakeFio)}),
		"stream":              tarGz(map[string]string{"stream": sh(fakeStream)}),
		"speedometer":         web("speedometer", "<html>Speedometer 3.1</html>"),
		"jetstream":           web("jetstream", "<html>JetStream 2.2</html>"),
		"ffmpeg":              tarGz(map[string]string{"ffmpeg": sh(fakeFFmpeg)}),
		"clip-h264-1080p":     []byte("not really a 1080p clip"),
		"ane-bench":           tarGz(map[string]string{"ane-bench": sh(fakeANE(modelSHA))}),
		"mobilenet-v1-coreml": model,
		"llama.cpp":           tarGz(map[string]string{"llama-bench": sh(fakeLlama)}),
		"llama-model":         []byte("a 3B Q4_K_M GGUF"),
		"mlx-bench":           tarGz(map[string]string{"mlx-bench": sh(fakeMLX)}),
		"smc-power":           tarGz(map[string]string{"smc-power": sh(fakeSMC)}),
	}
	for i, f := range set.Files[sets.MacOS] {
		b, ok := files[f.ID]
		if !ok {
			f.SHA256, f.Size = sets.Placeholder, 0
		} else {
			f.SHA256, f.Size, f.URL = sum(b), int64(len(b)), "https://example.com/"+f.ID
		}
		set.Files[sets.MacOS][i] = f
	}
	log := filepath.Join(t.TempDir(), "tools.log")
	t.Setenv("FAKE_TOOLS_LOG", log)
	return &bench{set: set, files: files, log: log, dt: dt}
}

// ran is what the fake tools were asked, a line each.
func (b *bench) ran() string {
	out, _ := os.ReadFile(b.log)
	return string(out)
}

// startedRun is a run labd started a moment before the fake host's boot.
func startedRun(h *fakeHost, id, tier string) *api.MacOSRun {
	at := h.booted.Add(-40 * time.Second)
	dl := at.Add(time.Hour)
	return &api.MacOSRun{ID: id, Mac: "m2-max", Tier: tier, State: api.MacOSRunning, Started: &at, Deadline: &dl}
}

func newRunner(t *testing.T, lab *fakeLab, h *fakeHost, b *bench) (*Runner, *int) {
	rebooted := new(int)
	r := &Runner{
		Config:         Config{Server: lab.srv.URL, Mac: "m2-max", Secret: "mos_secret"},
		Dir:            t.TempDir(),
		ShareDir:       t.TempDir(),
		Host:           h,
		Reboot:         func() error { *rebooted++; return nil },
		BrowserTimeout: 20 * time.Second,
		Log:            t.Logf,
	}
	if b != nil {
		r.Set = func(name string) (*sets.Set, error) {
			if name != "" && name != b.set.Name {
				return nil, fmt.Errorf("no set %s", name)
			}
			return b.set, nil
		}
	}
	return r, rebooted
}

func byMetric(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.Metric] = r
	}
	return m
}

func val(r Result) float64 {
	if r.Value == nil {
		return -1
	}
	return *r.Value
}

func itoa(n int) string { return strconv.Itoa(n) }
