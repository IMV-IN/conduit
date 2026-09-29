// Package router is the public Go library (adoption mode 2).
// It re-exports the internal routing engine's core types.
package router

import (
	"github.com/yourorg/conduit/internal/canonical"
	"github.com/yourorg/conduit/internal/config"
	irouter "github.com/yourorg/conduit/internal/router"
)

type (
	Plan      = irouter.Plan
	Candidate = irouter.Candidate
	Weights   = irouter.Weights
	Explore   = irouter.Explore
	Request   = canonical.Request
)

func Score(all []*Candidate, w Weights) []*Candidate { return irouter.Score(all, w) }
func Select(ranked []*Candidate, ex Explore, u float64) (int, float64) {
	return irouter.Select(ranked, ex, u)
}
func New(cfg *config.Config) *irouter.Router { return irouter.New(cfg) }
