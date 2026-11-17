package agent

import (
	"strings"
	"testing"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// An auto-unlocked Mac's known-good cmdline, as lab-only-autounlock leaves it
// and setup records it from /proc/cmdline.
const (
	unlockUUID      = "c8761528-040e-4dad-992e-d8f1800d46e0"
	unlockName      = "rd.luks.name=" + unlockUUID + "=root"
	unlockKey       = "rd.luks.key=" + unlockUUID + "=/etc/cryptsetup-keys.d/root.key"
	unlockKnownGood = "root=/dev/mapper/root rw rootflags=subvol=@ " + unlockName + " zswap.enabled=0 quiet splash " + unlockKey
)

func TestEveryTestCmdlineKeepsTheDiskUnlockingOnce(t *testing.T) {
	for _, a := range []api.StageArgs{
		{},
		{CmdlineBase: "default"},
		{CmdlineBase: "root=/dev/mapper/root rw"},
		{CmdlineStrip: []string{"rd.*"}},
		{CmdlineStrip: []string{"rd.luks.key"}},
		{CmdlineBase: "default", Serial: true, Cmdline: "foo=1"},
	} {
		got, err := stageCmdline(unlockKnownGood, stock, a, "j1")
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(got)
		for _, p := range []string{unlockName, unlockKey} {
			if n := countField(fields, p); n != 1 {
				t.Errorf("%+v: %q appears %d times in %s", a, p, n, got)
			}
		}
	}
}

func TestATestCmdlinesOwnDiskUnlockingWins(t *testing.T) {
	own := "root=/dev/mapper/r rw rd.luks.uuid=luks-" + strings.ToUpper(unlockUUID) + " rd.luks.key=" + unlockUUID + "=/other.key"
	got, err := stageCmdline(unlockKnownGood, stock, api.StageArgs{CmdlineBase: own}, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, unlockName) || strings.Contains(got, unlockKey) {
		t.Fatalf("the base sets up %s itself, but the known-good's was added: %s", unlockUUID, got)
	}
	// a cmdline without disk unlocking gets none
	got, err = stageCmdline(m3KnownGood, stock, api.StageArgs{}, "j2")
	if err != nil || strings.Contains(got, "luks") {
		t.Fatalf("%q %v", got, err)
	}
}
