# vitrine

Self-hosted artifact server. Agents create, read and iterate on single-file
HTML artifacts over MCP; people open a stable link in a shell that live
reloads, shares, keeps history, and gives pages a runtime compatible with
Claude artifacts (`await claude.use("db" | "room" | "sample" | "user")`).

One Go binary, one SQLite file. Implements the "Self-hosted artifact server — PRD".

## Layout

| file | what |
| --- | --- |
| `store.go` | SQLite schema, users, sessions, tokens, artifacts, versions (gzip), FTS5 search, grants |
| `access.go` | sharing levels, capability declarations, db access rules (`{self}`, `data/users/<id>`) |
| `docdb.go` | the `db` document store: paths, queries, set/update/delete, ifVersion, leases |
| `hub.go` | WebSocket hub: head changes, db snapshots, rooms/presence |
| `ai.go` | `sample` proxy to an Anthropic Messages gateway (exe.dev LLM integration) |
| `mcp.go` | MCP server (streamable HTTP, JSON responses) + the same tools at `/api/v1/tools/<name>` |
| `oauth.go` | OAuth 2.1 authorization server for MCP clients (RFC 9728/8414/7591, PKCE, rotating refresh) |
| `server.go` | routes, browser login (exe.dev headers or OIDC), sessions, CSRF, rate limits |
| `views.go` | shell page, sandboxed frame/raw content with CSP, UI API |
| `guide.go` | the `guide` tool's docs for agents |
| `web/runtime.js` | injected into every artifact: `window.claude.use()` talking to the shell over postMessage |
| `web/static/shell.js` | viewer chrome, live reload, history, share, and the bridge to the WebSocket |

## Routes

- `/a/<id>` shell (live head, or the viewer's pinned version), `/a/<id>/v/<n>` fixed version,
  `/a/<id>/raw` bare page (no runtime), `/c/<id>/<n>` frame content (sandboxed, opaque origin)
- `/mcp` MCP; `/.well-known/oauth-*`, `/oauth/*` OAuth for MCP clients
- `/` index + search, `/settings` connect agents and personal tokens

## Configure

Environment (see `/etc/vitrine/env` on the exe.dev VM):

```
VITRINE_ADDR=0.0.0.0:8787
VITRINE_BASE_URL=https://artifacts.example.com
VITRINE_DB=/var/lib/vitrine/vitrine.db
VITRINE_AUTH=exedev            # or oidc, or dev (local testing only)
VITRINE_ALLOW_SIGNIN=          # emails/@domains allowed to sign in; empty = anyone the IdP signs in
VITRINE_CREATORS=you@example.com
VITRINE_LLM_URL=https://llm.int.exe.xyz
VITRINE_MODEL_QUICK / _DEFAULT / _COMPLEX
# oidc mode:
VITRINE_OIDC_ISSUER=https://login.microsoftonline.com/<tenant>/v2.0
VITRINE_OIDC_CLIENT_ID=...  VITRINE_OIDC_CLIENT_SECRET=...   # redirect URI: <base>/auth/callback
VITRINE_LLM_HEADER="Authorization: Bearer ..."               # for other gateways
```

`exedev` mode trusts exe.dev's `X-ExeDev-*` headers: the server must only be
reachable through the exe.dev HTTPS proxy (verified: forged headers are stripped).

## Deploy / upgrade

```
deploy/install.sh          # builds, installs /usr/local/bin/vitrine, systemd unit, daily backup timer
vitrinectl token -email you@example.com -name laptop   # personal API token
vitrinectl import -owner you@example.com page.html...  # import HTML files
vitrinectl backup -o /var/lib/vitrine/backups/now.db
```

Backups: `vitrine-backup.timer` writes `/var/lib/vitrine/backups/vitrine-YYYY-MM-DD.db` daily, keeps 14 days.
Elsewhere: run the binary behind any TLS proxy that passes WebSockets, with `VITRINE_AUTH=oidc`.

## Not in v1

Multi-file artifacts, comments, scoped agent tokens, compaction, sample tools/images,
`sendToClaudeSession`, the `artifact`/`assets`/`mcp` capabilities.
