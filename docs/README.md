# Documentation

| Document | What it covers | Contract |
|---|---|---|
| [metrics.md](metrics.md) | Every metric, its labels, its unit, the rule that makes it appear or disappear, and where the data comes from. | Yes. Metric names and labels are public and pinned by tests. |
| [configuration.md](configuration.md) | Flags, environment variables, token handling and HTTP endpoints. | Yes. |
| [troubleshooting.md](troubleshooting.md) | Symptoms mapped to causes and fixes, using the exporter's own metrics. | No. |

Other places in the repository:

- [../README.md](../README.md): overview, quick start, scrape configuration, example queries.
- [../CONTRIBUTING.md](../CONTRIBUTING.md): layout, conventions and the checks to run before a pull request.
- [../CHANGELOG.md](../CHANGELOG.md): what changed in each release.
- [../deploy/](../deploy/): Kubernetes manifests.
- [../dashboards/](../dashboards/): dashboards.
