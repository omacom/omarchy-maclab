package macosrunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// macOS's own tools, and the file that names the user it logs in
// automatically.
const (
	fdesetupPath    = "/usr/bin/fdesetup"
	sysadminctlPath = "/usr/sbin/sysadminctl"
	defaultsPath    = "/usr/bin/defaults"
	loginWindowPref = "/Library/Preferences/com.apple.loginwindow"
)

// Setup is `sudo lab-macos-runner setup`, at the Mac: it asks on the
// terminal whether to turn FileVault off and whether to log User in
// automatically, two questions, and changes each only on a yes. fdesetup
// and sysadminctl then ask for the password themselves, on the same
// terminal.
type Setup struct {
	// In and Out are the terminal, /dev/tty: never stdin, which may be a
	// pipe. A child given In reads the terminal only when In is a file.
	In  io.Reader
	Out io.Writer
	// User is who ran it with sudo (SUDO_USER): the user to log in.
	User string
	// The tools; "" is macOS's own.
	Fdesetup, Sysadminctl, Defaults string
}

func or(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (s *Setup) say(f string, a ...any) { fmt.Fprintf(s.Out, f+"\n", a...) }

// ask asks a yes/no question; only y or yes is a yes, and a terminal that
// gives nothing is a no.
func (s *Setup) ask(q string) bool {
	fmt.Fprintf(s.Out, "%s [y/N] ", q)
	a := strings.ToLower(strings.TrimSpace(readLine(s.In)))
	return a == "y" || a == "yes"
}

// readLine reads one line a byte at a time, so that nothing after it is
// taken from the terminal before the next question or tool.
func readLine(r io.Reader) string {
	var b strings.Builder
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n == 1 {
			if one[0] == '\n' {
				return b.String()
			}
			b.WriteByte(one[0])
		}
		if err != nil {
			return b.String()
		}
	}
}

func (s *Setup) output(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return strings.TrimSpace(string(out)), err
}

// run runs a tool that may ask for a password, on the terminal.
func (s *Setup) run(ctx context.Context, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	if f, ok := s.In.(*os.File); ok {
		c.Stdin = f
	}
	c.Stdout, c.Stderr = s.Out, s.Out
	return c.Run()
}

func fileVaultOn(ctx context.Context, fdesetup string) (bool, error) {
	out, err := exec.CommandContext(ctx, fdesetup, "isactive").Output()
	switch strings.TrimSpace(string(out)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if err == nil {
		err = fmt.Errorf("fdesetup isactive said %q", strings.TrimSpace(string(out)))
	}
	return false, err
}

func autoLoginUser(ctx context.Context, defaults string) string {
	out, err := exec.CommandContext(ctx, defaults, "read", loginWindowPref, "autoLoginUser").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var errSetupUser = errors.New("run it with sudo from the account macOS is to log in: sudo lab-macos-runner setup")

// Run asks the two questions and does what was answered yes.
func (s *Setup) Run(ctx context.Context) error {
	if s.User == "" || s.User == "root" {
		return errSetupUser
	}
	fdesetup, sysadminctl := or(s.Fdesetup, fdesetupPath), or(s.Sysadminctl, sysadminctlPath)
	s.say("A macOS run needs nobody at this Mac only when macOS starts and logs %s in by itself. Nothing changes without your yes.", s.User)

	on, err := fileVaultOn(ctx, fdesetup)
	if err != nil {
		return fmt.Errorf("FileVault: %w", err)
	}
	keptOn := false
	if on {
		s.say("\nFileVault is on: at every boot macOS waits for a password typed at the screen.")
		if s.ask("Turn FileVault off (fdesetup disable, which asks for your password)?") {
			if err := s.run(ctx, fdesetup, "disable"); err != nil {
				return fmt.Errorf("fdesetup disable: %w: FileVault is as it was", err)
			}
			s.say("FileVault is being turned off; the disk decrypts in the background.")
		} else {
			keptOn = true
			s.say("FileVault stays on.")
		}
	} else {
		s.say("\nFileVault is off.")
	}

	switch {
	case autoLoginUser(ctx, or(s.Defaults, defaultsPath)) == s.User:
		s.say("macOS logs %s in automatically already.", s.User)
	case keptOn:
		s.say("macOS does not log in automatically while FileVault is on, so this Mac stays Attended: a macOS run waits for a person.")
		return nil
	case s.ask(fmt.Sprintf("\nLog %s in automatically at every boot (sysadminctl -autologin set, which asks for %s's password)?", s.User, s.User)):
		if err := s.run(ctx, sysadminctl, "-autologin", "set", "-userName", s.User, "-password", "-"); err != nil {
			return fmt.Errorf("sysadminctl -autologin set: %w: automatic login is as it was", err)
		}
		s.say("macOS logs %s in automatically from the next boot.", s.User)
	default:
		s.say("Automatic login stays as it is, so this Mac stays Attended: a macOS run waits for a person.")
		return nil
	}
	s.say("\nThe Mac stays Attended until a boot reaches %s's desktop with nobody typing; the runner tells the lab then.", s.User)
	return nil
}
