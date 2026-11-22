package macosrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// fakeSeat is a boot as the runner sees it: started some time after the
// boot, then one console holder per second of the script.
func fakeSeat(sinceBoot time.Duration, fileVault bool, autoLogin string, console ...string) Seat {
	boot := time.Date(2026, 11, 22, 9, 0, 0, 0, time.UTC)
	now := boot.Add(sinceBoot)
	i := 0
	return Seat{
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) { now = now.Add(d) },
		Boot:  func(context.Context) (time.Time, error) { return boot, nil },
		Console: func(context.Context) (string, error) {
			c := console[min(i, len(console)-1)]
			i++
			if c == "error" {
				return "", errors.New("scutil failed")
			}
			return c, nil
		},
		FileVault: func(context.Context) (bool, error) { return fileVault, nil },
		AutoLogin: func(context.Context) string { return autoLogin },
	}
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestTheUnattendedCheck(t *testing.T) {
	early := 20 * time.Second
	for _, c := range []struct {
		name  string
		seat  Seat
		user  string
		known bool
		alone bool
	}{
		{"logged in automatically", fakeSeat(early, false, "ada", "", "", "ada"), "ada", true, true},
		{"the user named by automatic login", fakeSeat(early, false, "ada", "", "ada"), "", true, true},
		{"the login window for a moment", fakeSeat(early, false, "ada", "", "loginwindow", "loginwindow", "ada"), "ada", true, true},
		{"a scutil that failed once", fakeSeat(early, false, "ada", "", "error", "ada"), "ada", true, true},
		{"the login window until somebody typed", fakeSeat(early, false, "ada", append(append([]string{""}, repeat("loginwindow", 12)...), "ada")...), "ada", true, false},
		{"the login window shown, then gone, then the user", fakeSeat(early, false, "ada", append(append([]string{""}, repeat("loginwindow", 8)...), "", "ada")...), "ada", true, false},
		{"the login window and nobody", fakeSeat(early, false, "ada", append([]string{""}, repeat("loginwindow", 700)...)...), "ada", true, false},
		{"FileVault on", fakeSeat(early, true, "ada", "", "ada"), "ada", true, false},
		{"no automatic login", fakeSeat(early, false, "", "", "ada"), "ada", true, false},
		{"automatic login of another user", fakeSeat(early, false, "grace", "", "grace"), "ada", true, false},
		{"another user at the console", fakeSeat(early, false, "ada", "", "grace"), "ada", true, false},
		{"started late in the boot", fakeSeat(10*time.Minute, false, "ada", "", "ada"), "ada", false, false},
		{"logged in before the first look", fakeSeat(early, false, "ada", "ada"), "ada", false, false},
		{"nobody within the wait", fakeSeat(early, false, "ada", ""), "ada", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := c.seat.Check(context.Background(), c.user, AttendanceWait)
			if a.Known != c.known || a.Unattended != c.alone || a.Why == "" {
				t.Errorf("got %+v, want known %v unattended %v", a, c.known, c.alone)
			}
		})
	}
}

func TestTheCheckReadsMacOSOwnWords(t *testing.T) {
	at, err := parseBootTime("{ sec = 1732240000, usec = 250000 } Fri Nov 22 09:46:40 2024\n")
	if err != nil || !at.Equal(time.Unix(1732240000, 250000000)) {
		t.Errorf("boot time: %v %v", at, err)
	}
	if _, err := parseBootTime("nothing"); err == nil {
		t.Error("took a boot time from nothing")
	}
	held := "<dictionary> {\n  GID : 20\n  Name : ada\n  UID : 501\n}\n"
	for in, want := range map[string]string{held: "ada", "  No such key\n": "", "<dictionary> {\n  Name : loginwindow\n}\n": "loginwindow"} {
		if got := parseConsoleUser(in); got != want {
			t.Errorf("console of %q: %q, want %q", in, got, want)
		}
	}
}

// The runner tells labd what a boot showed, and nothing when it showed
// nothing.
func TestTheRunnerSaysWhatABootShowed(t *testing.T) {
	var said []api.MacOSAttendance
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/macos/runner/attendance" || r.Header.Get("Authorization") != "Bearer mos_secret" {
			http.Error(w, "no", 401)
			return
		}
		var a api.MacOSAttendance
		json.NewDecoder(r.Body).Decode(&a)
		said = append(said, a)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	var logged []string
	r := &Runner{Config: Config{Server: srv.URL, Secret: "mos_secret", GUIUser: "ada"}, Log: func(f string, a ...any) {
		logged = append(logged, f)
	}}
	ctx := context.Background()
	if _, err := r.Attendance(ctx, fakeSeat(time.Second, false, "ada", "", "ada"), AttendanceWait); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Attendance(ctx, fakeSeat(time.Second, true, "ada", "", "ada"), AttendanceWait); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Attendance(ctx, fakeSeat(time.Hour, false, "ada", "", "ada"), AttendanceWait); err != nil {
		t.Fatal(err)
	}
	if len(said) != 2 || !said[0].Unattended || said[1].Unattended {
		t.Errorf("said %+v", said)
	}
	if len(logged) != 3 || !strings.Contains(logged[2], "nothing") {
		t.Errorf("logged %q", logged)
	}
}
