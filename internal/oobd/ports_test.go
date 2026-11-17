package oobd

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/oob"
)

func testdata(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPortsAreFoundByTheRIDTheToolMatches(t *testing.T) {
	ports := parsePorts(testdata(t, "ioreg-applehpm.txt"))
	want := map[int]hpmPort{
		0: {rid: 0, receptacle: 1, connected: true, macLink: true, productID: 29453, plugCount: 42},
		1: {rid: 1, receptacle: 2, plugCount: 27},
		2: {rid: 2, receptacle: 3, connected: true, macLink: true, productID: 29448, plugCount: 40},
		5: {rid: 5, receptacle: 1, plugCount: 1},
	}
	if len(ports) != len(want) {
		t.Fatalf("%d ports read, want %d: %+v", len(ports), len(want), ports)
	}
	for rid, w := range want {
		if ports[rid] != w {
			t.Errorf("RID %d: %+v, want %+v", rid, ports[rid], w)
		}
	}
}

func TestANameIsReadOnTheReceptacleItsBusBelongsTo(t *testing.T) {
	names, err := peerNames([]byte(testdata(t, "thunderbolt.json")))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[3] != "omarchy-m1-pro" {
		t.Fatalf("names by receptacle: %v, want omarchy-m1-pro on 3 alone", names)
	}
	// Two devices behind one receptacle are no single Mac.
	two := strings.Replace(testdata(t, "thunderbolt.json"), `"_items" : [`, `"_items" : [ {"_name" : "a dock", "device_name_key" : "a dock"},`, 1)
	if names, err := peerNames([]byte(two)); err != nil || len(names) != 0 {
		t.Fatalf("two devices on one receptacle gave %v, %v", names, err)
	}
	if _, err := peerNames([]byte("not json")); err == nil {
		t.Fatal("no error for what is not JSON")
	}
}

// stubs puts an ioreg and a system_profiler that print the captured text in a folder.
func stubs(t *testing.T) SystemPorts {
	t.Helper()
	return stubsOf(t, "ioreg-applehpm.txt")
}

func stubsOf(t *testing.T, ioreg string) SystemPorts {
	t.Helper()
	dir := t.TempDir()
	here, _ := os.Getwd()
	for name, file := range map[string]string{"ioreg": ioreg, "system_profiler": "thunderbolt.json"} {
		script := "#!/bin/sh\necho \"$*\" >> " + dir + "/" + name + ".args\ncat " + filepath.Join(here, "testdata", file) + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return SystemPorts{Ioreg: filepath.Join(dir, "ioreg"), Profiler: filepath.Join(dir, "system_profiler")}
}

func TestTheMinisPortsAsTheControllerReportsThem(t *testing.T) {
	sp := stubs(t)
	want := map[int]oob.Port{
		0: {RID: 0, Connected: true, ProductID: 29453, PlugCount: 42},
		1: {RID: 1, PlugCount: 27},
		2: {RID: 2, Connected: true, Name: "omarchy-m1-pro", ProductID: 29448, PlugCount: 40},
	}
	for rid, w := range want {
		got, err := sp.Read(context.Background(), rid)
		if err != nil {
			t.Fatalf("RID %d: %v", rid, err)
		}
		if got != w {
			t.Errorf("RID %d: %+v, want %+v", rid, got, w)
		}
	}
	if _, err := sp.Read(context.Background(), 7); err == nil || !strings.Contains(err.Error(), "no port with RID 7") {
		t.Fatalf("a RID this machine lacks: %v", err)
	}
	// An empty port has no name to look for.
	if n := strings.Count(testdataArgs(t, sp.Profiler), "\n"); n != 2 {
		t.Errorf("system_profiler ran %d times, want once for each connected port", n)
	}
}

// The mini on 2026-11-16: a display on RID 1, connected on CC alone with no
// USB-PD product id, which the reset tool cannot reach.
func TestADisplayOnCCAloneIsNoMac(t *testing.T) {
	sp := stubsOf(t, "ioreg-applehpm-display.txt")
	want := map[int]oob.Port{
		0: {RID: 0, Connected: true, ProductID: 29453, PlugCount: 42},
		1: {RID: 1, Connected: true, PlugCount: 2, NoMac: true},
		2: {RID: 2, Connected: true, Name: "omarchy-m1-pro", ProductID: 29448, PlugCount: 40},
	}
	for rid, w := range want {
		got, err := sp.Read(context.Background(), rid)
		if err != nil {
			t.Fatalf("RID %d: %v", rid, err)
		}
		if got != w {
			t.Errorf("RID %d: %+v, want %+v", rid, got, w)
		}
		if got, _ := sp.Look(context.Background(), rid); got.NoMac != w.NoMac {
			t.Errorf("RID %d: a look says no Mac %v, want %v", rid, got.NoMac, w.NoMac)
		}
	}
}

func testdataArgs(t *testing.T, tool string) string {
	t.Helper()
	b, _ := os.ReadFile(tool + ".args")
	return string(b)
}

func TestAPortReadThatFailsIsAnError(t *testing.T) {
	sp := SystemPorts{Ioreg: "/nonexistent/ioreg", Profiler: "/nonexistent/system_profiler"}
	if _, err := sp.Read(context.Background(), 0); err == nil {
		t.Fatal("no error with no ioreg")
	}
	// The ports are still read when only the name cannot be.
	ok := stubs(t)
	ok.Profiler = "/nonexistent/system_profiler"
	got, err := ok.Read(context.Background(), 2)
	if err != nil || !got.Connected || got.Name != "" || got.ProductID != 29448 {
		t.Fatalf("with no system_profiler: %+v, %v", got, err)
	}
}

// On a Mac, the real ioreg and system_profiler are read the same way.
func TestThisMachinesOwnPortsAreRead(t *testing.T) {
	sp := DefaultPorts()
	out, err := exec.Command(sp.Ioreg, "-l", "-w0", "-r", "-c", "AppleHPM").Output()
	if err != nil {
		t.Skipf("no ioreg here: %v", err)
	}
	ports := parsePorts(string(out))
	if len(ports) == 0 {
		t.Skip("this machine shows no USB-C port controller")
	}
	for rid, p := range ports {
		if p.rid != rid || p.plugCount < 0 {
			t.Errorf("RID %d read as %+v", rid, p)
		}
		got, err := sp.Read(context.Background(), rid)
		if err != nil || got.RID != rid || got.Connected != p.connected {
			t.Errorf("RID %d: %+v, %v", rid, got, err)
		}
	}
}

// The reset's last look is one read of the port and the tool right after it:
// the name is not read in between, which may take half a minute with a hung
// Mac on the bus, long enough for a cable to move.
func TestAResetsLastLookReadsNoName(t *testing.T) {
	sp := stubs(t)
	tool := &fakeTool{}
	s, err := New(Config{Token: testToken, Tool: tool, Ports: sp}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	r := &rig{t: t, srv: srv}
	if code, body := r.do("POST", "/v1/ports/2/reset", testToken, `{"product_id":29448,"plug_count":40}`); code != 200 {
		t.Fatalf("reset of a port as it was seen: %d %s", code, body)
	}
	if code, body := r.do("POST", "/v1/ports/2/reset", testToken, `{"product_id":29448,"plug_count":39}`); code != http.StatusConflict {
		t.Fatalf("reset of a port plugged again: %d %s", code, body)
	}
	if runs := tool.seen(); len(runs) != 1 || runs[0] != (toolRun{2, "reboot"}) {
		t.Fatalf("the tool ran %v, want the one reset", runs)
	}
	if n := strings.Count(testdataArgs(t, sp.Ioreg), "\n"); n != 2 {
		t.Errorf("ioreg ran %d times, want once for each reset", n)
	}
	if args := testdataArgs(t, sp.Profiler); args != "" {
		t.Errorf("the name was read before a reset:\n%s", args)
	}
	// The status still names the Mac.
	code, body := r.do("GET", "/v1/ports/2/status", testToken, "")
	if code != 200 || !strings.Contains(body, `"omarchy-m1-pro"`) {
		t.Errorf("status: %d %s", code, body)
	}
}
