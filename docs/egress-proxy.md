# Egress proxy

The egress proxy is the mechanism that turns `egress.mode: allowlist` from a YAML declaration into an actually enforced runtime constraint. Without it, allowlist-mode would be a lie — the sandbox would have unfiltered network access. With it, the sandbox can only reach hostnames the policy lets it reach.

This doc describes how that works end-to-end: the per-sandbox network topology, the proxy lifecycle, and the failure modes.

## What problem it solves

Sandboxes default to `network: none`. That's the safest setting and works for "run a Python script that doesn't need the internet." But many useful agent tasks *do* need the internet — call OpenAI, install packages from PyPI, fetch files from a known bucket. We want a way to say "yes you can reach the internet, but only these hosts."

Three options to enforce that:

1. **iptables on the sandbox** — fragile, requires CAP_NET_ADMIN, doesn't compose with `cap-drop=ALL`
2. **In-container DNS / hosts file rewriting** — easy to bypass, doesn't help once an IP is known
3. **An explicit forward proxy** — the sandbox can only reach a proxy we control; the proxy enforces the allowlist

We use option 3. It composes cleanly with hardened containers (the sandbox keeps `cap-drop=ALL`), enforces decisions in one place we can audit, and is well-understood by every HTTP client (`HTTPS_PROXY` / `HTTP_PROXY` env vars).

## Architecture

### Per-sandbox proxy and network

Every sandbox with `egress.mode: allowlist` gets its own proxy and its own Docker network. Both are managed by `kpd` and torn down when the sandbox is deleted.

```
                ┌─ Docker network: kp-net-<sandbox-id> ──────────────┐
                │                                                    │
                │   ┌─────────────────────┐    ┌──────────────────┐  │
                │   │  sandbox container  │    │  proxy container │  │
                │   │  HTTPS_PROXY=proxy  │ →  │  goproxy CONNECT │  │ → internet
                │   │  cap-drop=ALL       │    │  allowlist       │  │   (only allowed hosts)
                │   │  read-only rootfs   │    │                  │  │
                │   └─────────────────────┘    └──────────────────┘  │
                │                                                    │
                └────────────────────────────────────────────────────┘
                          (no other connectivity)
```

Why per-sandbox and not shared:

- **Allowlist is per-sandbox** — `python:3.12-slim` may allow `pypi.org` while `node:20-alpine` allows `registry.npmjs.org`. A single shared proxy would have to identify the caller via source IP and look up rules per sandbox, which adds attack surface and routing complexity.
- **Blast radius** — if the proxy goes weird (memory leak, hung connection), only that one sandbox is affected.
- **Lifecycle is identical** — proxy container and sandbox container are created together and removed together.

The cost: 2 containers per sandbox instead of 1. Acceptable.

### Why CONNECT-mode and not HTTP-rewrite

The proxy speaks HTTPS CONNECT only. The sandbox makes a normal HTTPS request to (say) `api.openai.com`; its HTTP client sees `HTTPS_PROXY=http://proxy:8080`, sends `CONNECT api.openai.com:443 HTTP/1.1`, and the proxy decides to allow or deny based on the CONNECT target. If allowed, the proxy opens a TCP connection to the target and pipes bytes both ways without decrypting them. The TLS handshake happens between the sandbox and OpenAI; the proxy never sees the request body or headers.

This means:

- The proxy can enforce **which hosts** the sandbox reaches (✅)
- The proxy **cannot** read or modify request bodies, headers, or response data (deliberate — see [security-model.md](./security-model.md) for why we don't MITM)

### What the proxy is

A small Go binary (`cmd/kpproxy`) using [`github.com/elazarl/goproxy`] in HTTPS-CONNECT mode. Configured via env vars at container start:

- `KP_ALLOWED_HOSTS` — comma-separated hostnames (resolved against the CONNECT target)
- `KP_DENY_HOSTS` — comma-separated hostnames blocked even if otherwise allowed (cloud metadata, link-local)

The binary is packaged as a small Docker image (multi-stage build, distroless or `alpine`-based). Operators build it locally and tag it as `kotakpasir/proxy:dev` (or set `KP_PROXY_IMAGE` to point elsewhere).

### Resolution flow at create time

```
agent: POST /v1/sandboxes { profile: "python-data" }
  │
  ▼
policy.Resolve()
  │   resolves to: image=python:3.12-slim, egress.mode=allowlist,
  │   egress.hosts=[api.openai.com, pypi.org, files.pythonhosted.org]
  │
  ▼
sandbox.Manager.Create()
  │
  ▼
runtime.docker.Create()
  ├─► docker network create kp-net-<sandbox-id>
  ├─► docker run --network kp-net-<sandbox-id>
  │              --network-alias proxy
  │              --label kotakpasir.sandbox-id=<id>
  │              --label kotakpasir.role=proxy
  │              -e KP_ALLOWED_HOSTS=api.openai.com,pypi.org,files.pythonhosted.org
  │              -e KP_DENY_HOSTS=169.254.169.254,metadata.google.internal,...
  │              kotakpasir/proxy:dev
  ├─► docker run --network kp-net-<sandbox-id>
  │              --label kotakpasir.sandbox-id=<id>
  │              --label kotakpasir.role=sandbox
  │              -e HTTPS_PROXY=http://proxy:8080
  │              -e HTTP_PROXY=http://proxy:8080
  │              -e NO_PROXY=
  │              python:3.12-slim
  │              tail -f /dev/null
  └─► persist {sandbox_id, container_id, proxy_container_id, network_id} in sqlite
```

Inside the sandbox, every Python `requests`, `httpx`, `aiohttp`, `urllib`, plus `curl`, `wget`, `pip` and `apt` honors `HTTPS_PROXY` / `HTTP_PROXY` automatically. Some libraries don't (raw sockets, custom HTTP clients) — those connections will simply fail because the sandbox has no other network egress.

### Teardown

`Manager.Delete` (and the reaper, on TTL expiry) removes everything in reverse:

```
1. docker rm -f <sandbox-container>
2. docker rm -f <proxy-container>
3. docker network rm kp-net-<sandbox-id>
```

If the sandbox crashes mid-create, the docker error path runs the same teardown to avoid leaks.

## Configuration

### YAML (per image or per profile)

```yaml
images:
  - name: python:3.12-slim
    egress:
      mode: allowlist
      hosts:
        - api.openai.com
        - pypi.org
        - files.pythonhosted.org

profiles:
  data-fetcher:
    image: alpine:latest
    egress:
      mode: allowlist
      hosts:
        - example.com

# Always blocked, even if listed in any allowlist
egress:
  global_deny:
    - 169.254.169.254
    - metadata.google.internal
    - metadata.aws.internal
```

### Env vars (operator-side)

| Env | Default | Purpose |
|---|---|---|
| `KP_PROXY_IMAGE` | `kotakpasir/proxy:dev` | Image used for the per-sandbox proxy. Build locally or push to your registry. |
| `KP_PROXY_PORT` | `8080` | Port the proxy listens on inside its container |

### Per-request (HTTP / MCP)

Callers can override resolved egress only by choosing a different profile or image (which carries its own egress rules). They cannot inject their own allowlist via API — that would defeat the policy. This is intentional.

## Failure modes

| Failure | Behavior |
|---|---|
| `KP_PROXY_IMAGE` not present in Docker | `Create` fails with "pull image: ..." (we don't auto-build it for you) |
| Proxy container fails to start | `Create` rolls back: removes any partially created network/sandbox; returns error |
| Sandbox tries to reach a non-allowlisted host | Proxy returns `403 Forbidden` on the CONNECT; the HTTP client sees a connection refused / 502 from upstream |
| Sandbox tries direct (non-proxy) traffic | Fails — the per-sandbox bridge has `internal: true`, no route to the host network |
| **Proxy crashes (panic, OOM, kernel kill)** | **Docker daemon restarts it automatically via `RestartPolicy=unless-stopped`.** Open connections drop; the sandbox transparently reconnects on the next call. |
| **Docker daemon restart / host reboot** | **Proxy comes back up with the daemon.** No action needed from kpd. |
| Cert-pinning library inside the sandbox | Works fine — CONNECT mode does not touch TLS, the sandbox terminates TLS with the real target |

## Proxy restart policy

The proxy container is started with `HostConfig.RestartPolicy.Name = "unless-stopped"`. Concretely:

| Event | Restarted? |
|---|---|
| Process panics / segfaults / exits non-zero | **Yes** — Docker daemon restarts it |
| Process OOM-killed (exceeds 64MB limit) | **Yes** |
| Docker daemon restart (`systemctl restart docker`, host reboot) | **Yes** — daemon brings it back on startup |
| `docker stop kp-proxy-X` (admin action) | No — explicit user stop |
| `docker kill kp-proxy-X` (admin action, any signal) | No — Docker treats this as user-initiated stop |
| `Manager.Delete(sandbox)` → `docker rm -f` | N/A — the container is removed entirely, nothing to restart |

This is deliberate: we restart on *failures*, not when an operator deliberately intervenes. If you want the proxy gone, deleting the sandbox is the right path; force-removing the proxy alone leaves the sandbox stranded with no egress path until the next restart.

The sandbox container itself has `RestartPolicy=no` — if a sandbox crashes, the agent should see the failure rather than have it papered over by an automatic restart that masks bugs.

## What this design does NOT do

These are deliberately out of scope; revisit only if a real use case justifies the cost:

- **Inspect or rewrite request bodies/headers** — would require MITM (see [security-model.md](./security-model.md))
- **Inject secrets at the TLS layer** — same
- **Rate-limit per-sandbox** — proxy could enforce QPS but doesn't yet
- **Capture audit logs of every request URL** — proxy logs CONNECT targets only (hostname:port). It does not see paths because TLS is opaque. If you want path-level auditing, you need MITM.
- **DNS allowlisting (block resolution of disallowed names)** — we filter at the CONNECT layer instead. DNS in the sandbox resolves normally; the connection to a disallowed host is what fails.

## Future work (in priority order)

1. **Proxy metrics** — Prometheus endpoint with per-host hit/deny counters
2. **Health check** — `HEALTHCHECK` directive in the proxy image so Docker tracks responsiveness, not just process liveness
3. **Pre-pulled cache for `KP_PROXY_IMAGE`** — `kpd` ensures the image exists at startup rather than on first create
4. **Per-sandbox proxy auth** — basic-auth credentials so a misconfigured client on the same network can't accidentally use someone else's proxy (low risk because the network is per-sandbox, but defense in depth)
