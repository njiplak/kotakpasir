# kotakpasir

Self-hosted sandboxes for AI agents. Spin up disposable Linux environments
where an LLM can write files, run shell commands, and reach a curated set of
hosts — all bounded by a YAML policy you wrote, on a $5 VPS you control.

`kotakpasir` is Indonesian for *sandbox*.

## Why

LLM agents need somewhere to actually run code. The hosted options work well
but cost real money per tenant; running them yourself usually means
microVMs that demand KVM/virtualization a small VPS doesn't have.

This project takes the pragmatic path: hardened Docker containers (cap-drop,
read-only rootfs, no-new-privs, pids/cpu/memory limits, `network=none` by
default) wrapped in a control plane with the surfaces an agent actually
needs — HTTP API, MCP, CLI, Go SDK. Containers aren't a security boundary
for fully untrusted code; gVisor and Firecracker backends are on the
[roadmap](./ROADMAP.md). For everything in between, this is enough.

## Status

Pre-1.0. The core is in daily use locally; everything in [`CHANGELOG.md`](./CHANGELOG.md)
under `[Unreleased]` is shipped and tested. Breaking changes are possible
until a version is tagged.

## Features

- **Pluggable runtime.** Docker today; gVisor / Kata / Firecracker on the roadmap.
- **Hardened by default.** Cap-drop ALL, no-new-privs, read-only rootfs,
  `network=none`. You opt in to anything looser.
- **Egress allowlist.** Per-sandbox HTTPS CONNECT proxy on an internal-only
  network. Cloud-metadata IPs are always denied.
- **Warm pool.** Per-image pre-started containers turn ~150 ms cold-starts
  into ~1 ms claims for default-spec requests.
- **YAML policy.** Image allowlist, named profiles, egress rules, global
  deny list, defaults. Strict mode (`KP_REQUIRE_POLICY=1`) refuses to start
  without a policy file.
- **Observability.** Prometheus metrics on `kpd` and the proxy, structured
  logs with `sandbox_id` correlation, per-subsystem `/healthz`, ring-buffer
  logs of every exec.
- **Multiple surfaces.** HTTP API (`kpd`), MCP server (`kpmcp`), CLI
  (`kp`), Go SDK at `pkg/kotakpasir`.

## Architecture at a glance

```
┌────────┐    ┌──────────────┐    ┌─────────────────────┐
│  agent │ ─► │  kpd / kpmcp │ ─► │ Docker daemon       │
│ (LLM)  │    │  control     │    │  ├─ sandbox(es)     │
└────────┘    │  plane       │    │  └─ per-sandbox     │
              │              │    │     egress proxy    │
              │  + SQLite    │    └─────────────────────┘
              │  + warm pool │
              └──────────────┘
```

`kpd` is a single Go process backed by a local SQLite store. Each sandbox
that opts into `egress: allowlist` gets its own proxy container on its own
internal-only network — sandboxes can reach the proxy and nothing else.

For deeper design notes see [`docs/security-model.md`](./docs/security-model.md),
[`docs/egress-proxy.md`](./docs/egress-proxy.md), and
[`docs/warm-pool.md`](./docs/warm-pool.md).

## Quickstart

Prerequisites: Docker daemon, Go 1.25+.

```bash
git clone <this-repo> kotakpasir && cd kotakpasir

# Build the egress proxy image once.
make proxy-image

# Pull the default sandbox image.
docker pull alpine:latest

# Copy the example config.
cp kotakpasir.yaml.example kotakpasir.yaml
cp .env.example .env

# Run the daemon.
go run ./cmd/kpd
```

In another terminal, drive it from the CLI:

```bash
ID=$(go run ./cmd/kp run -i alpine:latest)
go run ./cmd/kp exec "$ID" -- echo "hello from inside"
go run ./cmd/kp logs "$ID"
go run ./cmd/kp rm "$ID"
```

Or end-to-end via the Go SDK — see [`examples/quickstart`](./examples/quickstart)
and [`examples/streaming-pipeline`](./examples/streaming-pipeline).

Step-by-step walkthrough: [`docs/getting-started.md`](./docs/getting-started.md).
Putting it on a real server (systemd, TLS, Prometheus, backups): [`docs/deployment.md`](./docs/deployment.md).

## Configuration

Two files. Both have annotated examples checked in.

- **`kotakpasir.yaml`** — sandbox policy: images, profiles, egress, defaults.
  Path via `KP_POLICY_FILE` or `--policy`.
- **`.env`** — server / connection knobs (auto-loaded by `kpd` and `kpmcp`).

See `kotakpasir.yaml.example` and `.env.example` for the full surface.

## Surfaces

### HTTP API (`kpd`)

REST + SSE on Fiber v3. Bearer auth (`KPD_TOKEN`); `/healthz` and `/_metrics`
are unauthenticated by design — gate them via reverse proxy if exposed.

```
POST   /v1/sandboxes
GET    /v1/sandboxes
GET    /v1/sandboxes/:id
GET    /v1/sandboxes/:id/proxy        # egress proxy address (404 if none)
GET    /v1/sandboxes/:id/logs         # exec output ring buffer; ?follow=true for SSE
DELETE /v1/sandboxes/:id
POST   /v1/sandboxes/:id/stop
POST   /v1/sandboxes/:id/exec         # buffered
POST   /v1/sandboxes/:id/exec/stream  # SSE: stdout / stderr / exit events
GET    /healthz                       # per-subsystem; 200/503
GET    /_metrics                      # Prometheus
```

### CLI (`kp`)

```
kp ls                                 # list sandboxes (tabwriter)
kp run -i alpine:latest               # create + wait, print id
kp run -p python-data                 # using a named profile
kp exec <id> -- <cmd...>              # buffered
kp exec --stream <id> -- <cmd...>     # live SSE
kp logs <id> [-f] [--tail N]          # captured exec output
kp watch [id] [--interval Ns]         # state-change feed
kp inspect <id>                       # full JSON for jq
kp stop <id> | kp rm <id>             # lifecycle
kp completion {bash|zsh|fish|powershell}
```

Errors carry actionable hints (`→ run kp ls` / `→ check kotakpasir.yaml` /
`→ set KPD_TOKEN`). The CLI exits with the in-sandbox command's exit code so
scripts compose naturally.

### MCP server (`kpmcp`)

Six typed tools over stdio (`sandbox_create`, `sandbox_list`, `sandbox_get`,
`sandbox_exec`, `sandbox_stop`, `sandbox_delete`). Drop into any
MCP-compatible client (Claude Desktop, Cursor, etc.).

### Go SDK (`pkg/kotakpasir`)

```go
c := kotakpasir.New("http://127.0.0.1:8080",
    kotakpasir.WithToken(os.Getenv("KPD_TOKEN")))

sb, _ := c.Create(ctx, kotakpasir.CreateOptions{Image: "alpine:latest"})
defer c.Delete(ctx, sb.ID)

_, _ = c.WaitFor(ctx, sb.ID, kotakpasir.StateRunning, 0)

res, _ := c.Exec(ctx, sb.ID, kotakpasir.ExecOptions{
    Cmd: []string{"echo", "hi"},
})
fmt.Println(res.Stdout)
```

Typed errors (`ErrNotFound`, `ErrUnauthorized`, `ErrPolicyDenied`,
`ErrBadRequest`) work with `errors.Is`.

## Project layout

```
cmd/
  kpd/        HTTP control plane
  kpmcp/      MCP server
  kp/         CLI
  kpproxy/    standalone egress proxy
internal/
  api/        Fiber HTTP layer
  config/     env + .env loader
  metrics/    Prometheus instruments for kpd
  pool/       warm-pool implementation
  policy/     YAML policy parsing + resolver
  proxy/      egress proxy (CONNECT enforcement)
  runtime/    Runtime interface + Docker backend + conformance suite
  sandbox/    Manager, reaper, store, ring-buffer logs
pkg/kotakpasir/  public Go SDK
examples/        quickstart, streaming-pipeline
docs/            design + reference
```

## Security model — the short version

What it protects against: an LLM agent running `rm -rf /`, a misbehaving
sandbox process exhausting host resources, accidental data exfiltration to
arbitrary hosts, container processes escaping common Linux defaults.

What it does **not** protect against: kernel exploits, deliberately
adversarial container escapes, side-channel attacks. Containers aren't a
security boundary for fully untrusted code — pick gVisor or Firecracker
when those backends ship.

Full threat model and decisions log: [`docs/security-model.md`](./docs/security-model.md).

## Roadmap and changelog

- [`ROADMAP.md`](./ROADMAP.md) — direction split into Now / Next / Later /
  Open questions.
- [`CHANGELOG.md`](./CHANGELOG.md) — what's already shipped.

## Contributing

Issues and PRs welcome. If you're picking up a roadmap item, opening an
issue first lets us sanity-check scope before you spend time. The
conformance suite at `internal/runtime/runtimetest/` is the source of truth
for what a runtime backend must do — new backends should pass it.

## License

[MIT](./LICENSE.md). Use it, fork it, ship it.

## Origin

Built after seeing [this post](https://x.com/theCTO/status/2048983816799416743)
about agent sandboxing. The hosted options were great but pricey;
microVM stacks needed KVM that a $5 VPS doesn't expose. So: hardened Docker,
own control plane, ship it. The MVP went from 19:30 to 23:00 WIB on day one,
and it's been growing since.
