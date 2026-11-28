package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
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
	secret := hashSecret("secret-" + name)
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

func TestIdleOfflineCooldownPersistsAcrossRecoveryAndRestart(t *testing.T) {
	data := t.TempDir()
	clock := agent.NewFakeClock(opsStart)
	m := healthy()
	fm := newFakeMini(t)
	mod := func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	}

	a := newOpsAt(t, data, clock, m, mod)
	v := addIdleTestMac(a, "m2-max", api.KindLabOwned, api.StateReady, "")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	a.s.sight(a.s.ctx)

	// First offline event -> automatic Hard reset #1 -> recovers.
	clock.Advance(time.Minute)
	v.mu.Lock()
	v.setStateLocked(api.StateOffline, "no heartbeat")
	v.mu.Unlock()
	a.s.look(a.s.ctx)
	a.later(idleOfflineHold)
	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.resets == 1 && v.snapshot().State == api.StateRecovering
	})
	clock.Advance(30 * time.Second)
	v.heartbeat(api.Checkin{
		BootID: "boot-m2-max-2",
		Kernel: "7.3.0-1-ARCH",
		Health: api.Health{SystemState: "running"},
	})
	waitUntil(t, 2*time.Second, func() bool {
		return v.snapshot().State == api.StateReady
	})

	// Goes offline again 10 min later for 10 min (still well inside 1h cooldown):
	// recovery must NOT have erased the hourly reset limit.
	a.later(10 * time.Minute)
	v.mu.Lock()
	v.setStateLocked(api.StateOffline, "no heartbeat again")
	v.mu.Unlock()
	a.s.look(a.s.ctx)
	a.later(idleOfflineHold)
	fm.mu.Lock()
	resetsAfterRecovery := fm.resets
	fm.mu.Unlock()
	if resetsAfterRecovery != 1 {
		t.Fatalf("resets after recovery within 1h = %d, want 1", resetsAfterRecovery)
	}

	// Restart labd on the same data directory while still inside the 1h cooldown:
	// restart must NOT erase the hourly reset limit either.
	v.mu.Lock()
	if err := a.s.store.saveDevice(&v.d, v.secretHash); err != nil {
		v.mu.Unlock()
		t.Fatal(err)
	}
	v.mu.Unlock()
	a.stop()

	clock.Advance(5 * time.Minute)
	b := newOpsAt(t, data, clock, m, mod)
	b.s.sight(b.s.ctx)
	vb := b.s.dev("m2-max")
	if vb == nil {
		t.Fatal("m2-max missing after restart")
	}
	vb.mu.Lock()
	vb.setStateLocked(api.StateOffline, "still offline after restart")
	vb.mu.Unlock()
	b.s.look(b.s.ctx)
	b.later(idleOfflineHold)
	fm.mu.Lock()
	resetsAfterRestart := fm.resets
	fm.mu.Unlock()
	if resetsAfterRestart != 1 {
		t.Fatalf("resets after restart within 1h = %d, want 1", resetsAfterRestart)
	}

	// Once the full 1h cooldown has elapsed, a second automatic reset is allowed.
	b.later(40 * time.Minute)
	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.resets == 2
	})
}

func TestIdleOfflineQueuedResetCanceledByNewLease(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})
	v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "no heartbeat")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)

	// Hold the controller lock so the automatic idle reset has to wait in queue.
	probing, release := fm.holdProbes(t)
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		_, _ = fm.client.Probe(context.Background(), portLeft)
	}()
	<-probing

	o.s.look(o.s.ctx)
	o.later(idleOfflineHold)

	// Wait until idleRecover has claimed resetting and reached the controller queue.
	waitUntil(t, 2*time.Second, func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return v.resetting
	})
	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.resetsAsked == 1
	})

	// Take a Lease while the reset is waiting for the controller.
	v.mu.Lock()
	v.d.Lease = &api.Lease{Holder: "ada@desk", Maintainer: "ada", Expires: o.clock.Now().Add(2 * time.Hour)}
	v.bump()
	v.mu.Unlock()

	// Wait for idleRecover to abort before releasing the controller, then release.
	waitUntil(t, 2*time.Second, func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return !v.resetting
	})
	release()
	<-probeDone

	fm.mu.Lock()
	resets := fm.resets
	fm.mu.Unlock()
	if resets != 0 {
		t.Fatalf("sent %d resets after Lease was taken, want 0", resets)
	}
	if st := v.snapshot().State; st != api.StateOffline {
		t.Fatalf("state after canceled queued reset = %s, want offline (must not enter recovering)", st)
	}
	// Advance well past BootTimeout: no delayed failure alert and cooldown budget was restored.
	o.clock.Advance(5 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if len(o.told()) != 0 {
		t.Fatalf("unexpected alerts past BootTimeout when Lease canceled queued reset: %v", o.told())
	}
	if last, _ := o.s.lastIdleReset(v.snapshot().ID); !last.IsZero() {
		t.Fatalf("canceled queued reset consumed cooldown budget: %v", last)
	}
}

func TestIdleOfflineQueuedResetCanceledWhenMacReturnsOnline(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})
	v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "no heartbeat")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)

	probing, release := fm.holdProbes(t)
	probeDone := make(chan struct{})
	go func() {
		defer close(probeDone)
		_, _ = fm.client.Probe(context.Background(), portLeft)
	}()
	<-probing

	o.s.look(o.s.ctx)
	o.later(idleOfflineHold)

	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		defer fm.mu.Unlock()
		return fm.resetsAsked == 1
	})

	// The Mac wakes up on its own while the automatic reset waits in queue.
	o.clock.Advance(time.Second)
	v.heartbeat(api.Checkin{
		BootID: "boot-m2-max-1",
		Kernel: "7.3.0-1-ARCH",
		Health: api.Health{SystemState: "running"},
	})

	waitUntil(t, 2*time.Second, func() bool {
		v.mu.Lock()
		defer v.mu.Unlock()
		return !v.resetting
	})
	release()
	<-probeDone

	fm.mu.Lock()
	resets := fm.resets
	fm.mu.Unlock()
	if resets != 0 {
		t.Fatalf("sent %d resets after Mac returned online, want 0", resets)
	}
	if st := v.snapshot().State; st != api.StateReady {
		t.Fatalf("state after Mac returned online = %s, want ready", st)
	}
	// Advance past BootTimeout: the Mac stays ready and no failure alert fires.
	o.clock.Advance(3 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if len(o.told()) != 0 {
		t.Fatalf("unexpected alerts after queued reset was canceled by return: %v", o.told())
	}
}

func TestIdleOfflineOnSendingAndHeartbeatDuringResetKeepRecovering(t *testing.T) {
	fm := newFakeMini(t)
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})
	v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "no heartbeat")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)

	// 1) Right before OnSending fires (in onLook), update the Mac's BootID to
	// boot-m2-max-mid while still offline so OnSending must capture boot-mid.
	// 2) Give the tool 150ms (toolTime) and, as soon as the controller begins
	// executing Reboot (after 102 / OnSending), send a heartbeat moving the Mac
	// to StateReady — racing with fenceIdleWhileWaiting. Because OnSending has
	// already fired, idleRecover must still hold StateRecovering and must NOT
	// count boot-m2-max-mid as post-reset recovery.
	var once sync.Once
	fm.mu.Lock()
	fm.toolTime = 150 * time.Millisecond
	fm.onLook = func(int) {
		once.Do(func() {
			o.clock.Advance(2 * time.Second)
			v.mu.Lock()
			v.d.BootID = "boot-m2-max-mid"
			v.d.LastSeen = o.clock.Now()
			v.mu.Unlock()
			go func() {
				waitUntil(t, 2*time.Second, func() bool {
					fm.mu.Lock()
					defer fm.mu.Unlock()
					return len(fm.began) == 1
				})
				o.clock.Advance(time.Second)
				v.heartbeat(api.Checkin{
					BootID: "boot-m2-max-mid",
					Kernel: "7.3.0-1-ARCH",
					Health: api.Health{SystemState: "running"},
				})
			}()
		})
	}
	fm.mu.Unlock()

	o.s.look(o.s.ctx)
	o.later(idleOfflineHold)

	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		resets := fm.resets
		fm.mu.Unlock()
		return resets == 1 && v.snapshot().State == api.StateRecovering
	})
	// boot-m2-max-mid must NOT have counted as post-reset recovery.
	if len(o.told()) != 0 {
		t.Fatalf("earlier boot counted as recovery: %v", o.told())
	}

	// Only a newer boot after OnSending completes recovery.
	o.clock.Advance(30 * time.Second)
	v.heartbeat(api.Checkin{
		BootID: "boot-m2-max-after",
		Kernel: "7.3.0-1-ARCH",
		Health: api.Health{SystemState: "running"},
	})
	waitUntil(t, 2*time.Second, func() bool {
		return v.snapshot().State == api.StateReady && len(o.told()) == 1
	})
}

func TestIdleOfflineMayHaveGoneKeepsRecoveringUntilConfirmed(t *testing.T) {
	fm := newFakeMini(t)
	fm.mu.Lock()
	fm.rebootFails = errors.New("exit status 1: lost the port partner")
	fm.mu.Unlock()
	o := newOps(t, func(cfg *Config) {
		cfg.OOB = fm.client
		cfg.BootTimeout = 2 * time.Minute
	})
	v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "no heartbeat")
	fm.plug(portLeft, nil, "m2-max", productM2Max)
	o.s.sight(o.s.ctx)

	o.s.look(o.s.ctx)
	o.later(idleOfflineHold)

	// Even though Reboot returned an error after sending (mayHaveGone), the
	// Mac must stay in StateRecovering instead of alerting failure immediately.
	waitUntil(t, 2*time.Second, func() bool {
		fm.mu.Lock()
		resets := fm.resets
		fm.mu.Unlock()
		return resets == 1 && v.snapshot().State == api.StateRecovering
	})
	if len(o.told()) != 0 {
		t.Fatalf("mayHaveGone alerted prematurely: %v", o.told())
	}

	// Once it comes back on a new boot, it leaves StateRecovering and reports recovery.
	o.clock.Advance(30 * time.Second)
	v.heartbeat(api.Checkin{
		BootID: "boot-m2-max-2",
		Kernel: "7.3.0-1-ARCH",
		Health: api.Health{SystemState: "running"},
	})
	waitUntil(t, 2*time.Second, func() bool {
		return v.snapshot().State == api.StateReady && len(o.told()) == 1
	})
}

func TestIdleOfflineCanceledWaitDoesNotReportFailure(t *testing.T) {
	t.Run("server shutdown", func(t *testing.T) {
		fm := newFakeMini(t)
		o := newOps(t, func(cfg *Config) {
			cfg.OOB = fm.client
			cfg.BootTimeout = 2 * time.Minute
		})
		v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "no heartbeat")
		fm.plug(portLeft, nil, "m2-max", productM2Max)
		o.s.sight(o.s.ctx)

		o.s.look(o.s.ctx)
		o.later(idleOfflineHold)
		waitUntil(t, 2*time.Second, func() bool {
			return v.snapshot().State == api.StateRecovering
		})

		// Shut down labd while idleRecover is waiting for boot.
		o.stop()
		// Re-open the store to inspect recorded Agents lines after cancellation settles.
		time.Sleep(50 * time.Millisecond)
		if got := o.told(); len(got) != 0 {
			t.Fatalf("server shutdown reported failure: %v", got)
		}
	})

	t.Run("mac enrollment ended", func(t *testing.T) {
		fm := newFakeMini(t)
		o := newOps(t, func(cfg *Config) {
			cfg.OOB = fm.client
			cfg.BootTimeout = 2 * time.Minute
		})
		v := addIdleTestMac(o, "m2-max", api.KindLabOwned, api.StateOffline, "no heartbeat")
		fm.plug(portLeft, nil, "m2-max", productM2Max)
		o.s.sight(o.s.ctx)

		o.s.look(o.s.ctx)
		o.later(idleOfflineHold)
		waitUntil(t, 2*time.Second, func() bool {
			return v.snapshot().State == api.StateRecovering
		})

		// End the Mac's enrollment while idleRecover waits for boot.
		if err := o.s.endEnrollment(v, leaving{how: leftByMaintainer, by: Person{Handle: "ada", Maintainer: true}}); err != nil {
			t.Fatal(err)
		}
		o.clock.Advance(3 * time.Minute)
		time.Sleep(50 * time.Millisecond)
		if got := o.told(); len(got) != 0 {
			t.Fatalf("ended enrollment reported failure: %v", got)
		}
	})
}
