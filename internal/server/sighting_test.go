package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/omacom/omarchy-maclab/internal/agent"
	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/oob"
)

// A name read on a port that belongs to exactly one Lab-owned Mac's record
// makes a Sighting: the Mac, the port, the product id, the plug count.
func TestANameOnAPortOfExactlyOneLabOwnedMacMakesASighting(t *testing.T) {
	l := sightLab(t, labConfig{})
	sg, ok := l.s.cables.of(l.name)
	if want := (oob.Seen{ProductID: productM1Pro, PlugCount: 41}); !ok || sg.rid != portRight || sg.seen != want || sg.proved {
		t.Fatalf("the Sighting of %s: %+v %v, want the right port with %+v", l.name, sg, ok, want)
	}
	l.wantCable(l.name, api.CableSighted)
	if c := l.cableOf(l.name); c["port"] != "right" || c["proved"] != nil {
		t.Fatalf("the cable as shown: %v", c)
	}
	if tier := l.deviceJSON(l.name)["tier"]; tier != float64(2) {
		t.Fatalf("a sighted Mac's recovery tier: %v, want 2", tier)
	}

	// A name that is in no Lab-owned Mac's record places nothing.
	l.oob.plug(portLeft, nil, "somebody-elses-mac", productM2Max)
	l.sight()
	l.s.cables.mu.Lock()
	held := len(l.s.cables.held)
	l.s.cables.mu.Unlock()
	if held != 1 {
		t.Fatalf("%d Sightings, want the one", held)
	}
	l.oob.unplug(portLeft)

	// The name is the one the Mac itself reported as its hostname, too.
	l.waitDevice(func(d api.Device) bool { return d.Facts.Hostname == "sim" })
	l.oob.plug(portRight, l.sim, "SIM", productM1Pro)
	l.sight()
	if sg, ok := l.s.cables.of(l.name); !ok || sg.rid != portRight || sg.seen.PlugCount != 42 {
		t.Fatalf("sighted by its hostname: %+v %v", sg, ok)
	}

	// The same name in two records belongs to neither.
	l.addMac("m2-max")
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool {
		facts, _ := m["facts"].(map[string]any)
		return facts["hostname"] == "sim"
	})
	l.sight()
	if _, ok := l.s.cables.of(l.name); ok {
		t.Fatal("a name two Lab-owned Macs' records carry made a Sighting")
	}
	l.wantCable(l.name, api.CableNotSighted, "belongs to 2 Lab-owned Macs' records")
	l.wantCable("m2-max", api.CableNotSighted, "belongs to 2 Lab-owned Macs' records")
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusConflict || !strings.Contains(body, "no Hard reset was sent") {
		t.Fatalf("a Hard reset with no Sighting: %s %s", r.Status, body)
	}
	l.wantResets("nothing was sighted for sure")
}

// A Hard reset goes out only if the port is still connected with the same
// product id and plug count; otherwise the Sighting is dropped and the reset
// refused with the reason. The controller compares once more, under its
// lock.
func TestAHardResetGoesOutOnlyWhileThePortIsAsItWasSighted(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	// The look labd takes every 15 s: it knows the boot the baseline left.
	l.sight()
	refused := func(why, state string, saying ...string) {
		t.Helper()
		r, body := l.resetAs(harnessToken, l.name, "")
		if r.StatusCode != http.StatusConflict {
			t.Fatalf("%s: %s %s", why, r.Status, body)
		}
		for _, piece := range append(saying, "no Hard reset was sent", "The Sighting of "+l.name+" is dropped") {
			if !strings.Contains(body, piece) {
				t.Fatalf("%s: the refusal %q lacks %q", why, body, piece)
			}
		}
		l.wantResets(why)
		if _, ok := l.s.cables.of(l.name); ok {
			t.Fatalf("%s: the Sighting was kept", why)
		}
		l.wantCable(l.name, state, "its Sighting was dropped")
	}
	sighted := func() {
		t.Helper()
		l.sight()
		if _, ok := l.s.cables.of(l.name); !ok {
			t.Fatal("not sighted again")
		}
	}

	// Its cable pulled and plugged back in with the Mac up: no reset until
	// the lab can tell that from a restart, and the name it shows sights it
	// again.
	l.oob.plug(portRight, l.sim, l.name, productM1Pro)
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusConflict ||
		!strings.Contains(body, "the right port has plug count 42, not the 41 it was seen with, and "+l.name+" is up on the boot it had") {
		t.Fatalf("the plug count moved: %s %s", r.Status, body)
	}
	l.wantResets("the plug count moved")
	sighted()

	// Another model at the end of the cable.
	l.oob.swap(portRight, productM2Max)
	refused("the product id changed", api.CableNotSighted, "the right port shows product id 29453, not the 29448 it was seen with")
	l.oob.swap(portRight, productM1Pro)
	sighted()

	// Nothing on the port.
	l.oob.unplug(portRight)
	refused("the port is not connected", api.CableNone, "the right port is no longer connected")
	l.oob.plug(portRight, l.sim, l.name, productM1Pro)
	sighted()

	// The cable moves after labd looked: the controller's own look, under
	// its lock, is the last word.
	var once sync.Once
	l.oob.onRead(func(rid int) {
		if rid == portRight {
			once.Do(func() { l.oob.plug(portRight, l.sim, l.name, productM1Pro) })
		}
	})
	refused("the controller saw the port change", api.CableNotSighted, "the port with RID 2 has plug count 44, not the 43 it was seen with: no reset was sent")
	l.oob.onRead(nil)
	if log := l.oob.log.String(); !strings.Contains(log, "RID 2 reset refused") {
		t.Fatalf("the controller did not refuse it itself:\n%s", log)
	}
	sighted()

	// As it was sighted: the reset goes out, to that port.
	boot := l.bootOf(l.name)
	r, body := l.resetAs(harnessToken, l.name, "")
	if r.StatusCode != http.StatusOK || !strings.Contains(body, "Hard reset sent to "+l.name+" through the right port") {
		t.Fatalf("a Hard reset of a sighted Mac: %s %s", r.Status, body)
	}
	l.wantResets("the port was as it was sighted", portRight)
	l.waitDevice(func(d api.Device) bool { return d.BootID != boot && d.State == api.StateReady })
}

// Any conflict drops the Sightings involved.
func TestAnyConflictDropsTheSightingsInvolved(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	other := l.addMac("m2-max")
	l.oob.plug(portLeft, other, "m2-max", productM2Max)
	l.sight()
	l.wantCable(l.name, api.CableSighted)
	l.wantCable("m2-max", api.CableSighted)

	// One name on two ports.
	l.oob.plug(portMiddle, nil, "m2-max", productM2Max)
	l.sight()
	l.wantCable("m2-max", api.CableNotSighted, "its name is on left and middle ports at once")
	l.wantCable(l.name, api.CableSighted)
	if r, body := l.resetAs(harnessToken, "m2-max", ""); r.StatusCode != http.StatusConflict || !strings.Contains(body, "left and middle ports at once") {
		t.Fatalf("a Hard reset of a Mac whose name is on two ports: %s %s", r.Status, body)
	}
	l.wantResets("its name was on two ports")
	l.oob.unplug(portMiddle)
	l.sight()
	l.wantCable("m2-max", api.CableSighted)

	// A remembered Mac's name appearing on another port.
	l.oob.shows(portLeft, "")
	l.sight()
	if sg, ok := l.s.cables.of("m2-max"); !ok || sg.rid != portLeft {
		t.Fatalf("a Mac that stopped showing its name on an unchanged port: %+v %v", sg, ok)
	}
	l.oob.plug(portMiddle, nil, "m2-max", productM2Max)
	l.sight()
	if _, ok := l.s.cables.of("m2-max"); ok {
		t.Fatal("its name appeared on another port and a Sighting was kept or made")
	}
	l.wantCable("m2-max", api.CableNotSighted, "its name is on the middle port, and it was sighted on the left port")
	l.oob.unplug(portMiddle)
	l.sight()

	// Another Mac's name on the port a Mac was sighted on.
	l.oob.shows(portRight, "m2-max")
	l.sight()
	if _, ok := l.s.cables.of(l.name); ok {
		t.Fatalf("%s kept the port that shows another Mac's name", l.name)
	}
	if _, ok := l.s.cables.of("m2-max"); ok {
		t.Fatal("a Sighting was made in the look that found the conflict")
	}
	l.wantCable(l.name, api.CableNotSighted, "the right port shows the name of m2-max")
	l.wantResets("conflicts")
}

// After a restart labd holds no Sighting until it has looked at the ports,
// with the controller starting after it, as after an Unlock; then it keeps
// the one its data held, on a port that did not change.
func TestAfterARestartLabdHoldsNoSightingUntilItHasLookedAtThePorts(t *testing.T) {
	l := newLabWith(t, labConfig{oob: true})
	l.baseline()
	if _, ok := l.s.cables.of(l.name); !ok {
		t.Fatal("the Mac is not sighted before the restart")
	}
	// The controller starts after labd, as after an Unlock.
	l.oob.stop(true)
	cfg := l.s.cfg
	cfg.SightEvery = 50 * time.Millisecond
	fresh, err := New(l.ctx, cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, ok := fresh.cables.of(l.name); ok {
		t.Fatal("labd started with a Sighting")
	}
	l.oob.stop(false)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if sg, ok := fresh.cables.of(l.name); ok {
			if sg.rid != portRight || sg.seen != (oob.Seen{ProductID: productM1Pro, PlugCount: 41}) {
				t.Fatalf("the Sighting it rebuilt: %+v", sg)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("labd did not sight the Mac again")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The lab never Hard resets a Volunteer's Mac: not for a Maintainer, not
// for the holder of a Lease on it, not as a Proving reset, and not from the
// recovery ladder, even with the Mac on one of the mini's cables under its
// own name.
func TestAVolunteersMacNeverGetsAHardReset(t *testing.T) {
	l := sightLab(t, labConfig{volunteer: true})
	l.openForTheLab(time.Now())
	l.baseline()
	const never = "the lab never sends a Volunteer's Mac a Hard reset"
	if port := l.oob.look(portRight); !port.Connected || port.Name != l.name {
		t.Fatalf("the Volunteer's Mac is not on the right port under its name: %+v", port)
	}
	l.sight()
	if _, ok := l.s.cables.of(l.name); ok {
		t.Fatal("a Volunteer's Mac was sighted")
	}
	d := l.deviceJSON(l.name)
	if d["cable"] != nil || d["tier"] == float64(2) {
		t.Fatalf("a Volunteer's Mac is shown with a cable or a Hard reset: cable %v, tier %v", d["cable"], d["tier"])
	}
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusForbidden || !strings.Contains(body, never) {
		t.Fatalf("a Maintainer's Hard reset: %s %s", r.Status, body)
	}
	if r, body := l.resetAs(harnessToken, l.name, "right"); r.StatusCode != http.StatusForbidden || !strings.Contains(body, never) {
		t.Fatalf("a Proving reset of its port, naming it: %s %s", r.Status, body)
	}
	grace := l.member("grace")
	l.leaseAs(grace, 0)
	if r, body := l.resetAs(grace.Token, l.name, ""); r.StatusCode != http.StatusForbidden || !strings.Contains(body, never) {
		t.Fatalf("its Lease holder's Hard reset: %s %s", r.Status, body)
	}
	if r, body := l.as(grace.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("grace releasing: %s %s", r.Status, body)
	}
	v := l.s.dev(l.name)
	if port, err := l.s.hardReset(context.Background(), v); err == nil || port != "" || !strings.Contains(err.Error(), never) {
		t.Fatalf("labd's own Hard reset: port %q, %v", port, err)
	}

	// The recovery ladder, for a Volunteer's Mac that went silent: network
	// means, then its Volunteer is asked.
	go l.sim.Quiet(time.Minute)
	l.waitDevice(func(d api.Device) bool { return time.Since(d.LastSeen) > 2*aliveWindow })
	steps, ok := v.ladder(context.Background(), "lost during a test", time.Second, true)
	if ok || strings.Contains(strings.ToLower(strings.Join(steps, "|")), "hard reset") {
		t.Fatalf("the ladder on a Volunteer's Mac: %v %v", ok, steps)
	}
	got := l.s.dev(l.name).snapshot()
	if got.State != api.StateNeedsHands || got.Action == nil || !strings.Contains(got.Action.Why, "The lab never sends a Volunteer's Mac a Hard reset") {
		t.Fatalf("after the ladder: %s %+v", got.State, got.Action)
	}
	l.wantResets("a Volunteer's Mac")
}

// A Hard reset is allowed to a Maintainer and to a Lease holder on the
// Lab-owned Mac they hold; a Proving reset is a Maintainer's only.
func TestAHardResetIsAMaintainersOrTheLeaseHoldersAndAProvingResetAMaintainersOnly(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	grace, linus := l.member("grace"), l.member("linus")
	back := func(boot string) {
		t.Helper()
		l.waitDevice(func(d api.Device) bool { return d.BootID != boot && d.State == api.StateReady })
	}

	if r, body := l.resetAs(grace.Token, l.name, ""); r.StatusCode != http.StatusForbidden || !strings.Contains(body, "needs their own Lease") {
		t.Fatalf("an M team member with no Lease: %s %s", r.Status, body)
	}
	if r, body := l.resetAs("lab_no_such_token", l.name, ""); r.StatusCode != http.StatusUnauthorized && r.StatusCode != http.StatusForbidden {
		t.Fatalf("nobody: %s %s", r.Status, body)
	}
	l.wantResets("nobody who may")

	l.leaseAs(grace, 0)
	if r, body := l.resetAs(linus.Token, l.name, ""); r.StatusCode != http.StatusForbidden {
		t.Fatalf("an M team member under another's Lease: %s %s", r.Status, body)
	}
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusConflict || !strings.Contains(body, "leased by") {
		t.Fatalf("a Maintainer under another person's Lease: %s %s", r.Status, body)
	}
	for _, port := range []string{"right", "left"} {
		if r, body := l.resetAs(grace.Token, l.name, port); r.StatusCode != http.StatusForbidden || !strings.Contains(body, "a Proving reset is a Maintainer's") {
			t.Fatalf("a Lease holder's Proving reset of the %s port: %s %s", port, r.Status, body)
		}
	}
	l.wantResets("before the Lease holder's own")
	boot := l.bootOf(l.name)
	if r, body := l.resetAs(grace.Token, l.name, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("the Lease holder's Hard reset: %s %s", r.Status, body)
	}
	l.wantResets("the Lease holder's", portRight)
	back(boot)

	if r, body := l.as(grace.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != http.StatusOK {
		t.Fatalf("grace releasing: %s %s", r.Status, body)
	}
	boot = l.bootOf(l.name)
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusOK {
		t.Fatalf("a Maintainer's Hard reset of a free Mac: %s %s", r.Status, body)
	}
	l.wantResets("a Maintainer's", portRight, portRight)
	back(boot)
}

// A Proving reset names the port, and counts only if that Mac returns and
// every other Mac keeps its boot id.
func TestAProvingResetCountsOnlyIfThatMacReturnsAndEveryOtherKeepsItsBootID(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max, third := l.addMac("m2-max"), l.addMac("m3")
	// The M2 Max is on the middle port and shows the mini no name, as on a
	// USB 2 cable; the third Mac, on the left, shows none either.
	l.oob.plug(portMiddle, max, "", productM2Max)
	l.oob.plug(portLeft, third, "", productM2Max)
	l.sight()
	l.wantCable("m2-max", api.CableNotSighted, "`lab reset m2-max --prove left|middle`")
	if r, body := l.resetAs(harnessToken, "m2-max", ""); r.StatusCode != http.StatusConflict || !strings.Contains(body, "the lab has no Sighting of m2-max") {
		t.Fatalf("a Hard reset of a Mac the lab cannot place: %s %s", r.Status, body)
	}

	for _, c := range []struct {
		why, mac, port string
		status         int
		saying         string
	}{
		{"a port the lab server has not", "m2-max", "front", http.StatusBadRequest, "left, middle and right"},
		{"the port of a sighted Mac", "m2-max", "right", http.StatusConflict, "the right port is the cable of " + l.name},
		{"a Mac the lab has a Sighting of", l.name, "middle", http.StatusConflict, l.name + " is sighted on the right port"},
	} {
		if r, body := l.resetAs(harnessToken, c.mac, c.port); r.StatusCode != c.status || !strings.Contains(body, c.saying) {
			t.Fatalf("%s: %s %s", c.why, r.Status, body)
		}
	}
	l.oob.unplug(portLeft)
	if r, body := l.resetAs(harnessToken, "m2-max", "left"); r.StatusCode != http.StatusConflict || !strings.Contains(body, "nothing is connected to the left port") {
		t.Fatalf("a port with nothing on it: %s %s", r.Status, body)
	}
	l.oob.plug(portLeft, third, "", productM2Max)
	l.wantResets("Proving resets the lab refused")

	// The wrong port: another Mac restarts and this one does not return.
	boots := map[string]string{l.name: l.bootOf(l.name), "m2-max": l.bootOf("m2-max"), "m3": l.bootOf("m3")}
	r, body := l.resetAs(harnessToken, "m2-max", "left")
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "did not count") || !strings.Contains(body, "m2-max did not return on a new boot") ||
		!strings.Contains(body, "m3 restarted") || !strings.Contains(body, "m2-max is not placed") {
		t.Fatalf("a Proving reset of another Mac's port: %s %s", r.Status, body)
	}
	l.wantResets("the wrong port", portLeft)
	if _, ok := l.s.cables.of("m2-max"); ok {
		t.Fatal("a Proving reset that did not count placed the Mac")
	}
	l.wantCable("m2-max", api.CableNotSighted, "the Proving reset of the left port", "did not count")
	if l.bootOf("m2-max") != boots["m2-max"] || l.bootOf("m3") == boots["m3"] {
		t.Fatal("the reset did not land on the Mac on the left port alone")
	}
	l.waitDeviceJSON("m3", func(m map[string]any) bool { return m["alive"] == true })

	// The right port, while another Mac restarts too: it does not count.
	boots["m3"] = l.bootOf("m3")
	go func() {
		for n, _ := l.oob.resetsSent(); n < 2; n, _ = l.oob.resetsSent() {
			time.Sleep(5 * time.Millisecond)
		}
		third.PowerCycle()
	}()
	r, body = l.resetAs(harnessToken, "m2-max", "middle")
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "did not count") || !strings.Contains(body, "m3 restarted too") {
		t.Fatalf("a Proving reset while another Mac restarted: %s %s", r.Status, body)
	}
	l.wantResets("another Mac restarted too", portLeft, portMiddle)
	if _, ok := l.s.cables.of("m2-max"); ok {
		t.Fatal("a Proving reset during which another Mac restarted placed the Mac")
	}
	l.waitDeviceJSON("m3", func(m map[string]any) bool { return m["alive"] == true && m["state"] != string(api.StateRecovering) })
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["alive"] == true && m["state"] != string(api.StateRecovering) })

	// The right port and a quiet fleet: that Mac returns, every other Mac
	// keeps its boot id, and the Mac is placed.
	boots = map[string]string{l.name: l.bootOf(l.name), "m2-max": l.bootOf("m2-max"), "m3": l.bootOf("m3")}
	r, body = l.resetAs(harnessToken, "m2-max", "middle")
	var proved api.Proving
	if r.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &proved) != nil || proved.Device != "m2-max" || proved.Port != "middle" || !proved.Cable.Proved {
		t.Fatalf("a Proving reset of its own port: %s %s", r.Status, body)
	}
	l.wantResets("its own port", portLeft, portMiddle, portMiddle)
	if l.bootOf("m2-max") == boots["m2-max"] || l.bootOf(l.name) != boots[l.name] || l.bootOf("m3") != boots["m3"] {
		t.Fatal("the Proving reset counted though the boot ids say otherwise")
	}
	sg, ok := l.s.cables.of("m2-max")
	if !ok || sg.rid != portMiddle || !sg.proved || sg.seen != (oob.Seen{ProductID: productM2Max, PlugCount: 43}) {
		t.Fatalf("the Sighting a Proving reset made: %+v %v", sg, ok)
	}
	l.wantCable("m2-max", api.CableSighted)
	if c := l.cableOf("m2-max"); c["port"] != "middle" || c["proved"] != true {
		t.Fatalf("the cable as shown: %v", c)
	}
	// A look at the ports keeps it: the port shows no name, and has not changed.
	l.sight()
	if _, ok := l.s.cables.of("m2-max"); !ok {
		t.Fatal("the next look dropped the Sighting a Proving reset made")
	}

	// Now a Hard reset reaches it, and only it.
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["alive"] == true && m["state"] != string(api.StateRecovering) })
	boot := l.bootOf("m2-max")
	if r, body := l.resetAs(harnessToken, "m2-max", ""); r.StatusCode != http.StatusOK || !strings.Contains(body, "through the middle port") {
		t.Fatalf("a Hard reset of the placed Mac: %s %s", r.Status, body)
	}
	l.wantResets("the placed Mac", portLeft, portMiddle, portMiddle, portMiddle)
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["boot_id"] != boot && m["alive"] == true })
	l.sight()

	// Its cable pulled while it stayed up: once it is heard on its boot
	// again, the placing is gone with it.
	l.oob.plug(portMiddle, max, "", productM2Max)
	l.sight()
	l.heardAgain("m2-max")
	l.sight()
	l.wantCable("m2-max", api.CableNotSighted, "its Sighting was dropped", "plug count")
}

// With the controller unhealthy: a reset is refused with the reason, the
// Mac is shown as having no Hard reset, jobs still run, and a line is
// written for "Agents", once.
func TestWithTheControllerUnhealthyNoMacGetsAHardResetAndJobsStillRun(t *testing.T) {
	clock := agent.NewFakeClock(time.Now())
	l := sightLab(t, labConfig{clock: clock, config: func(c *Config) {
		// labd's watch of its own parts, looking when the test says so.
		c.Watch = &Watch{ByHand: true, Disk: func() (uint64, uint64, error) { return 50, 100, nil }}
	}})
	t.Cleanup(pumpClock(clock))
	l.baseline()
	told := func() (lines []string) {
		for _, line := range l.agentsLines() {
			if strings.Contains(line, "Out-of-band controller") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	l.s.look(l.ctx)
	if lines := told(); len(lines) != 0 {
		t.Fatalf("a healthy controller was told of: %q", lines)
	}

	l.oob.stop(true)
	l.sight()
	l.s.look(l.ctx)
	l.wantCable(l.name, api.CableSighted, "the Out-of-band controller is not healthy")
	if tier := l.deviceJSON(l.name)["tier"]; tier == float64(2) {
		t.Fatal("the Mac is still shown as having a Hard reset")
	}
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(body, "no Hard reset was sent: the Out-of-band controller is not healthy") {
		t.Fatalf("a Hard reset with the controller stopped: %s %s", r.Status, body)
	}
	if r, body := l.resetAs(harnessToken, l.name, "left"); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "not healthy") {
		t.Fatalf("a Proving reset with the controller stopped: %s %s", r.Status, body)
	}

	// Jobs still run.
	l.expect(l.run(api.JobSpec{Kernel: l.artifact("krel=7.2-while-unhealthy behavior=ok")}), api.OutcomePass)
	l.waitDevice(func(d api.Device) bool { return d.State == api.StateReady })

	// "Agents" is told once the trouble has lasted, and once only.
	clock.Advance(3 * time.Minute)
	for range 3 {
		l.sight()
		l.s.look(l.ctx)
	}
	if lines := told(); len(lines) != 1 || lines[0] != "The Out-of-band controller on the mini is not healthy: it does not answer as it should. No Mac gets a Hard reset until it does; jobs still run." {
		t.Fatalf("what \"Agents\" was told: %q", lines)
	}

	// The ladder skips the rung and says why.
	v := l.s.dev(l.name)
	go l.sim.Quiet(time.Minute)
	l.waitDevice(func(d api.Device) bool { return l.s.since(d.LastSeen) > 2*aliveWindow })
	steps, ok := v.ladder(context.Background(), "lost during a test", time.Second, false)
	if ok || !has(steps, "no Hard reset: the Out-of-band controller is not healthy") {
		t.Fatalf("the ladder with the controller stopped: %v %v", ok, steps)
	}
	if d := v.snapshot(); d.State != api.StateNeedsHands || d.Action == nil || !strings.Contains(d.Action.Why, "No Hard reset was sent: the Out-of-band controller is not healthy") {
		t.Fatalf("after the ladder: %s %+v", d.State, d.Action)
	}
	l.wantResets("the controller was stopped")

	l.oob.stop(false)
	for range 2 {
		l.sight()
		l.s.look(l.ctx)
	}
	l.wantCable(l.name, api.CableSighted)
	if lines := told(); len(lines) != 2 || lines[1] != "The Out-of-band controller on the mini answers again." {
		t.Fatalf("after it answered again: %q", lines)
	}
}

// A lab with no Out-of-band controller, and a controller that was never set
// up, are no trouble to tell "Agents" of: no Mac has a Hard reset, and each
// says why.
func TestALabWithNoControllerSendsNoHardResetAndSaysWhy(t *testing.T) {
	l := newLab(t, false)
	l.baseline()
	l.wantCable(l.name, api.CableNone, "labd was given no Out-of-band controller")
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "labd was given no Out-of-band controller") {
		t.Fatalf("a Hard reset: %s %s", r.Status, body)
	}
	if r, body := l.resetAs(harnessToken, l.name, "left"); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "no Proving reset was sent") {
		t.Fatalf("a Proving reset: %s %s", r.Status, body)
	}

	mini := newFakeMini(t)
	missing := *mini.client
	missing.TokenFile += ".not-yet"
	set := newLabWith(t, labConfig{config: func(c *Config) { c.OOB, c.SightEvery = &missing, time.Hour }})
	mini.plug(portRight, set.sim, set.name, productM1Pro)
	set.sight()
	set.wantCable(set.name, api.CableNotSighted, "the Out-of-band controller is not set up", "its token is not at")
	if r, body := set.resetAs(harnessToken, set.name, ""); r.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "is not set up") {
		t.Fatalf("a Hard reset before the controller is set up: %s %s", r.Status, body)
	}
	if lines := set.agentsLines(); has(lines, "Out-of-band controller") {
		t.Fatalf("a controller that is not set up yet was told of: %q", lines)
	}
}

// `lab oob` and `lab status` are told sighted, not sighted or no cable.
func TestTheLabSaysSightedNotSightedOrNoCable(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	other := l.addMac("m2-max")
	status := func(token, name string) api.OOBStatus {
		t.Helper()
		r, body := l.as(token, "GET", "/api/devices/"+name+"/oob", nil)
		var st api.OOBStatus
		if r.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &st) != nil {
			t.Fatalf("the cable of %s: %s %s", name, r.Status, body)
		}
		return st
	}

	// Every connected port is another Mac's: no cable can be this one's.
	l.wantCable("m2-max", api.CableNone, "no cable can be the one of m2-max")
	st := status(harnessToken, "m2-max")
	if st.Cable == nil || st.Cable.State != api.CableNone || st.Controller != api.ControllerHealthy || len(st.Ports) != 3 {
		t.Fatalf("no cable: %+v", st)
	}
	if p := st.Ports[2]; p.Port != "right" || !p.Connected || p.Mac != l.name || p.Name != l.name || p.ProductID != productM1Pro || p.PlugCount != 41 {
		t.Fatalf("the right port as shown: %+v", p)
	}
	if p := st.Ports[0]; p.Port != "left" || p.Connected || p.Mac != "" {
		t.Fatalf("the left port as shown: %+v", p)
	}

	// A connected port with nothing sighted on it could be its cable.
	l.oob.plug(portLeft, other, "", productM2Max)
	l.sight()
	if st = status(harnessToken, "m2-max"); st.Cable.State != api.CableNotSighted || !strings.Contains(st.Cable.NoHardReset, "--prove left") {
		t.Fatalf("not sighted: %+v", st.Cable)
	}

	l.oob.shows(portLeft, "m2-max")
	l.sight()
	if st = status(harnessToken, "m2-max"); st.Cable.State != api.CableSighted || st.Cable.Port != "left" || st.Cable.NoHardReset != "" || st.Cable.Since.IsZero() {
		t.Fatalf("sighted: %+v", st.Cable)
	}
	if st = status(harnessToken, l.name); st.Cable.State != api.CableSighted || st.Cable.Port != "right" {
		t.Fatalf("the first Mac: %+v", st.Cable)
	}
}

// A Mac's record that still names the old lab's controller of its own gets
// no reset through it: nothing is ever sent to an address a record carries,
// and no record takes one any more.
func TestAControllerNamedInAMacsRecordIsNeverAsked(t *testing.T) {
	var asked atomic.Int32
	old := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { asked.Add(1) }))
	defer old.Close()
	l := newLab(t, false)
	l.baseline()
	v := l.s.dev(l.name)
	v.mu.Lock()
	v.d.OOB = &api.OOBConfig{Driver: "oobd", URL: old.URL, Token: "t"}
	v.saveLocked()
	v.mu.Unlock()

	l.wantCable(l.name, api.CableNone, "labd was given no Out-of-band controller")
	if d := l.deviceJSON(l.name); d["oob"] != nil || d["tier"] == float64(2) {
		t.Fatalf("the Mac as shown: oob %v, tier %v", d["oob"], d["tier"])
	}
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a Hard reset: %s %s", r.Status, body)
	}
	go l.sim.Quiet(time.Minute)
	l.waitDevice(func(d api.Device) bool { return time.Since(d.LastSeen) > 2*aliveWindow })
	if steps, ok := v.ladder(context.Background(), "lost during a test", time.Second, false); ok || !has(steps, "no Hard reset: labd was given no Out-of-band controller") {
		t.Fatalf("the ladder: %v %v", ok, steps)
	}
	if n := asked.Load(); n != 0 {
		t.Fatalf("the record's address was asked %d times", n)
	}
	r, body := l.as(harnessToken, "PATCH", "/api/devices/"+l.name, api.DevicePatch{OOB: &api.OOBConfig{Driver: "oobd", URL: old.URL, Token: "t"}})
	if r.StatusCode != http.StatusBadRequest || !strings.Contains(body, "a Mac's record names no controller and no port") {
		t.Fatalf("a record given a controller of its own: %s %s", r.Status, body)
	}
}

// A person's Hard reset waits its turn at the controller for ResetWait at
// most, and no longer once the controller is sending it: a second reset
// that gets its turn late in its wait is answered sent, the ladder runs and
// the Mac is reset once.
func TestAResetThatGetsItsTurnLateIsSentAndTheLadderRuns(t *testing.T) {
	l := sightLab(t, labConfig{config: func(c *Config) { c.ResetWait = time.Second }})
	l.baseline()
	other := l.addMac("m2-max")
	l.oob.plug(portLeft, other, "m2-max", productM2Max)
	l.sight()
	l.wantCable("m2-max", api.CableSighted)
	l.oob.slow(800 * time.Millisecond)

	first := make(chan string, 1)
	go func() { r, body := l.resetAs(harnessToken, "m2-max", ""); first <- r.Status + " " + body }()
	for end := time.Now().Add(5 * time.Second); len(l.oob.resetsBegun()) == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the first reset never reached the tool")
		}
	}
	boot := l.bootOf(l.name)
	r, body := l.resetAs(harnessToken, l.name, "")
	if r.StatusCode != http.StatusOK || !strings.Contains(body, "Hard reset sent to "+l.name+" through the right port") {
		t.Fatalf("a Hard reset that got its turn 0.8 s into a 1 s wait: %s %s", r.Status, body)
	}
	if got := <-first; !strings.Contains(got, "Hard reset sent to m2-max") {
		t.Fatalf("the first reset: %s", got)
	}
	l.waitDevice(func(d api.Device) bool { return d.BootID != boot && d.State == api.StateReady })
	for end := time.Now().Add(5 * time.Second); !strings.Contains(l.logs.String(), l.name+": Hard reset by "+harnessMaintainer+": ok=true"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("the ladder did not see %s back:\n%s", l.name, l.logs.String())
		}
	}
	time.Sleep(300 * time.Millisecond)
	l.wantResets("each Mac once", portLeft, portRight)
}

// The Lease is asked again when the reset's turn comes: a reset whose
// holder released the Lease while it waited for the controller is never
// sent.
func TestAResetWhoseLeaseEndsWhileItWaitsIsNotSent(t *testing.T) {
	l := newEndsLab(t)
	l.leaseAs(l.grace, 0)
	// Another port's tool run holds the controller's one lock.
	probing, release := l.oob.holdProbes(t)
	probed := make(chan error, 1)
	go func() { _, err := l.oob.client.Probe(context.Background(), portLeft); probed <- err }()
	<-probing
	answer := make(chan string, 1)
	go func() { r, body := l.resetAs(l.grace.Token, l.name, ""); answer <- r.Status + " " + body }()
	v := l.s.dev(l.name)
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		v.mu.Lock()
		waiting := v.oobCallCancel != nil
		v.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the Hard reset never reached the controller")
		}
	}
	time.Sleep(200 * time.Millisecond) // past labd's own look, at the controller's lock
	if r, body := l.as(l.grace.Token, "DELETE", "/api/devices/"+l.name+"/lease", nil); r.StatusCode != 200 {
		t.Fatalf("releasing the Lease: %s %s", r.Status, body)
	}
	time.Sleep(1500 * time.Millisecond)
	release()
	if err := <-probed; err != nil {
		t.Fatal(err)
	}
	got := <-answer
	if !strings.Contains(got, "403") || !strings.Contains(got, "needs their own Lease") {
		t.Fatalf("a reset whose Lease was released while it waited: %s", got)
	}
	time.Sleep(300 * time.Millisecond)
	l.wantResets("the Lease ended before the reset's turn")
}

// A Hard reset is attributed to the person whose credential asked for it,
// never to a name the request carries.
func TestAHardResetIsByWhoeverAskedNotByAName(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	boot := l.bootOf(l.name)
	r, body := l.as(harnessToken, "POST", "/api/devices/"+l.name+"/reset?by=mallory", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("a Hard reset: %s %s", r.Status, body)
	}
	l.waitDevice(func(d api.Device) bool { return d.BootID != boot && d.State == api.StateReady })
	log := l.logs.String()
	if strings.Contains(log, "mallory") || !strings.Contains(log, "recovering: Hard reset by "+harnessMaintainer) {
		t.Fatalf("the reset's attribution, in labd's log:\n%s", log)
	}
}

// The ladder after a person's Hard reset waits for a boot other than the
// one the Mac had before the reset went out: a Mac back on its new boot
// before the ladder starts is not taken as stuck and reset again.
func TestTheLadderAfterAPersonsResetKnowsTheBootBeforeIt(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	v := l.s.dev(l.name)
	boot, at := l.bootOf(l.name), time.Now()
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.BootID != boot && d.State == api.StateReady })
	steps, ok := v.climb(l.ctx, "Hard reset by "+harnessMaintainer, l.s.cfg.BootTimeout, false,
		&sentReset{port: "right", at: at, prev: api.StateReady, boot: boot})
	if !ok || len(steps) != 2 {
		t.Fatalf("the ladder of a Mac back already: ok=%v steps=%q", ok, steps)
	}
	l.wantResets("the Mac was back before the ladder began")
}

// A Proving reset carries the port as labd read it at the proof's start,
// and the controller compares it under its lock: a port that changes in
// between gets no reset.
func TestAProvingResetOfAPortThatChangesMeanwhileIsNotSent(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max := l.addMac("m2-max")
	l.oob.plug(portMiddle, max, "", productM2Max)
	l.sight()
	l.wantCable("m2-max", api.CableNotSighted, "--prove")
	var reads atomic.Int32
	l.oob.onRead(func(rid int) {
		// The proof's look at its ports, then its look at the middle one:
		// the cable is pulled and plugged back in after that.
		if rid == portMiddle && reads.Add(1) == 2 {
			l.oob.plug(portMiddle, max, "", productM2Max)
		}
	})
	r, body := l.resetAs(harnessToken, "m2-max", "middle")
	l.oob.onRead(nil)
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "no Proving reset was sent") || !strings.Contains(body, "plug count") {
		t.Fatalf("a Proving reset of a port that changed after labd looked: %s %s", r.Status, body)
	}
	l.wantResets("the port changed before the reset")
	if _, ok := l.s.cables.of("m2-max"); ok {
		t.Fatal("a Proving reset that was not sent placed the Mac")
	}
}

// waitResetting waits until a person's Hard reset of the lab's Mac waits
// its turn at the controller.
func (l *lab) waitResetting() {
	l.t.Helper()
	v := l.s.dev(l.name)
	for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		v.mu.Lock()
		waiting := v.oobCallCancel != nil
		v.mu.Unlock()
		if waiting {
			return
		}
		if time.Now().After(end) {
			l.t.Fatal("the Hard reset never reached the controller")
		}
	}
}

// ladderSaid waits for the line labd logs when the ladder after a
// person's Hard reset ends, and returns it.
func (l *lab) ladderSaid() string {
	l.t.Helper()
	want := l.name + ": Hard reset by " + harnessMaintainer + ": ok="
	for end := time.Now().Add(20 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		for _, line := range strings.Split(l.logs.String(), "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		if time.Now().After(end) {
			l.t.Fatalf("the ladder after the reset never ended:\n%s", l.logs.String())
		}
	}
}

// The ladder after a person's Hard reset knows the Mac as it was when the
// controller sent the reset, not when it was asked for: a Mac that
// restarts by itself while the reset waits its turn is not taken as
// recovered before the reset's own reboot.
func TestAMacThatRestartsWhileItsResetWaitsIsNotTakenAsRecovered(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	l.oob.slow(300 * time.Millisecond)
	probing, release := l.oob.holdProbes(t)
	probed := make(chan error, 1)
	go func() { _, err := l.oob.client.Probe(context.Background(), portLeft); probed <- err }()
	<-probing
	answer := make(chan string, 1)
	go func() { r, body := l.resetAs(harnessToken, l.name, ""); answer <- r.Status + " " + body }()
	l.waitResetting()

	before := l.bootOf(l.name)
	l.sim.PowerCycle()
	l.waitDevice(func(d api.Device) bool { return d.BootID != before })
	own := l.bootOf(l.name)
	release()
	if err := <-probed; err != nil {
		t.Fatal(err)
	}
	if got := <-answer; !strings.Contains(got, "Hard reset sent to "+l.name) {
		t.Fatalf("the reset that waited: %s", got)
	}
	said := l.ladderSaid()
	if now := l.bootOf(l.name); now == own || !strings.Contains(said, "ok=true") {
		t.Fatalf("the ladder ended (%s) with the Mac on the boot it restarted to by itself, before the reset's reboot", said)
	}
	l.wantResets("the one reset", portRight)
}

// A person's Hard reset that may have gone out is taken as sent: the Mac
// takes no job, and the ladder watches it come back. Here the wait for the
// controller's turn ends as the controller begins the reset, so labd hears
// no 102 while the tool resets the Mac.
func TestAResetCalledOffAsTheControllerBeginsItIsWatchedAsSent(t *testing.T) {
	l := sightLab(t, labConfig{config: func(c *Config) { c.ResetWait = 300 * time.Millisecond }})
	l.baseline()
	l.oob.mu.Lock()
	l.oob.onLook = func(int) { time.Sleep(800 * time.Millisecond) } // past labd's wait
	l.oob.mu.Unlock()
	before := l.bootOf(l.name)
	r, body := l.resetAs(harnessToken, l.name, "")
	if r.StatusCode == http.StatusOK {
		t.Fatalf("a reset whose wait ended first: %s %s", r.Status, body)
	}
	if s := l.s.dev(l.name).snapshot().State; s != api.StateRecovering {
		t.Errorf("the Mac whose reset may have gone out is %s", s)
	}
	said := l.ladderSaid()
	if !strings.Contains(said, "ok=true") || l.bootOf(l.name) == before {
		t.Fatalf("the ladder after a reset that went out unheard: %s", said)
	}
	l.wantResets("the reset the controller began", portRight)
}

// A reset whose tool fails after the controller said it was going out may
// have reset the Mac: it is taken as sent, and the ladder watches.
func TestAResetThatFailsAfterItWentOutIsWatchedAsSent(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	l.oob.mu.Lock()
	l.oob.rebootFails = errors.New("exit status 1: lost the port's partner")
	l.oob.mu.Unlock()
	before := l.bootOf(l.name)
	r, body := l.resetAs(harnessToken, l.name, "")
	if r.StatusCode == http.StatusOK || !strings.Contains(body, "may have gone out") {
		t.Fatalf("a reset whose tool failed after it began: %s %s", r.Status, body)
	}
	said := l.ladderSaid()
	if !strings.Contains(said, "ok=true") || l.bootOf(l.name) == before {
		t.Fatalf("the ladder after a reset whose tool failed: %s", said)
	}
	l.wantResets("the reset the tool sent", portRight)
}

// A reset that may have gone out and did not leaves the Mac as it was once
// it is seen still up on its boot: no second reset, and no person asked.
func TestAResetThatMayHaveGoneOutAndDidNotEndsWithTheMacUp(t *testing.T) {
	l := sightLab(t, labConfig{config: func(c *Config) { c.ResetWait = 300 * time.Millisecond }})
	l.baseline()
	probing, release := l.oob.holdProbes(t)
	probed := make(chan error, 1)
	go func() { _, err := l.oob.client.Probe(context.Background(), portLeft); probed <- err }()
	<-probing
	r, body := l.resetAs(harnessToken, l.name, "")
	if r.StatusCode == http.StatusOK || !strings.Contains(body, "no Hard reset was sent") {
		t.Fatalf("a reset whose turn did not come: %s %s", r.Status, body)
	}
	// The controller learns its caller left when it sees the connection
	// close, a moment after labd let go: the reset ends without its turn.
	l.oob.waitResetCalls(t, "the Hard reset labd let go still waits at the controller", func(asked, ended int) bool { return asked > 0 && ended == asked })
	release()
	if err := <-probed; err != nil {
		t.Fatal(err)
	}
	if said := l.ladderSaid(); !strings.Contains(said, "ok=true") {
		t.Fatalf("the watch of a reset that did not go out: %s", said)
	}
	if s := l.s.dev(l.name).snapshot().State; s != api.StateReady {
		t.Errorf("after the watch the Mac is %s", s)
	}
	time.Sleep(300 * time.Millisecond)
	l.wantResets("no reset went out")
}

// A look at the ports that began before the controller stopped, and ends
// after a Hard reset found it not answering, does not make it healthy again:
// the newer news stands until a look that began after it answers.
func TestALookBegunBeforeTheControllerStoppedDoesNotUndoItsTrouble(t *testing.T) {
	l := newLabWith(t, labConfig{oob: true, config: func(c *Config) { c.SightEvery = time.Hour }})
	l.baseline()
	l.sight()
	if _, ok := l.s.cables.of(l.name); !ok {
		t.Fatal("the Mac is not sighted")
	}
	last := oob.RIDs[len(oob.RIDs)-1]
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	l.oob.onRead(func(rid int) {
		if rid == last {
			once.Do(func() { close(reached); <-release })
		}
	})
	done := make(chan struct{})
	go func() { l.sight(); close(done) }()
	<-reached
	l.oob.stop(true)
	if _, err := l.s.hardReset(l.ctx, l.s.dev(l.name)); err == nil || !strings.Contains(err.Error(), "not healthy") {
		t.Fatalf("a Hard reset with the controller stopped: %v", err)
	}
	close(release)
	<-done
	if why := l.s.dev(l.name).noHardReset(); !strings.Contains(why, "not healthy") {
		t.Fatalf("after the look that began before the stop, the Mac's Hard reset: %q", why)
	}
	l.oob.stop(false)
	l.sight()
	if why := l.s.dev(l.name).noHardReset(); why != "" {
		t.Fatalf("after a look once the controller answers again: %q", why)
	}
}

// A Mac retired while its Proving reset waits its turn at the controller
// gets no reset: the retirement calls the reset off, as it does the
// ladder's, and the Proving reset says it was not sent.
func TestARetirementDuringAWaitingProvingResetSendsNoReset(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max := l.addMac("m2-max")
	l.oob.plug(portMiddle, max, "", productM2Max)
	l.sight()
	l.wantCable("m2-max", api.CableNotSighted, "--prove")
	probing, release := l.oob.holdProbes(t)
	probed := make(chan error, 1)
	go func() { _, err := l.oob.client.Probe(context.Background(), portLeft); probed <- err }()
	<-probing
	v := l.s.dev("m2-max")
	proved := make(chan error, 1)
	go func() { _, err := l.s.prove(v, portMiddle, harnessMaintainer); proved <- err }()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		l.oob.mu.Lock()
		asked := l.oob.resetsAsked
		l.oob.mu.Unlock()
		if asked > 0 {
			break
		}
		if time.Now().After(end) {
			t.Fatal("the Proving reset never reached the controller")
		}
	}
	retired := make(chan error, 1)
	go func() {
		retired <- l.s.endEnrollment(v, leaving{how: leftByMaintainer, by: Person{Handle: harnessMaintainer}})
	}()
	select {
	case err := <-proved:
		if err == nil || !strings.Contains(err.Error(), "no Proving reset") {
			t.Errorf("the Proving reset of a Mac retired while it waited: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the Proving reset still waits at the controller after the Mac was retired")
	}
	// The controller sees the call off before its turn comes free.
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		l.oob.mu.Lock()
		ended := l.oob.resetsEnded == l.oob.resetsAsked
		l.oob.mu.Unlock()
		if ended {
			break
		}
	}
	release()
	if err := <-probed; err != nil {
		t.Fatal(err)
	}
	if err := <-retired; err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	l.wantResets("the Mac was retired while its Proving reset waited")
}

// A Proving reset holds the Mac from the moment it is admitted: a job that
// comes while the reset reads the controller waits for it, and is never the
// one the reset cuts.
func TestAJobQueuedWhileAProvingResetReadsThePortsWaitsForIt(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max := l.addMac("m2-max")
	l.oob.plug(portMiddle, max, "", productM2Max)
	l.sight()
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	l.oob.onRead(func(rid int) {
		if rid == portMiddle {
			once.Do(func() { close(reached); <-release })
		}
	})
	v := l.s.dev("m2-max")
	proved := make(chan error, 1)
	go func() { _, err := l.s.prove(v, portMiddle, harnessMaintainer); proved <- err }()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the Proving reset never read the ports")
	}
	j, _, err := l.s.Submit(api.JobSpec{Device: "m2-max", Baseline: true})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if d := v.snapshot(); d.ActiveJob != "" {
			close(release)
			t.Fatalf("job %s was given the Mac while its Proving reset read the ports", d.ActiveJob)
		}
	}
	close(release)
	if err := <-proved; err != nil {
		t.Fatalf("the Proving reset: %v", err)
	}
	l.wantResets("the Proving reset", portMiddle)
	if got := l.wait(j.ID, 60*time.Second); got.Outcome != api.OutcomePass {
		t.Errorf("the job that waited for the Proving reset: %s %s", got.Outcome, got.Summary)
	}
}

// placeOnMiddle enrolls a Lab-owned Mac on the middle port that shows the
// mini no name, and places it there by a Proving reset.
func (l *lab) placeOnMiddle(name string) *agent.Sim {
	l.t.Helper()
	sim := l.addMac(name)
	l.oob.plug(portMiddle, sim, "", productM2Max)
	l.sight()
	if r, body := l.resetAs(harnessToken, name, "middle"); r.StatusCode != http.StatusOK {
		l.t.Fatalf("the Proving reset of %s: %s %s", name, r.Status, body)
	}
	l.waitDeviceJSON(name, func(m map[string]any) bool { return m["alive"] == true && m["state"] != string(api.StateRecovering) })
	return sim
}

// heardAgain waits for a check-in from the Mac named name after now.
func (l *lab) heardAgain(name string) {
	l.t.Helper()
	since := l.s.now()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if l.s.dev(name).heardAt().After(since) {
			return
		}
	}
	l.t.Fatalf("%s was not heard again", name)
}

// A Mac's own restarts move its port's plug count (the M2 Max's went from 21
// to 25 across a job's two reboots) and do not end its Sighting: a Mac placed
// by a Proving reset still gets a Hard reset after a job.
func TestASightingHoldsAcrossTheMacsOwnRestarts(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	l.oob.countResets(2)
	max := l.placeOnMiddle("m2-max")
	sg, _ := l.s.cables.of("m2-max")
	placed := sg.seen.PlugCount
	held := func(why string, plugs int) {
		t.Helper()
		sg, ok := l.s.cables.of("m2-max")
		if !ok || sg.rid != portMiddle || !sg.proved || sg.seen != (oob.Seen{ProductID: productM2Max, PlugCount: plugs}) {
			t.Fatalf("%s: the Sighting is %+v %v, want the middle port at plug count %d", why, sg, ok, plugs)
		}
		l.wantCable("m2-max", api.CableSighted)
	}

	// A job reboots it into its kernel and back: two restarts.
	boot := l.bootOf("m2-max")
	j, _, err := l.s.Submit(api.JobSpec{Device: "m2-max", Baseline: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.wait(j.ID, 60*time.Second); got.Outcome != api.OutcomePass {
		t.Fatalf("the job: %s %s", got.Outcome, got.Summary)
	}
	if l.bootOf("m2-max") == boot {
		t.Fatal("the job did not restart the Mac")
	}
	l.oob.restarted(portMiddle, 4)
	l.sight()
	held("after a job", placed+4)

	// Looked at while it is down for a restart, then once it is back.
	boot = l.bootOf("m2-max")
	l.oob.restarted(portMiddle, 2)
	if err := max.Reboot(); err != nil {
		t.Fatal(err)
	}
	l.sight()
	held("while it restarts", placed+4)
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["boot_id"] != boot && m["alive"] == true })
	l.sight()
	held("back from its restart", placed+6)

	// A Hard reset reaches it, and the plugs the reset counts do not end the
	// Sighting either.
	boot = l.bootOf("m2-max")
	if r, body := l.resetAs(harnessToken, "m2-max", ""); r.StatusCode != http.StatusOK || !strings.Contains(body, "through the middle port") {
		t.Fatalf("a Hard reset after the Mac's own restarts: %s %s", r.Status, body)
	}
	l.wantResets("the Proving reset and the Hard reset", portMiddle, portMiddle)
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["boot_id"] != boot && m["alive"] == true })
	l.sight()
	held("back from its Hard reset", placed+8)
}

// A Mac that restarted into a hang shows its port's new plug count and is
// never heard on a new boot: its Sighting holds, and the Hard reset that
// brings it back goes out.
func TestAMacHungAfterItsRestartStillGetsItsHardReset(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max := l.placeOnMiddle("m2-max")
	boot := l.bootOf("m2-max")
	l.oob.restarted(portMiddle, 2)
	go max.Quiet(10 * time.Second)
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["alive"] == false })
	l.sight()
	if _, ok := l.s.cables.of("m2-max"); !ok {
		t.Fatal("the look dropped the Sighting of a Mac that restarted and went silent")
	}
	if r, body := l.resetAs(harnessToken, "m2-max", ""); r.StatusCode != http.StatusOK || !strings.Contains(body, "through the middle port") {
		t.Fatalf("a Hard reset of the hung Mac: %s %s", r.Status, body)
	}
	l.wantResets("the Proving reset and the Hard reset", portMiddle, portMiddle)
	l.waitDeviceJSON("m2-max", func(m map[string]any) bool { return m["boot_id"] != boot && m["alive"] == true })
	l.sight()
	if sg, ok := l.s.cables.of("m2-max"); !ok || sg.rid != portMiddle {
		t.Fatalf("after the Hard reset: %+v %v", sg, ok)
	}
}

// Early in boot a Mac's Thunderbolt link announces a placeholder name
// before its hostname is set (m1-pro's port showed "unknown_xd_device" for
// 30 s during a job's reboot on 2026-11-16): that is no other Mac. It
// neither ends nor places a Sighting, a Mac hung there still gets its Hard
// reset, and a Proving reset takes it as a port that shows no name.
func TestAPlaceholderNameIsNoOtherMac(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	l.sight()
	for _, name := range []string{"unknown_xd_device", "(none)", "localhost", "archlinux", ""} {
		l.oob.shows(portRight, name)
		l.sight()
		if sg, ok := l.s.cables.of(l.name); !ok || sg.rid != portRight {
			t.Fatalf("the right port shows %q: the Sighting %+v %v, want it kept", name, sg, ok)
		}
		l.wantCable(l.name, api.CableSighted)
	}
	l.oob.shows(portRight, "unknown_xd_device")
	boot := l.bootOf(l.name)
	if r, body := l.resetAs(harnessToken, l.name, ""); r.StatusCode != http.StatusOK || !strings.Contains(body, "through the right port") {
		t.Fatalf("a Hard reset of a Mac showing a placeholder name: %s %s", r.Status, body)
	}
	l.wantResets("the Mac showing a placeholder name", portRight)
	l.waitDevice(func(d api.Device) bool { return d.BootID != boot && d.State == api.StateReady })

	// A placeholder places nothing, and a Proving reset is for that port.
	max := l.addMac("m2-max")
	l.oob.plug(portMiddle, max, "unknown_xd_device", productM2Max)
	l.sight()
	l.wantCable("m2-max", api.CableNotSighted, "`lab reset m2-max --prove middle`")
	if r, body := l.resetAs(harnessToken, "m2-max", "middle"); r.StatusCode != http.StatusOK {
		t.Fatalf("the Proving reset of a port showing a placeholder name: %s %s", r.Status, body)
	}
	l.wantResets("the Hard reset and the Proving reset", portRight, portMiddle)
	if sg, ok := l.s.cables.of("m2-max"); !ok || sg.rid != portMiddle {
		t.Fatalf("after the Proving reset: %+v %v", sg, ok)
	}

	// A real name of no Lab-owned Mac still ends the Sighting.
	l.oob.shows(portRight, "somebody-elses-mac")
	l.sight()
	if _, ok := l.s.cables.of(l.name); ok {
		t.Fatal("the Sighting outlived another Mac's name on its port")
	}
	l.wantCable(l.name, api.CableNotSighted, `the right port shows the name "somebody-elses-mac", which is not its own`)
}

// A cable pulled and put back while its Mac stays up moves the plug count
// as a restart does, but the Mac is heard again on the boot it had: that
// ends the Sighting. Until the lab knows, no Hard reset goes out.
func TestACablePulledWhileItsMacStaysUpEndsItsSighting(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max := l.placeOnMiddle("m2-max")
	boot := l.bootOf("m2-max")
	sg, _ := l.s.cables.of("m2-max")
	l.oob.plug(portMiddle, max, "", productM2Max)
	r, body := l.resetAs(harnessToken, "m2-max", "")
	if r.StatusCode != http.StatusConflict || !strings.Contains(body, "no Hard reset was sent") ||
		!strings.Contains(body, fmt.Sprintf("the middle port has plug count %d, not the %d it was seen with", sg.seen.PlugCount+1, sg.seen.PlugCount)) ||
		!strings.Contains(body, "m2-max is up on the boot it had") {
		t.Fatalf("a Hard reset of a Mac up while its plug count moved: %s %s", r.Status, body)
	}
	l.wantResets("the Proving reset alone", portMiddle)
	l.heardAgain("m2-max")
	l.sight()
	if _, ok := l.s.cables.of("m2-max"); ok {
		t.Fatal("the Sighting outlived a cable pulled while its Mac stayed up")
	}
	l.wantCable("m2-max", api.CableNotSighted, "its Sighting was dropped", "plug count", "was heard since on the boot it had")
	if l.bootOf("m2-max") != boot {
		t.Fatal("the Mac was restarted")
	}
}

// restartedLabd is a second labd on this lab's data, as after a Deploy, with a
// log the test reads: it looks at the ports once as it starts.
func (l *lab) restartedLabd() (*Server, *logBuf) {
	l.t.Helper()
	logs := &logBuf{}
	fresh, err := New(l.ctx, l.s.cfg, log.New(logs, "labd: ", 0))
	if err != nil {
		l.t.Fatal(err)
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if strings.Contains(logs.String(), "across the restart") {
			break
		}
	}
	return fresh, logs
}

// A Sighting is kept in labd's data: after a restart it holds while its port
// is still connected with the same product id, and the lab's rules go on
// from there.
func TestASightingIsKeptAcrossARestartOfLabd(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	max := l.placeOnMiddle("m2-max")
	placed, _ := l.s.cables.of("m2-max")
	named, _ := l.s.cables.of(l.name)
	fresh, logs := l.restartedLabd()
	for name, was := range map[string]sighting{"m2-max": placed, l.name: named} {
		sg, ok := fresh.cables.of(name)
		if !ok || sg.rid != was.rid || sg.seen != was.seen || sg.proved != was.proved || sg.boot != was.boot {
			t.Fatalf("%s after the restart: %+v %v, want %+v", name, sg, ok, was)
		}
	}
	for _, line := range []string{"sighting kept across the restart: m2-max on middle", "sighting kept across the restart: " + l.name + " on right"} {
		if !strings.Contains(logs.String(), line) {
			t.Fatalf("the log lacks %q:\n%s", line, logs.String())
		}
	}
	// From there the rules go on: a higher count holds it until the lab
	// can tell a restart from a pull.
	l.oob.plug(portMiddle, max, "", productM2Max)
	fresh.sight(l.ctx)
	if _, ok := fresh.cables.of("m2-max"); !ok {
		t.Fatal("the restarted labd dropped the Sighting at the first look at a higher count")
	}
}

// A kept Sighting whose port shows another product id after the restart, or
// nothing, is not kept, and the log says why.
func TestASightingIsNotKeptAcrossARestartWhenItsPortChanged(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(l *lab)
		saying string
	}{
		{"another product id", func(l *lab) { l.oob.swap(portMiddle, productM1Pro) }, "the middle port shows product id 29448, not the 29453 it was seen with"},
		{"nothing on the port", func(l *lab) { l.oob.unplug(portMiddle) }, "the middle port is no longer connected"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := sightLab(t, labConfig{})
			l.baseline()
			l.placeOnMiddle("m2-max")
			c.change(l)
			fresh, logs := l.restartedLabd()
			if _, ok := fresh.cables.of("m2-max"); ok {
				t.Fatal("the Sighting was kept across the restart")
			}
			if line := "sighting not kept across the restart: m2-max on middle: " + c.saying; !strings.Contains(logs.String(), line) {
				t.Fatalf("the log lacks %q:\n%s", line, logs.String())
			}
			if _, ok := fresh.cables.of(l.name); !ok {
				t.Fatalf("%s, whose port did not change, was not kept", l.name)
			}
		})
	}
}

// After the mini restarts its ports count plugs from the start again: a
// kept Sighting holds by the same rule, from the port as it is then.
func TestASightingIsKeptAcrossARestartOfTheMini(t *testing.T) {
	l := sightLab(t, labConfig{})
	l.baseline()
	l.placeOnMiddle("m2-max")
	l.oob.mu.Lock()
	l.oob.plugs[portMiddle], l.oob.plugs[portRight] = 1, 1
	l.oob.mu.Unlock()
	fresh, logs := l.restartedLabd()
	for _, name := range []string{"m2-max", l.name} {
		sg, ok := fresh.cables.of(name)
		if !ok || sg.seen.PlugCount != 1 {
			t.Fatalf("%s after the mini restarted: %+v %v\n%s", name, sg, ok, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "sighting kept across the restart: m2-max on middle") {
		t.Fatalf("the log:\n%s", logs.String())
	}
}
