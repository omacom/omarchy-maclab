package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// A Candidate boot runs a Package set from a snapshot of @ (see api.CmdCandidateStage).
// @, its boot entry and the known-good kernel are never changed; only a set
// that changes the boot loader changes the ESP's boot.bin, only with a
// Sighting, and candidate_cleanup puts the known-good one back.

const (
	candidateMarker = "etc/maclab-candidate"
	candidateState  = "candidate.json"
	bootBinPrev     = "boot-prev.bin"
	bootBinStaged   = "boot-candidate.bin"
)

var (
	reSHA256       = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reCandidatePkg = regexp.MustCompile(`^[A-Za-z0-9@._+-]+$`)
)

// candidateRestoreSteps follow the Recovery section of the trio sheet.
const candidateRestoreSteps = `The Mac does not boot after a boot loader change. At the Mac:
1. If it's looping, hold the power button for about ten seconds to force it off.
2. Hold the power button until the startup options appear, choose Options, and open Terminal from the Utilities menu. Recovery may ask for a macOS administrator password first.
3. List the disks: diskutil list
4. Find the small partition (about 500 MB) whose name starts with "EFI - ", next to the Linux partitions, and note its identifier (for example disk0s4).
5. Mount it and check for the saved boot file: diskutil mount disk0sN; ls "/Volumes/EFI - "*/m1n1/boot-prev.bin (it must print one file).
6. Put the old boot file back: cd "/Volumes/EFI - XXXXX/m1n1"; cp boot-prev.bin boot.bin; ls -l boot.bin boot-prev.bin (the two must be the same size).
7. Reboot. The old boot loader starts the Mac's own system.`

type candidateRecord struct {
	Job        string `json:"job"`
	Set        string `json:"set"`
	BootLoader bool   `json:"boot_loader,omitempty"`
}

// candidateHost is what a Candidate boot does to the Mac; tests fake Run.
type candidateHost struct {
	Run        func(ctx context.Context, env []string, name string, args ...string) ([]byte, error)
	Top        string // where the btrfs top level is mounted
	RootSource string // findmnt's SOURCE for /, e.g. /dev/mapper/root[/@]
	Lim        *limineLayout
	KnownGood  *KnownGood
	GUIUser    string
	M1N1Dir    string // <esp>/m1n1
	PacmanDB   string // the known-good system's pacman database: /var/lib/pacman/local
	WorkDir    string
	DRMDir     string // /sys/class/drm
	Cmdline    string // this boot's
	Fetch      Fetcher
	UKI        ukiWriter
}

func (h *candidateHost) cand() string { return filepath.Join(h.Top, api.CandidateSubvolume) }

func (h *candidateHost) run(ctx context.Context, env []string, name string, args ...string) error {
	if out, err := h.Run(ctx, env, name, args...); err != nil {
		return fmt.Errorf("%s: %v: %s", name, err, lastLines(string(out), 15))
	}
	return nil
}

func (h *candidateHost) mountTop(ctx context.Context) (func(), error) {
	dev, _, _ := strings.Cut(h.RootSource, "[")
	if err := os.MkdirAll(h.Top, 0o700); err != nil {
		return nil, err
	}
	if err := h.run(ctx, nil, "mount", "-o", "subvolid=5", dev, h.Top); err != nil {
		return nil, err
	}
	return func() { h.run(context.Background(), nil, "umount", h.Top) }, nil
}

func (h *candidateHost) stage(ctx context.Context, job string, a api.CandidateStageArgs) (res api.CandidateStageResult, err error) {
	if job == "" || strings.ContainsAny(job, "/. ") {
		return res, fmt.Errorf("bad job id %q", job)
	}
	if len(a.Set.Packages) == 0 {
		return res, errors.New("the set has no packages")
	}
	for _, p := range a.Set.Packages {
		if !reCandidatePkg.MatchString(p.Name) || !reSHA256.MatchString(p.SHA256) {
			return res, fmt.Errorf("bad package %q (sha256 %q)", p.Name, p.SHA256)
		}
		if api.IsBootLoaderPackage(p.Name) && !h.installed(p.Name, p.Version) {
			res.BootLoader = true
		}
	}
	if res.BootLoader && !a.Sighting {
		return res, errors.New("the set changes the boot loader (m1n1 or U-Boot), and this Mac has no Sighting: refused")
	}
	files, err := h.fetchSet(ctx, job, a.Set)
	if err != nil {
		return res, err
	}
	defer os.RemoveAll(h.filesDir(job))

	umount, err := h.mountTop(ctx)
	if err != nil {
		return res, err
	}
	defer umount()
	cand := h.cand()
	if _, err := os.Stat(cand); err == nil {
		if err := h.run(ctx, nil, "btrfs", "subvolume", "delete", cand); err != nil {
			return res, err
		}
	}
	if err := h.run(ctx, nil, "btrfs", "subvolume", "snapshot", filepath.Join(h.Top, "@"), cand); err != nil {
		return res, err
	}
	bootDir := filepath.Join(h.Lim.Stage, job)
	defer func() {
		if err != nil {
			os.RemoveAll(bootDir)
			h.Lim.writeEntries()
			h.run(context.Background(), nil, "btrfs", "subvolume", "delete", cand)
		}
	}()
	pacman := []string{"--sysroot", cand, "-U", "--noconfirm"}
	if a.Offline {
		if err = writeOfflineConf(cand); err != nil {
			return res, err
		}
		pacman = append(pacman, "--config", "/"+offlineConf)
	}
	err = h.run(ctx, nil, "pacman", append(pacman, files...)...)
	if a.Offline {
		os.Remove(filepath.Join(cand, offlineConf))
	}
	if err != nil {
		return res, err
	}
	if err = prepareCandidate(cand, a.Set.Name); err != nil {
		return res, err
	}
	krel, err := candidateKernel(cand, a.Set, h.KnownGood.Kernel)
	if err != nil {
		return res, err
	}
	base, err := candidateRoot(h.KnownGood.Cmdline)
	if err != nil {
		return res, err
	}
	cmdline := testCmdline(base, api.StageArgs{BootTimeoutSec: a.BootTimeoutSec, WatchdogOpenTimeoutSec: a.WatchdogOpenTimeoutSec}, job)
	if err = os.MkdirAll(bootDir, 0o755); err != nil {
		return res, err
	}
	w := h.UKI
	w.Build = func(ctx context.Context, krel, image, cmdline, out string) error {
		cf := out + ".cmdline"
		if err := os.WriteFile(cf, []byte(cmdline+"\n"), 0o600); err != nil {
			return err
		}
		defer os.Remove(cf)
		return h.run(ctx, nil, "mkinitcpio", "-r", cand, "-k", krel, "-U", out, "--kernelimage", image, "--cmdline", cf)
	}
	if err = writeUKIWith(ctx, krel, filepath.Join(cand, "usr/lib/modules", krel, "vmlinuz"), cmdline, filepath.Join(bootDir, "uki.efi"), w); err != nil {
		return res, err
	}
	title := fmt.Sprintf("maclab %s: candidate %s, %s", job, a.Set.Name, krel)
	os.WriteFile(filepath.Join(bootDir, "krel"), []byte(krel), 0o644)
	if err = os.WriteFile(filepath.Join(bootDir, limineEntry), []byte(h.Lim.entryText(job, title)), 0o644); err != nil {
		return res, err
	}
	if err = h.Lim.writeEntries(); err != nil {
		return res, err
	}
	// The record first: candidate_cleanup reads it to put boot.bin back.
	if err = writeJSONAtomic(filepath.Join(h.WorkDir, candidateState), candidateRecord{Job: job, Set: a.Set.Name, BootLoader: res.BootLoader}); err != nil {
		return res, err
	}
	if res.BootLoader {
		if err = h.swapBootLoader(ctx, cand, krel); err != nil {
			return res, err
		}
		res.RestoreSteps = candidateRestoreSteps
	}
	res.Entry, res.KernelRelease, res.Cmdline = entryPrefix+job, krel, cmdline
	return res, nil
}

func (h *candidateHost) filesDir(job string) string {
	return filepath.Join(h.WorkDir, "candidate-files", job)
}

// fetchSet puts every file of the set in place and checks its sha256 before
// anything on the Mac changes.
func (h *candidateHost) fetchSet(ctx context.Context, job string, set api.CandidateSet) ([]string, error) {
	dir := h.filesDir(job)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	var files []string
	for _, p := range set.Packages {
		dst := filepath.Join(dir, p.Name+"-"+strings.ReplaceAll(p.Version, "/", "_")+".pkg.tar.zst")
		var err error
		if strings.HasPrefix(p.URL, "/") {
			err = copyFile(p.URL, dst)
		} else {
			err = h.Fetch(ctx, p.SHA256, dst)
		}
		if err == nil {
			var got string
			if got, err = fileSHA256(dst); err == nil && got != p.SHA256 {
				err = fmt.Errorf("sha256 is %s, want %s", got, p.SHA256)
			}
		}
		if err != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("%s %s: %w", p.Name, p.Version, err)
		}
		files = append(files, dst)
	}
	return files, nil
}

// installed is the known-good system holding package name at version.
func (h *candidateHost) installed(name, version string) bool {
	if version == "" || h.PacmanDB == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(h.PacmanDB, name+"-"+version, "desc"))
	return err == nil
}

// swapBootLoader keeps boot.bin as boot-prev.bin, builds the candidate's
// beside it from its m1n1, U-Boot, kernel krel's device trees and m1n1.conf
// (update-m1n1, as pacman's hook does), checks it, and only then puts it in
// place. A build that is not sound leaves boot.bin as it was.
func (h *candidateHost) swapBootLoader(ctx context.Context, cand, krel string) error {
	cur := filepath.Join(h.M1N1Dir, "boot.bin")
	prev := filepath.Join(h.M1N1Dir, bootBinPrev)
	staged := filepath.Join(h.M1N1Dir, bootBinStaged)
	m1n1 := filepath.Join(cand, "usr/lib/asahi-boot/m1n1.bin")
	uboot := filepath.Join(cand, "usr/lib/asahi-boot/u-boot-nodtb.bin")
	dtbs := filepath.Join(cand, "usr/lib/modules", krel, "dtbs", "*.dtb")
	for _, f := range []string{m1n1, uboot} {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("the candidate has no %s, so no boot.bin can be built from it; boot.bin is unchanged", strings.TrimPrefix(f, cand+"/"))
		}
	}
	if m, _ := filepath.Glob(dtbs); len(m) == 0 {
		return fmt.Errorf("the candidate's kernel %s has no device trees (usr/lib/modules/%s/dtbs); boot.bin is unchanged", krel, krel)
	}
	if err := copyFile(cur, prev); err != nil {
		return fmt.Errorf("keeping boot.bin: %w", err)
	}
	if err := syncFile(prev); err != nil {
		return err
	}
	os.Remove(staged)
	env := append(os.Environ(), "M1N1="+m1n1, "U_BOOT="+uboot, "DTBS="+dtbs, "CONFIG="+filepath.Join(cand, "etc/m1n1.conf"))
	if err := h.run(ctx, env, "update-m1n1", staged); err != nil {
		os.Remove(staged)
		os.Remove(staged + ".new")
		return fmt.Errorf("%w; boot.bin is unchanged", err)
	}
	if err := checkBootBin(staged, m1n1, uboot); err != nil {
		os.Remove(staged)
		return fmt.Errorf("the boot.bin built from the candidate is not sound, so boot.bin is unchanged: %w", err)
	}
	if err := syncFile(staged); err != nil {
		return err
	}
	if err := os.Rename(staged, cur); err != nil {
		return err
	}
	return syncFile(cur)
}

// restoreBootLoader puts the known-good boot.bin, kept as boot-prev.bin by
// swapBootLoader, back in place.
func (h *candidateHost) restoreBootLoader() error {
	prev := filepath.Join(h.M1N1Dir, bootBinPrev)
	staged := filepath.Join(h.M1N1Dir, bootBinStaged)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("putting the known-good boot loader back: %w; boot.bin is still the candidate's", err)
	}
	if err := copyFile(prev, staged); err != nil {
		return err
	}
	if err := syncFile(staged); err != nil {
		return err
	}
	if err := os.Rename(staged, filepath.Join(h.M1N1Dir, "boot.bin")); err != nil {
		return err
	}
	return syncFile(filepath.Join(h.M1N1Dir, "boot.bin"))
}

// reM1N1Option is a line update-m1n1 appends from m1n1.conf.
var reM1N1Option = regexp.MustCompile(`^(chosen\.[^=]+|display|mitigations)=.*$`)

// checkBootBin checks a boot.bin update-m1n1 built, the way m1n1 stage 1
// reads it: the candidate's m1n1 whole, then whole device trees, then
// U-Boot gzipped and whole, then only m1n1.conf options. A cut-short or
// empty file, or one of other parts, is refused.
func checkBootBin(path, m1n1Path, ubootPath string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m, err := os.ReadFile(m1n1Path)
	if err != nil {
		return err
	}
	u, err := os.ReadFile(ubootPath)
	if err != nil {
		return err
	}
	switch {
	case len(b) == 0:
		return errors.New("it is empty")
	case len(m) == 0:
		return errors.New("the candidate's m1n1.bin is empty")
	case !bytes.HasPrefix(b, m):
		n := min(len(b), len(m))
		i := 0
		for i < n && b[i] == m[i] {
			i++
		}
		return fmt.Errorf("it does not start with the candidate's m1n1 (%d bytes): they differ from byte %d of %d", len(m), i, len(b))
	}
	rest := b[len(m):]
	dtbs := 0
	for len(rest) >= 8 && binary.BigEndian.Uint32(rest) == 0xd00dfeed {
		n := binary.BigEndian.Uint32(rest[4:8])
		if n < 40 || uint64(n) > uint64(len(rest)) {
			return fmt.Errorf("device tree %d is cut short: %d bytes left of its %d", dtbs+1, len(rest), n)
		}
		rest, dtbs = rest[n:], dtbs+1
	}
	if dtbs == 0 {
		return errors.New("no device tree follows m1n1")
	}
	if len(rest) < 2 || rest[0] != 0x1f || rest[1] != 0x8b {
		return fmt.Errorf("no gzipped U-Boot follows the %d device trees", dtbs)
	}
	r := bytes.NewReader(rest)
	z, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("U-Boot: %w", err)
	}
	z.Multistream(false)
	got, err := io.ReadAll(z)
	if err != nil {
		return fmt.Errorf("U-Boot is cut short or damaged: %w", err)
	}
	if !bytes.Equal(got, u) {
		return fmt.Errorf("U-Boot is not the candidate's: %d bytes, want %d", len(got), len(u))
	}
	for _, l := range strings.Split(string(rest[len(rest)-r.Len():]), "\n") {
		if l != "" && !reM1N1Option.MatchString(l) {
			if len(l) > 40 {
				l = l[:40]
			}
			return fmt.Errorf("unexpected bytes after U-Boot: %q", l)
		}
	}
	return nil
}

// candidateRoot is the known-good cmdline booting @lab-candidate instead of @.
func candidateRoot(knownGood string) (string, error) {
	var out []string
	done := false
	for _, f := range strings.Fields(knownGood) {
		if v, ok := strings.CutPrefix(f, "rootflags="); ok {
			opts := []string{"subvol=" + api.CandidateSubvolume}
			for _, o := range strings.Split(v, ",") {
				if o != "" && !strings.HasPrefix(o, "subvol=") && !strings.HasPrefix(o, "subvolid=") {
					opts = append(opts, o)
				}
			}
			f, done = "rootflags="+strings.Join(opts, ","), true
		}
		out = append(out, f)
	}
	if !done {
		out = append(out, "rootflags=subvol="+api.CandidateSubvolume)
	}
	if !hasParam(knownGood, "root") {
		return "", fmt.Errorf("the known-good cmdline has no root=: %q", knownGood)
	}
	return withDiskUnlock(strings.Join(out, " "), knownGood), nil
}

// candidateKernel is the kernel a set installs, as pacman's database in the
// candidate lists its files, else the known-good kernel. The candidate's
// modules directories alone can't tell: pacman's hooks keep the running
// kernel's modules through an upgrade (uname -r, which in pacman's chroot is
// the known-good kernel), pkgbase and vmlinuz included.
func candidateKernel(cand string, set api.CandidateSet, knownGood string) (string, error) {
	for _, p := range set.Packages {
		for _, krel := range pacmanKernels(cand, p.Name) {
			if _, err := os.Stat(filepath.Join(cand, "usr/lib/modules", krel, "vmlinuz")); err == nil {
				return krel, nil
			}
		}
	}
	if _, err := os.Stat(filepath.Join(cand, "usr/lib/modules", knownGood, "vmlinuz")); err == nil {
		return knownGood, nil
	}
	return "", errors.New("the candidate has neither a kernel from the set nor the known-good kernel")
}

// pacmanKernels are the kernel releases whose vmlinuz the package installed
// in root owns, from pacman's local database.
func pacmanKernels(root, name string) []string {
	dbs, _ := filepath.Glob(filepath.Join(root, "var/lib/pacman/local", name+"-*", "desc"))
	var out []string
	for _, desc := range dbs {
		b, err := os.ReadFile(desc)
		if err != nil || pacmanField(string(b), "NAME") != name {
			continue
		}
		files, err := os.ReadFile(filepath.Join(filepath.Dir(desc), "files"))
		if err != nil {
			continue
		}
		for _, f := range strings.Split(string(files), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(f), "usr/lib/modules/"); ok {
				if krel, ok := strings.CutSuffix(rest, "/vmlinuz"); ok && krel != "" && !strings.Contains(krel, "/") {
					out = append(out, krel)
				}
			}
		}
	}
	return out
}

// pacmanField is the first line of a %FIELD% section of a pacman database file.
func pacmanField(desc, field string) string {
	_, rest, ok := strings.Cut("\n"+desc, "\n%"+field+"%\n")
	if !ok {
		return ""
	}
	line, _, _ := strings.Cut(rest, "\n")
	return strings.TrimSpace(line)
}

const candidateWirePlumber = `# Lab candidate boot: speakers start muted.
wireplumber.settings = {
  device.restore-routes = false
  device.routes.default-sink-volume = 0.0
}
`

// Re-muting every second is how an unmute is refused on a candidate boot.
const candidateMuteUnit = `[Unit]
Description=Lab candidate boot: keep the speakers muted
After=wireplumber.service

[Service]
ExecStart=/bin/sh -c 'while :; do wpctl set-mute @DEFAULT_AUDIO_SINK@ 1; wpctl set-volume @DEFAULT_AUDIO_SINK@ 0; sleep 1; done'
Restart=always

[Install]
WantedBy=default.target
`

const candidateALSAUnit = `[Unit]
Description=Lab candidate boot: ALSA controls muted
After=alsa-restore.service sound.target
Wants=sound.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'for c in /proc/asound/card[0-9]*; do n=$${c##*card}; for k in Master Speaker PCM; do amixer -q -c "$n" sset "$k" 0%% mute 2>/dev/null; done; done; true'

[Install]
WantedBy=multi-user.target
`

const candidateJournald = "[Journal]\nStorage=persistent\n"

// prepareCandidate makes the snapshot a candidate: its root in fstab is
// itself, its journal is persistent and inside it (not on @log), and its
// speakers are muted for every user from the first boot.
func prepareCandidate(cand, set string) error {
	write := func(p, s string) error {
		p = filepath.Join(cand, p)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, []byte(s), 0o644)
	}
	if err := candidateFstab(filepath.Join(cand, "etc/fstab")); err != nil {
		return err
	}
	for p, s := range map[string]string{
		candidateMarker: set + "\n",
		"etc/wireplumber/wireplumber.conf.d/90-lab-candidate-mute.conf": candidateWirePlumber,
		"etc/systemd/user/lab-candidate-mute.service":                   candidateMuteUnit,
		"etc/systemd/system/lab-candidate-alsa-mute.service":            candidateALSAUnit,
		"etc/systemd/journald.conf.d/90-lab-candidate.conf":             candidateJournald,
	} {
		if err := write(p, s); err != nil {
			return err
		}
	}
	for link, target := range map[string]string{
		"etc/systemd/user/default.target.wants/lab-candidate-mute.service":           "/etc/systemd/user/lab-candidate-mute.service",
		"etc/systemd/system/multi-user.target.wants/lab-candidate-alsa-mute.service": "/etc/systemd/system/lab-candidate-alsa-mute.service",
	} {
		if err := relink(cand, link, target); err != nil {
			return err
		}
	}
	// snapper works on /.snapshots, a subvolume inside @ that the candidate
	// holds as an empty directory: its units would fail there, and count as
	// the set's failed units. Masked in the candidate only.
	for _, u := range snapperUnits {
		if err := relink(cand, "etc/systemd/system/"+u, "/dev/null"); err != nil {
			return err
		}
	}
	return os.MkdirAll(filepath.Join(cand, "var/log/journal"), 0o2755)
}

var snapperUnits = []string{"snapper-cleanup.service", "snapper-cleanup.timer", "snapper-timeline.service",
	"snapper-timeline.timer", "snapper-boot.service", "snapper-boot.timer"}

// relink makes link, inside root, a symlink to target.
func relink(root, link, target string) error {
	p := filepath.Join(root, link)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	os.Remove(p)
	return os.Symlink(target, p)
}

func candidateFstab(path string) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		f := strings.Fields(l)
		if len(f) < 4 || strings.HasPrefix(f[0], "#") {
			continue
		}
		switch f[1] {
		case "/":
			var opts []string
			for _, o := range strings.Split(f[3], ",") {
				if !strings.HasPrefix(o, "subvol=") && !strings.HasPrefix(o, "subvolid=") {
					opts = append(opts, o)
				}
			}
			f[3] = strings.Join(append(opts, "subvol=/"+api.CandidateSubvolume), ",")
			lines[i] = strings.Join(f, " ")
		case "/var/log":
			lines[i] = "# lab candidate: " + l
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
}

// IsCandidateBoot is whether cmdline boots @lab-candidate.
func IsCandidateBoot(cmdline string) bool {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, "rootflags="); ok {
			for _, o := range strings.Split(v, ",") {
				if o == "subvol="+api.CandidateSubvolume || o == "subvol=/"+api.CandidateSubvolume {
					return true
				}
			}
		}
	}
	return false
}

func (h *candidateHost) check(ctx context.Context, a api.CandidateCheckArgs) (api.CandidateCheck, error) {
	var c api.CandidateCheck
	if !IsCandidateBoot(h.Cmdline) {
		return c, errors.New("this is not a candidate boot")
	}
	out, err := h.Run(ctx, nil, "systemctl", "--user", "-M", h.GUIUser+"@", "is-active", "graphical-session.target")
	c.Desktop = err == nil && strings.TrimSpace(string(out)) == "active"
	if !c.Desktop {
		c.Problems = append(c.Problems, "the desktop session is not up")
	}
	drivers, _ := filepath.Glob(filepath.Join(h.DRMDir, "renderD*", "device", "driver"))
	for _, d := range drivers {
		if t, err := filepath.EvalSymlinks(d); err == nil {
			c.GPUDrivers = append(c.GPUDrivers, filepath.Base(t))
		}
	}
	if a.GPUDriver != "" && !contains(c.GPUDrivers, a.GPUDriver) {
		c.Problems = append(c.Problems, fmt.Sprintf("the GPU driver is %q, want %s", strings.Join(c.GPUDrivers, " "), a.GPUDriver))
	}
	out, err = h.Run(ctx, nil, "systemctl", "--failed", "--plain", "--no-legend")
	if err != nil {
		c.Problems = append(c.Problems, "systemctl --failed: "+err.Error())
	}
	for _, l := range strings.Split(string(out), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			c.FailedUnits = append(c.FailedUnits, f[0])
		}
	}
	if len(c.FailedUnits) > 0 {
		c.Problems = append(c.Problems, "failed units: "+strings.Join(c.FailedUnits, " "))
	}
	c.State = api.CandidateConfirmed
	if len(c.Problems) > 0 {
		c.State = api.CandidateNotYet
	}
	return c, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// result is read back on known-good after a candidate boot that was not
// confirmed: the candidate's journal, from its subvolume, and the early
// unit's note of the Lab boot it watched there. With neither, the candidate
// never ran.
func (h *candidateHost) result(ctx context.Context) (api.CandidateFailure, error) {
	r := api.CandidateFailure{State: api.CandidateBootFailed}
	if IsCandidateBoot(h.Cmdline) {
		return r, errors.New("still on the candidate boot")
	}
	umount, err := h.mountTop(ctx)
	if err != nil {
		return r, err
	}
	defer umount()
	var seen labBootSeen
	if b, err := os.ReadFile(filepath.Join(h.cand(), h.WorkDir, labBootDir, labBootSeenFile)); err == nil {
		json.Unmarshal(b, &seen)
	}
	r.BootID, r.Deadline = seen.BootID, seen.Deadline
	dir := filepath.Join(h.cand(), "var/log/journal")
	if _, err := os.Stat(dir); err != nil {
		r.Journal = "the candidate has no journal: " + err.Error()
		r.NotBooted = seen.BootID == ""
		return r, nil
	}
	out, err := h.Run(ctx, nil, "journalctl", "-D", dir, "--no-pager", "-o", "short-monotonic", "-n", "5000")
	if err != nil {
		return r, fmt.Errorf("journalctl: %v: %s", err, lastLines(string(out), 5))
	}
	r.Journal = string(out)
	if j := strings.TrimSpace(r.Journal); (j == "" || j == "-- No entries --") && seen.BootID == "" {
		r.NotBooted = true
	}
	return r, nil
}

// armLabBoot writes the Lab boot record boot_once arms for job into the
// candidate as well, when job's entry is the staged candidate. The candidate
// is a snapshot of @ from before boot_once, so it holds the last Lab boot's
// record: its early unit would find those deadlines long past and reboot the
// candidate as soon as it starts.
func (h *candidateHost) armLabBoot(ctx context.Context, job string, write func(dir string) error) error {
	var rec candidateRecord
	b, err := os.ReadFile(filepath.Join(h.WorkDir, candidateState))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return fmt.Errorf("%s: %w", candidateState, err)
	}
	if rec.Job != job {
		return nil
	}
	umount, err := h.mountTop(ctx)
	if err != nil {
		return err
	}
	defer umount()
	if _, err := os.Stat(h.cand()); err != nil {
		return fmt.Errorf("the candidate of %s is gone: %w", job, err)
	}
	return write(filepath.Join(h.cand(), h.WorkDir, labBootDir))
}

// cleanup removes the candidate's entry and, unless kept, its subvolume, and
// puts the known-good boot loader back after a set that changed it.
func (h *candidateHost) cleanup(ctx context.Context, job string, keep bool) error {
	var errs []error
	var rec candidateRecord
	restored := true
	if b, err := os.ReadFile(filepath.Join(h.WorkDir, candidateState)); err == nil && json.Unmarshal(b, &rec) == nil && rec.BootLoader {
		if err := h.restoreBootLoader(); err != nil {
			errs, restored = append(errs, err), false
		}
	}
	if job != "" && !strings.ContainsAny(job, "/. ") {
		errs = append(errs, os.RemoveAll(filepath.Join(h.Lim.Stage, job)))
	}
	errs = append(errs, h.Lim.writeEntries())
	if !keep {
		umount, err := h.mountTop(ctx)
		if err != nil {
			errs = append(errs, err)
		} else {
			if _, err := os.Stat(h.cand()); err == nil {
				errs = append(errs, h.run(ctx, nil, "btrfs", "subvolume", "delete", h.cand()))
			}
			umount()
		}
	}
	if restored {
		os.Remove(filepath.Join(h.WorkDir, candidateState))
	}
	return errors.Join(errs...)
}

// candidateHost for this Mac: Limine only, as the UKI one-shot needs it.
func (l *Linux) candidateHost() (*candidateHost, error) {
	b, err := detectBootloader()
	if err != nil {
		return nil, err
	}
	lim, ok := b.(*limineLayout)
	if !ok {
		return nil, errors.New("a candidate boot needs Limine")
	}
	if p := lim.problems(); len(p) > 0 {
		return nil, fmt.Errorf("one-shot hook not ready: %s", strings.Join(p, "; "))
	}
	kg, err := l.knownGood()
	if err != nil {
		return nil, err
	}
	root, err := findMount("/")
	if err != nil {
		return nil, err
	}
	if root.FSType != "btrfs" {
		return nil, fmt.Errorf("/ is %s, not btrfs", root.FSType)
	}
	esp, err := findESP()
	if err != nil {
		return nil, err
	}
	_, _, cmdline := l.Identity()
	return &candidateHost{
		Run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Env = env
			if name == "systemctl" || name == "journalctl" {
				return cmd.Output()
			}
			return combinedGroup(cmd)
		},
		Top: "/run/maclab-candidate", RootSource: root.Source, Lim: lim, KnownGood: kg,
		GUIUser: l.GUIUser, M1N1Dir: filepath.Join(esp.Target, "m1n1"), PacmanDB: "/var/lib/pacman/local", WorkDir: l.WorkDir,
		DRMDir: "/sys/class/drm", Cmdline: cmdline,
		UKI: ukiWriter{ScratchRoot: "/var/lib/maclab",
			Lock:  func() (func(), error) { return lockBootPartition(time.Minute) },
			Space: measureUKISpace},
	}, nil
}

func (l *Linux) StageCandidate(ctx context.Context, job string, a api.CandidateStageArgs, fetch Fetcher) (api.CandidateStageResult, error) {
	h, err := l.candidateHost()
	if err != nil {
		return api.CandidateStageResult{}, err
	}
	h.Fetch = fetch
	return h.stage(ctx, job, a)
}

func (l *Linux) CheckCandidate(ctx context.Context, a api.CandidateCheckArgs) (api.CandidateCheck, error) {
	_, _, cmdline := l.Identity()
	h := &candidateHost{Run: func(ctx context.Context, env []string, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}, GUIUser: l.GUIUser, DRMDir: "/sys/class/drm", Cmdline: cmdline}
	return h.check(ctx, a)
}

func (l *Linux) CandidateResult(ctx context.Context) (api.CandidateFailure, error) {
	h, err := l.candidateHost()
	if err != nil {
		return api.CandidateFailure{}, err
	}
	return h.result(ctx)
}

// ArmCandidateLabBoot hands write the candidate's Lab boot directory when
// entry is the staged candidate's.
func (l *Linux) ArmCandidateLabBoot(ctx context.Context, entry string, write func(dir string) error) error {
	job, ok := strings.CutPrefix(entry, entryPrefix)
	if !ok {
		return nil
	}
	if _, err := os.Stat(filepath.Join(l.WorkDir, candidateState)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	h, err := l.candidateHost()
	if err != nil {
		return err
	}
	return h.armLabBoot(ctx, job, write)
}

func (l *Linux) CleanupCandidate(ctx context.Context, job string, keep bool) error {
	h, err := l.candidateHost()
	if err != nil {
		return err
	}
	return h.cleanup(ctx, job, keep)
}

var reJobFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// keepCandidateJournal uploads the candidate's journal to the job as
// <name>/journal.txt.
func (a *Agent) keepCandidateJournal(ctx context.Context, c api.Command, name, journal string) []string {
	if !reJobFileName.MatchString(name) || strings.Contains(name, "..") {
		name = "candidate"
	}
	dir, err := a.outDir(c)
	if err != nil {
		a.log.Printf("candidate journal: %v", err)
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "journal.txt"), []byte(journal), 0o644); err != nil {
		a.log.Printf("candidate journal: %v", err)
		os.RemoveAll(dir)
		return nil
	}
	return a.uploadDir(ctx, c.JobID, name, dir)
}

// candidateSystem is a System that can boot a Package set.
type candidateSystem interface {
	ResolveCandidate(context.Context, api.CandidateResolveArgs) (api.CandidateResolved, error)
	StageCandidate(context.Context, string, api.CandidateStageArgs, Fetcher) (api.CandidateStageResult, error)
	CheckCandidate(context.Context, api.CandidateCheckArgs) (api.CandidateCheck, error)
	CandidateResult(context.Context) (api.CandidateFailure, error)
	CleanupCandidate(context.Context, string, bool) error
}

func (a *Agent) candidate(ctx context.Context, c api.Command) (any, error) {
	cs, ok := a.sys.(candidateSystem)
	if !ok {
		return nil, errors.New("agent system has no candidate boot support")
	}
	args := c.Args
	if len(args) == 0 {
		args = []byte("{}")
	}
	switch c.Kind {
	case api.CmdCandidateResolve:
		var s api.CandidateResolveArgs
		if err := json.Unmarshal(args, &s); err != nil {
			return nil, err
		}
		return cs.ResolveCandidate(ctx, s)
	case api.CmdCandidateStage:
		var s api.CandidateStageArgs
		if err := json.Unmarshal(args, &s); err != nil {
			return nil, err
		}
		return cs.StageCandidate(ctx, c.JobID, s, a.fetch)
	case api.CmdCandidateCheck:
		var s api.CandidateCheckArgs
		if err := json.Unmarshal(args, &s); err != nil {
			return nil, err
		}
		return cs.CheckCandidate(ctx, s)
	case api.CmdCandidateResult:
		var s api.CandidateResultArgs
		if err := json.Unmarshal(args, &s); err != nil {
			return nil, err
		}
		f, err := cs.CandidateResult(ctx)
		if err == nil {
			f.Files = a.keepCandidateJournal(ctx, c, s.Name, f.Journal)
		}
		return f, err
	default:
		var s api.CandidateCleanupArgs
		if err := json.Unmarshal(args, &s); err != nil {
			return nil, err
		}
		return nil, cs.CleanupCandidate(ctx, c.JobID, s.Keep)
	}
}
