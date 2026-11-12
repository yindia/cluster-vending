# One image for the single binary: `cluster-vending serve` (the API server)
# and `cluster-vending controller` (the controller manager) are subcommands
# of it; the manifests under deploy/server and controller/config choose.
#
#   make docker-build IMAGE=registry.example.com/cluster-vending TAG=dev
#
# Nothing is baked in but the binary: no config file, no credentials.

# The Go version of go.mod.
ARG GO_VERSION=1.27

# Built on the build machine's architecture, cross-compiled to the target's:
# the binary is pure Go (CGO_ENABLED=0, as `make build`).
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
# The same values `make build` sets (pkg/version).
ARG VERSION=dev
ARG COMMIT=unknown
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
      -ldflags "-s -w \
        -X github.com/yindia/cluster-vending/pkg/version.Version=${VERSION} \
        -X github.com/yindia/cluster-vending/pkg/version.Commit=${COMMIT}" \
      -o /out/cluster-vending .

# No shell, no package manager; CA certificates for the server's AWS calls.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cluster-vending /cluster-vending
USER 65532:65532
ENTRYPOINT ["/cluster-vending"]
