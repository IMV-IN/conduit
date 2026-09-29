// Package telemetry exposes Prometheus metrics.
package telemetry

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	Requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "conduit_requests_total", Help: "Requests by policy/model/task/outcome.",
	}, []string{"policy", "model", "task", "outcome"})
	RouteDecisionSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "conduit_route_decision_seconds", Help: "Routing decision latency.",
		Buckets: []float64{0.00005, 0.0001, 0.0002, 0.0005, 0.001, 0.005},
	})
	GatewayOverheadSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "conduit_gateway_overhead_seconds", Help: "Gateway overhead excl. upstream.",
		Buckets: []float64{0.0002, 0.0005, 0.001, 0.002, 0.005, 0.01, 0.05},
	})
	TTFT = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "conduit_ttft_seconds", Help: "Time to first token.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"model"})
	CostUSD = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "conduit_cost_usd_total", Help: "Spend by model.",
	}, []string{"model"})
	Hedges = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "conduit_hedges_total", Help: "Hedges by result.",
	}, []string{"result"})
	BreakerState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "conduit_breaker_state", Help: "0=closed 1=half-open 2=open.",
	}, []string{"model"})
)
