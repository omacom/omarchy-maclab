package repotest

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// labd waits on its own clock (Server.clock), so that tests can run its
// leases, grace periods and backoffs fast. A wait on the wall clock is
// allowed only where it is listed here, with why.
var labdWallWaits = map[string]int{
	"gc.go":      1, // hourly artifact GC: no test waits for it
	"leaving.go": 1, // a request's 20 ms poll on another one's leaving, bounded in wall time
}

var wallWait = regexp.MustCompile(`\btime\.(Sleep|After|AfterFunc|NewTimer|NewTicker|Tick)\(`)
var sqlNow = regexp.MustCompile(`(?i)CURRENT_TIMESTAMP|datetime\('now'|unixepoch\(\)|strftime\([^)]*'now'`)

func TestLabdWaitsOnItsOwnClock(t *testing.T) {
	dir := filepath.Join(root(t), "internal", "server")
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(wallWait.FindAll(b, -1)); n != labdWallWaits[name] {
			t.Errorf("internal/server/%s waits on the wall clock %d times, want %d: use s.clock()", name, n, labdWallWaits[name])
		}
		// Stored times come from Go, on labd's clock, never from SQLite.
		if sqlNow.Match(b) {
			t.Errorf("internal/server/%s has SQLite read the time: pass s.now() instead", name)
		}
	}
}
