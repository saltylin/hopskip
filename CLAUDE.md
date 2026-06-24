# CLAUDE.md — Hopskip

This file orients an LLM coding agent (Claude Code or similar) working on
**Hopskip**. Read it fully before writing code. It is the source of truth for
architecture, the wire protocols, and the invariants that must not be violated.

The name reflects the core behavior: reaching a host is a series of hops the
agent types itself (`ssh aliyun`, then `ssh bandwagon`), exactly as a human
would — never a pre-declared route. Binary, module, and CLI are all `hopskip`.

---

## 1. What this project is

A **local, single-operator agent for driving a personal multi-cloud fleet by
typing like a human.** The operator owns scattered cloud hosts across providers
(Aliyun, BandwagonHost, GCP, etc.). Some are reachable only by hopping through
another host (`ssh aliyun`, then from aliyun `ssh bandwagon`). The operator
wants to open a browser, type "check why my web server is down", and watch an
LLM investigate and propose fixes across those hosts — issuing the same
keystrokes the operator would issue by hand.

Everything runs on the operator's own laptop. The operator's SSH keys, the
SQLite database, the API token, and the host inventory never leave the machine.
The only thing that goes to a third party is the chat content sent to the LLM
API — and that path is wrapped in an audit proxy.

### Why not just an SSH-exec tool?

This is the central design decision; do not "simplify" it away.

The common pattern (and most existing SSH-MCP servers) is structured exec:
`run_command(host, cmd)` with declarative connection config and `ProxyJump` for
multi-hop. **We deliberately reject that.** The operator wants the LLM to behave
like a human at a terminal: open a shell, type `ssh aliyun`, read the screen,
see the prompt, type `ssh bandwagon`, read again, then work. Multi-hop is not a
declared topology — it **emerges** from the LLM typing into a persistent
session. This is what lets the same mechanism handle:

- arbitrary hop depth (a → b → c → d) with zero new config,
- interactive prompts (host-key confirmation, password, sudo) that the LLM
  *sees on screen and responds to*,
- future gateways (e.g. an HTTP/JWT SSH gate) that have a CLI — the LLM just
  types the CLI command, same as a person would.

The inventory is **schema-less on purpose** (see §6). There is no fixed resource
model like Kubernetes. A host "is a GPU box" because a discovered fact says so,
not because a schema field declares it.

---

## 2. Tech stack (fixed — do not substitute)

| Layer            | Technology                                      |
|------------------|-------------------------------------------------|
| Daemon / backend | Go                                              |
| Terminal backend | **Daemon-owned PTYs** — one shell per session on a `creack/pty` master; the browser's xterm.js attaches directly so it owns native selection + scrollback. A server-side VT emulator (`hinshun/vt10x`) mirrors each PTY for the agent's `read_screen`. *(Amended from the original tmux backend — the operator chose direct PTYs to get true local-terminal selection/scroll; tmux's pane rendering is incompatible with xterm.js owning the mouse/scrollback. Sessions no longer survive a daemon restart.)* |
| Storage          | SQLite (single local file)                      |
| Frontend         | Vue (SPA)                                        |
| Asset delivery   | Go `embed.FS` base shell (single binary), overlaid by a **SQLite-backed** authored overlay (`web_files`), with an optional disk overlay (`HOPSKIP_WEB_DIR`) for dev. Resolution: disk → DB → embed (see `spec/frontend-extension-protocol.md`) |
| LLM transport    | HTTP/HTTPS through a local audit proxy (see §7) |
| Tool protocol    | MCP server (stdio + HTTP), terminal-driving tools (see §5) |
| Browser ↔ daemon | WebSocket (bidirectional; daemon pushes tokens/tool-calls, browser pushes chat + future approvals) |

The deliverable is a **single Go binary**: it embeds the Vue frontend via
`embed.FS`, serves it, runs the agent loop, hosts the MCP server, owns the PTY
sessions, and reads/writes the SQLite file. No external runtime, no container
required to run it. (No tmux dependency anymore — the terminal backend is
in-process PTYs; see §2.)

---

## 3. Component map

```
┌──────────────────────────── operator's laptop ────────────────────────────┐
│                                                                            │
│  Browser (Vue SPA, served from embed.FS)                                   │
│     │  WebSocket: chat in, tokens + tool-calls + host dashboard out        │
│     ▼                                                                      │
│  Go daemon                                                                 │
│     ├── HTTP server      serves embedded Vue assets + WS endpoint          │
│     ├── Agent loop       calls LLM API, dispatches tool_use, feeds results │
│     ├── MCP server       exposes terminal-driving + inventory tools        │
│     ├── PTY manager      one shell+PTY per session; write keys / VT mirror │
│     ├── SQLite store     hosts, tags, facts, sessions, audit refs          │
│     └── Audit proxy ─────► LLM API (Anthropic or OpenAI)                    │
│                            (log-only; records every request + response)    │
│                                                                            │
│  ~/.ssh/{config,keys}   used by the PTY shells exactly as a human's would  │
└────────────────────────────────────────────────────────────────────────────┘
                                     │
                          ssh (typed into a session)
                                     ▼
                    aliyun ──(ssh, typed again)──► bandwagon ──► ...
```

The agent loop is the heart: browser sends a message → daemon calls the LLM
through the audit proxy → LLM returns `tool_use` blocks → daemon executes them
(PTY writes, VT-screen reads, SQLite queries) → daemon appends `tool_result`
blocks → calls again → repeats until the LLM returns only text → stream that to
the browser. Stop conditions and a per-turn tool-call cap are in §8.

---

## 4. The architecture protocol (LLM-readable system description)

This block is intended to be injected into the LLM's system prompt (or an MCP
resource) so the model understands the world it operates in. Keep it in sync
with the code. It is written for the model, not the human.

```text
ENVIRONMENT: hopskip

You operate a personal fleet of remote hosts on behalf of one operator, from
their laptop. You act like a human at a terminal.

CORE MODEL
- You work through persistent terminal SESSIONS. A session is a live shell
  (a PTY shell) that retains state between your actions — current directory,
  environment, and crucially WHICH HOST YOU ARE ON.
- To reach a host that is only accessible via another host, you SSH in steps,
  exactly as a person would: open a session, type `ssh <intermediate>`, read
  the screen, confirm the prompt, then type `ssh <final>`. There is no
  pre-declared topology. The path is whatever you type.
- After every keystroke batch, READ THE SCREEN before deciding the next action.
  Output is asynchronous: a command may still be running, may be waiting for
  input (password, yes/no), or may have finished. Never assume; look.

KNOWING WHERE YOU ARE  (most important safety rule)
- A session can be nested several hosts deep. Before running ANY command that
  writes, deletes, restarts, kills, or installs, you MUST confirm which host
  the session is currently on — e.g. by reading the shell prompt or running a
  harmless identifying command (`hostname`, `whoami`). State the host in your
  reasoning before the mutating command. A destructive command on the wrong
  host is the worst failure in this system.

INVENTORY
- Hosts are described by free-form TAGS (what the operator asserts: role=gpu,
  provider=bandwagon) and FACTS (what gets discovered by looking: gpu=A100,
  disk_free=12G, nginx_listening=true). There is no fixed schema.
- You discover facts by opening a session on a host and running read-only
  commands, then recording what you learn. Treat facts as possibly stale;
  re-check when it matters.
- Notes on a host may tell you how to reach it (e.g. "reach via aliyun").
  Read the note, then type your way there. The inventory does NOT build the
  connection for you.

CONDUCT
- Diagnose freely with read-only commands (status, logs, ps, df, cat, grep).
- For commands that change system state, explain what you intend to run, on
  which host, and why, before running it. (A future version may require human
  approval for these; write your reasoning as if it will be reviewed.)
- Prefer key-based auth. If you must type a secret, know it may be captured in
  the session screen; minimize exposure.
- When done, summarize: what was wrong, what you observed, what you changed (if
  anything), and what you recommend the operator do next.
```

---

## 5. Tool protocol (MCP)

The daemon is an **MCP server**. Use the official Go MCP SDK / `mcp-go`-style
implementation. Expose tools over both stdio (for Claude Code / external MCP
clients) and an HTTP/SSE transport (for the daemon's own agent loop and the
browser path). Tool semantics are identical across transports.

The tools are **terminal-driving**, not structured-exec. The model's whole
interface to the fleet is: open a session, type into it, read it, close it —
plus inventory tools.

### 5.1 Terminal-driving tools

**`open_session`**
Open a new shell session (a fresh PTY running the operator's default
shell on the laptop).
- Input: `{ "label": string? }`  — optional human-readable label
- Output: `{ "session_id": string, "label": string }`
- The session starts on the **laptop**. To get onto a remote host, the model
  types `ssh ...` via `send_keys`.

**`send_keys`**
Type text into a session, exactly as keystrokes. Include `\n` to press Enter.
- Input:
  ```json
  {
    "session_id": "string",
    "keys": "string",
    "await_done": "bool (default true)",
    "timeout_ms": "int (default 30000)"
  }
  ```
- Behavior: appends a **done-sentinel** to detect completion and capture the
  exit code (see §5.3). When `await_done` is true, the daemon blocks until the
  sentinel appears or `timeout_ms` elapses, then returns the captured output.
  When false (for interactive cases like typing a password mid-prompt, or
  starting a long-running `tail -f`), it returns immediately and the model must
  `read_screen` itself.
- Output:
  ```json
  {
    "session_id": "string",
    "screen": "string (visible pane contents after the action)",
    "exit_code": "int | null (null if no sentinel resolved, e.g. await_done=false or still running)",
    "completed": "bool (true if sentinel resolved within timeout)"
  }
  ```

**`read_screen`**
Read the current visible contents of a session without typing anything. Used to
poll long-running commands, see an interactive prompt, or check the shell prompt
to confirm the current host.
- Input: `{ "session_id": "string", "lines": "int? (default: full visible pane)" }`
- Output: `{ "session_id": "string", "screen": "string" }`

**`list_sessions`**
- Input: `{}`
- Output: `{ "sessions": [ { "session_id", "label", "status", "opened_at" } ] }`

**`close_session`**
Kill the PTY shell and mark the session closed.
- Input: `{ "session_id": "string" }`
- Output: `{ "session_id": "string", "closed": true }`

### 5.2 Inventory tools

**`list_hosts`** — `{ "tag": "key=value"? }` → `{ "hosts": [ {id,name,notes,tags,facts} ] }`
**`get_host`** — `{ "host": "id-or-name" }` → full host record incl. tags + facts
**`add_host`** — `{ "name", "notes"? }` → `{ "id" }`
**`rename_host`** — `{ "host", "name" }` → `{ "ok": true }`  (changes the display name)
**`tag_host`** — `{ "host", "key", "value" }` → `{ "ok": true }`  (operator-asserted)
**`forget_tag`** — `{ "host", "key" }` → `{ "ok": true }`
**`record_fact`** — `{ "host", "key", "value" }` → `{ "ok": true }`  (model-discovered; sets observed_at = now)
**`forget_fact`** — `{ "host", "key" }` → `{ "ok": true }`

Inventory tools touch SQLite only. They never open connections — reaching a
host is always the model typing `ssh` into a session.

### 5.2a Operator-knowledge + fetch tools

**`remember`** — `{ "text" }` → `{ "id" }`  (durable fact/instruction; injected into
every future chat's system prompt, alongside a per-turn fleet inventory so host
notes — e.g. "run `mykinit` before ssh" — are always in context)
**`list_knowledge`** — `{}` → remembered facts with ids
**`forget_knowledge`** — `{ "id" }` → `{ "ok": true }`
**`fetch_url`** — `{ "url", "save_as"? }` → downloads an http(s) URL **through the
laptop** (and its proxy), saving it ON THE LAPTOP; to put it on a host the model
copies it FROM a laptop session (never from the host). For network-restricted hosts.
**`http_request`** — `{ "url", "method"?, "headers"?, "body"?, "save_as"? }` → makes
an arbitrary HTTP request **through the laptop and its configured proxy** (the
"don't shell out to curl" tool), returning `{ status, headers, body }`. Body inline
when textual+small, else saved on the laptop. Request auth headers stay local and
are not echoed back.

### 5.2b Frontend self-programming tools

The agent can author its own UI from chat (see `spec/frontend-extension-protocol.md`).
All writes go through `dispatch()`; a defensive loader contains any bad write.

**`list_web_files`** — `{}` → authored overlay files + the embedded base-shell paths
**`read_web_file`** — `{ "path" }` → a file's source (overlay first, then embed base)
**`write_web_file`** — `{ "path", "content", "content_type"? }` → upserts a DB-overlay asset
**`delete_web_file`** — `{ "path" }` → reverts that path to the base shell
**`register_extension`** — `{ "slug", "kind", "mount", "entry", "title"?, "sdk_range"?, "notes"? }`
(`kind`: `slot`|`route`|`override`) → loads the module on refresh
**`unregister_extension`** — `{ "slug" }` → removes a manifest entry
**`list_extensions`** — `{}` → the registered manifest

### 5.3 The done-sentinel (implementation detail the daemon owns)

To turn the fuzzy "is the command finished and did it succeed?" question into a
reliable signal, `send_keys` (when `await_done=true`) wraps the typed command:

```
<keys (trailing \n stripped)>; echo "__HOPSKIP_DONE_$?_<nonce>__"
```

then polls `capture-pane` until a line matching `__HOPSKIP_DONE_<code>_<nonce>__`
appears. Parse `<code>` as the exit code. The nonce is per-call (random) so
stale sentinels from earlier commands can't be mistaken for the current one.

This works identically regardless of how deep the session is nested, because it
is just shell running on whatever host the session currently sits on. That is
the whole reason the terminal model generalizes across hops for free.

Edge cases the daemon must handle:
- The typed input is itself interactive (e.g. `ssh` asking yes/no, or a password
  prompt). The sentinel will not appear until the foreground command exits, so
  these calls should use `await_done=false` and let the model drive via
  `read_screen` + `send_keys`. Document this clearly to the model in tool
  descriptions.
- Long-running / streaming commands (`tail -f`): `await_done=false`.
- Timeout: return `completed=false`, current screen, `exit_code=null`. Do NOT
  kill the command automatically (the model may want to keep watching).

---

## 6. Data model (SQLite, schema-less by intent)

The point is that capabilities are **discovered**, not declared. Do not add
typed columns per capability. Keep it key/value.

```sql
CREATE TABLE hosts (
  id          INTEGER PRIMARY KEY,
  name        TEXT UNIQUE NOT NULL,
  notes       TEXT,                       -- free text, incl. how to reach it
  created_at  TEXT NOT NULL
);

CREATE TABLE host_tags (                  -- operator asserts these
  host_id  INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  key      TEXT NOT NULL,
  value    TEXT NOT NULL,
  PRIMARY KEY (host_id, key)
);

CREATE TABLE host_facts (                 -- the model discovers these
  host_id     INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
  key         TEXT NOT NULL,
  value       TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  PRIMARY KEY (host_id, key)
);

CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,            -- session_id (uuid)
  host_id    INTEGER REFERENCES hosts(id),-- best-effort: where it STARTED targeting; may be null
  tmux_name  TEXT NOT NULL,               -- legacy column name; now holds the PTY identity (e.g. "pty:<pid>")
  label      TEXT,
  status     TEXT NOT NULL,               -- open | closed
  opened_at  TEXT NOT NULL,
  closed_at  TEXT
);

CREATE TABLE audit_log (                  -- see §7; one row per LLM API call
  id           INTEGER PRIMARY KEY,
  ts           TEXT NOT NULL,
  direction    TEXT NOT NULL,             -- request | response
  provider     TEXT NOT NULL,             -- anthropic | openai
  conversation TEXT,                      -- conversation/session correlation id
  body_path    TEXT NOT NULL,             -- path to the full body on disk (append-only)
  meta         TEXT                       -- json: status, token counts, model, etc.
);

CREATE TABLE llm_credentials (            -- provider API tokens, managed in the UI
  id          INTEGER PRIMARY KEY,
  label       TEXT NOT NULL,
  provider    TEXT NOT NULL,              -- anthropic | openai
  token       TEXT NOT NULL,              -- stays local; only sent as the provider auth header
  model       TEXT,                       -- default model for this credential
  active      INTEGER NOT NULL DEFAULT 0, -- exactly one active; drives new chats
  created_at  TEXT NOT NULL
);

CREATE TABLE chats (                      -- saved conversations (managed in the UI)
  id          TEXT PRIMARY KEY,
  title       TEXT NOT NULL DEFAULT '',    -- derived from the first user message
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE chat_messages (              -- persisted transcript (mirrors the UI lines)
  id          INTEGER PRIMARY KEY,
  chat_id     TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  role        TEXT NOT NULL,              -- user | assistant | tool | tool-result | notice | error
  text        TEXT NOT NULL,
  created_at  TEXT NOT NULL
);

CREATE TABLE app_settings (               -- key/value app settings (e.g. upstream_proxy)
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE web_files (                  -- LLM/operator-authored frontend overlay
  path         TEXT PRIMARY KEY,          -- normalized web path (no leading slash, no '..')
  content      BLOB NOT NULL,
  content_type TEXT,
  created_by   TEXT NOT NULL DEFAULT 'agent',
  updated_at   TEXT NOT NULL
);

CREATE TABLE web_extensions (             -- frontend manifest the shell's loader consumes
  id           INTEGER PRIMARY KEY,
  slug         TEXT UNIQUE NOT NULL,
  kind         TEXT NOT NULL,             -- route | slot | override
  mount        TEXT NOT NULL,             -- route path | slot name | overridden region
  entry        TEXT NOT NULL,             -- module URL, e.g. '/ext/connect/entry.mjs'
  title        TEXT,
  sdk_range    TEXT,
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_by   TEXT NOT NULL,             -- operator | agent  (provenance of self-written UI)
  created_at   TEXT NOT NULL,
  updated_at   TEXT,
  content_hash TEXT,                      -- sha256 of the entry module at register time
  notes        TEXT
);
```

The frontend is **self-programming**: the agent (or operator) authors UI by
writing `web_files` (DB-backed overlay, served above the embedded base shell via
`write_web_file`) and registering `web_extensions` (`register_extension`). The
authored overlay lives in **SQLite, not on disk** — so the whole authored UI
migrates with the `.db` and authoring is a scoped upsert with no filesystem path
to traverse. Static serving resolves **disk (`HOPSKIP_WEB_DIR`, optional/dev) →
DB overlay → embed.FS base**. The shell's loader is defensive: a broken
extension is a contained error card, never a blank shell. See
`spec/frontend-extension-protocol.md` (§2/§5 amended: overlay is SQLite-backed).

Chat transcripts are persisted for listing/viewing/deleting; the live agent
context (the provider runner's native message history) is kept in memory per
chat id for continuity across reconnects within a daemon run.

`host_id` on `sessions` is a weak hint only. The authoritative "where is this
session now" answer is always reading the live screen — a session can SSH
elsewhere at any time. Never trust the DB over the screen for host identity.

---

## 7. Audit proxy (log-only)

> **Current deployment choice:** auditing is delegated to the operator's own
> network proxy (the `upstream_proxy` Setting; blank ⇒ direct). The daemon routes all provider calls
> through it and does **not** run the internal audit proxy described below. This
> section remains the spec for the internal-proxy alternative (used when there is
> no auditing network proxy in front of the daemon).

**Requirement: every byte exchanged with the LLM API passes through a local
HTTP/HTTPS proxy that records the full request and response, append-only, before
forwarding.** This is for audit, not filtering. The chosen policy is **log-only**:
the proxy does not redact and does not block — it records faithfully and passes
through.

Design:

- The daemon does NOT call the LLM API directly. It is configured to send all
  LLM API traffic to a local proxy endpoint owned by the same binary (or a
  sibling process). Implement as a Go `httputil.ReverseProxy` (or an explicit
  forward proxy honoring `HTTPS_PROXY`) targeting the provider base URL.
- On each call, write two `audit_log` rows (request, response) and persist the
  full bodies to an append-only location on disk (`body_path`). Never mutate or
  delete audit files from code.
- Record metadata in `meta`: model, HTTP status, latency, token usage if the
  provider returns it, and the conversation correlation id so a whole agent run
  can be reconstructed.
- Streaming: the provider responses are SSE. The proxy must tee the stream —
  forward chunks to the agent loop in real time AND accumulate the full body for
  the audit record written on stream close.
- The API token lives only in the daemon/proxy environment. It is never sent to
  the browser and never written into the audit body files (it's a header the
  proxy adds on the outbound leg; do not log request headers containing it).
- Provider-agnostic: support an Anthropic target and an OpenAI target behind a
  small interface (`Provider` with `BaseURL`, request/response shaping). The
  agent loop speaks one internal message/tool format and adapts per provider.

Why log-only and not a gate: keep the first version simple and faithful. A
mutation-approval gate is a planned future layer (it belongs at the tool-dispatch
boundary in the agent loop, not in the network proxy). Design the tool dispatcher
so a gate can be inserted later without reshaping the proxy.

---

## 8. Agent loop (daemon)

Pseudocode — the loop is intentionally small:

```
messages = [ system(architecture_protocol §4), user(operator_message) ]
model    = selected_model_id or active_credential.model   # from the WS message / Settings
for step in 0..MAX_STEPS:                 # MAX_STEPS guards runaway loops
    resp = call_llm(model, messages)      # via audit proxy; stream tokens to browser
    if resp has only text:
        stream final text to browser; break
    for each tool_use in resp:
        result = dispatch(tool_use)        # PTY / sqlite; this is the future gate point
        emit tool_use + result to browser  # the "watch the commands" view
        append tool_result to messages
    append assistant(resp) + tool_results to messages
```

Rules:
- `MAX_STEPS` and a per-call wall-clock budget are required; a deep
  investigation can be 10+ steps and the history grows each step.
- The model id comes from the operator's selection per message (§9), not a
  constant. Pass it through; default to the **active credential's model**
  (per-provider code default: `claude-opus-4-8` / `gpt-4o`) when none is supplied.
- Opus 4.8 API surface: adaptive thinking only — do NOT send `temperature`,
  `top_p`, `top_k`, or `budget_tokens` (removed for this generation). `effort`
  is supported and defaults to high; only send it if the operator picked a
  level. Keep the provider request-shaping behind the `Provider` interface so
  an OpenAI model selected in the UI maps to the right request shape.
- Stream everything to the browser over the WebSocket: assistant tokens, each
  `tool_use` (so the operator sees the exact keystrokes/commands), and each
  result. This live transcript IS the product UI.
- `dispatch()` is the single choke point for all side effects. Keep it that way
  so the future mutation-approval gate has exactly one place to live.

---

## 9. Frontend (Vue, embedded)

- Vue SPA, built to static assets, embedded in the Go binary via `embed.FS` and
  served by the daemon's HTTP server. The running deliverable is one binary.
- **Runtime-extensible.** The embedded SPA is also a *host* for extensions: the
  operator — or the agent itself — can add new pages, panels, and buttons as code
  (real ES modules) served from an optional on-disk `HOPSKIP_WEB_DIR` overlay
  (resolved disk-first, falling back to `embed.FS`). New UI appears on a page
  refresh — no recompile. Extensions link the shell's Vue + a Hopskip SDK via an
  import map; their only side-effecting primitive (`jobs.run`) starts a normal
  agent run, so all actions still flow through `dispatch()` and the audit proxy.
  The full design — overlay serving, manifest, SDK, the agent's
  extension-authoring tools, and the security model (CSP `connect-src 'self'`, no
  token in the browser) — is in `spec/frontend-extension-protocol.md`.
- Two views, one page:
  1. **Chat / transcript** — operator types a request; the live agent transcript
     streams in: assistant text, every command issued (rendered as tool calls),
     every result. This is where "check why my web server is down" goes.
  2. **Fleet dashboard** — lists hosts with their tags and last-known facts
     (with `observed_at`), and live sessions. Reads the same SQLite the daemon
     writes.
- The live browser terminal streams a session's PTY directly into an xterm.js
  view (the operator watches/drives the same shell the agent does). xterm.js owns
  native selection + scrollback (the PTY is a plain byte stream — no tmux).
- **Model selector.** A control in the chat view lets the operator pick the
  model per conversation (and switch mid-thread). Default is `claude-opus-4-8`.
  Offer at least: `claude-opus-4-8` (default, for real work) and a cheap model
  (e.g. Haiku) for debugging the loop, where model quality is irrelevant to
  testing the PTY/sentinel/dispatch plumbing. The chosen model id is sent on
  the WebSocket with each chat message and passed straight into the agent
  loop's API call; the daemon does NOT hardcode the model — it uses the
  selected id, falling back to the active credential's model if none is sent.
  Optionally also expose the effort level (Opus 4.8 defaults to high; low is
  faster/cheaper per step).
- Transport: one WebSocket. Browser → daemon: chat messages with the selected
  model id (and, later, approve/reject for the mutation gate). Daemon →
  browser: streamed tokens, tool-call events, result events, host/session
  updates.

---

## 10. Build & run

- Build: compile the Vue app, embed via `embed.FS`, `go build` → single binary
  named `hopskip`.
- Config via env: env vars are reserved for what the process needs **before it can
  open the DB and serve** — there is **no env fallback for operator config**. The
  only env vars are `HOPSKIP_ADDR` (listen addr), `HOPSKIP_DB_PATH` (SQLite),
  `HOPSKIP_TMUX_BIN`, and `HOPSKIP_WEB_DIR` (optional on-disk dev overlay; unset ⇒
  DB-overlay + embed only).
- **Operator-tunable config lives in Settings, not env vars.** The network proxy
  (`upstream_proxy`, blank ⇒ direct — there is **no** built-in `127.0.0.1:8080`
  default), `max_steps`, `max_retries`, `terminal_scrollback`, `web_export_dir`,
  the cloud-provider list, and **provider tokens** (`llm_credentials`) are all set
  in the **Settings** UI and stored in SQLite (`app_settings` / `llm_credentials`).
  There is no `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` import and no `.env.local`
  loading — tokens are added in Settings. Add a new setting by appending one entry
  to the server-side settings registry (`internal/server/settings.go`) and reading
  the stored value where it is consumed — the UI renders it generically. Do not add
  new env-only knobs for operator config.
- `hopskip`, on start: opens/creates the SQLite file, starts the MCP server
  (stdio + HTTP), starts the HTTP/WS server, and is ready.

### 10.1 Credentials (static key)

The LLM API key is a **static key** — chosen for simplicity (one auth header, no
token refresh, no OAuth flow). It is **managed in the Settings UI and stored in
SQLite** (`llm_credentials`), never in the environment.

- The key comes from the provider console (e.g. console.anthropic.com →
  Settings → API Keys → Create Key). API access is billed per token, independent
  of any Pro/Max plan. Set a spend limit before testing — the agent loop resends
  growing history each step, so a deep run is many calls.
- Tokens are added/selected in **Settings → LLM tokens** (Anthropic + OpenAI,
  multiple stored, one active). The active credential's token is what the provider
  runner attaches as the outbound auth header. There is **no** `ANTHROPIC_API_KEY`/
  `OPENAI_API_KEY` env import and **no** `.env.local` loading.
- The token is never sent over the WebSocket to the browser (shown masked in the
  UI), never committed, and never placed in any prompt — it leaves only as the
  provider auth header, through the operator's network proxy (invariant #4).

---

## 11. Invariants (do not violate)

1. **Reaching a host is always the model typing `ssh` into a session.** No tool
   ever establishes a connection on the model's behalf. No `ProxyJump`-style
   declarative topology in the connection path. (A `via` tag may name a host's
   gateway — used to nest the topology and to drive the Connect button's
   *hop-by-hop typed* ssh sequence: `ssh <gateway>`, then from there `ssh
   <target>`. That is still typing ssh step by step in the session, not a
   `ProxyJump`/declarative connection mechanism.)
2. **Screen is truth for host identity.** Confirm current host by reading the
   screen before any mutating command. The DB's `host_id` is a hint, never
   authoritative.
3. **Inventory is key/value and schema-less.** Capabilities are discovered facts
   or asserted tags — never typed columns.
4. **All LLM API traffic is audited.** Auditing is delegated to the operator's
   own network proxy (the `upstream_proxy` **Setting**; blank ⇒ direct), through
   which the daemon routes every provider call — so the daemon no longer runs an
   internal audit proxy. API tokens are stored locally (SQLite, the
   `llm_credentials` table), settable per-provider from the UI, and only ever
   leave as the provider auth header. (An internal log-only proxy, §7, remains a
   valid alternative when no auditing network proxy is present.)
5. **All side effects flow through the single `dispatch()` choke point**, so the
   future mutation-approval gate has one home.
6. **The deliverable is one Go binary** with the default Vue shell embedded via
   `embed.FS`, fully functional standalone. No mandatory external services to run
   it. `HOPSKIP_WEB_DIR` is an *optional, additive* runtime overlay (resolved
   disk-first, embed-fallback) — never required (see
   `spec/frontend-extension-protocol.md`).
7. **Everything stays local.** Keys, SQLite, token, inventory never leave the
   laptop; only chat content reaches the LLM API (and is audited on the way).
   This holds even for runtime frontend extensions: a CSP `connect-src 'self'`
   confines arbitrary extension code to the local daemon, and the API token never
   reaches the browser. Extension side effects go only through `dispatch()` +
   the audit proxy (via the SDK's `jobs.run`), never a new privileged path.

---

## 12. Build order (suggested)

Each milestone is independently useful; ship them in order.

1. **PTY MCP core** — `open_session` / `send_keys` / `read_screen` /
   `close_session` with the done-sentinel (sessions are daemon-owned PTYs; a VT
   emulator mirrors each for `read_screen`). At this point an external MCP client
   can already do multi-hop by typing.
2. **SQLite inventory + inventory tools** — schema-less hosts/tags/facts.
3. **Audit proxy** — log-only, streaming-aware, provider-agnostic.
4. **Agent loop + WebSocket** — daemon owns the loop, streams transcript.
5. **Vue frontend embedded via `embed.FS`** — chat transcript + fleet dashboard.
6. **Frontend extension runtime** — `HOPSKIP_WEB_DIR` overlay serving, extension
   manifest + defensive loader, import-map SDK (`jobs.run` over the WS), and the
   agent's extension-authoring MCP tools. This is the self-programming milestone:
   the operator (or the agent) adds pages/buttons as code, live on refresh. See
   `spec/frontend-extension-protocol.md`.

Later (not v1): mutation-approval gate at `dispatch()`; in-browser live terminal
view; future HTTP/JWT SSH-gateway hosts (handled by the model typing the gate's
CLI — no protocol change needed).
