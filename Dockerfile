# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags "-s -w \
      -X github.com/CubePathInc/cubepath-k8s-exporter/internal/version.Version=${VERSION} \
      -X github.com/CubePathInc/cubepath-k8s-exporter/internal/version.Revision=${COMMIT}" \
    -o /out/cubepath-k8s-exporter ./cmd/cubepath-k8s-exporter

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/cubepath-k8s-exporter /cubepath-k8s-exporter
USER nonroot
EXPOSE 9777
ENTRYPOINT ["/cubepath-k8s-exporter"]
