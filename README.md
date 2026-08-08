# RackList probe agent

The community probe of the [RackList](https://racklist.eu) measurement network.

It asks RackList what to measure, measures it from your machine, and reports
back. **It never picks its own targets**: the pool is drawn server-side and
rotates over time. That is the whole point. A probe that could choose what it
measures could choose to measure something it has an interest in, and the
network is built precisely so that the party measuring is never the party with
an interest in the result.

The source is published so you can read exactly what runs on your machine
before you run it.

## What it does

Every few minutes, for each target the server assigned:

| Measured | How |
|---|---|
| Reachability | HTTP GET, redirects and auth walls count as up, 5xx counts as down |
| Response time | Full round trip of the request |
| DNS resolution | Time spent resolving the name |
| TTFB | Time to the first response byte |
| Certificate expiry | Days left on the leaf certificate (HTTPS targets) |

It sends nothing else. No page content, no headers, no data about your machine
beyond the IP it connects from, which RackList reads to determine your probe's
network operator and country. That reading happens server-side and is never
declared by the agent.

## Install

You need a probe token first: enrol a probe from your RackList account
(**Account → My probes**). The token is shown once, at creation.

### Without Docker (binary + systemd)

```bash
curl -fsSL https://github.com/RackList/probe-agent/releases/latest/download/install.sh -o install.sh
sudo PROBE_TOKEN=pb_xxxxx PROBE_API=https://racklist.eu/api/v1/probe sh install.sh
```

The script installs a static binary in `/usr/local/bin`, creates a dedicated
unprivileged system user, writes the token to `/etc/racklist-probe/agent.env`
(mode `0640`), and enables a hardened systemd unit. Re-run it to upgrade: your
configuration is left untouched.

```bash
systemctl status racklist-probe
journalctl -u racklist-probe -f
```

### With Docker

```bash
docker run -d --name racklist-probe \
  --restart unless-stopped \
  -e PROBE_TOKEN=pb_xxxxx \
  -e PROBE_API=https://racklist.eu/api/v1/probe \
  racklist/probe-agent:latest
```

The image is built `FROM scratch`: a static binary, a CA bundle, and nothing
else. No shell, no package manager, runs as UID 65534.

### From source

```bash
git clone https://github.com/RackList/probe-agent
cd probe-agent
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o racklist-probe-agent .
```

## Configuration

| Env var | Required | Default | Description |
|---|---|---|---|
| `PROBE_TOKEN` | yes | - | Your `pb_` token, shown once at enrolment |
| `PROBE_API` | yes | - | Base URL of the probe API, e.g. `https://racklist.eu/api/v1/probe` |
| `PROBE_TIMEOUT` | no | `10s` | Per-target timeout (`30s`, `5m`, or a bare integer read as seconds) |
| `PROBE_SUBMIT_TIMEOUT` | no | `15s` | Timeout on calls to RackList |
| `PROBE_INSECURE` | no | `false` | Skip TLS verification. Development instances only |

There is deliberately no setting for *what* to measure or *how often*: both come
from the server. The cadence is a network-wide parameter, and a fleet that paced
itself could never be retuned.

## Behaviour

- On start-up the agent fetches its pool. Without a pool there is nothing to
  measure, so it retries until the server answers.
- It measures a round immediately, then on the interval the server gave it.
- It re-reads its pool periodically: the imposed pool rotates, and an agent that
  never refreshed would drift onto pairs the server has already released.
- An unreachable target still produces a measurement. "Unreachable from here" is
  a result; dropping it would silently turn an outage into missing data.
- Failed submissions are retried with exponential backoff. A rejected token or a
  rejected payload is **not** retried: it would fail identically and only burn
  your rate limit.
- `SIGINT` / `SIGTERM` stop the agent cleanly.

## Security

- The token is a bearer secret. The agent refuses to start against a plain HTTP
  endpoint unless you explicitly set `PROBE_INSECURE`.
- The systemd unit runs unprivileged with an empty capability bounding set,
  `ProtectSystem=strict`, a syscall filter, and a memory cap. The agent needs
  outbound network access and nothing more.
- Nothing is written to disk. Logs go to stdout (the journal under systemd).
- If your token leaks, rotate it from your account: the old one stops working
  immediately.

## Development

```bash
go test ./...
go vet ./...

docker build -t racklist/probe-agent:dev .

docker run --rm \
  -e PROBE_TOKEN=pb_xxxxx \
  -e PROBE_API=https://racklist.dev.localhost/api/v1/probe \
  -e PROBE_INSECURE=true \
  racklist/probe-agent:dev
```

`PROBE_INSECURE` is required against a `*.dev.localhost` instance: the local
development CA is not in the scratch image. Never use it in production.
