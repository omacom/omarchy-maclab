package agent

import "golang.org/x/sys/unix"

// exited waits until process pid has ended, without reaping it. It reports
// false when it cannot tell.
func exited(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err == nil
		}
	}
}
