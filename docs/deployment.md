# Deployment

How to put kotakpasir on a real server. Aimed at a single-node Linux box
(VPS, bare metal, whatever) running Docker. Everything below assumes Ubuntu
24.04 or similar; adjust paths/units for your distro.

For multi-node / HA, see [`ROADMAP.md`](../ROADMAP.md) — that path isn't
ready yet.

## What you'll end up with

```
/opt/kotakpasir/
├── kpd                   # control plane binary
├── kp                    # CLI (optional but handy)
├── kotakpasir.yaml       # policy
└── .env                  # server / connection knobs

/var/lib/kotakpasir/
└── kotakpasir.db         # SQLite state (WAL files alongside)
```

`kpd` runs as a `systemd` unit. Docker daemon runs alongside. The egress
proxy image (`kotakpasir/proxy:dev`) lives in the local Docker registry,
built once during install.

## 1. Prerequisites

```bash
# Docker (https://docs.docker.com/engine/install/)
curl -fsSL https://get.docker.com | sh
sudo systemctl enable --now docker

# Go (only needed to build from source)
# https://go.dev/dl — Go 1.25 or later
```

Verify:

```bash
docker version
go version
```

A dedicated user keeps things tidy:

```bash
sudo useradd --system --home /var/lib/kotakpasir --shell /usr/sbin/nologin kpd
sudo usermod -aG docker kpd     # so kpd can talk to the Docker socket
sudo install -d -o kpd -g kpd /var/lib/kotakpasir
sudo install -d -o kpd -g kpd /opt/kotakpasir
```

## 2. Build artifacts

Clone and build on the server (cross-compile from your laptop also works).

```bash
git clone <this-repo> /tmp/kotakpasir
cd /tmp/kotakpasir

# Server binaries
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/kpd ./cmd/kpd
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/kp  ./cmd/kp

sudo install -o kpd -g kpd -m 0755 /tmp/kpd /opt/kotakpasir/kpd
sudo install -o kpd -g kpd -m 0755 /tmp/kp  /opt/kotakpasir/kp
sudo ln -sf /opt/kotakpasir/kp /usr/local/bin/kp

# Egress proxy image (built into local Docker, not pushed anywhere)
make proxy-image                  # tags kotakpasir/proxy:dev

# Pull every image referenced by your policy at least once.
docker pull alpine:latest
docker pull python:3.12-slim      # or whatever you allowlist
```

Cross-compile from a laptop:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags="-s -w" -o kpd-linux-amd64 ./cmd/kpd
scp kpd-linux-amd64 server:/opt/kotakpasir/kpd
```

## 3. Configuration

### Policy

Copy and edit:

```bash
sudo install -o kpd -g kpd -m 0644 \
  /tmp/kotakpasir/kotakpasir.yaml.example /opt/kotakpasir/kotakpasir.yaml
sudo -u kpd $EDITOR /opt/kotakpasir/kotakpasir.yaml
```

**In production set `KP_REQUIRE_POLICY=1`** so `kpd` refuses to start if the
file is missing — silent fallback to permissive defaults is the kind of
incident you don't want to debug at 3am.

### Server env

```bash
sudo -u kpd tee /opt/kotakpasir/.env >/dev/null <<'EOF'
KPD_ADDR=127.0.0.1:8080
KPD_TOKEN=<long random string, 32+ bytes>
KPD_LOG_LEVEL=info

KPD_DB=/var/lib/kotakpasir/kotakpasir.db
KP_POLICY_FILE=/opt/kotakpasir/kotakpasir.yaml
KP_REQUIRE_POLICY=1

KP_REAPER_INTERVAL=30s
KP_LOG_BUFFER_BYTES=262144
EOF
sudo chmod 0600 /opt/kotakpasir/.env
```

Generate a token:

```bash
openssl rand -base64 32
```

Bind to `127.0.0.1` and put a reverse proxy in front for TLS — see §6.

## 4. systemd unit

```bash
sudo tee /etc/systemd/system/kpd.service >/dev/null <<'EOF'
[Unit]
Description=kotakpasir control plane
After=network-online.target docker.service
Requires=docker.service

[Service]
Type=simple
User=kpd
Group=kpd
WorkingDirectory=/opt/kotakpasir
EnvironmentFile=/opt/kotakpasir/.env
ExecStart=/opt/kotakpasir/kpd
Restart=on-failure
RestartSec=2

# Hardening — kpd doesn't need much beyond the docker socket and its db.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/kotakpasir
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictNamespaces=true
RestrictRealtime=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now kpd
sudo systemctl status kpd
journalctl -u kpd -f
```

Sanity check:

```bash
curl -s http://127.0.0.1:8080/healthz | jq
# {"status":"ok","checks":{"store":{"ok":true},"runtime":{"ok":true},...}}
```

## 5. CLI access

`kp` reads the same env vars:

```bash
sudo tee /etc/profile.d/kp.sh >/dev/null <<'EOF'
export KPD_ADDR=http://127.0.0.1:8080
export KPD_TOKEN=<paste the token>
EOF
```

Or per-user via `~/.bashrc` / `~/.zshrc`. Then:

```bash
kp ls
ID=$(kp run -i alpine:latest)
kp exec "$ID" -- uname -a
kp rm "$ID"
```

Shell completion:

```bash
kp completion bash | sudo tee /etc/bash_completion.d/kp >/dev/null
# zsh: kp completion zsh > "${fpath[1]}/_kp"
```

## 6. TLS / reverse proxy

`kpd` does plain HTTP. Terminate TLS with Caddy / nginx / Traefik / your
favourite. Caddy is the lowest-friction option.

```caddyfile
kpd.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:8080

    # Optional: keep /healthz and /_metrics behind a different scope
    @internal path /_metrics /healthz
    handle @internal {
        @allowed remote_ip 10.0.0.0/8
        handle @allowed {
            reverse_proxy 127.0.0.1:8080
        }
        respond 403
    }
}
```

`/_metrics` and `/healthz` are unauthenticated by design — restrict them at
the reverse proxy if exposed.

For SSE endpoints (`/v1/sandboxes/:id/exec/stream`,
`/v1/sandboxes/:id/logs?follow=true`) make sure the proxy doesn't buffer
responses. Caddy handles this automatically; nginx needs
`proxy_buffering off` on those paths.

## 7. Prometheus

`kpd` exposes Prometheus metrics at `/_metrics`. Scrape config:

```yaml
scrape_configs:
  - job_name: kpd
    static_configs:
      - targets: ['kpd.internal:8080']
    metrics_path: /_metrics
```

Useful series to alert on:

- `kpd_sandbox_active` — gauge of running sandboxes; cap if you want.
- `rate(kpd_sandbox_created_total{outcome="runtime_error"}[5m])` — runtime
  trouble (Docker daemon, image pulls).
- `rate(kpd_sandbox_created_total{outcome="policy_denied"}[5m])` — agents
  hammering disallowed images; could be a misconfigured client.
- `kpd_pool_miss_total / kpd_pool_hit_total` — if the ratio creeps up,
  bump pool size or look at request mix.

The egress proxy exposes its own metrics — see [`egress-proxy.md`](./egress-proxy.md).

## 8. Health checks

Load-balancer probe:

```
GET /healthz
200 → status="ok"
503 → status="degraded" (body has per-subsystem detail)
```

The `503` is intentional: pulling a node out of rotation when its store or
Docker daemon is unreachable beats serving 500s.

## 9. Backups

Only one piece of state matters: `/var/lib/kotakpasir/kotakpasir.db`. SQLite
WAL mode means you also have `*.db-wal` and `*.db-shm` files alongside.

Simplest backup:

```bash
sqlite3 /var/lib/kotakpasir/kotakpasir.db ".backup '/backups/kpd-$(date +%F).db'"
```

Run that nightly via cron / systemd timer.

Loss of the DB = loss of sandbox tracking, not user data. Sandboxes
themselves are stateless beyond `/tmp` (read-only rootfs by default), so a
restore brings nothing back; it's only useful for forensics.

## 10. Updates

```bash
cd /tmp/kotakpasir && git pull
go build -trimpath -ldflags="-s -w" -o /tmp/kpd ./cmd/kpd
sudo install -o kpd -g kpd -m 0755 /tmp/kpd /opt/kotakpasir/kpd

# Rebuild proxy if Dockerfile.kpproxy or proxy code changed.
make proxy-image

sudo systemctl restart kpd
journalctl -u kpd -f
```

In-flight sandboxes survive a restart (they're recorded in the SQLite
store). The warm pool re-warms on startup.

## 11. Common pitfalls

- **`permission denied` on `/var/run/docker.sock`** — `kpd` user isn't in
  the `docker` group. `usermod -aG docker kpd && systemctl restart kpd`.
- **`pool warmup` blocks for minutes on first start** — pre-pull is doing
  its job; check `journalctl -u kpd` for `image pulled duration=…`. Set
  `KP_REQUIRE_POLICY=1` and a smaller `pool:` while you're testing.
- **`/healthz` returns `degraded` with `pool:<image> 0/N warm`** — the
  refill goroutine hasn't caught up. Usually transient; persistent
  failures point at registry or Docker daemon issues.
- **`KPD_TOKEN` set but clients still get 401** — make sure
  `Authorization: Bearer <token>` is being sent, not `Token <token>` or
  basic auth. `kp` and the SDK do this automatically.
- **SSE streams cut off** — reverse proxy is buffering. See §6.

## What's next

For day-to-day playing-around workflow on your laptop, see
[`getting-started.md`](./getting-started.md).

For threat model and trade-offs, see [`security-model.md`](./security-model.md).
