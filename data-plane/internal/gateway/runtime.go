package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"apigw/dataplane/internal/balancer"
	"apigw/dataplane/internal/config"
	"apigw/dataplane/internal/router"
)

// Runtime is a config snapshot compiled into ready-to-serve objects: the
// route trie, load balancers, reverse proxies and connection pools.
//
// It is immutable after Compile. The gateway swaps whole Runtimes atomically,
// so request handling never takes a lock and never sees a half-applied config.
type Runtime struct {
	version  uint64
	loadedAt time.Time
	router   *router.Router[*routeRT]
	services []*serviceRT
	routes   int
}

type serviceRT struct {
	name           string
	lb             balancer.Balancer
	proxy          *httputil.ReverseProxy
	transport      *http.Transport
	requestTimeout time.Duration
}

type routeRT struct {
	name         string
	svc          *serviceRT
	stripPrefix  bool
	preserveHost bool
}

// Close releases idle upstream connections. In-flight requests are not
// interrupted; their connections simply go idle and expire on their own.
func (rt *Runtime) Close() {
	for _, s := range rt.services {
		s.transport.CloseIdleConnections()
	}
}

// Compile turns a validated Config into a Runtime. Any error means nothing was
// published, so the caller keeps serving the previous Runtime.
func Compile(cfg *config.Config) (*Runtime, error) {
	rt := &Runtime{
		version:  cfg.Version,
		loadedAt: time.Now(),
		routes:   len(cfg.Routes),
	}

	byName := make(map[string]*serviceRT, len(cfg.Services))
	for _, sc := range cfg.Services {
		targets := make([]*balancer.Target, 0, len(sc.Targets))
		for _, tc := range sc.Targets {
			u, err := url.Parse(tc.URL)
			if err != nil {
				return nil, fmt.Errorf("service %q: target %q: %w", sc.Name, tc.URL, err)
			}
			targets = append(targets, &balancer.Target{URL: u, Weight: tc.Weight})
		}
		lb, err := balancer.New(sc.LBAlgorithm, targets)
		if err != nil {
			return nil, fmt.Errorf("service %q: %w", sc.Name, err)
		}

		tr := newTransport(sc)
		s := &serviceRT{
			name:           sc.Name,
			lb:             lb,
			transport:      tr,
			proxy:          newReverseProxy(tr),
			requestTimeout: ms(sc.RequestTimeoutMs),
		}
		byName[sc.Name] = s
		rt.services = append(rt.services, s)
	}

	routes := make([]router.Route[*routeRT], 0, len(cfg.Routes))
	for _, rc := range cfg.Routes {
		svc, ok := byName[rc.Service]
		if !ok {
			return nil, fmt.Errorf("route %q: unknown service %q", rc.Name, rc.Service)
		}
		paths := make([]router.PathMatch, len(rc.Paths))
		for i, p := range rc.Paths {
			paths[i] = router.PathMatch{Type: router.PathType(p.Type), Value: p.Value}
		}
		routes = append(routes, router.Route[*routeRT]{
			Hosts:    rc.Hosts,
			Methods:  rc.Methods,
			Paths:    paths,
			Priority: rc.Priority,
			Value: &routeRT{
				name:         rc.Name,
				svc:          svc,
				stripPrefix:  rc.StripPrefix,
				preserveHost: rc.PreserveHost,
			},
		})
	}

	rtr, err := router.New(routes)
	if err != nil {
		return nil, err
	}
	rt.router = rtr
	return rt, nil
}

func ms(v int) time.Duration { return time.Duration(v) * time.Millisecond }

// newTransport builds one connection pool per service.
func newTransport(s config.Service) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   ms(s.ConnectTimeoutMs),
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		// Never route upstream traffic through HTTP(S)_PROXY from the
		// environment: the default transport would.
		Proxy:       nil,
		DialContext: dialer.DialContext,

		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: ms(s.ResponseHeaderTimeoutMs),
		ExpectContinueTimeout: time.Second,

		// The stdlib default of 2 idle connections per host is the classic
		// gateway performance trap: under load it would constantly tear down
		// and re-dial upstream connections.
		MaxIdleConns:        1024,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	}
}

func newReverseProxy(tr http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// Rewrite (not Director): the proxy strips hop-by-hop headers and any
		// client-supplied Forwarded/X-Forwarded-* headers before calling it,
		// so a client cannot spoof its address to our upstreams.
		Rewrite:      rewrite,
		Transport:    tr,
		ErrorHandler: proxyErrorHandler,
		ModifyResponse: func(resp *http.Response) error {
			// Our request ID is already on the response; don't add a second
			// value if the upstream echoes its own.
			resp.Header.Del(requestIDHeader)
			return nil
		},
		// We log failures ourselves, once, in the access log.
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

// rewrite points the outbound request at the target chosen in ServeHTTP.
func rewrite(pr *httputil.ProxyRequest) {
	st := stateFrom(pr.In.Context())
	out, t := pr.Out, st.target

	out.URL.Scheme = t.URL.Scheme
	out.URL.Host = t.URL.Host
	out.URL.Path = joinPath(t.URL.Path, st.upstreamPath)
	out.URL.RawPath = ""
	// RawQuery is already copied from the inbound request.

	if st.route.preserveHost {
		out.Host = pr.In.Host
	} else {
		out.Host = t.URL.Host
	}
	pr.SetXForwarded()
}

// proxyErrorHandler converts transport failures into gateway responses.
func proxyErrorHandler(w http.ResponseWriter, r *http.Request, err error) {
	st := stateFrom(r.Context())
	st.err = err.Error()

	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		// The client went away; there is nobody to answer. 499 is nginx's
		// convention for "client closed request" and is for the log only.
		if rec, ok := w.(*statusRecorder); ok {
			rec.status = 499
		}
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		writeError(w, http.StatusGatewayTimeout, "upstream_timeout", "the upstream did not respond in time")
	default:
		writeError(w, http.StatusBadGateway, "bad_gateway", "the upstream is unavailable")
	}
}
