# AGENTS.md

Notes for anyone (or anything) changing this repository.

## What this is

A Prometheus exporter for CubePath Managed Kubernetes, in Go. A background loop reads the CubePath
customer API once a minute, keeps a snapshot in memory, and `/metrics` is served from that
snapshot. All data comes from GraphQL (`POST /graphql`, schema v1.7 or newer).

## Layout

| Path | What lives there |
|---|---|
| `cmd/cubepath-managed-kubernetes-exporter` | Entry point |
| `internal/config` | Flags and environment variables |
| `internal/cubepath` | API client: GraphQL documents, request chunking, error codes |
| `internal/snapshot` | Refresh loop and the in-memory snapshot |
| `internal/collector` | Snapshot to Prometheus metrics |
| `internal/server` | `/metrics`, `/healthz`, `/readyz` |
| `internal/fakeapi` | Fake API used by the tests. It enforces the same request limits as the real one |
| `testdata/` | Fixtures and the golden exposition |
| `docs/` | `metrics.md` is the metric contract |
| `dashboards/`, `rules/` | Grafana dashboard and alerting rules |
| `deploy/` | Kubernetes manifests and a docker-compose file |

## Commands

```bash
make build
make test          # go test -race ./...
gofmt -l .         # must print nothing
go vet ./...
go test ./internal/collector -update   # regenerate the golden file, then read the diff
promtool test rules rules/cubepath-kubernetes.rules.test.yaml
```

## Rules

- Metric names and labels are public. Renaming or removing one breaks dashboards and alerts that
  people already have. A change there needs `docs/metrics.md`, the golden file, the dashboard, the
  rules and a changelog entry, all in the same change. Tests fail if the doc, the dashboard or the
  rules mention a metric the code does not expose.
- A value the API does not have is a missing series, never a 0.
- The API returns gauges and per second rates. Do not turn them into counters.
- No switches to enable or disable features. What gets exported depends on what the token can
  read, and `cubepath_managed_kubernetes_exporter_family_available` reports it.
- The token must never reach a log line, an error message, a label or a fixture. It is wrapped in
  `cubepath.Secret` for that reason. There is no `--api-token` flag on purpose.
- Tests talk to `internal/fakeapi` only. Nothing in this repository calls the real API on its own.
- The GraphQL API limits a request to depth 6, 15 aliases, 2000 tokens and 24000 points. Each
  cluster costs 252 points (health 120, nodes 120, nodePools 12) and each server 1200. If you touch a GraphQL document, redo the sums in
  `internal/cubepath/chunk.go` and make the fake API agree.
- Text in the repository is in English.

## Git

Small commits with a plain subject line that says what changed. No generated footers or
co-author trailers.
