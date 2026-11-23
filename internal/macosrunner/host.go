package macosrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Host is what a run reads and changes of the Mac, so that the tests can
// play one.
type Host interface {
	Now() time.Time
	Sleep(context.Context, time.Duration)
	// SWVers is sw_vers -productVersion and -buildVersion.
	SWVers(context.Context) (version, build string, err error)
	// Boot is this boot's session id (kern.bootsessionuuid) and when it
	// began (kern.boottime).
	Boot(context.Context) (session string, at time.Time, err error)
	Power(context.Context) (Power, error)
	// CPUBusy is the share of CPU time in use over d, in percent.
	CPUBusy(ctx context.Context, d time.Duration) (float64, error)
	LowPowerMode(context.Context) (bool, error)
	SetLowPowerMode(ctx context.Context, on bool) error
	// Brightness is the built-in display's backlight, 0 to 1; ok is false
	// with no built-in display.
	Brightness(context.Context) (v float64, ok bool, err error)
	SetBrightness(context.Context, float64) error
	// Temperature is the hottest sensor in °C; ok is false with none.
	Temperature(context.Context) (c float64, ok bool)
	// Disk is the size of the volume holding path and what is free on it.
	Disk(path string) (total, free uint64, err error)
	// SSD is the size of the internal SSD, in bytes.
	SSD(context.Context) (uint64, error)
	CPUs() int
	// ConsoleUser is who is logged in at the screen, or "".
	ConsoleUser(context.Context) string
}

// Power is where the Mac draws its power from.
type Power struct {
	OnAC, Battery, Charging bool
}

// MacHost reads and changes macOS itself; GUIUser is whose session the
// backlight is changed in.
type MacHost struct{ GUIUser string }

func (MacHost) Now() time.Time                             { return time.Now() }
func (MacHost) Sleep(ctx context.Context, d time.Duration) { sleep(ctx, d) }
func (MacHost) CPUs() int                                  { return runtime.NumCPU() }

func (MacHost) ConsoleUser(ctx context.Context) string {
	u, err := MacSeat().Console(ctx)
	if err != nil || u == "loginwindow" || u == "root" {
		return ""
	}
	return u
}

func output(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

var reVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}$`)
var reBuild = regexp.MustCompile(`^[0-9]{2}[A-Z][0-9]{1,5}[a-z]?$`)

func (MacHost) SWVers(ctx context.Context) (string, string, error) {
	v, err := output(ctx, "/usr/bin/sw_vers", "-productVersion")
	if err != nil {
		return "", "", err
	}
	b, err := output(ctx, "/usr/bin/sw_vers", "-buildVersion")
	if err != nil {
		return "", "", err
	}
	return checkSWVers(v, b)
}

// checkSWVers keeps sw_vers's answers only when they are a version and a
// build, so that nothing else is ever reported.
func checkSWVers(v, b string) (string, string, error) {
	v, b = strings.TrimSpace(v), strings.TrimSpace(b)
	if !reVersion.MatchString(v) || !reBuild.MatchString(b) {
		return "", "", errors.New("sw_vers gave no product version and build")
	}
	return v, b, nil
}

func (MacHost) Boot(ctx context.Context) (string, time.Time, error) {
	s, err := output(ctx, "/usr/sbin/sysctl", "-n", "kern.bootsessionuuid")
	if err != nil {
		return "", time.Time{}, err
	}
	bt, err := output(ctx, "/usr/sbin/sysctl", "-n", "kern.boottime")
	if err != nil {
		return "", time.Time{}, err
	}
	at, err := parseBootTime(bt)
	return strings.TrimSpace(s), at, err
}

func (MacHost) Power(ctx context.Context) (Power, error) {
	out, err := output(ctx, "/usr/bin/pmset", "-g", "batt")
	if err != nil {
		return Power{}, err
	}
	return parseBatt(out), nil
}

// parseBatt reads pmset -g batt. A Mac with no battery draws from AC.
func parseBatt(s string) Power {
	p := Power{OnAC: strings.Contains(s, "'AC Power'"), Battery: strings.Contains(s, "InternalBattery")}
	if !p.Battery {
		p.OnAC = true
		return p
	}
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "InternalBattery") {
			l = strings.ToLower(l)
			p.Charging = strings.Contains(l, "; charging;") || strings.Contains(l, "; finishing charge;")
		}
	}
	return p
}

var reCPUUsage = regexp.MustCompile(`CPU usage: ([0-9.]+)% user, ([0-9.]+)% sys, ([0-9.]+)% idle`)

func (MacHost) CPUBusy(ctx context.Context, d time.Duration) (float64, error) {
	secs := max(int(d.Seconds()), 1)
	out, err := output(ctx, "/usr/bin/top", "-l", "2", "-n", "0", "-s", strconv.Itoa(secs))
	if err != nil {
		return 0, err
	}
	return parseTopBusy(out)
}

// parseTopBusy reads top -l 2's second sample: the first counts from boot.
func parseTopBusy(s string) (float64, error) {
	m := reCPUUsage.FindAllStringSubmatch(s, -1)
	if len(m) == 0 {
		return 0, errors.New("no CPU usage from top")
	}
	idle, _ := strconv.ParseFloat(m[len(m)-1][3], 64)
	return 100 - idle, nil
}

func (MacHost) LowPowerMode(ctx context.Context) (bool, error) {
	out, err := output(ctx, "/usr/bin/pmset", "-g", "custom")
	if err != nil {
		return false, err
	}
	return parseLowPowerMode(out), nil
}

var reLowPower = regexp.MustCompile(`(?m)^\s*lowpowermode\s+(\d+)`)

// parseLowPowerMode reads pmset -g custom's AC Power section, the one a run
// draws from; a Mac with no sections has only that one.
func parseLowPowerMode(s string) bool {
	if i := strings.Index(s, "AC Power:"); i >= 0 {
		s = s[i:]
		if j := strings.Index(s[1:], "Battery Power:"); j >= 0 {
			s = s[:j+1]
		}
	}
	m := reLowPower.FindStringSubmatch(s)
	return m != nil && m[1] != "0"
}

func (MacHost) SetLowPowerMode(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	_, err := output(ctx, "/usr/bin/pmset", "-c", "lowpowermode", v)
	return err
}

// The backlight is read and set through DisplayServices, in the logged-in
// user's session, with JavaScript for Automation: macOS has no command for
// it, and this asks for no permission.
const brightnessJXA = `ObjC.import('CoreGraphics');
$.NSBundle.bundleWithPath('/System/Library/PrivateFrameworks/DisplayServices.framework').load;
ObjC.bindFunction('DisplayServicesGetBrightness', ['int', ['unsigned int', 'float *']]);
ObjC.bindFunction('DisplayServicesSetBrightness', ['int', ['unsigned int', 'float']]);
function run(argv) {
  const d = $.CGMainDisplayID();
  if (!$.CGDisplayIsBuiltin(d)) return 'none';
  if (argv.length) { return $.DisplayServicesSetBrightness(d, parseFloat(argv[0])) === 0 ? 'set' : 'failed'; }
  const p = Ref(); return $.DisplayServicesGetBrightness(d, p) === 0 ? String(p[0]) : 'failed';
}`

func (h MacHost) jxa(ctx context.Context, args ...string) (string, error) {
	cmd := append([]string{"/usr/bin/osascript", "-l", "JavaScript", "-e", brightnessJXA}, args...)
	cmd = asUser(or(h.GUIUser, h.ConsoleUser(ctx)), cmd)
	out, err := output(ctx, cmd[0], cmd[1:]...)
	return strings.TrimSpace(out), err
}

func (h MacHost) Brightness(ctx context.Context) (float64, bool, error) {
	out, err := h.jxa(ctx)
	switch {
	case err != nil:
		return 0, false, err
	case out == "none":
		return 0, false, nil
	}
	v, err := strconv.ParseFloat(out, 64)
	if err != nil {
		return 0, false, errors.New("no backlight level from DisplayServices")
	}
	return v, true, nil
}

func (h MacHost) SetBrightness(ctx context.Context, v float64) error {
	out, err := h.jxa(ctx, strconv.FormatFloat(v, 'f', 3, 64))
	if err == nil && out != "set" {
		err = errors.New("DisplayServices did not set the backlight")
	}
	return err
}

// Temperature: macOS gives its sensors to no command without powermetrics,
// which is not used, so the cool-down is the fixed one.
func (MacHost) Temperature(context.Context) (float64, bool) { return 0, false }

func (MacHost) Disk(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, nil
}

var reDiskSize = regexp.MustCompile(`<key>(?:TotalSize|Size)</key>\s*<integer>(\d+)</integer>`)

// SSD is the whole internal disk's size from diskutil, the disk Apple
// Silicon boots from.
func (MacHost) SSD(ctx context.Context) (uint64, error) {
	out, err := output(ctx, "/usr/sbin/diskutil", "info", "-plist", "disk0")
	if err != nil {
		return 0, err
	}
	m := reDiskSize.FindStringSubmatch(out)
	if m == nil {
		return 0, errors.New("no disk size from diskutil")
	}
	return strconv.ParseUint(m[1], 10, 64)
}

// asUser runs cmd in user's desktop session when the runner is root (the
// LaunchDaemon): a browser or the backlight needs the window server.
func asUser(name string, cmd []string) []string {
	if name == "" || os.Geteuid() != 0 {
		return cmd
	}
	u, err := user.Lookup(name)
	if err != nil {
		return cmd
	}
	return append([]string{"/bin/launchctl", "asuser", u.Uid, "/usr/bin/sudo", "-u", name}, cmd...)
}

// chownTo gives path to user, when the runner is root.
func chownTo(name, path string) error {
	if name == "" || os.Geteuid() != 0 {
		return nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("the user the browser runs as: %w", err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return os.Chown(path, uid, gid)
}
