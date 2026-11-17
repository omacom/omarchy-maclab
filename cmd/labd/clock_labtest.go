//go:build labtest

package main

import (
	"os"
	"strconv"

	"github.com/omacom/omarchy-maclab/internal/agent"
)

// labClock, in a labd built with -tags labtest for the script suites, is the
// wall clock sped up MACLAB_TEST_CLOCK_SCALE times. A release never has it.
func labClock() agent.Clock {
	k, err := strconv.ParseFloat(os.Getenv("MACLAB_TEST_CLOCK_SCALE"), 64)
	if err != nil || k <= 1 {
		return nil
	}
	return agent.NewScaledClock(k)
}
