package agent

import (
	"time"

	"golang.org/x/sys/unix"
)

// exited waits until process pid has ended, without reaping it: macOS has no
// waitid WNOWAIT that Go can call, so it polls for the zombie. It reports
// false when it cannot tell. lab-agent runs on Linux; this is for its tests.
func exited(pid int) bool {
	const zombie = 5 // SZOMB
	for {
		p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || p.Proc.P_pid != int32(pid) {
			return false
		}
		if p.Proc.P_stat == zombie {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
}
