package transfer

import (
	"testing"
	"time"
)

// adaptiveSim plays the fetcher side: members ramp to whatever the budget
// grants, shed streams finish their retained range one second later, and the
// source delivers bytes according to a throughput model.
type adaptiveSim struct {
	t       *testing.T
	b       *Budget
	clock   *fakeClock
	key     SourceKey
	members []*Member
	pending []shedRelease
	// rate returns bytes delivered in one second for the given stream count and
	// the number of seconds since streams were last added.
	rate      func(streams int, sinceAdd int) float64
	sinceAdd  int
	trace     []int
	maxActive int
}

type shedRelease struct {
	m  *Member
	at time.Time
}

func newAdaptiveSim(t *testing.T, limits []int, remaining int64, rate func(int, int) float64) *adaptiveSim {
	b, clock := testBudget()
	sim := &adaptiveSim{t: t, b: b, clock: clock, key: "origin|https://sim.test|direct", rate: rate}
	for _, limit := range limits {
		m := b.Join(sim.key, limit, true)
		m.SetRemaining(remaining)
		sim.members = append(sim.members, m)
	}
	return sim
}

func (s *adaptiveSim) step() {
	now := s.clock.Now()
	kept := s.pending[:0]
	for _, p := range s.pending {
		if !p.at.After(now) {
			p.m.ReleaseShed()
		} else {
			kept = append(kept, p)
		}
	}
	s.pending = kept
	added := false
	for _, m := range s.members {
		for m.ShedOne() {
			s.pending = append(s.pending, shedRelease{m, now.Add(time.Second)})
		}
		for m.TryAcquire() {
			added = true
		}
	}
	if added {
		s.sinceAdd = 0
	} else {
		s.sinceAdd++
	}
	snap := s.members[0].Snapshot()
	active := snap.SourceActive
	if active > s.maxActive {
		s.maxActive = active
	}
	limitSum := 0
	for _, m := range s.members {
		if got := m.Snapshot().Active; got > m.limit {
			s.t.Fatalf("member exceeded its own limit: %d > %d", got, m.limit)
		}
		limitSum += m.limit
	}
	if active > limitSum {
		s.t.Fatalf("source exceeded its combined limits: %d > %d", active, limitSum)
	}
	s.members[0].Transferred(int64(s.rate(active, s.sinceAdd)))
	s.trace = append(s.trace, active-len(s.pending))
	s.clock.Advance(time.Second)
}

func (s *adaptiveSim) run(seconds int) {
	for i := 0; i < seconds; i++ {
		s.step()
	}
}

func (s *adaptiveSim) adaptive() AdaptiveSnapshot {
	snap := s.members[0].Snapshot()
	if snap.Adaptive == nil {
		s.t.Fatal("adaptive state missing")
	}
	return *snap.Adaptive
}

const mib = 1 << 20

func TestAdaptiveShedsStreamsThatDoNotAddThroughput(t *testing.T) {
	// A source capped at 1 MiB/s in total, whatever the stream count.
	sim := newAdaptiveSim(t, []int{4}, 10<<30, func(n, _ int) float64 { return mib })
	sim.run(3)
	if sim.maxActive != 4 {
		t.Fatalf("initial ramp must reach the task limit immediately, got %d", sim.maxActive)
	}
	sim.run(200)
	if got := sim.trace[len(sim.trace)-1]; got > 2 {
		t.Fatalf("capped source still uses %d streams after 200s", got)
	}
	ones := 0
	for _, n := range sim.trace[120:] {
		if n == 1 {
			ones++
		}
	}
	if ones < 40 {
		t.Fatalf("expected the source to settle at one stream most of the time, trace=%v", sim.trace)
	}
	if sim.maxActive > 4 {
		t.Fatalf("never above the task limit, saw %d", sim.maxActive)
	}
}

func TestAdaptiveKeepsStreamsThatEachAddThroughput(t *testing.T) {
	// Every connection is limited separately: four streams are four times faster.
	sim := newAdaptiveSim(t, []int{4}, 10<<30, func(n, _ int) float64 { return float64(n) * mib })
	sim.run(240)
	snap := sim.adaptive()
	if snap.Level != 4 || snap.Target != 4 {
		t.Fatalf("per-connection limited source must keep 4 streams, got %+v", snap)
	}
	if snap.Experiments != 1 {
		t.Fatalf("a confirmed top level needs exactly one lower test, got %d", snap.Experiments)
	}
	low := 0
	for _, n := range sim.trace {
		if n < 4 {
			low++
		}
	}
	// One lower test: about two settle periods plus two windows at half speed.
	if low > 16 {
		t.Fatalf("validation cost %d seconds below the limit", low)
	}
}

func TestAdaptiveDoesNotShedWhenItCostsThroughput(t *testing.T) {
	// Per-connection limit plus a total cap close to the four-stream rate: two
	// streams lose 7%, more than the 3% a shed may cost.
	sim := newAdaptiveSim(t, []int{4}, 10<<30, func(n, _ int) float64 {
		return min(float64(n)*0.2*mib, 0.43*mib)
	})
	sim.run(240)
	if snap := sim.adaptive(); snap.Level != 4 {
		t.Fatalf("shedding would cost more than 3%%: %+v", snap)
	}
}

func TestAdaptiveIgnoresFirstSecondBursts(t *testing.T) {
	// Newly added streams burst for one second, then the total stays capped.
	sim := newAdaptiveSim(t, []int{4}, 10<<30, func(n, sinceAdd int) float64 {
		if sinceAdd == 0 {
			return 10 * mib
		}
		return mib
	})
	sim.run(300)
	if snap := sim.adaptive(); snap.Level > 2 {
		t.Fatalf("bursts right after adding streams must not justify them: %+v", snap)
	}
}

func TestAdaptiveHoldsAfterFailedProbe(t *testing.T) {
	sim := newAdaptiveSim(t, []int{4}, 10<<30, func(n, _ int) float64 { return mib })
	sim.run(400)
	// After reaching one stream, upward probes to two streams fail and must be
	// spaced by at least the 30 second hold.
	var probes []int
	for i := 1; i < len(sim.trace); i++ {
		if sim.trace[i-1] == 1 && sim.trace[i] == 2 {
			probes = append(probes, i)
		}
	}
	if len(probes) < 2 {
		t.Fatalf("expected repeated upward probes, trace=%v", sim.trace)
	}
	for i := 1; i < len(probes); i++ {
		if gap := probes[i] - probes[i-1]; gap < 30 {
			t.Fatalf("probes %d seconds apart, want at least 30", gap)
		}
	}
}

func TestAdaptiveRateLimitEndsExperiment(t *testing.T) {
	sim := newAdaptiveSim(t, []int{4}, 10<<30, func(n, _ int) float64 { return float64(n) * mib })
	for i := 0; i < 60 && sim.adaptive().Phase != string(phaseLower); i++ {
		sim.step()
	}
	if sim.adaptive().Phase != string(phaseLower) {
		t.Fatal("expected a lower test to start")
	}
	sim.members[0].RateLimited(429, 5*time.Second, true)
	snap := sim.adaptive()
	if snap.Phase != string(phaseHold) || snap.Target != snap.Level {
		t.Fatalf("429 must end the experiment and restore the measured level: %+v", snap)
	}
}

func TestAdaptiveSkipsExperimentsForShortTransfers(t *testing.T) {
	// 30 MiB left at 1 MiB/s is 30 seconds, below the two-minute threshold.
	sim := newAdaptiveSim(t, []int{4}, 30*mib, func(n, _ int) float64 { return mib })
	sim.run(60)
	if snap := sim.adaptive(); snap.Experiments != 0 || snap.Target != 0 {
		t.Fatalf("short transfer must keep the plain ramp: %+v", snap)
	}
}

func TestAdaptiveGivesEveryTaskAStreamAndResetsOnJoin(t *testing.T) {
	sim := newAdaptiveSim(t, []int{4, 4}, 10<<30, func(n, _ int) float64 { return mib })
	sim.run(200)
	for _, m := range sim.members {
		if m.Snapshot().Active < 1 {
			t.Fatal("each running task keeps at least one stream")
		}
	}
	late := sim.b.Join(sim.key, 4, true)
	late.SetRemaining(10 << 30)
	sim.members = append(sim.members, late)
	if snap := sim.adaptive(); snap.Phase != string(phaseRamp) || snap.Target != 0 {
		t.Fatalf("a joining task rebuilds the windows and ramps freely: %+v", snap)
	}
	sim.step()
	if late.Snapshot().Active == 0 {
		t.Fatal("the new task gets streams immediately")
	}
}
