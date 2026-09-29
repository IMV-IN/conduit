// Package health implements circuit breakers and a rolling outcome window.
package health

import (
	"sync"
	"time"
)

// State is the breaker state.
type State int32

const (
	Closed State = iota
	Open
	HalfOpen
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// BreakerCfg configures a breaker.
type BreakerCfg struct {
	ConsecFails    int
	MinRequests    int
	ErrRate        float64
	OpenFor        time.Duration
	HalfOpenProbes int
}

func DefaultCfg() BreakerCfg {
	return BreakerCfg{ConsecFails: 5, MinRequests: 20, ErrRate: 0.5, OpenFor: 10 * time.Second, HalfOpenProbes: 3}
}

type outcome struct {
	t  time.Time
	ok bool
}

// Breaker is a per-model circuit breaker.
type Breaker struct {
	mu       sync.Mutex
	cfg      BreakerCfg
	state    State
	fails    int
	win      []outcome
	openedAt time.Time
	trips    int
	probes   int
	errs     int64
	total    int64
}

func NewBreaker(cfg BreakerCfg) *Breaker { return &Breaker{cfg: cfg} }

func (b *Breaker) backoff() time.Duration {
	d := b.cfg.OpenFor << min(b.trips, 5)
	if d <= 0 {
		d = b.cfg.OpenFor
	}
	return d
}

func (b *Breaker) Allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if now.Sub(b.openedAt) >= b.backoff() {
			b.state, b.probes = HalfOpen, 0
		} else {
			return false
		}
		fallthrough
	case HalfOpen:
		if b.probes < b.cfg.HalfOpenProbes {
			b.probes++
			return true
		}
		return false
	}
	return false
}

func (b *Breaker) Report(now time.Time, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.win = append(b.win, outcome{now, ok})
	cutoff := now.Add(-60 * time.Second)
	i := 0
	for i < len(b.win) && b.win[i].t.Before(cutoff) {
		i++
	}
	b.win = append([]outcome(nil), b.win[i:]...)
	b.total++
	if !ok {
		b.errs++
	}
	if ok {
		b.fails = 0
		if b.state == HalfOpen {
			b.state, b.trips = Closed, 0
		}
		return
	}
	b.fails++
	n := len(b.win)
	errs := 0
	for _, o := range b.win {
		if !o.ok {
			errs++
		}
	}
	rate := 0.0
	if n > 0 {
		rate = float64(errs) / float64(n)
	}
	if b.state == HalfOpen || b.fails >= b.cfg.ConsecFails ||
		(n >= b.cfg.MinRequests && rate >= b.cfg.ErrRate) {
		if b.state != Open {
			b.trips++
		}
		b.state, b.openedAt = Open, now
	}
}

func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// ErrRate returns rolling error rate and count for risk estimation.
func (b *Breaker) ErrRate() (errs, total int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.errs, b.total
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
