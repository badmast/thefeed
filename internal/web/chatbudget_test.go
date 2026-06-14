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

// TestChatBudgetScorerExploitsCheapest: once every arm is measured, the mode
// that spends the fewest queries wins the exploit picks.
func TestChatBudgetScorerExploitsCheapest(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, chatBudgetPresets["wide"])
	compact := budgetIdx(s, chatBudgetPresets["compact"])

	for n := 0; n < 9; n++ {
		now = now.Add(time.Second)
		i := s.pick(now, 0.99) // exploit
		q := 25                // most modes are heavy
		if i == wide {
			q = 6 // wide spends the fewest queries
		}
		s.record(i, q, 0, true, now)
	}
	if s.arms[wide].cost >= s.arms[compact].cost {
		t.Fatalf("cheap arm not lowest cost: wide=%.1f compact=%.1f", s.arms[wide].cost, s.arms[compact].cost)
	}
	now = now.Add(time.Second)
	if got := s.pick(now, 0.99); got != wide {
		t.Fatalf("exploit picked arm %d, want wide %d", got, wide)
	}
}

// TestChatBudgetScorerNeverAbandons: a consistently-costly arm keeps being
// re-measured (staleness) but stays a minority of picks.
func TestChatBudgetScorerNeverAbandons(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, chatBudgetPresets["wide"])
	counts := map[int]int{}
	r := 0.99
	for n := 0; n < 400; n++ {
		now = now.Add(30 * time.Second)
		i := s.pick(now, r)
		counts[i]++
		q := 25
		if i == wide {
			q = 6
		}
		s.record(i, q, 0, true, now)
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
	if counts[wide] <= counts[budgetIdx(s, chatBudgetPresets["compact"])] {
		t.Fatalf("cheapest not used most: %v", counts)
	}
}

// TestChatBudgetScorerCorrectsFast: when the cheap mode starts erroring (the
// network turned weak) its cost overtakes another within a few sends.
func TestChatBudgetScorerCorrectsFast(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, chatBudgetPresets["wide"])
	standard := budgetIdx(s, chatBudgetPresets["standard"])

	for n := 0; n < 20; n++ {
		now = now.Add(time.Second)
		s.record(wide, 6, 0, true, now)      // wide cheap+clean
		s.record(standard, 12, 2, true, now) // standard heavier
	}
	if s.arms[wide].cost >= s.arms[standard].cost {
		t.Fatalf("wide should be cheaper before the flip")
	}
	flipped := -1
	for n := 0; n < 10; n++ {
		now = now.Add(time.Second)
		s.record(wide, 30, 10, true, now)    // wide now losing many queries
		s.record(standard, 12, 0, true, now) // standard clean
		if s.arms[standard].cost < s.arms[wide].cost {
			flipped = n
			break
		}
	}
	if flipped < 0 {
		t.Fatalf("never corrected: wide=%.1f standard=%.1f", s.arms[wide].cost, s.arms[standard].cost)
	}
	if flipped > 5 {
		t.Fatalf("correction too slow (%d sends)", flipped+1)
	}
}

// TestChatBudgetScorerFailurePenalty: a failed send ranks worse than a heavy but
// successful one.
func TestChatBudgetScorerFailurePenalty(t *testing.T) {
	s := newChatBudgetScorer()
	now := time.Now()
	wide := budgetIdx(s, chatBudgetPresets["wide"])
	standard := budgetIdx(s, chatBudgetPresets["standard"])
	s.record(wide, 8, 4, false, now)     // failed send
	s.record(standard, 40, 5, true, now) // heavy but it worked
	if s.arms[standard].cost >= s.arms[wide].cost {
		t.Fatalf("failed mode (%.1f) should rank worse than heavy success (%.1f)", s.arms[wide].cost, s.arms[standard].cost)
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
