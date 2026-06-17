# Hopskip protocol specs

Precise, implementation-ready definitions of Hopskip's protocols (the MCP tools,
the LLM path, and the self-programming frontend).
`../CLAUDE.md` remains the **source of truth** for architecture and invariants;
these files expand its protocol sections without contradicting them. If a spec
ever conflicts with `CLAUDE.md`, `CLAUDE.md` wins and the spec is a bug.

| Spec | Defines | Expands (`CLAUDE.md`) |
|------|---------|-----------------------|
| [`mcp-protocol.md`](./mcp-protocol.md) | The MCP server: dual transport (stdio + Streamable HTTP), input/output JSON Schemas for the terminal-driving + inventory tools, tool annotations, the done-sentinel mechanism, error layering, session/concurrency semantics. (Knowledge/fetch + web-authoring tool families also ship — see its §1.) | §5 (tool protocol) |
| [`llm-protocol.md`](./llm-protocol.md) | The LLM path: the canonical provider-agnostic message/tool format, Anthropic & OpenAI adapter mappings, streaming, and the agent loop with its single `dispatch()` choke point. *(As built, auditing is delegated to the operator's network proxy — see its amendment note.)* | §4 (architecture protocol), §7 (audit proxy), §8 (agent loop) |
| [`frontend-extension-protocol.md`](./frontend-extension-protocol.md) | The runtime-extensible frontend: hybrid overlay serving (`embed.FS` + `HOPSKIP_WEB_DIR`), the extension manifest + defensive loader, the import-map SDK (`jobs.run` → agent loop), the agent's extension-authoring MCP tools (self-programming), and the security model (CSP `connect-src 'self'`, no token in the browser). | §2 (asset delivery), §9 (frontend), §10 (config), §11.6–7 (invariants) |

The MCP `inputSchema` for each tool **is** the `input_schema` the LLM protocol
sends to the model — one definition, two serializations. The frontend extension
protocol closes the loop: its authoring tools are a third MCP family, and its
`jobs.run` SDK primitive rides the agent loop the LLM protocol defines — so the
platform can program its own UI without any new privileged path.
