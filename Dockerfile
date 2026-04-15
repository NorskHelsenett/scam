# syntax=docker/dockerfile:1
# Build a tiny static binary on scratch. GOGC=50 inside the runtime keeps
# the live heap small in exchange for a little extra CPU on GC.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download
COPY main.go ./
ARG TARGETOS TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/spam-operator .

FROM scratch
ENV GOGC=50 GOMEMLIMIT=96MiB
# CA bundle for TLS to kube-apiservers reached via out-of-cluster kubeconfig.
# In-cluster auth uses the mounted serviceaccount ca.crt and doesn't need this,
# but it keeps the image usable for local/debug runs too.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/spam-operator /spam-operator
USER 65532:65532
ENTRYPOINT ["/spam-operator"]
