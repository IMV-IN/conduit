package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/IMV-IN/conduit/internal/canonical"
	"github.com/IMV-IN/conduit/internal/config"
	"github.com/IMV-IN/conduit/internal/router"
)

// scriptedUpstream is a fake provider: programmable delay, status and body.
type scriptedUpstream struct {
	delay  time.Duration
	status int
}

func (s scriptedUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(s.delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		if s.status == 200 {
			_, _ = fmt.Fprint(w, `{"id":"t","choices":[{"index":0,"finish_reason":"stop",`+
				`"message":{"role":"assistant","content":"hello"}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		} else {
			_, _ = fmt.Fprint(w, `{"error":{"code":"server_error","message":"boom","type":"server_error"}}`)
		}
	}
}

func testGateway(t *testing.T, a, b scriptedUpstream) (*Gateway, *httptest.Server, *httptest.Server) {
	t.Helper()
	sa := httptest.NewServer(a.handler())
	sb := httptest.NewServer(b.handler())
	t.Cleanup(sa.Close)
	t.Cleanup(sb.Close)
	cfg := &config.Config{
		Providers: []config.Provider{
			{Name: "pa", BaseURL: sa.URL, APIKeys: []string{"k"}},
			{Name: "pb", BaseURL: sb.URL, APIKeys: []string{"k"}},
		},
		Models: []config.Model{
			{ID: "m/a", Provider: "pa", Upstream: "a",
				LatencyPrior: config.LatencyPrior{TTFTMs: 100, TokensPerSec: 100}},
			{ID: "m/b", Provider: "pb", Upstream: "b",
				LatencyPrior: config.LatencyPrior{TTFTMs: 100, TokensPerSec: 100}},
		},
		TaskClasses: []string{"chat"},
		Policies: []config.Policy{
			{Name: "t", Objective: "balanced",
				Hedging:     config.Hedging{Enabled: true, MinDelayMs: 50, MaxShare: 1},
				Reliability: config.Reliability{MaxAttempts: 3}},
		},
	}
	return New(cfg), sa, sb
}

func testPlan(hedgeDelay time.Duration, share float64) *router.Plan {
	return &router.Plan{
		DecisionID: "d_test", Policy: "t",
		Attempts: []router.Attempt{
			{ModelID: "m/a", Provider: "pa", Upstream: "a"},
			{ModelID: "m/b", Provider: "pb", Upstream: "b"},
		},
		HedgeDelay: hedgeDelay, HedgeMaxShare: share,
		Task: "chat",
	}
}

func testBody() []byte {
	return []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`)
}

func testReq() *canonical.Request {
	return &canonical.Request{Requested: "auto", Task: "chat"}
}

// A hedged fallback that answers first must win, fast, with both launched.
func TestHedgeWins(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 500 * time.Millisecond, status: 200},
		scriptedUpstream{delay: 0, status: 200})
	t0 := time.Now()
	res := gw.execute(context.Background(), testReq(), testPlan(50*time.Millisecond, 1.0), testBody())
	el := time.Since(t0)
	if res.err != nil {
		t.Fatalf("expected winner, got %v", res.err)
	}
	if res.modelID != "m/b" {
		t.Fatalf("hedge should win, got %s", res.modelID)
	}
	if res.attempts != 2 {
		t.Fatalf("expected 2 launched attempts, got %d", res.attempts)
	}
	if el > 450*time.Millisecond {
		t.Fatalf("hedge took %v, expected <450ms", el)
	}
}

// No hedge delay: sequential semantics, primary wins alone.
func TestPrimaryWinsNoHedge(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 0, status: 200},
		scriptedUpstream{delay: 500 * time.Millisecond, status: 200})
	res := gw.execute(context.Background(), testReq(), testPlan(0, 1.0), testBody())
	if res.err != nil || res.modelID != "m/a" || res.attempts != 1 {
		t.Fatalf("got model=%s attempts=%d err=%v", res.modelID, res.attempts, res.err)
	}
}

// Retryable errors still fail over when hedging is off.
func TestFailoverOnError(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 0, status: 500},
		scriptedUpstream{delay: 0, status: 200})
	res := gw.execute(context.Background(), testReq(), testPlan(0, 1.0), testBody())
	if res.err != nil || res.modelID != "m/b" || res.attempts != 2 {
		t.Fatalf("got model=%s attempts=%d err=%v", res.modelID, res.attempts, res.err)
	}
}

// Zero hedge budget: no speculative duplicate even when the primary is slow.
// (The bucket starts full by design, so the test drains it first to simulate
// a sustained over-share state.)
func TestHedgeBudgetCaps(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 250 * time.Millisecond, status: 200},
		scriptedUpstream{delay: 0, status: 200})
	gw.hedges.mu.Lock()
	gw.hedges.tokens = 0
	gw.hedges.mu.Unlock()
	t0 := time.Now()
	res := gw.execute(context.Background(), testReq(), testPlan(20*time.Millisecond, 0), testBody())
	el := time.Since(t0)
	if res.err != nil || res.modelID != "m/a" || res.attempts != 1 {
		t.Fatalf("got model=%s attempts=%d err=%v", res.modelID, res.attempts, res.err)
	}
	if el < 200*time.Millisecond {
		t.Fatalf("primary should have run to completion (%v)", el)
	}
}

// A fresh gateway hedges immediately: the bucket starts full so the first
// incident is covered before income could accumulate.
func TestHedgeBudgetStartsFull(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 500 * time.Millisecond, status: 200},
		scriptedUpstream{delay: 0, status: 200})
	t0 := time.Now()
	res := gw.execute(context.Background(), testReq(), testPlan(50*time.Millisecond, 0.05), testBody())
	el := time.Since(t0)
	if res.err != nil || res.modelID != "m/b" || res.attempts != 2 {
		t.Fatalf("got model=%s attempts=%d err=%v", res.modelID, res.attempts, res.err)
	}
	if el > 450*time.Millisecond {
		t.Fatalf("hedge took %v, expected <450ms", el)
	}
}

// Client errors are terminal: no failover to the next model.
func TestClientErrorTerminal(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 0, status: 400},
		scriptedUpstream{delay: 0, status: 200})
	res := gw.execute(context.Background(), testReq(), testPlan(0, 1.0), testBody())
	if res.err == nil {
		t.Fatal("expected terminal client error")
	}
	if res.attempts != 1 {
		t.Fatalf("must not fail over on 400, attempts=%d", res.attempts)
	}
}

func streamReq() *canonical.Request {
	return &canonical.Request{Requested: "auto", Task: "chat", Stream: true}
}

// Non-streaming requests hedge on completion time (HedgeDelayTotal), not
// first-token time: a slow-completing primary inside its total budget must
// NOT trigger a speculative duplicate.
func TestNonStreamUsesTotalDelay(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 300 * time.Millisecond, status: 200},
		scriptedUpstream{delay: 0, status: 200})
	plan := testPlan(50*time.Millisecond, 1.0)
	plan.HedgeDelayTotal = 5 * time.Second
	res := gw.execute(context.Background(), testReq(), plan, testBody())
	if res.err != nil || res.modelID != "m/a" || res.attempts != 1 {
		t.Fatalf("got model=%s attempts=%d err=%v", res.modelID, res.attempts, res.err)
	}
}

// Streaming requests hedge on first-token time (HedgeDelay): the same slow
// primary MUST be hedged when streaming.
func TestStreamUsesFirstTokenDelay(t *testing.T) {
	gw, _, _ := testGateway(t,
		scriptedUpstream{delay: 300 * time.Millisecond, status: 200},
		scriptedUpstream{delay: 0, status: 200})
	plan := testPlan(50*time.Millisecond, 1.0)
	plan.HedgeDelayTotal = 5 * time.Second
	// Scripted bodies are JSON, not SSE; the first data line arrives with the
	// full body, so first-token time == completion time for the fixture.
	res := gw.execute(context.Background(), streamReq(), plan, testBody())
	if res.err != nil {
		t.Fatalf("expected winner, got %v", res.err)
	}
	if res.modelID != "m/b" || res.attempts != 2 {
		t.Fatalf("streaming should hedge on first-token delay: model=%s attempts=%d",
			res.modelID, res.attempts)
	}
	if len(res.firstChunk) == 0 || res.streamBody == nil {
		t.Fatal("winner must carry a gated stream")
	}
	res.streamBody.Close()
}
