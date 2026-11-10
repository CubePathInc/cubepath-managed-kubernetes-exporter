# Metrics

This document is the contract of the exporter: every metric it exposes, with its labels, its
unit and the rule that makes it appear or disappear. A test compares the metric names in the
tables below with the names the code registers, so the two cannot drift.

All cluster, node pool, node and node VM series carry the labels `cluster_uuid` and `cluster`.
Values are in base units (seconds, bytes, ratios). Samples carry no timestamps. Label values
that come from the API are cut to 128 characters.

## Data sources

The exporter reads the CubePath API with the token you give it and nothing else. It does not
connect to your cluster.

| Data | Endpoint | Scope |
|---|---|---|
| Cluster status, version, location, HA control plane | GraphQL `kubernetesCluster { status version location haControlPlane }` at `POST /graphql` | `kubernetes:read` |
| Node pools: plan, desired, min, max, autoscaling, current nodes | GraphQL `kubernetesCluster { nodePools }` | `kubernetes:read` |
| Cluster health, nodes (conditions, pool, allocatable, usage) | GraphQL `kubernetesCluster { health nodes }` | `kubernetes:read` |
| Server behind each node: CPU, memory, disk, network | GraphQL `vps(id:) { metrics networkBreakdown }` | `vps:read` |

Everything comes from GraphQL. The API needs GraphQL schema v1.7 or newer, the version that has
the status, location, HA and node pool fields. Against an older API the exporter logs an error
saying so and exports no cluster metrics.

How a refresh works (every 60 seconds by default):

1. One GraphQL query lists the clusters (uuid and name). `--cluster` and `--project-id` are
   applied here.
2. Status, node pools, health and nodes are read 15 clusters per request.
3. The servers behind the nodes are read 15 per request, with a one hour range. The exporter
   keeps the latest point of each series and drops it if it is older than 5 minutes.

The chunks of 15 come from the limits of the GraphQL API: 15 aliases and 24000 points per
request, where a cluster costs 252 points (health 120, nodes 120, node pools 12) and a server 1200. The API also allows 600 requests per
minute per token; a refresh of 3 clusters and 100 nodes is about 9 requests.

Details that explain what you see:

- A cluster that CubePath has not been able to check in the last 10 minutes has no health. The
  exporter reports `cubepath_managed_kubernetes_cluster_monitored 0` for it and leaves out the live state of its
  nodes rather than show old values.
- Node usage is empty without metrics-server.
- A server that is suspended, failed or being deleted has no `node_vm_*` series.
- The series of a server are `cpu_usage` (ratio), `memory_usage` (bytes) and disk and network in
  bytes per second. `nic="public"` and `nic="private"` come from the network breakdown;
  `nic="total"` is used when the breakdown has no data.
- Plan sizes are published by the API in MB and GB. The exporter converts them as MiB and GiB.
- If part of a refresh fails, what was read before for that part is kept while it is still fresh
  (clusters, with their status and node pools, 10 minutes; servers 5 minutes).

## Partial data

- A token without `vps:read` loses only the node VM family (`family_available{family="node_vm"}` is 0).
- A token without `kubernetes:read` cannot read anything: the refresh fails and only exporter
  metrics are served.
- If the API returns the node pools as null, the pool family is absent
  (`family_available{family="pools"}` is 0) and `refresh_errors_total{stage="pools"}` grows.
  `cubepath_managed_kubernetes_cluster_info` is still exported.
- An API older than schema v1.7 rejects the cluster query: the refresh fails and the exporter logs
  that the API does not support these fields.
- `METRICS_UNAVAILABLE` drops only the series it affects.
- A cluster not polled in the last 10 minutes has `cubepath_managed_kubernetes_cluster_monitored` 0 and no
  cluster health series. Its nodes keep `cubepath_managed_kubernetes_node_info` (without the kubelet version),
  `cubepath_managed_kubernetes_node_last_seen_timestamp_seconds` and the node VM series; the node Ready,
  condition, allocatable, usage and ready since series are absent, so `NodeNotReady` cannot fire
  from old state while `ClusterNotMonitored` already says the data is missing.
- Label values are de-duplicated after the 128 character cap: of two nodes, pools, components or
  readyz checks whose capped names are equal, only the first (in name order) is exposed.
- When one request of a refresh fails, the data it would have refreshed is kept while it is
  fresh: a cluster (with its node pools) for 10 minutes, a node VM sample for 5 minutes.
  Requests that were skipped (a rate limit or the refresh timeout) count as errors in
  `refresh_errors_total` like failed ones.
- `cubepath_managed_kubernetes_exporter_api_errors_total` starts with `code="UNAUTHENTICATED"` and
  `code="HTTP_403"` at 0, so `increase()` sees the first rejection.
- A failed poll (`cubepath_managed_kubernetes_cluster_up` 0) exposes no counters from that poll (latency, nodes,
  pods, components): they are not measurements.
- If a refresh fails, the previous snapshot is served for up to 10 minutes, then only exporter
  metrics are served. A stale value is never presented as fresh.

## Cluster metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `cubepath_managed_kubernetes_cluster_info` | gauge | `version`, `status`, `location`, `ha_control_plane` | Static facts, value 1. `version` is the configured cluster version. `location` is empty when the cluster has none. |
| `cubepath_managed_kubernetes_cluster_monitored` | gauge | | 1 when the cluster was polled in the last 10 minutes, else 0. |
| `cubepath_managed_kubernetes_cluster_up` | gauge | | 1 when the API server answered the latest poll. |
| `cubepath_managed_kubernetes_cluster_ready` | gauge | | 1 when every readyz check passed. |
| `cubepath_managed_kubernetes_cluster_readyz_check_failed` | gauge | `check` | 1 for each failed readyz check. |
| `cubepath_managed_kubernetes_cluster_apiserver_latency_seconds` | gauge | | Latency of the latest successful poll. |
| `cubepath_managed_kubernetes_cluster_last_poll_timestamp_seconds` | gauge | | Unix time of the latest poll. |
| `cubepath_managed_kubernetes_cluster_poll_error` | gauge | `kind` | 1 with the error kind (`dns`, `tcp`, `tls`, `unauthorized`, `readyz`, `timeout`, `http`, `decode`) when the latest poll failed. |
| `cubepath_managed_kubernetes_cluster_nodes` | gauge | `state` | Nodes by `total`, `ready` or `cordoned`. |
| `cubepath_managed_kubernetes_cluster_pods_pending` | gauge | | Pods in Pending phase. |
| `cubepath_managed_kubernetes_cluster_pods_failed` | gauge | | Pods in Failed phase. |
| `cubepath_managed_kubernetes_cluster_component_ready` | gauge | `component` | Ready replicas of a platform component (`cilium`, `api_proxy`, `konnectivity`, `coredns`, `autoscaler`). |
| `cubepath_managed_kubernetes_cluster_component_desired` | gauge | `component` | Desired replicas of the same components. |
| `cubepath_managed_kubernetes_cluster_metrics_server_installed` | gauge | | 1 when metrics-server is installed (node usage needs it). |
| `cubepath_managed_kubernetes_cluster_client_cert_expiry_timestamp_seconds` | gauge | | Unix time at which the monitoring client certificate expires. Absent until known. |
| `cubepath_managed_kubernetes_cluster_server_cert_expiry_timestamp_seconds` | gauge | | Unix time at which the API server certificate expires. Absent until known. |

## Node pool metrics

Present while the API returns the node pools. Extra label: `pool`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `cubepath_managed_kubernetes_node_pool_info` | gauge | `plan` | Static facts, value 1. |
| `cubepath_managed_kubernetes_node_pool_nodes` | gauge | `state` | Nodes by `desired`, `min`, `max` or `current` (members not removed). |
| `cubepath_managed_kubernetes_node_pool_autoscale_enabled` | gauge | | 1 when autoscaling is on. |
| `cubepath_managed_kubernetes_node_pool_plan_cpu_cores` | gauge | | Nominal vCPUs of the plan. |
| `cubepath_managed_kubernetes_node_pool_plan_memory_bytes` | gauge | | Nominal memory of the plan. |
| `cubepath_managed_kubernetes_node_pool_plan_disk_bytes` | gauge | | Nominal disk of the plan. |

## Node metrics

Extra label: `node`. Series from fields the collector has not seen are absent.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `cubepath_managed_kubernetes_node_info` | gauge | `pool`, `kubelet_version`, `vps_id` | Static facts, value 1. `pool` is empty when the API does not return it. |
| `cubepath_managed_kubernetes_node_ready` | gauge | | 1 when the Ready condition is true. |
| `cubepath_managed_kubernetes_node_unschedulable` | gauge | | 1 when the node is cordoned. |
| `cubepath_managed_kubernetes_node_condition` | gauge | `condition` | 1 when `memory_pressure`, `disk_pressure` or `pid_pressure` is true. |
| `cubepath_managed_kubernetes_node_ready_since_timestamp_seconds` | gauge | | Unix time of the last Ready transition. |
| `cubepath_managed_kubernetes_node_last_seen_timestamp_seconds` | gauge | | Unix time of the latest poll that saw the node. |
| `cubepath_managed_kubernetes_node_allocatable_cpu_cores` | gauge | | CPU the kubelet offers to pods. |
| `cubepath_managed_kubernetes_node_allocatable_memory_bytes` | gauge | | Memory the kubelet offers to pods. |
| `cubepath_managed_kubernetes_node_allocatable_pods` | gauge | | Pods the kubelet accepts. |
| `cubepath_managed_kubernetes_node_usage_cpu_cores` | gauge | | CPU used, from metrics-server (sampled every 5 minutes). |
| `cubepath_managed_kubernetes_node_usage_memory_bytes` | gauge | | Memory used, from metrics-server (sampled every 5 minutes). |

## Node VM metrics

The server behind the node, measured from outside the guest by the CubePath monitoring
collector. Needs the `vps:read` scope. Each series is the latest point of the last hour and is
dropped when it is older than 5 minutes. Extra label: `node`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `cubepath_managed_kubernetes_node_vm_cpu_usage_ratio` | gauge | | CPU usage of the server, 0 to 1 of its vCPUs. |
| `cubepath_managed_kubernetes_node_vm_memory_usage_bytes` | gauge | | Memory in use on the server. |
| `cubepath_managed_kubernetes_node_vm_disk_read_bytes_per_second` | gauge | | Disk read rate. |
| `cubepath_managed_kubernetes_node_vm_disk_write_bytes_per_second` | gauge | | Disk write rate. |
| `cubepath_managed_kubernetes_node_vm_network_receive_bytes_per_second` | gauge | `nic` | Receive rate per NIC (`public`, `private`); `total` (sum of the NICs) when the per-NIC data has no fresh point. |
| `cubepath_managed_kubernetes_node_vm_network_transmit_bytes_per_second` | gauge | `nic` | Transmit rate, same labels. |
| `cubepath_managed_kubernetes_node_vm_sample_timestamp_seconds` | gauge | | Unix time of the newest sample among the node VM series. |

## Exporter metrics

| Metric | Type | Labels | Description |
|---|---|---|---|
| `cubepath_managed_kubernetes_exporter_build_info` | gauge | `version`, `revision` | Build information, value 1. |
| `cubepath_managed_kubernetes_exporter_last_refresh_timestamp_seconds` | gauge | | Unix time of the latest refresh attempt. |
| `cubepath_managed_kubernetes_exporter_last_refresh_success` | gauge | | 1 when the latest refresh succeeded. |
| `cubepath_managed_kubernetes_exporter_refresh_duration_seconds` | gauge | | Duration of the latest refresh. |
| `cubepath_managed_kubernetes_exporter_refresh_errors_total` | counter | `stage` | Failures by stage: `clusters`, `pools`, `node_vm`. |
| `cubepath_managed_kubernetes_exporter_api_requests_total` | counter | `api`, `result` | Requests to the API: `api` is always `graphql`, `result` is `ok`, `http_error`, `rate_limited`, `network_error` or `decode_error`. |
| `cubepath_managed_kubernetes_exporter_api_errors_total` | counter | `code` | API errors by code (`UNAUTHENTICATED`, `FORBIDDEN`, `NOT_FOUND`, `BAD_USER_INPUT`, `METRICS_UNAVAILABLE`, `QUERY_TOO_COMPLEX`, `INTERNAL_ERROR`, `RATE_LIMITED`, `HTTP_403`, `HTTP_404`, `HTTP_400`, `HTTP_5XX`, `HTTP_OTHER`, `NETWORK`, `UNKNOWN`). |
| `cubepath_managed_kubernetes_exporter_family_available` | gauge | `family` | 1 when the latest refresh could read `clusters`, `pools` or `node_vm`. |
| `cubepath_managed_kubernetes_exporter_snapshot_age_seconds` | gauge | | Age of the snapshot, present once there is one. |
| `cubepath_managed_kubernetes_exporter_clusters` | gauge | | Clusters in the served snapshot. |
| `cubepath_managed_kubernetes_exporter_nodes` | gauge | | Nodes in the served snapshot. |
| `cubepath_managed_kubernetes_exporter_truncated` | gauge | | 1 when the snapshot was cut to the limits (100 clusters, 1000 nodes). |

The standard Go runtime and process metrics are exposed as well.
