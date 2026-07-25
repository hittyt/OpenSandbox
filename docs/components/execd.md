---
title: execd
description: The in-sandbox execution daemon providing HTTP APIs for code execution, shell commands, filesystem operations, PTY sessions, and metrics.
---

# execd - OpenSandbox Execution Daemon

`execd` is the runtime daemon used inside OpenSandbox sandboxes.

It is built on Gin and exposes HTTP APIs for code execution, shell commands, filesystem operations, PTY sessions, and metrics.

## Quick Start

### 1) Build

```bash
cd components/execd
make build
```

### 2) Start Jupyter Server

```bash
./tests/jupyter.sh
```

### 3) Run execd

```bash
./bin/execd \
  --jupyter-host=http://127.0.0.1:54321 \
  --jupyter-token=your-jupyter-token \
  --port=44772
```

### 4) Verify

```bash
curl -v http://localhost:44772/ping
```

## API

- OpenAPI spec: [execd-api.yaml](/api/)
- Common capability groups:
  - Code execution (`/code`, SSE stream)
  - Session and command execution (`/session`, `/command`)
  - Filesystem operations (`/files`, `/directories`)
  - Isolated sessions (`/v1/isolated/session`, bubblewrap namespaces)
  - PTY over WebSocket (`/pty`)
  - Local metrics endpoints (`/metrics`, `/metrics/watch`)

Shell-backed sessions use Bash when it is available and fall back to `sh` on
minimal images that do not include Bash. This applies to PTY sessions, the
Bash session API (which keeps its existing name for compatibility), and
isolated sessions. Commands submitted to a fallback session must use syntax
supported by that image's `sh` implementation.

## Isolated Sessions

Isolated sessions run a shell inside a per-execution
[bubblewrap](https://github.com/containers/bubblewrap) (`bwrap`) namespace,
created via `POST /v1/isolated/session`. Bash is preferred, with `sh` used as a
fallback. Beyond the workspace, callers can expose additional host paths into
the namespace.

### UID modes and capabilities

The optional `uid_mode` request field selects how identity is established:

- `setpriv` (the default) uses the container's existing user namespace and
  drops to the requested UID/GID with `setpriv`.
- `userns` creates a new user namespace and maps the requested UID/GID inside
  it, which can work in environments where the capabilities required by
  `setpriv` mode are unavailable.

At startup, execd probes both modes independently. `GET
/v1/isolated/capabilities` reports `setpriv_available` and
`userns_available`; the overall `available` field is true when either mode is
usable. Creating a session returns `503 NOT_SUPPORTED` only when the selected
mode is unavailable. The probes exercise the same identity path used at
runtime: the public `setpriv_available` flag covers execd's default UID/GID
path (so a root session that keeps UID/GID 0 does not require the `setpriv`
binary), while `userns` applies the UID/GID mapping and the setuid-aware
`--disable-userns` policy. A setpriv request that selects IDs different from
execd's own is checked against a separate startup identity-switch probe and
returns `503 NOT_SUPPORTED` before session side effects when that switch is not
available.

### Bind mounts

Two request fields control extra host paths:

- `extra_writable`: a list of paths bind-mounted read-write at the same path
  inside the namespace (`source == destination`).
- `binds`: explicit `source` → `dest` mappings, each optionally read-only.
  - `source` (required): host path to bind. It must **already exist** and is
    resolved (symlinks followed) before use.
  - `dest`: mount destination inside the namespace; defaults to `source` when
    omitted. It must be an **existing** mount point — `bwrap` cannot create a
    destination under the read-only root, so create the directory first.
  - `readonly` (default `false`): mount read-only (`--ro-bind`) when `true`,
    read-write (`--bind`) otherwise.

Example:

```json
{
  "workspace": { "path": "/workspace", "mode": "rw" },
  "binds": [
    { "source": "/data/in",  "dest": "/mnt/in", "readonly": true },
    { "source": "/data/out", "dest": "/mnt/out" }
  ]
}
```

### Writable allowlist

The source path of every `extra_writable` entry and every `binds` entry must
fall within the `allowed_writable` allowlist (see the isolation config file
below). The allowlist is enforced against the fully symlink-resolved real
path, so a symlink cannot redirect a bind outside the allowlist. An empty
allowlist rejects all `extra_writable`/`binds` requests.

The built-in default allowlist is `/workspace`, `/mnt`, `/media`, `/data`
(subpaths included). Set `allowed_writable` in the isolation config to
override it.

### Session authorization

Isolated-session authorization is selected once at execd startup:

- `legacy` (default) preserves the existing session-ID-only contract and
  allows `GET /v1/isolated/sessions`. Session creation omits the capability
  field so existing SDKs keep their legacy response shape.
- `capability` returns a 256-bit URL-safe `capability` exactly once from
  `POST /v1/isolated/session`. Every later request whose path contains a
  session ID must include exactly one
  `X-OpenSandbox-Session-Capability` header. Listing sessions is deliberately
  disabled and returns `403 SESSION_LIST_FORBIDDEN`.

In capability mode, a missing, malformed, duplicated, or wrong capability
returns `403 SESSION_CAPABILITY_INVALID` without revealing whether the session
ID exists. execd stores only a digest, so the plaintext capability cannot be
retrieved after creation. Treat it as a bearer secret and never include it in
URLs, logs, user-process environment variables, or sandbox metadata.

`GET /v1/isolated/capabilities` reports the effective
`session_auth_mode` and `session_capability_required`. Older execd builds omit
these fields; clients must treat absence as `legacy` and `false`.

Secure isolated sessions use a fail-closed startup gate. Operators must set
`EXECD_ISOLATION_ENABLED=true` (or `--isolation-enabled=true`) together with
`EXECD_SESSION_AUTH_MODE=capability` and configure a non-empty
`EXECD_ACCESS_TOKEN` without leading or trailing whitespace. Setting only one
side of the gate, omitting the token, or padding it with whitespace fails
startup. The default `false` + `legacy` pair preserves ordinary sandbox
behavior.

The current capability-mode network gate accepts only an explicit
`"share_net": false` create request, which produces a loopback-only session
network namespace. Omitting `share_net` returns
`503 SESSION_NETWORK_BACKEND_UNAVAILABLE` until the guarded per-session
default network backend is implemented. Setting it to `true` returns
`400 SESSION_SHARED_NETWORK_FORBIDDEN`. Both fail before session side effects;
secure admission never falls back to the sandbox's shared network namespace.

If `DELETE /v1/isolated/session/{sessionId}` returns
`500 SESSION_TEARDOWN_TIMEOUT`, operation admission has already been revoked
but the workload may still be alive. The manager must terminate the sandbox
and must not retry with the same capability.

## Configuration

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--jupyter-host` | `""` | Jupyter server URL reachable by execd. |
| `--jupyter-token` | `""` | Jupyter token for HTTP/WebSocket auth. |
| `--port` | `44772` | HTTP listen port. |
| `--log-level` | `6` | Log level (0=Emergency, 7=Debug). |
| `--access-token` | `""` | Optional shared API access token. |
| `--graceful-shutdown-timeout` | `1s` | SSE tail-drain wait window before closing. |
| `--jupyter-idle-poll-interval` | `100ms` | Poll interval after Jupyter reports idle. |
| `--isolation-config` | `""` | Path to the isolation TOML config (see below). |
| `--isolation-enabled` | `false` | Require fail-closed secure isolated-session startup; must be paired with capability auth. |
| `--isolated-session-auth-mode` | `legacy` | Isolated-session authorization mode: `legacy` or `capability`. |

### Environment Variables

| Variable | Description |
|---|---|
| `JUPYTER_HOST` | Same as `--jupyter-host` (overridden by explicit flag). |
| `JUPYTER_TOKEN` | Same as `--jupyter-token` (overridden by explicit flag). |
| `EXECD_ACCESS_TOKEN` | Same as `--access-token` (overridden by explicit flag). |
| `EXECD_API_GRACE_SHUTDOWN` | Same as `--graceful-shutdown-timeout`. |
| `EXECD_JUPYTER_IDLE_POLL_INTERVAL` | Same as `--jupyter-idle-poll-interval`. |
| `EXECD_ISOLATION_CONFIG` | Same as `--isolation-config`. |
| `EXECD_ISOLATION_ENABLED` | Same as `--isolation-enabled`; default `false`. |
| `EXECD_SESSION_AUTH_MODE` | Same as `--isolated-session-auth-mode`; default `legacy`. |
| `EXECD_CLONE3_COMPAT` | Linux clone3 compatibility switch (see below). |
| `EXECD_LOG_FILE` | Optional log output file path; default is stdout. |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | Preferred OTLP metrics endpoint. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | Fallback OTLP endpoint when metrics-specific endpoint is unset. |
| `OPENSANDBOX_ID` | Optional `sandbox_id` metric/resource attribute. |
| `OPENSANDBOX_EXECD_METRICS_EXTRA_ATTRS` | Optional extra metric attrs (`k=v,k2=v2`). |

### Isolation Config File

Isolated sessions read an optional TOML file given by `--isolation-config`
(or `EXECD_ISOLATION_CONFIG`). All fields are optional; omitted fields use
built-in defaults.

The auth mode is not part of this TOML file. Set it with
`--isolated-session-auth-mode=capability` or
`EXECD_SESSION_AUTH_MODE=capability`, together with the isolation-enabled gate
and access token described above. Invalid or mismatched values fail startup.
Before changing the process-wide mode, deploy capability-aware clients and
drain active sessions; there is no mixed-mode fallback.

```toml
# Parent directory for per-session overlay upper directories.
upper_root = "/var/lib/execd/isolation"

# Host paths callers may request via extra_writable / binds.
# Enforced against the fully symlink-resolved real path; subpaths are allowed.
# Default: ["/workspace", "/mnt", "/media", "/data"]. Empty = reject all.
allowed_writable = ["/workspace", "/mnt", "/media", "/data"]
```

## Observability

### OpenTelemetry Metrics

OTLP metrics export is enabled when either endpoint is set:

- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`
- `OTEL_EXPORTER_OTLP_ENDPOINT`

### Local Metrics Endpoints

- `GET /metrics`: point-in-time host metrics snapshot
- `GET /metrics/watch`: SSE stream (1s cadence)

## Linux clone3 Compatibility

Some sandbox environments fail on `clone3(2)`.
Set `EXECD_CLONE3_COMPAT` in sandbox env to force fallback behavior:

- `1` / `true` / `yes` / `on`: enable seccomp fallback
- `reexec`: enable fallback and re-exec binary

## License

`execd` is part of OpenSandbox. See the [LICENSE](https://github.com/opensandbox-group/OpenSandbox/blob/main/LICENSE).
