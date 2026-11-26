# Development

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
| `db.go` | the two SQL dialects: `?` rebinding for Postgres, row locks, unique-violation checks, Postgres migrations under an advisory lock |
| `migrate.go` | `migrate-store`: copy a whole store between SQLite and Postgres, either way |
| `server.go` | routes, sessions, CSRF, rate limits |
| `auth.go` | pluggable browser sign-in: exedev, oidc, proxy (headers or signed JWT), dev; trusted proxies and client addresses |
| `views.go` | shell page, sandboxed frame/raw content with CSP, UI API |
| `guide.go` | the `guide` tool's docs for agents |
| `web/runtime.js` | injected into every artifact: `window.claude.use()` talking to the shell over postMessage |
| `web/static/shell.js` | viewer chrome, live reload, history, share, and the bridge to the WebSocket |


## Routes

- `/a/<id>` shell (live head, or the viewer's pinned version), `/a/<id>/v/<n>` fixed version,
  `/a/<id>/raw` bare page (no runtime), `/c/<id>/<n>` frame content (sandboxed, opaque origin; `?vy=<px>` opens it scrolled down),
  `/a/<id>/thumb` uploaded thumbnail image
- `/?folder=<id>` one of the viewer's folders; `/api/folders` lists, creates, renames, deletes them, `/api/artifacts/<id>/folder` files an artifact
- `/mcp` MCP; `/.well-known/oauth-*`, `/oauth/*` OAuth for MCP clients
- `/` index + search, `/settings` connect agents and personal tokens

## Tests

```
go test ./...       # store tests, each on SQLite and on a real Postgres
```

Postgres comes from [embedded-postgres](https://github.com/fergusstrange/embedded-postgres),
which downloads and runs the real server binaries (cached in `~/.cache/embedded-postgres-go`);
set `VITRINE_TEST_PG_URL=postgres://user:pass@host:port/` to use an existing server instead.
`go run ./cmd/pgtest serve` keeps one running for the end-to-end suites, and
`pgtest fresh -db NAME` resets a database between them.

End-to-end suites: see [e2e/README.md](../e2e/README.md).
