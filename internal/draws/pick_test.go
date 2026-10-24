package draws

import "testing"

func TestPickDeterministicAndUnique(t *testing.T) {
	var seed [32]byte
	seed[0] = 42
	cands := []Candidate{{1, 1}, {2, 3}, {3, 10}, {4, 1}, {5, 7}, {6, 2}}
	reversed := []Candidate{{6, 2}, {5, 7}, {4, 1}, {3, 10}, {2, 3}, {1, 1}}

	a := Pick(seed, cands, 4)
	b := Pick(seed, reversed, 4)
	if len(a) != 4 {
		t.Fatalf("want 4 winners, got %d", len(a))
	}
	seen := map[int64]bool{}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("result depends on input order: %v vs %v", a, b)
		}
		if seen[a[i]] {
			t.Fatalf("duplicate winner %d in %v", a[i], a)
		}
		seen[a[i]] = true
	}
}

func TestPickMoreThanCandidates(t *testing.T) {
	got := Pick([32]byte{}, []Candidate{{1, 1}, {2, 1}}, 5)
	if len(got) != 2 {
		t.Fatalf("want all 2 candidates, got %v", got)
	}
}

func TestPickWeights(t *testing.T) {
	// У участника с 9 билетами из 10 первое место должно выпадать ~90% раз.
	wins := 0
	const runs = 20000
	for i := range runs {
		var seed [32]byte
		seed[0], seed[1], seed[2] = byte(i), byte(i>>8), byte(i>>16)
		if Pick(seed, []Candidate{{1, 9}, {2, 1}}, 1)[0] == 1 {
			wins++
		}
	}
	if share := float64(wins) / runs; share < 0.88 || share > 0.92 {
		t.Fatalf("weighted share %.3f, want ~0.90", share)
	}
}
