// Package stats holds Beta posteriors with lazy time-decay, Wilson bounds,
// and a tiny EWMA helper. All types are goroutine-safe where noted.
package stats

import (
	"math"
	"sync"
	"time"
)

// Beta tracks quality for one (task, model) pair.
type Beta struct {
	mu    sync.Mutex
	A, B  float64
	Stamp time.Time
}

func NewBeta(prior float64, strength float64, now time.Time) *Beta {
	if strength <= 0 {
		strength = 20
	}
	if prior <= 0 {
		prior = 0.5
	}
	if prior >= 1 {
		prior = 0.99
	}
	return &Beta{A: prior * strength, B: (1 - prior) * strength, Stamp: now}
}

func (b *Beta) decayLocked(now time.Time, halfLife time.Duration) {
	if b.Stamp.IsZero() {
		b.Stamp = now
		return
	}
	if halfLife <= 0 {
		return
	}
	dt := float64(now.Sub(b.Stamp))
	if dt <= 0 {
		return
	}
	g := math.Pow(0.5, dt/float64(halfLife))
	b.A *= g
	b.B *= g
	b.Stamp = now
}

// Update adds reward r in [0,1].
func (b *Beta) Update(r float64, now time.Time, halfLife time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decayLocked(now, halfLife)
	if r < 0 {
		r = 0
	}
	if r > 1 {
		r = 1
	}
	b.A += r
	b.B += 1 - r
}

// MeanSD returns posterior mean and sd (with kappa-ready evidence count).
func (b *Beta) MeanSD(now time.Time, halfLife time.Duration) (mean, sd, n float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.decayLocked(now, halfLife)
	s := b.A + b.B
	if s <= 0 {
		return 0.5, 0.25, 0
	}
	mean = b.A / s
	v := b.A * b.B / (s * s * (s + 1))
	if v < 0 {
		v = 0
	}
	return mean, math.Sqrt(v), s
}

// Merge adds another Beta's counts (commutative/associative after same-instant decay).
func (b *Beta) Merge(o *Beta, now time.Time, halfLife time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o.mu.Lock()
	defer o.mu.Unlock()
	b.decayLocked(now, halfLife)
	o.decayLocked(now, halfLife)
	b.A += o.A
	b.B += o.B
}

// WilsonUB returns the Wilson upper bound of an error rate.
func WilsonUB(errs, n int64, z float64) float64 {
	if n == 0 {
		return 0.5
	}
	if z == 0 {
		z = 1.96
	}
	p := float64(errs) / float64(n)
	den := 1 + z*z/float64(n)
	center := p + z*z/(2*float64(n))
	margin := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n*n)))
	ub := (center + margin) / den
	if ub > 1 {
		ub = 1
	}
	return ub
}

// EWMA is a simple exponentially-weighted moving average.
type EWMA struct {
	mu    sync.Mutex
	V     float64
	N     int64
	Alpha float64
}

func (e *EWMA) Add(x float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Alpha == 0 {
		e.Alpha = 0.2
	}
	if e.N == 0 {
		e.V = x
	} else {
		e.V = e.Alpha*x + (1-e.Alpha)*e.V
	}
	e.N++
}

func (e *EWMA) Get() (float64, int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.V, e.N
}
