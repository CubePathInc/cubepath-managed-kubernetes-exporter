package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/fakeapi"
)

const token = "tok-secret-for-main-test-777"

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// start runs the exporter against the fake and returns its address, its logs and a stop function.
func start(t *testing.T, apiToken string, extra ...string) (addr string, logs *syncBuf, stop func() error) {
	t.Helper()
	fake, err := fakeapi.LoadFixture("../../testdata/fixture", token)
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(fake) // http://127.0.0.1:port, allowed for tests
	t.Cleanup(api.Close)

	logs = &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	listening := make(chan net.Addr, 1)
	done := make(chan error, 1)
	args := append([]string{"--api-url", api.URL, "--web.listen-address", "127.0.0.1:0", "--log.level", "debug"}, extra...)
	envs := map[string]string{"CUBEPATH_API_TOKEN": apiToken}
	go func() {
		done <- run(ctx, args, func(k string) string { return envs[k] }, io.Discard, logs, func(a net.Addr) { listening <- a })
	}()
	select {
	case a := <-listening:
		addr = a.String()
	case err := <-done:
		t.Fatalf("run returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no listener")
	}
	var once sync.Once
	var result error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case result = <-done:
			case <-time.After(10 * time.Second):
				t.Error("the exporter did not stop on cancel")
			}
		})
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return addr, logs, stop
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func waitReady(t *testing.T, addr string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code, _ := fetch(t, "http://"+addr+"/readyz"); code == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("/readyz never answered %d", want)
}

func TestMetricsRenderAgainstFakeAPI(t *testing.T) {
	addr, logs, stop := start(t, token)
	waitReady(t, addr, 200)
	code, body := fetch(t, "http://"+addr+"/metrics")
	if code != 200 {
		t.Fatalf("metrics: %d", code)
	}
	for _, want := range []string{
		`cubepath_managed_kubernetes_cluster_up{cluster="prod",cluster_uuid="11111111-1111-1111-1111-111111111111"} 1`,
		`cubepath_managed_kubernetes_node_ready{cluster="prod",cluster_uuid="11111111-1111-1111-1111-111111111111",node="prod-default-0"} 1`,
		`cubepath_managed_kubernetes_node_vm_cpu_usage_ratio{cluster="prod",cluster_uuid="11111111-1111-1111-1111-111111111111",node="prod-default-0"} 0.125`,
		`cubepath_managed_kubernetes_node_vm_network_receive_bytes_per_second{cluster="prod",cluster_uuid="11111111-1111-1111-1111-111111111111",nic="private",node="prod-default-0"} 2700`,
		`cubepath_managed_kubernetes_node_pool_nodes{cluster="prod",cluster_uuid="11111111-1111-1111-1111-111111111111",pool="default",state="desired"} 2`,
		`cubepath_managed_kubernetes_cluster_monitored{cluster="staging",cluster_uuid="22222222-2222-2222-2222-222222222222"} 0`,
		`cubepath_managed_kubernetes_exporter_last_refresh_success 1`,
		`cubepath_managed_kubernetes_exporter_family_available{family="node_vm"} 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if err := stop(); err != nil {
		t.Fatalf("graceful shutdown returned %v", err)
	}
	if strings.Contains(logs.String(), token) {
		t.Fatal("the token appears in the logs")
	}
	if !strings.Contains(logs.String(), "refresh done") {
		t.Fatalf("logs: %s", logs.String())
	}
}

func TestRejectedTokenStaysNotReadyAndIsNeverLogged(t *testing.T) {
	const wrong = "tok-wrong-and-secret-888"
	addr, logs, _ := start(t, wrong, "--log.format", "json")
	// The first refresh fails: not ready, and the failure is a metric.
	deadline := time.Now().Add(10 * time.Second)
	var body string
	for time.Now().Before(deadline) {
		_, body = fetch(t, "http://"+addr+"/metrics")
		if strings.Contains(body, `cubepath_managed_kubernetes_exporter_last_refresh_success 0`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(body, `cubepath_managed_kubernetes_exporter_last_refresh_success 0`) || !strings.Contains(body, `api_errors_total{code="UNAUTHENTICATED"} 1`) ||
		!strings.Contains(body, `refresh_errors_total{stage="clusters"} 1`) {
		t.Fatalf("metrics:\n%s", body)
	}
	if code, _ := fetch(t, "http://"+addr+"/readyz"); code != 503 {
		t.Fatalf("readyz: %d", code)
	}
	if strings.Contains(body, "cubepath_managed_kubernetes_cluster_") {
		t.Fatal("no cluster series without a snapshot")
	}
	out := logs.String()
	if strings.Contains(out, wrong) || strings.Contains(out, token) || !strings.Contains(out, "UNAUTHENTICATED") {
		t.Fatalf("logs: %s", out)
	}
}

func TestVersionAndBadConfig(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"--version"}, func(string) string { return "" }, &out, io.Discard, nil); err != nil || !strings.HasPrefix(out.String(), "cubepath-managed-kubernetes-exporter ") {
		t.Fatalf("version: %q %v", out.String(), err)
	}
	if err := run(context.Background(), nil, func(string) string { return "" }, io.Discard, io.Discard, nil); err == nil {
		t.Fatal("a missing token must fail")
	}
}

func TestHelpExitsCleanly(t *testing.T) {
	var errOut bytes.Buffer
	for _, flagName := range []string{"--help", "-h"} {
		errOut.Reset()
		if err := run(context.Background(), []string{flagName}, func(string) string { return "" }, io.Discard, &errOut, nil); err != nil {
			t.Fatalf("%s must not be an error: %v", flagName, err)
		}
		if !strings.Contains(errOut.String(), "web.listen-address") {
			t.Fatalf("%s must print the usage: %q", flagName, errOut.String())
		}
	}
}
