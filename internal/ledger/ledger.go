// Package ledger is an append-only in-memory decision store with JSONL export.
// SQLite/Postgres writers land in Phase 4; the record shape is already final.
package ledger

import (
	"sync"
	"time"
)

// Candidate mirrors the routing candidate table.
type Candidate struct {
	Model    string  `json:"model"`
	Q        float64 `json:"q"`
	C        float64 `json:"c"`
	L        float64 `json:"l"`
	R        float64 `json:"r"`
	U        float64 `json:"u"`
	Frontier bool    `json:"frontier"`
	Reject   string  `json:"reject_reason,omitempty"`
}

// Attempt records one upstream try.
type Attempt struct {
	Model  string  `json:"model"`
	Status int     `json:"status"`
	TTFTMs float64 `json:"ttft_ms"`
	Error  string  `json:"error,omitempty"`
}

// Record is one decision.
type Record struct {
	DecisionID string      `json:"decision_id"`
	Ts         time.Time   `json:"ts"`
	Tenant     string      `json:"tenant"`
	KeyID      string      `json:"key_id"`
	Policy     string      `json:"policy"`
	Task       string      `json:"task"`
	TaskConf   float64     `json:"task_conf"`
	Chosen     string      `json:"chosen"`
	Propensity float64     `json:"propensity"`
	Explored   bool        `json:"explored"`
	Candidates []Candidate `json:"candidates"`
	Attempts   []Attempt   `json:"attempts"`
	Status     int         `json:"status"`
	TTFTMs     float64     `json:"ttft_ms"`
	TotalMs    float64     `json:"total_ms"`
	InTokens   int         `json:"in_tokens"`
	OutTokens  int         `json:"out_tokens"`
	CostUSD    float64     `json:"cost_usd"`
	Reward     *float64    `json:"reward,omitempty"`
	Relaxed    []string    `json:"relaxed,omitempty"`
}

// Store is a bounded ring buffer (drop-oldest on overflow, with counter).
type Store struct {
	mu      sync.RWMutex
	buf     []*Record
	cap     int
	Dropped uint64
	byID    map[string]*Record
}

func New(cap int) *Store {
	if cap <= 0 {
		cap = 100000
	}
	return &Store{cap: cap, byID: map[string]*Record{}}
}

func (s *Store) Append(r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) >= s.cap {
		old := s.buf[0]
		copy(s.buf, s.buf[1:])
		s.buf = s.buf[:len(s.buf)-1]
		delete(s.byID, old.DecisionID)
		s.Dropped++
	}
	s.buf = append(s.buf, r)
	s.byID[r.DecisionID] = r
}

func (s *Store) Get(id string) (*Record, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.byID[id]
	return r, ok
}

func (s *Store) SetReward(id string, v float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.byID[id]
	if !ok {
		return false
	}
	r.Reward = &v
	return true
}

// Search returns newest-first records matching a filter (bounded).
func (s *Store) Search(task, model, policy string, limit int) []*Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var out []*Record
	for i := len(s.buf) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.buf[i]
		if task != "" && r.Task != task {
			continue
		}
		if model != "" && r.Chosen != model {
			continue
		}
		if policy != "" && r.Policy != policy {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.buf)
}
