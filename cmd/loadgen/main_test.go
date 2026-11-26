package main

import (
	"math"
	"reflect"
	"testing"
)

func scenario() *Scenario {
	return &Scenario{Name: "t", Seed: 7, Arrivals: "poisson", Phases: []Phase{
		{Type: "fixed", DurationS: 10, RPS: 100},
		{Type: "ramp", DurationS: 10, FromRPS: 100, ToRPS: 300},
		{Type: "burst", DurationS: 10, RPS: 50, BurstRPS: 400, BurstEveryS: 5, BurstLenS: 1},
	}, Mix: Mix{LongFraction: 0.2, LongDelayMs: 500}}
}

func TestScheduleDeterministicForSeed(t *testing.T) {
	a, b := buildSchedule(scenario(), 0), buildSchedule(scenario(), 0)
	if !reflect.DeepEqual(a, b) || len(a) == 0 {
		t.Fatal("same seed must give the same schedule")
	}
	s2 := scenario()
	s2.Seed = 8
	if reflect.DeepEqual(a, buildSchedule(s2, 0)) {
		t.Fatal("different seeds must differ")
	}
}

func countIn(sched []Arrival, lo, hi float64) int {
	n := 0
	for _, a := range sched {
		if a.T >= lo && a.T < hi {
			n++
		}
	}
	return n
}

func TestOfferedLoadFollowsPhases(t *testing.T) {
	s := scenario()
	sched := buildSchedule(s, 0)
	// fixed 100 rps for 10 s ~ 1000 (Poisson: +-15%)
	if n := countIn(sched, 0, 10); math.Abs(float64(n)-1000) > 150 {
		t.Errorf("fixed phase: %d", n)
	}
	// ramp 100->300 over 10 s: average 200 -> ~2000
	if n := countIn(sched, 10, 20); math.Abs(float64(n)-2000) > 300 {
		t.Errorf("ramp phase: %d", n)
	}
	// burst: 400 rps for 1 s of every 5 + 50 rps otherwise -> 2*400 + 8*50 = 1200
	if n := countIn(sched, 20, 30); math.Abs(float64(n)-1200) > 250 {
		t.Errorf("burst phase: %d", n)
	}
	// the burst second must be denser than the quiet second
	if countIn(sched, 20, 21) < 2*countIn(sched, 22, 23) {
		t.Error("burst second is not denser")
	}
}

func TestUniformArrivalsAreEvenlySpaced(t *testing.T) {
	s := &Scenario{Seed: 1, Arrivals: "uniform", Phases: []Phase{{Type: "fixed", DurationS: 2, RPS: 50}}}
	sched := buildSchedule(s, 0)
	if len(sched) < 98 || len(sched) > 100 {
		t.Fatalf("expected ~99 arrivals, got %d", len(sched))
	}
	for i := 1; i < len(sched); i++ {
		if math.Abs((sched[i].T-sched[i-1].T)-0.02) > 1e-9 {
			t.Fatal("uniform arrivals must be 20 ms apart")
		}
	}
}

func TestMixedDurations(t *testing.T) {
	sched := buildSchedule(scenario(), 0)
	long := 0
	for _, a := range sched {
		if a.Long {
			long++
			if a.DelayMs != 500 {
				t.Fatal("long request must carry the long delay")
			}
		}
	}
	if f := float64(long) / float64(len(sched)); math.Abs(f-0.2) > 0.03 {
		t.Fatalf("long fraction %.3f, want ~0.2", f)
	}
}

func TestScaleCompressesTime(t *testing.T) {
	s := scenario()
	s.Faults = []FaultEvent{{AtS: 20, Backend: "Backend-1", Set: map[string]any{"down": true}}}
	s.scale(2)
	if s.duration() != 15 || s.Faults[0].AtS != 10 {
		t.Fatalf("duration=%v fault=%v", s.duration(), s.Faults[0].AtS)
	}
}

func TestValidationRejectsBadScenarios(t *testing.T) {
	bad := []func(*Scenario){
		func(s *Scenario) { s.Phases = nil },
		func(s *Scenario) { s.Phases[0].DurationS = 0 },
		func(s *Scenario) { s.Phases[0].Type = "sine" },
		func(s *Scenario) { s.Phases[0].RPS = 1e9 },
		func(s *Scenario) { s.Mix.LongFraction = 2 },
		func(s *Scenario) { s.Arrivals = "magic" },
		func(s *Scenario) { s.Faults = []FaultEvent{{AtS: 999, Backend: "B", Set: map[string]any{"x": 1}}} },
		func(s *Scenario) { s.Faults = []FaultEvent{{AtS: 1, Backend: "", Set: map[string]any{"x": 1}}} },
		func(s *Scenario) { s.Phases[2].BurstLenS = 99 },
	}
	for i, m := range bad {
		s := scenario()
		m(s)
		if s.validate() == nil {
			t.Errorf("case %d should be rejected", i)
		}
	}
	if err := scenario().validate(); err != nil {
		t.Fatal(err)
	}
}
