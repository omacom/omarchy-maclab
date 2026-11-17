//go:build !linux && !darwin

package agent

func exited(int) bool { return false }
