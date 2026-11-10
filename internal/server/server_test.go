package server_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/server"
	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/snapshot"
)

func get(t *testing.T, h http.Handler, method, path string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	b, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(b), rec.Header()
}

func TestEndpoints(t *testing.T) {
	store := snapshot.NewStore()
	h := server.Handler(server.NewRegistry(store), store, "/metrics")

	if code, body, _ := get(t, h, "GET", "/healthz"); code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("healthz: %d %q", code, body)
	}
	if code, _, _ := get(t, h, "GET", "/readyz"); code != 503 {
		t.Fatalf("readyz before a refresh: %d", code)
	}
	store.Stats().SetRefresh(time.Now(), false, time.Second, nil)
	if code, _, _ := get(t, h, "GET", "/readyz"); code != 503 {
		t.Fatalf("readyz after a failed refresh: %d", code)
	}
	store.Stats().SetRefresh(time.Now(), true, time.Second, map[string]bool{"clusters": true})
	if code, _, _ := get(t, h, "GET", "/readyz"); code != 200 {
		t.Fatalf("readyz after a successful refresh: %d", code)
	}

	code, body, hdr := get(t, h, "GET", "/metrics")
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics: %d %v", code, hdr)
	}
	for _, want := range []string{"cubepath_managed_kubernetes_exporter_build_info", "cubepath_managed_kubernetes_exporter_last_refresh_success 1", "go_goroutines", "process_start_time_seconds"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
	if code, body, _ := get(t, h, "GET", "/"); code != 200 || !strings.Contains(body, `href="/metrics"`) {
		t.Fatalf("landing: %d %q", code, body)
	}
	if code, _, _ := get(t, h, "GET", "/nope"); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
	if code, _, _ := get(t, h, "POST", "/metrics"); code != 405 {
		t.Fatalf("POST /metrics: %d", code)
	}
}

func TestCustomTelemetryPath(t *testing.T) {
	store := snapshot.NewStore()
	h := server.Handler(server.NewRegistry(store), store, "/custom/<x>")
	if code, _, _ := get(t, h, "GET", "/custom/%3Cx%3E"); code != 200 {
		t.Fatalf("custom path: %d", code)
	}
	if _, body, _ := get(t, h, "GET", "/"); strings.Contains(body, "<x>") {
		t.Fatal("the landing page must escape the path")
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	store := snapshot.NewStore()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(ctx, ln, server.Handler(server.NewRegistry(store), store, "/metrics"), nil)
	}()
	resp, err := http.Get("http://" + ln.Addr().String() + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	resp.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not stop")
	}
}
