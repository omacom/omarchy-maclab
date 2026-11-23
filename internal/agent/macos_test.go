package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBless is an asahi-bless that lists volumes and records its calls.
func fakeBless(t *testing.T, list string) (path, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	path = filepath.Join(dir, "asahi-bless")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\nif [ \"$1\" = --list-volumes ]; then cat <<'L'\n" + list + "L\nfi\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, calls
}

func fakeStartLab(t *testing.T, code int) (*Config, *int) {
	t.Helper()
	n := new(int)
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/macos-started" || r.Header.Get("Authorization") != "Bearer dev_secret" {
			t.Errorf("asked %s %s", r.Method, r.URL.Path)
		}
		*n++
		w.WriteHeader(code)
		w.Write([]byte(`{"id":"mos-1","state":"started"}`))
	}))
	t.Cleanup(hs.Close)
	return &Config{Server: hs.URL, Secret: "dev_secret", Name: "m2-max"}, n
}

func TestBootMacOSOnceBlessesTheMacOSVolumeForTheNextBootOnly(t *testing.T) {
	bless, calls := fakeBless(t, "1) Macintosh HD\n*2) Omarchy\n")
	cfg, asked := fakeStartLab(t, 200)
	rebooted := 0
	err := BootMacOSOnce(context.Background(), cfg, MacOSBoot{Bless: bless, Reboot: func() error { rebooted++; return nil }, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(b)); got != "--list-volumes\n--next --set-boot 1 --yes" {
		t.Errorf("asahi-bless calls:\n%s", got)
	}
	if *asked != 1 || rebooted != 1 {
		t.Errorf("labd told %d times, rebooted %d times", *asked, rebooted)
	}
}

// labd refusing the start (no run queued) puts the next boot back on this
// Linux, and nothing reboots.
func TestBootMacOSOnceChangesNothingWithoutAQueuedRun(t *testing.T) {
	bless, calls := fakeBless(t, "1) Macintosh HD\n*2) Omarchy\n")
	cfg, _ := fakeStartLab(t, 409)
	rebooted := 0
	err := BootMacOSOnce(context.Background(), cfg, MacOSBoot{Bless: bless, Reboot: func() error { rebooted++; return nil }, Log: t.Logf})
	if err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("err %v", err)
	}
	b, _ := os.ReadFile(calls)
	if got := strings.TrimSpace(string(b)); got != "--list-volumes\n--next --set-boot 1 --yes\n--next --set-boot 2 --yes" || rebooted != 0 {
		t.Errorf("asahi-bless calls:\n%s\nrebooted %d", got, rebooted)
	}
}

// labd is told the run started only once asahi-bless has set macOS as the
// next boot: a start that fails tells labd nothing and reboots nothing.
func TestBootMacOSOnceTellsLabdOnlyAfterAsahiBlessSucceeds(t *testing.T) {
	dir := t.TempDir()
	bless := filepath.Join(dir, "asahi-bless")
	script := "#!/bin/sh\nif [ \"$1\" = --list-volumes ]; then printf '1) Macintosh HD\\n*2) Omarchy\\n'; exit 0; fi\necho 'NVRAM write failed' >&2\nexit 1\n"
	if err := os.WriteFile(bless, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, asked := fakeStartLab(t, 200)
	err := BootMacOSOnce(context.Background(), cfg, MacOSBoot{Bless: bless, Reboot: func() error { t.Error("rebooted"); return nil }, Log: t.Logf})
	if err == nil || !strings.Contains(err.Error(), "NVRAM write failed") {
		t.Fatalf("err %v", err)
	}
	if *asked != 0 {
		t.Errorf("labd told %d times of a start that failed", *asked)
	}
}

func TestBootMacOSOnceRefusesWhenItCannotTellMacOS(t *testing.T) {
	for _, list := range []string{
		"*1) Omarchy\n", // no macOS
		"1) Macintosh HD\n2) macOS Test\n*3) Omarchy\n", // two
		"*1) Macintosh HD\n2) Omarchy\n",                // the default is not Linux
	} {
		bless, calls := fakeBless(t, list)
		cfg, asked := fakeStartLab(t, 200)
		err := BootMacOSOnce(context.Background(), cfg, MacOSBoot{Bless: bless, Reboot: func() error { t.Error("rebooted"); return nil }, Log: t.Logf})
		if err == nil {
			t.Errorf("%q: booted", list)
		}
		b, _ := os.ReadFile(calls)
		if strings.Contains(string(b), "--set-boot") || *asked != 0 {
			t.Errorf("%q: changed something: %q, labd told %d", list, b, *asked)
		}
	}
	// Named, one of two is taken.
	bless, calls := fakeBless(t, "1) Macintosh HD\n2) macOS Test\n*3) Omarchy\n")
	cfg, _ := fakeStartLab(t, 200)
	if err := BootMacOSOnce(context.Background(), cfg, MacOSBoot{Bless: bless, Volume: "macOS Test", Reboot: func() error { return nil }, Log: t.Logf}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(calls); !strings.Contains(string(b), "--next --set-boot 2 --yes") {
		t.Errorf("calls %q", b)
	}
}

func TestMacOSVolumesAreReadFromTheList(t *testing.T) {
	v := parseVolumes("1) Macintosh HD\n*2) Omarchy\n\nsomething else\n")
	if len(v) != 2 || v[0] != (blessVolume{Index: "1", Name: "Macintosh HD"}) || v[1] != (blessVolume{Index: "2", Name: "Omarchy", Default: true}) {
		t.Errorf("%+v", v)
	}
}
