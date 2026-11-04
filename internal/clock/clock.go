// Package clock lets the agent run on a real clock in production and on an
// accelerated, per-machine skewed clock in the simulator.
package clock

import "time"

type Clock interface{ Now() time.Time }

type Real struct{}

func (Real) Now() time.Time { return time.Now() }

// Sim runs `Speed` times faster than real time, starting at Start.
type Sim struct {
	Start     time.Time
	Speed     float64
	realStart time.Time
}

func NewSim(start time.Time, speed float64) *Sim {
	return &Sim{Start: start, Speed: speed, realStart: time.Now()}
}

func (s *Sim) Now() time.Time {
	el := time.Since(s.realStart)
	return s.Start.Add(time.Duration(float64(el) * s.Speed))
}

// Skewed models a machine RTC that drifts from true time.
type Skewed struct {
	Base Clock
	Skew time.Duration
}

func (k Skewed) Now() time.Time { return k.Base.Now().Add(k.Skew) }
