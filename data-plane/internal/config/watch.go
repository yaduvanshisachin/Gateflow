package config

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// FileWatcher re-applies the config file whenever it changes (polled) or the
// process receives SIGHUP.
//
// It is the Phase 1 "config source". In Phase 3 a gRPC subscriber replaces it
// and calls the very same apply callback, so nothing downstream changes.
//
// A rejected config never disturbs traffic: the previous runtime keeps serving.
type FileWatcher struct {
	Path     string
	Interval time.Duration
	Log      *slog.Logger
}

type fileStamp struct{ modNano, size int64 }

func stamp(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{fi.ModTime().UnixNano(), fi.Size()}
}

// Watch blocks until ctx is cancelled.
func (w *FileWatcher) Watch(ctx context.Context, apply func(*Config) error) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	tick := time.NewTicker(w.Interval)
	defer tick.Stop()

	last := stamp(w.Path)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			last = stamp(w.Path)
			w.reload(apply)
		case <-tick.C:
			cur := stamp(w.Path)
			if cur == last {
				continue
			}
			// Remember the stamp even if the reload fails, so a broken file
			// is reported once rather than on every tick.
			last = cur
			w.reload(apply)
		}
	}
}

func (w *FileWatcher) reload(apply func(*Config) error) {
	cfg, err := Load(w.Path)
	if err == nil {
		err = apply(cfg)
	}
	if err != nil {
		w.Log.Error("config reload rejected; still serving previous config",
			"path", w.Path, "error", err)
		return
	}
	w.Log.Info("config reloaded",
		"path", w.Path, "version", cfg.Version,
		"services", len(cfg.Services), "routes", len(cfg.Routes))
}
