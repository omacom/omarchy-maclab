package macosrunner

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMac is fdesetup, sysadminctl and defaults as small scripts: each
// says what it was asked to change in a log, and changes nothing.
type fakeMac struct {
	dir, log string
}

func newFakeMac(t *testing.T, fileVault, autoLogin string, failing ...string) *fakeMac {
	t.Helper()
	dir := t.TempDir()
	m := &fakeMac{dir: dir, log: filepath.Join(dir, "changed")}
	fail := map[string]string{}
	for _, f := range failing {
		fail[f] = "exit 1"
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("fdesetup", `if [ "$1" = isactive ]; then echo `+fileVault+`; [ `+fileVault+` = true ]; exit; fi
echo "fdesetup $*" >> `+m.log+`
`+fail["fdesetup"])
	write("sysadminctl", `echo "sysadminctl $*" >> `+m.log+`
`+fail["sysadminctl"])
	read := "exit 1"
	if autoLogin != "" {
		read = "echo " + autoLogin
	}
	write("defaults", `[ "$1" = read ] && [ "$2" = `+loginWindowPref+` ] && [ "$3" = autoLoginUser ] || { echo "defaults $*" >> `+m.log+`; exit 1; }
`+read)
	return m
}

func (m *fakeMac) changed(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(m.log)
	if os.IsNotExist(err) {
		return ""
	} else if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setUp runs setup as user with the answers typed on its terminal.
func (m *fakeMac) setUp(t *testing.T, user, answers string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	s := &Setup{In: strings.NewReader(answers), Out: &out, User: user,
		Fdesetup: filepath.Join(m.dir, "fdesetup"), Sysadminctl: filepath.Join(m.dir, "sysadminctl"), Defaults: filepath.Join(m.dir, "defaults")}
	err := s.Run(context.Background())
	return out.String(), err
}

func TestSetupAsksTwiceAndChangesOnlyWhatIsAnsweredYes(t *testing.T) {
	for _, c := range []struct {
		name, answers, changed string
	}{
		{"both yes", "y\nyes\n", "fdesetup disable\nsysadminctl -autologin set -userName ada -password -\n"},
		{"FileVault only", "Y\nn\n", "fdesetup disable\n"},
		{"automatic login refused after FileVault", "yes\n\n", "fdesetup disable\n"},
		{"no to FileVault asks nothing more", "no\ny\n", ""},
		{"nothing typed", "", ""},
		{"anything but yes", "sure\nok\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			m := newFakeMac(t, "true", "")
			out, err := m.setUp(t, "ada", c.answers)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got := m.changed(t); got != c.changed {
				t.Errorf("changed %q, want %q\n%s", got, c.changed, out)
			}
			if !strings.Contains(out, "Turn FileVault off (fdesetup disable") {
				t.Errorf("the FileVault question was not asked:\n%s", out)
			}
			if strings.Contains(c.changed, "sysadminctl") != strings.Contains(out, "macOS logs ada in automatically from the next boot") {
				t.Errorf("says the wrong thing about automatic login:\n%s", out)
			}
		})
	}
}

// With FileVault off already, only the automatic login is asked; with that
// set for the user too, nothing is asked and nothing changes.
func TestSetupAsksOnlyWhatIsNotSetYet(t *testing.T) {
	m := newFakeMac(t, "false", "")
	out, err := m.setUp(t, "ada", "y\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Turn FileVault off") || !strings.Contains(out, "FileVault is off") {
		t.Errorf("asked about FileVault while it is off:\n%s", out)
	}
	if got := m.changed(t); got != "sysadminctl -autologin set -userName ada -password -\n" {
		t.Errorf("changed %q", got)
	}

	m = newFakeMac(t, "false", "ada")
	out, err = m.setUp(t, "ada", "y\ny\n")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[y/N]") || !strings.Contains(out, "logs ada in automatically already") {
		t.Errorf("asked with everything set:\n%s", out)
	}
	if got := m.changed(t); got != "" {
		t.Errorf("changed %q with everything set", got)
	}

	// Another user logged in automatically: asked for this one.
	m = newFakeMac(t, "false", "grace")
	if _, err := m.setUp(t, "ada", "n\n"); err != nil {
		t.Fatal(err)
	}
	if got := m.changed(t); got != "" {
		t.Errorf("changed %q on a no", got)
	}
}

func TestSetupStopsWhenAToolFailsAndNeedsTheUserWhoRanSudo(t *testing.T) {
	m := newFakeMac(t, "true", "", "fdesetup")
	if _, err := m.setUp(t, "ada", "y\ny\n"); err == nil || !strings.Contains(err.Error(), "fdesetup disable") {
		t.Errorf("a failed fdesetup: %v", err)
	}
	if got := m.changed(t); got != "fdesetup disable\n" {
		t.Errorf("went on after fdesetup failed: %q", got)
	}

	m = newFakeMac(t, "false", "", "sysadminctl")
	if _, err := m.setUp(t, "ada", "y\n"); err == nil || !strings.Contains(err.Error(), "sysadminctl") {
		t.Errorf("a failed sysadminctl: %v", err)
	}

	for _, user := range []string{"", "root"} {
		m = newFakeMac(t, "true", "")
		if _, err := m.setUp(t, user, "y\ny\n"); err != errSetupUser {
			t.Errorf("user %q: %v", user, err)
		}
		if got := m.changed(t); got != "" {
			t.Errorf("user %q: changed %q", user, got)
		}
	}
}

// One answer per question: the second is not taken by the first.
func TestSetupReadsOneLinePerQuestion(t *testing.T) {
	r := strings.NewReader("y\nn\n")
	if a, b, c := readLine(r), readLine(r), readLine(r); a != "y" || b != "n" || c != "" {
		t.Errorf("%q %q %q", a, b, c)
	}
}
