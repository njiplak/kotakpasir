# Getting started

Picking the project back up tomorrow on the same machine? Walk through this doc top-to-bottom. It's the fastest path from cold-open terminal to "I'm exec-ing commands inside a sandbox via the SDK."

## Prerequisites (already satisfied on this machine)

You have these already from earlier sessions:

| Thing | Why | How to verify |
|---|---|---|
| Docker daemon running | runtime + proxy + sandbox containers | `docker version` |
| Go 1.25.7 | the project requires it | `go version` |
| `alpine:latest` image cached | default sandbox image for the policy | `docker images alpine:latest` |
| `kotakpasir/proxy:dev` image | the egress proxy | `docker images kotakpasir/proxy:dev` |

If any of those are missing, recover with:

```bash
# Pull alpine
docker pull alpine:latest

# Build the proxy image (takes ~30s the first time)
cd /Users/ilzam/Private/kotakpasir
docker build -f Dockerfile.kpproxy -t kotakpasir/proxy:dev .
```

## One-time setup

Copy the example policy to a working file. The `.example` file is committed; the working file isn't (it's already in `.gitignore` as `*.yaml` would be too aggressive — there's no specific ignore yet, so just keep it locally).

```bash
cd /Users/ilzam/Private/kotakpasir
cp kotakpasir.yaml.example kotakpasir.yaml
```

Optionally a `.env`:

```bash
cp .env.example .env
```

`.env` is auto-loaded by `kpd` and `kpmcp`. The defaults inside it work fine, so you usually don't need to touch it.

## Day-to-day workflow (two terminals)

### Terminal 1 — run kpd

```bash
cd /Users/ilzam/Private/kotakpasir
KP_POLICY_FILE=./kotakpasir.yaml go run ./cmd/kpd
```

You should see:

```
INFO warm pool ready image=alpine:latest target=3
INFO kpd listening addr=:8080 auth=false db=./kotakpasir.db policy=./kotakpasir.yaml images=2 profiles=1
INFO Server started on:  http://127.0.0.1:8080
```

The "warm pool ready" line confirms the pool spawned 3 alpine containers and they're idle waiting for claims.

### Terminal 2 — run the quickstart

```bash
cd /Users/ilzam/Private/kotakpasir
go run ./examples/quickstart
```

Expected output:

```
--- create ---
sandbox 7c7f2c08-... state=running

--- buffered exec ---
exit=0 duration=55ms
stdout: "hello\n"
stderr: "problem\n"

--- streaming exec (3-second loop) ---
  out| step 1
  out| step 2
  out| step 3
exit=0 duration=3.065s
--- deleted 7c7f2c08-... ---
```

If you see all four sections, everything is working: HTTP API, SQLite store, runtime, warm pool, streaming SSE, SDK.

## Things to play with

The order below goes roughly easiest → most interesting.

### 1. Watch the warm pool work

Edit `kotakpasir.yaml` and bump pool size:

```yaml
images:
  - name: alpine:latest
    pool: 5
```

Restart `kpd` (terminal 1: Ctrl-C, re-run). In terminal 2:

```bash
# Time five back-to-back creates
for i in 1 2 3 4 5; do
  curl -s -o /dev/null -w "create #$i: %{time_total}s\n" \
    -X POST http://127.0.0.1:8080/v1/sandboxes \
    -H 'Content-Type: application/json' -d '{"image":"alpine:latest"}'
done

# Clean up
curl -s http://127.0.0.1:8080/v1/sandboxes | jq -r '.[].id' \
  | xargs -I {} curl -s -X DELETE http://127.0.0.1:8080/v1/sandboxes/{}
```

You should see ~1ms creates because the pool keeps refilling.

Now set `pool: 0` and re-run — creates jump to ~150ms (cold start).

### 2. Test the egress proxy

The example policy has a `egress-test` profile that uses `egress.mode: allowlist` for `example.com`. Try it:

```bash
curl -s -X POST http://127.0.0.1:8080/v1/sandboxes \
  -H 'Content-Type: application/json' \
  -d '{"profile":"egress-test"}' | jq
```

You should see two new containers: `kp-<id>` (sandbox) and `kp-proxy-<id>` (proxy):

```bash
docker ps --filter label=kotakpasir.managed=true --format '{{.Names}} ({{.Status}})'
```

Inside the sandbox, the allowlist is enforced:

```bash
ID=<paste from the create response>

# Allowed → "HTTP/1.0 200 Connection established"
curl -s -X POST "http://127.0.0.1:8080/v1/sandboxes/$ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["sh","-c","printf '\''CONNECT example.com:443 HTTP/1.1\\r\\nHost:e\\r\\n\\r\\n'\'' | nc -w 3 proxy 8080 | head -1"]}' \
  | jq -r .stdout

# Denied → "" (proxy closes the connection)
curl -s -X POST "http://127.0.0.1:8080/v1/sandboxes/$ID/exec" \
  -H 'Content-Type: application/json' \
  -d '{"cmd":["sh","-c","printf '\''CONNECT 1.1.1.1:443 HTTP/1.1\\r\\nHost:e\\r\\n\\r\\n'\'' | nc -w 3 proxy 8080 | head -1"]}' \
  | jq -r .stdout
```

Then check the proxy's metrics:

```bash
docker exec kp-proxy-$ID /busybox/wget -qO- http://localhost:8080/_metrics | grep ^kpproxy
```

You should see `kpproxy_connect_allow_total{host="example.com"} 1` and `kpproxy_connect_deny_total{host="1.1.1.1",reason="not_in_allowlist"} 1`.

Don't forget to delete:

```bash
curl -s -X DELETE "http://127.0.0.1:8080/v1/sandboxes/$ID"
```

### 3. Build something with the SDK

The quickstart demonstrates the API. Modify it or copy it into a new file. The SDK lives at `nexteam.id/kotakpasir/pkg/kotakpasir`.

Streaming exec writes to whatever `io.Writer` you pass. To pipe stdout straight to your terminal:

```go
res, err := c.ExecStream(ctx, sb.ID, kotakpasir.ExecOptions{
    Cmd: []string{"sh", "-c", "for i in 1 2 3; do date; sleep 1; done"},
}, os.Stdout, os.Stderr)
```

You'll see output appear in real time, not buffered until completion.

To capture into a buffer for later parsing:

```go
var stdout bytes.Buffer
c.ExecStream(ctx, sb.ID, opts, &stdout, nil)
```

### 4. Try the MCP server

`kpmcp` exposes the same lifecycle as the HTTP API but over MCP stdio (the protocol Claude Desktop, Cursor, etc. speak). Quick check from the terminal:

```bash
{
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1.0"}}}'
  printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
  sleep 0.3
} | go run ./cmd/kpmcp 2>/dev/null | jq -r '.result.tools[]?.name // empty'
```

Output should be the six tool names: `sandbox_create`, `sandbox_list`, `sandbox_get`, `sandbox_exec`, `sandbox_stop`, `sandbox_delete`.

### 5. Use the `kp` CLI

The CLI wraps the SDK so you can drive everything from the terminal without curl/jq. Examples (terminal 2, while kpd runs in terminal 1):

```bash
# Build once for convenience (or use `go run ./cmd/kp ...` each time)
go build -o /tmp/kp ./cmd/kp
alias kp=/tmp/kp

# Lifecycle
ID=$(kp run -i alpine:latest)         # creates + waits for running, prints id
kp ls                                  # tabwriter listing
kp inspect "$ID"                       # full JSON for jq

# Exec — buffered (returns when done)
kp exec "$ID" -- sh -c "echo hi; id -u"

# Exec — streaming (live SSE; good for long-running)
kp exec --stream "$ID" -- sh -c "for i in 1 2 3; do echo step \$i; sleep 1; done"

# Cleanup
kp stop "$ID"           # stops the container, keeps the row
kp rm "$ID"             # tears down container + proxy + network if any

# Profiles
ID=$(kp run -p shell)   # uses kotakpasir.yaml profile

# Configuration
KPD_ADDR=http://other-host:8080 kp ls
KPD_TOKEN=secret kp ls

# Exit codes propagate through `kp exec` so scripts work:
if ! kp exec "$ID" -- ./run-tests.sh; then
  echo "tests failed"
fi
```

Note the `--` between `<id>` and your command — it tells the CLI to stop parsing flags so things like `-c` get through to the sandbox.

Typed errors give hints:

```
$ kp run -i ubuntu
kp: kotakpasir: policy denied (policy violation: image "ubuntu" not in policy.images allowlist)
  → check kotakpasir.yaml allowlist or use a defined profile

$ kp exec wrong-id -- echo hi
kp: kotakpasir: not found (not found)
  → run `kp ls` to see available sandboxes
```

### 6. Run the conformance suite

```bash
KP_DOCKER_TESTS=1 go test -v ./internal/runtime/docker/...
```

Four subtests, ~7 seconds. Hit this whenever you suspect something runtime-shaped broke. They cover Lifecycle, ExecStreams, Hardening, and EgressAllowlist.

## Image handling

| Image type | Auto-pulled? | Behavior |
|---|---|---|
| Sandbox images (e.g. `alpine:latest`, `python:3.12-slim`) | **Yes** | First Create against an un-pulled image triggers a pull; that one request blocks until the pull finishes (can take seconds-to-minutes for large images). Subsequent creates hit the local cache. |
| `kotakpasir/proxy:dev` | **No** | Must be built locally with `docker build -f Dockerfile.kpproxy -t kotakpasir/proxy:dev .`. If missing, Create fails with the build command in the error message. |

**Warm-pool gotcha**: if `pool: N` is set on an image that isn't pulled yet, `kpd` startup blocks on the pull (the first pool warmup triggers it). Pre-pull manually if you want fast startup:

```bash
docker pull alpine:latest          # plus any other images you've declared
```

## Common issues + fixes

| Symptom | Cause | Fix |
|---|---|---|
| `pull image "kotakpasir/proxy:dev": ... not found` on Create | Proxy image not built | `docker build -f Dockerfile.kpproxy -t kotakpasir/proxy:dev .` |
| `address already in use` when starting kpd | Another kpd / something on :8080 | `lsof -i :8080` then kill, or `--addr :8081` |
| `database is locked` | Another kpd open on same `KPD_DB` | Find and kill the other process, or use a different `--db` |
| `image "X" not in policy.images allowlist` | YAML has `images:` defined and your image isn't listed | Add it to YAML, or remove the `images:` block to allow any |
| Sandbox container left after Ctrl-C of kpd | Reaper TTL hasn't fired yet | `docker ps -a --filter label=kotakpasir.managed=true --format '{{.ID}}' \| xargs -r docker rm -f` |
| Quickstart hangs on `Health` | kpd not running, wrong port | Make sure terminal 1 is alive and on `:8080` |
| Slow first create even with pool | First request after kpd start; pool needed time to warm | Wait ~2s after the "warm pool ready" log line |
| `EOF` from ExecStream | kpd died mid-stream | Check terminal 1 logs |

## Useful commands while playing

```bash
# Watch kotakpasir-managed containers in real time (separate terminal)
watch -n 1 'docker ps -a --filter label=kotakpasir.managed=true \
  --format "table {{.Names}}\t{{.Status}}\t{{.Image}}"'

# Tail proxy logs for one sandbox
docker logs -f kp-proxy-<sandbox-id>

# Inspect resource limits on a sandbox
docker inspect kp-<sandbox-id> --format '
  ReadonlyRootfs: {{.HostConfig.ReadonlyRootfs}}
  NetworkMode:    {{.HostConfig.NetworkMode}}
  CapDrop:        {{.HostConfig.CapDrop}}
  PidsLimit:      {{.HostConfig.PidsLimit}}
  NanoCpus:       {{.HostConfig.NanoCpus}}
  Memory:         {{.HostConfig.Memory}}'

# Nuke ALL kotakpasir state (containers, networks, sqlite)
docker ps -a --filter label=kotakpasir.managed=true --format '{{.ID}}' | xargs -r docker rm -f
docker network ls --filter label=kotakpasir.managed=true --format '{{.ID}}' | xargs -r docker network rm
rm -f kotakpasir.db kotakpasir.db-shm kotakpasir.db-wal
```

## Where to read more

- [`docs/security-model.md`](./security-model.md) — what kotakpasir does and doesn't try to protect against
- [`docs/egress-proxy.md`](./egress-proxy.md) — full egress proxy design
- [`docs/warm-pool.md`](./warm-pool.md) — pool semantics + perf numbers
- `kotakpasir.yaml.example` — every policy field in one annotated file
- `.env.example` — every operator env var

## What's still missing (good places to extend)

If you find yourself wanting:

- **`/v1/sandboxes/:id/proxy` endpoint** to discover the proxy bridge IP for ops scraping
- **MCP streaming progress** (currently `sandbox_exec` is buffered; the SSE flow could be ported)
- **kpd-level metrics** (`kpd_sandbox_active`, `kpd_sandbox_created_total{image=...}`)
- **Image pre-pull at startup** for declared images
- **`kp` CLI feature parity** with the HTTP API (`kp ls`, `kp exec`, etc.)
- **Streaming exec in MCP** via `notifications/progress`

Each of those is a small, independent PR that doesn't disturb the core architecture.
