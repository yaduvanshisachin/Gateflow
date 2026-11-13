# apigw: a custom API gateway (Go data plane + Java control plane)

A learning project that grows into a production-grade gateway.

```
control-plane/   Java / Spring Boot   admin API, Postgres, config snapshots   (Phase 3)
data-plane/      Go                   the proxy that serves traffic           (Phase 1: done)
proto/           protobuf             the contract between the two            (draft)
```

The control plane can be down and traffic keeps flowing: the data plane holds
its config in memory and never talks to the database.

## Phase 1: what exists

A working reverse proxy using **only the Go standard library** (no `go get` needed):

| Feature | Where |
|---|---|
| Host / method / path routing, exact + longest-prefix, wildcard hosts, priority | `internal/router` |
| Round robin and smooth weighted round robin | `internal/balancer` |
| Strict config parsing and validation (all errors reported at once) | `internal/config` |
| Hot reload (file poll + `SIGHUP`), atomic swap, bad config rejected | `internal/config/watch.go`, `internal/gateway` |
| Reverse proxy, per-service connection pools, connect / header / total timeouts | `internal/gateway/runtime.go` |
| 404 / 502 / 504 JSON errors, request IDs, JSON access logs (`slog`) | `internal/gateway` |
| Admin port with `/healthz` and `/status`, graceful shutdown | `cmd/gateway` |

## Try it

```bash
cd data-plane
go test -race ./...

# four upstreams + one slow one
go run ./cmd/echo -addr 127.0.0.1:9001 -name users-a &
go run ./cmd/echo -addr 127.0.0.1:9002 -name users-b &
go run ./cmd/echo -addr 127.0.0.1:9003 -name orders-heavy &
go run ./cmd/echo -addr 127.0.0.1:9004 -name orders-light &
go run ./cmd/echo -addr 127.0.0.1:9005 -name slow -delay 3s &

go run ./cmd/gateway -config config.example.json

curl -i 'localhost:8080/api/users/42?x=1'   # round robin; upstream sees /42 (prefix stripped)
curl -s localhost:8080/api/orders/1         # weighted 3:1
curl -i localhost:8080/slow                 # 504 after ~1s
curl -s localhost:9901/status               # active config version
```

Now edit `config.example.json` while it runs (change a path, bump `version`).
Within two seconds the new routes are live; a broken file is rejected with an
error in the log and the previous config keeps serving.

## Config reference

```jsonc
{
  "version": 1,
  "services": [{
    "name": "users-svc",
    "lb_algorithm": "round_robin",        // or "weighted"
    "targets": [{ "url": "http://127.0.0.1:9001", "weight": 1 }],
    "connect_timeout_ms": 3000,           // dialing an upstream
    "response_header_timeout_ms": 30000,  // waiting for response headers
    "request_timeout_ms": 60000           // whole exchange, incl. streaming the body
  }],
  "routes": [{
    "name": "users-api",
    "service": "users-svc",
    "hosts": [],                          // empty = any; "api.example.com"; "*.example.com" (one label)
    "methods": ["GET"],                   // empty = any
    "paths": [{ "type": "prefix", "value": "/api/users" }],  // "exact" | "prefix"
    "strip_prefix": true,                 // /api/users/42 -> /42
    "preserve_host": false,               // forward client's Host instead of the target's
    "priority": 0                         // tie-break among equal matches
  }]
}
```

**Match precedence:** most specific host table first; within it, exact path beats
longest prefix; ties go to higher `priority`, then to the route defined first.
Prefixes match whole segments (`/api/users` does not match `/api/usersfoo`).
A method mismatch falls through to the next candidate.

## Design notes worth knowing

- **Immutable runtime + atomic swap.** A config is compiled into a `Runtime`
  (route trie, balancers, proxies, pools) and swapped with one
  `atomic.Pointer`. Requests take no lock and can never see a half-applied
  config. This is the same mechanism Phase 3 will use for gRPC-pushed snapshots.
- **Config source is pluggable.** `FileWatcher` calls `apply(*Config)`. The gRPC
  subscriber will call the same function.
- **Spoof-proof forwarding headers.** `httputil.ReverseProxy.Rewrite` drops any
  client-sent `X-Forwarded-*`; the gateway sets them itself.
- **Route and forward the same path.** The path is cleaned once and used for
  both matching and forwarding, so `/admin/../public` can't be routed as one
  thing and proxied as another.
- **Connection pools are tuned.** The stdlib default of 2 idle connections per
  host is a classic gateway bottleneck; we use 256.
- **Query strings are not logged** (they often carry tokens).
- Router lookup benchmark: ~118 ns, 0 allocations per match.

## Known Phase 1 limitations (deliberate, with a planned phase)

| Limitation | Planned |
|---|---|
| No auth, rate limiting, CORS, filter chain | Phase 2 |
| No Postgres / admin API / gRPC config push | Phase 3 |
| No health checks, retries, circuit breaker, Prometheus / OTel | Phase 4 |
| No Redis rate limits, rollback UI, Docker Compose packaging | Phase 5 |
| Percent-encoded slashes (`%2F`) in paths are decoded before forwarding | revisit with the filter chain |
| No request body size limit | Phase 2 |
| Path-match miss on method returns 404, not 405 | optional |
| gRPC proxying and WebSocket | later, as agreed |

## Roadmap

- [x] **Phase 1**: Go proxy with static config: router, reverse proxy, round robin, timeouts, access logs, hot reload
- [ ] **Phase 2**: filter chain; API key + JWT auth; rate limiting; CORS
- [ ] **Phase 3**: Spring Boot control plane (CRUD, Postgres, Flyway); `.proto` compiled for Java and Go; gRPC snapshot streaming with ACK/NACK
- [ ] **Phase 4**: health checks, retries, circuit breaker, metrics, tracing
- [ ] **Phase 5**: Redis rate limits, node status and rollback, Docker Compose deployment
