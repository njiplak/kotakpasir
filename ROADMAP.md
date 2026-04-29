# Roadmap

kotakpasir's direction at a glance. This is a strategic view — for fine-grained
work, see open issues. Items shift between sections as priorities change;
nothing here is a commitment.

If you want to pick something up, opening an issue first is a good idea so we
can sanity-check scope and avoid duplicate work.

## Next

Wanted soon, design mostly clear.

- Python and TypeScript SDKs. Most agents are written in one of those two; the
  Go SDK shape (`Create / WaitFor / ExecStream / Logs`) ports cleanly.
- MCP streaming exec via `notifications/progress` (or `CallToolResult` text
  chunks — needs validation against Claude Desktop / Cursor).
- gVisor (`runsc`) backend. The first non-Docker runtime is mostly a
  copy of the Docker impl with `--runtime=runsc`; conformance suite covers it.
- DNS-level allowlist on the egress proxy — fails earlier and reads cleaner in
  logs than CONNECT-time filtering.
- Per-host rate limiting on the proxy (token bucket) to prevent runaway agents
  from hammering an external API.

## Later

Bigger or riskier; revisit when there's a real driver.

- Firecracker backend (no Docker). Forces `Spec` to grow a backend-specific
  extras field; first real test of the runtime interface's edges.
- Kata Containers backend.
- CRIU-based pause/resume for instant cold-start. Linux-only.
- Sandbox port publishing (inbound) for webhook delivery. Per-sandbox source-IP
  allowlist, explicit policy opt-in.
- Web UI: live sandbox list, click-to-exec, real-time logs.

### Production-shaped, multi-tenancy

- API tokens with scopes (per-team, read-only / per-image / per-profile).
- Audit log of every Create / Exec / Delete with caller identity.
- RBAC matrix in policy: tokens → allowed images / profiles / max sandboxes /
  max TTL.
- Org / project scoping with quotas.
- HA story: Litestream-replicated SQLite, Postgres backend, or shard by
  sandbox-id.

## Open questions

- Should we publish `kotakpasir/proxy` to a public registry, or keep it as a
  contributor-built image? Trade-off: convenience vs version+sign+maintain
  obligation.
- Should the conformance suite run gVisor + Kata in CI on every PR? Each
  backend extends test wall-time meaningfully.
- What's the right shape for mounted volumes? Today sandboxes are stateless
  beyond `/tmp`. A per-sandbox named volume mounted at `/workspace`, deleted
  with the sandbox? Need to think through interaction with the read-only rootfs.
- Should the warm pool support per-profile pools, not just per-image? Profiles
  with non-default cpus/memory currently always cold-start.
- Path-level audit (would require MITM, deliberately ruled out today). Worth
  revisiting if there's demand.

## License

MIT — see [`LICENSE.md`](./LICENSE.md).
