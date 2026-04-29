# Security model

This document records the security decisions kotakpasir has made, the threat models it aims to address, and — equally importantly — the threats it deliberately doesn't try to stop. New contributors should read this before adding a "security feature" so we don't accidentally drift toward a model the project was never trying to be.

## Trust boundaries

```
┌──────────────────────────────────────────────────────────────────┐
│                                                                  │
│   operator (host)                                                │
│   ┌──────────────────────────────────────────────────────────┐   │
│   │   kpd / kpmcp                                            │   │
│   │   ┌──────────────────────────────────────────────────┐   │   │
│   │   │  policy (kotakpasir.yaml)                        │   │   │
│   │   │  store (sqlite)                                  │   │   │
│   │   └──────────────────────────────────────────────────┘   │   │
│   └──────────────────────────────────────────────────────────┘   │
│            │ docker exec / docker create                         │
│            ▼                                                     │
│   ┌──────────────────────────────────────────────────────────┐   │
│   │  sandbox container                  ← UNTRUSTED          │   │
│   │  - non-root, read-only rootfs                            │   │
│   │  - cap-drop=ALL, no-new-privs                            │   │
│   │  - network=none (or via proxy when egress allowlisted)   │   │
│   │  - pids, memory, cpu limits                              │   │
│   │  - holds whatever env vars the API caller sent           │   │
│   └──────────────────────────────────────────────────────────┘   │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

The trust boundary is the **container wall**. Anything inside the sandbox is untrusted; the host (`kpd`) is trusted. Code inside the sandbox can do anything to itself; we only stop it from doing things to *other* sandboxes, the host, or the rest of the internet.

## What kotakpasir tries to prevent

| Threat | Mitigation |
|---|---|
| Sandbox code escalates to root on the host | `cap-drop=ALL`, `no-new-privileges`, non-root UID, read-only rootfs, pids limit |
| Sandbox code exhausts host resources (forkbomb, OOM, CPU peg) | `--pids-limit`, `--memory`, `--cpus` |
| Sandbox code reaches arbitrary internet endpoints | Default `network=none`; `egress.mode: allowlist` for opt-in network |
| Sandbox code reaches cloud metadata endpoints (`169.254.169.254`) | `egress.global_deny` always wins, even over allowlists |
| Operator runs an unsanctioned image by accident | `kotakpasir.yaml` image allowlist (when `images:` is non-empty) |
| Sandboxes pile up forever and exhaust disk / memory | TTL + reaper (`KP_REAPER_INTERVAL`) |
| Two sandboxes interact when they shouldn't | Each is its own container; with the egress proxy, each gets its own Docker network |

## What kotakpasir explicitly does NOT try to prevent

These are **non-goals** — listing them so the next person reading the codebase doesn't try to "add" them in a way that complicates the architecture without buying a real defense.

### 1. Hostile-tenant kernel escapes

Containers share a kernel with the host. A kernel CVE compromises every container on a host simultaneously (e.g., runc CVE-2024-21626). Mitigations like `cap-drop=ALL` and seccomp shrink the attack surface but don't eliminate it.

If your threat model is **untrusted code from many separate tenants** (e.g. multi-tenant SaaS where customers' code runs in your sandboxes), wrap kotakpasir's runtime in something with hardware isolation: gVisor (`runsc`), Kata Containers, or Firecracker. The `Runtime` interface is designed for that swap.

If your threat model is **your own agent code that you wrote** running LLM-generated commands, the default container runtime is fine.

### 2. Secrets exfiltration from inside the sandbox

This is the one most worth being explicit about, because microsandbox markets a "secrets that can't leak" feature and we deliberately don't.

**The model**: API callers bring their own credentials in the `env` map of the create request. Those env vars live in the sandbox's environment at runtime. Code running inside the sandbox **can read them**.

**Why we accept this**:

- The caller already has the secret — they're the one who sent it
- Whoever writes/runs the agent code is responsible for what that code does with the env it was given
- The alternative (TLS-layer secret injection via MITM proxy) requires injecting our CA into every sandbox, breaks cert-pinning libraries, and adds a meaningful attack surface to the proxy itself
- For most agent workloads, **short-lived per-sandbox credentials** are operationally simpler and almost as good. If the credential's blast radius is "1 hour with one tenant's scope," leaking it isn't catastrophic.

**What we still do** to bound the damage if a secret does leak:

- The egress proxy stops the sandbox from POSTing the env to `attacker.com` — host allowlists are defense in depth even though they don't hide the value from the sandbox process itself
- We do not log env values, anywhere, ever
- API responses do not echo back env values

If your threat model genuinely requires the secret to be invisible to code running inside the sandbox, we are not the right tool. Use a system that issues a separate credential-broker process (like SPIFFE/SPIRE, Vault Agent, or AWS IRSA), or accept the cost of MITM injection in a dedicated proxy.

### 3. Inbound traffic to sandboxes

Sandboxes do not accept inbound network connections. The host talks *into* the sandbox via `docker exec` (a control-plane channel through the Docker daemon), not via a published port. This is by design and removes an entire class of "agent runs a vulnerable web server inside the sandbox" concerns.

If a future use case needs inbound (e.g. webhook delivery to a sandbox), it's a separate feature — port publishing, tied to allowlisted source addresses — and not something the egress proxy or current design covers.

## Decisions log

### 2026-04-28 — BYO-env model, no host-side secret injection

**Decision**: API callers pass their own credentials in `env`. `kotakpasir.yaml` has no `secrets:` section. `kpd` does not store, manage, or rotate caller credentials.

**Rationale**: kotakpasir's primary audience is operators running their own agent code (or running known agents on behalf of users). Those agents already have access to the credentials they need; forcing them through a kpd-owned secret store would add a configuration step, a rotation surface, and a single point of credential failure for no security gain. Operators who *do* want central credential management can layer it externally — SOPS-decrypted env at process start, Vault Agent sidecar, etc.

**Implications**: the egress proxy becomes more important (defense in depth for caller credentials); we never log env contents; secret rotation is the caller's responsibility.

### 2026-04-28 — Drop MITM from the egress proxy

**Decision**: The egress proxy operates in HTTPS CONNECT mode only. There is no plan to add a MITM mode that terminates TLS to inspect or rewrite headers.

**Rationale**:

- MITM is only useful for *header rewriting* and *secret injection at the TLS layer*. We've decided not to do secret injection (above). Header rewriting alone doesn't justify the cost.
- Operationally expensive: requires a per-`kpd` CA, persisted to disk, with a private key whose loss is a security incident; requires injecting that CA into every sandbox's trust store; breaks any image with cert-pinning code (Stripe SDKs, AWS SDKs in some configs, mTLS clients) in ways that are hard to debug.
- The CONNECT-mode allowlist gets us 100% of the egress-control value with ~30% of the implementation cost.

**Implications**: the proxy can enforce *which hosts* a sandbox reaches, but cannot inspect or modify what it sends. That's deliberate.

### 2026-04-28 — Pluggable runtime, Docker by default

**Decision**: `internal/runtime.Runtime` is the abstraction; `internal/runtime/docker` is the only implementation today. Future backends (gVisor, Kata, Firecracker) plug in behind the same interface.

**Rationale**: lets the project ship today on commodity Docker while leaving a clean upgrade path for operators with stronger isolation requirements. The runtime conformance test suite (`internal/runtime/runtimetest`) ensures any new backend honors the contract.

## Threat-model decision tree (for operators)

If you're deploying kotakpasir, walk through this:

1. **Who writes the code that runs inside?**
   - You / your team only → default config is fine
   - Anyone who sends you a request (multi-tenant SaaS, public service) → you need stronger isolation than container default — plan to add `runsc` or `kata` runtime
2. **Is your sandbox going to make outbound calls?**
   - No → `egress.default.mode: none` and you're done
   - Yes, to a known set of hosts → `egress.mode: allowlist` per image/profile, list the hosts
3. **Can the caller's credentials in env do real damage if exfiltrated?**
   - No (short-lived, scoped, rotatable cheaply) → default BYO-env is fine
   - Yes → mint short-lived tokens at the caller layer before passing to `kpd`; do not give the sandbox a long-lived high-privilege key
4. **Do sandboxes ever need to be reachable from outside?**
   - No → don't change anything (this is the default)
   - Yes → kotakpasir doesn't currently support this; add port publishing as a separate feature with explicit per-sandbox source-IP allowlisting
