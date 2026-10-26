package transfer

import (
	"sort"
	"sync"
	"time"
)

// fakeClock runs timers only when the test advances virtual time.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

// Advance moves time forward, firing due timers in order outside the lock.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
		var due *fakeTimer
		for i, t := range c.timers {
			if t.stopped {
				continue
			}
			if !t.at.After(target) {
				due = t
				c.timers = append(c.timers[:i], c.timers[i+1:]...)
			}
			break
		}
		if due == nil {
			c.now = target
			kept := c.timers[:0]
			for _, t := range c.timers {
				if !t.stopped {
					kept = append(kept, t)
				}
			}
			c.timers = kept
			c.mu.Unlock()
			return
		}
		if due.at.After(c.now) {
			c.now = due.at
		}
		due.stopped = true
		c.mu.Unlock()
		due.f()
	}
}
