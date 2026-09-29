// Mock LLM provider for testing Conduit.
//
// Speaks the OpenAI chat-completions dialect (streaming and non-streaming) and lets tests
// control latency, throughput, error rates, context limits, tool support and per-task
// "quality" at runtime via /__control endpoints.
//
// Quality simulation: the reply text ends with "ANSWER:OK" with probability
// quality[task] and "ANSWER:WRONG" otherwise. The task comes from request.metadata.eval_task.
// This lets k6 measure end-to-end correctness through any gateway.
//
//	go run ./test/mockprovider -addr :9001
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Profile struct {
	TTFTMedianMs  float64            `json:"ttft_median_ms"`
	TTFTSigma     float64            `json:"ttft_sigma"`     // lognormal sigma; 0 = constant
	TokensPerSec  float64            `json:"tokens_per_sec"` // 0 = instant
	OutputTokens  int                `json:"output_tokens"`
	ErrorRate     float64            `json:"error_rate"`      // -> HTTP 500
	RateLimitRate float64            `json:"rate_limit_rate"` // -> HTTP 429
	Down          bool               `json:"down"`            // -> HTTP 503
	ContextTokens int                `json:"context_tokens"`  // 0 = unlimited
	Tools         bool               `json:"tools"`
	Quality       map[string]float64 `json:"quality"` // task -> P(correct); "default" fallback
}

var (
	mu       sync.RWMutex
	profiles = map[string]Profile{}
)

func defaults() map[string]Profile {
	return map[string]Profile{
		"premium": {TTFTMedianMs: 900, TTFTSigma: 0.35, TokensPerSec: 60, OutputTokens: 48, ErrorRate: 0.003, Tools: true,
			Quality: map[string]float64{"default": 0.93, "code": 0.96, "reasoning_math": 0.96, "chat": 0.95}},
		"balanced": {TTFTMedianMs: 500, TTFTSigma: 0.30, TokensPerSec: 90, OutputTokens: 48, ErrorRate: 0.003, Tools: true,
			Quality: map[string]float64{"default": 0.85, "code": 0.86, "reasoning_math": 0.80, "chat": 0.92, "summarization": 0.90, "translation": 0.90}},
		"fast": {TTFTMedianMs: 200, TTFTSigma: 0.30, TokensPerSec: 180, OutputTokens: 48, ErrorRate: 0.003, Tools: true,
			Quality: map[string]float64{"default": 0.70, "code": 0.55, "reasoning_math": 0.45, "chat": 0.90, "classification_short": 0.94, "extraction_structured": 0.88, "summarization": 0.86, "translation": 0.85}},
		"tiny": {TTFTMedianMs: 100, TTFTSigma: 0.25, TokensPerSec: 250, OutputTokens: 32, ErrorRate: 0.003, ContextTokens: 4000,
			Quality: map[string]float64{"default": 0.50, "classification_short": 0.85, "chat": 0.70}},
		"notools": {TTFTMedianMs: 300, TTFTSigma: 0.30, TokensPerSec: 120, OutputTokens: 48, ErrorRate: 0.003, Tools: false,
			Quality: map[string]float64{"default": 0.80}},
	}
}

func reset() {
	mu.Lock()
	defer mu.Unlock()
	profiles = defaults()
}

type chatReq struct {
	Model     string           `json:"model"`
	Stream    bool             `json:"stream"`
	MaxTokens int              `json:"max_tokens"`
	Tools     []any            `json:"tools"`
	Messages  []map[string]any `json:"messages"`
	Metadata  map[string]any   `json:"metadata"`
}

func promptTokens(msgs []map[string]any) int {
	n := 0
	for _, m := range msgs {
		if s, ok := m["content"].(string); ok {
			n += len(s) / 4
		} else if parts, ok := m["content"].([]any); ok {
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if s, ok := pm["text"].(string); ok {
						n += len(s) / 4
					}
				}
			}
		}
	}
	return n
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg, "type": code}})
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func chat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, 400, "invalid_request", "bad json")
		return
	}
	mu.RLock()
	p, ok := profiles[req.Model]
	mu.RUnlock()
	if !ok {
		apiErr(w, 404, "model_not_found", "unknown model "+req.Model)
		return
	}

	// Fault injection (evaluated before any latency so failures are fast unless configured otherwise).
	if p.Down {
		apiErr(w, 503, "overloaded", "model is down")
		return
	}
	if rand.Float64() < p.ErrorRate {
		apiErr(w, 500, "server_error", "injected failure")
		return
	}
	if rand.Float64() < p.RateLimitRate {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("x-ratelimit-remaining-tokens", "0")
		apiErr(w, 429, "rate_limit_exceeded", "injected rate limit")
		return
	}

	// Strict validation so tests can detect routing-constraint violations.
	pt := promptTokens(req.Messages)
	if p.ContextTokens > 0 && pt > p.ContextTokens {
		apiErr(w, 400, "context_length_exceeded", fmt.Sprintf("prompt %d > limit %d", pt, p.ContextTokens))
		return
	}
	if len(req.Tools) > 0 && !p.Tools {
		apiErr(w, 400, "tools_unsupported", "model does not support tools")
		return
	}

	// TTFT
	ttft := p.TTFTMedianMs
	if p.TTFTSigma > 0 {
		ttft = p.TTFTMedianMs * math.Exp(p.TTFTSigma*rand.NormFloat64())
	}
	if !sleepCtx(r.Context(), time.Duration(ttft*float64(time.Millisecond))) {
		return
	}

	// Output with simulated correctness
	task, _ := req.Metadata["eval_task"].(string)
	q, found := p.Quality[task]
	if !found {
		q = p.Quality["default"]
		if q == 0 {
			q = 0.5
		}
	}
	verdict := "ANSWER:WRONG"
	if rand.Float64() < q {
		verdict = "ANSWER:OK"
	}
	n := p.OutputTokens
	if n <= 0 {
		n = 32
	}
	if req.MaxTokens > 0 && req.MaxTokens < n {
		n = req.MaxTokens
	}
	words := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		words = append(words, "tok")
	}
	words = append(words, verdict)

	id := fmt.Sprintf("chatcmpl-mock-%d", rand.Int63())
	created := time.Now().Unix()
	usage := map[string]any{"prompt_tokens": pt, "completion_tokens": n, "total_tokens": pt + n}
	perTok := time.Duration(0)
	if p.TokensPerSec > 0 {
		perTok = time.Duration(float64(time.Second) / p.TokensPerSec)
	}

	if !req.Stream {
		if !sleepCtx(r.Context(), perTok*time.Duration(n)) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": strings.Join(words, " ")}}},
			"usage": usage,
		})
		return
	}

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if fl != nil {
			fl.Flush()
		}
	}
	for i, wd := range words {
		if i > 0 && !sleepCtx(r.Context(), perTok) {
			return
		}
		txt := wd
		if i > 0 {
			txt = " " + wd
		}
		emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": txt}}}})
	}
	emit(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// POST /__control/profile?model=premium   body: partial Profile JSON (merged)
func setProfile(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	cur, ok := profiles[model]
	if !ok {
		http.Error(w, "unknown model", 404)
		return
	}
	b, _ := json.Marshal(cur)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k, v := range patch {
		m[k] = v
	}
	b2, _ := json.Marshal(m)
	var np Profile
	if err := json.Unmarshal(b2, &np); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	profiles[model] = np
	w.WriteHeader(204)
}

func main() {
	addr := flag.String("addr", ":9001", "listen address")
	flag.Parse()
	reset()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", chat)
	mux.HandleFunc("POST /__control/profile", setProfile)
	mux.HandleFunc("POST /__control/reset", func(w http.ResponseWriter, _ *http.Request) { reset(); w.WriteHeader(204) })
	mux.HandleFunc("GET /__control/profiles", func(w http.ResponseWriter, _ *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(profiles)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	log.Printf("mock provider listening on %s", *addr)
	log.Fatal((&http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
}
