# Static binary on scratch: no shell, no package manager, no libc.
# The attack surface of the container is the binary itself and nothing else,
# which matters for something running on a stranger's machine.

FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod ./
COPY *.go ./

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/probe-agent .

FROM scratch

# The agent talks HTTPS to RackList and to every target: without a CA bundle
# every measurement would fail certificate verification.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/probe-agent /probe-agent

# Nothing here needs root, and the image has no user database to look one up in.
USER 65534:65534

ENTRYPOINT ["/probe-agent"]
