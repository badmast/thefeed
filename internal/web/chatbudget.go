package web

import "time"

// Adaptive per-server selection of the chat cell budget (RFC §8.2). On top of
// the fixed presets (compact/standard/wide) there is an "auto" mode that scores
// each preset by how many DNS queries it spends per message and how many of
// those queries error — the same signal the feed scores resolvers on. This is
// what makes Auto safe on weak resolvers: Compact fragments every op into many
// small queries, so it costs (and loses) far more per message; the score ranks
// it last and Auto avoids it instead of flooding.

const chatBudgetModeAuto = "auto"

const (
	// chatScoreAlpha weights the newest sample in each arm's EWMAs, high enough
	// that a few recent sends dominate so a mode is re-ranked quickly.
	chatScoreAlpha = 0.4
	// chatExploreRate is the fraction of auto sends that explore instead of using
	// the cheapest mode, so none is ever abandoned (bounded, so a costly one
	// isn't hammered).
	chatExploreRate = 0.15
	// chatScoreStale: an arm unused this long is re-measured first (the path may
	// have changed); also how old samples age out.
	chatScoreStale = 10 * time.Minute
	// chatErrWeight: a lost query counts as this many queries of cost (errors are
	// worse than mere volume — they mean retransmits and risk).
	chatErrWeight = 3.0
	// chatFailPenalty: a send that failed outright adds this query-equivalent
	// cost, so a failing mode sinks below any working one.
	chatFailPenalty = 100.0
)

// chatBudgetArm is one candidate budget with its recent cost.
type chatBudgetArm struct {
	Budget  int     `json:"budget"`
	Queries float64 `json:"queries"` // EWMA queries per send (for display)
	Errors  float64 `json:"errors"`  // EWMA lost queries per send (for display)
	cost    float64 // EWMA combined cost used for ranking; lower = better
	Used    int     `json:"used"`
	lastAt  time.Time
}

// chatBudgetScorer picks a budget by lowest recent query cost (epsilon-greedy).
// One per server (each network path scores independently). Not safe for
// concurrent use; the caller holds chatHub.mu.
type chatBudgetScorer struct {
	arms []chatBudgetArm
}

// newChatBudgetScorer orders the arms Standard, Wide, Compact so the initial
// trials measure the low-query-count modes first; Compact (most queries) is
// tried last and, on a weak path, ranked out.
func newChatBudgetScorer() *chatBudgetScorer {
	return &chatBudgetScorer{arms: []chatBudgetArm{
		{Budget: chatBudgetPresets["standard"]},
		{Budget: chatBudgetPresets["wide"]},
		{Budget: chatBudgetPresets["compact"]},
	}}
}

// pick returns the arm index for the next send. r is a random value in [0,1)
// (injected for testability): an untried or stale arm is measured first, else
// with probability chatExploreRate it explores the least-recently-used arm, else
// it exploits the lowest-cost arm.
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
		if s.arms[i].cost < s.arms[best].cost {
			best = i
		}
	}
	return best
}

// record folds one send's query/error counts into an arm. cost = queries +
// weighted errors, plus a penalty if the message failed outright.
func (s *chatBudgetScorer) record(i, queries, errs int, ok bool, now time.Time) {
	if i < 0 || i >= len(s.arms) {
		return
	}
	if queries < 0 {
		queries = 0
	}
	if errs < 0 {
		errs = 0
	}
	sampleCost := float64(queries) + chatErrWeight*float64(errs)
	if !ok {
		sampleCost += chatFailPenalty
	}
	a := &s.arms[i]
	if a.Used == 0 {
		a.cost, a.Queries, a.Errors = sampleCost, float64(queries), float64(errs)
	} else {
		a.cost = ewma(a.cost, sampleCost)
		a.Queries = ewma(a.Queries, float64(queries))
		a.Errors = ewma(a.Errors, float64(errs))
	}
	a.Used++
	a.lastAt = now
}

func ewma(prev, sample float64) float64 {
	return (1-chatScoreAlpha)*prev + chatScoreAlpha*sample
}

// snapshot returns a copy of the arms for the UI.
func (s *chatBudgetScorer) snapshot() []chatBudgetArm {
	out := make([]chatBudgetArm, len(s.arms))
	copy(out, s.arms)
	return out
}
