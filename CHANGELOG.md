# Changelog

All notable changes to kotakpasir are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project will
adopt [Semantic Versioning](https://semver.org/) once a `1.0.0` is cut.

## [Unreleased]

Everything below is pre-1.0. Breaking changes are possible; tag a release
before depending on stability.

### Added

#### Core

- Pluggable `Runtime` interface (`internal/runtime/`) with a Docker backend.
  Cap-drop ALL, no-new-privs, read-only rootfs, pids/cpu/memory limits,
  `network=none` by default.
- SQLite store with WAL mode for sandbox state.
- `Manager` lifecycle (`internal/sandbox/manager.go`) and TTL reaper goroutine.
- Per-image warm pool with eager fill, async refill, and orphan cleanup on
  startup.
- YAML policy: image allowlist, named profiles, egress modes, global deny
  list, defaults. `KP_REQUIRE_POLICY=1` (or `--require-policy`) makes `kpd`
  refuse to start without a readable policy file.
- Pre-pull declared images at `kpd` startup via an optional `imageEnsurer`
  capability. Logs `image present` (cache hit) or `image pulled duration=Ns`.

#### Surfaces

- HTTP API (`kpd`) on Fiber v3: bearer auth, full CRUD, stop, exec, streaming
  exec via SSE.
- MCP server (`kpmcp`) using the official Go SDK, six typed tools, stdio
  transport.
- CLI (`kp`) with `ls / run / exec / exec --stream / inspect / stop / rm /
  logs / watch / completion`. Driven entirely by the public Go SDK; exits with
  the in-sandbox command's exit code so scripts work; typed errors give
  actionable hints.
- Public Go SDK at `pkg/kotakpasir/`: `Create / Get / List / Stop / Delete /
  Exec / ExecStream / Logs / LogsStream / WaitFor`. Typed errors
  (`ErrNotFound`, `ErrUnauthorized`, `ErrPolicyDenied`, `ErrBadRequest`).
- Standalone egress proxy binary (`kpproxy`).

#### Network + egress

- Per-sandbox proxy container on a per-sandbox internal network for
  `egress: allowlist`. Proxy enforces HTTPS CONNECT (no MITM, no path-level
  inspection). Global deny list always wins over allowlist. Three-way teardown
  on Delete.
- `GET /v1/sandboxes/:id/proxy` returns the egress proxy's routable address
  for sandboxes that have one (404 otherwise).

#### Observability

- `kpproxy` exposes `/_health` and `/_metrics` (`kpproxy_connect_allow_total`,
  `kpproxy_connect_deny_total{reason}`).
- `kpd` exposes `/_metrics` with `kpd_sandbox_created_total{image,profile,outcome}`
  (success | policy_denied | runtime_error), `kpd_sandbox_active`,
  `kpd_pool_hit_total{image}`, `kpd_pool_miss_total{image}`,
  `kpd_reaper_swept_total`.
- `kpd` `/healthz` reports per-subsystem status (`store`, `runtime`,
  `pool:<image>`); 200 when everything's healthy, 503 on any degradation.
- Sandbox-id correlation in `slog`: `Manager` emits `sandbox_id=<id>` on every
  lifecycle event, so `grep` traces an instance from create through reap.
- Docker `HEALTHCHECK` directive on the proxy image (10s interval).
- Per-sandbox ring buffer of exec output (default 256 KB,
  `KP_LOG_BUFFER_BYTES` overrides; 0 disables). Exposed via
  `GET /v1/sandboxes/:id/logs[?tail=N&follow=true]` — text/plain or SSE.

#### Quality

- `runtimetest.Suite` conformance tests against Docker (Lifecycle, ExecStreams,
  Hardening, EgressAllowlist).
- Policy resolver unit tests (precedence + validation).
- Ring-buffer unit tests (`internal/sandbox/logbuf`).
- SDK typed-error tests.
- `go vet` clean across the whole module; compile-time interface checks.

#### Tooling

- `Makefile` with `proxy-image`, `build`, `test`, `vet` targets. `PROXY_IMAGE`
  overrides the default tag.

#### Docs

- `docs/security-model.md` — threat model, decisions log, non-goals.
- `docs/egress-proxy.md` — design, lifecycle, failure modes.
- `docs/warm-pool.md` — semantics, performance numbers, security implications.
- `docs/getting-started.md` — copy-pastable setup.
- Annotated `kotakpasir.yaml.example` and `.env.example`.
