# Static binary on scratch: no shell, no package manager, no libc.
# The attack surface of the container is the binary itself and nothing else,
# which matters for something running on a stranger's machine.

# Cross-compile on the build host instead of emulating the Go toolchain
# under QEMU: multi-arch CI builds go from ~20 min to ~1 min.
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src
COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w" -o /out/probe-agent .

FROM scratch

# The agent talks HTTPS to RackList and to every target: without a CA bundle
# every measurement would fail certificate verification.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/probe-agent /probe-agent

# Nothing here needs root, and the image has no user database to look one up in.
USER 65534:65534

ENTRYPOINT ["/probe-agent"]
