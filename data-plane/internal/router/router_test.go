package router

import (
	"testing"
)

func build(t *testing.T, routes ...Route[string]) *Router[string] {
	t.Helper()
	r, err := New(routes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func prefix(v string) PathMatch { return PathMatch{PathPrefix, v} }
func exact(v string) PathMatch  { return PathMatch{PathExact, v} }

type probe struct {
	host, method, path string
	want               string // "" = expect no match
}

func check(t *testing.T, r *Router[string], probes []probe) {
	t.Helper()
	for _, p := range probes {
		res, ok := r.Match(p.host, p.method, p.path)
		got := ""
		if ok {
			got = res.Value
		}
		if got != p.want {
			t.Errorf("Match(%q, %s, %q) = %q, want %q", p.host, p.method, p.path, got, p.want)
		}
	}
}

func TestExactBeatsPrefix(t *testing.T) {
	r := build(t,
		Route[string]{Paths: []PathMatch{prefix("/api")}, Value: "prefix"},
		Route[string]{Paths: []PathMatch{exact("/api")}, Value: "exact"},
	)
	check(t, r, []probe{
		{"", "GET", "/api", "exact"},
		{"", "GET", "/api/x", "prefix"},
		{"", "GET", "/other", ""},
	})
}

func TestLongestPrefixWins(t *testing.T) {
	r := build(t,
		Route[string]{Paths: []PathMatch{prefix("/api")}, Value: "short"},
		Route[string]{Paths: []PathMatch{prefix("/api/users")}, Value: "long"},
	)
	check(t, r, []probe{
		{"", "GET", "/api", "short"},
		{"", "GET", "/api/orders", "short"},
		{"", "GET", "/api/users", "long"},
		{"", "GET", "/api/users/42/profile", "long"},
	})
}

func TestPrefixMatchesWholeSegmentsOnly(t *testing.T) {
	r := build(t, Route[string]{Paths: []PathMatch{prefix("/api/users")}, Value: "u"})
	check(t, r, []probe{
		{"", "GET", "/api/users", "u"},
		{"", "GET", "/api/users/1", "u"},
		{"", "GET", "/api/usersfoo", ""},
		{"", "GET", "/api", ""},
	})
}

func TestRootPrefixCatchesEverything(t *testing.T) {
	r := build(t, Route[string]{Paths: []PathMatch{prefix("/")}, Value: "root"})
	check(t, r, []probe{
		{"", "GET", "/", "root"},
		{"", "GET", "/a/b/c", "root"},
	})
}

func TestMethodFilterFallsThroughToShorterPrefix(t *testing.T) {
	r := build(t,
		Route[string]{Methods: []string{"GET"}, Paths: []PathMatch{prefix("/api/users")}, Value: "get-users"},
		Route[string]{Paths: []PathMatch{prefix("/api")}, Value: "api"},
	)
	check(t, r, []probe{
		{"", "GET", "/api/users/1", "get-users"},
		{"", "POST", "/api/users/1", "api"}, // deeper route rejects POST -> fall back
		{"", "POST", "/api", "api"},
	})
}

func TestMethodsAreCaseInsensitiveInConfig(t *testing.T) {
	r := build(t, Route[string]{Methods: []string{"get"}, Paths: []PathMatch{prefix("/x")}, Value: "x"})
	check(t, r, []probe{{"", "GET", "/x", "x"}, {"", "POST", "/x", ""}})
}

func TestHostPrecedence(t *testing.T) {
	r := build(t,
		Route[string]{Hosts: []string{"api.example.com"}, Paths: []PathMatch{prefix("/")}, Value: "exact-host"},
		Route[string]{Hosts: []string{"*.example.com"}, Paths: []PathMatch{prefix("/")}, Value: "wildcard"},
		Route[string]{Paths: []PathMatch{prefix("/")}, Value: "any"},
	)
	check(t, r, []probe{
		{"api.example.com", "GET", "/", "exact-host"},
		{"API.Example.COM:8443", "GET", "/", "exact-host"}, // case + port ignored
		{"web.example.com", "GET", "/", "wildcard"},
		{"a.b.example.com", "GET", "/", "any"}, // wildcard covers ONE label
		{"example.com", "GET", "/", "any"},
		{"other.org", "GET", "/", "any"},
	})
}

func TestHostTableFallsThroughWhenPathDoesNotMatch(t *testing.T) {
	r := build(t,
		Route[string]{Hosts: []string{"api.example.com"}, Paths: []PathMatch{prefix("/v1")}, Value: "host-v1"},
		Route[string]{Paths: []PathMatch{prefix("/")}, Value: "any"},
	)
	check(t, r, []probe{
		{"api.example.com", "GET", "/v1/x", "host-v1"},
		{"api.example.com", "GET", "/v2/x", "any"},
	})
}

func TestPriorityBreaksTies(t *testing.T) {
	r := build(t,
		Route[string]{Priority: 0, Paths: []PathMatch{prefix("/x")}, Value: "low"},
		Route[string]{Priority: 10, Paths: []PathMatch{prefix("/x")}, Value: "high"},
	)
	check(t, r, []probe{{"", "GET", "/x/1", "high"}})
}

func TestDefinitionOrderBreaksEqualPriority(t *testing.T) {
	r := build(t,
		Route[string]{Paths: []PathMatch{prefix("/x")}, Value: "first"},
		Route[string]{Paths: []PathMatch{prefix("/x")}, Value: "second"},
	)
	check(t, r, []probe{{"", "GET", "/x", "first"}})
}

func TestResultReportsMatchedPathRule(t *testing.T) {
	r := build(t, Route[string]{Paths: []PathMatch{prefix("/api/users/")}, Value: "u"}) // trailing slash cleaned
	res, ok := r.Match("", "GET", "/api/users/42")
	if !ok || res.Path != "/api/users" || res.Type != PathPrefix {
		t.Fatalf("got %+v ok=%v", res, ok)
	}
}

func TestNewRejectsBadRoutes(t *testing.T) {
	cases := map[string]Route[string]{
		"no paths":         {Value: "x"},
		"relative path":    {Paths: []PathMatch{prefix("api")}},
		"unknown type":     {Paths: []PathMatch{{"regex", "/x"}}},
		"mid wildcard":     {Hosts: []string{"a.*.example.com"}, Paths: []PathMatch{prefix("/")}},
		"bare wildcard":    {Hosts: []string{"*."}, Paths: []PathMatch{prefix("/")}},
		"trailing * label": {Hosts: []string{"*.*.com"}, Paths: []PathMatch{prefix("/")}},
	}
	for name, rt := range cases {
		if _, err := New([]Route[string]{rt}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func BenchmarkMatch(b *testing.B) {
	r, err := New([]Route[string]{
		{Paths: []PathMatch{prefix("/api/users")}, Value: "users"},
		{Paths: []PathMatch{prefix("/api/orders")}, Value: "orders"},
		{Hosts: []string{"api.example.com"}, Paths: []PathMatch{prefix("/api/users")}, Value: "host-users"},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Match("api.example.com:8080", "GET", "/api/users/42/profile"); !ok {
			b.Fatal("no match")
		}
	}
}
