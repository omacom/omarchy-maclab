package agent

import (
	"sync"
	"time"
)

// Clock is where labd and lab-agent read time: heartbeats, boot timeouts,
// leases and recovery all wait on it, so tests can drive them in milliseconds.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	NewTicker(d time.Duration) Ticker
	Sleep(d time.Duration)
	AfterFunc(d time.Duration, f func())
}

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// WallClock is the real clock, what production runs on.
var WallClock Clock = wallClock{}

type wallClock struct{}

func (wallClock) Now() time.Time                         { return time.Now() }
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
func (wallClock) Sleep(d time.Duration)                  { time.Sleep(d) }
func (wallClock) NewTicker(d time.Duration) Ticker       { return wallTicker{time.NewTicker(d)} }
func (wallClock) AfterFunc(d time.Duration, f func())    { time.AfterFunc(d, f) }

type wallTicker struct{ t *time.Ticker }

func (t wallTicker) C() <-chan time.Time { return t.t.C }
func (t wallTicker) Stop()               { t.t.Stop() }

// FakeClock stands still until a test advances it.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

type fakeWaiter struct {
	at    time.Time
	every time.Duration // a ticker's period; 0 for a one-shot
	c     chan time.Time
}

func NewFakeClock(now time.Time) *FakeClock { return &FakeClock{now: now} }

func (f *FakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *FakeClock) After(d time.Duration) <-chan time.Time { return f.wait(d, 0).c }

func (f *FakeClock) Sleep(d time.Duration) { <-f.After(d) }

func (f *FakeClock) AfterFunc(d time.Duration, fn func()) {
	c := f.After(d)
	go func() { <-c; fn() }()
}

func (f *FakeClock) NewTicker(d time.Duration) Ticker { return &fakeTicker{f, f.wait(d, d)} }

func (f *FakeClock) wait(d, every time.Duration) *fakeWaiter {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &fakeWaiter{at: f.now.Add(d), every: every, c: make(chan time.Time, 1)}
	if d <= 0 && every == 0 {
		w.c <- f.now
		return w
	}
	f.waiters = append(f.waiters, w)
	return w
}

// Next is when the earliest timer or ticker waiting on the clock falls due,
// and whether one is waiting.
func (f *FakeClock) Next() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var next time.Time
	for _, w := range f.waiters {
		if next.IsZero() || w.at.Before(next) {
			next = w.at
		}
	}
	return next, !next.IsZero()
}

// Advance moves the clock on and fires every timer and ticker that falls due.
// Like a real ticker, a fake one drops ticks nobody has read.
func (f *FakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	keep := f.waiters[:0]
	for _, w := range f.waiters {
		if !w.at.After(f.now) {
			select {
			case w.c <- f.now:
			default:
			}
			if w.every == 0 {
				continue
			}
			for !w.at.After(f.now) {
				w.at = w.at.Add(w.every)
			}
		}
		keep = append(keep, w)
	}
	f.waiters = keep
}

type fakeTicker struct {
	f *FakeClock
	w *fakeWaiter
}

func (t *fakeTicker) C() <-chan time.Time { return t.w.c }

func (t *fakeTicker) Stop() {
	t.f.mu.Lock()
	defer t.f.mu.Unlock()
	for i, w := range t.f.waiters {
		if w == t.w {
			t.f.waiters = append(t.f.waiters[:i], t.f.waiters[i+1:]...)
			return
		}
	}
}

// ScaledClock is the wall clock run k times faster, for tests whose waits
// are timings, not moments: it never needs advancing, so nothing stalls.
type ScaledClock struct {
	k     float64
	start time.Time // wall time when the clock was made
	from  time.Time // its time then
}

func NewScaledClock(k float64) *ScaledClock {
	now := time.Now()
	return &ScaledClock{k: k, start: now, from: now}
}

func (c *ScaledClock) Now() time.Time {
	return c.from.Add(time.Duration(float64(time.Since(c.start)) * c.k))
}

// Real is how long d on this clock takes on the wall clock.
func (c *ScaledClock) Real(d time.Duration) time.Duration {
	return time.Duration(float64(d) / c.k)
}

func (c *ScaledClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	time.AfterFunc(c.Real(d), func() { ch <- c.Now() })
	return ch
}

func (c *ScaledClock) Sleep(d time.Duration) { time.Sleep(c.Real(d)) }

func (c *ScaledClock) AfterFunc(d time.Duration, f func()) { time.AfterFunc(c.Real(d), f) }

func (c *ScaledClock) NewTicker(d time.Duration) Ticker {
	t := &scaledTicker{c: make(chan time.Time, 1), stop: make(chan struct{})}
	real := time.NewTicker(max(c.Real(d), time.Microsecond))
	go func() {
		defer real.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-real.C:
				select {
				case t.c <- c.Now():
				default:
				}
			}
		}
	}()
	return t
}

type scaledTicker struct {
	c    chan time.Time
	stop chan struct{}
	once sync.Once
}

func (t *scaledTicker) C() <-chan time.Time { return t.c }
func (t *scaledTicker) Stop()               { t.once.Do(func() { close(t.stop) }) }

// Scaled is how long d of wall time is on this clock.
func (c *ScaledClock) Scaled(d time.Duration) time.Duration {
	return time.Duration(float64(d) * c.k)
}
