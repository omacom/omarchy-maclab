package minitest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The lab account's scripts are run by hand with sudo -u lab from wherever
// the admin stands, often his home, which that account cannot read: each
// leaves that folder first, so that what it runs does not complain of it at
// every turn. bash's own complaint as it starts, before the first line of
// the script, is the one left.
func TestTheLabAccountsScriptsLeaveAFolderTheyCannotRead(t *testing.T) {
	m := newMini(t)
	for _, d := range []string{"var/lab/run", "var/lab/log", "var/lab/data", "var/lab/secrets"} {
		if err := os.MkdirAll(m.path(d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, script := range [][]string{
		{"backup", "--dry-run"},
		{"parts"},
		{"pending-updates"},
		{"notify", "agents", "a line"},
		{"dead-man", "--dry-run"},
		{"restore-drill", "--dry-run"},
	} {
		gone := filepath.Join(t.TempDir(), "home")
		if err := os.Mkdir(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		// The folder it is started in is gone by then: no account can read it.
		cmd := exec.Command("/bin/bash", "-c", `cd "$1" && rmdir "$1" && shift && exec "$@"`, "cwd", gone, filepath.Join(m.tree, "scripts", "mini", script[0]))
		cmd.Args = append(cmd.Args, script[1:]...)
		cmd.Env = m.env
		out, _ := cmd.CombinedOutput()
		if n := strings.Count(string(out), "error retrieving current directory"); n > 1 {
			t.Errorf("%s complained of the folder it was started in %d times:\n%s", script[0], n, out)
		}
	}
}
