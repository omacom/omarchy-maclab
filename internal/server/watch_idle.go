package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/omacom/omarchy-maclab/internal/api"
	"github.com/omacom/omarchy-maclab/internal/oob"
)

const (
	// idleOfflineHold is how long an idle, unleased Lab-owned Mac must stay
	// offline before labd attempts a rate-limited Hard reset or tells "Agents".
	idleOfflineHold = 10 * time.Minute
	// idleResetCooldown bounds automatic Hard resets of an idle offline Mac
	// so a dead or unplugged Mac never enters a reset loop.
	idleResetCooldown = time.Hour
	// markIdleResetPrefix persists the last automatic idle Hard reset time per
	// device enrollment ID in line_marks so recovery and restarts keep the limit.
	markIdleResetPrefix = "ops:idle-reset:"
)

func (s *Server) lastIdleReset(devID string) (time.Time, string) {
	if s.lines == nil {
		return time.Time{}, ""
	}
	raw, ok := s.lines.mark(markIdleResetPrefix + devID)
	if !ok {
		return time.Time{}, ""
	}
	ns, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, raw
	}
	return time.Unix(0, ns), raw
}

func (s *Server) recordIdleReset(devID string, at time.Time) {
	if s.lines == nil {
		return
	}
	s.lines.setMark(markIdleResetPrefix+devID, strconv.FormatInt(at.UnixNano(), 10))
}

func (s *Server) restoreIdleReset(devID, prev string) {
	if s.lines == nil {
		return
	}
	if prev == "" {
		s.lines.unmark(markIdleResetPrefix + devID)
		return
	}
	s.lines.setMark(markIdleResetPrefix+devID, prev)
}

// idleResetFencedLocked is whether an automatic idle reset must no longer go
// out to v because a person took a Lease, work started, the Mac left, or it
// already returned online. Caller holds v.mu.
func (v *dev) idleResetFencedLocked() bool {
	return v.d.State != api.StateOffline || v.liveLeaseLocked() != nil ||
		v.d.ActiveJob != "" || v.proving || v.updating != nil ||
		v.cleaning || v.forgotten
}

// fenceIdleWhileWaiting cancels an automatic idle Hard reset while it waits
// its turn at the Out-of-band controller if a Lease is taken or the Mac is no
// longer idle and offline. Once the controller begins sending, cancel stops nothing.
func (s *Server) fenceIdleWhileWaiting(ctx context.Context, cancel context.CancelFunc, v *dev, fenced *atomic.Bool) {
	tick := s.clock().NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		v.mu.Lock()
		stop := v.idleResetFencedLocked()
		ch := v.changed
		v.mu.Unlock()
		if stop {
			fenced.Store(true)
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ch:
		case <-tick.C():
		}
	}
}

// lookOffline watches idle, unleased Lab-owned Macs that go offline. After
// idleOfflineHold, a sighted Mac on AC gets one rate-limited Hard reset (at
// most once per idleResetCooldown); an unsighted Mac, one last seen on
// battery, or one that does not return after the reset tells "Agents" once.
func (s *Server) lookOffline(ctx context.Context, now time.Time) {
	w := &s.watch
	for _, v := range s.allDevs() {
		v.mu.Lock()
		d := v.d
		leased := v.liveLeaseLocked() != nil
		busy := d.ActiveJob != "" || v.proving || v.resetting || v.updating != nil || v.cleaning || v.forgotten
		noReset := v.noHardResetLocked()
		oldBoot := d.BootID
		v.mu.Unlock()

		if d.IsVolunteer() || d.State == api.StateLeft || oldBoot == "" {
			continue
		}
		key := "offline:" + d.ID
		switch d.State {
		case api.StateReady, api.StateNew:
			since := w.since[key]
			delete(w.since, key)
			msg := d.Name + " is back online."
			if !since.IsZero() {
				msg = fmt.Sprintf("%s is back online (was offline for %s).", d.Name, howLong(now.Sub(since)))
			}
			s.agents.Recovered(key, msg)
			continue
		case api.StateOffline:
		default:
			continue
		}
		if leased || busy {
			continue
		}
		since := w.since[key]
		if since.IsZero() {
			since = now
			w.since[key] = since
		}
		if now.Sub(since) < idleOfflineHold {
			continue
		}
		why := d.StateReason
		if why == "" {
			why = "offline since " + when(since)
		}
		canReset := noReset == "" && !d.Health.OnBattery
		lastReset, prevMark := s.lastIdleReset(d.ID)
		if canReset && (lastReset.IsZero() || now.Sub(lastReset) >= idleResetCooldown) {
			v.mu.Lock()
			eligible := !v.idleResetFencedLocked() && !v.resetting && !v.d.Health.OnBattery
			if eligible {
				v.resetting = true
			}
			v.mu.Unlock()
			if eligible {
				s.recordIdleReset(d.ID, now)
				go s.idleRecover(v, key, d.ID, prevMark, since, why)
			}
			continue
		}
		var detail string
		switch {
		case d.Health.OnBattery:
			detail = "no automatic Hard reset was sent because it was last seen on battery"
		case noReset != "":
			detail = "no automatic Hard reset was sent: " + noReset
		default:
			continue
		}
		s.agents.Trouble(key, fmt.Sprintf("%s has been offline since %s (%s); %s.", d.Name, when(since), why, detail))
	}
}

// idleRecover sends one rate-limited Hard reset to an idle offline Lab-owned
// Mac, waits one boot timeout for it to return on a new boot, and tells
// "Agents" the outcome. Caller set v.resetting = true.
func (s *Server) idleRecover(v *dev, key, devID, prevResetMark string, since time.Time, why string) {
	name := v.snapshot().Name
	rctx, cancel := s.withTimeout(s.ctx, s.cfg.ResetWait)
	var fencedCancel atomic.Bool
	go s.fenceIdleWhileWaiting(rctx, cancel, v, &fencedCancel)
	// Capture the Mac's time and boot id as the controller sends the reset so
	// an earlier check-in while waiting cannot count as post-reset recovery.
	var mark sync.Mutex
	var sentAt time.Time
	var sentBoot string
	dispatched := false
	rctx = oob.OnSending(rctx, func() {
		mark.Lock()
		defer mark.Unlock()
		sentAt, sentBoot, dispatched = s.now(), v.snapshot().BootID, true
		s.recordIdleReset(devID, sentAt)
	})
	port, err := v.resetOOB(rctx)
	cancel()
	mark.Lock()
	if !dispatched {
		sentAt, sentBoot = s.now(), v.snapshot().BootID
	}
	mark.Unlock()
	maybe := err != nil && resetMayHaveGone(err)
	// Only skip recovery and restore the cooldown when the controller confirmed
	// nothing was sent (!maybe). A fenced cancellation that returned oob.NotSent
	// (wrapped in mayHaveGone) may have raced the controller starting the reset,
	// so keep watching it in StateRecovering (suppressing any failure alert if
	// it was called off before 102).
	calledOffBefore102 := err != nil && !dispatched && fencedCancel.Load()
	confirmedNotSent := err != nil && !maybe

	v.mu.Lock()
	v.resetting = false
	if (err == nil || maybe) && s.ctx.Err() == nil {
		// Keep the Mac unavailable until the reset's boot is confirmed (or
		// a may-have-gone reset proves it never reached the Mac).
		v.setStateLocked(api.StateRecovering, "automatic Hard reset of idle offline Mac")
	}
	v.mu.Unlock()

	if s.ctx.Err() != nil {
		return
	}
	if confirmedNotSent {
		if calledOffBefore102 {
			s.restoreIdleReset(devID, prevResetMark)
			s.log.Printf("%s: automatic idle Hard reset called off before sending", name)
			return
		}
		s.log.Printf("%s: automatic idle Hard reset failed: %v", name, err)
		s.agents.Trouble(key, fmt.Sprintf("%s has been offline since %s (%s); automatic Hard reset failed: %v.", name, when(since), why, err))
		return
	}
	if maybe {
		s.log.Printf("%s: automatic Hard reset through the %s port may have gone out (offline since %s): %v", name, port, when(since), err)
	} else {
		s.log.Printf("%s: automatic Hard reset sent through the %s port (offline since %s)", name, port, when(since))
	}

	back := func() bool { return v.d.BootID != sentBoot && v.d.LastSeen.After(sentAt) }
	stillUp := func() bool {
		return maybe && v.d.BootID == sentBoot && v.aliveLocked() && v.d.LastSeen.After(sentAt.Add(s.cfg.RebootGrace))
	}
	waitErr := v.waitFor(s.ctx, s.cfg.BootTimeout, func() bool { return back() || stillUp() })
	if waitErr == nil {
		v.mu.Lock()
		rebooted := back()
		if v.d.State == api.StateRecovering {
			v.setStateLocked(v.idleStateLocked(), "")
			v.restore = ""
		}
		v.mu.Unlock()
		s.watch.mu.Lock()
		delete(s.watch.since, key)
		s.watch.mu.Unlock()
		if rebooted {
			s.log.Printf("%s: back online after automatic Hard reset through the %s port", name, port)
			if !s.agents.Recovered(key, fmt.Sprintf("%s is back online after an automatic Hard reset through the %s port.", name, port)) {
				s.agents.IdleRecovered(name, port, since)
			}
		} else {
			s.log.Printf("%s: automatic Hard reset did not go out; still up on its boot", name)
			s.agents.Recovered(key, name+" is back online.")
		}
		return
	}
	if s.ctx.Err() != nil || errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, errMacGone) {
		if s.ctx.Err() == nil && !errors.Is(waitErr, errMacGone) {
			v.mu.Lock()
			if v.d.State == api.StateRecovering {
				v.setStateLocked(api.StateOffline, why)
			}
			v.mu.Unlock()
		}
		return
	}
	v.mu.Lock()
	if v.d.State == api.StateRecovering {
		v.setStateLocked(api.StateOffline, why)
	}
	v.mu.Unlock()
	if calledOffBefore102 {
		s.log.Printf("%s: automatic idle Hard reset was called off before sending and the Mac stayed offline", name)
		return
	}
	s.log.Printf("%s: automatic Hard reset through the %s port did not bring it back within %s", name, port, s.cfg.BootTimeout)
	s.agents.Trouble(key, fmt.Sprintf("%s has been offline since %s (%s); one automatic Hard reset through the %s port did not bring it back.", name, when(since), why, port))
}
