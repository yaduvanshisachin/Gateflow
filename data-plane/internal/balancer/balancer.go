// Package balancer picks an upstream target for each request.
//
// Phase 1 has no health awareness: every target is always a candidate.
// Phase 4 adds active/passive health checks, which will plug in here by
// filtering the candidate set before Next picks.
package balancer

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
)

type Target struct {
	URL    *url.URL
	Weight int
}

type Balancer interface {
	// Next must be safe for concurrent use.
	Next() *Target
}

func New(algo string, targets []*Target) (Balancer, error) {
	if len(targets) == 0 {
		return nil, errors.New("no targets")
	}
	switch algo {
	case "", "round_robin":
		return &roundRobin{targets: targets}, nil
	case "weighted":
		return newSmoothWRR(targets), nil
	default:
		return nil, fmt.Errorf("unknown lb algorithm %q", algo)
	}
}

// roundRobin cycles through targets with a single atomic counter: lock-free,
// and even under concurrency every target gets an equal share.
type roundRobin struct {
	targets []*Target
	n       atomic.Uint64
}

func (r *roundRobin) Next() *Target {
	i := r.n.Add(1) - 1
	return r.targets[i%uint64(len(r.targets))]
}

// smoothWRR is nginx's smooth weighted round robin. Unlike naive "repeat each
// target weight times", it interleaves picks: weights 5/1/1 yield
// a a b a c a a rather than a a a a a b c, so a heavy target never receives a
// long burst.
//
// It takes a mutex because the algorithm mutates per-target state on every
// pick. That is fine at gateway scale; a lock-free variant can come later.
type smoothWRR struct {
	mu    sync.Mutex
	nodes []wrrNode
	total int
}

type wrrNode struct {
	target  *Target
	weight  int
	current int
}

func newSmoothWRR(targets []*Target) *smoothWRR {
	s := &smoothWRR{nodes: make([]wrrNode, len(targets))}
	for i, t := range targets {
		w := t.Weight
		if w <= 0 {
			w = 1
		}
		s.nodes[i] = wrrNode{target: t, weight: w}
		s.total += w
	}
	return s
}

func (s *smoothWRR) Next() *Target {
	s.mu.Lock()
	defer s.mu.Unlock()

	var best *wrrNode
	for i := range s.nodes {
		n := &s.nodes[i]
		n.current += n.weight
		if best == nil || n.current > best.current {
			best = n
		}
	}
	best.current -= s.total
	return best.target
}
