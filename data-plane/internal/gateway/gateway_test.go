package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"apigw/dataplane/internal/config"
)

type echoReply struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Query string `json:"query"`
	Host  string `json:"host"`
	XFF   string `json:"xff"`
	RID   string `json:"rid"`
}

// echoServer is a real HTTP upstream that reports what it received.
func echoServer(t *testing.T, name string, delay time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(echoReply{
			Name:  name,
			Path:  r.URL.Path,
			Query: r.URL.RawQuery,
			Host:  r.Host,
			XFF:   r.Header.Get("X-Forwarded-For"),
			RID:   r.Header.Get("X-Request-Id"),
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func parseConfig(t *testing.T, js string) *config.Config {
	t.Helper()
	cfg, err := config.Parse(strings.NewReader(js))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func startGateway(t *testing.T, js string) (*Gateway, *httptest.Server) {
	t.Helper()
	gw := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := gw.Apply(parseConfig(t, js)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := httptest.NewServer(gw)
	t.Cleanup(srv.Close)
	return gw, srv
}

func do(t *testing.T, method, target string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func echoOf(t *testing.T, body []byte) echoReply {
	t.Helper()
	var e echoReply
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not an echo reply: %q (%v)", body, err)
	}
	return e
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e map[string]string
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("not a JSON error: %q", body)
	}
	return e["error"]
}

func TestProxiesAndRewritesPaths(t *testing.T) {
	up := echoServer(t, "up", 0)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q}]}],
	  "routes":[
	    {"name":"users","service":"svc","paths":[{"type":"prefix","value":"/api/users"}],"strip_prefix":true},
	    {"name":"orders","service":"svc","paths":[{"type":"prefix","value":"/api/orders"}]}
	  ]}`, up.URL))

	cases := []struct{ req, wantPath, wantQuery string }{
		{"/api/users/42?x=1&y=two", "/42", "x=1&y=two"},  // prefix stripped, query intact
		{"/api/users", "/", ""},                          // stripping everything leaves "/"
		{"/api/orders/7", "/api/orders/7", ""},           // not stripped
		{"/api/orders/../orders/8", "/api/orders/8", ""}, // cleaned before routing AND forwarding
	}
	for _, c := range cases {
		resp, body := do(t, "GET", gw.URL+c.req, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d body %s", c.req, resp.StatusCode, body)
		}
		e := echoOf(t, body)
		if e.Path != c.wantPath || e.Query != c.wantQuery {
			t.Errorf("%s: upstream saw path=%q query=%q, want path=%q query=%q",
				c.req, e.Path, e.Query, c.wantPath, c.wantQuery)
		}
	}
}

func TestTargetBasePathIsPrepended(t *testing.T) {
	up := echoServer(t, "up", 0)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/v1"}],"strip_prefix":true}]}`,
		up.URL+"/internal/"))

	_, body := do(t, "GET", gw.URL+"/v1/things", nil)
	if got := echoOf(t, body).Path; got != "/internal/things" {
		t.Fatalf("path = %q", got)
	}
}

func TestUnknownRouteIs404JSON(t *testing.T) {
	up := echoServer(t, "up", 0)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","methods":["GET"],"paths":[{"type":"exact","value":"/only"}]}]}`, up.URL))

	for _, c := range []struct{ method, path string }{
		{"GET", "/nope"},
		{"POST", "/only"}, // path matches, method does not
	} {
		resp, body := do(t, c.method, gw.URL+c.path, nil)
		if resp.StatusCode != 404 || errorCode(t, body) != "route_not_found" {
			t.Errorf("%s %s: got %d %s", c.method, c.path, resp.StatusCode, body)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
	}
}

func TestRoundRobinAcrossTargets(t *testing.T) {
	a, b := echoServer(t, "a", 0), echoServer(t, "b", 0)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q},{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, a.URL, b.URL))

	counts := map[string]int{}
	for i := 0; i < 10; i++ {
		_, body := do(t, "GET", gw.URL+"/x", nil)
		counts[echoOf(t, body).Name]++
	}
	if counts["a"] != 5 || counts["b"] != 5 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestDeadUpstreamIs502(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // nothing listens here any more

	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, deadURL))

	resp, body := do(t, "GET", gw.URL+"/x", nil)
	if resp.StatusCode != 502 || errorCode(t, body) != "bad_gateway" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestSlowUpstreamIs504(t *testing.T) {
	slow := echoServer(t, "slow", 2*time.Second)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","response_header_timeout_ms":100,"targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, slow.URL))

	start := time.Now()
	resp, body := do(t, "GET", gw.URL+"/x", nil)
	if resp.StatusCode != 504 || errorCode(t, body) != "upstream_timeout" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("took %v; the timeout did not fire", time.Since(start))
	}
}

func TestOverallRequestTimeoutIs504(t *testing.T) {
	slow := echoServer(t, "slow", 2*time.Second)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","request_timeout_ms":150,"targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, slow.URL))

	resp, body := do(t, "GET", gw.URL+"/x", nil)
	if resp.StatusCode != 504 {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestClientForwardedHeadersAreNotTrusted(t *testing.T) {
	up := echoServer(t, "up", 0)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, up.URL))

	_, body := do(t, "GET", gw.URL+"/x", map[string]string{"X-Forwarded-For": "6.6.6.6"})
	xff := echoOf(t, body).XFF
	if xff == "" || strings.Contains(xff, "6.6.6.6") {
		t.Fatalf("upstream saw X-Forwarded-For = %q; spoofed value must be dropped", xff)
	}
}

func TestRequestID(t *testing.T) {
	up := echoServer(t, "up", 0)
	_, gw := startGateway(t, fmt.Sprintf(`{
	  "services":[{"name":"svc","targets":[{"url":%q}]}],
	  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, up.URL))

	// Generated when absent, and the upstream receives the same one.
	resp, body := do(t, "GET", gw.URL+"/x", nil)
	id := resp.Header.Get("X-Request-Id")
	if len(id) != 24 || echoOf(t, body).RID != id {
		t.Fatalf("response id %q, upstream saw %q", id, echoOf(t, body).RID)
	}

	// A well-formed inbound ID is preserved end to end, with no duplicates.
	resp, body = do(t, "GET", gw.URL+"/x", map[string]string{"X-Request-Id": "trace-abc.123"})
	if vals := resp.Header.Values("X-Request-Id"); len(vals) != 1 || vals[0] != "trace-abc.123" {
		t.Fatalf("response ids = %v", vals)
	}
	if echoOf(t, body).RID != "trace-abc.123" {
		t.Fatalf("upstream saw %q", echoOf(t, body).RID)
	}

	// A malformed one is replaced.
	resp, _ = do(t, "GET", gw.URL+"/x", map[string]string{"X-Request-Id": "bad id with spaces"})
	if got := resp.Header.Get("X-Request-Id"); got == "bad id with spaces" || len(got) != 24 {
		t.Fatalf("malformed id was not replaced: %q", got)
	}
}

func TestHostHeaderHandling(t *testing.T) {
	up := echoServer(t, "up", 0)
	upHost := mustHost(t, up.URL)

	for _, preserve := range []bool{false, true} {
		_, gw := startGateway(t, fmt.Sprintf(`{
		  "services":[{"name":"svc","targets":[{"url":%q}]}],
		  "routes":[{"name":"r","service":"svc","preserve_host":%t,"paths":[{"type":"prefix","value":"/"}]}]}`,
			up.URL, preserve))

		_, body := do(t, "GET", gw.URL+"/x", nil)
		got := echoOf(t, body).Host
		want := upHost
		if preserve {
			want = mustHost(t, gw.URL)
		}
		if got != want {
			t.Errorf("preserve_host=%v: upstream saw Host %q, want %q", preserve, got, want)
		}
	}
}

func TestHotSwapAndRejectedConfig(t *testing.T) {
	a, b := echoServer(t, "a", 0), echoServer(t, "b", 0)
	cfgFor := func(upstream string, version int) string {
		return fmt.Sprintf(`{"version":%d,
		  "services":[{"name":"svc","targets":[{"url":%q}]}],
		  "routes":[{"name":"r","service":"svc","paths":[{"type":"prefix","value":"/"}]}]}`, version, upstream)
	}

	gw, srv := startGateway(t, cfgFor(a.URL, 1))
	_, body := do(t, "GET", srv.URL+"/x", nil)
	if echoOf(t, body).Name != "a" {
		t.Fatal("expected upstream a")
	}

	if err := gw.Apply(parseConfig(t, cfgFor(b.URL, 2))); err != nil {
		t.Fatal(err)
	}
	_, body = do(t, "GET", srv.URL+"/x", nil)
	if echoOf(t, body).Name != "b" {
		t.Fatal("expected upstream b after swap")
	}
	if st, _ := gw.Status(); st.ConfigVersion != 2 {
		t.Fatalf("status version = %d", st.ConfigVersion)
	}

	// A config that compiles badly is rejected and the old one keeps serving.
	bad := &config.Config{
		Version:  3,
		Services: []config.Service{{Name: "svc", LBAlgorithm: "round_robin", Targets: []config.Target{{URL: "http://x:1", Weight: 1}}}},
		Routes: []config.Route{{
			Name: "r", Service: "svc",
			Paths: []config.PathMatch{{Type: "prefix", Value: "/"}},
			Hosts: []string{"a.*.example.com"}, // invalid wildcard, caught at compile time
		}},
	}
	if err := gw.Apply(bad); err == nil {
		t.Fatal("expected compile error")
	}
	_, body = do(t, "GET", srv.URL+"/x", nil)
	if echoOf(t, body).Name != "b" {
		t.Fatal("rejected config must not disturb serving")
	}
	if st, _ := gw.Status(); st.ConfigVersion != 2 {
		t.Fatalf("version after rejected apply = %d, want 2", st.ConfigVersion)
	}
}

func TestNotReadyBeforeAnyConfig(t *testing.T) {
	gw := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(gw)
	defer srv.Close()

	resp, body := do(t, "GET", srv.URL+"/x", nil)
	if resp.StatusCode != 503 || errorCode(t, body) != "not_ready" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if _, ok := gw.Status(); ok {
		t.Fatal("Status should report not ready")
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}
