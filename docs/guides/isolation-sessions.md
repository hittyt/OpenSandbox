---
title: Isolation Sessions
description: Per-execution bubblewrap-isolated bash sessions inside a sandbox for fast, contained code runs.
---

# Isolation Sessions

Isolation sessions run a **long-lived `bash` inside a
[bubblewrap](https://github.com/containers/bubblewrap) namespace**, so one
sandbox pod can host many mutually isolated task runs without spinning up a
new container. Each session gets its own PID, mount, tmpfs, and env
namespaces; startup is sub-millisecond.

## Table of Contents

- [Requirements](#requirements)
- [Overview](#overview)
- [How It Works](#how-it-works)
- [Quick Start](#quick-start)
- [Session Lifecycle](#session-lifecycle)
- [Workspace Modes](#workspace-modes)
- [Bind Mounts and Allowlist](#bind-mounts-and-allowlist)
- [Profiles and Defaults](#profiles-and-defaults)
- [Environment, UID, and Networking](#environment-uid-and-networking)
- [Filesystem Proxy](#filesystem-proxy)
- [Capabilities and Probing](#capabilities-and-probing)
- [Server Configuration](#server-configuration)
- [Limitations](#limitations)
- [See Also](#see-also)

---

## Requirements

Component versions needed for the features covered by this guide:

- `execd` >= 1.0.20 for base isolation session support; **>= 1.0.21
  recommended** for `binds`, `List sessions`, `uid_mode: "userns"`, and the
  default writable allowlist (`/workspace`, `/mnt`, `/media`, `/data`)
- `opensandbox-server` >= 0.2.1 — the server injects `CAP_SYS_ADMIN`,
  `apparmor=unconfined`, and the tmpfs mount required by `bwrap` when the
  execd image declares `bootstrap.execd.isolation`
- Python SDK >= 0.1.14 (`isolation.run_once` / `isolation.session` context
  manager); >= 0.1.13 for the generated isolation client only
- JavaScript / TypeScript SDK >= 0.1.10 (`isolation.runOnce` /
  `isolation.withSession`)
- Kotlin / Java SDK >= 1.0.16 (`isolation.runOnce()` /
  `isolation.withSession { ... }`); >= 1.0.15 for the generated isolation
  client only
- C# SDK >= 0.1.4 (`RunOnceAsync` / `WithSessionAsync`)
- Go SDK >= 1.0.4 (`IsolationRunOnce` / `IsolationWithSession`)

The `setpriv_available`, `userns_available`, `session_auth_mode`, and
`session_capability_required` fields on
[`/capabilities`](#capabilities-and-probing) were added after execd v1.0.21
and are only present on newer execd builds; older execd may omit them. Clients
must tolerate their absence and interpret missing session-auth fields as
`legacy` / `false`.

Host requirements (`bwrap` binary, `CAP_SYS_ADMIN`, `overlayfs`, etc.) are
listed under [Server Configuration → Host Requirements](#server-configuration).

---

## Overview

| Concept | Boundary | Startup | Typical Use |
|---|---|---|---|
| **Isolation session** | bubblewrap namespaces inside one sandbox | ~100 ms to create; subsequent `run` in an existing session is near-zero overhead | Many short, mutually isolated task runs reusing one long-lived session |
| Bash session (`/session`) | bash process, no extra namespaces | ms | Interactive REPL-style command sequences |
| Sandbox | container or pod | 100s of ms to seconds | Tenant, workspace, or user boundary |
| Secure runtime (gVisor / Kata) | user-space kernel or VM per sandbox | 10–500 ms | Hardware-level protection against container escape |

::: info Session creation vs. per-run cost
Creating a session takes about 100 ms because execd waits briefly after
starting `bwrap` to detect an immediately-exiting child. Once the session
exists, each `POST /run` reuses the same bash process, so per-run overhead
is negligible. Design workloads that amortize the create cost across many
`run` calls in one session.
:::

Good fits: **RL rollouts**, **batch code grading**, **multi-tool agent
runs** — one sandbox per worker, many isolated tasks inside.

Not a fit: cross-language kernels (use `/code`), interactive REPLs (use
`/session`), or hard trust boundaries against kernel exploits (use gVisor /
Kata; see [Secure Container Runtime](/guides/secure-container)).

---

## How It Works

`execd` forks one `bwrap` child per session; `bwrap` sets up Linux
namespaces, then `exec`s a long-lived `bash` inside them.

![Isolation session runtime layout](../public/images/isolation-sessions-layout.svg)

Two things the diagram doesn't show:

- `bash` is long-lived, so `export X=1` in one `run` is visible to the next
  `run` **in the same session** (never in another session).
- The FS proxy reads and writes the merged workspace view from **outside**
  the namespace, so uploads and downloads work while `bash` is busy.

### `run` request flow

![Isolation session run flow](../public/images/isolation-sessions-run-flow.svg)

Non-happy paths:

| Event | What happens |
|---|---|
| Run `timeout_seconds` elapsed | execd cancels the run context, which sends `SIGINT` to the bwrap process group and emits an `IsolatedError` SSE event; the session itself stays alive. |
| `bwrap` process exited | The next `run` returns an `IsolatedError` SSE event with `session process has exited`; only `ErrContextNotFound` (session ID unknown) becomes an HTTP-level error. `DELETE` + recreate. |
| Idle timeout reached | GC runs the same teardown as `DELETE`. |
| Client disconnects mid-SSE | The Gin request context is cancelled and execd sends `SIGINT` to the running command; the run does **not** continue in the background. |
| Session teardown times out | `DELETE` returns `500 SESSION_TEARDOWN_TIMEOUT`. The manager must terminate the sandbox; operation admission is already revoked, so it must not retry with the same capability. |

---

## Quick Start

### curl

```bash
# Probe.
curl -s http://localhost:44772/v1/isolated/capabilities

# Create. In capability mode, this is the only response that contains the
# capability, so retain it without printing or logging it. Legacy mode and
# older execd versions omit the field.
CREATE_RESPONSE=$(curl -s -X POST http://localhost:44772/v1/isolated/session \
  -H "Content-Type: application/json" \
  -d '{
    "profile": "strict",
    "workspace": {"path": "/workspace", "mode": "overlay"},
    "share_net": false,
    "idle_timeout_seconds": 300
  }')
SESSION=$(jq -r .session_id <<<"$CREATE_RESPONSE")
CAPABILITY=$(jq -r '.capability // empty' <<<"$CREATE_RESPONSE")

# The array is empty in legacy mode and with older execd. In capability mode it
# adds the required header without putting the secret in the URL.
SESSION_AUTH=()
if [[ -n "$CAPABILITY" ]]; then
  SESSION_AUTH=(-H "X-OpenSandbox-Session-Capability: $CAPABILITY")
fi

# Run (SSE: stdout / error / complete).
curl -N -X POST "http://localhost:44772/v1/isolated/session/$SESSION/run" \
  "${SESSION_AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d '{"code": "export X=1; echo $X", "timeout_seconds": 30}'

# Second run reuses shell state.
curl -N -X POST "http://localhost:44772/v1/isolated/session/$SESSION/run" \
  "${SESSION_AUTH[@]}" \
  -H "Content-Type: application/json" \
  -d '{"code": "echo $X"}'  # prints 1

# Delete.
curl -X DELETE "http://localhost:44772/v1/isolated/session/$SESSION" \
  "${SESSION_AUTH[@]}"
```

::: warning Capability-aware SDK required
The SDK examples below assume execd's default `legacy` session auth mode.
Before enabling `capability` mode, use an SDK version that retains the
one-time create response capability and injects
`X-OpenSandbox-Session-Capability` into every session-scoped request. An SDK
that only stores a session ID cannot attach to or operate on a capability
session.
:::

### Python SDK

```python
from opensandbox import Sandbox
from opensandbox.models.isolated import (
    CreateIsolatedSessionRequest, IsolatedWorkspaceSpec, IsolatedRunOpts,
)

# Sandbox.create is an async classmethod, so await it before entering
# the async context manager.
async with (await Sandbox.create("python:3.11")) as sandbox:
    # One-shot.
    await sandbox.isolation.run_once(
        "python -c 'print(42)'", workspace="/workspace", profile="strict",
    )

    # Persistent session.
    async with sandbox.isolation.session(
        CreateIsolatedSessionRequest(
            workspace=IsolatedWorkspaceSpec(path="/workspace", mode="overlay"),
            profile="strict",
            idle_timeout_seconds=300,
        )
    ) as session:
        await session.run("export STAGE=train")
        await session.run("python train.py", opts=IsolatedRunOpts(timeout_seconds=600))
        await session.files.write_file("/workspace/data.csv", data_bytes)

    # Reattach after a client restart.
    handle = await sandbox.isolation.attach(known_session_id)
```

### JavaScript / TypeScript SDK

```ts
await sandbox.isolation.runOnce("node -e 'console.log(1)'", "/workspace", {
  profile: "strict",
});

await sandbox.isolation.withSession(
  { profile: "strict", workspace: { path: "/workspace", mode: "overlay" } },
  async (session) => { await session.run("npm test"); },
);
```

### Kotlin SDK

```kotlin
sandbox.isolation().runOnce(code = "python -c 'print(1)'", workspace = "/workspace")

sandbox.isolation().withSession(request) { session ->
    session.run("ls /workspace")
}
```

---

## Session Lifecycle

Under `/v1/isolated/` on execd. Use `X-EXECD-ACCESS-TOKEN` when execd has
an access token configured. Session capability authorization is independent
of the execd access token; when both are enabled, clients must send both
headers.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/session` | Create; returns `{session_id, created_at}` in `legacy` mode and adds a one-time `capability` in `capability` mode. The field remains optional for older execd compatibility. |
| `GET` | `/sessions` | List active sessions in `legacy` mode; returns `403 SESSION_LIST_FORBIDDEN` in `capability` mode. |
| `GET` | `/session/{id}` | Full state; echoes creation params so a client can rebuild the handle. Requires the session capability in `capability` mode. |
| `POST` | `/session/{id}/run` | SSE stream: `stdout` / `error` / `complete`. Runs on the same session are serialized. Requires the session capability in `capability` mode. |
| `DELETE` | `/session/{id}` | Destroy. Requires the session capability in `capability` mode. |
| `GET` | `/capabilities` | Probe the isolator and effective session auth mode. |

`idle_timeout_seconds > 0` destroys idle sessions automatically; set to `0`
to disable idle GC and always `DELETE` explicitly.

### Session authorization modes

execd selects one process-wide mode at startup:

| Mode | Behavior |
|---|---|
| `legacy` (default) | Preserves session-ID-only access, omits the create-response capability, and allows `GET /sessions` for compatibility. |
| `capability` | Returns a 256-bit URL-safe capability exactly once at session creation. Every route whose path contains `{sessionId}` — including run, delete, diff/commit, file, and directory APIs — requires exactly one `X-OpenSandbox-Session-Capability` header. `GET /sessions` is forbidden, and create currently requires explicit `share_net: false`. |

A missing, malformed, duplicated, or different session's capability returns
`403 SESSION_CAPABILITY_INVALID`. The same response is used for an unknown
session ID, so authorization failures do not reveal whether a session exists.
The capability is a bearer secret: do not put it in URLs, logs, environment
variables passed into user code, or sandbox metadata.

The create response is the only recovery point for the plaintext capability;
execd keeps only a digest. If the caller loses the successful create response,
it cannot recover or rotate the capability; idle cleanup or execd teardown
must eventually remove the inaccessible session.

If `DELETE` returns `500 SESSION_TEARDOWN_TIMEOUT`, the session workload may
still be alive but new operation admission has already been revoked. The
manager must terminate the whole sandbox. Retrying `DELETE` or another
session-scoped request with the same capability is not a recovery path.

---

## Workspace Modes

| Mode | Semantics |
|---|---|
| `rw` | Bind-mount read-write; writes persist on the host. |
| `overlay` (default) | Copy-on-write via overlayfs; writes go to a per-session upper dir and vanish on `DELETE`. |
| `ro` | Bind-mount read-only; writes fail with `EROFS`. |

Overlay upper dirs live under `upper_root` (default `/var/lib/execd/isolation`).

```json
{ "workspace": { "path": "/workspace", "mode": "overlay" } }
```

---

## Bind Mounts and Allowlist

- **`extra_writable`** — paths bind-mounted read-write at the same path
  (`source == destination`).
- **`binds`** — explicit `source` → `dest` mappings, optionally `readonly`.
  `source` must already exist; `dest` must already exist inside the
  namespace (bake it into the sandbox image).

```json
{
  "workspace": { "path": "/workspace", "mode": "rw" },
  "extra_writable": ["/data/scratch"],
  "binds": [
    { "source": "/data/in",  "dest": "/mnt/in",  "readonly": true },
    { "source": "/data/out", "dest": "/mnt/out" }
  ]
}
```

Every `source` is checked against the operator-configured `allowed_writable`
allowlist **after** symlink resolution, so symlinks cannot escape it. Default
allowlist: `/workspace`, `/mnt`, `/media`, `/data` (subpaths allowed). Empty
allowlist rejects all extra binds.

---

## Profiles and Defaults

The `profile` field currently controls only how `/tmp` is exposed inside the
namespace:

| Profile | `/tmp` |
|---|---|
| `strict` (default) | Private tmpfs (`--tmpfs /tmp`) |
| `balanced` | Shared with the sandbox (`--bind /tmp /tmp`) |

`workspace.mode` and `env_passthrough` are **independent** of the profile:
when they are omitted, execd normalizes `workspace.mode` to `overlay` and
`env_passthrough.mode` to `deny` regardless of which profile you pick. Set
those fields explicitly if you want persistent workspace writes (`"rw"`) or
host env passthrough (`"allow"`).

---

## Environment, UID, and Networking

- **`env_passthrough`** — `mode: "allow"` + `keys` whitelists host env vars;
  default `deny`. Per-run overrides go in `IsolatedRunRequest.envs`.
- **`uid` / `gid`** with **`uid_mode: "setpriv"`** (default, real
  setuid/setgid drop) or **`"userns"`** (user namespace remap). Check
  `setpriv_available` / `userns_available` from
  [`/capabilities`](#capabilities-and-probing) before requesting a mode.
- **`share_net`** controls the session network namespace:

  | Value | `legacy` mode | `capability` mode |
  |---|---|---|
  | omitted | Shares the sandbox network namespace. | Fails closed with `503 SESSION_NETWORK_BACKEND_UNAVAILABLE`; the guarded per-session default backend is not implemented yet. |
  | `true` | Shares the sandbox network namespace; sandbox-level egress and Credential Vault policies still apply. | Rejected with `400 SESSION_SHARED_NETWORK_FORBIDDEN`. |
  | `false` | Creates a loopback-only network namespace. | The only currently admitted setting; creates a loopback-only network namespace. |

Both capability-mode failures happen before workload or workspace side
effects. They must not fall back to the sandbox's shared network namespace.

---

## Filesystem Proxy

Reads and writes the session's **merged** workspace view from outside the
namespace — this is how SDKs `upload`, `download`, and `list` without
spawning a shell. All paths are per session under
`/v1/isolated/session/{id}/`:

| Method | Path |
|---|---|
| `GET` | `files/info?path=...` |
| `GET` | `files/download?path=...` (supports `Range` and `offset`/`limit`) |
| `POST` | `files/upload` (multipart: `metadata` + `file`) |
| `DELETE` | `files?path=...` |
| `POST` | `files/mv` |
| `POST` | `files/permissions` |
| `POST` | `files/replace?verbose=true` |
| `GET` | `files/search?path=...&pattern=...` |
| `GET` | `directories/list?path=...&depth=N` |
| `POST` | `directories` |
| `DELETE` | `directories?path=...` |

Writes on a `ro` workspace fail; writes on `overlay` land in the upper dir
and vanish on `DELETE`.

---

## Capabilities and Probing

```bash
curl -s http://localhost:44772/v1/isolated/capabilities
```

```json
{
  "available": true,
  "isolator": "bwrap",
  "version": "0.9.0",
  "setpriv_available": true,
  "userns_available": false,
  "commit_supported": false,
  "diff_supported": false,
  "session_auth_mode": "legacy",
  "session_capability_required": false
}
```

- `available: false` — bubblewrap is missing or the host can't create the
  required namespaces (missing `CAP_SYS_ADMIN`, restricted user-ns sysctl, etc.).
- `setpriv_available` / `userns_available` — whether sessions with
  `uid_mode: "setpriv"` or `"userns"` can be created. `setpriv_available`
  reflects only execd's **default** UID/GID; a session that requests a
  different UID/GID may still return `503 NOT_SUPPORTED` when identity
  switching is unavailable.
- `commit_supported` / `diff_supported` — Phase 2 stubs, currently return `503`.
- `session_auth_mode` — the effective `legacy` or `capability` mode.
- `session_capability_required` — `true` exactly when every session-ID-scoped
  API requires `X-OpenSandbox-Session-Capability`. Older execd builds omit
  both auth fields; clients must interpret absence as `legacy` / `false`.

---

## Server Configuration

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--isolation-config` | `EXECD_ISOLATION_CONFIG` | empty | Path to the optional isolation TOML file. |
| `--isolation-enabled` | `EXECD_ISOLATION_ENABLED` | `false` | Server-owned admission gate for secure isolated sessions. |
| `--isolated-session-auth-mode` | `EXECD_SESSION_AUTH_MODE` | `legacy` | Session authorization mode: `legacy` or `capability`. |
| `--access-token` | `EXECD_ACCESS_TOKEN` | empty | Shared execd API token sent as `X-EXECD-ACCESS-TOKEN`. |

Gate A permits exactly two configurations:

- Ordinary compatibility mode: `EXECD_ISOLATION_ENABLED=false` and
  `EXECD_SESSION_AUTH_MODE=legacy` (both defaults).
- Secure capability mode: `EXECD_ISOLATION_ENABLED=true`,
  `EXECD_SESSION_AUTH_MODE=capability`, and a non-empty canonical
  `EXECD_ACCESS_TOKEN` with no leading or trailing whitespace.

For example:

```bash
EXECD_ISOLATION_ENABLED=true \
EXECD_SESSION_AUTH_MODE=capability \
EXECD_ACCESS_TOKEN="$EXECD_TOKEN" \
execd
```

The mode is process-wide and cannot be selected per request or per session.
Roll out capability-aware clients first, then drain active sessions before
restarting execd with `capability`. Do not run capability mode with clients
that only retain session IDs. A mixed Gate A pair, an invalid mode, an empty
token, or a token padded with whitespace fails execd startup rather than
falling back to the ordinary path. When secure mode is enabled, callers must
send both `X-EXECD-ACCESS-TOKEN` and the per-session capability header on
session-ID-scoped requests. Session creation must also send
`"share_net": false`; omitted or shared-network requests fail closed with the
stable errors described under [Environment, UID, and Networking](#environment-uid-and-networking).

The optional TOML file controls namespace and workspace settings:

```toml
# Parent directory for per-session overlay upper dirs.
upper_root = "/var/lib/execd/isolation"

# Hard limit on total upper directory size across all sessions (bytes).
# Default: 8 GiB. Set to 0 only if you want to disable the quota entirely.
upper_max_bytes = 8589934592  # 8 GiB

# Sources allowed for extra_writable / binds (symlink-resolved).
# Default: ["/workspace", "/mnt", "/media", "/data"]. Empty = reject all.
allowed_writable = ["/workspace", "/mnt", "/media", "/data"]
```

Example: `components/execd/configs/isolation.example.toml`.

**Host requirements:** `bwrap` binary in the execd image; `CAP_SYS_ADMIN`
(and `kernel.unprivileged_userns_clone=1` for `uid_mode: "userns"`);
`overlayfs` in the kernel for `overlay` workspaces.

Note: `/capabilities` reports `available: false` only when bwrap itself
cannot be started at all (missing binary or missing namespace capabilities).
A missing `overlayfs` does **not** flip `available` — the overlay probe only
influences Phase 2 `commit`/`diff` support, and default overlay-mode session
creation can still fail at runtime on such hosts. If you rely on
`workspace.mode: "overlay"`, verify `overlayfs` support directly on the
host.

---

## Limitations

- **`diff` / `commit` are Phase 2 stubs**, currently return `503`. Tracked
  in [OSEP-0013](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0013-isolated-execution-api.md).
- **No hardware-level guarantee.** Namespaces + seccomp only; pair with a
  secure runtime for kernel-exploit defense.
- **Linux only.** Non-Linux builds return `available: false`.
- **Serialized runs per session** — create multiple sessions for parallelism.
- **Bind destinations must pre-exist** in the sandbox image.

---

## See Also

- [OSEP-0013 — Isolated Execution API](https://github.com/opensandbox-group/OpenSandbox/blob/main/oseps/0013-isolated-execution-api.md)
- [execd](/components/execd)
- [Secure Container Runtime](/guides/secure-container)
- [execd OpenAPI spec](/api/)
