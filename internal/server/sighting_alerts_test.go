package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/oob"
)

func addSightTestMac(o *opsLab, name, hostname string, compatible []string) *dev {
	o.t.Helper()
	return addSightTestMacWithID(o, "d-"+name, name, hostname, compatible)
}

func addSightTestMacWithID(o *opsLab, id, name, hostname string, compatible []string) *dev {
	o.t.Helper()
	d := api.Device{
		ID:         id,
		Name:       name,
		State:      api.StateReady,
		LastSeen:   o.clock.Now(),
		BootID:     "boot-" + name,
		Kernel:     "7.3.0-1-ARCH",
		KnownGood:  "7.3.0-1-ARCH",
		Enrollment: api.Enrollment{Kind: api.KindLabOwned},
		Facts:      api.Facts{Hostname: hostname, Compatible: compatible, PreflightOK: true, AgentVersion: "0.12.0"},
	}
	secret := hashSecret("secret-" + id)
	if err := o.s.store.saveDevice(&d, secret); err != nil {
		o.t.Fatal(err)
	}
	v := newDev(o.s, d, secret)
	v.heard = o.clock.Now()
	o.s.mu.Lock()
	o.s.devs[name] = v
	o.s.mu.Unlock()
	return v
}

func tickOnline(o *opsLab, d time.Duration, devs ...*dev) {
	o.t.Helper()
	o.clock.Advance(d)
	now := o.clock.Now()
	for _, v := range devs {
		v.mu.Lock()
		v.d.LastSeen, v.heard = now, now
		v.d.State = api.StateReady
		v.mu.Unlock()
	}
	o.s.look(o.s.ctx)
}

func TestLostSightingAlertsAgentsAfterHoldAndClearsWhenSightedAgain(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
	})
	m1 := addSightTestMac(o, "m1-pro", "omarchy-m1-pro", []string{"apple,j314s", "apple,t6000"})
	remote := addSightTestMac(o, "remote-m3", "omarchy-m3", []string{"apple,j613", "apple,t8122"})

	fm.plug(portRight, nil, "omarchy-m1-pro", productM1Pro)
	o.s.sight(o.s.ctx)
	if _, ok := o.s.cables.of("m1-pro"); !ok {
		t.Fatal("m1-pro should be sighted")
	}

	// Never-sighted remote Mac produces no alert.
	for i := 0; i < 10; i++ {
		tickOnline(o, time.Minute, m1, remote)
	}
	o.wantTold()

	// Unplugging m1-pro drops its Sighting; after partHold (2 min), "Agents" is told once.
	fm.unplug(portRight)
	o.s.sight(o.s.ctx)
	tickOnline(o, time.Minute, m1, remote) // first look sees it unsighted (since=now)
	tickOnline(o, time.Minute, m1, remote) // 1 min < 2 min hold
	o.wantTold()
	tickOnline(o, time.Minute, m1, remote) // 2 min >= hold
	got := o.told()
	if len(got) != 1 || !strings.Contains(got[0], "m1-pro lost its Sighting") || !strings.Contains(got[0], "no Hard reset") {
		t.Fatalf("expected Sighting loss alert, got:\n%s", strings.Join(got, "\n"))
	}
	for i := 0; i < 10; i++ {
		tickOnline(o, time.Minute, m1, remote)
	}
	if len(o.told()) != 1 {
		t.Fatalf("duplicate alerts while unsighted: %v", o.told())
	}

	// Plugging m1-pro back in restores Sighting and clears the alert once.
	fm.plug(portRight, nil, "omarchy-m1-pro", productM1Pro)
	o.s.sight(o.s.ctx)
	tickOnline(o, time.Minute, m1, remote)
	got = o.told()
	if len(got) != 2 || got[1] != "m1-pro is sighted again on the right port." {
		t.Fatalf("expected Sighting recovery line, got:\n%s", strings.Join(got, "\n"))
	}
}

func TestOOBStatusHintsUniqueProductIDCandidateAndDuplicateHostname(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
	})
	addSightTestMac(o, "m2-max", "omarchy-m2-max", []string{"apple,j416c", "apple,t6021"})
	addSightTestMac(o, "m1-pro", "maralc-apple-macbookpro", []string{"apple,j314s", "apple,t6000"})
	addSightTestMac(o, "m4pro14", "maralc-apple-macbookpro", []string{"apple,j614s", "apple,t6040"})

	// left shows M2 Max product id with no Thunderbolt name; right shows the duplicate hostname.
	fm.plug(portLeft, nil, "", productM2Max)
	fm.plug(portMiddle, nil, "", 29465)
	fm.plug(portRight, nil, "maralc-apple-macbookpro", productM1Pro)
	o.s.sight(o.s.ctx)

	getOOB := func(name string) api.OOBStatus {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/devices/"+name+"/oob", nil)
		req = req.WithContext(context.WithValue(req.Context(), teamKey{}, "ada"))
		req.SetPathValue("name", name)
		rec := httptest.NewRecorder()
		o.s.hOOBStatus(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /oob %s: %d %s", name, rec.Code, rec.Body.String())
		}
		var st api.OOBStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	m2 := getOOB("m2-max")
	if m2.CandidatePort != "left" || m2.CandidateBoard != "j416c" {
		t.Fatalf("m2-max candidate = (%q, %q), want (left, j416c)", m2.CandidatePort, m2.CandidateBoard)
	}
	if len(m2.DuplicateHostname) != 0 {
		t.Fatalf("m2-max duplicate hostname = %v, want empty", m2.DuplicateHostname)
	}

	m4 := getOOB("m4pro14")
	if m4.CandidatePort != "middle" || m4.CandidateBoard != "j614s" {
		t.Fatalf("m4pro14 candidate = (%q, %q), want (middle, j614s)", m4.CandidatePort, m4.CandidateBoard)
	}
	if len(m4.DuplicateHostname) != 1 || m4.DuplicateHostname[0] != "m1-pro" {
		t.Fatalf("m4pro14 duplicate hostname = %v, want [m1-pro]", m4.DuplicateHostname)
	}

	// Adding a second unsighted j416c makes the candidate ambiguous: hint disappears.
	addSightTestMac(o, "m2-max-2", "omarchy-m2-max-2", []string{"apple,j416c", "apple,t6021"})
	m2 = getOOB("m2-max")
	if m2.CandidatePort != "" {
		t.Fatalf("ambiguous board gave candidate %q, want empty", m2.CandidatePort)
	}
}

func TestLostSightingPersistsAcrossLabdRestartBeforeAlert(t *testing.T) {
	data := t.TempDir()
	clock := agent.NewFakeClock(opsStart)
	m := healthy()
	fm := newFakeMini(t)
	mod := func(cfg *Config) { cfg.OOB = fm.client }

	// First labd sights m1-pro, then m1-pro loses its Sighting and labd
	// restarts before the 2-minute partHold alert fires.
	a := newOpsAt(t, data, clock, m, mod)
	addSightTestMac(a, "m1-pro", "omarchy-m1-pro", []string{"apple,j314s", "apple,t6000"})
	fm.plug(portRight, nil, "omarchy-m1-pro", productM1Pro)
	a.s.sight(a.s.ctx)
	if _, ok := a.s.cables.of("m1-pro"); !ok {
		t.Fatal("m1-pro should be sighted")
	}

	fm.unplug(portRight)
	a.s.sight(a.s.ctx)
	a.later(time.Minute) // still under partHold
	a.wantTold()
	a.stop()

	// Fresh labd starts on the same data directory with m1-pro still unsighted.
	clock.Advance(30 * time.Second)
	b := newOpsAt(t, data, clock, m, mod)
	b.s.sight(b.s.ctx)
	m1b := b.s.dev("m1-pro")
	tickOnline(b, time.Minute, m1b)
	tickOnline(b, time.Minute, m1b)
	got := b.told()
	if len(got) != 1 || !strings.Contains(got[0], "m1-pro lost its Sighting") {
		t.Fatalf("expected lost Sighting alert after restart, got:\n%s", strings.Join(got, "\n"))
	}

	// A brand-new enrollment (different ID) that has never been sighted does not inherit history.
	brandNew := addSightTestMacWithID(b, "d-brand-new", "remote-mac", "omarchy-remote", []string{"apple,j613", "apple,t8122"})
	for i := 0; i < 5; i++ {
		tickOnline(b, time.Minute, m1b, brandNew)
	}
	if len(b.told()) != 1 {
		t.Fatalf("unexpected alert for never-sighted enrollment: %v", b.told())
	}
}

func TestCandidatePortKeepsConflictingProductIDRejected(t *testing.T) {
	fm := newFakeMini(t)
	c := newCables(oob.NewFleet(fm.client))
	// Three sighted Macs share one unknown product id across two different
	// boards (two j999a, one j999b). Regardless of map iteration order, the
	// conflicting product id must remain permanently rejected.
	const sharedPID = 39999
	c.held["mac-a"] = sighting{rid: portLeft, seen: oob.Seen{ProductID: sharedPID, PlugCount: 1}}
	c.held["mac-b"] = sighting{rid: portMiddle, seen: oob.Seen{ProductID: sharedPID, PlugCount: 1}}
	c.held["mac-c"] = sighting{rid: 99, seen: oob.Seen{ProductID: sharedPID, PlugCount: 1}}
	c.ports[portRight] = oob.Port{RID: portRight, Connected: true, ProductID: sharedPID, PlugCount: 1}
	macs := []labOwned{
		{name: "mac-a", board: "j999a"},
		{name: "mac-b", board: "j999b"},
		{name: "mac-c", board: "j999a"},
		{name: "target-a", board: "j999a"},
		{name: "target-b", board: "j999b"},
	}
	for i := 0; i < 50; i++ {
		if port, board := c.candidatePortLocked("target-a", "j999a", macs); port != "" || board != "" {
			t.Fatalf("iteration %d: conflicting product id returned (%q, %q) for target-a, want empty", i, port, board)
		}
		if port, board := c.candidatePortLocked("target-b", "j999b", macs); port != "" || board != "" {
			t.Fatalf("iteration %d: conflicting product id returned (%q, %q) for target-b, want empty", i, port, board)
		}
	}
}
