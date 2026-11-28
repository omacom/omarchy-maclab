package server

import (
	"strings"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func addIdleTestMac(o *opsLab, name, kind string, state api.DeviceState, reason string) *dev {
	o.t.Helper()
	d := api.Device{
		ID:          "d-" + name,
		Name:        name,
		State:       state,
		StateReason: reason,
		LastSeen:    o.clock.Now(),
		BootID:      "boot-" + name + "-1",
		Kernel:      "7.3.0-1-ARCH",
		KnownGood:   "7.3.0-1-ARCH",
		Enrollment:  api.Enrollment{Kind: kind},
		Volunteer:   kind == api.KindVolunteer,
		Facts:       api.Facts{Hostname: name, PreflightOK: true, AgentVersion: "0.12.0"},
	}
	v := newDev(o.s, d, "secret-"+name)
	v.heard = o.clock.Now()
	o.s.mu.Lock()
	o.s.devs[name] = v
	o.s.mu.Unlock()
	return v
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not hold before timeout")
}

func TestIdleOfflineSightedMacGetsOneHardResetAndRecovers(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})
	v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateReady, "")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)
	if _, ok := o.s.cables.of("m2-max"); !ok {
		t.Fatal("m2-max not sighted on left")
	}

	// Mac goes offline at +1 min.
	o.clock.Advance(time.Minute)
	v.mu.Lock()
	v.setStateLocked(api.StateOffline, "asleep since 14:01:00; a key press wakes it")
	v.mu.Unlock()

	// Under idleOfflineHold (10 min): no reset and no alert yet.
	o.s.look(o.s.ctx) // marks since
	o.minutes(9)
	if got := fm.resets; got != 0 {
		t.Fatalf("sent %d resets before hold, want 0", got)
	}
	o.wantTold()

	// At 10 min offline: sends one automatic Hard reset.
	o.later(time.Minute)
	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.resets == 1 && v.snapshot().State == api.StateRecovering
	})

	// Simulate the Mac checking back in on a new boot.
	o.clock.Advance(30 * time.Second)
	v.heartbeat(api.Checkin{
		BootID: "boot-m2-max-2",
		Kernel: "7.3.0-1-ARCH",
		Health: api.Health{SystemState: "running"},
	})
	waitUntil(t, 2*time.Second, func() bool {
		return v.snapshot().State == api.StateReady && len(o.told()) == 1
	})
	lines := o.told()
	if !strings.Contains(lines[0], "m2-max was offline since ") || !strings.Contains(lines[0], "came back after an automatic Hard reset through the left port.") {
		t.Fatalf("unexpected Agents line: %q", lines[0])
	}
}

func TestIdleOfflineFailedHardResetAlertsOnceAndRespectsCooldown(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})
	v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateReady, "")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)

	v.mu.Lock()
	v.setStateLocked(api.StateOffline, "no heartbeat since 14:00:00")
	v.mu.Unlock()
	o.s.look(o.s.ctx)
	o.later(idleOfflineHold)

	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.resets == 1 && v.snapshot().State == api.StateRecovering
	})

	// Advance past BootTimeout without check-in: reset times out and alerts once.
	o.clock.Advance(3 * time.Minute)
	waitUntil(t, 2*time.Second, func() bool {
		return v.snapshot().State == api.StateOffline && len(o.told()) == 1
	})
	if !strings.Contains(o.told()[0], "one automatic Hard reset through the left port did not bring it back.") {
		t.Fatalf("unexpected alert: %q", o.told()[0])
	}

	// Within the 1-hour cooldown, no second Hard reset goes out and no duplicate alert.
	o.minutes(30)
	fm.mu.Lock()
	resets := fm.resets
	fm.mu.Unlock()
	if resets != 1 {
		t.Fatalf("resets during cooldown = %d, want 1", resets)
	}
	if len(o.told()) != 1 {
		t.Fatalf("alerts during cooldown = %d, want 1", len(o.told()))
	}
}

func TestIdleOfflineGuardsUnsightedLeasedBatteryAndVolunteer(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})

	unsighted := addIdleTestMac(o, "m4pro", api.KindLabOwned, api.StateOffline, "no heartbeat since 14:00:00")
	leased := addIdleTestMac(o, "m1-pro", api.KindLabOwned, api.StateOffline, "no heartbeat since 14:00:00")
	onBat := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "asleep since 14:00:00")
	vol := addIdleTestMac(o, "vol-air", api.KindVolunteer, api.StateOffline, "no heartbeat since 14:00:00")

	fm.plug(portRight, nil, "m1-pro", productM1Pro)
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)

	leased.mu.Lock()
	leased.d.Lease = &api.Lease{Holder: "ada@desk", Maintainer: "ada", Expires: o.clock.Now().Add(4 * time.Hour)}
	leased.mu.Unlock()

	onBat.mu.Lock()
	onBat.d.Health.OnBattery = true
	onBat.mu.Unlock()

	_ = vol
	o.s.look(o.s.ctx)
	o.later(idleOfflineHold)

	// Zero Hard resets: m4pro is unsighted, m1-pro is leased, m2-max was on battery, vol-air is a Volunteer's Mac.
	fm.mu.Lock()
	resets := fm.resets
	fm.mu.Unlock()
	if resets != 0 {
		t.Fatalf("sent %d resets, want 0", resets)
	}

	// Only unsighted m4pro and on-battery m2-max should alert "Agents" (once each).
	got := o.told()
	if len(got) != 2 {
		t.Fatalf("told %d lines, want 2:\n%s", len(got), strings.Join(got, "\n"))
	}
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "m4pro has been offline") || !strings.Contains(joined, "no automatic Hard reset was sent") {
		t.Errorf("missing m4pro alert:\n%s", joined)
	}
	if !strings.Contains(joined, "m2-max has been offline") || !strings.Contains(joined, "last seen on battery") {
		t.Errorf("missing m2-max battery guard alert:\n%s", joined)
	}
	if strings.Contains(joined, "m1-pro") || strings.Contains(joined, "vol-air") {
		t.Errorf("unexpected alert for leased or Volunteer's Mac:\n%s", joined)
	}

	// Returning online clears the unsighted alert once.
	o.clock.Advance(time.Minute)
	unsighted.heartbeat(api.Checkin{
		BootID: "boot-m4pro-2",
		Kernel: "7.3.0-1-ARCH",
		Health: api.Health{SystemState: "running"},
	})
	o.s.look(o.s.ctx)
	got = o.told()
	if len(got) != 3 || !strings.Contains(got[2], "m4pro is back online") {
		t.Fatalf("expected recovery line for m4pro, got:\n%s", strings.Join(got, "\n"))
	}
}
