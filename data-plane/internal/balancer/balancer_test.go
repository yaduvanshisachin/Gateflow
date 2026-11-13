package balancer

import (
	"net/url"
	"strings"
	"sync"
	"testing"
)

func targets(t *testing.T, weights ...int) []*Target {
	t.Helper()
	out := make([]*Target, len(weights))
	for i, w := range weights {
		u, _ := url.Parse("http://" + string(rune('a'+i)) + ":80")
		out[i] = &Target{URL: u, Weight: w}
	}
	return out
}

func names(b Balancer, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(b.Next().URL.Host[:1])
	}
	return sb.String()
}

func TestRoundRobinCycles(t *testing.T) {
	b, err := New("round_robin", targets(t, 1, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := names(b, 7); got != "abcabca" {
		t.Fatalf("got %s", got)
	}
}

func TestSmoothWeightedMatchesNginxSequence(t *testing.T) {
	b, err := New("weighted", targets(t, 5, 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	// Interleaved, not "aaaaabc".
	if got := names(b, 7); got != "aabacaa" {
		t.Fatalf("got %s, want aabacaa", got)
	}
	// The pattern repeats every sum(weights) picks.
	if got := names(b, 7); got != "aabacaa" {
		t.Fatalf("second cycle got %s", got)
	}
}

func TestWeightedDistributionOverManyPicks(t *testing.T) {
	b, _ := New("weighted", targets(t, 3, 1))
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		counts[b.Next().URL.Host]++
	}
	if counts["a:80"] != 300 || counts["b:80"] != 100 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestConcurrentUseIsSafeAndFair(t *testing.T) {
	for _, algo := range []string{"round_robin", "weighted"} {
		b, _ := New(algo, targets(t, 1, 1))
		var (
			mu     sync.Mutex
			counts = map[string]int{}
			wg     sync.WaitGroup
		)
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				local := map[string]int{}
				for i := 0; i < 500; i++ {
					local[b.Next().URL.Host]++
				}
				mu.Lock()
				for k, v := range local {
					counts[k] += v
				}
				mu.Unlock()
			}()
		}
		wg.Wait()
		if counts["a:80"] != 2000 || counts["b:80"] != 2000 {
			t.Errorf("%s: counts = %v", algo, counts)
		}
	}
}

func TestNewErrors(t *testing.T) {
	if _, err := New("round_robin", nil); err == nil {
		t.Error("expected error for no targets")
	}
	if _, err := New("nope", targets(t, 1)); err == nil {
		t.Error("expected error for unknown algorithm")
	}
}
