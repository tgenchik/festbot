package draws

import (
	"math/rand/v2"
	"slices"
)

// Candidate — участник розыгрыша и число его билетов (в дневном розыгрыше всегда 1,
// в финальном — число активных дней, но не больше потолка).
type Candidate struct {
	UserID  int64
	Tickets int
}

// Pick выбирает n разных победителей. Вероятность пропорциональна числу билетов,
// один человек не может выиграть дважды (выбор без возвращения).
//
// Результат полностью определяется seed и составом участников: розыгрыш можно
// повторить и проверить. Кандидаты сортируются, поэтому порядок входа не важен.
func Pick(seed [32]byte, candidates []Candidate, n int) []int64 {
	pool := slices.Clone(candidates)
	slices.SortFunc(pool, func(a, b Candidate) int {
		switch {
		case a.UserID < b.UserID:
			return -1
		case a.UserID > b.UserID:
			return 1
		}
		return 0
	})

	var total int64
	for _, c := range pool {
		total += int64(c.Tickets)
	}

	rng := rand.New(rand.NewChaCha8(seed))
	winners := make([]int64, 0, min(n, len(pool)))
	for len(winners) < n && total > 0 {
		x := rng.Int64N(total)
		for i, c := range pool {
			x -= int64(c.Tickets)
			if x < 0 {
				winners = append(winners, c.UserID)
				total -= int64(c.Tickets)
				pool = slices.Delete(pool, i, i+1)
				break
			}
		}
	}
	return winners
}
