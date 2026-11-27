package agent

import (
	"strings"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// installedStack is the installed version of each package of the Omarchy
// Mac stack, from pacman -Q; nil where pacman cannot be asked.
func installedStack() map[string]string {
	out, err := toolOutput("pacman", "-Q")
	if err != nil {
		return nil
	}
	return stackOf(out)
}

// stackOf reads pacman -Q's "name version" lines, keeping those a Mac
// reports (api.BootedPackage).
func stackOf(out []byte) map[string]string {
	var m map[string]string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || !api.BootedPackage(f[0]) {
			continue
		}
		if m == nil {
			m = map[string]string{}
		}
		m[f[0]] = f[1]
	}
	return m
}
