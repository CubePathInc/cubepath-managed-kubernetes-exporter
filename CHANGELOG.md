# Changelog

## 0.1.0 (2026-11-10)

First version.

- Cluster and control plane: reachability, readiness, failed readyz checks, API server latency,
  node counts, pending and failed pods, platform components, certificate expiry.
- Node pools: plan, desired, minimum, maximum and current nodes, autoscaling.
- Nodes: Ready, cordoned, pressure conditions, allocatable resources, usage when metrics-server
  is installed.
- Server behind each node: CPU, memory, disk and network (public and private).
- Exporter health: refresh result and duration, API requests and errors, snapshot age.
- Grafana dashboard and Prometheus alerting rules.
- Docker image, Kubernetes manifests and a docker-compose example.
