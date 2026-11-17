package server

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"
)

// labd's own log is outside what a forgetting reaches, so a Lease ended
// over its holder is logged with the holder's account, never a handle:
// not the holder's, not the one their lab named the holder with, not the
// Maintainer's who ended it.
func TestALeaseEndedByForceIsLoggedByAccount(t *testing.T) {
	l := rolesLab(t, nil)
	logs := &lockedBuffer{b: &bytes.Buffer{}}
	l.s.log.SetOutput(logs)
	l.lrMac("desk-mac", "j313", nil)
	grace := l.lrMember(graceID)
	grace.mustLease("desk-mac", "grace@desk", time.Hour)
	l.signedIn(maintainerID).did("/people/"+strconv.FormatInt(graceID, 10)+"/leases/desk-mac/end", "why", "grace asked", "sure", "yes")

	var ended []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "desk-mac") && strings.Contains(line, "ended") {
			ended = append(ended, line)
		}
	}
	if len(ended) == 0 {
		t.Fatalf("no ending in labd's log:\n%s", logs.String())
	}
	for _, line := range ended {
		if strings.Contains(line, "grace") || strings.Contains(line, "ravi") || !strings.Contains(line, "account "+strconv.FormatInt(graceID, 10)) {
			t.Errorf("the ending in labd's log: %s", line)
		}
	}
}
