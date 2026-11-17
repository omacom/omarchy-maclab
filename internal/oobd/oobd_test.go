package oobd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/oob"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type toolRun struct {
	rid     int
	command string
}

// fakeTool stands in for the reset tool: it writes down what it was asked and
// how many runs were in flight at once.
type fakeTool struct {
	mu      sync.Mutex
	runs    []toolRun
	running int
	most    int
	hold    chan struct{}
	started chan toolRun
	fail    map[int]string
}

func (f *fakeTool) Run(ctx context.Context, rid int, command string) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, toolRun{rid, command})
	f.running++
	if f.running > f.most {
		f.most = f.running
	}
	hold, started, failure := f.hold, f.started, f.fail[rid]
	f.mu.Unlock()
	if started != nil {
		started <- toolRun{rid, command}
	}
	if hold != nil {
		<-hold
	}
	f.mu.Lock()
	f.running--
	f.mu.Unlock()
	if failure != "" {
		return failure + "\n", errors.New("exit status 255")
	}
	return fmt.Sprintf("Found RID %d\nConnection: Source\nStatus: DBMa\n", rid), nil
}

func (f *fakeTool) seen() []toolRun {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]toolRun(nil), f.runs...)
}

type fakePorts struct {
	mu    sync.Mutex
	ports map[int]oob.Port
	reads int
}

func (f *fakePorts) Read(ctx context.Context, rid int) (oob.Port, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	p, ok := f.ports[rid]
	if !ok {
		return oob.Port{}, fmt.Errorf("this machine has no port with RID %d", rid)
	}
	return p, nil
}

func (f *fakePorts) Look(ctx context.Context, rid int) (oob.Port, error) {
	p, err := f.Read(ctx, rid)
	p.Name = ""
	return p, err
}

func (f *fakePorts) set(p oob.Port) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ports[p.RID] = p
}

// theMini is the mini's three rear ports as they were read on 2026-11-14:
// the M2 Max on the middle one with no Thunderbolt link, the left one empty,
// the M1 Pro on the right one.
func theMini() *fakePorts {
	return &fakePorts{ports: map[int]oob.Port{
		0: {RID: 0, Connected: true, ProductID: 29453, PlugCount: 42},
		1: {RID: 1, PlugCount: 27},
		2: {RID: 2, Connected: true, Name: "omarchy-m1-pro", ProductID: 29448, PlugCount: 40},
	}}
}

type rig struct {
	t     *testing.T
	tool  *fakeTool
	ports *fakePorts
	srv   *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, tool: &fakeTool{}, ports: theMini()}
	s, err := New(Config{Token: testToken, Tool: r.tool, Ports: r.ports}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer(s.Handler())
	t.Cleanup(r.srv.Close)
	return r
}

// holdTool makes every tool run wait for the test to let it go, and lets all
// of them go when the test ends, however it ends.
func (r *rig) holdTool() {
	hold := make(chan struct{})
	r.tool.hold = hold
	r.tool.started = make(chan toolRun, 8)
	r.t.Cleanup(func() { close(hold) })
}

func (r *rig) do(method, path, token, body string) (int, string) {
	r.t.Helper()
	req, err := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestAResetNamesItsPortAndRunsTheToolOnThatPort(t *testing.T) {
	r := newRig(t)
	for _, rid := range []int{2, 0, 1} {
		if code, body := r.do("POST", fmt.Sprintf("/v1/ports/%d/reset", rid), testToken, ""); code != 200 {
			t.Fatalf("reset of RID %d: %d %s", rid, code, body)
		}
	}
	want := []toolRun{{2, "reboot"}, {0, "reboot"}, {1, "reboot"}}
	if got := r.tool.seen(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("the tool ran %v, want %v", got, want)
	}
}

func TestNoRequestGoesWithoutAPort(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/reset"},
		{"GET", "/v1/status"},
		{"POST", "/v1/ports/reset"},
		{"GET", "/v1/ports/status"},
		{"GET", "/v1/ports"},
		{"POST", "/v1/serial/arm"},
		{"GET", "/v1/serial"},
		{"GET", "/v1/serial/raw"},
		{"POST", "/v1/serial/write"},
		{"GET", "/v1/camera"},
		{"GET", "/v1/camera/stream"},
	} {
		if code, body := r.do(c.method, c.path, testToken, ""); code != 404 {
			t.Errorf("%s %s: %d %s, want 404", c.method, c.path, code, body)
		}
	}
	if runs := r.tool.seen(); len(runs) != 0 {
		t.Fatalf("the tool ran with no port named: %v", runs)
	}
}

func TestARIDOutsideTheListIsRefusedBeforeTheTool(t *testing.T) {
	r := newRig(t)
	for _, rid := range []string{"3", "5", "-1", "x", "1a", "01", "+1", "0x1", "1.0", "%201", "1%20", "4294967296", "99999999999999999999"} {
		for _, c := range []struct{ method, what string }{{"POST", "reset"}, {"GET", "status"}, {"GET", "status?probe=1"}} {
			code, body := r.do(c.method, "/v1/ports/"+rid+"/"+c.what, testToken, "")
			if code != 400 {
				t.Errorf("%s of RID %q: %d %s, want 400", c.what, rid, code, body)
			}
			if code == 400 && !strings.Contains(body, "0, 1, 2") {
				t.Errorf("%s of RID %q: the refusal doesn't name the ports this machine has: %s", c.what, rid, body)
			}
		}
	}
	if runs := r.tool.seen(); len(runs) != 0 {
		t.Fatalf("the tool ran for a refused RID: %v", runs)
	}
	if r.ports.reads != 0 {
		t.Fatalf("a refused RID was looked for on %d ports", r.ports.reads)
	}
}

func TestTwoResetsAskedTogetherRunOneAfterTheOther(t *testing.T) {
	r := newRig(t)
	r.holdTool()

	codes := make(chan int, 2)
	for _, rid := range []int{0, 2} {
		go func() {
			code, _ := r.do("POST", fmt.Sprintf("/v1/ports/%d/reset", rid), testToken, "")
			codes <- code
		}()
	}
	first := <-r.tool.started
	select {
	case second := <-r.tool.started:
		t.Fatalf("RID %d was reset while the reset of RID %d was still running", second.rid, first.rid)
	case <-time.After(300 * time.Millisecond):
	}
	r.tool.hold <- struct{}{}
	second := <-r.tool.started
	r.tool.hold <- struct{}{}
	for range 2 {
		if code := <-codes; code != 200 {
			t.Fatalf("a reset answered %d", code)
		}
	}
	if first.rid == second.rid {
		t.Fatalf("RID %d was reset twice", first.rid)
	}
	if r.tool.most != 1 {
		t.Fatalf("%d tool runs at once", r.tool.most)
	}
	if runs := r.tool.seen(); len(runs) != 2 {
		t.Fatalf("each Mac is reset once; the tool ran %v", runs)
	}
}

func TestAProbeWaitsForAResetToo(t *testing.T) {
	r := newRig(t)
	r.holdTool()
	done := make(chan int, 2)
	go func() { code, _ := r.do("POST", "/v1/ports/2/reset", testToken, ""); done <- code }()
	<-r.tool.started
	go func() { code, _ := r.do("GET", "/v1/ports/0/status?probe=1", testToken, ""); done <- code }()
	select {
	case run := <-r.tool.started:
		t.Fatalf("%v ran during a reset", run)
	case <-time.After(300 * time.Millisecond):
	}
	// What the mini sees needs no tool run, so it answers during a reset.
	if code, body := r.do("GET", "/v1/ports/0/status", testToken, ""); code != 200 {
		t.Fatalf("status during a reset: %d %s", code, body)
	}
	r.tool.hold <- struct{}{}
	<-r.tool.started
	r.tool.hold <- struct{}{}
	<-done
	<-done
	if r.tool.most != 1 {
		t.Fatalf("%d tool runs at once", r.tool.most)
	}
}

func TestAResetNobodyWaitsForIsNotSent(t *testing.T) {
	r := newRig(t)
	r.holdTool()
	first := make(chan int, 1)
	go func() { code, _ := r.do("POST", "/v1/ports/2/reset", testToken, ""); first <- code }()
	<-r.tool.started

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", r.srv.URL+"/v1/ports/0/reset", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	gone := make(chan struct{})
	go func() { http.DefaultClient.Do(req); close(gone) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-gone
	time.Sleep(200 * time.Millisecond)

	r.tool.hold <- struct{}{}
	<-first
	select {
	case run := <-r.tool.started:
		t.Fatalf("%v was sent after its caller had gone", run)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStatusSaysWhatIsOnEachPortAndRunsNoTool(t *testing.T) {
	r := newRig(t)
	want := map[int]oob.Port{
		0: {RID: 0, Connected: true, ProductID: 29453, PlugCount: 42},
		1: {RID: 1, PlugCount: 27},
		2: {RID: 2, Connected: true, Name: "omarchy-m1-pro", ProductID: 29448, PlugCount: 40},
	}
	for rid, w := range want {
		code, body := r.do("GET", fmt.Sprintf("/v1/ports/%d/status", rid), testToken, "")
		if code != 200 {
			t.Fatalf("status of RID %d: %d %s", rid, code, body)
		}
		var got oob.Port
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("status of RID %d is not JSON: %s", rid, body)
		}
		if got != w {
			t.Errorf("RID %d: %+v, want %+v", rid, got, w)
		}
		for _, key := range []string{`"rid"`, `"connected"`, `"plug_count"`} {
			if !strings.Contains(body, key) {
				t.Errorf("RID %d: no %s in %s", rid, key, body)
			}
		}
	}
	if runs := r.tool.seen(); len(runs) != 0 {
		t.Fatalf("status ran the tool: %v", runs)
	}
}

func TestAProbeAsksTheToolForNothingButItsAnswer(t *testing.T) {
	r := newRig(t)
	r.tool.fail = map[int]string{1: "Found RID 1\nConnection: None\nNo connection detected"}
	for rid, failed := range map[int]bool{0: false, 1: true, 2: false} {
		code, body := r.do("GET", fmt.Sprintf("/v1/ports/%d/status?probe=1", rid), testToken, "")
		if code != 200 {
			t.Fatalf("probe of RID %d: %d %s", rid, code, body)
		}
		var got oob.Port
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.ToolFailed != failed || got.Tool == "" {
			t.Errorf("RID %d: tool %q, failed %v", rid, got.Tool, got.ToolFailed)
		}
	}
	for _, run := range r.tool.seen() {
		if run.command != "nop" {
			t.Fatalf("a probe ran %q", run.command)
		}
	}
	if len(r.tool.seen()) != 3 {
		t.Fatalf("the tool ran %v", r.tool.seen())
	}
}

// A display on CC alone is no Mac: the tool's "No connection detected" there
// is its right answer, not a failure. Anywhere else it still fails.
func TestAProbeOfADisplayIsNoMacAndNoFailure(t *testing.T) {
	r := newRig(t)
	r.ports.ports[1] = oob.Port{RID: 1, Connected: true, PlugCount: 2, NoMac: true}
	noConnection := "Mac type: J873sAP\nLooking for HPM devices...\nFound RID 1: IOService:/hpm1@A/AppleHPMARMSPMI\nConnection: None\nExiting DBMa mode... OK\nNo connection detected"
	probe := func(rid int) (oob.Port, string) {
		t.Helper()
		code, body := r.do("GET", fmt.Sprintf("/v1/ports/%d/status?probe=1", rid), testToken, "")
		if code != 200 {
			t.Fatalf("probe of RID %d: %d %s", rid, code, body)
		}
		var got oob.Port
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		return got, body
	}

	r.tool.fail = map[int]string{1: noConnection, 0: noConnection}
	got, body := probe(1)
	if !got.NoMac || got.ToolFailed || !strings.Contains(got.Tool, "No connection detected") || !strings.Contains(body, `"no_mac":true`) {
		t.Errorf("a display: %s", body)
	}
	if got, body := probe(0); got.NoMac || !got.ToolFailed {
		t.Errorf("a Mac on USB2 the tool finds no connection on passed: %s", body)
	}
	r.tool.fail = map[int]string{1: "IOKit says no"}
	if got, body := probe(1); !got.ToolFailed {
		t.Errorf("a tool failing otherwise on a display passed: %s", body)
	}
}

// The controller compares the port with what labd saw at the Sighting, once
// more, under the lock a reset holds.
func TestAResetIsRefusedWhenThePortIsNotAsItWasSeen(t *testing.T) {
	r := newRig(t)
	seen := `{"product_id":29448,"plug_count":40}`
	if code, body := r.do("POST", "/v1/ports/2/reset", testToken, seen); code != 200 {
		t.Fatalf("reset of a port as it was seen: %d %s", code, body)
	}
	for _, c := range []struct {
		why  string
		now  oob.Port
		want string
	}{
		{"unplugged", oob.Port{RID: 2, PlugCount: 41}, "not connected"},
		{"another model", oob.Port{RID: 2, Connected: true, ProductID: 29453, PlugCount: 40}, "product id"},
		{"plugged again", oob.Port{RID: 2, Connected: true, ProductID: 29448, PlugCount: 42}, "plug count"},
	} {
		r.ports.set(c.now)
		code, body := r.do("POST", "/v1/ports/2/reset", testToken, seen)
		if code != http.StatusConflict || !strings.Contains(body, c.want) {
			t.Errorf("%s: %d %s, want 409 naming the %s", c.why, code, body, c.want)
		}
	}
	if runs := r.tool.seen(); len(runs) != 1 {
		t.Fatalf("the tool ran %v, want the one reset", runs)
	}
	for _, body := range []string{`{"product_id":"x"}`, `{"plug_count":40}`, `{"product_id":29448}`, `[]`, `{"product_id":29448,"plug_count":40,"rid":0}`, `{"product_id":0,"plug_count":0}`} {
		if code, _ := r.do("POST", "/v1/ports/2/reset", testToken, body); code != 400 {
			t.Errorf("%s: %d, want 400", body, code)
		}
	}
	if runs := r.tool.seen(); len(runs) != 1 {
		t.Fatalf("a reset with a bad body reached the tool: %v", runs)
	}
}

func TestAResetTheToolFailsIsReportedWithItsLastLine(t *testing.T) {
	r := newRig(t)
	r.tool.fail = map[int]string{1: "Found RID 1\nConnection: None\nNo connection detected"}
	code, body := r.do("POST", "/v1/ports/1/reset", testToken, "")
	if code != http.StatusBadGateway || !strings.Contains(body, "No connection detected") {
		t.Fatalf("%d %s", code, body)
	}
}

func TestEveryRequestNeedsTheToken(t *testing.T) {
	r := newRig(t)
	for _, token := range []string{"", "wrong", testToken[:len(testToken)-1], testToken + "0"} {
		for _, c := range []struct{ method, path string }{{"POST", "/v1/ports/0/reset"}, {"GET", "/v1/ports/0/status"}, {"GET", "/v1/ports/9/status"}} {
			if code, _ := r.do(c.method, c.path, token, ""); code != 401 {
				t.Errorf("%s %s with token %q: %d, want 401", c.method, c.path, token, code)
			}
		}
	}
	if runs := r.tool.seen(); len(runs) != 0 || r.ports.reads != 0 {
		t.Fatalf("a request with no token got as far as the tool (%v) or the ports (%d reads)", runs, r.ports.reads)
	}
	if _, err := New(Config{Tool: r.tool, Ports: r.ports}, log.Default()); err == nil {
		t.Fatal("a controller with no token was made")
	}
}

func TestACallerFromAnotherMachineIsRefused(t *testing.T) {
	r := newRig(t)
	s, _ := New(Config{Token: testToken, Tool: r.tool, Ports: r.ports}, log.New(io.Discard, "", 0))
	for _, from := range []string{"192.168.0.9:50000", "100.64.0.1:50000", "[2001:db8::1]:50000", ""} {
		req := httptest.NewRequest("POST", "/v1/ports/0/reset", nil)
		req.RemoteAddr = from
		req.Header.Set("Authorization", "Bearer "+testToken)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("a caller at %q: %d, want 403", from, rec.Code)
		}
	}
	if runs := r.tool.seen(); len(runs) != 0 {
		t.Fatalf("the tool ran for a caller from another machine: %v", runs)
	}
}

func TestTheControllerListensOnLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:7780", "[::1]:7780"} {
		if err := Loopback(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":7780", "0.0.0.0:7780", "192.168.1.20:7780", "100.64.0.1:7780", "localhost:7780", "[::]:7780", "7780", ""} {
		if err := Loopback(addr); err == nil {
			t.Errorf("%q was taken for a loopback address", addr)
		}
	}
}

// The real tool is a program: ExecTool runs it with --rid, the port and one
// command, and nothing else.
func TestTheToolIsRunWithItsRIDAndNothingSecret(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	tool := filepath.Join(dir, "macvdmtool")
	script := "#!/bin/sh\nprintf '%s\\n' \"$#\" \"$@\" >> " + record + "\nenv >> " + record + ".env\necho \"Found RID $2\"\n[ \"$2\" = 1 ] && { echo 'No connection detected'; exit 255; }\nexit 0\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OOBD_TOKEN", testToken)
	x := ExecTool{Path: tool}
	if out, err := x.Run(context.Background(), 2, "reboot"); err != nil || !strings.Contains(out, "Found RID 2") {
		t.Fatalf("reboot: %v %q", err, out)
	}
	if _, err := x.Run(context.Background(), 0, "nop"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Run(context.Background(), 1, "reboot"); err == nil || !strings.Contains(err.Error(), "No connection detected") {
		t.Fatalf("a failed run: %v", err)
	}
	got, _ := os.ReadFile(record)
	if want := "3\n--rid\n2\nreboot\n3\n--rid\n0\nnop\n3\n--rid\n1\nreboot\n"; string(got) != want {
		t.Fatalf("the tool was run with\n%s\nwant\n%s", got, want)
	}
	if env, _ := os.ReadFile(record + ".env"); strings.Contains(string(env), testToken) || strings.Contains(string(got), testToken) {
		t.Fatal("the token reached the tool")
	}

	for _, c := range []struct {
		rid     int
		command string
	}{{0, "serial"}, {0, "dfu"}, {0, "debugusb"}, {0, "reboot serial"}, {0, ""}, {0, "--rid=1"}, {-1, "reboot"}} {
		if _, err := x.Run(context.Background(), c.rid, c.command); err == nil {
			t.Errorf("the tool was run with RID %d and %q", c.rid, c.command)
		}
	}
	if after, _ := os.ReadFile(record); string(after) != string(got) {
		t.Fatalf("a refused command reached the tool:\n%s", after)
	}
	if _, err := (ExecTool{Path: filepath.Join(dir, "none")}).Run(context.Background(), 0, "nop"); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("a missing tool: %v", err)
	}
}

// The whole path: a request, the controller, the tool as a program.
func TestAResetReachesTheProgramWithItsValidatedRID(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	tool := filepath.Join(dir, "macvdmtool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho \"$*\" >> "+record+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Token: testToken, Tool: ExecTool{Path: tool}, Ports: theMini()}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	r := &rig{t: t, srv: srv}
	for _, rid := range []string{"1", "7", "x", "2"} {
		r.do("POST", "/v1/ports/"+rid+"/reset", testToken, "")
	}
	got, _ := os.ReadFile(record)
	if string(got) != "--rid 1 reboot\n--rid 2 reboot\n" {
		t.Fatalf("the program was run with:\n%s", got)
	}
}

// labd's wait bounds its turn at the controller, not the tool run: a reset
// the controller began is answered as sent after its caller's deadline, and
// one still waiting at that deadline is never sent.
func TestAResetIsWaitedForOnceTheControllerBeganIt(t *testing.T) {
	r := newRig(t)
	r.holdTool()
	file := filepath.Join(t.TempDir(), "oobd.token")
	if err := os.WriteFile(file, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := oob.NewClient(r.srv.URL, file)
	if err != nil {
		t.Fatal(err)
	}
	sent := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		sent <- c.Reset(ctx, 2, nil)
	}()
	<-r.tool.started
	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		waited <- c.Reset(ctx, 0, nil)
	}()
	time.Sleep(600 * time.Millisecond)
	r.tool.hold <- struct{}{}
	if err := <-sent; err != nil {
		t.Fatalf("a reset the tool ran, its caller's deadline past: %v", err)
	}
	var unsent *oob.NotSent
	if err := <-waited; !errors.As(err, &unsent) || !strings.Contains(err.Error(), "did not come in time") {
		t.Fatalf("a reset whose turn came after its caller's deadline: %v", err)
	}
	select {
	case run := <-r.tool.started:
		t.Fatalf("%v was sent after its caller stopped waiting", run)
	case <-time.After(300 * time.Millisecond):
	}
}
