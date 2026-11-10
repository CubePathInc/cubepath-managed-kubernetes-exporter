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
      -X github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/version.Version=${VERSION} \
      -X github.com/CubePathInc/cubepath-managed-kubernetes-exporter/internal/version.Revision=${COMMIT}" \
    -o /out/cubepath-managed-kubernetes-exporter ./cmd/cubepath-managed-kubernetes-exporter

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/cubepath-managed-kubernetes-exporter /cubepath-managed-kubernetes-exporter
USER nonroot
EXPOSE 9777
ENTRYPOINT ["/cubepath-managed-kubernetes-exporter"]
