# Contributing

Bug reports and pull requests are welcome.

## Reporting a problem

Open an issue with the exporter version (`cubepath-managed-kubernetes-exporter --version`), how you run it, and
the output of:

```bash
curl -s localhost:9777/metrics | grep cubepath_managed_kubernetes_exporter_
```

Remove your token from anything you paste. The exporter never prints it, but a shell history or
a manifest might.

## Changing the code

You need Go 1.26 or newer.

```bash
make build
make test
gofmt -l .     # must print nothing
go vet ./...
```

The tests run against a fake of the CubePath API that lives in `internal/fakeapi`, so you do not
need an account to work on the exporter.

Before you open a pull request:

- If you add, rename or remove a metric, update `docs/metrics.md`, the dashboard and the rules in
  the same change. The tests will tell you if they disagree.
- If the output of `/metrics` changes, regenerate the golden file with
  `go test ./internal/collector -update` and check the diff is what you meant.
- Add a line to `CHANGELOG.md` for anything a user would notice.

[AGENTS.md](AGENTS.md) has the layout of the repository and the rules the code follows.
