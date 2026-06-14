package web

import "time"

// Adaptive per-server selection of the chat cell budget (RFC §8.2). On top of
// the fixed presets (compact/standard/wide) there is an "auto" mode that
// measures each preset by recent send success and prefers the best one while
// never abandoning the others — so if the wide (fewer, larger queries) mode
// stops losing packets it stays there, and if the network changes the scores
// correct within a handful of messages.

// chatBudgetModeAuto names the adaptive mode (the fixed modes are the preset
// names in chatBudgetPresets: compact / standard / wide).
const chatBudgetModeAuto = "auto"

const (
	// chatScoreAlpha weights the newest outcome in each arm's EWMA score: high
	// enough that a few recent sends dominate, so a mode that starts failing (or
	// recovers) is re-scored within a handful of messages.
	chatScoreAlpha = 0.35
	// chatExploreRate is the fraction of auto sends that explore instead of using
	// the current best — bounded, so a poor mode is still sampled but never
	// hammered.
	chatExploreRate = 0.2
	// chatScoreStale: an arm unused this long is re-measured first (the path may
	// have changed); also how "old results" age out of relevance.
	chatScoreStale = 10 * time.Minute
)

// chatBudgetArm is one candidate budget with its recent-success score.
type chatBudgetArm struct {
	Budget int       `json:"budget"`
	Score  float64   `json:"score"` // EWMA of recent outcomes, 0..1 (1 = always succeeding)
	Used   int       `json:"used"`
	lastAt time.Time // live state, not serialized
}

// chatBudgetScorer adaptively picks a budget by recent success (epsilon-greedy
// over EWMA scores). One per server (each network path scores independently).
// Not safe for concurrent use; the caller holds chatHub.mu.
type chatBudgetScorer struct {
	arms []chatBudgetArm
}

// newChatBudgetScorer scores the three presets, in a stable order, starting
// optimistic so each is tried before any is trusted.
func newChatBudgetScorer() *chatBudgetScorer {
	return &chatBudgetScorer{arms: []chatBudgetArm{
		{Budget: chatBudgetPresets["compact"], Score: 1},
		{Budget: chatBudgetPresets["standard"], Score: 1},
		{Budget: chatBudgetPresets["wide"], Score: 1},
	}}
}

// pick returns the arm index to use for the next send. r is a random value in
// [0,1) (injected for testability). An untried or stale arm is re-measured
// first; otherwise with probability chatExploreRate it explores the
// least-recently-used arm, else it exploits the highest score.
func (s *chatBudgetScorer) pick(now time.Time, r float64) int {
	for i := range s.arms {
		if s.arms[i].Used == 0 || now.Sub(s.arms[i].lastAt) > chatScoreStale {
			return i
		}
	}
	if r < chatExploreRate {
		lru := 0
		for i := range s.arms {
			if s.arms[i].lastAt.Before(s.arms[lru].lastAt) {
				lru = i
			}
		}
		return lru
	}
	best := 0
	for i := range s.arms {
		if s.arms[i].Score > s.arms[best].Score {
			best = i
		}
	}
	return best
}

// record folds a send outcome into an arm's recent-success EWMA.
func (s *chatBudgetScorer) record(i int, success bool, now time.Time) {
	if i < 0 || i >= len(s.arms) {
		return
	}
	o := 0.0
	if success {
		o = 1
	}
	a := &s.arms[i]
	a.Score = (1-chatScoreAlpha)*a.Score + chatScoreAlpha*o
	a.Used++
	a.lastAt = now
}

// snapshot returns a copy of the arms for the UI.
func (s *chatBudgetScorer) snapshot() []chatBudgetArm {
	out := make([]chatBudgetArm, len(s.arms))
	copy(out, s.arms)
	return out
}
