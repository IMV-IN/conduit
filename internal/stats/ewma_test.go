package stats

import "testing"

func TestEWMAReset(t *testing.T) {
	e := &EWMA{}
	for i := 0; i < 20; i++ {
		e.Add(6000)
	}
	if v, n := e.Get(); n != 20 || v < 5000 {
		t.Fatalf("expected converged EWMA, got v=%v n=%v", v, n)
	}
	e.Reset(900)
	if v, n := e.Get(); n != 0 || v != 900 {
		t.Fatalf("expected reset to prior, got v=%v n=%v", v, n)
	}
}
