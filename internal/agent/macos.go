package agent

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// MacOSBoot is how boot-macos-once reaches the Mac; the zero value is the
// real one.
type MacOSBoot struct {
	Bless  string       // asahi-bless; "" finds it on PATH
	Volume string       // the macOS volume's name, when the Mac has more than one
	Reboot func() error // nil is systemctl reboot
	Log    func(string, ...any)
}

type blessVolume struct {
	Index, Name string
	Default     bool
}

var reVolume = regexp.MustCompile(`^\s*(\*?)\s*(\d+)\)\s+(.+?)\s*$`)

// parseVolumes reads asahi-bless --list-volumes: "1) Macintosh HD",
// "*2) Omarchy", the star on the persistent default.
func parseVolumes(out string) []blessVolume {
	var vs []blessVolume
	for _, line := range strings.Split(out, "\n") {
		if m := reVolume.FindStringSubmatch(line); m != nil {
			vs = append(vs, blessVolume{Index: m[2], Name: m[3], Default: m[1] == "*"})
		}
	}
	return vs
}

func linuxVolume(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "omarchy") || strings.Contains(n, "linux") || strings.Contains(n, "asahi")
}

// macOSVolume picks the volume to boot once: not the persistent default,
// which must be this Linux so that the next boot after comes back, and not
// another Linux. One only, or the one named.
func macOSVolume(vs []blessVolume, named string) (blessVolume, error) {
	var def *blessVolume
	var cands []blessVolume
	for i, v := range vs {
		if v.Default {
			def = &vs[i]
			continue
		}
		if named != "" && v.Name == named || named == "" && !linuxVolume(v.Name) {
			cands = append(cands, v)
		}
	}
	switch {
	case def == nil || !linuxVolume(def.Name):
		return blessVolume{}, fmt.Errorf("the persistent default boot is not this Linux (%+v): booting macOS once might not come back", vs)
	case len(cands) == 0:
		return blessVolume{}, fmt.Errorf("no macOS volume in asahi-bless --list-volumes (%+v)", vs)
	case len(cands) > 1:
		return blessVolume{}, fmt.Errorf("more than one macOS volume (%+v): name one with --volume", cands)
	}
	return cands[0], nil
}

// BootMacOSOnce boots the Mac into macOS for one boot, for the macOS run
// labd has queued: it finds the macOS volume, sets it as the next boot only
// (asahi-bless --next), and only then tells labd the run started and
// reboots. When labd refuses (no run is queued), the next boot is put back
// on this Linux. The boot after returns to the persistent default, this
// Linux.
func BootMacOSOnce(ctx context.Context, cfg *Config, o MacOSBoot) error {
	say := o.Log
	if say == nil {
		say = func(string, ...any) {}
	}
	started := func() error {
		var run api.MacOSRun
		if err := postJSON(ctx, cfg.client(30*time.Second), strings.TrimRight(cfg.Server, "/")+"/api/agent/macos-started", cfg.Secret, struct{}{}, &run); err != nil {
			return fmt.Errorf("labd: %w", err)
		}
		say("macOS run %s started: booting macOS once", run.ID)
		return nil
	}
	if err := blessMacOSNext(ctx, o, started); err != nil {
		return err
	}
	reboot := o.Reboot
	if reboot == nil {
		reboot = func() error { return exec.Command("systemctl", "reboot").Run() }
	}
	return reboot()
}

// blessMacOSNext finds the macOS volume, sets it as the next boot only and,
// only when that succeeded, has started tell labd. A start that fails
// tells labd nothing: the run labd queued is cancelled by whoever asked
// for the boot. A start labd refuses puts the next boot back on this Linux.
func blessMacOSNext(ctx context.Context, o MacOSBoot, started func() error) error {
	bless := o.Bless
	if bless == "" {
		bless = "asahi-bless"
	}
	out, err := exec.CommandContext(ctx, bless, "--list-volumes").Output()
	if err != nil {
		return fmt.Errorf("asahi-bless --list-volumes: %w", err)
	}
	vs := parseVolumes(string(out))
	vol, err := macOSVolume(vs, o.Volume)
	if err != nil {
		return err
	}
	next := func(v blessVolume) error {
		if b, err := exec.CommandContext(ctx, bless, "--next", "--set-boot", v.Index, "--yes").CombinedOutput(); err != nil {
			return fmt.Errorf("asahi-bless --next --set-boot %s: %v: %s", v.Index, err, strings.TrimSpace(string(b)))
		}
		return nil
	}
	if err := next(vol); err != nil {
		return err
	}
	if err := started(); err != nil {
		for _, v := range vs {
			if v.Default {
				if uerr := next(v); uerr != nil {
					return fmt.Errorf("%w; and the next boot is still macOS: %v", err, uerr)
				}
			}
		}
		return err
	}
	return nil
}
