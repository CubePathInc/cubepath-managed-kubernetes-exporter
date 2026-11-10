# cubepath-managed-kubernetes-exporter

Prometheus exporter for [CubePath](https://cubepath.com) Managed Kubernetes.

Nodes of a managed cluster have no SSH access, so the usual "log in and look" is not an option.
This exporter reads what the CubePath API knows about your clusters and serves it on `/metrics`:
control plane health, node pools, node state, and CPU, memory, disk and network of the server
behind each node. It needs nothing installed in the cluster. Run it anywhere that can reach
`api.cubepath.com`.

## Quick start

Create an API token in the CubePath dashboard with the scopes `kubernetes:read` and `vps:read`,
then:

```bash
docker run -d --name cubepath-managed-kubernetes-exporter -p 127.0.0.1:9777:9777 \
  -e CUBEPATH_API_TOKEN="$CUBEPATH_API_TOKEN" \
  ghcr.io/cubepathinc/cubepath-managed-kubernetes-exporter:latest

curl -s localhost:9777/metrics | grep cubepath_managed_kubernetes_
```

Or with the binary from the [releases page](https://github.com/CubePathInc/cubepath-managed-kubernetes-exporter/releases):

```bash
export CUBEPATH_API_TOKEN=...
./cubepath-managed-kubernetes-exporter
```

Prometheus:

```yaml
scrape_configs:
  - job_name: cubepath-managed-kubernetes
    scrape_interval: 60s
    static_configs:
      - targets: ["localhost:9777"]
```

Other ways to run it:

- [deploy/kubernetes/](deploy/kubernetes/): Deployment, Service and an optional ServiceMonitor.
  The token lives in a Secret and is mounted as a file.
- [deploy/docker-compose.yaml](deploy/docker-compose.yaml): the exporter plus a Prometheus that
  already scrapes it.

## Metrics

Everything is prefixed with `cubepath_managed_kubernetes_` and labelled with `cluster_uuid` and `cluster`.
The complete list is in [docs/metrics.md](docs/metrics.md).

| Family | Examples |
|---|---|
| Cluster and control plane | `cluster_up`, `cluster_ready`, `cluster_apiserver_latency_seconds`, `cluster_pods_pending`, `cluster_component_ready`, `cluster_server_cert_expiry_timestamp_seconds` |
| Node pools | `node_pool_nodes{state="desired\|min\|max\|current"}`, `node_pool_autoscale_enabled`, `node_pool_plan_cpu_cores` |
| Nodes | `node_ready`, `node_condition`, `node_unschedulable`, `node_allocatable_memory_bytes`, `node_usage_cpu_cores` |
| Server behind each node | `node_vm_cpu_usage_ratio`, `node_vm_memory_usage_bytes`, `node_vm_disk_write_bytes_per_second`, `node_vm_network_receive_bytes_per_second{nic}` |
| The exporter itself | `exporter_last_refresh_success`, `exporter_family_available`, `exporter_api_errors_total` |

Things worth knowing before you build dashboards on it:

- **Data is one minute old at best.** CubePath checks each cluster every 60 seconds, so the
  exporter refreshes every 60 seconds too and scrapes are served from memory. Scraping faster
  gains nothing.
- **Node usage needs metrics-server.** `node_usage_*` only exists if metrics-server is installed
  in the cluster, and it is sampled every 5 minutes. The `node_vm_*` series always exist and have
  a 30 second step.
- **Two memory numbers, both correct.** `node_usage_memory_bytes` is what the kubelet reports.
  `node_vm_memory_usage_bytes` is measured from outside the node and includes page cache and
  whatever else the operating system holds, so it is higher.
- **Rates, not counters.** Disk and network series are already per second. Use `avg_over_time`,
  not `rate`.
- **No value means no series.** If the API has nothing for a node, the series is missing. It is
  never reported as 0.

## What is not here

Pods, containers, volumes, filesystem usage, load average: anything that has to be read from
inside the node. For that, install kube-prometheus-stack from the add-on catalog
(`cubecli kubernetes addon install`). The two go well together: the stack sees your workloads,
this exporter sees the control plane, the pools and the servers underneath, and keeps reporting
when the cluster itself is in trouble.

## Token

| Scope | Used for |
|---|---|
| `kubernetes:read` | Clusters, nodes and node pools. Required. |
| `vps:read` | The `node_vm_*` series. Without it the rest still works. |

The exporter only reads, so do not give the token any write scope. The token is taken from
`CUBEPATH_API_TOKEN` or from a file (`--api-token-file`); there is no flag for the token itself,
so it never shows up in `ps`. If the token has an IP allow-list, add the address the exporter
calls from. For an exporter running inside a private cluster that is the NAT gateway.

`/metrics` has no authentication. Keep it on a private network.

## Configuration

The defaults are fine for most setups. The flags you are most likely to need:

| Flag | Default | |
|---|---|---|
| `--api-token-file` | | Read the token from a file instead of `CUBEPATH_API_TOKEN` |
| `--web.listen-address` | `:9777` | |
| `--cluster` | all | Only export this cluster uuid (repeatable) |
| `--project-id` | all | Only export the clusters of one project |
| `--refresh-interval` | `60s` | Minimum `30s` |

Every flag also has a `CUBEPATH_EXPORTER_*` environment variable. See
[docs/configuration.md](docs/configuration.md).

## Dashboard and alerts

- [dashboards/cubepath-kubernetes.json](dashboards/cubepath-kubernetes.json): Grafana dashboard
  (Grafana 10 or newer). Import it and pick your Prometheus data source.
- [rules/cubepath-kubernetes.rules.yaml](rules/cubepath-kubernetes.rules.yaml): alerting rules for
  the control plane, nodes, pools, certificates and the exporter.

A few queries to start with:

```promql
# control plane unreachable
cubepath_managed_kubernetes_cluster_up == 0

# nodes not Ready
cubepath_managed_kubernetes_node_ready == 0

# pool smaller than it should be
cubepath_managed_kubernetes_node_pool_nodes{state="current"}
  < ignoring(state) cubepath_managed_kubernetes_node_pool_nodes{state="desired"}

# days until the API server certificate expires
(cubepath_managed_kubernetes_cluster_server_cert_expiry_timestamp_seconds - time()) / 86400
```

## When something looks wrong

Check `cubepath_managed_kubernetes_exporter_last_refresh_success` and
`cubepath_managed_kubernetes_exporter_api_errors_total` first. [docs/troubleshooting.md](docs/troubleshooting.md)
maps the usual symptoms (rejected token, missing scope, IP allow-list, rate limit) to their cause.

## Building

Go 1.26 or newer.

```bash
git clone https://github.com/CubePathInc/cubepath-managed-kubernetes-exporter.git
cd cubepath-managed-kubernetes-exporter
make build
make test
```

Tests run against a local fake of the API and never touch the real one.

## License

[MIT](LICENSE)
