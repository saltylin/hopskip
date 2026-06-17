// Hopskip extension SDK (embedded, stable URL: /sdk/hopskip.mjs).
// The runtime contract authored extension modules link against. See
// spec/frontend-extension-protocol.md §4. CSP confines all of this to the local
// daemon (connect-src 'self'); the API token never reaches the browser.

export const sdkVersion = '1.0.0'

const wsBase = (location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host

async function req(method, path, body) {
  const opt = { method, headers: {} }
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json'
    opt.body = JSON.stringify(body)
  }
  const r = await fetch(path, opt)
  const text = await r.text()
  let data
  try { data = text ? JSON.parse(text) : {} } catch { data = { raw: text } }
  if (!r.ok) throw new Error((data && data.error) || ('HTTP ' + r.status))
  return data
}

// Read-only-ish local API access (same origin only). Extensions use this to read
// inventory/host data — e.g. a Connect button checking a host's preconnect steps.
export const api = {
  get: (p) => req('GET', p),
  post: (p, b) => req('POST', p, b),
  put: (p, b) => req('PUT', p, b),
  del: (p) => req('DELETE', p),
}

// jobs.run — the ONLY side-effecting primitive (spec §4.3): it starts a normal
// agent run (through dispatch() + the audit path), never a privileged shortcut.
// Returns { id, onEvent(fn), done } where done resolves on the run's final event.
export function runJob({ title, host, prompt, params, model } = {}) {
  return (async () => {
    const chat = await api.post('/api/chats')
    const ws = new WebSocket(wsBase + '/ws/chat')
    const listeners = []
    let resolveDone
    const done = new Promise((res) => { resolveDone = res })
    ws.onopen = () => {
      const text = params ? (prompt + '\n\n' + JSON.stringify(params)) : prompt
      ws.send(JSON.stringify({ type: 'user_message', chat_id: chat.id, text, model }))
    }
    ws.onmessage = (ev) => {
      let e
      try { e = JSON.parse(ev.data) } catch { return }
      listeners.forEach((fn) => { try { fn(e) } catch {} })
      if (e.kind === 'done' || e.kind === 'error') resolveDone(e)
    }
    return { id: chat.id, title, host, onEvent: (fn) => listeners.push(fn), done, close: () => ws.close() }
  })()
}

export const jobs = { run: runJob }
export default { sdkVersion, api, jobs }
