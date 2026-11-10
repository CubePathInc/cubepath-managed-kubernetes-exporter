# Troubleshooting

Start with the exporter's own metrics. They say whether the problem is the exporter, the API or
the cluster:

```promql
cubepath_k8s_exporter_last_refresh_success
cubepath_k8s_exporter_snapshot_age_seconds
cubepath_k8s_exporter_family_available
increase(cubepath_k8s_exporter_api_errors_total[15m])
increase(cubepath_k8s_exporter_refresh_errors_total[15m])
```

The logs (standard error) add the stage and the API error code of each failed refresh. They never
contain the token. Use `--log.level debug` for more detail.

## Symptom table

| Symptom | Likely cause | Where to look |
|---|---|---|
| `/readyz` stays `503`, no cluster metrics | The first refresh failed: bad token, no `kubernetes:read`, or the API is unreachable. | `api_errors_total{code}`, logs |
| `api_errors_total{code="UNAUTHENTICATED"}` grows | The API did not accept the token. | See below |
| `api_errors_total{code="FORBIDDEN"}` grows, `family_available{family="node_vm"} 0` | The token has no `vps:read` scope. | See below |
| `api_errors_total{code="HTTP_403"}` grows | The token IP allow-list rejects the exporter. | See below |
| `api_errors_total{code="RATE_LIMITED"}` grows | HTTP 429, too many requests for the token. | See below |
| A cluster has no health series and its nodes only `node_info` and VM series, `cluster_monitored 0` | The cluster was not polled in the last 10 minutes. | See below |
| `node_usage_*` missing, `cluster_metrics_server_installed 0` | metrics-server is not installed in the cluster. | See below |
| `snapshot_age_seconds` grows, `last_refresh_success 0` | Refreshes are failing and a stale snapshot is being served. | See below |
| `family_available{family="pools"} 0` | The REST inventory could not be read. | See below |
| `exporter_truncated 1` | More than 100 clusters or 1000 nodes. | Use `--cluster` or `--project-id`: both are applied before the clusters are read, so they choose which ones you get |

## UNAUTHENTICATED

The API answers HTTP 200 with an error whose code is `UNAUTHENTICATED` (or HTTP 401) when the
credential is wrong, expired or revoked. The exporter cannot read anything, so `/readyz` stays
`503` if this happens on the first refresh, and `last_refresh_success` is 0.

- Check that the token was copied completely and has no extra characters. A token file may end in
  a newline, which is fine; a token with spaces inside is rejected at startup.
- Check that you are using the token and not another credential, and that it was not revoked or
  rotated in the dashboard.
- If you pass the token through `--api-token-file`, remember that the file wins over
  `CUBEPATH_API_TOKEN`: an old file can hide a new variable.
- Check `--api-url` (it should be `https://api.cubepath.com`).

## FORBIDDEN on the servers (vps)

The token can read clusters but not servers: it lacks the `vps:read` scope. The exporter keeps
working, the node VM family (`cubepath_k8s_node_vm_*`) is absent and
`family_available{family="node_vm"}` is 0. Create a token with `kubernetes:read` and `vps:read`
(and nothing else). The same code on the cluster query means the token has no `kubernetes:read`.

A server that is suspended, being deleted or failed is also refused by the API, so its node
has no node VM series while the rest of the cluster is fine.

## HTTP 403 from the IP allow-list

A token with an IP allow-list is refused with HTTP 403 when the exporter calls from another
address (`api_errors_total{code="HTTP_403"}`). Add the public address the exporter reaches the API
from. The list takes single IP addresses. A private cluster, or a Deployment inside one, reaches
the Internet through the NAT gateway of its network, so allow the public address of that gateway.

## HTTP 429

The API allows 600 requests per minute per token. One exporter at the default interval uses a
tiny part of that, so a 429 usually means the same token is shared with other tools or with
several exporters. The exporter waits for the `Retry-After` time (at most 10 minutes) and
`api_requests_total{result="rate_limited"}` counts the events. Give each tool its own token, run
one exporter per token, and keep `--refresh-interval` at 60s or higher.

## health is null: the cluster is not monitored

The CubePath monitoring collector polls each cluster every 60 seconds. When the latest poll is
older than 10 minutes, the API returns no health for it and the exporter sets
`cubepath_k8s_cluster_monitored` to 0 and drops the cluster health series and the live state of
its nodes (Ready, conditions, allocatable, usage), keeping `node_info`, `node_last_seen` and the
node VM series. This
happens for a cluster that was just created, is being updated, or whose control plane is
unreachable. Wait a few minutes. If it persists, check the cluster in the CubePath dashboard and
contact support with the cluster UUID.

A cluster that is monitored but whose latest poll failed has `cluster_up 0` and
`cluster_poll_error{kind}` set (for example `tls` or `timeout`). In that case latency, node and pod
counts and components are left out because they are not measurements.

## Node usage is absent

`cubepath_k8s_node_usage_cpu_cores` and `cubepath_k8s_node_usage_memory_bytes` come from
metrics-server inside the cluster. When it is not installed,
`cubepath_k8s_cluster_metrics_server_installed` is 0 and the usage series are absent. Install
metrics-server from the add-on catalog. Usage is sampled every 5 minutes, so it appears after the
next sample. The node VM series do not need metrics-server.

## The snapshot is stale

When a refresh fails, the exporter keeps serving the previous snapshot for up to 10 minutes and
then only its own metrics. Signs: `last_refresh_success 0` and `snapshot_age_seconds` above 60.
After 10 minutes the cluster series disappear instead of showing old values. When only some
requests fail, the data they would have refreshed is kept while it is fresh and the failures are
counted in `refresh_errors_total` (`stage` `clusters`, `pools` or `node_vm`).

1. Find the cause with `api_errors_total` and `refresh_errors_total{stage}` (`clusters`, `pools` or
   `node_vm`) and the logs.
2. Network problems appear as `api_errors_total{code="NETWORK"}` and
   `api_requests_total{result="network_error"}`: check DNS and outbound HTTPS from where the
   exporter runs.
3. Repeated failures back off up to 5 minutes between attempts, so recovery can take a few minutes
   after the cause is fixed.

An alert on `cubepath_k8s_exporter_snapshot_age_seconds > 600` catches this.

## Node pools are missing

Pool metrics and the status, location and high availability labels come from the REST API. If it
fails, `family_available{family="pools"}` is 0 and `refresh_errors_total{stage="pools"}` grows. A
cached copy is served for up to 15 minutes. Check the same causes as above (scope, allow-list,
rate limit, network).

## A metric is missing

Series are absent, never zero, when the API has no value. The table of
[metrics.md](metrics.md) lists the rule for each one. Node VM series are also dropped when their
latest point is older than 5 minutes.
