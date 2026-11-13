// Package router matches requests (host, method, path) to routes.
//
// Precedence, highest first:
//
//  1. Host: exact host > single-label wildcard ("*.example.com") > any host.
//     A more specific host table that has a match always wins.
//  2. Path: exact match > longest matching prefix. Prefixes match on whole
//     path segments, so "/api/users" matches "/api/users/1" but not
//     "/api/usersfoo".
//  3. Ties at the same path: higher Priority first, then definition order.
//
// A request that matches on path but not on method falls through to the next
// candidate (for example a shorter prefix) instead of failing immediately.
//
// Paths passed to Match must already be cleaned (see path.Clean): a leading
// "/", no trailing slash, no "." or ".." segments, no empty segments.
//
// A Router is immutable after New and safe for concurrent use. Lookup cost is
// proportional to the number of path segments, not the number of routes.
package router

import (
	"fmt"
	"net"
	"path"
	"sort"
	"strings"
)

type PathType string

const (
	PathExact  PathType = "exact"
	PathPrefix PathType = "prefix"
)

type PathMatch struct {
	Type  PathType
	Value string
}

// Route is a routing rule carrying an arbitrary payload T.
type Route[T any] struct {
	Hosts    []string // empty or "*" = any host
	Methods  []string // empty = any method
	Paths    []PathMatch
	Priority int
	Value    T
}

// Result is what Match returns: the payload plus which path rule matched,
// which callers need for prefix stripping.
type Result[T any] struct {
	Value T
	Path  string // the (cleaned) path rule that matched
	Type  PathType
}

type entry[T any] struct {
	methods  map[string]struct{} // nil = any
	priority int
	path     string
	typ      PathType
	value    T
}

func (e *entry[T]) allows(method string) bool {
	if e.methods == nil {
		return true
	}
	_, ok := e.methods[method]
	return ok
}

func (e *entry[T]) result() Result[T] {
	return Result[T]{Value: e.value, Path: e.path, Type: e.typ}
}

// node is one path segment in a trie.
type node[T any] struct {
	children map[string]*node[T]
	exact    []*entry[T] // routes that end exactly here
	prefix   []*entry[T] // routes whose prefix ends here
}

type Router[T any] struct {
	exactHosts map[string]*node[T]
	wildHosts  map[string]*node[T] // key: suffix after "*."
	anyHost    *node[T]
}

// New builds a Router. Routes are inserted in order, which is what makes
// "definition order" the final tie-breaker.
func New[T any](routes []Route[T]) (*Router[T], error) {
	r := &Router[T]{
		exactHosts: make(map[string]*node[T]),
		wildHosts:  make(map[string]*node[T]),
		anyHost:    &node[T]{},
	}

	for i, rt := range routes {
		if len(rt.Paths) == 0 {
			return nil, fmt.Errorf("route %d: at least one path is required", i)
		}

		var methods map[string]struct{}
		if len(rt.Methods) > 0 {
			methods = make(map[string]struct{}, len(rt.Methods))
			for _, m := range rt.Methods {
				methods[strings.ToUpper(m)] = struct{}{}
			}
		}

		hosts := rt.Hosts
		if len(hosts) == 0 {
			hosts = []string{""}
		}
		for _, h := range hosts {
			root, err := r.rootFor(h)
			if err != nil {
				return nil, fmt.Errorf("route %d: %w", i, err)
			}
			for _, p := range rt.Paths {
				if p.Type != PathExact && p.Type != PathPrefix {
					return nil, fmt.Errorf("route %d: unknown path type %q", i, p.Type)
				}
				if !strings.HasPrefix(p.Value, "/") {
					return nil, fmt.Errorf("route %d: path %q must start with \"/\"", i, p.Value)
				}
				root.insert(&entry[T]{
					methods:  methods,
					priority: rt.Priority,
					path:     path.Clean(p.Value),
					typ:      p.Type,
					value:    rt.Value,
				})
			}
		}
	}
	return r, nil
}

func (r *Router[T]) rootFor(host string) (*node[T], error) {
	host = strings.ToLower(strings.TrimSpace(host))
	switch {
	case host == "" || host == "*":
		return r.anyHost, nil
	case strings.HasPrefix(host, "*."):
		suffix := host[2:]
		if suffix == "" || strings.Contains(suffix, "*") {
			return nil, fmt.Errorf("invalid wildcard host %q", host)
		}
		return getOrCreate(r.wildHosts, suffix), nil
	case strings.Contains(host, "*"):
		return nil, fmt.Errorf("invalid host %q: only a leading \"*.\" wildcard is supported", host)
	default:
		return getOrCreate(r.exactHosts, host), nil
	}
}

func getOrCreate[T any](m map[string]*node[T], key string) *node[T] {
	n := m[key]
	if n == nil {
		n = &node[T]{}
		m[key] = n
	}
	return n
}

func segments(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func (n *node[T]) insert(e *entry[T]) {
	cur := n
	for _, seg := range segments(e.path) {
		if cur.children == nil {
			cur.children = make(map[string]*node[T])
		}
		child := cur.children[seg]
		if child == nil {
			child = &node[T]{}
			cur.children[seg] = child
		}
		cur = child
	}

	list := &cur.prefix
	if e.typ == PathExact {
		list = &cur.exact
	}
	*list = append(*list, e)
	// Stable, so equal priorities keep definition order.
	sort.SliceStable(*list, func(i, j int) bool {
		return (*list)[i].priority > (*list)[j].priority
	})
}

// Match finds the best route. host may include a port. path must be cleaned.
func (r *Router[T]) Match(host, method, path string) (Result[T], bool) {
	host = normalizeHost(host)

	if n := r.exactHosts[host]; n != nil {
		if res, ok := n.match(method, path); ok {
			return res, true
		}
	}
	// Wildcards cover exactly one label: "a.example.com" -> "example.com".
	if i := strings.IndexByte(host, '.'); i > 0 {
		if n := r.wildHosts[host[i+1:]]; n != nil {
			if res, ok := n.match(method, path); ok {
				return res, true
			}
		}
	}
	return r.anyHost.match(method, path)
}

func (n *node[T]) match(method, p string) (Result[T], bool) {
	// Remember every node on the walk that has prefix routes, so that if the
	// deepest one rejects the method we can fall back to a shorter prefix.
	// The fixed array keeps typical lookups allocation-free.
	var buf [16]*node[T]
	prefixes := buf[:0]

	cur := n
	if len(cur.prefix) > 0 {
		prefixes = append(prefixes, cur)
	}

	complete := true // did we consume the whole path?
	rest := p
	for {
		rest = strings.TrimPrefix(rest, "/")
		if rest == "" {
			break
		}
		seg := rest
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seg, rest = rest[:i], rest[i:]
		} else {
			rest = ""
		}
		child := cur.children[seg]
		if child == nil {
			complete = false
			break
		}
		cur = child
		if len(cur.prefix) > 0 {
			prefixes = append(prefixes, cur)
		}
	}

	if complete {
		for _, e := range cur.exact {
			if e.allows(method) {
				return e.result(), true
			}
		}
	}
	for i := len(prefixes) - 1; i >= 0; i-- {
		for _, e := range prefixes[i].prefix {
			if e.allows(method) {
				return e.result(), true
			}
		}
	}
	var zero Result[T]
	return zero, false
}

func normalizeHost(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	return strings.ToLower(strings.TrimSuffix(h, "."))
}
