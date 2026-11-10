// Package server is the HTTP surface of the exporter.
package server

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/collector"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/snapshot"
	"github.com/CubePathInc/cubepath-k8s-exporter/internal/version"
)

// NewRegistry returns a registry with the exporter collector and the Go and process collectors.
func NewRegistry(store *snapshot.Store) *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collector.New(store, nil),
	)
	return reg
}

// Handler builds the HTTP handler: metrics, /healthz, /readyz and a landing page.
func Handler(reg *prometheus.Registry, store *snapshot.Store, telemetryPath string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+telemetryPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if !store.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintln(w, "no successful refresh yet")
			return
		}
		_, _ = fmt.Fprintln(w, "ready")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>CubePath Kubernetes exporter</title></head>
<body><h1>CubePath Kubernetes exporter</h1>
<p>Version %s</p>
<ul><li><a href="%s">Metrics</a></li><li><a href="/healthz">Health</a></li><li><a href="/readyz">Readiness</a></li></ul>
</body></html>
`, html.EscapeString(version.Version), html.EscapeString(telemetryPath))
	})
	return mux
}

// Serve serves h on ln until ctx is done, then shuts down gracefully.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown did not finish cleanly")
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
