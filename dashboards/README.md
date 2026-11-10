# Grafana dashboard

`cubepath-kubernetes.json` is a single dashboard for the metrics of cubepath-k8s-exporter. It
works with Grafana 10 and newer and with any Prometheus-compatible data source (Prometheus,
Mimir, VictoriaMetrics, Thanos).

## Import

1. Make sure Prometheus scrapes the exporter (see `../examples/prometheus.yml`). A scrape
   interval of 60 seconds is enough: the data only changes once a minute.
2. In Grafana open **Dashboards**, then **New**, then **Import**.
3. Upload `cubepath-kubernetes.json` (or paste its content) and press **Load**.
4. Pick your Prometheus data source when asked and press **Import**.

To provision it from a file instead, copy the JSON to a folder that a Grafana dashboard provider
reads, for example:

```yaml
apiVersion: 1
providers:
  - name: cubepath
    type: file
    options:
      path: /var/lib/grafana/dashboards/cubepath
```

## Variables

| Variable | Meaning |
|---|---|
| `datasource` | The Prometheus data source to query. |
| `cluster` | One or more clusters. The list shows the cluster name and filters by `cluster_uuid`, so two clusters with the same name stay apart. Defaults to all. |
| `node` | One or more nodes of the selected clusters (label `node`). Defaults to all. |

The `cluster` variable filters every panel except the Exporter health row, which describes the
exporter itself. The `node` variable filters the Nodes and Node VM rows.

## Rows

- **Overview**: clusters monitored, up and ready, nodes ready, API server latency, days left on
  the certificates and the cluster inventory table.
- **Control plane and platform components**: readyz checks, poll errors, pending and failed
  pods, ready against desired replicas of the platform components, age of the latest poll.
- **Node pools**: current, desired, minimum and maximum nodes per pool, pools below their
  desired size, plan and autoscaling table.
- **Nodes**: Ready, pressure conditions and cordoned state, allocatable against usage for CPU and
  memory, and a node table.
- **Node VM**: CPU, memory, disk I/O and public and private network traffic of the server behind
  each node, measured from outside the machine.
- **Exporter health**: last refresh result, snapshot age, which data families the token can read,
  refresh and API errors.

## Empty panels

A panel is empty when the data does not exist, never because it is zero:

- Node usage panels need metrics-server in the cluster.
- Node pool panels need the CubePath inventory (`family_available{family="pools"}`).
- Node VM panels need a token with the `vps:read` scope (`family_available{family="node_vm"}`).
- Cluster panels are empty for a cluster that was not polled in the last 10 minutes
  (`cubepath_k8s_cluster_monitored` is 0).

The pool table joins its queries on the pool name, so two selected clusters with a pool of the
same name share a row; select one cluster to read it unambiguously.

## Alerting rules

Matching Prometheus alerting rules live in `../rules/`.
