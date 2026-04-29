# Warm pool

Docker's biggest weakness as a sandboxing runtime is cold-start latency: a `docker run` against an already-pulled image still takes 150–500 ms because the daemon must build the cgroup, set up the network, attach storage, and start the container process. Microsandbox's microVMs hit ~100 ms because they skip most of that work.

We close most of that gap with a warm pool: per-image queues of pre-started containers that requests can claim instantly, with an async refiller spawning replacements in the background.

## Numbers from this codebase

Measured via the HTTP API on Docker Desktop 28.3 / Apple Silicon:

| Path | p50 latency | Notes |
|---|---|---|
| Cold start (no pool) | **~150 ms** | `docker run` against pre-pulled `alpine:latest` |
| Warm claim (pool=3) | **~1 ms** | Pop from pool + write SQLite row |

That's a **100–300× speedup** for the common case.

## How it works

```
kpd startup
   │
   ▼
For each image with `pool: N` in policy:
   │   spawn N hardened containers running `tail -f /dev/null`
   │   each labeled kotakpasir.role=pool-warm
   │
   ▼
agent: POST /v1/sandboxes  { image: alpine:latest }
   │
   ▼
Manager.Create:
   │   1. policy.Resolve → effective spec
   │   2. if spec matches the pool exactly:
   │         pop one container ID from pool
   │         write sandbox row to sqlite (RuntimeID = popped ID)
   │         signal refiller to spawn replacement
   │         return ← ~1 ms
   │   3. otherwise:
   │         docker run new container ← ~150 ms
```

The async refiller maintains the target queue size by spawning replacements as the pool drains. With a target of 3 and modest concurrency, the pool stays full enough that even bursty workloads see warm hits.

## What gets pooled

A pool entry must match the resolved spec exactly, because resource limits are mostly immutable on a running Docker container. Concretely, a request hits the pool only when **all** of these are true:

| Condition | Why |
|---|---|
| `image` matches a pool image | obvious |
| `egress.mode` is `none` (or empty) | per-sandbox proxy + network can't be pre-warmed; each sandbox needs its own |
| Resolved `cpus` matches the pool's | `--cpus` is set at create time |
| Resolved `memory_mb` matches the pool's | same |

If any condition fails, we transparently cold-start instead. Operators can use this to allow some flexibility (per-image policy with `pool: N` for the common shape) while still allowing one-off requests with custom limits.

What pools are NOT used for:

- **`egress.mode: allowlist`** — by design. Each such sandbox needs its own per-sandbox network and proxy container, which can't be shared. Validation rejects `pool: N` combined with `egress.mode: allowlist` at policy load time.
- **Profiles or images with non-default cpu/memory** — only when the resolved spec matches what the pool was warmed with.
- **Per-request resource overrides** — same reason. If the caller asks for `cpus: 4` but the pool is warmed with `cpus: 1`, the request cold-starts.

## Configuration

Per image, in `kotakpasir.yaml`:

```yaml
images:
  - name: alpine:latest
    pool: 3              # keep 3 warm; default 0 (no pool)

  - name: python:3.12-slim
    cpus: 2.0
    memory_mb: 1024
    pool: 2              # pool with the image's own resource overrides

  - name: node:20-alpine
    egress:
      mode: allowlist
      hosts: [registry.npmjs.org]
    # No pool: incompatible with allowlist mode (would fail validation)
```

There's no top-level `pool:` knob; it's deliberately per-image so operators size hot images explicitly.

## Lifecycle

- **Startup**: `Manager.Start(ctx)` orphan-cleans any leftover `kotakpasir.role=pool-warm` containers from a previous kpd run, then eagerly spawns `pool: N` containers per image and launches the refiller goroutine.
- **Claim**: `Manager.Create` pops one ID, writes the sandbox row to SQLite, signals the refiller. The container is now indistinguishable from a cold-started one — the only label the pool added (`kotakpasir.role=pool-warm`) is harmless because cleanup goes by `kotakpasir.sandbox-id`, which the manager added on top.
- **Refill**: a goroutine spawns replacements until the pool is back at target. Runs in the kpd process context so it's automatically cancelled on shutdown.
- **Shutdown**: `Manager.Shutdown(ctx)` drains every pool — force-removes all remaining warm containers. Called from `kpd`'s deferred shutdown using a fresh background context (the parent context is already cancelled by the time deferred shutdown runs).
- **Reaper interaction**: claimed containers have a `kotakpasir.sandbox-id` label and a SQLite row; the reaper finds them via SQLite as before. Warm pool entries don't have a sandbox-id label and aren't in SQLite, so the reaper doesn't touch them — by design.

## Failure modes

| Failure | Behavior |
|---|---|
| Pool empty when a Create arrives | Cold-start path runs. Logged as a refill miss; refiller will catch up. |
| Pool refill fails (e.g. image pull error) | Logged as warning. Pool stays below target until refill succeeds. Cold-starts continue working. |
| Pooled container dies on its own | It stays "alive" from the pool's perspective until claimed; on claim it'll fail Exec. Future work: health-check warm entries before handing them out. |
| `kpd` killed mid-refill | Orphan cleanup on next startup removes any partially-warmed containers. |

## Security implications

Worth being explicit about, since pre-warming is a divergence from the "fresh container per request" model:

- **No state leakage**: pool entries are one-shot — they leave the pool on claim and never return. Two different sandboxes never share a container, even sequentially.
- **Same hardening as cold-start**: pool entries go through the exact same `runtime.docker.Create` path as cold sandboxes. They get `cap-drop=ALL`, `no-new-privileges`, read-only rootfs, the same pids/cpu/memory limits, and `network=none`. The pool is just an *earlier* version of the same operation.
- **Labels only**: pool entries have a small set of identification labels (`kotakpasir.role=pool-warm`, `kotakpasir.pool-image=<image>`, `kotakpasir.managed=true`). Nothing user-supplied is in them.
- **Resource cost**: each pool entry is a running process. With target=3 and 64 MB pid limits per container, the steady-state cost is 3 × image-baseline RAM. For `alpine:latest`'s `tail -f /dev/null` that's a few MB total. For heavier images (Python with started runtime, etc.) plan accordingly.

## When NOT to use the pool

- **Highly variable resource requests** — if every caller asks for different cpu/memory, nothing matches the pool and you pay the warming cost without the benefit.
- **Mostly egress workloads** — anything with `egress.mode: allowlist` is per-sandbox; pool doesn't help.
- **Strict cost ceilings** — pool entries are running processes. If you have hundreds of images each pooled at N=5, the steady-state container count gets large. Pool only the hot images.

## Future work

- **Health checks before handout** — currently `tail -f /dev/null` is so simple we don't bother, but if pool images get more complex (pre-imported language runtimes, etc.), validating the entry is alive before claim is worth it.
- **Resource overrides on claim via `docker update`** — Docker supports updating cpus/memory on a running container. We could pool generic and tune per-claim, broadening pool match to all default-image requests regardless of per-request cpu/memory.
- **Persistent metrics** — pool hit rate, average warm-time-to-claim, refill failures.
