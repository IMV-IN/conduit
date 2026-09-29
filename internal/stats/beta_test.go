package stats

import (
	"math"
	"testing"
	"time"
)

func TestBetaMeanSD(t *testing.T) {
	b := NewBeta(0.8, 20, time.Now())
	mean, sd, n := b.MeanSD(time.Now(), 0) // halfLife 0 = no decay
	if math.Abs(mean-0.8) > 1e-9 {
		t.Fatalf("mean %v", mean)
	}
	if math.Abs(n-20) > 1e-9 {
		t.Fatalf("n %v", n)
	}
	_ = sd
}

func TestBetaUpdateMovesMean(t *testing.T) {
	now := time.Now()
	b := NewBeta(0.5, 10, now)
	b.Update(1, now, 24*time.Hour)
	mean, _, _ := b.MeanSD(now, 24*time.Hour)
	if mean <= 0.5 {
		t.Fatalf("positive reward should raise mean, got %v", mean)
	}
}

func TestBetaDecayForgets(t *testing.T) {
	now := time.Now()
	b := NewBeta(0.9, 100, now)
	// ~13 half-lives later the old mass (~0.01) is negligible vs one new observation.
	later := now.Add(90 * 24 * time.Hour)
	b.Update(0, later, 7*24*time.Hour) // old mass mostly decayed; new 0 dominates
	mean, _, _ := b.MeanSD(later, 7*24*time.Hour)
	if mean > 0.5 {
		t.Fatalf("after heavy decay + failure, mean should drop, got %v", mean)
	}
}

func TestWilsonUB(t *testing.T) {
	if ub := WilsonUB(0, 0, 0); ub != 0.5 {
		t.Fatalf("empty window should be 0.5, got %v", ub)
	}
	ub := WilsonUB(50, 100, 1.96)
	if ub < 0.5 || ub > 0.65 {
		t.Fatalf("50%% errors should give UB ~0.6, got %v", ub)
	}
}
