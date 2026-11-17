package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/detect"
)

// panicGrace is how long after a panic labd waits for the Mac to reboot by
// itself: panic=10 should have rebooted it well within this.
var panicGrace = 60 * time.Second

// waitBack waits for the Mac to come up on a new boot after `since`. When it
// doesn't, it gives the Mac selfRecovery more to come back by itself (a Lab
// boot's deadlines reboot it), then walks the recovery ladder. It returns the
// ladder steps used, whether the Mac is back, and whether the ladder found it
// still up on oldBoot: it never went down by itself, whatever the ladder did
// after. A check-in on oldBoot after upAfter says it is still up: what labd
// heard over the whole wait, not whether a check-in fell in the last
// aliveWindow at its end, which a slow check-in can miss. It goes by when
// the check-in came (heard), not by LastSeen, which is when labd had saved
// it: a check-in from before the Mac went down, saved slowly, says nothing.
// That check-in must also still be the Mac's last word at the deadline (see
// stillHeardLocked).
func (v *dev) waitBack(ctx context.Context, oldBoot string, since, upAfter time.Time, timeout, selfRecovery time.Duration) (steps []string, ok, neverLeft bool, err error) {
	var reason string
	err = v.waitFor(ctx, timeout+selfRecovery, func() bool {
		if v.d.BootID != oldBoot && v.d.LastSeen.After(since) {
			return true
		}
		now := v.s.now()
		if v.d.IsVolunteer() {
			// A Volunteer's Mac gets to recover by itself until its Hard ceiling plus a boot timeout.
			return false
		}
		for _, e := range v.events {
			if e.Time.After(since) && !(v.networkOnlyLocked() && e.Source == "serial") && e.Kind == detect.Panic && now.Sub(e.Time) > panicGrace && !v.supersededLocked(e, oldBoot) {
				reason = "kernel panicked and did not reboot within " + panicGrace.String()
				return true
			}
		}
		return false
	})
	if err != nil && err != context.DeadlineExceeded {
		return nil, false, false, err
	}
	v.mu.Lock()
	back := v.d.BootID != oldBoot && v.d.LastSeen.After(since)
	stillUp := v.d.BootID == oldBoot && v.heard.After(upAfter) && v.stillHeardLocked(v.s.now())
	v.mu.Unlock()
	if back {
		return nil, true, false, nil
	}
	if reason == "" {
		switch {
		case stillUp:
			neverLeft = true
			reason = fmt.Sprintf("still on the old boot %s after asking it to reboot", timeout+selfRecovery)
		case selfRecovery > 0:
			reason = fmt.Sprintf("did not check in within %s, nor come back by itself %s after that", timeout, selfRecovery)
		default:
			reason = fmt.Sprintf("did not check in within %s", timeout)
		}
	}
	steps, ok = v.ladder(ctx, reason, timeout, stillUp)
	return steps, ok, neverLeft, nil
}

// stillHeardLocked is whether the Mac's last check-in is still its word at
// now: heard within quietGrace, or since gone to sleep or into a spell off
// the network it announced. One check-in and then silence for good, with
// nothing said, is a hang. Caller holds mu.
func (v *dev) stillHeardLocked(now time.Time) bool {
	return now.Sub(v.heard) <= v.s.quietGrace() || !v.asleep.IsZero() || v.quietLocked(now)
}

// ladder is the recovery ladder: a soft reboot, a Hard reset (twice), then a
// person. It returns the steps taken and whether the Mac came back.
//
// A Hard reset goes only to a Lab-owned Mac the lab has a Sighting of, while
// the Out-of-band controller is healthy. Without one the rung is skipped:
// the Mac's own watchdog had its time before the ladder (see
// lostSelfRecovery), so a person is next, and is told why. A Volunteer's Mac
// never gets a Hard reset.
//
// A Mac that left the lab (lab-agent uninstall) is never reset or handed to a
// human: its agent stopped on purpose.
func (v *dev) ladder(ctx context.Context, reason string, bootTimeout time.Duration, trySoft bool) ([]string, bool) {
	return v.climb(ctx, reason, bootTimeout, trySoft, nil)
}

// sentReset is a Hard reset a person asked for, which went out before the
// ladder began: through which port, when, and the state and boot the Mac
// was in.
type sentReset struct {
	port string
	// at is when the controller sent it, and boot the Mac's boot id then.
	at   time.Time
	prev api.DeviceState
	boot string
	// maybe is a reset that may have gone out (see mayHaveGone): a Mac
	// still up on boot after it did not get it.
	maybe bool
}

// mayHaveGone is a Hard reset's failure after it may have gone out: the
// controller said it was going out and then failed, the connection was
// lost before any answer, or the wait for its turn ended just as it may
// have come. It is taken as sent, and the Mac is watched.
type mayHaveGone struct{ error }

func (e mayHaveGone) Unwrap() error { return e.error }

func resetMayHaveGone(err error) bool {
	var m mayHaveGone
	return errors.As(err, &m)
}

// climb is the ladder. With sent, its first Hard reset is that one. It
// stops once the Mac's enrollment has ended: a retired Mac is not one that
// did not come back.
func (v *dev) climb(ctx context.Context, reason string, bootTimeout time.Duration, trySoft bool, sent *sentReset) ([]string, bool) {
	stopped := func() string {
		if why := v.goneReason(); why != "" {
			return why
		}
		return v.leaving()
	}
	if why := stopped(); why != "" {
		return []string{"not recovered: " + why}, false
	}
	v.mu.Lock()
	stuck := v.d.BootID
	// trySoft is the caller's word that the Mac still checks in on its boot.
	alive := trySoft
	volunteers := v.d.IsVolunteer()
	noReset := v.noHardResetLocked()
	prev := v.d.State
	v.mu.Unlock()
	if sent != nil {
		noReset, prev, stuck = "", sent.prev, sent.boot
	}
	v.setState(api.StateRecovering, reason)
	v.s.log.Printf("%s: recovering: %s", v.d.Name, reason)
	steps := []string{"detected: " + reason}

	backSince := func(t time.Time) func() bool {
		return func() bool { return v.d.BootID != stuck && v.d.LastSeen.After(t) }
	}
	done := func() ([]string, bool) {
		v.setState(prev, "")
		return steps, true
	}

	if alive {
		steps = append(steps, "soft reboot via agent")
		t0 := v.s.now()
		v.send(api.CmdReboot, "", nil)
		if v.waitFor(ctx, bootTimeout, backSince(t0)) == nil {
			return done()
		}
		if why := stopped(); why != "" {
			return append(steps, "stopped: "+why), false
		}
		steps = append(steps, "soft reboot did not bring it back")
	}
	// unreset is why no Hard reset brought it back, for the person asked.
	var unreset string
	switch {
	case volunteers:
	case noReset != "":
		steps = append(steps, "no Hard reset: "+noReset)
		unreset = "No Hard reset was sent: " + noReset
	default:
		count := 0
	resets:
		for i := 1; i <= 2; i++ {
			if ctx.Err() != nil {
				return steps, false
			}
			if why := stopped(); why != "" {
				return append(steps, "stopped: "+why), false
			}
			var t0 time.Time
			var port string
			maybe := false
			if i == 1 && sent != nil {
				t0, port, maybe = sent.at, sent.port, sent.maybe
			} else {
				t0 = v.s.now()
				rctx, cancel := context.WithTimeout(ctx, time.Minute)
				var err error
				port, err = v.resetOOB(rctx)
				cancel()
				if err != nil && !resetMayHaveGone(err) {
					steps = append(steps, fmt.Sprintf("hard reset %d not sent: %v", i, err))
					unreset = strings.TrimPrefix(err.Error(), "no Hard reset was sent: ")
					if v.noHardReset() != "" {
						break resets
					}
					continue
				}
				maybe = err != nil
			}
			count++
			if maybe {
				steps = append(steps, fmt.Sprintf("hard reset %d may have gone out via the %s port", i, port))
			} else {
				steps = append(steps, fmt.Sprintf("hard reset %d via the %s port", i, port))
			}
			// A Mac a reset may have missed and that still checks in on its
			// boot after a reboot's grace did not get it.
			stillUp := func() bool {
				return maybe && v.d.BootID == stuck && v.aliveLocked() && v.d.LastSeen.After(t0.Add(v.s.cfg.RebootGrace))
			}
			back := backSince(t0)
			if v.waitFor(ctx, bootTimeout, func() bool { return back() || stillUp() }) == nil {
				v.mu.Lock()
				missed := !back()
				v.mu.Unlock()
				if missed {
					steps = append(steps, fmt.Sprintf("hard reset %d did not go out: still up on its boot", i))
				}
				return done()
			}
			if why := stopped(); why != "" {
				return append(steps, "stopped: "+why), false
			}
			steps = append(steps, fmt.Sprintf("no check-in %s after hard reset %d", bootTimeout, i))
		}
		switch {
		case count == 2:
			unreset = "Two Hard resets did not bring it back"
		case count == 1:
			unreset = "One Hard reset did not bring it back, and another was not sent: " + unreset
		default:
			unreset = "No Hard reset was sent: " + unreset
		}
	}
	if ctx.Err() != nil {
		return steps, false
	}
	if why := stopped(); why != "" {
		return append(steps, "stopped: "+why), false
	}
	v.mu.Lock()
	act := humanAction(v.d, reason, unreset, v.lastLogLocked(), v.s.now())
	n := Notification{Device: v.d.Name, Title: act.Title, Body: act.Text(), Priority: "urgent"}
	if v.d.IsVolunteer() {
		n = v.s.notice(v.d, noticeRestart, jobWording{})
	}
	v.d.Action = act
	v.restore = ""
	v.setStateLocked(api.StateNeedsHands, reason)
	v.dirty = true
	v.mu.Unlock()
	steps = append(steps, "asked a human to power cycle it")
	v.s.notify(n)
	return steps, false
}

func (v *dev) lastLogLocked() string {
	for i := len(v.events) - 1; i >= 0; i-- {
		if detect.Fatal(v.events[i].Kind) && !(v.networkOnlyLocked() && v.events[i].Source == "serial") {
			return v.events[i].Line
		}
	}
	return ""
}

// humanAction writes the instruction for the person at the desk: which
// machine, exactly what to press, and that nothing else is needed.
// unreset is why no Hard reset brought it back, as a sentence: none could be
// sent, or two were and it stayed away.
func humanAction(d api.Device, reason, unreset, lastLog string, now time.Time) *api.HumanAction {
	where := d.Name
	if d.Label != "" {
		where = fmt.Sprintf("%s (%s)", d.Name, d.Label)
	}
	a := &api.HumanAction{Title: d.Name + " needs a power cycle", LastLog: lastLog, Since: now}
	switch d.Model {
	case api.ModelDesktop:
		a.Steps = []string{
			"Find " + where + ".",
			"Unplug its power cable and wait 10 seconds.",
			"Plug it back in. If it doesn't start, press the power button on the back once.",
		}
	default:
		a.Steps = []string{
			"Find " + where + ". Open the lid if it's closed.",
			"Hold the power button (the Touch ID key, top right of the keyboard) for 10 seconds, until the screen stays black.",
			"Wait 5 seconds, then press it once to turn it back on.",
		}
	}
	a.Steps = append(a.Steps, "That's it. The lab notices when it checks in again and picks up from there.")
	a.Why = reason
	switch {
	case d.EffectiveRecoveryMode == api.RecoveryNetworkOnly:
		a.Why += ". Network-only recovery is selected; no Hard reset was attempted"
	case d.IsVolunteer():
		a.Why += ". The lab never sends a Volunteer's Mac a Hard reset"
	case unreset != "":
		a.Why += ". " + unreset
	}
	return a
}
