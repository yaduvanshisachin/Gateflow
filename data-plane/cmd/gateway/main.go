// Command gateway is the data plane: it serves traffic from an in-memory
// routing config and hot-reloads that config without dropping requests.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"apigw/dataplane/internal/config"
	"apigw/dataplane/internal/gateway"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath = flag.String("config", "config.json", "path to the routing config (JSON)")
		listen  = flag.String("listen", ":8080", "address for proxied traffic")
		admin   = flag.String("admin", "127.0.0.1:9901", "address for /healthz and /status (empty disables)")
		every   = flag.Duration("reload-interval", 2*time.Second, "how often to check the config file for changes")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Fail fast: refuse to start on a bad config. (A bad *reload* later is
	// rejected and the previous config keeps serving.)
	gw := gateway.New(log)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := gw.Apply(cfg); err != nil {
		return err
	}
	log.Info("config loaded", "path", *cfgPath, "version", cfg.Version,
		"services", len(cfg.Services), "routes", len(cfg.Routes))

	go (&config.FileWatcher{Path: *cfgPath, Interval: *every, Log: log}).Watch(ctx, gw.Apply)

	servers := []*http.Server{{
		Addr:    *listen,
		Handler: gw,
		// Bound slow-header attacks. There is intentionally no WriteTimeout:
		// it would cut off long downloads and streams. Per-service
		// request_timeout_ms bounds the exchange instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}}
	if *admin != "" {
		servers = append(servers, &http.Server{
			Addr:              *admin,
			Handler:           adminHandler(gw),
			ReadHeaderTimeout: 5 * time.Second,
		})
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errc:
		return err
	}

	// Graceful: stop accepting, let in-flight requests finish.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdownCtx)
	}
	return nil
}

// adminHandler lives on its own port so admin paths can never collide with,
// or be reachable through, a user-defined route.
func adminHandler(gw *gateway.Gateway) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := gw.Status(); !ok {
			http.Error(w, "no config loaded", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		st, _ := gw.Status()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	return mux
}
