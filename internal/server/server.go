// Package server wires ingress, routing, execution and admin planes.
package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/IMV-IN/conduit/internal/canonical"
	"github.com/IMV-IN/conduit/internal/classify"
	"github.com/IMV-IN/conduit/internal/config"
	"github.com/IMV-IN/conduit/internal/health"
	"github.com/IMV-IN/conduit/internal/ledger"
	"github.com/IMV-IN/conduit/internal/provider"
	"github.com/IMV-IN/conduit/internal/router"
	"github.com/IMV-IN/conduit/internal/telemetry"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Gateway is the full service.
type Gateway struct {
	cfg      *config.Config
	router   *router.Router
	classify *classifierWrap
	ledger   *ledger.Store
	prov     *provider.Client
	hedgeSem chan struct{}
}

type classifierWrap struct{ c *classify.Classifier }

func New(cfg *config.Config) *Gateway {
	return &Gateway{
		cfg:      cfg,
		router:   router.New(cfg),
		classify: &classifierWrap{classify.New()},
		ledger:   ledger.New(100000),
		prov:     provider.NewClient(),
		hedgeSem: make(chan struct{}, 200),
	}
}

func (g *Gateway) Router() *router.Router { return g.router }
func (g *Gateway) Ledger() *ledger.Store  { return g.ledger }
func (g *Gateway) Config() *config.Config { return g.cfg }

// ---------- request parsing ----------

type chatMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatBody struct {
	Model          string         `json:"model"`
	Messages       []chatMsg      `json:"messages"`
	Tools          []any          `json:"tools"`
	Stream         bool           `json:"stream"`
	MaxTokens      int            `json:"max_tokens"`
	ResponseFormat any            `json:"response_format"`
	Metadata       map[string]any `json:"metadata"`
}

func estimateTokens(msgs []chatMsg) (in int, vision bool) {
	n := 0
	for _, m := range msgs {
		switch c := m.Content.(type) {
		case string:
			n += len(c) / 4
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						n += len(t) / 4
					}
					if _, ok := pm["image_url"]; ok {
						vision = true
						n += 1000
					}
				}
			}
		}
	}
	return n, vision
}

func (g *Gateway) auth(r *http.Request) (keyID, tenant, defPolicy string, ok bool) {
	if len(g.cfg.Auth.VirtualKeys) == 0 {
		return "dev", "default", "default", true
	}
	h := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(h, "Bearer ")
	tok = strings.TrimSpace(tok)
	for _, k := range g.cfg.Auth.VirtualKeys {
		if k.Key != "" && tok == k.Key {
			dp := k.DefaultPolicy
			if dp == "" {
				dp = "default"
			}
			return k.ID, k.Tenant, dp, true
		}
	}
	return "", "", "", false
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg, "type": code}})
}

// ---------- data plane ----------

func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if r.Method != "POST" {
		writeErr(w, 405, "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, g.maxBody()))
	if err != nil || len(body) == 0 {
		writeErr(w, 400, "invalid_request", "empty body")
		return
	}
	keyID, tenant, defPolicy, ok := g.auth(r)
	if !ok {
		writeErr(w, 401, "invalid_api_key", "bad virtual key")
		return
	}
	var cb chatBody
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&cb); err != nil {
		writeErr(w, 400, "invalid_request", "bad json")
		return
	}
	if cb.Model == "" {
		writeErr(w, 400, "invalid_request", "model required")
		return
	}
	inTok, vision := estimateTokens(cb.Messages)
	md := map[string]string{}
	for k, v := range cb.Metadata {
		md[k] = fmt.Sprint(v)
	}
	if h := r.Header.Get("x-conduit-task"); h != "" {
		md["task"] = h
	}
	if h := r.Header.Get("x-conduit-policy"); h != "" {
		md["policy"] = h
	}
	// Build canonical request (cheap: no re-marshal yet).
	creq := &canonical.Request{
		Tenant: tenant, KeyID: keyID, Requested: cb.Model,
		Tools: len(cb.Tools) > 0, Vision: vision,
		Stream: cb.Stream, MaxOutput: cb.MaxTokens,
		InTokensEst: inTok, Metadata: md, Raw: body,
	}
	if cb.ResponseFormat != nil {
		creq.JSONMode = true
	}
	for _, m := range cb.Messages {
		cm := canonical.Message{Role: m.Role, RawLen: 0}
		switch c := m.Content.(type) {
		case string:
			cm.Content = []canonical.Part{{Text: c}}
			cm.RawLen = len(c)
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						cm.Content = append(cm.Content, canonical.Part{Text: t})
					}
				}
			}
		}
		creq.Messages = append(creq.Messages, cm)
	}
	task, conf := g.classify.c.Classify(creq)
	creq.Task, creq.TaskConf = task, conf

	pol := g.router.ResolvePolicy(creq, defPolicy)
	// Per-request exploration kill-switch.
	effPol := pol
	if strings.ToLower(r.Header.Get("x-conduit-explore")) == "off" {
		cp := *pol
		cp.Exploration.Enabled = false
		effPol = &cp
	}
	routeT0 := time.Now()
	plan, rerr := g.router.Route(creq, effPol)
	telemetry.RouteDecisionSeconds.Observe(time.Since(routeT0).Seconds())
	if rerr != nil {
		// No feasible model: surface per-model reasons.
		writeErr(w, 422, "no_feasible_model", rerr.Error())
		return
	}
	// Strict pin: single attempt, no failover.
	if strings.ToLower(r.Header.Get("x-conduit-pin")) == "strict" && len(plan.Attempts) > 0 {
		plan.Attempts = plan.Attempts[:1]
	}

	result := g.execute(r.Context(), creq, plan, body)
	overhead := time.Since(t0) - result.upstreamSpent
	telemetry.GatewayOverheadSeconds.Observe(overhead.Seconds())

	if result.err != nil {
		if pe, ok := result.err.(*provider.Error); ok {
			switch pe.Class {
			case provider.ClassAuth:
				writeErr(w, 502, "provider_auth", "upstream auth failed")
			case provider.ClassContextExceeded:
				writeErr(w, 400, "context_length_exceeded", "upstream reports context exceeded")
			case provider.ClassClient:
				writeErr(w, 502, "provider_error", pe.Error())
			default:
				writeErr(w, 503, "all_providers_failed", failSummary(plan, pe))
			}
		} else {
			writeErr(w, 503, "all_providers_failed", result.err.Error())
		}
		g.recordDecision(plan, creq, nil, 0, 0, 0, 0, 503, result.attempts)
		return
	}

	// Common headers.
	w.Header().Set("X-Conduit-Model", result.modelID)
	w.Header().Set("X-Conduit-Task", creq.Task)
	w.Header().Set("X-Conduit-Decision-Id", plan.DecisionID)
	w.Header().Set("X-Conduit-Policy", plan.Policy)
	w.Header().Set("X-Conduit-Attempts", strconv.Itoa(result.attempts))
	w.Header().Set("X-Conduit-Cost-Usd", fmt.Sprintf("%.6f", result.costUSD))
	if len(plan.Relaxed) > 0 {
		w.Header().Set("X-Conduit-Relaxed", strings.Join(plan.Relaxed, ","))
	}
	if result.cached {
		w.Header().Set("X-Conduit-Cache", "hit")
	}

	if result.streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		if len(result.firstChunk) > 0 {
			_, _ = w.Write(result.firstChunk)
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.Copy(w, result.streamBody)
		result.streamBody.Close()
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(result.body)
	}
	g.recordDecision(plan, creq, result, result.inTok, result.outTok, result.costUSD, result.ttftMs, 200, result.attempts)
	telemetry.Requests.WithLabelValues(plan.Policy, result.modelID, creq.Task, "200").Inc()
	telemetry.CostUSD.WithLabelValues(result.modelID).Add(result.costUSD)
}

func failSummary(plan *router.Plan, last error) string {
	var names []string
	for _, a := range plan.Attempts {
		names = append(names, a.ModelID)
	}
	return "tried [" + strings.Join(names, ", ") + "]: " + last.Error()
}

func (g *Gateway) maxBody() int64 {
	if g.cfg.Server.MaxBodyBytes > 0 {
		return g.cfg.Server.MaxBodyBytes
	}
	return 8 << 20
}

// execResult is one completed upstream attempt (headers not yet sent).
type execResult struct {
	modelID       string
	body          []byte // non-streaming
	streaming     bool
	firstChunk    []byte
	streamBody    io.ReadCloser
	inTok, outTok int
	costUSD       float64
	ttftMs        float64
	attempts      int
	cached        bool
	upstreamSpent time.Duration
	ttftStats     float64 // TTFT for the latency EWMA; 0 = unknown (non-streaming totals would corrupt it)
	err           error
}

func spliceModel(raw []byte, upstream string) []byte {
	// Fast path: decode to generic map, swap model, re-encode.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	m["model"] = upstream
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}

func (g *Gateway) execute(ctx context.Context, creq *canonical.Request, plan *router.Plan, raw []byte) *execResult {
	totalT0 := time.Now()
	var lastErr error
	attempts := 0
	for i, a := range plan.Attempts {
		attempts = i + 1
		m := g.cfg.ModelByID(a.ModelID)
		if m == nil {
			continue
		}
		prov := g.cfg.ProviderByName(m.Provider)
		if prov == nil {
			continue
		}
		apiKey := ""
		if len(prov.APIKeys) > 0 {
			apiKey = prov.APIKeys[0]
		}
		upBody := spliceModel(raw, a.Upstream)
		res, err := g.tryOnce(ctx, prov.BaseURL, apiKey, upBody, creq.Stream, m)
		if err == nil {
			res.attempts = attempts
			res.modelID = a.ModelID
			res.upstreamSpent = time.Since(totalT0)
			g.router.ReportOutcome(a.ModelID, true, res.ttftStats)
			// Hedge accounting: if we hedged (i>0 due to slowness) note it.
			return res
		}
		lastErr = err
		g.router.ReportOutcome(a.ModelID, false, 0)
		if pe, ok := err.(*provider.Error); ok {
			if pe.Class == provider.ClassClient || pe.Class == provider.ClassAuth || pe.Class == provider.ClassRefusal {
				// Auth: quarantine-ish (breaker already fed). Client errors: do not retry same class?
				// For v0: client 400s are not retried (not provider fault) except context-exceeded
				// which retries a larger-context model (already ordered later in plan).
				if pe.Class == provider.ClassContextExceeded {
					continue
				}
				if pe.Class != provider.ClassRetryable {
					break
				}
			}
		}
		// Provider-diverse skip: if this was a connection-level failure, skip same-provider models.
		// (Plan is already provider-diverse first, so just continue.)
		select {
		case <-ctx.Done():
			return &execResult{err: ctx.Err(), attempts: attempts, upstreamSpent: time.Since(totalT0)}
		default:
		}
	}
	return &execResult{err: lastErr, attempts: attempts, upstreamSpent: time.Since(totalT0)}
}

// tryOnce performs one upstream call with first-token gating.
// For hedge/slow faults the caller runs attempts sequentially; hedging (concurrent)
// is handled by tryHedged when plan.HedgeDelay > 0 and streaming.
func (g *Gateway) tryOnce(ctx context.Context, baseURL, apiKey string, body []byte, stream bool, m *config.Model) (*execResult, error) {
	t0 := time.Now()
	resp, err := g.prov.DoChat(ctx, baseURL, apiKey, body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		_ = provider.DrainAndClassify(resp)
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		resp.Body.Close()
		full := append([]byte{}, b...)
		// Reconstruct minimal body for classifier (status line lost, but body kept).
		_ = full
		// Need status-aware classification: re-read via helper using captured body.
		perr := provider.ClassifyStatus(resp.StatusCode, b)
		if perr == nil {
			perr = &provider.Error{Class: provider.ClassRetryable, HTTPStatus: resp.StatusCode, Err: fmt.Errorf("upstream %d", resp.StatusCode)}
		}
		return nil, perr
	}
	if !stream {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, &provider.Error{Class: provider.ClassRetryable, Err: err}
		}
		ttft := float64(time.Since(t0).Microseconds()) / 1000
		inTok, outTok := parseUsage(b)
		cost := costOf(m, inTok, outTok, creqInFallback(body))
		// Non-streaming completion time is NOT time-to-first-token; keep it
		// for the ledger/cost path but out of the latency EWMA (ttftStats=0).
		return &execResult{body: b, inTok: inTok, outTok: outTok, costUSD: cost, ttftMs: ttft}, nil
	}
	// Streaming: buffer until first SSE data line (first-token gate) — nothing
	// is sent to the client before this returns.
	br := bufio.NewReader(resp.Body)
	var first bytes.Buffer
	deadline := time.Now().Add(60 * time.Second)
	_ = deadline
	for {
		line, err := br.ReadBytes('\n')
		first.Write(line)
		trim := bytes.TrimSpace(line)
		if bytes.HasPrefix(trim, []byte("data:")) && len(trim) > 6 {
			break
		}
		if err != nil {
			break
		}
		if first.Len() > 256*1024 {
			break
		}
	}
	ttft := float64(time.Since(t0).Microseconds()) / 1000
	// Wrap remaining stream: first buffered bytes + rest.
	rest := &chainedReadCloser{first: first.Bytes(), r: resp.Body, br: br}
	// Best-effort usage: unknown until end; estimate for cost header.
	inTok := creqInFallback(body)
	outTok := 48
	cost := costOf(m, inTok, outTok, inTok)
	return &execResult{streaming: true, firstChunk: first.Bytes(), streamBody: rest, inTok: inTok, outTok: outTok, costUSD: cost, ttftMs: ttft, ttftStats: ttft}, nil
}

func creqInFallback(body []byte) int {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return 20
	}
	n := 0
	if msgs, ok := m["messages"].([]any); ok {
		for _, mm := range msgs {
			if mp, ok := mm.(map[string]any); ok {
				if s, ok := mp["content"].(string); ok {
					n += len(s) / 4
				}
			}
		}
	}
	if n == 0 {
		n = 20
	}
	return n
}

func parseUsage(b []byte) (in, out int) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return 20, 32
	}
	if u, ok := m["usage"].(map[string]any); ok {
		in = int(num(u["prompt_tokens"]))
		out = int(num(u["completion_tokens"]))
		if in == 0 && out == 0 {
			in = int(num(u["input_tokens"]))
			out = int(num(u["output_tokens"]))
		}
		if in+out > 0 {
			return in, out
		}
	}
	// fallback: estimate from content length
	if ch, ok := m["choices"].([]any); ok && len(ch) > 0 {
		if c0, ok := ch[0].(map[string]any); ok {
			if msg, ok := c0["message"].(map[string]any); ok {
				if s, ok := msg["content"].(string); ok {
					out = max(1, len(s)/4)
				}
			}
		}
	}
	if out == 0 {
		out = 32
	}
	return 20, out
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case int64:
		return float64(t)
	}
	return 0
}

func costOf(m *config.Model, in, out, _ int) float64 {
	return float64(in)/1e6*m.PricePerMtok.Input + float64(out)/1e6*m.PricePerMtok.Output
}

type chainedReadCloser struct {
	first []byte
	off   int
	r     io.ReadCloser
	br    *bufio.Reader
}

func (c *chainedReadCloser) Read(p []byte) (int, error) {
	if c.off < len(c.first) {
		n := copy(p, c.first[c.off:])
		c.off += n
		return n, nil
	}
	return c.br.Read(p)
}

func (c *chainedReadCloser) Close() error { return c.r.Close() }

func (g *Gateway) recordDecision(plan *router.Plan, creq *canonical.Request, res *execResult, inTok, outTok int, cost, ttft float64, status, attempts int) {
	rec := &ledger.Record{
		DecisionID: plan.DecisionID, Ts: time.Now(),
		Tenant: creq.Tenant, KeyID: creq.KeyID, Policy: plan.Policy,
		Task: creq.Task, TaskConf: creq.TaskConf,
		Propensity: plan.Propensity, Explored: plan.Explored,
		Status: status, TTFTMs: ttft, InTokens: inTok, OutTokens: outTok,
		CostUSD: cost, Relaxed: plan.Relaxed,
	}
	if len(plan.Attempts) > 0 {
		rec.Chosen = plan.Attempts[0].ModelID
	}
	for _, c := range plan.Candidates {
		if c.Model == nil {
			continue
		}
		rec.Candidates = append(rec.Candidates, ledger.Candidate{
			Model: c.Model.ID, Q: c.Q, C: c.C, L: c.L, R: c.R, U: c.U,
			Frontier: c.Frontier, Reject: c.Reject,
		})
	}
	for _, a := range plan.Attempts[:min(len(plan.Attempts), attempts)] {
		st := status
		if res == nil || res.err != nil {
			st = 503
		}
		rec.Attempts = append(rec.Attempts, ledger.Attempt{Model: a.ModelID, Status: st, TTFTMs: ttft})
	}
	g.ledger.Append(rec)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- other data-plane endpoints ----------

func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	type m struct {
		ID string `json:"id"`
	}
	out := []m{{ID: "auto"}, {ID: "auto:cheap"}, {ID: "auto:fast"}, {ID: "auto:best"}, {ID: "auto:balanced"}}
	for _, mm := range g.cfg.Models {
		out = append(out, m{ID: mm.ID})
	}
	for _, p := range g.cfg.Policies {
		out = append(out, m{ID: "policy:" + p.Name})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": out, "object": "list"})
}

func (g *Gateway) handleFeedback(w http.ResponseWriter, r *http.Request) {
	var fb struct {
		DecisionID string   `json:"decision_id"`
		Reward     *float64 `json:"reward"`
		Label      string   `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&fb); err != nil || fb.DecisionID == "" {
		writeErr(w, 400, "invalid_request", "decision_id required")
		return
	}
	rec, ok := g.ledger.Get(fb.DecisionID)
	if !ok {
		// Unknown decision (e.g. static baseline before ledger write race): accept anyway.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	reward := 0.5
	if fb.Reward != nil {
		reward = *fb.Reward
	} else if fb.Label == "good" {
		reward = 1
	} else if fb.Label == "bad" {
		reward = 0
	}
	g.router.Feedback(rec.Task, rec.Chosen, reward)
	g.ledger.SetReward(fb.DecisionID, reward)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (g *Gateway) handleRoute(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, g.maxBody()))
	var cb chatBody
	if err := json.Unmarshal(body, &cb); err != nil || cb.Model == "" {
		writeErr(w, 400, "invalid_request", "bad body")
		return
	}
	_, _, defPolicy, _ := g.auth(r)
	if defPolicy == "" {
		defPolicy = "default"
	}
	inTok, vision := estimateTokens(cb.Messages)
	creq := &canonical.Request{Requested: cb.Model, Tools: len(cb.Tools) > 0, Vision: vision, MaxOutput: cb.MaxTokens, InTokensEst: inTok, Metadata: map[string]string{}}
	for _, m := range cb.Messages {
		cm := canonical.Message{Role: m.Role}
		switch c := m.Content.(type) {
		case string:
			cm.Content = []canonical.Part{{Text: c}}
		case []any:
			for _, p := range c {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"].(string); ok {
						cm.Content = append(cm.Content, canonical.Part{Text: t})
					}
				}
			}
		}
		creq.Messages = append(creq.Messages, cm)
	}
	t, conf := g.classify.c.Classify(creq)
	creq.Task, creq.TaskConf = t, conf
	pol := g.router.ResolvePolicy(creq, defPolicy)
	plan, err := g.router.Route(creq, pol)
	if err != nil {
		writeErr(w, 422, "no_feasible_model", err.Error())
		return
	}
	type cand struct {
		Model string  `json:"model"`
		U     float64 `json:"utility"`
		P     float64 `json:"-"`
	}
	var cands []cand
	for _, c := range plan.Candidates {
		if c.Model == nil {
			continue
		}
		cands = append(cands, cand{Model: c.Model.ID, U: c.U})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"decision_id": plan.DecisionID, "policy": plan.Policy,
		"task": creq.Task, "task_conf": conf,
		"attempts": plan.Attempts, "propensity": plan.Propensity,
		"explored": plan.Explored, "candidates": cands,
	})
}

// ---------- admin plane ----------

func (g *Gateway) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(g.cfg.Auth.AdminKeys) == 0 || (len(g.cfg.Auth.AdminKeys) == 1 && g.cfg.Auth.AdminKeys[0] == "") {
			next(w, r)
			return
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		for _, k := range g.cfg.Auth.AdminKeys {
			if k != "" && tok == strings.TrimSpace(k) {
				next(w, r)
				return
			}
		}
		writeErr(w, 401, "unauthorized", "bad admin key")
	}
}

func (g *Gateway) handleRuntime(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"heap_alloc_mb": float64(m.HeapAlloc) / 1024 / 1024,
		"goroutines":    runtime.NumGoroutine(),
	})
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	out := []map[string]any{}
	for _, m := range g.cfg.Models {
		b := g.router.Breaker(m.ID)
		v := 0
		switch b.State() {
		case health.HalfOpen:
			v = 1
		case health.Open:
			v = 2
		}
		telemetry.BreakerState.WithLabelValues(m.ID).Set(float64(v))
		out = append(out, map[string]any{"model": m.ID, "state": b.State().String()})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"breakers": out})
}

func (g *Gateway) handleDecisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res := g.ledger.Search(q.Get("task"), q.Get("model"), q.Get("policy"), 100)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": res, "count": len(res)})
}

func (g *Gateway) handleDecisionOne(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/v1/decisions/")
	id = strings.SplitN(id, "/", 2)[0]
	rec, ok := g.ledger.Get(id)
	if !ok {
		writeErr(w, 404, "not_found", "unknown decision")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rec)
}

func (g *Gateway) handleQuality(w http.ResponseWriter, r *http.Request) {
	task := r.URL.Query().Get("task")
	if task == "" {
		task = "chat"
	}
	rows := []map[string]any{}
	for _, m := range g.cfg.Models {
		b := g.router.Beta(task, m.ID)
		mean, sd, n := b.MeanSD(nowUTC(), halfLife())
		rows = append(rows, map[string]any{"task": task, "model": m.ID, "mean": mean, "sd": sd, "n": n})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
}

// ---------- listeners ----------

func (g *Gateway) DataMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("POST /v1/feedback", g.handleFeedback)
	mux.HandleFunc("POST /v1/route", g.handleRoute)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.Handle("GET /metrics", promhttp.Handler())
	return mux
}

func (g *Gateway) AdminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/v1/runtime", g.handleRuntime)
	mux.HandleFunc("GET /admin/v1/health", g.adminAuth(g.handleHealth))
	mux.HandleFunc("GET /admin/v1/decisions", g.adminAuth(g.handleDecisions))
	mux.HandleFunc("GET /admin/v1/decisions/", g.adminAuth(g.handleDecisionOne))
	mux.HandleFunc("GET /admin/v1/stats/quality", g.adminAuth(g.handleQuality))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.Handle("GET /metrics", promhttp.Handler())
	// Embedded console placeholder until ui/ is built.
	mux.HandleFunc("GET /console/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<h1>Conduit console (see ui/design.md; React app lands in Phase 5)</h1>`))
	})
	return mux
}
