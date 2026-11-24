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
  `/a/<id>/raw` bare page (no runtime), `/c/<id>/<n>` frame content (sandboxed, opaque origin)
- `/mcp` MCP; `/.well-known/oauth-*`, `/oauth/*` OAuth for MCP clients
- `/` index + search, `/settings` connect agents and personal tokens

## Configure

Everything is environment variables (on an exe.dev VM: `/etc/vitrine/env`).

```
VITRINE_BASE_URL=https://artifacts.example.com   # the public URL: OAuth metadata, cookies, CSP and WebSocket checks use it
VITRINE_ADDR=0.0.0.0:8787       # default 127.0.0.1:8787; if unset and PORT is set (Cloud Run): 0.0.0.0:$PORT
VITRINE_DB=/var/lib/vitrine/vitrine.db   # a SQLite file, or postgres://user:pass@host/db (one replica until the event bus lands)
VITRINE_AUTH=                   # required: exedev | oidc | proxy | dev (below)
VITRINE_ALLOW_SIGNIN=           # emails/@domains allowed to sign in; empty = anyone the sign-in method accepts
VITRINE_ALLOW_GROUPS=           # groups allowed to sign in (oidc claim or proxy header); empty = any
VITRINE_CREATORS=you@example.com   # who may create artifacts; empty = anyone signed in
VITRINE_TRUSTED_PROXIES=        # addresses/CIDRs of proxies in front, or * when the platform guarantees one (Cloud Run)
VITRINE_LOGOUT_URL=             # where the browser goes after sign-out (the proxy's or IdP's sign-out page)
VITRINE_LLM_URL=                # Anthropic Messages endpoint for the page `sample` capability; empty = off
VITRINE_LLM_HEADER=             # e.g. "x-api-key: ..." for api.anthropic.com
VITRINE_MODEL_QUICK / _DEFAULT / _COMPLEX
```

### Sign-in

Sign-in is pluggable (`auth.go`). Whatever the method, vitrine then keeps its
own session, and agents connect through vitrine's own OAuth server (or personal
tokens), which sends the user through the same sign-in. vitrine refuses to
start with a configuration that would let anyone sign in as anyone.

| `VITRINE_AUTH` | Use it for | Needs |
|---|---|---|
| `exedev` | an exe.dev VM | nothing: exe.dev's proxy sets `X-ExeDev-UserID`/`-Email` and strips them from clients. Only safe when the server is reachable solely through that proxy. |
| `oidc` | any OpenID Connect provider: Google, Entra ID, Okta, Keycloak, Dex | `VITRINE_OIDC_ISSUER`, `VITRINE_OIDC_CLIENT_ID`, `VITRINE_OIDC_CLIENT_SECRET`; redirect URI `<base>/auth/callback`. Optional `VITRINE_OIDC_SCOPES`, `VITRINE_GROUPS_CLAIM` (default `groups`). |
| `proxy` | an authenticating proxy in front: oauth2-proxy, Pomerium, Cloudflare Access, Teleport, an ingress with auth | either plain headers plus `VITRINE_TRUSTED_PROXIES` and/or `VITRINE_PROXY_SECRET`, or a signed JWT header (below) |
| `dev` | tests | refuses any non-loopback address |

`proxy` mode reads identity only at `GET /auth/proxy`: that is the one path the
proxy must protect (and set the headers on). Everything else must pass through
unauthenticated, because agents reach `/mcp`, `/.well-known/*` and `/oauth/*`
with their own tokens, and public links need no account.

```
# plain headers (oauth2-proxy defaults shown)
VITRINE_PROXY_EMAIL_HEADER=X-Forwarded-Email
VITRINE_PROXY_USER_HEADER=X-Forwarded-User
VITRINE_PROXY_NAME_HEADER=X-Forwarded-Preferred-Username
VITRINE_PROXY_GROUPS_HEADER=X-Forwarded-Groups
VITRINE_TRUSTED_PROXIES=10.0.0.0/8      # requests to /auth/proxy from elsewhere are refused
VITRINE_PROXY_SECRET="X-Proxy-Secret: <random>"   # optional extra check, or instead of addresses
VITRINE_PROXY_LOGIN_URL=/oauth2/start?rd={rd}     # optional: the proxy's start page; {rd} = return URL

# or a signed JWT header, checked against the proxy's published keys
VITRINE_PROXY_JWT_HEADER=Cf-Access-Jwt-Assertion  # or X-Goog-Iap-Jwt-Assertion, X-Pomerium-Jwt-Assertion
VITRINE_PROXY_JWT_JWKS=https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
VITRINE_PROXY_JWT_ISSUER=https://<team>.cloudflareaccess.com
VITRINE_PROXY_JWT_AUDIENCE=<application audience tag>
```

`VITRINE_TRUSTED_PROXIES` also decides whose `X-Forwarded-For` counts for rate
limits; on exe.dev everything arrives through its proxy, so it is trusted there.

## Deploy / upgrade

On an exe.dev VM:

```
deploy/install.sh          # builds, installs /usr/local/bin/vitrine, systemd unit, daily backup timer
vitrinectl token -email you@example.com -name laptop   # personal API token
vitrinectl import -owner you@example.com page.html doc.md...  # import HTML pages and markdown documents
vitrinectl backup -o /var/lib/vitrine/backups/now.db
vitrine migrate-store -from vitrine.db -to postgres://...   # or back: -from postgres://... -to copy.db (destination must be empty)
```

Backups: `vitrine-backup.timer` writes `/var/lib/vitrine/backups/vitrine-YYYY-MM-DD.db` daily, keeps 14 days.

Elsewhere: run the binary behind any TLS proxy or ingress that passes
WebSockets, with `VITRINE_AUTH=oidc` or `proxy`. With SQLite that is one
replica with a persistent volume; several replicas (and Cloud Run) need the
Postgres store from the PRD addendum.

## Tests

```
go test ./...       # store tests, each on SQLite and on a real Postgres
```

Postgres comes from [embedded-postgres](https://github.com/fergusstrange/embedded-postgres),
which downloads and runs the real server binaries (cached in `~/.cache/embedded-postgres-go`);
set `VITRINE_TEST_PG_URL=postgres://user:pass@host:port/` to use an existing server instead.
`go run ./cmd/pgtest serve` keeps one running for the end-to-end suites, and
`pgtest fresh -db NAME` resets a database between them.

## Not in v1

Multi-file artifacts, comments, scoped agent tokens, compaction, sample tools/images,
`sendToClaudeSession`, the `artifact`/`assets`/`mcp` capabilities.

## License

MIT, see [LICENSE](LICENSE).
