package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/saltylin/hopskip/internal/session"
	"github.com/saltylin/hopskip/internal/store"
)

// systemPrompt is the architecture protocol (CLAUDE.md §4) — the model's
// description of the world it operates. Keep in sync with CLAUDE.md.
const systemPrompt = `ENVIRONMENT: hopskip

You operate a personal fleet of remote hosts on behalf of one operator, from
their laptop. You act like a human at a terminal.

CORE MODEL
- You work through persistent terminal SESSIONS. A session is a live shell
  (a tmux pane) that retains state between your actions — current directory,
  environment, and crucially WHICH HOST YOU ARE ON.
- To reach a host that is only accessible via another host, you SSH in steps,
  exactly as a person would: open a session, type ` + "`ssh <intermediate>`" + `, read
  the screen, confirm the prompt, then type ` + "`ssh <final>`" + `. There is no
  pre-declared topology. The path is whatever you type.
- After every keystroke batch, READ THE SCREEN before deciding the next action.
  Output is asynchronous: a command may still be running, may be waiting for
  input (password, yes/no), or may have finished. Never assume; look.

KNOWING WHERE YOU ARE  (most important safety rule)
- A session can be nested several hosts deep. Before running ANY command that
  writes, deletes, restarts, kills, or installs, you MUST confirm which host
  the session is currently on — e.g. by reading the shell prompt or running a
  harmless identifying command (hostname, whoami). State the host in your
  reasoning before the mutating command. A destructive command on the wrong
  host is the worst failure in this system.

INVENTORY
- Hosts are described by free-form TAGS (what the operator asserts: role=gpu,
  provider=bandwagon) and FACTS (what gets discovered by looking: gpu=A100,
  disk_free=12G, nginx_listening=true). There is no fixed schema.
- When the operator refers to a host by a word (e.g. "alidev", "the web box"),
  that word is almost always the host's NAME. Resolve it with get_host (which
  accepts a name OR an id), or call list_hosts with NO filter and read the names.
  Do NOT invent a tag filter from that word (e.g. provider=alidev): the list_hosts
  tag filter is an EXACT match on operator-asserted tags only and never matches a
  name, so a wrong guess returns an empty list. An empty result means your filter
  was wrong — NOT that the host is missing; fall back to an unfiltered list_hosts
  before telling the operator the host can't be found.
- You discover facts by opening a session on a host and running read-only
  commands, then recording what you learn with record_fact. Treat facts as
  possibly stale; re-check when it matters.
- A host note may tell you how to reach it (e.g. "reach via aliyun") and a host
  may carry an ssh tag with its ssh target. Read those, then type your way there.
  The inventory does NOT build the connection for you.
- A host NOTE IS AUTHORITATIVE and may state a PRE-CONNECTION STEP — something to
  do before you ssh to that host (e.g. "run ` + "`mykinit`" + ` in a local shell first").
  Every host's note is given to you under FLEET INVENTORY below, so you always
  have it. Before you ssh to a host, RE-READ its note and perform any such step in
  a LAPTOP session FIRST, then ssh. Skipping a stated pre-step is a real failure —
  the connection or what follows will often break (e.g. missing auth ticket).
- If a host is only reachable through another (a jump/bastion host), record that
  with a "via" tag naming the gateway host (e.g. via=myaliyun), and put the host's
  ssh spec AS SEEN FROM THAT GATEWAY in its "ssh" tag (e.g. ssh="-p 26152 1.2.3.4").
  That nests it under the gateway in the topology and lets the operator's Connect
  button hop through. You still reach it by typing ssh hop-by-hop: ssh to the
  gateway first, then from there ssh to the target.

CONDUCT
- Diagnose freely with read-only commands (status, logs, ps, df, cat, grep).
- For commands that change system state, explain what you intend to run, on
  which host, and why, before running it.
- Prefer key-based auth. If you must type a secret, know it may be captured in
  the session screen; minimize exposure.
- To make an HTTP request (call an API, check an endpoint, hit a webhook), use the
  http_request tool rather than running curl/wget/httpie in a shell session: it
  goes through the operator's laptop and its configured proxy, keeps any auth
  tokens on the laptop, and returns a clean { status, headers, body }. Reach for a
  shell HTTP client only when the request must originate FROM a specific host.
- Some hosts are network-restricted and can't reach certain sites (e.g. overseas
  ones). When that applies — usually the operator will have told you, recorded
  under OPERATOR KNOWLEDGE — go through the operator's laptop/proxy: fetch_url to
  download a file, or http_request for an API call. A downloaded file lives ON THE
  LAPTOP at the path the tool returns. Don't assume a host is restricted unless you
  were told or you observe it failing.
- To get a laptop-downloaded file ONTO a host, the copy MUST run FROM THE LAPTOP
  — the laptop has the file and can reach the host; the host has NEITHER the file
  NOR a route back to the laptop. So: call open_session to get a fresh session
  (it starts on the laptop), CONFIRM it is the laptop (read the prompt or run
  hostname), and scp from there. NEVER run the copy inside a session that is
  already ssh'd into the host — that is the single most common mistake here. For
  a host behind a jump host, use scp's ProxyJump and UPPERCASE -P for the port
  (scp uses -P; ssh uses -p), e.g.:
    scp -J <gateway-user@host> -P <port> <laptop-path> <user@host>:<dest>
  Then, back in the host's session, fix permissions if needed (e.g. chmod +x).
- When the operator tells you a durable fact about their environment or how to
  handle something, call remember so you don't lose it across chats.
- When done, summarize: what was wrong, what you observed, what you changed (if
  anything), and what you recommend the operator do next.`

// toolSpec is a provider-agnostic tool definition; each provider runner converts
// it into its own SDK's tool/function shape.
type toolSpec struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

func toolSpecs() []toolSpec {
	str := map[string]any{"type": "string"}
	integer := map[string]any{"type": "integer"}
	boolean := map[string]any{"type": "boolean"}
	return []toolSpec{
		{"open_session", "Open a new shell session (a tmux pane on the laptop). It starts on the laptop; type `ssh ...` via send_keys to reach a host.",
			map[string]any{"label": str}, nil},
		{"send_keys", "Type keystrokes into a session. Include \\n to press Enter. await_done=true (default) wraps the command to detect completion and capture the exit code; use await_done=false for interactive prompts (host-key yes/no, password, sudo) and long-running commands, then poll read_screen.",
			map[string]any{"session_id": str, "keys": str, "await_done": boolean, "timeout_ms": integer}, []string{"session_id", "keys"}},
		{"read_screen", "Read a session's visible screen without typing. Use it to confirm which host you are on before any mutating command, and to poll long-running output. lines limits to the last N lines.",
			map[string]any{"session_id": str, "lines": integer}, []string{"session_id"}},
		{"list_sessions", "List terminal sessions and their status.", map[string]any{}, nil},
		{"close_session", "Close (kill) a terminal session.", map[string]any{"session_id": str}, []string{"session_id"}},
		{"list_hosts", "List inventory hosts with their tags and facts. With NO argument it returns ALL hosts — use that to find a host the operator named. The optional tag filter is an EXACT match 'key=value' on operator-asserted tags only; it does NOT match host names, so never pass a guessed provider/role for a named host — use get_host or an unfiltered list instead.", map[string]any{"tag": str}, nil},
		{"get_host", "Get one host by id or name, with its tags and facts. This is how you resolve a host the operator names (e.g. 'alidev').", map[string]any{"host": str}, []string{"host"}},
		{"add_host", "Add a host to the inventory.", map[string]any{"name": str, "notes": str}, []string{"name"}},
		{"rename_host", "Change a host's display name — its main identity in the inventory. Tags do NOT change the display name; this is the only way. The new name must be unique. (Tip: if the old name was the connection target like an IP, set an `ssh` tag to it first so you can still reach the host.)", map[string]any{"host": str, "name": str}, []string{"host", "name"}},
		{"tag_host", "Set an operator-asserted tag (key/value metadata) on a host, e.g. key=ssh value=user@host or key=role value=gpu. Tags are metadata only and do NOT change the host's display name — use rename_host for that.", map[string]any{"host": str, "key": str, "value": str}, []string{"host", "key", "value"}},
		{"record_fact", "Record a discovered fact about a host, stamped with the current time.", map[string]any{"host": str, "key": str, "value": str}, []string{"host", "key", "value"}},
		{"forget_fact", "Delete a fact from a host.", map[string]any{"host": str, "key": str}, []string{"host", "key"}},
		{"forget_tag", "Remove an operator-asserted tag from a host (the counterpart to tag_host).", map[string]any{"host": str, "key": str}, []string{"host", "key"}},
		{"fetch_url", "Download an http(s) URL THROUGH THE OPERATOR'S LAPTOP (which can reach sites a remote host may not, and uses the configured proxy). The file is saved ON THE LAPTOP at the returned absolute path; small text files are also returned inline. IMPORTANT: to put the file onto a host, the copy MUST run FROM THE LAPTOP — call open_session (it starts on the laptop), confirm you are on the laptop, then scp to the host (for a host behind a jump host: scp -J <gateway> -P <port> <laptop-path> user@host:<dest>; scp uses uppercase -P for the port). NEVER scp from a session that is already on the host — it has neither the file nor a route back to the laptop.",
			map[string]any{"url": str, "save_as": str}, []string{"url"}},
		{"http_request", "Make an HTTP request THROUGH THE OPERATOR'S LAPTOP and its configured network proxy, and get the response back. PREFER THIS over running curl/wget/httpie in a shell session — it uses the operator's proxy automatically, keeps auth tokens on the laptop, and returns a clean structured result. Use it to call APIs, check endpoints, hit webhooks, etc. — especially when a remote host can't reach a site or you need the proxy. Supports any method, custom headers, and a request body. Returns { status, headers, body } — the body is inline when it's textual and not too large, otherwise saved on the laptop with a note. (For plain file downloads you intend to copy to a host, fetch_url is more convenient.)",
			map[string]any{
				"url":     str,
				"method":  str,
				"headers": map[string]any{"type": "object", "additionalProperties": str, "description": "request headers, name->value"},
				"body":    str,
				"save_as": str,
			}, []string{"url"}},
		{"remember", "Save a durable fact or instruction the operator told you to remember; it is loaded into your context in EVERY future chat. Use it whenever the operator tells you something about their environment, conventions, or how to handle a situation that you should not forget.",
			map[string]any{"text": str}, []string{"text"}},
		{"list_knowledge", "List the durable facts/instructions you have remembered (with their ids).", map[string]any{}, nil},
		{"forget_knowledge", "Delete a remembered fact by its id.", map[string]any{"id": integer}, []string{"id"}},
		{"list_web_files", "List the frontend assets you can read or override: the LLM/operator-authored overlay files (stored in the DB, served above the base shell) and the embedded base-shell file paths. Use this to discover what UI code exists before reading or overriding it.", map[string]any{}, nil},
		{"read_web_file", "Read a frontend file's full source. Resolves the authored overlay first, then the embedded base shell — so you see the code that is actually served. Use it to study the shell before writing an override (e.g. how the Connect terminal works) or to read an extension you wrote. path is a web path like 'app.js' or 'ext/connect/entry.mjs'.", map[string]any{"path": str}, []string{"path"}},
		{"write_web_file", "Create or replace a frontend asset in the authored overlay (DB-backed; served above the embedded base shell, live on the next browser refresh). This is how you change the platform's own UI from chat. Keep changes small and prefer a separate extension module over rewriting the whole shell. path is a web path (no leading slash, no '..'); content is the file's full text. After writing an extension module, call register_extension so the shell loads it.", map[string]any{"path": str, "content": str, "content_type": str}, []string{"path", "content"}},
		{"delete_web_file", "Remove an authored overlay asset, reverting that path to the embedded base shell. path is the web path you wrote.", map[string]any{"path": str}, []string{"path"}},
		{"register_extension", "Register (or update) a frontend extension in the manifest so the shell loads it on refresh. kind: 'slot' renders your module into a named built-in slot (mount = slot name, e.g. 'host-detail.actions'); 'route' adds a page (mount = path); 'override' replaces a built-in region (mount = region name, e.g. 'connect.steps' — advanced, trusted). entry is the module's web path you wrote with write_web_file (e.g. '/ext/connect/entry.mjs'). The loader is defensive: a broken extension shows an error card, never a blank shell.", map[string]any{"slug": str, "kind": str, "mount": str, "entry": str, "title": str, "sdk_range": str, "notes": str}, []string{"slug", "kind", "mount", "entry"}},
		{"unregister_extension", "Remove an extension from the manifest by slug (its files remain unless you also delete_web_file them). Use enabled-toggling via register_extension if you only want to disable temporarily.", map[string]any{"slug": str}, []string{"slug"}},
		{"list_extensions", "List registered frontend extensions (the manifest the shell loads), with their kind, mount, entry, and enabled state.", map[string]any{}, nil},
	}
}

// Dispatcher is the single side-effect choke point (invariant 5): every tool the
// model calls is executed here, against tmux + SQLite. A future mutation gate
// lives here.
type Dispatcher struct {
	mgr    *session.Manager
	store  *store.Store
	client *http.Client // proxy-aware, for fetch_url
	dlDir  string       // where fetch_url saves files (absolute)
	webFS  fs.FS        // embedded base shell, for read_web_file (may be nil)
}

// NewDispatcher builds a Dispatcher. client is the proxy-aware HTTP client used
// by fetch_url; webFS is the embedded base shell read by the web-authoring tools.
func NewDispatcher(mgr *session.Manager, st *store.Store, client *http.Client, webFS fs.FS) *Dispatcher {
	dl := os.Getenv("HOPSKIP_DOWNLOAD_DIR")
	if dl == "" {
		dl = "downloads"
	}
	if abs, err := filepath.Abs(dl); err == nil {
		dl = abs
	}
	return &Dispatcher{mgr: mgr, store: st, client: client, dlDir: dl, webFS: webFS}
}

// knowledgePrompt renders the operator's remembered facts for the system prompt.
func (d *Dispatcher) knowledgePrompt() string {
	ks, err := d.store.ListKnowledge()
	if err != nil || len(ks) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nOPERATOR KNOWLEDGE — durable facts and instructions the operator told you to remember. Treat them as authoritative and apply them:\n")
	for _, k := range ks {
		b.WriteString(fmt.Sprintf("- (#%d) %s\n", k.ID, k.Text))
	}
	return b.String()
}

// inventoryPrompt injects the current fleet — host names, operator NOTES, and
// operator-asserted tags — into the system prompt every turn, so the model
// honors per-host notes (e.g. "run mykinit before ssh") WITHOUT having to call
// get_host first. Discovered facts are omitted to keep it compact (fetch them
// with get_host when needed). This is the reliability backbone for notes: a note
// the model never sees is a note it can't follow.
func (d *Dispatcher) inventoryPrompt() string {
	hosts, err := d.store.ListHosts()
	if err != nil || len(hosts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nFLEET INVENTORY — the operator's hosts, with their notes and tags. A host NOTE IS AUTHORITATIVE: if it states something to do before connecting (e.g. \"run X first\"), you MUST do it in a laptop session BEFORE you ssh to that host. Re-read the note each time you connect to that host.\n")
	for _, h := range hosts {
		b.WriteString("- " + h.Name)
		if len(h.Tags) > 0 {
			b.WriteString(" [" + joinTags(h.Tags) + "]")
		}
		b.WriteString("\n")
		if note := strings.TrimSpace(h.Notes); note != "" {
			b.WriteString("    note: " + clip(note, 400) + "\n")
		}
	}
	return b.String()
}

func joinTags(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ", ")
}

// preConnectReminder surfaces a host's operator note at the moment the model
// types an ssh toward that host — a belt-and-suspenders catch in case the note
// in FLEET INVENTORY was overlooked. Non-blocking by design (the ssh still runs);
// it rides back in the send_keys result so the model can self-correct (e.g. exit,
// run the required pre-step, reconnect). The dispatch choke point is the right
// home for this (a future hard pre-connect gate would live here too).
func (d *Dispatcher) preConnectReminder(keys string) string {
	if !strings.Contains(keys, "ssh") {
		return ""
	}
	hosts, err := d.store.ListHosts()
	if err != nil {
		return ""
	}
	for _, h := range hosts {
		note := strings.TrimSpace(h.Notes)
		if note == "" || !hostMentionedIn(keys, h) {
			continue
		}
		return "host '" + h.Name + "' has an operator note: " + clip(note, 400) +
			" — if it requires a pre-connection step you have NOT done yet, exit back to the laptop, do it, then reconnect."
	}
	return ""
}

// hostMentionedIn reports whether the typed keys target host h — by its name as a
// standalone token (ssh devbox, root@devbox, devbox:port) or by its literal ssh
// tag. Token-bounded so "devbox" does not match "devbox-staging".
func hostMentionedIn(keys string, h store.Host) bool {
	sep := func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';' || r == '&' || r == '|'
	}
	for _, f := range strings.FieldsFunc(keys, sep) {
		if f == h.Name || strings.Contains(f, "@"+h.Name) || strings.HasPrefix(f, h.Name+":") {
			return true
		}
	}
	if ssh := strings.TrimSpace(h.Tags["ssh"]); ssh != "" && strings.Contains(keys, ssh) {
		return true
	}
	return false
}

// Dispatch executes one tool call and returns (resultJSON, isError).
func (d *Dispatcher) Dispatch(name string, input json.RawMessage) (string, bool) {
	switch name {
	case "open_session":
		var in struct {
			Label string `json:"label"`
		}
		_ = json.Unmarshal(input, &in)
		ss, err := d.mgr.Open(in.Label, nil)
		if err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"session_id": ss.ID, "label": ss.Label}), false

	case "send_keys":
		var in struct {
			SessionID string `json:"session_id"`
			Keys      string `json:"keys"`
			AwaitDone *bool  `json:"await_done"`
			TimeoutMs int    `json:"timeout_ms"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		await := true
		if in.AwaitDone != nil {
			await = *in.AwaitDone
		}
		screen, code, completed, err := d.mgr.SendKeys(in.SessionID, in.Keys, await, in.TimeoutMs)
		if err != nil {
			return errStr(err), true
		}
		res := map[string]any{
			"session_id": in.SessionID, "screen": screen, "exit_code": code, "completed": completed,
		}
		if rem := d.preConnectReminder(in.Keys); rem != "" {
			res["reminder"] = rem
		}
		return jsonStr(res), false

	case "read_screen":
		var in struct {
			SessionID string `json:"session_id"`
			Lines     int    `json:"lines"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		screen, err := d.mgr.Capture(in.SessionID, in.Lines)
		if err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"session_id": in.SessionID, "screen": screen}), false

	case "list_sessions":
		sessions, err := d.store.ListSessions()
		if err != nil {
			return errStr(err), true
		}
		if sessions == nil {
			sessions = []store.Session{}
		}
		return jsonStr(map[string]any{"sessions": sessions}), false

	case "close_session":
		var in struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		if err := d.mgr.Close(in.SessionID); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"session_id": in.SessionID, "closed": true}), false

	case "list_hosts":
		var in struct {
			Tag string `json:"tag"`
		}
		_ = json.Unmarshal(input, &in)
		hosts, err := d.store.ListHosts()
		if err != nil {
			return errStr(err), true
		}
		if k, v, ok := splitTag(in.Tag); ok {
			kept := hosts[:0]
			for _, h := range hosts {
				if h.Tags[k] == v {
					kept = append(kept, h)
				}
			}
			hosts = kept
		}
		if hosts == nil {
			hosts = []store.Host{}
		}
		return jsonStr(map[string]any{"hosts": hosts}), false

	case "get_host":
		var in struct {
			Host string `json:"host"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		h, err := d.resolve(in.Host)
		if err != nil {
			return errStr(err), true
		}
		return jsonStr(h), false

	case "add_host":
		var in struct {
			Name  string `json:"name"`
			Notes string `json:"notes"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		id, err := d.store.AddHost(strings.TrimSpace(in.Name), in.Notes)
		if err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"id": id}), false

	case "rename_host":
		var in struct {
			Host string `json:"host"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		h, err := d.resolve(in.Host)
		if err != nil {
			return errStr(err), true
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			return errStr(fmt.Errorf("name is required")), true
		}
		if err := d.store.UpdateHost(h.ID, name, h.Notes); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true, "id": h.ID, "name": name}), false

	case "tag_host":
		var in struct {
			Host  string `json:"host"`
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		h, err := d.resolve(in.Host)
		if err != nil {
			return errStr(err), true
		}
		if err := d.store.TagHost(h.ID, in.Key, in.Value); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true}), false

	case "record_fact":
		var in struct {
			Host  string `json:"host"`
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		h, err := d.resolve(in.Host)
		if err != nil {
			return errStr(err), true
		}
		if err := d.store.RecordFact(h.ID, in.Key, in.Value); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true}), false

	case "forget_fact":
		var in struct {
			Host string `json:"host"`
			Key  string `json:"key"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		h, err := d.resolve(in.Host)
		if err != nil {
			return errStr(err), true
		}
		if err := d.store.DeleteFact(h.ID, in.Key); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true}), false

	case "forget_tag":
		var in struct {
			Host string `json:"host"`
			Key  string `json:"key"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		h, err := d.resolve(in.Host)
		if err != nil {
			return errStr(err), true
		}
		if err := d.store.DeleteTag(h.ID, in.Key); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true}), false

	case "fetch_url":
		var in struct {
			URL    string `json:"url"`
			SaveAs string `json:"save_as"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		return d.fetchURL(strings.TrimSpace(in.URL), strings.TrimSpace(in.SaveAs))

	case "http_request":
		var in struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
			Body    string            `json:"body"`
			SaveAs  string            `json:"save_as"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		return d.httpRequest(strings.TrimSpace(in.URL), in.Method, in.Headers, in.Body, strings.TrimSpace(in.SaveAs))

	case "remember":
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return errStr(fmt.Errorf("text is required")), true
		}
		id, err := d.store.AddKnowledge(text)
		if err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true, "id": id}), false

	case "list_knowledge":
		ks, err := d.store.ListKnowledge()
		if err != nil {
			return errStr(err), true
		}
		if ks == nil {
			ks = []store.Knowledge{}
		}
		return jsonStr(map[string]any{"knowledge": ks}), false

	case "forget_knowledge":
		var in struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		if err := d.store.DeleteKnowledge(in.ID); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true}), false

	case "list_web_files":
		return d.listWebFiles()

	case "read_web_file":
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		return d.readWebFile(in.Path)

	case "write_web_file":
		var in struct {
			Path        string `json:"path"`
			Content     string `json:"content"`
			ContentType string `json:"content_type"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		return d.writeWebFile(in.Path, in.Content, in.ContentType)

	case "delete_web_file":
		var in struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		norm, ok := store.NormalizeWebPath(in.Path)
		if !ok {
			return errStr(fmt.Errorf("invalid path")), true
		}
		if err := d.store.DeleteWebFile(norm); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true, "path": norm}), false

	case "register_extension":
		var in store.Extension
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		return d.registerExtension(in)

	case "unregister_extension":
		var in struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(input, &in); err != nil {
			return errStr(err), true
		}
		if err := d.store.DeleteExtension(strings.TrimSpace(in.Slug)); err != nil {
			return errStr(err), true
		}
		return jsonStr(map[string]any{"ok": true}), false

	case "list_extensions":
		exts, err := d.store.ListExtensions(false)
		if err != nil {
			return errStr(err), true
		}
		if exts == nil {
			exts = []store.Extension{}
		}
		return jsonStr(map[string]any{"extensions": exts}), false

	default:
		return fmt.Sprintf(`{"error":"unknown tool %q"}`, name), true
	}
}

// --- frontend self-programming tools (the agent reprograms its own UI) ---

func (d *Dispatcher) listWebFiles() (string, bool) {
	overlay, err := d.store.ListWebFiles()
	if err != nil {
		return errStr(err), true
	}
	if overlay == nil {
		overlay = []store.WebFile{}
	}
	base := []string{}
	if d.webFS != nil {
		_ = fs.WalkDir(d.webFS, ".", func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				base = append(base, p)
			}
			return nil
		})
		sort.Strings(base)
	}
	return jsonStr(map[string]any{
		"overlay":    overlay, // authored files (DB), served above the base shell
		"base_shell": base,    // embedded base-shell paths you can read and override
	}), false
}

func (d *Dispatcher) readWebFile(p string) (string, bool) {
	norm, ok := store.NormalizeWebPath(p)
	if !ok {
		return errStr(fmt.Errorf("invalid path")), true
	}
	if b, ct, ok := d.store.GetWebFile(norm); ok {
		return jsonStr(map[string]any{"path": norm, "source": "overlay", "content_type": ct, "content": string(b)}), false
	}
	if d.webFS != nil {
		if b, err := fs.ReadFile(d.webFS, norm); err == nil {
			return jsonStr(map[string]any{"path": norm, "source": "embed", "content": string(b)}), false
		}
	}
	return errStr(fmt.Errorf("no such web file: %s", norm)), true
}

func (d *Dispatcher) writeWebFile(p, content, contentType string) (string, bool) {
	norm, ok := store.NormalizeWebPath(p)
	if !ok {
		return errStr(fmt.Errorf("invalid path (no leading slash, no '..' allowed)")), true
	}
	if err := d.store.PutWebFile(norm, []byte(content), strings.TrimSpace(contentType), "agent"); err != nil {
		return errStr(err), true
	}
	return jsonStr(map[string]any{"ok": true, "path": norm, "bytes": len(content), "served_at": "/" + norm}), false
}

func (d *Dispatcher) registerExtension(e store.Extension) (string, bool) {
	e.Slug = strings.TrimSpace(e.Slug)
	e.Kind = strings.TrimSpace(e.Kind)
	e.Mount = strings.TrimSpace(e.Mount)
	e.Entry = strings.TrimSpace(e.Entry)
	if e.Slug == "" || e.Mount == "" || e.Entry == "" {
		return errStr(fmt.Errorf("slug, mount and entry are required")), true
	}
	switch e.Kind {
	case "slot", "route", "override":
	default:
		return errStr(fmt.Errorf("kind must be slot, route or override")), true
	}
	e.Enabled = true
	e.CreatedBy = "agent"
	// pin a content hash of the entry module if it lives in the overlay
	if ep, ok := store.NormalizeWebPath(e.Entry); ok {
		if b, _, ok := d.store.GetWebFile(ep); ok {
			e.ContentHash = store.HashContent(b)
		}
	}
	if err := d.store.UpsertExtension(e); err != nil {
		return errStr(err), true
	}
	return jsonStr(map[string]any{"ok": true, "slug": e.Slug, "kind": e.Kind, "mount": e.Mount, "content_hash": e.ContentHash}), false
}

// resolve interprets a host reference as an id (all digits) or a name.
func (d *Dispatcher) resolve(ref string) (store.Host, error) {
	ref = strings.TrimSpace(ref)
	if isDigits(ref) {
		id, _ := strconv.ParseInt(ref, 10, 64)
		return d.store.GetHost(id)
	}
	return d.store.GetHostByName(ref)
}

func jsonStr(v any) string {
	// Don't HTML-escape: tool results carry shell output full of <, >, & — escaping
	// them to </>/& hurts both the transcript display and what the
	// model reads back. A JSON Encoder with SetEscapeHTML(false) emits them literally.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return `{"error":"marshal failed"}`
	}
	return strings.TrimRight(buf.String(), "\n") // Encode appends a trailing newline
}

func errStr(err error) string { return jsonStr(map[string]any{"error": err.Error()}) }

func splitTag(tag string) (string, string, bool) {
	tag = strings.TrimSpace(tag)
	i := strings.IndexByte(tag, '=')
	if tag == "" || i < 0 {
		return "", "", false
	}
	return tag[:i], tag[i+1:], true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// toolResultDisplayLimit caps the tool-result text streamed to / persisted for
// the browser (the model always gets the full result). Generous enough to hold a
// full terminal screen so results rarely truncate; the frontend tolerates a
// truncated tail anyway (loose JSON parse + unescape).
const toolResultDisplayLimit = 6000

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Trim back to a rune boundary so we never split a multibyte UTF-8 char.
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// fetchURL downloads a URL through the operator's (proxy-aware) HTTP client and
// saves it under the downloads dir, returning the local path (and inline text for
// small text files). This is the laptop-side "download proxy" for hosts that
// can't reach a URL directly.
func (d *Dispatcher) fetchURL(rawURL, saveAs string) (string, bool) {
	if rawURL == "" {
		return errStr(fmt.Errorf("url is required")), true
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return errStr(fmt.Errorf("url must start with http:// or https://")), true
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return errStr(err), true
	}
	req.Header.Set("User-Agent", "hopskip-fetch/1")
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return errStr(err), true
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(d.dlDir, 0o755); err != nil {
		return errStr(err), true
	}
	name := saveAs
	if name == "" {
		name = filenameFromURL(rawURL)
	}
	fp := filepath.Join(d.dlDir, safeName(name))
	f, err := os.Create(fp)
	if err != nil {
		return errStr(err), true
	}
	const maxPreview = 64 * 1024
	var buf bytes.Buffer
	n, copyErr := io.Copy(io.MultiWriter(f, &capWriter{buf: &buf, max: maxPreview}), resp.Body)
	_ = f.Close()
	if copyErr != nil {
		return errStr(copyErr), true
	}
	res := map[string]any{
		"url": rawURL, "status": resp.StatusCode,
		"content_type": resp.Header.Get("Content-Type"), "bytes": n,
		"path": fp, "saved_on": "operator laptop",
	}
	if n <= int64(maxPreview) && looksTextual(resp.Header.Get("Content-Type"), buf.Bytes()) {
		res["text"] = buf.String()
	} else {
		res["note"] = "saved ON THE LAPTOP at " + fp + ". To deliver it to a host, open_session (starts on the laptop), confirm you're on the laptop, then scp from there: scp [-J <gateway>] [-P <port>] " + fp + " user@host:<dest>. Do NOT scp from a session that is already on the host."
	}
	return jsonStr(res), resp.StatusCode >= 400
}

// httpRequest makes an arbitrary HTTP request through the operator's proxy-aware
// client and returns the structured response. This is the "don't shell out to
// curl" tool: methods, headers, and a body are supported; the response body is
// returned inline when textual and small, else saved on the laptop. Request
// headers/body (which may carry auth) are NOT echoed back into the transcript.
func (d *Dispatcher) httpRequest(rawURL, method string, headers map[string]string, body, saveAs string) (string, bool) {
	if rawURL == "" {
		return errStr(fmt.Errorf("url is required")), true
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return errStr(fmt.Errorf("url must start with http:// or https://")), true
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = "GET"
	}
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, bodyReader)
	if err != nil {
		return errStr(err), true
	}
	req.Header.Set("User-Agent", "hopskip-http/1")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := d.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return errStr(err), true
	}
	defer resp.Body.Close()

	// Read the body bounded by a hard cap to protect memory; flag truncation.
	const hardCap = 8 << 20    // 8 MiB read bound
	const maxInline = 256 << 10 // inline up to 256 KiB of textual body
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, hardCap+1))
	if readErr != nil {
		return errStr(readErr), true
	}
	truncated := len(data) > hardCap
	if truncated {
		data = data[:hardCap]
	}

	// Response headers are safe to surface (they come from the server).
	respHeaders := map[string]string{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	ct := resp.Header.Get("Content-Type")
	res := map[string]any{
		"url": rawURL, "method": method, "status": resp.StatusCode,
		"status_text": http.StatusText(resp.StatusCode),
		"headers":     respHeaders, "bytes": len(data),
	}
	if truncated {
		res["truncated"] = true
	}

	textual := looksTextual(ct, data)
	switch {
	case saveAs != "":
		// explicit save → write the body to the laptop's download dir
		if err := os.MkdirAll(d.dlDir, 0o755); err != nil {
			return errStr(err), true
		}
		fp := filepath.Join(d.dlDir, safeName(saveAs))
		if err := os.WriteFile(fp, data, 0o644); err != nil {
			return errStr(err), true
		}
		res["path"] = fp
		res["saved_on"] = "operator laptop"
		if textual && len(data) <= maxInline {
			res["body"] = string(data) // small enough to also include inline
		}
	case textual && len(data) <= maxInline:
		res["body"] = string(data)
	case len(data) == 0:
		// no body (e.g. a 204 or HEAD); nothing to add
	default:
		// binary or too large to inline → save it
		if err := os.MkdirAll(d.dlDir, 0o755); err == nil {
			fp := filepath.Join(d.dlDir, safeName(filenameFromURL(rawURL)))
			if os.WriteFile(fp, data, 0o644) == nil {
				res["path"] = fp
				res["saved_on"] = "operator laptop"
			}
		}
		res["note"] = "response body is binary or large; saved on the laptop (see path). Not inlined."
	}
	return jsonStr(res), resp.StatusCode >= 400
}

// capWriter keeps only the first max bytes written (for a text preview).
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (c *capWriter) Write(p []byte) (int, error) {
	if rem := c.max - c.buf.Len(); rem > 0 {
		if rem > len(p) {
			rem = len(p)
		}
		c.buf.Write(p[:rem])
	}
	return len(p), nil
}

func filenameFromURL(u string) string {
	if pu, err := url.Parse(u); err == nil {
		base := path.Base(pu.Path)
		if base != "" && base != "/" && base != "." {
			return base
		}
	}
	return "download"
}

func safeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.ReplaceAll(name, " ", "_")
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}

func looksTextual(ct string, sample []byte) bool {
	c := strings.ToLower(ct)
	for _, t := range []string{"text/", "json", "xml", "yaml", "yml", "javascript", "ecmascript", "shell", "x-sh", "csv", "x-www-form", "toml"} {
		if strings.Contains(c, t) {
			return true
		}
	}
	if c == "" || strings.Contains(c, "octet-stream") {
		if !utf8.Valid(sample) {
			return false
		}
		nonprint := 0
		for _, b := range sample {
			if b < 9 || (b > 13 && b < 32) || b == 127 {
				nonprint++
			}
		}
		return len(sample) == 0 || nonprint*20 < len(sample)
	}
	return false
}
