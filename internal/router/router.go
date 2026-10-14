// Package router implements hard filters, utility scoring, guarded
// exploration and plan building. Score/Select are pure and table-testable.
package router

import (
	"crypto/sha256"
	"encoding/binary"
	"hash/fnv"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IMV-IN/conduit/internal/canonical"
	"github.com/IMV-IN/conduit/internal/config"
	"github.com/IMV-IN/conduit/internal/health"
	"github.com/IMV-IN/conduit/internal/stats"
)

// Weights for the utility score.
type Weights struct{ Quality, Cost, Latency, Risk float64 }

func weightsFor(p *config.Policy) Weights {
	if len(p.Weights) == 4 {
		return Weights{p.Weights["quality"], p.Weights["cost"], p.Weights["latency"], p.Weights["risk"]}
	}
	switch p.Objective {
	case "best":
		return Weights{0.80, 0.02, 0.08, 0.10}
	case "fast":
		return Weights{0.30, 0.10, 0.55, 0.05}
	case "cheap":
		return Weights{0.30, 0.55, 0.10, 0.05}
	default: // balanced
		return Weights{0.50, 0.20, 0.20, 0.10}
	}
}

// Candidate is one model evaluated for a request.
type Candidate struct {
	Model    *config.Model
	Q, C, L  float64 // quality, USD, ms
	R        float64
	cn, ln   float64
	U        float64
	Reject   string
	Frontier bool
}

// Score normalises cost (log) and latency (minmax) across survivors and ranks by U.
func Score(all []*Candidate, w Weights) []*Candidate {
	var ranked []*Candidate
	for _, c := range all {
		if c.Reject == "" {
			ranked = append(ranked, c)
		}
	}
	if len(ranked) == 0 {
		return nil
	}
	cLo, cHi := math.Inf(1), math.Inf(-1)
	lLo, lHi := math.Inf(1), math.Inf(-1)
	for _, c := range ranked {
		lc := math.Log(c.C + 1e-6)
		cLo, cHi = math.Min(cLo, lc), math.Max(cHi, lc)
		lLo, lHi = math.Min(lLo, c.L), math.Max(lHi, c.L)
	}
	norm := func(x, lo, hi float64) float64 {
		if hi-lo < 1e-12 {
			return 0
		}
		return (x - lo) / (hi - lo)
	}
	for _, c := range ranked {
		c.cn = norm(math.Log(c.C+1e-6), cLo, cHi)
		c.ln = norm(c.L, lLo, lHi)
		c.U = w.Quality*c.Q - w.Cost*c.cn - w.Latency*c.ln - w.Risk*c.R
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].U > ranked[j].U })
	markFrontier(ranked)
	return ranked
}

func markFrontier(ranked []*Candidate) {
	for i, c := range ranked {
		dom := false
		for j, o := range ranked {
			if i == j {
				continue
			}
			if o.Q >= c.Q && o.C <= c.C && o.L <= c.L &&
				(o.Q > c.Q || o.C < c.C || o.L < c.L) {
				dom = true
				break
			}
		}
		c.Frontier = !dom
	}
}

// Explore params.
type Explore struct{ Epsilon, Delta, Tau float64 }

// Select draws from p(a) = (1-eps)*guardedSoftmax + eps/n, returning exact propensity.
// u must be uniform in [0,1).
func Select(ranked []*Candidate, ex Explore, u float64) (int, float64) {
	n := len(ranked)
	if n == 1 {
		return 0, 1
	}
	if ex.Tau <= 0 {
		ex.Tau = 0.05
	}
	best := ranked[0].U
	w := make([]float64, n)
	var z float64
	for i, c := range ranked {
		if best-c.U <= ex.Delta {
			w[i] = math.Exp((c.U - best) / ex.Tau)
			z += w[i]
		}
	}
	if z == 0 {
		w[0], z = 1, 1
	}
	acc := 0.0
	for i := range ranked {
		p := (1-ex.Epsilon)*w[i]/z + ex.Epsilon/float64(n)
		acc += p
		if u < acc {
			return i, p
		}
	}
	last := n - 1
	return last, (1-ex.Epsilon)*w[last]/z + ex.Epsilon/float64(n)
}

// exploreScale narrows exploration when the caller stated an explicit quality
// intent. "best" means "don't experiment on me": the guard shrinks to 0.2x
// (≈2pp of quality with best weights, safely below the typical winner gap so
// near-ties don't leak in on floating-point boundaries) and the epsilon
// floor halves. Other objectives keep the policy's configured values.
func exploreScale(objective string) float64 {
	if objective == "best" {
		return 0.2
	}
	return 1.0
}

// SeedFromID hashes a decision ID to a uniform in [0,1) (reproducible).
func SeedFromID(id string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	v := h.Sum64()
	// 53 bits of randomness -> [0,1)
	return float64(v>>11) / float64(1<<53)
}

func hexEnc(b []byte) string {
	const hexd = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, v := range b {
		out = append(out, hexd[v>>4], hexd[v&0xf])
	}
	return string(out)
}

// Attempt is one ordered fallback.
type Attempt struct {
	ModelID  string
	Provider string
	Upstream string
}

// Plan is the router output.
type Plan struct {
	DecisionID string
	Attempts   []Attempt
	HedgeDelay time.Duration
	// HedgeDelayTotal pads HedgeDelay with the primary's expected output
	// time. Streaming hedges on first-token time (HedgeDelay); non-streaming
	// requests can only hedge on completion, so the executor uses the
	// larger of the two. Computed from priors, never from live latency
	// (which would delay the hedge exactly when it is needed most).
	HedgeDelayTotal time.Duration
	// HedgeMaxShare caps speculative hedged requests as a fraction of
	// traffic (0.05 = 5%). The executor enforces it with a token bucket.
	HedgeMaxShare float64
	// HedgeBurst sizes the executor's token bucket (see hedgeBudget).
	HedgeBurst int
	Relaxed    []string
	Candidates []*Candidate
	Propensity float64
	Explored   bool
	Chosen     int
	Policy     string
	Task       string
	TaskConf   float64
	EstCost    float64
}

// Router holds live state: betas, breakers, EWMAs.
type Router struct {
	cfg      *config.Config
	betas    map[string]*stats.Beta // "task\x00model" -> Beta
	breakers map[string]*health.Breaker
	ewmaTTFT map[string]*stats.EWMA
	rr       map[string]int         // round-robin cursors (guarded by mu)
	eject    map[string]time.Time   // model -> slow-ejection expiry (guarded by mu)
	probe    map[string]*probeState // model -> post-eject trickle probe (guarded by mu)
	hedged   map[string][]time.Time // model -> recent hedge-timer firings as primary (guarded by mu)
	primary  map[string][]time.Time // model -> recent primary selections (guarded by mu)
	mu       sync.Mutex
	halfLife time.Duration
}

func New(cfg *config.Config) *Router {
	r := &Router{
		cfg:      cfg,
		betas:    map[string]*stats.Beta{},
		breakers: map[string]*health.Breaker{},
		ewmaTTFT: map[string]*stats.EWMA{},
		rr:       map[string]int{},
		eject:    map[string]time.Time{},
		probe:    map[string]*probeState{},
		hedged:   map[string][]time.Time{},
		primary:  map[string][]time.Time{},
		halfLife: 7 * 24 * time.Hour,
	}
	now := time.Now()
	for _, m := range cfg.Models {
		r.breakers[m.ID] = health.NewBreaker(health.DefaultCfg())
		r.ewmaTTFT[m.ID] = &stats.EWMA{}
		strength := m.PriorStrength
		if strength == 0 {
			strength = 20
		}
		for _, t := range cfg.TaskClasses {
			p := m.Priors[t]
			if p == 0 {
				p = m.Priors["default"]
			}
			if p == 0 {
				p = 0.6
			}
			r.betas[t+"\x00"+m.ID] = stats.NewBeta(p, strength, now)
		}
		// ensure "chat" fallback exists
		if _, ok := r.betas["chat\x00"+m.ID]; !ok {
			r.betas["chat\x00"+m.ID] = stats.NewBeta(0.6, strength, now)
		}
	}
	return r
}

func (r *Router) Beta(task, model string) *stats.Beta {
	k := task + "\x00" + model
	if b, ok := r.betas[k]; ok {
		return b
	}
	b := stats.NewBeta(0.6, 20, time.Now())
	r.betas[k] = b
	return b
}

func (r *Router) Breaker(model string) *health.Breaker {
	if b, ok := r.breakers[model]; ok {
		return b
	}
	b := health.NewBreaker(health.DefaultCfg())
	r.breakers[model] = b
	return b
}

func (r *Router) Feedback(task, model string, reward float64) {
	r.Beta(task, model).Update(reward, time.Now(), r.halfLife)
}

func (r *Router) ReportOutcome(model string, ok bool, ttftMs float64) {
	r.Breaker(model).Report(time.Now(), ok)
	if ok && ttftMs > 0 {
		r.ewma(model).Add(ttftMs)
	}
}

// ResolvePolicy picks a policy for a request.
func (r *Router) ResolvePolicy(req *canonical.Request, defaultPolicy string) *config.Policy {
	if strings.HasPrefix(req.Requested, "policy:") {
		if p := r.cfg.PolicyByName(strings.TrimPrefix(req.Requested, "policy:")); p != nil {
			return p
		}
	}
	if v, ok := req.Metadata["policy"]; ok && v != "" {
		if p := r.cfg.PolicyByName(v); p != nil {
			return p
		}
	}
	// CEL match: support the subset used in example configs
	// (request.metadata.app == "support"). Full cel-go lands in Phase 3.
	for i := range r.cfg.Policies {
		p := &r.cfg.Policies[i]
		if p.Match != "" && celMatchSubset(p.Match, req.Metadata) {
			return p
		}
	}
	if p := r.cfg.PolicyByName(defaultPolicy); p != nil {
		return p
	}
	return &r.cfg.Policies[0]
}

func celMatchSubset(expr string, md map[string]string) bool {
	// supports: request.metadata.<k> == "<v>"
	expr = strings.TrimSpace(expr)
	if !strings.Contains(expr, "==") {
		return false
	}
	parts := strings.SplitN(expr, "==", 2)
	lhs := strings.TrimSpace(parts[0])
	rhs := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
	key := strings.TrimPrefix(lhs, "request.metadata.")
	if key == lhs {
		return false
	}
	return md[key] == rhs
}

// ObjectiveOf maps requested model alias to (policy override, objective).
func ObjectiveOf(requested string) string {
	switch requested {
	case "auto:cheap":
		return "cheap"
	case "auto:fast":
		return "fast"
	case "auto:best":
		return "best"
	case "auto:balanced", "auto":
		return "balanced"
	}
	if strings.HasPrefix(requested, "auto:") {
		return "balanced"
	}
	return ""
}

// Route builds a Plan for req under policy p.
func (r *Router) Route(req *canonical.Request, p *config.Policy) (*Plan, error) {
	// Static strategies (baselines).
	if p.Strategy == "fixed" && p.Model != "" {
		r.notePrimary(p.Model)
		return &Plan{
			DecisionID: newDecisionID(req),
			Attempts:   []Attempt{{ModelID: p.Model, Provider: providerOf(r.cfg, p.Model), Upstream: upstreamOf(r.cfg, p.Model)}},
			Propensity: 1, Policy: p.Name, Task: req.Task, TaskConf: req.TaskConf,
			Candidates: []*Candidate{{Model: r.cfg.ModelByID(p.Model), Q: 1, U: 1}},
		}, nil
	}
	if p.Strategy == "round_robin" && len(p.Models) > 0 {
		r.mu.Lock()
		i := r.rr[p.Name] % len(p.Models)
		r.rr[p.Name]++
		r.mu.Unlock()
		mid := p.Models[i]
		var attempts []Attempt
		for k := 0; k < len(p.Models); k++ {
			m := p.Models[(i+k)%len(p.Models)]
			attempts = append(attempts, Attempt{ModelID: m, Provider: providerOf(r.cfg, m), Upstream: upstreamOf(r.cfg, m)})
		}
		_ = mid
		r.notePrimary(mid)
		return &Plan{
			DecisionID: newDecisionID(req), Attempts: attempts,
			Propensity: 1.0 / float64(len(p.Models)), Policy: p.Name, Task: req.Task, TaskConf: req.TaskConf,
		}, nil
	}
	// Pinned provider/model passthrough.
	if m := r.cfg.ModelByID(req.Requested); m != nil {
		r.notePrimary(m.ID)
		return &Plan{
			DecisionID: newDecisionID(req),
			Attempts:   []Attempt{{ModelID: m.ID, Provider: m.Provider, Upstream: m.Upstream}},
			Propensity: 1, Policy: p.Name, Task: req.Task, TaskConf: req.TaskConf,
		}, nil
	}

	obj := ObjectiveOf(req.Requested)
	eff := *p
	if obj != "" {
		eff.Objective = obj
	}
	if eff.Objective == "" {
		eff.Objective = "balanced"
	}
	w := weightsFor(&eff)

	now := time.Now()
	all := make([]*Candidate, 0, len(r.cfg.Models))
	for i := range r.cfg.Models {
		m := &r.cfg.Models[i]
		c := &Candidate{Model: m}
		if reason := r.filter(req, m, &eff, now); reason != "" {
			c.Reject = reason
		} else {
			c.Q, c.C, c.L, c.R = r.estimate(req, m, now)
		}
		all = append(all, c)
	}
	ranked := Score(all, w)
	relaxed := []string{}
	if len(ranked) == 0 {
		// Relax SLO filters (latency/cost/quality) but never capability/context.
		relaxed = []string{"slo"}
		saved := eff.Constraints
		eff.Constraints = config.Constraints{}
		for _, c := range all {
			c.Reject = ""
			if reason := r.filterStrict(req, c.Model, &eff, now); reason != "" {
				c.Reject = reason
			} else {
				c.Q, c.C, c.L, c.R = r.estimate(req, c.Model, now)
			}
		}
		_ = saved
		ranked = Score(all, w)
		if len(ranked) == 0 {
			return &Plan{DecisionID: newDecisionID(req), Policy: eff.Name, Task: req.Task, Candidates: all}, errNoFeasible(all)
		}
	}

	ex := Explore{Epsilon: eff.Exploration.Epsilon, Delta: eff.Exploration.Delta, Tau: eff.Exploration.Tau}
	if !eff.Exploration.Enabled {
		ex.Epsilon, ex.Delta = 0, 0
	}
	// An explicit auto:<objective> intent scales exploration (see exploreScale).
	if s := exploreScale(obj); s != 1.0 {
		ex.Delta *= s
		ex.Epsilon *= 0.5
	}
	if ex.Tau <= 0 {
		ex.Tau = 0.05
	}
	if ex.Delta <= 0 && ex.Epsilon > 0 {
		ex.Delta = 0.08
	}
	// Exploration off per request header is handled by caller (sets Enabled=false copy).
	did := newDecisionID(req)
	u := SeedFromID(did)
	idx, prop := Select(ranked, ex, u)
	explored := idx != 0

	// Provider-diverse fallbacks ordered by U.
	var attempts []Attempt
	seenProv := map[string]bool{}
	attempts = append(attempts, Attempt{ModelID: ranked[idx].Model.ID, Provider: ranked[idx].Model.Provider, Upstream: ranked[idx].Model.Upstream})
	seenProv[ranked[idx].Model.Provider] = true
	maxAtt := eff.Reliability.MaxAttempts
	if maxAtt <= 0 {
		maxAtt = 3
	}
	for _, c := range ranked {
		if len(attempts) >= maxAtt {
			break
		}
		if c.Model.ID == ranked[idx].Model.ID {
			continue
		}
		if seenProv[c.Model.Provider] {
			continue
		}
		attempts = append(attempts, Attempt{ModelID: c.Model.ID, Provider: c.Model.Provider, Upstream: c.Model.Upstream})
		seenProv[c.Model.Provider] = true
	}
	// Fill remaining slots ignoring provider diversity.
	for _, c := range ranked {
		if len(attempts) >= maxAtt {
			break
		}
		dup := false
		for _, a := range attempts {
			if a.ModelID == c.Model.ID {
				dup = true
			}
		}
		if !dup {
			attempts = append(attempts, Attempt{ModelID: c.Model.ID, Provider: c.Model.Provider, Upstream: c.Model.Upstream})
		}
	}

	hd := time.Duration(0)
	hs := 0.05
	if eff.Hedging.Enabled && eff.Hedging.MinDelayMs > 0 {
		hd = time.Duration(eff.Hedging.MinDelayMs) * time.Millisecond
	}
	if eff.Hedging.MaxShare > 0 {
		hs = eff.Hedging.MaxShare
	}
	hb := eff.Hedging.Burst
	if hb <= 0 {
		hb = 100
	}
	// Non-streaming hedge delay pads first-token time with the primary's
	// expected output time (priors, not live estimates).
	hdTotal := hd
	if hd > 0 && len(ranked) > idx {
		tps := ranked[idx].Model.LatencyPrior.TokensPerSec
		if tps <= 0 {
			tps = 80
		}
		eOut := 48.0
		if req.MaxOutput > 0 && float64(req.MaxOutput) < eOut {
			eOut = float64(req.MaxOutput)
		}
		hdTotal = hd + time.Duration(eOut/tps*1000)*time.Millisecond
	}
	if len(attempts) > 0 {
		r.notePrimary(attempts[0].ModelID)
	}
	return &Plan{
		DecisionID: did, Attempts: attempts, HedgeDelay: hd, HedgeMaxShare: hs,
		HedgeBurst:      hb,
		HedgeDelayTotal: hdTotal, Relaxed: relaxed,
		Candidates: all, Propensity: prop, Explored: explored, Chosen: idx,
		Policy: eff.Name, Task: req.Task, TaskConf: req.TaskConf,
		EstCost: ranked[idx].C,
	}, nil
}

func (r *Router) filter(req *canonical.Request, m *config.Model, p *config.Policy, now time.Time) string {
	if s := r.filterStrict(req, m, p, now); s != "" {
		return s
	}
	// SLO filters (relaxable).
	if p.Constraints.MaxCostUSD > 0 {
		_, c, _, _ := r.estimate(req, m, now)
		if c > p.Constraints.MaxCostUSD {
			return "budget SLO"
		}
	}
	if p.Constraints.MaxLatencyMs > 0 {
		_, _, l, _ := r.estimate(req, m, now)
		if l > float64(p.Constraints.MaxLatencyMs) {
			return "latency SLO"
		}
	}
	if p.Constraints.MinQuality > 0 {
		q, _, _, _ := r.estimate(req, m, now)
		if q < p.Constraints.MinQuality {
			return "quality SLO"
		}
	}
	return ""
}

func (r *Router) filterStrict(req *canonical.Request, m *config.Model, p *config.Policy, now time.Time) string {
	// Context fit with 10% safety margin.
	need := float64(req.InTokensEst)*1.10 + float64(reservedOut(req.MaxOutput))
	if m.ContextTokens > 0 && need > float64(m.ContextTokens) {
		return "context 4k"
	}
	if req.Tools && !m.HasCap("tools") {
		return "capability tools"
	}
	if req.Vision && !m.HasCap("vision") {
		return "capability vision"
	}
	if !r.Breaker(m.ID).Allow(now) {
		return "open breaker"
	}
	if reason := r.slowReject(m, now); reason != "" {
		return reason
	}
	if len(p.AllowModels) > 0 && !globAny(p.AllowModels, m.ID) {
		return "allow-list"
	}
	for _, g := range p.DenyModels {
		if ok, _ := path.Match(g, m.ID); ok || g == m.ID {
			return "deny-list"
		}
	}
	if len(p.Regions) > 0 && len(m.Regions) > 0 {
		hit := false
		for _, a := range p.Regions {
			for _, b := range m.Regions {
				if a == b {
					hit = true
				}
			}
		}
		if !hit {
			return "residency"
		}
	}
	return ""
}

// ewma returns the TTFT tracker for a model, creating it on demand.
func (r *Router) ewma(id string) *stats.EWMA {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.ewmaTTFT[id]
	if !ok {
		e = &stats.EWMA{}
		r.ewmaTTFT[id] = e
	}
	return e
}

// notePrimary records a primary selection for the ejection rate gate.
func (r *Router) notePrimary(model string) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primary[model] = append(pruneBefore(r.primary[model], now.Add(-time.Minute)), now)
}

func (r *Router) primariesRecent(model string, now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primary[model] = pruneBefore(r.primary[model], now.Add(-time.Minute))
	return len(r.primary[model])
}

// ReportHedgeLaunched records that a hedge timer fired against this model
// as primary — i.e. it exceeded its hedge delay. The launch itself is the
// slowness signal (we cannot wait for the outcome: re-ejection must precede
// the wave of slow completions, not follow it). A single firing means little
// (healthy p99 exceedances happen); repeated firings eject the model.
// Calibrate min_delay above healthy p99 first-token latency so healthy
// traffic rarely fires.
func (r *Router) ReportHedgeLaunched(model string) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hedged[model] = append(pruneBefore(r.hedged[model], now.Add(-time.Minute)), now)
}

func pruneBefore(l []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(l) && l[i].Before(cut) {
		i++
	}
	return append([]time.Time(nil), l[i:]...)
}

// EjectUntil reports when a model's slow-ejection lifts (for the health API).
func (r *Router) EjectUntil(model string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	until, ok := r.eject[model]
	return until, ok
}

func (r *Router) hedgedRecent(model string, now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hedged[model] = pruneBefore(r.hedged[model], now.Add(-time.Minute))
	return len(r.hedged[model])
}

// hedgedSince counts firings within an explicit window (no pruning side
// effect beyond it). Used for the burst rule below.
func (r *Router) hedgedSince(model string, now time.Time, window time.Duration) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	cut := now.Add(-window)
	for _, t := range r.hedged[model] {
		if !t.Before(cut) {
			n++
		}
	}
	return n
}

// probeState is a post-ejection trickle probe. Re-admitting a still-slow
// model to full traffic would recreate the storm the ejection contained, so
// the model is re-admitted at 1-in-K filter checks while fresh evidence
// forms. Quota bounds the total admitted probes; the verdict comes from
// that evidence, not the quota itself. Slow probes hedge (their timer
// fires); healed probes complete cleanly (no launches).
type probeState struct {
	quota int
	used  int
	since time.Time
	seen  int
}

const probeQuota = 12

// probeTrickle admits 1-in-K checks while probing. At 30 rps this sends
// ~3 probes/s: enough for a verdict in seconds, cheap enough that every
// probe hedges within budget.
const probeTrickle = 10

// peersDistressed reports whether any other model currently looks slow
// itself: EWMA TTFT ≥1.5× its prior over ≥3 samples. Uniform slowdowns (a
// slow CI runner, regional degradation) raise every model's latency
// together; ejecting on absolute thresholds then just churns routing
// without improving it — 02 caught exactly this on shared CI hardware
// (best_prefers_premium_for_code collapsed to 44% with zero errors).
// Models without samples are skipped: with nothing to compare against,
// absolute evidence stands, so cold-start detection still works.
func (r *Router) peersDistressed(mID string) bool {
	for i := range r.cfg.Models {
		o := &r.cfg.Models[i]
		if o.ID == mID || o.LatencyPrior.TTFTMs <= 0 {
			continue
		}
		if v, n := r.ewma(o.ID).Get(); n >= 3 && v > 1.5*o.LatencyPrior.TTFTMs {
			return true
		}
	}
	return false
}

// slowReject implements latency ejection (DESIGN §8.1): a model is ejected
// for 15 s when either (a) its measured first-token latency stays above 3x
// its prior over ≥10 samples, (b) its hedge timer fired ≥25 times in 10 s
// (correlated slowness; the floor sits ~3x above healthy exceedances at
// 30 rps so variance alone cannot trip it), or (c) its firings sustain ≥5%
// of its primary selections. On expiry the model enters a trickle probe
// (1-in-10 checks, quota 12): three hedge launches re-eject it fast, a
// spent quota with clean evidence graduates it. Recovery is therefore
// bounded by one probe, and sustained slow-faults cost a few hedged probes
// per 15 s cycle — inside the hedge budget.
func (r *Router) slowReject(m *config.Model, now time.Time) string {
	base := m.LatencyPrior.TTFTMs
	if base <= 0 {
		return ""
	}
	eject := func() string {
		// Uniform-slowness guard: ejecting one model while its peers are
		// equally slow just churns. Per-request hedging (budget-capped)
		// still rescues individually slow requests.
		if r.peersDistressed(m.ID) {
			return ""
		}
		r.mu.Lock()
		r.eject[m.ID] = now.Add(15 * time.Second)
		delete(r.hedged, m.ID)
		delete(r.primary, m.ID)
		delete(r.probe, m.ID)
		r.mu.Unlock()
		return "slow: latency ejection"
	}
	e := r.ewma(m.ID)
	v, n := e.Get()
	r.mu.Lock()
	until, ok := r.eject[m.ID]
	r.mu.Unlock()
	if ok {
		if now.Before(until) {
			return "slow: latency ejection"
		}
		// Expired: reset evidence and open a bounded trickle probe. The
		// reset makes the model rank first so probes actually select it;
		// the quota bounds the cost if it is still slow.
		r.mu.Lock()
		delete(r.eject, m.ID)
		delete(r.hedged, m.ID)
		delete(r.primary, m.ID)
		r.probe[m.ID] = &probeState{quota: probeQuota, since: now}
		r.mu.Unlock()
		e.Reset(base)
	}
	r.mu.Lock()
	pr, probing := r.probe[m.ID]
	r.mu.Unlock()
	if probing {
		// Fast path: fresh launches already proved continued slowness.
		// Launches are counted from probe start, so pre-probe history
		// cannot trip this.
		r.mu.Lock()
		since := pr.since
		r.mu.Unlock()
		if r.hedgedSince(m.ID, now, now.Sub(since)) >= 3 {
			return eject()
		}
		// Trickle admission: only 1-in-K checks may select the model.
		// The reset above makes it rank first, so admitted checks track
		// selections ~1:1 (minus exploration picks of others).
		r.mu.Lock()
		pr.seen++
		admit := pr.seen%probeTrickle == 1
		over := pr.used >= pr.quota
		if admit && !over {
			pr.used++
		}
		r.mu.Unlock()
		if over {
			// Quota spent: judge by fresh evidence. Slow completions
			// backstop a starved hedge budget (no launches observable).
			if v, n := e.Get(); n >= 3 && v > 3*base {
				return eject()
			}
			r.mu.Lock()
			delete(r.probe, m.ID)
			r.mu.Unlock()
			return "" // probes ran clean: graduate to full traffic
		}
		if !admit {
			return "slow: probing"
		}
		return ""
	}
	if n >= 10 && v > 3*base {
		return eject()
	}
	if r.hedgedSince(m.ID, now, 10*time.Second) >= 25 {
		return eject()
	}
	lh := r.hedgedRecent(m.ID, now)
	if lh >= 5 && float64(lh)/float64(max(1, r.primariesRecent(m.ID, now))) >= 0.05 {
		return eject()
	}
	return ""
}

func reservedOut(maxOut int) int {
	if maxOut <= 0 {
		return 512
	}
	return maxOut
}

func globAny(patterns []string, id string) bool {
	for _, g := range patterns {
		if ok, _ := path.Match(g, id); ok || g == id {
			return true
		}
		// support "*/mini*" style against full id
		if strings.Contains(g, "*") {
			// crude: prefix/suffix match
			trim := strings.ReplaceAll(g, "*", "")
			if strings.Contains(id, trim) {
				return true
			}
		}
	}
	return false
}

// estimate returns (Q, C USD, L ms, R).
func (r *Router) estimate(req *canonical.Request, m *config.Model, now time.Time) (float64, float64, float64, float64) {
	// Q: Beta mean + kappa*sd, kappa decays with evidence.
	b := r.Beta(req.Task, m.ID)
	mean, sd, n := b.MeanSD(now, r.halfLife)
	kappa := 1.0
	if n > 20 {
		kappa = 0.2 + 0.8*20/n
		if kappa < 0.2 {
			kappa = 0.2
		}
	}
	q := mean + kappa*sd
	if q > 1 {
		q = 1
	}
	// C.
	eOut := 48.0
	if req.MaxOutput > 0 && float64(req.MaxOutput) < eOut {
		eOut = float64(req.MaxOutput)
	}
	c := float64(req.InTokensEst)/1e6*m.PricePerMtok.Input + eOut/1e6*m.PricePerMtok.Output
	// L: TTFT prior corrected by EWMA + output time.
	ttft := m.LatencyPrior.TTFTMs
	if v, cnt := r.ewma(m.ID).Get(); cnt > 5 {
		ttft = 0.5*ttft + 0.5*v
	}
	tps := m.LatencyPrior.TokensPerSec
	if tps <= 0 {
		tps = 80
	}
	l := ttft + eOut/tps*1000
	// R: Wilson upper bound of the breaker error rate, shrunk toward a small
	// prior while samples are few. Without shrinkage a model with 0 errors in
	// 5 requests gets R=0.43, which punishes models merely for being
	// unexplored and distorts quality-first objectives.
	errs, total := r.Breaker(m.ID).ErrRate()
	ub := stats.WilsonUB(errs, total, 1.96)
	w := float64(total) / (float64(total) + 20)
	risk := (1-w)*0.02 + w*ub
	return q, c, l, risk
}

func providerOf(cfg *config.Config, id string) string {
	if m := cfg.ModelByID(id); m != nil {
		return m.Provider
	}
	return ""
}

func upstreamOf(cfg *config.Config, id string) string {
	if m := cfg.ModelByID(id); m != nil {
		return m.Upstream
	}
	return id
}

func newDecisionID(req *canonical.Request) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(time.Now().UnixNano()))
	h := sha256.Sum256(append([]byte(req.Requested), b[:]...))
	return "d_" + hexEnc(h[:8])
}

type noFeasible struct {
	n int
}

func errNoFeasible(all []*Candidate) error { return &noFeasible{n: len(all)} }

func (e *noFeasible) Error() string { return "no_feasible_model" }
