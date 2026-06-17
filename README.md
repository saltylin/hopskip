# Hopskip

A local, single-operator agent for driving a personal multi-cloud fleet by typing
like a human. See [`CLAUDE.md`](./CLAUDE.md) for the full architecture and the
protocol specs in [`spec/`](./spec).

The whole app is **one Go binary**: it embeds the Vue shell (`web/` via `embed.FS`),
owns the tmux sessions, runs the agent loop, hosts the MCP tools, and reads/writes a
single SQLite file. No external runtime, no separate frontend build step.

## Requirements

- **Go** (a recent toolchain; the module auto-selects one via `go.mod`)
- **tmux** — the session backend (`brew install tmux`)

## Build & run

```sh
make build && ./hopskip      # or: make run   (see `make help` for all targets)
# → listening on http://127.0.0.1:8765
```

Open <http://127.0.0.1:8765>, add a provider token in **Settings** (or drop one in
`.env.local`), and start chatting. Step-by-step setup, install-to-PATH, and
troubleshooting are in [`INSTALL.md`](./INSTALL.md).

## What you can do

- **Chat to investigate the fleet.** The agent works through persistent terminal
  sessions, typing `ssh` to hop host-to-host exactly as you would and reading the
  screen between actions — no pre-declared topology. The live transcript streams
  in: assistant text, every command (as a tool call), every result, plus a live
  **"thinking…"** indicator with elapsed time and token usage.
- **Fleet topology + host detail.** Hosts are schema-less: free-form **tags** the
  operator asserts (`provider=aliyun`, `role=gpu`) and **facts** the model
  discovers. A `via` tag nests a host under its jump host; the **Connect** button
  types `ssh` hop-by-hop (never a `ProxyJump`-style declared route — invariant 1).
- **Browser terminal.** A real xterm.js terminal on a tmux session, streamed live:
  drag to select (copied to your clipboard via OSC 52), wheel to scroll deep
  history, bar cursor, configurable scrollback.
- **Operator knowledge + per-host notes.** Tell the agent a durable fact ("aliyun
  hosts can't reach GitHub") and it remembers it across chats; the fleet inventory
  and per-host notes are injected every turn, so a note like "run `mykinit` before
  ssh" is honored. A proxy-aware `fetch_url` downloads through the laptop for
  network-restricted hosts.
- **Self-programming UI.** The agent (or you) can author new pages, panels, and
  buttons **from chat**: it writes real ES modules into a DB-backed overlay and
  registers them, and a defensive loader mounts them on refresh (a broken
  extension is a contained error card, never a blank shell). The **Changes** tab
  shows what was authored vs the base shell (with line diffs) and a **Save to
  disk** that commits the overlay to disk. See
  [`spec/frontend-extension-protocol.md`](./spec/frontend-extension-protocol.md).
- **Settings** (all live, no restart): multi-provider LLM tokens (Anthropic +
  OpenAI, switchable; OpenAI "pro"/reasoning models auto-route to the Responses
  API), network proxy, max steps & retries, terminal scrollback, and the
  cloud-provider list.

## Configuration

**Operator-tunable config lives in the Settings UI** (stored in SQLite): provider
tokens, the network proxy, max steps & retries, terminal scrollback, the
cloud-provider list — all live, no restart. There is **no env fallback** for these.

Env vars are reserved for the few things that **can't** live in the DB — what the
process needs *before* it can open the DB and serve:

| Var | Default | Meaning |
|-----|---------|---------|
| `HOPSKIP_ADDR` | `127.0.0.1:8765` | listen address |
| `HOPSKIP_DB_PATH` | `hopskip.db` | SQLite file (inventory, chats, settings, **and the authored UI overlay**) |
| `HOPSKIP_TMUX_BIN` | `tmux` | tmux binary |
| `HOPSKIP_WEB_DIR` | _(unset)_ | optional disk overlay for hand-editing the UI (served *above* the DB overlay); unset ⇒ DB-overlay + embed only |

**Provider tokens** → **Settings → LLM tokens** (Anthropic + OpenAI, switchable).
Stored locally in SQLite, shown masked, and sent only as the provider auth header.
**Network proxy** → **Settings → General → Network proxy**; blank ⇒ a direct
connection (the default). Both take effect with no restart.

**Frontend overlay & serving order.** Static assets resolve **`HOPSKIP_WEB_DIR`
(dev) → SQLite `web_files` overlay (pending authored edits) → `web_export_dir`
(committed by *Save to disk*) → embedded base shell.** The base shell is always
fully functional on its own (single-binary guarantee); the overlays only add.

## Status

Everything in the suggested build order (CLAUDE.md §12) is implemented:

| Milestone | State |
|-----------|-------|
| tmux session core (`open`/`send_keys`/`read_screen`/`close`, done-sentinel) | ✅ |
| SQLite inventory (schema-less hosts/tags/facts) + inventory tools | ✅ |
| Real agent loop — **Anthropic (Opus 4.8) + OpenAI** (Chat + Responses API), switchable, streamed to the browser | ✅ |
| Browser terminal (live xterm.js over tmux), chat transcript + saved chats | ✅ |
| Settings (multi-provider tokens, proxy, steps/retries, scrollback, providers) — live, no restart | ✅ |
| Operator knowledge + per-host note injection; proxy-aware `fetch_url` | ✅ |
| Frontend extension runtime (self-programming UI): DB-backed overlay, authoring tools, defensive loader, **Changes** tab + Save-to-disk | ✅ |

Auditing is delegated to the operator's own network proxy (set in **Settings**,
blank ⇒ direct); the daemon routes every provider call through it (CLAUDE.md
invariant 4). A future
mutation-approval gate has one home — the single `dispatch()` choke point.
