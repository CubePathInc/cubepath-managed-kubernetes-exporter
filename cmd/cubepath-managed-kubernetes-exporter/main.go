// Command cubepath-managed-kubernetes-exporter exports CubePath Managed Kubernetes data in Prometheus format.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/config"
	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/cubepath"
	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/server"
	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/snapshot"
	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run is main without the process globals. onListen, when set, receives the bound address.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, onListen func(net.Addr)) error {
	cfg, err := config.Parse(args, getenv, stderr)
	if errors.Is(err, config.ErrVersion) {
		fmt.Fprintf(stdout, "cubepath-managed-kubernetes-exporter %s (%s)\n", version.Version, version.Revision)
		return nil
	}
	if errors.Is(err, flag.ErrHelp) {
		return nil // the usage was already printed by the flag package
	}
	if err != nil {
		return err
	}
	log := cfg.NewLogger(stderr)
	log.Info("starting", "version", version.Version, "api_url", cfg.APIURL, "listen", cfg.ListenAddress,
		"refresh_interval", cfg.RefreshInterval.String(), "clusters_filter", len(cfg.Clusters))

	store := snapshot.NewStore()
	client, err := cubepath.New(cubepath.Options{
		BaseURL:   cfg.APIURL,
		Token:     cfg.Token,
		UserAgent: version.UserAgent(),
		Observe:   store.Stats().ObserveRequest,
	})
	if err != nil {
		return err
	}
	refresher := snapshot.NewRefresher(client, store, snapshot.Config{
		ProjectID: cfg.ProjectID,
		Clusters:  cfg.Clusters,
		Interval:  cfg.RefreshInterval,
	}, log, nil)

	ln, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		return err
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	log.Info("listening", "address", ln.Addr().String(), "metrics_path", cfg.TelemetryPath)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		refresher.Run(ctx)
	}()

	reg := server.NewRegistry(store)
	err = server.Serve(ctx, ln, server.Handler(reg, store, cfg.TelemetryPath), log)
	cancel()
	<-done
	log.Info("stopped")
	return err
}
