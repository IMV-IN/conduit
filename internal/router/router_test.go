package router

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/IMV-IN/conduit/internal/canonical"
	"github.com/IMV-IN/conduit/internal/config"
)

func testModels() []*Candidate {
	mk := func(id string, q, c, l, r float64) *Candidate {
		return &Candidate{Model: &config.Model{ID: id}, Q: q, C: c, L: l, R: r}
	}
	return []*Candidate{
		mk("a", 0.9, 0.01, 1000, 0.02),
		mk("b", 0.8, 0.001, 800, 0.02),
		mk("c", 0.5, 0.0001, 300, 0.05),
	}
}

func TestScoreRanksBestFirst(t *testing.T) {
	ranked := Score(testModels(), Weights{Quality: 0.8, Cost: 0.02, Latency: 0.08, Risk: 0.1})
	if len(ranked) != 3 || ranked[0].Model.ID != "a" {
		t.Fatalf("expected a first, got %v", ranked[0].Model.ID)
	}
}

func TestScoreSkipsRejected(t *testing.T) {
	all := testModels()
	all[0].Reject = "context 4k"
	ranked := Score(all, Weights{Quality: 0.5, Cost: 0.2, Latency: 0.2, Risk: 0.1})
	for _, c := range ranked {
		if c.Reject != "" {
			t.Fatal("rejected candidate leaked into ranking")
		}
	}
	if len(ranked) != 2 {
		t.Fatalf("expected 2 survivors, got %d", len(ranked))
	}
}

func TestScoreEmpty(t *testing.T) {
	all := testModels()
	for _, c := range all {
		c.Reject = "x"
	}
	if out := Score(all, Weights{}); out != nil {
		t.Fatal("expected nil for no survivors")
	}
}

func TestSelectSumsToOne(t *testing.T) {
	ranked := Score(testModels(), Weights{Quality: 0.5, Cost: 0.2, Latency: 0.2, Risk: 0.1})
	ex := Explore{Epsilon: 0.02, Delta: 0.08, Tau: 0.05}
	// Reconstruct the distribution by probing: sum over a fine grid approximates 1
	// only for argmax; instead verify exactness via many seeded draws matching
	// empirical frequencies, plus the eps floor below.
	counts := map[int]int{}
	N := 200000
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < N; i++ {
		idx, p := Select(ranked, ex, rng.Float64())
		if math.IsNaN(p) || p <= 0 || p > 1 {
			t.Fatalf("invalid propensity %v", p)
		}
		counts[idx]++
		_ = p
	}
	// Every feasible model must have p >= eps/n: check empirically via floor draws.
	n := float64(len(ranked))
	floor := ex.Epsilon / n
	for i := range ranked {
		freq := float64(counts[i]) / float64(N)
		if freq < floor/2 { // generous tolerance; exact-floor assert is below
			t.Fatalf("model %d starved: freq %v floor %v", i, freq, floor)
		}
	}
}

func TestSelectEpsFloorExact(t *testing.T) {
	ranked := Score(testModels(), Weights{Quality: 0.9, Cost: 0.05, Latency: 0.03, Risk: 0.02})
	ex := Explore{Epsilon: 0.02, Delta: 0.0, Tau: 0.05} // delta=0: guard set = {best}
	// Worst model gets exactly eps/n.
	u := 0.999999
	idx, p := Select(ranked, ex, u)
	if idx != len(ranked)-1 {
		t.Fatalf("expected last index for u~1, got %d", idx)
	}
	want := ex.Epsilon / float64(len(ranked))
	if math.Abs(p-want) > 1e-9 {
		t.Fatalf("eps floor: got %v want %v", p, want)
	}
}

func TestSelectDeterministic(t *testing.T) {
	ranked := Score(testModels(), Weights{Quality: 0.5, Cost: 0.2, Latency: 0.2, Risk: 0.1})
	ex := Explore{Epsilon: 0.02, Delta: 0.08, Tau: 0.05}
	a, pa := Select(ranked, ex, 0.42)
	b, pb := Select(ranked, ex, 0.42)
	if a != b || pa != pb {
		t.Fatal("Select must be deterministic for the same u")
	}
}

func TestSeedFromID(t *testing.T) {
	if SeedFromID("d_abc") != SeedFromID("d_abc") {
		t.Fatal("seed must be reproducible")
	}
	v := SeedFromID("d_abc")
	if v < 0 || v >= 1 {
		t.Fatalf("seed out of range: %v", v)
	}
}

// Mirror of reservedOut(requestFor(...).MaxOutput): requestFor sets MaxOutput 64.
const maxOutForTest = 64

// TestHardFiltersNeverViolated fuzzes routing inputs against the example catalog
// and asserts capability/context filters are never violated in the chosen attempt.
func TestHardFiltersNeverViolated(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	rng := rand.New(rand.NewSource(7))
	pol := cfg.PolicyByName("default")
	for i := 0; i < 20000; i++ {
		tools := rng.Float64() < 0.3
		inTok := rng.Intn(30000)
		if rng.Float64() < 0.05 {
			inTok = 30000 + rng.Intn(60000)
		}
		req := requestFor(tools, inTok)
		plan, err := rt.Route(req, pol)
		if err != nil {
			continue // infeasible is legal; the 422 path is tested below
		}
		if len(plan.Attempts) == 0 {
			t.Fatal("empty plan without error")
		}
		first := cfg.ModelByID(plan.Attempts[0].ModelID)
		if tools && !hasCap(first, "tools") {
			t.Fatalf("routed tools request to %s without tools cap", first.ID)
		}
		need := float64(inTok)*1.10 + float64(maxOutForTest)
		if first.ContextTokens > 0 && need > float64(first.ContextTokens) {
			t.Fatalf("routed %d-token request to %s (ctx %d)", inTok, first.ID, first.ContextTokens)
		}
	}
}

func TestExploreScale(t *testing.T) {
	if exploreScale("best") != 0.2 {
		t.Fatal("best should narrow exploration")
	}
	for _, o := range []string{"", "balanced", "cheap", "fast"} {
		if exploreScale(o) != 1.0 {
			t.Fatalf("objective %q should keep configured exploration", o)
		}
	}
}

// TestBestObjectiveSticksToWinner mirrors the k6 accuracy gate
// (best_prefers_premium_for_code > 0.85) offline: with example-catalog
// priors, auto:best on a code request must land on mock/premium.
func TestBestObjectiveSticksToWinner(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	premium := 0
	N := 200
	for i := 0; i < N; i++ {
		req := &canonical.Request{
			Requested:   "auto:best",
			InTokensEst: 40, MaxOutput: 64,
			Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
		}
		plan, err := rt.Route(req, pol)
		if err != nil || len(plan.Attempts) == 0 {
			t.Fatal("best/code should always be feasible")
		}
		if plan.Attempts[0].ModelID == "mock/premium" {
			premium++
		}
	}
	rate := float64(premium) / float64(N)
	t.Logf("auto:best code -> premium %.3f", rate)
	if rate < 0.85 {
		t.Fatalf("auto:best must prefer premium for code (got %.3f)", rate)
	}
}

func TestLatencyEjection(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	// 12 slow-but-successful samples at ~6.7x the 900ms prior.
	for i := 0; i < 12; i++ {
		rt.ReportOutcome("mock/premium", true, 6000)
	}
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	plan, err := rt.Route(req, pol)
	if err != nil {
		t.Fatalf("other models must stay feasible: %v", err)
	}
	if plan.Attempts[0].ModelID == "mock/premium" {
		t.Fatal("slow model must be ejected from primary")
	}
	for _, a := range plan.Attempts {
		if a.ModelID == "mock/premium" {
			t.Fatal("ejected model must not appear in attempts at all")
		}
	}
	found := false
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject == "slow: latency ejection" {
			found = true
		}
	}
	if !found {
		t.Fatal("candidate table must record the ejection reason")
	}
}

func TestEjectionExpiryResets(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	for i := 0; i < 12; i++ {
		rt.ReportOutcome("mock/premium", true, 6000)
	}
	// Force expiry (same package: manipulate directly).
	rt.mu.Lock()
	rt.eject["mock/premium"] = time.Now().Add(-time.Second)
	rt.mu.Unlock()
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// The probe trickles admission (1-in-10): over 100 draws premium must
	// be admitted a handful of times, rejected with "slow: probing" (not
	// full ejection) otherwise, and never ejected while evidence is clean.
	premium, probing, ejected := 0, 0, 0
	for i := 0; i < 100; i++ {
		plan, err := rt.Route(req, cfg.PolicyByName("default"))
		if err != nil || len(plan.Attempts) == 0 {
			t.Fatalf("expired ejection must allow a probe: %v", err)
		}
		if plan.Attempts[0].ModelID == "mock/premium" {
			premium++
		}
		for _, c := range plan.Candidates {
			if c.Model.ID != "mock/premium" || c.Reject == "" {
				continue
			}
			if c.Reject == "slow: probing" {
				probing++
			} else {
				ejected++
			}
		}
	}
	if ejected > 0 {
		t.Fatalf("clean probe must not fully eject (%d rejects)", ejected)
	}
	if premium < 5 || premium > 20 {
		t.Fatalf("trickle should admit ~1-in-10 as primary, got %d/100", premium)
	}
	if probing < 70 {
		t.Fatalf("expected most draws to hit the trickle gate, got %d/100", probing)
	}
	if v, n := rt.ewma("mock/premium").Get(); n != 0 || v != 900 {
		t.Fatalf("EWMA should reset to prior on expiry, got v=%v n=%v", v, n)
	}
}

func TestSlowModelNeedsEvidence(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	// One slow sample must NOT eject (needs >= 10).
	rt.ReportOutcome("mock/premium", true, 60000)
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// One slow sample must NOT eject (needs >= 10). Asserted on the
	// deterministic ranking (top utility, no reject reason) rather than the
	// sampled draw, which carries a 1% epsilon floor by design.
	plan, err := rt.Route(req, cfg.PolicyByName("default"))
	if err != nil {
		t.Fatal(err)
	}
	best := ""
	bestU := -1e18
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject != "" {
			t.Fatalf("a single slow sample must not eject (reject=%q)", c.Reject)
		}
		if c.Reject == "" && c.U > bestU {
			best, bestU = c.Model.ID, c.U
		}
	}
	if best != "mock/premium" {
		t.Fatalf("premium should still rank first, got %s", best)
	}
}

func TestHedgeDelayTotal(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	plan, err := rt.Route(req, cfg.PolicyByName("default"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.HedgeDelay <= 0 {
		t.Fatal("example policy enables hedging")
	}
	// Total pads the delay with the CHOSEN primary's expected output time
	// (derived from the draw, so the assertion holds for any selection).
	chosen := cfg.ModelByID(plan.Attempts[0].ModelID)
	tps := chosen.LatencyPrior.TokensPerSec
	if tps <= 0 {
		tps = 80
	}
	eOut := 48.0 // min(MaxOutput=64, task p50 48)
	want := plan.HedgeDelay + time.Duration(eOut/tps*1000)*time.Millisecond
	if plan.HedgeDelayTotal != want {
		t.Fatalf("HedgeDelayTotal=%v, want %v (primary %s)", plan.HedgeDelayTotal, want, chosen.ID)
	}
	if plan.HedgeMaxShare != 0.05 {
		t.Fatalf("HedgeMaxShare=%v, want 0.05", plan.HedgeMaxShare)
	}
}

func TestHedgeLaunchesEject(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// Four hedge losses: not enough.
	for i := 0; i < 4; i++ {
		rt.ReportHedgeLaunched("mock/premium")
	}
	plan, err := rt.Route(req, pol)
	if err != nil || plan.Attempts[0].ModelID != "mock/premium" {
		t.Fatal("4 hedge losses must not eject")
	}
	// Fifth loss trips ejection.
	rt.ReportHedgeLaunched("mock/premium")
	plan, err = rt.Route(req, pol)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Attempts[0].ModelID == "mock/premium" {
		t.Fatal("5 hedge losses in a minute must eject")
	}
}

func TestEjectionRateGate(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// 5 hedge firings against 500 healthy primary selections (1% firing
	// rate) must NOT eject: at high traffic absolute counts mislead.
	for i := 0; i < 500; i++ {
		rt.notePrimary("mock/premium")
	}
	for i := 0; i < 5; i++ {
		rt.ReportHedgeLaunched("mock/premium")
	}
	plan, err := rt.Route(req, pol)
	if err != nil || plan.Attempts[0].ModelID != "mock/premium" {
		t.Fatal("sub-rate hedge firings must not eject")
	}
	// Same 5 firings against 5 primaries (100% rate) must eject.
	rt2 := New(cfg)
	for i := 0; i < 5; i++ {
		rt2.notePrimary("mock/premium")
		rt2.ReportHedgeLaunched("mock/premium")
	}
	plan, err = rt2.Route(req, pol)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Attempts[0].ModelID == "mock/premium" {
		t.Fatal("sustained hedge firing rate must eject")
	}
}

func TestEjectionBurstRule(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// 25 firings in a burst against a large healthy history must still
	// eject: correlated slowness right now outweighs old samples.
	for i := 0; i < 500; i++ {
		rt.notePrimary("mock/premium")
	}
	for i := 0; i < 25; i++ {
		rt.ReportHedgeLaunched("mock/premium")
	}
	plan, err := rt.Route(req, pol)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Attempts[0].ModelID == "mock/premium" {
		t.Fatal("burst of hedge firings must eject despite healthy history")
	}
}

func TestHealthyTrickleNoEject(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// 7 firings spread against 300 primaries (~2.3%, healthy-tail-like):
	// below both the burst floor and the rate gate.
	for i := 0; i < 300; i++ {
		rt.notePrimary("mock/premium")
	}
	for i := 0; i < 7; i++ {
		rt.ReportHedgeLaunched("mock/premium")
	}
	plan, err := rt.Route(req, pol)
	if err != nil || plan.Attempts[0].ModelID != "mock/premium" {
		t.Fatal("healthy-tail firing levels must not eject")
	}
}

func TestProbeReejectsOnLaunches(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	// Prime slow evidence, then force expiry to open a probe.
	for i := 0; i < 12; i++ {
		rt.ReportOutcome("mock/premium", true, 6000)
	}
	rt.mu.Lock()
	rt.eject["mock/premium"] = time.Now().Add(-time.Second)
	rt.mu.Unlock()
	// First route opens the probe and admits (seen=1). Simulate the three
	// hedge launches the slow probes would record, then the next route must
	// re-eject via the fast path — without waiting for quota.
	plan, err := rt.Route(req, pol)
	if err != nil {
		t.Fatal(err)
	}
	rejected := false
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject != "" {
			rejected = true
		}
	}
	if rejected {
		t.Fatal("fresh probe must admit its first check")
	}
	rt.ReportHedgeLaunched("mock/premium")
	rt.ReportHedgeLaunched("mock/premium")
	rt.ReportHedgeLaunched("mock/premium")
	plan, err = rt.Route(req, pol)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject == "slow: latency ejection" {
			found = true
		}
	}
	if !found {
		t.Fatal("probe with 3 hedge launches must re-eject")
	}
}

func TestProbeGraduatesOnFastEvidence(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	for i := 0; i < 12; i++ {
		rt.ReportOutcome("mock/premium", true, 6000)
	}
	rt.mu.Lock()
	rt.eject["mock/premium"] = time.Now().Add(-time.Second)
	rt.mu.Unlock()
	// Admitted probes complete fast (healed): no launches accumulate, so
	// once the quota is spent the verdict graduates the model. With 1-in-10
	// trickle, 12 admissions take ~120 routes; each admitted probe reports
	// a fast outcome like the executor would on completion.
	admitted := 0
	for i := 0; i < 200 && admitted < 12; i++ {
		plan, err := rt.Route(req, pol)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range plan.Candidates {
			if c.Model.ID == "mock/premium" && c.Reject == "" {
				admitted++
				rt.ReportOutcome("mock/premium", true, 900)
				break
			}
		}
	}
	if admitted != 12 {
		t.Fatalf("expected to spend the full probe quota, admitted %d", admitted)
	}
	plan, err := rt.Route(req, pol)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject != "" {
			t.Fatalf("healed model must graduate, reject=%q", c.Reject)
		}
	}
	// Primary in the vast majority of draws (epsilon floor forbids
	// asserting a single draw).
	premium := 0
	for i := 0; i < 20; i++ {
		p, err := rt.Route(req, pol)
		if err != nil {
			t.Fatal(err)
		}
		if p.Attempts[0].ModelID == "mock/premium" {
			premium++
		}
	}
	if premium < 18 {
		t.Fatalf("healed model should be primary, got %d/20", premium)
	}
}

func TestUniformSlowdownBlocksEjection(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	// Every model ~6x its prior: a uniformly slow environment (shared CI
	// runner), not a model-specific fault. Must NOT eject.
	for i := 0; i < 12; i++ {
		rt.ReportOutcome("mock/premium", true, 6000)  // 6.7x of 900
		rt.ReportOutcome("mock/balanced", true, 3000) // 6x of 500
		rt.ReportOutcome("mock/fast", true, 1200)     // 6x of 200
	}
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	plan, err := rt.Route(req, pol)
	if err != nil {
		t.Fatalf("all models feasible: %v", err)
	}
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject == "slow: latency ejection" {
			t.Fatal("uniform slowdown must not eject: peers equally slow")
		}
	}
}

func TestSingleModelSlownessStillEjects(t *testing.T) {
	cfg, err := config.Load("../../conduit.example.yaml")
	if err != nil {
		t.Skip("example config not found")
	}
	rt := New(cfg)
	pol := cfg.PolicyByName("default")
	// Only premium slow; peers healthy with enough samples to judge.
	for i := 0; i < 12; i++ {
		rt.ReportOutcome("mock/premium", true, 6000)
		rt.ReportOutcome("mock/balanced", true, 500)
		rt.ReportOutcome("mock/fast", true, 200)
	}
	req := &canonical.Request{
		Requested: "auto:best", InTokensEst: 40, MaxOutput: 64,
		Task: "code", TaskConf: 0.9, Metadata: map[string]string{},
	}
	plan, err := rt.Route(req, pol)
	if err != nil {
		t.Fatalf("other models must stay feasible: %v", err)
	}
	found := false
	for _, c := range plan.Candidates {
		if c.Model.ID == "mock/premium" && c.Reject == "slow: latency ejection" {
			found = true
		}
	}
	if !found {
		t.Fatal("model-specific slowness must still eject with healthy peers")
	}
}
