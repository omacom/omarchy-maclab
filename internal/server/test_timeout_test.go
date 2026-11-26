package server

import (
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// labd waits for a test at least as long as lab-agent lets it run: a bench
// builtin with no timeout of its own, as lab run --test bench sends it, runs
// its whole list.
func TestLabdWaitsForABenchListAsLongAsTheAgentRunsIt(t *testing.T) {
	r := &jobRun{s: &Server{cfg: Config{TestGrace: 2 * time.Minute}}}
	for builtin, list := range map[string]time.Duration{"bench": 30 * time.Minute, "bench-extended": 75 * time.Minute} {
		if got := r.testTimeout(api.TestSpec{Name: builtin, Builtin: builtin}); got != list+2*time.Minute {
			t.Errorf("%s: labd waits %s, want %s and the grace", builtin, got, list)
		}
	}
	if got := r.testTimeout(api.TestSpec{Name: "boot-health", Builtin: "boot-health"}); got != 7*time.Minute {
		t.Errorf("boot-health: labd waits %s, want 7m", got)
	}
	if got := r.testTimeout(api.TestSpec{Name: "a-bench", Builtin: "bench", TimeoutSec: 600}); got != 12*time.Minute {
		t.Errorf("a bench with its own timeout: labd waits %s, want 12m", got)
	}
}
