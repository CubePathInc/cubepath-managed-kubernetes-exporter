// Package config parses the command line and the environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/cubepath"
	"github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/snapshot"
)

// EnvPrefix prefixes the environment variable of every flag.
const EnvPrefix = "CUBEPATH_EXPORTER_"

// TokenEnv is the environment variable that carries the API token.
const TokenEnv = "CUBEPATH_API_TOKEN"

// DefaultAPIURL is the CubePath API.
const DefaultAPIURL = "https://api.cubepath.com"

// ErrVersion is returned by Parse when --version was requested.
var ErrVersion = errors.New("version requested")

// Config is the validated configuration.
type Config struct {
	Token           cubepath.Secret
	APIURL          string
	ListenAddress   string
	TelemetryPath   string
	Clusters        []string
	ProjectID       string
	LogLevel        slog.Level
	LogFormat       string
	RefreshInterval time.Duration
}

// String never includes the token.
func (c Config) String() string {
	return fmt.Sprintf("Config{Token:%s APIURL:%s ListenAddress:%s TelemetryPath:%s Clusters:%v ProjectID:%s LogLevel:%s LogFormat:%s RefreshInterval:%s}",
		c.Token, c.APIURL, c.ListenAddress, c.TelemetryPath, c.Clusters, c.ProjectID, c.LogLevel, c.LogFormat, c.RefreshInterval)
}

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

var (
	clusterRe   = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
	projectIDRe = regexp.MustCompile(`^[0-9]{1,18}$`)
)

func envName(flagName string) string {
	return EnvPrefix + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(flagName))
}

// Parse reads args and the environment. A flag given on the command line wins over its
// environment variable. It returns ErrVersion for --version.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	fs := flag.NewFlagSet("cubepath-managed-kubernetes-exporter", flag.ContinueOnError)
	fs.SetOutput(out)

	var (
		tokenFile, apiURL, listen, path, projectID, logLevel, logFormat string
		interval                                                        time.Duration
		clusters                                                        stringList
		showVersion                                                     bool
	)
	fs.StringVar(&tokenFile, "api-token-file", "", "Path to a file that holds the CubePath API token. The token can also be set in "+TokenEnv+".")
	fs.StringVar(&apiURL, "api-url", DefaultAPIURL, "Base URL of the CubePath API.")
	fs.StringVar(&listen, "web.listen-address", ":9777", "Address to listen on for the metrics endpoint.")
	fs.StringVar(&path, "web.telemetry-path", "/metrics", "Path under which the metrics are exposed.")
	fs.Var(&clusters, "cluster", "UUID of a cluster to export (repeatable). By default every cluster the token can read.")
	fs.StringVar(&projectID, "project-id", "", "Only export the clusters of this project (numeric id).")
	fs.StringVar(&logLevel, "log.level", "info", "Log level: debug, info, warn or error.")
	fs.StringVar(&logFormat, "log.format", "text", "Log format: text or json.")
	fs.DurationVar(&interval, "refresh-interval", snapshot.DefaultInterval, "How often the CubePath API is read (minimum 30s). Scrapes only read memory.")
	fs.BoolVar(&showVersion, "version", false, "Print the version and exit.")

	// Defaults from the environment, before the command line is parsed.
	var envErr error
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == "version" {
			return
		}
		v, ok := lookup(getenv, envName(f.Name))
		if !ok {
			return
		}
		if f.Name == "cluster" {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					clusters = append(clusters, part)
				}
			}
			return
		}
		if err := f.Value.Set(v); err != nil && envErr == nil {
			envErr = fmt.Errorf("%s: %w", envName(f.Name), err)
		}
	})
	if envErr != nil {
		return Config{}, envErr
	}
	// Repeated --cluster flags replace the environment list rather than extend it.
	fromEnv := len(clusters)
	envClusters := append(stringList(nil), clusters...)
	clusters = nil
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if showVersion {
		return Config{}, ErrVersion
	}
	if len(clusters) == 0 && fromEnv > 0 {
		clusters = envClusters
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg := Config{APIURL: apiURL, ListenAddress: listen, TelemetryPath: path, ProjectID: projectID, LogFormat: logFormat, RefreshInterval: interval}

	// The token file wins over the environment.
	switch {
	case tokenFile != "":
		b, err := os.ReadFile(tokenFile)
		if err != nil {
			return Config{}, fmt.Errorf("reading the token file: %w", err)
		}
		cfg.Token = cubepath.Secret(strings.TrimSpace(string(b)))
	default:
		cfg.Token = cubepath.Secret(strings.TrimSpace(getenv(TokenEnv)))
	}
	if cfg.Token == "" {
		return Config{}, errors.New("no API token: set " + TokenEnv + " or --api-token-file")
	}
	if strings.ContainsAny(cfg.Token.Reveal(), " \t\r\n") {
		return Config{}, errors.New("the API token must be a single line")
	}

	if _, err := cubepath.ValidateBaseURL(cfg.APIURL); err != nil {
		return Config{}, err
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/")
	if cfg.RefreshInterval < snapshot.MinInterval {
		return Config{}, fmt.Errorf("--refresh-interval must be at least %s", snapshot.MinInterval)
	}
	if err := validateTelemetryPath(cfg.TelemetryPath); err != nil {
		return Config{}, err
	}
	if cfg.ListenAddress == "" {
		return Config{}, errors.New("--web.listen-address must not be empty")
	}
	if cfg.ProjectID != "" && !projectIDRe.MatchString(cfg.ProjectID) {
		return Config{}, errors.New("--project-id must be numeric")
	}
	for _, c := range clusters {
		if !clusterRe.MatchString(c) {
			return Config{}, fmt.Errorf("invalid cluster uuid %q", c)
		}
		cfg.Clusters = append(cfg.Clusters, strings.ToLower(c))
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(logLevel)); err != nil {
		return Config{}, fmt.Errorf("--log.level: %w", err)
	}
	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return Config{}, errors.New("--log.format must be text or json")
	}
	return cfg, nil
}

var telemetryPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*[A-Za-z0-9._~-]$`)

// validateTelemetryPath refuses what the HTTP router would reject or what shadows a built-in route.
func validateTelemetryPath(p string) error {
	switch {
	case !strings.HasPrefix(p, "/") || p == "/":
		return errors.New("--web.telemetry-path must start with / and not be /")
	case p == "/healthz" || p == "/readyz":
		return fmt.Errorf("--web.telemetry-path must not be %s: the path is used by the health endpoints", p)
	case !telemetryPathRe.MatchString(p) || strings.Contains(p, "//"):
		return errors.New("--web.telemetry-path may only contain letters, digits and . _ ~ - /, without a trailing slash")
	}
	return nil
}

func lookup(getenv func(string) string, name string) (string, bool) {
	v := getenv(name)
	return v, v != ""
}

// NewLogger builds the logger of the configuration.
func (c Config) NewLogger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.LogLevel}
	if c.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
