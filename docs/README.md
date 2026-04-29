# kotakpasir docs

This directory holds design and reference docs. For project-level docs see
the repo root: [`README`](../README.md), [`ROADMAP`](../ROADMAP.md),
[`CHANGELOG`](../CHANGELOG.md).

| Doc | What's in it |
|---|---|
| [getting-started.md](./getting-started.md) | **Start here**: from cold-open terminal to running the SDK quickstart in two terminals. Copy-pastable commands, what to expect, common fixes. |
| [deployment.md](./deployment.md) | Putting kotakpasir on a real server: build, systemd unit, TLS, Prometheus scrape, backups, updates, common pitfalls. |
| [security-model.md](./security-model.md) | What kotakpasir protects against, what it doesn't, and why. The trust boundaries and the deliberate non-goals. |
| [egress-proxy.md](./egress-proxy.md) | Design and lifecycle of the per-sandbox egress proxy. How `mode: allowlist` works in practice. |
| [warm-pool.md](./warm-pool.md) | How per-image warm pools turn ~150 ms cold-starts into ~1 ms claims, and what's deliberately excluded. |
