package transfer

import "time"

const (
	// Throughput is measured over stable windows after streams settle.
	adaptiveWindow  = 5 * time.Second
	adaptiveWindows = 2
	adaptiveSettle  = 2 * time.Second
	// No new experiment for this long after one finishes.
	adaptiveHold = 30 * time.Second
	// A higher stream level is kept only if it beats its lower neighbour by 10%.
	adaptiveGain = 0.10
	// Streams are shed only when the lower level keeps 97% of the throughput,
	// so a shed costs at most a few percent of speed.
	adaptiveKeep = 0.97
	// Experiments need this much remaining transfer time to repay their cost.
	adaptiveMinRemaining = 120 * time.Second
)

type adaptPhase string

const (
	// Tasks ramp to their own limits as before; the first stable windows
	// become the baseline.
	phaseRamp adaptPhase = "ramp"
	// Running at half the streams to see whether the extra half pays off.
	phaseLower adaptPhase = "testLower"
	// Running at twice the streams to see whether more streams pay off.
	phaseHigher adaptPhase = "probeHigher"
	// Measuring the current level; no experiment before holdUntil.
	phaseHold adaptPhase = "hold"
)

// adaptiveState validates a source's stream count with throughput measured
// across all of its tasks, so streams that only take bandwidth from each other
// are not mistaken for a gain. It never delays the initial ramp: it starts
// from what the tasks' own limits allow and only runs experiments when the
// remaining transfer is long enough to absorb their cost.
type adaptiveState struct {
	target      int // stream target; 0 means the members' combined limits
	level       int // streams the current rate was measured at
	rate        float64
	confirmed   bool // level beat its lower neighbour
	phase       adaptPhase
	trial       int
	samples     []float64
	windowStart time.Time
	windowBytes int64
	lastActive  int
	settleUntil time.Time
	holdUntil   time.Time
	experiments int
}

// AdaptiveSnapshot is the diagnostic view of a source's adaptive state.
type AdaptiveSnapshot struct {
	Target      int     `json:"target"`
	Level       int     `json:"level"`
	Phase       string  `json:"phase"`
	Rate        float64 `json:"rate"`
	Experiments int     `json:"experiments"`
}

func (a *adaptiveState) snapshot() AdaptiveSnapshot {
	return AdaptiveSnapshot{Target: a.target, Level: a.level, Phase: string(a.phase), Rate: a.rate, Experiments: a.experiments}
}

// reset starts over after tasks join or leave: the new mix ramps freely and
// the windows are rebuilt.
func (a *adaptiveState) reset(now time.Time, s *source) {
	*a = adaptiveState{
		phase:       phaseRamp,
		windowStart: now,
		windowBytes: s.bytes.Load(),
		lastActive:  s.active,
		settleUntil: now.Add(adaptiveSettle),
		experiments: a.experiments,
	}
}

// rateLimited ends any experiment; the source cooldown handles the pause.
func (a *adaptiveState) rateLimited(now time.Time) {
	if a.phase == phaseLower || a.phase == phaseHigher {
		a.target = a.level
	}
	if a.phase != phaseRamp {
		a.phase = phaseHold
	}
	a.holdUntil = now.Add(adaptiveHold)
	a.samples = a.samples[:0]
	a.settleUntil = now.Add(adaptiveSettle)
}

func (a *adaptiveState) tick(now time.Time, s *source) {
	bytes := s.bytes.Load()
	current := s.active - s.shedding
	if current != a.lastActive {
		// Streams starting or draining distort the rate; wait for stability.
		a.lastActive = current
		a.settleUntil = later(a.settleUntil, now.Add(adaptiveSettle))
	}
	if now.Before(a.settleUntil) || s.active != current {
		a.windowStart, a.windowBytes = now, bytes
		a.samples = a.samples[:0]
		return
	}
	elapsed := now.Sub(a.windowStart)
	if elapsed < adaptiveWindow {
		return
	}
	a.samples = append(a.samples, float64(bytes-a.windowBytes)/elapsed.Seconds())
	a.windowStart, a.windowBytes = now, bytes
	if len(a.samples) < adaptiveWindows {
		return
	}
	mean := 0.0
	for _, rate := range a.samples {
		mean += rate
	}
	mean /= float64(len(a.samples))
	a.samples = a.samples[:0]
	a.measured(now, s, current, mean)
}

func (a *adaptiveState) measured(now time.Time, s *source, current int, mean float64) {
	switch a.phase {
	case phaseLower:
		if current < a.level {
			if mean >= adaptiveKeep*a.rate {
				a.level, a.rate, a.confirmed = current, mean, false
			} else {
				a.confirmed = true
			}
		}
		a.finish(now)
	case phaseHigher:
		if current > a.level && mean >= (1+adaptiveGain)*a.rate {
			a.level, a.rate, a.confirmed = current, mean, true
		}
		a.finish(now)
	default:
		// Ramp and hold keep the baseline current at the running level.
		a.level, a.rate = current, mean
		if a.phase == phaseRamp {
			a.phase, a.holdUntil = phaseHold, now
		}
		if !now.Before(a.holdUntil) {
			a.next(now, s)
		}
	}
}

func (a *adaptiveState) finish(now time.Time) {
	a.target = a.level
	a.phase = phaseHold
	a.holdUntil = now.Add(adaptiveHold)
}

// next starts the following experiment when the remaining transfer can pay
// for it: first halve unconfirmed levels, then probe one doubling up.
func (a *adaptiveState) next(now time.Time, s *source) {
	floor, top := len(s.members), s.limitSum()
	if a.rate <= 0 || !a.worthExperiment(s) {
		a.holdUntil = now.Add(adaptiveHold)
		return
	}
	switch {
	case !a.confirmed && a.level > floor:
		a.trial, a.phase = max(floor, a.level/2), phaseLower
	case a.level < top:
		a.trial, a.phase = min(top, a.level*2), phaseHigher
	default:
		a.holdUntil = now.Add(adaptiveHold)
		return
	}
	a.target = a.trial
	a.experiments++
	a.settleUntil = now.Add(adaptiveSettle)
}

func (a *adaptiveState) worthExperiment(s *source) bool {
	var remaining int64
	for _, m := range s.members {
		remaining += m.remaining.Load()
	}
	return remaining > 0 && float64(remaining)/a.rate >= adaptiveMinRemaining.Seconds()
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
