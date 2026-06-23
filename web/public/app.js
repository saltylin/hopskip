import { createApp, reactive, markRaw, h } from './vendor/vue.esm-browser.prod.js'

const wsBase = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host

// ssh options for the Connect convenience: keepalive (so idle sessions survive) +
// auto-accept new host keys (so chained hops don't stall on a yes/no prompt).
const SSH_OPTS = '-o ServerAliveInterval=60 -o ServerAliveCountMax=10 -o StrictHostKeyChecking=accept-new'

// Markdown renderer for assistant messages. html:false escapes raw HTML, so it's
// XSS-safe even for host command output echoed into the transcript; links open in
// a new tab so they don't navigate the SPA away.
const md = window.markdownit
  ? window.markdownit({ html: false, linkify: true, breaks: true })
  : null
if (md) {
  const defOpen = md.renderer.rules.link_open || ((t, i, o, e, s) => s.renderToken(t, i, o))
  md.renderer.rules.link_open = (t, i, o, e, s) => {
    t[i].attrSet('target', '_blank')
    t[i].attrSet('rel', 'noopener noreferrer')
    return defOpen(t, i, o, e, s)
  }
}
function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))
}
function renderMd(text) {
  if (md) return md.render(text || '')
  return escapeHtml(text || '').replace(/\n/g, '<br>')
}

// Tool calls and results are JSON strings — shown raw they print literal "\n" and
// "<". Parsing turns those back into real newlines and characters; we then
// lay the fields out as readable "key: value" lines (multi-line values like a
// captured screen flow onto their own lines). The bubble is white-space:pre-wrap,
// so the newlines render. Anything that isn't JSON is shown unchanged.
// Tidy a multi-line value for display: right-trim each line, drop leading/trailing
// blank lines (a captured screen is padded to the pane height), and collapse runs
// of blank lines. Also cleans up already-stored results from older chats.
function tidyMultiline(v) {
  let lines = v.split('\n').map((l) => l.replace(/[ \t]+$/, ''))
  while (lines.length && lines[0] === '') lines.shift()
  while (lines.length && lines[lines.length - 1] === '') lines.pop()
  const out = []; let blank = 0
  for (const l of lines) { if (l === '') { if (++blank > 1) continue } else blank = 0; out.push(l) }
  return out.join('\n')
}
function prettyKV(obj) {
  return Object.entries(obj).map(([k, v]) => {
    if (typeof v === 'string') return v.includes('\n') ? k + ':\n' + tidyMultiline(v) : k + ': ' + v
    return k + ': ' + JSON.stringify(v)
  }).join('\n')
}
// Turn JSON string escapes (\n, \t, \uXXXX, \", \\) back into real characters.
// Used as a fallback when a result can't be parsed (e.g. it was truncated for
// display, cutting the JSON mid-string) so it's still readable, not literal "\n".
function jsonUnescape(s) {
  let out = ''
  for (let i = 0; i < s.length; i++) {
    if (s[i] !== '\\' || i + 1 >= s.length) { out += s[i]; continue }
    const c = s[++i]
    if (c === 'n') out += '\n'
    else if (c === 't') out += '\t'
    else if (c === 'r') out += ''
    else if (c === 'u') { out += String.fromCharCode(parseInt(s.substr(i + 1, 4), 16) || 0); i += 4 }
    else out += c // \" \\ \/ → the literal char
  }
  return out
}
// Parse JSON, tolerating a value that was truncated mid-string for display: try
// the text as-is, then a few repairs that close an unterminated string/object.
function looseParseObject(text) {
  const t = (text || '').trim()
  if (!t.startsWith('{')) return null
  const tries = [t, t + '"}', t + '}', t + '"}}', t + '}}']
  const comma = t.lastIndexOf(',')
  if (comma > 0) tries.push(t.slice(0, comma) + '}')
  for (const c of tries) {
    try { const v = JSON.parse(c); if (v && typeof v === 'object' && !Array.isArray(v)) return v } catch (e) {}
  }
  return null
}
function formatToolResult(text) {
  const obj = looseParseObject(text)
  if (obj) return prettyKV(obj)
  try {
    const v = JSON.parse(text)
    return typeof v === 'string' ? v : JSON.stringify(v, null, 2)
  } catch (e) {}
  return jsonUnescape(text) // not parseable even loosely — at least unescape it
}
// A tool_use line is "name  {args-json}"; pretty-print the args, keep the name.
function formatToolUse(text) {
  const i = text.indexOf('  ')
  if (i > 0) {
    const name = text.slice(0, i), rest = text.slice(i + 2)
    const a = looseParseObject(rest)
    if (a) return Object.keys(a).length ? name + '\n' + prettyKV(a) : name
    return name + '\n' + jsonUnescape(rest)
  }
  return text
}

// cloud provider is a schema-less `provider` tag; give each value a stable color.
function providerOf(h) { return (h && h.tags && h.tags.provider) || '' }
const PROVIDER_COLORS = ['#4f9cf9', '#6ee7b7', '#f0a868', '#c084fc', '#f472b6', '#38bdf8', '#fbbf24', '#34d399']
function providerColor(p) {
  if (!p) return '#5b6677'
  let n = 0
  for (let i = 0; i < p.length; i++) n = (n * 31 + p.charCodeAt(i)) >>> 0
  return PROVIDER_COLORS[n % PROVIDER_COLORS.length]
}

async function api(method, path, body) {
  const opt = { method, headers: {} }
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json'
    opt.body = JSON.stringify(body)
  }
  const res = await fetch(path, opt)
  const text = await res.text()
  const data = text ? JSON.parse(text) : {}
  if (!res.ok) throw new Error(data.error || ('HTTP ' + res.status))
  return data
}

function sshTargetOf(h) {
  if (!h) return ''
  return (h.tags && h.tags.ssh) || h.name || ''
}

const termTheme = {
  background: '#000000', foreground: '#d6deeb', cursor: '#6ee7b7',
  // a clear, high-contrast selection: solid accent blue with dark text on top
  selectionBackground: '#4f9cf9', selectionForeground: '#06121f',
  selectionInactiveBackground: '#3a6ea5',
  black: '#0e1116', brightBlack: '#5b6677',
}

// ---- Terminal view: a real browser terminal attached to a tmux session ----
//
// Connection-state model fixes the "ssh button still fires after you're already
// logged in" bug: once we've typed `ssh <target>` (manually or auto on open),
// loggedIn flips and the button is replaced by a static pill, so it can't run a
// nested/duplicate ssh. Reattached sessions start assumed-connected.
const TermView = {
  props: {
    session: Object,
    host: Object,
    steps: { type: Array, default: () => [] },
    autoConnect: { type: Boolean, default: false },
    startConnected: { type: Boolean, default: false },
    scrollback: { type: Number, default: 100000 },
  },
  emits: ['close'],
  data() { return { connected: false, loggedIn: this.startConnected } },
  computed: {
    hasConnect() { return this.steps.length > 0 },
    hops() { return this.steps.length },
    targetLabel() {
      if (this.steps.length) return this.steps[this.steps.length - 1]
      return this.host ? this.host.name : ''
    },
    connectBtnLabel() {
      return this.hops > 1 ? ('⮕ connect (' + this.hops + ' hops)') : ('⮕ ssh ' + this.targetLabel)
    },
  },
  mounted() {
    const term = new window.Terminal({
      fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
      fontSize: 13, cursorBlink: true, cursorStyle: 'bar', theme: termTheme, scrollback: this.scrollback,
    })
    const fit = new window.FitAddon.FitAddon()
    term.loadAddon(fit)
    term.open(this.$refs.term)
    fit.fit()
    this.term = term
    this.fit = fit

    const url = wsBase + '/ws/term/' + this.session.session_id +
      '?rows=' + term.rows + '&cols=' + term.cols
    const ws = new WebSocket(url)
    ws.binaryType = 'arraybuffer'
    this.ws = ws

    ws.onopen = () => {
      this.connected = true
      this.sendResize()
      term.focus()
      // Auto-connect: land on the host instead of the laptop. Still just typing
      // `ssh` into the session (invariant 1) — a convenience, not a declared route.
      if (this.autoConnect && this.hasConnect && !this.loggedIn) {
        setTimeout(() => this.doConnect(), 350)
      }
    }
    ws.onmessage = (ev) => {
      if (ev.data instanceof ArrayBuffer) term.write(new Uint8Array(ev.data))
    }
    ws.onclose = () => {
      this.connected = false
      term.write('\r\n\x1b[2m[disconnected]\x1b[0m\r\n')
    }
    term.onData((d) => {
      if (ws.readyState === 1) ws.send(new TextEncoder().encode(d))
    })

    // Selection is tmux's (mouse on), copied on drag-end and delivered to the
    // browser via OSC 52 — honor it so a drag puts the text on the system
    // clipboard. (onSelectionChange still covers an xterm-local Shift-drag.)
    term.onSelectionChange(() => {
      const sel = term.getSelection()
      if (sel && navigator.clipboard) navigator.clipboard.writeText(sel).catch(() => {})
    })
    if (term.parser && term.parser.registerOscHandler) {
      term.parser.registerOscHandler(52, (data) => {
        // data is "<Pc>;<base64>" (e.g. "c;SGVsbG8=")
        const semi = String(data).indexOf(';')
        if (semi >= 0 && navigator.clipboard) {
          try {
            const text = atob(String(data).slice(semi + 1))
            if (text) navigator.clipboard.writeText(text).catch(() => {})
          } catch (e) {}
        }
        return true
      })
    }
    term.attachCustomKeyEventHandler((e) => {
      if (e.type !== 'keydown') return true
      const isC = e.key === 'c' || e.key === 'C'
      const isV = e.key === 'v' || e.key === 'V'
      const copyCombo = (e.metaKey && isC) || (e.ctrlKey && e.shiftKey && isC)
      const pasteCombo = (e.metaKey && isV) || (e.ctrlKey && e.shiftKey && isV)
      if (copyCombo) {
        const sel = term.getSelection()
        if (sel && navigator.clipboard) navigator.clipboard.writeText(sel).catch(() => {})
        e.preventDefault(); return false
      }
      if (pasteCombo) {
        if (navigator.clipboard) navigator.clipboard.readText().then((t) => { if (t) this.type(t) }).catch(() => {})
        e.preventDefault(); return false
      }
      return true
    })

    this.ro = new ResizeObserver(() => { try { fit.fit(); this.sendResize() } catch (e) {} })
    this.ro.observe(this.$refs.term)
  },
  beforeUnmount() {
    if (this.ro) this.ro.disconnect()
    if (this.ws) { this.ws.onclose = null; this.ws.close() }
    if (this.term) this.term.dispose()
  },
  methods: {
    sendResize() {
      if (this.ws && this.ws.readyState === 1) {
        this.ws.send(JSON.stringify({ type: 'resize', cols: this.term.cols, rows: this.term.rows }))
      }
    },
    type(text) {
      if (this.ws && this.ws.readyState === 1) {
        this.ws.send(new TextEncoder().encode(text))
        this.term.focus()
      }
    },
    doConnect() {
      if (!this.steps.length || this.loggedIn) return
      this.loggedIn = true // lock the button to prevent a second, wrong ssh
      // Type each hop in sequence, spaced so each ssh lands before the next.
      this.steps.forEach((spec, i) => {
        setTimeout(() => this.type('ssh ' + SSH_OPTS + ' ' + spec + '\n'), i * 1800)
      })
    },
    resetConn() {
      // UI-only: use after you `exit` back to the laptop, to re-enable ssh.
      this.loggedIn = false
      this.term.focus()
    },
  },
  template: `
    <div class="main">
      <div class="main-head">
        <span class="title">{{ host ? host.name : 'scratch terminal' }}</span>
        <span class="host-meta" v-if="targetLabel">ssh {{ targetLabel }}<span v-if="hops > 1"> · {{ hops - 1 }} hop{{ hops > 2 ? 's' : '' }}</span></span>
        <span class="spacer"></span>
        <span class="dim" style="font-size:12px">{{ connected ? '● live' : '○ closed' }}</span>
        <template v-if="hasConnect">
          <button v-if="!loggedIn" class="conn-pill" @click="doConnect()">{{ connectBtnLabel }}</button>
          <template v-else>
            <span class="conn-pill connected" title="this session is on the remote host">● {{ targetLabel }}</span>
            <button class="reset-pill" title="I exited back to the laptop — re-enable ssh" @click="resetConn()">↺</button>
          </template>
        </template>
        <button class="btn ghost" @click="$emit('close')">Close session</button>
      </div>
      <div class="term-wrap"><div class="term-host" ref="term"></div></div>
    </div>
  `,
}

// ---- Chat view ----
const ChatView = {
  props: { agentConfigured: Boolean },
  emits: ['fleet-changed'],
  data() { return { chats: [], currentId: null, messages: [], input: '', connected: false,
                    status: { active: false, phase: '', start: 0, tokIn: 0, tokOut: 0 },
                    now: 0, lastSummary: null, canJumpTop: false, canJumpBottom: false } },
  async mounted() {
    this.connect()
    await this.loadChats()
    if (this.chats.length) await this.selectChat(this.chats[0])
    else await this.newChat()
  },
  beforeUnmount() {
    this._closing = true
    clearTimeout(this._reconnectT)
    if (this.ws) { this.ws.onclose = null; this.ws.close() }
    this.stopTicker()
  },
  updated() {
    // Stick to the bottom only when the user is already near it, so the live
    // "thinking…" timer (which re-renders every tick) doesn't yank them down
    // while they scroll up to read.
    const el = this.$refs.scroll
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 120) el.scrollTop = el.scrollHeight
    this.onScroll() // refresh the jump-button visibility as content changes
  },
  computed: {
    elapsedSec() { return this.status.active ? Math.max(0, (this.now - this.status.start) / 1000) : 0 },
    liveTokens() { return (this.status.tokIn || 0) + (this.status.tokOut || 0) },
    statusLabel() { return this.status.phase === 'responding' ? 'Responding' : 'Thinking' },
  },
  methods: {
    connect() {
      const ws = new WebSocket(wsBase + '/ws/chat')
      this.ws = ws
      ws.onopen = () => { this.connected = true; this._reconnectDelay = 500 }
      // Auto-reconnect: a dropped socket (idle/NAT/proxy/laptop sleep) otherwise
      // leaves Send greyed out until a manual refresh. Back off 0.5s→8s.
      ws.onclose = () => { this.connected = false; this.scheduleReconnect() }
      ws.onerror = () => { try { ws.close() } catch (e) {} } // -> onclose -> reconnect
      ws.onmessage = (ev) => { try { this.onEvent(JSON.parse(ev.data)) } catch (e) {} }
    },
    scheduleReconnect() {
      if (this._closing) return // intentional close on unmount
      const delay = this._reconnectDelay || 500
      this._reconnectDelay = Math.min(delay * 2, 8000)
      clearTimeout(this._reconnectT)
      this._reconnectT = setTimeout(() => { if (!this._closing) this.connect() }, delay)
    },
    async loadChats() { this.chats = (await api('GET', '/api/chats')).chats || [] },
    async newChat() {
      const c = await api('POST', '/api/chats')
      this.currentId = c.id; this.messages = []
      await this.loadChats()
    },
    async selectChat(c) {
      this.currentId = c.id
      const d = await api('GET', '/api/chats/' + c.id + '/messages')
      this.messages = (d.messages || []).map((m) => {
        if (m.role === 'assistant') return { role: m.role, text: m.text, html: renderMd(m.text) }
        if (m.role === 'tool') return { role: m.role, text: m.text, disp: formatToolUse(m.text) }
        if (m.role === 'tool-result') return { role: m.role, text: m.text, disp: formatToolResult(m.text) }
        return { role: m.role, text: m.text }
      })
    },
    async deleteChat(c) {
      if (!confirm('Delete this chat?')) return
      const wasCurrent = this.currentId === c.id
      await api('DELETE', '/api/chats/' + c.id)
      await this.loadChats()
      if (wasCurrent) {
        if (this.chats.length) await this.selectChat(this.chats[0])
        else await this.newChat()
      }
    },
    chatLabel(c) { return c.title || 'New chat' },
    fmtNum(n) { return (n || 0).toLocaleString() },
    startTicker() { if (!this._tick) this._tick = setInterval(() => { this.now = Date.now() }, 250) },
    stopTicker() { if (this._tick) { clearInterval(this._tick); this._tick = null } },
    beginTurn() {
      this.now = Date.now()
      this.status = { active: true, phase: 'thinking', start: this.now, tokIn: 0, tokOut: 0 }
      this.lastSummary = null
      this.startTicker()
    },
    endTurn() {
      if (this.status.active) {
        this.lastSummary = { sec: (Date.now() - this.status.start) / 1000, tok: this.liveTokens }
      }
      this.status.active = false
      this.stopTicker()
    },
    onEvent(e) {
      if (e.kind === 'token') {
        this.status.active = true; this.status.phase = 'responding'; this.startTicker()
        if (!this.open) { this.open = { role: 'assistant', text: '', html: '' }; this.messages.push(this.open) }
        this.open.text += e.text || ''
        this.open.html = renderMd(this.open.text) // only the streaming message re-renders
      } else if (e.kind === 'thinking') {
        this.status.active = true; this.status.phase = 'thinking'; this.startTicker()
      } else if (e.kind === 'usage') {
        this.status.tokIn += e.in || 0; this.status.tokOut += e.out || 0
      } else if (e.kind === 'tool_use') {
        this.open = null; this.status.phase = 'thinking'
        this.messages.push({ role: 'tool', text: e.text || '', disp: formatToolUse(e.text || '') })
      } else if (e.kind === 'tool_result') {
        this.status.phase = 'thinking'
        this.messages.push({ role: 'tool-result', text: e.text || '', disp: formatToolResult(e.text || '') })
      } else if (e.kind === 'notice') {
        this.open = null; this.messages.push({ role: 'notice', text: e.text || '' })
      } else if (e.kind === 'error') {
        this.open = null; this.endTurn(); this.messages.push({ role: 'error', text: e.text || 'error' })
      } else if (e.kind === 'done') {
        // The agent may have changed the inventory or sessions during this turn —
        // tell the app to refresh the sidebar + topology (no page reload needed).
        this.open = null; this.endTurn(); this.loadChats(); this.$emit('fleet-changed')
      }
    },
    send() {
      const text = this.input.trim()
      if (!text || !this.connected || !this.currentId) return
      this.messages.push({ role: 'user', text })
      this.ws.send(JSON.stringify({ type: 'user_message', chat_id: this.currentId, text }))
      this.input = ''; this.open = null; this.beginTurn()
    },
    // Ask the daemon to cancel the in-flight run. The agent loop cancels at the
    // next provider call and emits a "⏹ Stopped." notice + done, which ends the
    // turn here. endTurn locally too so the button flips back without waiting.
    stop() {
      if (this.ws && this.ws.readyState === 1) {
        this.ws.send(JSON.stringify({ type: 'stop', chat_id: this.currentId }))
      }
      this.status.phase = 'thinking'
    },
    onKey(e) { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); this.send() } },
    // Jump-to-top/bottom: each button shows only when there's somewhere to go in
    // that direction (a small threshold avoids flicker right at the edges).
    onScroll() {
      const el = this.$refs.scroll
      if (!el) { this.canJumpTop = this.canJumpBottom = false; return }
      this.canJumpTop = el.scrollTop > 200
      this.canJumpBottom = el.scrollHeight - el.scrollTop - el.clientHeight > 200
    },
    scrollToTop() { const el = this.$refs.scroll; if (el) el.scrollTo({ top: 0, behavior: 'smooth' }) },
    scrollToBottom() { const el = this.$refs.scroll; if (el) el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' }) },
  },
  template: `
    <div class="main">
      <div class="main-head">
        <span class="title">Chat</span><span class="spacer"></span>
        <span class="badge" :class="{ open: agentConfigured }">
          {{ agentConfigured ? 'agent ready' : 'agent not configured' }}
        </span>
      </div>
      <div class="chat-layout">
        <div class="chat-list">
          <button class="btn" style="margin:8px; width:calc(100% - 16px)" @click="newChat()">＋ New chat</button>
          <div v-for="c in chats" :key="c.id" class="chat-item"
               :class="{ active: c.id === currentId }" @click="selectChat(c)">
            <span class="ci-title">{{ chatLabel(c) }}</span>
            <span class="x" @click.stop="deleteChat(c)" title="delete">✕</span>
          </div>
          <div v-if="chats.length === 0" class="dim" style="padding:8px; font-size:12px">No saved chats yet.</div>
        </div>
        <div class="chat">
          <div class="transcript" ref="scroll" @scroll="onScroll">
            <div v-if="messages.length === 0" class="empty">
              Ask Hopskip to investigate your fleet — e.g. <em>"check why my web server is down"</em>.<br>
              Chats are saved automatically; switch or delete them on the left.
            </div>
            <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
              <div class="who" v-if="m.role !== 'user'">{{ m.role }}</div>
              <div v-if="m.role === 'assistant'" class="bubble md" v-html="m.html"></div>
              <div v-else class="bubble">{{ m.disp || m.text }}</div>
            </div>
            <div v-if="status.active" class="status-line">
              <span class="dot-pulse"></span>
              {{ statusLabel }}… <span class="mono">{{ elapsedSec.toFixed(1) }}s</span>
              <span v-if="liveTokens" class="dim"> · {{ fmtNum(liveTokens) }} tokens</span>
            </div>
            <div v-else-if="lastSummary" class="turn-summary">
              {{ lastSummary.sec.toFixed(1) }}s<span v-if="lastSummary.tok"> · {{ fmtNum(lastSummary.tok) }} tokens</span>
            </div>
          </div>
          <div class="scroll-jumps">
            <button v-if="canJumpTop" class="jump-btn" title="jump to top" @click="scrollToTop()">↑</button>
            <button v-if="canJumpBottom" class="jump-btn" title="jump to bottom" @click="scrollToBottom()">↓</button>
          </div>
          <div class="composer">
            <textarea v-model="input" rows="1" placeholder="Message Hopskip…  (Enter to send)" @keydown="onKey"></textarea>
            <button v-if="status.active" class="btn stop" @click="stop()" title="stop the agent">⏹ Stop</button>
            <button v-else class="btn" :disabled="!connected || !input.trim()" @click="send()">
              {{ connected ? 'Send' : 'Reconnecting…' }}
            </button>
          </div>
        </div>
      </div>
    </div>
  `,
}

// ---- Topology view: local node (root) -> cloud hosts ----
const TopoView = {
  props: { hosts: Array, sessions: Array, providers: Array },
  emits: ['open'],
  data() { return { w: 900, activeProvider: '' } },
  mounted() {
    this.measure()
    this.ro = new ResizeObserver(() => this.measure())
    if (this.$refs.box && this.$refs.box.parentElement) this.ro.observe(this.$refs.box.parentElement)
  },
  beforeUnmount() { if (this.ro) this.ro.disconnect() },
  computed: {
    topoProviders() {
      const out = []
      ;(this.hosts || []).forEach((h) => { const p = providerOf(h); if (p && !out.includes(p)) out.push(p) })
      return out
    },
    layout() {
      // Build a tree: a host's parent is the host named by its `via` tag (a jump
      // host), if that host exists; otherwise the laptop (root). Then a simple
      // tidy-tree layout — leaves get sequential columns, parents center over kids.
      const hosts = this.hosts || []
      const rootY = 56, topY = 200, levelGap = 140, leafGap = 200
      const byName = {}
      hosts.forEach((h) => { byName[h.name] = h })
      const parentOf = (h) => {
        const via = h.tags && h.tags.via
        const gw = via && byName[via]
        return gw && gw.id !== h.id ? gw : null
      }
      const kids = {}
      hosts.forEach((h) => { kids[h.id] = [] })
      const parentId = {}
      const roots = []
      hosts.forEach((h) => {
        const p = parentOf(h)
        let cyc = false, cur = p, guard = 0
        while (cur && guard < 32) { if (cur.id === h.id) { cyc = true; break } cur = parentOf(cur); guard++ }
        if (p && !cyc) { kids[p.id].push(h); parentId[h.id] = p.id }
        else { roots.push(h); parentId[h.id] = null }
      })
      let leaf = 0, maxDepth = 1
      const pos = {}
      const assign = (h, depth) => {
        maxDepth = Math.max(maxDepth, depth)
        const cs = kids[h.id]
        if (!cs.length) { pos[h.id] = { col: leaf++, depth } }
        else {
          cs.forEach((c) => assign(c, depth + 1))
          const cols = cs.map((c) => pos[c.id].col)
          pos[h.id] = { col: (Math.min(...cols) + Math.max(...cols)) / 2, depth }
        }
      }
      roots.forEach((h) => assign(h, 1))
      const leaves = Math.max(1, leaf)
      const w = Math.max(this.w || 900, leaves * leafGap)
      const rootX = w / 2
      const colX = (c) => ((c + 0.5) / leaves) * w
      const depthY = (d) => topY + (d - 1) * levelGap
      const nodes = [], edges = []
      hosts.forEach((h) => {
        const p = pos[h.id]; if (!p) return
        const x = colX(p.col), y = depthY(p.depth)
        nodes.push({ host: h, x, y })
        const pid = parentId[h.id]
        if (pid == null) edges.push({ x1: rootX, y1: rootY, x2: x, y2: y })
        else { const pp = pos[pid]; edges.push({ x1: colX(pp.col), y1: depthY(pp.depth), x2: x, y2: y }) }
      })
      const height = (hosts.length === 0 ? topY : depthY(maxDepth)) + 110
      return { rootX, rootY, nodes, edges, height, w }
    },
  },
  methods: {
    measure() {
      const sc = this.$refs.box && this.$refs.box.parentElement
      if (sc && sc.clientWidth) this.w = Math.max(320, sc.clientWidth - 24)
    },
    nodeStyle(x, y) { return { left: x + 'px', top: y + 'px' } },
    sessOf(h) { return (this.sessions || []).filter((s) => s.status === 'open' && s.host_id === h.id).length },
    sshOf(h) { return (h.tags && h.tags.ssh) || '' },
    provOf(h) { return providerOf(h) },
    provColor(p) { return providerColor(p) },
    provStyle(p) { return { color: providerColor(p), borderColor: providerColor(p) } },
    dimmed(h) { return this.activeProvider && providerOf(h) !== this.activeProvider },
    toggleProv(p) { this.activeProvider = this.activeProvider === p ? '' : p },
  },
  template: `
    <div class="main">
      <div class="main-head">
        <span class="title">Topology</span>
        <span class="host-meta">local root · {{ hosts.length }} cloud host{{ hosts.length === 1 ? '' : 's' }}</span>
        <span class="spacer"></span>
        <div class="prov-filter" v-if="topoProviders.length">
          <button class="prov-chip" :class="{ active: !activeProvider }" @click="activeProvider = ''">all</button>
          <button v-for="p in topoProviders" :key="p" class="prov-chip" :class="{ active: activeProvider === p }"
                  :style="{ color: provColor(p), borderColor: provColor(p) }" @click="toggleProv(p)">{{ p }}</button>
        </div>
      </div>
      <div class="topo-scroll">
        <div class="topo-box" ref="box" :style="{ height: layout.height + 'px', width: layout.w + 'px' }">
          <svg class="topo-svg" :width="layout.w" :height="layout.height">
            <line v-for="(e, i) in layout.edges" :key="i" :x1="e.x1" :y1="e.y1" :x2="e.x2" :y2="e.y2" />
          </svg>
          <div class="topo-node root" :style="nodeStyle(layout.rootX, layout.rootY)">
            <div class="tn-title">💻 This laptop</div>
            <div class="tn-sub">local · root</div>
          </div>
          <div v-for="nd in layout.nodes" :key="nd.host.id"
               class="topo-node host" :class="{ live: sessOf(nd.host) > 0 }"
               :style="[nodeStyle(nd.x, nd.y), { opacity: dimmed(nd.host) ? 0.28 : 1 }]" @click="$emit('open', nd.host)">
            <div class="tn-title">🖥 {{ nd.host.name }}</div>
            <div class="tn-sub" v-if="sshOf(nd.host)">{{ sshOf(nd.host) }}</div>
            <div class="tn-meta">
              <span v-if="provOf(nd.host)" class="prov-badge" :style="provStyle(provOf(nd.host))">{{ provOf(nd.host) }}</span>
              <span v-if="sessOf(nd.host) > 0" class="live-dot">● {{ sessOf(nd.host) }}</span>
            </div>
          </div>
          <div v-if="hosts.length === 0" class="topo-empty">
            No cloud hosts yet — add one in the sidebar, then it appears here under your laptop.
          </div>
        </div>
      </div>
    </div>
  `,
}

// ---- Host detail view ----
const HostDetailView = {
  props: { host: Object, providers: { type: Array, default: () => [] } },
  emits: ['terminal', 'deleted', 'back', 'changed'],
  data() { return { full: this.host, loading: true, editing: false, editErr: '', edit: { name: '', ssh: '', via: '', provider: '', notes: '' } } },
  async mounted() { await this.refresh() },
  computed: {
    sshTarget() { return sshTargetOf(this.full) },
    provider() { return providerOf(this.full) },
    tagEntries() { return Object.entries((this.full && this.full.tags) || {}) },
    factEntries() { return Object.entries((this.full && this.full.facts) || {}) },
  },
  methods: {
    async refresh() {
      try { this.full = await api('GET', '/api/hosts/' + this.host.id) }
      catch (e) { this.full = this.host } finally { this.loading = false }
    },
    async del() {
      if (!confirm('Delete host "' + this.full.name + '"?')) return
      await api('DELETE', '/api/hosts/' + this.full.id)
      this.$emit('deleted', this.full)
    },
    startEdit() {
      // Prefill ssh with the current connection target so renaming away from an
      // IP doesn't lose how to reach the host.
      this.editErr = ''
      this.edit = {
        name: this.full.name,
        ssh: this.sshTarget || '',
        via: (this.full.tags && this.full.tags.via) || '',
        provider: (this.full.tags && this.full.tags.provider) || '',
        notes: this.full.notes || '',
      }
      this.editing = true
    },
    provStyle(p) { return { color: providerColor(p), borderColor: providerColor(p) } },
    cancelEdit() { this.editing = false },
    async saveEdit() {
      this.editErr = ''
      const name = this.edit.name.trim()
      if (!name) { this.editErr = 'name is required'; return }
      try {
        this.full = await api('PATCH', '/api/hosts/' + this.full.id, { name, ssh: this.edit.ssh.trim(), via: this.edit.via.trim(), provider: this.edit.provider.trim(), notes: this.edit.notes.trim() })
        this.editing = false
        this.$emit('changed')
      } catch (e) { this.editErr = e.message }
    },
  },
  template: `
    <div class="main">
      <div class="main-head">
        <button class="btn ghost" @click="$emit('back')">← Topology</button>
        <span class="title">{{ full.name }}</span>
        <span v-if="provider" class="prov-badge" :style="provStyle(provider)">{{ provider }}</span>
        <span class="host-meta" v-if="sshTarget">ssh {{ sshTarget }}</span>
        <span class="spacer"></span>
        <button class="btn ghost" @click="startEdit()">Edit</button>
        <button class="btn" @click="$emit('terminal', full)">Open terminal</button>
        <button class="btn ghost danger" @click="del()">Delete</button>
      </div>
      <div class="detail">
        <section v-if="editing">
          <h3>Edit host</h3>
          <div class="form" style="max-width:520px; padding:0; gap:8px">
            <label class="dim" style="font-size:12px">Display name</label>
            <input v-model="edit.name" placeholder="name" @keyup.enter="saveEdit()" />
            <label class="dim" style="font-size:12px">SSH target <span class="dim">(from its gateway if reached via one, e.g. -p 26152 1.2.3.4)</span></label>
            <input v-model="edit.ssh" placeholder="user@host (blank to clear)" @keyup.enter="saveEdit()" />
            <label class="dim" style="font-size:12px">Reach via (jump host name, optional)</label>
            <input v-model="edit.via" placeholder="gateway host name, e.g. myaliyun (blank = direct)" @keyup.enter="saveEdit()" />
            <label class="dim" style="font-size:12px">Cloud provider (optional)</label>
            <input v-model="edit.provider" list="providers-edit" placeholder="e.g. aliyun (blank to clear)" @keyup.enter="saveEdit()" />
            <datalist id="providers-edit"><option v-for="p in providers" :key="p" :value="p"></option></datalist>
            <label class="dim" style="font-size:12px">Notes</label>
            <textarea v-model="edit.notes" rows="2" placeholder="notes"></textarea>
            <div style="display:flex; gap:8px">
              <button class="btn" @click="saveEdit()">Save</button>
              <button class="btn ghost" @click="cancelEdit()">Cancel</button>
            </div>
            <div class="err" v-if="editErr">{{ editErr }}</div>
          </div>
        </section>
        <section v-if="full.notes">
          <h3>Notes</h3>
          <div class="notes">{{ full.notes }}</div>
        </section>
        <section>
          <h3>Tags <span class="dim">· operator-asserted</span></h3>
          <table class="kv" v-if="tagEntries.length">
            <tr v-for="[k, v] in tagEntries" :key="k"><td class="k">{{ k }}</td><td>{{ v }}</td></tr>
          </table>
          <div v-else class="dim">No tags.</div>
        </section>
        <section>
          <h3>Facts <span class="dim">· model-discovered</span></h3>
          <table class="kv" v-if="factEntries.length">
            <tr><th>key</th><th>value</th><th>observed</th></tr>
            <tr v-for="[k, f] in factEntries" :key="k">
              <td class="k">{{ k }}</td><td>{{ f.value }}</td><td class="dim">{{ f.observed_at }}</td>
            </tr>
          </table>
          <div v-else class="dim">No facts recorded yet — they appear once the agent (or you) discovers things on the host.</div>
        </section>
        <section class="dim" style="font-size:12px">Added {{ full.created_at }}</section>
      </div>
    </div>
  `,
}

// ---- Root app ----
// ---- Settings: manage LLM provider tokens, pick the active one ----
const SettingsView = {
  emits: ['changed'],
  data() {
    return { tab: 'tokens', creds: [], providers: {}, defaults: {}, err: '', busy: false,
             form: { label: '', provider: 'anthropic', model: '', customModel: '', token: '' },
             general: [], generalMsg: '', knowledge: [], kText: '' }
  },
  async mounted() { await this.load(); await this.loadGeneral(); await this.loadKnowledge() },
  methods: {
    async load() {
      const d = await api('GET', '/api/llm/credentials')
      this.creds = (d.credentials || []).map((c) => ({ ...c, _savedModel: c.model || '', _custom: '' }))
      this.providers = d.providers || {}
      this.defaults = d.defaults || {}
      if (!this.form.model) this.form.model = this.defaults[this.form.provider] || ''
    },
    models(p) { return this.providers[p] || [] },
    // options for an existing token's dropdown: provider suggestions, plus its
    // own current model if that id isn't in the suggestion list (e.g. a custom one).
    modelOpts(c) {
      const base = this.providers[c.provider] || []
      if (c._savedModel && !base.includes(c._savedModel)) return [c._savedModel, ...base]
      return base
    },
    onProvider() { this.form.model = this.defaults[this.form.provider] || ''; this.form.customModel = '' },
    async add() {
      this.err = ''
      if (!this.form.token.trim()) { this.err = 'token is required'; return }
      const model = this.form.model === '__custom__' ? this.form.customModel.trim() : this.form.model.trim()
      if (!model) { this.err = 'model is required'; return }
      this.busy = true
      try {
        await api('POST', '/api/llm/credentials', {
          label: this.form.label.trim(), provider: this.form.provider,
          token: this.form.token.trim(), model,
        })
        this.form = { label: '', provider: this.form.provider, model: this.defaults[this.form.provider] || '', customModel: '', token: '' }
        await this.load(); this.$emit('changed')
      } catch (e) { this.err = e.message } finally { this.busy = false }
    },
    async activate(c) { await api('POST', '/api/llm/credentials/' + c.id + '/activate'); await this.load(); this.$emit('changed') },
    // chose a suggested model from the dropdown -> save immediately
    // (or revealed the custom field, which saves via saveRowCustom).
    async onRowModel(c) {
      if (c.model === '__custom__') { c._custom = ''; return }
      await this.saveModel(c)
    },
    async saveRowCustom(c) {
      const m = (c._custom || '').trim()
      if (!m) { this.err = 'model cannot be empty'; return }
      c.model = m
      await this.saveModel(c)
    },
    async saveModel(c) {
      this.err = ''
      const m = (c.model || '').trim()
      if (m === '__custom__' || m === c._savedModel) return
      if (!m) { this.err = 'model cannot be empty'; c.model = c._savedModel; return }
      try {
        await api('PATCH', '/api/llm/credentials/' + c.id, { label: c.label, model: m })
        c.model = m; c._savedModel = m
        this.$emit('changed') // active model may have changed
      } catch (e) { this.err = e.message; c.model = c._savedModel }
    },
    async del(c) { if (!confirm('Delete token "' + c.label + '"?')) return; await api('DELETE', '/api/llm/credentials/' + c.id); await this.load(); this.$emit('changed') },
    async loadGeneral() { this.general = ((await api('GET', '/api/settings')).settings || []).map((g) => ({ ...g })) },
    async saveSetting(g) {
      this.generalMsg = ''
      try {
        await api('PUT', '/api/settings/' + g.key, { value: String(g.value == null ? '' : g.value).trim() })
        await this.loadGeneral(); this.generalMsg = g.label + ' saved — applies on the next request.'
      } catch (e) { this.generalMsg = e.message }
    },
    async loadKnowledge() { this.knowledge = (await api('GET', '/api/knowledge')).knowledge || [] },
    async addKnowledge() {
      const text = this.kText.trim()
      if (!text) return
      await api('POST', '/api/knowledge', { text })
      this.kText = ''; await this.loadKnowledge()
    },
    async delKnowledge(k) { await api('DELETE', '/api/knowledge/' + k.id); await this.loadKnowledge() },
  },
  template: `
    <div class="main">
      <div class="main-head">
        <span class="title">Settings</span>
        <div class="subtabs">
          <button class="subtab" :class="{ active: tab === 'tokens' }" @click="tab = 'tokens'">LLM tokens</button>
          <button class="subtab" :class="{ active: tab === 'general' }" @click="tab = 'general'">General</button>
          <button class="subtab" :class="{ active: tab === 'knowledge' }" @click="tab = 'knowledge'">Knowledge</button>
        </div>
        <span class="spacer"></span>
      </div>
      <div class="detail" v-show="tab === 'tokens'">
        <section>
          <h3>API tokens <span class="dim">· the active one drives new chats · edit a model inline</span></h3>
          <table class="kv" v-if="creds.length">
            <tr><th>active</th><th>label</th><th>provider</th><th>model</th><th>token</th><th></th></tr>
            <tr v-for="c in creds" :key="c.id">
              <td>
                <span v-if="c.active" class="badge open">● active</span>
                <button v-else class="btn ghost" @click="activate(c)">Use</button>
              </td>
              <td>{{ c.label }}</td>
              <td><span class="badge">{{ c.provider }}</span></td>
              <td>
                <select class="modelsel" v-model="c.model" @change="onRowModel(c)">
                  <option v-for="m in modelOpts(c)" :key="m" :value="m">{{ m }}</option>
                  <option value="__custom__">Custom…</option>
                </select>
                <template v-if="c.model === '__custom__'">
                  <input class="modelin" v-model="c._custom" placeholder="model id"
                         @keyup.enter="saveRowCustom(c)" />
                  <button class="btn ghost sm" @mousedown.prevent @click="saveRowCustom(c)">save</button>
                </template>
              </td>
              <td class="dim">{{ c.token_masked }}</td>
              <td><span class="x" @click="del(c)" title="delete">✕</span></td>
            </tr>
          </table>
          <div v-else class="dim">No tokens yet — add one below.</div>
        </section>
        <section>
          <h3>Add a token</h3>
          <div class="form" style="max-width:520px; padding:0; gap:8px">
            <label class="dim" style="font-size:12px">Provider</label>
            <select v-model="form.provider" @change="onProvider()">
              <option value="anthropic">Anthropic (Claude)</option>
              <option value="openai">OpenAI</option>
            </select>
            <label class="dim" style="font-size:12px">Model</label>
            <select v-model="form.model">
              <option v-for="m in models(form.provider)" :key="m" :value="m">{{ m }}</option>
              <option value="__custom__">Custom…</option>
            </select>
            <input v-if="form.model === '__custom__'" v-model="form.customModel" placeholder="enter model id" />
            <label class="dim" style="font-size:12px">Label (optional)</label>
            <input v-model="form.label" placeholder="e.g. personal" />
            <label class="dim" style="font-size:12px">API token</label>
            <input v-model="form.token" type="password" placeholder="paste secret token" @keyup.enter="add()" />
            <button class="btn" :disabled="busy" @click="add()">{{ busy ? 'Saving…' : 'Add token' }}</button>
            <div class="err" v-if="err">{{ err }}</div>
          </div>
        </section>
        <section class="dim" style="font-size:12px; line-height:1.6">
          Tokens are stored locally in SQLite and only ever sent as the provider's auth header,
          through your network proxy. They are never echoed back to the browser (shown masked).
        </section>
      </div>

      <div class="detail" v-show="tab === 'general'">
        <section v-for="g in general" :key="g.key">
          <h3>{{ g.label }}</h3>
          <div class="form" style="max-width:560px; padding:0; gap:8px">
            <div class="dim" style="font-size:12px; line-height:1.5">{{ g.help }}</div>
            <input v-model="g.value" :type="g.type === 'number' ? 'number' : 'text'"
                   :placeholder="g.placeholder" @keyup.enter="saveSetting(g)" />
            <div class="dim" style="font-size:12px">
              Currently using <span class="k">{{ g.effective || 'direct' }}</span>{{ g.is_set ? '' : ' (default ' + g.default + ')' }}.
            </div>
            <button class="btn" @click="saveSetting(g)">Save</button>
          </div>
        </section>
        <div v-if="generalMsg" style="font-size:12px; color: var(--accent-2); padding-bottom:12px">{{ generalMsg }}</div>
        <section class="dim" style="font-size:12px; line-height:1.6">
          Settings are stored locally in SQLite and take effect on the next request — no restart.
          They override the matching environment variables.
        </section>
      </div>

      <div class="detail" v-show="tab === 'knowledge'">
        <section>
          <h3>What Hopskip remembers <span class="dim">· loaded into the agent's context every chat</span></h3>
          <table class="kv" v-if="knowledge.length">
            <tr v-for="k in knowledge" :key="k.id">
              <td>{{ k.text }}</td>
              <td style="width:1%; white-space:nowrap"><span class="x" @click="delKnowledge(k)" title="forget">✕</span></td>
            </tr>
          </table>
          <div v-else class="dim">Nothing yet. Tell the agent things to remember in chat (it calls its remember tool), or add one below.</div>
        </section>
        <section>
          <h3>Add a fact</h3>
          <div class="form" style="max-width:560px; padding:0; gap:8px">
            <textarea v-model="kText" rows="3" placeholder="e.g. Aliyun hosts (provider=aliyun/aliyun-hk) can't reach overseas sites like GitHub. To install software, fetch the file via my laptop with fetch_url then scp it over."></textarea>
            <button class="btn" :disabled="!kText.trim()" @click="addKnowledge()">Remember</button>
          </div>
        </section>
      </div>
    </div>
  `,
}

// ---- "Changes" tab: what the agent authored vs the base shell ----
// Minimal LCS line diff so an override can be shown understandably (+/- lines).
function lineDiff(aText, bText) {
  const a = (aText || '').split('\n'), b = (bText || '').split('\n')
  const n = a.length, m = b.length
  const dp = Array.from({ length: n + 1 }, () => new Int32Array(m + 1))
  for (let i = n - 1; i >= 0; i--)
    for (let j = m - 1; j >= 0; j--)
      dp[i][j] = a[i] === b[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1])
  const out = []; let i = 0, j = 0
  while (i < n && j < m) {
    if (a[i] === b[j]) { out.push({ t: ' ', text: a[i] }); i++; j++ }
    else if (dp[i + 1][j] >= dp[i][j + 1]) { out.push({ t: '-', text: a[i] }); i++ }
    else { out.push({ t: '+', text: b[j] }); j++ }
  }
  while (i < n) out.push({ t: '-', text: a[i++] })
  while (j < m) out.push({ t: '+', text: b[j++] })
  return out
}

const ChangesView = {
  data() {
    return { files: [], extensions: [], openPath: null, diff: null, summary: '',
             loading: false, exporting: false, exportMsg: '' }
  },
  async mounted() { await this.load() },
  methods: {
    async load() {
      this.loading = true
      try { const d = await api('GET', '/api/web/changes'); this.files = d.files || []; this.extensions = d.extensions || [] }
      catch (e) {} finally { this.loading = false }
    },
    prefix(ln) { return ln.t === '+' ? '+ ' : ln.t === '-' ? '- ' : '  ' },
    async toggle(f) {
      if (this.openPath === f.path) { this.openPath = null; this.diff = null; return }
      this.openPath = f.path; this.diff = null; this.summary = '…'
      try {
        const ov = await api('GET', '/api/web/content?layer=overlay&path=' + encodeURIComponent(f.path))
        let base = ''
        if (f.status === 'override') {
          base = (await api('GET', '/api/web/content?layer=base&path=' + encodeURIComponent(f.path))).content || ''
        }
        const d = lineDiff(base, ov.content || '')
        const add = d.filter((x) => x.t === '+').length, del = d.filter((x) => x.t === '-').length
        this.summary = (f.status === 'new' ? 'new file · ' : '') + '+' + add + ' −' + del + ' lines'
        this.diff = d
      } catch (e) { this.summary = 'failed to load: ' + e.message }
    },
    async save() {
      this.exporting = true; this.exportMsg = ''
      try {
        const r = await api('POST', '/api/web/export')
        this.exportMsg = '✓ Committed ' + r.count + ' file' + (r.count === 1 ? '' : 's') + ' to ' + r.dir
        this.openPath = null; this.diff = null
        await this.load() // refresh: committed (saved) changes drop off the list
      } catch (e) { this.exportMsg = 'Error: ' + e.message } finally { this.exporting = false }
    },
  },
  template: `
    <div class="main">
      <div class="main-head">
        <span class="title">Changes</span>
        <span class="dim" style="font-size:12px">what the agent authored on top of the base shell</span>
        <span class="spacer"></span>
        <button class="btn ghost" @click="load()" title="refresh">↻</button>
        <button class="btn" :disabled="exporting" @click="save()">{{ exporting ? 'Saving…' : '💾 Save to disk' }}</button>
      </div>
      <div class="detail">
        <div v-if="exportMsg" class="export-msg">{{ exportMsg }}</div>

        <section v-if="extensions.length">
          <h3>Features <span class="dim">· registered UI extensions</span></h3>
          <div v-for="e in extensions" :key="e.slug" class="change-feat">
            <span class="badge" :class="e.kind">{{ e.kind }}</span>
            <strong>{{ e.title || e.slug }}</strong>
            <span class="dim"> mounts at {{ e.mount }}</span>
            <span class="badge" v-if="!e.enabled" style="opacity:.6">disabled</span>
            <div class="dim" v-if="e.notes" style="margin-top:3px">{{ e.notes }}</div>
            <div class="k" style="font-size:11px;opacity:.7">{{ e.entry }} · by {{ e.created_by }}</div>
          </div>
        </section>

        <section>
          <h3>Files changed <span class="dim">· authored overlay vs base shell</span></h3>
          <div v-if="!files.length && !loading" class="dim">
            No customizations yet — the agent hasn't authored any UI. The base shell is unchanged.
          </div>
          <div v-for="f in files" :key="f.path" class="change-file">
            <div class="cf-head" @click="toggle(f)">
              <span class="caret">{{ openPath === f.path ? '▾' : '▸' }}</span>
              <span class="badge" :class="f.status">{{ f.status }}</span>
              <span class="k">{{ f.path }}</span>
              <span class="dim" style="font-size:11px"> · {{ f.size }} B · by {{ f.created_by }} · {{ (f.updated_at||'').slice(0,16).replace('T',' ') }}</span>
            </div>
            <div v-if="openPath === f.path" class="cf-body">
              <div class="dim" style="font-size:11px;margin:2px 0 6px">{{ summary }}</div>
              <div class="difftext" v-if="diff">
                <div v-for="(ln, i) in diff" :key="i" class="dl"
                     :class="ln.t === '+' ? 'add' : ln.t === '-' ? 'del' : 'ctx'">{{ prefix(ln) + ln.text }}</div>
              </div>
            </div>
          </div>
        </section>
      </div>
    </div>
  `,
}

// ---- Frontend extension runtime (the agent reprograms its own UI) ----
// The manifest lives in SQLite; code lives in the DB-backed overlay. The loader
// is defensive by construction: a bad import or a throwing component yields a
// contained error card, NEVER a blank shell. See spec/frontend-extension-protocol.md §5.
const extRegistry = reactive({ slots: {}, overrides: {}, errors: [] })

function extError(slug, message) {
  extRegistry.errors.push({ slug, message: String(message), at: Date.now() })
}

async function loadExtensions() {
  let manifest = []
  try { manifest = (await api('GET', '/api/extensions')).extensions || [] }
  catch (e) { return } // no manifest endpoint / empty → pure base shell
  for (const e of manifest) {
    try {
      const mod = await import(/* @vite-ignore */ e.entry) // dynamic ES import from the overlay
      const def = (mod && mod.default) || mod
      if (e.kind === 'slot') {
        if (!extRegistry.slots[e.mount]) extRegistry.slots[e.mount] = []
        extRegistry.slots[e.mount].push({ slug: e.slug, component: markRaw(def) })
      } else if (e.kind === 'override') {
        extRegistry.overrides[e.mount] = markRaw({ slug: e.slug, module: mod })
      } else {
        extError(e.slug, "extension kind '" + e.kind + "' is not supported yet")
      }
    } catch (err) {
      extError(e.slug, (err && err.message) || err)
    }
  }
}

// Per-child error boundary: one broken extension can't take out its siblings or
// the shell. A throwing component renders nothing and records a card.
const ExtBoundary = {
  props: { comp: { type: [Object, Function], required: true }, context: { type: Object, default: () => ({}) }, slug: String },
  data() { return { failed: false } },
  errorCaptured(err) { this.failed = true; extError(this.slug || '(extension)', (err && err.message) || err); return false },
  render() { return this.failed ? null : h(this.comp, { context: this.context }) },
}

// <extension-slot name="host-detail.actions" :context="{ host }" /> renders every
// enabled slot extension registered for that mount.
const ExtensionSlot = {
  name: 'ExtensionSlot',
  components: { ExtBoundary },
  props: { name: { type: String, required: true }, context: { type: Object, default: () => ({}) } },
  render() {
    const items = extRegistry.slots[this.name] || []
    return items.map((it) => h(ExtBoundary, { comp: it.component, context: this.context, slug: it.slug, key: it.slug }))
  },
}

const ExtErrors = {
  render() {
    if (!extRegistry.errors.length) return null
    return h('div', { class: 'ext-errors' }, extRegistry.errors.map((e, i) =>
      h('div', { class: 'ext-error', key: i }, [
        h('span', '⚠ extension ' + (e.slug || '') + ': ' + e.message),
        h('span', { class: 'x', title: 'dismiss', onClick: () => extRegistry.errors.splice(i, 1) }, '✕'),
      ])))
  },
}

const App = {
  components: { TermView, ChatView, TopoView, HostDetailView, SettingsView, ChangesView, ExtensionSlot, ExtErrors },
  data() {
    return {
      hosts: [], sessions: [], status: { agent_configured: false }, providers: [],
      view: 'topo', selectedHost: null, sidebarOpen: true, hostsOpen: true,
      activeSession: null, activeHost: null, termSteps: [], termAutoConnect: false, termStartConnected: false,
      form: { name: '', ssh: '', via: '', provider: '', notes: '' }, addErr: '', adding: false,
    }
  },
  computed: {
    openSessions() { return this.sessions.filter((s) => s.status === 'open') },
    // The URL path for the current view (topo = '/', chat = '/chat',
    // host-detail = '/hosts/:id', terminal = '/term/:session', …).
    currentPath() {
      switch (this.view) {
        case 'chat': return '/chat'
        case 'settings': return '/settings'
        case 'changes': return '/changes'
        case 'host-detail': return this.selectedHost ? '/hosts/' + this.selectedHost.id : '/'
        case 'term': return this.activeSession ? '/term/' + this.activeSession.session_id : '/'
        default: return '/'
      }
    },
  },
  watch: {
    // Reflect the active tab/view in the browser URL so it's deep-linkable and
    // back/forward work. The reverse direction (URL → view) is applyRoute().
    currentPath(p) {
      if (window.history && p !== window.location.pathname) window.history.pushState({}, '', p)
    },
  },
  async mounted() {
    await Promise.all([this.loadStatus(), this.loadHosts(), this.loadSessions(), this.loadProviders()])
    loadExtensions() // load agent/operator-authored UI; defensive — never blocks the shell
    window.addEventListener('popstate', () => this.applyRoute(window.location.pathname))
    this.applyRoute(window.location.pathname) // honor a deep link / refreshed path on load
  },
  methods: {
    async loadStatus() { try { this.status = await api('GET', '/api/status') } catch (e) {} },
    async loadHosts() { this.hosts = (await api('GET', '/api/hosts')).hosts || [] },
    async loadSessions() { this.sessions = (await api('GET', '/api/sessions')).sessions || [] },
    async loadProviders() { try { this.providers = (await api('GET', '/api/providers')).providers || [] } catch (e) {} },
    provOf(h) { return providerOf(h) },
    provColor(p) { return providerColor(p) },
    onFleetChanged() { this.loadHosts(); this.loadSessions() },
    showTopo() { this.view = 'topo'; this.activeSession = null },
    showChat() { this.view = 'chat'; this.activeSession = null },
    showSettings() { this.view = 'settings'; this.activeSession = null },
    showChanges() { this.view = 'changes'; this.activeSession = null },
    showHostDetail(h) { this.selectedHost = h; this.view = 'host-detail'; this.activeSession = null },
    // Set the view from a URL path (initial load, refresh, back/forward). Unknown
    // hosts/sessions fall back to the topology home.
    applyRoute(path) {
      const parts = (path || '/').split('/').filter(Boolean)
      const seg = parts[0] || ''
      if (seg === 'chat') { this.view = 'chat'; this.activeSession = null }
      else if (seg === 'settings') { this.view = 'settings'; this.activeSession = null }
      else if (seg === 'changes') { this.view = 'changes'; this.activeSession = null }
      else if (seg === 'hosts' && parts[1]) {
        const h = this.hosts.find((x) => String(x.id) === parts[1])
        if (h) { this.selectedHost = h; this.view = 'host-detail'; this.activeSession = null }
        else { this.view = 'topo'; this.activeSession = null }
      } else if (seg === 'term' && parts[1]) {
        const s = this.sessions.find((x) => x.session_id === parts[1])
        if (s) this.reattach(s)
        else { this.view = 'topo'; this.activeSession = null }
      } else { this.view = 'topo'; this.activeSession = null }
    },
    async addHost() {
      this.addErr = ''
      const name = this.form.name.trim()
      if (!name) { this.addErr = 'name is required'; return }
      this.adding = true
      try {
        await api('POST', '/api/hosts', { name, ssh: this.form.ssh.trim(), via: this.form.via.trim(), provider: this.form.provider.trim(), notes: this.form.notes.trim() })
        this.form = { name: '', ssh: '', via: '', provider: '', notes: '' }
        await this.loadHosts()
      } catch (e) { this.addErr = e.message } finally { this.adding = false }
    },
    async deleteHost(h) {
      if (!confirm('Delete host "' + h.name + '"?')) return
      await api('DELETE', '/api/hosts/' + h.id)
      await this.loadHosts()
      if (this.selectedHost && this.selectedHost.id === h.id) this.showTopo()
    },
    onDetailDeleted() { this.loadHosts(); this.showTopo() },
    // connectChain resolves the ssh hops to reach a host: each gateway (from the
    // host's `via` tag chain), laptop-first, then the host itself. One element =
    // a direct host. Reaching it is still hop-by-hop typed ssh (no ProxyJump).
    connectChain(host) {
      if (!host) return []
      const sshSpec = (h) => (h.tags && h.tags.ssh) || h.name
      const byName = {}
      this.hosts.forEach((h) => { byName[h.name] = h })
      const gateways = []
      const seen = new Set()
      let cur = host
      while (cur && cur.tags && cur.tags.via && !seen.has(cur.id) && gateways.length < 8) {
        seen.add(cur.id)
        const gw = byName[cur.tags.via]
        if (!gw || gw.id === cur.id) break
        gateways.unshift(gw) // laptop-first
        cur = gw
      }
      return gateways.map(sshSpec).concat([sshSpec(host)])
    },
    async openTerminal(host) {
      const s = await api('POST', '/api/sessions', { host_id: host.id, label: host.name })
      this.activeSession = s
      this.activeHost = host
      this.termSteps = this.connectChain(host)         // gateways… then the host
      this.termAutoConnect = this.termSteps.length > 0  // land on the host directly
      this.termStartConnected = false
      this.view = 'term'
      await this.loadSessions()
    },
    async newBlankSession() {
      const s = await api('POST', '/api/sessions', { label: 'scratch' })
      this.activeSession = s; this.activeHost = null
      this.termSteps = []; this.termAutoConnect = false; this.termStartConnected = false
      this.view = 'term'
      await this.loadSessions()
    },
    reattach(s) {
      this.activeSession = s
      this.activeHost = this.hosts.find((h) => h.id === s.host_id) || null
      // We can't know where a live session sits — assume connected so the ssh
      // button doesn't auto-fire or invite a duplicate login.
      this.termSteps = []
      this.termAutoConnect = false
      this.termStartConnected = !!this.activeHost
      this.view = 'term'
    },
    async closeActive() {
      if (this.activeSession) await this.closeSession(this.activeSession)
      this.showTopo()
    },
    async closeSession(s) {
      await api('DELETE', '/api/sessions/' + s.session_id)
      if (this.activeSession && this.activeSession.session_id === s.session_id) this.showTopo()
      await this.loadSessions()
    },
    sshOf(h) { return (h.tags && h.tags.ssh) || '' },
  },
  template: `
    <div class="layout" :class="{ 'sidebar-collapsed': !sidebarOpen }">
      <aside class="sidebar">
        <div class="brand">
          <button class="fold-btn" @click="sidebarOpen = !sidebarOpen"
                  :title="sidebarOpen ? 'collapse sidebar' : 'expand sidebar'">{{ sidebarOpen ? '«' : '»' }}</button>
          <span class="dot"></span> <span class="brand-text">Hopskip</span>
          <span class="status">{{ status.active_model || (status.agent_configured ? 'agent ready' : 'no token') }}</span>
        </div>
        <div class="sidebar-scroll">
          <div class="nav-item" :class="{ active: view === 'topo' }" @click="showTopo()">🌐 Topology</div>
          <div class="nav-item" :class="{ active: view === 'chat' }" @click="showChat()">💬 Chat</div>
          <div class="nav-item" :class="{ active: view === 'settings' }" @click="showSettings()">⚙ Settings</div>
          <div class="nav-item" :class="{ active: view === 'changes' }" @click="showChanges()">🧩 Changes</div>
          <extension-slot name="global.nav" :context="{}" />

          <div class="section-title foldable" @click="hostsOpen = !hostsOpen">
            <span class="caret">{{ hostsOpen ? '▾' : '▸' }}</span> Hosts <span class="count">{{ hosts.length }}</span>
          </div>
          <div v-show="hostsOpen">
            <div v-for="h in hosts" :key="h.id"
                 class="nav-item"
                 :class="{ active: (view === 'host-detail' && selectedHost && selectedHost.id === h.id) }"
                 @click="showHostDetail(h)">
              <span class="prov-dot" :style="{ background: provColor(provOf(h)) }" :title="provOf(h) || 'no provider'"></span>{{ h.name }}
              <span class="sub" v-if="sshOf(h)">{{ sshOf(h) }}</span>
              <span class="x term" @click.stop="openTerminal(h)" title="open terminal">❯</span>
              <span class="x" @click.stop="deleteHost(h)" title="delete">✕</span>
            </div>
            <div v-if="hosts.length === 0" class="dim" style="padding:4px 10px;font-size:12px">No hosts yet — add one below.</div>
          </div>

          <div class="section-title">Add host</div>
          <div class="form">
            <input v-model="form.name" placeholder="name (e.g. aliyun)" @keyup.enter="addHost()" />
            <input v-model="form.ssh" placeholder="ssh target (e.g. user@1.2.3.4)" @keyup.enter="addHost()" />
            <input v-model="form.via" placeholder="reach via (jump host name, optional)" @keyup.enter="addHost()" />
            <input v-model="form.provider" list="providers-add" placeholder="cloud provider (optional)" @keyup.enter="addHost()" />
            <datalist id="providers-add"><option v-for="p in providers" :key="p" :value="p"></option></datalist>
            <textarea v-model="form.notes" rows="2" placeholder="notes (how to reach it, etc.)"></textarea>
            <button class="btn" :disabled="adding" @click="addHost()">{{ adding ? 'Adding…' : 'Add host' }}</button>
            <div class="err" v-if="addErr">{{ addErr }}</div>
          </div>

          <div class="section-title">Sessions</div>
          <div v-for="s in openSessions" :key="s.session_id"
               class="nav-item"
               :class="{ active: activeSession && activeSession.session_id === s.session_id }"
               @click="reattach(s)">
            ⌁ {{ s.label || s.session_id.slice(0, 8) }}
            <span class="badge open">open</span>
            <span class="x" @click.stop="closeSession(s)" title="close">✕</span>
          </div>
          <div class="nav-item dim" @click="newBlankSession()">＋ new blank session</div>
        </div>
      </aside>

      <main class="main" style="flex:1; min-width:0">
        <ext-errors />
        <topo-view v-if="view === 'topo'" :hosts="hosts" :sessions="sessions" :providers="providers" @open="showHostDetail" />
        <settings-view v-else-if="view === 'settings'" @changed="loadStatus" />
        <changes-view v-else-if="view === 'changes'" />
        <host-detail-view v-else-if="view === 'host-detail' && selectedHost"
                          :key="selectedHost.id" :host="selectedHost" :providers="providers"
                          @terminal="openTerminal" @back="showTopo" @deleted="onDetailDeleted" @changed="loadHosts" />
        <term-view v-else-if="view === 'term' && activeSession"
                   :key="activeSession.session_id"
                   :session="activeSession" :host="activeHost" :steps="termSteps"
                   :auto-connect="termAutoConnect" :start-connected="termStartConnected"
                   :scrollback="status.terminal_scrollback || 100000"
                   @close="closeActive()" />
        <!-- Chat stays mounted (v-show, not v-if) so its transcript, live WebSocket,
             and the agent's conversation context all persist across tab switches. -->
        <chat-view v-show="view === 'chat'" :agent-configured="status.agent_configured" @fleet-changed="onFleetChanged" />
      </main>
    </div>
  `,
}

const app = createApp(App)
// Final safety net: any uncaught error from authored extension code becomes a
// dismissible card, never a white screen.
app.config.errorHandler = (err, vm, info) => { extError('(runtime)', (err && err.message) || err) }
app.mount('#app')

// Exposed for tests and future extension tooling; unused by the browser shell.
export { App, TermView, ChatView, TopoView, HostDetailView, SettingsView, ChangesView }
export { ExtensionSlot, ExtErrors, extRegistry, loadExtensions, extError, lineDiff }
export { formatToolUse, formatToolResult, prettyKV }
