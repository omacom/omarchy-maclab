//go:build !labtest

package main

import "github.com/omacom/omarchy-maclab/internal/agent"

// labClock is the wall clock: only a test build runs labd on another.
func labClock() agent.Clock { return nil }
