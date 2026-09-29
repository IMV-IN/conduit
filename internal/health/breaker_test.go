package health

import (
	"testing"
	"time"
)

func TestBreakerTripsOnConsecFails(t *testing.T) {
	b := NewBreaker(BreakerCfg{ConsecFails: 3, MinRequests: 100, ErrRate: 0.9, OpenFor: time.Minute, HalfOpenProbes: 1})
	now := time.Now()
	for i := 0; i < 3; i++ {
		b.Report(now, false)
	}
	if b.State() != Open {
		t.Fatal("breaker should be open after 3 consecutive failures")
	}
	if b.Allow(now) {
		t.Fatal("open breaker must not allow traffic")
	}
}

func TestBreakerHalfOpenRecovery(t *testing.T) {
	b := NewBreaker(BreakerCfg{ConsecFails: 1, MinRequests: 100, ErrRate: 0.9, OpenFor: 10 * time.Millisecond, HalfOpenProbes: 1})
	now := time.Now()
	b.Report(now, false)
	if b.State() != Open {
		t.Fatal("should be open")
	}
	later := now.Add(time.Second)
	if !b.Allow(later) {
		t.Fatal("should admit a half-open probe after backoff")
	}
	b.Report(later, true)
	if b.State() != Closed {
		t.Fatal("successful probe should close the breaker")
	}
}

func TestBreakerSuccessResetsConsec(t *testing.T) {
	b := NewBreaker(BreakerCfg{ConsecFails: 3, MinRequests: 100, ErrRate: 0.9, OpenFor: time.Minute, HalfOpenProbes: 1})
	now := time.Now()
	b.Report(now, false)
	b.Report(now, false)
	b.Report(now, true)
	b.Report(now, false)
	b.Report(now, false)
	if b.State() != Closed {
		t.Fatal("success should reset the consecutive-failure count")
	}
}
