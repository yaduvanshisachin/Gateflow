// Package gateway is the data plane's request path: match a route, pick an
// upstream target, proxy the request, and write one access-log line.
package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"apigw/dataplane/internal/balancer"
	"apigw/dataplane/internal/config"
)

// drainDelay is how long a replaced Runtime keeps its idle upstream
// connections before they are closed.
const drainDelay = 30 * time.Second

type Gateway struct {
	rt  atomic.Pointer[Runtime]
	log *slog.Logger
}

func New(log *slog.Logger) *Gateway {
	return &Gateway{log: log}
}

// Apply compiles cfg and, only if that succeeds, atomically swaps it in.
// On error the previous config keeps serving.
func (g *Gateway) Apply(cfg *config.Config) error {
	rt, err := Compile(cfg)
	if err != nil {
		return err
	}
	if old := g.rt.Swap(rt); old != nil {
		time.AfterFunc(drainDelay, old.Close)
	}
	return nil
}

type Status struct {
	ConfigVersion uint64    `json:"config_version"`
	LoadedAt      time.Time `json:"loaded_at"`
	Services      int       `json:"services"`
	Routes        int       `json:"routes"`
}

// Status reports the active config; ok is false until one has been applied.
func (g *Gateway) Status() (Status, bool) {
	rt := g.rt.Load()
	if rt == nil {
		return Status{}, false
	}
	return Status{
		ConfigVersion: rt.version,
		LoadedAt:      rt.loadedAt,
		Services:      len(rt.services),
		Routes:        rt.routes,
	}, true
}

// reqState is per-request scratch space shared between ServeHTTP, the proxy's
// Rewrite/ErrorHandler hooks and the access log. It travels in the context.
// Phase 2's filter chain will grow this into the RequestContext.
type reqState struct {
	route        *routeRT
	target       *balancer.Target
	upstreamPath string
	err          string
}

type stateKey struct{}

func stateFrom(ctx context.Context) *reqState {
	if st, ok := ctx.Value(stateKey{}).(*reqState); ok {
		return st
	}
	return &reqState{}
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w}
	st := &reqState{}

	id := requestID(r)
	r.Header.Set(requestIDHeader, id) // forwarded to the upstream
	rec.Header().Set(requestIDHeader, id)
	r = r.WithContext(context.WithValue(r.Context(), stateKey{}, st))

	defer func() { g.logAccess(r, rec, st, id, start) }()

	// One atomic load; everything below uses this immutable snapshot, so a
	// reload mid-request cannot change behaviour half way through.
	rt := g.rt.Load()
	if rt == nil {
		writeError(rec, http.StatusServiceUnavailable, "not_ready", "no configuration loaded")
		return
	}

	p := cleanPath(r.URL.Path)
	res, ok := rt.router.Match(r.Host, r.Method, p)
	if !ok {
		writeError(rec, http.StatusNotFound, "route_not_found", "no route matches this request")
		return
	}
	route := res.Value

	upstreamPath := p
	if route.stripPrefix && res.Type == "prefix" {
		upstreamPath = stripPrefix(p, res.Path)
	}
	st.route = route
	st.target = route.svc.lb.Next()
	st.upstreamPath = upstreamPath

	ctx, cancel := context.WithTimeout(r.Context(), route.svc.requestTimeout)
	defer cancel()
	r = r.WithContext(ctx)

	route.svc.proxy.ServeHTTP(rec, r)
}

func (g *Gateway) logAccess(r *http.Request, rec *statusRecorder, st *reqState, id string, start time.Time) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}

	// The query string is deliberately not logged: it often carries tokens.
	attrs := []slog.Attr{
		slog.String("request_id", id),
		slog.String("method", r.Method),
		slog.String("host", r.Host),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes", rec.bytes),
		slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
		slog.String("client_ip", clientIP(r)),
		slog.String("user_agent", r.UserAgent()),
	}
	if st.route != nil {
		attrs = append(attrs,
			slog.String("route", st.route.name),
			slog.String("service", st.route.svc.name))
	}
	if st.target != nil {
		attrs = append(attrs, slog.String("upstream", st.target.URL.Host))
	}
	if st.err != "" {
		attrs = append(attrs, slog.String("error", st.err))
	}

	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelWarn
	}
	g.log.LogAttrs(r.Context(), level, "access", attrs...)
}
