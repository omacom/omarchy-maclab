package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

// A build with a Mac's config asks the Mac for it the first time; a Mac
// that does not answer leaves POST /api/builds waiting for as long as labd
// would wait on a Mac, past the edge's 100 s. The ask has a time of its own,
// and the answer says whose config could not be read.
func TestAskingForABuildWithAMacsConfigAnswersInTime(t *testing.T) {
	l := rolesLab(t, func(c *Config) { c.AliveWindow, c.OfflineAfter = time.Hour, time.Hour })
	l.lrMac("desk-mac", "j313", nil)
	old := createBuildWait
	createBuildWait = time.Second
	t.Cleanup(func() { createBuildWait = old })
	body, _ := json.Marshal(api.BuildRequest{Source: gitRepo(t), Device: "desk-mac"})
	start := time.Now()
	w := l.signedIn(maintainerID).write("POST", "/api/builds", string(body), "Content-Type", "application/json")
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("POST /api/builds answered after %s with a 1s limit", took)
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "desk-mac's kernel config") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if all, _ := l.s.store.builds(`1=1`); len(all) != 0 {
		t.Fatalf("%d builds were queued without a config", len(all))
	}
}
