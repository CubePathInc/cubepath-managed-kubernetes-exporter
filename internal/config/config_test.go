package config_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/CubePathInc/cubepath-k8s-exporter/internal/config"
)

const secret = "tok-very-secret-123456"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func parse(t *testing.T, args []string, e map[string]string) (config.Config, error) {
	t.Helper()
	var out bytes.Buffer
	return config.Parse(args, env(e), &out)
}

func TestDefaults(t *testing.T) {
	cfg, err := parse(t, nil, map[string]string{"CUBEPATH_API_TOKEN": secret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != "https://api.cubepath.com" || cfg.ListenAddress != ":9777" || cfg.TelemetryPath != "/metrics" ||
		cfg.RefreshInterval != 60*time.Second || cfg.LogFormat != "text" || cfg.LogLevel.String() != "INFO" ||
		len(cfg.Clusters) != 0 || cfg.ProjectID != "" || cfg.Token.Reveal() != secret {
		t.Fatalf("defaults: %v", cfg)
	}
}

func TestEnvironmentVariables(t *testing.T) {
	cfg, err := parse(t, nil, map[string]string{
		"CUBEPATH_API_TOKEN":                   secret,
		"CUBEPATH_EXPORTER_API_URL":            "http://127.0.0.1:9999/",
		"CUBEPATH_EXPORTER_WEB_LISTEN_ADDRESS": "127.0.0.1:1",
		"CUBEPATH_EXPORTER_WEB_TELEMETRY_PATH": "/m",
		"CUBEPATH_EXPORTER_CLUSTER":            "AAAA-1, bbbb-2",
		"CUBEPATH_EXPORTER_PROJECT_ID":         "42",
		"CUBEPATH_EXPORTER_LOG_LEVEL":          "debug",
		"CUBEPATH_EXPORTER_LOG_FORMAT":         "json",
		"CUBEPATH_EXPORTER_REFRESH_INTERVAL":   "45s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != "http://127.0.0.1:9999" || cfg.ListenAddress != "127.0.0.1:1" || cfg.TelemetryPath != "/m" ||
		strings.Join(cfg.Clusters, ",") != "aaaa-1,bbbb-2" || cfg.ProjectID != "42" || cfg.LogLevel.String() != "DEBUG" ||
		cfg.LogFormat != "json" || cfg.RefreshInterval != 45*time.Second {
		t.Fatalf("config: %v", cfg)
	}
}

func TestFlagOverridesEnvironment(t *testing.T) {
	cfg, err := parse(t, []string{"--web.listen-address=:1234", "--cluster", "cc-1", "--cluster=cc-2"}, map[string]string{
		"CUBEPATH_API_TOKEN":                   secret,
		"CUBEPATH_EXPORTER_WEB_LISTEN_ADDRESS": ":1",
		"CUBEPATH_EXPORTER_CLUSTER":            "env-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddress != ":1234" || strings.Join(cfg.Clusters, ",") != "cc-1,cc-2" {
		t.Fatalf("config: %v", cfg)
	}
}

func TestTokenFileWinsAndIsTrimmed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  file-token-abc \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := parse(t, []string{"--api-token-file", path}, map[string]string{"CUBEPATH_API_TOKEN": secret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token.Reveal() != "file-token-abc" {
		t.Fatalf("the file must win, got %q", cfg.Token.Reveal())
	}
	// Through the environment variable of the flag.
	cfg, err = parse(t, nil, map[string]string{"CUBEPATH_EXPORTER_API_TOKEN_FILE": path})
	if err != nil || cfg.Token.Reveal() != "file-token-abc" {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
}

func TestNoTokenFlag(t *testing.T) {
	if _, err := parse(t, []string{"--api-token", "x"}, map[string]string{"CUBEPATH_API_TOKEN": secret}); err == nil {
		t.Fatal("there must be no --api-token flag")
	}
	if _, err := parse(t, nil, nil); err == nil || !strings.Contains(err.Error(), "CUBEPATH_API_TOKEN") {
		t.Fatalf("expected a missing token error, got %v", err)
	}
}

func TestTokenNeverPrinted(t *testing.T) {
	cfg, err := parse(t, nil, map[string]string{"CUBEPATH_API_TOKEN": secret})
	if err != nil {
		t.Fatal(err)
	}
	outputs := []string{
		cfg.String(), fmt.Sprint(cfg), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg),
		fmt.Sprintf("%v", &cfg), fmt.Sprintf("%s", cfg.Token), fmt.Sprintf("%q", cfg.Token),
	}
	var logs bytes.Buffer
	cfg.NewLogger(&logs).Info("config", "config", cfg, "token", cfg.Token)
	cfg.NewLogger(&logs).Info("config", "cfg", fmt.Sprintf("%+v", cfg))
	outputs = append(outputs, logs.String())
	for _, o := range outputs {
		if strings.Contains(o, secret) {
			t.Fatalf("token leaked: %s", o)
		}
	}
	if !strings.Contains(cfg.String(), "[redacted]") {
		t.Fatal("the redaction marker is expected")
	}
}

func TestTokenNotInErrors(t *testing.T) {
	// A token-looking value in the wrong place must not be echoed by validation errors.
	_, err := parse(t, []string{"--api-url", "http://example.org/" + secret}, map[string]string{"CUBEPATH_API_TOKEN": secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err=%v", err)
	}
	_, err = parse(t, []string{"--api-token-file", "/nonexistent/" + "t"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	_, err = parse(t, nil, map[string]string{"CUBEPATH_API_TOKEN": "has space " + secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err=%v", err)
	}
}

func TestValidation(t *testing.T) {
	base := map[string]string{"CUBEPATH_API_TOKEN": secret}
	bad := [][]string{
		{"--refresh-interval", "10s"},
		{"--api-url", "http://api.cubepath.com"},
		{"--api-url", "nonsense"},
		{"--web.telemetry-path", "metrics"},
		{"--web.telemetry-path", "/"},
		{"--web.telemetry-path", "/healthz"},
		{"--web.telemetry-path", "/readyz"},
		{"--web.telemetry-path", "/metrics/"},
		{"--web.telemetry-path", "/m{x}"},
		{"--web.telemetry-path", "/a b"},
		{"--web.telemetry-path", "/a//b"},
		{"--web.listen-address", ""},
		{"--project-id", "abc"},
		{"--cluster", "bad uuid"},
		{"--cluster", "../x"},
		{"--log.level", "loud"},
		{"--log.format", "xml"},
		{"extra"},
	}
	for _, args := range bad {
		if _, err := parse(t, args, base); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
	for _, ok := range []string{"/metrics", "/cubepath/metrics", "/m-1.x_y"} {
		if _, err := parse(t, []string{"--web.telemetry-path", ok}, base); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	if _, err := parse(t, []string{"--refresh-interval", "30s", "--api-url", "http://localhost:8000"}, base); err != nil {
		t.Errorf("valid options rejected: %v", err)
	}
}

func TestVersionFlag(t *testing.T) {
	_, err := parse(t, []string{"--version"}, nil)
	if !errors.Is(err, config.ErrVersion) {
		t.Fatalf("err=%v", err)
	}
}
