# Configuration

Every flag can also be set with an environment variable named `CUBEPATH_EXPORTER_<NAME>`, where
the flag name is upper-cased and `.` and `-` become `_`. A flag given on the command line wins
over its environment variable.

## Flags

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--api-token-file` | `CUBEPATH_EXPORTER_API_TOKEN_FILE` | none | Path to a file that holds the API token. The file is trimmed of surrounding whitespace. |
| `--api-url` | `CUBEPATH_EXPORTER_API_URL` | `https://api.cubepath.com` | Base URL of the CubePath API. `https` only, except `http` for `localhost` and `127.0.0.1`. No credentials, query or fragment. |
| `--web.listen-address` | `CUBEPATH_EXPORTER_WEB_LISTEN_ADDRESS` | `:9777` | Address the HTTP server listens on. |
| `--web.telemetry-path` | `CUBEPATH_EXPORTER_WEB_TELEMETRY_PATH` | `/metrics` | Path of the metrics endpoint. Must start with `/` and not be `/`. |
| `--cluster` | `CUBEPATH_EXPORTER_CLUSTER` | every readable cluster | UUID of a cluster to export. Repeat the flag for several clusters; in the environment variable use a comma separated list. Repeated flags replace the environment list. |
| `--project-id` | `CUBEPATH_EXPORTER_PROJECT_ID` | all projects | Only export the clusters of this project (numeric id). |
| `--refresh-interval` | `CUBEPATH_EXPORTER_REFRESH_INTERVAL` | `60s` | How often the API is read. Minimum `30s`. Scrapes only read memory. |
| `--log.level` | `CUBEPATH_EXPORTER_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `--log.format` | `CUBEPATH_EXPORTER_LOG_FORMAT` | `text` | `text` or `json`. Logs go to standard error. |
| `--version` | none | | Print the version and exit. |

## The API token

The token is read from, in this order:

1. The file given with `--api-token-file` (or `CUBEPATH_EXPORTER_API_TOKEN_FILE`).
2. The environment variable `CUBEPATH_API_TOKEN`.

The exporter refuses to start without a token, or with a token that spans more than one line.
There is deliberately no `--api-token` flag, so the token cannot leak through a process list. The
token is never printed or logged. Create it with the scopes `kubernetes:read` and `vps:read` only.

## Endpoints

| Path | Description |
|---|---|
| `/metrics` (or `--web.telemetry-path`) | Prometheus exposition. |
| `/healthz` | Always `200 ok` while the process runs. Use it as a liveness probe. |
| `/readyz` | `503` until the first successful refresh, then `200`. It stays `200` if later refreshes fail while a snapshot is still being served, so a short API outage does not restart the exporter. Use it as a readiness probe. |
| `/` | A small landing page with links. |

## Examples

Export two clusters, JSON logs, a token from a file:

```bash
cubepath-k8s-exporter \
  --api-token-file /run/secrets/cubepath-token \
  --cluster 11111111-2222-3333-4444-555555555555 \
  --cluster 66666666-7777-8888-9999-000000000000 \
  --log.format json
```

The same with environment variables:

```bash
export CUBEPATH_EXPORTER_API_TOKEN_FILE=/run/secrets/cubepath-token
export CUBEPATH_EXPORTER_CLUSTER=11111111-2222-3333-4444-555555555555,66666666-7777-8888-9999-000000000000
export CUBEPATH_EXPORTER_LOG_FORMAT=json
cubepath-k8s-exporter
```

## Behaviour that follows from the token

There are no switches to enable or disable a metric family. What the token can read decides what
is exported:

- Without `vps:read` the node VM family is absent.
- If the REST inventory cannot be read, the node pool family is absent and `cubepath_k8s_cluster_info`
  loses its status, location and high availability labels.
- Without `kubernetes:read` nothing can be read and only the exporter metrics are served.

`cubepath_k8s_exporter_family_available{family="clusters|pools|node_vm"}` tells you which families
the latest refresh could read.

## Limits

At most 100 clusters and 1000 nodes are exported per snapshot. When the limit cuts the list,
`cubepath_k8s_exporter_truncated` is 1, and the clusters kept are the first 100 by uuid. The
`--cluster` and `--project-id` filters are applied before the costly read, so they choose the
clusters; the API prices each cluster at 240 points and the exporter reads 15 per request. Label values that come from the API are cut to 128
characters.
