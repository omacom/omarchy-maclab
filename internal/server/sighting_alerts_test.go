package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func addSightTestMac(o *opsLab, name, hostname string, compatible []string) *dev {
	o.t.Helper()
	d := api.Device{
		ID:         "d-" + name,
		Name:       name,
		State:      api.StateReady,
		LastSeen:   o.clock.Now(),
		BootID:     "boot-" + name,
		Kernel:     "7.3.0-1-ARCH",
		KnownGood:  "7.3.0-1-ARCH",
		Enrollment: api.Enrollment{Kind: api.KindLabOwned},
		Facts:      api.Facts{Hostname: hostname, Compatible: compatible, PreflightOK: true, AgentVersion: "0.12.0"},
	}
	v := newDev(o.s, d, "secret-"+name)
	v.heard = o.clock.Now()
	o.s.mu.Lock()
	o.s.devs[name] = v
	o.s.mu.Unlock()
	return v
}

func TestLostSightingAlertsAgentsAfterHoldAndClearsWhenSightedAgain(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
	})
	addSightTestMac(o, "m1-pro", "omarchy-m1-pro", []string{"apple,j314s", "apple,t6000"})
	addSightTestMac(o, "remote-m3", "omarchy-m3", []string{"apple,j613", "apple,t8122"})

	fm.plug(portRight, nil, "omarchy-m1-pro", productM1Pro)
	o.s.sight(o.s.ctx)
	if _, ok := o.s.cables.of("m1-pro"); !ok {
		t.Fatal("m1-pro should be sighted")
	}

	// Never-sighted remote Mac produces no alert.
	o.minutes(10)
	o.wantTold()

	// Unplugging m1-pro drops its Sighting; after partHold (2 min), "Agents" is told once.
	fm.unplug(portRight)
	o.s.sight(o.s.ctx)
	o.later(time.Minute) // first look sees it unsighted (since=now)
	o.later(time.Minute) // 1 min < 2 min hold
	o.wantTold()
	o.later(time.Minute) // 2 min >= hold
	got := o.told()
	if len(got) != 1 || !strings.Contains(got[0], "m1-pro lost its Sighting") || !strings.Contains(got[0], "no Hard reset") {
		t.Fatalf("expected Sighting loss alert, got:\n%s", strings.Join(got, "\n"))
	}
	o.minutes(10)
	if len(o.told()) != 1 {
		t.Fatalf("duplicate alerts while unsighted: %v", o.told())
	}

	// Plugging m1-pro back in restores Sighting and clears the alert once.
	fm.plug(portRight, nil, "omarchy-m1-pro", productM1Pro)
	o.s.sight(o.s.ctx)
	o.later(time.Minute)
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
