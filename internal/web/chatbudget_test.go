package web

import (
	"testing"
	"time"
)

func budgetIdx(s *chatBudgetScorer, budget int) int {
	for i := range s.arms {
		if s.arms[i].Budget == budget {
			return i
		}
	}
	return -1
}

// TestChatBudgetScorerExploitsWinner: once every arm has been measured, a mode
// that keeps succeeding while another keeps failing wins the exploit picks.
func TestChatBudgetScorerExploitsWinner(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, 21)
	comp := budgetIdx(s, 8)

	// Warm up all arms (pick returns untried arms first), recording wide=good,
	// the rest bad.
	for n := 0; n < 12; n++ {
		now = now.Add(time.Second)
		i := s.pick(now, 0.99) // exploit
		s.record(i, i == wide, now)
	}
	if s.arms[wide].Score <= s.arms[comp].Score {
		t.Fatalf("winner not scored highest: wide=%.2f comp=%.2f", s.arms[wide].Score, s.arms[comp].Score)
	}
	// An exploit pick (r above the explore rate, no stale arms) chooses wide.
	now = now.Add(time.Second)
	if got := s.pick(now, 0.99); got != wide {
		t.Fatalf("exploit picked arm %d, want wide %d", got, wide)
	}
}

// TestChatBudgetScorerNeverAbandons: even a consistently-losing arm keeps being
// re-measured (staleness re-explore), but is not hammered.
func TestChatBudgetScorerNeverAbandons(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, 21)
	counts := map[int]int{}
	r := 0.99 // mostly exploit
	for n := 0; n < 400; n++ {
		now = now.Add(30 * time.Second) // advance so stale re-explore kicks in
		i := s.pick(now, r)
		counts[i]++
		s.record(i, i == wide, now) // only wide succeeds
		r += 0.37
		if r >= 1 {
			r -= 1
		}
	}
	for i := range s.arms {
		if counts[i] == 0 {
			t.Fatalf("arm %d (budget %d) was never tested", i, s.arms[i].Budget)
		}
	}
	// The winner is used the most; a loser is sampled but stays a minority.
	if counts[wide] <= counts[budgetIdx(s, 8)] {
		t.Fatalf("winner not used most: %v", counts)
	}
}

// TestChatBudgetScorerCorrectsFast: when the winning mode suddenly starts
// failing (network change), its EWMA drops below a now-better mode within a few
// sends.
func TestChatBudgetScorerCorrectsFast(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, 21)
	comp := budgetIdx(s, 8)

	// Phase 1: wide is great, compact is bad.
	for n := 0; n < 20; n++ {
		now = now.Add(time.Second)
		s.record(wide, true, now)
		s.record(comp, false, now)
	}
	if s.arms[wide].Score < 0.9 {
		t.Fatalf("wide should be high before the flip: %.2f", s.arms[wide].Score)
	}
	// Network flips: wide now fails. After a few sends its score must fall below
	// compact's recovered score.
	flipped := -1
	for n := 0; n < 10; n++ {
		now = now.Add(time.Second)
		s.record(wide, false, now)
		s.record(comp, true, now)
		if s.arms[comp].Score > s.arms[wide].Score {
			flipped = n
			break
		}
	}
	if flipped < 0 {
		t.Fatalf("scores never corrected after the flip: wide=%.2f comp=%.2f", s.arms[wide].Score, s.arms[comp].Score)
	}
	if flipped > 5 {
		t.Fatalf("correction too slow (%d sends)", flipped+1)
	}
}

func TestChatBudgetScorerArmsMatchPresets(t *testing.T) {
	s := newChatBudgetScorer()
	for _, b := range []int{chatBudgetPresets["compact"], chatBudgetPresets["standard"], chatBudgetPresets["wide"]} {
		if budgetIdx(s, b) < 0 {
			t.Fatalf("scorer missing an arm for preset budget %d", b)
		}
	}
}
