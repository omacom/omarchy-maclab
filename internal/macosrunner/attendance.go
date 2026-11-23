package macosrunner

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// The Unattended check. The LaunchDaemon starts the runner early in every
// boot, before anybody can log in, and the runner watches who holds the
// console (State:/Users/ConsoleUser) until its user's desktop is reached. A
// boot reached that user's desktop with nobody typing anything when all of
// these hold:
//
//   - FileVault is off (fdesetup isactive). With it on, a password was typed
//     before macOS started, and macOS then opens that user's session itself,
//     with no login window: the one case the console cannot tell apart.
//   - macOS is set to log the user in automatically (autoLoginUser).
//   - The runner started within bootWindow of the boot (kern.boottime) and
//     its first look found nobody at the console yet, so it saw the boot
//     from before any login.
//   - The console then went to that user without the login window
//     ("loginwindow") holding it for more than loginWindowGrace. macOS may
//     show the login window for a moment as it logs in automatically;
//     nobody types a password into it that fast.
//
// Attended is a boot that held the login window longer, a Mac with FileVault
// on or with no automatic login for the user, or a console that went to
// someone else. Anything else (a runner started late in the boot, a user
// logged in before the first look, nobody at the console within the wait)
// tells the lab nothing.
const (
	bootWindow       = 3 * time.Minute
	loginWindowGrace = 5 * time.Second
	AttendanceWait   = 10 * time.Minute
	consoleLook      = time.Second
)

// Seat is what the check reads of the Mac.
type Seat struct {
	Now   func() time.Time
	Sleep func(context.Context, time.Duration)
	// Boot is when this boot began.
	Boot func(context.Context) (time.Time, error)
	// Console is who holds the console: "" for nobody, "loginwindow" while
	// the login window is shown, else the user logged in.
	Console   func(context.Context) (string, error)
	FileVault func(context.Context) (bool, error)
	// AutoLogin is the user macOS logs in automatically, or "".
	AutoLogin func(context.Context) string
}

// MacSeat reads macOS itself.
func MacSeat() Seat {
	return Seat{
		Now:   time.Now,
		Sleep: sleep,
		Boot: func(ctx context.Context) (time.Time, error) {
			out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "kern.boottime").Output()
			if err != nil {
				return time.Time{}, err
			}
			return parseBootTime(string(out))
		},
		Console: func(ctx context.Context) (string, error) {
			c := exec.CommandContext(ctx, "/usr/sbin/scutil")
			c.Stdin = strings.NewReader("show State:/Users/ConsoleUser\n")
			out, err := c.Output()
			if err != nil {
				return "", err
			}
			return parseConsoleUser(string(out)), nil
		},
		FileVault: func(ctx context.Context) (bool, error) { return fileVaultOn(ctx, fdesetupPath) },
		AutoLogin: func(ctx context.Context) string { return autoLoginUser(ctx, defaultsPath) },
	}
}

var reBootTime = regexp.MustCompile(`sec = (\d+), usec = (\d+)`)

// parseBootTime reads sysctl -n kern.boottime: { sec = 1732240000, usec = 5 } Fri Nov 22 …
func parseBootTime(s string) (time.Time, error) {
	m := reBootTime.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("kern.boottime: %q", strings.TrimSpace(s))
	}
	sec, _ := strconv.ParseInt(m[1], 10, 64)
	usec, _ := strconv.ParseInt(m[2], 10, 64)
	return time.Unix(sec, usec*1000), nil
}

var reConsoleName = regexp.MustCompile(`(?m)^\s*Name : (\S+)\s*$`)

// parseConsoleUser reads scutil's State:/Users/ConsoleUser: its Name, or ""
// when nobody holds the console (no such key).
func parseConsoleUser(s string) string {
	if m := reConsoleName.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// Attendance is what one boot showed.
type Attendance struct {
	// Known is whether the boot says anything; Unattended is what.
	Known, Unattended bool
	Why               string
}

func attended(why string) Attendance   { return Attendance{Known: true, Why: why} }
func unknown(why string) Attendance    { return Attendance{Why: why} }
func unattended(why string) Attendance { return Attendance{Known: true, Unattended: true, Why: why} }

// Check watches this boot for user, "" being the user macOS logs in
// automatically, for up to wait.
func (m Seat) Check(ctx context.Context, user string, wait time.Duration) Attendance {
	start := m.Now()
	boot, err := m.Boot(ctx)
	if err != nil {
		return unknown(fmt.Sprintf("when this boot began: %v", err))
	}
	if start.Sub(boot) > bootWindow {
		return unknown("the runner started too late in this boot to see it from the start")
	}
	on, err := m.FileVault(ctx)
	if err != nil {
		return unknown(fmt.Sprintf("FileVault: %v", err))
	}
	if on {
		return attended("FileVault is on: a password is typed before macOS starts")
	}
	auto := m.AutoLogin(ctx)
	if user == "" {
		user = auto
	}
	if auto == "" || auto != user {
		return attended(fmt.Sprintf("macOS does not log %s in automatically", or(user, "anybody")))
	}
	var shownSince time.Time
	shown := false
	for first := true; ; first = false {
		now := m.Now()
		c, err := m.Console(ctx)
		if err != nil {
			c = ""
		}
		switch c {
		case "", "root":
			shownSince = time.Time{}
		case "loginwindow":
			if shownSince.IsZero() {
				shownSince = now
			}
			if now.Sub(shownSince) > loginWindowGrace {
				shown = true
			}
		case user:
			switch {
			case first:
				return unknown(user + " was logged in before the runner first looked")
			case shown:
				return attended("the login window was shown before " + user + " logged in")
			}
			return unattended("this boot reached " + user + "'s desktop with nobody typing")
		default:
			return attended("the console went to another user")
		}
		if now.Sub(start) >= wait {
			if shown {
				return attended("the login window waited, and nobody logged in")
			}
			return unknown("nobody logged in within " + wait.String())
		}
		m.Sleep(ctx, consoleLook)
		if ctx.Err() != nil {
			return unknown("stopped")
		}
	}
}

// Attendance checks this boot and tells labd what it showed, when it shows
// anything: Unattended, or Attended again.
func (r *Runner) Attendance(ctx context.Context, seat Seat, wait time.Duration) (Attendance, error) {
	a := seat.Check(ctx, r.Config.GUIUser, wait)
	if !a.Known {
		r.say("this boot tells the lab nothing of whether the Mac is Attended: %s", a.Why)
		return a, nil
	}
	word := api.MacOSAttended
	if a.Unattended {
		word = api.MacOSUnattended
	}
	r.say("an %s Mac: %s", word, a.Why)
	return a, call(ctx, r.client(), http.MethodPost, r.url("/api/macos/runner/attendance"), r.Config.Secret, api.MacOSAttendance{Unattended: a.Unattended}, nil)
}
