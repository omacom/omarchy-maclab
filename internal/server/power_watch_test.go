package server

import (
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
)

func addWatchedDevice(o *opsLab, name, kind string) *dev {
	o.t.Helper()
	d := api.Device{
		ID:         "d-" + name,
		Name:       name,
		State:      api.StateReady,
		LastSeen:   o.clock.Now(),
		BootID:     "boot-" + name,
		Kernel:     "7.3.0-1-ARCH",
		KnownGood:  "7.3.0-1-ARCH",
		Enrollment: api.Enrollment{Kind: kind},
		Volunteer:  kind == api.KindVolunteer,
		Facts:      api.Facts{Hostname: name, PreflightOK: true, AgentVersion: "0.12.0"},
	}
	v := newDev(o.s, d, "secret-"+name)
	v.heard = o.clock.Now()
	o.s.mu.Lock()
	o.s.devs[name] = v
	o.s.mu.Unlock()
	return v
}

func setDeviceHealth(o *opsLab, v *dev, h api.Health, alive bool) {
	v.mu.Lock()
	v.d.Health = h
	if alive {
		now := o.clock.Now()
		v.d.LastSeen, v.heard = now, now
		v.d.State = api.StateReady
	} else {
		old := o.clock.Now().Add(-10 * time.Minute)
		v.d.LastSeen, v.heard = old, old
		v.d.State = api.StateOffline
	}
	v.mu.Unlock()
}

func TestUnderpoweredDrainingLabMacAlertsAgentsAndStaysLatchedWhileOffline(t *testing.T) {
	o := newOps(t, nil)
	v := addWatchedDevice(o, "m2-max", api.KindLabOwned)
	pct5 := 5
	drainUW := int64(-2122000)
	pct88 := 88
	chargeUW := int64(42654000)

	healthyAC := api.Health{
		SystemState: "running",
		Power: &api.Power{
			BatteryPercent:    &pct88,
			BatteryPowerUW:    &chargeUW,
			InputPowerLimitUW: 139720000,
		},
	}
	draining := api.Health{
		SystemState: "running",
		Power: &api.Power{
			BatteryPercent:    &pct5,
			BatteryPowerUW:    &drainUW,
			InputPowerLimitUW: 7500000,
			Draining:          true,
		},
	}

	setDeviceHealth(o, v, healthyAC, true)
	o.minutes(10)
	o.wantTold()

	// Draining for 4 minutes is under powerHold (5 minutes): no alert yet.
	for i := 0; i < 4; i++ {
		o.clock.Advance(time.Minute)
		setDeviceHealth(o, v, draining, true)
		o.s.look(o.s.ctx)
	}
	o.wantTold()

	// Brief recovery resets the hold window.
	o.clock.Advance(time.Minute)
	setDeviceHealth(o, v, healthyAC, true)
	o.s.look(o.s.ctx)
	o.wantTold()

	// Sustained draining past 5 minutes tells "Agents" once.
	for i := 0; i < 6; i++ {
		o.clock.Advance(time.Minute)
		setDeviceHealth(o, v, draining, true)
		o.s.look(o.s.ctx)
	}
	trouble := "m2-max is losing charge on underpowered AC (5%, -2.1 W, 7.5 W limit): plug in its charger."
	o.wantTold(trouble)

	// Going offline (e.g. upowerd suspend) must NOT clear the power alert.
	setDeviceHealth(o, v, draining, false)
	o.minutes(15)
	o.wantTold(trouble)

	// Checking back in on adequate AC clears the alert once.
	o.clock.Advance(time.Minute)
	setDeviceHealth(o, v, healthyAC, true)
	o.s.look(o.s.ctx)
	recovered := "m2-max is on AC power again: AC (140 W limit), battery 88%, +42.7 W."
	o.wantTold(trouble, recovered)

	o.clock.Advance(time.Minute)
	setDeviceHealth(o, v, healthyAC, true)
	o.s.look(o.s.ctx)
	o.wantTold(trouble, recovered)
}

func TestVolunteersMacPowerNeverAlertsAgentsAndDrainingSkipsPerf(t *testing.T) {
	o := newOps(t, nil)
	vol := addWatchedDevice(o, "vol-air", api.KindVolunteer)
	labMac := addWatchedDevice(o, "m2-max", api.KindLabOwned)
	pct10 := 10
	drainUW := int64(-3000000)
	draining := api.Health{
		SystemState: "running",
		Power: &api.Power{
			BatteryPercent:    &pct10,
			BatteryPowerUW:    &drainUW,
			InputPowerLimitUW: 7500000,
			Draining:          true,
		},
	}

	// Volunteer's Mac on battery never alerts "Agents".
	for i := 0; i < 10; i++ {
		o.clock.Advance(time.Minute)
		setDeviceHealth(o, vol, api.Health{OnBattery: true}, true)
		o.s.look(o.s.ctx)
	}
	o.wantTold()

	// Performance scheduler refuses a Lab-owned Mac draining on underpowered AC.
	setDeviceHealth(o, labMac, draining, true)
	labMac.mu.Lock()
	why := labMac.perfIdleLocked(5 * time.Minute)
	labMac.mu.Unlock()
	if why != "it is losing charge on underpowered AC" {
		t.Fatalf("perfIdleLocked = %q, want draining refusal", why)
	}
}
