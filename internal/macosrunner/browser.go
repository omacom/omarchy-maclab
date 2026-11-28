package macosrunner

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent/tests/bench/sets"
)

// The web benchmarks, as Omarchy's runner drives them: the pinned page
// served on 127.0.0.1, the pinned Chrome for Testing headless on the GPU,
// and the page's own result read over the DevTools protocol.
type webApp struct {
	query, ready, start, done, result string
	timeout                           time.Duration
}

var webApps = map[string]webApp{
	"speedometer": {
		query:   "?startAutomatically&iterationCount=10",
		done:    "!!(globalThis.benchmarkClient && benchmarkClient._hasResults && !benchmarkClient._isRunning)",
		result:  "(() => { const s = benchmarkClient._metrics.Score; return s ? {value: s.mean, spread: s.delta || null} : {value: null}; })()",
		timeout: 15 * time.Minute,
	},
	"motionmark": {
		ready:   "!!(document.getElementById('start-button') && !document.getElementById('start-button').disabled)",
		start:   "benchmarkController.startBenchmark(); true",
		done:    "document.body.classList.contains('showing-results')",
		result:  "(() => { const r = benchmarkRunnerClient.results; return {value: r.score, spread: r.score - r.scoreLowerBound}; })()",
		timeout: 15 * time.Minute,
	},
	"jetstream": {
		ready:   "!!document.querySelector('#status a.button')",
		start:   "JetStream.start(); true",
		done:    "document.getElementById('result-summary').classList.contains('done')",
		result:  "({value: geomean(JetStream.benchmarks.map(b => b.score)), spread: null})",
		timeout: 20 * time.Minute,
	},
}

// browserFlags are Omarchy's: headless on the GPU at 1920 by 1080, device
// pixel ratio 2, so the two systems' browser rows compare like for like.
var browserFlags = []string{
	"--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-extensions",
	"--disable-background-timer-throttling", "--disable-renderer-backgrounding",
	"--headless", "--enable-gpu", "--window-size=1920,1080",
	"--screen-info={0,0 3840x2160 devicePixelRatio=2}",
	"--remote-debugging-port=0", "--remote-allow-origins=*",
}

const glRenderer = "(() => { const gl = document.createElement('canvas').getContext('webgl2');" +
	" const e = gl && gl.getExtension('WEBGL_debug_renderer_info');" +
	" return e ? gl.getParameter(e.UNMASKED_RENDERER_WEBGL) : null; })()"

// frameRate is the page's effective frame rate over 120 frames.
const frameRate = "new Promise(done => { let n = 0, t0 = 0; const f = t => { if (!t0) t0 = t;" +
	" if (++n > 120) done(1000 * (n - 1) / (t - t0)); else requestAnimationFrame(f); }; requestAnimationFrame(f); })"

// drawRate is frameRate's measure while a canvas the size of the screen is
// redrawn every frame, Omarchy's DRAW_RATE: from bench-4 the rate MotionMark
// and Speedometer count, on about:blank before the page loads.
const drawRate = "new Promise(r => { const c = document.createElement('canvas');" +
	" c.width = innerWidth * devicePixelRatio; c.height = innerHeight * devicePixelRatio;" +
	" c.style.cssText = 'position:fixed;left:0;top:0;width:100vw;height:100vh';" +
	" document.body.appendChild(c); const g = c.getContext('2d');" +
	" let n = 0; let t0 = null; function f(t) {" +
	" g.fillStyle = 'hsl(' + (t / 5 % 360) + ',70%,50%)'; g.fillRect(0, 0, c.width, c.height);" +
	" for (let i = 0; i < 200; i++) { g.fillStyle = 'hsl(' + (i * 7 + t / 3) % 360 + ',80%,60%)';" +
	" g.fillRect((i * 97 + t) % c.width, (i * 53 + t / 2) % c.height, 120, 120); }" +
	" if (t0 === null) t0 = t; else n++;" +
	" if (t - t0 >= 2000) { c.remove(); r(n * 1000 / (t - t0)); } else requestAnimationFrame(f); }" +
	" requestAnimationFrame(f); })"

var reSoftwareGL = regexp.MustCompile(`(?i)swiftshader|llvmpipe|softpipe|software|basic render`)

// The page's frame rate must be within a tenth of 60 Hz, as on Omarchy:
// MotionMark scales its work to it.
const hzWanted, hzSlack = 60.0, 0.1

func browserStep(page string) func(*session, context.Context) map[string]outcome {
	return func(s *session, ctx context.Context) map[string]outcome {
		return map[string]outcome{page: s.browser(ctx, page)}
	}
}

// browser runs one web benchmark: the page served from its pinned files,
// Chrome for Testing in a fresh profile owned by the logged-in user.
func (s *session) browser(ctx context.Context, page string) outcome {
	app := webApps[page]
	exe, err := s.program(ctx, "chrome")
	if err != nil {
		fmt.Fprintf(s.log, "%s: %v\n", page, err)
		return because(Error)
	}
	root, err := s.dir(ctx, page)
	if err != nil {
		fmt.Fprintf(s.log, "%s: %v\n", page, err)
		return because(Error)
	}
	f, _ := s.set.File(sets.MacOS, page)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return because(Error)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(filepath.Join(root, f.Root))), ErrorLog: nil}
	go srv.Serve(ln)
	defer srv.Close()

	profile, err := os.MkdirTemp(s.share, "profile-")
	if err == nil {
		err = chownTo(s.user, profile)
	}
	if err != nil {
		fmt.Fprintf(s.log, "%s: the profile: %v\n", page, err)
		return because(Error)
	}
	defer os.RemoveAll(profile)
	// The folder, not its index.html, which Go's file server redirects.
	u := fmt.Sprintf("http://%s/%s%s", ln.Addr(), strings.TrimSuffix(or(f.Entry, "index.html"), "index.html"), app.query)
	drawn := s.drawProbe(page)
	first := u
	if drawn {
		first = "about:blank"
	}
	args := asUser(s.user, append(append([]string{exe, "--user-data-dir=" + profile}, browserFlags...), first))
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout, cmd.Stderr = s.log, s.log
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(s.log, "%s: %v\n", page, err)
		return because(Error)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	defer func() {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}()
	timeout := app.timeout
	if s.r.BrowserTimeout > 0 {
		timeout = s.r.BrowserTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	o, err := s.drive(ctx, page, app, profile, exited, drawn, u)
	if err != nil {
		fmt.Fprintf(s.log, "%s: %v\n", page, err)
	}
	return o
}

// drawProbe is whether the metric's frame rate is the draw probe's.
func (s *session) drawProbe(metric string) bool {
	m, ok := s.set.Metric(metric)
	return ok && m.Settings["refresh_measured_on"] == "draw_probe"
}

// drive measures the frame rate, on the page it opened or, drawn, with the
// draw probe before going to u, and runs the benchmark.
func (s *session) drive(ctx context.Context, page string, app webApp, profile string, exited <-chan struct{}, drawn bool, u string) (outcome, error) {
	wait := func(what string, step time.Duration, fn func() bool) error {
		for !fn() {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s: %w", what, ctx.Err())
			case <-exited:
				return fmt.Errorf("%s: the browser ended", what)
			case <-time.After(step):
			}
		}
		return nil
	}
	var port int
	if err := wait("the DevTools port", 200*time.Millisecond, func() bool {
		port = devtoolsPort(profile)
		return port > 0
	}); err != nil {
		return because(Error), err
	}
	ws, err := pageSocket(ctx, port)
	if err != nil {
		return because(Error), err
	}
	c, err := dialCDP(ctx, ws)
	if err != nil {
		return because(Error), err
	}
	defer c.close()
	defer c.call("Browser.close", nil)
	var renderer string
	wait("the GL renderer", time.Second, func() bool {
		c.eval(glRenderer, false, &renderer)
		return renderer != ""
	})
	if renderer == "" || reSoftwareGL.MatchString(renderer) {
		return because(SoftwareRenderer), errors.New("the page is not drawn on the GPU")
	}
	rate := frameRate
	if drawn {
		rate = drawRate
	}
	var hz float64
	if err := c.eval(rate, true, &hz); err != nil {
		return because(Error), err
	}
	if c := s.conditionsOut; c != nil && c.RefreshHz == nil {
		c.RefreshHz = spread(round(hz, 1))
	}
	if hz < hzWanted*(1-hzSlack) || hz > hzWanted*(1+hzSlack) {
		return because(FellBack), fmt.Errorf("the page draws at %.1f Hz", hz)
	}
	if drawn {
		if _, err := c.call("Page.navigate", map[string]any{"url": u}); err != nil {
			return because(Error), err
		}
	}
	truthy := func(expr string) func() bool {
		return func() bool {
			var ok bool
			return c.eval(expr, false, &ok) == nil && ok
		}
	}
	if app.ready != "" {
		if err := wait("the start button", 2*time.Second, truthy(app.ready)); err != nil {
			return because(Error), err
		}
		var ignored any
		if err := c.eval(app.start, false, &ignored); err != nil {
			return because(Error), err
		}
	}
	if err := wait(page, 5*time.Second, truthy(app.done)); err != nil {
		return because(Error), err
	}
	var res struct {
		Value  *float64 `json:"value"`
		Spread *float64 `json:"spread"`
	}
	if err := c.eval(app.result, false, &res); err != nil {
		return because(Error), err
	}
	if res.Value == nil || *res.Value <= 0 {
		return because(Error), errors.New("the page gave no result")
	}
	return outcome{value: *res.Value, spread: res.Spread}, nil
}

// devtoolsPort is the port Chrome wrote in the profile, or 0.
func devtoolsPort(profile string) int {
	b, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
	if err != nil {
		return 0
	}
	line, _, _ := strings.Cut(string(b), "\n")
	p, _ := strconv.Atoi(strings.TrimSpace(line))
	return p
}

// pageSocket is the DevTools address of the browser's page.
func pageSocket(ctx context.Context, port int) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/json/list", port), nil)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var pages []struct {
		Type string `json:"type"`
		WS   string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pages); err != nil {
		return "", err
	}
	for _, p := range pages {
		if p.Type == "page" && strings.HasPrefix(p.WS, "ws://127.0.0.1:") {
			return p.WS, nil
		}
	}
	return "", errors.New("the browser has no page")
}

// cdp is a DevTools protocol connection: a WebSocket, text frames only.
type cdp struct {
	conn net.Conn
	br   *bufio.Reader
	id   int
}

func dialCDP(ctx context.Context, ws string) (*cdp, error) {
	u, err := url.Parse(ws)
	if err != nil || u.Scheme != "ws" {
		return nil, fmt.Errorf("DevTools address %q", ws)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", u.RequestURI(), u.Host, k)
	br := bufio.NewReader(conn)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	sum := sha1.Sum([]byte(k + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.StatusCode != 101 || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		conn.Close()
		return nil, fmt.Errorf("DevTools refused the WebSocket: %s", resp.Status)
	}
	conn.SetDeadline(time.Time{})
	return &cdp{conn: conn, br: br}, nil
}

func (c *cdp) close() { c.conn.Close() }

// call sends one command and waits for its answer, skipping events.
func (c *cdp) call(method string, params any) (json.RawMessage, error) {
	c.id++
	b, _ := json.Marshal(map[string]any{"id": c.id, "method": method, "params": params})
	c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	defer c.conn.SetDeadline(time.Time{})
	if err := c.write(0x1, b); err != nil {
		return nil, err
	}
	for {
		msg, err := c.read()
		if err != nil {
			return nil, err
		}
		var m struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(msg, &m) != nil || m.ID != c.id {
			continue
		}
		if m.Error != nil {
			return nil, errors.New("DevTools: " + m.Error.Message)
		}
		return m.Result, nil
	}
}

// eval evaluates expr in the page, waiting for a promise when await, into v.
func (c *cdp) eval(expr string, await bool, v any) error {
	raw, err := c.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true, "awaitPromise": await})
	if err != nil {
		return err
	}
	var r struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if r.Exception != nil {
		return errors.New("the page threw")
	}
	if len(r.Result.Value) == 0 {
		return errors.New("no value")
	}
	return json.Unmarshal(r.Result.Value, v)
}

func (c *cdp) write(op byte, payload []byte) error {
	head := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n < 1<<16:
		head = append(head, 0x80|126, byte(n>>8), byte(n))
	default:
		head = binary.BigEndian.AppendUint64(append(head, 0x80|127), uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	head = append(head, mask...)
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	_, err := c.conn.Write(append(head, masked...))
	return err
}

// read is the next whole text message; pings are answered.
func (c *cdp) read() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > 64<<20 {
			return nil, errors.New("DevTools message too large")
		}
		var mask []byte
		if h[1]&0x80 != 0 {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(c.br, mask); err != nil {
				return nil, err
			}
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.br, p); err != nil {
			return nil, err
		}
		for i := range p {
			if mask != nil {
				p[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x8:
			return nil, errors.New("DevTools closed the connection")
		case 0x9:
			c.write(0xA, p)
			continue
		case 0xA:
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}
