MODULE  := github.com/CubePathInc/cubepath-k8s-exporter
BINARY  := cubepath-k8s-exporter
VERSION ?= dev
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
IMAGE   ?= ghcr.io/cubepathinc/cubepath-k8s-exporter
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Revision=$(COMMIT)

.PHONY: build test lint docker run snapshot clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

test:
	go test -race ./...

lint:
	golangci-lint run

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t $(IMAGE):$(VERSION) .

# Needs CUBEPATH_API_TOKEN in the environment.
run: build
	./bin/$(BINARY)

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
