# Installing hopskip

hopskip is a **single Go binary**: it embeds the Vue frontend (`web/` via
`embed.FS`), owns the PTY sessions, runs the agent loop, and uses one local
SQLite file. There is no separate frontend build and nothing to run alongside it.

> This is a **local, single-operator** tool — run it on your own laptop. Your SSH
> keys, the SQLite database, and your provider token never leave the machine; only
> chat content goes to the LLM API (through your network proxy).

---

## 1. Prerequisites

| Need | Why | Install (macOS) | Install (Debian/Ubuntu) |
|------|-----|-----------------|--------------------------|
| **Go** (recent toolchain) | builds the binary; `go.mod` auto-selects the version | `brew install go` | `sudo apt install golang` (or [go.dev/dl](https://go.dev/dl/)) |

That's it — sessions run on daemon-owned PTYs using your `$SHELL` (else
`/bin/bash`/`/bin/sh`). **No tmux required.** Check Go:

```sh
go version     # e.g. go1.25.x
```

You'll also want an **LLM API key** to actually drive the fleet from chat — an
**Anthropic** key (console.anthropic.com → API Keys) and/or an **OpenAI** key. API
access is billed per token and is separate from any chat subscription; set a spend
limit in the provider console before testing (a deep agent run is many calls).

---

## 2. Get the code

```sh
git clone <your-hopskip-remote> hopskip
cd hopskip
```

(or just `cd` into the existing checkout.)

---

## 3. Build & run

Using the Makefile (see `make help` for all targets):

```sh
make build      # -> ./hopskip
make run        # build, then run on http://127.0.0.1:8765
```

Or plain Go:

```sh
go build -o hopskip .
./hopskip
# → hopskip: listening on http://127.0.0.1:8765
```

Open <http://127.0.0.1:8765>.

The first run creates `hopskip.db` (inventory, chats, settings, and the authored
UI overlay) in the working directory, so run it from a stable folder.

---

## 4. First-run setup (add a provider token)

Open **Settings → LLM tokens**, pick a provider (Anthropic or OpenAI), paste the
token, and mark it active — switchable any time; the active one drives new chats.
Tokens are stored locally in SQLite, shown masked, and sent only as the provider
auth header. (There is no env-var / `.env.local` import — tokens are Settings-only.)

Then add a host in the sidebar and chat — e.g. *"check why my web server is down."*

---

## 5. Install onto your PATH (optional)

```sh
make install               # -> /usr/local/bin/hopskip   (may need: sudo make install)
make install PREFIX=$HOME/.local   # -> ~/.local/bin/hopskip   (no sudo)
make uninstall             # remove it
```

After installing, run `hopskip` from any directory (it writes its `hopskip.db`,
`downloads/`, and `web_overlay/` into the **current** directory).

---

## 6. Configuration

**Operator-tunable config lives in the Settings UI** (stored in SQLite) — provider
tokens, the network proxy, max steps/retries, terminal scrollback, the
cloud-provider list. There is no env fallback for these. The few env vars are
reserved for what the process needs *before* it can open the DB:

```sh
HOPSKIP_ADDR=127.0.0.1:8765      # listen address
HOPSKIP_DB_PATH=hopskip.db       # SQLite file
HOPSKIP_WEB_DIR=                 # optional disk overlay for hand-editing the UI (dev)
```

> **Network proxy:** if your provider calls must go through an auditing proxy, set
> it in **Settings → General → Network proxy** (blank ⇒ direct, which is the
> default — there is no longer a built-in `127.0.0.1:8080`).

---

## 7. Develop

```sh
make check     # go vet + go test ./...
make test
make fmt
```

The frontend is plain static assets under `web/public/` (vendored Vue, no bundler).
Editing them needs a rebuild to re-embed (`make build`) — or point `HOPSKIP_WEB_DIR`
at a disk folder to iterate without rebuilding (it's served above the embedded
shell). The agent can also author UI at runtime from chat (the **Changes** tab and
`spec/frontend-extension-protocol.md`).

---

## 8. Troubleshooting

- **`no shell found`** — set `$SHELL` to a usable shell (sessions run on PTYs).
- **`go build` hangs/fails fetching modules** (restrictive corporate network) — use
  a public module proxy:
  ```sh
  export GOPROXY=https://proxy.golang.org,direct   # or a regional mirror
  make build
  ```
- **Browser shows a stale UI / old favicon after an update** — hard-refresh
  (`Cmd/Ctrl+Shift+R`); the daemon sends `Cache-Control: no-cache`, but browsers
  cache favicons aggressively (reopen the tab if needed).
- **Chat says "no token"** — add one in **Settings → LLM tokens** (§4).
