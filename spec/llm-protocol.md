# Hopskip LLM Protocol Specification

> **Status:** Draft v1 — partly superseded by the implementation; see the
> amendment note below.
> **Authority:** `CLAUDE.md` is the source of truth for architecture and
> invariants. This file is the *precise definition* of how the daemon talks to an
> LLM provider: the canonical message/tool format, the provider adapters, the
> audit-proxy wire behavior, streaming, and the agent loop. It expands
> `CLAUDE.md` §4 (architecture protocol), §7 (audit proxy), and §8 (agent loop).
> Where this file conflicts with `CLAUDE.md`, `CLAUDE.md` wins.
>
> **AMENDED (as built).** The daemon does **not** run the internal audit proxy
> described here — auditing is delegated to the operator's own network proxy
> (the `upstream_proxy` **Setting**; blank ⇒ direct), through which every provider
> call is routed (`CLAUDE.md` invariant 4). Two providers ship, pluggable behind a `runner`
> interface: **Anthropic** (Messages API, adaptive thinking) and **OpenAI** —
> which speaks **Chat Completions** by default and transparently falls back to the
> **Responses API** for "pro"/reasoning models that reject chat completions. The
> browser-facing WebSocket stream additionally carries `thinking` (a per-call
> "model is working" marker) and `usage` (`{in, out}` token counts) events, on top
> of the token / `tool_use` / `tool_result` / `notice` / `error` / `done` events;
> these drive the chat view's live elapsed-time + token-usage indicator. Tokens
> live in SQLite (`llm_credentials`) and are sent only as the provider auth header.
> The canonical message/tool format and agent-loop shape below remain accurate.
> **Companion:** [`mcp-protocol.md`](./mcp-protocol.md) defines the tools that the
> model drives; this file defines how those tools are presented to the model and
> how their results are fed back.

---

## 1. Scope and the two hard rules

This protocol covers everything between the agent loop and the LLM API. Two
invariants shape all of it:

- **Invariant 4 — the daemon never calls the provider directly.** *All* LLM API
  traffic goes through the local audit proxy, which records every request and
  response append-only before forwarding. The API token lives only in the
  proxy's environment and never appears in audit bodies or in the browser.
- **Invariant 5 — all side effects flow through one `dispatch()` choke point**,
  so the future mutation-approval gate has exactly one home.

The daemon speaks **one internal canonical message/tool format** and adapts it
per provider (Anthropic or OpenAI) behind a small `Provider` interface
(`CLAUDE.md` §7, last bullet).

```
agent loop ──canonical──▶ Provider adapter ──provider JSON──▶ audit proxy ──▶ provider API
   ▲                                                              │ (tees + logs)
   └────────────── canonical response / stream deltas ───────────┘
```

---

## 2. Canonical representation (provider-agnostic)

The agent loop manipulates only these types. Adapters translate to/from provider
wire formats (§4). This canonical model is intentionally Anthropic-shaped (content
blocks), because that maps cleanly onto tool use in both providers.

### 2.1 Request envelope

```text
LLMRequest {
  model:        string                 // resolved per provider (§6)
  system:       string                 // the architecture protocol (§5)
  messages:     Message[]              // the running transcript (no system here)
  tools:        ToolDef[]              // from the MCP tool catalog (§5.2)
  max_tokens:   int
  temperature:  float?                 // optional
  stream:       bool                   // always true in the agent loop
  stop:         string[]?              // optional stop sequences
}
```

`system` is a top-level field, **not** a message (this matches Anthropic; the
OpenAI adapter materializes it as a leading system message — §4.2).

### 2.2 Messages and content blocks

```text
Message {
  role:    "user" | "assistant"
  content: ContentBlock[]
}

ContentBlock =
  | TextBlock       { type: "text",        text: string }
  | ToolUseBlock    { type: "tool_use",    id: string, name: string, input: object }
  | ToolResultBlock { type: "tool_result", tool_use_id: string,
                      content: string,     // JSON-encoded tool output, or error text
                      is_error: bool }
```

- An **assistant** turn may contain interleaved `text` and `tool_use` blocks.
- A **tool_result** is delivered back in a **user** message (Anthropic convention)
  whose `content` is one `tool_result` block per tool call from the prior
  assistant turn. The adapter remaps this for OpenAI (§4.2).
- `tool_use.id` ↔ `tool_result.tool_use_id` MUST match exactly so the provider can
  pair call and result.

### 2.3 Response

```text
LLMResponse {
  content:     ContentBlock[]          // text + tool_use blocks the model produced
  stop_reason: "end_turn" | "tool_use" | "max_tokens" | "stop_sequence"
  usage:       { input_tokens: int, output_tokens: int }
}
```

`stop_reason = "tool_use"` is the signal that the agent loop must execute tools
and continue. `"end_turn"` with only text blocks means the turn is finished.

### 2.4 Streaming deltas (loop → browser)

While a response streams, the adapter emits canonical deltas that the agent loop
forwards to the browser WebSocket and accumulates into the final `LLMResponse`:

```text
StreamEvent =
  | { kind: "text_delta",      text: string }
  | { kind: "tool_use_start",  id, name }
  | { kind: "tool_use_delta",  id, partial_json: string }   // streamed tool args
  | { kind: "tool_use_stop",   id }
  | { kind: "message_stop",    stop_reason, usage }
```

> The exact browser-facing WebSocket framing (chat in, tokens/tool-calls/results
> out) is the frontend transport (`CLAUDE.md` §9) and is **out of scope here**;
> this spec only defines the canonical deltas the loop has available to forward.

---

## 3. Tool definitions sent to the model

```text
ToolDef {
  name:         string        // e.g. "send_keys"
  description:  string        // model-facing guidance (see below)
  input_schema: object        // JSON Schema — identical to the MCP inputSchema
}
```

The `input_schema` for each tool is **byte-for-byte the MCP `inputSchema`** from
[`mcp-protocol.md`](./mcp-protocol.md) §5–§7. There is one schema definition;
both the MCP server and the LLM tool list serialize it.

Tool descriptions must teach the model the operationally critical behaviors,
especially:

- `send_keys`: include `\n` to press Enter; **use `await_done=false` for
  interactive prompts (host-key yes/no, password, sudo) and for long-running /
  streaming commands** — the done-sentinel can't appear until the foreground
  command exits (`mcp-protocol.md` §5.2, §6).
- `read_screen`: the way to **confirm which host you are on** before any mutating
  command, and to poll long output.

---

## 4. Provider adapters

```go
// Conceptual interface (CLAUDE.md §7). The agent loop depends only on this.
type Provider interface {
    Name() string                                  // "anthropic" | "openai"
    UpstreamBaseURL() string                       // real provider base; used to configure the proxy target
    BuildHTTPRequest(LLMRequest) (*http.Request, error)  // body shaped per provider; targets the PROXY addr, no auth header
    ParseStream(io.Reader, func(StreamEvent)) (LLMResponse, error)
}
```

Key point: `BuildHTTPRequest` targets the **proxy** (`HOPSKIP_PROXY_ADDR`), not
the provider, and **does not attach the API token** — the proxy injects it on the
outbound leg (§7, Invariant 4). `UpstreamBaseURL()` is what the proxy forwards to.

### 4.1 Anthropic adapter

- **Endpoint (upstream):** `POST {base}/v1/messages` (base default
  `https://api.anthropic.com`).
- **Headers set by adapter:** `content-type: application/json`,
  `anthropic-version: 2023-06-01`, `accept: text/event-stream`. **No** `x-api-key`
  (proxy adds it).
- **Body mapping:**

  | Canonical            | Anthropic body                                             |
  |----------------------|------------------------------------------------------------|
  | `system`             | top-level `system` (string)                                |
  | `messages`           | `messages: [{ role, content: [blocks] }]` (1:1)            |
  | `TextBlock`          | `{ "type": "text", "text" }`                               |
  | `ToolUseBlock`       | `{ "type": "tool_use", "id", "name", "input" }`            |
  | `ToolResultBlock`    | `{ "type": "tool_result", "tool_use_id", "content", "is_error" }` |
  | `tools[]`            | `{ "name", "description", "input_schema" }`                |
  | `max_tokens`/`stream`| same names                                                 |

- **Response → canonical:** `content[]` blocks map 1:1; `stop_reason` maps 1:1
  (`end_turn`/`tool_use`/`max_tokens`/`stop_sequence`); `usage.input_tokens` /
  `usage.output_tokens` map directly.
- **SSE events parsed:** `message_start`, `content_block_start`,
  `content_block_delta` (`text_delta` → `text_delta`; `input_json_delta` →
  `tool_use_delta`), `content_block_stop`, `message_delta` (carries final
  `stop_reason` + `usage`), `message_stop`.

### 4.2 OpenAI adapter

- **Endpoint (upstream):** `POST {base}/v1/chat/completions` (base default
  `https://api.openai.com`).
- **Headers set by adapter:** `content-type: application/json`,
  `accept: text/event-stream`. **No** `Authorization` (proxy adds the
  `Bearer` token).
- **Body mapping:**

  | Canonical                  | OpenAI body                                                                 |
  |----------------------------|-----------------------------------------------------------------------------|
  | `system`                   | leading `{ "role": "system", "content": <system> }` message                 |
  | `TextBlock` (user/asst)    | message `content` string                                                    |
  | `ToolUseBlock`             | assistant `tool_calls: [{ id, type:"function", function:{ name, arguments } }]` where `arguments` is `JSON.stringify(input)` |
  | `ToolResultBlock`          | `{ "role": "tool", "tool_call_id": <tool_use_id>, "content": <content> }`    |
  | `tools[]`                  | `{ "type":"function", "function": { name, description, parameters:<input_schema> } }` |
  | `max_tokens`/`stream`      | `max_tokens` / `stream: true` (request `stream_options:{include_usage:true}` so usage arrives in the stream) |

  Note the structural remap: Anthropic carries `tool_result` inside a *user*
  message's content; OpenAI carries it as a distinct top-level `role:"tool"`
  message. The adapter is responsible for this reshaping in both directions.

- **Response → canonical:** `choices[0].message.content` → `TextBlock`;
  `choices[0].message.tool_calls[]` → `ToolUseBlock` (parse `arguments` JSON into
  `input`); `finish_reason` maps `stop`→`end_turn`, `tool_calls`→`tool_use`,
  `length`→`max_tokens`. `usage.prompt_tokens`→`input_tokens`,
  `usage.completion_tokens`→`output_tokens`.
- **SSE events parsed:** `data:` lines carrying `choices[].delta` (`content` →
  `text_delta`; `tool_calls[].function.arguments` fragments → `tool_use_delta`,
  keyed by `tool_calls[].index`/`id`); terminal `data: [DONE]`. Final `usage`
  arrives in the last chunk when `include_usage` is set.

> The OpenAI Responses API is an acceptable alternative target behind the same
> interface, but Chat Completions is the v1 baseline for simplest request/response
> shaping.

---

## 5. System prompt & tool catalog assembly

For every agent run the loop assembles:

- `LLMRequest.system` = the **architecture protocol** verbatim (`CLAUDE.md` §4 —
  "ENVIRONMENT: hopskip …"). This is the model's description of the world it
  operates: the core terminal model, the *knowing-where-you-are* safety rule, the
  inventory model, and conduct rules. Keep it in sync with `CLAUDE.md` §4.
- `LLMRequest.tools` = the full MCP tool catalog (§3), terminal-driving +
  inventory.
- `LLMRequest.messages` = the running transcript (§8).

---

## 6. Configuration (env / flags, prefix `HOPSKIP_`)

| Variable               | Meaning                                                                 |
|------------------------|-------------------------------------------------------------------------|
| `HOPSKIP_LLM_PROVIDER` | `anthropic` \| `openai` — selects the adapter.                          |
| *(provider API key)*   | Read by the **proxy only**. Never logged, never sent to the browser.    |
| `HOPSKIP_PROXY_ADDR`   | Local address the daemon's LLM client targets (e.g. `127.0.0.1:8788`).  |
| `HOPSKIP_AUDIT_DIR`    | Append-only directory for full request/response body files.             |
| `max_steps` *(Setting)* | Agent-loop step cap (§8) — a DB Setting, not an env var (default 512). |
| `HOPSKIP_DB_PATH`      | SQLite file (audit rows + inventory).                                   |

The model id is resolved from configuration per provider; the canonical
`LLMRequest.model` carries the resolved id.

---

## 7. Audit proxy (log-only, streaming-aware, provider-agnostic)

The proxy is the **only** path to the provider (Invariant 4). Policy is
**log-only**: it records faithfully and forwards — it does **not** redact and does
**not** block. (A mutation gate is a *different* layer, at `dispatch()` — §8.)

### 7.1 Topology

Implement as a Go `httputil.ReverseProxy` (or an explicit forward proxy honoring
`HTTPS_PROXY`) whose target is the active provider's `UpstreamBaseURL()`. The
daemon's LLM client sends provider-shaped requests to `HOPSKIP_PROXY_ADDR`; the
proxy forwards them upstream.

### 7.2 Request leg

On receiving a request from the daemon, the proxy:

1. Reads a correlation header `X-Hopskip-Conversation: <conversation-id>` if
   present, then **strips it** (it must not reach the provider).
2. Persists the **full request body** to an append-only file under
   `HOPSKIP_AUDIT_DIR` (`body_path`).
3. Writes an `audit_log` row: `direction="request"`, `provider`, `conversation`,
   `body_path`, `meta` (model, ts).
4. **Injects the auth header** on the outbound leg only — `x-api-key` (Anthropic)
   or `Authorization: Bearer …` (OpenAI) — from the proxy's environment.
5. Forwards upstream.

> **Token hygiene (Invariant 4):** the auth header is added *after* logging and
> only on the outbound request. Audit body files contain **bodies, not headers**.
> Request headers carrying the token are never logged. The daemon never set the
> token in the first place.

### 7.3 Response leg — streaming tee

Provider responses are SSE. The proxy **tees** the stream:

- forward each chunk to the daemon (the agent loop) in real time, **and**
- accumulate the full raw response body in parallel.

On stream close, the proxy:

1. Persists the accumulated **full response body** to an append-only file
   (`body_path`).
2. Writes an `audit_log` row: `direction="response"`, with `meta` = HTTP status,
   latency_ms, token usage (parsed from the final SSE events when present —
   Anthropic `message_delta.usage`; OpenAI final-chunk `usage`), model, and the
   same `conversation` id.

A whole agent run is reconstructable by selecting `audit_log` rows for a
`conversation`, in `ts` order. (Schema: `CLAUDE.md` §6, `audit_log` table.)

### 7.4 Append-only discipline

Audit files and rows are **append-only and never mutated or deleted from code**
(Invariant 4 / `CLAUDE.md` §7). Body files live under `HOPSKIP_AUDIT_DIR`; the
`audit_log` row references them by `body_path`.

---

## 8. Agent loop

The loop is intentionally small (`CLAUDE.md` §8). One operator message in; a live
transcript out; tools dispatched through one choke point.

```text
conversation_id = new()
messages = []                                   # canonical Message[] (NOT incl. system)
messages.append(user(operator_message))

for step in 0 .. MAX_STEPS-1:                   # the max_steps Setting (default 512) guards runaway loops
    enforce_wall_clock_budget()                 # per-call budget; abort cleanly if exceeded

    resp = call_llm(LLMRequest{                 # via the audit proxy; X-Hopskip-Conversation set
        system: ARCHITECTURE_PROTOCOL,          # §5
        messages, tools: TOOL_CATALOG,
        stream: true, ...
    })
    # streaming deltas are forwarded to the browser as they arrive

    if resp.stop_reason != "tool_use":          # only text → done
        stream_final_text_to_browser(resp)
        break

    messages.append(assistant(resp.content))    # record the assistant turn (text + tool_use)

    tool_results = []
    for tool_use in resp.content where type == "tool_use"   # capped per turn (§8.1)
        result = dispatch(tool_use.name, tool_use.input)    # ◀── THE SINGLE CHOKE POINT
        emit_to_browser(tool_use, result)                   # "watch the commands" view
        tool_results.append(tool_result(tool_use.id, result))

    messages.append(user(tool_results))         # feed results back; loop continues
else:
    stream_notice_to_browser("step budget exhausted")   # MAX_STEPS reached
```

### 8.1 Rules

- **`MAX_STEPS`** (the `max_steps` **Setting**, default 512) and a **per-call wall-clock budget** are
  both required. A deep investigation can be 10+ steps and the transcript grows
  each step, so both bounds matter.
- **Per-turn tool-call cap:** a single assistant turn may request many tools; the
  loop caps how many it executes per turn (configurable). Excess tool calls are
  reported back as tool errors so the model can retry deliberately.
- **Stream everything to the browser:** assistant tokens, each `tool_use` (so the
  operator sees the *exact* keystrokes/commands), and each result. This live
  transcript **is** the product UI (`CLAUDE.md` §9).
- **`dispatch()` is the single side-effect choke point** (Invariant 5). It maps a
  tool name + input to the in-process MCP tool handler (PTY / SQLite), and is the
  one place the future **mutation-approval gate** will live — it can inspect the
  tool (e.g. `send_keys` with `destructive`/`openWorld` hints from
  `mcp-protocol.md` §4.3), pause for operator approval, then proceed or reject —
  **without reshaping the proxy or the loop.** Design `dispatch()` for that
  insertion now.

### 8.2 Error & retry semantics

- **Provider/transport errors** (the proxy returns non-2xx, or the upstream
  errors): surface to the loop. Transient errors (HTTP 429/503, timeouts) MAY be
  retried with backoff up to a small bound; persistent errors end the turn with a
  clear message streamed to the browser. Every attempt is still audited (§7).
- **Tool errors** come back from `dispatch()` as `tool_result` blocks with
  `is_error=true` (e.g. unknown session, host not found, command timeout). These
  are **fed back to the model**, not raised as loop failures — the model is
  expected to read them and adapt (open a new session, re-check the host, etc.).
- **Partial streams:** if a stream dies mid-response, the accumulated partial is
  audited, the turn is marked failed, and the loop ends the run with a notice.

---

## 9. Invariants honored (cross-check with `CLAUDE.md` §11)

4. **All LLM API traffic goes through the audit proxy.** The daemon's adapters
   target `HOPSKIP_PROXY_ADDR`, never the provider; the token is injected by the
   proxy on the outbound leg and never logged or sent to the browser; audit rows
   are append-only. ✔
5. **All side effects flow through `dispatch()`.** The loop has exactly one place
   that executes tools, ready to host the mutation gate. ✔
6. **One Go binary.** The proxy is owned by the same binary (or a sibling process
   it manages); no external service is required to run it. ✔
7. **Everything stays local.** Only chat content reaches the provider, and that
   path is audited on the way out; keys, SQLite, token, and inventory never leave
   the laptop. ✔

(Invariants 1–3 — typed-`ssh` reachability, screen-is-truth, schema-less
inventory — are enforced by the tool protocol; see
[`mcp-protocol.md`](./mcp-protocol.md).)
