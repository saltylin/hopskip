# Hopskip MCP Protocol Specification

> **Status:** Draft v1 — implementation target
> **Authority:** `CLAUDE.md` is the source of truth for architecture and
> invariants. This file is the *precise wire-level definition* of the MCP tool
> protocol summarized in `CLAUDE.md` §5. Where this file adds detail, it must
> stay consistent with `CLAUDE.md`; where it appears to conflict, `CLAUDE.md`
> wins and this file is a bug.
> **Companion:** [`llm-protocol.md`](./llm-protocol.md) defines how the agent
> loop talks to the LLM provider and how these same tools are presented to the
> model.

---

## 1. Scope and role

The Hopskip daemon embeds an **MCP server** ([Model Context Protocol]). It is the
*entire* interface between any model (the daemon's own agent loop, or an external
MCP client such as Claude Code) and the fleet. There are two tool families:

| Family            | Tools                                                                 | Backing store        |
|-------------------|-----------------------------------------------------------------------|----------------------|
| Terminal-driving  | `open_session`, `send_keys`, `read_screen`, `list_sessions`, `close_session` | tmux panes           |
| Inventory         | `list_hosts`, `get_host`, `add_host`, `rename_host`, `tag_host`, `forget_tag`, `record_fact`, `forget_fact` | SQLite (schema-less) |
| Knowledge + fetch | `remember`, `list_knowledge`, `forget_knowledge`, `fetch_url`, `http_request` | SQLite + laptop network (proxy-aware) |
| Frontend extension *(optional)* | `list_web_files`, `read_web_file`, `write_web_file`, `delete_web_file`, `register_extension`, `unregister_extension`, `list_extensions` | SQLite (`web_files` + `web_extensions`) |

> **Implemented names.** This table reflects the shipped tool names. (Earlier
> drafts used `write_extension_file`/`set_extension_enabled`/etc.; the build uses
> the `*_web_file` + `*_extension` names above. The authored overlay is
> SQLite-backed — see `frontend-extension-protocol.md` §2/§5, amended.)

The frontend-extension family lets the agent **author the platform's own UI** at
runtime (the self-programming surface); it is defined in
[`frontend-extension-protocol.md`](./frontend-extension-protocol.md) §8 and obeys
the same conventions (§4) and the single `dispatch()` choke point as every other
tool. It is optional — the core terminal-driving + inventory tools stand alone.

The model's *whole* mental model of "doing something on a host" is: **open a
session, type into it, read it, close it** — plus reading/writing inventory.
There is deliberately **no** `run_command(host, cmd)` tool and **no** tool that
opens a connection on the model's behalf (`CLAUDE.md` §1, Invariant 1).

[Model Context Protocol]: https://modelcontextprotocol.io

### 1.1 Protocol baseline

- **Wire framing:** JSON-RPC 2.0, per the MCP base protocol.
- **MCP revision targeted:** `2025-06-18` (advertise this in `initialize`; accept
  older clients down to `2025-03-26` and negotiate down if asked).
- **Tool semantics are identical across transports.** A tool behaves the same
  whether invoked over stdio or HTTP.

---

## 2. Transports

The daemon exposes the same tool set over two transports concurrently (§2 of
`CLAUDE.md`'s tech-stack table).

### 2.1 stdio

For external MCP clients (Claude Code, other agents) that launch `hopskip` as a
subprocess.

- JSON-RPC messages are exchanged as **newline-delimited JSON** over stdin
  (client→server) and stdout (server→client). One JSON-RPC message per line; no
  embedded raw newlines in a single message.
- **stdout carries protocol only.** All logging/diagnostics go to **stderr**.
  Writing non-protocol bytes to stdout corrupts the stream and is a bug.

### 2.2 HTTP (Streamable HTTP)

For the daemon's own agent loop and the browser-driven path. Implement the MCP
**Streamable HTTP** transport (the `2025-03-26`+ transport that supersedes the
legacy HTTP+SSE pair). A single HTTP endpoint (default path `/mcp`) handles all
JSON-RPC traffic:

- **Client → server:** HTTP `POST /mcp` with a single JSON-RPC request/notification
  as the body. `Accept: application/json, text/event-stream`.
  - If the server answers immediately, it replies `200` with a JSON-RPC response
    body (`Content-Type: application/json`).
  - If the server wants to stream (progress notifications, then the response), it
    replies `200` with `Content-Type: text/event-stream` and emits SSE `message`
    events, the last of which is the JSON-RPC response.
- **Server → client stream (optional):** HTTP `GET /mcp` opens a standalone SSE
  channel for server-initiated messages.
- **Session header:** the server MAY issue an `Mcp-Session-Id` on `initialize`;
  clients echo it on subsequent requests. (This is the *MCP* session — distinct
  from a Hopskip **terminal session** / tmux pane. Do not confuse them.)
- **Version header:** clients send `MCP-Protocol-Version: 2025-06-18` on every
  HTTP request after initialization.
- **Binding:** localhost only. Hopskip is single-operator and local (Invariant 7).

> Legacy compatibility: if a client speaks the old HTTP+SSE transport
> (`2024-11-05`), the server MAY support it, but Streamable HTTP is the default.

---

## 3. Lifecycle (both transports)

Standard MCP handshake:

1. Client → `initialize` with its `protocolVersion`, `capabilities`, `clientInfo`.
2. Server → result with negotiated `protocolVersion`, server `capabilities`
   (`tools: { listChanged: false }`, optionally `resources: {}`), `serverInfo`
   (`name: "hopskip"`, version string).
3. Client → `notifications/initialized`.
4. Normal operation: `tools/list`, `tools/call`, optional `resources/*`, `ping`.

The server's tool list is **static** for a process lifetime; `listChanged` is
`false` and the server does not emit `notifications/tools/list_changed`.

---

## 4. Common conventions

### 4.1 Two error layers — keep them distinct

| Layer            | Mechanism                                                        | Use for                                                                 |
|------------------|------------------------------------------------------------------|-------------------------------------------------------------------------|
| **Protocol error** | JSON-RPC `error` object (`code`, `message`)                     | Malformed request, unknown method, unknown tool name, schema-invalid args |
| **Tool error**     | A normal `tools/call` *result* with `isError: true`            | The tool ran but failed in a domain sense (unknown session, host not found, timeout that the caller should reason about) |

Rationale: a tool error is **information the model should see and act on** (e.g.
"session not found → open a new one"), so it must come back as a tool result, not
a transport failure. Reserve JSON-RPC errors for the call being unusable.

Relevant JSON-RPC codes: `-32700` parse, `-32600` invalid request, `-32601`
method/tool not found, `-32602` invalid params (schema validation failure),
`-32603` internal.

### 4.2 Tool result shape

Every `tools/call` result returns:

- `content`: an array with at least one block. Hopskip always includes a
  `{"type":"text", ...}` block whose text is the JSON-encoded structured output
  (human- and model-readable fallback).
- `structuredContent`: the structured object, validated against the tool's
  declared `outputSchema`.
- `isError`: boolean (omitted or `false` on success).

Clients that understand `outputSchema`/`structuredContent` should prefer it; the
text block is the compatibility fallback.

### 4.3 Tool annotations

Each tool declares MCP annotations so a UI / gate can reason about blast radius.
These are **hints**, not enforcement (the real enforcement point is the agent
loop's `dispatch()` gate — `llm-protocol.md` §7, Invariant 5).

| Tool            | `readOnlyHint` | `destructiveHint` | `idempotentHint` | `openWorldHint` |
|-----------------|:--------------:|:-----------------:|:----------------:|:---------------:|
| `open_session`  | false          | false             | false            | false           |
| `send_keys`     | false          | **true**          | false            | **true**        |
| `read_screen`   | **true**       | false             | true             | false           |
| `list_sessions` | **true**       | false             | true             | false           |
| `close_session` | false          | **true**          | true             | false           |
| `list_hosts`    | **true**       | false             | true             | false           |
| `get_host`      | **true**       | false             | true             | false           |
| `add_host`      | false          | false             | false            | false           |
| `tag_host`      | false          | false             | true             | false           |
| `record_fact`   | false          | false             | true             | false           |
| `forget_fact`   | false          | **true**          | true             | false           |

`send_keys` is `openWorld + destructive` because the keystrokes can run anything
on whatever host the session currently sits on — this is the tool the future
mutation gate watches most closely.

### 4.4 Common types

```text
SessionId   : string   — UUIDv4, e.g. "8f3c...".
SessionStatus : "open" | "closed"
HostId      : integer  — SQLite rowid.
Timestamp   : string   — RFC 3339 / ISO-8601 UTC, e.g. "2026-06-14T10:42:00Z".
```

`Host` object (returned by `list_hosts` and `get_host`):

```json
{
  "id": 7,
  "name": "bandwagon",
  "notes": "reach via aliyun; small KVM box",
  "tags":  { "provider": "bandwagon", "role": "web" },
  "facts": {
    "nginx_listening": { "value": "true", "observed_at": "2026-06-14T10:40:11Z" },
    "disk_free":       { "value": "12G",  "observed_at": "2026-06-13T22:01:09Z" }
  }
}
```

- `tags` — flat `key → value` map; **operator-asserted** (`host_tags`).
- `facts` — `key → { value, observed_at }` map; **model-discovered** (`host_facts`).
  Every fact carries its own observation timestamp; treat as possibly stale.

### 4.5 Host reference resolution (`host` parameter)

Inventory tools take `host` as "id-or-name". Resolution is deterministic:

1. If the string matches `^[0-9]+$`, interpret it as a `HostId` and look up by id.
2. Otherwise, look up by exact `name`.

Consequence: a host whose `name` is all digits is addressable only by id. This is
an accepted, documented edge — names are normally human words.

---

## 5. Terminal-driving tools

> **AMENDED (as built):** the terminal backend is now **daemon-owned PTYs**, not
> tmux (see `CLAUDE.md` §2). The tool *contracts* below are unchanged — only the
> implementation differs: a session is a shell on a `creack/pty` master; keystrokes
> are written to the PTY (`\n`→`\r`); `read_screen` serializes a server-side VT
> emulator (`hinshun/vt10x`) mirroring the PTY; `close` kills the shell. So
> "tmux pane / capture-pane / send-keys / kill-pane" in this section now read as
> "PTY shell / VT screen / PTY write / kill shell". Sessions no longer survive a
> daemon restart (the live map is in-process; on startup any still-`open` DB rows
> are marked closed).

These drive PTY shells. A **session** = one shell on a PTY running the operator's
default login shell, starting **on the laptop**. Reaching a remote host is always
the model typing `ssh …` into the session (Invariant 1). The session is the only
authoritative answer to "where am I"; the DB is a hint (Invariant 2).

### 5.1 `open_session`

Open a fresh shell session.

**Input schema**
```json
{
  "type": "object",
  "properties": {
    "label": { "type": "string", "description": "Optional human-readable label." }
  },
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "label":      { "type": "string" }
  },
  "required": ["session_id", "label"],
  "additionalProperties": false
}
```

**Behavior**
- Allocates a tmux pane named `hopskip-<short-id>` running the operator's default
  shell (`$SHELL`), starting on the laptop.
- Generates a `session_id` (UUIDv4) and inserts a `sessions` row
  (`status="open"`, `opened_at=now`, `tmux_name`, `label`).
- If `label` is omitted, the server assigns one (e.g. the short id).
- The session is **not** on any remote host yet. To get onto a host, the model
  sends `ssh …` via `send_keys`.

### 5.2 `send_keys`

Type keystrokes into a session, exactly as a human would.

**Input schema**
```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "keys":       { "type": "string",
                    "description": "Literal keystrokes. '\\n' presses Enter." },
    "await_done": { "type": "boolean", "default": true },
    "timeout_ms": { "type": "integer", "default": 30000, "minimum": 0 }
  },
  "required": ["session_id", "keys"],
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "screen":     { "type": "string",
                    "description": "Visible pane contents after the action." },
    "exit_code":  { "type": ["integer", "null"] },
    "completed":  { "type": "boolean" }
  },
  "required": ["session_id", "screen", "exit_code", "completed"],
  "additionalProperties": false
}
```

**Keystroke encoding**
- `keys` is sent literally to the pane; a `\n` (newline) byte is delivered as an
  **Enter** keypress. Implementation: literal segments via `tmux send-keys -l`,
  with each `\n` translated to an Enter key event. All other bytes (`;`, quotes,
  control chars the caller embeds) are passed through literally.

**Completion modes**

- **`await_done = true` (default):** the daemon wraps the command with a
  done-sentinel (§6), then polls the pane until the sentinel resolves or
  `timeout_ms` elapses.
  - Sentinel resolved → `completed=true`, `exit_code=<parsed code>`.
  - Timed out → `completed=false`, `exit_code=null`, current `screen` returned.
    The underlying command is **not** killed (the model may keep watching via
    `read_screen`). See §6.4.
  - `timeout_ms = 0` → send, take one immediate capture, return without polling.

- **`await_done = false`:** keystrokes are sent **verbatim, with no sentinel**;
  the call returns immediately with the current `screen`, `exit_code=null`,
  `completed=false`. Use this for:
  - interactive prompts the model must answer (host-key `yes/no`, password,
    sudo) — the sentinel can't appear until the foreground command exits;
  - long-running / streaming commands (`tail -f`, `top`);
  - multi-line input / heredocs (the sentinel-wrapping in §6 assumes `keys` is a
    single logical shell command line).

**Errors (tool-result `isError: true`)**: unknown `session_id`; session already
`closed`.

### 5.3 `read_screen`

Read a session's visible contents without typing. The primary way to **confirm
which host you are on** before a mutating command (Invariant 2), and to poll
long-running output.

**Input schema**
```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "lines":      { "type": "integer", "minimum": 1,
                    "description": "Return only the last N lines. Default: full visible pane." }
  },
  "required": ["session_id"],
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "screen":     { "type": "string" }
  },
  "required": ["session_id", "screen"],
  "additionalProperties": false
}
```

**Behavior**: captures the pane (`tmux capture-pane -p -J`, wrapped lines joined,
no escape sequences). With `lines`, returns the last `N` lines of the visible
pane. **Errors**: unknown `session_id`; closed session.

### 5.4 `list_sessions`

**Input schema**: `{ "type": "object", "properties": {}, "additionalProperties": false }`

**Output schema**
```json
{
  "type": "object",
  "properties": {
    "sessions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "session_id": { "type": "string" },
          "label":      { "type": "string" },
          "status":     { "type": "string", "enum": ["open", "closed"] },
          "opened_at":  { "type": "string" }
        },
        "required": ["session_id", "label", "status", "opened_at"],
        "additionalProperties": false
      }
    }
  },
  "required": ["sessions"],
  "additionalProperties": false
}
```

Lists all sessions the daemon knows about (open and recently closed, per the
`sessions` table). Order: most-recently-opened first.

### 5.5 `close_session`

Kill the tmux pane and mark the session closed.

**Input schema**
```json
{
  "type": "object",
  "properties": { "session_id": { "type": "string" } },
  "required": ["session_id"],
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "properties": {
    "session_id": { "type": "string" },
    "closed":     { "type": "boolean", "const": true }
  },
  "required": ["session_id", "closed"],
  "additionalProperties": false
}
```

**Behavior**: kills the pane (`tmux kill-pane`), sets `status="closed"`,
`closed_at=now`. **Idempotent**: closing an already-closed session returns
`closed: true` without error. Unknown `session_id` → tool error.

---

## 6. The done-sentinel (normative)

This is the mechanism that turns "is the command finished, and did it succeed?"
into a reliable signal, for `send_keys` with `await_done=true`. It is pure shell,
so it works identically no matter how many hosts deep the session is nested —
that is *why* the terminal model generalizes across hops for free (`CLAUDE.md`
§5.3).

### 6.1 Constants

```text
PREFIX  = "__HOPSKIP_DONE_"
SUFFIX  = "__"
nonce   = per-call random token, ≥ 8 hex chars from a CSPRNG (unique per call)
```

### 6.2 Command wrapping

Given the caller's `keys`, strip **exactly one** trailing `\n` (if present) to get
`CMD`, then send to the pane:

```
CMD; echo "__HOPSKIP_DONE_$?_<nonce>__"\n
```

`$?` expands to the exit status of `CMD` (the last command in a pipeline/list).
The trailing `\n` executes the line.

### 6.3 Completion detection

Poll the pane (`capture-pane`) on a short interval (e.g. 100 ms) until a line
matches, anchored, this regex:

```
__HOPSKIP_DONE_(\d+)_<nonce>__
```

- Capture group 1 = the exit code; parse as integer.
- **Critical safety point:** the *typed command line itself* is echoed by the
  terminal as `…; echo "__HOPSKIP_DONE_$?_<nonce>__"` — containing the literal
  `$?`, which is **not** `\d+`, so the regex does **not** match the echo. Only the
  resolved output line (with a real integer) matches. Never relax `(\d+)` to also
  accept `$?`.
- The per-call `nonce` guarantees a stale sentinel from a previous command can't
  be mistaken for this one. Coincidental appearance in command output is
  astronomically unlikely.

### 6.4 Outcomes

| Condition                              | `completed` | `exit_code`     | command killed? |
|----------------------------------------|:-----------:|-----------------|:---------------:|
| Sentinel line matched within timeout   | `true`      | parsed integer  | no              |
| `timeout_ms` elapsed, no match         | `false`     | `null`          | **no** (keep watching) |
| `await_done=false`                     | `false`     | `null`          | n/a (no sentinel sent) |

### 6.5 Screen normalization (SHOULD)

Before returning `screen`, the daemon SHOULD remove sentinel artifacts so the
model sees clean output:

- drop the resolved sentinel **output** line, and
- drop the appended `; echo "__HOPSKIP_DONE_…__"` suffix from the echoed command
  line.

This is cosmetic; if omitted, the model will still see the sentinel and must
ignore it. Normalization MUST NOT alter any other pane content.

### 6.6 Limits

`timeout_ms` MAY be clamped to a server-configured hard ceiling (it must not
exceed the agent loop's per-call wall-clock budget — `llm-protocol.md` §7). The
wrapping in §6.2 assumes `CMD` is a single logical shell line; for multi-line or
interactive input the caller must use `await_done=false`.

---

## 7. Inventory tools

These touch **SQLite only**. They never open a connection — reaching a host is
always the model typing `ssh` into a session (Invariant 1). The store is
**schema-less key/value** (Invariant 3): capabilities are *discovered facts* or
*asserted tags*, never typed columns. Schema in `CLAUDE.md` §6.

### 7.1 `list_hosts`

**Input schema**
```json
{
  "type": "object",
  "properties": {
    "tag": { "type": "string",
             "pattern": "^[^=]+=.*$",
             "description": "Optional filter 'key=value'. Returns hosts whose tags include it." }
  },
  "additionalProperties": false
}
```

**Output schema**
```json
{
  "type": "object",
  "properties": {
    "hosts": { "type": "array", "items": { "$ref": "#/$defs/Host" } }
  },
  "required": ["hosts"],
  "additionalProperties": false,
  "$defs": { "Host": { "$comment": "see §4.4 Host object" } }
}
```

With `tag="key=value"`, return only hosts whose `host_tags` contains that exact
key/value pair. Without `tag`, return all hosts.

### 7.2 `get_host`

**Input**: `{ "host": "<id-or-name>" }` (resolution per §4.5).
**Output**: a single `Host` object (§4.4), with full `tags` + `facts`.
**Errors**: host not found → tool error.

### 7.3 `add_host`

**Input schema**
```json
{
  "type": "object",
  "properties": {
    "name":  { "type": "string", "minLength": 1 },
    "notes": { "type": "string", "description": "Free text, incl. how to reach it." }
  },
  "required": ["name"],
  "additionalProperties": false
}
```
**Output**: `{ "id": <HostId> }`.
**Errors**: duplicate `name` (UNIQUE) → tool error.

### 7.4 `tag_host`  (operator-asserted)

**Input**: `{ "host": "<id-or-name>", "key": "<string>", "value": "<string>" }`.
**Output**: `{ "ok": true }`.
Upserts into `host_tags` (PK `(host_id, key)` → setting an existing key replaces
its value). Host not found → tool error.

### 7.5 `record_fact`  (model-discovered)

**Input**: `{ "host": "<id-or-name>", "key": "<string>", "value": "<string>" }`.
**Output**: `{ "ok": true }`.
Upserts into `host_facts` and **sets `observed_at = now`**. This is how the model
writes down what it learned by looking at a host.

### 7.6 `forget_fact`

**Input**: `{ "host": "<id-or-name>", "key": "<string>" }`.
**Output**: `{ "ok": true }`.
Deletes the `(host_id, key)` fact. Deleting a missing fact is **idempotent** →
`{ "ok": true }` (no error).

---

## 8. Resources (optional)

The daemon MAY expose MCP resources to give a model read-only context:

- `hopskip://architecture` — the architecture protocol (`CLAUDE.md` §4),
  `text/plain`. Lets an external MCP client load the same world-description the
  daemon's own agent loop injects as a system prompt.

If resources are exposed, advertise `resources: {}` in `initialize` and support
`resources/list` + `resources/read`. Resources are not required for v1; the
architecture protocol can equally be delivered purely as the agent-loop system
prompt (`llm-protocol.md` §5).

---

## 9. Concurrency & lifecycle

- **Per-session serialization:** `send_keys` and `read_screen` on the *same*
  session are serialized by a per-session lock (you cannot type two things into
  one pane at once). Operations on *different* sessions run concurrently.
- **Pane naming:** each session owns a uniquely named tmux pane
  (`hopskip-<short-id>`); the `tmux_name` is recorded in the `sessions` row.
- **Crash recovery:** on startup the daemon reconciles the `sessions` table
  against live tmux panes; sessions whose panes no longer exist are marked
  `closed`.
- **`host_id` on a session is a weak hint only** (where it *started* targeting).
  The authoritative current host is always the live screen (Invariant 2). Never
  return a host-identity claim derived from `host_id`.

---

## 10. Invariants honored (cross-check with `CLAUDE.md` §11)

1. No tool establishes a connection. `ssh` is only ever typed via `send_keys`.
   There is no `ProxyJump`-style declarative path anywhere in this protocol. ✔
2. Screen is truth for host identity; `read_screen` exists precisely for the
   pre-mutation host check; `host_id` is documented as a hint. ✔
3. Inventory is schema-less key/value (`tags`, `facts`); no typed capability
   columns. ✔
5. Tool annotations + the single dispatch point (in the agent loop) keep the
   future mutation gate to one home; this protocol only *describes* blast radius,
   it does not gate. ✔

(Invariants 4, 6, 7 are about the audit proxy, single-binary packaging, and
locality — see [`llm-protocol.md`](./llm-protocol.md) and `CLAUDE.md`.)
