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
	"time"

	"github.com/yourorg/conduit/internal/canonical"
	"github.com/yourorg/conduit/internal/config"
	"github.com/yourorg/conduit/internal/health"
	"github.com/yourorg/conduit/internal/stats"
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

// SeedFromID hashes a decision ID to a uniform in [0,1) (reproducible).
func SeedFromID(id string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	v := h.Sum64()
	// 53 bits of randomness -> [0,1)
	return float64(v>>11) / float64(1<<53)
}

func decisionID() string {
	var b [16]byte
	// crypto random would be better; but keep stdlib-only determinism via time+counter
	h := sha256.Sum256([]byte(time.Now().String()))
	copy(b[:], h[:16])
	return "d_" + hexEnc(b[:])
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
	rr       map[string]int // round-robin cursors
	halfLife time.Duration
}

func New(cfg *config.Config) *Router {
	r := &Router{
		cfg:      cfg,
		betas:    map[string]*stats.Beta{},
		breakers: map[string]*health.Breaker{},
		ewmaTTFT: map[string]*stats.EWMA{},
		rr:       map[string]int{},
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
		if e, ok2 := r.ewmaTTFT[model]; ok2 {
			e.Add(ttftMs)
		}
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
		return &Plan{
			DecisionID: newDecisionID(req),
			Attempts:   []Attempt{{ModelID: p.Model, Provider: providerOf(r.cfg, p.Model), Upstream: upstreamOf(r.cfg, p.Model)}},
			Propensity: 1, Policy: p.Name, Task: req.Task, TaskConf: req.TaskConf,
			Candidates: []*Candidate{{Model: r.cfg.ModelByID(p.Model), Q: 1, U: 1}},
		}, nil
	}
	if p.Strategy == "round_robin" && len(p.Models) > 0 {
		i := r.rr[p.Name] % len(p.Models)
		r.rr[p.Name]++
		mid := p.Models[i]
		var attempts []Attempt
		for k := 0; k < len(p.Models); k++ {
			m := p.Models[(i+k)%len(p.Models)]
			attempts = append(attempts, Attempt{ModelID: m, Provider: providerOf(r.cfg, m), Upstream: upstreamOf(r.cfg, m)})
		}
		_ = mid
		return &Plan{
			DecisionID: newDecisionID(req), Attempts: attempts,
			Propensity: 1.0 / float64(len(p.Models)), Policy: p.Name, Task: req.Task, TaskConf: req.TaskConf,
		}, nil
	}
	// Pinned provider/model passthrough.
	if m := r.cfg.ModelByID(req.Requested); m != nil {
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
	if eff.Hedging.Enabled && eff.Hedging.MinDelayMs > 0 {
		hd = time.Duration(eff.Hedging.MinDelayMs) * time.Millisecond
	}
	return &Plan{
		DecisionID: did, Attempts: attempts, HedgeDelay: hd, Relaxed: relaxed,
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
	if v, cnt := r.ewmaTTFT[m.ID].Get(); cnt > 5 {
		ttft = 0.5*ttft + 0.5*v
	}
	tps := m.LatencyPrior.TokensPerSec
	if tps <= 0 {
		tps = 80
	}
	l := ttft + eOut/tps*1000
	// R: Wilson UB of breaker error rate.
	errs, total := r.Breaker(m.ID).ErrRate()
	risk := stats.WilsonUB(errs, total, 1.96)
	if total < 5 {
		risk = 0.02
	}
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

var didCtr uint64

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
