# Hopskip Frontend Extension Protocol Specification

> **Status:** Draft v1 — implementation target
> **Authority:** `CLAUDE.md` is the source of truth. This file defines the
> *runtime-extensible frontend*: how the platform serves a default shell yet lets
> the operator **or the agent itself** add new pages, panels, and buttons as code
> at runtime, with only a page refresh — no recompile, no rebuild of the binary.
> It refines `CLAUDE.md` §2 (asset delivery), §9 (frontend), §10 (config), and
> §11 (invariants 6 & 7).
> **Companions:** [`mcp-protocol.md`](./mcp-protocol.md) (tools the agent drives),
> [`llm-protocol.md`](./llm-protocol.md) (the agent loop the SDK's `runJob` rides).

---

## 1. Goal and the chosen design point

The operator wants to **program the platform from inside the platform**: click a
host, land on its detail page, and (for example) spin up an ad-hoc button that
runs an LLM job against that host — and have Hopskip *generate that UI on its own
surface*, visible after a refresh.

Two decisions fix the design (made deliberately, with eyes open to the cost):

1. **Programmability = full custom code/assets.** Extensions are real frontend
   modules (Vue components / pages / JS), not a constrained declarative palette.
   Maximum flexibility; a real browser-side code-execution surface (§7).
2. **Asset serving = hybrid overlay.** The default shell is embedded via
   `embed.FS` (single binary, works with zero external files). An optional
   on-disk `HOPSKIP_WEB_DIR` is overlaid *disk-first* so new/overriding assets
   appear at runtime. The override is **never required** — so Invariant 6 holds.

> **Why embed.FS was not actually the blocker.** The dynamic part of the UI is
> *content served from a mutable location*, not the embedded bundle. Embedding the
> default shell and overlaying a mutable dir are complementary: the binary always
> runs standalone, and runtime extensions live in the overlay.

---

## 2. Serving model — the overlay

> **AMENDED (implemented):** the authored overlay is **SQLite-backed**, not a
> disk dir. Authored UI lives in the `web_files` table (CLAUDE.md §6), so the
> whole self-written frontend migrates with the `.db` and authoring is a scoped
> DB upsert — there is *no filesystem path to traverse* (a simplification of the
> §7.1#4 guard). The disk overlay is retained as an optional, higher-precedence
> *dev* layer. Resolution is now **three layers**:
>
> ```
> resolve(path):
>     if HOPSKIP_WEB_DIR set and (HOPSKIP_WEB_DIR / path) is a regular file:
>         serve from disk            # optional dev override
>     else if web_files has path:
>         serve from the DB overlay  # LLM/operator-authored UI (the mutable layer)
>     else:
>         serve from embed.FS        # the default shell baked into the binary
> ```

The original two-layer model (kept for reference):

```
resolve(path):
    if HOPSKIP_WEB_DIR set and (HOPSKIP_WEB_DIR / path) exists and is a regular file:
        serve from disk            # mutable overlay — extensions & overrides
    else:
        serve from embed.FS        # the default shell baked into the binary
```

- **Disk-first, embed-fallback.** A file present in the overlay *shadows* the
  embedded one (lets you override `index.html`, swap a CSS, etc.). A file absent
  from the overlay is served from embed. With `HOPSKIP_WEB_DIR` unset, the server
  is pure embed — identical to today.
- **Single-binary guarantee preserved.** The embedded shell MUST be fully
  functional on its own (Invariant 6, refined). The overlay only adds.
- **SPA fallback** (history routing): unknown non-asset paths return the shell's
  `index.html` (overlay-first), so client-side routes — including
  extension-added routes (§6) — deep-link correctly.
- **Safe mode:** a launch flag (`--no-extensions`) or unsetting `HOPSKIP_WEB_DIR`
  serves embed-only and skips the extension manifest, so a broken extension can
  never brick the UI permanently.

### 2.1 Overlay directory layout

```
$HOPSKIP_WEB_DIR/
├── index.html                 # optional: override the embedded shell HTML
├── assets/…                   # optional: override embedded assets
└── extensions/
    └── <slug>/                # one dir per extension, slug-named
        ├── entry.mjs          # ES module, default-exports a component/route
        └── *.{mjs,js,css}     # supporting modules / styles
```

The daemon serves extension files under a dedicated mount, **`/ext/<slug>/…`**,
mapping to `$HOPSKIP_WEB_DIR/extensions/<slug>/…`, with `Content-Type:
text/javascript` for `.mjs`/`.js`. Only `.mjs`, `.js`, and `.css` are served from
this mount; other extensions are 404 (defense-in-depth; this is a local app, but
keep the served surface tight).

> **The manifest is in SQLite, not on disk** (§5). Files provide the *code*; the
> DB provides the *registration* (which slug mounts where, enabled, provenance),
> so registration is transactional and audited.

---

## 3. The shell as a host for extensions

The embedded Vue SPA is both the product UI **and** a generic host that loads
extension modules at runtime. It owns:

- the stable chrome — navigation, chat/transcript, fleet dashboard, terminal view
  (`CLAUDE.md` §9);
- **named extension slots** on built-in pages (§6);
- a router that accepts **extension-added routes** (§6);
- an **import map** and **SDK** that extensions link against (§4);
- the **loader** that reads the manifest and dynamically imports modules (§5.2).

### 3.1 Import map (shared runtime, no duplicate bundles)

The shell's `index.html` declares an import map so extension modules import the
shell's own Vue and SDK by alias — they ship no copy of Vue and can't drift in
version:

```html
<script type="importmap">
{ "imports": {
    "hopskip:vue": "/sdk/vue.mjs",
    "hopskip:sdk": "/sdk/hopskip.mjs"
} }
</script>
```

`/sdk/vue.mjs` and `/sdk/hopskip.mjs` are part of the **embedded** shell (stable,
versioned), served by the daemon. Extensions do `import { h, ref } from
'hopskip:vue'` and `import { hopskip } from 'hopskip:sdk'`.

---

## 4. The extension SDK (runtime contract)

This is the **stable API** extension code may rely on. It is versioned
(`hopskip.sdkVersion`, semver). The shell advertises its version; the loader skips
or warns on incompatible extensions (§5.3).

### 4.1 An extension module

An extension's `entry.mjs` default-exports a Vue component (for a slot or route),
or a small object describing what it provides:

```js
import { defineComponent, h, ref } from 'hopskip:vue'
import { hopskip } from 'hopskip:sdk'

export default defineComponent({
  // slot widgets receive the slot context as props, e.g. { host }
  props: { host: Object },
  setup(props) {
    const out = ref(null)
    async function runHealthCheck() {
      const run = hopskip.jobs.run({
        title: 'GPU health check',
        host: props.host.name,                       // bind to THIS host
        prompt: `On ${props.host.name}, run nvidia-smi; summarize temps, ` +
                `ECC errors, and utilization. Read-only.`,
      })
      run.onEvent(e => { if (e.kind === 'message_stop') out.value = run.text() })
    }
    return () => h('div', [
      h('button', { onClick: runHealthCheck }, 'GPU health check'),
      out.value && h('pre', out.value),
    ])
  },
})
```

### 4.2 SDK surface (`hopskip:sdk`)

| Member | Signature → returns | Notes |
|--------|---------------------|-------|
| `hopskip.sdkVersion` | `string` (semver) | for compatibility checks |
| `hopskip.api.getHost(idOrName)` | `Promise<Host>` | reads inventory (same data the dashboard uses) |
| `hopskip.api.listHosts(tag?)` | `Promise<Host[]>` | |
| `hopskip.jobs.run({title, host?, prompt, params?})` | `Run` | **the only side-effecting primitive** — starts an agent run; see §4.3 |
| `hopskip.context` | reactive object | route/slot context (e.g. current `host`) |
| `hopskip.nav.go(path)` | `void` | client-side navigation |
| `hopskip.toast(msg, level?)` | `void` | non-blocking UI feedback |

`Host` is the inventory object from `mcp-protocol.md` §4.4 (`tags` + timestamped
`facts`).

### 4.3 `jobs.run` — the bridge to the agent loop (the safety hinge)

`hopskip.jobs.run(...)` is the **one** way extension code causes anything to
happen on a host. It does **not** run commands; it **starts an agent run**: the
templated `prompt` (bound to the chosen host/params) becomes the operator-message
that drives the normal agent loop (`llm-protocol.md` §8). Therefore every action
an extension can trigger:

- flows through the single **`dispatch()`** choke point (Invariant 5),
- is recorded by the **audit proxy** (Invariant 4),
- and will pass the **future mutation gate** with no new bypass.

`run` is a handle multiplexed over the shell's existing WebSocket:

```text
Run {
  id:       string
  onEvent(cb: (StreamEvent) => void): void   // StreamEvent per llm-protocol §2.4
  text():   string                            // accumulated assistant text so far
  cancel(): void
}
```

There is intentionally **no** `sendKeys`/`openSession` in the SDK. Raw
terminal-driving stays the agent's job behind `dispatch()`; arbitrary browser code
asking the agent to do work (auditable) is fine, arbitrary browser code typing
into a host shell directly (unaudited, ungated) is not. Keep it that way.

---

## 5. Extension registry (manifest)

### 5.1 Data model (SQLite, schema-style consistent with `CLAUDE.md` §6)

```sql
CREATE TABLE web_extensions (
  id           INTEGER PRIMARY KEY,
  slug         TEXT UNIQUE NOT NULL,        -- dir name under extensions/, and id
  kind         TEXT NOT NULL,               -- 'route' | 'slot' | 'override'
  mount        TEXT NOT NULL,               -- route path (kind=route) OR slot name (kind=slot)
  entry        TEXT NOT NULL,               -- module URL, e.g. '/ext/gpu-health/entry.mjs'
  title        TEXT,
  sdk_range    TEXT,                         -- semver range expected, e.g. '>=1 <2'
  enabled      INTEGER NOT NULL DEFAULT 1,
  created_by   TEXT NOT NULL,               -- 'operator' | 'agent'  (provenance)
  created_at   TEXT NOT NULL,
  updated_at   TEXT,
  content_hash TEXT,                         -- hash of entry module at register time
  notes        TEXT
);
```

- **`kind`**: `route` adds a top-level page at `mount` (a path like
  `/tools/gpu`); `slot` renders into a named slot `mount` on a built-in page;
  `override` replaces a built-in slot/region (advanced).
- **`created_by`** records whether the operator or the agent authored it — the
  provenance trail for self-written UI.
- **`content_hash`** pins what was registered, so a later silent file change is
  detectable.

### 5.2 Manifest API + loader

- `GET /api/extensions` → enabled `web_extensions` rows (the manifest the shell
  consumes). The shell calls this on load and on refresh.
- Loader (in the shell), **defensive by construction**:

  ```js
  const exts = await fetch('/api/extensions').then(r => r.json())
  for (const e of exts) {
    if (!sdkSatisfies(e.sdk_range)) { warnIncompatible(e); continue }   // §5.3
    try {
      const mod = await import(e.entry)        // dynamic ES import from /ext/<slug>/…
      mountExtension(e, mod.default)           // into router (route) or slot (slot)
    } catch (err) {
      showExtensionError(e, err)               // NON-FATAL card; never crashes the shell
    }
  }
  ```

A failed import, a throwing component, or a version mismatch yields a contained
error card — **never** a blank shell. This is what makes "the agent writes UI
code" survivable.

### 5.3 Versioning

Each extension declares `sdk_range`; the shell exposes `hopskip.sdkVersion`. The
loader skips (with a visible notice) extensions whose range the running shell does
not satisfy, so a shell upgrade can't be silently broken by a stale extension, and
vice-versa.

---

## 6. Mount points: routes and slots

### 6.1 Built-in slots (extension insertion points)

Built-in pages render named slots that extensions target by `mount`. Initial set
(extend as needed; keep names stable — they are API):

| Slot name             | Where | Context passed to the extension |
|-----------------------|-------|---------------------------------|
| `host-detail.panels`  | Host detail page | `{ host }` (full Host record) |
| `host-detail.actions` | Host detail page action bar | `{ host }` |
| `dashboard.cards`     | Fleet dashboard | `{ hosts }` |
| `global.nav`          | Top navigation | `{}` |

A slot is rendered by a shell component that looks up enabled `kind='slot'`
extensions whose `mount` equals the slot name and renders each, passing the slot
context as props:

```html
<extension-slot name="host-detail.panels" :context="{ host }" />
```

This is exactly the "add an ad-hoc button to a host's detail page" path: register
a `kind='slot'`, `mount='host-detail.panels'` extension whose component renders
the button and calls `hopskip.jobs.run(...)` bound to `props.host`.

### 6.2 Routes

A `kind='route'` extension contributes a full page; the shell registers it on the
router at load: `router.addRoute({ path: ext.mount, component: mod.default })`.
Deep links resolve via the SPA fallback (§2).

---

## 7. Security model (full-custom-code — stated honestly)

Extensions are arbitrary same-origin code, and some of it is **authored by the
LLM**. That is a genuine code-execution surface. The design keeps it within
Hopskip's invariants *without* clipping the flexibility that was chosen:

### 7.1 Hard guards (MUST)

1. **`connect-src 'self'` (the load-bearing guard).** The shell is served with a
   Content-Security-Policy that confines network egress to the local daemon.
   Arbitrary extension code can manipulate the DOM and call the local API, but it
   **cannot exfiltrate to a third party** — preserving Invariant 7 ("everything
   stays local") even for code the operator did not write.
2. **No API token in the browser, ever** (Invariant 4/7). Extensions cannot reach
   the LLM provider directly; the token lives only in the daemon/proxy. The only
   route to the model is `hopskip.jobs.run` → the audited agent loop.
3. **Side effects only via the agent loop.** The SDK exposes no raw terminal
   driving. Every host-affecting action an extension can start is an agent run
   through `dispatch()` + audit proxy (§4.3).
4. **Authoring is scoped and audited** (§8). *(Amended — see §2.)* Authored code
   is written to the SQLite `web_files` overlay via `write_web_file`, keyed by a
   normalized web path (leading-slash and `..` rejected in `NormalizeWebPath`),
   so there is no filesystem path to traverse; registrations land in
   `web_extensions` with `created_by` + a `content_hash` of the entry module.
5. **Kill switch.** `--no-extensions` / unset `HOPSKIP_WEB_DIR` → embed-only,
   manifest ignored. Per-extension `enabled=0` disables one without deletion.

### 7.2 Recommended CSP

```
Content-Security-Policy:
  default-src 'self';
  connect-src 'self';                 # ← the guard: no third-party egress
  img-src    'self' data:;
  style-src  'self' 'unsafe-inline';  # Vue scoped/inline styles
  script-src 'self' 'unsafe-eval';    # see §7.3 — enables runtime template compile
```

### 7.3 The `'unsafe-eval'` tradeoff (conscious choice)

To let the agent write natural template-based Vue components that "just work on
refresh" (no build server), the shell includes Vue's **in-browser template
compiler**, which uses `new Function` and therefore needs `script-src
'unsafe-eval'`. This is accepted because:

- the real exfiltration guard is **`connect-src 'self'`**, not `script-src`;
- extensions are same-origin and already trusted to run as page code;
- it removes the need for a daemon-side bundler (keeps the single-binary, no-build
  promise).

**Stricter alternative** (drop `'unsafe-eval'`): require extensions to ship
**precompiled render functions** (`h(...)`/JSX, no runtime template strings). More
secure, but then template authoring needs a compile step somewhere. Choose per
threat tolerance; default here is the pragmatic in-browser compiler.

### 7.4 Residual risk (acknowledged)

A prompt-injected agent could write a buggy or hostile extension (e.g. one that
defaces the UI or spams `jobs.run`). The guards bound the *blast radius* (no data
egress, no token, no unaudited host actions, killable, attributable) but do not
eliminate in-browser misbehavior. If untrusted extensions ever become a concern,
the upgrade path is **iframe isolation** per extension with a `postMessage` SDK
bridge — deliberately *not* the default, because it contradicts the "full app
capabilities, full DOM" point that was chosen.

---

## 8. Self-programming: agent-facing extension tools (MCP)

So the platform can generate its own UI, the daemon exposes a third MCP tool
family (alongside terminal-driving and inventory — `mcp-protocol.md` §1). These go
through the same MCP server and the same `dispatch()` choke point (Invariant 5).
Files live in the SQLite `web_files` overlay (amended §2/§5), keyed by a normalized
web path — so authoring is a scoped DB upsert with no filesystem path to traverse.

> **As built.** The shipped tool names differ from the original draft below; these
> are the implemented ones.

| Tool | Input | Output | Effect |
|------|-------|--------|--------|
| `list_web_files` | `{}` | `{ overlay: [...], base_shell: [...] }` | Lists authored overlay files + the embedded base-shell paths. |
| `read_web_file` | `{ path }` | `{ path, source, content }` | Reads source — overlay first, then the embedded base shell. |
| `write_web_file` | `{ path, content, content_type? }` | `{ ok, path, bytes, served_at }` | Upserts a `web_files` overlay asset (`created_by='agent'`). `path` normalized — leading `/` and `..` rejected. |
| `delete_web_file` | `{ path }` | `{ ok: true, path }` | Removes an overlay asset, reverting that path to the base shell. |
| `register_extension` | `{ slug, kind, mount, entry, title?, sdk_range?, notes? }` | `{ ok, slug, kind, mount, content_hash }` | Upserts a `web_extensions` row; `created_by='agent'`, stamps `content_hash` of the entry. |
| `unregister_extension` | `{ slug }` | `{ ok: true }` | Removes a manifest row (files remain unless also `delete_web_file`d). |
| `list_extensions` | `{}` | `{ extensions: [...] }` | Lists the manifest. |

These reuse the MCP conventions of `mcp-protocol.md` §4 (two error layers,
structured output, annotations). `write_web_file`, `delete_web_file`,
`register_extension`, and `unregister_extension` are **not** read-only. A separate
**Save to disk** action (`POST /api/web/export`) *commits* the overlay: it writes
the files to `web_export_dir`, serves them from there (below pending DB edits), and
removes them from `web_files` — so the Changes tab shows only not-yet-saved work.

### 8.1 End-to-end self-programming flow

```
operator (chat): "Add a GPU-health button to the host detail page."
  └─ agent loop:
       write_web_file{ path:"ext/gpu-health/entry.mjs",
                       content:<the component above> }
       register_extension{ slug:"gpu-health", kind:"slot",
                           mount:"host-detail.panels",
                           entry:"/ext/gpu-health/entry.mjs",
                           title:"GPU health", sdk_range:">=1 <2" }
       assistant: "Done — refresh the host page and you'll see a
                   'GPU health check' button."
operator: refresh → shell loads manifest → imports /ext/gpu-health/entry.mjs
        → renders into host-detail.panels with { host } context.
click → hopskip.jobs.run(...) → agent run → dispatch() → audit proxy → transcript.
```

A page refresh is the deploy step — exactly the requirement.

---

## 9. Config (additions to `CLAUDE.md` §10)

| Variable | Meaning |
|----------|---------|
| `HOPSKIP_WEB_DIR` | Optional on-disk overlay root (extensions + asset overrides). Unset ⇒ embed-only. |
| `--no-extensions` (flag) | Serve embed-only; ignore the manifest. Recovery / safe mode. |

---

## 10. Invariants honored / refined (cross-check with `CLAUDE.md` §11)

- **6 (refined).** The deliverable is still one Go binary, with the **default
  shell embedded** and fully functional standalone. `HOPSKIP_WEB_DIR` is an
  *optional, additive* overlay — never a mandatory external service. ✔
- **7 (upheld under arbitrary code).** Even full-custom extensions cannot leak
  data: `connect-src 'self'` confines egress to the local daemon, and the API
  token never enters the browser. Everything still stays local. ✔
- **4 & 5 (upheld).** The SDK's only side-effecting primitive (`jobs.run`) routes
  through the agent loop → `dispatch()` → audit proxy. Extensions add UI, not new
  privileged paths. ✔
- **1–3 (unaffected).** Reachability-by-typed-`ssh`, screen-is-truth, and
  schema-less inventory are untouched; extensions consume inventory read-only and
  reach hosts only by asking the agent. ✔
