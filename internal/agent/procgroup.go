package agent

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// stopGrace is how long the programs a command runs have, once lab-agent
// stops it, between SIGTERM and SIGKILL.
const stopGrace = 5 * time.Second

// runGroup runs cmd, made with exec.CommandContext, as a process group of
// its own, and ends it as one. When cmd's context ends, the group gets
// SIGTERM; cmd.WaitDelay (stopGrace if unset) later its program is killed
// and its pipes are closed, so a child that keeps them open holds nothing
// up. Once the program has ended, and before it is reaped, whatever is left
// in its group gets SIGTERM (unless it already had one) and, stopGrace after
// it, SIGKILL. So lab-agent's Run, which waits for its commands, ends in a
// bounded time, and a command leaves no process behind in its group. A
// child that calls setsid leaves the group, and outlives the command: only
// a cgroup per command would hold it.
func runGroup(cmd *exec.Cmd) error {
	wait, err := startGroup(cmd)
	if err != nil {
		return err
	}
	return wait()
}

// startGroup is runGroup in two steps, for a caller that reads a pipe while
// the program runs: it starts cmd, and wait does the rest of runGroup.
func startGroup(cmd *exec.Cmd) (wait func() error, err error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	var mu sync.Mutex
	var termed time.Time
	cmd.Cancel = func() error {
		mu.Lock()
		termed = time.Now()
		mu.Unlock()
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = stopGrace
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() error {
		// The group is signalled while its leader is a zombie, which keeps
		// the group's number from going to another process.
		pg := cmd.Process.Pid
		if exited(pg) {
			mu.Lock()
			if termed.IsZero() {
				syscall.Kill(-pg, syscall.SIGTERM)
				termed = time.Now()
			}
			mu.Unlock()
		}
		err := cmd.Wait()
		mu.Lock()
		t := termed
		mu.Unlock()
		endGroup(pg, t)
		return err
	}, nil
}

// combinedGroup is cmd.CombinedOutput through runGroup.
func combinedGroup(cmd *exec.Cmd) ([]byte, error) {
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := runGroup(cmd)
	return b.Bytes(), err
}

// endGroup ends what is left of process group pg: SIGTERM, unless it had
// one at termed, then SIGKILL once stopGrace has passed since.
func endGroup(pg int, termed time.Time) {
	if termed.IsZero() {
		if syscall.Kill(-pg, syscall.SIGTERM) != nil {
			return // nothing is left
		}
		termed = time.Now()
	}
	for time.Since(termed) < stopGrace {
		if syscall.Kill(-pg, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	syscall.Kill(-pg, syscall.SIGKILL)
}

// toolTimeout bounds a system tool lab-agent runs outside any command's
// context (grub-editenv, chattr, findmnt, systemctl, sync). It is a bound,
// not a stop: a tool that changes the boot is not cut when a command is.
var toolTimeout = time.Minute

// tool runs a system tool through runGroup, ended after toolTimeout, its
// output to stdout and stderr (nil discards).
func tool(stdout, stderr io.Writer, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return runGroup(cmd)
}

// toolOutput is a tool's stdout, as cmd.Output.
func toolOutput(name string, args ...string) ([]byte, error) {
	var b bytes.Buffer
	err := tool(&b, nil, name, args...)
	return b.Bytes(), err
}

// toolCombined is a tool's stdout and stderr together, as cmd.CombinedOutput.
func toolCombined(name string, args ...string) ([]byte, error) {
	var b bytes.Buffer
	err := tool(&b, &b, name, args...)
	return b.Bytes(), err
}
